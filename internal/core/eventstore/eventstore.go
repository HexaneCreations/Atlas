// Package eventstore durably persists events from the event bus.
//
// The bus is deliberately lossy under back-pressure (see
// internal/platform/eventbus): a monitoring system must never slow down the
// thing it monitors. Alerting, incident timelines, and correlation cannot
// build on a feed that may silently drop, so this package is the durable
// subscriber the bus's own documentation names — it persists and reconciles,
// the bus stays the source of "something changed", the database stays the
// source of truth.
package eventstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

// Record is one durably stored event.
type Record struct {
	ID         string
	Time       time.Time
	NodeID     string
	Topic      string
	Source     string
	Subject    string
	Payload    json.RawMessage
	ReceivedAt time.Time
}

// Filter selects records for a query. The zero value matches everything,
// newest first, bounded by a default limit.
type Filter struct {
	NodeID string
	Topic  string
	Since  time.Time
	Until  time.Time
	// Before is a pagination cursor: only records strictly older than this
	// are returned.
	Before time.Time
	Limit  int
}

// Store persists event records. Satisfied by
// [github.com/hexane/atlas/internal/storage/eventstore.Repository].
type Store interface {
	Insert(ctx context.Context, rec Record) error
}

// Tap wraps a Store so fn observes every record that is successfully
// inserted — local host events (via [Writer]) and fleet events forwarded
// through the agent transport alike, since both paths write through a Store.
//
// This is how the alert rule engine sees event-kind rules fire fleet-wide
// without either write path knowing alerting exists.
func Tap(store Store, fn func(context.Context, Record)) Store {
	return tappedStore{store: store, fn: fn}
}

type tappedStore struct {
	store Store
	fn    func(context.Context, Record)
}

func (t tappedStore) Insert(ctx context.Context, rec Record) error {
	if err := t.store.Insert(ctx, rec); err != nil {
		return err
	}
	if t.fn != nil {
		t.fn(ctx, rec)
	}
	return nil
}

// Writer subscribes to a local event bus and durably persists every event,
// attributed to one node.
type Writer struct {
	bus    *eventbus.Bus
	store  Store
	nodeID string
	logger *slog.Logger

	mu   sync.Mutex
	sub  *eventbus.Subscription
	done chan struct{}
	wg   sync.WaitGroup
}

// Options configures a [Writer].
type Options struct {
	Bus    *eventbus.Bus
	Store  Store
	NodeID string
	Logger *slog.Logger
}

// New builds a Writer. It does not subscribe until Start.
func New(opts Options) *Writer {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Writer{bus: opts.Bus, store: opts.Store, nodeID: opts.NodeID, logger: opts.Logger}
}

// Name implements [lifecycle.Component].
func (w *Writer) Name() string { return "eventstore.writer" }

// Start subscribes to the bus and begins persisting.
func (w *Writer) Start(context.Context) error {
	const op = "eventstore.Writer.Start"

	if w.bus == nil || w.store == nil {
		return errs.New(errs.CodeInvalidArgument, "eventstore writer requires a bus and a store").WithOp(op)
	}

	sub, err := w.bus.Subscribe("core.eventstore", eventbus.MatchAll)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not subscribe to the event bus").WithOp(op)
	}

	w.mu.Lock()
	w.sub = sub
	w.done = make(chan struct{})
	w.mu.Unlock()

	w.wg.Add(1)
	go w.drain(sub, w.done)
	return nil
}

// Stop unsubscribes and waits for the drain to finish.
func (w *Writer) Stop(context.Context) error {
	w.mu.Lock()
	sub, done := w.sub, w.done
	w.sub, w.done = nil, nil
	w.mu.Unlock()

	if sub == nil {
		return nil
	}
	close(done)
	sub.Close()
	w.wg.Wait()
	return nil
}

func (w *Writer) drain(sub *eventbus.Subscription, done <-chan struct{}) {
	defer w.wg.Done()
	for {
		select {
		case <-done:
			return
		case event, open := <-sub.C:
			if !open {
				return
			}
			w.write(event)
		}
	}
}

func (w *Writer) write(e eventbus.Event) {
	payload, err := json.Marshal(e.Payload)
	if err != nil || payload == nil {
		payload = []byte("{}")
	}

	rec := Record{
		ID: e.ID, Time: e.Time, NodeID: w.nodeID, Topic: string(e.Topic),
		Source: e.Source, Subject: e.Subject, Payload: payload, ReceivedAt: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.store.Insert(ctx, rec); err != nil {
		w.logger.ErrorContext(ctx, "could not persist event",
			slog.String("topic", string(e.Topic)), slog.String("error", err.Error()))
	}
}

// Dropped reports how many events the bus discarded for this writer because
// it could not keep up. Non-zero means the durable log has gaps.
func (w *Writer) Dropped() uint64 {
	w.mu.Lock()
	sub := w.sub
	w.mu.Unlock()
	if sub == nil {
		return 0
	}
	return sub.Dropped()
}
