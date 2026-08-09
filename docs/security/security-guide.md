# Security Guide

## Threat model

Atlas is unusual: it is read-only, and yet the data it reads is among the most
sensitive an organisation holds. A complete inventory of hosts, versions,
listening ports, running processes, container images, and service dependencies
is a reconnaissance map. **The primary risk is disclosure, not modification.**

| Asset | Risk if disclosed |
| --- | --- |
| Host inventory, OS and kernel versions | Identifies unpatched systems to target |
| Open ports and listening services | Attack surface enumeration |
| Process and binary lists | Reveals installed tooling and versions |
| Container images and tags | Maps deployed software to known CVEs |
| Service dependency graph | Shows blast radius and which systems to pivot to |
| Database credentials in Atlas's own configuration | Direct access to everything Atlas has stored |

The corresponding assurance: **Atlas has no code path that modifies an observed
system.** No container restart, no service reload, no configuration write.

## Current status

**Phase 0 has no authentication.** Any client that can reach the port can read
every endpoint. Authentication and RBAC are Tier 4 items; the
`unauthenticated` and `permission_denied` error codes are already reserved.

**Until then, Atlas must be protected at the network layer.** The default bind
address is `127.0.0.1` specifically so that exposing it is a deliberate act.
Acceptable Phase 0 deployments:

- Loopback only, reached over an SSH tunnel.
- Behind an authenticating reverse proxy (OAuth2 Proxy, an identity-aware
  proxy, or mTLS at the edge).
- On a private network segment with firewall rules restricting access.

**Not acceptable:** binding to `0.0.0.0` on an internet-reachable host.

## Controls implemented in Phase 0

### Information disclosure

The most likely way an observability platform leaks is through its own error
messages. Atlas addresses this in the error type rather than in each handler,
so it cannot be forgotten:

- `errs.Message()` returns a fixed generic string for internal or unclassified
  errors — never the underlying text, which may carry connection strings, host
  names, or query fragments.
- `errs.Details()` returns nothing for internal errors.
- The same accessors back the health report, so a failing dependency check
  cannot leak a driver error either.

Asserted by tests at three levels: the error package, the HTTP layer, and the
health package each verify that a wrapped credential-bearing error surfaces
only the generic message. See [ADR-0009](../adr/0009-typed-error-kernel.md).

### Credentials in logs

Log attributes whose keys look like credentials — matching `password`,
`secret`, `token`, `credential`, `apikey`, `api_key`, `authorization`,
`private_key`, `session_id`, `cookie`, and similar, as substrings — have their
values replaced with `[REDACTED]` before reaching the writer.

Substring matching is deliberate: it catches `db_password`, `PGPASSWORD`, and
`agent_token` without maintaining an exhaustive list.

The `config.Database` type additionally implements `slog.LogValuer`, so logging
the whole struct omits the password too — key matching cannot see inside a
struct logged as a single value.

The database DSN has two forms: `DSN()` carries the password and goes only to
the pool constructor; `SafeDSN()` is used everywhere a human or a log line will
see it.

### Secret handling

- `database.password` **cannot be set from YAML.** The field is excluded from
  deserialisation, so a credential cannot be committed to a configuration
  repository even by mistake. A YAML file attempting it fails startup.
- Any string setting can be sourced from a file via a `_FILE`-suffixed
  environment variable. This is the preferred mechanism, because orchestrators
  mount secrets as files precisely so they never appear in a process's
  environment, where anything able to read `/proc` could retrieve them.
- `atlas-server config` never prints the password. Tested, because that command
  runs inside production containers and would otherwise be a way to read a
  secret out of a running deployment.

### Transport security

Production configuration requires `database.ssl_mode` to be `verify-ca` or
`verify-full`. `require` is rejected: it is widely and wrongly believed to be
safe, but it accepts any certificate, so it stops passive sniffing and not an
active machine-in-the-middle.

These rules are enforced by configuration validation rather than left to a
deployment checklist, because a checklist cannot fail a rollout.

### HTTP hardening

| Control | Effect |
| --- | --- |
| `X-Content-Type-Options: nosniff` | Prevents content-type confusion |
| `X-Frame-Options: DENY` | Infrastructure data is never framed |
| `Referrer-Policy: no-referrer` | URLs are not leaked to third parties |
| `Cross-Origin-Opener-Policy: same-origin` | Isolates the browsing context |
| `Cache-Control: no-store` | Responses are not retained by shared caches |
| `MaxBytesReader` | Request bodies capped at 1 MiB by default |
| `ReadHeaderTimeout` | Bounds Slowloris-style connection exhaustion |
| Panic recovery | A panic returns 500 rather than terminating the process |

**CORS** echoes only exact allow-listed origins. A wildcard is rejected outright
in production, since it would let any site's JavaScript read the infrastructure
inventory. A request from a non-listed origin receives a normal response with no
CORS headers — the browser enforces the policy, and returning an error would
break non-browser clients such as `curl` and monitoring probes.

**Request ids** accepted from clients are validated: alphanumeric plus `-`, `_`,
and `.`, at most 128 characters. Anything else is replaced with a generated id.
This value reaches log files and JSON responses, and an unvalidated string there
is a log-injection vector.

**`X-Forwarded-For` is deliberately ignored** when recording the client address.
It is client-controlled and trustworthy only when a known proxy overwrites it;
honouring it unconditionally would let any caller forge the address in audit
logs. Proxy-aware resolution arrives with authentication, where the trusted-proxy
list it requires also belongs.

### Container image

The runtime image is `scratch`: it contains the static binary, CA certificates,
timezone data, and a `passwd` entry. No shell, no package manager, no
interpreter. An attacker reaching the container finds nothing to pivot with.
The process runs as an unprivileged user.

### Supply chain

- Dependencies are minimal: `pgx` and `yaml.v3` in production.
- `go.sum` pins every module hash; CI fails if `go.mod` is not tidy.
- Builds use `-trimpath`; version, commit, and build time are stamped at link
  time, and `dirty` reports whether the tree had uncommitted changes.

## Deployment checklist

Before exposing Atlas beyond loopback:

- [ ] `environment: production` is set, so hardening validation is active.
- [ ] `database.ssl_mode` is `verify-ca` or `verify-full`.
- [ ] The database password comes from a mounted file, not the environment.
- [ ] `allowed_origins` lists exact HTTPS origins; no wildcard.
- [ ] An authenticating reverse proxy sits in front, or access is restricted by
      firewall to an administrative network.
- [ ] TLS terminates at the proxy with a valid certificate.
- [ ] The database user has only the privileges Atlas needs.
- [ ] Logs ship to an aggregator with restricted access — they contain host
      names, paths, and inventory data.
- [ ] Backups are encrypted at rest and restoration has been tested.

## Database privileges

Atlas needs `CONNECT`, `USAGE` on its schema, `SELECT`/`INSERT`/`UPDATE`/
`DELETE` on its tables, and `CREATE` for migrations. It does **not** need
superuser, except once to install the extensions in migration `0001`. Install
those separately as a superuser and run Atlas with a restricted role:

```sql
-- as superuser, once
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE ROLE atlas LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE atlas TO atlas;
GRANT USAGE, CREATE ON SCHEMA public TO atlas;
```

Migration `0001` uses `CREATE EXTENSION IF NOT EXISTS`, so it succeeds as a
no-op once the extensions are present.

## Reporting a vulnerability

Report privately to the platform team rather than in a public issue. Include
the version from `/api/v1/system/info` and a `request_id` if one is relevant.

## Planned controls

| Control | Phase |
| --- | --- |
| Authentication (OIDC and API tokens) | Tier 4 |
| RBAC with per-resource authorisation | Tier 4 |
| Audit logging of reads | Tier 4 |
| mTLS between agent and control plane | Tier 4 |
| Trusted-proxy-aware client address resolution | With authentication |
| Rate limiting per identity | With authentication |
