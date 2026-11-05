package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the companion's share audio (issue #898, part C of #897;
// docs/design/voice-control-plane.md §13): PUT and DELETE
// /v1/channels/{channel}/voice/companion/audio, the consent's end with the
// share, and a companion publishing the sound with no consent recorded.
// Everything here runs against the scripted LiveKit and the fixture's manual
// clock.

const (
	audioApplication = `{"scope":"application"}`
	audioSystem      = `{"scope":"system_excluding_call"}`
)

func (f *presenceFixture) companionAudio(t *testing.T, who, body string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPut, "/v1/channels/ops/voice/companion/audio", f.tokens[who], body)
}

func (f *presenceFixture) stopCompanionAudio(t *testing.T, who string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodDelete, "/v1/channels/ops/voice/companion/audio", f.tokens[who], "")
}

// mustConsent asks for the share's sound as ann with body and requires it
// granted, with the scope the body named and the started_at of share.
func (f *presenceFixture) mustConsent(t *testing.T, body string, share schema.VoiceShareV1) schema.VoiceCompanionAudioV1 {
	t.Helper()
	res := f.companionAudio(t, "ann", body)
	if res.status != http.StatusOK {
		t.Fatalf("ann consents to share audio: %d %s, want 200", res.status, res.body)
	}
	dec := json.NewDecoder(strings.NewReader(res.body))
	dec.DisallowUnknownFields()
	var audio schema.VoiceCompanionAudioV1
	if err := dec.Decode(&audio); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if err := audio.Validate(); err != nil {
		t.Fatalf("answer %s does not validate: %v", res.body, err)
	}
	if !strings.Contains(body, string(audio.Scope)) || !audio.StartedAt.Time().Equal(share.StartedAt.Time()) {
		t.Fatalf("answer = %+v, want the scope of %s and the share's started_at %s", audio, body, share.StartedAt.Time())
	}
	return audio
}

// lastCompanionUpdate is the permission LiveKit was last asked to set on
// ann's companion.
func (f *presenceFixture) lastCompanionUpdate(t *testing.T) fakeUpdate {
	t.Helper()
	ups := f.lk.updatesFor(f.screenID("ann"))
	if len(ups) == 0 {
		t.Fatal("ann's companion was never updated")
	}
	return ups[len(ups)-1]
}

var (
	screenOnly     = []string{"SCREEN_SHARE"}
	screenAndAudio = []string{"SCREEN_SHARE", "SCREEN_SHARE_AUDIO"}
)

// TestVoiceCompanionAudio: the route's refusals, each issuing nothing; the
// grant, which names both screen-share sources with the companion's own
// canSubscribe and writes voice_share_audio_started; a repeated request as a
// no-op; a scope change as a record only; DELETE, which sets the companion
// back to the screen and writes voice_share_audio_stopped reason=self; a
// claim again keeping the consent; and no secret in anything written.
func TestVoiceCompanionAudio(t *testing.T) {
	t.Run("refusals issue nothing", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		updates := func() int { return len(f.lk.updatesFor(f.screenID("ann"))) }
		check := func(t *testing.T, who, body string, status int, code string) {
			t.Helper()
			before := updates()
			res := f.companionAudio(t, who, body)
			if res.status != status || (code != "" && errCodeIfAny(t, res.body) != code) {
				t.Fatalf("%s with %q: %d %s, want %d %s", who, body, res.status, res.body, status, code)
			}
			if strings.Contains(res.body, "principal") || strings.Contains(res.body, room) {
				t.Errorf("the refusal names somebody or the room: %s", res.body)
			}
			if n := updates(); n != before {
				t.Errorf("a refusal updated the companion (%d updates, had %d)", n, before)
			}
			if n := len(f.audits(t, store.AuditVoiceShareAudioStarted)) + len(f.audits(t, store.AuditVoiceShareAudioStopped)); n != 0 {
				t.Errorf("a refusal wrote %d audio rows", n)
			}
		}
		// Checks 1-5, before the body is read.
		check(t, "bob", audioApplication, http.StatusNotFound, "channel_not_found")
		check(t, "bob", `not json`, http.StatusNotFound, "channel_not_found")
		check(t, "robo", audioApplication, http.StatusForbidden, "")
		// The body, before the share is looked at: nobody holds one yet.
		check(t, "ann", `{"scope":"none"}`, http.StatusBadRequest, "invalid_request")
		check(t, "ann", `{"scope":"application","identity":"p1.screen"}`, http.StatusBadRequest, "invalid_request")
		check(t, "ann", ``, http.StatusBadRequest, "invalid_request")
		check(t, "ann", `{"scope":"application"}`+strings.Repeat(" ", 2048), http.StatusRequestEntityTooLarge, "request_too_large")
		// Nobody holds the share.
		check(t, "ann", audioApplication, http.StatusConflict, schema.ErrorCodeVoiceNoSession)
		// Someone else does.
		f.mustClaim(t, "ann2")
		check(t, "ann", audioApplication, http.StatusConflict, schema.ErrorCodeVoiceShareBusy)
		f.mustStop(t, "ann2")
		f.at(1_500)
		f.pass(t)
		// The caller holds it, but their companion is not in the room as the
		// last pass listed it.
		f.mustClaim(t, "ann")
		f.lk.drop(room, f.screenID("ann"))
		f.at(2_000)
		f.pass(t)
		check(t, "ann", audioApplication, http.StatusConflict, schema.ErrorCodeVoiceNoSession)
	})

	t.Run("no companion at all", func(t *testing.T) {
		f, _ := newShareFixture(t)
		f.mustClaim(t, "ann")
		res := f.companionAudio(t, "ann", audioApplication)
		if res.status != http.StatusConflict || errCodeIfAny(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("got %d %s, want 409 voice_no_session", res.status, res.body)
		}
		if n := f.lk.count("UpdateParticipant"); n != 1 { // the claim's own grant
			t.Errorf("UpdateParticipant calls = %d, want only the claim's", n)
		}
	})

	t.Run("grant, no-op, scope change, delete", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		grants := len(f.lk.updatesFor(f.screenID("ann"))) // the claim's restoration
		f.at(1_500)
		f.mustConsent(t, audioApplication, share)
		up := f.lastCompanionUpdate(t)
		if !up.canPublish || !up.canSubscribe || !slices.Equal(up.sources, screenAndAudio) {
			t.Fatalf("the grant set %+v, want canPublish, canSubscribe and %v", up, screenAndAudio)
		}
		if n := len(f.lk.updatesFor(f.screenID("ann"))); n != grants+1 {
			t.Errorf("companion updates = %d, want %d", n, grants+1)
		}
		if got := f.lk.updatesFor(f.identity("ann")); len(got) != 1 {
			t.Errorf("ann's own participant was updated %d times, want the claim's once", len(got))
		}
		want := []string{"ann started audio=true", "ann audio_started scope=application"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
		// The sound follows the companion's track, as the person's does.
		f.publishScreen(room, true, true)
		f.at(2_000)
		f.pass(t)
		if got := f.sharing(t, f.ops); got != "ann+audio" {
			t.Errorf("presence shows %q, want ann+audio", got)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("a consented sound was audited as unclaimed %d times", n)
		}

		// The same scope again: nothing is written anywhere.
		f.mustConsent(t, audioApplication, share)
		if n := len(f.lk.updatesFor(f.screenID("ann"))); n != grants+1 {
			t.Errorf("a repeated consent updated the companion (%d updates)", n)
		}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("after a repeated consent share rows %q, want %q", got, want)
		}

		// Another scope: recorded, the permission unchanged.
		f.at(2_500)
		audio := f.mustConsent(t, audioSystem, share)
		if audio.Scope != schema.VoiceShareAudioScopeSystemExcludingCall {
			t.Errorf("scope = %q", audio.Scope)
		}
		if n := len(f.lk.updatesFor(f.screenID("ann"))); n != grants+1 {
			t.Errorf("a scope change updated the companion (%d updates)", n)
		}
		want = append(want, "ann audio_started scope=system_excluding_call")
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("after a scope change share rows %q, want %q", got, want)
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.screenAudio {
			t.Error("a scope change unpublished the sound")
		}

		// DELETE: the screen stays, the sound goes, once.
		f.at(3_000)
		if res := f.stopCompanionAudio(t, "ann"); res.status != http.StatusNoContent || res.body != "" {
			t.Fatalf("DELETE: %d %q, want 204 and no body", res.status, res.body)
		}
		up = f.lastCompanionUpdate(t)
		if !up.canPublish || !up.canSubscribe || !slices.Equal(up.sources, screenOnly) {
			t.Fatalf("DELETE set %+v, want canPublish, canSubscribe and %v", up, screenOnly)
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.screen || p.screenAudio || !p.shareGranted {
			t.Errorf("after DELETE the companion has %+v, want the screen and no sound", p)
		}
		want = append(want, "ann audio_stopped principal=ann reason=self")
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("after DELETE share rows %q, want %q", got, want)
		}
		f.at(3_500)
		f.pass(t)
		if got := f.sharing(t, f.ops); got != "ann" {
			t.Errorf("presence shows %q, want ann still sharing without sound", got)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("the pass after DELETE audited %d unclaimed rows", n)
		}
		// Again, and by someone who holds nothing: nothing happens.
		updates := len(f.lk.updatesFor(f.screenID("ann")))
		for _, who := range []string{"ann", "ann2"} {
			if res := f.stopCompanionAudio(t, who); res.status != http.StatusNoContent {
				t.Errorf("%s's DELETE with nothing to withdraw: %d %s, want 204", who, res.status, res.body)
			}
		}
		if n := len(f.lk.updatesFor(f.screenID("ann"))); n != updates {
			t.Errorf("a DELETE with nothing to withdraw updated the companion")
		}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("after idle DELETEs share rows %q, want %q", got, want)
		}
		// The share itself is untouched: it ends by its own rule.
		if holder := f.shareState(room); holder != f.ids["ann"] {
			t.Errorf("the share is held by %d, want ann", holder)
		}
	})

	t.Run("a claim again keeps the consent", func(t *testing.T) {
		f, _ := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplication, share)
		f.mustClaim(t, "ann")
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenAndAudio) {
			t.Errorf("the claim again set %+v, want the consent kept: %v", up, screenAndAudio)
		}
		// A consent is answered as before: no new row.
		f.mustConsent(t, audioApplication, share)
		want := []string{"ann started audio=true", "ann audio_started scope=application"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
	})

	t.Run("a publish-only companion keeps its grant", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
			fakeParticipant{identity: f.screenID("ann"), joinedMs: 3_000, noSubscribe: true})
		f.at(100)
		f.pass(t)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplication, share)
		if up := f.lastCompanionUpdate(t); up.canSubscribe || !slices.Equal(up.sources, screenAndAudio) {
			t.Errorf("the grant set %+v, want canSubscribe false and %v", up, screenAndAudio)
		}
		if res := f.stopCompanionAudio(t, "ann"); res.status != http.StatusNoContent {
			t.Fatalf("DELETE: %d %s", res.status, res.body)
		}
		if up := f.lastCompanionUpdate(t); up.canSubscribe || !slices.Equal(up.sources, screenOnly) {
			t.Errorf("DELETE set %+v, want canSubscribe false and %v", up, screenOnly)
		}
	})

	t.Run("livekit refuses: nothing is recorded", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.lk.setUpdateFail(f.screenID("ann"), true)
		res := f.companionAudio(t, "ann", audioApplication)
		if res.status != http.StatusServiceUnavailable || errCodeIfAny(t, res.body) != schema.ErrorCodeVoiceUnavailable {
			t.Fatalf("got %d %s, want 503 voice_unavailable", res.status, res.body)
		}
		if n := len(f.audits(t, store.AuditVoiceShareAudioStarted)); n != 0 {
			t.Errorf("a refused grant wrote %d voice_share_audio_started rows", n)
		}
		// A pass meanwhile finds nothing to withdraw and nothing to audit.
		f.at(1_500)
		f.pass(t)
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("the pass audited %d unclaimed rows", n)
		}
		// Asking again, once LiveKit answers, is a first consent.
		f.lk.setUpdateFail(f.screenID("ann"), false)
		f.mustConsent(t, audioApplication, share)
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenAndAudio) {
			t.Errorf("the grant set %+v, want %v", up, screenAndAudio)
		}
		if n := len(f.audits(t, store.AuditVoiceShareAudioStarted)); n != 1 {
			t.Errorf("voice_share_audio_started rows = %d, want 1", n)
		}
		// The companion gone from LiveKit before a pass could say so.
		f.stopCompanionAudio(t, "ann")
		f.lk.drop(room, f.screenID("ann"))
		res = f.companionAudio(t, "ann", audioApplication)
		if res.status != http.StatusConflict || errCodeIfAny(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("with the companion gone: %d %s, want 409 voice_no_session", res.status, res.body)
		}
		if n := len(f.audits(t, store.AuditVoiceShareAudioStarted)); n != 1 {
			t.Errorf("voice_share_audio_started rows = %d, want still 1", n)
		}
	})

	t.Run("no secret in any row, line or body", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		token := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Token
		var bodies []string
		keep := func(res wireResult) { bodies = append(bodies, res.body) }
		f.at(1_000)
		f.mustClaim(t, "ann")
		// The sound published with no consent: withdrawn, unclaimed.
		f.publishScreen(room, true, true)
		f.at(1_200)
		f.pass(t)
		// A grant LiveKit refuses, then one it takes, a scope change and a
		// stranger's request.
		f.lk.setUpdateFail(f.screenID("ann"), true)
		keep(f.companionAudio(t, "ann", audioApplication))
		f.lk.setUpdateFail(f.screenID("ann"), false)
		keep(f.companionAudio(t, "ann", audioApplication))
		keep(f.companionAudio(t, "ann", audioSystem))
		keep(f.companionAudio(t, "ann2", audioApplication))
		f.publishScreen(room, true, true)
		f.at(2_000)
		f.pass(t)
		// A DELETE whose withdrawal fails while the sound is published: the
		// companion is removed.
		f.lk.setUpdateFail(f.screenID("ann"), true)
		keep(f.stopCompanionAudio(t, "ann"))
		f.lk.setUpdateFail(f.screenID("ann"), false)
		f.mustStop(t, "ann")

		var haystack strings.Builder
		for _, e := range f.audit(t) {
			fmt.Fprintf(&haystack, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
		}
		haystack.WriteString(f.logs.buf.String())
		for _, b := range bodies {
			haystack.WriteString(b)
		}
		text := haystack.String()
		for what, secret := range map[string]string{
			"join token": token, "join token body": strings.Split(token, ".")[1],
			"api secret": voiceTestSecret, "api key": voiceTestKey, "room name": room,
			"ann's login token": f.tokens["ann"],
		} {
			if strings.Contains(text, secret) {
				t.Errorf("%s appears in an audit row, log line or response", what)
			}
		}
		for _, exercised := range []string{
			store.AuditVoiceShareAudioStarted, store.AuditVoiceShareAudioStopped, store.AuditVoiceShareUnclaimed,
			"withdrawing a companion's share audio failed", "a companion's audio could not be granted",
		} {
			if !strings.Contains(text, exercised) {
				t.Errorf("the run did not exercise %q", exercised)
			}
		}
	})
}

// TestVoiceCompanionAudioEndsWithTheShare: every way a share ends also ends
// the consent, once, with the share's reason and actor: the companion is
// withdrawn whole (the sound with the screen) and voice_share_audio_stopped is
// written beside voice_share_stopped.
func TestVoiceCompanionAudioEndsWithTheShare(t *testing.T) {
	tests := []struct {
		name  string
		end   func(t *testing.T, f *presenceFixture, room string)
		actor string
		// roomGone says the end deleted the room: there is no companion left
		// to withdraw from.
		roomGone bool
	}{
		{name: "self", actor: "ann", end: func(t *testing.T, f *presenceFixture, _ string) { f.mustStop(t, "ann") }},
		{name: "operator", actor: "root", end: func(t *testing.T, f *presenceFixture, _ string) {
			if res := f.stopShare(t, "root", "ops"); res.status != http.StatusNoContent {
				t.Fatalf("root stops the share: %d %s", res.status, res.body)
			}
		}},
		{name: "left", actor: "ann", end: func(t *testing.T, f *presenceFixture, room string) {
			f.lk.drop(room, f.identity("ann"))
			f.at(3_000)
			f.pass(t)
		}},
		{name: "access_lost", actor: "ann", end: func(t *testing.T, f *presenceFixture, _ string) {
			f.revokeShareScreen(t)
			f.at(3_000)
			f.pass(t)
		}},
		{name: "rotated", actor: "ann", roomGone: true, end: func(t *testing.T, f *presenceFixture, _ string) {
			if code := f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok); code != http.StatusNoContent {
				t.Fatalf("remove ann from the channel: %d", code)
			}
		}},
		{name: "idle", actor: "ann", end: func(t *testing.T, f *presenceFixture, room string) {
			f.publishScreen(room, false, false)
			f.at(8_000)
			f.pass(t)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := shareWithCompanion(t)
			f.at(1_000)
			share := f.mustClaim(t, "ann")
			f.mustConsent(t, audioApplication, share)
			f.publishScreen(room, true, true)
			f.at(2_000)
			f.pass(t)
			if got := f.sharing(t, f.ops); got != "ann+audio" {
				t.Fatalf("before the end presence shows %q, want ann+audio", got)
			}
			tt.end(t, f, room)
			want := []string{
				"ann started audio=true", "ann audio_started scope=application",
				tt.actor + " audio_stopped principal=ann reason=" + tt.name,
				tt.actor + " stopped principal=ann reason=" + tt.name,
			}
			if got := f.shareRows(t); !slices.Equal(got, want) {
				t.Errorf("share rows %q, want %q", got, want)
			}
			if tt.roomGone {
				if f.lk.has(room) {
					t.Error("the room survives its rotation")
				}
			} else {
				up := f.lastCompanionUpdate(t)
				if up.canPublish || len(up.sources) != 0 {
					t.Errorf("the end left the companion %+v, want nothing to publish", up)
				}
				if p, ok := f.lk.participant(room, f.screenID("ann")); ok && (p.screenAudio || p.shareGranted) {
					t.Errorf("after the end the companion has %+v", p)
				}
			}
			if got := f.sharing(t, f.ops); got != "-" {
				t.Errorf("after the end presence shows %q", got)
			}
			// Once: a pass later writes nothing more, and a DELETE finds
			// nothing to withdraw.
			f.at(10_000)
			f.pass(t)
			f.stopCompanionAudio(t, "ann")
			if got := f.shareRows(t); !slices.Equal(got, want) {
				t.Errorf("after another pass share rows %q, want %q", got, want)
			}
			if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
				t.Errorf("unclaimed rows = %d, want 0", n)
			}
		})
	}
}

// TestVoiceCompanionAudioUnclaimed: a companion publishing the share's sound
// while its person holds the share with no consent recorded (a stale token
// after a withdrawal) has that one source withdrawn, the screen kept, and is
// audited voice_share_unclaimed with sources=screen_share_audio, once until
// the sound is gone; the share goes on and presence never shows the sound. A
// withdrawal LiveKit will not take removes the companion. The sound published
// again after a DELETE is the same case.
func TestVoiceCompanionAudioUnclaimed(t *testing.T) {
	unclaimedDetail := func(f *presenceFixture) string {
		return fmt.Sprintf("channel=%d sources=screen_share_audio participant=screen source=observed", f.ops.ID)
	}
	t.Run("withdrawn, the screen kept", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		f.mustClaim(t, "ann")
		grants := len(f.lk.updatesFor(f.screenID("ann")))
		f.publishScreen(room, true, true)
		f.at(1_500)
		f.pass(t)
		up := f.lastCompanionUpdate(t)
		if !up.canPublish || !up.canSubscribe || !slices.Equal(up.sources, screenOnly) {
			t.Fatalf("the pass set %+v, want canPublish, canSubscribe and %v", up, screenOnly)
		}
		if n := len(f.lk.updatesFor(f.screenID("ann"))); n != grants+1 {
			t.Errorf("companion updates = %d, want %d", n, grants+1)
		}
		if got := f.lk.updatesFor(f.identity("ann")); len(got) != 1 {
			t.Errorf("ann's own participant was updated %d times, want the claim's once", len(got))
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.screen || p.screenAudio || !p.shareGranted {
			t.Errorf("the companion has %+v, want the screen and no sound", p)
		}
		rows := f.audits(t, store.AuditVoiceShareUnclaimed)
		if len(rows) != 1 || rows[0].Actor != f.actor("ann") || rows[0].Detail != unclaimedDetail(f) {
			t.Fatalf("unclaimed rows = %+v, want one by ann with detail %q", rows, unclaimedDetail(f))
		}
		if got := f.sharing(t, f.ops); got != "ann" {
			t.Errorf("presence shows %q, want ann without sound", got)
		}
		if holder := f.shareState(room); holder != f.ids["ann"] {
			t.Errorf("the share is held by %d, want ann", holder)
		}
		// Once: the next pass finds the sound gone and writes nothing.
		f.at(2_000)
		f.pass(t)
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 1 {
			t.Errorf("unclaimed rows after another pass = %d, want 1", n)
		}
		if n := len(f.lk.updatesFor(f.screenID("ann"))); n != grants+1 {
			t.Errorf("the next pass updated the companion again (%d updates)", n)
		}
		// Published again (a new stale token): a new one.
		f.publishScreen(room, true, true)
		f.at(2_500)
		f.pass(t)
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 2 {
			t.Errorf("unclaimed rows after the sound came back = %d, want 2", n)
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.screen || p.screenAudio {
			t.Errorf("the companion has %+v, want the screen and no sound", p)
		}
		want := []string{"ann started audio=true", "ann unclaimed sources=screen_share_audio participant=screen source=observed", "ann unclaimed sources=screen_share_audio participant=screen source=observed"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
	})
	t.Run("not withdrawn: removed", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		f.mustClaim(t, "ann")
		f.publishScreen(room, true, true)
		f.lk.setUpdateStatus(http.StatusInternalServerError)
		f.at(1_500)
		f.pass(t)
		if f.lk.in(room, f.screenID("ann")) {
			t.Fatal("the companion is still connected with an unclaimed sound")
		}
		if !f.lk.in(room, f.identity("ann")) {
			t.Error("ann herself was removed")
		}
		want := fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s participant=screen source=observed", f.ops.ID, f.ids["ann"], voiceReasonShareUnclaimed)
		if got := f.removedRows(t); !slices.Equal(got, []string{want}) {
			t.Errorf("removal rows = %q, want %q", got, want)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 1 {
			t.Errorf("unclaimed rows = %d, want 1", n)
		}
	})
	t.Run("after a DELETE the sound published again is unclaimed", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplication, share)
		f.publishScreen(room, true, true)
		f.at(1_500)
		f.pass(t)
		if res := f.stopCompanionAudio(t, "ann"); res.status != http.StatusNoContent {
			t.Fatalf("DELETE: %d %s", res.status, res.body)
		}
		// A pass that lists the room after the withdrawal was confirmed and
		// still sees the sound: a stale token published it again.
		f.publishScreen(room, true, true)
		f.at(2_000)
		f.pass(t)
		rows := f.audits(t, store.AuditVoiceShareUnclaimed)
		if len(rows) != 1 || rows[0].Detail != unclaimedDetail(f) {
			t.Fatalf("unclaimed rows = %+v, want one with detail %q", rows, unclaimedDetail(f))
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.screen || p.screenAudio {
			t.Errorf("the companion has %+v, want the screen and no sound", p)
		}
		if got := f.sharing(t, f.ops); got != "ann" {
			t.Errorf("presence shows %q, want ann without sound", got)
		}
	})
	t.Run("a withdrawal that fails after a DELETE is retried quietly", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplication, share)
		f.publishScreen(room, true, false)
		f.at(1_500)
		f.pass(t)
		f.lk.setUpdateFail(f.screenID("ann"), true)
		if res := f.stopCompanionAudio(t, "ann"); res.status != http.StatusNoContent {
			t.Fatalf("DELETE: %d %s", res.status, res.body)
		}
		if !f.lk.in(room, f.screenID("ann")) {
			t.Fatal("a companion publishing no sound was removed")
		}
		f.lk.setUpdateFail(f.screenID("ann"), false)
		// The companion's permission still names the sound; the next pass
		// does not see a track, and does not need to: the withdrawal is
		// owed, and made on the next pass that lists the sound, without a
		// row.
		f.publishScreen(room, true, true)
		f.at(2_000)
		f.pass(t)
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenOnly) {
			t.Errorf("the pass set %+v, want %v", up, screenOnly)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("a withdrawal the DELETE owed was audited as unclaimed %d times", n)
		}
		want := []string{"ann started audio=true", "ann audio_started scope=application", "ann audio_stopped principal=ann reason=self"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
	})
	t.Run("the person's own sound needs no consent", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		f.mustClaim(t, "ann")
		f.publish(room, "ann", true, true)
		f.at(1_500)
		f.pass(t)
		if got := f.sharing(t, f.ops); got != "ann+audio" {
			t.Errorf("presence shows %q, want ann+audio", got)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("unclaimed rows = %d, want 0", n)
		}
	})
}

// holdCompanionAudioGrant makes the scripted LiveKit hold the companion's
// audio grant (the UpdateParticipant naming the screen for ann's companion)
// until release is closed, and closes granting once it is held. Set after the
// claim, whose own restoration of the companion names the screen too.
func holdCompanionAudioGrant(f *presenceFixture) (granting, release chan struct{}) {
	granting, release = make(chan struct{}), make(chan struct{})
	screen := f.screenID("ann")
	var once sync.Once
	f.lk.setOnUpdate(func(_, identity string, share bool) {
		if share && identity == screen {
			once.Do(func() {
				close(granting)
				<-release
			})
		}
	})
	return granting, release
}

// TestVoiceCompanionAudioRevocationWaitsForGrantInFlight (the security
// review's N2, the class #1265 closed for claimShare): the consent has passed
// its checks and its grant is inside LiveKit's answer when a revocation of
// ann's right or place commits. The revocation's stop path must find the
// consent running (in the room, or in a room its own rotation has just
// retired) and must not answer while the grant is in flight. When it
// answers, the companion is granted nothing in the old room and nobody holds
// the share, even where DeleteRoom failed and the old room is still there.
func TestVoiceCompanionAudioRevocationWaitsForGrantInFlight(t *testing.T) {
	for _, rev := range revocationCases() {
		t.Run(rev.name, func(t *testing.T) {
			f, room := shareWithCompanion(t)
			rev.setUp(f, room)
			f.at(1_000)
			f.mustClaim(t, "ann")
			granting, release := holdCompanionAudioGrant(f)
			waiting := make(chan int, 1)
			f.srv.voice.endSharesSeam = func(rooms int) {
				select {
				case waiting <- rooms: // the first call only
				default:
				}
			}
			consented := make(chan wireResult, 1)
			go func() { consented <- f.companionAudio(t, "ann", audioApplication) }()
			<-granting
			claimRoom := f.srv.voice.roomNamed(room)
			if claimRoom == nil {
				t.Fatal("the consent's room is not in the poller")
			}
			revoked := make(chan struct{})
			go func() {
				defer close(revoked)
				rev.do(t, f)
			}()
			// The revocation has committed and its hook has reached the stop
			// path; it must have found the consent running, and cannot answer
			// while the grant is held inside LiveKit.
			if n := <-waiting; n == 0 {
				t.Error("the revocation's stop path found no consent to wait for")
			}
			close(release)
			<-revoked
			f.srv.voice.mu.Lock()
			claimer := claimRoom.claimer
			f.srv.voice.mu.Unlock()
			if claimer != 0 {
				t.Errorf("the revocation answered while a consent of %d was still running", claimer)
			}
			// Read at the moment the revocation has answered.
			if p, ok := f.lk.participant(room, f.screenID("ann")); ok && (p.shareGranted || p.screenAudio) {
				t.Errorf("the companion is still granted %+v when the revocation answered", p)
			}
			if f.granted(t, room, "ann") {
				t.Error("ann's screen is still granted when the revocation answered")
			}
			if holder := f.shareState(room); holder != 0 {
				t.Errorf("the poller still holds a share for %d", holder)
			}
			res := <-consented
			if strings.HasPrefix(rev.name, "permissions PUT") {
				// The grant landed before the revocation ended the share, and
				// the revocation took it back: a consent that started and
				// stopped with the share.
				if res.status != http.StatusOK {
					t.Errorf("the consent answered %d %s, want 200", res.status, res.body)
				}
				want := []string{"ann started audio=true", "ann audio_started scope=application",
					"ann audio_stopped principal=ann reason=access_lost", "ann stopped principal=ann reason=access_lost"}
				if got := f.shareRows(t); !slices.Equal(got, want) {
					t.Errorf("share rows %q, want %q", got, want)
				}
			} else if res.status == http.StatusOK {
				t.Errorf("the consent was answered 200 after its room was rotated: %s", res.body)
			}
			f.srv.voice.mu.Lock()
			left := len(f.srv.voice.retiring)
			f.srv.voice.mu.Unlock()
			if left != 0 {
				t.Errorf("%d retired rooms are still held for their claims", left)
			}
		})
	}
}

// TestVoiceCompanionAudioShareEndsMidGrant (the security review's N1): a pass
// ends the share (idle) while the consent's grant is inside LiveKit. The
// consent never started, so the share's end writes voice_share_stopped alone,
// with no orphan voice_share_audio_stopped; the consent is refused as for a
// share that is gone, and what LiveKit granted is taken back.
func TestVoiceCompanionAudioShareEndsMidGrant(t *testing.T) {
	f, room := shareWithCompanion(t)
	f.at(1_000)
	f.mustClaim(t, "ann")
	granting, release := holdCompanionAudioGrant(f)
	consented := make(chan wireResult, 1)
	go func() { consented <- f.companionAudio(t, "ann", audioApplication) }()
	<-granting
	// Nothing was ever published: at 16 s the pass ends the share idle. Its
	// withdrawal waits on the room's claims, which the consent holds.
	f.at(16_100)
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		f.pass(t)
	}()
	waitUntil(t, "the pass to end the share", func() bool { return f.shareState(room) == 0 })
	close(release)
	res := <-consented
	<-passed
	if res.status != http.StatusConflict || errCodeIfAny(t, res.body) != schema.ErrorCodeVoiceNoSession {
		t.Errorf("the consent answered %d %s, want 409 voice_no_session", res.status, res.body)
	}
	want := []string{"ann started audio=true", "ann stopped principal=ann reason=idle"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q: no audio row for a consent that never started", got, want)
	}
	if p, ok := f.lk.participant(room, f.screenID("ann")); !ok || p.shareGranted || p.screenAudio {
		t.Errorf("after the end the companion is %+v (listed %v), want it there with nothing granted", p, ok)
	}
	// Nothing lingers: a later pass writes nothing more, and a DELETE finds
	// nothing to withdraw.
	f.at(17_000)
	f.pass(t)
	f.stopCompanionAudio(t, "ann")
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("after another pass share rows %q, want %q", got, want)
	}
}
