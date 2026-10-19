package server

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The production limits, spelled out so that a change to them, or to what the
// test-only option does when it is off, fails here (issue #609).
func TestInviteLimitsProductionDefaults(t *testing.T) {
	want := inviteLimits{
		previewPerAddress:  inviteLimit{"preview_per_address", 30, 10 * time.Minute},
		redeemPerAddress:   inviteLimit{"redeem_per_address", 10, 10 * time.Minute},
		redeemGlobal:       inviteLimit{"redeem_server_wide", 20, time.Minute},
		createPerPrincipal: inviteLimit{"create_per_principal", 20, time.Hour},
	}
	if got := newTestServerWithConfig(t, Config{}).inviteLimits.limits; got != want {
		t.Errorf("a server with the option off has limits %+v, want %+v", got, want)
	}
	relaxed := newTestServerWithConfig(t, Config{TestRelaxInviteLimits: true}).inviteLimits.limits
	for _, l := range []struct {
		got, base inviteLimit
	}{
		{relaxed.previewPerAddress, want.previewPerAddress},
		{relaxed.redeemPerAddress, want.redeemPerAddress},
	} {
		if l.got.burst <= l.base.burst || l.got.window != l.base.window || l.got.name != l.base.name {
			t.Errorf("the option left %+v where it should have raised the burst of %+v", l.got, l.base)
		}
	}
	if relaxed.redeemGlobal != want.redeemGlobal || relaxed.createPerPrincipal != want.createPerPrincipal {
		t.Errorf("the option changed a limit that is not per-address: %+v", relaxed)
	}
}

// The real limits, with their real values, still refuse: the eleventh
// redemption and the thirty-first preview from one address in ten minutes get
// 429 (what the web client words as "Too many tries from your network").
func TestInviteLimitsRealValuesRefuse(t *testing.T) {
	tests := []struct {
		route string
		body  func(*testing.T) string
		allow int
	}{
		{"/v1/invites/redeem", func(t *testing.T) string {
			return inviteJSON(t, schema.RedeemInviteRequestV1{Code: fakeInviteCode, Name: "someone"})
		}, 10},
		{"/v1/invites/preview", func(t *testing.T) string {
			return inviteJSON(t, schema.InvitePreviewRequestV1{Code: fakeInviteCode})
		}, 30},
	}
	for _, tt := range tests {
		t.Run(tt.route, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			f.srv.setInviteLimits(inviteLimitsFor(Config{}))
			body := tt.body(t)
			for i := range tt.allow {
				if rec := f.send(t, "POST", tt.route, "", body, "198.51.100.1:1000"); rec.Code != http.StatusNotFound {
					t.Fatalf("attempt %d = %d %s", i+1, rec.Code, rec.Body)
				}
			}
			assertErrorBody(t, f.send(t, "POST", tt.route, "", body, "198.51.100.1:1000"), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		})
	}
}

// With the test-only option on, the same address is not limited.
func TestInviteLimitsRelaxedDoNotRefuse(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.srv.setInviteLimits(inviteLimitsFor(Config{TestRelaxInviteLimits: true}))
	body := inviteJSON(t, schema.RedeemInviteRequestV1{Code: fakeInviteCode, Name: "someone"})
	for i := range 200 {
		if rec := f.send(t, "POST", "/v1/invites/redeem", "", body, "198.51.100.1:1000"); rec.Code != http.StatusNotFound {
			t.Fatalf("attempt %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
}

// The option needs every listener to be loopback, and says so loudly when it
// is on.
func TestListenRelaxedInviteLimits(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
		warns   bool
	}{
		{name: "off, any address", cfg: Config{Listen: "0.0.0.0:0"}},
		{name: "on, loopback", cfg: Config{Listen: "127.0.0.1:0", TestRelaxInviteLimits: true}, warns: true},
		{name: "on, loopback with a loopback metrics listener", cfg: Config{Listen: "127.0.0.1:0", MetricsListen: "127.0.0.1:0", TestRelaxInviteLimits: true}, warns: true},
		{name: "on, every interface", cfg: Config{Listen: "0.0.0.0:0", TestRelaxInviteLimits: true}, wantErr: "not loopback"},
		{name: "on, no host", cfg: Config{Listen: ":0", TestRelaxInviteLimits: true}, wantErr: "not loopback"},
		{name: "on, loopback behind a trusted proxy", cfg: Config{Listen: "127.0.0.1:0", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, TestRelaxInviteLimits: true}, wantErr: "trusted proxies"},
		{name: "on, loopback with a remote metrics listener", cfg: Config{Listen: "127.0.0.1:0", MetricsListen: "0.0.0.0:0", MetricsAllowRemote: true, TestRelaxInviteLimits: true}, wantErr: "metrics listener"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs syncBuffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			srv := newTestServerWithConfig(t, tt.cfg)
			srv.cfg.Listen = tt.cfg.Listen
			err := srv.Listen()
			t.Cleanup(func() {
				if srv.ln != nil {
					_ = srv.ln.Close()
				}
				if srv.metricsLn != nil {
					_ = srv.metricsLn.Close()
				}
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Listen = %v, want an error containing %q", err, tt.wantErr)
				}
				if srv.ln != nil || srv.metricsLn != nil {
					t.Error("a refused server is left bound")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(logs.buf.String(), "TEST-ONLY relaxed"); got != tt.warns {
				t.Errorf("warning logged = %v, want %v; log:\n%s", got, tt.warns, logs.buf.String())
			}
		})
	}
}

// addrListener is a listener that only reports an address, for
// checkRelaxedLimitsLoopback.
type addrListener struct {
	net.Listener
	addr net.Addr
}

func (l addrListener) Addr() net.Addr { return l.addr }

// TestCheckRelaxedLimitsLoopback: the relaxed limits are judged on the bound
// address, so every loopback form passes and anything else, including a
// listener that is not TCP, is refused.
func TestCheckRelaxedLimitsLoopback(t *testing.T) {
	tests := []struct {
		name string
		addr net.Addr
		ok   bool
	}{
		{name: "IPv4 loopback", addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}, ok: true},
		{name: "another 127/8 address", addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 1}, ok: true},
		{name: "IPv6 loopback", addr: &net.TCPAddr{IP: net.IPv6loopback, Port: 1}, ok: true},
		{name: "IPv4-mapped loopback", addr: &net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1"), Port: 1}, ok: true},
		{name: "every interface", addr: &net.TCPAddr{IP: net.IPv4zero, Port: 1}},
		{name: "IPv6 every interface", addr: &net.TCPAddr{IP: net.IPv6unspecified, Port: 1}},
		{name: "a LAN address", addr: &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 1}},
		{name: "not TCP", addr: &net.UnixAddr{Name: "/tmp/conchd.sock", Net: "unix"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRelaxedLimitsLoopback("HTTP", addrListener{addr: tt.addr})
			if (err == nil) != tt.ok {
				t.Errorf("checkRelaxedLimitsLoopback(%v) = %v, want ok %v", tt.addr, err, tt.ok)
			}
		})
	}
}
