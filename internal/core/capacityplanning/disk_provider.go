package capacityplanning

import "fmt"

const (
	DefaultDiskWarningPercent  = 75.0
	DefaultDiskCriticalPercent = 90.0
)

// DiskProvider assesses disk capacity across every filesystem on a node.
// Utilization and status follow the most-utilized mount — the one closest
// to full is the one that determines whether the node is at risk — while
// remaining headroom sums free space across every mount, the total room
// left on the node.
type DiskProvider struct {
	WarningPercent  float64
	CriticalPercent float64
}

// NewDiskProvider builds a DiskProvider. Zero-valued thresholds default to
// [DefaultDiskWarningPercent] and [DefaultDiskCriticalPercent].
func NewDiskProvider(warning, critical float64) DiskProvider {
	if warning <= 0 {
		warning = DefaultDiskWarningPercent
	}
	if critical <= 0 {
		critical = DefaultDiskCriticalPercent
	}
	return DiskProvider{WarningPercent: warning, CriticalPercent: critical}
}

func (p DiskProvider) Name() string { return DomainDisk }

func (p DiskProvider) Assess(s Snapshot) Domain {
	if len(s.Disks) == 0 {
		return Domain{Detail: "no disk utilization data"}
	}

	var worstPercent float64
	var worstMount string
	var remainingGB float64
	for _, d := range s.Disks {
		if d.UtilizationPercent > worstPercent {
			worstPercent, worstMount = d.UtilizationPercent, d.Mountpoint
		}
		remainingGB += max((d.TotalBytes-d.UsedBytes)/bytesPerGB, 0)
	}

	return Domain{
		Available: true, UtilizationPercent: worstPercent,
		RemainingCapacity: remainingGB, RemainingUnit: "GB",
		Status: statusFor(worstPercent, p.WarningPercent, p.CriticalPercent),
		Detail: fmt.Sprintf("most utilized mount %q at %.1f%%, %.1f GB free across %d mount(s)",
			worstMount, worstPercent, remainingGB, len(s.Disks)),
	}
}
