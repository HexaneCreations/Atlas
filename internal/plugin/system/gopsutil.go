package system

import (
	"context"
	stdnet "net"
	"slices"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

// gopsutilProvider reads host facts through gopsutil.
//
// gopsutil is the same library Telegraf ships, covers Linux and macOS from one
// code path, and has years of production exposure to the edge cases that make
// hand-rolled /proc parsing expensive to get right — cgroup v2 versus v1,
// containers reporting the host's memory, filesystems that lie about capacity.
//
// It is deliberately hidden behind [Provider]. See ADR-0012 for the trade-off
// and the conditions under which a Linux-native implementation should replace
// it.
type gopsutilProvider struct{}

// NewProvider returns a Provider backed by gopsutil.
func NewProvider() Provider { return gopsutilProvider{} }

func (gopsutilProvider) Host(ctx context.Context) (HostInfo, error) {
	const op = "system.Provider.Host"

	info, err := host.InfoWithContext(ctx)
	if err != nil {
		return HostInfo{}, errs.Wrap(err, errs.CodeUnavailable, "could not read host information").WithOp(op)
	}

	out := HostInfo{
		Hostname:           info.Hostname,
		OS:                 info.OS,
		Platform:           info.Platform,
		PlatformVersion:    info.PlatformVersion,
		KernelVersion:      info.KernelVersion,
		KernelArch:         info.KernelArch,
		BootTime:           time.Unix(int64(info.BootTime), 0),
		Virtualization:     info.VirtualizationSystem,
		VirtualizationRole: info.VirtualizationRole,
		Timezone:           localTimezone(),
		FQDN:               resolveFQDN(ctx, info.Hostname),
		HardwareUUID:       readHardwareUUID(ctx),
	}

	// Core counts are reported separately from host info and may be
	// unavailable in restricted environments. They are useful but not
	// essential, so a failure leaves them zero rather than failing the read:
	// hostname and kernel version are worth having on their own.
	if logical, err := cpu.CountsWithContext(ctx, true); err == nil {
		out.LogicalCores = logical
	}
	if physical, err := cpu.CountsWithContext(ctx, false); err == nil {
		out.PhysicalCores = physical
	}
	if infos, err := cpu.InfoWithContext(ctx); err == nil && len(infos) > 0 {
		out.CPUModel = strings.TrimSpace(infos[0].ModelName)
		sockets := make(map[string]struct{}, len(infos))
		for _, info := range infos {
			if info.PhysicalID != "" {
				sockets[info.PhysicalID] = struct{}{}
			}
		}
		out.CPUSockets = len(sockets)
	}
	return out, nil
}

func localTimezone() string {
	zone, _ := time.Now().Zone()
	if name := time.Local.String(); name != "" && name != "Local" {
		return name
	}
	return zone
}

// resolveFQDN asks the host's resolver for the canonical name behind
// hostname. A host with no resolvable domain is normal, not an error, so
// anything unresolved yields "".
func resolveFQDN(ctx context.Context, hostname string) string {
	if hostname == "" {
		return ""
	}
	var resolver stdnet.Resolver
	cname, err := resolver.LookupCNAME(ctx, hostname)
	if err != nil {
		return ""
	}
	cname = strings.TrimSuffix(cname, ".")
	if cname == hostname {
		return ""
	}
	return cname
}

func (gopsutilProvider) CPUPercent(ctx context.Context) ([]float64, error) {
	const op = "system.Provider.CPUPercent"

	// A zero interval makes gopsutil compare against the counters it saw on
	// the previous call rather than sleeping to measure a window. Sleeping
	// inside a collector would burn a scheduler slot doing nothing, and the
	// collection interval already provides the window.
	percents, err := cpu.PercentWithContext(ctx, 0, true)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read CPU utilisation").WithOp(op)
	}
	return percents, nil
}

func (gopsutilProvider) CPUTimes(ctx context.Context) (CPUTimes, error) {
	const op = "system.Provider.CPUTimes"

	times, err := cpu.TimesWithContext(ctx, false) // aggregated across cores
	if err != nil {
		return CPUTimes{}, errs.Wrap(err, errs.CodeUnavailable, "could not read CPU times").WithOp(op)
	}
	if len(times) == 0 {
		return CPUTimes{}, errs.New(errs.CodeUnavailable, "the host reported no CPU times").WithOp(op)
	}

	t := times[0]
	return CPUTimes{
		User: t.User, System: t.System, Idle: t.Idle,
		IOWait: t.Iowait, Steal: t.Steal, Nice: t.Nice,
		IRQ: t.Irq, SoftIRQ: t.Softirq,
	}, nil
}

func (gopsutilProvider) Memory(ctx context.Context) (MemoryInfo, error) {
	const op = "system.Provider.Memory"

	v, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return MemoryInfo{}, errs.Wrap(err, errs.CodeUnavailable, "could not read memory usage").WithOp(op)
	}

	out := MemoryInfo{
		Total: v.Total, Available: v.Available, Used: v.Used,
		Free: v.Free, Cached: v.Cached, Buffers: v.Buffers,
		UsedPercent: v.UsedPercent,
	}

	// gopsutil already computes UsedPercent from Available on platforms that
	// report it. Recompute defensively where Total is known but the figure
	// came back as zero, so a missing percentage does not read as an idle
	// machine.
	if out.UsedPercent == 0 && out.Total > 0 && out.Available <= out.Total {
		out.UsedPercent = percent(out.Total-out.Available, out.Total)
	}
	return out, nil
}

func (gopsutilProvider) Swap(ctx context.Context) (SwapInfo, error) {
	const op = "system.Provider.Swap"

	s, err := mem.SwapMemoryWithContext(ctx)
	if err != nil {
		return SwapInfo{}, errs.Wrap(err, errs.CodeUnavailable, "could not read swap usage").WithOp(op)
	}
	return SwapInfo{
		Total: s.Total, Used: s.Used, Free: s.Free,
		UsedPercent: s.UsedPercent, Sin: s.Sin, Sout: s.Sout,
	}, nil
}

// pseudoFilesystems are mounted but describe no real storage. Reporting them
// would add dozens of series per host, most of them constant at 100% full,
// which is noise that hides the one filesystem that matters.
var pseudoFilesystems = []string{
	"autofs", "binfmt_misc", "bpf", "cgroup", "cgroup2", "configfs", "debugfs",
	"devfs", "devpts", "devtmpfs", "efivarfs", "fuse.portal", "fusectl",
	"hugetlbfs", "iso9660", "mqueue", "nsfs", "overlay", "proc", "pstore",
	"ramfs", "rpc_pipefs", "securityfs", "selinuxfs", "squashfs", "sysfs",
	"tracefs",
}

func (gopsutilProvider) Partitions(ctx context.Context) ([]Partition, error) {
	const op = "system.Provider.Partitions"

	// all=false already excludes most pseudo filesystems; the explicit list
	// below covers what varies between platforms and kernel versions.
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list filesystems").WithOp(op)
	}

	out := make([]Partition, 0, len(parts))
	for _, p := range parts {
		if slices.Contains(pseudoFilesystems, strings.ToLower(p.Fstype)) {
			continue
		}
		// A read-only mount that is by definition full — a snap image, a
		// mounted DMG — would otherwise sit permanently at 100% and train
		// operators to ignore disk alerts.
		if slices.Contains(p.Opts, "ro") && strings.HasPrefix(p.Mountpoint, "/snap") {
			continue
		}
		out = append(out, Partition{
			Device: p.Device, Mountpoint: p.Mountpoint,
			Fstype: p.Fstype, Opts: p.Opts,
		})
	}
	return out, nil
}

func (gopsutilProvider) DiskUsage(ctx context.Context, mountpoint string) (DiskUsage, error) {
	const op = "system.Provider.DiskUsage"

	u, err := disk.UsageWithContext(ctx, mountpoint)
	if err != nil {
		return DiskUsage{}, errs.Wrap(err, errs.CodeUnavailable,
			"could not read usage for a filesystem").
			WithOp(op).WithDetail("mountpoint", mountpoint)
	}

	return DiskUsage{
		Path: u.Path, Fstype: u.Fstype,
		Total: u.Total, Free: u.Free, Used: u.Used,
		UsedPercent:       u.UsedPercent,
		InodesTotal:       u.InodesTotal,
		InodesUsed:        u.InodesUsed,
		InodesUsedPercent: u.InodesUsedPercent,
	}, nil
}

func (gopsutilProvider) DiskIO(ctx context.Context) ([]DiskIOCounters, error) {
	const op = "system.Provider.DiskIO"

	counters, err := disk.IOCountersWithContext(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read disk I/O counters").WithOp(op)
	}

	out := make([]DiskIOCounters, 0, len(counters))
	for name, c := range counters {
		out = append(out, DiskIOCounters{
			Device:    name,
			ReadCount: c.ReadCount, WriteCount: c.WriteCount,
			ReadBytes: c.ReadBytes, WriteBytes: c.WriteBytes,
			IoTime: c.IoTime,
		})
	}
	// Map iteration order is random; sorting keeps logs and any ordering
	// assumption downstream stable.
	slices.SortFunc(out, func(a, b DiskIOCounters) int { return strings.Compare(a.Device, b.Device) })
	return out, nil
}

func (gopsutilProvider) NetworkIO(ctx context.Context) ([]NetworkIOCounters, error) {
	const op = "system.Provider.NetworkIO"

	counters, err := net.IOCountersWithContext(ctx, true) // per interface
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read network counters").WithOp(op)
	}

	out := make([]NetworkIOCounters, 0, len(counters))
	for _, c := range counters {
		out = append(out, NetworkIOCounters{
			Name:      c.Name,
			BytesSent: c.BytesSent, BytesRecv: c.BytesRecv,
			PacketsSent: c.PacketsSent, PacketsRecv: c.PacketsRecv,
			Errin: c.Errin, Errout: c.Errout,
			Dropin: c.Dropin, Dropout: c.Dropout,
		})
	}
	slices.SortFunc(out, func(a, b NetworkIOCounters) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (gopsutilProvider) LoadAverage(ctx context.Context) (LoadAverage, error) {
	const op = "system.Provider.LoadAverage"

	avg, err := load.AvgWithContext(ctx)
	if err != nil {
		return LoadAverage{}, errs.Wrap(err, errs.CodeUnavailable, "could not read load average").WithOp(op)
	}
	return LoadAverage{Load1: avg.Load1, Load5: avg.Load5, Load15: avg.Load15}, nil
}

// percent computes part/total as a percentage, returning zero when total is
// zero.
//
// Filesystems and cgroups do report zero capacity — an unmounted device, an
// unlimited cgroup — and a NaN reaching a sample would be rejected at the
// pipeline boundary, losing the whole batch rather than one value.
func percent(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}
