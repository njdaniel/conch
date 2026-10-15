package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
// result equal to limit means "at least that many"). It is the unread count of
// a read mark (issue #351). It embeds the same visibility condition as
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

// messageColumns is the column list scanMessages reads.
const messageColumns = `m.id, m.channel_id, m.author_id, m.body, m.payload_schema, m.payload_json, m.created_at, m.audience_kind, m.net_id, m.thread_root_id`

// The visibility rule for which messages a reader may see, as SQL conditions
// over the messages table aliased m. The list and the search both embed the
// same two constants in their own statement, so a message is filtered in the
// statement that selects it and the two cannot disagree. See
// ListVisibleMessages for the rule in words. The statements are built from
// constants only (never from a caller's text), so every value is a bound
// parameter.
const (
	// visibleToScopedReader takes the reader's principal id three times.
	visibleToScopedReader = `(m.audience_kind IS NULL OR (
			        EXISTS (SELECT 1 FROM message_recipients r
			                WHERE r.message_id = m.id AND r.principal_id = ?)
			    AND EXISTS (SELECT 1 FROM channel_members c
			                WHERE c.channel_id = m.channel_id AND c.principal_id = ?)
			    AND EXISTS (SELECT 1 FROM principals p
			                WHERE p.id = ? AND p.disabled_at IS NULL)))`
	visibleToAnyReader = `m.audience_kind IS NULL`

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
		   AND m.channel_id = ? AND (? = 0 OR m.id < ?) AND `
	searchTail = `
		 ORDER BY m.id DESC LIMIT ?`
)

// visibilityArgs reports whether reader is a scoped reader with an identity
// (which selects visibleToScopedReader) and the predicate's arguments.
func visibilityArgs(reader Reader) (scoped bool, args []any) {
	if reader.Scoped && reader.PrincipalID > 0 {
		return true, []any{reader.PrincipalID, reader.PrincipalID, reader.PrincipalID}
	}
	return false, nil
}

func listSQL(scoped, newestFirst bool) string {
	tail := listTail
	if newestFirst {
		tail = listDescTail
	}
	if scoped {
		return listHead + visibleToScopedReader + tail
	}
	return listHead + visibleToAnyReader + tail
}

func countSQL(scoped bool) string {
	if scoped {
		return countHead + visibleToScopedReader + countTail
	}
	return countHead + visibleToAnyReader + countTail
}

func newestSQL(scoped bool) string {
	if scoped {
		return newestHead + visibleToScopedReader + newestTail
	}
	return newestHead + visibleToAnyReader + newestTail
}

func searchSQL(scoped bool) string {
	if scoped {
		return searchHead + visibleToScopedReader + searchTail
	}
	return searchHead + visibleToAnyReader + searchTail
}

// scanMessages reads the rows of a statement selecting messageColumns, and
// loads the audience of any whisper among them. It closes rows.
func (s *Store) scanMessages(ctx context.Context, rows *sql.Rows) ([]Message, error) {
	var msgs []Message
	var whispers []int // indexes into msgs of principals-audience messages
	for rows.Next() {
		var m Message
		var payloadSchema, kind sql.NullString
		var payloadJSON []byte
		var netID, threadRoot sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.AuthorID, &m.Body, &payloadSchema, &payloadJSON, &createdAt, &kind, &netID, &threadRoot); err != nil {
			return nil, err
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
	return msgs, s.fillWhisperAudiences(ctx, msgs, whispers)
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
func (s *Store) fillWhisperAudiences(ctx context.Context, msgs []Message, idx []int) error {
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
	rows, err := s.db.QueryContext(ctx,
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
//   - they uploaded it (attached or not), or
//   - the message it is attached to is visible to them by the same
//     visibleToScopedReader / visibleToAnyReader constants ListVisibleMessages
//     and SearchVisibleMessages embed: a whisper's file is readable by the
//     whisper's readers and nobody else, a net message's file by the net's
//     recipients, and a removed member loses every file they could reach
//     through a message.
//
// Channel membership and an enabled principal are required for the uploader as
// well: leaving the channel closes it to you, your uploads included. Operators
// get no exemption. A reader with no verified identity is never given a file.
func (s *Store) FileVisibleTo(ctx context.Context, fileID int64, reader Reader) (File, error) {
	if reader.PrincipalID <= 0 {
		return File{}, ErrNotFound
	}
	scoped, predArgs := visibilityArgs(reader)
	args := append([]any{fileID, reader.PrincipalID, reader.PrincipalID, reader.PrincipalID}, predArgs...)
	f, err := scanFile(s.db.QueryRowContext(ctx, fileSQL(scoped), args...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return File{}, ErrNotFound
	case err != nil:
		return File{}, fmt.Errorf("store: read file %d: %w", fileID, err)
	}
	return f, nil
}

// fileHead and fileTail wrap the message visibility condition for a file. The
// condition is spliced in as a constant (never a caller's text); every value is
// a bound parameter, in this order: file id, reader (member), reader (enabled),
// reader (uploader), then the predicate's own.
const (
	fileHead = `SELECT ` + fileColumns + ` FROM files f
		 WHERE f.id = ?
		   AND EXISTS (SELECT 1 FROM channel_members cm
		               WHERE cm.channel_id = f.channel_id AND cm.principal_id = ?)
		   AND EXISTS (SELECT 1 FROM principals up
		               WHERE up.id = ? AND up.disabled_at IS NULL)
		   AND (f.uploader_id = ? OR EXISTS (
		         SELECT 1 FROM messages m
		          WHERE m.id = f.message_id AND m.channel_id = f.channel_id AND `
	fileTail = `))`
)

func fileSQL(scoped bool) string {
	if scoped {
		return fileHead + visibleToScopedReader + fileTail
	}
	return fileHead + visibleToAnyReader + fileTail
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
	if scoped {
		return `SELECT ` + columns + rootFrom + visibleToScopedReader
	}
	return `SELECT ` + columns + rootFrom + visibleToAnyReader
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
	if scoped {
		return threadHead + visibleToScopedReader + listTail
	}
	return threadHead + visibleToAnyReader + listTail
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
		switch err := tx.QueryRowContext(ctx, rootSQL("m.audience_kind, m.net_id", scoped),
			append([]any{post.RootID, post.ChannelID}, predArgs...)...).Scan(&kind, &rootNet); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrThreadNotFound
		case err != nil:
			return fmt.Errorf("store: check thread root %d: %w", post.RootID, err)
		}
		if !kind.Valid {
			id, err := insertChannelWideTx(ctx, tx, post.ChannelID, post.AuthorID, post.Body, post.Payload, post.RootID, now)
			if err != nil {
				return err
			}
			msg = Message{ID: id, ChannelID: post.ChannelID, AuthorID: post.AuthorID, Body: post.Body, Payload: post.Payload,
				CreatedAt: now, ThreadRootID: post.RootID}
			return nil
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
		scopedPost := ScopedPost{ChannelID: post.ChannelID, AuthorID: post.AuthorID, Body: post.Body, Payload: post.Payload}
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
		msg, err = insertScopedTx(ctx, tx, scopedPost, post.RootID, net, audience, recipients, now)
		return err
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

		msg, err = insertScopedTx(ctx, tx, p, 0, netID, audience, recipients, now)
		return err
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
	return Message{ID: id, ChannelID: p.ChannelID, AuthorID: p.AuthorID, Body: p.Body, Payload: p.Payload,
		CreatedAt: now, Audience: &audience, ThreadRootID: rootID}, nil
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
		"SELECT 1 FROM channel_members WHERE channel_id = ? AND principal_id = ?", channelID, principalID).Scan(&one); {
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
		 WHERE n.id = ? AND n.channel_id = ? AND n.archived_at IS NULL AND m.principal_id = ?`,
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
		 JOIN channel_members c ON c.channel_id = ? AND c.principal_id = m.principal_id
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

// runColumns is the column list scanRun reads, over runs aliased ru.
const runColumns = `ru.id, ru.session_id, ru.principal_id, ru.channel_id, ru.message_id, ru.state, ru.task, ru.link, ru.started_at, ru.ended_at, ru.outcome`

// runVisibleHead and runVisibleTail wrap the message condition into the run
// condition, over runs aliased ru. Every value is a bound parameter, in this
// order: reader (member), reader (enabled), then the message predicate's own.
// The aliases differ from the message predicate's (r, c, p) so nothing in it
// can bind to the run by accident.
const (
	runVisibleHead = `(EXISTS (SELECT 1 FROM channel_members rc
	                   WHERE rc.channel_id = ru.channel_id AND rc.principal_id = ?)
	     AND EXISTS (SELECT 1 FROM principals rp WHERE rp.id = ? AND rp.disabled_at IS NULL)
	     AND (ru.message_id IS NULL OR EXISTS (
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
	return scoped, append([]any{reader.PrincipalID, reader.PrincipalID}, predArgs...), true
}

// runCond is the run condition over runs aliased ru. Like listSQL it is
// built from constants only; scoped picks the message predicate.
func runCond(scoped bool) string {
	if scoped {
		return runVisibleHead + visibleToScopedReader + runVisibleTail
	}
	return runVisibleHead + visibleToAnyReader + runVisibleTail
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
	if scoped {
		return `SELECT ` + columns + anchorFrom + visibleToScopedReader
	}
	return `SELECT ` + columns + anchorFrom + visibleToAnyReader
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
		 WHERE n.id = ? AND n.channel_id = ? AND n.archived_at IS NULL AND m.principal_id = ?`,
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
