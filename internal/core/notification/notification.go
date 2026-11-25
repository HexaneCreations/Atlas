// Package notification defines current-state notifications: channels to
// deliver to, rules for what to watch, the evaluator's per-subject state,
// and the durable per-(event, channel) delivery queue.
//
// Storage is internal/storage/notification. Senders, the dispatcher and the
// evaluator build on these types.
package notification

import (
	"errors"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// ChannelType identifies a delivery mechanism. Validated here, not in the
// database, so adding one needs no migration.
type ChannelType string

const (
	ChannelEmail   ChannelType = "email"
	ChannelTeams   ChannelType = "teams"
	ChannelWebhook ChannelType = "webhook"
)

// KnownChannelTypes are the channel types a channel may be created with.
// email has no [Sender] registered yet (step 7 built teams and webhook
// only); a channel created with it enqueues normally and fails every
// delivery with "no sender registered for channel type", the same way an
// unregistered type always has.
var KnownChannelTypes = map[ChannelType]bool{ChannelEmail: true, ChannelTeams: true, ChannelWebhook: true}

// Severity orders how serious a notification is.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Valid reports whether s is a known severity.
func (s Severity) Valid() bool { return s == SeverityWarning || s == SeverityCritical }

// severityRank orders Severity for a channel's min_severity comparison:
// critical outranks warning. An unrecognised value ranks below every known
// severity, so [Engine.Notify] filters it out rather than treating it as
// meeting an unknown channel's threshold.
func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 1
	case SeverityWarning:
		return 0
	default:
		return -1
	}
}

// ErrSecretUnreadable is returned (wrapped) when a stored channel secret
// cannot be decrypted with the configured key. Its message never carries
// secret material.
var ErrSecretUnreadable = errors.New("the channel secret cannot be decrypted with the configured encryption key")

// Channel is a configured delivery destination as every reader sees it. It
// never holds the secret: only whether one is configured.
type Channel struct {
	ID          string
	Name        string
	Type        ChannelType
	Enabled     bool
	MinSeverity Severity
	// Config holds non-secret settings only.
	Config           map[string]any
	SecretConfigured bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ChannelInput is a channel to create or the replacement for an existing
// one. A nil Secret on update keeps the stored secret.
type ChannelInput struct {
	Name        string
	Type        ChannelType
	Enabled     bool
	MinSeverity Severity
	Config      map[string]any
	Secret      *string
}

// Validate reports whether the input is well formed. Type-specific config is
// validated by the sender for that type.
func (in ChannelInput) Validate() error {
	const op = "notification.ChannelInput.Validate"
	switch {
	case strings.TrimSpace(in.Name) == "":
		return errs.New(errs.CodeInvalidArgument, "a name is required").WithOp(op)
	case !KnownChannelTypes[in.Type]:
		return errs.New(errs.CodeInvalidArgument, "unknown channel type %q (want email, teams, or webhook)", in.Type).WithOp(op)
	case !in.MinSeverity.Valid():
		return errs.New(errs.CodeInvalidArgument, "min_severity must be warning or critical").WithOp(op)
	}
	return nil
}

// Trigger is a watched condition or event.
type Trigger string

const (
	TriggerNodeDown         Trigger = "node_down"
	TriggerDiskPct          Trigger = "disk_pct"
	TriggerRAMPct           Trigger = "ram_pct"
	TriggerContainerExit    Trigger = "container_exit"
	TriggerContainerRestart Trigger = "container_restart"
	TriggerServiceFailed    Trigger = "service_failed"
)

// KnownTriggers are every trigger a rule may name.
var KnownTriggers = map[Trigger]bool{
	TriggerNodeDown: true, TriggerDiskPct: true, TriggerRAMPct: true,
	TriggerContainerExit: true, TriggerContainerRestart: true, TriggerServiceFailed: true,
}

// UsesThresholds reports whether the trigger compares a percentage against
// warn/crit thresholds.
func (t Trigger) UsesThresholds() bool { return t == TriggerDiskPct || t == TriggerRAMPct }

// RuleSpec is a stored rule row. NodeID "" is the global default for its
// trigger and must be complete; a node override may leave any field nil to
// inherit the global value.
type RuleSpec struct {
	Trigger         Trigger
	NodeID          string
	Enabled         *bool
	WarnThreshold   *float64
	CritThreshold   *float64
	ForSeconds      *int
	RecoverySeconds *int
	Hysteresis      *float64
	CooldownSeconds *int
	SendRecovery    *bool
	// SubjectExcludes nil inherits; an empty non-nil slice excludes nothing.
	SubjectExcludes []string
	UpdatedAt       time.Time
	UpdatedBy       string
}

// IsGlobal reports whether the spec is a trigger's global default.
func (s RuleSpec) IsGlobal() bool { return s.NodeID == "" }

// Validate checks the spec on its own: a known trigger, non-negative
// durations, ordered thresholds, and, for a global row, every field set.
func (s RuleSpec) Validate() error {
	const op = "notification.RuleSpec.Validate"
	if !KnownTriggers[s.Trigger] {
		return errs.New(errs.CodeInvalidArgument, "unknown trigger %q", s.Trigger).WithOp(op)
	}
	for name, v := range map[string]*int{
		"for_seconds": s.ForSeconds, "recovery_seconds": s.RecoverySeconds, "cooldown_seconds": s.CooldownSeconds,
	} {
		if v != nil && *v < 0 {
			return errs.New(errs.CodeInvalidArgument, "%s must not be negative", name).WithOp(op)
		}
	}
	if s.Hysteresis != nil && *s.Hysteresis < 0 {
		return errs.New(errs.CodeInvalidArgument, "hysteresis must not be negative").WithOp(op)
	}
	if !s.Trigger.UsesThresholds() && (s.WarnThreshold != nil || s.CritThreshold != nil) {
		return errs.New(errs.CodeInvalidArgument, "trigger %q takes no thresholds", s.Trigger).WithOp(op)
	}
	if s.WarnThreshold != nil && s.CritThreshold != nil && *s.WarnThreshold > *s.CritThreshold {
		return errs.New(errs.CodeInvalidArgument, "warn_threshold must not exceed crit_threshold").WithOp(op)
	}
	if s.IsGlobal() {
		complete := s.Enabled != nil && s.ForSeconds != nil && s.RecoverySeconds != nil && s.Hysteresis != nil &&
			s.CooldownSeconds != nil && s.SendRecovery != nil && s.SubjectExcludes != nil
		if s.Trigger.UsesThresholds() {
			complete = complete && s.WarnThreshold != nil && s.CritThreshold != nil
		}
		if !complete {
			return errs.New(errs.CodeInvalidArgument, "a global rule must set every field").WithOp(op)
		}
	}
	return nil
}

// Rule is the effective rule for one trigger on one node: the global default
// with the node's override merged over it field by field.
type Rule struct {
	Trigger Trigger
	// NodeID is the node the rule was resolved for; "" for the global rule.
	NodeID          string
	Enabled         bool
	WarnThreshold   *float64
	CritThreshold   *float64
	ForSeconds      int
	RecoverySeconds int
	Hysteresis      float64
	CooldownSeconds int
	SendRecovery    bool
	SubjectExcludes []string
	// Overridden names the fields the node override supplied.
	Overridden []string
}

// MergeRule applies override (nil for none) over global, field by field. The
// global spec must be complete (see [RuleSpec.Validate]).
func MergeRule(global RuleSpec, override *RuleSpec, nodeID string) (Rule, error) {
	const op = "notification.MergeRule"
	if !global.IsGlobal() {
		return Rule{}, errs.New(errs.CodeInternal, "the base of a merge must be a global rule").WithOp(op)
	}
	if err := global.Validate(); err != nil {
		return Rule{}, err
	}
	r := Rule{
		Trigger: global.Trigger, NodeID: nodeID,
		Enabled: *global.Enabled, WarnThreshold: global.WarnThreshold, CritThreshold: global.CritThreshold,
		ForSeconds: *global.ForSeconds, RecoverySeconds: *global.RecoverySeconds, Hysteresis: *global.Hysteresis,
		CooldownSeconds: *global.CooldownSeconds, SendRecovery: *global.SendRecovery,
		SubjectExcludes: global.SubjectExcludes,
	}
	if override == nil {
		return r, nil
	}
	if override.Trigger != global.Trigger {
		return Rule{}, errs.New(errs.CodeInternal, "override trigger %q does not match %q", override.Trigger, global.Trigger).WithOp(op)
	}
	mergeVal(&r.Enabled, override.Enabled, "enabled", &r.Overridden)
	mergePtr(&r.WarnThreshold, override.WarnThreshold, "warn_threshold", &r.Overridden)
	mergePtr(&r.CritThreshold, override.CritThreshold, "crit_threshold", &r.Overridden)
	mergeVal(&r.ForSeconds, override.ForSeconds, "for_seconds", &r.Overridden)
	mergeVal(&r.RecoverySeconds, override.RecoverySeconds, "recovery_seconds", &r.Overridden)
	mergeVal(&r.Hysteresis, override.Hysteresis, "hysteresis", &r.Overridden)
	mergeVal(&r.CooldownSeconds, override.CooldownSeconds, "cooldown_seconds", &r.Overridden)
	mergeVal(&r.SendRecovery, override.SendRecovery, "send_recovery", &r.Overridden)
	if override.SubjectExcludes != nil {
		r.SubjectExcludes = override.SubjectExcludes
		r.Overridden = append(r.Overridden, "subject_excludes")
	}
	if r.WarnThreshold != nil && r.CritThreshold != nil && *r.WarnThreshold > *r.CritThreshold {
		return Rule{}, errs.New(errs.CodeInvalidArgument,
			"the effective warn_threshold %g exceeds crit_threshold %g", *r.WarnThreshold, *r.CritThreshold).WithOp(op)
	}
	return r, nil
}

func mergeVal[T any](dst *T, src *T, name string, overridden *[]string) {
	if src != nil {
		*dst = *src
		*overridden = append(*overridden, name)
	}
}

func mergePtr[T any](dst **T, src *T, name string, overridden *[]string) {
	if src != nil {
		v := *src
		*dst = &v
		*overridden = append(*overridden, name)
	}
}

// StateKind is where a (trigger, node, subject) stands in the evaluator.
type StateKind string

const (
	StateOK         StateKind = "ok"
	StatePending    StateKind = "pending"
	StateFiring     StateKind = "firing"
	StateRecovering StateKind = "recovering"
)

// State is the evaluator's memory for one (trigger, node, subject).
type State struct {
	Trigger Trigger
	NodeID  string
	// Subject is "" for node-level triggers, else the mountpoint, container
	// name or unit name.
	Subject          string
	State            StateKind
	Severity         Severity // "" while ok
	Value            *float64
	ConditionSince   *time.Time
	StateSince       time.Time
	LastNotifiedAt   *time.Time
	LastNotifiedKind Kind
	FlapCount        int
	FlapWindowStart  *time.Time
	Baseline         map[string]any
	UpdatedAt        time.Time
}

// Kind is what a delivered notification announces.
type Kind string

const (
	KindFiring    Kind = "firing"
	KindEscalated Kind = "escalated"
	KindRecovered Kind = "recovered"
	KindFlapping  Kind = "flapping"
	KindTest      Kind = "test"
)

// Status is where a delivery stands. It stays Pending across retries.
type Status string

const (
	StatusPending   Status = "pending"
	StatusDelivered Status = "delivered"
	StatusFailed    Status = "failed"
)

// Notification is one rendered message to fan out to channels. EventID is
// the dedupe key: offering it to a channel twice creates one delivery.
type Notification struct {
	EventID  string
	Kind     Kind
	Trigger  Trigger
	NodeID   string
	Subject  string
	Severity Severity
	Title    string
	Message  string
}

// Delivery is one (event, channel) delivery record.
type Delivery struct {
	ID            string
	EventID       string
	ChannelID     string
	ChannelType   ChannelType
	Kind          Kind
	Trigger       Trigger
	NodeID        string
	Subject       string
	Severity      Severity
	Title         string
	Message       string
	Status        Status
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	DeliveredAt   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DeliveryCursor marks a position in the newest-first delivery log.
type DeliveryCursor struct {
	CreatedAt time.Time
	ID        string
}

// DeliveryFilter selects a page of the delivery log, newest first.
type DeliveryFilter struct {
	ChannelID string
	Status    Status
	NodeID    string
	Trigger   Trigger
	Limit     int
	// Before, when set, returns only deliveries strictly older than it.
	Before *DeliveryCursor
}

// DeliveryPage is one page of the delivery log. Next is nil on the last page.
type DeliveryPage struct {
	Deliveries []Delivery
	Next       *DeliveryCursor
}
