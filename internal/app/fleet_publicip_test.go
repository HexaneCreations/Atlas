package app

import (
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

func TestObservedIP(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
		ok   bool
	}{
		{"ipv4 tcp", "/ip4/203.0.113.7/tcp/4001", "203.0.113.7", true},
		{"ipv4 quic", "/ip4/198.51.100.9/udp/4001/quic-v1", "198.51.100.9", true},
		{"ipv6 tcp", "/ip6/2001:db8::1/tcp/4001", "2001:db8::1", true},
		{"unspecified is rejected", "/ip4/0.0.0.0/tcp/4001", "", false},
		{"relayed circuit has no direct ip", "/ip4/203.0.113.7/tcp/4001/p2p-circuit", "", false},
		{"dns name is not an ip", "/dns4/example.com/tcp/443", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := ma.NewMultiaddr(tc.addr)
			if err != nil {
				t.Fatalf("build multiaddr %q: %v", tc.addr, err)
			}
			got, ok := observedIP(addr)
			if ok != tc.ok || got != tc.want {
				t.Errorf("observedIP(%q) = (%q, %v), want (%q, %v)", tc.addr, got, ok, tc.want, tc.ok)
			}
		})
	}

	if _, ok := observedIP(nil); ok {
		t.Error("observedIP(nil) reported ok")
	}
}
