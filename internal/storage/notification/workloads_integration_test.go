//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/storage/metric"
	"github.com/hexane/atlas/internal/storage/notification"
	"github.com/jackc/pgx/v5/pgxpool"
)

const workloadNode = "workload-node"

// workloadFixture evaluates the container and service triggers against the
// real rule seeds, state rows and metric_latest. Evaluator clocks are fake;
// sample times are real, because LatestForMetric compares them with now().
type workloadFixture struct {
	t     *testing.T
	repo  *notification.Repository
	pool  *pgxpool.Pool
	clock *evaluatorFakeClock
	eval  *corenotification.Evaluator
}

func newWorkloadFixture(t *testing.T) *workloadFixture {
	t.Helper()
	repo, pool := newRepository(t)
	seedNode(t, pool, workloadNode)
	secret := "fake-secret"
	if _, err := repo.CreateChannel(context.Background(), channelInput("workloads", &secret)); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	f := &workloadFixture{t: t, repo: repo, pool: pool, clock: newEvaluatorFakeClock(time.Now())}
	f.eval = f.evaluator(f.clock.now)
	return f
}

func (f *workloadFixture) evaluator(now func() time.Time) *corenotification.Evaluator {
	metricRepo := metric.NewRepository(f.pool)
	return corenotification.NewEvaluator(corenotification.EvaluatorOptions{
		Rules: f.repo, States: f.repo, Now: now,
		Notifier:                   corenotification.NewEngine(corenotification.Options{Channels: f.repo, Deliveries: f.repo}),
		Nodes:                      evalNodeAdapter{repo: metricRepo},
		Metrics:                    evalMetricAdapter{repo: metricRepo},
		ContainerCollectorInterval: 30 * time.Second, ServiceCollectorInterval: 60 * time.Second,
	})
}

func (f *workloadFixture) sample(name string, labels map[string]string, value float64, at time.Time) {
	f.t.Helper()
	raw, _ := json.Marshal(labels)
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO metric_latest (node_id, metric, collector_id, series_key, labels, unit, kind, value, time)
		VALUES ($1, $2, 'test', $3::text, $3::text::jsonb, 'count', 'gauge', $4, $5)
		ON CONFLICT (node_id, metric, collector_id, series_key) DO UPDATE SET value = EXCLUDED.value, time = EXCLUDED.time`,
		workloadNode, name, string(raw), value, at); err != nil {
		f.t.Fatalf("write sample %s: %v", name, err)
	}
}

// container writes one collector run's samples for a container.
func (f *workloadFixture) container(name string, up, restarts, exitCode float64) {
	f.t.Helper()
	f.containerAt(name, up, restarts, exitCode, time.Now())
}

func (f *workloadFixture) containerAt(name string, up, restarts, exitCode float64, at time.Time) {
	f.t.Helper()
	labels := map[string]string{"container": name}
	f.sample(corenotification.MetricContainerUp, labels, up, at)
	f.sample(corenotification.MetricContainerRestarts, labels, restarts, at)
	if exitCode != 0 {
		f.sample(corenotification.MetricContainerExitCode, labels, exitCode, at)
	}
}

func (f *workloadFixture) unit(name string, up float64) {
	f.t.Helper()
	f.sample(corenotification.MetricServiceUp, map[string]string{"unit": name}, up, time.Now())
}

// evaluate keeps the node seen on the fake clock, then runs one tick.
func (f *workloadFixture) evaluate() {
	f.t.Helper()
	f.seen()
	f.eval.EvaluateOnce(context.Background())
}

func (f *workloadFixture) seen() {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE nodes SET last_seen_at = $2 WHERE node_id = $1`, workloadNode, f.clock.now()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *workloadFixture) state(trigger corenotification.Trigger, subject string) (corenotification.State, bool) {
	f.t.Helper()
	s, found, err := f.repo.GetState(context.Background(), trigger, workloadNode, subject)
	if err != nil {
		f.t.Fatal(err)
	}
	return s, found
}

func (f *workloadFixture) wantState(trigger corenotification.Trigger, subject string, want corenotification.StateKind) {
	f.t.Helper()
	if s, _ := f.state(trigger, subject); s.State != want {
		f.t.Fatalf("%s[%s] = %q, want %q", trigger, subject, s.State, want)
	}
}

// deliveries returns every queued delivery's kind for trigger, newest first.
func (f *workloadFixture) deliveries(trigger corenotification.Trigger) []corenotification.Kind {
	f.t.Helper()
	page, err := f.repo.ListDeliveries(context.Background(), corenotification.DeliveryFilter{NodeID: workloadNode, Trigger: trigger, Limit: 200})
	if err != nil {
		f.t.Fatal(err)
	}
	var kinds []corenotification.Kind
	for _, d := range page.Deliveries {
		kinds = append(kinds, d.Kind)
	}
	return kinds
}

func (f *workloadFixture) wantDeliveries(trigger corenotification.Trigger, want ...corenotification.Kind) {
	f.t.Helper()
	if got := f.deliveries(trigger); !reflect.DeepEqual(got, want) {
		f.t.Fatalf("%s deliveries (newest first) = %v, want %v", trigger, got, want)
	}
}

func TestNotificationContainerExitFullCycle(t *testing.T) {
	f := newWorkloadFixture(t)
	f.container("web", 1, 0, 0)
	f.container("never-up", 0, 0, 1)
	f.evaluate()
	f.container("web", 0, 0, 137)
	f.evaluate()
	f.clock.advance(30 * time.Second)
	f.evaluate()
	f.wantDeliveries(corenotification.TriggerContainerExit, corenotification.KindFiring)

	f.container("web", 1, 0, 0) // the exit_code row is still there, and still fresh
	f.evaluate()
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateRecovering)
	f.clock.advance(120 * time.Second)
	f.evaluate()
	f.wantDeliveries(corenotification.TriggerContainerExit, corenotification.KindRecovered, corenotification.KindFiring)
	if _, found := f.state(corenotification.TriggerContainerExit, "never-up"); found {
		t.Error("a container never seen up has a row")
	}

	// A clean stop right after: up 0, exit_code row fresh but from the crash.
	f.container("web", 0, 0, 0)
	f.evaluate()
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateOK)
}

func TestNotificationServiceFailedFullCycle(t *testing.T) {
	f := newWorkloadFixture(t)
	f.unit("nginx.service", 1)
	f.unit("apache2.service", 0)
	f.evaluate()
	f.unit("nginx.service", 0)
	f.evaluate()
	f.clock.advance(299 * time.Second)
	f.evaluate()
	f.wantDeliveries(corenotification.TriggerServiceFailed)
	f.clock.advance(time.Second)
	f.evaluate()
	f.wantDeliveries(corenotification.TriggerServiceFailed, corenotification.KindFiring)
	f.unit("nginx.service", 1)
	f.evaluate()
	f.clock.advance(120 * time.Second)
	f.evaluate()
	f.wantDeliveries(corenotification.TriggerServiceFailed, corenotification.KindRecovered, corenotification.KindFiring)
}

// The baseline survives the real jsonb column: seen_up and last_restarts are
// read back by a fresh evaluator (a restarted server) and compared, and an
// unchanged tick writes nothing.
func TestNotificationBaselineRoundTripsThroughTheRepository(t *testing.T) {
	f := newWorkloadFixture(t)
	f.container("web", 1, 12, 0)
	f.container("idle", 0, 0, 0)
	f.evaluate()

	exit, _ := f.state(corenotification.TriggerContainerExit, "web")
	if !reflect.DeepEqual(exit.Baseline, map[string]any{"seen_up": true}) {
		t.Errorf("container_exit baseline = %#v", exit.Baseline)
	}
	restart, _ := f.state(corenotification.TriggerContainerRestart, "web")
	if !reflect.DeepEqual(restart.Baseline, map[string]any{"last_restarts": float64(12)}) {
		t.Errorf("container_restart baseline = %#v", restart.Baseline)
	}

	f.clock.advance(30 * time.Second)
	f.evaluate()
	if again, _ := f.state(corenotification.TriggerContainerRestart, "web"); !again.UpdatedAt.Equal(restart.UpdatedAt) {
		t.Error("an unchanged row was written again")
	}

	f.eval = f.evaluator(f.clock.now) // a server restart
	f.evaluate()
	f.wantDeliveries(corenotification.TriggerContainerRestart)

	f.container("web", 1, 14, 0)
	f.evaluate()
	f.wantDeliveries(corenotification.TriggerContainerRestart, corenotification.KindFiring)
	if s, _ := f.state(corenotification.TriggerContainerRestart, "web"); s.Baseline["last_restarts"] != float64(14) {
		t.Errorf("last_restarts = %v after firing, want 14", s.Baseline["last_restarts"])
	}
}

// Two evaluators (two server processes) with different clocks tick at the
// same moment over the same rows: every event is queued once.
func TestNotificationTwoEvaluatorsDoNotDoubleFire(t *testing.T) {
	f := newWorkloadFixture(t)
	other := f.evaluator(func() time.Time { return f.clock.now().Add(7*time.Second + 123*time.Nanosecond) })

	const n = 25
	for i := range n {
		f.container(fmt.Sprintf("c%02d", i), 1, 0, 0)
	}
	f.evaluate()
	for i := range n {
		f.container(fmt.Sprintf("c%02d", i), 0, 1, 1)
	}

	for range 3 {
		f.seen()
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, e := range []*corenotification.Evaluator{f.eval, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				e.EvaluateOnce(context.Background())
			}()
		}
		close(start)
		wg.Wait()
		f.clock.advance(30 * time.Second)
	}

	for _, trigger := range []corenotification.Trigger{corenotification.TriggerContainerExit, corenotification.TriggerContainerRestart} {
		if got := len(f.deliveries(trigger)); got != n {
			t.Errorf("%s: %d deliveries, want exactly %d (one per container)", trigger, got, n)
		}
	}
}

func TestNotificationBeginPendingKeepsTheFirstRecord(t *testing.T) {
	f := newWorkloadFixture(t)
	ctx := context.Background()
	first := time.Now().UTC().Truncate(time.Microsecond)
	second := first.Add(5 * time.Second)
	pending := func(since time.Time) corenotification.State {
		return corenotification.State{
			Trigger: corenotification.TriggerContainerExit, NodeID: workloadNode, Subject: "web",
			State: corenotification.StatePending, StateSince: since, ConditionSince: &since,
		}
	}
	for _, since := range []time.Time{first, second} {
		got, err := f.repo.BeginPending(ctx, pending(since))
		if err != nil {
			t.Fatal(err)
		}
		if got.State != corenotification.StatePending || !got.ConditionSince.Equal(first) {
			t.Fatalf("BeginPending(%v) = %+v, want the first record's condition_since %v", since, got, first)
		}
	}
}

func TestNotificationVanishedContainer(t *testing.T) {
	f := newWorkloadFixture(t)
	f.container("steady", 1, 0, 0)
	f.container("web", 1, 0, 0)
	f.evaluate()
	f.container("web", 0, 0, 1)
	f.evaluate()
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateFiring)

	// Removed: web's series goes stale while steady's stays fresh.
	f.containerAt("web", 0, 0, 1, time.Now().Add(-10*time.Minute))
	f.evaluate()
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateFiring)
	f.clock.advance(91 * time.Second)
	f.evaluate()
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateOK)
	f.clock.advance(24 * time.Hour)
	f.evaluate()
	for _, trigger := range []corenotification.Trigger{corenotification.TriggerContainerExit, corenotification.TriggerContainerRestart} {
		if _, found := f.state(trigger, "web"); found {
			t.Errorf("%s row survived more than 24h of absence", trigger)
		}
		if _, found := f.state(trigger, "steady"); !found {
			t.Errorf("%s row of a present container was deleted", trigger)
		}
	}
	f.wantDeliveries(corenotification.TriggerContainerExit, corenotification.KindFiring)
}

func TestNotificationWholeFamilyStaleIsUnknown(t *testing.T) {
	f := newWorkloadFixture(t)
	f.container("steady", 1, 0, 0)
	f.container("web", 1, 0, 0)
	f.evaluate()
	f.container("web", 0, 0, 1)
	f.evaluate()
	before, _ := f.state(corenotification.TriggerContainerExit, "web")

	if _, err := f.pool.Exec(context.Background(), `UPDATE metric_latest SET time = time - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		f.clock.advance(12 * time.Hour)
		f.evaluate()
	}
	after, found := f.state(corenotification.TriggerContainerExit, "web")
	if !found || !reflect.DeepEqual(before, after) {
		t.Errorf("row changed while its whole family was stale: %+v -> %+v (found %v)", before, after, found)
	}
	f.wantDeliveries(corenotification.TriggerContainerExit, corenotification.KindFiring)
}

// Ending a mute, either way, forgets last_restarts but not seen_up.
func TestNotificationEndingAMuteRebaselinesRestarts(t *testing.T) {
	for name, end := range map[string]func(context.Context, *notification.Repository) (bool, error){
		"expiry": func(ctx context.Context, r *notification.Repository) (bool, error) {
			return r.EndMuteAfterExpiry(ctx, workloadNode, time.Now())
		},
		"explicit": func(ctx context.Context, r *notification.Repository) (bool, error) {
			return r.EndMuteExplicitly(ctx, workloadNode)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkloadFixture(t)
			ctx := context.Background()
			f.container("web", 1, 3, 0)
			f.evaluate()
			if _, err := f.repo.MuteNode(ctx, workloadNode, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			f.container("web", 1, 8, 0) // during the mute
			if ok, err := end(ctx, f.repo); err != nil || !ok {
				t.Fatalf("end mute: %v %v", ok, err)
			}
			if s, _ := f.state(corenotification.TriggerContainerExit, "web"); s.Baseline["seen_up"] != true {
				t.Errorf("container_exit baseline = %v, want seen_up kept", s.Baseline)
			}
			f.evaluate()
			f.wantDeliveries(corenotification.TriggerContainerRestart)
			f.container("web", 1, 9, 0)
			f.evaluate()
			f.wantDeliveries(corenotification.TriggerContainerRestart, corenotification.KindFiring)
		})
	}
}
