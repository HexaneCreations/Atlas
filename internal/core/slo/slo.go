// Package slo evaluates Service Level Objectives against historical metric
// samples.
//
// Evaluation is windowed compliance, not forecasting: what fraction of
// samples in the definition's window satisfied the compliant condition,
// what fraction of the error budget that consumed, and the status that
// implies. Nothing here projects forward.
//
// This package does not depend on the alert engine or the alert store —
// only on [alert.Comparison], a pure value type it reuses rather than
// reimplementing four comparison operators. An SLO and an alert rule are
// deliberately separate concepts evaluated independently; alerting may
// consume SLO state later, but never the reverse.
package slo

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/core/alert"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Status is where an SLO's error budget stands.
type Status string

const (
	StatusHealthy  Status = "healthy"
	StatusWarning  Status = "warning"
	StatusBreached Status = "breached"
)

// DefaultWarningBudgetPercent is how much of the error budget may be
// consumed before an SLO is [StatusWarning], for a [Definition] that does
// not set its own.
const DefaultWarningBudgetPercent = 75.0

// Definition is an SLO's configuration.
type Definition struct {
	ID   string
	Name string
	// NodeID scopes the SLO — the same scoping unit [alert.Rule.NodeID]
	// uses. A "service" scope does not exist as a first-class concept
	// anywhere in Atlas today (a systemd unit is a metric label, not an
	// addressable entity), so this package does not invent one; node is
	// the real scope this codebase has.
	NodeID string
	// Signal is informational: which Golden Signal this SLO belongs to,
	// for grouping and display. Evaluation reads Metric directly and does
	// not interpret this field.
	Signal string

	Metric string
	// Comparison and Threshold define the *compliant* condition — e.g.
	// ComparisonLT with Threshold 200 means "compliant when value < 200".
	// This is the opposite framing from [alert.Rule], where they define
	// the condition that raises concern; the comparison operators
	// themselves are the same four, reused as-is.
	Comparison alert.Comparison
	Threshold  float64

	// TargetPercentage is the compliance target, e.g. 99.9.
	TargetPercentage float64
	// Window is how far back evaluation looks.
	Window time.Duration
	// WarningBudgetPercent overrides DefaultWarningBudgetPercent. Zero uses
	// the default.
	WarningBudgetPercent float64

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate reports whether the definition is well formed.
func (d Definition) Validate() error {
	const op = "slo.Definition.Validate"

	if d.Name == "" {
		return errs.New(errs.CodeInvalidArgument, "a name is required").WithOp(op)
	}
	if d.NodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "a node id is required").WithOp(op)
	}
	if d.Metric == "" {
		return errs.New(errs.CodeInvalidArgument, "a metric is required").WithOp(op)
	}
	if !d.Comparison.Valid() {
		return errs.New(errs.CodeInvalidArgument, "comparison must be one of gt, gte, lt, lte").WithOp(op)
	}
	if d.TargetPercentage <= 0 || d.TargetPercentage > 100 {
		return errs.New(errs.CodeInvalidArgument, "target_percentage must be between 0 and 100").WithOp(op)
	}
	if d.Window <= 0 {
		return errs.New(errs.CodeInvalidArgument, "a positive window is required").WithOp(op)
	}
	return nil
}

// warningBudgetPercent returns the configured threshold, or the default.
func (d Definition) warningBudgetPercent() float64 {
	if d.WarningBudgetPercent > 0 {
		return d.WarningBudgetPercent
	}
	return DefaultWarningBudgetPercent
}

// Evaluation is one SLO's compliance over its window, computed at request
// time.
type Evaluation struct {
	Definition Definition
	// Available is false when no samples exist in the window — nothing to
	// evaluate, not 0% or 100% compliance.
	Available bool

	SampleCount int
	Compliance  float64 // 0-100: percent of samples that were compliant.

	// ErrorBudget is 100 - TargetPercentage, the allowed non-compliant
	// share.
	ErrorBudget float64
	// ErrorBudgetConsumedPercent is how much of ErrorBudget has been used.
	// It can exceed 100: that is the SLO being over budget, a fact worth
	// keeping rather than clamping away.
	ErrorBudgetConsumedPercent float64
	Status                     Status

	WindowStart time.Time
	WindowEnd   time.Time
	EvaluatedAt time.Time
}

// SampleSource reads raw metric values for one node's metric over a range.
// Satisfied via an adapter over the existing metric repository's range
// query; see internal/app. Multiple label combinations for the same metric
// name (e.g. one series per network interface) are pooled into one set of
// values — each still evaluated independently against the threshold.
type SampleSource interface {
	Samples(ctx context.Context, nodeID, metric string, from, to time.Time) ([]float64, error)
}

// Store persists SLO definitions. Satisfied by
// [github.com/hexane/atlas/internal/storage/slo.Repository].
type Store interface {
	ListSLOs(ctx context.Context) ([]Definition, error)
	GetSLO(ctx context.Context, id string) (Definition, error)
	CreateSLO(ctx context.Context, def Definition) (Definition, error)
	UpdateSLO(ctx context.Context, def Definition) (Definition, error)
	DeleteSLO(ctx context.Context, id string) error
}

// Engine evaluates SLO definitions against historical samples. It holds no
// state and does no background work: every evaluation reads fresh.
type Engine struct {
	samples SampleSource
}

// Options configures an [Engine].
type Options struct {
	Samples SampleSource
}

// NewEngine builds an Engine.
func NewEngine(opts Options) *Engine {
	return &Engine{samples: opts.Samples}
}

// Evaluate computes def's current compliance over its window.
func (e *Engine) Evaluate(ctx context.Context, def Definition) (Evaluation, error) {
	if err := def.Validate(); err != nil {
		return Evaluation{}, err
	}

	now := time.Now()
	from := now.Add(-def.Window)
	values, err := e.samples.Samples(ctx, def.NodeID, def.Metric, from, now)
	if err != nil {
		return Evaluation{}, err
	}

	eval := Evaluation{Definition: def, WindowStart: from, WindowEnd: now, EvaluatedAt: now}
	if len(values) == 0 {
		return eval, nil
	}

	compliant := 0
	for _, v := range values {
		if def.Comparison.Evaluate(v, def.Threshold) {
			compliant++
		}
	}

	eval.Available = true
	eval.SampleCount = len(values)
	eval.Compliance = float64(compliant) / float64(len(values)) * 100
	eval.ErrorBudget = 100 - def.TargetPercentage

	switch {
	case eval.ErrorBudget <= 0:
		if eval.Compliance < 100 {
			eval.ErrorBudgetConsumedPercent = 100
		}
	default:
		eval.ErrorBudgetConsumedPercent = (100 - eval.Compliance) / eval.ErrorBudget * 100
	}

	// A tolerance against floating-point division landing a hair under an
	// exact boundary (e.g. 9/10 is not exactly representable), which would
	// otherwise flip a genuinely-breached SLO to warning by 1e-13.
	const boundaryEpsilon = 1e-6

	switch {
	case eval.ErrorBudgetConsumedPercent >= 100-boundaryEpsilon:
		eval.Status = StatusBreached
	case eval.ErrorBudgetConsumedPercent >= def.warningBudgetPercent()-boundaryEpsilon:
		eval.Status = StatusWarning
	default:
		eval.Status = StatusHealthy
	}

	return eval, nil
}
