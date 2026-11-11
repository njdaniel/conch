package push

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestForbidden(t *testing.T) {
	const (
		never = "never"   // refused with or without allowPrivate
		lan   = "lan"     // refused unless allowPrivate
		ok    = "allowed" // admitted either way
	)
	tests := []struct {
		name string
		addr string
		want string
	}{
		// IPv4 ranges.
		{"unspecified v4", "0.0.0.0", never},
		{"this network", "0.1.2.3", never},
		{"loopback", "127.0.0.1", never},
		{"loopback high", "127.255.255.254", never},
		{"RFC1918 10/8", "10.0.0.1", lan},
		{"RFC1918 172.16/12", "172.16.0.1", lan},
		{"RFC1918 172.31", "172.31.255.255", lan},
		{"RFC1918 192.168/16", "192.168.1.1", lan},
		{"CGNAT low", "100.64.0.1", lan},
		{"CGNAT high", "100.127.255.254", lan},
		{"link-local", "169.254.1.1", never},
		{"metadata", "169.254.169.254", never},
		{"ECS task credentials", "169.254.170.2", never},
		{"IETF assignments", "192.0.0.8", never},
		{"benchmarking", "198.18.0.1", never},
		{"benchmarking high", "198.19.255.255", never},
		{"multicast", "224.0.0.1", never},
		{"multicast high", "239.255.255.255", never},
		{"reserved", "240.0.0.1", never},
		{"broadcast", "255.255.255.255", never},
		{"just outside 172.16/12", "172.32.0.1", ok},
		{"just outside CGNAT", "100.128.0.1", ok},
		{"public v4", "8.8.8.8", ok},
		{"public v4 b", "93.184.216.34", ok},
		// IPv6.
		{"unspecified v6", "::", never},
		{"loopback v6", "::1", never},
		{"link-local v6", "fe80::1", never},
		{"link-local v6 zone", "fe80::1%eth0", never},
		{"ULA fc", "fc00::1", lan},
		{"ULA fd", "fd12:3456::1", lan},
		{"metadata v6", "fd00:ec2::254", never},
		{"multicast v6", "ff02::1", never},
		{"public v6", "2606:4700:4700::1111", ok},
		{"public v6 b", "2a00:1450:4001::200e", ok},
		// IPv4-mapped.
		{"mapped private", "::ffff:10.0.0.1", lan},
		{"mapped loopback", "::ffff:127.0.0.1", never},
		{"mapped metadata", "::ffff:169.254.169.254", never},
		{"mapped public", "::ffff:8.8.8.8", ok},
		// IPv4-compatible (::/96) is refused whole.
		{"compatible private", "::10.0.0.1", never},
		{"compatible loopback", "::127.0.0.1", never},
		{"compatible public", "::8.8.8.8", never},
		// NAT64 well-known /96.
		{"nat64 private", "64:ff9b::a00:1", lan},
		{"nat64 loopback", "64:ff9b::7f00:1", never},
		{"nat64 metadata", "64:ff9b::a9fe:a9fe", never},
		{"nat64 192.168", "64:ff9b::c0a8:1", lan},
		{"nat64 public", "64:ff9b::808:808", ok},
		// NAT64 local-use /48 is refused whole, whichever layout the
		// mapping uses.
		{"nat64 local private last4", "64:ff9b:1::a00:1", never},
		{"nat64 local loopback last4", "64:ff9b:1::7f00:1", never},
		{"nat64 local private rfc6052", "64:ff9b:1:a00:0:0100::", never},
		{"nat64 local metadata rfc6052", "64:ff9b:1:a9fe:a9:fe00::", never},
		{"nat64 local public", "64:ff9b:1::808:808", never},
		// 6to4.
		{"6to4 private", "2002:0a00:0001::", lan},
		{"6to4 loopback", "2002:7f00:0001::", never},
		{"6to4 metadata", "2002:a9fe:a9fe::", never},
		{"6to4 192.168", "2002:c0a8:0101::1", lan},
		{"6to4 public", "2002:0808:0808::", ok},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := netip.MustParseAddr(tc.addr)
			wantStrict := tc.want != ok
			wantAllow := tc.want == never
			if got := Forbidden(a); got != wantStrict {
				t.Errorf("Forbidden = %v, want %v", got, wantStrict)
			}
			if got := ForbiddenWith(a, false); got != wantStrict {
				t.Errorf("ForbiddenWith(false) = %v, want %v", got, wantStrict)
			}
			if got := ForbiddenWith(a, true); got != wantAllow {
				t.Errorf("ForbiddenWith(true) = %v, want %v", got, wantAllow)
			}
		})
	}
}

func TestForbiddenInvalid(t *testing.T) {
	if !Forbidden(netip.Addr{}) || !ForbiddenWith(netip.Addr{}, true) {
		t.Fatal("the zero Addr must be forbidden")
	}
}

// Every lan row must be relaxed by allowPrivate and every other row must not,
// from one table.
func TestAllowPrivateFollowsTable(t *testing.T) {
	for _, r := range forbiddenRanges {
		a := r.prefix.Addr()
		if r.prefix.IsSingleIP() {
			// A single-address row (metadata) is checked by TestForbidden.
			if r.lan {
				t.Errorf("single-address row %s must not be lan", r.prefix)
			}
			continue
		}
		// Use an address inside the row that no other row narrows.
		if got := ForbiddenWith(a.Next(), true); got == r.lan {
			// 169.254.0.1 etc.: relaxed rows admit, others refuse.
			t.Errorf("%s lan=%v but allowPrivate refused=%v", r.prefix, r.lan, got)
		}
	}
}

type lookupResult struct {
	addrs []netip.Addr
	err   error
}

func TestCheck(t *testing.T) {
	pub4 := netip.MustParseAddr("93.184.216.34")
	pub6 := netip.MustParseAddr("2606:4700:4700::1111")
	priv6 := netip.MustParseAddr("fd00::5")
	tests := []struct {
		name         string
		host         string
		res          lookupResult
		allowPrivate bool
		want         []netip.Addr
		wantErr      error
		wantLookups  int
	}{
		{"public pair", "h.example", lookupResult{addrs: []netip.Addr{pub4, pub6}}, false, []netip.Addr{pub4, pub6}, nil, 1},
		{"mixed public A private AAAA", "h.example", lookupResult{addrs: []netip.Addr{pub4, priv6}}, false, nil, ErrAddressRefused, 1},
		{"mixed private first", "h.example", lookupResult{addrs: []netip.Addr{priv6, pub4}}, false, nil, ErrAddressRefused, 1},
		{"mixed admitted under allowPrivate", "h.example", lookupResult{addrs: []netip.Addr{pub4, priv6}}, true, []netip.Addr{pub4, priv6}, nil, 1},
		{"mixed with loopback under allowPrivate", "h.example", lookupResult{addrs: []netip.Addr{pub4, netip.MustParseAddr("127.0.0.1")}}, true, nil, ErrAddressRefused, 1},
		{"mapped private answer", "h.example", lookupResult{addrs: []netip.Addr{netip.MustParseAddr("::ffff:10.0.0.1")}}, false, nil, ErrAddressRefused, 1},
		{"mapped public answer is unmapped", "h.example", lookupResult{addrs: []netip.Addr{netip.MustParseAddr("::ffff:8.8.8.8")}}, false, []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil, 1},
		{"lookup failure", "h.example", lookupResult{err: errors.New("lookup marker-host.example: no such host")}, false, nil, ErrLookupFailed, 1},
		{"empty answer", "h.example", lookupResult{}, false, nil, ErrLookupFailed, 1},
		{"empty host", "", lookupResult{}, false, nil, ErrLookupFailed, 0},
		{"public v4 literal", "93.184.216.34", lookupResult{}, false, []netip.Addr{pub4}, nil, 0},
		{"public v6 literal", "2606:4700:4700::1111", lookupResult{}, false, []netip.Addr{pub6}, nil, 0},
		{"loopback literal", "127.0.0.1", lookupResult{}, false, nil, ErrAddressRefused, 0},
		{"metadata literal", "169.254.169.254", lookupResult{}, true, nil, ErrAddressRefused, 0},
		{"private literal", "10.0.0.1", lookupResult{}, false, nil, ErrAddressRefused, 0},
		{"private literal allowed", "10.0.0.1", lookupResult{}, true, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil, 0},
		{"mapped literal", "::ffff:10.0.0.1", lookupResult{}, false, nil, ErrAddressRefused, 0},
		{"nat64 literal", "64:ff9b::a00:1", lookupResult{}, false, nil, ErrAddressRefused, 0},
		{"zoned literal", "fe80::1%eth0", lookupResult{}, false, nil, ErrAddressRefused, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := &fakeResolver{fn: func(int, string) ([]netip.Addr, error) { return tc.res.addrs, tc.res.err }}
			got, err := Check(context.Background(), tc.host, res, tc.allowPrivate)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if res.calls() != tc.wantLookups {
				t.Errorf("lookups = %d, want %d", res.calls(), tc.wantLookups)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("addrs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("addrs[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLookupTimeoutIsTwoSeconds(t *testing.T) {
	if lookupTimeout != 2*time.Second {
		t.Fatalf("lookupTimeout = %v, want 2s", lookupTimeout)
	}
}

func TestCheckLookupTimeout(t *testing.T) {
	old := lookupTimeout
	lookupTimeout = 20 * time.Millisecond
	t.Cleanup(func() { lookupTimeout = old })

	blocking := &blockingResolver{}
	start := time.Now()
	_, err := Check(context.Background(), "slow.example", blocking, false)
	if !errors.Is(err, ErrLookupFailed) {
		t.Fatalf("err = %v, want %v", err, ErrLookupFailed)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("lookup was not bounded")
	}
}

type blockingResolver struct{}

func (blockingResolver) LookupNetIP(ctx context.Context, _, _ string) ([]netip.Addr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCheckCallerContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
		want string
	}{
		{"cancelled", func() context.Context {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			return c
		}, ClassRequestFailed},
		{"deadline", func() context.Context {
			c, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return c
		}, ClassTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Check(tc.ctx(), "h.example", blockingResolver{}, false)
			if got := Classify(err, nil); got != tc.want {
				t.Errorf("class = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckErrorsCarryNoHost(t *testing.T) {
	const host = "marker-host.example"
	cases := []struct {
		name string
		res  *fakeResolver
		host string
	}{
		{"refused", fixed("10.0.0.1"), host},
		{"lookup failed", &fakeResolver{fn: func(_ int, h string) ([]netip.Addr, error) {
			return nil, errors.New("lookup " + h + ": no such host at 203.0.113.99")
		}}, host},
		{"literal", fixed(), "10.0.0.77"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Check(context.Background(), tc.host, tc.res, false)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, bad := range []string{tc.host, "10.0.0", "203.0.113", "marker"} {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error %q contains %q", err, bad)
				}
			}
		})
	}
}
