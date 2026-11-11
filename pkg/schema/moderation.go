package schema

import (
	"errors"
	"fmt"
	"strings"
)

// Channel moderation (issue #849, docs/design/moderation.md §10, signed off
// 2026-11-05): the ban list of a channel and the tombstone of a removed
// message. Everything here is additive: new types and constants, and the
// optional MessageV2.Removed; no published type changes meaning. The REST-only
// bodies carry no schema field, as ChannelMemberV1 does not; MessageRemovedV1
// does, because it travels on the events socket. The removal route reuses
// ErrorCodeMessageNotFound (reaction.go): one answer for a message that does
// not exist, is in another channel, or is not visible to the caller.

// MessageRemovedBody is the body of every removed message, on every read
// surface and every envelope version (moderation.md §7.3). A v0 or v1 reader
// gets it with no marker and still renders something safe.
const MessageRemovedBody = "This message was removed by a moderator."

// MessageRemovedSchemaV1 names the events-socket frame that says a message
// was removed (moderation.md §7.4). A frame of its own, as MessageEditedV1
// is, because on the events socket a MessageV2 means "a new message" and a
// client notifies on it; a removal notifies no one and replaces the copy the
// client holds (a notification preview, an open view).
const MessageRemovedSchemaV1 = "conch.message_removed.v1"

// ChannelBanV1 is one entry of GET /v1/channels/{channel}/bans (moderation.md
// §6): a principal kept out of the channel, who banned it, and when. The ban
// is keyed by PrincipalID; a rename, conversion or hide does not lift it.
// Name is the principal's current name as the server renders it everywhere
// (a guest's handle, hidden-<id> for a hidden principal), read at list time
// and never stored with the ban: it is for display and confirmation only,
// and authorization never uses it. No reason, expiry, role, kind, address or
// credential is carried.
type ChannelBanV1 struct {
	PrincipalID int64  `json:"principal_id"`
	Name        string `json:"name"`
	// CreatedBy is the principal who banned; omitted when unknown (a ban
	// written with authentication off). A manager may repeat or lift only a
	// ban they created, so a client shows it.
	CreatedBy int64     `json:"created_by,omitempty"`
	CreatedAt Timestamp `json:"created_at"`
}

// Validate reports whether the entry is well-formed: a positive principal_id,
// a name that is not blank, a created_by that is absent or positive (it may
// equal principal_id: a manager may ban themselves, §6.1), and a created_at.
func (b ChannelBanV1) Validate() error {
	if b.PrincipalID <= 0 {
		return fmt.Errorf("schema: channel ban principal_id must be positive, got %d", b.PrincipalID)
	}
	if strings.TrimSpace(b.Name) == "" {
		return fmt.Errorf("schema: channel ban %d name is required", b.PrincipalID)
	}
	if b.CreatedBy < 0 {
		return fmt.Errorf("schema: channel ban %d created_by must not be negative, got %d", b.PrincipalID, b.CreatedBy)
	}
	if b.CreatedAt.IsZero() {
		return fmt.Errorf("schema: channel ban %d created_at is required", b.PrincipalID)
	}
	return nil
}

// ListChannelBansResponseV1 is the body of GET /v1/channels/{channel}/bans.
// Bans is always a JSON array (empty when none), ordered by principal_id.
type ListChannelBansResponseV1 struct {
	Bans []ChannelBanV1 `json:"bans"`
}

// Validate reports whether the list is well-formed: a non-nil array of valid
// entries in strictly ascending principal_id order, without repeats.
func (l ListChannelBansResponseV1) Validate() error {
	if l.Bans == nil {
		return errors.New("schema: channel bans must be an array, not null")
	}
	for i, b := range l.Bans {
		if err := b.Validate(); err != nil {
			return err
		}
		if i > 0 && b.PrincipalID <= l.Bans[i-1].PrincipalID {
			return errors.New("schema: channel bans must be in ascending principal_id order, without repeats")
		}
	}
	return nil
}

// MessageRemovedV1 carries the tombstone as the subscriber may see it, read
// with the subscriber's own reader when the frame is sent. It is sent only
// to a subscriber who could see the message: a whisper's removal reveals
// nothing to anyone else.
type MessageRemovedV1 struct {
	// Schema is the frame discriminator; always MessageRemovedSchemaV1.
	Schema  string    `json:"schema"`
	Message MessageV2 `json:"message"`
}

// Validate reports whether the frame is well-formed: the schema name, and a
// valid message that is removed.
func (f MessageRemovedV1) Validate() error {
	if f.Schema != MessageRemovedSchemaV1 {
		return fmt.Errorf("schema: message removed schema must be %q, got %q", MessageRemovedSchemaV1, f.Schema)
	}
	if err := f.Message.Validate(); err != nil {
		return err
	}
	if !f.Message.Removed {
		return errors.New("schema: message removed frame must carry a removed message")
	}
	return nil
}
