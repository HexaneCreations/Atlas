package agent

import "testing"

// TestLoadConfigReadsRendezvousAndLegacyLibP2PVars proves the new
// discovery-based vars and the deprecated static-target var all load
// independently, so an operator's existing ATLAS_AGENT_LIBP2P_SERVER_ADDR
// config keeps working untouched alongside (or instead of) the new pair.
func TestLoadConfigReadsRendezvousAndLegacyLibP2PVars(t *testing.T) {
	for k, v := range map[string]string{
		"ATLAS_AGENT_LIBP2P_SERVER_ADDR":    "/ip4/203.0.113.5/tcp/4102/p2p/12D3KooWlegacy",
		"ATLAS_AGENT_LIBP2P_RELAY_ADDR":     "/ip4/203.0.113.9/tcp/4103/p2p/12D3KooWrelay",
		"ATLAS_AGENT_LIBP2P_SERVER_PEER_ID": "12D3KooWserver",
	} {
		t.Setenv(k, v)
	}

	cfg := LoadConfig()

	if cfg.LibP2PServerAddr != "/ip4/203.0.113.5/tcp/4102/p2p/12D3KooWlegacy" {
		t.Errorf("LibP2PServerAddr = %q, want the legacy value preserved", cfg.LibP2PServerAddr)
	}
	if cfg.LibP2PRelayAddr != "/ip4/203.0.113.9/tcp/4103/p2p/12D3KooWrelay" {
		t.Errorf("LibP2PRelayAddr = %q, want the configured relay address", cfg.LibP2PRelayAddr)
	}
	if cfg.LibP2PServerPeerID != "12D3KooWserver" {
		t.Errorf("LibP2PServerPeerID = %q, want the configured peer id", cfg.LibP2PServerPeerID)
	}
}

// TestLoadConfigLibP2PVarsDefaultEmpty proves an operator who sets none of
// this config gets an explicit empty state, not a surprising default that
// would make ATLAS_AGENT_TRANSPORT=libp2p silently pick a target.
func TestLoadConfigLibP2PVarsDefaultEmpty(t *testing.T) {
	cfg := LoadConfig()

	if cfg.LibP2PServerAddr != "" || cfg.LibP2PRelayAddr != "" || cfg.LibP2PServerPeerID != "" {
		t.Errorf("expected all libp2p target config to default empty, got %+v", cfg)
	}
}
