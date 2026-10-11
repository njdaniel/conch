package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// refusedRoomGone joins LiveKit with token and requires LiveKit to turn it away
// because the room no longer exists. A join that is refused for another
// reason (an expired token is 401) would pass the test for the wrong reason,
// so the answer is checked, not only the refusal.
func (l *live) refusedRoomGone(ctx context.Context, what, token string) error {
	conn, status, body, err := dialSignal(ctx, l.srv.wsURL, token)
	if err == nil {
		conn.Close()
		return fmt.Errorf("%s was accepted: LiveKit admitted the removed member (HTTP %d); it should have refused because the room is gone", what, status)
	}
	if status != http.StatusNotFound || !strings.Contains(body, "room does not exist") {
		return fmt.Errorf("%s was refused with HTTP %d %q, want 404 \"requested room does not exist\"", what, status, l.h.redact(body))
	}
	return nil
}

func (l *live) rotations() (int, error) { return l.auditCount(store.AuditVoiceRoomRotated, "", "") }

// expectRotation requires exactly one more voice_room_rotated row than before,
// for the channel, written by the system, with exactly this reason and no
// room name.
func (l *live) expectRotation(what string, before int, subject, reason string, within time.Duration) error {
	if err := waitFor(what+": one more voice_room_rotated row", within, func() (bool, string) {
		n, err := l.rotations()
		if err != nil {
			return false, err.Error()
		}
		return n == before+1, fmt.Sprintf("%d voice_room_rotated rows, want %d", n, before+1)
	}); err != nil {
		return err
	}
	rows, err := l.d.auditRows(store.AuditVoiceRoomRotated)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("%s: no voice_room_rotated row", what)
	}
	last := rows[len(rows)-1]
	if last.Actor != "system" || last.Subject != subject || last.Detail != "reason="+reason {
		return fmt.Errorf("%s: voice_room_rotated row has actor %q subject %q detail %q; want system, %s, \"reason=%s\"", what, last.Actor, last.Subject, last.Detail, subject, reason)
	}
	return nil
}

// ---- scenario 2: removing a member who was issued a session rotates the room

func (l *live) removed(ctx context.Context) error {
	h, d := l.h, l.d
	alice, bob, oldRoom := l.alice, l.bob, l.bridgeRoom
	bobID, aliceID := bob.id, alice.id

	// Bob connects with a token conchd issues, and so is sent one by LiveKit.
	sb, err := h.session(bob, "bridge")
	if err != nil {
		return err
	}
	conchdToken := sb.Rooms[0].Token
	conn1, status, body, err := dialSignal(ctx, l.srv.wsURL, conchdToken)
	if err != nil {
		return fmt.Errorf("bob joining with conchd's token: HTTP %d %s", status, h.redact(body))
	}
	defer conn1.Close()
	var lkToken string
	if err := waitFor("LiveKit to send bob a token of its own", 30*time.Second, func() (bool, string) {
		for _, t := range conn1.issuedTokens() {
			if t != conchdToken {
				lkToken = t
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d JWTs seen, none new", len(conn1.issuedTokens()))
	}); err != nil {
		return err
	}
	h.secret("a LiveKit-issued token", lkToken)

	// Control: while bob is a member, the token LiveKit sent him admits him.
	// Without this, the refusal below could be a refusal of anything.
	conn2, status, body, err := dialSignal(ctx, l.srv.wsURL, lkToken)
	if err != nil {
		return fmt.Errorf("control: bob rejoining with LiveKit's own token while still a member: HTTP %d %s", status, h.redact(body))
	}
	defer conn2.Close()
	if err := waitFor("LiveKit to list bob in bridge's room", 30*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(oldRoom, bobID)
		return ok, saw
	}); err != nil {
		return err
	}
	if ok, _, saw := l.lkHas(oldRoom, aliceID); !ok {
		return fmt.Errorf("before the removal alice is not in bridge's room (%s)", saw)
	}

	// Everyone who is connected is shown, to everyone, on every surface: the
	// REST snapshot of each, alice's socket, and bob's socket.
	bobWS, status, err := h.openPresenceSocket(ctx, bob)
	if err != nil {
		return fmt.Errorf("bob opening the presence socket: HTTP %d", status)
	}
	for _, who := range []struct {
		name string
		p    *person
	}{{"alice", alice}, {"bob", bob}} {
		if err := waitFor("presence (REST) as "+who.name+" to list exactly alice and bob", 30*time.Second, func() (bool, string) {
			doc, err := who.p.presence("bridge")
			if err != nil {
				return false, err.Error()
			}
			ids := presenceIDs(doc)
			return sameIDs(ids, []int64{aliceID, bobID}), fmt.Sprintf("lists %v", ids)
		}); err != nil {
			return err
		}
	}
	if err := l.aliceWS.waitIDs("alice's presence socket to list exactly alice and bob", 30*time.Second, aliceID, bobID); err != nil {
		return err
	}
	if err := bobWS.waitIDs("bob's presence socket to list exactly alice and bob", 30*time.Second, aliceID, bobID); err != nil {
		return err
	}

	before, err := l.rotations()
	if err != nil {
		return err
	}
	leftBefore := map[int64]int{}
	for _, id := range []int64{aliceID, bobID} {
		if leftBefore[id], err = l.auditCount(store.AuditVoiceLeft, l.bridgeSubject, fmt.Sprintf("principal:%d", id)); err != nil {
			return err
		}
	}
	// A token for the rejoin attempt, fresh so that it cannot be refused for
	// having expired.
	removedAt := time.Now()
	fresh, err := h.session(bob, "bridge")
	if err != nil {
		return err
	}
	h.say("ok   removal set-up: alice (lk) and bob are in bridge's room and shown, exactly, on every surface; LiveKit sent bob its own token (control: it admits him while he is a member)")

	if err := d.operator.call(http.MethodDelete, fmt.Sprintf("/v1/channels/bridge/members/%d", bobID), nil, nil); err != nil {
		return fmt.Errorf("remove bob from bridge: %w", err)
	}

	// The removal's hook runs before the response is written, so the answer is
	// the same at once: no waiting for a sweep.
	if err := l.refusedRoomGone(ctx, "the token LiveKit itself sent bob", lkToken); err != nil {
		return err
	}
	if err := l.refusedRoomGone(ctx, "a fresh token conchd issued bob", fresh.Rooms[0].Token); err != nil {
		return err
	}
	if err := waitFor("bridge's old room to be gone from LiveKit", 20*time.Second, func() (bool, string) {
		rooms, err := l.lkRooms()
		if err != nil {
			return false, err.Error()
		}
		return !contains(rooms, oldRoom), fmt.Sprintf("LiveKit has %d rooms, the old one among them", len(rooms))
	}); err != nil {
		return err
	}
	if err := waitFor("everyone in the old room to be disconnected", 20*time.Second, func() (bool, string) {
		bobGone, sawBob := l.lkGone(oldRoom, bobID)
		aliceGone, sawAlice := l.lkGone(oldRoom, aliceID)
		return bobGone && aliceGone && conn2.closed(), fmt.Sprintf("bob: %s; alice: %s; bob's connection closed=%v", sawBob, sawAlice, conn2.closed())
	}); err != nil {
		return err
	}
	// The removal ends bob's presence socket: he may no longer see the channel.
	if err := waitFor("bob's presence socket to be closed by his removal", 20*time.Second, func() (bool, string) {
		ended, _ := bobWS.closeStatus()
		_, frames := bobWS.latest()
		return ended, fmt.Sprintf("still open (%d frames received)", frames)
	}); err != nil {
		return err
	}
	if _, st := bobWS.closeStatus(); st != websocket.StatusPolicyViolation {
		return fmt.Errorf("bob's presence socket was closed with status %d, want %d (policy violation: no longer a member)", st, websocket.StatusPolicyViolation)
	}
	// The headless client has done its part; its retries would only hit the
	// same refusal.
	l.aliceHL.stop()

	if err := expectRefusal("a removed member asking for a session", bob.api, http.MethodPost, "/v1/channels/bridge/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	// The other member gets a session for the new room and is shown there.
	sa, err := h.session(alice, "bridge")
	if err != nil {
		return err
	}
	newRoom := sa.Rooms[0].Room
	h.room(newRoom)
	if newRoom == oldRoom {
		return fmt.Errorf("after the removal alice was given the same room as before")
	}
	hl, err := l.joinHeadless(sa.Rooms[0])
	if err != nil {
		return err
	}
	l.aliceHL, l.bridgeRoom = hl, newRoom
	if err := waitForOrFail("LiveKit to list alice in the new room", 60*time.Second, func() (bool, string, error) {
		ok, _, saw := l.lkHas(newRoom, aliceID)
		if !ok && hl.exited() {
			return false, "", errors.New("lk exited: " + hl.tail(h))
		}
		return ok, saw, nil
	}); err != nil {
		return err
	}
	if rooms, err := l.lkRooms(); err != nil || contains(rooms, oldRoom) || !contains(rooms, newRoom) {
		return fmt.Errorf("after rejoining: old room present=%v new room present=%v (%s)", contains(rooms, oldRoom), contains(rooms, newRoom), exitText(err))
	}
	if err := waitFor("alice's presence to show exactly her, in the new room", 30*time.Second, func() (bool, string) {
		doc, err := alice.presence("bridge")
		if err != nil {
			return false, err.Error()
		}
		ids := presenceIDs(doc)
		return sameIDs(ids, []int64{aliceID}), fmt.Sprintf("lists %v", ids)
	}); err != nil {
		return err
	}
	if err := l.aliceWS.waitIDs("alice's presence socket to follow her to the new room", 30*time.Second, aliceID); err != nil {
		return err
	}
	out, err := alice.cli.run("", "voice", "status", "bridge")
	if err != nil || !strings.HasPrefix(out, fmt.Sprintf("%d alice ", aliceID)) {
		return fmt.Errorf("conch voice status as alice after the rotation: exit=%s output=%q, want her own line", exitText(err), strings.TrimSpace(out))
	}

	// Audit: one rotation, with the reason and no room name; and the people
	// who were in the old room are recorded as having left it.
	if err := l.expectRotation("bob's removal", before, l.bridgeSubject, store.VoiceRotateMemberRemoved, 30*time.Second); err != nil {
		return err
	}
	for _, id := range []int64{aliceID, bobID} {
		actor := fmt.Sprintf("principal:%d", id)
		if err := waitFor(fmt.Sprintf("a voice_left row for principal %d after the rotation", id), 30*time.Second, func() (bool, string) {
			n, err := l.auditCount(store.AuditVoiceLeft, l.bridgeSubject, actor)
			if err != nil {
				return false, err.Error()
			}
			return n == leftBefore[id]+1, fmt.Sprintf("%d voice_left rows, was %d", n, leftBefore[id])
		}); err != nil {
			return err
		}
	}
	// alice was left unmuted and reports nothing, so she had an unreported
	// transmission open in the old room. The rotation closed it at once, with
	// reason=left, before it recorded her leaving (issue #135,
	// docs/design/conch-voice.md §6).
	aliceActor := fmt.Sprintf("principal:%d", aliceID)
	if err := waitFor("alice's open transmission to be closed with reason=left by the rotation", 30*time.Second, func() (bool, string) {
		_, stops, _, err := l.transmitRows(l.bridgeSubject, aliceActor, removedAt)
		if err != nil {
			return false, err.Error()
		}
		if len(stops) == 0 {
			return false, "no observed voice_transmit_stopped row for alice since the removal"
		}
		lefts, err := l.d.auditRows(store.AuditVoiceLeft)
		if err != nil {
			return false, err.Error()
		}
		var left store.AuditEvent
		for _, e := range lefts {
			if e.Subject == l.bridgeSubject && e.Actor == aliceActor {
				left = e
			}
		}
		return strings.HasSuffix(stops[0].Detail, " source=observed reason=left") && stops[0].ID < left.ID,
			fmt.Sprintf("the first stop since the removal has detail %q and id %d; her last voice_left has id %d", stops[0].Detail, stops[0].ID, left.ID)
	}); err != nil {
		return err
	}
	h.say("ok   removed: bob's removal disconnected everyone and deleted the old room, closed his presence socket (policy violation); both his tokens (conchd's, LiveKit's) are refused 404; alice got a new room and is shown there; one voice_room_rotated reason=member_removed; alice's unreported transmission closed with reason=left, then voice_left for alice and bob")
	return nil
}

// ---- scenario 3: removing a member who holds a session but never connected

func (l *live) removedNotConnected(ctx context.Context) error {
	h, d := l.h, l.d
	dan, err := h.newPerson(d, "dan", "bridge")
	if err != nil {
		return err
	}
	sd, err := h.session(dan, "bridge")
	if err != nil {
		return err
	}
	room := sd.Rooms[0].Room
	// Control: the room exists, so the refusal below is the rotation's doing.
	if rooms, err := l.lkRooms(); err != nil || !contains(rooms, room) {
		return fmt.Errorf("control: bridge's room is not in LiveKit before the removal (%s)", exitText(err))
	}
	before, err := l.rotations()
	if err != nil {
		return err
	}
	if err := d.operator.call(http.MethodDelete, fmt.Sprintf("/v1/channels/bridge/members/%d", dan.id), nil, nil); err != nil {
		return err
	}
	if err := l.refusedRoomGone(ctx, "the token of a removed holder who never connected", sd.Rooms[0].Token); err != nil {
		return err
	}
	if err := l.expectRotation("dan's removal", before, l.bridgeSubject, store.VoiceRotateMemberRemoved, 30*time.Second); err != nil {
		return err
	}
	l.aliceHL.stop()
	h.say("ok   removed while not connected: a holder who never joined still rotates the room, and his token is refused")
	return nil
}

// ---- scenario 4: every other way of losing the right to be in a room

// rotationCase is one way a holder stops being entitled. Each runs in a
// channel of its own: a member gets a session, connects, is sent LiveKit's own
// token, and then the thing happens.
type rotationCase struct {
	name   string
	reason string
	// act makes it happen; nil for the credential that expires by itself.
	act func(l *live, p *person) error
	// expires, when set, gives the holder a credential that stops being live
	// this long after it was made.
	expires time.Duration
	// extraCredential gives the holder a second credential, so that
	// revoke-all has more than the one in use to revoke.
	extraCredential bool
}

func (l *live) rotationCases() []rotationCase {
	op := func(method, path string) error { return l.d.operator.call(method, path, nil, nil) }
	return []rotationCase{
		{name: "principal-disabled", reason: store.VoiceRotatePrincipalDisabled, act: func(l *live, p *person) error {
			return op(http.MethodPost, fmt.Sprintf("/v1/principals/%d/disable", p.id))
		}},
		{name: "credential-revoked", reason: store.VoiceRotateRevoked, act: func(l *live, p *person) error {
			return op(http.MethodDelete, fmt.Sprintf("/v1/credentials/%d", p.credID))
		}},
		{name: "all-credentials-revoked", reason: store.VoiceRotateRevoked, extraCredential: true, act: func(l *live, p *person) error {
			return op(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", p.id))
		}},
	}
}

func (l *live) rotationScenario(ctx context.Context) error {
	for _, c := range l.rotationCases() {
		if err := l.rotationCase(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
	}
	return nil
}

// rotationCase runs one case. For a case with act == nil it waits for the
// sweep to notice, with no request at all (the credential simply expires).
func (l *live) rotationCase(ctx context.Context, c rotationCase) error {
	h, d := l.h, l.d
	chName := "rot-" + c.name
	chID, err := d.createChannel(chName)
	if err != nil {
		return err
	}
	subject := fmt.Sprintf("channel:%d", chID)
	opts := personOpts{noCLI: true}
	if c.expires > 0 {
		t := time.Now().Add(c.expires)
		opts.expires = &t
	}
	holder, err := h.newPersonWith(d, "holder-"+c.name, opts, chName)
	if err != nil {
		return err
	}
	if c.extraCredential {
		var extra schema.CreateCredentialResponseV1
		if err := d.operator.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", holder.id), schema.CreateCredentialRequestV1{Label: "second"}, &extra); err != nil {
			return err
		}
		h.secret("a second conch credential", extra.Token)
	}
	sess, err := h.session(holder, chName)
	if err != nil {
		return err
	}
	room, conchdToken := sess.Rooms[0].Room, sess.Rooms[0].Token
	conn1, status, body, err := dialSignal(ctx, l.srv.wsURL, conchdToken)
	if err != nil {
		return fmt.Errorf("the holder joining with conchd's token: HTTP %d %s", status, h.redact(body))
	}
	defer conn1.Close()
	var lkToken string
	if err := waitFor("LiveKit to send the holder a token of its own", 30*time.Second, func() (bool, string) {
		for _, t := range conn1.issuedTokens() {
			if t != conchdToken {
				lkToken = t
				return true, ""
			}
		}
		return false, "no token of LiveKit's yet"
	}); err != nil {
		return err
	}
	h.secret("a LiveKit-issued token", lkToken)
	// Control: LiveKit's token admits the holder while they are entitled.
	conn2, status, body, err := dialSignal(ctx, l.srv.wsURL, lkToken)
	if err != nil {
		return fmt.Errorf("control: the holder rejoining with LiveKit's own token while entitled: HTTP %d %s", status, h.redact(body))
	}
	defer conn2.Close()
	if err := waitFor("LiveKit to list the holder", 30*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(room, holder.id)
		return ok, saw
	}); err != nil {
		return err
	}
	before, err := l.rotations()
	if err != nil {
		return err
	}
	within := 30 * time.Second
	if c.act != nil {
		if err := c.act(l, holder); err != nil {
			return err
		}
	} else {
		// Nothing is done: the credential expires, and the sweep (at most
		// 30 s apart) is the only thing that can notice.
		within = 75 * time.Second
	}
	if c.act != nil {
		if err := l.refusedRoomGone(ctx, "LiveKit's own token for the holder", lkToken); err != nil {
			return err
		}
	}
	if err := l.expectRotation(c.name, before, subject, c.reason, within); err != nil {
		return err
	}
	if err := l.refusedRoomGone(ctx, "LiveKit's own token for the holder", lkToken); err != nil {
		return err
	}
	if err := l.refusedRoomGone(ctx, "the token conchd issued the holder", conchdToken); err != nil {
		return err
	}
	if err := waitFor("the holder to be disconnected and the room gone", 20*time.Second, func() (bool, string) {
		gone, saw := l.lkGone(room, holder.id)
		return gone && conn2.closed(), fmt.Sprintf("%s; connection closed=%v", saw, conn2.closed())
	}); err != nil {
		return err
	}
	h.say("ok   rotation (%s): LiveKit's token and conchd's are refused 404, the holder is disconnected, one voice_room_rotated reason=%s", c.name, c.reason)
	return nil
}

// credentialExpires: a credential that expires with no request at all is
// noticed by the sweep alone. The wait it needs doubles as the sweep after
// which the never-issued check is repeated.
func (l *live) credentialExpires(ctx context.Context) error {
	if err := l.rotationCase(ctx, rotationCase{name: "credential-expired", reason: store.VoiceRotateExpired, expires: 12 * time.Second}); err != nil {
		return fmt.Errorf("credential-expired: %w", err)
	}
	if l.afterSweep != nil {
		return l.afterSweep()
	}
	return nil
}

// ---- scenario 5: removing a member who was never issued a session

func (l *live) neverIssued(ctx context.Context) error {
	h, d := l.h, l.d
	calmID, err := d.createChannel("calm")
	if err != nil {
		return err
	}
	calmSubject := fmt.Sprintf("channel:%d", calmID)
	fay, err := h.newPerson(d, "fay", "calm")
	if err != nil {
		return err
	}
	l.fay = fay
	gus, err := h.newPerson(d, "gus", "calm")
	if err != nil {
		return err
	}
	hal, err := h.newPerson(d, "hal", "calm")
	if err != nil {
		return err
	}
	sf, err := h.session(fay, "calm")
	if err != nil {
		return err
	}
	sg, err := h.session(gus, "calm")
	if err != nil {
		return err
	}
	calmRoom := sf.Rooms[0].Room
	h.room(calmRoom)
	for _, s := range []struct {
		who   *person
		grant schema.VoiceRoomGrant
	}{{fay, sf.Rooms[0]}, {gus, sg.Rooms[0]}} {
		conn, status, body, err := dialSignal(ctx, l.srv.wsURL, s.grant.Token)
		if err != nil {
			return fmt.Errorf("%s joining calm: HTTP %d %s", s.who.name, status, h.redact(body))
		}
		h.onCleanup(conn.Close)
		who := s.who
		if err := waitFor("LiveKit to list "+who.name+" in calm's room", 30*time.Second, func() (bool, string) {
			ok, _, saw := l.lkHas(calmRoom, who.id)
			return ok, saw
		}); err != nil {
			return err
		}
	}
	if err := d.operator.call(http.MethodDelete, fmt.Sprintf("/v1/channels/calm/members/%d", hal.id), nil, nil); err != nil {
		return fmt.Errorf("remove hal from calm: %w", err)
	}
	check := func(when string) error {
		// Other channels rotate while this runs; calm must never.
		after, err := l.auditCount(store.AuditVoiceRoomRotated, calmSubject, "")
		if err != nil {
			return err
		}
		if after != 0 {
			return fmt.Errorf("%s: %d voice_room_rotated rows for calm: removing a member who never had a session rotated its room", when, after)
		}
		rooms, err := l.lkRooms()
		if err != nil {
			return err
		}
		if !contains(rooms, calmRoom) {
			return fmt.Errorf("%s: calm's room is gone from LiveKit", when)
		}
		for _, p := range []*person{fay, gus} {
			if ok, _, saw := l.lkHas(calmRoom, p.id); !ok {
				return fmt.Errorf("%s: %s was disconnected (%s)", when, p.name, saw)
			}
		}
		again, err := h.session(fay, "calm")
		if err != nil {
			return err
		}
		if again.Rooms[0].Room != calmRoom {
			return fmt.Errorf("%s: fay was given a different room after the removal of someone who never had a session", when)
		}
		return nil
	}
	// The hook has run by the time the removal is answered.
	if err := check("right after the removal"); err != nil {
		return err
	}
	// The same must hold after a sweep (the poller's 30-second pass over
	// every stored room). The run waits for one anyway, in the expiry case
	// below, which can only be noticed by a sweep that starts after this
	// removal; l.afterSweep is called then, so this check adds no wait of
	// its own.
	l.afterSweep = func() error { return check("after a sweep") }
	h.say("ok   never issued: removing hal, who never had a session, rotated nothing and disconnected nobody (checked again after a sweep below)")
	return nil
}

// ---- scenario 6: LiveKit stopped

func (l *live) liveKitDown(ctx context.Context) error {
	h := l.h
	fay := l.fay
	outagesBefore, err := l.auditCount(store.AuditVoiceEnforcementUnavailable, "", "")
	if err != nil {
		return err
	}
	if err := l.srv.stop(ctx); err != nil {
		return err
	}
	h.say("ok   LiveKit container stopped")
	if err := expectDown(fay, "calm"); err != nil {
		return err
	}
	if err := l.waitAudit("exactly one voice_enforcement_unavailable row for the outage", store.AuditVoiceEnforcementUnavailable, "", "", outagesBefore+1); err != nil {
		return err
	}
	// Messaging does not notice.
	if out, err := fay.cli.run("", "send", "calm", "still here"); err != nil {
		return fmt.Errorf("posting a message with LiveKit down: %w (%s)", err, strings.TrimSpace(out))
	}
	h.say("ok   LiveKit down: session answers voice_unavailable, presence says unavailable, conch voice status exits nonzero, voice_enforcement_unavailable audited once, messaging works")
	cfg := l.srv.settings()
	return h.runDogfood("voice configured, LiveKit container stopped",
		"CONCHD_LIVEKIT_URL="+cfg.url, "CONCHD_LIVEKIT_API_KEY="+cfg.key, "CONCHD_LIVEKIT_API_SECRET="+cfg.secret)
}
