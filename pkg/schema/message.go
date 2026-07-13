package schema

import "time"

// MessageV0 is the v0 wire representation of a persisted message. V0 carries
// only its rendered body; typed payloads are introduced by a later version.
type MessageV0 struct {
	ID        int64     `json:"id"`
	ChannelID int64     `json:"channel_id"`
	AuthorID  int64     `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// PostMessageRequest is the request body for posting a v0 message.
type PostMessageRequest struct {
	AuthorID int64  `json:"author_id"`
	Body     string `json:"body"`
}

// PostMessageResponse is the response body after a message is persisted.
type PostMessageResponse struct {
	Message MessageV0 `json:"message"`
}

// ListMessagesResponse is one forward page of messages. NextAfter is the
// message ID to pass as the next request's after parameter; zero means there
// is no known subsequent page.
type ListMessagesResponse struct {
	Messages  []MessageV0 `json:"messages"`
	NextAfter int64       `json:"next_after,omitempty"`
}

// Error is the structured error body returned by REST endpoints.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
