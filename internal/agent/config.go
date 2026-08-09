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
	// LibP2PServerAddr is the control plane's full multiaddr, including its
	// Peer ID (".../p2p/<id>"). Required when Transport is "libp2p".
	LibP2PServerAddr string

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
