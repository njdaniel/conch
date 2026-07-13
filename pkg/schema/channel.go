package schema

import "time"

// ChannelV0 is the v0 wire representation of a channel.
type ChannelV0 struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateChannelRequest is the request body for creating a v0 channel.
type CreateChannelRequest struct {
	Name string `json:"name"`
}

// CreateChannelResponse is the response body after a channel is created.
type CreateChannelResponse struct {
	Channel ChannelV0 `json:"channel"`
}
