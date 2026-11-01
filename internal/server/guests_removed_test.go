package server

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Guests are removed for now (issue #1005): no route starts a visit, none
// makes an entry point that admits one, and no front door or preview offers
// one, whatever a database from before the change holds. Visits already
// live can still end, be ended and convert. The helpers below set up, under
// the store, what such a database holds, since no route can write it now.

// legacyExec runs query on srv's database file, underneath the store.
func legacyExec(t *testing.T, srv *Server, query string, args ...any) {
	t.Helper()
	path, ok := testDBPaths.Load(srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// legacySettingRows writes join settings rows as given, and the open
// channels, underneath the store: what a server from before issue #1005
// could hold, which Open would turn off and no route can write now.
//
// Since migration 40 (issue #979) the mode is stored as open_signup and the
// open channels are the default channels, so the rows are written as that
// migration leaves them: open_joining account or both as open_signup true,
// guest or off as false, and each channel a public default.
func legacySettingRows(t *testing.T, srv *Server, rows map[string]string, channels ...store.Channel) {
	t.Helper()
	for k, v := range rows {
		if k == "open_joining" {
			k = "open_signup"
			v = strconv.FormatBool(v == string(schema.OpenJoiningAccount) || v == string(schema.OpenJoiningBoth))
		}
		legacyExec(t, srv, `INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, 0)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v)
	}
	for _, c := range channels {
		legacyExec(t, srv, `UPDATE channels SET visibility = 'public', is_default = 1 WHERE id = ?`, c.ID)
	}
}

// legacyGuestSettings writes the joining settings a server from before
// issue #1005 could hold: guests enabled, the bare address in mode, open to
// channels.
func legacyGuestSettings(t *testing.T, srv *Server, mode schema.OpenJoiningMode, channels ...store.Channel) {
	t.Helper()
	legacySettingRows(t, srv, map[string]string{"guests_enabled": "true", "open_joining": string(mode)}, channels...)
}

// legacyInvite makes an invite that admits accounts, then gives it join_as
// mode underneath the store, as an invite made before issue #1005 has.
func (f *inviteFixture) legacyInvite(t *testing.T, req schema.CreateInviteRequestV1, mode schema.InviteJoinMode) schema.CreateInviteResponseV1 {
	t.Helper()
	req.JoinAs = schema.InviteJoinAccount
	resp := f.createInvite(t, f.rootTok, req)
	legacyExec(t, f.srv, "UPDATE invites SET join_as = ? WHERE id = ?", string(mode), resp.Invite.ID)
	resp.Invite.JoinAs = mode
	return resp
}

// seededGuest is the answer an entry route gave for a visit, for a visit
// started through the store.
func (f *authFixture) seededGuest(t *testing.T, g store.CreatedGuest, channels []schema.InviteChannelV1) schema.JoinGuestResponseV1 {
	t.Helper()
	return schema.JoinGuestResponseV1{
		Principal:  principalFromStore(g.Principal),
		Guest:      f.srv.guestV1(context.Background(), g.Visit),
		Credential: g.Credential,
		Token:      g.Token,
		Channels:   channels,
	}
}

// seedGuest starts a visit from the bare address through the store, as POST
// /v1/join/guest did before issue #1005: admitted to every open channel
// under the settings' bounds and voice.
func (f *authFixture) seedGuest(t *testing.T, label string) schema.JoinGuestResponseV1 {
	t.Helper()
	ctx := context.Background()
	set, _, err := f.srv.store.JoinSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	var channels []schema.InviteChannelV1
	for _, name := range set.OpenChannels {
		c, err := f.srv.store.ChannelByName(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
		channels = append(channels, schema.InviteChannelV1{ID: c.ID, Name: c.Name})
	}
	g, err := f.srv.store.CreateGuest(ctx, "system", store.NewGuest{Label: label, ServerName: f.srv.serverName(),
		Origin: schema.GuestOriginAddress, Channels: ids, VoiceAllowed: set.JoinVoice,
		Idle: time.Duration(set.GuestIdleSeconds) * time.Second, Absolute: time.Duration(set.GuestAbsoluteSeconds) * time.Second})
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	return f.seededGuest(t, g, channels)
}

// seedInviteGuest starts a visit through inv by the store, as POST
// /v1/invites/redeem-guest did before issue #1005, spending one use.
func (f *inviteFixture) seedInviteGuest(t *testing.T, inv schema.InviteV1, label string) schema.JoinGuestResponseV1 {
	t.Helper()
	ctx := context.Background()
	set, _, err := f.srv.store.JoinSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(inv.Channels))
	for i, c := range inv.Channels {
		ids[i] = c.ID
	}
	g, err := f.srv.store.CreateGuest(ctx, "invite:"+strconv.FormatInt(inv.ID, 10), store.NewGuest{Label: label, ServerName: f.srv.serverName(),
		Origin: schema.GuestOriginInvite, InviteID: inv.ID, InvitedBy: inv.CreatedBy, Channels: ids, VoiceAllowed: inv.Voice,
		Idle: time.Duration(set.GuestIdleSeconds) * time.Second, Absolute: time.Duration(set.GuestAbsoluteSeconds) * time.Second,
		CredentialLifetime: time.Duration(inv.CredentialLifetimeSeconds) * time.Second})
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	legacyExec(t, f.srv, "UPDATE invites SET uses = uses + 1 WHERE id = ?", inv.ID)
	return f.seededGuest(t, g, inv.Channels)
}

// guestCount is how many visits the store holds, in any state.
func (f *authFixture) guestCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, st := range []schema.GuestState{schema.GuestStateActive, schema.GuestStateEnded, schema.GuestStateConverted} {
		v, _, err := f.srv.store.ListGuests(context.Background(), store.GuestFilter{State: st, Limit: schema.MaxListGuestsLimit})
		if err != nil {
			t.Fatal(err)
		}
		n += len(v)
	}
	return n
}

// No route starts a visit: the address join and guest redemption answer 403
// guests_disabled whatever the body, the code or the stored policy, start
// no visit and spend no use, and an invite made to admit guests still makes
// accounts if it admitted them.
func TestGuestsRemovedNoRouteStartsAVisit(t *testing.T) {
	f := newInviteFixture(t, Config{})
	legacyGuestSettings(t, f.srv, schema.OpenJoiningBoth, f.red)
	open := schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"red"}}
	both := f.legacyInvite(t, open, schema.InviteJoinBoth)
	guestOnly := f.legacyInvite(t, open, schema.InviteJoinGuest)
	unknown := schema.InviteCodePrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for _, tt := range []struct {
		name, path, body string
	}{
		{"address, no label", "/v1/join/guest", `{}`},
		{"address, a label", "/v1/join/guest", inviteJSON(t, schema.JoinGuestRequestV1{Label: "Pat"})},
		{"address, a bad label", "/v1/join/guest", `{"label":"\u0000"}`},
		{"address, no body", "/v1/join/guest", ``},
		{"invite admitting both", "/v1/invites/redeem-guest", inviteJSON(t, schema.RedeemInviteGuestRequestV1{Code: both.Code, Label: "Pat"})},
		{"invite admitting guests only", "/v1/invites/redeem-guest", inviteJSON(t, schema.RedeemInviteGuestRequestV1{Code: guestOnly.Code})},
		{"an unknown code", "/v1/invites/redeem-guest", inviteJSON(t, schema.RedeemInviteGuestRequestV1{Code: unknown})},
		{"a malformed code", "/v1/invites/redeem-guest", `{"code":"nope"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.send(t, "POST", tt.path, "", tt.body, "")
			assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
		})
	}
	if n := f.guestCount(t); n != 0 {
		t.Fatalf("%d visits exist after the refusals, want none", n)
	}
	invites, err := f.srv.store.ListInvites(context.Background(), store.InviteFilter{}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range invites {
		if inv.Uses != 0 {
			t.Errorf("invite %d (%s): uses = %d after refused visits, want 0", inv.ID, inv.JoinAs, inv.Uses)
		}
	}
	// The invite that admitted both still makes an account; the guest-only
	// one makes nothing, as before.
	rec := f.send(t, "POST", "/v2/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV2{Code: both.Code, Name: "newcomer", Password: testJoinPassword}), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem the both invite for an account = %d %s", rec.Code, rec.Body)
	}
	rec = f.send(t, "POST", "/v2/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV2{Code: guestOnly.Code, Name: "another", Password: testJoinPassword}), "")
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeJoiningClosed)
}

// No route makes an entry point that admits guests: an invite admits
// accounts only, and the settings refuse guests_enabled true and the guest
// modes with 409 join_policy_conflict, writing nothing; the rest still
// work.
func TestGuestsRemovedNoGuestEntryPoint(t *testing.T) {
	for _, tt := range []struct {
		name   string
		joinAs schema.InviteJoinMode
		status int
		code   string
	}{
		{"join_as guest", schema.InviteJoinGuest, http.StatusConflict, schema.ErrorCodeJoinPolicyConflict},
		{"join_as both", schema.InviteJoinBoth, http.StatusConflict, schema.ErrorCodeJoinPolicyConflict},
		{"join_as account", schema.InviteJoinAccount, http.StatusCreated, ""},
		{"join_as left out", "", http.StatusCreated, ""},
	} {
		t.Run("invite "+tt.name, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			rec := f.send(t, "POST", "/v1/invites", f.rootTok,
				inviteJSON(t, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"red"}, JoinAs: tt.joinAs}), "")
			if tt.code != "" {
				assertErrorBody(t, rec, tt.status, tt.code)
				if n := len(f.auditOf(t, store.AuditInviteCreated)); n != 0 {
					t.Errorf("%d invite_created rows after a refusal, want none", n)
				}
				return
			}
			if rec.Code != tt.status {
				t.Fatalf("create = %d %s", rec.Code, rec.Body)
			}
			if got := decodeBody[schema.CreateInviteResponseV1](t, rec).Invite.JoinAs; got != schema.InviteJoinAccount {
				t.Errorf("join_as = %q, want account", got)
			}
		})
	}

	on, off := true, false
	for _, tt := range []struct {
		name   string
		req    schema.PutJoinSettingsRequestV1
		status int
		want   schema.OpenJoiningMode
	}{
		{"guests on", schema.PutJoinSettingsRequestV1{GuestsEnabled: &on}, http.StatusConflict, ""},
		{"guests on with accounts", schema.PutJoinSettingsRequestV1{GuestsEnabled: &on, OpenJoining: modePtr(schema.OpenJoiningAccount), OpenChannels: &[]string{"red"}}, http.StatusConflict, ""},
		{"guest-only address", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningGuest), OpenChannels: &[]string{"red"}}, http.StatusConflict, ""},
		{"address open to both", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningBoth), OpenChannels: &[]string{"red"}}, http.StatusConflict, ""},
		{"both with guests off", schema.PutJoinSettingsRequestV1{GuestsEnabled: &off, OpenJoining: modePtr(schema.OpenJoiningBoth), OpenChannels: &[]string{"red"}}, http.StatusConflict, ""},
		{"accounts", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningAccount), OpenChannels: &[]string{"red"}}, http.StatusOK, schema.OpenJoiningAccount},
		{"off", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningOff)}, http.StatusOK, schema.OpenJoiningOff},
		{"guests off", schema.PutJoinSettingsRequestV1{GuestsEnabled: &off, MaxGuests: int64Ptr(3)}, http.StatusOK, schema.OpenJoiningOff},
	} {
		t.Run("settings "+tt.name, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			rec := f.putJoin(t, tt.req)
			if tt.status == http.StatusConflict {
				assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodeJoinPolicyConflict)
				if n := len(f.auditOf(t, store.AuditJoinSettingsChanged)) + len(f.auditOf(t, store.AuditOpenChannelAdded)); n != 0 {
					t.Errorf("%d settings rows after a refusal, want none", n)
				}
				return
			}
			if rec.Code != tt.status {
				t.Fatalf("put = %d %s", rec.Code, rec.Body)
			}
			got := decodeBody[schema.JoinSettingsResponseV1](t, rec).Settings
			if got.GuestsEnabled || got.OpenJoining != tt.want {
				t.Errorf("settings = guests %v, mode %q; want guests off, mode %q", got.GuestsEnabled, got.OpenJoining, tt.want)
			}
		})
	}
}

// A server whose stored rows still admit guests, before a restart has
// normalized them, offers no visit at its front door or in an invite's
// preview, and a settings change it accepts leaves guests off.
func TestGuestsRemovedNothingOffersAVisit(t *testing.T) {
	f := newInviteFixture(t, Config{})
	legacyGuestSettings(t, f.srv, schema.OpenJoiningBoth, f.red)
	if got := f.frontDoor(t).Join; got.Guest || got.GuestsFull || !got.Account {
		t.Errorf("front door = %+v, want accounts only", got)
	}
	open := schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"red"}}
	for _, tt := range []struct {
		mode    schema.InviteJoinMode
		account bool
	}{{schema.InviteJoinBoth, true}, {schema.InviteJoinGuest, false}} {
		inv := f.legacyInvite(t, open, tt.mode)
		rec := f.send(t, "POST", "/v1/invites/preview", "", inviteJSON(t, schema.InvitePreviewRequestV1{Code: inv.Code}), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("preview = %d %s", rec.Code, rec.Body)
		}
		got := decodeBody[schema.InvitePreviewV1](t, rec).Join
		if got == nil || got.Guest || got.GuestsFull || got.Account != tt.account {
			t.Errorf("preview of a %s invite: join = %+v, want account %v and no guest", tt.mode, got, tt.account)
		}
	}
	resp := f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{MaxGuests: int64Ptr(7)})
	if resp.Settings.GuestsEnabled || resp.Settings.OpenJoining != schema.OpenJoiningAccount {
		t.Errorf("after a change: guests %v, mode %q; want guests off and accounts", resp.Settings.GuestsEnabled, resp.Settings.OpenJoining)
	}
}

// A visit already live when guests were removed can finish: the guest ends
// it, an operator ends it, End all guest visits ends it, or the guest
// converts, and a post it made keeps its guest attribution.
func TestGuestsRemovedExistingVisitsFinish(t *testing.T) {
	setup := func(t *testing.T) (*inviteFixture, schema.JoinGuestResponseV1) {
		f := newInviteFixture(t, Config{})
		f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningAccount), OpenChannels: &[]string{"red"}})
		return f, f.seedGuest(t, "Pat")
	}
	t.Run("End visit", func(t *testing.T) {
		f, g := setup(t)
		rec := f.send(t, "POST", "/v1/guest/end", g.Token, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("end visit = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("an operator's end", func(t *testing.T) {
		f, g := setup(t)
		rec := f.send(t, "POST", "/v1/guests/"+strconv.FormatInt(g.Principal.ID, 10)+"/end", f.rootTok, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("end guest = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("End all guest visits", func(t *testing.T) {
		f, _ := setup(t)
		rec := f.send(t, "POST", "/v1/guests/end", f.rootTok, `{"dry_run":false}`, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("end all = %d %s", rec.Code, rec.Body)
		}
		if got := decodeBody[schema.EndGuestsResponseV1](t, rec); got.Count != 1 {
			t.Errorf("ended %d visits, want 1", got.Count)
		}
	})
	t.Run("conversion keeps the guest's post", func(t *testing.T) {
		f, g := setup(t)
		rec := f.send(t, "POST", "/v2/channels/red/messages", g.Token, `{"body":"hello from a guest"}`, "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("guest post = %d %s", rec.Code, rec.Body)
		}
		msg := decodeBody[schema.PostMessageResponseV2](t, rec).Message
		rec = f.send(t, "POST", "/v1/guest/convert", g.Token, inviteJSON(t, schema.ConvertGuestRequestV1{Name: "patricia", Password: testJoinPassword}), "")
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("convert = %d %s", rec.Code, rec.Body)
		}
		rec = f.send(t, "GET", "/v2/channels/red/messages", f.aliceTok, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("history = %d %s", rec.Code, rec.Body)
		}
		for _, m := range decodeBody[schema.ListMessagesResponseV2](t, rec).Messages {
			if m.ID == msg.ID && (m.GuestAuthor == nil || m.GuestAuthor.Handle != g.Guest.Handle) {
				t.Errorf("the guest's post after conversion: guest_author = %+v, want the visit's handle %q", m.GuestAuthor, g.Guest.Handle)
			}
		}
	})
}
