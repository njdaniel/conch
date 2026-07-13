package store

import (
	"context"
	"fmt"
	"time"
)

// PrincipalKind distinguishes humans from agents (ADR-000 D1).
type PrincipalKind string

const (
	PrincipalHuman PrincipalKind = "human"
	PrincipalAgent PrincipalKind = "agent"
)

// Principal is a human or agent identity.
type Principal struct {
	ID        int64
	Kind      PrincipalKind
	Name      string
	CreatedAt time.Time
}

// Channel is a named message stream.
type Channel struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// Message is a single rendered message in a channel. Typed machine payloads
// (D8) are deferred to P1.
type Message struct {
	ID        int64
	ChannelID int64
	AuthorID  int64
	Body      string
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
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO principals (kind, name, created_at) VALUES (?, ?, ?)",
		string(kind), name, now.UnixMilli())
	if err != nil {
		return Principal{}, fmt.Errorf("store: create principal %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Principal{}, fmt.Errorf("store: create principal %q: %w", name, err)
	}
	return Principal{ID: id, Kind: kind, Name: name, CreatedAt: now}, nil
}

// CreateChannel creates a named channel. Names are unique.
func (s *Store) CreateChannel(ctx context.Context, name string) (Channel, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO channels (name, created_at) VALUES (?, ?)",
		name, now.UnixMilli())
	if err != nil {
		return Channel{}, fmt.Errorf("store: create channel %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Channel{}, fmt.Errorf("store: create channel %q: %w", name, err)
	}
	return Channel{ID: id, Name: name, CreatedAt: now}, nil
}

// InsertMessage appends a message to a channel. The channel and author must
// exist (enforced by foreign keys).
func (s *Store) InsertMessage(ctx context.Context, channelID, authorID int64, body string) (Message, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO messages (channel_id, author_id, body, created_at) VALUES (?, ?, ?, ?)",
		channelID, authorID, body, now.UnixMilli())
	if err != nil {
		return Message{}, fmt.Errorf("store: insert message in channel %d: %w", channelID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("store: insert message in channel %d: %w", channelID, err)
	}
	return Message{ID: id, ChannelID: channelID, AuthorID: authorID, Body: body, CreatedAt: now}, nil
}

// ListMessages returns up to limit messages in channelID with ID greater than
// afterID, in ascending ID order (insertion order). Pass afterID = 0 to start
// from the beginning; pass the last message's ID to fetch the next page.
func (s *Store) ListMessages(ctx context.Context, channelID int64, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list messages: limit must be positive, got %d", limit)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel_id, author_id, body, created_at
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
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.AuthorID, &m.Body, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list messages in channel %d: %w", channelID, err)
		}
		m.CreatedAt = time.UnixMilli(createdAt)
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
