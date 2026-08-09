// Package ports observes what this host is listening for connections on, and
// whether what answers speaks TLS.
//
// # Why this is not a per-PID inventory dressed up differently
//
// A listening socket is bound to a port, not to a process — SO_REUSEPORT lets
// several processes share one, and the process behind a port can be replaced
// by a restart without the port ever closing. So [Port] carries the process
// that currently owns the socket as an attribute, the same way a container
// carries a name, rather than treating the process as the identity of the
// thing being observed. The port is the identity.
//
// # Permission is normal, not an error
//
// Resolving *which* process owns a socket requires reading kernel state that
// belongs, on every platform Atlas targets, to whichever user started that
// process — the same boundary the process and cron plugins already document.
// Atlas running unprivileged will see every listening port but only the
// process names for its own. A production deployment that wants full
// attribution runs as root, the same trade-off made by every comparable
// agent; Atlas reports what it can see rather than refusing to report
// anything.
package ports

import "context"

// Provider supplies the host's listening sockets.
type Provider interface {
	// ListenPorts returns every socket accepting connections or bound to
	// receive them.
	ListenPorts(ctx context.Context) ([]Port, error)
}

// Protocol is a transport-layer protocol.
type Protocol string

const (
	ProtocolTCP Protocol = "tcp"
	ProtocolUDP Protocol = "udp"
)

// Port is one listening socket.
type Port struct {
	Protocol Protocol
	// Address is the local address the socket is bound to. "0.0.0.0" or
	// "::" means every interface; anything else is a deliberate bind an
	// operator chose, which is worth being able to see.
	Address string
	Port    uint32

	// PID and Process identify the current owner, when Atlas has permission
	// to see it. PID is zero and Process is empty otherwise — absence, not
	// an error.
	PID     int32
	Process string
}
