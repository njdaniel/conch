package server

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func mustPrefixes(t *testing.T, raw string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrustedProxies(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClientAddr(t *testing.T) {
	loopback := "127.0.0.1/32,::1/128"
	chain := "127.0.0.1/32,10.0.0.0/8"
	tests := []struct {
		name    string
		trusted string
		remote  string
		xff     []string // one entry per X-Forwarded-For header
		want    string
	}{
		// No trusted proxies: the peer, whatever the header says.
		{"no proxies, no header", "", "203.0.113.7:5555", nil, "203.0.113.7"},
		{"no proxies, header ignored", "", "203.0.113.7:5555", []string{"198.51.100.1"}, "203.0.113.7"},
		{"no proxies, loopback peer ignores header", "", "127.0.0.1:5555", []string{"198.51.100.1"}, "127.0.0.1"},
		{"no proxies, IPv6 peer", "", "[2001:db8::1]:443", nil, "2001:db8::1"},
		{"no proxies, mapped IPv4 peer", "", "[::ffff:203.0.113.7]:443", nil, "203.0.113.7"},
		{"no proxies, zoned peer", "", "[fe80::1%eth0]:443", nil, "fe80::1"},

		// A trusted peer: the header is walked from the right.
		{"trusted, single entry", loopback, "127.0.0.1:5555", []string{"198.51.100.1"}, "198.51.100.1"},
		{"trusted, no header", loopback, "127.0.0.1:5555", nil, "127.0.0.1"},
		{"trusted, empty header", loopback, "127.0.0.1:5555", []string{""}, "127.0.0.1"},
		{"trusted, client spoofs a left entry", loopback, "127.0.0.1:5555", []string{"1.1.1.1, 198.51.100.1"}, "198.51.100.1"},
		{"trusted, chained proxies", chain, "127.0.0.1:5555", []string{"198.51.100.1, 10.1.2.3"}, "198.51.100.1"},
		{"trusted, chained across headers", chain, "127.0.0.1:5555", []string{"1.1.1.1, 198.51.100.1", "10.1.2.3"}, "198.51.100.1"},
		{"trusted, only proxies in the header", chain, "127.0.0.1:5555", []string{"10.0.0.1, 10.0.0.2"}, "127.0.0.1"},
		{"trusted, IPv6 client", loopback, "[::1]:5555", []string{"2001:db8::7"}, "2001:db8::7"},
		{"trusted, IPv6 client with port", loopback, "127.0.0.1:5555", []string{"[2001:db8::7]:1234"}, "2001:db8::7"},
		{"trusted, IPv4 client with port", loopback, "127.0.0.1:5555", []string{"198.51.100.1:1234"}, "198.51.100.1"},
		{"trusted, mapped client", loopback, "127.0.0.1:5555", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"trusted, spaces", loopback, "127.0.0.1:5555", []string{"  198.51.100.1  "}, "198.51.100.1"},

		// Malformed: the peer.
		{"trusted, garbage", loopback, "127.0.0.1:5555", []string{"not-an-address"}, "127.0.0.1"},
		{"trusted, empty entry on the right", loopback, "127.0.0.1:5555", []string{"198.51.100.1,"}, "127.0.0.1"},
		{"trusted, garbage between proxies", chain, "127.0.0.1:5555", []string{"198.51.100.1, junk, 10.0.0.1"}, "127.0.0.1"},
		{"trusted, unknown header form", loopback, "127.0.0.1:5555", []string{"for=198.51.100.1"}, "127.0.0.1"},
		// Garbage left of the client is never reached.
		{"trusted, garbage left of the client", loopback, "127.0.0.1:5555", []string{"junk, 198.51.100.1"}, "198.51.100.1"},

		// An untrusted peer never has its header read.
		{"untrusted peer spoofs", loopback, "203.0.113.7:5555", []string{"198.51.100.1"}, "203.0.113.7"},
		{"untrusted peer spoofs a proxy", loopback, "203.0.113.7:5555", []string{"127.0.0.1"}, "203.0.113.7"},
		{"untrusted IPv6 peer", loopback, "[2001:db8::99]:5555", []string{"198.51.100.1"}, "2001:db8::99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/invites/redeem", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			// Forwarded is never read.
			r.Header.Set("Forwarded", "for=192.0.2.200")
			got := clientAddr(r, mustPrefixes(t, tt.trusted))
			if got.String() != tt.want {
				t.Errorf("clientAddr = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClientAddrUnparsablePeer(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "pipe"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := clientAddr(r, mustPrefixes(t, "0.0.0.0/0")); got.IsValid() {
		t.Errorf("clientAddr = %s, want the zero address", got)
	}
	if limitKey(netip.Addr{}) != "unknown" || logAddr(netip.Addr{}) != "unknown" {
		t.Error("the zero address does not have its own bucket and log form")
	}
}

func TestParseTrustedProxies(t *testing.T) {
	tests := []struct {
		raw     string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{" , ", nil, false},
		{"127.0.0.1/32", []string{"127.0.0.1/32"}, false},
		{"127.0.0.1/32,::1/128", []string{"127.0.0.1/32", "::1/128"}, false},
		{"10.9.8.7/8", []string{"10.0.0.0/8"}, false},
		{"2001:db8::1/32", []string{"2001:db8::/32"}, false},
		{"127.0.0.1", nil, true},
		{"localhost/32", nil, true},
		{"127.0.0.1/33", nil, true},
		{"::ffff:10.0.0.0/104", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := ParseTrustedProxies(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i].String() != tt.want[i] {
					t.Errorf("prefix %d = %s, want %s", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLimitKeyAndLogAddr(t *testing.T) {
	tests := []struct {
		addr, key, log string
	}{
		{"198.51.100.77", "198.51.100.77", "198.51.100.0/24"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64", "2001:db8:1::/48"},
		{"::1", "::/64", "::/48"},
	}
	for _, tt := range tests {
		a := netip.MustParseAddr(tt.addr)
		if got := limitKey(a); got != tt.key {
			t.Errorf("limitKey(%s) = %s, want %s", tt.addr, got, tt.key)
		}
		if got := logAddr(a); got != tt.log {
			t.Errorf("logAddr(%s) = %s, want %s", tt.addr, got, tt.log)
		}
	}
	// Two addresses of one /64 share a bucket; of two /64s they do not.
	a, b := netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::ffff")
	c := netip.MustParseAddr("2001:db8:0:1::1")
	if limitKey(a) != limitKey(b) || limitKey(a) == limitKey(c) {
		t.Errorf("IPv6 buckets: %s %s %s", limitKey(a), limitKey(b), limitKey(c))
	}
}
