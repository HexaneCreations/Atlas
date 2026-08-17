// Package transport carries collected observations from wherever they are
// produced to wherever they are stored.
//
// This package exists for one reason: Atlas ships first as a single binary
// observing its own host, and must later observe a fleet through remote
// agents (Tier 4). Those are the same collectors and the same storage with a
// different distance in between.
//
// By making that distance an interface from the first day, the change is a
// swap of one implementation for another at the composition root:
//
//	Collector -> Scheduler -> Transport -> Sink -> Storage
//	                             |
//	              +--------------+--------------+
//	              |                             |
//	        inprocess.Transport           grpc.Transport
//	        (single node, today)          (agent to server, Tier 4)
//
// No collector and no repository knows which is in use. The alternative —
// having the scheduler call storage directly and refactoring every collector
// when agents arrive — is the single largest avoidable rewrite in the
// roadmap. See docs/adr/0005-transport-seam.md.
package transport

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
)

// ProtocolVersion is the wire-contract version every agent request carries
// and every control plane enforces.
//
// It lives here, below both the agent client (internal/core/transport/remote)
// and the server handler (internal/api/agent), so the two cannot drift into
// disagreeing about what version they speak. Bump it when the envelope or
// request shape changes incompatibly; a mismatch is then refused explicitly
// rather than being absorbed as incidental JSON tolerance, which is how a
// half-upgraded fleet silently corrupts data.
const ProtocolVersion = 1

// Origin identifies where an observation was made.
//
// Every envelope carries one, including in single-node deployments where it
// is always the local host. Making origin unconditional means the storage
// schema, the query layer, and the UI are multi-node from the start, and
// Tier 4 adds agents without a data migration.
type Origin struct {
	// NodeID is the stable identifier of the observed machine. It survives
	// hostname changes and re-imaging, so historical metrics stay attached to
	// the machine they describe.
	NodeID string `json:"node_id"`
	// Hostname is the machine's current name, for display.
	Hostname string `json:"hostname"`
	// AgentVersion is the version of the code that made the observation. In
	// a fleet, agents upgrade at different times; without this, a metric that
	// changes meaning between versions is impossible to interpret.
	AgentVersion string `json:"agent_version"`
	// Environment is the operator-assigned tag for this machine. It rides on
	// the origin rather than being looked up centrally because in a fleet the
	// agent is the only thing that knows it — the control plane has no
	// inventory to consult.
	Environment string `json:"environment,omitempty"`
}

// Validate reports whether the origin is usable.
func (o Origin) Validate() error {
	if o.NodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "origin has no node ID")
	}
	return nil
}

// Envelope is one unit of telemetry in transit, with everything the receiving
// end needs to attribute, order, and deduplicate it.
//
// The envelope is deliberately payload-agnostic. Metrics were the first thing
// it carried and inventory snapshots the second; events, audit records, traces
// and cost attribution will follow. Each of those needs the same
// authentication, the same idempotency, the same spooling decision and the
// same flow control, and gets them by being a [Payload] rather than by growing
// an endpoint of its own. See [Kind].
type Envelope struct {
	// ID uniquely identifies this envelope. Assigned on Send when empty, and
	// used by the receiver to discard duplicates — a retrying transport may
	// deliver the same envelope more than once.
	ID string `json:"id"`
	// Origin says where the observations came from.
	//
	// Over a network this field is a *claim*, not a fact: it arrives in the
	// request body, where anything holding a credential can write whatever it
	// likes. The ingest boundary overwrites it from the authenticated peer
	// before the envelope reaches storage.
	Origin Origin `json:"origin"`
	// Payload is what this envelope carries. Its concrete type is named by
	// its own Kind.
	Payload Payload `json:"payload"`
	// SentAt is when the sender dispatched it. Compared against arrival time,
	// it reveals clock skew and transport lag across a fleet.
	SentAt time.Time `json:"sent_at"`
}

// Kind reports what the envelope carries, or the empty kind if it carries
// nothing.
func (e Envelope) Kind() Kind {
	if e.Payload == nil {
		return ""
	}
	return e.Payload.Kind()
}

// Class reports how this envelope must be delivered.
func (e Envelope) Class() Class { return ClassOf(e.Kind()) }

// Validate reports whether the envelope is well formed.
func (e Envelope) Validate() error {
	if err := e.Origin.Validate(); err != nil {
		return err
	}
	if e.Payload == nil {
		return errs.New(errs.CodeInvalidArgument, "envelope has no payload")
	}
	return e.Payload.Validate()
}

// Transport delivers envelopes towards storage.
//
// Send may be called concurrently. Implementations that cross a network are
// expected to handle their own retries and buffering, and to return an error
// only when delivery has genuinely failed — the scheduler's response to an
// error is to log and drop, because holding observations in the collector
// would grow memory without bound on a host that has lost its uplink.
type Transport interface {
	// Send delivers an envelope. It must respect the context deadline.
	Send(ctx context.Context, env Envelope) error
	// Close releases resources and flushes anything buffered.
	Close() error
}

// Sink is the receiving end of a transport: the thing that does something
// durable with observations.
//
// In a single-node deployment the sink is the storage layer, reached directly
// through [inprocess.Transport]. In a fleet the sink lives in the control
// plane and the transport is the network.
type Sink interface {
	// Receive accepts an envelope. Returning an error tells the transport
	// delivery failed; a retrying transport may then send it again, so
	// implementations must be idempotent on [Envelope.ID].
	Receive(ctx context.Context, env Envelope) error
}

// SinkFunc adapts a function to [Sink].
type SinkFunc func(ctx context.Context, env Envelope) error

// Receive implements [Sink].
func (f SinkFunc) Receive(ctx context.Context, env Envelope) error { return f(ctx, env) }

// NewEnvelope builds a metrics envelope for a batch, assigning an ID and send
// time.
//
// It keeps its original signature — taking a [collect.Batch] rather than a
// [Payload] — because the scheduler is the only caller and metrics are the
// only thing it produces. Making every collection site name its payload type
// would be ceremony with no reader.
func NewEnvelope(origin Origin, batch collect.Batch) Envelope {
	return NewEnvelopeOf(origin, Metrics{Batch: batch})
}

// NewEnvelopeOf builds an envelope for any payload, assigning an ID and send
// time. This is the general form used by non-metric telemetry.
func NewEnvelopeOf(origin Origin, payload Payload) Envelope {
	return Envelope{
		ID:      id.New(),
		Origin:  origin,
		Payload: payload,
		SentAt:  time.Now(),
	}
}
