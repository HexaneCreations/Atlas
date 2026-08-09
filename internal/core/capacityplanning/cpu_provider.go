package capacityplanning

import "fmt"

const (
	DefaultCPUWarningPercent  = 75.0
	DefaultCPUCriticalPercent = 90.0
)

// CPUProvider assesses CPU capacity: utilization against the node's core
// count, remaining headroom in cores.
type CPUProvider struct {
	WarningPercent  float64
	CriticalPercent float64
}

// NewCPUProvider builds a CPUProvider. Zero-valued thresholds default to
// [DefaultCPUWarningPercent] and [DefaultCPUCriticalPercent].
func NewCPUProvider(warning, critical float64) CPUProvider {
	if warning <= 0 {
		warning = DefaultCPUWarningPercent
	}
	if critical <= 0 {
		critical = DefaultCPUCriticalPercent
	}
	return CPUProvider{WarningPercent: warning, CriticalPercent: critical}
}

func (p CPUProvider) Name() string { return DomainCPU }

func (p CPUProvider) Assess(s Snapshot) Domain {
	if !s.HasCPU || s.CPUCores == 0 {
		return Domain{Detail: "no CPU utilization data"}
	}

	remaining := float64(s.CPUCores) * (1 - s.CPUUtilizationPercent/100)
	remaining = max(remaining, 0)

	return Domain{
		Available: true, UtilizationPercent: s.CPUUtilizationPercent,
		RemainingCapacity: remaining, RemainingUnit: "cores",
		Status: statusFor(s.CPUUtilizationPercent, p.WarningPercent, p.CriticalPercent),
		Detail: fmt.Sprintf("%.1f of %d cores in use", float64(s.CPUCores)*s.CPUUtilizationPercent/100, s.CPUCores),
	}
}
