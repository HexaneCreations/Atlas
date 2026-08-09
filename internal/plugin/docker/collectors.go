package docker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
)

// labelsForMetrics returns the labels a container's series carry.
//
// This is deliberately a short, fixed list rather than the container's own
// labels. A container's label set is arbitrary — Kubernetes alone attaches a
// dozen, some containing pod UIDs — and copying them onto every sample is the
// textbook route to unbounded cardinality. The full label set is still
// available on the container inventory; it just does not become a series
// dimension.
//
// The container *name* is included and the id is not: names are stable across
// a restart, ids are not, and charting by id would break a dashboard every
// time a container was recreated.
func labelsForMetrics(c Container) map[string]string {
	labels := map[string]string{"container": c.Name}
	if project := c.ComposeProject(); project != "" {
		labels["compose_project"] = project
	}
	if service := c.ComposeService(); service != "" {
		labels["compose_service"] = service
	}
	return labels
}

// containerCollector reports container inventory, lifecycle state, and health.
type containerCollector struct {
	client Client
	logger *slog.Logger
}

func newContainerCollector(c Client, logger *slog.Logger) *containerCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &containerCollector{client: c, logger: logger}
}

func (c *containerCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "docker.containers",
		Name:        "Containers",
		Description: "Container counts by state, health, restarts, and uptime.",
		// Listing containers costs one inspect call each, so this runs less
		// often than the stats collector. Lifecycle changes arrive on the
		// event stream in near real time anyway; this is the periodic
		// reconciliation, not the primary signal.
		Interval: 30 * time.Second,
		Timeout:  25 * time.Second,
	}
}

func (c *containerCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	containers, err := c.client.Containers(ctx)
	if err != nil {
		return nil, err
	}

	samples := make([]collect.Sample, 0, len(containers)*4+8)

	byState := map[State]int{}
	byHealth := map[Health]int{}

	for _, ct := range containers {
		byState[ct.State]++
		byHealth[ct.Health]++

		labels := labelsForMetrics(ct)

		// A per-container "up" gauge is what lets a dashboard show a specific
		// container's history rather than only an aggregate count. One series
		// per container is bounded by the number of containers, which is the
		// kind of cardinality that is fine.
		up := 0.0
		if ct.State.Running() {
			up = 1
		}
		samples = append(samples,
			labelled("docker.container.up", up, collect.UnitCount, now, labels),
			// Restarts are the signal a snapshot cannot show: a container that
			// is running now but has restarted forty times is not healthy.
			labelled("docker.container.restarts", float64(ct.RestartCount), collect.UnitCount, now, labels),
		)

		if ct.State.Running() && !ct.StartedAt.IsZero() {
			samples = append(samples,
				labelled("docker.container.uptime", now.Sub(ct.StartedAt).Seconds(), collect.UnitSeconds, now, labels))
		}

		// Health is only meaningful where the image declares a check.
		// Reporting "0" for the majority of images that declare none would
		// make an unhealthy-container alert fire for every container on the
		// host.
		if ct.Health != HealthNone && ct.Health != "" {
			healthy := 0.0
			if ct.Health == HealthHealthy {
				healthy = 1
			}
			withStatus := collect.CloneLabels(labels)
			withStatus["health"] = string(ct.Health)
			samples = append(samples,
				labelled("docker.container.healthy", healthy, collect.UnitCount, now, withStatus))
		}

		// A non-zero exit code on a stopped container is why it stopped.
		if !ct.State.Running() && ct.ExitCode != 0 {
			samples = append(samples,
				labelled("docker.container.exit_code", float64(ct.ExitCode), collect.UnitCount, now, labels))
		}
	}

	// Aggregates. Every state is emitted, including zeros, so a chart shows a
	// state falling to zero rather than the series simply disappearing.
	for _, state := range []State{
		StateRunning, StateExited, StatePaused, StateRestarting, StateCreated, StateDead,
	} {
		samples = append(samples, labelled("docker.containers.count",
			float64(byState[state]), collect.UnitCount, now, map[string]string{"state": string(state)}))
	}
	for _, health := range []Health{HealthHealthy, HealthUnhealthy, HealthStarting} {
		samples = append(samples, labelled("docker.containers.health",
			float64(byHealth[health]), collect.UnitCount, now, map[string]string{"health": string(health)}))
	}
	samples = append(samples, gauge("docker.containers.total", float64(len(containers)), collect.UnitCount, now))

	return samples, nil
}

// statsCollector reports per-container resource use.
//
// Split from the inventory collector because the two have different costs and
// different useful cadences: stats matter at fifteen-second resolution, while
// the container list changes rarely and costs an inspect per container.
type statsCollector struct {
	client Client
	logger *slog.Logger
	rates  *rateTracker
	// concurrency bounds simultaneous stats calls. A host with two hundred
	// containers would otherwise open two hundred connections to the daemon at
	// once, which is a denial of service against the thing being monitored.
	concurrency int
}

func newStatsCollector(c Client, logger *slog.Logger, concurrency int) *statsCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if concurrency <= 0 {
		concurrency = 8
	}
	return &statsCollector{client: c, logger: logger, rates: newRateTracker(), concurrency: concurrency}
}

func (c *statsCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "docker.stats",
		Name:        "Container resources",
		Description: "Per-container CPU, memory, network, and block I/O.",
		Timeout:     20 * time.Second,
	}
}

func (c *statsCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	containers, err := c.client.Containers(ctx)
	if err != nil {
		return nil, err
	}

	running := make([]Container, 0, len(containers))
	for _, ct := range containers {
		if ct.State.Running() {
			running = append(running, ct)
		}
	}
	if len(running) == 0 {
		return nil, nil
	}

	var (
		mu      sync.Mutex
		samples = make([]collect.Sample, 0, len(running)*8)
		seen    = make(map[string]struct{}, len(running)*4)
		wg      sync.WaitGroup
		slots   = make(chan struct{}, c.concurrency)
	)

	for _, ct := range running {
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}

			stats, err := c.client.ContainerStats(ctx, ct.ID)
			if err != nil {
				// A container that stopped between the list and the stats read
				// is routine on a busy host. One unreadable container must not
				// cost the run.
				c.logger.DebugContext(ctx, "could not read container stats",
					slog.String("container", ct.Name), slog.Any("error", err))
				return
			}

			produced, keys := c.samplesFor(ct, stats, now)

			mu.Lock()
			samples = append(samples, produced...)
			for _, k := range keys {
				seen[k] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Containers are created and destroyed constantly. Retaining only what was
	// just observed keeps the rate tracker bounded by the containers that
	// exist rather than every container ever seen.
	c.rates.retain(seen)

	return samples, nil
}

func (c *statsCollector) samplesFor(ct Container, s Stats, now time.Time) ([]collect.Sample, []string) {
	labels := labelsForMetrics(ct)

	out := []collect.Sample{
		labelled("docker.container.cpu.usage", clampPercent(s.CPUPercent), collect.UnitPercent, now, labels),
		labelled("docker.container.memory.usage", float64(s.MemoryUsage), collect.UnitBytes, now, labels),
		labelled("docker.container.pids", float64(s.PIDs), collect.UnitCount, now, labels),
	}

	// A memory limit of zero means unlimited, and a percentage against it
	// would be meaningless — or a division by zero.
	if s.MemoryLimit > 0 {
		out = append(out,
			labelled("docker.container.memory.limit", float64(s.MemoryLimit), collect.UnitBytes, now, labels),
			labelled("docker.container.memory.percent", clampPercent(s.MemoryPercent), collect.UnitPercent, now, labels),
		)
	}

	// Network and block figures are cumulative counters; their rate is the
	// signal. Keyed by container id so a recreated container starts a fresh
	// baseline rather than reporting a huge negative delta.
	counters := []struct {
		key, metric string
		value       float64
		unit        collect.Unit
	}{
		{"net.rx", "docker.container.network.rx", float64(s.NetworkRxBytes), collect.UnitBytesPerSecond},
		{"net.tx", "docker.container.network.tx", float64(s.NetworkTxBytes), collect.UnitBytesPerSecond},
		{"blk.read", "docker.container.block.read", float64(s.BlockReadBytes), collect.UnitBytesPerSecond},
		{"blk.write", "docker.container.block.write", float64(s.BlockWriteBytes), collect.UnitBytesPerSecond},
	}

	keys := make([]string, 0, len(counters))
	for _, ctr := range counters {
		key := ct.ID + ":" + ctr.key
		keys = append(keys, key)
		if rate, ok := c.rates.rate(key, ctr.value, now); ok {
			out = append(out, labelled(ctr.metric, rate, ctr.unit, now, labels))
		}
	}
	return out, keys
}

// inventoryCollector reports images, networks, and volumes.
type inventoryCollector struct {
	client Client
	logger *slog.Logger
}

func newInventoryCollector(c Client, logger *slog.Logger) *inventoryCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &inventoryCollector{client: c, logger: logger}
}

func (c *inventoryCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "docker.inventory",
		Name:        "Docker inventory",
		Description: "Image, network, and volume counts and sizes.",
		// Inventory changes on a deploy, not on a fifteen-second cadence, and
		// listing volumes with usage data is expensive on some storage
		// drivers.
		Interval: 5 * time.Minute,
		Timeout:  60 * time.Second,
	}
}

func (c *inventoryCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()
	samples := make([]collect.Sample, 0, 6)

	// Each read is independent: a daemon that cannot list volumes can usually
	// still list images, and returning what succeeded beats returning nothing.
	if images, err := c.client.Images(ctx); err == nil {
		samples = append(samples, gauge("docker.images.count", float64(images.Count), collect.UnitCount, now))
		if images.SizeBytes > 0 {
			samples = append(samples, gauge("docker.images.size", float64(images.SizeBytes), collect.UnitBytes, now))
		}
	} else {
		c.logger.DebugContext(ctx, "could not list images", slog.Any("error", err))
	}

	if networks, err := c.client.Networks(ctx); err == nil {
		samples = append(samples, gauge("docker.networks.count", float64(networks), collect.UnitCount, now))
	} else {
		c.logger.DebugContext(ctx, "could not list networks", slog.Any("error", err))
	}

	if volumes, err := c.client.Volumes(ctx); err == nil {
		samples = append(samples, gauge("docker.volumes.count", float64(volumes.Count), collect.UnitCount, now))
		if volumes.SizeBytes > 0 {
			samples = append(samples, gauge("docker.volumes.size", float64(volumes.SizeBytes), collect.UnitBytes, now))
		}
	} else {
		c.logger.DebugContext(ctx, "could not list volumes", slog.Any("error", err))
	}

	return samples, nil
}

// --- shared helpers -------------------------------------------------------

func gauge(metric string, value float64, unit collect.Unit, at time.Time) collect.Sample {
	return collect.Sample{Metric: metric, Value: value, Unit: unit, Kind: collect.KindGauge, Time: at}
}

func labelled(metric string, value float64, unit collect.Unit, at time.Time, labels map[string]string) collect.Sample {
	return collect.Sample{
		Metric: metric, Value: value, Unit: unit, Kind: collect.KindGauge, Time: at,
		Labels: collect.CloneLabels(labels),
	}
}

func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}
