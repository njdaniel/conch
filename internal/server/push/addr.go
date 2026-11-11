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
// credentials at 169.254.170.2. Everything else is never a place a distributor lives, and
// would only ever be reached by mistake or by an attack: loopback is conchd's
// own host (its admin and metrics surface), unspecified, multicast and
// reserved ranges are not unicast peers, and 169.254.169.254 and
// fd00:ec2::254 are cloud metadata endpoints that sit inside the link-local
// and ULA rows, so they get a non-lan row of their own. The predicate refuses
// an address when any row that matches it is not relaxed.
var forbiddenRanges = []rangeRow{
	{netip.MustParsePrefix("0.0.0.0/8"), false},          // "this network", unspecified
	{netip.MustParsePrefix("10.0.0.0/8"), true},          // RFC 1918
	{netip.MustParsePrefix("100.64.0.0/10"), true},       // CGNAT
	{netip.MustParsePrefix("127.0.0.0/8"), false},        // loopback
	{netip.MustParsePrefix("169.254.0.0/16"), false},     // link-local: never relaxed
	{netip.MustParsePrefix("169.254.169.254/32"), false}, // cloud metadata
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
	{netip.MustParsePrefix("fc00::/7"), true},           // ULA
	{netip.MustParsePrefix("fd00:ec2::254/128"), false}, // cloud metadata (AWS IPv6)
	{netip.MustParsePrefix("fe80::/10"), false},         // link-local: never relaxed
	{netip.MustParsePrefix("ff00::/8"), false},          // multicast
}

// The IPv6 forms whose embedded IPv4 address is extracted. IPv4-mapped
// (::ffff:0:0/96) is unmapped by netip before the table is consulted;
// IPv4-compatible (::/96) and NAT64 local-use (64:ff9b:1::/48) are refused
// whole by the table.
var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
)

// Forbidden reports whether a connection to a must be refused: it is in a
// private, loopback, link-local, multicast or otherwise non-public range, or
// it is an IPv6 form (IPv4-mapped, IPv4-compatible, NAT64, 6to4) of a
// forbidden IPv4 address.
func Forbidden(a netip.Addr) bool {
	return ForbiddenWith(a, false)
}

// ForbiddenWith is Forbidden with the operator's allowPrivate setting. When
// allowPrivate is true the RFC 1918, CGNAT, ULA and link-local rows are
// admitted; loopback, unspecified, multicast, reserved ranges and the
// metadata addresses stay forbidden.
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
	if relaxed {
		// Link-local fails IsGlobalUnicast, so a relaxed row must not fall
		// through to the catch-all below.
		return false
	}
	if !a.IsGlobalUnicast() {
		return true
	}
	for _, v4 := range embeddedV4(a) {
		if ForbiddenWith(v4, allowPrivate) {
			return true
		}
	}
	return false
}

// embeddedV4 returns the IPv4 addresses a well-known NAT64 or 6to4 IPv6 address can
// stand for. It returns nothing for any other address.
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

// DefaultResolver is the system resolver, the Resolver used when a caller
// passes nil.
var DefaultResolver Resolver = net.DefaultResolver

// Check resolves host and returns its addresses if every one of them is
// allowed. An IP-literal host is checked directly, with no lookup. Every A
// and AAAA answer is checked, and the whole host is refused if any one is
// forbidden. A nil resolver means DefaultResolver. The lookup is bounded to
// two seconds.
//
// The returned addresses are unmapped and carry no zone. Errors are
// ErrAddressRefused and ErrLookupFailed (or, when the caller's own context
// ended, a timeout or request-failed class): none echoes the host.
func Check(ctx context.Context, host string, resolver Resolver, allowPrivate bool) ([]netip.Addr, error) {
	if resolver == nil {
		resolver = DefaultResolver
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
