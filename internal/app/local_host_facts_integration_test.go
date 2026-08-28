//go:build integration

// The local self-monitoring path for host facts: a plain atlas-server
// observes its own host through internal/plugin/system's host collector, and
// nodes.hardware_uuid must be populated for its own row the same way it is
// for a remote agent's (see remote_inventory_promotion_integration_test.go).
package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/plugin/system"
)

func TestLocalHostFactsPopulateHardwareUUID(t *testing.T) {
	if os.Getenv(testDatabaseURLEnv) == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	hwUUID := ""
	if info, err := system.NewProvider().Host(context.Background()); err == nil {
		hwUUID = info.HardwareUUID
	}
	if hwUUID == "" {
		t.Skip("this host supplies no hardware UUID (unprivileged Linux / container); nothing to assert for the local path")
	}

	base := bootServer(t)
	client := authenticatedTestClient(t, base, "viewer")

	type node struct {
		OS           string `json:"os"`
		Platform     string `json:"platform"`
		HardwareUUID string `json:"hardware_uuid"`
	}
	type listResp struct {
		Nodes []node `json:"nodes"`
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/api/v1/nodes")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		var body listResp
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			time.Sleep(time.Second)
			continue
		}
		for _, n := range body.Nodes {
			if n.HardwareUUID != "" {
				if n.HardwareUUID != hwUUID {
					t.Fatalf("local node hardware_uuid = %q, want this host's real value %q", n.HardwareUUID, hwUUID)
				}
				if n.OS == "" || n.Platform == "" {
					t.Errorf("local host facts incomplete alongside hardware_uuid: os=%q platform=%q", n.OS, n.Platform)
				}
				return // populated and correct
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("local node's hardware_uuid was never populated by the self-monitoring host collector")
}
