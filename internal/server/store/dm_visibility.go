package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The visibility rule of direct messages (issue #2017, ADR-017 Decisions 1, 3
// and 12), and the only file that reads their rows.
//
// A conversation is visible to a viewer when all of these hold, in the one
// condition dmVisibleCond:
//
//   - the viewer is one of the two principals of the pair. Nobody else sees
//     it, whatever their role: an operator, an owner or a member of every
//     channel has no exemption, and a conversation id guessed by a stranger
//     is ErrDMNotFound, as one that does not exist;
//   - the viewer stands: a human principal, enabled, and not a guest
//     (dmStandingCond). Disabling a principal hides its conversations from
//     it and keeps them, so enabling it again resumes them, as for whispers
//     (audience.go);
//   - the conversation has been opened by the viewer or has a message. A pair
//     the other person opened and never wrote in is empty: nothing in it
//     tells the viewer it exists, not a list row, not a fetch by id.
//
// Every statement below that reads a conversation, a message, a revision, a
// removal, the search index, a file or a read mark of a conversation embeds
// dmVisibleCond or runs after dmViewTx, which is the same condition as a
// statement. TestOnlyDMVisibilityGoReadsDMRows (dm_guard_test.go) keeps the
// reads of those tables in this file, as TestOnlyAudienceGoReadsMessageRows
// does for channel messages. dm.go writes, and asks this file for what it
// needs to know first and for the row it changed.
//
// Whether the viewer may *contact* the other person (blocks, preferences,
// switches) is not visibility and is not here: it is dmContactTx in dm.go.
// Reading retained history needs standing and membership only.

// ErrDMNotFound is every refusal to show a conversation, a message or a file
// of one: no such conversation, the viewer is not one of its pair, the
// viewer does not stand, the conversation is empty and the viewer never
// opened it, or the file is not the viewer's to read. One error, so the
// answer never says which.
var ErrDMNotFound = errors.New("store: direct message conversation not found")

// dmStandingCond holds when principal ?N is a human, enabled, and not a
// guest (a guest is one exactly while its visit is active, migration 31; a
// guest who converted to a member stands). It is a format with one verb, the parameter number.
const dmStandingCond = `EXISTS (SELECT 1 FROM principals sp WHERE sp.id = ?%[1]d
	AND sp.kind = 'human' AND sp.disabled_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM guest_visits sg WHERE sg.principal_id = sp.id AND sg.state = 'active'))`

// dmVisibleCond is the whole rule, over a dm_conversations row aliased c and
// the viewer as parameter ?1.
var dmVisibleCond = `(c.lo = ?1 OR c.hi = ?1) AND ` + fmt.Sprintf(dmStandingCond, 1) + `
	AND (EXISTS (SELECT 1 FROM dm_viewer_state vs0 WHERE vs0.conversation_id = c.id AND vs0.principal_id = ?1)
	  OR EXISTS (SELECT 1 FROM direct_messages vm0 WHERE vm0.conversation_id = c.id))`

// dmViewerStandsTx reports whether principalID stands (dmStandingCond).
func dmViewerStandsTx(ctx context.Context, q execer, principalID int64) (bool, error) {
	var ok bool
	if err := q.QueryRowContext(ctx, `SELECT `+fmt.Sprintf(dmStandingCond, 1), principalID).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: read standing of principal %d: %w", principalID, err)
	}
	return ok, nil
}

// dmRow is a conversation row the viewer may see.
type dmRow struct {
	id, lo, hi int64
	createdAt  time.Time
}

// peer is the participant who is not viewer.
func (r dmRow) peer(viewer int64) int64 {
	if r.lo == viewer {
		return r.hi
	}
	return r.lo
}

// dmViewTx is the one function every DM read goes through: the conversation
// convID as viewer may see it, or ErrDMNotFound.
func dmViewTx(ctx context.Context, q execer, viewer, convID int64) (dmRow, error) {
	var r dmRow
	var at int64
	err := q.QueryRowContext(ctx,
		`SELECT c.id, c.lo, c.hi, c.created_at FROM dm_conversations c WHERE c.id = ?2 AND `+dmVisibleCond,
		viewer, convID).Scan(&r.id, &r.lo, &r.hi, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return dmRow{}, ErrDMNotFound
	}
	if err != nil {
		return dmRow{}, fmt.Errorf("store: read conversation %d: %w", convID, err)
	}
	r.createdAt = time.UnixMilli(at)
	return r, nil
}

// dmPairTx finds the conversation of two principals in a write's transaction:
// its id, or 0 when the pair has none. It is a lookup by pair, which only
// the opener asks, after standing is checked; it hands out nothing but the
// id, and OpenDM then runs dmViewTx before returning anything.
func dmPairTx(ctx context.Context, q execer, a, b int64) (int64, error) {
	lo, hi := min(a, b), max(a, b)
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM dm_conversations WHERE lo = ? AND hi = ?`, lo, hi).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: find the conversation of %d and %d: %w", lo, hi, err)
	}
	return id, nil
}

// dmReadTx runs fn in one read-only transaction, so a conversation, its
// messages and the counts about them are one snapshot.
func (s *Store) dmReadTx(ctx context.Context, fn func(tx execer) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// DMConversation is a conversation as one of its participants sees it. The
// pair is the same for both; State, LastReadID and Unread are the viewer's.
type DMConversation struct {
	ID        int64
	Lo, Hi    int64
	PeerID    int64
	// CreatedAt is when the conversation first became visible to the peer:
	// the time of its first message. It is zero until there is one; an open
	// never sets it, so it tells nothing about who opened first.
	CreatedAt time.Time
	// Revision is the conversation's change counter: 0 until the first
	// message, then the number of messages, edits and removals so far (the
	// revision of the latest one).
	Revision int64
	// State is the viewer's own archive/hide state; open when never set.
	State schema.DMState
	// LastMessageID is the newest message's id, 0 when there is none, and
	// LastMessageAt when it was posted (zero when there is none). A removed
	// message still counts: its tombstone keeps its place.
	LastMessageID int64
	LastMessageAt time.Time
	LastReadID    int64
	Unread        int
}

// dmSelectConversation reads one conversation's per-viewer columns; ?1 is the
// viewer and ?2 the conversation.
const dmSelectConversation = `SELECT c.id, c.lo, c.hi, COALESCE(fm.created_at, 0), COALESCE(vs.state, 'open'),
	COALESCE(lm.id, 0), COALESCE(lm.created_at, 0), COALESCE(rm.last_read_id, 0),
	COALESCE((SELECT MAX(revision) FROM direct_messages WHERE conversation_id = c.id), 0)
	FROM dm_conversations c
	LEFT JOIN direct_messages fm ON fm.id = (SELECT MIN(id) FROM direct_messages WHERE conversation_id = c.id)
	LEFT JOIN direct_messages lm ON lm.id = (SELECT MAX(id) FROM direct_messages WHERE conversation_id = c.id)
	LEFT JOIN dm_viewer_state vs ON vs.conversation_id = c.id AND vs.principal_id = ?1
	LEFT JOIN dm_read_marks rm ON rm.conversation_id = c.id AND rm.principal_id = ?1`

func scanDMConversation(row interface{ Scan(...any) error }, viewer int64) (DMConversation, error) {
	var c DMConversation
	var created, lastAt int64
	var state string
	if err := row.Scan(&c.ID, &c.Lo, &c.Hi, &created, &state, &c.LastMessageID, &lastAt, &c.LastReadID, &c.Revision); err != nil {
		return DMConversation{}, err
	}
	if c.Revision > 0 {
		c.CreatedAt = time.UnixMilli(created)
	}
	c.State = schema.DMState(state)
	if c.LastMessageID > 0 {
		c.LastMessageAt = time.UnixMilli(lastAt)
	}
	c.PeerID = c.Lo
	if c.Lo == viewer {
		c.PeerID = c.Hi
	}
	return c, nil
}

// dmUnreadTx counts, up to schema.MaxUnreadCount, the messages of convID after
// afterID that viewer did not write and that stand (a removed message is not
// unread). The caller has run dmViewTx.
func dmUnreadTx(ctx context.Context, q execer, convID, viewer, afterID int64) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM direct_messages m
		WHERE m.conversation_id = ? AND m.id > ? AND m.author_id <> ?
		  AND NOT EXISTS (SELECT 1 FROM direct_message_removals r WHERE r.message_id = m.id)
		LIMIT ?)`, convID, afterID, viewer, schema.MaxUnreadCount).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count unread in conversation %d: %w", convID, err)
	}
	return n, nil
}

// dmConversationTx is the conversation convID as viewer sees it, or
// ErrDMNotFound.
func dmConversationTx(ctx context.Context, q execer, viewer, convID int64) (DMConversation, error) {
	if _, err := dmViewTx(ctx, q, viewer, convID); err != nil {
		return DMConversation{}, err
	}
	c, err := scanDMConversation(q.QueryRowContext(ctx, dmSelectConversation+` WHERE c.id = ?2`, viewer, convID), viewer)
	if err != nil {
		return DMConversation{}, fmt.Errorf("store: read conversation %d: %w", convID, err)
	}
	if c.Unread, err = dmUnreadTx(ctx, q, convID, viewer, c.LastReadID); err != nil {
		return DMConversation{}, err
	}
	return c, nil
}

// DMConversationFor returns conversation convID as viewer sees it, or
// ErrDMNotFound.
func (s *Store) DMConversationFor(ctx context.Context, viewer, convID int64) (DMConversation, error) {
	var out DMConversation
	err := s.dmReadTx(ctx, func(tx execer) (err error) {
		out, err = dmConversationTx(ctx, tx, viewer, convID)
		return err
	})
	return out, err
}

// DMListCursor is where the next page of ListDMConversations starts: the
// sort key of the last conversation of the previous one. The zero value
// starts from the top. The server wraps it in an opaque cursor; it is never
// read by a client.
type DMListCursor struct {
	// Empty is true once the page has passed into the conversations with no
	// message, which come after every one that has a message.
	Empty bool
	// At is the last conversation's last_message_at in milliseconds (unused
	// when Empty) and ID its id.
	At, ID int64
}

// IsZero reports whether c starts from the top.
func (c DMListCursor) IsZero() bool { return c == DMListCursor{} }

// ListDMConversations returns up to limit of viewer's conversations in state
// (open or archived: a hidden conversation is in no list) after cursor, in
// the order of schema.ListDMConversationsResponseV1: last_message_at
// descending (ties by id descending), then those with no message, by id
// descending. A conversation with no message is listed only for a viewer who
// opened it (the visibility rule); the other participant has none until the
// first message. A viewer who does not stand gets none.
func (s *Store) ListDMConversations(ctx context.Context, viewer int64, state schema.DMState, cursor DMListCursor, limit int) ([]DMConversation, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list conversations: limit must be positive, got %d", limit)
	}
	if state != schema.DMStateOpen && state != schema.DMStateArchived {
		return nil, errors.New("store: list conversations: only the open and archived lists exist")
	}
	if cursor.ID < 0 || cursor.At < 0 || (cursor.Empty && cursor.At != 0) {
		return nil, errors.New("store: list conversations: a bad cursor")
	}
	// ?3 is the cursor's group (0 has messages, 1 none), ?4 its time, ?5 its
	// id: the sort key is (no message, last_message_at DESC, id DESC).
	group := 0
	if cursor.Empty {
		group = 1
	}
	var out []DMConversation
	err := s.dmReadTx(ctx, func(tx execer) error {
		rows, err := tx.QueryContext(ctx, dmSelectConversation+` WHERE `+dmVisibleCond+` AND COALESCE(vs.state, 'open') = ?2
			AND (?3 = 0 AND ?4 = 0 AND ?5 = 0
			  OR (lm.id IS NULL) > ?3
			  OR (lm.id IS NULL) = ?3 AND (?3 = 1 AND c.id < ?5
			       OR ?3 = 0 AND (lm.created_at < ?4 OR lm.created_at = ?4 AND c.id < ?5)))
			ORDER BY lm.id IS NULL, lm.created_at DESC, c.id DESC LIMIT ?6`,
			viewer, string(state), group, cursor.At, cursor.ID, limit)
		if err != nil {
			return fmt.Errorf("store: list conversations of %d: %w", viewer, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			c, err := scanDMConversation(rows, viewer)
			if err != nil {
				return fmt.Errorf("store: list conversations of %d: %w", viewer, err)
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: list conversations of %d: %w", viewer, err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for i := range out {
			if out[i].Unread, err = dmUnreadTx(ctx, tx, out[i].ID, viewer, out[i].LastReadID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// CursorAfter is the cursor that continues a list after c.
func (c DMConversation) CursorAfter() DMListCursor {
	if c.LastMessageID == 0 {
		return DMListCursor{Empty: true, ID: c.ID}
	}
	return DMListCursor{At: c.LastMessageAt.UnixMilli(), ID: c.ID}
}

// DMMessage is one message of a conversation as its viewer reads it: a
// removed one is the tombstone, with the author's removal body, no files and
// no edit time.
type DMMessage struct {
	ID             int64
	ConversationID int64
	AuthorID       int64
	Body           string
	FileIDs        []int64
	CreatedAt      time.Time
	// EditedAt is zero for a message never edited, and for a removed one.
	EditedAt time.Time
	Removed  bool
	// ClientNonce is the nonce the author sent it with, set only on the
	// author's own copies: the peer's never carry it.
	ClientNonce string
	// Revision is the conversation's revision at this copy's latest change:
	// its send, its latest edit or its removal.
	Revision int64
}

// dmMessageColumns selects what a DMMessage is made of from direct_messages m
// left-joined to direct_message_removals r. The original body and edit time
// of a removed message are not selected, so no code path can hand them out.
const dmMessageColumns = `m.id, m.conversation_id, m.author_id,
	CASE WHEN r.message_id IS NULL THEN m.body ELSE '' END, m.created_at,
	CASE WHEN r.message_id IS NULL THEN m.edited_at END,
	r.message_id IS NOT NULL, m.client_nonce, m.revision`

const dmMessageFrom = ` FROM direct_messages m LEFT JOIN direct_message_removals r ON r.message_id = m.id`

// scanDMMessagesTx reads the rows of a statement selecting dmMessageColumns,
// projects them for viewer and loads their files. It closes rows.
func scanDMMessagesTx(ctx context.Context, q execer, rows *sql.Rows, viewer int64) ([]DMMessage, error) {
	defer func() { _ = rows.Close() }()
	var msgs []DMMessage
	byID := map[int64]int{}
	for rows.Next() {
		var m DMMessage
		var created int64
		var edited sql.NullInt64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.AuthorID, &m.Body, &created, &edited, &m.Removed, &m.ClientNonce, &m.Revision); err != nil {
			return nil, err
		}
		m.CreatedAt = time.UnixMilli(created)
		if m.Removed {
			m.Body = schema.MessageRemovedByAuthorBody
		} else if edited.Valid {
			m.EditedAt = time.UnixMilli(edited.Int64)
		}
		if m.AuthorID != viewer {
			m.ClientNonce = ""
		}
		byID[m.ID] = len(msgs)
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return msgs, nil
	}
	ids := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if !m.Removed {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return msgs, nil
	}
	frows, err := q.QueryContext(ctx, `SELECT message_id, id FROM dm_files WHERE message_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)
		ORDER BY message_id, position, id`, ids...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = frows.Close() }()
	for frows.Next() {
		var mid, fid int64
		if err := frows.Scan(&mid, &fid); err != nil {
			return nil, err
		}
		i := byID[mid]
		msgs[i].FileIDs = append(msgs[i].FileIDs, fid)
	}
	return msgs, frows.Err()
}

// dmMessageTx reads message messageID of conversation convID as viewer reads
// it, or ErrMessageNotFound; the caller has run dmViewTx.
func dmMessageTx(ctx context.Context, q execer, convID, viewer, messageID int64) (DMMessage, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+dmMessageColumns+dmMessageFrom+` WHERE m.id = ? AND m.conversation_id = ?`, messageID, convID)
	if err != nil {
		return DMMessage{}, fmt.Errorf("store: read message %d: %w", messageID, err)
	}
	msgs, err := scanDMMessagesTx(ctx, q, rows, viewer)
	if err != nil {
		return DMMessage{}, fmt.Errorf("store: read message %d: %w", messageID, err)
	}
	if len(msgs) != 1 {
		return DMMessage{}, ErrMessageNotFound
	}
	return msgs[0], nil
}

// DMMessageFor returns one message of conversation convID as viewer reads it:
// ErrDMNotFound when viewer may not see the conversation, ErrMessageNotFound
// when it has no such message.
func (s *Store) DMMessageFor(ctx context.Context, viewer, convID, messageID int64) (DMMessage, error) {
	var out DMMessage
	err := s.dmReadTx(ctx, func(tx execer) (err error) {
		if _, err = dmViewTx(ctx, tx, viewer, convID); err != nil {
			return err
		}
		out, err = dmMessageTx(ctx, tx, convID, viewer, messageID)
		return err
	})
	return out, err
}

// ListDMMessagesPage returns up to limit messages of conversation convID as
// viewer reads them, in the direction of page (the channel MessagePage's
// rules: After with ascending reads, Before with newest-first ones), or
// ErrDMNotFound. Removed messages are in the page, as tombstones.
func (s *Store) ListDMMessagesPage(ctx context.Context, viewer, convID int64, page MessagePage, limit int) ([]DMMessage, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list direct messages: limit must be positive, got %d", limit)
	}
	if (page.NewestFirst && page.After != 0) || (!page.NewestFirst && page.Before != 0) {
		return nil, errors.New("store: list direct messages: after goes with ascending reads and before with newest-first ones")
	}
	if page.After < 0 || page.Before < 0 {
		return nil, errors.New("store: list direct messages: a cursor is never negative")
	}
	before := page.Before
	if before == 0 {
		before = math.MaxInt64
	}
	order := "ASC"
	if page.NewestFirst {
		order = "DESC"
	}
	var out []DMMessage
	err := s.dmReadTx(ctx, func(tx execer) error {
		if _, err := dmViewTx(ctx, tx, viewer, convID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+dmMessageColumns+dmMessageFrom+
			` WHERE m.conversation_id = ? AND m.id > ? AND m.id < ? ORDER BY m.id `+order+` LIMIT ?`,
			convID, page.After, before, limit)
		if err != nil {
			return fmt.Errorf("store: list messages of conversation %d: %w", convID, err)
		}
		if out, err = scanDMMessagesTx(ctx, tx, rows, viewer); err != nil {
			return fmt.Errorf("store: list messages of conversation %d: %w", convID, err)
		}
		return nil
	})
	return out, err
}

// ListDMChanges returns up to limit messages of conversation convID whose
// revision is above after, each once as it is now (its send, its latest edit
// or its tombstone), ascending by revision, as viewer reads them;
// ErrDMNotFound when viewer may not see the conversation. It is a read, so it
// asks for visibility and nothing about contact: a block or a switch does not
// refuse it. Removed messages are in it, as tombstones.
func (s *Store) ListDMChanges(ctx context.Context, viewer, convID, after int64, limit int) ([]DMMessage, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list changes: limit must be positive, got %d", limit)
	}
	if after < 0 {
		return nil, errors.New("store: list changes: a revision is never negative")
	}
	var out []DMMessage
	err := s.dmReadTx(ctx, func(tx execer) error {
		if _, err := dmViewTx(ctx, tx, viewer, convID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+dmMessageColumns+dmMessageFrom+
			` WHERE m.conversation_id = ? AND m.revision > ? ORDER BY m.revision ASC LIMIT ?`,
			convID, after, limit)
		if err != nil {
			return fmt.Errorf("store: list changes of conversation %d: %w", convID, err)
		}
		if out, err = scanDMMessagesTx(ctx, tx, rows, viewer); err != nil {
			return fmt.Errorf("store: list changes of conversation %d: %w", convID, err)
		}
		return nil
	})
	return out, err
}

// dmNextRevisionTx is the revision the next change of convID takes: one above
// the largest held. The write lock is held by the caller, so it is not taken
// twice.
func dmNextRevisionTx(ctx context.Context, q execer, convID int64) (int64, error) {
	var rev int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1 FROM direct_messages WHERE conversation_id = ?`, convID).Scan(&rev); err != nil {
		return 0, fmt.Errorf("store: next revision of conversation %d: %w", convID, err)
	}
	return rev, nil
}

// SearchDMMessages returns up to limit messages of conversation convID, as
// viewer reads them, whose body contains every one of words, newest first,
// with id below before (0 starts from the newest); ErrDMNotFound when viewer
// may not see the conversation. The words are plain words, as for
// SearchVisibleMessages: none reaches FTS5's query language. The conversation
// is one token of the match, so the search never leaves it, and a removed
// message matches nothing (its words leave the index when it is removed, and
// the statement leaves it out as well).
func (s *Store) SearchDMMessages(ctx context.Context, viewer, convID int64, words []string, before int64, limit int) ([]DMMessage, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: search direct messages: limit must be positive, got %d", limit)
	}
	if before < 0 {
		return nil, errors.New("store: search direct messages: a cursor is never negative")
	}
	var out []DMMessage
	err := s.dmReadTx(ctx, func(tx execer) error {
		if _, err := dmViewTx(ctx, tx, viewer, convID); err != nil {
			return err
		}
		all := ftsAllWords(words)
		if all == "" {
			return nil
		}
		match := fmt.Sprintf(`fts_conversation : "d%d" AND body : (%s)`, convID, all)
		rows, err := tx.QueryContext(ctx, `SELECT `+dmMessageColumns+`
			FROM direct_messages_fts(?) ix
			JOIN direct_messages m ON m.id = ix.rowid
			LEFT JOIN direct_message_removals r ON r.message_id = m.id
			WHERE m.conversation_id = ? AND r.message_id IS NULL
			  AND (? = 0 OR m.id < ?)
			ORDER BY m.id DESC LIMIT ?`, match, convID, before, before, limit)
		if err != nil {
			return fmt.Errorf("store: search conversation %d: %w", convID, err)
		}
		if out, err = scanDMMessagesTx(ctx, tx, rows, viewer); err != nil {
			return fmt.Errorf("store: search conversation %d: %w", convID, err)
		}
		return nil
	})
	return out, err
}

// dmNewestMessageIDTx is the id of the newest message of convID, 0 when it
// has none. Removed messages count: their tombstones are in every list. The
// caller has run dmViewTx. MarkDMRead clamps a mark to it.
func dmNewestMessageIDTx(ctx context.Context, q execer, convID int64) (int64, error) {
	var id int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM direct_messages WHERE conversation_id = ?`, convID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: newest message of conversation %d: %w", convID, err)
	}
	return id, nil
}

// DMReadMark is a participant's read mark in one conversation and what is
// unread after it. The zero LastReadID is "never marked", and UpdatedAt is
// then zero too.
type DMReadMark struct {
	ConversationID int64
	LastReadID     int64
	Unread         int
	UpdatedAt      time.Time
}

// dmReadMarkTx reads viewer's mark in convID; the caller has run dmViewTx.
func dmReadMarkTx(ctx context.Context, q execer, convID, viewer int64) (DMReadMark, error) {
	m := DMReadMark{ConversationID: convID}
	var at int64
	err := q.QueryRowContext(ctx, `SELECT last_read_id, updated_at FROM dm_read_marks WHERE principal_id = ? AND conversation_id = ?`,
		viewer, convID).Scan(&m.LastReadID, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return DMReadMark{}, fmt.Errorf("store: read mark of conversation %d for principal %d: %w", convID, viewer, err)
	default:
		m.UpdatedAt = time.UnixMilli(at)
	}
	var uerr error
	m.Unread, uerr = dmUnreadTx(ctx, q, convID, viewer, m.LastReadID)
	return m, uerr
}

// DMReadMarkFor returns viewer's read mark in convID, or ErrDMNotFound.
func (s *Store) DMReadMarkFor(ctx context.Context, viewer, convID int64) (DMReadMark, error) {
	var out DMReadMark
	err := s.dmReadTx(ctx, func(tx execer) error {
		if _, err := dmViewTx(ctx, tx, viewer, convID); err != nil {
			return err
		}
		var err error
		out, err = dmReadMarkTx(ctx, tx, convID, viewer)
		return err
	})
	return out, err
}

// dmNonceReplayTx finds the message author already sent to convID with this
// client nonce no earlier than since, and returns its id, or 0 if none. The
// caller has run dmViewTx.
func dmNonceReplayTx(ctx context.Context, q execer, convID, author int64, nonce string, since time.Time) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM direct_messages
		WHERE conversation_id = ? AND author_id = ? AND client_nonce = ? AND created_at >= ?
		ORDER BY id DESC LIMIT 1`, convID, author, nonce, since.UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: look up client nonce in conversation %d: %w", convID, err)
	}
	return id, nil
}

// dmEditStateTx reads what an edit of messageID needs, for its author: the
// author, whether it was removed, its current body and time, and the number
// of revisions kept. ErrMessageNotFound when the conversation has no such
// message. The caller has run dmViewTx.
func dmEditStateTx(ctx context.Context, q execer, convID, messageID int64) (author int64, removed bool, body string, created, edited time.Time, revisions int, err error) {
	var c int64
	var e sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT m.author_id, r.message_id IS NOT NULL, m.body, m.created_at, m.edited_at,
		(SELECT COALESCE(MAX(revision), 0) FROM direct_message_revisions WHERE message_id = m.id)
		FROM direct_messages m LEFT JOIN direct_message_removals r ON r.message_id = m.id
		WHERE m.id = ? AND m.conversation_id = ?`, messageID, convID).Scan(&author, &removed, &body, &c, &e, &revisions)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, "", time.Time{}, time.Time{}, 0, ErrMessageNotFound
	}
	if err != nil {
		return 0, false, "", time.Time{}, time.Time{}, 0, fmt.Errorf("store: read message %d: %w", messageID, err)
	}
	created = time.UnixMilli(c)
	if e.Valid {
		edited = time.UnixMilli(e.Int64)
	}
	return author, removed, body, created, edited, revisions, nil
}

// dmMessageAuthorTx is the author of messageID in convID and whether it is
// already removed, or ErrMessageNotFound. The caller has run dmViewTx.
func dmMessageAuthorTx(ctx context.Context, q execer, convID, messageID int64) (author int64, removed bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT m.author_id, r.message_id IS NOT NULL
		FROM direct_messages m LEFT JOIN direct_message_removals r ON r.message_id = m.id
		WHERE m.id = ? AND m.conversation_id = ?`, messageID, convID).Scan(&author, &removed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrMessageNotFound
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: read message %d: %w", messageID, err)
	}
	return author, removed, nil
}

// ErrDMFileNotAttachable is a send naming a file that is not the author's to
// attach: unknown, another conversation's, another participant's, already
// attached, or named twice. One error for each, so a send never says which.
var ErrDMFileNotAttachable = errors.New("store: direct message file not attachable")

// dmAttachableTx reports whether file fileID is unattached, in convID and
// uploaded by author. The caller has run dmViewTx.
func dmAttachableTx(ctx context.Context, q execer, convID, author, fileID int64) (bool, error) {
	var ok bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM dm_files
		WHERE id = ? AND conversation_id = ? AND uploader_id = ? AND message_id IS NULL)`,
		fileID, convID, author).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: read file %d: %w", fileID, err)
	}
	return ok, nil
}

// DMFile is the metadata of a file of a conversation. The bytes are the blob
// store's, by SHA256.
type DMFile struct {
	ID             int64
	ConversationID int64
	UploaderID     int64
	Filename       string
	MediaType      string
	Size           int64
	SHA256         string
	CreatedAt      time.Time
}

// dmFileTx reads file fileID for viewer, who must be a participant who may
// see the conversation (dmViewTx). The file is readable when it is attached
// to a message that stands, or unattached and viewer uploaded it: a removed
// message's files go with it, and one participant's upload waiting for a send
// is not the other's to see. Every other case is ErrDMNotFound.
func dmFileTx(ctx context.Context, q execer, viewer, fileID int64) (DMFile, error) {
	var f DMFile
	var at int64
	err := q.QueryRowContext(ctx, `SELECT fl.id, fl.conversation_id, fl.uploader_id, fl.filename, fl.media_type, fl.size, fl.sha256, fl.created_at
		FROM dm_files fl
		WHERE fl.id = ?2 AND (
			(fl.message_id IS NULL AND fl.uploader_id = ?1)
			OR (fl.message_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM direct_message_removals r WHERE r.message_id = fl.message_id)))`,
		viewer, fileID).Scan(&f.ID, &f.ConversationID, &f.UploaderID, &f.Filename, &f.MediaType, &f.Size, &f.SHA256, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return DMFile{}, ErrDMNotFound
	}
	if err != nil {
		return DMFile{}, fmt.Errorf("store: read file %d: %w", fileID, err)
	}
	f.CreatedAt = time.UnixMilli(at)
	if _, err := dmViewTx(ctx, q, viewer, f.ConversationID); err != nil {
		return DMFile{}, err
	}
	return f, nil
}

// DMFileVisibleTo returns the metadata of file fileID if viewer may read it:
// the one rule for DM files, the conversation's visibility and the file's
// own (dmFileTx). Anything else is ErrDMNotFound, whether or not the file
// exists. Agents never read DM files: that is the caller's rule
// (humansOnlyFiles), and a viewer here is a human who stands.
func (s *Store) DMFileVisibleTo(ctx context.Context, viewer, fileID int64) (DMFile, error) {
	var out DMFile
	err := s.dmReadTx(ctx, func(tx execer) (err error) {
		out, err = dmFileTx(ctx, tx, viewer, fileID)
		return err
	})
	return out, err
}
