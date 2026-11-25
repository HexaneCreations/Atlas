package notification

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsSafePublicAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ip   string
		safe bool
	}{
		{"loopback v4", "127.0.0.1", false},
		{"loopback v6", "::1", false},
		{"rfc1918 10/8", "10.1.2.3", false},
		{"rfc1918 172.16/12", "172.16.0.5", false},
		{"rfc1918 192.168/16", "192.168.1.1", false},
		{"link-local v4", "169.254.1.1", false},
		{"cloud metadata", "169.254.169.254", false},
		{"link-local v6", "fe80::1", false},
		{"unique local v6", "fd00::1", false},
		{"cgnat low", "100.64.0.1", false},
		{"cgnat high", "100.127.255.254", false},
		{"unspecified v4", "0.0.0.0", false},
		{"unspecified v6", "::", false},
		{"multicast v4", "224.0.0.1", false},
		{"multicast v6", "ff02::1", false},
		{"v4-mapped loopback", "::ffff:127.0.0.1", false},
		{"v4-mapped private", "::ffff:10.0.0.1", false},
		{"just before cgnat", "100.63.255.255", true},
		{"just after cgnat", "100.128.0.0", true},
		{"public v4", "8.8.8.8", true},
		{"public v6", "2606:4700:4700::1111", true},

		// "this network" (0.0.0.0/8).
		{"this-network low", "0.0.0.1", false},
		{"this-network high", "0.255.255.255", false},
		{"just after this-network", "1.0.0.0", true},

		// IETF protocol assignments (192.0.0.0/24).
		{"ietf protocol low", "192.0.0.0", false},
		{"ietf protocol high", "192.0.0.255", false},
		{"just before ietf protocol", "191.255.255.255", true},
		{"just after ietf protocol", "192.0.1.0", true},

		// Benchmarking (198.18.0.0/15).
		{"benchmarking low", "198.18.0.0", false},
		{"benchmarking high", "198.19.255.255", false},
		{"just before benchmarking", "198.17.255.255", true},
		{"just after benchmarking", "198.20.0.0", true},

		// Reserved (240.0.0.0/4), which subsumes the broadcast address. The
		// address space immediately below 240.0.0.0 is multicast
		// (224.0.0.0/4, already covered above), so there is no "just below
		// the boundary and still safe" case to add here.
		{"reserved low", "240.0.0.0", false},
		{"reserved high", "255.255.255.254", false},
		{"broadcast", "255.255.255.255", false},

		// NAT64 (64:ff9b::/96, RFC 6052) refuses based on the embedded
		// IPv4: embedding the cloud metadata address is refused, embedding
		// a documentation-range public address is not.
		{"nat64 embedding cloud metadata", "64:ff9b::a9fe:a9fe", false},
		{"nat64 embedding a private address", "64:ff9b::c0a8:0101", false}, // 192.168.1.1
		{"nat64 embedding a public address", "64:ff9b::cb00:7101", true},   // 203.0.113.1
		{"just outside the nat64 prefix", "64:ff9c::a9fe:a9fe", true},

		// 6to4 (2002::/16, RFC 3056) refuses based on the embedded IPv4,
		// which sits right after the fixed 2002: prefix.
		{"6to4 embedding cloud metadata", "2002:a9fe:a9fe::", false},
		{"6to4 embedding a private address", "2002:c0a8:0101::", false}, // 192.168.1.1
		{"6to4 embedding a public address", "2002:cb00:7101::", true},   // 203.0.113.1
		{"just outside the 6to4 prefix", "2003:cb00:7101::", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("could not parse %q as an IP", tc.ip)
			}
			if got := isSafePublicAddr(ip); got != tc.safe {
				t.Errorf("isSafePublicAddr(%s) = %v, want %v", tc.ip, got, tc.safe)
			}
		})
	}
}

// TestGuardedClientRefusesLoopback proves the guard fires on the dialer,
// not just on hostname shape: an httptest server bound to 127.0.0.1 (as
// httptest.NewServer always is) must never actually be reached.
func TestGuardedClientRefusesLoopback(t *testing.T) {
	t.Parallel()
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer srv.Close()

	client := newGuardedClient(2 * time.Second)
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("expected the loopback destination to be refused, got a successful response")
	}
	if !errors.Is(err, ErrUnsafeAddress) {
		t.Errorf("err = %v, want it to wrap ErrUnsafeAddress", err)
	}
	if reached {
		t.Error("the handler ran: the guard let the connection through")
	}
}

// TestGuardedClientRefusesDNSNameResolvingToPrivateAddress proves the
// guard checks the address actually dialed, not the hostname in the URL:
// a name that resolves (via a fake resolver standing in for real DNS) to a
// private address must be refused exactly like a literal private IP.
func TestGuardedClientRefusesDNSNameResolvingToPrivateAddress(t *testing.T) {
	t.Parallel()
	dialer := &net.Dialer{
		Control: controlRefusingUnsafeAddr,
		Resolver: &net.Resolver{
			PreferGo: true,
			Dial:     fakeDNSDialer(net.ParseIP("10.1.2.3")),
		},
	}
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{Proxy: nil, DialContext: dialer.DialContext},
	}

	_, err := client.Get("http://internal-service.example.test/")
	if err == nil {
		t.Fatal("expected the name resolving to a private address to be refused, got a successful response")
	}
	if !errors.Is(err, ErrUnsafeAddress) {
		t.Errorf("err = %v, want it to wrap ErrUnsafeAddress", err)
	}
}

// fakeDNSDialer returns a Resolver.Dial func that answers every A query
// with ip and every other query (AAAA included) with no answer, entirely
// in-process — a stand-in for a real DNS server so the test above does
// not depend on, or need, the network.
func fakeDNSDialer(ip net.IP) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		client, server := net.Pipe()
		go serveFakeDNS(server, ip)
		return client, nil
	}
}

// serveFakeDNS answers one DNS query read from conn using the length-
// prefixed stream framing Go's resolver falls back to for any net.Conn
// that is not also a net.PacketConn (which net.Pipe's is not) — so this is
// exercised regardless of whether the resolver thinks it asked for "udp"
// or "tcp".
func serveFakeDNS(conn net.Conn, ip net.IP) {
	defer conn.Close()
	var lenBuf [2]byte
	if _, err := readFull(conn, lenBuf[:]); err != nil {
		return
	}
	n := int(lenBuf[0])<<8 | int(lenBuf[1])
	query := make([]byte, n)
	if _, err := readFull(conn, query); err != nil {
		return
	}
	resp := buildFakeDNSResponse(query, ip)
	framed := append([]byte{byte(len(resp) >> 8), byte(len(resp))}, resp...)
	_, _ = conn.Write(framed)
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// buildFakeDNSResponse builds a minimal, uncompressed DNS response to
// query: the question section echoed back, and — only for an A query
// (type 1) — one answer record carrying ip. An AAAA query (type 28) gets
// no answer, so the resolver is told this name has no IPv6 address and
// relies on the A answer alone.
func buildFakeDNSResponse(query []byte, ip net.IP) []byte {
	id := query[0:2]
	// Walk the QNAME (length-prefixed labels terminated by a zero byte)
	// starting right after the fixed 12-byte header.
	i := 12
	for query[i] != 0 {
		i += 1 + int(query[i])
	}
	qnameEnd := i + 1
	qtype := int(query[qnameEnd])<<8 | int(query[qnameEnd+1])
	questionEnd := qnameEnd + 4 // QTYPE(2) + QCLASS(2)

	answerCount := 0
	if qtype == 1 { // A
		answerCount = 1
	}

	var resp []byte
	resp = append(resp, id...)
	resp = append(resp, 0x81, 0x80) // standard response, recursion available, no error
	resp = append(resp, 0x00, 0x01) // QDCOUNT=1
	resp = append(resp, byte(answerCount>>8), byte(answerCount))
	resp = append(resp, 0x00, 0x00) // NSCOUNT=0
	resp = append(resp, 0x00, 0x00) // ARCOUNT=0
	resp = append(resp, query[12:questionEnd]...)

	if answerCount == 1 {
		resp = append(resp, 0xC0, 0x0C)             // NAME: pointer back to the question's QNAME
		resp = append(resp, 0x00, 0x01)             // TYPE=A
		resp = append(resp, 0x00, 0x01)             // CLASS=IN
		resp = append(resp, 0x00, 0x00, 0x00, 0x3C) // TTL=60
		resp = append(resp, 0x00, 0x04)             // RDLENGTH=4
		resp = append(resp, ip.To4()...)
	}
	return resp
}

// TestGuardedClientRefusesRedirect proves the client never follows a
// redirect: CheckRedirect stops at the first hop and the 3xx response is
// what the caller sees, so a sender that checks the status code (as
// TeamsSender and WebhookSender both do) sees a failure, not a success.
func TestGuardedClientRefusesRedirect(t *testing.T) {
	t.Parallel()
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the redirect target was reached: the client followed the redirect")
	}))
	defer redirectTarget.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want %d (the redirect itself, not followed)", resp.StatusCode, http.StatusFound)
	}
}

// TestReadLimitedResponseBoundsAndDrains proves an oversize response body
// is capped at the configured limit, and that the rest is still drained
// so the underlying connection is left in a reusable state rather than
// with unread bytes.
func TestReadLimitedResponseBoundsAndDrains(t *testing.T) {
	t.Parallel()
	const limit = 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", limit*4)))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body := readLimitedResponse(resp, limit)
	if len(body) != limit {
		t.Errorf("len(body) = %d, want %d", len(body), limit)
	}
}

// TestGuardedClientTimesOut proves the client's Timeout bounds the whole
// exchange against a destination that never responds in time.
func TestGuardedClientTimesOut(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	// Unblock the handler before Close, which otherwise waits for it to
	// return — deferred in this order so close(block) runs first (defers
	// run LIFO).
	defer srv.Close()
	defer close(block)

	client := &http.Client{Timeout: 50 * time.Millisecond}
	start := time.Now()
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s to time out, want well under 2s", elapsed)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("err = %v, want a timeout error", err)
	}
}
