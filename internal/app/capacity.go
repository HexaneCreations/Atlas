package app

import (
	"context"

	corecapacity "github.com/hexane/atlas/internal/core/capacityplanning"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/metric"
)

// newCapacityEngine builds the capacity planning engine over the same lazy,
// pool-backed adapter style as [newHealthEngine] and [newCostEngine].
//
// Network, container-density, and process-density are left unconfigured
// (their ceilings default to zero, i.e. unavailable) — Atlas does not
// discover a link speed or an operator's intended density limit, and
// guessing one would be exactly the kind of invented number this project
// refuses to produce. A deployment that knows its ceilings configures the
// corresponding provider.
func newCapacityEngine(pool *postgres.Pool) *corecapacity.Engine {
	return corecapacity.NewEngine(corecapacity.Options{
		Snapshots: lazySnapshotSource{pool: pool},
		Providers: []corecapacity.Provider{
			corecapacity.NewCPUProvider(0, 0),
			corecapacity.NewMemoryProvider(0, 0),
			corecapacity.NewDiskProvider(0, 0),
			corecapacity.NewNetworkProvider(0, 0, 0),
			corecapacity.NewContainerDensityProvider(0, 0, 0),
			corecapacity.NewProcessDensityProvider(0, 0, 0),
		},
	})
}

// lazySnapshotSource adapts the metric repository to
// [corecapacity.SnapshotSource], deferring repository construction to call
// time — for the same reason as [lazyInventoryStore].
type lazySnapshotSource struct{ pool *postgres.Pool }

func (l lazySnapshotSource) Snapshot(ctx context.Context, nodeID string) (corecapacity.Snapshot, error) {
	repo := metric.NewRepository(l.pool.DB())

	node, err := repo.GetNode(ctx, nodeID)
	if err != nil {
		return corecapacity.Snapshot{}, err
	}
	values, err := repo.Latest(ctx, nodeID, usageLookback)
	if err != nil {
		return corecapacity.Snapshot{}, err
	}

	s := corecapacity.Snapshot{NodeID: nodeID, CPUCores: node.CPUCores}
	disks := map[string]corecapacity.DiskMount{}

	for _, v := range values {
		switch v.Metric {
		case metricCPUUsage:
			s.CPUUtilizationPercent, s.HasCPU = v.Value, true
		case metricMemoryTotal:
			s.MemoryTotalBytes, s.HasMemory = v.Value, true
		case metricMemoryUsed:
			s.MemoryUsedBytes, s.HasMemory = v.Value, true
		case metricMemoryUsage:
			s.MemoryUtilizationPercent, s.HasMemory = v.Value, true
		case metricDiskTotal:
			mount := diskMount(disks, v.Labels["mountpoint"])
			mount.TotalBytes = v.Value
			disks[v.Labels["mountpoint"]] = mount
		case metricDiskUsed:
			mount := diskMount(disks, v.Labels["mountpoint"])
			mount.UsedBytes = v.Value
			disks[v.Labels["mountpoint"]] = mount
		case metricDiskUsage:
			mount := diskMount(disks, v.Labels["mountpoint"])
			mount.UtilizationPercent = v.Value
			disks[v.Labels["mountpoint"]] = mount
		case metricNetworkRxBytes:
			s.NetworkRxBytesPerSec, s.HasNetwork = s.NetworkRxBytesPerSec+v.Value, true
		case metricNetworkTxBytes:
			s.NetworkTxBytesPerSec, s.HasNetwork = s.NetworkTxBytesPerSec+v.Value, true
		case metricContainersCount:
			if v.Labels["state"] == containersCountRunning {
				s.RunningContainers, s.HasContainers = s.RunningContainers+int(v.Value), true
			}
		case metricProcessTotal:
			s.RunningProcesses, s.HasProcesses = int(v.Value), true
		}
	}

	for _, d := range disks {
		if d.TotalBytes > 0 {
			s.Disks = append(s.Disks, d)
		}
	}
	return s, nil
}

func diskMount(disks map[string]corecapacity.DiskMount, mountpoint string) corecapacity.DiskMount {
	d := disks[mountpoint]
	d.Mountpoint = mountpoint
	return d
}
