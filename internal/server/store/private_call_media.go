package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Keep retired names as tombstones. A LiveKit refresh token can recreate an
// old room when auto-create is enabled, including after our prefix changes.
var privateCallMediaMigration = []string{
	`CREATE TABLE call_rooms (
		call_id TEXT PRIMARY KEY REFERENCES private_calls(id),
		name TEXT NOT NULL UNIQUE,
		retired INTEGER NOT NULL DEFAULT 0,
		delete_pending INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX call_rooms_work ON call_rooms (retired, delete_pending)
		WHERE retired = 0 OR delete_pending = 1`,
	`CREATE TABLE call_room_holders (
		call_id TEXT NOT NULL REFERENCES call_rooms(call_id),
		principal_id INTEGER NOT NULL REFERENCES principals(id),
		credential_id INTEGER NOT NULL REFERENCES credentials(id),
		PRIMARY KEY (call_id, principal_id)
	)`,
}

// Index the same ended_at written transactionally with room retirement.
var privateCallMediaWatchMigration = []string{
	`CREATE INDEX private_calls_ended_at ON private_calls (ended_at) WHERE ended_at IS NOT NULL`,
}

// Watch retired rooms on the fast pass for one refresh token's 10-minute
// lifetime plus about a minute of LiveKit expiry leeway. With room.auto_create
// disabled, a retired room name cannot be recreated. If enabled, each rejoin
// yields a fresh refresh token, so the orphan sweep keeps checking older rooms.
const privateCallMediaWatchWindow = 11 * time.Minute

func retireCallRoom(ctx context.Context, tx execer, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM call_room_holders WHERE call_id = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE call_rooms SET retired = 1, delete_pending = 1 WHERE call_id = ?`, id)
	return err
}

// retireLiveCallRooms queues media cleanup after a bulk restart or restore.
func retireLiveCallRooms(ctx context.Context, tx execer) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM call_room_holders`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE call_rooms SET retired = 1, delete_pending = 1 WHERE retired = 0`)
	return err
}

// PreparePrivateCallMedia checks eligibility and reserves the attempt's one
// room before any request can create it in LiveKit. With media disabled it
// checks access without reserving a name.
func (s *Store) PreparePrivateCallMedia(ctx context.Context, id string, actor PrivateCallActor, clock func() time.Time, configured bool) (name string, out PrivateCallResult, err error) {
	out, err = s.withPrivateCallMedia(ctx, id, actor, clock, func(tx execer, _ privateCall) error {
		if !configured {
			return nil
		}
		err := tx.QueryRowContext(ctx, `SELECT name FROM call_rooms WHERE call_id = ? AND retired = 0`, id).Scan(&name)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		name, err = newVoiceRoomName(s.roomPrefix())
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO call_rooms (call_id, name) VALUES (?, ?)`, id, name)
		return err
	})
	return name, out, err
}

// GrantPrivateCallMedia rechecks access after CreateRoom. The local signer
// runs under the same write lock as revocation and retirement. The caller can
// return the signed grant only after the holder record commits successfully.
func (s *Store) GrantPrivateCallMedia(ctx context.Context, id string, actor PrivateCallActor, clock func() time.Time, name string, sign func(int64) error) (PrivateCallResult, error) {
	return s.withPrivateCallMedia(ctx, id, actor, clock, func(tx execer, c privateCall) error {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM call_rooms WHERE call_id = ? AND retired = 0`, id).Scan(&current); err != nil {
			return err
		}
		if current != name {
			return errors.New("store: private call room changed during grant")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO call_room_holders (call_id, principal_id, credential_id)
			VALUES (?, ?, ?) ON CONFLICT (call_id, principal_id) DO NOTHING`,
			id, actor.PrincipalID, actor.CredentialID); err != nil {
			return err
		}
		return sign(c.Revision)
	})
}

func (s *Store) withPrivateCallMedia(ctx context.Context, id string, actor PrivateCallActor, clock func() time.Time, allowed func(execer, privateCall) error) (out PrivateCallResult, err error) {
	err = s.withImmediateTx(ctx, func(tx execer) error {
		now := clock().Truncate(time.Millisecond)
		if live, err := privateCallRequestLive(ctx, tx, actor, now); err != nil {
			return err
		} else if !live {
			return ErrPrivateCallNotFound
		}
		c, err := privateCallByID(ctx, tx, id)
		if err != nil {
			return err
		}
		// Unavailable invites are visible only in the original invite response
		// and its retries. Like the action routes, treat the ID as absent.
		if c.role(actor.PrincipalID) == "" || c.State == schema.PrivateCallStateUnavailable {
			return ErrPrivateCallNotFound
		}
		if _, err := refreshPrivateCall(ctx, tx, &c, now); err != nil {
			return err
		}
		out.Call = c.view(actor)
		voice, err := privateCallVoiceAllowed(ctx, tx, actor.PrincipalID)
		if err != nil {
			return err
		}
		switch {
		case !voice:
			out.Code = schema.ErrorCodeVoiceNotAllowed
		case c.State != schema.PrivateCallStateAccepted && c.State != schema.PrivateCallStateActive:
			out.Code = schema.ErrorCodeCallStateChanged
		case !out.Call.SelectedHere:
			out.Code = schema.ErrorCodeCallWrongDevice
		default:
			return allowed(tx, c)
		}
		return nil
	})
	return out, err
}

// PrivateCallMediaRoomLive is checked under the poller's room control lock
// before creating a room. Once retired, the same name cannot become live again.
func (s *Store) PrivateCallMediaRoomLive(ctx context.Context, name string) (bool, error) {
	var live bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM call_rooms
		WHERE name = ? AND retired = 0)`, name).Scan(&live)
	return live, err
}

type PrivateCallMediaRoom struct {
	CallID, Name           string
	Revision               int64
	Retired, DeletePending bool
	CallerID, CalleeID     int64
}

// PrivateCallMediaRooms returns live rooms, pending deletes and recent
// tombstones. Read recent tombstones by ended_at so old ones stay off the fast path.
func (s *Store) PrivateCallMediaRooms(ctx context.Context, now time.Time) ([]PrivateCallMediaRoom, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.call_id, r.name, c.revision, r.retired, r.delete_pending, c.caller, c.callee
		FROM call_rooms r JOIN private_calls c ON c.id = r.call_id
		WHERE r.retired = 0 OR r.delete_pending = 1
		UNION ALL
		SELECT r.call_id, r.name, c.revision, r.retired, r.delete_pending, c.caller, c.callee
		FROM private_calls c JOIN call_rooms r ON r.call_id = c.id
		WHERE c.ended_at > ? AND r.retired = 1 AND r.delete_pending = 0
		ORDER BY retired DESC, delete_pending DESC, call_id`, now.Add(-privateCallMediaWatchWindow).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rooms []PrivateCallMediaRoom
	for rows.Next() {
		var r PrivateCallMediaRoom
		if err := rows.Scan(&r.CallID, &r.Name, &r.Revision, &r.Retired, &r.DeletePending, &r.CallerID, &r.CalleeID); err != nil {
			return nil, err
		}
		rooms = append(rooms, r)
	}
	return rooms, rows.Err()
}

// PrivateCallMediaHolders is read after the LiveKit participant list. Every
// legitimately connected endpoint already had a committed holder at that point.
func (s *Store) PrivateCallMediaHolders(ctx context.Context, id string) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT principal_id, credential_id FROM call_room_holders WHERE call_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	holders := make(map[int64]int64, 2)
	for rows.Next() {
		var principal, credential int64
		if err := rows.Scan(&principal, &credential); err != nil {
			return nil, err
		}
		holders[principal] = credential
	}
	return holders, rows.Err()
}

func (s *Store) MarkPrivateCallMediaDeleted(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE call_rooms SET delete_pending = 0 WHERE name = ? AND retired = 1`, name)
	return err
}

// PrivateCallRoomStates classifies names already seen in LiveKit. Live rooms
// are known to the orphan sweep and retired rooms can only be deleted.
func (s *Store) PrivateCallRoomStates(ctx context.Context, names []string) (map[string]bool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(names)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name, retired FROM call_rooms
		WHERE name IN (SELECT value FROM json_each(?))`, string(raw))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	states := make(map[string]bool)
	for rows.Next() {
		var name string
		var retired bool
		if err := rows.Scan(&name, &retired); err != nil {
			return nil, err
		}
		states[name] = retired
	}
	return states, rows.Err()
}

// ObservePrivateCallEndpoints applies media poller observations in poll
// order for the attempt revision observed. Each
// credential is the selected credential seen in the room, or zero when
// absent. A late observation cannot resurrect an expired attempt.
func (s *Store) ObservePrivateCallEndpoints(ctx context.Context, id string, revision, callerCredential, calleeCredential int64, clock func() time.Time) error {
	return s.withImmediateTx(ctx, func(tx execer) error {
		c, err := privateCallByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if c.Revision != revision || (c.State != schema.PrivateCallStateAccepted && c.State != schema.PrivateCallStateActive) {
			return nil
		}
		now := clock().Truncate(time.Millisecond)
		if changed, err := refreshPrivateCall(ctx, tx, &c, now); changed || err != nil {
			return err
		}
		callerPresent := callerCredential == c.callerCredential
		calleePresent := calleeCredential == c.calleeCredential
		if c.State == schema.PrivateCallStateAccepted {
			if callerPresent && calleePresent {
				return applyPrivateCallServerEvent(ctx, tx, &c, schema.PrivateCallEventJoined, now)
			}
			return nil
		}
		return recordPrivateCallAbsence(ctx, tx, c, callerPresent, calleePresent, now)
	})
}
