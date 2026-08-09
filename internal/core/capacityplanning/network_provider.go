package capacityplanning

import "fmt"

const (
	DefaultNetworkWarningPercent  = 75.0
	DefaultNetworkCriticalPercent = 90.0
)

// NetworkProvider assesses network utilization against a configured link
// capacity. Atlas has no way to discover a link's actual capacity — an
// interface's negotiated speed is not among the metrics collected — so
// CapacityBytesPerSec must be configured for this domain to be available at
// all; left unconfigured, it honestly reports unavailable rather than
// inventing a ceiling.
type NetworkProvider struct {
	// CapacityBytesPerSec is the assumed link capacity (received plus sent).
	// Zero (the default) means unconfigured.
	CapacityBytesPerSec float64
	WarningPercent      float64
	CriticalPercent     float64
}

// NewNetworkProvider builds a NetworkProvider. Zero-valued thresholds
// default to [DefaultNetworkWarningPercent] and
// [DefaultNetworkCriticalPercent]; capacityBytesPerSec is not defaulted —
// see the type doc.
func NewNetworkProvider(capacityBytesPerSec, warning, critical float64) NetworkProvider {
	if warning <= 0 {
		warning = DefaultNetworkWarningPercent
	}
	if critical <= 0 {
		critical = DefaultNetworkCriticalPercent
	}
	return NetworkProvider{CapacityBytesPerSec: capacityBytesPerSec, WarningPercent: warning, CriticalPercent: critical}
}

func (p NetworkProvider) Name() string { return DomainNetwork }

func (p NetworkProvider) Assess(s Snapshot) Domain {
	if !s.HasNetwork {
		return Domain{Detail: "no network throughput data"}
	}

	throughput := s.NetworkRxBytesPerSec + s.NetworkTxBytesPerSec
	if p.CapacityBytesPerSec <= 0 {
		return Domain{Detail: fmt.Sprintf("%.0f B/s observed; no link capacity configured", throughput)}
	}

	pct := throughput / p.CapacityBytesPerSec * 100
	remaining := max((p.CapacityBytesPerSec-throughput)/bytesPerGB, 0)

	return Domain{
		Available: true, UtilizationPercent: pct,
		RemainingCapacity: remaining, RemainingUnit: "GB/s",
		Status: statusFor(pct, p.WarningPercent, p.CriticalPercent),
		Detail: fmt.Sprintf("%.0f of %.0f B/s in use", throughput, p.CapacityBytesPerSec),
	}
}
