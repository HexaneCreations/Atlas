//go:build integration

// The default-deny session check on /api/v1, over real HTTP against the fully
// wired server.
package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/hexane/atlas/internal/api/v1"
	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/id"
	storageinventory "github.com/hexane/atlas/internal/storage/inventory"
)

// publicRoutes freezes every /api/v1 route an anonymous caller can reach.
// Adding one here is a security decision, not a test fix.
var publicRoutes = []string{
	"GET /api/v1/auth/me",
	"GET /api/v1/system/health",
	"POST /api/v1/agent/install/redeem",
	"POST /api/v1/auth/login",
	"POST /api/v1/auth/logout",
}

type routeRecorder struct{ patterns []string }

func (r *routeRecorder) Handle(pattern string, _ http.Handler) {
	r.patterns = append(r.patterns, pattern)
}

func recordedRoutes(t *testing.T) []string {
	t.Helper()
	rec := &routeRecorder{}
	v1.NewHandler(v1.Deps{}).Mount(rec)
	if len(rec.patterns) < 70 {
		t.Fatalf("recorded only %d routes", len(rec.patterns))
	}
	return rec.patterns
}

var routeParam = regexp.MustCompile(`\{[^}]+\}`)

var webSocketHeaders = map[string]string{
	"Connection":            "Upgrade",
	"Upgrade":               "websocket",
	"Sec-WebSocket-Version": "13",
	"Sec-WebSocket-Key":     "dGhlIHNhbXBsZSBub25jZQ==",
}

func sessionRefused(status int, raw string) bool {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(raw), &envelope)
	return status == http.StatusUnauthorized && envelope.Error.Message == "authentication required"
}

// Every registered route, called with no session, answers 401 "authentication
// required" — through the ordinary chain and through the streaming one — and
// the exceptions are exactly publicRoutes. A new route reachable anonymously
// fails here without being listed anywhere.
func TestAnonymousCallersReachOnlyTheAllowlist(t *testing.T) {
	f := bootInstallServer(t)

	for _, chain := range []struct {
		name   string
		header map[string]string
	}{
		{"http", nil},
		{"websocket", webSocketHeaders},
	} {
		t.Run(chain.name, func(t *testing.T) {
			var public []string
			for _, pattern := range recordedRoutes(t) {
				method, path, _ := strings.Cut(pattern, " ")
				body := ""
				if method == http.MethodPost || method == http.MethodPut {
					body = "{}"
				}
				header := map[string]string{"X-Forwarded-For": freshIP() + ", 172.18.0.2"}
				for k, v := range chain.header {
					header[k] = v
				}
				t.Run(pattern, func(t *testing.T) {
					status, raw := f.send(http.DefaultClient, method, routeParam.ReplaceAllString(path, "x"), body, header)
					if sessionRefused(status, raw) {
						return
					}
					public = append(public, pattern)
					if !slices.Contains(publicRoutes, pattern) {
						t.Errorf("anonymous call answered %d %.200s; want 401 authentication required", status, raw)
					}
				})
			}
			slices.Sort(public)
			if !slices.Equal(public, publicRoutes) {
				t.Errorf("routes reachable without a session:\n got %v\nwant %v", public, publicRoutes)
			}
		})
	}
}

// A cookie that does not resolve to a live session is the same as none, and
// so is another spelling of a protected path.
func TestInvalidSessionsAndPathVariantsAreRefused(t *testing.T) {
	f := bootInstallServer(t)

	loggedOut := authenticatedTestClient(t, f.base, coreuser.RoleViewer)
	if status, raw := f.send(loggedOut, http.MethodGet, "/api/v1/events", "", nil); status != http.StatusOK {
		t.Fatalf("before logout: %d %s", status, raw)
	}
	var cookie string
	probe, err := http.NewRequest(http.MethodGet, f.base+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range loggedOut.Jar.Cookies(probe.URL) {
		cookie = c.Name + "=" + c.Value
	}
	if status, raw := f.send(loggedOut, http.MethodPost, "/api/v1/auth/logout", "", nil); status != http.StatusNoContent {
		t.Fatalf("logout: %d %s", status, raw)
	}

	for name, c := range map[string]string{"revoked": cookie, "forged": "atlas_session=forged", "empty": "atlas_session="} {
		status, raw := f.send(http.DefaultClient, http.MethodGet, "/api/v1/events", "", map[string]string{"Cookie": c})
		if !sessionRefused(status, raw) {
			t.Errorf("%s session: %d %.200s, want 401", name, status, raw)
		}
	}

	for _, path := range []string{
		"/api/v1/events/", "/api/v1//events", "/api/v1/./events", "/api/v1/auth/../events",
		"/api/v1/auth/login/../../events", "/api/v1/%65vents", "/API/v1/events", "/api/v1/events?node=x",
	} {
		if status, raw := f.send(http.DefaultClient, http.MethodGet, path, "", nil); status != http.StatusUnauthorized && status != http.StatusNotFound {
			t.Errorf("GET %s: %d %.200s, want 401 or 404", path, status, raw)
		}
	}
	if status, _ := f.send(http.DefaultClient, http.MethodHead, "/api/v1/events", "", nil); status != http.StatusUnauthorized {
		t.Errorf("HEAD /api/v1/events: %d, want 401", status)
	}
}

// A logged-in viewer still gets 200 from everything the UI loads (see
// web/src/api). Host-dependent inventory is read from a pushed snapshot, so
// the result does not depend on the machine running the test.
func TestViewerStillReachesEveryRouteTheUIUses(t *testing.T) {
	f := bootInstallServer(t)
	ctx := context.Background()

	nodeID := "public-routes-it-" + id.New()
	seedNode(t, os.Getenv(testDatabaseURLEnv), nodeID)
	inv := storageinventory.NewRepository(f.pool)
	for subject, data := range map[coreinventory.Subject]string{
		coreinventory.SubjectContainers:   `[]`,
		coreinventory.SubjectProcesses:    `[]`,
		coreinventory.SubjectServices:     `[]`,
		coreinventory.SubjectServiceGraph: `{}`,
		coreinventory.SubjectCronJobs:     `[]`,
		coreinventory.SubjectPorts:        `[]`,
		coreinventory.SubjectMounts:       `[]`,
	} {
		if err := inv.Put(ctx, coreinventory.StoredSnapshot{
			NodeID: nodeID, Subject: subject, Data: json.RawMessage(data), ObservedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed %s: %v", subject, err)
		}
	}

	viewer := authenticatedTestClient(t, f.base, coreuser.RoleViewer)
	q := "?node=" + nodeID
	for _, path := range []string{
		"/auth/me",
		"/system/info", "/system/health", "/system/runtime",
		"/nodes", "/nodes/" + nodeID,
		"/collectors", "/activity?limit=12",
		"/metrics" + q + "&metric=system.cpu.usage&range=1h", "/metrics/latest" + q, "/metrics/names" + q,
		"/containers" + q, "/processes" + q, "/services" + q,
		"/cron" + q, "/ports" + q, "/mounts" + q,
	} {
		name, _, _ := strings.Cut(strings.Replace(path, nodeID, "{nodeID}", 1), "?")
		t.Run(name, func(t *testing.T) {
			if status, raw := f.send(viewer, http.MethodGet, "/api/v1"+path, "", nil); status != http.StatusOK {
				t.Errorf("viewer: %d %.300s, want 200", status, raw)
			}
		})
	}

	// Answered live by this host's own Docker and systemd, never from a
	// snapshot, so whether they can be 200 depends on the machine. What must
	// hold anywhere is that the viewer gets past authentication and
	// authorization.
	for _, path := range []string{"/containers/no-such-container", "/services/graph", "/services/no-such.service"} {
		if status, raw := f.send(viewer, http.MethodGet, "/api/v1"+path, "", nil); status == http.StatusUnauthorized || status == http.StatusForbidden {
			t.Errorf("viewer %s: %d %.300s, want past authentication and authorization", path, status, raw)
		}
	}

	// The live log socket: the session passes the check on the streaming
	// chain too, and the answer is the handler's own.
	if status, raw := f.send(viewer, http.MethodGet, "/api/v1/containers/x/logs/follow", "", webSocketHeaders); status != http.StatusForbidden {
		t.Errorf("viewer logs/follow: %d %.300s, want the handler's 403 (no node.logs.read)", status, raw)
	}
	operator := authenticatedTestClient(t, f.base, coreuser.RoleOperator)
	if status, raw := f.send(operator, http.MethodGet, "/api/v1/containers/no-such-container/logs/follow", "", webSocketHeaders); status == http.StatusUnauthorized || status == http.StatusForbidden {
		t.Errorf("operator logs/follow: %d %.300s, want past authentication and authorization", status, raw)
	}
}
