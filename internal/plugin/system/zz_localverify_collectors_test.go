package system

import (
	"context"
	"encoding/json"
	"testing"
)

// Dumps the real host's network identity and host facts as the agent would
// actually collect them — direct evidence for goal §7/§8, run against
// whatever machine executes this test suite.
func TestLocalVerifyNetworkIdentityRealOutput(t *testing.T) {
	got, err := NewProvider().NetworkIdentity(context.Background())
	if err != nil {
		t.Fatalf("NetworkIdentity: %v", err)
	}
	raw, _ := json.MarshalIndent(got, "", "  ")
	t.Logf("NetworkIdentity JSON:\n%s", raw)

	if len(got.Interfaces) == 0 {
		t.Fatal("no interfaces reported")
	}
	for _, iface := range got.Interfaces {
		t.Logf("interface: name=%q up=%v loopback=%v mac=%q mtu=%d ipv4=%v ipv6=%v flags=%v",
			iface.Name, iface.Up, iface.Loopback, iface.MAC, iface.MTU, iface.IPv4, iface.IPv6, iface.Flags)
	}
	for _, gw := range got.Gateways {
		t.Logf("gateway: family=%q address=%q interface=%q", gw.Family, gw.Address, gw.Interface)
	}
	t.Logf("dns servers: %v", got.DNSServers)
	t.Logf("dns search: %v", got.DNSSearch)

	if len(got.Gateways) == 0 {
		t.Log("NOTE: no default gateway found on this host/sandbox — expected in some CI/container network namespaces, not a failure")
	}
	if len(got.DNSServers) == 0 {
		t.Log("NOTE: no DNS servers found (e.g. /etc/resolv.conf absent or empty) — expected on some hosts, not a failure")
	}
}

func TestLocalVerifyHostFactsRealOutput(t *testing.T) {
	got, err := NewProvider().Host(context.Background())
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	raw, _ := json.MarshalIndent(got, "", "  ")
	t.Logf("HostInfo JSON:\n%s", raw)

	if got.Hostname == "" {
		t.Error("hostname missing")
	}
	if got.OS == "" {
		t.Error("os missing")
	}
	if got.KernelArch == "" {
		t.Error("kernel_arch missing")
	}
	if got.LogicalCores <= 0 {
		t.Error("logical_cores missing")
	}
	if got.BootTime.IsZero() {
		t.Error("boot_time missing")
	}
	t.Logf("uptime: %s", got.Uptime())
	if got.CPUModel == "" {
		t.Log("NOTE: cpu_model empty on this host/sandbox — best-effort field, not a failure")
	}
	if got.Timezone == "" {
		t.Error("timezone missing — expected on every host")
	}
	if got.FQDN == "" {
		t.Log("NOTE: fqdn empty — normal for a host with no resolvable domain")
	}
	if got.Virtualization == "" {
		t.Log("NOTE: virtualization empty — normal on bare metal or undetectable environments")
	}
}
