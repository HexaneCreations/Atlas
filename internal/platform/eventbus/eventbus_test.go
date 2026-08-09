package eventbus_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/eventbus"
)

func recv(t *testing.T, sub *eventbus.Subscription) eventbus.Event {
	t.Helper()
	select {
	case e, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription channel closed unexpectedly")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return eventbus.Event{}
	}
}

func TestPatternMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern eventbus.Pattern
		topic   eventbus.Topic
		want    bool
	}{
		{"**", "docker.container.started", true},
		{"**", "system", true},
		{"docker.**", "docker.container.started", true},
		{"docker.**", "docker", true},
		{"docker.**", "system.disk.full", false},
		{"docker.container.*", "docker.container.started", true},
		{"docker.container.*", "docker.container.health.changed", false},
		{"docker.container", "docker.container.started", false},
		{"*.*.failed", "collector.run.failed", true},
		{"*.*.failed", "collector.run.succeeded", false},
		{"docker.container.started", "docker.container.started", true},
		{"docker.container.started", "docker.container.stopped", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.pattern)+"~"+string(tt.topic), func(t *testing.T) {
			t.Parallel()
			if got := tt.pattern.Matches(tt.topic); got != tt.want {
				t.Errorf("Pattern(%q).Matches(%q) = %v, want %v", tt.pattern, tt.topic, got, tt.want)
			}
		})
	}
}

func TestPatternAndTopicValidation(t *testing.T) {
	t.Parallel()

	invalidPatterns := []eventbus.Pattern{"", "docker..started", "docker.**.started", "dock*er.x"}
	for _, p := range invalidPatterns {
		if p.Valid() {
			t.Errorf("Pattern(%q).Valid() = true, want false", p)
		}
	}
	for _, p := range []eventbus.Pattern{"**", "docker.**", "*.*.failed", "a.b.c"} {
		if !p.Valid() {
			t.Errorf("Pattern(%q).Valid() = false, want true", p)
		}
	}

	// Wildcards are a subscription concept; a published topic must be concrete.
	for _, tp := range []eventbus.Topic{"", "docker..x", "docker.*", "**"} {
		if tp.Valid() {
			t.Errorf("Topic(%q).Valid() = true, want false", tp)
		}
	}
}

func TestPublishReachesMatchingSubscribersOnly(t *testing.T) {
	t.Parallel()

	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	defer bus.Close()

	docker, err := bus.Subscribe("docker-watcher", "docker.**")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()

	system, err := bus.Subscribe("system-watcher", "system.**")
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()

	bus.Publish(context.Background(), eventbus.Event{
		Topic:   "docker.container.started",
		Source:  "plugin.docker",
		Subject: "abc123",
	})

	got := recv(t, docker)
	if got.Subject != "abc123" {
		t.Errorf("Subject = %q, want abc123", got.Subject)
	}
	if got.ID == "" {
		t.Error("bus did not assign an event ID")
	}
	if got.Time.IsZero() {
		t.Error("bus did not assign an event timestamp")
	}

	select {
	case e := <-system.C:
		t.Errorf("non-matching subscriber received %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// The defining property of this bus: one wedged subscriber must not stall a
// publisher. If this test can hang, the design is wrong.
func TestSlowSubscriberNeverBlocksPublisher(t *testing.T) {
	t.Parallel()

	const buffer, publishes = 4, 1000

	bus := eventbus.New(eventbus.Options{BufferSize: buffer})
	defer bus.Close()

	stalled, err := bus.Subscribe("stalled", eventbus.MatchAll)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range publishes {
			bus.Publish(context.Background(), eventbus.Event{Topic: "test.event.fired"})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads")
	}

	stats := bus.Stats()
	if stats.Published != publishes {
		t.Errorf("Published = %d, want %d", stats.Published, publishes)
	}
	if stats.Delivered != buffer {
		t.Errorf("Delivered = %d, want %d (the buffer, then drops)", stats.Delivered, buffer)
	}
	if want := uint64(publishes - buffer); stats.Dropped != want {
		t.Errorf("Dropped = %d, want %d", stats.Dropped, want)
	}
	if stalled.Dropped() != uint64(publishes-buffer) {
		t.Errorf("Subscription.Dropped() = %d, want %d", stalled.Dropped(), publishes-buffer)
	}
}

// A slow subscriber must degrade alone, not take healthy ones down with it.
func TestOneSlowSubscriberDoesNotAffectAnother(t *testing.T) {
	t.Parallel()

	bus := eventbus.New(eventbus.Options{BufferSize: 2})
	defer bus.Close()

	slow, err := bus.Subscribe("slow", eventbus.MatchAll)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()

	fast, err := bus.Subscribe("fast", eventbus.MatchAll)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()

	const n = 50
	received := make(chan int, 1)
	go func() {
		count := 0
		for range fast.C {
			count++
			if count == n {
				break
			}
		}
		received <- count
	}()

	for range n {
		bus.Publish(context.Background(), eventbus.Event{Topic: "test.event.fired"})
		// Let the fast consumer drain, so its queue never fills.
		time.Sleep(time.Millisecond)
	}

	select {
	case got := <-received:
		if got != n {
			t.Errorf("fast subscriber received %d events, want %d", got, n)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("fast subscriber starved; it got only %d of %d", n-int(slow.Dropped()), n)
	}

	if slow.Dropped() == 0 {
		t.Error("expected the slow subscriber to have dropped events")
	}
}

func TestCloseIsIdempotentAndClosesChannel(t *testing.T) {
	t.Parallel()

	bus := eventbus.New(eventbus.Options{BufferSize: 1})
	defer bus.Close()

	sub, err := bus.Subscribe("s", eventbus.MatchAll)
	if err != nil {
		t.Fatal(err)
	}

	sub.Close()
	sub.Close() // must not panic on a double close

	if _, ok := <-sub.C; ok {
		t.Error("channel should be closed after Close")
	}
	if got := bus.Stats().Subscribers; got != 0 {
		t.Errorf("Subscribers = %d after Close, want 0", got)
	}

	// Publishing to a bus with no subscribers must be harmless.
	bus.Publish(context.Background(), eventbus.Event{Topic: "test.event.fired"})
}

func TestBusCloseClosesAllSubscriptions(t *testing.T) {
	t.Parallel()

	bus := eventbus.New(eventbus.Options{BufferSize: 1})
	subs := make([]*eventbus.Subscription, 3)
	for i := range subs {
		s, err := bus.Subscribe("s", eventbus.MatchAll)
		if err != nil {
			t.Fatal(err)
		}
		subs[i] = s
	}

	if err := bus.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	for i, s := range subs {
		if _, ok := <-s.C; ok {
			t.Errorf("subscription %d channel still open after bus.Close", i)
		}
		s.Close() // per-subscription Close after bus Close must be safe
	}

	if _, err := bus.Subscribe("late", eventbus.MatchAll); err == nil {
		t.Error("Subscribe on a closed bus should fail")
	}
	bus.Publish(context.Background(), eventbus.Event{Topic: "test.event.fired"})
}

func TestInvalidTopicIsDiscarded(t *testing.T) {
	t.Parallel()

	bus := eventbus.New(eventbus.Options{BufferSize: 4})
	defer bus.Close()

	sub, err := bus.Subscribe("s", eventbus.MatchAll)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	bus.Publish(context.Background(), eventbus.Event{Topic: "docker..started"})

	select {
	case e := <-sub.C:
		t.Errorf("invalid topic was delivered: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	if got := bus.Stats().Published; got != 0 {
		t.Errorf("Published = %d, want 0", got)
	}
}

func TestSubscribeRejectsInvalidPattern(t *testing.T) {
	t.Parallel()

	bus := eventbus.New(eventbus.Options{BufferSize: 1})
	defer bus.Close()

	if _, err := bus.Subscribe("s", "docker.**.started"); err == nil {
		t.Error("Subscribe accepted a pattern with a non-terminal **")
	}
}

// Run with -race: concurrent publish, subscribe, and close must be safe.
func TestConcurrentPublishSubscribeClose(t *testing.T) {
	t.Parallel()

	bus := eventbus.New(eventbus.Options{BufferSize: 16})
	defer bus.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				bus.Publish(ctx, eventbus.Event{Topic: "test.event.fired"})
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				sub, err := bus.Subscribe("churn", eventbus.MatchAll)
				if err != nil {
					return
				}
				select {
				case <-sub.C:
				default:
				}
				sub.Close()
			}
		}()
	}
	wg.Wait()

	if bus.Stats().Published == 0 {
		t.Error("no events were published during the concurrency run")
	}
}
