package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// defaultRelationshipID names the Agent's implicit, backward-compatible
// relationship: the one that reads the legacy flat ATLAS_AGENT_* vars and
// lives at DataDir's root rather than under DataDir/relationships/. Reserved
// — an operator cannot name an ATLAS_AGENT_RELATIONSHIPS entry "default".
const defaultRelationshipID = "default"

// RelationshipBootstrap is env-sourced, bootstrap-only configuration for one
// Control-Plane relationship. Consulted only until relationship.json exists
// for it — see loadOrAdoptRelationshipConfig. Token and CABundlePath are
// deliberately never persisted to relationship.json and are re-read from the
// environment on every start, exactly like the pre-Phase-3 single-relationship
// Agent already did: Token is a one-time bootstrap secret, and CABundlePath
// names an operator-managed external file whose path may legitimately need
// to keep being supplied.
type RelationshipBootstrap struct {
	ControlPlaneURL string
	Token           string
	CABundlePath    string
	// Environment tags every envelope sent to this control plane. It is
	// per-relationship because the same host is legitimately "development"
	// to one control plane and "production" to another. Empty falls back to
	// the process-wide ATLAS_AGENT_ENVIRONMENT.
	Environment        string
	Transport          string
	LibP2PServerAddr   string
	LibP2PRelayAddr    string
	LibP2PServerPeerID string

	// InsecureBootstrap permits first-contact enrollment with no CA bundle,
	// trusting whatever certificate the endpoint presents and pinning it.
	// Required explicitly because the alternative — doing it silently — puts
	// every first enrollment in a fleet one intercepted connection away from
	// a control plane the operator never chose. Like Token and
	// CABundlePath, it is bootstrap-only and never persisted.
	InsecureBootstrap bool

	AgentOpsContainerLogsDisabled bool
}

// relationshipConfig is a relationship's resolved configuration: from
// relationship.json if present, else from a RelationshipBootstrap. dataDir is
// derived, never persisted.
type relationshipConfig struct {
	id      string
	dataDir string
	RelationshipBootstrap
}

// dataDirFor returns the DataDir a relationship's certs/CA/spool/config live
// under. The default relationship keeps the pre-Phase-3 flat layout
// (agentDataDir itself, unmoved); every other relationship gets its own
// subdirectory — this is what makes "preserve the legacy DataDir layout" and
// "add a relationship" simultaneously true.
func dataDirFor(agentDataDir, id string) string {
	if id == defaultRelationshipID {
		return agentDataDir
	}
	return filepath.Join(agentDataDir, "relationships", id)
}

// relationshipBootstrapFromConfig adapts the Agent's existing flat Config
// into a RelationshipBootstrap for the implicit default relationship —
// Config itself is untouched by Phase 3, so every existing Config{} literal
// (production and test) keeps compiling and behaving identically.
func relationshipBootstrapFromConfig(cfg Config) RelationshipBootstrap {
	return RelationshipBootstrap{
		ControlPlaneURL:               cfg.ControlPlaneURL,
		Token:                         cfg.Token,
		CABundlePath:                  cfg.CABundlePath,
		InsecureBootstrap:             cfg.InsecureBootstrap,
		Environment:                   cfg.Environment,
		Transport:                     cfg.Transport,
		LibP2PServerAddr:              cfg.LibP2PServerAddr,
		LibP2PRelayAddr:               cfg.LibP2PRelayAddr,
		LibP2PServerPeerID:            cfg.LibP2PServerPeerID,
		AgentOpsContainerLogsDisabled: cfg.AgentOpsContainerLogsDisabled,
	}
}

// validRelationshipID matches exactly the characters safe to use as a
// filesystem directory name component (see dataDirFor): letters, digits,
// underscore, hyphen. Nothing else — no ".", "..", "/", "\", whitespace, or
// any other character — is permitted, which is what makes it impossible for
// dataDirFor to ever escape DataDir/relationships/ or collide with another
// relationship's or the default relationship's own directory.
var validRelationshipID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validateRelationshipID checked before any directory is derived from id
// (see dataDirFor's callers in discoverRelationships and New) — an invalid
// id is a configuration error to reject outright, never sanitized into
// something else. Sanitizing (e.g. stripping bad characters) would still let
// two different operator-supplied ids collide on the same sanitized
// directory; rejecting does not.
func validateRelationshipID(id string) error {
	if id == "" {
		return fmt.Errorf("relationship id must not be empty")
	}
	if !validRelationshipID.MatchString(id) {
		return fmt.Errorf("relationship id %q is invalid: only letters, digits, '_' and '-' are allowed", id)
	}
	return nil
}

// discoverRelationships returns every relationship this Agent process should
// attempt: always "default" (from cfg's legacy flat fields), plus any named
// in ATLAS_AGENT_RELATIONSHIPS. An explicit "default" entry in that list is a
// configuration error — the id is reserved. Every other id is validated
// before it can ever reach dataDirFor.
func discoverRelationships(cfg Config) (map[string]RelationshipBootstrap, error) {
	out := map[string]RelationshipBootstrap{
		defaultRelationshipID: relationshipBootstrapFromConfig(cfg),
	}
	extra, err := LoadRelationshipConfigs()
	if err != nil {
		return nil, err
	}
	for id, boot := range extra {
		if id == defaultRelationshipID {
			return nil, fmt.Errorf("relationship id %q is reserved for the implicit default relationship; choose a different id", id)
		}
		if err := validateRelationshipID(id); err != nil {
			return nil, fmt.Errorf("invalid relationship id: %w", err)
		}
		out[id] = boot
	}
	return out, nil
}

// relationshipPersisted is relationship.json's on-disk shape. It deliberately
// carries no Control-Plane identity/fingerprint field: that identity is
// always re-derived live from the adjacent ca-cert.pem (see
// pki.ControlPlaneID), never duplicated here — a single source of truth, per
// the approved Phase 1 clarification extended to this file.
type relationshipPersisted struct {
	ID                            string    `json:"id"`
	ControlPlaneURL               string    `json:"control_plane_url"`
	Environment                   string    `json:"environment,omitempty"`
	Transport                     string    `json:"transport"`
	LibP2PServerAddr              string    `json:"libp2p_server_addr,omitempty"`
	LibP2PRelayAddr               string    `json:"libp2p_relay_addr,omitempty"`
	LibP2PServerPeerID            string    `json:"libp2p_server_peer_id,omitempty"`
	AgentOpsContainerLogsDisabled bool      `json:"agentops_container_logs_disabled"`
	BootstrappedAt                time.Time `json:"bootstrapped_at"`
}

func relationshipJSONPath(dataDir string) string {
	return filepath.Join(dataDir, "relationship.json")
}

// loadOrAdoptRelationshipConfig resolves a relationship's connection config.
// If relationship.json already exists it is authoritative for every field it
// carries — boot's equivalent fields are ignored entirely, so a changed
// env var after first bootstrap has no effect until the file is removed or
// edited by hand. If it does not exist yet, boot is used as-is; the caller
// (bootstrap, in credentials.go) is responsible for calling
// persistRelationshipConfig once bootstrap actually succeeds.
func loadOrAdoptRelationshipConfig(id, dataDir string, boot RelationshipBootstrap) (relationshipConfig, error) {
	path := relationshipJSONPath(dataDir)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var p relationshipPersisted
		if jsonErr := json.Unmarshal(data, &p); jsonErr != nil {
			return relationshipConfig{}, fmt.Errorf("parse %s: %w", path, jsonErr)
		}
		return relationshipConfig{
			id: id, dataDir: dataDir,
			RelationshipBootstrap: RelationshipBootstrap{
				ControlPlaneURL:               p.ControlPlaneURL,
				Token:                         boot.Token,
				CABundlePath:                  boot.CABundlePath,
				InsecureBootstrap:             boot.InsecureBootstrap,
				Environment:                   p.Environment,
				Transport:                     p.Transport,
				LibP2PServerAddr:              p.LibP2PServerAddr,
				LibP2PRelayAddr:               p.LibP2PRelayAddr,
				LibP2PServerPeerID:            p.LibP2PServerPeerID,
				AgentOpsContainerLogsDisabled: p.AgentOpsContainerLogsDisabled,
			},
		}, nil
	case os.IsNotExist(err):
		return relationshipConfig{id: id, dataDir: dataDir, RelationshipBootstrap: boot}, nil
	default:
		return relationshipConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
}

// persistRelationshipConfig writes relationship.json, making relCfg
// authoritative for every subsequent Agent start. A no-op if the file
// already exists — a relationship's resolved connection config is fixed at
// first bootstrap success and never silently drifts if the operator's env
// vars change later.
func persistRelationshipConfig(relCfg relationshipConfig) error {
	path := relationshipJSONPath(relCfg.dataDir)
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	p := relationshipPersisted{
		ID:                            relCfg.id,
		ControlPlaneURL:               relCfg.ControlPlaneURL,
		Environment:                   relCfg.Environment,
		Transport:                     relCfg.Transport,
		LibP2PServerAddr:              relCfg.LibP2PServerAddr,
		LibP2PRelayAddr:               relCfg.LibP2PRelayAddr,
		LibP2PServerPeerID:            relCfg.LibP2PServerPeerID,
		AgentOpsContainerLogsDisabled: relCfg.AgentOpsContainerLogsDisabled,
		BootstrappedAt:                time.Now().UTC(),
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(relCfg.dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
