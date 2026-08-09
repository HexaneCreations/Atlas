//go:build integration

// The health score engine is wired entirely through lazy, pool-backed
// adapters (see internal/app/health.go) that no unit test touches — this is
// the test that would catch a wrong table name or column in any of them.
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

// seedHealthScoreFixture inserts one node with a firing critical alert, an
// open critical incident, a threshold-violating metric sample, and a
// danger-level event — one instance of every non-inventory signal the health
// score engine reads, each independently verifiable against the SQL each
// provider issues.
func seedHealthScoreFixture(t *testing.T, pool *pgxpool.Pool, nodeID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}

	exec(`INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ($1, $1, $2)`, nodeID, now)

	ruleID := "healthtest-rule-" + nodeID
	exec(`INSERT INTO alert_rules (id, name, enabled, kind, severity, metric, comparison, threshold, node_id)
		VALUES ($1, 'health score test rule', true, 'threshold', 'critical', $2, 'gt', 90, $3)`,
		ruleID, "healthscore.test.cpu."+nodeID, nodeID)
	exec(`INSERT INTO alert_states (rule_id, node_id, series_key, state, value, updated_at)
		VALUES ($1, $2, '', 'firing', 95, $3)`, ruleID, nodeID, now)

	exec(`INSERT INTO metric_samples (time, node_id, collector_id, metric, value, unit, kind, labels)
		VALUES ($1, $2, 'test', $3, 95, 'percent', 'gauge', '{}'::jsonb)`,
		now, nodeID, "healthscore.test.cpu."+nodeID)

	incidentID := "healthtest-incident-" + nodeID
	exec(`INSERT INTO incidents (id, title, status, severity, opened_at, updated_at)
		VALUES ($1, 'health score test incident', 'open', 'critical', $2, $2)`, incidentID, now)
	exec(`INSERT INTO incident_members (id, incident_id, kind, ref_id, node_id, topic, severity, time, is_root_cause)
		VALUES ($1, $2, 'event', $3, $4, 'test.topic', 'critical', $5, true)`,
		"healthtest-member-"+nodeID, incidentID, "healthtest-evt-"+nodeID, nodeID, now)

	exec(`INSERT INTO events (id, time, node_id, topic, source, subject, payload)
		VALUES ($1, $2, $3, 'docker.container.oom', 'test', '', '{}'::jsonb)`,
		"healthtest-event-"+nodeID, now, nodeID)

	exec(`INSERT INTO inventory_snapshots (node_id, subject, observed_at, received_at, content_hash, data)
		VALUES ($1, 'containers', $2, $2, 'h1', '[]'::jsonb)`, nodeID, now)
}

func TestHealthScoreReflectsEverySeededSignal(t *testing.T) {
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

	nodeID := fmt.Sprintf("healthtest-node-%d", time.Now().UnixNano())
	seedHealthScoreFixture(t, seedPool, nodeID)

	resp, err := http.Get(base + "/api/v1/health/score?node=" + nodeID)
	if err != nil {
		t.Fatalf("GET health/score: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var got struct {
		NodeID  string  `json:"node_id"`
		Score   float64 `json:"score"`
		Avail   bool    `json:"available"`
		Signals []struct {
			Name      string  `json:"name"`
			Weight    float64 `json:"weight"`
			Score     float64 `json:"score"`
			Available bool    `json:"available"`
		} `json:"signals"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.NodeID != nodeID {
		t.Errorf("node_id = %q, want %q", got.NodeID, nodeID)
	}
	if !got.Avail {
		t.Fatal("expected the result to be available")
	}

	byName := map[string]struct {
		Score     float64
		Available bool
	}{}
	for _, s := range got.Signals {
		byName[s.Name] = struct {
			Score     float64
			Available bool
		}{s.Score, s.Available}
	}
	if len(byName) != 6 {
		t.Fatalf("expected 6 distinct signals, got %d: %+v", len(byName), got.Signals)
	}

	// One critical alert firing: 100 - 40.
	if s := byName["alerts"]; !s.Available || s.Score != 60 {
		t.Errorf("alerts signal = %+v, want available score 60", s)
	}
	// One critical open incident: 100 - 50.
	if s := byName["incidents"]; !s.Available || s.Score != 50 {
		t.Errorf("incidents signal = %+v, want available score 50", s)
	}
	// One critical threshold violation: 100 - 40.
	if s := byName["metrics"]; !s.Available || s.Score != 60 {
		t.Errorf("metrics signal = %+v, want available score 60", s)
	}
	// Just seeded: fresh heartbeat.
	if s := byName["heartbeat"]; !s.Available || s.Score != 100 {
		t.Errorf("heartbeat signal = %+v, want available score 100", s)
	}
	// Just seeded: fresh inventory.
	if s := byName["inventory"]; !s.Available || s.Score != 100 {
		t.Errorf("inventory signal = %+v, want available score 100", s)
	}
	// One danger-level event: 100 - 10.
	if s := byName["events"]; !s.Available || s.Score != 90 {
		t.Errorf("events signal = %+v, want available score 90", s)
	}

	// Every signal available: overall is the exact weighted average —
	// (60*25 + 50*25 + 60*20 + 100*15 + 100*10 + 90*5) / 100.
	wantOverall := 69.0
	if got.Score != wantOverall {
		t.Errorf("overall score = %v, want %v", got.Score, wantOverall)
	}
}

func TestHealthScoreFleetIncludesSeededNode(t *testing.T) {
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

	nodeID := fmt.Sprintf("healthtest-fleet-node-%d", time.Now().UnixNano())
	seedHealthScoreFixture(t, seedPool, nodeID)

	resp, err := http.Get(base + "/api/v1/health/fleet")
	if err != nil {
		t.Fatalf("GET health/fleet: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var got struct {
		Nodes []struct {
			NodeID string  `json:"node_id"`
			Score  float64 `json:"score"`
		} `json:"nodes"`
		Total int `json:"total"`
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
			if n.Score != 69 {
				t.Errorf("seeded node score = %v, want 69", n.Score)
			}
		}
	}
	if !found {
		t.Fatalf("seeded node %s not present in the fleet listing", nodeID)
	}
}
