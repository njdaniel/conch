package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// ---- scenario: removing a person from voice only (issue #850,
// docs/design/moderation.md §4)
//
// Two people are in bridge's room. The operator removes one: LiveKit
// disconnects only her, the room is neither rotated nor deleted, the other
// stays connected and shown, her presence socket closes, she stays a member
// and keeps reading the channel, a plain member cannot do it, and she joins
// again at once, on the same credential and on another.

func (l *live) kickedFromVoice(ctx context.Context) error {
	h, d := l.h, l.d
	eve, err := h.newPerson(d, "eve", "bridge")
	if err != nil {
		return err
	}
	finn, err := h.newPerson(d, "finn", "bridge")
	if err != nil {
		return err
	}
	join := func(p *person) (*lkit.SignalConn, string, error) {
		s, err := h.session(p, "bridge")
		if err != nil {
			return nil, "", err
		}
		h.room(s.Rooms[0].Room)
		conn, status, body, err := lkit.DialSignal(ctx, l.srv.WSURL, s.Rooms[0].Token)
		if err != nil {
			return nil, "", fmt.Errorf("%s joining with conchd's token: HTTP %d %s", p.name, status, h.redact(body))
		}
		return conn, s.Rooms[0].Room, nil
	}
	eveConn, room, err := join(eve)
	if err != nil {
		return err
	}
	defer func() { eveConn.Close() }()
	finnConn, finnRoom, err := join(finn)
	if err != nil {
		return err
	}
	defer finnConn.Close()
	if room != finnRoom {
		return fmt.Errorf("eve and finn were given different rooms")
	}
	for _, p := range []*person{eve, finn} {
		if err := waitFor("LiveKit to list "+p.name, 30*time.Second, func() (bool, string) {
			ok, _, saw := l.lkHas(room, p.id)
			return ok, saw
		}); err != nil {
			return err
		}
		if err := waitFor("presence to list "+p.name, 30*time.Second, func() (bool, string) {
			listed, _, saw, err := presenceOf(finn, p.id)
			if err != nil {
				return false, err.Error()
			}
			return listed, saw
		}); err != nil {
			return err
		}
	}
	eveWS, status, err := h.openPresenceSocket(ctx, eve)
	if err != nil {
		return fmt.Errorf("eve opening the presence socket: HTTP %d", status)
	}
	defer eveWS.Close()
	finnWS, status, err := h.openPresenceSocket(ctx, finn)
	if err != nil {
		return fmt.Errorf("finn opening the presence socket: HTTP %d", status)
	}
	defer finnWS.Close()
	if err := eveWS.waitIDs("eve's presence socket to list eve and finn", 30*time.Second, eve.id, finn.id); err != nil {
		return err
	}
	before, err := l.rotations()
	if err != nil {
		return err
	}
	kick := fmt.Sprintf("/v1/channels/bridge/voice/participants/%d", eve.id)

	// Control and the gate: a plain member has no standing, and nothing
	// happened to anyone.
	if err := expectRefusal("a plain member removing eve from voice", finn.api, http.MethodDelete, kick, http.StatusForbidden, "forbidden"); err != nil {
		return err
	}
	if ok, _, saw := l.lkHas(room, eve.id); !ok {
		return fmt.Errorf("a refused removal removed eve (%s)", saw)
	}

	if err := d.operator.call(http.MethodDelete, kick, nil, nil); err != nil {
		return fmt.Errorf("remove eve from voice: %w", err)
	}

	// The answer came after the work: LiveKit has disconnected her, and no one
	// else.
	if err := waitFor("eve to be disconnected from bridge's room", 20*time.Second, func() (bool, string) {
		gone, saw := l.lkGone(room, eve.id)
		return gone && eveConn.Closed(), fmt.Sprintf("%s; her connection closed=%v", saw, eveConn.Closed())
	}); err != nil {
		return err
	}
	if ok, _, saw := l.lkHas(room, finn.id); !ok || finnConn.Closed() {
		return fmt.Errorf("finn was disconnected by eve's removal (in room=%v, closed=%v, %s)", ok, finnConn.Closed(), saw)
	}
	if rooms, err := l.lkRooms(); err != nil || !contains(rooms, room) {
		return fmt.Errorf("bridge's room is gone from LiveKit after a voice-only removal (%s)", exitText(err))
	}
	if n, err := l.rotations(); err != nil || n != before {
		return fmt.Errorf("a voice-only removal rotated a room: %d rotations, was %d (%s)", n, before, exitText(err))
	}
	// Presence: she is gone from every surface and he is not; her presence
	// socket was closed by the removal, his was not.
	if err := waitFor("presence to list exactly finn", 30*time.Second, func() (bool, string) {
		doc, err := finn.presence("bridge")
		if err != nil {
			return false, err.Error()
		}
		ids := presenceIDs(doc)
		return sameIDs(ids, []int64{finn.id}), fmt.Sprintf("lists %v", ids)
	}); err != nil {
		return err
	}
	if err := waitFor("eve's presence socket to be closed by the removal", 20*time.Second, func() (bool, string) {
		ended, _ := eveWS.closeStatus()
		return ended, "still open"
	}); err != nil {
		return err
	}
	// Closed 1008 with the reason word itself (schema.SocketCloseVoiceRemoved,
	// issue #1409): removed from voice, not from the channel. A client keys on
	// the value, and does not rejoin by itself.
	if _, st := eveWS.closeStatus(); st != websocket.StatusPolicyViolation {
		return fmt.Errorf("eve's presence socket was closed with status %d, want %d", st, websocket.StatusPolicyViolation)
	}
	if got := eveWS.closeReason(); got != string(schema.SocketCloseVoiceRemoved) {
		return fmt.Errorf("eve's presence socket was closed with reason %q, want %q", got, schema.SocketCloseVoiceRemoved)
	}
	if ended, _ := finnWS.closeStatus(); ended {
		return fmt.Errorf("finn's presence socket was closed by eve's removal")
	}
	// Chat access is unchanged: she is still a member and still reads the
	// channel.
	for _, path := range []string{"/v1/channels/bridge/members", "/v2/channels/bridge/messages"} {
		var out any
		if err := eve.api.call(http.MethodGet, path, nil, &out); err != nil {
			return fmt.Errorf("eve after a voice-only removal: %w", err)
		}
	}

	// Audit: one request row by the operator, ids and result only, and the
	// disconnection's own row with its reason.
	want := fmt.Sprintf("principal=%d result=completed", eve.id)
	rows, err := d.auditRows(store.AuditVoiceParticipantKicked)
	if err != nil {
		return err
	}
	n := 0
	for _, e := range rows {
		if e.Subject == l.bridgeSubject && e.Detail == want && e.Actor != "system" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%d voice_participant_kicked rows with detail %q, want 1 (rows: %+v)", n, want, rows)
	}
	removed, err := d.auditRows(store.AuditVoiceParticipantRemoved)
	if err != nil {
		return err
	}
	seen := false
	for _, e := range removed {
		if strings.Contains(e.Detail, fmt.Sprintf("principal=%d reason=moderator_kick", eve.id)) {
			seen = true
		}
	}
	if !seen {
		return fmt.Errorf("no voice_participant_removed row with reason=moderator_kick for eve")
	}

	// Idempotent: removing her again, now absent, is a 204 and one more
	// request row.
	if err := d.operator.call(http.MethodDelete, kick, nil, nil); err != nil {
		return fmt.Errorf("remove absent eve from voice: %w", err)
	}

	// Rejoin at once: the same credential, then another one.
	eveConn.Close()
	again, _, err := join(eve)
	if err != nil {
		return fmt.Errorf("rejoin on the same credential: %w", err)
	}
	defer func() { again.Close() }()
	if err := waitFor("eve to be listed in LiveKit after rejoining", 30*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(room, eve.id)
		return ok, saw
	}); err != nil {
		return err
	}
	if err := d.operator.call(http.MethodDelete, kick, nil, nil); err != nil {
		return err
	}
	if err := waitFor("eve to be removed again", 20*time.Second, func() (bool, string) {
		gone, saw := l.lkGone(room, eve.id)
		return gone && again.Closed(), saw
	}); err != nil {
		return err
	}
	var cred schema.CreateCredentialResponseV1
	if err := d.operator.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", eve.id), schema.CreateCredentialRequestV1{Label: "eve-alternate"}, &cred); err != nil {
		return fmt.Errorf("a second credential for eve: %w", err)
	}
	h.secret("eve's alternate conch credential", cred.Token)
	alt := *eve
	alt.token, alt.credID, alt.api = cred.Token, cred.Credential.ID, d.as(cred.Token)
	altConn, _, err := join(&alt)
	if err != nil {
		return fmt.Errorf("rejoin on another credential: %w", err)
	}
	defer altConn.Close()
	if err := waitFor("eve to be listed in LiveKit on her other credential", 30*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(room, eve.id)
		return ok, saw
	}); err != nil {
		return err
	}
	h.say("ok   removed from voice only: LiveKit disconnected eve and nobody else, the room was neither rotated nor deleted, finn stayed connected and shown, eve's presence socket closed, she stayed a member and kept reading; a plain member was refused; voice_participant_kicked and moderator_kick rows; she rejoined on the same credential and on another")
	return nil
}
