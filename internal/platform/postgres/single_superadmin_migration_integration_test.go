//go:build integration

package postgres

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

const singleSuperadminVersion = 22

// migrationsBefore returns the embedded migrations older than version, so a
// test can seed data on the schema a real upgrade starts from.
func migrationsBefore(t *testing.T, version int64) fs.FS {
	t.Helper()
	all, err := NewMigrator(nil, embeddedMigrations(t), nil).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out := fstest.MapFS{}
	for _, m := range all {
		if m.Version < version {
			out[m.Filename()] = &fstest.MapFile{Data: []byte(m.SQL)}
		}
	}
	return out
}

// 0022_single_superadmin.sql against databases holding 0, 1, and 2 active
// superadmin grants (plus the revoked, disabled, and node-scoped variants):
// it applies when at most one fleet-wide grant is active, and otherwise
// fails with its own message, leaving no index and no ledger row behind.
func TestSingleSuperadminMigrationPreconditions(t *testing.T) {
	type grant struct {
		user, node string
		revoked    bool
	}
	cases := []struct {
		name     string
		disabled []string
		grants   []grant
		wantErr  string // "" means the migration applies
	}{
		{name: "no superadmin"},
		{name: "one superadmin", grants: []grant{{user: "u1"}}},
		{name: "one active, one revoked", grants: []grant{{user: "u1"}, {user: "u2", revoked: true}}},
		{name: "one superadmin, disabled", disabled: []string{"u1"}, grants: []grant{{user: "u1"}}},
		{name: "two superadmins", grants: []grant{{user: "u1"}, {user: "u2"}}, wantErr: "found 2 active superadmin grants"},
		{name: "two superadmins, one disabled", disabled: []string{"u2"}, grants: []grant{{user: "u1"}, {user: "u2"}},
			wantErr: "found 2 active superadmin grants"},
		{name: "node-scoped superadmin", grants: []grant{{user: "u1", node: "node-1"}}, wantErr: "node-scoped superadmin grant"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := freshDatabase(t)
			ctx := context.Background()
			if _, err := NewMigrator(pool, migrationsBefore(t, singleSuperadminVersion), nil).Apply(ctx); err != nil {
				t.Fatalf("apply migrations before %04d: %v", singleSuperadminVersion, err)
			}
			for _, u := range []string{"u1", "u2"} {
				if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, password_hash) VALUES ($1, $1, 'x')`, u); err != nil {
					t.Fatalf("seed user: %v", err)
				}
			}
			for _, u := range c.disabled {
				if _, err := pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, u); err != nil {
					t.Fatalf("disable: %v", err)
				}
			}
			for i, g := range c.grants {
				var node, revoked any
				if g.node != "" {
					node = g.node
				}
				if g.revoked {
					revoked = "2026-01-01T00:00:00Z"
				}
				if _, err := pool.Exec(ctx, `
					INSERT INTO user_node_roles (id, user_id, node_id, role_name, revoked_at)
					VALUES ($1, $2, $3, 'superadmin', $4::timestamptz)`,
					"g"+string(rune('0'+i)), g.user, node, revoked); err != nil {
					t.Fatalf("seed grant: %v", err)
				}
			}

			m := NewMigrator(pool, embeddedMigrations(t), nil)
			_, err := m.Apply(ctx)

			var indexExists, recorded bool
			if qerr := pool.QueryRow(ctx, `SELECT to_regclass('idx_user_node_roles_one_superadmin') IS NOT NULL`).Scan(&indexExists); qerr != nil {
				t.Fatal(qerr)
			}
			if qerr := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM atlas_schema_migrations WHERE version = $1)`,
				singleSuperadminVersion).Scan(&recorded); qerr != nil {
				t.Fatal(qerr)
			}

			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Apply() = %v, want success", err)
				}
				if !indexExists || !recorded {
					t.Errorf("index exists = %v, recorded = %v; want both true", indexExists, recorded)
				}
				return
			}
			if err == nil || !strings.Contains(errorChain(err), c.wantErr) {
				t.Fatalf("Apply() = %v, want an error containing %q", err, c.wantErr)
			}
			if indexExists || recorded {
				t.Errorf("index exists = %v, recorded = %v after a refused migration; want both false", indexExists, recorded)
			}
		})
	}
}

// errorChain flattens an error and everything it wraps, so the Postgres
// RAISE message is visible past the migrator's own wrapper text.
func errorChain(err error) string {
	var b strings.Builder
	for e := err; e != nil; {
		b.WriteString(e.Error())
		b.WriteString(" | ")
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return b.String()
}
