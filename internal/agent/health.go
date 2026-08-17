package agent

import (
	"context"
	"path/filepath"
	"time"

	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/build"
)

// LockPath is the single-instance lock guarding an agent data directory.
// Defined here so the process that acquires it and the health report that
// names it cannot disagree.
func LockPath(dataDir string) string {
	return filepath.Join(dataDir, "atlas-agent.lock")
}

// AgentHealth is the agent's report on its own operation, keyed by the
// relationship it is reported to.
//
// Health is per node *and* relationship, never a single global record: one
// agent serves several control planes independently, and a relationship that
// cannot deliver must be visible as broken to the others rather than hidden
// behind an aggregate. Each control plane sees the full set, so an operator
// on one can tell "this agent is failing everywhere" from "this agent is
// failing only towards me".
type AgentHealth struct {
	NodeID string `json:"node_id"`
	// Version, Commit and BuildTime identify the binary that produced this
	// report. Commit and BuildTime are empty on an unstamped build.
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`

	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	ObservedAt    time.Time `json:"observed_at"`

	// SingleInstanceLock is the data directory whose exclusive lock this
	// process holds. Its presence is the proof that no second agent is
	// writing the same state.
	SingleInstanceLock string `json:"single_instance_lock,omitempty"`

	Collectors    []CollectorHealth    `json:"collectors,omitempty"`
	Relationships []RelationshipHealth `json:"relationships"`
}

// CollectorHealth is one plugin's activation outcome. A plugin that is
// absent on this host ("not_detected") is normal and distinct from one that
// failed, which is not.
type CollectorHealth struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// RelationshipHealth is one control-plane relationship's delivery state.
type RelationshipHealth struct {
	ID          string `json:"id"`
	Environment string `json:"environment,omitempty"`
	// Transport is "https" or "libp2p".
	Transport string `json:"transport"`
	// Connected reflects the most recent delivery attempt, not a held
	// connection: the agent is a client, so "connected" only ever means
	// "the last attempt succeeded".
	Connected bool `json:"connected"`
	// PeerID is the control plane's libp2p Peer ID, empty on the HTTPS
	// transport. The agent's own private keys are never reported.
	PeerID string `json:"peer_id,omitempty"`

	Sent     uint64 `json:"sent"`
	Failed   uint64 `json:"failed"`
	Rejected uint64 `json:"rejected"`
	Retries  uint64 `json:"retries"`

	SpoolDepth   int    `json:"spool_depth"`
	SpoolBytes   int64  `json:"spool_bytes"`
	SpoolDropped uint64 `json:"spool_dropped"`

	LastSuccess       time.Time `json:"last_success,omitzero"`
	LastFailure       time.Time `json:"last_failure,omitzero"`
	LastFailureReason string    `json:"last_failure_reason,omitempty"`

	// CertificateExpiry is when this relationship's client certificate stops
	// being accepted. An agent whose renewal loop is silently failing looks
	// healthy until this passes, which is exactly why it is reported.
	CertificateExpiry time.Time `json:"certificate_expiry,omitzero"`
	CertificateValid  bool      `json:"certificate_valid"`
}

// healthReporter builds the agent's self-health snapshot on demand.
type healthReporter struct {
	nodeID    string
	lockPath  string
	startedAt time.Time

	relationships map[string]*relationshipRuntime
	environments  map[string]string
	collectors    func() []CollectorHealth
}

func newHealthReporter(nodeID, lockPath string, relationships map[string]*relationshipRuntime,
	environments map[string]string, collectors func() []CollectorHealth) *healthReporter {
	return &healthReporter{
		nodeID: nodeID, lockPath: lockPath, startedAt: time.Now(),
		relationships: relationships, environments: environments, collectors: collectors,
	}
}

func (h *healthReporter) snapshot(now time.Time) AgentHealth {
	info := build.Current()

	out := AgentHealth{
		NodeID:             h.nodeID,
		Version:            info.Version,
		Commit:             info.Commit,
		BuildTime:          info.BuildTime,
		StartedAt:          h.startedAt,
		UptimeSeconds:      now.Sub(h.startedAt).Seconds(),
		ObservedAt:         now,
		SingleInstanceLock: h.lockPath,
	}
	if h.collectors != nil {
		out.Collectors = h.collectors()
	}

	for id, rt := range h.relationships {
		rh := RelationshipHealth{
			ID:          id,
			Environment: h.environments[id],
			Transport:   rt.relCfg.Transport,
		}
		if rt.peerID != "" {
			rh.PeerID = rt.peerID.String()
		}
		if rt.transport != nil {
			stats := rt.transport.Stats()
			rh.Sent, rh.Failed, rh.Rejected, rh.Retries = stats.Sent, stats.Failed, stats.Rejected, stats.Retries
			rh.SpoolDepth, rh.SpoolBytes, rh.SpoolDropped = stats.Spooled, stats.SpooledBytes, stats.Dropped
			rh.LastSuccess, rh.LastFailure = stats.LastSuccess, stats.LastFailure
			rh.LastFailureReason = stats.LastFailureReason
			rh.Connected = stats.LastSuccess.After(stats.LastFailure)
		}
		if rt.holder != nil {
			if leaf := rt.holder.current(); leaf != nil {
				rh.CertificateExpiry = leaf.NotAfter
				rh.CertificateValid = now.Before(leaf.NotAfter) && now.After(leaf.NotBefore)
			}
		}
		out.Relationships = append(out.Relationships, rh)
	}
	return out
}

// healthSource exposes the reporter as an inventory subject, so self-health
// travels the same validated, deduplicated, per-relationship path as every
// other snapshot instead of needing a channel of its own.
func healthSource(reporter *healthReporter) inventorySource {
	return inventorySource{coreinventory.SubjectAgentHealth, func(context.Context) (any, error) {
		return reporter.snapshot(time.Now()), nil
	}}
}

func collectorHealthFrom(states []plugin.State) []CollectorHealth {
	out := make([]CollectorHealth, 0, len(states))
	for _, st := range states {
		out = append(out, CollectorHealth{ID: st.ID, Status: string(st.Status), Error: st.Error})
	}
	return out
}
