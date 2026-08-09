
# Atlas — Implementation Context

## Project Overview

Atlas is a production observability platform ("Observe Everything. Control Nothing.") — a single pane of glass for servers, containers, processes, services, fleets, and infrastructure.

Atlas is 100% read-only toward monitored infrastructure.

---

## Core Principles

- Observe Everything. Control Nothing.
- Connect by Identity, Never by Address.
- Truth Before Convenience.

---

## Current Project Status

Backend

✅ Agent architecture
✅ Fleet architecture
✅ Remote inventory
✅ Event Store
✅ Alert Rule Engine
✅ Incident Timeline

Frontend

✅ Core dashboard foundation
✅ Tier-1 monitoring pages

⏳ Advanced observability UI deferred until backend completion.

Networking

✅ HTTPS + mTLS
✅ Fleet enrollment
✅ Spool & replay
✅ Remote inventory

⏳ libp2p deferred.

---

## Current Implementation Roadmap

1. Correlation Engine
2. Health Score
3. Cost Analysis
4. Capacity Planning
5. SLO / Golden Signals
6. libp2p Transport
7. Atlas Relay
8. AI Features

---

## Development Rules

- Code first.
- Backend first.
- Prefer tests over explanations.
- Keep comments minimal.
- No documentation unless requested.
- No ADRs unless requested.
- No roadmap rewrites.
- Continue milestone-by-milestone.
- Stop only for:
  - architecture decisions
  - security concerns
  - blockers
