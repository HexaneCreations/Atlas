package collect

import (
	"slices"
	"sync"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Registry holds the collectors available to a process.
//
// Registration is a composition-time activity: plugins are detected, they
// contribute their collectors, and the scheduler then runs whatever is
// present. The registry is safe for concurrent use so that a plugin
// discovered later — a Docker daemon that starts after Atlas did — can add
// its collectors without a restart.
type Registry struct {
	mu         sync.RWMutex
	byID       map[string]Collector
	orderAdded []string

	// Streamers are held separately because the scheduler drives them
	// differently, but they share the ID namespace above.
	streamersByID map[string]Streamer
	streamerOrder []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byID:          make(map[string]Collector),
		streamersByID: make(map[string]Streamer),
	}
}

// Register adds a collector.
//
// It fails on an invalid descriptor or a duplicate ID. Duplicate IDs are
// rejected rather than overwritten: two collectors claiming "system.cpu"
// would silently halve the sample rate of whichever lost, and the resulting
// gaps would look like a monitoring outage rather than a configuration bug.
func (r *Registry) Register(c Collector) error {
	const op = "collect.Registry.Register"

	desc := c.Descriptor()
	if err := desc.Validate(); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "cannot register collector").WithOp(op)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byID[desc.ID]; exists {
		return errs.New(errs.CodeAlreadyExists, "collector %q is already registered", desc.ID).
			WithOp(op).WithDetail("collector_id", desc.ID)
	}
	if _, exists := r.streamersByID[desc.ID]; exists {
		return errs.New(errs.CodeAlreadyExists, "streamer %q is already registered", desc.ID).
			WithOp(op).WithDetail("collector_id", desc.ID)
	}
	r.byID[desc.ID] = c
	r.orderAdded = append(r.orderAdded, desc.ID)
	return nil
}

// MustRegister registers a collector and panics on failure.
//
// For use only in composition code where a failure is a programming error
// that should stop the process at startup rather than produce a running
// Atlas with a silently missing collector.
func (r *Registry) MustRegister(c Collector) {
	if err := r.Register(c); err != nil {
		panic("collect: " + err.Error())
	}
}

// Unregister removes a collector. It reports whether one was removed.
func (r *Registry) Unregister(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byID[id]; !ok {
		return false
	}
	delete(r.byID, id)
	r.orderAdded = slices.DeleteFunc(r.orderAdded, func(s string) bool { return s == id })
	return true
}

// Get returns the collector with the given ID.
func (r *Registry) Get(id string) (Collector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byID[id]
	return c, ok
}

// All returns every registered collector in registration order.
//
// Order is preserved so that startup logs and the collectors API list them
// predictably; a map's random iteration order makes a diff between two runs
// unreadable.
func (r *Registry) All() []Collector {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Collector, 0, len(r.byID))
	for _, id := range r.orderAdded {
		out = append(out, r.byID[id])
	}
	return out
}

// Descriptors returns the descriptor of every registered collector, in
// registration order.
func (r *Registry) Descriptors() []Descriptor {
	collectors := r.All()
	out := make([]Descriptor, 0, len(collectors))
	for _, c := range collectors {
		out = append(out, c.Descriptor())
	}
	return out
}

// Len returns the number of registered collectors.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// RegisterStreamer adds a streaming collector.
//
// Streamers and collectors share one ID namespace, so a duplicate across the
// two kinds is rejected just as a duplicate within either would be. They are
// tracked separately because the scheduler drives them differently: one on a
// ticker, the other supervised continuously.
func (r *Registry) RegisterStreamer(s Streamer) error {
	const op = "collect.Registry.RegisterStreamer"

	desc := s.Descriptor()
	if err := desc.Validate(); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "cannot register streamer").WithOp(op)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byID[desc.ID]; exists {
		return errs.New(errs.CodeAlreadyExists, "collector %q is already registered", desc.ID).
			WithOp(op).WithDetail("collector_id", desc.ID)
	}
	if _, exists := r.streamersByID[desc.ID]; exists {
		return errs.New(errs.CodeAlreadyExists, "streamer %q is already registered", desc.ID).
			WithOp(op).WithDetail("collector_id", desc.ID)
	}

	if r.streamersByID == nil {
		r.streamersByID = make(map[string]Streamer)
	}
	r.streamersByID[desc.ID] = s
	r.streamerOrder = append(r.streamerOrder, desc.ID)
	return nil
}

// Streamers returns every registered streamer in registration order.
func (r *Registry) Streamers() []Streamer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Streamer, 0, len(r.streamersByID))
	for _, id := range r.streamerOrder {
		out = append(out, r.streamersByID[id])
	}
	return out
}

// StreamerDescriptors returns the descriptor of every streamer.
func (r *Registry) StreamerDescriptors() []Descriptor {
	streamers := r.Streamers()
	out := make([]Descriptor, 0, len(streamers))
	for _, s := range streamers {
		out = append(out, s.Descriptor())
	}
	return out
}
