package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// refused joins LiveKit with token and requires LiveKit to turn it away
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

// ---- scenario 2: removing a member who was issued a session rotates the room

func (l *live) removed(ctx context.Context) error {
	h, d := l.h, l.d
	alice, bob, oldRoom := l.alice, l.bob, l.bridgeRoom

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
	bobID, aliceID := bob.id, alice.id
	if err := waitFor("LiveKit to list bob in bridge's room", 30*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(oldRoom, bobID)
		return ok, saw
	}); err != nil {
		return err
	}
	if ok, _, saw := l.lkHas(oldRoom, aliceID); !ok {
		return fmt.Errorf("before the removal alice is not in bridge's room (%s)", saw)
	}
	before, err := l.rotations()
	if err != nil {
		return err
	}
	// A token for the rejoin attempt, fresh so that it cannot be refused for
	// having expired.
	fresh, err := h.session(bob, "bridge")
	if err != nil {
		return err
	}
	h.say("ok   removal set-up: alice (lk) and bob are in bridge's room; LiveKit sent bob its own token (control: it admits him while he is a member)")

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
		inBob, _, _ := l.lkHas(oldRoom, bobID)
		inAlice, _, _ := l.lkHas(oldRoom, aliceID)
		return !inBob && !inAlice && conn2.closed(), fmt.Sprintf("bob in room=%v alice in room=%v bob's connection closed=%v", inBob, inAlice, conn2.closed())
	}); err != nil {
		return err
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
	if err := waitFor("LiveKit to list alice in the new room", 60*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(newRoom, aliceID)
		if !ok && hl.exited() {
			return false, "lk exited: " + hl.tail(h)
		}
		return ok, saw
	}); err != nil {
		return err
	}
	if rooms, err := l.lkRooms(); err != nil || contains(rooms, oldRoom) || !contains(rooms, newRoom) {
		return fmt.Errorf("after rejoining: old room present=%v new room present=%v (%s)", contains(rooms, oldRoom), contains(rooms, newRoom), exitText(err))
	}
	if err := waitFor("alice's presence to show her in the new room", 30*time.Second, func() (bool, string) {
		listed, _, saw, err := presenceOf(alice, aliceID)
		if err != nil {
			return false, err.Error()
		}
		return listed, saw
	}); err != nil {
		return err
	}
	out, err := alice.cli.run("", "voice", "status", "bridge")
	if err != nil || !strings.HasPrefix(out, fmt.Sprintf("%d alice ", aliceID)) {
		return fmt.Errorf("conch voice status as alice after the rotation: exit=%s output=%q, want her own line", exitText(err), strings.TrimSpace(out))
	}

	// Audit: one rotation, with the reason and no room name.
	if err := l.waitAudit("one voice_room_rotated row for bridge", store.AuditVoiceRoomRotated, l.bridgeSubject, "system", 1); err != nil {
		return err
	}
	rows, err := d.auditRows(store.AuditVoiceRoomRotated)
	if err != nil {
		return err
	}
	if len(rows) != before+1 || rows[len(rows)-1].Detail != "reason=member_removed" {
		return fmt.Errorf("voice_room_rotated rows: %d (was %d), last detail %q; want one new row with detail \"reason=member_removed\"", len(rows), before, rows[len(rows)-1].Detail)
	}
	h.say("ok   removed: bob's removal disconnected everyone and deleted the old room; both his tokens (conchd's, LiveKit's) are refused 404; alice got a new room and is shown there; one voice_room_rotated reason=member_removed")
	return nil
}

// ---- scenario 3: removing a member who was never issued a session

func (l *live) neverIssued(ctx context.Context) error {
	h, d := l.h, l.d
	if _, err := d.createChannel("calm"); err != nil {
		return err
	}
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
	before, err := l.rotations()
	if err != nil {
		return err
	}
	if err := d.operator.call(http.MethodDelete, fmt.Sprintf("/v1/channels/calm/members/%d", hal.id), nil, nil); err != nil {
		return fmt.Errorf("remove hal from calm: %w", err)
	}
	check := func(when string) error {
		after, err := l.rotations()
		if err != nil {
			return err
		}
		if after != before {
			return fmt.Errorf("%s: %d voice_room_rotated rows, were %d: removing a member who never had a session rotated a room", when, after, before)
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
		return nil
	}
	// The hook has run by the time the removal is answered.
	if err := check("right after the removal"); err != nil {
		return err
	}
	// Nothing is the thing being checked, so there is no event to wait for.
	// A few poller passes (500 ms each) give a wrongly scheduled rotation or
	// removal time to show up.
	time.Sleep(3 * time.Second)
	if err := check("after several poller passes"); err != nil {
		return err
	}
	again, err := h.session(fay, "calm")
	if err != nil {
		return err
	}
	if again.Rooms[0].Room != calmRoom {
		return fmt.Errorf("fay was given a different room after the removal of someone who never had a session")
	}
	h.say("ok   never issued: removing hal, who never had a session, rotated nothing and disconnected nobody")
	return nil
}

// ---- scenario 4: LiveKit stopped

func (l *live) liveKitDown(ctx context.Context) error {
	h := l.h
	fay := l.fay
	if err := l.srv.stop(ctx); err != nil {
		return err
	}
	h.say("ok   LiveKit container stopped")
	if err := expectDown(fay, "calm"); err != nil {
		return err
	}
	if err := waitFor("a voice_enforcement_unavailable audit row", 30*time.Second, func() (bool, string) {
		n, err := l.auditCount(store.AuditVoiceEnforcementUnavailable, "", "")
		if err != nil {
			return false, err.Error()
		}
		return n >= 1, fmt.Sprintf("%d rows", n)
	}); err != nil {
		return err
	}
	// Messaging does not notice.
	if out, err := fay.cli.run("", "send", "calm", "still here"); err != nil {
		return fmt.Errorf("posting a message with LiveKit down: %w (%s)", err, strings.TrimSpace(out))
	}
	h.say("ok   LiveKit down: session answers voice_unavailable, presence says unavailable, conch voice status exits nonzero, voice_enforcement_unavailable audited, messaging works")
	cfg := l.srv.settings()
	return h.runDogfood("voice configured, LiveKit container stopped",
		"CONCHD_LIVEKIT_URL="+cfg.url, "CONCHD_LIVEKIT_API_KEY="+cfg.key, "CONCHD_LIVEKIT_API_SECRET="+cfg.secret)
}
