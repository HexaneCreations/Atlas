package relay

import (
	"os"
	"strings"
)

// Config is Atlas Relay's runtime configuration, read from environment
// variables prefixed ATLAS_RELAY_.
type Config struct {
	DataDir     string
	ListenAddrs []string
	LogLevel    string
}

// LoadConfig reads configuration from the environment, applying defaults.
func LoadConfig() Config {
	return Config{
		DataDir:     getenv("ATLAS_RELAY_DATA_DIR", "/var/lib/atlas-relay"),
		ListenAddrs: splitList(getenv("ATLAS_RELAY_LISTEN_ADDRS", "/ip4/0.0.0.0/tcp/4103")),
		LogLevel:    getenv("ATLAS_RELAY_LOG_LEVEL", "info"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
