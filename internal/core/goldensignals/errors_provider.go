package goldensignals

// ErrorsProvider measures network interface errors and drops (received
// plus sent) per second — the only continuous, honest error-rate telemetry
// Atlas collects for infrastructure. See [Snapshot.NetworkErrorsPerSec] for
// why alert and event frequency are deliberately not folded in here.
type ErrorsProvider struct{}

func (ErrorsProvider) Name() SignalName { return SignalErrors }

func (ErrorsProvider) Measure(in Inputs) Signal {
	if !in.Snapshot.HasNetworkErrors {
		return Signal{Detail: "no network error/drop data"}
	}
	return Signal{
		Available: true, Value: in.Snapshot.NetworkErrorsPerSec, Unit: "errors/sec",
		Detail: "network interface errors and drops, received plus sent",
	}
}
