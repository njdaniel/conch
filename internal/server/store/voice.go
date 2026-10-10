package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
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

// voiceRoomPrefix starts every room name. The rest is 128 random bits, never
// derived from a channel or net name.
const voiceRoomPrefix = "conch-"

// VoiceRoom is a stored voice room. NetID is 0 for the channel-wide room.
type VoiceRoom struct {
	ID        int64
	ChannelID int64
	NetID     int64
	RoomName  string
	CreatedAt time.Time
}

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

// ChannelVoiceRoom returns the channel-wide voice room of channelID, creating
// its row on first use. Every call after the first is one read: a room row is
// never changed or deleted, so a row that is found is the answer, and a member
// asking for sessions in a loop takes no write lock. Creation is idempotent
// and safe under concurrency: the insert and the read run in one immediate
// transaction, the partial unique index admits one channel-wide row per
// channel, and every caller gets that row. The channel must exist (a foreign
// key enforces it).
func (s *Store) ChannelVoiceRoom(ctx context.Context, channelID int64) (VoiceRoom, error) {
	existing := VoiceRoom{ChannelID: channelID}
	var existingAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, room_name, created_at FROM voice_rooms WHERE channel_id = ? AND net_id IS NULL`,
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
		// A conflict on the channel-wide index is the normal "already exists"
		// case. A clash on room_name (2^-128) is not covered by this target and
		// surfaces as an error rather than being swallowed.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO voice_rooms (channel_id, net_id, room_name, created_at) VALUES (?, NULL, ?, ?)
			 ON CONFLICT (channel_id) WHERE net_id IS NULL DO NOTHING`,
			channelID, name, now.UnixMilli()); err != nil {
			return fmt.Errorf("store: create voice room for channel %d: %w", channelID, err)
		}
		var createdAt int64
		err := tx.QueryRowContext(ctx,
			`SELECT id, room_name, created_at FROM voice_rooms WHERE channel_id = ? AND net_id IS NULL`,
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

// CountVoiceRooms returns the number of stored voice rooms (all audiences).
func (s *Store) CountVoiceRooms(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM voice_rooms").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count voice rooms: %w", err)
	}
	return n, nil
}
