package system

import (
	"context"
	"strconv"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
)

// cpuCollector reports processor utilisation.
//
// It emits an aggregate busy percentage, a per-core breakdown, and the split
// by CPU state. The breakdown matters because one number cannot distinguish
// situations that call for opposite responses: a host at 90% in user time is
// doing work and may need a bigger machine, while a host at 90% in iowait is
// starved on storage and a bigger CPU would change nothing.
type cpuCollector struct {
	provider Provider
	rates    *rateTracker
}

func newCPUCollector(p Provider) *cpuCollector {
	return &cpuCollector{provider: p, rates: newRateTracker()}
}

func (c *cpuCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "system.cpu",
		Name:        "CPU",
		Description: "Processor utilisation, per core and by CPU state.",
		Timeout:     5 * time.Second,
	}
}

func (c *cpuCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()
	samples := make([]collect.Sample, 0, 16)

	perCore, err := c.provider.CPUPercent(ctx)
	if err != nil {
		return nil, err
	}

	// The aggregate is the mean of the per-core figures rather than a second
	// reading from the provider. Two separate reads would cover slightly
	// different windows, and the aggregate would not equal the mean of the
	// parts — which is the first thing anyone checks when a chart looks wrong.
	var total float64
	for i, pct := range perCore {
		total += pct
		samples = append(samples, collect.Sample{
			Metric: "system.cpu.core.usage",
			Value:  clampPercent(pct),
			Unit:   collect.UnitPercent,
			Kind:   collect.KindGauge,
			Time:   now,
			Labels: map[string]string{"core": strconv.Itoa(i)},
		})
	}

	if len(perCore) > 0 {
		samples = append(samples, collect.Sample{
			Metric: "system.cpu.usage",
			Value:  clampPercent(total / float64(len(perCore))),
			Unit:   collect.UnitPercent,
			Kind:   collect.KindGauge,
			Time:   now,
		})
	}

	// The state breakdown is best-effort: on a host where per-core utilisation
	// read cleanly, losing the breakdown is not worth discarding the batch.
	// Returning partial data beats returning none.
	times, err := c.provider.CPUTimes(ctx)
	if err != nil {
		return samples, nil
	}

	// CPU times are cumulative seconds since boot, so each state's share is
	// the rate of change of its counter — seconds of that state per second of
	// wall clock, which across all cores sums to the core count.
	states := map[string]float64{
		"user": times.User, "system": times.System, "idle": times.Idle,
		"iowait": times.IOWait, "steal": times.Steal, "nice": times.Nice,
		"irq": times.IRQ, "softirq": times.SoftIRQ,
	}
	for state, seconds := range states {
		perSecond, ok := c.rates.rate("state:"+state, seconds, now)
		if !ok {
			continue // first run, or the counters reset
		}
		samples = append(samples, collect.Sample{
			Metric: "system.cpu.time",
			Value:  perSecond,
			Unit:   collect.UnitRatio,
			Kind:   collect.KindGauge,
			Time:   now,
			Labels: map[string]string{"state": state},
		})
	}

	return samples, nil
}

// clampPercent bounds a percentage to 0-100.
//
// Utilisation derived from counter deltas can land slightly outside the range
// when a counter is updated between two reads, and on some virtualised
// platforms the guest's clock and the hypervisor's disagree enough to produce
// 100.4%. Storing it would make an axis auto-scale past 100 and make every
// chart on the page look subtly wrong.
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
