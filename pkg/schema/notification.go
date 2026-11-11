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
		return fmt.Errorf("schema: notification preferences policy %q is not one of off, mentions, all", policy)
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
type NotificationKind string

const (
	// NotificationKindMessage: a message, under the policy "all".
	NotificationKindMessage NotificationKind = "message"
	// NotificationKindMention: the body mentions the reader (Mentions).
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
// AuthorName is omitted when the reader may not be told who wrote it (the
// channel hides its members and the routes' author predicate says no).
// Text is the start of the message, at most MaxNotificationTextChars
// characters. It is omitted unless the credential's preview switch is on,
// and omitted then too when the server will not excerpt the message (a
// spoiler, or a message with no text).
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
// Cursor is the id to pass as after in the next request. Every match at or
// below it has been returned, in this page or an earlier one, so a client
// stores it as its high-water mark once it has rendered the page. It is
// the highest message id the server examined for this answer, which may be
// above the last summary's and is never below it. The server examines a
// bounded number of candidates per request, about 2,000. When it reaches
// that bound before the page is full, the cursor is the highest id it
// examined, so a long run of messages that do not match costs a client
// nothing and never holds it in place. When there was nothing above after
// to examine, the cursor is after itself.
//
// More is true when the server stopped before the newest message, because
// the page was full or the bound was reached: the client asks again with
// Cursor. It is false when everything up to now has been examined.
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
// ascending message_id, and cursor is not negative and not below the last
// summary's message_id.
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
	return nil
}
