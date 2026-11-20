package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// privateCallMigration keeps participants, endpoint credentials and retry
// receipts out of channel voice, presence, and audit data. The DDL is
// idempotent so migration fixtures can replay older schema changes without
// dropping the private call tables.
var privateCallMigration = []string{
	`CREATE TABLE IF NOT EXISTS private_call_pairs (
		conversation_id INTEGER PRIMARY KEY CHECK (conversation_id > 0),
		lo INTEGER NOT NULL REFERENCES principals(id),
		hi INTEGER NOT NULL REFERENCES principals(id),
		CHECK (lo < hi), UNIQUE (lo, hi)
	)`,
	`CREATE TABLE IF NOT EXISTS private_calls (
		id TEXT PRIMARY KEY,
		conversation_id INTEGER NOT NULL REFERENCES private_call_pairs(conversation_id),
		caller INTEGER NOT NULL REFERENCES principals(id),
		callee INTEGER NOT NULL REFERENCES principals(id),
		caller_credential INTEGER NOT NULL REFERENCES credentials(id),
		callee_credential INTEGER REFERENCES credentials(id),
		state TEXT NOT NULL,
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
	`CREATE INDEX IF NOT EXISTS private_calls_history ON private_calls (conversation_id, created_at DESC, id DESC)`,
	`CREATE TABLE IF NOT EXISTS private_call_holders (
		principal_id INTEGER PRIMARY KEY REFERENCES principals(id),
		call_id TEXT NOT NULL REFERENCES private_calls(id)
	)`,
	`CREATE INDEX IF NOT EXISTS private_call_holders_attempt ON private_call_holders (call_id)`,
	`CREATE TABLE IF NOT EXISTS private_call_actions (
		principal_id INTEGER NOT NULL REFERENCES principals(id),
		action_id TEXT NOT NULL,
		event TEXT NOT NULL,
		target TEXT NOT NULL,
		call_id TEXT REFERENCES private_calls(id),
		outcome TEXT NOT NULL,
		glare INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		PRIMARY KEY (principal_id, action_id)
	)`,
	`CREATE INDEX IF NOT EXISTS private_call_actions_expiry ON private_call_actions (expires_at)`,
}

// Register migration 51 after the ordered migrations declared by the store.
func init() { migrations = append(migrations, privateCallMigration) }

var (
	ErrPrivateCallNotFound = errors.New("store: private call not found")
	ErrPrivateCallPair     = errors.New("store: private call conversation not found")
	ErrPrivateCallActor    = errors.New("store: private call action not allowed")
	ErrPrivateCallCursor   = errors.New("store: invalid private call history cursor")
)

// PrivateCallActor is a principal and the credential authenticated for this request.
type PrivateCallActor struct {
	PrincipalID, CredentialID int64
}

// PrivateCallResult carries a participant's projection and the command's
// outcome. Code is empty for a successful command.
type PrivateCallResult struct {
	schema.PrivateCallResponseV1
	Created bool
	Code    string
}

type privateCall struct {
	schema.PrivateCallV1
	callerCredential, calleeCredential int64
	lastEvent                          string
	deadline, endedAt                  sql.NullInt64
	callerAbsent, calleeAbsent         sql.NullInt64
}

// The store records restore separately. The wire schema uses server_restart
// for both restart and restore.
const privateCallReasonRestore schema.PrivateCallReason = "restore"

const privateCallColumns = `c.id, c.conversation_id, c.caller, c.callee, c.caller_credential,
	COALESCE(c.callee_credential, 0), c.state, c.reason, c.last_event, c.revision,
	c.created_at, c.deadline, c.ended_at, c.caller_absent_since, c.callee_absent_since`

func scanPrivateCall(row interface{ Scan(...any) error }) (privateCall, error) {
	var c privateCall
	var created int64
	err := row.Scan(&c.ID, &c.ConversationID, &c.CallerID, &c.CalleeID, &c.callerCredential,
		&c.calleeCredential, &c.State, &c.Reason, &c.lastEvent, &c.Revision, &created,
		&c.deadline, &c.endedAt, &c.callerAbsent, &c.calleeAbsent)
	if errors.Is(err, sql.ErrNoRows) {
		return privateCall{}, ErrPrivateCallNotFound
	}
	c.Schema = schema.PrivateCallSchemaV1
	c.CreatedAt = schema.NewTimestamp(time.UnixMilli(created))
	return c, err
}

func privateCallByID(ctx context.Context, tx execer, id string) (privateCall, error) {
	return scanPrivateCall(tx.QueryRowContext(ctx, `SELECT `+privateCallColumns+` FROM private_calls c WHERE c.id = ?`, id))
}

func privateCallHeldBy(ctx context.Context, tx execer, principalID int64) (privateCall, error) {
	return scanPrivateCall(tx.QueryRowContext(ctx, `SELECT `+privateCallColumns+`
		FROM private_call_holders h JOIN private_calls c ON c.id = h.call_id WHERE h.principal_id = ?`, principalID))
}

func (c privateCall) role(principalID int64) schema.PrivateCallRole {
	switch principalID {
	case c.CallerID:
		return schema.PrivateCallRoleCaller
	case c.CalleeID:
		if c.State != schema.PrivateCallStateUnavailable {
			return schema.PrivateCallRoleCallee
		}
	}
	return ""
}

func (c privateCall) view(actor PrivateCallActor) schema.PrivateCallV1 {
	doc := c.PrivateCallV1
	doc.Role = c.role(actor.PrincipalID)
	if doc.State.Terminal() {
		if doc.Reason == privateCallReasonRestore {
			doc.Reason = schema.PrivateCallReasonServerRestart
		}
		doc.EndedAt = privateCallTimestamp(c.endedAt.Int64)
		return doc
	}
	credential := c.callerCredential
	if doc.Role == schema.PrivateCallRoleCallee {
		credential = c.calleeCredential
	}
	doc.SelectedHere = credential != 0 && actor.CredentialID == credential
	switch doc.State {
	case schema.PrivateCallStateRinging:
		doc.RingDeadline = privateCallTimestamp(c.deadline.Int64)
	case schema.PrivateCallStateAccepted:
		doc.JoinDeadline = privateCallTimestamp(c.deadline.Int64)
	}
	return doc
}

func privateCallTimestamp(ms int64) *schema.Timestamp {
	t := schema.NewTimestamp(time.UnixMilli(ms))
	return &t
}

// BindPrivateCallConversation supplies the immutable pair from the DM store.
// #2018 should replace this bridge and privateCallPeer with its conversation
// lookup in the same transaction. No call route accepts a pair from a client.
func (s *Store) BindPrivateCallConversation(ctx context.Context, conversationID, a, b int64) error {
	err := s.db.QueryRowContext(ctx, `INSERT INTO private_call_pairs (conversation_id, lo, hi)
		VALUES (?, ?, ?) ON CONFLICT (conversation_id) DO UPDATE SET lo = excluded.lo
		WHERE lo = excluded.lo AND hi = excluded.hi RETURNING conversation_id`,
		conversationID, min(a, b), max(a, b)).Scan(&conversationID)
	return err
}

func privateCallPeer(ctx context.Context, tx execer, conversationID, participant int64) (int64, error) {
	var peer int64
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN lo = ? THEN hi ELSE lo END
		FROM private_call_pairs WHERE conversation_id = ? AND ? IN (lo, hi)`,
		participant, conversationID, participant).Scan(&peer)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrPrivateCallPair
	}
	return peer, err
}

// privateCallContactAllowed is the #2018 eligibility seam. Its policy must
// cover both directions and run in this transaction for invites and live calls.
func privateCallContactAllowed(_ context.Context, _ execer, _, _ int64) (bool, error) {
	return true, nil
}

func privateCallVoiceAllowed(ctx context.Context, tx execer, principalID int64) (bool, error) {
	p, found, err := queryPrincipal(ctx, tx, principalID)
	if err != nil || !found || p.Kind != PrincipalHuman || p.DisabledAt != nil ||
		(p.GuestState != "" && p.GuestState != schema.GuestStateConverted) {
		return false, err
	}
	return principalPermittedTx(ctx, tx, p, schema.PermUseVoice)
}

// A zero credential asks whether the callee has any signed-in device.
// #2031 can additionally require a recipient reachable by the ring stream.
func privateCallCredentialLive(ctx context.Context, tx execer, actor PrivateCallActor, now time.Time) (bool, error) {
	var live bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM credentials c JOIN principals p ON p.id = c.principal_id
		WHERE p.id = ? AND p.disabled_at IS NULL AND p.kind = 'human' AND (? = 0 OR c.id = ?)
		AND NOT EXISTS (SELECT 1 FROM guest_visits g WHERE g.principal_id = p.id AND g.state <> 'converted')
		AND `+
		liveCredentialWhere(now)+`)`, actor.PrincipalID, actor.CredentialID, actor.CredentialID).Scan(&live) // #nosec G202 -- liveCredentialWhere builds SQL from a time integer.
	return live, err
}

func privateCallRequestLive(ctx context.Context, tx execer, actor PrivateCallActor, now time.Time) (bool, error) {
	if actor.CredentialID <= 0 {
		return false, nil
	}
	return privateCallCredentialLive(ctx, tx, actor, now)
}

func createPrivateCall(ctx context.Context, tx execer, conversationID int64, actor PrivateCallActor, peer int64, event string, now time.Time) (privateCall, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return privateCall{}, err
	}
	row, ok := schema.NextPrivateCall(schema.PrivateCallFromNone, event)
	if !ok {
		return privateCall{}, fmt.Errorf("store: invalid private call creation event %q", event)
	}
	c := privateCall{
		PrivateCallV1: schema.PrivateCallV1{
			Schema: schema.PrivateCallSchemaV1, ID: base64.RawURLEncoding.EncodeToString(random[:]),
			ConversationID: conversationID, CallerID: actor.PrincipalID, CalleeID: peer,
			State: row.To, Revision: 1, CreatedAt: schema.NewTimestamp(now),
		},
		callerCredential: actor.CredentialID, lastEvent: event,
	}
	if c.State.Terminal() {
		c.endedAt = privateCallTime(now)
	} else {
		c.deadline = privateCallTime(now.Add(schema.PrivateCallRingSeconds * time.Second))
	}
	if err := c.view(actor).Validate(); err != nil {
		return privateCall{}, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO private_calls
		(id, conversation_id, caller, callee, caller_credential, state, last_event, revision, created_at, deadline, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, conversationID, c.CallerID, c.CalleeID,
		c.callerCredential, c.State, c.lastEvent, c.Revision, now.UnixMilli(), c.deadline, c.endedAt)
	if err != nil || c.State.Terminal() {
		return c, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO private_call_holders (principal_id, call_id) VALUES (?, ?), (?, ?)`,
		c.CallerID, c.ID, c.CalleeID, c.ID)
	return c, err
}

func privateCallTime(t time.Time) sql.NullInt64 {
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func transitionPrivateCall(ctx context.Context, tx execer, c *privateCall, row schema.PrivateCallTransition, credentialID int64, now time.Time) error {
	previous := *c
	c.State, c.Reason, c.lastEvent = row.To, row.Reason, row.Event
	c.Revision++
	c.deadline = sql.NullInt64{}
	if row.Event == schema.PrivateCallEventAccept {
		c.calleeCredential = credentialID
		c.deadline = privateCallTime(now.Add(schema.PrivateCallJoinSeconds * time.Second))
		c.deadline.Int64 = max(c.deadline.Int64, c.CreatedAt.Time().UnixMilli())
	}
	if c.State.Terminal() {
		// Wall clocks can move backwards (including when restoring a backup).
		c.endedAt = privateCallTime(now)
		c.endedAt.Int64 = max(c.endedAt.Int64, c.CreatedAt.Time().UnixMilli())
		c.callerAbsent, c.calleeAbsent = sql.NullInt64{}, sql.NullInt64{}
	}
	reader := PrivateCallActor{PrincipalID: c.CallerID}
	if err := schema.ValidatePrivateCallStep(previous.view(reader), c.view(reader)); err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE private_calls SET state = ?, reason = ?, last_event = ?, revision = ?,
		callee_credential = NULLIF(?, 0), deadline = ?, ended_at = ?, caller_absent_since = ?, callee_absent_since = ?
		WHERE id = ? AND revision = ?`, c.State, c.Reason, c.lastEvent, c.Revision, c.calleeCredential,
		c.deadline, c.endedAt, c.callerAbsent, c.calleeAbsent, c.ID, previous.Revision)
	if err != nil {
		return err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: check private call revision CAS: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("store: private call revision CAS affected %d rows", n)
	}
	if c.State.Terminal() {
		// #2032 can retire its room in this same transaction. Terminal
		// attempts stay available for its orphan sweep after a crash.
		_, err = tx.ExecContext(ctx, `DELETE FROM private_call_holders WHERE call_id = ?`, c.ID)
	}
	return err
}
