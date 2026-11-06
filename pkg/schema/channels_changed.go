package schema

import "fmt"

// The channel-list signal (issue #1027; browser UX audit 3, F1). A person
// added to a channel, or removed from one, by someone else had no way to learn
// it without a reload: no events frame said "your channels changed". The
// events socket (GET /v1/events/ws) now sends this document to every human
// events socket of a principal whose answer to GET /v1/channels may have
// changed, and to no one else. Design: docs/design/membership-live.md.
//
// It is a signal, not a delta. It names no channel, no principal and no
// reason, so nothing about a channel can leak through it, and a race between
// the change and the frame cannot either: the subscriber's goroutine reads
// nothing to send it. A client that receives it re-reads GET /v1/channels and
// GET /v1/read-state and replaces, never merges, what it holds; a channel
// absent from the new list is gone, and its routes answer 404
// channel_not_found as they do for any channel the caller is not in.
//
// It differs from conch.directory_changed.v1 (community-model §13.6) in what
// it tells the client to re-read: this one the caller's own channels, that
// one the people directory. A single change may send both, each to its own
// recipients.
//
// It is additive: a new type, and no existing frame changes. Readers of the
// events socket already skip a schema they do not know, and readers of this
// one ignore unknown fields; the server never adds content to v1.

// ChannelsChangedSchemaV1 is the versioned name identifying the v1
// channel-list signal on the events socket.
const ChannelsChangedSchemaV1 = "conch.channels_changed.v1"

// ChannelsChangedV1 tells a client that the caller's channel list may have
// changed. It carries nothing but its schema.
type ChannelsChangedV1 struct {
	// Schema is the document discriminator; always ChannelsChangedSchemaV1.
	Schema string `json:"schema"`
}

// Validate reports whether the frame is a v1 channel-list signal.
func (c ChannelsChangedV1) Validate() error {
	if c.Schema != ChannelsChangedSchemaV1 {
		return fmt.Errorf("schema: channels changed schema must be %q, got %q", ChannelsChangedSchemaV1, c.Schema)
	}
	return nil
}
