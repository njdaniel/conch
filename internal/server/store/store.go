// Package store is conchd's embedded SQLite storage layer (ADR-002:
// modernc.org/sqlite, WAL mode, no cgo). It owns the database schema via
// embedded migrations and exposes the queries the server core needs.
//
// Audit events are append-only at this API surface: the package provides no
// function that updates or deletes a row in audit_events, and the schema
// enforces the same with triggers.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// migrations is the ordered list of schema migrations. Each migration is a
// slice of individual SQL statements executed sequentially in a single
// transaction. The schema version of a database (PRAGMA user_version) is the
// number of migrations applied, so entries must never be edited or reordered
// once released — only appended.
var migrations = [][]string{
	// 1: P0 baseline — principals, channels, messages, audit_events.
	// Message payload columns are deferred to P1 (issue #2).
	{
		`CREATE TABLE principals (
	id         INTEGER PRIMARY KEY,
	kind       TEXT    NOT NULL CHECK (kind IN ('human', 'agent')),
	name       TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
)`,
		`CREATE TABLE channels (
	id         INTEGER PRIMARY KEY,
	name       TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
)`,
		`CREATE TABLE messages (
	id         INTEGER PRIMARY KEY,
	channel_id INTEGER NOT NULL REFERENCES channels (id),
	author_id  INTEGER NOT NULL REFERENCES principals (id),
	body       TEXT    NOT NULL,
	created_at INTEGER NOT NULL
)`,
		`CREATE INDEX messages_by_channel ON messages (channel_id, id)`,
		// No foreign keys: the audit log must outlive whatever it refers to, so
		// the actor is recorded as text (e.g. "principal:3" or "system").
		`CREATE TABLE audit_events (
	id         INTEGER PRIMARY KEY,
	actor      TEXT    NOT NULL,
	action     TEXT    NOT NULL,
	subject    TEXT    NOT NULL DEFAULT '',
	detail     TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
)`,
		`CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events
BEGIN
	SELECT RAISE(ABORT, 'audit_events is append-only');
END`,
		`CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
BEGIN
	SELECT RAISE(ABORT, 'audit_events is append-only');
END`,
	},
}

// Store is the embedded SQLite database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the database at path, enables WAL mode
// and foreign-key enforcement on every connection, and applies any pending
// migrations. Migrations are idempotent: reopening an up-to-date database is
// a no-op.
func Open(ctx context.Context, path string) (*Store, error) {
	// journal_mode is persistent but the other pragmas are per-connection,
	// so they are set in the DSN to cover every pooled connection.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping verifies that the database is reachable and accepting queries.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// migrate applies every migration past the database's current schema version,
// each in its own transaction, bumping PRAGMA user_version as it goes.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than this binary (max %d)", version, len(migrations))
	}

	for i := version; i < len(migrations); i++ {
		if err := s.applyMigration(ctx, i); err != nil {
			return fmt.Errorf("store: apply migration %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, i int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range migrations[i] {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	// PRAGMA does not accept bound parameters; i+1 is a trusted integer.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
		return err
	}
	return tx.Commit()
}
