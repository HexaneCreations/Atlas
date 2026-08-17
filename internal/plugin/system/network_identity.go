package system

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"strconv"
	"strings"
)

// resolvConfPath is where the resolver configuration lives on both supported
// platforms. A host whose resolver is managed elsewhere (systemd-resolved
// with a stub, a container with none) simply yields no servers.
const resolvConfPath = "/etc/resolv.conf"

// procRoutePath and procRoute6Path are Linux's routing tables. They are read
// by presence rather than by build tag: the agent ships as one binary for
// many hosts, and a platform without them must degrade to "no gateway
// known", not fail to compile or error.
const (
	procRoutePath  = "/proc/net/route"
	procRoute6Path = "/proc/net/ipv6_route"
)

func (gopsutilProvider) NetworkIdentity(ctx context.Context) (NetworkIdentity, error) {
	out := NetworkIdentity{Interfaces: readInterfaces()}
	if ctx.Err() != nil {
		return out, nil
	}

	out.Gateways = readGateways()
	out.DNSServers, out.DNSSearch = readResolvConf(resolvConfPath)
	return out, nil
}

// readInterfaces enumerates addressing per interface. Interfaces come and go
// (a container starting, a VPN connecting), so a read that fails entirely
// yields nothing rather than an error — the rest of the identity is still
// worth reporting.
func readInterfaces() []NetworkInterface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	out := make([]NetworkInterface, 0, len(ifaces))
	for _, iface := range ifaces {
		ni := NetworkInterface{
			Name:     iface.Name,
			Up:       iface.Flags&net.FlagUp != 0,
			Loopback: iface.Flags&net.FlagLoopback != 0,
			MAC:      iface.HardwareAddr.String(),
			MTU:      iface.MTU,
			Flags:    interfaceFlags(iface.Flags),
		}

		// Addresses require a second syscall per interface and are the part
		// most likely to be refused in a restricted namespace.
		if addrs, err := iface.Addrs(); err == nil {
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok {
					continue
				}
				if ipNet.IP.To4() != nil {
					ni.IPv4 = append(ni.IPv4, ipNet.String())
					continue
				}
				ni.IPv6 = append(ni.IPv6, ipNet.String())
			}
		}
		out = append(out, ni)
	}
	return out
}

func interfaceFlags(flags net.Flags) []string {
	var out []string
	for _, f := range []struct {
		flag net.Flags
		name string
	}{
		{net.FlagUp, "up"},
		{net.FlagBroadcast, "broadcast"},
		{net.FlagLoopback, "loopback"},
		{net.FlagPointToPoint, "point-to-point"},
		{net.FlagMulticast, "multicast"},
		{net.FlagRunning, "running"},
	} {
		if flags&f.flag != 0 {
			out = append(out, f.name)
		}
	}
	return out
}

func readGateways() []Gateway {
	var out []Gateway
	out = append(out, parseProcRoute(procRoutePath)...)
	out = append(out, parseProcRoute6(procRoute6Path)...)
	return out
}

// parseProcRoute reads Linux's IPv4 routing table, whose destination and
// gateway columns are little-endian hex words.
func parseProcRoute(path string) []Gateway {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []Gateway
	scanner := bufio.NewScanner(f)
	scanner.Scan() // header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		// Destination 00000000 is the default route; anything else is a
		// specific network and not what "the gateway" means to an operator.
		if fields[1] != "00000000" {
			continue
		}
		ip, ok := parseLittleEndianHexIPv4(fields[2])
		if !ok || ip.IsUnspecified() {
			continue
		}
		out = append(out, Gateway{Family: "ipv4", Address: ip.String(), Interface: fields[0]})
	}
	return out
}

func parseLittleEndianHexIPv4(s string) (net.IP, bool) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return nil, false
	}
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(v))
	return net.IP(buf), true
}

// parseProcRoute6 reads Linux's IPv6 routing table. Its columns are
// big-endian hex without separators; the default route is prefix length 0.
func parseProcRoute6(path string) []Gateway {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []Gateway
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		if fields[1] != "00" {
			continue
		}
		ip, ok := parseHexIPv6(fields[4])
		if !ok || ip.IsUnspecified() {
			continue
		}
		out = append(out, Gateway{Family: "ipv6", Address: ip.String(), Interface: fields[9]})
	}
	return out
}

func parseHexIPv6(s string) (net.IP, bool) {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != net.IPv6len {
		return nil, false
	}
	return net.IP(raw), true
}

func readResolvConf(path string) (servers, search []string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			servers = append(servers, fields[1])
		case "search", "domain":
			search = append(search, fields[1:]...)
		}
	}
	return servers, search
}
