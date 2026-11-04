package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The media half (needs Docker and the pinned LiveKit, as e2e/voice does).
// A's ann talks and shares her screen through a real lk participant, amy is
// connected too; then B tries to reach that room: by asking conchd (refused,
// in the matrix and here), by bending a token of B's own to name A's room
// (LiveKit refuses it: the grant names one room and is signed), and by
// watching everything LiveKit offers in B's own room (nothing of A's).

// voiceA is what A holds in voice.
type voiceA struct {
	room  string
	annLK *lkit.Headless
	amy   *lkit.SignalConn
}

// session asks for a voice session in channel as m and registers its tokens
// as secrets and, for A, its room as a marker.
func (w *world) session(m *member, channel string) (schema.VoiceSessionResponseV1, error) {
	var resp schema.VoiceSessionResponseV1
	if err := m.api.call(http.MethodPost, "/v1/channels/"+channel+"/voice/session", nil, &resp); err != nil {
		return resp, err
	}
	if len(resp.Rooms) != 1 {
		return resp, fmt.Errorf("%s's session in %s has %d rooms, want 1", m.name, channel, len(resp.Rooms))
	}
	for _, g := range resp.Rooms {
		w.h.secret("a voice join token", g.Token)
		w.h.hide("a voice room name", g.Room)
		if w.isA(m.name) {
			w.marks.secret("A's voice join token", g.Token)
			w.marks.secret("A's voice room", g.Room)
		}
	}
	return resp, nil
}

// lkHost is LiveKit's address as lk dials it.
func (lk *liveKit) hostPort() string { return strings.TrimPrefix(lk.srv.WSURL, "ws://") }

// has asks LiveKit whether identity p<id> is in room.
func (lk *liveKit) has(room string, id int64) (bool, lkit.Participant, string) {
	ctx, cancel := context.WithTimeout(stopCtx, 5*time.Second)
	defer cancel()
	parts, err := lk.adm.ListParticipants(ctx, room)
	if err != nil {
		return false, lkit.Participant{}, err.Error()
	}
	var ids []string
	for _, p := range parts {
		ids = append(ids, p.Identity)
		if p.Identity == identity(id) {
			return true, p, ""
		}
	}
	return false, lkit.Participant{}, fmt.Sprintf("LiveKit lists %v", ids)
}

// gone reports that LiveKit says p<id> is not in room, or that the room is
// gone. A call that failed otherwise says nothing.
func (lk *liveKit) gone(room string, id int64) (bool, string) {
	ctx, cancel := context.WithTimeout(stopCtx, 5*time.Second)
	defer cancel()
	parts, err := lk.adm.ListParticipants(ctx, room)
	if lkit.RoomNotFound(err) {
		return true, "the room does not exist"
	}
	if err != nil {
		return false, "LiveKit could not be asked: " + err.Error()
	}
	for _, p := range parts {
		if p.Identity == identity(id) {
			return false, "LiveKit lists them in the room"
		}
	}
	return true, "LiveKit does not list them"
}

// anywhere counts the rooms LiveKit lists p<id> in.
func (lk *liveKit) anywhere(id int64) (int, error) {
	ctx, cancel := context.WithTimeout(stopCtx, 10*time.Second)
	defer cancel()
	rooms, err := lk.adm.ListRooms(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, room := range rooms {
		parts, err := lk.adm.ListParticipants(ctx, room)
		if lkit.RoomNotFound(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, p := range parts {
			if p.Identity == identity(id) {
				n++
			}
		}
	}
	return n, nil
}

func identity(id int64) string { return fmt.Sprintf("p%d", id) }

// lkParticipantKeys are the keys under which lk names a participant by
// identity in its log, as lk v2.19.0 and the server-sdk-go it is built with
// write them: "participant" (participant connected, track subscribed, and
// every line of a remote participant's own logger). "participantID" and
// "localParticipant" hold the SID LiveKit assigns (PA_...), never the
// identity conchd chose, so they cannot name p<id>.
var lkParticipantKeys = []string{"participant"}

// namesParticipant reports whether an lk log line names participant p<id>:
// as the whole value of one of lkParticipantKeys. A bare p<id> is not enough,
// since a room name, a track id or a longer identity (p<id>0) can hold it.
func namesParticipant(line string, id int64) bool {
	for _, k := range lkParticipantKeys {
		if strings.Contains(line, `"`+k+`": "`+identity(id)+`"`) {
			return true
		}
	}
	return false
}

// linesNaming returns the lines of hl's log that name participant p<id> and
// contain every one of also. Every check of whom lk was told of goes through
// it, so no two members are matched in different ways.
func linesNaming(hl *lkit.Headless, id int64, also ...string) []string {
	var out []string
	for _, line := range hl.LogLines(also...) {
		if namesParticipant(line, id) {
			out = append(out, line)
		}
	}
	return out
}

// namesSID reports whether an lk log line names the participant LiveKit
// gave sid (PA_...): as a whole JSON string value, under whatever key. lk
// v2.19.0 writes a SID under "participantID" (participant connected; could
// not find participant, deferring track update) and "localParticipant";
// matching the quoted value rather than a list of keys also catches a key
// not seen yet. A SID is random, so unlike p<id> it is no room name by
// chance; but a longer value holding it (a SID that ends in it) is not it.
func namesSID(line, sid string) bool {
	return sid != "" && strings.Contains(line, `"`+sid+`"`)
}

// linesNamingSID returns the lines of hl's log that name the participant
// LiveKit gave sid.
func linesNamingSID(hl *lkit.Headless, sid string) []string {
	var out []string
	for _, line := range hl.LogLines() {
		if namesSID(line, sid) {
			out = append(out, line)
		}
	}
	return out
}

// voiceA puts A in voice: ann joins through lk, publishing a microphone,
// claims the share and publishes a screen and its sound on the same
// connection; amy joins with a bare signalling connection.
func (w *world) startVoiceA() (*voiceA, error) {
	lk := w.lk
	ann, amy := w.a.people[0], w.a.people[2]
	camp := w.a.main()
	sa, err := w.session(ann, camp)
	if err != nil {
		return nil, err
	}
	v := &voiceA{room: sa.Rooms[0].Room}
	dir, err := w.h.mkdir("share-")
	if err != nil {
		return nil, err
	}
	shareAudio := filepath.Join(dir, "share-audio.ogg")
	if err := lkit.WriteSilentOpus(shareAudio, 900); err != nil {
		return nil, err
	}
	// As e2e/voice's share scenario: lk publishes its share tracks as soon
	// as it joins, so the relay holds them back until the claim is granted.
	gate := lkit.NewShareFilter(true)
	if v.annLK, err = w.h.kit.StartLK(lk.lkBin, lk.hostPort(), sa.Rooms[0].Token, gate.Filter, "--publish-demo", "--publish", shareAudio, "--publish", lk.ogg); err != nil {
		return nil, err
	}
	if err := waitFor("LiveKit to list ann in A's room", 60*time.Second, func() (bool, string) {
		ok, _, saw := lk.has(v.room, ann.id)
		if !ok && v.annLK.Exited() {
			return false, "lk exited: " + v.annLK.Tail()
		}
		return ok, saw
	}); err != nil {
		return nil, err
	}
	select {
	case <-gate.Held():
	case <-time.After(30 * time.Second):
		return nil, errors.New("ann's lk did not try to publish its share tracks within 30 s")
	}
	var share schema.VoiceShareV1
	if err := ann.api.call(http.MethodPost, "/v1/channels/"+camp+"/voice/share", nil, &share); err != nil {
		return nil, err
	}
	gate.Release()
	if err := waitFor("LiveKit to list ann's screen and its sound", 30*time.Second, func() (bool, string) {
		ok, p, saw := lk.has(v.room, ann.id)
		if !ok {
			return false, saw
		}
		screen, audio := lkit.ShareTracks(p)
		return screen && audio, fmt.Sprintf("sources %v", p.Sources())
	}); err != nil {
		return nil, err
	}
	if err := ann.api.call(http.MethodPost, "/v1/channels/"+camp+"/voice/transmit", schema.VoiceTransmitReportV1{State: schema.VoiceTransmitStateStarted}, nil); err != nil {
		return nil, err
	}
	sm, err := w.session(amy, camp)
	if err != nil {
		return nil, err
	}
	if v.amy, err = w.joinSignal(sm.Rooms[0]); err != nil {
		return nil, fmt.Errorf("amy joins A's room: %w", err)
	}
	if err := waitFor("LiveKit to list amy in A's room", 30*time.Second, func() (bool, string) {
		ok, _, saw := lk.has(v.room, amy.id)
		return ok, saw
	}); err != nil {
		return nil, err
	}
	w.h.say("ok   A in voice: ann talks and shares a screen with its sound through lk, amy is connected (LiveKit lists both)")
	return v, nil
}

// joinSignal connects a bare signalling participant on a grant conchd issued.
func (w *world) joinSignal(g schema.VoiceRoomGrant) (*lkit.SignalConn, error) {
	ctx, cancel := context.WithTimeout(stopCtx, 15*time.Second)
	defer cancel()
	c, status, body, err := lkit.DialSignal(ctx, w.lk.srv.WSURL, g.Token)
	if err != nil {
		return nil, fmt.Errorf("HTTP %d %s", status, w.h.redact(body))
	}
	w.h.onCleanup(c.Close)
	return c, nil
}

// mediaB is what B tries against A's room.
func (w *world) mediaB(v *voiceA) error {
	lk := w.lk
	bea, bob := w.b.people[0], w.b.people[1]
	ann := w.a.people[0]

	// Asking conchd for A's room.
	status, body, err := bea.api.do(http.MethodPost, "/v1/channels/"+w.a.main()+"/voice/session", nil)
	if err != nil {
		return err
	}
	if err := checkRefusal(result{status: status, body: body}, channelNotFound, asked{strs: []string{w.a.main()}}); err != nil {
		return fmt.Errorf("bea asks for a voice session in A's channel: %w", err)
	}

	// A's camera, live while B looks (issue #1241).
	cam, err := w.startCameraA(v)
	defer cam.stop()
	if err != nil {
		return fmt.Errorf("group A's camera: %w", err)
	}

	// B's own grant names B's room, and only it.
	sb, err := w.session(bea, w.b.main())
	if err != nil {
		return err
	}
	g := sb.Rooms[0]
	if g.Room == v.room {
		return errors.New("bea's grant for B's channel names A's room")
	}
	claims, err := videoGrant(g.Token)
	if err != nil {
		return err
	}
	if claims["room"] != g.Room || claims["roomAdmin"] == true || claims["roomList"] == true || claims["roomCreate"] == true {
		return fmt.Errorf("bea's token grants more than joining B's room: grants %v", grantKeys(claims))
	}

	// The same token, its room changed to A's: LiveKit refuses it.
	bent, err := withRoom(g.Token, v.room)
	if err != nil {
		return err
	}
	w.h.secret("a bent voice token", bent)
	ctx, cancel := context.WithTimeout(stopCtx, 15*time.Second)
	c, status2, body2, err := lkit.DialSignal(ctx, lk.srv.WSURL, bent)
	cancel()
	if err == nil {
		c.Close()
		return errors.New("LiveKit admitted B's token with its room changed to A's")
	}
	if status2 != http.StatusUnauthorized {
		return fmt.Errorf("a B token bent to A's room: LiveKit answered HTTP %d %q, want 401", status2, w.h.redact(body2))
	}
	if gone, saw := lk.gone(v.room, bea.id); !gone {
		return fmt.Errorf("after the bent token, bea is in A's room: %s", saw)
	}

	// B in B's own room: bea talks, bob watches everything LiveKit offers
	// there. bob is subscribed to bea and to nothing of ann's.
	beaLK, err := w.h.kit.StartHeadless(lk.lkBin, lk.ogg, lk.hostPort(), g.Token)
	if err != nil {
		return err
	}
	defer beaLK.Stop()
	sv, err := w.session(bob, w.b.main())
	if err != nil {
		return err
	}
	viewer, err := w.h.kit.StartLK(lk.lkBin, lk.hostPort(), sv.Rooms[0].Token, nil, "--auto-subscribe")
	if err != nil {
		return err
	}
	defer viewer.Stop()
	if err := waitFor("bob to be subscribed to bea's microphone in B's room", 30*time.Second, func() (bool, string) {
		got := linesNaming(viewer, bea.id, "track subscribed")
		if viewer.Exited() {
			return false, "lk exited: " + viewer.Tail()
		}
		return len(got) > 0, fmt.Sprintf("%d subscriptions to bea", len(got))
	}); err != nil {
		return err
	}
	// ann's share is live while bob looks.
	if ok, p, saw := lk.has(v.room, ann.id); !ok {
		return fmt.Errorf("ann left A's room while B looked: %s", saw)
	} else if screen, audio := lkit.ShareTracks(p); !screen || !audio {
		return fmt.Errorf("ann's share ended while B looked: sources %v", p.Sources())
	}
	// cam's camera is live while bob looks, too.
	if ok, p, saw := lk.has(v.room, cam.cam.id); !ok {
		return fmt.Errorf("cam left A's room while B looked: %s", saw)
	} else if !cameraTrack(p) {
		return fmt.Errorf("cam's camera ended while B looked: sources %v", p.Sources())
	}
	for _, who := range []struct {
		name string
		id   int64
	}{{"ann", ann.id}, {"amy", w.a.people[2].id}, {"cam", cam.cam.id}} {
		if got := linesNaming(viewer, who.id); len(got) > 0 {
			return fmt.Errorf("bob's lk in B's room was told of %s: %s", who.name, w.h.redact(got[0]))
		}
		// LiveKit also names a participant by the SID it gave them, and a
		// line may carry that alone. The SID is read from A's room now: it
		// is the connection bob could have been told of.
		ok, p, saw := lk.has(v.room, who.id)
		if !ok {
			return fmt.Errorf("%s left A's room while B looked: %s", who.name, saw)
		}
		if !strings.HasPrefix(p.SID, "PA_") {
			return fmt.Errorf("LiveKit lists %s in A's room without a participant SID (%q), so bob's lk log cannot be checked for it", who.name, p.SID)
		}
		if got := linesNamingSID(viewer, p.SID); len(got) > 0 {
			return fmt.Errorf("bob's lk in B's room was told of %s by SID: %s", who.name, w.h.redact(got[0]))
		}
	}
	for _, hl := range []*lkit.Headless{beaLK, viewer} {
		for _, t := range hl.Relay.IssuedTokens() {
			w.h.secret("a LiveKit-issued token", t)
			if c, err := videoGrant(t); err == nil && c["room"] == v.room {
				return errors.New("LiveKit issued a B participant a token for A's room")
			}
		}
	}
	for _, id := range []int64{bea.id, bob.id} {
		if gone, saw := lk.gone(v.room, id); !gone {
			return fmt.Errorf("a B member is in A's room: %s", saw)
		}
	}
	w.h.say("ok   media: bea's grant names B's room alone; the same token bent to A's room is refused by LiveKit (401); bob, subscribed to everything in B's room, hears bea and is told nothing of ann's screen, sound or microphone, of cam's camera, or of amy (by identity or by LiveKit SID)")
	return w.cameraRowsA(v, cam)
}

// videoGrant decodes a LiveKit token's video grant (without checking its
// signature; LiveKit does that).
func videoGrant(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("a voice token is not a three-part JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("a voice token's payload is not base64url")
	}
	var c struct {
		Video map[string]any `json:"video"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New("a voice token's payload is not JSON")
	}
	return c.Video, nil
}

// grantKeys names a grant's keys, for a message that must not print the room.
func grantKeys(grant map[string]any) []string {
	var out []string
	for k, v := range grant {
		if k == "room" {
			continue
		}
		out = append(out, fmt.Sprintf("%s=%v", k, v))
	}
	return out
}

// withRoom is token with the room of its video grant changed to room and its
// signature left as it was: what a B member who learnt A's room name could
// make without the API secret.
func withRoom(token, room string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("a voice token is not a three-part JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	video, ok := payload["video"].(map[string]any)
	if !ok {
		return "", errors.New("a voice token has no video grant")
	}
	video["room"] = room
	out, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(out) + "." + parts[2], nil
}

// ---------------------------------------------------------------------------
// The camera (issue #1241, ADR-015; docs/design/webcam-video.md §11.3)
//
// With the camera turned on for both columns by the owner, cam, a member of
// A's channel, first tries to publish a camera on the join grant alone (the
// "modified client": LiveKit refuses the source), then claims one and
// publishes it, and it is live while B looks from B's own room (mediaB: B is
// told nothing of it). Then the two mismatched kinds (S1): a video track
// declared as a microphone under the join grant, and a sound declared as a
// camera under a claim; each is either refused by LiveKit or found by a pass,
// which removes its publisher within the bound and audits it naming the
// declared source and type. Last, the owner turns the camera off for both
// columns, through the real PUT, and cam's camera is withdrawn before the PUT
// answers, which also puts the world back as the later phases expect it.

// cameraA is what A holds of the camera.
type cameraA struct {
	cam   *member
	camLK *lkit.Headless
}

// camera source and type numbers, as LiveKit's room API may write them.
const (
	lkSourceCamera           = 1
	lkSourceMicrophone       = 2
	lkSourceScreenShare      = 3
	lkSourceScreenShareAudio = 4
)

// trackOf reports whether t is declared as source (by name or number) and is
// of kind typ ("AUDIO" or "VIDEO"; an absent type is AUDIO, protojson's
// default).
func trackOf(t lkit.Track, source string, sourceNum int, typ string) bool {
	src := fmt.Sprint(t.Source)
	if src != source && src != itoa(int64(sourceNum)) {
		return false
	}
	kind := fmt.Sprint(t.Type)
	switch typ {
	case "AUDIO":
		return t.Type == nil || kind == "AUDIO" || kind == "0"
	case "VIDEO":
		return kind == "VIDEO" || kind == "1"
	}
	return false
}

// cameraTrack is whether LiveKit lists a video track declared as the camera.
func cameraTrack(p lkit.Participant) bool {
	for _, t := range p.Tracks {
		if trackOf(t, "CAMERA", lkSourceCamera, "VIDEO") {
			return true
		}
	}
	return false
}

// sourceGate is a relay filter that rewrites the source of lk's track called
// name to source (LiveKit's number) and, gated, holds that add_track request
// back until Release: how lk joins, waits while conchd grants the camera,
// and then publishes on the same connection, as a browser does. It is the
// share's filter (lkit.ShareFilter) for any source.
type sourceGate struct {
	match  func(name string) bool
	source uint64
	gate   chan struct{} // nil: never wait
	once   sync.Once
	held   chan struct{}
}

func newSourceGate(gated bool, source uint64, match func(name string) bool) *sourceGate {
	g := &sourceGate{match: match, source: source, held: make(chan struct{})}
	if gated {
		g.gate = make(chan struct{})
	}
	return g
}

// Release lets a held track through.
func (g *sourceGate) Release() {
	if g.gate != nil {
		close(g.gate)
	}
}

// Held is closed once the first matching track has reached the filter.
func (g *sourceGate) Held() <-chan struct{} { return g.held }

// Filter is the relay filter (Kit.StartLK).
func (g *sourceGate) Filter(ctx context.Context, frame []byte) []byte {
	out, rewrote := lkit.RewriteAddTrackSource(frame, func(name string) (uint64, bool) {
		if g.match(name) {
			return g.source, true
		}
		return 0, false
	})
	if !rewrote {
		return frame
	}
	g.once.Do(func() { close(g.held) })
	if g.gate != nil {
		select {
		case <-g.gate:
		case <-ctx.Done():
		}
	}
	return out
}

// claimCamera claims a camera in channel as m, and requires the claim's
// answer to be well formed and m's.
func claimCamera(m *member, channel string) error {
	var c schema.VoiceCameraV1
	if err := m.api.call(http.MethodPost, "/v1/channels/"+channel+"/voice/camera", nil, &c); err != nil {
		return err
	}
	if c.PrincipalID != m.id {
		return fmt.Errorf("the camera claim names principal %d, want %d", c.PrincipalID, m.id)
	}
	return c.Validate()
}

// startCameraA turns the camera on for both columns, has cam refused a
// camera on the join grant, then claim one and publish it.
func (w *world) startCameraA(v *voiceA) (*cameraA, error) {
	lk := w.lk
	camp := w.a.main()
	if err := w.setCameraCells(true); err != nil {
		return nil, err
	}
	cam, err := w.newPerson(w.aPerson("cam"), camp)
	if err != nil {
		return nil, err
	}
	// The modified client: a camera published on the join grant, with no
	// claim. lk publishes its demo video as a camera; LiveKit refuses the
	// source (the grant names the microphone alone), and lk gives up when
	// its publish is never answered.
	s, err := w.session(cam, camp)
	if err != nil {
		return nil, err
	}
	rogue, err := w.h.kit.StartLK(lk.lkBin, lk.hostPort(), s.Rooms[0].Token, nil, "--publish-demo")
	if err != nil {
		return nil, err
	}
	if err := waitFor("cam's camera without a claim to be refused", 45*time.Second, func() (bool, string) {
		if ok, p, _ := lk.has(v.room, cam.id); ok && cameraTrack(p) {
			return false, "LiveKit lists cam's camera"
		}
		return rogue.Exited(), "lk is still trying to publish"
	}); err != nil {
		rogue.Stop()
		return nil, err
	}
	if ok, p, _ := lk.has(v.room, cam.id); ok && cameraTrack(p) {
		rogue.Stop()
		return nil, fmt.Errorf("LiveKit published cam's camera on the join grant: sources %v", p.Sources())
	}
	rogue.Stop()
	if err := waitFor("cam's refused participant to leave A's room", 30*time.Second, func() (bool, string) {
		return lk.gone(v.room, cam.id)
	}); err != nil {
		return nil, err
	}

	// The claim, and the camera published on the same connection once it is
	// granted.
	s, err = w.session(cam, camp)
	if err != nil {
		return nil, err
	}
	gate := newSourceGate(true, lkSourceCamera, func(name string) bool { return name == "demo" })
	camLK, err := w.h.kit.StartLK(lk.lkBin, lk.hostPort(), s.Rooms[0].Token, gate.Filter, "--publish-demo")
	if err != nil {
		return nil, err
	}
	c := &cameraA{cam: cam, camLK: camLK}
	if err := waitFor("LiveKit to list cam in A's room", 60*time.Second, func() (bool, string) {
		ok, _, saw := lk.has(v.room, cam.id)
		if !ok && camLK.Exited() {
			return false, "lk exited: " + camLK.Tail()
		}
		return ok, saw
	}); err != nil {
		return c, err
	}
	select {
	case <-gate.Held():
	case <-time.After(30 * time.Second):
		return c, errors.New("cam's lk did not try to publish its camera within 30 s")
	}
	if err := claimCamera(cam, camp); err != nil {
		return c, fmt.Errorf("cam claims a camera: %w", err)
	}
	gate.Release()
	if err := waitFor("LiveKit to list cam's camera", 30*time.Second, func() (bool, string) {
		ok, p, saw := lk.has(v.room, cam.id)
		if !ok {
			return false, saw
		}
		return cameraTrack(p), fmt.Sprintf("sources %v", p.Sources())
	}); err != nil {
		return c, err
	}
	w.h.say("ok   camera: cam's camera on the join grant alone was refused by LiveKit; claimed, it is published in A's room (LiveKit lists a video track declared camera)")
	return c, nil
}

// stop ends cam's participant.
func (c *cameraA) stop() {
	if c != nil && c.camLK != nil {
		c.camLK.Stop()
	}
}

// cameraRowsA runs the mismatched kinds and the cell's cut against A's live
// camera.
func (w *world) cameraRowsA(v *voiceA, c *cameraA) error {
	lk := w.lk
	camp := w.a.main()
	findings := []string{}

	// One mismatched kind each: whoever publishes it, and how.
	dir, err := w.h.mkdir("camera-")
	if err != nil {
		return err
	}
	camAudio := filepath.Join(dir, "camera-audio.ogg")
	if err := lkit.WriteSilentOpus(camAudio, 900); err != nil {
		return err
	}
	// The screen share's sources by kind (the review of #1258): the share is
	// held in A's second channel, whose room has no share yet (ann holds the
	// first room's for the revocation phase), by the member who publishes the
	// mismatched track. lk publishes audio files and video, never data: data
	// under the share's sound is the server's unit tests' (TestVoiceCameraKindAndSource).
	shareAudio := filepath.Join(dir, "share-mismatch.ogg")
	if err := lkit.WriteSilentOpus(shareAudio, 900); err != nil {
		return err
	}
	den := w.a.channels[1]
	isDemo := func(n string) bool { return n == "demo" }
	isShareAudio := func(n string) bool { return strings.HasSuffix(n, "share-mismatch.ogg") }
	for _, k := range []struct {
		what, who   string
		claim       bool
		source      uint64
		match       func(string) bool
		options     []string
		trackSource string
		srcNum      int
		trackType   string
		// share says the member claims the screen share first, in den.
		share bool
	}{
		{"a video track declared as the microphone, under the join grant", "vid", false, lkSourceMicrophone,
			isDemo, []string{"--publish-demo"}, "microphone", lkSourceMicrophone, "video", false},
		{"a sound declared as the camera, under a claim", "snd", true, lkSourceCamera,
			func(n string) bool { return strings.HasSuffix(n, "camera-audio.ogg") }, []string{"--publish", camAudio}, "camera", lkSourceCamera, "audio", false},
		{"a sound declared as the screen share, under the join grant", "ssa", false, lkSourceScreenShare,
			isShareAudio, []string{"--publish", shareAudio}, "screen_share", lkSourceScreenShare, "audio", false},
		{"a sound declared as the screen share, under the share", "ssb", false, lkSourceScreenShare,
			isShareAudio, []string{"--publish", shareAudio}, "screen_share", lkSourceScreenShare, "audio", true},
		{"a video track declared as the share's sound, under the share", "ssc", false, lkSourceScreenShareAudio,
			isDemo, []string{"--publish-demo"}, "screen_share_audio", lkSourceScreenShareAudio, "video", true},
	} {
		channel := camp
		if k.share {
			channel = den
		}
		m, err := w.newPerson(w.aPerson(k.who), channel)
		if err != nil {
			return err
		}
		s, err := w.session(m, channel)
		if err != nil {
			return err
		}
		room := s.Rooms[0].Room
		gate := newSourceGate(k.claim || k.share, k.source, k.match)
		hl, err := w.h.kit.StartLK(lk.lkBin, lk.hostPort(), s.Rooms[0].Token, gate.Filter, k.options...)
		if err != nil {
			return err
		}
		if err := waitFor("LiveKit to list "+k.who+" in A's room", 60*time.Second, func() (bool, string) {
			ok, _, saw := lk.has(room, m.id)
			if !ok && hl.Exited() {
				return false, "lk exited: " + hl.Tail()
			}
			return ok, saw
		}); err != nil {
			hl.Stop()
			return err
		}
		if k.claim {
			select {
			case <-gate.Held():
			case <-time.After(30 * time.Second):
				hl.Stop()
				return fmt.Errorf("%s's lk did not try to publish within 30 s", k.who)
			}
			if err := claimCamera(m, camp); err != nil {
				hl.Stop()
				return fmt.Errorf("%s claims a camera: %w", k.who, err)
			}
			gate.Release()
		}
		if k.share {
			select {
			case <-gate.Held():
			case <-time.After(30 * time.Second):
				hl.Stop()
				return fmt.Errorf("%s's lk did not try to publish within 30 s", k.who)
			}
			// The row before this one removed its publisher holding den's
			// share, which ends (reason=left) on the next pass that finds them
			// gone: until then the claim is busy, and is asked again.
			if err := waitFor(k.who+"'s share claim in den", 15*time.Second, func() (bool, string) {
				status, body, err := m.api.do(http.MethodPost, "/v1/channels/"+den+"/voice/share", nil)
				switch {
				case err != nil:
					return false, err.Error()
				case status == http.StatusOK:
					return true, ""
				case status == http.StatusConflict && errorCode(body) == schema.ErrorCodeVoiceShareBusy:
					return false, "den's share is still the last row's"
				}
				return false, fmt.Sprintf("status %d %s", status, body)
			}); err != nil {
				hl.Stop()
				return err
			}
			gate.Release()
		}
		// Either LiveKit refuses the track (lk gives up and the track is never
		// listed), or it lists it, and then a pass must remove the publisher
		// within the bound: a pass (1.5 s) and a removal (4 s).
		// unclaimedRows counts the pass's rows for m naming the declared
		// source and type: the proof the track was accepted, even when the
		// pass removed m before this loop saw LiveKit list it.
		want := fmt.Sprintf("track_source=%s track_type=%s", k.trackSource, k.trackType)
		unclaimedRows := func() (int, error) {
			rows, err := w.auditRows(store.AuditVoiceCameraUnclaimed)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, r := range rows {
				if r.Actor == "principal:"+itoa(m.id) && strings.Contains(r.Detail, want) && strings.Contains(r.Detail, "source=observed") {
					n++
				}
			}
			return n, nil
		}
		listedAt := time.Time{}
		audited := false
		err = waitFor(k.what+" to be refused by LiveKit or removed by a pass", 45*time.Second, func() (bool, string) {
			ok, p, saw := lk.has(room, m.id)
			if ok {
				for _, t := range p.Tracks {
					if trackOf(t, strings.ToUpper(k.trackSource), k.srcNum, strings.ToUpper(k.trackType)) && listedAt.IsZero() {
						listedAt = time.Now()
					}
				}
			}
			if n, err := unclaimedRows(); err == nil && n > 0 {
				audited = true
			}
			if !listedAt.IsZero() || audited {
				return !ok, "LiveKit lists the mismatched track: " + saw
			}
			return hl.Exited(), "lk is still trying to publish"
		})
		tail := hl.Tail()
		hl.Stop()
		if err != nil {
			return err
		}
		if listedAt.IsZero() && !audited {
			if !strings.Contains(tail, "timed out publishing track") {
				return fmt.Errorf("%s: lk exited with no row and without the refusal: %s", k.what, tail)
			}
			findings = append(findings, "K8: LiveKit refused "+k.what)
			continue
		}
		// The 6 s bound is measured from the first look that saw the track
		// listed; a pass that removed the publisher between two looks left
		// nothing to measure it from.
		measured := !listedAt.IsZero()
		took := time.Duration(0)
		if measured {
			took = time.Since(listedAt)
			if took > 6*time.Second {
				return fmt.Errorf("%s was listed for %s before its publisher was removed, past the bound (a pass and a removal, 5.5 s)", k.what, took.Round(time.Millisecond))
			}
		}
		found, err := unclaimedRows()
		if err != nil {
			return err
		}
		if found != 1 {
			return fmt.Errorf("%s: %d voice_camera_unclaimed rows naming %s, want 1", k.what, found, want)
		}
		// The removal is audited once LiveKit has answered it, a moment after
		// the participant is gone: the row is waited for.
		if err := waitFor(k.what+": the removal of its publisher to be audited", 15*time.Second, func() (bool, string) {
			removed, err := w.auditRows(store.AuditVoiceParticipantRemoved)
			if err != nil {
				return false, err.Error()
			}
			return slices.ContainsFunc(removed, func(r schema.AuditEventV1) bool {
				return strings.Contains(r.Detail, "principal="+itoa(m.id)+" ") && strings.Contains(r.Detail, "reason=camera_unclaimed")
			}), fmt.Sprintf("no voice_participant_removed row with reason=camera_unclaimed for its publisher among %d", len(removed))
		}); err != nil {
			return err
		}
		if measured {
			findings = append(findings, fmt.Sprintf("K8: LiveKit accepted %s; the pass removed its publisher %s after LiveKit listed it, and audited it once", k.what, took.Round(time.Millisecond)))
		} else {
			findings = append(findings, fmt.Sprintf("K8: LiveKit accepted %s (the pass's row says so); the pass removed its publisher before this run saw the track listed, so the 6 s bound was not measured; audited once", k.what))
		}
	}
	for _, f := range findings {
		w.h.say("     finding: %s", f)
	}

	// The owner turns the camera off for both columns, through the real PUT:
	// cam's claim ends (access_lost) and its camera is withdrawn before the
	// PUT answers.
	start := time.Now()
	if err := w.setCameraCells(false); err != nil {
		return err
	}
	answered := time.Since(start)
	if ok, p, saw := lk.has(v.room, c.cam.id); !ok {
		return fmt.Errorf("cam left A's room when the camera was turned off: %s", saw)
	} else if cameraTrack(p) {
		// LiveKit unpublishes a withdrawn source at once (measured for a
		// share, K3): poll briefly, within the bound of one pass.
		if err := waitFor("cam's camera to be withdrawn", 2*time.Second, func() (bool, string) {
			_, p, _ := lk.has(v.room, c.cam.id)
			return !cameraTrack(p), fmt.Sprintf("sources %v", p.Sources())
		}); err != nil {
			return fmt.Errorf("the camera's cell off: %w", err)
		}
	}
	stopped, err := w.auditRows(store.AuditVoiceCameraStopped)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(stopped, func(r schema.AuditEventV1) bool {
		return strings.Contains(r.Detail, "principal="+itoa(c.cam.id)+" ") && strings.Contains(r.Detail, "reason=access_lost")
	}) {
		return errors.New("the camera's cell off wrote no voice_camera_stopped row with reason=access_lost for cam")
	}
	w.h.say("ok   camera: with the cell off for both columns through PUT /v1/permissions, cam's camera was withdrawn by the PUT's hook (it answered in %s) and its claim ended access_lost", answered.Round(time.Millisecond))
	return nil
}
