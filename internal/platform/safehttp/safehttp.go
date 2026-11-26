// Package safehttp guards outbound connections to destinations that come
// from configuration rather than source code: notification channels and
// website monitors. The guard runs as a [net.Dialer] Control callback, after
// DNS resolution and once per candidate address, so it checks the IP that is
// actually being dialed: a DNS name cannot be used to reach an address the
// guard refuses, and every redirect hop is checked again because each hop
// dials again.
package safehttp

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrUnsafeAddress means a destination resolved to an address the guard
// refuses to dial.
var ErrUnsafeAddress = errors.New("refusing to dial a non-public address")

// Control returns a [net.Dialer] Control callback that refuses every
// address [Allowed] refuses for allowPrivate.
func Control(allowPrivate bool) func(network, address string, _ syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrUnsafeAddress, network)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("%w: %q did not resolve to a literal IP", ErrUnsafeAddress, host)
		}
		if !Allowed(ip, allowPrivate) {
			return ErrUnsafeAddress
		}
		return nil
	}
}

// Allowed reports whether ip may be dialed. Without allowPrivate only
// [IsPublicAddr] addresses are. allowPrivate additionally admits loopback,
// RFC 1918, IPv6 unique-local and CGNAT addresses (an operator monitoring
// their own internal sites), but never link-local, the cloud metadata
// addresses, unspecified, multicast or reserved space.
func Allowed(ip net.IP, allowPrivate bool) bool {
	if IsPublicAddr(ip) {
		return true
	}
	if !allowPrivate {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.IsLoopback() || ip4.IsPrivate() || cgnatBlock.Contains(ip4)
	}
	if awsMetadataV6.Contains(ip) {
		return false
	}
	if embedded := embeddedIPv4(ip); embedded != nil {
		return Allowed(embedded, true)
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// Reserved and special-purpose ranges with no net.IP helper, checked
// alongside loopback/private/link-local/unspecified/multicast, which the
// standard library already recognises directly on a net.IP.
var (
	zeroBlock      = mustParseCIDR("0.0.0.0/8")     // RFC 791 §3.2, "this network"
	cgnatBlock     = mustParseCIDR("100.64.0.0/10") // RFC 6598, shared address space
	ietfProtocol   = mustParseCIDR("192.0.0.0/24")  // RFC 6890, IETF protocol assignments
	benchmarking   = mustParseCIDR("198.18.0.0/15") // RFC 2544, network interconnect testing
	reservedFuture = mustParseCIDR("240.0.0.0/4")   // RFC 1112, reserved; subsumes 255.255.255.255

	nat64     = mustParseCIDR("64:ff9b::/96") // RFC 6052, the NAT64 well-known prefix
	sixToFour = mustParseCIDR("2002::/16")    // RFC 3056, 6to4

	// The EC2 IPv6 instance metadata endpoint, fd00:ec2::254, sits inside
	// the unique-local block allowPrivate otherwise admits.
	awsMetadataV6 = mustParseCIDR("fd00:ec2::/32")
)

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// IsPublicAddr reports whether ip is a public unicast address.
//
// For an IPv4 address (including one carried as IPv4-mapped IPv6,
// ::ffff:a.b.c.d, which ip.To4() resolves the same as a literal v4
// address): not loopback, private (RFC1918), link-local (including
// 169.254.169.254's /16), unspecified, multicast, "this network"
// (0.0.0.0/8), IETF protocol assignments (192.0.0.0/24), benchmarking
// (198.18.0.0/15), reserved (240.0.0.0/4, which already subsumes the
// 255.255.255.255 broadcast address), or CGNAT (100.64.0.0/10).
//
// For a genuine IPv6 address: not loopback, private (the IPv6 ULA block,
// fc00::/7), link-local, unspecified, or multicast. NAT64 (64:ff9b::/96)
// and 6to4 (2002::/16) carry an IPv4 address in fixed bits, so that address
// is extracted and checked instead: 64:ff9b::a9fe:a9fe (NAT64 for
// 169.254.169.254) is refused because of the address it carries.
func IsPublicAddr(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4.IsLoopback(), ip4.IsPrivate(), ip4.IsLinkLocalUnicast(), ip4.IsLinkLocalMulticast(),
			ip4.IsUnspecified(), ip4.IsMulticast():
			return false
		case zeroBlock.Contains(ip4), ietfProtocol.Contains(ip4), benchmarking.Contains(ip4),
			reservedFuture.Contains(ip4), cgnatBlock.Contains(ip4):
			return false
		}
		return true
	}

	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsUnspecified(), ip.IsMulticast():
		return false
	}
	if embedded := embeddedIPv4(ip); embedded != nil {
		return IsPublicAddr(embedded)
	}
	return true
}

// embeddedIPv4 extracts the IPv4 address a NAT64 (RFC 6052) or 6to4
// (RFC 3056) IPv6 address carries, or nil if ip is neither.
func embeddedIPv4(ip net.IP) net.IP {
	ip16 := ip.To16()
	if ip16 == nil {
		return nil
	}
	switch {
	case nat64.Contains(ip16):
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	case sixToFour.Contains(ip16):
		return net.IPv4(ip16[2], ip16[3], ip16[4], ip16[5])
	}
	return nil
}
