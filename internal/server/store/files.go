package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
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

// ErrReservedPayload is returned by the message inserts when a message's
// payload schema is one only the server may author (schema.ReservedMessagePayload).
// The check sits in the insert, so no post route can forget it. Nothing is stored.
var ErrReservedPayload = errors.New("store: reserved payload schema")

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
		var err error
		id, err = insertFileTx(ctx, tx, n, now)
		return err
	})
	if err != nil {
		return File{}, err
	}
	return File{ID: id, ChannelID: n.ChannelID, UploaderID: n.UploaderID, Filename: n.Filename,
		MediaType: n.MediaType, Size: n.Size, SHA256: n.SHA256, CreatedAt: now}, nil
}

// insertFileTx is InsertFile in the caller's transaction: the membership
// check, the archived guard, the row and its file_uploaded audit event. Every
// file row is made here, so an upload a message brings with it (MessageUploads)
// is recorded and audited exactly as one sent to the upload route is.
func insertFileTx(ctx context.Context, tx execer, n NewFile, now time.Time) (int64, error) {
	member, err := isChannelMemberTx(ctx, tx, n.ChannelID, n.UploaderID)
	if err != nil {
		return 0, err
	}
	if !member {
		return 0, ErrNotChannelMember
	}
	// The archived guard of an upload (issue #844).
	if err := channelNotArchivedTx(ctx, tx, n.ChannelID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO files (channel_id, uploader_id, filename, media_type, size, sha256, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		n.ChannelID, n.UploaderID, n.Filename, n.MediaType, n.Size, n.SHA256, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: insert file in channel %d: %w", n.ChannelID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert file in channel %d: %w", n.ChannelID, err)
	}
	return id, appendAuditEventTx(ctx, tx, principalActor(n.UploaderID), AuditFileUploaded, fileSubject(id),
		fmt.Sprintf("channel=%d size=%d sha256=%s", n.ChannelID, n.Size, n.SHA256), now)
}

// MessageUploads are files a message brings with it (issue #1798): an agent's
// post_message with attachments. They are recorded in the transaction that
// inserts the message, which attaches them, so the files and the message are
// stored together or not at all: a refused post leaves no file row, and a
// file row is never left without its message.
//
// A post that carries uploads carries no payload of its own. The store writes
// the conch.file_attachment.v1 payload that names the new rows, in the order
// of Files.
type MessageUploads struct {
	// Files are the files to record, one to schema.MaxFileAttachments of
	// them. The channel and the uploader are the post's channel and author,
	// whatever these say.
	Files []NewFile
	// Place puts the bytes where the rows say they are. It is called once,
	// last, inside the transaction: after every row is written and before
	// the commit, so that a row never points at nothing (the order the
	// upload route keeps) and a refused post has placed nothing. If it fails
	// the transaction is rolled back.
	Place func() error
}

// errUploadsWithPayload is a post that carries both uploads and a payload:
// the caller's mistake, never a request's (the tool refuses that pair).
var errUploadsWithPayload = errors.New("store: a post with uploads carries no payload of its own")

// recordUploadsTx records the files of u as authorID's uploads to channelID
// and returns the payload that attaches them, for the message insert that
// follows in the same transaction. With no uploads it returns payload as it
// was given.
func recordUploadsTx(
	ctx context.Context, tx execer, channelID, authorID int64, u *MessageUploads, payload *schema.Payload, now time.Time,
) (*schema.Payload, error) {
	if u == nil {
		return payload, nil
	}
	if payload != nil {
		return nil, errUploadsWithPayload
	}
	att := schema.FileAttachmentV1{FileIDs: make([]int64, 0, len(u.Files))}
	for _, n := range u.Files {
		n.ChannelID, n.UploaderID = channelID, authorID
		id, err := insertFileTx(ctx, tx, n, now)
		if err != nil {
			return nil, err
		}
		att.FileIDs = append(att.FileIDs, id)
	}
	if err := att.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFileAttachment, err)
	}
	data, err := json.Marshal(att)
	if err != nil {
		return nil, fmt.Errorf("store: encode file attachment: %w", err)
	}
	return &schema.Payload{Schema: schema.FileAttachmentV1Name, Data: data}, nil
}

// place runs u's Place, if there is one, as the last step of the transaction.
func (u *MessageUploads) place() error {
	if u == nil || u.Place == nil {
		return nil
	}
	return u.Place()
}

// InsertMessageWithUploads is InsertMessageV1 for a channel-wide message that
// brings its files with it: the file rows, the message that attaches them and
// every audit event are one transaction (see MessageUploads).
func (s *Store) InsertMessageWithUploads(ctx context.Context, channelID, authorID int64, body string, uploads *MessageUploads) (Message, error) {
	now := time.Now().Truncate(time.Millisecond)
	var id int64
	var payload *schema.Payload
	var guest *schema.GuestAttributionV1
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var err error
		if payload, err = recordUploadsTx(ctx, tx, channelID, authorID, uploads, nil, now); err != nil {
			return err
		}
		if id, err = insertChannelWideTx(ctx, tx, channelID, authorID, body, payload, 0, now); err != nil {
			return err
		}
		if guest, err = guestAuthorTx(ctx, tx, id); err != nil {
			return err
		}
		return uploads.place()
	})
	if err != nil {
		return Message{}, err
	}
	return Message{ID: id, ChannelID: channelID, AuthorID: authorID, Body: body, Payload: payload, CreatedAt: now, GuestAuthor: guest}, nil
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
