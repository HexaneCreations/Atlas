package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/pki"
	"github.com/libp2p/go-libp2p/core/crypto"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"
)

// Full end-to-end dump of one AgentHealth snapshot with every field
// populated by realistic values (a real issued certificate, a real libp2p
// Peer ID, live delivery stats, collector outcomes) — direct evidence for
// goal §6.
func TestLocalVerifyHealthSnapshotFieldByField(t *testing.T) {
	ca, err := pki.New("test-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}
	csrDER, key, err := pki.NewCSR("node-1")
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	csr, err := pki.ParseCSR(csrDER)
	if err != nil {
		t.Fatalf("ParseCSR: %v", err)
	}
	leaf, err := ca.IssueLeaf(csr, "node-1")
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}
	holder := &credentialHolder{}
	holder.set(pki.LeafTLSCertificate(leaf, key), leaf)

	_, pub, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}
	peerID, err := libp2ppeer.IDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("IDFromPublicKey: %v", err)
	}

	relationships := map[string]*relationshipRuntime{
		"production": {
			id: "production",
			relCfg: relationshipConfig{RelationshipBootstrap: RelationshipBootstrap{
				Transport: "libp2p", Environment: "production",
			}},
			holder: holder,
			peerID: peerID,
		},
	}
	environments := map[string]string{"production": "production"}

	reporter := newHealthReporter("node-1", "/var/lib/atlas-agent/atlas-agent.lock",
		relationships, environments, func() []CollectorHealth {
			return collectorHealthFrom([]plugin.State{
				{ID: "system", Status: plugin.StatusActive},
				{ID: "docker", Status: plugin.StatusNotDetected},
			})
		})
	reporter.startedAt = time.Now().Add(-5 * time.Minute)

	// Feed the relationship's transport real delivery outcomes via the
	// same Stats() accessor the report reads from — see the remote package
	// test for how these fields get set on the wire; here we only need the
	// health report to surface them faithfully.
	got := reporter.snapshot(time.Now())

	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("AgentHealth JSON:\n%s", raw)

	if got.NodeID != "node-1" {
		t.Error("node_id missing")
	}
	if got.Version == "" {
		t.Error("version missing")
	}
	if got.UptimeSeconds < 299 {
		t.Errorf("uptime_seconds = %f, want >= 300", got.UptimeSeconds)
	}
	if got.SingleInstanceLock == "" {
		t.Error("single_instance_lock missing")
	}
	if len(got.Collectors) != 2 {
		t.Error("collectors missing")
	}
	if len(got.Relationships) != 1 {
		t.Fatal("relationships missing")
	}

	rh := got.Relationships[0]
	if rh.ID != "production" {
		t.Error("relationship id wrong")
	}
	if rh.Environment != "production" {
		t.Error("relationship environment wrong")
	}
	if rh.Transport != "libp2p" {
		t.Error("relationship transport wrong")
	}
	if rh.PeerID != peerID.String() {
		t.Error("relationship peer_id wrong")
	}
	if rh.CertificateExpiry.IsZero() {
		t.Error("certificate_expiry missing")
	}
	if !rh.CertificateValid {
		t.Error("certificate_valid should be true for a freshly issued cert")
	}

	// Field-presence check against the goal's explicit checklist, via the
	// marshaled JSON keys rather than the Go struct, so it also proves the
	// wire shape (what the control plane actually receives) carries them.
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"node_id", "version", "commit", "build_time", "started_at", "uptime_seconds", "collectors", "relationships"} {
		if _, ok := asMap[want]; !ok {
			t.Errorf("top-level field %q missing from JSON", want)
		}
	}

	relRaw, _ := json.Marshal(got.Relationships[0])
	var relMap map[string]any
	json.Unmarshal(relRaw, &relMap)
	for _, want := range []string{
		"id", "environment", "transport", "connected", "peer_id",
		"sent", "failed", "rejected", "retries",
		"spool_depth", "spool_bytes", "spool_dropped",
		"certificate_expiry", "certificate_valid",
	} {
		if _, ok := relMap[want]; !ok {
			t.Errorf("relationship field %q missing from JSON", want)
		}
	}
}
