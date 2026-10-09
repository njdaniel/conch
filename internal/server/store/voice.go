package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Voice rooms (issue #126, docs/design/voice-control-plane.md §3). conchd, not
// LiveKit, names the room for an audience, and the name is stored here. This
// file knows nothing about LiveKit: it hands out stable names.

// AuditVoiceSessionIssued is written by the server for every voice session it
// issues, never for a refusal. Its subject is "channel:<id>". The detail names
// the channel, the identity and what was granted; it never contains a token or
// a room name.
const AuditVoiceSessionIssued = "voice_session_issued"

// Audit actions written by the voice presence poller (issue #127, design note
// §7). Their times are accurate to one polling interval. The detail of each
// carries source=observed; none carries a token or a room name.
const (
	AuditVoiceJoined                 = "voice_joined"
	AuditVoiceLeft                   = "voice_left"
	AuditVoiceTransmitStarted        = "voice_transmit_started"
	AuditVoiceTransmitStopped        = "voice_transmit_stopped"
	AuditVoiceParticipantRemoved     = "voice_participant_removed"
	AuditVoiceEnforcementUnavailable = "voice_enforcement_unavailable"
)

// AuditVoiceRoomRotated is written, by the store and in the transaction that
// rotates the room, once per rotation (issue #161). Its actor is "system",
// its subject "channel:<id>" and its detail "reason=<reason>"; it never
// contains a room name, old or new.
const AuditVoiceRoomRotated = "voice_room_rotated"

// Why a room was rotated: the first way one of its holders stopped being
// valid, in this order. They are the reasons InvalidVoiceRooms reports and the
// detail of voice_room_rotated.
const (
	VoiceRotatePrincipalDisabled = "principal_disabled"
	VoiceRotateMemberRemoved     = "member_removed"
	VoiceRotateRevoked           = "credential_revoked"
	VoiceRotateExpired           = "credential_expired"
	VoiceRotateNotHuman          = "not_human"
)

// voiceRoomPrefix starts every room name. The rest is 128 random bits, never
// derived from a channel or net name.
const voiceRoomPrefix = "conch-"

// VoiceRoom is a stored voice room. NetID is 0 for the channel-wide room.
// RetiredAt is zero for a live room. A retired room is one the channel no
// longer uses; its row is kept so that the sweep can find the room in LiveKit
// and delete it, until a day after LiveKit last listed it (RetiredAt is moved
// forward each time a sweep still finds the room there).
type VoiceRoom struct {
	ID        int64
	ChannelID int64
	NetID     int64
	RoomName  string
	CreatedAt time.Time
	RetiredAt time.Time
}

// ErrVoiceRoomRetired is returned when a holder is recorded against a room
// that was rotated away. The caller asks for the channel's current room again.
var ErrVoiceRoomRetired = errors.New("store: voice room is retired")

// newVoiceRoomName returns "conch-" plus 128 random bits in lower-case base32
// without padding (26 characters).
func newVoiceRoomName() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: generate voice room name: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return voiceRoomPrefix + strings.ToLower(enc), nil
}

// ChannelVoiceRoom returns the channel's live channel-wide voice room,
// creating its row on first use. Every call after the first is one read, so a
// member asking for sessions in a loop takes no write lock. Creation is
// idempotent and safe under concurrency: the insert and the read run in one
// immediate transaction, the partial unique index admits one live
// channel-wide row per channel, and every caller gets that row. After a
// rotation the answer is the new room; a retired room is never returned. The
// channel must exist (a foreign key enforces it).
func (s *Store) ChannelVoiceRoom(ctx context.Context, channelID int64) (VoiceRoom, error) {
	existing := VoiceRoom{ChannelID: channelID}
	var existingAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, room_name, created_at FROM voice_rooms WHERE channel_id = ? AND net_id IS NULL AND retired_at IS NULL`,
		channelID).Scan(&existing.ID, &existing.RoomName, &existingAt)
	if err == nil {
		existing.CreatedAt = time.UnixMilli(existingAt)
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return VoiceRoom{}, fmt.Errorf("store: read voice room for channel %d: %w", channelID, err)
	}
	name, err := newVoiceRoomName()
	if err != nil {
		return VoiceRoom{}, err
	}
	now := time.Now().Truncate(time.Millisecond)
	var room VoiceRoom
	err = s.withImmediateTx(ctx, func(tx execer) error {
		// A conflict on the live channel-wide index is the normal "already
		// exists" case. A clash on room_name (2^-128) is not covered by this
		// target and surfaces as an error rather than being swallowed.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO voice_rooms (channel_id, net_id, room_name, created_at) VALUES (?, NULL, ?, ?)
			 ON CONFLICT (channel_id) WHERE net_id IS NULL AND retired_at IS NULL DO NOTHING`,
			channelID, name, now.UnixMilli()); err != nil {
			return fmt.Errorf("store: create voice room for channel %d: %w", channelID, err)
		}
		var createdAt int64
		err := tx.QueryRowContext(ctx,
			`SELECT id, room_name, created_at FROM voice_rooms WHERE channel_id = ? AND net_id IS NULL AND retired_at IS NULL`,
			channelID).Scan(&room.ID, &room.RoomName, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: voice room for channel %d: %w", channelID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: read voice room for channel %d: %w", channelID, err)
		}
		room.ChannelID = channelID
		room.CreatedAt = time.UnixMilli(createdAt)
		return nil
	})
	if err != nil {
		return VoiceRoom{}, err
	}
	return room, nil
}

// CountVoiceRooms returns the number of stored voice rooms (all audiences,
// live and retired).
func (s *Store) CountVoiceRooms(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM voice_rooms").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count voice rooms: %w", err)
	}
	return n, nil
}

// voiceRoomColumns is the select list queryVoiceRooms scans.
const voiceRoomColumns = `r.id, r.channel_id, r.net_id, r.room_name, r.created_at, r.retired_at`

// ListVoiceRooms returns every live voice room, oldest first. The presence
// poller's sweep uses it to learn which of LiveKit's rooms are Conch's.
func (s *Store) ListVoiceRooms(ctx context.Context) ([]VoiceRoom, error) {
	return s.queryVoiceRooms(ctx, "list voice rooms",
		`SELECT `+voiceRoomColumns+` FROM voice_rooms r WHERE r.retired_at IS NULL ORDER BY r.id ASC`)
}

// ListRetiredVoiceRooms returns every retired voice room, oldest first: the
// rooms the sweep must see gone from LiveKit.
func (s *Store) ListRetiredVoiceRooms(ctx context.Context) ([]VoiceRoom, error) {
	return s.queryVoiceRooms(ctx, "list retired voice rooms",
		`SELECT `+voiceRoomColumns+` FROM voice_rooms r WHERE r.retired_at IS NOT NULL ORDER BY r.id ASC`)
}

// SeenRetiredVoiceRooms records that LiveKit still listed these retired rooms
// at the given time, by moving their retired_at forward to it. The time a
// retired row is kept (PruneRetiredVoiceRooms) is therefore counted from when
// LiveKit last had the room, not from the rotation: a room that could only be
// deleted late (conchd stopped, LiveKit unreachable) had people in it and
// LiveKit renewing their tokens until then. It never moves a time backward
// and never touches a live row.
func (s *Store) SeenRetiredVoiceRooms(ctx context.Context, ids []int64, at time.Time) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE voice_rooms SET retired_at = ? WHERE id = ? AND retired_at IS NOT NULL AND retired_at < ?`,
			at.UnixMilli(), id, at.UnixMilli()); err != nil {
			return fmt.Errorf("store: mark retired voice room seen: %w", err)
		}
	}
	return nil
}

// PruneRetiredVoiceRooms deletes the retired rooms whose retired_at (the
// rotation, or the last time LiveKit still listed the room, whichever is
// later) is before cutoff, except those whose ids are in keep (the ones
// LiveKit lists now, which the sweep is deleting there), and returns how many
// it deleted. A retired row exists so that the sweep can find a room LiveKit
// still has; once LiveKit has not had it for long enough that no token for it
// can be valid, there is nothing left for the row to do. Room names are 128 random bits, so
// a pruned name being generated again is not a case. Live rows are never
// touched, nor a row that still has a holder recorded against it.
func (s *Store) PruneRetiredVoiceRooms(ctx context.Context, cutoff time.Time, keep []int64) (int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM voice_rooms WHERE retired_at IS NOT NULL AND retired_at < ? ORDER BY id ASC`, cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: prune retired voice rooms: %w", err)
	}
	var old []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: prune retired voice rooms: %w", err)
		}
		if !slices.Contains(keep, id) {
			old = append(old, id)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: prune retired voice rooms: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: prune retired voice rooms: %w", err)
	}
	// Each delete repeats the conditions, so a row is judged as it is at the
	// moment it goes, not as it was when listed.
	var pruned int64
	for _, id := range old {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM voice_rooms
			 WHERE id = ? AND retired_at IS NOT NULL AND retired_at < ?
			   AND NOT EXISTS (SELECT 1 FROM voice_room_holders h WHERE h.room_id = voice_rooms.id)`,
			id, cutoff.UnixMilli())
		if err != nil {
			return pruned, fmt.Errorf("store: prune retired voice rooms: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return pruned, fmt.Errorf("store: prune retired voice rooms: %w", err)
		}
		pruned += n
	}
	return pruned, nil
}

// VoiceRoomsForChannel returns the live voice rooms of channelID.
func (s *Store) VoiceRoomsForChannel(ctx context.Context, channelID int64) ([]VoiceRoom, error) {
	return s.queryVoiceRooms(ctx, fmt.Sprintf("list voice rooms of channel %d", channelID),
		`SELECT `+voiceRoomColumns+` FROM voice_rooms r WHERE r.channel_id = ? AND r.retired_at IS NULL ORDER BY r.id ASC`, channelID)
}

// VoiceRoomsForMember returns the live voice rooms of every channel
// principalID is currently a member of: the rooms an immediate removal has to
// cover when a principal is disabled.
func (s *Store) VoiceRoomsForMember(ctx context.Context, principalID int64) ([]VoiceRoom, error) {
	return s.queryVoiceRooms(ctx, fmt.Sprintf("list voice rooms of principal %d", principalID),
		`SELECT `+voiceRoomColumns+`
		 FROM voice_rooms r JOIN channel_members m ON m.channel_id = r.channel_id
		 WHERE m.principal_id = ? AND r.retired_at IS NULL ORDER BY r.id ASC`, principalID)
}

// queryVoiceRooms runs query and scans voice rooms; what names the operation
// in errors (never a room name).
func (s *Store) queryVoiceRooms(ctx context.Context, what, query string, args ...any) ([]VoiceRoom, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	rooms := []VoiceRoom{}
	for rows.Next() {
		var r VoiceRoom
		var net, retired sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&r.ID, &r.ChannelID, &net, &r.RoomName, &createdAt, &retired); err != nil {
			return nil, fmt.Errorf("store: %s: %w", what, err)
		}
		r.NetID = net.Int64
		r.CreatedAt = time.UnixMilli(createdAt)
		if retired.Valid {
			r.RetiredAt = time.UnixMilli(retired.Int64)
		}
		rooms = append(rooms, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	return rooms, nil
}

// ---------------------------------------------------------------------------
// Holders and rotation (issue #161)

// VoiceHolder is one credential a session was issued under for a room: a
// person who may hold a token LiveKit will keep renewing.
type VoiceHolder struct {
	RoomID       int64
	PrincipalID  int64
	CredentialID int64
}

// RecordVoiceHolder records that a session for roomID was issued to
// principalID under credentialID. It is called before the token is signed, so
// a token never exists for a holder the store does not know. It is idempotent:
// the row exists once per (room, principal, credential), and recording it
// again is a read, not a write. It returns ErrVoiceRoomRetired when the room
// was rotated away (the caller asks for the channel's current room again), and
// ErrNotFound for a room that does not exist.
func (s *Store) RecordVoiceHolder(ctx context.Context, roomID, principalID, credentialID int64) error {
	var retired sql.NullInt64
	var have bool
	err := s.db.QueryRowContext(ctx,
		`SELECT r.retired_at, EXISTS (SELECT 1 FROM voice_room_holders h
		   WHERE h.room_id = r.id AND h.principal_id = ? AND h.credential_id = ?)
		 FROM voice_rooms r WHERE r.id = ?`, principalID, credentialID, roomID).Scan(&retired, &have)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("store: read voice holder: %w", err)
	case retired.Valid:
		return ErrVoiceRoomRetired
	case have:
		return nil
	}
	now := time.Now().Truncate(time.Millisecond)
	return s.withImmediateTx(ctx, func(tx execer) error {
		// Re-checked under the write lock: a rotation may have committed since
		// the read above, and a holder must never be added to a retired room.
		var retired sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT retired_at FROM voice_rooms WHERE id = ?`, roomID).Scan(&retired)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: read voice room: %w", err)
		case retired.Valid:
			return ErrVoiceRoomRetired
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO voice_room_holders (room_id, principal_id, credential_id, created_at) VALUES (?, ?, ?, ?)`,
			roomID, principalID, credentialID, now.UnixMilli()); err != nil {
			return fmt.Errorf("store: record voice holder: %w", err)
		}
		return nil
	})
}

// VoiceRoomLive reports whether roomID exists and has not been retired.
func (s *Store) VoiceRoomLive(ctx context.Context, roomID int64) (bool, error) {
	var retired sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT retired_at FROM voice_rooms WHERE id = ?`, roomID).Scan(&retired)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read voice room: %w", err)
	}
	return !retired.Valid, nil
}

// VoiceRoomHolders returns the holders recorded against roomID.
func (s *Store) VoiceRoomHolders(ctx context.Context, roomID int64) ([]VoiceHolder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT room_id, principal_id, credential_id FROM voice_room_holders WHERE room_id = ?
		 ORDER BY principal_id, credential_id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: list voice holders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []VoiceHolder
	for rows.Next() {
		var h VoiceHolder
		if err := rows.Scan(&h.RoomID, &h.PrincipalID, &h.CredentialID); err != nil {
			return nil, fmt.Errorf("store: list voice holders: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list voice holders: %w", err)
	}
	return out, nil
}

// VoiceViolation is a live room one of whose holders is no longer valid.
type VoiceViolation struct {
	Room VoiceRoom
	// Reason is one of the VoiceRotate* constants: the first way, in the
	// order those constants are listed, a holder of the room stopped being
	// valid.
	Reason string
}

// InvalidVoiceRooms returns every live room with a holder that is not a live
// credential of a current, enabled, human member of the room's channel, in
// one statement for the whole table. "Live credential" is the
// definition CredentialLive uses (not revoked, not expired at now, principal
// not disabled) and now is passed in so the caller's clock decides expiry. A
// room with no holders, and every retired room, is never returned. A net's
// room (V5) is held to the same conditions, which a seat on a net also needs;
// V5 adds net membership to them. Leaving net rooms out until then would let
// their holders go unchecked the day a session is first issued for one.
func (s *Store) InvalidVoiceRooms(ctx context.Context, now time.Time) ([]VoiceViolation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+voiceRoomColumns+`, MIN(CASE
		     WHEN p.id IS NULL OR p.disabled_at IS NOT NULL THEN 1
		     WHEN NOT EXISTS (SELECT 1 FROM channel_members m
		                      WHERE m.channel_id = r.channel_id AND m.principal_id = h.principal_id) THEN 2
		     WHEN c.id IS NULL OR c.principal_id <> h.principal_id OR c.revoked_at IS NOT NULL THEN 3
		     WHEN c.expires_at IS NOT NULL AND c.expires_at <= ? THEN 4
		     WHEN p.kind <> 'human' THEN 5
		   END) AS why
		 FROM voice_rooms r
		 JOIN voice_room_holders h ON h.room_id = r.id
		 LEFT JOIN credentials c ON c.id = h.credential_id
		 LEFT JOIN principals p ON p.id = h.principal_id
		 WHERE r.retired_at IS NULL
		 GROUP BY r.id
		 HAVING why IS NOT NULL
		 ORDER BY r.id ASC`, now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("store: find invalid voice rooms: %w", err)
	}
	defer func() { _ = rows.Close() }()
	reasons := map[int64]string{
		1: VoiceRotatePrincipalDisabled,
		2: VoiceRotateMemberRemoved,
		3: VoiceRotateRevoked,
		4: VoiceRotateExpired,
		5: VoiceRotateNotHuman,
	}
	var out []VoiceViolation
	for rows.Next() {
		var r VoiceRoom
		var net, retired sql.NullInt64
		var createdAt, why int64
		if err := rows.Scan(&r.ID, &r.ChannelID, &net, &r.RoomName, &createdAt, &retired, &why); err != nil {
			return nil, fmt.Errorf("store: find invalid voice rooms: %w", err)
		}
		r.NetID = net.Int64
		r.CreatedAt = time.UnixMilli(createdAt)
		out = append(out, VoiceViolation{Room: r, Reason: reasons[why]})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: find invalid voice rooms: %w", err)
	}
	return out, nil
}

// RotateVoiceRoom gives the audience of roomID a new room: in one transaction
// it retires the row, drops the holders recorded against it, inserts the next
// live row under a fresh name and appends one voice_room_rotated event. It
// reports rotated == false, having changed nothing, when roomID was already
// retired, so concurrent callers rotate a room exactly once between them. The
// retired name is never handed out again: names are 128 random bits, and
// room_name is unique over live and retired rows while the retired row lasts.
// Deleting the room in LiveKit is the caller's job and is retried by the sweep
// from the retired row.
func (s *Store) RotateVoiceRoom(ctx context.Context, roomID int64, reason string) (next VoiceRoom, rotated bool, err error) {
	name, err := newVoiceRoomName()
	if err != nil {
		return VoiceRoom{}, false, err
	}
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		var channelID int64
		var net, retired sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT channel_id, net_id, retired_at FROM voice_rooms WHERE id = ?`, roomID).Scan(&channelID, &net, &retired)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: read voice room: %w", err)
		case retired.Valid:
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE voice_rooms SET retired_at = ? WHERE id = ?`, now.UnixMilli(), roomID); err != nil {
			return fmt.Errorf("store: retire voice room: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM voice_room_holders WHERE room_id = ?`, roomID); err != nil {
			return fmt.Errorf("store: drop voice holders: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO voice_rooms (channel_id, net_id, room_name, created_at) VALUES (?, ?, ?, ?)`,
			channelID, net, name, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: create next voice room: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create next voice room: %w", err)
		}
		if err := appendAuditEventTx(ctx, tx, "system", AuditVoiceRoomRotated,
			fmt.Sprintf("channel:%d", channelID), "reason="+reason, now); err != nil {
			return err
		}
		next = VoiceRoom{ID: id, ChannelID: channelID, NetID: net.Int64, RoomName: name, CreatedAt: now}
		rotated = true
		return nil
	})
	if err != nil {
		return VoiceRoom{}, false, err
	}
	return next, rotated, nil
}
