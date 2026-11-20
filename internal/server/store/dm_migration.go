package store

// dmMigration is migration 53: local direct messages (issue #2017, ADR-017 Decisions 1, 5 and 12,
// the design note that comes with it, §2.1). Purely additive: no existing table, row,
// trigger or index is touched, so channels, two-member private channels and
// whispers read as before and a pre-DM database gains only empty tables.
// (Another branch, #2047, also adds a migration here: whichever merges
// second takes the next number; the entries are independent.)
//
// dm_conversations is the ordered pair, stored once: lo < hi, UNIQUE, both
// human principals (the insert trigger, since a CHECK cannot read another
// table). Nothing in a row ever changes, and a row is never deleted, so a
// conversation can not gain a member or lose its id. Its id is explicitly
// assigned at random so it carries no information about who opened first.
// Conversations, direct messages and DM files each have their own id space,
// apart from channels, messages and files.
//
// dm_viewer_state is one participant's own view: the archive/hide state
// (absent = open) and, by being there, that the participant has opened the
// conversation, which is how an empty pair the other person opened stays
// invisible to this one (dm_visibility.go). direct_messages and the tables
// around it mirror messages, message_revisions and message_removals
// (migrations 1, 38 and 45): a message's text changes only by edit, with
// the old text kept; removal is a permanent tombstone by the author alone
// (no moderator column, no by_author flag). direct_messages_fts is an
// external-content index, as messages_fts, one conversation token per row.
// dm_files is DM file ownership: a file belongs to one conversation, is
// uploaded by one participant and is attached once, to a message of that
// conversation. dm_read_marks is a participant's read mark.
var dmMigration = []string{

	`CREATE TABLE dm_conversations (
	id         INTEGER PRIMARY KEY,
	lo         INTEGER NOT NULL REFERENCES principals (id),
	hi         INTEGER NOT NULL REFERENCES principals (id),
	created_at INTEGER NOT NULL,
	CHECK (lo < hi),
	CHECK (typeof(id) = 'integer' AND id BETWEEN 1 AND 9007199254740991),
	UNIQUE (lo, hi)
) WITHOUT ROWID`,
	`CREATE INDEX dm_conversations_by_hi ON dm_conversations (hi, lo)`,
	`CREATE TRIGGER dm_conversations_humans BEFORE INSERT ON dm_conversations
WHEN (SELECT COUNT(*) FROM principals WHERE id IN (NEW.lo, NEW.hi) AND kind = 'human') <> 2
BEGIN SELECT RAISE(ABORT, 'a conversation is between two human principals'); END`,
	`CREATE TRIGGER dm_conversations_no_update BEFORE UPDATE ON dm_conversations
BEGIN SELECT RAISE(ABORT, 'a conversation never changes'); END`,
	`CREATE TRIGGER dm_conversations_no_delete BEFORE DELETE ON dm_conversations
BEGIN SELECT RAISE(ABORT, 'a conversation is never deleted'); END`,
	`CREATE TABLE dm_viewer_state (
	principal_id    INTEGER NOT NULL REFERENCES principals (id),
	conversation_id INTEGER NOT NULL REFERENCES dm_conversations (id),
	state           TEXT    NOT NULL CHECK (state IN ('open', 'archived', 'hidden')),
	updated_at      INTEGER NOT NULL,
	PRIMARY KEY (principal_id, conversation_id)
) WITHOUT ROWID`,
	`CREATE TRIGGER dm_viewer_state_member BEFORE INSERT ON dm_viewer_state
WHEN NOT EXISTS (SELECT 1 FROM dm_conversations WHERE id = NEW.conversation_id AND NEW.principal_id IN (lo, hi))
BEGIN SELECT RAISE(ABORT, 'only a participant has a view of a conversation'); END`,
	`CREATE TABLE direct_messages (
	id               INTEGER PRIMARY KEY,
	conversation_id  INTEGER NOT NULL REFERENCES dm_conversations (id),
	author_id        INTEGER NOT NULL REFERENCES principals (id),
	body             TEXT    NOT NULL,
	created_at       INTEGER NOT NULL,
	edited_at        INTEGER,
	client_nonce     TEXT    NOT NULL,
	revision         INTEGER NOT NULL CHECK (revision >= 1),
	fts_conversation TEXT GENERATED ALWAYS AS ('d' || conversation_id) VIRTUAL
)`,
	`CREATE INDEX direct_messages_by_conversation ON direct_messages (conversation_id, id)`,
	// The conversation's revision (schema.DMConversationV1.Revision) is
	// stored with the changes themselves: each message, edit and removal
	// takes the next number, so a message's revision is that of its latest
	// change and the conversation's is the largest of its messages'. UNIQUE
	// makes a number go to one message only, so two changes never share one.
	`CREATE UNIQUE INDEX direct_messages_by_revision ON direct_messages (conversation_id, revision)`,
	`CREATE INDEX direct_messages_by_nonce ON direct_messages (conversation_id, author_id, client_nonce)`,
	`CREATE TRIGGER direct_messages_author BEFORE INSERT ON direct_messages
WHEN NOT EXISTS (SELECT 1 FROM dm_conversations WHERE id = NEW.conversation_id AND NEW.author_id IN (lo, hi))
BEGIN SELECT RAISE(ABORT, 'only a participant writes in a conversation'); END`,
	// A value comparison, as messages_only_body_changes: see migration 38.
	`CREATE TRIGGER direct_messages_only_body_changes BEFORE UPDATE ON direct_messages
WHEN NEW.id IS NOT OLD.id OR NEW.conversation_id IS NOT OLD.conversation_id
  OR NEW.author_id IS NOT OLD.author_id OR NEW.created_at IS NOT OLD.created_at
  OR NEW.client_nonce IS NOT OLD.client_nonce
  OR NEW.revision <= OLD.revision
  OR EXISTS (SELECT 1 FROM direct_message_removals WHERE message_id = OLD.id)
BEGIN SELECT RAISE(ABORT, 'only the body, edited_at and revision of a message that stands may change, the revision only forward'); END`,
	`CREATE TRIGGER direct_messages_edited_at_forward BEFORE UPDATE OF edited_at ON direct_messages
WHEN NEW.edited_at IS NULL OR NEW.edited_at <= COALESCE(OLD.edited_at, OLD.created_at)
BEGIN SELECT RAISE(ABORT, 'edited_at must move forward'); END`,
	`CREATE TRIGGER direct_messages_no_delete BEFORE DELETE ON direct_messages
BEGIN SELECT RAISE(ABORT, 'a message is removed by a tombstone, never deleted'); END`,
	`CREATE TABLE direct_message_revisions (
	message_id INTEGER NOT NULL REFERENCES direct_messages (id),
	revision   INTEGER NOT NULL CHECK (revision >= 1),
	body       TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (message_id, revision)
)`,
	`CREATE TRIGGER direct_message_revisions_no_update BEFORE UPDATE ON direct_message_revisions
BEGIN SELECT RAISE(ABORT, 'message revisions are append-only'); END`,
	`CREATE TRIGGER direct_message_revisions_no_delete BEFORE DELETE ON direct_message_revisions
BEGIN SELECT RAISE(ABORT, 'message revisions are append-only'); END`,
	`CREATE TABLE direct_message_removals (
	message_id      INTEGER PRIMARY KEY REFERENCES direct_messages (id),
	conversation_id INTEGER NOT NULL REFERENCES dm_conversations (id),
	removed_by      INTEGER NOT NULL REFERENCES principals (id),
	removed_at      INTEGER NOT NULL
)`,
	`CREATE TRIGGER direct_message_removals_author BEFORE INSERT ON direct_message_removals
WHEN NEW.removed_by IS NOT (SELECT author_id FROM direct_messages WHERE id = NEW.message_id)
  OR NEW.conversation_id IS NOT (SELECT conversation_id FROM direct_messages WHERE id = NEW.message_id)
BEGIN SELECT RAISE(ABORT, 'only the author removes a message of a conversation'); END`,
	`CREATE TRIGGER direct_message_removals_no_update BEFORE UPDATE ON direct_message_removals
BEGIN SELECT RAISE(ABORT, 'message removals are permanent'); END`,
	`CREATE TRIGGER direct_message_removals_no_delete BEFORE DELETE ON direct_message_removals
BEGIN SELECT RAISE(ABORT, 'message removals are permanent'); END`,
	`CREATE VIRTUAL TABLE direct_messages_fts USING fts5(body, fts_conversation, content='direct_messages', content_rowid='id')`,
	`CREATE TRIGGER direct_messages_fts_insert AFTER INSERT ON direct_messages BEGIN
	INSERT INTO direct_messages_fts (rowid, body, fts_conversation) VALUES (new.id, new.body, new.fts_conversation);
END`,
	`CREATE TRIGGER direct_messages_fts_update AFTER UPDATE OF body ON direct_messages BEGIN
	INSERT INTO direct_messages_fts (direct_messages_fts, rowid, body, fts_conversation)
	VALUES ('delete', OLD.id, OLD.body, OLD.fts_conversation);
	INSERT INTO direct_messages_fts (rowid, body, fts_conversation)
	VALUES (NEW.id, NEW.body, NEW.fts_conversation);
END`,
	// A removed message's words stop matching; its row stays, as a channel
	// message's does, and is projected as the tombstone on read.
	`CREATE TRIGGER direct_messages_fts_remove AFTER INSERT ON direct_message_removals BEGIN
	INSERT INTO direct_messages_fts (direct_messages_fts, rowid, body, fts_conversation)
	SELECT 'delete', id, body, fts_conversation FROM direct_messages WHERE id = NEW.message_id;
END`,
	`CREATE TABLE dm_files (
	id              INTEGER PRIMARY KEY,
	conversation_id INTEGER NOT NULL REFERENCES dm_conversations (id),
	uploader_id     INTEGER NOT NULL REFERENCES principals (id),
	filename        TEXT    NOT NULL,
	media_type      TEXT    NOT NULL,
	size            INTEGER NOT NULL CHECK (size >= 0),
	sha256          TEXT    NOT NULL CHECK (length(sha256) = 64),
	created_at      INTEGER NOT NULL,
	message_id      INTEGER REFERENCES direct_messages (id),
	position        INTEGER NOT NULL DEFAULT 0
)`,
	`CREATE INDEX dm_files_attached ON dm_files (message_id) WHERE message_id IS NOT NULL`,
	`CREATE TRIGGER dm_files_uploader BEFORE INSERT ON dm_files
WHEN NOT EXISTS (SELECT 1 FROM dm_conversations WHERE id = NEW.conversation_id AND NEW.uploader_id IN (lo, hi))
BEGIN SELECT RAISE(ABORT, 'only a participant uploads to a conversation'); END`,
	// Only message_id changes, once, from NULL, to a message of the same
	// conversation by the uploader.
	`CREATE TRIGGER dm_files_attach_once BEFORE UPDATE ON dm_files
WHEN OLD.message_id IS NOT NULL OR NEW.message_id IS NULL
  OR NEW.id IS NOT OLD.id OR NEW.conversation_id IS NOT OLD.conversation_id
  OR NEW.uploader_id IS NOT OLD.uploader_id OR NEW.filename IS NOT OLD.filename
  OR NEW.media_type IS NOT OLD.media_type OR NEW.size IS NOT OLD.size
  OR NEW.sha256 IS NOT OLD.sha256 OR NEW.created_at IS NOT OLD.created_at
  OR NOT EXISTS (SELECT 1 FROM direct_messages WHERE id = NEW.message_id
                 AND conversation_id = OLD.conversation_id AND author_id = OLD.uploader_id)
BEGIN SELECT RAISE(ABORT, 'a file attaches once, to its uploader''s message in its conversation'); END`,
	`CREATE TRIGGER dm_files_no_delete BEFORE DELETE ON dm_files
BEGIN SELECT RAISE(ABORT, 'a file of a conversation is never deleted'); END`,
	`CREATE TABLE dm_read_marks (
	principal_id    INTEGER NOT NULL REFERENCES principals (id),
	conversation_id INTEGER NOT NULL REFERENCES dm_conversations (id),
	last_read_id    INTEGER NOT NULL CHECK (last_read_id >= 0),
	updated_at      INTEGER NOT NULL,
	PRIMARY KEY (principal_id, conversation_id)
) WITHOUT ROWID`,
}
