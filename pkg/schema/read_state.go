package schema

import (
	"errors"
	"fmt"
)

// Read-state wire shapes (issue #351; UX-02, UX-08). A read mark says how far
// a person has read a channel, and the unread count is how many messages they
// may see past it. The mark belongs to the principal, not to a session or a
// credential: one person on two clients has one mark.
//
// The count is of the messages the reader may see (channel-wide ones, their
// nets', their whispers), never of the channel's messages. The server decides
// that; a client never counts.

// ReadStateSchemaV1 is the versioned name identifying the v1 read-state
// document on the wire.
const ReadStateSchemaV1 = "conch.read_state.v1"

// MaxUnreadCount caps ReadStateV1.Unread. A count at the cap means "at least
// this many"; the server never counts further, so the cost of a count is
// bounded however long a channel has been left alone.
const MaxUnreadCount = 1000

// ErrorCodeReadStateRequireAuth: the caller is not a verified principal
// (conchd runs with authentication off), so there is no one to hold a mark
// for (HTTP 400). The human-only refusal of an agent is ErrorCodeHumanRequired.
const ErrorCodeReadStateRequireAuth = "read_state_require_auth"

// ReadStateV1 is one person's read state in one channel. It is the element
// of ListReadStateResponseV1 and the body of PUT
// /v1/channels/{channel}/read-state.
type ReadStateV1 struct {
	// Schema is the document discriminator; always ReadStateSchemaV1.
	Schema string `json:"schema"`
	// ChannelID is the channel the state is for.
	ChannelID int64 `json:"channel_id"`
	// LastReadID is the id of the newest message the person has marked as
	// read; 0 when they never marked the channel.
	LastReadID int64 `json:"last_read_id"`
	// Unread is how many messages after LastReadID the person may see, at most
	// MaxUnreadCount.
	Unread int `json:"unread"`
	// UpdatedAt is when the mark last moved; omitted when it never has.
	UpdatedAt *Timestamp `json:"updated_at,omitempty"`
}

// Validate reports whether the document is structurally well-formed.
func (r ReadStateV1) Validate() error {
	if r.Schema != ReadStateSchemaV1 {
		return fmt.Errorf("schema: read state schema must be %q, got %q", ReadStateSchemaV1, r.Schema)
	}
	if r.ChannelID <= 0 {
		return errors.New("schema: read state channel_id must be positive")
	}
	if r.LastReadID < 0 {
		return errors.New("schema: read state last_read_id must not be negative")
	}
	if r.Unread < 0 || r.Unread > MaxUnreadCount {
		return fmt.Errorf("schema: read state unread must be between 0 and %d", MaxUnreadCount)
	}
	if r.UpdatedAt != nil && r.UpdatedAt.IsZero() {
		return errors.New("schema: read state updated_at must not be zero")
	}
	if r.LastReadID == 0 && r.UpdatedAt != nil {
		return errors.New("schema: read state that was never marked must not carry updated_at")
	}
	return nil
}

// MarkReadRequestV1 is the request body of PUT
// /v1/channels/{channel}/read-state. The mark only moves forward: a lower id
// than the stored one changes nothing, and one above the newest message the
// caller may see is taken as that message.
type MarkReadRequestV1 struct {
	// LastReadID is the id of the newest message the person has read.
	LastReadID int64 `json:"last_read_id"`
}

// Validate reports whether the request is structurally well-formed.
func (r MarkReadRequestV1) Validate() error {
	if r.LastReadID < 0 {
		return errors.New("schema: mark read last_read_id must not be negative")
	}
	return nil
}

// ListReadStateResponseV1 is the body of GET /v1/read-state: one document per
// channel the caller is a member of, ordered by channel id. ReadState is
// always a JSON array; the type has no Validate, so that is the server's
// promise: it sends an array, never null.
type ListReadStateResponseV1 struct {
	ReadState []ReadStateV1 `json:"read_state"`
}
