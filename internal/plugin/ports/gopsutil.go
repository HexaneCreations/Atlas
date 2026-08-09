package ports

import (
	"context"
	"strconv"
	"strings"
	"syscall"

	"github.com/hexane/atlas/internal/platform/errs"
	gopsnet "github.com/shirou/gopsutil/v4/net"
	gopsproc "github.com/shirou/gopsutil/v4/process"
)

// gopsutilProvider reads the connection table through gopsutil: /proc/net on
// Linux, lsof on macOS and the BSDs. See ADR-0012 for why gopsutil is the
// default rather than a platform-native reader from the start.
type gopsutilProvider struct{}

// NewProvider returns a Provider backed by gopsutil.
func NewProvider() Provider { return gopsutilProvider{} }

func (gopsutilProvider) ListenPorts(ctx context.Context) ([]Port, error) {
	const op = "ports.Provider.ListenPorts"

	// "inet" is tcp4+tcp6+udp4+udp6 — every IP socket, excluding local unix
	// sockets, which have no port and are not what "listening service" means
	// to an operator reading this list.
	conns, err := gopsnet.ConnectionsWithContext(ctx, "inet")
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list network connections").WithOp(op)
	}

	// SO_REUSEPORT lets several processes share one bound socket, and a
	// connection table can otherwise list the same port once per listener.
	// One row per port is what "listening service" means; which process
	// answers is secondary and, when they differ, ambiguous anyway.
	seen := make(map[string]bool, len(conns))
	out := make([]Port, 0, len(conns))

	for _, c := range conns {
		if ctx.Err() != nil {
			break
		}

		proto, ok := protocolOf(c.Type)
		if !ok {
			continue
		}
		// TCP has an explicit LISTEN state; a socket in any other state is a
		// connection, not something accepting new ones. UDP is connectionless
		// and gopsutil reports no status for it at all — every bound UDP
		// socket found here is, by definition, listening.
		if proto == ProtocolTCP && !strings.EqualFold(c.Status, "LISTEN") {
			continue
		}

		address := normaliseAddress(c.Laddr.IP)

		key := string(proto) + "|" + address + "|" + strconv.FormatUint(uint64(c.Laddr.Port), 10)
		if seen[key] {
			continue
		}
		seen[key] = true

		out = append(out, Port{
			Protocol: proto,
			Address:  address,
			Port:     c.Laddr.Port,
			PID:      c.Pid,
			Process:  processName(ctx, c.Pid),
		})
	}
	return out, nil
}

// normaliseAddress makes a wildcard bind read the same regardless of which
// platform reported it. Linux reports 0.0.0.0 or :: directly; macOS and the
// BSDs read the connection table through lsof, which prints a bare "*" for
// the same thing. Without this, "what is this port bound to" would have a
// platform-specific answer for the identical situation.
func normaliseAddress(addr string) string {
	if addr == "*" {
		return "0.0.0.0"
	}
	return addr
}

func protocolOf(sockType uint32) (Protocol, bool) {
	switch sockType {
	case syscall.SOCK_STREAM:
		return ProtocolTCP, true
	case syscall.SOCK_DGRAM:
		return ProtocolUDP, true
	default:
		return "", false
	}
}

// processName resolves a PID to a name, tolerating every way that can fail:
// no permission to see another user's process, or the process having already
// exited between the connection snapshot and this lookup. Either way, an
// empty name is the honest answer, not an error worth losing the port over.
func processName(ctx context.Context, pid int32) string {
	if pid <= 0 {
		return ""
	}
	proc, err := gopsproc.NewProcessWithContext(ctx, pid)
	if err != nil {
		return ""
	}
	name, err := proc.NameWithContext(ctx)
	if err != nil {
		return ""
	}
	return name
}

var _ Provider = gopsutilProvider{}
