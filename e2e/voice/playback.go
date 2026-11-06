package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/pkg/schema"
)

// The playback-owner scenario (issue #935, docs/design/windows-desktop.md
// §7.3 F8): H7's companion publishes a tone as screen_share_audio. Under B2
// H8's companion, not H8's helper, receives it; under B1 the helper receives
// it exactly once and the companion receives nothing. H7 never hears its own
// companion, and a helper with no companion keeps hearing share audio.
//
// lk joins with autoSubscribe false unless --auto-subscribe is passed. For the
// B2 companion, the scenario asks LiveKit to subscribe that connection to C7's
// screen tracks alone, the server-side equivalent of the JS companion's
// selective setSubscribed calls. The lk log is the receipt of what arrived.

const playbackChannel = "playback"

func (l *live) startPlaybackVoice(p *person, mic string, tone int, owner string) (*voiceClient, error) {
	var options []string
	if owner != "" {
		options = []string{"--share-audio-owner", owner}
	}
	started := time.Now()
	c, err := l.startVoiceClient(p, playbackChannel, mic, tone, options...)
	if err != nil {
		return nil, err
	}
	if _, err := c.wait("connected", 60*time.Second, started, connectionConnected); err != nil {
		return nil, err
	}
	if mic != "none" {
		if _, err := c.wait("ready to talk", 30*time.Second, started, readyToTalk); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func stopPlaybackVoice(ctx context.Context, c *voiceClient) error {
	if err := c.say("quit"); err != nil {
		return err
	}
	select {
	case <-c.done:
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s's conch-voice was still running 30 s after quit", c.who.name)
	case <-ctx.Done():
		return errInterrupted
	}
	if c.exit != nil {
		return fmt.Errorf("%s's conch-voice ended with %s after quit; standard error: %s", c.who.name, exitText(c.exit), c.stderrTail())
	}
	return nil
}

// publishPlaybackShare connects p's B2 companion, claims the share, consents
// to its sound, and releases the held lk publish only after LiveKit has both
// permissions. It returns the two published track IDs.
func (l *live) publishPlaybackShare(p *person, room, audioPath string) (*lkit.Headless, string, string, error) {
	h := l.h
	comp, err := h.mustCompanion(p, schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishScreen, Subscribe: true}, playbackChannel)
	if err != nil {
		return nil, "", "", err
	}
	if comp.Room != room {
		return nil, "", "", errors.New("the playback companion was given another room than the person's")
	}
	gate := lkit.NewShareFilter(true)
	hl, err := h.startLK(l.lkBin, strings.TrimPrefix(l.srv.WSURL, "ws://"), comp.Token, gate.Filter, "--publish-demo", "--publish", audioPath)
	if err != nil {
		return nil, "", "", err
	}
	if err := l.lkWaitHas(p.name+"'s playback companion", room, comp.Identity, hl); err != nil {
		hl.Stop()
		return nil, "", "", err
	}
	select {
	case <-gate.Held():
	case <-time.After(30 * time.Second):
		hl.Stop()
		return nil, "", "", errors.New("the playback companion's lk did not try to publish within 30 s")
	}

	before := len(hl.Relay.IssuedTokens())
	share, status, code, err := claimShare(p, playbackChannel)
	if err != nil {
		hl.Stop()
		return nil, "", "", err
	}
	if status != http.StatusOK || share.PrincipalID != p.id {
		hl.Stop()
		return nil, "", "", fmt.Errorf("%s claims the playback share: status %d code %q share %+v", p.name, status, code, share)
	}
	var audio schema.VoiceCompanionAudioV1
	if err := waitForOrFail(p.name+"'s playback audio consent", 20*time.Second, func() (bool, string, error) {
		a, status, code, err := requestCompanionAudio(p, schema.VoiceShareAudioScopeApplication, playbackChannel)
		if err != nil {
			return false, "", err
		}
		if status == http.StatusOK {
			audio = a
			return true, "", nil
		}
		if status == http.StatusConflict && code == schema.ErrorCodeVoiceNoSession {
			return false, fmt.Sprintf("status %d %s", status, code), nil
		}
		return false, "", fmt.Errorf("status %d code %q", status, code)
	}); err != nil {
		hl.Stop()
		return nil, "", "", err
	}
	if audio.Scope != schema.VoiceShareAudioScopeApplication || !audio.StartedAt.Time().Equal(share.StartedAt.Time()) {
		hl.Stop()
		return nil, "", "", fmt.Errorf("the playback consent answered %+v, want application and the share's started_at %s", audio, share.StartedAt.Time())
	}
	if err := waitFor("LiveKit to grant the playback companion screen and sound", 20*time.Second, func() (bool, string) {
		ok, part, saw := l.lkHasIdentity(room, comp.Identity)
		if !ok {
			return false, saw
		}
		return checkCompanionSources(part.Permission, true, "SCREEN_SHARE", "SCREEN_SHARE_AUDIO") == nil, fmt.Sprint(part.Permission)
	}); err != nil {
		hl.Stop()
		return nil, "", "", err
	}
	if err := waitFor("LiveKit to refresh the playback companion's token", 10*time.Second, func() (bool, string) {
		return len(hl.Relay.IssuedTokens()) > before, "no token since the audio consent"
	}); err != nil {
		hl.Stop()
		return nil, "", "", err
	}
	gate.Release()

	var screenSID, audioSID string
	if err := waitForOrFail("LiveKit to list the playback companion's screen and tone", 30*time.Second, func() (bool, string, error) {
		ok, part, saw := l.lkHasIdentity(room, comp.Identity)
		if !ok {
			if hl.Exited() {
				return false, "", errors.New("lk exited: " + hl.Tail())
			}
			return false, saw, nil
		}
		for _, track := range part.Tracks {
			switch fmt.Sprint(track.Source) {
			case "SCREEN_SHARE", "3":
				screenSID = track.SID
			case "SCREEN_SHARE_AUDIO", "4":
				audioSID = track.SID
			}
		}
		return screenSID != "" && audioSID != "", fmt.Sprintf("sources %v", part.Sources()), nil
	}); err != nil {
		hl.Stop()
		return nil, "", "", err
	}
	return hl, screenSID, audioSID, nil
}

// subscribePlaybackTracks makes the already-connected, non-auto-subscribing
// companion subscribe to exactly these tracks. The API secret is in lk's
// environment, not its command line; it is already in the leak registry.
func (l *live) subscribePlaybackTracks(room, identity string, sids ...string) error {
	args := []string{"room", "update-subscriptions", "--room", room, "--identity", identity, "--subscribe"}
	cmd := lkit.GroupCommand(stopCtx, l.lkBin, append(args, sids...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Dir(l.lkBin),
		"TMPDIR=" + filepath.Dir(l.lkBin),
		"LIVEKIT_URL=" + l.srv.APIURL,
		"LIVEKIT_API_KEY=" + l.srv.Key,
		"LIVEKIT_API_SECRET=" + l.srv.Secret,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("subscribing %s to the companion's screen tracks: %w: %s", identity, err, l.h.redact(string(out)))
	}
	return nil
}

func waitCompanionTracks(hl *lkit.Headless, participant string) error {
	return waitFor("the B2 companion to receive C7's screen and tone", 30*time.Second, func() (bool, string) {
		got := hl.LogLines("track subscribed", `"participant": "`+participant+`"`)
		return lkit.ContainsAll(got, `"source": "SCREEN_SHARE",`, `"source": "SCREEN_SHARE_AUDIO",`), fmt.Sprintf("%d subscriptions", len(got))
	})
}

const (
	playbackFramesPerSecond = 100.0
	playbackCountTolerance  = 1.25
	playbackLevelTolerance  = 0.25
)

func playbackReady(c *voiceClient, what string, since time.Time, want map[string]int) error {
	if len(want) == 0 {
		return nil
	}
	return waitForOrFail(what+": "+c.who.name+"'s first audible frames of every expected tone", 20*time.Second, func() (bool, string, error) {
		stats := c.statsWithin(since, time.Now())
		if c.exited() {
			return false, "", fmt.Errorf("the client exited (%s); standard error: %s", exitText(c.exit), c.stderrTail())
		}
		if len(stats) == 0 {
			return false, "no whole stats line", nil
		}
		line := stats[len(stats)-1]
		found := 0
		speakers, _ := line["speakers"].([]any)
		for _, raw := range speakers {
			speaker, _ := raw.(map[string]any)
			id, _ := speaker["speaker"].(string)
			tone, wanted := want[id]
			if wanted && number(speaker["audible_frames"]) > 0 && number(speaker["dominant_hz"]) == float64(tone) {
				found++
			}
		}
		mix, _ := line["mix"].(map[string]any)
		ready := found == len(want) && number(mix["audible_frames"]) > 0
		return ready, fmt.Sprintf("%d of %d expected tones audible", found, len(want)), nil
	})
}

// playbackHeard bounds both the number of copies received and the level played.
// A quarter-scale sine has RMS 0.25/sqrt(2); independent tones add in power.
func playbackHeard(c *voiceClient, what string, from, to time.Time, want map[string]int, publisher *voiceClient, talking bool) error {
	if err := c.heard(what, from, to, 2, want); err != nil {
		return err
	}
	if len(want) == 0 {
		return nil
	}
	stats := c.statsWithin(from, to)
	totals := map[string]float64{}
	for _, line := range stats {
		speakers, _ := line["speakers"].([]any)
		for _, raw := range speakers {
			speaker, _ := raw.(map[string]any)
			id, _ := speaker["speaker"].(string)
			if _, wanted := want[id]; wanted {
				totals[id] += number(speaker["frames"])
			}
		}
		mix, _ := line["mix"].(map[string]any)
		rms := number(mix["rms"])
		reference := math.Sqrt(float64(len(want))) * 0.25 / math.Sqrt2
		tolerance := reference * playbackLevelTolerance
		if math.Abs(rms-reference) > tolerance {
			return fmt.Errorf("%s: %s's mix RMS is %.5f, want %.5f ± %.5f for one copy of each tone (possible double playback)", what, c.who.name, rms, reference, tolerance)
		}
	}

	microphone := ""
	published, publisherLines := 0.0, 0
	if talking {
		microphone = identity(publisher.who.id)
		for _, line := range publisher.statsWithin(from, to) {
			published += number(line["frames_sent"])
			publisherLines++
		}
	}
	for id, frames := range totals {
		upper := playbackFramesPerSecond * float64(len(stats)) * playbackCountTolerance
		if id == microphone && publisherLines > 0 {
			upper = published / float64(publisherLines) * float64(len(stats)) * playbackCountTolerance
		}
		if frames > upper {
			return fmt.Errorf("%s: %s counted %.0f frames from %s over %d stats lines, want at most %.0f from one publisher with 25%% tolerance (possible double playback)", what, c.who.name, frames, id, len(stats), upper)
		}
	}
	return nil
}

// playbackWindow waits for each expected receiver to hear its tones, then
// checks two fresh whole seconds in every helper's sink. own is H7, other is
// H8, and standalone has no companion and no owner flag. talking says whether
// H7's microphone tone is expected in the other sinks.
func playbackWindow(what string, since time.Time, own, other, standalone *voiceClient, wantOwn, wantOther, wantStandalone map[string]int, talking bool) error {
	checks := []struct {
		c    *voiceClient
		want map[string]int
		send bool
	}{{own, wantOwn, talking}, {other, wantOther, false}, {standalone, wantStandalone, false}}
	for _, check := range checks {
		if err := playbackReady(check.c, what, since, check.want); err != nil {
			return err
		}
	}
	from := time.Now()
	var to time.Time
	for _, check := range checks {
		at, err := check.c.waitStats(2, from)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		to = at
	}
	for _, check := range checks {
		if err := playbackHeard(check.c, what, from, to, check.want, own, talking); err != nil {
			return err
		}
		if err := check.c.sending(what, from, to, check.send); err != nil {
			return err
		}
	}
	return nil
}

func pressPlaybackVoice(own, other, standalone *voiceClient, what string, wantOther, wantStandalone map[string]int) error {
	asked := time.Now()
	if err := own.say("down"); err != nil {
		return err
	}
	on, err := own.wait("transmitting after down", 15*time.Second, asked, transmittingIs(true))
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := playbackWindow(what, on, own, other, standalone, nil, wantOther, wantStandalone, true); err != nil {
		return err
	}
	asked = time.Now()
	if err := own.say("up"); err != nil {
		return err
	}
	if _, err := own.wait("not transmitting after up", 15*time.Second, asked, transmittingIs(false)); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func (l *live) companionPlayback(ctx context.Context) error {
	h, d := l.h, l.d
	if l.voiceBin == "" {
		h.say("SKIP companion playback owners: %s", l.voiceSkip)
		return nil
	}
	if _, err := d.createChannel(playbackChannel); err != nil {
		return err
	}
	h7, err := h.newPerson(d, "h7", playbackChannel)
	if err != nil {
		return err
	}
	h8, err := h.newPerson(d, "h8", playbackChannel)
	if err != nil {
		return err
	}
	standalonePerson, err := h.newPerson(d, "standalone", playbackChannel)
	if err != nil {
		return err
	}
	dir, err := h.mkdir("playback-")
	if err != nil {
		return err
	}
	shareAudio := filepath.Join(dir, "share-audio.ogg")
	if err := writePlaybackTone(shareAudio, 300); err != nil {
		return err
	}

	own, err := l.startPlaybackVoice(h7, fmt.Sprintf("tone:%d", toneAnn), toneAnn, "companion")
	if err != nil {
		return err
	}
	other, err := l.startPlaybackVoice(h8, "none", 0, "companion")
	if err != nil {
		return err
	}
	standalone, err := l.startPlaybackVoice(standalonePerson, "none", 0, "")
	if err != nil {
		return err
	}
	sess, err := h.session(h7, playbackChannel)
	if err != nil {
		return err
	}
	room := sess.Rooms[0].Room
	shareID := identity(h7.id) + ".screen"
	// conch-voice's stats use its display-safe speaker_label, not the raw
	// LiveKit identity: '.' becomes '?'.
	shareLabel := strings.ReplaceAll(shareID, ".", "?")
	c7, screenSID, audioSID, err := l.publishPlaybackShare(h7, room, shareAudio)
	if err != nil {
		return err
	}
	defer c7.Stop()

	// B2: C8 joins after C7's tracks exist, with autoSubscribe off. It gets
	// only the two screen tracks, never H7's microphone.
	c8resp, err := h.mustCompanion(h8, schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishScreen, Subscribe: true}, playbackChannel)
	if err != nil {
		return err
	}
	c8, err := h.startLK(l.lkBin, strings.TrimPrefix(l.srv.WSURL, "ws://"), c8resp.Token, nil)
	if err != nil {
		return err
	}
	defer c8.Stop()
	if err := l.lkWaitHas("H8's B2 companion", room, c8resp.Identity, c8); err != nil {
		return err
	}
	if got := c8.LogLines("track subscribed"); len(got) != 0 {
		return fmt.Errorf("the B2 companion auto-subscribed before its selective subscriptions: %s", h.redact(strings.Join(got, "\n")))
	}
	if err := l.subscribePlaybackTracks(room, c8resp.Identity, screenSID, audioSID); err != nil {
		return err
	}
	if err := waitCompanionTracks(c8, shareID); err != nil {
		return err
	}

	shareWant := map[string]int{shareLabel: toneShare}
	if err := playbackWindow("B2 with only C7 publishing", time.Now(), own, other, standalone, nil, nil, shareWant, false); err != nil {
		return err
	}
	voiceWant := map[string]int{identity(h7.id): toneAnn}
	voiceAndShare := map[string]int{identity(h7.id): toneAnn, shareLabel: toneShare}
	if err := pressPlaybackVoice(own, other, standalone, "B2 with H7's microphone and C7's tone", voiceWant, voiceAndShare); err != nil {
		return err
	}
	for _, line := range c8.LogLines("track subscribed") {
		if strings.Contains(line, `"source": "MICROPHONE",`) {
			return fmt.Errorf("the B2 companion received a microphone track: %s", h.redact(line))
		}
	}
	h.say("ok   companion playback B2: H8 heard H7's microphone once but never C7's tone; C8 received C7's screen and tone and no voice track; H7 never heard its own companion; the no-companion helper heard both tones")

	// A browser-style viewer for person 8 publishes nothing and receives both
	// C7 tracks. A second viewer displaces the first; folding keeps presence
	// byte-for-byte unchanged. Let H7's release reach presence first, so the
	// comparison is of the viewer displacement, not the preceding microphone.
	if err := waitFor("H7's microphone release to reach presence", 30*time.Second, func() (bool, string) {
		doc, err := h8.presence(playbackChannel)
		if err != nil {
			return false, err.Error()
		}
		for _, room := range doc.Rooms {
			for _, participant := range room.Participants {
				if participant.PrincipalID == h7.id {
					return !participant.Transmitting, fmt.Sprintf("transmitting=%v", participant.Transmitting)
				}
			}
		}
		return false, "H7 is not listed"
	}); err != nil {
		return err
	}
	beforePresence, err := h8.presence(playbackChannel)
	if err != nil {
		return err
	}
	viewerReq := schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishNone, Subscribe: true}
	view1resp, status, code, err := h.requestCompanion(h8, viewerReq, playbackChannel)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("H8 asks for its first viewer: status %d code %q: %w", status, code, err)
	}
	view1, err := h.startLK(l.lkBin, strings.TrimPrefix(l.srv.WSURL, "ws://"), view1resp.Token, nil, "--auto-subscribe")
	if err != nil {
		return err
	}
	defer view1.Stop()
	if err := l.lkWaitHas("H8's first viewer", room, view1resp.Identity, view1); err != nil {
		return err
	}
	if err := waitCompanionTracks(view1, shareID); err != nil {
		return err
	}
	ok, firstViewer, saw := l.lkHasIdentity(room, view1resp.Identity)
	if !ok {
		return fmt.Errorf("the first viewer left before displacement: %s", saw)
	}
	if len(firstViewer.Tracks) != 0 || firstViewer.Permission["can_publish"] != false {
		return fmt.Errorf("the first viewer published or may publish: tracks %v permission %v", firstViewer.Sources(), firstViewer.Permission)
	}
	view2resp, status, code, err := h.requestCompanion(h8, viewerReq, playbackChannel)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("H8 asks for its second viewer: status %d code %q: %w", status, code, err)
	}
	view2, err := h.startLK(l.lkBin, strings.TrimPrefix(l.srv.WSURL, "ws://"), view2resp.Token, nil, "--auto-subscribe")
	if err != nil {
		return err
	}
	defer view2.Stop()
	if err := waitForOrFail("the second viewer to displace the first", 30*time.Second, func() (bool, string, error) {
		ok, current, saw := l.lkHasIdentity(room, view2resp.Identity)
		return ok && current.SID != firstViewer.SID && view1.Exited(), saw, nil
	}); err != nil {
		return err
	}
	if err := waitCompanionTracks(view2, shareID); err != nil {
		return err
	}
	if ok, current, saw := l.lkHasIdentity(room, view2resp.Identity); !ok {
		return fmt.Errorf("the second viewer is not listed: %s", saw)
	} else if len(current.Tracks) != 0 {
		return fmt.Errorf("the second viewer published %v", current.Sources())
	}
	if err := waitFor("presence to be unchanged by the viewer displacement", 30*time.Second, func() (bool, string) {
		after, err := h8.presence(playbackChannel)
		return err == nil && reflect.DeepEqual(beforePresence, after), fmt.Sprintf("before %+v after %+v (error %v)", beforePresence, after, err)
	}); err != nil {
		return err
	}
	h.say("ok   companion viewer: p8.view received C7's screen and tone and published nothing; a second viewer displaced the first and presence was unchanged")

	// Start the B1 arrangement cleanly. The helpers now have the default
	// --share-audio-owner=helper; C8's grant forbids every subscription even
	// though lk asks to auto-subscribe. C7 remains the B2 share publisher.
	view1.Stop()
	view2.Stop()
	if err := h7.api.call(http.MethodDelete, "/v1/channels/"+playbackChannel+"/voice/share", nil, nil); err != nil {
		return fmt.Errorf("H7 stops the B2 playback share: %w", err)
	}
	c7.Stop()
	c8.Stop()
	if err := h7.removeCompanion("", playbackChannel); err != nil {
		return err
	}
	if err := h8.removeCompanion("", playbackChannel); err != nil {
		return err
	}
	if err := stopPlaybackVoice(ctx, other); err != nil {
		return err
	}
	if err := stopPlaybackVoice(ctx, own); err != nil {
		return err
	}
	own, err = l.startPlaybackVoice(h7, fmt.Sprintf("tone:%d", toneAnn), toneAnn, "")
	if err != nil {
		return err
	}
	other, err = l.startPlaybackVoice(h8, "none", 0, "")
	if err != nil {
		return err
	}
	c7, _, _, err = l.publishPlaybackShare(h7, room, shareAudio)
	if err != nil {
		return err
	}
	defer c7.Stop()
	b1resp, err := h.mustCompanion(h8, schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishScreen}, playbackChannel)
	if err != nil {
		return err
	}
	b1, err := h.startLK(l.lkBin, strings.TrimPrefix(l.srv.WSURL, "ws://"), b1resp.Token, nil, "--auto-subscribe")
	if err != nil {
		return err
	}
	defer b1.Stop()
	if err := l.lkWaitHas("H8's B1 companion", room, b1resp.Identity, b1); err != nil {
		return err
	}
	if ok, part, saw := l.lkHasIdentity(room, b1resp.Identity); !ok {
		return fmt.Errorf("the B1 companion is not listed: %s", saw)
	} else if part.Permission["can_subscribe"] != false {
		return fmt.Errorf("the B1 companion may subscribe: %v", part.Permission)
	}

	if err := playbackWindow("B1 with only C7 publishing", time.Now(), own, other, standalone, nil, shareWant, shareWant, false); err != nil {
		return err
	}
	if err := pressPlaybackVoice(own, other, standalone, "B1 with H7's microphone and C7's tone", voiceAndShare, voiceAndShare); err != nil {
		return err
	}
	if got := b1.LogLines("track subscribed"); len(got) != 0 {
		return fmt.Errorf("the B1 companion received %d tracks despite canSubscribe=false: %s", len(got), h.redact(strings.Join(got, "\n")))
	}
	h.say("ok   companion playback B1: C8 received nothing; H8 heard C7's tone and H7's microphone at one publisher's frame count and level; H7 never heard its own companion; the no-companion helper heard both tones")
	return nil
}

// shareOpusPacket is one 20 ms, 48 kHz Opus CELT packet of the 1760 Hz tone,
// encoded with libopus at 64 kbit/s. Repeating it is a steady tone after the
// decoder's first frame, and keeps the test independent of an Opus encoder or
// another binary. The packet is below Ogg's one-lacing-value limit.
const shareOpusPacket = "f88a7f8e8295106ce2999a9233bc37d451fb170a8a3fa787d08fd007450f5d8255bbf8fbb5fec1bd5f122af28aee70d72e0b8100e0ecfd6995142fcc4387f509601e8600231179303c93ce1b74c9bdd5b794f8cbbebe81372dccdb54a70b07c56b13c36648000000000000001635ccec5612a67c214d27831750e59750e596bc48b6b81410666768873e88873e9ffdf477aa35a42d1984318e82a074f72cffd6e56c668b90406c1122bfde1d8366bbdef161d2f23299f1ad144ede21a55c35674b4cb777519cbeb5395289f278c97ee1164f7565d0d06469885430847056d4275fbfb9a9596c"

const (
	playbackOggSerial    = 0x63377337
	playbackFrameSamples = 960
	playbackPreSkip      = 312
	playbackPacketsPage  = 50
)

var playbackCRCTable = func() [256]uint32 {
	var table [256]uint32
	for i := range table {
		r := uint32(i) << 24 // #nosec G115 -- i < 256
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		table[i] = r
	}
	return table
}()

func playbackCRC(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = crc<<8 ^ playbackCRCTable[byte(crc>>24)^b]
	}
	return crc
}

func playbackOggPage(header byte, granule int64, sequence uint32, packets [][]byte) []byte {
	var page bytes.Buffer
	page.WriteString("OggS")
	page.WriteByte(0)
	page.WriteByte(header)
	_ = binary.Write(&page, binary.LittleEndian, granule)
	_ = binary.Write(&page, binary.LittleEndian, uint32(playbackOggSerial))
	_ = binary.Write(&page, binary.LittleEndian, sequence)
	_ = binary.Write(&page, binary.LittleEndian, uint32(0))
	page.WriteByte(byte(len(packets))) // #nosec G115 -- at most playbackPacketsPage
	for _, packet := range packets {
		page.WriteByte(byte(len(packet))) // #nosec G115 -- shareOpusPacket is 230 bytes
	}
	for _, packet := range packets {
		page.Write(packet)
	}
	out := page.Bytes()
	binary.LittleEndian.PutUint32(out[22:26], playbackCRC(out))
	return out
}

func writePlaybackTone(path string, seconds int) error {
	packet, err := hex.DecodeString(shareOpusPacket)
	if err != nil {
		return err
	}
	if seconds <= 0 || len(packet) >= 255 {
		return errors.New("the playback tone needs a positive duration and a short Opus packet")
	}
	head := new(bytes.Buffer)
	head.WriteString("OpusHead")
	head.WriteByte(1)
	head.WriteByte(1)
	_ = binary.Write(head, binary.LittleEndian, uint16(playbackPreSkip))
	_ = binary.Write(head, binary.LittleEndian, uint32(48000))
	_ = binary.Write(head, binary.LittleEndian, uint16(0))
	head.WriteByte(0)
	tags := new(bytes.Buffer)
	tags.WriteString("OpusTags")
	vendor := "conch e2e"
	_ = binary.Write(tags, binary.LittleEndian, uint32(len(vendor))) // #nosec G115 -- constant, short
	tags.WriteString(vendor)
	_ = binary.Write(tags, binary.LittleEndian, uint32(0))

	var out bytes.Buffer
	out.Write(playbackOggPage(0x02, 0, 0, [][]byte{head.Bytes()}))
	out.Write(playbackOggPage(0x00, 0, 1, [][]byte{tags.Bytes()}))
	total := seconds * 1000 / 20
	sequence := uint32(2)
	for done := 0; done < total; {
		n := min(playbackPacketsPage, total-done)
		packets := make([][]byte, n)
		for i := range packets {
			packets[i] = packet
		}
		done += n
		header := byte(0)
		if done == total {
			header = 0x04
		}
		out.Write(playbackOggPage(header, int64(done)*playbackFrameSamples+playbackPreSkip, sequence, packets))
		sequence++
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
