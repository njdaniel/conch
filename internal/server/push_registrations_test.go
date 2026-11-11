package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/push"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Push registrations through the handlers (issue #1714, Mobile M11-B). The
// store's own tests hold revocation and the sweep; this file holds the routes:
// access, every refusal in its order, the limits, T13 isolation and the leak
// test. Nothing here uses the network: the address check gets a fake resolver.

const (
	testPushP256DH = "BGcgk_hM5FumxxG6GMUm7aSR_ccjoEV9Par_u_xOGOW2Yhl_pplE_MS0Nq75kpvAP0RoZwEi-1HPIqR4w4tUGcE"
	testPushAuth   = "UjWbXUnDSgTzOXcC2Bq_Xg"
)

// fakeResolver answers LookupNetIP from a table; a host it does not know, or
// one mapped to nil, fails to resolve.
type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	answers, ok := f[host]
	if !ok || answers == nil {
		return nil, errors.New("no such host: " + host)
	}
	var out []netip.Addr
	for _, a := range answers {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

var testPushHosts = fakeResolver{
	"push.example":    {"93.184.216.34"},
	"private.example": {"10.1.2.3"},
	"mixed.example":   {"93.184.216.34", "192.168.0.9"},
	"lan.example":     {"192.168.1.20"},
	"loop.example":    {"127.0.0.1"},
	"down.example":    nil,
}

func pushBody(endpoint string) string {
	return fmt.Sprintf(`{"transport":"webpush","platform":"web","app":"https://app.example","endpoint":%q,"p256dh":%q,"auth":%q}`,
		endpoint, testPushP256DH, testPushAuth)
}

type pushFixture struct {
	*authFixture
}

func newPushFixture(t *testing.T, cfg PushConfig) *pushFixture {
	t.Helper()
	if cfg.WebPush && cfg.VAPID == nil {
		key, err := push.LoadOrCreateVAPIDKey(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cfg.VAPID = key
	}
	if cfg.Resolver == nil {
		cfg.Resolver = testPushHosts
	}
	f := &pushFixture{authFixture: newAuthFixtureWithConfig(t, Config{AuthMode: AuthRequired, Push: cfg})}
	f.srv.relaxNotificationLimits()
	return f
}

func (f *pushFixture) credential(t *testing.T, p store.Principal, label string) string {
	t.Helper()
	_, tok, err := f.srv.store.CreateCredential(context.Background(), "system", p.ID, label, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *pushFixture) register(t *testing.T, tok, endpoint string) schema.PushRegistrationV1 {
	t.Helper()
	rec := f.do(t, "POST", "/v1/push/registrations", tok, pushBody(endpoint))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.PushRegistrationV1](t, rec)
}

func TestPushConfigRoute(t *testing.T) {
	tests := []struct {
		name string
		cfg  PushConfig
		want string // "" means webpush with a key
	}{
		{"no flag", PushConfig{}, `{"transports":[]}` + "\n"},
		{"allow-private alone changes nothing", PushConfig{AllowPrivate: true}, `{"transports":[]}` + "\n"},
		{"webpush without a key fails closed", PushConfig{WebPush: true, VAPID: nil}, ""},
		{"webpush", PushConfig{WebPush: true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f *pushFixture
			if tt.name == "webpush without a key fails closed" {
				f = &pushFixture{newAuthFixtureWithConfig(t, Config{AuthMode: AuthRequired, Push: tt.cfg})}
				tt.want = `{"transports":[]}` + "\n"
			} else {
				f = newPushFixture(t, tt.cfg)
			}
			// A person and an agent both read it.
			for name, tok := range map[string]string{"person": f.aliceTok, "agent": f.botTok} {
				rec := f.do(t, "GET", "/v1/push/config", tok, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
				}
				if tt.want != "" {
					if rec.Body.String() != tt.want {
						t.Errorf("%s: %q, want %q", name, rec.Body, tt.want)
					}
					continue
				}
				cfg := decodeBody[schema.PushConfigV1](t, rec)
				if err := cfg.Validate(); err != nil || len(cfg.Transports) != 1 || cfg.Transports[0] != schema.PushTransportWebPush {
					t.Errorf("%s: config %+v: %v", name, cfg, err)
				}
				if cfg.VAPIDPublicKey != f.srv.cfg.Push.VAPID.PublicKey() {
					t.Errorf("%s: wrong key", name)
				}
			}
			if rec := f.do(t, "GET", "/v1/push/config", "", ""); rec.Code != http.StatusUnauthorized {
				t.Errorf("no credential = %d", rec.Code)
			}
		})
	}
}

func TestPushRegistrationRefusals(t *testing.T) {
	long := "https://push.example/" + strings.Repeat("a", schema.MaxPushEndpointBytes-len("https://push.example/")+1)
	tests := []struct {
		name   string
		cfg    PushConfig
		tok    func(f *pushFixture) string
		body   string
		status int
		code   string
	}{
		{"created", PushConfig{WebPush: true}, nil, pushBody("https://push.example/sub/1"), 201, ""},
		{"created on the default port, named", PushConfig{WebPush: true}, nil, pushBody("https://push.example:443/sub/1"), 201, ""},
		{"push off", PushConfig{}, nil, pushBody("https://push.example/x"), 409, schema.ErrorCodePushNotConfigured},
		{"relay", PushConfig{WebPush: true}, nil,
			`{"transport":"relay","platform":"ios","app":"org.example","provider":"apns","token":"abc"}`, 409, schema.ErrorCodePushTransportUnavailable},
		{"relay with push off", PushConfig{}, nil,
			`{"transport":"relay","platform":"ios","app":"org.example","provider":"apns","token":"abc"}`, 409, schema.ErrorCodePushNotConfigured},
		{"not JSON", PushConfig{WebPush: true}, nil, `endpoint=x`, 400, "invalid_request"},
		{"unknown field", PushConfig{WebPush: true}, nil, `{"transport":"webpush","platform":"web","app":"a","push":1}`, 400, "invalid_request"},
		{"http scheme", PushConfig{WebPush: true}, nil, pushBody("http://push.example/x"), 400, "invalid_request"},
		{"1001 bytes", PushConfig{WebPush: true}, nil, pushBody(long), 400, "invalid_request"},
		{"p256dh not on the curve", PushConfig{WebPush: true}, nil,
			strings.Replace(pushBody("https://push.example/x"), testPushP256DH, "BAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 1), 400, "invalid_request"},
		{"private answer", PushConfig{WebPush: true}, nil, pushBody("https://private.example/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"mixed answer", PushConfig{WebPush: true}, nil, pushBody("https://mixed.example/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"loopback name", PushConfig{WebPush: true}, nil, pushBody("https://loop.example/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"lookup failure", PushConfig{WebPush: true}, nil, pushBody("https://down.example/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"unknown host", PushConfig{WebPush: true}, nil, pushBody("https://nowhere.example/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"IPv4 literal", PushConfig{WebPush: true}, nil, pushBody("https://127.0.0.1/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"IPv6 literal", PushConfig{WebPush: true}, nil, pushBody("https://[::1]/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"metadata address", PushConfig{WebPush: true}, nil, pushBody("https://169.254.169.254/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"mapped IPv6 literal", PushConfig{WebPush: true}, nil, pushBody("https://[::ffff:10.0.0.1]/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"a LAN answer, allow-private off", PushConfig{WebPush: true}, nil, pushBody("https://lan.example/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"a LAN answer, allow-private on", PushConfig{WebPush: true, AllowPrivate: true}, nil, pushBody("https://lan.example/x"), 201, ""},
		{"loopback stays refused with allow-private on", PushConfig{WebPush: true, AllowPrivate: true}, nil, pushBody("https://loop.example/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"a mixed public and LAN answer, allow-private on", PushConfig{WebPush: true, AllowPrivate: true}, nil, pushBody("https://mixed.example/x"), 201, ""},
		{"the unspecified address, allow-private on", PushConfig{WebPush: true, AllowPrivate: true}, nil, pushBody("https://0.0.0.0/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"a public name on another port", PushConfig{WebPush: true}, nil, pushBody("https://push.example:8443/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"a public name on another port, allow-private on", PushConfig{WebPush: true, AllowPrivate: true}, nil, pushBody("https://push.example:8443/x"), 201, ""},
		{"port 0", PushConfig{WebPush: true, AllowPrivate: true}, nil, pushBody("https://push.example:0/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"port out of range", PushConfig{WebPush: true, AllowPrivate: true}, nil, pushBody("https://push.example:70000/x"), 400, schema.ErrorCodePushEndpointRefused},
		{"an agent", PushConfig{WebPush: true}, func(f *pushFixture) string { return f.botTok }, pushBody("https://push.example/x"), 403, schema.ErrorCodeHumanRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPushFixture(t, tt.cfg)
			tok := f.aliceTok
			if tt.tok != nil {
				tok = tt.tok(f)
			}
			rec := f.do(t, "POST", "/v1/push/registrations", tok, tt.body)
			if rec.Code != tt.status {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tt.status)
			}
			if tt.status == 201 {
				reg := decodeBody[schema.PushRegistrationV1](t, rec)
				if err := reg.Validate(); err != nil {
					t.Errorf("answer is not valid: %v", err)
				}
				for _, secret := range []string{"push.example", testPushP256DH, testPushAuth, "/sub/1"} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Errorf("the answer holds %q: %s", secret, rec.Body)
					}
				}
				return
			}
			assertErrorBody(t, rec, tt.status, tt.code)
			// No refusal echoes any part of the request.
			for _, echo := range []string{"example", "127.0.0.1", "169.254", "::1", testPushP256DH, testPushAuth} {
				if strings.Contains(rec.Body.String(), echo) {
					t.Errorf("the refusal holds %q: %s", echo, rec.Body)
				}
			}
			if tt.code == schema.ErrorCodePushEndpointRefused {
				if got := decodeBody[schema.Error](t, rec).Message; got != pushEndpointRefusedMessage {
					t.Errorf("message = %q, want the fixed one", got)
				}
			}
			if regs, _ := f.srv.store.ListOwnPushRegistrations(context.Background(), 1); len(regs) != 0 {
				t.Errorf("a refusal stored %d registrations", len(regs))
			}
		})
	}

	t.Run("every endpoint refusal is the same bytes", func(t *testing.T) {
		f := newPushFixture(t, PushConfig{WebPush: true})
		var first string
		for _, ep := range []string{"https://private.example/a", "https://mixed.example/b", "https://down.example/c", "https://127.0.0.1/d", "https://push.example:8443/e"} {
			rec := f.do(t, "POST", "/v1/push/registrations", f.aliceTok, pushBody(ep))
			body := rec.Body.String()
			if first == "" {
				first = body
			}
			if rec.Code != 400 || body != first {
				t.Errorf("%s: %d %q differs from %q", ep, rec.Code, body, first)
			}
		}
	})
}

func TestPushRegistrationLimitsAndRate(t *testing.T) {
	t.Run("per credential, per principal", func(t *testing.T) {
		f := newPushFixture(t, PushConfig{WebPush: true})
		f.register(t, f.aliceTok, "https://push.example/1")
		f.register(t, f.aliceTok, "https://push.example/2")
		rec := f.do(t, "POST", "/v1/push/registrations", f.aliceTok, pushBody("https://push.example/3"))
		assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodeTooManyPushRegistrations)
		// Seven credentials in all fill the principal's 16.
		for i := 0; i < 6; i++ {
			tok := f.credential(t, f.alice, fmt.Sprint("c", i))
			f.register(t, tok, "https://push.example/a")
			f.register(t, tok, "https://push.example/b")
		}
		last := f.credential(t, f.alice, "last")
		f.register(t, last, "https://push.example/a")
		f.register(t, last, "https://push.example/b")
		extra := f.credential(t, f.alice, "extra")
		rec = f.do(t, "POST", "/v1/push/registrations", extra, pushBody("https://push.example/b"))
		assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodeTooManyPushRegistrations)
		// Someone else is not counted.
		f.register(t, f.rootTok, "https://push.example/root")
	})
	t.Run("rate limit: 429 with Retry-After, nothing stored", func(t *testing.T) {
		f := newPushFixture(t, PushConfig{WebPush: true})
		f.srv.setNotificationLimits(inviteLimit{"w", 2, time.Hour}, inviteLimit{"r", 100, time.Hour})
		f.register(t, f.aliceTok, "https://push.example/1")
		f.register(t, f.aliceTok, "https://push.example/2")
		for _, c := range []struct{ method, path, body string }{
			{"POST", "/v1/push/registrations", pushBody("https://push.example/3")},
			{"PUT", "/v1/push/registrations/anything", pushBody("https://push.example/3")},
			{"DELETE", "/v1/push/registrations/anything", ""},
		} {
			rec := f.do(t, c.method, c.path, f.aliceTok, c.body)
			assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
			if rec.Header().Get("Retry-After") == "" {
				t.Errorf("%s: no Retry-After", c.method)
			}
		}
		regs, err := f.srv.store.ListOwnPushRegistrations(context.Background(), 2)
		_ = regs
		if err != nil {
			t.Fatal(err)
		}
		if rec := f.do(t, "GET", "/v1/push/registrations/mine", f.aliceTok, ""); len(decodeBody[schema.ListOwnPushRegistrationsResponseV1](t, rec).Registrations) != 2 {
			t.Errorf("a limited write changed the list: %s", rec.Body)
		}
	})
}

func TestPushRegistrationOwnRoutes(t *testing.T) {
	f := newPushFixture(t, PushConfig{WebPush: true})
	second := f.credential(t, f.alice, "desk")
	mine := f.register(t, f.aliceTok, "https://push.example/one")
	other := f.register(t, second, "https://push.example/two")

	t.Run("/mine lists the calling credential's, with no target", func(t *testing.T) {
		rec := f.do(t, "GET", "/v1/push/registrations/mine", f.aliceTok, "")
		list := decodeBody[schema.ListOwnPushRegistrationsResponseV1](t, rec)
		if err := list.Validate(); err != nil || len(list.Registrations) != 1 || list.Registrations[0].ID != mine.ID {
			t.Fatalf("list = %+v, %v", list, err)
		}
		for _, secret := range []string{"push.example", testPushP256DH, testPushAuth, other.ID} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("/mine holds %q", secret)
			}
		}
	})

	t.Run("credentials/mine shows which sign-ins have push", func(t *testing.T) {
		rec := f.do(t, "GET", "/v1/credentials/mine", f.aliceTok, "")
		got := decodeBody[schema.ListOwnCredentialsResponseV1](t, rec)
		pushes := 0
		for _, c := range got.Credentials {
			if c.Push {
				pushes++
			}
		}
		if len(got.Credentials) != 2 || pushes != 2 {
			t.Errorf("credentials = %+v", got.Credentials)
		}
		rec = f.do(t, "GET", "/v1/credentials/mine", f.rootTok, "")
		for _, c := range decodeBody[schema.ListOwnCredentialsResponseV1](t, rec).Credentials {
			if c.Push {
				t.Errorf("root has no registration: %+v", c)
			}
		}
	})

	t.Run("PUT replaces the target, keeps the id, changes the fingerprint", func(t *testing.T) {
		rec := f.do(t, "PUT", "/v1/push/registrations/"+mine.ID, f.aliceTok, pushBody("https://push.example/replaced"))
		if rec.Code != 200 {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
		}
		got := decodeBody[schema.PushRegistrationV1](t, rec)
		if got.ID != mine.ID || got.TargetFingerprint == mine.TargetFingerprint {
			t.Errorf("replaced = %+v, was %+v", got, mine)
		}
		// The same target again: the same fingerprint.
		again := decodeBody[schema.PushRegistrationV1](t, f.do(t, "PUT", "/v1/push/registrations/"+mine.ID, f.aliceTok, pushBody("https://push.example/replaced")))
		if again.TargetFingerprint != got.TargetFingerprint {
			t.Errorf("fingerprint moved with the target unchanged")
		}
		for name, body := range map[string]string{
			"another transport": `{"transport":"relay","platform":"web","app":"a","provider":"fcm","token":"t"}`,
			"another platform":  strings.Replace(pushBody("https://push.example/x"), `"web"`, `"android"`, 1),
		} {
			if rec := f.do(t, "PUT", "/v1/push/registrations/"+mine.ID, f.aliceTok, body); rec.Code != 400 && rec.Code != 409 {
				t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
			}
		}
		assertErrorBody(t, f.do(t, "PUT", "/v1/push/registrations/"+mine.ID, f.aliceTok, pushBody("https://private.example/x")),
			400, schema.ErrorCodePushEndpointRefused)
		assertErrorBody(t, f.do(t, "PUT", "/v1/push/registrations/"+mine.ID, f.aliceTok,
			`{"transport":"webpush","platform":"android","app":"a","endpoint":"https://push.example/x","p256dh":"`+testPushP256DH+`","auth":"`+testPushAuth+`"}`),
			400, "invalid_request")
	})

	t.Run("T13: another's, an ended and a never-issued id answer the same bytes", func(t *testing.T) {
		gone := f.register(t, f.aliceTok, "https://push.example/gone")
		if rec := f.do(t, "DELETE", "/v1/push/registrations/"+gone.ID, f.aliceTok, ""); rec.Code != 204 {
			t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
		}
		ids := map[string]string{"another credential's": other.ID, "ended": gone.ID, "never issued": "A" + strings.Repeat("b", 42), "malformed": "!!"}
		for _, method := range []string{"PUT", "DELETE"} {
			var want *httptest.ResponseRecorder
			for name, id := range ids {
				body := ""
				if method == "PUT" {
					body = pushBody("https://push.example/x")
				}
				rec := f.do(t, method, "/v1/push/registrations/"+id, f.aliceTok, body)
				assertErrorBody(t, rec, 404, schema.ErrorCodePushRegistrationNotFound)
				if want == nil {
					want = rec
					continue
				}
				if rec.Body.String() != want.Body.String() || rec.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
					t.Errorf("%s %s: %q differs from %q", method, name, rec.Body, want.Body)
				}
			}
		}
		// The other credential's registration is untouched.
		if rec := f.do(t, "GET", "/v1/push/registrations/mine", second, ""); len(decodeBody[schema.ListOwnPushRegistrationsResponseV1](t, rec).Registrations) != 1 {
			t.Errorf("another credential's registration changed")
		}
	})

	t.Run("DELETE ends it with reason self, and works with push off", func(t *testing.T) {
		off := newPushFixture(t, PushConfig{WebPush: true})
		reg := off.register(t, off.aliceTok, "https://push.example/x")
		off.srv.cfg.Push = PushConfig{} // the operator turned push off
		assertErrorBody(t, off.do(t, "PUT", "/v1/push/registrations/"+reg.ID, off.aliceTok, pushBody("https://push.example/y")), 409, schema.ErrorCodePushNotConfigured)
		if rec := off.do(t, "GET", "/v1/push/registrations/mine", off.aliceTok, ""); rec.Code != 200 {
			t.Errorf("/mine with push off = %d", rec.Code)
		}
		if rec := off.do(t, "DELETE", "/v1/push/registrations/"+reg.ID, off.aliceTok, ""); rec.Code != 204 {
			t.Fatalf("DELETE with push off = %d %s", rec.Code, rec.Body)
		}
		found := false
		for _, e := range off.audit(t) {
			if e.Action == schema.AuditActionPushRegistrationEnded && strings.HasSuffix(e.Detail, "reason=self") && strings.Contains(e.Detail, reg.ID) {
				found = true
			}
		}
		if !found {
			t.Error("no push_registration_ended reason=self row")
		}
	})
}

func TestPushRoutesWithNoFlag(t *testing.T) {
	f := newPushFixture(t, PushConfig{})
	if rec := f.do(t, "GET", "/v1/push/registrations/mine", f.aliceTok, ""); rec.Code != 200 || rec.Body.String() != `{"registrations":[]}`+"\n" {
		t.Errorf("/mine = %d %q", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/push/registrations", f.aliceTok, pushBody("https://push.example/x")), 409, schema.ErrorCodePushNotConfigured)
	assertErrorBody(t, f.do(t, "DELETE", "/v1/push/registrations/none", f.aliceTok, ""), 404, schema.ErrorCodePushRegistrationNotFound)
}

func TestPendingFeedRecordsFetch(t *testing.T) {
	f := newPushFixture(t, PushConfig{WebPush: true})
	reg := f.register(t, f.aliceTok, "https://push.example/x")
	f.srv.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if rec := f.do(t, "GET", "/v1/notifications/pending", f.aliceTok, ""); rec.Code != 200 {
		t.Fatalf("pending = %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[schema.ListOwnPushRegistrationsResponseV1](t, f.do(t, "GET", "/v1/push/registrations/mine", f.aliceTok, ""))
	if len(got.Registrations) != 1 || got.Registrations[0].ID != reg.ID || got.Registrations[0].LastFetchAt == nil {
		t.Fatalf("after a fetch: %+v", got)
	}
	if !got.Registrations[0].LastFetchAt.Time().After(time.Now().Add(24 * time.Hour)) {
		t.Errorf("last_fetch_at = %v, want about now+48h", got.Registrations[0].LastFetchAt)
	}
}

// TestPushAuditAndLogsHoldNoTarget is the leak test, in the pattern of
// TestNtfyTransportErrorsDoNotNameTheTopic: register with marked values, run
// every route and every way a registration ends, and require that no response
// body, audit row or log line holds a marker.
func TestPushAuditAndLogsHoldNoTarget(t *testing.T) {
	logs := captureLogs(t)
	const (
		host     = "marker-host-91ac.example"
		path     = "/MARKER-PATH-77d2"
		endpoint = "https://" + host + path
		app      = "org.marker.app9e"
	)
	resolver := fakeResolver{host: {"93.184.216.34"}, "private-marker-3b.example": {"10.0.0.5"}}
	f := newPushFixture(t, PushConfig{WebPush: true, Resolver: resolver})
	markers := []string{host, "MARKER-PATH", "private-marker-3b", testPushP256DH, testPushAuth, "93.184.216.34", "10.0.0.5"}
	var bodies []string
	do := func(method, p, tok, body string) *httptest.ResponseRecorder {
		rec := f.do(t, method, p, tok, body)
		bodies = append(bodies, rec.Body.String())
		for k, v := range rec.Header() {
			bodies = append(bodies, k+": "+strings.Join(v, ","))
		}
		return rec
	}
	req := func(ep string) string {
		return strings.Replace(pushBody(ep), "https://app.example", app, 1)
	}
	a := do("POST", "/v1/push/registrations", f.aliceTok, req(endpoint))
	reg := decodeBody[schema.PushRegistrationV1](t, a)
	do("POST", "/v1/push/registrations", f.aliceTok, req("https://private-marker-3b.example"+path))
	do("POST", "/v1/push/registrations", f.aliceTok, req("https://nowhere-marker.example"+path))
	do("POST", "/v1/push/registrations", f.aliceTok, req("http://"+host+path))
	do("POST", "/v1/push/registrations", f.botTok, req(endpoint))
	do("POST", "/v1/push/registrations", f.aliceTok, `{"transport":"webpush","endpoint":"`+endpoint+`" garbage`)
	do("GET", "/v1/push/config", f.aliceTok, "")
	do("GET", "/v1/push/registrations/mine", f.aliceTok, "")
	do("GET", "/v1/credentials/mine", f.aliceTok, "")
	do("GET", "/v1/notifications/pending", f.aliceTok, "")
	do("PUT", "/v1/push/registrations/"+reg.ID, f.aliceTok, req(endpoint+"2"))
	do("PUT", "/v1/push/registrations/"+reg.ID, f.rootTok, req(endpoint+"3"))
	do("PUT", "/v1/push/registrations/"+reg.ID, f.aliceTok, req("https://private-marker-3b.example"+path))
	// Every way it ends: its own delete, a revocation, the sweep, a key change.
	second := f.register(t, f.aliceTok, endpoint+"/s")
	do("DELETE", "/v1/push/registrations/"+second.ID, f.aliceTok, "")
	do("DELETE", "/v1/push/registrations/"+second.ID, f.aliceTok, "")
	do("DELETE", "/v1/credentials/current", f.aliceTok, "")
	tok := f.credential(t, f.alice, "sweep")
	f.srv.relaxNotificationLimits()
	do("POST", "/v1/push/registrations", tok, req(endpoint+"/t"))
	f.srv.now = func() time.Time { return time.Now().Add(schemaStaleWindow) }
	f.srv.sweepSessions(context.Background())
	do("POST", "/v1/push/registrations", f.rootTok, req(endpoint+"/u"))
	if _, _, err := f.srv.store.ReconcilePushVAPIDKey(context.Background(), "another-key", time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, b := range bodies {
		for _, m := range markers {
			if strings.Contains(b, m) {
				t.Errorf("a response holds %q: %s", m, b)
			}
		}
	}
	for _, e := range f.audit(t) {
		if strings.Contains(e.Actor+e.Action+e.Subject+e.Detail, app) {
			t.Errorf("audit row %s holds the app id", e.Action)
		}
		all := e.Actor + "|" + e.Action + "|" + e.Subject + "|" + e.Detail
		for _, m := range markers {
			if strings.Contains(all, m) {
				t.Errorf("audit row %s holds %q: %s", e.Action, m, all)
			}
		}
		if strings.HasPrefix(e.Action, "push_registration_") && strings.Contains(all, reg.TargetFingerprint) {
			t.Errorf("audit row %s holds the fingerprint", e.Action)
		}
	}
	for _, m := range append(markers, app) {
		if strings.Contains(logs.buf.String(), m) {
			t.Errorf("a log line holds %q", m)
		}
	}
	// The run did end registrations, so the check above had rows to look at.
	ended := 0
	for _, e := range f.audit(t) {
		if e.Action == schema.AuditActionPushRegistrationEnded {
			ended++
		}
	}
	if ended < 4 {
		t.Errorf("only %d push_registration_ended rows: the test did not reach every ending", ended)
	}
}

// schemaStaleWindow is a little more than the stale window.
const schemaStaleWindow = store.PushStaleAfter + time.Hour

// N people-sized requests at once for one credential: at most two rows, and the
// rest 409 too_many_push_registrations (review S5).
func TestPushConcurrentPostsHoldTheLimit(t *testing.T) {
	f := newPushFixture(t, PushConfig{WebPush: true})
	const n = 10
	codes := make(chan int, n)
	bodies := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := f.do(t, "POST", "/v1/push/registrations", f.aliceTok, pushBody(fmt.Sprintf("https://push.example/c%d", i)))
			codes <- rec.Code
			bodies <- rec.Body.String()
		}()
	}
	wg.Wait()
	close(codes)
	close(bodies)
	created, refused := 0, 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			refused++
		default:
			t.Errorf("status %d", c)
		}
	}
	for b := range bodies {
		if strings.Contains(b, `"code"`) && !strings.Contains(b, schema.ErrorCodeTooManyPushRegistrations) {
			t.Errorf("a refusal that is not too_many_push_registrations: %s", b)
		}
	}
	if created != schema.MaxPushRegistrationsPerCredential || refused != n-created {
		t.Errorf("created %d, refused %d; want %d created", created, refused, schema.MaxPushRegistrationsPerCredential)
	}
	got := decodeBody[schema.ListOwnPushRegistrationsResponseV1](t, f.do(t, "GET", "/v1/push/registrations/mine", f.aliceTok, ""))
	if len(got.Registrations) != schema.MaxPushRegistrationsPerCredential {
		t.Errorf("%d active rows", len(got.Registrations))
	}
}
