package schema

import (
	"errors"
	"fmt"
)

// MessageSchemaV1 is the versioned name identifying the v1 message envelope on
// the wire. Every MessageV1 carries it in its Schema field so a decoder can
// recognize the envelope version without out-of-band context.
const MessageSchemaV1 = "conch.message.v1"

// MessageV1 is the v1 wire representation of a persisted message: the typed
// envelope introduced in P1. It carries the rendered body every message has
// (ADR-000 D8) plus an optional machine payload identified by a versioned
// schema name.
//
// V1 is a new type alongside MessageV0, never an edit of it. Relative to V0 it
// (1) adds the self-describing Schema discriminator, (2) adds the optional
// Payload, and (3) pins CreatedAt to the UTC/millisecond wire form via
// Timestamp. Channel and author are referenced by id, as in V0.
type MessageV1 struct {
	// Schema is the envelope version discriminator; always MessageSchemaV1.
	Schema string `json:"schema"`
	// ID is the server-assigned message id.
	ID int64 `json:"id"`
	// ChannelID references the channel the message belongs to.
	ChannelID int64 `json:"channel_id"`
	// AuthorID references the authoring principal (human or agent).
	AuthorID int64 `json:"author_id"`
	// CreatedAt is when the server persisted the message (UTC, ms precision).
	CreatedAt Timestamp `json:"created_at"`
	// Body is the rendered, human-readable form of the message; always present.
	Body string `json:"body"`
	// Payload is the optional typed machine payload; nil (omitted) when absent.
	Payload *Payload `json:"payload,omitempty"`
}

// Validate reports whether the envelope is structurally well-formed. It does
// not require a payload's schema to be registered — unknown payload schemas are
// tolerated by design (forward compatibility) — only that the payload's name is
// well-formed and its data is valid JSON.
func (m MessageV1) Validate() error {
	if m.Schema != MessageSchemaV1 {
		return fmt.Errorf("schema: message schema must be %q, got %q", MessageSchemaV1, m.Schema)
	}
	if m.ID <= 0 {
		return fmt.Errorf("schema: message id must be positive, got %d", m.ID)
	}
	if m.ChannelID <= 0 {
		return fmt.Errorf("schema: message channel_id must be positive, got %d", m.ChannelID)
	}
	if m.AuthorID <= 0 {
		return fmt.Errorf("schema: message author_id must be positive, got %d", m.AuthorID)
	}
	if m.CreatedAt.IsZero() {
		return errors.New("schema: message created_at is required")
	}
	if m.Body == "" {
		return errors.New("schema: message body is required")
	}
	if m.Payload != nil {
		if err := m.Payload.Validate(); err != nil {
			return err
		}
	}
	return nil
}
