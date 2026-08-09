package api_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/api"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/postgres"
)

// newTestAPI builds the real router with a controllable health registry.
// The pool is unstarted, which is exactly the state the API must survive:
// every endpoint here is required to answer when the database is down.
func newTestAPI(t *testing.T, checkers ...health.Checker) http.Handler {
	t.Helper()

	cfg := config.Default()
	reg := health.NewRegistry(nil)
	reg.Register(checkers...)

	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	return api.New(api.Deps{
		Config: &cfg,
		Health: reg,
		Pool:   postgres.NewPool(cfg.Database, nil),
		Bus:    bus,
	})
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// Liveness must not depend on anything. Probing the database here is how a
// database blip becomes a cascading restart of every instance.
func TestLivenessIgnoresDependencies(t *testing.T) {
	t.Parallel()

	h := newTestAPI(t, health.Func{
		CheckName:  "database",
		IsCritical: true,
		Probe:      func(context.Context) error { return stderrors.New("database is down") },
	})

	rec := get(t, h, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d with a dead database, want 200", rec.Code)
	}
}

func TestReadinessReflectsCriticalDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		checker health.Checker
		want    int
	}{
		{
			name:    "healthy",
			checker: health.Func{CheckName: "database", IsCritical: true},
			want:    http.StatusOK,
		},
		{
			name: "critical failure drains the instance",
			checker: health.Func{CheckName: "database", IsCritical: true,
				Probe: func(context.Context) error { return stderrors.New("unreachable") }},
			want: http.StatusServiceUnavailable,
		},
		{
			// Degraded still serves: losing the last working instance over
			// reduced visibility is worse than the reduced visibility.
			name: "non-critical failure keeps serving",
			checker: health.Func{CheckName: "docker", IsCritical: false,
				Probe: func(context.Context) error { return stderrors.New("no socket") }},
			want: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := get(t, newTestAPI(t, tt.checker), "/readyz")
			if rec.Code != tt.want {
				t.Errorf("/readyz = %d, want %d", rec.Code, tt.want)
			}
			if decode(t, rec)["status"] == nil {
				t.Error("readiness body has no status field")
			}
		})
	}
}

func TestSystemInfoAnswersWithoutTheDatabase(t *testing.T) {
	t.Parallel()

	rec := get(t, newTestAPI(t), "/api/v1/system/info")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := decode(t, rec)
	for _, field := range []string{"version", "go_version", "platform", "environment", "uptime_seconds", "api_version"} {
		if _, ok := body[field]; !ok {
			t.Errorf("response is missing %q: %v", field, body)
		}
	}
	if body["api_version"] != "v1" {
		t.Errorf("api_version = %v, want v1", body["api_version"])
	}
}

// This endpoint is a diagnostic; a non-200 would hide the detail in exactly
// the situation it is needed.
func TestSystemHealthAlwaysReturnsOK(t *testing.T) {
	t.Parallel()

	h := newTestAPI(t, health.Func{
		CheckName:  "database",
		IsCritical: true,
		Probe:      func(context.Context) error { return stderrors.New("unreachable") },
	})

	rec := get(t, h, "/api/v1/system/health")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even when unhealthy", rec.Code)
	}
	if got := decode(t, rec)["status"]; got != string(health.StatusUnhealthy) {
		t.Errorf("status field = %v, want unhealthy", got)
	}
}

func TestSystemRuntimeReportsSelfTelemetry(t *testing.T) {
	t.Parallel()

	rec := get(t, newTestAPI(t), "/api/v1/system/runtime")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := decode(t, rec)
	for _, section := range []string{"database", "event_bus", "process"} {
		if _, ok := body[section]; !ok {
			t.Errorf("response is missing the %q section", section)
		}
	}
	process, _ := body["process"].(map[string]any)
	if n, _ := process["goroutines"].(float64); n <= 0 {
		t.Errorf("goroutines = %v, want a positive count", process["goroutines"])
	}
}

// Every failure must be parseable by the same client-side handler.
func TestUnknownAPIPathReturnsTypedError(t *testing.T) {
	t.Parallel()

	rec := get(t, newTestAPI(t), "/api/v1/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}

	var resp httpx.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("404 body is not the standard envelope: %v", err)
	}
	if resp.Error.Code != errs.CodeNotFound {
		t.Errorf("code = %q, want not_found", resp.Error.Code)
	}
	if resp.Error.RequestID == "" {
		t.Error("error body has no request id")
	}
}

func TestUnsupportedMethodIsRejectedByTheRouter(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newTestAPI(t).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/system/info", nil))

	// Atlas is read-only; a write verb must never reach a handler.
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/v1/system/info = %d, want 405", rec.Code)
	}
}

func TestBaseMiddlewareIsAppliedToEveryRoute(t *testing.T) {
	t.Parallel()

	h := newTestAPI(t)
	for _, path := range []string{"/healthz", "/readyz", "/api/v1/system/info"} {
		rec := get(t, h, path)
		if rec.Header().Get(httpx.RequestIDHeader) == "" {
			t.Errorf("%s has no %s header", path, httpx.RequestIDHeader)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s is missing security headers", path)
		}
	}
}
