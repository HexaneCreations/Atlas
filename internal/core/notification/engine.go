package notification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/platform/id"
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
	// DefaultConcurrency bounds how many of a claimed batch are sent at
	// once. Bounded rather than all-at-once so one slow or hung destination
	// cannot starve the rest of the batch out of their share of the tick.
	DefaultConcurrency = 4
	// DefaultLease is how long a claimed delivery is protected from being
	// claimed again before it is treated as abandoned. It has to outlast a
	// single Send comfortably (Send itself is bounded by
	// DefaultSendTimeout, 10s) — long enough that a slow but healthy send
	// is never raced by a second claimer, short enough that a worker that
	// crashed mid-send does not leave its rows stuck for long.
	DefaultLease = 2 * time.Minute
	// DefaultRetentionInterval is how often Run sweeps old deliveries.
	DefaultRetentionInterval = 1 * time.Hour
)

// ChannelStore is what [Engine] needs from the channel repository.
type ChannelStore interface {
	ListChannels(ctx context.Context) ([]Channel, error)
	GetChannel(ctx context.Context, id string) (Channel, error)
	// ChannelSecret returns a channel's decrypted secret. send calls this
	// immediately before Send, never caches the result across deliveries,
	// and passes it to the Sender as a plain argument that goes out of
	// scope when Send returns — see [Sender].
	ChannelSecret(ctx context.Context, channelID string) (string, error)
}

// NodeInfoSource resolves a node's display name and address for a
// notification at send time. Nothing it returns is stored: a retry
// resolves again.
type NodeInfoSource interface {
	// NodeInfo returns the node's hostname and observed address, either
	// of which may be "". An unknown node is an error or empty values.
	NodeInfo(ctx context.Context, nodeID string) (name, address string, err error)
}

// nodeInfoTimeout bounds one lookup so a slow database never holds a
// claimed delivery's lease much longer than its send.
const nodeInfoTimeout = 2 * time.Second

// SweepResult reports what one [DeliveryStore.SweepDeliveries] call changed.
type SweepResult struct {
	// Deleted is how many finished (delivered or failed) rows were removed.
	Deleted int64
	// Expired is how many rows that sat pending too long were marked failed.
	Expired int64
}

// DeliveryStore is the durable retry queue [Engine] drains.
//
// Claiming is how two [Engine]s — two goroutines in one process, or two
// separate server processes against the same database — avoid sending the
// same delivery twice: both ClaimDue and ClaimByEvent are a single atomic
// statement that both picks rows no other claimer currently holds and
// marks them held, rather than a separate read followed by a separate
// write that leaves a window for two claimers to both read the same row
// before either writes.
type DeliveryStore interface {
	// Enqueue inserts one pending delivery per channel and returns how many
	// were new. Idempotent on (n.EventID, channel.ID).
	Enqueue(ctx context.Context, n Notification, channels []Channel) (int, error)
	// ClaimDue atomically claims up to limit pending deliveries at or past
	// now, oldest first, setting next_attempt_at to lease on the way out —
	// see the package doc for what that lease means for the delivery
	// guarantee.
	ClaimDue(ctx context.Context, now, lease time.Time, limit int) ([]Delivery, error)
	// ClaimByEvent atomically claims the one pending delivery for
	// (eventID, channelID) — unique by schema — the same way ClaimDue
	// does, for [Engine.SendTest]'s synchronous single-row send. found is
	// false if the row is not pending (already claimed, already finished,
	// or never existed).
	ClaimByEvent(ctx context.Context, eventID, channelID string, lease time.Time) (d Delivery, found bool, err error)
	MarkDelivered(ctx context.Context, id string, at time.Time) error
	MarkRetry(ctx context.Context, id string, attempts int, nextAttempt time.Time, lastErr string) error
	MarkExhausted(ctx context.Context, id string, attempts int, lastErr string) error
	// SweepDeliveries deletes finished deliveries past their retention
	// window and marks a delivery that has sat pending too long as failed.
	SweepDeliveries(ctx context.Context, now time.Time) (SweepResult, error)
}

// Engine enqueues events and, on its own schedule, delivers what is due.
//
// # Delivery guarantee
//
// At-least-once, never at-most-once. A crash between a successful Send and
// MarkDelivered leaves the row claimed (its next_attempt_at pushed out to
// the lease) but still "pending" — once the lease passes, ClaimDue picks it
// up again and it is sent a second time. UNIQUE (event_id, channel_id) does
// not prevent that second send (the row already exists; nothing is
// inserted twice), but it is what makes a second Notify call for the same
// event safe to repeat and what will let a future idempotency key in a
// Sender's own payload dedupe on the receiving end if that ever matters
// enough to add. Today, a duplicate send is the accepted cost of never
// silently losing one.
type Engine struct {
	channels   ChannelStore
	deliveries DeliveryStore
	senders    map[ChannelType]Sender
	nodeInfo   NodeInfoSource
	logger     *slog.Logger

	maxAttempts       int
	backoffBase       time.Duration
	backoffMax        time.Duration
	dispatchInterval  time.Duration
	batchSize         int
	concurrency       int
	lease             time.Duration
	retentionInterval time.Duration

	mu     sync.Mutex
	stopCh chan struct{}
	wg     sync.WaitGroup
	// wake lets Notify nudge Run into dispatching immediately instead of
	// waiting out the rest of the poll interval, buffered by one so a
	// burst of Notify calls coalesces into a single extra tick rather than
	// queuing one per call.
	wake chan struct{}
}

// Options configures an [Engine].
type Options struct {
	Channels   ChannelStore
	Deliveries DeliveryStore
	// Senders maps a channel type to what delivers it. A type with no
	// registered sender fails its deliveries immediately with that fact as
	// the error, rather than retrying something that can never succeed.
	Senders map[ChannelType]Sender
	// NodeInfo, when set, fills [Delivery.NodeName] and
	// [Delivery.NodeAddress] just before each send.
	NodeInfo NodeInfoSource
	Logger   *slog.Logger

	MaxAttempts      int
	BackoffBase      time.Duration
	BackoffMax       time.Duration
	DispatchInterval time.Duration
	BatchSize        int
	// Concurrency bounds how many claimed deliveries are sent at once.
	Concurrency int
	// Lease is how long a claimed delivery is protected from a second
	// claimer. See [DefaultLease].
	Lease time.Duration
	// RetentionInterval is how often old deliveries are swept.
	RetentionInterval time.Duration
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
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Lease <= 0 {
		opts.Lease = DefaultLease
	}
	if opts.RetentionInterval <= 0 {
		opts.RetentionInterval = DefaultRetentionInterval
	}
	return &Engine{
		channels: opts.Channels, deliveries: opts.Deliveries, senders: opts.Senders, nodeInfo: opts.NodeInfo, logger: opts.Logger,
		maxAttempts: opts.MaxAttempts, backoffBase: opts.BackoffBase, backoffMax: opts.BackoffMax,
		dispatchInterval: opts.DispatchInterval, batchSize: opts.BatchSize, concurrency: opts.Concurrency,
		lease: opts.Lease, retentionInterval: opts.RetentionInterval,
		wake: make(chan struct{}, 1),
	}
}

// Notify enqueues n for delivery to every enabled channel.
//
// This does one durable write and returns — no network call happens on
// this path. That is what keeps a caller from blocking on, or failing
// because of, notification delivery.
func (e *Engine) Notify(ctx context.Context, n Notification) error {
	channels, err := e.channels.ListChannels(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not list notification channels",
			slog.String("event_id", n.EventID), slog.String("error", err.Error()))
		return err
	}

	enabled := make([]Channel, 0, len(channels))
	for _, c := range channels {
		if !c.Enabled {
			continue
		}
		// A test notification always reaches every enabled channel,
		// regardless of min_severity: it exists to prove the channel's
		// destination and credentials work, not to exercise severity
		// routing. Every other kind — including recovered, whose
		// Severity is the severity of the alert it resolves — is filtered
		// the same way: below the channel's min_severity, it is not sent.
		if n.Kind != KindTest && severityRank(n.Severity) < severityRank(c.MinSeverity) {
			continue
		}
		enabled = append(enabled, c)
	}
	if len(enabled) == 0 {
		return nil
	}
	_, err = e.deliveries.Enqueue(ctx, n, enabled)
	if err != nil {
		return err
	}
	e.nudge()
	return nil
}

// nudge wakes Run's loop into dispatching now instead of waiting out the
// rest of the poll interval, if Run is running at all — a pure throughput
// optimisation, never required for correctness (the next regular tick, or
// another process's, gets there regardless).
func (e *Engine) nudge() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run dispatches due deliveries — on a timer, or immediately when Notify
// wakes it — and periodically sweeps old ones, until ctx is cancelled or
// Stop is called.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	stopCh := make(chan struct{})
	e.stopCh = stopCh
	e.mu.Unlock()

	e.wg.Add(1)
	defer e.wg.Done()

	ticker := time.NewTicker(e.dispatchInterval)
	defer ticker.Stop()
	retention := time.NewTicker(e.retentionInterval)
	defer retention.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			e.dispatchDue(ctx)
		case <-e.wake:
			e.dispatchDue(ctx)
		case <-retention.C:
			e.sweep(ctx)
		}
	}
}

// Stop tells Run to stop claiming new work, then waits — bounded by ctx —
// for whatever it already claimed to finish sending. If ctx's deadline
// passes first, Stop returns anyway rather than block the rest of the
// process's shutdown; the deliveries still in flight are not stranded by
// this, only by this method returning without them: each one's lease
// still expires on its own schedule, at which point ClaimDue (this
// process's, after a restart, or another process's) picks it up again —
// the same at-least-once guarantee a mid-send crash gets, just triggered
// by a timeout instead of a kill.
//
// This takes a context, unlike [corealert.Engine.Stop] and
// [coreincident.Engine.Stop]: those engines' own work is a DB read-and-
// write with no real way to hang, so an unbounded Wait never mattered.
// This one's in-flight work is a network call with its own ~10s timeout
// per send, times up to [DefaultConcurrency] of them outstanding — a real
// enough duration that "wait for Run to return" needs an actual bound,
// supplied by the caller's shutdown deadline rather than a constant this
// package would otherwise have to invent.
func (e *Engine) Stop(ctx context.Context) {
	e.mu.Lock()
	stopCh := e.stopCh
	e.stopCh = nil
	e.mu.Unlock()

	if stopCh == nil {
		return
	}
	close(stopCh)

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		e.logger.WarnContext(ctx, "stopped before in-flight notification sends finished; "+
			"their leases will expire and they will be retried")
	}
}

// DispatchOnce claims and attempts every currently due delivery once,
// without waiting for Run's own poll interval or wake signal. Exported so
// a test can trigger a deterministic dispatch cycle instead of racing a
// timer, and usable later by an operator-facing "flush now" action.
func (e *Engine) DispatchOnce(ctx context.Context) {
	e.dispatchDue(ctx)
}

func (e *Engine) dispatchDue(ctx context.Context) {
	now := time.Now()
	due, err := e.deliveries.ClaimDue(ctx, now, now.Add(e.lease), e.batchSize)
	if err != nil {
		e.logger.ErrorContext(ctx, "could not claim due notification deliveries", slog.String("error", err.Error()))
		return
	}
	if len(due) == 0 {
		return
	}
	e.logger.DebugContext(ctx, "claimed due notification deliveries", slog.Int("count", len(due)))

	// A plain map would race: every claimed delivery runs in its own
	// goroutine below, and more than one can name the same channel.
	var channelMu sync.Mutex
	channelCache := map[string]Channel{}
	getChannel := func(channelID string) (Channel, error) {
		channelMu.Lock()
		c, ok := channelCache[channelID]
		channelMu.Unlock()
		if ok {
			return c, nil
		}
		c, err := e.channels.GetChannel(ctx, channelID)
		if err != nil {
			return Channel{}, err
		}
		channelMu.Lock()
		channelCache[channelID] = c
		channelMu.Unlock()
		return c, nil
	}

	nodes := newNodeInfoCache()

	// Bounded by a buffered channel used as a semaphore: at most
	// e.concurrency sends in flight at once, however large the claimed
	// batch is. dispatchDue (and so Stop, which waits for Run, which waits
	// for this call) does not return until every one of them has.
	sem := make(chan struct{}, e.concurrency)
	var wg sync.WaitGroup
	for _, d := range due {
		channel, err := getChannel(d.ChannelID)
		if err != nil {
			e.fail(ctx, d, err)
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(channel Channel, d Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			e.resolveNode(ctx, &d, nodes)
			_ = e.send(ctx, channel, d, e.fail)
		}(channel, d)
	}
	wg.Wait()
}

// nodeInfoCache holds one dispatch batch's node lookups, so a batch that
// names the same node many times queries it once, even concurrently.
type nodeInfoCache struct {
	mu    sync.Mutex
	known map[string]*nodeInfoEntry
}

type nodeInfoEntry struct {
	once          sync.Once
	name, address string
}

func newNodeInfoCache() *nodeInfoCache { return &nodeInfoCache{known: map[string]*nodeInfoEntry{}} }

func (c *nodeInfoCache) entry(nodeID string) *nodeInfoEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	en, ok := c.known[nodeID]
	if !ok {
		en = &nodeInfoEntry{}
		c.known[nodeID] = en
	}
	return en
}

// resolveNode fills d's node name and address. It never fails the
// delivery: on any error the fields stay empty and the sender falls back
// to the node id.
func (e *Engine) resolveNode(ctx context.Context, d *Delivery, cache *nodeInfoCache) {
	if e.nodeInfo == nil || d.NodeID == "" {
		return
	}
	en := cache.entry(d.NodeID)
	en.once.Do(func() {
		lookupCtx, cancel := context.WithTimeout(ctx, nodeInfoTimeout)
		defer cancel()
		name, address, err := e.nodeInfo.NodeInfo(lookupCtx, d.NodeID)
		if err != nil {
			e.logger.DebugContext(ctx, "could not resolve a notification's node; sending with its id",
				slog.String("node_id", d.NodeID), slog.String("error", Redact(err.Error())))
			return
		}
		en.name, en.address = name, address
	})
	d.NodeName, d.NodeAddress = en.name, en.address
}

// send attempts one delivery against its already-resolved channel: looks
// up the registered Sender, decrypts the secret immediately before
// calling it, and records the outcome. onFailure decides what a failure
// means for this call site — dispatchDue passes e.fail (retry with
// backoff, eventually exhaust), SendTest passes e.exhaust directly (a
// manual test is one shot, with no "later" to retry at). An unknown
// channel type always goes straight to e.exhaust regardless of
// onFailure: no amount of retrying makes a sender appear.
func (e *Engine) send(ctx context.Context, channel Channel, d Delivery, onFailure func(context.Context, Delivery, error)) error {
	sender, ok := e.senders[channel.Type]
	if !ok {
		err := noSenderError(channel.Type)
		e.exhaust(ctx, d, err)
		return err
	}

	// Decrypted here, immediately before Send, and nowhere else: not
	// cached alongside the channel (which may be reused across many
	// deliveries in one batch), not read until this exact delivery needs
	// it. A missing or unusable key — [ErrSecretUnreadable], or no key
	// configured at all — fails this one delivery through onFailure like
	// any other send error; it is never a plaintext fallback and never a
	// panic.
	secret, err := e.channels.ChannelSecret(ctx, channel.ID)
	if err != nil {
		onFailure(ctx, d, err)
		return err
	}

	if err := sender.Send(ctx, channel, secret, d); err != nil {
		onFailure(ctx, d, err)
		return err
	}

	if err := e.deliveries.MarkDelivered(ctx, d.ID, time.Now()); err != nil {
		e.logger.ErrorContext(ctx, "could not mark a notification delivered",
			slog.String("delivery_id", d.ID), slog.String("error", err.Error()))
	}
	return nil
}

// SendTestOutcome is the result of one [Engine.SendTest] call.
type SendTestOutcome struct {
	Delivered bool
	// Error is empty when Delivered is true; otherwise a fixed category
	// message safe to return to an API caller — see [SendErrorCategory].
	Error string
}

// SendTest sends one test notification to channelID immediately and
// synchronously, for the "Test" button a future API exposes (step 10).
//
// It deliberately bypasses both filters [Engine.Notify] applies: min_
// severity (a test is not a real alert, so there is no severity to
// compare) and Enabled (an operator testing a channel they just disabled,
// or have not enabled yet, still needs to know whether the destination
// and credentials work). The delivery is still enqueued through the
// normal path first, so the attempt appears in the delivery log like any
// other, then claimed by its exact (event, channel) key — ClaimByEvent —
// rather than waiting for the batch poll, and sent inline rather than
// through the retry queue: a failure here is final, recorded with
// MarkExhausted via e.exhaust, not retried later.
func (e *Engine) SendTest(ctx context.Context, channelID string) (SendTestOutcome, error) {
	const op = "notification.Engine.SendTest"

	channel, err := e.channels.GetChannel(ctx, channelID)
	if err != nil {
		return SendTestOutcome{}, err
	}

	n := testNotification(channel.Name)
	eventID := n.EventID
	if _, err := e.deliveries.Enqueue(ctx, n, []Channel{channel}); err != nil {
		return SendTestOutcome{}, err
	}

	d, found, err := e.deliveries.ClaimByEvent(ctx, eventID, channel.ID, time.Now().Add(e.lease))
	if err != nil {
		return SendTestOutcome{}, err
	}
	if !found {
		// Only reachable if the background dispatcher's own poll claimed
		// this exact, just-inserted row in the instant between Enqueue and
		// here. The test notification is not lost — whatever claimed it
		// will still send it — just not synchronously to this caller.
		return SendTestOutcome{}, fmt.Errorf("%s: %w", op, ErrTestClaimed)
	}

	// A test's failure is recorded and logged as its category only: the
	// full text quotes the destination's response.
	exhaust := func(ctx context.Context, d Delivery, sendErr error) {
		e.exhaust(ctx, d, errors.New(SendErrorCategory(sendErr)))
	}
	if sendErr := e.send(ctx, channel, d, exhaust); sendErr != nil {
		return SendTestOutcome{Error: SendErrorCategory(sendErr)}, nil
	}
	return SendTestOutcome{Delivered: true}, nil
}

// testNotification is the message every test sends. Its EventID is unique
// per call: two tests must both send.
func testNotification(channelName string) Notification {
	return Notification{
		EventID: "test-" + id.New(), Kind: KindTest,
		Title:   "Atlas test notification",
		Message: fmt.Sprintf("This is a test of the %q notification channel.", channelName),
	}
}

// sweep deletes finished deliveries past their retention window and marks
// anything that has sat pending too long as failed. See
// [DeliveryStore.SweepDeliveries] and, for the actual windows,
// storagenotification.DeliveredRetention / FailedRetention / PendingExpiry.
func (e *Engine) sweep(ctx context.Context) {
	res, err := e.deliveries.SweepDeliveries(ctx, time.Now())
	if err != nil {
		e.logger.ErrorContext(ctx, "could not sweep old notification deliveries", slog.String("error", err.Error()))
		return
	}
	if res.Deleted > 0 || res.Expired > 0 {
		e.logger.InfoContext(ctx, "swept notification deliveries",
			slog.Int64("deleted", res.Deleted), slog.Int64("expired", res.Expired))
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
	// Logged separately from the stored last_error (which [DeliveryStore]
	// sanitises on its own way into the database): this is a log line, not
	// a column, and would otherwise carry sendErr's raw text verbatim —
	// including a Teams URL's sig= or a custom header value a Sender
	// returned unredacted.
	e.logger.WarnContext(ctx, "notification delivery exhausted its retries",
		slog.String("delivery_id", d.ID), slog.String("channel_id", d.ChannelID), slog.String("error", Redact(sendErr.Error())))
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
