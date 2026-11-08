package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// sharer is one sharing participant of v2 presence.
type sharer struct {
	id      int64
	audio   bool
	started schema.Timestamp
	quality *schema.VoiceShareQuality
}

// presenceV2 reads channel's presence as p in v2 (issue #1483), decoded
// strictly and validated.
func (p *person) presenceV2(channel string) (schema.VoicePresenceV2, error) {
	var doc schema.VoicePresenceV2
	status, data, err := p.api.do(http.MethodGet, "/v1/channels/"+channel+"/voice?"+schema.VoicePresenceSchemaParam+"="+schema.VoicePresenceSchemaV2, nil)
	if err != nil {
		return doc, err
	}
	if status != http.StatusOK {
		return doc, fmt.Errorf("v2 presence for %s: status %d", channel, status)
	}
	if err := decodeStrict(data, &doc); err != nil {
		return doc, fmt.Errorf("v2 presence does not decode strictly: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return doc, fmt.Errorf("v2 presence does not validate: %w", err)
	}
	return doc, nil
}

// sharersIn returns who v2 presence, as viewer reads it, shows sharing in
// channel, by principal id.
func sharersIn(viewer *person, channel string) ([]sharer, error) {
	doc, err := viewer.presenceV2(channel)
	if err != nil {
		return nil, err
	}
	var out []sharer
	for _, r := range doc.Rooms {
		for _, p := range r.Participants {
			if p.Sharing {
				out = append(out, sharer{id: p.PrincipalID, audio: p.SharingAudio, started: *p.ShareStartedAt, quality: p.ShareQuality})
			}
		}
	}
	return out, nil
}

// soleSharerIn is sharingIn for a room that holds at most one share: who
// shares, and whether with sound; 0 when nobody does. Two is an error.
func soleSharerIn(viewer *person, channel string) (int64, bool, error) {
	got, err := sharersIn(viewer, channel)
	switch {
	case err != nil:
		return 0, false, err
	case len(got) > 1:
		return 0, false, fmt.Errorf("presence shows %d sharers, want at most one", len(got))
	case len(got) == 1:
		return got[0].id, got[0].audio, nil
	}
	return 0, false, nil
}

// v1SharersIn returns who v1 presence, as viewer reads it, shows sharing in
// channel (at most one, whatever the room holds).
func v1SharersIn(viewer *person, channel string) ([]int64, error) {
	doc, err := viewer.presence(channel)
	if err != nil {
		return nil, err
	}
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("v1 presence does not validate: %w", err)
	}
	var out []int64
	for _, r := range doc.Rooms {
		for _, p := range r.Participants {
			if p.Sharing {
				out = append(out, p.PrincipalID)
			}
		}
	}
	return out, nil
}

// claimShare claims a share in channel as p, with no body, and returns the
// decoded answer (a VoiceShareV1).
func claimShare(p *person, channel string) (schema.VoiceShareV1, int, string, error) {
	var share schema.VoiceShareV1
	status, data, err := p.api.do(http.MethodPost, "/v1/channels/"+channel+"/voice/share", nil)
	if err != nil || status != http.StatusOK {
		var e schema.Error
		_ = decodeStrict(data, &e)
		return share, status, e.Code, err
	}
	if err := decodeStrict(data, &share); err != nil {
		return share, status, "", fmt.Errorf("decode the share: %w", err)
	}
	if err := share.Validate(); err != nil {
		return share, status, "", fmt.Errorf("the share does not satisfy the schema: %w", err)
	}
	return share, status, "", nil
}

// screenShare: a member claims the share and publishes a screen and its
// sound, a second member watches, the share is stopped and its tracks
// disappear; before the claim, and for anyone else, LiveKit refuses those
// sources.
func (l *live) screenShare(ctx context.Context) error {
	h, d := l.h, l.d
	channelID, err := d.createChannel("screen")
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("channel:%d", channelID)
	sam, err := h.newPerson(d, "sam", "screen")
	if err != nil {
		return err
	}
	val, err := h.newPerson(d, "val", "screen")
	if err != nil {
		return err
	}
	una, err := h.newPerson(d, "una", "screen")
	if err != nil {
		return err
	}
	dir, err := h.mkdir("share-")
	if err != nil {
		return err
	}
	shareAudio := filepath.Join(dir, "share-audio.ogg")
	if err := lkit.WriteSilentOpus(shareAudio, 900); err != nil {
		return err
	}
	hostPort := strings.TrimPrefix(l.srv.WSURL, "ws://")

	// Before any claim, val tries to publish a screen and una its sound, each
	// on the join grant: LiveKit refuses both sources, and lk gives up when
	// its publish is never answered (10 s).
	type attempt struct {
		who  *person
		what string
		args []string
		lk   *lkit.Headless
	}
	attempts := []*attempt{
		{who: val, what: "a screen", args: []string{"--publish-demo"}},
		{who: una, what: "a screen's sound", args: []string{"--publish", shareAudio}},
	}
	var room string
	for _, a := range attempts {
		s, err := h.session(a.who, "screen")
		if err != nil {
			return err
		}
		room = s.Rooms[0].Room
		if a.lk, err = h.startLK(l.lkBin, hostPort, s.Rooms[0].Token, lkit.NewShareFilter(false).Filter, a.args...); err != nil {
			return err
		}
	}
	for _, a := range attempts {
		if err := waitForOrFail(a.who.name+"'s unclaimed publish of "+a.what+" to be refused", 45*time.Second, func() (bool, string, error) {
			if ok, p, _ := l.lkHas(room, a.who.id); ok {
				if screen, audio := lkit.ShareTracks(p); screen || audio {
					return false, "", fmt.Errorf("LiveKit published %s's share track without a claim: sources %v", a.who.name, p.Sources())
				}
			}
			return a.lk.Exited(), "lk is still trying to publish", nil
		}); err != nil {
			return err
		}
		if !strings.Contains(a.lk.Tail(), "timed out publishing track") {
			return fmt.Errorf("%s's lk exited without the refusal: %s", a.who.name, a.lk.Tail())
		}
		a.lk.Stop()
	}
	h.say("ok   unclaimed: LiveKit refused a screen (val) and a screen's sound (una) on the join grant (lk: timed out publishing track)")

	// sam joins. lk publishes the screen and its sound first; the relay holds
	// them back until the share is granted. Not for long: everything lk sends
	// after them waits too, its pings included. (Tried: held through the 10 s
	// above, the claim was granted but LiveKit sent the connection no new
	// token.) A browser claims as soon as it has joined, which is what this
	// does.
	ss, err := h.session(sam, "screen")
	if err != nil {
		return err
	}
	if ss.Rooms[0].Room != room {
		return errors.New("sam and val were given different rooms for one channel")
	}
	gate := lkit.NewShareFilter(true)
	samLK, err := h.startLK(l.lkBin, hostPort, ss.Rooms[0].Token, gate.Filter, "--publish-demo", "--publish", shareAudio, "--publish", l.ogg)
	if err != nil {
		return err
	}
	defer samLK.Stop()
	var samLive lkit.Participant
	if err := waitForOrFail("LiveKit to list sam in screen's room", 60*time.Second, func() (bool, string, error) {
		ok, p, saw := l.lkHas(room, sam.id)
		if !ok && samLK.Exited() {
			return false, "", errors.New("lk exited: " + samLK.Tail())
		}
		samLive = p
		return ok, saw, nil
	}); err != nil {
		return err
	}
	select {
	case <-gate.Held():
	case <-time.After(30 * time.Second):
		return errors.New("lk did not try to publish its share tracks within 30 s")
	}
	if err := checkSharePermission(samLive.Permission, false); err != nil {
		return fmt.Errorf("before the claim, LiveKit's permission for sam: %w", err)
	}
	h.say("ok   share set-up: sam joined screen's room on conchd's token; LiveKit holds the join grant exactly (microphone only)")

	// sam claims the share.
	before := len(samLK.Relay.IssuedTokens())
	share, status, code, err := claimShare(sam, "screen")
	if err != nil {
		return fmt.Errorf("sam claims the share: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("sam claims the share: status %d code %q", status, code)
	}
	if share.PrincipalID != sam.id || !share.Audio {
		return fmt.Errorf("sam's share = %+v, want sam's, with sound", share)
	}
	if ok, p, saw := l.lkHas(room, sam.id); !ok {
		return fmt.Errorf("sam left the room during the claim: %s", saw)
	} else if err := checkSharePermission(p.Permission, true); err != nil {
		return fmt.Errorf("after the claim, LiveKit's permission for sam: %w", err)
	}
	var refreshed string
	if err := waitFor("LiveKit to send sam a token carrying the share", 10*time.Second, func() (bool, string) {
		toks := samLK.Relay.IssuedTokens()
		if len(toks) > before {
			refreshed = toks[len(toks)-1]
			return true, ""
		}
		return false, fmt.Sprintf("%d tokens sent, none since the claim", len(toks))
	}); err != nil {
		return err
	}
	h.secret("a LiveKit-issued token", refreshed)
	if err := h.checkLiveKitToken(refreshed, d.lk, identity(sam.id), room, true); err != nil {
		return fmt.Errorf("the token LiveKit sent sam after the claim: %w", err)
	}

	// The share is published on the same connection.
	gate.Release()
	if err := waitForOrFail("LiveKit to list sam's screen and its sound", 30*time.Second, func() (bool, string, error) {
		ok, p, saw := l.lkHas(room, sam.id)
		if !ok {
			if samLK.Exited() {
				return false, "", errors.New("lk exited: " + samLK.Tail())
			}
			return false, saw, nil
		}
		screen, audio := lkit.ShareTracks(p)
		return screen && audio, fmt.Sprintf("sources %v", p.Sources()), nil
	}); err != nil {
		return err
	}
	if err := waitFor("val's presence to show sam sharing with sound", 30*time.Second, func() (bool, string) {
		who, audio, err := soleSharerIn(val, "screen")
		if err != nil {
			return false, err.Error()
		}
		return who == sam.id && audio, fmt.Sprintf("sharing=%d audio=%v", who, audio)
	}); err != nil {
		return err
	}
	h.say("ok   claimed: sam's claim set exactly the share's permission in LiveKit and its fresh token; sam published a screen and its sound; presence shows it")

	// val watches.
	sv, err := h.session(val, "screen")
	if err != nil {
		return err
	}
	viewer, err := h.startLK(l.lkBin, hostPort, sv.Rooms[0].Token, nil, "--auto-subscribe")
	if err != nil {
		return err
	}
	defer viewer.Stop()
	samIdentity := `"participant": "` + identity(sam.id) + `"`
	if err := waitFor("val to be subscribed to sam's screen and its sound", 30*time.Second, func() (bool, string) {
		got := viewer.LogLines("track subscribed", samIdentity)
		return lkit.ContainsAll(got, `"source": "SCREEN_SHARE",`, `"source": "SCREEN_SHARE_AUDIO",`), fmt.Sprintf("%d subscriptions to sam's tracks", len(got))
	}); err != nil {
		return err
	}
	// A second claim while sam shares succeeds now (several shares at once,
	// issue #1483): val claims, sees it granted, and lets it go with its
	// started_at, so that what follows is sam's alone.
	valShare, status, code, err := claimShare(val, "screen")
	if err != nil {
		return fmt.Errorf("val claims while sam shares: %w", err)
	} else if status != http.StatusOK || valShare.PrincipalID != val.id {
		return fmt.Errorf("val claims while sam shares: status %d code %q, want 200", status, code)
	}
	if err := releaseShare(val, "screen", valShare.StartedAt); err != nil {
		return err
	}
	h.say("ok   watched: val subscribed to sam's screen and its sound; val's own claim beside sam's succeeded")

	// sam stops: the tracks are gone for the viewer, at once.
	before = len(samLK.Relay.IssuedTokens())
	if err := sam.api.call(http.MethodDelete, "/v1/channels/screen/voice/share", nil, nil); err != nil {
		return fmt.Errorf("sam stops the share: %w", err)
	}
	if ok, p, saw := l.lkHas(room, sam.id); !ok {
		return fmt.Errorf("sam left the room when the share stopped: %s", saw)
	} else if screen, audio := lkit.ShareTracks(p); screen || audio {
		return fmt.Errorf("LiveKit still lists sam's share tracks after the stop answered: sources %v", p.Sources())
	} else if err := checkSharePermission(p.Permission, false); err != nil {
		return fmt.Errorf("after the stop, LiveKit's permission for sam: %w", err)
	}
	if err := waitFor("val to be told sam's share tracks are gone", 15*time.Second, func() (bool, string) {
		got := viewer.LogLines("track unpublished", samIdentity)
		return lkit.ContainsAll(got, `"source": "SCREEN_SHARE",`, `"source": "SCREEN_SHARE_AUDIO",`), fmt.Sprintf("%d of sam's tracks unpublished", len(got))
	}); err != nil {
		return err
	}
	if err := waitFor("val's presence to show nobody sharing", 15*time.Second, func() (bool, string) {
		who, _, err := soleSharerIn(val, "screen")
		if err != nil {
			return false, err.Error()
		}
		return who == 0, fmt.Sprintf("sharing=%d", who)
	}); err != nil {
		return err
	}
	if err := waitFor("LiveKit to send sam a token without the share", 10*time.Second, func() (bool, string) {
		toks := samLK.Relay.IssuedTokens()
		if len(toks) > before {
			refreshed = toks[len(toks)-1]
			return true, ""
		}
		return false, "no token since the stop"
	}); err != nil {
		return err
	}
	h.secret("a LiveKit-issued token", refreshed)
	if err := h.checkLiveKitToken(refreshed, d.lk, identity(sam.id), room, false); err != nil {
		return fmt.Errorf("the token LiveKit sent sam after the stop: %w", err)
	}
	h.say("ok   stopped: the share's tracks left LiveKit before the stop answered, val was told, presence says so, and sam's permission and token are the join grant again")

	// Once it has stopped, the next claim succeeds, and an operator who is
	// not a member stops it.
	if _, status, code, err := claimShare(val, "screen"); err != nil {
		return fmt.Errorf("val claims after sam stopped: %w", err)
	} else if status != http.StatusOK {
		return fmt.Errorf("val claims after sam stopped: status %d code %q", status, code)
	}
	if err := d.operator.call(http.MethodDelete, fmt.Sprintf("/v1/channels/screen/voice/share?%s=%d", schema.VoiceSharePrincipalParam, val.id), nil, nil); err != nil {
		return fmt.Errorf("the operator stops val's share: %w", err)
	}
	if ok, p, _ := l.lkHas(room, val.id); ok {
		if err := checkSharePermission(p.Permission, false); err != nil {
			return fmt.Errorf("after the operator's stop, LiveKit's permission for val: %w", err)
		}
	}

	// The audit log: each start and stop, with its reason, nothing unclaimed
	// (nothing was ever published without a claim), and no room name (the
	// final scan covers every row).
	var got []string
	all, err := d.audit()
	if err != nil {
		return err
	}
	names := map[string]string{fmt.Sprintf("principal:%d", sam.id): "sam", fmt.Sprintf("principal:%d", val.id): "val"}
	for _, e := range all {
		if !strings.HasPrefix(e.Action, "voice_share_") || e.Subject != subject {
			continue
		}
		who, ok := names[e.Actor]
		if !ok {
			who = "operator"
		}
		detail := strings.NewReplacer(fmt.Sprintf("principal=%d", sam.id), "principal=sam", fmt.Sprintf("principal=%d", val.id), "principal=val").Replace(e.Detail)
		got = append(got, who+" "+e.Action+" "+strings.TrimPrefix(detail, fmt.Sprintf("channel=%d ", channelID)))
	}
	want := []string{
		"sam " + store.AuditVoiceShareStarted + " audio=true",
		"val " + store.AuditVoiceShareStarted + " audio=true",
		"val " + store.AuditVoiceShareStopped + " principal=val reason=self",
		"sam " + store.AuditVoiceShareStopped + " principal=sam reason=self",
		"val " + store.AuditVoiceShareStarted + " audio=true",
		"operator " + store.AuditVoiceShareStopped + " principal=val reason=operator",
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("share audit rows %q, want %q", got, want)
	}
	h.say("ok   audited: voice_share_started and voice_share_stopped (self, operator), and no voice_share_unclaimed")
	return nil
}

// releaseShare sends DELETE …/voice/share as p naming their own claim, and
// requires 204.
func releaseShare(p *person, channel string, started schema.Timestamp) error {
	query := schema.VoiceShareClaimParam + "=" + started.Time().Format(time.RFC3339Nano)
	status, data, err := p.api.do(http.MethodDelete, "/v1/channels/"+channel+"/voice/share?"+query, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		var e schema.Error
		_ = decodeStrict(data, &e)
		return fmt.Errorf("%s releases their share: status %d code %q, want 204", p.name, status, e.Code)
	}
	return nil
}

// multiSharer is one member of the multishare scenario with an lk
// connection of their own, held at the gate until their claim is granted.
type multiSharer struct {
	p    *person
	lk   *lkit.Headless
	gate *lkit.ShareFilter
	// tokens is how many tokens LiveKit had sent them when they joined.
	tokens int
}

// claimShareWith claims a share in channel as p with a quality body and
// returns the status, the error code and the raw answer.
func claimShareWith(p *person, channel string, req schema.VoiceShareRequestV1) (int, string, []byte, error) {
	status, data, err := p.api.do(http.MethodPost, "/v1/channels/"+channel+"/voice/share", req)
	if err != nil {
		return status, "", data, err
	}
	var e schema.Error
	if status != http.StatusOK {
		_ = decodeStrict(data, &e)
	}
	return status, e.Code, data, nil
}

// multiShare: several shares at once in one room (issue #1483, design
// voice-multi-share.md §9.3). Sam and val both claim and both publish a
// screen, val with sound; una sees both in v2 presence and is subscribed to
// both, while a v1 reader sees exactly one; sam changes his quality with no
// new row; the room fills to six and the seventh claim is refused without a
// row; sam's stop leaves val's stream standing and frees a place; an operator
// who is not a member stops val's stream by naming them; a late release naming
// an ended claim is stale.
func (l *live) multiShare(ctx context.Context) error {
	h, d := l.h, l.d
	channelID, err := d.createChannel("multi")
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("channel:%d", channelID)
	sam, err := h.newPerson(d, "msam", "multi")
	if err != nil {
		return err
	}
	val, err := h.newPerson(d, "mval", "multi")
	if err != nil {
		return err
	}
	una, err := h.newPerson(d, "muna", "multi")
	if err != nil {
		return err
	}
	dir, err := h.mkdir("multishare-")
	if err != nil {
		return err
	}
	shareAudio := filepath.Join(dir, "share-audio.ogg")
	if err := lkit.WriteSilentOpus(shareAudio, 900); err != nil {
		return err
	}
	hostPort := strings.TrimPrefix(l.srv.WSURL, "ws://")

	// join connects p with lk, publishing args once the gate lets them, and
	// waits until LiveKit lists them and lk has tried to publish.
	var room string
	join := func(p *person, args ...string) (*multiSharer, error) {
		s, err := h.session(p, "multi")
		if err != nil {
			return nil, err
		}
		room = s.Rooms[0].Room
		gate := lkit.NewShareFilter(true)
		hl, err := h.startLK(l.lkBin, hostPort, s.Rooms[0].Token, gate.Filter, args...)
		if err != nil {
			return nil, err
		}
		if err := waitForOrFail("LiveKit to list "+p.name, 60*time.Second, func() (bool, string, error) {
			ok, _, saw := l.lkHas(room, p.id)
			if !ok && hl.Exited() {
				return false, "", errors.New("lk exited: " + hl.Tail())
			}
			return ok, saw, nil
		}); err != nil {
			return nil, err
		}
		if len(args) > 0 {
			select {
			case <-gate.Held():
			case <-time.After(30 * time.Second):
				return nil, fmt.Errorf("%s's lk did not try to publish within 30 s", p.name)
			}
		}
		return &multiSharer{p: p, lk: hl, gate: gate, tokens: len(hl.Relay.IssuedTokens())}, nil
	}
	published := func(m *multiSharer, audio bool) error {
		// As screenShare does: LiveKit sends the connection a token carrying
		// the share once the claim is granted, and only then are the held
		// publishes released.
		if err := waitFor("LiveKit to send "+m.p.name+" a token carrying the share", 10*time.Second, func() (bool, string) {
			return len(m.lk.Relay.IssuedTokens()) > m.tokens, "no token since the claim"
		}); err != nil {
			return err
		}
		m.gate.Release()
		return waitForOrFail("LiveKit to list "+m.p.name+"'s screen", 30*time.Second, func() (bool, string, error) {
			ok, p, saw := l.lkHas(room, m.p.id)
			if !ok {
				if m.lk.Exited() {
					return false, "", errors.New("lk exited: " + m.lk.Tail())
				}
				return false, saw, nil
			}
			screen, snd := lkit.ShareTracks(p)
			return screen && (snd || !audio), fmt.Sprintf("sources %v", p.Sources()), nil
		})
	}

	samM, err := join(sam, "--publish-demo")
	if err != nil {
		return err
	}
	defer samM.lk.Stop()
	valM, err := join(val, "--publish-demo", "--publish", shareAudio)
	if err != nil {
		return err
	}
	defer valM.lk.Stop()

	// Both claim: sam with a quality, val without a body. sam's answer is a
	// VoiceShareV2, val's a VoiceShareV1 (strict decodes: a closed reader sees
	// no field it does not know).
	q1080 := schema.VoiceShareQuality{Resolution: schema.VoiceShareResolution1080p, FrameRate: 60}
	status, code, data, err := claimShareWith(sam, "multi", schema.VoiceShareRequestV1{Quality: &q1080})
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("sam claims with a quality: status %d code %q: %w", status, code, err)
	}
	var samShare schema.VoiceShareV2
	if err := decodeStrict(data, &samShare); err != nil {
		return fmt.Errorf("sam's answer is not a VoiceShareV2: %w", err)
	}
	if err := samShare.Validate(); err != nil || samShare.PrincipalID != sam.id || samShare.Quality != q1080 {
		return fmt.Errorf("sam's answer %+v: %w", samShare, err)
	}
	valShare, status, code, err := claimShare(val, "multi")
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("val claims beside sam: status %d code %q: %w", status, code, err)
	}
	if valShare.PrincipalID != val.id || !valShare.StartedAt.Time().After(samShare.StartedAt.Time()) {
		return fmt.Errorf("val's share %+v, want val's, started after sam's %s", valShare, samShare.StartedAt.Time())
	}
	if err := published(samM, false); err != nil {
		return err
	}
	if err := published(valM, true); err != nil {
		return err
	}
	h.say("ok   two shares: sam (1080p/60, answered VoiceShareV2) and val (no body, VoiceShareV1) both claimed and both published a screen, val with sound")

	// una watches both and reads v2 presence.
	su, err := h.session(una, "multi")
	if err != nil {
		return err
	}
	viewer, err := h.startLK(l.lkBin, hostPort, su.Rooms[0].Token, nil, "--auto-subscribe")
	if err != nil {
		return err
	}
	defer viewer.Stop()
	for _, who := range []*person{sam, val} {
		ident := `"participant": "` + identity(who.id) + `"`
		if err := waitFor("una to be subscribed to "+who.name+"'s screen", 30*time.Second, func() (bool, string) {
			got := viewer.LogLines("track subscribed", ident)
			return lkit.ContainsAll(got, `"source": "SCREEN_SHARE",`), fmt.Sprintf("%d subscriptions to %s's tracks", len(got), who.name)
		}); err != nil {
			return err
		}
	}
	var seen []sharer
	if err := waitFor("una's v2 presence to show sam and val sharing", 30*time.Second, func() (bool, string) {
		got, err := sharersIn(una, "multi")
		if err != nil {
			return false, err.Error()
		}
		seen = got
		return len(got) == 2, fmt.Sprintf("%d sharers", len(got))
	}); err != nil {
		return err
	}
	byID := map[int64]sharer{}
	for _, s := range seen {
		byID[s.id] = s
	}
	sv, vv := byID[sam.id], byID[val.id]
	switch {
	case sv.id == 0 || vv.id == 0:
		return fmt.Errorf("v2 presence shows %+v, want sam and val", seen)
	case !sv.started.Time().Equal(samShare.StartedAt.Time()) || !vv.started.Time().Equal(valShare.StartedAt.Time()) || sv.started.Time().Equal(vv.started.Time()):
		return fmt.Errorf("share_started_at: sam %s val %s, want their claims' distinct times", sv.started.Time(), vv.started.Time())
	case sv.quality == nil || *sv.quality != q1080 || vv.quality != nil:
		return fmt.Errorf("share_quality: sam %+v val %+v, want sam's 1080p/60 and none on val", sv.quality, vv.quality)
	case !vv.audio:
		return errors.New("v2 presence does not show val's sound")
	}
	one, err := v1SharersIn(una, "multi")
	if err != nil {
		return err
	}
	if len(one) != 1 || one[0] != sam.id {
		return fmt.Errorf("v1 presence shows %v sharing, want sam alone (the earliest-started)", one)
	}
	h.say("ok   watched: una is subscribed to both screens; v2 presence lists both with their share_started_at, sam's share_quality and none on val; v1 lists sam alone and validates")

	// sam changes his quality: a presence frame, no new row.
	rows, err := d.auditRows(store.AuditVoiceShareStarted)
	if err != nil {
		return err
	}
	q720 := schema.VoiceShareQuality{Resolution: schema.VoiceShareResolution720p, FrameRate: 30}
	status, code, data, err = claimShareWith(sam, "multi", schema.VoiceShareRequestV1{Quality: &q720})
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("sam changes his quality: status %d code %q: %w", status, code, err)
	}
	var changed schema.VoiceShareV2
	if err := decodeStrict(data, &changed); err != nil || changed.Quality != q720 || !changed.StartedAt.Time().Equal(samShare.StartedAt.Time()) {
		return fmt.Errorf("sam's repeat answered %s (%w), want the same share at 720p/30", data, err)
	}
	if err := waitFor("una's next frame to show sam at 720p/30", 15*time.Second, func() (bool, string) {
		got, err := sharersIn(una, "multi")
		if err != nil {
			return false, err.Error()
		}
		for _, s := range got {
			if s.id == sam.id {
				return s.quality != nil && *s.quality == q720, fmt.Sprintf("quality %+v", s.quality)
			}
		}
		return false, "sam not sharing"
	}); err != nil {
		return err
	}
	if after, err := d.auditRows(store.AuditVoiceShareStarted); err != nil || len(after) != len(rows) {
		return fmt.Errorf("a quality change wrote a voice_share_started row (%d rows, had %d): %w", len(after), len(rows), err)
	}
	h.say("ok   quality: sam's repeat with 720p/30 changed what una reads and wrote no row")

	// Four more members claim: the room holds six. The seventh is refused
	// voice_share_full, with no row and no change in LiveKit.
	var extras []*person
	for i := 1; i <= 5; i++ {
		e, err := h.newPerson(d, fmt.Sprintf("mx%d", i), "multi")
		if err != nil {
			return err
		}
		m, err := join(e)
		if err != nil {
			return err
		}
		defer m.lk.Stop()
		extras = append(extras, e)
	}
	for _, e := range extras[:4] {
		if _, status, code, err := claimShare(e, "multi"); err != nil || status != http.StatusOK {
			return fmt.Errorf("%s claims (the room holds fewer than six): status %d code %q: %w", e.name, status, code, err)
		}
	}
	started, err := d.auditRows(store.AuditVoiceShareStarted)
	if err != nil {
		return err
	}
	seventh := extras[4]
	for _, req := range []schema.VoiceShareRequestV1{{}, {Quality: &q720}} {
		status, code, _, err := claimShareWith(seventh, "multi", req)
		if err != nil {
			return err
		}
		if status != http.StatusConflict || code != schema.ErrorCodeVoiceShareFull {
			return fmt.Errorf("the seventh claim: status %d code %q, want 409 %s", status, code, schema.ErrorCodeVoiceShareFull)
		}
	}
	if after, err := d.auditRows(store.AuditVoiceShareStarted); err != nil || len(after) != len(started) {
		return fmt.Errorf("a refused claim wrote a row (%d rows, had %d): %w", len(after), len(started), err)
	}
	if ok, p, _ := l.lkHas(room, seventh.id); ok {
		if err := checkSharePermission(p.Permission, false); err != nil {
			return fmt.Errorf("the refused claimant's permission: %w", err)
		}
	}
	h.say("ok   cap: six shares are granted; the seventh claim is 409 voice_share_full, writes no row and grants nothing")

	// sam stops with his started_at: his tracks are gone for una at once,
	// val's are still listed, v1 now shows val, and the seventh claim works.
	if err := releaseShare(sam, "multi", samShare.StartedAt); err != nil {
		return err
	}
	if ok, p, saw := l.lkHas(room, sam.id); !ok {
		return fmt.Errorf("sam left the room when his share stopped: %s", saw)
	} else if screen, _ := lkit.ShareTracks(p); screen {
		return fmt.Errorf("LiveKit still lists sam's screen after his stop answered: %v", p.Sources())
	}
	if ok, p, saw := l.lkHas(room, val.id); !ok {
		return fmt.Errorf("val left the room when sam's share stopped: %s", saw)
	} else if screen, audio := lkit.ShareTracks(p); !screen || !audio {
		return fmt.Errorf("sam's stop touched val's stream: %v", p.Sources())
	}
	if err := waitFor("una to be told sam's screen is gone", 15*time.Second, func() (bool, string) {
		got := viewer.LogLines("track unpublished", `"participant": "`+identity(sam.id)+`"`)
		return lkit.ContainsAll(got, `"source": "SCREEN_SHARE",`), fmt.Sprintf("%d of sam's tracks unpublished", len(got))
	}); err != nil {
		return err
	}
	if err := waitFor("the v1 reader to be shown val", 15*time.Second, func() (bool, string) {
		got, err := v1SharersIn(una, "multi")
		if err != nil {
			return false, err.Error()
		}
		return len(got) == 1 && got[0] == val.id, fmt.Sprintf("v1 shows %v", got)
	}); err != nil {
		return err
	}
	if _, status, code, err := claimShare(seventh, "multi"); err != nil || status != http.StatusOK {
		return fmt.Errorf("the seventh claim after sam's stop: status %d code %q: %w", status, code, err)
	}
	h.say("ok   stopped: sam's stop took his tracks from una at once, left val's stream standing, moved the v1 reader to val and freed a place")

	// An operator who is not a member stops val's stream by naming them; a
	// late release naming sam's ended claim is stale.
	if err := d.operator.call(http.MethodDelete, fmt.Sprintf("/v1/channels/multi/voice/share?%s=%d", schema.VoiceSharePrincipalParam, val.id), nil, nil); err != nil {
		return fmt.Errorf("the operator stops val's share: %w", err)
	}
	if ok, p, saw := l.lkHas(room, val.id); !ok {
		return fmt.Errorf("val left the room when the operator stopped the share: %s", saw)
	} else if screen, audio := lkit.ShareTracks(p); screen || audio {
		return fmt.Errorf("LiveKit still lists val's tracks after the operator's stop answered: %v", p.Sources())
	}
	query := schema.VoiceShareClaimParam + "=" + samShare.StartedAt.Time().Format(time.RFC3339Nano)
	if status, code, err := sam.api.refusal(http.MethodDelete, "/v1/channels/multi/voice/share?"+query); err != nil || status != http.StatusConflict || code != schema.ErrorCodeVoiceShareStale {
		return fmt.Errorf("sam's late release: status %d code %q (%w), want 409 %s", status, code, err, schema.ErrorCodeVoiceShareStale)
	}
	h.say("ok   moderated: an operator who is not a member stopped val's stream with ?principal=; a late release naming sam's ended claim is voice_share_stale")

	// The rows: every started stream has its own stopped row, with the
	// quality on the started row and the operator as the actor of val's.
	all, err := d.audit()
	if err != nil {
		return err
	}
	var starts, stops []string
	for _, e := range all {
		if e.Subject != subject {
			continue
		}
		switch e.Action {
		case store.AuditVoiceShareStarted:
			starts = append(starts, e.Detail)
		case store.AuditVoiceShareStopped:
			stops = append(stops, e.Detail)
		}
	}
	if len(starts) != 7 {
		return fmt.Errorf("%d started rows %q, want sam, val, four more and the seventh's retry", len(starts), starts)
	}
	if want := fmt.Sprintf("channel=%d audio=true quality=1080p/60", channelID); starts[0] != want {
		return fmt.Errorf("sam's started row %q, want %q", starts[0], want)
	}
	if want := fmt.Sprintf("channel=%d principal=%d reason=operator", channelID, val.id); !slices.Contains(stops, want) {
		return fmt.Errorf("stopped rows %q lack %q", stops, want)
	}
	if want := fmt.Sprintf("channel=%d principal=%d reason=self", channelID, sam.id); !slices.Contains(stops, want) {
		return fmt.Errorf("stopped rows %q lack %q", stops, want)
	}
	h.say("ok   audited: seven voice_share_started rows (sam's with quality=1080p/60), sam's stop reason=self and val's reason=operator, nothing for the refused claims")
	return nil
}
