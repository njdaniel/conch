package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Local direct messages, the write side (issue #2017, ADR-017, the DM wire
// contract in pkg/schema/dm.go). This file opens conversations, writes,
// edits and removes messages, moves read marks and per-viewer state, and
// records files. It reads no DM row except through dm_visibility.go, which
// owns the visibility rule; a write asks it first (dmViewTx) and asks it for
// the row it changed.
//
// No DM operation writes an audit row. ADR-017 Decision 8: no audit row names
// the counterparty, and the audit log is readable by operators, so the
// pair, the messages and the opening of a conversation are never in it.
// Nothing here reads a clock: the caller passes the time.

var (
	// ErrDMSelf is an open whose target is the caller: a conversation has two
	// different principals (the table's CHECK says so too).
	ErrDMSelf = errors.New("store: a conversation is between two different principals")
	// ErrDMNotHuman is an open whose target is an agent: conversations are
	// between people.
	ErrDMNotHuman = errors.New("store: direct messages are between human principals")
	// ErrDMNoStanding is a write by someone who may not use direct messages at
	// all: not a human, disabled, or a guest.
	ErrDMNoStanding = errors.New("store: the principal may not use direct messages")
	// ErrDMContactUnavailable is a write to a person who cannot be contacted
	// now. It never says why (dmContactTx).
	ErrDMContactUnavailable = errors.New("store: direct message contact unavailable")
	// ErrDMNotAuthor is an edit or removal of a message the editor did not
	// write. There is no moderator removal of a direct message.
	ErrDMNotAuthor = errors.New("store: only the author changes a direct message")
)

// dmAct is what dmContactTx is asked about.
type dmAct int

const (
	// dmActOpen creates a conversation that does not exist yet. Resuming one
	// that exists needs no contact: reading history is for its participants.
	dmActOpen dmAct = iota
	// dmActSend is a send or an edit.
	dmActSend
)

// dmContactTx is the single seam of eligibility: whether from may contact to
// now, for a new conversation or a write. It is called inside the write's
// transaction, after from has been shown to stand and after the conversation
// (if any) has been shown visible to from, so a refusal here leaks nothing
// the caller did not already know.
//
// Today it checks the one thing the tables decide: to is a human, is enabled
// and is not a guest. Blocks (#1832), the recipient's contact preference, the
// operator's and the server's switches and mutual visibility on a
// hide_members server are #2018's, and go here and nowhere else; so does
// anything that must read other tables to decide. The answer is one error for
// every reason, ErrDMContactUnavailable. Act-as credentials are a property of
// a request, not of the store, and are refused by the routes.
func dmContactTx(ctx context.Context, tx execer, from, to int64, act dmAct) error {
	_ = from
	_ = act
	ok, err := dmViewerStandsTx(ctx, tx, to)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDMContactUnavailable
	}
	return nil
}

// OpenDM opens or resumes the conversation between caller and peer and
// returns it as caller sees it; created is true when this call made it. One
// IMMEDIATE transaction, so two opens of one pair, from either side, make one
// conversation and return one id, and a failure leaves nothing behind.
//
// The checks, in order, each refusal storing nothing:
//
//  1. caller stands (ErrDMNoStanding);
//  2. peer is not caller (ErrDMSelf), exists (ErrPrincipalNotFound) and is a
//     human (ErrDMNotHuman);
//  3. a pair that has no conversation yet needs dmContactTx
//     (ErrDMContactUnavailable). A pair that has one is resumed whatever the
//     peer's state: history stays readable.
//
// It records that caller has opened the conversation, and clears an archive
// or hide of caller's. It creates nothing the peer can see: the peer's view
// of the conversation exists only once it has a message (dm_visibility.go),
// and opening writes no message, no audit row and nothing the peer's reads
// look at.
func (s *Store) OpenDM(ctx context.Context, caller, peer int64, now time.Time) (conv DMConversation, created bool, err error) {
	err = s.withImmediateTx(ctx, func(tx execer) error {
		var err error
		conv, created, err = openDMTx(ctx, tx, caller, peer, now)
		return err
	})
	if err != nil {
		return DMConversation{}, false, err
	}
	return conv, created, nil
}

func openDMTx(ctx context.Context, tx execer, caller, peer int64, now time.Time) (DMConversation, bool, error) {
	if ok, err := dmViewerStandsTx(ctx, tx, caller); err != nil {
		return DMConversation{}, false, err
	} else if !ok {
		return DMConversation{}, false, ErrDMNoStanding
	}
	if caller == peer {
		return DMConversation{}, false, ErrDMSelf
	}
	var kind string
	switch err := tx.QueryRowContext(ctx, "SELECT kind FROM principals WHERE id = ?", peer).Scan(&kind); {
	case errors.Is(err, sql.ErrNoRows):
		return DMConversation{}, false, ErrPrincipalNotFound
	case err != nil:
		return DMConversation{}, false, fmt.Errorf("store: read principal %d: %w", peer, err)
	}
	if PrincipalKind(kind) != PrincipalHuman {
		return DMConversation{}, false, ErrDMNotHuman
	}
	id, err := dmPairTx(ctx, tx, caller, peer)
	if err != nil {
		return DMConversation{}, false, err
	}
	created := id == 0
	if created {
		if err := dmContactTx(ctx, tx, caller, peer, dmActOpen); err != nil {
			return DMConversation{}, false, err
		}
		// The write lock is held, so no one else inserts this pair between
		// the lookup and here; DO NOTHING and a second lookup would still
		// make a lost race harmless, and UNIQUE (lo, hi) makes a second
		// conversation impossible.
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO dm_conversations (lo, hi, created_at) VALUES (?, ?, ?) ON CONFLICT (lo, hi) DO NOTHING",
			min(caller, peer), max(caller, peer), now.UnixMilli()); err != nil {
			return DMConversation{}, false, fmt.Errorf("store: open conversation of %d and %d: %w", caller, peer, err)
		}
		if id, err = dmPairTx(ctx, tx, caller, peer); err != nil {
			return DMConversation{}, false, err
		}
		if id == 0 {
			return DMConversation{}, false, fmt.Errorf("store: open conversation of %d and %d: no row after insert", caller, peer)
		}
	}
	if err := setDMStateTx(ctx, tx, caller, id, schema.DMStateOpen, now); err != nil {
		return DMConversation{}, false, err
	}
	conv, err := dmConversationTx(ctx, tx, caller, id)
	if err != nil {
		return DMConversation{}, false, err
	}
	return conv, created, nil
}

// setDMStateTx stores viewer's own state of convID. Only a participant may
// have one (a trigger of migration 51 says so as well).
func setDMStateTx(ctx context.Context, tx execer, viewer, convID int64, state schema.DMState, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO dm_viewer_state (principal_id, conversation_id, state, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (principal_id, conversation_id) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at`,
		viewer, convID, string(state), now.UnixMilli()); err != nil {
		return fmt.Errorf("store: set state of conversation %d for principal %d: %w", convID, viewer, err)
	}
	return nil
}

// SetDMState sets viewer's own archive, hide or reopen of convID and returns
// the conversation as viewer now sees it. It changes nothing for the peer.
// ErrDMNotFound when viewer may not see the conversation. Reading is not
// contact: it needs no eligibility.
func (s *Store) SetDMState(ctx context.Context, viewer, convID int64, state schema.DMState, now time.Time) (DMConversation, error) {
	if !state.Valid() {
		return DMConversation{}, fmt.Errorf("store: set conversation state: %q is not a state", state)
	}
	var out DMConversation
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if _, err := dmViewTx(ctx, tx, viewer, convID); err != nil {
			return err
		}
		if err := setDMStateTx(ctx, tx, viewer, convID, state, now); err != nil {
			return err
		}
		var err error
		out, err = dmConversationTx(ctx, tx, viewer, convID)
		return err
	})
	if err != nil {
		return DMConversation{}, err
	}
	return out, nil
}

// DMPost is a message to send. The caller has checked that Body is neither
// empty nor too long, as for a channel post.
type DMPost struct {
	ConversationID int64
	AuthorID       int64
	Body           string
	// FileIDs are DM files, uploaded by the author to this conversation and
	// not yet attached, in the order to show them.
	FileIDs []int64
	// ClientNonce lets a repeated send be recognised; see InsertDMMessage.
	ClientNonce string
}

// InsertDMMessage stores post as the next message of its conversation and
// returns it as its author reads it; replayed is true when it returned an
// earlier send instead. One IMMEDIATE transaction, the checks in order, each
// refusal storing nothing:
//
//  1. the author may see the conversation (ErrDMNotFound);
//  2. the author may contact the peer now (dmContactTx, ErrDMContactUnavailable);
//  3. an earlier message of this author in this conversation carries the same
//     nonce, within schema.DMClientNonceWindowSeconds: that message is
//     returned, nothing is written (replayed);
//  4. every file is the author's to attach (ErrDMFileNotAttachable);
//  5. the message row, its files' attachment, and, for each participant whose
//     conversation is hidden, the reopening: a message opens a hidden
//     conversation again (an archived one stays archived: it still counts as
//     unread). The peer's view of the conversation begins with this message.
func (s *Store) InsertDMMessage(ctx context.Context, post DMPost, now time.Time) (msg DMMessage, replayed bool, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		row, err := dmViewTx(ctx, tx, post.AuthorID, post.ConversationID)
		if err != nil {
			return err
		}
		if err := dmContactTx(ctx, tx, post.AuthorID, row.peer(post.AuthorID), dmActSend); err != nil {
			return err
		}
		since := now.Add(-schema.DMClientNonceWindowSeconds * time.Second)
		prior, err := dmNonceReplayTx(ctx, tx, post.ConversationID, post.AuthorID, post.ClientNonce, since)
		if err != nil {
			return err
		}
		if prior != 0 {
			replayed = true
			msg, err = dmMessageTx(ctx, tx, post.ConversationID, post.AuthorID, prior)
			return err
		}
		seen := make(map[int64]struct{}, len(post.FileIDs))
		for _, fid := range post.FileIDs {
			if _, dup := seen[fid]; dup {
				return ErrDMFileNotAttachable
			}
			seen[fid] = struct{}{}
			ok, err := dmAttachableTx(ctx, tx, post.ConversationID, post.AuthorID, fid)
			if err != nil {
				return err
			}
			if !ok {
				return ErrDMFileNotAttachable
			}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO direct_messages (conversation_id, author_id, body, created_at, client_nonce) VALUES (?, ?, ?, ?, ?)`,
			post.ConversationID, post.AuthorID, post.Body, now.UnixMilli(), post.ClientNonce)
		if err != nil {
			return fmt.Errorf("store: write message in conversation %d: %w", post.ConversationID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: write message in conversation %d: %w", post.ConversationID, err)
		}
		for i, fid := range post.FileIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE dm_files SET position = ?, message_id = ? WHERE id = ?`, i, id, fid); err != nil {
				return fmt.Errorf("store: attach file %d to message %d: %w", fid, id, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE dm_viewer_state SET state = 'open', updated_at = ? WHERE conversation_id = ? AND state = 'hidden'`,
			now.UnixMilli(), post.ConversationID); err != nil {
			return fmt.Errorf("store: reopen conversation %d: %w", post.ConversationID, err)
		}
		msg, err = dmMessageTx(ctx, tx, post.ConversationID, post.AuthorID, id)
		return err
	})
	if err != nil {
		return DMMessage{}, false, err
	}
	return msg, replayed, nil
}

// DMMessageEdit is EditDMMessage's answer.
type DMMessageEdit struct {
	// Message is the message as its author now reads it.
	Message DMMessage
	// Changed is false when the new text equals the current one: nothing was
	// written.
	Changed bool
	// Revision is the revision the edit created; 0 when nothing changed.
	Revision int
}

// EditDMMessage replaces the text of message messageID of convID, which
// editor must have written, with body, as EditOwnMessage does for a channel
// message, in one IMMEDIATE transaction and in this order, each refusal
// storing nothing:
//
//  1. editor may see the conversation (ErrDMNotFound);
//  2. the message is in it (ErrMessageNotFound), and editor wrote it
//     (ErrDMNotAuthor);
//  3. it is not removed (ErrMessageNotEditable);
//  4. editor may contact the peer now (ErrDMContactUnavailable);
//  5. body equal to the current text: nothing is written;
//  6. within schema.MessageEditWindowSeconds of posting, by clock read once
//     the write lock is held (ErrMessageEditWindowClosed);
//  7. fewer than schema.MaxMessageEdits edits so far
//     (ErrMessageEditLimitReached);
//  8. the write: revision 1 (the text as posted) on the first edit, the new
//     revision, and the row, with the search index following by trigger.
//
// edited_at is strictly after created_at and strictly increasing, whatever
// the clock does. No audit row is written. The emptiness and size of body
// are the caller's checks.
func (s *Store) EditDMMessage(ctx context.Context, editor, convID, messageID int64, body string, clock func() time.Time) (DMMessageEdit, error) {
	var out DMMessageEdit
	err := s.withImmediateTx(ctx, func(tx execer) error {
		row, err := dmViewTx(ctx, tx, editor, convID)
		if err != nil {
			return err
		}
		author, removed, cur, created, edited, revisions, err := dmEditStateTx(ctx, tx, convID, messageID)
		if err != nil {
			return err
		}
		if author != editor {
			return ErrDMNotAuthor
		}
		if removed {
			return ErrMessageNotEditable
		}
		if err := dmContactTx(ctx, tx, editor, row.peer(editor), dmActSend); err != nil {
			return err
		}
		if body == cur {
			msg, err := dmMessageTx(ctx, tx, convID, editor, messageID)
			out = DMMessageEdit{Message: msg}
			return err
		}
		now := clock().Truncate(time.Millisecond)
		if now.UnixMilli()-created.UnixMilli() > schema.MessageEditWindowSeconds*1000 {
			return ErrMessageEditWindowClosed
		}
		if revisions > 0 && revisions-1 >= schema.MaxMessageEdits {
			return ErrMessageEditLimitReached
		}
		if revisions == 0 {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO direct_message_revisions (message_id, revision, body, created_at) VALUES (?, 1, ?, ?)",
				messageID, cur, created.UnixMilli()); err != nil {
				return fmt.Errorf("store: keep the text of message %d as posted: %w", messageID, err)
			}
			revisions = 1
		}
		floor := created
		if !edited.IsZero() {
			floor = edited
		}
		at := now
		if !at.After(floor) {
			at = floor.Add(time.Millisecond)
		}
		revision := revisions + 1
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO direct_message_revisions (message_id, revision, body, created_at) VALUES (?, ?, ?, ?)",
			messageID, revision, body, at.UnixMilli()); err != nil {
			return fmt.Errorf("store: keep revision %d of message %d: %w", revision, messageID, err)
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE direct_messages SET body = ?, edited_at = ? WHERE id = ? AND conversation_id = ? AND author_id = ?",
			body, at.UnixMilli(), messageID, convID, editor)
		if err != nil {
			return fmt.Errorf("store: edit message %d: %w", messageID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: edit message %d: %w", messageID, err)
		}
		if n != 1 {
			// Found in this transaction a moment ago: anything else is a
			// fault, and rolls the edit back.
			return fmt.Errorf("store: edit message %d: changed %d rows", messageID, n)
		}
		msg, err := dmMessageTx(ctx, tx, convID, editor, messageID)
		out = DMMessageEdit{Message: msg, Changed: true, Revision: revision}
		return err
	})
	if err != nil {
		return DMMessageEdit{}, err
	}
	return out, nil
}

// DMMessageRemoval is RemoveDMMessage's answer.
type DMMessageRemoval struct {
	// Message is the tombstone, as the remover reads it.
	Message DMMessage
	// Changed is false when the message was already removed.
	Changed bool
}

// RemoveDMMessage removes message messageID of convID, which remover must
// have written, for both participants: the row stays and reads as a
// tombstone (author's removal body, no files, no edit time), its words leave
// the search index, and its files stop being readable. One IMMEDIATE
// transaction: the conversation must be visible to remover (ErrDMNotFound),
// the message in it (ErrMessageNotFound) and remover's own (ErrDMNotAuthor);
// a message already removed writes nothing. Removing is allowed whatever the
// peer's state or the contact rules: it reduces what is kept. No audit row.
func (s *Store) RemoveDMMessage(ctx context.Context, remover, convID, messageID int64, now time.Time) (DMMessageRemoval, error) {
	var out DMMessageRemoval
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if _, err := dmViewTx(ctx, tx, remover, convID); err != nil {
			return err
		}
		author, removed, err := dmMessageAuthorTx(ctx, tx, convID, messageID)
		if err != nil {
			return err
		}
		if author != remover {
			return ErrDMNotAuthor
		}
		if !removed {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO direct_message_removals (message_id, conversation_id, removed_by, removed_at) VALUES (?, ?, ?, ?)",
				messageID, convID, remover, now.Truncate(time.Millisecond).UnixMilli()); err != nil {
				return fmt.Errorf("store: remove message %d: %w", messageID, err)
			}
		}
		msg, err := dmMessageTx(ctx, tx, convID, remover, messageID)
		out = DMMessageRemoval{Message: msg, Changed: !removed}
		return err
	})
	if err != nil {
		return DMMessageRemoval{}, err
	}
	return out, nil
}

// MarkDMRead moves reader's mark in convID forward to lastReadID and returns
// the mark as stored, with what is unread after it. As MarkRead does for a
// channel:
//
//   - a lastReadID below the stored mark leaves it alone (the larger id wins,
//     in the statement itself, so two clients racing cannot move it back);
//   - a lastReadID above the newest message of the conversation is taken as
//     that message;
//   - updated_at changes only when the mark does.
//
// ErrDMNotFound when reader may not see the conversation. Moving a mark is
// not contact and needs no eligibility; it is the reader's own and the peer
// is never told.
func (s *Store) MarkDMRead(ctx context.Context, reader, convID, lastReadID int64, now time.Time) (DMReadMark, error) {
	if lastReadID < 0 {
		return DMReadMark{}, errors.New("store: mark direct messages read: a mark is never negative")
	}
	var out DMReadMark
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if _, err := dmViewTx(ctx, tx, reader, convID); err != nil {
			return err
		}
		newest, err := dmNewestMessageIDTx(ctx, tx, convID)
		if err != nil {
			return err
		}
		lastReadID = min(lastReadID, newest)
		if lastReadID > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dm_read_marks (principal_id, conversation_id, last_read_id, updated_at)
				VALUES (?, ?, ?, ?)
				ON CONFLICT (principal_id, conversation_id) DO UPDATE SET
				    updated_at   = CASE WHEN excluded.last_read_id > dm_read_marks.last_read_id
				                        THEN excluded.updated_at ELSE dm_read_marks.updated_at END,
				    last_read_id = MAX(dm_read_marks.last_read_id, excluded.last_read_id)`,
				reader, convID, lastReadID, now.UnixMilli()); err != nil {
				return fmt.Errorf("store: mark conversation %d read for principal %d: %w", convID, reader, err)
			}
		}
		out, err = dmReadMarkTx(ctx, tx, convID, reader)
		return err
	})
	if err != nil {
		return DMReadMark{}, err
	}
	return out, nil
}

// NewDMFile is a file to record in a conversation. The caller has already
// written the bytes to the blob store and computed Size and SHA256.
type NewDMFile struct {
	ConversationID int64
	UploaderID     int64
	Filename       string
	MediaType      string
	Size           int64
	SHA256         string
}

// AddDMFile records an uploaded file of a conversation, attached to nothing
// yet: the uploader must see the conversation (ErrDMNotFound). It becomes
// part of a message when the uploader sends it (DMPost.FileIDs), once, and
// until then only the uploader can read it. Both participants may upload; a
// file is never shared across conversations or with channels, and has its own
// id space.
func (s *Store) AddDMFile(ctx context.Context, f NewDMFile, now time.Time) (DMFile, error) {
	var out DMFile
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if _, err := dmViewTx(ctx, tx, f.UploaderID, f.ConversationID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO dm_files (conversation_id, uploader_id, filename, media_type, size, sha256, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			f.ConversationID, f.UploaderID, f.Filename, f.MediaType, f.Size, f.SHA256, now.Truncate(time.Millisecond).UnixMilli())
		if err != nil {
			return fmt.Errorf("store: record file in conversation %d: %w", f.ConversationID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: record file in conversation %d: %w", f.ConversationID, err)
		}
		out, err = dmFileTx(ctx, tx, f.UploaderID, id)
		return err
	})
	if err != nil {
		return DMFile{}, err
	}
	return out, nil
}
