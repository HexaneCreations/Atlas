package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/hexane/atlas/internal/platform/log"
)

// Middleware wraps an http.Handler with additional behaviour.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware into a single wrapper.
//
// The first middleware listed is the outermost, so a chain reads top to
// bottom in the order a request encounters it. Getting this backwards is the
// classic middleware bug — a recoverer installed inside the logger cannot
// protect it — so the order is fixed here and asserted in tests.
func Chain(mw ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(mw) - 1; i >= 0; i-- {
			next = mw[i](next)
		}
		return next
	}
}

// RequestIDHeader carries the correlation id in and out of Atlas.
const RequestIDHeader = "X-Request-Id"

// maxInboundRequestIDLen bounds a client-supplied id. It is echoed into logs
// and response bodies, so it must not be a channel for unbounded data.
const maxInboundRequestIDLen = 128

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID assigns every request a correlation id, exposes it on the
// context, attaches it to the request's log attributes, and echoes it in the
// response header.
//
// An inbound X-Request-Id is honoured so a trace begun at a load balancer or
// an upstream service survives into Atlas — but only after validation:
// characters are restricted and length is capped, because this value reaches
// log files and JSON responses. An untrusted string that lands in a log
// aggregator is a log-injection vector.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := sanitizeRequestID(r.Header.Get(RequestIDHeader))
			if rid == "" {
				rid = id.New()
			}

			ctx := context.WithValue(r.Context(), requestIDKey, rid)
			ctx = log.WithAttrs(ctx, slog.String("request_id", rid))

			w.Header().Set(RequestIDHeader, rid)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sanitizeRequestID returns the id if it is safe to propagate, else "".
func sanitizeRequestID(raw string) string {
	if raw == "" || len(raw) > maxInboundRequestIDLen {
		return ""
	}
	for _, c := range raw {
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum && c != '-' && c != '_' && c != '.' {
			return ""
		}
	}
	return raw
}

// RequestIDFromContext returns the correlation id for the request, or "" if
// the RequestID middleware is not installed.
func RequestIDFromContext(ctx context.Context) string {
	rid, _ := ctx.Value(requestIDKey).(string)
	return rid
}

// Recoverer converts a panicking handler into a 500 and keeps the process
// alive.
//
// Atlas reads from many unstable surfaces — /proc entries that vanish
// mid-read, Docker payloads that change shape between engine versions — and a
// nil dereference in one collector's endpoint must not take down monitoring
// for an entire fleet. The stack is logged; the client is told nothing beyond
// the correlation id.
//
// Install this outermost, so it also covers the other middleware.
func Recoverer() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// net/http panics with ErrAbortHandler to abort a connection
				// deliberately. Swallowing it would break that contract.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				ctx := r.Context()
				log.FromContext(ctx).ErrorContext(ctx, "panic recovered in HTTP handler",
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				Error(w, r, errs.New(errs.CodeInternal, "panic in handler").WithOp("httpx.Recoverer"))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog records one structured line per completed request.
//
// Health and readiness probes are logged at debug level: an orchestrator polls
// them every few seconds and at info level they would drown every line that
// matters.
func AccessLog() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			ctx := r.Context()
			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", clientIP(r)),
			}

			logger := log.FromContext(ctx)
			switch {
			case isProbePath(r.URL.Path):
				logger.DebugContext(ctx, "request completed", attrs...)
			case rec.status >= http.StatusInternalServerError:
				logger.ErrorContext(ctx, "request completed", attrs...)
			default:
				logger.InfoContext(ctx, "request completed", attrs...)
			}
		})
	}
}

func isProbePath(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

// clientIP returns the peer address.
//
// It deliberately ignores X-Forwarded-For. That header is client-controlled
// and trustworthy only when a known proxy overwrites it; honouring it
// unconditionally would let any caller forge the address that appears in
// audit logs. Proxy-aware address resolution arrives with the authentication
// work, where the trusted-proxy list it requires also belongs.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recordingWriter captures the status and byte count of a response.
//
// It implements Unwrap so that http.ResponseController reaches the underlying
// writer: streaming endpoints (log tails, event streams) need Flush, and a
// wrapper that hides it would silently buffer a stream forever.
type recordingWriter struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return // net/http already warns about this; don't corrupt our record
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SecurityHeaders sets response headers that constrain how a browser treats
// Atlas's responses.
//
// The API returns JSON describing infrastructure. None of it should ever be
// framed, sniffed as another content type, or sent as a referrer to a third
// party.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			// API responses are per-request infrastructure state and must not
			// be retained by shared caches or browser history.
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}

// CORS answers preflight requests and adds access-control headers for
// origins on the allow list.
//
// Only exact origins are echoed. A request from an origin not on the list
// gets no CORS headers at all rather than an error: the browser enforces the
// policy, and returning a normal response keeps non-browser clients — curl,
// the agent, monitoring probes — working unchanged.
//
// Credentials are not enabled. Atlas has no session cookie yet; the header
// will be added alongside authentication, together with the stricter origin
// rules it requires.
func CORS(allowedOrigins []string) Middleware {
	allowAll := slices.Contains(allowedOrigins, "*")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			// Vary is required whenever the response depends on Origin, or a
			// shared cache may serve one origin's response to another.
			w.Header().Add("Vary", "Origin")

			if origin != "" && (allowAll || slices.Contains(allowedOrigins, origin)) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, "+RequestIDHeader)
				h.Set("Access-Control-Expose-Headers", RequestIDHeader)
				h.Set("Access-Control-Max-Age", "600")
			}

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBodyBytes caps how much of a request body a handler can read.
//
// Atlas is read-only and its request bodies are small, so a low cap costs
// nothing and closes off trivial memory exhaustion.
func MaxBodyBytes(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout gives each request a context deadline.
//
// It sets a deadline rather than using http.TimeoutHandler, which buffers the
// entire response in memory to be able to replace it — behaviour that breaks
// every streaming endpoint Atlas is going to need for live logs and events.
// Handlers are expected to propagate the request context into their I/O, at
// which point the deadline does its job without buffering anything.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// IsWebSocketUpgrade reports whether r is asking to upgrade to a WebSocket
// connection, per RFC 6455: an Upgrade header of "websocket" and a Connection
// header that includes "Upgrade" among its (possibly comma-separated) tokens.
//
// This is the dispatch signal between the JSON request pipeline and the
// streaming one — see [StreamMiddleware] — because it is the one thing that
// actually distinguishes them: not the path, which is routing's job, and not
// a header Atlas invented, which a client would have to know to send. A
// request that claims this but never completes the handshake gets nothing
// more than a connection held open a little longer than usual; it cannot
// reach a handler it isn't routed to.
func IsWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

// StreamMiddleware is the chain a long-lived connection runs through:
// everything [BaseMiddleware] provides except a fixed deadline.
//
// [Timeout] is right for a request-response cycle and wrong for a WebSocket
// that is meant to stay open for as long as an operator is watching a log —
// applying it would silently disconnect a live view mid-incident. A streaming
// handler is expected to bound its own lifetime instead: by the client
// disconnecting (which ends r.Context() once the hijacked connection closes),
// by a ping/pong keepalive that detects a dead peer, and by a maximum session
// duration it enforces itself, documented alongside the handler.
//
// CORS is omitted deliberately rather than folded in. A WebSocket handshake
// is not subject to the browser's CORS preflight machinery — cross-origin
// protection for it has to happen inside the handshake itself (see
// websocket.AcceptOptions.OriginPatterns), and adding CORS response headers
// here would suggest a protection that is not actually in effect.
func StreamMiddleware(cfg config.Server) Middleware {
	return Chain(
		Recoverer(),
		RequestID(),
		AccessLog(),
		SecurityHeaders(),
		MaxBodyBytes(cfg.MaxRequestBytes),
	)
}
