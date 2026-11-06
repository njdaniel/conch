package schema

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Message reactions (issue #953, docs/design/message-reactions.md §3 and §5):
// the reaction key, its canonical form and bounds, and the wire shapes of the
// reaction routes, the four MCP tools and the events-socket frame. All of it
// is additive: new types, constants and error codes, no change to a published
// type. ReactionSummaryV1 carries a schema field because it travels on the
// events socket; the REST-only bodies do not, as ChannelMemberV1 does not.

// ReactionSummarySchemaV1 names the v1 reaction summary: every reaction on
// one message, aggregated, as the caller may see it. It is the body of the
// reaction reads and writes and the frame the events socket sends.
const ReactionSummarySchemaV1 = "conch.reaction_summary.v1"

const (
	// MaxReactionKeyBytes and MaxReactionKeyCodePoints bound a reaction key
	// before any lookup (§3.3). The longest listed sequences (family ZWJ
	// sequences, subdivision flags) are at most 10 code points and under 36
	// bytes; the headroom is for a future Unicode version. Clients refuse
	// the same inputs before sending.
	MaxReactionKeyBytes      = 64
	MaxReactionKeyCodePoints = 16

	// MaxReactionKindsPerMessage is the write limit on distinct keys a
	// message holds (§7.3). It may be raised or lowered later without
	// touching the wire: Validate does not use it.
	MaxReactionKindsPerMessage = 20
	// MaxReactionsPerPrincipalPerMessage is the write limit on one
	// principal's reactions to one message (§7.3). Not a wire bound either.
	MaxReactionsPerPrincipalPerMessage = 10

	// MaxReactionKindsOnWire is the published bound a ReactionSummaryV1 is
	// validated against. It is fixed for v1: a message that already holds
	// more kinds than a later, lower write limit must still encode, or the
	// events sockets that carry it would close. Changing it is a breaking
	// change (a v2).
	MaxReactionKindsOnWire = 100

	// MaxReactionBatchMessages is the most message ids one batch read
	// (GET /v2/channels/{channel}/reactions, read_reactions) may name.
	MaxReactionBatchMessages = 100

	// DefaultReactorLimit and MaxReactorLimit are the page size of the
	// reactor list when the caller gives none, and the most it may ask for.
	DefaultReactorLimit = 50
	MaxReactorLimit     = 100
)

// Error codes of the reaction routes and tools (§4.3), additive on Error.
const (
	// ErrorCodeMessageNotFound: the message does not exist, is in another
	// channel, or is not visible to the caller; one answer for all three
	// (HTTP 404).
	ErrorCodeMessageNotFound = "message_not_found"
	// ErrorCodeInvalidReaction: the emoji is not valid UTF-8, is not one
	// canonical reaction key (CanonicalReactionKey) or exceeds the bounds
	// (HTTP 400).
	ErrorCodeInvalidReaction = "invalid_reaction"
	// ErrorCodeReactionNotPermitted: the caller can see the message but may
	// not react to it: no current member seat on the net it was said on, or
	// a removed message (HTTP 403). An agent lacking the audience permission
	// gets the existing forbidden instead.
	ErrorCodeReactionNotPermitted = "reaction_not_permitted"
	// ErrorCodeTooManyReactions: the write would exceed
	// MaxReactionKindsPerMessage or MaxReactionsPerPrincipalPerMessage;
	// nothing stored (HTTP 409).
	ErrorCodeTooManyReactions = "too_many_reactions"
	// ErrorCodeReactionsRequireAuth: conchd runs with authentication off, so
	// there is no principal to hold a reaction (HTTP 400).
	ErrorCodeReactionsRequireAuth = "reactions_require_auth"
)

// ErrInvalidReaction is what every refusal of CanonicalReactionKey and
// ValidateReactionKey wraps; the server answers it as
// ErrorCodeInvalidReaction. The wrapped message says which rule refused.
var ErrInvalidReaction = errors.New("schema: invalid reaction")

// CanonicalReactionKey returns the canonical key for s (§3.2): the
// fully-qualified sequence of the one emoji s is, exactly as emoji-test.txt
// lists it. In order: s must be valid UTF-8; it must be within
// MaxReactionKeyBytes and MaxReactionKeyCodePoints; every U+FE0F and U+FE0E
// in it is removed, wherever it occurs, so a stray or missing presentation
// selector canonicalizes to the listed key; and the remainder must be one
// catalog sequence with its selectors removed. There is no other
// normalization, and no pre-filter on character class: a format or control
// character is accepted exactly where a listed sequence has it and refused
// everywhere else, by the failed lookup. Components alone (a skin tone, a
// hair component), text, shortcodes, an empty string and two emoji in a row
// are all refused. Every error wraps ErrInvalidReaction.
func CanonicalReactionKey(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrInvalidReaction)
	}
	if len(s) > MaxReactionKeyBytes {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidReaction, MaxReactionKeyBytes)
	}
	if utf8.RuneCountInString(s) > MaxReactionKeyCodePoints {
		return "", fmt.Errorf("%w: more than %d code points", ErrInvalidReaction, MaxReactionKeyCodePoints)
	}
	key, ok := emojiByStripped[stripPresentationSelectors(s)]
	if !ok {
		return "", fmt.Errorf("%w: not one emoji of Unicode Emoji %s", ErrInvalidReaction, EmojiVersion)
	}
	return key, nil
}

// ValidateReactionKey reports whether key is already canonical: an input
// CanonicalReactionKey accepts and returns unchanged. A summary, a record or
// a fixture carrying a non-canonical key fails validation.
func ValidateReactionKey(key string) error {
	canonical, err := CanonicalReactionKey(key)
	if err != nil {
		return err
	}
	if canonical != key {
		return fmt.Errorf("%w: not the canonical key", ErrInvalidReaction)
	}
	return nil
}

// ReactionCountV1 is one emoji on one message: how many principals the
// caller may know of hold it, and whether the caller is one of them.
type ReactionCountV1 struct {
	// Emoji is the canonical key.
	Emoji string `json:"emoji"`
	// Count is the number of durable records with this key on the message;
	// at least 1 (a key nobody holds is not listed).
	Count int `json:"count"`
	// Me is whether the caller holds this reaction. Omitted when false.
	Me bool `json:"me,omitempty"`
}

// Validate reports whether the count is well-formed: a canonical key held at
// least once.
func (c ReactionCountV1) Validate() error {
	if err := ValidateReactionKey(c.Emoji); err != nil {
		return fmt.Errorf("reaction emoji %q: %w", c.Emoji, err)
	}
	if c.Count < 1 {
		return errors.New("schema: reaction count must be at least 1")
	}
	return nil
}

// ReactionSummaryV1 is the whole reaction state of one message for one
// reader. Reactions is always a JSON array (empty when none), ordered by the
// time of the earliest reaction still held with that key, then by key, so
// the order a client shows is stable while counts grow; a key may move when
// its earliest reaction is removed.
type ReactionSummaryV1 struct {
	// Schema is the document discriminator; always ReactionSummarySchemaV1.
	Schema    string            `json:"schema"`
	ChannelID int64             `json:"channel_id"`
	MessageID int64             `json:"message_id"`
	Reactions []ReactionCountV1 `json:"reactions"`
	// AsOf is the server time the summary was read. A client keeps the
	// latest it has seen for a message (§8.3).
	AsOf Timestamp `json:"as_of"`
}

// Validate reports whether the summary is well-formed: the schema name,
// positive ids, a non-nil Reactions of at most MaxReactionKindsOnWire
// entries (never the live write limit), each valid with the keys distinct,
// and a non-zero AsOf.
func (s ReactionSummaryV1) Validate() error {
	if s.Schema != ReactionSummarySchemaV1 {
		return fmt.Errorf("schema: reaction summary schema must be %q, got %q", ReactionSummarySchemaV1, s.Schema)
	}
	if s.ChannelID <= 0 {
		return errors.New("schema: reaction summary channel_id must be positive")
	}
	if s.MessageID <= 0 {
		return errors.New("schema: reaction summary message_id must be positive")
	}
	if s.Reactions == nil {
		return errors.New("schema: reaction summary reactions must be an array, not null")
	}
	if len(s.Reactions) > MaxReactionKindsOnWire {
		return fmt.Errorf("schema: reaction summary holds %d kinds, at most %d", len(s.Reactions), MaxReactionKindsOnWire)
	}
	seen := make(map[string]struct{}, len(s.Reactions))
	for _, r := range s.Reactions {
		if err := r.Validate(); err != nil {
			return err
		}
		if _, dup := seen[r.Emoji]; dup {
			return fmt.Errorf("schema: reaction summary lists %q twice", r.Emoji)
		}
		seen[r.Emoji] = struct{}{}
	}
	if s.AsOf.IsZero() {
		return errors.New("schema: reaction summary as_of is required")
	}
	return nil
}

// ReactionSummariesResponseV1 is the body of the batch read: one summary per
// requested message the caller may see that has at least one reaction,
// ordered by message_id. Always a JSON array. An id the caller named that is
// absent (invisible, unknown or unreacted) is an empty summary to the client.
type ReactionSummariesResponseV1 struct {
	Summaries []ReactionSummaryV1 `json:"summaries"`
}

// Validate reports whether the response is well-formed: a non-nil array of
// valid summaries in strictly ascending message_id order.
func (r ReactionSummariesResponseV1) Validate() error {
	if r.Summaries == nil {
		return errors.New("schema: reaction summaries must be an array, not null")
	}
	for i, s := range r.Summaries {
		if err := s.Validate(); err != nil {
			return err
		}
		if i > 0 && s.MessageID <= r.Summaries[i-1].MessageID {
			return errors.New("schema: reaction summaries must be in ascending message_id order, without repeats")
		}
	}
	return nil
}

// ReactionV1 is one record: who reacted with one emoji, and when. The
// reactor is named by id only, as a message names its author: a client
// resolves principal_id the way it resolves author_id (the member list, or
// hidden-<id> for a principal it cannot resolve), so a former member's
// current name is never carried here. GuestReactor is the attribution the
// reactor had when it reacted, stored with the record and never changed
// (guests-and-accounts.md §4.4), and is what renders a guest's reaction.
type ReactionV1 struct {
	PrincipalID  int64               `json:"principal_id"`
	Emoji        string              `json:"emoji"`
	CreatedAt    Timestamp           `json:"created_at"`
	GuestReactor *GuestAttributionV1 `json:"guest_reactor,omitempty"`
}

// Validate reports whether the record is well-formed: a positive principal,
// a canonical key, a non-zero time, and a valid attribution when present.
func (r ReactionV1) Validate() error {
	if r.PrincipalID <= 0 {
		return errors.New("schema: reaction principal_id must be positive")
	}
	if err := ValidateReactionKey(r.Emoji); err != nil {
		return fmt.Errorf("reaction emoji %q: %w", r.Emoji, err)
	}
	if r.CreatedAt.IsZero() {
		return errors.New("schema: reaction created_at is required")
	}
	if r.GuestReactor != nil {
		if err := r.GuestReactor.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ListReactorsResponseV1 is one page of the principals holding one emoji on
// one message, ordered by principal_id ascending. NextAfter is the id to pass
// as the next request's after: the last principal_id of this page; zero
// (omitted) means no known further page.
type ListReactorsResponseV1 struct {
	Reactors  []ReactionV1 `json:"reactors"`
	NextAfter int64        `json:"next_after,omitempty"`
}

// Validate reports whether the page is well-formed: a non-nil array of valid
// records, all with one emoji, in strictly ascending principal_id order, and
// a NextAfter that is zero or the last record's principal_id.
func (l ListReactorsResponseV1) Validate() error {
	if l.Reactors == nil {
		return errors.New("schema: reactors must be an array, not null")
	}
	for i, r := range l.Reactors {
		if err := r.Validate(); err != nil {
			return err
		}
		if i == 0 {
			continue
		}
		if r.Emoji != l.Reactors[0].Emoji {
			return errors.New("schema: a reactor page lists one emoji")
		}
		if r.PrincipalID <= l.Reactors[i-1].PrincipalID {
			return errors.New("schema: reactors must be in ascending principal_id order, without repeats")
		}
	}
	if l.NextAfter < 0 {
		return errors.New("schema: reactors next_after must not be negative")
	}
	if l.NextAfter != 0 && (len(l.Reactors) == 0 || l.NextAfter != l.Reactors[len(l.Reactors)-1].PrincipalID) {
		return errors.New("schema: reactors next_after must be the last principal_id of the page")
	}
	return nil
}
