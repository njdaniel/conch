package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Open sign-up and the bare address under the community model (issue #987,
// ADR-013, community-model design §3.4 Option B): the joining settings stay
// on v1 with a documented mapping, open sign-up makes the member a member
// link makes, and the bare address never admits a guest.

// Every Option B write mapping, as the owner: what is refused (and then
// changes nothing), what is accepted, and what the settings then read.
func TestJoinSettingsOptionBMapping(t *testing.T) {
	f := newInviteFixture(t, Config{})
	f.makePublic(t, "blue") // red stays private
	on, off := true, false
	conflict := func(code string) func(*testing.T, *httptest.ResponseRecorder) {
		return func(t *testing.T, rec *httptest.ResponseRecorder) {
			t.Helper()
			assertErrorBody(t, rec, http.StatusConflict, code)
		}
	}
	jpc, csc := conflict(schema.ErrorCodeJoinPolicyConflict), conflict(schema.ErrorCodeChannelSettingsConflict)
	refusals := []struct {
		name  string
		req   schema.PutJoinSettingsRequestV1
		check func(*testing.T, *httptest.ResponseRecorder)
	}{
		{"open sign-up with no default channel", schema.PutJoinSettingsRequestV1{OpenSignup: &on}, jpc},
		{"open_joining account with no default channel", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningAccount)}, jpc},
		{"open_joining guest", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningGuest), OpenChannels: &[]string{"blue"}}, jpc},
		{"open_joining both", schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningBoth), OpenChannels: &[]string{"blue"}}, jpc},
		{"join_voice false", schema.PutJoinSettingsRequestV1{JoinVoice: &off}, jpc},
		{"join_voice false beside open sign-up", schema.PutJoinSettingsRequestV1{JoinVoice: &off, OpenSignup: &on, OpenChannels: &[]string{"blue"}}, jpc},
		{"guests_enabled true", schema.PutJoinSettingsRequestV1{GuestsEnabled: &on}, jpc},
		{"guests_enabled true beside open sign-up", schema.PutJoinSettingsRequestV1{GuestsEnabled: &on, OpenSignup: &on, OpenChannels: &[]string{"blue"}}, jpc},
		{"guests_enabled true beside a bound", schema.PutJoinSettingsRequestV1{GuestsEnabled: &on, MaxGuests: int64Ptr(3)}, jpc},
		{"open_signup and open_joining disagree", schema.PutJoinSettingsRequestV1{OpenSignup: &on, OpenJoining: modePtr(schema.OpenJoiningOff), OpenChannels: &[]string{"blue"}}, jpc},
		{"a private channel", schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"red"}}, csc},
		{"a private channel beside a public one", schema.PutJoinSettingsRequestV1{OpenSignup: &on, OpenChannels: &[]string{"blue", "red"}}, csc},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, f.putJoin(t, tt.req))
		})
	}
	if n := len(f.auditOf(t, store.AuditJoinSettingsChanged)) + len(f.auditOf(t, store.AuditOpenChannelAdded)); n != 0 {
		t.Errorf("the refusals wrote %d settings rows", n)
	}
	if red, err := f.srv.store.ChannelByID(context.Background(), f.red.ID); err != nil || red.Visibility != schema.ChannelPrivate || red.IsDefault {
		t.Errorf("red after the refusals = %+v, %v; want private and not a default", red, err)
	}

	// The read mapping before anything is set: off, voice on, no default
	// channel, and the warning that says so.
	got := decodeBody[schema.JoinSettingsResponseV1](t, f.send(t, "GET", "/v1/join/settings", f.rootTok, "", ""))
	if s := got.Settings; s.OpenJoining != schema.OpenJoiningOff || !s.JoinVoice || s.GuestsEnabled || s.OpenSignup == nil || *s.OpenSignup ||
		s.MemberLinkDefaultExpirySeconds == nil || len(s.OpenChannels) != 0 || len(got.DefaultChannels) != 0 ||
		!slices.Contains(got.Warnings, schema.JoinWarningNoDefaultChannel) {
		t.Fatalf("settings before any change = %+v", got)
	}

	// Accepted: open_signup with a public default channel; open_joining reads
	// account, open_channels and default_channels name it, the warning goes.
	got = f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenSignup: &on, OpenChannels: &[]string{"blue"}, JoinVoice: &on})
	if s := got.Settings; s.OpenJoining != schema.OpenJoiningAccount || s.OpenSignup == nil || !*s.OpenSignup || !s.JoinVoice ||
		!slices.Equal(s.OpenChannels, []string{"blue"}) || len(got.DefaultChannels) != 1 || got.DefaultChannels[0].ID != f.blue.ID ||
		slices.Contains(got.Warnings, schema.JoinWarningNoDefaultChannel) {
		t.Fatalf("settings after open sign-up = %+v", got)
	}
	// open_joining off is the same as open_signup false.
	got = f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningOff)})
	if s := got.Settings; s.OpenJoining != schema.OpenJoiningOff || *s.OpenSignup {
		t.Fatalf("settings after open_joining off = %+v", s)
	}
	// and account the same as true.
	got = f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenJoining: modePtr(schema.OpenJoiningAccount)})
	if s := got.Settings; s.OpenJoining != schema.OpenJoiningAccount || !*s.OpenSignup {
		t.Fatalf("settings after open_joining account = %+v", s)
	}
	// open_channels marks exactly those channels default and unmarks the rest.
	f.makePublic(t, "red")
	got = f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"red"}})
	if !slices.Equal(got.Settings.OpenChannels, []string{"red"}) {
		t.Fatalf("open channels = %v, want red", got.Settings.OpenChannels)
	}
	if blue, err := f.srv.store.ChannelByID(context.Background(), f.blue.ID); err != nil || blue.IsDefault || blue.Visibility != schema.ChannelPublic {
		t.Errorf("blue after unlisting = %+v, %v; want public and not a default", blue, err)
	}
}

// Who may read and change the joining settings: the owner changes them; an
// operator without change_server_settings, a member and a guest may not.
// (Recent authentication is required of every writer: the route is
// reauthAlways in operator_auth_test.go's route table, which
// TestRecentAuthClass and TestRecentAuthGate hold every route to.)
func TestJoinSettingsWriters(t *testing.T) {
	f := newAdminFixture(t)
	body := `{"join_voice":true}`
	if rec := f.do(t, "PUT", "/v1/join/settings", f.own2Tok, body); rec.Code != http.StatusOK {
		t.Errorf("the owner's change = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.do(t, "PUT", "/v1/join/settings", f.op2Tok, body), http.StatusForbidden, schema.ErrorCodePermissionDenied)
	for name, tok := range map[string]string{"member": f.aliceTok, "guest": f.guestTok} {
		if rec := f.do(t, "PUT", "/v1/join/settings", tok, body); rec.Code != http.StatusForbidden {
			t.Errorf("a %s's change = %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
}

// What open sign-up makes is the member a member link makes, field by field,
// apart from who invited it. It needs a default channel, a pause pauses it,
// and unmarking the last default channel turns it off, audited.
func TestOpenSignupMakesAMemberLinkMember(t *testing.T) {
	ctx := context.Background()
	f := newInviteFixture(t, Config{})
	if _, err := f.srv.store.InsertMessage(ctx, f.red.ID, f.alice.ID, "said before anyone joined"); err != nil {
		t.Fatal(err)
	}
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenSignup: boolPtr(true), OpenChannels: &[]string{"red"}})
	signup := mustAccount(t, f.joinAccount(t, "signup", ""))
	link := f.createMemberLink(t, f.rootTok, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	rec := f.redeem(t, link.Code, "linked")
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem the member link = %d %s", rec.Code, rec.Body)
	}
	linked := decodeBody[schema.RedeemInviteResponseV1](t, rec)

	type row struct {
		role, kind               string
		owner, voice, mayInvite  int
		disabled, hidden, guests int
		invitedBy                sql.NullInt64
		memberships              string
		credRenews, credLifetime int64
	}
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	read := func(id int64) row {
		t.Helper()
		var r row
		err := db.QueryRowContext(ctx, `
			SELECT role, kind, owner, voice_allowed, may_invite, disabled_at IS NOT NULL, hidden_at IS NOT NULL,
			  (SELECT COUNT(*) FROM guest_visits WHERE principal_id = p.id), invited_by,
			  (SELECT COALESCE(group_concat(channel_id || ':' || can_decide || ':' || COALESCE(history_after, -1), ','), '')
			     FROM (SELECT * FROM channel_members WHERE principal_id = p.id ORDER BY channel_id)),
			  (SELECT renew_ms FROM credentials WHERE principal_id = p.id),
			  (SELECT expires_at - created_at FROM credentials WHERE principal_id = p.id)
			FROM principals p WHERE id = ?`, id).Scan(&r.role, &r.kind, &r.owner, &r.voice, &r.mayInvite, &r.disabled, &r.hidden, &r.guests,
			&r.invitedBy, &r.memberships, &r.credRenews, &r.credLifetime)
		if err != nil {
			t.Fatalf("read principal %d: %v", id, err)
		}
		return r
	}
	a, b := read(signup.Principal.ID), read(linked.Principal.ID)
	// The one difference by design: a member link's joiner names its maker.
	if a.invitedBy.Valid || !b.invitedBy.Valid {
		t.Errorf("invited_by: sign-up %v, link %v; want none and the maker", a.invitedBy, b.invitedBy)
	}
	// Both are the design's member, compared on their real flags (issue
	// #986): voice and the may_invite brake on, for each.
	if a.mayInvite != 1 || a.voice != 1 || b.mayInvite != 1 || b.voice != 1 {
		t.Errorf("may_invite, voice_allowed: sign-up %d, %d; link %d, %d; want all 1", a.mayInvite, a.voice, b.mayInvite, b.voice)
	}
	a.invitedBy, b.invitedBy = sql.NullInt64{}, sql.NullInt64{}
	if a != b {
		t.Errorf("the sign-up member differs from the member link's:\n sign-up %+v\n link    %+v", a, b)
	}
	want := fmt.Sprintf("%d:0:-1", f.red.ID)
	const lifetime = schema.DefaultInviteCredentialLifetimeSeconds * 1000
	if a.role != "member" || a.voice != 1 || a.memberships != want || a.credRenews != lifetime || a.credLifetime != lifetime {
		t.Errorf("sign-up member = %+v; want a member, voice on, %s (no right to decide, no bound), a 90-day renewing credential", a, want)
	}
	msgs := decodeBody[schema.ListMessagesResponseV2](t, f.send(t, "GET", "/v2/channels/red/messages", signup.Token, "", ""))
	if len(msgs.Messages) != 1 {
		t.Errorf("the sign-up member reads %d earlier messages, want 1 (no history bound)", len(msgs.Messages))
	}

	// Paused, it makes nobody, and the front door says paused.
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{JoiningPaused: boolPtr(true)})
	if info := f.frontDoor(t); info.Join != (schema.JoinOptionsV1{Paused: true}) {
		t.Errorf("front door while paused = %+v", info.Join)
	}
	assertErrorBody(t, f.joinAccount(t, "while-paused", ""), http.StatusForbidden, schema.ErrorCodeJoiningPaused)
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{JoiningPaused: boolPtr(false)})

	// The last default channel unmarked turns open sign-up off, audited, and
	// the address then makes nobody.
	if rec := f.send(t, "PUT", fmt.Sprintf("/v1/channels/%d/settings", f.red.ID), f.rootTok, `{"default":false}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("unmark red = %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[schema.JoinSettingsResponseV1](t, f.send(t, "GET", "/v1/join/settings", f.rootTok, "", ""))
	if got.Settings.OpenSignup == nil || *got.Settings.OpenSignup || got.Settings.OpenJoining != schema.OpenJoiningOff ||
		!slices.Contains(got.Warnings, schema.JoinWarningNoDefaultChannel) {
		t.Errorf("after the last default channel went = %+v", got)
	}
	turnedOff := false
	for _, e := range f.auditOf(t, store.AuditJoinSettingsChanged) {
		turnedOff = turnedOff || strings.Contains(e.Detail, "last_default_unmarked")
	}
	if !turnedOff {
		t.Error("turning open sign-up off with the last default channel was not audited")
	}
	assertErrorBody(t, f.joinAccount(t, "too-late", ""), http.StatusForbidden, schema.ErrorCodeJoiningClosed)
}

// POST /v1/join/guest is 403 guests_disabled always: with open sign-up on
// and off, and with guests_enabled and a guest mode seeded true underneath
// the store. GET /v1/join never offers a guest and always offers voice, and
// names no channel, private or not.
func TestBareAddressNeverAdmitsAGuest(t *testing.T) {
	for _, tt := range []struct {
		name string
		rows map[string]string
	}{
		{"open sign-up off", map[string]string{"open_joining": "off"}},
		{"open sign-up on", map[string]string{"open_joining": "account"}},
		{"guests seeded on, address both", map[string]string{"guests_enabled": "true", "open_joining": "both"}},
		{"guests seeded on, address guest", map[string]string{"guests_enabled": "true", "open_joining": "guest"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			legacySettingRows(t, f.srv, tt.rows, f.blue)
			before := len(f.audit(t))
			rec := f.joinGuest(t, "visitor", "")
			assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeGuestsDisabled)
			if n, err := f.srv.store.LiveGuests(context.Background()); err != nil || n != 0 {
				t.Errorf("%d live visits, %v", n, err)
			}
			if n := len(f.audit(t)); n != before {
				t.Errorf("the refusal wrote %d audit rows", n-before)
			}
			raw := f.send(t, "GET", "/v1/join", "", "", "")
			info := decodeBody[schema.JoinInfoV1](t, raw)
			if info.Join.Guest || info.Join.GuestsFull || !info.Voice {
				t.Errorf("front door = %+v; want no guest and voice", info)
			}
			for _, name := range []string{"red", "blue"} {
				if strings.Contains(raw.Body.String(), name) {
					t.Errorf("the front door names channel %q: %s", name, raw.Body)
				}
			}
		})
	}
}

// The approval chain (CLAUDE.md rule 3) with a member open sign-up made:
// request, notify, resolve, audit, through the REST API. The member sees the
// approval in the default channel, read-only, and is refused when it tries,
// audited; the existing approver decides, the quorum is unchanged, and the
// approval resolves with its audit rows.
func TestOpenSignupMemberCannotDecideFullChain(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var notified []string
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		notified = append(notified, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ntfy.Close()
	f := newInviteFixture(t, Config{Ntfy: approvals.NtfyConfig{
		Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second}})
	defer f.srv.approvals.Close()

	// red is the default channel; alice, added by an operator, holds the
	// right to decide; open sign-up adds a member to red.
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{OpenSignup: boolPtr(true), OpenChannels: &[]string{"red"}})
	joiner := mustAccount(t, f.joinAccount(t, "signup", ""))
	if ok, err := f.srv.store.CanDecideApprovals(ctx, f.red.ID, joiner.Principal.ID); err != nil || ok {
		t.Fatalf("the sign-up member decides approvals: %v, %v", ok, err)
	}

	// Request.
	rec := f.send(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(f.red.ID, 0), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	created := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval
	id, quorum := created.ID, created.Quorum
	path := fmt.Sprintf("/v1/approvals/%d/decisions", id)

	// Notify.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(notified)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the approval was never notified")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The sign-up member sees it in the default channel, read-only.
	list := decodeBody[schema.ListApprovalsResponseV1](t, f.send(t, "GET", "/v1/approvals", joiner.Token, "", ""))
	if len(list.Approvals) != 1 || list.Approvals[0].ID != id || len(list.ReadOnly) != 1 || list.ReadOnly[0] != id {
		t.Fatalf("the sign-up member's list = %+v, want the approval, read-only", list)
	}
	// Refused, audited, and the approval stays pending.
	assertErrorBody(t, f.send(t, "POST", path, joiner.Token, decisionBody(0), ""), http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	if a, err := f.srv.store.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending || a.Quorum != quorum {
		t.Fatalf("after the refusal approval = %+v, %v; want pending with quorum %d", a, err, quorum)
	}
	joinerActor := fmt.Sprintf("principal:%d", joiner.Principal.ID)
	refused := false
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && e.Actor == joinerActor && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted) {
			refused = true
		}
		if e.Action == store.AuditDecisionCast && e.Actor == joinerActor {
			t.Error("the sign-up member's refused decision was cast")
		}
	}
	if !refused {
		t.Error("the sign-up member's refusal was not audited")
	}

	// Resolve: alice decides, and one decision still meets the quorum.
	rec = f.send(t, "POST", path, f.aliceTok, decisionBody(0), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}

	// Audit: created, decided by alice, resolved, in that order.
	var chain []string
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
			if e.Subject == fmt.Sprintf("approval:%d", id) {
				chain = append(chain, e.Action+" "+e.Actor)
			}
		}
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", f.alice.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}
