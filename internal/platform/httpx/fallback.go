package httpx

import (
	"net/http"
	"strings"

	"github.com/hexane/atlas/internal/platform/errs"
)

// JSONErrorFallback rewrites the router's built-in plain-text errors into
// Atlas's JSON envelope.
//
// net/http's ServeMux answers an unrouted path with a text/plain 404 and a
// path routed only for other methods with a text/plain 405. Both are correct
// HTTP and neither is parseable by a client that expects every Atlas failure
// to have the same shape.
//
// The obvious alternative — registering a catch-all handler on the mux — is
// worse than it looks: a catch-all matches before the router can decide that
// a path exists under a different method, so every 405 silently becomes a
// 404. That is a real loss, because 405 is what tells a caller they used a
// write verb against a read-only API.
//
// So the router keeps its own matching, and this middleware translates only
// what the router itself produced, identified by a response that carries no
// JSON content type.
func JSONErrorFallback() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fw := &fallbackWriter{ResponseWriter: w, request: r}
			next.ServeHTTP(fw, r)
		})
	}
}

// fallbackWriter intercepts a router-generated error and replaces it.
type fallbackWriter struct {
	http.ResponseWriter
	request *http.Request

	intercepted bool
	wroteHeader bool
}

func (w *fallbackWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	// A handler that produced its own JSON error has already set the content
	// type; only the router's plain-text output is rewritten.
	isJSON := strings.HasPrefix(w.Header().Get("Content-Type"), "application/json")
	if !isJSON && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) {
		w.intercepted = true
		Error(w.ResponseWriter, w.request, errorForStatus(status, w.request))
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *fallbackWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.intercepted {
		// Swallow the router's plain-text body; the JSON envelope is already
		// written. Report success so the caller sees no I/O error.
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap keeps http.ResponseController working through this wrapper, which
// streaming endpoints depend on for Flush.
func (w *fallbackWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func errorForStatus(status int, r *http.Request) error {
	if status == http.StatusMethodNotAllowed {
		return errs.New(errs.CodeMethodNotAllowed, "method %s is not allowed on this endpoint", r.Method).
			WithDetail("method", r.Method).
			WithDetail("path", r.URL.Path).
			WithDetail("allowed", "Atlas is read-only; only GET, HEAD, and OPTIONS are supported").
			WithOp("httpx.JSONErrorFallback")
	}
	return errs.New(errs.CodeNotFound, "no such endpoint").
		WithDetail("path", r.URL.Path).
		WithOp("httpx.JSONErrorFallback")
}
