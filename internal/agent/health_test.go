package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/plugin"
)

func testReporter(relationships map[string]*relationshipRuntime, environments map[string]string) *healthReporter {
	return newHealthReporter("node-1", "/var/lib/atlas-agent/atlas-agent.lock", relationships, environments, nil)
}

func TestHealthReportsEveryRelationshipIndependently(t *testing.T) {
	t.Parallel()

	relationships := map[string]*relationshipRuntime{
		"development": {id: "development", relCfg: relationshipConfig{
			RelationshipBootstrap: RelationshipBootstrap{Transport: "https"}}},
		"production": {id: "production", relCfg: relationshipConfig{
			RelationshipBootstrap: RelationshipBootstrap{Transport: "libp2p"}}},
	}
	environments := map[string]string{"development": "development", "production": "production"}

	got := testReporter(relationships, environments).snapshot(time.Now())

	if len(got.Relationships) != 2 {
		t.Fatalf("got %d relationships, want 2 — one relationship must never hide another", len(got.Relationships))
	}
	byID := map[string]RelationshipHealth{}
	for _, rh := range got.Relationships {
		byID[rh.ID] = rh
	}
	if byID["development"].Environment != "development" || byID["production"].Environment != "production" {
		t.Errorf("environments = %q / %q, want each relationship's own tag",
			byID["development"].Environment, byID["production"].Environment)
	}
	if byID["development"].Transport != "https" || byID["production"].Transport != "libp2p" {
		t.Errorf("transports = %q / %q, want each relationship's own transport",
			byID["development"].Transport, byID["production"].Transport)
	}
}

func TestHealthReportsIdentityAndUptime(t *testing.T) {
	t.Parallel()

	reporter := testReporter(map[string]*relationshipRuntime{}, nil)
	reporter.startedAt = time.Now().Add(-90 * time.Second)

	got := reporter.snapshot(time.Now())

	if got.NodeID != "node-1" {
		t.Errorf("node id = %q, want node-1", got.NodeID)
	}
	if got.Version == "" {
		t.Error("version is empty; the report must name the binary that produced it")
	}
	if got.UptimeSeconds < 89 || got.UptimeSeconds > 120 {
		t.Errorf("uptime = %.1fs, want about 90s", got.UptimeSeconds)
	}
	if got.SingleInstanceLock == "" {
		t.Error("single-instance lock path is empty")
	}
}

func TestHealthReportsCollectorOutcomes(t *testing.T) {
	t.Parallel()

	reporter := newHealthReporter("node-1", "/lock", map[string]*relationshipRuntime{}, nil,
		func() []CollectorHealth {
			return collectorHealthFrom([]plugin.State{
				{ID: "system", Status: plugin.StatusActive},
				{ID: "docker", Status: plugin.StatusNotDetected},
				{ID: "ports", Status: plugin.StatusInitFailed, Error: "permission denied"},
			})
		})

	got := reporter.snapshot(time.Now())

	if len(got.Collectors) != 3 {
		t.Fatalf("got %d collectors, want 3", len(got.Collectors))
	}
	var failed CollectorHealth
	for _, c := range got.Collectors {
		if c.ID == "ports" {
			failed = c
		}
	}
	if failed.Error != "permission denied" {
		t.Errorf("failed collector error = %q, want the reason preserved", failed.Error)
	}
}

// The report is a payload sent to a control plane, so it must never carry
// key material — only identities and expiry.
func TestHealthSnapshotCarriesNoPrivateKeyMaterial(t *testing.T) {
	t.Parallel()

	relationships := map[string]*relationshipRuntime{
		"production": {id: "production", relCfg: relationshipConfig{
			RelationshipBootstrap: RelationshipBootstrap{Transport: "libp2p"}}},
	}
	got := testReporter(relationships, nil).snapshot(time.Now())

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"PRIVATE KEY", "private_key", "BEGIN RSA", "BEGIN EC"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("health payload contains %q: %s", forbidden, raw)
		}
	}
}

func TestHealthSourceProducesTheAgentHealthSubject(t *testing.T) {
	t.Parallel()

	src := healthSource(testReporter(map[string]*relationshipRuntime{}, nil))
	if src.subject != coreinventory.SubjectAgentHealth {
		t.Errorf("subject = %q, want %q", src.subject, coreinventory.SubjectAgentHealth)
	}

	data, err := src.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, ok := data.(AgentHealth); !ok {
		t.Errorf("fetch returned %T, want AgentHealth", data)
	}
}
