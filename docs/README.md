# Atlas Documentation

> **Atlas — Observe Everything. Control Nothing.**

Atlas is a production-grade Internal Developer Platform for infrastructure
observability: a single pane of glass over servers, containers, services,
processes, cron jobs, logs, and operational health. It is **strictly
read-only** and never modifies the systems it observes.

## Start here

| If you want to… | Read |
| --- | --- |
| Run Atlas locally in five minutes | [Developer guide](development/developer-guide.md) |
| Understand how the system fits together | [System architecture](architecture/system-architecture.md) |
| Know *why* something is built the way it is | [Architecture Decision Records](adr/README.md) |
| Deploy Atlas | [Deployment guide](operations/deployment.md) |
| Configure Atlas | [Configuration guide](operations/configuration.md) |
| Call the API | [API reference](api/README.md) |
| Add support for a new technology | [Plugin development guide](plugins/plugin-development.md) |
| Diagnose a problem | [Troubleshooting guide](operations/troubleshooting.md) |
| See what is built and what is next | [Phase plan](roadmap/phases.md) |

## Documentation map

### Architecture
- [System architecture](architecture/system-architecture.md) — components, boundaries, and the shape of the whole
- [Backend architecture](architecture/backend-architecture.md) — package layout, layering rules, and the dependency direction
- [Frontend architecture](architecture/frontend-architecture.md) — the web application's structure and data-fetching model
- [Data flow](architecture/data-flow.md) — sequence and flow diagrams for the paths that matter

### Decisions
- [ADR index](adr/README.md) — every significant architectural decision, with the alternatives that were rejected and why

### Reference
- [API reference](api/README.md) — endpoints, the error envelope, and status mapping
- [API versioning policy](api/versioning.md) — what may change within a version, and what forces a new one
- [Database schema](database/schema.md) — tables, migration mechanics, and conventions

### Operations
- [Configuration](operations/configuration.md) — every setting, its default, and its environment variable
- [Deployment](operations/deployment.md) — container, systemd, and orchestrator deployment
- [Troubleshooting](operations/troubleshooting.md) — symptoms, causes, and fixes
- [Runbooks](operations/runbooks/README.md) — procedures for operating Atlas itself
- [Security](security/security-guide.md) — the threat model and the controls that address it

### Development
- [Developer guide](development/developer-guide.md) — setup, workflow, and common tasks
- [Coding standards](development/coding-standards.md) — the conventions this codebase holds itself to
- [Testing strategy](development/testing.md) — what is tested, at which level, and why
- [Plugin development](plugins/plugin-development.md) — adding a new observable technology

### Planning
- [Phase plan](roadmap/phases.md) — the delivery sequence, with the status of each phase

## How this documentation is maintained

Documentation changes in the same commit as the code it describes. A pull
request that changes behaviour without changing the corresponding document is
incomplete, in the same way that one without tests is incomplete.

Two rules keep it from drifting:

1. **Rationale lives in the code, structure lives here.** Why a particular
   function makes a trade-off belongs in a comment beside that function, where
   it will be seen by whoever changes it. These documents describe how the
   parts relate, which is the thing no single file can show.
2. **Decisions are immutable.** An ADR is never edited to reflect a new
   decision. It is superseded by a new ADR that references it, so the
   reasoning behind an earlier choice survives even after the choice changes.
