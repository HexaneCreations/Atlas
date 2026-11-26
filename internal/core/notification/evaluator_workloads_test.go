package notification

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// workloadHarness drives the container and service triggers for node "n1".
// Every tick re-stamps the current samples at the fake clock's now, the way a
// collector run does.
type workloadHarness struct {
	*evalHarness
	t          *testing.T
	containers map[string]fakeContainer
	units      map[string]float64
}

type fakeContainer struct {
	up, restarts, exitCode float64 // exitCode 0: no exit_code series
}

const tickEvery = 30 * time.Second

func newWorkloadHarness(t *testing.T) *workloadHarness {
	t.Helper()
	h := newHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}, "")
	return &workloadHarness{
		evalHarness: h, t: t,
		containers: map[string]fakeContainer{"steady": {up: 1}},
		units:      map[string]float64{"sshd.service": 1},
	}
}

// publish writes the current containers and units as fresh samples.
func (h *workloadHarness) publish() {
	at := h.clock.now()
	var up, restarts, exit, units []MetricSample
	for name, c := range h.containers {
		labels := map[string]string{"container": name, "compose_project": "shop", "compose_service": "svc-" + name}
		up = append(up, MetricSample{NodeID: "n1", Labels: labels, Value: c.up, Time: at})
		restarts = append(restarts, MetricSample{NodeID: "n1", Labels: labels, Value: c.restarts, Time: at})
		if c.exitCode != 0 {
			exit = append(exit, MetricSample{NodeID: "n1", Labels: labels, Value: c.exitCode, Time: at})
		}
	}
	for name, v := range h.units {
		units = append(units, MetricSample{NodeID: "n1", Labels: map[string]string{"unit": name}, Value: v, Time: at})
	}
	h.metrics.set(MetricContainerUp, up)
	h.metrics.set(MetricContainerRestarts, restarts)
	h.metrics.set(MetricContainerExitCode, exit)
	h.metrics.set(MetricServiceUp, units)
}

// tick publishes, evaluates, then moves the clock on by one interval. The
// node itself stays seen.
func (h *workloadHarness) tick() {
	h.publish()
	h.tickStale()
}

// tickStale evaluates without publishing anything new.
func (h *workloadHarness) tickStale() {
	h.nodes.mu.Lock()
	h.nodes.nodes["n1"].LastSeenAt = h.clock.now()
	h.nodes.mu.Unlock()
	h.eval.EvaluateOnce(context.Background())
	h.clock.advance(tickEvery)
}

func (h *workloadHarness) ticks(n int) {
	for range n {
		h.tick()
	}
}

func (h *workloadHarness) state(trigger Trigger, subject string) State {
	s, _, _ := h.states.GetState(context.Background(), trigger, "n1", subject)
	return s
}

func (h *workloadHarness) wantKinds(want ...Kind) {
	h.t.Helper()
	sent := h.notifier.all()
	got := make([]Kind, len(sent))
	for i, n := range sent {
		got[i] = n.Kind
	}
	if len(got) != len(want) {
		h.t.Fatalf("sent %v, want %v (%+v)", got, want, sent)
	}
	for i := range want {
		if got[i] != want[i] {
			h.t.Fatalf("sent %v, want %v", got, want)
		}
	}
}

func (h *workloadHarness) wantState(trigger Trigger, subject string, want StateKind) {
	h.t.Helper()
	if got := h.state(trigger, subject).State; got != want {
		h.t.Fatalf("%s[%s] = %q, want %q", trigger, subject, got, want)
	}
}

// --- container_exit -------------------------------------------------------

func TestContainerExitNonZeroFiresOnceAndRecovers(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.wantKinds()

	h.containers["web"] = fakeContainer{up: 0, exitCode: 137}
	h.tick() // for_seconds 0: fires on the tick that observes it
	h.wantKinds(KindFiring)
	n := h.notifier.all()[0]
	if n.Trigger != TriggerContainerExit || n.Subject != "web" || n.Severity != SeverityCritical {
		t.Errorf("notification = %+v", n)
	}
	if want := "container web (compose project shop, service svc-web) exited with code 137 " +
		"(killed: usually out of memory or a stop timeout)"; n.Message != want {
		t.Errorf("message = %q, want %q (the node is its own field)", n.Message, want)
	}

	h.ticks(10) // still down
	h.wantKinds(KindFiring)

	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.wantState(TriggerContainerExit, "web", StateRecovering)
	h.ticks(3) // 90s of 120s
	h.wantKinds(KindFiring)
	h.tick()
	h.wantKinds(KindFiring, KindRecovered)
	if msg := h.notifier.all()[1].Message; msg != "container web (compose project shop, service svc-web) is running" {
		t.Errorf("recovered message = %q, want it to say the container is running", msg)
	}
	h.wantState(TriggerContainerExit, "web", StateOK)
}

func TestContainerExitRespectsSendRecovery(t *testing.T) {
	h := newWorkloadHarness(t)
	r := h.rules.rules[TriggerContainerExit]
	r.SendRecovery = false
	h.rules.rules[TriggerContainerExit] = r
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.containers["web"] = fakeContainer{up: 0, exitCode: 1}
	h.tick()
	h.containers["web"] = fakeContainer{up: 1}
	h.ticks(6)
	h.wantKinds(KindFiring)
	h.wantState(TriggerContainerExit, "web", StateOK)
}

func TestContainerExitZeroNeverFires(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["job"] = fakeContainer{up: 1}
	h.tick()
	h.containers["job"] = fakeContainer{up: 0}
	h.ticks(10)
	h.wantKinds()
}

// A crash, a restart and a clean stop all inside the staleness window leave
// the crash's exit_code sample fresh while up is 0 for another reason.
func TestContainerExitIgnoresAnExitCodeFromAnEarlierRun(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()

	h.containers["web"] = fakeContainer{up: 0}
	h.publish()
	leftover := MetricSample{NodeID: "n1", Labels: map[string]string{"container": "web"}, Value: 1, Time: h.clock.now().Add(-30 * time.Second)}
	h.metrics.set(MetricContainerExitCode, []MetricSample{leftover})
	h.tickStale()
	h.wantKinds()
}

func TestContainerExitNeverSeenUpNeverFires(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["dead"] = fakeContainer{up: 0, exitCode: 1}
	h.ticks(10)
	h.wantKinds()
	if _, found, _ := h.states.GetState(context.Background(), TriggerContainerExit, "n1", "dead"); found {
		t.Error("a container never seen up has a container_exit row; it has nothing to remember")
	}
}

// An unreadable exit_code metric must not read as "stopped cleanly" and
// recover a firing container.
func TestContainerExitUnreadableExitCodesAreUnknown(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.containers["web"] = fakeContainer{up: 0, exitCode: 1}
	h.tick()
	h.metrics.errs = map[string]error{MetricContainerExitCode: errors.New("db down")}
	h.ticks(10)
	h.wantState(TriggerContainerExit, "web", StateFiring)
	h.wantKinds(KindFiring)
}

// A container recreated under another compose project is a second, briefly
// still-fresh series with the same container name; the newest one wins.
func TestContainerDuplicateSeriesUsesTheNewest(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.publish()
	old := MetricSample{NodeID: "n1", Labels: map[string]string{"container": "web"}, Value: 0, Time: h.clock.now().Add(-20 * time.Second)}
	h.metrics.mu.Lock()
	h.metrics.samples[MetricContainerUp] = append([]MetricSample{old}, h.metrics.samples[MetricContainerUp]...)
	h.metrics.samples[MetricContainerUp] = append(h.metrics.samples[MetricContainerUp], old)
	h.metrics.samples[MetricContainerExitCode] = []MetricSample{{NodeID: "n1", Labels: old.Labels, Value: 1, Time: old.Time}}
	h.metrics.mu.Unlock()
	h.tickStale()
	h.wantKinds()
}

// --- container_restart ----------------------------------------------------

func TestContainerRestartFirstObservationNeverFires(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1, restarts: 40}
	h.ticks(5)
	h.wantKinds()
	if got := h.state(TriggerContainerRestart, "web").Baseline[baselineLastRestarts]; got != 40.0 {
		t.Errorf("last_restarts = %v, want 40", got)
	}
}

func TestContainerRestartIncreaseFiresOnceAndRecovers(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1, restarts: 3}
	h.tick()
	h.containers["web"] = fakeContainer{up: 1, restarts: 5}
	h.tick()
	h.wantKinds(KindFiring)
	n := h.notifier.all()[0]
	if want := "container web (compose project shop, service svc-web) restarted 2 time(s) since the previous observation (5 in total)"; n.Severity != SeverityWarning || n.Message != want {
		t.Errorf("notification = %+v, want message %q", n, want)
	}
	h.tick() // no further increase
	h.wantState(TriggerContainerRestart, "web", StateRecovering)
	h.ticks(4)
	h.wantKinds(KindFiring, KindRecovered)
	h.wantState(TriggerContainerRestart, "web", StateOK)
}

// Every further increase restarts the recovery wait.
func TestContainerRestartFurtherIncreaseDelaysRecovery(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	for i := range 10 { // one restart every other tick
		h.containers["web"] = fakeContainer{up: 1, restarts: float64(i + 1)}
		h.ticks(2)
	}
	h.wantKinds(KindFiring)
}

func TestContainerRestartDecreaseResetsSilently(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1, restarts: 7}
	h.tick()
	h.containers["web"] = fakeContainer{up: 1, restarts: 0} // recreated
	h.ticks(2)
	h.wantKinds()
	h.containers["web"] = fakeContainer{up: 1, restarts: 1}
	h.tick()
	h.wantKinds(KindFiring) // compared with 0, not 7
}

func TestContainerRestartDuringCooldownSendsExactlyOneFlapping(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.containers["web"] = fakeContainer{up: 1, restarts: 1}
	h.ticks(6) // fires, then recovers
	h.wantKinds(KindFiring, KindRecovered)

	h.containers["web"] = fakeContainer{up: 1, restarts: 2} // well inside the 1800s cooldown
	h.tick()
	h.wantKinds(KindFiring, KindRecovered, KindFlapping)
	h.containers["web"] = fakeContainer{up: 1, restarts: 3}
	h.tick()
	h.wantKinds(KindFiring, KindRecovered, KindFlapping)
}

// With a hold configured, the increase must survive the hold.
func TestContainerRestartWithAHoldStillFires(t *testing.T) {
	h := newWorkloadHarness(t)
	r := h.rules.rules[TriggerContainerRestart]
	r.ForSeconds = 60
	h.rules.rules[TriggerContainerRestart] = r
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.containers["web"] = fakeContainer{up: 1, restarts: 1}
	h.ticks(2)
	h.wantKinds()
	h.tick()
	h.wantKinds(KindFiring)
}

// A failed Notify must not consume the increase, and the retry must reuse the
// event id.
func TestContainerRestartFailedNotifyRetriesWithTheSameEventID(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.containers["web"] = fakeContainer{up: 1, restarts: 1}
	h.notifier.err = errors.New("queue down")
	h.tick()
	pending := h.state(TriggerContainerRestart, "web")
	if pending.State != StatePending || pending.Baseline[baselineLastRestarts] != 0.0 {
		t.Fatalf("after a failed Notify: %+v, want pending with the baseline untouched", pending)
	}
	h.notifier.err = nil
	h.tick()
	h.wantKinds(KindFiring)
	want := eventID(TriggerContainerRestart, "n1", "web", *pending.ConditionSince, KindFiring, SeverityWarning)
	if got := h.notifier.all()[0].EventID; got != want {
		t.Errorf("event id = %q, want %q", got, want)
	}
}

// Restarts counted while the rule is disabled or the container excluded are
// not announced when it is enabled again.
func TestContainerRestartBaselineFollowsWhileDisabledOrExcluded(t *testing.T) {
	for name, off := range map[string]func(*Rule){
		"disabled": func(r *Rule) { r.Enabled = false },
		"excluded": func(r *Rule) { r.SubjectExcludes = []string{"web"} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkloadHarness(t)
			on := h.rules.rules[TriggerContainerRestart]
			h.containers["web"] = fakeContainer{up: 1}
			h.tick()
			r := on
			off(&r)
			h.rules.rules[TriggerContainerRestart] = r
			h.containers["web"] = fakeContainer{up: 1, restarts: 9}
			h.ticks(2)
			h.rules.rules[TriggerContainerRestart] = on
			h.ticks(2)
			h.wantKinds()
		})
	}
}

// --- service_failed -------------------------------------------------------

func TestServiceFailedHoldFireAndRecover(t *testing.T) {
	h := newWorkloadHarness(t)
	h.units["nginx.service"] = 1
	h.tick()
	h.units["nginx.service"] = 0
	h.ticks(10) // the 10th is 270s after the first
	h.wantKinds()
	h.wantState(TriggerServiceFailed, "nginx.service", StatePending)
	h.tick() // 300s
	h.wantKinds(KindFiring)
	if n := h.notifier.all()[0]; n.Trigger != TriggerServiceFailed || n.Subject != "nginx.service" || n.Severity != SeverityCritical ||
		n.Message != "service nginx.service is not running (failed or stopped)" {
		t.Errorf("notification = %+v", n)
	}
	h.units["nginx.service"] = 1
	h.ticks(5)
	h.wantKinds(KindFiring, KindRecovered)
	h.wantState(TriggerServiceFailed, "nginx.service", StateOK)
}

func TestServiceFailedShorterThanTheHoldDoesNotFire(t *testing.T) {
	h := newWorkloadHarness(t)
	h.units["nginx.service"] = 1
	h.tick()
	h.units["nginx.service"] = 0
	h.ticks(9)
	h.units["nginx.service"] = 1
	h.tick()
	h.units["nginx.service"] = 0
	h.ticks(9)
	h.wantKinds()
}

func TestServiceNeverUpNeverFires(t *testing.T) {
	h := newWorkloadHarness(t)
	h.units["apache2.service"] = 0
	h.ticks(30)
	h.wantKinds()
	if _, found, _ := h.states.GetState(context.Background(), TriggerServiceFailed, "n1", "apache2.service"); found {
		t.Error("a unit never seen up has a service_failed row")
	}
}

// --- vanished series ------------------------------------------------------

func TestVanishedContainerResetsSilently(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.containers["web"] = fakeContainer{up: 0, exitCode: 1, restarts: 1}
	h.tick()
	h.wantState(TriggerContainerExit, "web", StateFiring)
	h.wantState(TriggerContainerRestart, "web", StateFiring)
	sent := len(h.notifier.all())

	delete(h.containers, "web") // "steady" stays fresh
	h.ticks(4)                  // absent for 90s at the last one: not longer than the window
	h.wantState(TriggerContainerExit, "web", StateFiring)
	h.tick()
	h.wantState(TriggerContainerExit, "web", StateOK)
	h.wantState(TriggerContainerRestart, "web", StateOK)
	h.ticks(10)
	if got := len(h.notifier.all()); got != sent {
		t.Fatalf("sent %d more notifications after the container vanished, want none", got-sent)
	}

	// Back again: no longer absent.
	h.containers["web"] = fakeContainer{up: 1, restarts: 1}
	h.tick()
	if _, ok := h.state(TriggerContainerExit, "web").Baseline[baselineAbsentSince]; ok {
		t.Error("absent_since survived the container's return")
	}
}

func TestVanishedUnitUsesTheServiceWindow(t *testing.T) {
	h := newWorkloadHarness(t)
	h.units["nginx.service"] = 1
	h.tick()
	h.units["nginx.service"] = 0
	h.ticks(11)
	h.wantState(TriggerServiceFailed, "nginx.service", StateFiring)
	delete(h.units, "nginx.service")
	h.ticks(7) // absent for 180s at the last one
	h.wantState(TriggerServiceFailed, "nginx.service", StateFiring)
	h.tick()
	h.wantState(TriggerServiceFailed, "nginx.service", StateOK)
	h.wantKinds(KindFiring)
}

// A short gap (one container missing from a run or two) resets nothing.
func TestBrieflyMissingContainerIsNotReset(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	down := fakeContainer{up: 0, exitCode: 1}
	for range 5 {
		h.containers["web"] = down
		h.tick()
		delete(h.containers, "web")
		h.ticks(3)
	}
	h.wantState(TriggerContainerExit, "web", StateFiring)
	h.wantKinds(KindFiring)
}

func TestWholeFamilyStaleIsUnknown(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.units["nginx.service"] = 1
	h.tick()
	h.containers["web"] = fakeContainer{up: 0, exitCode: 1}
	h.units["nginx.service"] = 0
	h.ticks(11)
	h.wantKinds(KindFiring, KindFiring)
	before := h.states.all()

	for range 40 { // 20 minutes with nothing new; past both windows almost at once
		h.tickStale()
	}
	after := h.states.all()
	if len(after) != len(before) {
		t.Fatalf("rows went from %d to %d while everything was stale", len(before), len(after))
	}
	for k, s := range before {
		if !sameState(s, after[k]) {
			t.Errorf("%v changed while its family was stale: %+v -> %+v", k, s, after[k])
		}
	}
	h.wantKinds(KindFiring, KindFiring)
}

func TestVanishedRowsAreDeletedAfterRetention(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	delete(h.containers, "web")
	h.tick() // absent from here
	h.clock.advance(vanishedRetention - 2*tickEvery)
	h.tick()
	if _, found, _ := h.states.GetState(context.Background(), TriggerContainerRestart, "n1", "web"); !found {
		t.Fatal("row deleted before 24h of absence")
	}
	h.ticks(3)
	for _, trigger := range []Trigger{TriggerContainerExit, TriggerContainerRestart} {
		if _, found, _ := h.states.GetState(context.Background(), trigger, "n1", "web"); found {
			t.Errorf("%s row still there after more than 24h absent", trigger)
		}
		if _, found, _ := h.states.GetState(context.Background(), trigger, "n1", "steady"); !found {
			t.Errorf("%s row of a present container was deleted", trigger)
		}
	}
	h.wantKinds()
}

// --- 9a behaviour ---------------------------------------------------------

func TestNodeDownFiringSuppressesWorkloads(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.units["nginx.service"] = 1
	h.tick()
	seen := h.clock.now()
	_ = h.states.UpsertState(context.Background(), State{
		Trigger: TriggerNodeDown, NodeID: "n1", State: StateFiring, Severity: SeverityCritical, StateSince: seen,
	})
	h.nodes.nodes["n1"].LastSeenAt = seen.Add(-time.Hour)
	before := h.states.all()

	h.containers["web"] = fakeContainer{up: 0, exitCode: 1, restarts: 4}
	h.units["nginx.service"] = 0
	for range 20 {
		h.publish()
		h.eval.EvaluateOnce(context.Background())
		h.clock.advance(tickEvery)
	}
	h.wantKinds()
	for k, s := range h.states.all() {
		if !sameState(before[k], s) {
			t.Errorf("%v changed while node_down was firing", k)
		}
	}
}

func TestWorkloadsSkipLocalArchivedAndMutedNodes(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	h := newHarness(t, []EvalNode{
		{NodeID: "local", LastSeenAt: now},
		{NodeID: "archived", LastSeenAt: now, ArchivedAt: &now},
		{NodeID: "muted", LastSeenAt: now, AlertsMutedUntil: &future},
	}, "local")
	var up, restarts, units []MetricSample
	for _, id := range []string{"local", "archived", "muted"} {
		up = append(up, MetricSample{NodeID: id, Labels: map[string]string{"container": "web"}, Value: 1, Time: now})
		restarts = append(restarts, MetricSample{NodeID: id, Labels: map[string]string{"container": "web"}, Value: 2, Time: now})
		units = append(units, MetricSample{NodeID: id, Labels: map[string]string{"unit": "nginx.service"}, Value: 1, Time: now})
	}
	h.metrics.set(MetricContainerUp, up)
	h.metrics.set(MetricContainerRestarts, restarts)
	h.metrics.set(MetricServiceUp, units)
	h.eval.EvaluateOnce(context.Background())
	if rows := h.states.all(); len(rows) != 0 {
		t.Errorf("rows were written for skipped nodes: %+v", rows)
	}
}

// Restarts that happened while a node was muted are not announced once the
// mute ends; the next one is.
func TestRestartsDuringAMuteAreNotAnnouncedAfterIt(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1}
	h.tick()
	h.nodes.setMuted("n1", h.clock.now().Add(5*tickEvery))
	h.containers["web"] = fakeContainer{up: 1, restarts: 6}
	h.ticks(8)
	h.wantKinds()
	h.containers["web"] = fakeContainer{up: 1, restarts: 7}
	h.tick()
	h.wantKinds(KindFiring)
}

// Steady state costs no writes at all.
func TestUnchangedWorkloadRowsAreNotRewritten(t *testing.T) {
	h := newWorkloadHarness(t)
	h.containers["web"] = fakeContainer{up: 1, restarts: 2}
	h.containers["gone"] = fakeContainer{up: 1}
	h.ticks(2)
	delete(h.containers, "gone")
	h.ticks(2) // records absent_since once
	before := h.states.upserts
	h.ticks(20)
	if got := h.states.upserts - before; got != 0 {
		t.Errorf("%d writes over 20 unchanged ticks, want 0", got)
	}
}

func TestWorkloadsWithoutAnIntervalAreNotEvaluated(t *testing.T) {
	h := newWorkloadHarness(t)
	h.eval.containerInterval, h.eval.serviceInterval = 0, 0
	h.ticks(3)
	if rows := h.states.all(); len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
}

func TestExitCodeText(t *testing.T) {
	for code, want := range map[int]string{137: "killed", 143: "SIGTERM", 2: "2"} {
		if got := exitCodeText(code); !strings.Contains(got, want) {
			t.Errorf("exitCodeText(%d) = %q, want it to mention %q", code, got, want)
		}
	}
}
