package agent

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"os"
	"testing"

	"github.com/hexane/atlas/internal/platform/pki"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// testPeerID returns a fresh, valid, unique Peer ID. resolvePeerIDConflicts
// and buildAgentOpsLookup only ever compare peer.ID values for equality, so
// a real (but otherwise meaningless) key is sufficient — the seed argument
// exists only to make call sites self-documenting, not for determinism.
func testPeerID(t *testing.T, _ string) peer.ID {
	t.Helper()
	priv, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("derive peer id: %v", err)
	}
	return pid
}

// Two relationships targeting the same libp2p peer id is a configuration/
// security error and must fail Agent startup outright — not a warning, not
// a "last one wins". See the approved Phase 3 decision on duplicate peer ids.
func TestResolvePeerIDConflictsRejectsDuplicateAcrossRelationships(t *testing.T) {
	t.Parallel()
	pid := testPeerID(t, "shared-cp")

	relConfigs := map[string]relationshipConfig{
		"production": {id: "production", RelationshipBootstrap: RelationshipBootstrap{
			Transport: "libp2p", LibP2PRelayAddr: "/ip4/1.2.3.4/tcp/4103/p2p/" + testPeerID(t, "relay").String(),
			LibP2PServerPeerID: pid.String(),
		}},
		"development": {id: "development", RelationshipBootstrap: RelationshipBootstrap{
			Transport: "libp2p", LibP2PRelayAddr: "/ip4/1.2.3.4/tcp/4103/p2p/" + testPeerID(t, "relay").String(),
			LibP2PServerPeerID: pid.String(),
		}},
	}

	_, _, err := resolvePeerIDConflicts(relConfigs)
	if err == nil {
		t.Fatal("expected resolvePeerIDConflicts to reject two relationships resolving to the same peer id")
	}
}

func TestResolvePeerIDConflictsAcceptsDistinctPeerIDs(t *testing.T) {
	t.Parallel()
	relayID := testPeerID(t, "relay")
	relConfigs := map[string]relationshipConfig{
		"production": {id: "production", RelationshipBootstrap: RelationshipBootstrap{
			Transport: "libp2p", LibP2PRelayAddr: "/ip4/1.2.3.4/tcp/4103/p2p/" + relayID.String(),
			LibP2PServerPeerID: testPeerID(t, "prod-cp").String(),
		}},
		"development": {id: "development", RelationshipBootstrap: RelationshipBootstrap{
			Transport: "libp2p", LibP2PRelayAddr: "/ip4/1.2.3.4/tcp/4103/p2p/" + relayID.String(),
			LibP2PServerPeerID: testPeerID(t, "dev-cp").String(),
		}},
		"default": {id: "default", RelationshipBootstrap: RelationshipBootstrap{Transport: "https", ControlPlaneURL: "https://127.0.0.1:8443"}},
	}

	peerIDs, needsHost, err := resolvePeerIDConflicts(relConfigs)
	if err != nil {
		t.Fatalf("resolvePeerIDConflicts: %v", err)
	}
	if !needsHost {
		t.Error("needsP2PHost = false, want true (two relationships use the libp2p transport)")
	}
	if len(peerIDs) != 2 {
		t.Errorf("resolved %d peer ids, want 2 (the https relationship must not appear)", len(peerIDs))
	}
}

func TestResolvePeerIDConflictsNoLibP2PRelationshipsNeedsNoHost(t *testing.T) {
	t.Parallel()
	relConfigs := map[string]relationshipConfig{
		"default": {id: "default", RelationshipBootstrap: RelationshipBootstrap{Transport: "https", ControlPlaneURL: "https://127.0.0.1:8443"}},
	}
	_, needsHost, err := resolvePeerIDConflicts(relConfigs)
	if err != nil {
		t.Fatalf("resolvePeerIDConflicts: %v", err)
	}
	if needsHost {
		t.Error("needsP2PHost = true, want false for an all-https relationship set")
	}
}

// A server peer id that is one character short of a real one (the classic
// copy/paste truncation) is not decodable: resolvePeerIDConflicts must skip
// the relationship rather than register a bogus peer id, while still
// reporting that a libp2p host is needed — the relationship then fails on
// its own in bootstrapRelationship, where peer.Decode runs again.
func TestResolveLibP2PServerPeerIDRejectsTruncatedPeerID(t *testing.T) {
	t.Parallel()
	full := testPeerID(t, "local-cp").String()
	truncated := full[:len(full)-1]

	relayAddr := "/ip4/1.2.3.4/tcp/4103/p2p/" + testPeerID(t, "relay").String()
	relCfg := relationshipConfig{id: "local", RelationshipBootstrap: RelationshipBootstrap{
		Transport: "libp2p", LibP2PRelayAddr: relayAddr, LibP2PServerPeerID: truncated,
	}}

	if _, ok := resolveLibP2PServerPeerID(relCfg); ok {
		t.Fatalf("resolveLibP2PServerPeerID accepted truncated peer id %q", truncated)
	}
	if _, err := peer.Decode(truncated); err == nil {
		t.Fatalf("peer.Decode accepted truncated peer id %q", truncated)
	}

	relCfg.LibP2PServerPeerID = full
	if _, ok := resolveLibP2PServerPeerID(relCfg); !ok {
		t.Fatalf("resolveLibP2PServerPeerID rejected valid peer id %q", full)
	}

	relCfg.LibP2PServerPeerID = truncated
	peerIDs, needsHost, err := resolvePeerIDConflicts(map[string]relationshipConfig{"local": relCfg})
	if err != nil {
		t.Fatalf("resolvePeerIDConflicts: %v", err)
	}
	if !needsHost {
		t.Error("needsP2PHost = false, want true (the relationship still requests the libp2p transport)")
	}
	if len(peerIDs) != 0 {
		t.Errorf("registered %d peer ids, want 0 (an undecodable peer id must not enter the demux map)", len(peerIDs))
	}
}

// --- AgentOps demux: each inbound stream is decided on the Peer ID it
// arrived from, never on insertion order or on holding a credential --------

func testRelationshipRuntime(id string) *relationshipRuntime {
	return &relationshipRuntime{
		id:     id,
		relCfg: relationshipConfig{id: id},
		// A distinct pointer per call is all these tests need to prove "not
		// the same CA" — content is irrelevant here.
		caCert: &x509.Certificate{},
		holder: &credentialHolder{},
	}
}

func TestBuildAgentOpsLookupRoutesByPeerIDNotByInsertionOrder(t *testing.T) {
	t.Parallel()
	prodPeer, devPeer := testPeerID(t, "prod-cp"), testPeerID(t, "dev-cp")
	stranger := testPeerID(t, "stranger")

	relationships := map[string]*relationshipRuntime{
		"production":  testRelationshipRuntime("production"),
		"development": testRelationshipRuntime("development"),
	}
	peerIDToRelationship := map[peer.ID]string{prodPeer: "production", devPeer: "development"}

	lookup := buildAgentOpsLookup(relationships, peerIDToRelationship)

	if _, ok := lookup(prodPeer); !ok {
		t.Error("lookup(prodPeer) not ok")
	}
	if _, ok := lookup(devPeer); !ok {
		t.Error("lookup(devPeer) not ok")
	}
	if _, ok := lookup(stranger); ok {
		t.Error("lookup accepted a peer id belonging to no relationship")
	}
}

func TestBuildAgentOpsLookupRejectsUnknownPeer(t *testing.T) {
	t.Parallel()
	lookup := buildAgentOpsLookup(map[string]*relationshipRuntime{}, map[peer.ID]string{})
	if _, ok := lookup(testPeerID(t, "stranger")); ok {
		t.Error("lookup accepted a peer id it has no relationship record for")
	}
}

// A relationship whose peer id was known statically (from config) but which
// did not survive bootstrap must not serve AgentOps — the Agent has no
// working relationship with that control plane.
func TestBuildAgentOpsLookupRejectsPeerOfARelationshipThatFailedBootstrap(t *testing.T) {
	t.Parallel()
	devPeer := testPeerID(t, "dev-cp")
	// "development" is known in the peer map (config resolved successfully)
	// but absent from relationships (bootstrap failed and it was dropped).
	peerIDToRelationship := map[peer.ID]string{devPeer: "development"}
	lookup := buildAgentOpsLookup(map[string]*relationshipRuntime{}, peerIDToRelationship)

	if _, ok := lookup(devPeer); ok {
		t.Error("lookup served a peer belonging to a relationship that never finished bootstrapping")
	}
}

func TestBuildAgentOpsLookupHonorsPerRelationshipAuthorization(t *testing.T) {
	t.Parallel()
	prodPeer := testPeerID(t, "prod-cp")
	rt := testRelationshipRuntime("production")
	rt.relCfg.AgentOpsContainerLogsDisabled = true

	lookup := buildAgentOpsLookup(
		map[string]*relationshipRuntime{"production": rt},
		map[peer.ID]string{prodPeer: "production"},
	)

	allow, ok := lookup(prodPeer)
	if !ok {
		t.Fatal("lookup(prodPeer) not ok")
	}
	if allow {
		t.Error("allowContainerLogs = true, want false: this relationship has AgentOpsContainerLogsDisabled set")
	}
}

// A libp2p relationship holds no certificate at all now (no enrollment), so
// the demux must still serve it — authorization is the peer id, never the
// presence of a credential.
func TestBuildAgentOpsLookupServesRelationshipWithNoCredential(t *testing.T) {
	t.Parallel()
	prodPeer := testPeerID(t, "prod-cp")
	rt := &relationshipRuntime{id: "production", relCfg: relationshipConfig{id: "production"}}

	lookup := buildAgentOpsLookup(
		map[string]*relationshipRuntime{"production": rt},
		map[peer.ID]string{prodPeer: "production"},
	)

	allow, ok := lookup(prodPeer)
	if !ok || !allow {
		t.Fatalf("lookup = (%v, %v), want (true, true) for a credential-free libp2p relationship", allow, ok)
	}
}

// --- Finding B: relationship resolution failures are isolated, not fatal
// to Agent.New as a whole -----------------------------------------------

// seedHealthyRelationship writes a self-consistent, already-enrolled
// relationship directly to disk — CA, leaf cert/key, and a persisted
// relationship.json — so bootstrap() takes its "already enrolled" fast path
// with no network access at all. This is what lets these tests exercise the
// real Agent.New() without a real control plane.
func seedHealthyRelationship(t *testing.T, dataDir, id, nodeID string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dataDir, err)
	}

	ca, err := pki.New("test-ca-" + id)
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}
	if err := savePinnedCA(dataDir, ca.Cert()); err != nil {
		t.Fatalf("savePinnedCA: %v", err)
	}

	csrDER, key, err := pki.NewCSR(nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	csr, err := pki.ParseCSR(csrDER)
	if err != nil {
		t.Fatalf("ParseCSR: %v", err)
	}
	leaf, err := ca.IssueLeaf(csr, nodeID)
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}
	if err := pki.SaveLeaf(dataDir, leaf.Raw, key); err != nil {
		t.Fatalf("SaveLeaf: %v", err)
	}

	relCfg := relationshipConfig{
		id: id, dataDir: dataDir,
		RelationshipBootstrap: RelationshipBootstrap{ControlPlaneURL: "https://127.0.0.1:1", Transport: "https"},
	}
	if err := persistRelationshipConfig(relCfg); err != nil {
		t.Fatalf("persistRelationshipConfig: %v", err)
	}
}

// seedCorruptedRelationship writes an unparsable relationship.json — the
// exact fixture Finding B is about. No cert/CA files are needed: resolution
// fails before bootstrap() is ever reached.
func seedCorruptedRelationship(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dataDir, err)
	}
	if err := os.WriteFile(relationshipJSONPath(dataDir), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt relationship.json: %v", err)
	}
}

// Test case 1: healthy production + corrupted development => New succeeds,
// production is available, development is not.
func TestNewHealthyProductionSurvivesCorruptedDevelopment(t *testing.T) {
	shrinkBootstrapBackoff(t) // bounds the "default" relationship's own doomed retry (no token) to a couple seconds
	agentDataDir := t.TempDir()
	const nodeID = "test-node-shared"

	seedHealthyRelationship(t, dataDirFor(agentDataDir, "production"), "production", nodeID)
	seedCorruptedRelationship(t, dataDirFor(agentDataDir, "development"))
	t.Setenv("ATLAS_AGENT_RELATIONSHIPS", "production,development")

	a, err := New(context.Background(), Config{DataDir: agentDataDir, NodeID: nodeID}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if _, ok := a.relationships["production"]; !ok {
		t.Error("healthy production relationship did not survive a corrupted sibling")
	}
	if _, ok := a.relationships["development"]; ok {
		t.Error("corrupted development relationship should have been dropped, not started")
	}
}

// Test case 2: the mirror image — corrupted production + healthy
// development => New succeeds, development is available, production is not.
func TestNewHealthyDevelopmentSurvivesCorruptedProduction(t *testing.T) {
	shrinkBootstrapBackoff(t)
	agentDataDir := t.TempDir()
	const nodeID = "test-node-shared"

	seedCorruptedRelationship(t, dataDirFor(agentDataDir, "production"))
	seedHealthyRelationship(t, dataDirFor(agentDataDir, "development"), "development", nodeID)
	t.Setenv("ATLAS_AGENT_RELATIONSHIPS", "production,development")

	a, err := New(context.Background(), Config{DataDir: agentDataDir, NodeID: nodeID}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if _, ok := a.relationships["development"]; !ok {
		t.Error("healthy development relationship did not survive a corrupted sibling")
	}
	if _, ok := a.relationships["production"]; ok {
		t.Error("corrupted production relationship should have been dropped, not started")
	}
}

// Test case 3: every relationship unusable (all corrupted, default has no
// token) => New fails outright — zero usable relationships is the only
// condition under which it should.
func TestNewFailsWhenEveryRelationshipIsCorrupted(t *testing.T) {
	shrinkBootstrapBackoff(t)
	agentDataDir := t.TempDir()

	seedCorruptedRelationship(t, dataDirFor(agentDataDir, "production"))
	seedCorruptedRelationship(t, dataDirFor(agentDataDir, "development"))
	t.Setenv("ATLAS_AGENT_RELATIONSHIPS", "production,development")

	// "default" has no token and no existing certificate, so it fails
	// bootstrap on its own — every one of the three relationships this
	// process would attempt is unusable.
	_, err := New(context.Background(), Config{DataDir: agentDataDir}, discardLogger())
	if err == nil {
		t.Fatal("New succeeded with zero usable relationships")
	}
}
