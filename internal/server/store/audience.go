package store

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Scoped messages (issue #116, ADR-005).
//
// A message is either channel-wide (audience_kind NULL, no recipient rows) or
// scoped to a net or to an explicit list of principals. Who a scoped message
// reached is decided once, at post time, and stored in message_recipients.
//
// This file is the only place that reads message rows. ListVisibleMessagesPage
// (and ListVisibleMessages, its ascending form) is the one visibility function:
// every list in every protocol version, and the MCP read tool, go through it, so there is exactly one place where a scoped
// message can be handed to someone it was not meant for. A structural test
// (TestMessageReadsStayInTheVisibilityFunction) fails if another file in the
// server reads the messages table.

// AuditMessageScoped is written, in the insert transaction, for every scoped
// post. Its subject is "message:<id>". The detail names the channel, the
// audience kind, the net where there is one, and the resolved recipient ids;
// it never contains the message body.
const AuditMessageScoped = "message_scoped"

var (
	// ErrNetNotFound is returned by InsertScopedMessage when the net does not
	// exist in the channel, is archived, or the author is not on it. These are
	// deliberately one error: a caller who is not on a net must not be able to
	// tell it exists.
	ErrNetNotFound = errors.New("store: net not found")
	// ErrNetMonitorOnly is returned when the author is on the net as a monitor,
	// who listens but may not transmit.
	ErrNetMonitorOnly = errors.New("store: a net monitor may not transmit")
	// ErrInvalidAudience is returned when a principals audience is malformed or
	// names a principal who is not a current member of the channel.
	ErrInvalidAudience = errors.New("store: invalid audience")
)

// Reader describes who is reading, for ListVisibleMessages.
type Reader struct {
	// PrincipalID is the verified, authenticated reader, or 0 when there is
	// none (authentication off, or an unidentified connection).
	PrincipalID int64
	// Scoped says the reader's protocol can express an audience (v2). A v0 or
	// v1 reader cannot: it must never be given a scoped message, because it
	// would render a whisper as an open message.
	Scoped bool
}

// ChannelWideOnly is the reader for every path that cannot express an
// audience or has no verified identity: it sees channel-wide messages only.
var ChannelWideOnly = Reader{}

// ListVisibleMessages returns up to limit messages in channelID with ID
// greater than afterID that reader may see, in ascending ID order. Pass
// afterID = 0 to start from the beginning, or the last returned ID to fetch
// the next page. Hidden messages are filtered before the limit applies, so a
// page is full of visible messages and a cursor never stalls on one.
//
// The rule, in full:
//
//   - A channel-wide message is visible to every reader. Whether the reader may
//     use the channel at all is the caller's check (the handlers' membership
//     gate), as it always was, and is not repeated here because with
//     authentication off there is no reader to check.
//   - A scoped message is visible only to a Scoped reader with a verified
//     PrincipalID who has a recipient row for it AND is, now, a member of the
//     channel AND is an enabled principal. A recipient row on its own is never
//     enough: removing a principal from the channel hides everything scoped
//     from them, including what they received earlier, and disabling a
//     principal or revoking its credentials keeps its rows and seats on
//     purpose so that re-enabling restores them, which is only safe because
//     this predicate also requires the principal to be enabled.
//   - Operators get no exemption. Oversight of scoped traffic is the audit
//     log, not a read bypass.
//   - Every reader with a verified PrincipalID, whatever its protocol, is
//     also held to its history bound in the channel (issue #719, ADR-009,
//     docs/design/guests-and-accounts.md §4.3): when its membership carries
//     one (history_after, set at a guest's admission and kept through
//     conversion), only messages after it are visible, and a reply only when
//     its thread's root is after it too, so an old thread is not read through
//     its new replies. A guest whose membership has no bound sees nothing of
//     the channel: an object whose permitted provenance cannot be shown is
//     refused, never assumed. A member with no bound is not affected.
func (s *Store) ListVisibleMessages(ctx context.Context, channelID int64, reader Reader, afterID int64, limit int) ([]Message, error) {
	return s.ListVisibleMessagesPage(ctx, channelID, reader, MessagePage{After: afterID}, limit)
}

// MessagePage says which messages ListVisibleMessagesPage reads. The zero
// value is the first page, ascending. After is the largest id already held
// (ascending reads); Before is the smallest (newest-first reads, where zero
// means from the newest). A page has one direction, so After with NewestFirst,
// or Before without it, is an error rather than a guess.
type MessagePage struct {
	After, Before int64
	NewestFirst   bool
}

// ListVisibleMessagesPage is ListVisibleMessages with a direction: up to limit
// visible messages of channelID, ascending with id greater than page.After, or
// with page.NewestFirst descending with id less than page.Before (all of them
// when it is zero). The visibility rule is the one documented above and is in
// the same statement, before the limit, in either direction.
func (s *Store) ListVisibleMessagesPage(ctx context.Context, channelID int64, reader Reader, page MessagePage, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list messages: limit must be positive, got %d", limit)
	}
	if (page.NewestFirst && page.After != 0) || (!page.NewestFirst && page.Before != 0) {
		return nil, errors.New("store: list messages: after goes with ascending reads and before with newest-first ones")
	}
	if page.After < 0 || page.Before < 0 {
		return nil, errors.New("store: list messages: a cursor is never negative")
	}
	before := page.Before
	if before == 0 {
		before = math.MaxInt64
	}
	scoped, predArgs := visibilityArgs(reader)
	rows, err := s.db.QueryContext(ctx, listSQL(scoped, page.NewestFirst),
		append(append([]any{channelID, page.After, before}, predArgs...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()

	msgs, err := s.scanMessages(ctx, rows)
	if err != nil {
		return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
	}
	return msgs, nil
}

// CountVisibleMessagesAfter returns how many messages in channelID with ID
// greater than afterID reader may see, counting no further than limit (so a
// result equal to limit means "at least that many"). It embeds the same
// visibility condition as
// ListVisibleMessages, in the same statement, so a message the reader cannot
// see is not counted, and a count is never a way to learn that a hidden
// message exists. The cap is applied inside the statement, so the work is
// bounded by it however long the channel has gone unread.
func (s *Store) CountVisibleMessagesAfter(ctx context.Context, channelID int64, reader Reader, afterID int64, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("store: count messages: limit must be positive, got %d", limit)
	}
	if afterID < 0 {
		return 0, errors.New("store: count messages: a cursor is never negative")
	}
	scoped, predArgs := visibilityArgs(reader)
	var n int
	err := s.db.QueryRowContext(ctx, countSQL(scoped),
		append(append([]any{channelID, afterID}, predArgs...), limit)...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count messages in channel %d: %w", channelID, err)
	}
	return n, nil
}

// CountVisibleUnreadMessagesAfter is CountVisibleMessagesAfter for an unread badge:
// the reader's own posts are not unread to them, while every other visibility
// and history-bound rule is identical.
func (s *Store) CountVisibleUnreadMessagesAfter(ctx context.Context, channelID int64, reader Reader, afterID int64, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("store: count messages: limit must be positive, got %d", limit)
	}
	if afterID < 0 {
		return 0, errors.New("store: count messages: a cursor is never negative")
	}
	scoped, predArgs := visibilityArgs(reader)
	var n int
	err := s.db.QueryRowContext(ctx, unreadSQL(scoped),
		append(append([]any{channelID, afterID, reader.PrincipalID}, predArgs...), limit)...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count unread messages in channel %d: %w", channelID, err)
	}
	return n, nil
}

// newestVisibleMessageID returns the id of the newest message in channelID
// that reader may see, or 0 when there is none. MarkRead clamps a mark to it,
// so a mark can never point past what the reader has seen, nor be used to
// probe for the ids of messages they cannot see.
func (s *Store) newestVisibleMessageID(ctx context.Context, channelID int64, reader Reader) (int64, error) {
	scoped, predArgs := visibilityArgs(reader)
	var id int64
	err := s.db.QueryRowContext(ctx, newestSQL(scoped), append([]any{channelID}, predArgs...)...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: newest message in channel %d: %w", channelID, err)
	}
	return id, nil
}

// ChannelAuthor is one author named by ListChannelAuthors.
type ChannelAuthor struct {
	PrincipalID int64
	Name        string
	Kind        PrincipalKind
}

// ListChannelAuthors answers, for each of ids, whether that principal
// authored at least one message in channelID that reader may read, and names
// the ones that did, ordered by id (design §13.5). "May read" is
// messageVisible, the condition every message read here embeds, in the same
// statement: a whisper the reader was not sent, or a message before the
// reader's history bound, does not make its author known. An id that
// authored nothing the reader may read is absent, whether or not the
// principal exists, so the answer discloses only what a visible message
// already disclosed to the channel. Authors may be departed, disabled or
// ended guests: the message stays attributed. At most MaxChannelAuthorIDs
// distinct positive ids are asked; more is an error.
func (s *Store) ListChannelAuthors(ctx context.Context, channelID int64, reader Reader, ids []int64) ([]ChannelAuthor, error) {
	distinct := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			distinct[id] = struct{}{}
		}
	}
	if len(distinct) > schema.MaxChannelAuthorIDs {
		return nil, fmt.Errorf("store: list channel authors: %d ids asked, at most %d", len(distinct), schema.MaxChannelAuthorIDs)
	}
	if len(distinct) == 0 {
		return []ChannelAuthor{}, nil
	}
	scoped, predArgs := visibilityArgs(reader)
	args := make([]any, 0, len(distinct)+1+len(predArgs))
	marks := make([]string, 0, len(distinct))
	for id := range distinct {
		marks = append(marks, "?")
		args = append(args, id)
	}
	args = append(append(args, channelID), predArgs...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.name, p.kind FROM principals p
		 WHERE p.id IN (`+strings.Join(marks, ",")+`)
		   AND EXISTS (SELECT 1 FROM messages m
		               WHERE m.channel_id = ? AND m.author_id = p.id AND `+messageVisible(scoped)+`)
		 ORDER BY p.id`, // #nosec G202 -- only "?" marks and constant conditions are joined; every value is an argument
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: list authors of channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []ChannelAuthor{}
	for rows.Next() {
		var a ChannelAuthor
		var kind string
		if err := rows.Scan(&a.PrincipalID, &a.Name, &kind); err != nil {
			return nil, fmt.Errorf("store: list authors of channel %d: %w", channelID, err)
		}
		a.Kind = PrincipalKind(kind)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list authors of channel %d: %w", channelID, err)
	}
	return out, nil
}

// AuthorReaders is who holds a message of authorID that the authors route
// would let them name (issue #1282), for the hide signal: the ids of the
// humans, enabled and not hidden, who are a member of a channel where authorID
// authored a message that they may read. "May read" is messageVisible with
// the same scoped reader ListChannelAuthors is asked with, so the answer is
// exactly the people for whom GET /v1/channels/{channel}/authors names the
// author, and a frame to them tells nobody anything the message had not
// already told them. An agent is never listed (it has no directory), nor is
// anyone with no such message, whatever they know of the author otherwise.
// Called after the author was hidden: their messages stay, their memberships
// are gone.
func (s *Store) AuthorReaders(ctx context.Context, authorID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT v.id FROM principals v
		 JOIN `+liveMembers+` cm ON cm.principal_id = v.id
		 WHERE v.kind = 'human' AND v.disabled_at IS NULL AND v.hidden_at IS NULL
		   AND v.id <> ?
		   AND EXISTS (SELECT 1 FROM messages x WHERE x.channel_id = cm.channel_id AND x.author_id = ?)
		 ORDER BY v.id`, authorID, authorID)
	if err != nil {
		return nil, fmt.Errorf("store: readers of author %d: %w", authorID, err)
	}
	candidates, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("store: readers of author %d: %w", authorID, err)
	}
	var out []int64
	for _, id := range candidates {
		scoped, predArgs := visibilityArgs(Reader{Scoped: true, PrincipalID: id})
		args := append([]any{id, authorID}, predArgs...)
		var ok bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM messages m
			                JOIN `+liveMembers+` rc ON rc.channel_id = m.channel_id AND rc.principal_id = ?
			                WHERE m.author_id = ? AND `+messageVisible(scoped)+`)`, // #nosec G202 -- constant conditions only
			args...).Scan(&ok); err != nil {
			return nil, fmt.Errorf("store: readers of author %d: %w", authorID, err)
		}
		if ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// scanIDs reads a single integer column and closes rows.
func scanIDs(rows *sql.Rows) ([]int64, error) {
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// historyWatermarkTx is the history bound a guest admitted to channelID now
// gets (issue #705): the id of the channel's last message, or 0 when it has
// none. Message ids only grow, so every later message of the channel has a
// greater one. It reads no message, only the largest id, and lives here
// because this file is the one that reads the messages table.
func historyWatermarkTx(ctx context.Context, tx execer, channelID int64) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(id), 0) FROM messages WHERE channel_id = ?", channelID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: history watermark of channel %d: %w", channelID, err)
	}
	return id, nil
}

// removedCond is true, over messages aliased m, for a message a moderator
// removed (docs/design/moderation.md section 7). It has no arguments.
const removedCond = `EXISTS (SELECT 1 FROM message_removals mrm WHERE mrm.message_id = m.id)`

// notRemovedCond is the negation of removedCond, for the statements that must
// not select a removed message at all (search, a new reply to a root).
const notRemovedCond = `NOT ` + removedCond

// messageColumns is the column list scanMessages reads. A removed message is
// projected as a tombstone in the statement itself: its body, payload and
// edit time are never selected, so no reader of these rows can hold them. The
// last column says which rows are tombstones.
const messageColumns = `m.id, m.channel_id, m.author_id,
	CASE WHEN ` + removedCond + ` THEN '' ELSE m.body END,
	CASE WHEN ` + removedCond + ` THEN NULL ELSE m.payload_schema END,
	CASE WHEN ` + removedCond + ` THEN NULL ELSE m.payload_json END,
	m.created_at, m.audience_kind, m.net_id, m.thread_root_id,
	(SELECT g.handle FROM message_guest_attribution g WHERE g.message_id = m.id),
	(SELECT g.label FROM message_guest_attribution g WHERE g.message_id = m.id),
	CASE WHEN ` + removedCond + ` THEN NULL ELSE m.edited_at END,
	` + removedCond

// The visibility rule for which messages a reader may see, as SQL conditions
// over the messages table aliased m. The list and the search both embed the
// same condition (messageVisible) in their own statement, so a message is
// filtered in the statement that selects it and the two cannot disagree. See
// ListVisibleMessages for the rule in words. The statements are built from
// constants only (never from a caller's text), so every value is a bound
// parameter.
const (
	// visibleToScopedReader takes the reader's principal id three times.
	visibleToScopedReader = `(m.audience_kind IS NULL OR (
			        EXISTS (SELECT 1 FROM message_recipients r
			                WHERE r.message_id = m.id AND r.principal_id = ?)
			    AND EXISTS (SELECT 1 FROM ` + liveMembers + ` c
			                WHERE c.channel_id = m.channel_id AND c.principal_id = ?)
			    AND EXISTS (SELECT 1 FROM principals p
			                WHERE p.id = ? AND p.disabled_at IS NULL)))`
	visibleToAnyReader = `m.audience_kind IS NULL`
	// withinHistoryBound is the reader's history bound (issue #719), and
	// takes the reader's principal id three times (0 for no reader, who has
	// no bound). A message is within it when the reader is not a guest now
	// and no membership bound of theirs in the message's channel is at or
	// after it, or when such a bound exists and is before it. "It" is the
	// thread's root for a reply (a root's id is always below its replies'),
	// so a new reply does not carry an old thread past the bound. A NULL
	// bound compares as neither, so a guest without one sees nothing. The
	// aliases (hg, hb, ha) are this condition's own. It is not a membership
	// check: for a banned non-guest it is satisfiable, so it must stay behind
	// one (isChannelMemberTx, the handlers' channel gate).
	withinHistoryBound = `((NOT EXISTS (SELECT 1 FROM guest_visits hg
			                    WHERE hg.principal_id = ? AND hg.state = 'active')
			    AND NOT EXISTS (SELECT 1 FROM ` + liveMembers + ` hb
			                    WHERE hb.channel_id = m.channel_id AND hb.principal_id = ?
			                      AND COALESCE(m.thread_root_id, m.id) <= hb.history_after))
			 OR EXISTS (SELECT 1 FROM ` + liveMembers + ` ha
			            WHERE ha.channel_id = m.channel_id AND ha.principal_id = ?
			              AND COALESCE(m.thread_root_id, m.id) > ha.history_after))`

	listHead = `SELECT ` + messageColumns + ` FROM messages m
		 WHERE m.channel_id = ? AND m.id > ? AND m.id < ? AND `
	listTail = `
		 ORDER BY m.id ASC LIMIT ?`
	// The newest-first read is the same statement walked backwards: both
	// bounds are always written so the planner sees one range on the
	// channel index, and only the direction differs.
	listDescTail = `
		 ORDER BY m.id DESC LIMIT ?`

	countHead = `SELECT COUNT(*) FROM (SELECT 1 FROM messages m
		 WHERE m.channel_id = ? AND m.id > ? AND `
	countTail = `
		 LIMIT ?)`
	newestHead = `SELECT m.id FROM messages m
		 WHERE m.channel_id = ? AND `
	// Newest first and stopping at the first visible row, so the cost is the
	// hidden messages at the top of the channel and not the whole channel.
	newestTail = `
		 ORDER BY m.id DESC LIMIT 1`

	searchHead = `SELECT ` + messageColumns + ` FROM messages m
		 WHERE m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)
		   AND m.channel_id = ? AND (? = 0 OR m.id < ?) AND ` + notRemovedCond + ` AND `
	searchTail = `
		 ORDER BY m.id DESC LIMIT ?`
)

// visibilityArgs reports whether reader is a scoped reader with an identity
// (which selects visibleToScopedReader) and the arguments of messageVisible:
// the audience condition's, then the history bound's, which every reader is
// held to.
func visibilityArgs(reader Reader) (scoped bool, args []any) {
	bound := boundReader(reader)
	if reader.Scoped && reader.PrincipalID > 0 {
		return true, []any{reader.PrincipalID, reader.PrincipalID, reader.PrincipalID, bound, bound, bound}
	}
	return false, []any{bound, bound, bound}
}

// boundReader is the principal id the history bound is asked for: the
// verified reader, or 0 when there is none (no guest and no membership has
// that id, so the bound is then no restriction).
func boundReader(reader Reader) int64 {
	return max(reader.PrincipalID, 0)
}

// messageVisible is the whole message condition: the audience rule for the
// reader's protocol, and the history bound. It is the one condition every
// statement here embeds, for messages and for what is read through them.
func messageVisible(scoped bool) string {
	if scoped {
		return visibleToScopedReader + ` AND ` + withinHistoryBound
	}
	return visibleToAnyReader + ` AND ` + withinHistoryBound
}

func listSQL(scoped, newestFirst bool) string {
	tail := listTail
	if newestFirst {
		tail = listDescTail
	}
	return listHead + messageVisible(scoped) + tail
}

func countSQL(scoped bool) string {
	return countHead + messageVisible(scoped) + countTail
}

func unreadSQL(scoped bool) string {
	return countHead + `m.author_id <> ? AND ` + messageVisible(scoped) + countTail
}

func newestSQL(scoped bool) string {
	return newestHead + messageVisible(scoped) + newestTail
}

func searchSQL(scoped bool) string {
	return searchHead + messageVisible(scoped) + searchTail
}

// scanMessages reads the rows of a statement selecting messageColumns, and
// loads the audience of any whisper among them. It closes rows.
func (s *Store) scanMessages(ctx context.Context, rows *sql.Rows) ([]Message, error) {
	return scanMessagesWith(ctx, s.db, rows)
}

// scanMessagesWith is scanMessages reading the whisper audiences through q:
// the transaction a statement ran in, when it ran in one.
func scanMessagesWith(ctx context.Context, q execer, rows *sql.Rows) ([]Message, error) {
	var msgs []Message
	var whispers []int // indexes into msgs of principals-audience messages
	for rows.Next() {
		var m Message
		var payloadSchema, kind, guestHandle, guestLabel sql.NullString
		var payloadJSON []byte
		var netID, threadRoot, editedAt sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.AuthorID, &m.Body, &payloadSchema, &payloadJSON, &createdAt, &kind, &netID, &threadRoot,
			&guestHandle, &guestLabel, &editedAt, &m.Removed); err != nil {
			return nil, err
		}
		if m.Removed {
			// The tombstone (moderation.md section 7.3): the statement
			// selected no original, and this is the only body there is.
			m.Body = schema.MessageRemovedBody
			payloadSchema, payloadJSON, editedAt = sql.NullString{}, nil, sql.NullInt64{}
		}
		if editedAt.Valid {
			m.EditedAt = time.UnixMilli(editedAt.Int64)
		}
		if guestHandle.Valid {
			a := guestAttribution(guestHandle.String, guestLabel.String)
			m.GuestAuthor = &a
		}
		m.ThreadRootID = threadRoot.Int64 // zero when NULL
		m.CreatedAt = time.UnixMilli(createdAt)
		if payloadSchema.Valid {
			m.Payload = &schema.Payload{Schema: payloadSchema.String, Data: payloadJSON}
		}
		switch {
		case !kind.Valid:
		case schema.AudienceKind(kind.String) == schema.AudienceKindNet && netID.Valid:
			m.Audience = &schema.Audience{Kind: schema.AudienceKindNet, NetID: netID.Int64}
		case schema.AudienceKind(kind.String) == schema.AudienceKindPrincipals:
			m.Audience = &schema.Audience{Kind: schema.AudienceKindPrincipals}
			whispers = append(whispers, len(msgs))
		default:
			// Never fall back to treating an audience we cannot read as
			// channel-wide.
			return nil, fmt.Errorf("store: message %d has an unreadable audience %q", m.ID, kind.String)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return msgs, fillWhisperAudiences(ctx, q, msgs, whispers)
}

// SearchVisibleMessages returns up to limit messages in channelID whose body
// contains every one of words, that reader may see, newest first. Pass
// before = 0 to start from the newest, or the last returned ID to fetch the
// next page; only messages with a smaller ID are returned.
//
// It shows a reader exactly what ListVisibleMessages shows them: the match and
// the visibility predicate are conditions of one statement, so a message the
// reader cannot see is never selected, never counted against limit, and cannot
// move a page boundary. Filtering after the match, in Go, would make the page
// size and the next cursor depend on messages the reader may not know exist.
//
// words are plain words, never FTS5 syntax: each becomes one quoted FTS5
// string (a quote inside it doubled) and all of them are required, so nothing
// a caller types reaches FTS5's query language. A word the tokenizer reduces
// to nothing (for example "*") matches nothing. With no words the result is
// empty. Matching is case-insensitive on whole words (the unicode61 tokenizer).
func (s *Store) SearchVisibleMessages(ctx context.Context, channelID int64, reader Reader, words []string, before int64, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: search messages: limit must be positive, got %d", limit)
	}
	match := ftsInChannel(channelID, words)
	if match == "" {
		return nil, nil
	}
	scoped, predArgs := visibilityArgs(reader)
	// before = 0 means "no upper bound"; the bound is written so the statement
	// text does not change with it.
	rows, err := s.db.QueryContext(ctx, searchSQL(scoped),
		append(append([]any{match, channelID, before, before}, predArgs...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: search messages in channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	msgs, err := s.scanMessages(ctx, rows)
	if err != nil {
		return nil, fmt.Errorf("store: search messages in channel %d: %w", channelID, err)
	}
	return msgs, nil
}

// ftsInChannel builds the FTS5 query of a search: the channel's token, and
// every word, each confined to its own column. The channel clause is written
// from an integer; the words are ftsAllWords's. The words are confined to the
// body column so that a word a caller types can never match the channel token.
// It is empty when there is nothing to search for.
func ftsInChannel(channelID int64, words []string) string {
	all := ftsAllWords(words)
	if all == "" {
		return ""
	}
	return fmt.Sprintf(`fts_channel : "c%d" AND body : (%s)`, channelID, all)
}

// ftsAllWords builds an FTS5 query that requires every word, each as a quoted
// string. Inside an FTS5 string only the quote is special, and it is doubled.
// A NUL byte would end the string early in C, so it separates words as
// whitespace does.
func ftsAllWords(words []string) string {
	var b strings.Builder
	for _, w := range words {
		for _, part := range strings.Fields(strings.ReplaceAll(w, "\x00", " ")) {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(part, `"`, `""`))
			b.WriteByte('"')
		}
	}
	return b.String()
}

// fillWhisperAudiences loads the principal list of the whispers at msgs[idx].
// The list is the wire audience (a reader replies in kind by reusing it), so
// it is loaded only for principals audiences; a net message's recipients are
// never read back.
func fillWhisperAudiences(ctx context.Context, q execer, msgs []Message, idx []int) error {
	if len(idx) == 0 {
		return nil
	}
	byID := make(map[int64]*schema.Audience, len(idx))
	ids := make([]int64, 0, len(idx))
	for _, i := range idx {
		byID[msgs[i].ID] = msgs[i].Audience
		ids = append(ids, msgs[i].ID)
	}
	// One JSON array parameter instead of a built placeholder list, so the
	// statement text is constant.
	list, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("store: load whisper audiences: %w", err)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT message_id, principal_id FROM message_recipients
		 WHERE message_id IN (SELECT value FROM json_each(?))
		 ORDER BY message_id ASC, principal_id ASC`, string(list))
	if err != nil {
		return fmt.Errorf("store: load whisper audiences: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID, principalID int64
		if err := rows.Scan(&messageID, &principalID); err != nil {
			return fmt.Errorf("store: load whisper audiences: %w", err)
		}
		a := byID[messageID]
		a.PrincipalIDs = append(a.PrincipalIDs, principalID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: load whisper audiences: %w", err)
	}
	return nil
}

// FileVisibleTo returns the file fileID if reader may fetch it, and ErrNotFound
// otherwise. "Otherwise" is one answer for every reason: the file does not
// exist, it is in a channel the reader is not in, it is attached to a message
// the reader cannot see, or the reader is not the uploader of a file nobody
// has attached. The caller must answer all of them alike.
//
// A file is a second way to read what a message carries, so the rule is the
// message's, not a copy of it. The reader may fetch the file when they are,
// now, a member of the file's channel and an enabled principal, and either
//
//   - they uploaded it (attached or not) and have no history bound there, or
//   - the message it is attached to is visible to them by the same
//     condition (messageVisible) ListVisibleMessages
//     and SearchVisibleMessages embed: a whisper's file is readable by the
//     whisper's readers and nobody else, a net message's file by the net's
//     recipients, and a removed member loses every file they could reach
//     through a message.
//
// Channel membership and an enabled principal are required for the uploader as
// well: leaving the channel closes it to you, your uploads included. Operators
// get no exemption. A reader with no verified identity is never given a file.
//
// A reader held to a history bound in the file's channel (issue #719: a guest,
// or a membership with history_after) reaches a file only through its message,
// which the bound applies to like any other. The uploader's own way in is not
// theirs: a file nobody has attached has no message to place it after the
// bound, so its provenance cannot be shown and it is refused.
func (s *Store) FileVisibleTo(ctx context.Context, fileID int64, reader Reader) (File, error) {
	if reader.PrincipalID <= 0 {
		return File{}, ErrNotFound
	}
	scoped, predArgs := visibilityArgs(reader)
	p := reader.PrincipalID
	args := append([]any{fileID, p, p, p}, predArgs...)
	f, err := scanFile(s.db.QueryRowContext(ctx, fileSQL(scoped), args...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return File{}, ErrNotFound
	case err != nil:
		return File{}, fmt.Errorf("store: read file %d: %w", fileID, err)
	}
	return f, nil
}

// noHistoryBound is true when the reader is not a guest now and has no
// history bound in the channel of the row the statement selects (issue #719).
// It guards the ways to an object that do not go through a message, which
// the message condition's bound cannot place: a file's uploader and a run
// that answers no message. It takes the reader's principal id twice; the
// channel is the column the embedding statement names, never a caller's text.
func noHistoryBound(channelColumn string) string {
	return `(NOT EXISTS (SELECT 1 FROM guest_visits ng WHERE ng.principal_id = ? AND ng.state = 'active')
		     AND NOT EXISTS (SELECT 1 FROM ` + liveMembers + ` nb
		                     WHERE nb.channel_id = ` + channelColumn + ` AND nb.principal_id = ?
		                       AND nb.history_after IS NOT NULL))`
}

// fileHead and fileTail wrap the message visibility condition for a file. The
// condition is spliced in from constants (never a caller's text); every value
// is a bound parameter, in this order: file id, reader (member), reader
// (enabled), reader (uploader), then the predicate's own. The uploader reads
// their own file without the history bound: a file someone uploaded is never
// from before a bound they hold (a bound is set when they are admitted, and
// conversion keeps the same principal), so the bound has nothing to hide
// there, and a converted member can read their own upload before attaching
// it (#719 review).
var (
	fileHead = `SELECT ` + fileColumns + ` FROM files f
		 WHERE f.id = ?
		   AND NOT EXISTS (SELECT 1 FROM message_removals fr WHERE fr.message_id = f.message_id)
		   AND EXISTS (SELECT 1 FROM ` + liveMembers + ` cm
		               WHERE cm.channel_id = f.channel_id AND cm.principal_id = ?)
		   AND EXISTS (SELECT 1 FROM principals up
		               WHERE up.id = ? AND up.disabled_at IS NULL)
		   AND (f.uploader_id = ? OR EXISTS (
		         SELECT 1 FROM messages m
		          WHERE m.id = f.message_id AND m.channel_id = f.channel_id AND `
	fileTail = `))`
)

func fileSQL(scoped bool) string {
	return fileHead + messageVisible(scoped) + fileTail
}

// attachFilesTx honours a message's conch.file_attachment.v1 payload inside the
// transaction that inserts the message, whose id is messageID. A payload of any
// other schema (or none) attaches nothing. Each file must be in channelID,
// uploaded by authorID and not yet attached; the UPDATE matches only such a row,
// so the database, not a check before it, decides which of two concurrent posts
// wins a file: the transactions are IMMEDIATE and serialise, and the second one
// finds message_id already set. Any failure is ErrFileAttachment and the caller
// rolls the whole post back.
func attachFilesTx(ctx context.Context, tx execer, channelID, authorID, messageID int64, payload *schema.Payload, at time.Time) error {
	if payload == nil || payload.Schema != schema.FileAttachmentV1Name {
		return nil
	}
	v, _, err := payload.Decode()
	if err != nil {
		return fmt.Errorf("%w: payload is malformed", ErrFileAttachment)
	}
	att, ok := v.(*schema.FileAttachmentV1)
	if !ok || att.Validate() != nil {
		return fmt.Errorf("%w: payload is malformed", ErrFileAttachment)
	}
	for _, id := range att.FileIDs {
		res, err := tx.ExecContext(ctx,
			`UPDATE files SET message_id = ?
			 WHERE id = ? AND channel_id = ? AND uploader_id = ? AND message_id IS NULL`,
			messageID, id, channelID, authorID)
		if err != nil {
			return fmt.Errorf("store: attach file %d to message %d: %w", id, messageID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: attach file %d to message %d: %w", id, messageID, err)
		}
		if n != 1 {
			return fmt.Errorf("%w: a file is not available to attach", ErrFileAttachment)
		}
		if err := appendAuditEventTx(ctx, tx, principalActor(authorID), AuditFileAttached, fileSubject(id),
			fmt.Sprintf("message=%d", messageID), at); err != nil {
			return err
		}
	}
	return nil
}

// Threads (issue #25). A thread is a root message and the replies that name it,
// one level deep. A thread is not an audience of its own: a reply has the
// audience its root has, and the three functions below ask the visibility
// question with the same constants as the list, the search and the file check,
// in the same statement as the selection. Nothing is filtered in Go afterwards.

// ErrThreadNotFound is returned when a thread's root cannot be used by the
// reader: it does not exist, is in another channel, is itself a reply, or is
// not visible to the reader. These are deliberately one error, because message
// ids are sequential and any difference between them would let a reader probe
// for messages scoped away from them.
var ErrThreadNotFound = errors.New("store: thread root not found")

// rootFrom is the selection of a thread root: a message of the channel that is
// not itself a reply, and that the reader may see (the visibility condition is
// appended). Both the read and the check inside the insert transaction use it.
const rootFrom = ` FROM messages m
		 WHERE m.id = ? AND m.channel_id = ? AND m.thread_root_id IS NULL AND `

func rootSQL(columns string, scoped bool) string {
	return `SELECT ` + columns + rootFrom + messageVisible(scoped)
}

// ReplyRootMessage returns the message rootID of channelID if reader may reply
// to it: it exists in the channel, is not itself a reply and is visible to
// reader by the list's own rule. Every other case is ErrThreadNotFound.
func (s *Store) ReplyRootMessage(ctx context.Context, channelID int64, reader Reader, rootID int64) (Message, error) {
	scoped, predArgs := visibilityArgs(reader)
	rows, err := s.db.QueryContext(ctx, rootSQL(messageColumns, scoped),
		append([]any{rootID, channelID}, predArgs...)...)
	if err != nil {
		return Message{}, fmt.Errorf("store: read thread root %d: %w", rootID, err)
	}
	defer func() { _ = rows.Close() }()
	msgs, err := s.scanMessages(ctx, rows)
	if err != nil {
		return Message{}, fmt.Errorf("store: read thread root %d: %w", rootID, err)
	}
	if len(msgs) != 1 {
		return Message{}, ErrThreadNotFound
	}
	return msgs[0], nil
}

// ListThreadMessages returns up to limit messages of the thread rootID in
// channelID with ID greater than afterID that reader may see, in ascending ID
// order: the root first (on the page that starts before it), then the replies.
// It is ListVisibleMessages narrowed to one thread: the same visibility
// condition in the same statement as the selection, so a reader gets exactly
// the channel listing's rows that belong to the thread, and a hidden reply
// neither fills a page nor moves a cursor. If the reader cannot see the root
// the answer is ErrThreadNotFound, whatever the thread holds.
func (s *Store) ListThreadMessages(ctx context.Context, channelID int64, reader Reader, rootID, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list thread: limit must be positive, got %d", limit)
	}
	if _, err := s.ReplyRootMessage(ctx, channelID, reader, rootID); err != nil {
		return nil, err
	}
	scoped, predArgs := visibilityArgs(reader)
	rows, err := s.db.QueryContext(ctx, threadSQL(scoped),
		append(append([]any{channelID, rootID, rootID, afterID}, predArgs...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: list thread %d: %w", rootID, err)
	}
	defer func() { _ = rows.Close() }()
	msgs, err := s.scanMessages(ctx, rows)
	if err != nil {
		return nil, fmt.Errorf("store: list thread %d: %w", rootID, err)
	}
	return msgs, nil
}

const (
	threadHead = `SELECT ` + messageColumns + ` FROM messages m
		 WHERE m.channel_id = ? AND (m.id = ? OR m.thread_root_id = ?) AND m.id > ? AND `
)

func threadSQL(scoped bool) string {
	return threadHead + messageVisible(scoped) + listTail
}

// ReplyPost is a reply to store. It carries no audience: the reply's is its
// root's, read inside the insert transaction.
type ReplyPost struct {
	ChannelID int64
	AuthorID  int64
	// RootID is the message replied to.
	RootID  int64
	Body    string
	Payload *schema.Payload
	// Uploads are files the reply brings with it (see MessageUploads), or
	// nil. With them Payload must be nil: the store writes the attachment.
	Uploads *MessageUploads
}

// InsertReplyMessage stores a reply to post.RootID and returns it with the
// resolved recipient ids for fan-out (nil for a reply to a channel-wide root).
// Whether reader may see the root is decided in the same IMMEDIATE transaction
// that inserts the reply, with the list's visibility condition, so a reply is
// never stored for a caller who could see the root a moment ago and cannot
// now. reader is the verified poster; with no verified identity
// (PrincipalID 0) only a channel-wide root can be replied to.
//
// A reply reaches those who can see its root at this moment and are still
// entitled to its audience, never anyone else:
//
//   - channel-wide root: the reply is channel-wide;
//   - net root: a post to the net under the net rules (the author must be on it
//     and not a monitor), delivered to those of the root's recipients who are
//     on the net now;
//   - whisper root: the root's recipients, the author of the root among them,
//     whoever of them replies.
//
// "The root's recipients" are those stored for the root who are still channel
// members, because that is what visibility asks. Someone who joined the net
// after the root was posted is not one: they cannot see the root, and must not
// be handed a reply to a message they cannot see. Someone taken off the net
// since can still see the root (what they were sent stays theirs), but gets
// no reply: taking a principal off a net is how they are cut off from what is
// said on it from then on, and an old thread must not be a way round that.
//
// A refused reply stores nothing. ErrThreadNotFound covers every way the root
// is unusable (see its doc); ErrNotChannelMember a poster removed from the
// channel; ErrNetNotFound and ErrNetMonitorOnly the net rules; ErrInvalidAudience
// a whisper left with no recipient but its poster.
func (s *Store) InsertReplyMessage(ctx context.Context, post ReplyPost, reader Reader) (Message, []int64, error) {
	if reader.PrincipalID != 0 && reader.PrincipalID != post.AuthorID {
		// The reader is who visibility is asked for; it must be the poster.
		return Message{}, nil, ErrThreadNotFound
	}
	now := time.Now().Truncate(time.Millisecond)
	scoped, predArgs := visibilityArgs(reader)
	var msg Message
	var recipients []int64
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if reader.PrincipalID > 0 {
			member, err := isChannelMemberTx(ctx, tx, post.ChannelID, post.AuthorID)
			if err != nil {
				return err
			}
			if !member {
				return ErrNotChannelMember
			}
		}
		var kind sql.NullString
		var rootNet sql.NullInt64
		// A removed root takes no new reply (moderation.md section 10.1, Q3):
		// it is unusable, as an unknown one is.
		switch err := tx.QueryRowContext(ctx, rootSQL("m.audience_kind, m.net_id", scoped)+` AND `+notRemovedCond,
			append([]any{post.RootID, post.ChannelID}, predArgs...)...).Scan(&kind, &rootNet); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrThreadNotFound
		case err != nil:
			return fmt.Errorf("store: check thread root %d: %w", post.RootID, err)
		}
		// Files the reply brings with it are recorded here, once the root is
		// known to be usable, and attached by the insert below (issue #1798).
		payload, err := recordUploadsTx(ctx, tx, post.ChannelID, post.AuthorID, post.Uploads, post.Payload, now)
		if err != nil {
			return err
		}
		if !kind.Valid {
			id, err := insertChannelWideTx(ctx, tx, post.ChannelID, post.AuthorID, post.Body, payload, post.RootID, now)
			if err != nil {
				return err
			}
			msg = Message{ID: id, ChannelID: post.ChannelID, AuthorID: post.AuthorID, Body: post.Body, Payload: payload,
				CreatedAt: now, ThreadRootID: post.RootID}
			if msg.GuestAuthor, err = guestAuthorTx(ctx, tx, id); err != nil {
				return err
			}
			return post.Uploads.place()
		}
		// The candidates are the people who can see the root now: its
		// recipients, less any who have since left the channel. Not the net's
		// roster as it stands, which would reach someone who joined the net
		// after the root and cannot see it. For a net root the roster then
		// narrows them further, below.
		rootRecipients, err := messageRecipientsTx(ctx, tx, post.RootID)
		if err != nil {
			return err
		}
		for _, id := range rootRecipients {
			member, err := isChannelMemberTx(ctx, tx, post.ChannelID, id)
			if err != nil {
				return err
			}
			if member {
				recipients = append(recipients, id)
			}
		}
		scopedPost := ScopedPost{ChannelID: post.ChannelID, AuthorID: post.AuthorID, Body: post.Body, Payload: payload}
		var audience schema.Audience
		var net any
		switch schema.AudienceKind(kind.String) {
		case schema.AudienceKindNet:
			if !rootNet.Valid {
				return fmt.Errorf("store: message %d has a net audience with no net", post.RootID)
			}
			// Posting to a net is for those on it who may transmit, and what
			// is said on a net goes to those on it now: of the root's
			// recipients, only the ones the roster still holds.
			roster, err := resolveNetRecipientsTx(ctx, tx, post.ChannelID, rootNet.Int64, post.AuthorID)
			if err != nil {
				return err
			}
			onNet := make(map[int64]bool, len(roster))
			for _, id := range roster {
				onNet[id] = true
			}
			kept := recipients[:0]
			for _, id := range recipients {
				if onNet[id] {
					kept = append(kept, id)
				}
			}
			recipients = kept
			audience = schema.Audience{Kind: schema.AudienceKindNet, NetID: rootNet.Int64}
			net = rootNet.Int64
		case schema.AudienceKindPrincipals:
			if len(recipients) < 2 {
				return fmt.Errorf("%w: a whisper needs a recipient other than the author", ErrInvalidAudience)
			}
			audience = schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: append([]int64(nil), recipients...)}
		default:
			// Never read an audience we do not understand as something wider.
			return fmt.Errorf("store: message %d has an unreadable audience %q", post.RootID, kind.String)
		}
		scopedPost.Audience = audience
		if msg, err = insertScopedTx(ctx, tx, scopedPost, post.RootID, net, audience, recipients, now); err != nil {
			return err
		}
		return post.Uploads.place()
	})
	if err != nil {
		return Message{}, nil, err
	}
	return msg, recipients, nil
}

// messageRecipientsTx returns the recipient ids stored for messageID, ascending.
func messageRecipientsTx(ctx context.Context, tx execer, messageID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT principal_id FROM message_recipients WHERE message_id = ? ORDER BY principal_id ASC", messageID)
	if err != nil {
		return nil, fmt.Errorf("store: read recipients of message %d: %w", messageID, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: read recipients of message %d: %w", messageID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read recipients of message %d: %w", messageID, err)
	}
	return ids, nil
}

// CountMessages returns how many messages channelID holds, scoped ones
// included. It exists for tests, which use it to assert that a refused post
// stored nothing. It must never be served to a caller: the count reveals that
// messages exist which the caller may not see. The structural guard
// (TestMessageReadsStayInTheVisibilityFunction) fails if non-test code outside
// the store calls it.
func (s *Store) CountMessages(ctx context.Context, channelID int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE channel_id = ?", channelID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count messages in channel %d: %w", channelID, err)
	}
	return n, nil
}

// ScopedPost is a message to store with an audience.
type ScopedPost struct {
	ChannelID int64
	AuthorID  int64
	Body      string
	Payload   *schema.Payload
	// Audience is the normalized audience: for a principals audience the
	// sorted, de-duplicated list including the author (schema.Audience.Normalize).
	Audience schema.Audience
	// Uploads are files the message brings with it (see MessageUploads), or
	// nil. With them Payload must be nil: the store writes the attachment.
	Uploads *MessageUploads
}

// InsertScopedMessage resolves the audience, stores the message, its
// recipients and its audit events in one transaction, and returns the stored
// message and the resolved recipient ids (sorted ascending, the author
// included) for fan-out. The recipients are a snapshot of the roster at this
// moment; the transaction is IMMEDIATE so the roster cannot change between
// reading it and storing the message.
//
// The author must be a current member of the channel (ErrNotChannelMember
// otherwise). For a net audience the net must be live, in this channel, and
// the author on it as a member: ErrNetNotFound covers unknown, archived,
// belongs to another channel and author not on it alike, and ErrNetMonitorOnly
// is returned only to a monitor of the net. For a principals audience every
// listed principal must be a current channel member and the list must hold a
// principal other than the author (ErrInvalidAudience otherwise). A refused
// post stores nothing.
func (s *Store) InsertScopedMessage(ctx context.Context, p ScopedPost) (Message, []int64, error) {
	if err := p.Audience.Validate(); err != nil {
		return Message{}, nil, fmt.Errorf("%w: %w", ErrInvalidAudience, err)
	}
	now := time.Now().Truncate(time.Millisecond)
	var msg Message
	var recipients []int64
	err := s.withImmediateTx(ctx, func(tx execer) error {
		author, err := isChannelMemberTx(ctx, tx, p.ChannelID, p.AuthorID)
		if err != nil {
			return err
		}
		if !author {
			return ErrNotChannelMember
		}
		var netID any
		var audience schema.Audience
		switch p.Audience.Kind {
		case schema.AudienceKindNet:
			if recipients, err = resolveNetRecipientsTx(ctx, tx, p.ChannelID, p.Audience.NetID, p.AuthorID); err != nil {
				return err
			}
			netID = p.Audience.NetID
			audience = schema.Audience{Kind: schema.AudienceKindNet, NetID: p.Audience.NetID}
		case schema.AudienceKindPrincipals:
			if recipients, err = resolveWhisperRecipientsTx(ctx, tx, p.ChannelID, p.AuthorID, p.Audience.PrincipalIDs); err != nil {
				return err
			}
			audience = schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: append([]int64(nil), recipients...)}
		default:
			return fmt.Errorf("%w: kind %q", ErrInvalidAudience, p.Audience.Kind)
		}

		// Files the message brings with it are recorded here, once the
		// audience is resolved, and attached by the insert below (issue
		// #1798).
		if p.Payload, err = recordUploadsTx(ctx, tx, p.ChannelID, p.AuthorID, p.Uploads, p.Payload, now); err != nil {
			return err
		}
		if msg, err = insertScopedTx(ctx, tx, p, 0, netID, audience, recipients, now); err != nil {
			return err
		}
		return p.Uploads.place()
	})
	if err != nil {
		return Message{}, nil, err
	}
	return msg, recipients, nil
}

// insertScopedTx stores a scoped message (a reply when rootID is not zero), its
// recipients, its audit events and any file it attaches, in the caller's
// transaction. Posts and replies end here, so a reply writes the same rows,
// including the message_scoped audit event, as any scoped post.
func insertScopedTx(
	ctx context.Context, tx execer, p ScopedPost, rootID int64, netID any, audience schema.Audience, recipients []int64, now time.Time,
) (Message, error) {
	// The same reserved-payload refusal as insertChannelWideTx (issue #1345).
	if p.Payload != nil && schema.ReservedMessagePayload(p.Payload.Schema) {
		return Message{}, ErrReservedPayload
	}
	// The archived guard of every scoped post and reply (issue #844): the
	// other of the two INSERT INTO messages.
	if err := channelNotArchivedTx(ctx, tx, p.ChannelID); err != nil {
		return Message{}, err
	}
	var payloadSchema, payloadJSON, thread any
	if p.Payload != nil {
		payloadSchema = p.Payload.Schema
		payloadJSON = []byte(p.Payload.Data)
	}
	if rootID > 0 {
		thread = rootID
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (channel_id, author_id, body, created_at, payload_schema, payload_json, audience_kind, net_id, thread_root_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ChannelID, p.AuthorID, p.Body, now.UnixMilli(), payloadSchema, payloadJSON, string(audience.Kind), netID, thread)
	if err != nil {
		return Message{}, fmt.Errorf("store: insert scoped message in channel %d: %w", p.ChannelID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("store: insert scoped message in channel %d: %w", p.ChannelID, err)
	}
	for _, r := range recipients {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message_recipients (message_id, principal_id) VALUES (?, ?)", id, r); err != nil {
			return Message{}, fmt.Errorf("store: store recipient %d of message %d: %w", r, id, err)
		}
	}
	actor := principalActor(p.AuthorID)
	subject := fmt.Sprintf("message:%d", id)
	if err := appendAuditEventTx(ctx, tx, actor, "message.post", subject, "", now); err != nil {
		return Message{}, err
	}
	if err := appendAuditEventTx(ctx, tx, actor, AuditMessageScoped, subject,
		scopedDetail(p.ChannelID, audience, recipients), now); err != nil {
		return Message{}, err
	}
	if err := attachFilesTx(ctx, tx, p.ChannelID, p.AuthorID, id, p.Payload, now); err != nil {
		return Message{}, err
	}
	guest, err := guestAuthorTx(ctx, tx, id)
	if err != nil {
		return Message{}, err
	}
	return Message{ID: id, ChannelID: p.ChannelID, AuthorID: p.AuthorID, Body: p.Body, Payload: p.Payload,
		CreatedAt: now, Audience: &audience, ThreadRootID: rootID, GuestAuthor: guest}, nil
}

// scopedDetail is the message_scoped audit detail. It names who was addressed,
// never what was said.
func scopedDetail(channelID int64, a schema.Audience, recipients []int64) string {
	ids := make([]string, len(recipients))
	for i, r := range recipients {
		ids[i] = strconv.FormatInt(r, 10)
	}
	d := fmt.Sprintf("channel=%d audience=%s", channelID, a.Kind)
	if a.Kind == schema.AudienceKindNet {
		d += fmt.Sprintf(" net=%d", a.NetID)
	}
	return d + " recipients=" + strings.Join(ids, ",")
}

func isChannelMemberTx(ctx context.Context, tx execer, channelID, principalID int64) (bool, error) {
	var one int
	switch err := tx.QueryRowContext(ctx,
		"SELECT 1 FROM "+liveMembers+" WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: check member %d of channel %d: %w", principalID, channelID, err)
	}
	return true, nil
}

// resolveNetRecipientsTx returns every member and monitor of the net, plus the
// author, after checking the author may transmit on it. Net seats are removed
// when a principal leaves the channel; the join on channel_members is a second
// line so a stale seat could never receive.
func resolveNetRecipientsTx(ctx context.Context, tx execer, channelID, netID, authorID int64) ([]int64, error) {
	var role string
	switch err := tx.QueryRowContext(ctx,
		`SELECT m.role FROM nets n JOIN net_members m ON m.net_id = n.id
		 WHERE n.id = ? AND n.channel_id = ? AND n.archived_at IS NULL AND m.principal_id = ? AND `+notBannedSQL("m.principal_id"),
		netID, channelID, authorID).Scan(&role); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNetNotFound
	case err != nil:
		return nil, fmt.Errorf("store: read seat on net %d: %w", netID, err)
	}
	if schema.NetRole(role) != schema.NetRoleMember {
		return nil, ErrNetMonitorOnly
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT m.principal_id FROM net_members m
		 JOIN `+liveMembers+` c ON c.channel_id = ? AND c.principal_id = m.principal_id
		 WHERE m.net_id = ? ORDER BY m.principal_id ASC`, channelID, netID)
	if err != nil {
		return nil, fmt.Errorf("store: resolve recipients of net %d: %w", netID, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: resolve recipients of net %d: %w", netID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: resolve recipients of net %d: %w", netID, err)
	}
	return withAuthor(ids, authorID), nil
}

// resolveWhisperRecipientsTx checks a whisper's targets and returns the
// normalized list: sorted, distinct, author included.
func resolveWhisperRecipientsTx(ctx context.Context, tx execer, channelID, authorID int64, targets []int64) ([]int64, error) {
	ids := withAuthor(append([]int64(nil), targets...), authorID)
	if len(ids) < 2 {
		return nil, fmt.Errorf("%w: a whisper needs a recipient other than the author", ErrInvalidAudience)
	}
	for _, id := range ids {
		member, err := isChannelMemberTx(ctx, tx, channelID, id)
		if err != nil {
			return nil, err
		}
		if !member {
			return nil, fmt.Errorf("%w: every recipient must be a member of the channel", ErrInvalidAudience)
		}
	}
	return ids, nil
}

// withAuthor returns ids plus authorID, sorted ascending without duplicates.
func withAuthor(ids []int64, authorID int64) []int64 {
	ids = append(ids, authorID)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := ids[:0]
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			out = append(out, id)
		}
	}
	return out
}

// Runs (issue #266, decision D8). A run belongs to one channel and may answer
// one message in it. Its task and its activity can quote that message, so the
// run is visible to exactly the readers who can see the message, and a run
// that answers no message to the channel's members. Activity has no rule of
// its own: it is read only through its run.
//
// This file is the only place that reads rows of runs, run_activity and
// interventions (TestOnlyAudienceGoReadsRunRows): the reads below, the roster's look-up of a
// session's runs, and the reads the writes in runs.go make inside their
// transactions all embed the one condition, runVisibility, in the statement
// that selects the row, as the message reads embed visibleToScopedReader.
//
// The rule, in full. A reader sees a run when they have a verified
// PrincipalID, are now a member of the run's channel, are an enabled
// principal, and either the run answers no message or the message is in the
// run's channel and visible to them by the message rule above (for a scoped
// message: a recipient row, current membership and an enabled principal; a
// v0/v1 reader sees only a run on a channel-wide message). Removing a reader
// from the channel hides every run there from them, those they saw before
// included. Operators get no exemption. A run the reader cannot see and a run
// that does not exist are the same answer, ErrRunNotFound, everywhere.
//
// A reader held to a history bound in the run's channel (issue #719) sees a
// run only through its message, which the bound applies to: a run that
// answers no message has nothing to place it after the bound, so it is
// refused to them (default-deny), and so are its activity and interventions.

// runColumns is the column list scanRun reads, over runs aliased ru.
const runColumns = `ru.id, ru.session_id, ru.principal_id, ru.channel_id, ru.message_id, ru.state, ru.task, ru.link, ru.started_at, ru.ended_at, ru.outcome`

// runVisibleHead and runVisibleTail wrap the message condition into the run
// condition, over runs aliased ru. Every value is a bound parameter, in this
// order: reader (member), reader (enabled), reader twice (no history bound),
// then the message predicate's own. The aliases differ from the message
// predicate's (r, c, p) so nothing in it can bind to the run by accident.
var (
	runVisibleHead = `(EXISTS (SELECT 1 FROM ` + liveMembers + ` rc
	                   WHERE rc.channel_id = ru.channel_id AND rc.principal_id = ?)
	     AND EXISTS (SELECT 1 FROM principals rp WHERE rp.id = ? AND rp.disabled_at IS NULL)
	     AND ((ru.message_id IS NULL AND ` + noHistoryBound("ru.channel_id") + `) OR EXISTS (
	           SELECT 1 FROM messages m
	            WHERE m.id = ru.message_id AND m.channel_id = ru.channel_id AND `
	runVisibleTail = `)))`
)

// runVisibility reports whether reader may see any run at all (a reader with
// no verified identity sees none), which form of the message predicate the
// run condition embeds for them, and the condition's arguments.
func runVisibility(reader Reader) (scoped bool, args []any, ok bool) {
	if reader.PrincipalID <= 0 {
		return false, nil, false
	}
	scoped, predArgs := visibilityArgs(reader)
	p := reader.PrincipalID
	return scoped, append([]any{p, p, p, p}, predArgs...), true
}

// runCond is the run condition over runs aliased ru. Like listSQL it is
// built from constants only; scoped picks the message predicate.
func runCond(scoped bool) string {
	return runVisibleHead + messageVisible(scoped) + runVisibleTail
}

// The statements that read runs and activity, each with the run condition
// spliced in by the function below it. Their parameters are listed in order
// at each call.
const (
	runByIDHead  = `SELECT ` + runColumns + ` FROM runs ru WHERE ru.id = ? AND `
	ownedRunHead = `SELECT ` + runColumns + ` FROM runs ru JOIN sessions so ON so.id = ru.session_id
		 WHERE ru.id = ? AND so.credential_id = ? AND ru.principal_id = ? AND `
	runListHead = `SELECT ` + runColumns + ` FROM runs ru
		 WHERE ru.channel_id = ? AND ru.id > ? AND ru.id < ? AND (? <= 0 OR ru.session_id = ?) AND `
	runListTail = ` ORDER BY ru.id ASC LIMIT ?`
	// The newest-first read is the same statement walked backwards: the
	// bounds are written the same way so the planner sees one range on the
	// channel index, and only the direction differs.
	runListDescTail  = ` ORDER BY ru.id DESC LIMIT ?`
	activityListHead = `SELECT a.id, a.run_id, ru.principal_id, a.kind, a.summary, a.outcome, a.data, a.at
		 FROM run_activity a JOIN runs ru ON ru.id = a.run_id
		 WHERE a.run_id = ? AND a.id > ? AND `
	activityListTail = ` ORDER BY a.id ASC LIMIT ?`
	// The rows are, per session, its most recent open run and its most
	// recent run, which may be the same row. The open states are listed
	// rather than the terminal ones negated, so runs_by_session serves the
	// look-up. The task is selected only under the run condition.
	sessionRunsHead = `SELECT ru.session_id, ru.id, ru.state, CASE WHEN `
	sessionRunsTail = ` THEN ru.task END
		 FROM runs ru
		 WHERE ru.id IN (
		       SELECT max(o.id) FROM runs o
		        WHERE o.session_id IN (SELECT value FROM json_each(?))
		          AND o.state IN ('queued','starting','running','blocked','waiting')
		        GROUP BY o.session_id
		       UNION
		       SELECT max(l.id) FROM runs l
		        WHERE l.session_id IN (SELECT value FROM json_each(?))
		        GROUP BY l.session_id)
		 ORDER BY ru.session_id, ru.id`
)

func runByIDSQL(scoped bool) string  { return runByIDHead + runCond(scoped) }
func ownedRunSQL(scoped bool) string { return ownedRunHead + runCond(scoped) }
func runListSQL(scoped, newestFirst bool) string {
	if newestFirst {
		return runListHead + runCond(scoped) + runListDescTail
	}
	return runListHead + runCond(scoped) + runListTail
}
func activityListSQL(scoped bool) string {
	return activityListHead + runCond(scoped) + activityListTail
}

// sessionRunsSQL takes ok = false for a reader with no verified identity,
// for whom the condition is false and no task is selected.
func sessionRunsSQL(scoped, ok bool) string {
	if !ok {
		return sessionRunsHead + "0" + sessionRunsTail
	}
	return sessionRunsHead + runCond(scoped) + sessionRunsTail
}

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var (
		r              Run
		message, ended sql.NullInt64
		state          string
		started        int64
	)
	if err := row.Scan(&r.ID, &r.SessionID, &r.PrincipalID, &r.ChannelID, &message, &state, &r.Task, &r.Link,
		&started, &ended, &r.Outcome); err != nil {
		return Run{}, err
	}
	r.MessageID = message.Int64 // zero when NULL
	r.State = schema.RunState(state)
	if !r.State.Valid() {
		// The CHECK constraint makes this impossible; never guess a state.
		return Run{}, fmt.Errorf("store: run %d has an unreadable state %q", r.ID, state)
	}
	r.StartedAt = time.UnixMilli(started)
	if ended.Valid {
		t := time.UnixMilli(ended.Int64)
		r.EndedAt = &t
	}
	return r, nil
}

// RunVisible returns run runID if reader may see it, and ErrRunNotFound
// otherwise, whatever the reason.
func (s *Store) RunVisible(ctx context.Context, runID int64, reader Reader) (Run, error) {
	return runVisibleTx(ctx, s.db, runID, reader)
}

// runVisibleTx is RunVisible inside a transaction, for the write in
// interventions.go that must decide on the run in the transaction that stores
// its row.
func runVisibleTx(ctx context.Context, tx execer, runID int64, reader Reader) (Run, error) {
	scoped, args, ok := runVisibility(reader)
	if !ok {
		return Run{}, ErrRunNotFound
	}
	run, err := scanRun(tx.QueryRowContext(ctx, runByIDSQL(scoped), append([]any{runID}, args...)...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Run{}, ErrRunNotFound
	case err != nil:
		return Run{}, fmt.Errorf("store: read run %d: %w", runID, err)
	}
	return run, nil
}

// OwnedRun is RunVisible narrowed to the runs of credentialID's sessions, and
// to runs of reader's own principal: a run the caller may report on. Every
// other case is ErrRunNotFound, so a run of another credential, of the same
// principal or not, cannot be told from one that does not exist.
func (s *Store) OwnedRun(ctx context.Context, runID, credentialID int64, reader Reader) (Run, error) {
	return ownedRunTx(ctx, s.db, runID, credentialID, reader)
}

// ownedRunTx is OwnedRun inside a transaction, for the writes in runs.go.
func ownedRunTx(ctx context.Context, tx execer, runID, credentialID int64, reader Reader) (Run, error) {
	scoped, args, ok := runVisibility(reader)
	if !ok || credentialID <= 0 {
		return Run{}, ErrRunNotFound
	}
	// The reader is the caller and the credential is the caller's, so the run
	// is also required to be the reader's own: a credential and a reader that
	// do not belong together are refused, not reconciled.
	run, err := scanRun(tx.QueryRowContext(ctx, ownedRunSQL(scoped),
		append([]any{runID, credentialID, reader.PrincipalID}, args...)...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Run{}, ErrRunNotFound
	case err != nil:
		return Run{}, fmt.Errorf("store: read run %d: %w", runID, err)
	}
	return run, nil
}

// countOpenRunsTx is how many runs of session sessionID have not ended. It
// reads no run's content, only how many there are, and is here because this
// is the one file that reads the runs table. CreateRun asks it inside its
// transaction.
// openRunStates are the states of a run that has not ended; a terminal state
// sets ended_at (the table's CHECK ties them), so the two are the same set.
var openRunStates = [5]schema.RunState{schema.RunStateQueued, schema.RunStateStarting, schema.RunStateRunning, schema.RunStateBlocked, schema.RunStateWaiting}

func countOpenRunsTx(ctx context.Context, tx execer, sessionID int64) (int, error) {
	var n int
	// By the open states, not ended_at: runs_by_session (session_id, state)
	// then answers from the index alone, touching only open runs instead of
	// the session's whole history, inside the write lock.
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE session_id = ? AND state IN (?, ?, ?, ?, ?)", sessionID,
		openRunStates[0], openRunStates[1], openRunStates[2], openRunStates[3], openRunStates[4]).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count open runs of session %d: %w", sessionID, err)
	}
	return n, nil
}

// countOpenRunsOfCredentialTx is how many runs have not ended on the sessions
// of credential credentialID that have not ended, live and offline alike
// (issue #402). Offline sessions' runs stay open and a session cannot be told
// to forget them, so a bound on live sessions alone would let new sessions
// pile new runs on top. Ended sessions are left out: ending one ends its runs
// in the same transaction, so they hold none, except runs left open by a
// session that ended before #402. Those could never be freed (the session
// cannot be ended again, and a run the agent no longer sees cannot be ended
// by it), so counting them could lock the credential out for good; leaving
// ended sessions out also keeps the walk to the sessions still in use.
// CreateRun asks it inside its transaction.
func countOpenRunsOfCredentialTx(ctx context.Context, tx execer, credentialID int64) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs r JOIN sessions s ON s.id = r.session_id
		 WHERE s.credential_id = ? AND s.ended_at IS NULL AND r.state IN (?, ?, ?, ?, ?)`, credentialID,
		openRunStates[0], openRunStates[1], openRunStates[2], openRunStates[3], openRunStates[4]).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count open runs of credential %d: %w", credentialID, err)
	}
	return n, nil
}

// endSessionRunsTx ends every run of session sessionID that has not ended:
// state failed, outcome RunOutcomeSessionEnded, ended_at now (never before the
// run's started_at), and one run_ended event each, in the caller's
// transaction. The agent can no longer see these runs, so left open they
// would hold a slot of the credential's bound for good (issue #402). It
// appends each run it ended to *ended, for the caller to announce once the
// transaction has committed (issue #405).
func endSessionRunsTx(ctx context.Context, tx execer, actor string, sessionID int64, now time.Time, ended *[]EndedRun) error {
	type open struct{ id, principalID, channelID int64 }
	rows, err := tx.QueryContext(ctx,
		"SELECT id, principal_id, channel_id FROM runs WHERE session_id = ? AND state IN (?, ?, ?, ?, ?) ORDER BY id", sessionID,
		openRunStates[0], openRunStates[1], openRunStates[2], openRunStates[3], openRunStates[4])
	if err != nil {
		return fmt.Errorf("store: list open runs of session %d: %w", sessionID, err)
	}
	var found []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.principalID, &o.channelID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scan open run: %w", err)
		}
		found = append(found, o)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: list open runs of session %d: %w", sessionID, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: list open runs of session %d: %w", sessionID, err)
	}
	for _, o := range found {
		res, err := tx.ExecContext(ctx,
			"UPDATE runs SET state = ?, outcome = ?, ended_at = max(started_at, ?) WHERE id = ? AND state IN (?, ?, ?, ?, ?)",
			string(schema.RunStateFailed), RunOutcomeSessionEnded, now.UnixMilli(), o.id,
			openRunStates[0], openRunStates[1], openRunStates[2], openRunStates[3], openRunStates[4])
		if err != nil {
			return fmt.Errorf("store: end run %d with its session: %w", o.id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: end run %d with its session: %w", o.id, err)
		} else if n != 1 {
			return fmt.Errorf("store: end run %d with its session: affected %d rows", o.id, n)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditRunEnded, runSubject(o.id),
			fmt.Sprintf("state=%s cause=session_ended session=%d principal=%d channel=%d", schema.RunStateFailed, sessionID, o.principalID, o.channelID), now); err != nil {
			return err
		}
		*ended = append(*ended, EndedRun{ID: o.id, ChannelID: o.channelID})
	}
	return nil
}

// countRunActivityTx is how many activity entries run runID holds, for
// AppendRunActivity, inside its transaction.
func countRunActivityTx(ctx context.Context, tx execer, runID int64) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM run_activity WHERE run_id = ?", runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count activity of run %d: %w", runID, err)
	}
	return n, nil
}

// RunPage says which runs ListVisibleRuns reads. The zero value is the first
// page, ascending. After is the largest id already held (ascending reads);
// Before is the smallest (newest-first reads, where zero means from the
// newest). A page has one direction, so After with NewestFirst, or Before
// without it, is an error rather than a guess.
type RunPage struct {
	After, Before int64
	NewestFirst   bool
}

// ListVisibleRuns returns up to limit runs of channelID that reader may see:
// ascending with id greater than page.After, or with page.NewestFirst
// descending with id less than page.Before (all of them when it is zero).
// Narrowed to session sessionID when it is positive. Hidden runs are filtered
// in the statement, before the limit applies, so a page is full of visible
// runs and a cursor never stalls on a hidden one. The result is never nil.
func (s *Store) ListVisibleRuns(ctx context.Context, channelID int64, reader Reader, sessionID int64, page RunPage, limit int) ([]Run, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list runs: limit must be positive, got %d", limit)
	}
	if (page.NewestFirst && page.After != 0) || (!page.NewestFirst && page.Before != 0) {
		return nil, errors.New("store: list runs: after goes with ascending reads and before with newest-first ones")
	}
	before := page.Before
	if before <= 0 {
		before = math.MaxInt64
	}
	out := []Run{}
	scoped, args, ok := runVisibility(reader)
	if !ok {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, runListSQL(scoped, page.NewestFirst),
		append(append([]any{channelID, page.After, before, sessionID, sessionID}, args...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: list runs in channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list runs in channel %d: %w", channelID, err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list runs in channel %d: %w", channelID, err)
	}
	return out, nil
}

// ListVisibleActivity returns up to limit entries of run runID with id greater
// than afterID, oldest first, if reader may see the run, and ErrRunNotFound
// otherwise. The run condition is also in the statement that selects the
// entries, so a reader who loses sight of the run between the two gets
// nothing. The result is never nil.
func (s *Store) ListVisibleActivity(ctx context.Context, runID int64, reader Reader, afterID int64, limit int) ([]Activity, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list activity: limit must be positive, got %d", limit)
	}
	if _, err := s.RunVisible(ctx, runID, reader); err != nil {
		return nil, err
	}
	scoped, args, ok := runVisibility(reader)
	if !ok {
		return nil, ErrRunNotFound
	}
	rows, err := s.db.QueryContext(ctx, activityListSQL(scoped),
		append(append([]any{runID, afterID}, args...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: list activity of run %d: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Activity{}
	for rows.Next() {
		var (
			a    Activity
			kind string
			data []byte
			at   int64
		)
		if err := rows.Scan(&a.ID, &a.RunID, &a.PrincipalID, &kind, &a.Summary, &a.Outcome, &data, &at); err != nil {
			return nil, fmt.Errorf("store: list activity of run %d: %w", runID, err)
		}
		a.Kind = schema.ActivityKind(kind)
		if len(data) > 0 {
			a.Data = json.RawMessage(data)
		}
		a.At = time.UnixMilli(at)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list activity of run %d: %w", runID, err)
	}
	return out, nil
}

// RunAnchorAudience returns the audience of message messageID of channelID if
// reader may see it by the message rule: nil for a channel-wide message, the
// net for a net message, the principal list for a whisper. It is how the
// server decides which channel permission a run answering the message needs.
// A message that does not exist, is in another channel or is not visible to
// reader is ErrRunAnchorNotFound, one answer for all. The body is read, as
// every message read here reads it, and never returned.
func (s *Store) RunAnchorAudience(ctx context.Context, channelID, messageID int64, reader Reader) (*schema.Audience, error) {
	if reader.PrincipalID <= 0 {
		return nil, ErrRunAnchorNotFound
	}
	scoped, predArgs := visibilityArgs(reader)
	rows, err := s.db.QueryContext(ctx, anchorSQL(messageColumns, scoped),
		append([]any{messageID, channelID}, predArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("store: read run message %d: %w", messageID, err)
	}
	defer func() { _ = rows.Close() }()
	msgs, err := s.scanMessages(ctx, rows)
	if err != nil {
		return nil, fmt.Errorf("store: read run message %d: %w", messageID, err)
	}
	if len(msgs) != 1 {
		return nil, ErrRunAnchorNotFound
	}
	return msgs[0].Audience, nil
}

// anchorFrom selects a message of a channel that the reader may see (the
// visibility condition is appended). Unlike a thread root, any message can be
// answered by a run, a reply included.
const anchorFrom = ` FROM messages m
		 WHERE m.id = ? AND m.channel_id = ? AND `

func anchorSQL(columns string, scoped bool) string {
	return `SELECT ` + columns + anchorFrom + messageVisible(scoped)
}

// runAnchorVisibleTx is CreateRun's check of the message a run answers, inside
// its transaction: the message must be in the channel and visible to reader
// now, and reader must be allowed to speak to the message's audience.
//
// Seeing is not enough for a net message. A run's task and activity reach the
// net, so starting one is speaking on it, and a monitor, who has a recipient
// row for every message on the net, may only listen. So for a net message the
// reader must be on the net, which must be live and in this channel, as a
// member: the rule resolveNetRecipientsTx applies to a post. Anything else is
// ErrRunAnchorNotFound, the same answer as a message that is not there, so a
// monitor learns nothing a refusal of any other kind would not tell it. A
// whisper needs nothing more: every recipient of a whisper may reply to it.
func runAnchorVisibleTx(ctx context.Context, tx execer, channelID, messageID int64, reader Reader) error {
	if reader.PrincipalID <= 0 {
		return ErrRunAnchorNotFound
	}
	scoped, predArgs := visibilityArgs(reader)
	var (
		kind  sql.NullString
		netID sql.NullInt64
	)
	switch err := tx.QueryRowContext(ctx, anchorSQL("m.audience_kind, m.net_id", scoped),
		append([]any{messageID, channelID}, predArgs...)...).Scan(&kind, &netID); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrRunAnchorNotFound
	case err != nil:
		return fmt.Errorf("store: check run message %d: %w", messageID, err)
	}
	if !kind.Valid || schema.AudienceKind(kind.String) != schema.AudienceKindNet {
		return nil
	}
	if !netID.Valid {
		// Never read a net message without a net as something wider.
		return ErrRunAnchorNotFound
	}
	var role string
	switch err := tx.QueryRowContext(ctx,
		`SELECT m.role FROM nets n JOIN net_members m ON m.net_id = n.id
		 WHERE n.id = ? AND n.channel_id = ? AND n.archived_at IS NULL AND m.principal_id = ? AND `+notBannedSQL("m.principal_id"),
		netID.Int64, channelID, reader.PrincipalID).Scan(&role); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrRunAnchorNotFound
	case err != nil:
		return fmt.Errorf("store: check seat on net %d: %w", netID.Int64, err)
	}
	if schema.NetRole(role) != schema.NetRoleMember {
		return ErrRunAnchorNotFound
	}
	return nil
}

// SessionRuns is what a roster needs about one session's runs.
type SessionRuns struct {
	// OpenState is the state of the session's most recent open (not
	// terminal) run, and "" when none is open. It decides the status, which
	// every reader of the roster is shown.
	OpenState schema.RunState
	// LastState is the state of the session's most recent run, and "" when
	// it has none.
	LastState schema.RunState
	// CurrentRun and Task are that open run's id and task, set only when the
	// reader may see the run; otherwise both are empty.
	CurrentRun int64
	Task       string
}

// SessionRunViews returns, for each of sessionIDs that has a run, what
// SessionRuns says, as reader may see it. A session with no run is absent.
//
// The status a session shows is everyone's to see, so the states are read
// whoever reads. The task is selected only under the run condition, in the
// same statement: for a reader who may not see the open run, it never leaves
// the database, and the run's id is dropped with it.
func (s *Store) SessionRunViews(ctx context.Context, sessionIDs []int64, reader Reader) (map[int64]SessionRuns, error) {
	out := map[int64]SessionRuns{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	list, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("store: session runs: %w", err)
	}
	// args is nil for a reader with no verified identity, whose condition is
	// false and takes none.
	scoped, args, ok := runVisibility(reader)
	rows, err := s.db.QueryContext(ctx, sessionRunsSQL(scoped, ok), append(args, string(list), string(list))...)
	if err != nil {
		return nil, fmt.Errorf("store: session runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			sessionID, runID int64
			state            string
			task             sql.NullString
		)
		if err := rows.Scan(&sessionID, &runID, &state, &task); err != nil {
			return nil, fmt.Errorf("store: session runs: %w", err)
		}
		st := schema.RunState(state)
		if !st.Valid() {
			return nil, fmt.Errorf("store: run %d has an unreadable state %q", runID, state)
		}
		v := out[sessionID]
		// Rows come in ascending id within a session, so the last one is
		// its most recent run.
		v.LastState = st
		if !schema.RunStateTerminal(st) {
			v.OpenState = st
			v.CurrentRun, v.Task = 0, ""
			if task.Valid {
				v.CurrentRun, v.Task = runID, task.String
			}
		}
		out[sessionID] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: session runs: %w", err)
	}
	return out, nil
}

// Interventions (issue #287, decision D10). An intervention is a person's
// request of a run, so it has no visibility of its own: it is visible to
// exactly the readers who can see its run (the run rule above), and its body,
// which is the person speaking to the run, reaches no one else. Every
// statement below that selects an intervention for a reader embeds the one
// run condition, runCond, over the run it joins, with the caller as reader.
// Operators get no exemption, and an intervention the reader cannot see and
// one that does not exist are the same answer, ErrInterventionNotFound.
//
// The table is read nowhere else (TestOnlyAudienceGoReadsRunRows). The one
// read that serves no reader, the count of open requests behind the 16-open
// bound, is here for that reason and is asked only inside the transaction
// that has just seen the run by the rule.

// interventionColumns is the column list scanIntervention reads, over
// interventions aliased iv and runs aliased ru (for the run's channel).
const interventionColumns = `iv.id, iv.run_id, ru.channel_id, iv.kind, iv.body, iv.state, iv.requested_by, iv.requested_at, iv.answered_at, iv.reason`

// The statements that read interventions, each with the run condition spliced
// in by the function below it. Their parameters are listed in order at each
// call.
const (
	interventionByIDHead = `SELECT ` + interventionColumns + ` FROM interventions iv JOIN runs ru ON ru.id = iv.run_id
		 WHERE iv.id = ? AND `
	ownedInterventionHead = `SELECT ` + interventionColumns + ` FROM interventions iv JOIN runs ru ON ru.id = iv.run_id
		 JOIN sessions so ON so.id = ru.session_id
		 WHERE iv.id = ? AND so.credential_id = ? AND ru.principal_id = ? AND `
	runInterventionsHead = `SELECT ` + interventionColumns + ` FROM interventions iv JOIN runs ru ON ru.id = iv.run_id
		 WHERE iv.run_id = ? AND iv.id > ? AND `
	sessionInterventionsHead = `SELECT ` + interventionColumns + ` FROM interventions iv JOIN runs ru ON ru.id = iv.run_id
		 JOIN sessions so ON so.id = ru.session_id
		 WHERE ru.session_id = ? AND so.credential_id = ? AND ru.principal_id = ? AND iv.id > ? AND (? = '' OR iv.state = ?) AND `
	interventionListTail = ` ORDER BY iv.id ASC LIMIT ?`
)

func interventionByIDSQL(scoped bool) string { return interventionByIDHead + runCond(scoped) }
func ownedInterventionSQL(scoped bool) string {
	return ownedInterventionHead + runCond(scoped)
}
func runInterventionsSQL(scoped bool) string {
	return runInterventionsHead + runCond(scoped) + interventionListTail
}
func sessionInterventionsSQL(scoped bool) string {
	return sessionInterventionsHead + runCond(scoped) + interventionListTail
}

func scanIntervention(row interface{ Scan(...any) error }) (Intervention, error) {
	var (
		iv          Intervention
		kind, state string
		requested   int64
		answered    sql.NullInt64
	)
	if err := row.Scan(&iv.ID, &iv.RunID, &iv.ChannelID, &kind, &iv.Body, &state, &iv.RequestedBy, &requested,
		&answered, &iv.Reason); err != nil {
		return Intervention{}, err
	}
	iv.Kind, iv.State = schema.InterventionKind(kind), schema.InterventionState(state)
	if !iv.Kind.Valid() || !iv.State.Valid() {
		// The CHECK constraints make this impossible; never guess.
		return Intervention{}, fmt.Errorf("store: intervention %d has an unreadable kind %q or state %q", iv.ID, kind, state)
	}
	iv.RequestedAt = time.UnixMilli(requested)
	if answered.Valid {
		t := time.UnixMilli(answered.Int64)
		iv.AnsweredAt = &t
	}
	return iv, nil
}

// InterventionVisible returns intervention id if reader may see its run, and
// ErrInterventionNotFound otherwise, whatever the reason.
func (s *Store) InterventionVisible(ctx context.Context, id int64, reader Reader) (Intervention, error) {
	scoped, args, ok := runVisibility(reader)
	if !ok {
		return Intervention{}, ErrInterventionNotFound
	}
	iv, err := scanIntervention(s.db.QueryRowContext(ctx, interventionByIDSQL(scoped), append([]any{id}, args...)...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Intervention{}, ErrInterventionNotFound
	case err != nil:
		return Intervention{}, fmt.Errorf("store: read intervention %d: %w", id, err)
	}
	return iv, nil
}

// OwnedIntervention is InterventionVisible narrowed to the interventions on
// runs of credentialID's sessions, and of reader's own principal: the ones
// the caller may answer. Every other case is ErrInterventionNotFound, so an
// intervention on another credential's run, of the same principal or not,
// cannot be told from one that does not exist.
func (s *Store) OwnedIntervention(ctx context.Context, id, credentialID int64, reader Reader) (Intervention, error) {
	return ownedInterventionTx(ctx, s.db, id, credentialID, reader)
}

// ownedInterventionTx is OwnedIntervention inside a transaction, for the write
// in interventions.go.
func ownedInterventionTx(ctx context.Context, tx execer, id, credentialID int64, reader Reader) (Intervention, error) {
	scoped, args, ok := runVisibility(reader)
	if !ok || credentialID <= 0 {
		return Intervention{}, ErrInterventionNotFound
	}
	iv, err := scanIntervention(tx.QueryRowContext(ctx, ownedInterventionSQL(scoped),
		append([]any{id, credentialID, reader.PrincipalID}, args...)...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Intervention{}, ErrInterventionNotFound
	case err != nil:
		return Intervention{}, fmt.Errorf("store: read intervention %d: %w", id, err)
	}
	return iv, nil
}

// ListVisibleInterventions returns up to limit interventions on run runID with
// id greater than afterID, oldest first, if reader may see the run, and
// ErrRunNotFound otherwise. The run condition is also in the statement that
// selects the rows, so a reader who loses sight of the run between the two
// gets nothing. The result is never nil.
func (s *Store) ListVisibleInterventions(ctx context.Context, runID int64, reader Reader, afterID int64, limit int) ([]Intervention, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list interventions: limit must be positive, got %d", limit)
	}
	if _, err := s.RunVisible(ctx, runID, reader); err != nil {
		return nil, err
	}
	scoped, args, ok := runVisibility(reader)
	if !ok {
		return nil, ErrRunNotFound
	}
	return s.queryInterventions(ctx, runInterventionsSQL(scoped),
		append(append([]any{runID, afterID}, args...), limit)...)
}

// ListSessionInterventions returns up to limit interventions with id greater
// than afterID, oldest first, on the runs of session sessionID, narrowed to
// state when it is not empty: what the session's adapter polls for. The
// session must be credentialID's and the runs the reader's own, both in the
// statement, and the runs must be ones reader may still see, so an agent that
// has left a run's channel is not handed what people say to that run. The
// result is never nil.
func (s *Store) ListSessionInterventions(ctx context.Context, sessionID, credentialID int64, reader Reader, state schema.InterventionState, afterID int64, limit int) ([]Intervention, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list session interventions: limit must be positive, got %d", limit)
	}
	scoped, args, ok := runVisibility(reader)
	if !ok || credentialID <= 0 {
		return []Intervention{}, nil
	}
	return s.queryInterventions(ctx, sessionInterventionsSQL(scoped),
		append(append([]any{sessionID, credentialID, reader.PrincipalID, afterID, string(state), string(state)}, args...), limit)...)
}

func (s *Store) queryInterventions(ctx context.Context, query string, args ...any) ([]Intervention, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list interventions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Intervention{}
	for rows.Next() {
		iv, err := scanIntervention(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list interventions: %w", err)
		}
		out = append(out, iv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list interventions: %w", err)
	}
	return out, nil
}

// requestedInterventionsTx counts the interventions on run runID still in
// state requested, for the bound on how many a run may hold open. It serves
// no reader; the caller has just seen the run by the run rule in the same
// transaction.
func requestedInterventionsTx(ctx context.Context, tx execer, runID int64) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM interventions WHERE run_id = ? AND state = 'requested'", runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count requested interventions of run %d: %w", runID, err)
	}
	return n, nil
}

// Reactions (issue #765, docs/design/message-reactions.md §6.1). A reaction
// is read and written only through the message it is on, so the statements
// that name the messages table live here, in this file, and reactions.go holds
// the ones over message_reactions alone. Every one of them embeds two
// conditions for the caller, in the statement that selects the message:
//
//   - messageVisible(scoped = true): the audience rule and the history bound.
//     For a channel-wide message it checks neither membership nor that the
//     reader is enabled, because the handlers' gate does;
//   - the explicit clause below: the reader is, now, a member of the
//     message's channel and an enabled principal, in the form runVisibleHead
//     uses. The gate cannot be relied on for this: a reader removed between
//     the gate and the statement, or a subscriber whose reference was flushed
//     after a removal committed and before its socket was dropped, would
//     otherwise read a channel-wide message's reactions.
//
// A reader that is not Scoped, or has no verified principal, sees no message
// here: reactions exist on v2 routes only, for a verified caller, so there is
// nothing to read for anyone else and the answer fails closed.
//
// A removed message (docs/design/moderation.md §7, issue #852) is no target of
// a reaction: reactionMessageCond excludes it, so every statement below that
// uses it answers ErrMessageNotFound for one, as for a message the reader
// cannot see. reactionVisibleCond is the same condition without that clause,
// for the readers that must still find the tombstone (the author's own
// message read, the removal itself).

// ErrMessageNotFound is the one error for a message a reaction call cannot
// use: it does not exist, is in another channel, or the reader cannot see it.
// Message ids are sequential, so any difference between those would let a
// reader probe for messages scoped away from them.
var ErrMessageNotFound = errors.New("store: message not found")

// reactionMemberCond takes the reader's principal id twice.
const reactionMemberCond = `EXISTS (SELECT 1 FROM ` + liveMembers + ` rc
	                   WHERE rc.channel_id = m.channel_id AND rc.principal_id = ?)
	     AND EXISTS (SELECT 1 FROM principals rp WHERE rp.id = ? AND rp.disabled_at IS NULL)`

// reactionVisibleCond is the message rule and the member-and-enabled clause
// over a message aliased m. Its arguments are reactionArgs(reader).
var reactionVisibleCond = messageVisible(true) + ` AND ` + reactionMemberCond

// reactionMessageCond is the whole condition a reaction statement puts on a
// message aliased m: reactionVisibleCond, and the message is not removed. Its
// arguments are reactionArgs(reader).
var reactionMessageCond = reactionVisibleCond + ` AND ` + notRemovedCond

// reactionArgs is the arguments of reactionMessageCond for reader, and false
// when the reader may see no message at all.
func reactionArgs(reader Reader) ([]any, bool) {
	if !reader.Scoped || reader.PrincipalID <= 0 {
		return nil, false
	}
	_, args := visibilityArgs(reader)
	return append(args, reader.PrincipalID, reader.PrincipalID), true
}

// ReactionTarget is what a reaction needs to know of the message it is on.
type ReactionTarget struct {
	// Audience is nil for a channel-wide message, the net for a net message,
	// and for a whisper a principals audience with every recipient id stored
	// for it (the author among them), ascending: an agent's audience check
	// (whisper_agent when another recipient is an agent) needs the ids, as a
	// reply's does.
	Audience *schema.Audience
}

// reactionTargetTx reads the audience of message messageID of channelID for
// reader, or ErrMessageNotFound. It is the probe of §6.1: one statement, with
// the reader's conditions in it.
func reactionTargetTx(ctx context.Context, tx execer, channelID int64, reader Reader, messageID int64) (ReactionTarget, error) {
	args, ok := reactionArgs(reader)
	if !ok {
		return ReactionTarget{}, ErrMessageNotFound
	}
	var kind sql.NullString
	var net sql.NullInt64
	switch err := tx.QueryRowContext(ctx,
		`SELECT m.audience_kind, m.net_id FROM messages m
		 WHERE m.id = ? AND m.channel_id = ? AND `+reactionMessageCond,
		append([]any{messageID, channelID}, args...)...).Scan(&kind, &net); {
	case errors.Is(err, sql.ErrNoRows):
		return ReactionTarget{}, ErrMessageNotFound
	case err != nil:
		return ReactionTarget{}, fmt.Errorf("store: read message %d for a reaction: %w", messageID, err)
	}
	switch {
	case !kind.Valid:
		return ReactionTarget{}, nil
	case schema.AudienceKind(kind.String) == schema.AudienceKindNet && net.Valid:
		return ReactionTarget{Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: net.Int64}}, nil
	case schema.AudienceKind(kind.String) == schema.AudienceKindPrincipals:
		// The recipients were decided at post time and are never changed, so
		// these are the ids the message was said to.
		ids, err := messageRecipientsTx(ctx, tx, messageID)
		if err != nil {
			return ReactionTarget{}, err
		}
		return ReactionTarget{Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: ids}}, nil
	default:
		// Never read an audience we do not understand as something wider.
		return ReactionTarget{}, fmt.Errorf("store: message %d has an unreadable audience %q", messageID, kind.String)
	}
}

// ReactionTargetOf is reactionTargetTx outside a transaction: the handler's
// probe, which an agent's audience permission is decided from. The write
// transaction reads the row again, so what is stored is what was authorized.
func (s *Store) ReactionTargetOf(ctx context.Context, channelID int64, reader Reader, messageID int64) (ReactionTarget, error) {
	return reactionTargetTx(ctx, s.db, channelID, reader, messageID)
}

// reactableMessagesTx returns which of ids are messages of channelID that
// reader may see, ascending: the batch read's probe, one statement for every
// requested id. ids are bound as one JSON array, so the statement does not
// change with how many there are.
func reactableMessagesTx(ctx context.Context, tx execer, channelID int64, reader Reader, ids []int64) ([]int64, error) {
	args, ok := reactionArgs(reader)
	if !ok || len(ids) == 0 {
		return nil, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("store: encode message ids: %w", err)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT m.id FROM messages m
		 WHERE m.channel_id = ? AND m.id IN (SELECT value FROM json_each(?)) AND `+reactionMessageCond+`
		 ORDER BY m.id ASC`,
		append([]any{channelID, string(list)}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("store: read messages for reactions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var visible []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: read messages for reactions: %w", err)
		}
		visible = append(visible, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read messages for reactions: %w", err)
	}
	return visible, nil
}

// Message edits (issue #845, docs/design/message-editing.md §8). An author
// changes the text of their own recent message; every version is kept in
// message_revisions, which, like the message, is read only here: earlier
// text is as sensitive as the current text (TestOnlyAudienceGoReadsMessageRows).
//
// Both functions find the message with the reaction probe's condition
// (reactionMessageCond: the one visibility condition and the explicit
// member-and-enabled clause) narrowed to author_id = the reader, in the
// statement that selects it. So a message that is unknown, in another
// channel, someone else's, a whisper the reader is not in, a net message they
// cannot read, or from before their history bound is one answer,
// ErrMessageNotFound, and only the author ever learns more.
//
// A removed message (docs/design/moderation.md §7, issue #852) is found by
// the same condition, as a tombstone. EditOwnMessage answers
// ErrMessageNotEditable for it, and the revision read ErrMessageNotFound: the
// earlier text of a removed message is never served, even to its author.

// AuditMessageEdited is written in the edit's transaction for every edit that
// changed the text: subject "message:<id>", detail "channel=<id>
// revision=<n>", n the revision the edit created. Never any text, length or
// difference.
const AuditMessageEdited = "message_edited"

// Errors of EditOwnMessage, besides ErrMessageNotFound and
// ErrNotChannelMember. Each is reachable only by the message's author, who can
// read it.
var (
	// ErrMessageNotEditable: the message carries a payload other than a file
	// attachment, or was said on a net where the author no longer holds a
	// member seat.
	ErrMessageNotEditable = errors.New("store: message not editable")
	// ErrMessageEditWindowClosed: more than schema.MessageEditWindowSeconds
	// have passed since the message was posted.
	ErrMessageEditWindowClosed = errors.New("store: message edit window closed")
	// ErrMessageEditLimitReached: the message has been edited
	// schema.MaxMessageEdits times.
	ErrMessageEditLimitReached = errors.New("store: message edit limit reached")
)

// MessageEdit is EditOwnMessage's answer.
type MessageEdit struct {
	// Message is the message as the author now reads it, by the projection
	// a list read uses.
	Message Message
	// Recipients is who a scoped message was delivered to when it was
	// posted, read back for the edit's broadcast; nil for a channel-wide one.
	Recipients []int64
	// Changed is false when the new text equals the current text: nothing
	// was written.
	Changed bool
	// Revision is the revision the edit created; 0 when nothing changed.
	Revision int
}

// MessageRevision is one version of a message's text.
type MessageRevision struct {
	Revision  int
	Body      string
	CreatedAt time.Time
}

// ownMessageSQL selects one message of the reader's own by the reaction
// probe's condition. Its arguments are the message id, the channel id, the
// reader as author, then reactionArgs(reader).
var ownMessageSQL = `SELECT ` + messageColumns + ` FROM messages m
	 WHERE m.id = ? AND m.channel_id = ? AND m.author_id = ? AND ` + reactionVisibleCond

// ownMessageTx reads message messageID of channelID if reader wrote it and may
// see it now, or ErrMessageNotFound.
func ownMessageTx(ctx context.Context, tx execer, channelID int64, reader Reader, messageID int64) (Message, error) {
	args, ok := reactionArgs(reader)
	if !ok || messageID <= 0 {
		return Message{}, ErrMessageNotFound
	}
	rows, err := tx.QueryContext(ctx, ownMessageSQL, append([]any{messageID, channelID, reader.PrincipalID}, args...)...)
	if err != nil {
		return Message{}, fmt.Errorf("store: read own message %d: %w", messageID, err)
	}
	defer func() { _ = rows.Close() }()
	msgs, err := scanMessagesWith(ctx, tx, rows)
	if err != nil {
		return Message{}, fmt.Errorf("store: read own message %d: %w", messageID, err)
	}
	if len(msgs) != 1 {
		return Message{}, ErrMessageNotFound
	}
	return msgs[0], nil
}

// holdsNetMemberSeatTx reports whether principalID holds a member seat (not a
// monitor's) on live net netID of channelID: what transmitting there takes.
func holdsNetMemberSeatTx(ctx context.Context, tx execer, channelID, netID, principalID int64) (bool, error) {
	var role string
	switch err := tx.QueryRowContext(ctx,
		`SELECT nm.role FROM nets n JOIN net_members nm ON nm.net_id = n.id
		 WHERE n.id = ? AND n.channel_id = ? AND n.archived_at IS NULL AND nm.principal_id = ? AND `+notBannedSQL("nm.principal_id"),
		netID, channelID, principalID).Scan(&role); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: read seat on net %d: %w", netID, err)
	}
	return schema.NetRole(role) == schema.NetRoleMember, nil
}

// EditOwnMessage replaces the text of message messageID of channelID, which
// reader must have written, with body, and answers the message as reader now
// reads it (§4.2 step 7). One IMMEDIATE transaction, the checks in this
// order, each refusal storing nothing:
//
//  1. reader is, now, a member of the channel and enabled
//     (ErrNotChannelMember);
//  2. the message is reader's own and visible to reader (ErrMessageNotFound
//     for every other case alike);
//  3. it is a text message: no payload or a file attachment, and on a net the
//     author still holds a member seat (ErrMessageNotEditable);
//  4. body equal to the current text: nothing is written, Changed is false;
//  5. at most schema.MessageEditWindowSeconds since created_at, by clock,
//     read once the write lock is held (ErrMessageEditWindowClosed);
//  6. fewer than schema.MaxMessageEdits edits so far
//     (ErrMessageEditLimitReached);
//  7. the write: revision 1 (the text as posted) on the first edit, the new
//     revision, the row (the search index follows by trigger), and the
//     message_edited audit row.
//
// The payload, attachments, audience, recipients, thread link, guest
// attribution, reactions and read marks are not touched; the triggers of
// migration 38 refuse an update of any column but body and edited_at.
// edited_at is max(now, the previous edited_at or created_at + 1 ms), so it
// is strictly after created_at and strictly increasing per message, whatever
// the clock does. The emptiness and size of body are the caller's checks.
func (s *Store) EditOwnMessage(ctx context.Context, channelID int64, reader Reader, messageID int64, body string, clock func() time.Time) (MessageEdit, error) {
	var out MessageEdit
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if err := reactionReaderTx(ctx, tx, channelID, reader); err != nil {
			return err
		}
		// The archived guard of an edit (issue #844), after the reader's
		// standing is known.
		if err := channelNotArchivedTx(ctx, tx, channelID); err != nil {
			return err
		}
		msg, err := ownMessageTx(ctx, tx, channelID, reader, messageID)
		if err != nil {
			return err
		}
		if msg.Removed {
			return ErrMessageNotEditable
		}
		if msg.Payload != nil && msg.Payload.Schema != schema.FileAttachmentV1Name {
			return ErrMessageNotEditable
		}
		if msg.Audience != nil && msg.Audience.Kind == schema.AudienceKindNet {
			seat, err := holdsNetMemberSeatTx(ctx, tx, channelID, msg.Audience.NetID, reader.PrincipalID)
			if err != nil {
				return err
			}
			if !seat {
				return ErrMessageNotEditable
			}
		}
		if body == msg.Body {
			out = MessageEdit{Message: msg}
			return nil
		}
		now := clock().Truncate(time.Millisecond)
		if now.UnixMilli()-msg.CreatedAt.UnixMilli() > schema.MessageEditWindowSeconds*1000 {
			return ErrMessageEditWindowClosed
		}
		var last int
		if err := tx.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(revision), 0) FROM message_revisions WHERE message_id = ?", messageID).Scan(&last); err != nil {
			return fmt.Errorf("store: read revisions of message %d: %w", messageID, err)
		}
		if last > 0 && last-1 >= schema.MaxMessageEdits {
			return ErrMessageEditLimitReached
		}
		if last == 0 {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO message_revisions (message_id, revision, body, created_at) VALUES (?, 1, ?, ?)",
				messageID, msg.Body, msg.CreatedAt.UnixMilli()); err != nil {
				return fmt.Errorf("store: keep the text of message %d as posted: %w", messageID, err)
			}
			last = 1
		}
		floor := msg.CreatedAt
		if !msg.EditedAt.IsZero() {
			floor = msg.EditedAt
		}
		editedAt := now
		if !editedAt.After(floor) {
			editedAt = floor.Add(time.Millisecond)
		}
		revision := last + 1
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message_revisions (message_id, revision, body, created_at) VALUES (?, ?, ?, ?)",
			messageID, revision, body, editedAt.UnixMilli()); err != nil {
			return fmt.Errorf("store: keep revision %d of message %d: %w", revision, messageID, err)
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE messages SET body = ?, edited_at = ? WHERE id = ? AND channel_id = ? AND author_id = ?",
			body, editedAt.UnixMilli(), messageID, channelID, reader.PrincipalID)
		if err != nil {
			return fmt.Errorf("store: edit message %d: %w", messageID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: edit message %d: %w", messageID, err)
		}
		if n != 1 {
			// The select above found the row in this transaction; anything
			// else is a fault, and rolls the edit back.
			return fmt.Errorf("store: edit message %d: changed %d rows", messageID, n)
		}
		if err := appendAuditEventTx(ctx, tx, principalActor(reader.PrincipalID), AuditMessageEdited,
			messageSubject(messageID), fmt.Sprintf("channel=%d revision=%d", channelID, revision), editedAt); err != nil {
			return err
		}
		var recipients []int64
		if msg.Audience != nil {
			if recipients, err = messageRecipientsTx(ctx, tx, messageID); err != nil {
				return err
			}
		}
		msg.Body, msg.EditedAt = body, editedAt
		out = MessageEdit{Message: msg, Recipients: recipients, Changed: true, Revision: revision}
		return nil
	})
	if err != nil {
		return MessageEdit{}, err
	}
	return out, nil
}

// OwnMessageRevisions returns every version of the text of message messageID
// of channelID, oldest first, if reader wrote it and may see it now (§5): the
// stored revisions of an edited message, or, for one never edited, its one
// revision from the message itself. The refusals are EditOwnMessage's first
// two, ErrNotChannelMember and ErrMessageNotFound; there is no window.
func (s *Store) OwnMessageRevisions(ctx context.Context, channelID int64, reader Reader, messageID int64) ([]MessageRevision, error) {
	var out []MessageRevision
	err := s.readTx(ctx, func(tx execer) error {
		if err := reactionReaderTx(ctx, tx, channelID, reader); err != nil {
			return err
		}
		msg, err := ownMessageTx(ctx, tx, channelID, reader, messageID)
		if err != nil {
			return err
		}
		if msg.Removed {
			return ErrMessageNotFound
		}
		rows, err := tx.QueryContext(ctx,
			"SELECT revision, body, created_at FROM message_revisions WHERE message_id = ? ORDER BY revision ASC", messageID)
		if err != nil {
			return fmt.Errorf("store: read revisions of message %d: %w", messageID, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r MessageRevision
			var at int64
			if err := rows.Scan(&r.Revision, &r.Body, &at); err != nil {
				return fmt.Errorf("store: read revisions of message %d: %w", messageID, err)
			}
			r.CreatedAt = time.UnixMilli(at)
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: read revisions of message %d: %w", messageID, err)
		}
		if len(out) == 0 {
			out = []MessageRevision{{Revision: 1, Body: msg.Body, CreatedAt: msg.CreatedAt}}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Message removal (issue #852, docs/design/moderation.md §7). A moderator
// removes a message for its whole audience. The messages row is never touched:
// message_removals marks it, and every read here projects the tombstone
// (messageColumns), so there is no read of the original to forget to filter.

// AuditMessageRemoved is written in the removal's transaction when a message
// goes from visible to removed: subject "message:<id>", detail "channel=<id>".
// Never any text, payload, reason or recipient.
const AuditMessageRemoved = "message_removed"

// MessageRemoval is RemoveMessage's answer.
type MessageRemoval struct {
	// Message is the tombstone, by the projection every read uses.
	Message Message
	// Recipients is who a scoped message was delivered to when it was
	// posted, read back for the live tombstone; nil for a channel-wide one.
	Recipients []int64
	// Changed is false when the message was already removed: nothing was
	// written, and nothing is to be broadcast.
	Changed bool
}

// visibleMessageTx reads message messageID of channelID if reader may see it
// now, removed or not, or ErrMessageNotFound for every other case alike. For
// a reader with a verified principal that is reactionVisibleCond (the one
// message rule, plus current membership and an enabled principal); with
// authentication off (principal 0) it is the channel-wide rule, as a list read.
func visibleMessageTx(ctx context.Context, tx execer, channelID int64, reader Reader, messageID int64) (Message, error) {
	if messageID <= 0 {
		return Message{}, ErrMessageNotFound
	}
	var (
		cond string
		args []any
	)
	if reader.PrincipalID > 0 {
		ra, ok := reactionArgs(reader)
		if !ok {
			return Message{}, ErrMessageNotFound
		}
		cond, args = reactionVisibleCond, ra
	} else {
		scoped, pa := visibilityArgs(reader)
		cond, args = messageVisible(scoped), pa
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages m WHERE m.id = ? AND m.channel_id = ? AND `+cond,
		append([]any{messageID, channelID}, args...)...)
	if err != nil {
		return Message{}, fmt.Errorf("store: read message %d: %w", messageID, err)
	}
	defer func() { _ = rows.Close() }()
	msgs, err := scanMessagesWith(ctx, tx, rows)
	if err != nil {
		return Message{}, fmt.Errorf("store: read message %d: %w", messageID, err)
	}
	if len(msgs) != 1 {
		return Message{}, ErrMessageNotFound
	}
	return msgs[0], nil
}

// RemoveMessage removes message messageID of channelID for everyone in its
// audience, on reader's behalf and as actor (an audit actor string), in one
// IMMEDIATE transaction:
//
//  1. reader is, now, a member of the channel and enabled (ErrNotChannelMember);
//     with authentication off (principal 0) there is no one to check;
//  2. reader still stands as an owner, an operator or this channel's manager
//     (ErrManagerRefused), read under the write lock so a grant revoked after
//     the route guard cannot race the write;
//  3. the message is in the channel and reader may see it by the one message
//     rule and their history bound (ErrMessageNotFound for every other case:
//     unknown, another channel, a whisper or net message they are not on,
//     before their bound). Owners and operators get no exemption;
//  4. already removed: nothing is written (Changed false);
//  5. the removal row and the message_removed audit row.
//
// An archived channel allows it (Q1, reduce-only). The original body and
// payload are never read into the answer or the audit row.
func (s *Store) RemoveMessage(ctx context.Context, channelID int64, reader Reader, messageID int64, actor string, now time.Time) (MessageRemoval, error) {
	var out MessageRemoval
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if reader.PrincipalID > 0 {
			if err := reactionReaderTx(ctx, tx, channelID, reader); err != nil {
				return err
			}
		}
		if _, err := memberActorStandingTx(ctx, tx, channelID, reader.PrincipalID); err != nil {
			return err
		}
		msg, err := visibleMessageTx(ctx, tx, channelID, reader, messageID)
		if err != nil {
			return err
		}
		if msg.Removed {
			out = MessageRemoval{Message: msg}
			return nil
		}
		var by any
		if reader.PrincipalID > 0 {
			by = reader.PrincipalID
		}
		at := now.Truncate(time.Millisecond)
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message_removals (message_id, channel_id, removed_by, removed_at) VALUES (?, ?, ?, ?)",
			messageID, channelID, by, at.UnixMilli()); err != nil {
			return fmt.Errorf("store: remove message %d: %w", messageID, err)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditMessageRemoved, messageSubject(messageID),
			fmt.Sprintf("channel=%d", channelID), at); err != nil {
			return err
		}
		var recipients []int64
		if msg.Audience != nil {
			if recipients, err = messageRecipientsTx(ctx, tx, messageID); err != nil {
				return err
			}
		}
		// The projection of the row as it now reads.
		msg.Removed, msg.Body, msg.Payload, msg.EditedAt = true, schema.MessageRemovedBody, nil, time.Time{}
		out = MessageRemoval{Message: msg, Recipients: recipients, Changed: true}
		return nil
	})
	if err != nil {
		return MessageRemoval{}, err
	}
	return out, nil
}

// The pending-notification feed (issue #1713, Mobile M11-A;
// docs/design/mobile-notifications.md §4.3 and §5.5).
//
// ListPendingNotifications answers what is new for a reader now, shaped by
// the preferences of the credential that asks. It is a message read, so it
// is held to the rule of this file, and to one rule more: the answer is
// never a function of a message the reader cannot read. A page of "the next
// 2,000 rows above the cursor, filtered afterwards" would let any member who
// passes any `after` learn how much is said in channels they are not in and
// in whispers not to them (the activity-volume oracle of §4.2): the cursor
// would stop on a hidden row, or the bound would be spent on one. So the
// candidates are chosen in the statement, and only these reach Go:
//
//   - messageVisible: the audience predicate and the history bound, for the
//     scoped reader, exactly as the lists embed it;
//   - the reader is a live member of the message's channel now, so a
//     message in a channel they have left, been banned from, or that went
//     private without them is not a candidate whatever the audience says;
//   - the id is above both after and the reader's read mark in that channel
//     (a channel never marked starts at 0), which is where unread starts;
//   - the message was not written by the reader, and is not removed.
//
// Every one of those is a fact the reader holds or may read, so the bound
// counts only candidates, the cursor is the id of a candidate the reader may
// read or after itself, and `more` means "a candidate exists above the
// cursor". What runs in Go after that, over candidates only, is what depends
// on the credential's preferences: the mutes and the policy. A candidate
// that does not match is examined, counted against the bound and may carry
// the cursor.

// PendingNotificationScanBound is the most candidates one call examines, so
// that the mentions filter cannot make a call unbounded. When it is reached
// the call answers what matched, with the cursor at the highest candidate
// examined and more set.
const PendingNotificationScanBound = 2000

// The candidates are read one channel at a time, and merged (issues #1725 and
// #1730). A single statement over every channel the reader is in joins their
// memberships and sorts every unread candidate before it applies its limit,
// so its cost follows the reader's backlog (and, with a plan that walks
// messages by id, other channels' messages too). The global first N
// candidates in id order are always within each channel's first N, so the
// statement below takes one channel's candidates above a lower bound, in id
// order, through messages_by_channel, with every condition of the single
// statement, and pendingCandidates merges channels by id, with a min-heap,
// and stops at the bound. The answer is the same byte for byte.
//
// What is bounded, and what is not. The bound is the work of a call in
// candidates: a seek per channel the reader is in, about the limit's worth of
// candidates, and the heap's log of the channel count per candidate taken. A
// channel's seek starts above the greatest of the cursor, the read mark and
// the reader's history bound there (history_after), so rows at or below the
// bound, and rows the reader has read, are not walked. What is not bounded is
// the rows the seek passes over after that start without selecting them, in
// the reader's own channels, above their mark: the reader's own posts,
// removed messages, whispers not addressed to the reader, net messages for a
// net the reader is not on, replies after the bound to a thread whose root is
// at or below it (a reply is held to its root's id), and, for a guest with no
// bound (who sees nothing), every row. A call's latency therefore grows with
// that volume. It is a timing signal about what is said in channels the
// reader is in, and it does not change an answer: content,
// cursor and `more` are functions of candidates only (the byte-identity
// tests). The alternative, a stored high-water mark of examined ids per
// reader, would be new state on a read path that revocation (a removal or a
// ban lifted) and a new whisper recipient (a row added below the mark) would
// each invalidate, so it is not kept. See docs/design/mobile-notifications.md
// section 4.2.

// pendingChannelsSQL lists the reader's live channels with their read marks
// (0 for a channel never marked) and their history bounds (0 for a membership
// without one), in channel id order. Arguments: the reader (read mark), the
// reader (member). It reads no message. It is a list of channels the reader is
// in, and of marks and bounds they hold.
//
// A message at or below a membership's history_after is never visible to its
// holder (messageVisible: withinHistoryBound is false for it whether the
// reader is a guest or not, a reply's root being below the reply), so the
// bound is a safe place to start a seek. It is only a starting point: the
// statement keeps the whole predicate, and is the authority.
const pendingChannelsSQL = `SELECT mem.channel_id, COALESCE(rm.last_read_id, 0), COALESCE(mem.history_after, 0)
	FROM ` + liveMembers + ` mem
	LEFT JOIN read_marks rm ON rm.principal_id = ? AND rm.channel_id = mem.channel_id
	WHERE mem.principal_id = ?
	ORDER BY mem.channel_id`

// pendingNotificationsHead and Tail wrap the message visibility condition,
// as listHead and listTail do, and select one channel's candidates
// ascending, at most the batch asked for. The index is named because, once
// the database has been ANALYZEd, the planner on a server with few channels
// prefers a walk of messages by id above the bound, filtering the channel
// afterwards, which reads every channel's messages (measured in #1725). Arguments, in order: the spoiler
// prefix's length and text, the channel, the lower bound (the greatest of
// after, the channel's read mark and the reader's history bound there), the reader (member), the reader
// (author), then messageVisible's six, then the limit. The statement is
// built from constants only.
const (
	pendingNotificationsHead = `SELECT m.id, m.channel_id, ch.name, m.author_id, m.body, m.created_at, m.audience_kind,
	(SELECT g.handle FROM message_guest_attribution g WHERE g.message_id = m.id),
	(SELECT g.label FROM message_guest_attribution g WHERE g.message_id = m.id),
	EXISTS (SELECT 1 FROM files sf WHERE sf.message_id = m.id AND substr(sf.filename, 1, ?) = ?)
	FROM messages m INDEXED BY messages_by_channel
	JOIN channels ch ON ch.id = m.channel_id
	WHERE m.channel_id = ? AND m.id > ?
	  AND EXISTS (SELECT 1 FROM ` + liveMembers + ` mem WHERE mem.channel_id = m.channel_id AND mem.principal_id = ?)
	  AND m.author_id <> ? AND ` + notRemovedCond + `
	  AND `
	pendingNotificationsTail = `
	ORDER BY m.id ASC LIMIT ?`
)

func pendingNotificationsSQL() string {
	return pendingNotificationsHead + messageVisible(true) + pendingNotificationsTail
}

// pendingFirstBatch and pendingMaxBatch size a channel's reads in the merge:
// the first read of a channel takes one row (most channels have nothing, and
// the merge needs only each channel's next candidate to start), and every
// later read of the same channel doubles, up to the maximum. The sizes shape
// the work and never the answer.
const (
	pendingFirstBatch = 1
	pendingMaxBatch   = 256
)

// pendingHeap is the merge's min-heap of channels with a buffered candidate,
// ordered by the id of the head of each buffer (ids are unique, so no ties).
type pendingHeap []*pendingChannel

func (h pendingHeap) Len() int { return len(h) }
func (h pendingHeap) Less(i, j int) bool {
	return h[i].buf[0].summary.MessageID < h[j].buf[0].summary.MessageID
}
func (h pendingHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pendingHeap) Push(x any)   { *h = append(*h, x.(*pendingChannel)) }
func (h *pendingHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return x
}

// pendingCandidate is one row the feed selected: a message the reader may
// read, above their mark, that they did not write.
type pendingCandidate struct {
	summary   schema.NotificationSummaryV1
	body      string
	whisper   bool
	guestName string
	spoiler   bool
}

// pendingChannel is the merge's state for one channel: the candidates read
// and not yet taken (in id order), the id to read above next, and whether
// the channel has no more.
type pendingChannel struct {
	id    int64
	above int64
	batch int
	buf   []pendingCandidate
	done  bool
}

// fill reads the channel's next batch of candidates above its last read id.
func (c *pendingChannel) fill(ctx context.Context, stmt *sql.Stmt, predArgs []any, reader int64) error {
	args := []any{len(schema.SpoilerFilePrefix), schema.SpoilerFilePrefix, c.id, c.above, reader, reader}
	args = append(append(args, predArgs...), c.batch)
	rows, err := stmt.QueryContext(ctx, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var cand pendingCandidate
		var created int64
		var kind, guestHandle, guestLabel sql.NullString
		if err := rows.Scan(&cand.summary.MessageID, &cand.summary.ChannelID, &cand.summary.ChannelName, &cand.summary.AuthorID, &cand.body,
			&created, &kind, &guestHandle, &guestLabel, &cand.spoiler); err != nil {
			return err
		}
		cand.summary.Schema = schema.NotificationSummarySchemaV1
		cand.summary.PostedAt = schema.NewTimestamp(time.UnixMilli(created))
		cand.whisper = kind.Valid && schema.AudienceKind(kind.String) == schema.AudienceKindPrincipals
		if guestHandle.Valid {
			cand.guestName = schema.GuestDisplayName(guestLabel.String, guestHandle.String)
		}
		c.buf = append(c.buf, cand)
		c.above = cand.summary.MessageID
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// A short batch means the channel has no more above its last id. A
	// full one may or may not.
	c.done = n < c.batch
	c.batch = min(c.batch*2, pendingMaxBatch)
	return nil
}

// pendingCandidates returns the first limit candidates for reader above
// after, across the reader's live channels, in message id order: what the
// single statement of issue #1713 returned, chosen by merging the channels'
// own first candidates (see the comment above pendingChannelsSQL). It runs in
// one read transaction, a snapshot, as the single statement did.
//
// Its work is bounded in candidates: one channel list, one seek per channel
// the reader is in, and, in the merge, about limit candidates plus what each
// channel's last batch over-read (at most pendingMaxBatch each), at a heap's
// log of the channel count each. Rows a seek passes without selecting (see
// the comment above pendingChannelsSQL) are not bounded. The count of
// channels is the reader's own membership, and every channel counts: leaving
// one out would change the answer.
func (s *Store) pendingCandidates(ctx context.Context, reader Reader, after int64, limit int) ([]pendingCandidate, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, pendingChannelsSQL, reader.PrincipalID, reader.PrincipalID)
	if err != nil {
		return nil, err
	}
	var chans []*pendingChannel
	for rows.Next() {
		c := &pendingChannel{batch: pendingFirstBatch}
		var mark, bound int64
		if err := rows.Scan(&c.id, &mark, &bound); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.above = max(after, mark, bound)
		chans = append(chans, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// Prepared once: the statement is long, and parsing it per channel would
	// cost more than reading a channel with nothing new.
	stmt, err := tx.PrepareContext(ctx, pendingNotificationsSQL())
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()
	_, predArgs := visibilityArgs(Reader{PrincipalID: reader.PrincipalID, Scoped: true})
	for _, c := range chans {
		if err := c.fill(ctx, stmt, predArgs, reader.PrincipalID); err != nil {
			return nil, err
		}
	}
	// Merge: a min-heap of the channels that have a candidate, by their
	// head's message id. Take the lowest head, and read more of its channel
	// when the buffer is empty and the channel may have more, before the
	// next comparison, so every unfinished channel always shows its true
	// next candidate.
	h := make(pendingHeap, 0, len(chans))
	for _, c := range chans {
		if len(c.buf) > 0 {
			h = append(h, c)
		}
	}
	heap.Init(&h)
	out := make([]pendingCandidate, 0, min(limit, 64))
	for len(out) < limit && len(h) > 0 {
		next := h[0]
		out = append(out, next.buf[0])
		next.buf = next.buf[1:]
		if len(next.buf) == 0 && !next.done {
			if err := next.fill(ctx, stmt, predArgs, reader.PrincipalID); err != nil {
				return nil, err
			}
		}
		if len(next.buf) > 0 {
			heap.Fix(&h, 0)
		} else {
			heap.Pop(&h)
		}
	}
	return out, nil
}

// ListPendingNotifications returns one page of the feed for reader, under the
// preferences of credentialID (the defaults when it has none): at most
// schema.MaxPendingNotificationsPage summaries, ascending by message id, with
// the cursor and more of schema.ListPendingNotificationsResponseV1. The
// reader must be a scoped reader with a verified principal; the request's
// readerFor reader is.
//
// A candidate qualifies when its channel is not muted and the policy admits
// it: off admits nothing (and reads nothing), mentions a whisper the reader
// received or a body that mentions the reader's current name, all every
// candidate. The kind is whisper, then mention, then message, whatever the
// policy. A page that fills stops at its last summary, so the cursor never
// skips a match.
//
// author_name is given only as ListChannelAuthors would give it to this
// reader in the message's channel, or as the guest attribution the message
// carries for a guest's post. text is given only under preview, and never for
// a spoiler: a body containing "||", or a message with an attachment whose
// stored name starts with schema.SpoilerFilePrefix.
func (s *Store) ListPendingNotifications(ctx context.Context, reader Reader, credentialID, after int64) (schema.ListPendingNotificationsResponseV1, error) {
	if !reader.Scoped || reader.PrincipalID <= 0 {
		return schema.ListPendingNotificationsResponseV1{}, errors.New("store: list pending notifications: the feed needs a scoped reader with a principal")
	}
	if after < 0 {
		return schema.ListPendingNotificationsResponseV1{}, errors.New("store: list pending notifications: a cursor is never negative")
	}
	empty := schema.ListPendingNotificationsResponseV1{Summaries: []schema.NotificationSummaryV1{}, Cursor: after}
	prefs, err := notificationPreferencesOf(ctx, s.db, credentialID)
	if err != nil {
		return schema.ListPendingNotificationsResponseV1{}, err
	}
	if prefs.Policy == schema.NotificationPolicyOff {
		return empty, nil
	}
	var readerName string
	if err := s.db.QueryRowContext(ctx, "SELECT name FROM principals WHERE id = ?", reader.PrincipalID).Scan(&readerName); err != nil {
		return schema.ListPendingNotificationsResponseV1{}, fmt.Errorf("store: list pending notifications: reader: %w", err)
	}

	cands, err := s.pendingCandidates(ctx, reader, after, PendingNotificationScanBound+1)
	if err != nil {
		return schema.ListPendingNotificationsResponseV1{}, fmt.Errorf("store: list pending notifications: %w", err)
	}

	muted := make(map[int64]struct{}, len(prefs.Muted))
	for _, id := range prefs.Muted {
		muted[id] = struct{}{}
	}
	examine := cands
	truncated := false
	if len(examine) > PendingNotificationScanBound {
		examine, truncated = examine[:PendingNotificationScanBound], true
	}
	resp := empty
	var kept []pendingCandidate
	pageFull := false
	for i, c := range examine {
		resp.Cursor = c.summary.MessageID
		kind := schema.NotificationKindMessage
		switch {
		case c.whisper:
			kind = schema.NotificationKindWhisper
		case schema.Mentions(c.body, readerName):
			kind = schema.NotificationKindMention
		}
		if _, isMuted := muted[c.summary.ChannelID]; isMuted {
			continue
		}
		if prefs.Policy == schema.NotificationPolicyMentions && kind == schema.NotificationKindMessage {
			continue
		}
		c.summary.Kind = kind
		if prefs.Preview && !c.spoiler && !strings.Contains(c.body, "||") {
			c.summary.Text = notificationText(c.body)
		}
		if c.guestName != "" {
			c.summary.AuthorName = c.guestName
		}
		kept = append(kept, c)
		if len(kept) == schema.MaxPendingNotificationsPage {
			// The page is full: stop at this summary, so the cursor never
			// passes a match that is not in it. A candidate beyond it, in
			// the lookahead too, is what more reports.
			resp.More = i+1 < len(cands)
			pageFull = true
			break
		}
	}
	if !pageFull {
		resp.More = truncated
	}

	// Names, once per channel and only for the page, through the authors
	// rule. A guest's post carries its own attribution, set above.
	byChannel := map[int64][]int64{}
	for _, c := range kept {
		if c.guestName == "" {
			byChannel[c.summary.ChannelID] = append(byChannel[c.summary.ChannelID], c.summary.AuthorID)
		}
	}
	names := map[[2]int64]string{}
	for channelID, ids := range byChannel {
		authors, err := s.ListChannelAuthors(ctx, channelID, reader, ids)
		if err != nil {
			return schema.ListPendingNotificationsResponseV1{}, fmt.Errorf("store: list pending notifications: %w", err)
		}
		for _, a := range authors {
			names[[2]int64{channelID, a.PrincipalID}] = a.Name
		}
	}
	for _, c := range kept {
		if c.guestName == "" {
			c.summary.AuthorName = names[[2]int64{c.summary.ChannelID, c.summary.AuthorID}]
		}
		resp.Summaries = append(resp.Summaries, c.summary)
	}
	return resp, nil
}

// PushWakeTarget is one registration a post should wake: what the sender needs
// to address one wake, and the two times it coalesces by. The endpoint and the
// keys are the registration's write-only target (see push_registrations.go):
// they go to the sender and nowhere else, and no log line, error or audit row
// holds them.
type PushWakeTarget struct {
	RegistrationID string
	CredentialID   int64
	PrincipalID    int64
	Endpoint       string
	P256DH         string
	Auth           string
	// LastWakeAt and LastFetchAt are the registration's own times, the zero
	// time for "never".
	LastWakeAt  time.Time
	LastFetchAt time.Time
}

// PushTargetScanBound is the most registrations one post is evaluated for: the
// ones on live members of the channel are few (a person holds at most
// schema.MaxPushRegistrationsPerPrincipal), and a channel is bounded, so this
// is a safety net, not a limit anyone meets.
const PushTargetScanBound = 5000

// pushTargetScanBound is the bound in force: PushTargetScanBound, a variable
// only so a test can lower it.
var pushTargetScanBound = PushTargetScanBound

// pushCandidatesSQL selects the registrations of channelID's live members that
// a wake could be for, before the message is looked at. A registration is
// a candidate when
//
//   - it is active, and a webpush one;
//   - its credential is live (liveCredentialWhere, the one predicate of
//     push_registrations.go: not revoked, full scope, issued under its
//     principal's present authentication generation, so a sign-in a restore
//     killed is never woken, and neither expired nor idle past its bound); a
//     guest's visit is checked in Go (credentialIdle.live);
//   - its principal is a human, enabled, not hidden, and a live member of the
//     channel (liveMembers: not banned), and did not write the message.
//
// Arguments, in order: the channel, the author, the bound.
func pushCandidatesSQL(now time.Time) string {
	// liveCredentialWhere is a constant of this package plus an integer.
	return `SELECT r.id, r.credential_id, r.principal_id, r.endpoint, r.p256dh, r.auth,
	       r.last_wake_at, r.last_fetch_at, p.name, ` + idleBoundColumns + `
	FROM push_registrations r
	JOIN credentials c ON c.id = r.credential_id AND c.principal_id = r.principal_id
	JOIN principals p ON p.id = r.principal_id
	JOIN ` + liveMembers + ` mem ON mem.channel_id = ?1 AND mem.principal_id = r.principal_id
	WHERE r.state = 'active' AND r.transport = 'webpush'
	  AND ` + liveCredentialWhere(now) + `
	  AND p.kind = 'human' AND p.disabled_at IS NULL AND p.hidden_at IS NULL
	  AND p.id <> ?2
	ORDER BY r.id LIMIT ?3`
}

// pushMessageSQL reads the post being evaluated: its body and audience, and
// only when it is in the channel and not removed. It is the only place the
// push path reads a body, and the body leaves this file only as the answer of
// pushPolicyAdmits.
const pushMessageSQL = `SELECT m.body, m.audience_kind, m.author_id FROM messages m
	WHERE m.id = ? AND m.channel_id = ? AND ` + notRemovedCond

// pushVisibleSQL asks whether the message is visible to a scoped reader:
// messageVisible (the audience rule, the reader being a recipient and a live
// member for a scoped message, and the history bound) in the same statement
// that selects the message, exactly as the lists embed it. Arguments: the
// message id, then visibilityArgs' six for the reader.
var pushVisibleSQL = `SELECT 1 FROM messages m WHERE m.id = ? AND ` + notRemovedCond + ` AND ` + messageVisible(true)

// PushTargets returns the registrations that a new message should wake, for the
// sender (issue #1715, docs/design/mobile-notifications.md §6.2). It is the
// post-time counterpart of the pending feed, and answers the same question,
// for the same reader rule: a registration is returned when
//
//   - it is a candidate (pushCandidatesSQL): an active webpush registration of
//     a live, full-scope credential at its principal's present generation,
//     whose principal is a human, enabled, not hidden, a live member of the
//     channel and not the author;
//   - the credential is within its idle bound at now (credentialIdle.live);
//   - the message exists in the channel, is not removed, and is visible to
//     that principal (messageVisible with their scoped reader, so a net
//     message they are not in, a whisper to someone else, and what is before
//     their history bound are not);
//   - the credential's policy and mutes admit it (pushPolicyAdmits): off
//     admits nothing, a muted channel nothing, mentions a whisper the
//     principal received or a body that mentions their current name, all every
//     message.
//
// Every one of those is read now, so a principal removed from the channel, a
// credential revoked or a message removed between the post and this call is
// not woken. It is bounded by PushTargetScanBound. A message that is not
// found, or has no target, gives no targets and no error. The author excluded
// is the message's own, read from its row.
func (s *Store) PushTargets(ctx context.Context, channelID, messageID int64, now time.Time) ([]PushWakeTarget, error) {
	var body string
	var kind sql.NullString
	var authorID int64 // the message's own author: never a caller's word for it
	switch err := s.db.QueryRowContext(ctx, pushMessageSQL, messageID, channelID).Scan(&body, &kind, &authorID); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: push targets: message: %w", err)
	}
	whisper := kind.Valid && schema.AudienceKind(kind.String) == schema.AudienceKindPrincipals

	rows, err := s.db.QueryContext(ctx, pushCandidatesSQL(now), channelID, authorID, pushTargetScanBound) // #nosec G202 -- constants plus an integer
	if err != nil {
		return nil, fmt.Errorf("store: push targets: candidates: %w", err)
	}
	type candidate struct {
		target PushWakeTarget
		name   string
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		var wake, fetch sql.NullInt64
		var idle credentialIdle
		if err := rows.Scan(&c.target.RegistrationID, &c.target.CredentialID, &c.target.PrincipalID,
			&c.target.Endpoint, &c.target.P256DH, &c.target.Auth, &wake, &fetch, &c.name,
			&idle.createdAt, &idle.idleMs, &idle.lastUsedAt, &idle.guestState, &idle.guestExpiresAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: push targets: scan: %w", err)
		}
		if !idle.live(now) {
			continue
		}
		if wake.Valid {
			c.target.LastWakeAt = time.UnixMilli(wake.Int64)
		}
		if fetch.Valid {
			c.target.LastFetchAt = time.UnixMilli(fetch.Int64)
		}
		cands = append(cands, c)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: push targets: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: push targets: %w", err)
	}

	// Visibility is per principal, once each, however many registrations they
	// hold; preferences are per credential.
	visible := map[int64]bool{}
	var out []PushWakeTarget
	for _, c := range cands {
		ok, seen := visible[c.target.PrincipalID]
		if !seen {
			_, args := visibilityArgs(Reader{PrincipalID: c.target.PrincipalID, Scoped: true})
			var one int
			switch err := s.db.QueryRowContext(ctx, pushVisibleSQL, append([]any{messageID}, args...)...).Scan(&one); {
			case errors.Is(err, sql.ErrNoRows):
				ok = false
			case err != nil:
				return nil, fmt.Errorf("store: push targets: visibility: %w", err)
			default:
				ok = true
			}
			visible[c.target.PrincipalID] = ok
		}
		if !ok {
			continue
		}
		prefs, err := notificationPreferencesOf(ctx, s.db, c.target.CredentialID)
		if err != nil {
			return nil, err
		}
		if pushPolicyAdmits(prefs, channelID, whisper, body, c.name) {
			out = append(out, c.target)
		}
	}
	return out, nil
}

// pushPolicyAdmits is the policy of §6.2 for a message the principal may
// already see: the credential's mutes first (a muted channel wakes nothing,
// whatever the policy), then off (nothing), mentions (a whisper the principal
// received, which the visibility condition has shown they are a recipient of,
// or a body that mentions their current name by schema.Mentions) and all
// (everything). A policy this code does not know admits nothing. It is the
// same rule the feed applies to its candidates, so a wake is sent for exactly
// the messages the feed would list.
func pushPolicyAdmits(prefs schema.NotificationPreferencesV1, channelID int64, whisper bool, body, name string) bool {
	if slices.Contains(prefs.Muted, channelID) {
		return false
	}
	switch prefs.Policy {
	case schema.NotificationPolicyAll:
		return true
	case schema.NotificationPolicyMentions:
		return whisper || schema.Mentions(body, name)
	default:
		return false
	}
}
