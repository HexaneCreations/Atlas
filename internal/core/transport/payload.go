package transport

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

// Kind names what an envelope carries.
//
// Atlas began by shipping one thing across the transport — metric batches —
// and will ship several more: inventory snapshots, events, audit records,
// cost attribution, security findings. Without a discriminator each of those
// arrives as its own endpoint with its own authentication, its own
// idempotency handling, its own spooling decision, and its own backpressure
// logic. That is four copies of the difficult parts and four opportunities to
// get one of them subtly wrong.
//
// With a discriminator, a new telemetry type is a new Kind and a registered
// [Receiver]. The transport, the spool, the ingest endpoint, the credential
// check, and the flow control are written once.
type Kind string

const (
	// KindMetrics carries a [collect.Batch]: one collection run's samples.
	KindMetrics Kind = "metrics"
	// KindInventory carries a snapshot of a host's current state — the
	// container list, the unit list, the listening sockets.
	KindInventory Kind = "inventory"
	// KindEvents carries one event observed on an agent's local event bus.
	KindEvents Kind = "events"
)

// Class determines how a payload must be delivered.
//
// This is the axis that actually matters, and it has exactly two values. Every
// telemetry type Atlas will carry falls into one of them, so the transport
// needs two behaviours rather than one per payload type.
type Class int

const (
	// ClassStream is append-only history: ordered, deduplicated on envelope
	// ID, and spooled through an outage. Losing one envelope loses a piece of
	// the record permanently, because nothing later supersedes it. Metrics,
	// events, audit records and traces are all streams.
	ClassStream Class = iota

	// ClassSnapshot is current state: latest-wins, replaced on arrival, and
	// deliberately *not* spooled. A snapshot produced during an outage has no
	// value once a newer one exists, so buffering it wastes disk to deliver
	// something stale. Inventory, cost attribution and posture findings are
	// snapshots.
	ClassSnapshot
)

// String names the class for logs and errors.
func (c Class) String() string {
	switch c {
	case ClassStream:
		return "stream"
	case ClassSnapshot:
		return "snapshot"
	default:
		return fmt.Sprintf("Class(%d)", int(c))
	}
}

// Payload is the body of an envelope.
//
// Implementations are values, not pointers, and must be safe to read
// concurrently: an envelope may be spooled, retried, and logged from several
// goroutines. A payload that shares mutable state with its producer will have
// its history rewritten under it — see [collect.CloneLabels] for the same
// hazard one level down.
type Payload interface {
	// Kind identifies the payload type. It must be constant and registered.
	Kind() Kind
	// Validate reports whether the payload is well formed. The transport
	// calls it before delivery, so a malformed payload is refused at the
	// boundary rather than found in storage later.
	Validate() error
}

// Metrics is the payload carrying one collection run.
//
// It wraps [collect.Batch] rather than [collect.Batch] implementing [Payload]
// directly, because collect deliberately depends on nothing but the platform
// packages — that is what lets a collector be unit-tested with no
// infrastructure at all, and it is worth a one-field wrapper to keep.
type Metrics struct {
	Batch collect.Batch `json:"batch"`
}

// Kind implements [Payload].
func (Metrics) Kind() Kind { return KindMetrics }

// Validate implements [Payload].
func (m Metrics) Validate() error {
	if m.Batch.CollectorID == "" {
		return errs.New(errs.CodeInvalidArgument, "metrics payload has no collector ID")
	}
	return nil
}

// MetricsOf returns the batch an envelope carries.
//
// The boolean is false for any other kind. A caller that has been handed an
// envelope by the [Router] should treat false as a programming error rather
// than a runtime condition: the router dispatches by kind, so a metrics
// receiver cannot legitimately see anything else.
func MetricsOf(env Envelope) (collect.Batch, bool) {
	m, ok := env.Payload.(Metrics)
	if !ok {
		return collect.Batch{}, false
	}
	return m.Batch, true
}

// Events carries one occurrence from an agent's local event bus, forwarded
// to the control plane so it survives a restart and can be alerted on and
// correlated fleet-wide — see internal/platform/eventbus and
// internal/core/eventstore.
type Events struct {
	Event eventbus.Event `json:"event"`
}

// Kind implements [Payload].
func (Events) Kind() Kind { return KindEvents }

// Validate implements [Payload].
func (e Events) Validate() error {
	if !e.Event.Topic.Valid() {
		return errs.New(errs.CodeInvalidArgument, "events payload has an invalid topic")
	}
	return nil
}

// EventsOf returns the event an envelope carries. The boolean is false for
// any other kind.
func EventsOf(env Envelope) (eventbus.Event, bool) {
	e, ok := env.Payload.(Events)
	if !ok {
		return eventbus.Event{}, false
	}
	return e.Event, true
}

// Decoder builds a payload from its JSON body.
//
// Each kind supplies its own, rather than the transport reflecting over a
// registered example. Reflection would work, but it moves a decoding mistake
// from compile time to the ingest path, which is the one place in the system
// where a silent failure loses data that cannot be re-collected.
type Decoder func(json.RawMessage) (Payload, error)

// kindSpec is what registration records about a kind.
type kindSpec struct {
	class  Class
	decode Decoder
}

var (
	kindsMu sync.RWMutex
	kinds   = map[Kind]kindSpec{}
)

// RegisterKind declares a payload kind, its delivery class, and its decoder.
//
// Registration is the single place a telemetry type is introduced. It panics
// on an empty or duplicate kind because both are programming errors that
// would otherwise surface as an undecodable envelope in production.
func RegisterKind(k Kind, class Class, decode Decoder) {
	if k == "" {
		panic("transport: RegisterKind with an empty kind")
	}
	if decode == nil {
		panic("transport: RegisterKind with no decoder for kind " + string(k))
	}

	kindsMu.Lock()
	defer kindsMu.Unlock()
	if _, exists := kinds[k]; exists {
		panic("transport: kind already registered: " + string(k))
	}
	kinds[k] = kindSpec{class: class, decode: decode}
}

func init() {
	RegisterKind(KindMetrics, ClassStream, func(raw json.RawMessage) (Payload, error) {
		var m Metrics
		if len(raw) == 0 {
			return m, nil
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	})
	RegisterKind(KindEvents, ClassStream, func(raw json.RawMessage) (Payload, error) {
		var e Events
		if len(raw) == 0 {
			return e, nil
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		return e, nil
	})
}

// ClassOf reports how a kind must be delivered.
//
// Unregistered kinds report ClassStream: the conservative answer. Treating an
// unknown payload as a snapshot would let the transport discard it on the
// first failure, and silently dropping data Atlas was asked to carry is the
// worse of the two mistakes.
func ClassOf(k Kind) Class {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	spec, ok := kinds[k]
	if !ok {
		return ClassStream
	}
	return spec.class
}

// KnownKinds lists registered kinds in a stable order, for diagnostics.
func KnownKinds() []Kind {
	kindsMu.RLock()
	defer kindsMu.RUnlock()

	out := make([]Kind, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// decodePayload builds a payload of kind k from its JSON body.
func decodePayload(k Kind, raw json.RawMessage) (Payload, error) {
	kindsMu.RLock()
	spec, ok := kinds[k]
	kindsMu.RUnlock()

	if !ok {
		// An unknown kind is what an older control plane sees when a newer
		// agent sends something it has never heard of. The error names both
		// the kind and what is understood, so the diagnosis is in the log
		// rather than in a debugger.
		return nil, errs.New(errs.CodeInvalidArgument, "unknown telemetry kind %q", k).
			WithDetail("kind", string(k)).
			WithDetail("known", fmt.Sprint(KnownKinds()))
	}

	payload, err := spec.decode(raw)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInvalidArgument, "could not decode a %s payload", k).
			WithDetail("kind", string(k))
	}
	if payload == nil {
		return nil, errs.New(errs.CodeInternal, "the decoder for kind %q returned no payload", k).
			WithDetail("kind", string(k))
	}
	return payload, nil
}

// wireEnvelope is the JSON form of an [Envelope].
//
// The payload is held as raw JSON so that decoding can be a two-step: read the
// kind, then decode the body into the type that kind names. A single-step
// decode into an interface field is not possible, and a struct with one
// optional field per kind would have to change every time a telemetry type is
// added — which is the coupling this whole mechanism exists to remove.
type wireEnvelope struct {
	ID      string          `json:"id"`
	Origin  Origin          `json:"origin"`
	Kind    Kind            `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	SentAt  time.Time       `json:"sent_at"`
}

// MarshalJSON implements [json.Marshaler].
func (e Envelope) MarshalJSON() ([]byte, error) {
	const op = "transport.Envelope.MarshalJSON"

	if e.Payload == nil {
		return nil, errs.New(errs.CodeInvalidArgument, "envelope has no payload").WithOp(op)
	}
	body, err := json.Marshal(e.Payload)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not encode the payload").
			WithOp(op).WithDetail("kind", string(e.Payload.Kind()))
	}

	return json.Marshal(wireEnvelope{
		ID:      e.ID,
		Origin:  e.Origin,
		Kind:    e.Payload.Kind(),
		Payload: body,
		SentAt:  e.SentAt.UTC(),
	})
}

// UnmarshalJSON implements [json.Unmarshaler].
func (e *Envelope) UnmarshalJSON(data []byte) error {
	const op = "transport.Envelope.UnmarshalJSON"

	var wire wireEnvelope
	if err := json.Unmarshal(data, &wire); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "could not decode the envelope").WithOp(op)
	}

	payload, err := decodePayload(wire.Kind, wire.Payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeOf(err), "could not decode the envelope").WithOp(op)
	}

	e.ID = wire.ID
	e.Origin = wire.Origin
	e.Payload = payload
	e.SentAt = wire.SentAt
	return nil
}
