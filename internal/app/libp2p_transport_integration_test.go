//go:build integration

// Proves the libp2p POC transport (docs/adr/0012-connect-by-identity.md)
// carries the real Agent collection pipeline end to end: a real
// internal/agent.Agent enrolls, then pushes metrics, inventory and events,
// over a real libp2p stream into a real fleetPipeline listener, and X.509
// mTLS authorization runs on top of that stream exactly as it does over TCP.
//
// This is the automated (loopback) half of the POC's verification. The
// physical Mac-behind-NAT -> reachable-Linux-server half is run manually —
// see the POC report for exact commands and results.
package app_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

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
// address — never a direct one — reaching it and completing enrollment plus
// telemetry through the relay hop.
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

// runAgentAndAssertObserved starts a real libp2p-transport agent dialing
// peerAddr and waits for the control plane to observe nodeID.
func runAgentAndAssertObserved(t *testing.T, instance *app.App, peerAddr, nodeID string) {
	t.Helper()

	tok, err := corefleet.NewToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	spec := corefleet.TokenSpec{
		Label: "libp2p-poc", Environment: "test", AllowedCIDR: "0.0.0.0/0",
		MaxUses: 1, TTL: time.Hour,
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("token spec: %v", err)
	}
	repo := storagefleet.NewRepository(instance.Pool.DB())
	if err := repo.CreateToken(context.Background(), tok.Hash, spec, time.Now()); err != nil {
		t.Fatalf("create token: %v", err)
	}

	agentCfg := agent.Config{
		// Unused for dialing under the libp2p transport — routing is by peer
		// id, not address — but still validated as a TLS ServerName, so it
		// must match a SAN on the control plane's leaf certificate, which
		// defaults to localhost/127.0.0.1. See pki.NewServerLeaf.
		ControlPlaneURL:    "https://localhost",
		Token:              tok.Plaintext,
		DataDir:            t.TempDir(),
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
		t.Fatalf("agent.New() error = %v: agent enrollment over libp2p failed, so X.509 authorization never got a chance to run", err)
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
