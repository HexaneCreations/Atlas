package costanalysis

// ReferenceRates are the per-unit rates [ReferencePricing] charges. Every
// rate is independent and zero-valued rates simply cost nothing, so a
// deployment can price only what it cares about.
type ReferenceRates struct {
	// CPUCoreHour is cost per vCPU-hour at 100% utilization.
	CPUCoreHour float64
	// MemoryGBHour is cost per GB-hour of total (provisioned) memory.
	MemoryGBHour float64
	// DiskGBHour is cost per GB-hour of total (provisioned) disk.
	DiskGBHour float64
	// NetworkGB is cost per GB transferred (received plus sent).
	NetworkGB float64
	// ContainerHour is cost per running-container-hour.
	ContainerHour float64
}

// DefaultReferenceRates are illustrative flat rates, not tied to any
// provider — a starting point for a deployment to override, and what
// [ReferencePricing] uses if none are given.
func DefaultReferenceRates() ReferenceRates {
	return ReferenceRates{
		CPUCoreHour:   0.02,
		MemoryGBHour:  0.005,
		DiskGBHour:    0.0001,
		NetworkGB:     0.01,
		ContainerHour: 0.01,
	}
}

// ReferencePricing is a simple, provider-neutral linear-rate [PricingModel]:
// each category is usage times its rate. It is both a usable default and a
// template for a real rate card — AWS, Azure, GCP, on-prem, or a custom
// enterprise one — which need only implement the same five interfaces.
type ReferencePricing struct {
	Rates ReferenceRates
}

// NewReferencePricing builds a ReferencePricing. Zero-valued rates default
// to [DefaultReferenceRates].
func NewReferencePricing(rates ReferenceRates) ReferencePricing {
	if rates == (ReferenceRates{}) {
		rates = DefaultReferenceRates()
	}
	return ReferencePricing{Rates: rates}
}

func (p ReferencePricing) Name() string { return "reference" }

func (p ReferencePricing) CPU() CPUPricing { return referenceCPUPricing{rate: p.Rates.CPUCoreHour} }
func (p ReferencePricing) Memory() MemoryPricing {
	return referenceMemoryPricing{rate: p.Rates.MemoryGBHour}
}
func (p ReferencePricing) Disk() DiskPricing { return referenceDiskPricing{rate: p.Rates.DiskGBHour} }
func (p ReferencePricing) Network() NetworkPricing {
	return referenceNetworkPricing{rate: p.Rates.NetworkGB}
}
func (p ReferencePricing) Container() ContainerPricing {
	return referenceContainerPricing{rate: p.Rates.ContainerHour}
}

const bytesPerGB = 1 << 30

type referenceCPUPricing struct{ rate float64 }

func (p referenceCPUPricing) Cost(u Usage) float64 {
	return float64(u.CPUCores) * (u.CPUUtilizationPercent / 100) * p.rate
}

type referenceMemoryPricing struct{ rate float64 }

func (p referenceMemoryPricing) Cost(u Usage) float64 {
	return (u.MemoryTotalBytes / bytesPerGB) * p.rate
}

type referenceDiskPricing struct{ rate float64 }

func (p referenceDiskPricing) Cost(u Usage) float64 {
	return (u.DiskTotalBytes / bytesPerGB) * p.rate
}

type referenceNetworkPricing struct{ rate float64 }

// Cost treats the sampled throughput as sustained for an hour — the amount
// a node would transfer in an hour at its current rate — since that is what
// makes this comparable to the other categories' cost-per-hour.
func (p referenceNetworkPricing) Cost(u Usage) float64 {
	bytesPerHour := (u.NetworkRxBytesPerSec + u.NetworkTxBytesPerSec) * 3600
	return (bytesPerHour / bytesPerGB) * p.rate
}

type referenceContainerPricing struct{ rate float64 }

func (p referenceContainerPricing) Cost(u Usage) float64 {
	return float64(u.RunningContainers) * p.rate
}
