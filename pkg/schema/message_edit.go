package schema

import (
	"errors"
	"fmt"
	"strings"
)

// Message editing (issue #845, docs/design/message-editing.md): the author's
// change to the text of their own recent message. Everything here is
// additive: new types, constants and error codes, and the optional
// MessageV2.EditedAt; no published type changes meaning. The REST-only bodies
// carry no schema field, as ReactionSummariesResponseV1 does not;
// MessageEditedV1 does, because it travels on the events socket.

// MessageEditedSchemaV1 names the events-socket frame that says a message's
// text changed. It is a frame of its own, not a second MessageV2, because on
// the events socket a MessageV2 means "a new message" and a client notifies
// on it; an edit notifies no one (§7.2).
const MessageEditedSchemaV1 = "conch.message_edited.v1"

const (
	// MessageEditWindowSeconds is how long after its created_at a message
	// may be edited: one hour (§3.2). The server decides by its own clock; a
	// client uses this only to stop offering the action.
	MessageEditWindowSeconds = 3600

	// MaxMessageEdits is the most times one message may be edited, so the
	// most revisions it holds is MaxMessageEdits+1, the text as posted
	// included (§3.3). It is the published bound a revision list is
	// validated against, as well as the write limit: lowering the write
	// limit later must leave this alone, or a stored list would stop
	// validating.
	MaxMessageEdits = 10
)

// Error codes of the edit and revision routes (§4.3), additive on Error. The
// refusals for a message the caller may not edit, cannot see, or that does
// not exist are the existing ErrorCodeMessageNotFound; an agent gets the
// existing ErrorCodeHumanRequired.
const (
	// ErrorCodeMessageNotEditable: the caller wrote the message and can see
	// it, but it is not a text message they may change: it carries a payload
	// other than a file attachment (an approval request, a typed signal), it
	// was removed by a moderator, or it was said on a net where the caller
	// no longer holds a member seat (HTTP 409).
	ErrorCodeMessageNotEditable = "message_not_editable"
	// ErrorCodeMessageEditWindowClosed: more than MessageEditWindowSeconds
	// have passed since the message was posted (HTTP 409).
	ErrorCodeMessageEditWindowClosed = "message_edit_window_closed"
	// ErrorCodeMessageEditLimitReached: the message has been edited
	// MaxMessageEdits times already (HTTP 409).
	ErrorCodeMessageEditLimitReached = "message_edit_limit_reached"
	// ErrorCodeMessageEditRequiresAuth: conchd runs with authentication off,
	// so there is no principal to be the author (HTTP 400).
	ErrorCodeMessageEditRequiresAuth = "message_edit_requires_auth"
)

// EditMessageRequestV1 is the body of PUT
// /v2/channels/{channel}/messages/{message_id}/body: the message's new text,
// and nothing else. The payload, audience, thread and attachments cannot be
// named, so cannot be changed; the server decodes with unknown fields
// refused, so a request that tries is a 400, not a silent no-op.
type EditMessageRequestV1 struct {
	// Body is the new rendered, human-readable text. The same bounds as a
	// post: not empty or only white space, and the whole request at most
	// MaxPostRequestBytes.
	Body string `json:"body"`
}

// Validate reports whether the request is well-formed: a body that is not
// empty and not only white space. The size bound is the server's request
// limit, not checked here.
func (r EditMessageRequestV1) Validate() error {
	if strings.TrimSpace(r.Body) == "" {
		return errors.New("schema: edit message body must not be empty")
	}
	return nil
}

// EditMessageResponseV1 is the answer to an edit: the message as it now is
// for the caller, the same projection a list read gives. After an edit that
// changed the text it carries EditedAt; after a request whose body equals the
// current text nothing changed, and it is the message as it was (with no
// EditedAt if it was never edited).
type EditMessageResponseV1 struct {
	Message MessageV2 `json:"message"`
}

// Validate reports whether the response's message is well-formed.
func (r EditMessageResponseV1) Validate() error {
	return r.Message.Validate()
}

// MessageRevisionV1 is one version of a message's text. Revision 1 is the
// text as posted, at the message's created_at; revision n+1 is the text the
// nth edit set, at that edit's time. The current text is the highest
// revision, and equals the message's body.
type MessageRevisionV1 struct {
	Revision  int       `json:"revision"`
	Body      string    `json:"body"`
	CreatedAt Timestamp `json:"created_at"`
}

// Validate reports whether the revision is well-formed: a positive number, a
// body and a time.
func (r MessageRevisionV1) Validate() error {
	if r.Revision < 1 {
		return fmt.Errorf("schema: message revision must be at least 1, got %d", r.Revision)
	}
	if r.Body == "" {
		return errors.New("schema: message revision body is required")
	}
	if r.CreatedAt.IsZero() {
		return errors.New("schema: message revision created_at is required")
	}
	return nil
}

// ListMessageRevisionsResponseV1 is the body of GET
// /v2/channels/{channel}/messages/{message_id}/revisions: every version of
// one message's text, oldest first, for its author alone (§5). A message
// never edited has one revision, its text as posted. There is no
// pagination: the list is bounded by MaxMessageEdits.
type ListMessageRevisionsResponseV1 struct {
	ChannelID int64               `json:"channel_id"`
	MessageID int64               `json:"message_id"`
	Revisions []MessageRevisionV1 `json:"revisions"`
}

// Validate reports whether the list is well-formed: positive ids, and 1 to
// MaxMessageEdits+1 valid revisions numbered 1, 2, 3 … in order, each later
// than the one before.
func (r ListMessageRevisionsResponseV1) Validate() error {
	if r.ChannelID <= 0 {
		return errors.New("schema: message revisions channel_id must be positive")
	}
	if r.MessageID <= 0 {
		return errors.New("schema: message revisions message_id must be positive")
	}
	if len(r.Revisions) == 0 || len(r.Revisions) > MaxMessageEdits+1 {
		return fmt.Errorf("schema: message revisions must list 1 to %d revisions, got %d", MaxMessageEdits+1, len(r.Revisions))
	}
	for i, rev := range r.Revisions {
		if err := rev.Validate(); err != nil {
			return err
		}
		if rev.Revision != i+1 {
			return fmt.Errorf("schema: message revision %d is listed in place %d; revisions are numbered from 1 in order", rev.Revision, i+1)
		}
		if i > 0 && !rev.CreatedAt.Time().After(r.Revisions[i-1].CreatedAt.Time()) {
			return fmt.Errorf("schema: message revision %d must be later than revision %d", rev.Revision, i)
		}
	}
	return nil
}

// MessageEditedV1 is the events-socket frame for an edit: the edited message
// as the subscriber may see it, read with the subscriber's own reader when the
// frame is sent (§7.2). A client updates a copy it holds (a notification's
// preview, an open view) and does not notify, count unread, or treat it as a
// new message.
type MessageEditedV1 struct {
	// Schema is the frame discriminator; always MessageEditedSchemaV1.
	Schema  string    `json:"schema"`
	Message MessageV2 `json:"message"`
}

// Validate reports whether the frame is well-formed: the schema name, and a
// valid message that carries EditedAt.
func (f MessageEditedV1) Validate() error {
	if f.Schema != MessageEditedSchemaV1 {
		return fmt.Errorf("schema: message edited schema must be %q, got %q", MessageEditedSchemaV1, f.Schema)
	}
	if err := f.Message.Validate(); err != nil {
		return err
	}
	if f.Message.EditedAt == nil {
		return errors.New("schema: message edited frame must carry a message with edited_at")
	}
	return nil
}
