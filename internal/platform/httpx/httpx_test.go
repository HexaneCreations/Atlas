package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/log"
)

func decodeError(t *testing.T, body []byte) httpx.ErrorResponse {
	t.Helper()
	var resp httpx.ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return resp
}

func TestStatusForEveryCode(t *testing.T) {
	t.Parallel()

	tests := map[errs.Code]int{
		errs.CodeInvalidArgument:    http.StatusBadRequest,
		errs.CodeUnauthenticated:    http.StatusUnauthorized,
		errs.CodePermissionDenied:   http.StatusForbidden,
		errs.CodeNotFound:           http.StatusNotFound,
		errs.CodeMethodNotAllowed:   http.StatusMethodNotAllowed,
		errs.CodeAlreadyExists:      http.StatusConflict,
		errs.CodeFailedPrecondition: http.StatusPreconditionFailed,
		errs.CodeRateLimited:        http.StatusTooManyRequests,
		errs.CodeDeadlineExceeded:   http.StatusGatewayTimeout,
		errs.CodeUnavailable:        http.StatusServiceUnavailable,
		errs.CodeNotImplemented:     http.StatusNotImplemented,
		errs.CodeInternal:           http.StatusInternalServerError,
		errs.Code("unknown"):        http.StatusInternalServerError,
	}

	for code, want := range tests {
		if got := httpx.StatusFor(code); got != want {
			t.Errorf("StatusFor(%q) = %d, want %d", code, got, want)
		}
	}
}

// The HTTP layer must not become a second place where redaction can be
// forgotten; it inherits the errs boundary.
func TestErrorResponseRedactsInternalDetail(t *testing.T) {
	t.Parallel()

	const secret = "dial tcp 10.0.0.5:5432: password authentication failed"
	handler := httpx.Handler(func(http.ResponseWriter, *http.Request) error {
		return errs.Wrap(stderrors.New(secret), errs.CodeInternal, "query failed").
			WithOp("postgres.Pool.Query")
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/servers", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") || strings.Contains(rec.Body.String(), "password") {
		t.Errorf("response leaked internal detail: %s", rec.Body.String())
	}
	if got := decodeError(t, rec.Body.Bytes()).Error.Code; got != errs.CodeInternal {
		t.Errorf("code = %q, want internal", got)
	}
}

func TestErrorResponseCarriesClientSafeDetail(t *testing.T) {
	t.Parallel()

	handler := httpx.Handler(func(http.ResponseWriter, *http.Request) error {
		return errs.New(errs.CodeNotFound, "container not found").WithDetail("id", "abc123")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/containers/abc123", nil)
	httpx.RequestID()(handler).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := decodeError(t, rec.Body.Bytes())
	if body.Error.Message != "container not found" {
		t.Errorf("message = %q", body.Error.Message)
	}
	if body.Error.Details["id"] != "abc123" {
		t.Errorf("details = %v", body.Error.Details)
	}
	if body.Error.RequestID == "" {
		t.Error("error body should carry the request id")
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	t.Parallel()

	var seen string
	h := httpx.RequestID()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestIDFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no request id on the context")
	}
	if got := rec.Header().Get(httpx.RequestIDHeader); got != seen {
		t.Errorf("header = %q, context = %q; they must match", got, seen)
	}
}

// An inbound id is echoed into logs and JSON, so untrusted values must be
// rejected rather than propagated.
func TestRequestIDSanitisesInboundValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		inbound  string
		accepted bool
	}{
		{"clean", "trace-abc_123.4", true},
		{"newline injection", "abc\nlevel=error msg=fake", false},
		{"ansi escape", "abc\x1b[31m", false},
		{"whitespace", "abc def", false},
		{"json breakout", `abc","admin":true`, false},
		{"too long", strings.Repeat("a", 129), false},
		{"at limit", strings.Repeat("a", 128), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var seen string
			h := httpx.RequestID()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = httpx.RequestIDFromContext(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(httpx.RequestIDHeader, tt.inbound)
			h.ServeHTTP(httptest.NewRecorder(), req)

			if tt.accepted && seen != tt.inbound {
				t.Errorf("id = %q, want the inbound value %q", seen, tt.inbound)
			}
			if !tt.accepted && seen == tt.inbound {
				t.Errorf("unsafe inbound id %q was propagated", tt.inbound)
			}
			if seen == "" {
				t.Error("a rejected inbound id must still be replaced with a generated one")
			}
		})
	}
}

func TestRecovererConvertsPanicToFiveHundred(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := log.New(log.Config{Level: "debug", Format: log.FormatJSON}, &buf)
	if err != nil {
		t.Fatal(err)
	}

	h := httpx.Chain(httpx.Recoverer(), httpx.RequestID())(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("collector dereferenced a vanished process")
		}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/processes", nil)
	h.ServeHTTP(rec, req.WithContext(log.Into(req.Context(), logger)))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "vanished process") {
		t.Errorf("panic text leaked to the client: %s", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "panic recovered") {
		t.Error("panic was not logged")
	}
	if !strings.Contains(buf.String(), "stack") {
		t.Error("panic log has no stack trace")
	}
}

// net/http uses ErrAbortHandler as a control signal; swallowing it would
// break connection aborts.
func TestRecovererRepanicsOnErrAbortHandler(t *testing.T) {
	t.Parallel()

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", rec)
		}
	}()

	h := httpx.Recoverer()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestChainOrdersOutermostFirst(t *testing.T) {
	t.Parallel()

	var order []string
	mw := func(name string) httpx.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	httpx.Chain(mw("first"), mw("second"), mw("third"))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			order = append(order, "handler")
		}),
	).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "third", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestCORS(t *testing.T) {
	t.Parallel()

	allowed := []string{"https://atlas.example.com"}

	tests := []struct {
		name        string
		origin      string
		wantAllowed bool
	}{
		{"allowed origin", "https://atlas.example.com", true},
		{"other origin", "https://evil.example.com", false},
		{"scheme mismatch", "http://atlas.example.com", false},
		{"no origin header", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := httpx.CORS(allowed)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			h.ServeHTTP(rec, req)

			got := rec.Header().Get("Access-Control-Allow-Origin")
			if tt.wantAllowed && got != tt.origin {
				t.Errorf("Allow-Origin = %q, want %q", got, tt.origin)
			}
			if !tt.wantAllowed && got != "" {
				t.Errorf("Allow-Origin = %q, want empty", got)
			}
			// Vary must always be present, or a shared cache can serve one
			// origin's response to another.
			if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
				t.Error("Vary: Origin missing")
			}
			// A disallowed origin still gets a normal response; the browser,
			// not the server, enforces the policy.
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		})
	}
}

func TestCORSPreflight(t *testing.T) {
	t.Parallel()

	h := httpx.CORS([]string{"https://atlas.example.com"})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("preflight reached the handler")
		}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/servers", nil)
	req.Header.Set("Origin", "https://atlas.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("preflight response has no Allow-Methods")
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	h := httpx.SecurityHeaders()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestAccessLogRecordsStatusAndBytes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := log.New(log.Config{Level: "debug", Format: log.FormatJSON}, &buf)
	if err != nil {
		t.Fatal(err)
	}

	h := httpx.AccessLog()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(log.Into(req.Context(), logger)))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	if rec["status"] != float64(http.StatusTeapot) {
		t.Errorf("status = %v, want 418", rec["status"])
	}
	if rec["bytes"] != float64(5) {
		t.Errorf("bytes = %v, want 5", rec["bytes"])
	}
	if rec["path"] != "/v1/servers" {
		t.Errorf("path = %v", rec["path"])
	}
}

// Probes run every few seconds; at info level they would bury real traffic.
func TestAccessLogDemotesProbes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := log.New(log.Config{Level: "info", Format: log.FormatJSON}, &buf)
	if err != nil {
		t.Fatal(err)
	}

	h := httpx.AccessLog()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req.WithContext(log.Into(req.Context(), logger)))
	}

	if buf.Len() != 0 {
		t.Errorf("probe requests logged at info level: %s", buf.String())
	}
}

// Streaming endpoints for live logs and Docker events depend on Flush
// reaching through the access-log wrapper.
func TestResponseWriterRemainsFlushable(t *testing.T) {
	t.Parallel()

	flushed := make(chan bool, 1)
	h := httpx.Chain(httpx.Recoverer(), httpx.RequestID(), httpx.AccessLog())(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("chunk"))
			flushed <- http.NewResponseController(w).Flush() == nil
		}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !<-flushed {
		t.Error("Flush did not reach the underlying ResponseWriter")
	}
}

func TestMaxBodyBytes(t *testing.T) {
	t.Parallel()

	h := httpx.MaxBodyBytes(16)(httpx.Handler(func(_ http.ResponseWriter, r *http.Request) error {
		buf := make([]byte, 1024)
		for {
			_, err := r.Body.Read(buf)
			if err != nil {
				if strings.Contains(err.Error(), "too large") {
					return errs.New(errs.CodeInvalidArgument, "request body too large")
				}
				return nil
			}
		}
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 1024)))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized body", rec.Code)
	}
}

// The agent-facing listener is reachable by every enrolled node, so each
// of these protections is load-bearing there specifically — a missing body
// cap or an unrecovered panic affects monitoring for the whole fleet, not
// one browser session.
func TestAgentMiddlewareCapsBodyRecoversPanicsAndTimesOut(t *testing.T) {
	t.Parallel()

	fleetCfg := config.Default().Fleet
	fleetCfg.MaxRequestBytes = 16
	fleetCfg.RequestTimeout = 30 * time.Millisecond

	t.Run("body cap", func(t *testing.T) {
		h := httpx.AgentMiddleware(fleetCfg)(httpx.Handler(func(_ http.ResponseWriter, r *http.Request) error {
			buf := make([]byte, 1024)
			for {
				_, err := r.Body.Read(buf)
				if err != nil {
					if strings.Contains(err.Error(), "too large") {
						return errs.New(errs.CodeInvalidArgument, "request body too large")
					}
					return nil
				}
			}
		}))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", strings.NewReader(strings.Repeat("x", 1024)))
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for an oversized agent body", rec.Code)
		}
	})

	t.Run("panic recovery", func(t *testing.T) {
		h := httpx.AgentMiddleware(fleetCfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("handler dereferenced a nil envelope")
		}))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", nil))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "nil envelope") {
			t.Errorf("panic text leaked to the agent: %s", rec.Body.String())
		}
	})

	t.Run("request timeout", func(t *testing.T) {
		h := httpx.AgentMiddleware(fleetCfg)(httpx.Handler(func(_ http.ResponseWriter, r *http.Request) error {
			select {
			case <-r.Context().Done():
				return errs.New(errs.CodeDeadlineExceeded, "request timed out")
			case <-time.After(5 * time.Second):
				return nil
			}
		}))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", nil))

		if rec.Code != http.StatusGatewayTimeout {
			t.Errorf("status = %d, want 504", rec.Code)
		}
	})

	// No browser calls this listener; advertising a CORS policy nothing
	// enforces would misrepresent the protection actually in effect.
	t.Run("no CORS headers", func(t *testing.T) {
		h := httpx.AgentMiddleware(fleetCfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", nil)
		req.Header.Set("Origin", "https://example.com")
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty on the agent listener", got)
		}
	})
}

func TestTimeoutSetsDeadlineWithoutBuffering(t *testing.T) {
	t.Parallel()

	h := httpx.Timeout(30 * time.Millisecond)(httpx.Handler(func(_ http.ResponseWriter, r *http.Request) error {
		select {
		case <-r.Context().Done():
			return errs.New(errs.CodeDeadlineExceeded, "request timed out")
		case <-time.After(5 * time.Second):
			return nil
		}
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", rec.Code)
	}
}

func TestServerStartServeAndGracefulStop(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Server
	cfg.Port = 0 // let the kernel choose a free port

	mux := http.NewServeMux()
	mux.Handle("GET /ping", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		httpx.JSON(w, r, http.StatusOK, map[string]string{"pong": "true"})
		return nil
	}))

	srv := httpx.NewServer(cfg, mux, slog.New(slog.DiscardHandler))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	resp, err := http.Get("http://" + srv.Addr() + "/ping")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
	// Stop must be safe to call twice; the supervisor may retry.
	if err := srv.Stop(ctx); err != nil {
		t.Errorf("second Stop() error = %v", err)
	}
}

// A port conflict must fail startup, not surface later as a fault.
func TestServerStartFailsOnBindConflict(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Server
	cfg.Port = 0

	first := httpx.NewServer(cfg, http.NewServeMux(), nil)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	defer first.Stop(context.Background())

	_, portStr, _ := net.SplitHostPort(first.Addr())
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("could not parse bound port %q: %v", portStr, err)
	}

	conflicting := config.Default().Server
	conflicting.Host = "127.0.0.1"
	conflicting.Port = port

	second := httpx.NewServer(conflicting, http.NewServeMux(), nil)
	if err := second.Start(context.Background()); err == nil {
		second.Stop(context.Background())
		t.Fatal("Start() succeeded on an already-bound port")
	}
}

func TestStopBeforeStartIsSafe(t *testing.T) {
	t.Parallel()

	srv := httpx.NewServer(config.Default().Server, http.NewServeMux(), nil)
	if err := srv.Stop(context.Background()); err != nil {
		t.Errorf("Stop() before Start error = %v", err)
	}
}
