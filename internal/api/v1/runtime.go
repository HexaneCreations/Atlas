package v1

import "runtime"

const bytesPerMB = 1024 * 1024

// currentProcessRuntime samples the Go runtime.
//
// It uses runtime.ReadMemStats, which briefly stops the world. That is
// acceptable here because the endpoint is polled by dashboards at human
// timescales, not by collectors on a fifteen-second loop. If this ever moves
// onto a collection path, it should switch to runtime/metrics, which samples
// without the pause.
func currentProcessRuntime() ProcessRuntime {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return ProcessRuntime{
		Goroutines:  runtime.NumGoroutine(),
		HeapAllocMB: mem.HeapAlloc / bytesPerMB,
		HeapSysMB:   mem.HeapSys / bytesPerMB,
		GCCycles:    mem.NumGC,
		NumCPU:      runtime.NumCPU(),
		MaxProcs:    runtime.GOMAXPROCS(0),
	}
}
