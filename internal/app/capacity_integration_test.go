//go:build integration

// The capacity planning engine is wired entirely through a lazy,
// pool-backed adapter (see internal/app/capacity.go) that no unit test
// touches — this is the test that would catch a wrong metric name, label,
// or column there.
package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedCapacityFixture inserts one node at 80% CPU, 80% memory, one
// over-threshold disk mount and one healthy one, and running containers and
// processes — enough to drive every configured domain (network, container,
// and process density stay unconfigured by default, so they are exercised
// as "unavailable" here).
func seedCapacityFixture(t *testing.T, pool *pgxpool.Pool, nodeID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	sample := func(metric string, value float64, labels string) {
		t.Helper()
		exec(`INSERT INTO metric_samples (time, node_id, collector_id, metric, value, unit, kind, labels)
			VALUES ($1, $2, 'test', $3, $4, 'na', 'gauge', $5::jsonb)`,
			now, nodeID, metric, value, labels)
	}

	exec(`INSERT INTO nodes (node_id, hostname, cpu_cores, last_seen_at) VALUES ($1, $1, $2, $3)`,
		nodeID, 8, now)

	sample("system.cpu.usage", 80, "{}")
	sample("system.memory.total", 16*float64(bytesPerGB), "{}")
	sample("system.memory.used", 12.8*float64(bytesPerGB), "{}")
	sample("system.memory.usage", 80, "{}")
	sample("system.disk.total", 100*float64(bytesPerGB), `{"mountpoint":"/"}`)
	sample("system.disk.used", 95*float64(bytesPerGB), `{"mountpoint":"/"}`)
	sample("system.disk.usage", 95, `{"mountpoint":"/"}`)
	sample("system.disk.total", 200*float64(bytesPerGB), `{"mountpoint":"/data"}`)
	sample("system.disk.used", 20*float64(bytesPerGB), `{"mountpoint":"/data"}`)
	sample("system.disk.usage", 10, `{"mountpoint":"/data"}`)
	sample("docker.containers.count", 7, `{"state":"running"}`)
	sample("process.total", 120, "{}")
}

func TestCapacitySummaryReflectsEverySeededSignal(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	base := bootServer(t)

	seedPool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer seedPool.Close()

	nodeID := fmt.Sprintf("capacitytest-node-%d", time.Now().UnixNano())
	seedCapacityFixture(t, seedPool, nodeID)

	client := authenticatedTestClient(t, base, "viewer")
	resp, err := client.Get(base + "/api/v1/capacity/summary?node=" + nodeID)
	if err != nil {
		t.Fatalf("GET capacity/summary: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var got struct {
		NodeID  string `json:"node_id"`
		Status  string `json:"status"`
		Domains []struct {
			Name               string  `json:"name"`
			Available          bool    `json:"available"`
			UtilizationPercent float64 `json:"utilization_percent"`
			RemainingCapacity  float64 `json:"remaining_capacity"`
			RemainingUnit      string  `json:"remaining_unit"`
			Status             string  `json:"status"`
		} `json:"domains"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.NodeID != nodeID {
		t.Errorf("node_id = %q, want %q", got.NodeID, nodeID)
	}
	// Disk's worst mount is at 95%, critical: the node's overall status
	// must be dragged to critical by it.
	if got.Status != "critical" {
		t.Errorf("status = %q, want critical (driven by the 95%% disk mount)", got.Status)
	}
	if len(got.Domains) != 6 {
		t.Fatalf("expected 6 domains, got %d: %+v", len(got.Domains), got.Domains)
	}

	byName := map[string]struct {
		Available          bool
		UtilizationPercent float64
		RemainingCapacity  float64
		RemainingUnit      string
		Status             string
	}{}
	for _, d := range got.Domains {
		byName[d.Name] = struct {
			Available          bool
			UtilizationPercent float64
			RemainingCapacity  float64
			RemainingUnit      string
			Status             string
		}{d.Available, d.UtilizationPercent, d.RemainingCapacity, d.RemainingUnit, d.Status}
	}

	if d := byName["cpu"]; !d.Available || d.UtilizationPercent != 80 || !almostEqual(d.RemainingCapacity, 1.6) || d.Status != "warning" {
		t.Errorf("cpu domain = %+v, want available 80%%, 1.6 cores remaining, warning", d)
	}
	if d := byName["memory"]; !d.Available || d.UtilizationPercent != 80 || d.Status != "warning" {
		t.Errorf("memory domain = %+v, want available 80%%, warning", d)
	}
	if d := byName["disk"]; !d.Available || d.UtilizationPercent != 95 || d.Status != "critical" {
		t.Errorf("disk domain = %+v, want available 95%% (worst mount), critical", d)
	}
	if d := byName["disk"]; d.RemainingCapacity != 5+180 {
		t.Errorf("disk remaining = %v, want %v (summed across both mounts)", d.RemainingCapacity, 5+180.0)
	}
	// Network, container_density, and process_density are unconfigured by
	// default — they must report unavailable, not a fabricated ceiling.
	if d := byName["network"]; d.Available {
		t.Errorf("network domain = %+v, want unavailable (no configured link capacity)", d)
	}
	if d := byName["container_density"]; d.Available {
		t.Errorf("container_density domain = %+v, want unavailable (no configured ceiling)", d)
	}
	if d := byName["process_density"]; d.Available {
		t.Errorf("process_density domain = %+v, want unavailable (no configured ceiling)", d)
	}
}

func TestCapacityFleetRollsUpWorstStatusAcrossNodes(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	base := bootServer(t)

	seedPool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer seedPool.Close()

	nodeID := fmt.Sprintf("capacitytest-fleet-node-%d", time.Now().UnixNano())
	seedCapacityFixture(t, seedPool, nodeID)

	resp, err := http.Get(base + "/api/v1/capacity/fleet")
	if err != nil {
		t.Fatalf("GET capacity/fleet: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var got struct {
		Nodes []struct {
			NodeID string `json:"node_id"`
			Status string `json:"status"`
		} `json:"nodes"`
		Total        int            `json:"total"`
		Status       string         `json:"status"`
		StatusCounts map[string]int `json:"status_counts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != len(got.Nodes) {
		t.Errorf("total = %d, len(nodes) = %d", got.Total, len(got.Nodes))
	}

	var found bool
	for _, n := range got.Nodes {
		if n.NodeID == nodeID {
			found = true
			if n.Status != "critical" {
				t.Errorf("seeded node status = %q, want critical", n.Status)
			}
		}
	}
	if !found {
		t.Fatalf("seeded node %s not present in the fleet listing", nodeID)
	}
	if got.Status != "critical" {
		t.Errorf("fleet status = %q, want critical (at least one node is critical)", got.Status)
	}
	if got.StatusCounts["critical"] < 1 {
		t.Errorf("status_counts[critical] = %d, want at least 1", got.StatusCounts["critical"])
	}
}
