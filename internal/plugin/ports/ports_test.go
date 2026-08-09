package ports

import (
	"context"
	"net"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Two invariants matter more than anything else in this package: a plaintext
// service must never be reported as having a certificate, and the TLS probe
// budget must never grow with how many ports happen to be open — the same
// cardinality discipline every other plugin in Atlas enforces, applied here
// to network I/O instead of metric series.

func TestPortCollectorCountsByProtocol(t *testing.T) {
	provider := &fakeProvider{ports: []Port{
		{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: 22},
		{Protocol: ProtocolTCP, Address: "127.0.0.1", Port: 5432},
		{Protocol: ProtocolUDP, Address: "0.0.0.0", Port: 53},
	}}

	samples, err := newPortCollector(provider, nil, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	values := map[string]float64{}
	for _, s := range samples {
		key := s.Metric
		if proto, ok := s.Labels["protocol"]; ok {
			key += "{" + proto + "}"
		}
		values[key] = s.Value
	}

	want := map[string]float64{
		"port.listening.total":      3,
		"port.listening.count{tcp}": 2,
		"port.listening.count{udp}": 1,
	}
	for metric, wantValue := range want {
		if got := values[metric]; got != wantValue {
			t.Errorf("%s = %v, want %v", metric, got, wantValue)
		}
	}
}

func TestPortCollectorReportsWatchedPortsUpAndDown(t *testing.T) {
	provider := &fakeProvider{ports: []Port{
		{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: 5432},
	}}

	samples, err := newPortCollector(provider, []int{5432, 6379}, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	up := map[string]float64{}
	for _, s := range samples {
		if s.Metric == "port.watched.up" {
			up[s.Labels["port"]] = s.Value
		}
	}

	if up["5432"] != 1 {
		t.Errorf("port 5432 up = %v, want 1 (it is listening)", up["5432"])
	}
	if up["6379"] != 0 {
		t.Errorf("port 6379 up = %v, want 0 (nothing is listening there)", up["6379"])
	}
}

func TestPortCollectorWatchedSeriesStayBoundedRegardlessOfHowManyPortsAreOpen(t *testing.T) {
	// A host can have hundreds of ephemeral or unexpected listeners. Only the
	// operator-named watch list may become a permanent series — everything
	// else is inventory, never a label.
	ports := make([]Port, 0, 300)
	for i := range 300 {
		ports = append(ports, Port{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: uint32(20000 + i)})
	}
	provider := &fakeProvider{ports: ports}

	watch := []int{22, 80, 443}
	samples, err := newPortCollector(provider, watch, nil).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	perPortSeries := 0
	for _, s := range samples {
		if s.Metric == "port.watched.up" {
			perPortSeries++
		}
	}
	if perPortSeries != len(watch) {
		t.Errorf("got %d port.watched.up series for 300 open ports, want exactly %d (the watch list size)",
			perPortSeries, len(watch))
	}
}

func TestNormaliseAddressTranslatesLsofWildcard(t *testing.T) {
	// Found live: macOS's lsof-backed connection table prints a bare "*" for
	// a wildcard bind, where Linux's /proc reader gives 0.0.0.0 or :: for the
	// identical situation. Without this, the same "listens on every
	// interface" state would read differently depending on which OS Atlas
	// happened to be running on.
	if got := normaliseAddress("*"); got != "0.0.0.0" {
		t.Errorf("normaliseAddress(*) = %q, want 0.0.0.0", got)
	}
	if got := normaliseAddress("127.0.0.1"); got != "127.0.0.1" {
		t.Errorf("normaliseAddress should not touch a real address, got %q", got)
	}
}

func TestDialAddressTranslatesWildcardBinds(t *testing.T) {
	tests := []struct{ bind, want string }{
		{"0.0.0.0", "127.0.0.1:443"},
		{"::", "127.0.0.1:443"},
		{"", "127.0.0.1:443"},
		{"10.0.0.5", "10.0.0.5:443"},
		{"127.0.0.1", "127.0.0.1:443"},
	}
	for _, tc := range tests {
		if got := dialAddress(tc.bind, 443); got != tc.want {
			t.Errorf("dialAddress(%q, 443) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

func TestProbeTargetsPrioritisesWatchedPortsAndDropsAbsentOnes(t *testing.T) {
	live := []Port{
		{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: 9999},
		{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: 443}, // watched
		{Protocol: ProtocolUDP, Address: "0.0.0.0", Port: 53},  // never a TLS target
	}
	c := newTLSCollector(&fakeProvider{}, []int{443, 8443}, 10, newCertCache(), nil)

	targets := c.probeTargets(live)

	if _, ok := targets[443]; !ok {
		t.Error("watched port 443, which is listening, was not selected for probing")
	}
	if _, ok := targets[9999]; !ok {
		t.Error("discovered listening port 9999 was not selected for probing")
	}
	if _, ok := targets[53]; ok {
		t.Error("a UDP port was selected for a TLS probe")
	}
	// 8443 is watched but nothing is listening there this sweep — nothing to
	// probe, and critically nothing that should keep a stale cache entry
	// alive.
	if _, ok := targets[8443]; ok {
		t.Error("a watched port with no listener was selected for probing")
	}
}

func TestProbeTargetsRespectsTheBudgetRegardlessOfHowManyPortsAreOpen(t *testing.T) {
	live := make([]Port, 0, 500)
	for i := range 500 {
		live = append(live, Port{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: uint32(30000 + i)})
	}
	c := newTLSCollector(&fakeProvider{}, nil, 10, newCertCache(), nil)

	targets := c.probeTargets(live)
	if len(targets) != 10 {
		t.Errorf("got %d probe targets from 500 open ports, want the configured budget of 10", len(targets))
	}
}

func TestCertCacheReplaceDropsStaleEntries(t *testing.T) {
	cache := newCertCache()
	cache.replace(map[uint32]Certificate{443: {Subject: "old"}}, map[uint32]struct{}{443: {}})

	if _, ok := cache.get(443); !ok {
		t.Fatal("certificate not present after the first replace")
	}

	// A service that stopped answering TLS on this port — redeployed as
	// plaintext, or simply gone — must not leave a certificate behind that
	// looks current.
	cache.replace(map[uint32]Certificate{}, map[uint32]struct{}{443: {}})

	if _, ok := cache.get(443); ok {
		t.Error("stale certificate survived a replace that no longer found it")
	}

	// The port was still attempted, which is what distinguishes "this service
	// is plaintext now" from "Atlas stopped looking at it".
	if !cache.wasProbed(443) {
		t.Error("probe attempt not recorded after replace")
	}
}

// A port with no certificate must be distinguishable from a port that was
// never probed. Probing is budgeted and TCP-only, so both are routine, and a
// security view that conflates them reports absence of evidence as evidence
// of absence.
func TestInventoryDistinguishesUnprobedFromPlaintext(t *testing.T) {
	provider := &fakeProvider{ports: []Port{
		{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: 80, Process: "nginx"},
		{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: 9999, Process: "app"},
		{Protocol: ProtocolUDP, Address: "0.0.0.0", Port: 53, Process: "dns"},
	}}
	p := New(Options{Provider: provider})
	// 80 was probed and answered plaintext; 9999 was never reached.
	p.certs.replace(map[uint32]Certificate{}, map[uint32]struct{}{80: {}})

	out, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}

	byPort := map[uint32]Listener{}
	for _, l := range out {
		byPort[l.Socket.Port] = l
	}

	if !byPort[80].TLSProbed {
		t.Error("port 80 was probed but is not reported as such")
	}
	if byPort[9999].TLSProbed {
		t.Error("port 9999 was never probed but is reported as probed")
	}
	// UDP is never probed at all, and must not claim otherwise.
	if byPort[53].TLSProbed {
		t.Error("a UDP port is reported as TLS-probed")
	}
}

func TestInventoryMergesLivePortsWithCachedCertificates(t *testing.T) {
	provider := &fakeProvider{ports: []Port{
		{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: 443, Process: "nginx"},
		{Protocol: ProtocolTCP, Address: "0.0.0.0", Port: 22, Process: "sshd"},
		{Protocol: ProtocolUDP, Address: "0.0.0.0", Port: 53},
	}}

	p := New(Options{Provider: provider})
	p.certs.replace(map[uint32]Certificate{443: {Subject: "example.internal"}}, map[uint32]struct{}{443: {}, 22: {}})

	listeners, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(listeners) != 3 {
		t.Fatalf("got %d listeners, want 3", len(listeners))
	}

	// Sorted ascending by port: 22, 53, 443.
	if listeners[0].Socket.Port != 22 || listeners[0].TLS != nil {
		t.Errorf("position 0 = %+v, want port 22 with no certificate (ssh is not TLS)", listeners[0])
	}
	if listeners[1].Socket.Port != 53 || listeners[1].TLS != nil {
		t.Errorf("position 1 = %+v, want port 53 (UDP) with no certificate", listeners[1])
	}
	if listeners[2].Socket.Port != 443 || listeners[2].TLS == nil || listeners[2].TLS.Subject != "example.internal" {
		t.Errorf("position 2 = %+v, want port 443 carrying the cached certificate", listeners[2])
	}
}

func TestDetectFailsWhenTheConnectionTableCannotBeRead(t *testing.T) {
	p := New(Options{Provider: &fakeProvider{err: context.DeadlineExceeded}})
	if _, err := p.Detect(context.Background()); err == nil {
		t.Error("Detect swallowed a provider error")
	}

	p = New(Options{Provider: &fakeProvider{}})
	ok, err := p.Detect(context.Background())
	if err != nil || !ok {
		t.Errorf("Detect on a host with zero listening ports = (%v, %v), want (true, nil) — an empty list is still a real reading", ok, err)
	}
}

// probeCertificate is exercised against a real TLS listener rather than
// mocked: certificate parsing and the plaintext-fails-cleanly path are
// exactly the kind of thing that looks right in review and is wrong against
// an actual handshake.
func TestProbeCertificateReadsARealHandshake(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	cert, ok := probeCertificate(context.Background(), addr, 2*time.Second)
	if !ok {
		t.Fatal("probeCertificate found no certificate on a live TLS listener")
	}
	if cert.NotAfter.Before(time.Now()) {
		t.Errorf("NotAfter = %v, want a certificate that has not expired yet", cert.NotAfter)
	}
	if !cert.SelfSigned {
		t.Error("httptest's certificate is self-signed and should be reported as such")
	}
}

func TestProbeCertificateFailsCleanlyOnPlaintext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	// Accept and immediately close, so the handshake attempt has something
	// real to fail against rather than a connection refused.
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	_, ok := probeCertificate(context.Background(), ln.Addr().String(), 2*time.Second)
	if ok {
		t.Error("probeCertificate reported a certificate from a plaintext listener")
	}
}

func TestProbeCertificateFailsOnAnUnreachablePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens here now

	_, ok := probeCertificate(context.Background(), "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
	if ok {
		t.Error("probeCertificate reported a certificate from a closed port")
	}
}

func TestGopsutilProviderFindsARealListeningSocket(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	port := uint32(ln.Addr().(*net.TCPAddr).Port)

	ports, err := NewProvider().ListenPorts(context.Background())
	if err != nil {
		t.Fatalf("ListenPorts: %v", err)
	}

	var found *Port
	for i := range ports {
		if ports[i].Protocol == ProtocolTCP && ports[i].Port == port {
			found = &ports[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the socket this test just opened on port %d was not found among %d listening ports",
			port, len(ports))
	}
	// This process owns the socket, so its own PID must be resolvable — the
	// one case where permission can never be the reason a name is missing.
	if found.Process == "" {
		t.Error("could not resolve the process name of this test's own listening socket")
	}
}

// fakeProvider returns ports a test set.
type fakeProvider struct {
	ports []Port
	err   error
}

func (f *fakeProvider) ListenPorts(context.Context) ([]Port, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ports, nil
}

var _ Provider = (*fakeProvider)(nil)
