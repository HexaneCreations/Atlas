package capacityplanning

import "testing"

func TestCPUProviderUnavailableWithoutData(t *testing.T) {
	p := NewCPUProvider(0, 0)
	d := p.Assess(Snapshot{})
	if d.Available {
		t.Fatal("expected unavailable with no CPU data")
	}
}

func TestCPUProviderComputesUtilizationAndRemaining(t *testing.T) {
	p := NewCPUProvider(0, 0)
	d := p.Assess(Snapshot{HasCPU: true, CPUCores: 4, CPUUtilizationPercent: 50})
	if !d.Available {
		t.Fatal("expected available")
	}
	if d.UtilizationPercent != 50 {
		t.Errorf("utilization = %v, want 50", d.UtilizationPercent)
	}
	if d.RemainingCapacity != 2 {
		t.Errorf("remaining = %v, want 2 cores", d.RemainingCapacity)
	}
	if d.Status != StatusHealthy {
		t.Errorf("status = %v, want healthy", d.Status)
	}
}

func TestCPUProviderStatusEscalatesAtThresholds(t *testing.T) {
	p := NewCPUProvider(75, 90)
	if got := p.Assess(Snapshot{HasCPU: true, CPUCores: 1, CPUUtilizationPercent: 95}).Status; got != StatusCritical {
		t.Errorf("status at 95%% = %v, want critical", got)
	}
	if got := p.Assess(Snapshot{HasCPU: true, CPUCores: 1, CPUUtilizationPercent: 80}).Status; got != StatusWarning {
		t.Errorf("status at 80%% = %v, want warning", got)
	}
}

func TestMemoryProviderComputesRemainingGB(t *testing.T) {
	p := NewMemoryProvider(0, 0)
	d := p.Assess(Snapshot{
		HasMemory: true, MemoryTotalBytes: 8 * bytesPerGB, MemoryUsedBytes: 6 * bytesPerGB, MemoryUtilizationPercent: 75,
	})
	if !d.Available {
		t.Fatal("expected available")
	}
	if d.RemainingCapacity != 2 {
		t.Errorf("remaining = %v, want 2 GB", d.RemainingCapacity)
	}
	if d.Status != StatusWarning {
		t.Errorf("status = %v, want warning at the 75%% default threshold", d.Status)
	}
}

func TestMemoryProviderUnavailableWithoutData(t *testing.T) {
	if NewMemoryProvider(0, 0).Assess(Snapshot{}).Available {
		t.Fatal("expected unavailable with no memory data")
	}
}

func TestDiskProviderUsesTheWorstMountForStatusAndSumsRemaining(t *testing.T) {
	p := NewDiskProvider(0, 0)
	d := p.Assess(Snapshot{Disks: []DiskMount{
		{Mountpoint: "/", TotalBytes: 100 * bytesPerGB, UsedBytes: 95 * bytesPerGB, UtilizationPercent: 95},
		{Mountpoint: "/data", TotalBytes: 100 * bytesPerGB, UsedBytes: 10 * bytesPerGB, UtilizationPercent: 10},
	}})
	if !d.Available {
		t.Fatal("expected available")
	}
	if d.UtilizationPercent != 95 {
		t.Errorf("utilization = %v, want 95 (the worst mount)", d.UtilizationPercent)
	}
	if d.Status != StatusCritical {
		t.Errorf("status = %v, want critical", d.Status)
	}
	wantRemaining := 5.0 + 90.0
	if d.RemainingCapacity != wantRemaining {
		t.Errorf("remaining = %v, want %v (summed across both mounts)", d.RemainingCapacity, wantRemaining)
	}
}

func TestDiskProviderUnavailableWithoutData(t *testing.T) {
	if NewDiskProvider(0, 0).Assess(Snapshot{}).Available {
		t.Fatal("expected unavailable with no disk data")
	}
}

func TestNetworkProviderUnavailableWithoutConfiguredCapacity(t *testing.T) {
	p := NewNetworkProvider(0, 0, 0)
	d := p.Assess(Snapshot{HasNetwork: true, NetworkRxBytesPerSec: 1000})
	if d.Available {
		t.Fatal("expected unavailable when no link capacity is configured")
	}
}

func TestNetworkProviderUnavailableWithoutData(t *testing.T) {
	p := NewNetworkProvider(1000, 0, 0)
	if p.Assess(Snapshot{}).Available {
		t.Fatal("expected unavailable with no network data")
	}
}

func TestNetworkProviderComputesUtilizationAgainstConfiguredCapacity(t *testing.T) {
	p := NewNetworkProvider(1000, 0, 0)
	d := p.Assess(Snapshot{HasNetwork: true, NetworkRxBytesPerSec: 400, NetworkTxBytesPerSec: 100})
	if !d.Available {
		t.Fatal("expected available")
	}
	if d.UtilizationPercent != 50 {
		t.Errorf("utilization = %v, want 50", d.UtilizationPercent)
	}
}

func TestContainerDensityProviderUnavailableWithoutConfiguredMax(t *testing.T) {
	p := NewContainerDensityProvider(0, 0, 0)
	d := p.Assess(Snapshot{HasContainers: true, RunningContainers: 5})
	if d.Available {
		t.Fatal("expected unavailable when no density ceiling is configured")
	}
}

func TestContainerDensityProviderComputesUtilization(t *testing.T) {
	p := NewContainerDensityProvider(10, 0, 0)
	d := p.Assess(Snapshot{HasContainers: true, RunningContainers: 5})
	if !d.Available {
		t.Fatal("expected available")
	}
	if d.UtilizationPercent != 50 {
		t.Errorf("utilization = %v, want 50", d.UtilizationPercent)
	}
	if d.RemainingCapacity != 5 {
		t.Errorf("remaining = %v, want 5", d.RemainingCapacity)
	}
}

func TestProcessDensityProviderUnavailableWithoutConfiguredMax(t *testing.T) {
	p := NewProcessDensityProvider(0, 0, 0)
	d := p.Assess(Snapshot{HasProcesses: true, RunningProcesses: 50})
	if d.Available {
		t.Fatal("expected unavailable when no density ceiling is configured")
	}
}

func TestProcessDensityProviderComputesUtilization(t *testing.T) {
	p := NewProcessDensityProvider(100, 0, 0)
	d := p.Assess(Snapshot{HasProcesses: true, RunningProcesses: 25})
	if !d.Available {
		t.Fatal("expected available")
	}
	if d.UtilizationPercent != 25 {
		t.Errorf("utilization = %v, want 25", d.UtilizationPercent)
	}
	if d.RemainingCapacity != 75 {
		t.Errorf("remaining = %v, want 75", d.RemainingCapacity)
	}
}
