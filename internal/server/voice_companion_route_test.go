package server

import (
	"context"
	"database/sql"
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

// Tests of the route that issues the companion and the viewer (issue #933,
// docs/design/voice-control-plane.md §13): POST and DELETE
// /v1/channels/{channel}/voice/companion. The poller's side, folding them into
// the person, is voice_companion_test.go (issue #932). Everything here runs
// against the scripted LiveKit and the fixture's manual clock.

const (
	companionScreen     = `{"publish":"screen","subscribe":true}`
	companionPublish    = `{"publish":"screen","subscribe":false}`
	companionViewerBody = `{"publish":"none","subscribe":true}`
)

func (f *presenceFixture) companion(t *testing.T, who, channel, body string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPost, "/v1/channels/"+channel+"/voice/companion", f.tokens[who], body)
}

func (f *presenceFixture) removeCompanion(t *testing.T, who, query string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodDelete, "/v1/channels/ops/voice/companion"+query, f.tokens[who], "")
}

func decodeCompanion(t *testing.T, res wireResult) schema.VoiceCompanionResponseV1 {
	t.Helper()
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", res.status, res.body)
	}
	var resp schema.VoiceCompanionResponseV1
	if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("response does not validate: %v; body %s", err, res.body)
	}
	return resp
}

// holders lists the recorded holders of ops' room as "principal/credential".
func (f *presenceFixture) holders(t *testing.T) []string {
	t.Helper()
	room, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := f.srv.store.VoiceRoomHolders(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range hs {
		out = append(out, fmt.Sprintf("%d/%d", h.PrincipalID, h.CredentialID))
	}
	return out
}

// companionConnects makes the scripted LiveKit list ann's companion beside the participants already in the room, as it does
// once the companion has used its token, and lets a pass see it.
func (f *presenceFixture) companionConnects(t *testing.T, room string) {
	t.Helper()
	parts := []fakeParticipant{
		mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
		{identity: f.screenID("ann"), joinedMs: 3_000, shareGranted: true},
	}
	f.lk.setRoom(room, parts...)
	f.at(100)
	f.pass(t)
}

// noCompanionTraffic checks that a refusal issued nothing: no grant audit
// row, no companion removal row, no CreateRoom, and no new holder.
func noCompanionTraffic(t *testing.T, f *presenceFixture, holdersBefore []string, createsBefore int) {
	t.Helper()
	if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 2 { // the two sessions the fixture asked for
		t.Errorf("voice_session_issued rows = %d, want only the fixture's two", n)
	}
	if n := len(f.audits(t, store.AuditVoiceCompanionRemoved)); n != 0 {
		t.Errorf("voice_companion_removed rows = %d, want 0", n)
	}
	if n := f.lk.count("CreateRoom"); n != createsBefore {
		t.Errorf("CreateRoom calls = %d, want %d", n, createsBefore)
	}
	if got := f.holders(t); !slices.Equal(got, holdersBefore) {
		t.Errorf("holders = %v, want %v", got, holdersBefore)
	}
}

// TestVoiceCompanionRefusals runs the refusals of the POST in the order of
// the design (§13): each row is made to fail one check while every earlier
// check passes and, where the order matters, a later check would fail too, so
// that the answer is the first. Every refusal issues nothing.
func TestVoiceCompanionRefusals(t *testing.T) {
	tests := []struct {
		name       string
		who        string
		channel    string
		body       string
		wantStatus int
		wantCode   string
		denied     int // access_denied audit rows
	}{
		// 1. a verified caller
		{"unauthenticated", "", "ops", companionScreen, 401, "unauthenticated", 0},
		// 2. the channel exists and the caller is a member, whatever the body
		{"non-member", "bob", "ops", companionScreen, 404, "channel_not_found", 0},
		{"non-member, malformed body", "bob", "ops", `{"publish":`, 404, "channel_not_found", 0},
		{"non-member, unknown field", "bob", "ops", `{"publish":"screen","subscribe":true,"x":1}`, 404, "channel_not_found", 0},
		{"unknown channel", "ann", "nosuch", companionScreen, 404, "channel_not_found", 0},
		{"operator who is not a member", "root", "ops", companionScreen, 404, "channel_not_found", 0},
		{"member of another channel", "ann2", "ops2", companionScreen, 404, "channel_not_found", 0},
		// 3. the caller is human, whatever the body: audited
		{"agent member", "robo", "ops", companionScreen, 403, "forbidden", 1},
		{"agent member, malformed body", "robo", "ops", `{`, 403, "forbidden", 1},
		// 6. the body (voice is configured, so 5 passed)
		{"empty body", "ann", "ops", ``, 400, "invalid_request", 0},
		{"malformed", "ann", "ops", `{"publish":`, 400, "invalid_request", 0},
		{"null", "ann", "ops", `null`, 400, "invalid_request", 0},
		{"unknown field", "ann", "ops", `{"publish":"screen","subscribe":true,"identity":"p1.screen"}`, 400, "invalid_request", 0},
		{"two objects", "ann", "ops", companionScreen + companionScreen, 400, "invalid_request", 0},
		{"camera", "ann", "ops", `{"publish":"camera","subscribe":true}`, 400, "invalid_request", 0},
		{"viewer that cannot subscribe", "ann", "ops", `{"publish":"none","subscribe":false}`, 400, "invalid_request", 0},
		{"the body is read before the room is looked for", "ann", "ops2", `{"subscribe":true}`, 400, "invalid_request", 0},
		// 8. a room someone holds a session for: ops2 has none
		{"no room", "ann", "ops2", companionScreen, 409, "voice_no_session", 0},
		{"no room, viewer", "ann", "ops2", companionViewerBody, 409, "voice_no_session", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newShareFixture(t)
			holders, creates := f.holders(t), f.lk.count("CreateRoom")
			res := f.companion(t, tt.who, tt.channel, tt.body)
			if res.status != tt.wantStatus || errCode(t, res.body) != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if strings.Contains(res.body, `"token"`) || strings.Contains(res.body, "eyJ") {
				t.Errorf("a refusal carries a token: %s", res.body)
			}
			noCompanionTraffic(t, f, holders, creates)
			if got := len(f.audits(t, "access_denied")); got != tt.denied {
				t.Errorf("access_denied rows = %d, want %d", got, tt.denied)
			}
		})
	}
}

// TestVoiceCompanionNotFoundIsIdentical: whatever a non-member sends, and
// whether or not voice is configured, the answer is the unknown-channel 404,
// byte for byte; the body is not read before membership is known.
func TestVoiceCompanionNotFoundIsIdentical(t *testing.T) {
	bodies := []string{companionScreen, companionViewerBody, `{"publish":`, ``, `null`, `{"publish":"camera"}`}
	for _, o := range []presenceOpts{{auth: AuthRequired}, {auth: AuthRequired, unconfigured: true}} {
		f := newPresenceFixture(t, o)
		want := f.companion(t, "ann", "nosuch", companionScreen)
		if want.status != 404 || errCode(t, want.body) != "channel_not_found" {
			t.Fatalf("unknown channel answer = %+v", want)
		}
		for _, who := range []string{"bob", "root"} {
			for _, body := range bodies {
				if got := f.companion(t, who, "ops", body); got != want {
					t.Errorf("%s with %q (unconfigured=%v): %+v differs from the unknown-channel answer %+v", who, body, o.unconfigured, got, want)
				}
			}
			if got := f.removeCompanion(t, who, "?publish=bogus"); got != want {
				t.Errorf("DELETE by %s: %+v differs from the unknown-channel answer %+v", who, got, want)
			}
		}
	}
}

// TestVoiceCompanionNotConfigured: a member is told voice is not set up before
// the body is read; nothing is made.
func TestVoiceCompanionNotConfigured(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired, unconfigured: true})
	for _, body := range []string{companionScreen, `{"publish":`} {
		res := f.companion(t, "ann", "ops", body)
		if res.status != 503 || errCode(t, res.body) != schema.ErrorCodeVoiceNotConfigured {
			t.Errorf("body %q: %d %s, want 503 voice_not_configured", body, res.status, res.body)
		}
	}
	if res := f.removeCompanion(t, "ann", ""); res.status != 503 || errCode(t, res.body) != schema.ErrorCodeVoiceNotConfigured {
		t.Errorf("DELETE: %d %s, want 503 voice_not_configured", res.status, res.body)
	}
}

// TestVoiceCompanionNeedsTheParent: a companion joins the person's connection.
func TestVoiceCompanionNeedsTheParent(t *testing.T) {
	t.Run("the person is not connected", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.lk.drop(room, f.identity("ann"))
		f.at(100)
		f.pass(t)
		holders, creates := f.holders(t), f.lk.count("CreateRoom")
		res := f.companion(t, "ann", "ops", companionScreen)
		if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("got %d %s, want 409 voice_no_session", res.status, res.body)
		}
		noCompanionTraffic(t, f, holders, creates)
	})
	t.Run("only as a viewer", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann2"), fakeParticipant{identity: f.viewID("ann"), joinedMs: 3_000})
		f.at(100)
		f.pass(t)
		if res := f.companion(t, "ann", "ops", companionScreen); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("got %d %s, want 409 voice_no_session: a viewer is not the person's connection", res.status, res.body)
		}
	})
	t.Run("evicted since the last pass", func(t *testing.T) {
		f, _ := newShareFixture(t)
		f.srv.voice.forget(context.Background(), f.room(t, f.ops), f.ids["ann"])
		if res := f.companion(t, "ann", "ops", companionScreen); res.status != http.StatusConflict {
			t.Fatalf("got %d %s, want 409", res.status, res.body)
		}
	})
	t.Run("publish none needs no parent", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann2"))
		f.at(100)
		f.pass(t)
		resp := decodeCompanion(t, f.companion(t, "ann", "ops", companionViewerBody))
		if resp.Identity != f.viewID("ann") || resp.Publish != schema.VoiceCompanionPublishNone {
			t.Errorf("response = %+v", resp)
		}
	})
}

// TestVoiceCompanionBusy: one companion per person; the viewer is displaced
// by a second one at LiveKit and so never refused.
func TestVoiceCompanionBusy(t *testing.T) {
	f, room := newShareFixture(t)
	decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	// Until the companion is seen in the room a second request is answered as
	// the first (the window the design records, §13); once a pass lists it, 409.
	f.companionConnects(t, room)
	holders, creates := f.holders(t), f.lk.count("CreateRoom")
	for _, body := range []string{companionScreen, companionPublish} {
		res := f.companion(t, "ann", "ops", body)
		if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCompanionBusy {
			t.Fatalf("second companion %s: %d %s, want 409 voice_companion_busy", body, res.status, res.body)
		}
		if strings.Contains(res.body, f.screenID("ann")) || strings.Contains(res.body, room) {
			t.Errorf("the refusal names an identity or the room: %s", res.body)
		}
	}
	if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 3 {
		t.Errorf("voice_session_issued rows = %d, want the two sessions and one companion", n)
	}
	if n := f.lk.count("CreateRoom"); n != creates {
		t.Errorf("a refused companion asked LiveKit for a room")
	}
	if got := f.holders(t); !slices.Equal(got, holders) {
		t.Errorf("holders changed on a refusal: %v -> %v", holders, got)
	}
	// Another person's companion is not mine.
	f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
		fakeParticipant{identity: f.screenID("ann2"), joinedMs: 3_000})
	f.at(2_000) // past the pass gap since the token above
	f.pass(t)
	decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	f.companionConnects(t, room)

	// Publish none: not refused, with a viewer present or not.
	f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
		fakeParticipant{identity: f.viewID("ann"), joinedMs: 3_000})
	f.at(2_300)
	f.pass(t)
	if resp := decodeCompanion(t, f.companion(t, "ann", "ops", companionViewerBody)); resp.Identity != f.viewID("ann") {
		t.Errorf("second viewer identity = %q", resp.Identity)
	}
}

// TestVoiceCompanionDeleteFrees: after DELETE (or the companion leaving and a
// pass noticing) a new request succeeds.
func TestVoiceCompanionDeleteFrees(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.companionConnects(t, room)
		if res := f.companion(t, "ann", "ops", companionScreen); res.status != http.StatusConflict {
			t.Fatalf("while present: %d, want 409", res.status)
		}
		if res := f.removeCompanion(t, "ann", ""); res.status != http.StatusNoContent {
			t.Fatalf("DELETE = %d %s", res.status, res.body)
		}
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	})
	t.Run("left by itself, DELETE before any pass noticed", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.companionConnects(t, room)
		f.lk.drop(room, f.screenID("ann"))
		if res := f.removeCompanion(t, "ann", ""); res.status != http.StatusNoContent {
			t.Fatalf("DELETE = %d %s", res.status, res.body)
		}
		if n := len(f.audits(t, store.AuditVoiceCompanionRemoved)); n != 0 {
			t.Errorf("voice_companion_removed rows = %d: nothing was removed", n)
		}
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	})
	t.Run("left and a pass noticed", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.companionConnects(t, room)
		f.lk.drop(room, f.screenID("ann"))
		f.at(200)
		f.pass(t)
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	})
}

// TestVoiceCompanionGrantsTheMember decodes the signed token of each role and
// compares the whole claim set, and checks the person's own token is as it
// was.
func TestVoiceCompanionGrantsTheMember(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		identity     func(f *presenceFixture) string
		wantSubTail  string
		wantPublish  schema.VoiceCompanionPublish
		wantSub      bool
		canPublish   bool
		wantSources  []any
		wantAuditing string
	}{
		{"companion that subscribes", companionScreen, func(f *presenceFixture) string { return f.screenID("ann") }, ".screen",
			schema.VoiceCompanionPublishScreen, true, true, []any{"screen_share"}, "companion:screen:subscribe"},
		{"publish-only companion", companionPublish, func(f *presenceFixture) string { return f.screenID("ann") }, ".screen",
			schema.VoiceCompanionPublishScreen, false, true, []any{"screen_share"}, "companion:screen:publish_only"},
		{"viewer", companionViewerBody, func(f *presenceFixture) string { return f.viewID("ann") }, ".view",
			schema.VoiceCompanionPublishNone, true, false, []any{}, "companion:view"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			before := time.Now()
			rec := f.do(t, http.MethodPost, "/v1/channels/ops/voice/companion", f.tokens["ann"], tt.body)
			after := time.Now()
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
			}
			resp := decodeCompanion(t, wireResult{rec.Code, rec.Body.String(), rec.Header().Get("Content-Type")})
			if resp.LivekitURL != voiceTestURL || resp.Room != room {
				t.Errorf("url/room = %q/%q", resp.LivekitURL, resp.Room)
			}
			if want := tt.identity(f); resp.Identity != want {
				t.Errorf("identity = %q, want %q", resp.Identity, want)
			}
			if resp.Publish != tt.wantPublish || resp.Subscribe != tt.wantSub {
				t.Errorf("echo = %s/%v, want %s/%v", resp.Publish, resp.Subscribe, tt.wantPublish, tt.wantSub)
			}

			claims := decodeVoiceJWT(t, resp.Token, voiceTestSecret)
			nbf, exp := int64(claims["nbf"].(float64)), int64(claims["exp"].(float64))
			wantVideo := map[string]any{
				"roomJoin": true, "room": room, "canSubscribe": tt.wantSub,
				"canPublish": tt.canPublish, "canPublishData": false, "canPublishSources": tt.wantSources,
			}
			if got := claims["video"]; fmt.Sprint(got) != fmt.Sprint(wantVideo) {
				t.Errorf("video = %v, want %v", got, wantVideo)
			}
			checks := []struct {
				name string
				ok   bool
			}{
				{"iss is the configured key", claims["iss"] == voiceTestKey},
				{"sub is the identity", claims["sub"] == resp.Identity},
				{"expires 15 seconds after issue", exp-nbf == 15},
				{"not-before is the issue time", nbf >= before.Unix() && nbf <= after.Unix()},
				{"expires_at is the issue time plus 15 s, whole seconds", resp.ExpiresAt.Time().Nanosecond() == 0 &&
					resp.ExpiresAt.Time().Unix() >= before.Unix()+15 && resp.ExpiresAt.Time().Unix() <= after.Unix()+15},
				{"expires_at is never later than the token's expiry", resp.ExpiresAt.Time().Unix() <= exp},
			}
			for _, c := range checks {
				if !c.ok {
					t.Errorf("%s: claims %v, expires_at %v", c.name, claims, resp.ExpiresAt)
				}
			}
			// The holder was recorded for the caller's credential.
			want := fmt.Sprintf("%d/%d", f.ids["ann"], f.credID(t, "ann"))
			if !slices.Contains(f.holders(t), want) {
				t.Errorf("holders %v lack %s", f.holders(t), want)
			}
			// One audit row, with the identity and the grant, and no secret.
			rows := f.audits(t, store.AuditVoiceSessionIssued)
			last := rows[len(rows)-1]
			wantDetail := fmt.Sprintf("channel=%d identity=%s grants=%s", f.ops.ID, resp.Identity, tt.wantAuditing)
			if len(rows) != 3 || last.Actor != f.actor("ann") || last.Subject != fmt.Sprintf("channel:%d", f.ops.ID) || last.Detail != wantDetail {
				t.Errorf("audit rows = %d, last %+v, want 3 and detail %q", len(rows), last, wantDetail)
			}
		})
	}
}

// TestVoicePersonTokenIsUnchanged: the person's own token and session keep the
// claims they had before companions existed, and the session names the
// companion's identity.
func TestVoicePersonTokenIsUnchanged(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	res := f.session(t, "ann", "ops")
	resp := decodeSession(t, res)
	room := resp.Rooms[0].Room
	claims := decodeVoiceJWT(t, resp.Rooms[0].Token, voiceTestSecret)
	wantVideo := map[string]any{
		"roomJoin": true, "room": room, "canSubscribe": true,
		"canPublish": true, "canPublishData": false, "canPublishSources": []any{"microphone"},
	}
	if fmt.Sprint(claims["video"]) != fmt.Sprint(wantVideo) || claims["sub"] != f.identity("ann") {
		t.Errorf("the person's claims are %v, want sub %s video %v", claims, f.identity("ann"), wantVideo)
	}
	if len(claims) != 6 {
		t.Errorf("the person's token has claims %v, want exactly iss, sub, jti, nbf, exp, video", claims)
	}
	if resp.CompanionIdentity != f.screenID("ann") {
		t.Errorf("companion_identity = %q, want %q", resp.CompanionIdentity, f.screenID("ann"))
	}
}

// TestVoiceSessionCarriesCompanionIdentity: every issued session names the
// companion identity, built from the caller's own id.
func TestVoiceSessionCarriesCompanionIdentity(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	for _, who := range []string{"ann", "ann2"} {
		resp := decodeSession(t, f.session(t, who, "ops"))
		if want := f.identity(who) + ".screen"; resp.CompanionIdentity != want || resp.CompanionIdentity == resp.Identity {
			t.Errorf("%s: companion_identity = %q, identity %q, want %q", who, resp.CompanionIdentity, resp.Identity, want)
		}
	}
}

// TestVoiceCompanionHolderBeforeToken: the holder is recorded before LiveKit
// is asked for the room, and so before any token.
func TestVoiceCompanionHolderBeforeToken(t *testing.T) {
	t.Run("recorded first", func(t *testing.T) {
		f, _ := newShareFixture(t)
		real := f.srv.voice.recordHolder
		calls := 0
		f.srv.voice.recordHolder = func(ctx context.Context, room, principal, cred int64) error {
			calls++
			if principal != f.ids["ann"] {
				t.Errorf("holder principal = %d, want ann", principal)
			}
			if n := f.lk.count("CreateRoom"); n != 2 { // the fixture's two sessions
				t.Errorf("CreateRoom calls = %d when the holder was recorded, want the fixture's 2", n)
			}
			return real(ctx, room, principal, cred)
		}
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
		if calls != 1 {
			t.Errorf("recordHolder calls = %d, want 1", calls)
		}
	})
	t.Run("recording fails", func(t *testing.T) {
		f, _ := newShareFixture(t)
		creates := f.lk.count("CreateRoom")
		f.srv.voice.recordHolder = func(context.Context, int64, int64, int64) error { return fmt.Errorf("store unavailable") }
		res := f.companion(t, "ann", "ops", companionScreen)
		if res.status != http.StatusInternalServerError || strings.Contains(res.body, "token") || strings.Contains(res.body, "eyJ") {
			t.Fatalf("got %d %s, want a 500 with no token", res.status, res.body)
		}
		if f.lk.count("CreateRoom") != creates {
			t.Error("LiveKit was asked for a room although the holder could not be recorded")
		}
		if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 2 {
			t.Errorf("voice_session_issued rows = %d, want only the fixture's two", n)
		}
	})
}

// TestVoiceCompanionRechecksBeforeSigning: a caller who loses their place
// while LiveKit is asked for the room gets the unknown-channel 404 and no
// token; a room rotated under the request leaves nothing to join.
func TestVoiceCompanionRechecksBeforeSigning(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		body string
		lose func(f *presenceFixture) error
	}{
		{"removed from the channel", companionScreen, func(f *presenceFixture) error {
			_, err := f.srv.store.RemoveChannelMember(ctx, "system", f.ops.ID, f.ids["ann"])
			return err
		}},
		{"principal disabled, viewer", companionViewerBody, func(f *presenceFixture) error {
			_, err := f.srv.store.DisablePrincipal(ctx, "system", f.ids["ann"])
			return err
		}},
		{"all credentials revoked", companionScreen, func(f *presenceFixture) error {
			_, err := f.srv.store.RevokeAllCredentials(ctx, "system", f.ids["ann"])
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newShareFixture(t)
			f.lk.setOnCreate(func(string) {
				if err := tt.lose(f); err != nil {
					t.Errorf("lose entitlement: %v", err)
				}
			})
			res := f.companion(t, "ann", "ops", tt.body)
			if res.status != http.StatusNotFound || errCode(t, res.body) != "channel_not_found" {
				t.Fatalf("answer = %d %s, want the unknown-channel 404", res.status, res.body)
			}
			if strings.Contains(res.body, "token") || strings.Contains(res.body, "eyJ") {
				t.Errorf("the refusal carries a token: %s", res.body)
			}
			if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 2 {
				t.Errorf("voice_session_issued rows = %d, want only the fixture's two", n)
			}
			if other := f.companion(t, "bob", "ops", tt.body); other.body != res.body {
				t.Errorf("differs from a non-member's answer:\n %s\n %s", res.body, other.body)
			}
		})
	}
	t.Run("room rotated under the request", func(t *testing.T) {
		f, _ := newShareFixture(t)
		room, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.lk.setOnCreate(func(string) {
			if _, _, err := f.srv.store.RotateVoiceRoom(ctx, room.ID, "test"); err != nil {
				t.Errorf("rotate: %v", err)
			}
		})
		res := f.companion(t, "ann", "ops", companionViewerBody)
		// The new room has no session in it: nothing to join, and no token.
		if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("got %d %s, want 409 voice_no_session", res.status, res.body)
		}
		if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 2 {
			t.Errorf("voice_session_issued rows = %d, want only the fixture's two", n)
		}
	})
}

// TestVoiceCompanionSuccessorRoomIsNotSigned: the handler's checks (the
// person is in the room, no companion yet) were made against the room it
// found. If that room is rotated while LiveKit is asked, and someone else has
// meanwhile obtained a session on the successor, no token is signed for the
// successor: the checks never ran there.
func TestVoiceCompanionSuccessorRoomIsNotSigned(t *testing.T) {
	for _, body := range []string{companionScreen, companionViewerBody} {
		f, _ := newShareFixture(t)
		ctx := context.Background()
		old, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID)
		if err != nil {
			t.Fatal(err)
		}
		fired := false
		f.lk.setOnCreate(func(string) {
			if fired {
				return
			}
			fired = true
			if _, _, err := f.srv.store.RotateVoiceRoom(ctx, old.ID, "test"); err != nil {
				t.Errorf("rotate: %v", err)
			}
			// Another person holds a session on the successor.
			decodeSession(t, f.session(t, "ann2", "ops"))
		})
		res := f.companion(t, "ann", "ops", body)
		if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("%s: got %d %s, want 409 voice_no_session", body, res.status, res.body)
		}
		if strings.Contains(res.body, "eyJ") {
			t.Errorf("the refusal carries a token: %s", res.body)
		}
		for _, e := range f.audits(t, store.AuditVoiceSessionIssued) {
			if strings.Contains(e.Detail, "companion:") {
				t.Errorf("a companion grant was audited: %q", e.Detail)
			}
		}
		// And nothing was recorded for ann on the successor.
		cur, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID)
		if err != nil {
			t.Fatal(err)
		}
		hs, err := f.srv.store.VoiceRoomHolders(ctx, cur.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hs {
			if h.PrincipalID == f.ids["ann"] {
				t.Errorf("ann was recorded as a holder of the successor room")
			}
		}
	}
}

// TestVoiceCompanionQuickSecondRequest: a companion token just signed counts
// as present for one pass gap, so two quick requests get one token and one
// 409, without waiting for a pass to list the companion. DELETE and the
// passing of the gap free the slot; a viewer is never held back.
func TestVoiceCompanionQuickSecondRequest(t *testing.T) {
	t.Run("second is busy", func(t *testing.T) {
		f, _ := newShareFixture(t)
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
		res := f.companion(t, "ann", "ops", companionPublish)
		if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCompanionBusy {
			t.Fatalf("got %d %s, want 409 voice_companion_busy", res.status, res.body)
		}
		if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 3 {
			t.Errorf("voice_session_issued rows = %d, want 3", n)
		}
		// A viewer is not held back.
		decodeCompanion(t, f.companion(t, "ann", "ops", companionViewerBody))
	})
	t.Run("the gap passes", func(t *testing.T) {
		f, _ := newShareFixture(t)
		f.at(0)
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
		f.clock.Advance(voicePassGapMax + time.Millisecond)
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	})
	t.Run("DELETE frees it", func(t *testing.T) {
		f, _ := newShareFixture(t)
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
		if res := f.removeCompanion(t, "ann", ""); res.status != http.StatusNoContent {
			t.Fatalf("DELETE = %d", res.status)
		}
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	})
	t.Run("a failed audit write does not hold the slot", func(t *testing.T) {
		f, _ := newShareFixture(t)
		path, _ := testDBPaths.Load(f.srv)
		db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(`CREATE TRIGGER quick_audit_fails BEFORE INSERT ON audit_events
			WHEN NEW.action = 'voice_session_issued' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
			t.Fatal(err)
		}
		if res := f.companion(t, "ann", "ops", companionScreen); res.status != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", res.status)
		}
		if _, err := db.Exec(`DROP TRIGGER quick_audit_fails`); err != nil {
			t.Fatal(err)
		}
		decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	})
}

// TestVoiceCompanionLiveKitDown: a LiveKit that cannot create the room is 503
// and issues nothing.
func TestVoiceCompanionLiveKitDown(t *testing.T) {
	f, _ := newShareFixture(t)
	f.lk.setCreateStatus(http.StatusInternalServerError)
	res := f.companion(t, "ann", "ops", companionScreen)
	if res.status != http.StatusServiceUnavailable || errCode(t, res.body) != schema.ErrorCodeVoiceUnavailable {
		t.Fatalf("got %d %s, want 503 voice_unavailable", res.status, res.body)
	}
	if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 2 {
		t.Errorf("voice_session_issued rows = %d, want only the fixture's two", n)
	}
	for _, s := range []string{voiceTestSecret, "eyJ"} {
		if strings.Contains(f.logs.buf.String(), s) {
			t.Errorf("logs contain %q", s)
		}
	}
}

// TestVoiceCompanionAuditFailureWithholdsTheToken: a grant conchd cannot
// account for is not issued.
func TestVoiceCompanionAuditFailureWithholdsTheToken(t *testing.T) {
	f, _ := newShareFixture(t)
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER companion_audit_fails BEFORE INSERT ON audit_events
		WHEN NEW.action = 'voice_session_issued' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	res := f.companion(t, "ann", "ops", companionScreen)
	if res.status != http.StatusInternalServerError || strings.Contains(res.body, "token") || strings.Contains(res.body, "eyJ") {
		t.Fatalf("got %d %s, want a 500 with no token", res.status, res.body)
	}
}

// TestVoiceCompanionAuditAndLogs: no response field but token and room, no
// log line, and no audit row names a token, its signature, the API secret or
// the room name.
func TestVoiceCompanionAuditAndLogs(t *testing.T) {
	f, room := newShareFixture(t)
	f.companionConnects(t, room)
	screen := f.companion(t, "ann", "ops", companionScreen) // 409: busy, a refusal that logs
	if screen.status != http.StatusConflict {
		t.Fatalf("setup: %d", screen.status)
	}
	if res := f.removeCompanion(t, "ann", ""); res.status != http.StatusNoContent {
		t.Fatalf("setup: %d", res.status)
	}
	res := f.companion(t, "ann", "ops", companionScreen)
	resp := decodeCompanion(t, res)
	view := decodeCompanion(t, f.companion(t, "ann", "ops", companionViewerBody))

	for _, r := range []struct {
		body string
		resp schema.VoiceCompanionResponseV1
	}{{res.body, resp}, {"", view}} {
		var fields map[string]any
		if r.body != "" {
			if err := json.Unmarshal([]byte(r.body), &fields); err != nil {
				t.Fatal(err)
			}
			for k, v := range fields {
				if k == "token" || k == "room" {
					continue
				}
				s := fmt.Sprint(v)
				if strings.Contains(s, r.resp.Token) || strings.Contains(s, room) {
					t.Errorf("response field %q names the token or the room", k)
				}
			}
		}
	}
	logs := f.logs.buf.String()
	if !strings.Contains(logs, "voice: companion issued") {
		t.Fatalf("log capture is not seeing the companion log lines:\n%s", logs)
	}
	for _, tok := range []string{resp.Token, view.Token} {
		secrets := []struct{ what, text string }{
			{"token", tok}, {"token signature", tok[strings.LastIndex(tok, ".")+1:]},
			{"API secret", voiceTestSecret}, {"room name", room},
		}
		for _, s := range secrets {
			if strings.Contains(logs, s.text) {
				t.Errorf("a log line contains the %s", s.what)
			}
			for _, ev := range f.audit(t) {
				if strings.Contains(ev.Actor+ev.Action+ev.Subject+ev.Detail, s.text) {
					t.Errorf("audit event %d (%s) contains the %s", ev.ID, ev.Action, s.what)
				}
			}
		}
	}
}

// TestVoiceCompanionDelete: both, one, none present, bad queries, LiveKit
// down. 204 on every success; never p<id>; audited only when something was
// removed.
func TestVoiceCompanionDelete(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		viewer      bool
		screen      bool
		wantStatus  int
		wantGone    []string // which of screen, view are gone afterwards
		wantRemoved []string // participant= values of the audit rows
	}{
		{"both, no parameter", "", true, true, 204, []string{"screen", "view"}, []string{"screen", "view"}},
		{"screen only named", "?publish=screen", true, true, 204, []string{"screen"}, []string{"screen"}},
		{"viewer only named", "?publish=none", true, true, 204, []string{"view"}, []string{"view"}},
		{"none present", "", false, false, 204, nil, nil},
		{"named one not present", "?publish=none", false, true, 204, nil, nil},
		{"unknown value", "?publish=camera", true, true, 400, nil, nil},
		{"empty value", "?publish=", true, true, 400, nil, nil},
		{"repeated", "?publish=screen&publish=none", true, true, 400, nil, nil},
		{"upper case", "?publish=Screen", true, true, 400, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			parts := []fakeParticipant{mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2")}
			if tt.screen {
				parts = append(parts, fakeParticipant{identity: f.screenID("ann"), joinedMs: 3_000, shareGranted: true})
			}
			if tt.viewer {
				parts = append(parts, fakeParticipant{identity: f.viewID("ann"), joinedMs: 4_000})
			}
			f.lk.setRoom(room, parts...)
			f.at(100)
			f.pass(t)
			res := f.removeCompanion(t, "ann", tt.query)
			if res.status != tt.wantStatus || (tt.wantStatus == 204 && res.body != "") {
				t.Fatalf("got %d %q, want %d", res.status, res.body, tt.wantStatus)
			}
			if tt.wantStatus == 400 && errCode(t, res.body) != "invalid_request" {
				t.Errorf("code = %s", errCode(t, res.body))
			}
			// The person and everyone else stay.
			for _, who := range []string{"ann", "ann2"} {
				if !f.lk.in(room, f.identity(who)) {
					t.Errorf("%s was removed", who)
				}
			}
			if f.lk.in(room, f.screenID("ann")) == slices.Contains(tt.wantGone, "screen") && tt.screen {
				t.Errorf("screen companion present = %v, want gone = %v", f.lk.in(room, f.screenID("ann")), slices.Contains(tt.wantGone, "screen"))
			}
			if f.lk.in(room, f.viewID("ann")) == slices.Contains(tt.wantGone, "view") && tt.viewer {
				t.Errorf("viewer present = %v, want gone = %v", f.lk.in(room, f.viewID("ann")), slices.Contains(tt.wantGone, "view"))
			}
			var removed []string
			for _, e := range f.audits(t, store.AuditVoiceCompanionRemoved) {
				if e.Actor != f.actor("ann") || e.Subject != fmt.Sprintf("channel:%d", f.ops.ID) {
					t.Errorf("row actor/subject = %q / %q", e.Actor, e.Subject)
				}
				part := "screen"
				if strings.Contains(e.Detail, "participant=view") {
					part = "view"
				}
				if want := fmt.Sprintf("channel=%d principal=%d participant=%s reason=requested", f.ops.ID, f.ids["ann"], part); e.Detail != want {
					t.Errorf("detail = %q, want %q", e.Detail, want)
				}
				removed = append(removed, part)
			}
			if !slices.Equal(removed, tt.wantRemoved) {
				t.Errorf("removal rows = %v, want %v", removed, tt.wantRemoved)
			}
			// It never ends the claim and never removes the person: nothing
			// but suffixed identities was asked of LiveKit.
			if n := f.lk.personRemovals(); n != 0 {
				t.Errorf("DELETE removed %d person connections", n)
			}
		})
	}
	t.Run("never someone else's", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
			fakeParticipant{identity: f.screenID("ann2"), joinedMs: 3_000}, fakeParticipant{identity: f.viewID("ann2"), joinedMs: 3_000})
		f.at(100)
		f.pass(t)
		if res := f.removeCompanion(t, "ann", ""); res.status != http.StatusNoContent {
			t.Fatalf("DELETE = %d", res.status)
		}
		if !f.lk.in(room, f.screenID("ann2")) || !f.lk.in(room, f.viewID("ann2")) {
			t.Error("ann removed ann2's participants")
		}
	})
	t.Run("a share claim survives", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.companionConnects(t, room)
		f.mustClaim(t, "ann")
		if res := f.removeCompanion(t, "ann", ""); res.status != http.StatusNoContent {
			t.Fatalf("DELETE = %d", res.status)
		}
		if got := f.shareState(room); got != f.ids["ann"] {
			t.Errorf("the share is held by %d after DELETE, want ann", got)
		}
	})
	t.Run("no room: nothing to remove and none made", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		if res := f.removeCompanion(t, "ann", ""); res.status != http.StatusNoContent {
			t.Fatalf("DELETE = %d", res.status)
		}
		if n := f.lk.count("RemoveParticipant"); n != 0 {
			t.Errorf("RemoveParticipant calls = %d", n)
		}
	})
	t.Run("livekit cannot be asked", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.companionConnects(t, room)
		f.lk.setRemoveStatus(http.StatusInternalServerError)
		res := f.removeCompanion(t, "ann", "")
		if res.status != http.StatusServiceUnavailable || errCode(t, res.body) != schema.ErrorCodeVoiceUnavailable {
			t.Fatalf("got %d %s, want 503 voice_unavailable", res.status, res.body)
		}
		if n := len(f.audits(t, store.AuditVoiceCompanionRemoved)); n != 0 {
			t.Errorf("removal rows = %d, want 0", n)
		}
		// Still there, so still busy.
		f.lk.setRemoveStatus(0)
		if res := f.companion(t, "ann", "ops", companionScreen); res.status != http.StatusConflict {
			t.Errorf("after the failed DELETE: %d, want 409", res.status)
		}
	})
}

// TestVoiceCompanionUnclaimedShare: a companion's screen permission follows
// the room's share claim (§13). A companion issued while nothing is claimed
// connects with the screen in its token, has it withdrawn on the next pass,
// and is given it back when the person claims.
func TestVoiceCompanionUnclaimedShare(t *testing.T) {
	f, room := newShareFixture(t)
	decodeCompanion(t, f.companion(t, "ann", "ops", companionScreen))
	// The companion connects on its token, which names the screen, and the
	// pass that lists it finds a screen grant nobody holds the share for.
	f.companionConnects(t, room)
	f.at(500)
	f.pass(t)
	ups := f.lk.updatesFor(f.screenID("ann"))
	if len(ups) != 1 || ups[0].canPublish || ups[0].sources == nil || len(ups[0].sources) != 0 {
		t.Fatalf("companion updates after a pass with no claim = %+v, want one: canPublish false, sources []", ups)
	}
	if !ups[0].canSubscribe {
		t.Error("the withdrawal took away the companion's right to receive")
	}
	if p, _ := f.lk.participant(room, f.screenID("ann")); p.screen || p.shareGranted {
		t.Errorf("the unclaimed companion still has %+v", p)
	}
	// The claim gives the screen back, to the companion and nothing more.
	f.mustClaim(t, "ann")
	ups = f.lk.updatesFor(f.screenID("ann"))
	last := ups[len(ups)-1]
	if !last.canPublish || !slices.Equal(last.sources, []string{"SCREEN_SHARE"}) {
		t.Errorf("after the claim the companion's last update = %+v, want canPublish with [SCREEN_SHARE]", last)
	}
}

// TestVoiceCompanionPublishOnlyKeepsItsGrant: a publish-only companion is
// withdrawn and restored without gaining the right to receive.
func TestVoiceCompanionPublishOnlyKeepsItsGrant(t *testing.T) {
	f, room := newShareFixture(t)
	decodeCompanion(t, f.companion(t, "ann", "ops", companionPublish))
	f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
		fakeParticipant{identity: f.screenID("ann"), joinedMs: 3_000, shareGranted: true, noSubscribe: true})
	f.at(100)
	f.pass(t)
	f.publishScreen(room, true, false)
	f.at(500)
	f.pass(t)
	f.mustClaim(t, "ann")
	for _, u := range f.lk.updatesFor(f.screenID("ann")) {
		if u.canSubscribe {
			t.Errorf("an update gave the publish-only companion the right to receive: %+v", u)
		}
	}
}
