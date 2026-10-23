package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// Ending visits, timed (design §3.4, §4.2, §9.4): each guest holds every
// socket variant, the occupancy socket, a ticket and, with LiveKit, a
// participant in the plaza's room, and is ended by one of the three levers:
// End visit (the guest itself), the operator ending one visit, and End all
// guest visits through an invite. Each is held to the bounds the groups'
// revocations are (revoke.go): two seconds for a socket, one voice poller
// pass plus a room rotation for LiveKit, from the moment the request is
// sent; then the ended stay out. A visit's end rotates the room it was in,
// which takes everyone out of it, so each guest joins the plaza's room
// afresh before its turn.

// holdInVoice is hold, and with LiveKit the participant too.
func (w *world) holdInVoice(m *member) (holder, error) {
	h, err := w.hold(m, w.p.channel, "")
	if err != nil {
		return h, err
	}
	if w.lk != nil {
		if err := w.joinVoice(m, h.room); err != nil {
			return h, err
		}
	}
	return h, nil
}

// revokeGuests ends the cohort's three visits and returns what it measured.
// After End visit, the first guest's posts stay hers, and a new visit from
// her address under her label is someone new, with a bound of its own.
func (w *world) revokeGuests(bounds voiceBounds, nickPost int64) ([]timing, error) {
	p := w.p
	var out []timing

	// 1. End visit. Nick holds what he held from the start too.
	fresh, err := w.holdInVoice(p.nick)
	if err != nil {
		return nil, err
	}
	nick := fresh
	nick.sockets = append(append([]*feed{}, p.nickHeld.sockets...), fresh.sockets...)
	var ended schema.EndVisitResponseV1
	t, err := w.measure("a guest ends her own visit (End visit)", []holder{nick}, bounds, func() error {
		return p.nick.api.call(http.MethodPost, "/v1/guest/end", nil, &ended)
	})
	if err != nil {
		return nil, err
	}
	if ended.Guest.State != schema.GuestStateEnded || ended.Guest.EndReason != schema.GuestEndVisit || ended.Guest.Handle != p.nick.guest.Handle {
		return nil, fmt.Errorf("the End visit answered %+v", ended.Guest)
	}
	if err := closedAllRevoked(nick.sockets); err != nil {
		return nil, err
	}
	if err := w.staysOut([]holder{nick}, bounds); err != nil {
		return nil, fmt.Errorf("after End visit: %w", err)
	}
	out = append(out, t)
	if err := w.checkAttribution(p.pia.api, nickPost, *p.nick.guest); err != nil {
		return nil, fmt.Errorf("after End visit, Nick's post: %w", err)
	}
	// Someone of another group reads no part of the plaza: the plaza is to
	// bea a channel that does not exist.
	bea := w.b.people[0]
	ps, pb, err := bea.api.do(http.MethodGet, "/v2/channels/"+pch(w)+"/messages?thread="+id(nickPost), nil)
	if err != nil {
		return nil, err
	}
	g := w.ghost()
	gs0, gb0, err := bea.api.do(http.MethodGet, "/v2/channels/"+g.name+"/messages?thread="+id(nickPost), nil)
	if err != nil {
		return nil, err
	}
	if err := checkRefusal(result{status: ps, body: pb}, channelNotFound, asked{strs: []string{w.p.channel}, ids: []kindID{{"message", nickPost}}}); err != nil {
		return nil, fmt.Errorf("bea reads Nick's post: %w", err)
	}
	if gs0 != ps || !bytes.Equal(bytes.ReplaceAll(pb, []byte(w.p.channel), []byte("<asked>")), bytes.ReplaceAll(gb0, []byte(g.name), []byte("<asked>"))) {
		return nil, fmt.Errorf("bea is told of the plaza %d %s, of a channel that never existed %d %s", ps, pb, gs0, gb0)
	}

	// A new visit from the same address, under the same label: a new
	// principal, a new handle, and a bound after everything said to or by
	// the first.
	from := w.newcomerAt(p.nick.addr)
	again, err := w.joinGuest("Nick", &from)
	if err != nil {
		return nil, err
	}
	if again.id == p.nick.id || again.guest.Handle == p.nick.guest.Handle {
		return nil, fmt.Errorf("a new visit from Nick's address is principal %d %q, the same as the visit that ended", again.id, again.guest.Handle)
	}
	status, body, err := again.api.do(http.MethodGet, "/v2/channels/"+pch(w)+"/messages?after=0&limit=100", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the new visit reads the plaza: status %d", status)
	}
	if leaks := w.leaks(body, asked{}); len(leaks) > 0 {
		return nil, fmt.Errorf("the new visit reads the plaza, and it holds %s", strings.Join(leaks, "; "))
	}
	for _, old := range []string{p.whisperToNick, w.pubName("from-nick"), p.control} {
		if strings.Contains(string(body), old) {
			return nil, fmt.Errorf("the new visit from Nick's address reads %q, from before it began", old)
		}
	}
	ts, tb, err := again.api.do(http.MethodGet, "/v2/channels/"+pch(w)+"/messages?thread="+id(nickPost), nil)
	if err != nil {
		return nil, err
	}
	gs, gb, err := again.api.do(http.MethodGet, "/v2/channels/"+pch(w)+"/messages?thread="+id(w.ghost().id), nil)
	if err != nil {
		return nil, err
	}
	if ts != http.StatusNotFound || gs != ts || !bytes.Equal(normalizeID(tb, nickPost), normalizeID(gb, w.ghost().id)) {
		return nil, fmt.Errorf("the new visit asks for the first visit's post as a thread: %d %s; for one that never existed: %d %s", ts, tb, gs, gb)
	}

	// 2. The operator ends a visit.
	h2, err := w.holdInVoice(p.nick2)
	if err != nil {
		return nil, err
	}
	var byOp schema.EndGuestResponseV1
	t, err = w.measure("the operator ends a visit", []holder{h2}, bounds, func() error {
		return w.op.call(http.MethodPost, fmt.Sprintf("/v1/guests/%d/end", p.nick2.id), nil, &byOp)
	})
	if err != nil {
		return nil, err
	}
	if byOp.Guest.State != schema.GuestStateEnded || byOp.Guest.EndReason != schema.GuestEndOperator {
		return nil, fmt.Errorf("the operator's end answered %+v", byOp.Guest)
	}
	if err := closedAllRevoked(h2.sockets); err != nil {
		return nil, err
	}
	if err := w.staysOut([]holder{h2}, bounds); err != nil {
		return nil, fmt.Errorf("after the operator ended a visit: %w", err)
	}
	out = append(out, t)

	// 3. End all guest visits through the guest-only invite: the dry run
	// names Ola alone, and the end ends her.
	h3, err := w.holdInVoice(p.ola)
	if err != nil {
		return nil, err
	}
	inviteID := p.invites[schema.InviteJoinGuest].id
	var dry schema.EndGuestsResponseV1
	if err := w.op.call(http.MethodPost, "/v1/guests/end", schema.EndGuestsRequestV1{DryRun: ptr(true), InviteID: inviteID}, &dry); err != nil {
		return nil, err
	}
	if !dry.DryRun || dry.Count != 1 || len(dry.Guests) != 1 || dry.Guests[0].ID != p.ola.id || dry.Remaining != 0 {
		return nil, fmt.Errorf("the dry run of ending the invite's visits names %+v, want Ola alone", dry)
	}
	var all schema.EndGuestsResponseV1
	t, err = w.measure("End all guest visits through an invite", []holder{h3}, bounds, func() error {
		return w.op.call(http.MethodPost, "/v1/guests/end", schema.EndGuestsRequestV1{DryRun: ptr(false), InviteID: inviteID}, &all)
	})
	if err != nil {
		return nil, err
	}
	if all.DryRun || all.Count != 1 || all.Guests[0].ID != p.ola.id {
		return nil, fmt.Errorf("ending the invite's visits answered %+v", all)
	}
	if err := closedAllRevoked(h3.sockets); err != nil {
		return nil, err
	}
	if err := w.staysOut([]holder{h3}, bounds); err != nil {
		return nil, fmt.Errorf("after End all guest visits: %w", err)
	}
	out = append(out, t)
	if err := again.api.call(http.MethodPost, "/v1/guest/end", nil, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeID sets aside the id a request named.
func normalizeID(body []byte, id int64) []byte { return normalize(body, subject{id: id}) }

// checkMCPClosed: a guest's credential does not open the agents' surface
// (design §4.3: MCP is agents only).
func (w *world) checkMCPClosed(g *member) error {
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"isolation-check","version":"0"}}}`)
	status, body, err := g.api.send(http.MethodPost, "/mcp", "application/json", initialize)
	if err != nil {
		return err
	}
	// The MCP endpoint has one answer for every credential it does not take
	// (mcp_auth.go): 401, the same as for a token nobody was issued.
	ghost := g.api
	ghost.token = schema.CredentialTokenPrefix + strings.Repeat("Q", 43)
	gs, gb, err := ghost.send(http.MethodPost, "/mcp", "application/json", initialize)
	if err != nil {
		return err
	}
	if status != http.StatusUnauthorized || gs != status || !bytes.Equal(body, gb) {
		return fmt.Errorf("a guest's credential on /mcp: %d %s; a token nobody was issued: %d %s; want the one 401", status, w.h.redact(string(body)), gs, gb)
	}
	if leaks := w.leaks(body, asked{}); len(leaks) > 0 {
		return fmt.Errorf("a guest's refusal on /mcp holds %s", strings.Join(leaks, "; "))
	}
	return nil
}
