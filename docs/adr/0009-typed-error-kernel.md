# ADR-0009: A typed error kernel with a redaction boundary

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

Errors in Atlas cross layers and reach two very different audiences.

An **operator** needs everything: the failing operation, the wrapped cause, the
connection target, the SQL state. A **client** — potentially unauthenticated,
and reading an API that describes an organisation's entire infrastructure —
must receive a stable classification and a safe message, and nothing more.

The default Go idiom, `fmt.Errorf("...: %w", err)`, produces a single string
for both audiences. When such an error reaches an HTTP handler, the natural
implementation is `http.Error(w, err.Error(), 500)`, and that one line publishes
the database host, the username, and the reason authentication failed.

This is not a hypothetical. It is one of the most common information-disclosure
findings in web application assessments, and it happens because the safe path
requires every handler to remember to redact.

## Decision

**A typed error kernel, `internal/platform/errs`, in which the redaction
boundary is a property of the error type rather than a habit of the handler.**

```go
type Error struct {
    Code    Code            // stable, machine-readable; part of the API contract
    Message string          // client-safe
    Op      string          // logical operation; logs only
    Details map[string]any  // client-safe structured context
    cause   error           // unexported: logs only
}
```

Three accessors define the boundary:

- `errs.CodeOf(err)` — the classification. Unclassified errors report
  `internal`, because an unclassified failure is by definition unexpected.
- `errs.Message(err)` — client-safe text. **For `internal` or unclassified
  errors it returns a fixed generic string, never the underlying text.**
- `errs.Details(err)` — client-safe structured context; nil for internal errors.

`err.Error()` returns the full operator-facing chain and is used only for logs.

`internal/platform/httpx` maps codes to HTTP statuses in exactly one table and
builds every error response from those three accessors. The same accessors back
the health report, so a failing dependency check cannot leak a driver error
either.

The package re-exports `Is`, `As`, `Join`, and `Unwrap` from the standard
library, so a caller never needs to import both.

## Alternatives considered

**Plain `fmt.Errorf` with `%w`.** Idiomatic and dependency-free. Rejected for
the disclosure reason above, and because there is no way to map an error to an
HTTP status without either string matching or sentinel comparison at every call
site — which produces exactly the inconsistency where one endpoint returns 404
for a missing container and another returns 400.

**`pkg/errors` or `cockroachdb/errors`.** Mature, with stack traces. Rejected
because neither addresses the audience separation, which is the actual problem
here; Atlas would still need this layer on top. Stack traces were also judged
not worth their cost: the `Op` chain gives a call path for a fraction of the
allocation, and it is composed of names chosen by the author rather than
function names chosen by the compiler.

**Sentinel errors with `errors.Is` at handler level.** Works for a small fixed
set. Rejected because it does not scale to the number of failure modes across
tiers, and it puts the status decision back in each handler.

**Redacting in the HTTP layer only.** Simpler, and one place to get right.
Rejected because the HTTP layer is not the only egress: the health report, the
plugin activation state, the scheduler's failure events, and eventually the
WebSocket stream all carry error text outward. Putting the boundary in the
error type means each of those inherits it rather than reimplementing it.

## Consequences

**Good.**

- Internal detail cannot leak through any egress, regardless of what a handler
  does. This is asserted by tests at three levels: the error package, the HTTP
  layer, and the health package each verify that a wrapped credential-bearing
  error surfaces only the generic message.
- One code-to-status table means the API is consistent by construction.
- `Code` is a stable contract clients can branch on, and the TypeScript client
  mirrors it as a union type.
- The `Op` chain gives a readable failure path in logs at negligible cost.
- Errors carry client-safe `Details`, so a validation failure can name the
  offending field without a bespoke error type.

**Costs.**

- Constructing errors is more verbose than `fmt.Errorf`: a code must be chosen
  every time. That is the intended friction — choosing a code is choosing what
  the caller will see.
- An author can still defeat the boundary by putting sensitive data in
  `Message` or `Details`. Documented in the type's doc comment as "write these
  assuming an unauthenticated caller will read them", and a review checkpoint.
- Debugging a client-reported problem requires correlating by request id rather
  than reading the response body. This is the intended trade, and it is why
  every error body carries `request_id`.

**Revisit if** the `Op` chain proves insufficient for diagnosing production
failures, at which point adding opt-in stack capture for `internal` errors is
the incremental step.
