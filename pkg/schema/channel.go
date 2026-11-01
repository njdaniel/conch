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
	// Visibility is public or private (ADR-013 decision 2, issue #978).
	// Omitted by a server that predates the community model, which a
	// client reads as private: every channel then was. A server with it
	// always sends it.
	Visibility ChannelVisibility `json:"visibility,omitempty"`
	// Default is true when new members start in this channel (ADR-013
	// decision 5). Omitted when false. A default channel is always public.
	Default bool `json:"default,omitempty"`
}

// The community model (ADR-013, docs/design/community-model.md §3.2, issue
// #978): a channel is public or private, any member may create one, public
// channels are listed in a directory that members join from, and operators
// holding manage_channels (ADR-014) set a channel's visibility and whether
// it is a default channel. These shapes are additive REST bodies with no
// schema-name field, versioned by the V1 type suffix.

// ChannelVisibility says who can find a channel (ADR-013 decision 2). The
// vocabulary is closed.
type ChannelVisibility string

const (
	// ChannelPublic: listed to every member in the directory; any member may
	// join it themselves. A default channel is always public.
	ChannelPublic ChannelVisibility = "public"
	// ChannelPrivate: visible only to its members; its managers and
	// operators add people. To anyone else it does not exist: every route
	// that names it answers the 404 a channel that was never made gets.
	ChannelPrivate ChannelVisibility = "private"
)

// Valid reports whether v is a recognized visibility.
func (v ChannelVisibility) Valid() bool {
	switch v {
	case ChannelPublic, ChannelPrivate:
		return true
	default:
		return false
	}
}

// Error codes of the channel routes of the community model (Error.Code).
const (
	// ErrorCodeChannelNotJoinable (HTTP 403): the caller may not join this
	// public channel themselves: an agent, or a principal banned from it
	// (moderation). A private channel is never this: to a non-member it is
	// channel_not_found, as for a channel that does not exist.
	ErrorCodeChannelNotJoinable = "channel_not_joinable"
	// ErrorCodeChannelSettingsConflict (HTTP 409): the settings asked for
	// contradict each other or the server: a private default channel, or
	// the joining settings' open_channels naming a private channel
	// (community-model §3.4, Option B).
	ErrorCodeChannelSettingsConflict = "channel_settings_conflict"
	// ErrorCodeChannelLastMember (HTTP 409): the caller is the last member
	// of a private channel and may not leave it; an operator adds someone
	// first, or archives the channel once archiving exists.
	ErrorCodeChannelLastMember = "channel_last_member"
)

// CreateChannelRequestV1 is the body of POST /v1/channels: a member makes a
// channel and chooses its visibility (ADR-013 decision 2; the permission is
// create_public_channels or create_private_channels, ADR-014). The creator
// is its first member and its manager. Name is held to the create rule and
// is unique by how it reads, as principal names are; every collision
// answers one 409 channel_exists, and a caller without manage_channels has
// a daily attempt limit, so the collision is a bounded, audited existence
// oracle on private names (community-model §5).
type CreateChannelRequestV1 struct {
	Name       string            `json:"name"`
	Visibility ChannelVisibility `json:"visibility"`
}

// Validate reports whether the request is well-formed: Name meets the
// create rule (CreateChannelRequest.Validate) and Visibility is known. That
// the name clashes with no other channel's is the server's to decide.
func (r CreateChannelRequestV1) Validate() error {
	if err := (CreateChannelRequest{Name: r.Name}).Validate(); err != nil {
		return err
	}
	if !r.Visibility.Valid() {
		return fmt.Errorf("schema: channel visibility must be one of: %s", strings.Join(Vocabularies["ChannelVisibility"], ", "))
	}
	return nil
}

// CreateChannelResponseV1 is the response to POST /v1/channels: the channel
// as it now is, with Manages true for the creator.
type CreateChannelResponseV1 struct {
	Channel ChannelV0 `json:"channel"`
}

// ChannelListingV1 is one row of the channel directory, GET
// /v1/directory/channels: a public channel, or a private channel the caller
// is in. Joined says whether the caller is a member; Manages whether they
// manage it, present only when Joined. Members is the member count, present
// only on the ?scope=all listing of a caller holding manage_channels
// (ADR-014), which also lists every private channel; the member view
// carries no counts and no names, so a count on a private channel can
// never be an oracle (community-model §5).
type ChannelListingV1 struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	CreatedAt  Timestamp         `json:"created_at"`
	Visibility ChannelVisibility `json:"visibility"`
	Default    bool              `json:"default,omitempty"`
	Joined     bool              `json:"joined"`
	Manages    bool              `json:"manages,omitempty"`
	Members    *int64            `json:"members,omitempty"`
}

// ChannelDirectoryResponseV1 is the body of GET /v1/directory/channels.
// Channels is ordered by id ascending and is always a JSON array, never
// null: producers send an empty slice.
type ChannelDirectoryResponseV1 struct {
	Channels []ChannelListingV1 `json:"channels"`
}

// JoinChannelResponseV1 answers POST /v1/channels/{channel}/join: the
// channel and the caller's membership, new or, when the caller was already
// a member, the existing one (the route is idempotent). A self-join never
// carries can_decide or manages.
type JoinChannelResponseV1 struct {
	Channel ChannelV0       `json:"channel"`
	Member  ChannelMemberV1 `json:"member"`
}

// ChannelSettingsRequestV1 is the body of PUT /v1/channels/{id}/settings
// (manage_channels, ADR-014): the fields to change; one left out keeps its
// value. At least one must be present. Marking a private channel default,
// or turning a default channel private, is 409 channel_settings_conflict.
type ChannelSettingsRequestV1 struct {
	Visibility *ChannelVisibility `json:"visibility,omitempty"`
	Default    *bool              `json:"default,omitempty"`
}

// Validate reports whether the request changes something and names a known
// visibility when it names one. Whether the result is consistent with the
// channel (a default channel is public) is the server's to decide.
func (r ChannelSettingsRequestV1) Validate() error {
	if r.Visibility == nil && r.Default == nil {
		return errors.New("schema: the request changes no setting")
	}
	if r.Visibility != nil && !r.Visibility.Valid() {
		return fmt.Errorf("schema: channel visibility must be one of: %s", strings.Join(Vocabularies["ChannelVisibility"], ", "))
	}
	return nil
}

// ChannelSettingsResponseV1 is the answer of PUT /v1/channels/{id}/settings:
// the channel as it now is; whether the change turned open sign-up off,
// because the last default channel was unmarked (community-model §2.1);
// and the warnings the change raised, conditions to show and never
// refusals. Warnings carries default_channel_exposes_members when a
// channel is marked default while members do not hold see_member_directory
// (community-model §13.7 under ADR-014). It is omitted when empty, and by
// a server that predates it. Turning a channel private touches no guest and
// no guest link while none can exist (ADR-013 amendment of 2026-10-30).
type ChannelSettingsResponseV1 struct {
	Channel             ChannelV0            `json:"channel"`
	OpenSignupTurnedOff bool                 `json:"open_signup_turned_off,omitempty"`
	Warnings            []PermissionsWarning `json:"warnings,omitempty"`
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
// opened the socket with, and finds its entry by channel_id. Since the
// community model (ADR-013) it is also sent when a channel's settings
// change, and carries the same two optional fields as ChannelV0.
type ChannelDocV1 struct {
	// Schema is the document discriminator; always ChannelSchemaV1.
	Schema string `json:"schema"`
	// ChannelID is the channel's id, as every events document names its
	// channel.
	ChannelID int64     `json:"channel_id"`
	Name      string    `json:"name"`
	CreatedAt Timestamp `json:"created_at"`
	// Visibility and Default are as on ChannelV0: omitted by a server that
	// predates the community model, which a reader takes as private and
	// not default.
	Visibility ChannelVisibility `json:"visibility,omitempty"`
	Default    bool              `json:"default,omitempty"`
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
	if d.Visibility != "" && !d.Visibility.Valid() {
		return fmt.Errorf("schema: channel visibility must be one of: %s", strings.Join(Vocabularies["ChannelVisibility"], ", "))
	}
	return nil
}
