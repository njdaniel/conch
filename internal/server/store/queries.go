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
	// HiddenAt is when the principal was hidden (issue #445); nil means not
	// hidden. A hidden principal is always disabled.
	HiddenAt *time.Time
	// InvitedBy and InviteID are the principal and the invite this account
	// came through (issue #436); zero for an account no invite made.
	InvitedBy int64
	InviteID  int64
	// MayInvite is the right to make personal invites. Operators may always
	// invite whatever this says; nobody else holds it until granted.
	MayInvite bool
	// VoiceAllowed is whether the principal may use voice. True for every
	// account unless an invite made without it. Every voice route refuses a
	// person without it (voicePresenceChannel, internal/server/voice.go).
	VoiceAllowed bool
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
	// Audience is nil for a channel-wide message. For a scoped message it is
	// the wire form: a net audience carries only the net id, a principals
	// audience the full normalized list including the author. The resolved
	// recipients of a net message are deliberately not part of a Message; they
	// are stored and returned only by InsertScopedMessage for fan-out.
	Audience *schema.Audience
	// ThreadRootID is the message this one replies to; zero when it is not a reply.
	ThreadRootID int64
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

// CreatePrincipal is CreatePrincipalAs without the audit row. It is for
// seeding a store in tests; no route creates a principal through it
// (TestCreatesAreAudited in the server package checks).
func (s *Store) CreatePrincipal(ctx context.Context, kind PrincipalKind, name string) (Principal, error) {
	return s.createPrincipal(ctx, "", kind, name)
}

// CreatePrincipalAs registers a human or agent identity and writes
// principal_created (detail kind=<kind>), attributed to actor, in the
// creating transaction (issue #445). Names are unique by their key (issue
// #441): a name that clashes with an existing one, by case or by look-alike
// characters, or that is reserved for hidden principals, is ErrDuplicate; a
// name with nothing a reader can see is ErrNameInvalid.
func (s *Store) CreatePrincipalAs(ctx context.Context, actor string, kind PrincipalKind, name string) (Principal, error) {
	if actor == "" {
		return Principal{}, errors.New("store: create principal: an actor is required")
	}
	return s.createPrincipal(ctx, actor, kind, name)
}

// createPrincipal is CreatePrincipalAs, with no audit row when actor is
// empty.
func (s *Store) createPrincipal(ctx context.Context, actor string, kind PrincipalKind, name string) (Principal, error) {
	if kind != PrincipalHuman && kind != PrincipalAgent {
		return Principal{}, fmt.Errorf("store: invalid principal kind %q", kind)
	}
	// SQLite stores timestamps at millisecond precision. Normalize the
	// returned value to the same precision so a create response exactly
	// describes what a subsequent read returns.
	now := time.Now().Truncate(time.Millisecond)
	var id int64
	// IMMEDIATE, so the clash check and the insert are one step: a second
	// create of the same key waits for this one and then sees its row.
	err := s.withImmediateTx(ctx, func(tx execer) error {
		keys, err := claimNameTx(ctx, tx, name, 0)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO principals (kind, name, name_key, name_skel, created_at) VALUES (?, ?, ?, ?, ?)",
			string(kind), name, keys.Folded, keys.Skeletal, now.UnixMilli())
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("store: create principal %q: %w", name, ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: create principal %q: %w", name, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: create principal %q: %w", name, err)
		}
		if actor == "" {
			return nil
		}
		return appendAuditEventTx(ctx, tx, actor, AuditPrincipalCreated, principalActor(id), "kind="+string(kind), now)
	})
	if err != nil {
		return Principal{}, err
	}
	return Principal{ID: id, Kind: kind, Name: name, Role: RoleMember, CreatedAt: now, VoiceAllowed: true}, nil
}

// CreateChannel creates a named channel with no members and no audit row. It
// is for seeding a store in tests; the route creates channels through
// CreateChannelAs (TestCreatesAreAudited in the server package checks).
// Names are unique.
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
	p, found, err := queryPrincipal(ctx, s.db, id)
	if err == nil && !found {
		err = fmt.Errorf("store: find principal %d: %w", id, ErrNotFound)
	}
	return p, err
}

// queryPrincipal reads principal id through q, a database or a transaction.
// found is false, with no error, when there is none.
func queryPrincipal(ctx context.Context, q execer, id int64) (p Principal, found bool, err error) {
	var kind, role string
	var createdAt int64
	var disabledAt, hiddenAt, invitedBy, inviteID sql.NullInt64
	err = q.QueryRowContext(ctx,
		`SELECT id, kind, name, role, created_at, disabled_at, hidden_at, invited_by, invite_id, may_invite, voice_allowed
		 FROM principals WHERE id = ?`, id,
	).Scan(&p.ID, &kind, &p.Name, &role, &createdAt, &disabledAt, &hiddenAt, &invitedBy, &inviteID, &p.MayInvite, &p.VoiceAllowed)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, false, nil
	}
	if err != nil {
		return Principal{}, false, fmt.Errorf("store: find principal %d: %w", id, err)
	}
	p.Kind = PrincipalKind(kind)
	p.Role = Role(role)
	p.CreatedAt = time.UnixMilli(createdAt)
	p.InvitedBy = invitedBy.Int64
	p.InviteID = inviteID.Int64
	if disabledAt.Valid {
		t := time.UnixMilli(disabledAt.Int64)
		p.DisabledAt = &t
	}
	if hiddenAt.Valid {
		t := time.UnixMilli(hiddenAt.Int64)
		p.HiddenAt = &t
	}
	return p, true, nil
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
	var id int64
	// BEGIN IMMEDIATE, like the scoped insert: the message insert fires the
	// full-text index trigger (migration 15), and with a deferred transaction
	// concurrent posts failed with SQLITE_BUSY that the busy timeout does not
	// retry. Taking the write lock first makes them queue instead.
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var err error
		id, err = insertChannelWideTx(ctx, tx, channelID, authorID, body, payload, 0, now)
		return err
	})
	if err != nil {
		return Message{}, err
	}
	return Message{ID: id, ChannelID: channelID, AuthorID: authorID, Body: body, Payload: payload, CreatedAt: now}, nil
}

// insertChannelWideTx stores a channel-wide message (a reply to a channel-wide
// root when rootID is not zero), its author-attributed audit event and any file
// it attaches, in the caller's transaction, and returns its id.
func insertChannelWideTx(
	ctx context.Context, tx execer, channelID, authorID int64, body string, payload *schema.Payload, rootID int64, now time.Time,
) (int64, error) {
	var payloadSchema, payloadJSON, thread any
	if payload != nil {
		payloadSchema = payload.Schema
		payloadJSON = []byte(payload.Data)
	}
	if rootID > 0 {
		thread = rootID
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (channel_id, author_id, body, created_at, payload_schema, payload_json, thread_root_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		channelID, authorID, body, now.UnixMilli(), payloadSchema, payloadJSON, thread)
	if err != nil {
		return 0, fmt.Errorf("store: insert message in channel %d: %w", channelID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert message in channel %d: %w", channelID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_events (actor, action, subject, detail, created_at) VALUES (?, ?, ?, '', ?)`,
		fmt.Sprintf("principal:%d", authorID), "message.post", fmt.Sprintf("message:%d", id), now.UnixMilli()); err != nil {
		return 0, fmt.Errorf("store: audit inserted message %d: %w", id, err)
	}
	return id, attachFilesTx(ctx, tx, channelID, authorID, id, payload, now)
}

// AppendAuditEvent appends an entry to the audit log. There is deliberately
// no corresponding update or delete: the log is append-only.
func (s *Store) AppendAuditEvent(ctx context.Context, actor, action, subject, detail string) (AuditEvent, error) {
	return s.AppendAuditEventAt(ctx, actor, action, subject, detail, time.Now())
}

// AppendAuditEventAt appends an entry whose time is at rather than the moment
// of the write. It is for events whose time the server fixes before it can
// write them: a transmit report is stamped at the instant it is applied, and
// an unreported transmission is timed at the first pass that saw it, which is
// known to be one only at a later pass (issue #135,
// docs/design/conch-voice.md §6). at is always the server's own clock; a
// caller must never pass a time a client supplied.
//
// Rows are still appended in the order they are written, so created_at is not
// guaranteed to rise with id: id order is the order of writing, created_at is
// when the thing happened.
func (s *Store) AppendAuditEventAt(ctx context.Context, actor, action, subject, detail string, at time.Time) (AuditEvent, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO audit_events (actor, action, subject, detail, created_at) VALUES (?, ?, ?, ?, ?)",
		actor, action, subject, detail, at.UnixMilli())
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: append audit event %q: %w", action, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: append audit event %q: %w", action, err)
	}
	return AuditEvent{ID: id, Actor: actor, Action: action, Subject: subject, Detail: detail, CreatedAt: at}, nil
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
