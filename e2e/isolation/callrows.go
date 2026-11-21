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
// The rows name no object of A's: A has no conversation the matrix can name,
// since the routes that open one (the DM routes, #2019) are not served yet,
// so the ids are ones never issued, and a call is only ever reached through
// one of A's conversations or A's attempt ids. When the DM routes exist, the
// world builder gives A a conversation and an attempt, and these rows grow a
// pick (A's id, with the ghost comparison) beside the ones here.

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
			probe: func(*world, subject) probe {
				return probe{method: http.MethodPost, path: "/v1/calls/" + neverIssuedCall + "/session"}
			}},
	}
}
