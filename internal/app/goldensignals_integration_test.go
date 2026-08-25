//go:build integration

// Golden Signals and SLO evaluation are wired entirely through lazy,
// pool-backed adapters (see internal/app/goldensignals.go and
// internal/app/slo.go) that no unit test touches — this is the test that
// would catch a wrong metric name, label, or a windowed query gone wrong.
package app_test

import (
	"bytes"
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

func TestGoldenSignalsHonestlyReportsLatencyUnavailableAndMeasuresTheRest(t *testing.T) {
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

	nodeID := fmt.Sprintf("signalstest-node-%d", time.Now().UnixNano())
	ctx := context.Background()
	now := time.Now().UTC()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := seedPool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	sample := func(metric string, value float64, labels string) {
		t.Helper()
		exec(`INSERT INTO metric_samples (time, node_id, collector_id, metric, value, unit, kind, labels)
			VALUES ($1, $2, 'test', $3, $4, 'na', 'gauge', $5::jsonb)`,
			now, nodeID, metric, value, labels)
	}

	exec(`INSERT INTO nodes (node_id, hostname, cpu_cores, last_seen_at) VALUES ($1, $1, $2, $3)`, nodeID, 8, now)
	sample("system.cpu.usage", 85, "{}") // drives saturation, no memory/disk seeded
	sample("system.network.rx.bytes", 500, `{"interface":"eth0"}`)
	sample("system.network.tx.bytes", 300, `{"interface":"eth0"}`)
	sample("system.network.rx.errors", 2, `{"interface":"eth0"}`)
	sample("system.network.tx.dropped", 1, `{"interface":"eth0"}`)

	client := authenticatedTestClient(t, base, "viewer")
	resp, err := client.Get(base + "/api/v1/signals?node=" + nodeID)
	if err != nil {
		t.Fatalf("GET signals: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var got struct {
		NodeID  string `json:"node_id"`
		Signals []struct {
			Name      string  `json:"name"`
			Available bool    `json:"available"`
			Value     float64 `json:"value"`
			Unit      string  `json:"unit"`
		} `json:"signals"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NodeID != nodeID {
		t.Errorf("node_id = %q, want %q", got.NodeID, nodeID)
	}
	if len(got.Signals) != 4 {
		t.Fatalf("expected 4 signals, got %d", len(got.Signals))
	}

	byName := map[string]struct {
		Available bool
		Value     float64
	}{}
	for _, s := range got.Signals {
		byName[s.Name] = struct {
			Available bool
			Value     float64
		}{s.Available, s.Value}
	}

	if d := byName["latency"]; d.Available {
		t.Error("latency must be reported unavailable — Atlas has no latency telemetry")
	}
	if d := byName["traffic"]; !d.Available || d.Value != 800 {
		t.Errorf("traffic = %+v, want available value 800 (500 rx + 300 tx)", d)
	}
	if d := byName["errors"]; !d.Available || d.Value != 3 {
		t.Errorf("errors = %+v, want available value 3 (2 rx.errors + 1 tx.dropped)", d)
	}
	if d := byName["saturation"]; !d.Available || d.Value != 85 {
		t.Errorf("saturation = %+v, want available value 85 (CPU, the only resource seeded)", d)
	}
}

func TestSLOLifecycleAndStatusAgainstRealHistory(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	base := bootServer(t)
	// Creating an SLO is a fleet.write action now — see
	// docs/adr/0013-human-user-authentication-and-authorization.md.
	client := authenticatedTestClient(t, base, "operator")

	seedPool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer seedPool.Close()

	nodeID := fmt.Sprintf("slotest-node-%d", time.Now().UnixNano())
	ctx := context.Background()
	now := time.Now().UTC()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := seedPool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ($1, $1, $2)`, nodeID, now)

	// 7 compliant samples (< 80) and 1 non-compliant (>= 80) — 8 total so
	// compliance (7/8) lands on a binary-exact fraction, spread across the
	// last 50 minutes so a 1-hour window's raw-resolution query covers all
	// of them.
	values := []float64{10, 20, 30, 40, 50, 60, 70, 95}
	for i, v := range values {
		ts := now.Add(-time.Duration(50-i*6) * time.Minute)
		exec(`INSERT INTO metric_samples (time, node_id, collector_id, metric, value, unit, kind, labels)
			VALUES ($1, $2, 'test', 'system.cpu.usage', $3, 'percent', 'gauge', '{}'::jsonb)`, ts, nodeID, v)
	}

	// Create the SLO via the API. Target 80% against 87.5% actual
	// compliance: a 20-point error budget, 12.5 points of which are used —
	// 62.5% consumed, comfortably under the default 75% warning threshold.
	body, _ := json.Marshal(map[string]any{
		"name": "cpu-under-80", "node_id": nodeID, "signal": "saturation",
		"metric": "system.cpu.usage", "comparison": "lt", "threshold": 80,
		"target_percentage": 80, "window_seconds": 3600,
	})
	createResp, err := client.Post(base+"/api/v1/slo", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST slo: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(createResp.Body)
		t.Fatalf("status = %d, want 201: %s", createResp.StatusCode, respBody)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created SLO has no id")
	}

	// List includes it. Reads on /slo are still open — not in scope for
	// this pass, see docs/adr/0013-human-user-authentication-and-authorization.md.
	listResp, err := http.Get(base + "/api/v1/slo")
	if err != nil {
		t.Fatalf("GET slo list: %v", err)
	}
	defer listResp.Body.Close()
	var list struct {
		Total int `json:"total"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total < 1 {
		t.Fatal("expected the created SLO to appear in the list")
	}

	// Status reflects the seeded history: 7/8 compliant = 87.5% compliance
	// against an 80% target.
	statusResp, err := http.Get(base + "/api/v1/slo/" + created.ID + "/status")
	if err != nil {
		t.Fatalf("GET slo status: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(statusResp.Body)
		t.Fatalf("status = %d, want 200: %s", statusResp.StatusCode, respBody)
	}
	var eval struct {
		Available                  bool    `json:"available"`
		SampleCount                int     `json:"sample_count"`
		Compliance                 float64 `json:"compliance"`
		ErrorBudget                float64 `json:"error_budget"`
		ErrorBudgetConsumedPercent float64 `json:"error_budget_consumed_percent"`
		Status                     string  `json:"status"`
	}
	if err := json.NewDecoder(statusResp.Body).Decode(&eval); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !eval.Available {
		t.Fatal("expected available")
	}
	if eval.SampleCount != 8 {
		t.Errorf("sample count = %d, want 8", eval.SampleCount)
	}
	if eval.Compliance != 87.5 {
		t.Errorf("compliance = %v, want 87.5", eval.Compliance)
	}
	if eval.ErrorBudget != 20 {
		t.Errorf("error budget = %v, want 20", eval.ErrorBudget)
	}
	if eval.ErrorBudgetConsumedPercent != 62.5 {
		t.Errorf("consumed = %v, want 62.5", eval.ErrorBudgetConsumedPercent)
	}
	if eval.Status != "healthy" {
		t.Errorf("status = %q, want healthy", eval.Status)
	}

	// Delete removes it — also fleet.write.
	delReq, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/slo/"+created.ID, nil)
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE slo: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", delResp.StatusCode)
	}

	getAfterDelete, err := http.Get(base + "/api/v1/slo/" + created.ID)
	if err != nil {
		t.Fatalf("GET after delete: %v", err)
	}
	defer getAfterDelete.Body.Close()
	if getAfterDelete.StatusCode != http.StatusNotFound {
		t.Errorf("status after delete = %d, want 404", getAfterDelete.StatusCode)
	}
}

func TestSLOStatusUnavailableWithNoSamplesInWindow(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	base := bootServer(t)
	client := authenticatedTestClient(t, base, "operator")
	nodeID := fmt.Sprintf("slotest-empty-node-%d", time.Now().UnixNano())

	body, _ := json.Marshal(map[string]any{
		"name": "no-data", "node_id": nodeID, "metric": "system.cpu.usage",
		"comparison": "lt", "threshold": 80, "target_percentage": 99, "window_seconds": 3600,
	})
	createResp, err := client.Post(base+"/api/v1/slo", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST slo: %v", err)
	}
	defer createResp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	statusResp, err := http.Get(base + "/api/v1/slo/" + created.ID + "/status")
	if err != nil {
		t.Fatalf("GET slo status: %v", err)
	}
	defer statusResp.Body.Close()
	var eval struct {
		Available bool `json:"available"`
	}
	if err := json.NewDecoder(statusResp.Body).Decode(&eval); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if eval.Available {
		t.Fatal("expected unavailable with no samples for a node that never reported this metric")
	}
}
