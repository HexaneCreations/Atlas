// Package errs defines Atlas's typed error kernel.
//
// Every error that crosses a layer boundary carries a machine-readable [Code].
// Transport adapters (HTTP, gRPC) translate that code into a protocol status,
// so no handler ever hand-picks a status line and no two endpoints disagree
// about what "not found" means.
//
// An error has two audiences and they are kept strictly apart:
//
//   - Message and Details are safe to return to a client. Write them assuming
//     an unauthenticated caller will read them.
//   - The wrapped cause and Op chain are for operators. They stay in logs.
//
// Keeping the split inside the error type — rather than trusting each handler
// to redact — is what stops internal detail from leaking through an API that
// is otherwise strictly read-only.
package errs

import (
	stderrors "errors"
	"fmt"
	"maps"
	"strings"
)

// Re-exported so callers need only this package, never both it and stdlib
// errors. Shadowing the standard library in import blocks is a recurring
// source of mistakes; this removes the need.
var (
	Is     = stderrors.Is
	As     = stderrors.As
	Join   = stderrors.Join
	Unwrap = stderrors.Unwrap
)

// Code is a stable, machine-readable error classification.
//
// Codes are part of Atlas's public API contract: clients may branch on them,
// so a code's meaning must never change once released. The set is deliberately
// small and modelled on gRPC's canonical codes, which already have well-known
// mappings to every transport Atlas is likely to speak.
type Code string

const (
	// CodeInvalidArgument means the caller supplied a malformed or
	// semantically invalid input. Retrying without changing it will fail.
	CodeInvalidArgument Code = "invalid_argument"
	// CodeNotFound means the requested resource does not exist.
	CodeNotFound Code = "not_found"
	// CodeMethodNotAllowed means the endpoint exists but not for this HTTP
	// method. Distinct from CodeNotFound because it is the signal that tells
	// a caller they used a write verb against Atlas's read-only API.
	CodeMethodNotAllowed Code = "method_not_allowed"
	// CodeAlreadyExists means creating the resource would collide with one
	// that already exists.
	CodeAlreadyExists Code = "already_exists"
	// CodeUnauthenticated means the caller did not present valid credentials.
	CodeUnauthenticated Code = "unauthenticated"
	// CodePermissionDenied means the caller is known but not allowed to
	// perform this operation.
	CodePermissionDenied Code = "permission_denied"
	// CodeFailedPrecondition means the system is not in a state that permits
	// the operation. The caller may be able to fix the state and retry.
	CodeFailedPrecondition Code = "failed_precondition"
	// CodeRateLimited means the caller exceeded a quota and should back off.
	CodeRateLimited Code = "rate_limited"
	// CodeDeadlineExceeded means the operation ran out of time.
	CodeDeadlineExceeded Code = "deadline_exceeded"
	// CodeUnavailable means a dependency is down or unreachable. Retrying
	// later may succeed.
	CodeUnavailable Code = "unavailable"
	// CodeNotImplemented means the capability exists in the API surface but
	// is not supported by this deployment (for example, a plugin that is not
	// installed).
	CodeNotImplemented Code = "not_implemented"
	// CodeInternal is the fallback for unexpected failures. Its Message is
	// never derived from the underlying cause.
	CodeInternal Code = "internal"
)

// Valid reports whether c is a recognised code.
func (c Code) Valid() bool {
	switch c {
	case CodeInvalidArgument, CodeNotFound, CodeMethodNotAllowed, CodeAlreadyExists,
		CodeUnauthenticated, CodePermissionDenied, CodeFailedPrecondition,
		CodeRateLimited, CodeDeadlineExceeded, CodeUnavailable,
		CodeNotImplemented, CodeInternal:
		return true
	}
	return false
}

// Error is Atlas's structured error type.
//
// Construct one with [New] or [Wrap] rather than by literal, so that the
// client-safe and operator-only fields cannot be transposed by accident.
type Error struct {
	// Code classifies the failure. Always set.
	Code Code
	// Message is a human-readable description safe to return to any caller.
	Message string
	// Op names the logical operation that failed, such as
	// "postgres.Migrator.Apply". Ops accumulate as the error propagates,
	// producing a call path without the cost of a stack trace. Logs only.
	Op string
	// Details carries structured, client-safe context — a field name that
	// failed validation, the id that was not found. Never put credentials,
	// file paths, or dependency error text here.
	Details map[string]any

	// cause is the wrapped underlying error. Operator-facing only: it is
	// reachable via Unwrap for logging and errors.Is/As, and is never
	// rendered into a client response.
	cause error
}

// New builds an Error with no underlying cause.
//
// The message is formatted eagerly and must be safe to show to a client.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap annotates cause with a code and a client-safe message.
//
// Wrap returns nil when cause is nil, so it can be used directly on the result
// of a call without a preceding nil check. If cause is already an *Error, its
// Details are inherited; the new code and message take precedence, which lets
// an outer layer restate a low-level failure in its own vocabulary.
func Wrap(cause error, code Code, format string, args ...any) *Error {
	if cause == nil {
		return nil
	}
	e := &Error{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
		cause:   cause,
	}
	var inner *Error
	if As(cause, &inner) && len(inner.Details) > 0 {
		e.Details = maps.Clone(inner.Details)
	}
	return e
}

// WithOp records the operation that failed and returns e for chaining.
func (e *Error) WithOp(op string) *Error {
	e.Op = op
	return e
}

// WithDetail attaches a client-safe key/value pair and returns e for chaining.
func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, 4)
	}
	e.Details[key] = value
	return e
}

// Error renders the full operator-facing chain, including wrapped causes.
//
// This output may reach logs but must never reach an API response; use
// [Message] for that.
func (e *Error) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString(string(e.Code))
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// CodeOf reports the [Code] carried by err.
//
// Errors that did not originate in Atlas have no code of their own, so they
// are reported as [CodeInternal]: an unclassified failure is by definition
// unexpected, and treating it as anything less severe would understate it.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// HasCode reports whether err carries the given code.
func HasCode(err error, code Code) bool { return CodeOf(err) == code }

// Message returns the client-safe message for err.
//
// For unclassified or internal errors it returns a fixed generic string rather
// than err.Error(), because an arbitrary Go error's text may embed connection
// strings, host names, or query fragments. This is the single choke point that
// makes that leak impossible.
func Message(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if As(err, &e) && e.Code != CodeInternal && e.Message != "" {
		return e.Message
	}
	return "An internal error occurred."
}

// Details returns the client-safe details for err, or nil if there are none.
//
// Internal errors expose no details, for the same reason they expose no
// message.
func Details(err error) map[string]any {
	var e *Error
	if err != nil && As(err, &e) && e.Code != CodeInternal {
		return e.Details
	}
	return nil
}
