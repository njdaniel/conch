package server

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Guest entry (issue #710): the front door, Join as guest from the bare
// address and through an invite, the joining settings, the guest list and
// End all guest visits. The policy matrix checks that what a preview offers
// and what a write allows agree; the race tests that the policy, the caps
// and the open channels are checked in the transaction that starts a visit.

func modePtr(m schema.OpenJoiningMode) *schema.OpenJoiningMode { return &m }

// openTo is the settings change that lets guests join from the bare address
// into channels.
func openTo(channels ...string) schema.PutJoinSettingsRequestV1 {
	return schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(true), OpenJoining: modePtr(schema.OpenJoiningGuest), OpenChannels: &channels}
}

func (f *authFixture) putJoin(t *testing.T, req schema.PutJoinSettingsRequestV1) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, "PUT", "/v1/join/settings", f.rootTok, inviteJSON(t, req))
}

func (f *authFixture) mustPutJoin(t *testing.T, req schema.PutJoinSettingsRequestV1) schema.JoinSettingsResponseV1 {
	t.Helper()
	rec := f.putJoin(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put join settings = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.JoinSettingsResponseV1](t, rec)
}

// secureDo is authFixture.do over HTTPS, as the guest entry routes need
// (secureTransport).
func secureDo(t *testing.T, f *authFixture, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.TLS = &tls.ConnectionState{}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *authFixture) frontDoor(t *testing.T) schema.JoinInfoV1 {
	t.Helper()
	rec := f.do(t, "GET", "/v1/join", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("front door = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.JoinInfoV1](t, rec)
}

// joinGuest asks for a visit from the bare address, from remote.
func (f *inviteFixture) joinGuest(t *testing.T, label, remote string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/join/guest", "", inviteJSON(t, schema.JoinGuestRequestV1{Label: label}), remote)
}

func (f *inviteFixture) redeemGuest(t *testing.T, code, label, remote string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/invites/redeem-guest", "", inviteJSON(t, schema.RedeemInviteGuestRequestV1{Code: code, Label: label}), remote)
}

func mustJoined(t *testing.T, rec *httptest.ResponseRecorder) schema.JoinGuestResponseV1 {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("join = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: the answer carries a token", got)
	}
	return decodeBody[schema.JoinGuestResponseV1](t, rec)
}

// auditOf returns the rows of action, oldest first.
func (f *authFixture) auditOf(t *testing.T, action string) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// The bare address under every combination of the master switch, its mode
// and its pause: what the front door offers is exactly what Join as guest
// and Create an account (issue #711) do. Each case runs both writes, and a
// refused one leaves nothing behind.
func TestGuestEntryAddressMatrix(t *testing.T) {
	const closed, paused = schema.ErrorCodeJoiningClosed, schema.ErrorCodeJoiningPaused
	tests := []struct {
		name     string
		enabled  bool
		mode     schema.OpenJoiningMode
		paused   bool
		offer    schema.JoinOptionsV1
		status   int
		code     string
		accountC string // the refusal of an account, or "" when one is made
	}{
		{"nothing changed", false, schema.OpenJoiningOff, false, schema.JoinOptionsV1{}, 403, schema.ErrorCodeGuestsDisabled, closed},
		{"guests on, address off", true, schema.OpenJoiningOff, false, schema.JoinOptionsV1{}, 403, schema.ErrorCodeGuestsDisabled, closed},
		{"guests on, address accounts only", true, schema.OpenJoiningAccount, false, schema.JoinOptionsV1{Account: true}, 403, schema.ErrorCodeGuestsDisabled, ""},
		{"guests off, address both", false, schema.OpenJoiningBoth, false, schema.JoinOptionsV1{Account: true}, 403, schema.ErrorCodeGuestsDisabled, ""},
		{"guests off, address accounts only", false, schema.OpenJoiningAccount, false, schema.JoinOptionsV1{Account: true}, 403, schema.ErrorCodeGuestsDisabled, ""},
		{"guests on, address guest", true, schema.OpenJoiningGuest, false, schema.JoinOptionsV1{Guest: true}, 201, "", closed},
		{"guests on, address both", true, schema.OpenJoiningBoth, false, schema.JoinOptionsV1{Guest: true, Account: true}, 201, "", ""},
		{"guests on, address guest, paused", true, schema.OpenJoiningGuest, true, schema.JoinOptionsV1{Paused: true}, 403, schema.ErrorCodeJoiningPaused, closed},
		{"guests on, address both, paused", true, schema.OpenJoiningBoth, true, schema.JoinOptionsV1{Paused: true}, 403, schema.ErrorCodeJoiningPaused, paused},
		{"guests off, address accounts only, paused", false, schema.OpenJoiningAccount, true, schema.JoinOptionsV1{Paused: true}, 403, schema.ErrorCodeGuestsDisabled, paused},
		{"guests off, address off, paused", false, schema.OpenJoiningOff, true, schema.JoinOptionsV1{}, 403, schema.ErrorCodeGuestsDisabled, closed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			req := schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(tt.enabled), JoiningPaused: boolPtr(tt.paused),
				OpenJoining: modePtr(tt.mode), OpenChannels: &[]string{"red"}}
			f.mustPutJoin(t, req)
			if got := f.frontDoor(t).Join; got != tt.offer {
				t.Errorf("front door offers %+v, want %+v", got, tt.offer)
			}
			rec := f.joinAccount(t, "newcomer", "")
			if (tt.accountC == "") != tt.offer.Account {
				t.Fatalf("the case is wrong: an account is made (%t) where none is offered (%t)", tt.accountC == "", tt.offer.Account)
			}
			if tt.accountC == "" {
				mustAccount(t, rec)
			} else {
				assertErrorBody(t, rec, http.StatusForbidden, tt.accountC)
				if _, found, err := f.srv.store.LoginCandidate(context.Background(), "newcomer"); err != nil || found {
					t.Errorf("a refused account was made: %v, %v", found, err)
				}
			}
			rec = f.joinGuest(t, "", "")
			if tt.status == http.StatusCreated {
				mustJoined(t, rec)
				return
			}
			assertErrorBody(t, rec, tt.status, tt.code)
			if n, err := f.srv.store.LiveGuests(context.Background()); err != nil || n != 0 {
				t.Errorf("a refused join left %d live visits, %v", n, err)
			}
		})
	}
}

// Invites under every join mode and the master switch: the preview's join is
// what redemption allows, by either route; a guest-only invite makes no
// account through the passwordless v1 redemption, and nothing is spent by a
// refusal.
func TestGuestEntryInviteMatrix(t *testing.T) {
	tests := []struct {
		joinAs  schema.InviteJoinMode
		enabled bool
		offer   schema.JoinOptionsV1
	}{
		{"", false, schema.JoinOptionsV1{Account: true}},
		{"", true, schema.JoinOptionsV1{Account: true}},
		{schema.InviteJoinAccount, true, schema.JoinOptionsV1{Account: true}},
		{schema.InviteJoinGuest, false, schema.JoinOptionsV1{}},
		{schema.InviteJoinGuest, true, schema.JoinOptionsV1{Guest: true}},
		{schema.InviteJoinBoth, false, schema.JoinOptionsV1{Account: true}},
		{schema.InviteJoinBoth, true, schema.JoinOptionsV1{Guest: true, Account: true}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("join_as=%q guests=%t", tt.joinAs, tt.enabled), func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(tt.enabled)})
			req := openLink("red")
			req.JoinAs = tt.joinAs
			inv := f.createInvite(t, f.rootTok, req)
			wantMode := tt.joinAs
			if wantMode == "" {
				wantMode = schema.InviteJoinAccount
			}
			if inv.Invite.JoinAs != wantMode {
				t.Errorf("invite join_as = %q, want %q", inv.Invite.JoinAs, wantMode)
			}
			rec := f.preview(t, inv.Code)
			p := decodeBody[schema.InvitePreviewV1](t, rec)
			if p.Join == nil || *p.Join != tt.offer {
				t.Fatalf("preview join = %+v, want %+v", p.Join, tt.offer)
			}
			uses := func() int64 {
				list := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.rootTok, "", ""))
				for _, i := range list.Invites {
					if i.ID == inv.Invite.ID {
						return i.Uses
					}
				}
				t.Fatal("the invite is not listed")
				return 0
			}
			rec = f.redeemGuest(t, inv.Code, "Pat", "")
			if tt.offer.Guest {
				g := mustJoined(t, rec)
				if g.Guest.Origin != schema.GuestOriginInvite || len(g.Channels) != 1 || g.Channels[0].Name != "red" {
					t.Errorf("visit = %+v, channels %+v", g.Guest, g.Channels)
				}
			} else {
				assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
			}
			before := uses()
			rec = f.redeem(t, inv.Code, "newcomer")
			if tt.offer.Account {
				if rec.Code != http.StatusCreated {
					t.Fatalf("v1 redeem = %d %s", rec.Code, rec.Body)
				}
			} else {
				assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeJoiningClosed)
				if after := uses(); after != before {
					t.Errorf("a refused v1 redemption spent a use: %d -> %d", before, after)
				}
			}
		})
	}
}

// A paused or invalid invite and a malformed one answer as the existing
// redemption does, and the refusal reveals nothing a guest switch would not:
// with guests off every well-formed code is guests_disabled.
func TestRedeemInviteGuestRefusals(t *testing.T) {
	f := newInviteFixture(t, Config{})
	req := openLink("red")
	req.JoinAs = schema.InviteJoinGuest
	inv := f.createInvite(t, f.rootTok, req)
	unknown := schema.InviteCodePrefix + strings.Repeat("A", 43)

	assertErrorBody(t, f.redeemGuest(t, inv.Code, "", ""), http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
	assertErrorBody(t, f.redeemGuest(t, unknown, "", ""), http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(true)})
	tests := []struct {
		name, code, label string
		status            int
		errCode           string
	}{
		{"malformed code", "nope", "", 404, schema.ErrorCodeInviteInvalid},
		{"unknown code", unknown, "", 404, schema.ErrorCodeInviteInvalid},
		{"control character in the label", inv.Code, "Pa\u0007t", 400, "invalid_request"},
		{"a label in the guest namespace", inv.Code, "guest-x", 400, "invalid_request"},
		{"the server's own name", inv.Code, DefaultServerName, 400, "invalid_request"},
		{"a label in the hidden namespace", inv.Code, "Hidden-1", 400, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorBody(t, f.redeemGuest(t, tt.code, tt.label, ""), tt.status, tt.errCode)
		})
	}
	if _, err := f.srv.store.SetInvitesPaused(context.Background(), "system", true); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.redeemGuest(t, inv.Code, "", ""), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)
	assertErrorBody(t, f.preview(t, inv.Code), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)
	if n, err := f.srv.store.LiveGuests(context.Background()); err != nil || n != 0 {
		t.Errorf("refusals left %d live visits, %v", n, err)
	}
}

// A visit through an invite: what the invite grants and no more, its hard
// deadline bounded by the invite's credential lifetime, a use spent, the
// invite recorded on the principal so disabling the invite's joiners ends
// it, and every row attributed to the invite.
func TestRedeemInviteGuestVisit(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(true)})
	req := personal("red", "blue")
	req.JoinAs, req.Voice = schema.InviteJoinBoth, true
	req.CredentialLifetimeSeconds = int64Ptr(600)
	inv := f.createInvite(t, f.rootTok, req)
	g := mustJoined(t, f.redeemGuest(t, inv.Code, "Pat", ""))
	if err := g.Guest.Validate(); err != nil {
		t.Errorf("the visit does not validate: %v", err)
	}
	if g.Guest.Label != "Pat" || g.Principal.Name != g.Guest.Handle || g.Token == "" {
		t.Errorf("joined = %+v", g)
	}
	if d := g.Guest.ExpiresAt.Time().Sub(g.Guest.StartedAt.Time()); d != 600*time.Second {
		t.Errorf("visit lasts %v, want the invite's credential lifetime, 10m", d)
	}
	if names := []string{g.Channels[0].Name, g.Channels[1].Name}; len(g.Channels) != 2 || !slices.Contains(names, "red") || !slices.Contains(names, "blue") {
		t.Errorf("channels = %+v", g.Channels)
	}
	// A personal invite has one use: it is spent.
	assertErrorBody(t, f.redeemGuest(t, inv.Code, "", ""), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
	starts := f.auditOf(t, store.AuditGuestStarted)
	if len(starts) != 1 || starts[0].Actor != fmt.Sprintf("invite:%d", inv.Invite.ID) ||
		starts[0].Detail != fmt.Sprintf("origin=invite invite=%d", inv.Invite.ID) {
		t.Errorf("guest_started rows = %+v", starts)
	}
	var redeemed bool
	for _, e := range f.auditOf(t, store.AuditInviteRedeemed) {
		redeemed = redeemed || strings.HasSuffix(e.Detail, "guest=1")
	}
	if !redeemed {
		t.Error("no invite_redeemed row marks the guest redemption")
	}
	// The invite's joiners include the guest; disabling them ends its visit.
	rec := f.send(t, "POST", fmt.Sprintf("/v1/invites/%d/disable-joiners", inv.Invite.ID), f.rootTok, `{"dry_run":false}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable joiners = %d %s", rec.Code, rec.Body)
	}
	v, err := f.srv.store.GuestVisitOf(context.Background(), g.Principal.ID)
	if err != nil || v.State != schema.GuestStateEnded || v.EndReason != schema.GuestEndOperator {
		t.Errorf("after disabling the joiners the visit is %+v, %v", v, err)
	}
}

// A front door read a moment ago admits nobody the policy now refuses:
// turning guests off refuses the next join through both entry points, turns
// a guest-only address off with it, and leaves the live visit to its
// bounds, said in the answer and the audit row.
func TestGuestEntryStalePreview(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.mustPutJoin(t, openTo("red"))
	req := openLink("red")
	req.JoinAs = schema.InviteJoinGuest
	inv := f.createInvite(t, f.rootTok, req)
	if !f.frontDoor(t).Join.Guest || !decodeBody[schema.InvitePreviewV1](t, f.preview(t, inv.Code)).Join.Guest {
		t.Fatal("the entry points do not offer a visit")
	}
	live := mustJoined(t, f.joinGuest(t, "", ""))

	resp := f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(false)})
	if resp.Settings.OpenJoining != schema.OpenJoiningOff || resp.LiveGuests != 1 ||
		!slices.Contains(resp.Warnings, schema.JoinWarningLiveGuestsRemain) {
		t.Errorf("turning guests off answered %+v", resp)
	}
	rows := f.auditOf(t, store.AuditJoinSettingsChanged)
	last := rows[len(rows)-1]
	if last.Detail != "open_joining=guest->off guests_enabled=true->false live_guests=1" || last.Subject != "server" {
		t.Errorf("join_settings_changed = %+v", last)
	}

	assertErrorBody(t, f.joinGuest(t, "", ""), http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
	assertErrorBody(t, f.redeemGuest(t, inv.Code, "", ""), http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
	if p := decodeBody[schema.InvitePreviewV1](t, f.preview(t, inv.Code)); p.Join == nil || p.Join.Guest {
		t.Errorf("preview after guests went off = %+v", p.Join)
	}
	if f.frontDoor(t).Join.Guest {
		t.Error("the front door still offers a visit")
	}
	// The live visit lasts until its bounds or End all guest visits.
	if rec := f.send(t, "GET", "/v1/whoami", live.Token, "", ""); rec.Code != http.StatusOK {
		t.Errorf("the live guest after guests went off: whoami = %d", rec.Code)
	}
}

// The settings refuse contradictions and bad values, and write nothing when
// they do; a channel must exist; turning guests off while the address is
// both leaves account mode as it was.
func TestJoinSettingsRefusals(t *testing.T) {
	f := newInviteFixture(t, Config{})
	tests := []struct {
		name   string
		req    schema.PutJoinSettingsRequestV1
		status int
		code   string
	}{
		{"nothing to change", schema.PutJoinSettingsRequestV1{}, 400, "invalid_request"},
		{"guest-only with guests off", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningGuest), OpenChannels: &[]string{"red"}}, 409, schema.ErrorCodeJoinPolicyConflict},
		{"open joining with no channel", schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(true), OpenJoining: modePtr(schema.OpenJoiningBoth)}, 409, schema.ErrorCodeJoinPolicyConflict},
		{"a channel that does not exist", openTo("nosuch"), 400, "invalid_request"},
		{"an unknown mode", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr("sometimes")}, 400, "invalid_request"},
		{"a negative cap", schema.PutJoinSettingsRequestV1{MaxGuests: int64Ptr(-1)}, 400, "invalid_request"},
		{"a per-address cap of zero", schema.PutJoinSettingsRequestV1{MaxGuestsPerAddress: int64Ptr(0)}, 400, "invalid_request"},
		{"an idle bound past the absolute", schema.PutJoinSettingsRequestV1{GuestIdleSeconds: int64Ptr(90000)}, 400, "invalid_request"},
		{"an absolute bound past a day", schema.PutJoinSettingsRequestV1{GuestAbsoluteSeconds: int64Ptr(90000)}, 400, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorBody(t, f.putJoin(t, tt.req), tt.status, tt.code)
		})
	}
	if rows := f.auditOf(t, store.AuditJoinSettingsChanged); len(rows) != 0 {
		t.Errorf("refused changes wrote %d rows", len(rows))
	}
	got := decodeBody[schema.JoinSettingsResponseV1](t, f.send(t, "GET", "/v1/join/settings", f.rootTok, "", ""))
	if fmt.Sprint(got.Settings) != fmt.Sprint(schema.DefaultJoinSettings()) {
		t.Errorf("after refusals the settings are %+v", got.Settings)
	}

	// The last open channel cannot go while joining is on.
	f.mustPutJoin(t, openTo("red", "blue"))
	assertErrorBody(t, f.putJoin(t, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{}}), http.StatusConflict, schema.ErrorCodeJoinPolicyConflict)
	resp := f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"blue"}})
	if !slices.Equal(resp.Settings.OpenChannels, []string{"blue"}) {
		t.Errorf("open channels = %v", resp.Settings.OpenChannels)
	}
	if added, removed := f.auditOf(t, store.AuditOpenChannelAdded), f.auditOf(t, store.AuditOpenChannelRemoved); len(added) != 2 ||
		len(removed) != 1 || removed[0].Subject != fmt.Sprintf("channel:%d", f.red.ID) {
		t.Errorf("open channel rows: added %+v, removed %+v", added, removed)
	}
	// Both, then guests off: the address keeps its account mode.
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningBoth)})
	if resp := f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(false)}); resp.Settings.OpenJoining != schema.OpenJoiningBoth {
		t.Errorf("guests off with the address on both: mode = %q", resp.Settings.OpenJoining)
	}
	// Setting what is already set writes nothing.
	n := len(f.auditOf(t, store.AuditJoinSettingsChanged))
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(false)})
	if m := len(f.auditOf(t, store.AuditJoinSettingsChanged)); m != n {
		t.Errorf("a change to nothing wrote a row: %d -> %d", n, m)
	}
}

// Guests cannot be enabled while a permanent principal has a name in the
// reserved namespace, from before migration 31; the settings warn about it.
func TestJoinSettingsReservedNames(t *testing.T) {
	f := newInviteFixture(t, Config{})
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	keys := names.KeysOf("Guestbook")
	// Underneath the store, which refuses the name now, as a database from
	// before the reservation would hold it.
	if _, err := db.Exec(`INSERT INTO principals (kind, name, name_key, name_skel, role, created_at) VALUES ('human', 'Guestbook', ?, ?, 'member', 0)`,
		keys.Folded, keys.Skeletal); err != nil {
		t.Fatal(err)
	}
	got := decodeBody[schema.JoinSettingsResponseV1](t, f.send(t, "GET", "/v1/join/settings", f.rootTok, "", ""))
	if !slices.Contains(got.Warnings, schema.JoinWarningReservedNamesInUse) {
		t.Errorf("warnings = %v", got.Warnings)
	}
	assertErrorBody(t, f.putJoin(t, openTo("red")), http.StatusConflict, schema.ErrorCodeJoinPolicyConflict)
	// Other changes still go through.
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{MaxGuests: int64Ptr(7)})
}

// The cap on live visits holds under concurrent joins through both entry
// points: exactly max_guests start. A visit past its deadline, not yet swept,
// does not hold a place; an ended one gives its place back.
func TestGuestCapUnderConcurrentJoins(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.mustPutJoin(t, openTo("red"))
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{MaxGuests: int64Ptr(3)})
	req := openLink("red")
	req.JoinAs = schema.InviteJoinGuest
	inv := f.createInvite(t, f.rootTok, req)

	const tries = 16
	codes := make([]int, tries)
	var wg sync.WaitGroup
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote := fmt.Sprintf("198.51.100.%d:1", i+1)
			if i%2 == 0 {
				codes[i] = f.joinGuest(t, "", remote).Code
			} else {
				codes[i] = f.redeemGuest(t, inv.Code, "", remote).Code
			}
		}()
	}
	wg.Wait()
	var made, full int
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			made++
		case http.StatusTooManyRequests:
			full++
		default:
			t.Errorf("a join answered %d", c)
		}
	}
	if made != 3 || full != tries-3 {
		t.Fatalf("made %d, refused %d as full; want 3 and %d", made, full, tries-3)
	}
	if n, err := f.srv.store.LiveGuests(context.Background()); err != nil || n != 3 {
		t.Errorf("live = %d, %v", n, err)
	}
	info := f.frontDoor(t)
	if info.Join.Guest || !info.Join.GuestsFull {
		t.Errorf("front door at the cap = %+v", info.Join)
	}
	assertErrorBody(t, f.joinGuest(t, "", "203.0.113.9:1"), http.StatusTooManyRequests, schema.ErrorCodeGuestsFull)

	// End one: its place is free.
	list := decodeBody[schema.ListGuestsResponseV1](t, f.send(t, "GET", "/v1/guests", f.rootTok, "", ""))
	if rec := f.send(t, "POST", fmt.Sprintf("/v1/guests/%d/end", list.Guests[0].ID), f.rootTok, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("end = %d %s", rec.Code, rec.Body)
	}
	mustJoined(t, f.joinGuest(t, "", "203.0.113.9:1"))

	// A visit past its deadline holds no place, before any sweep.
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{MaxGuests: int64Ptr(4)})
	if _, err := f.srv.store.CreateGuest(context.Background(), "system", store.NewGuest{Origin: schema.GuestOriginAddress,
		Channels: []int64{f.red.ID}, Idle: time.Hour, Absolute: 200 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.joinGuest(t, "", "203.0.113.10:1"), http.StatusTooManyRequests, schema.ErrorCodeGuestsFull)
	time.Sleep(300 * time.Millisecond)
	mustJoined(t, f.joinGuest(t, "", "203.0.113.10:1"))
}

// The per-address cap (review F13): one address fills its share and no more,
// another address still joins; behind a trusted proxy the forwarded address
// is the one counted, and an untrusted peer cannot choose it.
func TestGuestCapPerAddress(t *testing.T) {
	proxies, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	f := newInviteFixture(t, Config{TrustedProxies: proxies})
	f.mustPutJoin(t, openTo("red"))
	resp := f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{MaxGuestsPerAddress: int64Ptr(2)})
	if slices.Contains(resp.Warnings, schema.JoinWarningTrustedProxyUnset) {
		t.Errorf("warnings = %v with a trusted proxy set", resp.Warnings)
	}
	via := func(client string) *httptest.ResponseRecorder {
		return f.send(t, "POST", "/v1/join/guest", "", `{}`, "10.0.0.1:1", client)
	}
	mustJoined(t, via("198.51.100.7"))
	mustJoined(t, via("198.51.100.7"))
	assertErrorBody(t, via("198.51.100.7"), http.StatusTooManyRequests, schema.ErrorCodeGuestsFull)
	// An untrusted peer cannot choose its bucket with a forwarded header: a
	// forwarded request it sends is not taken as secure at all. Without one
	// it is its own address.
	assertErrorBody(t, f.send(t, "POST", "/v1/join/guest", "", `{}`, "192.0.2.50:1", "198.51.100.8"), http.StatusForbidden, schema.ErrorCodeHTTPSRequired)
	mustJoined(t, f.send(t, "POST", "/v1/join/guest", "", `{}`, "192.0.2.50:1"))
	mustJoined(t, via("198.51.100.8"))
	got := decodeBody[schema.JoinSettingsResponseV1](t, f.send(t, "GET", "/v1/join/settings", f.rootTok, "", ""))
	if got.LiveGuests != 4 || slices.Contains(got.Warnings, schema.JoinWarningOneAddress) {
		t.Errorf("settings = %+v", got)
	}

	// Concurrent joins from one address take its share and no more.
	g := newInviteFixture(t, Config{})
	g.mustPutJoin(t, openTo("red"))
	codes := make([]int, 10)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = g.joinGuest(t, "", "198.51.100.99:1").Code
		}()
	}
	wg.Wait()
	made := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			made++
		} else if c != http.StatusTooManyRequests {
			t.Errorf("a join answered %d", c)
		}
	}
	if made != schema.DefaultMaxGuestsPerAddress {
		t.Errorf("one address started %d visits, want %d", made, schema.DefaultMaxGuestsPerAddress)
	}
	got = decodeBody[schema.JoinSettingsResponseV1](t, g.send(t, "GET", "/v1/join/settings", g.rootTok, "", ""))
	if !slices.Contains(got.Warnings, schema.JoinWarningOneAddress) || !slices.Contains(got.Warnings, schema.JoinWarningTrustedProxyUnset) {
		t.Errorf("warnings with every visit from one address and no proxy = %v", got.Warnings)
	}
}

// The open channel set races guest joins and mode changes: whatever wins,
// no visit starts with nowhere to go, and each visit's channels were open.
func TestOpenChannelsRaceJoins(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.mustPutJoin(t, openTo("red"))
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{MaxGuests: int64Ptr(0), MaxGuestsPerAddress: int64Ptr(schema.MaxMaxGuests)})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Joining is off in one state of six, so most joins race a channel
		// change and some race the switch.
		sets := []schema.PutJoinSettingsRequestV1{
			{OpenChannels: &[]string{"blue"}},
			{OpenChannels: &[]string{"red", "blue"}},
			{OpenChannels: &[]string{}}, // refused while on: the last channel stays
			{OpenChannels: &[]string{"red"}},
			{OpenJoining: modePtr(schema.OpenJoiningOff), OpenChannels: &[]string{}},
			openTo("red"),
		}
		for i := 0; ctx.Err() == nil; i++ {
			rec := f.putJoin(t, sets[i%len(sets)])
			if rec.Code != http.StatusOK && rec.Code != http.StatusConflict {
				t.Errorf("settings change = %d %s", rec.Code, rec.Body)
				return
			}
		}
	}()
	var joined []schema.JoinGuestResponseV1
	var mu sync.Mutex
	var joiners sync.WaitGroup
	for w := range 4 {
		joiners.Add(1)
		go func() {
			defer joiners.Done()
			for i := range 15 {
				rec := f.joinGuest(t, "", fmt.Sprintf("198.51.%d.%d:1", w, i+1))
				switch rec.Code {
				case http.StatusCreated:
					mu.Lock()
					joined = append(joined, decodeBody[schema.JoinGuestResponseV1](t, rec))
					mu.Unlock()
				case http.StatusForbidden:
				default:
					t.Errorf("join = %d %s", rec.Code, rec.Body)
				}
			}
		}()
	}
	joiners.Wait()
	cancel()
	wg.Wait()
	if len(joined) == 0 {
		t.Fatal("no join won a race; the test proved nothing")
	}
	for _, g := range joined {
		members, err := f.srv.store.ListChannelsForPrincipal(context.Background(), g.Principal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Channels) == 0 || len(members) != len(g.Channels) {
			t.Errorf("visit %d answered channels %+v and holds %v", g.Principal.ID, g.Channels, members)
		}
	}
}

// A visit from the bare address joins the open channels only, with a fresh
// history bound and no right to decide; it reads what was posted after it
// arrived and nothing before. Joining starts no voice: nothing is asked of
// LiveKit, and without join_voice the guest has none.
func TestJoinGuestScopeAndHistory(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	if rec := f.do(t, "POST", "/v2/channels/ops/messages", f.tokens["ann"], `{"body":"before-the-visit"}`); rec.Code != http.StatusCreated {
		t.Fatalf("post = %d %s", rec.Code, rec.Body)
	}
	f.mustPutJoin(t, openTo("ops"))
	calls := len(f.lk.calls())
	g := mustJoined(t, presenceSend(t, f, "POST", "/v1/join/guest", "", `{"label":"Pat"}`))
	// presenceSend logs the response in the call log; nothing else may be.
	for _, c := range f.lk.calls()[calls:] {
		if c.method != "http-response" {
			t.Errorf("joining asked LiveKit for %s", c.method)
		}
	}
	if g.Guest.State != schema.GuestStateActive || g.Guest.Origin != schema.GuestOriginAddress || g.Guest.Label != "Pat" ||
		len(g.Channels) != 1 || g.Channels[0].Name != "ops" {
		t.Errorf("joined = %+v", g)
	}
	if p, err := f.srv.store.PrincipalByID(context.Background(), g.Principal.ID); err != nil || p.VoiceAllowed || p.InviteID != 0 {
		t.Errorf("the guest's principal = %+v, %v; want no voice and no invite", p, err)
	}
	if d := g.Guest.ExpiresAt.Time().Sub(g.Guest.StartedAt.Time()); d != schema.DefaultGuestAbsoluteSeconds*time.Second {
		t.Errorf("visit lasts %v", d)
	}
	starts := f.auditOf(t, store.AuditGuestStarted)
	if len(starts) != 1 || starts[0].Actor != "system" || starts[0].Detail != "origin=address" {
		t.Errorf("guest_started = %+v", starts)
	}
	if rec := f.do(t, "POST", "/v2/channels/ops/messages", f.tokens["ann"], `{"body":"during-the-visit"}`); rec.Code != http.StatusCreated {
		t.Fatalf("post = %d", rec.Code)
	}
	rec := f.do(t, "GET", "/v2/channels/ops/messages", g.Token, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "before-the-visit") || !strings.Contains(rec.Body.String(), "during-the-visit") {
		t.Errorf("the guest reads %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.do(t, "GET", "/v2/channels/ops2/messages", g.Token, ""), http.StatusNotFound, "channel_not_found")
	if can, err := f.srv.store.CanDecideApprovals(context.Background(), f.ops.ID, g.Principal.ID); err != nil || can {
		t.Errorf("the guest can decide: %v, %v", can, err)
	}
	// Without join_voice, no voice session.
	assertErrorBody(t, f.do(t, "POST", "/v1/channels/ops/voice/session", g.Token, ""), http.StatusForbidden, "voice_not_allowed")
	who := decodeBody[schema.WhoAmIResponseV1](t, f.do(t, "GET", "/v1/whoami", g.Token, ""))
	if who.IdentityClass != schema.IdentityGuest || who.Guest == nil || who.Guest.Handle != g.Guest.Handle {
		t.Errorf("whoami = %+v", who)
	}
}

// End all guest visits: the dry run lists the active visits and changes
// nothing; the confirmed run ends them, and only them, in one go: each
// guest's sockets close within the bound and it leaves voice, a converted
// member and the members keep everything, and the audit records each end and
// the whole.
func TestEndAllGuestVisits(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.srv.credRecheckInterval = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.mustPutJoin(t, openTo("ops"))
	a := mustJoined(t, presenceSend(t, f, "POST", "/v1/join/guest", "", `{}`))
	b := mustJoined(t, presenceSend(t, f, "POST", "/v1/join/guest", "", `{}`))
	c := newGuest(t, f.srv, "", time.Hour, time.Hour, f.ops.ID)
	conv := newGuest(t, f.srv, "", time.Hour, time.Hour, f.ops.ID)
	// A bare-address visit converts while the address offers accounts
	// (issue #711).
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningBoth)})
	converted, err := f.srv.store.ConvertGuest(ctx, "system", store.GuestConversion{PrincipalID: conv.Principal.ID, Name: "kept",
		Record: hashedRecord(t, f.srv, testJoinPassword), CredentialLabel: "member", CredentialLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	guests := []int64{a.Principal.ID, b.Principal.ID, c.Principal.ID}

	rec := f.do(t, "POST", "/v1/guests/end", f.rootTok, `{"dry_run":true}`)
	dry := decodeBody[schema.EndGuestsResponseV1](t, rec)
	var ids []int64
	for _, g := range dry.Guests {
		ids = append(ids, g.ID)
	}
	if rec.Code != 200 || !dry.DryRun || dry.Count != 3 || dry.Remaining != 0 || !slices.Equal(ids, guests) {
		t.Fatalf("dry run = %d %+v", rec.Code, dry)
	}
	if n, err := f.srv.store.LiveGuests(ctx); err != nil || n != 3 {
		t.Fatalf("after the dry run live = %d, %v", n, err)
	}
	assertErrorBody(t, f.do(t, "POST", "/v1/guests/end", f.rootTok, `{}`), http.StatusBadRequest, "invalid_request")

	room := f.inUse(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: voiceIdentity(a.Principal.ID), joinedMs: 1_000, published: true},
		fakeParticipant{identity: f.identity("ann"), joinedMs: 2_000})
	f.pass(t)
	base := wsTestServer(t, f.srv)
	var logs []*frameLog
	for _, g := range []string{a.Token, b.Token, c.Token} {
		logs = append(logs, recordFrames(dialAs(t, ctx, base, "/v2/ws?channel=ops", g)))
	}
	member := recordFrames(dialAs(t, ctx, base, "/v2/ws?channel=ops", f.tokens["ann"]))
	kept := recordFrames(dialAs(t, ctx, base, "/v2/ws?channel=ops", converted.Token))
	lkBefore := len(f.lk.calls())

	start := time.Now()
	rec = f.do(t, "POST", "/v1/guests/end", f.rootTok, `{"dry_run":false}`)
	done := decodeBody[schema.EndGuestsResponseV1](t, rec)
	if rec.Code != 200 || done.DryRun || done.Count != 3 || done.Remaining != 0 {
		t.Fatalf("end all = %d %+v", rec.Code, done)
	}
	for i, l := range logs {
		select {
		case <-l.done:
		case <-time.After(guestSocketBound + time.Second):
			t.Fatalf("guest %d's socket stayed open %v after the end", i, time.Since(start))
		}
	}
	if took := time.Since(start); took > guestSocketBound {
		t.Errorf("the guests' sockets closed %v after the end, bound %v", took, guestSocketBound)
	}
	if !member.open() || !kept.open() {
		t.Errorf("a member's socket closed: member %v, converted %v", member.err, kept.err)
	}
	var removed bool
	for _, call := range f.lk.calls()[lkBefore:] {
		if call.method == "RemoveParticipant" {
			// Each ended guest is removed by name from its channels' rooms,
			// in case it is in one; nobody else is.
			if !slices.ContainsFunc(guests, func(id int64) bool { return call.identity == voiceIdentity(id) }) {
				t.Errorf("removed %q from voice, want the guests only", call.identity)
			}
			removed = removed || call.identity == voiceIdentity(a.Principal.ID)
		}
	}
	if !removed {
		t.Error("the guest in voice was not removed from the room")
	}
	for _, g := range guests {
		v, err := f.srv.store.GuestVisitOf(ctx, g)
		if err != nil || v.State != schema.GuestStateEnded || v.EndReason != schema.GuestEndOperator {
			t.Errorf("visit %d = %+v, %v", g, v, err)
		}
	}
	for _, tok := range []string{a.Token, b.Token, c.Token} {
		assertErrorBody(t, f.do(t, "GET", "/v1/whoami", tok, ""), http.StatusUnauthorized, "unauthenticated")
	}
	if rec := f.do(t, "GET", "/v1/whoami", converted.Token, ""); rec.Code != http.StatusOK {
		t.Errorf("the converted member after the end: whoami = %d", rec.Code)
	}
	if rows := f.auditOf(t, store.AuditGuestsEnded); len(rows) != 1 || rows[0].Detail != "count=3 remaining=0 origin=any" {
		t.Errorf("guests_ended = %+v", rows)
	}
	var endedRows int
	for _, e := range f.auditOf(t, store.AuditGuestEnded) {
		if e.Detail == "reason=operator" {
			endedRows++
		}
	}
	if endedRows != 3 {
		t.Errorf("guest_ended rows with reason operator = %d, want 3", endedRows)
	}
	// Nothing is left to end.
	again := decodeBody[schema.EndGuestsResponseV1](t, f.do(t, "POST", "/v1/guests/end", f.rootTok, `{"dry_run":false}`))
	if again.Count != 0 || len(again.Guests) != 0 {
		t.Errorf("a second end ended %+v", again)
	}
}

// End all guest visits by origin or invite selects only those.
func TestEndGuestsSelection(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.mustPutJoin(t, openTo("red"))
	req := openLink("red")
	req.JoinAs = schema.InviteJoinGuest
	one, two := f.createInvite(t, f.rootTok, req), f.createInvite(t, f.rootTok, req)
	addr := mustJoined(t, f.joinGuest(t, "", ""))
	viaOne := mustJoined(t, f.redeemGuest(t, one.Code, "", ""))
	viaTwo := mustJoined(t, f.redeemGuest(t, two.Code, "", ""))
	end := func(body string) schema.EndGuestsResponseV1 {
		rec := f.send(t, "POST", "/v1/guests/end", f.rootTok, body, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("end %s = %d %s", body, rec.Code, rec.Body)
		}
		return decodeBody[schema.EndGuestsResponseV1](t, rec)
	}
	refs := func(r schema.EndGuestsResponseV1) []int64 {
		var out []int64
		for _, g := range r.Guests {
			out = append(out, g.ID)
		}
		return out
	}
	if got := refs(end(`{"dry_run":true,"origin":"address"}`)); !slices.Equal(got, []int64{addr.Principal.ID}) {
		t.Errorf("origin address selects %v", got)
	}
	if got := refs(end(`{"dry_run":true,"origin":"invite"}`)); !slices.Equal(got, []int64{viaOne.Principal.ID, viaTwo.Principal.ID}) {
		t.Errorf("origin invite selects %v", got)
	}
	if got := refs(end(fmt.Sprintf(`{"dry_run":false,"invite_id":%d}`, one.Invite.ID))); !slices.Equal(got, []int64{viaOne.Principal.ID}) {
		t.Errorf("invite %d ends %v", one.Invite.ID, got)
	}
	assertErrorBody(t, f.send(t, "POST", "/v1/guests/end", f.rootTok, `{"dry_run":true,"origin":"address","invite_id":3}`, ""),
		http.StatusBadRequest, "invalid_request")
	if n, err := f.srv.store.LiveGuests(context.Background()); err != nil || n != 2 {
		t.Errorf("live after one invite's end = %d, %v", n, err)
	}
	rows := f.auditOf(t, store.AuditGuestsEnded)
	if len(rows) != 1 || rows[0].Detail != fmt.Sprintf("count=1 remaining=0 origin=any invite=%d", one.Invite.ID) {
		t.Errorf("guests_ended = %+v", rows)
	}
}

// An operator ends one visit; a principal that is not a guest is 404 and a
// visit already ended 409. The guest list pages newest first and filters.
func TestEndOneGuestAndListGuests(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.mustPutJoin(t, openTo("red"))
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{MaxGuestsPerAddress: int64Ptr(10)})
	var ids []int64
	for range 5 {
		ids = append(ids, mustJoined(t, f.joinGuest(t, "", "")).Principal.ID)
	}
	assertErrorBody(t, f.send(t, "POST", fmt.Sprintf("/v1/guests/%d/end", f.alice.ID), f.rootTok, "", ""), http.StatusNotFound, schema.ErrorCodeNotAGuest)
	rec := f.send(t, "POST", fmt.Sprintf("/v1/guests/%d/end", ids[0]), f.rootTok, "", "")
	ended := decodeBody[schema.EndGuestResponseV1](t, rec).Guest
	if rec.Code != 200 || ended.State != schema.GuestStateEnded || ended.EndReason != schema.GuestEndOperator || ended.ID != ids[0] {
		t.Fatalf("end = %d %+v", rec.Code, ended)
	}
	assertErrorBody(t, f.send(t, "POST", fmt.Sprintf("/v1/guests/%d/end", ids[0]), f.rootTok, "", ""), http.StatusConflict, schema.ErrorCodeGuestEnded)

	page := func(query string) schema.ListGuestsResponseV1 {
		rec := f.send(t, "GET", "/v1/guests"+query, f.rootTok, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s = %d %s", query, rec.Code, rec.Body)
		}
		return decodeBody[schema.ListGuestsResponseV1](t, rec)
	}
	listed := func(r schema.ListGuestsResponseV1) []int64 {
		var out []int64
		for _, g := range r.Guests {
			out = append(out, g.ID)
		}
		return out
	}
	first := page("?limit=2")
	if got := listed(first); !slices.Equal(got, []int64{ids[4], ids[3]}) || first.NextBefore != ids[3] {
		t.Errorf("first page = %v next %d", got, first.NextBefore)
	}
	second := page(fmt.Sprintf("?limit=2&before=%d", first.NextBefore))
	if got := listed(second); !slices.Equal(got, []int64{ids[2], ids[1]}) || second.NextBefore != 0 {
		t.Errorf("second page = %v next %d", got, second.NextBefore)
	}
	if got := listed(page("?state=ended")); !slices.Equal(got, []int64{ids[0]}) {
		t.Errorf("ended = %v", got)
	}
	if got := page("?origin=invite"); len(got.Guests) != 0 {
		t.Errorf("origin invite = %+v", got)
	}
	for _, q := range []string{"?state=gone", "?origin=window", "?before=0", "?limit=0", fmt.Sprintf("?limit=%d", schema.MaxListGuestsLimit+1)} {
		assertErrorBody(t, f.send(t, "GET", "/v1/guests"+q, f.rootTok, "", ""), http.StatusBadRequest, "invalid_request")
	}
}

// The entry routes share #437's buckets: Join as guest spends the
// per-address redemption allowance an invite redemption does, before the
// body is read.
func TestGuestEntrySharesInviteLimits(t *testing.T) {
	f := newInviteFixture(t, Config{})
	l := looseInviteLimits()
	l.redeemPerAddress = inviteLimit{"redeem_per_address", 3, time.Hour}
	f.srv.setInviteLimits(l)
	f.mustPutJoin(t, openTo("red"))
	inv := f.createInvite(t, f.rootTok, openLink("red"))
	const remote = "198.51.100.40:1"
	mustJoined(t, f.joinGuest(t, "", remote))
	if rec := f.send(t, "POST", "/v2/invites/redeem", "", redeemJSON(t, inv.Code, "newcomer"), remote); rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.redeemGuest(t, inv.Code, "", remote), http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
	for _, rec := range []*httptest.ResponseRecorder{f.joinGuest(t, "", remote), f.redeemGuest(t, inv.Code, "", remote),
		f.send(t, "POST", "/v1/join/guest", "", "not json", remote)} {
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	}
	mustJoined(t, f.joinGuest(t, "", "198.51.100.41:1"))
}

// With authentication off there is nobody to join as (design §7.4): the
// front door offers nothing, password sign-in included, and every other
// route of #710 and #711 is 403 accounts_require_auth.
func TestGuestEntryWithAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	info := f.frontDoor(t)
	if info.Join != (schema.JoinOptionsV1{}) || info.PasswordLogin || info.Voice || info.ServerName != DefaultServerName ||
		info.GuestIdleSeconds != schema.DefaultGuestIdleSeconds || info.GuestAbsoluteSeconds != schema.DefaultGuestAbsoluteSeconds {
		t.Errorf("front door = %+v", info)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/join/guest", `{}`},
		{"POST", "/v1/invites/redeem-guest", `{}`},
		{"GET", "/v1/join/settings", ""},
		{"PUT", "/v1/join/settings", `{"guests_enabled":true}`},
		{"GET", "/v1/guests", ""},
		{"POST", "/v1/guests/1/end", ""},
		{"POST", "/v1/guests/end", `{"dry_run":true}`},
		{"POST", "/v1/join/account", inviteJSON(t, schema.JoinAccountRequestV1{Name: "newcomer", Password: testJoinPassword})},
		{"POST", "/v1/guest/convert", inviteJSON(t, schema.ConvertGuestRequestV1{Name: "newcomer", Password: testJoinPassword})},
		{"POST", "/v1/guests/1/conversion-permit", `{}`},
	} {
		assertErrorBody(t, f.do(t, c.method, c.path, "", c.body), http.StatusForbidden, schema.ErrorCodeAccountsRequireAuth)
	}
}

// The front door carries the configured server name, and no guest may take
// it as a label.
func TestServerNameOnTheFrontDoor(t *testing.T) {
	f := newInviteFixture(t, Config{ServerName: "Harbour Club"})
	if got := f.frontDoor(t).ServerName; got != "Harbour Club" {
		t.Errorf("server name = %q", got)
	}
	f.mustPutJoin(t, openTo("red"))
	assertErrorBody(t, f.joinGuest(t, "harbour club", ""), http.StatusBadRequest, "invalid_request")
	if s := decodeBody[schema.JoinSettingsResponseV1](t, f.send(t, "GET", "/v1/join/settings", f.rootTok, "", "")); s.ServerName != "Harbour Club" {
		t.Errorf("settings server name = %q", s.ServerName)
	}
	for _, bad := range []string{"", "  ", strings.Repeat("x", MaxServerNameLength+1), "a\u202eb"} {
		if ValidateServerName(bad) == nil {
			t.Errorf("ValidateServerName(%q) accepted it", bad)
		}
	}
	if err := ValidateServerName("Harbour Club"); err != nil {
		t.Errorf("ValidateServerName: %v", err)
	}
}

// Both entry routes answer a bearer token, and the guest redemption takes a
// code: over plain HTTP beyond loopback they are 403 https_required before
// anything is read, a rate-limit token included; loopback and HTTPS are
// served. The front door is not a code route and answers either way.
func TestGuestEntryNeedsHTTPS(t *testing.T) {
	f := newInviteFixture(t, Config{})
	l := looseInviteLimits()
	l.redeemPerAddress = inviteLimit{"redeem_per_address", 1, time.Hour}
	f.srv.setInviteLimits(l)
	f.mustPutJoin(t, openTo("red"))
	req := openLink("red")
	req.JoinAs = schema.InviteJoinGuest
	inv := f.createInvite(t, f.rootTok, req)
	plain := func(method, path, body, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, r)
		return rec
	}
	for range 3 {
		assertErrorBody(t, plain("POST", "/v1/join/guest", `{}`, "198.51.100.60:1"), http.StatusForbidden, schema.ErrorCodeHTTPSRequired)
		assertErrorBody(t, plain("POST", "/v1/invites/redeem-guest", inviteJSON(t, schema.RedeemInviteGuestRequestV1{Code: inv.Code}), "198.51.100.61:1"),
			http.StatusForbidden, schema.ErrorCodeHTTPSRequired)
	}
	if n, err := f.srv.store.LiveGuests(context.Background()); err != nil || n != 0 {
		t.Fatalf("plain HTTP started %d visits, %v", n, err)
	}
	// The refusals spent no token: each address still has its one.
	mustJoined(t, f.joinGuest(t, "", "198.51.100.60:1"))
	mustJoined(t, f.redeemGuest(t, inv.Code, "", "198.51.100.61:1"))
	mustJoined(t, plain("POST", "/v1/join/guest", `{}`, "127.0.0.1:1"))
	if rec := plain("GET", "/v1/join", "", "198.51.100.62:1"); rec.Code != http.StatusOK {
		t.Errorf("the front door over plain HTTP = %d %s", rec.Code, rec.Body)
	}
}

// A guest's bearer reaches none of the operator's guest routes: the route
// table refuses it, and nothing changes.
func TestGuestCannotAdministerGuests(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.mustPutJoin(t, openTo("red"))
	g := mustJoined(t, f.joinGuest(t, "", ""))
	other := mustJoined(t, f.joinGuest(t, "", ""))
	before := decodeBody[schema.JoinSettingsResponseV1](t, f.send(t, "GET", "/v1/join/settings", f.rootTok, "", ""))
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/v1/join/settings", `{"guests_enabled":false}`},
		{"GET", "/v1/guests", ""},
		{"POST", "/v1/guests/end", `{"dry_run":false}`},
	} {
		assertErrorBody(t, f.send(t, c.method, c.path, g.Token, c.body, ""), http.StatusForbidden, "forbidden")
	}
	after := decodeBody[schema.JoinSettingsResponseV1](t, f.send(t, "GET", "/v1/join/settings", f.rootTok, "", ""))
	if fmt.Sprint(after.Settings) != fmt.Sprint(before.Settings) || after.LiveGuests != 2 {
		t.Errorf("settings before %+v, after %+v", before, after)
	}
	for _, id := range []int64{g.Principal.ID, other.Principal.ID} {
		if v, err := f.srv.store.GuestVisitOf(context.Background(), id); err != nil || v.State != schema.GuestStateActive {
			t.Errorf("visit %d = %+v, %v", id, v, err)
		}
	}
	if rows := f.auditOf(t, store.AuditJoinSettingsChanged); len(rows) != 1 {
		t.Errorf("join_settings_changed rows = %d, want only the operator's opening", len(rows))
	}
	if rows := f.auditOf(t, store.AuditGuestsEnded); len(rows) != 0 {
		t.Errorf("guests_ended rows = %+v", rows)
	}
}
