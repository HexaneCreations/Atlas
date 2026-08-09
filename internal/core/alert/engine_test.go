package alert

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/eventstore"
)

type fakeStore struct {
	mu      sync.Mutex
	rules   []Rule
	states  map[string]AlertState
	history []HistoryEntry
}

func newFakeStore(rules ...Rule) *fakeStore {
	return &fakeStore{rules: rules, states: map[string]AlertState{}}
}

func stateKey(ruleID, nodeID, series string) string { return ruleID + "|" + nodeID + "|" + series }

func (f *fakeStore) ListRules(context.Context) ([]Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Rule(nil), f.rules...), nil
}
func (f *fakeStore) GetRule(context.Context, string) (Rule, error)      { return Rule{}, nil }
func (f *fakeStore) CreateRule(_ context.Context, r Rule) (Rule, error) { return r, nil }
func (f *fakeStore) UpdateRule(_ context.Context, r Rule) (Rule, error) { return r, nil }
func (f *fakeStore) DeleteRule(context.Context, string) error           { return nil }

func (f *fakeStore) GetState(_ context.Context, ruleID, nodeID, series string) (AlertState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.states[stateKey(ruleID, nodeID, series)]
	return s, ok, nil
}

func (f *fakeStore) SaveState(_ context.Context, s AlertState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[stateKey(s.RuleID, s.NodeID, s.SeriesKey)] = s
	return nil
}

func (f *fakeStore) ListActiveStates(context.Context) ([]AlertState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []AlertState
	for _, s := range f.states {
		if s.State != StateOK {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) AppendHistory(_ context.Context, e HistoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history = append(f.history, e)
	return nil
}

func (f *fakeStore) QueryHistory(context.Context, HistoryFilter) ([]HistoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HistoryEntry(nil), f.history...), nil
}

func (f *fakeStore) state(ruleID, nodeID, series string) (AlertState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.states[stateKey(ruleID, nodeID, series)]
	return s, ok
}

func (f *fakeStore) historyLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.history)
}

func testEngine(store Store) *Engine {
	return NewEngine(Options{Store: store})
}

func cpuRule() Rule {
	return Rule{
		ID: "rule-cpu", Name: "High CPU", Enabled: true, Kind: KindThreshold, Severity: SeverityWarning,
		Metric: "system.cpu.usage", Comparison: ComparisonGT, Threshold: 90,
	}
}

func TestThresholdFiresImmediatelyWithNoForDuration(t *testing.T) {
	rule := cpuRule()
	store := newFakeStore(rule)
	e := testEngine(store)

	e.applyThreshold(context.Background(), rule, "node-1", "", 95)

	state, ok := store.state(rule.ID, "node-1", "")
	if !ok || state.State != StateFiring {
		t.Fatalf("expected firing, got %+v (found=%v)", state, ok)
	}
	if store.historyLen() != 1 {
		t.Fatalf("expected one history entry, got %d", store.historyLen())
	}
}

func TestThresholdRequiresForDurationBeforeFiring(t *testing.T) {
	rule := cpuRule()
	rule.For = time.Minute
	store := newFakeStore(rule)
	e := testEngine(store)
	ctx := context.Background()

	e.applyThreshold(ctx, rule, "node-1", "", 95)
	state, ok := store.state(rule.ID, "node-1", "")
	if !ok || state.State != StatePending {
		t.Fatalf("expected pending immediately after breach, got %+v", state)
	}
	if store.historyLen() != 0 {
		t.Fatalf("pending should not write history yet, got %d entries", store.historyLen())
	}

	// Re-evaluate before the "for" window elapses: still pending.
	e.applyThreshold(ctx, rule, "node-1", "", 96)
	state, _ = store.state(rule.ID, "node-1", "")
	if state.State != StatePending {
		t.Fatalf("expected still pending, got %s", state.State)
	}

	// Force the pending window to have started far enough in the past.
	state.PendingSince = time.Now().Add(-2 * time.Minute)
	if err := store.SaveState(ctx, state); err != nil {
		t.Fatalf("save: %v", err)
	}
	e.applyThreshold(ctx, rule, "node-1", "", 97)
	state, _ = store.state(rule.ID, "node-1", "")
	if state.State != StateFiring {
		t.Fatalf("expected firing once the for-duration elapsed, got %s", state.State)
	}
	if store.historyLen() != 1 {
		t.Fatalf("expected exactly one history entry on the firing transition, got %d", store.historyLen())
	}
}

func TestThresholdResolvesWhenConditionClears(t *testing.T) {
	rule := cpuRule()
	store := newFakeStore(rule)
	e := testEngine(store)
	ctx := context.Background()

	e.applyThreshold(ctx, rule, "node-1", "", 95)
	e.applyThreshold(ctx, rule, "node-1", "", 50)

	state, ok := store.state(rule.ID, "node-1", "")
	if !ok || state.State != StateOK {
		t.Fatalf("expected resolved to ok, got %+v", state)
	}
	if store.historyLen() != 2 {
		t.Fatalf("expected firing + resolved history entries, got %d", store.historyLen())
	}
}

func TestThresholdBelowBoundNeverFiresOrWritesHistory(t *testing.T) {
	rule := cpuRule()
	store := newFakeStore(rule)
	e := testEngine(store)

	e.applyThreshold(context.Background(), rule, "node-1", "", 10)

	if _, ok := store.state(rule.ID, "node-1", ""); ok {
		t.Fatal("expected no state to be written for a value that never breached")
	}
	if store.historyLen() != 0 {
		t.Fatalf("expected no history, got %d", store.historyLen())
	}
}

func TestThresholdSeriesAreIndependentPerLabelSet(t *testing.T) {
	rule := cpuRule()
	rule.Metric = "system.disk.usage"
	store := newFakeStore(rule)
	e := testEngine(store)
	ctx := context.Background()

	e.applyThreshold(ctx, rule, "node-1", seriesKey(map[string]string{"mount": "/"}), 95)
	e.applyThreshold(ctx, rule, "node-1", seriesKey(map[string]string{"mount": "/data"}), 10)

	root, ok := store.state(rule.ID, "node-1", seriesKey(map[string]string{"mount": "/"}))
	if !ok || root.State != StateFiring {
		t.Fatalf("expected / to be firing, got %+v", root)
	}
	if _, ok := store.state(rule.ID, "node-1", seriesKey(map[string]string{"mount": "/data"})); ok {
		t.Fatal("expected /data series to remain untouched below threshold")
	}
}

func TestThresholdRuleScopedToOneNodeIgnoresOthers(t *testing.T) {
	rule := cpuRule()
	rule.NodeID = "node-1"
	store := newFakeStore(rule)
	e := testEngine(store)
	metrics := fakeMetrics{samples: []FleetSample{
		{NodeID: "node-1", Value: 95},
		{NodeID: "node-2", Value: 99},
	}}
	e.metrics = metrics

	e.evaluateThreshold(context.Background(), rule)

	if _, ok := store.state(rule.ID, "node-1", ""); !ok {
		t.Fatal("expected node-1 to be evaluated")
	}
	if _, ok := store.state(rule.ID, "node-2", ""); ok {
		t.Fatal("expected node-2 to be skipped: rule is scoped to node-1")
	}
}

type fakeMetrics struct{ samples []FleetSample }

func (f fakeMetrics) LatestForMetric(context.Context, string, time.Duration) ([]FleetSample, error) {
	return f.samples, nil
}

func TestHandleEventFiresMatchingRuleAndSkipsOthers(t *testing.T) {
	match := Rule{ID: "rule-oom", Name: "OOM", Enabled: true, Kind: KindEvent, Severity: SeverityCritical, Topic: "docker.container.oom"}
	other := Rule{ID: "rule-reboot", Name: "Reboot", Enabled: true, Kind: KindEvent, Severity: SeverityWarning, Topic: "system.host.rebooted"}
	disabled := Rule{ID: "rule-disabled", Name: "Disabled", Enabled: false, Kind: KindEvent, Severity: SeverityWarning, Topic: "docker.container.oom"}
	store := newFakeStore(match, other, disabled)
	e := testEngine(store)

	e.HandleEvent(context.Background(), eventstore.Record{
		ID: "evt-1", Topic: "docker.container.oom", NodeID: "node-1", Subject: "container-abc",
	})

	if store.historyLen() != 1 {
		t.Fatalf("expected exactly one history entry, got %d", store.historyLen())
	}
	entry := store.history[0]
	if entry.RuleID != match.ID || entry.NodeID != "node-1" || entry.State != StateFiring {
		t.Fatalf("unexpected history entry: %+v", entry)
	}
}

func TestHandleEventRespectsSubjectFilter(t *testing.T) {
	rule := Rule{ID: "rule-1", Name: "Specific container", Enabled: true, Kind: KindEvent, Severity: SeverityWarning,
		Topic: "docker.container.oom", Subject: "container-abc"}
	store := newFakeStore(rule)
	e := testEngine(store)

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", Topic: "docker.container.oom", NodeID: "n1", Subject: "container-xyz"})
	if store.historyLen() != 0 {
		t.Fatalf("expected no match for a different subject, got %d entries", store.historyLen())
	}

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", Topic: "docker.container.oom", NodeID: "n1", Subject: "container-abc"})
	if store.historyLen() != 1 {
		t.Fatalf("expected a match for the configured subject, got %d entries", store.historyLen())
	}
}
