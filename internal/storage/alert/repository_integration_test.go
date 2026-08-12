//go:build integration

// These tests exercise the alert repository against a real
// PostgreSQL/TimescaleDB server, behind the `integration` build tag so
// `go test ./...` stays hermetic.
//
//	make db-up && make test-integration
package alert_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corealert "github.com/hexane/atlas/internal/core/alert"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/alert"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newRepository(t *testing.T) *alert.Repository {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}

	admin, err := pgxpool.NewWithConfig(context.Background(), parsed)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("atlas_alert_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.NewWithConfig(context.Background(), parsed)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	cfg := config.Default()
	cfg.Database.Host = parsed.ConnConfig.Host
	cfg.Database.Port = int(parsed.ConnConfig.Port)
	cfg.Database.Name = name
	cfg.Database.User = parsed.ConnConfig.User
	cfg.Database.Password = parsed.ConnConfig.Password
	cfg.Database.SSLMode = "disable"
	cfg.Database.MigrateOnStart = true

	pool := postgres.NewPool(cfg.Database, log.Discard())
	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool start: %v", err)
	}
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	migrator := postgres.NewMigrator(pool.DB(), migrations.FS, log.Discard())
	if _, err := migrator.Apply(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	return alert.NewRepository(pool.DB())
}

func thresholdRule() corealert.Rule {
	return corealert.Rule{
		Name: "High CPU", Enabled: true, Kind: corealert.KindThreshold, Severity: corealert.SeverityWarning,
		Metric: "system.cpu.usage", Comparison: corealert.ComparisonGT, Threshold: 90, For: 5 * time.Minute,
	}
}

func TestCreateRuleAssignsIDAndPersists(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateRule(ctx, thresholdRule())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected an id to be assigned")
	}

	got, err := repo.GetRule(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "High CPU" || got.Metric != "system.cpu.usage" || got.Comparison != corealert.ComparisonGT ||
		got.Threshold != 90 || got.For != 5*time.Minute || !got.Enabled {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestGetRuleNotFound(t *testing.T) {
	repo := newRepository(t)
	if _, err := repo.GetRule(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("expected an error for an unknown rule id")
	}
}

func TestUpdateRuleChangesFields(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateRule(ctx, thresholdRule())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// CreateRule returns the in-memory time.Now() value at ns/monotonic
	// precision; PostgreSQL's timestamptz column only round-trips at
	// microsecond precision, which is what UpdateRule's RETURNING reads
	// back. Truncate here so the comparison below is against the precision
	// actually persisted, not against a value that never round-tripped.
	created.CreatedAt = created.CreatedAt.Truncate(time.Microsecond)

	created.Threshold = 95
	created.Enabled = false
	updated, err := repo.UpdateRule(ctx, created)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Threshold != 95 || updated.Enabled {
		t.Fatalf("update did not apply: %+v", updated)
	}
	if updated.CreatedAt.IsZero() || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("update must preserve created_at, got %v want %v", updated.CreatedAt, created.CreatedAt)
	}

	got, err := repo.GetRule(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Threshold != 95 || got.Enabled {
		t.Fatalf("update did not persist: %+v", got)
	}
}

func TestDeleteRuleCascadesState(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateRule(ctx, thresholdRule())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = repo.SaveState(ctx, corealert.AlertState{RuleID: created.ID, NodeID: "node-1", State: corealert.StateFiring, UpdatedAt: time.Now()})
	if err != nil {
		t.Fatalf("save state: %v", err)
	}

	if err := repo.DeleteRule(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.DeleteRule(ctx, created.ID); err == nil {
		t.Fatal("expected deleting an already-deleted rule to fail")
	}

	if _, found, err := repo.GetState(ctx, created.ID, "node-1", ""); err != nil || found {
		t.Fatalf("expected state to cascade-delete with its rule, found=%v err=%v", found, err)
	}
}

func TestSaveStateUpsertsAndListActiveStatesFiltersOK(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateRule(ctx, thresholdRule())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	firing := corealert.AlertState{RuleID: created.ID, NodeID: "node-1", State: corealert.StateFiring, Value: 95, UpdatedAt: time.Now()}
	if err := repo.SaveState(ctx, firing); err != nil {
		t.Fatalf("save firing: %v", err)
	}
	ok := corealert.AlertState{RuleID: created.ID, NodeID: "node-2", State: corealert.StateOK, Value: 10, UpdatedAt: time.Now()}
	if err := repo.SaveState(ctx, ok); err != nil {
		t.Fatalf("save ok: %v", err)
	}

	active, err := repo.ListActiveStates(ctx)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 1 || active[0].NodeID != "node-1" {
		t.Fatalf("expected only the firing state, got %+v", active)
	}

	// Upsert: same key, new state.
	firing.State = corealert.StateOK
	firing.UpdatedAt = time.Now()
	if err := repo.SaveState(ctx, firing); err != nil {
		t.Fatalf("save resolved: %v", err)
	}
	active, err = repo.ListActiveStates(ctx)
	if err != nil {
		t.Fatalf("list active after resolve: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no active states after resolving, got %+v", active)
	}
}

func TestAppendHistoryIsIdempotentAndQueryFilters(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	entry := corealert.HistoryEntry{ID: "hist-1", Time: now, RuleID: "rule-1", NodeID: "node-1", State: corealert.StateFiring, Value: 95}
	if err := repo.AppendHistory(ctx, entry); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := repo.AppendHistory(ctx, entry); err != nil {
		t.Fatalf("second append: %v", err)
	}

	other := corealert.HistoryEntry{ID: "hist-2", Time: now.Add(time.Minute), RuleID: "rule-2", NodeID: "node-2", State: corealert.StateFiring}
	if err := repo.AppendHistory(ctx, other); err != nil {
		t.Fatalf("append other: %v", err)
	}

	got, err := repo.QueryHistory(ctx, corealert.HistoryFilter{RuleID: "rule-1"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the duplicate to be deduplicated and the other rule excluded, got %d", len(got))
	}
}
