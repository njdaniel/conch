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

// sharingIn returns who presence, as viewer reads it, shows sharing in
// channel, and whether with sound; 0 when nobody is.
func sharingIn(viewer *person, channel string) (int64, bool, error) {
	doc, err := viewer.presence(channel)
	if err != nil {
		return 0, false, err
	}
	if err := doc.Validate(); err != nil {
		return 0, false, fmt.Errorf("presence does not validate: %w", err)
	}
	for _, r := range doc.Rooms {
		for _, p := range r.Participants {
			if p.Sharing {
				return p.PrincipalID, p.SharingAudio, nil
			}
		}
	}
	return 0, false, nil
}

// claimShare claims channel's share as p and returns the decoded answer.
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
		who, audio, err := sharingIn(val, "screen")
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
	if _, status, code, err := claimShare(val, "screen"); err != nil {
		return fmt.Errorf("val claims while sam shares: %w", err)
	} else if status != http.StatusConflict || code != schema.ErrorCodeVoiceShareBusy {
		return fmt.Errorf("val claims while sam shares: status %d code %q, want 409 %s", status, code, schema.ErrorCodeVoiceShareBusy)
	}
	h.say("ok   watched: val subscribed to sam's screen and its sound; val's own claim is busy")

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
		who, _, err := sharingIn(val, "screen")
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
	if err := d.operator.call(http.MethodDelete, "/v1/channels/screen/voice/share", nil, nil); err != nil {
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
