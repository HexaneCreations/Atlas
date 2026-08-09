//go:build integration

// These tests exercise the incident repository against a real
// PostgreSQL/TimescaleDB server, behind the `integration` build tag so
// `go test ./...` stays hermetic.
//
//	make db-up && make test-integration
package incident_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	coreincident "github.com/hexane/atlas/internal/core/incident"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/incident"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newRepositoryWithPool(t *testing.T) (*incident.Repository, *pgxpool.Pool) {
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

	name := fmt.Sprintf("atlas_incident_test_%d", time.Now().UnixNano())
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

	return incident.NewRepository(pool.DB()), pool.DB()
}

func newRepository(t *testing.T) *incident.Repository {
	t.Helper()
	repo, _ := newRepositoryWithPool(t)
	return repo
}

// insertNode inserts a minimal nodes row so environment-tier queries, which
// join incident_members to nodes, have something to join against.
func insertNode(t *testing.T, pool *pgxpool.Pool, nodeID, environment string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO nodes (node_id, hostname, environment) VALUES ($1, $1, $2)`,
		nodeID, environment)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func openIncident(now time.Time) coreincident.Incident {
	return coreincident.Incident{
		ID: "inc-" + fmt.Sprint(now.UnixNano()), Title: "Container OOM", Status: coreincident.StatusOpen,
		Severity: coreincident.SeverityCritical, RootCauseKind: coreincident.MemberEvent, RootCauseRefID: "evt-1",
		RootCauseTopic: "docker.container.oom", OpenedAt: now, UpdatedAt: now,
	}
}

func TestCreateAndGetIncidentRoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	inc := openIncident(now)
	created, err := repo.CreateIncident(ctx, inc)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetIncident(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != inc.Title || got.Status != coreincident.StatusOpen || got.RootCauseRefID != "evt-1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestAddMemberIsIdempotentOnKindAndRefID(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	inc, err := repo.CreateIncident(ctx, openIncident(now))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	member := coreincident.Member{
		ID: "mem-1", IncidentID: inc.ID, Kind: coreincident.MemberEvent, RefID: "evt-1",
		NodeID: "node-1", Topic: "docker.container.oom", Severity: coreincident.SeverityCritical,
		Time: now, IsRootCause: true,
	}
	if err := repo.AddMember(ctx, member); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := repo.AddMember(ctx, member); err != nil {
		t.Fatalf("second add: %v", err)
	}

	detail, err := repo.GetDetail(ctx, inc.ID)
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	if len(detail.Members) != 1 {
		t.Fatalf("expected the duplicate member to be deduplicated, got %d", len(detail.Members))
	}
}

func TestFindCorrelatableRespectsNodeAndWindow(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	inc, err := repo.CreateIncident(ctx, openIncident(now))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = repo.AddMember(ctx, coreincident.Member{
		ID: "mem-1", IncidentID: inc.ID, Kind: coreincident.MemberEvent, RefID: "evt-1",
		NodeID: "node-1", Topic: "docker.container.oom", Severity: coreincident.SeverityCritical, Time: now,
	})
	if err != nil {
		t.Fatalf("add member: %v", err)
	}

	found, ok, err := repo.FindCorrelatable(ctx, "node-1", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("find (matching node/window): %v", err)
	}
	if !ok || found.ID != inc.ID {
		t.Fatalf("expected to find the incident, got found=%v id=%s", ok, found.ID)
	}

	_, ok, err = repo.FindCorrelatable(ctx, "node-2", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("find (different node): %v", err)
	}
	if ok {
		t.Fatal("expected no match for a different node")
	}

	_, ok, err = repo.FindCorrelatable(ctx, "node-1", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("find (window too tight): %v", err)
	}
	if ok {
		t.Fatal("expected no match once the window excludes the member")
	}
}

func TestFindCorrelatableByEnvironmentRespectsEnvironmentAndWindow(t *testing.T) {
	repo, pool := newRepositoryWithPool(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	insertNode(t, pool, "node-1", "production")
	insertNode(t, pool, "node-2", "production")
	insertNode(t, pool, "node-3", "staging")

	inc, err := repo.CreateIncident(ctx, openIncident(now))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = repo.AddMember(ctx, coreincident.Member{
		ID: "mem-1", IncidentID: inc.ID, Kind: coreincident.MemberEvent, RefID: "evt-1",
		NodeID: "node-1", Topic: "docker.container.oom", Severity: coreincident.SeverityCritical, Time: now,
	})
	if err != nil {
		t.Fatalf("add member: %v", err)
	}

	found, ok, err := repo.FindCorrelatableByEnvironment(ctx, "production", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("find (matching environment/window, different node): %v", err)
	}
	if !ok || found.ID != inc.ID {
		t.Fatalf("expected to find the incident via node-2's shared environment, got found=%v id=%s", ok, found.ID)
	}

	_, ok, err = repo.FindCorrelatableByEnvironment(ctx, "staging", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("find (different environment): %v", err)
	}
	if ok {
		t.Fatal("expected no match for a different environment")
	}

	_, ok, err = repo.FindCorrelatableByEnvironment(ctx, "production", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("find (window too tight): %v", err)
	}
	if ok {
		t.Fatal("expected no match once the window excludes the member")
	}
}

func TestFindCorrelatableIgnoresResolvedIncidents(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	inc, err := repo.CreateIncident(ctx, openIncident(now))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = repo.AddMember(ctx, coreincident.Member{
		ID: "mem-1", IncidentID: inc.ID, Kind: coreincident.MemberEvent, RefID: "evt-1",
		NodeID: "node-1", Topic: "docker.container.oom", Severity: coreincident.SeverityCritical, Time: now,
	})
	if err != nil {
		t.Fatalf("add member: %v", err)
	}

	inc.Status, inc.ResolvedAt = coreincident.StatusResolved, now
	if err := repo.UpdateIncident(ctx, inc); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	_, ok, err := repo.FindCorrelatable(ctx, "node-1", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if ok {
		t.Fatal("expected a resolved incident not to be correlatable")
	}
}

func TestListIncidentsFiltersByStatusAndNode(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	open, err := repo.CreateIncident(ctx, openIncident(now))
	if err != nil {
		t.Fatalf("create open: %v", err)
	}
	if err := repo.AddMember(ctx, coreincident.Member{
		ID: "mem-open", IncidentID: open.ID, Kind: coreincident.MemberEvent, RefID: "evt-open",
		NodeID: "node-1", Topic: "docker.container.oom", Severity: coreincident.SeverityCritical, Time: now,
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	resolved := openIncident(now.Add(time.Second))
	resolved.Status, resolved.ResolvedAt = coreincident.StatusResolved, now.Add(time.Minute)
	if _, err := repo.CreateIncident(ctx, resolved); err != nil {
		t.Fatalf("create resolved: %v", err)
	}

	openList, err := repo.ListIncidents(ctx, coreincident.Filter{Status: coreincident.StatusOpen})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(openList) != 1 || openList[0].ID != open.ID {
		t.Fatalf("expected exactly the open incident, got %+v", openList)
	}

	byNode, err := repo.ListIncidents(ctx, coreincident.Filter{NodeID: "node-1"})
	if err != nil {
		t.Fatalf("list by node: %v", err)
	}
	if len(byNode) != 1 || byNode[0].ID != open.ID {
		t.Fatalf("expected exactly the incident touching node-1, got %+v", byNode)
	}
}

func TestGetDetailComputesAffectedNodesAndSubjects(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	inc, err := repo.CreateIncident(ctx, openIncident(now))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = repo.AddMember(ctx, coreincident.Member{
		ID: "mem-1", IncidentID: inc.ID, Kind: coreincident.MemberEvent, RefID: "evt-1",
		NodeID: "node-1", Topic: "docker.container.oom", Severity: coreincident.SeverityCritical, Time: now, IsRootCause: true,
	})
	if err != nil {
		t.Fatalf("add member 1: %v", err)
	}
	err = repo.AddMember(ctx, coreincident.Member{
		ID: "mem-2", IncidentID: inc.ID, Kind: coreincident.MemberEvent, RefID: "evt-2",
		NodeID: "node-2", Topic: "docker.container.died", Severity: coreincident.SeverityWarning, Time: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("add member 2: %v", err)
	}

	detail, err := repo.GetDetail(ctx, inc.ID)
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	if len(detail.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(detail.Members))
	}
	if len(detail.AffectedNodes) != 2 {
		t.Fatalf("expected 2 affected nodes, got %+v", detail.AffectedNodes)
	}
	if !detail.Members[0].IsRootCause {
		t.Fatalf("expected the first member (root cause) to be flagged, got %+v", detail.Members[0])
	}
}

func TestUpdateIncidentNotFound(t *testing.T) {
	repo := newRepository(t)
	err := repo.UpdateIncident(context.Background(), coreincident.Incident{
		ID: "does-not-exist", Status: coreincident.StatusResolved, Severity: coreincident.SeverityWarning,
		UpdatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected an error updating an incident that does not exist")
	}
}
