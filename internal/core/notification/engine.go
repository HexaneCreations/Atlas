package notification

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	// DefaultMaxAttempts bounds retries: the fifth failure gives up.
	DefaultMaxAttempts = 5
	// DefaultBackoffBase and DefaultBackoffMax bound the exponential
	// backoff between attempts — the same doubling-with-ceiling shape
	// [github.com/hexane/atlas/internal/core/transport/remote.Transport]'s
	// replay loop uses, applied to a retried delivery row instead of a
	// retried HTTP POST.
	DefaultBackoffBase = 10 * time.Second
	DefaultBackoffMax  = 10 * time.Minute
	// DefaultDispatchInterval is how often Run checks for due deliveries.
	DefaultDispatchInterval = 5 * time.Second
	// DefaultBatchSize bounds how many deliveries one dispatch tick claims.
	DefaultBatchSize = 50
)

// Engine enqueues events and, on its own schedule, delivers what is due.
type Engine struct {
	channels   ChannelStore
	deliveries DeliveryStore
	senders    map[ChannelType]Sender
	logger     *slog.Logger

	maxAttempts      int
	backoffBase      time.Duration
	backoffMax       time.Duration
	dispatchInterval time.Duration
	batchSize        int

	mu     sync.Mutex
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options configures an [Engine].
type Options struct {
	Channels   ChannelStore
	Deliveries DeliveryStore
	// Senders maps a channel type to what delivers it. A type with no
	// registered sender fails its deliveries immediately with that fact as
	// the error, rather than retrying something that can never succeed.
	Senders map[ChannelType]Sender
	Logger  *slog.Logger

	MaxAttempts      int
	BackoffBase      time.Duration
	BackoffMax       time.Duration
	DispatchInterval time.Duration
	BatchSize        int
}

// NewEngine builds an Engine. It does not dispatch anything until Run.
func NewEngine(opts Options) *Engine {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = DefaultBackoffBase
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = DefaultBackoffMax
	}
	if opts.DispatchInterval <= 0 {
		opts.DispatchInterval = DefaultDispatchInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	return &Engine{
		channels: opts.Channels, deliveries: opts.Deliveries, senders: opts.Senders, logger: opts.Logger,
		maxAttempts: opts.MaxAttempts, backoffBase: opts.BackoffBase, backoffMax: opts.BackoffMax,
		dispatchInterval: opts.DispatchInterval, batchSize: opts.BatchSize,
	}
}

// Notify enqueues event for delivery to every enabled channel.
//
// This does one durable write and returns — no network call happens on
// this path. That is what keeps a caller (the alert engine's OnTransition
// hook) from blocking on, or failing because of, notification delivery.
func (e *Engine) Notify(ctx context.Context, event Event) error {
	channels, err := e.channels.ListChannels(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not list notification channels",
			slog.String("event_id", event.ID), slog.String("error", err.Error()))
		return err
	}

	enabled := make([]Channel, 0, len(channels))
	for _, c := range channels {
		if c.Enabled {
			enabled = append(enabled, c)
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	return e.deliveries.Enqueue(ctx, event, enabled)
}

// Run dispatches due deliveries on a timer until ctx is cancelled or Stop
// is called.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	stopCh := make(chan struct{})
	e.stopCh = stopCh
	e.mu.Unlock()

	e.wg.Add(1)
	defer e.wg.Done()

	ticker := time.NewTicker(e.dispatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			e.dispatchDue(ctx)
		}
	}
}

// Stop ends Run and waits for it to return.
func (e *Engine) Stop() {
	e.mu.Lock()
	stopCh := e.stopCh
	e.stopCh = nil
	e.mu.Unlock()

	if stopCh == nil {
		return
	}
	close(stopCh)
	e.wg.Wait()
}

func (e *Engine) dispatchDue(ctx context.Context) {
	due, err := e.deliveries.DueDeliveries(ctx, time.Now(), e.batchSize)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not list due notification deliveries", slog.String("error", err.Error()))
		return
	}

	channelCache := map[string]Channel{}
	for _, d := range due {
		channel, ok := channelCache[d.ChannelID]
		if !ok {
			channel, err = e.channels.GetChannel(ctx, d.ChannelID)
			if err != nil {
				e.fail(ctx, d, err)
				continue
			}
			channelCache[d.ChannelID] = channel
		}

		sender, ok := e.senders[channel.Type]
		if !ok {
			e.exhaust(ctx, d, fmt.Errorf("no sender registered for channel type %q", channel.Type))
			continue
		}

		if err := sender.Send(ctx, channel, d); err != nil {
			e.fail(ctx, d, err)
			continue
		}

		if err := e.deliveries.MarkDelivered(ctx, d.ID, time.Now()); err != nil {
			e.logger.ErrorContext(ctx, "could not mark a notification delivered",
				slog.String("delivery_id", d.ID), slog.String("error", err.Error()))
		}
	}
}

func (e *Engine) fail(ctx context.Context, d Delivery, sendErr error) {
	attempts := d.Attempts + 1
	if attempts >= e.maxAttempts {
		e.exhaust(ctx, d, sendErr)
		return
	}

	next := time.Now().Add(jitter(e.backoff(attempts)))
	if err := e.deliveries.MarkRetry(ctx, d.ID, attempts, next, sendErr.Error()); err != nil {
		e.logger.ErrorContext(ctx, "could not record a notification retry",
			slog.String("delivery_id", d.ID), slog.String("error", err.Error()))
	}
}

func (e *Engine) exhaust(ctx context.Context, d Delivery, sendErr error) {
	e.logger.WarnContext(ctx, "notification delivery exhausted its retries",
		slog.String("delivery_id", d.ID), slog.String("channel_id", d.ChannelID), slog.String("error", sendErr.Error()))
	if err := e.deliveries.MarkExhausted(ctx, d.ID, d.Attempts+1, sendErr.Error()); err != nil {
		e.logger.ErrorContext(ctx, "could not record a notification failure",
			slog.String("delivery_id", d.ID), slog.String("error", err.Error()))
	}
}

// backoff returns the delay before the given attempt number, doubling from
// backoffBase and capped at backoffMax.
func (e *Engine) backoff(attempt int) time.Duration {
	d := e.backoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= e.backoffMax {
			return e.backoffMax
		}
	}
	return d
}

// jitter returns a uniformly random duration in [0, d] — full jitter, the
// same shape [remote.Transport]'s replay loop applies to its own backoff.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}
