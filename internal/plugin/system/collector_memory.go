package system

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
)

// memoryCollector reports physical memory and swap.
//
// Memory and swap are collected together because they are read together: swap
// usage is only interpretable next to memory pressure. A host with 8 GB of
// swap in use and plenty of free memory swapped hours ago and recovered; the
// same 8 GB alongside exhausted memory is a host about to invoke the OOM
// killer.
type memoryCollector struct {
	provider Provider
	rates    *rateTracker
}

func newMemoryCollector(p Provider) *memoryCollector {
	return &memoryCollector{provider: p, rates: newRateTracker()}
}

func (c *memoryCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "system.memory",
		Name:        "Memory",
		Description: "Physical memory and swap usage, with swap activity rates.",
		Timeout:     5 * time.Second,
	}
}

func (c *memoryCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()
	samples := make([]collect.Sample, 0, 12)

	m, err := c.provider.Memory(ctx)
	if err != nil {
		return nil, err
	}

	samples = append(samples,
		gauge("system.memory.total", float64(m.Total), collect.UnitBytes, now),
		gauge("system.memory.used", float64(m.Used), collect.UnitBytes, now),
		gauge("system.memory.free", float64(m.Free), collect.UnitBytes, now),
		// Available, not Free, is the figure that answers "can this host take
		// more work". Linux keeps free memory near zero by design, filling it
		// with page cache it will surrender on demand; alerting on Free
		// produces a permanent false alarm on every healthy Linux host.
		gauge("system.memory.available", float64(m.Available), collect.UnitBytes, now),
		gauge("system.memory.usage", clampPercent(m.UsedPercent), collect.UnitPercent, now),
	)

	// Cached and buffers are Linux concepts; on platforms that do not report
	// them, emitting zeros would draw a flat line implying a measurement was
	// taken. Omitting the series says truthfully that there is no such number.
	if m.Cached > 0 {
		samples = append(samples, gauge("system.memory.cached", float64(m.Cached), collect.UnitBytes, now))
	}
	if m.Buffers > 0 {
		samples = append(samples, gauge("system.memory.buffers", float64(m.Buffers), collect.UnitBytes, now))
	}

	s, err := c.provider.Swap(ctx)
	if err != nil {
		return samples, nil // partial data beats none
	}

	samples = append(samples,
		gauge("system.swap.total", float64(s.Total), collect.UnitBytes, now),
		gauge("system.swap.used", float64(s.Used), collect.UnitBytes, now),
		gauge("system.swap.free", float64(s.Free), collect.UnitBytes, now),
		gauge("system.swap.usage", clampPercent(s.UsedPercent), collect.UnitPercent, now),
	)

	// Swap *activity* is the pressure signal, not swap occupancy. Sustained
	// paging is what makes a host slow; a full but quiet swap file is inert.
	if rate, ok := c.rates.rate("swap.in", float64(s.Sin), now); ok {
		samples = append(samples, gauge("system.swap.in", rate, collect.UnitBytesPerSecond, now))
	}
	if rate, ok := c.rates.rate("swap.out", float64(s.Sout), now); ok {
		samples = append(samples, gauge("system.swap.out", rate, collect.UnitBytesPerSecond, now))
	}

	return samples, nil
}

// gauge builds an unlabelled gauge sample.
func gauge(metric string, value float64, unit collect.Unit, at time.Time) collect.Sample {
	return collect.Sample{
		Metric: metric, Value: value,
		Unit: unit, Kind: collect.KindGauge, Time: at,
	}
}
