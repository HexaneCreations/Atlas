package capacityplanning

const bytesPerGB = 1 << 30

// statusFor classifies value against warning and critical thresholds, both
// on the same scale as value (typically a percentage).
func statusFor(value, warning, critical float64) Status {
	switch {
	case value >= critical:
		return StatusCritical
	case value >= warning:
		return StatusWarning
	default:
		return StatusHealthy
	}
}
