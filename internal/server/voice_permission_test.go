package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #984 (ADR-013 decision 4, design §2.5, §3.6, §4): PUT
// /v1/principals/{id}/voice-permission switches a person's voice off or on at
// any time, with immediate effect on live media. The route is
// accessPermission + manage_members (ADR-014), re-authentication class,
// owner-protected. These tests prove the server's decisions on one machine
// against a scripted LiveKit, not real media; the revocation interleavings
// (claims in flight) are rows of revocations in voice_share_revoke_test.go.

func voicePermissionPath(id int64) string {
	return fmt.Sprintf("/v1/principals/%d/voice-permission", id)
}

// putVoice sends the switch as the owner and returns the answer.
func (f *presenceFixture) putVoice(t *testing.T, target int64, allowed bool) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPut, voicePermissionPath(target), f.rootTok, fmt.Sprintf(`{"voice_allowed":%t}`, allowed))
}

func decodeVoicePermission(t *testing.T, res wireResult) schema.VoicePermissionResponseV1 {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(res.body))
	dec.DisallowUnknownFields()
	var out schema.VoicePermissionResponseV1
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	return out
}

// voiceStored reads the stored switch.
func (f *presenceFixture) voiceStored(t *testing.T, who string) bool {
	t.Helper()
	p, err := f.srv.store.PrincipalByID(context.Background(), f.ids[who])
	if err != nil {
		t.Fatal(err)
	}
	return p.VoiceAllowed
}

// TestVoicePermissionRoute is the route table: callers by role against
// targets by kind. A caller who may not use the route learns nothing of the
// target (one answer for an existing and a ghost id), and a hidden id is the
// unknown id's 404.
func TestVoicePermissionRoute(t *testing.T) {
	const (
		ok   = http.StatusOK
		forb = http.StatusForbidden
	)
	type want struct {
		status int
		code   string
	}
	// Target names: member (ann2), operator (op2), owner (root), agent
	// (robo), hidden (hid), unknown (ghost).
	targets := []string{"member", "operator", "owner", "agent", "hidden", "unknown"}
	tests := []struct {
		name   string
		caller string
		// guest makes the caller a guest visitor.
		guest bool
		want  map[string]want
	}{
		{"member", "member-1", false, map[string]want{
			"member": {forb, "forbidden"}, "operator": {forb, "forbidden"}, "owner": {forb, "forbidden"},
			"agent": {forb, "forbidden"}, "hidden": {forb, "forbidden"}, "unknown": {forb, "forbidden"},
		}},
		{"guest", "", true, map[string]want{
			"member": {forb, "forbidden"}, "operator": {forb, "forbidden"}, "owner": {forb, "forbidden"},
			"agent": {forb, "forbidden"}, "hidden": {forb, "forbidden"}, "unknown": {forb, "forbidden"},
		}},
		{"agent", "robo", false, map[string]want{
			"member": {forb, "forbidden"}, "operator": {forb, "forbidden"}, "owner": {forb, "forbidden"},
			"agent": {forb, "forbidden"}, "hidden": {forb, "forbidden"}, "unknown": {forb, "forbidden"},
		}},
		{"unauthenticated", "none", false, map[string]want{
			"member": {http.StatusUnauthorized, "unauthenticated"}, "operator": {http.StatusUnauthorized, "unauthenticated"},
			"owner": {http.StatusUnauthorized, "unauthenticated"}, "agent": {http.StatusUnauthorized, "unauthenticated"},
			"hidden": {http.StatusUnauthorized, "unauthenticated"}, "unknown": {http.StatusUnauthorized, "unauthenticated"},
		}},
		{"non-owner operator", "op1", false, map[string]want{
			"member": {ok, ""}, "operator": {ok, ""}, "owner": {forb, "forbidden"},
			"agent": {http.StatusConflict, schema.ErrorCodeNotAPerson}, "hidden": {http.StatusNotFound, "principal_not_found"},
			"unknown": {http.StatusNotFound, "principal_not_found"},
		}},
		{"owner", "root", false, map[string]want{
			"member": {ok, ""}, "operator": {ok, ""}, "owner": {ok, ""},
			"agent": {http.StatusConflict, schema.ErrorCodeNotAPerson}, "hidden": {http.StatusNotFound, "principal_not_found"},
			"unknown": {http.StatusNotFound, "principal_not_found"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			f.addPerson(t, "member-1", false)
			f.addPerson(t, "op1", true)
			f.addPerson(t, "op2", true)
			f.addPerson(t, "hid", false)
			if _, err := f.srv.store.HidePrincipal(context.Background(), "system", f.ids["hid"], store.HideOptions{}); err != nil {
				t.Fatal(err)
			}
			token := f.tokens[tt.caller]
			if tt.guest {
				token = newGuest(t, f.srv, "Pat", time.Hour, time.Hour, f.ops.ID).Token
			}
			if tt.caller == "none" {
				token = ""
			}
			ids := map[string]int64{"member": f.ids["ann2"], "operator": f.ids["op2"], "owner": f.ids["root"],
				"agent": f.ids["robo"], "hidden": f.ids["hid"], "unknown": 999_999}
			bodies := map[string][]byte{}
			for _, target := range targets {
				w := tt.want[target]
				rec := f.do(t, http.MethodPut, voicePermissionPath(ids[target]), token, `{"voice_allowed":false}`)
				if rec.Code != w.status {
					t.Errorf("-> %s: status = %d %s, want %d", target, rec.Code, rec.Body, w.status)
					continue
				}
				if w.code != "" && errCode(t, rec.Body.String()) != w.code {
					t.Errorf("-> %s: code = %s, want %s", target, rec.Body, w.code)
				}
				if w.status == ok {
					got := decodeVoicePermission(t, wireResult{status: rec.Code, body: rec.Body.String()})
					if got.PrincipalID != ids[target] || got.VoiceAllowed {
						t.Errorf("-> %s: answer %+v", target, got)
					}
				}
				bodies[target] = rec.Body.Bytes()
			}
			// An answer that tells an existing target from a ghost is a leak:
			// byte-identical for a caller the route refuses outright, and for
			// the hidden and the unknown id.
			if tt.want["member"].status == forb || tt.want["member"].status == http.StatusUnauthorized {
				for _, target := range targets[1:] {
					if !bytes.Equal(bodies["member"], bodies[target]) {
						t.Errorf("-> %s answers %s, the member target %s", target, bodies[target], bodies["member"])
					}
				}
			} else if !bytes.Equal(bodies["hidden"], bodies["unknown"]) {
				t.Errorf("hidden answers %s, unknown %s", bodies["hidden"], bodies["unknown"])
			}
			// A refused request changed nothing.
			if tt.want["member"].status != ok {
				for _, who := range []string{"ann2", "op2", "root"} {
					if !f.voiceStored(t, who) {
						t.Errorf("%s lost voice though every request was refused", who)
					}
				}
			}
		})
	}
}

// TestVoicePermissionBodyAndState: a body without the field, or with an
// unknown one, is refused and changes nothing; the answer says whether it
// changed; each change is one audit row and a repeat none; whoami and the
// operator's principal list follow both ways.
func TestVoicePermissionBodyAndState(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ann := f.ids["ann"]
	for _, body := range []string{`{}`, `{"voice_allowed":null}`, `{"voice_allowed":false,"x":1}`, `{"voice_allowed":false} {}`, `{`, ``} {
		res := f.callREST(t, http.MethodPut, voicePermissionPath(ann), f.rootTok, body)
		if res.status != http.StatusBadRequest {
			t.Errorf("body %q = %d %s, want 400", body, res.status, res.body)
		}
	}
	if !f.voiceStored(t, "ann") || len(f.audits(t, store.AuditVoicePermissionChanged)) != 0 {
		t.Fatal("a refused body changed something")
	}
	steps := []struct {
		allowed     bool
		wantChanged bool
		wantRows    int
	}{
		{true, false, 0}, // already on
		{false, true, 1},
		{false, false, 1}, // repeat
		{true, true, 2},
		{true, false, 2},
	}
	for i, st := range steps {
		res := f.putVoice(t, ann, st.allowed)
		if res.status != http.StatusOK {
			t.Fatalf("step %d: %d %s", i, res.status, res.body)
		}
		got := decodeVoicePermission(t, res)
		if got.Changed != st.wantChanged || got.VoiceAllowed != st.allowed || got.PrincipalID != ann {
			t.Errorf("step %d: answer %+v, want changed=%t voice_allowed=%t", i, got, st.wantChanged, st.allowed)
		}
		rows := f.audits(t, store.AuditVoicePermissionChanged)
		if len(rows) != st.wantRows {
			t.Errorf("step %d: %d voice_permission_changed rows, want %d", i, len(rows), st.wantRows)
		}
		if st.wantChanged {
			last := rows[len(rows)-1]
			if last.Actor != f.actor("root") || last.Subject != f.actor("ann") || last.Detail != fmt.Sprintf("voice_allowed=%t", st.allowed) {
				t.Errorf("step %d: row %+v", i, last)
			}
		}
		var who schema.WhoAmIResponseV1
		if res := f.callREST(t, http.MethodGet, "/v1/whoami", f.tokens["ann"], ""); res.status != http.StatusOK || json.Unmarshal([]byte(res.body), &who) != nil || who.VoiceAllowed == nil || *who.VoiceAllowed != st.allowed {
			t.Errorf("step %d: whoami = %d %s, want voice_allowed=%t", i, res.status, res.body, st.allowed)
		}
		list := f.callREST(t, http.MethodGet, "/v1/principals", f.rootTok, "")
		var page schema.ListPrincipalsResponseV1
		if list.status != http.StatusOK || json.Unmarshal([]byte(list.body), &page) != nil {
			t.Fatalf("step %d: list = %d %s", i, list.status, list.body)
		}
		for _, p := range page.Principals {
			switch p.Kind {
			case schema.PrincipalHuman:
				if p.VoiceAllowed == nil || p.MayInvite == nil {
					t.Errorf("step %d: %s lacks voice_allowed or may_invite", i, p.Name)
				} else if p.ID == ann && *p.VoiceAllowed != st.allowed {
					t.Errorf("step %d: list says voice_allowed=%t", i, *p.VoiceAllowed)
				}
			default:
				if p.VoiceAllowed != nil || p.MayInvite != nil {
					t.Errorf("agent %s carries voice_allowed or may_invite", p.Name)
				}
			}
		}
	}
	// On changed nothing live: no removal row at all in this run.
	if n := len(f.audits(t, store.AuditVoiceParticipantRemoved)); n != 0 {
		t.Errorf("%d voice_participant_removed rows from a run with nobody in voice", n)
	}
}

// TestVoicePermissionOffRemovesAtOnce: the PUT off removes p<id>,
// p<id>.screen and p<id>.view from the room before it answers, with no pass
// run: one voice_participant_removed row each with reason voice_not_allowed, a
// held share gone, nobody else touched and no room rotated. Turning voice on
// changes nothing live.
func TestVoicePermissionOffRemovesAtOnce(t *testing.T) {
	f, room := newVoiceRoomFixture(t)
	f.mustClaim(t, "ann")
	stored := f.room(t, f.ops)
	deleted := f.lk.count("DeleteRoom")

	res := f.putVoice(t, f.ids["ann"], false)
	if res.status != http.StatusOK {
		t.Fatalf("PUT = %d %s", res.status, res.body)
	}
	// Read at the moment the PUT has answered.
	id := f.identity("ann")
	for _, name := range []string{id, id + voiceSuffixScreen, id + voiceSuffixView} {
		if f.lk.in(room, name) {
			t.Errorf("%s is still connected when the PUT answered", name)
		}
	}
	for _, who := range []string{"ann2", "op1"} {
		if !f.lk.in(room, f.identity(who)) {
			t.Errorf("%s was removed by ann's switch", who)
		}
	}
	if got := f.who(t, f.ops); got != "ann2,op1" {
		t.Errorf("presence shows %q, want ann2,op1", got)
	}
	if holder := f.shareState(room); holder != 0 || f.granted(t, room, "ann") {
		t.Errorf("a share outlived the switch (holder %d)", holder)
	}
	want := []string{"ann started audio=true", "ann stopped principal=ann reason=access_lost"}
	if got := f.shareRows(t); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("share rows %q, want %q", got, want)
	}
	removed := map[string]int{}
	for _, e := range f.audits(t, store.AuditVoiceParticipantRemoved) {
		if !strings.Contains(e.Detail, "reason="+voiceReasonVoiceOff) {
			t.Errorf("removal row %q lacks reason=%s", e.Detail, voiceReasonVoiceOff)
		}
		if !strings.Contains(e.Detail, fmt.Sprintf("principal=%d ", f.ids["ann"])) {
			t.Errorf("removal row %q is not ann's", e.Detail)
		}
		switch {
		case strings.Contains(e.Detail, "participant=screen"):
			removed["screen"]++
		case strings.Contains(e.Detail, "participant=view"):
			removed["view"]++
		default:
			removed["person"]++
		}
	}
	for _, kind := range []string{"person", "screen", "view"} {
		if removed[kind] != 1 {
			t.Errorf("%d removal rows for the %s, want 1", removed[kind], kind)
		}
	}
	if n := len(f.audits(t, store.AuditVoiceParticipantRemoved)); n != 3 {
		t.Errorf("%d removal rows, want 3 (person, screen, view)", n)
	}
	if got := f.room(t, f.ops); got != stored {
		t.Errorf("the room was rotated: %q, was %q", got, stored)
	}
	if got := f.lk.count("DeleteRoom") - deleted; got != 0 {
		t.Errorf("%d DeleteRoom calls, want none", got)
	}

	// On changes nothing live, and the next session is issued again.
	removals := f.lk.count("RemoveParticipant")
	if res := f.putVoice(t, f.ids["ann"], true); res.status != http.StatusOK {
		t.Fatalf("PUT on = %d %s", res.status, res.body)
	}
	if got := f.lk.count("RemoveParticipant"); got != removals {
		t.Errorf("turning voice on made %d removals", got-removals)
	}
	if res := f.session(t, "ann", "ops"); res.status != http.StatusOK {
		t.Errorf("a session after turning voice on = %d %s", res.status, res.body)
	}
}

// TestVoicePermissionOwnerStaysInVoice: the brake does not bind an owner
// (design §5): an owner switched off by another owner is not removed.
func TestVoicePermissionOwnerStaysInVoice(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	if _, err := f.srv.store.AddChannelMember(context.Background(), "system", f.ops.ID, f.ids["root"], 0); err != nil {
		t.Fatal(err)
	}
	room := decodeSession(t, f.session(t, "root", "ops")).Rooms[0].Room
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("root"), joinedMs: 1_000, published: true, muted: true})
	f.at(0)
	f.pass(t)
	if res := f.putVoice(t, f.ids["root"], false); res.status != http.StatusOK {
		t.Fatalf("PUT = %d %s", res.status, res.body)
	}
	if !f.lk.in(room, f.identity("root")) {
		t.Error("the owner was removed by the brake, which skips owners")
	}
	f.pass(t)
	if !f.lk.in(room, f.identity("root")) {
		t.Error("the owner was removed by the next pass")
	}
}

// TestVoicePermissionHookFailureLeavesItToThePass: LiveKit refuses the
// removal; the PUT still answers 200 and the switch stands, and the next pass
// removes the person.
func TestVoicePermissionHookFailureLeavesItToThePass(t *testing.T) {
	f, room := newVoiceRoomFixture(t)
	f.lk.setRemoveStatus(http.StatusInternalServerError)
	res := f.putVoice(t, f.ids["ann"], false)
	if res.status != http.StatusOK || !decodeVoicePermission(t, res).Changed {
		t.Fatalf("PUT = %d %s, want 200 changed", res.status, res.body)
	}
	id := f.identity("ann")
	if !f.lk.in(room, id) {
		t.Fatal("setup: the refused removal removed them")
	}
	if f.voiceStored(t, "ann") {
		t.Fatal("the switch did not stand")
	}
	f.lk.setRemoveStatus(0)
	f.pass(t)
	for _, name := range []string{id, id + voiceSuffixScreen, id + voiceSuffixView} {
		if f.lk.in(room, name) {
			t.Errorf("%s is still connected after the next pass", name)
		}
	}
}

// TestVoicePermissionTokenMintedBeforeTheFlip: a join token signed before the
// flip may connect (LiveKit knows nothing of the change) and the next pass
// removes all three identities with the voice reason; a new session is
// refused and nothing rotated.
func TestVoicePermissionTokenMintedBeforeTheFlip(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room // the token
	stored := f.room(t, f.ops)
	if res := f.putVoice(t, f.ids["ann"], false); res.status != http.StatusOK {
		t.Fatalf("PUT = %d %s", res.status, res.body)
	}
	f.lk.setRoom(room, f.connected(room, "ann")...) // the old token connects
	f.at(0)
	f.pass(t)
	id := f.identity("ann")
	for _, name := range []string{id, id + voiceSuffixScreen, id + voiceSuffixView} {
		if f.lk.in(room, name) {
			t.Errorf("%s is still connected after the pass", name)
		}
	}
	n := 0
	for _, e := range f.audits(t, store.AuditVoiceParticipantRemoved) {
		if strings.Contains(e.Detail, "reason="+voiceReasonVoiceOff) {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d removal rows with reason=%s, want 3", n, voiceReasonVoiceOff)
	}
	if res := f.session(t, "ann", "ops"); res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
		t.Errorf("a new session = %d %s, want 403 voice_not_allowed", res.status, res.body)
	}
	if got := f.room(t, f.ops); got != stored {
		t.Errorf("the room was rotated: %q, was %q", got, stored)
	}
}

// TestVoicePermissionFlipUnderRequests: the switch commits while a session or
// a companion grant is inside LiveKit's answer; voiceStillEntitled finds it
// and signs nothing (403 voice_not_allowed, no token, no session row). Run
// under -race.
func TestVoicePermissionFlipUnderRequests(t *testing.T) {
	flip := func(t *testing.T, f *presenceFixture) {
		t.Helper()
		if _, err := f.srv.store.SetVoiceAllowed(context.Background(), "system", 0, f.ids["ann"], false, false); err != nil {
			t.Errorf("flip: %v", err)
		}
	}
	tests := []struct {
		name string
		do   func(t *testing.T, f *presenceFixture) wireResult
	}{
		{"session", func(t *testing.T, f *presenceFixture) wireResult { return f.session(t, "ann", "ops") }},
		{"companion screen", func(t *testing.T, f *presenceFixture) wireResult {
			return f.companion(t, "ann", "ops", companionScreen)
		}},
		{"companion viewer", func(t *testing.T, f *presenceFixture) wireResult {
			return f.companion(t, "ann", "ops", companionViewerBody)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newShareFixture(t)
			issued := len(f.audits(t, store.AuditVoiceSessionIssued))
			f.lk.setOnCreate(func(string) { flip(t, f) })
			res := tt.do(t, f)
			if res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
				t.Fatalf("answer = %d %s, want 403 voice_not_allowed", res.status, res.body)
			}
			if strings.Contains(res.body, "eyJ") || strings.Contains(res.body, "token") {
				t.Errorf("the refusal carries a token: %s", res.body)
			}
			if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != issued {
				t.Errorf("voice_session_issued rows = %d, want %d", n, issued)
			}
		})
	}
	// Transmit and share after the flip are refused up front.
	t.Run("transmit and share after the flip", func(t *testing.T) {
		f, _ := newShareFixture(t)
		flip(t, f)
		if res := f.report(t, "ann", "ops", "transmitting"); res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
			t.Errorf("transmit = %d %s, want 403 voice_not_allowed", res.status, res.body)
		}
		if res := f.claimShare(t, "ann", "ops"); res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
			t.Errorf("share = %d %s, want 403 voice_not_allowed", res.status, res.body)
		}
	})
}
