package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the agent's runtime configuration, read from environment
// variables prefixed ATLAS_AGENT_.
type Config struct {
	ControlPlaneURL string
	Token           string
	DataDir         string
	CABundlePath    string
	NodeID          string
	Environment     string
	// InsecureBootstrap permits first-contact enrollment with no CA bundle
	// configured. See RelationshipBootstrap.InsecureBootstrap.
	InsecureBootstrap bool

	// Transport selects what carries requests to the control plane: "https"
	// (default, TCP + mTLS) or "libp2p" (POC — dial by Peer ID; see
	// docs/adr/0012-connect-by-identity.md). Only the dial changes; the
	// enrollment, renewal and telemetry HTTP calls are identical either way.
	Transport string
	// LibP2PServerAddr is a full manually-assembled control plane multiaddr,
	// including its Peer ID (".../p2p/<id>", or a
	// ".../p2p-circuit/p2p/<id>" through a relay). Deprecated: this was the
	// only option before rendezvous discovery existed (below) and still
	// works standalone for backward compatibility, but requires an operator
	// to hand-build a fresh circuit address per Server. Ignored when
	// LibP2PRelayAddr and LibP2PServerPeerID are both set.
	LibP2PServerAddr string
	// LibP2PRelayAddr is the Atlas Relay's multiaddr, including its Peer ID
	// — the same value for every Agent in a fleet, used to look up the
	// control plane's current direct/circuit addresses instead of requiring
	// one manually per Server. See ADR-0012's rendezvous-via-relay design.
	LibP2PRelayAddr string
	// LibP2PServerPeerID is the control plane's Peer ID (no address). Paired
	// with LibP2PRelayAddr, this is all an Agent needs to discover and dial
	// the Server, whether it's directly reachable or only via the relay.
	LibP2PServerPeerID string

	CollectionInterval time.Duration
	CollectionTimeout  time.Duration
	InventoryInterval  time.Duration

	// AgentOpsContainerLogsDisabled is a local, explicit authorization gate
	// for the one privileged AgentOps operation that exists: independent of,
	// and never implied by, a control plane successfully authenticating.
	// Phrased as a "disabled" flag rather than "allowed" so the zero value —
	// what a Config{} literal not built through LoadConfig gets — preserves
	// current behavior (allowed) rather than silently denying every AgentOps
	// request. An operator sets it true to revoke this Agent's participation
	// without touching the control plane.
	AgentOpsContainerLogsDisabled bool

	// SecretRedactionDisabled transmits process command lines and cron
	// commands exactly as the host reports them, credentials included.
	// Phrased as a "disabled" flag so the zero value — what a Config{}
	// literal not built through LoadConfig gets — is the safe one.
	SecretRedactionDisabled bool

	LogLevel string
}

// LoadConfig reads configuration from the environment, applying defaults.
func LoadConfig() Config {
	cfg := Config{
		ControlPlaneURL:               getenv("ATLAS_AGENT_CONTROL_PLANE_URL", "https://127.0.0.1:8443"),
		Token:                         os.Getenv("ATLAS_AGENT_TOKEN"),
		DataDir:                       getenv("ATLAS_AGENT_DATA_DIR", "/var/lib/atlas-agent"),
		CABundlePath:                  os.Getenv("ATLAS_AGENT_CA_BUNDLE"),
		InsecureBootstrap:             getenvBool("ATLAS_AGENT_INSECURE_BOOTSTRAP", false),
		NodeID:                        os.Getenv("ATLAS_AGENT_NODE_ID"),
		Environment:                   os.Getenv("ATLAS_AGENT_ENVIRONMENT"),
		Transport:                     getenv("ATLAS_AGENT_TRANSPORT", "https"),
		LibP2PServerAddr:              os.Getenv("ATLAS_AGENT_LIBP2P_SERVER_ADDR"),
		LibP2PRelayAddr:               os.Getenv("ATLAS_AGENT_LIBP2P_RELAY_ADDR"),
		LibP2PServerPeerID:            os.Getenv("ATLAS_AGENT_LIBP2P_SERVER_PEER_ID"),
		CollectionInterval:            getenvDuration("ATLAS_AGENT_COLLECTION_INTERVAL", 15*time.Second),
		CollectionTimeout:             getenvDuration("ATLAS_AGENT_COLLECTION_TIMEOUT", 10*time.Second),
		InventoryInterval:             getenvDuration("ATLAS_AGENT_INVENTORY_INTERVAL", 60*time.Second),
		AgentOpsContainerLogsDisabled: getenvBool("ATLAS_AGENT_AGENTOPS_CONTAINER_LOGS_DISABLED", false),
		SecretRedactionDisabled:       getenvBool("ATLAS_AGENT_SECRET_REDACTION_DISABLED", false),
		LogLevel:                      getenv("ATLAS_AGENT_LOG_LEVEL", "info"),
	}
	return cfg
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getenvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// relationshipsEnvVar lists additional Control-Plane relationship ids,
// comma-separated (e.g. "production,development"). Each id's own bootstrap
// config is read from ATLAS_AGENT_RELATIONSHIP_<ID>_* (id upper-cased,
// non-alphanumeric characters replaced with "_"). Consulted only for
// bootstrap — see relationship.go's DataDir-authoritative precedence rule;
// once a relationship has bootstrapped once, relationship.json governs and
// these vars are ignored.
const relationshipsEnvVar = "ATLAS_AGENT_RELATIONSHIPS"

// LoadRelationshipConfigs reads every relationship named in
// ATLAS_AGENT_RELATIONSHIPS from the environment. The implicit "default"
// relationship is not included here — it is always derived from Config's
// existing flat fields (see relationshipBootstrapFromConfig in
// relationship.go), so this function and Config/LoadConfig never interact.
func LoadRelationshipConfigs() (map[string]RelationshipBootstrap, error) {
	raw := os.Getenv(relationshipsEnvVar)
	if raw == "" {
		return nil, nil
	}

	out := make(map[string]RelationshipBootstrap)
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("%s: relationship id %q listed more than once", relationshipsEnvVar, id)
		}
		out[id] = loadRelationshipBootstrapEnv(id)
	}
	return out, nil
}

// relationshipEnvPrefix derives an id's environment variable prefix.
// Non-alphanumeric characters become "_" so an id like "prod-eu" still
// produces a valid, unambiguous variable name.
func relationshipEnvPrefix(id string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(id) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return "ATLAS_AGENT_RELATIONSHIP_" + b.String() + "_"
}

func loadRelationshipBootstrapEnv(id string) RelationshipBootstrap {
	prefix := relationshipEnvPrefix(id)
	return RelationshipBootstrap{
		ControlPlaneURL:               os.Getenv(prefix + "CONTROL_PLANE_URL"),
		Token:                         os.Getenv(prefix + "TOKEN"),
		CABundlePath:                  os.Getenv(prefix + "CA_BUNDLE"),
		InsecureBootstrap:             getenvBool(prefix+"INSECURE_BOOTSTRAP", false),
		Environment:                   os.Getenv(prefix + "ENVIRONMENT"),
		Transport:                     getenv(prefix+"TRANSPORT", "https"),
		LibP2PServerAddr:              os.Getenv(prefix + "LIBP2P_SERVER_ADDR"),
		LibP2PRelayAddr:               os.Getenv(prefix + "LIBP2P_RELAY_ADDR"),
		LibP2PServerPeerID:            os.Getenv(prefix + "LIBP2P_SERVER_PEER_ID"),
		AgentOpsContainerLogsDisabled: getenvBool(prefix+"AGENTOPS_CONTAINER_LOGS_DISABLED", false),
	}
}
