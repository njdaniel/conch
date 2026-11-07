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

// The live-media rows of the permission gates (issue #1217, ADR-014 design
// §7.5): the four voice gates that need a participant in a LiveKit room or a
// live share, which a request and its response cannot make. They run only
// with LiveKit; without it voiceMemberRows' caller says so.
//
//   - the voice switch's hook (handleVoicePermission, voiceAfterVoiceOff,
//     voiceEvictLost; not voiceWithheldAmong, which only a column cut or a
//     role change reaches, see pendingGates): the owner switches a member's
//     voice off through the real PUT, and the moment the PUT answers LiveKit
//     no longer lists the member and the removal is audited;
//   - voiceEntitlement: a join token signed before that switch still
//     connects (LiveKit knows nothing of it), and the next pass removes the
//     participant, audited with the voice reason; a new session is refused;
//   - sharesWithheld: share_screen is cut in the store, with no hook run (a
//     restart, a missed call), and the next pass ends the live share and
//     withdraws its tracks, audited access_lost, while the member stays in
//     the room.
//
// Each row has its control: with the cell back the same member is entitled
// again (a new session, a connection that stays; a fresh claim of the share).
//
//   - camerasWithheld and endCameraIfWithheld (issue #1467, the camera's two
//     gates that a live claim can drive; cameraGateRows): use_camera is cut
//     in the store with no hook and the next pass ends the live camera
//     (audited access_lost); a role change that withholds use_camera ends it
//     before the PUT answers.
//
// voice_share.go voicePoller.shareEntitled and voice.go Server.voiceStillEntitled
// have no row here: each is reached only by a cut landing inside a claim or a
// session request, which the server's seams make (see pendingGates).

// voiceGatePasses is how long the rows wait for the poller: a pass runs every
// 1.5 s, and a removal or a withdrawal answers within seconds. The bound is
// generous so that a loaded CI machine does not fail a row that is right.
const voiceGatePasses = 30 * time.Second

// removalsOf counts the voice_participant_removed rows naming principal id
// with the voice reason.
func (w *world) removalsOf(id int64) (int, error) {
	rows, err := w.auditRows(store.AuditVoiceParticipantRemoved)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if strings.Contains(r.Detail, "principal="+itoa(id)+" ") && strings.Contains(r.Detail, "reason=voice_not_allowed") {
			n++
		}
	}
	return n, nil
}

// joinAs connects a bare signalling participant on m's own session in
// channel, and answers the room, the grant's token and the connection.
func (w *world) joinAs(m *member, channel string) (room string, c *lkit.SignalConn, err error) {
	s, err := w.session(m, channel)
	if err != nil {
		return "", nil, err
	}
	c, err = w.joinSignal(s.Rooms[0])
	if err != nil {
		return "", nil, err
	}
	return s.Rooms[0].Room, c, nil
}

// requireListed waits for LiveKit to list m in room.
func (w *world) requireListed(what, room string, m *member) error {
	return waitFor(what, 15*time.Second, func() (bool, string) {
		ok, _, saw := w.lk.has(room, m.id)
		return ok, saw
	})
}

// requireStays requires that LiveKit still lists m in room after the poller
// has had two passes to decide otherwise: the control that the member is
// entitled.
func (w *world) requireStays(what, room string, m *member) error {
	time.Sleep(4 * time.Second)
	if ok, _, saw := w.lk.has(room, m.id); !ok {
		return fmt.Errorf("%s: %s left the room: %s", what, m.name, saw)
	}
	return nil
}

// voiceGateRows runs the rows for the members of channel (a channel the
// restricted phase made). It answers how many ran.
func (w *world) voiceGateRows(channel string, name func(string) string) (rows int, err error) {
	voice := "/v1/channels/" + channel + "/voice"
	voiceSwitch := func(m *member, allowed bool) error {
		return w.op.call(http.MethodPut, "/v1/principals/"+itoa(m.id)+"/voice-permission", schema.VoicePermissionRequestV1{VoiceAllowed: allowed}, nil)
	}

	// -- voice off: the hook, then a token from before the cut.
	v, err := w.newPerson(name("vgate"), channel)
	if err != nil {
		return rows, err
	}
	room, live, err := w.joinAs(v, channel)
	if err != nil {
		return rows, fmt.Errorf("the control: a member who holds use_voice joins: %w", err)
	}
	if err := w.requireListed("LiveKit to list the member who holds use_voice", room, v); err != nil {
		return rows, err
	}
	// A token signed while the member was entitled, kept for after the cut.
	stale, err := w.session(v, channel)
	if err != nil {
		return rows, err
	}
	if err := voiceSwitch(v, false); err != nil {
		return rows, fmt.Errorf("the owner switches the member's voice off: %w", err)
	}
	// The hook finishes before the PUT answers (the server's
	// TestVoicePermissionOffRemovesAtOnce is the deterministic proof), so the
	// check is made at once, with no wait: a pass (1.5 s) would be too late
	// to be what removed the participant.
	if gone, saw := w.lk.gone(room, v.id); !gone {
		return rows, fmt.Errorf("the hook of the voice switch: the member is still in LiveKit when the PUT has answered: %s", saw)
	}
	if n, err := w.removalsOf(v.id); err != nil {
		return rows, err
	} else if n == 0 {
		return rows, errors.New("the hook of the voice switch: no voice_participant_removed row for the member when the PUT has answered")
	}
	live.Close()
	rows++

	// The stale token connects (LiveKit knows nothing of the switch) and the
	// next pass removes it (voiceEntitlement), audited with the voice reason.
	before, err := w.removalsOf(v.id)
	if err != nil {
		return rows, err
	}
	late, err := w.joinSignal(stale.Rooms[0])
	if err != nil {
		return rows, fmt.Errorf("a token signed before the switch no longer connects (the pass is not what refuses it): %w", err)
	}
	defer late.Close()
	if err := waitFor("the pass to remove the member whose token predates the switch", voiceGatePasses, func() (bool, string) {
		n, err := w.removalsOf(v.id)
		if err != nil {
			return false, err.Error()
		}
		if n <= before {
			return false, "no voice_participant_removed row with the voice reason for the member yet"
		}
		return w.lk.gone(stale.Rooms[0].Room, v.id)
	}); err != nil {
		return rows, fmt.Errorf("a participant on a token from before the switch (voiceEntitlement): %w", err)
	}
	status, body, err := v.api.do(http.MethodPost, voice+"/session", nil)
	if err := refusedNaming("a new session after the switch", status, body, err, schema.ErrorCodeVoiceNotAllowed, schema.PermUseVoice); err != nil {
		return rows, err
	}
	late.Close()
	rows++

	// The control: the switch back, and the member is entitled again.
	if err := voiceSwitch(v, true); err != nil {
		return rows, fmt.Errorf("the owner switches the member's voice back on: %w", err)
	}
	room, back, err := w.joinAs(v, channel)
	if err != nil {
		return rows, fmt.Errorf("the control: with the voice switch back the member joins: %w", err)
	}
	defer back.Close()
	if err := w.requireListed("LiveKit to list the member with the voice switch back", room, v); err != nil {
		return rows, err
	}
	if err := w.requireStays("the control: with the voice switch back", room, v); err != nil {
		return rows, err
	}
	back.Close()

	// -- share_screen cut in the store: the pass ends the share.
	n, err := w.sharesWithheldRow(channel, name)
	rows += n
	if err != nil {
		return rows, err
	}

	// -- use_camera cut in the store, and by a role change (issue #1467).
	n, err = w.cameraGateRows(channel, name)
	rows += n
	return rows, err
}

// sharesWithheldRow is sharesWithheld's row (voice_share.go): a member's live
// share, share_screen then cut in the store with no hook, ended by the pass.
func (w *world) sharesWithheldRow(channel string, name func(string) string) (rows int, err error) {
	lk := w.lk
	sharer, err := w.newPerson(name("sgate"), channel)
	if err != nil {
		return 0, err
	}
	s, err := w.session(sharer, channel)
	if err != nil {
		return 0, err
	}
	room := s.Rooms[0].Room
	dir, err := w.h.mkdir("sgate-")
	if err != nil {
		return 0, err
	}
	shareAudio := filepath.Join(dir, "share-audio.ogg")
	if err := lkit.WriteSilentOpus(shareAudio, 900); err != nil {
		return 0, err
	}
	// As startVoiceA: lk publishes its share as soon as it joins, so the
	// relay holds the tracks back until the claim is granted.
	gate := lkit.NewShareFilter(true)
	hl, err := w.h.kit.StartLK(lk.lkBin, lk.hostPort(), s.Rooms[0].Token, gate.Filter, "--publish-demo", "--publish", shareAudio, "--publish", lk.ogg)
	if err != nil {
		return 0, err
	}
	defer hl.Stop()
	if err := waitFor("LiveKit to list the sharer", 60*time.Second, func() (bool, string) {
		ok, _, saw := lk.has(room, sharer.id)
		if !ok && hl.Exited() {
			return false, "lk exited: " + hl.Tail()
		}
		return ok, saw
	}); err != nil {
		return 0, err
	}
	select {
	case <-gate.Held():
	case <-time.After(30 * time.Second):
		return 0, errors.New("the sharer's lk did not try to publish its share tracks within 30 s")
	}
	if err := sharer.api.call(http.MethodPost, "/v1/channels/"+channel+"/voice/share", nil, nil); err != nil {
		return 0, fmt.Errorf("the control: a member who holds share_screen claims the share: %w", err)
	}
	gate.Release()
	if err := waitFor("LiveKit to list the sharer's screen and its sound", 30*time.Second, func() (bool, string) {
		ok, p, saw := lk.has(room, sharer.id)
		if !ok {
			return false, saw
		}
		screen, audio := lkit.ShareTracks(p)
		return screen && audio, fmt.Sprintf("sources %v", p.Sources())
	}); err != nil {
		return 0, err
	}

	// The cut, as a store edit: the PUT's hook (endWithheldShares) does not
	// run, so only the pass (sharesWithheld) can find it.
	var who schema.WhoAmIResponseV1
	if err := w.op.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return 0, err
	}
	if err := w.underStore(func(ctx context.Context, st *store.Store) error {
		doc, err := st.Permissions(ctx)
		if err != nil {
			return err
		}
		_, err = st.PutPermissions(ctx, fmt.Sprintf("principal:%d", who.ID), who.ID, schema.PutPermissionsRequestV1{
			Revision: doc.Revision,
			Changes:  []schema.PermissionChangeV1{{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false}},
		}, time.Now())
		return err
	}); err != nil {
		return 0, fmt.Errorf("cut share_screen from the member column in the store: %w", err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = w.setCells(schema.PermissionRoleMember, true, schema.PermShareScreen)
		}
	}()
	if err := waitFor("the pass to end the share whose cell was cut", voiceGatePasses, func() (bool, string) {
		ok, p, saw := lk.has(room, sharer.id)
		if !ok {
			return false, "the sharer left the room: " + saw
		}
		screen, audio := lkit.ShareTracks(p)
		if screen || audio {
			return false, fmt.Sprintf("sources %v", p.Sources())
		}
		stopped, err := w.auditRows(store.AuditVoiceShareStopped)
		if err != nil {
			return false, err.Error()
		}
		for _, r := range stopped {
			if strings.Contains(r.Detail, "principal="+itoa(sharer.id)+" ") && strings.Contains(r.Detail, "reason=access_lost") {
				return true, ""
			}
		}
		return false, "no voice_share_stopped row with reason=access_lost for the sharer"
	}); err != nil {
		return 0, fmt.Errorf("a share whose share_screen was cut in the store (sharesWithheld): %w", err)
	}
	// The cell is gone; a claim now is refused at the handler as before.
	status, body, err := sharer.api.do(http.MethodPost, "/v1/channels/"+channel+"/voice/share", nil)
	if err := refusedNaming("the claim of a member whose share_screen was cut in the store", status, body, err, schema.ErrorCodeVoiceNotAllowed, schema.PermShareScreen); err != nil {
		return 0, err
	}
	rows++

	// The control: the cell back through the real PUT, the same member
	// claims the share again, still in the room.
	if err := w.setCells(schema.PermissionRoleMember, true, schema.PermShareScreen); err != nil {
		return rows, fmt.Errorf("the owner gives share_screen back: %w", err)
	}
	restored = true
	if err := sharer.api.call(http.MethodPost, "/v1/channels/"+channel+"/voice/share", nil, nil); err != nil {
		return rows, fmt.Errorf("the control: with share_screen back the member claims the share again: %w", err)
	}
	if err := sharer.api.call(http.MethodDelete, "/v1/channels/"+channel+"/voice/share", nil, nil); err != nil {
		return rows, fmt.Errorf("the control: the member stops the share: %w", err)
	}
	return rows, nil
}

// cameraCells reads the member and operator cells of use_camera.
func (w *world) cameraCells() (member, operator bool, err error) {
	var doc schema.PermissionsResponseV1
	if err := w.op.call(http.MethodGet, "/v1/permissions", nil, &doc); err != nil {
		return false, false, err
	}
	for _, r := range doc.Rows {
		if r.Permission == schema.PermUseCamera {
			return r.Member, r.Operator, nil
		}
	}
	return false, false, errors.New("the permissions document has no use_camera row")
}

// liveCamera makes a person in channel (an operator if operator), has them
// join the room, claim a camera and publish it, and answers when LiveKit
// lists the camera. hl is the person's lk; the caller stops it.
func (w *world) liveCamera(channel, who string, operator bool) (m *member, room string, hl *lkit.Headless, err error) {
	lk := w.lk
	if m, err = w.newPerson(who, channel); err != nil {
		return nil, "", nil, err
	}
	if operator {
		if err := w.op.call(http.MethodPut, "/v1/principals/"+itoa(m.id)+"/role", schema.RoleChangeRequestV1{Role: schema.RoleOperator}, nil); err != nil {
			return nil, "", nil, fmt.Errorf("the owner makes %s an operator: %w", who, err)
		}
	}
	s, err := w.session(m, channel)
	if err != nil {
		return nil, "", nil, err
	}
	room = s.Rooms[0].Room
	// As startCameraA: lk publishes its demo video as soon as it joins, so
	// the relay holds it back until the claim is granted.
	gate := newSourceGate(true, lkSourceCamera, func(name string) bool { return name == "demo" })
	if hl, err = w.h.kit.StartLK(lk.lkBin, lk.hostPort(), s.Rooms[0].Token, gate.Filter, "--publish-demo"); err != nil {
		return nil, "", nil, err
	}
	fail := func(err error) error {
		hl.Stop()
		return err
	}
	if err := waitFor("LiveKit to list "+who, 60*time.Second, func() (bool, string) {
		ok, _, saw := lk.has(room, m.id)
		if !ok && hl.Exited() {
			return false, "lk exited: " + hl.Tail()
		}
		return ok, saw
	}); err != nil {
		return nil, "", nil, fail(err)
	}
	select {
	case <-gate.Held():
	case <-time.After(30 * time.Second):
		return nil, "", nil, fail(fmt.Errorf("%s's lk did not try to publish its camera within 30 s", who))
	}
	if err := claimCamera(m, channel); err != nil {
		return nil, "", nil, fail(fmt.Errorf("the control: %s claims a camera: %w", who, err))
	}
	gate.Release()
	if err := waitFor("LiveKit to list "+who+"'s camera", 30*time.Second, func() (bool, string) {
		ok, p, saw := lk.has(room, m.id)
		if !ok {
			return false, saw
		}
		return cameraTrack(p), fmt.Sprintf("sources %v", p.Sources())
	}); err != nil {
		return nil, "", nil, fail(err)
	}
	return m, room, hl, nil
}

// cameraStoppedAccessLost reports whether the audit trail holds a
// voice_camera_stopped row with reason=access_lost naming principal id.
func (w *world) cameraStoppedAccessLost(id int64) (bool, error) {
	stopped, err := w.auditRows(store.AuditVoiceCameraStopped)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(stopped, func(r schema.AuditEventV1) bool {
		return strings.Contains(r.Detail, "principal="+itoa(id)+" ") && strings.Contains(r.Detail, "reason=access_lost")
	}), nil
}

// cameraGateRows are camerasWithheld's row (voice_camera.go) and
// endCameraIfWithheld's (permissions.go), against a live camera in channel,
// with the camera cells put back as found.
func (w *world) cameraGateRows(channel string, name func(string) string) (rows int, err error) {
	lk := w.lk
	wasMember, wasOperator, err := w.cameraCells()
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = w.setCells(schema.PermissionRoleMember, wasMember, schema.PermUseCamera)
		_ = w.setCells(schema.PermissionRoleOperator, wasOperator, schema.PermUseCamera)
	}()
	if err := w.setCameraCells(true); err != nil {
		return 0, err
	}

	// -- camerasWithheld: use_camera cut in the store, with no hook run.
	cam, room, hl, err := w.liveCamera(channel, name("cgate"), false)
	if err != nil {
		return 0, err
	}
	defer hl.Stop()
	var who schema.WhoAmIResponseV1
	if err := w.op.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return 0, err
	}
	if err := w.underStore(func(ctx context.Context, st *store.Store) error {
		doc, err := st.Permissions(ctx)
		if err != nil {
			return err
		}
		_, err = st.PutPermissions(ctx, fmt.Sprintf("principal:%d", who.ID), who.ID, schema.PutPermissionsRequestV1{
			Revision: doc.Revision,
			Changes:  []schema.PermissionChangeV1{{Permission: schema.PermUseCamera, Role: schema.PermissionRoleMember, Granted: false}},
		}, time.Now())
		return err
	}); err != nil {
		return 0, fmt.Errorf("cut use_camera from the member column in the store: %w", err)
	}
	if err := waitFor("the pass to end the camera whose cell was cut", voiceGatePasses, func() (bool, string) {
		ok, p, saw := lk.has(room, cam.id)
		if !ok {
			return false, "the member left the room: " + saw
		}
		if cameraTrack(p) {
			return false, fmt.Sprintf("sources %v", p.Sources())
		}
		held, err := w.cameraStoppedAccessLost(cam.id)
		if err != nil {
			return false, err.Error()
		}
		return held, "no voice_camera_stopped row with reason=access_lost for the member"
	}); err != nil {
		return 0, fmt.Errorf("a camera whose use_camera was cut in the store (camerasWithheld): %w", err)
	}
	status, body, err := cam.api.do(http.MethodPost, "/v1/channels/"+channel+"/voice/camera", nil)
	if err := refusedNaming("the camera claim of a member whose use_camera was cut in the store", status, body, err, schema.ErrorCodePermissionDenied, schema.PermUseCamera); err != nil {
		return 0, err
	}
	rows++
	// The control: the cell back through the real PUT, a fresh claim succeeds.
	if err := w.setCells(schema.PermissionRoleMember, true, schema.PermUseCamera); err != nil {
		return rows, fmt.Errorf("the owner gives use_camera back: %w", err)
	}
	if err := claimCamera(cam, channel); err != nil {
		return rows, fmt.Errorf("the control: with use_camera back the member claims a camera again: %w", err)
	}
	if err := cam.api.call(http.MethodDelete, "/v1/channels/"+channel+"/voice/camera", nil, nil); err != nil {
		return rows, fmt.Errorf("the control: the member stops the camera: %w", err)
	}
	hl.Stop()

	// -- endCameraIfWithheld: a demotion to a role without use_camera.
	if err := w.setCells(schema.PermissionRoleMember, false, schema.PermUseCamera); err != nil {
		return rows, err
	}
	op, oroom, ohl, err := w.liveCamera(channel, name("cdemote"), true)
	if err != nil {
		return rows, fmt.Errorf("an operator, whose cell holds use_camera, claims a camera: %w", err)
	}
	defer ohl.Stop()
	if err := w.op.call(http.MethodPut, "/v1/principals/"+itoa(op.id)+"/role", schema.RoleChangeRequestV1{Role: schema.RoleMember}, nil); err != nil {
		return rows, fmt.Errorf("the owner demotes the camera holder: %w", err)
	}
	// The hook ends the camera before the PUT answers: the audit row is read
	// at once, with no wait, since a pass (1.5 s) would be too late to be
	// what ended it. LiveKit unpublishes a withdrawn source within a moment.
	held, err := w.cameraStoppedAccessLost(op.id)
	if err != nil {
		return rows, err
	}
	if !held {
		return rows, errors.New("the role change's hook (endCameraIfWithheld): no voice_camera_stopped row with reason=access_lost for the demoted member when the PUT has answered")
	}
	if err := waitFor("the demoted member's camera to be withdrawn", 2*time.Second, func() (bool, string) {
		ok, p, saw := lk.has(oroom, op.id)
		if !ok {
			return false, "the member left the room: " + saw
		}
		return !cameraTrack(p), fmt.Sprintf("sources %v", p.Sources())
	}); err != nil {
		return rows, fmt.Errorf("the role change's hook (endCameraIfWithheld): %w", err)
	}
	rows++
	// The control: the demoted member, with the cell back, claims again.
	if err := w.setCells(schema.PermissionRoleMember, true, schema.PermUseCamera); err != nil {
		return rows, err
	}
	if err := claimCamera(op, channel); err != nil {
		return rows, fmt.Errorf("the control: with use_camera back the demoted member claims a camera: %w", err)
	}
	if err := op.api.call(http.MethodDelete, "/v1/channels/"+channel+"/voice/camera", nil, nil); err != nil {
		return rows, fmt.Errorf("the control: the member stops the camera: %w", err)
	}
	return rows, nil
}
