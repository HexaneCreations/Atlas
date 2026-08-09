//go:build integration

// These tests exercise the pool and migrator against a real
// PostgreSQL/TimescaleDB server. They are behind the `integration` build tag
// so that `go test ./...` stays hermetic and fast; CI runs them as a separate
// job with a database service.
//
//	make db-up && make test-integration
//
// Set ATLAS_TEST_DATABASE_URL to point at a server where the test user may
// create and drop databases. Each test gets its own database, so tests remain
// independent and can run in parallel.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

// adminDSN returns the server URL, skipping the test if none is configured.
func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	return dsn
}

// freshDatabase creates an empty database and returns a pool connected to it.
// The database is dropped when the test finishes.
func freshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminDSN(t))
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	defer admin.Close()

	// Test names are not valid identifiers; derive a safe unique name.
	name := fmt.Sprintf("atlas_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	pool, err := pgxpool.New(ctx, replaceDatabase(adminDSN(t), name))
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), adminDSN(t))
		if err != nil {
			t.Logf("could not reconnect to drop %s: %v", name, err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("could not drop %s: %v", name, err)
		}
	})
	return pool
}

func replaceDatabase(dsn, name string) string {
	base, query, hasQuery := strings.Cut(dsn, "?")
	slash := strings.LastIndex(base, "/")
	out := base[:slash+1] + name
	if hasQuery {
		out += "?" + query
	}
	return out
}

func TestApplyCreatesSchemaAndIsIdempotent(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	m := NewMigrator(pool, embeddedMigrations(t), nil)

	first, err := m.Apply(ctx)
	if err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	if len(first.Applied) == 0 {
		t.Fatal("first Apply() applied nothing")
	}

	// Re-running must be a no-op, which is what makes MigrateOnStart safe on
	// every restart.
	second, err := m.Apply(ctx)
	if err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("second Apply() re-applied %d migrations", len(second.Applied))
	}
	if second.AlreadyCurrent != len(first.Applied) {
		t.Errorf("AlreadyCurrent = %d, want %d", second.AlreadyCurrent, len(first.Applied))
	}
}

func TestMigration0001InstallsRequiredExtensions(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	if _, err := NewMigrator(pool, embeddedMigrations(t), nil).Apply(ctx); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	for _, ext := range []string{"timescaledb", "pgcrypto"} {
		var present bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", ext).Scan(&present)
		if err != nil {
			t.Fatalf("query pg_extension: %v", err)
		}
		if !present {
			t.Errorf("extension %q was not installed by migration 0001", ext)
		}
	}
}

func TestStatusAndPendingReflectAppliedState(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	m := NewMigrator(pool, embeddedMigrations(t), nil)
	if _, err := m.Apply(ctx); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	applied, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("Status() returned nothing after a successful Apply")
	}
	if applied[0].Version != 1 || applied[0].Name != "extensions" {
		t.Errorf("first applied = %+v, want version 1 extensions", applied[0])
	}
	if applied[0].AppliedAt.IsZero() {
		t.Error("AppliedAt was not recorded")
	}

	pending, err := m.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("Pending() = %d migrations after Apply, want 0", len(pending))
	}
}

// A rolling deploy starts several instances at once. Exactly one must apply
// each migration; the rest must wait and then find nothing to do.
func TestConcurrentApplyIsSerialisedByAdvisoryLock(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	const instances = 5
	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		totalApplied int
		failures     []error
	)

	for range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := NewMigrator(pool, embeddedMigrations(t), nil).Apply(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			totalApplied += len(res.Applied)
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("concurrent Apply produced errors: %v", errors.Join(failures...))
	}

	expected, err := NewMigrator(pool, embeddedMigrations(t), nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	if totalApplied != len(expected) {
		t.Errorf("migrations applied %d times in total, want exactly %d", totalApplied, len(expected))
	}
}

func TestApplyRejectsEditedMigration(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	original := fstest.MapFS{
		"0001_extensions.sql": &fstest.MapFile{Data: []byte("CREATE EXTENSION IF NOT EXISTS pgcrypto;")},
	}
	if _, err := NewMigrator(pool, original, nil).Apply(ctx); err != nil {
		t.Fatalf("initial Apply() error = %v", err)
	}

	edited := fstest.MapFS{
		"0001_extensions.sql": &fstest.MapFile{Data: []byte("CREATE EXTENSION IF NOT EXISTS timescaledb;")},
	}
	_, err := NewMigrator(pool, edited, nil).Apply(ctx)
	if err == nil {
		t.Fatal("Apply() accepted a migration whose content changed after it was applied")
	}
	if got := errs.CodeOf(err); got != errs.CodeFailedPrecondition {
		t.Errorf("code = %q, want failed_precondition", got)
	}
}

// A failing migration must leave no partial state and no ledger entry, so a
// retry after the fix starts cleanly.
func TestFailedMigrationRollsBackAndIsNotRecorded(t *testing.T) {
	pool := freshDatabase(t)
	ctx := context.Background()

	broken := fstest.MapFS{
		"0001_good.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE first_table (id int PRIMARY KEY);")},
		"0002_broken.sql": &fstest.MapFile{Data: []byte("CREATE TABLE second_table (id int PRIMARY KEY); SELECT this_function_does_not_exist();")},
	}

	m := NewMigrator(pool, broken, nil)
	res, err := m.Apply(ctx)
	if err == nil {
		t.Fatal("Apply() succeeded despite a broken migration")
	}
	if len(res.Applied) != 1 {
		t.Errorf("Applied = %d, want 1 (the good migration before the failure)", len(res.Applied))
	}

	// The good migration is committed and recorded.
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('first_table') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("the migration that succeeded was rolled back")
	}

	// The broken one left nothing behind.
	if err := pool.QueryRow(ctx, "SELECT to_regclass('second_table') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("a failed migration left a partially created table behind")
	}

	applied, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Errorf("ledger = %+v, want only version 1", applied)
	}
}

func TestPoolLifecycleAgainstRealServer(t *testing.T) {
	pool := freshDatabase(t)
	pool.Close() // freshDatabase only needed to create the database

	cfg := config.Default().Database
	cfg.SSLMode = "disable"
	if dsn := os.Getenv(testDatabaseURLEnv); dsn != "" {
		applyDSNToConfig(t, &cfg, dsn)
	}

	p := NewPool(cfg, nil)
	ctx := context.Background()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := p.Ping(ctx); err != nil {
		t.Errorf("Ping() error = %v", err)
	}
	version, err := p.Version(ctx)
	if err != nil {
		t.Errorf("Version() error = %v", err)
	}
	if !strings.Contains(version, "PostgreSQL") {
		t.Errorf("Version() = %q, want it to name PostgreSQL", version)
	}
	if stats := p.Stats(); stats.MaxConns != cfg.MaxConns {
		t.Errorf("Stats().MaxConns = %d, want %d", stats.MaxConns, cfg.MaxConns)
	}
	if strings.Contains(p.String(), cfg.Password) && cfg.Password != "" {
		t.Error("String() leaked the password")
	}

	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
	if err := p.Ping(ctx); err == nil {
		t.Error("Ping() succeeded after Stop")
	}
}

func TestPoolStartFailsFastWhenUnreachable(t *testing.T) {
	cfg := config.Default().Database
	cfg.Host = "127.0.0.1"
	cfg.Port = 1 // nothing listens here
	cfg.SSLMode = "disable"
	cfg.ConnectTimeout = 2 * time.Second

	err := NewPool(cfg, nil).Start(context.Background())
	if err == nil {
		t.Fatal("Start() succeeded against an unreachable server")
	}
	if got := errs.CodeOf(err); got != errs.CodeUnavailable {
		t.Errorf("code = %q, want unavailable", got)
	}
}

// applyDSNToConfig fills a config.Database from a libpq URL so the pool test
// targets the same server as the rest of the suite.
func applyDSNToConfig(t *testing.T, cfg *config.Database, dsn string) {
	t.Helper()
	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}
	cfg.Host = parsed.ConnConfig.Host
	cfg.Port = int(parsed.ConnConfig.Port)
	cfg.Name = parsed.ConnConfig.Database
	cfg.User = parsed.ConnConfig.User
	cfg.Password = parsed.ConnConfig.Password
}
