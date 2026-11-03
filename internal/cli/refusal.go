package cli

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/njdaniel/conch/pkg/schema"
)

// The community-model commands (ADR-013, issue #994) say why the server
// refused in their own words, chosen by the refusal's code, as the web page
// does. What the server wrote in its message, and a code this client does not
// know, are never printed: either may carry a channel's name, a proxy's page
// or a terminal escape. The status is enough to tell an unknown refusal from
// a missing server.

// refusalError is a refusal in this client's words. It unwraps to the
// server's ServerError, so IsServerCode still tells the cause.
type refusalError struct {
	line  string
	cause error
}

func (e *refusalError) Error() string { return e.line }

func (e *refusalError) Unwrap() error { return e.cause }

// operatorOnly and communityOnly are the 403 forbidden lines a command uses
// when it is certain of the cause: an operator-only route, and a route for
// people in the community, which agents and people outside it cannot use.
const (
	operatorOnly   = "only an operator may do this"
	voiceForbidden = "the server does not allow this: only an operator who manages members may switch a voice, and only an owner may switch an owner's"
	communityOnly  = "only a person in the community may do this: agents and people outside it cannot"
)

// refusalLines maps a server code to a command's own sentence for it.
type refusalLines map[string]string

// commonRefusals are the sentences every community command shares. A command's
// own lines are looked up first.
var commonRefusals = refusalLines{
	schema.ErrorCodeReauthRequired:  "this needs your password first: run 'conch reauth' (a script credential cannot give one; sign in with 'conch login --password'), then try again",
	"forbidden":                     "the server does not allow you to do this",
	schema.ErrorCodeHumanRequired:   "only a person may do this, not an agent",
	schema.ErrorCodeGuestNotAllowed: "a guest may not do this",
	"channel_not_found":             "no such channel, or you cannot see it",
	"principal_not_found":           "no such person",
	"invalid_request":               "the server did not accept the request",
	"internal_error":                "the server failed; nothing was changed. Try again",
}

// refused turns a server refusal into a refusalError in lines' words, then the
// common ones. Any other error (a network failure, an expired sign-in, a
// body this client could not read) is returned as it is: those lines are this
// client's own already.
func refused(err error, lines refusalLines) error {
	var se *ServerError
	if err == nil || !errors.As(err, &se) {
		return err
	}
	return &refusalError{line: refusalLine(se, lines), cause: err}
}

func refusalLine(se *ServerError, lines refusalLines) string {
	if se.Status == http.StatusTooManyRequests {
		if se.RetryAfterSeconds > 0 {
			return fmt.Sprintf("too many tries: wait %s and try again", span(int64(se.RetryAfterSeconds)))
		}
		return "too many tries: wait a while and try again"
	}
	if line, ok := lines[se.Code]; ok {
		return line
	}
	if se.Code == schema.ErrorCodePermissionDenied {
		// The permission is named when it is one this client knows, so a name
		// from the server is never printed raw.
		if p := schema.Permission(se.Permission); p.Valid() {
			return fmt.Sprintf("you do not have the %s permission, which this needs", p)
		}
		return "you do not have the permission this needs"
	}
	if line, ok := commonRefusals[se.Code]; ok {
		return line
	}
	return fmt.Sprintf("the server refused this (HTTP %d)", se.Status)
}
