package schema

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Notification preferences and the pending feed (issue #1712, Mobile M11-S;
// docs/design/mobile-notifications.md §4.3, §6 and §9.1, shapes S3 and S8).
//
// The preferences are one object per credential, which is one sign-in: a
// phone on "mentions" and a desk on "all" are two credentials. They exist
// with or without a push registration (push.go). A client on a server with
// no push transport reads and writes the same object and applies it to the
// alerts it builds from the events socket; with a transport, the server
// applies it to decide whether to send a wake, and the feed applies it too.
//
// The feed is what an app fetches after a wake, with its credential: what is
// new for this reader now, computed at the time of the request by the
// predicates the message routes use. A wake carries none of it
// (PushWakeV1).
//
// These are new types; nothing published changes (D-8 on #1116: additive, no
// version bump). A summary is rendered on its own and carries its schema
// name. The rest are REST bodies, versioned by the V1 type suffix, and carry
// no schema-name field.

// NotificationSummarySchemaV1 is the schema name of a feed summary.
const NotificationSummarySchemaV1 = "conch.notification_summary.v1"

const (
	// MaxMutedChannels is how many channel ids a credential's muted list may
	// hold (§5.5).
	MaxMutedChannels = 256
	// MaxPendingNotificationsPage is the most summaries one feed answer
	// holds (§5.7).
	MaxPendingNotificationsPage = 20
	// MaxNotificationTextChars is the most characters (runes) of message
	// text a summary carries when preview is on (§6.3).
	MaxNotificationTextChars = 160
)

// NotificationPolicy is which messages a credential is notified of (§6.2).
// It is the browser's vocabulary. The vocabulary is closed.
type NotificationPolicy string

const (
	// NotificationPolicyOff: nothing. It is the default.
	NotificationPolicyOff NotificationPolicy = "off"
	// NotificationPolicyMentions: a message that mentions the reader's
	// current display name (Mentions), or a whisper addressed to them.
	NotificationPolicyMentions NotificationPolicy = "mentions"
	// NotificationPolicyAll: every message the reader may see and did not
	// write.
	NotificationPolicyAll NotificationPolicy = "all"
)

// Valid reports whether p is a recognized policy.
func (p NotificationPolicy) Valid() bool {
	switch p {
	case NotificationPolicyOff, NotificationPolicyMentions, NotificationPolicyAll:
		return true
	default:
		return false
	}
}

// NotificationPreferencesV1 is a credential's notification preferences: the
// response to GET and PUT /v1/notifications/preferences (§6.1). The route
// takes no id; it is always the calling credential's own.
//
// Policy says which messages notify. Muted is the channels that never do,
// whatever the policy; it is never null. A mute hides alerts, not unread
// badges. Preview says whether a feed summary carries the message's text
// (§6.3). QuietWhenUnreachable says what the phone does with a wake when it
// cannot reach the server: false shows a generic line, true shows nothing
// where the platform allows it.
//
// A muted id is stored as given and is not checked against membership: a
// channel the person cannot see never matches anyway (§5.5).
type NotificationPreferencesV1 struct {
	Policy               NotificationPolicy `json:"policy"`
	Muted                []int64            `json:"muted"`
	Preview              bool               `json:"preview"`
	QuietWhenUnreachable bool               `json:"quiet_when_unreachable"`
}

// DefaultNotificationPreferences is what a credential has before it has
// written any: policy off, nothing muted, no preview, and the generic line
// when the server cannot be reached.
func DefaultNotificationPreferences() NotificationPreferencesV1 {
	return NotificationPreferencesV1{Policy: NotificationPolicyOff, Muted: []int64{}}
}

// Validate reports whether the preferences are well-formed, by the rules of
// PutNotificationPreferencesRequestV1.
func (p NotificationPreferencesV1) Validate() error {
	return validateNotificationPreferences(p.Policy, p.Muted)
}

// PutNotificationPreferencesRequestV1 is the body of PUT
// /v1/notifications/preferences: the same fields as
// NotificationPreferencesV1. It replaces the whole object, so nothing is
// kept from before: policy is required, muted must be sent (as [] for
// none), and a switch left out is off.
//
// Muted holds at most MaxMutedChannels ids, each positive. The same id twice
// is accepted; the server stores it once.
type PutNotificationPreferencesRequestV1 struct {
	Policy               NotificationPolicy `json:"policy"`
	Muted                []int64            `json:"muted"`
	Preview              bool               `json:"preview"`
	QuietWhenUnreachable bool               `json:"quiet_when_unreachable"`
}

// Validate reports whether the request is well-formed: a known policy, and a
// muted list that is present, holds at most MaxMutedChannels ids and no id
// below 1.
func (r PutNotificationPreferencesRequestV1) Validate() error {
	return validateNotificationPreferences(r.Policy, r.Muted)
}

func validateNotificationPreferences(policy NotificationPolicy, muted []int64) error {
	if policy == "" {
		return errors.New("schema: notification preferences policy is required")
	}
	if !policy.Valid() {
		// The client's value is not repeated: a refusal is logged and sent
		// back.
		return errors.New("schema: notification preferences policy is not one of off, mentions, all")
	}
	if muted == nil {
		return errors.New("schema: notification preferences muted must be a list, not null or missing")
	}
	if len(muted) > MaxMutedChannels {
		return fmt.Errorf("schema: notification preferences muted must hold at most %d channels", MaxMutedChannels)
	}
	for i, id := range muted {
		if id <= 0 {
			return fmt.Errorf("schema: notification preferences muted[%d] must be a positive channel id", i)
		}
	}
	return nil
}

// NotificationKind is why a message is in the feed. The server's vocabulary
// is closed, and Valid refuses a value outside it. A client tolerates a kind
// it does not know: it still reads the summary and renders it, so a later
// kind is an addition, not a new version (as JoinWarning is).
//
// A message has one kind, and it does not depend on the policy: whisper
// before mention before message. A whisper to the reader is "whisper" even
// when its body also mentions them, and a message that mentions them is
// "mention" under the policy "all" as under "mentions".
type NotificationKind string

const (
	// NotificationKindMessage: neither of the others. Only the policy "all"
	// puts one in the feed.
	NotificationKindMessage NotificationKind = "message"
	// NotificationKindMention: the body mentions the reader (Mentions), and
	// the message is not a whisper to them.
	NotificationKindMention NotificationKind = "mention"
	// NotificationKindWhisper: a whisper addressed to the reader. It counts
	// as a mention under the policy "mentions" (Q3 on #1116).
	NotificationKindWhisper NotificationKind = "whisper"
)

// Valid reports whether k is a recognized kind.
func (k NotificationKind) Valid() bool {
	switch k {
	case NotificationKindMessage, NotificationKindMention, NotificationKindWhisper:
		return true
	default:
		return false
	}
}

// NotificationSummaryV1 is one message of the pending feed
// (conch.notification_summary.v1, §4.3): what a phone needs to render one
// notification and to open the right place when it is tapped.
//
// A removed message, the tombstone a moderator leaves (MessageV2.Removed),
// is never summarised.
//
// AuthorID is always sent, as MessageV2 always carries its author. Only
// AuthorName passes through the hide-members predicate: it is the name the
// channel's authors route (ChannelAuthorV1) would give this reader, whether
// or not they hold see_member_directory, and it is omitted when that rule
// gives them no name. For a message posted during a guest visit it is the
// guest attribution the message carries (MessageV2.GuestAuthor), never the
// current name of a member the guest has since become.
//
// Text is the start of the message's body. It is omitted unless the
// credential's preview switch is on. It is omitted then too for a spoiler,
// which is a message whose body contains "||" or that has an attachment
// whose stored name starts with SpoilerFilePrefix (decision 2 on #1116; the
// rule fails safe, so a body that only looks like a spoiler is withheld as
// well), and for a message with no text. The cut is the browser's: runs of
// whitespace become one space, the ends are trimmed, and a body longer than
// MaxNotificationTextChars characters is cut to its first 159 and "…", which
// is MaxNotificationTextChars in all.
type NotificationSummaryV1 struct {
	Schema      string           `json:"schema"`
	ChannelID   int64            `json:"channel_id"`
	ChannelName string           `json:"channel_name"`
	MessageID   int64            `json:"message_id"`
	PostedAt    Timestamp        `json:"posted_at"`
	Kind        NotificationKind `json:"kind"`
	AuthorID    int64            `json:"author_id"`
	AuthorName  string           `json:"author_name,omitempty"`
	Text        string           `json:"text,omitempty"`
}

// Validate reports whether the summary is well-formed: the schema is
// conch.notification_summary.v1, the ids are positive, the channel has a
// name, posted_at is set, the kind is known, and text is at most
// MaxNotificationTextChars characters.
func (s NotificationSummaryV1) Validate() error {
	if s.Schema != NotificationSummarySchemaV1 {
		return fmt.Errorf("schema: notification summary schema must be %q, got %q", NotificationSummarySchemaV1, s.Schema)
	}
	if s.ChannelID <= 0 {
		return errors.New("schema: notification summary channel_id must be positive")
	}
	if s.ChannelName == "" {
		return errors.New("schema: notification summary channel_name must not be empty")
	}
	if s.MessageID <= 0 {
		return errors.New("schema: notification summary message_id must be positive")
	}
	if s.PostedAt.IsZero() {
		return errors.New("schema: notification summary posted_at must be set")
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("schema: notification summary kind %q is not one of message, mention, whisper", s.Kind)
	}
	if s.AuthorID <= 0 {
		return errors.New("schema: notification summary author_id must be positive")
	}
	if utf8.RuneCountInString(s.Text) > MaxNotificationTextChars {
		return fmt.Errorf("schema: notification summary text must be at most %d characters", MaxNotificationTextChars)
	}
	return nil
}

// ListPendingNotificationsResponseV1 is the response to GET
// /v1/notifications/pending?after=<cursor> (§4.3): one page of what is new
// for the calling credential's reader. The request names no registration;
// the credential scopes it.
//
// Summaries is never null, is ascending by message_id, and holds at most
// MaxPendingNotificationsPage.
//
// The request's after is optional and defaults to 0: a non-negative int64.
// One that is malformed or negative is refused with invalid_request.
//
// A candidate is a message above after that this reader may read: it passes
// the audience predicate and is in a channel the reader is in now. The
// server examines a bounded number of candidates per request, about 2,000,
// and the bound counts candidates only. A message the reader cannot see is
// never examined, never counted and never moves the cursor.
//
// One residual is not a candidate bound: the server passes over, without
// selecting them, the reader's own posts, removed messages and whispers not
// addressed to the reader, in the channels the reader is in, above their read
// mark. A request's latency grows with that volume. It tells the reader how
// much was said in whispers in their own channels, a timing form of the
// activity-volume oracle of docs/design/mobile-notifications.md section 4.2;
// it changes no content, cursor or more, and it concerns only channels the
// reader is already in. History the reader may not read (before their history
// bound) is not walked.
//
// Cursor is the id to pass as after in the next request. Every match at or
// below it has been returned, in this page or an earlier one, so a client
// stores it as its high-water mark once it has rendered the page. It is the
// id of the highest candidate examined for this answer, which may be above
// the last summary's and is never below it. When the bound is reached
// before the page is full, the cursor is the highest candidate examined, so
// a long run of messages that do not match costs a client nothing and never
// holds it in place. When there was no candidate above after, the cursor is
// after itself. So the cursor is always the id of a message the reader may
// read, or after: like SearchMessagesResponseV2.NextBefore, it is never a
// function of messages the caller cannot see, and says nothing about how
// many of those there are or where their ids lie.
//
// More is true when the server stopped before the newest candidate, because
// the page was full or the bound was reached: the client asks again with
// Cursor. It is false when every candidate up to now has been examined. It
// refers to the newest message this reader may read, never to the newest
// message on the server. When More is true the cursor is above after, so a
// client that asks again always makes progress.
//
// The single cursor assumes message ids are globally monotonic: one counter
// across every channel, so that "above the cursor" means "newer" whichever
// channel a message is in. That holds today, because every message is a row
// of one messages table with an integer primary key
// (internal/server/store/store.go). A scheme with per-channel ids would
// break it, and would owe this route a cursor per channel.
type ListPendingNotificationsResponseV1 struct {
	Summaries []NotificationSummaryV1 `json:"summaries"`
	Cursor    int64                   `json:"cursor"`
	More      bool                    `json:"more"`
}

// Validate reports whether the page is well-formed: summaries is a list, not
// null, of at most MaxPendingNotificationsPage valid summaries in strictly
// ascending message_id, and cursor is not negative, not below the last
// summary's message_id, and positive when more is true (it is then the id
// of a message, above the request's after).
func (r ListPendingNotificationsResponseV1) Validate() error {
	if r.Summaries == nil {
		return errors.New("schema: pending notifications summaries must be a list, not null")
	}
	if len(r.Summaries) > MaxPendingNotificationsPage {
		return fmt.Errorf("schema: pending notifications summaries must be at most %d", MaxPendingNotificationsPage)
	}
	if r.Cursor < 0 {
		return errors.New("schema: pending notifications cursor must not be negative")
	}
	last := int64(0)
	for i, s := range r.Summaries {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("schema: pending notifications summaries[%d]: %w", i, err)
		}
		if s.MessageID <= last {
			return fmt.Errorf("schema: pending notifications summaries[%d] is not in ascending message_id", i)
		}
		last = s.MessageID
	}
	if r.Cursor < last {
		return errors.New("schema: pending notifications cursor must not be below the last summary's message_id")
	}
	if r.More && r.Cursor == 0 {
		return errors.New("schema: pending notifications cursor must be a message id when more is true")
	}
	return nil
}
