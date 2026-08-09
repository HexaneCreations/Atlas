package plugin

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Registry holds registered plugins and manages their activation.
//
// It satisfies [lifecycle.Component] indirectly through the composition root:
// Activate is called during startup and Close during shutdown.
type Registry struct {
	logger *slog.Logger

	mu       sync.RWMutex
	plugins  []Plugin
	byID     map[string]Plugin
	states   map[string]State
	started  []Plugin
	disabled map[string]bool
	sections SectionDecoder
}

// SectionDecoder returns a decoder for one plugin's configuration section, or
// nil when that plugin has no section.
//
// Supplied by the composition root, which owns configuration; the registry
// only routes each plugin to its own section.
type SectionDecoder func(pluginID string) func(target any) error

// WithConfig sets the source of per-plugin configuration sections.
func (r *Registry) WithConfig(decoder SectionDecoder) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sections = decoder
	return r
}

// decoderFor resolves a plugin's decoder, falling back to a no-op so a plugin
// can always call Env.Config without a nil check.
func (r *Registry) decoderFor(pluginID string) func(any) error {
	r.mu.RLock()
	sections := r.sections
	r.mu.RUnlock()

	if sections == nil {
		return NoConfig
	}
	if decode := sections(pluginID); decode != nil {
		return decode
	}
	return NoConfig
}

// NewRegistry builds an empty registry.
//
// disabled lists plugin IDs an operator has turned off. Disabled plugins are
// never detected or initialised, which is the supported way to stop Atlas
// touching a subsystem — safer than uninstalling, and reversible without a
// rebuild.
func NewRegistry(logger *slog.Logger, disabled []string) *Registry {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	off := make(map[string]bool, len(disabled))
	for _, id := range disabled {
		off[id] = true
	}
	return &Registry{
		logger:   logger,
		byID:     make(map[string]Plugin),
		states:   make(map[string]State),
		disabled: off,
	}
}

// Register adds a plugin. Duplicate IDs are rejected.
func (r *Registry) Register(p Plugin) error {
	const op = "plugin.Registry.Register"

	desc := p.Descriptor()
	if err := desc.Validate(); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "cannot register plugin").WithOp(op)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byID[desc.ID]; exists {
		return errs.New(errs.CodeAlreadyExists, "plugin %q is already registered", desc.ID).
			WithOp(op).WithDetail("plugin_id", desc.ID)
	}
	r.byID[desc.ID] = p
	r.plugins = append(r.plugins, p)
	return nil
}

// Activate detects and initialises every registered plugin, registering the
// collectors of those that come up.
//
// A plugin that is absent, fails detection, or fails to initialise does not
// stop activation. That is the central design choice: Atlas runs on
// heterogeneous hosts, and a machine without Redis must still report its CPU.
// Every outcome is recorded in [Registry.States] and surfaced through the API,
// so a plugin that failed is visible rather than merely missing — the
// difference between "no Docker here" and "Docker is broken" is exactly what
// an operator needs.
//
// Activate returns an error only if the registry itself cannot proceed.
func (r *Registry) Activate(ctx context.Context, env Env) error {
	r.mu.Lock()
	plugins := slices.Clone(r.plugins)
	r.mu.Unlock()

	for _, p := range plugins {
		state := r.activateOne(ctx, p, env)

		r.mu.Lock()
		r.states[state.ID] = state
		r.mu.Unlock()
	}

	r.logger.InfoContext(ctx, "plugins activated",
		slog.Int("registered", len(plugins)),
		slog.Int("active", len(r.ActiveStates())),
	)
	return nil
}

func (r *Registry) activateOne(ctx context.Context, p Plugin, env Env) State {
	desc := p.Descriptor()
	state := State{
		Descriptor: desc,
		ID:         desc.ID,
		Name:       desc.Name,
		Subject:    desc.Subject,
	}

	r.mu.RLock()
	off := r.disabled[desc.ID]
	r.mu.RUnlock()
	if off {
		state.Status = StatusDisabled
		r.logger.InfoContext(ctx, "plugin disabled by configuration", slog.String("plugin_id", desc.ID))
		return state
	}

	detected, err := r.safeDetect(ctx, p)
	switch {
	case err != nil:
		state.Status = StatusDetectionFailed
		state.Error = errs.Message(err)
		r.logger.WarnContext(ctx, "plugin detection failed",
			slog.String("plugin_id", desc.ID), slog.Any("error", err))
		return state
	case !detected:
		state.Status = StatusNotDetected
		r.logger.DebugContext(ctx, "plugin subject not present on this host",
			slog.String("plugin_id", desc.ID), slog.String("subject", desc.Subject))
		return state
	}

	// Give each plugin a logger already tagged with its ID, so nothing it
	// logs is ambiguous about its origin.
	pluginEnv := env
	pluginEnv.Logger = r.logger.With(slog.String("plugin_id", desc.ID))
	pluginEnv.Config = r.decoderFor(desc.ID)

	if err := p.Init(ctx, pluginEnv); err != nil {
		state.Status = StatusInitFailed
		state.Error = errs.Message(err)
		r.logger.ErrorContext(ctx, "plugin failed to initialise",
			slog.String("plugin_id", desc.ID), slog.Any("error", err))
		return state
	}

	r.mu.Lock()
	r.started = append(r.started, p)
	r.mu.Unlock()

	state.Status = StatusActive
	state.Collectors = len(p.Collectors())
	if sp, ok := p.(StreamingPlugin); ok {
		state.Streamers = len(sp.Streamers())
	}
	r.logger.InfoContext(ctx, "plugin active",
		slog.String("plugin_id", desc.ID), slog.Int("collectors", state.Collectors))
	return state
}

// safeDetect runs Detect with panic isolation. Detection touches sockets and
// filesystems that may be in unexpected states, and a panic there must not
// prevent the remaining plugins from being tried.
func (r *Registry) safeDetect(ctx context.Context, p Plugin) (detected bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			detected = false
			err = errs.New(errs.CodeInternal, "detection panicked: %v", rec).
				WithOp("plugin.Registry.safeDetect")
		}
	}()
	return p.Detect(ctx)
}

// RegisterCollectors adds every active plugin's collectors to a registry.
//
// Collector registration failures are reported rather than ignored: a plugin
// whose collector ID collides with another's is a bug that must be fixed, not
// a runtime condition to route around.
func (r *Registry) RegisterCollectors(target *collect.Registry) error {
	r.mu.RLock()
	started := slices.Clone(r.started)
	r.mu.RUnlock()

	var failures []error
	for _, p := range started {
		for _, c := range p.Collectors() {
			if err := target.Register(c); err != nil {
				failures = append(failures, errs.Wrap(err, errs.CodeOf(err),
					"plugin %q contributed a collector that could not be registered", p.Descriptor().ID))
			}
		}
		sp, ok := p.(StreamingPlugin)
		if !ok {
			continue
		}
		for _, st := range sp.Streamers() {
			if err := target.RegisterStreamer(st); err != nil {
				failures = append(failures, errs.Wrap(err, errs.CodeOf(err),
					"plugin %q contributed a streamer that could not be registered", p.Descriptor().ID))
			}
		}
	}
	return errs.Join(failures...)
}

// Close shuts down every plugin that was initialised, in reverse activation
// order. Errors are collected so that one stubborn plugin does not prevent
// the rest from releasing their resources.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	started := r.started
	r.started = nil
	r.mu.Unlock()

	var failures []error
	for i := len(started) - 1; i >= 0; i-- {
		p := started[i]
		if err := p.Close(ctx); err != nil {
			r.logger.ErrorContext(ctx, "plugin did not close cleanly",
				slog.String("plugin_id", p.Descriptor().ID), slog.Any("error", err))
			failures = append(failures, err)
		}
	}
	return errs.Join(failures...)
}

// States returns the activation outcome of every registered plugin, in
// registration order.
func (r *Registry) States() []State {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]State, 0, len(r.plugins))
	for _, p := range r.plugins {
		if state, ok := r.states[p.Descriptor().ID]; ok {
			out = append(out, state)
		}
	}
	return out
}

// ActiveStates returns only the plugins that are running.
func (r *Registry) ActiveStates() []State {
	var out []State
	for _, s := range r.States() {
		if s.Active() {
			out = append(out, s)
		}
	}
	return out
}

// Get returns a registered plugin by ID.
func (r *Registry) Get(id string) (Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byID[id]
	return p, ok
}

// Len returns the number of registered plugins.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.plugins)
}
