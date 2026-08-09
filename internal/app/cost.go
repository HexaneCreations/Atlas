package app

import (
	"context"
	"time"

	corecostanalysis "github.com/hexane/atlas/internal/core/costanalysis"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/metric"
)

// usageLookback bounds how stale a "current" sample may be and still count
// toward a usage snapshot — the same convention as [corealert.DefaultLookback].
const usageLookback = 5 * time.Minute

const (
	metricCPUUsage         = "system.cpu.usage"
	metricMemoryTotal      = "system.memory.total"
	metricMemoryUsed       = "system.memory.used"
	metricMemoryUsage      = "system.memory.usage"
	metricDiskTotal        = "system.disk.total"
	metricDiskUsed         = "system.disk.used"
	metricDiskUsage        = "system.disk.usage"
	metricNetworkRxBytes   = "system.network.rx.bytes"
	metricNetworkTxBytes   = "system.network.tx.bytes"
	metricContainersCount  = "docker.containers.count"
	metricProcessTotal     = "process.total"
	containersCountRunning = "running"
)

// newCostEngine builds the cost analysis engine over the same lazy,
// pool-backed adapter style as [newHealthEngine], with the reference,
// provider-neutral pricing model as the default rate card.
func newCostEngine(pool *postgres.Pool) *corecostanalysis.Engine {
	return corecostanalysis.NewEngine(corecostanalysis.Options{
		Usage:   lazyUsageSource{pool: pool},
		Pricing: corecostanalysis.NewReferencePricing(corecostanalysis.DefaultReferenceRates()),
	})
}

// lazyUsageSource adapts the metric repository to [corecostanalysis.UsageSource],
// deferring repository construction to call time — for the same reason as
// [lazyInventoryStore].
type lazyUsageSource struct{ pool *postgres.Pool }

func (l lazyUsageSource) Usage(ctx context.Context, nodeID string) (corecostanalysis.Usage, error) {
	repo := metric.NewRepository(l.pool.DB())

	node, err := repo.GetNode(ctx, nodeID)
	if err != nil {
		return corecostanalysis.Usage{}, err
	}
	values, err := repo.Latest(ctx, nodeID, usageLookback)
	if err != nil {
		return corecostanalysis.Usage{}, err
	}

	u := corecostanalysis.Usage{NodeID: nodeID, CPUCores: node.CPUCores, UptimeSeconds: node.UptimeSeconds()}
	for _, v := range values {
		switch v.Metric {
		case metricCPUUsage:
			u.CPUUtilizationPercent = v.Value
		case metricMemoryTotal:
			u.MemoryTotalBytes = v.Value
		case metricMemoryUsed:
			u.MemoryUsedBytes = v.Value
		case metricDiskTotal:
			// Summed across every mountpoint's label — total provisioned
			// capacity across the node's filesystems.
			u.DiskTotalBytes += v.Value
		case metricDiskUsed:
			u.DiskUsedBytes += v.Value
		case metricNetworkRxBytes:
			// Summed across every interface's label.
			u.NetworkRxBytesPerSec += v.Value
		case metricNetworkTxBytes:
			u.NetworkTxBytesPerSec += v.Value
		case metricContainersCount:
			if v.Labels["state"] == containersCountRunning {
				u.RunningContainers += int(v.Value)
			}
		case metricProcessTotal:
			u.RunningProcesses = int(v.Value)
		}
	}
	return u, nil
}
