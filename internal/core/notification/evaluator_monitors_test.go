package notification

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMonitorSource struct {
	mu       sync.Mutex
	monitors map[string]*EvalMonitor
}

func (f *fakeMonitorSource) ListEvalMonitors(context.Context) ([]EvalMonitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []EvalMonitor{}
	for _, m := range f.monitors {
		out = append(out, *m)
	}
	return out, nil
}

func (f *fakeMonitorSource) update(id string, change func(*EvalMonitor)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.monitors[id])
}

// fakeMonitorStates is the monitor half of the fake store; states live in
// the shared fakeStateStore.
type fakeMonitorStates struct {
	*fakeStateStore
	source *fakeMonitorSource
}

func (f fakeMonitorStates) GetMonitorState(_ context.Context, trigger Trigger, monitorID, subject string) (State, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.states[stateKey{string(trigger), MonitorScope(monitorID), subject}]
	return s, ok, nil
}

func (f fakeMonitorStates) ListActiveMonitorStates(context.Context) ([]State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []State
	for _, s := range f.states {
		if s.MonitorID != "" && s.State != StateOK {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f fakeMonitorStates) ResetMonitorStates(_ context.Context, monitorID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, s := range f.states {
		if s.MonitorID == monitorID && s.State != StateOK {
			s.State, s.Severity, s.Value, s.ConditionSince, s.FlapCount, s.FlapWindowStart, s.Baseline =
				StateOK, "", nil, nil, 0, nil, nil
			f.states[k] = s
		}
	}
	return nil
}

func (f fakeMonitorStates) EndMonitorMuteAfterExpiry(ctx context.Context, monitorID string, now time.Time) (bool, error) {
	cleared := false
	f.source.update(monitorID, func(m *EvalMonitor) {
		if m.AlertsMutedUntil == nil || !m.AlertsMutedUntil.After(now) {
			m.AlertsMutedUntil, cleared = nil, true
		}
	})
	if !cleared {
		return false, nil
	}
	return true, f.ResetMonitorStates(ctx, monitorID)
}

type monitorHarness struct {
	*evalHarness
	source  *fakeMonitorSource
	offline bool
}

func monitorRules() map[Trigger]Rule {
	rules := defaultRules()
	rules[TriggerWebsiteDown] = Rule{Trigger: TriggerWebsiteDown, Enabled: true, CooldownSeconds: 1800, SendRecovery: true}
	rules[TriggerWebsiteCertExpiry] = Rule{Trigger: TriggerWebsiteCertExpiry, Enabled: true, CooldownSeconds: 1800, SendRecovery: true}
	return rules
}

func newMonitorHarness(t *testing.T, nodes []EvalNode, monitors ...EvalMonitor) *monitorHarness {
	t.Helper()
	h := &monitorHarness{evalHarness: newHarness(t, nodes, ""), source: &fakeMonitorSource{monitors: map[string]*EvalMonitor{}}}
	h.rules.rules = monitorRules()
	for i := range monitors {
		m := monitors[i]
		h.source.monitors[m.MonitorID] = &m
	}
	h.eval = NewEvaluator(EvaluatorOptions{
		Rules: h.rules, States: h.states, Nodes: h.nodes, Metrics: h.metrics, Notifier: h.notifier,
		Now: h.clock.now, Monitors: h.source, MonitorStates: fakeMonitorStates{fakeStateStore: h.states, source: h.source},
		ProbeOffline:               func() bool { return h.offline },
		ContainerCollectorInterval: 30 * time.Second, ServiceCollectorInterval: 60 * time.Second,
	})
	return h
}

func upMonitor(id string) EvalMonitor {
	return EvalMonitor{MonitorID: id, Active: true, Status: "up", CertWarnDays: 14, CertCritDays: 3, CertNotify: true}
}

func (h *monitorHarness) markDown(id string, code int) time.Time {
	at := h.clock.now()
	h.source.update(id, func(m *EvalMonitor) {
		m.Status, m.StatusSince, m.DownSince, m.LastFailure = "down", at, &at, "http_status"
		m.LastStatusCode, m.LastResponseMS = &code, ptrInt(120)
	})
	return at
}

func (h *monitorHarness) markUp(id string) {
	at := h.clock.now()
	h.source.update(id, func(m *EvalMonitor) {
		m.Status, m.StatusSince, m.DownSince, m.LastFailure = "up", at, nil, ""
		m.LastStatusCode, m.LastResponseMS = ptrInt(200), ptrInt(80)
	})
}

func ptrInt(v int) *int { return &v }

func (h *monitorHarness) tick() []Notification {
	before := len(h.notifier.all())
	h.eval.EvaluateOnce(context.Background())
	return h.notifier.all()[before:]
}

func (h *monitorHarness) state(trigger Trigger, id string) (State, bool) {
	s, ok := h.states.all()[stateKey{string(trigger), MonitorScope(id), ""}]
	return s, ok
}

func TestMonitorDownFiresOnlyOnceTheProberMarksItDown(t *testing.T) {
	h := newMonitorHarness(t, nil, upMonitor("mon1"))
	h.source.update("mon1", func(m *EvalMonitor) { m.Status, m.LastFailure = "pending", "timeout" })
	if got := h.tick(); len(got) != 0 {
		t.Fatalf("pending (retries not exhausted) sent %+v", got)
	}

	h.clock.advance(30 * time.Second)
	downAt := h.markDown("mon1", 503)
	got := h.tick()
	if len(got) != 1 {
		t.Fatalf("down sent %d notifications, want 1 on the same tick", len(got))
	}
	n := got[0]
	want := fmt.Sprintf("notif-website_down-m:mon1--%d-firing-critical", downAt.Unix())
	if n.EventID != want || n.Kind != KindFiring || n.Severity != SeverityCritical || n.MonitorID != "mon1" || n.NodeID != "" {
		t.Fatalf("notification = %+v, want event id %s", n, want)
	}
	d := n.Details
	if d.Failure != "http_status" || d.StatusCode == nil || *d.StatusCode != 503 || d.ResponseMS == nil ||
		d.DownSince == nil || !d.DownSince.Equal(downAt) {
		t.Errorf("details = %+v", d)
	}
	if strings.Contains(n.Title+n.Message, "mon1") {
		t.Errorf("the stored text names the monitor: %q %q", n.Title, n.Message)
	}

	h.clock.advance(30 * time.Second)
	if got := h.tick(); len(got) != 0 {
		t.Errorf("a monitor still down re-sent %+v", got)
	}
}

func TestMonitorRecoveryCarriesTheOutageDuration(t *testing.T) {
	h := newMonitorHarness(t, nil, upMonitor("mon1"))
	downAt := h.markDown("mon1", 500)
	h.tick()
	h.clock.advance(7 * time.Minute)
	h.markUp("mon1")
	got := h.tick()
	if len(got) != 1 || got[0].Kind != KindRecovered || got[0].Severity != SeverityCritical {
		t.Fatalf("recovery = %+v", got)
	}
	d := got[0].Details
	if d.OutageSeconds == nil || *d.OutageSeconds != 420 || d.DownSince == nil || !d.DownSince.Equal(downAt) ||
		d.StatusCode == nil || *d.StatusCode != 200 || d.Failure != "" {
		t.Errorf("recovery details = %+v", d)
	}
	if s, _ := h.state(TriggerWebsiteDown, "mon1"); s.State != StateOK || len(s.Baseline) != 0 {
		t.Errorf("state after recovery = %+v", s)
	}
	h.clock.advance(30 * time.Second)
	if got := h.tick(); len(got) != 0 {
		t.Errorf("an up monitor sent %+v", got)
	}
}

func TestMonitorDownAgainWithinCooldownIsFlapping(t *testing.T) {
	h := newMonitorHarness(t, nil, upMonitor("mon1"))
	h.markDown("mon1", 502)
	h.tick()
	h.clock.advance(time.Minute)
	h.markUp("mon1")
	h.tick()
	h.clock.advance(time.Minute)
	h.markDown("mon1", 502)
	got := h.tick()
	if len(got) != 1 || got[0].Kind != KindFlapping {
		t.Fatalf("second down within cooldown = %+v, want one flapping", got)
	}
	if !strings.Contains(got[0].Message, "cooldown") {
		t.Errorf("flapping message = %q", got[0].Message)
	}
	h.clock.advance(30 * time.Second)
	if got := h.tick(); len(got) != 0 {
		t.Errorf("flapping repeated: %+v", got)
	}
}

func TestPausedMutedAndRemovedMonitorsResetSilently(t *testing.T) {
	h := newMonitorHarness(t, nil, upMonitor("paused"), upMonitor("muted"), upMonitor("gone"))
	for _, id := range []string{"paused", "muted", "gone"} {
		h.markDown(id, 503)
	}
	if got := h.tick(); len(got) != 3 {
		t.Fatalf("setup fired %d, want 3", len(got))
	}

	h.clock.advance(time.Minute)
	h.source.update("paused", func(m *EvalMonitor) { m.Active = false })
	until := h.clock.now().Add(time.Hour)
	h.source.update("muted", func(m *EvalMonitor) { m.AlertsMutedUntil = &until })
	h.source.mu.Lock()
	delete(h.source.monitors, "gone")
	h.source.mu.Unlock()
	for _, id := range []string{"paused", "muted"} {
		h.markUp(id)
	}
	if got := h.tick(); len(got) != 0 {
		t.Fatalf("paused, muted or removed monitors sent %+v", got)
	}
	if s, _ := h.state(TriggerWebsiteDown, "paused"); s.State != StateOK {
		t.Errorf("paused monitor state = %s, want a silent reset to ok", s.State)
	}
	if s, _ := h.state(TriggerWebsiteDown, "muted"); s.State != StateFiring {
		t.Errorf("muted monitor state = %s, want frozen while muted", s.State)
	}

	// The mute ends: its states reset silently, and it is evaluated afresh.
	h.clock.advance(2 * time.Hour)
	if got := h.tick(); len(got) != 0 {
		t.Fatalf("an expired mute sent %+v", got)
	}
	if s, _ := h.state(TriggerWebsiteDown, "muted"); s.State != StateOK {
		t.Errorf("state after the mute ended = %s", s.State)
	}
	h.markDown("muted", 503)
	if got := h.tick(); len(got) != 1 || got[0].MonitorID != "muted" {
		t.Errorf("an unmuted monitor going down sent %+v", got)
	}
}

func TestProbeOfflineNeverFiresOrRecoversWebsiteDown(t *testing.T) {
	h := newMonitorHarness(t, nil, upMonitor("mon1"), upMonitor("mon2"))
	h.markDown("mon1", 503)
	h.tick()

	h.offline = true
	h.clock.advance(time.Minute)
	h.markDown("mon2", 0)
	h.markUp("mon1")
	if got := h.tick(); len(got) != 0 {
		t.Fatalf("probe offline sent %+v", got)
	}
	if s, _ := h.state(TriggerWebsiteDown, "mon1"); s.State != StateFiring {
		t.Errorf("mon1 = %s, want still firing while the probe is offline", s.State)
	}
	if _, found := h.state(TriggerWebsiteDown, "mon2"); found {
		t.Error("mon2 got a state while the probe was offline")
	}

	h.offline = false
	h.clock.advance(time.Minute)
	got := h.tick()
	kinds := map[string]Kind{}
	for _, n := range got {
		kinds[n.MonitorID] = n.Kind
	}
	if kinds["mon1"] != KindRecovered || kinds["mon2"] != KindFiring {
		t.Errorf("after the probe came back = %v", kinds)
	}
}

func TestDisabledMonitorRuleResetsSilentlyAndDoesNotFire(t *testing.T) {
	h := newMonitorHarness(t, nil, upMonitor("mon1"))
	h.markDown("mon1", 503)
	h.tick()

	no := false
	h.rules.mu.Lock()
	h.rules.overrides = append(h.rules.overrides, RuleSpec{Trigger: TriggerWebsiteDown, MonitorID: "mon1", Enabled: &no})
	h.rules.mu.Unlock()
	h.clock.advance(time.Minute)
	h.markUp("mon1")
	if got := h.tick(); len(got) != 0 {
		t.Fatalf("a disabled rule sent %+v", got)
	}
	if s, _ := h.state(TriggerWebsiteDown, "mon1"); s.State != StateOK {
		t.Errorf("state = %s, want reset", s.State)
	}
	h.markDown("mon1", 503)
	if got := h.tick(); len(got) != 0 {
		t.Errorf("a disabled rule fired: %+v", got)
	}
}

func TestCertExpiryWarnsEscalatesAndRecoversOnRenewal(t *testing.T) {
	h := newMonitorHarness(t, nil, upMonitor("mon1"))
	expires := h.clock.now().Add(20 * 24 * time.Hour)
	h.source.update("mon1", func(m *EvalMonitor) { m.CertExpiresAt = &expires })
	if got := h.tick(); len(got) != 0 {
		t.Fatalf("20 days left sent %+v", got)
	}

	h.clock.advance(7 * 24 * time.Hour) // 13 days left: inside the 14-day warning
	got := h.tick()
	if len(got) != 1 || got[0].Kind != KindFiring || got[0].Severity != SeverityWarning {
		t.Fatalf("warning crossing = %+v", got)
	}
	if d := got[0].Details; d.CertDaysLeft == nil || *d.CertDaysLeft != 13 || d.CertExpiresAt == nil || !d.CertExpiresAt.Equal(expires) {
		t.Errorf("cert details = %+v", d)
	}
	h.clock.advance(24 * time.Hour)
	if again := h.tick(); len(again) != 0 {
		t.Errorf("a second day in the warning window re-sent %+v", again)
	}

	h.clock.advance(10 * 24 * time.Hour) // 2 days left: inside the 3-day critical
	got = h.tick()
	if len(got) != 1 || got[0].Kind != KindEscalated || got[0].Severity != SeverityCritical {
		t.Fatalf("critical crossing = %+v", got)
	}
	h.clock.advance(time.Hour)
	if again := h.tick(); len(again) != 0 {
		t.Errorf("critical re-sent %+v", again)
	}

	renewed := h.clock.now().Add(90 * 24 * time.Hour)
	h.source.update("mon1", func(m *EvalMonitor) { m.CertExpiresAt = &renewed })
	got = h.tick()
	if len(got) != 1 || got[0].Kind != KindRecovered || !strings.Contains(got[0].Message, "renewed") {
		t.Fatalf("renewal = %+v", got)
	}
	if d := got[0].Details; d.CertExpiresAt == nil || !d.CertExpiresAt.Equal(renewed) {
		t.Errorf("renewal details = %+v", d)
	}
}

func TestCertExpiryIgnoredWithoutCertNotify(t *testing.T) {
	h := newMonitorHarness(t, nil, upMonitor("mon1"))
	soon := h.clock.now().Add(24 * time.Hour)
	h.source.update("mon1", func(m *EvalMonitor) { m.CertExpiresAt, m.CertNotify = &soon, false })
	if got := h.tick(); len(got) != 0 {
		t.Errorf("cert_notify off sent %+v", got)
	}
}

func TestMonitorsLeaveNodeAlertsAndTheirEventIDsAlone(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newMonitorHarness(t, []EvalNode{{NodeID: "n1", LastSeenAt: start.Add(-time.Hour)}}, upMonitor("mon1"))
	h.markDown("mon1", 503)
	first := h.tick()
	if len(first) != 1 || first[0].MonitorID != "mon1" {
		t.Fatalf("first tick = %+v", first)
	}
	s, ok := h.states.all()[stateKey{string(TriggerNodeDown), "n1", ""}]
	if !ok || s.State != StatePending || s.MonitorID != "" {
		t.Fatalf("node state = %+v", s)
	}
	h.clock.advance(9 * time.Minute)
	got := h.tick()
	if len(got) != 1 || got[0].NodeID != "n1" || got[0].MonitorID != "" {
		t.Fatalf("node_down = %+v", got)
	}
	if want := fmt.Sprintf("notif-node_down-n1--%d-firing-critical", start.Unix()); got[0].EventID != want {
		t.Errorf("node event id = %s, want %s", got[0].EventID, want)
	}
}
