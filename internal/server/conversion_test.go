package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Accounts (issue #711): Create an account from the bare address, Create an
// account during a guest visit, and the operator's conversion permits. The
// success and refusal matrices check what each route makes and that a
// refusal makes nothing and consumes nothing; the race tests land a policy
// change, an End visit, a sign-out, an expiry, a disable, a taken name or a
// second request in the gap between the hash and the writing transaction;
// and the approval test runs the whole chain to show a conversion grants no
// right to decide.

// hashedRecord is password's record as the server would store it.
func hashedRecord(t *testing.T, srv *Server, password string) string {
	t.Helper()
	n, err := normalizePassword(password)
	if err != nil {
		t.Fatal(err)
	}
	record, err := srv.passwords.hasher.hash(n)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// openBoth is the settings change that lets guests and accounts join from
// the bare address into channels.
func openBoth(channels ...string) schema.PutJoinSettingsRequestV1 {
	return schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(true), OpenJoining: modePtr(schema.OpenJoiningBoth), OpenChannels: &channels}
}

// joinAccount asks the bare address for an account called name, with
// testJoinPassword, from remote.
func (f *inviteFixture) joinAccount(t *testing.T, name, remote string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/join/account", "", inviteJSON(t, schema.JoinAccountRequestV1{Name: name, Password: testJoinPassword}), remote)
}

func mustAccount(t *testing.T, rec *httptest.ResponseRecorder) schema.JoinAccountResponseV1 {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("join account = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: the answer carries a token", got)
	}
	return decodeBody[schema.JoinAccountResponseV1](t, rec)
}

// convert asks, with the guest's token, to become name with
// testJoinPassword, and the permit when one is given.
func (f *inviteFixture) convert(t *testing.T, token, name, permit string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/guest/convert", token,
		inviteJSON(t, schema.ConvertGuestRequestV1{Name: name, Password: testJoinPassword, Permit: permit}), "")
}

func mustConverted(t *testing.T, rec *httptest.ResponseRecorder) schema.ConvertGuestResponseV1 {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("convert = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: the answer carries a token", got)
	}
	return decodeBody[schema.ConvertGuestResponseV1](t, rec)
}

func (f *inviteFixture) whoami(t *testing.T, token string) (int, schema.WhoAmIResponseV1) {
	t.Helper()
	rec := f.send(t, "GET", "/v1/whoami", token, "", "")
	if rec.Code != http.StatusOK {
		return rec.Code, schema.WhoAmIResponseV1{}
	}
	return rec.Code, decodeBody[schema.WhoAmIResponseV1](t, rec)
}

// passwordLogin signs name in with testJoinPassword, as from a new device.
func (f *inviteFixture) passwordLogin(t *testing.T, name string) schema.LoginResponseV1 {
	t.Helper()
	rec := f.send(t, "POST", "/v1/login", "", inviteJSON(t, schema.LoginRequestV1{Name: name, Password: testJoinPassword}), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login as %s = %d %s", name, rec.Code, rec.Body)
	}
	out := decodeBody[schema.LoginResponseV1](t, rec)
	if out.Next != schema.LoginNextSignedIn || out.Token == "" {
		t.Fatalf("login as %s = %+v", name, out)
	}
	return out
}

// membership is one channel_members row of a principal, as the store holds
// it.
type membership struct {
	channel, historyAfter int64
	canDecide             bool
}

func (f *loginFixture) memberships(t *testing.T, id int64) []membership {
	t.Helper()
	raw := f.queryString(t, fmt.Sprintf(
		`SELECT COALESCE(group_concat(channel_id || ':' || COALESCE(history_after, -1) || ':' || can_decide, ','), '')
		 FROM (SELECT * FROM channel_members WHERE principal_id = %d ORDER BY channel_id)`, id))
	var out []membership
	for _, part := range strings.Split(raw, ",") {
		if part == "" {
			continue
		}
		var m membership
		var decide int
		if _, err := fmt.Sscanf(part, "%d:%d:%d", &m.channel, &m.historyAfter, &decide); err != nil {
			t.Fatalf("membership %q: %v", part, err)
		}
		m.canDecide = decide != 0
		out = append(out, m)
	}
	return out
}

func (f *loginFixture) passwordCount(t *testing.T, id int64) string {
	t.Helper()
	return f.queryString(t, fmt.Sprintf("SELECT COUNT(*) FROM passwords WHERE principal_id = %d", id))
}

// An account from the bare address: made with its password, in every open
// channel with no right to decide and a history bound at its admission, and
// signed in again from another device with the password alone once it has
// signed out.
func TestJoinAccount(t *testing.T) {
	ctx := context.Background()
	f := newLoginFixture(t, Config{})
	if _, err := f.srv.store.InsertMessage(ctx, f.red.ID, f.alice.ID, "said before the account joined"); err != nil {
		t.Fatal(err)
	}
	f.mustPutJoin(t, openBoth("red"))
	if info := f.frontDoor(t); !info.Join.Account || !info.PasswordLogin {
		t.Fatalf("front door = %+v, want an account and password sign-in offered", info)
	}
	acct := mustAccount(t, f.joinAccount(t, "newcomer", ""))
	if acct.Principal.Name != "newcomer" || acct.Principal.Kind != schema.PrincipalHuman ||
		len(acct.Channels) != 1 || acct.Channels[0].ID != f.red.ID || !acct.Credential.Renews {
		t.Fatalf("account = %+v", acct)
	}
	id := acct.Principal.ID
	if code, who := f.whoami(t, acct.Token); code != 200 || who.ID != id || who.IdentityClass != schema.IdentityMember || who.Guest != nil || who.MayInvite {
		t.Fatalf("whoami = %d %+v", code, who)
	}
	ms := f.memberships(t, id)
	if len(ms) != 1 || ms[0].channel != f.red.ID || ms[0].canDecide || ms[0].historyAfter <= 0 {
		t.Errorf("memberships = %+v, want red with no right to decide and a history bound", ms)
	}
	if ok, err := f.srv.store.CanDecideApprovals(ctx, f.red.ID, id); err != nil || ok {
		t.Errorf("CanDecideApprovals = %v, %v", ok, err)
	}
	msgs := decodeBody[schema.ListMessagesResponseV2](t, f.send(t, "GET", "/v2/channels/red/messages", acct.Token, "", ""))
	if len(msgs.Messages) != 0 {
		t.Errorf("the new account reads %d messages from before it joined", len(msgs.Messages))
	}
	if f.passwordCount(t, id) != "1" {
		t.Error("the account has no password")
	}
	for _, c := range []struct{ action, detail string }{
		{store.AuditPrincipalCreated, "kind=human origin=address"},
		{store.AuditPasswordSet, "via=join"},
		{store.AuditMemberAdded, fmt.Sprintf("principal=%d", id)},
	} {
		var found bool
		for _, e := range f.auditOf(t, c.action) {
			found = found || strings.Contains(e.Detail, c.detail)
			if strings.Contains(e.Detail, testJoinPassword) || strings.Contains(e.Detail, acct.Token) {
				t.Errorf("%s audits a secret: %s", c.action, e.Detail)
			}
		}
		if !found {
			t.Errorf("no %s row with %q", c.action, c.detail)
		}
	}

	// Sign out, then sign in on a new device with the password alone.
	if rec := f.send(t, "DELETE", "/v1/credentials/current", acct.Token, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
	}
	if code, _ := f.whoami(t, acct.Token); code != http.StatusUnauthorized {
		t.Errorf("the signed-out token = %d", code)
	}
	login := f.passwordLogin(t, "newcomer")
	if code, who := f.whoami(t, login.Token); code != 200 || who.ID != id {
		t.Errorf("whoami on the new device = %d %+v", code, who)
	}
}

// Refused accounts from the bare address: each answer is the documented
// one, and none makes a principal or stores a password.
func TestJoinAccountRefusals(t *testing.T) {
	f := newLoginFixture(t, Config{ServerName: "Harbour Club"})
	f.mustPutJoin(t, openBoth("red"))
	tests := []struct {
		name, password string
		status         int
		code           string
	}{
		{"newcomer", "short", http.StatusBadRequest, "invalid_request"},
		{"newcomer", "newcomer and the rest of it", http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable},
		{"newcomer", "a harbour club password", http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable},
		{"guest-newcomer", testJoinPassword, http.StatusBadRequest, "invalid_request"},
		// Cyrillic е: the folded key is not the prefix's, the skeleton is.
		{"guеst-newcomer", testJoinPassword, http.StatusConflict, schema.ErrorCodePrincipalExists},
		{"alice", testJoinPassword, http.StatusConflict, schema.ErrorCodePrincipalExists},
		{"ALICE", testJoinPassword, http.StatusConflict, schema.ErrorCodePrincipalExists},
		{"", testJoinPassword, http.StatusBadRequest, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.name), func(t *testing.T) {
			rec := f.send(t, "POST", "/v1/join/account", "", inviteJSON(t, schema.JoinAccountRequestV1{Name: tt.name, Password: tt.password}), "")
			assertErrorBody(t, rec, tt.status, tt.code)
		})
	}
	if n := f.queryString(t, "SELECT COUNT(*) FROM principals WHERE name NOT IN ('root', 'alice', 'bot', 'ghost')"); n != "0" {
		t.Errorf("refusals made %s principals", n)
	}
	if n := f.queryString(t, "SELECT COUNT(*) FROM passwords"); n != "2" {
		t.Errorf("passwords = %s, want only alice's and root's", n)
	}
}

// The name-probe allowance (design §8) is spent by a name found taken, and
// once it is gone an address makes no account until it refills; another
// address may. Over plain HTTP beyond loopback the route is refused before
// it spends anything.
func TestJoinAccountLimits(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.mustPutJoin(t, openBoth("red"))
	l := looseLoginLimits()
	l.nameProbesPerAddress = inviteLimit{"name_probes_per_address", 2, time.Hour}
	f.srv.setLoginLimits(l)
	il := looseInviteLimits()
	il.redeemPerAddress = inviteLimit{"redeem_per_address", 4, time.Hour}
	f.srv.setInviteLimits(il)
	const remote = "198.51.100.80:1"
	plain := httptest.NewRequest("POST", "/v1/join/account", strings.NewReader(inviteJSON(t, schema.JoinAccountRequestV1{Name: "x-plain", Password: testJoinPassword})))
	plain.RemoteAddr = remote
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, plain)
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeHTTPSRequired)
	for range 2 {
		assertErrorBody(t, f.joinAccount(t, "alice", remote), http.StatusConflict, schema.ErrorCodePrincipalExists)
	}
	assertErrorBody(t, f.joinAccount(t, "newcomer", remote), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	mustAccount(t, f.joinAccount(t, "newcomer", "198.51.100.81:1"))
	// The plain request spent none of the four redemptions: the three
	// above and this one fit.
	f.srv.setLoginLimits(looseLoginLimits())
	mustAccount(t, f.joinAccount(t, "another", remote))
}

// What lands between the hash and the writing transaction decides: the store
// reads the policy and the name again and makes nothing it would refuse.
func TestJoinAccountRechecksAfterHashing(t *testing.T) {
	tests := []struct {
		name   string
		gap    func(t *testing.T, f *loginFixture)
		status int
		code   string
	}{
		{"joining closed", func(t *testing.T, f *loginFixture) {
			f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningGuest)})
		}, http.StatusForbidden, schema.ErrorCodeJoiningClosed},
		{"joining paused", func(t *testing.T, f *loginFixture) {
			f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{JoiningPaused: boolPtr(true)})
		}, http.StatusForbidden, schema.ErrorCodeJoiningPaused},
		{"name taken", func(t *testing.T, f *loginFixture) {
			if _, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalHuman, "Newcomer"); err != nil {
				t.Fatal(err)
			}
		}, http.StatusConflict, schema.ErrorCodePrincipalExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLoginFixture(t, Config{})
			f.mustPutJoin(t, openBoth("red"))
			f.srv.beforePasswordWrite = func() { tt.gap(t, f) }
			assertErrorBody(t, f.joinAccount(t, "newcomer", ""), tt.status, tt.code)
			f.srv.beforePasswordWrite = nil
			if n := f.queryString(t, "SELECT COUNT(*) FROM principals WHERE name = 'newcomer'"); n != "0" {
				t.Errorf("an account was made: %s", n)
			}
			if n := f.queryString(t, "SELECT COUNT(*) FROM passwords"); n != "2" {
				t.Errorf("passwords = %s, want only alice's and root's", n)
			}
		})
	}
}

// Create an account during a visit: the same principal, its origin, its
// memberships with their history bounds and no right to decide, its earlier
// posts still a guest's; the guest token and its sockets end, a fresh member
// token works, the address book forgets the visit, and a lost answer is
// recovered by signing in with the password.
func TestConvertGuestOverREST(t *testing.T) {
	ctx := context.Background()
	f := newLoginFixture(t, Config{})
	if _, err := f.srv.store.InsertMessage(ctx, f.red.ID, f.alice.ID, "said before the visit"); err != nil {
		t.Fatal(err)
	}
	f.mustPutJoin(t, openBoth("red"))
	const remote = "198.51.100.90:1"
	g := mustJoined(t, f.joinGuest(t, "Pat", remote))
	id := g.Principal.ID
	if code, who := f.whoami(t, g.Token); code != 200 || who.Guest == nil || !who.Guest.CanConvert {
		t.Fatalf("whoami = %d %+v, want a guest that may convert", code, who)
	}
	rec := f.send(t, "POST", "/v2/channels/red/messages", g.Token, `{"body":"said as a guest"}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("guest post = %d %s", rec.Code, rec.Body)
	}
	guestPost := decodeBody[schema.PostMessageResponseV2](t, rec).Message.ID
	before := f.memberships(t, id)
	if len(before) != 1 || before[0].canDecide || before[0].historyAfter <= 0 {
		t.Fatalf("the guest's memberships = %+v", before)
	}
	if !slices.Contains(f.srv.guestAddrs.peers(limitKey(clientAddrOf(t, remote))), id) {
		t.Fatal("the address book does not hold the visit")
	}
	wsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	base := wsTestServer(t, f.srv)
	sock := recordFrames(dialAs(t, wsCtx, base, "/v2/ws?channel=red", g.Token))

	start := time.Now()
	c := mustConverted(t, f.convert(t, g.Token, "patricia", ""))
	if c.Principal.ID != id || c.Principal.Name != "patricia" || !c.Credential.Renews {
		t.Fatalf("converted = %+v", c)
	}
	select {
	case <-sock.done:
	case <-time.After(guestSocketBound + time.Second):
		t.Fatalf("the guest token's socket stayed open %v after the conversion", time.Since(start))
	}
	if code, _ := f.whoami(t, g.Token); code != http.StatusUnauthorized {
		t.Errorf("the guest token after conversion = %d", code)
	}
	code, who := f.whoami(t, c.Token)
	if code != 200 || who.ID != id || who.IdentityClass != schema.IdentityMember || who.Guest != nil || who.Name != "patricia" || who.MayInvite {
		t.Fatalf("whoami as the member = %d %+v", code, who)
	}
	if after := f.memberships(t, id); !slices.Equal(after, before) {
		t.Errorf("memberships after %+v, before %+v", after, before)
	}
	if slices.Contains(f.srv.guestAddrs.peers(limitKey(clientAddrOf(t, remote))), id) {
		t.Error("the address book still holds the converted visit")
	}
	msgs := decodeBody[schema.ListMessagesResponseV2](t, f.send(t, "GET", "/v2/channels/red/messages", c.Token, "", ""))
	if len(msgs.Messages) != 1 || msgs.Messages[0].ID != guestPost || msgs.Messages[0].GuestAuthor == nil ||
		msgs.Messages[0].GuestAuthor.Handle != g.Guest.Handle || msgs.Messages[0].GuestAuthor.Label != "Pat" {
		t.Fatalf("the member reads %+v, want only its guest post, attributed to the guest", msgs.Messages)
	}
	rec = f.send(t, "POST", "/v2/channels/red/messages", c.Token, `{"body":"said as a member"}`, "")
	if rec.Code != http.StatusCreated || decodeBody[schema.PostMessageResponseV2](t, rec).Message.GuestAuthor != nil {
		t.Errorf("member post = %d %s, want no guest attribution", rec.Code, rec.Body)
	}
	v, err := f.srv.store.GuestVisitOf(ctx, id)
	if err != nil || v.State != schema.GuestStateConverted || v.Origin != schema.GuestOriginAddress || v.Handle != g.Guest.Handle {
		t.Errorf("visit = %+v, %v", v, err)
	}
	rows := f.auditOf(t, store.AuditGuestConverted)
	if len(rows) != 1 || rows[0].Detail != fmt.Sprintf("from=%q to=%q", g.Guest.Handle, "patricia") {
		t.Errorf("guest_converted rows = %+v", rows)
	}
	for _, e := range f.audit(t) {
		if strings.Contains(e.Detail, testJoinPassword) || strings.Contains(e.Detail, c.Token) || strings.Contains(e.Detail, g.Token) {
			t.Errorf("%s audits a secret: %s", e.Action, e.Detail)
		}
	}
	// The answer was lost: the password signs the member in again, and the
	// old guest token never converts a second time.
	login := f.passwordLogin(t, "patricia")
	if code, who := f.whoami(t, login.Token); code != 200 || who.ID != id {
		t.Errorf("after the lost answer = %d %+v", code, who)
	}
	assertErrorBody(t, f.convert(t, g.Token, "patricia2", ""), http.StatusUnauthorized, "unauthenticated")
	assertErrorBody(t, f.convert(t, c.Token, "patricia2", ""), http.StatusForbidden, schema.ErrorCodeNotAGuest)
}

// clientAddrOf is the client address a request from remote has.
func clientAddrOf(t *testing.T, remote string) netip.Addr {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = remote
	return clientAddr(req, nil)
}

// Conversion under the policy of the visit's origin (design §3.3): what
// whoami's can_convert says is what the conversion does; a refusal leaves
// the guest as it was, still a guest with a working token and no password.
func TestConvertGuestPolicyMatrix(t *testing.T) {
	type world struct {
		*loginFixture
		token string
		id    int64
		inv   int64
	}
	viaAddress := func(t *testing.T, f *loginFixture) world {
		f.mustPutJoin(t, openBoth("red"))
		g := mustJoined(t, f.joinGuest(t, "", ""))
		return world{loginFixture: f, token: g.Token, id: g.Principal.ID}
	}
	viaInvite := func(joinAs schema.InviteJoinMode, kind schema.InviteKind) func(t *testing.T, f *loginFixture) world {
		return func(t *testing.T, f *loginFixture) world {
			f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(true)})
			req := schema.CreateInviteRequestV1{Kind: kind, Channels: []string{"red"}, JoinAs: joinAs}
			inv := f.createInvite(t, f.rootTok, req)
			g := mustJoined(t, f.redeemGuest(t, inv.Code, "", ""))
			return world{loginFixture: f, token: g.Token, id: g.Principal.ID, inv: inv.Invite.ID}
		}
	}
	tests := []struct {
		name   string
		start  func(t *testing.T, f *loginFixture) world
		then   func(t *testing.T, w world)
		status int
		code   string
	}{
		{"address both", viaAddress, nil, http.StatusOK, ""},
		{"address accounts only after", viaAddress, func(t *testing.T, w world) {
			w.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningAccount)})
		}, http.StatusOK, ""},
		{"address with guests turned off", viaAddress, func(t *testing.T, w world) {
			w.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(false)})
		}, http.StatusOK, ""},
		{"address guests only", viaAddress, func(t *testing.T, w world) {
			w.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningGuest)})
		}, http.StatusForbidden, schema.ErrorCodeConversionNotAllowed},
		{"address off", viaAddress, func(t *testing.T, w world) {
			w.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningOff)})
		}, http.StatusForbidden, schema.ErrorCodeConversionNotAllowed},
		{"address paused", viaAddress, func(t *testing.T, w world) {
			w.mustPutJoin(t, schema.PutJoinSettingsRequestV1{JoiningPaused: boolPtr(true)})
		}, http.StatusForbidden, schema.ErrorCodeJoiningPaused},
		{"invite both", viaInvite(schema.InviteJoinBoth, schema.InviteKindOpen), nil, http.StatusOK, ""},
		{"personal invite both, its one use spent", viaInvite(schema.InviteJoinBoth, schema.InviteKindPersonal), nil, http.StatusOK, ""},
		{"invite guests only", viaInvite(schema.InviteJoinGuest, schema.InviteKindOpen), nil, http.StatusForbidden, schema.ErrorCodeConversionNotAllowed},
		{"invite closed", viaInvite(schema.InviteJoinBoth, schema.InviteKindOpen), func(t *testing.T, w world) {
			if rec := w.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", w.inv), w.rootTok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("close = %d %s", rec.Code, rec.Body)
			}
		}, http.StatusForbidden, schema.ErrorCodeConversionNotAllowed},
		{"invite expired", viaInvite(schema.InviteJoinBoth, schema.InviteKindOpen), func(t *testing.T, w world) {
			w.advance(schema.MaxOpenInviteLifetime + time.Second)
		}, http.StatusForbidden, schema.ErrorCodeConversionNotAllowed},
		{"invites paused", viaInvite(schema.InviteJoinBoth, schema.InviteKindOpen), func(t *testing.T, w world) {
			if rec := w.send(t, "PUT", "/v1/invites/settings", w.rootTok, `{"paused":true}`, ""); rec.Code != http.StatusOK {
				t.Fatalf("pause = %d %s", rec.Code, rec.Body)
			}
		}, http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.start(t, newLoginFixture(t, Config{}))
			if tt.then != nil {
				tt.then(t, w)
			}
			_, who := w.whoami(t, w.token)
			if who.Guest == nil || who.Guest.CanConvert != (tt.status == http.StatusOK) {
				t.Errorf("whoami's guest = %+v, want can_convert %t", who.Guest, tt.status == http.StatusOK)
			}
			rec := w.convert(t, w.token, "member", "")
			if tt.status == http.StatusOK {
				c := mustConverted(t, rec)
				p, err := w.srv.store.PrincipalByID(context.Background(), w.id)
				if err != nil || c.Principal.ID != w.id || p.InviteID != w.inv || p.GuestState != schema.GuestStateConverted {
					t.Errorf("converted principal = %+v, %v; want the same id, invite %d", p, err, w.inv)
				}
				return
			}
			assertErrorBody(t, rec, tt.status, tt.code)
			if code, who := w.whoami(t, w.token); code != 200 || who.Guest == nil || who.Guest.State != schema.GuestStateActive {
				t.Errorf("after the refusal whoami = %d %+v, want the guest unchanged", code, who)
			}
			if w.passwordCount(t, w.id) != "0" {
				t.Error("a refused conversion stored a password")
			}
		})
	}
}

// Refusals of the caller, the body and the name leave the guest unchanged.
func TestConvertGuestRefusals(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.mustPutJoin(t, openBoth("red"))
	g := mustJoined(t, f.joinGuest(t, "Pat", ""))
	for _, tok := range []string{f.aliceTok, f.rootTok, f.botTok} {
		assertErrorBody(t, f.convert(t, tok, "member", ""), http.StatusForbidden, schema.ErrorCodeNotAGuest)
	}
	tests := []struct {
		name, password, permit string
		status                 int
		code                   string
	}{
		{"member", "short", "", http.StatusBadRequest, "invalid_request"},
		{"member", "member and the rest of it", "", http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable},
		{"guest-member", testJoinPassword, "", http.StatusBadRequest, "invalid_request"},
		{"guеst-member", testJoinPassword, "", http.StatusConflict, schema.ErrorCodePrincipalExists},
		{"alice", testJoinPassword, "", http.StatusConflict, schema.ErrorCodePrincipalExists},
		{g.Guest.Handle, testJoinPassword, "", http.StatusBadRequest, "invalid_request"},
		{"member", testJoinPassword, "not-a-permit", http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid},
		{"member", testJoinPassword, schema.SignInLinkCodePrefix + strings.Repeat("A", 43), http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.name), func(t *testing.T) {
			rec := f.send(t, "POST", "/v1/guest/convert", g.Token,
				inviteJSON(t, schema.ConvertGuestRequestV1{Name: tt.name, Password: tt.password, Permit: tt.permit}), "")
			assertErrorBody(t, rec, tt.status, tt.code)
		})
	}
	if code, who := f.whoami(t, g.Token); code != 200 || who.Guest == nil || who.Name != g.Guest.Handle {
		t.Errorf("after the refusals whoami = %d %+v", code, who)
	}
	if f.passwordCount(t, g.Principal.ID) != "0" {
		t.Error("a refused conversion stored a password")
	}
}

// An operator's conversion permit lets one live guest create an account
// under a closed policy, once; it is no other link, no other guest's, and
// for no ended or converted visit.
func TestConversionPermit(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.mustPutJoin(t, openTo("red"))
	g := mustJoined(t, f.joinGuest(t, "", ""))
	h := mustJoined(t, f.joinGuest(t, "", ""))
	assertErrorBody(t, f.convert(t, g.Token, "member", ""), http.StatusForbidden, schema.ErrorCodeConversionNotAllowed)

	permitPath := func(id int64) string { return fmt.Sprintf("/v1/guests/%d/conversion-permit", id) }
	issue := func(id int64) schema.CreateSignInLinkResponseV1 {
		t.Helper()
		rec := f.send(t, "POST", permitPath(id), f.rootTok, `{}`, "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("permit = %d %s", rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q", got)
		}
		return decodeBody[schema.CreateSignInLinkResponseV1](t, rec)
	}
	p := issue(g.Principal.ID)
	spare := issue(g.Principal.ID)
	if p.SignInLink.Purpose != schema.SignInLinkPurposeConversion || p.SignInLink.PrincipalID != g.Principal.ID ||
		p.SignInLink.ExpiresAt.Time().Sub(p.SignInLink.CreatedAt.Time()) != schema.DefaultRestrictedLinkLifetime {
		t.Errorf("permit = %+v", p.SignInLink)
	}
	if rows := f.auditOf(t, store.AuditConversionPermitIssued); len(rows) != 2 || rows[0].Subject != fmt.Sprintf("principal:%d", g.Principal.ID) ||
		strings.Contains(rows[0].Detail, p.Code) {
		t.Errorf("conversion_permit_issued rows = %+v", rows)
	}
	// It is no sign-in link and no other guest's permit.
	for _, path := range []string{"/v1/sign-in-links/preview", "/v1/sign-in-links/redeem"} {
		assertErrorBody(t, f.send(t, "POST", path, "", inviteJSON(t, schema.SignInLinkPreviewRequestV1{Code: p.Code}), ""),
			http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	}
	assertErrorBody(t, f.convert(t, h.Token, "member", p.Code), http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid)
	// A sign-in link of a member's is no permit.
	link := decodeBody[schema.CreateSignInLinkResponseV1](t, f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", f.alice.ID), f.rootTok, `{}`, ""))
	assertErrorBody(t, f.convert(t, g.Token, "member", link.Code), http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid)

	c := mustConverted(t, f.convert(t, g.Token, "member", p.Code))
	if c.Principal.ID != g.Principal.ID {
		t.Fatalf("converted = %+v", c)
	}
	if rows := f.auditOf(t, store.AuditGuestConverted); len(rows) != 1 || !strings.HasSuffix(rows[0].Detail, fmt.Sprintf(" permit=%d", p.SignInLink.ID)) {
		t.Errorf("guest_converted rows = %+v", rows)
	}
	var closed bool
	for _, e := range f.auditOf(t, store.AuditSignInLinkClosed) {
		closed = closed || e.Detail == fmt.Sprintf("sign_in_link=%d reason=%s", spare.SignInLink.ID, store.SignInLinkCloseConverted)
	}
	if !closed {
		t.Error("the guest's spare permit was not closed by its conversion")
	}
	// Spent: not even its own guest's, let alone another's.
	assertErrorBody(t, f.convert(t, h.Token, "other", p.Code), http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid)
	assertErrorBody(t, f.convert(t, h.Token, "other", spare.Code), http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid)

	// No permit for a member, a converted visit or an ended one; and none
	// from a member.
	assertErrorBody(t, f.send(t, "POST", permitPath(f.alice.ID), f.rootTok, `{}`, ""), http.StatusNotFound, schema.ErrorCodeNotAGuest)
	assertErrorBody(t, f.send(t, "POST", permitPath(g.Principal.ID), f.rootTok, `{}`, ""), http.StatusConflict, schema.ErrorCodeGuestEnded)
	assertErrorBody(t, f.send(t, "POST", permitPath(h.Principal.ID), f.aliceTok, `{}`, ""), http.StatusForbidden, "forbidden")
	late := issue(h.Principal.ID)
	if rec := f.send(t, "POST", "/v1/guest/end", h.Token, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("end visit = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.send(t, "POST", permitPath(h.Principal.ID), f.rootTok, `{}`, ""), http.StatusConflict, schema.ErrorCodeGuestEnded)
	if err := f.srv.store.CheckConversion(context.Background(), h.Principal.ID, store.HashSignInLinkCode(late.Code), f.srv.now()); err == nil {
		t.Error("a permit revived an ended visit")
	}

	// A permit runs out.
	k := mustJoined(t, f.joinGuest(t, "", ""))
	short := issue(k.Principal.ID)
	f.advance(schema.DefaultRestrictedLinkLifetime + time.Second)
	assertErrorBody(t, f.convert(t, k.Token, "member2", short.Code), http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid)
	if f.passwordCount(t, k.Principal.ID) != "0" || f.passwordCount(t, h.Principal.ID) != "0" {
		t.Error("a refused conversion stored a password")
	}
}

// What lands between the hash and the converting transaction decides, and
// of a conversion and a sign-out, an End visit, an expiry, a disable or a
// second conversion exactly one wins: a refused conversion writes nothing,
// and nothing brings an ended visit back.
func TestConvertGuestRechecksAfterHashing(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		gap    func(t *testing.T, f *loginFixture, g schema.JoinGuestResponseV1)
		status int
		code   string
		ended  bool
	}{
		{"End visit", func(t *testing.T, f *loginFixture, g schema.JoinGuestResponseV1) {
			if _, err := f.srv.store.EndGuestVisit(ctx, "system", g.Principal.ID, schema.GuestEndVisit); err != nil {
				t.Fatal(err)
			}
		}, http.StatusConflict, schema.ErrorCodeGuestEnded, true},
		{"sign-out", func(t *testing.T, f *loginFixture, g schema.JoinGuestResponseV1) {
			if err := f.srv.store.RevokeCredential(ctx, "system", g.Credential.ID); err != nil {
				t.Fatal(err)
			}
		}, http.StatusConflict, schema.ErrorCodeGuestEnded, true},
		{"expiry", func(t *testing.T, f *loginFixture, g schema.JoinGuestResponseV1) {
			past := time.Now().Add(-time.Minute).UnixMilli()
			f.exec(t, "UPDATE credentials SET expires_at = ? WHERE id = ?", past, g.Credential.ID)
		}, http.StatusConflict, schema.ErrorCodeGuestEnded, false},
		{"disable", func(t *testing.T, f *loginFixture, g schema.JoinGuestResponseV1) {
			if _, err := f.srv.store.DisablePrincipal(ctx, "system", g.Principal.ID); err != nil {
				t.Fatal(err)
			}
		}, http.StatusConflict, schema.ErrorCodeGuestEnded, true},
		{"policy closed", func(t *testing.T, f *loginFixture, g schema.JoinGuestResponseV1) {
			f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningGuest)})
		}, http.StatusForbidden, schema.ErrorCodeConversionNotAllowed, false},
		{"name taken", func(t *testing.T, f *loginFixture, g schema.JoinGuestResponseV1) {
			if _, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "Member"); err != nil {
				t.Fatal(err)
			}
		}, http.StatusConflict, schema.ErrorCodePrincipalExists, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLoginFixture(t, Config{})
			f.mustPutJoin(t, openBoth("red"))
			g := mustJoined(t, f.joinGuest(t, "", ""))
			f.srv.beforePasswordWrite = func() { tt.gap(t, f, g) }
			assertErrorBody(t, f.convert(t, g.Token, "member", ""), tt.status, tt.code)
			f.srv.beforePasswordWrite = nil
			v, err := f.srv.store.GuestVisitOf(ctx, g.Principal.ID)
			if err != nil || v.State == schema.GuestStateConverted || (v.State == schema.GuestStateEnded) != tt.ended {
				t.Errorf("visit = %+v, %v; want ended %t and never converted", v, err, tt.ended)
			}
			p, err := f.srv.store.PrincipalByID(ctx, g.Principal.ID)
			if err != nil || p.Name != g.Guest.Handle {
				t.Errorf("principal = %+v, %v; want its handle", p, err)
			}
			if f.passwordCount(t, g.Principal.ID) != "0" {
				t.Error("a refused conversion stored a password")
			}
			if tt.ended {
				// Nothing brings it back: not a second try, not a permit.
				if rec := f.convert(t, g.Token, "member2", ""); rec.Code != http.StatusUnauthorized {
					t.Errorf("a second try = %d %s", rec.Code, rec.Body)
				}
				assertErrorBody(t, f.send(t, "POST", fmt.Sprintf("/v1/guests/%d/conversion-permit", g.Principal.ID), f.rootTok, `{}`, ""),
					http.StatusConflict, schema.ErrorCodeGuestEnded)
			}
		})
	}
}

// Two conversions of one visit at once: the second is refused at once with
// 429 and Retry-After, before it hashes anything, while the first is in
// flight; the first converts; and one name, one password and one member
// credential exist.
func TestConvertGuestDuplicateRequests(t *testing.T) {
	ctx := context.Background()
	f := newLoginFixture(t, Config{})
	f.mustPutJoin(t, openBoth("red"))
	g := mustJoined(t, f.joinGuest(t, "", ""))
	inWrite, release := make(chan struct{}), make(chan struct{})
	f.srv.beforePasswordWrite = func() {
		close(inWrite)
		<-release
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- f.convert(t, g.Token, "member0", "") }()
	<-inWrite
	hashed := f.srv.passwords.hashes.Load()
	rec := f.convert(t, g.Token, "member1", "")
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("the duplicate's 429 has no Retry-After")
	}
	if n := f.srv.passwords.hashes.Load(); n != hashed {
		t.Errorf("the duplicate hashed %d passwords", n-hashed)
	}
	close(release)
	mustConverted(t, <-first)
	f.srv.beforePasswordWrite = nil
	// The visit is converted; the old token is no one's now.
	assertErrorBody(t, f.convert(t, g.Token, "member1", ""), http.StatusUnauthorized, "unauthenticated")
	if n := f.queryString(t, "SELECT COUNT(*) FROM principals WHERE name IN ('member0', 'member1')"); n != "1" {
		t.Errorf("%s of the two names exist", n)
	}
	if f.passwordCount(t, g.Principal.ID) != "1" {
		t.Errorf("passwords = %s", f.passwordCount(t, g.Principal.ID))
	}
	if n := f.queryString(t, fmt.Sprintf("SELECT COUNT(*) FROM credentials WHERE principal_id = %d AND revoked_at IS NULL", g.Principal.ID)); n != "1" {
		t.Errorf("live credentials = %s", n)
	}
	if v, err := f.srv.store.GuestVisitOf(ctx, g.Principal.ID); err != nil || v.State != schema.GuestStateConverted {
		t.Errorf("visit = %+v, %v", v, err)
	}
	// The guard is per guest: another guest converts while nothing of the
	// first is in flight, and the guard is gone once a conversion ends.
	h := mustJoined(t, f.joinGuest(t, "", ""))
	assertErrorBody(t, f.convert(t, h.Token, "member0", ""), http.StatusConflict, schema.ErrorCodePrincipalExists)
	mustConverted(t, f.convert(t, h.Token, "member2", ""))
}

// A guest's hashed conversion attempts are counted, five a window by
// default: once they are spent the next is refused with 429 before any
// hash, and another guest is not affected. Refusals that need no hash
// spend nothing.
func TestConvertGuestAttemptLimit(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.mustPutJoin(t, openBoth("red"))
	l := looseLoginLimits()
	l.conversionsPerPrincipal = inviteLimit{"conversions_per_principal", 2, time.Hour}
	f.srv.setLoginLimits(l)
	g := mustJoined(t, f.joinGuest(t, "", ""))
	// A refusal before the hash spends no attempt.
	for range 3 {
		assertErrorBody(t, f.convert(t, g.Token, "alice", ""), http.StatusConflict, schema.ErrorCodePrincipalExists)
	}
	// Two attempts that hash and then fail: the policy closes in the gap.
	f.srv.beforePasswordWrite = func() {
		f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningGuest)})
	}
	for range 2 {
		assertErrorBody(t, f.convert(t, g.Token, "member", ""), http.StatusForbidden, schema.ErrorCodeConversionNotAllowed)
		f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningBoth)})
	}
	f.srv.beforePasswordWrite = nil
	hashed := f.srv.passwords.hashes.Load()
	rec := f.convert(t, g.Token, "member", "")
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("the 429 has no Retry-After")
	}
	if n := f.srv.passwords.hashes.Load(); n != hashed {
		t.Errorf("the refused attempt hashed %d passwords", n-hashed)
	}
	if code, who := f.whoami(t, g.Token); code != 200 || who.Guest == nil {
		t.Errorf("the refused guest = %d %+v, want unchanged", code, who)
	}
	h := mustJoined(t, f.joinGuest(t, "", ""))
	mustConverted(t, f.convert(t, h.Token, "member", ""))
}

// A hash that has run is not thrown away: a caller who goes away once its
// password is hashed still gets its account, from the bare address and from
// a visit.
func TestAccountWritesOutliveTheCaller(t *testing.T) {
	ctx := context.Background()
	f := newLoginFixture(t, Config{})
	f.mustPutJoin(t, openBoth("red"))
	gone := func(path, token string, body any) {
		t.Helper()
		reqCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		f.srv.beforePasswordWrite = cancel
		defer func() { f.srv.beforePasswordWrite = nil }()
		req := httptest.NewRequest("POST", path, strings.NewReader(inviteJSON(t, body))).WithContext(reqCtx)
		req.TLS = &tls.ConnectionState{}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		f.srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	gone("/v1/join/account", "", schema.JoinAccountRequestV1{Name: "newcomer", Password: testJoinPassword})
	if c, found, err := f.srv.store.LoginCandidate(ctx, "newcomer"); err != nil || !found || c.Record == "" {
		t.Errorf("the account of a caller who left = %+v, %v, %v; want made, with its password", c, found, err)
	}
	g := mustJoined(t, f.joinGuest(t, "", ""))
	gone("/v1/guest/convert", g.Token, schema.ConvertGuestRequestV1{Name: "patricia", Password: testJoinPassword})
	if v, err := f.srv.store.GuestVisitOf(ctx, g.Principal.ID); err != nil || v.State != schema.GuestStateConverted {
		t.Errorf("the visit of a caller who left = %+v, %v; want converted", v, err)
	}
	// The guard is released, and the password signs the member in.
	f.passwordLogin(t, "patricia")
}

// A guest's name probes are its own: an anonymous caller who spends its
// address's on POST /v1/join/account holds no guest's conversion from that
// address up, and a guest who spends its own holds up no other guest.
func TestConversionNameProbesArePerGuest(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.mustPutJoin(t, openBoth("red"))
	l := looseLoginLimits()
	l.nameProbesPerAddress = inviteLimit{"name_probes_per_address", 1, time.Hour}
	l.nameProbesPerPrincipal = inviteLimit{"name_probes_per_principal", 1, time.Hour}
	f.srv.setLoginLimits(l)
	const remote = "198.51.100.95:1"
	g := mustJoined(t, f.joinGuest(t, "", remote))
	h := mustJoined(t, f.joinGuest(t, "", remote))
	assertErrorBody(t, f.joinAccount(t, "alice", remote), http.StatusConflict, schema.ErrorCodePrincipalExists)
	assertErrorBody(t, f.joinAccount(t, "a-free-name", remote), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	convertFrom := func(token, name string) *httptest.ResponseRecorder {
		return f.send(t, "POST", "/v1/guest/convert", token, inviteJSON(t, schema.ConvertGuestRequestV1{Name: name, Password: testJoinPassword}), remote)
	}
	assertErrorBody(t, convertFrom(g.Token, "alice"), http.StatusConflict, schema.ErrorCodePrincipalExists)
	assertErrorBody(t, convertFrom(g.Token, "member"), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	mustConverted(t, convertFrom(h.Token, "member"))
}
