# ADR-0006: Compiled-in plugins with runtime detection

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

The requirements state that adding support for a new technology — Kubernetes,
Redis, RabbitMQ, MongoDB, nginx — should mean creating a new plugin rather than
modifying the platform core.

There is also a fleet problem that is easy to miss. One Atlas binary runs on
hosts with wildly different software. A machine with no Docker daemon must not
report a broken Docker integration; it must report no Docker integration. Those
look identical to a naive registry, and confusing them means an operator either
chases a phantom fault or ignores a real one.

## Decision

**Plugins are Go packages compiled into the binary, implementing one interface,
and activated through a four-stage lifecycle.**

```go
type Plugin interface {
    Descriptor() Descriptor
    Detect(ctx context.Context) (bool, error)
    Init(ctx context.Context, env Env) error
    Collectors() []collect.Collector
    Close(ctx context.Context) error
}
```

1. **Registered** — compiled in and known to exist.
2. **Detected** — asked whether its subject is actually present on this host.
3. **Initialised** — given its dependencies, contributing collectors.
4. **Closed** — releasing resources at shutdown.

Every outcome is recorded as a `Status` and surfaced through the API:
`active`, `not_detected`, `detection_failed`, `init_failed`, or `disabled`.
The distinction between `not_detected` and `detection_failed` is the whole
point of the detect stage.

Plugins receive an `Env` containing a logger, the event bus, and the node id.
There is **no database handle** in `Env`, and that omission is deliberate:
plugins observe and publish; they do not write to storage.

## Alternatives considered

**Go's `plugin` package, loading `.so` files at runtime.** True runtime
extensibility, and third parties could ship plugins without rebuilding Atlas.
Rejected on four counts, any one of which would be sufficient. It requires the
plugin and host to be built with the identical Go toolchain version and
identical versions of every shared dependency, which is unworkable in practice.
It breaks cross-compilation and static linking, which would end the
single-binary and `scratch`-container properties. It is unsupported on Windows
and poorly supported on macOS. And it gives third-party code execution inside a
process that reads the host's most sensitive state — a security position that
is hard to defend for a monitoring agent running with elevated read access.

**Sidecar plugins as separate processes over gRPC, in the style of HashiCorp's
plugin system.** Genuine isolation, language independence, and a crashing
plugin cannot take down the host process. A serious option, and the right one
for a product with a third-party plugin ecosystem. Rejected for now because it
multiplies operational complexity — process supervision, health checking, and
version negotiation per plugin — for a first-party plugin set, and because
per-sample IPC on the collection hot path is a real cost. Worth revisiting if
third-party plugins become a requirement.

**Scripting, with plugins as Lua or Starlark.** Rejected: the collectors need
efficient syscall-level access to `/proc` and to daemon sockets, which is
exactly what an embedded scripting runtime is worst at.

## Consequences

**Good.**

- One static binary keeps every property that made Go the right choice.
- Plugins are ordinary Go: type-checked, testable with the standard tooling,
  debuggable in place, and refactorable across the whole tree by an IDE.
- No plugin version negotiation, no ABI, no compatibility matrix.
- Adding a technology touches one new directory plus one registration line —
  the requirement is met at the source level, which is where it matters for a
  first-party plugin set.
- Detection makes one binary correct on a heterogeneous fleet, and makes the
  difference between "absent" and "broken" visible to operators.
- Panic isolation in `safeDetect` means a plugin probing an unexpected socket
  state cannot stop the remaining plugins from starting.

**Costs.**

- Adding a plugin requires rebuilding and redeploying Atlas. For a first-party
  plugin set this is the normal release process; for third parties it would be
  disqualifying.
- No third-party plugin ecosystem is possible under this decision.
- Every plugin's dependencies are linked into every binary, so the Kubernetes
  client ships even to hosts that will never see Kubernetes. Currently a
  binary-size concern only, and a small one.

**Revisit if** third-party plugins become a product requirement, at which point
the sidecar-over-gRPC model is the alternative to reach for — and the `Plugin`
interface defined here is close enough to a process boundary that it could
front such an implementation.
