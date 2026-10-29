package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the screen share (issue #443, docs/design/voice-control-plane.md
// §12): the two endpoints, and the passes that keep the share honest, over the
// scripted LiveKit with the fixture's manual clock. Nothing sleeps.

// ---------------------------------------------------------------------------
// Fixture additions

// newShareFixture is a presence fixture in which ann and ann2 hold sessions in
// ops and are connected to its room, muted, and a pass has seen them. It
// returns the room's name.
func newShareFixture(t *testing.T) (*presenceFixture, string) {
	t.Helper()
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	decodeSession(t, f.session(t, "ann2", "ops"))
	f.lk.setRoom(room,
		fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true, muted: true},
		fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000, published: true, muted: true})
	f.at(0)
	f.pass(t)
	return f, room
}

func (f *presenceFixture) claimShare(t *testing.T, who, channel string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPost, "/v1/channels/"+channel+"/voice/share", f.tokens[who], "")
}

func (f *presenceFixture) stopShare(t *testing.T, who, channel string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodDelete, "/v1/channels/"+channel+"/voice/share", f.tokens[who], "")
}

// mustClaim claims the share in ops as who and requires it granted.
func (f *presenceFixture) mustClaim(t *testing.T, who string) schema.VoiceShareV1 {
	t.Helper()
	res := f.claimShare(t, who, "ops")
	if res.status != http.StatusOK {
		t.Fatalf("%s claims the share: %d %s, want 200", who, res.status, res.body)
	}
	dec := json.NewDecoder(strings.NewReader(res.body))
	dec.DisallowUnknownFields()
	var share schema.VoiceShareV1
	if err := dec.Decode(&share); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if err := share.Validate(); err != nil {
		t.Fatalf("share %s does not validate: %v", res.body, err)
	}
	if share.PrincipalID != f.ids[who] || !share.Audio {
		t.Fatalf("share = %+v, want %s's with sound", share, who)
	}
	return share
}

// mustStop stops the share in ops as who and requires 204.
func (f *presenceFixture) mustStop(t *testing.T, who string) {
	t.Helper()
	if res := f.stopShare(t, who, "ops"); res.status != http.StatusNoContent || res.body != "" {
		t.Fatalf("%s stops the share: %d %q, want 204 and no body", who, res.status, res.body)
	}
}

// shareRows lists the share rows of the audit log in order, as
// "<actor> <started|stopped|unclaimed> <rest of the detail>", checking on the
// way that each has the channel as subject and starts its detail with it.
func (f *presenceFixture) shareRows(t *testing.T) []string {
	t.Helper()
	names := map[string]string{}
	for name, id := range f.ids {
		names[fmt.Sprintf("principal:%d", id)] = name
		names[fmt.Sprintf("principal=%d", id)] = "principal=" + name
	}
	var out []string
	for _, e := range f.audit(t) {
		if !strings.HasPrefix(e.Action, "voice_share_") {
			continue
		}
		who, ok := names[e.Actor]
		if !ok {
			t.Errorf("%s has actor %q, want a principal", e.Action, e.Actor)
		}
		if want := fmt.Sprintf("channel:%d", f.ops.ID); e.Subject != want {
			t.Errorf("%s has subject %q, want %q", e.Action, e.Subject, want)
		}
		rest, ok := strings.CutPrefix(e.Detail, fmt.Sprintf("channel=%d ", f.ops.ID))
		if !ok {
			t.Errorf("%s has detail %q, want it to begin with the channel", e.Action, e.Detail)
		}
		for raw, name := range names {
			if strings.HasPrefix(raw, "principal=") {
				rest = strings.ReplaceAll(rest, raw+" ", name+" ")
			}
		}
		out = append(out, fmt.Sprintf("%s %s %s", who, strings.TrimPrefix(e.Action, "voice_share_"), rest))
	}
	return out
}

// sharing names who presence shows sharing in ch, with "+audio" when the
// share has sound, or "-" when nobody is.
func (f *presenceFixture) sharing(t *testing.T, ch store.Channel) string {
	t.Helper()
	s := f.snap(t, ch)
	byID := map[int64]string{}
	for name, id := range f.ids {
		byID[id] = name
	}
	for _, r := range s.Rooms {
		for _, p := range r.Participants {
			if p.Sharing {
				if p.SharingAudio {
					return byID[p.PrincipalID] + "+audio"
				}
				return byID[p.PrincipalID]
			}
		}
	}
	return "-"
}

// publish sets who's screen-share tracks in room, as a browser that holds the
// grant publishes or unpublishes them.
func (f *presenceFixture) publish(room, who string, screen, audio bool) {
	f.lk.update(room, f.identity(who), func(p *fakeParticipant) { p.screen, p.screenAudio = screen, audio })
}

// granted reports whether the fake LiveKit lets who's connection to room
// publish the screen-share sources.
func (f *presenceFixture) granted(t *testing.T, room, who string) bool {
	t.Helper()
	p, ok := f.lk.participant(room, f.identity(who))
	return ok && p.shareGranted
}

// shareState returns who holds the poller's share of room, or 0 when nobody
// does.
func (f *presenceFixture) shareState(room string) int64 {
	p := f.srv.voice
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rooms[room]
	if r == nil || r.share == nil {
		return 0
	}
	return r.share.principalID
}

// ---------------------------------------------------------------------------
// The claim

// TestVoiceShareClaimRefusals: the claim checks the caller exactly as a
// session does, and in this order: authentication, the unknown-channel 404,
// the agent 403, voice_not_allowed, voice not configured; then the session
// (voice_no_session), and only then whether the room's share is free
// (voice_share_busy). A refusal grants nothing and writes no share row.
func TestVoiceShareClaimRefusals(t *testing.T) {
	held := func(who string) func(*testing.T, *presenceFixture) {
		return func(t *testing.T, f *presenceFixture) { decodeSession(t, f.session(t, who, "ops")) }
	}
	connected := func(t *testing.T, f *presenceFixture) {
		room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
		f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
	}
	tests := []struct {
		name       string
		opts       presenceOpts
		setup      func(*testing.T, *presenceFixture)
		who        string
		channel    string
		wantStatus int
		wantCode   string
		wantDenied string // reason of the one access_denied row, if any
		wantGrants int    // UpdateParticipant calls the refusal is allowed to make
	}{
		{"no credential", presenceOpts{auth: AuthRequired}, connected, "", "ops", 401, "unauthenticated", "", 0},
		{"a disabled principal", presenceOpts{auth: AuthRequired}, nil, "dora", "ops", 401, "unauthenticated", "", 0},
		{"authentication off", presenceOpts{auth: AuthOff}, nil, "", "ops", 400, schema.ErrorCodeVoiceRequiresAuth, "", 0},
		{"a non-member", presenceOpts{auth: AuthRequired}, connected, "bob", "ops", 404, "channel_not_found", "", 0},
		{"an unknown channel", presenceOpts{auth: AuthRequired}, connected, "ann", "nosuch", 404, "channel_not_found", "", 0},
		{"an operator who is not a member", presenceOpts{auth: AuthRequired}, connected, "root", "ops", 404, "channel_not_found", "", 0},
		{"a non-member, voice not configured", presenceOpts{auth: AuthRequired, unconfigured: true}, nil, "bob", "ops", 404, "channel_not_found", "", 0},
		{"an agent member", presenceOpts{auth: AuthRequired}, connected, "robo", "ops", 403, "forbidden", denyAgentVoice, 0},
		{"an agent that is not a member", presenceOpts{auth: AuthRequired}, nil, "robo", "ops2", 404, "channel_not_found", denyNotMember, 0},
		{"a member without voice", presenceOpts{auth: AuthRequired}, nil, "vic", "ops", 403, schema.ErrorCodeVoiceNotAllowed, schema.ErrorCodeVoiceNotAllowed, 0},
		{"a member without voice, voice not configured", presenceOpts{auth: AuthRequired, unconfigured: true}, nil, "vic", "ops", 403, schema.ErrorCodeVoiceNotAllowed, schema.ErrorCodeVoiceNotAllowed, 0},
		{"a member without voice in another channel", presenceOpts{auth: AuthRequired}, nil, "vic", "ops2", 404, "channel_not_found", "", 0},
		{"a member, voice not configured", presenceOpts{auth: AuthRequired, unconfigured: true}, nil, "ann", "ops", 503, schema.ErrorCodeVoiceNotConfigured, "", 0},
		{"a member who was never issued a session", presenceOpts{auth: AuthRequired}, nil, "ann", "ops", 409, schema.ErrorCodeVoiceNoSession, "", 0},
		{"a member with no session while another holds one", presenceOpts{auth: AuthRequired}, connected, "ann2", "ops", 409, schema.ErrorCodeVoiceNoSession, "", 0},
		{"a session in another channel", presenceOpts{auth: AuthRequired}, connected, "ann", "ops2", 409, schema.ErrorCodeVoiceNoSession, "", 0},
		{"a session under another credential", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) {
				connected(t, f)
				_, tok := f.credentialWithExpiry(t, "ann", time.Now().Add(time.Hour))
				f.tokens["ann-other"] = tok
			}, "ann-other", "ops", 409, schema.ErrorCodeVoiceNoSession, "", 0},
		// A share is granted to a connection, and a holder who has none is
		// told they have no session to share from.
		{"a holder who is not connected", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", 409, schema.ErrorCodeVoiceNoSession, "", 1},
		{"someone else holds the share", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) {
				connected(t, f)
				decodeSession(t, f.session(t, "ann2", "ops"))
				f.mustClaim(t, "ann")
			}, "ann2", "ops", 409, schema.ErrorCodeVoiceShareBusy, "", 0},
		{"LiveKit does not answer", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) {
				connected(t, f)
				f.lk.setUpdateStatus(http.StatusServiceUnavailable)
			}, "ann", "ops", 503, schema.ErrorCodeVoiceUnavailable, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, tt.opts)
			f.joinWithoutVoice(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			rows := len(f.shareRows(t))
			grants := f.lk.count("UpdateParticipant")
			denied := len(f.audits(t, "access_denied"))
			holder := f.shareState(f.room(t, f.ops))

			res := f.claimShare(t, tt.who, tt.channel)
			if res.status != tt.wantStatus || errCode(t, res.body) != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if got := f.shareRows(t); len(got) != rows {
				t.Errorf("a refused claim wrote share rows: %q", got[rows:])
			}
			if got := f.lk.count("UpdateParticipant") - grants; got != tt.wantGrants {
				t.Errorf("the refusal made %d UpdateParticipant calls, want %d", got, tt.wantGrants)
			}
			if now := f.shareState(f.room(t, f.ops)); now != holder {
				t.Errorf("the share's holder is %d after a refused claim, was %d", now, holder)
			}
			denials := f.audits(t, "access_denied")[denied:]
			switch {
			case tt.wantDenied == "" && len(denials) != 0:
				t.Errorf("access_denied rows = %+v, want none", denials)
			case tt.wantDenied != "":
				if len(denials) != 1 || !strings.Contains(denials[0].Detail, tt.wantDenied) || denials[0].Subject != "POST /v1/channels/{channel}/voice/share" {
					t.Errorf("access_denied rows = %+v, want one for %s on this route", denials, tt.wantDenied)
				}
			}
			for _, secret := range []string{"conch-", f.tokens["ann"]} {
				if strings.Contains(res.body, secret) {
					t.Errorf("the refusal contains %q: %s", secret, res.body)
				}
			}
		})
	}
}

// joinWithoutVoice adds vic to the presence fixture as voiceFixture's does: a
// member of ops through an invite that did not grant voice (issue #437).
func (f *presenceFixture) joinWithoutVoice(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	inv, err := f.srv.store.CreateInvite(ctx, "system", f.root.ID,
		schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"ops"}}, store.InviteOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.srv.store.RedeemInvite(ctx, store.HashInviteCode(inv.Code), "vic", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if joined.Principal.VoiceAllowed {
		t.Fatal("the joiner holds voice")
	}
	f.tokens["vic"], f.ids["vic"] = joined.Token, joined.Principal.ID
}

// TestVoiceShareNotFoundIsIdentical: a non-member gets the unknown-channel 404
// from both share routes, byte for byte, voice configured or not; so does an
// operator who is not a member claiming, but not stopping.
func TestVoiceShareNotFoundIsIdentical(t *testing.T) {
	for _, opts := range []presenceOpts{{auth: AuthRequired}, {auth: AuthRequired, unconfigured: true}} {
		f := newPresenceFixture(t, opts)
		want := f.claimShare(t, "ann", "nosuch")
		if want.status != 404 || errCode(t, want.body) != "channel_not_found" {
			t.Fatalf("unknown channel answer = %+v", want)
		}
		for _, c := range []struct{ who, channel string }{{"bob", "ops"}, {"bob", "nosuch"}, {"root", "ops"}, {"robo", "ops2"}} {
			if got := f.claimShare(t, c.who, c.channel); got != want {
				t.Errorf("unconfigured=%v: %s claiming in %s: %+v, want %+v", opts.unconfigured, c.who, c.channel, got, want)
			}
		}
		for _, c := range []struct{ who, channel string }{{"bob", "ops"}, {"bob", "nosuch"}, {"robo", "ops2"}, {"root", "nosuch"}} {
			if got := f.stopShare(t, c.who, c.channel); got != want {
				t.Errorf("unconfigured=%v: %s stopping in %s: %+v, want %+v", opts.unconfigured, c.who, c.channel, got, want)
			}
		}
	}
}

// TestVoiceShareStartAndStop is the share's life through the endpoints: the
// claim grants the sources in LiveKit and is shown and audited, a second claim
// is busy, the sharer stops it, the sources and tracks are gone and presence
// says so, and the next claim succeeds.
func TestVoiceShareStartAndStop(t *testing.T) {
	f, room := newShareFixture(t)
	if f.granted(t, room, "ann") {
		t.Fatal("the join token's connection may already share")
	}

	f.at(1_000)
	share := f.mustClaim(t, "ann")
	if !share.StartedAt.Time().Equal(ruleT0.Add(time.Second)) {
		t.Errorf("started_at = %v, want the moment of the claim", share.StartedAt.Time())
	}
	if !f.granted(t, room, "ann") || f.granted(t, room, "ann2") {
		t.Fatal("the claim did not grant ann alone the screen-share sources")
	}
	if got := f.sharing(t, f.ops); got != "ann" {
		t.Errorf("presence shows %q sharing, want ann at once", got)
	}
	// Asking again while holding it grants again and answers the same share.
	f.at(1_200)
	if again := f.mustClaim(t, "ann"); again != share {
		t.Errorf("a second claim by the holder = %+v, want %+v", again, share)
	}
	if res := f.claimShare(t, "ann2", "ops"); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceShareBusy {
		t.Fatalf("ann2 claims while ann shares: %d %s, want 409 voice_share_busy", res.status, res.body)
	} else if strings.Contains(res.body, f.identity("ann")) || strings.Contains(res.body, "ann") {
		t.Errorf("the busy answer names the sharer: %s", res.body)
	}

	// The browser publishes the screen and its sound.
	f.publish(room, "ann", true, true)
	f.at(1_500)
	f.pass(t)
	if got := f.sharing(t, f.ops); got != "ann+audio" {
		t.Errorf("presence shows %q, want ann sharing with sound", got)
	}

	f.at(2_000)
	f.mustStop(t, "ann")
	if p, _ := f.lk.participant(room, f.identity("ann")); p.shareGranted || p.screen || p.screenAudio {
		t.Errorf("after the stop LiveKit still has %+v", p)
	}
	if got := f.sharing(t, f.ops); got != "-" {
		t.Errorf("presence shows %q sharing after the stop", got)
	}
	f.at(2_500)
	f.pass(t)
	f.mustClaim(t, "ann2")
	want := []string{"ann started audio=true", "ann stopped principal=ann reason=self", "ann2 started audio=true"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
	if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
		t.Errorf("an honest share wrote %d unclaimed rows", n)
	}
}

// TestVoiceShareStop: who may stop the share. The sharer and an operator,
// member or not; another member is refused 403 and audited, and the share goes
// on; a non-member learns nothing. Stopping when nobody shares changes nothing.
func TestVoiceShareStop(t *testing.T) {
	tests := []struct {
		name       string
		who        string
		wantStatus int
		wantCode   string
		wantRows   []string
		wantDenied bool
	}{
		{"the sharer", "ann", 204, "", []string{"ann started audio=true", "ann stopped principal=ann reason=self"}, false},
		{"an operator who is not a member", "root", 204, "", []string{"ann started audio=true", "root stopped principal=ann reason=operator"}, false},
		{"another member", "ann2", 403, "forbidden", []string{"ann started audio=true"}, true},
		{"a non-member", "bob", 404, "channel_not_found", []string{"ann started audio=true"}, false},
		{"an agent member", "robo", 403, "forbidden", []string{"ann started audio=true"}, true},
		{"no credential", "", 401, "unauthenticated", []string{"ann started audio=true"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			f.mustClaim(t, "ann")
			f.publish(room, "ann", true, false)
			denied := len(f.audits(t, "access_denied"))
			res := f.stopShare(t, tt.who, "ops")
			if res.status != tt.wantStatus || (tt.wantCode != "" && errCode(t, res.body) != tt.wantCode) {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if got := f.shareRows(t); !slices.Equal(got, tt.wantRows) {
				t.Errorf("share rows %q, want %q", got, tt.wantRows)
			}
			stopped := tt.wantStatus == http.StatusNoContent
			if p, _ := f.lk.participant(room, f.identity("ann")); p.shareGranted == stopped || p.screen == stopped {
				t.Errorf("LiveKit has ann as %+v; stopped=%v", p, stopped)
			}
			if got, want := f.sharing(t, f.ops) == "-", stopped; got != want {
				t.Errorf("presence shows nobody sharing = %v, want %v", got, want)
			}
			if got := len(f.audits(t, "access_denied")) - denied; (got == 1) != tt.wantDenied {
				t.Errorf("access_denied rows = %d, want one: %v", got, tt.wantDenied)
			}
		})
	}

	t.Run("when nobody shares", func(t *testing.T) {
		f, _ := newShareFixture(t)
		calls := f.lk.count("UpdateParticipant")
		for _, who := range []string{"ann", "ann2", "root"} {
			f.mustStop(t, who)
		}
		f.stopShare(t, "root", "ops2") // a channel with no room at all
		if got := f.shareRows(t); len(got) != 0 {
			t.Errorf("share rows %q, want none", got)
		}
		if got := f.lk.count("UpdateParticipant") - calls; got != 0 {
			t.Errorf("%d UpdateParticipant calls, want none", got)
		}
	})
}

// ---------------------------------------------------------------------------
// The passes

// TestVoiceShareUnclaimed: a screen-share track nobody granted (forced in the
// fake: LiveKit would refuse it) is withdrawn on the first pass that sees it,
// and audited once. A grant without a track, as a restart leaves behind, is
// withdrawn without a row.
func TestVoiceShareUnclaimed(t *testing.T) {
	f, room := newShareFixture(t)
	f.publish(room, "ann2", true, true)
	f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) { p.shareGranted = true })
	f.at(500)
	f.pass(t)
	for _, who := range []string{"ann", "ann2"} {
		if p, _ := f.lk.participant(room, f.identity(who)); p.shareGranted || p.screen || p.screenAudio {
			t.Errorf("%s still has %+v after the pass", who, p)
		}
	}
	f.at(1_000)
	f.pass(t)
	want := []string{"ann2 unclaimed sources=screen_share,screen_share_audio source=observed"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
	if got := f.sharing(t, f.ops); got != "-" {
		t.Errorf("presence shows %q sharing", got)
	}
	// Both are still in the room: withdrawing is not removing.
	if got := f.who(t, f.ops); got != "ann,ann2" {
		t.Errorf("presence = %q, want ann,ann2", got)
	}

	// Forced again on a later connection: audited again.
	f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.screen = true })
	f.at(1_500)
	f.pass(t)
	if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 2 {
		t.Errorf("unclaimed rows = %d, want 2", n)
	}
}

// TestVoiceShareUnclaimedNotWithdrawn: when LiveKit refuses to take an
// unclaimed share's sources away, the participant is removed instead, and
// that is audited as a removal: fail closed.
func TestVoiceShareUnclaimedNotWithdrawn(t *testing.T) {
	f, room := newShareFixture(t)
	f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.screen = true })
	f.lk.setUpdateStatus(http.StatusInternalServerError)
	f.at(500)
	f.pass(t)
	if f.lk.in(room, f.identity("ann2")) {
		t.Fatal("ann2 is still connected with an unclaimed share")
	}
	rows := f.audits(t, store.AuditVoiceParticipantRemoved)
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, "reason="+voiceReasonShareUnclaimed) || !strings.Contains(rows[0].Detail, fmt.Sprintf("principal=%d", f.ids["ann2"])) {
		t.Errorf("removal rows = %+v, want one for ann2 with reason=%s", rows, voiceReasonShareUnclaimed)
	}
}

// TestVoiceShareEnds: every way a share ends by itself, each audited once with
// its reason, the share gone from presence and its sources from LiveKit.
func TestVoiceShareEnds(t *testing.T) {
	tests := []struct {
		name string
		// run starts with ann's share claimed at 1 s and runs passes.
		run      func(t *testing.T, f *presenceFixture, room string)
		wantRows []string
		// granted is what LiveKit should let ann's connection do at the end;
		// gone means ann is not connected at all.
		gone bool
	}{
		{
			name: "nothing is ever published: idle 15 s after the claim",
			run: func(t *testing.T, f *presenceFixture, _ string) {
				f.at(15_999)
				f.pass(t)
				if got := f.sharing(t, f.ops); got != "ann" {
					t.Fatalf("at 14.999 s after the claim presence shows %q, want ann still sharing", got)
				}
				f.at(16_000)
				f.pass(t)
			},
			wantRows: []string{"ann started audio=true", "ann stopped principal=ann reason=idle"},
		},
		{
			name: "the tracks go: idle 5 s after the last pass that saw one",
			run: func(t *testing.T, f *presenceFixture, room string) {
				f.publish(room, "ann", true, false)
				f.at(10_000)
				f.pass(t)
				f.at(20_000) // long past the first 15 s: a published share is not idle
				f.pass(t)
				f.publish(room, "ann", false, false)
				f.at(24_999)
				f.pass(t)
				if got := f.sharing(t, f.ops); got != "ann" {
					t.Fatalf("4.999 s after the tracks went presence shows %q, want ann", got)
				}
				f.at(25_000)
				f.pass(t)
			},
			wantRows: []string{"ann started audio=true", "ann stopped principal=ann reason=idle"},
		},
		{
			name: "the sharer leaves the room",
			run: func(t *testing.T, f *presenceFixture, room string) {
				f.publish(room, "ann", true, true)
				f.at(2_000)
				f.pass(t)
				f.lk.drop(room, f.identity("ann"))
				f.at(2_500)
				f.pass(t)
			},
			wantRows: []string{"ann started audio=true", "ann stopped principal=ann reason=left"},
			gone:     true,
		},
		{
			// The sharer reconnects between two passes with a fresh join
			// token: the share is theirs still, and they claim again to be
			// granted the sources on the new connection.
			name: "a reconnect keeps the share",
			run: func(t *testing.T, f *presenceFixture, room string) {
				f.publish(room, "ann", true, false)
				f.at(2_000)
				f.pass(t)
				f.lk.drop(room, f.identity("ann"))
				f.lk.setRoom(room, append([]fakeParticipant{{identity: f.identity("ann"), joinedMs: 9_000}}, mustParticipant(t, f, room, "ann2"))...)
				f.at(2_500)
				f.pass(t)
				if got := f.sharing(t, f.ops); got != "ann" {
					t.Fatalf("after a reconnect presence shows %q sharing, want ann", got)
				}
				f.mustClaim(t, "ann")
				f.publish(room, "ann", true, false)
				f.at(3_000)
				f.pass(t)
				f.mustStop(t, "ann")
			},
			wantRows: []string{"ann started audio=true", "ann stopped principal=ann reason=self"},
		},
		{
			// Removed from the channel in the store, without the hook that
			// rotates the room (the sweep would rotate it): the pass finds
			// the sharer no longer entitled.
			name: "the sharer loses access and a pass finds it",
			run: func(t *testing.T, f *presenceFixture, _ string) {
				if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
					t.Fatal(err)
				}
				f.at(2_000)
				f.pass(t)
			},
			wantRows: []string{"ann started audio=true", "ann stopped principal=ann reason=access_lost"},
			gone:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			f.at(1_000)
			f.mustClaim(t, "ann")
			tt.run(t, f, room)
			if got := f.shareRows(t); !slices.Equal(got, tt.wantRows) {
				t.Errorf("share rows %q, want %q", got, tt.wantRows)
			}
			if got := f.sharing(t, f.ops); got != "-" {
				t.Errorf("presence shows %q sharing at the end", got)
			}
			if holder := f.shareState(room); holder != 0 {
				t.Errorf("the poller still has a share held by %d", holder)
			}
			p, here := f.lk.participant(room, f.identity("ann"))
			if here == tt.gone {
				t.Errorf("ann connected = %v, want %v", here, !tt.gone)
			}
			if p.shareGranted || p.screen || p.screenAudio {
				t.Errorf("LiveKit still lets ann share: %+v", p)
			}
			// Someone else may share now.
			f.at(40_000)
			f.mustClaim(t, "ann2")
		})
	}
}

func mustParticipant(t *testing.T, f *presenceFixture, room, who string) fakeParticipant {
	t.Helper()
	p, ok := f.lk.participant(room, f.identity(who))
	if !ok {
		t.Fatalf("%s is not in the room", who)
	}
	return p
}

// TestVoiceShareEndsWithRotation: removing the sharer from the channel, or
// disabling them (issue #438's teardown), rotates the room and ends the share
// with it: written as rotated, before the response; nothing of it survives in
// the poller, in LiveKit or in presence, and the channel's new room has no
// share until someone claims one.
func TestVoiceShareEndsWithRotation(t *testing.T) {
	for _, tt := range []struct {
		name, method, path string
	}{
		{"removed from the channel", http.MethodDelete, "/v1/channels/ops/members/%d"},
		{"disabled", http.MethodPost, "/v1/principals/%d/disable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			f.mustClaim(t, "ann")
			f.publish(room, "ann", true, true)
			f.at(500)
			f.pass(t)
			if code := f.doOrdered(t, tt.method, fmt.Sprintf(tt.path, f.ids["ann"]), f.rootTok); code != http.StatusNoContent {
				t.Fatalf("status = %d", code)
			}
			want := []string{"ann started audio=true", "ann stopped principal=ann reason=rotated"}
			if got := f.shareRows(t); !slices.Equal(got, want) {
				t.Errorf("share rows %q, want %q", got, want)
			}
			if f.lk.has(room) {
				t.Error("the old room is still in LiveKit")
			}
			if holder := f.shareState(room); holder != 0 {
				t.Errorf("the old room's share survives, held by %d", holder)
			}
			next := f.room(t, f.ops)
			if next == room {
				t.Fatal("the room was not rotated")
			}
			if holder := f.shareState(next); holder != 0 {
				t.Errorf("the new room has a share held by %d", holder)
			}
			if got := f.sharing(t, f.ops); got != "-" {
				t.Errorf("presence shows %q sharing", got)
			}
			// The rows of the rotation: the share's end comes before anyone's
			// leaving.
			var order []string
			for _, e := range f.audit(t) {
				if e.Action == store.AuditVoiceShareStopped || (e.Action == store.AuditVoiceLeft && e.Actor == f.actor("ann")) {
					order = append(order, e.Action)
				}
			}
			if !slices.Equal(order, []string{store.AuditVoiceShareStopped, store.AuditVoiceLeft}) {
				t.Errorf("rows in the order %q, want the share's end before ann's leaving", order)
			}
			// ann2 rejoins the new room and may share in it.
			decodeSession(t, f.session(t, "ann2", "ops"))
			f.lk.setRoom(next, fakeParticipant{identity: f.identity("ann2"), joinedMs: 5_000})
			f.at(1_000)
			f.pass(t)
			f.mustClaim(t, "ann2")
		})
	}
}

// TestVoiceShareWithLiveKitDown: with LiveKit down no share can be granted
// (503), a share already held survives the outage and is judged by the first
// pass after it, and a stop during the outage ends the share at once (row and
// presence) and has its sources withdrawn when LiveKit answers again, without
// an unclaimed row.
func TestVoiceShareWithLiveKitDown(t *testing.T) {
	t.Run("a claim is refused, then granted once LiveKit answers", func(t *testing.T) {
		f, _ := newShareFixture(t)
		f.lk.setUpdateStatus(http.StatusServiceUnavailable)
		if res := f.claimShare(t, "ann", "ops"); res.status != 503 || errCode(t, res.body) != schema.ErrorCodeVoiceUnavailable {
			t.Fatalf("claim with LiveKit down: %d %s", res.status, res.body)
		}
		if holder := f.shareState(f.room(t, f.ops)); holder != 0 {
			t.Fatalf("a refused claim left a share held by %d", holder)
		}
		f.lk.setUpdateStatus(0)
		f.mustClaim(t, "ann")
	})
	t.Run("a share survives an outage", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.mustClaim(t, "ann")
		f.publish(room, "ann", true, false)
		f.at(1_000)
		f.pass(t)
		f.lk.setListStatus(http.StatusInternalServerError)
		f.at(2_000)
		f.pass(t)
		if s := f.snap(t, f.ops); s.Available {
			t.Fatal("presence is available with LiveKit failing")
		}
		f.lk.setListStatus(0)
		f.at(30_000) // long past the 5 s a share's tracks may be gone
		f.pass(t)
		if got := f.sharing(t, f.ops); got != "ann" {
			t.Errorf("after the outage presence shows %q, want ann still sharing", got)
		}
		if got := f.shareRows(t); !slices.Equal(got, []string{"ann started audio=true"}) {
			t.Errorf("share rows %q", got)
		}
	})
	// A stop LiveKit will not carry out: the share ends in conchd at once,
	// and the next pass, finding it still published and its sources still
	// not withdrawn, removes the sharer from the room, so that nobody goes on
	// watching it (fail closed).
	t.Run("a stop LiveKit cannot carry out", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.mustClaim(t, "ann")
		f.publish(room, "ann", true, false)
		f.lk.setUpdateStatus(http.StatusServiceUnavailable)
		f.mustStop(t, "root")
		if got := f.sharing(t, f.ops); got != "-" {
			t.Errorf("presence shows %q sharing after the stop", got)
		}
		if !f.granted(t, room, "ann") {
			t.Fatal("the fake withdrew a grant while failing")
		}
		f.at(1_000)
		f.pass(t)
		if f.lk.in(room, f.identity("ann")) {
			t.Fatal("the sharer is still in the room, still sharing, after a pass")
		}
		removed := f.audits(t, store.AuditVoiceParticipantRemoved)
		if len(removed) != 1 || !strings.Contains(removed[0].Detail, "reason="+voiceReasonShareNotWithdrawn) {
			t.Errorf("removal rows = %+v, want one with reason=%s", removed, voiceReasonShareNotWithdrawn)
		}
		want := []string{"ann started audio=true", "root stopped principal=ann reason=operator"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q (a stopped share is not unclaimed)", got, want)
		}
	})
	t.Run("a stop during an outage that ends before the next pass", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.mustClaim(t, "ann")
		f.publish(room, "ann", true, false)
		f.lk.setUpdateStatus(http.StatusServiceUnavailable)
		f.mustStop(t, "ann")
		f.lk.setUpdateStatus(0)
		f.at(1_000)
		f.pass(t)
		if p, here := f.lk.participant(room, f.identity("ann")); !here || p.shareGranted || p.screen {
			t.Errorf("the pass after the outage did not withdraw, or removed the sharer: here=%v %+v", here, p)
		}
		if n := len(f.audits(t, store.AuditVoiceParticipantRemoved)); n != 0 {
			t.Errorf("%d removals, want none: the sources could be withdrawn", n)
		}
		want := []string{"ann started audio=true", "ann stopped principal=ann reason=self"}
		if got := f.shareRows(t); !slices.Equal(got, want) {
			t.Errorf("share rows %q, want %q", got, want)
		}
	})
}

// TestVoiceShareRepublishAfterStop: once LiveKit has confirmed that a stopped
// share's sources are gone, a share published again (a client rejoining on a
// token LiveKit sent it during the share) is a share nobody granted: audited
// as unclaimed and withdrawn, whether or not a pass came in between.
func TestVoiceShareRepublishAfterStop(t *testing.T) {
	for _, between := range []bool{true, false} {
		t.Run(fmt.Sprintf("a pass in between: %v", between), func(t *testing.T) {
			f, room := newShareFixture(t)
			f.mustClaim(t, "ann")
			f.publish(room, "ann", true, true)
			f.at(500)
			f.pass(t)
			f.mustStop(t, "ann")
			if between {
				f.at(1_000)
				f.pass(t)
			}
			// The rejoin: a new connection, granted by LiveKit's old token.
			f.lk.drop(room, f.identity("ann"))
			f.lk.setRoom(room, append([]fakeParticipant{{identity: f.identity("ann"), joinedMs: 9_000, shareGranted: true, screen: true}}, mustParticipant(t, f, room, "ann2"))...)
			f.at(2_000)
			f.pass(t)
			want := []string{"ann started audio=true", "ann stopped principal=ann reason=self", "ann unclaimed sources=screen_share source=observed"}
			if got := f.shareRows(t); !slices.Equal(got, want) {
				t.Errorf("share rows %q, want %q", got, want)
			}
			if p, _ := f.lk.participant(room, f.identity("ann")); p.shareGranted || p.screen {
				t.Errorf("the republished share was not withdrawn: %+v", p)
			}
		})
	}
}

// TestVoiceShareNotInANetRoom: a share is claimed only in a channel-wide room,
// where every member may publish. A claim for any other room is refused
// before LiveKit is asked (409 voice_no_session at the endpoint).
func TestVoiceShareNotInANetRoom(t *testing.T) {
	f, room := newShareFixture(t)
	calls := f.lk.count("UpdateParticipant")
	net := store.VoiceRoom{ID: 999, ChannelID: f.ops.ID, NetID: 3, RoomName: room + "-net"}
	if _, err := f.srv.voice.claimShare(context.Background(), net, f.ids["ann"]); !errors.Is(err, errVoiceShareNotChannelRoom) {
		t.Fatalf("claim in a net room: %v, want errVoiceShareNotChannelRoom", err)
	}
	if got := f.lk.count("UpdateParticipant") - calls; got != 0 {
		t.Errorf("%d UpdateParticipant calls for a refused claim", got)
	}
	if !voiceRoomPublishes(store.VoiceRoom{}) || voiceRoomPublishes(net) {
		t.Error("voiceRoomPublishes does not follow voiceAudiences")
	}
}

// TestVoiceShareWithdrawalKeepsAListenerListening: withdrawing a source from
// a participant whose permission does not let them publish (a net monitor,
// from V5) leaves them unable to publish: losing a share never makes anyone a
// publisher. (Share sources named in a listener's permission grant nothing:
// LiveKit allows no source to a participant who may not publish, so with no
// track there is nothing to withdraw and nothing is asked of LiveKit.)
func TestVoiceShareWithdrawalKeepsAListenerListening(t *testing.T) {
	for _, track := range []bool{false, true} {
		t.Run(fmt.Sprintf("with a track published: %v", track), func(t *testing.T) {
			f, room := newShareFixture(t)
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.listenOnly, p.shareGranted, p.screen = true, true, track
			})
			calls := f.lk.count("UpdateParticipant")
			f.at(500)
			f.pass(t)
			p, here := f.lk.participant(room, f.identity("ann2"))
			if !here || !p.listenOnly || p.screen {
				t.Errorf("after the pass ann2 is here=%v %+v, want still listen-only and no share track", here, p)
			}
			if got, want := f.lk.count("UpdateParticipant")-calls, map[bool]int{false: 0, true: 1}[track]; got != want {
				t.Errorf("UpdateParticipant calls = %d, want %d", got, want)
			}
		})
	}
}

// TestVoiceShareLostVoice: voice_allowed is part of what the poller checks.
// A person without voice found in a room is removed by name; a sharer whose
// voice is taken away (no route does that today, so the share is set in the
// poller as a claim would) loses the share with reason=access_lost.
func TestVoiceShareLostVoice(t *testing.T) {
	f, room := newShareFixture(t)
	f.joinWithoutVoice(t)
	vic := f.identity("vic")
	f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"), fakeParticipant{identity: vic, joinedMs: 3_000, shareGranted: true, screen: true})
	r := f.srv.voice.roomNamed(room)
	r.order.Lock()
	f.srv.voice.mu.Lock()
	r.share = &voiceShare{principalID: f.ids["vic"], startedAt: f.clock.Now()}
	f.srv.voice.mu.Unlock()
	r.order.Unlock()
	f.at(500)
	f.pass(t)
	if f.lk.in(room, vic) {
		t.Error("a person without voice is still in the room")
	}
	removed := f.audits(t, store.AuditVoiceParticipantRemoved)
	if len(removed) != 1 || !strings.Contains(removed[0].Detail, "reason="+voiceReasonVoiceOff) {
		t.Errorf("removal rows = %+v, want one with reason=%s", removed, voiceReasonVoiceOff)
	}
	if got, want := f.shareRows(t), []string{"vic stopped principal=vic reason=access_lost"}; !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
	if got := f.who(t, f.ops); got != "ann,ann2" {
		t.Errorf("presence = %q, want ann,ann2", got)
	}
}

// TestVoiceShareClaimInARoomRotatedMeanwhile: a rotation that lands while the
// grant is being made, without the poller hearing of it. The claim finds the
// room retired, takes the grant back at once (not leaving it to a DeleteRoom
// that may fail), ends the share as rotated, and answers 409.
func TestVoiceShareClaimInARoomRotatedMeanwhile(t *testing.T) {
	f, room := newShareFixture(t)
	stored, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	f.lk.setOnUpdate(func(_, _ string, share bool) {
		if share {
			once.Do(func() {
				if _, _, err := f.srv.store.RotateVoiceRoom(context.Background(), stored.ID, store.VoiceRotateMemberRemoved); err != nil {
					t.Error(err)
				}
			})
		}
	})
	res := f.claimShare(t, "ann", "ops")
	if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
		t.Fatalf("claim in a room rotated meanwhile: %d %s, want 409 voice_no_session", res.status, res.body)
	}
	if f.granted(t, room, "ann") {
		t.Error("the grant in the retired room was left for its deletion")
	}
	if n := f.lk.callsFor("DeleteRoom", room); n != 0 {
		t.Errorf("DeleteRoom calls = %d; the test means the grant to be taken back without one", n)
	}
	want := []string{"ann started audio=true", "ann stopped principal=ann reason=rotated"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
}

// TestVoiceShareRestart: claims are in memory, and a restart drops them. The
// first pass after it finds the share's grant and track with no claim: it
// withdraws them and audits the track as unclaimed. Nothing of the old share
// is shown.
func TestVoiceShareRestart(t *testing.T) {
	f, room := newShareFixture(t)
	f.mustClaim(t, "ann")
	f.publish(room, "ann", true, false)
	f.at(1_000)
	f.pass(t)
	f.restart()
	f.at(5_000)
	f.sweep(t)
	f.pass(t)
	if p, _ := f.lk.participant(room, f.identity("ann")); p.shareGranted || p.screen {
		t.Errorf("after the restart LiveKit still lets ann share: %+v", p)
	}
	want := []string{"ann started audio=true", "ann unclaimed sources=screen_share source=observed"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
	if got := f.sharing(t, f.ops); got != "-" {
		t.Errorf("presence shows %q sharing", got)
	}
}

// TestVoiceShareGrantIsNeverTakenBack: a pass that decides to withdraw a
// participant's sources does it under the room's claims, and checks again
// there: a claim that lands between the pass's decision and its withdrawal is
// not undone by it.
func TestVoiceShareGrantIsNeverTakenBack(t *testing.T) {
	f, room := newShareFixture(t)
	// A grant left behind (as by a restart): the pass will want it gone.
	f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) { p.shareGranted = true })
	r := f.srv.voice.roomNamed(room)
	r.claims.Lock()
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		f.at(500)
		f.srv.voice.runPass(context.Background())
	}()
	// The pass has decided and waits for the claims: claim now, as the
	// lock's holder, the way claimShare records one.
	waitUntil(t, "the pass to decide", func() bool {
		f.srv.voice.mu.Lock()
		defer f.srv.voice.mu.Unlock()
		_, owed := r.withdrawing[voiceWithdrawKey{f.ids["ann"], voiceRolePerson}]
		return owed
	})
	r.order.Lock()
	f.srv.voice.mu.Lock()
	r.share = &voiceShare{principalID: f.ids["ann"], startedAt: f.clock.Now()}
	f.srv.voice.mu.Unlock()
	r.order.Unlock()
	r.claims.Unlock()
	<-passed
	if !f.granted(t, room, "ann") {
		t.Error("the pass withdrew a grant made after it decided")
	}
}

// TestVoiceShareAuditFailure: a claim whose row cannot be written does not
// start: it is answered 500, the sources are withdrawn, and nobody is shown
// sharing.
func TestVoiceShareAuditFailure(t *testing.T) {
	f, room := newShareFixture(t)
	write := f.srv.voice.appendAudit
	f.srv.voice.appendAudit = func(context.Context, string, string, string, string, time.Time) (store.AuditEvent, error) {
		return store.AuditEvent{}, errors.New("disk full")
	}
	res := f.claimShare(t, "ann", "ops")
	f.srv.voice.appendAudit = write
	if res.status != http.StatusInternalServerError {
		t.Fatalf("claim with the audit log failing: %d %s, want 500", res.status, res.body)
	}
	if f.granted(t, room, "ann") {
		t.Error("the sources of a share that never started were not withdrawn")
	}
	if holder := f.shareState(room); holder != 0 {
		t.Errorf("a share is held by %d", holder)
	}
	if got := f.sharing(t, f.ops); got != "-" {
		t.Errorf("presence shows %q sharing", got)
	}
	f.mustClaim(t, "ann")
}

// TestVoiceShareKeepsSecrets: no room name, token or secret in any share row,
// log line or answer, whichever way the share went.
func TestVoiceShareKeepsSecrets(t *testing.T) {
	f, room := newShareFixture(t)
	token := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Token
	var bodies []string
	keep := func(res wireResult) { bodies = append(bodies, res.body) }
	keep(f.claimShare(t, "ann", "ops"))
	keep(f.claimShare(t, "ann2", "ops"))
	keep(f.stopShare(t, "ann2", "ops"))
	keep(f.stopShare(t, "bob", "ops"))
	f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.screen = true })
	f.at(500)
	f.pass(t)
	f.lk.setUpdateStatus(http.StatusServiceUnavailable)
	keep(f.stopShare(t, "root", "ops"))
	keep(f.claimShare(t, "ann2", "ops"))
	f.lk.setUpdateStatus(0)
	f.at(16_000)
	f.pass(t)
	if code := f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok); code != 204 {
		t.Fatalf("remove status = %d", code)
	}

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
		"ann's login token": f.tokens["ann"], "the new room's name": f.room(t, f.ops),
	} {
		if strings.Contains(text, secret) {
			t.Errorf("%s appears in an audit row, log line or response", what)
		}
	}
	for _, exercised := range []string{
		store.AuditVoiceShareStarted, store.AuditVoiceShareStopped, store.AuditVoiceShareUnclaimed,
		"reason=operator", schema.ErrorCodeVoiceShareBusy, schema.ErrorCodeVoiceUnavailable, "withdrawing screen-share sources failed",
	} {
		if !strings.Contains(text, exercised) {
			t.Errorf("the run did not exercise %q", exercised)
		}
	}
}
