package capacityplanning

import "fmt"

const (
	DefaultMemoryWarningPercent  = 75.0
	DefaultMemoryCriticalPercent = 90.0
)

// MemoryProvider assesses memory capacity: utilization against total
// memory, remaining headroom in GB.
type MemoryProvider struct {
	WarningPercent  float64
	CriticalPercent float64
}

// NewMemoryProvider builds a MemoryProvider. Zero-valued thresholds default
// to [DefaultMemoryWarningPercent] and [DefaultMemoryCriticalPercent].
func NewMemoryProvider(warning, critical float64) MemoryProvider {
	if warning <= 0 {
		warning = DefaultMemoryWarningPercent
	}
	if critical <= 0 {
		critical = DefaultMemoryCriticalPercent
	}
	return MemoryProvider{WarningPercent: warning, CriticalPercent: critical}
}

func (p MemoryProvider) Name() string { return DomainMemory }

func (p MemoryProvider) Assess(s Snapshot) Domain {
	if !s.HasMemory || s.MemoryTotalBytes == 0 {
		return Domain{Detail: "no memory utilization data"}
	}

	remainingGB := max((s.MemoryTotalBytes-s.MemoryUsedBytes)/bytesPerGB, 0)

	return Domain{
		Available: true, UtilizationPercent: s.MemoryUtilizationPercent,
		RemainingCapacity: remainingGB, RemainingUnit: "GB",
		Status: statusFor(s.MemoryUtilizationPercent, p.WarningPercent, p.CriticalPercent),
		Detail: fmt.Sprintf("%.1f of %.1f GB in use", s.MemoryUsedBytes/bytesPerGB, s.MemoryTotalBytes/bytesPerGB),
	}
}
