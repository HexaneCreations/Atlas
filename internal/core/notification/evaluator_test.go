package notification

import (
	"context"
	"errors"
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
// firing notification (the state still transitions) when the last
// notification was too recent, but never withholds an escalation.
func TestComputeTransitionCooldown(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := percentRule()
	rule.CooldownSeconds = 600

	lastNotified := t0.Add(-100 * time.Second) // 100s ago, within the 600s cooldown
	pending := State{
		State: StatePending, StateSince: t0.Add(-300 * time.Second), ConditionSince: ptr(t0.Add(-300 * time.Second)),
		LastNotifiedAt: &lastNotified,
	}
	next, kind, _ := computeTransition(rule, pending, true, true, ptr(85.0), severityForPercentRule(rule, ""), t0)
	if next.State != StateFiring {
		t.Fatalf("state = %v, want firing even though the notification is suppressed", next.State)
	}
	if kind != "" {
		t.Errorf("kind = %v, want none: within cooldown of the last notification", kind)
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
	mu     sync.Mutex
	states map[stateKey]State
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
	f.states[stateKey{string(s.Trigger), s.NodeID, s.Subject}] = s
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

type fakeEvalNodeStore struct{ nodes []EvalNode }

func (f fakeEvalNodeStore) ListNodes(context.Context) ([]EvalNode, error) { return f.nodes, nil }

type fakeMetricSource struct {
	mu      sync.Mutex
	samples map[string][]MetricSample
	now     func() time.Time
}

func (f *fakeMetricSource) LatestForMetric(_ context.Context, metric string, within time.Duration) ([]MetricSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// defaultRules builds the three triggers this step covers, matching
// migration 0024's seeded defaults: disk 80/90/5, ram 85/95/5, node_down
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
		nodes:    &fakeEvalNodeStore{nodes: nodes},
		notifier: &fakeNotifier{},
	}
	h.metrics = &fakeMetricSource{samples: map[string][]MetricSample{}, now: h.clock.now}
	h.eval = NewEvaluator(EvaluatorOptions{
		Rules: h.rules, States: h.states, Nodes: h.nodes, Metrics: h.metrics, Notifier: h.notifier,
		Now: h.clock.now, LocalNodeID: localNodeID,
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

// TestEvaluatorDeterministicEventIDs covers item 4's mechanism directly:
// the same (trigger, node, subject, state_since, kind) always produces
// the same event id, so a crash-and-retry of the same transition
// reproduces it exactly rather than minting a new one Enqueue cannot dedupe.
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
	// same pre-transition state and evaluate again.
	seed()
	h.eval.EvaluateOnce(context.Background())
	second := h.notifier.all()
	if len(second) != 2 {
		t.Fatalf("second attempt total = %d, want 2 (one more call to Notify)", len(second))
	}
	if second[0].EventID != second[1].EventID {
		t.Errorf("event ids differ across the replay: %q vs %q, want identical", second[0].EventID, second[1].EventID)
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
