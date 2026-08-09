# Data Flow and Sequence Diagrams

The paths that matter, in the order a reader is likely to need them.

## Startup

Components start in registration order; a failure rolls back everything already
started, so the process never lingers half-initialised.

```mermaid
sequenceDiagram
    participant Main as cmd/atlas-server
    participant Cfg as config
    participant App as app (composition root)
    participant Sup as lifecycle.Supervisor
    participant Bus as event.bus
    participant Pool as postgres.pool
    participant Mig as postgres.migrations
    participant HTTP as http.server

    Main->>Cfg: Load(defaults → YAML → env)
    Cfg->>Cfg: Validate — all violations at once
    alt invalid
        Cfg-->>Main: error naming every offending key
        Main-->>Main: exit 1 (never starts degraded)
    end
    Cfg-->>Main: *Config
    Main->>App: New(cfg, logger)
    Note over App: constructs everything, connects to nothing
    App->>Sup: Register(bus, pool, migrations, server)
    Main->>App: Run(ctx)
    App->>Sup: Run(ctx)

    Sup->>Bus: Start
    Sup->>Pool: Start
    Pool->>Pool: open pool, Ping within connect_timeout
    alt database unreachable
        Pool-->>Sup: error
        Sup->>Bus: Stop (rollback, reverse order)
        Sup-->>Main: exit 1
    end
    Sup->>Mig: Start
    Mig->>Mig: pg_advisory_lock, apply pending, unlock
    Sup->>HTTP: Start
    HTTP->>HTTP: bind listener synchronously
    Note over HTTP: a port conflict is a startup error,<br/>not a later surprise
    Sup-->>Main: blocks until ctx cancelled or a fault
```

## Shutdown

```mermaid
sequenceDiagram
    participant OS
    participant Main as cmd/atlas-server
    participant Sup as lifecycle.Supervisor
    participant HTTP as http.server
    participant Mig as postgres.migrations
    participant Pool as postgres.pool
    participant Bus as event.bus

    OS->>Main: SIGTERM
    Main->>Sup: ctx cancelled
    Note over Sup: shutdown ctx uses context.WithoutCancel,<br/>so every Stop gets a live deadline
    Sup->>HTTP: Stop — drain in-flight requests
    HTTP-->>Sup: drained
    Sup->>Mig: Stop
    Sup->>Pool: Stop — wait for checked-out connections
    Sup->>Bus: Stop — close every subscription
    Sup-->>Main: exit 0

    Note over Sup: reverse order matters: the listener stops<br/>accepting before the pool it uses closes
```

Bounded by `server.shutdown_timeout`. A component still running when the budget
expires is abandoned and the shutdown is reported as unclean rather than
silently truncated.

## Serving a request

```mermaid
sequenceDiagram
    participant C as Client
    participant R as Recoverer
    participant ID as RequestID
    participant L as AccessLog
    participant F as JSONErrorFallback
    participant M as ServeMux
    participant H as Handler
    participant D as Dependency

    C->>R: GET /api/v1/system/health
    R->>ID: (panics below are caught here)
    ID->>ID: validate or generate correlation id
    Note over ID: id goes on the context, the response<br/>header, and every log attr below
    ID->>L: start timer
    L->>F: 
    F->>M: 
    M->>H: matched route
    H->>D: query
    alt success
        D-->>H: data
        H->>C: 200 + JSON
    else failure
        D-->>H: error with errs.Code
        H->>C: status from the code table
        Note over H,C: body carries code, safe message,<br/>details, request_id — never the cause
    end
    L->>L: log method, path, status, bytes, duration
```

## Collection (Phase 1 onward)

```mermaid
sequenceDiagram
    participant S as Scheduler
    participant C as Collector
    participant T as Transport
    participant K as Sink
    participant DB as TimescaleDB
    participant B as Event bus

    loop every interval, jittered
        S->>S: acquire a concurrency slot
        S->>C: Collect(ctx with timeout)
        alt returns samples
            C-->>S: []Sample
            S->>S: validate; drop only invalid samples
            S->>T: Send(Envelope{Origin, Batch})
            T->>K: Receive
            K->>DB: bulk insert
            opt recovering from failure
                S->>B: publish collector.run.recovered
            end
        else error or timeout
            C-->>S: error
            S->>B: publish collector.run.failed
        else panic
            C-->>S: recovered by safeCollect
            S->>B: publish collector.run.panicked
            Note over S: other collectors are unaffected
        end
        S->>S: release the slot
    end
```

Four safety properties are enforced here, each because the opposite has taken
down a monitored host in some agent somewhere: bounded runs, non-overlapping
runs, a concurrency ceiling, and start jitter.

## Events

```mermaid
flowchart LR
    P1["Docker plugin<br/>event stream"]
    P2["systemd plugin"]
    P3["Scheduler<br/>collector health"]
    Bus{{"Event bus<br/>bounded queue per subscriber"}}
    S1["Incident timeline<br/>persists, then reconciles"]
    S2["WebSocket fan-out<br/>to browsers"]
    S3["Alert engine"]

    P1 --> Bus
    P2 --> Bus
    P3 --> Bus
    Bus -->|"matching pattern"| S1
    Bus --> S2
    Bus --> S3
```

If a subscriber's queue is full the event is dropped **for that subscriber
alone**, counted, and a rate-limited warning is logged. Publishers never block.

The reason is the scenario where it matters: a browser tab holding a WebSocket
subscription on a laptop that goes to sleep. If publishing blocked, that
stalled client would back-pressure into the collector scheduler, delay metric
collection, and make Atlas report a healthy host as unresponsive — the
monitoring system becoming the outage. See
[ADR-0008](../adr/0008-lossy-event-bus.md).

Consumers that cannot lose events persist from a durable subscriber and
reconcile on restart. The bus says something changed; the database remains the
source of truth.

## The transport seam

The same collectors and the same storage, with a different distance between.

```mermaid
flowchart TB
    subgraph Now["Today — single node"]
        C1["Collector"] --> S1["Scheduler"] --> T1["InProcess"] --> K1["Sink"] --> D1[("Database")]
    end

    subgraph Later["Tier 4 — fleet"]
        C2["Collector"] --> S2["Scheduler"] --> T2["gRPC over mTLS"]
        T2 -.->|network| K2["Ingest sink"] --> D2[("Database")]
    end

    Now -.->|"swap one implementation<br/>at the composition root"| Later
```

`Origin` — node id, hostname, agent version — is carried on every envelope
today, in single-node deployments where it is constant. That redundancy is
deliberate: the schema, query layer, and UI are multi-node from the first row
written, so Tier 4 adds agents with **no data migration**. See
[ADR-0005](../adr/0005-transport-seam.md).

## Migrations

```mermaid
sequenceDiagram
    participant A as Instance A
    participant B as Instance B
    participant PG as PostgreSQL

    par rolling deploy
        A->>PG: pg_advisory_lock(ATLAS)
        PG-->>A: acquired
    and
        B->>PG: pg_advisory_lock(ATLAS)
        Note over B,PG: blocks
    end

    A->>PG: create ledger if absent
    A->>PG: read applied migrations
    A->>A: verify checksums of applied files
    alt a file changed since it was applied
        A-->>A: fail startup — applied migrations are immutable
    end
    loop each pending migration
        A->>PG: BEGIN; migration SQL; INSERT ledger row; COMMIT
    end
    A->>PG: pg_advisory_unlock

    PG-->>B: acquired
    B->>PG: read applied migrations
    Note over B: nothing pending — clean no-op
    B->>PG: pg_advisory_unlock
```

The unlock runs on a context derived with `context.WithoutCancel`. If the run's
context was cancelled, an unlock on that context would fail and the lock would
linger until the connection was reaped — blocking every other instance in the
meantime.
