# Architecture Decision Records

An ADR records one significant decision: the context that forced it, the
options considered, the choice made, and what that choice costs.

## Why we keep them

The expensive question on a long-lived system is never "what does this code
do" — the code answers that. It is "why is it like this, and what breaks if I
change it?" Without a record, that knowledge lives only in the heads of people
who were present, and it leaves when they do. A team then either preserves a
constraint nobody can justify, or removes one whose reason they never learned.

## Rules

1. **ADRs are immutable.** Once accepted, an ADR is never edited to reflect a
   changed decision. Write a new one that supersedes it and link both ways.
   The point is to preserve reasoning, including reasoning later found wrong.
2. **Record the alternatives.** An ADR listing only the chosen option is a
   description, not a decision. The rejected options are the valuable part.
3. **Record the cost.** Every real decision has one. An ADR with no downside
   in its consequences section has not been thought through.
4. **One decision per record.**

## Status values

| Status | Meaning |
| --- | --- |
| Proposed | Under discussion |
| Accepted | In force |
| Superseded | Replaced; the record names its replacement |
| Deprecated | No longer applies, with no direct replacement |

## Index

| # | Title | Status | Phase |
| --- | --- | --- | --- |
| [0001](0001-go-and-react-typescript.md) | Go backend with a React and TypeScript frontend | Accepted | 0 |
| [0002](0002-modular-monolith.md) | Modular monolith with enforced layering | Accepted | 0 |
| [0003](0003-postgresql-timescaledb.md) | PostgreSQL with TimescaleDB as the single datastore | Accepted | 0 |
| [0004](0004-constructor-injection.md) | Explicit constructor injection, no DI container | Accepted | 0 |
| [0005](0005-transport-seam.md) | A transport seam between collection and storage | Accepted | 0 |
| [0006](0006-compiled-in-plugins.md) | Compiled-in plugins with runtime detection | Accepted | 0 |
| [0007](0007-forward-only-migrations.md) | Forward-only database migrations | Accepted | 0 |
| [0008](0008-lossy-event-bus.md) | A lossy, non-blocking event bus | Accepted | 0 |
| [0009](0009-typed-error-kernel.md) | A typed error kernel with a redaction boundary | Accepted | 0 |
| [0010](0010-url-path-api-versioning.md) | URL-path API versioning | Accepted | 0 |
| [0011](0011-deferred-rbac.md) | Authentication and RBAC, deferred with a fixed shape | Superseded by 0013 | 4 |
| [0012](0012-connect-by-identity.md) | Connect by identity, never by address | Accepted | 4 |
| [0013](0013-human-user-authentication-and-authorization.md) | Human-user authentication and authorization | Accepted | 4 |

## Template

```markdown
# ADR-NNNN: Title

- **Status:** Proposed | Accepted | Superseded by ADR-NNNN
- **Date:** YYYY-MM-DD
- **Phase:** N

## Context
The situation that forces a decision. Facts and constraints, not opinions.

## Decision
What we will do, stated plainly.

## Alternatives considered
Each option, with why it was rejected. Be fair to the ones not chosen.

## Consequences
What this makes easy, what it makes hard, and what it costs. Include the
conditions under which this decision should be revisited.
```
