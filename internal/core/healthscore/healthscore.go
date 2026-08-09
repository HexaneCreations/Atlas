// Package healthscore computes a fleet-wide, per-node health score from
// signals this package does not own — alerts, incidents, metrics, heartbeat,
// inventory, events.
//
// A [Provider] computes one signal; an [Engine] combines whatever providers
// it is given by weighted average. The combination logic never changes to
// add a signal — only the provider list does — which is what keeps this
// composable rather than a hardcoded formula.
package healthscore

import (
	"context"
	"log/slog"
	"time"
)

// Signal is one provider's contribution to a node's score.
type Signal struct {
	Name   string
	Weight float64
	// Score is 0-100, meaningful only when Available.
	Score float64
	// Available is false when the provider had nothing to say — a
	// dependency not wired, a node never observed, nothing to evaluate. An
	// unavailable signal is excluded from the weighted average rather than
	// counted as either healthy or unhealthy.
	Available bool
	Detail    string
}

// Provider computes one signal of a node's health.
//
// A provider reports "nothing to say" through Signal.Available == false, not
// through an error. An error return means evaluating the signal genuinely
// failed (a query errored) — the [Engine] logs it and, like an unavailable
// signal, excludes it from the weighted average, so one failing signal
// degrades visibility into that dimension rather than the whole score.
type Provider interface {
	Name() string
	Score(ctx context.Context, nodeID string) (Signal, error)
}

// Weighted pairs a Provider with the weight its signal carries in the
// overall score. Weights need not sum to any particular total — the engine
// normalises by the total weight of whichever signals were available for a
// given node, so one node missing a signal another node has does not skew
// either score.
type Weighted struct {
	Provider Provider
	Weight   float64
}

// Result is one node's computed health score.
type Result struct {
	NodeID  string
	Overall float64 // 0-100, meaningful only when Available.
	// Available is false when every provider was unavailable for this node —
	// nothing to compute a score from, not a score of zero.
	Available  bool
	Signals    []Signal
	ComputedAt time.Time
}

// Engine computes health scores from a configurable set of weighted
// providers. It holds no state and does no background work: every call to
// [Engine.Score] reads current signals fresh.
type Engine struct {
	providers []Weighted
	logger    *slog.Logger
}

// Options configures an [Engine].
type Options struct {
	Providers []Weighted
	Logger    *slog.Logger
}

// NewEngine builds an Engine over providers.
func NewEngine(opts Options) *Engine {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Engine{providers: opts.Providers, logger: opts.Logger}
}

// Score computes nodeID's health score across every registered provider.
func (e *Engine) Score(ctx context.Context, nodeID string) Result {
	signals := make([]Signal, 0, len(e.providers))
	var weightedSum, totalWeight float64

	for _, pw := range e.providers {
		sig, err := pw.Provider.Score(ctx, nodeID)
		sig.Name = pw.Provider.Name()
		sig.Weight = pw.Weight
		if err != nil {
			e.logger.ErrorContext(ctx, "health signal unavailable",
				slog.String("provider", pw.Provider.Name()), slog.String("node_id", nodeID), slog.String("error", err.Error()))
			sig.Available, sig.Detail = false, "signal evaluation failed"
		}

		signals = append(signals, sig)
		if sig.Available {
			weightedSum += sig.Score * pw.Weight
			totalWeight += pw.Weight
		}
	}

	result := Result{NodeID: nodeID, Signals: signals, ComputedAt: time.Now()}
	if totalWeight > 0 {
		result.Available = true
		result.Overall = min(max(weightedSum/totalWeight, 0), 100)
	}
	return result
}
