package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/log"
)

// env builds a LookupFunc over a map, so tests never mutate process state and
// can run in parallel.
func env(vars map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("compiled-in defaults must be valid, got: %v", err)
	}
}

func TestLoadWithNoSourcesReturnsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(config.Options{Lookup: env(nil)})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.Server.Addr(), "127.0.0.1:8080"; got != want {
		t.Errorf("Server.Addr() = %q, want %q", got, want)
	}
	if cfg.Environment != config.EnvDevelopment {
		t.Errorf("Environment = %q, want development", cfg.Environment)
	}
}

// Defaulting to loopback is a security property, not a preference: Atlas
// exposes a complete infrastructure inventory.
func TestDefaultBindAddressIsLoopback(t *testing.T) {
	t.Parallel()

	if got := config.Default().Server.Host; got != "127.0.0.1" {
		t.Errorf("default bind host = %q, want 127.0.0.1", got)
	}
}

func TestEnvironmentOverridesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_SERVER_HOST":               "0.0.0.0",
		"ATLAS_SERVER_PORT":               "9000",
		"ATLAS_SERVER_READ_TIMEOUT":       "45s",
		"ATLAS_SERVER_ALLOWED_ORIGINS":    "https://atlas.example.com, https://ops.example.com",
		"ATLAS_DATABASE_MAX_CONNS":        "64",
		"ATLAS_DATABASE_MIGRATE_ON_START": "false",
		"ATLAS_LOGGING_LEVEL":             "debug",
		"ATLAS_LOGGING_FORMAT":            "text",
	})})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got, want := cfg.Server.Addr(), "0.0.0.0:9000"; got != want {
		t.Errorf("Addr() = %q, want %q", got, want)
	}
	if got, want := cfg.Server.ReadTimeout, 45*time.Second; got != want {
		t.Errorf("ReadTimeout = %v, want %v", got, want)
	}
	want := []string{"https://atlas.example.com", "https://ops.example.com"}
	if !slices.Equal(cfg.Server.AllowedOrigins, want) {
		t.Errorf("AllowedOrigins = %v, want %v", cfg.Server.AllowedOrigins, want)
	}
	if cfg.Database.MaxConns != 64 {
		t.Errorf("MaxConns = %d, want 64", cfg.Database.MaxConns)
	}
	if cfg.Database.MigrateOnStart {
		t.Error("MigrateOnStart = true, want false")
	}
	if cfg.Logging.Format != log.FormatText || cfg.Logging.Level != "debug" {
		t.Errorf("Logging = %+v, want debug/text", cfg.Logging)
	}
}

func TestPrecedenceDefaultsThenFileThenEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "atlas.yaml")
	yaml := `
environment: staging
server:
  port: 7000
  read_timeout: 25s
database:
  name: atlas_staging
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(config.Options{
		Path: path,
		// Environment overrides the file's port; the file's read_timeout and
		// database name survive; untouched keys keep their defaults.
		Lookup: env(map[string]string{"ATLAS_SERVER_PORT": "9999"}),
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Port != 9999 {
		t.Errorf("Port = %d, want 9999 (env must beat file)", cfg.Server.Port)
	}
	if got, want := cfg.Server.ReadTimeout, 25*time.Second; got != want {
		t.Errorf("ReadTimeout = %v, want %v (file must beat default)", got, want)
	}
	if cfg.Database.Name != "atlas_staging" {
		t.Errorf("Database.Name = %q, want atlas_staging", cfg.Database.Name)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want the default 127.0.0.1", cfg.Server.Host)
	}
	if cfg.Environment != config.EnvStaging {
		t.Errorf("Environment = %q, want staging", cfg.Environment)
	}
}

func TestUnknownFileKeyIsRejected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "atlas.yaml")
	if err := os.WriteFile(path, []byte("server:\n  prot: 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load(config.Options{Path: path, Lookup: env(nil)})
	if err == nil {
		t.Fatal("Load() accepted an unknown key; a typo must fail startup")
	}
	if !strings.Contains(err.Error(), "prot") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// Secrets arrive as mounted files in every orchestrator Atlas targets.
func TestPasswordFromSecretFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	secret := filepath.Join(dir, "db-password")
	if err := os.WriteFile(secret, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_DATABASE_PASSWORD_FILE": secret,
	})})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.Database.Password, "s3cr3t"; got != want {
		t.Errorf("Password = %q, want %q (trailing newline must be trimmed)", got, want)
	}
}

func TestMissingSecretFileFailsLoudly(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_DATABASE_PASSWORD_FILE": "/nonexistent/atlas-secret",
	})})
	if err == nil {
		t.Fatal("Load() ignored an unreadable secret file")
	}
}

// A password must not be expressible in the file that gets committed.
func TestPasswordCannotBeSetFromYAML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "atlas.yaml")
	if err := os.WriteFile(path, []byte("database:\n  password: committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(config.Options{Path: path, Lookup: env(nil)}); err == nil {
		t.Fatal("YAML was allowed to set database.password")
	}
}

func TestProductionHardening(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		"ATLAS_ENVIRONMENT":       "production",
		"ATLAS_DATABASE_PASSWORD": "pw",
		"ATLAS_DATABASE_SSL_MODE": "verify-full",
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	tests := []struct {
		name    string
		vars    map[string]string
		wantErr string
	}{
		{"valid production", base, ""},
		{
			name:    "non-verifying ssl mode",
			vars:    with(map[string]string{"ATLAS_DATABASE_SSL_MODE": "require"}),
			wantErr: "verify-ca or verify-full",
		},
		{
			name:    "ssl disabled",
			vars:    with(map[string]string{"ATLAS_DATABASE_SSL_MODE": "disable"}),
			wantErr: "does not verify",
		},
		{
			name:    "missing password",
			vars:    with(map[string]string{"ATLAS_DATABASE_PASSWORD": ""}),
			wantErr: "must be set in production",
		},
		{
			name:    "wildcard cors",
			vars:    with(map[string]string{"ATLAS_SERVER_ALLOWED_ORIGINS": "*"}),
			wantErr: "wildcard",
		},
		{
			name:    "plaintext origin",
			vars:    with(map[string]string{"ATLAS_SERVER_ALLOWED_ORIGINS": "http://ops.example.com"}),
			wantErr: "plaintext http",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(config.Options{Lookup: env(tt.vars)})
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v, want success", err)
			case tt.wantErr == "":
			case err == nil:
				t.Fatalf("Load() succeeded, want error containing %q", tt.wantErr)
			case !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Development is intentionally permissive so a laptop needs no TLS setup.
func TestDevelopmentAllowsWeakSSLAndWildcardOrigin(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_DATABASE_SSL_MODE":      "disable",
		"ATLAS_SERVER_ALLOWED_ORIGINS": "*",
	})})
	if err != nil {
		t.Fatalf("development config rejected: %v", err)
	}
}

func TestValidationReportsAllProblemsAtOnce(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_SERVER_PORT":               "70000",
		"ATLAS_DATABASE_NAME":             "",
		"ATLAS_EVENT_BUS_BUFFER_SIZE":     "0",
		"ATLAS_COLLECTION_MAX_CONCURRENT": "0",
	})})
	if err == nil {
		t.Fatal("Load() accepted an invalid configuration")
	}
	for _, want := range []string{"server.port", "database.name", "event_bus.buffer_size", "collection.max_concurrent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
}

func TestInvalidEnvValueNamesTheVariable(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_SERVER_READ_TIMEOUT": "half a minute",
	})})
	if err == nil {
		t.Fatal("Load() accepted an unparseable duration")
	}
	if !strings.Contains(err.Error(), "ATLAS_SERVER_READ_TIMEOUT") {
		t.Errorf("error should name the variable, got: %v", err)
	}
}

func TestCollectionTimeoutMustFitWithinInterval(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_COLLECTION_DEFAULT_INTERVAL": "5s",
		"ATLAS_COLLECTION_TIMEOUT":          "30s",
	})})
	if err == nil {
		t.Fatal("a collection timeout longer than its interval must be rejected")
	}
}

func TestDSNRedaction(t *testing.T) {
	t.Parallel()

	db := config.Default().Database
	db.Password = "hunter2"

	if !strings.Contains(db.DSN(), "hunter2") {
		t.Error("DSN() must contain the password; it is what the pool connects with")
	}
	if strings.Contains(db.SafeDSN(), "hunter2") {
		t.Errorf("SafeDSN() leaked the password: %s", db.SafeDSN())
	}
	for _, want := range []string{"sslmode=", "application_name=atlas"} {
		if !strings.Contains(db.DSN(), want) {
			t.Errorf("DSN() missing %q: %s", want, db.SafeDSN())
		}
	}
}

// The documented environment reference is generated from the struct, so this
// guards against a field being added without an override.
func TestEnvVarsCoversEverySection(t *testing.T) {
	t.Parallel()

	vars := config.EnvVars()
	for _, want := range []string{
		"ATLAS_ENVIRONMENT",
		"ATLAS_SERVER_PORT",
		"ATLAS_DATABASE_PASSWORD",
		"ATLAS_LOGGING_LEVEL",
		"ATLAS_EVENT_BUS_BUFFER_SIZE",
		"ATLAS_COLLECTION_DEFAULT_INTERVAL",
	} {
		if !slices.Contains(vars, want) {
			t.Errorf("EnvVars() missing %q", want)
		}
	}
	for _, name := range vars {
		if !strings.HasPrefix(name, "ATLAS_") {
			t.Errorf("variable %q does not carry the ATLAS_ prefix", name)
		}
	}
}

// A setting whose own name ends in _FILE collides with the secret-file
// convention: ATLAS_NODE_ID plus the suffix is exactly ATLAS_NODE_ID_FILE.
// The explicitly declared variable must win, or node.id_file is unsettable.
func TestSettingEndingInFileIsNotTreatedAsSecretIndirection(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_NODE_ID_FILE": "/var/lib/atlas/node-id",
	})})
	if err != nil {
		t.Fatalf("Load() error = %v; the value was read as a secret file path", err)
	}
	if got, want := cfg.Node.IDFile, "/var/lib/atlas/node-id"; got != want {
		t.Errorf("Node.IDFile = %q, want %q", got, want)
	}
	if cfg.Node.ID != "" {
		t.Errorf("Node.ID = %q; it must not be populated from the id_file path", cfg.Node.ID)
	}
}

// The indirection must still work for settings that have no _FILE sibling.
func TestSecretIndirectionStillWorksForOtherSettings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	secret := filepath.Join(dir, "pw")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(config.Options{Lookup: env(map[string]string{
		"ATLAS_DATABASE_PASSWORD_FILE": secret,
	})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Password != "s3cret" {
		t.Errorf("Password = %q, want s3cret", cfg.Database.Password)
	}
}
