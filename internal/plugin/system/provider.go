// Package system observes the host operating system: CPU, memory, swap, disk,
// network, load, and the descriptive facts about the machine itself.
//
// # Why there is a Provider interface
//
// Collectors in this package never call a system library directly. They call
// [Provider], and the composition root supplies an implementation.
//
// That indirection earns its keep three times over. It lets every collector be
// tested by feeding it exact numbers, including the awkward cases — a division
// by zero when a filesystem reports zero capacity, a counter that wrapped, a
// disk that vanished between two calls — which are precisely the cases that
// cannot be reproduced against a real machine on demand. It keeps the choice
// of system library at one seam, so a Linux-native /proc reader can replace
// gopsutil for production hosts without a collector changing. And it makes the
// dependency direction obvious: collectors depend on an interface this package
// owns, not on a third party's data shapes.
package system

import (
	"context"
	"time"
)

// Provider supplies raw host facts.
//
// Implementations must respect the context deadline: every method here reads
// from the operating system, and on a host with a wedged filesystem or a
// saturated disk those reads can block for a long time. A provider that
// ignores cancellation pins a scheduler slot at exactly the moment monitoring
// matters most.
//
// Every method returns absolute readings, not deltas. Rate calculation belongs
// to the collector that owns the metric, because only it knows whether a
// counter reset means a reboot or an interface being recreated.
type Provider interface {
	// Host returns the descriptive facts about the machine. Cheap enough to
	// call on a slow interval, not on every collection.
	Host(ctx context.Context) (HostInfo, error)

	// CPUPercent returns per-core busy percentages since the previous call,
	// or since boot on the first call.
	CPUPercent(ctx context.Context) ([]float64, error)
	// CPUTimes returns cumulative time spent in each CPU state, aggregated
	// across cores. Used for the breakdown a single busy percentage hides.
	CPUTimes(ctx context.Context) (CPUTimes, error)

	// Memory returns physical memory usage.
	Memory(ctx context.Context) (MemoryInfo, error)
	// Swap returns swap usage and its cumulative page counters.
	Swap(ctx context.Context) (SwapInfo, error)

	// Partitions returns the mounted filesystems worth reporting.
	Partitions(ctx context.Context) ([]Partition, error)
	// DiskUsage returns capacity for one mount point.
	DiskUsage(ctx context.Context, mountpoint string) (DiskUsage, error)
	// DiskIO returns cumulative per-device I/O counters.
	DiskIO(ctx context.Context) ([]DiskIOCounters, error)

	// NetworkIO returns cumulative per-interface counters.
	NetworkIO(ctx context.Context) ([]NetworkIOCounters, error)

	// LoadAverage returns the 1, 5, and 15 minute run-queue averages.
	LoadAverage(ctx context.Context) (LoadAverage, error)
}

// HostInfo describes the machine.
type HostInfo struct {
	Hostname string
	// OS is the kernel family: "linux", "darwin".
	OS string
	// Platform is the distribution or product: "ubuntu", "darwin".
	Platform string
	// PlatformVersion is its version: "24.04", "15.3.1".
	PlatformVersion string
	// KernelVersion is the running kernel release.
	KernelVersion string
	// KernelArch is the machine architecture: "x86_64", "arm64".
	KernelArch string
	// BootTime is when the machine started. Uptime is derived from it at
	// query time rather than stored, so it cannot go stale.
	BootTime time.Time
	// LogicalCores is the count of schedulable CPUs, which is what load
	// average must be normalised against.
	LogicalCores int
	// PhysicalCores excludes hardware threads. Zero when unavailable.
	PhysicalCores int
}

// Uptime returns how long the machine has been running.
func (h HostInfo) Uptime() time.Duration {
	if h.BootTime.IsZero() {
		return 0
	}
	return time.Since(h.BootTime)
}

// CPUTimes is cumulative time in each CPU state, in seconds since boot.
type CPUTimes struct {
	User   float64
	System float64
	Idle   float64
	// IOWait is time blocked on I/O. Linux only; zero elsewhere. It is the
	// figure that distinguishes a busy CPU from a starved one.
	IOWait  float64
	Steal   float64
	Nice    float64
	IRQ     float64
	SoftIRQ float64
}

// Total returns the sum of all states.
func (c CPUTimes) Total() float64 {
	return c.User + c.System + c.Idle + c.IOWait + c.Steal + c.Nice + c.IRQ + c.SoftIRQ
}

// MemoryInfo is physical memory usage, in bytes.
type MemoryInfo struct {
	Total     uint64
	Available uint64
	Used      uint64
	Free      uint64
	Cached    uint64
	Buffers   uint64
	// UsedPercent is computed against Available rather than Free, so page
	// cache is not counted as pressure. A Linux host with almost no Free
	// memory is usually perfectly healthy.
	UsedPercent float64
}

// SwapInfo is swap usage, in bytes, with its cumulative page counters.
type SwapInfo struct {
	Total       uint64
	Used        uint64
	Free        uint64
	UsedPercent float64
	// Sin and Sout are cumulative pages swapped in and out. Sustained
	// non-zero rates indicate real memory pressure, which static usage alone
	// does not: swap that was filled hours ago and never touched is harmless.
	Sin  uint64
	Sout uint64
}

// Partition is a mounted filesystem.
type Partition struct {
	Device     string
	Mountpoint string
	Fstype     string
	Opts       []string
}

// DiskUsage is capacity for one mount point, in bytes.
type DiskUsage struct {
	Path        string
	Fstype      string
	Total       uint64
	Free        uint64
	Used        uint64
	UsedPercent float64
	InodesTotal uint64
	InodesUsed  uint64
	// InodesUsedPercent matters independently of byte usage: a filesystem
	// full of tiny files exhausts inodes while reporting plenty of free
	// space, and the failure looks identical to a full disk to whatever hits
	// it.
	InodesUsedPercent float64
}

// DiskIOCounters is cumulative I/O for one block device.
type DiskIOCounters struct {
	Device     string
	ReadCount  uint64
	WriteCount uint64
	ReadBytes  uint64
	WriteBytes uint64
	// IoTime is cumulative milliseconds during which the device had I/O in
	// flight. Its rate of change is utilisation, which is the figure that
	// reveals saturation before throughput does.
	IoTime uint64
}

// NetworkIOCounters is cumulative traffic for one interface.
type NetworkIOCounters struct {
	Name        string
	BytesSent   uint64
	BytesRecv   uint64
	PacketsSent uint64
	PacketsRecv uint64
	Errin       uint64
	Errout      uint64
	Dropin      uint64
	Dropout     uint64
}

// LoadAverage is the run-queue average over three windows.
type LoadAverage struct {
	Load1  float64
	Load5  float64
	Load15 float64
}
