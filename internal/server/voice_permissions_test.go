package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #1016 (ADR-014 A4, docs/design/admin-roles.md §4.1, §5, §7.3-§7.5):
// use_voice and share_screen decide voice and screen share, with the
// person's voice brake ANDed in, and a change to either cell for a role has
// its effect before the PUT answers. The voice fixture is the presence
// fixture (voice_presence_fake_test.go): these tests prove the server's
// decisions on one machine against a scripted LiveKit, not real audio, a LAN
// or a real client's reconnect.

// addPerson adds a human to ops, as a member or, with op set, an operator
// (promoted before the credential is made), and returns its token. Its id
// and token are in f.ids and f.tokens.
func (f *presenceFixture) addPerson(t *testing.T, name string, op bool) {
	t.Helper()
	ctx := context.Background()
	p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, name)
	if err != nil {
		t.Fatal(err)
	}
	if op {
		if _, err := f.srv.store.SetRole(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, p.ID, store.RoleOperator, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, tok, err := f.srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.ops.ID, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	f.ids[name], f.tokens[name] = p.ID, tok
}

// brake turns who's voice_allowed off, underneath the store: no route does
// yet (#984).
func (f *presenceFixture) brake(t *testing.T, who string) {
	t.Helper()
	legacyExec(t, f.srv, "UPDATE principals SET voice_allowed = 0 WHERE id = ?", f.ids[who])
}

// setCells turns the given permissions off in the given columns, as the
// owner, through the store.
func (f *presenceFixture) setCells(t *testing.T, perms []schema.Permission, roles ...schema.PermissionRole) {
	t.Helper()
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision}
	for _, p := range perms {
		for _, r := range roles {
			req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: p, Role: r, Granted: false})
		}
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, req, time.Now()); err != nil {
		t.Fatalf("set cells %v: %v", perms, err)
	}
}

// putPermissionsREST sends PUT /v1/permissions as the owner.
func (f *presenceFixture) putPermissionsREST(t *testing.T, req schema.PutPermissionsRequestV1) *httpResult {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "PUT", "/v1/permissions", f.rootTok, string(body))
	return &httpResult{code: rec.Code, body: rec.Body.Bytes()}
}

func (f *presenceFixture) revision(t *testing.T) int64 {
	t.Helper()
	doc, err := f.srv.store.Permissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return doc.Revision
}

var (
	voiceCells = []schema.Permission{schema.PermShareScreen, schema.PermUseVoice}
	shareCell  = []schema.Permission{schema.PermShareScreen}
	bothRoles  = []schema.PermissionRole{schema.PermissionRoleMember, schema.PermissionRoleOperator}
	memberCol  = []schema.PermissionRole{schema.PermissionRoleMember}
)

// TestVoiceRoutesRefuseByPermission is the refusal table of the voice and
// share routes (design §7.3, §7.4, §5): the role's cell, the person's brake,
// both, and the owner and the operator under a brake. Each refusal is 403
// voice_not_allowed carrying the permission, whichever check said no, on
// every route; nothing reaches LiveKit.
func TestVoiceRoutesRefuseByPermission(t *testing.T) {
	const (
		session   = "session"
		transmit  = "transmit"
		companion = "companion"
		share     = "share"
	)
	all := []string{session, transmit, companion, share}
	refuse := func(perm schema.Permission, routes ...string) map[string]schema.Permission {
		m := map[string]schema.Permission{}
		for _, r := range routes {
			m[r] = perm
		}
		return m
	}
	tests := []struct {
		name  string
		who   string
		setup func(t *testing.T, f *presenceFixture)
		// refused is the permission each refused route names; a route
		// absent from it is let through the gate.
		refused map[string]schema.Permission
	}{
		{"role cell off for members", "member-1", func(t *testing.T, f *presenceFixture) {
			f.setCells(t, voiceCells, memberCol...)
		}, refuse(schema.PermUseVoice, all...)},
		{"brake off", "member-1", func(t *testing.T, f *presenceFixture) {
			f.brake(t, "member-1")
		}, refuse(schema.PermUseVoice, all...)},
		{"role cell off and brake off", "member-1", func(t *testing.T, f *presenceFixture) {
			f.setCells(t, voiceCells, memberCol...)
			f.brake(t, "member-1")
		}, refuse(schema.PermUseVoice, all...)},
		{"operator with the brake off", "operator-1", func(t *testing.T, f *presenceFixture) {
			f.brake(t, "operator-1")
		}, refuse(schema.PermUseVoice, all...)},
		{"operator cell off", "operator-1", func(t *testing.T, f *presenceFixture) {
			f.setCells(t, voiceCells, bothRoles...)
		}, refuse(schema.PermUseVoice, all...)},
		{"owner with the brake off", "owner-1", func(t *testing.T, f *presenceFixture) {
			f.brake(t, "owner-1")
		}, nil},
		{"owner with every cell off", "owner-1", func(t *testing.T, f *presenceFixture) {
			f.setCells(t, voiceCells, bothRoles...)
		}, nil},
		{"member with share_screen off only", "member-1", func(t *testing.T, f *presenceFixture) {
			f.setCells(t, shareCell, memberCol...)
		}, refuse(schema.PermShareScreen, share)},
		{"operator with share_screen off only", "operator-1", func(t *testing.T, f *presenceFixture) {
			f.setCells(t, shareCell, bothRoles...)
		}, refuse(schema.PermShareScreen, share)},
		{"operator with the operator cell on while members are off", "operator-1", func(t *testing.T, f *presenceFixture) {
			f.setCells(t, voiceCells, memberCol...)
		}, nil},
		{"defaults", "member-1", func(t *testing.T, f *presenceFixture) {}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			f.addPerson(t, "member-1", false)
			f.addPerson(t, "operator-1", true)
			// The owner is a member of ops too, with its own token.
			f.ids["owner-1"], f.tokens["owner-1"] = f.root.ID, f.rootTok
			if _, err := f.srv.store.AddChannelMember(context.Background(), "system", f.ops.ID, f.root.ID, 0); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, f)
			calls := func() int { return len(f.lk.calls()) }
			before := calls()
			routes := map[string]wireResult{
				session:   f.session(t, tt.who, "ops"),
				transmit:  f.reportBody(t, tt.who, "ops", `{"state":"started"}`),
				companion: f.callREST(t, http.MethodPost, "/v1/channels/ops/voice/companion", f.tokens[tt.who], `{"publish":"screen","subscribe":false}`),
				share:     f.claimShare(t, tt.who, "ops"),
			}
			for name, res := range routes {
				perm, refused := tt.refused[name]
				if !refused {
					if res.status == http.StatusForbidden {
						t.Errorf("%s: refused 403 %s, want let through the gate", name, res.body)
					}
					continue
				}
				if res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
					t.Errorf("%s: %d %s, want 403 voice_not_allowed", name, res.status, res.body)
					continue
				}
				if got := permissionOf(t, []byte(res.body)); got != perm {
					t.Errorf("%s: the refusal names %q, want %q", name, got, perm)
				}
			}
			if len(tt.refused) == len(all) {
				if got := calls() - before; got != 0 {
					t.Errorf("%d requests reached LiveKit for refused callers", got)
				}
			}
			if tt.refused == nil && routes[session].status != http.StatusOK {
				t.Errorf("session for %s = %d %s, want 200", tt.who, routes[session].status, routes[session].body)
			}
		})
	}
}

// TestVoiceOccupancyRefusesByPermission: the occupancy document is a voice
// route too (voice_occupancy.go): a person without use_voice, by the role or
// the brake, is refused with voice_not_allowed and the permission.
func TestVoiceOccupancyRefusesByPermission(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, f *presenceFixture)
		code  int
	}{
		{"role cell off", func(t *testing.T, f *presenceFixture) { f.setCells(t, voiceCells, memberCol...) }, http.StatusForbidden},
		{"brake off", func(t *testing.T, f *presenceFixture) { f.brake(t, "member-1") }, http.StatusForbidden},
		{"defaults", func(t *testing.T, f *presenceFixture) {}, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			f.addPerson(t, "member-1", false)
			tt.setup(t, f)
			res, _ := f.getOccupancy(t, "member-1")
			if res.status != tt.code {
				t.Fatalf("occupancy = %d %s, want %d", res.status, res.body, tt.code)
			}
			if tt.code == http.StatusForbidden {
				if errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed || permissionOf(t, []byte(res.body)) != schema.PermUseVoice {
					t.Errorf("occupancy refusal = %s, want voice_not_allowed naming use_voice", res.body)
				}
			}
		})
	}
}

// A guest's voice (the security review of #1013): permittedTx answers false
// for every guest, so a guest whose visit allowed voice loses it when A4
// moves voice onto it. That only narrows access, and guests are removed for
// now (#1005): the change is stated here and pinned.
func TestVoiceGuestHoldsNoVoice(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	g := newGuest(t, f.srv, "Pat", time.Hour, time.Hour, f.ops.ID)
	res := f.callREST(t, http.MethodPost, "/v1/channels/ops/voice/session", g.Token, "")
	if res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
		t.Fatalf("a guest whose visit allowed voice: %d %s, want 403 voice_not_allowed", res.status, res.body)
	}
	if !g.Principal.VoiceAllowed {
		t.Fatal("the guest's visit did not allow voice, so the test proves nothing")
	}
}

// newVoiceRoomFixture is newShareFixture's room with each of ann's three
// identities connected (person, companion, viewer) beside ann2's, and an
// operator, op1, in the room too.
func newVoiceRoomFixture(t *testing.T) (*presenceFixture, string) {
	t.Helper()
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.addPerson(t, "op1", true)
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	decodeSession(t, f.session(t, "ann2", "ops"))
	decodeSession(t, f.session(t, "op1", "ops"))
	f.lk.setRoom(room, f.connected(room, "ann", "ann2", "op1")...)
	f.at(0)
	f.pass(t)
	return f, room
}

// connected lists who's three identities (p<id>, p<id>.screen, p<id>.view)
// for ann, and the person alone for the others.
func (f *presenceFixture) connected(_ string, who ...string) []fakeParticipant {
	var out []fakeParticipant
	for i, w := range who {
		out = append(out, fakeParticipant{identity: f.identity(w), joinedMs: int64(1_000 * (i + 1)), published: true, muted: true})
		if w == "ann" {
			out = append(out,
				fakeParticipant{identity: f.identity(w) + voiceSuffixScreen, joinedMs: 1_500},
				fakeParticipant{identity: f.identity(w) + voiceSuffixView, joinedMs: 1_600})
		}
	}
	return out
}

// TestPutUseVoiceOffRemovesThoseAffectedAtOnce: the PUT turns use_voice off
// for members; when it answers, every member in voice has left (presence,
// LiveKit, the audit), with no pass run, the operator is untouched, and no
// room was rotated (design §7.5).
func TestPutUseVoiceOffRemovesThoseAffectedAtOnce(t *testing.T) {
	f, room := newVoiceRoomFixture(t)
	f.mustClaim(t, "ann")
	rooms := f.lk.count("DeleteRoom")
	stored := f.room(t, f.ops)

	res := f.putPermissionsREST(t, schema.PutPermissionsRequestV1{Revision: f.revision(t), Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
		{Permission: schema.PermUseVoice, Role: schema.PermissionRoleMember, Granted: false},
	}})
	if res.code != http.StatusOK {
		t.Fatalf("the PUT = %d %s", res.code, res.body)
	}
	// Read at the moment the PUT has answered: no pass has run since.
	for _, who := range []string{"ann", "ann2"} {
		id := f.identity(who)
		for _, name := range []string{id, id + voiceSuffixScreen, id + voiceSuffixView} {
			if f.lk.in(room, name) {
				t.Errorf("%s is still connected when the PUT answered", name)
			}
		}
	}
	if !f.lk.in(room, f.identity("op1")) {
		t.Error("the operator, whose cell is still on, was removed")
	}
	if got := f.who(t, f.ops); strings.Contains(got, "ann") {
		t.Errorf("presence still shows %q after the PUT answered", got)
	}
	if holder := f.shareState(room); holder != 0 {
		t.Errorf("the poller still holds a share for %d", holder)
	}
	removed := f.audits(t, store.AuditVoiceParticipantRemoved)
	if len(removed) == 0 {
		t.Fatal("no voice_participant_removed row")
	}
	for _, e := range removed {
		if !strings.Contains(e.Detail, "reason="+voiceReasonVoiceOff) {
			t.Errorf("removal row %q does not say reason=%s", e.Detail, voiceReasonVoiceOff)
		}
	}
	if left := len(f.audits(t, store.AuditVoiceLeft)); left != 2 {
		t.Errorf("%d voice_left rows, want 2 (ann and ann2)", left)
	}
	// Rooms are not rotated: same stored room, nothing deleted.
	if got := f.room(t, f.ops); got != stored {
		t.Errorf("the channel's room is now %q, was %q: it was rotated", got, stored)
	}
	if got := f.lk.count("DeleteRoom") - rooms; got != 0 {
		t.Errorf("%d DeleteRoom calls, want none", got)
	}
}

// TestJoinTokenMintedBeforeARoleChange: a token signed before use_voice went
// off for a role still connects (LiveKit knows nothing of the change), and
// the next pass removes p<id>, p<id>.screen and p<id>.view with the voice
// reason: the bound is one pass plus one token lifetime (design §7.5).
func TestJoinTokenMintedBeforeARoleChange(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room // the token
	res := f.putPermissionsREST(t, schema.PutPermissionsRequestV1{Revision: f.revision(t), Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
		{Permission: schema.PermUseVoice, Role: schema.PermissionRoleMember, Granted: false},
	}})
	if res.code != http.StatusOK {
		t.Fatalf("the PUT = %d %s", res.code, res.body)
	}
	// The old token connects after the change.
	f.lk.setRoom(room, f.connected(room, "ann")...)
	f.at(0)
	f.pass(t)
	id := f.identity("ann")
	for _, name := range []string{id, id + voiceSuffixScreen, id + voiceSuffixView} {
		if f.lk.in(room, name) {
			t.Errorf("%s is still connected after the pass", name)
		}
	}
	var reasons []string
	for _, e := range f.audits(t, store.AuditVoiceParticipantRemoved) {
		if strings.Contains(e.Detail, "reason="+voiceReasonVoiceOff) {
			reasons = append(reasons, e.Detail)
		}
	}
	if len(reasons) != 3 {
		t.Errorf("%d removal rows with reason=%s, want 3 (person, screen, view): %q", len(reasons), voiceReasonVoiceOff, reasons)
	}
	// And a new session is refused.
	if res := f.session(t, "ann", "ops"); res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
		t.Errorf("a new session = %d %s, want 403 voice_not_allowed", res.status, res.body)
	}
}

// TestPassEndsAShareWhoseCellWentOff: share_screen is read on every pass, as
// use_voice is. The document changes under the poller with no hook run (a
// restart, a missed call): the next pass ends the share through the stop
// path, audited, and takes the screen back.
func TestPassEndsAShareWhoseCellWentOff(t *testing.T) {
	f, room := newShareFixture(t)
	f.mustClaim(t, "ann")
	f.setCells(t, shareCell, bothRoles...)
	if !f.granted(t, room, "ann") {
		t.Fatal("setup: the share was ended by the write alone")
	}
	f.pass(t)
	if f.granted(t, room, "ann") {
		t.Error("the screen is still granted after the pass")
	}
	if holder := f.shareState(room); holder != 0 {
		t.Errorf("the poller still holds a share for %d", holder)
	}
	want := []string{"ann started audio=true", "ann stopped principal=ann reason=access_lost"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
	// Voice itself is untouched: ann is still in the room.
	if !f.lk.in(room, f.identity("ann")) {
		t.Error("ann lost voice with share_screen")
	}
}

// TestPutShareScreenOffEndsAShareAudited: through the real route, the live
// share of the affected role stops before the PUT answers and a share stop
// is audited; a share by someone whose role keeps the cell stays.
func TestPutShareScreenOffEndsAShareAudited(t *testing.T) {
	f, room := newShareFixture(t)
	f.mustClaim(t, "ann")
	res := f.putPermissionsREST(t, schema.PutPermissionsRequestV1{Revision: f.revision(t), Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
	}})
	if res.code != http.StatusOK {
		t.Fatalf("the PUT = %d %s", res.code, res.body)
	}
	if f.granted(t, room, "ann") || f.shareState(room) != 0 {
		t.Error("the share is still live when the PUT answered")
	}
	want := []string{"ann started audio=true", "ann stopped principal=ann reason=access_lost"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
}

// TestEnforcedPermissionsRefuseAnUnenforcedCell pins the safety net itself
// now that every permission is enforced: a cell whose permission is not in
// enforcedPermissions is refused, 409 permissions_conflict, and changes
// nothing.
func TestEnforcedPermissionsRefuseAnUnenforcedCell(t *testing.T) {
	saved := enforcedPermissions
	enforcedPermissions = slices.DeleteFunc(slices.Clone(saved), func(p schema.Permission) bool { return p == schema.PermUseVoice })
	t.Cleanup(func() { enforcedPermissions = saved })
	f := newRestrictedFixture(t, Config{})
	before := revisionOf(t, f)
	res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: before, Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
		{Permission: schema.PermUseVoice, Role: schema.PermissionRoleMember, Granted: false},
	}})
	if res.code != http.StatusConflict || res.errorCode(t) != schema.ErrorCodePermissionsConflict || !strings.Contains(string(res.body), "use_voice") {
		t.Fatalf("a cell the server does not enforce = %d %s, want 409 permissions_conflict naming use_voice", res.code, res.body)
	}
	if revisionOf(t, f) != before {
		t.Error("a refused PUT changed the document")
	}
}

// TestRoleChangeWithholdingUseVoiceRemovesAtOnce (#1131): a role change that
// takes the target's use_voice removes them from voice before the PUT
// answers, as PUT /v1/permissions does; one that keeps it leaves them be.
func TestRoleChangeWithholdingUseVoiceRemovesAtOnce(t *testing.T) {
	tests := []struct {
		name       string
		memberOff  bool // member.use_voice is off before the demotion
		wantRemain bool
	}{
		{"use_voice withheld from member removes", true, false},
		{"use_voice kept leaves them in voice", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			f.addPerson(t, "op2", true)
			if tc.memberOff {
				f.setCells(t, voiceCells, schema.PermissionRoleMember)
			}
			f.srv.credRecheckInterval = time.Hour
			room := decodeSession(t, f.session(t, "op2", "ops")).Rooms[0].Room
			id := f.identity("op2")
			names := []string{id, id + voiceSuffixScreen, id + voiceSuffixView}
			f.lk.setRoom(room,
				fakeParticipant{identity: id, joinedMs: 1_000, published: true, muted: true},
				fakeParticipant{identity: id + voiceSuffixScreen, joinedMs: 1_500},
				fakeParticipant{identity: id + voiceSuffixView, joinedMs: 1_600})
			f.at(0)
			f.pass(t)
			for _, n := range names {
				if !f.lk.in(room, n) {
					t.Fatalf("%s is not in voice before the demotion", n)
				}
			}
			base := wsTestServer(t, f.srv)
			sock, _, err := dialVoice(t, base, "ops", f.tokens["op2"])
			if err != nil {
				t.Fatal(err)
			}
			readPresence(t, sock)

			rec := f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/role", f.ids["op2"]), f.rootTok, `{"role":"member"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("the role PUT = %d %s", rec.Code, rec.Body.Bytes())
			}
			// Straight after the answer: no pass has run since.
			for _, n := range names {
				if got := f.lk.in(room, n); got != tc.wantRemain {
					t.Errorf("%s in voice = %v after the PUT, want %v", n, got, tc.wantRemain)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _, err = sock.Read(ctx)
			closed := err != nil && ctx.Err() == nil
			if closed == tc.wantRemain {
				t.Errorf("voice socket closed = %v (err %v), want %v", closed, err, !tc.wantRemain)
			}
			removed := 0
			for _, e := range f.audits(t, store.AuditVoiceParticipantRemoved) {
				if strings.Contains(e.Detail, "reason="+voiceReasonVoiceOff) {
					removed++
				}
			}
			if !tc.wantRemain && removed == 0 {
				t.Error("no voice_participant_removed row for the demotion")
			}
		})
	}
}
