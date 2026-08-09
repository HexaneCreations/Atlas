# Frontend Architecture

The Atlas console is a React 19 application in TypeScript, built by Vite. It
is an operations tool: it is read during incidents, by people under time
pressure, often over a slow connection into a bastion host. That shapes most of
the decisions below.

## Structure

```
web/
  index.html
  vite.config.ts        dev server, API proxy, build settings
  eslint.config.js      type-aware linting
  tsconfig.app.json     strict compiler options for src/
  src/
    main.tsx            entry point, QueryClient, providers
    App.tsx             shell and panels
    styles.css          design tokens, light and dark
    api/
      types.ts          types mirroring the Go API
      client.ts         the single HTTP client and error model
      queries.ts        TanStack Query definitions and refresh policy
```

## Data fetching

**All HTTP goes through `apiGet` in `api/client.ts`.** One place means error
handling exists once, and because the server returns the same envelope for
every failure — including router-level 404s and 405s — the client turns any
failed response into the same `ApiError`. A caller never has to distinguish
"the endpoint failed" from "the request never reached an endpoint".

```ts
export class ApiError extends Error {
  readonly code: ErrorCode;      // mirrors errs.Code in Go
  readonly status: number;
  readonly requestId: string | undefined;
  get retryable(): boolean { … }  // unavailable | deadline_exceeded | rate_limited
}
```

`retryable` is derived from the code rather than the status, and it drives the
retry policy: retrying a `404` wastes requests and delays the moment the
operator sees the real message, while retrying an `unavailable` dependency is
exactly what should happen while Atlas restarts.

**Untrusted JSON is narrowed, not asserted.** `isApiErrorResponse` is a real
type guard, so a proxy's HTML error page cannot flow through as if it were an
Atlas envelope and fail later somewhere confusing.

**There is no write function.** Atlas is read-only; the API answers any write
with `405`. A client that cannot express one makes the guarantee visible in the
frontend as well as the backend.

## Server state

Server state lives in TanStack Query, never in component state. Refresh
intervals are per-resource, because the data moves at very different rates:

| Query | Interval | Why |
| --- | --- | --- |
| `useSystemInfo` | 60s stale time | Build identity changes only on deploy |
| `useSystemHealth` | 5s, **including in background tabs** | An operator who tabs back during an incident needs current state, not a stale snapshot followed by a visible refresh |
| `useSystemRuntime` | 5s | Live resource figures |

The `QueryClient` is created **at module scope**. Creating it inside a
component would discard the entire cache on every re-render of that component,
turning a dashboard into a request storm against the system it monitors.

## The three-state rule

**Every data-driven view distinguishes loading, error, and empty.**

```tsx
{query.isPending      ? <p>Loading…</p>
 : query.error        ? <p className="…--error">{query.error.message}</p>
 : query.data         ? children(query.data)
 :                      <p>No data.</p>}
```

All three render as an identical blank box if this is not done deliberately —
and in an operations tool, a failed query that looks empty reads as *the
absence of a problem* rather than the absence of data. This is the single
convention every later view must follow.

## Type safety

`strict`, `noUncheckedIndexedAccess`, and `exactOptionalPropertyTypes` are all
enabled, with type-aware ESLint rules on top. `no-floating-promises` is an
error, because an unhandled rejection in a data-fetching UI shows stale data
with no indication that the refresh failed.

API types in `api/types.ts` are written by hand and mirror the Go structs. At
three endpoints, a code generator is more machinery than the surface justifies.
That trade flips once drift becomes likely; the answer then is generation from
an OpenAPI document, planned for Phase 2 when the API surface grows.

## Styling

Plain CSS with custom properties. No framework, no CSS-in-JS runtime.

Both colour schemes are defined from the start via `prefers-color-scheme`.
Operations tools are read at 3am as often as at 3pm, and retrofitting a dark
theme onto hard-coded colours is a far larger job than defining tokens once.

Two details that matter for this kind of interface:

- **Tabular numerals** on every changing value, so figures do not jitter
  horizontally as panels refresh every few seconds.
- **Status is never colour alone.** Every status dot carries an accessible
  label, because colour alone excludes anyone with a colour vision deficiency
  from the most important signal on the page.

## Development and production

In development, Vite proxies `/api`, `/healthz`, and `/readyz` to
`127.0.0.1:8080`. This keeps the browser on a single origin, so development
exercises the same same-origin path as production and never needs CORS relaxed
just to make local work — a relaxation that has a habit of surviving into
deployment.

In production the built bundle is served by Atlas itself from the same origin,
so no CORS configuration is needed at all.

Source maps are enabled in production builds: diagnosing a console error during
an incident matters more than hiding the source of an internal tool.

## Planned

| Item | Phase |
| --- | --- |
| Routing (`react-router` is already a dependency) | 1 |
| Time-series charts | 1 |
| Component tests (Vitest, React Testing Library) | 1 |
| WebSocket subscription for live updates | 2 |
| Virtualised tables for large container and process lists | 2 |
| End-to-end tests (Playwright) | 1–2 |
| Generated API types from OpenAPI | 2 |
