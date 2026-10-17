package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Read marks (issue #351). A read mark is how far one principal has read one
// channel. It is a message id and nothing else: which messages are unread is
// worked out when asked, by CountVisibleMessagesAfter in audience.go, which is
// the only code that reads message rows. This file keeps the numbers.
//
// The mark belongs to the principal, not to a session or a credential, so two
// clients of one person share it. Nothing here reads a clock of its own: the
// caller passes the time.

// ReadMark is a stored read mark. The zero value is "never marked".
type ReadMark struct {
	ChannelID  int64
	LastReadID int64
	UpdatedAt  time.Time
}

// ReadMarksFor returns principalID's marks by channel id. A channel never
// marked has no entry. Whether the principal may still use a channel is the
// caller's question: a mark outlives membership.
func (s *Store) ReadMarksFor(ctx context.Context, principalID int64) (map[int64]ReadMark, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel_id, last_read_id, updated_at FROM read_marks WHERE principal_id = ?`, principalID)
	if err != nil {
		return nil, fmt.Errorf("store: list read marks of principal %d: %w", principalID, err)
	}
	defer func() { _ = rows.Close() }()
	marks := map[int64]ReadMark{}
	for rows.Next() {
		var m ReadMark
		var at int64
		if err := rows.Scan(&m.ChannelID, &m.LastReadID, &at); err != nil {
			return nil, fmt.Errorf("store: list read marks of principal %d: %w", principalID, err)
		}
		m.UpdatedAt = time.UnixMilli(at)
		marks[m.ChannelID] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list read marks of principal %d: %w", principalID, err)
	}
	return marks, nil
}

// MarkRead moves the reader's mark in channelID forward to lastReadID and
// returns the mark as stored. The caller has checked that the reader is a
// member of the channel.
//
//   - A lastReadID below the stored mark leaves it alone (the larger id wins,
//     in the statement itself, so two clients racing cannot move it back).
//   - A lastReadID above the newest message the reader may see is taken as
//     that message, by the same visibility function the lists use.
//
// updated_at changes only when the mark does. A reader without a verified
// principal has no mark to move and is refused.
func (s *Store) MarkRead(ctx context.Context, channelID int64, reader Reader, lastReadID int64, now time.Time) (ReadMark, error) {
	if reader.PrincipalID <= 0 {
		return ReadMark{}, errors.New("store: mark read: a read mark needs a principal")
	}
	if lastReadID < 0 {
		return ReadMark{}, errors.New("store: mark read: a mark is never negative")
	}
	newest, err := s.newestVisibleMessageID(ctx, channelID, reader)
	if err != nil {
		return ReadMark{}, err
	}
	lastReadID = min(lastReadID, newest)
	var mark ReadMark
	var at int64
	if lastReadID == 0 {
		// Nothing to move to (a mark of 0, or nothing visible yet): report
		// the stored mark and write no row.
		err = s.db.QueryRowContext(ctx,
			`SELECT channel_id, last_read_id, updated_at FROM read_marks WHERE principal_id = ? AND channel_id = ?`,
			reader.PrincipalID, channelID).Scan(&mark.ChannelID, &mark.LastReadID, &at)
		if errors.Is(err, sql.ErrNoRows) {
			return ReadMark{ChannelID: channelID}, nil
		}
		if err != nil {
			return ReadMark{}, fmt.Errorf("store: read mark of channel %d for principal %d: %w", channelID, reader.PrincipalID, err)
		}
		mark.UpdatedAt = time.UnixMilli(at)
		return mark, nil
	}
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO read_marks (principal_id, channel_id, last_read_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (principal_id, channel_id) DO UPDATE SET
		     updated_at   = CASE WHEN excluded.last_read_id > read_marks.last_read_id
		                         THEN excluded.updated_at ELSE read_marks.updated_at END,
		     last_read_id = MAX(read_marks.last_read_id, excluded.last_read_id)
		 RETURNING channel_id, last_read_id, updated_at`,
		reader.PrincipalID, channelID, lastReadID, now.UnixMilli()).Scan(&mark.ChannelID, &mark.LastReadID, &at)
	if err != nil {
		return ReadMark{}, fmt.Errorf("store: mark channel %d read for principal %d: %w", channelID, reader.PrincipalID, err)
	}
	mark.UpdatedAt = time.UnixMilli(at)
	return mark, nil
}
