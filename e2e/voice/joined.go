package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
)

// ---- scenario 1: agents refused, joined, transmitting, outsider, by name

func (l *live) joinedTransmittingOutsider(ctx context.Context) error {
	h, d := l.h, l.d
	bridgeID, err := d.createChannel("bridge")
	if err != nil {
		return err
	}
	if _, err := d.createChannel("side"); err != nil {
		return err
	}
	bridgeSubject := fmt.Sprintf("channel:%d", bridgeID)
	alice, err := h.newPerson(d, "alice", "bridge")
	if err != nil {
		return err
	}
	bob, err := h.newPerson(d, "bob", "bridge")
	if err != nil {
		return err
	}
	carol, err := h.newPerson(d, "carol", "side")
	if err != nil {
		return err
	}
	bot, err := h.newAgent(d, "bot", "bridge", bridgeID)
	if err != nil {
		return err
	}
	l.bot = bot

	// An agent that is a member of the channel and holds a manifest is still
	// refused voice, in every surface, and the refusals are audited.
	for _, r := range []struct {
		what, method, path string
	}{
		{"an agent asking for a session in a channel it is a member of", http.MethodPost, "/v1/channels/bridge/voice/session"},
		{"an agent reading presence of a channel it is a member of", http.MethodGet, "/v1/channels/bridge/voice"},
	} {
		if err := expectRefusal(r.what, bot.api, r.method, r.path, http.StatusForbidden, "forbidden"); err != nil {
			return err
		}
	}
	if _, status, err := h.openPresenceSocket(ctx, bot); err == nil || status != http.StatusForbidden {
		return fmt.Errorf("an agent opening the presence socket: HTTP %d (err %s), want 403", status, exitText(err))
	}
	botActor := fmt.Sprintf("principal:%d", bot.id)
	if err := waitFor("the agent's three refusals in the audit log", 15*time.Second, func() (bool, string) {
		rows, err := l.d.auditRows("access_denied")
		if err != nil {
			return false, err.Error()
		}
		n := 0
		for _, e := range rows {
			if e.Actor == botActor && strings.Contains(e.Detail, "reason=agents_no_voice") && e.Subject != "" {
				n++
			}
		}
		return n == 3, fmt.Sprintf("%d access_denied rows for the agent with reason agents_no_voice, want 3", n)
	}); err != nil {
		return err
	}
	h.say("ok   agent: a member agent with a manifest is refused 403 forbidden on session, presence and the presence socket; three access_denied rows")

	// alice watches presence on the socket from the start.
	aliceWS, status, err := h.openPresenceSocket(ctx, alice)
	if err != nil {
		return fmt.Errorf("alice opening the presence socket: HTTP %d", status)
	}
	l.aliceWS = aliceWS
	if err := aliceWS.waitIDs("alice's presence socket to send a first, empty snapshot", 15*time.Second); err != nil {
		return err
	}

	// Two humans get sessions.
	sa, err := h.session(alice, "bridge")
	if err != nil {
		return err
	}
	if _, err := h.session(bob, "bridge"); err != nil {
		return err
	}
	if g := sa.Rooms[0]; sa.LivekitURL != l.srv.wsURL || !g.CanPublish {
		return fmt.Errorf("alice's session: url %q (want %q), can_publish=%v (want true)", sa.LivekitURL, l.srv.wsURL, g.CanPublish)
	}
	bridgeRoom := sa.Rooms[0].Room
	h.room(bridgeRoom)
	h.say("ok   alice and bob each got a session for bridge (schema-valid; token signed with the API secret, p<id>, 15 s, one room, join+subscribe+publish(microphone) only)")

	// While LiveKit is up the session endpoint must not say it is down:
	// asked again, it answers again.
	if _, err := h.session(bob, "bridge"); err != nil {
		return fmt.Errorf("session endpoint while LiveKit is up: %w", err)
	}

	// A headless participant joins as alice on the token conchd issued.
	hl, err := l.joinHeadless(sa.Rooms[0])
	if err != nil {
		return err
	}
	l.alice, l.bob, l.carol, l.bridgeRoom, l.bridgeSubject, l.aliceHL = alice, bob, carol, bridgeRoom, bridgeSubject, hl
	aliceID := alice.id
	if err := waitForOrFail("LiveKit to list alice in bridge's room", 60*time.Second, func() (bool, string, error) {
		ok, _, saw := l.lkHas(bridgeRoom, aliceID)
		if !ok && hl.exited() {
			return false, "", errors.New("lk exited: " + hl.tail(h))
		}
		return ok, saw, nil
	}); err != nil {
		return err
	}
	if err := waitFor("bob's presence to show alice", 30*time.Second, func() (bool, string) {
		listed, _, saw, err := presenceOf(bob, aliceID)
		if err != nil {
			return false, err.Error()
		}
		return listed, saw
	}); err != nil {
		return err
	}
	want := fmt.Sprintf("%d ", aliceID)
	if err := waitFor("conch voice status (as bob) to list alice", 30*time.Second, func() (bool, string) {
		out, err := bob.cli.run("", "voice", "status", "bridge")
		if err != nil {
			return false, oneLine(out, err)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, want) {
				return true, ""
			}
		}
		return false, strings.TrimSpace(out)
	}); err != nil {
		return err
	}
	// Presence shows exactly who is connected: alice, and nobody who holds
	// a session without having joined (bob) or is not in the channel (carol).
	doc, err := bob.presence("bridge")
	if err != nil {
		return err
	}
	if shown := presenceIDs(doc); !sameIDs(shown, []int64{aliceID}) {
		return fmt.Errorf("presence for bridge lists principals %v, want alice (%d) alone", shown, aliceID)
	}
	if err := aliceWS.waitIDs("alice's presence socket to show her", 15*time.Second, aliceID); err != nil {
		return err
	}
	if err := l.waitAudit("voice_session_issued rows for bridge", store.AuditVoiceSessionIssued, bridgeSubject, "", 3); err != nil {
		return err
	}
	if err := l.waitAudit("alice's voice_joined row", store.AuditVoiceJoined, bridgeSubject, fmt.Sprintf("principal:%d", aliceID), 1); err != nil {
		return err
	}
	h.say("ok   joined: lk joined as alice on conchd's token; LiveKit, presence (REST, socket and conch voice status) agree; audit has voice_session_issued and voice_joined")

	if err := l.transmitting(ctx); err != nil {
		return err
	}
	if err := l.outsider(ctx); err != nil {
		return err
	}
	return l.enforcedByName(ctx)
}

// transmitting: unmute and mute, seen in presence and audited.
func (l *live) transmitting(ctx context.Context) error {
	h, bob, alice := l.h, l.bob, l.alice
	aliceID, bridgeRoom, bridgeSubject := alice.id, l.bridgeRoom, l.bridgeSubject
	// lk publishes its audio track unmuted. Muting and unmuting is done at
	// LiveKit's side, which is all conchd can see of a key press: the muted
	// state of a published microphone track.
	actor := fmt.Sprintf("principal:%d", aliceID)
	var track lkTrack
	if err := waitFor("alice's microphone track in LiveKit", 30*time.Second, func() (bool, string) {
		ok, p, saw := l.lkHas(bridgeRoom, aliceID)
		if !ok {
			return false, saw
		}
		t, has := p.mic()
		track = t
		return has, "alice is in the room but has no microphone track"
	}); err != nil {
		return err
	}
	setMuted := func(muted bool) error {
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := l.adm.mute(mctx, bridgeRoom, identity(aliceID), track.SID, muted); err != nil {
			return err
		}
		return waitFor(fmt.Sprintf("LiveKit to report alice's track muted=%v", muted), 15*time.Second, func() (bool, string) {
			ok, p, saw := l.lkHas(bridgeRoom, aliceID)
			if !ok {
				return false, saw
			}
			t, _ := p.mic()
			return t.Muted == muted, fmt.Sprintf("muted=%v", t.Muted)
		})
	}
	seen := func(talking bool) error {
		return waitFor(fmt.Sprintf("bob's presence to show alice talking=%v", talking), 30*time.Second, func() (bool, string) {
			listed, tk, saw, err := presenceOf(bob, aliceID)
			if err != nil {
				return false, err.Error()
			}
			return listed && tk == talking, saw + fmt.Sprintf(" listed=%v talking=%v", listed, tk)
		})
	}
	// Start from muted so that the unmute is the event under test.
	if err := setMuted(true); err != nil {
		return err
	}
	if err := seen(false); err != nil {
		return err
	}
	started, _ := l.auditCount(store.AuditVoiceTransmitStarted, bridgeSubject, actor)
	stopped, _ := l.auditCount(store.AuditVoiceTransmitStopped, bridgeSubject, actor)
	if err := setMuted(false); err != nil {
		return err
	}
	if err := seen(true); err != nil {
		return err
	}
	if err := l.waitAudit("a voice_transmit_started row after the unmute", store.AuditVoiceTransmitStarted, bridgeSubject, actor, started+1); err != nil {
		return err
	}
	if out, err := bob.cli.run("", "voice", "status", "bridge"); err != nil || !strings.Contains(out, fmt.Sprintf("%d - talking ", aliceID)) {
		return fmt.Errorf("conch voice status while alice is unmuted: exit=%s output=%q, want her line to say talking", exitText(err), strings.TrimSpace(out))
	}
	if err := setMuted(true); err != nil {
		return err
	}
	if err := seen(false); err != nil {
		return err
	}
	if err := l.waitAudit("a voice_transmit_stopped row after the mute", store.AuditVoiceTransmitStopped, bridgeSubject, actor, stopped+1); err != nil {
		return err
	}
	// Leave her transmitting for what follows.
	if err := setMuted(false); err != nil {
		return err
	}
	if err := seen(true); err != nil {
		return err
	}
	h.say("ok   transmitting: unmute shows talking and writes voice_transmit_started; mute shows quiet and writes voice_transmit_stopped")
	return nil
}

// outsider: carol is not in bridge.
func (l *live) outsider(ctx context.Context) error {
	h, bob, carol, bridgeRoom := l.h, l.bob, l.carol, l.bridgeRoom
	if err := expectRefusal("an outsider asking for a session in bridge", carol.api, http.MethodPost, "/v1/channels/bridge/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if err := expectRefusal("anyone asking for a session in a channel that does not exist", carol.api, http.MethodPost, "/v1/channels/no-such-channel/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if err := expectRefusal("an outsider reading bridge's presence", carol.api, http.MethodGet, "/v1/channels/bridge/voice", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if _, status, err := h.openPresenceSocket(ctx, carol); err == nil || status != http.StatusNotFound {
		return fmt.Errorf("an outsider opening bridge's presence socket: HTTP %d (err %s), want 404", status, exitText(err))
	}
	// A token for a different room puts the holder in that room, not this one.
	cs, err := h.session(carol, "side")
	if err != nil {
		return err
	}
	sideRoom := cs.Rooms[0].Room
	h.room(sideRoom)
	if sideRoom == bridgeRoom {
		return errors.New("bridge and side were given the same room")
	}
	sc, status, body, err := dialSignal(ctx, l.srv.wsURL, cs.Rooms[0].Token)
	if err != nil {
		return fmt.Errorf("carol joining side with her own token: HTTP %d %s", status, h.redact(body))
	}
	h.onCleanup(sc.Close)
	if err := waitFor("LiveKit to list carol in side's room", 30*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(sideRoom, carol.id)
		return ok, saw
	}); err != nil {
		return err
	}
	// The conchd poller must have seen her in side before "she is not in
	// bridge's presence" means anything: bridge's presence is read once her
	// own channel's presence lists her.
	if err := waitFor("carol's own presence to show her in side", 30*time.Second, func() (bool, string) {
		listed, _, saw, err := presenceIn(carol, "side", carol.id)
		if err != nil {
			return false, err.Error()
		}
		return listed, saw
	}); err != nil {
		return err
	}
	if gone, saw := l.lkGone(bridgeRoom, carol.id); !gone {
		return fmt.Errorf("carol, whose token is for side, is not known to be out of bridge's room: %s", saw)
	}
	doc, err := bob.presence("bridge")
	if err != nil {
		return err
	}
	if shown := presenceIDs(doc); !sameIDs(shown, []int64{l.alice.id}) {
		return fmt.Errorf("bridge's presence lists %v, want alice (%d) alone: carol holds a token for another room", shown, l.alice.id)
	}
	sc.Close()
	h.say("ok   outsider: refused the unknown-channel answer on session, presence and the presence socket; a token for side puts carol in side (conchd's presence for side lists her), not in bridge")
	return nil
}

// enforcedByName: an identity conchd never issued a token to joins the
// channel's room with a token only the API secret could sign (a leaked
// secret). The poller must notice and remove her within a few passes, never
// show her, and say so in the audit log.
func (l *live) enforcedByName(ctx context.Context) error {
	h, bob, carol, bridgeRoom := l.h, l.bob, l.carol, l.bridgeRoom
	tok, err := l.adm.mintJoin(identity(carol.id), bridgeRoom)
	if err != nil {
		return err
	}
	h.secret("a join token minted by this program", tok)
	removedBefore, err := l.auditCount(store.AuditVoiceParticipantRemoved, "", "")
	if err != nil {
		return err
	}
	sc, status, body, err := dialSignal(ctx, l.srv.wsURL, tok)
	if err != nil {
		return fmt.Errorf("carol joining bridge with a token minted from the API secret: HTTP %d %s", status, h.redact(body))
	}
	h.onCleanup(sc.Close)
	needle := fmt.Sprintf("principal=%d reason=not_member", carol.id)
	if err := waitFor("conchd to remove carol, who was never issued a token, from bridge's room", 20*time.Second, func() (bool, string) {
		// She must never be shown, in any pass, while she is there.
		if listed, _, saw, err := presenceOf(bob, carol.id); err == nil && listed {
			return false, "bob's presence lists carol: " + saw
		}
		gone, sawLK := l.lkGone(bridgeRoom, carol.id)
		rows, err := l.d.auditRows(store.AuditVoiceParticipantRemoved)
		if err != nil {
			return false, err.Error()
		}
		n := 0
		for _, e := range rows {
			if e.Actor == "system" && strings.Contains(e.Detail, needle) {
				n++
			}
		}
		return gone && sc.closed() && n == 1 && len(rows) == removedBefore+1,
			fmt.Sprintf("LiveKit: %s; her connection closed=%v; %d removal rows for her", sawLK, sc.closed(), n)
	}); err != nil {
		return err
	}
	// She was never shown, and nobody else was disturbed.
	if listed, _, saw, err := presenceOf(bob, carol.id); err != nil || listed {
		return fmt.Errorf("bob's presence lists carol after her removal (%s, err %s)", saw, exitText(err))
	}
	if ok, _, saw := l.lkHas(bridgeRoom, l.alice.id); !ok {
		return fmt.Errorf("alice was disconnected when carol was removed (%s)", saw)
	}
	h.say("ok   by name: carol, whom conchd never issued a token, joined bridge on a token minted from the API secret; she was removed within a few passes, never shown, and one voice_participant_removed (reason=not_member) was audited")
	return nil
}

// presenceIn is presenceOf for any channel.
func presenceIn(viewer *person, channel string, id int64) (listed, talking bool, saw string, err error) {
	doc, err := viewer.presence(channel)
	if err != nil {
		return false, false, "", err
	}
	ids := presenceIDs(doc)
	for _, p := range doc.Rooms {
		for _, q := range p.Participants {
			if q.PrincipalID == id {
				listed, talking = true, q.Transmitting
			}
		}
	}
	return listed, talking, fmt.Sprintf("available=%v principals=%v", doc.Available, ids), nil
}
