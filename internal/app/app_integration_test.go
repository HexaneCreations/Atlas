//go:build integration

// End-to-end tests that boot a real Atlas process against a real database.
//
// These are the tests that would have caught a wiring mistake no unit test can
// see: a component registered in the wrong order, a migration that never runs,
// a handler that needs a dependency the composition root did not pass.
//
//	make db-up && make test-integration
package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/app"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/hexane/atlas/internal/platform/log"
	storageuser "github.com/hexane/atlas/internal/storage/user"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

// bootServer starts a complete Atlas against a fresh database and returns its
// base URL. Everything is torn down when the test finishes.
func bootServer(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}

	cfg := config.Default()
	cfg.Server.Port = 0 // let the kernel choose a free port
	cfg.Database.Host = parsed.ConnConfig.Host
	cfg.Database.Port = int(parsed.ConnConfig.Port)
	cfg.Database.Name = parsed.ConnConfig.Database
	cfg.Database.User = parsed.ConnConfig.User
	cfg.Database.Password = parsed.ConnConfig.Password
	cfg.Database.SSLMode = "disable"
	cfg.Database.MigrateOnStart = true

	if err := cfg.Validate(); err != nil {
		t.Fatalf("test configuration is invalid: %v", err)
	}

	instance, err := app.New(&cfg, log.Discard())
	if err != nil {
		t.Fatalf("app.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- instance.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() returned %v on shutdown, want a clean exit", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Atlas did not shut down within 30s")
		}
	})

	// Run is asynchronous and the server binds inside it, so the address is
	// only meaningful once binding has happened. With port 0 the configured
	// address ends in ":0" until then.
	base := "http://" + waitForBoundAddress(t, instance)
	waitUntilReady(t, base)
	return base
}

func waitForBoundAddress(t *testing.T, instance *app.App) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if addr := instance.Addr(); !strings.HasSuffix(addr, ":0") {
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the HTTP server never bound a port")
	return ""
}

func waitUntilReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Atlas never became ready")
}

// authenticatedTestClient creates a human user with a fleet-wide grant of
// role, logs in over the real HTTP auth endpoints, and returns an
// *http.Client whose cookie jar carries the resulting session — the same
// path a browser follows, exercised for real rather than by injecting a
// principal directly.
//
// Node-scoped endpoints (health/score, cost/estimate, capacity/summary,
// signals, containers, and the rest reachable through
// [Handler.requireScope]) now require this; every test in this package that
// calls one directly needs a client from here instead of the bare
// http.Get/http.DefaultClient the pre-authentication tests used. See
// docs/adr/0011-deferred-rbac.md and internal/core/user — this is a
// completely separate identity domain from an Agent's own libp2p Peer ID.
func authenticatedTestClient(t *testing.T, base, role string) *http.Client {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Unique per call: bootServer reuses one persistent database across
	// tests and reruns (unlike the storage-layer integration tests, which
	// each create a fresh one), so a fixed username collides with the
	// unique index on username the moment this runs twice.
	username := "it-operator-" + strings.ToLower(role) + "-" + id.New()
	const password = "integration-test-password"

	hash, err := coreuser.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	repo := storageuser.NewRepository(pool)
	if err := repo.CreateUser(context.Background(), coreuser.User{Username: username, PasswordHash: hash}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := repo.ByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	grant := coreuser.GrantSpec{UserID: u.ID, FleetWide: true, Role: role, GrantedBy: "integration-test"}
	if err := repo.Grant(context.Background(), grant, time.Now()); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}

	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	resp, err := client.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}

	return client
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s (%q): %v", url, body, err)
	}
	return resp.StatusCode, decoded
}

// The whole point of the composition root: everything comes up in order and
// the API answers.
func TestAtlasBootsAndServes(t *testing.T) {
	base := bootServer(t)

	t.Run("liveness", func(t *testing.T) {
		status, body := getJSON(t, base+"/healthz")
		if status != http.StatusOK {
			t.Errorf("/healthz = %d, want 200", status)
		}
		if body["status"] != "ok" {
			t.Errorf("body = %v", body)
		}
	})

	t.Run("readiness reports a healthy database", func(t *testing.T) {
		status, body := getJSON(t, base+"/readyz")
		if status != http.StatusOK {
			t.Errorf("/readyz = %d, want 200", status)
		}
		if body["status"] != "healthy" {
			t.Errorf("status = %v, want healthy", body["status"])
		}
	})

	t.Run("system info", func(t *testing.T) {
		status, body := getJSON(t, base+"/api/v1/system/info")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if body["api_version"] != "v1" {
			t.Errorf("api_version = %v", body["api_version"])
		}
		if uptime, _ := body["uptime_seconds"].(float64); uptime <= 0 {
			t.Errorf("uptime_seconds = %v, want positive", body["uptime_seconds"])
		}
	})

	t.Run("system health names the database check", func(t *testing.T) {
		_, body := getJSON(t, base+"/api/v1/system/health")
		checks, _ := body["checks"].([]any)
		if len(checks) == 0 {
			t.Fatal("no health checks were reported")
		}
		var found bool
		for _, c := range checks {
			check, _ := c.(map[string]any)
			if check["name"] == "database" {
				found = true
				if check["status"] != "healthy" {
					t.Errorf("database check = %v, want healthy", check["status"])
				}
				if check["critical"] != true {
					t.Error("the database check should be critical")
				}
			}
		}
		if !found {
			t.Errorf("no database check in %v", checks)
		}
	})

	t.Run("runtime telemetry reflects a live pool", func(t *testing.T) {
		_, body := getJSON(t, base+"/api/v1/system/runtime")
		db, _ := body["database"].(map[string]any)
		if maxConns, _ := db["max_conns"].(float64); maxConns <= 0 {
			t.Errorf("database.max_conns = %v, want the configured ceiling", db["max_conns"])
		}
	})
}

// Atlas is read-only. A write verb must be refused by the router, in the
// standard envelope, before it reaches any handler.
func TestWriteMethodsAreRefused(t *testing.T) {
	base := bootServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, base+"/api/v1/system/info", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s = %d, want 405", method, resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want the standard JSON envelope", ct)
			}

			body, _ := io.ReadAll(resp.Body)
			var envelope struct {
				Error struct {
					Code      string `json:"code"`
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatalf("body is not the standard envelope: %q", body)
			}
			if envelope.Error.Code != "method_not_allowed" {
				t.Errorf("code = %q, want method_not_allowed", envelope.Error.Code)
			}
			if envelope.Error.RequestID == "" {
				t.Error("error body has no request id")
			}
		})
	}
}

// Migrations must have run as part of startup, not as a manual step.
func TestStartupAppliesMigrations(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}
	bootServer(t)

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var applied int
	err = pool.QueryRow(context.Background(),
		"SELECT count(*) FROM atlas_schema_migrations").Scan(&applied)
	if err != nil {
		t.Fatalf("the migration ledger does not exist after startup: %v", err)
	}
	if applied == 0 {
		t.Error("startup recorded no applied migrations")
	}

	var hasTimescale bool
	err = pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')").Scan(&hasTimescale)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTimescale {
		t.Error("TimescaleDB was not installed by startup migrations")
	}
}

// A pending migration with migrate_on_start disabled must stop the process
// rather than let it run against a schema it does not expect.
func TestStartupRefusesToRunWithPendingMigrations(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}

	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}

	// A database with no ledger has every migration pending.
	name := "atlas_pending_test"
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	cfg := config.Default()
	cfg.Server.Port = 0
	cfg.Database.Host = parsed.ConnConfig.Host
	cfg.Database.Port = int(parsed.ConnConfig.Port)
	cfg.Database.Name = name
	cfg.Database.User = parsed.ConnConfig.User
	cfg.Database.Password = parsed.ConnConfig.Password
	cfg.Database.SSLMode = "disable"
	cfg.Database.MigrateOnStart = false

	instance, err := app.New(&cfg, log.Discard())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runErr := instance.Run(ctx)
	if runErr == nil {
		t.Fatal("Atlas started with pending migrations and migrate_on_start disabled")
	}
	if !strings.Contains(runErr.Error(), "pending") {
		t.Errorf("error = %v, want it to name the pending migrations", runErr)
	}
}
