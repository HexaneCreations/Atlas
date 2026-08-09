package transport

import (
	"context"
	"sync"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Receiver handles envelopes of one kind.
//
// A receiver owns its own storage and its own transaction. That separation is
// deliberate: metric samples want a COPY into a hypertable, inventory
// snapshots want an upsert keyed by node and subject, and a future event
// stream will want something else again. Forcing them through one write path
// would mean the worst of all three.
type Receiver interface {
	// Kind is the payload kind this receiver handles. It must be constant.
	Kind() Kind
	// Receive persists an envelope. It must be idempotent on [Envelope.ID]:
	// any transport that retries may deliver the same envelope twice.
	Receive(ctx context.Context, env Envelope) error
}

// Router is a [Sink] that dispatches envelopes to a receiver by kind.
//
// This is the seam that keeps the ingest path from growing a branch per
// telemetry type. Authentication, deduplication, clock-skew checking and flow
// control all happen before the router; everything after it is the concern of
// exactly one receiver. Adding events, traces, or cost attribution means
// registering a receiver, not editing a dispatch table that lives somewhere
// else.
type Router struct {
	mu        sync.RWMutex
	receivers map[Kind]Receiver
}

// NewRouter builds an empty router.
func NewRouter() *Router {
	return &Router{receivers: make(map[Kind]Receiver)}
}

// Register attaches a receiver for its kind.
//
// A duplicate registration is refused rather than silently overwriting: two
// receivers for one kind means one of them was going to be receiving nothing,
// and finding that out at startup is much cheaper than finding it out from a
// missing week of data.
func (r *Router) Register(recv Receiver) error {
	const op = "transport.Router.Register"

	if recv == nil {
		return errs.New(errs.CodeInvalidArgument, "cannot register a nil receiver").WithOp(op)
	}
	kind := recv.Kind()
	if kind == "" {
		return errs.New(errs.CodeInvalidArgument, "cannot register a receiver with no kind").WithOp(op)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.receivers[kind]; exists {
		return errs.New(errs.CodeAlreadyExists,
			"a receiver for %q is already registered", kind).
			WithOp(op).WithDetail("kind", string(kind))
	}
	r.receivers[kind] = recv
	return nil
}

// Receive implements [Sink] by dispatching to the receiver for the envelope's
// kind.
func (r *Router) Receive(ctx context.Context, env Envelope) error {
	const op = "transport.Router.Receive"

	kind := env.Kind()
	if kind == "" {
		return errs.New(errs.CodeInvalidArgument, "envelope has no payload").WithOp(op)
	}

	r.mu.RLock()
	recv, ok := r.receivers[kind]
	r.mu.RUnlock()

	if !ok {
		// A registered kind with no receiver is what a partially-deployed
		// control plane looks like: the agent knows how to send something
		// this server was not built to store. Refusing tells the agent to
		// stop sending it, which is better than accepting and discarding.
		return errs.New(errs.CodeNotImplemented,
			"this Atlas instance does not store %q telemetry", kind).
			WithOp(op).
			WithDetail("kind", string(kind)).
			WithDetail("accepted", kindList(r.kinds()))
	}

	return recv.Receive(ctx, env)
}

// Handles reports whether a receiver is registered for a kind.
func (r *Router) Handles(k Kind) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.receivers[k]
	return ok
}

// Kinds lists the kinds this router accepts, in a stable order. The ingest
// endpoint reports them so an agent can tell what a server understands
// without trying it.
func (r *Router) Kinds() []Kind { return r.kinds() }

func (r *Router) kinds() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Kind, 0, len(r.receivers))
	for _, k := range KnownKinds() {
		if _, ok := r.receivers[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

func kindList(kinds []Kind) string {
	if len(kinds) == 0 {
		return "none"
	}
	s := string(kinds[0])
	for _, k := range kinds[1:] {
		s += ", " + string(k)
	}
	return s
}

var _ Sink = (*Router)(nil)
