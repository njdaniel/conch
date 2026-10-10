package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrNotFound is returned when a requested store entity does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrDuplicate is returned when a create operation violates a uniqueness
// constraint (e.g. a channel or principal name that already exists).
var ErrDuplicate = errors.New("store: duplicate")

// isUniqueConstraintErr reports whether err is a SQLite UNIQUE constraint
// failure. Error.Code carries the extended result code.
func isUniqueConstraintErr(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// PrincipalKind distinguishes humans from agents (ADR-000 D1).
type PrincipalKind string

const (
	PrincipalHuman PrincipalKind = "human"
	PrincipalAgent PrincipalKind = "agent"
)

// Role is a principal's administrative role (issue #89). Operators administer
// the instance; members do not. It is not a wire field of PrincipalV0.
type Role string

const (
	RoleOperator Role = "operator"
	RoleMember   Role = "member"
)

// Principal is a human or agent identity.
type Principal struct {
	ID        int64
	Kind      PrincipalKind
	Name      string
	Role      Role
	CreatedAt time.Time
	// DisabledAt is when the principal was disabled (issue #101); nil means
	// enabled. A disabled principal cannot authenticate.
	DisabledAt *time.Time
}

// Channel is a named message stream.
type Channel struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// Message is a single rendered message in a channel, with an optional typed
// machine payload.
type Message struct {
	ID        int64
	ChannelID int64
	AuthorID  int64
	Body      string
	Payload   *schema.Payload
	CreatedAt time.Time
}

// AuditEvent is one append-only entry in the audit log. Actor is free text
// (e.g. "principal:3" or "system") rather than a foreign key so the log
// outlives whatever it refers to.
type AuditEvent struct {
	ID        int64
	Actor     string
	Action    string
	Subject   string
	Detail    string
	CreatedAt time.Time
}

// CreatePrincipal registers a human or agent identity. Names are unique.
func (s *Store) CreatePrincipal(ctx context.Context, kind PrincipalKind, name string) (Principal, error) {
	if kind != PrincipalHuman && kind != PrincipalAgent {
		return Principal{}, fmt.Errorf("store: invalid principal kind %q", kind)
	}
	// SQLite stores timestamps at millisecond precision. Normalize the
	// returned value to the same precision so a create response exactly
	// describes what a subsequent read returns.
	now := time.Now().Truncate(time.Millisecond)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO principals (kind, name, created_at) VALUES (?, ?, ?)",
		string(kind), name, now.UnixMilli())
	if isUniqueConstraintErr(err) {
		return Principal{}, fmt.Errorf("store: create principal %q: %w", name, ErrDuplicate)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("store: create principal %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Principal{}, fmt.Errorf("store: create principal %q: %w", name, err)
	}
	return Principal{ID: id, Kind: kind, Name: name, Role: RoleMember, CreatedAt: now}, nil
}

// CreateChannel creates a named channel. Names are unique.
func (s *Store) CreateChannel(ctx context.Context, name string) (Channel, error) {
	// SQLite stores timestamps at millisecond precision. Normalize the
	// returned value to the same precision so a create response exactly
	// describes what a subsequent read returns.
	now := time.Now().Truncate(time.Millisecond)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO channels (name, created_at) VALUES (?, ?)",
		name, now.UnixMilli())
	if isUniqueConstraintErr(err) {
		return Channel{}, fmt.Errorf("store: create channel %q: %w", name, ErrDuplicate)
	}
	if err != nil {
		return Channel{}, fmt.Errorf("store: create channel %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Channel{}, fmt.Errorf("store: create channel %q: %w", name, err)
	}
	return Channel{ID: id, Name: name, CreatedAt: now}, nil
}

// ChannelByName returns the channel with name. It returns ErrNotFound when no
// such channel exists.
func (s *Store) ChannelByName(ctx context.Context, name string) (Channel, error) {
	var ch Channel
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name, created_at FROM channels WHERE name = ?", name,
	).Scan(&ch.ID, &ch.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, fmt.Errorf("store: find channel %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Channel{}, fmt.Errorf("store: find channel %q: %w", name, err)
	}
	ch.CreatedAt = time.UnixMilli(createdAt)
	return ch, nil
}

// ListChannels returns every channel ordered by id ascending. It returns an
// empty, non-nil slice when there are none.
func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, created_at FROM channels ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	channels := []Channel{}
	for rows.Next() {
		var ch Channel
		var createdAt int64
		if err := rows.Scan(&ch.ID, &ch.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan channel: %w", err)
		}
		ch.CreatedAt = time.UnixMilli(createdAt)
		channels = append(channels, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	return channels, nil
}

// ChannelByID returns the channel with id. It returns ErrNotFound when no
// such channel exists.
func (s *Store) ChannelByID(ctx context.Context, id int64) (Channel, error) {
	var ch Channel
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name, created_at FROM channels WHERE id = ?", id,
	).Scan(&ch.ID, &ch.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, fmt.Errorf("store: find channel %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Channel{}, fmt.Errorf("store: find channel %d: %w", id, err)
	}
	ch.CreatedAt = time.UnixMilli(createdAt)
	return ch, nil
}

// PrincipalByID returns the principal with id. It returns ErrNotFound when no
// such principal exists.
func (s *Store) PrincipalByID(ctx context.Context, id int64) (Principal, error) {
	var p Principal
	var kind, role string
	var createdAt int64
	var disabledAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT id, kind, name, role, created_at, disabled_at FROM principals WHERE id = ?", id,
	).Scan(&p.ID, &kind, &p.Name, &role, &createdAt, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, fmt.Errorf("store: find principal %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("store: find principal %d: %w", id, err)
	}
	p.Kind = PrincipalKind(kind)
	p.Role = Role(role)
	p.CreatedAt = time.UnixMilli(createdAt)
	if disabledAt.Valid {
		t := time.UnixMilli(disabledAt.Int64)
		p.DisabledAt = &t
	}
	return p, nil
}

// InsertMessage appends a message to a channel. The channel and author must
// exist (enforced by foreign keys).
func (s *Store) InsertMessage(ctx context.Context, channelID, authorID int64, body string) (Message, error) {
	return s.InsertMessageV1(ctx, channelID, authorID, body, nil)
}

// InsertMessageV1 appends a message and its author-attributed audit event in
// one transaction. The payload is stored opaquely for forward compatibility.
func (s *Store) InsertMessageV1(
	ctx context.Context, channelID, authorID int64, body string, payload *schema.Payload,
) (Message, error) {
	// SQLite stores message timestamps at millisecond precision. Normalize the
	// returned value to the same precision so a POST response exactly describes
	// what a subsequent read returns.
	now := time.Now().Truncate(time.Millisecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, fmt.Errorf("store: begin insert message in channel %d: %w", channelID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var payloadSchema any
	var payloadJSON any
	if payload != nil {
		payloadSchema = payload.Schema
		payloadJSON = []byte(payload.Data)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (channel_id, author_id, body, created_at, payload_schema, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		channelID, authorID, body, now.UnixMilli(), payloadSchema, payloadJSON)
	if err != nil {
		return Message{}, fmt.Errorf("store: insert message in channel %d: %w", channelID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("store: insert message in channel %d: %w", channelID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_events (actor, action, subject, detail, created_at) VALUES (?, ?, ?, '', ?)`,
		fmt.Sprintf("principal:%d", authorID), "message.post", fmt.Sprintf("message:%d", id), now.UnixMilli()); err != nil {
		return Message{}, fmt.Errorf("store: audit inserted message %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return Message{}, fmt.Errorf("store: commit inserted message %d: %w", id, err)
	}
	return Message{ID: id, ChannelID: channelID, AuthorID: authorID, Body: body, Payload: payload, CreatedAt: now}, nil
}

// ListMessages returns up to limit messages in channelID with ID greater than
// afterID, in ascending ID order (insertion order). Pass afterID = 0 to start
// from the beginning; pass the last message's ID to fetch the next page.
func (s *Store) ListMessages(ctx context.Context, channelID int64, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list messages: limit must be positive, got %d", limit)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel_id, author_id, body, payload_schema, payload_json, created_at
		 FROM messages WHERE channel_id = ? AND id > ?
		 ORDER BY id ASC LIMIT ?`,
		channelID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []Message
	for rows.Next() {
		var m Message
		var payloadSchema sql.NullString
		var payloadJSON []byte
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.AuthorID, &m.Body, &payloadSchema, &payloadJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
		}
		m.CreatedAt = time.UnixMilli(createdAt)
		if payloadSchema.Valid {
			m.Payload = &schema.Payload{Schema: payloadSchema.String, Data: payloadJSON}
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
	}
	return msgs, nil
}

// AppendAuditEvent appends an entry to the audit log. There is deliberately
// no corresponding update or delete: the log is append-only.
func (s *Store) AppendAuditEvent(ctx context.Context, actor, action, subject, detail string) (AuditEvent, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO audit_events (actor, action, subject, detail, created_at) VALUES (?, ?, ?, ?, ?)",
		actor, action, subject, detail, now.UnixMilli())
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: append audit event %q: %w", action, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: append audit event %q: %w", action, err)
	}
	return AuditEvent{ID: id, Actor: actor, Action: action, Subject: subject, Detail: detail, CreatedAt: now}, nil
}

// LastAuditEvent returns the most recent audit event with the given action,
// or ErrNotFound when there is none.
func (s *Store) LastAuditEvent(ctx context.Context, action string) (AuditEvent, error) {
	var e AuditEvent
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, actor, action, subject, detail, created_at
		 FROM audit_events WHERE action = ?
		 ORDER BY id DESC LIMIT 1`, action).
		Scan(&e.ID, &e.Actor, &e.Action, &e.Subject, &e.Detail, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AuditEvent{}, fmt.Errorf("store: last audit event %q: %w", action, ErrNotFound)
	}
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: last audit event %q: %w", action, err)
	}
	e.CreatedAt = time.UnixMilli(createdAt)
	return e, nil
}

// LastAuditID returns the id of the newest audit event, or 0 for an empty log.
func (s *Store) LastAuditID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM audit_events").Scan(&id); err != nil {
		return 0, fmt.Errorf("store: last audit id: %w", err)
	}
	return id, nil
}

// ListAuditEvents returns up to limit audit events with ID greater than
// afterID, in ascending ID order.
func (s *Store) ListAuditEvents(ctx context.Context, afterID int64, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list audit events: limit must be positive, got %d", limit)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, actor, action, subject, detail, created_at
		 FROM audit_events WHERE id > ?
		 ORDER BY id ASC LIMIT ?`,
		afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Subject, &e.Detail, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list audit events: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit events: %w", err)
	}
	return events, nil
}
