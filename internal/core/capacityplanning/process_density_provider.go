package capacityplanning

import "fmt"

const (
	DefaultProcessDensityWarningPercent  = 75.0
	DefaultProcessDensityCriticalPercent = 90.0
)

// ProcessDensityProvider assesses running-process count against a
// configured maximum (a kernel PID limit, an operator-chosen ceiling).
// Atlas does not collect the host's PID limit today, so MaxProcesses must
// be configured for this domain to be available at all — see
// [NetworkProvider] for the same convention.
type ProcessDensityProvider struct {
	// MaxProcesses is the configured density ceiling. Zero (the default)
	// means unconfigured.
	MaxProcesses    int
	WarningPercent  float64
	CriticalPercent float64
}

// NewProcessDensityProvider builds a ProcessDensityProvider. Zero-valued
// thresholds default to [DefaultProcessDensityWarningPercent] and
// [DefaultProcessDensityCriticalPercent]; maxProcesses is not defaulted.
func NewProcessDensityProvider(maxProcesses int, warning, critical float64) ProcessDensityProvider {
	if warning <= 0 {
		warning = DefaultProcessDensityWarningPercent
	}
	if critical <= 0 {
		critical = DefaultProcessDensityCriticalPercent
	}
	return ProcessDensityProvider{MaxProcesses: maxProcesses, WarningPercent: warning, CriticalPercent: critical}
}

func (p ProcessDensityProvider) Name() string { return DomainProcessDensity }

func (p ProcessDensityProvider) Assess(s Snapshot) Domain {
	if !s.HasProcesses {
		return Domain{Detail: "no process count data"}
	}
	if p.MaxProcesses <= 0 {
		return Domain{Detail: fmt.Sprintf("%d running; no density ceiling configured", s.RunningProcesses)}
	}

	pct := float64(s.RunningProcesses) / float64(p.MaxProcesses) * 100
	remaining := max(float64(p.MaxProcesses-s.RunningProcesses), 0)

	return Domain{
		Available: true, UtilizationPercent: pct,
		RemainingCapacity: remaining, RemainingUnit: "processes",
		Status: statusFor(pct, p.WarningPercent, p.CriticalPercent),
		Detail: fmt.Sprintf("%d of %d processes running", s.RunningProcesses, p.MaxProcesses),
	}
}
