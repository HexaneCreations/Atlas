package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- Finding A: relationship id validation ---------------------------------

func TestValidateRelationshipIDRejectsUnsafeValues(t *testing.T) {
	t.Parallel()
	cases := []string{
		"..",
		".",
		"../production",
		"production/../../development",
		`production\development`,
		"",
		"   ",
	}
	for _, id := range cases {
		if err := validateRelationshipID(id); err == nil {
			t.Errorf("validateRelationshipID(%q) = nil, want an error", id)
		}
	}
}

func TestValidateRelationshipIDAcceptsSafeValues(t *testing.T) {
	t.Parallel()
	cases := []string{"production", "development", "prod-us_01"}
	for _, id := range cases {
		if err := validateRelationshipID(id); err != nil {
			t.Errorf("validateRelationshipID(%q) = %v, want nil", id, err)
		}
	}
}

// The property validation exists to guarantee: an id that would let
// dataDirFor collide with another relationship's directory (or escape
// DataDir/relationships/ entirely) must never reach dataDirFor at all.
func TestDiscoverRelationshipsRejectsPathTraversalID(t *testing.T) {
	cases := []string{"..", "../production", "production/../../development", `production\development`}
	for _, id := range cases {
		t.Setenv("ATLAS_AGENT_RELATIONSHIPS", id)
		if _, err := discoverRelationships(Config{DataDir: t.TempDir()}); err == nil {
			t.Errorf("discoverRelationships accepted relationship id %q", id)
		}
	}
}

func TestLoadRelationshipConfigsRejectsDuplicateID(t *testing.T) {
	t.Setenv("ATLAS_AGENT_RELATIONSHIPS", "production,production")
	if _, err := LoadRelationshipConfigs(); err == nil {
		t.Fatal("LoadRelationshipConfigs accepted a duplicate relationship id")
	}
}

func TestDataDirForDefaultIsAgentDataDirUnmoved(t *testing.T) {
	t.Parallel()
	if got := dataDirFor("/var/lib/atlas-agent", defaultRelationshipID); got != "/var/lib/atlas-agent" {
		t.Errorf("dataDirFor(default) = %q, want the agent DataDir itself, unmoved", got)
	}
}

func TestDataDirForNamedRelationshipIsASubdirectory(t *testing.T) {
	t.Parallel()
	got := dataDirFor("/var/lib/atlas-agent", "production")
	want := filepath.Join("/var/lib/atlas-agent", "relationships", "production")
	if got != want {
		t.Errorf("dataDirFor(production) = %q, want %q", got, want)
	}
}

func TestDiscoverRelationshipsAlwaysIncludesDefault(t *testing.T) {
	cfg := Config{ControlPlaneURL: "https://cp.example:8443", Token: "atlas_enroll_x", DataDir: t.TempDir()}

	rels, err := discoverRelationships(cfg)
	if err != nil {
		t.Fatalf("discoverRelationships: %v", err)
	}
	boot, ok := rels[defaultRelationshipID]
	if !ok {
		t.Fatal("discoverRelationships did not include the implicit default relationship")
	}
	if boot.ControlPlaneURL != cfg.ControlPlaneURL || boot.Token != cfg.Token {
		t.Errorf("default relationship bootstrap = %+v, want it derived from cfg's flat fields", boot)
	}
}

func TestDiscoverRelationshipsIncludesEnvNamedRelationships(t *testing.T) {
	t.Setenv("ATLAS_AGENT_RELATIONSHIPS", "production,development")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_PRODUCTION_CONTROL_PLANE_URL", "https://prod:8443")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_DEVELOPMENT_CONTROL_PLANE_URL", "https://dev:8443")

	cfg := Config{DataDir: t.TempDir()}
	rels, err := discoverRelationships(cfg)
	if err != nil {
		t.Fatalf("discoverRelationships: %v", err)
	}
	if len(rels) != 3 {
		t.Fatalf("got %d relationships, want 3 (default + production + development): %+v", len(rels), rels)
	}
	if rels["production"].ControlPlaneURL != "https://prod:8443" {
		t.Errorf("production URL = %q, want https://prod:8443", rels["production"].ControlPlaneURL)
	}
	if rels["development"].ControlPlaneURL != "https://dev:8443" {
		t.Errorf("development URL = %q, want https://dev:8443", rels["development"].ControlPlaneURL)
	}
}

// "default" is reserved for the implicit legacy relationship — an operator
// naming a relationship "default" in ATLAS_AGENT_RELATIONSHIPS is a
// configuration error, not something that should silently shadow or merge
// with the flat-config relationship.
func TestDiscoverRelationshipsRejectsExplicitDefaultID(t *testing.T) {
	t.Setenv("ATLAS_AGENT_RELATIONSHIPS", "default")

	if _, err := discoverRelationships(Config{DataDir: t.TempDir()}); err == nil {
		t.Fatal("discoverRelationships accepted an explicit \"default\" relationship id")
	}
}

// The core DataDir-authoritative precedence rule: once relationship.json
// exists, it governs — a changed bootstrap value (env var, in production;
// the boot argument here) must have no effect.
func TestLoadOrAdoptRelationshipConfigPersistedFileWinsOverBootstrap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	persisted := relationshipPersisted{
		ID: "production", ControlPlaneURL: "https://persisted:8443", Transport: "https",
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(relationshipJSONPath(dir), data, 0o644); err != nil {
		t.Fatalf("write relationship.json fixture: %v", err)
	}

	boot := RelationshipBootstrap{ControlPlaneURL: "https://changed-env-value:8443", Transport: "libp2p"}
	relCfg, err := loadOrAdoptRelationshipConfig("production", dir, boot)
	if err != nil {
		t.Fatalf("loadOrAdoptRelationshipConfig: %v", err)
	}
	if relCfg.ControlPlaneURL != "https://persisted:8443" {
		t.Errorf("ControlPlaneURL = %q, want the persisted value, unaffected by a changed bootstrap value", relCfg.ControlPlaneURL)
	}
	if relCfg.Transport != "https" {
		t.Errorf("Transport = %q, want the persisted value", relCfg.Transport)
	}
}

// Finding B: a relationship.json that exists but fails to parse must be
// treated as unusable — it must never fall back to boot's (environment-
// sourced) values, which would silently re-bootstrap using whatever the
// environment currently says rather than refusing outright.
func TestLoadOrAdoptRelationshipConfigCorruptedFileNeverFallsBackToBootstrap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := os.WriteFile(relationshipJSONPath(dir), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt relationship.json fixture: %v", err)
	}

	boot := RelationshipBootstrap{ControlPlaneURL: "https://should-never-be-used:8443", Token: "should-never-be-used"}
	relCfg, err := loadOrAdoptRelationshipConfig("development", dir, boot)
	if err == nil {
		t.Fatalf("expected an error for corrupted relationship.json, got relCfg = %+v", relCfg)
	}
}

// Without a persisted file yet, bootstrap values govern as-is — this is the
// path a brand-new relationship (or a not-yet-adopted legacy layout) takes.
func TestLoadOrAdoptRelationshipConfigUsesBootstrapWhenNoFileExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	boot := RelationshipBootstrap{ControlPlaneURL: "https://fresh:8443", Token: "atlas_enroll_x", Transport: "https"}
	relCfg, err := loadOrAdoptRelationshipConfig("production", dir, boot)
	if err != nil {
		t.Fatalf("loadOrAdoptRelationshipConfig: %v", err)
	}
	if relCfg.ControlPlaneURL != "https://fresh:8443" || relCfg.Token != "atlas_enroll_x" {
		t.Errorf("relCfg = %+v, want it to use the bootstrap values verbatim", relCfg)
	}
}

func TestPersistRelationshipConfigWritesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	relCfg := relationshipConfig{
		id: "production", dataDir: dir,
		RelationshipBootstrap: RelationshipBootstrap{ControlPlaneURL: "https://prod:8443", Transport: "https"},
	}
	if err := persistRelationshipConfig(relCfg); err != nil {
		t.Fatalf("persistRelationshipConfig: %v", err)
	}
	if _, err := os.Stat(relationshipJSONPath(dir)); err != nil {
		t.Fatalf("relationship.json was not written: %v", err)
	}

	// A second call with different values must not overwrite the first —
	// relationship.json is fixed at first success (see the function's doc).
	changed := relCfg
	changed.ControlPlaneURL = "https://different:8443"
	if err := persistRelationshipConfig(changed); err != nil {
		t.Fatalf("second persistRelationshipConfig: %v", err)
	}

	reloaded, err := loadOrAdoptRelationshipConfig("production", dir, RelationshipBootstrap{})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ControlPlaneURL != "https://prod:8443" {
		t.Errorf("ControlPlaneURL after a second persist call = %q, want the original https://prod:8443 (persist must be write-once)", reloaded.ControlPlaneURL)
	}
}

// relationship.json must never carry a Control-Plane identity/fingerprint
// field — that identity is always re-derived live from the adjacent
// ca-cert.pem (pki.ControlPlaneID), never duplicated on disk.
func TestPersistRelationshipConfigCarriesNoControlPlaneIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	relCfg := relationshipConfig{
		id: "production", dataDir: dir,
		RelationshipBootstrap: RelationshipBootstrap{ControlPlaneURL: "https://prod:8443", Transport: "https"},
	}
	if err := persistRelationshipConfig(relCfg); err != nil {
		t.Fatalf("persistRelationshipConfig: %v", err)
	}

	raw, err := os.ReadFile(relationshipJSONPath(dir))
	if err != nil {
		t.Fatalf("read relationship.json: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"control_plane_id", "fingerprint", "ca_fingerprint"} {
		if _, present := generic[forbidden]; present {
			t.Errorf("relationship.json carries a %q field; Control-Plane identity must only ever come from the pinned CA certificate", forbidden)
		}
	}
}
