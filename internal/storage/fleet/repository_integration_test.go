//go:build integration

// These tests exercise fleet storage against a real PostgreSQL/TimescaleDB
// server, behind the `integration` build tag so `go test ./...` stays
// hermetic — see internal/storage/metric/repository_integration_test.go for
// the same convention and its rationale.
//
//	make db-up && make test-integration
package fleet_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	corefleet "github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/fleet"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newRepository(t *testing.T) *fleet.Repository {
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

	name := fmt.Sprintf("atlas_fleet_test_%d", time.Now().UnixNano())
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

	return fleet.NewRepository(pool.DB())
}

func spec(over func(*corefleet.TokenSpec)) corefleet.TokenSpec {
	s := corefleet.TokenSpec{
		Environment: "production", AllowedCIDR: "0.0.0.0/0",
		MaxUses: 1, TTL: time.Hour,
	}
	if over != nil {
		over(&s)
	}
	return s
}

func TestRedeemConsumesOneUse(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	tok, err := corefleet.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := repo.CreateToken(ctx, tok.Hash, spec(func(s *corefleet.TokenSpec) { s.MaxUses = 2 }), now); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	grant, err := repo.Redeem(ctx, tok.Hash, net.ParseIP("10.0.0.5"), now)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if grant.Environment != "production" {
		t.Errorf("Environment = %q, want production", grant.Environment)
	}

	// Second redemption succeeds (max uses 2); third must fail.
	if _, err := repo.Redeem(ctx, tok.Hash, net.ParseIP("10.0.0.5"), now); err != nil {
		t.Fatalf("second Redeem: %v", err)
	}
	if _, err := repo.Redeem(ctx, tok.Hash, net.ParseIP("10.0.0.5"), now); err == nil {
		t.Fatal("Redeem succeeded a third time past max uses")
	}
}

func TestRedeemRefusesExpiredToken(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	tok, _ := corefleet.NewToken()
	if err := repo.CreateToken(ctx, tok.Hash, spec(func(s *corefleet.TokenSpec) { s.TTL = time.Second }), now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if _, err := repo.Redeem(ctx, tok.Hash, nil, now); err == nil {
		t.Fatal("Redeem succeeded past the token's expiry")
	}
}

func TestRedeemRefusesOutsideCIDR(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	tok, _ := corefleet.NewToken()
	if err := repo.CreateToken(ctx, tok.Hash, spec(func(s *corefleet.TokenSpec) { s.AllowedCIDR = "10.0.0.0/8" }), now); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if _, err := repo.Redeem(ctx, tok.Hash, net.ParseIP("192.168.1.1"), now); err == nil {
		t.Fatal("Redeem succeeded from outside the token's allowed CIDR")
	}
	if _, err := repo.Redeem(ctx, tok.Hash, net.ParseIP("10.1.2.3"), now); err != nil {
		t.Fatalf("Redeem should have succeeded from inside the CIDR: %v", err)
	}
}

// Two concurrent redemptions against a single-use token must not both
// succeed — this is the race the design requires Redeem be atomic against.
func TestRedeemIsRaceSafeUnderConcurrency(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	tok, _ := corefleet.NewToken()
	if err := repo.CreateToken(ctx, tok.Hash, spec(nil), now); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	const attempts = 20
	results := make(chan error, attempts)
	for range attempts {
		go func() {
			_, err := repo.Redeem(ctx, tok.Hash, nil, now)
			results <- err
		}()
	}

	successes := 0
	for range attempts {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("successful redemptions = %d, want exactly 1 (max_uses=1)", successes)
	}
}

func TestActiveCredentialRoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if active, err := repo.ActiveCredential(ctx, "node-none", now); err != nil || active != nil {
		t.Fatalf("ActiveCredential for an unknown node = %+v, %v; want nil, nil", active, err)
	}

	cred := corefleet.Credential{
		Fingerprint: "fp-1", NodeID: "node-a",
		IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := repo.RecordIssuance(ctx, cred); err != nil {
		t.Fatalf("RecordIssuance: %v", err)
	}

	active, err := repo.ActiveCredential(ctx, "node-a", now)
	if err != nil {
		t.Fatalf("ActiveCredential: %v", err)
	}
	if active == nil || active.Fingerprint != "fp-1" {
		t.Fatalf("ActiveCredential = %+v, want fp-1", active)
	}
}

// A revoked credential must stop being active immediately, independent of
// its expiry — this is the renewal-supersession path.
func TestRevokeRemovesCredentialFromActiveSet(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	cred := corefleet.Credential{Fingerprint: "fp-2", NodeID: "node-b", IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := repo.RecordIssuance(ctx, cred); err != nil {
		t.Fatalf("RecordIssuance: %v", err)
	}
	if err := repo.Revoke(ctx, "fp-2", "superseded", now); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	active, err := repo.ActiveCredential(ctx, "node-b", now)
	if err != nil {
		t.Fatalf("ActiveCredential: %v", err)
	}
	if active != nil {
		t.Errorf("revoked credential is still reported active: %+v", active)
	}
}

// An expired-but-not-revoked credential must also stop being active — the
// other half of "live" alongside not-revoked.
func TestExpiredCredentialIsNotActive(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	cred := corefleet.Credential{Fingerprint: "fp-3", NodeID: "node-c", IssuedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-24 * time.Hour)}
	if err := repo.RecordIssuance(ctx, cred); err != nil {
		t.Fatalf("RecordIssuance: %v", err)
	}

	active, err := repo.ActiveCredential(ctx, "node-c", now)
	if err != nil {
		t.Fatalf("ActiveCredential: %v", err)
	}
	if active != nil {
		t.Errorf("expired credential is still reported active: %+v", active)
	}
}

func TestIsGrantedFalseForAnUngrantedNode(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	granted, err := repo.IsGranted(ctx, "node-e", corefleet.OperationContainerLogs)
	if err != nil {
		t.Fatalf("IsGranted: %v", err)
	}
	if granted {
		t.Error("IsGranted true for a node that was never granted anything")
	}
}

func TestGrantThenIsGrantedRoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.Grant(ctx, "node-f", corefleet.OperationContainerLogs, "operator", now); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	granted, err := repo.IsGranted(ctx, "node-f", corefleet.OperationContainerLogs)
	if err != nil {
		t.Fatalf("IsGranted: %v", err)
	}
	if !granted {
		t.Error("IsGranted false immediately after Grant")
	}
}

func TestRevokeGrantRemovesAuthorization(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.Grant(ctx, "node-g", corefleet.OperationContainerLogs, "operator", now); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := repo.RevokeGrant(ctx, "node-g", corefleet.OperationContainerLogs, "compromised", now); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	granted, err := repo.IsGranted(ctx, "node-g", corefleet.OperationContainerLogs)
	if err != nil {
		t.Fatalf("IsGranted: %v", err)
	}
	if granted {
		t.Error("IsGranted true after RevokeGrant")
	}
}

// The invariant the design depends on: Grant must never resurrect a grant
// already revoked, so a re-enrollment can't silently undo an operator's
// revocation.
func TestGrantDoesNotResurrectARevokedGrant(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.Grant(ctx, "node-h", corefleet.OperationContainerLogs, "operator", now); err != nil {
		t.Fatalf("first Grant: %v", err)
	}
	if err := repo.RevokeGrant(ctx, "node-h", corefleet.OperationContainerLogs, "compromised", now); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if err := repo.Grant(ctx, "node-h", corefleet.OperationContainerLogs, "re-enrollment", now); err != nil {
		t.Fatalf("second Grant: %v", err)
	}

	granted, err := repo.IsGranted(ctx, "node-h", corefleet.OperationContainerLogs)
	if err != nil {
		t.Fatalf("IsGranted: %v", err)
	}
	if granted {
		t.Error("a second Grant call resurrected a revoked grant")
	}
}

func TestDenylistRoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if denied, err := repo.IsDenied(ctx, "node-d"); err != nil || denied {
		t.Fatalf("IsDenied for an unlisted node = %v, %v; want false, nil", denied, err)
	}

	if err := repo.Deny(ctx, "node-d", "compromised", now); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if denied, err := repo.IsDenied(ctx, "node-d"); err != nil || !denied {
		t.Fatalf("IsDenied after Deny = %v, %v; want true, nil", denied, err)
	}

	if err := repo.Allow(ctx, "node-d"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if denied, err := repo.IsDenied(ctx, "node-d"); err != nil || denied {
		t.Fatalf("IsDenied after Allow = %v, %v; want false, nil", denied, err)
	}
}
