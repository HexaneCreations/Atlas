package incident

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/alert"
	"github.com/hexane/atlas/internal/core/eventstore"
)

type fakeStore struct {
	mu        sync.Mutex
	incidents map[string]Incident
	members   []Member
	// envOf backs both FindCorrelatableByEnvironment (as the "nodes" table
	// join) and, when the store doubles as a NodeEnvironments, Environment.
	envOf map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{incidents: map[string]Incident{}, envOf: map[string]string{}}
}

// Environment lets fakeStore double as a [NodeEnvironments] for tests of the
// environment correlation tier.
func (f *fakeStore) Environment(_ context.Context, nodeID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.envOf[nodeID], nil
}

func (f *fakeStore) FindCorrelatable(_ context.Context, nodeID string, since time.Time) (Incident, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var best Incident
	found := false
	for _, m := range f.members {
		if m.NodeID != nodeID || m.Time.Before(since) {
			continue
		}
		inc, ok := f.incidents[m.IncidentID]
		if !ok || inc.Status != StatusOpen {
			continue
		}
		if !found || inc.UpdatedAt.After(best.UpdatedAt) {
			best, found = inc, true
		}
	}
	return best, found, nil
}

func (f *fakeStore) FindCorrelatableByEnvironment(_ context.Context, environment string, since time.Time) (Incident, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var best Incident
	found := false
	for _, m := range f.members {
		if f.envOf[m.NodeID] != environment || m.Time.Before(since) {
			continue
		}
		inc, ok := f.incidents[m.IncidentID]
		if !ok || inc.Status != StatusOpen {
			continue
		}
		if !found || inc.UpdatedAt.After(best.UpdatedAt) {
			best, found = inc, true
		}
	}
	return best, found, nil
}

func (f *fakeStore) CreateIncident(_ context.Context, inc Incident) (Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incidents[inc.ID] = inc
	return inc, nil
}

func (f *fakeStore) UpdateIncident(_ context.Context, inc Incident) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incidents[inc.ID] = inc
	return nil
}

func (f *fakeStore) AddMember(_ context.Context, m Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.members {
		if existing.Kind == m.Kind && existing.RefID == m.RefID {
			return nil
		}
	}
	f.members = append(f.members, m)
	return nil
}

func (f *fakeStore) ListIncidents(context.Context, Filter) ([]Incident, error) { return nil, nil }

func (f *fakeStore) GetIncident(_ context.Context, id string) (Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.incidents[id], nil
}

func (f *fakeStore) GetDetail(context.Context, string) (Detail, error) { return Detail{}, nil }

func (f *fakeStore) ListOpenIncidents(context.Context) ([]Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Incident
	for _, inc := range f.incidents {
		if inc.Status == StatusOpen {
			out = append(out, inc)
		}
	}
	return out, nil
}

func (f *fakeStore) membersOf(incidentID string) []Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Member
	for _, m := range f.members {
		if m.IncidentID == incidentID {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeStore) incidentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.incidents)
}

func testEngine(store Store) *Engine {
	return NewEngine(Options{Store: store, Window: 10 * time.Minute, ResolveAfter: 15 * time.Minute})
}

func testEngineWithEnvironments(store *fakeStore) *Engine {
	return NewEngine(Options{Store: store, Environments: store, Window: 10 * time.Minute, ResolveAfter: 15 * time.Minute})
}

func TestEventOpensIncidentForNotableTopic(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{
		ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now,
	})

	if store.incidentCount() != 1 {
		t.Fatalf("expected one incident, got %d", store.incidentCount())
	}
}

func TestEventIgnoresRoutineTopic(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)

	e.HandleEvent(context.Background(), eventstore.Record{
		ID: "evt-1", NodeID: "node-1", Topic: "docker.container.started", Time: time.Now(),
	})

	if store.incidentCount() != 0 {
		t.Fatalf("expected no incident for a routine topic, got %d", store.incidentCount())
	}
}

func TestEventIgnoresUnknownTopic(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)

	e.HandleEvent(context.Background(), eventstore.Record{
		ID: "evt-1", NodeID: "node-1", Topic: "some.unclassified.topic", Time: time.Now(),
	})

	if store.incidentCount() != 0 {
		t.Fatalf("expected no incident for an unclassified topic, got %d", store.incidentCount())
	}
}

func TestSecondOccurrenceOnSameNodeWithinWindowJoinsIncident(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now})
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-1", Topic: "docker.container.died", Time: now.Add(time.Minute)})

	if store.incidentCount() != 1 {
		t.Fatalf("expected both events to join one incident, got %d incidents", store.incidentCount())
	}

	var incID string
	for id := range store.incidents {
		incID = id
	}
	if got := len(store.membersOf(incID)); got != 2 {
		t.Fatalf("expected 2 members, got %d", got)
	}
}

func TestOccurrenceOutsideWindowOpensNewIncident(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now})
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-1", Topic: "docker.container.died", Time: now.Add(20 * time.Minute)})

	if store.incidentCount() != 2 {
		t.Fatalf("expected a second incident once the window elapsed, got %d", store.incidentCount())
	}
}

func TestOccurrenceOnDifferentNodeOpensNewIncidentWithoutEnvironments(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now})
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-2", Topic: "docker.container.oom", Time: now})

	if store.incidentCount() != 2 {
		t.Fatalf("expected correlation to stay per-node with no NodeEnvironments configured, got %d incidents", store.incidentCount())
	}
}

func TestCrossNodeSameEnvironmentCorrelates(t *testing.T) {
	store := newFakeStore()
	store.envOf["node-1"] = "production"
	store.envOf["node-2"] = "production"
	e := testEngineWithEnvironments(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now})
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-2", Topic: "docker.container.oom", Time: now.Add(time.Minute)})

	if store.incidentCount() != 1 {
		t.Fatalf("expected both nodes' events to join one incident via the environment tier, got %d incidents", store.incidentCount())
	}

	var incID string
	for id := range store.incidents {
		incID = id
	}
	members := store.membersOf(incID)
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	nodes := map[string]bool{}
	for _, m := range members {
		nodes[m.NodeID] = true
	}
	if !nodes["node-1"] || !nodes["node-2"] {
		t.Fatalf("expected members from both nodes, got %+v", members)
	}
}

func TestCrossNodeDifferentEnvironmentOpensNewIncident(t *testing.T) {
	store := newFakeStore()
	store.envOf["node-1"] = "production"
	store.envOf["node-2"] = "staging"
	e := testEngineWithEnvironments(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now})
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-2", Topic: "docker.container.oom", Time: now})

	if store.incidentCount() != 2 {
		t.Fatalf("expected different environments to stay separate incidents, got %d", store.incidentCount())
	}
}

func TestUntaggedNodesNeverJoinEnvironmentTier(t *testing.T) {
	store := newFakeStore()
	// Both nodes left untagged (envOf defaults to "").
	e := testEngineWithEnvironments(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now})
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-2", Topic: "docker.container.oom", Time: now})

	if store.incidentCount() != 2 {
		t.Fatalf("expected untagged nodes not to correlate via environment, got %d incidents", store.incidentCount())
	}
}

func TestSameNodeTierIsTriedBeforeEnvironmentTier(t *testing.T) {
	store := newFakeStore()
	store.envOf["node-1"] = "production"
	store.envOf["node-2"] = "production"
	e := testEngineWithEnvironments(store)
	now := time.Now()

	// node-2 opens its own incident first.
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-2", Topic: "docker.container.oom", Time: now})
	// node-1's first occurrence has no same-node incident to join, so it
	// should join node-2's incident via the environment tier.
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-1", Topic: "docker.container.oom", Time: now.Add(time.Minute)})
	// node-1's second occurrence now has a same-node incident (the one it
	// just joined) — it must stay there, not open a third incident.
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-3", NodeID: "node-1", Topic: "docker.container.died", Time: now.Add(2 * time.Minute)})

	if store.incidentCount() != 1 {
		t.Fatalf("expected one incident across both nodes, got %d", store.incidentCount())
	}
}

func TestRootCauseIsTheFirstMember(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "collector.run.panicked", Time: now})
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-1", Topic: "docker.container.oom", Time: now.Add(time.Minute)})

	var inc Incident
	for _, v := range store.incidents {
		inc = v
	}
	if inc.RootCauseRefID != "evt-1" {
		t.Fatalf("expected the first event to be the root cause, got %s", inc.RootCauseRefID)
	}

	members := store.membersOf(inc.ID)
	rootCount := 0
	for _, m := range members {
		if m.IsRootCause {
			rootCount++
			if m.RefID != "evt-1" {
				t.Errorf("wrong member marked as root cause: %s", m.RefID)
			}
		}
	}
	if rootCount != 1 {
		t.Fatalf("expected exactly one root-cause member, got %d", rootCount)
	}
}

func TestSeverityEscalatesButNeverDowngrades(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)
	now := time.Now()

	// collector.run.failed is a warning-level topic.
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "collector.run.failed", Time: now})
	var inc Incident
	for _, v := range store.incidents {
		inc = v
	}
	if inc.Severity != SeverityWarning {
		t.Fatalf("expected warning severity, got %s", inc.Severity)
	}

	// collector.run.panicked is danger-level; the incident should escalate.
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-1", Topic: "collector.run.panicked", Time: now.Add(time.Minute)})
	for _, v := range store.incidents {
		inc = v
	}
	if inc.Severity != SeverityCritical {
		t.Fatalf("expected escalation to critical, got %s", inc.Severity)
	}

	// A subsequent warning-level event must not downgrade it back.
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-3", NodeID: "node-1", Topic: "collector.run.failed", Time: now.Add(2 * time.Minute)})
	for _, v := range store.incidents {
		inc = v
	}
	if inc.Severity != SeverityCritical {
		t.Fatalf("severity must not downgrade, got %s", inc.Severity)
	}
}

func TestAlertTransitionOnlyFiringCorrelates(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)

	e.HandleAlertTransition(context.Background(), alert.HistoryEntry{
		ID: "hist-1", RuleID: "rule-1", NodeID: "node-1", State: alert.StateOK, Time: time.Now(),
	})
	if store.incidentCount() != 0 {
		t.Fatalf("expected a resolution transition not to open an incident, got %d", store.incidentCount())
	}

	e.HandleAlertTransition(context.Background(), alert.HistoryEntry{
		ID: "hist-2", RuleID: "rule-1", NodeID: "node-1", State: alert.StateFiring,
		Severity: alert.SeverityCritical, Time: time.Now(), Message: "High CPU",
	})
	if store.incidentCount() != 1 {
		t.Fatalf("expected a firing transition to open an incident, got %d", store.incidentCount())
	}
}

func TestEventAndAlertOnSameNodeCorrelateIntoOneIncident(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now})
	e.HandleAlertTransition(context.Background(), alert.HistoryEntry{
		ID: "hist-1", RuleID: "rule-1", NodeID: "node-1", State: alert.StateFiring,
		Severity: alert.SeverityWarning, Time: now.Add(time.Minute),
	})

	if store.incidentCount() != 1 {
		t.Fatalf("expected the event and the alert to correlate into one incident, got %d", store.incidentCount())
	}
}

func TestResolveQuietClosesStaleIncidentsOnly(t *testing.T) {
	store := newFakeStore()
	e := testEngine(store)
	now := time.Now()

	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-1", NodeID: "node-1", Topic: "docker.container.oom", Time: now.Add(-20 * time.Minute)})
	e.HandleEvent(context.Background(), eventstore.Record{ID: "evt-2", NodeID: "node-2", Topic: "docker.container.oom", Time: now})

	e.resolveQuiet(context.Background())

	var stale, fresh Incident
	for _, inc := range store.incidents {
		for _, m := range store.membersOf(inc.ID) {
			if m.NodeID == "node-1" {
				stale = inc
			}
			if m.NodeID == "node-2" {
				fresh = inc
			}
		}
	}
	if stale.Status != StatusResolved {
		t.Fatalf("expected the quiet incident to resolve, got %s", stale.Status)
	}
	if fresh.Status != StatusOpen {
		t.Fatalf("expected the recently updated incident to stay open, got %s", fresh.Status)
	}
}
