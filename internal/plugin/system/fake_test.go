package system

import (
	"context"
	"errors"
	"sync"
	"time"
)

// fakeProvider is a Provider whose every reading is set by the test.
//
// This is the reason Provider exists. The situations that matter most —
// a filesystem reporting zero capacity, a counter that wrapped on reboot, a
// disk that vanishes between two calls, a host with no swap — cannot be
// produced on demand against a real machine, and are exactly the ones where a
// collector divides by zero or draws a spike that never happened.
type fakeProvider struct {
	mu sync.Mutex

	host            HostInfo
	cpuPct          []float64
	cpuTimes        CPUTimes
	memory          MemoryInfo
	swap            SwapInfo
	parts           []Partition
	usage           map[string]DiskUsage
	diskIO          []DiskIOCounters
	networkIO       []NetworkIOCounters
	networkIdentity NetworkIdentity
	load            LoadAverage

	// Per-method errors, so a test can fail one reading and assert that the
	// collector still returns everything else.
	hostErr, cpuPctErr, cpuTimesErr, memErr, swapErr error
	partsErr, diskIOErr, netErr, loadErr             error
	netIdentityErr                                   error
	usageErr                                         map[string]error

	// blockFor makes a method sleep, for cancellation tests.
	blockFor time.Duration
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		host: HostInfo{
			Hostname: "web-01", OS: "linux", Platform: "ubuntu",
			PlatformVersion: "24.04", KernelVersion: "6.8.0", KernelArch: "x86_64",
			BootTime: time.Now().Add(-72 * time.Hour), LogicalCores: 8, PhysicalCores: 4,
		},
		cpuPct:   []float64{10, 20, 30, 40, 50, 60, 70, 80},
		cpuTimes: CPUTimes{User: 1000, System: 500, Idle: 8000, IOWait: 100},
		memory: MemoryInfo{
			Total: 16 << 30, Available: 8 << 30, Used: 8 << 30,
			Free: 2 << 30, Cached: 5 << 30, Buffers: 1 << 30, UsedPercent: 50,
		},
		swap: SwapInfo{Total: 4 << 30, Used: 1 << 30, Free: 3 << 30, UsedPercent: 25, Sin: 1000, Sout: 2000},
		parts: []Partition{
			{Device: "/dev/sda1", Mountpoint: "/", Fstype: "ext4"},
		},
		usage: map[string]DiskUsage{
			"/": {
				Path: "/", Fstype: "ext4",
				Total: 500 << 30, Used: 250 << 30, Free: 250 << 30, UsedPercent: 50,
				InodesTotal: 1000000, InodesUsed: 200000, InodesUsedPercent: 20,
			},
		},
		diskIO: []DiskIOCounters{
			{Device: "sda", ReadCount: 100, WriteCount: 200, ReadBytes: 1 << 20, WriteBytes: 2 << 20, IoTime: 5000},
		},
		networkIO: []NetworkIOCounters{
			{Name: "eth0", BytesRecv: 1 << 20, BytesSent: 2 << 20, PacketsRecv: 100, PacketsSent: 200},
		},
		load:     LoadAverage{Load1: 1.5, Load5: 2.0, Load15: 2.5},
		usageErr: map[string]error{},
	}
}

func (f *fakeProvider) block(ctx context.Context) error {
	f.mu.Lock()
	d := f.blockFor
	f.mu.Unlock()
	if d == 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeProvider) Host(ctx context.Context) (HostInfo, error) {
	if err := f.block(ctx); err != nil {
		return HostInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.host, f.hostErr
}

func (f *fakeProvider) CPUPercent(ctx context.Context) ([]float64, error) {
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cpuPct, f.cpuPctErr
}

func (f *fakeProvider) CPUTimes(ctx context.Context) (CPUTimes, error) {
	if err := f.block(ctx); err != nil {
		return CPUTimes{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cpuTimes, f.cpuTimesErr
}

func (f *fakeProvider) Memory(ctx context.Context) (MemoryInfo, error) {
	if err := f.block(ctx); err != nil {
		return MemoryInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.memory, f.memErr
}

func (f *fakeProvider) Swap(ctx context.Context) (SwapInfo, error) {
	if err := f.block(ctx); err != nil {
		return SwapInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.swap, f.swapErr
}

func (f *fakeProvider) Partitions(ctx context.Context) ([]Partition, error) {
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parts, f.partsErr
}

func (f *fakeProvider) DiskUsage(ctx context.Context, mountpoint string) (DiskUsage, error) {
	if err := f.block(ctx); err != nil {
		return DiskUsage{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.usageErr[mountpoint]; ok {
		return DiskUsage{}, err
	}
	u, ok := f.usage[mountpoint]
	if !ok {
		return DiskUsage{}, errors.New("no such mount point")
	}
	return u, nil
}

func (f *fakeProvider) DiskIO(ctx context.Context) ([]DiskIOCounters, error) {
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diskIO, f.diskIOErr
}

func (f *fakeProvider) NetworkIO(ctx context.Context) ([]NetworkIOCounters, error) {
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networkIO, f.netErr
}

func (f *fakeProvider) NetworkIdentity(ctx context.Context) (NetworkIdentity, error) {
	if err := f.block(ctx); err != nil {
		return NetworkIdentity{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networkIdentity, f.netIdentityErr
}

func (f *fakeProvider) LoadAverage(ctx context.Context) (LoadAverage, error) {
	if err := f.block(ctx); err != nil {
		return LoadAverage{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.load, f.loadErr
}

var _ Provider = (*fakeProvider)(nil)
