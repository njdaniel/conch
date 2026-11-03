package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The camera (ADR-015, issue #1241; docs/design/webcam-video.md §4, §11.4,
// §12). A member who holds use_camera claims a camera from conchd, which has
// LiveKit let that one connection publish the camera source as well; the pass
// withdraws any camera nobody claimed and judges every track by its kind and
// its declared source together. This scenario runs it against the pinned
// LiveKit with lk participants, which publish a synthetic demo video as a
// camera, not a real camera, and records K1 to K8 of §12 as findings on every
// run.
//
// It runs on a conchd of its own, so that it can restart it (restart drops the
// claims) without disturbing the other scenarios. Two conchds on one LiveKit
// would sweep each other's rooms away as orphans, so this one names its rooms
// with a prefix of its own.

// LiveKit's TrackSource numbers, as they travel in an add_track request.
const (
	lkSourceCamera           = 1
	lkSourceMicrophone       = 2
	lkSourceScreenShareAudio = 4
)

// trackFilter is a relay filter that rewrites the source lk's tracks are
// declared under, by name (source), and with a gate holds back the first it
// rewrites until Release: lk publishes as soon as it has joined, and a camera
// is published only once it is claimed, as a browser does.
type trackFilter struct {
	source func(name string) (uint64, bool)
	gate   chan struct{} // nil: never wait
	once   sync.Once
	held   chan struct{}
}

func newTrackFilter(gated bool, source func(name string) (uint64, bool)) *trackFilter {
	f := &trackFilter{source: source, held: make(chan struct{})}
	if gated {
		f.gate = make(chan struct{})
	}
	return f
}

func (f *trackFilter) Release() {
	if f.gate != nil {
		close(f.gate)
	}
}

func (f *trackFilter) Held() <-chan struct{} { return f.held }

func (f *trackFilter) Filter(ctx context.Context, frame []byte) []byte {
	out, rewrote := lkit.RewriteAddTrackSource(frame, f.source)
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

// sources maps lk's track names to the source each is declared under: "demo"
// is lk's demo video, anything else a file whose name ends so.
func sources(by map[string]uint64) func(string) (uint64, bool) {
	return func(name string) (uint64, bool) {
		for suffix, src := range by {
			if name == suffix || (suffix != "demo" && strings.HasSuffix(name, suffix)) {
				return src, true
			}
		}
		return 0, false
	}
}

// lkTrack is p's published track under source, as LiveKit lists it (by
// name or number).
func lkTrack(p lkit.Participant, source string, num int) (lkit.Track, bool) {
	for _, t := range p.Tracks {
		if s := fmt.Sprint(t.Source); s == source || s == fmt.Sprint(num) {
			return t, true
		}
	}
	return lkit.Track{}, false
}

// claimCamera claims a camera in channel as p, naming a claim with query
// (schema.VoiceCameraClaimParam=…) or not.
func claimCamera(p *person, channel, query string) (schema.VoiceCameraV1, int, string, error) {
	var c schema.VoiceCameraV1
	path := "/v1/channels/" + channel + "/voice/camera"
	if query != "" {
		path += "?" + query
	}
	status, data, err := p.api.do(http.MethodPost, path, nil)
	if err != nil || status != http.StatusOK {
		var e schema.Error
		_ = decodeStrict(data, &e)
		return c, status, e.Code, err
	}
	if err := decodeStrict(data, &c); err != nil {
		return c, status, "", fmt.Errorf("decode the camera: %w", err)
	}
	if err := c.Validate(); err != nil {
		return c, status, "", fmt.Errorf("the camera does not satisfy the schema: %w", err)
	}
	if c.PrincipalID != p.id {
		return c, status, "", fmt.Errorf("%s's camera names principal %d", p.name, c.PrincipalID)
	}
	return c, status, "", nil
}

func mustCamera(p *person, channel, query string) (schema.VoiceCameraV1, error) {
	c, status, code, err := claimCamera(p, channel, query)
	if err != nil {
		return c, fmt.Errorf("%s claims a camera: %w", p.name, err)
	}
	if status != http.StatusOK {
		return c, fmt.Errorf("%s claims a camera: status %d code %q", p.name, status, code)
	}
	return c, nil
}

// cameraQuery names claim c.
func cameraQuery(c schema.VoiceCameraV1) string {
	return schema.VoiceCameraClaimParam + "=" + c.StartedAt.Time().Format(time.RFC3339Nano)
}

// stopCamera sends DELETE …/voice/camera as a, with query, and requires 204.
func stopCamera(a api, channel, query string) error {
	path := "/v1/channels/" + channel + "/voice/camera"
	if query != "" {
		path += "?" + query
	}
	status, data, err := a.do(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		var e schema.Error
		_ = decodeStrict(data, &e)
		return fmt.Errorf("DELETE camera: status %d code %q, want 204", status, e.Code)
	}
	return nil
}

// camerasIn lists who presence, as viewer reads it, shows with a camera in
// the scenario's channel, and the viewer's own camera_ended.
func camerasIn(viewer *person) ([]int64, *schema.VoiceCameraEnded, error) {
	doc, err := viewer.presence("cams")
	if err != nil {
		return nil, nil, err
	}
	if err := doc.Validate(); err != nil {
		return nil, nil, fmt.Errorf("presence does not validate: %w", err)
	}
	var out []int64
	for _, r := range doc.Rooms {
		for _, p := range r.Participants {
			if p.Camera {
				out = append(out, p.PrincipalID)
			}
		}
	}
	return out, doc.CameraEnded, nil
}

// enableCamera turns use_camera on for operators and members, as the owner,
// in one PUT: precedence (member ⇒ operator) wants the operator cell on with
// the member cell.
func enableCamera(d *conchdProc) error {
	var doc schema.PermissionsResponseV1
	if err := d.operator.call(http.MethodGet, "/v1/permissions", nil, &doc); err != nil {
		return err
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermUseCamera, Role: schema.PermissionRoleOperator, Granted: true},
		{Permission: schema.PermUseCamera, Role: schema.PermissionRoleMember, Granted: true},
	}}
	return d.operator.call(http.MethodPut, "/v1/permissions", req, nil)
}

// camera is the scenario.
func (l *live) camera(ctx context.Context) error {
	h := l.h
	d, err := h.startConchdEnv("camera", settingsOf(l.srv), livekit.EnvRoomPrefix+"=conchcam-")
	if err != nil {
		return err
	}
	defer d.stop()
	channelID, err := d.createChannel("cams")
	if err != nil {
		return err
	}
	if _, err := d.createChannel("capcams"); err != nil {
		return err
	}
	subject := fmt.Sprintf("channel:%d", channelID)
	people := map[string]*person{}
	for _, name := range []string{"cal", "dee", "fay", "gil", "hal", "ivy", "jo"} {
		p, err := h.newPersonWith(d, name, personOpts{noCLI: true}, "cams")
		if err != nil {
			return err
		}
		people[name] = p
	}
	cal, dee := people["cal"], people["dee"]
	dir, err := h.mkdir("camera-")
	if err != nil {
		return err
	}
	shareAudio := filepath.Join(dir, "share-audio.ogg")
	cameraAudio := filepath.Join(dir, "camera-audio.ogg")
	for _, f := range []string{shareAudio, cameraAudio} {
		if err := lkit.WriteSilentOpus(f, 900); err != nil {
			return err
		}
	}
	hostPort := strings.TrimPrefix(l.srv.WSURL, "ws://")
	// join joins as p through a filter declaring lk's tracks as by says,
	// gated or not, with options, and waits for LiveKit to list it.
	join := func(p *person, gated bool, by map[string]uint64, options ...string) (*lkit.Headless, *trackFilter, string, error) {
		s, err := h.session(p, "cams")
		if err != nil {
			return nil, nil, "", err
		}
		room := s.Rooms[0].Room
		f := newTrackFilter(gated, sources(by))
		hl, err := h.startLK(l.lkBin, hostPort, s.Rooms[0].Token, f.Filter, options...)
		if err != nil {
			return nil, nil, "", err
		}
		err = waitForOrFail("LiveKit to list "+p.name, 60*time.Second, func() (bool, string, error) {
			ok, _, saw := l.lkHas(room, p.id)
			if !ok && hl.Exited() {
				return false, "", errors.New("lk exited: " + hl.Tail())
			}
			return ok, saw, nil
		})
		return hl, f, room, err
	}

	// Off by default (ADR-015 decision 1): a member's claim is refused until
	// the owner turns the camera on for members.
	if _, err := h.session(cal, "cams"); err != nil {
		return err
	}
	if _, status, code, err := claimCamera(cal, "cams", ""); err != nil {
		return err
	} else if status != http.StatusForbidden || code != schema.ErrorCodePermissionDenied {
		return fmt.Errorf("a member's claim with use_camera off: status %d code %q, want 403 %s", status, code, schema.ErrorCodePermissionDenied)
	}
	if err := enableCamera(d); err != nil {
		return fmt.Errorf("turn use_camera on: %w", err)
	}
	h.say("ok   camera: off by default (a member's claim is 403 permission_denied); the owner turned it on for operators and members in one PUT")

	// K1: the join grant refuses a camera. dee publishes lk's demo video,
	// which lk declares a camera, with no claim.
	k1, _, room, err := join(dee, false, nil, "--publish-demo")
	if err != nil {
		return err
	}
	if err := waitForOrFail("dee's unclaimed camera to be refused", 45*time.Second, func() (bool, string, error) {
		if ok, p, _ := l.lkHas(room, dee.id); ok {
			if _, cam := lkTrack(p, "CAMERA", lkSourceCamera); cam {
				return false, "", fmt.Errorf("LiveKit published dee's camera without a claim: sources %v", p.Sources())
			}
		}
		return k1.Exited(), "lk is still trying to publish", nil
	}); err != nil {
		return err
	}
	if !strings.Contains(k1.Tail(), "timed out publishing track") {
		return fmt.Errorf("dee's lk exited without the refusal: %s", k1.Tail())
	}
	k1.Stop()
	h.say("finding K1: the join grant [\"microphone\"] refuses a camera: LiveKit never listed dee's demo video, and lk gave up (timed out publishing track)")

	// cal joins with the demo video as a camera and silence as a microphone;
	// the relay holds the camera back until it is claimed.
	calLK, calGate, _, err := join(cal, true, map[string]uint64{"demo": lkSourceCamera}, "--publish-demo", "--publish", l.ogg)
	if err != nil {
		return err
	}
	defer calLK.Stop()
	select {
	case <-calGate.Held():
	case <-time.After(30 * time.Second):
		return errors.New("lk did not try to publish its camera within 30 s")
	}
	if ok, p, saw := l.lkHas(room, cal.id); !ok {
		return fmt.Errorf("cal is not in the room: %s", saw)
	} else if err := checkPublishPermission(p.Permission, false, false); err != nil {
		return fmt.Errorf("before the claim, LiveKit's permission for cal: %w", err)
	}

	// The claim (K2, K4).
	before := len(calLK.Relay.IssuedTokens())
	claim, err := mustCamera(cal, "cams", "")
	if err != nil {
		return err
	}
	if ok, p, saw := l.lkHas(room, cal.id); !ok {
		return fmt.Errorf("cal left the room during the claim: %s", saw)
	} else if err := checkPublishPermission(p.Permission, false, true); err != nil {
		return fmt.Errorf("after the claim, LiveKit's permission for cal: %w", err)
	}
	var during string
	if err := waitFor("LiveKit to send cal a token carrying the camera", 10*time.Second, func() (bool, string) {
		toks := calLK.Relay.IssuedTokens()
		if len(toks) > before {
			during = toks[len(toks)-1]
			return true, ""
		}
		return false, fmt.Sprintf("%d tokens sent, none since the claim", len(toks))
	}); err != nil {
		return err
	}
	h.secret("a LiveKit-issued token", during)
	if err := h.checkLiveKitTokenGrant(during, d.lk, identity(cal.id), room, false, true); err != nil {
		return fmt.Errorf("finding K4: the token LiveKit sent cal after the claim: %w", err)
	}
	h.say("finding K4: LiveKit's refreshed token carries the updated sources: [microphone camera] after the claim")
	calGate.Release()
	var camTrack lkit.Track
	if err := waitForOrFail("LiveKit to list cal's camera", 30*time.Second, func() (bool, string, error) {
		ok, p, saw := l.lkHas(room, cal.id)
		if !ok {
			if calLK.Exited() {
				return false, "", errors.New("lk exited: " + calLK.Tail())
			}
			return false, saw, nil
		}
		t, cam := lkTrack(p, "CAMERA", lkSourceCamera)
		camTrack = t
		return cam, fmt.Sprintf("sources %v", p.Sources()), nil
	}); err != nil {
		return err
	}
	h.say("finding K2: UpdateParticipant adding CAMERA to a live connection let it publish: cal's camera was listed once the claim answered")
	if typ := fmt.Sprint(camTrack.Type); typ != "VIDEO" && typ != "1" {
		return fmt.Errorf("finding K6: LiveKit lists cal's camera with type %v, want VIDEO", camTrack.Type)
	}
	h.say("finding K6: ListParticipants lists the camera as source=%v type=%v muted=%v", camTrack.Source, camTrack.Type, camTrack.Muted)
	if err := waitFor("dee's presence to show cal's camera", 30*time.Second, func() (bool, string) {
		ids, _, err := camerasIn(dee)
		if err != nil {
			return false, err.Error()
		}
		return slices.Equal(ids, []int64{cal.id}), fmt.Sprintf("cameras %v", ids)
	}); err != nil {
		return err
	}

	// dee receives it.
	sd, err := h.session(dee, "cams")
	if err != nil {
		return err
	}
	viewer, err := h.startLK(l.lkBin, hostPort, sd.Rooms[0].Token, nil, "--auto-subscribe")
	if err != nil {
		return err
	}
	defer viewer.Stop()
	calIdentity := `"participant": "` + identity(cal.id) + `"`
	if err := waitFor("dee to be subscribed to cal's camera", 30*time.Second, func() (bool, string) {
		got := viewer.LogLines("track subscribed", calIdentity)
		return lkit.ContainsAll(got, `"source": "CAMERA",`), fmt.Sprintf("%d subscriptions to cal's tracks", len(got))
	}); err != nil {
		return err
	}
	h.say("ok   camera claimed: cal's claim set exactly [MICROPHONE CAMERA] in LiveKit and its fresh token; cal published a camera; presence shows it; dee received it")

	// cal stops it (K3): gone from LiveKit before the stop answers; the
	// microphone stays.
	before = len(calLK.Relay.IssuedTokens())
	if err := stopCamera(cal.api, "cams", cameraQuery(claim)); err != nil {
		return fmt.Errorf("cal stops the camera: %w", err)
	}
	if ok, p, saw := l.lkHas(room, cal.id); !ok {
		return fmt.Errorf("cal left the room when the camera stopped: %s", saw)
	} else if _, cam := lkTrack(p, "CAMERA", lkSourceCamera); cam {
		return fmt.Errorf("finding K3: LiveKit still lists cal's camera after the stop answered: sources %v", p.Sources())
	} else if _, mic := p.Mic(); !mic {
		return fmt.Errorf("finding K3: withdrawing the camera took cal's microphone: sources %v", p.Sources())
	} else if err := checkPublishPermission(p.Permission, false, false); err != nil {
		return fmt.Errorf("after the stop, LiveKit's permission for cal: %w", err)
	}
	if err := waitFor("dee to be told cal's camera is gone", 15*time.Second, func() (bool, string) {
		got := viewer.LogLines("track unpublished", calIdentity)
		return lkit.ContainsAll(got, `"source": "CAMERA",`), fmt.Sprintf("%d of cal's tracks unpublished", len(got))
	}); err != nil {
		return err
	}
	if err := waitFor("presence to take cal's camera away, with camera_ended for cal alone", 15*time.Second, func() (bool, string) {
		ids, own, err := camerasIn(cal)
		if err != nil {
			return false, err.Error()
		}
		_, other, err := camerasIn(dee)
		if err != nil {
			return false, err.Error()
		}
		ok := len(ids) == 0 && own != nil && own.Reason == schema.VoiceCameraEndSelf && other == nil
		return ok, fmt.Sprintf("cameras %v, cal's camera_ended %+v, dee's %+v", ids, own, other)
	}); err != nil {
		return err
	}
	var after string
	if err := waitFor("LiveKit to send cal a token without the camera", 10*time.Second, func() (bool, string) {
		toks := calLK.Relay.IssuedTokens()
		if len(toks) > before {
			after = toks[len(toks)-1]
			return true, ""
		}
		return false, "no token since the stop"
	}); err != nil {
		return err
	}
	h.secret("a LiveKit-issued token", after)
	if err := h.checkLiveKitTokenGrant(after, d.lk, identity(cal.id), room, false, false); err != nil {
		return fmt.Errorf("the token LiveKit sent cal after the stop: %w", err)
	}
	// A name of the claim that ended is stale (S2).
	if _, status, code, err := claimCamera(cal, "cams", cameraQuery(claim)); err != nil {
		return err
	} else if status != http.StatusConflict || code != schema.ErrorCodeVoiceCameraStale {
		return fmt.Errorf("a named claim after the stop: status %d code %q, want 409 %s", status, code, schema.ErrorCodeVoiceCameraStale)
	}
	h.say("finding K3: withdrawing CAMERA unpublished cal's camera before the stop answered and left the microphone; dee was told; the next token was the join grant")
	h.say("ok   camera stopped: presence took it away, camera_ended (self) in cal's document alone; a resume naming it is 409 voice_camera_stale")

	// The operator stops cal's camera.
	if _, err := mustCamera(cal, "cams", ""); err != nil {
		return err
	}
	if err := stopCamera(d.operator, "cams", fmt.Sprintf("%s=%d", schema.VoiceCameraPrincipalParam, cal.id)); err != nil {
		return fmt.Errorf("the operator stops cal's camera: %w", err)
	}
	if ok, p, _ := l.lkHas(room, cal.id); ok {
		if err := checkPublishPermission(p.Permission, false, false); err != nil {
			return fmt.Errorf("after the operator's stop, LiveKit's permission for cal: %w", err)
		}
	}
	if _, own, err := camerasIn(cal); err != nil {
		return err
	} else if own == nil || own.Reason != schema.VoiceCameraEndOperator {
		return fmt.Errorf("cal's camera_ended after the operator's stop: %+v, want operator", own)
	}
	h.say("ok   operator stop: the owner's DELETE ?principal= ended cal's camera (camera_ended operator) and withdrew it")

	// A camera and a share together (K7, S4): fay stops her camera and keeps
	// her share; gil stops his share and keeps his camera. The share's track
	// is its sound (lk publishes one video, the demo, which is the camera).
	together := map[string]uint64{"demo": lkSourceCamera, "share-audio.ogg": lkSourceScreenShareAudio}
	for i, name := range []string{"fay", "gil"} {
		p := people[name]
		hl, gate, _, err := join(p, true, together, "--publish-demo", "--publish", shareAudio, "--publish", l.ogg)
		if err != nil {
			return err
		}
		select {
		case <-gate.Held():
		case <-time.After(30 * time.Second):
			return fmt.Errorf("%s's lk did not try to publish within 30 s", name)
		}
		c, err := mustCamera(p, "cams", "")
		if err != nil {
			return err
		}
		if _, status, code, err := claimShare(p, "cams"); err != nil {
			return fmt.Errorf("%s claims the share: %w", name, err)
		} else if status != http.StatusOK {
			return fmt.Errorf("%s claims the share: status %d code %q", name, status, code)
		}
		if ok, lp, saw := l.lkHas(room, p.id); !ok {
			return fmt.Errorf("%s left the room: %s", name, saw)
		} else if err := checkPublishPermission(lp.Permission, true, true); err != nil {
			return fmt.Errorf("finding K7: the combined permission for %s: %w", name, err)
		}
		gate.Release()
		if err := waitForOrFail(name+"'s camera and share to be listed", 30*time.Second, func() (bool, string, error) {
			ok, lp, saw := l.lkHas(room, p.id)
			if !ok {
				if hl.Exited() {
					return false, "", errors.New("lk exited: " + hl.Tail())
				}
				return false, saw, nil
			}
			_, cam := lkTrack(lp, "CAMERA", lkSourceCamera)
			_, audio := lkit.ShareTracks(lp)
			return cam && audio, fmt.Sprintf("sources %v", lp.Sources()), nil
		}); err != nil {
			return err
		}
		if i == 0 {
			if err := stopCamera(p.api, "cams", cameraQuery(c)); err != nil {
				return err
			}
		} else if err := p.api.call(http.MethodDelete, "/v1/channels/cams/voice/share", nil, nil); err != nil {
			return err
		}
		ok, lp, saw := l.lkHas(room, p.id)
		if !ok {
			return fmt.Errorf("%s left the room: %s", name, saw)
		}
		_, cam := lkTrack(lp, "CAMERA", lkSourceCamera)
		_, audio := lkit.ShareTracks(lp)
		if i == 0 && (cam || !audio) || i == 1 && (!cam || audio) {
			return fmt.Errorf("finding K7: after %s's %s stop, sources %v", name, map[int]string{0: "camera", 1: "share"}[i], lp.Sources())
		}
		if err := checkPublishPermission(lp.Permission, i == 0, i == 1); err != nil {
			return fmt.Errorf("after %s's stop, LiveKit's permission: %w", name, err)
		}
		// The other one goes too.
		if i == 0 {
			if err := p.api.call(http.MethodDelete, "/v1/channels/cams/voice/share", nil, nil); err != nil {
				return err
			}
		} else if err := stopCamera(p.api, "cams", cameraQuery(c)); err != nil {
			return err
		}
		hl.Stop()
	}
	h.say("finding K7: the combined permission [MICROPHONE SCREEN_SHARE SCREEN_SHARE_AUDIO CAMERA] is accepted, and each source is enforced alone: fay's camera stop left her share's track, gil's share stop left his camera")

	// K5: a token LiveKit sent during a claim still grants the camera after the
	// claim ended. cal rejoins on it, publishing a camera with no claim; the
	// pass withdraws it and audits it, and removes nobody.
	calLK.Stop()
	k5, err := h.startLK(l.lkBin, hostPort, during, nil, "--publish-demo")
	if err != nil {
		return err
	}
	defer k5.Stop()
	if err := l.waitAuditOn(d, "the pass to audit cal's camera on the refreshed token", store.AuditVoiceCameraUnclaimed, subject, fmt.Sprintf("principal:%d", cal.id)); err != nil {
		return fmt.Errorf("finding K5: %w", err)
	}
	if err := waitFor("the pass to withdraw cal's camera on the refreshed token", 15*time.Second, func() (bool, string) {
		ok, p, saw := l.lkHas(room, cal.id)
		if !ok {
			return false, saw
		}
		_, cam := lkTrack(p, "CAMERA", lkSourceCamera)
		return !cam && checkPublishPermission(p.Permission, false, false) == nil, fmt.Sprintf("sources %v permission %v", p.Sources(), p.Permission["can_publish_sources"])
	}); err != nil {
		return err
	}
	if n, err := l.auditCountOn(d, store.AuditVoiceParticipantRemoved, subject, ""); err != nil {
		return err
	} else if n != 0 {
		return fmt.Errorf("finding K5: %d voice_participant_removed rows: an honest client on a refreshed token is withdrawn from, not removed", n)
	}
	k5.Stop()
	h.say("finding K5: a token LiveKit sent during the claim still granted the camera after it ended; the pass withdrew it and wrote voice_camera_unclaimed, and removed nobody")

	// K8: whether LiveKit accepts a track whose kind disagrees with its
	// declared source (S1). hal declares lk's demo video a microphone, under
	// the join grant; ivy declares a sound file a camera, under a claim.
	k8a, k8aAccepted, err := l.mismatch(d, people["hal"], room, join, false, map[string]uint64{"demo": lkSourceMicrophone}, "MICROPHONE", lkSourceMicrophone, "track_source=microphone track_type=video", "--publish-demo")
	if err != nil {
		return fmt.Errorf("finding K8 (video as microphone): %w", err)
	}
	k8b, _, err := l.mismatch(d, people["ivy"], room, join, true, map[string]uint64{"camera-audio.ogg": lkSourceCamera}, "CAMERA", lkSourceCamera, "track_source=camera track_type=audio", "--publish", cameraAudio)
	if err != nil {
		return fmt.Errorf("finding K8 (audio as camera): %w", err)
	}
	h.say("finding K8: a VIDEO track declared microphone under the join grant: %s", k8a)
	h.say("finding K8: an AUDIO track declared camera under a camera claim: %s", k8b)
	if k8aAccepted {
		// Whoever else was in the room may have received it before the pass
		// removed its publisher: lk subscribes to everything, which the
		// browser does not (its wanted filter checks kind and source, C6a).
		got := viewer.LogLines("track subscribed", `"participant": "`+identity(people["hal"].id)+`"`)
		h.say("finding K8: dee's lk (auto-subscribe) subscribed to hal's video declared microphone before the removal: %v (%d log lines)", len(got) > 0, len(got))
	}

	// Restart drops the claims. jo claims and publishes; conchd restarts; its
	// first pass finds the camera with no claim, withdraws it and audits it,
	// and removes nobody.
	jo := people["jo"]
	joLK, joGate, _, err := join(jo, true, map[string]uint64{"demo": lkSourceCamera}, "--publish-demo", "--publish", l.ogg)
	if err != nil {
		return err
	}
	defer joLK.Stop()
	select {
	case <-joGate.Held():
	case <-time.After(30 * time.Second):
		return errors.New("jo's lk did not try to publish within 30 s")
	}
	joClaim, err := mustCamera(jo, "cams", "")
	if err != nil {
		return err
	}
	joGate.Release()
	if err := waitFor("LiveKit to list jo's camera", 30*time.Second, func() (bool, string) {
		ok, p, saw := l.lkHas(room, jo.id)
		if !ok {
			return false, saw
		}
		_, cam := lkTrack(p, "CAMERA", lkSourceCamera)
		return cam, fmt.Sprintf("sources %v", p.Sources())
	}); err != nil {
		return err
	}
	if err := d.restart(); err != nil {
		return fmt.Errorf("restart conchd: %w", err)
	}
	if err := l.waitAuditOn(d, "the first pass after the restart to audit jo's camera", store.AuditVoiceCameraUnclaimed, subject, fmt.Sprintf("principal:%d", jo.id)); err != nil {
		return err
	}
	if err := waitFor("the first pass after the restart to withdraw jo's camera", 15*time.Second, func() (bool, string) {
		ok, p, saw := l.lkHas(room, jo.id)
		if !ok {
			return false, saw
		}
		_, cam := lkTrack(p, "CAMERA", lkSourceCamera)
		return !cam, fmt.Sprintf("sources %v", p.Sources())
	}); err != nil {
		return err
	}
	if ok, _, saw := l.lkHas(room, jo.id); !ok {
		return fmt.Errorf("jo was removed after the restart: %s", saw)
	}
	if ids, own, err := camerasIn(jo); err != nil {
		return err
	} else if len(ids) != 0 || own != nil {
		return fmt.Errorf("after the restart presence shows cameras %v and camera_ended %+v, want none", ids, own)
	}
	if _, status, code, err := claimCamera(jo, "cams", cameraQuery(joClaim)); err != nil {
		return err
	} else if status != http.StatusConflict || code != schema.ErrorCodeVoiceCameraStale {
		return fmt.Errorf("jo's resume after the restart: status %d code %q, want 409 %s", status, code, schema.ErrorCodeVoiceCameraStale)
	}
	joLK.Stop()
	h.say("ok   restart: the claim was dropped; the first pass withdrew jo's camera and wrote voice_camera_unclaimed without removing jo; a resume naming the old claim is stale")

	// The cap: nine claims in one room, and the tenth refused, naming nobody;
	// a holder's repeat is not refused by it. Signal-only participants: the
	// cap counts claims, and a claim needs only a connection.
	if err := l.cameraCap(ctx, d); err != nil {
		return fmt.Errorf("the cap: %w", err)
	}

	// The audit log: per person, each start and its one stop, with its
	// reason; the unclaimed rows the pass wrote; nothing for a refusal.
	rows, err := cameraRowsOf(d, subject, people)
	if err != nil {
		return err
	}
	want := map[string][]string{
		"cal": {"started", "stopped principal=cal reason=self", "started", "operator stopped principal=cal reason=operator", "unclaimed source=observed track_source=camera track_type=video"},
		"fay": {"started", "stopped principal=fay reason=self"},
		"gil": {"started", "stopped principal=gil reason=self"},
		"jo":  {"started", "unclaimed source=observed track_source=camera track_type=video"},
		"dee": nil,
	}
	for who, w := range want {
		if got := rows[who]; !slices.Equal(got, w) {
			return fmt.Errorf("%s's camera rows %q, want %q", who, got, w)
		}
	}
	h.say("ok   audited: one voice_camera_started and one voice_camera_stopped per claim (self, operator), voice_camera_unclaimed for the refreshed token and after the restart (the restart's claim has no stop row: it was dropped), nothing for a refusal")
	if err := d.scanAudit("camera"); err != nil {
		return err
	}
	return nil
}

// mismatch is one row of K8: p joins (claiming a camera first when claim),
// publishing a track declared as by says. It reports what LiveKit did, and
// whether it accepted the track. If it did, the pass must remove p within the
// bound and write voice_camera_unclaimed naming the declared source and type.
func (l *live) mismatch(d *conchdProc, p *person, room string, join func(*person, bool, map[string]uint64, ...string) (*lkit.Headless, *trackFilter, string, error),
	claim bool, by map[string]uint64, source string, num int, detail string, options ...string) (string, bool, error) {
	hl, gate, _, err := join(p, claim, by, options...)
	if err != nil {
		return "", false, err
	}
	defer hl.Stop()
	if claim {
		select {
		case <-gate.Held():
		case <-time.After(30 * time.Second):
			return "", false, errors.New("lk did not try to publish within 30 s")
		}
		if _, err := mustCamera(p, "cams", ""); err != nil {
			return "", false, err
		}
		gate.Release()
	}
	actor := fmt.Sprintf("principal:%d", p.id)
	subject := ""
	accepted := false
	err = waitForOrFail("LiveKit to accept or refuse "+p.name+"'s track", 45*time.Second, func() (bool, string, error) {
		if n, err := l.auditCountOn(d, store.AuditVoiceCameraUnclaimed, subject, actor); err == nil && n > 0 {
			accepted = true
			return true, "", nil
		}
		if ok, lp, _ := l.lkHas(room, p.id); ok {
			if _, there := lkTrack(lp, source, num); there {
				accepted = true
				return true, "", nil
			}
		}
		return hl.Exited(), "lk is still trying to publish", nil
	})
	if err != nil {
		return "", false, err
	}
	if !accepted {
		if !strings.Contains(hl.Tail(), "timed out publishing track") {
			return "", false, fmt.Errorf("lk exited without the refusal: %s", hl.Tail())
		}
		if claim {
			if err := stopCamera(p.api, "cams", ""); err != nil {
				return "", false, err
			}
		}
		return "refused by LiveKit (lk timed out publishing track); the pass's rule is belt and braces", false, nil
	}
	// Accepted: the pass writes the row and removes the publisher within
	// the bound (a pass, then the removal: a few seconds).
	if err := l.waitAuditOn(d, "the pass to audit "+p.name+"'s mismatched track", store.AuditVoiceCameraUnclaimed, "", actor); err != nil {
		return "", false, err
	}
	rows, err := d.auditRows(store.AuditVoiceCameraUnclaimed)
	if err != nil {
		return "", false, err
	}
	for _, e := range rows {
		if e.Actor == actor && !strings.Contains(e.Detail, detail) {
			return "", false, fmt.Errorf("the unclaimed row says %q, want %q", e.Detail, detail)
		}
	}
	start := time.Now()
	if err := waitFor(p.name+" to be removed", 15*time.Second, func() (bool, string) {
		return l.lkGone(room, p.id)
	}); err != nil {
		return "", false, err
	}
	removed, err := d.auditRows(store.AuditVoiceParticipantRemoved)
	if err != nil {
		return "", false, err
	}
	found := false
	for _, e := range removed {
		if strings.Contains(e.Detail, fmt.Sprintf("principal=%d ", p.id)) && strings.Contains(e.Detail, "reason=camera_unclaimed") {
			found = true
		}
	}
	if !found {
		return "", false, fmt.Errorf("no voice_participant_removed reason=camera_unclaimed for %s", p.name)
	}
	if claim {
		// The claim ends with its holder gone.
		if err := l.waitAuditOn(d, p.name+"'s claim to end", store.AuditVoiceCameraStopped, "", "system"); err != nil {
			return "", false, err
		}
	}
	return fmt.Sprintf("accepted by LiveKit; the pass wrote voice_camera_unclaimed (%s) and removed the publisher (camera_unclaimed), gone from LiveKit within %s of the row", detail, time.Since(start).Round(100*time.Millisecond)), true, nil
}

// cameraCap claims nine cameras in capcams and sees the tenth refused.
func (l *live) cameraCap(ctx context.Context, d *conchdProc) error {
	h := l.h
	var conns []*lkit.SignalConn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	var members []*person
	var room string
	for i := 1; i <= 10; i++ {
		p, err := h.newPersonWith(d, fmt.Sprintf("cap%d", i), personOpts{noCLI: true}, "capcams")
		if err != nil {
			return err
		}
		s, err := h.session(p, "capcams")
		if err != nil {
			return err
		}
		room = s.Rooms[0].Room
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		c, status, body, err := lkit.DialSignal(dctx, l.srv.WSURL, s.Rooms[0].Token)
		cancel()
		if err != nil {
			return fmt.Errorf("%s's signalling join: %d %s: %w", p.name, status, h.redact(body), err)
		}
		conns = append(conns, c)
		members = append(members, p)
	}
	for _, p := range members {
		if err := waitFor("LiveKit to list "+p.name, 30*time.Second, func() (bool, string) {
			ok, _, saw := l.lkHas(room, p.id)
			return ok, saw
		}); err != nil {
			return err
		}
	}
	var claims []schema.VoiceCameraV1
	for _, p := range members[:9] {
		c, err := mustCamera(p, "capcams", "")
		if err != nil {
			return err
		}
		claims = append(claims, c)
	}
	status, data, err := members[9].api.do(http.MethodPost, "/v1/channels/capcams/voice/camera", nil)
	if err != nil {
		return err
	}
	var e schema.Error
	_ = decodeStrict(data, &e)
	if status != http.StatusConflict || e.Code != schema.ErrorCodeVoiceCameraFull {
		return fmt.Errorf("the tenth claim: status %d code %q, want 409 %s", status, e.Code, schema.ErrorCodeVoiceCameraFull)
	}
	for _, p := range members {
		if strings.Contains(e.Message, p.name) || strings.Contains(e.Message, fmt.Sprint(p.id)) {
			return fmt.Errorf("the refusal names %s: %q", p.name, e.Message)
		}
	}
	if _, err := mustCamera(members[0], "capcams", cameraQuery(claims[0])); err != nil {
		return fmt.Errorf("a holder's continue at the cap: %w", err)
	}
	for i, p := range members[:9] {
		if err := stopCamera(p.api, "capcams", cameraQuery(claims[i])); err != nil {
			return fmt.Errorf("%s releases: %w", p.name, err)
		}
	}
	if _, err := mustCamera(members[9], "capcams", ""); err != nil {
		return fmt.Errorf("a claim once there is room: %w", err)
	}
	if err := stopCamera(members[9].api, "capcams", ""); err != nil {
		return err
	}
	h.say("ok   cap: nine cameras claimed in one room, the tenth refused 409 voice_camera_full naming nobody, a holder's continue answered at the cap, and a claim made once one was released")
	return nil
}

// cameraRowsOf is the camera rows of subject, per person, as "<what> <rest of
// the detail>", with the actor named before what when it is not the person
// (the operator) and principals by name.
func cameraRowsOf(d *conchdProc, subject string, people map[string]*person) (map[string][]string, error) {
	all, err := d.audit()
	if err != nil {
		return nil, err
	}
	byActor := map[string]string{}
	replace := []string{}
	for name, p := range people {
		byActor[fmt.Sprintf("principal:%d", p.id)] = name
		replace = append(replace, fmt.Sprintf("principal=%d ", p.id), "principal="+name+" ")
	}
	r := strings.NewReplacer(replace...)
	out := map[string][]string{}
	for _, e := range all {
		if !strings.HasPrefix(e.Action, "voice_camera_") || e.Subject != subject {
			continue
		}
		what := strings.TrimPrefix(e.Action, "voice_camera_")
		detail := r.Replace(e.Detail + " ")
		_, rest, _ := strings.Cut(detail, " ")
		rest = strings.TrimSpace(rest)
		who, person := byActor[e.Actor]
		if !person {
			// A stop by the operator or the system: whose it was is in the
			// detail.
			for name := range people {
				if strings.Contains(detail, "principal="+name+" ") {
					who = name
				}
			}
			actor := "operator"
			if e.Actor == "system" {
				actor = "system"
			}
			what = actor + " " + what
		}
		out[who] = append(out[who], strings.TrimSpace(what+" "+rest))
	}
	return out, nil
}

// auditCountOn is auditCount on another conchd than the live half's.
func (l *live) auditCountOn(d *conchdProc, action, subject, actor string) (int, error) {
	rows, err := d.auditRows(action)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range rows {
		if (subject == "" || e.Subject == subject) && (actor == "" || e.Actor == actor) {
			n++
		}
	}
	return n, nil
}

// waitAuditOn waits, on another conchd than the live half's, for a row with
// action (narrowed as auditCount narrows) to be written.
func (l *live) waitAuditOn(d *conchdProc, what, action, subject, actor string) error {
	return waitFor(what, 30*time.Second, func() (bool, string) {
		n, err := l.auditCountOn(d, action, subject, actor)
		if err != nil {
			return false, err.Error()
		}
		return n > 0, fmt.Sprintf("no %s row yet", action)
	})
}
