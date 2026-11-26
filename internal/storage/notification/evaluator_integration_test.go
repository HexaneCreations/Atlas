//go:build integration

package notification_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/storage/metric"
	"github.com/hexane/atlas/internal/storage/notification"
)

// evaluatorFakeClock is a small, local stand-in for a real clock — item 5
// of the brief: no sleeps, time advances only when the test says so.
type evaluatorFakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newEvaluatorFakeClock(t time.Time) *evaluatorFakeClock { return &evaluatorFakeClock{t: t} }
func (c *evaluatorFakeClock) now() time.Time                { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *evaluatorFakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// evalNodeAdapter and evalMetricAdapter mirror internal/app/notification.go's
// own adapters over *metric.Repository — duplicated here (not imported;
// internal/app is not importable from a storage package) so this test
// drives the evaluator against the real node and metric tables rather
// than a fake.
type evalNodeAdapter struct{ repo *metric.Repository }

func (a evalNodeAdapter) ListNodes(ctx context.Context) ([]corenotification.EvalNode, error) {
	nodes, err := a.repo.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]corenotification.EvalNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, corenotification.EvalNode{
			NodeID: n.NodeID, LastSeenAt: n.LastSeenAt, ArchivedAt: n.ArchivedAt, AlertsMutedUntil: n.AlertsMutedUntil,
		})
	}
	return out, nil
}

type evalMetricAdapter struct{ repo *metric.Repository }

func (a evalMetricAdapter) LatestForMetric(ctx context.Context, name string, within time.Duration) ([]corenotification.MetricSample, error) {
	values, err := a.repo.LatestForMetric(ctx, name, within)
	if err != nil {
		return nil, err
	}
	out := make([]corenotification.MetricSample, 0, len(values))
	for _, v := range values {
		out = append(out, corenotification.MetricSample{NodeID: v.NodeID, Labels: v.Labels, Value: v.Value, Time: v.Time})
	}
	return out, nil
}

// countingFakeSender is a [corenotification.Sender] that never touches the
// network: it just counts calls and remembers what it was asked to send,
// so the test can assert on kinds without a real Teams/webhook secret.
type countingFakeSender struct {
	calls atomic.Int32
	mu    sync.Mutex
	sent  []corenotification.Delivery
}

func (s *countingFakeSender) Send(_ context.Context, _ corenotification.Channel, _ string, d corenotification.Delivery) error {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, d)
	return nil
}

func (s *countingFakeSender) kinds() []corenotification.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]corenotification.Kind, 0, len(s.sent))
	for _, d := range s.sent {
		out = append(out, d.Kind)
	}
	return out
}

// TestEvaluatorFullCycleOKToFiringToRecovered drives node_down through a
// complete ok -> pending -> firing -> recovering -> ok (recovered) cycle
// against the real notification repository and the real node table,
// through the real [corenotification.Engine] dispatcher wired with a
// fake Sender — the test required by the brief.
func TestEvaluatorFullCycleOKToFiringToRecovered(t *testing.T) {
	repo, pool := newRepository(t)
	metricRepo := metric.NewRepository(pool)
	ctx := context.Background()

	const nodeID = "cycle-node"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ($1, $1, now())`, nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	secret := "fake-secret"
	channel, err := repo.CreateChannel(ctx, channelInput("evaluator-cycle", &secret))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	sender := &countingFakeSender{}
	engine := corenotification.NewEngine(corenotification.Options{
		Channels: repo, Deliveries: repo, Logger: nil,
		Senders: map[corenotification.ChannelType]corenotification.Sender{channel.Type: sender},
	})

	clock := newEvaluatorFakeClock(time.Now().UTC())
	eval := corenotification.NewEvaluator(corenotification.EvaluatorOptions{
		Rules: repo, States: repo, Notifier: engine, Now: clock.now,
		Nodes:   evalNodeAdapter{repo: metricRepo},
		Metrics: evalMetricAdapter{repo: metricRepo},
	})

	// Tick 1: the node was just seen, condition false. Nothing happens.
	eval.EvaluateOnce(ctx)
	engine.DispatchOnce(ctx)
	if sender.calls.Load() != 0 {
		t.Fatalf("calls = %d after tick 1, want 0 (node is current)", sender.calls.Load())
	}

	// The node goes silent: advance the clock past node_down's 120s
	// condition without touching last_seen_at.
	clock.advance(121 * time.Second)
	eval.EvaluateOnce(ctx)
	engine.DispatchOnce(ctx)
	if sender.calls.Load() != 0 {
		t.Fatalf("calls = %d after entering pending, want 0 (for_seconds has not elapsed)", sender.calls.Load())
	}

	// for_seconds (480s, the seeded node_down default) elapses: firing,
	// and the dispatcher actually sends it.
	clock.advance(480 * time.Second)
	eval.EvaluateOnce(ctx)
	engine.DispatchOnce(ctx)
	if sender.calls.Load() != 1 {
		t.Fatalf("calls = %d after the hold elapsed, want 1 (firing)", sender.calls.Load())
	}

	// The node reports again: condition false, firing -> recovering.
	if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at = $2 WHERE node_id = $1`, nodeID, clock.now()); err != nil {
		t.Fatalf("mark the node seen again: %v", err)
	}
	eval.EvaluateOnce(ctx)
	engine.DispatchOnce(ctx)
	if sender.calls.Load() != 1 {
		t.Fatalf("calls = %d while recovering, want still 1 (no notification until recovery_seconds elapses)", sender.calls.Load())
	}

	// recovery_seconds (120s, seeded default) elapses: ok, and
	// send_recovery (seeded true) means the dispatcher sends "recovered".
	clock.advance(120 * time.Second)
	eval.EvaluateOnce(ctx)
	engine.DispatchOnce(ctx)
	if sender.calls.Load() != 2 {
		t.Fatalf("calls = %d after recovery, want 2 (firing, then recovered)", sender.calls.Load())
	}

	kinds := sender.kinds()
	if len(kinds) != 2 || kinds[0] != corenotification.KindFiring || kinds[1] != corenotification.KindRecovered {
		t.Fatalf("kinds sent = %v, want [firing recovered]", kinds)
	}

	finalState, found, err := repo.GetState(ctx, corenotification.TriggerNodeDown, nodeID, "")
	if err != nil || !found {
		t.Fatalf("GetState: found=%v err=%v", found, err)
	}
	if finalState.State != corenotification.StateOK {
		t.Errorf("final state = %v, want ok", finalState.State)
	}

	page, err := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{NodeID: nodeID})
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	delivered := 0
	for _, d := range page.Deliveries {
		if d.Status == corenotification.StatusDelivered {
			delivered++
		}
	}
	if delivered != 2 {
		t.Errorf("delivered deliveries = %d, want 2", delivered)
	}
}

// failOnceStateStore wraps the real repository and fails exactly the
// first UpsertState call that would persist a firing transition (the one
// immediately after a successful Notify sets LastNotifiedKind to
// KindFiring) — a stand-in for a crash between that successful Notify and
// the state write that would have advanced past it. Every other method
// always delegates untouched.
type failOnceStateStore struct {
	repo   *notification.Repository
	mu     sync.Mutex
	failed bool
}

func (f *failOnceStateStore) GetState(ctx context.Context, trigger corenotification.Trigger, nodeID, subject string) (corenotification.State, bool, error) {
	return f.repo.GetState(ctx, trigger, nodeID, subject)
}

func (f *failOnceStateStore) ListActiveStates(ctx context.Context) ([]corenotification.State, error) {
	return f.repo.ListActiveStates(ctx)
}

func (f *failOnceStateStore) UpsertState(ctx context.Context, s corenotification.State) error {
	f.mu.Lock()
	shouldFail := !f.failed && s.LastNotifiedKind == corenotification.KindFiring
	if shouldFail {
		f.failed = true
	}
	f.mu.Unlock()
	if shouldFail {
		return errors.New("simulated crash between a successful Notify and the state write that would follow it")
	}
	return f.repo.UpsertState(ctx, s)
}

func (f *failOnceStateStore) EndMuteAfterExpiry(ctx context.Context, nodeID string, now time.Time) (bool, error) {
	return f.repo.EndMuteAfterExpiry(ctx, nodeID, now)
}

func (f *failOnceStateStore) ListNodeStates(ctx context.Context, nodeID string, triggers ...corenotification.Trigger) ([]corenotification.State, error) {
	return f.repo.ListNodeStates(ctx, nodeID, triggers...)
}

func (f *failOnceStateStore) BeginPending(ctx context.Context, s corenotification.State) (corenotification.State, error) {
	return f.repo.BeginPending(ctx, s)
}

func (f *failOnceStateStore) DeleteState(ctx context.Context, trigger corenotification.Trigger, nodeID, subject string) error {
	return f.repo.DeleteState(ctx, trigger, nodeID, subject)
}

func (f *failOnceStateStore) ResetNodeStates(ctx context.Context, nodeID string) error {
	return f.repo.ResetNodeStates(ctx, nodeID)
}

// TestEvaluatorRetriedUpsertFailureStillQueuesExactlyOneDeliveryPerChannel
// covers fix-round item 1's required restart-safety test: Notify
// succeeds (queuing a delivery) but the state write that was supposed to
// follow it fails, so the next tick recomputes and retries the exact same
// transition. The retry must still queue exactly one delivery per
// channel — proving the event id stayed identical across the retry
// (anchored on the pre-transition state, not on anything the failed
// attempt computed fresh) and that Enqueue's
// UNIQUE(event_id, channel_id) actually caught the repeat.
func TestEvaluatorRetriedUpsertFailureStillQueuesExactlyOneDeliveryPerChannel(t *testing.T) {
	repo, pool := newRepository(t)
	metricRepo := metric.NewRepository(pool)
	ctx := context.Background()

	const nodeID = "retry-node"
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ($1, $1, now())`, nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	secret := "fake-secret"
	channel, err := repo.CreateChannel(ctx, channelInput("evaluator-retry", &secret))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	states := &failOnceStateStore{repo: repo}
	clock := newEvaluatorFakeClock(time.Now().UTC())
	eval := corenotification.NewEvaluator(corenotification.EvaluatorOptions{
		Rules: repo, States: states, Notifier: corenotification.NewEngine(corenotification.Options{
			Channels: repo, Deliveries: repo,
			Senders: map[corenotification.ChannelType]corenotification.Sender{channel.Type: &countingFakeSender{}},
		}),
		Now:     clock.now,
		Nodes:   evalNodeAdapter{repo: metricRepo},
		Metrics: evalMetricAdapter{repo: metricRepo},
	})

	// -> pending (past node_down's 120s condition).
	clock.advance(121 * time.Second)
	eval.EvaluateOnce(ctx)
	if s, found, _ := repo.GetState(ctx, corenotification.TriggerNodeDown, nodeID, ""); !found || s.State != corenotification.StatePending {
		t.Fatalf("state after the condition first became true = %+v (found=%v), want pending", s, found)
	}

	// -> firing (480s hold elapses): Notify succeeds, UpsertState fails.
	clock.advance(480 * time.Second)
	eval.EvaluateOnce(ctx)
	if s, _, _ := repo.GetState(ctx, corenotification.TriggerNodeDown, nodeID, ""); s.State != corenotification.StatePending {
		t.Fatalf("state after the simulated UpsertState failure = %v, want still pending (the failed save must not stick)", s.State)
	}

	// The retry tick: the same transition is recomputed and retried.
	eval.EvaluateOnce(ctx)
	final, found, err := repo.GetState(ctx, corenotification.TriggerNodeDown, nodeID, "")
	if err != nil || !found || final.State != corenotification.StateFiring {
		t.Fatalf("state after the retry = %+v (found=%v, err=%v), want firing", final, found, err)
	}

	page, err := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{NodeID: nodeID})
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(page.Deliveries) != 1 {
		t.Fatalf("deliveries for %s = %d, want exactly 1 (one per channel): the retry's event id must have "+
			"matched the first attempt's for Enqueue to dedupe it", nodeID, len(page.Deliveries))
	}
}
