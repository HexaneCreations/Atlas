package incident

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/activity"
	"github.com/hexane/atlas/internal/core/alert"
	"github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/id"
)

func activityTopic(topic string) eventbus.Topic { return eventbus.Topic(topic) }

// fromActivitySeverity maps the activity feed's four-level scale onto the
// three-level incident scale: only warning and danger ever reach here (see
// [Engine.HandleEvent]'s filter), and danger is what incidents call critical.
func fromActivitySeverity(s activity.Severity) Severity {
	if s == activity.SeverityDanger {
		return SeverityCritical
	}
	return SeverityWarning
}

func fromAlertSeverity(s alert.Severity) Severity {
	if s == alert.SeverityCritical {
		return SeverityCritical
	}
	return SeverityWarning
}

const (
	// DefaultWindow is how close in time two occurrences on the same node
	// must be to join one incident.
	DefaultWindow = 10 * time.Minute
	// DefaultResolveAfter is how long an incident must see no new
	// correlated activity before it auto-resolves.
	DefaultResolveAfter = 15 * time.Minute
	// resolveCheckInterval is how often the resolver sweeps open incidents.
	resolveCheckInterval = time.Minute
)

// Engine correlates events and alert firings into incidents.
//
// Correlation runs two tiers. Same-node within [Engine.window] is tried
// first. When that finds nothing and [Engine.environments] is configured, an
// occurrence also joins the most recently updated open incident that has a
// member on any node sharing its environment tag within the same window —
// cross-node correlation for a fleet-wide cascading failure, coarse-grained
// on the one grouping fact every node already carries. Finer, topology-aware
// correlation (service dependency, network adjacency) is future work; it
// slots in as a further tier over the same [Store] and [Occurrence] shape.
type Engine struct {
	store        Store
	environments NodeEnvironments
	window       time.Duration
	resolveAfter time.Duration
	logger       *slog.Logger

	mu     sync.Mutex
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options configures an [Engine].
type Options struct {
	Store Store
	// Environments enables the cross-node environment correlation tier. Nil
	// keeps correlation same-node only.
	Environments NodeEnvironments
	Window       time.Duration
	ResolveAfter time.Duration
	Logger       *slog.Logger
}

// NewEngine builds an Engine. It does not resolve anything until Run.
func NewEngine(opts Options) *Engine {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Window <= 0 {
		opts.Window = DefaultWindow
	}
	if opts.ResolveAfter <= 0 {
		opts.ResolveAfter = DefaultResolveAfter
	}
	return &Engine{
		store: opts.Store, environments: opts.Environments,
		window: opts.Window, resolveAfter: opts.ResolveAfter, logger: opts.Logger,
	}
}

// Run sweeps open incidents for auto-resolution on a timer until ctx is
// cancelled or Stop is called.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	stopCh := make(chan struct{})
	e.stopCh = stopCh
	e.mu.Unlock()

	e.wg.Add(1)
	defer e.wg.Done()

	ticker := time.NewTicker(resolveCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			e.resolveQuiet(ctx)
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

// HandleEvent correlates one durably stored event, if its topic is
// notable enough to be incident-worthy. Wired via [eventstore.Tap] on both
// the local event writer and the fleet ingest receiver — see internal/app.
func (e *Engine) HandleEvent(ctx context.Context, rec eventstore.Record) {
	sev, ok := activity.Classify(activityTopic(rec.Topic))
	if !ok || (sev != activity.SeverityWarning && sev != activity.SeverityDanger) {
		// Routine churn (container started, hostname renamed) is real
		// activity-feed material but not, on its own, an incident.
		return
	}

	e.correlate(ctx, Occurrence{
		Kind: MemberEvent, RefID: rec.ID, NodeID: rec.NodeID, Topic: rec.Topic,
		Severity: fromActivitySeverity(sev), Time: rec.Time, Title: activity.TitleFor(activityTopic(rec.Topic)),
	})
}

// HandleAlertTransition correlates a rule firing. Resolutions
// ([alert.StateOK]) are not correlated directly — an incident they belong to
// resolves through [Engine.resolveQuiet] once activity goes quiet, the same
// policy applied uniformly to event- and alert-originated incidents.
func (e *Engine) HandleAlertTransition(ctx context.Context, entry alert.HistoryEntry) {
	if entry.State != alert.StateFiring {
		return
	}

	e.correlate(ctx, Occurrence{
		Kind: MemberAlert, RefID: entry.ID, NodeID: entry.NodeID, Topic: "alert:" + entry.RuleID,
		Severity: fromAlertSeverity(entry.Severity), Time: entry.Time, Title: entry.Message,
	})
}

func (e *Engine) correlate(ctx context.Context, occ Occurrence) {
	since := occ.Time.Add(-e.window)
	inc, found, err := e.store.FindCorrelatable(ctx, occ.NodeID, since)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not search for a correlatable incident",
			slog.String("node_id", occ.NodeID), slog.String("error", err.Error()))
		return
	}

	if !found && e.environments != nil {
		inc, found, err = e.correlateByEnvironment(ctx, occ, since)
		if err != nil {
			e.logger.ErrorContext(ctx, "could not search for a correlatable incident by environment",
				slog.String("node_id", occ.NodeID), slog.String("error", err.Error()))
			return
		}
	}

	if !found {
		title := occ.Title
		if title == "" {
			title = occ.Topic
		}
		inc = Incident{
			ID: id.New(), Title: title, Status: StatusOpen, Severity: occ.Severity,
			RootCauseKind: occ.Kind, RootCauseRefID: occ.RefID, RootCauseTopic: occ.Topic,
			OpenedAt: occ.Time, UpdatedAt: occ.Time,
		}
		created, err := e.store.CreateIncident(ctx, inc)
		if err != nil {
			e.logger.ErrorContext(ctx, "could not open an incident", slog.String("error", err.Error()))
			return
		}
		inc = created
	} else {
		inc.Severity = maxSeverity(inc.Severity, occ.Severity)
		inc.UpdatedAt = occ.Time
		if err := e.store.UpdateIncident(ctx, inc); err != nil {
			e.logger.ErrorContext(ctx, "could not update an incident", slog.String("incident_id", inc.ID), slog.String("error", err.Error()))
			return
		}
	}

	member := Member{
		ID: id.New(), IncidentID: inc.ID, Kind: occ.Kind, RefID: occ.RefID,
		NodeID: occ.NodeID, Topic: occ.Topic, Severity: occ.Severity, Time: occ.Time,
		IsRootCause: inc.RootCauseKind == occ.Kind && inc.RootCauseRefID == occ.RefID,
	}
	if err := e.store.AddMember(ctx, member); err != nil {
		e.logger.ErrorContext(ctx, "could not add an incident member",
			slog.String("incident_id", inc.ID), slog.String("error", err.Error()))
	}
}

// correlateByEnvironment is the environment correlation tier: it runs only
// when occ found no same-node incident. An untagged node (empty environment)
// never joins this tier — Atlas will not invent an environment for a node
// that was never told one.
func (e *Engine) correlateByEnvironment(ctx context.Context, occ Occurrence, since time.Time) (Incident, bool, error) {
	env, err := e.environments.Environment(ctx, occ.NodeID)
	if err != nil {
		return Incident{}, false, err
	}
	if env == "" {
		return Incident{}, false, nil
	}
	return e.store.FindCorrelatableByEnvironment(ctx, env, since)
}

func (e *Engine) resolveQuiet(ctx context.Context) {
	open, err := e.store.ListOpenIncidents(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not list open incidents", slog.String("error", err.Error()))
		return
	}

	now := time.Now()
	for _, inc := range open {
		if now.Sub(inc.UpdatedAt) < e.resolveAfter {
			continue
		}
		inc.Status, inc.ResolvedAt = StatusResolved, now
		if err := e.store.UpdateIncident(ctx, inc); err != nil {
			e.logger.ErrorContext(ctx, "could not resolve an incident",
				slog.String("incident_id", inc.ID), slog.String("error", err.Error()))
		}
	}
}
