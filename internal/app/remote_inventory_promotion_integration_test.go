//go:build integration

// Proves the regression the cyrene-dev-v2-shows-NULL investigation was
// about: a remote agent already collects host facts and interface addressing
// and pushes them, but until now nothing on receipt promoted them out of
// inventory_snapshots into the nodes table, so nodes.os / nodes.platform /
// node_addresses stayed NULL/empty for every remote node.
//
// A real internal/agent.Agent pushes over a real libp2p stream into a real
// control plane; this asserts the promoted columns are populated afterwards,
// that nodes.public_ip carries the server-observed connection address
// captured by the fleet host's Connected notifiee, and that
// nodes.hardware_uuid is promoted from the same host snapshot.
package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/agent"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/plugin/system"
)

func TestRemoteInventoryPromotedIntoNodesTable(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	// The in-process agent reads this machine's real hardware UUID. On a host
	// that cannot supply one (unprivileged Linux, container) it is honestly
	// empty and there is nothing to assert; when non-empty, the promoted
	// value must match it exactly.
	hwUUID := ""
	if info, err := system.NewProvider().Host(context.Background()); err == nil {
		hwUUID = info.HardwareUUID
	}

	instance, peerAddr := bootLibP2PFleetServer(t, "")
	nodeID := "promotion-it-node-" + id.New()

	dataDir := t.TempDir()
	authorizeAgentPeer(t, instance, dataDir, nodeID, "test")

	agentCfg := agent.Config{
		ControlPlaneURL:    "https://localhost", // unused for dialing; see runAgentAndAssertObserved
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
		t.Fatalf("agent.New(): %v", err)
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
	client := authenticatedTestClient(t, base, "viewer")

	type nodeResp struct {
		OS           string `json:"os"`
		Platform     string `json:"platform"`
		Kernel       string `json:"kernel"`
		PublicIP     string `json:"public_ip"`
		HardwareUUID string `json:"hardware_uuid"`
		BootTime     string `json:"boot_time"`
		Address      []struct {
			Interface string `json:"interface"`
			Address   string `json:"address"`
		} `json:"addresses"`
	}

	var got nodeResp
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/api/v1/nodes/" + nodeID)
		if err == nil && resp.StatusCode == http.StatusOK {
			var body nodeResp
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			got = body
			// Host promotion: os + platform. Network promotion: at least one
			// address. Notifiee: a server-observed public ip (loopback here).
			hwReady := hwUUID == "" || body.HardwareUUID != ""
			if body.OS != "" && body.Platform != "" && len(body.Address) > 0 && body.PublicIP != "" && hwReady {
				break
			}
		} else if err == nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}

	if got.OS == "" || got.Platform == "" {
		t.Errorf("host facts were not promoted into nodes: os=%q platform=%q kernel=%q", got.OS, got.Platform, got.Kernel)
	}
	if len(got.Address) == 0 {
		t.Error("network addressing was not promoted into node_addresses (no addresses on the node detail)")
	} else {
		for _, ad := range got.Address {
			if ad.Interface == "" || ad.Address == "" {
				t.Errorf("promoted address is malformed: %+v", ad)
			}
		}
	}
	if got.PublicIP == "" {
		t.Error("nodes.public_ip was not captured by the fleet host Connected notifiee")
	}
	if hwUUID == "" {
		t.Logf("this host supplies no hardware UUID; nodes.hardware_uuid promotion not asserted")
	} else if got.HardwareUUID != hwUUID {
		t.Errorf("hardware_uuid = %q, want the host's real value %q promoted from the host snapshot", got.HardwareUUID, hwUUID)
	}
}
