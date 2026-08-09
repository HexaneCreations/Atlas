package system

import (
	"context"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
)

// networkCollector reports per-interface traffic rates and error counts.
type networkCollector struct {
	provider Provider
	rates    *rateTracker
}

func newNetworkCollector(p Provider) *networkCollector {
	return &networkCollector{provider: p, rates: newRateTracker()}
}

func (c *networkCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "system.network",
		Name:        "Network",
		Description: "Per-interface throughput, packet rates, errors, and drops.",
		Timeout:     10 * time.Second,
	}
}

// skippedInterfacePrefixes are virtual interfaces that would multiply series
// without adding signal.
//
// A container host creates a veth pair per container; on a busy host that is
// hundreds of short-lived interfaces, each generating its own series that
// exists for minutes. That is unbounded label cardinality — the thing most
// likely to make a time-series store unusable — in exchange for traffic
// figures already visible on the bridge the veths attach to.
var skippedInterfacePrefixes = []string{
	"veth",   // container side of a bridge pair
	"docker", // docker0 and friends; the containers themselves are Phase 2
	"br-",    // docker user-defined bridges
	"virbr",  // libvirt bridges
	"cni",    // Kubernetes CNI
	"flannel",
	"cali", // Calico
	"tunl",
	"lxcbr",
}

func (c *networkCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	interfaces, err := c.provider.NetworkIO(ctx)
	if err != nil {
		return nil, err
	}

	samples := make([]collect.Sample, 0, len(interfaces)*6)
	seen := make(map[string]struct{}, len(interfaces)*8)

	for _, iface := range interfaces {
		if skipInterface(iface.Name) {
			continue
		}
		// An interface that has moved nothing since boot is not carrying
		// traffic and never was. A typical macOS or cloud host presents a
		// dozen of these — tunnels, AWDL, unused NICs — and each would consume
		// eight series of permanent zeroes, spending cardinality budget on
		// lines that can never move.
		if iface.BytesRecv == 0 && iface.BytesSent == 0 {
			continue
		}

		labels := map[string]string{"interface": iface.Name}

		counters := []struct {
			key    string
			metric string
			value  float64
			unit   collect.Unit
		}{
			{"rx.bytes", "system.network.rx.bytes", float64(iface.BytesRecv), collect.UnitBytesPerSecond},
			{"tx.bytes", "system.network.tx.bytes", float64(iface.BytesSent), collect.UnitBytesPerSecond},
			{"rx.packets", "system.network.rx.packets", float64(iface.PacketsRecv), collect.UnitOperationsPerSecond},
			{"tx.packets", "system.network.tx.packets", float64(iface.PacketsSent), collect.UnitOperationsPerSecond},
			// Errors and drops are reported as rates too. A cumulative total
			// only ever rises, so a chart of it is a staircase that says
			// nothing about whether the problem is happening *now*.
			{"rx.errors", "system.network.rx.errors", float64(iface.Errin), collect.UnitOperationsPerSecond},
			{"tx.errors", "system.network.tx.errors", float64(iface.Errout), collect.UnitOperationsPerSecond},
			{"rx.dropped", "system.network.rx.dropped", float64(iface.Dropin), collect.UnitOperationsPerSecond},
			{"tx.dropped", "system.network.tx.dropped", float64(iface.Dropout), collect.UnitOperationsPerSecond},
		}

		for _, ctr := range counters {
			key := iface.Name + ":" + ctr.key
			seen[key] = struct{}{}
			if rate, ok := c.rates.rate(key, ctr.value, now); ok {
				samples = append(samples, labelled(ctr.metric, rate, ctr.unit, now, labels))
			}
		}
	}

	// Interfaces come and go constantly on a container host. Retaining only
	// what was just observed bounds memory to the interfaces that exist.
	c.rates.retain(seen)

	return samples, nil
}

func skipInterface(name string) bool {
	// Loopback traffic is real but is never the subject of a capacity or
	// saturation question, and on a busy host it dwarfs external traffic
	// enough to distort an auto-scaled axis.
	if name == "lo" || name == "lo0" {
		return true
	}
	for _, prefix := range skippedInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// loadCollector reports the run-queue averages.
type loadCollector struct {
	provider Provider
	// cores normalises load into a comparable figure. Load 8 is idle on a
	// 64-core machine and a crisis on a 2-core one, so the raw number cannot
	// be alerted on across a heterogeneous fleet.
	cores int
}

func newLoadCollector(p Provider, cores int) *loadCollector {
	return &loadCollector{provider: p, cores: cores}
}

func (c *loadCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "system.load",
		Name:        "Load average",
		Description: "Run-queue averages over 1, 5, and 15 minutes, raw and per core.",
		Timeout:     5 * time.Second,
	}
}

func (c *loadCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	avg, err := c.provider.LoadAverage(ctx)
	if err != nil {
		return nil, err
	}

	samples := []collect.Sample{
		labelled("system.load.average", avg.Load1, collect.UnitRatio, now, map[string]string{"window": "1m"}),
		labelled("system.load.average", avg.Load5, collect.UnitRatio, now, map[string]string{"window": "5m"}),
		labelled("system.load.average", avg.Load15, collect.UnitRatio, now, map[string]string{"window": "15m"}),
	}

	// Per-core load is the figure that is comparable across machines: 1.0
	// means the run queue exactly matches the available cores, whatever the
	// core count.
	if c.cores > 0 {
		cores := float64(c.cores)
		samples = append(samples,
			labelled("system.load.per_core", avg.Load1/cores, collect.UnitRatio, now, map[string]string{"window": "1m"}),
			labelled("system.load.per_core", avg.Load5/cores, collect.UnitRatio, now, map[string]string{"window": "5m"}),
			labelled("system.load.per_core", avg.Load15/cores, collect.UnitRatio, now, map[string]string{"window": "15m"}),
		)
	}

	return samples, nil
}
