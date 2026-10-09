package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
// This file is the only place that reads message rows. ListVisibleMessages is
// the one visibility function: every list in every protocol version, and the
// MCP read tool, go through it, so there is exactly one place where a scoped
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
	if limit <= 0 {
		return nil, fmt.Errorf("store: list messages: limit must be positive, got %d", limit)
	}
	const columns = `m.id, m.channel_id, m.author_id, m.body, m.payload_schema, m.payload_json, m.created_at, m.audience_kind, m.net_id`
	var rows *sql.Rows
	var err error
	if reader.Scoped && reader.PrincipalID > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+columns+` FROM messages m
			 WHERE m.channel_id = ? AND m.id > ?
			   AND (m.audience_kind IS NULL OR (
			        EXISTS (SELECT 1 FROM message_recipients r
			                WHERE r.message_id = m.id AND r.principal_id = ?)
			    AND EXISTS (SELECT 1 FROM channel_members c
			                WHERE c.channel_id = m.channel_id AND c.principal_id = ?)
			    AND EXISTS (SELECT 1 FROM principals p
			                WHERE p.id = ? AND p.disabled_at IS NULL)))
			 ORDER BY m.id ASC LIMIT ?`,
			channelID, afterID, reader.PrincipalID, reader.PrincipalID, reader.PrincipalID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+columns+` FROM messages m
			 WHERE m.channel_id = ? AND m.id > ? AND m.audience_kind IS NULL
			 ORDER BY m.id ASC LIMIT ?`,
			channelID, afterID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []Message
	var whispers []int // indexes into msgs of principals-audience messages
	for rows.Next() {
		var m Message
		var payloadSchema, kind sql.NullString
		var payloadJSON []byte
		var netID sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.AuthorID, &m.Body, &payloadSchema, &payloadJSON, &createdAt, &kind, &netID); err != nil {
			return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
		}
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
		return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
	}
	return msgs, s.fillWhisperAudiences(ctx, msgs, whispers)
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

		var payloadSchema, payloadJSON any
		if p.Payload != nil {
			payloadSchema = p.Payload.Schema
			payloadJSON = []byte(p.Payload.Data)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO messages (channel_id, author_id, body, created_at, payload_schema, payload_json, audience_kind, net_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ChannelID, p.AuthorID, p.Body, now.UnixMilli(), payloadSchema, payloadJSON, string(p.Audience.Kind), netID)
		if err != nil {
			return fmt.Errorf("store: insert scoped message in channel %d: %w", p.ChannelID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: insert scoped message in channel %d: %w", p.ChannelID, err)
		}
		for _, r := range recipients {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO message_recipients (message_id, principal_id) VALUES (?, ?)", id, r); err != nil {
				return fmt.Errorf("store: store recipient %d of message %d: %w", r, id, err)
			}
		}
		actor := principalActor(p.AuthorID)
		subject := fmt.Sprintf("message:%d", id)
		if err := appendAuditEventTx(ctx, tx, actor, "message.post", subject, "", now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditMessageScoped, subject,
			scopedDetail(p.ChannelID, audience, recipients), now); err != nil {
			return err
		}
		msg = Message{ID: id, ChannelID: p.ChannelID, AuthorID: p.AuthorID, Body: p.Body, Payload: p.Payload,
			CreatedAt: now, Audience: &audience}
		return nil
	})
	if err != nil {
		return Message{}, nil, err
	}
	return msg, recipients, nil
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
