package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/platform/config"
)

// `atlas-server config` runs inside production containers. If it could print
// the database password, it would be a way to read a secret out of a running
// deployment.
func TestRenderNeverExposesThePassword(t *testing.T) {
	t.Parallel()

	const secret = "sup3r-s3cret-passw0rd"

	cfg := config.Default()
	cfg.Database.Password = secret

	rendered := cfg.Render()
	encoded, err := json.Marshal(rendered)
	if err != nil {
		t.Fatalf("marshal rendered config: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("Render() leaked the database password: %s", encoded)
	}

	db, ok := rendered["database"].(map[string]any)
	if !ok {
		t.Fatal("rendered config has no database section")
	}
	if _, present := db["password"]; present {
		t.Error("the database section contains a password key")
	}

	// Marshalling the whole Config directly must be safe too, since that is
	// what a future caller is most likely to reach for.
	direct, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(direct), secret) {
		t.Errorf("json.Marshal(Config) leaked the password: %s", direct)
	}
}

// Durations must round-trip as the same syntax the configuration file accepts,
// so an operator can compare output against input without converting units.
func TestRenderFormatsDurationsAsConfigurationSyntax(t *testing.T) {
	t.Parallel()

	rendered := defaultConfig().Render()

	server, ok := rendered["server"].(map[string]any)
	if !ok {
		t.Fatal("rendered config has no server section")
	}

	tests := map[string]string{
		"read_timeout":     "30s",
		"idle_timeout":     "2m0s",
		"shutdown_timeout": "20s",
	}
	for key, want := range tests {
		if got := server[key]; got != want {
			t.Errorf("server.%s = %v, want %q", key, got, want)
		}
	}

	db, _ := rendered["database"].(map[string]any)
	if got := db["max_conn_lifetime"]; got != "1h0m0s" {
		t.Errorf("database.max_conn_lifetime = %v, want 1h0m0s", got)
	}
	// Non-duration numbers must stay numbers.
	if got := db["max_conns"]; got != int32(16) {
		t.Errorf("database.max_conns = %v (%T), want the numeric 16", got, got)
	}
}

func TestRenderIncludesEverySection(t *testing.T) {
	t.Parallel()

	rendered := defaultConfig().Render()
	for _, section := range []string{"environment", "server", "database", "logging", "event_bus", "collection"} {
		if _, ok := rendered[section]; !ok {
			t.Errorf("rendered config is missing %q", section)
		}
	}
}

// A nil slice must render as an empty list, not null, so the output is stable
// for anything consuming it.
func TestRenderNormalisesNilSlices(t *testing.T) {
	t.Parallel()

	rendered := defaultConfig().Render()
	server, _ := rendered["server"].(map[string]any)

	origins, ok := server["allowed_origins"].([]any)
	if !ok {
		t.Fatalf("allowed_origins = %v (%T), want a list", server["allowed_origins"], server["allowed_origins"])
	}
	if len(origins) != 0 {
		t.Errorf("allowed_origins = %v, want empty", origins)
	}
}

// defaultConfig returns an addressable copy, since Render has a pointer
// receiver.
func defaultConfig() *config.Config {
	cfg := config.Default()
	return &cfg
}
