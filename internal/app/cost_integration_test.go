//go:build integration

// The cost analysis engine is wired entirely through a lazy, pool-backed
// adapter (see internal/app/cost.go) that no unit test touches — this is
// the test that would catch a wrong metric name, label, or column there.
package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedCostFixture inserts one node with cpu_cores set and every metric the
// cost engine reads: CPU utilization, memory, disk (across two mountpoints,
// to pin that disk is summed rather than overwritten), network throughput,
// running containers, and process count.
func seedCostFixture(t *testing.T, pool *pgxpool.Pool, nodeID string) {
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
		nodeID, 4, now)

	sample("system.cpu.usage", 50, "{}")
	sample("system.memory.total", 8*float64(bytesPerGB), "{}")
	sample("system.disk.total", 60*float64(bytesPerGB), `{"mountpoint":"/"}`)
	sample("system.disk.total", 40*float64(bytesPerGB), `{"mountpoint":"/data"}`)
	sample("system.network.rx.bytes", 1<<20, `{"interface":"eth0"}`) // 1 MiB/s
	sample("system.network.tx.bytes", 0, `{"interface":"eth0"}`)
	sample("docker.containers.count", 3, `{"state":"running"}`)
	sample("docker.containers.count", 1, `{"state":"exited"}`)
	sample("process.total", 50, "{}")
}

const bytesPerGB = 1 << 30

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostEstimateReflectsEverySeededSignal(t *testing.T) {
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

	nodeID := fmt.Sprintf("costtest-node-%d", time.Now().UnixNano())
	seedCostFixture(t, seedPool, nodeID)

	resp, err := http.Get(base + "/api/v1/cost/estimate?node=" + nodeID)
	if err != nil {
		t.Fatalf("GET cost/estimate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var got struct {
		NodeID       string `json:"node_id"`
		PricingModel string `json:"pricing_model"`
		Usage        struct {
			CPUCores              int     `json:"cpu_cores"`
			CPUUtilizationPercent float64 `json:"cpu_utilization_percent"`
			MemoryTotalBytes      float64 `json:"memory_total_bytes"`
			DiskTotalBytes        float64 `json:"disk_total_bytes"`
			RunningContainers     int     `json:"running_containers"`
			RunningProcesses      int     `json:"running_processes"`
			UptimeSeconds         float64 `json:"uptime_seconds"`
		} `json:"usage"`
		Breakdown struct {
			CPU       float64 `json:"cpu"`
			Memory    float64 `json:"memory"`
			Disk      float64 `json:"disk"`
			Network   float64 `json:"network"`
			Container float64 `json:"container"`
		} `json:"breakdown"`
		HourlyTotal        float64 `json:"hourly_total"`
		EstimatedSinceBoot float64 `json:"estimated_since_boot"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.NodeID != nodeID {
		t.Errorf("node_id = %q, want %q", got.NodeID, nodeID)
	}
	if got.PricingModel != "reference" {
		t.Errorf("pricing_model = %q, want reference", got.PricingModel)
	}

	// Usage read back correctly, including disk summed across mountpoints
	// and containers filtered to the running state.
	if got.Usage.CPUCores != 4 {
		t.Errorf("cpu_cores = %d, want 4", got.Usage.CPUCores)
	}
	if got.Usage.CPUUtilizationPercent != 50 {
		t.Errorf("cpu_utilization_percent = %v, want 50", got.Usage.CPUUtilizationPercent)
	}
	if got.Usage.MemoryTotalBytes != 8*float64(bytesPerGB) {
		t.Errorf("memory_total_bytes = %v, want %v", got.Usage.MemoryTotalBytes, 8*float64(bytesPerGB))
	}
	if got.Usage.DiskTotalBytes != 100*float64(bytesPerGB) {
		t.Errorf("disk_total_bytes = %v, want %v (60GB + 40GB summed)", got.Usage.DiskTotalBytes, 100*float64(bytesPerGB))
	}
	if got.Usage.RunningContainers != 3 {
		t.Errorf("running_containers = %d, want 3 (exited must not count)", got.Usage.RunningContainers)
	}
	if got.Usage.RunningProcesses != 50 {
		t.Errorf("running_processes = %d, want 50", got.Usage.RunningProcesses)
	}
	if got.Usage.UptimeSeconds != 0 {
		t.Errorf("uptime_seconds = %v, want 0 (no boot time seeded)", got.Usage.UptimeSeconds)
	}

	// Cost, against the default reference rates: CPU 0.02/core-hr, memory
	// 0.005/GB-hr, disk 0.0001/GB-hr, network 0.01/GB, container 0.01/hr.
	wantCPU := 4 * 0.5 * 0.02             // 0.04
	wantMemory := 8 * 0.005               // 0.04
	wantDisk := 100 * 0.0001              // 0.01
	wantNetwork := (3600.0 / 1024) * 0.01 // 1 MiB/s for an hour = 3600/1024 GB
	wantContainer := 3 * 0.01             // 0.03

	if !almostEqual(got.Breakdown.CPU, wantCPU) {
		t.Errorf("breakdown.cpu = %v, want %v", got.Breakdown.CPU, wantCPU)
	}
	if !almostEqual(got.Breakdown.Memory, wantMemory) {
		t.Errorf("breakdown.memory = %v, want %v", got.Breakdown.Memory, wantMemory)
	}
	if !almostEqual(got.Breakdown.Disk, wantDisk) {
		t.Errorf("breakdown.disk = %v, want %v", got.Breakdown.Disk, wantDisk)
	}
	if !almostEqual(got.Breakdown.Network, wantNetwork) {
		t.Errorf("breakdown.network = %v, want %v", got.Breakdown.Network, wantNetwork)
	}
	if !almostEqual(got.Breakdown.Container, wantContainer) {
		t.Errorf("breakdown.container = %v, want %v", got.Breakdown.Container, wantContainer)
	}

	wantHourly := wantCPU + wantMemory + wantDisk + wantNetwork + wantContainer
	if !almostEqual(got.HourlyTotal, wantHourly) {
		t.Errorf("hourly_total = %v, want %v", got.HourlyTotal, wantHourly)
	}
	if got.EstimatedSinceBoot != 0 {
		t.Errorf("estimated_since_boot = %v, want 0 (uptime is 0)", got.EstimatedSinceBoot)
	}
}

func TestCostFleetIncludesSeededNodeInTheFleetTotal(t *testing.T) {
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

	nodeID := fmt.Sprintf("costtest-fleet-node-%d", time.Now().UnixNano())
	seedCostFixture(t, seedPool, nodeID)

	resp, err := http.Get(base + "/api/v1/cost/fleet")
	if err != nil {
		t.Fatalf("GET cost/fleet: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var got struct {
		Nodes []struct {
			NodeID      string  `json:"node_id"`
			HourlyTotal float64 `json:"hourly_total"`
		} `json:"nodes"`
		Total            int     `json:"total"`
		FleetHourlyTotal float64 `json:"fleet_hourly_total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != len(got.Nodes) {
		t.Errorf("total = %d, len(nodes) = %d", got.Total, len(got.Nodes))
	}

	var found bool
	var seededHourly float64
	for _, n := range got.Nodes {
		if n.NodeID == nodeID {
			found = true
			seededHourly = n.HourlyTotal
		}
	}
	if !found {
		t.Fatalf("seeded node %s not present in the fleet listing", nodeID)
	}
	if seededHourly <= 0 {
		t.Errorf("seeded node hourly total = %v, want positive", seededHourly)
	}
	if got.FleetHourlyTotal < seededHourly {
		t.Errorf("fleet hourly total (%v) is less than the seeded node's own total (%v)", got.FleetHourlyTotal, seededHourly)
	}
}
