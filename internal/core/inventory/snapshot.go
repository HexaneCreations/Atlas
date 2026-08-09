package inventory

import (
	"context"
	"encoding/json"
	"time"
)

// Subject names one kind of inventory an agent pushes: the container list,
// the systemd unit list, the listening sockets. It is the key a snapshot is
// stored and retrieved under, alongside a node id.
type Subject string

const (
	SubjectProcesses    Subject = "processes"
	SubjectServices     Subject = "services"
	SubjectServiceGraph Subject = "service_graph"
	SubjectCronJobs     Subject = "cron_jobs"
	SubjectPorts        Subject = "ports"
	SubjectMounts       Subject = "mounts"
	SubjectContainers   Subject = "containers"
)

// Meta is the provenance of an inventory response: whose host, how current.
//
// Every inventory response carries this. Local inventory is live — the
// process list is what is running this millisecond, because it came straight
// from the host. Remote inventory is a snapshot, at most a push interval old.
// Presenting the second as the first is exactly the class of lie this project
// has spent every milestone eliminating (a frozen Docker healthcheck reported
// as current, a fabricated fleet summary), so freshness is not something a
// caller can forget to check: every response states it.
type Meta struct {
	NodeID     string    `json:"node_id"`
	ObservedAt time.Time `json:"observed_at"`
	// Live is true only when read directly from the host this instant. False
	// means "as of ObservedAt", however recent that happens to be.
	Live bool `json:"live"`
}

// LiveMeta builds the provenance for inventory read straight from the local
// host.
func LiveMeta(nodeID string) Meta {
	return Meta{NodeID: nodeID, ObservedAt: time.Now(), Live: true}
}

// Snapshot wraps typed inventory data with its provenance. T is whatever the
// caller's decoder produces — []process.Process, []docker.Container, and so
// on; this package deliberately does not import those, since core must not
// depend on plugin (see docs/adr/0002-modular-monolith.md).
type Snapshot[T any] struct {
	Data T
	Meta Meta
}

// StoredSnapshot is the persisted, still-encoded form of a pushed inventory
// snapshot. Decoding into a concrete type happens at the API layer, which
// already knows which plugin type a subject corresponds to — this package
// stays generic so it does not need to learn about every plugin's types.
type StoredSnapshot struct {
	NodeID      string
	Subject     Subject
	ObservedAt  time.Time
	ReceivedAt  time.Time
	ContentHash string
	Data        json.RawMessage
}

// Store is the storage surface for latest-only inventory snapshots.
//
// Implementations replace a (node, subject) row in place — see
// docs/architecture/agent-design.md sec 1 for why inventory is never stored
// as history the way metrics are.
type Store interface {
	// Put upserts a node's snapshot for a subject.
	Put(ctx context.Context, s StoredSnapshot) error
	// Get returns the current snapshot for (node, subject). Returns a
	// [errs.CodeNotFound] error if no snapshot has ever been pushed for it.
	Get(ctx context.Context, nodeID string, subject Subject) (StoredSnapshot, error)
	// HasReported reports whether a node has pushed *any* inventory subject.
	// This is what distinguishes "this node's agent does not report
	// containers" (Docker absent there) from "this node has never reported
	// inventory at all" (no agent, or one that has not pushed yet) — the two
	// different remote-inventory failures a caller must not conflate.
	HasReported(ctx context.Context, nodeID string) (bool, error)
}
