package goldensignals

import "fmt"

// TrafficProvider measures network throughput: received plus sent bytes
// per second.
type TrafficProvider struct{}

func (TrafficProvider) Name() SignalName { return SignalTraffic }

func (TrafficProvider) Measure(in Inputs) Signal {
	if !in.Snapshot.HasNetworkTraffic {
		return Signal{Detail: "no network throughput data"}
	}
	rx, tx := in.Snapshot.NetworkRxBytesPerSec, in.Snapshot.NetworkTxBytesPerSec
	return Signal{
		Available: true, Value: rx + tx, Unit: "bytes/sec",
		Detail: fmt.Sprintf("rx %.0f B/s, tx %.0f B/s", rx, tx),
	}
}
