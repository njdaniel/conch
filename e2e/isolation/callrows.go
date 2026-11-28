package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// The private call routes (issue #2030, ADR-017). A call attempt belongs to
// the two people of a direct-message conversation and to nobody else, and
// every route here answers about the caller's own attempts alone:
//
//   - a conversation id or a call id the caller has no part in, and one that
//     was never issued, are the same 404 (dm_not_found, call_not_found), with
//     one fixed body; a call id never says whether the attempt exists;
//   - the snapshot of the caller's current call is the caller's own: with no
//     call of hers, it is empty;
//   - every route is in selfMadeCredentialRoutes, so the operator-made
//     prober (ola: her credential was made on her behalf, which is what
//     act-as is) is refused 403 own_sign_in_required before anything is
//     looked up. That refusal is the end-to-end proof that an act-as
//     credential does not reach a person's calls; it stays an expectation of
//     the row.
//
// The media row names A's accepted attempt and compares its refusal with the
// answer for a never-issued id. The other lookup rows use never-issued ids.

var (
	dmNotFound   = answer{http.StatusNotFound, schema.ErrorCodeDMNotFound}
	callNotFound = answer{http.StatusNotFound, schema.ErrorCodeCallNotFound}
)

// neverIssuedConversation and neverIssuedCall are ids nobody was given: a
// conversation id past any in use and a well-formed call id of the right
// shape (schema.MinPrivateCallIDBytes characters of base64url).
const (
	neverIssuedConversation = 987654321
	neverIssuedCall         = "never-issued-call-0123"
)

func callsOfConversation() string {
	return "/v1/dms/" + strconv.Itoa(neverIssuedConversation) + "/calls"
}

func (w *world) acceptCallA() error {
	ann, abe := w.a.people[0], w.a.people[1]
	var invite schema.PrivateCallResponseV1
	path := fmt.Sprintf("/v1/dms/%d/calls", w.a.dmID)
	if err := ann.api.call(http.MethodPost, path, schema.InvitePrivateCallRequestV1{ActionID: "isolation-invite"}, &invite); err != nil {
		return err
	}
	var accepted schema.PrivateCallResponseV1
	path = "/v1/calls/" + invite.Call.ID + "/accept"
	action := schema.PrivateCallActionRequestV1{ActionID: "isolation-accept", Revision: invite.Call.Revision}
	if err := abe.api.call(http.MethodPost, path, action, &accepted); err != nil {
		return err
	}
	if accepted.Call.ID != invite.Call.ID || accepted.Call.State != schema.PrivateCallStateAccepted {
		return fmt.Errorf("accept did not return A's accepted call")
	}
	if err := accepted.Call.Validate(); err != nil {
		return err
	}
	w.a.callID = accepted.Call.ID
	w.marks.str("A's call attempt", w.a.callID)
	return nil
}

// checkNoCurrentCall is the snapshot row's own check: a valid snapshot with
// no call in it, since the prober has none.
func checkNoCurrentCall(_ *world, _ api, body []byte) error {
	var snap schema.PrivateCallSnapshotV1
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return fmt.Errorf("the call snapshot does not decode: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return err
	}
	if snap.Call != nil {
		return fmt.Errorf("the prober has no call, and her snapshot holds one")
	}
	return nil
}

// callMatrixRows are the rows for private calls and their media route.
func callMatrixRows() []row {
	action := schema.PrivateCallActionRequestV1{ActionID: "isolation-probe", Revision: 1}
	callAction := func(verb string) row {
		return row{route: "POST /v1/calls/{call}/" + verb, what: "acts (" + verb + ") on a call attempt she has no part in",
			want: callNotFound, withoutPassword: personNotOwn,
			probe: func(*world, subject) probe {
				return post("/v1/calls/"+neverIssuedCall+"/"+verb, action)
			}}
	}
	return []row{
		{route: "POST /v1/dms/{conversation}/calls", what: "invites into a conversation she has no part in",
			want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(*world, subject) probe {
				return post(callsOfConversation(), schema.InvitePrivateCallRequestV1{ActionID: "isolation-probe"})
			}},
		{route: "GET /v1/dms/{conversation}/calls", what: "lists the calls of a conversation she has no part in",
			want: dmNotFound, withoutPassword: personNotOwn,
			probe: func(*world, subject) probe { return get(callsOfConversation()) }},
		{route: "GET /v1/calls/current", what: "reads her own current call (none)", own: true, inspect: checkNoCurrentCall, withoutPassword: personNotOwn,
			probe: func(*world, subject) probe { return get("/v1/calls/current") }},
		callAction("accept"), callAction("decline"), callAction("cancel"), callAction("hangup"),
		{route: "POST /v1/calls/{call}/session", what: "requests media for a call attempt she has no part in",
			want: callNotFound, withoutPassword: personNotOwn,
			pick: func(w *world) subject { return subject{kind: "call", name: w.a.callID} },
			probe: func(_ *world, s subject) probe {
				id := s.name
				if len(id) < schema.MinPrivateCallIDBytes {
					id = neverIssuedCall
				}
				return post("/v1/calls/"+id+"/session", nil)
			}},
	}
}
