package schema

import (
	"errors"
	"fmt"
)

// The per-person events stream (issue #2040, ADR-017, docs/design/
// direct-messages.md §2.3), GET /v1/events/person/ws.
//
// It is a stream scoped to one signed-in person, not to a channel: it exists
// so that a member with no channel open still receives their direct-message
// frames and, once the call frames land, incoming-call rings. Only a human
// member's own credential opens it; an agent, a guest and an act-as
// credential are refused. It never carries a channel document.
//
// Every frame is one JSON text message, told apart by its schema field, as on
// the channel events socket. The frames, in the order a socket sees them:
//
//  1. PersonStreamHelloV1, once, first: the socket's own instance id.
//  2. DMSnapshotV1, once: the reconnect snapshot (each conversation's Revision says which
//     DM frames to expect after it: those above it).
//  3. Then, as they happen and only those addressed to this person:
//     DMMessageV1, DMMessageEditedV1, DMMessageRemovedV1 and DMChangedV1.
//     Call frames are added to this list by the call issue (#2029); a client
//     ignores a schema it does not know. The server's HeartbeatV1 also goes
//     out on the connection at its usual interval, for the client's silence
//     watchdog.
//
// The family is additive: a new type and a new schema name, no published type
// changes.

// PersonStreamHelloSchemaV1 names the first frame of the per-person stream.
const PersonStreamHelloSchemaV1 = "conch.person_stream_hello.v1"

// PersonStreamInstanceIDLen is the length of PersonStreamHelloV1.InstanceID:
// 128 random bits as lowercase hexadecimal.
const PersonStreamInstanceIDLen = 32

// PersonStreamHelloV1 is the first frame on every connection of the
// per-person stream. InstanceID is the id of this socket alone: it is random,
// it is not derived from the credential or the person, and the next
// connection of the same tab gets another. Ringing dismissal and the
// selected-endpoint choice name a socket by it, so that two tabs of one
// person (one credential, one browser profile) can be told apart (ADR-017,
// review §4). It is not a secret and not an authenticator: it grants nothing
// and is never accepted in place of a credential.
type PersonStreamHelloV1 struct {
	// Schema is the document discriminator; always PersonStreamHelloSchemaV1.
	Schema string `json:"schema"`
	// InstanceID is this socket's id, PersonStreamInstanceIDLen lowercase
	// hexadecimal characters.
	InstanceID string `json:"instance_id"`
}

// Validate reports whether the frame is well-formed.
func (h PersonStreamHelloV1) Validate() error {
	if h.Schema != PersonStreamHelloSchemaV1 {
		return fmt.Errorf("schema: person stream hello schema must be %q, got %q", PersonStreamHelloSchemaV1, h.Schema)
	}
	if len(h.InstanceID) != PersonStreamInstanceIDLen {
		return fmt.Errorf("schema: person stream hello instance_id must be %d characters", PersonStreamInstanceIDLen)
	}
	for _, c := range h.InstanceID {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errors.New("schema: person stream hello instance_id must be lowercase hexadecimal")
		}
	}
	return nil
}
