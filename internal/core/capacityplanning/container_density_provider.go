package capacityplanning

import "fmt"

const (
	DefaultContainerDensityWarningPercent  = 75.0
	DefaultContainerDensityCriticalPercent = 90.0
)

// ContainerDensityProvider assesses running-container count against a
// configured maximum. Atlas does not know an operator's intended density
// limit, so MaxContainers must be configured for this domain to be
// available at all — see [NetworkProvider] for the same convention.
type ContainerDensityProvider struct {
	// MaxContainers is the configured density ceiling. Zero (the default)
	// means unconfigured.
	MaxContainers   int
	WarningPercent  float64
	CriticalPercent float64
}

// NewContainerDensityProvider builds a ContainerDensityProvider.
// Zero-valued thresholds default to
// [DefaultContainerDensityWarningPercent] and
// [DefaultContainerDensityCriticalPercent]; maxContainers is not defaulted.
func NewContainerDensityProvider(maxContainers int, warning, critical float64) ContainerDensityProvider {
	if warning <= 0 {
		warning = DefaultContainerDensityWarningPercent
	}
	if critical <= 0 {
		critical = DefaultContainerDensityCriticalPercent
	}
	return ContainerDensityProvider{MaxContainers: maxContainers, WarningPercent: warning, CriticalPercent: critical}
}

func (p ContainerDensityProvider) Name() string { return DomainContainerDensity }

func (p ContainerDensityProvider) Assess(s Snapshot) Domain {
	if !s.HasContainers {
		return Domain{Detail: "no container count data"}
	}
	if p.MaxContainers <= 0 {
		return Domain{Detail: fmt.Sprintf("%d running; no density ceiling configured", s.RunningContainers)}
	}

	pct := float64(s.RunningContainers) / float64(p.MaxContainers) * 100
	remaining := max(float64(p.MaxContainers-s.RunningContainers), 0)

	return Domain{
		Available: true, UtilizationPercent: pct,
		RemainingCapacity: remaining, RemainingUnit: "containers",
		Status: statusFor(pct, p.WarningPercent, p.CriticalPercent),
		Detail: fmt.Sprintf("%d of %d containers running", s.RunningContainers, p.MaxContainers),
	}
}
