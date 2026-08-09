# Documentation debt

Deliberately deferred so the application could be completed first. Each line is
enough to write the real document later without re-deriving the reasoning.

**Rule for this file:** append when you build something undocumented. Delete the
line when the real document exists.

## Phase 1 — outstanding

### `docs/database/schema.md`
- `nodes` table: node_id PK (derived, stable across rename), facts nullable because a node row is created on first sample before the host collector runs, `last_seen_at` drives derived status.
- `metric_samples` hypertable: 7-day chunks · compress after 1 day, `segmentby=(node_id,metric)` — this is what makes denormalised storage affordable · raw retention **30 days**.
- Continuous aggregates `metric_samples_1m` / `_1h`: min+max kept alongside avg because an average hides the spike; 1h averages weighted by sample_count, not avg-of-avgs. Retention 90 days / 2 years.
- No FK from samples to nodes — deliberate: FK would lock the parent row on every COPY.
- `end_offset` on refresh policies leaves the newest bucket alone; materialising a partial bucket publishes a wrong value that is never recomputed.

### `docs/operations/configuration.md`
- New section `node`: `id` (pin identity), `id_file` (persist path).
- New `collection` keys: `max_series_per_collector` (default 1000, negative disables), `series_window` (default 1h).
- Note the `_FILE` collision rule: a declared setting ending in `_file` wins over the secret-file indirection.

### `docs/api/README.md`
- `GET /nodes`, `/nodes/{id}` — status derived from silence vs collection interval (up / stale / down at 3× / 10×).
- `GET /metrics` — `node` required; `range=6h` **or** `from`/`to`; `resolution` auto-selects raw ≤6h, 1m ≤14d, else 1h; `max_points` default 1500, cap 10000; response reports which resolution answered.
- `GET /metrics/latest?node=&within=`, `GET /metrics/names?node=`.
- `GET /collectors` — plugin states, per-collector health, series count vs budget, scheduler + ingest stats.

### ADRs to write
- **0011 — Denormalised metric storage.** Chose one row per sample with metric text + JSONB labels over a Prometheus-style series registry. Registry is more space-efficient but adds a lookup cache with its own invalidation bugs; TimescaleDB `segmentby` compression closes most of the gap. Needs cardinality control (0013) to be safe.
- **0012 — gopsutil behind a Provider interface.** Roadmap says "from /proc", but development is on darwin; hand-rolled /proc parsers would mean nothing works locally during the phase that builds the charts. gopsutil is what Telegraf ships. Interface keeps a Linux-native implementation swappable. Verified on Linux: iowait, steal, virtio devices, memory.cached all present.
- **0013 — Cardinality budget enforced in the scheduler.** Unbounded labels are the most common way a metrics platform dies, usually a one-line plugin bug. Enforced where every sample already passes. Drops *new* series and keeps established ones, so a dashboard does not lose what it is built on. 1h eviction window handles legitimate churn.

### `docs/roadmap/phases.md`
- Mark Phase 1 delivered; note C4 was pulled forward from the readiness review.

### Frontend
- `docs/architecture/frontend-architecture.md`: chart palette is validated (dataviz validator, both modes); slots 1–2 only; never substitute hues without re-running it.

## Amendments owed to `agent-readiness-review.md`
- **H1 supersede:** "batch the liveness writes" → **adopt SWIM-style gossip for liveness**. Gives sub-second failure detection, constant control-plane load regardless of fleet size, survives a control-plane outage, and distinguishes "host is dead" from "host cannot reach the control plane" — which the current design cannot do at all.
- **New (H7): peer relay.** An agent in a DMZ or edge segment with no route to the control plane currently cannot report. Netdata-style parent/child relay. Gossip carries membership only, never metrics.

## Known gaps (not documentation)
- No unit tests for `storage/metric` or the Phase 1 API handlers — covered indirectly by integration tests only.
- Review items C1, C2, C3, H1–H6, M1–M10 remain open and scheduled.

## Phase 2 — outstanding

### `docs/api/README.md`
- `GET/containers`, `/{id}`, `/{id}/logs` (tail, `since`) — inventory, not stored; `not_implemented` when Docker is absent.
- `GET /containers/{id}/logs/follow` — **WebSocket**, not a normal request/response endpoint. Requires a genuine RFC 6455 upgrade (`httpx.IsWebSocketUpgrade`); dispatched through `httpx.StreamMiddleware` instead of `BaseMiddleware`, which is what exempts it from the fixed request timeout. Close codes: 1000 + reason `"closing"` (session-bound reconnect, client should retry silently), 1000 + `"the container's log stream ended"` (real end, do not retry), 1011 (failure). Session capped at `maxFollowDuration` (6h); server pings every 20s.
- `GET /processes`, `/services`, `/cron` — same inventory pattern; `not_implemented` distinguishes "no plugin here" from "found nothing".
- `GET /ports` — listening sockets + cached TLS certificate detail. `GET /mounts` — filesystem inventory, always available (system plugin).

### ADRs to write
- **0014 — WebSocket library: `coder/websocket` over gorilla.** Context-native API fits Atlas's context-propagation convention throughout; gorilla is unmaintained. `CloseRead` is the mechanism that detects client disconnect for a server-push-only stream.
- **0015 — Origin checking for WebSocket is not CORS.** `websocket.AcceptOptions.OriginPatterns` matches on host, not full origin string — different convention from `httpx.CORS`, translated at the call site. Cross-site WebSocket hijacking bypasses the browser's same-origin policy entirely, so this check is load-bearing, not advisory.
- **0016 — TLS certificate probing skips chain verification deliberately.** Atlas reports what a service presents (self-signed and internal-CA certs are the common case for service-to-service traffic), not whether a browser would trust it. `InsecureSkipVerify` is intentional; see `internal/plugin/ports/tls.go`.
- **0017 — Port probing budget and address resolution.** Why watched ports are probed even when this sweep doesn't see them live (so a flapping port doesn't get dropped/re-added from the cache), why `dialAddress` maps wildcard binds to 127.0.0.1, and the `"*"` vs `"0.0.0.0"` platform quirk (macOS/BSD lsof vs Linux /proc) normalised in `normaliseAddress`.

### `docs/operations/configuration.md`
- `plugins.service.watch` (unit names), `plugins.process.top_n` / `inventory_limit`, `plugins.ports.watch` (port numbers) / `max_tls_probes` — all per-plugin YAML sections, **not** settable via env var (opaque `yaml.Node`, file-only).
- `server.allowed_origins` now also governs the WebSocket origin check, not only CORS headers.

### Frontend
- `docs/architecture/frontend-architecture.md`: the WebSocket hook pattern (`useContainerLogFollow`) — plain `useEffect` + native `WebSocket`, not TanStack Query (no single cacheable answer); auto-reconnect on the server's session-bound close reason; auto-scroll that disengages once the operator scrolls up.
- Vite dev proxy needs `ws: true` on the `/api` entry or the follow endpoint never leaves "connecting" in development.

### `docs/roadmap/phases.md`
- Mark Phase 2 delivered: Docker (containers, health, stats, images/networks/volumes, live stats, events), live container logs over WebSocket, processes, services (systemd), cron, ports/listening services/mounted disks/SSL certificate status.
- OpenAPI generation (listed under Phase 2) was **not** done — hand-written TypeScript types are still current and manually kept in sync. Flag as intentionally deferred, not forgotten, when Phase 2 is marked complete.

### Known gaps (not documentation)
- Port→process attribution is best-effort: unprivileged Atlas only resolves the owner of sockets it owns itself on most platforms (documented in the `ports` package doc, not yet in an ops-facing doc).
- No custom per-service health checks beyond generic TLS probing — roadmap's "nginx, Redis, PostgreSQL, MySQL, SSH" service list is covered by the systemd watch list, not by protocol-specific probes.
