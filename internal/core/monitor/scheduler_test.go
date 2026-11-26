package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestOutageDetectorCorrelates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	d := NewOutageDetector(nil)
	ok := Result{Up: true}
	dns := Result{Failure: FailureDNS}
	status := Result{Failure: FailureHTTPStatus}

	for i, h := range []string{"a", "b", "c", "d"} {
		d.Observe(ctx, h, ok, now.Add(time.Duration(i)*time.Second))
	}
	// Two network failures of four hosts: below three hosts.
	d.Observe(ctx, "a", dns, now.Add(5*time.Second))
	if off, _ := d.Observe(ctx, "b", dns, now.Add(6*time.Second)); off {
		t.Fatal("two hosts must not be an outage")
	}
	// A site answering with 503 is the site, not the probe.
	if off, _ := d.Observe(ctx, "c", status, now.Add(7*time.Second)); off {
		t.Fatal("http_status failures must not count")
	}
	off, changed := d.Observe(ctx, "c", dns, now.Add(8*time.Second))
	if !off || !changed || !d.State().Offline || !d.State().Since.Equal(now.Add(8*time.Second)) {
		t.Fatalf("three of four hosts failing at the network level: off=%v changed=%v %+v", off, changed, d.State())
	}
	if _, changed := d.Observe(ctx, "d", dns, now.Add(9*time.Second)); changed {
		t.Error("staying offline is not a change")
	}
	// Recovery: successes bring it under the threshold.
	d.Observe(ctx, "a", ok, now.Add(10*time.Second))
	off, changed = d.Observe(ctx, "b", ok, now.Add(11*time.Second))
	if off || !changed || d.State().Offline {
		t.Fatalf("recovered: off=%v changed=%v", off, changed)
	}
	// Old observations age out of the window.
	d2 := NewOutageDetector(nil)
	for _, h := range []string{"a", "b"} {
		d2.Observe(ctx, h, dns, now)
	}
	if off, _ := d2.Observe(ctx, "c", dns, now.Add(outageWindow+time.Second)); off {
		t.Error("failures outside the window must not count")
	}
}

func TestOutageDetectorCanaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	alive, dials := false, 0
	d := NewOutageDetector([]string{"canary:443"})
	d.dial = func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if alive {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}
		return nil, errors.New("unreachable")
	}
	// One host is enough with canaries: they decide.
	if off, _ := d.Observe(ctx, "only", Result{Failure: FailureConnect}, now); !off {
		t.Fatal("dead canaries and a network failure: offline")
	}
	// Cached: no redial within the window.
	d.Observe(ctx, "only", Result{Failure: FailureConnect}, now.Add(time.Second))
	if dials != 1 {
		t.Errorf("dials = %d, want 1 (cached)", dials)
	}
	mu.Lock()
	alive = true
	mu.Unlock()
	if off, changed := d.Observe(ctx, "only", Result{Up: true}, now.Add(canaryCacheWindow)); off || !changed {
		t.Fatal("canaries answering again: online")
	}
	// Online, a failure with live canaries is the site's.
	if off, _ := d.Observe(ctx, "only", Result{Failure: FailureTimeout}, now.Add(2*canaryCacheWindow)); off {
		t.Error("live canaries: not offline")
	}
}

// fakeStore is an in-memory [Store].
type fakeStore struct {
	mu       sync.Mutex
	due      []Due
	records  []CheckRecord
	resched  map[string]time.Time
	spread   int
	pruned   []time.Time
	claimLim []int
}

func (f *fakeStore) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.due)
}

func (f *fakeStore) SpreadOverdue(context.Context, time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spread++
	return 0, nil
}

func (f *fakeStore) ClaimDue(_ context.Context, limit int, _ time.Duration) ([]Due, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimLim = append(f.claimLim, limit)
	n := min(limit, len(f.due))
	out := f.due[:n]
	f.due = f.due[n:]
	return out, nil
}

func (f *fakeStore) RecordCheck(_ context.Context, rec CheckRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	return nil
}

func (f *fakeStore) Reschedule(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resched == nil {
		f.resched = map[string]time.Time{}
	}
	f.resched[id] = at
	return nil
}

func (f *fakeStore) PruneEvents(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned = append(f.pruned, before)
	return 0, nil
}

// fakeChecker returns results by monitor id and tracks concurrency.
type fakeChecker struct {
	mu       sync.Mutex
	results  map[string]Result
	inFlight int
	peak     int
	hold     time.Duration
	secrets  map[string]Secret
}

func (c *fakeChecker) Check(_ context.Context, m Monitor, s Secret) Result {
	c.mu.Lock()
	c.inFlight++
	c.peak = max(c.peak, c.inFlight)
	if c.secrets == nil {
		c.secrets = map[string]Secret{}
	}
	c.secrets[m.ID] = s
	r := c.results[m.ID]
	c.mu.Unlock()
	time.Sleep(c.hold)
	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	return r
}

func dueMonitor(id, host string) Due {
	m := Defaults()
	m.ID, m.Name, m.URL = id, id, "https://"+host+"/"
	return Due{Monitor: m, Counters: Counters{Status: StatusUp}}
}

func TestSchedulerBoundsConcurrencyAndRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	checker := &fakeChecker{results: map[string]Result{}, hold: 20 * time.Millisecond}
	for i := range 10 {
		id := fmt.Sprintf("m%d", i)
		store.due = append(store.due, dueMonitor(id, id+".example"))
		checker.results[id] = Result{Up: true, StatusCode: 200}
	}
	checker.results["m3"] = Result{Failure: FailureHTTPStatus, StatusCode: 500}
	s := NewScheduler(SchedulerOptions{Store: store, Checker: checker, MaxConcurrent: 3, Now: func() time.Time { return now }})

	ctx := context.Background()
	for store.pending() > 0 {
		s.RunDue(ctx)
		time.Sleep(5 * time.Millisecond)
	}
	s.Wait()
	if checker.peak > 3 {
		t.Errorf("peak concurrency = %d, want <= 3", checker.peak)
	}
	for _, l := range store.claimLim {
		if l > 3 {
			t.Errorf("claimed %d at once with 3 slots", l)
		}
	}
	if len(store.records) != 10 {
		t.Fatalf("records = %d", len(store.records))
	}
	for _, rec := range store.records {
		switch rec.MonitorID {
		case "m3":
			if rec.Counters.Status != StatusPending || rec.Heartbeat != StatusPending || !rec.NextCheckAt.Equal(now.Add(time.Minute)) {
				t.Errorf("m3: %+v", rec)
			}
		default:
			if rec.Counters.Status != StatusUp || rec.Heartbeat != StatusUp || rec.Event != nil {
				t.Errorf("%s: %+v", rec.MonitorID, rec)
			}
		}
	}
}

func TestSchedulerDoesNotMarkEverythingDownWhenOffline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	checker := &fakeChecker{results: map[string]Result{}}
	for i := range 6 {
		id := fmt.Sprintf("m%d", i)
		d := dueMonitor(id, id+".example")
		d.Monitor.Retries = 0 // the first failure would be down
		store.due = append(store.due, d)
		checker.results[id] = Result{Failure: FailureConnect}
	}
	s := NewScheduler(SchedulerOptions{Store: store, Checker: checker, MaxConcurrent: 1, Now: func() time.Time { return now }})
	for range 6 {
		s.RunDue(context.Background())
		s.Wait()
	}
	if len(store.records) != 6 {
		t.Fatalf("records = %d", len(store.records))
	}
	down := 0
	for _, rec := range store.records {
		if rec.Counters.Status == StatusDown {
			down++
		}
	}
	// The first two (fewer than three hosts) count; from the third the
	// probe is offline and the rest are unknown with frozen counters.
	if down != 2 {
		t.Errorf("monitors marked down = %d, want 2", down)
	}
	for _, rec := range store.records[2:] {
		if rec.Heartbeat != StatusUnknown || rec.Event != nil || rec.Counters.Status != StatusUp {
			t.Errorf("%s during the outage: %+v", rec.MonitorID, rec)
		}
	}
	if !s.ProbeState().Offline {
		t.Error("probe state must report offline")
	}
}

func TestSchedulerSkipsAnUnreadableSecret(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	d := dueMonitor("m1", "a.example")
	d.SecretUnreadable = true
	store := &fakeStore{due: []Due{d}}
	checker := &fakeChecker{results: map[string]Result{}}
	s := NewScheduler(SchedulerOptions{Store: store, Checker: checker, Now: func() time.Time { return now }})
	s.RunDue(context.Background())
	s.Wait()
	if len(store.records) != 0 || len(checker.secrets) != 0 {
		t.Error("a check with an unreadable secret must not run or record")
	}
	if !store.resched["m1"].Equal(now.Add(time.Minute)) {
		t.Errorf("rescheduled at %v", store.resched["m1"])
	}
}

func TestSchedulerRunSpreadsPrunesAndStops(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	s := NewScheduler(SchedulerOptions{Store: store, Checker: &fakeChecker{}, Poll: 5 * time.Millisecond, Now: func() time.Time { return now }})
	go s.Run(context.Background())
	time.Sleep(30 * time.Millisecond)
	s.Stop()
	s.Stop() // idempotent
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.spread != 1 {
		t.Errorf("SpreadOverdue calls = %d, want 1", store.spread)
	}
	if len(store.pruned) != 1 || !store.pruned[0].Equal(now.Add(-365*24*time.Hour)) {
		t.Errorf("pruned = %v", store.pruned)
	}
	if len(store.claimLim) == 0 {
		t.Error("Run never polled")
	}
	NewScheduler(SchedulerOptions{Store: store}).Stop() // never started: returns
}
