package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// voiceKickFixture is the presence fixture with the moderation cast added:
//
//	mgr      a human member of ops who manages it
//	mgr2     a human member of ops2 only, who manages ops2 (a manager elsewhere)
//	ann      also manages ops2, and is a plain member of ops
//	plain    a human member of ops with no standing
//	op2      a non-owner operator, a member of nothing
//
// root is the owner operator.
type voiceKickFixture struct{ *presenceFixture }

func newVoiceKickFixture(t *testing.T, o presenceOpts) *voiceKickFixture {
	t.Helper()
	f := &voiceKickFixture{newPresenceFixture(t, o)}
	ctx := context.Background()
	for _, spec := range []struct {
		name string
		in   []store.Channel
	}{
		{"mgr", []store.Channel{f.ops}},
		{"mgr2", []store.Channel{f.ops2}},
		{"plain", []store.Channel{f.ops}},
		{"op2", nil},
	} {
		p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, spec.name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := f.srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[spec.name], f.ids[spec.name] = tok, p.ID
		for _, ch := range spec.in {
			if _, err := f.srv.store.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.srv.store.SetChannelMemberManages(ctx, "system", f.ops.ID, f.ids["mgr"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.SetChannelMemberManages(ctx, "system", f.ops2.ID, f.ids["mgr2"], true); err != nil {
		t.Fatal(err)
	}
	// ann is a member of ops and a manager of ops2: standing elsewhere is none here.
	if _, err := f.srv.store.SetChannelMemberManages(ctx, "system", f.ops2.ID, f.ids["ann"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.SetRole(ctx, "system", f.ids["root"], f.ids["op2"], store.RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *voiceKickFixture) kick(t *testing.T, as, channel string, target int64) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodDelete, fmt.Sprintf("/v1/channels/%s/voice/participants/%d", channel, target), f.tokens[as], "")
}

func (f *voiceKickFixture) kicked(t *testing.T) []store.AuditEvent {
	t.Helper()
	return f.audits(t, store.AuditVoiceParticipantKicked)
}

// TestVoiceKickActorMatrix: who may remove whom (docs/design/moderation.md
// §4.1, §9 case 1). The target is never read before the standing is, so every
// refusal is the same for every target, and a person's standing is the only
// thing a 204 reveals.
func TestVoiceKickActorMatrix(t *testing.T) {
	tests := []struct {
		name    string
		actor   string
		channel string
		want    int
		audited bool
	}{
		{"owner operator", "root", "ops", http.StatusNoContent, true},
		{"owner operator, channel it is not in", "root", "ops2", http.StatusNoContent, true},
		{"non-owner operator, not a member", "op2", "ops", http.StatusNoContent, true},
		{"manager of the channel", "mgr", "ops", http.StatusNoContent, true},
		{"manager of another channel, a member here", "ann", "ops", http.StatusForbidden, false},
		{"manager of another channel, not a member here", "mgr2", "ops", http.StatusNotFound, false},
		{"plain member", "plain", "ops", http.StatusForbidden, false},
		{"non-member", "bob", "ops", http.StatusNotFound, false},
		{"agent", "robo", "ops", http.StatusForbidden, false},
		{"unknown channel", "root", "nosuch", http.StatusNotFound, false},
	}
	// Targets of every kind, present or not: the answer does not depend on them.
	targets := []string{"root", "op2", "mgr", "plain", "ann", "robo", "dora"}
	for _, tt := range tests {
		for _, target := range targets {
			t.Run(tt.name+"/target "+target, func(t *testing.T) {
				f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
				got := f.kick(t, tt.actor, tt.channel, f.ids[target])
				if got.status != tt.want {
					t.Fatalf("status = %d (%s), want %d", got.status, got.body, tt.want)
				}
				rows := f.kicked(t)
				if tt.audited {
					wantDetail := fmt.Sprintf("principal=%d result=completed", f.ids[target])
					if len(rows) != 1 || rows[0].Actor != f.actor(tt.actor) || rows[0].Detail != wantDetail {
						t.Fatalf("audit = %+v, want one %q by %s", rows, wantDetail, tt.actor)
					}
				} else if len(rows) != 0 {
					t.Fatalf("a refusal wrote %+v", rows)
				}
			})
		}
	}
}

// TestVoiceKickGhostIdentical: a manager of another channel who is not a member
// gets exactly the response for a channel that does not exist, whatever the
// target (design §9 case 1; the isolation matrix in e2e/isolation repeats it
// over the wire).
func TestVoiceKickGhostIdentical(t *testing.T) {
	f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
	for _, target := range []int64{f.ids["ann"], f.ids["mgr"], 99999} {
		ghost := f.kick(t, "mgr2", "nosuch", target)
		real := f.kick(t, "mgr2", "ops", target)
		if ghost != real || ghost.status != http.StatusNotFound {
			t.Errorf("target %d: ghost %+v, real %+v: want identical 404s", target, ghost, real)
		}
	}
	// A bad target id is a request error only for someone with standing.
	if got := f.callREST(t, http.MethodDelete, "/v1/channels/ops/voice/participants/x", f.tokens["plain"], ""); got.status != http.StatusForbidden {
		t.Errorf("plain member with a bad id = %d, want 403 before the id is read", got.status)
	}
	if got := f.callREST(t, http.MethodDelete, "/v1/channels/ops/voice/participants/x", f.tokens["mgr"], ""); got.status != http.StatusBadRequest {
		t.Errorf("manager with a bad id = %d, want 400", got.status)
	}
	// No credential at all.
	if got := f.callREST(t, http.MethodDelete, "/v1/channels/ops/voice/participants/1", "", ""); got.status != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", got.status)
	}
}

// TestVoiceKickRevokedManager: the standing is read from the store, not from
// the guard's earlier answer (CheckChannelModerator, which the handler calls
// before any work), so a revoked grant removes nobody.
func TestVoiceKickRevokedManager(t *testing.T) {
	f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
	f.pass(t)
	if err := f.srv.store.CheckChannelModerator(context.Background(), f.ops.ID, f.ids["mgr"]); err != nil {
		t.Fatalf("manager: %v", err)
	}
	if _, err := f.srv.store.SetChannelMemberManages(context.Background(), "system", f.ops.ID, f.ids["mgr"], false); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.CheckChannelModerator(context.Background(), f.ops.ID, f.ids["mgr"]); err == nil {
		t.Fatal("a manager whose grant was revoked still passes the standing check")
	}
	if got := f.kick(t, "mgr", "ops", f.ids["ann"]); got.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got.status)
	}
	if f.lk.count("RemoveParticipant") != 0 || len(f.kicked(t)) != 0 {
		t.Fatal("a refused request did work or wrote a row")
	}
}

// TestVoiceKickEffects: a successful removal ends presence and the transmit
// before LiveKit is asked, removes only the target, from every current room of
// the channel (a net room included) and from no other channel, leaves
// membership, credentials and everyone else alone, and a rejoin works.
func TestVoiceKickEffects(t *testing.T) {
	f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	room := f.inUse(t, f.ops)
	room2 := f.inUse(t, f.ops2)
	netRoom := store.VoiceRoom{ID: 9001, ChannelID: f.ops.ID, NetID: 7, RoomName: "net-room-for-test"}
	main, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.voice.channelRooms = func(_ context.Context, id int64) ([]store.VoiceRoom, error) {
		if id != f.ops.ID {
			return nil, nil
		}
		return []store.VoiceRoom{main, netRoom}, nil
	}
	ann, ann2 := f.identity("ann"), f.identity("ann2")
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000, published: true}, fakeParticipant{identity: ann2, joinedMs: 2_000})
	f.lk.setRoom(netRoom.RoomName, fakeParticipant{identity: ann, joinedMs: 1_000})
	f.lk.setRoom(room2, fakeParticipant{identity: ann, joinedMs: 1_000})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann*,ann2" {
		t.Fatalf("before: %q", got)
	}

	// At the first LiveKit removal she is already out of presence.
	var firstOnce sync.Once
	var atFirst string
	f.lk.setOnRemove(func(_, identity string) {
		if identity == ann {
			firstOnce.Do(func() { atFirst = f.who(t, f.ops) })
		}
	})
	creds := f.credID(t, "ann")
	got := f.kick(t, "mgr", "ops", f.ids["ann"])
	if got.status != http.StatusNoContent || got.body != "" {
		t.Fatalf("kick = %d %q", got.status, got.body)
	}
	if atFirst != "ann2" {
		t.Errorf("presence at the first removal = %q, want ann2 only (presence ends before LiveKit)", atFirst)
	}
	if f.lk.in(room, ann) || f.lk.in(netRoom.RoomName, ann) {
		t.Error("ann is still in a room of the channel")
	}
	if !f.lk.in(room, ann2) {
		t.Error("another participant was removed")
	}
	if !f.lk.in(room2, ann) {
		t.Error("ann was removed from another channel's room")
	}
	if f.lk.count("DeleteRoom") != 0 || f.lk.count("CreateRoom") != 0 {
		t.Error("a room was rotated or recreated")
	}
	if got := f.who(t, f.ops); got != "ann2" {
		t.Errorf("presence after = %q, want ann2", got)
	}
	// Membership, credential and the chat side are unchanged.
	if ok, err := f.srv.store.IsChannelMember(ctx, f.ops.ID, f.ids["ann"]); err != nil || !ok {
		t.Errorf("ann is no longer a member: %v %v", ok, err)
	}
	if f.credID(t, "ann") != creds {
		t.Error("credential changed")
	}
	if r := f.callREST(t, http.MethodGet, "/v1/whoami", f.tokens["ann"], ""); r.status != http.StatusOK {
		t.Errorf("ann's credential = %d", r.status)
	}

	// Audit: one request row, by the moderator, ids and result only; the
	// telemetry names the reason and no room.
	rows := f.kicked(t)
	if len(rows) != 1 || rows[0].Actor != f.actor("mgr") || rows[0].Subject != fmt.Sprintf("channel:%d", f.ops.ID) ||
		rows[0].Detail != fmt.Sprintf("principal=%d result=completed", f.ids["ann"]) {
		t.Fatalf("kicked rows = %+v", rows)
	}
	removed := f.audits(t, store.AuditVoiceParticipantRemoved)
	if len(removed) != 2 {
		t.Fatalf("voice_participant_removed rows = %+v, want one per room", removed)
	}
	for _, e := range f.audit(t) {
		for _, secret := range []string{room, room2, netRoom.RoomName, f.tokens["mgr"], f.tokens["ann"], "127.0.0.1", "192.0.2."} {
			if strings.Contains(e.Subject+" "+e.Detail+" "+e.Actor, secret) {
				t.Errorf("audit row %s %q leaks %q", e.Action, e.Detail, secret)
			}
		}
	}
	for _, e := range removed {
		if !strings.Contains(e.Detail, "reason="+voiceReasonModeratorKick) {
			t.Errorf("removed detail = %q, want reason=moderator_kick", e.Detail)
		}
	}

	// A rejoin is not refused.
	f.lk.setOnRemove(nil)
	if r := f.session(t, "ann", "ops"); r.status != http.StatusOK {
		t.Fatalf("rejoin = %d %s", r.status, r.body)
	}
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 9_000}, fakeParticipant{identity: ann2, joinedMs: 2_000})
	f.clock.Advance(testPassGap)
	f.pass(t)
	if got := f.who(t, f.ops); !strings.Contains(got, "ann") {
		t.Errorf("after rejoin = %q, want ann back", got)
	}
}

// TestVoiceKickPresenceCloseReason: the kicked target's presence socket is
// closed 1008 with the text schema.SocketCloseVoiceRemoved, the one close a
// client keys on by value (issue #1409); a real removal from the channel
// still says "no longer a member". Someone else's socket in the channel, and
// the target's own in another channel, stay open either way.
func TestVoiceKickPresenceCloseReason(t *testing.T) {
	tests := []struct {
		name       string
		act        func(t *testing.T, f *voiceKickFixture) int
		wantReason string
		stillIn    bool
	}{
		{"removed from voice only", func(t *testing.T, f *voiceKickFixture) int {
			return f.kick(t, "mgr", "ops", f.ids["ann"]).status
		}, string(schema.SocketCloseVoiceRemoved), true},
		{"removed from the channel", func(t *testing.T, f *voiceKickFixture) int {
			return f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok)
		}, "no longer a member of this channel", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
			base := wsTestServer(t, f.srv)
			annOps, _, err := dialVoice(t, base, "ops", f.tokens["ann"])
			if err != nil {
				t.Fatal(err)
			}
			readPresence(t, annOps)
			annOps2, _, err := dialVoice(t, base, "ops2", f.tokens["ann"])
			if err != nil {
				t.Fatal(err)
			}
			readPresence(t, annOps2)
			plainOps, _, err := dialVoice(t, base, "ops", f.tokens["plain"])
			if err != nil {
				t.Fatal(err)
			}
			readPresence(t, plainOps)

			if code := tt.act(t, f); code != http.StatusNoContent {
				t.Fatalf("status = %d", code)
			}
			ce := expectCloseError(t, annOps)
			if ce.Code != websocket.StatusPolicyViolation || ce.Reason != tt.wantReason {
				t.Errorf("close = %d %q, want %d %q", ce.Code, ce.Reason, websocket.StatusPolicyViolation, tt.wantReason)
			}
			if member, err := f.srv.store.IsChannelMember(context.Background(), f.ops.ID, f.ids["ann"]); err != nil || member != tt.stillIn {
				t.Errorf("ann a member of ops = %v %v, want %v", member, err, tt.stillIn)
			}
			// The others are untouched: a change still reaches them.
			room := f.inUse(t, f.ops)
			f.lk.setRoom(room, fakeParticipant{identity: f.identity("plain"), joinedMs: 1_000})
			f.lk.setRoom(f.inUse(t, f.ops2), fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
			f.pass(t)
			if s := readPresence(t, plainOps); !s.Available {
				t.Errorf("plain's frame: %+v", s)
			}
			if s := readPresence(t, annOps2); !s.Available {
				t.Errorf("ann's ops2 frame: %+v", s)
			}
		})
	}
}

// TestVoiceKickRacingRemoval: a kick and a removal from the channel in the
// same instant. The kick has already detached ann's subscription when the
// removal commits, after the close path's own membership read; the removal's
// drop must still reach that subscription, so the frame says "no longer a
// member" and never voice_removed to someone whose membership is gone (#1458
// review). The removal is run from the close path's test hook, between its
// membership read and its choice of reason: deterministic, no timing.
func TestVoiceKickRacingRemoval(t *testing.T) {
	f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
	base := wsTestServer(t, f.srv)
	annOps, _, err := dialVoice(t, base, "ops", f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	readPresence(t, annOps)
	var once sync.Once
	errs := make(chan error, 1)
	f.srv.voice.beforeCloseReason = func() {
		once.Do(func() {
			ctx := context.Background()
			_, err := f.srv.store.RemoveChannelMember(ctx, "system", f.ops.ID, f.ids["ann"])
			errs <- err
			f.srv.voiceMemberRemoved(ctx, f.ops.ID, f.ids["ann"])
		})
	}
	if got := f.kick(t, "mgr", "ops", f.ids["ann"]); got.status != http.StatusNoContent {
		t.Fatalf("kick = %d %q", got.status, got.body)
	}
	ce := expectCloseError(t, annOps)
	if err := <-errs; err != nil {
		t.Fatalf("remove: %v", err)
	}
	if want := "no longer a member of this channel"; ce.Code != websocket.StatusPolicyViolation || ce.Reason != want {
		t.Errorf("close = %d %q, want %d %q", ce.Code, ce.Reason, websocket.StatusPolicyViolation, want)
	}
}

// TestVoiceKickCloseReasonStoreFailure: when the close path cannot read
// membership it fails closed with an error, never with voice_removed, which
// would tell a client it is still a member on no evidence.
func TestVoiceKickCloseReasonStoreFailure(t *testing.T) {
	f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
	base := wsTestServer(t, f.srv)
	annOps, _, err := dialVoice(t, base, "ops", f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	readPresence(t, annOps)
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.srv.voice.kickPrincipal(f.ops.ID, f.ids["ann"])
	ce := expectCloseError(t, annOps)
	if ce.Code != websocket.StatusInternalError || ce.Reason != "membership check failed" {
		t.Errorf("close = %d %q, want %d %q", ce.Code, ce.Reason, websocket.StatusInternalError, "membership check failed")
	}
}

// expectCloseError waits for conn to be closed by the server and returns the
// close frame, code and text.
func expectCloseError(t *testing.T, conn *websocket.Conn) websocket.CloseError {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("socket ended without a close frame: %v", err)
		}
		return ce
	}
}

// TestVoiceKickAbsentAndUnconfigured: a target who is not in voice, an unknown
// id and an agent all answer 204 and write the request row; with LiveKit
// unconfigured there can be no media participant and the answer is 204 too.
func TestVoiceKickAbsentAndUnconfigured(t *testing.T) {
	tests := []struct {
		name         string
		unconfigured bool
		target       string
		id           int64
	}{
		{"member not in voice", false, "ann", 0},
		{"unknown principal", false, "", 987654},
		{"an agent", false, "robo", 0},
		{"unconfigured", true, "ann", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired, unconfigured: tt.unconfigured})
			id := tt.id
			if tt.target != "" {
				id = f.ids[tt.target]
			}
			if got := f.kick(t, "mgr", "ops", id); got.status != http.StatusNoContent {
				t.Fatalf("status = %d %s", got.status, got.body)
			}
			rows := f.kicked(t)
			if len(rows) != 1 || rows[0].Detail != fmt.Sprintf("principal=%d result=completed", id) {
				t.Fatalf("rows = %+v", rows)
			}
			if n := len(f.audits(t, store.AuditVoiceParticipantRemoved)); n != 0 {
				t.Errorf("an absent target wrote %d voice_participant_removed rows", n)
			}
		})
	}
}

// TestVoiceKickLiveKitFailure: a LiveKit that is down, or that fails one room
// of several, never gives a 204. Presence still ends, the row says failed, and
// a retry after the failure clears completes.
func TestVoiceKickLiveKitFailure(t *testing.T) {
	tests := []struct {
		name string
		fail func(f *voiceKickFixture, main, net string)
	}{
		{"livekit down", func(f *voiceKickFixture, _, _ string) { f.lk.setRemoveStatus(http.StatusInternalServerError) }},
		{"one room of two", func(f *voiceKickFixture, _, net string) {
			f.lk.config(func(l *scriptedLiveKit) { l.removeFail = map[string]bool{net: true} })
		}},
		{"the main room, the net room fine", func(f *voiceKickFixture, main, _ string) {
			f.lk.config(func(l *scriptedLiveKit) { l.removeFail = map[string]bool{main: true} })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
			ctx := context.Background()
			room := f.inUse(t, f.ops)
			main, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID)
			if err != nil {
				t.Fatal(err)
			}
			netRoom := store.VoiceRoom{ID: 9001, ChannelID: f.ops.ID, NetID: 7, RoomName: "net-room-for-test"}
			f.srv.voice.channelRooms = func(context.Context, int64) ([]store.VoiceRoom, error) {
				return []store.VoiceRoom{main, netRoom}, nil
			}
			ann := f.identity("ann")
			f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000, published: true})
			f.lk.setRoom(netRoom.RoomName, fakeParticipant{identity: ann, joinedMs: 1_000})
			f.pass(t)
			tt.fail(f, room, netRoom.RoomName)

			got := f.kick(t, "mgr", "ops", f.ids["ann"])
			if got.status != http.StatusServiceUnavailable || !strings.Contains(got.body, "voice_unavailable") {
				t.Fatalf("status = %d %s, want 503 voice_unavailable", got.status, got.body)
			}
			if strings.Contains(got.body, room) || strings.Contains(got.body, netRoom.RoomName) {
				t.Error("the error names a room")
			}
			rows := f.kicked(t)
			if len(rows) != 1 || rows[0].Detail != fmt.Sprintf("principal=%d result=failed", f.ids["ann"]) {
				t.Fatalf("rows = %+v, want one failed", rows)
			}
			if got := f.who(t, f.ops); strings.Contains(got, "ann") {
				t.Errorf("presence = %q: it ends before LiveKit is asked, whatever comes of it", got)
			}
			if ok, _ := f.srv.store.IsChannelMember(ctx, f.ops.ID, f.ids["ann"]); !ok {
				t.Error("membership changed")
			}

			// The retry, with LiveKit well again, completes.
			f.lk.setRemoveStatus(0)
			f.lk.config(func(l *scriptedLiveKit) { l.removeFail = nil })
			if got := f.kick(t, "mgr", "ops", f.ids["ann"]); got.status != http.StatusNoContent {
				t.Fatalf("retry = %d %s", got.status, got.body)
			}
			if f.lk.in(room, ann) || f.lk.in(netRoom.RoomName, ann) {
				t.Error("ann is still in a room after the retry")
			}
			results := []string{}
			for _, e := range f.kicked(t) {
				results = append(results, e.Detail[strings.LastIndex(e.Detail, "result="):])
			}
			if !slices.Equal(results, []string{"result=failed", "result=completed"}) {
				t.Errorf("results = %v", results)
			}
		})
	}
}

// TestVoiceKickRoomListFailure: when the channel's rooms cannot be read there
// is no success to give.
func TestVoiceKickRoomListFailure(t *testing.T) {
	f := newVoiceKickFixture(t, presenceOpts{auth: AuthRequired})
	f.srv.voice.channelRooms = func(context.Context, int64) ([]store.VoiceRoom, error) {
		return nil, fmt.Errorf("store is down")
	}
	got := f.kick(t, "root", "ops", f.ids["ann"])
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s, want 503", got.status, got.body)
	}
	if rows := f.kicked(t); len(rows) != 1 || !strings.HasSuffix(rows[0].Detail, "result=failed") {
		t.Fatalf("rows = %+v", rows)
	}
}
