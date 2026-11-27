package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/njdaniel/conch/pkg/schema"
)

// The per-person events stream (issue #2040, epic #2013): a socket that
// carries frames addressed to the caller alone, and no channel. It names no
// channel and no principal, so B's member has nothing of A's to ask for: the
// rows are "own" rows, the same shape as the occupancy socket's. A success
// shows a person's own self-made credential opens it (bea's is one: she
// joined through an invite). The other prober, ola, was made by hand, and her
// credential came through POST /v1/principals/{id}/credentials, which the
// store records as made by another: the same request is refused to her, 403
// own_sign_in_required, with the bearer and with a ticket. That refusal is the
// end-to-end proof that a credential made on someone's behalf (the act-as
// kind) cannot open the stream; it stays an expectation of the row, not a
// property to loosen. The first frame is the stream's hello, a document of the stream's
// own that holds an instance id and nothing else, so no marker of A's can be
// in it (the marker scan reads every body the matrix keeps). The DM frames
// that follow the hello are covered by the server's person stream tests
// until the DM store (#2017) installs its source.

// personPath is the person stream.
const personPath = "/v1/events/person/ws"

// personSock is the request that opens the person stream: it takes no
// channel, and its row reads the first frame, the hello.
func personSock(ticket bool) probe {
	return probe{method: http.MethodGet, path: personPath, socket: true, ticket: ticket, frame: true}
}

// checkPersonHello is the person rows' own check: the first frame is a valid
// hello and nothing more (no field it does not declare), so the stream
// opens with nothing of anyone else's.
func checkPersonHello(_ *world, _ api, body []byte) error {
	var hello schema.PersonStreamHelloV1
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hello); err != nil {
		return fmt.Errorf("the person stream's first frame is not a hello: %w", err)
	}
	return hello.Validate()
}

// personNotOwn is the refusal of a prober whose credential another made.
var personNotOwn = answer{http.StatusForbidden, schema.ErrorCodeOwnSignInRequired}

// personMatrixRows are the rows of the person stream.
func personMatrixRows() []row {
	return []row{
		{route: "GET /v1/events/person/ws", what: "opens her own person stream", own: true, inspect: checkPersonHello, withoutPassword: personNotOwn,
			probe: func(*world, subject) probe { return personSock(false) }},
		{route: "GET /v1/events/person/ws", what: "opens her own person stream with a ticket", own: true, inspect: checkPersonHello, withoutPassword: personNotOwn,
			probe: func(*world, subject) probe { return personSock(true) }},
	}
}
