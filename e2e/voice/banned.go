package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// ---- scenario: banning a person from a channel (issue #851,
// docs/design/moderation.md §6)
//
// A person is in bridge's room. The operator bans her: LiveKit disconnects
// her, she cannot get a session again (on her credential or another), the
// ban list names her, an add is refused, and the audit has the ban and the
// removal. Lifting the ban restores nothing: she still cannot join, until an
// operator adds her back by hand.

func (l *live) bannedFromChannel(ctx context.Context) error {
	h, d := l.h, l.d
	jo, err := h.newPerson(d, "jo", "bridge")
	if err != nil {
		return err
	}
	watcher, err := h.newPerson(d, "kim", "bridge")
	if err != nil {
		return err
	}
	s, err := h.session(jo, "bridge")
	if err != nil {
		return err
	}
	room := s.Rooms[0].Room
	h.room(room)
	conn, status, body, err := lkit.DialSignal(ctx, l.srv.WSURL, s.Rooms[0].Token)
	if err != nil {
		return fmt.Errorf("jo joining with conchd's token: HTTP %d %s", status, h.redact(body))
	}
	defer func() { conn.Close() }()
	if err := waitFor("LiveKit to list jo", 30*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(room, jo.id)
		return ok, saw
	}); err != nil {
		return err
	}
	ban := fmt.Sprintf("/v1/channels/bridge/bans/%d", jo.id)

	// The gate: a plain member has no standing, and nothing happened.
	if err := expectRefusal("a plain member banning jo", watcher.api, http.MethodPut, ban, http.StatusForbidden, "forbidden"); err != nil {
		return err
	}
	if ok, _, saw := l.lkHas(room, jo.id); !ok {
		return fmt.Errorf("a refused ban removed jo (%s)", saw)
	}

	if err := d.operator.call(http.MethodPut, ban, nil, nil); err != nil {
		return fmt.Errorf("ban jo: %w", err)
	}
	// The ban is a membership removal for the live parts: her LiveKit
	// connection ends and she is no longer listed.
	if err := waitFor("jo to be disconnected from bridge's room", 30*time.Second, func() (bool, string) {
		gone, saw := l.lkGone(room, jo.id)
		return gone && conn.Closed(), fmt.Sprintf("%s; her connection closed=%v", saw, conn.Closed())
	}); err != nil {
		return err
	}
	if err := waitFor("presence to list nobody", 30*time.Second, func() (bool, string) {
		doc, err := watcher.presence("bridge")
		if err != nil {
			return false, err.Error()
		}
		ids := presenceIDs(doc)
		return len(ids) == 0, fmt.Sprintf("lists %v", ids)
	}); err != nil {
		return err
	}

	// She cannot come back: a session, the channel, on this credential and on
	// another.
	if err := expectRefusal("jo asking for a voice session after the ban", jo.api, http.MethodPost, "/v1/channels/bridge/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if err := expectRefusal("jo reading the channel after the ban", jo.api, http.MethodGet, "/v2/channels/bridge/messages", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	var cred schema.CreateCredentialResponseV1
	if err := d.operator.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", jo.id), schema.CreateCredentialRequestV1{Label: "jo-alternate"}, &cred); err != nil {
		return fmt.Errorf("a second credential for jo: %w", err)
	}
	h.secret("jo's alternate conch credential", cred.Token)
	if err := expectRefusal("jo's other credential asking for a voice session", d.as(cred.Token), http.MethodPost, "/v1/channels/bridge/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	// Re-adding is refused while the ban stands.
	if err := expectRefusal("the operator re-adding banned jo", d.operator, http.MethodPut, fmt.Sprintf("/v1/channels/bridge/members/%d", jo.id), http.StatusNotFound, "principal_not_found"); err != nil {
		return err
	}
	// The list names her; the audit has the ban and the removal, ids only.
	var list schema.ListChannelBansResponseV1
	if err := d.operator.call(http.MethodGet, "/v1/channels/bridge/bans", nil, &list); err != nil {
		return fmt.Errorf("list bans: %w", err)
	}
	if err := list.Validate(); err != nil {
		return fmt.Errorf("the ban list is invalid: %w", err)
	}
	if len(list.Bans) != 1 || list.Bans[0].PrincipalID != jo.id || list.Bans[0].Name != jo.name {
		return fmt.Errorf("the ban list = %+v, want jo alone", list)
	}
	added, err := d.auditRows(store.AuditChannelBanAdded)
	if err != nil {
		return err
	}
	n := 0
	for _, e := range added {
		if e.Subject == l.bridgeSubject && e.Detail == fmt.Sprintf("principal=%d", jo.id) && e.Actor != "system" {
			n++
		}
		if strings.Contains(e.Detail+e.Subject+e.Actor, jo.name) {
			return fmt.Errorf("a ban audit row carries a name: %+v", e)
		}
	}
	if n != 1 {
		return fmt.Errorf("%d channel_ban_added rows for jo, want 1 (rows: %+v)", n, added)
	}

	// Lifting the ban restores nothing.
	if err := d.operator.call(http.MethodDelete, ban, nil, nil); err != nil {
		return fmt.Errorf("lift jo's ban: %w", err)
	}
	if err := expectRefusal("jo after the ban was lifted", jo.api, http.MethodPost, "/v1/channels/bridge/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	// Only an explicit add admits her, and then she joins voice again.
	if err := d.operator.call(http.MethodPut, fmt.Sprintf("/v1/channels/bridge/members/%d", jo.id), nil, nil); err != nil {
		return fmt.Errorf("add jo back: %w", err)
	}
	again, err := h.session(jo, "bridge")
	if err != nil {
		return fmt.Errorf("jo's session after being added back: %w", err)
	}
	h.room(again.Rooms[0].Room)
	back, status, body, err := lkit.DialSignal(ctx, l.srv.WSURL, again.Rooms[0].Token)
	if err != nil {
		return fmt.Errorf("jo rejoining: HTTP %d %s", status, h.redact(body))
	}
	defer back.Close()
	h.say("ok   banned from a channel: LiveKit disconnected jo, she cannot get a session or read the channel on any credential, an add was refused while the ban stood, the list and audit named her by id, unbanning restored nothing, and an explicit add let her rejoin")
	return nil
}
