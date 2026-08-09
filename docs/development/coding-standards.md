# Coding Standards

Conventions this codebase holds itself to. Where a rule is unusual, the reason
is given — a rule whose reason is not recorded gets removed by whoever finds it
inconvenient.

## The architectural rule

**`internal/platform` may not import `internal/core` or `internal/api`.
`internal/core` may not import `internal/api`.**

This is the most important rule here and it is a review checkpoint. It is what
makes the platform testable with no domain concept present and the domain
testable with no infrastructure. Go's `internal` visibility cannot express it,
so people must.

## Go

### Formatting and tooling

`gofmt -s`, `go vet`, and `golangci-lint` all pass. CI enforces all three.

### Naming

- Packages: short, lowercase, no underscores, and named for what they provide
  rather than what they contain. `errs`, not `errorutil`.
- Avoid stutter: `collect.Registry`, not `collect.CollectRegistry`.
- Interfaces are named for behaviour: `Collector`, `Transport`, `Checker`.
- Acronyms keep their case: `nodeID`, `httpServer`, `dsnURL`.

### Errors

Every error crossing a layer boundary carries an `errs.Code`:

```go
return errs.Wrap(err, errs.CodeUnavailable, "database is unreachable").
    WithOp("postgres.Pool.Ping")
```

- **`Message` and `Details` are client-safe.** Write them assuming an
  unauthenticated caller will read them.
- **The wrapped cause and `Op` are operator-only.** They stay in logs.
- Use `errs.CodeInternal` for anything unexpected; its message is never derived
  from the cause.
- `Op` is `package.Type.Method`, giving a call path without a stack trace.

Never return a bare `fmt.Errorf` from a function whose error can reach an API
boundary. See [ADR-0009](../adr/0009-typed-error-kernel.md).

### Context

- `context.Context` is the first parameter, always named `ctx`.
- Never store a context in a struct.
- Every blocking operation honours cancellation. This is not a style
  preference: a collector that ignores its context can pin a scheduler slot on
  a wedged filesystem, which is exactly when monitoring must keep working.
- Use `context.WithoutCancel` when deriving a shutdown or cleanup context from
  one that is already cancelled — otherwise the cleanup gets an expired
  deadline and a graceful drain becomes an immediate kill.

### Concurrency

- Every concurrent type documents what guards what.
- Prefer a channel as a semaphore over a counter plus condition variable.
- Non-blocking sends (`select` with `default`) where a slow consumer must never
  affect a producer.
- Any goroutine started must have a defined way to stop.
- `-race` on every test run.

### Comments

Comments explain **why**, not what. The code states what it does.

```go
// Bad — restates the code.
// Set the max connections.
poolCfg.MaxConns = cfg.MaxConns

// Good — explains a decision the reader cannot infer.
// Jitter prevents a pool opened all at once from expiring all at once,
// which would otherwise produce a periodic reconnect stampede.
poolCfg.MaxConnLifetimeJitter = cfg.MaxConnLifetime / 10
```

Every exported symbol has a doc comment beginning with its name. Package
comments explain the package's purpose and any decision that shaped it; where
the decision had alternatives worth recording, link the ADR.

Comment density should match the surrounding code. Dense reasoning belongs
where a reader would otherwise be surprised — a non-obvious trade-off, a
deliberate omission, an ordering that matters.

### Testing

See the [testing strategy](testing.md). In brief: external test packages,
table-driven, `t.Parallel()`, injected clocks, hand-written doubles, and test
names that state a claim.

### Dependencies

The production dependency list is `pgx` and `yaml.v3`. Adding a third-party
dependency requires justifying it against the standard library, and the bar is
high: every dependency is a supply-chain surface and a maintenance obligation
for the life of the product.

This is why Atlas uses `log/slog` rather than a logging library, `net/http`'s
router rather than a framework, and a hand-written migrator rather than a
migration library — in each case the standard library or a small amount of
owned code covers the need.

## TypeScript

### Strictness

`strict`, `noUncheckedIndexedAccess`, and `exactOptionalPropertyTypes` are all
on, and ESLint runs type-aware rules.

This is deliberate for an operations console: a silently `undefined` field
renders as a blank cell, which an operator reads as *the absence of a problem*
rather than the absence of data. A build failure is strictly better.

### Data fetching

- All HTTP goes through `apiGet` in `src/api/client.ts`, so error handling
  exists once.
- Untrusted JSON is narrowed with a type guard, not asserted. A proxy's HTML
  error page must not flow through as if it were an Atlas envelope.
- Server state lives in TanStack Query, not in component state.
- The `QueryClient` is created at module scope; creating it inside a component
  discards the cache on every re-render, turning a dashboard into a request
  storm against the thing it monitors.

### Components

- Function components with hooks.
- **Every data-driven view distinguishes loading, error, and empty.** All three
  look identical if they render as a blank box, and conflating them in an
  operations tool means a broken query reads as a healthy system.
- Status is never conveyed by colour alone; it always carries a label or an
  accessible name.

## Git

- Commit messages: imperative mood, explaining why rather than what.
- A commit that changes behaviour includes its tests and its documentation.
- A pull request that changes behaviour without updating the affected document
  is incomplete.

## Review checklist

- [ ] The layering rule is respected.
- [ ] Errors carry an appropriate code, and nothing sensitive is in `Message`
      or `Details`.
- [ ] Every blocking call honours its context.
- [ ] New concurrency is race-tested.
- [ ] Comments explain why, not what.
- [ ] Tests state a claim that would matter if it broke.
- [ ] Documentation is updated in the same change.
- [ ] Nothing modifies an observed system.
