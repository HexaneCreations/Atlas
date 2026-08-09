// Package alert evaluates alert rules against metrics and events, and keeps
// the durable state and history a fleet-wide dashboard reads from.
//
// Two rule kinds share one lifecycle. A threshold rule is a sustained
// condition — evaluated on a timer, with a pending/firing state machine so a
// single spike does not alert. An event rule is edge-triggered — it fires
// once per matching occurrence from [github.com/hexane/atlas/internal/core/eventstore],
// with no pending state, because there is nothing to sustain.
package alert

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Kind is what a rule evaluates.
type Kind string

const (
	// KindThreshold compares a metric's latest value against a bound,
	// sustained for [Rule.For] before it fires.
	KindThreshold Kind = "threshold"
	// KindEvent fires once for every event matching [Rule.Topic].
	KindEvent Kind = "event"
)

// Comparison is how a threshold rule compares a value to its bound.
type Comparison string

const (
	ComparisonGT  Comparison = "gt"
	ComparisonGTE Comparison = "gte"
	ComparisonLT  Comparison = "lt"
	ComparisonLTE Comparison = "lte"
)

// Valid reports whether c is a known comparison.
func (c Comparison) Valid() bool {
	switch c {
	case ComparisonGT, ComparisonGTE, ComparisonLT, ComparisonLTE:
		return true
	default:
		return false
	}
}

// Evaluate reports whether value satisfies the comparison against bound.
func (c Comparison) Evaluate(value, bound float64) bool {
	switch c {
	case ComparisonGT:
		return value > bound
	case ComparisonGTE:
		return value >= bound
	case ComparisonLT:
		return value < bound
	case ComparisonLTE:
		return value <= bound
	default:
		return false
	}
}

// Severity is how urgently a firing rule should draw attention.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Valid reports whether s is a known severity.
func (s Severity) Valid() bool {
	return s == SeverityWarning || s == SeverityCritical
}

// State is where a (rule, node, series) currently stands.
type State string

const (
	StateOK      State = "ok"
	StatePending State = "pending"
	StateFiring  State = "firing"
)

// Rule defines a condition that produces alerts when true.
type Rule struct {
	ID          string
	Name        string
	Description string
	Enabled     bool
	Kind        Kind
	Severity    Severity

	// Threshold fields, set when Kind == KindThreshold.
	Metric     string
	Comparison Comparison
	Threshold  float64
	// For is how long the condition must hold before the rule fires. Zero
	// fires immediately on the first breach.
	For time.Duration
	// NodeID scopes the rule to one node. Empty evaluates every node
	// reporting Metric.
	NodeID string

	// Event fields, set when Kind == KindEvent.
	Topic string
	// Subject narrows to one resource (a container id, a unit name). Empty
	// matches any subject.
	Subject string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate reports whether the rule is well formed for its kind.
func (r Rule) Validate() error {
	const op = "alert.Rule.Validate"

	if r.Name == "" {
		return errs.New(errs.CodeInvalidArgument, "a rule name is required").WithOp(op)
	}
	if !r.Severity.Valid() {
		return errs.New(errs.CodeInvalidArgument, "severity must be %q or %q", SeverityWarning, SeverityCritical).WithOp(op)
	}

	switch r.Kind {
	case KindThreshold:
		if r.Metric == "" {
			return errs.New(errs.CodeInvalidArgument, "a threshold rule requires a metric").WithOp(op)
		}
		if !r.Comparison.Valid() {
			return errs.New(errs.CodeInvalidArgument, "comparison must be one of gt, gte, lt, lte").WithOp(op)
		}
		if r.For < 0 {
			return errs.New(errs.CodeInvalidArgument, "for_seconds cannot be negative").WithOp(op)
		}
	case KindEvent:
		if r.Topic == "" {
			return errs.New(errs.CodeInvalidArgument, "an event rule requires a topic").WithOp(op)
		}
	default:
		return errs.New(errs.CodeInvalidArgument, "kind must be %q or %q", KindThreshold, KindEvent).WithOp(op)
	}
	return nil
}

// AlertState is the current status of one rule against one node's series.
type AlertState struct {
	RuleID    string
	NodeID    string
	SeriesKey string
	State     State
	Value     float64
	Message   string

	PendingSince time.Time
	FiredAt      time.Time
	ResolvedAt   time.Time
	UpdatedAt    time.Time
}

// HistoryEntry is one durable transition: a rule started firing, or resolved.
type HistoryEntry struct {
	ID       string
	Time     time.Time
	RuleID   string
	NodeID   string
	State    State
	Severity Severity
	Value    float64
	Message  string
}

// HistoryFilter selects history entries, newest first.
type HistoryFilter struct {
	RuleID string
	NodeID string
	Since  time.Time
	Before time.Time
	Limit  int
}

// Store persists rules, their live state, and their history. Satisfied by
// [github.com/hexane/atlas/internal/storage/alert.Repository].
type Store interface {
	ListRules(ctx context.Context) ([]Rule, error)
	GetRule(ctx context.Context, id string) (Rule, error)
	CreateRule(ctx context.Context, rule Rule) (Rule, error)
	UpdateRule(ctx context.Context, rule Rule) (Rule, error)
	DeleteRule(ctx context.Context, id string) error

	GetState(ctx context.Context, ruleID, nodeID, seriesKey string) (AlertState, bool, error)
	SaveState(ctx context.Context, state AlertState) error
	ListActiveStates(ctx context.Context) ([]AlertState, error)

	AppendHistory(ctx context.Context, entry HistoryEntry) error
	QueryHistory(ctx context.Context, filter HistoryFilter) ([]HistoryEntry, error)
}

// FleetSample is one node's latest value of a metric, the read path the
// threshold evaluator needs. Satisfied by
// [github.com/hexane/atlas/internal/storage/metric.FleetLatestValue].
type FleetSample struct {
	NodeID string
	Labels map[string]string
	Value  float64
	Time   time.Time
}

// MetricSource is the metric read path the threshold evaluator needs.
type MetricSource interface {
	LatestForMetric(ctx context.Context, metric string, within time.Duration) ([]FleetSample, error)
}
