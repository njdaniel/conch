package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// The entry gates (design §2, §9.1, §9.2): each step changes the joining
// policy as the operator, then a newcomer of its own (an address of its own,
// through the trusted proxy) tries each entry point, and the front door says
// what the policy now offers. The store reads the policy again in the
// transaction that starts a visit or makes an account, so the try that
// follows a change is refused even though the front door read before it said
// yes: that is the stale page of §9.1. The steps run in order; each says the
// settings it changes, and the last puts the plaza back as the cohort found
// it.

// tryEntry is one attempt at an entry point.
type tryEntry struct {
	what string
	// do makes the attempt from from and returns the answer.
	do func(w *world, from api) (int, []byte, error)
	// want is the refusal; the zero answer wants a success. A visit that
	// starts is ended at once by its own credential, so that it holds no
	// place under the caps (inspect may look at it first).
	want    answer
	inspect func(w *world, g *member, body []byte) error
	// from is who tries, when it is not a newcomer of its own.
	from func(w *world) api
}

// gate is one step: the change, what the front door must then offer, and
// the tries.
type gate struct {
	what string
	set  func(w *world) schema.PutJoinSettingsRequestV1
	// invitesPaused, when set, pauses or resumes invite redemption too.
	invitesPaused *bool
	front         schema.JoinOptionsV1
	tries         []tryEntry
	// check runs after the tries.
	check func(w *world, set schema.JoinSettingsResponseV1) error
}

func settings(r schema.PutJoinSettingsRequestV1) func(*world) schema.PutJoinSettingsRequestV1 {
	return func(*world) schema.PutJoinSettingsRequestV1 { return r }
}

func tryGuest(want answer) tryEntry {
	return tryEntry{what: "joins as a guest at the bare address", want: want, do: func(w *world, from api) (int, []byte, error) {
		return w.entry(from, http.MethodPost, "/v1/join/guest", schema.JoinGuestRequestV1{Label: "Lee"})
	}}
}

func tryInviteGuest(mode schema.InviteJoinMode, want answer) tryEntry {
	return tryEntry{what: "joins as a guest through the " + string(mode) + " invite", want: want, do: func(w *world, from api) (int, []byte, error) {
		return w.entry(from, http.MethodPost, "/v1/invites/redeem-guest", schema.RedeemInviteGuestRequestV1{Code: w.p.invites[mode].code, Label: "Lee"})
	}}
}

// tryPersonalGuest has the operator make a personal invite to the plaza for
// a guest, and redeems it as one: §9.1 names personal invites beside open
// links.
func tryPersonalGuest(want answer) tryEntry {
	return tryEntry{what: "joins as a guest through a personal invite for a guest", want: want, do: func(w *world, from api) (int, []byte, error) {
		inv, err := w.invite(w.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{w.p.channel}, JoinAs: schema.InviteJoinGuest})
		if err != nil {
			return 0, nil, err
		}
		return w.entry(from, http.MethodPost, "/v1/invites/redeem-guest", schema.RedeemInviteGuestRequestV1{Code: inv.Code, Label: "Lee"})
	}}
}

func tryAccount(want answer) tryEntry {
	return tryEntry{what: "creates an account at the bare address", want: want, do: func(w *world, from api) (int, []byte, error) {
		return w.entry(from, http.MethodPost, "/v1/join/account", schema.JoinAccountRequestV1{Name: w.pubName(fmt.Sprintf("joiner-%d", w.addrs)), Password: joinPassword})
	}}
}

func tryInviteAccount(mode schema.InviteJoinMode, want answer) tryEntry {
	return tryEntry{what: "creates an account through the " + string(mode) + " invite", want: want, do: func(w *world, from api) (int, []byte, error) {
		return w.entry(from, http.MethodPost, "/v2/invites/redeem", schema.RedeemInviteRequestV2{Code: w.p.invites[mode].code,
			Name: w.pubName(fmt.Sprintf("joiner-%d", w.addrs)), Password: joinPassword})
	}}
}

// tryPreview previews the invite of mode and requires that it offers join.
func tryPreview(mode schema.InviteJoinMode, join schema.JoinOptionsV1) tryEntry {
	return tryEntry{what: "previews the " + string(mode) + " invite",
		do: func(w *world, from api) (int, []byte, error) {
			return from.do(http.MethodPost, "/v1/invites/preview", schema.InvitePreviewRequestV1{Code: w.p.invites[mode].code})
		},
		inspect: func(_ *world, _ *member, body []byte) error {
			var pv schema.InvitePreviewV1
			if err := json.Unmarshal(body, &pv); err != nil {
				return err
			}
			if pv.Join == nil || *pv.Join != join {
				return fmt.Errorf("the preview offers %+v, want %+v", pv.Join, join)
			}
			return nil
		}}
}

var (
	guestsDisabled = answer{http.StatusForbidden, schema.ErrorCodeGuestsDisabled}
	joiningClosed  = answer{http.StatusForbidden, schema.ErrorCodeJoiningClosed}
	joiningPaused  = answer{http.StatusForbidden, schema.ErrorCodeJoiningPaused}
	invitesPaused  = answer{http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused}
	guestsFull     = answer{http.StatusTooManyRequests, schema.ErrorCodeGuestsFull}
)

// gates is the steps, in order.
func gates() []gate {
	yes, no := true, false
	return []gate{
		{what: "the address open to guests and accounts", set: settings(schema.PutJoinSettingsRequestV1{GuestsEnabled: &yes, OpenJoining: ptr(schema.OpenJoiningBoth)}),
			front: schema.JoinOptionsV1{Guest: true, Account: true},
			tries: []tryEntry{tryPreview(schema.InviteJoinBoth, schema.JoinOptionsV1{Guest: true, Account: true})}},
		// The front door said yes a moment ago; the page that read it is stale.
		{what: "guests turned off, accounts still open", set: settings(schema.PutJoinSettingsRequestV1{GuestsEnabled: &no}),
			front: schema.JoinOptionsV1{Account: true},
			tries: []tryEntry{tryGuest(guestsDisabled), tryInviteGuest(schema.InviteJoinBoth, guestsDisabled), tryPersonalGuest(guestsDisabled),
				tryPreview(schema.InviteJoinBoth, schema.JoinOptionsV1{Account: true}), tryAccount(answer{}), tryInviteAccount(schema.InviteJoinBoth, answer{})},
			check: guestsStay},
		{what: "the address guest-only", set: settings(schema.PutJoinSettingsRequestV1{GuestsEnabled: &yes, OpenJoining: ptr(schema.OpenJoiningGuest)}),
			front: schema.JoinOptionsV1{Guest: true},
			tries: []tryEntry{tryAccount(joiningClosed), tryGuest(answer{})},
			check: func(w *world, _ schema.JoinSettingsResponseV1) error { return w.conversionNeedsPermit() }},
		{what: "the address closed, invites open", set: settings(schema.PutJoinSettingsRequestV1{OpenJoining: ptr(schema.OpenJoiningOff)}),
			front: schema.JoinOptionsV1{},
			tries: []tryEntry{tryGuest(guestsDisabled), tryAccount(joiningClosed), tryInviteGuest(schema.InviteJoinGuest, answer{})}},
		{what: "each invite's mode", set: settings(schema.PutJoinSettingsRequestV1{OpenJoining: ptr(schema.OpenJoiningBoth)}),
			front: schema.JoinOptionsV1{Guest: true, Account: true},
			tries: []tryEntry{tryInviteAccount(schema.InviteJoinGuest, joiningClosed), tryInviteGuest(schema.InviteJoinAccount, guestsDisabled),
				tryPreview(schema.InviteJoinGuest, schema.JoinOptionsV1{Guest: true}), tryPreview(schema.InviteJoinAccount, schema.JoinOptionsV1{Account: true}),
				tryPreview(schema.InviteJoinBoth, schema.JoinOptionsV1{Guest: true, Account: true}),
				// With guests on, a personal invite for a guest admits one: so
				// its refusal while guests are off is the switch's doing.
				tryPersonalGuest(answer{})}},
		// Pausing joining pauses the bare address and the conversion of its
		// guests, not invites.
		{what: "joining paused", set: settings(schema.PutJoinSettingsRequestV1{JoiningPaused: &yes}),
			front: schema.JoinOptionsV1{Paused: true},
			tries: []tryEntry{tryGuest(joiningPaused), tryAccount(joiningPaused), tryInviteGuest(schema.InviteJoinBoth, answer{})},
			check: func(w *world, _ schema.JoinSettingsResponseV1) error {
				return w.convertRefused(w.p.sam, joiningPaused, "while joining is paused")
			}},
		{what: "invites paused, joining resumed", set: settings(schema.PutJoinSettingsRequestV1{JoiningPaused: &no}), invitesPaused: &yes,
			front: schema.JoinOptionsV1{Guest: true, Account: true},
			tries: []tryEntry{tryInviteGuest(schema.InviteJoinBoth, invitesPaused), tryInviteAccount(schema.InviteJoinBoth, invitesPaused), tryGuest(answer{})},
			check: func(w *world, _ schema.JoinSettingsResponseV1) error {
				return w.convertRefused(w.p.ola, invitesPaused, "of a guest who came by invite, while invites are paused")
			}},
		// The cap is every live visit, from the address and through invites.
		// It is set below the cohort's four, so the expiring visits running
		// out meanwhile cannot open a place.
		{what: "the cap reached", invitesPaused: &no, set: settings(schema.PutJoinSettingsRequestV1{MaxGuests: ptr(int64(1))}),
			front: schema.JoinOptionsV1{Account: true, GuestsFull: true},
			tries: []tryEntry{tryGuest(guestsFull), tryInviteGuest(schema.InviteJoinBoth, guestsFull)}},
		// The cap per address: Nick's address holds his visit.
		{what: "one visit per address", set: settings(schema.PutJoinSettingsRequestV1{MaxGuests: ptr(int64(schema.DefaultMaxGuests)), MaxGuestsPerAddress: ptr(int64(1))}),
			front: schema.JoinOptionsV1{Guest: true, Account: true},
			tries: []tryEntry{
				{what: "joins as a guest from the address of a live visit", want: guestsFull, from: func(w *world) api { return w.newcomerAt(w.p.nick.addr) },
					do: tryGuest(answer{}).do},
				{what: "reads the front door from the address of a live visit", from: func(w *world) api { return w.newcomerAt(w.p.nick.addr) },
					do: func(_ *world, from api) (int, []byte, error) { return from.do(http.MethodGet, "/v1/join", nil) },
					inspect: func(_ *world, _ *member, body []byte) error {
						var info schema.JoinInfoV1
						if err := json.Unmarshal(body, &info); err != nil {
							return err
						}
						if info.Join.Guest || !info.Join.GuestsFull {
							return fmt.Errorf("the front door at a full address offers %+v", info.Join)
						}
						return nil
					}},
				tryGuest(answer{})}},
		// A closed invite admits nobody, as a guest or otherwise.
		{what: "a closed invite", set: settings(schema.PutJoinSettingsRequestV1{MaxGuestsPerAddress: ptr(int64(schema.DefaultMaxGuestsPerAddress))}),
			front: schema.JoinOptionsV1{Guest: true, Account: true},
			tries: []tryEntry{{what: "joins as a guest through an invite the operator closed", want: answer{http.StatusNotFound, schema.ErrorCodeInviteInvalid},
				do: func(w *world, from api) (int, []byte, error) {
					closed, err := w.invite(w.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{w.p.channel}, JoinAs: schema.InviteJoinBoth})
					if err != nil {
						return 0, nil, err
					}
					if err := w.op.call(http.MethodDelete, fmt.Sprintf("/v1/invites/%d", closed.Invite.ID), nil, nil); err != nil {
						return 0, nil, err
					}
					return w.entry(from, http.MethodPost, "/v1/invites/redeem-guest", schema.RedeemInviteGuestRequestV1{Code: closed.Code, Label: "Lee"})
				}}}},
		// Voice is the address's to give: off, a joiner holds none.
		{what: "voice off for joiners", set: settings(schema.PutJoinSettingsRequestV1{JoinVoice: &no}),
			front: schema.JoinOptionsV1{Guest: true, Account: true},
			tries: []tryEntry{{what: "joins as a guest and asks for voice", do: tryGuest(answer{}).do, inspect: noVoice}}},
	}
}

// newcomerAt is a newcomer at addr.
func (w *world) newcomerAt(addr string) api {
	return api{h: w.h, base: w.d.baseURL, who: "a newcomer at " + addr, addr: addr}
}

// runGates runs every step and puts the plaza back as the cohort found it.
// It returns how many tries it made.
func (w *world) runGates() (int, error) {
	n := 0
	for _, g := range gates() {
		resp, err := w.joinSettings(g.set(w))
		if err != nil {
			return n, fmt.Errorf("%s: %w", g.what, err)
		}
		if g.invitesPaused != nil {
			if err := w.op.call(http.MethodPut, "/v1/invites/settings", schema.InviteSettingsV1{Paused: *g.invitesPaused}, nil); err != nil {
				return n, err
			}
		}
		var info schema.JoinInfoV1
		if err := w.newcomer("a newcomer").call(http.MethodGet, "/v1/join", nil, &info); err != nil {
			return n, err
		}
		if info.Join != g.front {
			return n, fmt.Errorf("%s: the front door offers %+v, want %+v", g.what, info.Join, g.front)
		}
		for _, t := range g.tries {
			from := w.newcomer("a newcomer")
			if t.from != nil {
				from = t.from(w)
			}
			n++
			status, body, err := t.do(w, from)
			if err != nil {
				return n, fmt.Errorf("%s: a newcomer %s: %w", g.what, t.what, err)
			}
			if err := w.settleTry(t, from, status, body); err != nil {
				return n, fmt.Errorf("%s: a newcomer %s: %w", g.what, t.what, err)
			}
		}
		if g.check != nil {
			if err := g.check(w, resp); err != nil {
				return n, fmt.Errorf("%s: %w", g.what, err)
			}
		}
	}
	return n, w.openPlaza()
}

// settleTry judges one try's answer, and ends a visit it started.
func (w *world) settleTry(t tryEntry, from api, status int, body []byte) error {
	if leaks := w.leaks(body, asked{}); len(leaks) > 0 {
		return fmt.Errorf("status %d, and the body holds %s", status, strings.Join(leaks, "; "))
	}
	if t.want != (answer{}) {
		return checkRefusal(result{status: status, body: body}, t.want, asked{})
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("status %d %s, want a success", status, w.h.redact(strings.TrimSpace(string(body))))
	}
	var g *member
	if status == http.StatusCreated && bytes.Contains(body, []byte(`"guest":`)) {
		var resp schema.JoinGuestResponseV1
		if err := json.Unmarshal(body, &resp); err != nil {
			return err
		}
		w.h.secret("a newcomer's credential", resp.Token)
		var err error
		if g, err = w.guestJoined(resp, from.addr); err != nil {
			return err
		}
	} else if tok := tokenOf(body); tok != "" {
		w.h.secret("a newcomer's credential", tok)
	}
	if t.inspect != nil {
		if err := t.inspect(w, g, body); err != nil {
			return err
		}
	}
	if g != nil {
		var ended schema.EndVisitResponseV1
		if err := g.api.call(http.MethodPost, "/v1/guest/end", nil, &ended); err != nil {
			return err
		}
	}
	return nil
}

// tokenOf is the token field of an answer, "" when it has none.
func tokenOf(body []byte) string {
	var t struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &t)
	return t.Token
}

// guestsStay: turning guests off ends no visit (design §2). Nick's sockets
// stay open and his credential works, and the operator is told how many
// visits remain.
func guestsStay(w *world, set schema.JoinSettingsResponseV1) error {
	p := w.p
	if set.LiveGuests < 1 {
		return fmt.Errorf("turning guests off said %d live visits, with Nick's live", set.LiveGuests)
	}
	warned := false
	for _, wn := range set.Warnings {
		warned = warned || wn == schema.JoinWarningLiveGuestsRemain
	}
	if !warned {
		return fmt.Errorf("turning guests off with visits live warned %v, not %s", set.Warnings, schema.JoinWarningLiveGuestsRemain)
	}
	if status, _, err := p.nick.api.do(http.MethodGet, "/v1/whoami", nil); err != nil {
		return err
	} else if status != http.StatusOK {
		return fmt.Errorf("after guests were turned off, Nick's credential answers %d, want 200", status)
	}
	for _, f := range p.nickHeld.sockets {
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s closed when guests were turned off", f.what)
		}
	}
	return nil
}

// noVoice: a guest who joined while voice was off for joiners holds none.
func noVoice(w *world, g *member, _ []byte) error {
	var who schema.WhoAmIResponseV1
	if err := g.api.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return err
	}
	if who.VoiceAllowed == nil || *who.VoiceAllowed {
		return fmt.Errorf("a guest who joined with voice off is told voice_allowed %v", who.VoiceAllowed)
	}
	status, body, err := g.api.do(http.MethodPost, "/v1/channels/"+pch(w)+"/voice/session", nil)
	if err != nil {
		return err
	}
	// The caller's voice is looked at before whether the server has any,
	// so the answer is the same with LiveKit and without.
	return checkRefusal(result{status: status, body: body}, answer{http.StatusForbidden, schema.ErrorCodeVoiceNotAllowed}, asked{strs: []string{w.p.channel}})
}
