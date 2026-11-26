package safehttp

import (
	"errors"
	"net"
	"testing"
)

func TestAllowed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ip              string
		public, private bool // allowed without / with allowPrivate
	}{
		{"8.8.8.8", true, true},
		{"2606:4700:4700::1111", true, true},
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"10.1.2.3", false, true},
		{"172.16.0.5", false, true},
		{"192.168.1.1", false, true},
		{"100.64.0.1", false, true},
		{"fd00::1", false, true},
		{"::ffff:10.0.0.1", false, true},
		{"64:ff9b::c0a8:0101", false, true}, // NAT64 of 192.168.1.1

		// Never allowed, whatever the setting.
		{"169.254.169.254", false, false},
		{"169.254.1.1", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"64:ff9b::a9fe:a9fe", false, false}, // NAT64 of the metadata address
		{"2002:a9fe:a9fe::", false, false},   // 6to4 of the metadata address
		{"fd00:ec2::254", false, false},
		{"fe80::1", false, false},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"0.1.2.3", false, false},
		{"224.0.0.1", false, false},
		{"ff02::1", false, false},
		{"240.0.0.1", false, false},
		{"255.255.255.255", false, false},
		{"198.18.0.1", false, false},
		{"192.0.0.8", false, false},
	} {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("parse %q", tc.ip)
		}
		if got := Allowed(ip, false); got != tc.public {
			t.Errorf("Allowed(%s, false) = %v, want %v", tc.ip, got, tc.public)
		}
		if got := Allowed(ip, true); got != tc.private {
			t.Errorf("Allowed(%s, true) = %v, want %v", tc.ip, got, tc.private)
		}
	}
}

func TestControlChecksTheDialedAddress(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		addr         string
		allowPrivate bool
		ok           bool
	}{
		{"8.8.8.8:443", false, true},
		{"127.0.0.1:80", false, false},
		{"127.0.0.1:80", true, true},
		{"[::1]:80", true, true},
		{"169.254.169.254:80", true, false},
		{"not-an-ip:80", true, false},
		{"missing-port", true, false},
	} {
		err := Control(c.allowPrivate)("tcp", c.addr, nil)
		if (err == nil) != c.ok {
			t.Errorf("Control(%v)(%s) = %v, want ok=%v", c.allowPrivate, c.addr, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrUnsafeAddress) {
			t.Errorf("Control(%s) error %v is not ErrUnsafeAddress", c.addr, err)
		}
	}
}
