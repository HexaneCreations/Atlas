//go:build integration

package inventory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/inventory"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newRepository(t *testing.T) *inventory.Repository {
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

	name := fmt.Sprintf("atlas_inventory_test_%d", time.Now().UnixNano())
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

	return inventory.NewRepository(pool.DB())
}

// jsonEqual compares JSON documents by value, not by byte layout. Postgres's
// jsonb type reformats whitespace on storage (and may reorder object keys),
// so a round-tripped payload is never byte-identical to what was written even
// though it decodes to the same value.
func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not valid JSON: %v", err)
	}
	gj, _ := json.Marshal(g)
	wj, _ := json.Marshal(w)
	return string(gj) == string(wj)
}

func TestGetReturnsNotFoundBeforeAnyPush(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	_, err := repo.Get(ctx, "node-a", coreinventory.SubjectContainers)
	if err == nil {
		t.Fatal("Get succeeded for a subject never pushed")
	}
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("code = %q, want not_found", errs.CodeOf(err))
	}
}

func TestPutThenGetRoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	snap := coreinventory.StoredSnapshot{
		NodeID: "node-b", Subject: coreinventory.SubjectContainers,
		ObservedAt: now, ContentHash: "abc123",
		Data: []byte(`[{"name":"web","state":"running"}]`),
	}
	if err := repo.Put(ctx, snap); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := repo.Get(ctx, "node-b", coreinventory.SubjectContainers)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.ObservedAt.Equal(now) {
		t.Errorf("ObservedAt = %v, want %v", got.ObservedAt, now)
	}
	if got.ContentHash != "abc123" {
		t.Errorf("ContentHash = %q, want abc123", got.ContentHash)
	}
	if !jsonEqual(t, got.Data, snap.Data) {
		t.Errorf("Data = %s, want %s", got.Data, snap.Data)
	}
	if got.ReceivedAt.IsZero() {
		t.Error("ReceivedAt was not set")
	}
}

// Latest-only: a second push for the same (node, subject) replaces the
// first rather than accumulating. This is the property that makes inventory
// different from metric_samples — see the package doc.
func TestPutReplacesRatherThanAppends(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first := coreinventory.StoredSnapshot{
		NodeID: "node-c", Subject: coreinventory.SubjectPorts,
		ObservedAt: now, ContentHash: "v1", Data: []byte(`[{"port":80}]`),
	}
	if err := repo.Put(ctx, first); err != nil {
		t.Fatalf("Put (first): %v", err)
	}

	second := coreinventory.StoredSnapshot{
		NodeID: "node-c", Subject: coreinventory.SubjectPorts,
		ObservedAt: now.Add(time.Minute), ContentHash: "v2", Data: []byte(`[{"port":443}]`),
	}
	if err := repo.Put(ctx, second); err != nil {
		t.Fatalf("Put (second): %v", err)
	}

	got, err := repo.Get(ctx, "node-c", coreinventory.SubjectPorts)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ContentHash != "v2" {
		t.Errorf("ContentHash = %q after a second push, want v2 (replaced, not appended)", got.ContentHash)
	}
	if !jsonEqual(t, got.Data, second.Data) {
		t.Errorf("Data = %s, want the second push's payload", got.Data)
	}
}

// Two subjects for the same node are independent rows, not one overwriting
// the other.
func TestPutIsIndependentPerSubject(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.Put(ctx, coreinventory.StoredSnapshot{
		NodeID: "node-d", Subject: coreinventory.SubjectContainers,
		ObservedAt: now, ContentHash: "c1", Data: []byte(`[]`),
	}); err != nil {
		t.Fatalf("Put containers: %v", err)
	}
	if err := repo.Put(ctx, coreinventory.StoredSnapshot{
		NodeID: "node-d", Subject: coreinventory.SubjectServices,
		ObservedAt: now, ContentHash: "s1", Data: []byte(`[]`),
	}); err != nil {
		t.Fatalf("Put services: %v", err)
	}

	if _, err := repo.Get(ctx, "node-d", coreinventory.SubjectContainers); err != nil {
		t.Errorf("Get containers: %v", err)
	}
	if _, err := repo.Get(ctx, "node-d", coreinventory.SubjectServices); err != nil {
		t.Errorf("Get services: %v", err)
	}
}

func TestLastReceivedAtReflectsTheNewestSubject(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	_, found, err := repo.LastReceivedAt(ctx, "node-g")
	if err != nil {
		t.Fatalf("LastReceivedAt (never pushed): %v", err)
	}
	if found {
		t.Fatal("found = true before any push")
	}

	if err := repo.Put(ctx, coreinventory.StoredSnapshot{
		NodeID: "node-g", Subject: coreinventory.SubjectContainers,
		ObservedAt: now, ContentHash: "c1", Data: []byte(`[]`),
	}); err != nil {
		t.Fatalf("Put containers: %v", err)
	}
	first, found, err := repo.LastReceivedAt(ctx, "node-g")
	if err != nil {
		t.Fatalf("LastReceivedAt (after first push): %v", err)
	}
	if !found {
		t.Fatal("found = false after a push")
	}

	// A second push, to a different subject, pushed later — the freshest
	// receipt across every subject, not just the first one pushed.
	if err := repo.Put(ctx, coreinventory.StoredSnapshot{
		NodeID: "node-g", Subject: coreinventory.SubjectPorts,
		ObservedAt: now.Add(time.Minute), ContentHash: "p1", Data: []byte(`[]`),
	}); err != nil {
		t.Fatalf("Put ports: %v", err)
	}
	second, found, err := repo.LastReceivedAt(ctx, "node-g")
	if err != nil {
		t.Fatalf("LastReceivedAt (after second push): %v", err)
	}
	if !found {
		t.Fatal("found = false after a second push")
	}
	if !second.After(first) && !second.Equal(first) {
		t.Errorf("second LastReceivedAt (%v) is before the first (%v)", second, first)
	}

	// A different, untouched node must not be affected.
	_, found, err = repo.LastReceivedAt(ctx, "node-h")
	if err != nil {
		t.Fatalf("LastReceivedAt (unrelated node): %v", err)
	}
	if found {
		t.Error("found = true for an unrelated node")
	}
}

func TestHasReportedReflectsAnySubject(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	reported, err := repo.HasReported(ctx, "node-e")
	if err != nil {
		t.Fatalf("HasReported: %v", err)
	}
	if reported {
		t.Fatal("HasReported = true before any push")
	}

	if err := repo.Put(ctx, coreinventory.StoredSnapshot{
		NodeID: "node-e", Subject: coreinventory.SubjectMounts,
		ObservedAt: time.Now(), ContentHash: "h", Data: []byte(`[]`),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reported, err = repo.HasReported(ctx, "node-e")
	if err != nil {
		t.Fatalf("HasReported: %v", err)
	}
	if !reported {
		t.Error("HasReported = false after a push")
	}

	// A different node, still untouched, must not be affected.
	reported, err = repo.HasReported(ctx, "node-f")
	if err != nil {
		t.Fatalf("HasReported: %v", err)
	}
	if reported {
		t.Error("HasReported = true for an unrelated node")
	}
}
