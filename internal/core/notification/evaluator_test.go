package notification

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- computeTransition: the pure state machine, no I/O -----------------

// percentRule builds a disk/ram-shaped rule for the pure transition
// tests: warn 80, crit 90, hysteresis 5, for_seconds 300, recovery_seconds
// 120, cooldown 0 (not under test here — see TestComputeTransitionCooldown),
// send_recovery true.
func percentRule() Rule {
	warn, crit := 80.0, 90.0
	return Rule{
		Trigger: TriggerDiskPct, Enabled: true, WarnThreshold: &warn, CritThreshold: &crit,
		ForSeconds: 300, RecoverySeconds: 120, Hysteresis: 5, CooldownSeconds: 0, SendRecovery: true,
	}
}

func severityForPercentRule(rule Rule, current Severity) func(*float64) Severity {
	return func(v *float64) Severity { return thresholdSeverity(*v, rule, current) }
}

func TestComputeTransitionHold(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule()

	// ok -> pending the instant the condition is true.
	next, kind, _ := computeTransition(rule, State{}, false, true, ptr(85.0), severityForPercentRule(rule, ""), t0)
	if next.State != StatePending || kind != "" {
		t.Fatalf("ok->pending: state=%v kind=%v", next.State, kind)
	}
	if next.ConditionSince == nil || !next.ConditionSince.Equal(t0) {
		t.Fatalf("ConditionSince = %v, want %v", next.ConditionSince, t0)
	}

	// pending, short of for_seconds -> stays pending, no notification.
	t1 := t0.Add(299 * time.Second)
	next, kind, _ = computeTransition(rule, next, true, true, ptr(85.0), severityForPercentRule(rule, ""), t1)
	if next.State != StatePending || kind != "" {
		t.Fatalf("pending (short of hold): state=%v kind=%v", next.State, kind)
	}

	// pending, for_seconds elapsed -> firing, with a firing notification.
	t2 := t0.Add(300 * time.Second)
	next, kind, _ = computeTransition(rule, next, true, true, ptr(85.0), severityForPercentRule(rule, ""), t2)
	if next.State != StateFiring || kind != KindFiring {
		t.Fatalf("pending->firing: state=%v kind=%v, want firing/KindFiring", next.State, kind)
	}
	if next.Severity != SeverityWarning {
		t.Errorf("severity = %v, want warning (85 is below crit 90)", next.Severity)
	}
}

func TestComputeTransitionPendingAbortsBackToOK(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule()
	pending := State{State: StatePending, StateSince: t0, ConditionSince: &t0}

	next, kind, _ := computeTransition(rule, pending, true, false, ptr(50.0), severityForPercentRule(rule, ""), t0.Add(10*time.Second))
	if next.State != StateOK || kind != "" {
		t.Fatalf("pending->ok (condition cleared before hold): state=%v kind=%v", next.State, kind)
	}
}

func TestComputeTransitionEscalation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule()
	firing := State{State: StateFiring, StateSince: t0, Severity: SeverityWarning}

	next, kind, _ := computeTransition(rule, firing, true, true, ptr(95.0), severityForPercentRule(rule, SeverityWarning), t0.Add(time.Minute))
	if next.State != StateFiring || kind != KindEscalated {
		t.Fatalf("warning->critical: state=%v kind=%v, want firing/KindEscalated", next.State, kind)
	}
	if next.Severity != SeverityCritical {
		t.Errorf("severity = %v, want critical", next.Severity)
	}
}

func TestComputeTransitionSeverityHysteresis(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule() // crit=90, hysteresis=5
	firing := State{State: StateFiring, StateSince: t0, Severity: SeverityCritical}

	// 87 is below crit (90) but not below crit-hysteresis (85): must stay
	// critical, no notification — the demotion margin from the brief.
	next, kind, _ := computeTransition(rule, firing, true, true, ptr(87.0), severityForPercentRule(rule, SeverityCritical), t0.Add(time.Minute))
	if next.State != StateFiring || next.Severity != SeverityCritical || kind != "" {
		t.Fatalf("87%% (within the hysteresis margin): state=%v severity=%v kind=%v, want firing/critical/none",
			next.State, next.Severity, kind)
	}

	// 84 is below crit-hysteresis (85): demotes to warning, still no
	// notification (a decrease never sends one).
	next, kind, _ = computeTransition(rule, firing, true, true, ptr(84.0), severityForPercentRule(rule, SeverityCritical), t0.Add(time.Minute))
	if next.State != StateFiring || next.Severity != SeverityWarning || kind != "" {
		t.Fatalf("84%% (past the hysteresis margin): state=%v severity=%v kind=%v, want firing/warning/none",
			next.State, next.Severity, kind)
	}
}

func TestComputeTransitionRecoveryHysteresis(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule() // warn=80, hysteresis=5
	firing := State{State: StateFiring, StateSince: t0, Severity: SeverityWarning}

	// 76 is below warn (80) but not below warn-hysteresis (75): the
	// condition must still count as true (already active), so this stays
	// firing, not recovering.
	alreadyActive := true
	conditionTrue := 76.0 >= 80.0-rule.Hysteresis
	if !alreadyActive || conditionTrue != true {
		t.Fatalf("precondition: 76 should still read as true once already active (>= %v)", 80.0-rule.Hysteresis)
	}
	next, kind, _ := computeTransition(rule, firing, true, conditionTrue, ptr(76.0), severityForPercentRule(rule, SeverityWarning), t0.Add(time.Minute))
	if next.State != StateFiring || kind != "" {
		t.Fatalf("76%% (within recovery hysteresis): state=%v kind=%v, want still firing", next.State, kind)
	}

	// 74 is below warn-hysteresis (75): now genuinely false, moves to
	// recovering.
	conditionTrue = 74.0 >= 80.0-rule.Hysteresis
	next, kind, _ = computeTransition(rule, firing, true, conditionTrue, ptr(74.0), severityForPercentRule(rule, SeverityWarning), t0.Add(time.Minute))
	if next.State != StateRecovering || kind != "" {
		t.Fatalf("74%% (past recovery hysteresis): state=%v kind=%v, want recovering", next.State, kind)
	}
}

func TestComputeTransitionRecovery(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule() // recovery_seconds=120, send_recovery=true
	recovering := State{State: StateRecovering, StateSince: t0, Severity: SeverityWarning}

	// Short of recovery_seconds: stays recovering, no notification.
	next, kind, _ := computeTransition(rule, recovering, true, false, ptr(10.0), severityForPercentRule(rule, SeverityWarning), t0.Add(119*time.Second))
	if next.State != StateRecovering || kind != "" {
		t.Fatalf("recovering (short of hold): state=%v kind=%v", next.State, kind)
	}

	// recovery_seconds elapsed: ok, with a recovered notification.
	next, kind, notifySev := computeTransition(rule, recovering, true, false, ptr(10.0), severityForPercentRule(rule, SeverityWarning), t0.Add(120*time.Second))
	if next.State != StateOK || kind != KindRecovered {
		t.Fatalf("recovering->ok: state=%v kind=%v, want ok/KindRecovered", next.State, kind)
	}
	if next.Severity != "" {
		t.Errorf("severity = %q, want cleared on recovery", next.Severity)
	}
	if notifySev != SeverityWarning {
		t.Errorf("notify severity = %q, want warning (the severity of the alert being resolved, not the cleared state)", notifySev)
	}
}

func TestComputeTransitionRecoveryWithoutSendRecovery(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule()
	rule.SendRecovery = false
	recovering := State{State: StateRecovering, StateSince: t0, Severity: SeverityWarning}

	next, kind, _ := computeTransition(rule, recovering, true, false, ptr(10.0), severityForPercentRule(rule, SeverityWarning), t0.Add(120*time.Second))
	if next.State != StateOK || kind != "" {
		t.Fatalf("recovering->ok, send_recovery=false: state=%v kind=%v, want ok/none", next.State, kind)
	}
}

// TestComputeTransitionRecoveryInterrupted is the brief's "recovery
// interrupted" case: the condition becomes true again while recovering,
// before recovery_seconds elapses, and must return to firing with no new
// notification.
func TestComputeTransitionRecoveryInterrupted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule()
	recovering := State{State: StateRecovering, StateSince: t0, Severity: SeverityWarning}

	next, kind, _ := computeTransition(rule, recovering, true, true, ptr(85.0), severityForPercentRule(rule, SeverityWarning), t0.Add(30*time.Second))
	if next.State != StateFiring || kind != "" {
		t.Fatalf("recovering->firing (interrupted): state=%v kind=%v, want firing/none", next.State, kind)
	}
	if next.FlapCount != 1 || next.FlapWindowStart == nil {
		t.Errorf("flap_count = %d, flap_window_start = %v, want 1 and set", next.FlapCount, next.FlapWindowStart)
	}
}

// TestComputeTransitionCooldown proves cooldown_seconds withholds a fresh
// firing's own "firing" notification (the state still transitions) when
// the last notification was too recent, sending "flapping" in its place
// (fix-round item 2) rather than nothing — and never withholds an
// escalation.
func TestComputeTransitionCooldown(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule()
	rule.CooldownSeconds = 600

	lastNotified := t0.Add(-100 * time.Second) // 100s ago, within the 600s cooldown
	pending := State{
		State: StatePending, StateSince: t0.Add(-300 * time.Second), ConditionSince: ptr(t0.Add(-300 * time.Second)),
		LastNotifiedAt: &lastNotified,
	}
	next, kind, sev := computeTransition(rule, pending, true, true, ptr(85.0), severityForPercentRule(rule, ""), t0)
	if next.State != StateFiring {
		t.Fatalf("state = %v, want firing even though the firing notification is suppressed", next.State)
	}
	if kind != KindFlapping {
		t.Errorf("kind = %v, want KindFlapping: a real second incident must never go completely silent", kind)
	}
	if sev != SeverityWarning {
		t.Errorf("flapping severity = %q, want warning (85 is below crit 90)", sev)
	}
	if next.FlapCount != 1 {
		t.Errorf("flap_count = %d, want 1 after the flapping notification", next.FlapCount)
	}

	// The very next tick, still firing at the same severity: nothing more
	// is sent — one flapping per cycle, not a repeat every tick.
	next, kind, _ = computeTransition(rule, next, true, true, ptr(85.0), severityForPercentRule(rule, SeverityWarning), t0.Add(time.Minute))
	if kind != "" {
		t.Errorf("kind = %v on the following tick, want none: flapping already said this cycle re-fired", kind)
	}

	// Escalation is never gated by cooldown.
	firing := State{State: StateFiring, StateSince: t0, Severity: SeverityWarning, LastNotifiedAt: &lastNotified}
	next, kind, _ = computeTransition(rule, firing, true, true, ptr(95.0), severityForPercentRule(rule, SeverityWarning), t0)
	if kind != KindEscalated {
		t.Errorf("kind = %v, want KindEscalated regardless of cooldown", kind)
	}
}

// --- Evaluator-level behaviour, with fakes and a fake clock -------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }
func (c *fakeClock) now() time.Time       { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeRuleStore struct {
	mu    sync.Mutex
	rules map[Trigger]Rule
}

func (f *fakeRuleStore) EffectiveRule(_ context.Context, trigger Trigger, nodeID string) (Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rules[trigger]
	if !ok {
		return Rule{}, errors.New("no rule for trigger")
	}
	r.NodeID = nodeID
	return r, nil
}

type stateKey struct{ trigger, nodeID, subject string }

type fakeStateStore struct {
	mu      sync.Mutex
	states  map[stateKey]State
	upserts int
	// nodes lets EndMuteAfterExpiry perform its guarded clear against the
	// same node record the harness's NodeStore serves — wired by
	// newHarness, mirroring how the real Repository.EndMuteAfterExpiry
	// reaches across to the nodes table in the same transaction as its
	// own notification_state reset.
	nodes *fakeEvalNodeStore
	// endMuteOverride, when set, replaces EndMuteAfterExpiry's normal
	// guarded-clear-then-reset behaviour outright — a test's way to force
	// exactly the "did=false" outcome (a concurrent re-mute, or a write
	// failure) without needing a real race.
	endMuteOverride func(nodeID string, now time.Time) (bool, error)
}

func newFakeStateStore() *fakeStateStore { return &fakeStateStore{states: map[stateKey]State{}} }

func (f *fakeStateStore) GetState(_ context.Context, trigger Trigger, nodeID, subject string) (State, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.states[stateKey{string(trigger), nodeID, subject}]
	return s, ok, nil
}

func (f *fakeStateStore) UpsertState(_ context.Context, s State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	f.states[stateKey{string(s.Trigger), s.NodeID, s.Subject}] = s
	return nil
}

func (f *fakeStateStore) ListNodeStates(_ context.Context, nodeID string, triggers ...Trigger) ([]State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []State{}
	for k, s := range f.states {
		if k.nodeID == nodeID && slices.Contains(triggers, Trigger(k.trigger)) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStateStore) BeginPending(_ context.Context, s State) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := stateKey{string(s.Trigger), s.NodeID, s.Subject}
	if stored, ok := f.states[k]; ok && stored.State != StateOK {
		return stored, nil
	}
	f.upserts++
	f.states[k] = s
	return s, nil
}

func (f *fakeStateStore) DeleteState(_ context.Context, trigger Trigger, nodeID, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.states, stateKey{string(trigger), nodeID, subject})
	return nil
}

// ResetNodeStates mirrors the real repository's single-statement reset:
// every non-ok row for nodeID, across every trigger and subject, becomes
// ok with its severity and counters cleared, without touching any other
// node's rows.
func (f *fakeStateStore) ResetNodeStates(_ context.Context, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, s := range f.states {
		if k.nodeID != nodeID {
			continue
		}
		if k.trigger == string(TriggerContainerRestart) && s.Baseline != nil {
			s.Baseline = maps.Clone(s.Baseline)
			delete(s.Baseline, baselineLastRestarts)
			f.states[k] = s
		}
		if s.State == StateOK {
			continue
		}
		s.State, s.Severity, s.Value = StateOK, "", nil
		s.ConditionSince, s.FlapCount, s.FlapWindowStart = nil, 0, nil
		f.states[k] = s
	}
	return nil
}

func (f *fakeStateStore) all() map[stateKey]State {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[stateKey]State, len(f.states))
	for k, v := range f.states {
		out[k] = v
	}
	return out
}

// EndMuteAfterExpiry mirrors the real Repository.EndMuteAfterExpiry: it
// clears the node's mute only if, right now, it is still the one that
// was found expired (nodes.clearMuteIfExpired carries the same guard the
// real UPDATE's WHERE clause does), and resets state only if that clear
// actually happened — see the fix round's own report for why these two
// must be tied together rather than the bare reset the first fix round
// shipped.
func (f *fakeStateStore) EndMuteAfterExpiry(ctx context.Context, nodeID string, now time.Time) (bool, error) {
	if f.endMuteOverride != nil {
		return f.endMuteOverride(nodeID, now)
	}
	if f.nodes == nil || !f.nodes.clearMuteIfExpired(nodeID, now) {
		return false, nil
	}
	if err := f.ResetNodeStates(ctx, nodeID); err != nil {
		return false, err
	}
	return true, nil
}

// fakeEvalNodeStore is keyed and mutable (unlike a plain slice) so a
// test can simulate the column actually clearing — and a concurrent
// re-mute racing ahead of that clear — the same way the real nodes table
// would.
type fakeEvalNodeStore struct {
	mu    sync.Mutex
	nodes map[string]*EvalNode
}

func newFakeEvalNodeStore(nodes []EvalNode) *fakeEvalNodeStore {
	m := make(map[string]*EvalNode, len(nodes))
	for i := range nodes {
		n := nodes[i]
		m[n.NodeID] = &n
	}
	return &fakeEvalNodeStore{nodes: m}
}

func (f *fakeEvalNodeStore) ListNodes(context.Context) ([]EvalNode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EvalNode, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, *n)
	}
	return out, nil
}

// clearMuteIfExpired is the fake's counterpart to the real repository's
// guarded UPDATE: it clears AlertsMutedUntil to nil only if the node
// exists and, right now, its mute is nil or not after now — exactly the
// condition that must hold for the clear to be the one the caller is
// still trying to end, not a newer one a concurrent re-mute set first.
func (f *fakeEvalNodeStore) clearMuteIfExpired(nodeID string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[nodeID]
	if !ok {
		return false
	}
	if n.AlertsMutedUntil != nil && n.AlertsMutedUntil.After(now) {
		return false
	}
	n.AlertsMutedUntil = nil
	return true
}

// setMuted lets a test simulate a concurrent re-mute landing between the
// evaluator's own read of a node (via ListNodes) and its end-mute call.
func (f *fakeEvalNodeStore) setMuted(nodeID string, until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.nodes[nodeID]; ok {
		n.AlertsMutedUntil = &until
	}
}

// alertsMutedUntil reports the node's current column value, for a test
// to assert against directly rather than only through its side effects.
func (f *fakeEvalNodeStore) alertsMutedUntil(nodeID string) *time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[nodeID]
	if !ok || n.AlertsMutedUntil == nil {
		return nil
	}
	v := *n.AlertsMutedUntil
	return &v
}

type fakeMetricSource struct {
	mu      sync.Mutex
	samples map[string][]MetricSample
	errs    map[string]error
	now     func() time.Time
}

func (f *fakeMetricSource) LatestForMetric(_ context.Context, metric string, within time.Duration) ([]MetricSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[metric]; err != nil {
		return nil, err
	}
	now := f.now()
	out := []MetricSample{}
	for _, s := range f.samples[metric] {
		if now.Sub(s.Time) <= within {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeMetricSource) set(metric string, samples []MetricSample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples[metric] = samples
}

type fakeNotifier struct {
	mu   sync.Mutex
	sent []Notification
	err  error
}

func (f *fakeNotifier) Notify(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, n)
	return nil
}

func (f *fakeNotifier) all() []Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Notification{}, f.sent...)
}

// defaultRules builds a rule per trigger, matching migration 0024's seeded defaults: disk 80/90/5, ram 85/95/5, node_down
// 480s hold (NodeDownAfter, 120s, is the condition itself, separate from
// this hold).
func defaultRules() map[Trigger]Rule {
	diskWarn, diskCrit := 80.0, 90.0
	ramWarn, ramCrit := 85.0, 95.0
	return map[Trigger]Rule{
		TriggerDiskPct: {Trigger: TriggerDiskPct, Enabled: true, WarnThreshold: &diskWarn, CritThreshold: &diskCrit,
			ForSeconds: 300, RecoverySeconds: 120, Hysteresis: 5, SendRecovery: true},
		TriggerRAMPct: {Trigger: TriggerRAMPct, Enabled: true, WarnThreshold: &ramWarn, CritThreshold: &ramCrit,
			ForSeconds: 300, RecoverySeconds: 120, Hysteresis: 5, SendRecovery: true},
		TriggerNodeDown: {Trigger: TriggerNodeDown, Enabled: true,
			ForSeconds: 480, RecoverySeconds: 120, SendRecovery: true},
		TriggerContainerExit: {Trigger: TriggerContainerExit, Enabled: true,
			RecoverySeconds: 120, CooldownSeconds: 1800, SendRecovery: true},
		TriggerContainerRestart: {Trigger: TriggerContainerRestart, Enabled: true,
			RecoverySeconds: 120, CooldownSeconds: 1800, SendRecovery: true},
		TriggerServiceFailed: {Trigger: TriggerServiceFailed, Enabled: true,
			ForSeconds: 300, RecoverySeconds: 120, CooldownSeconds: 1800, SendRecovery: true},
	}
}

type evalHarness struct {
	clock    *fakeClock
	rules    *fakeRuleStore
	states   *fakeStateStore
	nodes    *fakeEvalNodeStore
	metrics  *fakeMetricSource
	notifier *fakeNotifier
	eval     *Evaluator
}

func newHarness(t *testing.T, nodes []EvalNode, localNodeID string) *evalHarness {
	t.Helper()
	h := &evalHarness{
		clock:    newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		rules:    &fakeRuleStore{rules: defaultRules()},
		states:   newFakeStateStore(),
		nodes:    newFakeEvalNodeStore(nodes),
		notifier: &fakeNotifier{},
	}
	h.states.nodes = h.nodes
	h.metrics = &fakeMetricSource{samples: map[string][]MetricSample{}, now: h.clock.now}
	h.eval = NewEvaluator(EvaluatorOptions{
		Rules: h.rules, States: h.states, Nodes: h.nodes, Metrics: h.metrics, Notifier: h.notifier,
		Now: h.clock.now, LocalNodeID: localNodeID,
		ContainerCollectorInterval: 30 * time.Second, ServiceCollectorInterval: 60 * time.Second,
	})
	return h
}

func diskSample(nodeID, mountpoint string, value float64, at time.Time) MetricSample {
	return MetricSample{NodeID: nodeID, Labels: map[string]string{diskMountpointLabel: mountpoint}, Value: value, Time: at}
}

func ramSample(nodeID string, value float64, at time.Time) MetricSample {
	return MetricSample{NodeID: nodeID, Value: value, Time: at}
}

// TestEvaluatorStaleDataNeverTransitions covers item 1: a sample old
// enough to fall outside the staleness window simply does not reach the
// evaluator (fakeMetricSource enforces the same within-window filtering
// LatestForMetric's real SQL does), so an existing pending state must
// stay exactly as it was.
func TestEvaluatorStaleDataNeverTransitions(t *testing.T) {
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}, "")
	conditionSince := h.clock.now().Add(-250 * time.Second)
	h.states.UpsertState(context.Background(), State{
		Trigger: TriggerDiskPct, NodeID: "n1", Subject: "/data", State: StatePending,
		StateSince: conditionSince, ConditionSince: &conditionSince,
	})

	// Stale: 181s old against a 180s window (3 * 60s disk interval).
	stale := diskSample("n1", "/data", 95, h.clock.now().Add(-181*time.Second))
	h.metrics.set(MetricDiskUsage, []MetricSample{stale})

	h.eval.EvaluateOnce(context.Background())

	got, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/data")
	if !found || got.State != StatePending || !got.StateSince.Equal(conditionSince) {
		t.Fatalf("state = %+v, want untouched pending from before the stale read", got)
	}
	if len(h.notifier.all()) != 0 {
		t.Errorf("sent %d notifications, want 0 from stale data", len(h.notifier.all()))
	}
}

// TestEvaluatorNodeDownSuppressesDiskAndRAM covers item 2.
func TestEvaluatorNodeDownSuppressesDiskAndRAM(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: start}}, "")

	// First tick: the condition (unseen > 120s) becomes true, so node_down
	// enters pending — this is also the tick that sets ConditionSince,
	// which the hold below counts from.
	h.clock.advance(120*time.Second + time.Second)
	h.eval.EvaluateOnce(context.Background())
	if s, _, _ := h.states.GetState(context.Background(), TriggerNodeDown, "n1", ""); s.State != StatePending {
		t.Fatalf("after the condition first became true, node_down = %v, want pending", s.State)
	}

	// Second tick, for_seconds (480s) later: node_down reaches firing.
	h.clock.advance(480 * time.Second)
	hot := diskSample("n1", "/data", 99, h.clock.now())
	h.metrics.set(MetricDiskUsage, []MetricSample{hot})
	h.eval.EvaluateOnce(context.Background())

	downState, found, _ := h.states.GetState(context.Background(), TriggerNodeDown, "n1", "")
	if !found || downState.State != StateFiring {
		t.Fatalf("node_down state = %+v, want firing", downState)
	}
	if _, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/data"); found {
		t.Error("disk_pct state exists while node_down is firing; it must be suppressed entirely")
	}

	for _, n := range h.notifier.all() {
		if n.Trigger == TriggerDiskPct {
			t.Error("a disk_pct notification was sent while node_down is firing")
		}
	}
}

// TestEvaluatorSkipsLocalArchivedAndMutedNodes covers item 3's three
// full-skip cases.
func TestEvaluatorSkipsLocalArchivedAndMutedNodes(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	archivedAt := now.Add(-time.Hour)

	h := newHarness(t, []EvalNode{
		{NodeID: "local", LastSeenAt: now},
		{NodeID: "archived", LastSeenAt: now, ArchivedAt: &archivedAt},
		{NodeID: "muted", LastSeenAt: now, AlertsMutedUntil: &future},
	}, "local")

	hot := []MetricSample{
		diskSample("local", "/data", 99, h.clock.now()),
		diskSample("archived", "/data", 99, h.clock.now()),
		diskSample("muted", "/data", 99, h.clock.now()),
	}
	h.metrics.set(MetricDiskUsage, hot)
	h.eval.EvaluateOnce(context.Background())

	for _, nodeID := range []string{"local", "archived", "muted"} {
		if _, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, nodeID, "/data"); found {
			t.Errorf("node %q has notification state; it should have been skipped entirely", nodeID)
		}
	}
	if len(h.notifier.all()) != 0 {
		t.Errorf("sent %d notifications, want 0 from fully-skipped nodes", len(h.notifier.all()))
	}
}

// TestEvaluatorMuteEndedResetsStateSilently covers item 3's reset: a node
// frozen non-ok by a mute that has since expired is reset to ok with no
// notification, and resumes normal evaluation the same tick.
func TestEvaluatorMuteEndedResetsStateSilently(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	muteEnded := now.Add(-time.Hour)
	frozenAt := muteEnded.Add(-time.Hour) // updated well before the mute ended

	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now, AlertsMutedUntil: &muteEnded}}, "")
	h.states.UpsertState(context.Background(), State{
		Trigger: TriggerNodeDown, NodeID: "n1", State: StateFiring, Severity: SeverityCritical,
		StateSince: frozenAt, UpdatedAt: frozenAt,
	})

	h.eval.EvaluateOnce(context.Background())

	got, found, _ := h.states.GetState(context.Background(), TriggerNodeDown, "n1", "")
	if !found || got.State != StateOK {
		t.Fatalf("node_down state after the mute ended = %+v, want silently reset to ok", got)
	}
	for _, n := range h.notifier.all() {
		if n.Trigger == TriggerNodeDown && n.NodeID == "n1" {
			t.Errorf("a notification was sent for the mute-ended reset: %+v", n)
		}
	}
}

// TestEvaluatorMuteEndedResetsEveryDiskSubject covers fix-round item 3's
// bug fix directly: the previous version's reset only ever looked at
// subject "", so a disk_pct row frozen under a mountpoint subject
// survived a mute and could still send a late, spurious recovered
// message once that mountpoint's real data resumed. ResetNodeStates
// covers every subject in one statement, so both mountpoints below must
// come back ok, silently, in the same tick the mute ends.
func TestEvaluatorMuteEndedResetsEveryDiskSubject(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	muteEnded := now.Add(-time.Hour)
	frozenAt := muteEnded.Add(-time.Hour)

	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now, AlertsMutedUntil: &muteEnded}}, "")
	for _, subject := range []string{"/", "/data"} {
		h.states.UpsertState(context.Background(), State{
			Trigger: TriggerDiskPct, NodeID: "n1", Subject: subject, State: StateFiring, Severity: SeverityWarning,
			StateSince: frozenAt, UpdatedAt: frozenAt,
		})
	}

	h.eval.EvaluateOnce(context.Background())

	for _, subject := range []string{"/", "/data"} {
		got, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", subject)
		if !found || got.State != StateOK {
			t.Errorf("disk_pct[%q] after the mute ended = %+v, want silently reset to ok", subject, got)
		}
	}
	if len(h.notifier.all()) != 0 {
		t.Errorf("sent %d notifications for the mute-ended reset, want 0", len(h.notifier.all()))
	}
}

// TestEvaluatorMuteEndedResetHappensExactlyOnceAcrossMultipleTicks is the
// second fix round's own regression test for its blocker: a bare,
// unguarded reset run on every tick (what the first fix round shipped)
// still "passes" a single-tick test like the two above, since one tick
// never reveals that the column never actually got cleared. Running
// several ticks here, all with the condition false, proves the reset
// really happens exactly once — the column reads nil afterwards, and the
// row is never flipped to recovering/recovered along the way, since it
// was never firing for real after the first tick reset it.
func TestEvaluatorMuteEndedResetHappensExactlyOnceAcrossMultipleTicks(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	muteEnded := now.Add(-time.Hour)
	frozenAt := muteEnded.Add(-time.Hour)

	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now, AlertsMutedUntil: &muteEnded}}, "")
	h.states.UpsertState(context.Background(), State{
		Trigger: TriggerDiskPct, NodeID: "n1", Subject: "/data", State: StateFiring, Severity: SeverityWarning,
		StateSince: frozenAt, UpdatedAt: frozenAt,
	})

	for i := 0; i < 5; i++ {
		h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 10, h.clock.now())})
		h.eval.EvaluateOnce(context.Background())
		h.clock.advance(30 * time.Second)
	}

	got, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/data")
	if !found || got.State != StateOK {
		t.Fatalf("disk_pct state after 5 ticks past expiry = %+v (found=%v), want ok", got, found)
	}
	if sent := h.notifier.all(); len(sent) != 0 {
		t.Errorf("sent = %+v, want nothing: a mute ending is never a real recovery", sent)
	}
	if until := h.nodes.alertsMutedUntil("n1"); until != nil {
		t.Errorf("alerts_muted_until = %v, want nil after the mute ended", until)
	}
}

// TestEvaluatorNodeCanAlertAgainAfterAMuteEnds is the blocker itself,
// reproduced directly: with the first fix round's bare, unconditional
// reset, alerts_muted_until never cleared, so resetFrozenByMute ran on
// every single tick for as long as it stayed in the past — forever. A
// disk_pct condition that went true right after the mute ended would get
// wiped back to ok before its for_seconds hold could complete, reset to
// pending with a brand new ConditionSince on the very next tick, wiped
// again, and so on without end: the node could never alert again after
// being muted once. This test fails against that code (disk_pct never
// reaches firing) and must pass against the fix (EndMuteAfterExpiry
// clears the column in the same write as its reset, so this branch is
// never reached a second time for this mute).
func TestEvaluatorNodeCanAlertAgainAfterAMuteEnds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	muteEnded := now.Add(-time.Second) // already expired as of the very first tick
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now, AlertsMutedUntil: &muteEnded}}, "")

	// Four ticks, 100s apart: the mute ends on the first one, then a
	// disk_pct condition holds true across the rest of its 300s
	// for_seconds.
	for i := 0; i < 4; i++ {
		h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, h.clock.now())})
		h.eval.EvaluateOnce(context.Background())
		if i < 3 {
			h.clock.advance(100 * time.Second)
		}
	}

	got, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/data")
	if !found || got.State != StateFiring {
		t.Fatalf("disk_pct state after a full hold past the mute's expiry = %+v (found=%v), want firing", got, found)
	}
	sent := h.notifier.all()
	if len(sent) != 1 || sent[0].Kind != KindFiring {
		t.Fatalf("sent = %+v, want exactly one KindFiring", sent)
	}
	if until := h.nodes.alertsMutedUntil("n1"); until != nil {
		t.Errorf("alerts_muted_until = %v, want nil after the mute ended", until)
	}
}

// TestEvaluatorConditionTrueNotResetBetweenConsecutiveTicksPastExpiry
// isolates the same guarantee down to its smallest case: once the mute
// has ended, a condition's own ConditionSince must survive untouched
// from one tick to the very next, not just across the four ticks of the
// fuller test above.
func TestEvaluatorConditionTrueNotResetBetweenConsecutiveTicksPastExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	muteEnded := now.Add(-time.Second)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now, AlertsMutedUntil: &muteEnded}}, "")

	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, h.clock.now())})
	h.eval.EvaluateOnce(context.Background()) // mute ends; disk goes ok -> pending

	first, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/data")
	if !found || first.State != StatePending || first.ConditionSince == nil {
		t.Fatalf("after the first tick past expiry, disk_pct = %+v (found=%v), want pending with ConditionSince set", first, found)
	}

	h.clock.advance(50 * time.Second)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, h.clock.now())})
	h.eval.EvaluateOnce(context.Background())

	second, found, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/data")
	if !found || second.State != StatePending {
		t.Fatalf("after the second consecutive tick, disk_pct = %+v (found=%v), want still pending", second, found)
	}
	if !second.ConditionSince.Equal(*first.ConditionSince) {
		t.Errorf("ConditionSince moved from %v to %v across two consecutive ticks — the mute-ended reset must not run again",
			*first.ConditionSince, *second.ConditionSince)
	}
}

// TestEvaluatorConcurrentRemuteLeavesNodeMutedAndStatesUntouched covers
// the required-safe outcome of EndMuteAfterExpiry's own guard losing a
// race: a concurrent re-mute's later alerts_muted_until won in the gap
// between the evaluator's read and this call, so the guarded clear
// matches nothing, nothing is reset, and the node must be treated as
// still muted for this tick — no evaluation, no notification.
func TestEvaluatorConcurrentRemuteLeavesNodeMutedAndStatesUntouched(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	muteEnded := now.Add(-time.Second)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now, AlertsMutedUntil: &muteEnded}}, "")
	frozenAt := now.Add(-time.Hour)
	h.states.UpsertState(context.Background(), State{
		Trigger: TriggerNodeDown, NodeID: "n1", State: StateFiring, Severity: SeverityCritical,
		StateSince: frozenAt, UpdatedAt: frozenAt,
	})

	h.states.endMuteOverride = func(string, time.Time) (bool, error) { return false, nil }

	h.eval.EvaluateOnce(context.Background())

	got, found, _ := h.states.GetState(context.Background(), TriggerNodeDown, "n1", "")
	if !found || got.State != StateFiring {
		t.Fatalf("node_down state = %+v (found=%v), want untouched firing: a lost race must reset nothing", got, found)
	}
	if sent := h.notifier.all(); len(sent) != 0 {
		t.Errorf("sent = %+v, want nothing: the node must be treated as still muted this tick", sent)
	}
}

// TestResetNodeStatesSupportsAnEarlyUnmute covers fix-round item 3's other
// half directly: the evaluator cannot detect an early, explicit unmute on
// its own (once alerts_muted_until reads NULL there is no record of when
// it changed), so that case is "supported" by exposing ResetNodeStates
// for the future unmute API to call itself — this proves the primitive
// those callers would use actually clears a frozen state silently, the
// same as the natural-expiry path above.
func TestResetNodeStatesSupportsAnEarlyUnmute(t *testing.T) {
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: time.Now()}}, "")
	frozenAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.states.UpsertState(context.Background(), State{
		Trigger: TriggerNodeDown, NodeID: "n1", State: StateFiring, Severity: SeverityCritical, StateSince: frozenAt,
	})
	// A second node's state must never be touched by the first node's reset.
	h.states.UpsertState(context.Background(), State{
		Trigger: TriggerNodeDown, NodeID: "n2", State: StateFiring, Severity: SeverityCritical, StateSince: frozenAt,
	})

	if err := h.states.ResetNodeStates(context.Background(), "n1"); err != nil {
		t.Fatalf("ResetNodeStates: %v", err)
	}

	got, found, _ := h.states.GetState(context.Background(), TriggerNodeDown, "n1", "")
	if !found || got.State != StateOK || got.Severity != "" {
		t.Errorf("n1 after an early unmute's reset = %+v, want ok with severity cleared", got)
	}
	other, _, _ := h.states.GetState(context.Background(), TriggerNodeDown, "n2", "")
	if other.State != StateFiring {
		t.Errorf("n2 state = %v, want untouched firing — ResetNodeStates must be scoped to its own node", other.State)
	}
	if len(h.notifier.all()) != 0 {
		t.Errorf("sent %d notifications from ResetNodeStates, want 0: a mute ending is never itself a notification", len(h.notifier.all()))
	}
}

// --- Cooldown-suppressed firing sends flapping, not silence (item 2) ---

// TestEvaluatorCooldownSuppressedFiringSendsFlappingThenRecovered drives a
// full cycle: fire normally, recover, re-fire again quickly enough to
// land inside cooldown_seconds of that recovery and have the firing
// itself suppressed, then fully recover again. Exactly one flapping must
// stand in for the suppressed firing, and the eventual recovered message
// still goes out as always.
func TestEvaluatorCooldownSuppressedFiringSendsFlappingThenRecovered(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now}}, "")
	rule := h.rules.rules[TriggerDiskPct]
	rule.CooldownSeconds = 1800
	h.rules.rules[TriggerDiskPct] = rule
	ctx := context.Background()

	// Seed straight past the hold, the same shortcut
	// TestEvaluatorRestartMidPending uses, since this test is about
	// cooldown and flapping, not re-deriving the for_seconds wait with a
	// real clock (which would also run the sample stale across a 300s
	// jump).
	seedPastHold := func(lastNotifiedAt *time.Time) {
		since := h.clock.now().Add(-300 * time.Second)
		h.states.UpsertState(ctx, State{
			Trigger: TriggerDiskPct, NodeID: "n1", Subject: "/data", State: StatePending,
			StateSince: since, ConditionSince: &since, LastNotifiedAt: lastNotifiedAt,
		})
	}

	seedPastHold(nil)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, h.clock.now())})
	h.eval.EvaluateOnce(ctx) // pending -> firing (first ever activation, sends firing)

	if s, _, _ := h.states.GetState(ctx, TriggerDiskPct, "n1", "/data"); s.State != StateFiring {
		t.Fatalf("state after the hold = %v, want firing", s.State)
	}

	// -> recovering -> ok (recovered): the condition clears and
	// recovery_seconds (120s) elapses.
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 10, h.clock.now())})
	h.eval.EvaluateOnce(ctx) // firing -> recovering
	h.clock.advance(120 * time.Second)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 10, h.clock.now())})
	h.eval.EvaluateOnce(ctx) // recovering -> ok, sends recovered

	firstRound := h.notifier.all()
	if len(firstRound) != 2 || firstRound[0].Kind != KindFiring || firstRound[1].Kind != KindRecovered {
		t.Fatalf("first round sent = %+v, want exactly [firing recovered]", firstRound)
	}
	okState, _, _ := h.states.GetState(ctx, TriggerDiskPct, "n1", "/data")

	// Re-fire immediately, carrying forward the LastNotifiedAt the
	// recovered notification just set — exactly what a real OK->pending
	// transition does on its own (see computeTransition's StateOK case),
	// just without waiting out the hold a second time. Well inside the
	// 1800s cooldown measured from that recovered notification.
	seedPastHold(okState.LastNotifiedAt)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, h.clock.now())})
	h.eval.EvaluateOnce(ctx)

	afterRefire := h.notifier.all()
	if len(afterRefire) != 3 || afterRefire[2].Kind != KindFlapping {
		t.Fatalf("after the suppressed re-fire, sent = %+v, want exactly one more: flapping", afterRefire)
	}
	if s, _, _ := h.states.GetState(ctx, TriggerDiskPct, "n1", "/data"); s.State != StateFiring {
		t.Fatalf("state after the suppressed re-fire = %v, want firing (the state still transitions)", s.State)
	}

	// A second tick, still firing, nothing changed: no further flapping.
	h.eval.EvaluateOnce(ctx)
	if len(h.notifier.all()) != 3 {
		t.Fatalf("sent %d after a no-op tick while still firing, want still 3: at most one flapping per cycle", len(h.notifier.all()))
	}

	// Finally recover for real: the eventual recovered message still goes out.
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 10, h.clock.now())})
	h.eval.EvaluateOnce(ctx) // firing -> recovering
	h.clock.advance(120 * time.Second)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 10, h.clock.now())})
	h.eval.EvaluateOnce(ctx) // recovering -> ok, sends recovered

	final := h.notifier.all()
	if len(final) != 4 || final[3].Kind != KindRecovered {
		t.Fatalf("final sent = %+v, want exactly one more: recovered", final)
	}
}

// TestEvaluatorRefireAfterCooldownFiresNormally proves cooldown is a
// window, not a permanent downgrade: once cooldown_seconds has actually
// elapsed since the last notification, a fresh activation sends a normal
// firing, not flapping.
func TestEvaluatorRefireAfterCooldownFiresNormally(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now}}, "")
	rule := h.rules.rules[TriggerDiskPct]
	rule.CooldownSeconds = 60
	h.rules.rules[TriggerDiskPct] = rule
	ctx := context.Background()

	seedPastHold := func(lastNotifiedAt *time.Time) {
		since := h.clock.now().Add(-300 * time.Second)
		h.states.UpsertState(ctx, State{
			Trigger: TriggerDiskPct, NodeID: "n1", Subject: "/data", State: StatePending,
			StateSince: since, ConditionSince: &since, LastNotifiedAt: lastNotifiedAt,
		})
	}

	seedPastHold(nil)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, h.clock.now())})
	h.eval.EvaluateOnce(ctx) // -> firing (first activation, sends firing)

	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 10, h.clock.now())})
	h.eval.EvaluateOnce(ctx) // -> recovering
	h.clock.advance(120 * time.Second)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 10, h.clock.now())})
	h.eval.EvaluateOnce(ctx) // -> ok, sends recovered
	okState, _, _ := h.states.GetState(ctx, TriggerDiskPct, "n1", "/data")

	// Well past the 60s cooldown by the time it re-fires.
	h.clock.advance(100 * time.Second)
	seedPastHold(okState.LastNotifiedAt)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, h.clock.now())})
	h.eval.EvaluateOnce(ctx)

	sent := h.notifier.all()
	if len(sent) != 3 || sent[2].Kind != KindFiring {
		t.Fatalf("sent = %+v, want the third notification to be a normal KindFiring", sent)
	}
}

// TestEvaluatorRestartMidPending and TestEvaluatorRestartMidFiring cover
// item 4: a fresh Evaluator instance (a stand-in for a server restart,
// since all state lives in the StateStore, not the Evaluator itself) must
// continue a transition already in progress rather than lose or repeat it.

func TestEvaluatorRestartMidPending(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now}}, "")
	conditionSince := now.Add(-299 * time.Second) // 1s short of disk's 300s hold
	h.states.UpsertState(context.Background(), State{
		Trigger: TriggerDiskPct, NodeID: "n1", Subject: "/data", State: StatePending,
		StateSince: conditionSince, ConditionSince: &conditionSince,
	})

	// A brand-new Evaluator over the same stores and clock — nothing about
	// restart safety lives in the Evaluator struct itself.
	fresh := NewEvaluator(EvaluatorOptions{
		Rules: h.rules, States: h.states, Nodes: h.nodes, Metrics: h.metrics, Notifier: h.notifier, Now: h.clock.now,
	})

	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, now)})
	h.clock.advance(2 * time.Second) // now past the 300s hold
	fresh.EvaluateOnce(context.Background())

	got, _, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/data")
	if got.State != StateFiring {
		t.Fatalf("state = %v, want firing: the pending hold should have completed across the restart", got.State)
	}
	if len(h.notifier.all()) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1", len(h.notifier.all()))
	}
}

func TestEvaluatorRestartMidFiring(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now}}, "")
	h.states.UpsertState(context.Background(), State{
		Trigger: TriggerDiskPct, NodeID: "n1", Subject: "/data", State: StateFiring, Severity: SeverityWarning,
		StateSince: now.Add(-time.Hour),
	})

	fresh := NewEvaluator(EvaluatorOptions{
		Rules: h.rules, States: h.states, Nodes: h.nodes, Metrics: h.metrics, Notifier: h.notifier, Now: h.clock.now,
	})
	// Still firing, now critical: escalation must still fire across the restart.
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 95, now)})
	fresh.EvaluateOnce(context.Background())

	got, _, _ := h.states.GetState(context.Background(), TriggerDiskPct, "n1", "/data")
	if got.State != StateFiring || got.Severity != SeverityCritical {
		t.Fatalf("state = %+v, want firing/critical", got)
	}
	sent := h.notifier.all()
	if len(sent) != 1 || sent[0].Kind != KindEscalated {
		t.Fatalf("sent = %+v, want exactly one KindEscalated", sent)
	}
}

// TestEvaluatorDeterministicEventIDs covers fix-round item 1 directly:
// retrying the exact same pre-transition (trigger, node, subject) state
// produces the same event id even when the clock has moved on in
// between — proving the id is anchored on current (pre-transition), not
// next.StateSince (the previous version's bug: that field is set to the
// tick's own now on a firing transition, so a retry at a later now used
// to mint a different id every time, which Enqueue's
// UNIQUE(event_id, channel_id) could never dedupe).
func TestEvaluatorDeterministicEventIDs(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: now}}, "")
	conditionSince := now.Add(-300 * time.Second)
	seed := func() {
		h.states.UpsertState(context.Background(), State{
			Trigger: TriggerDiskPct, NodeID: "n1", Subject: "/data", State: StatePending,
			StateSince: conditionSince, ConditionSince: &conditionSince,
		})
	}

	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, now)})

	// First attempt.
	seed()
	h.eval.EvaluateOnce(context.Background())
	first := h.notifier.all()
	if len(first) != 1 {
		t.Fatalf("first attempt sent %d, want 1", len(first))
	}

	// Simulate a crash before the state write landed: re-seed the exact
	// same pre-transition state, advance the clock (a real restart would
	// not resume at the same instant), and evaluate again.
	h.clock.advance(5 * time.Minute)
	h.metrics.set(MetricDiskUsage, []MetricSample{diskSample("n1", "/data", 85, h.clock.now())})
	seed()
	h.eval.EvaluateOnce(context.Background())
	second := h.notifier.all()
	if len(second) != 2 {
		t.Fatalf("second attempt total = %d, want 2 (one more call to Notify)", len(second))
	}
	if second[0].EventID != second[1].EventID {
		t.Errorf("event ids differ across the replay despite the clock moving on: %q vs %q, want identical",
			second[0].EventID, second[1].EventID)
	}
}

// TestEventAnchorUsesThePreTransitionState covers eventAnchor directly:
// for every kind, the anchor must come from current (what was true before
// this tick), never from a field computeTransition only set this tick.
func TestEventAnchorUsesThePreTransitionState(t *testing.T) {
	conditionSince := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stateSince := conditionSince.Add(time.Hour)

	firingAnchor := eventAnchor(KindFiring, State{State: StatePending, StateSince: stateSince, ConditionSince: &conditionSince})
	if !firingAnchor.Equal(conditionSince) {
		t.Errorf("firing anchor = %v, want the pending state's own ConditionSince %v", firingAnchor, conditionSince)
	}

	flappingAnchor := eventAnchor(KindFlapping, State{State: StatePending, StateSince: stateSince, ConditionSince: &conditionSince})
	if !flappingAnchor.Equal(conditionSince) {
		t.Errorf("flapping anchor = %v, want the same ConditionSince firing would use", flappingAnchor)
	}

	escalatedAnchor := eventAnchor(KindEscalated, State{State: StateFiring, StateSince: stateSince})
	if !escalatedAnchor.Equal(stateSince) {
		t.Errorf("escalated anchor = %v, want the firing state's own StateSince %v", escalatedAnchor, stateSince)
	}

	recoveredAnchor := eventAnchor(KindRecovered, State{State: StateRecovering, StateSince: stateSince})
	if !recoveredAnchor.Equal(stateSince) {
		t.Errorf("recovered anchor = %v, want the recovering state's own StateSince %v", recoveredAnchor, stateSince)
	}
}

// TestEvaluatorUnreadableRuleOrEmptyNodeNeverErrors covers item 6: a rule
// store error, a node with no metric data, and a newly-created node (no
// prior state) must all be handled without a panic.
func TestEvaluatorUnreadableRuleOrEmptyNodeNeverErrors(t *testing.T) {
	h := newHarness(t, []EvalNode{{NodeID: "brand-new", LastSeenAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}, "")
	h.rules.rules = map[Trigger]Rule{} // no rules at all: EffectiveRule errors for every trigger

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EvaluateOnce panicked: %v", r)
		}
	}()
	h.eval.EvaluateOnce(context.Background())

	if len(h.notifier.all()) != 0 {
		t.Errorf("sent %d notifications with no rules configured, want 0", len(h.notifier.all()))
	}
}

const longNodeID = "b53fd026994b1bf7c0ffee0123456789"

// runUntil ticks the harness every minute, keeping the node seen when seen
// is true and publishing whatever publish sets, until want notifications
// have been sent in total.
func runUntil(t *testing.T, h *evalHarness, seen bool, publish func(now time.Time), want int) []Notification {
	t.Helper()
	for range 30 {
		if seen {
			h.nodes.mu.Lock()
			h.nodes.nodes[longNodeID].LastSeenAt = h.clock.now()
			h.nodes.mu.Unlock()
		}
		publish(h.clock.now())
		h.eval.EvaluateOnce(context.Background())
		if got := h.notifier.all(); len(got) >= want {
			return got
		}
		h.clock.advance(time.Minute)
	}
	t.Fatalf("sent %d notifications, want %d", len(h.notifier.all()), want)
	return nil
}

func TestEvaluatorMessagesDoNotRepeatTheNodeID(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	node := []EvalNode{{NodeID: longNodeID, LastSeenAt: start}}
	none := func(time.Time) {}

	t.Run("disk", func(t *testing.T) {
		h := newHarness(t, node, "")
		value := 95.0
		publish := func(now time.Time) {
			h.metrics.set(MetricDiskUsage, []MetricSample{diskSample(longNodeID, "/var", value, now)})
		}
		fired := runUntil(t, h, true, publish, 1)
		value = 10
		sent := runUntil(t, h, true, publish, 2)
		wantMessages(t, sent, fired[0], "/var is at 95.0%", "/var is at 10.0%")
	})
	t.Run("ram", func(t *testing.T) {
		h := newHarness(t, node, "")
		value := 90.0
		publish := func(now time.Time) { h.metrics.set(MetricRAMUsage, []MetricSample{ramSample(longNodeID, value, now)}) }
		fired := runUntil(t, h, true, publish, 1)
		value = 10
		sent := runUntil(t, h, true, publish, 2)
		wantMessages(t, sent, fired[0], "RAM is at 90.0%", "RAM is at 10.0%")
	})
	t.Run("node_down", func(t *testing.T) {
		h := newHarness(t, node, "")
		fired := runUntil(t, h, false, none, 1)
		if fired[0].Title != "Node unreachable" {
			t.Errorf("title = %q", fired[0].Title)
		}
		sent := runUntil(t, h, true, none, 2)
		// The condition holds after 2m silent, then 8m more: 11m at the
		// minute it fires.
		wantMessages(t, sent, fired[0], "the node has not reported for 11m", "the node is reporting again")
	})
}

func wantMessages(t *testing.T, sent []Notification, fired Notification, firing, recovered string) {
	t.Helper()
	if len(sent) != 2 || sent[1].Kind != KindRecovered {
		t.Fatalf("sent = %+v, want a firing then a recovered", sent)
	}
	if fired.Message != firing || sent[1].Message != recovered {
		t.Errorf("messages = %q / %q, want %q / %q", fired.Message, sent[1].Message, firing, recovered)
	}
	for _, n := range sent {
		if strings.Contains(n.Message, longNodeID) || strings.Contains(n.Title, longNodeID) {
			t.Errorf("notification repeats the node id: %+v", n)
		}
		if n.NodeID != longNodeID {
			t.Errorf("NodeID = %q, want the node as its own field", n.NodeID)
		}
	}
}

func TestSilenceText(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		40 * time.Second:                        "40s",
		45*time.Second + time.Millisecond:       "45s",
		11*time.Minute + 30*time.Second:         "11m",
		time.Hour + 5*time.Minute + time.Second: "1h5m",
	} {
		if got := silenceText(d); got != want {
			t.Errorf("silenceText(%s) = %q, want %q", d, got, want)
		}
	}
}

// recordingHandler keeps every log record for inspection.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) transitionsAt(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Message == "notification state transition" && r.Level == level {
			n++
		}
	}
	return n
}

func TestBaselineWritesDoNotLogTransitionsAtInfo(t *testing.T) {
	h := newWorkloadHarness(t)
	logs := &recordingHandler{}
	h.eval = NewEvaluator(EvaluatorOptions{
		Rules: h.rules, States: h.states, Nodes: h.nodes, Metrics: h.metrics, Notifier: h.notifier,
		Now: h.clock.now, Logger: slog.New(logs),
		ContainerCollectorInterval: 30 * time.Second, ServiceCollectorInterval: 60 * time.Second,
	})
	h.containers["web"] = fakeContainer{up: 1}

	h.tick()
	h.tick()
	if got := logs.transitionsAt(slog.LevelInfo); got != 0 {
		t.Fatalf("baseline ticks logged %d transitions at INFO, want 0", got)
	}
	if logs.transitionsAt(slog.LevelDebug) == 0 {
		t.Error("baseline writes were not logged at DEBUG")
	}

	h.containers["web"] = fakeContainer{up: 0, exitCode: 137}
	h.tick()
	h.wantKinds(KindFiring)
	if got := logs.transitionsAt(slog.LevelInfo); got == 0 {
		t.Error("a real firing transition was not logged at INFO")
	}
}
