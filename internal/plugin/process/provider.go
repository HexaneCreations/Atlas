// Package process observes running processes and binaries.
//
// # The cardinality trap
//
// This plugin is the one most likely to destroy a metrics datastore, and the
// mistake is seductive: a PID makes an obvious label. It is also unbounded —
// a host churns through thousands of PIDs an hour, each creating a series that
// is written once and never again. That is precisely the failure the
// scheduler's cardinality budget exists to catch, and a collector should not
// be relying on the safety net.
//
// So processes are reported two ways, neither of them per-PID:
//
//   - Aggregates by state, thread count, and a zombie count. Bounded by the
//     number of states, which is a handful.
//   - The top few consumers, labelled by process *name*. Bounded by the limit
//     per run, and names repeat across runs rather than being unique.
//
// The full per-process list is served live from the API instead, where it is
// inventory rather than a time series — the same treatment containers get, and
// for the same reason.
package process

import (
	"context"
	"time"
)

// Provider supplies process facts.
//
// Implementations must honour the context. Reading /proc entails thousands of
// small file reads on a busy host, and a process that exits mid-read returns
// an error that must not abort the sweep.
type Provider interface {
	// Processes returns a snapshot of every process the caller may see.
	Processes(ctx context.Context) ([]Process, error)
}

// State is a process's scheduler state, normalised across platforms.
type State string

const (
	StateRunning  State = "running"
	StateSleeping State = "sleeping"
	StateStopped  State = "stopped"
	// StateZombie is a process that has exited but whose parent has not
	// reaped it. A rising zombie count means a parent is not calling wait(),
	// which eventually exhausts the PID table.
	StateZombie State = "zombie"
	StateIdle   State = "idle"
	StateOther  State = "other"
)

// Process is one running process.
type Process struct {
	PID  int32
	PPID int32
	// Name is the executable name, and the only identifier stable enough to
	// use as a metric label.
	Name string
	// Executable is the full path to the binary. Reported in the live
	// inventory, never as a label.
	Executable string
	// Cmdline is the full command line, truncated. Inventory only — it can
	// contain credentials passed as arguments.
	Cmdline  string
	Username string

	State State

	CPUPercent    float64
	MemoryRSS     uint64
	MemoryPercent float64
	Threads       int32

	CreatedAt time.Time
}

// RunningFor returns how long the process has been alive.
func (p Process) RunningFor() time.Duration {
	if p.CreatedAt.IsZero() {
		return 0
	}
	return time.Since(p.CreatedAt)
}
