package costanalysis

import "testing"

func TestReferenceCPUPricingScalesByCoresAndUtilization(t *testing.T) {
	p := NewReferencePricing(ReferenceRates{CPUCoreHour: 1})
	got := p.CPU().Cost(Usage{CPUCores: 4, CPUUtilizationPercent: 50})
	want := 4.0 * 0.5 * 1
	if got != want {
		t.Errorf("cpu cost = %v, want %v", got, want)
	}
}

func TestReferenceMemoryPricingScalesByProvisionedGB(t *testing.T) {
	p := NewReferencePricing(ReferenceRates{MemoryGBHour: 2})
	got := p.Memory().Cost(Usage{MemoryTotalBytes: bytesPerGB * 8})
	want := 8.0 * 2
	if got != want {
		t.Errorf("memory cost = %v, want %v", got, want)
	}
}

func TestReferenceDiskPricingScalesByProvisionedGB(t *testing.T) {
	p := NewReferencePricing(ReferenceRates{DiskGBHour: 0.5})
	got := p.Disk().Cost(Usage{DiskTotalBytes: bytesPerGB * 100})
	want := 100.0 * 0.5
	if got != want {
		t.Errorf("disk cost = %v, want %v", got, want)
	}
}

func TestReferenceNetworkPricingExtrapolatesThroughputToAnHour(t *testing.T) {
	p := NewReferencePricing(ReferenceRates{NetworkGB: 1})
	// 1 GB/s sustained for an hour is 3600 GB.
	got := p.Network().Cost(Usage{NetworkRxBytesPerSec: bytesPerGB, NetworkTxBytesPerSec: 0})
	want := 3600.0
	if got != want {
		t.Errorf("network cost = %v, want %v", got, want)
	}
}

func TestReferenceContainerPricingScalesByRunningCount(t *testing.T) {
	p := NewReferencePricing(ReferenceRates{ContainerHour: 0.1})
	got := p.Container().Cost(Usage{RunningContainers: 5})
	want := 5.0 * 0.1
	if got != want {
		t.Errorf("container cost = %v, want %v", got, want)
	}
}

func TestNewReferencePricingDefaultsZeroRates(t *testing.T) {
	p := NewReferencePricing(ReferenceRates{})
	if p.Rates != DefaultReferenceRates() {
		t.Errorf("rates = %+v, want the defaults", p.Rates)
	}
}

func TestZeroRateCategoryCostsNothing(t *testing.T) {
	p := NewReferencePricing(ReferenceRates{CPUCoreHour: 0, MemoryGBHour: 1})
	if got := p.CPU().Cost(Usage{CPUCores: 64, CPUUtilizationPercent: 100}); got != 0 {
		t.Errorf("cpu cost = %v, want 0 for a zero rate", got)
	}
}
