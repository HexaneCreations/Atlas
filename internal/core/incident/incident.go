// Package incident correlates events and alert firings into incidents.
//
// An incident is a view over rows that already exist in
// [github.com/hexane/atlas/internal/core/eventstore] and
// [github.com/hexane/atlas/internal/core/alert] — this package never copies
// their payload or message, only enough (node, topic, severity, time) to
// group and summarise. The event and alert history remain the complete,
// authoritative record; an incident is what makes them navigable.
package incident

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Status is where an incident stands.
type Status string

const (
	StatusOpen     Status = "open"
	StatusResolved Status = "resolved"
)

// MemberKind is what an incident member references.
type MemberKind string

const (
	// MemberEvent references an eventstore.Record.
	MemberEvent MemberKind = "event"
	// MemberAlert references an alert.HistoryEntry.
	MemberAlert MemberKind = "alert"
)

// Severity is an incident's or a member's severity, on the scale the
// correlator uses to decide what is incident-worthy and to pick the
// incident's overall severity.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

func (s Severity) rank() int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	default:
		return 0
	}
}

// maxSeverity returns whichever of a, b ranks higher.
func maxSeverity(a, b Severity) Severity {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// Occurrence is one correlator input: an event or an alert transition,
// reduced to what correlation needs.
type Occurrence struct {
	Kind     MemberKind
	RefID    string
	NodeID   string
	Topic    string
	Severity Severity
	Time     time.Time
	// Title, when set, seeds the incident title if this occurrence opens a
	// new incident.
	Title string
}

// Incident is a correlated group of events and alert firings.
type Incident struct {
	ID       string
	Title    string
	Status   Status
	Severity Severity

	// Root cause is the earliest member of the incident — see
	// [Engine.correlate]. Populated once, at creation, and never revised:
	// a better answer than "what happened first" is future RCA work, not a
	// moving target inside this package.
	RootCauseKind  MemberKind
	RootCauseRefID string
	RootCauseTopic string

	OpenedAt   time.Time
	UpdatedAt  time.Time
	ResolvedAt time.Time
}

// DurationSeconds reports how long the incident has been open, or was open
// for, computed at read time rather than stored — see [metric.NodeResponse]
// for the same convention applied to uptime.
func (i Incident) DurationSeconds(now time.Time) float64 {
	end := i.ResolvedAt
	if end.IsZero() {
		end = now
	}
	return end.Sub(i.OpenedAt).Seconds()
}

// Member is one event or alert firing folded into an incident.
type Member struct {
	ID          string
	IncidentID  string
	Kind        MemberKind
	RefID       string
	NodeID      string
	Topic       string
	Severity    Severity
	Time        time.Time
	IsRootCause bool
}

// Detail is an incident with everything a detail view needs, computed from
// its members rather than stored redundantly on the incident itself.
type Detail struct {
	Incident
	Members          []Member
	AffectedNodes    []string
	AffectedSubjects []string
}

// Filter selects incidents for a list query, newest-updated first.
type Filter struct {
	Status Status
	NodeID string
	Since  time.Time
	Before time.Time
	Limit  int
}

// Store persists incidents and their members. Satisfied by
// [github.com/hexane/atlas/internal/storage/incident.Repository].
type Store interface {
	// FindCorrelatable returns the most recently updated open incident that
	// already has a member on nodeID at or after since, if one exists.
	FindCorrelatable(ctx context.Context, nodeID string, since time.Time) (Incident, bool, error)
	// FindCorrelatableByEnvironment returns the most recently updated open
	// incident that already has a member on some node tagged with
	// environment, at or after since — the cross-node correlation tier. See
	// [Engine.correlate].
	FindCorrelatableByEnvironment(ctx context.Context, environment string, since time.Time) (Incident, bool, error)
	CreateIncident(ctx context.Context, inc Incident) (Incident, error)
	UpdateIncident(ctx context.Context, inc Incident) error
	// AddMember is idempotent on (kind, ref_id): the same event or alert
	// entry can be offered to correlation more than once without error.
	AddMember(ctx context.Context, m Member) error

	ListIncidents(ctx context.Context, filter Filter) ([]Incident, error)
	GetIncident(ctx context.Context, id string) (Incident, error)
	GetDetail(ctx context.Context, id string) (Detail, error)
	ListOpenIncidents(ctx context.Context) ([]Incident, error)
}

// NodeEnvironments resolves a node's operator-assigned environment tag — the
// environment correlation tier's read path (see [Engine.correlate]). Empty
// means untagged: Atlas does not invent an environment for a node that was
// never told one, so an untagged node never joins the environment tier.
// Satisfied via a thin adapter over
// [github.com/hexane/atlas/internal/storage/metric.Repository.GetNode]; see
// internal/app.
type NodeEnvironments interface {
	Environment(ctx context.Context, nodeID string) (string, error)
}

// Validate reports whether an incident is well formed.
func (i Incident) Validate() error {
	const op = "incident.Incident.Validate"
	if i.Title == "" {
		return errs.New(errs.CodeInvalidArgument, "an incident requires a title").WithOp(op)
	}
	if i.Status != StatusOpen && i.Status != StatusResolved {
		return errs.New(errs.CodeInvalidArgument, "status must be %q or %q", StatusOpen, StatusResolved).WithOp(op)
	}
	return nil
}
