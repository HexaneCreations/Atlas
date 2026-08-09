// Package eventbus provides Atlas's in-process publish/subscribe bus.
//
// The bus is the seam that makes the platform event-driven. Plugins observe
// their domain and publish; the API layer, the incident timeline, and the
// WebSocket fan-out subscribe. No publisher knows who is listening, so a new
// consumer is added without touching the code that produces the data.
//
// # Delivery guarantees
//
// Delivery is at-most-once, in-process, and lossy under back-pressure. Each
// subscriber gets a bounded queue; when that queue is full, the event is
// dropped for that subscriber alone and a counter is incremented.
//
// That trade is deliberate, and it is the single most important property of
// this package. A monitoring system's failure mode must never be to slow down
// the thing it monitors. If publishers blocked on a slow subscriber, a stalled
// WebSocket client would back-pressure into the collector scheduler, which
// would delay metric collection, which would make Atlas report a healthy host
// as unresponsive — the observer becoming the outage. Dropping observations
// for one lagging consumer, visibly and countably, is strictly better.
//
// Consumers that genuinely cannot lose events must not rely on the bus alone.
// They persist from a durable subscriber and reconcile on restart; the bus
// tells them something changed, the database remains the source of truth.
package eventbus

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
)

// dropLogInterval rate-limits the warning emitted when a subscriber is
// falling behind. Without it, the condition that produces drops — a flood of
// events — would also produce a flood of log lines about them.
const dropLogInterval = 30 * time.Second

// Bus is an in-process event bus. The zero value is not usable; call [New].
//
// A Bus is safe for concurrent use by any number of publishers and
// subscribers.
type Bus struct {
	bufferSize int
	logger     *slog.Logger
	now        func() time.Time

	mu     sync.RWMutex
	subs   map[string]*Subscription
	closed bool

	published atomic.Uint64
	delivered atomic.Uint64
	dropped   atomic.Uint64
}

// Options configures a [Bus].
type Options struct {
	// BufferSize is the queue depth given to each subscriber. Larger buffers
	// absorb longer stalls at the cost of memory per subscriber.
	BufferSize int
	// Logger receives back-pressure warnings. Defaults to a discarding logger.
	Logger *slog.Logger
	// Now is injected by tests that need deterministic timestamps.
	Now func() time.Time
}

// New builds a Bus.
func New(opts Options) *Bus {
	if opts.BufferSize < 1 {
		opts.BufferSize = 1
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Bus{
		bufferSize: opts.BufferSize,
		logger:     opts.Logger,
		now:        opts.Now,
		subs:       make(map[string]*Subscription),
	}
}

// Subscription is a registered consumer of events.
//
// Read from [Subscription.C] until it is closed. Always call
// [Subscription.Close] when finished — typically with defer, immediately
// after subscribing — or the bus will keep queueing events for a consumer
// that no longer exists.
type Subscription struct {
	// C delivers matching events. The bus closes it when the subscription or
	// the bus itself is closed, so a range over C terminates cleanly.
	C <-chan Event

	id      string
	name    string
	pattern Pattern
	ch      chan Event
	bus     *Bus

	closeOnce sync.Once
	dropped   atomic.Uint64
	lastWarn  atomic.Int64 // unix nanos of the last back-pressure warning
}

// Name returns the human-readable name given at subscription time.
func (s *Subscription) Name() string { return s.name }

// Pattern returns the topic pattern this subscription matches.
func (s *Subscription) Pattern() Pattern { return s.pattern }

// Dropped returns how many events this subscriber has missed because its
// queue was full. A non-zero and rising value means the consumer is too slow
// for its event rate: make it faster, give it a larger buffer, or narrow its
// pattern.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Close unsubscribes and closes [Subscription.C]. It is idempotent and safe
// to call concurrently with publishes.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.bus.remove(s)
	})
}

// Subscribe registers a consumer of every event matching pattern.
//
// name identifies the subscriber in logs and metrics; make it descriptive
// enough to act on ("api.websocket-fanout", not "sub1").
//
// It returns an error for an invalid pattern or a closed bus. Subscribing is
// a startup-time operation, so a caller that controls its own pattern may
// treat failure as fatal.
func (b *Bus) Subscribe(name string, pattern Pattern) (*Subscription, error) {
	if !pattern.Valid() {
		return nil, errs.New(errs.CodeInvalidArgument, "invalid topic pattern %q", pattern).
			WithOp("eventbus.Bus.Subscribe")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errs.New(errs.CodeUnavailable, "event bus is closed").
			WithOp("eventbus.Bus.Subscribe")
	}

	ch := make(chan Event, b.bufferSize)
	sub := &Subscription{
		C:       ch,
		id:      id.New(),
		name:    name,
		pattern: pattern,
		ch:      ch,
		bus:     b,
	}
	b.subs[sub.id] = sub
	return sub, nil
}

// Publish delivers an event to every matching subscriber.
//
// It never blocks and never returns an error, because there is nothing a
// publisher could usefully do about a slow subscriber: it cannot wait (that
// is the failure mode this design exists to prevent) and it cannot retry
// (the queue would still be full). Back-pressure is surfaced through
// [Stats] and through the warning logged here, which is where an operator
// can actually act on it.
//
// ID and Time are assigned when empty. An invalid topic is dropped with a
// warning rather than delivered, since subscribers match on topic and a
// malformed one would silently reach nobody.
func (b *Bus) Publish(ctx context.Context, e Event) {
	if !e.Topic.Valid() {
		b.logger.WarnContext(ctx, "discarding event with invalid topic",
			slog.String("topic", string(e.Topic)), slog.String("source", e.Source))
		return
	}
	if e.ID == "" {
		e.ID = id.New()
	}
	if e.Time.IsZero() {
		e.Time = b.now()
	}
	b.published.Add(1)

	// The read lock is held only for non-blocking sends, so publishers never
	// serialise behind a subscriber and Close cannot race a send.
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}

	for _, sub := range b.subs {
		if !sub.pattern.Matches(e.Topic) {
			continue
		}
		select {
		case sub.ch <- e:
			b.delivered.Add(1)
		default:
			b.dropped.Add(1)
			sub.dropped.Add(1)
			b.warnDropped(ctx, sub, e)
		}
	}
}

func (b *Bus) warnDropped(ctx context.Context, sub *Subscription, e Event) {
	now := b.now()
	last := sub.lastWarn.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < dropLogInterval {
		return
	}
	if !sub.lastWarn.CompareAndSwap(last, now.UnixNano()) {
		return // another publisher is emitting the warning
	}
	b.logger.WarnContext(ctx, "event dropped: subscriber queue is full",
		slog.String("subscriber", sub.name),
		slog.String("pattern", string(sub.pattern)),
		slog.String("topic", string(e.Topic)),
		slog.Uint64("dropped_total", sub.dropped.Load()),
		slog.Int("buffer_size", b.bufferSize),
	)
}

func (b *Bus) remove(sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[sub.id]; !ok {
		return // already removed, e.g. by Close
	}
	delete(b.subs, sub.id)
	// Safe under the write lock: no publisher can be inside a send.
	close(sub.ch)
}

// Close shuts the bus down and closes every subscriber channel, which ends
// any range loop over [Subscription.C]. Publishes after Close are discarded.
// Close is idempotent.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for _, sub := range b.subs {
		close(sub.ch)
	}
	clear(b.subs)
	return nil
}

// Stats is a point-in-time snapshot of bus activity.
type Stats struct {
	// Subscribers is the number of open subscriptions.
	Subscribers int `json:"subscribers"`
	// Published counts events accepted by the bus.
	Published uint64 `json:"published"`
	// Delivered counts successful enqueues; one event delivered to three
	// subscribers counts three times.
	Delivered uint64 `json:"delivered"`
	// Dropped counts enqueues refused because a subscriber's queue was full.
	// This is the health signal for the bus: it should be zero.
	Dropped uint64 `json:"dropped"`
}

// Stats returns a snapshot of bus activity.
//
// The counters are read independently, so a snapshot taken during heavy
// publishing may not be perfectly self-consistent. They are intended for
// trend and alerting, where that skew is immaterial.
func (b *Bus) Stats() Stats {
	b.mu.RLock()
	n := len(b.subs)
	b.mu.RUnlock()

	return Stats{
		Subscribers: n,
		Published:   b.published.Load(),
		Delivered:   b.delivered.Load(),
		Dropped:     b.dropped.Load(),
	}
}
