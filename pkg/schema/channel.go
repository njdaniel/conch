package schema

import (
	"errors"
	"strings"
	"time"
)

// ChannelV0 is the v0 wire representation of a channel.
type ChannelV0 struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
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
