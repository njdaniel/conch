package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// Guests and moderation (issue #853, moderation.md §6.4 and §9 cases 6 and
// 9): in the plaza, a guest's visit is a principal, and a ban or a message
// removal is about that principal and that history bound alone.
//
//   - A message removed before a visit began is no part of the visit: no
//     tombstone in its history, no frame when a later removal of an older
//     message happens, nothing of its text.
//   - A removal inside the bound reaches the guest as the tombstone, in each
//     socket family's own envelope.
//   - A ban takes the guest out of the plaza at once and keeps it out: the
//     routes, the sockets, an add by an operator. Its credential and visit
//     stay valid: ending the visit is a separate lever.
//   - The next visit from the same address under the same label is a new
//     principal with a new handle and a bound of its own, which the old ban
//     does not match; it reads nothing from before it, and a removal of
//     anything from before it sends it nothing.

func (m *mod) guests() error {
	w := m.w
	p := w.p
	plaza, ch := p.channel, url.PathEscape(p.channel)
	say := func(who *member, body string) (int64, error) {
		var resp schema.PostMessageResponseV2
		if err := who.api.call(http.MethodPost, "/v2/channels/"+ch+"/messages", schema.PostMessageRequestV2{Body: body}, &resp); err != nil {
			return 0, err
		}
		return resp.Message.ID, nil
	}
	removeAsOp := func(id int64) error {
		return w.op.call(http.MethodDelete, "/v2/channels/"+ch+"/messages/"+itoa(id), nil, nil)
	}
	read := func(a api) ([]schema.MessageV2, []byte, error) {
		status, body, err := a.do(http.MethodGet, "/v2/channels/"+ch+"/messages?after=0&limit=100", nil)
		if err != nil {
			return nil, nil, err
		}
		if status != http.StatusOK {
			return nil, body, fmt.Errorf("%s reads the plaza: %d %s", a.who, status, strings.TrimSpace(string(body)))
		}
		var page schema.ListMessagesResponseV2
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, body, err
		}
		return page.Messages, body, nil
	}
	pre1, pre2, pre3 := m.text("plaza: removed before the visit"), m.text("plaza: removed during the visit, from before it"), m.text("plaza: from before, never removed")
	in1, in2, ctl := m.text("plaza: removed inside the bound"), m.text("plaza: said before the next visit"), m.text("plaza: control")
	var preIDs [3]int64
	var err error
	for i, t := range []string{pre1, pre2, pre3} {
		if preIDs[i], err = say(p.pia, t); err != nil {
			return err
		}
	}
	// A removal before any visit of this scenario: nothing but the plaza's
	// own members will ever see its tombstone.
	if err := removeAsOp(preIDs[0]); err != nil {
		return fmt.Errorf("the operator removing a plaza message: %w", err)
	}

	// ---- the first visit

	g1, err := w.joinGuest("Mod", nil)
	if err != nil {
		return err
	}
	feeds, err := openAll(g1, plaza)
	if err != nil {
		return err
	}
	in1ID, err := say(p.pia, in1)
	if err != nil {
		return err
	}
	in2ID, err := say(p.pia, in2)
	if err != nil {
		return err
	}
	ctlID, err := say(p.pia, ctl)
	if err != nil {
		return err
	}
	for _, f := range feeds {
		if k := kindOf(f); k == kindMessageV0 || k == kindMessageV2 {
			if err := waitFor(f.what+" to carry the control", modWait, func() (bool, string) {
				for _, fr := range f.received() {
					if bytes.Contains(fr, []byte(ctl)) {
						return true, ""
					}
				}
				return false, fmt.Sprintf("%d frames", len(f.received()))
			}); err != nil {
				return err
			}
		}
	}
	// A file shared inside the visit's bound, which the guest can read until
	// it is banned (§6.2: the ban closes the file path too).
	f1, err := w.upload(p.pia, m.text("plaza: shared file")+".txt", m.text("plaza: shared file contents"))
	if err != nil {
		return err
	}
	if _, err := w.ppost(p.pia, false, "", attachment(m.text("plaza: shared file message"), f1)); err != nil {
		return err
	}
	if status, _, err := g1.api.do(http.MethodGet, fmt.Sprintf("/v2/files/%d", f1), nil); err != nil || status != http.StatusOK {
		return fmt.Errorf("the guest reading a file shared inside its visit before the ban: %d %s", status, errText(err))
	}
	msgs, body, err := read(g1.api)
	if err != nil {
		return err
	}
	for _, old := range []string{pre1, pre2, pre3, schema.MessageRemovedBody} {
		if bytes.Contains(body, []byte(old)) {
			return fmt.Errorf("the guest's history holds %q, from before its visit (or a tombstone of it)", old)
		}
	}
	for _, msg := range msgs {
		if msg.ID <= preIDs[2] {
			return fmt.Errorf("the guest's history holds message %d, from before its visit began", msg.ID)
		}
	}
	// A removal of an older message while the guest's sockets are open: the
	// guest is sent nothing, in any family.
	base := feedCounts(feeds)
	if err := removeAsOp(preIDs[1]); err != nil {
		return err
	}
	if err := m.checkRemoval("a message from before the visit", feeds, base, preIDs[1], func(*feed) want { return wantNothing }, pre1, pre2, pre3); err != nil {
		return err
	}
	for i, f := range feeds {
		for j, fr := range f.received() {
			if bytes.Contains(fr, []byte(pre2)) || bytes.Contains(fr, []byte(schema.MessageRemovedBody)) && decodeFrame(fr).names(preIDs[1]) {
				return fmt.Errorf("frame %d of socket %d (%s) is about a message from before the visit", j, i, f.what)
			}
		}
	}
	// A removal inside the bound: the tombstone, in each family's envelope.
	base = feedCounts(feeds)
	if err := removeAsOp(in1ID); err != nil {
		return err
	}
	if err := m.checkRemoval("a message inside the visit's bound", feeds, base, in1ID, func(f *feed) want { return wideWant(f) }, in1); err != nil {
		return err
	}
	msgs, body, err = read(g1.api)
	if err != nil {
		return err
	}
	if bytes.Contains(body, []byte(in1)) {
		return fmt.Errorf("the guest's history holds the text of a message removed inside its bound")
	}
	found := false
	for _, msg := range msgs {
		if msg.ID == in1ID {
			found = msg.Removed && msg.Body == schema.MessageRemovedBody
		}
	}
	if !found {
		return fmt.Errorf("the guest's history has no tombstone for the message removed inside its bound")
	}
	if res, _, err := m.searchIn(plaza, g1.api, in1, 10, 0); err != nil || len(res.Messages) != 0 {
		return fmt.Errorf("the guest found a removed message by search: %+v %v", res, errText(err))
	}
	// A guest has no standing to remove anything.
	if got, err := g1.api.do2(http.MethodDelete, "/v2/channels/"+ch+"/messages/"+itoa(in2ID)); err != nil {
		return err
	} else if err := checkRefusal(got, operatorOnly, asked{ids: []kindID{{"message", in2ID}}}); err != nil {
		return fmt.Errorf("a guest removing a message: %w", err)
	}

	// ---- the ban

	ban := fmt.Sprintf("/v1/channels/%s/bans/%d", ch, g1.id)
	t, err := w.measure("the operator bans the guest from the plaza", []holder{{m: g1, sockets: feeds}}, m.bounds, func() error {
		return w.op.call(http.MethodPut, ban, nil, nil)
	})
	if err != nil {
		return err
	}
	if err := closedRemoved(feeds); err != nil {
		return err
	}
	var who schema.WhoAmIResponseV1
	if err := g1.api.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return fmt.Errorf("the banned guest's credential: %w", err)
	}
	if who.Guest == nil || who.Guest.State != schema.GuestStateActive || who.Guest.Handle != g1.guest.Handle {
		return fmt.Errorf("the banned guest's visit after the ban: %+v, want it active and unchanged", who.Guest)
	}
	m.skip = map[string]bool{"POST /v2/channels/{channel}/files": true, "POST /v1/channels/{channel}/leave": true}
	n, err := m.outIn(plaza, g1.api, "the banned guest", in2ID, f1, in2)
	m.skip = nil
	if err != nil {
		return err
	}
	for _, v := range socketVariants {
		if v.path == "/v1/voice/ws" {
			continue
		}
		f, r, err := dialSocket(g1.api, "the banned guest's "+v.path, v.path, url.Values{"channel": {plaza}}, v.ticket)
		if err != nil {
			return err
		}
		if f != nil {
			f.close()
			return fmt.Errorf("the banned guest opened %s on the plaza", v.path)
		}
		if err := checkRefusal(result(r), channelNotFound, asked{strs: []string{plaza}}); err != nil {
			return fmt.Errorf("the banned guest on %s: %w", v.path, err)
		}
	}
	for _, c := range []struct {
		what string
		p    probe
	}{
		{"an operator's add by id", put(fmt.Sprintf("/v1/channels/%s/members/%d", ch, g1.id), nil)},
		{"an operator's add by name", post("/v1/channels/"+ch+"/members", schema.AddChannelMemberRequestV1{ID: g1.id})},
	} {
		got, err := w.send(w.op, c.p)
		if err != nil {
			return err
		}
		if err := checkRefusal(got, answer{http.StatusNotFound, "principal_not_found"}, asked{ids: []kindID{{"principal", g1.id}}}); err != nil {
			return fmt.Errorf("%s re-admitting the banned guest: %w", c.what, err)
		}
	}
	var bans schema.ListChannelBansResponseV1
	if err := w.op.call(http.MethodGet, "/v1/channels/"+ch+"/bans", nil, &bans); err != nil {
		return err
	}
	if len(bans.Bans) != 1 || bans.Bans[0].PrincipalID != g1.id || bans.Bans[0].Name != g1.guest.Handle {
		return fmt.Errorf("the plaza's ban list is %+v, want the guest's handle alone", bans)
	}

	// ---- the next visit: someone new

	from := w.newcomerAt(g1.addr)
	g2, err := w.joinGuest("Mod", &from)
	if err != nil {
		return err
	}
	if g2.id == g1.id || g2.guest.Handle == g1.guest.Handle || g2.guest.Tag == g1.guest.Tag {
		return fmt.Errorf("a new visit from the banned guest's address and under its label is %d %q %q, the same as %d %q %q",
			g2.id, g2.guest.Handle, g2.guest.Tag, g1.id, g1.guest.Handle, g1.guest.Tag)
	}
	feeds2, err := openAll(g2, plaza)
	if err != nil {
		return fmt.Errorf("the new visit was refused the plaza, which the old ban does not cover: %w", err)
	}
	msgs, body, err = read(g2.api)
	if err != nil {
		return err
	}
	for _, old := range []string{pre1, pre2, pre3, in1, in2, ctl, schema.MessageRemovedBody} {
		if bytes.Contains(body, []byte(old)) {
			return fmt.Errorf("the new visit's history holds %q, from before it began (or a tombstone of it)", old)
		}
	}
	for _, msg := range msgs {
		if msg.ID <= ctlID {
			return fmt.Errorf("the new visit's history holds message %d, from before it began", msg.ID)
		}
	}
	for _, id := range []int64{in1ID, preIDs[0], preIDs[1]} {
		got, err := g2.api.do2(http.MethodGet, "/v2/channels/"+ch+"/messages?thread="+itoa(id))
		if err != nil {
			return err
		}
		gh, err := g2.api.do2(http.MethodGet, "/v2/channels/"+ch+"/messages?thread="+itoa(w.ghost().id))
		if err != nil {
			return err
		}
		if got.status != http.StatusNotFound || gh.status != got.status || !bytes.Equal(scrub(gh.body, nil, w.ghost().id), scrub(got.body, nil, id)) {
			return fmt.Errorf("the new visit asks for message %d from before it as a thread: %d %s; for one nobody issued: %d %s", id, got.status, got.body, gh.status, gh.body)
		}
	}
	replay, r, err := dialSocket(g2.api, "the new visit's replaying /v2/ws", "/v2/ws", url.Values{"channel": {plaza}, "after": {"0"}}, false)
	if err != nil || replay == nil {
		return fmt.Errorf("the new visit's replaying socket: %s %d %s", errText(err), r.status, r.body)
	}
	feeds2 = append(feeds2, replay)
	// Something said inside the new bound, and the control.
	in3 := m.text("plaza: inside the new visit")
	in3ID, err := say(p.pia, in3)
	if err != nil {
		return err
	}
	for _, f := range feeds2 {
		if k := kindOf(f); k == kindMessageV0 || k == kindMessageV2 {
			if err := waitFor(f.what+" to carry what was said", modWait, func() (bool, string) {
				for _, fr := range f.received() {
					if bytes.Contains(fr, []byte(in3)) {
						return true, ""
					}
				}
				return false, fmt.Sprintf("%d frames", len(f.received()))
			}); err != nil {
				return err
			}
		}
	}
	// Removals of everything from before the new visit send it nothing.
	base = feedCounts(feeds2)
	for _, id := range []int64{in2ID, preIDs[2], ctlID} {
		if err := removeAsOp(id); err != nil {
			return err
		}
	}
	for _, id := range []int64{in2ID, preIDs[2], ctlID} {
		if err := m.checkRemoval("a message from before the new visit", feeds2, base, id, func(*feed) want { return wantNothing }, pre1, pre2, pre3, in1, in2, ctl); err != nil {
			return err
		}
	}
	// And one inside it reaches it.
	base = feedCounts(feeds2)
	if err := removeAsOp(in3ID); err != nil {
		return err
	}
	owed := func(f *feed) want {
		if f == replay {
			return wantEnvelope
		}
		return wideWant(f)
	}
	if err := m.checkRemoval("a message inside the new visit's bound", feeds2, base, in3ID, owed, in3); err != nil {
		return err
	}
	// The old ban matches the old principal alone.
	bans = schema.ListChannelBansResponseV1{}
	if err := w.op.call(http.MethodGet, "/v1/channels/"+ch+"/bans", nil, &bans); err != nil {
		return err
	}
	if len(bans.Bans) != 1 || bans.Bans[0].PrincipalID != g1.id {
		return fmt.Errorf("the plaza's ban list after the new visit is %+v, want the first visit's alone", bans)
	}

	// ---- conversion keeps the ban (§6.3)

	g3, err := w.joinGuest("Mod", nil)
	if err != nil {
		return err
	}
	ban3 := fmt.Sprintf("/v1/channels/%s/bans/%d", ch, g3.id)
	if err := w.op.call(http.MethodPut, ban3, nil, nil); err != nil {
		return err
	}
	var permit schema.CreateSignInLinkResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/guests/%d/conversion-permit", g3.id), schema.CreateConversionPermitRequestV1{}, &permit); err != nil {
		return fmt.Errorf("a conversion permit for the banned guest: %w", err)
	}
	w.h.secret("a conversion permit", permit.Code)
	name3 := w.pubName("converted-mod")
	var conv schema.ConvertGuestResponseV1
	if err := g3.api.call(http.MethodPost, "/v1/guest/convert", schema.ConvertGuestRequestV1{Name: name3, Password: convertPassword, Permit: permit.Code}, &conv); err != nil {
		return fmt.Errorf("the banned guest converting: %w", err)
	}
	member3, err := w.converted(g3, name3, conv)
	if err != nil {
		return err
	}
	if _, err := m.outIn(plaza, member3.api, "the converted member, banned as a guest", in2ID, f1, in2); err != nil {
		return fmt.Errorf("after the conversion: %w", err)
	}
	bans = schema.ListChannelBansResponseV1{}
	if err := w.op.call(http.MethodGet, "/v1/channels/"+ch+"/bans", nil, &bans); err != nil {
		return err
	}
	if !slices.ContainsFunc(bans.Bans, func(b schema.ChannelBanV1) bool { return b.PrincipalID == g3.id && b.Name == name3 }) {
		return fmt.Errorf("the plaza's ban list after the conversion is %+v, want the same principal's ban under its new name", bans)
	}
	if err := w.op.call(http.MethodDelete, ban3, nil, nil); err != nil {
		return err
	}
	if status, _, err := member3.api.do(http.MethodGet, "/v2/channels/"+ch+"/messages", nil); err != nil || status != http.StatusNotFound {
		return fmt.Errorf("the converted member after the unban: %d %s, want 404: nothing is restored", status, errText(err))
	}

	// ---- clean up: the ban lifted (restoring nothing) and both visits ended
	// by the operator, a lever of its own.

	if err := w.op.call(http.MethodDelete, ban, nil, nil); err != nil {
		return err
	}
	if status, _, err := g1.api.do(http.MethodGet, "/v2/channels/"+ch+"/messages", nil); err != nil || status != http.StatusNotFound {
		return fmt.Errorf("the guest after the unban: %d %v, want 404: an unban restores nothing", status, errText(err))
	}
	for _, f := range feeds2 {
		f.close()
	}
	for _, g := range []*member{g1, g2} {
		var ended schema.EndGuestResponseV1
		if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/guests/%d/end", g.id), nil, &ended); err != nil {
			return err
		}
	}
	m.say("guests: a guest of the plaza (its history from its bound) saw nothing of a message removed before its visit and was sent no frame when an older message was removed, was sent the tombstone of one removed inside its bound in each socket family's envelope, was refused the removal route, and was banned by the operator: its sockets closed (%s), its visit and credential stayed good, every route and socket variant refused it as for a ghost (%d requests) and an add by an operator was refused; the next visit from its address under its label was a new principal (%q, not %q) that the ban did not match, read nothing from before it and was sent nothing when anything from before it was removed, and was sent the tombstone of a message inside its bound (also on a socket replaying from the start); a banned guest that converted to an account kept its ban under its new name and gained nothing; lifting the ban restored nothing and the operator ended both visits", t, n, g2.guest.Handle, g1.guest.Handle)
	return nil
}
