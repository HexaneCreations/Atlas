package goldensignals

import (
	"fmt"

	"github.com/hexane/atlas/internal/core/capacityplanning"
)

// resourceDomains are the capacity domains that count toward Saturation —
// CPU, memory, disk. Network, container density, and process density are
// capacity domains too, but density and link-capacity ceilings are not
// "how saturated is this resource" in the classic Golden Signal sense, and
// both are unavailable by default anyway (see [capacityplanning.NetworkProvider]).
var resourceDomains = map[string]bool{
	capacityplanning.DomainCPU:    true,
	capacityplanning.DomainMemory: true,
	capacityplanning.DomainDisk:   true,
}

// SaturationProvider measures saturation as the most-utilized of CPU,
// memory, and disk — reusing [capacityplanning.Engine]'s per-resource
// utilization rather than recomputing it. The worst resource is what
// determines whether the node is at risk, the same reasoning
// [capacityplanning.DiskProvider] applies across mount points.
type SaturationProvider struct{}

func (SaturationProvider) Name() SignalName { return SignalSaturation }

func (SaturationProvider) Measure(in Inputs) Signal {
	var worst float64
	var worstDomain string
	available := false

	for _, d := range in.Capacity.Domains {
		if !d.Available || !resourceDomains[d.Name] {
			continue
		}
		available = true
		if d.UtilizationPercent > worst {
			worst, worstDomain = d.UtilizationPercent, d.Name
		}
	}

	if !available {
		return Signal{Detail: "no CPU, memory, or disk utilization data"}
	}
	return Signal{
		Available: true, Value: worst, Unit: "percent",
		Detail: fmt.Sprintf("most saturated resource: %s at %.1f%%", worstDomain, worst),
	}
}
