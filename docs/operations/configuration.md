# Configuration Guide

## How configuration is resolved

Three layers, each overriding the last:

1. **Compiled-in defaults.** Atlas starts with no configuration file and no
   environment at all, in development mode against a local Postgres.
2. **An optional YAML file**, for settings that belong under version control
   and review. Located with `--config <path>` or `ATLAS_CONFIG_FILE`.
3. **Environment variables** prefixed `ATLAS_`, for per-deployment values and
   anything injected by an orchestrator.

Everything is validated once at startup, before any component is constructed.
A misconfigured Atlas fails immediately with a message naming the offending
key — it never starts degraded and discovers the problem on the first request.

**All problems are reported at once**, so bringing up a deployment in a
pipeline does not cost one restart cycle per typo:

```
atlas-server: invalid configuration:
  - database.ssl_mode: "require" does not verify the server certificate; production requires verify-ca or verify-full
  - server.allowed_origins: wildcard "*" is not permitted in production; list the exact origins that may call the API
```

## Inspecting the resolved configuration

```bash
atlas-server config
```

Prints the fully resolved configuration as JSON, with durations in the same
syntax the file accepts (`30s`, not `30000000000`), plus the redacted database
DSN and every recognised environment variable. **The database password is never
included**, so this is safe to run inside a production container.

## Environment variable naming

Names are derived from the configuration structure: section, then key, joined
with underscores beneath `ATLAS_`.

```
logging.level        →  ATLAS_LOGGING_LEVEL
database.max_conns   →  ATLAS_DATABASE_MAX_CONNS
event_bus.buffer_size →  ATLAS_EVENT_BUS_BUFFER_SIZE
```

Because names are derived rather than listed by hand, a new setting cannot be
added without its environment override existing.

## Secrets

**Never put a secret in the YAML file.** The `database.password` field is
excluded from YAML deserialisation entirely, so a configuration file cannot
set it even by mistake.

Supply secrets from a mounted file:

```bash
ATLAS_DATABASE_PASSWORD_FILE=/run/secrets/atlas-db-password
```

Any string setting supports the `_FILE` suffix. The file's contents become the
value, with trailing newlines trimmed. This is the preferred mechanism because
Docker and Kubernetes mount secrets as files precisely so they never appear in
a process's environment, where anything able to read `/proc` could retrieve
them.

`ATLAS_DATABASE_PASSWORD` is supported directly for development.

## Reference

### Top level

| Key | Environment variable | Default | Notes |
| --- | --- | --- | --- |
| `environment` | `ATLAS_ENVIRONMENT` | `development` | `development`, `staging`, or `production`. Production enables stricter validation. |

### `server`

| Key | Environment variable | Default | Notes |
| --- | --- | --- | --- |
| `host` | `ATLAS_SERVER_HOST` | `127.0.0.1` | Loopback by default. Atlas exposes a full infrastructure inventory, so reaching the network must be deliberate. |
| `port` | `ATLAS_SERVER_PORT` | `8080` | `0` requests an ephemeral port from the kernel. |
| `read_header_timeout` | `ATLAS_SERVER_READ_HEADER_TIMEOUT` | `10s` | Bounds Slowloris-style attacks. Must not exceed `read_timeout`. |
| `read_timeout` | `ATLAS_SERVER_READ_TIMEOUT` | `30s` | |
| `write_timeout` | `ATLAS_SERVER_WRITE_TIMEOUT` | `60s` | Streaming endpoints opt out per route. |
| `idle_timeout` | `ATLAS_SERVER_IDLE_TIMEOUT` | `2m` | |
| `shutdown_timeout` | `ATLAS_SERVER_SHUTDOWN_TIMEOUT` | `20s` | Bounds the whole graceful drain. |
| `max_request_bytes` | `ATLAS_SERVER_MAX_REQUEST_BYTES` | `1048576` | 1 MiB. Atlas is read-only; bodies are small. |
| `allowed_origins` | `ATLAS_SERVER_ALLOWED_ORIGINS` | *(empty)* | Comma-separated browser origins. Empty means same-origin only, correct when Atlas serves the built frontend. |

### `database`

| Key | Environment variable | Default | Notes |
| --- | --- | --- | --- |
| `host` | `ATLAS_DATABASE_HOST` | `127.0.0.1` | |
| `port` | `ATLAS_DATABASE_PORT` | `5432` | |
| `name` | `ATLAS_DATABASE_NAME` | `atlas` | |
| `user` | `ATLAS_DATABASE_USER` | `atlas` | |
| *(not in YAML)* | `ATLAS_DATABASE_PASSWORD` / `..._PASSWORD_FILE` | *(empty)* | Required in production. |
| `ssl_mode` | `ATLAS_DATABASE_SSL_MODE` | `prefer` | Production requires `verify-ca` or `verify-full`. |
| `max_conns` | `ATLAS_DATABASE_MAX_CONNS` | `16` | |
| `min_conns` | `ATLAS_DATABASE_MIN_CONNS` | `2` | Must not exceed `max_conns`. |
| `max_conn_lifetime` | `ATLAS_DATABASE_MAX_CONN_LIFETIME` | `1h` | Jittered by 10% to avoid a reconnect stampede. |
| `max_conn_idle_time` | `ATLAS_DATABASE_MAX_CONN_IDLE_TIME` | `30m` | Must not exceed `max_conn_lifetime`. |
| `connect_timeout` | `ATLAS_DATABASE_CONNECT_TIMEOUT` | `10s` | |
| `migrate_on_start` | `ATLAS_DATABASE_MIGRATE_ON_START` | `true` | When `false`, pending migrations make startup fail; run `atlas-server migrate` first. |

### `logging`

| Key | Environment variable | Default | Notes |
| --- | --- | --- | --- |
| `level` | `ATLAS_LOGGING_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `format` | `ATLAS_LOGGING_FORMAT` | `json` | `json` or `text`. Use `json` anywhere logs are aggregated. |
| `add_source` | `ATLAS_LOGGING_ADD_SOURCE` | `false` | Adds file and line; costs a caller lookup per record. |

### `event_bus`

| Key | Environment variable | Default | Notes |
| --- | --- | --- | --- |
| `buffer_size` | `ATLAS_EVENT_BUS_BUFFER_SIZE` | `256` | Queue depth per subscriber. A full queue drops events for that subscriber; it never blocks a publisher. See [ADR-0008](../adr/0008-lossy-event-bus.md). |

### `collection`

Consumed by the collector scheduler, which is composed into the server from
Phase 1.

| Key | Environment variable | Default | Notes |
| --- | --- | --- | --- |
| `default_interval` | `ATLAS_COLLECTION_DEFAULT_INTERVAL` | `15s` | Used by collectors that do not specify their own. |
| `timeout` | `ATLAS_COLLECTION_TIMEOUT` | `10s` | Bounds one run. Must not exceed `default_interval`, or runs would overlap indefinitely. |
| `max_concurrent` | `ATLAS_COLLECTION_MAX_CONCURRENT` | `8` | Caps simultaneous collectors, bounding Atlas's own footprint on a loaded host. |

## Production validation

Setting `environment: production` enables rules that a deployment checklist
cannot enforce, because a checklist cannot fail a rollout:

| Rule | Why |
| --- | --- |
| `database.ssl_mode` must be `verify-ca` or `verify-full` | `require` is widely believed to be safe but accepts any certificate: it stops passive sniffing, not an active machine-in-the-middle. |
| `database.password` must be set | Prevents accidental reliance on trust authentication. |
| `allowed_origins` may not contain `*` | A wildcard would let any site's JavaScript read the infrastructure inventory. |
| `allowed_origins` may not use plaintext `http://` (except localhost) | Prevents downgrade. |

## Example: production YAML

```yaml
# /etc/atlas/atlas.yaml — contains no secrets and is safe to commit.
environment: production

server:
  host: 0.0.0.0        # behind a reverse proxy
  port: 8080
  shutdown_timeout: 30s
  allowed_origins:
    - https://atlas.example.com

database:
  host: postgres.internal
  name: atlas
  user: atlas
  ssl_mode: verify-full
  max_conns: 32
  migrate_on_start: false   # applied by a separate migrate job

logging:
  level: info
  format: json

collection:
  default_interval: 15s
  timeout: 10s
  max_concurrent: 12
```

With the secret supplied separately:

```bash
ATLAS_DATABASE_PASSWORD_FILE=/run/secrets/atlas-db-password
```
