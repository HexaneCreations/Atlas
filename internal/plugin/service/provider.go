// Package service observes system services.
//
// systemd is the only backend implemented, because it is what every current
// Linux distribution uses. The [Provider] seam means adding OpenRC, launchd, or
// Windows services later is a new implementation rather than a change to the
// collector.
//
// Detection matters more here than for most plugins: a host without systemd —
// a macOS laptop, an Alpine container, a BSD box — must report *no service
// integration*, not a broken one.
package service

import (
	"context"
	"time"
)

// Provider supplies service facts.
type Provider interface {
	// Available reports whether this service manager is usable on this host.
	Available(ctx context.Context) bool
	// Units returns the services the manager knows about.
	Units(ctx context.Context) ([]Unit, error)
	// Structure returns the dependency graph across *every* unit type, not
	// just services.
	//
	// Services alone would produce a graph of dangling edges: on a plain
	// Debian host, 62 of 139 units are services and dependencies point
	// overwhelmingly at the other 77 — targets, sockets, mounts, slices.
	//
	// Separate from Units because it changes on a different timescale.
	// Structure changes when unit files change, which is to say on deploy;
	// state changes constantly. Collecting them apart is what lets the graph
	// be cached and the states over it stay fresh.
	Structure(ctx context.Context) (Structure, error)
}

// Structure is the dependency graph as collected, before live state is
// overlaid.
type Structure struct {
	Nodes []Node
	Edges []Edge
	// CollectedAt is when the manager was read.
	CollectedAt time.Time
}

// ActiveState is systemd's high-level state for a unit.
type ActiveState string

const (
	ActiveStateActive       ActiveState = "active"
	ActiveStateInactive     ActiveState = "inactive"
	ActiveStateFailed       ActiveState = "failed"
	ActiveStateActivating   ActiveState = "activating"
	ActiveStateDeactivating ActiveState = "deactivating"
	ActiveStateUnknown      ActiveState = "unknown"
)

// SubState is the unit-type-specific state — "running", "exited", "dead".
//
// Kept alongside ActiveState because the pair distinguishes cases that matter:
// a oneshot unit that is `active (exited)` succeeded and finished, while a
// service that is `active (running)` is still up. Collapsing them would report
// a completed backup job as if it were a running daemon.
type SubState string

// Unit is one service.
type Unit struct {
	Name        string
	Description string

	ActiveState ActiveState
	SubState    SubState
	// LoadState is whether the unit file was found and parsed: "loaded",
	// "not-found", "masked".
	LoadState string
	// Enabled reports whether the unit starts at boot. A service that is
	// running but not enabled will silently vanish on the next reboot, which
	// is a class of outage that is invisible until it happens.
	Enabled bool

	// MainPID is the service's main process, zero when not running.
	MainPID uint32
	// RestartCount is how many times systemd has restarted the unit, where
	// the manager reports it.
	RestartCount uint32

	ActiveSince time.Time
	// MemoryBytes and CPUSeconds come from the unit's cgroup, where the
	// manager exposes them.
	MemoryBytes uint64
	CPUSeconds  float64
}

// Failed reports whether the unit is in a failed state.
func (u Unit) Failed() bool { return u.ActiveState == ActiveStateFailed }

// Running reports whether the unit is active and its processes are up.
//
// A oneshot unit that exited successfully is active but not running, and
// reporting it as running would make a completed job look like a live daemon.
func (u Unit) Running() bool {
	return u.ActiveState == ActiveStateActive && u.SubState == "running"
}

// Uptime returns how long the unit has been active.
func (u Unit) Uptime() time.Duration {
	if u.ActiveSince.IsZero() {
		return 0
	}
	return time.Since(u.ActiveSince)
}
