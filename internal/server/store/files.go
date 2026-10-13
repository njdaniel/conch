package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Files (issue #23). This file holds the rows of uploaded files. The question
// "may this reader fetch this file" joins the messages table, so it is asked in
// audience.go (FileVisibleTo) with the same visibility constants as the list
// and the search; nothing here reads a message.

const (
	// AuditFileUploaded is written, in the transaction that makes the file row,
	// for every upload. Its subject is "file:<id>". The detail names the
	// channel, the size and the hash; it never contains the filename, which
	// is whatever the uploader typed.
	AuditFileUploaded = "file_uploaded"
	// AuditFileAttached is written, in the transaction that inserts the
	// message, for each file the message attaches. Its subject is "file:<id>"
	// and the detail names the message.
	AuditFileAttached = "file_attached"
)

// ErrFileAttachment is returned by the message inserts when a message's
// conch.file_attachment.v1 payload cannot be honoured: it is malformed, or a
// file it names does not exist, is in another channel, was uploaded by someone
// else, or is already attached. These are one error on purpose: the poster
// learns nothing about a file that is not theirs. The post stores nothing.
var ErrFileAttachment = errors.New("store: invalid file attachment")

// File is an uploaded file's metadata. The bytes are not in the database.
type File struct {
	ID         int64
	ChannelID  int64
	UploaderID int64
	Filename   string
	MediaType  string
	Size       int64
	SHA256     string
	CreatedAt  time.Time
}

// NewFile is a file to record. The caller has already written the bytes to
// disk and computed Size and SHA256 from them.
type NewFile struct {
	ChannelID  int64
	UploaderID int64
	Filename   string
	MediaType  string
	Size       int64
	SHA256     string
}

// fileColumns is the column list scanFile reads.
const fileColumns = `f.id, f.channel_id, f.uploader_id, f.filename, f.media_type, f.size, f.sha256, f.created_at`

func scanFile(row interface{ Scan(...any) error }) (File, error) {
	var f File
	var createdAt int64
	if err := row.Scan(&f.ID, &f.ChannelID, &f.UploaderID, &f.Filename, &f.MediaType, &f.Size, &f.SHA256, &createdAt); err != nil {
		return File{}, err
	}
	f.CreatedAt = time.UnixMilli(createdAt)
	return f, nil
}

// InsertFile records an uploaded file and its file_uploaded audit event in one
// transaction. The uploader must be a current member of the channel
// (ErrNotChannelMember otherwise): the handler checked at the start of the
// upload, and the upload can take a while.
func (s *Store) InsertFile(ctx context.Context, n NewFile) (File, error) {
	now := time.Now().Truncate(time.Millisecond)
	var id int64
	err := s.withImmediateTx(ctx, func(tx execer) error {
		member, err := isChannelMemberTx(ctx, tx, n.ChannelID, n.UploaderID)
		if err != nil {
			return err
		}
		if !member {
			return ErrNotChannelMember
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO files (channel_id, uploader_id, filename, media_type, size, sha256, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			n.ChannelID, n.UploaderID, n.Filename, n.MediaType, n.Size, n.SHA256, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: insert file in channel %d: %w", n.ChannelID, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: insert file in channel %d: %w", n.ChannelID, err)
		}
		return appendAuditEventTx(ctx, tx, principalActor(n.UploaderID), AuditFileUploaded, fileSubject(id),
			fmt.Sprintf("channel=%d size=%d sha256=%s", n.ChannelID, n.Size, n.SHA256), now)
	})
	if err != nil {
		return File{}, err
	}
	return File{ID: id, ChannelID: n.ChannelID, UploaderID: n.UploaderID, Filename: n.Filename,
		MediaType: n.MediaType, Size: n.Size, SHA256: n.SHA256, CreatedAt: now}, nil
}

// CountFiles returns how many file rows exist, attached or not. It exists for
// tests, which use it to assert that a refused upload recorded nothing.
func (s *Store) CountFiles(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM files").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count files: %w", err)
	}
	return n, nil
}

func fileSubject(id int64) string { return fmt.Sprintf("file:%d", id) }
