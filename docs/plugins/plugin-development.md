# Plugin Development Guide

A plugin teaches Atlas to observe one technology. Adding Kubernetes, Redis, or
RabbitMQ support means writing a plugin — **never modifying the platform
core**. See [ADR-0006](../adr/0006-compiled-in-plugins.md) for why plugins are
compiled in rather than loaded at runtime.

## The contract

```go
type Plugin interface {
    Descriptor() Descriptor
    Detect(ctx context.Context) (bool, error)
    Init(ctx context.Context, env Env) error
    Collectors() []collect.Collector
    Close(ctx context.Context) error
}
```

### Lifecycle

| Stage | Meaning | Notes |
| --- | --- | --- |
| **Registered** | Compiled in and known to exist | |
| **Detected** | Asked whether its subject is present on this host | Cheap, no side effects. Runs for every plugin at startup. |
| **Initialised** | Given dependencies; contributes collectors | Only if detection returned true |
| **Closed** | Releases resources | Only if `Init` succeeded. Must be idempotent. |

### Detection is the important stage

A host without Docker must report **no Docker integration**, not a **broken
Docker integration**. Those look identical without a detect stage, and
confusing them means an operator either chases a phantom fault or ignores a
real one.

`Detect` returning `false` is a normal outcome, not an error. An error means
detection itself could not be completed — the socket exists but could not be
probed. The two produce different statuses:

| Status | Meaning |
| --- | --- |
| `active` | Detected and initialised |
| `not_detected` | Subject absent from this host. **Expected, not a fault.** |
| `detection_failed` | Probing errored — investigate |
| `init_failed` | Detected but could not start — investigate |
| `disabled` | Turned off by configuration |

A failure in any one plugin never stops the others. Atlas runs on heterogeneous
hosts, and a machine without Redis must still report its CPU.

## Writing a plugin

Create `internal/plugin/<name>/`.

```go
// Package redis observes a Redis server.
package redis

import (
    "context"
    "net"
    "time"

    "github.com/hexane/atlas/internal/core/collect"
    "github.com/hexane/atlas/internal/core/plugin"
    "github.com/hexane/atlas/internal/platform/errs"
)

type Plugin struct {
    addr   string
    env    plugin.Env
    client *client // whatever the plugin needs
}

func New(addr string) *Plugin { return &Plugin{addr: addr} }

func (p *Plugin) Descriptor() plugin.Descriptor {
    return plugin.Descriptor{
        ID:          "redis",
        Name:        "Redis",
        Description: "Observes Redis memory, clients, and keyspace.",
        Subject:     "redis",
    }
}

// Detect reports whether a Redis server answers at the configured address.
//
// A short timeout is used deliberately: detection runs at startup for every
// registered plugin, so a slow probe delays Atlas becoming ready.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
    dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
    defer cancel()

    conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", p.addr)
    if err != nil {
        // Nothing listening is "absent", not "broken".
        return false, nil
    }
    defer conn.Close()
    return true, nil
}

func (p *Plugin) Init(ctx context.Context, env plugin.Env) error {
    p.env = env

    c, err := connect(ctx, p.addr)
    if err != nil {
        return errs.Wrap(err, errs.CodeUnavailable, "could not connect to Redis").
            WithOp("redis.Plugin.Init")
    }
    p.client = c
    return nil
}

func (p *Plugin) Collectors() []collect.Collector {
    return []collect.Collector{&memoryCollector{client: p.client}}
}

func (p *Plugin) Close(ctx context.Context) error {
    if p.client == nil {
        return nil // Init never succeeded, or Close already ran
    }
    err := p.client.Close()
    p.client = nil
    return err
}
```

## Writing a collector

```go
type memoryCollector struct{ client *client }

func (c *memoryCollector) Descriptor() collect.Descriptor {
    return collect.Descriptor{
        ID:          "redis.memory",
        Name:        "Redis memory",
        Description: "Memory used by the Redis server.",
        Interval:    15 * time.Second, // zero means use the configured default
        Timeout:     5 * time.Second,
    }
}

func (c *memoryCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
    info, err := c.client.Info(ctx) // must honour ctx
    if err != nil {
        return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read Redis INFO").
            WithOp("redis.memoryCollector.Collect")
    }

    now := time.Now()
    return []collect.Sample{{
        Metric: "redis.memory.used",
        Value:  float64(info.UsedMemory),
        Unit:   collect.UnitBytes,
        Kind:   collect.KindGauge,
        Time:   now,
    }}, nil
}
```

### Rules for collectors

1. **Honour the context.** A collector that ignores cancellation can pin a
   scheduler slot on a host with a wedged filesystem or an unresponsive daemon
   — precisely when an operator most needs monitoring to keep working.
2. **Set `Unit` and `Kind` on every sample.** Kind determines valid
   aggregation: averaging a counter is meaningless, and summing a gauge across
   hosts is usually wrong. Recording it lets the query layer refuse those
   mistakes rather than produce a plausible wrong number.
3. **Convert durations to seconds and sizes to bytes.** Storage and display
   never have to guess a scale.
4. **Keep label cardinality bounded.** Every distinct label combination is a
   distinct series. Never label with a PID, a request id, or a timestamp.
5. **Clone labels you keep between runs.** Samples travel asynchronously; a
   mutation on the next run would rewrite history already handed off. Use
   `collect.CloneLabels`.
6. **Be safe for concurrent use.** The scheduler may start a run while a
   previous one finishes.
7. **Return partial results rather than nothing.** If three of four metrics
   read cleanly, return those three. The scheduler validates each sample and
   drops only the invalid ones.

The scheduler provides the safety net around all of this: per-run timeouts,
non-overlapping runs, a concurrency ceiling, start jitter, and panic isolation.
A panic in one collector never stops the others — but do not rely on it.

## Publishing events

Plugins with a push-based source publish directly to the event bus:

```go
func (p *Plugin) watchEvents(ctx context.Context) {
    for event := range p.client.Events(ctx) {
        p.env.Bus.Publish(ctx, eventbus.Event{
            Topic:   "redis.keyspace.evicted",
            Source:  "plugin.redis",
            Subject: event.Key,
            Payload: event,
        })
    }
}
```

Topics are `<plugin>.<resource>.<action>`, ordered general to specific so a
subscriber can match `redis.**` without enumerating resources that do not exist
yet.

**Publishing never blocks and events may be dropped** under back-pressure. If a
consumer cannot tolerate loss, it must persist and reconcile rather than rely
on the bus. See [ADR-0008](../adr/0008-lossy-event-bus.md).

## What a plugin may not do

`plugin.Env` carries a logger, the event bus, and the node id. It contains **no
database handle**, and that omission is deliberate: plugins observe and
publish; they do not write to storage. Samples reach storage through the
scheduler and the transport seam.

And the standing constraint: **a plugin must never modify what it observes.**
No restarts, no reloads, no writes. Read-only is the property that makes Atlas
safe to run on production hosts.

## Testing

A plugin is ordinary Go and should be testable without its subject present:

```go
func TestDetectReturnsFalseWhenNothingListens(t *testing.T) {
    p := redis.New("127.0.0.1:1") // nothing listens here

    detected, err := p.Detect(context.Background())
    if err != nil {
        t.Errorf("Detect() error = %v; an absent subject is not an error", err)
    }
    if detected {
        t.Error("Detect() = true with nothing listening")
    }
}
```

Test collectors by calling `Collect` directly and asserting on the samples —
no scheduler, no database, no HTTP server. Tests needing the real technology go
behind the `integration` build tag with a service in `docker-compose.yml`. See
the [testing strategy](../development/testing.md).

## Registering

Add one line to the composition root:

```go
plugins.Register(redis.New(cfg.Redis.Addr))
```

An operator can disable it without a rebuild by adding its ID to the disabled
list, which is the supported way to stop Atlas touching a subsystem — safer
than uninstalling, and reversible.

## Checklist

- [ ] `Descriptor` returns a stable ID in `<technology>` form and is valid
      before `Init`.
- [ ] `Detect` is cheap, side-effect free, and returns `(false, nil)` when the
      subject is absent.
- [ ] `Init` runs only after successful detection and wraps failures with an
      `errs` code.
- [ ] `Close` is idempotent and safe when `Init` never ran.
- [ ] Collector IDs are `<plugin>.<subject>` and globally unique.
- [ ] Every sample has `Unit`, `Kind`, and `Time`.
- [ ] Every I/O call honours the context.
- [ ] Label cardinality is bounded.
- [ ] Nothing modifies the observed system.
- [ ] Tests cover detection when absent, detection when present, and collection.
- [ ] Metrics are documented.
