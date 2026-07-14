package schema

import (
	"errors"
	"fmt"
)

// PostMessageRequestV1 is the request body for posting a v1 message: the same
// author and rendered body a v0 post carries, plus an optional typed machine
// payload. It reuses the exact Payload wrapper MessageV1 uses, so a payload that
// posts cleanly is a payload that reads back cleanly.
//
// The server, not the client, assigns the message id, channel, timestamp, and
// envelope schema; a request therefore carries none of those. Like MessageV0's
// PostMessageRequest this is an API shape, not a persisted or registered
// payload — it is not entered in the payload registry.
type PostMessageRequestV1 struct {
	// AuthorID references the authoring principal (human or agent).
	AuthorID int64 `json:"author_id"`
	// Body is the rendered, human-readable form of the message; always present.
	Body string `json:"body"`
	// Payload is the optional typed machine payload; nil (omitted) when absent.
	Payload *Payload `json:"payload,omitempty"`
}

// Validate reports whether the request is structurally well-formed. Its payload
// rules mirror MessageV1: a payload, when present, must have a well-formed
// versioned name and valid JSON data, but its schema need not be registered —
// unknown payload schemas are tolerated by design (forward compatibility).
func (r PostMessageRequestV1) Validate() error {
	if r.AuthorID <= 0 {
		return fmt.Errorf("schema: post message author_id must be positive, got %d", r.AuthorID)
	}
	if r.Body == "" {
		return errors.New("schema: post message body is required")
	}
	if r.Payload != nil {
		if err := r.Payload.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// PostMessageResponseV1 is the response body after a v1 message is persisted. It
// embeds the full MessageV1 the server assigned, including any typed payload.
type PostMessageResponseV1 struct {
	Message MessageV1 `json:"message"`
}

// ListMessagesResponseV1 is one forward page of v1 messages. NextAfter is the
// message ID to pass as the next request's after parameter; zero means there is
// no known subsequent page. This mirrors ListMessagesResponse's pagination
// convention, carrying MessageV1 envelopes instead of MessageV0.
type ListMessagesResponseV1 struct {
	Messages  []MessageV1 `json:"messages"`
	NextAfter int64       `json:"next_after,omitempty"`
}
