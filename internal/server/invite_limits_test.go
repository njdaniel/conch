package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestTokenBucket(t *testing.T) {
	l := inviteLimit{"t", 3, 30 * time.Second} // one token every 10 s
	t0 := time.Unix(1_000_000, 0)
	type step struct {
		at        time.Duration
		refund    bool
		wantOK    bool
		wantRetry time.Duration
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"burst then refusal", []step{
			{0, false, true, 0}, {0, false, true, 0}, {0, false, true, 0},
			{0, false, false, 10 * time.Second},
			{4 * time.Second, false, false, 6 * time.Second},
		}},
		{"refills evenly", []step{
			{0, false, true, 0}, {0, false, true, 0}, {0, false, true, 0},
			{10 * time.Second, false, true, 0},
			{10 * time.Second, false, false, 10 * time.Second},
			{40 * time.Second, false, true, 0}, {40 * time.Second, false, true, 0}, {40 * time.Second, false, true, 0},
			{40 * time.Second, false, false, 10 * time.Second},
		}},
		{"never more than the burst", []step{
			{time.Hour, false, true, 0}, {time.Hour, false, true, 0}, {time.Hour, false, true, 0},
			{time.Hour, false, false, 10 * time.Second},
		}},
		{"a refund gives one back", []step{
			{0, false, true, 0}, {0, false, true, 0}, {0, false, true, 0},
			{0, true, false, 0},
			{0, false, true, 0},
			{0, false, false, 10 * time.Second},
		}},
		{"a clock that steps back refills nothing", []step{
			{time.Minute, false, true, 0}, {time.Minute, false, true, 0}, {time.Minute, false, true, 0},
			{0, false, false, 10 * time.Second},
			{time.Minute, false, false, 10 * time.Second},
		}},
		{"retry is at least a second", []step{
			{0, false, true, 0}, {0, false, true, 0}, {0, false, true, 0},
			{9*time.Second + 999*time.Millisecond, false, false, time.Second},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b tokenBucket
			for i, s := range tt.steps {
				if s.refund {
					b.refund(l, t0.Add(s.at))
					continue
				}
				ok, retry := b.take(l, t0.Add(s.at))
				if ok != s.wantOK || retry != s.wantRetry {
					t.Errorf("step %d: take = %v, %s; want %v, %s", i, ok, retry, s.wantOK, s.wantRetry)
				}
			}
		})
	}
}

// Idle buckets are pruned, and past maxInviteLimitKeys new keys share one.
func TestBucketSetBounded(t *testing.T) {
	l := inviteLimit{"t", 1, time.Minute}
	t0 := time.Unix(1_000_000, 0)
	var s bucketSet
	for i := range maxInviteLimitKeys + 10 {
		s.bucket(l, strconv.Itoa(i), "overflow|a", t0).take(l, t0)
	}
	if n := len(s.buckets); n != maxInviteLimitKeys+1 {
		t.Fatalf("buckets = %d, want the cap plus the overflow bucket", n)
	}
	if ok, _ := s.bucket(l, "another", "overflow|a", t0).take(l, t0); ok {
		t.Error("a new key past the cap did not share its network's spent overflow bucket")
	}
	// Another network past the cap has its own overflow bucket: a flood that
	// filled the table from one network does not lock out the rest.
	if ok, _ := s.bucket(l, "elsewhere", "overflow|b", t0).take(l, t0); !ok {
		t.Error("a key from another network shared the flooding network's overflow bucket")
	}
	// The overflow buckets are bounded too: past them, everyone shares one.
	for i := range maxInviteOverflowKeys + 10 {
		s.bucket(l, "k", "overflow|n"+strconv.Itoa(i), t0)
	}
	if n := len(s.buckets); n > maxInviteLimitKeys+maxInviteOverflowKeys+1 {
		t.Errorf("buckets = %d, want at most %d", n, maxInviteLimitKeys+maxInviteOverflowKeys+1)
	}
	// A minute on, every bucket has refilled and is dropped.
	s.bucket(l, "later", "overflow|a", t0.Add(time.Minute))
	if n := len(s.buckets); n != 1 {
		t.Errorf("buckets after the prune = %d, want 1", n)
	}
}

// An address past a full table shares the bucket of its IPv4 /16 or IPv6 /32,
// so a flood from one IPv6 /48 cannot drain the bucket of other networks.
func TestOverflowKey(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"2001:db8:1:1::1", "2001:db8:ffff:2::9", true},
		{"2001:db8:1:1::1", "2001:db9::1", false},
		{"198.51.1.2", "198.51.200.7", true},
		{"198.51.1.2", "198.52.1.2", false},
	} {
		got := overflowKey(netip.MustParseAddr(tc.a)) == overflowKey(netip.MustParseAddr(tc.b))
		if got != tc.same {
			t.Errorf("overflowKey(%s) == overflowKey(%s) is %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
	if k := overflowKey(netip.Addr{}); k != inviteOverflowKey {
		t.Errorf("overflowKey(invalid) = %q, want %q", k, inviteOverflowKey)
	}
}

// Redeem and preview are limited per client address, separately, and a
// refusal is a 429 with Retry-After and rate_limited.
func TestInviteLimitsPerAddress(t *testing.T) {
	for _, route := range []string{"/v1/invites/redeem", "/v1/invites/preview"} {
		t.Run(route, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			limits := looseInviteLimits()
			limits.redeemPerAddress = inviteLimit{"redeem_per_address", 3, 3 * time.Minute}
			limits.previewPerAddress = inviteLimit{"preview_per_address", 3, 3 * time.Minute}
			f.srv.setInviteLimits(limits)
			body := inviteJSON(t, schema.RedeemInviteRequestV1{Code: fakeInviteCode, Name: "someone"})
			if strings.HasSuffix(route, "preview") {
				body = inviteJSON(t, schema.InvitePreviewRequestV1{Code: fakeInviteCode})
			}
			call := func(remote string) *httptest.ResponseRecorder {
				return f.send(t, "POST", route, "", body, remote)
			}
			for i := range 3 {
				if rec := call("198.51.100.1:1000"); rec.Code != http.StatusNotFound {
					t.Fatalf("attempt %d = %d %s", i, rec.Code, rec.Body)
				}
			}
			rec := call("198.51.100.1:2000")
			assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
			if got := rec.Header().Get("Retry-After"); got != "60" {
				t.Errorf("Retry-After = %q, want 60", got)
			}
			if e := decodeBody[schema.Error](t, rec); e.RetryAfterSeconds != 60 {
				t.Errorf("retry_after_seconds = %d, want 60", e.RetryAfterSeconds)
			}
			// Another address has its own bucket; the other route's is apart.
			if rec := call("198.51.100.2:1000"); rec.Code != http.StatusNotFound {
				t.Errorf("another address = %d, want 404", rec.Code)
			}
			// A malformed body costs an attempt too: the limit comes first.
			f.advance(time.Minute)
			if rec := f.send(t, "POST", route, "", "not json", "198.51.100.1:1000"); rec.Code != http.StatusBadRequest {
				t.Errorf("after the refill = %d, want 400", rec.Code)
			}
			assertErrorBody(t, call("198.51.100.1:1000"), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		})
	}
}

// Preview and redeem from one address spend separate buckets.
func TestInviteLimitsPreviewAndRedeemApart(t *testing.T) {
	f := newInviteFixture(t, Config{})
	limits := looseInviteLimits()
	limits.redeemPerAddress = inviteLimit{"redeem_per_address", 1, time.Hour}
	limits.previewPerAddress = inviteLimit{"preview_per_address", 1, time.Hour}
	f.srv.setInviteLimits(limits)
	if rec := f.preview(t, fakeInviteCode); rec.Code != http.StatusNotFound {
		t.Fatalf("preview = %d", rec.Code)
	}
	if rec := f.redeem(t, fakeInviteCode, "someone"); rec.Code != http.StatusNotFound {
		t.Errorf("redeem after a preview = %d, want 404", rec.Code)
	}
}

// Successful redemptions are limited server-wide, whatever the address;
// failed ones do not count.
func TestInviteRedeemLimitedServerWide(t *testing.T) {
	f := newInviteFixture(t, Config{})
	limits := looseInviteLimits()
	limits.redeemGlobal = inviteLimit{"redeem_server_wide", 2, time.Minute}
	f.srv.setInviteLimits(limits)
	link := f.createInvite(t, f.rootTok, openLink("general"))
	redeem := func(i int, code, name string) *httptest.ResponseRecorder {
		return f.send(t, "POST", "/v1/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV1{Code: code, Name: name}), fmt.Sprintf("198.51.100.%d:1", i))
	}
	for i := range 5 { // refused at the store: they give the token back
		assertErrorBody(t, redeem(i, fakeInviteCode, "nobody"), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
	}
	assertErrorBody(t, redeem(10, link.Code, "alice"), http.StatusConflict, schema.ErrorCodePrincipalExists)
	for i, name := range []string{"one", "two"} {
		if rec := redeem(20+i, link.Code, name); rec.Code != http.StatusCreated {
			t.Fatalf("redeem %s = %d %s", name, rec.Code, rec.Body)
		}
	}
	rec := redeem(30, link.Code, "three")
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if rec.Header().Get("Retry-After") != "30" {
		t.Errorf("Retry-After = %q, want 30", rec.Header().Get("Retry-After"))
	}
	f.advance(30 * time.Second)
	if rec := redeem(31, link.Code, "three"); rec.Code != http.StatusCreated {
		t.Errorf("after the refill = %d %s", rec.Code, rec.Body)
	}
}

// Making invites is limited per principal, operators included.
func TestInviteCreateLimitedPerPrincipal(t *testing.T) {
	f := newInviteFixture(t, Config{})
	limits := looseInviteLimits()
	limits.createPerPrincipal = inviteLimit{"create_per_principal", 2, time.Hour}
	f.srv.setInviteLimits(limits)
	f.createInvite(t, f.rootTok, openLink("general"))
	// A refused create spends a token as well.
	assertErrorBody(t, f.send(t, "POST", "/v1/invites", f.rootTok, `{}`, ""), http.StatusBadRequest, "invalid_request")
	rec := f.send(t, "POST", "/v1/invites", f.rootTok, inviteJSON(t, openLink("general")), "")
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if rec.Header().Get("Retry-After") != "1800" {
		t.Errorf("Retry-After = %q, want 1800", rec.Header().Get("Retry-After"))
	}
	// alice has a bucket of her own (and is refused for want of the right).
	assertErrorBody(t, f.send(t, "POST", "/v1/invites", f.aliceTok, inviteJSON(t, personal("red")), ""), http.StatusForbidden, schema.ErrorCodeInviteNotAllowed)
	var rows []string
	for _, e := range f.audit(t) {
		if e.Action == auditInviteRateLimited {
			rows = append(rows, e.Actor+" "+e.Subject+" "+e.Detail)
		}
	}
	want := fmt.Sprintf("principal:%d POST /v1/invites limit=create_per_principal max=2 window_seconds=3600 count=1", f.root.ID)
	if len(rows) != 1 || rows[0] != want {
		t.Errorf("invite_rate_limited rows = %q, want %q", rows, want)
	}
}

// A burst of 1,000 refused redeems writes a small, fixed number of audit
// rows, as does a burst of 1,000 invalid codes; neither carries the address.
func TestInviteRefusalAuditIsBounded(t *testing.T) {
	// thinned is how many rows writeNth keeps of 999 or 1,000 refusals: the
	// first eight, then the 16th, 32nd, 64th, 128th, 256th and 512th.
	const thinned = thinAfter + 6
	tests := []struct {
		name   string
		limits func(*inviteLimits)
		action string
		detail string
		// invalid is how many invite_redeem_refused rows come with the rate
		// limit's: the one attempt it lets through has an invalid code.
		invalid int
	}{
		{"rate limited", func(l *inviteLimits) { l.redeemPerAddress = inviteLimit{"redeem_per_address", 1, time.Hour} },
			auditInviteRateLimited, "limit=redeem_per_address max=1 window_seconds=3600 count=", 1},
		{"invalid codes", func(*inviteLimits) {}, auditInviteRedeemRefused, "reason=invite_invalid count=", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			limits := looseInviteLimits()
			tt.limits(&limits)
			f.srv.setInviteLimits(limits)
			body := inviteJSON(t, schema.RedeemInviteRequestV1{Code: fakeInviteCode, Name: "someone"})
			before := len(f.audit(t))
			for range 1000 {
				f.send(t, "POST", "/v1/invites/redeem", "", body, "203.0.113.50:1")
			}
			var rows []string
			invalid := 0
			for _, e := range f.audit(t)[before:] {
				if e.Action == auditInviteRedeemRefused && tt.action != auditInviteRedeemRefused {
					invalid++
					continue
				}
				if e.Action != tt.action {
					t.Errorf("unexpected row %s %s", e.Action, e.Detail)
					continue
				}
				if e.Actor != inviteAnonymousAuditActor || e.Subject != "POST /v1/invites/redeem" || !strings.HasPrefix(e.Detail, tt.detail) {
					t.Errorf("row = %s %s %s", e.Actor, e.Subject, e.Detail)
				}
				if strings.Contains(e.Detail, "203.0.113") || strings.Contains(e.Detail, fakeInviteCode) {
					t.Errorf("row carries the address or the code: %s", e.Detail)
				}
				rows = append(rows, e.Detail)
			}
			if len(rows) != thinned || invalid != tt.invalid {
				t.Errorf("%d rows (and %d invalid-code rows) for 1,000 refusals, want %d (and %d): %q", len(rows), invalid, thinned, tt.invalid, rows)
			}
			// A new audit window starts the count again.
			f.advance(denialCoalesceWindow)
			f.send(t, "POST", "/v1/invites/redeem", "", body, "203.0.113.50:1")
			if last := f.audit(t); !strings.HasSuffix(last[len(last)-1].Detail, " count=1") {
				t.Errorf("first row of a new window = %q", last[len(last)-1].Detail)
			}
		})
	}
}

// Behind a trusted proxy each forwarded client has its own bucket; from a
// peer that is not trusted the header is ignored.
func TestInviteLimitsBehindAProxy(t *testing.T) {
	f := newInviteFixture(t, Config{TrustedProxies: mustPrefixes(t, "127.0.0.1/32,::1/128")})
	limits := looseInviteLimits()
	limits.redeemPerAddress = inviteLimit{"redeem_per_address", 1, time.Hour}
	f.srv.setInviteLimits(limits)
	body := inviteJSON(t, schema.RedeemInviteRequestV1{Code: fakeInviteCode, Name: "someone"})
	call := func(remote string, xff ...string) int {
		return f.send(t, "POST", "/v1/invites/redeem", "", body, remote, xff...).Code
	}
	steps := []struct {
		name   string
		remote string
		xff    []string
		want   int
	}{
		{"client A through the proxy", "127.0.0.1:4000", []string{"198.51.100.1"}, http.StatusNotFound},
		{"client A again", "127.0.0.1:4001", []string{"198.51.100.1"}, http.StatusTooManyRequests},
		{"client B through the proxy", "127.0.0.1:4002", []string{"198.51.100.2"}, http.StatusNotFound},
		{"client C over IPv6 loopback", "[::1]:4003", []string{"2001:db8::3"}, http.StatusNotFound},
		{"client A spoofing a left entry", "127.0.0.1:4004", []string{"198.51.100.9, 198.51.100.1"}, http.StatusTooManyRequests},
		{"the proxy itself, no header", "127.0.0.1:4005", nil, http.StatusNotFound},
		{"the proxy itself again", "127.0.0.1:4006", nil, http.StatusTooManyRequests},
		{"untrusted peer", "203.0.113.5:5000", []string{"198.51.100.50"}, http.StatusNotFound},
		{"untrusted peer, another header", "203.0.113.5:5001", []string{"198.51.100.51"}, http.StatusTooManyRequests},
		{"untrusted peer claims client B's bucket", "203.0.113.6:5000", []string{"198.51.100.2"}, http.StatusNotFound},
	}
	for _, s := range steps {
		if got := call(s.remote, s.xff...); got != s.want {
			t.Errorf("%s: %d, want %d", s.name, got, s.want)
		}
	}
}
