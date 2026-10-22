package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Temporary guests on the server (issue #705): End visit, the deadline
// sweeper, what follows a visit's end on every socket and in voice, and the
// refusals of the existing routes. These run against the real clock: the
// deadlines are short, the waits bounded by what the design states.

// guestSocketBound is how long after a visit's end every socket of it must
// have closed (design §4.2: two seconds from an explicit terminal commit).
const guestSocketBound = 2 * time.Second

// newGuest starts a visit in channels through the store, as the entry
// routes (#710) will.
func newGuest(t *testing.T, srv *Server, label string, idle, absolute time.Duration, channels ...int64) store.CreatedGuest {
	t.Helper()
	g, err := srv.store.CreateGuest(context.Background(), "system", store.NewGuest{Label: label, Origin: schema.GuestOriginAddress,
		Channels: channels, Idle: idle, Absolute: absolute, VoiceAllowed: true})
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	return g
}

// No route makes a passwordless member (design §7.3 "Ordering"), and a
// server nobody has configured makes nobody through the entry routes. The
// two account routes exist now (#711): each takes a password with the name,
// refuses a body without one before anything else is looked at, and, on an
// unconfigured server, refuses even a good one: the bare address offers no
// account, and nobody is a guest to convert. The guest entry routes (#710)
// admit no guest.
func TestUnconfiguredServerAdmitsNobody(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	good := inviteJSON(t, schema.JoinAccountRequestV1{Name: "newcomer", Password: testJoinPassword})
	for _, c := range []struct {
		method, path, token string
		status              int
		code                string
	}{
		// A name with no password is a 400 on both routes: neither can make
		// an account without one.
		{"POST", "/v1/join/account", "", http.StatusBadRequest, "invalid_request"},
		{"POST", "/v1/guest/convert", f.rootTok, http.StatusForbidden, schema.ErrorCodeNotAGuest},
	} {
		assertErrorBody(t, secureDo(t, f, c.method, c.path, c.token, `{"name":"newcomer"}`), c.status, c.code)
	}
	assertErrorBody(t, secureDo(t, f, "POST", "/v1/join/account", "", good), http.StatusForbidden, schema.ErrorCodeJoiningClosed)
	for _, tok := range []string{f.rootTok, f.aliceTok, f.botTok} {
		assertErrorBody(t, secureDo(t, f, "POST", "/v1/guest/convert", tok, good), http.StatusForbidden, schema.ErrorCodeNotAGuest)
	}
	assertErrorBody(t, secureDo(t, f, "POST", "/v1/guest/convert", "", good), http.StatusUnauthorized, "unauthenticated")
	if _, found, err := f.srv.store.LoginCandidate(context.Background(), "newcomer"); err != nil || found {
		t.Errorf("an account was made: found %v, %v", found, err)
	}
	// Password sign-in exists whenever authentication is on (#707); the
	// address offers nothing else.
	info := decodeBody[schema.JoinInfoV1](t, f.do(t, "GET", "/v1/join", "", ""))
	if info.Join != (schema.JoinOptionsV1{}) || !info.PasswordLogin {
		t.Errorf("the front door of an unconfigured server offers %+v", info)
	}
	assertErrorBody(t, secureDo(t, f, "POST", "/v1/join/guest", "", `{}`), http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
	assertErrorBody(t, f.do(t, "POST", "/v1/join/guest", "", `{}`), http.StatusForbidden, schema.ErrorCodeHTTPSRequired)
	if n, err := f.srv.store.LiveGuests(context.Background()); err != nil || n != 0 {
		t.Errorf("live guests = %d, %v", n, err)
	}
}

// A guest as the API shows it: whoami, the member list, its posts; End visit
// answers the ended visit, and the token is dead after it while the post's
// attribution stays.
func TestGuestVisitOverREST(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	g := newGuest(t, f.srv, "Pat", time.Hour, 24*time.Hour, f.ops.ID)

	rec := f.do(t, "GET", "/v1/whoami", g.Token, "")
	who := decodeBody[schema.WhoAmIResponseV1](t, rec)
	if rec.Code != 200 || who.IdentityClass != schema.IdentityGuest || who.Name != g.Visit.Handle || who.Guest == nil ||
		who.Guest.Handle != g.Visit.Handle || who.Guest.Label != "Pat" || who.Guest.State != schema.GuestStateActive {
		t.Fatalf("whoami = %d %s", rec.Code, rec.Body)
	}
	if err := who.Guest.Validate(); err != nil {
		t.Errorf("whoami's visit does not validate: %v", err)
	}
	if who.Guest.CanConvert {
		t.Error("can_convert is true, but the bare address of an unconfigured server offers no account")
	}

	rec = f.do(t, "GET", "/v1/channels/ops/members", f.tokens["ann"], "")
	members := decodeBody[schema.ListChannelMembersResponseV1](t, rec)
	var seen bool
	for _, m := range members.Members {
		if m.PrincipalID == g.Principal.ID {
			seen = true
			if m.Guest == nil || m.Guest.Handle != g.Visit.Handle || m.Guest.Tag != schema.GuestTag(g.Visit.Handle) {
				t.Errorf("member entry = %+v, want its guest attribution", m)
			}
			if m.CanDecide != nil && *m.CanDecide {
				t.Errorf("guest member can decide: %+v", m)
			}
		}
	}
	if !seen {
		t.Fatalf("the guest is not in ops's members: %s", rec.Body)
	}
	if err := (schema.ChannelMembersV1{Schema: schema.ChannelMembersSchemaV1, ChannelID: f.ops.ID, Members: members.Members}).Validate(); err != nil {
		t.Errorf("the member list does not validate: %v", err)
	}

	rec = f.do(t, "POST", "/v2/channels/ops/messages", g.Token, `{"body":"hello from a guest"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post = %d %s", rec.Code, rec.Body)
	}
	posted := decodeBody[schema.PostMessageResponseV2](t, rec).Message
	if posted.GuestAuthor == nil || posted.GuestAuthor.Handle != g.Visit.Handle || posted.GuestAuthor.Label != "Pat" {
		t.Fatalf("posted guest_author = %+v", posted.GuestAuthor)
	}

	rec = f.do(t, "POST", "/v1/guest/end", g.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("end visit = %d %s", rec.Code, rec.Body)
	}
	ended := decodeBody[schema.EndVisitResponseV1](t, rec).Guest
	if ended.State != schema.GuestStateEnded || ended.EndReason != schema.GuestEndVisit || ended.EndedAt == nil {
		t.Errorf("end visit answered %+v", ended)
	}
	if err := ended.Validate(); err != nil {
		t.Errorf("the ended visit does not validate: %v", err)
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/guest/end", g.Token, ""), http.StatusUnauthorized, "unauthenticated")
	assertErrorBody(t, f.do(t, "GET", "/v1/whoami", g.Token, ""), http.StatusUnauthorized, "unauthenticated")

	rec = f.do(t, "GET", "/v2/channels/ops/messages", f.tokens["ann"], "")
	list := decodeBody[schema.ListMessagesResponseV2](t, rec)
	var kept bool
	for _, m := range list.Messages {
		if m.ID == posted.ID {
			kept = m.GuestAuthor != nil && m.GuestAuthor.Handle == g.Visit.Handle
		}
	}
	if !kept {
		t.Errorf("after the end the post lost its attribution: %s", rec.Body)
	}
	// Nobody else has a visit to end.
	for _, tok := range []string{f.rootTok, f.tokens["ann"], f.tokens["robo"]} {
		assertErrorBody(t, f.do(t, "POST", "/v1/guest/end", tok, ""), http.StatusForbidden, schema.ErrorCodeNotAGuest)
	}
}

// With authentication off there is no visit: End visit is refused as the
// design's routes are (§7.4).
func TestEndVisitWithAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	assertErrorBody(t, f.do(t, "POST", "/v1/guest/end", "", ""), http.StatusForbidden, schema.ErrorCodeAccountsRequireAuth)
}

// What the existing routes refuse a guest, or refuse to do to one.
func TestGuestRefusalsOnExistingRoutes(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	g := newGuest(t, f.srv, "", time.Hour, 24*time.Hour, f.ops.ID)
	id := g.Principal.ID
	tests := []struct {
		name, method, path, token, body string
		status                          int
		code                            string
	}{
		{"it renames itself", "PUT", fmt.Sprintf("/v1/principals/%d/name", id), g.Token, `{"name":"pat"}`, 409, schema.ErrorCodeGuestNotHideable},
		{"an operator renames it", "PUT", fmt.Sprintf("/v1/principals/%d/name", id), f.rootTok, `{"name":"pat"}`, 409, schema.ErrorCodeGuestNotHideable},
		{"an operator hides it", "POST", fmt.Sprintf("/v1/principals/%d/hide", id), f.rootTok, "", 409, schema.ErrorCodeGuestNotHideable},
		{"an operator gives it a credential", "POST", fmt.Sprintf("/v1/principals/%d/credentials", id), f.rootTok, `{"label":"x"}`, 403, schema.ErrorCodeGuestNotAllowed},
		{"an operator rotates its credential", "POST", fmt.Sprintf("/v1/credentials/%d/rotate", g.Credential.ID), f.rootTok, "", 403, schema.ErrorCodeGuestNotAllowed},
		{"an operator gives it the decide right", "PUT", fmt.Sprintf("/v1/channels/ops/members/%d", id), f.rootTok, `{"can_decide":true}`, 403, schema.ErrorCodeGuestNotAllowed},
		{"an operator makes it a sign-in link", "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", id), f.rootTok, `{}`, 400, "invalid_request"},
		{"a member takes its name", "POST", "/v0/principals", f.rootTok, `{"kind":"human","name":"Guest-x"}`, 409, schema.ErrorCodePrincipalExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorBody(t, f.do(t, tt.method, tt.path, tt.token, tt.body), tt.status, tt.code)
		})
	}
	// An operator adding it without saying leaves the right off.
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/ops2/members/%d", id), f.rootTok, ""); rec.Code/100 != 2 {
		t.Fatalf("admit to ops2 = %d %s", rec.Code, rec.Body)
	}
	if can, err := f.srv.store.CanDecideApprovals(context.Background(), f.ops2.ID, id); err != nil || can {
		t.Errorf("guest can decide in ops2: %v, %v", can, err)
	}
	// The visit is untouched by all of it.
	if p, err := f.srv.store.ResolveCredential(context.Background(), g.Token); err != nil || p.Name != g.Visit.Handle || !p.IsGuest() {
		t.Errorf("the guest after the refusals: %+v, %v", p, err)
	}
}

// A refresh or a dropped connection is not a logout: the same live token
// opens its sockets again and keeps working.
func TestGuestReconnectsWhileLive(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	g := newGuest(t, f.srv, "", time.Hour, 24*time.Hour, f.ops.ID)
	base := wsTestServer(t, f.srv)
	for i := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn := dialAs(t, ctx, base, "/v2/ws?channel=ops", g.Token)
		events := dialEvents(t, base, "/v1/events/ws?channel=ops", g.Token)
		if fr := events.next(); fr.schema != schema.SessionRosterSchemaV1 {
			t.Fatalf("round %d: events first frame = %s", i, fr)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "refresh")
		_ = events.conn.Close(websocket.StatusNormalClosure, "refresh")
		cancel()
		if rec := f.do(t, "GET", "/v1/whoami", g.Token, ""); rec.Code != 200 {
			t.Fatalf("round %d: whoami = %d", i, rec.Code)
		}
	}
}

// Every way a visit ends closes every socket of the guest within the bound,
// in each family, and leaves a member's sockets open; a reconnect with the
// token is refused. The sweeper case is the real-clock idle expiry, with the
// sockets answering pings throughout: an open socket and its keepalive do
// not keep a visit alive (review F5).
func TestGuestEndClosesItsSockets(t *testing.T) {
	ends := []struct {
		name string
		// idle is the visit's idle bound; the sweeper runs when it is short.
		idle  time.Duration
		end   func(t *testing.T, f *presenceFixture, g store.CreatedGuest)
		bound time.Duration
	}{
		{"end visit", time.Hour, func(t *testing.T, f *presenceFixture, g store.CreatedGuest) {
			if rec := f.do(t, "POST", "/v1/guest/end", g.Token, ""); rec.Code != 200 {
				t.Fatalf("end visit = %d %s", rec.Code, rec.Body)
			}
		}, guestSocketBound},
		{"sign out", time.Hour, func(t *testing.T, f *presenceFixture, g store.CreatedGuest) {
			if rec := f.do(t, "DELETE", "/v1/credentials/current", g.Token, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
			}
		}, guestSocketBound},
		{"sign out everywhere", time.Hour, func(t *testing.T, f *presenceFixture, g store.CreatedGuest) {
			if rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", g.Principal.ID), g.Token, ""); rec.Code != 200 {
				t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
			}
		}, guestSocketBound},
		{"operator revokes", time.Hour, func(t *testing.T, f *presenceFixture, g store.CreatedGuest) {
			if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", g.Credential.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
			}
		}, guestSocketBound},
		{"operator disables", time.Hour, func(t *testing.T, f *presenceFixture, g store.CreatedGuest) {
			if rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", g.Principal.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("disable = %d %s", rec.Code, rec.Body)
			}
		}, guestSocketBound},
		// Idle at 1.5 s, never touched by a REST request: the sweeper's
		// one-second fallback plus the socket bound.
		{"idle expiry, sockets pinging", 1500 * time.Millisecond, func(*testing.T, *presenceFixture, store.CreatedGuest) {}, 1500*time.Millisecond + defaultGuestSweepInterval + guestSocketBound},
	}
	for _, e := range ends {
		t.Run(e.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			f.srv.credRecheckInterval = time.Hour
			f.srv.pingInterval, f.srv.pingTimeout = 100*time.Millisecond, time.Second
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stop := f.srv.startGuestSweeper(ctx)
			defer stop()
			g := newGuest(t, f.srv, "", e.idle, time.Hour, f.ops.ID)
			base := wsTestServer(t, f.srv)
			families := []string{"/v0/ws?channel=ops", "/v1/ws?channel=ops", "/v2/ws?channel=ops", "/v1/events/ws?channel=ops",
				"/v1/voice/ws?channel=ops", "/v1/voice/occupancy/ws"}
			var guestLogs []*frameLog
			for _, path := range families {
				guestLogs = append(guestLogs, recordFrames(dialAs(t, ctx, base, path, g.Token)))
			}
			member := recordFrames(dialAs(t, ctx, base, "/v2/ws?channel=ops", f.tokens["ann"]))
			start := time.Now()
			e.end(t, f, g)
			for i, l := range guestLogs {
				select {
				case <-l.done:
				case <-time.After(e.bound + time.Second):
					t.Fatalf("%s stayed open %v after the end", families[i], time.Since(start))
				}
			}
			if took := time.Since(start); took > e.bound {
				t.Errorf("the guest's sockets closed %v after the end, bound %v", took, e.bound)
			}
			if !member.open() {
				t.Errorf("a member's socket closed: %v", member.err)
			}
			// The token is dead for a reconnect, on every transport.
			if _, resp, err := websocket.Dial(ctx, base+"/v2/ws?channel=ops", &websocket.DialOptions{ //nolint:bodyclose // refused, never upgraded
				HTTPHeader: http.Header{"Authorization": {"Bearer " + g.Token}}}); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("reconnect after the end: %v", err)
			}
			assertErrorBody(t, f.do(t, "POST", "/v1/ws-tickets", g.Token, ""), http.StatusUnauthorized, "unauthenticated")
			v, err := f.srv.store.GuestVisitOf(context.Background(), g.Principal.ID)
			if err != nil || v.State != schema.GuestStateEnded {
				t.Fatalf("visit = %+v, %v", v, err)
			}
			// Its channels' member lists no longer hold it.
			members, err := f.srv.store.ListChannelMembers(context.Background(), f.ops.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range members {
				if m.PrincipalID == g.Principal.ID {
					t.Errorf("the ended guest is still a member of ops")
				}
			}
		})
	}
}

// An expired visit is refused before any sweep runs, and the sweep that runs
// when the server starts ends it: a restart does not keep it alive.
func TestGuestExpiredBeforeTheSweep(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	g := newGuest(t, f.srv, "", time.Hour, 300*time.Millisecond, f.ops.ID)
	time.Sleep(400 * time.Millisecond)
	assertErrorBody(t, f.do(t, "GET", "/v1/whoami", g.Token, ""), http.StatusUnauthorized, "unauthenticated")
	if v, err := f.srv.store.GuestVisitOf(context.Background(), g.Principal.ID); err != nil || v.State != schema.GuestStateActive {
		t.Fatalf("before the sweep: %+v, %v", v, err)
	}
	if n, err := f.srv.store.LiveGuests(context.Background()); err != nil || n != 0 {
		t.Errorf("an expired visit counts as live: %d, %v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := f.srv.startGuestSweeper(ctx)
	defer stop()
	waitUntil(t, "the sweeper's first pass ends the visit", func() bool {
		v, err := f.srv.store.GuestVisitOf(context.Background(), g.Principal.ID)
		return err == nil && v.State == schema.GuestStateEnded && v.EndReason == schema.GuestEndExpired
	})
}

// Ending a visit takes the guest out of voice before the response: it is
// removed by name from the rooms of the channels it was in. A visit past its
// deadline is removed by the next pass, without waiting for the sweep.
func TestGuestMediaTeardown(t *testing.T) {
	t.Run("end visit", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		g := newGuest(t, f.srv, "", time.Hour, time.Hour, f.ops.ID, f.ops2.ID)
		rooms := map[string]string{"ops": f.inUse(t, f.ops), "ops2": f.inUse(t, f.ops2)}
		identity := voiceIdentity(g.Principal.ID)
		for _, r := range rooms {
			f.lk.setRoom(r, fakeParticipant{identity: identity, joinedMs: 1_000, published: true},
				fakeParticipant{identity: f.identity("ann"), joinedMs: 2_000})
		}
		f.pass(t)
		base := len(f.lk.calls())
		if code := f.doOrdered(t, "POST", "/v1/guest/end", g.Token); code != http.StatusOK {
			t.Fatalf("end visit = %d", code)
		}
		var removed []string
		respAt := 0
		for _, c := range f.lk.calls()[base:] {
			switch c.method {
			case "RemoveParticipant":
				if c.identity != identity {
					t.Errorf("removed %q, want the guest only", c.identity)
				}
				if respAt != 0 {
					t.Errorf("RemoveParticipant came after the response")
				}
				removed = append(removed, c.room)
			case "http-response":
				respAt = c.seq
			}
		}
		slices.Sort(removed)
		want := []string{rooms["ops"], rooms["ops2"]}
		slices.Sort(want)
		if !slices.Equal(removed, want) {
			t.Fatalf("RemoveParticipant rooms = %v, want both channels' rooms", removed)
		}
		if got := f.who(t, f.ops); strings.Contains(got, ",") || got == "-" {
			t.Errorf("presence after the end = %q, want ann alone", got)
		}
	})
	t.Run("deadline before the sweep", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		g := newGuest(t, f.srv, "", time.Hour, 300*time.Millisecond, f.ops.ID)
		room := f.inUse(t, f.ops)
		identity := voiceIdentity(g.Principal.ID)
		f.lk.setRoom(room, fakeParticipant{identity: identity, joinedMs: 1_000, published: true})
		f.pass(t)
		time.Sleep(400 * time.Millisecond)
		f.clock.Advance(testPassGap)
		f.pass(t)
		if f.lk.in(room, identity) {
			t.Fatal("the guest is still in the room after a pass past its deadline")
		}
		var found bool
		for _, e := range f.audits(t, store.AuditVoiceParticipantRemoved) {
			if strings.Contains(e.Detail, "reason="+voiceReasonGuestEnded) {
				found = true
			}
		}
		if !found {
			t.Errorf("no voice_participant_removed with reason %s", voiceReasonGuestEnded)
		}
		if v, err := f.srv.store.GuestVisitOf(context.Background(), g.Principal.ID); err != nil || v.State != schema.GuestStateActive {
			t.Errorf("the sweep was not needed, yet the visit is %+v, %v", v, err)
		}
	})
}
