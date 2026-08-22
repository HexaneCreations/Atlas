//go:build integration

// Proves the libp2p transport (docs/adr/0012-connect-by-identity.md) carries
// the real Agent collection pipeline end to end: a real internal/agent.Agent
// pushes metrics, inventory and events over a real libp2p stream into a real
// fleetPipeline listener, with no enrollment, no token and no certificate
// anywhere in the path. Authentication is the Noise handshake; authorization
// is the agent_peers record registered for the Agent's Peer ID before it
// starts (see authorizeAgentPeer).
//
// This is the automated (loopback) half of the verification. The physical
// Mac-behind-NAT -> reachable-Linux-server half is run manually — see the
// deployment report for exact commands and results.
package app_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/hexane/atlas/internal/agent"
	"github.com/hexane/atlas/internal/app"
	corefleet "github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/hexane/atlas/internal/platform/log"
	storagefleet "github.com/hexane/atlas/internal/storage/fleet"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bootLibP2PFleetServer starts a real Atlas control plane with the fleet
// listener enabled on both HTTPS and libp2p, and returns it alongside the
// libp2p multiaddr an agent dials to reach it. relayAddr, when non-empty,
// makes the control plane reserve a slot on that relay instead of relying
// on its own (here, always reachable on loopback) listen address — the
// returned multiaddr is then the circuit address through the relay.
func bootLibP2PFleetServer(t *testing.T, relayAddr string) (*app.App, string) {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}

	cfg := config.Default()
	cfg.Server.Port = 0
	cfg.Database.Host = parsed.ConnConfig.Host
	cfg.Database.Port = int(parsed.ConnConfig.Port)
	cfg.Database.Name = parsed.ConnConfig.Database
	cfg.Database.User = parsed.ConnConfig.User
	cfg.Database.Password = parsed.ConnConfig.Password
	cfg.Database.SSLMode = "disable"
	cfg.Database.MigrateOnStart = true

	cfg.Fleet.Enabled = true
	cfg.Fleet.Host = "127.0.0.1"
	cfg.Fleet.Port = freeTCPPort(t)
	cfg.Fleet.DataDir = t.TempDir()
	cfg.Fleet.LibP2PEnabled = true
	cfg.Fleet.LibP2PListenAddrs = []string{"/ip4/127.0.0.1/tcp/0"}
	cfg.Fleet.LibP2PRelayAddr = relayAddr

	if err := cfg.Validate(); err != nil {
		t.Fatalf("test configuration is invalid: %v", err)
	}

	instance, err := app.New(&cfg, log.Discard())
	if err != nil {
		t.Fatalf("app.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() returned %v on shutdown, want a clean exit", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Atlas did not shut down within 30s")
		}
	})

	base := "http://" + waitForBoundAddress(t, instance)
	waitUntilReady(t, base)

	var addrs []string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		addrs = instance.Fleet.LibP2PPeerAddrs()
		if len(addrs) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(addrs) == 0 {
		t.Fatal("fleet libp2p listener never advertised an address")
	}
	return instance, addrs[0]
}

// freeTCPPort asks the kernel for an ephemeral port and releases it
// immediately. config.Fleet.Port, unlike config.Server.Port, must be
// nonzero (fleet.validate rejects 0), so tests need an actual number rather
// than relying on the OS to fill one in at bind time.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestAgentPushesTelemetryOverLibP2PTransport(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	instance, peerAddr := bootLibP2PFleetServer(t, "")
	runAgentAndAssertObserved(t, instance, peerAddr, "libp2p-poc-node-"+id.New())
}

// TestAgentPushesTelemetryOverAtlasRelay proves the Atlas Relay v1 path
// end to end: a real relay host (circuit-relay-v2 service, no Atlas logic),
// a real control plane that reserves a slot on it instead of relying on its
// own listen address, and a real agent that is only ever given the circuit
// address — never a direct one — reaching it and delivering telemetry
// through the relay hop.
func TestAgentPushesTelemetryOverAtlasRelay(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	relayHost, err := libp2ptransport.NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })
	relayAddrs := libp2ptransport.Addrs(relayHost)
	if len(relayAddrs) == 0 {
		t.Fatal("relay advertised no listen addresses")
	}

	instance, circuitAddr := bootLibP2PFleetServer(t, relayAddrs[0])
	if !strings.Contains(circuitAddr, "/p2p-circuit/") {
		t.Fatalf("expected a circuit address, got %q", circuitAddr)
	}
	runAgentAndAssertObserved(t, instance, circuitAddr, "atlas-relay-poc-node-"+id.New())
}

// TestAgentPushesTelemetryOverRendezvousDiscovery proves the "Automatic
// Direct-or-Relay Connectivity" milestone end to end: the Agent is given
// only the Relay's address and the Server's Peer ID — never a manually
// assembled circuit multiaddr — looks the Server's addresses up via the
// Relay's rendezvous registry, and reaches it (here, through the relay,
// since this control plane has no direct listen address the Agent could
// dial) to deliver telemetry.
func TestAgentPushesTelemetryOverRendezvousDiscovery(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	relayHost, err := libp2ptransport.NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })
	libp2ptransport.RegisterRendezvousHandlers(relayHost, libp2ptransport.NewRegistry())
	relayAddrs := libp2ptransport.Addrs(relayHost)
	if len(relayAddrs) == 0 {
		t.Fatal("relay advertised no listen addresses")
	}

	instance, circuitAddr := bootLibP2PFleetServer(t, relayAddrs[0])
	if !strings.Contains(circuitAddr, "/p2p-circuit/") {
		t.Fatalf("expected a circuit address, got %q", circuitAddr)
	}
	serverTarget, err := libp2ptransport.ParseTarget(circuitAddr)
	if err != nil {
		t.Fatalf("parse server peer id out of circuit addr: %v", err)
	}

	runDiscoveryAgentAndAssertObserved(t, instance, relayAddrs[0], serverTarget.ID.String(), "atlas-rendezvous-poc-node-"+id.New())
}

// authorizeAgentPeer registers the Peer ID an agent running out of dataDir
// will dial with, binding it to nodeID and environment — the libp2p
// transport's whole admission mechanism now that there is no enrollment
// token (see migrations/0011_agent_peers.sql). agent.PeerID creates the
// identity file if it does not exist yet, which is why the agent must be
// started with this same dataDir afterwards.
func authorizeAgentPeer(t *testing.T, instance *app.App, dataDir, nodeID, environment string) {
	t.Helper()

	peerID, err := agent.PeerID(dataDir)
	if err != nil {
		t.Fatalf("derive agent peer id: %v", err)
	}
	spec := corefleet.PeerSpec{
		PeerID: peerID, NodeID: nodeID, Environment: environment, Role: corefleet.PeerRoleAgent,
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("peer spec: %v", err)
	}
	repo := storagefleet.NewRepository(instance.Pool.DB())
	if err := repo.RegisterPeer(context.Background(), spec); err != nil {
		t.Fatalf("authorize agent peer: %v", err)
	}
}

// runAgentAndAssertObserved starts a real libp2p-transport agent dialing
// peerAddr and waits for the control plane to observe nodeID.
func runAgentAndAssertObserved(t *testing.T, instance *app.App, peerAddr, nodeID string) {
	t.Helper()

	// The operator flow, exactly: derive the agent's Peer ID from the data
	// dir it will run with, authorize that Peer ID for this node, then start
	// the agent. No enrollment token exists anywhere in this path.
	dataDir := t.TempDir()
	authorizeAgentPeer(t, instance, dataDir, nodeID, "test")

	agentCfg := agent.Config{
		// Unused for dialing under the libp2p transport — routing is by peer
		// id, not address. Its scheme is rewritten to http by the agent: the
		// libp2p stream is already authenticated and encrypted by Noise.
		ControlPlaneURL:    "https://localhost",
		DataDir:            dataDir,
		NodeID:             nodeID,
		Environment:        "test",
		Transport:          "libp2p",
		LibP2PServerAddr:   peerAddr,
		CollectionInterval: 2 * time.Second,
		CollectionTimeout:  2 * time.Second,
		InventoryInterval:  2 * time.Second,
	}

	a, err := agent.New(context.Background(), agentCfg, log.Discard())
	if err != nil {
		t.Fatalf("agent.New() error = %v: the agent could not start on the libp2p transport", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() { agentDone <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-agentDone:
		case <-time.After(15 * time.Second):
			t.Error("agent did not shut down within 15s")
		}
	})

	base := "http://" + waitForBoundAddress(t, instance)
	deadline := time.Now().Add(30 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/nodes/" + agentCfg.NodeID)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				found = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatal("node was never observed by the control plane over the libp2p transport")
	}
}

// runDiscoveryAgentAndAssertObserved is [runAgentAndAssertObserved]'s
// rendezvous-discovery counterpart: the agent is configured with only the
// Relay's address and the Server's Peer ID (LibP2PServerAddr left empty),
// so it must resolve the dial target itself instead of being handed one.
func runDiscoveryAgentAndAssertObserved(t *testing.T, instance *app.App, relayAddr, serverPeerID, nodeID string) {
	t.Helper()

	dataDir := t.TempDir()
	authorizeAgentPeer(t, instance, dataDir, nodeID, "test")

	agentCfg := agent.Config{
		ControlPlaneURL:    "https://localhost", // unused for dialing; see runAgentAndAssertObserved
		DataDir:            dataDir,
		NodeID:             nodeID,
		Environment:        "test",
		Transport:          "libp2p",
		LibP2PRelayAddr:    relayAddr,
		LibP2PServerPeerID: serverPeerID,
		CollectionInterval: 2 * time.Second,
		CollectionTimeout:  2 * time.Second,
		InventoryInterval:  2 * time.Second,
	}

	a, err := agent.New(context.Background(), agentCfg, log.Discard())
	if err != nil {
		t.Fatalf("agent.New() error = %v: rendezvous discovery over libp2p failed", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() { agentDone <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-agentDone:
		case <-time.After(15 * time.Second):
			t.Error("agent did not shut down within 15s")
		}
	})

	base := "http://" + waitForBoundAddress(t, instance)
	deadline := time.Now().Add(30 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/nodes/" + agentCfg.NodeID)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				found = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatal("node was never observed by the control plane over rendezvous-discovered libp2p transport")
	}
}

// #19: the real path this milestone adds — Browser/API -> Server -> Relay ->
// Agent -> Docker -> logs back through Agent -> Server -> Browser/API. Real
// Relay, real rendezvous discovery, real Agent, and the Agent's real local
// Docker daemon (whatever containers actually happen to be running on this
// machine) — not a fake docker.Client, so this proves AgentOps end to end
// against genuine container output, not just the protocol in isolation
// (which internal/core/transport/libp2ptransport/agentops_test.go already
// covers with a fake).
func TestRemoteContainerLogsOverAgentOps(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	relayHost, err := libp2ptransport.NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })
	libp2ptransport.RegisterRendezvousHandlers(relayHost, libp2ptransport.NewRegistry())
	relayAddrs := libp2ptransport.Addrs(relayHost)
	if len(relayAddrs) == 0 {
		t.Fatal("relay advertised no listen addresses")
	}

	instance, circuitAddr := bootLibP2PFleetServer(t, relayAddrs[0])
	serverTarget, err := libp2ptransport.ParseTarget(circuitAddr)
	if err != nil {
		t.Fatalf("parse server peer id out of circuit addr: %v", err)
	}

	nodeID := "agentops-it-node-" + id.New()
	tok, err := corefleet.NewToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	spec := corefleet.TokenSpec{Label: "agentops-it", Environment: "test", AllowedCIDR: "0.0.0.0/0", MaxUses: 1, TTL: time.Hour}
	if err := spec.Validate(); err != nil {
		t.Fatalf("token spec: %v", err)
	}
	repo := storagefleet.NewRepository(instance.Pool.DB())
	if err := repo.CreateToken(context.Background(), tok.Hash, spec, time.Now()); err != nil {
		t.Fatalf("create token: %v", err)
	}

	agentCfg := agent.Config{
		ControlPlaneURL:    "https://localhost",
		Token:              tok.Plaintext,
		DataDir:            t.TempDir(),
		NodeID:             nodeID,
		Environment:        "test",
		Transport:          "libp2p",
		LibP2PRelayAddr:    relayAddrs[0],
		LibP2PServerPeerID: serverTarget.ID.String(),
		CollectionInterval: 2 * time.Second,
		CollectionTimeout:  2 * time.Second,
		InventoryInterval:  2 * time.Second,
	}

	a, err := agent.New(context.Background(), agentCfg, log.Discard())
	if err != nil {
		t.Fatalf("agent.New(): %v", err)
	}
	agentCtx, agentCancel := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() { agentDone <- a.Run(agentCtx) }()
	t.Cleanup(func() {
		agentCancel()
		select {
		case <-agentDone:
		case <-time.After(15 * time.Second):
			t.Error("agent did not shut down within 15s")
		}
	})

	base := "http://" + waitForBoundAddress(t, instance)

	// Wait for the agent's real container inventory to arrive — whatever is
	// actually running on this machine's Docker daemon. If nothing is
	// running (or Docker is absent here), that is an environment fact this
	// test cannot fabricate around; it skips rather than reporting a false
	// pass or a misleading failure.
	var containerID string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/containers?node=" + nodeID)
		if err == nil {
			var payload struct {
				Containers []struct {
					ID string `json:"id"`
				} `json:"containers"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			if len(payload.Containers) > 0 {
				containerID = payload.Containers[0].ID
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if containerID == "" {
		t.Skip("no containers observed from this machine's real Docker daemon within 30s; nothing to fetch remote logs for")
	}

	// Non-follow: the real request/response path, Browser/API -> Server ->
	// AgentOps -> Agent -> real docker.Client.Logs -> back.
	resp, err := http.Get(base + "/api/v1/containers/" + containerID + "/logs?node=" + nodeID + "&tail=5")
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET logs status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var logsPayload struct {
		ContainerID string `json:"container_id"`
		Total       int    `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&logsPayload); err != nil {
		t.Fatalf("decode logs response: %v", err)
	}
	if logsPayload.ContainerID != containerID {
		t.Errorf("container_id = %q, want %q", logsPayload.ContainerID, containerID)
	}

	// Follow: a real WebSocket upgrade over the same remote path, proving
	// the live-follow leg (not just the tail request above) reaches the
	// Agent's real Docker connection and a clean session teardown works.
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/api/v1/containers/" + containerID + "/logs/follow?node=" + nodeID + "&tail=1"
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer wsCancel()
	conn, wsResp, err := websocket.Dial(wsCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("follow dial: %v (status %v)", err, wsResp)
	}
	conn.Close(websocket.StatusNormalClosure, "done")
}
