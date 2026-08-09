// Package httpx provides Atlas's HTTP plumbing: a uniform response envelope,
// a middleware chain, and a supervised server component.
//
// It knows nothing about servers, containers, or metrics. Feature packages
// depend on httpx; httpx depends on none of them. That direction is what lets
// the API surface grow through every tier without this layer changing.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
)

// ErrorBody is the error payload returned by every Atlas endpoint.
//
// A single shape across the whole API means a client writes one error handler
// rather than one per endpoint, and that handler can branch on Code — a
// stable contract — instead of parsing prose.
type ErrorBody struct {
	// Code is a stable [errs.Code]. Clients branch on this.
	Code errs.Code `json:"code"`
	// Message is human-readable and safe to display.
	Message string `json:"message"`
	// Details carries structured context, such as the field that failed
	// validation. Absent when there is none.
	Details map[string]any `json:"details,omitempty"`
	// RequestID ties the response to server-side logs. Quoting it in a bug
	// report is enough to find the exact request.
	RequestID string `json:"request_id,omitempty"`
}

// ErrorResponse is the top-level JSON body for a failed request.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// statusByCode maps Atlas error codes to HTTP statuses.
//
// Centralising the mapping is the point: without it, two handlers eventually
// disagree about whether a missing container is 404 or 400, and clients end up
// coding to the endpoint rather than to the contract.
var statusByCode = map[errs.Code]int{
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
}

// StatusFor returns the HTTP status for an error code. Unknown codes map to
// 500, matching [errs.CodeOf]'s treatment of unclassified errors.
func StatusFor(code errs.Code) int {
	if status, ok := statusByCode[code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// JSON writes v as a JSON response with the given status.
//
// Encoding happens into a buffer before anything is written, so an encoding
// failure can still produce a clean 500. Encoding straight to the
// ResponseWriter would commit a 200 and then emit truncated JSON — a failure
// that looks like success to every client.
func JSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		log.FromContext(r.Context()).ErrorContext(r.Context(),
			"failed to encode response body", slog.Any("error", err))
		Error(w, r, errs.Wrap(err, errs.CodeInternal, "response encoding failed"))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// NoContent writes a 204 with no body.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error writes err as the standard error envelope, choosing the status from
// the error's code.
//
// Client-safe fields come from [errs.Message] and [errs.Details], which
// return generic values for internal errors. The full error, including its
// cause, is logged instead — at error level for 5xx, warn for 4xx, since a
// client's bad request is not an operator's problem to page on.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	code := errs.CodeOf(err)
	status := StatusFor(code)

	logger := log.FromContext(ctx)
	attrs := []any{
		slog.String("error_code", string(code)),
		slog.Int("status", status),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("error", err),
	}
	if status >= http.StatusInternalServerError {
		logger.ErrorContext(ctx, "request failed", attrs...)
	} else {
		logger.WarnContext(ctx, "request rejected", attrs...)
	}

	body := ErrorResponse{Error: ErrorBody{
		Code:      code,
		Message:   errs.Message(err),
		Details:   errs.Details(err),
		RequestID: RequestIDFromContext(ctx),
	}}

	payload, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		// Details came from a handler and could contain an unmarshalable
		// value. Never fail to produce an error response; drop the details.
		body.Error.Details = nil
		payload, _ = json.Marshal(body)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// Handler is an http.Handler whose ServeHTTP may return an error.
//
// Returning an error rather than writing one lets a handler end with
// `return err` and be certain the response is rendered, logged, and given the
// right status by one piece of code. Handlers that forget are the usual
// source of an endpoint that returns 200 with an empty body on failure.
type Handler func(w http.ResponseWriter, r *http.Request) error

// ServeHTTP implements http.Handler.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		Error(w, r, err)
	}
}
