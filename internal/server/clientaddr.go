package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// The client's address (issue #437, ADR-008 Consequences). conchd reads it for
// one purpose only: the rate limits of the invite routes, which are the first
// routes anyone may call without a credential that create something. Behind a
// reverse proxy every connection comes from the proxy, so the address is taken
// from X-Forwarded-For, but only from a proxy the operator said to trust.
//
// The address is personal data: a tester's home address, more or less. It is
// never written to the audit trail, never put in an error body, and a log line
// carries at most the truncated form of logAddr (an IPv4 /24, an IPv6 /48),
// which is enough to see that one network is hammering the server and not
// enough to name a household. The full address lives only in the memory of
// the rate limiter, and a restart forgets it.

// ParseTrustedProxies reads the --trusted-proxies / CONCHD_TRUSTED_PROXIES
// value: comma-separated CIDRs such as 127.0.0.1/32,::1/128. Empty means no
// proxy is trusted, so X-Forwarded-For is never read. A bare address is
// refused rather than guessed at: the operator says how wide the trust is.
func ParseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q is not a CIDR such as 127.0.0.1/32 or ::1/128", part)
		}
		// 10.0.0.1/8 means 10.0.0.0/8; an IPv4-mapped IPv6 prefix would never
		// match, since addresses are unmapped before they are compared.
		if p.Addr().Is4In6() {
			return nil, fmt.Errorf("trusted proxy %q is an IPv4-mapped IPv6 prefix; write the IPv4 CIDR", part)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// clientAddr is the address of the client that sent r, as rate limits see it.
//
// With no trusted proxies it is the host part of r.RemoteAddr. When the peer
// (RemoteAddr) is inside a trusted CIDR, X-Forwarded-For is walked from the
// right, skipping trusted addresses, and the first address not in a trusted
// CIDR is the client: every entry to the right of it was appended by a proxy
// the operator trusts, and anything to its left is whatever the client chose
// to send. If no such address is found, or an entry reached on the way is not
// an address, the peer is used. A peer outside the trusted CIDRs never has its
// header read, so a client cannot choose its own bucket. The Forwarded header
// is not read.
//
// The zero Addr (RemoteAddr unparsable, which net/http never produces) is a
// valid answer: every such request shares one bucket.
func clientAddr(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := remoteAddr(r.RemoteAddr)
	if len(trusted) == 0 || !peer.IsValid() || !inPrefixes(peer, trusted) {
		return peer
	}
	// Several X-Forwarded-For headers are one list, in order (RFC 9110 §5.3).
	header := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	for header != "" {
		var entry string
		if i := strings.LastIndexByte(header, ','); i >= 0 {
			header, entry = header[:i], header[i+1:]
		} else {
			header, entry = "", header
		}
		a, ok := forwardedAddr(strings.TrimSpace(entry))
		if !ok {
			return peer
		}
		if !inPrefixes(a, trusted) {
			return a
		}
	}
	return peer
}

// remoteAddr parses the host part of an http.Request's RemoteAddr.
func remoteAddr(hostport string) netip.Addr {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.WithZone("").Unmap()
}

// forwardedAddr parses one X-Forwarded-For entry: an address, or (as some
// proxies write it) an address with a port, IPv6 in brackets. Anything else,
// an empty entry included, is malformed.
func forwardedAddr(entry string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(entry); err == nil {
		return a.WithZone("").Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(entry); err == nil {
		return ap.Addr().WithZone("").Unmap(), true
	}
	return netip.Addr{}, false
}

func inPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// limitKey is the rate-limit bucket of an address. An IPv4 address is its
// own bucket. An IPv6 address is bucketed by its /64: one subscriber usually
// holds a whole /64 and can pick any address in it, so a per-address bucket
// would be no limit at all.
func limitKey(a netip.Addr) string {
	if !a.IsValid() {
		return "unknown"
	}
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// overflowKey is the bucket an address shares once its limit's table is
// full (invite_limits.go): its IPv4 /16 or IPv6 /32, a network wide enough
// that one attacker rarely holds more than one, and narrow enough that a flood
// from it leaves other networks their own bucket.
func overflowKey(a netip.Addr) string {
	if !a.IsValid() {
		return inviteOverflowKey
	}
	bits := 16
	if a.Is6() {
		bits = 32
	}
	p, _ := a.Prefix(bits)
	return inviteOverflowKey + "|" + p.String()
}

// logAddr is the most of an address a log line may carry: the IPv4 /24 or the
// IPv6 /48 it is in. See the comment at the top of this file for why.
func logAddr(a netip.Addr) string {
	if !a.IsValid() {
		return "unknown"
	}
	bits := 24
	if a.Is6() {
		bits = 48
	}
	p, _ := a.Prefix(bits)
	return p.String()
}
