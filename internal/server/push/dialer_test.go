package push

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestDialerRefusals(t *testing.T) {
	tests := []struct {
		name         string
		res          *fakeResolver
		allowPrivate bool
		network      string
		address      string
		wantClass    string
	}{
		{"private answer", fixed("10.0.0.1"), false, "tcp", "h.example:443", ClassAddressRefused},
		{"metadata answer even allowPrivate", fixed("169.254.169.254"), true, "tcp", "h.example:443", ClassAddressRefused},
		{"loopback answer even allowPrivate", fixed("127.0.0.1"), true, "tcp", "h.example:443", ClassAddressRefused},
		{"mapped private", fixed("::ffff:10.0.0.1"), false, "tcp", "h.example:443", ClassAddressRefused},
		{"compatible private", fixed("::10.0.0.1"), false, "tcp", "h.example:443", ClassAddressRefused},
		{"nat64 private", fixed("64:ff9b::a00:1"), false, "tcp", "h.example:443", ClassAddressRefused},
		{"nat64 local private", fixed("64:ff9b:1::a00:1"), false, "tcp", "h.example:443", ClassAddressRefused},
		{"6to4 private", fixed("2002:0a00:0001::"), false, "tcp", "h.example:443", ClassAddressRefused},
		{"mixed public and private", fixed("93.184.216.34", "fd00::5"), false, "tcp", "h.example:443", ClassAddressRefused},
		{"literal private", fixed(), false, "tcp", "10.0.0.1:443", ClassAddressRefused},
		{"lookup failure", &fakeResolver{fn: func(int, string) ([]netip.Addr, error) { return nil, errors.New("boom") }}, false, "tcp", "h.example:443", ClassNameLookup},
		{"bad network", fixed("93.184.216.34"), false, "udp", "h.example:443", ClassRequestFailed},
		{"no port", fixed("93.184.216.34"), false, "tcp", "h.example", ClassRequestFailed},
		{"port zero", fixed("93.184.216.34"), false, "tcp", "h.example:0", ClassRequestFailed},
		{"port too big", fixed("93.184.216.34"), false, "tcp", "h.example:70000", ClassRequestFailed},
		{"tcp4 with only v6 answers", fixed("2606:4700:4700::1111"), false, "tcp4", "h.example:443", ClassRequestFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			d := NewDialer(tc.res, tc.allowPrivate)
			d.connect = rec.connect
			conn, err := d.DialContext(context.Background(), tc.network, tc.address)
			if err == nil {
				_ = conn.Close()
				t.Fatal("want an error")
			}
			if got := Classify(err, nil); got != tc.wantClass {
				t.Errorf("class = %q, want %q", got, tc.wantClass)
			}
			if tc.wantClass == ClassAddressRefused && len(rec.seen()) != 0 {
				t.Errorf("dialled after a refusal: %v", rec.seen())
			}
		})
	}
}

// The system dialer must only ever see a numeric ip:port taken from the check.
func TestDialerConnectsToCheckedAddress(t *testing.T) {
	// Two public answers; the first fails, the second is dialled. A resolver
	// that would answer differently on a second lookup proves no second lookup
	// happens between the check and the connects.
	res := sequence(
		[]string{"93.184.216.34", "2606:4700:4700::1111"},
		[]string{"10.0.0.1"},
	)
	rec := &recorder{}
	d := NewDialer(res, false)
	d.connect = rec.connect
	_, err := d.DialContext(context.Background(), "tcp", "h.example:8443")
	if got := Classify(err, nil); got != ClassRequestFailed {
		t.Fatalf("class = %q, want %q", got, ClassRequestFailed)
	}
	if res.calls() != 1 {
		t.Fatalf("lookups = %d, want 1 for one dial", res.calls())
	}
	want := []string{"93.184.216.34:8443", "[2606:4700:4700::1111]:8443"}
	got := rec.seen()
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d = %q, want %q", i, got[i], want[i])
		}
		if _, err := netip.ParseAddrPort(got[i]); err != nil {
			t.Errorf("target %q is not a numeric address and port", got[i])
		}
		if strings.Contains(got[i], "h.example") {
			t.Errorf("target %q carries the host name", got[i])
		}
	}
}

func TestDialerNetworkFamilies(t *testing.T) {
	tests := []struct {
		network string
		want    []string
	}{
		{"tcp", []string{"93.184.216.34:443", "[2606:4700:4700::1111]:443"}},
		{"tcp4", []string{"93.184.216.34:443"}},
		{"tcp6", []string{"[2606:4700:4700::1111]:443"}},
	}
	for _, tc := range tests {
		t.Run(tc.network, func(t *testing.T) {
			rec := &recorder{}
			d := NewDialer(fixed("93.184.216.34", "2606:4700:4700::1111"), false)
			d.connect = rec.connect
			_, _ = d.DialContext(context.Background(), tc.network, "h.example:443")
			got := rec.seen()
			if len(got) != len(tc.want) {
				t.Fatalf("targets = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("target %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The real connect re-checks the numeric address in the socket's Control hook.
func TestConnectNumericControlRefuses(t *testing.T) {
	d := NewDialer(fixed(), false)
	for _, addr := range []string{"127.0.0.1:9", "10.0.0.1:9", "[::1]:9", "169.254.169.254:80"} {
		conn, err := d.connectNumeric(context.Background(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("%s: connected", addr)
		}
		if got := connectClass(context.Background(), err); got.Error() != ClassAddressRefused {
			t.Errorf("%s: class = %q", addr, got)
		}
	}
}

func TestDialerErrorsCarryNoAddress(t *testing.T) {
	rec := &recorder{} // fails with an error naming marker-dial-error
	d := NewDialer(fixed("93.184.216.34"), false)
	d.connect = rec.connect
	_, err := d.DialContext(context.Background(), "tcp", "marker-host.example:443")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, bad := range []string{"marker", "93.184", "443"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error %q contains %q", err, bad)
		}
	}
}
