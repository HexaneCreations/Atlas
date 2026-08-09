package alert

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/platform/id"
)

const (
	// DefaultInterval is how often threshold rules are re-evaluated.
	DefaultInterval = 30 * time.Second
	// DefaultLookback bounds how stale a "latest" sample may be and still
	// count. Wider than the collection interval so one missed push does not
	// spuriously resolve an alert.
	DefaultLookback = 5 * time.Minute
)

// Engine evaluates alert rules and maintains their state and history.
type Engine struct {
	store        Store
	metrics      MetricSource
	logger       *slog.Logger
	interval     time.Duration
	lookback     time.Duration
	onTransition func(context.Context, HistoryEntry)

	mu     sync.Mutex
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options configures an [Engine].
type Options struct {
	Store    Store
	Metrics  MetricSource
	Logger   *slog.Logger
	Interval time.Duration
	Lookback time.Duration
	// OnTransition observes every durable history entry after it is
	// written. This is how the incident timeline correlates alert firings
	// without this package depending on it.
	OnTransition func(context.Context, HistoryEntry)
}

// NewEngine builds an Engine. It does not evaluate anything until Run.
func NewEngine(opts Options) *Engine {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Lookback <= 0 {
		opts.Lookback = DefaultLookback
	}
	return &Engine{
		store: opts.Store, metrics: opts.Metrics, logger: opts.Logger,
		interval: opts.Interval, lookback: opts.Lookback, onTransition: opts.OnTransition,
	}
}

// Run evaluates threshold rules on a timer until ctx is cancelled or Stop is
// called.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	stopCh := make(chan struct{})
	e.stopCh = stopCh
	e.mu.Unlock()

	e.wg.Add(1)
	defer e.wg.Done()

	e.evaluateAll(ctx)

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			e.evaluateAll(ctx)
		}
	}
}

// Stop ends Run and waits for it to return.
func (e *Engine) Stop() {
	e.mu.Lock()
	stopCh := e.stopCh
	e.stopCh = nil
	e.mu.Unlock()

	if stopCh == nil {
		return
	}
	close(stopCh)
	e.wg.Wait()
}

func (e *Engine) evaluateAll(ctx context.Context) {
	rules, err := e.store.ListRules(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not list alert rules", slog.String("error", err.Error()))
		return
	}
	for _, rule := range rules {
		if !rule.Enabled || rule.Kind != KindThreshold {
			continue
		}
		e.evaluateThreshold(ctx, rule)
	}
}

func (e *Engine) evaluateThreshold(ctx context.Context, rule Rule) {
	samples, err := e.metrics.LatestForMetric(ctx, rule.Metric, e.lookback)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read metric for rule evaluation",
			slog.String("rule_id", rule.ID), slog.String("metric", rule.Metric), slog.String("error", err.Error()))
		return
	}

	for _, s := range samples {
		if rule.NodeID != "" && s.NodeID != rule.NodeID {
			continue
		}
		e.applyThreshold(ctx, rule, s.NodeID, seriesKey(s.Labels), s.Value)
	}
}

func (e *Engine) applyThreshold(ctx context.Context, rule Rule, nodeID, series string, value float64) {
	match := rule.Comparison.Evaluate(value, rule.Threshold)
	current, found, err := e.store.GetState(ctx, rule.ID, nodeID, series)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read alert state",
			slog.String("rule_id", rule.ID), slog.String("node_id", nodeID), slog.String("error", err.Error()))
		return
	}
	now := time.Now()

	if !match {
		if found && current.State != StateOK {
			current.State, current.Value, current.ResolvedAt, current.UpdatedAt = StateOK, value, now, now
			e.save(ctx, rule, current, "condition cleared")
		}
		return
	}

	switch {
	case !found || current.State == StateOK:
		next := AlertState{RuleID: rule.ID, NodeID: nodeID, SeriesKey: series, Value: value, UpdatedAt: now}
		if rule.For <= 0 {
			next.State, next.FiredAt = StateFiring, now
			e.save(ctx, rule, next, "")
		} else {
			next.State, next.PendingSince = StatePending, now
			e.saveQuiet(ctx, next)
		}
	case current.State == StatePending && now.Sub(current.PendingSince) >= rule.For:
		current.State, current.FiredAt, current.Value, current.UpdatedAt = StateFiring, now, value, now
		e.save(ctx, rule, current, "")
	default:
		current.Value, current.UpdatedAt = value, now
		e.saveQuiet(ctx, current)
	}
}

// HandleEvent evaluates event rules against one durably stored record. Wired
// as the write-time hook on both the local event writer and the fleet
// ingest receiver, via [eventstore.Tap] — see internal/app for the wiring.
func (e *Engine) HandleEvent(ctx context.Context, rec eventstore.Record) {
	rules, err := e.store.ListRules(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not list alert rules", slog.String("error", err.Error()))
		return
	}

	for _, rule := range rules {
		if !rule.Enabled || rule.Kind != KindEvent || rule.Topic != rec.Topic {
			continue
		}
		if rule.Subject != "" && rule.Subject != rec.Subject {
			continue
		}

		entry := HistoryEntry{
			ID: id.New(), Time: time.Now(), RuleID: rule.ID, NodeID: rec.NodeID,
			State: StateFiring, Severity: rule.Severity, Message: rec.Subject,
		}
		e.appendHistory(ctx, rule.ID, entry)
	}
}

func (e *Engine) save(ctx context.Context, rule Rule, state AlertState, message string) {
	if message != "" {
		state.Message = message
	} else if state.Message == "" {
		state.Message = rule.Name
	}
	e.saveQuiet(ctx, state)

	entry := HistoryEntry{
		ID: id.New(), Time: state.UpdatedAt, RuleID: rule.ID, NodeID: state.NodeID,
		State: state.State, Severity: rule.Severity, Value: state.Value, Message: state.Message,
	}
	e.appendHistory(ctx, rule.ID, entry)
}

func (e *Engine) appendHistory(ctx context.Context, ruleID string, entry HistoryEntry) {
	if err := e.store.AppendHistory(ctx, entry); err != nil {
		e.logger.ErrorContext(ctx, "could not record alert history",
			slog.String("rule_id", ruleID), slog.String("error", err.Error()))
		return
	}
	if e.onTransition != nil {
		e.onTransition(ctx, entry)
	}
}

func (e *Engine) saveQuiet(ctx context.Context, state AlertState) {
	if err := e.store.SaveState(ctx, state); err != nil {
		e.logger.ErrorContext(ctx, "could not save alert state",
			slog.String("rule_id", state.RuleID), slog.String("node_id", state.NodeID), slog.String("error", err.Error()))
	}
}

// seriesKey canonicalises a label set so the same series always maps to the
// same key regardless of map iteration order.
func seriesKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}
