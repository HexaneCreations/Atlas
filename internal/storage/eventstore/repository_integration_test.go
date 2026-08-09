//go:build integration

// These tests exercise the event store repository against a real
// PostgreSQL/TimescaleDB server, behind the `integration` build tag so
// `go test ./...` stays hermetic.
//
//	make db-up && make test-integration
package eventstore_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	coreeventstore "github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/eventstore"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newRepository(t *testing.T) *eventstore.Repository {
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

	name := fmt.Sprintf("atlas_eventstore_test_%d", time.Now().UnixNano())
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

	return eventstore.NewRepository(pool.DB())
}

func TestInsertPersistsAndQueryReturnsEvents(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	rec := coreeventstore.Record{
		ID: "evt-1", Time: now, NodeID: "node-1", Topic: "docker.container.started",
		Source: "plugin.docker", Subject: "abc123", Payload: []byte(`{"name":"nginx"}`),
	}
	if err := repo.Insert(ctx, rec); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.Query(ctx, coreeventstore.Filter{NodeID: "node-1"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].ID != "evt-1" || got[0].Topic != "docker.container.started" {
		t.Errorf("unexpected event: %+v", got[0])
	}
}

func TestInsertIsIdempotentOnEventID(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	rec := coreeventstore.Record{ID: "evt-dup", Time: now, NodeID: "node-1", Topic: "system.host.rebooted"}
	if err := repo.Insert(ctx, rec); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := repo.Insert(ctx, rec); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	got, err := repo.Query(ctx, coreeventstore.Filter{NodeID: "node-1"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the duplicate to be deduplicated, got %d rows", len(got))
	}
}

func TestQueryFiltersByTopicAndTime(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)

	for i, topic := range []string{"docker.container.started", "docker.container.stopped", "system.host.rebooted"} {
		err := repo.Insert(ctx, coreeventstore.Record{
			ID: fmt.Sprintf("evt-%d", i), Time: base.Add(time.Duration(i) * time.Minute),
			NodeID: "node-1", Topic: topic,
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	got, err := repo.Query(ctx, coreeventstore.Filter{NodeID: "node-1", Topic: "docker.container.started"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].Topic != "docker.container.started" {
		t.Fatalf("topic filter did not narrow results: %+v", got)
	}

	got, err = repo.Query(ctx, coreeventstore.Filter{NodeID: "node-1", Since: base.Add(90 * time.Second)})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].Topic != "system.host.rebooted" {
		t.Fatalf("since filter did not narrow results: %+v", got)
	}
}

func TestReceiverAttributesEventToAuthenticatedNode(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	recv := eventstore.NewReceiver(repo)

	if recv.Kind() != transport.KindEvents {
		t.Fatalf("unexpected kind: %s", recv.Kind())
	}

	env := transport.Envelope{
		ID:     "env-1",
		Origin: transport.Origin{NodeID: "authenticated-node", Hostname: "h"},
		Payload: transport.Events{Event: eventbus.Event{
			ID: "evt-recv", Topic: "docker.container.oom", Time: time.Now().UTC(),
		}},
	}
	if err := recv.Receive(ctx, env); err != nil {
		t.Fatalf("receive: %v", err)
	}

	got, err := repo.Query(ctx, coreeventstore.Filter{NodeID: "authenticated-node"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "authenticated-node" {
		t.Fatalf("event not attributed to the authenticated node: %+v", got)
	}
}
