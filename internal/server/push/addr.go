package push

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"
)

// lookupTimeout bounds one name lookup. It is a variable only so a test can
// shorten it; nothing outside the package's tests assigns it.
var lookupTimeout = 2 * time.Second

// testPermit, when non-nil, names addresses that are not forbidden. It exists
// so that a loopback httptest TLS server can stand in for a public host. Only
// _test.go files assign it: production code in this package never does, and
// it is unexported, so no other package can.
var testPermit func(netip.Addr) bool

// rangeRow is one forbidden range. lan marks the rows that allowPrivate
// relaxes.
type rangeRow struct {
	prefix netip.Prefix
	lan    bool
}

// forbiddenRanges is the one source of truth for which ranges are refused and
// which of those allowPrivate relaxes.
//
// A row is lan only when it is the private address space of a network an
// operator can legitimately run a push distributor on: RFC 1918, CGNAT and
// ULA. Link-local is never relaxed: no distributor is reached at a link-local
// address (a zone-less fe80:: address cannot even be dialled), and 169.254/16
// holds cloud services beyond the metadata address, such as AWS ECS's task
// credentials at 169.254.170.2. Everything else is never a place a
// distributor lives, and would only ever be reached by mistake or by an
// attack: loopback is conchd's own host (its admin and metrics surface),
// unspecified, multicast and reserved ranges are not unicast peers, and the
// cloud metadata and credential addresses (169.254.169.254, 100.100.100.200,
// the fd00:ec2::/32 family and so on) sit inside the CGNAT and ULA rows, so
// they get non-lan rows of their own. So does 100.100.0.0/16, inside CGNAT:
// it holds Tailscale's local tailscaled at 100.100.100.100 and Alibaba's
// other internal services. IPv6 is also a positive allow-list, see
// ForbiddenWith. The predicate refuses an address when any row that matches
// it is not relaxed.
var forbiddenRanges = []rangeRow{
	{netip.MustParsePrefix("0.0.0.0/8"), false},          // "this network", unspecified
	{netip.MustParsePrefix("10.0.0.0/8"), true},          // RFC 1918
	{netip.MustParsePrefix("100.64.0.0/10"), true},       // CGNAT
	{netip.MustParsePrefix("127.0.0.0/8"), false},        // loopback
	{netip.MustParsePrefix("169.254.0.0/16"), false},     // link-local: never relaxed
	{netip.MustParsePrefix("169.254.169.254/32"), false}, // cloud metadata
	{netip.MustParsePrefix("168.63.129.16/32"), false},   // Azure wireserver
	{netip.MustParsePrefix("100.100.0.0/16"), false},     // Tailscale tailscaled, Alibaba services, inside CGNAT
	{netip.MustParsePrefix("100.100.100.200/32"), false}, // Alibaba metadata
	{netip.MustParsePrefix("192.0.2.0/24"), false},       // documentation
	{netip.MustParsePrefix("198.51.100.0/24"), false},    // documentation
	{netip.MustParsePrefix("203.0.113.0/24"), false},     // documentation
	{netip.MustParsePrefix("192.88.99.0/24"), false},     // 6to4 relay anycast
	{netip.MustParsePrefix("172.16.0.0/12"), true},       // RFC 1918
	{netip.MustParsePrefix("192.0.0.0/24"), false},       // IETF protocol assignments
	{netip.MustParsePrefix("192.168.0.0/16"), true},      // RFC 1918
	{netip.MustParsePrefix("198.18.0.0/15"), false},      // benchmarking
	{netip.MustParsePrefix("224.0.0.0/4"), false},        // multicast
	{netip.MustParsePrefix("240.0.0.0/4"), false},        // reserved, includes broadcast
	// IPv4-compatible (::/96) is deprecated and never routed. The whole
	// range is refused, which also covers :: and ::1.
	{netip.MustParsePrefix("::/96"), false},
	// NAT64 local-use (RFC 8215) leaves the IPv4 mapping to whoever runs the
	// translator (a /96 reading and the RFC 6052 /48 layout disagree about
	// where the address sits), and it is not the public internet. The whole
	// range is refused rather than guessing where the address is.
	{netip.MustParsePrefix("64:ff9b:1::/48"), false},
	{netip.MustParsePrefix("fc00::/7"), true}, // ULA
	// Cloud metadata and credential addresses inside the ULA row, so
	// allowPrivate must not admit them.
	{netip.MustParsePrefix("fd00:ec2::/32"), false},          // AWS IMDS v6, EKS Pod Identity
	{netip.MustParsePrefix("fd20:ce::254/128"), false},       // GCP metadata v6
	{netip.MustParsePrefix("fd00:c1::a9fe:a9fe/128"), false}, // Oracle metadata v6
	{netip.MustParsePrefix("fec0::/10"), false},              // site-local, deprecated
	// IETF protocol assignments (IANA: not globally reachable). It holds
	// Teredo (2001::/32), PCP anycast, benchmarking, ORCHID and unassigned
	// space, and is refused whole rather than slice by slice.
	{netip.MustParsePrefix("2001::/23"), false},
	{netip.MustParsePrefix("2001:db8::/32"), false}, // documentation
	{netip.MustParsePrefix("3fff::/20"), false},     // documentation
	{netip.MustParsePrefix("fe80::/10"), false},     // link-local: never relaxed
	{netip.MustParsePrefix("ff00::/8"), false},      // multicast
}

// The IPv6 forms whose embedded IPv4 address is extracted. IPv4-mapped
// (::ffff:0:0/96) is unmapped by netip before the table is consulted;
// IPv4-compatible (::/96) and NAT64 local-use (64:ff9b:1::/48) are refused
// whole by the table.
var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
	// globalUnicastV6 is the only block IPv6 addresses are admitted from.
	globalUnicastV6 = netip.MustParsePrefix("2000::/3")
)

// Forbidden reports whether a connection to a must be refused: it is in a
// private, loopback, link-local, multicast or otherwise non-public range, or
// it is an IPv6 form of a forbidden IPv4 address. IPv4-mapped, well-known NAT64
// and 6to4 addresses are refused by their embedded IPv4 address; IPv4-compatible
// (::/96) and NAT64 local-use (64:ff9b:1::/48) are refused whole.
func Forbidden(a netip.Addr) bool {
	return ForbiddenWith(a, false)
}

// ForbiddenWith is Forbidden with the operator's allowPrivate setting. When
// allowPrivate is true the RFC 1918, CGNAT and ULA rows are admitted, except
// the cloud metadata addresses inside them; link-local, loopback, unspecified,
// multicast, reserved and documentation ranges stay forbidden.
//
// IPv4 is a deny list: a public address is any global unicast address no row
// matches. IPv6 is a positive allow-list: after unmapping, an address is
// admitted only if it is in the global unicast block 2000::/3, or in the
// well-known NAT64 prefix 64:ff9b::/96, or (under allowPrivate) in a lan row,
// and no deny row matches it. Everything else, such as site-local fec0::/10,
// 100::/64, 5f00::/16 and SIIT ::ffff:0:0:0/96, is refused because it is not
// listed. The embedded IPv4 address of a NAT64 or 6to4 form is always checked
// in strict mode: a translator path to 10.0.0.1 reaches the translator's
// network, not the operator's LAN.
func ForbiddenWith(a netip.Addr, allowPrivate bool) bool {
	if !a.IsValid() {
		return true
	}
	a = a.WithZone("").Unmap()
	if testPermit != nil && testPermit(a) {
		return false
	}
	relaxed := false
	for _, r := range forbiddenRanges {
		if !r.prefix.Contains(a) {
			continue
		}
		if r.lan && allowPrivate {
			relaxed = true
			continue
		}
		return true
	}
	if a.Is4() {
		return !a.IsGlobalUnicast()
	}
	if relaxed {
		// Only ULA is a relaxed IPv6 row. It lies outside 2000::/3, so it
		// has to be admitted here, before the allow-list below.
		return false
	}
	if !globalUnicastV6.Contains(a) && !nat64WellKnown.Contains(a) {
		return true
	}
	for _, v4 := range embeddedV4(a) {
		if ForbiddenWith(v4, false) {
			return true
		}
	}
	return false
}

// embeddedV4 returns the IPv4 address a well-known NAT64 (64:ff9b::/96) or
// 6to4 address stands for. It returns nothing for any other address.
//
// Limit: a network-specific NAT64 prefix (RFC 6052) inside 2000::/3, as an
// IPv6-only host with its own translator might use, is an ordinary global
// unicast address here, and an embedded forbidden IPv4 address is not
// decoded. An operator setting for extra NAT64 prefixes is future work.
func embeddedV4(a netip.Addr) []netip.Addr {
	if !a.Is6() {
		return nil
	}
	b := a.As16()
	switch {
	case nat64WellKnown.Contains(a):
		return []netip.Addr{netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})}
	case sixToFour.Contains(a):
		return []netip.Addr{netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})}
	}
	return nil
}

// Resolver looks up every A and AAAA answer for a host. *net.Resolver
// satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// defaultResolver is the system resolver, the Resolver used when a caller
// passes nil.
var defaultResolver Resolver = net.DefaultResolver

// Check resolves host and returns its addresses if every one of them is
// allowed. An IP-literal host is checked directly, with no lookup. Every A
// and AAAA answer is checked, and the whole host is refused if any one is
// forbidden. A nil resolver means the system resolver. The lookup is bounded to
// two seconds.
//
// The returned addresses are unmapped and carry no zone. Errors are
// ErrAddressRefused and ErrLookupFailed (or, when the caller's own context
// ended, a timeout or request-failed class): none echoes the host.
func Check(ctx context.Context, host string, resolver Resolver, allowPrivate bool) ([]netip.Addr, error) {
	if resolver == nil {
		resolver = defaultResolver
	}
	if host == "" {
		return nil, ErrLookupFailed
	}
	if lit, err := netip.ParseAddr(host); err == nil {
		if ForbiddenWith(lit, allowPrivate) {
			return nil, ErrAddressRefused
		}
		return []netip.Addr{lit.WithZone("").Unmap()}, nil
	}

	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	answers, err := resolver.LookupNetIP(lctx, "ip", host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctxClass(ctx.Err())
		}
		return nil, ErrLookupFailed
	}
	if len(answers) == 0 {
		return nil, ErrLookupFailed
	}
	out := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		if ForbiddenWith(a, allowPrivate) {
			return nil, ErrAddressRefused
		}
		out = append(out, a.WithZone("").Unmap())
	}
	return out, nil
}

// ctxClass names why the caller's own context ended.
func ctxClass(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return classedError(ClassTimeout)
	}
	return classedError(ClassRequestFailed)
}
