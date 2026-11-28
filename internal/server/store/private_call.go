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
	Created    bool
	Code       string
	RetryAfter time.Duration
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

func (c privateCall) peer(principalID int64) int64 {
	if principalID == c.CallerID {
		return c.CalleeID
	}
	return c.CallerID
}

func (c privateCall) selectedCredential(principalID int64) int64 {
	switch principalID {
	case c.CallerID:
		return c.callerCredential
	case c.CalleeID:
		return c.calleeCredential
	default:
		return 0
	}
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
	credential := c.selectedCredential(actor.PrincipalID)
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

// privateCallPairTx is the one lookup of the pair an invite or a history read
// is about: the conversation as caller may see it (dmViewTx), and its other
// person as caller may know them (dmMayKnowTx, the directory's rule, which a
// resume of a conversation is held to as well). A conversation the caller
// cannot see, a peer the caller may not know and an id never issued are all
// the same ErrPrivateCallPair, so nothing tells them apart.
func privateCallPairTx(ctx context.Context, tx execer, caller, conversationID int64) (int64, error) {
	row, err := dmViewTx(ctx, tx, caller, conversationID)
	if errors.Is(err, ErrDMNotFound) {
		return 0, ErrPrivateCallPair
	}
	if err != nil {
		return 0, err
	}
	peer := row.peer(caller)
	known, err := dmMayKnowTx(ctx, tx, caller, peer)
	if err != nil {
		return 0, err
	}
	if !known {
		return 0, ErrPrivateCallPair
	}
	return peer, nil
}

func privateCallVoiceAllowed(ctx context.Context, tx execer, principalID int64) (bool, error) {
	p, _, err := queryPrincipal(ctx, tx, principalID)
	if err != nil {
		return false, err
	}
	d, err := readPermissionDocTx(ctx, tx)
	return err == nil && p.Kind == PrincipalHuman && principalHolds(p, d, schema.PermUseVoice), err
}

// privateCallOwnCredential holds for a credentials row aliased c that its
// principal made for themselves (credentials.self_made, provenance.go: the
// one record the route guard reads too), so a callee is rung only on a device
// that can answer: an act-as credential, one redeemed from a link another
// made and one from before the provenance migration are not endpoints.
// #nosec G101 -- an SQL fragment, not a credential.
const privateCallOwnCredential = `c.self_made = 1`

// A zero credential asks whether the callee has any signed-in device of their
// own, including one that can reconnect to the ring stream.
func privateCallCredentialLive(ctx context.Context, tx execer, actor PrivateCallActor, now time.Time) (bool, error) {
	var live bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM credentials c JOIN principals p ON p.id = c.principal_id
		WHERE p.id = ? AND p.disabled_at IS NULL AND p.hidden_at IS NULL AND p.kind = 'human' AND (? = 0 OR c.id = ?)
		AND NOT EXISTS (SELECT 1 FROM guest_visits g WHERE g.principal_id = p.id AND g.state <> 'converted')
		AND `+privateCallOwnCredential+` AND `+
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
	if err == nil {
		recordPrivateCallUpdate(tx, c)
	}
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
		// Retire the room before releasing the participants for another call.
		if err := retireCallRoom(ctx, tx, c.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM private_call_holders WHERE call_id = ?`, c.ID)
	}
	if err == nil {
		recordPrivateCallUpdate(tx, *c)
	}
	return err
}
