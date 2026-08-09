# ADR-0004: Explicit constructor injection, no DI container

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

The requirements call for dependency injection. Atlas has a real dependency
graph with ordering constraints: the pool before the migrator, the migrator
before anything that reads the schema, everything before the HTTP server.

Go offers several approaches: a runtime container such as Uber's `dig` or `fx`,
compile-time generation with Google's `wire`, or writing the wiring by hand.

## Decision

**Hand-written constructor injection in one composition root,
`internal/app/app.go`.** Every dependency is constructed there and passed
explicitly into whatever needs it. No container, no reflection, no code
generation.

Components are registered with `lifecycle.Supervisor` in dependency order;
startup follows that order and shutdown reverses it exactly.

## Alternatives considered

**A runtime container (`dig`, `fx`).** Automatic resolution, and the graph
scales without the wiring file growing. Rejected because it converts a missing
dependency from a compile error into a runtime panic during startup, and
because the resolution order becomes implicit — which is precisely the thing
Atlas most needs to be explicit about, since the pool must be up before
migrations run. It also adds a framework that every future maintainer must
learn before they can read `main`.

**Compile-time generation (`wire`).** Keeps type safety and removes the manual
wiring. A defensible choice. Rejected as insufficiently valuable at Atlas's
scale: the generated file must still be regenerated and committed, the error
messages are harder to read than a plain constructor call, and the graph is
small enough that the file it would generate is barely shorter than the one
written by hand. The cost — a build-time tool and a generated artifact in
review — is not repaid.

**Package-level singletons.** Simplest to write. Rejected outright: it makes
components untestable in isolation, makes initialisation order implicit and
fragile, and makes two instances in one process impossible — which the
integration tests require, since they boot a full Atlas per test.

## Consequences

**Good.**

- The dependency graph is readable top to bottom in one file. A new engineer
  can see the entire structure of the process in a few minutes.
- A missing or mistyped dependency is a compile error, not a startup panic.
- Startup and shutdown order is written down, not inferred, and is asserted by
  tests in `internal/platform/lifecycle`.
- No framework to learn, no reflection to debug, no generated code to review.
- Tests construct exactly the subgraph they need. The API tests build a router
  with an unstarted pool, because the endpoints under test must answer when the
  database is down.

**Costs.**

- `app.go` grows as Atlas does. This is accepted, and is arguably the point: a
  composition root that stays small is usually one that has pushed its wiring
  somewhere less visible.
- Adding a dependency deep in the graph means threading it through, which is
  more typing than a container would need. That friction is a mild but real
  incentive against gratuitous dependencies.

**Revisit if** the composition root exceeds roughly 500 lines, at which point
splitting it into per-subsystem constructors — still hand-written — is the next
step, not adopting a container.
