package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Voice and archiving (issue #844, decision 7): archiving a channel ends the
// voice in it before the settings PUT answers, and nobody can rejoin until it
// is restored. Over the scripted LiveKit and the manual clock; nothing sleeps.

func (f *presenceFixture) putArchived(t *testing.T, archived bool) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", f.ops.ID), f.rootTok, fmt.Sprintf(`{"archived":%t}`, archived))
}

// archiveRefusals are the voice routes that claim something; each is
// refused 409 channel_archived while the channel is archived.
func (f *presenceFixture) voiceClaims(t *testing.T, who string) map[string]wireResult {
	t.Helper()
	return map[string]wireResult{
		"session":   f.session(t, who, "ops"),
		"share":     f.claimShare(t, who, "ops"),
		"camera":    f.camera(t, http.MethodPost, who, "ops", ""),
		"companion": f.callREST(t, http.MethodPost, "/v1/channels/ops/voice/companion", f.tokens[who], `{"publish":"none","subscribe":true}`),
	}
}

// TestArchiveEndsVoice: people in voice, a share and a camera all end before
// the PUT answers: the room rotates with channel_archived, the share and the
// camera end with access_lost, the participants are gone from LiveKit and from
// presence, and the claim routes refuse until the restore, which starts
// nothing.
func TestArchiveEndsVoice(t *testing.T) {
	f, room := newCameraFixture(t)
	f.at(1_000)
	f.mustClaim(t, "ann")
	f.publish(room, "ann", true, true)
	f.mustCamera(t, "ann2", "")
	f.at(1_500)
	f.pass(t)
	if f.sharing(t, f.ops) != "ann+audio" || len(f.cameraHolders(room)) != 1 || !f.lk.has(room) {
		t.Fatal("the fixture is not in voice with a share and a camera")
	}
	issuedBefore := len(f.audits(t, store.AuditVoiceSessionIssued))

	res := f.putArchived(t, true)
	if res.status != http.StatusOK {
		t.Fatalf("archive = %d %s", res.status, res.body)
	}

	// Before the answer was written, all of it had happened.
	if f.lk.has(room) {
		t.Error("the old room is still in LiveKit")
	}
	next := f.room(t, f.ops)
	if next == room {
		t.Error("the room was not rotated")
	}
	if holder := f.shareState(room); holder != 0 {
		t.Errorf("the old room's share survives, held by %d", holder)
	}
	if holders := f.cameraHolders(room); len(holders) != 0 {
		t.Errorf("the old room's cameras survive: %v", holders)
	}
	if got := f.sharing(t, f.ops); got != "-" {
		t.Errorf("presence shows %q sharing", got)
	}
	if who := f.who(t, f.ops); len(who) != 0 {
		t.Errorf("presence still lists %v in the channel's room", who)
	}
	if want := []string{"ann started audio=true", "ann stopped principal=ann reason=access_lost"}; !slices.Equal(f.shareRows(t), want) {
		t.Errorf("share rows %q, want %q", f.shareRows(t), want)
	}
	if want := []string{"ann2 started", "system stopped principal=ann2 reason=access_lost"}; !slices.Equal(f.cameraRows(t), want) {
		t.Errorf("camera rows %q, want %q", f.cameraRows(t), want)
	}
	rotations := f.audits(t, store.AuditVoiceRoomRotated)
	if len(rotations) != 1 || rotations[0].Detail != "reason=channel_archived" || rotations[0].Subject != fmt.Sprintf("channel:%d", f.ops.ID) {
		t.Errorf("voice_room_rotated rows = %+v, want one with reason=channel_archived", rotations)
	}
	for _, e := range f.audit(t) {
		if strings.Contains(e.Detail, room) || strings.Contains(e.Detail, next) {
			t.Errorf("an audit row names a room: %+v", e)
		}
	}

	// Nobody can rejoin: every claim is refused, issues nothing, and writes
	// no row.
	before := len(f.audit(t))
	for _, who := range []string{"ann", "ann2"} {
		for name, res := range f.voiceClaims(t, who) {
			assertWire(t, res, http.StatusConflict, schema.ErrorCodeChannelArchived)
			if strings.Contains(res.body, "token") {
				t.Errorf("%s as %s carries a token: %s", name, who, res.body)
			}
		}
	}
	if len(f.audit(t)) != before || len(f.audits(t, store.AuditVoiceSessionIssued)) != issuedBefore {
		t.Error("a refused voice claim wrote audit rows")
	}
	// What lets go keeps working: the stops and the companion's removal.
	for name, res := range map[string]wireResult{
		"stop share":       f.stopShare(t, "ann", "ops"),
		"stop camera":      f.camera(t, http.MethodDelete, "ann2", "ops", ""),
		"remove companion": f.callREST(t, http.MethodDelete, "/v1/channels/ops/voice/companion", f.tokens["ann"], ""),
	} {
		if res.status != http.StatusNoContent {
			t.Errorf("%s in an archived channel = %d %s, want 204", name, res.status, res.body)
		}
	}
	// Reading voice presence still works for a member.
	if res := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens["ann"], ""); res.status != http.StatusOK {
		t.Errorf("the presence snapshot of an archived channel = %d %s, want 200", res.status, res.body)
	}

	// Restore starts nothing: no session, no room, no rotation.
	rotationsBefore := len(f.audits(t, store.AuditVoiceRoomRotated))
	if res := f.putArchived(t, false); res.status != http.StatusOK {
		t.Fatalf("restore = %d %s", res.status, res.body)
	}
	if len(f.audits(t, store.AuditVoiceSessionIssued)) != issuedBefore || len(f.audits(t, store.AuditVoiceRoomRotated)) != rotationsBefore {
		t.Error("the restore started or rotated something")
	}
	if f.lk.has(f.room(t, f.ops)) {
		t.Error("the restore created a room in LiveKit")
	}
	if who := f.who(t, f.ops); len(who) != 0 {
		t.Errorf("after the restore presence lists %v", who)
	}
	// And voice works again on request.
	decodeSession(t, f.session(t, "ann", "ops"))
}

// TestArchiveVoiceClaimsLeakNothing: the voice claims refuse an archived
// channel only to a member who may use voice; a non-member, an agent and a
// person without voice get the answers they got before.
func TestArchiveVoiceClaimsLeakNothing(t *testing.T) {
	f, _ := newCameraFixture(t)
	if res := f.putArchived(t, true); res.status != http.StatusOK {
		t.Fatalf("archive = %d %s", res.status, res.body)
	}
	for name, res := range f.voiceClaims(t, "bob") { // bob is in no channel
		assertWire(t, res, http.StatusNotFound, "channel_not_found")
		_ = name
	}
	for name, res := range f.voiceClaims(t, "robo") { // an agent member
		if res.status != http.StatusForbidden {
			t.Errorf("%s as an agent = %d %s, want the agent refusal 403", name, res.status, res.body)
		}
	}
}

// TestVoiceSessionRechecksArchiveBeforeSigning: a session whose handler saw a
// live channel, and which the archive then overtakes, gets no token and records
// no holder (voiceGrantOnce re-reads the channel; decision 7, "before tokens are
// signed"). The stale channel value is the one the handler had read.
func TestVoiceSessionRechecksArchiveBeforeSigning(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	stale := f.ops // read live
	archived := true
	if _, err := f.srv.store.ChannelSettings(ctx, "system", 0, f.ops.ID, store.ChannelSettingsChange{Archived: &archived}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ann, err := f.srv.store.PrincipalByID(ctx, f.ids["ann"])
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := f.srv.voiceSession(withCredentialID(ctx, f.credID(t, "ann")), ann, stale)
	if !errors.Is(err, errVoiceChannelArchived) {
		t.Fatalf("voiceSession on a channel archived under it = %v, want errVoiceChannelArchived", err)
	}
	if len(resp.Rooms) != 0 {
		t.Errorf("a refused session carries rooms: %+v", resp)
	}
	if n := f.lk.count("CreateRoom"); n != 0 {
		t.Errorf("LiveKit was asked to create %d room(s) for a refused session", n)
	}
}

// TestVoiceSessionRechecksArchiveAfterLiveKit: the archive lands while LiveKit
// is being asked to create the room, after the first check and before the
// token is signed: the second check refuses, and no token is issued.
func TestVoiceSessionRechecksArchiveAfterLiveKit(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	archived := true
	f.lk.setOnCreate(func(string) {
		if _, err := f.srv.store.ChannelSettings(ctx, "system", 0, f.ops.ID, store.ChannelSettingsChange{Archived: &archived}, time.Now()); err != nil {
			t.Error(err)
		}
	})
	ann, err := f.srv.store.PrincipalByID(ctx, f.ids["ann"])
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := f.srv.voiceSession(withCredentialID(ctx, f.credID(t, "ann")), ann, f.ops)
	if !errors.Is(err, errVoiceChannelArchived) {
		t.Fatalf("voiceSession archived during CreateRoom = %v, want errVoiceChannelArchived", err)
	}
	if len(resp.Rooms) != 0 {
		t.Errorf("a refused session carries rooms: %+v", resp)
	}
}
