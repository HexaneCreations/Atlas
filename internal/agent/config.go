package agent

import (
	"os"
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

	LogLevel string
}

// LoadConfig reads configuration from the environment, applying defaults.
func LoadConfig() Config {
	cfg := Config{
		ControlPlaneURL:    getenv("ATLAS_AGENT_CONTROL_PLANE_URL", "https://127.0.0.1:8443"),
		Token:              os.Getenv("ATLAS_AGENT_TOKEN"),
		DataDir:            getenv("ATLAS_AGENT_DATA_DIR", "/var/lib/atlas-agent"),
		CABundlePath:       os.Getenv("ATLAS_AGENT_CA_BUNDLE"),
		NodeID:             os.Getenv("ATLAS_AGENT_NODE_ID"),
		Environment:        os.Getenv("ATLAS_AGENT_ENVIRONMENT"),
		Transport:          getenv("ATLAS_AGENT_TRANSPORT", "https"),
		LibP2PServerAddr:   os.Getenv("ATLAS_AGENT_LIBP2P_SERVER_ADDR"),
		LibP2PRelayAddr:    os.Getenv("ATLAS_AGENT_LIBP2P_RELAY_ADDR"),
		LibP2PServerPeerID: os.Getenv("ATLAS_AGENT_LIBP2P_SERVER_PEER_ID"),
		CollectionInterval: getenvDuration("ATLAS_AGENT_COLLECTION_INTERVAL", 15*time.Second),
		CollectionTimeout:  getenvDuration("ATLAS_AGENT_COLLECTION_TIMEOUT", 10*time.Second),
		InventoryInterval:  getenvDuration("ATLAS_AGENT_INVENTORY_INTERVAL", 60*time.Second),
		LogLevel:           getenv("ATLAS_AGENT_LOG_LEVEL", "info"),
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
