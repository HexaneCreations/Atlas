package goldensignals

// LatencyProvider always reports unavailable.
//
// Atlas collects no request- or operation-level duration for monitored
// infrastructure: no tracing, no per-request instrumentation, and the one
// place a duration is timed (a collector's own run, [scheduler.CollectorHealth])
// measures Atlas monitoring itself, not the host or service being observed.
// Inventing a number here would be exactly the fabrication this project
// refuses to do — see docs/context/ENGINEERING_GUIDE.md.
type LatencyProvider struct{}

func (LatencyProvider) Name() SignalName { return SignalLatency }

func (LatencyProvider) Measure(Inputs) Signal {
	return Signal{Detail: "Atlas collects no latency telemetry for monitored infrastructure"}
}
