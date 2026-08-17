package system

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestParseProcRouteFindsOnlyTheDefaultRoute(t *testing.T) {
	t.Parallel()

	// Columns as the kernel writes them: destination and gateway are
	// little-endian hex. 0102A8C0 is 192.168.2.1.
	path := writeTemp(t, "route", `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0102A8C0	0003	0	0	100	00000000	0	0	0
eth0	0002A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
`)

	got := parseProcRoute(path)

	if len(got) != 1 {
		t.Fatalf("got %d gateways, want only the default route: %+v", len(got), got)
	}
	if got[0].Address != "192.168.2.1" {
		t.Errorf("address = %q, want 192.168.2.1", got[0].Address)
	}
	if got[0].Interface != "eth0" {
		t.Errorf("interface = %q, want eth0", got[0].Interface)
	}
	if got[0].Family != "ipv4" {
		t.Errorf("family = %q, want ipv4", got[0].Family)
	}
}

func TestParseProcRoute6FindsDefaultRoute(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "ipv6_route",
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe80000000000000021122fffe334455 00000400 00000001 00000000 00000003 eth0\n"+
			"fd0000000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000002 00000000 00000001 eth0\n")

	got := parseProcRoute6(path)

	if len(got) != 1 {
		t.Fatalf("got %d gateways, want only the default route: %+v", len(got), got)
	}
	if got[0].Address != "fe80::211:22ff:fe33:4455" {
		t.Errorf("address = %q, want fe80::211:22ff:fe33:4455", got[0].Address)
	}
	if got[0].Family != "ipv6" {
		t.Errorf("family = %q, want ipv6", got[0].Family)
	}
}

// A host with no /proc (macOS, a restricted container) must report no
// gateway rather than failing — the binary is the same everywhere.
func TestParseProcRouteMissingFileYieldsNothing(t *testing.T) {
	t.Parallel()

	if got := parseProcRoute(filepath.Join(t.TempDir(), "absent")); got != nil {
		t.Errorf("got %+v, want nil for a missing route table", got)
	}
	if got := parseProcRoute6(filepath.Join(t.TempDir(), "absent")); got != nil {
		t.Errorf("got %+v, want nil for a missing ipv6 route table", got)
	}
}

func TestReadResolvConf(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "resolv.conf", `# Managed by something
nameserver 10.0.0.2
nameserver 2001:4860:4860::8888
search internal.example.com example.com
options edns0 trust-ad
`)

	servers, search := readResolvConf(path)

	if len(servers) != 2 || servers[0] != "10.0.0.2" || servers[1] != "2001:4860:4860::8888" {
		t.Errorf("servers = %v, want both nameservers in order", servers)
	}
	if len(search) != 2 || search[0] != "internal.example.com" {
		t.Errorf("search = %v, want both search domains in order", search)
	}
}

func TestReadResolvConfMissingFileYieldsNothing(t *testing.T) {
	t.Parallel()

	servers, search := readResolvConf(filepath.Join(t.TempDir(), "absent"))
	if servers != nil || search != nil {
		t.Errorf("got servers=%v search=%v, want nothing when the file is absent", servers, search)
	}
}

// Runs against the real host: whatever this machine is, enumeration must
// succeed and produce a coherent loopback entry, since that is the one
// interface every supported platform has.
func TestNetworkIdentityReadsThisHost(t *testing.T) {
	t.Parallel()

	got, err := NewProvider().NetworkIdentity(context.Background())
	if err != nil {
		t.Fatalf("NetworkIdentity: %v", err)
	}
	if len(got.Interfaces) == 0 {
		t.Fatal("no interfaces reported; every host has at least loopback")
	}

	var loopback *NetworkInterface
	for i := range got.Interfaces {
		if got.Interfaces[i].Loopback {
			loopback = &got.Interfaces[i]
			break
		}
	}
	if loopback == nil {
		t.Fatalf("no loopback interface among %d reported", len(got.Interfaces))
	}
	if loopback.Name == "" {
		t.Error("loopback interface has no name")
	}
	if len(loopback.IPv4) == 0 && len(loopback.IPv6) == 0 {
		t.Error("loopback interface has no addresses")
	}
	for _, addr := range loopback.IPv4 {
		if _, _, err := net.ParseCIDR(addr); err != nil {
			t.Errorf("IPv4 address %q is not in CIDR form: %v", addr, err)
		}
	}
}

// Runs against the real host. CPU model and virtualization are best-effort
// by design — a restricted container may expose neither — so this asserts
// the facts that every supported platform can answer, and that the
// best-effort ones never fail the read.
func TestHostReportsTopologyAndVirtualizationFacts(t *testing.T) {
	t.Parallel()

	got, err := NewProvider().Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if got.Hostname == "" {
		t.Error("hostname is empty")
	}
	if got.LogicalCores <= 0 {
		t.Errorf("logical cores = %d, want a positive count", got.LogicalCores)
	}
	if got.Timezone == "" {
		t.Error("timezone is empty; every host has a configured zone")
	}
	if got.CPUSockets < 0 {
		t.Errorf("cpu sockets = %d, want zero or more", got.CPUSockets)
	}
}
