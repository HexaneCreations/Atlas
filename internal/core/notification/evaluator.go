package notification

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

const (
	// DefaultEvaluationInterval is how often the evaluator re-checks every
	// node and series.
	DefaultEvaluationInterval = 30 * time.Second

	// NodeDownAfter is how long a node may go unseen before node_down's
	// condition is true. for_seconds (seeded at 480) is a hold applied on
	// top of this, not instead of it: a node has to be silent for this
	// long, continuously, for for_seconds more, before node_down fires.
	NodeDownAfter = 120 * time.Second

	// DiskCollectorInterval mirrors system.disk's own Descriptor.Interval
	// (internal/plugin/system/collector_disk.go): a fixed 60s override, not
	// the configurable collection default, because capacity moves slowly
	// and statfs is comparatively expensive. Since it is a fixed override
	// in the collector itself, it is a constant here too, not an Option.
	DiskCollectorInterval = 60 * time.Second
	// DefaultRAMCollectorInterval mirrors system.memory's Descriptor, which
	// sets no Interval of its own and so falls back to whichever
	// collection.default_interval the deployment runs with
	// (config.Default()'s own default is this same 15s). Overridable via
	// EvaluatorOptions.RAMCollectorInterval for a deployment that changes
	// collection.default_interval away from its default.
	DefaultRAMCollectorInterval = 15 * time.Second

	// StaleFactor is how many collector intervals old a metric_latest
	// sample may be and still count as current — see item 1 of the brief
	// this implements.
	StaleFactor = 3

	// MetricDiskUsage and MetricRAMUsage are the metric_latest names the
	// collectors already write for disk and memory usage percent —
	// internal/plugin/system/collector_disk.go and collector_memory.go.
	// Disk is per mount point, labelled "mountpoint" (also "device" and
	// "fstype", which the evaluator does not need); RAM is a single
	// unlabelled node-wide gauge.
	MetricDiskUsage = "system.disk.usage"
	MetricRAMUsage  = "system.memory.usage"
	// diskMountpointLabel is the disk series' label naming its subject.
	diskMountpointLabel = "mountpoint"
)

// EvalNode is the subset of a node's record the evaluator needs: enough to
// decide node_down's own condition and every node's eligibility, without
// depending on [metric.Node] (a core package may not import a storage
// one) or exposing them through the storage layer's public API types.
type EvalNode struct {
	NodeID     string
	LastSeenAt time.Time
	// ArchivedAt and AlertsMutedUntil are nil in the common case — see
	// [RuleStore] for why both fully exclude a node, independent of this
	// evaluator's own notification_state.
	ArchivedAt       *time.Time
	AlertsMutedUntil *time.Time
}

// MetricSample is one node's freshest value of one metric, within
// whatever staleness window the caller asked [MetricSource.LatestForMetric]
// for.
type MetricSample struct {
	NodeID string
	Labels map[string]string
	Value  float64
	Time   time.Time
}

// RuleStore is what the evaluator needs to resolve a trigger's effective
// rule for a node. Satisfied by [github.com/hexane/atlas/internal/storage/
// notification.Repository].
type RuleStore interface {
	EffectiveRule(ctx context.Context, trigger Trigger, nodeID string) (Rule, error)
}

// StateStore is the evaluator's persistent memory, per (trigger, node,
// subject). Satisfied by the same Repository as RuleStore.
type StateStore interface {
	GetState(ctx context.Context, trigger Trigger, nodeID, subject string) (State, bool, error)
	UpsertState(ctx context.Context, s State) error
	// EndMuteAfterExpiry ends nodeID's mute — clearing alerts_muted_until
	// and silently resetting every non-ok state row for the node to ok,
	// without going through the notifier — but only if the mute still
	// reads as expired (not after now) at the moment of the write. did is
	// false, with nothing cleared and nothing reset, when a concurrent
	// re-mute's own alerts_muted_until won that race; see
	// [Evaluator.endExpiredMute] and the second fix-round's report for why
	// this has to be one atomic, guarded operation rather than the bare,
	// unconditional reset the first fix round shipped (it ran every tick
	// for as long as the column stayed in the past — forever, since
	// nothing ever cleared it — wiping out any new transition the node
	// started since the mute ended, so the node could never alert again).
	EndMuteAfterExpiry(ctx context.Context, nodeID string, now time.Time) (did bool, err error)
	// ListNodeStates returns every state row of one node for the given
	// triggers.
	ListNodeStates(ctx context.Context, nodeID string, triggers ...Trigger) ([]State, error)
	// BeginPending records s (a pending state) unless the stored row has
	// already left ok, and returns the row as it then stands: s, or whatever
	// a concurrent evaluator recorded first. Both evaluators therefore
	// continue from one ConditionSince and build the same event id.
	BeginPending(ctx context.Context, s State) (State, error)
	// DeleteState removes one state row; a missing row is not an error.
	DeleteState(ctx context.Context, trigger Trigger, nodeID, subject string) error
}

// NodeStore is what the evaluator needs from the node repository: every
// node's liveness and the two columns that remove it from evaluation
// entirely. Satisfied by an adapter over [metric.Repository.ListNodes] —
// core/notification cannot import internal/storage/metric directly.
type NodeStore interface {
	ListNodes(ctx context.Context) ([]EvalNode, error)
}

// MetricSource is the evaluator's read path for every metric it watches.
// Satisfied by an adapter over [metric.Repository.LatestForMetric].
type MetricSource interface {
	// LatestForMetric returns the freshest sample per (node, labels) for
	// metric, among those no older than within. A sample older than that
	// (or missing entirely) simply does not appear — which is exactly the
	// staleness behaviour item 1 of the brief wants: the evaluator only
	// ever sees fresh-enough data, so it cannot make a transition from
	// stale data because stale data is never handed to it.
	LatestForMetric(ctx context.Context, metric string, within time.Duration) ([]MetricSample, error)
}

// Notifier is what the evaluator calls on a transition. Satisfied by
// *[Engine] itself — same package, no adapter needed.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// EvaluatorOptions configures an [Evaluator].
type EvaluatorOptions struct {
	Rules    RuleStore
	States   StateStore
	Nodes    NodeStore
	Metrics  MetricSource
	Notifier Notifier
	Logger   *slog.Logger

	// Now returns the current time. Defaults to time.Now; tests supply a
	// fake so the whole state machine runs without a real sleep anywhere —
	// item 5 of the brief. The evaluator never uses an agent-reported
	// timestamp (a metric sample's own Time is used only to decide
	// freshness within LatestForMetric's window, already resolved by the
	// time a sample reaches this package) for staleness or hold timing.
	Now func() time.Time

	Interval time.Duration
	// RAMCollectorInterval overrides DefaultRAMCollectorInterval — see its
	// doc comment.
	RAMCollectorInterval time.Duration
	// ContainerCollectorInterval and ServiceCollectorInterval are the
	// registered intervals of the collectors behind docker.container.* and
	// service.*. There is no default: with one unset, its triggers are not
	// evaluated rather than evaluated against an assumed staleness window.
	ContainerCollectorInterval time.Duration
	ServiceCollectorInterval   time.Duration

	// LocalNodeID is this server's own node id — the same value
	// internal/platform/hostid.Resolve already produces at startup and
	// internal/app/app.go already threads into the local collection
	// pipeline. The evaluator skips it entirely: see item 3 of the brief.
	LocalNodeID string
}

// Evaluator is the periodic rule evaluator for disk_pct, ram_pct and
// node_down, and (evaluator_workloads.go) container_exit,
// container_restart and service_failed.
//
// Every tick it reads current data, resolves each trigger's effective
// rule (global merged with any per-node override, through [MergeRule]),
// advances notification_state's state machine, and calls Notifier.Notify
// on a transition that warrants one.
type Evaluator struct {
	rules       RuleStore
	states      StateStore
	nodes       NodeStore
	metrics     MetricSource
	notifier    Notifier
	logger      *slog.Logger
	now         func() time.Time
	interval    time.Duration
	ramInterval time.Duration
	localNodeID string

	containerInterval time.Duration
	serviceInterval   time.Duration

	mu     sync.Mutex
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewEvaluator builds an Evaluator. It does not evaluate anything until Run.
func NewEvaluator(opts EvaluatorOptions) *Evaluator {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultEvaluationInterval
	}
	if opts.RAMCollectorInterval <= 0 {
		opts.RAMCollectorInterval = DefaultRAMCollectorInterval
	}
	return &Evaluator{
		rules: opts.Rules, states: opts.States, nodes: opts.Nodes, metrics: opts.Metrics, notifier: opts.Notifier,
		logger: opts.Logger, now: opts.Now, interval: opts.Interval, ramInterval: opts.RAMCollectorInterval,
		localNodeID:       opts.LocalNodeID,
		containerInterval: opts.ContainerCollectorInterval, serviceInterval: opts.ServiceCollectorInterval,
	}
}

// Run evaluates on a timer until ctx is cancelled or Stop is called.
func (e *Evaluator) Run(ctx context.Context) {
	e.mu.Lock()
	stopCh := make(chan struct{})
	e.stopCh = stopCh
	e.mu.Unlock()

	e.wg.Add(1)
	defer e.wg.Done()

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			e.EvaluateOnce(ctx)
		}
	}
}

// Stop ends Run and waits for it to return. Unlike [Engine.Stop], this
// takes no context: the evaluator's own work per tick is database reads
// and writes with no real way to hang, the same reasoning
// [corealert.Engine.Stop] and [coreincident.Engine.Stop] already use.
func (e *Evaluator) Stop() {
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

// EvaluateOnce runs one full evaluation pass. Exported so a test can
// trigger a deterministic tick instead of racing a timer.
func (e *Evaluator) EvaluateOnce(ctx context.Context) {
	nodes, err := e.nodes.ListNodes(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not list nodes for notification evaluation", slog.String("error", err.Error()))
		return
	}

	disk, err := e.metrics.LatestForMetric(ctx, MetricDiskUsage, StaleFactor*DiskCollectorInterval)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read disk usage for notification evaluation", slog.String("error", err.Error()))
		disk = nil // node_down still runs; disk just sees no fresh data this tick
	}
	ram, err := e.metrics.LatestForMetric(ctx, MetricRAMUsage, StaleFactor*e.ramInterval)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read ram usage for notification evaluation", slog.String("error", err.Error()))
		ram = nil
	}

	diskByNode := map[string][]MetricSample{}
	for _, s := range disk {
		diskByNode[s.NodeID] = append(diskByNode[s.NodeID], s)
	}
	ramByNode := map[string]MetricSample{}
	for _, s := range ram {
		ramByNode[s.NodeID] = s
	}

	workloads := e.readWorkloads(ctx)

	// Microseconds are what the state table stores. A finer now could read
	// back later than itself, and a zero-second hold would not have elapsed.
	now := e.now().Truncate(time.Microsecond)
	for _, n := range nodes {
		e.evaluateNode(ctx, n, diskByNode[n.NodeID], ramByNode[n.NodeID], workloads[n.NodeID], now)
	}
}

func (e *Evaluator) evaluateNode(
	ctx context.Context, n EvalNode, disk []MetricSample, ram MetricSample, workloads *nodeWorkloads, now time.Time,
) {
	// Skip entirely, touching no state at all — item 3 of the brief.
	if n.NodeID == e.localNodeID {
		return
	}
	if n.ArchivedAt != nil {
		return
	}
	if n.AlertsMutedUntil != nil {
		if n.AlertsMutedUntil.After(now) {
			return // muted: not read, not written, full stop
		}
		// Not muted now, but was at some point (the column is still set,
		// just in the past): end the mute — clearing the column and
		// resetting every frozen state silently, atomically — before
		// normal evaluation resumes. If that does not happen this tick
		// (an error, or a concurrent re-mute got there first), the node
		// must not be evaluated against states that may still need to
		// stay frozen.
		if !e.endExpiredMute(ctx, n, now) {
			return
		}
	}

	nodeDownFiring := e.evaluateNodeDown(ctx, n, now)
	// While node_down is firing, every other trigger is suppressed entirely —
	// item 2. This also satisfies "re-evaluate fresh data before sending
	// recovered": once node_down stops firing, disk/ram resume from
	// whatever is actually fresh at that moment (staleness, handled
	// separately above, still applies to it).
	if nodeDownFiring {
		return
	}

	e.evaluateDisk(ctx, n.NodeID, disk, now)
	if ram.NodeID != "" {
		e.evaluateRAM(ctx, n.NodeID, ram.Value, now)
	}
	e.evaluateWorkloads(ctx, n.NodeID, workloads, now)
}

// endExpiredMute ends a mute whose alerts_muted_until the evaluator just
// read as non-nil and not after now: clearing the column and resetting
// every non-ok notification_state row for the node to ok, under every
// trigger and subject, both in the one transaction EndMuteAfterExpiry
// runs. This must happen at most once *per mute*, not once per tick: a
// previous version of this code ran an unconditional, unguarded reset
// (then called resetNodeStates) on every single tick for as long as
// alerts_muted_until stayed in the past — which, since nothing in that
// version ever cleared the column, was forever. Any new pending
// transition the node started after the mute truly ended was wiped back
// to ok on the very next tick before it could ever complete its hold, so
// the node could never alert again after being muted once. That version's
// own comment called this "idempotent," which was true only in the
// narrow sense that it found nothing to reset once the node was all-ok;
// it was not idempotent against the node's own ability to alert again,
// which is what actually mattered and what the fix round's tests had not
// exercised (they only ever ran one tick past expiry).
//
// The fix is structural, not a guard added on top: EndMuteAfterExpiry
// clears alerts_muted_until as part of the very same write that resets
// state, so after it succeeds the column reads NULL and the next tick's
// own "AlertsMutedUntil != nil" check above is false — this whole branch
// is simply never reached again for that mute. Nothing needs to
// remember "already did this one."
//
// did is false, with nothing cleared or reset, in two cases the caller
// must treat identically — do not evaluate this node this tick: a
// concurrent re-mute's own, later alerts_muted_until won the race against
// this call (EndMuteAfterExpiry's own guard is what catches that), or the
// write itself failed (logged here; the node is simply skipped rather
// than evaluated against states that may still need to stay frozen).
func (e *Evaluator) endExpiredMute(ctx context.Context, n EvalNode, now time.Time) (did bool) {
	did, err := e.states.EndMuteAfterExpiry(ctx, n.NodeID, now)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not end a node's expired mute; skipping it this tick",
			slog.String("node_id", n.NodeID), slog.String("error", err.Error()))
		return false
	}
	return did
}

func (e *Evaluator) evaluateNodeDown(ctx context.Context, n EvalNode, now time.Time) (firing bool) {
	rule, err := e.rules.EffectiveRule(ctx, TriggerNodeDown, n.NodeID)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not resolve the node_down rule", slog.String("node_id", n.NodeID))
		return false
	}
	if !rule.Enabled {
		return false
	}

	conditionTrue := now.Sub(n.LastSeenAt) > NodeDownAfter
	always := func(*float64) Severity { return SeverityCritical }
	next := e.evaluateSubject(ctx, rule, TriggerNodeDown, n.NodeID, "", conditionTrue, nil, always, now,
		"Node unreachable", fmt.Sprintf("node %s has not reported for over %s", n.NodeID, NodeDownAfter))
	return next == StateFiring
}

func (e *Evaluator) evaluateDisk(ctx context.Context, nodeID string, samples []MetricSample, now time.Time) {
	if len(samples) == 0 {
		return
	}
	rule, err := e.rules.EffectiveRule(ctx, TriggerDiskPct, nodeID)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not resolve the disk_pct rule", slog.String("node_id", nodeID))
		return
	}
	if !rule.Enabled || rule.WarnThreshold == nil {
		return
	}

	for _, s := range samples {
		mountpoint := s.Labels[diskMountpointLabel]
		if slices.Contains(rule.SubjectExcludes, mountpoint) {
			continue
		}
		e.evaluateThreshold(ctx, rule, TriggerDiskPct, nodeID, mountpoint, s.Value, now,
			func(sev Severity) (string, string) {
				return "Disk usage " + string(sev), fmt.Sprintf("%s on %s is at %.1f%%", mountpoint, nodeID, s.Value)
			})
	}
}

func (e *Evaluator) evaluateRAM(ctx context.Context, nodeID string, value float64, now time.Time) {
	rule, err := e.rules.EffectiveRule(ctx, TriggerRAMPct, nodeID)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not resolve the ram_pct rule", slog.String("node_id", nodeID))
		return
	}
	if !rule.Enabled || rule.WarnThreshold == nil {
		return
	}
	e.evaluateThreshold(ctx, rule, TriggerRAMPct, nodeID, "", value, now,
		func(sev Severity) (string, string) {
			return "RAM usage " + string(sev), fmt.Sprintf("%s is at %.1f%%", nodeID, value)
		})
}

// evaluateThreshold is disk_pct and ram_pct's shared condition and
// severity logic: the entry threshold is warn_threshold; once the
// subject is already flagged, hysteresis lowers the bound that counts as
// recovered, and demoting severity from critical back to warning needs
// the same margin before it is lowered — both exactly as the brief's
// "Hysteresis for disk and ram" and "a decrease needs the hysteresis
// margin" lines ask for.
func (e *Evaluator) evaluateThreshold(
	ctx context.Context, rule Rule, trigger Trigger, nodeID, subject string, value float64, now time.Time,
	titleMessage func(Severity) (string, string),
) {
	current, found, err := e.states.GetState(ctx, trigger, nodeID, subject)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read notification state",
			slog.String("trigger", string(trigger)), slog.String("node_id", nodeID))
		return
	}
	alreadyActive := found && current.State != StateOK

	warn := *rule.WarnThreshold
	conditionTrue := value >= warn
	if alreadyActive {
		conditionTrue = value >= warn-rule.Hysteresis
	}

	severityOf := func(v *float64) Severity {
		return thresholdSeverity(*v, rule, current.Severity)
	}

	v := value
	e.evaluateSubjectWithTitle(ctx, rule, trigger, nodeID, subject, current, found, conditionTrue, &v, severityOf, now, titleMessage, nil)
}

// thresholdSeverity resolves warn vs critical for a true condition.
// Escalating (warning -> critical) happens the instant value reaches
// crit_threshold — the brief's "a severity increase ... sends escalated"
// is about this being immediate. Demoting needs the hysteresis margin,
// the same way recovering does.
func thresholdSeverity(value float64, rule Rule, current Severity) Severity {
	if rule.CritThreshold == nil {
		return SeverityWarning
	}
	if current == SeverityCritical {
		if value < *rule.CritThreshold-rule.Hysteresis {
			return SeverityWarning
		}
		return SeverityCritical
	}
	if value >= *rule.CritThreshold {
		return SeverityCritical
	}
	return SeverityWarning
}

// evaluateSubject is node_down's entry point: no stored value, a fixed
// severity, and a title/message built from the node id alone.
func (e *Evaluator) evaluateSubject(
	ctx context.Context, rule Rule, trigger Trigger, nodeID, subject string, conditionTrue bool, value *float64,
	severityOf func(*float64) Severity, now time.Time, title, message string,
) StateKind {
	current, found, err := e.states.GetState(ctx, trigger, nodeID, subject)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not read notification state",
			slog.String("trigger", string(trigger)), slog.String("node_id", nodeID))
		return StateOK // unknown; never suppress on the strength of an error we could not even read past
	}
	return e.evaluateSubjectWithTitle(ctx, rule, trigger, nodeID, subject, current, found, conditionTrue, value,
		severityOf, now, func(Severity) (string, string) { return title, message }, nil)
}

// evaluateSubjectWithTitle runs the shared state machine (computeTransition)
// for one (trigger, node, subject), then enqueues a notification (if any)
// strictly before persisting the new state — see the report's restart-
// safety section for why that order, not the reverse, is what item 4
// needs.
//
// track, when not nil, returns the baseline to store alongside the state
// this tick arrived at. A row that would be written back exactly as it was
// read is not written.
func (e *Evaluator) evaluateSubjectWithTitle(
	ctx context.Context, rule Rule, trigger Trigger, nodeID, subject string, current State, found bool,
	conditionTrue bool, value *float64, severityOf func(*float64) Severity, now time.Time,
	titleMessage func(Severity) (string, string), track func(StateKind) map[string]any,
) StateKind {
	next, kind, notifySeverity := computeTransition(rule, current, found, conditionTrue, value, severityOf, now)
	next.Trigger, next.NodeID, next.Subject = trigger, nodeID, subject
	if track != nil {
		next.Baseline = track(next.State)
	}

	save := next.State != StateOK || len(next.Baseline) > 0
	if found {
		save = !sameState(current, next)
	}
	if kind != "" {
		title, message := titleMessage(notifySeverity)
		n := Notification{
			// current, not next: see eventAnchor and the fix-round report's
			// item 1 for why the id must be built only from a timestamp this
			// tick did not just compute.
			EventID: eventID(trigger, nodeID, subject, eventAnchor(kind, current), kind, notifySeverity),
			Kind:    kind,
			Trigger: trigger,
			NodeID:  nodeID,
			Subject: subject,
			// notifySeverity, not next.Severity: a recovered notification
			// follows the severity of the alert it resolves (the brief's
			// own words), but the stored state's severity is already
			// cleared to "" in next by the time we get here — there is no
			// active severity for a resolved condition to report as
			// current.
			Severity: notifySeverity,
			Title:    title,
			Message:  message,
		}
		if err := e.notifier.Notify(ctx, n); err != nil {
			e.logger.ErrorContext(ctx, "could not enqueue a notification",
				slog.String("trigger", string(trigger)), slog.String("node_id", nodeID), slog.String("error", Redact(err.Error())))
			// Not saved: the next tick recomputes the same transition and
			// retries with the same deterministic event id, which Enqueue's
			// UNIQUE(event_id, channel_id) dedupes if the first attempt
			// actually got through before whatever failed here.
			return current.State
		}
		next.LastNotifiedAt, next.LastNotifiedKind = &now, kind
		save = true
	}

	if !save {
		return next.State
	}
	next.UpdatedAt = now
	if err := e.states.UpsertState(ctx, next); err != nil {
		e.logger.ErrorContext(ctx, "could not save notification state",
			slog.String("trigger", string(trigger)), slog.String("node_id", nodeID))
		return current.State
	}
	if next.State != current.State {
		e.logger.InfoContext(ctx, "notification state transition",
			slog.String("trigger", string(trigger)), slog.String("node_id", nodeID), slog.String("state", string(next.State)))
	}
	return next.State
}

// computeTransition is the state machine itself, with no I/O: given the
// current state and this tick's verdict, it decides the next state and,
// if any, which kind of notification it warrants. severityOf is called
// only while the condition is true (there is nothing to call it with
// otherwise).
//
// The third return value is the severity the notification itself should
// carry — usually just the returned State's own Severity, except for a
// recovered notification: the state's Severity is cleared to "" (nothing
// is active any more), but "recovered follows the severity of the alert
// it resolves" (the brief's own words), so the notification needs the
// severity from just before it was cleared.
func computeTransition(
	rule Rule, current State, found bool, conditionTrue bool, value *float64,
	severityOf func(*float64) Severity, now time.Time,
) (State, Kind, Severity) {
	next := current
	if !found {
		next = State{State: StateOK, StateSince: now}
	}

	if !conditionTrue {
		switch next.State {
		case StateOK:
			return next, "", next.Severity
		case StatePending:
			next.State, next.StateSince, next.ConditionSince, next.Value = StateOK, now, nil, value
			next.FlapCount, next.FlapWindowStart = 0, nil
			return next, "", next.Severity
		case StateFiring:
			next.State, next.StateSince, next.Value = StateRecovering, now, value
			return next, "", next.Severity
		case StateRecovering:
			if now.Sub(next.StateSince) < time.Duration(rule.RecoverySeconds)*time.Second {
				next.Value = value
				return next, "", next.Severity
			}
			resolvedSeverity := next.Severity
			next.State, next.StateSince, next.Value = StateOK, now, value
			next.Severity, next.FlapCount, next.FlapWindowStart = "", 0, nil
			if rule.SendRecovery {
				return next, KindRecovered, resolvedSeverity
			}
			return next, "", resolvedSeverity
		}
		return next, "", next.Severity
	}

	switch next.State {
	case StateOK:
		nowCopy := now
		next.State, next.StateSince, next.ConditionSince, next.Value = StatePending, now, &nowCopy, value
		return next, "", next.Severity

	case StatePending:
		next.Value = value
		since := next.ConditionSince
		if since == nil {
			since = &now
			next.ConditionSince = since
		}
		if now.Sub(*since) < time.Duration(rule.ForSeconds)*time.Second {
			return next, "", next.Severity
		}
		next.State, next.StateSince, next.Severity = StateFiring, now, severityOf(value)
		if withinCooldown(current.LastNotifiedAt, rule.CooldownSeconds, now) {
			// A real second incident, suppressed: silence here would mean
			// the operator never learns this fired at all until (if ever)
			// it later recovers. Send one kind flapping instead, saying so
			// — never itself gated by cooldown — and count it, so a
			// consumer of flap_count has something real to read. This
			// pending->firing transition runs exactly once per entry into
			// firing (the next tick lands in the StateFiring case below,
			// never back here), so "at most once per cycle" falls out of
			// the state machine's own shape with no extra guard needed.
			next.FlapCount++
			return next, KindFlapping, next.Severity
		}
		return next, KindFiring, next.Severity

	case StateFiring:
		next.Value = value
		sev := severityOf(value)
		if sev == next.Severity {
			return next, "", next.Severity
		}
		increased := severityRank(sev) > severityRank(next.Severity)
		next.Severity = sev
		if increased {
			return next, KindEscalated, next.Severity
		}
		return next, "", next.Severity // demotion already passed the hysteresis test in severityOf

	case StateRecovering:
		// Condition true again before recovery_seconds elapsed: back to
		// firing, no new notification — explicit in the brief. Treated as
		// a fresh firing cycle (state_since resets) since nothing tracks
		// the original firing's own duration separately; see the report.
		next.State, next.StateSince, next.Value = StateFiring, now, value
		next.Severity = severityOf(value)
		next.FlapCount++
		if next.FlapWindowStart == nil {
			next.FlapWindowStart = &now
		}
		return next, "", next.Severity
	}
	return next, "", next.Severity
}

// withinCooldown reports whether a *fresh* firing episode's own
// notification should be withheld: cooldown_seconds bounds how often a
// new activation may re-announce itself, not how long a continuously
// firing one stays quiet (that is for_seconds/recovery_seconds's job, and
// v1 sends no periodic reminder at all — see the report). A suppressed
// firing still sends a one-time kind flapping instead of nothing — see
// computeTransition's StatePending case. Escalated and recovered are
// never gated by this: an escalation is new information by definition,
// and a recovery is the one-time counterpart to a firing the operator
// already knows about (flapping included — it is still an activation the
// operator was told about).
func withinCooldown(lastNotifiedAt *time.Time, cooldownSeconds int, now time.Time) bool {
	return lastNotifiedAt != nil && now.Sub(*lastNotifiedAt) < time.Duration(cooldownSeconds)*time.Second
}

// eventAnchor is the timestamp eventID must be built from for kind: always
// a field of current, the pre-transition state, never anything this tick
// just computed into next. That is what makes the id survive a retry of
// the same transition intact — a crash between a successful Notify and
// the UpsertState that would have advanced current to next leaves current
// exactly as it was, so the next tick recomputes the identical anchor and
// Enqueue's UNIQUE(event_id, channel_id) absorbs the repeat (fix-round
// item 1; the previous version anchored on next.StateSince, which is set
// to that tick's own now on a firing transition and so changed on every
// retry, defeating the dedupe entirely).
//
//   - firing and flapping both come from the StatePending case: anchored
//     on the pending state's own ConditionSince, which does not move again
//     until the next full recovery to ok.
//   - escalated comes from the StateFiring case: anchored on that firing
//     state's StateSince, which stays fixed for the rest of the firing
//     cycle — kind's severity is folded into eventID too, since more than
//     one escalation (critical, demoted, critical again) can share that
//     same anchor within one firing cycle.
//   - recovered comes from the StateRecovering case: anchored on that
//     recovering state's own StateSince.
func eventAnchor(kind Kind, current State) time.Time {
	switch kind {
	case KindFiring, KindFlapping:
		if current.ConditionSince != nil {
			return *current.ConditionSince
		}
	case KindEscalated, KindRecovered:
		return current.StateSince
	}
	return current.StateSince
}

// eventID is deterministic per transition: the same (trigger, node,
// subject, anchor, kind, severity) always produces the same id — see
// eventAnchor for what anchor must be to make that survive a retry.
func eventID(trigger Trigger, nodeID, subject string, anchor time.Time, kind Kind, severity Severity) string {
	return fmt.Sprintf("notif-%s-%s-%s-%d-%s-%s", trigger, nodeID, subject, anchor.Unix(), kind, severity)
}
