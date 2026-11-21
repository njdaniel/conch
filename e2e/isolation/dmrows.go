package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/njdaniel/conch/pkg/schema"
)

// Local direct messages, the first routes (issue #2019, epic #2013, ADR-017):
// open or resume, the caller's inbox, one conversation, and the caller's own
// state of it. Every route is in selfMadeCredentialRoutes, so every row has
// two prober outcomes. Bea joined through an invite and made her own
// credential: her own rows succeed. Ola was made by hand and her credential
// came through POST /v1/principals/{id}/credentials, which the store records
// as made by another: every request is refused to her with 403
// own_sign_in_required, reads included (withoutPassword, as for the person
// stream). That refusal is the end-to-end proof that an operator's act-as
// credential cannot open a person's DMs, and it stays an expectation of the
// rows, not a property to loosen.
//
// B's prober opens a conversation with B's own other member (bob), never an
// A person, so a success holds nothing of A's. A's one conversation is ann's
// empty open with abe (world.go): B names its id, and the answer must be the
// one a conversation that never existed gets (404 dm_not_found, the ghost
// comparison), whatever the prober's role. B's inbox never holds it.

// dmNotFound is the refusal for a conversation id the caller is not part of.
// bobOf is the B member the prober opens a conversation with: the first of
// B's people who is not the prober. The route check's stand-in world has one
// person, who is then the stand-in's peer too (no request is sent).
func bobOf(w *world) int64 {
	for _, p := range w.b.people {
		if p.id != w.prober.id {
			return p.id
		}
	}
	return w.prober.id
}

// dmMatrixRows are the rows of the DM routes.
func dmMatrixRows() []row {
	return []row{
		{route: "POST /v1/dms", what: "opens a conversation with a member of her own group", own: true, withoutPassword: personNotOwn, inspect: checkDMOpen,
			probe: func(w *world, _ subject) probe { return post("/v1/dms", schema.OpenDMRequestV1{PeerID: bobOf(w)}) }},
		{route: "GET /v1/dms", what: "lists her own inbox", own: true, withoutPassword: personNotOwn, inspect: checkDMInbox,
			probe: func(*world, subject) probe { return get("/v1/dms") }},
		{route: "GET /v1/dms/{conversation}", what: "reads A's conversation by id", pick: aConversation, want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(_ *world, s subject) probe { return get("/v1/dms/" + id(s.id)) }},
		{route: "PUT /v1/dms/{conversation}/state", what: "hides A's conversation by id", pick: aConversation, want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(_ *world, s subject) probe {
				return put("/v1/dms/"+id(s.id)+"/state", schema.SetDMStateRequestV1{State: schema.DMStateHidden})
			}},
	}
}

// checkDMOpen is the open row's own check: the answer is a valid response, the
// conversation is the prober's with bob, and it says nothing of when or by
// whom it was opened (no created_at, no message).
func checkDMOpen(w *world, _ api, body []byte) error {
	var resp schema.OpenDMResponseV1
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("the open's answer is not a conversation: %w", err)
	}
	if err := resp.Validate(); err != nil {
		return err
	}
	c := resp.Conversation
	if c.PeerID != bobOf(w) || !slices.Contains(c.ParticipantIDs, w.prober.id) || c.ID == w.a.dmID {
		return fmt.Errorf("the open answered conversation %d of %v, want the prober's with %d", c.ID, c.ParticipantIDs, bobOf(w))
	}
	if c.CreatedAt != nil || c.LastMessageID != 0 || c.State != schema.DMStateOpen {
		return fmt.Errorf("an empty open carries a trace: %+v", c)
	}
	return nil
}

// checkDMInbox is the inbox row's own check: a valid page, every conversation
// the prober's own, and none of A's.
func checkDMInbox(w *world, _ api, body []byte) error {
	var page schema.ListDMConversationsResponseV1
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&page); err != nil {
		return fmt.Errorf("the inbox is not a list of conversations: %w", err)
	}
	if err := page.Validate(); err != nil {
		return err
	}
	for _, c := range page.Conversations {
		if c.ID == w.a.dmID || !slices.Contains(c.ParticipantIDs, w.prober.id) {
			return fmt.Errorf("the inbox holds conversation %d of %v", c.ID, c.ParticipantIDs)
		}
		for _, p := range w.a.people {
			if slices.Contains(c.ParticipantIDs, p.id) {
				return fmt.Errorf("the inbox holds a conversation with A's %s", p.name)
			}
		}
	}
	return nil
}
