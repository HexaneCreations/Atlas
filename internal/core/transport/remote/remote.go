package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/core/transport/spool"
	"github.com/hexane/atlas/internal/platform/errs"
)

const (
	maxBatchSize   = 200
	pollInterval   = 2 * time.Second
	backoffMin     = 1 * time.Second
	backoffMax     = 2 * time.Minute
	requestTimeout = 30 * time.Second
)

type telemetryRequest struct {
	ProtocolVersion int                  `json:"protocol_version"`
	Envelopes       []transport.Envelope `json:"envelopes"`
}

type rejectedEnvelope struct {
	EnvelopeID string `json:"envelope_id"`
	Reason     string `json:"reason"`
}

type telemetryResponse struct {
	Accepted     int                `json:"accepted"`
	Rejected     []rejectedEnvelope `json:"rejected"`
	Directive    string             `json:"directive"`
	RetryAfterMs int                `json:"retry_after_ms"`
}

// Options configures a [Transport].
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	TLSConfig  *tls.Config
	Spool      *spool.Spool
	Logger     *slog.Logger
}

// Transport pushes envelopes to a control plane over HTTPS.
//
// Stream-class envelopes are spooled before delivery is attempted, so an
// outage never loses them. Snapshot-class envelopes are sent immediately and
// dropped on failure — a stale snapshot has no value once a newer one
// exists.
type Transport struct {
	client *http.Client
	url    string
	spool  *spool.Spool
	logger *slog.Logger

	wakeCh chan struct{}
	stopCh chan struct{}
	wg     sync.WaitGroup
	closed atomic.Bool

	sent     atomic.Uint64
	failed   atomic.Uint64
	rejected atomic.Uint64
	retries  atomic.Uint64

	// outcomeMu guards the last-delivery record. It is separate from the
	// counters above because a reader needs the time and its reason to be
	// consistent with each other, which three independent atomics cannot
	// promise.
	outcomeMu     sync.Mutex
	lastSuccess   time.Time
	lastFailure   time.Time
	failureReason string
}

// New builds a Transport and starts its background replay loop.
func New(opts Options) (*Transport, error) {
	const op = "remote.New"
	if opts.BaseURL == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "remote transport requires a base URL").WithOp(op)
	}
	if opts.Spool == nil {
		return nil, errs.New(errs.CodeInvalidArgument, "remote transport requires a spool").WithOp(op)
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout:   requestTimeout,
			Transport: &http.Transport{TLSClientConfig: opts.TLSConfig},
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	t := &Transport{
		client: client,
		url:    opts.BaseURL + "/api/v1/agent/telemetry",
		spool:  opts.Spool,
		logger: logger,
		wakeCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}

	t.wg.Add(1)
	go t.replayLoop()

	return t, nil
}

// Send implements [transport.Transport].
func (t *Transport) Send(ctx context.Context, env transport.Envelope) error {
	const op = "remote.Transport.Send"
	if t.closed.Load() {
		return errs.New(errs.CodeUnavailable, "transport is closed").WithOp(op)
	}

	if env.Class() == transport.ClassSnapshot {
		resp, err := t.post(ctx, []transport.Envelope{env})
		if err != nil {
			t.failed.Add(1)
			t.recordFailure(err)
			return errs.Wrap(err, errs.CodeUnavailable, "could not deliver snapshot").WithOp(op)
		}
		if resp.Accepted == 0 {
			t.rejected.Add(1)
			reason := "unknown"
			if len(resp.Rejected) > 0 {
				reason = resp.Rejected[0].Reason
			}
			rejection := errs.New(errs.CodeInvalidArgument, "snapshot rejected: %s", reason).WithOp(op)
			t.recordFailure(rejection)
			return rejection
		}
		t.sent.Add(1)
		t.recordSuccess()
		return nil
	}

	if err := t.spool.Enqueue(env); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not spool envelope").WithOp(op)
	}
	t.wake()
	return nil
}

// Close stops the replay loop. Anything still spooled survives on disk for
// the next process.
func (t *Transport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(t.stopCh)
	t.wg.Wait()
	return nil
}

// Stats reports delivery counts and the outcome of the most recent attempt
// in each direction. It is what lets a control plane distinguish "this agent
// is fine and has nothing to say" from "this agent cannot reach me".
type Stats struct {
	Sent     uint64 `json:"sent"`
	Failed   uint64 `json:"failed"`
	Rejected uint64 `json:"rejected"`
	Spooled  int    `json:"spooled"`
	// SpooledBytes is the spool's disk footprint, the figure that matters
	// for a host running out of space during a long outage.
	SpooledBytes int64 `json:"spooled_bytes"`
	// Dropped counts envelopes evicted because the spool hit its size cap.
	Dropped uint64 `json:"dropped"`
	// Retries counts delivery attempts that failed and were rescheduled.
	Retries uint64 `json:"retries"`

	LastSuccess time.Time `json:"last_success,omitzero"`
	LastFailure time.Time `json:"last_failure,omitzero"`
	// LastFailureReason is the most recent failure's message. It is an
	// agent-side diagnostic string, never a credential — the transport only
	// ever sees connection and HTTP status errors.
	LastFailureReason string `json:"last_failure_reason,omitempty"`
}

func (t *Transport) Stats() Stats {
	t.outcomeMu.Lock()
	lastSuccess, lastFailure, reason := t.lastSuccess, t.lastFailure, t.failureReason
	t.outcomeMu.Unlock()

	return Stats{
		Sent: t.sent.Load(), Failed: t.failed.Load(), Rejected: t.rejected.Load(),
		Spooled: t.spool.Len(), SpooledBytes: t.spool.Bytes(), Dropped: t.spool.Dropped(),
		Retries:     t.retries.Load(),
		LastSuccess: lastSuccess, LastFailure: lastFailure, LastFailureReason: reason,
	}
}

func (t *Transport) recordSuccess() {
	t.outcomeMu.Lock()
	defer t.outcomeMu.Unlock()
	t.lastSuccess = time.Now()
}

func (t *Transport) recordFailure(err error) {
	t.outcomeMu.Lock()
	defer t.outcomeMu.Unlock()
	t.lastFailure = time.Now()
	t.failureReason = err.Error()
}

func (t *Transport) wake() {
	select {
	case t.wakeCh <- struct{}{}:
	default:
	}
}

func (t *Transport) replayLoop() {
	defer t.wg.Done()

	backoff := backoffMin
	for {
		select {
		case <-t.stopCh:
			return
		case <-t.wakeCh:
		case <-time.After(pollInterval):
		}

		for {
			if t.spool.Len() == 0 {
				break
			}

			batch, err := t.spool.PeekN(maxBatchSize)
			if err != nil || len(batch) == 0 {
				break
			}

			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			resp, err := t.post(ctx, batch)
			cancel()

			if err != nil {
				t.failed.Add(uint64(len(batch)))
				t.retries.Add(1)
				t.recordFailure(err)
				t.logger.Warn("telemetry delivery failed, will retry",
					slog.String("error", err.Error()), slog.Duration("backoff", backoff))
				select {
				case <-t.stopCh:
					return
				case <-time.After(jitter(backoff)):
				}
				backoff = min(backoff*2, backoffMax)
				break
			}

			if err := t.spool.DequeueN(len(batch)); err != nil {
				t.logger.Error("could not clear delivered envelopes from spool", slog.String("error", err.Error()))
			}
			t.sent.Add(uint64(resp.Accepted))
			t.rejected.Add(uint64(len(resp.Rejected)))
			t.recordSuccess()
			backoff = backoffMin

			if resp.Directive == "slow_down" || resp.Directive == "pause" {
				delay := time.Duration(resp.RetryAfterMs) * time.Millisecond
				if delay <= 0 {
					delay = pollInterval
				}
				select {
				case <-t.stopCh:
					return
				case <-time.After(delay):
				}
			}
		}
	}
}

func (t *Transport) post(ctx context.Context, envelopes []transport.Envelope) (telemetryResponse, error) {
	body, err := json.Marshal(telemetryRequest{ProtocolVersion: transport.ProtocolVersion, Envelopes: envelopes})
	if err != nil {
		return telemetryResponse{}, fmt.Errorf("encode telemetry request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return telemetryResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return telemetryResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return telemetryResponse{}, fmt.Errorf("telemetry endpoint returned %d", resp.StatusCode)
	}

	var out telemetryResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return telemetryResponse{}, fmt.Errorf("decode telemetry response: %w", err)
	}
	return out, nil
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(rand.Int64N(int64(d) + 1))
}

var _ transport.Transport = (*Transport)(nil)
