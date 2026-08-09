// Package plugin defines how Atlas learns to observe a new technology.
//
// The rule the package exists to enforce: adding Kubernetes, RabbitMQ, or
// MongoDB support means writing a new plugin, never editing the platform. The
// core knows about [Plugin]; it knows nothing about Docker.
//
// A plugin's life has four stages, and the middle one is what distinguishes
// this design from a plain registry:
//
//  1. Registered — compiled in and known to exist.
//  2. Detected — asked whether its subject is actually present on this host.
//     A machine without a Docker daemon must not report a broken Docker
//     integration; it must report no Docker integration. Detection is what
//     makes one binary correct on every host in a heterogeneous fleet.
//  3. Initialised — given its dependencies, and contributing collectors.
//  4. Closed — releasing sockets and connections at shutdown.
//
// Plugins are compiled in rather than loaded from shared objects. Go plugins
// require exact toolchain and dependency matching, break cross-compilation,
// and give a third party code execution inside a process that reads the
// host's most sensitive state. The extensibility that matters here is at the
// source level: one interface, one directory per plugin, no core changes. See
// docs/adr/0006-compiled-in-plugins.md.
package plugin

import (
	"context"
	"log/slog"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

// Descriptor is a plugin's static identity.
type Descriptor struct {
	// ID uniquely identifies the plugin: "docker", "systemd", "postgres".
	// Stable across releases; it appears in configuration and in metric
	// labels.
	ID string
	// Name is the human-readable name for the UI.
	Name string
	// Description explains what the plugin observes.
	Description string
	// Subject names the technology observed, for display in a catalog of
	// available integrations.
	Subject string
}

// Validate reports whether the descriptor is usable.
func (d Descriptor) Validate() error {
	if d.ID == "" {
		return errs.New(errs.CodeInvalidArgument, "plugin descriptor has no ID")
	}
	if d.Name == "" {
		return errs.New(errs.CodeInvalidArgument, "plugin %q has no name", d.ID)
	}
	return nil
}

// Env carries the platform services a plugin may use.
//
// Passing dependencies in rather than letting plugins reach for globals is
// what makes a plugin testable in isolation, and it is also the enforcement
// point for what a plugin is allowed to touch: there is no database handle
// here, because plugins observe and publish, they do not write to storage.
type Env struct {
	// Logger is pre-tagged with the plugin's ID.
	Logger *slog.Logger
	// Bus is where a plugin publishes events it observes directly, such as
	// Docker's event stream. Optional; may be nil in tests.
	Bus *eventbus.Bus
	// NodeID identifies the machine being observed, for event attribution.
	NodeID string

	// Config decodes this plugin's configuration section into target, which
	// should be a pointer to the plugin's own settings struct.
	//
	// A decoder rather than a parsed value, because the core cannot know a
	// plugin's shape: the Redis plugin needs an address and credentials, the
	// Docker plugin a socket path, and neither belongs in a type this package
	// owns. Decoding in the plugin also means a malformed section fails inside
	// the plugin that understands it, with a message naming the right field.
	//
	// Never nil. When a plugin has no configuration section it leaves target
	// untouched and returns nil, so a plugin can always call it without a
	// guard and get its own defaults.
	Config func(target any) error
}

// NoConfig is the decoder used when a plugin has no configuration section.
func NoConfig(any) error { return nil }

// StreamingPlugin is implemented by plugins that observe push-based sources —
// Docker's event stream, a log tail, systemd signals.
//
// It is a separate, optional interface rather than a method on [Plugin] so
// that the many plugins with nothing to stream are not forced to declare an
// empty method, and so adding streaming to an existing plugin is additive.
//
// The streamers returned here are supervised by the scheduler and get the same
// guarantees as polled collectors: panic isolation, restart with backoff,
// health reporting, cardinality enforcement, and bounded shutdown.
type StreamingPlugin interface {
	Plugin
	// Streamers returns the streaming collectors this plugin contributes.
	// Called after Init, alongside Collectors.
	Streamers() []collect.Streamer
}

// Plugin observes one technology and contributes collectors for it.
//
// Implementations must be safe for concurrent use after Init returns.
type Plugin interface {
	// Descriptor returns the plugin's identity. It must be constant and must
	// not depend on Init having run.
	Descriptor() Descriptor

	// Detect reports whether this plugin's subject is present on this host —
	// whether the Docker socket exists and answers, whether systemd is PID 1.
	//
	// It must be cheap and must not have side effects; it runs at startup
	// for every registered plugin. Returning false is a normal outcome, not
	// an error: an error means detection itself could not be completed.
	Detect(ctx context.Context) (bool, error)

	// Init prepares the plugin. It is called only after Detect returns true.
	Init(ctx context.Context, env Env) error

	// Collectors returns the collectors this plugin contributes. Called after
	// Init. Returning none is valid for a plugin that only publishes events.
	Collectors() []collect.Collector

	// Close releases resources. Called once at shutdown, and only if Init
	// succeeded. It must be idempotent.
	Close(ctx context.Context) error
}

// Status describes what happened to a plugin during activation.
type Status string

const (
	// StatusActive means the plugin was detected and initialised.
	StatusActive Status = "active"
	// StatusNotDetected means the plugin's subject is absent from this host.
	// The expected state for most plugins on most machines, and not a fault.
	StatusNotDetected Status = "not_detected"
	// StatusDetectionFailed means detection itself errored — the socket
	// exists but could not be probed.
	StatusDetectionFailed Status = "detection_failed"
	// StatusInitFailed means the plugin was detected but could not start.
	StatusInitFailed Status = "init_failed"
	// StatusDisabled means an operator turned the plugin off in configuration.
	StatusDisabled Status = "disabled"
)

// State is the outcome of activating one plugin.
type State struct {
	Descriptor Descriptor `json:"-"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Subject    string     `json:"subject,omitempty"`
	Status     Status     `json:"status"`
	// Error explains a detection or initialisation failure, in client-safe
	// terms. Empty unless Status is a failure.
	Error string `json:"error,omitempty"`
	// Collectors counts the polled collectors the plugin contributed.
	Collectors int `json:"collectors"`
	// Streamers counts the continuously supervised sources it contributed.
	Streamers int `json:"streamers"`
}

// Active reports whether the plugin is running.
func (s State) Active() bool { return s.Status == StatusActive }
