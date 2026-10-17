package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// ---- the screen share (issue #443, design note §12)
//
// A share is published by a client that has claimed it from conchd, which then
// has LiveKit let that one connection publish the screen-share sources. lk
// publishes files and a demo video, always as a microphone and a camera, and
// always as soon as it has joined. The relay in front of it (relay.filter)
// makes up for both:
//
//   - it rewrites the source of named tracks in lk's add_track requests: the
//     demo video becomes a screen share, and a file called share-audio.ogg
//     its sound. To LiveKit that is a screen share like a browser's: the
//     source is what the client says it is, and the permission check is on
//     the source.
//   - it can hold back the first of those requests until the harness lets it
//     go, which is how lk joins, waits while conchd grants the share, and
//     then publishes it on the same connection, as a browser does.
//
// LiveKit itself is asked what it did throughout: the permission it holds for
// the sharer, the tracks it lists, and what it told a viewer.

// LiveKit's TrackSource numbers, as they travel in an add_track request.
const (
	sourceScreenShare      = 3
	sourceScreenShareAudio = 4
)

// shareTrackSource says which source lk's track called name is to have: the
// demo video is the screen, share-audio.ogg its sound, anything else what lk
// gave it.
func shareTrackSource(name string) (uint64, bool) {
	switch {
	case name == "demo":
		return sourceScreenShare, true
	case strings.HasSuffix(name, "share-audio.ogg"):
		return sourceScreenShareAudio, true
	}
	return 0, false
}

// shareFilter is a relay filter that rewrites lk's share tracks to the
// screen-share sources. With a gate, the first share track waits until the
// gate is closed (or the connection ends); held reports, once, that one is
// waiting.
type shareFilter struct {
	gate chan struct{} // nil: never wait
	once sync.Once
	held chan struct{}
}

func newShareFilter(gated bool) *shareFilter {
	f := &shareFilter{held: make(chan struct{})}
	if gated {
		f.gate = make(chan struct{})
	}
	return f
}

func (f *shareFilter) release() {
	if f.gate != nil {
		close(f.gate)
	}
}

func (f *shareFilter) filter(ctx context.Context, frame []byte) []byte {
	out, rewrote := rewriteAddTrackSource(frame, shareTrackSource)
	if !rewrote {
		return frame
	}
	f.once.Do(func() { close(f.held) })
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
		}
	}
	return out
}

// rewriteAddTrackSource rewrites the source of an add_track signal request
// (SignalRequest field 4, AddTrackRequest; its name is field 2 and its source
// field 8, livekit_rtc.proto) when source(name) says to. Any other frame, or
// one it cannot read, is returned unchanged. It walks the protobuf wire
// format itself: no protobuf library is a dependency of this module.
func rewriteAddTrackSource(msg []byte, source func(name string) (uint64, bool)) ([]byte, bool) {
	for i := 0; i < len(msg); {
		key, n := uvarint(msg[i:])
		if n == 0 {
			return msg, false
		}
		field, wire := key>>3, key&7
		start := i
		i += n
		if field != 4 || wire != 2 {
			size := fieldSize(msg[i:], wire)
			if size < 0 {
				return msg, false
			}
			i += size
			continue
		}
		inner, size, ok := lengthPrefixed(msg[i:])
		if !ok {
			return msg, false
		}
		end := i + size
		name, kept := "", []byte(nil)
		for j := 0; j < len(inner); {
			k2, n2 := uvarint(inner[j:])
			if n2 == 0 {
				ok = false
				break
			}
			f2, w2 := k2>>3, k2&7
			size := fieldSize(inner[j+n2:], w2)
			if size < 0 {
				ok = false
				break
			}
			if f2 == 2 && w2 == 2 {
				value, _, _ := lengthPrefixed(inner[j+n2:])
				name = string(value)
			}
			if f2 != 8 {
				kept = append(kept, inner[j:j+n2+size]...)
			}
			j += n2 + size
		}
		to, rewrite := source(name)
		if !ok || !rewrite {
			return msg, false
		}
		kept = append(append(kept, 8<<3), putUvarint(to)...)
		out := append([]byte(nil), msg[:start]...)
		out = append(out, putUvarint(4<<3|2)...)
		out = append(out, putUvarint(uint64(len(kept)))...)
		out = append(out, kept...)
		return append(out, msg[end:]...), true
	}
	return msg, false
}

// uvarint reads a protobuf varint; n is 0 if there is none.
func uvarint(b []byte) (v uint64, n int) {
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

func putUvarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// lengthPrefixed reads a length-delimited value at the start of b: the value,
// and how many bytes it takes with its length. ok is false when it does not
// fit in b.
func lengthPrefixed(b []byte) (value []byte, size int, ok bool) {
	l, n := uvarint(b)
	if n == 0 || l > uint64(len(b)-n) { // #nosec G115 -- n <= len(b): uvarint read n bytes of b
		return nil, 0, false
	}
	size = n + int(l) // #nosec G115 -- l <= len(b)-n, checked above
	return b[n:size], size, true
}

// fieldSize is the length of a field's value of wire type wire at the start
// of b, or -1 if it cannot be read.
func fieldSize(b []byte, wire uint64) int {
	switch wire {
	case 0:
		if _, n := uvarint(b); n > 0 {
			return n
		}
	case 1:
		if len(b) >= 8 {
			return 8
		}
	case 2:
		if _, size, ok := lengthPrefixed(b); ok {
			return size
		}
	case 5:
		if len(b) >= 4 {
			return 4
		}
	}
	return -1
}

// shareTracks reports which screen-share sources LiveKit lists for p.
func shareTracks(p lkParticipant) (screen, audio bool) {
	for _, s := range p.sources() {
		switch s {
		case "SCREEN_SHARE", "3":
			screen = true
		case "SCREEN_SHARE_AUDIO", "4":
			audio = true
		}
	}
	return screen, audio
}

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
	if err := writeSilentOpus(shareAudio, 900); err != nil {
		return err
	}
	hostPort := strings.TrimPrefix(l.srv.wsURL, "ws://")

	// Before any claim, val tries to publish a screen and una its sound, each
	// on the join grant: LiveKit refuses both sources, and lk gives up when
	// its publish is never answered (10 s).
	type attempt struct {
		who  *person
		what string
		args []string
		lk   *headless
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
		if a.lk, err = h.startLK(l.lkBin, hostPort, s.Rooms[0].Token, newShareFilter(false).filter, a.args...); err != nil {
			return err
		}
	}
	for _, a := range attempts {
		if err := waitForOrFail(a.who.name+"'s unclaimed publish of "+a.what+" to be refused", 45*time.Second, func() (bool, string, error) {
			if ok, p, _ := l.lkHas(room, a.who.id); ok {
				if screen, audio := shareTracks(p); screen || audio {
					return false, "", fmt.Errorf("LiveKit published %s's share track without a claim: sources %v", a.who.name, p.sources())
				}
			}
			return a.lk.exited(), "lk is still trying to publish", nil
		}); err != nil {
			return err
		}
		if !strings.Contains(a.lk.tail(h), "timed out publishing track") {
			return fmt.Errorf("%s's lk exited without the refusal: %s", a.who.name, a.lk.tail(h))
		}
		a.lk.stop()
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
	gate := newShareFilter(true)
	samLK, err := h.startLK(l.lkBin, hostPort, ss.Rooms[0].Token, gate.filter, "--publish-demo", "--publish", shareAudio, "--publish", l.ogg)
	if err != nil {
		return err
	}
	defer samLK.stop()
	var samLive lkParticipant
	if err := waitForOrFail("LiveKit to list sam in screen's room", 60*time.Second, func() (bool, string, error) {
		ok, p, saw := l.lkHas(room, sam.id)
		if !ok && samLK.exited() {
			return false, "", errors.New("lk exited: " + samLK.tail(h))
		}
		samLive = p
		return ok, saw, nil
	}); err != nil {
		return err
	}
	select {
	case <-gate.held:
	case <-time.After(30 * time.Second):
		return errors.New("lk did not try to publish its share tracks within 30 s")
	}
	if err := checkSharePermission(samLive.Permission, false); err != nil {
		return fmt.Errorf("before the claim, LiveKit's permission for sam: %w", err)
	}
	h.say("ok   share set-up: sam joined screen's room on conchd's token; LiveKit holds the join grant exactly (microphone only)")

	// sam claims the share.
	before := len(samLK.relay.issuedTokens())
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
		toks := samLK.relay.issuedTokens()
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
	gate.release()
	if err := waitForOrFail("LiveKit to list sam's screen and its sound", 30*time.Second, func() (bool, string, error) {
		ok, p, saw := l.lkHas(room, sam.id)
		if !ok {
			if samLK.exited() {
				return false, "", errors.New("lk exited: " + samLK.tail(h))
			}
			return false, saw, nil
		}
		screen, audio := shareTracks(p)
		return screen && audio, fmt.Sprintf("sources %v", p.sources()), nil
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
	defer viewer.stop()
	samIdentity := `"participant": "` + identity(sam.id) + `"`
	if err := waitFor("val to be subscribed to sam's screen and its sound", 30*time.Second, func() (bool, string) {
		got := lkLogLines(viewer, "track subscribed", samIdentity)
		return containsAll(got, `"source": "SCREEN_SHARE",`, `"source": "SCREEN_SHARE_AUDIO",`), fmt.Sprintf("%d subscriptions to sam's tracks", len(got))
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
	before = len(samLK.relay.issuedTokens())
	if err := sam.api.call(http.MethodDelete, "/v1/channels/screen/voice/share", nil, nil); err != nil {
		return fmt.Errorf("sam stops the share: %w", err)
	}
	if ok, p, saw := l.lkHas(room, sam.id); !ok {
		return fmt.Errorf("sam left the room when the share stopped: %s", saw)
	} else if screen, audio := shareTracks(p); screen || audio {
		return fmt.Errorf("LiveKit still lists sam's share tracks after the stop answered: sources %v", p.sources())
	} else if err := checkSharePermission(p.Permission, false); err != nil {
		return fmt.Errorf("after the stop, LiveKit's permission for sam: %w", err)
	}
	if err := waitFor("val to be told sam's share tracks are gone", 15*time.Second, func() (bool, string) {
		got := lkLogLines(viewer, "track unpublished", samIdentity)
		return containsAll(got, `"source": "SCREEN_SHARE",`, `"source": "SCREEN_SHARE_AUDIO",`), fmt.Sprintf("%d of sam's tracks unpublished", len(got))
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
		toks := samLK.relay.issuedTokens()
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

// lkLogLines returns the lines of hl's lk output that contain every one of
// the strings.
func lkLogLines(hl *headless, contains ...string) []string {
	data, _ := os.ReadFile(hl.logPath) // #nosec G304 -- a path under this program's temp dir
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if containsAll([]string{line}, contains...) {
			out = append(out, line)
		}
	}
	return out
}

// containsAll reports whether each string is in at least one of lines.
func containsAll(lines []string, each ...string) bool {
	for _, s := range each {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, s) }) {
			return false
		}
	}
	return true
}
