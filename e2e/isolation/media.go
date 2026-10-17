package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
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
		if strings.HasPrefix(m.name, "alpha-") {
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
	beaIdentity := `"participant": "` + identity(bea.id) + `"`
	if err := waitFor("bob to be subscribed to bea's microphone in B's room", 30*time.Second, func() (bool, string) {
		got := viewer.LogLines("track subscribed", beaIdentity)
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
	annIdentity := `"participant": "` + identity(ann.id) + `"`
	if got := viewer.LogLines(annIdentity); len(got) > 0 {
		return fmt.Errorf("bob's lk in B's room was told of ann: %s", w.h.redact(got[0]))
	}
	if got := viewer.LogLines(identity(w.a.people[2].id) + `"`); len(got) > 0 {
		return fmt.Errorf("bob's lk in B's room was told of amy: %s", w.h.redact(got[0]))
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
	w.h.say("ok   media: bea's grant names B's room alone; the same token bent to A's room is refused by LiveKit (401); bob, subscribed to everything in B's room, hears bea and is told nothing of ann's screen, sound or microphone, or of amy")
	return nil
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
