package schema

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Version 2 of the pending feed's summary (issue #2016, DRAFT pending
// ADR-017, #2038). A direct message cannot be a NotificationSummaryV1: v1
// requires a positive channel_id and a channel_name, and a DM has neither.
// Bending v1 to carry one (a zero channel id, a conversation id in its
// place) would have an older phone open a channel that does not exist, or
// the wrong one. So v2 names where the message was said with an explicit
// MessageContext, and a v1 client is never sent a DM.
//
// v1 is unchanged and stays served: GET /v1/notifications/pending answers
// ListPendingNotificationsResponseV1 and never holds a DM; GET
// /v2/notifications/pending answers ListPendingNotificationsResponseV2 with
// both. The two share the request, the candidate bound and the More rule of
// v1 (notification.go), but not the cursor: v2's is opaque and moves on every
// examined candidate (see ListPendingNotificationsResponseV2), because DM
// message ids are an id space of their own and no number orders them against
// channel message ids.
//
// This is a new version of a published family and needs Nick's sign-off.

// NotificationSummarySchemaV2 is the schema name of a v2 feed summary.
const NotificationSummarySchemaV2 = "conch.notification_summary.v2"

// MessageContextKind is where a message was said: a channel, or a direct
// conversation. The vocabulary is closed and is a union's tag.
type MessageContextKind string

const (
	// MessageContextChannel: a channel, named by channel_id and
	// channel_name.
	MessageContextChannel MessageContextKind = "channel"
	// MessageContextDirect: a direct conversation, named by
	// conversation_id.
	MessageContextDirect MessageContextKind = "direct"
)

// Valid reports whether k is a recognized context kind.
func (k MessageContextKind) Valid() bool {
	switch k {
	case MessageContextChannel, MessageContextDirect:
		return true
	default:
		return false
	}
}

// MessageContext says where a message was said, so that a channel id and a
// conversation id are never mistaken for each other:
//
//	{"kind": "channel", "channel_id": 7, "channel_name": "ops"}
//	{"kind": "direct", "conversation_id": 12}
//
// Exactly the fields of its kind are set.
type MessageContext struct {
	Kind           MessageContextKind `json:"kind"`
	ChannelID      int64              `json:"channel_id,omitempty"`
	ChannelName    string             `json:"channel_name,omitempty"`
	ConversationID int64              `json:"conversation_id,omitempty"`
}

// Validate reports whether the context has a known kind and exactly that
// kind's fields.
func (c MessageContext) Validate() error {
	switch c.Kind {
	case MessageContextChannel:
		if c.ChannelID <= 0 {
			return errors.New("schema: message context channel_id must be positive")
		}
		if c.ChannelName == "" {
			return errors.New("schema: message context channel_name must not be empty")
		}
		if c.ConversationID != 0 {
			return errors.New("schema: message context of kind channel must not carry conversation_id")
		}
	case MessageContextDirect:
		if c.ConversationID <= 0 {
			return errors.New("schema: message context conversation_id must be positive")
		}
		if c.ChannelID != 0 || c.ChannelName != "" {
			return errors.New("schema: message context of kind direct must not carry a channel")
		}
	default:
		return errors.New("schema: message context kind is not one of channel, direct")
	}
	return nil
}

// NotificationSummaryV2 is one message of the v2 pending feed: a
// NotificationSummaryV1 with the channel replaced by Context. Every other
// field means what it means in v1, by v1's rules (the author name's
// predicate, the preview switch, the spoiler rule and the cut).
//
// Kind is NotificationKindDirect exactly when Context is direct, and one of
// v1's three kinds exactly when it is a channel. Under the policy
// "mentions" a DM to the reader notifies, as a whisper does; under "off",
// nothing does.
type NotificationSummaryV2 struct {
	Schema     string           `json:"schema"`
	Context    MessageContext   `json:"context"`
	MessageID  int64            `json:"message_id"`
	PostedAt   Timestamp        `json:"posted_at"`
	Kind       NotificationKind `json:"kind"`
	AuthorID   int64            `json:"author_id"`
	AuthorName string           `json:"author_name,omitempty"`
	Text       string           `json:"text,omitempty"`
}

// Validate reports whether the summary is well-formed: v1's rules, a valid
// context, and a kind that agrees with it.
func (s NotificationSummaryV2) Validate() error {
	if s.Schema != NotificationSummarySchemaV2 {
		return fmt.Errorf("schema: notification summary schema must be %q, got %q", NotificationSummarySchemaV2, s.Schema)
	}
	if err := s.Context.Validate(); err != nil {
		return fmt.Errorf("schema: notification summary: %w", err)
	}
	if s.MessageID <= 0 {
		return errors.New("schema: notification summary message_id must be positive")
	}
	if s.PostedAt.IsZero() {
		return errors.New("schema: notification summary posted_at must be set")
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("schema: notification summary kind %q is not one of message, mention, whisper, direct", s.Kind)
	}
	if (s.Kind == NotificationKindDirect) != (s.Context.Kind == MessageContextDirect) {
		return errors.New("schema: notification summary kind direct must go with, and only with, a direct context")
	}
	if s.AuthorID <= 0 {
		return errors.New("schema: notification summary author_id must be positive")
	}
	if utf8.RuneCountInString(s.Text) > MaxNotificationTextChars {
		return fmt.Errorf("schema: notification summary text must be at most %d characters", MaxNotificationTextChars)
	}
	return nil
}

// ListPendingNotificationsResponseV2 is the answer to GET
// /v2/notifications/pending?after=<cursor>: ListPendingNotificationsResponseV1
// with v2 summaries, by the same rules except the cursor and the two below.
//
// A DM is a candidate only for a credential that a live DM frame would be
// delivered to now (the delivery rule in dm.go's header, design §3.2): the
// credential is a person's own sign-in, and not an act-as credential or an
// agent's; the person is one of the conversation's pair and enabled; and no
// personal block holds between the pair. v1's guard (a human holding a
// credential) is not enough, because it admits an operator-issued act-as
// credential, which ADR-017 Decision 7 bars from every DM route; a feed that
// reused it would hand that credential a conversation id, an author and a
// preview of a DM it may not read. For any credential that fails the test,
// DMs are not candidates: never examined, never counted, never a summary, a
// cursor position or a More, so the feed is exactly what it would be with no
// DMs on the server (and the credential still gets its channel summaries).
// The test is made per candidate at request time, so access that ends stops
// the next page. The server's and the person's own DM switches do not apply:
// they refuse new contact, not reading what was already said to the reader.
//
// The cursor is opaque, a string, not v1's message id: channel message ids
// and DM message ids are separate id spaces (DMMessageV1), so no one number
// orders a channel message against a DM. The client passes Cursor back as
// ?after= and never reads it; an empty ?after= starts from the oldest
// pending summary. Summaries are in the order the server delivers them,
// oldest first by posted_at, and a (context, message_id) pair appears once.
// A cursor the server did not issue, or longer than MaxDMCursorBytes, is
// invalid_request.
//
// Progress. A bounded scan can examine candidates and select none: a long
// run of channel messages the reader muted, or whose notification policy
// refuses them, with a pending DM beyond the bound. So the cursor is the
// position of the last eligible candidate examined for this answer, whether
// or not it became a summary, never behind the last summary and never behind
// ?after=. An empty page therefore carries a cursor whenever the scan
// examined anything past ?after=, and More is true only when a further
// request starts strictly past ?after= (CheckProgress): a client never
// rescans the same rows, and never needs to handle More with an unchanged
// cursor. A page that has summaries always has a cursor, so a client always
// holds its resume position. Only when nothing at all was examined (no
// candidate past ?after=) is the cursor ?after= itself, or empty at the
// start, and then More is false. Like v1's, the cursor is never a function
// of messages the reader may not see.
type ListPendingNotificationsResponseV2 struct {
	Summaries []NotificationSummaryV2 `json:"summaries"`
	Cursor    string                  `json:"cursor,omitempty"`
	More      bool                    `json:"more"`
}

// Validate reports whether the page is well-formed: a list, not null, of at
// most MaxPendingNotificationsPage valid summaries, no one twice and none
// earlier than the one before, a cursor of URL-safe characters, present
// whenever there are summaries or there is more.
func (r ListPendingNotificationsResponseV2) Validate() error {
	if r.Summaries == nil {
		return errors.New("schema: pending notifications summaries must be a list, not null")
	}
	if len(r.Summaries) > MaxPendingNotificationsPage {
		return fmt.Errorf("schema: pending notifications summaries must be at most %d", MaxPendingNotificationsPage)
	}
	type key struct {
		kind MessageContextKind
		ctx  int64
		msg  int64
	}
	seen := make(map[key]struct{}, len(r.Summaries))
	for i, s := range r.Summaries {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("schema: pending notifications summaries[%d]: %w", i, err)
		}
		ctx := s.Context.ChannelID
		if s.Context.Kind == MessageContextDirect {
			ctx = s.Context.ConversationID
		}
		k := key{s.Context.Kind, ctx, s.MessageID}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("schema: pending notifications summaries[%d] is listed twice", i)
		}
		seen[k] = struct{}{}
		if i > 0 && s.PostedAt.Time().Before(r.Summaries[i-1].PostedAt.Time()) {
			return fmt.Errorf("schema: pending notifications summaries[%d] is earlier than the one before", i)
		}
	}
	if r.Cursor != "" && !validDMToken(r.Cursor, MaxDMCursorBytes) {
		return fmt.Errorf("schema: pending notifications cursor must be 1 to %d URL-safe characters", MaxDMCursorBytes)
	}
	if len(r.Summaries) > 0 && r.Cursor == "" {
		return errors.New("schema: pending notifications cursor is required when there are summaries")
	}
	if r.More && r.Cursor == "" {
		return errors.New("schema: pending notifications cursor is required when more is true")
	}
	return nil
}

// CheckProgress reports whether the page, answering a request whose ?after=
// was after, lets a client make progress: when More is true the cursor must
// differ from after, so asking again cannot rescan the same rows. A client
// treats a violation as a server fault and does not loop on it.
func (r ListPendingNotificationsResponseV2) CheckProgress(after string) error {
	if r.More && r.Cursor == after {
		return errors.New("schema: pending notifications more is true but the cursor did not advance past after")
	}
	return nil
}
