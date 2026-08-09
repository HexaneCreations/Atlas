package system

import (
	"context"
	"log/slog"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
)

// diskCollector reports filesystem capacity.
//
// Capacity moves slowly, so this runs on a much longer interval than CPU or
// memory. Polling every fifteen seconds would multiply a `statfs` call across
// every mount point for a number that changes over hours — and on a host with
// a stalled network mount, `statfs` is exactly the call that blocks.
type diskCollector struct {
	provider Provider
	logger   *slog.Logger
}

func newDiskCollector(p Provider, logger *slog.Logger) *diskCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &diskCollector{provider: p, logger: logger}
}

func (c *diskCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "system.disk",
		Name:        "Disk capacity",
		Description: "Filesystem space and inode usage per mount point.",
		Interval:    60 * time.Second,
		Timeout:     30 * time.Second,
	}
}

func (c *diskCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	partitions, err := c.provider.Partitions(ctx)
	if err != nil {
		return nil, err
	}

	samples := make([]collect.Sample, 0, len(partitions)*5)
	for _, p := range partitions {
		// Honour cancellation between mount points. On a host with a hung NFS
		// mount, one statfs can consume the entire run budget, and the
		// filesystems already read are worth returning.
		if ctx.Err() != nil {
			break
		}

		usage, err := c.provider.DiskUsage(ctx, p.Mountpoint)
		if err != nil {
			// A single unreadable mount — a permission-restricted path, a
			// device removed between listing and reading — must not discard
			// every other filesystem on the host.
			c.logger.DebugContext(ctx, "skipping unreadable filesystem",
				slog.String("mountpoint", p.Mountpoint), slog.Any("error", err))
			continue
		}
		if usage.Total == 0 {
			continue // nothing meaningful to report about a zero-capacity mount
		}

		labels := map[string]string{
			"mountpoint": p.Mountpoint,
			"device":     p.Device,
			"fstype":     p.Fstype,
		}

		samples = append(samples,
			labelled("system.disk.total", float64(usage.Total), collect.UnitBytes, now, labels),
			labelled("system.disk.used", float64(usage.Used), collect.UnitBytes, now, labels),
			labelled("system.disk.free", float64(usage.Free), collect.UnitBytes, now, labels),
			labelled("system.disk.usage", clampPercent(usage.UsedPercent), collect.UnitPercent, now, labels),
		)

		// Inode exhaustion produces "no space left on device" on a filesystem
		// reporting plenty of free bytes. Without this series the symptom is
		// unexplainable from the dashboard.
		if usage.InodesTotal > 0 {
			samples = append(samples,
				labelled("system.disk.inodes.usage", clampPercent(usage.InodesUsedPercent), collect.UnitPercent, now, labels),
				labelled("system.disk.inodes.total", float64(usage.InodesTotal), collect.UnitCount, now, labels),
			)
		}
	}

	return samples, nil
}

// diskIOCollector reports block device throughput and utilisation.
type diskIOCollector struct {
	provider Provider
	rates    *rateTracker
}

func newDiskIOCollector(p Provider) *diskIOCollector {
	return &diskIOCollector{provider: p, rates: newRateTracker()}
}

func (c *diskIOCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "system.diskio",
		Name:        "Disk I/O",
		Description: "Per-device read and write throughput, operations, and utilisation.",
		Timeout:     10 * time.Second,
	}
}

func (c *diskIOCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	devices, err := c.provider.DiskIO(ctx)
	if err != nil {
		return nil, err
	}

	samples := make([]collect.Sample, 0, len(devices)*5)
	seen := make(map[string]struct{}, len(devices)*5)

	for _, d := range devices {
		labels := map[string]string{"device": d.Device}

		counters := []struct {
			key    string
			metric string
			value  float64
			unit   collect.Unit
		}{
			{"read.bytes", "system.diskio.read.bytes", float64(d.ReadBytes), collect.UnitBytesPerSecond},
			{"write.bytes", "system.diskio.write.bytes", float64(d.WriteBytes), collect.UnitBytesPerSecond},
			{"read.ops", "system.diskio.read.operations", float64(d.ReadCount), collect.UnitOperationsPerSecond},
			{"write.ops", "system.diskio.write.operations", float64(d.WriteCount), collect.UnitOperationsPerSecond},
		}

		for _, c2 := range counters {
			key := d.Device + ":" + c2.key
			seen[key] = struct{}{}
			if rate, ok := c.rates.rate(key, c2.value, now); ok {
				samples = append(samples, labelled(c2.metric, rate, c2.unit, now, labels))
			}
		}

		// IoTime counts milliseconds with I/O in flight, so its rate is the
		// fraction of wall-clock time the device was busy — utilisation.
		// It saturates before throughput does, which makes it the earliest
		// warning that storage is becoming the bottleneck.
		key := d.Device + ":io.time"
		seen[key] = struct{}{}
		if rate, ok := c.rates.rate(key, float64(d.IoTime), now); ok {
			samples = append(samples,
				labelled("system.diskio.utilisation", clampPercent(rate/10), collect.UnitPercent, now, labels))
		}
	}

	// Drop history for devices that no longer exist, so a host that churns
	// through loop or device-mapper devices does not grow this map forever.
	c.rates.retain(seen)

	return samples, nil
}

// labelled builds a labelled gauge sample.
//
// Labels are cloned because samples travel asynchronously through the
// pipeline: reusing the caller's map would let the next iteration rewrite the
// labels of a sample already handed off.
func labelled(metric string, value float64, unit collect.Unit, at time.Time, labels map[string]string) collect.Sample {
	return collect.Sample{
		Metric: metric, Value: value,
		Unit: unit, Kind: collect.KindGauge, Time: at,
		Labels: collect.CloneLabels(labels),
	}
}
