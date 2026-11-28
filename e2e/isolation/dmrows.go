package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/njdaniel/conch/pkg/schema"
)

// Local direct messages (issues #2019, #2021 and #2022, ADR-017). Bea uses her own
// credential and can open and list her conversations. Ola uses one issued by
// the operator and gets 403 own_sign_in_required on every DM route.
//
// Bea names Ann and Abe's conversation and gets the same 404 dm_not_found
// as for a conversation that never existed. When she uses their message ID
// in her own conversation with Bob, the refusal also matches the answer
// for a message that never existed.

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
	rows := []row{
		{route: "POST /v1/dms", what: "opens a conversation with a member of her own group", own: true, withoutPassword: personNotOwn, inspect: checkDMOpen,
			probe: func(w *world, _ subject) probe { return post("/v1/dms", schema.OpenDMRequestV1{PeerID: bobOf(w)}) }},
		{route: "GET /v1/dms", what: "lists her own inbox", own: true, withoutPassword: personNotOwn, inspect: checkDMInbox,
			probe: func(*world, subject) probe { return get("/v1/dms") }},
		{route: "GET /v1/dms/snapshot", what: "recovers her own inbox and unread counts", own: true, withoutPassword: personNotOwn, inspect: checkDMSnapshot,
			probe: func(*world, subject) probe { return get("/v1/dms/snapshot") }},
		{route: "GET /v1/dms/{conversation}", what: "reads A's conversation by id", pick: aConversation, want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(_ *world, s subject) probe { return get("/v1/dms/" + id(s.id)) }},
		{route: "GET /v1/dms/{conversation}/changes", what: "reads message changes in A's conversation", pick: aConversation, want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(_ *world, s subject) probe { return get("/v1/dms/" + id(s.id) + "/changes") }},
		{route: "PUT /v1/dms/{conversation}/read-state", what: "marks A's conversation as read", pick: aConversation, want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(w *world, s subject) probe {
				return put("/v1/dms/"+id(s.id)+"/read-state", schema.MarkReadRequestV1{LastReadID: w.a.dmMessageID})
			}},
		{route: "PUT /v1/dms/{conversation}/state", what: "hides A's conversation by id", pick: aConversation, want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(_ *world, s subject) probe {
				return put("/v1/dms/"+id(s.id)+"/state", schema.SetDMStateRequestV1{State: schema.DMStateHidden})
			}},
	}

	for _, r := range dmMessageRoutes {
		rows = append(rows, row{route: r.route, what: r.what, pick: aConversation, want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(w *world, s subject) probe { return r.probe(dmMessagesPath(s.id), w.a.dmMessageID) }})
		if r.item {
			rows = append(rows, row{route: r.route, what: "targets A's message in B's conversation", pick: aDMMessage, want: messageNotFound, withoutPassword: personNotOwn,
				probe: func(w *world, s subject) probe { return r.probe(dmMessagesPath(w.b.dmID), s.id) }})
		}
	}
	for _, cursor := range []struct{ name, query string }{{"after", "after="}, {"before", "order=desc&before="}} {
		rows = append(rows, row{route: "GET /v1/dms/{conversation}/messages", what: "pages B's conversation using A's message as " + cursor.name,
			pick: aDMMessage, want: answer{http.StatusBadRequest, "invalid_request"}, withoutPassword: personNotOwn,
			probe: func(w *world, s subject) probe { return get(dmMessagesPath(w.b.dmID) + "?" + cursor.query + id(s.id)) }})
	}
	return rows
}

var dmMessageRoutes = []struct {
	route string
	what  string
	item  bool // the path also addresses a message
	probe func(path string, message int64) probe
}{
	{route: "GET /v1/dms/{conversation}/messages", what: "reads A's conversation history",
		probe: func(path string, _ int64) probe { return get(path) }},
	{route: "POST /v1/dms/{conversation}/messages", what: "sends a message to A's conversation",
		probe: func(path string, _ int64) probe {
			return post(path, schema.PostDMMessageRequestV1{Body: "a message from another session", ClientNonce: "isolation-probe"})
		}},
	{route: "PATCH /v1/dms/{conversation}/messages/{message}", what: "edits a message in A's conversation", item: true,
		probe: func(path string, message int64) probe {
			return probe{method: http.MethodPatch, path: path + "/" + id(message), body: schema.EditDMMessageRequestV1{Body: "an edit from another session"}}
		}},
	{route: "DELETE /v1/dms/{conversation}/messages/{message}", what: "removes a message in A's conversation", item: true,
		probe: func(path string, message int64) probe { return del(path + "/" + id(message)) }},
}

func dmMessagesPath(conversation int64) string {
	return "/v1/dms/" + id(conversation) + "/messages"
}

func aDMMessage(w *world) subject {
	return subject{kind: "dm_message", id: w.a.dmMessageID}
}

func (w *world) seedDMMessage(ann *member) error {
	body := w.aName("word-dm")
	var sent schema.PostDMMessageResponseV1
	if err := ann.api.call(http.MethodPost, dmMessagesPath(w.a.dmID), schema.PostDMMessageRequestV1{Body: body, ClientNonce: "isolation-fixture"}, &sent); err != nil {
		return fmt.Errorf("ann sends a direct message to abe: %w", err)
	}
	if err := sent.Validate(); err != nil {
		return fmt.Errorf("ann's direct message: %w", err)
	}
	w.a.dmMessageID = sent.Message.ID
	w.marks.id("dm_message", "A's direct message", w.a.dmMessageID)
	w.marks.str("A's direct message", body)
	return nil
}

// checkDMMessageCredentials tries the same routes as Ann, the author of the
// fixture message. An act-as credential and a revoked self-issued session
// must each be refused even though the account is still a participant.
func (w *world) checkDMMessageCredentials(ann, actAs *member) error {
	prober := w.prober
	defer func() { w.prober = prober }()

	check := func(m *member, want answer) error {
		rows := make([]row, 0, len(dmMessageRoutes))
		for _, r := range dmMessageRoutes {
			rows = append(rows, row{route: r.route, what: r.what, want: want,
				probe: func(w *world, _ subject) probe { return r.probe(dmMessagesPath(w.a.dmID), w.a.dmMessageID) }})
		}
		_, err := w.runMatrix(m, rows)
		return err
	}
	if err := check(actAs, personNotOwn); err != nil {
		return fmt.Errorf("using Ann's act-as credential: %w", err)
	}
	session, err := w.signIn(w.newcomer("Ann's DM sign-in"), ann.name, ann.id, joinPassword)
	if err != nil {
		return err
	}
	w.registerMember(session)
	var history schema.ListDMMessagesResponseV1
	if err := session.api.call(http.MethodGet, dmMessagesPath(w.a.dmID), nil, &history); err != nil {
		return err
	}
	if len(history.Messages) != 1 || history.Messages[0].ID != w.a.dmMessageID || history.Messages[0].Body != w.aName("word-dm") {
		return fmt.Errorf("the live sign-in of Ann did not read the fixture message in her conversation")
	}
	if err := w.op.call(http.MethodDelete, "/v1/credentials/"+id(session.credID), nil, nil); err != nil {
		return err
	}
	if err := check(session, answer{http.StatusUnauthorized, "unauthenticated"}); err != nil {
		return fmt.Errorf("using Ann's revoked sign-in: %w", err)
	}
	return nil
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
	return checkDMConversations(w, page.Conversations)
}

func checkDMSnapshot(w *world, _ api, body []byte) error {
	var snapshot schema.DMSnapshotV1
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snapshot); err != nil {
		return fmt.Errorf("the snapshot is not a DM snapshot: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	return checkDMConversations(w, snapshot.Conversations)
}

func checkDMConversations(w *world, rows []schema.DMConversationV1) error {
	for _, c := range rows {
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
