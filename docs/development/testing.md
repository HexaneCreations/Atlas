# Testing Strategy

## What a test is for

A test in Atlas exists to state a guarantee and prove it still holds. If a test
would pass regardless of whether a behaviour is correct, it is documentation
with a runtime cost.

This means the test names in this codebase read as claims:

- `TestSlowSubscriberNeverBlocksPublisher`
- `TestFailedStartRollsBackStartedComponents`
- `TestErrorResponseRedactsInternalDetail`
- `TestApplyRejectsEditedMigration`
- `TestRunsOfOneCollectorNeverOverlap`

Each names a property that, if it broke, would cause real damage. Several
already caught real defects during Phase 0 — a data race on the HTTP server's
listener, a migrator that failed against a database with no ledger yet, and a
router whose catch-all was silently converting every `405` into a `404`.

## Levels

### Unit tests — the default

Fast, hermetic, no external dependencies. Run with `make test`.

Every platform and core package is unit-tested against its own contract:

| Package | What is proven |
| --- | --- |
| `errs` | Internal and unclassified errors never surface their cause; wrapping preserves sentinels and copies details rather than aliasing them |
| `log` | Credential-shaped keys are redacted; context attributes propagate; deriving two children from one context does not let either see the other's attributes |
| `config` | Layer precedence; secrets cannot come from YAML; production hardening; all violations reported at once |
| `eventbus` | A wedged subscriber cannot block a publisher or affect a healthy subscriber; pattern matching; safe concurrent publish, subscribe, and close |
| `lifecycle` | Start order, reverse stop order, rollback on failed start, live deadline after cancellation, bounded shutdown |
| `httpx` | Status mapping, redaction, request-id sanitisation, panic recovery, middleware order, `Flush` reaching through wrappers |
| `health` | Critical versus non-critical aggregation, concurrent execution, redaction, panic containment |
| `collect` | Sample and descriptor validation, registry duplicate rejection, label cloning |
| `transport` | Envelope validation, delivery, failure propagation with the code intact, closed-transport refusal |
| `scheduler` | Timeout enforcement, non-overlapping runs, concurrency ceiling, panic isolation, invalid-sample partitioning, failure and recovery reporting |
| `plugin` | Every activation outcome recorded distinctly, detection panic containment, collector collision reporting, reverse-order close |

### Integration tests — behind a build tag

Real PostgreSQL and TimescaleDB. Run with `make db-up && make test-integration`.

Guarded by `//go:build integration` so `go test ./...` stays hermetic and fast.
Each test creates its own database, so they remain independent and parallel.

These prove what unit tests structurally cannot:

- Migrations actually apply, are idempotent, and install the required
  extensions.
- Five concurrent migrators apply each migration exactly once — the advisory
  lock works against a real Postgres, not a mock of one.
- A failed migration rolls back completely and records nothing.
- An edited migration is rejected by checksum.
- A complete Atlas boots, serves, and shuts down cleanly.
- Write verbs are refused with `405` and the standard envelope.
- Startup refuses to proceed with pending migrations when configured to.

### Race detection — not optional

Every test run uses `-race`, locally and in CI. Atlas is concurrent in the
scheduler, the event bus, the lifecycle supervisor, and the HTTP server, and a
data race in any of them corrupts the observations the platform exists to
produce.

This is not theoretical: the race detector found a genuine race on the HTTP
server's listener during Phase 0, where `Addr()` read a field that `Start`
wrote from another goroutine.

## Conventions

**External test packages.** Tests live in `package foo_test`, importing `foo`.
This forces them to exercise the public API, which means a refactor of
unexported internals does not break the suite — and if it does, that is
information.

The exception is where a test must reach an unexported function whose behaviour
is a guarantee in its own right, such as `verifyChecksums`.

**Table-driven tests** for anything with more than two cases.

**Injected clocks and randomness.** The scheduler takes `Now` and `Jitter`; the
event bus takes `Now`; `config.Load` takes `Lookup` and `ReadFile`. Tests are
deterministic rather than timing-dependent, and they never mutate process
state, so they can run in parallel.

**`t.Parallel()` by default.** Tests that cannot run in parallel are usually
tests that touch global state, which is worth fixing.

**No mocking framework.** Test doubles are hand-written and small: a
`recordingSink`, a `fakeCollector`, a `recorder` component. Hand-written
doubles are readable at the call site and cannot drift from an interface
without a compile error.

**Assert on the claim, not the implementation.** A test that checks how a
scheduler dispatches breaks on refactoring; one that checks that two runs of a
collector never overlap survives it and is what the reader actually cares about.

**Comment the non-obvious ones.** Where a test encodes a subtle property, a
comment above it says what would go wrong without it — for example, why the
lifecycle test asserts that `Stop` receives a live deadline after cancellation.

## Coverage

Coverage is measured and reported but is **not a gate**. A percentage target
reliably produces tests of trivial getters, which raises the number without
raising confidence. What matters is whether the guarantees above are asserted.

```bash
make cover    # writes coverage.html and prints the total
```

## What is not yet tested

Named honestly, since an untested area that nobody has written down is
indistinguishable from one nobody has noticed:

| Gap | Plan |
| --- | --- |
| No frontend unit tests | The Phase 0 frontend is a scaffold with no logic beyond formatting. Vitest and React Testing Library arrive in Phase 1 with the first real views. |
| No end-to-end browser tests | Playwright in Phase 1, once there is a view worth asserting on. |
| No load or soak testing | Phase 1, once collectors produce real ingest volume. |
| No fuzzing | Worth adding for the config env-binder and the migration filename parser. |
| No mutation testing | Considered for the platform packages, where a false sense of coverage is most costly. |

## Writing a test for a new contribution

Ask what would break if this code were wrong, and assert that. Concretely:

- **A collector:** call `Collect` and check the samples. Cover the case where
  the subject is absent or malformed.
- **An endpoint:** add a case to `internal/api/router_test.go` covering the
  success shape, the error shape, and the status.
- **A platform package:** unit-test the contract, including its concurrent
  behaviour under `-race`.
- **Anything touching the database:** an integration test behind the tag.
