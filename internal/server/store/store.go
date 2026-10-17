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
	// 11: Nets (issue #115, ADR-005). A net is a named subset of one channel's
	// members. Nets are archived, never deleted, so messages that reference a
	// net keep a valid id; the partial unique index frees a name for reuse once
	// its net is archived. net_members rows are kept when a net is archived
	// (they record who was on it) and removed when the principal leaves the
	// channel. Timestamps are unix milliseconds UTC. No data is backfilled: a
	// database from before this migration has no nets.
	{
		`CREATE TABLE nets (
	id          INTEGER PRIMARY KEY,
	channel_id  INTEGER NOT NULL REFERENCES channels (id),
	name        TEXT    NOT NULL,
	created_by  INTEGER REFERENCES principals (id),
	created_at  INTEGER NOT NULL,
	archived_at INTEGER
)`,
		`CREATE UNIQUE INDEX nets_live_name ON nets (channel_id, name) WHERE archived_at IS NULL`,
		`CREATE TABLE net_members (
	net_id       INTEGER NOT NULL REFERENCES nets (id),
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	role         TEXT    NOT NULL CHECK (role IN ('member','monitor')),
	added_by     INTEGER REFERENCES principals (id),
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (net_id, principal_id)
)`,
		`CREATE INDEX net_members_by_principal ON net_members (principal_id)`,
	},
	// 12: Scoped messages (issue #116, ADR-005). audience_kind is NULL for a
	// channel-wide message, which is what every existing row is and stays, and
	// 'net' or 'principals' for a scoped one; net_id is set only for a net
	// message. The people a scoped message reached are resolved once, at post
	// time, and stored in message_recipients: joining a net later reveals no
	// history and leaving it hides nothing already received. Recipient rows
	// are written in the same transaction as the message and are never
	// updated or deleted. A channel-wide message has none. The unique pair is
	// the primary key; the principal index serves "what was I sent".
	{
		`ALTER TABLE messages ADD COLUMN audience_kind TEXT CHECK (audience_kind IN ('net','principals'))`,
		`ALTER TABLE messages ADD COLUMN net_id INTEGER REFERENCES nets (id)`,
		`CREATE TABLE message_recipients (
	message_id   INTEGER NOT NULL REFERENCES messages (id),
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	PRIMARY KEY (message_id, principal_id)
)`,
		`CREATE INDEX message_recipients_by_principal ON message_recipients (principal_id)`,
	},
	// 13: Voice rooms (issue #126, docs/design/voice-control-plane.md §3).
	// conchd names the LiveKit room for an audience and keeps the name; LiveKit
	// itself forgets rooms, so this row is the durable thing. net_id is NULL
	// for the channel-wide room and unused until nets get rooms (V5). SQLite
	// treats NULLs as distinct in a unique index, so "one room per audience"
	// is two partial unique indexes: one for the channel-wide room, one for
	// each net. room_name is unique across all rows. A net room must belong
	// to the net's own channel: the composite foreign key says so (it needs
	// the unique index on nets it references, and is not checked while net_id
	// is NULL), so a row can never pair one channel with another's net.
	{
		`CREATE UNIQUE INDEX nets_id_channel ON nets (id, channel_id)`,
		`CREATE TABLE voice_rooms (
	id         INTEGER PRIMARY KEY,
	channel_id INTEGER NOT NULL REFERENCES channels (id),
	net_id     INTEGER,
	room_name  TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	FOREIGN KEY (net_id, channel_id) REFERENCES nets (id, channel_id)
)`,
		`CREATE UNIQUE INDEX voice_rooms_channel_wide ON voice_rooms (channel_id) WHERE net_id IS NULL`,
		`CREATE UNIQUE INDEX voice_rooms_by_net ON voice_rooms (net_id) WHERE net_id IS NOT NULL`,
	},
	// 14: Rotating voice rooms (issue #161, docs/design/voice-control-plane.md
	// §5). LiveKit gives every connected participant a renewable token of its
	// own, so removing someone from a room never stops them rejoining; the room
	// has to change name instead. A rotated room is retired, not deleted: its
	// row stays so the name is never handed out again and the sweep can find
	// rooms LiveKit still has to delete. "One room per audience" becomes "one
	// live room per audience", so the two partial unique indexes are dropped
	// (SQLite cannot alter a partial index) and recreated over live rows.
	// room_name stays unique across live and retired rows. voice_room_holders
	// records, durably, every credential a session was issued under for a room:
	// the set of people who may hold a token LiveKit will renew. Its rows go
	// when the room is retired.
	//
	// Rooms that exist before this migration have no holder rows, although
	// tokens for them may be out and LiveKit may be renewing them, so nobody
	// losing their place could ever rotate them. They are all retired here:
	// the first sweep deletes any that LiveKit still has, and the next session
	// for a channel creates its room afresh, with holders recorded.
	{
		`ALTER TABLE voice_rooms ADD COLUMN retired_at INTEGER`,
		`UPDATE voice_rooms SET retired_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000`,
		`DROP INDEX voice_rooms_channel_wide`,
		`DROP INDEX voice_rooms_by_net`,
		`CREATE UNIQUE INDEX voice_rooms_channel_wide ON voice_rooms (channel_id) WHERE net_id IS NULL AND retired_at IS NULL`,
		`CREATE UNIQUE INDEX voice_rooms_by_net ON voice_rooms (net_id) WHERE net_id IS NOT NULL AND retired_at IS NULL`,
		`CREATE TABLE voice_room_holders (
	room_id       INTEGER NOT NULL REFERENCES voice_rooms (id),
	principal_id  INTEGER NOT NULL REFERENCES principals (id),
	credential_id INTEGER NOT NULL REFERENCES credentials (id),
	created_at    INTEGER NOT NULL,
	PRIMARY KEY (room_id, principal_id, credential_id)
)`,
		`CREATE INDEX voice_room_holders_by_credential ON voice_room_holders (credential_id)`,
	},
	// 15: Message search (issue #22). messages_fts is an external-content FTS5
	// index over messages.body (default unicode61 tokenizer: case-insensitive
	// whole words), keyed by the message id as its rowid. It stores no text of
	// its own. Messages are never updated or deleted, so an insert trigger is
	// all that keeps it in step; if that ever changes, update and delete
	// triggers are needed with it. The rebuild indexes every message that
	// exists before this migration, scoped ones included: the index knows
	// nothing of audiences, and visibility is applied by the one search query
	// in audience.go, in the same statement as the match.
	{
		`CREATE VIRTUAL TABLE messages_fts USING fts5(body, content='messages', content_rowid='id')`,
		`CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts (rowid, body) VALUES (new.id, new.body);
END`,
		`INSERT INTO messages_fts (messages_fts) VALUES ('rebuild')`,
	},
	// 16: File uploads (issue #23). One row per uploaded file; the bytes live
	// on disk under a path derived from sha256 alone (internal/server/files.go),
	// never from filename. message_id is NULL until a message attaches the
	// file and is set by that message's own insert transaction, so "attached
	// at most once" is decided by the database: the attaching UPDATE matches
	// only rows whose message_id is still NULL. It is deliberately not
	// unique, because one message may attach several files (up to 10). Who may
	// fetch a file is asked in audience.go, with the message visibility rule,
	// since it joins messages. Files are never deleted. Timestamps are unix
	// milliseconds UTC. No data is backfilled.
	{
		`CREATE TABLE files (
	id          INTEGER PRIMARY KEY,
	channel_id  INTEGER NOT NULL REFERENCES channels (id),
	uploader_id INTEGER NOT NULL REFERENCES principals (id),
	filename    TEXT    NOT NULL,
	media_type  TEXT    NOT NULL,
	size        INTEGER NOT NULL CHECK (size >= 0),
	sha256      TEXT    NOT NULL CHECK (length(sha256) = 64),
	created_at  INTEGER NOT NULL,
	message_id  INTEGER REFERENCES messages (id)
)`,
		`CREATE INDEX files_by_message ON files (message_id) WHERE message_id IS NOT NULL`,
	},
	// 17: Threads (issue #25). thread_root_id is the message a reply answers,
	// NULL for every message that is not a reply, which includes all that exist
	// before this migration (nothing is backfilled). Threads are one level deep:
	// the server only ever stores a root that is itself not a reply, and the
	// reply's audience is its root's (decided in audience.go, in the insert
	// transaction), so the column carries no access rule of its own. The index
	// serves the thread listing.
	{
		`ALTER TABLE messages ADD COLUMN thread_root_id INTEGER REFERENCES messages (id)`,
		`CREATE INDEX messages_by_thread ON messages (thread_root_id, id)`,
	},
	// 18: Sessions (issue #265, P3 epic #263, decision D1). One row per live
	// client or runtime of a principal, human or agent. A session belongs to
	// the credential that created it (credential_id), which is how ownership
	// is decided; revoking or disabling ends it in the same transaction.
	// Rows are never deleted: ended_at is set when a session ends and state
	// then stays 'offline' (the CHECK says so, as SessionV1.Validate does).
	// The server flips state to 'stale' and 'offline' on heartbeat
	// thresholds. interventions is a JSON list, '[]' when none. Timestamps
	// are unix milliseconds UTC. No data is backfilled. The partial index is
	// for the sweeper, which looks only at sessions that can still change.
	{
		`CREATE TABLE sessions (
	id            INTEGER PRIMARY KEY,
	principal_id  INTEGER NOT NULL REFERENCES principals (id),
	credential_id INTEGER NOT NULL REFERENCES credentials (id),
	client        TEXT    NOT NULL,
	runtime       TEXT    NOT NULL DEFAULT '',
	model         TEXT    NOT NULL DEFAULT '',
	host          TEXT    NOT NULL DEFAULT '',
	interventions TEXT    NOT NULL DEFAULT '[]',
	started_at    INTEGER NOT NULL,
	last_seen_at  INTEGER NOT NULL,
	ended_at      INTEGER,
	state         TEXT    NOT NULL CHECK (state IN ('online','stale','offline')),
	CHECK (ended_at IS NULL OR state = 'offline')
)`,
		`CREATE INDEX sessions_by_principal ON sessions (principal_id, ended_at)`,
		`CREATE INDEX sessions_by_credential ON sessions (credential_id)`,
		`CREATE INDEX sessions_live_by_last_seen ON sessions (last_seen_at) WHERE ended_at IS NULL AND state <> 'offline'`,
	},
	// 19: Runs (issue #266, P3 epic #263, decision D8). One row per piece of
	// work an agent session reports. A run belongs to one channel and may
	// answer one message in it (message_id, NULL for none); that pair decides
	// who may see it, and the rule lives in audience.go beside the message
	// rule, which it is built on. principal_id is the session's principal,
	// copied so a run names its agent without a join. ended_at is set exactly
	// when the state is terminal (the CHECK says so, as RunV1.Validate does).
	// link and outcome are '' when absent. Timestamps are unix milliseconds
	// UTC. Rows are never deleted. No data is backfilled. The indexes serve
	// the channel listing and the roster's look-up of a session's runs.
	{
		`CREATE TABLE runs (
	id           INTEGER PRIMARY KEY,
	session_id   INTEGER NOT NULL REFERENCES sessions (id),
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	channel_id   INTEGER NOT NULL REFERENCES channels (id),
	message_id   INTEGER REFERENCES messages (id),
	state        TEXT    NOT NULL CHECK (state IN ('queued','starting','running','blocked','waiting','completed','failed','cancelled')),
	task         TEXT    NOT NULL,
	link         TEXT    NOT NULL DEFAULT '',
	started_at   INTEGER NOT NULL,
	ended_at     INTEGER,
	outcome      TEXT    NOT NULL DEFAULT '',
	CHECK ((ended_at IS NULL) = (state NOT IN ('completed','failed','cancelled')))
)`,
		`CREATE INDEX runs_by_channel ON runs (channel_id, id)`,
		`CREATE INDEX runs_by_session ON runs (session_id, state)`,
	},
	// 20: Run activity (issue #266). One row per entry an agent appends to
	// one of its runs. An entry has no visibility of its own: it is read only
	// through its run, by the run's rule. data is the entry's JSON detail as
	// the server encoded it, NULL when there is none. at is the server's
	// clock, unix milliseconds UTC. Rows are never deleted.
	{
		`CREATE TABLE run_activity (
	id      INTEGER PRIMARY KEY,
	run_id  INTEGER NOT NULL REFERENCES runs (id),
	kind    TEXT    NOT NULL,
	summary TEXT    NOT NULL,
	outcome TEXT    NOT NULL DEFAULT '',
	data    BLOB,
	at      INTEGER NOT NULL
)`,
		`CREATE INDEX run_activity_by_run ON run_activity (run_id, id)`,
	},
	// 21: Interventions (issue #287, P3 epic #263, decision D10). One row per
	// request a person makes of a run: take a message, cancel, pause or
	// resume. A row has no visibility of its own: it is read only through its
	// run, by the run's rule (audience.go). Only the server's side of the
	// story is kept: requested_by and requested_at are the person's request;
	// state, answered_at and reason are what the run's adapter reported, and
	// answered_at is set exactly when the state is no longer 'requested'. A
	// message has a body and no other kind has one (the CHECKs say so, as
	// InterventionV1.Validate does). Timestamps are unix milliseconds UTC.
	// Rows are never deleted, and nothing in this table changes a run. No data
	// is backfilled. The index serves the per-run list and the 16-open bound.
	{
		`CREATE TABLE interventions (
	id           INTEGER PRIMARY KEY,
	run_id       INTEGER NOT NULL REFERENCES runs (id),
	kind         TEXT    NOT NULL CHECK (kind IN ('message','cancel','pause','resume')),
	body         TEXT    NOT NULL DEFAULT '',
	state        TEXT    NOT NULL CHECK (state IN ('requested','acknowledged','applied','failed')),
	requested_by INTEGER NOT NULL REFERENCES principals (id),
	requested_at INTEGER NOT NULL,
	answered_at  INTEGER,
	reason       TEXT    NOT NULL DEFAULT '',
	CHECK ((answered_at IS NULL) = (state = 'requested')),
	CHECK ((kind = 'message') = (body <> '')),
	CHECK (state <> 'requested' OR reason = ''),
	CHECK (state <> 'failed' OR reason <> '')
)`,
		`CREATE INDEX interventions_by_run ON interventions (run_id, id)`,
	},
	// 22: Search knows the channel (issue #235). The index of migration 15 had
	// no notion of a channel, so a search first found every message anywhere
	// with the words and only then kept the caller's channel: its cost grew
	// with the whole database. The index gets a second column, fts_channel,
	// holding the token "c<channel id>" (one whole token to the unicode61
	// tokenizer), and the search requires it, so FTS5 intersects the word's
	// postings with the channel's. An external-content table reads its columns
	// from the content table by name, so messages gets a virtual generated
	// column of that name: nothing is stored twice and the two cannot differ.
	// The old table and trigger are dropped and the index is rebuilt, so every
	// message that exists is indexed again, scoped ones included, as before.
	// Messages are never updated or deleted, as in migration 15. The generated
	// column appears in SELECT * on messages: a later migration that rebuilds
	// the table must name its columns, never copy it with a bare SELECT *.
	{
		`ALTER TABLE messages ADD COLUMN fts_channel TEXT GENERATED ALWAYS AS ('c' || channel_id) VIRTUAL`,
		`DROP TRIGGER messages_fts_insert`,
		`DROP TABLE messages_fts`,
		`CREATE VIRTUAL TABLE messages_fts USING fts5(body, fts_channel, content='messages', content_rowid='id')`,
		`CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts (rowid, body, fts_channel) VALUES (new.id, new.body, new.fts_channel);
END`,
		`INSERT INTO messages_fts (messages_fts) VALUES ('rebuild')`,
	},
	// 23: Read marks (issue #351). How far one person has read one channel:
	// the id of the newest message they marked, per principal and not per
	// credential, so a person's two clients share it. The mark is a plain
	// number and holds no message content; what is unread is worked out when
	// asked, by the visibility function. The mark only moves forward (the
	// upsert in MarkRead keeps the larger id). A row outlives the person's
	// membership of the channel, so rejoining picks the mark up again.
	{
		`CREATE TABLE read_marks (
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	channel_id   INTEGER NOT NULL REFERENCES channels (id),
	last_read_id INTEGER NOT NULL CHECK (last_read_id >= 0),
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (principal_id, channel_id)
)`,
	},
	// 24: Invites (issue #436, ADR-008 §4). An invite is a second way to get
	// a credential: whoever holds its code joins as a new human principal
	// with the channels and grants the invite names. Only the SHA-256 of the
	// code is stored, as for credentials. max_uses NULL means uncapped (an
	// open link's default); a personal invite always has max_uses 1. The
	// invite row stays after it is used up, closed or expired: the account
	// records which invite and inviter it came through.
	//
	// principals gains the invite that made it and its maker, the right to
	// invite (nobody holds it until granted; operators can always invite) and
	// voice_allowed (every existing principal keeps voice, as today). The new
	// columns reference rows that may not exist yet, so they are nullable and
	// ADD COLUMN-safe. server_settings holds switches an operator flips at
	// runtime; invites_paused is the first.
	{
		`CREATE TABLE invites (
	id                     INTEGER PRIMARY KEY,
	kind                   TEXT    NOT NULL CHECK (kind IN ('personal', 'open')),
	code_hash              TEXT    NOT NULL UNIQUE,
	created_by             INTEGER NOT NULL REFERENCES principals (id),
	created_at             INTEGER NOT NULL,
	expires_at             INTEGER NOT NULL,
	max_uses               INTEGER CHECK (max_uses IS NULL OR max_uses >= 1),
	uses                   INTEGER NOT NULL DEFAULT 0 CHECK (uses >= 0),
	closed_at              INTEGER,
	closed_by              INTEGER REFERENCES principals (id),
	voice                  INTEGER NOT NULL DEFAULT 0 CHECK (voice IN (0, 1)),
	may_invite             INTEGER NOT NULL DEFAULT 0 CHECK (may_invite IN (0, 1)),
	credential_lifetime_ms INTEGER NOT NULL CHECK (credential_lifetime_ms > 0),
	CHECK (kind = 'open' OR COALESCE(max_uses, 0) = 1)
)`,
		`CREATE INDEX invites_by_creator ON invites (created_by, kind)`,
		`CREATE TABLE invite_channels (
	invite_id  INTEGER NOT NULL REFERENCES invites (id),
	channel_id INTEGER NOT NULL REFERENCES channels (id),
	PRIMARY KEY (invite_id, channel_id)
)`,
		`ALTER TABLE principals ADD COLUMN invited_by INTEGER REFERENCES principals (id)`,
		`ALTER TABLE principals ADD COLUMN invite_id INTEGER REFERENCES invites (id)`,
		`ALTER TABLE principals ADD COLUMN may_invite INTEGER NOT NULL DEFAULT 0 CHECK (may_invite IN (0, 1))`,
		`ALTER TABLE principals ADD COLUMN voice_allowed INTEGER NOT NULL DEFAULT 1 CHECK (voice_allowed IN (0, 1))`,
		`CREATE INDEX principals_by_invite ON principals (invite_id) WHERE invite_id IS NOT NULL`,
		`CREATE TABLE server_settings (
	key        TEXT    PRIMARY KEY,
	value      TEXT    NOT NULL,
	updated_at INTEGER NOT NULL
)`,
	},
	// 25: Who may decide a channel's approvals (issue #450). Until now any
	// human member could, which stops being safe once people join through
	// invites. can_decide is per membership; every existing membership keeps
	// the right (the column default), as does a member an operator adds
	// without saying otherwise. A member who joins through an invite is
	// inserted with 0.
	{
		`ALTER TABLE channel_members ADD COLUMN can_decide INTEGER NOT NULL DEFAULT 1 CHECK (can_decide IN (0, 1))`,
	},
	// 26: Renewable credentials (issue #439). renew_ms, when set, is how far
	// past a use the credential's expiry moves: a request that authenticates
	// with it sets expires_at to now + renew_ms, at most once a day
	// (ResolveCredentialDetail). NULL is a fixed expiry, or none, which every
	// existing credential keeps. A renewable credential always has an expiry,
	// so one left unused runs out.
	{
		`ALTER TABLE credentials ADD COLUMN renew_ms INTEGER CHECK (renew_ms IS NULL OR (renew_ms > 0 AND expires_at IS NOT NULL))`,
	},
	// 27: Name keys (issue #441). name_key and name_skel are a principal
	// name's two keys (names.KeysOf): case-folded and reduced to its UTS #39
	// confusable skeleton, folding first and mapping first. Two names that
	// share either key are one name, and every write that sets a name
	// refuses a key someone else holds (claimNameTx). The check is in the
	// writing transaction, not a UNIQUE index, because a database may
	// already hold two names with one key ("Alice" and "alice"); those are
	// kept, and the Go step logs them. The keys are computed in Go, so the
	// step fills them for every existing row, and the triggers refuse a row
	// written without them: no path that creates or renames a principal can
	// forget them.
	{
		`ALTER TABLE principals ADD COLUMN name_key TEXT`,
		`ALTER TABLE principals ADD COLUMN name_skel TEXT`,
		`CREATE INDEX principals_by_name_key ON principals (name_key)`,
		`CREATE INDEX principals_by_name_skel ON principals (name_skel)`,
		`CREATE TRIGGER principals_name_key_insert BEFORE INSERT ON principals
WHEN NEW.name_key IS NULL OR NEW.name_skel IS NULL
BEGIN
	SELECT RAISE(ABORT, 'principals.name_key and name_skel are required');
END`,
		`CREATE TRIGGER principals_name_key_update BEFORE UPDATE OF name, name_key, name_skel ON principals
WHEN NEW.name_key IS NULL OR NEW.name_skel IS NULL
BEGIN
	SELECT RAISE(ABORT, 'principals.name_key and name_skel are required');
END`,
	},
	// 28: Hidden principals (issue #445). Deleting an account means disabling
	// and hiding it: hidden_at is when it was hidden, NULL for every existing
	// principal. A hidden principal is always disabled, which the CHECK holds
	// to; enabling it clears both. Names starting with "hidden-" are reserved
	// for hidden principals from now on, and the Go step logs any principal
	// that already has one: it keeps its name.
	{
		`ALTER TABLE principals ADD COLUMN hidden_at INTEGER CHECK (hidden_at IS NULL OR disabled_at IS NOT NULL)`,
	},
	// 29 (problemReportsMigration): Problem reports (issue #446). One row per
	// report a person sent. document is the report as the operator reads it
	// (schema.ProblemReportV1, JSON); the columns beside it are what a list
	// shows and filters on, and what changes when an operator files it.
	// Nothing existing changes, so nothing is backfilled.
	{
		`CREATE TABLE problem_reports (
	id             INTEGER PRIMARY KEY,
	principal_id   INTEGER NOT NULL REFERENCES principals (id),
	created_at     INTEGER NOT NULL,
	state          TEXT    NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'filed')),
	client_name    TEXT    NOT NULL,
	client_version TEXT    NOT NULL,
	server_version TEXT    NOT NULL,
	often          TEXT    NOT NULL,
	document       TEXT    NOT NULL,
	issue          TEXT,
	filed_at       INTEGER,
	filed_by       INTEGER REFERENCES principals (id),
	CHECK ((state = 'filed') = (issue IS NOT NULL AND filed_at IS NOT NULL))
)`,
		`CREATE INDEX problem_reports_by_principal ON problem_reports (principal_id, created_at)`,
	},
	// 30: Sign-in links (issue #493). Number 30 on main, after problem
	// reports (29, issue #446). An operator makes a one-time code that
	// signs an existing person back in: whoever uses it gets a new credential
	// for principal_id. Only the SHA-256 of the code is stored, as for
	// invites. used_at and credential_id are set together by the one use;
	// closed_at is set when the person, or the operator who made it, is
	// disabled. The row stays afterwards, for the record.
	{
		`CREATE TABLE sign_in_links (
	id            INTEGER PRIMARY KEY,
	code_hash     TEXT    NOT NULL UNIQUE,
	principal_id  INTEGER NOT NULL REFERENCES principals (id),
	created_by    INTEGER NOT NULL REFERENCES principals (id),
	created_at    INTEGER NOT NULL,
	expires_at    INTEGER NOT NULL,
	used_at       INTEGER,
	credential_id INTEGER REFERENCES credentials (id),
	closed_at     INTEGER,
	CHECK ((used_at IS NULL) = (credential_id IS NULL))
)`,
		`CREATE INDEX sign_in_links_by_principal ON sign_in_links (principal_id)`,
		`CREATE INDEX sign_in_links_by_creator ON sign_in_links (created_by)`,
	},
}

// migrationSteps holds Go code that runs inside a migration's transaction
// after its SQL statements, for the rare change SQL cannot express. It is
// keyed by the migration's 1-based number (the schema version it produces).
var migrationSteps = map[int]func(ctx context.Context, tx *sql.Tx) error{
	10:                hashPlaintextHooks,
	nameKeyMigration:  fillNameKeys,
	hiddenAtMigration: reportReservedNames,
}

// Store is the embedded SQLite database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
	// rollbackSQL replaces "ROLLBACK" in withImmediateTx when set. Tests set
	// it to a statement that fails, to exercise the path where a transaction
	// cannot be ended and the connection must be discarded.
	rollbackSQL string
	// runsEnded, when set, is told which runs a committed transaction failed
	// because their session ended (issue #405).
	runsEnded func([]EndedRun)
	// voiceRoomPrefix starts the name of each new voice room; empty means
	// the default. See SetVoiceRoomPrefix.
	voiceRoomPrefix string
}

// EndedRun names a run that ended with its session: the store changed it
// without SetRunState, so the server must be told to push it.
type EndedRun struct {
	ID, ChannelID int64
}

// OnRunsEnded sets the function the store calls, after the commit and never
// inside a transaction, with the runs a session-ending call failed. Set it
// before the store is used from more than one goroutine. There is one such
// function: a later call replaces the earlier one, so one store serves one
// Server.
func (s *Store) OnRunsEnded(fn func([]EndedRun)) { s.runsEnded = fn }

// announceRunsEnded tells the OnRunsEnded function about runs, if there are
// any. Callers reach it only after their transaction has committed.
func (s *Store) announceRunsEnded(runs []EndedRun) {
	if len(runs) > 0 && s.runsEnded != nil {
		s.runsEnded(runs)
	}
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
