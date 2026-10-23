package schema

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ChannelV0 is the v0 wire representation of a channel.
type ChannelV0 struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Manages is true when the caller is a manager of this channel
	// (manages = 1 on their membership row, ADR-010, issue #697). Omitted
	// when false, and on routes where there is no per-caller membership
	// row (such as POST /v0/channels). An operator's standing comes from
	// WhoAmIResponseV1.Role, not from this bit.
	Manages bool `json:"manages,omitempty"`
}

// MaxChannelNameLength is the longest allowed channel name, in characters.
const MaxChannelNameLength = 100

// CreateChannelRequest is the request body for creating a v0 channel.
type CreateChannelRequest struct {
	Name string `json:"name"`
}

// Validate reports whether the create request is well-formed: the name is
// non-blank and passes ValidateDisplayName. Names stored before the rule
// existed keep working; it is enforced only on creation.
func (r CreateChannelRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("schema: channel name must not be empty")
	}
	return ValidateDisplayName("channel name", r.Name, MaxChannelNameLength)
}

// ListChannelsResponse is the response body for listing every channel,
// ordered by id ascending. Channels is always a JSON array, never null.
type ListChannelsResponse struct {
	Channels []ChannelV0 `json:"channels"`
}

// CreateChannelResponse is the response body after a channel is created.
type CreateChannelResponse struct {
	Channel ChannelV0 `json:"channel"`
}

// Renaming a channel (issue #727, docs/design/channel-rename.md). The rename
// route is the one channel route addressed by id: the name is what it
// changes. These shapes are additive.

// ErrorCodeChannelNameStale is the 409 a rename gets when its From is not the
// channel's name now: another rename came first, and the caller decides again
// from the current list.
const ErrorCodeChannelNameStale = "channel_name_stale"

// RenameChannelRequestV1 is the body of PUT /v1/channels/{id}/name.
type RenameChannelRequestV1 struct {
	// Name is the new name: the rule a name meets on creation, checked by
	// Validate.
	Name string `json:"name"`
	// From is the name the caller believes the channel has now. The server
	// refuses the rename (409 channel_name_stale) when it is not, so two
	// operators working from different lists cannot rename past each other.
	// Required; the same string as Name is allowed (a no-op).
	From string `json:"from"`
}

// Validate reports whether the request is well-formed: Name meets the create
// rule (CreateChannelRequest.Validate) and From is not blank. From is only
// compared with the stored name, never stored, so nothing more is asked of
// it. That the new name clashes with no other channel's is the server's to
// decide.
func (r RenameChannelRequestV1) Validate() error {
	if err := (CreateChannelRequest{Name: r.Name}).Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.From) == "" {
		return errors.New("schema: rename from must not be empty")
	}
	return nil
}

// RenameChannelResponseV1 is the response to a rename: the channel as it now
// is.
type RenameChannelResponseV1 struct {
	Channel ChannelV0 `json:"channel"`
}

// ChannelSchemaV1 names the document the events socket sends when the
// channel it follows changes: today, when it is renamed.
const ChannelSchemaV1 = "conch.channel.v1"

// ChannelDocV1 is the channel as it now is. Like the roster and the member
// list it is the whole state, not a delta: a reader that misses one and later
// reads GET /v1/channels is in the same place. It is sent only on a change,
// never on connect, and carries no old name: the reader knows the name it
// opened the socket with, and finds its entry by channel_id.
type ChannelDocV1 struct {
	// Schema is the document discriminator; always ChannelSchemaV1.
	Schema string `json:"schema"`
	// ChannelID is the channel's id, as every events document names its
	// channel.
	ChannelID int64     `json:"channel_id"`
	Name      string    `json:"name"`
	CreatedAt Timestamp `json:"created_at"`
}

// Validate reports whether the document is structurally well-formed: the v1
// schema name, a positive channel_id, a name and a created_at. The name is
// not held to the create rule: a channel named before the rule existed keeps
// its name, and this document must be able to carry it.
func (d ChannelDocV1) Validate() error {
	if d.Schema != ChannelSchemaV1 {
		return fmt.Errorf("schema: channel schema must be %q, got %q", ChannelSchemaV1, d.Schema)
	}
	if d.ChannelID <= 0 {
		return fmt.Errorf("schema: channel channel_id must be positive, got %d", d.ChannelID)
	}
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("schema: channel name is required")
	}
	if d.CreatedAt.IsZero() {
		return errors.New("schema: channel created_at is required")
	}
	return nil
}
