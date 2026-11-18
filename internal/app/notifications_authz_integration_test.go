//go:build integration

// The notification endpoints against the real role_permissions seed: only a
// fleet-wide superadmin reaches them. The handler-level matrix with a fake
// authorizer is in internal/api/v1/notifications_authz_test.go; this is the
// test that would catch the migration granting the permission to the wrong
// role, or the page-access axis leaking into it.
package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	storagepageauthz "github.com/hexane/atlas/internal/storage/pageauthz"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNotificationEndpointsAreSuperadminOnlyAgainstTheRealSeed(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// An admin holding the full-access page bundle (0016) as well: page
	// access must not stand in for notifications.manage.
	fullAccessAdmin, fullAccessAdminID := bareUser(t, base, "admin")
	if err := storagepageauthz.NewRepository(pool).AssignRoleAccess(context.Background(),
		corepageauthz.RoleAccessAssignmentSpec{
			UserID: fullAccessAdminID, RoleAccessName: "full-access", FleetWide: true, GrantedBy: "integration-test",
		}, time.Now()); err != nil {
		t.Fatalf("AssignRoleAccess(full-access): %v", err)
	}

	callers := []struct {
		name   string
		client *http.Client
		allow  bool
	}{
		{"fleet-wide superadmin", authenticatedTestClient(t, base, "superadmin"), true},
		{"admin", authenticatedTestClient(t, base, "admin"), false},
		{"admin with full-access bundle", fullAccessAdmin, false},
		{"operator", authenticatedTestClient(t, base, "operator"), false},
		{"viewer", authenticatedTestClient(t, base, "viewer"), false},
	}

	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			want := http.StatusForbidden
			if c.allow {
				want = http.StatusOK
			}
			for _, path := range []string{"/api/v1/notifications/channels", "/api/v1/notifications/deliveries"} {
				resp, err := c.client.Get(base + path)
				if err != nil {
					t.Fatalf("GET %s: %v", path, err)
				}
				resp.Body.Close()
				if resp.StatusCode != want {
					t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, want)
				}
			}

			if !c.allow {
				resp, err := c.client.Post(base+"/api/v1/notifications/channels", "application/json",
					strings.NewReader(`{"name":"it-refused","type":"teams"}`))
				if err != nil {
					t.Fatalf("POST channel: %v", err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("POST channel status = %d, want 403", resp.StatusCode)
				}
			}

			resp, err := c.client.Get(base + "/api/v1/auth/me")
			if err != nil {
				t.Fatalf("GET /auth/me: %v", err)
			}
			defer resp.Body.Close()
			var me struct {
				CanManageNotifications bool `json:"can_manage_notifications"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
				t.Fatalf("decode /auth/me: %v", err)
			}
			if me.CanManageNotifications != c.allow {
				t.Errorf("can_manage_notifications = %v, want %v", me.CanManageNotifications, c.allow)
			}
		})
	}
}

// Migration 0021 is idempotent, and superadmin is the only role that holds
// notifications.manage.
func TestNotificationsManageMigrationIsIdempotentAndSuperadminOnly(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	bootServer(t) // applies migrations
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	sql, err := migrations.FS.ReadFile("0021_notifications_manage_permission.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for i := range 2 {
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("re-apply %d: %v", i+1, err)
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT role_name FROM role_permissions WHERE permission_key = 'notifications.manage' ORDER BY role_name`)
	if err != nil {
		t.Fatalf("query role_permissions: %v", err)
	}
	var roles []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		roles = append(roles, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !slices.Equal(roles, []string{"superadmin"}) {
		t.Errorf("roles holding notifications.manage = %v, want [superadmin]", roles)
	}
}
