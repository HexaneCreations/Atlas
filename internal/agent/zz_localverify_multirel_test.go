package agent

import (
	"testing"
)

func TestLocalVerifyMultiRelationshipParsing(t *testing.T) {
	t.Setenv("ATLAS_AGENT_CONTROL_PLANE_URL", "https://local-default:8443")
	t.Setenv("ATLAS_AGENT_TOKEN", "default-token")
	t.Setenv("ATLAS_AGENT_ENVIRONMENT", "default-env")

	t.Setenv("ATLAS_AGENT_RELATIONSHIPS", "local,production")

	t.Setenv("ATLAS_AGENT_RELATIONSHIP_LOCAL_CONTROL_PLANE_URL", "https://local-atlas:8443")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_LOCAL_TOKEN", "local-token")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_LOCAL_ENVIRONMENT", "development")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_LOCAL_TRANSPORT", "https")

	t.Setenv("ATLAS_AGENT_RELATIONSHIP_PRODUCTION_CONTROL_PLANE_URL", "https://prod-atlas:8443")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TOKEN", "prod-token")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_PRODUCTION_ENVIRONMENT", "production")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TRANSPORT", "libp2p")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_PRODUCTION_LIBP2P_RELAY_ADDR", "/ip4/198.51.100.1/tcp/4103/p2p/12D3KooWRelay")
	t.Setenv("ATLAS_AGENT_RELATIONSHIP_PRODUCTION_LIBP2P_SERVER_PEER_ID", "12D3KooWProdServer")

	cfg := LoadConfig()
	if cfg.ControlPlaneURL != "https://local-default:8443" || cfg.Environment != "default-env" {
		t.Fatalf("default relationship not parsed correctly: %+v", cfg)
	}

	rels, err := LoadRelationshipConfigs()
	if err != nil {
		t.Fatalf("LoadRelationshipConfigs: %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("got %d relationships, want 2: %+v", len(rels), rels)
	}

	local, ok := rels["local"]
	if !ok {
		t.Fatal("local relationship missing")
	}
	prod, ok := rels["production"]
	if !ok {
		t.Fatal("production relationship missing")
	}

	if local.ControlPlaneURL != "https://local-atlas:8443" || local.Environment != "development" || local.Transport != "https" {
		t.Errorf("local relationship isolated incorrectly: %+v", local)
	}
	if prod.ControlPlaneURL != "https://prod-atlas:8443" || prod.Environment != "production" || prod.Transport != "libp2p" {
		t.Errorf("production relationship isolated incorrectly: %+v", prod)
	}
	if prod.LibP2PRelayAddr == "" || prod.LibP2PServerPeerID == "" {
		t.Errorf("production libp2p fields not parsed: %+v", prod)
	}

	// One relationship's config must not leak into another's fields.
	if local.LibP2PRelayAddr != "" || local.LibP2PServerPeerID != "" {
		t.Errorf("production's libp2p config leaked into local: %+v", local)
	}
	if local.Token == prod.Token {
		t.Error("local and production tokens are identical — isolation broken")
	}

	t.Logf("default:    %+v", cfg)
	t.Logf("local:      %+v", local)
	t.Logf("production: %+v", prod)
}

func TestLocalVerifyDefaultReservedNameRejected(t *testing.T) {
	t.Setenv("ATLAS_AGENT_RELATIONSHIPS", "default")
	if _, err := discoverRelationships(Config{}); err == nil {
		t.Fatal("expected an error naming a relationship 'default' explicitly")
	}
}
