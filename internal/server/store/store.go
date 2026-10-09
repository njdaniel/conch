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
	"log/slog"
	"os"
	"path/filepath"

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
	// 2: MessageV1 typed payload persistence. NULL columns represent a message
	// without a payload.
	{
		`ALTER TABLE messages ADD COLUMN payload_schema TEXT`,
		`ALTER TABLE messages ADD COLUMN payload_json BLOB`,
	},
	// 3: P1 approvals (issue #12) — first-class approval entity with its own
	// state machine (docs/design/approval-object.md), decisions, and the shared
	// resolution store. Timestamps are unix milliseconds UTC throughout.
	{
		`CREATE TABLE approvals (
	id             INTEGER PRIMARY KEY,
	requester_id   INTEGER NOT NULL REFERENCES principals (id),
	channel_id     INTEGER NOT NULL REFERENCES channels (id),
	title          TEXT    NOT NULL,
	body           TEXT    NOT NULL,
	payload_schema TEXT    NOT NULL DEFAULT '',
	payload_json   TEXT    NOT NULL DEFAULT '',
	options_json   TEXT    NOT NULL,
	deadline       INTEGER NOT NULL,
	grace_deadline INTEGER NOT NULL,
	quorum         INTEGER NOT NULL CHECK (quorum >= 1),
	escalation_kind         TEXT    NOT NULL DEFAULT '',
	escalation_principal_id INTEGER NOT NULL DEFAULT 0,
	escalation_topic        TEXT    NOT NULL DEFAULT '',
	state          TEXT    NOT NULL CHECK (state IN ('pending', 'escalated', 'resolved', 'expired')),
	created_at     INTEGER NOT NULL
)`,
		`CREATE INDEX approvals_open_by_deadline ON approvals (state, deadline)`,
		`CREATE TABLE approval_decisions (
	id           INTEGER PRIMARY KEY,
	approval_id  INTEGER NOT NULL REFERENCES approvals (id),
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	option_id    TEXT    NOT NULL,
	reason       TEXT    NOT NULL CHECK (reason <> ''),
	created_at   INTEGER NOT NULL,
	UNIQUE (approval_id, principal_id)
)`,
		// The shared resolution store (approval-object.md §3): exactly one
		// resolution event per approval, stored as canonical
		// approval.resolution.v1 JSON — what waiters, pollers, and the audit
		// trail all read.
		`CREATE TABLE approval_resolutions (
	approval_id     INTEGER PRIMARY KEY REFERENCES approvals (id),
	outcome         TEXT    NOT NULL CHECK (outcome IN ('approved', 'rejected', 'custom', 'expired')),
	option_id       TEXT    NOT NULL DEFAULT '',
	resolution_json TEXT    NOT NULL,
	resolved_at     INTEGER NOT NULL
)`,
	},
	// 4: Webhook tokens map unauthenticated ingest requests to an existing
	// channel and principal identity.
	{
		`CREATE TABLE hooks (
	token        TEXT PRIMARY KEY,
	channel_id   INTEGER NOT NULL REFERENCES channels (id),
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	created_at   INTEGER NOT NULL
)`,
	},
	// 5: Agent capability manifests (issue #77, docs/design/agent-manifest.md).
	// One row per agent principal; the three policy lists are JSON text and the
	// manifest is always read whole. Creates the table only: existing agent
	// principals get no manifest, which means no access once enforcement lands
	// (#79). Timestamps are unix milliseconds UTC.
	{
		`CREATE TABLE agent_manifests (
	principal_id INTEGER PRIMARY KEY REFERENCES principals (id),
	display_name TEXT    NOT NULL,
	tier         TEXT    NOT NULL,
	capabilities TEXT    NOT NULL,
	channels     TEXT    NOT NULL,
	rate_limits  TEXT    NOT NULL,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
)`,
	},
	// 6: Bearer credentials (issue #78). Each row binds one token to exactly
	// one principal. Only the SHA-256 of the token (lowercase hex) is stored,
	// never the token. Revocation is a timestamp, not a delete. Creates the
	// table only: existing principals get no credentials. Timestamps are unix
	// milliseconds UTC.
	{
		`CREATE TABLE credentials (
	id           INTEGER PRIMARY KEY,
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	label        TEXT    NOT NULL,
	token_hash   TEXT    NOT NULL,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER,
	revoked_at   INTEGER
)`,
		`CREATE UNIQUE INDEX credentials_by_token_hash ON credentials (token_hash)`,
		`CREATE INDEX credentials_by_principal ON credentials (principal_id)`,
	},
	// 7: Principal roles (issue #89). Every existing principal becomes a
	// member; nobody is silently promoted. Operators are created only by
	// Store.BootstrapOperator.
	{
		`ALTER TABLE principals ADD COLUMN role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('operator','member'))`,
	},
	// 8: Channel membership (issue #90, ADR-003). An explicit member list per
	// channel. The backfill makes today's effective behaviour (every principal
	// can use every channel) explicit for existing data: one row per existing
	// (channel, principal) pair with added_by NULL. Channels and principals
	// created afterwards are not auto-joined. Timestamps are unix milliseconds
	// UTC.
	{
		`CREATE TABLE channel_members (
	channel_id   INTEGER NOT NULL REFERENCES channels (id),
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	added_by     INTEGER REFERENCES principals (id),
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (channel_id, principal_id)
)`,
		`CREATE INDEX channel_members_by_principal ON channel_members (principal_id)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at)
	SELECT c.id, p.id, NULL, CAST(strftime('%s', 'now') AS INTEGER) * 1000
	FROM channels c CROSS JOIN principals p`,
	},
	// 9: Principal disabling (issue #101). disabled_at is unix milliseconds
	// UTC; NULL means enabled. Existing principals stay enabled.
	{
		`ALTER TABLE principals ADD COLUMN disabled_at INTEGER`,
	},
	// 10: Hashed, revocable webhook hooks (issue #104). The plaintext token
	// primary key is replaced by token_hash (lowercase hex SHA-256, as
	// credentials), plus id, label and revoked_at. Existing rows are copied
	// across by migrationSteps[10], which hashes each token in Go; hashing
	// needs no new SQL function and the hook URLs already handed out keep
	// working. The old table is dropped by that step, with secure_delete on
	// (see Open) so its pages do not keep the plaintext tokens.
	{
		`ALTER TABLE hooks RENAME TO hooks_plaintext`,
		`CREATE TABLE hooks (
	id           INTEGER PRIMARY KEY,
	token_hash   TEXT    NOT NULL,
	label        TEXT    NOT NULL DEFAULT '',
	channel_id   INTEGER NOT NULL REFERENCES channels (id),
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	created_at   INTEGER NOT NULL,
	revoked_at   INTEGER
)`,
		`CREATE UNIQUE INDEX hooks_by_token_hash ON hooks (token_hash)`,
	},
}

// migrationSteps holds Go code that runs inside a migration's transaction
// after its SQL statements, for the rare change SQL cannot express. It is
// keyed by the migration's 1-based number (the schema version it produces).
var migrationSteps = map[int]func(ctx context.Context, tx *sql.Tx) error{
	10: hashPlaintextHooks,
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
	// The database holds the hashes of every bearer credential and hook
	// token and the whole message log, so it must never be readable by other
	// local users. Doing this before SQLite touches the file also fixes the
	// mode its -wal and -shm siblings are created with.
	if err := securePermissions(path); err != nil {
		return nil, err
	}

	// journal_mode is persistent but the other pragmas are per-connection,
	// so they are set in the DSN to cover every pooled connection.
	// secure_delete zeroes freed pages so a dropped or deleted secret does
	// not linger in the file.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=secure_delete(1)"

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

// securePermissions creates the database file with mode 0600 if it does not
// exist, and tightens it and its -wal/-shm siblings to 0600 if they are
// accessible to group or others. A tightening is reported in exactly one log
// line; a file that is already private logs nothing.
//
// path is a filesystem path, not a SQLite URI. Each file is inspected and
// changed through one open handle, so what is checked is what is changed: a
// name swapped for something else between the two steps cannot redirect the
// chmod. Only a regular file is touched.
func securePermissions(path string) error {
	// File modes do not protect a database in a directory every user can
	// write to: anyone could replace the files outright. That is the
	// operator's arrangement to fix, so it is reported rather than refused.
	// Only world-writable is flagged. Group-writable is the normal result of
	// a umask of 002 where each user has a private group, and warning about
	// it on every start would teach operators to ignore the line.
	if info, err := os.Stat(filepath.Dir(path)); err == nil && info.Mode().Perm()&0o002 != 0 {
		slog.Warn("store: the data directory is writable by other users, who could replace the database; restrict it to its owner",
			"dir", filepath.Dir(path), "mode", fmt.Sprintf("%04o", info.Mode().Perm()))
	}
	var tightened []string
	for i, p := range []string{path, path + "-wal", path + "-shm"} {
		flags := os.O_RDWR
		if i == 0 {
			flags |= os.O_CREATE // only the database itself is created here
		}
		was, changed, err := restrictToOwner(p, flags)
		if err != nil {
			return err
		}
		if changed {
			tightened = append(tightened, fmt.Sprintf("%s (was %04o)", p, was))
		}
	}
	if len(tightened) > 0 {
		slog.Warn("store: database files were readable by other users; restricted to owner only (0600)", "files", tightened)
	}
	return nil
}

// restrictToOwner opens p, and if it is a regular file with any group or
// other permission bit set, makes it 0600. It reports the previous mode and
// whether it changed anything. A missing file is not an error.
func restrictToOwner(p string, flags int) (was os.FileMode, changed bool, err error) {
	f, err := os.OpenFile(p, flags, 0o600) // #nosec G304 -- operator-configured data directory
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: open %s: %w", p, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("store: close %s: %w", p, cerr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return 0, false, fmt.Errorf("store: stat %s: %w", p, err)
	}
	if !info.Mode().IsRegular() {
		return 0, false, fmt.Errorf("store: %s is not a regular file", p)
	}
	was = info.Mode().Perm()
	if was&0o077 == 0 {
		return was, false, nil
	}
	if err := f.Chmod(0o600); err != nil {
		return was, false, fmt.Errorf("store: restrict permissions of %s: %w", p, err)
	}
	return was, true, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping verifies the database is reachable, acquiring a connection from the
// pool. It is used by the server's health endpoint.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
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
	return s.truncateWAL(ctx)
}

// truncateWAL folds the write-ahead log into the database and truncates it.
// It runs on every start, not only after a migration. Migration 10 replaced
// plaintext hook tokens with hashes, and their old page images can sit in WAL
// frames long after the table is gone: secure_delete zeroes pages in the
// database file, not frames already written to the log. Tying the checkpoint
// to "just migrated" would skip it for good if the process died between the
// migration's commit and the checkpoint, so it is unconditional and cheap.
//
// The pragma reports a blocked checkpoint in its result row, not as an error.
// Another process holding the database open is the only cause at startup; it
// is logged rather than fatal, since the next start tries again.
func (s *Store) truncateWAL(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("store: checkpoint at startup: %w", err)
	}
	if busy != 0 {
		slog.Warn("store: could not truncate the write-ahead log at startup because another process has the database open; it will be retried on the next start")
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
	if step, ok := migrationSteps[i+1]; ok {
		if err := step(ctx, tx); err != nil {
			return err
		}
	}
	// PRAGMA does not accept bound parameters; i+1 is a trusted integer.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
		return err
	}
	return tx.Commit()
}
