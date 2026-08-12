//go:build integration

package slo_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corealert "github.com/hexane/atlas/internal/core/alert"
	coreslo "github.com/hexane/atlas/internal/core/slo"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/slo"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newRepository(t *testing.T) *slo.Repository {
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

	name := fmt.Sprintf("atlas_slo_test_%d", time.Now().UnixNano())
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

	return slo.NewRepository(pool.DB())
}

func testDefinition() coreslo.Definition {
	return coreslo.Definition{
		Name: "cpu-under-80", NodeID: "node-1", Signal: "saturation",
		Metric: "system.cpu.usage", Comparison: corealert.ComparisonLT, Threshold: 80,
		TargetPercentage: 99, Window: time.Hour, WarningBudgetPercent: 60,
	}
}

func TestCreateAndGetSLORoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateSLO(ctx, testDefinition())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected an assigned id")
	}

	got, err := repo.GetSLO(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "cpu-under-80" || got.NodeID != "node-1" || got.Metric != "system.cpu.usage" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Comparison != corealert.ComparisonLT || got.Threshold != 80 {
		t.Fatalf("comparison/threshold mismatch: %+v", got)
	}
	if got.Window != time.Hour {
		t.Fatalf("window = %v, want 1h", got.Window)
	}
	if got.WarningBudgetPercent != 60 {
		t.Fatalf("warning budget = %v, want 60", got.WarningBudgetPercent)
	}
}

func TestGetSLONotFound(t *testing.T) {
	repo := newRepository(t)
	_, err := repo.GetSLO(context.Background(), "does-not-exist")
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("code = %q, want not_found", errs.CodeOf(err))
	}
}

func TestListSLOsReturnsNewestFirst(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	first, err := repo.CreateSLO(ctx, testDefinition())
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := testDefinition()
	second.Name = "mem-under-90"
	if _, err := repo.CreateSLO(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	list, err := repo.ListSLOs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) < 2 {
		t.Fatalf("expected at least 2 SLOs, got %d", len(list))
	}
	var foundFirst bool
	for _, d := range list {
		if d.ID == first.ID {
			foundFirst = true
		}
	}
	if !foundFirst {
		t.Fatal("expected the first-created SLO to appear in the list")
	}
}

func TestUpdateSLOChangesFieldsButPreservesCreatedAt(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateSLO(ctx, testDefinition())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// CreateSLO returns the in-memory time.Now() value at ns/monotonic
	// precision; PostgreSQL's timestamptz column only round-trips at
	// microsecond precision, which is what UpdateSLO reads back. Truncate
	// here so the comparison below is against the precision actually
	// persisted, not against a value that never round-tripped.
	created.CreatedAt = created.CreatedAt.Truncate(time.Microsecond)

	updated := created
	updated.Threshold = 90
	updated.TargetPercentage = 95
	result, err := repo.UpdateSLO(ctx, updated)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if result.Threshold != 90 || result.TargetPercentage != 95 {
		t.Fatalf("update did not apply: %+v", result)
	}
	if !result.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at changed: got %v, want %v", result.CreatedAt, created.CreatedAt)
	}
}

func TestUpdateSLONotFound(t *testing.T) {
	repo := newRepository(t)
	def := testDefinition()
	def.ID = "does-not-exist"
	_, err := repo.UpdateSLO(context.Background(), def)
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("code = %q, want not_found", errs.CodeOf(err))
	}
}

func TestDeleteSLORemovesIt(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateSLO(ctx, testDefinition())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.DeleteSLO(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = repo.GetSLO(ctx, created.ID)
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("code after delete = %q, want not_found", errs.CodeOf(err))
	}
}

func TestDeleteSLONotFound(t *testing.T) {
	repo := newRepository(t)
	err := repo.DeleteSLO(context.Background(), "does-not-exist")
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("code = %q, want not_found", errs.CodeOf(err))
	}
}
