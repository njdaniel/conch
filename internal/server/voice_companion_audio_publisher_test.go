package server

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the consent that names its publisher (issue #1853,
// docs/design/linux-share-audio.md §8, decision D-3): a consent with
// publisher: person is recorded and audited and asks LiveKit nothing, the
// companion's own sound under it is unclaimed, and every end of the share
// closes it as before.

const (
	audioApplicationPerson    = `{"scope":"application","publisher":"person"}`
	audioApplicationCompanion = `{"scope":"application","publisher":"companion"}`
	audioSystemPerson         = `{"scope":"system_excluding_call","publisher":"person"}`
)

// consentBody asks for the share's sound as ann and returns the raw answer,
// requiring 200.
func (f *presenceFixture) consentBody(t *testing.T, body string) string {
	t.Helper()
	res := f.companionAudio(t, "ann", body)
	if res.status != http.StatusOK {
		t.Fatalf("ann consents with %s: %d %s, want 200", body, res.status, res.body)
	}
	return res.body
}

func (f *presenceFixture) companionUpdates() int {
	return len(f.lk.updatesFor(f.screenID("ann")))
}

func TestVoiceCompanionAudioPublisherRefusals(t *testing.T) {
	tests := []struct {
		name   string
		who    string
		body   string
		status int
		code   string
	}{
		{"unknown publisher", "ann", `{"scope":"application","publisher":"both"}`, http.StatusBadRequest, "invalid_request"},
		{"wrong case", "ann", `{"scope":"application","publisher":"Person"}`, http.StatusBadRequest, "invalid_request"},
		{"publisher not a string", "ann", `{"scope":"application","publisher":1}`, http.StatusBadRequest, "invalid_request"},
		{"an older body shape with the field misspelt", "ann", `{"scope":"application","publishers":"person"}`, http.StatusBadRequest, "invalid_request"},
		{"non-member", "bob", audioApplicationPerson, http.StatusNotFound, "channel_not_found"},
		{"not a person", "robo", audioApplicationPerson, http.StatusForbidden, ""},
		{"not the holder", "ann2", audioApplicationPerson, http.StatusConflict, schema.ErrorCodeVoiceNoSession},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := shareWithCompanion(t)
			f.at(1_000)
			f.mustClaim(t, "ann")
			updates := f.lk.count("UpdateParticipant")
			res := f.companionAudio(t, tt.who, tt.body)
			if res.status != tt.status || (tt.code != "" && errCodeIfAny(t, res.body) != tt.code) {
				t.Fatalf("%s with %q: %d %s, want %d %s", tt.who, tt.body, res.status, res.body, tt.status, tt.code)
			}
			if n := f.lk.count("UpdateParticipant"); n != updates {
				t.Errorf("a refusal asked LiveKit (%d calls, had %d)", n, updates)
			}
			if n := len(f.audits(t, store.AuditVoiceShareAudioStarted)); n != 0 {
				t.Errorf("a refusal wrote %d voice_share_audio_started rows", n)
			}
		})
	}
	t.Run("a share nobody holds", func(t *testing.T) {
		f, _ := shareWithCompanion(t)
		res := f.companionAudio(t, "ann", audioApplicationPerson)
		if res.status != http.StatusConflict || errCodeIfAny(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("got %d %s, want 409 voice_no_session", res.status, res.body)
		}
	})
	t.Run("the companion not in the room", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		f.mustClaim(t, "ann")
		f.lk.drop(room, f.screenID("ann"))
		f.at(2_000)
		f.pass(t)
		res := f.companionAudio(t, "ann", audioApplicationPerson)
		if res.status != http.StatusConflict || errCodeIfAny(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("got %d %s, want 409 voice_no_session", res.status, res.body)
		}
	})
}

func TestVoiceCompanionAudioPublisher(t *testing.T) {
	t.Run("absent means companion, byte for byte", func(t *testing.T) {
		f, _ := shareWithCompanion(t)
		f.at(1_000)
		f.mustClaim(t, "ann")
		body := f.consentBody(t, audioApplication)
		if strings.Contains(body, "publisher") {
			t.Errorf("an answer to a request naming no publisher carries one: %s", body)
		}
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenAndAudio) {
			t.Errorf("the grant set %+v, want %v", up, screenAndAudio)
		}
		want := []string{"ann started audio=true", "ann audio_started scope=application"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
	})

	t.Run("companion named: echoed, the same grant and row", func(t *testing.T) {
		f, _ := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		audio := f.mustConsent(t, audioApplicationCompanion, share)
		if audio.Publisher != schema.VoiceShareAudioPublisherCompanion {
			t.Errorf("publisher = %q, want companion echoed", audio.Publisher)
		}
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenAndAudio) {
			t.Errorf("the grant set %+v, want %v", up, screenAndAudio)
		}
		want := []string{"ann started audio=true", "ann audio_started scope=application"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
		// Absent and companion are the same consent: a no-op.
		f.consentBody(t, audioApplication)
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("after the same consent without the field share rows %q, want %q", got, want)
		}
	})

	t.Run("person: recorded, LiveKit not asked, the companion stays at the screen", func(t *testing.T) {
		f, _ := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		calls := f.lk.count("UpdateParticipant")
		companion := f.companionUpdates()
		f.at(1_500)
		audio := f.mustConsent(t, audioApplicationPerson, share)
		if audio.Publisher != schema.VoiceShareAudioPublisherPerson {
			t.Errorf("publisher = %q, want person", audio.Publisher)
		}
		if n := f.lk.count("UpdateParticipant"); n != calls {
			t.Errorf("UpdateParticipant calls = %d, want the claim's %d: LiveKit must not be asked", n, calls)
		}
		if n := f.companionUpdates(); n != companion {
			t.Errorf("the companion was updated %d times, want %d", n, companion)
		}
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenOnly) {
			t.Errorf("the companion's grant is %+v, want %v", up, screenOnly)
		}
		want := []string{"ann started audio=true", "ann audio_started scope=application publisher=person"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}

		// A repeat is a no-op 200.
		f.consentBody(t, audioApplicationPerson)
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("after a repeat share rows %q, want %q", got, want)
		}
		if n := f.lk.count("UpdateParticipant"); n != calls {
			t.Errorf("a repeat asked LiveKit (%d calls)", n)
		}
		// Another scope: recorded, a row, still no LiveKit.
		f.mustConsent(t, audioSystemPerson, share)
		want = append(want, "ann audio_started scope=system_excluding_call publisher=person")
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("after a scope change share rows %q, want %q", got, want)
		}
		if n := f.lk.count("UpdateParticipant"); n != calls {
			t.Errorf("a scope change asked LiveKit (%d calls)", n)
		}
	})

	t.Run("person: the sound is the person's, the companion's is unclaimed", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplicationPerson, share)
		unclaimed := fmt.Sprintf("channel=%d sources=screen_share_audio participant=screen source=observed", f.ops.ID)

		// The person's own sound counts, consent or not.
		f.publish(room, "ann", true, true)
		f.at(1_500)
		f.pass(t)
		if got := f.sharing(t, f.ops); got != "ann+audio" {
			t.Errorf("presence shows %q, want ann+audio from the person's own track", got)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("the person's sound was audited as unclaimed %d times", n)
		}

		// The companion's sound under a person consent is withdrawn, once.
		f.publishScreen(room, true, true)
		f.at(2_000)
		f.pass(t)
		rows := f.audits(t, store.AuditVoiceShareUnclaimed)
		if len(rows) != 1 || rows[0].Actor != f.actor("ann") || rows[0].Detail != unclaimed {
			t.Fatalf("unclaimed rows = %+v, want one by ann with detail %q", rows, unclaimed)
		}
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenOnly) {
			t.Errorf("the pass set %+v, want %v", up, screenOnly)
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.screen || p.screenAudio {
			t.Errorf("the companion has %+v, want the screen and no sound", p)
		}
		updates := f.companionUpdates()
		f.at(2_500)
		f.pass(t)
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 1 {
			t.Errorf("unclaimed rows after another pass = %d, want once", n)
		}
		if n := f.companionUpdates(); n != updates {
			t.Errorf("the next pass updated the companion again (%d updates)", n)
		}
		// Claiming again, or consenting again, never restores audio to it.
		f.mustClaim(t, "ann")
		f.consentBody(t, audioApplicationPerson)
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenOnly) {
			t.Errorf("a claim again set %+v, want %v", up, screenOnly)
		}
		f.publishScreen(room, true, true)
		f.at(3_000)
		f.pass(t)
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 2 {
			t.Errorf("unclaimed rows after the sound came back = %d, want 2", n)
		}
	})

	t.Run("person: a companion that cannot be withdrawn is removed", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplicationPerson, share)
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
	})

	t.Run("person: DELETE clears the consent", func(t *testing.T) {
		f, _ := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplicationPerson, share)
		f.at(2_000)
		if res := f.stopCompanionAudio(t, "ann"); res.status != http.StatusNoContent || res.body != "" {
			t.Fatalf("DELETE: %d %q, want 204 and no body", res.status, res.body)
		}
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenOnly) {
			t.Errorf("DELETE set %+v, want %v", up, screenOnly)
		}
		want := []string{
			"ann started audio=true", "ann audio_started scope=application publisher=person",
			"ann audio_stopped principal=ann reason=self",
		}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
		// Once: another DELETE finds nothing.
		f.stopCompanionAudio(t, "ann")
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("after another DELETE share rows %q, want %q", got, want)
		}
		// A consent after it is a first consent again, with the publisher
		// forgotten: the companion's default applies.
		f.mustConsent(t, audioApplication, share)
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenAndAudio) {
			t.Errorf("a companion consent after the DELETE set %+v, want %v", up, screenAndAudio)
		}
	})

	t.Run("changing the publisher on a live consent", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplication, share)
		want := []string{"ann started audio=true", "ann audio_started scope=application"}

		// companion -> person: a row, and the companion's sound withdrawn.
		f.publishScreen(room, true, true)
		f.at(1_500)
		f.pass(t)
		f.mustConsent(t, audioApplicationPerson, share)
		want = append(want, "ann audio_started scope=application publisher=person")
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenOnly) {
			t.Errorf("companion -> person set %+v, want %v", up, screenOnly)
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); p.screenAudio {
			t.Error("companion -> person left the companion publishing the sound")
		}

		// person -> companion: a row, and the companion granted the sound.
		f.mustConsent(t, audioApplicationCompanion, share)
		want = append(want, "ann audio_started scope=application")
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
		if up := f.lastCompanionUpdate(t); !up.canPublish || !slices.Equal(up.sources, screenAndAudio) {
			t.Errorf("person -> companion set %+v, want %v", up, screenAndAudio)
		}
		f.publishScreen(room, true, true)
		f.at(2_000)
		f.pass(t)
		if got := f.sharing(t, f.ops); got != "ann+audio" {
			t.Errorf("presence shows %q, want ann+audio under the companion's consent", got)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("a consented sound was audited as unclaimed %d times", n)
		}

		// The share's end closes it once, with the one stopped row.
		f.at(3_000)
		f.mustStop(t, "ann")
		want = append(want, "ann audio_stopped principal=ann reason=self", "ann stopped principal=ann reason=self")
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
	})

	t.Run("person to companion with LiveKit refusing: the person consent stands", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		share := f.mustClaim(t, "ann")
		f.mustConsent(t, audioApplicationPerson, share)
		f.lk.setUpdateFail(f.screenID("ann"), true)
		res := f.companionAudio(t, "ann", audioApplicationCompanion)
		if res.status != http.StatusServiceUnavailable || errCodeIfAny(t, res.body) != schema.ErrorCodeVoiceUnavailable {
			t.Fatalf("got %d %s, want 503 voice_unavailable", res.status, res.body)
		}
		f.lk.setUpdateFail(f.screenID("ann"), false)
		want := []string{"ann started audio=true", "ann audio_started scope=application publisher=person"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
		// The person consent still governs: whatever LiveKit may have taken
		// of the grant is withdrawn by the passes, quietly (owed, as on the
		// companion path), and the consent still ends with a stopped row.
		f.publishScreen(room, true, true)
		f.at(1_500)
		f.pass(t)
		if up := f.lastCompanionUpdate(t); !slices.Equal(up.sources, screenOnly) {
			t.Errorf("the pass set %+v, want %v", up, screenOnly)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("unclaimed rows = %d, want 0: the withdrawal was owed", n)
		}
		f.at(2_000)
		f.mustStop(t, "ann")
		want = append(want,
			"ann audio_stopped principal=ann reason=self", "ann stopped principal=ann reason=self")
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
	})
}

// TestVoiceCompanionAudioPersonEndsWithTheShare: every way a share ends also
// ends a consent naming the person, once, with the share's reason and actor,
// and the stopped row precedes voice_share_stopped. The person's own sound is
// the share's until it ends.
func TestVoiceCompanionAudioPersonEndsWithTheShare(t *testing.T) {
	tests := []struct {
		name  string
		end   func(t *testing.T, f *presenceFixture, room string)
		actor string
	}{
		{name: "self", actor: "ann", end: func(t *testing.T, f *presenceFixture, _ string) { f.mustStop(t, "ann") }},
		{name: "operator", actor: "root", end: func(t *testing.T, f *presenceFixture, _ string) {
			if res := f.stopShareOf(t, "root", "ann"); res.status != http.StatusNoContent {
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
		{name: "rotated", actor: "ann", end: func(t *testing.T, f *presenceFixture, _ string) {
			if code := f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok); code != http.StatusNoContent {
				t.Fatalf("remove ann from the channel: %d", code)
			}
		}},
		{name: "idle", actor: "ann", end: func(t *testing.T, f *presenceFixture, room string) {
			f.publish(room, "ann", false, false)
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
			f.mustConsent(t, audioApplicationPerson, share)
			f.publish(room, "ann", true, true)
			f.at(2_000)
			f.pass(t)
			if got := f.sharing(t, f.ops); got != "ann+audio" {
				t.Fatalf("before the end presence shows %q, want ann+audio", got)
			}
			tt.end(t, f, room)
			want := []string{
				"ann started audio=true", "ann audio_started scope=application publisher=person",
				tt.actor + " audio_stopped principal=ann reason=" + tt.name,
				tt.actor + " stopped principal=ann reason=" + tt.name,
			}
			if got := f.shareRows(t); !slices.Equal(got, want) {
				t.Errorf("share rows %q, want %q", got, want)
			}
			if got := f.sharing(t, f.ops); got != "-" {
				t.Errorf("after the end presence shows %q", got)
			}
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
