package store

// privateCallMigration is migration 55 (issue #2030, ADR-017), registered
// in store.go's migrations after the DM store's 53 and the contact policy's
// 54. It keeps participants, endpoint credentials and retry
// receipts out of channel voice, presence, and audit data. The pair an
// attempt belongs to is the DM store's own conversation (dm_conversations,
// migration 53), whose visibility is defined in dm_visibility.go.
var privateCallMigration = []string{
	`CREATE TABLE private_calls (
		id TEXT PRIMARY KEY,
		conversation_id INTEGER NOT NULL REFERENCES dm_conversations(id),
		caller INTEGER NOT NULL REFERENCES principals(id),
		callee INTEGER NOT NULL REFERENCES principals(id),
		caller_credential INTEGER NOT NULL REFERENCES credentials(id),
		callee_credential INTEGER REFERENCES credentials(id),
		state TEXT NOT NULL CHECK (state IN ('unavailable', 'ringing', 'accepted', 'active',
			'declined', 'cancelled', 'missed', 'ended', 'failed')),
		reason TEXT NOT NULL DEFAULT '',
		last_event TEXT NOT NULL,
		revision INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		deadline INTEGER,
		ended_at INTEGER,
		caller_absent_since INTEGER,
		callee_absent_since INTEGER,
		CHECK (caller <> callee)
	)`,
	`CREATE TRIGGER private_calls_pair BEFORE INSERT ON private_calls
		WHEN NOT EXISTS (SELECT 1 FROM dm_conversations WHERE id = NEW.conversation_id
			AND lo = MIN(NEW.caller, NEW.callee) AND hi = MAX(NEW.caller, NEW.callee))
		BEGIN SELECT RAISE(ABORT, 'a private call must be between the conversation participants'); END`,
	`CREATE TRIGGER private_calls_pair_immutable BEFORE UPDATE OF conversation_id, caller, callee ON private_calls
		WHEN NEW.conversation_id <> OLD.conversation_id OR NEW.caller <> OLD.caller OR NEW.callee <> OLD.callee
		BEGIN SELECT RAISE(ABORT, 'a private call cannot change its conversation or participants'); END`,
	`CREATE INDEX private_calls_history ON private_calls (conversation_id, created_at DESC, id DESC)`,
	`CREATE TABLE private_call_holders (
		principal_id INTEGER PRIMARY KEY REFERENCES principals(id),
		call_id TEXT NOT NULL REFERENCES private_calls(id)
	)`,
	`CREATE INDEX private_call_holders_attempt ON private_call_holders (call_id)`,
	`CREATE TABLE private_call_actions (
		credential_id INTEGER NOT NULL REFERENCES credentials(id),
		action_id TEXT NOT NULL,
		event TEXT NOT NULL,
		target TEXT NOT NULL,
		request_revision INTEGER NOT NULL,
		call_id TEXT NOT NULL REFERENCES private_calls(id),
		glare INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		PRIMARY KEY (credential_id, action_id)
	)`,
	`CREATE INDEX private_call_actions_expiry ON private_call_actions (expires_at)`,
}
