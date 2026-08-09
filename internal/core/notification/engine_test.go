package notification

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeChannelStore struct {
	mu       sync.Mutex
	channels map[string]Channel
}

func newFakeChannelStore(channels ...Channel) *fakeChannelStore {
	m := map[string]Channel{}
	for _, c := range channels {
		m[c.ID] = c
	}
	return &fakeChannelStore{channels: m}
}

func (f *fakeChannelStore) ListChannels(context.Context) ([]Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Channel, 0, len(f.channels))
	for _, c := range f.channels {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeChannelStore) GetChannel(_ context.Context, id string) (Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.channels[id]
	if !ok {
		return Channel{}, errors.New("not found")
	}
	return c, nil
}

func (f *fakeChannelStore) CreateChannel(_ context.Context, c Channel) (Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[c.ID] = c
	return c, nil
}
func (f *fakeChannelStore) UpdateChannel(_ context.Context, c Channel) (Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[c.ID] = c
	return c, nil
}
func (f *fakeChannelStore) DeleteChannel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.channels, id)
	return nil
}

type fakeDeliveryStore struct {
	mu         sync.Mutex
	deliveries map[string]*Delivery
	seen       map[[2]string]bool // (eventID, channelID) dedup
	seq        int
}

func newFakeDeliveryStore() *fakeDeliveryStore {
	return &fakeDeliveryStore{deliveries: map[string]*Delivery{}, seen: map[[2]string]bool{}}
}

func (f *fakeDeliveryStore) Enqueue(_ context.Context, event Event, channels []Channel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range channels {
		key := [2]string{event.ID, c.ID}
		if f.seen[key] {
			continue
		}
		f.seen[key] = true
		f.seq++
		id := "delivery-" + string(rune('a'+f.seq))
		f.deliveries[id] = &Delivery{
			ID: id, EventID: event.ID, ChannelID: c.ID, Trigger: event.Trigger,
			NodeID: event.NodeID, Severity: event.Severity, Title: event.Title, Message: event.Message,
			EventTime: event.Time, Status: StatusPending, NextAttemptAt: time.Now(),
		}
	}
	return nil
}

func (f *fakeDeliveryStore) DueDeliveries(_ context.Context, now time.Time, limit int) ([]Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Delivery
	for _, d := range f.deliveries {
		if d.Status == StatusPending && !d.NextAttemptAt.After(now) {
			out = append(out, *d)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeDeliveryStore) MarkDelivered(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.deliveries[id]
	d.Status, d.UpdatedAt = StatusDelivered, at
	return nil
}

func (f *fakeDeliveryStore) MarkRetry(_ context.Context, id string, attempts int, next time.Time, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.deliveries[id]
	d.Attempts, d.NextAttemptAt, d.LastError = attempts, next, lastErr
	return nil
}

func (f *fakeDeliveryStore) MarkExhausted(_ context.Context, id string, attempts int, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.deliveries[id]
	d.Status, d.Attempts, d.LastError = StatusFailed, attempts, lastErr
	return nil
}

func (f *fakeDeliveryStore) ListDeliveries(context.Context, DeliveryFilter) ([]Delivery, error) {
	return nil, nil
}

func (f *fakeDeliveryStore) get(id string) Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.deliveries[id]
}

func (f *fakeDeliveryStore) all() []Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Delivery, 0, len(f.deliveries))
	for _, d := range f.deliveries {
		out = append(out, *d)
	}
	return out
}

type fakeSender struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeSender) Send(context.Context, Channel, Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func testChannel(id string) Channel {
	return Channel{ID: id, Name: id, Type: ChannelWebhook, Enabled: true, Webhook: WebhookConfig{URL: "http://example.invalid"}}
}

func TestNotifyEnqueuesOnlyForEnabledChannels(t *testing.T) {
	channels := newFakeChannelStore(testChannel("c1"), Channel{ID: "c2", Name: "c2", Type: ChannelWebhook, Enabled: false, Webhook: WebhookConfig{URL: "x"}})
	deliveries := newFakeDeliveryStore()
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{}})

	if err := e.Notify(context.Background(), Event{ID: "evt-1", Trigger: TriggerAlertTransition}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	all := deliveries.all()
	if len(all) != 1 {
		t.Fatalf("expected 1 delivery (only the enabled channel), got %d", len(all))
	}
	if all[0].ChannelID != "c1" {
		t.Errorf("delivery channel = %q, want c1", all[0].ChannelID)
	}
}

func TestNotifyIsIdempotentPerEventAndChannel(t *testing.T) {
	channels := newFakeChannelStore(testChannel("c1"))
	deliveries := newFakeDeliveryStore()
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries})

	ctx := context.Background()
	if err := e.Notify(ctx, Event{ID: "evt-1"}); err != nil {
		t.Fatalf("notify 1: %v", err)
	}
	if err := e.Notify(ctx, Event{ID: "evt-1"}); err != nil {
		t.Fatalf("notify 2: %v", err)
	}
	if len(deliveries.all()) != 1 {
		t.Fatalf("expected exactly 1 delivery after two Notify calls for the same event, got %d", len(deliveries.all()))
	}
}

func TestDispatchDueDeliversAndMarksDelivered(t *testing.T) {
	channels := newFakeChannelStore(testChannel("c1"))
	deliveries := newFakeDeliveryStore()
	sender := &fakeSender{}
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{ChannelWebhook: sender}})

	ctx := context.Background()
	if err := e.Notify(ctx, Event{ID: "evt-1"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	e.dispatchDue(ctx)

	all := deliveries.all()
	if len(all) != 1 || all[0].Status != StatusDelivered {
		t.Fatalf("expected the delivery marked delivered, got %+v", all)
	}
	if sender.calls != 1 {
		t.Errorf("sender called %d times, want 1", sender.calls)
	}
}

func TestDispatchDueRetriesOnFailureUntilExhausted(t *testing.T) {
	channels := newFakeChannelStore(testChannel("c1"))
	deliveries := newFakeDeliveryStore()
	sender := &fakeSender{err: errors.New("boom")}
	e := NewEngine(Options{
		Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{ChannelWebhook: sender},
		MaxAttempts: 3, BackoffBase: time.Millisecond, BackoffMax: time.Millisecond,
	})

	ctx := context.Background()
	if err := e.Notify(ctx, Event{ID: "evt-1"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	var id string
	for _, d := range deliveries.all() {
		id = d.ID
	}

	// Force each retry due immediately regardless of jittered backoff.
	forceDue := func() {
		deliveries.mu.Lock()
		deliveries.deliveries[id].NextAttemptAt = time.Now().Add(-time.Second)
		deliveries.mu.Unlock()
	}

	forceDue()
	e.dispatchDue(ctx)
	if got := deliveries.get(id); got.Status != StatusPending || got.Attempts != 1 {
		t.Fatalf("after attempt 1: %+v, want pending/attempts=1", got)
	}

	forceDue()
	e.dispatchDue(ctx)
	if got := deliveries.get(id); got.Status != StatusPending || got.Attempts != 2 {
		t.Fatalf("after attempt 2: %+v, want pending/attempts=2", got)
	}

	forceDue()
	e.dispatchDue(ctx)
	got := deliveries.get(id)
	if got.Status != StatusFailed {
		t.Fatalf("after attempt 3 (max): %+v, want failed", got)
	}
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", got.Attempts)
	}
	if got.LastError == "" {
		t.Error("expected last_error to be recorded")
	}
	if sender.calls != 3 {
		t.Errorf("sender called %d times, want 3", sender.calls)
	}
}

func TestDispatchDueFailsImmediatelyWithNoSenderForChannelType(t *testing.T) {
	channels := newFakeChannelStore(testChannel("c1"))
	deliveries := newFakeDeliveryStore()
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{}})

	ctx := context.Background()
	if err := e.Notify(ctx, Event{ID: "evt-1"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	e.dispatchDue(ctx)

	all := deliveries.all()
	if len(all) != 1 || all[0].Status != StatusFailed {
		t.Fatalf("expected immediate failure with no sender registered, got %+v", all)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	e := NewEngine(Options{BackoffBase: time.Second, BackoffMax: 8 * time.Second})
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 8 * time.Second}, // capped
	}
	for _, c := range cases {
		if got := e.backoff(c.attempt); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestJitterStaysWithinBound(t *testing.T) {
	for range 100 {
		got := jitter(10 * time.Millisecond)
		if got < 0 || got > 10*time.Millisecond {
			t.Fatalf("jitter out of bounds: %v", got)
		}
	}
}
