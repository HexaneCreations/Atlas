# Deployment Guide

## Requirements

| Component | Requirement |
| --- | --- |
| Atlas | One static binary. No runtime dependency. |
| Database | PostgreSQL 15+ **with the TimescaleDB extension** |
| Reverse proxy | Required until authentication ships — see [security](../security/security-guide.md) |

**TimescaleDB is a hard requirement**, not an optimisation. Migration `0001`
installs it, and a database without the extension available fails to migrate. A
managed PostgreSQL service that does not offer TimescaleDB cannot host Atlas.
See [ADR-0003](../adr/0003-postgresql-timescaledb.md).

## Container image

The runtime image is `scratch`: the static binary, CA certificates, timezone
data, and a `passwd` entry. No shell, no package manager, no interpreter.

```bash
make image      # or:
docker build \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg BUILDTIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -t atlas:1.0.0 .
```

Because there is no shell, use the HTTP probes for health rather than an
`exec`-based check.

## Deploying

### Docker

```bash
docker run -d --name atlas \
  -p 127.0.0.1:8080:8080 \
  -e ATLAS_ENVIRONMENT=production \
  -e ATLAS_DATABASE_HOST=postgres.internal \
  -e ATLAS_DATABASE_NAME=atlas \
  -e ATLAS_DATABASE_USER=atlas \
  -e ATLAS_DATABASE_SSL_MODE=verify-full \
  -e ATLAS_DATABASE_PASSWORD_FILE=/run/secrets/atlas-db \
  -v /etc/atlas/db-password:/run/secrets/atlas-db:ro \
  --read-only \
  --cap-drop=ALL \
  --security-opt no-new-privileges \
  atlas:1.0.0
```

The image sets `ATLAS_SERVER_HOST=0.0.0.0` because the container *is* the
network boundary; the loopback default protects a developer laptop, not this.
Publish the port deliberately, as above.

### Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: atlas
spec:
  replicas: 2
  selector:
    matchLabels: { app: atlas }
  template:
    metadata:
      labels: { app: atlas }
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 10001
      containers:
        - name: atlas
          image: atlas:1.0.0
          ports:
            - containerPort: 8080
          env:
            - name: ATLAS_ENVIRONMENT
              value: production
            - name: ATLAS_DATABASE_HOST
              value: postgres.internal
            - name: ATLAS_DATABASE_SSL_MODE
              value: verify-full
            - name: ATLAS_DATABASE_PASSWORD_FILE
              value: /run/secrets/atlas/password
            # Replicas would otherwise race to migrate on startup. The
            # advisory lock makes that safe, but a dedicated Job is clearer
            # and keeps a slow migration from delaying every pod.
            - name: ATLAS_DATABASE_MIGRATE_ON_START
              value: "false"
          volumeMounts:
            - name: db-secret
              mountPath: /run/secrets/atlas
              readOnly: true
          # Liveness checks nothing but the process. Probing dependencies here
          # would turn a database blip into a cascading restart of every pod.
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
            periodSeconds: 10
          readinessProbe:
            httpGet: { path: /readyz, port: 8080 }
            periodSeconds: 5
          startupProbe:
            httpGet: { path: /readyz, port: 8080 }
            failureThreshold: 30
            periodSeconds: 2
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          resources:
            requests: { cpu: 100m, memory: 128Mi }
            limits:   { memory: 512Mi }
      volumes:
        - name: db-secret
          secret: { secretName: atlas-db }
```

With migrations as a separate Job run before the rollout:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: atlas-migrate
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: migrate
          image: atlas:1.0.0
          args: ["migrate"]
          envFrom: [{ secretRef: { name: atlas-db-env } }]
```

### systemd

```ini
[Unit]
Description=Atlas observability platform
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=atlas
Group=atlas
ExecStart=/usr/local/bin/atlas-server --config /etc/atlas/atlas.yaml serve
Restart=on-failure
RestartSec=5s

Environment=ATLAS_DATABASE_PASSWORD_FILE=/etc/atlas/db-password

# Atlas is read-only and needs no write access to the filesystem.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictNamespaces=true
MemoryDenyWriteExecute=true

# SIGTERM begins a graceful drain; allow more than shutdown_timeout.
KillSignal=SIGTERM
TimeoutStopSec=45

[Install]
WantedBy=multi-user.target
```

> From Phase 1, collectors read `/proc`, `/sys`, and the Docker socket. Those
> hardening directives will need selective relaxation, documented alongside the
> collectors that require it.

## Reverse proxy

Until authentication ships, an authenticating proxy is mandatory. Minimal nginx:

```nginx
server {
    listen 443 ssl http2;
    server_name atlas.example.com;

    ssl_certificate     /etc/ssl/atlas.crt;
    ssl_certificate_key /etc/ssl/atlas.key;

    location / {
        auth_request /oauth2/auth;      # or equivalent
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Request-Id      $request_id;

        # Streaming endpoints from Phase 2 need buffering off.
        proxy_buffering off;
        proxy_read_timeout 300s;
    }
}
```

Atlas honours an inbound `X-Request-Id` if it is alphanumeric with `-`, `_`, or
`.` and at most 128 characters, so a trace begun at the proxy survives into
Atlas's logs.

## Graceful shutdown

On `SIGTERM` or `SIGINT`, Atlas stops components in exact reverse order:
`http.server` drains first, then migrations, then the pool, then the event bus.
The whole sequence is bounded by `server.shutdown_timeout` (default 20s).

Give the supervisor more than that — 45s for systemd's `TimeoutStopSec`, or
Kubernetes' `terminationGracePeriodSeconds` — so a drain is never cut short by
the process manager.

A second `SIGINT` kills immediately, which is what an operator who has decided
not to wait expects.

## Upgrades

Because migrations are forward-only and every migration must be
backward-compatible with the previous release
([ADR-0007](../adr/0007-forward-only-migrations.md)), a rolling upgrade is:

1. Run `atlas-server migrate` with the new image.
2. Roll out the new version.
3. If it must be rolled back, deploy the previous version. **Do not attempt to
   reverse the migration** — the previous version is designed to run against
   the newer schema.

## Backups

Because there are no down migrations, restoring from backup is the last resort
for a data-destroying mistake. This makes tested backups a hard requirement.

```bash
pg_dump --format=custom --compress=9 \
        --host=postgres.internal --username=atlas atlas \
        > atlas-$(date -u +%Y%m%dT%H%M%SZ).dump
```

- Encrypt at rest. A dump contains the full infrastructure inventory.
- **Test restoration on a schedule.** An untested backup is a hypothesis.
- TimescaleDB hypertables are dumped by `pg_dump`, but restore into a database
  with the extension already installed.

## Post-deployment verification

```bash
curl -sf https://atlas.example.com/readyz | jq .status          # "healthy"
curl -s  https://atlas.example.com/api/v1/system/info | jq      # expected version, dirty=false
curl -s  https://atlas.example.com/api/v1/system/runtime | jq   # pool and bus healthy
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
     https://atlas.example.com/api/v1/system/info                # 405
```

Then work through the checklist in the
[security guide](../security/security-guide.md).

## What to alert on

| Signal | Condition | Meaning |
| --- | --- | --- |
| `/readyz` | Non-200 for more than a minute | Instance cannot serve |
| `database.empty_acquire_count` | Rising | Pool undersized for load |
| `event_bus.dropped` | Non-zero and rising | A subscriber cannot keep up ([ADR-0008](../adr/0008-lossy-event-bus.md)) |
| `process.goroutines` | Monotonically increasing | Goroutine leak |
| Log `"slow query"` | Frequent | Database contention or a missing index |
| Log `"component fault"` | Any | A component failed at runtime; shutdown followed |
