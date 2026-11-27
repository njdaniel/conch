package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
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
	// ErrDMEmptyBody is a send or an edit whose body is empty or only white
	// space (strings.TrimSpace). It is checked after the conversation is
	// shown to the author and before contact, the order of schema's header.
	ErrDMEmptyBody = errors.New("store: a direct message body is empty or only white space")
	// ErrDMNonceConflict is a retry within the nonce window with different send inputs.
	ErrDMNonceConflict = errors.New("store: direct message client nonce already used for a different message")
)

// dmAct is what dmContactTx (dm_policy.go) is asked about. Every act has the
// same rules; it names the caller's purpose.
type dmAct int

const (
	// dmActOpen introduces a conversation to the caller. Resuming one already
	// visible to the caller needs no contact.
	dmActOpen dmAct = iota
	// dmActSend is a send or an edit.
	dmActSend
	// dmActCall is a private call invite (ADR-017 Decision 3).
	dmActCall
)

// OpenDM opens or resumes the conversation between caller and peer and
// returns the caller's view. An IMMEDIATE transaction ensures simultaneous
// opens of the same pair return one id and a failed open leaves no changes.
//
// The checks, in the refusal order of schema/dm.go (design §3.1), each
// refusal storing nothing:
//
//  1. caller stands (ErrDMNoStanding);
//  2. unless the caller can already see a conversation with peer (a resume,
//     which is exempt), the server's switch and the caller's own are on
//     (dmWriteGateTx, ErrDMsOff). It is asked before peer is looked up, so
//     it says nothing about peer;
//  3. peer is not caller (ErrDMSelf);
//  4. peer is visible to the caller: it exists, is not hidden, has not ended
//     a guest visit, and the directory's rule lets the caller know it
//     (knownPrincipalsSQL). Any of these failing is ErrPrincipalNotFound,
//     the very error of an id never issued, from one statement, so a
//     hidden principal and a nonexistent one cannot be told apart. It holds
//     for a resume too: a peer the caller may no longer know is not found
//     by id, though the conversation still reads by its own id. For a new
//     pair the rule is PutBlock's (listedPrincipalSQL), which also excludes
//     disabled principals and active guests the caller neither shares a
//     channel with nor invited;
//  5. peer is a human (ErrDMNotHuman);
//  6. an open of a conversation the caller cannot already see needs
//     dmContactTx (ErrDMContactUnavailable).
//
// Opening records the caller's view, reopens it if hidden, and preserves an
// archive. The peer's visibility stays governed by dmVisibleCond.
func (s *Store) OpenDM(ctx context.Context, caller, peer int64, now time.Time) (DMConversation, error) {
	return s.openDM(ctx, caller, peer, nil, func() time.Time { return now }, nil)
}

// OpenDMFromCredential checks the request's credential under the write lock.
func (s *Store) OpenDMFromCredential(ctx context.Context, caller, credential, peer int64, clock func() time.Time, limitNew func(time.Time) error) (DMConversation, error) {
	return s.openDM(ctx, caller, peer, &credential, clock, limitNew)
}

func (s *Store) openDM(ctx context.Context, caller, peer int64, credential *int64, clock func() time.Time, limitNew func(time.Time) error) (DMConversation, error) {
	return s.withDMWrite(ctx, caller, credential, clock, func(tx execer, now time.Time) (DMConversation, error) {
		return openDMTx(ctx, tx, caller, peer, now, limitNew)
	})
}

func (s *Store) withDMWrite(ctx context.Context, caller int64, credential *int64, clock func() time.Time, write func(execer, time.Time) (DMConversation, error)) (DMConversation, error) {
	var out DMConversation
	err := s.withDMCredentialWrite(ctx, caller, credential, clock, func(tx execer, now time.Time) error {
		var err error
		out, err = write(tx, now)
		return err
	})
	if errors.Is(err, ErrCredentialInvalid) {
		return DMConversation{}, ErrDMNoStanding
	}
	if err != nil {
		return DMConversation{}, err
	}
	return out, nil
}

// withDMCredentialWrite rechecks the request credential under the write lock,
// so revocation cannot race a DM write. A nil credential is for store callers
// that do not act on behalf of an authenticated request.
func (s *Store) withDMCredentialWrite(ctx context.Context, caller int64, credential *int64, clock func() time.Time, write func(execer, time.Time) error) error {
	return s.withImmediateTx(ctx, func(tx execer) error {
		now := clock()
		if credential != nil {
			actor := PrivateCallActor{PrincipalID: caller, CredentialID: *credential}
			live, err := privateCallRequestLive(ctx, tx, actor, now)
			if err != nil {
				return err
			}
			if !live {
				return ErrCredentialInvalid
			}
		}
		return write(tx, now)
	})
}

func openDMTx(ctx context.Context, tx execer, caller, peer int64, now time.Time, limitNew func(time.Time) error) (DMConversation, error) {
	if ok, err := dmViewerStandsTx(ctx, tx, caller); err != nil {
		return DMConversation{}, err
	} else if !ok {
		return DMConversation{}, ErrDMNoStanding
	}
	// A pair the caller can already see: only the caller's own view decides
	// whether the write gate applies, so asking tells them nothing new.
	id, err := dmPairTx(ctx, tx, caller, peer, true)
	if err != nil {
		return DMConversation{}, err
	}
	if id == 0 {
		if err := dmWriteGateTx(ctx, tx, caller); err != nil {
			return DMConversation{}, err
		}
		if limitNew != nil {
			if err := limitNew(now); err != nil {
				return DMConversation{}, err
			}
		}
	}
	if caller == peer {
		return DMConversation{}, ErrDMSelf
	}
	seeAll, err := permittedTx(ctx, tx, caller, schema.PermSeeMemberDirectory)
	if err != nil {
		return DMConversation{}, err
	}
	known, args, _ := knownPrincipalsSQL(caller, seeAll)
	cond := dmLivePrincipalCond + " AND (" + known + ")"
	if id == 0 {
		// A pair the caller cannot see answers probes as PutBlock does.
		cond, args = listedPrincipalSQL(caller, seeAll)
	}
	var kind string
	switch err := tx.QueryRowContext(ctx, `SELECT p.kind FROM principals p WHERE p.id = ? AND `+cond, // #nosec G202 -- every fragment is a constant
		append([]any{peer}, args...)...).Scan(&kind); {
	case errors.Is(err, sql.ErrNoRows):
		return DMConversation{}, ErrPrincipalNotFound
	case err != nil:
		return DMConversation{}, fmt.Errorf("store: read principal %d: %w", peer, err)
	}
	if PrincipalKind(kind) != PrincipalHuman {
		return DMConversation{}, ErrDMNotHuman
	}
	if id == 0 {
		if err := dmContactTx(ctx, tx, caller, peer, dmActOpen); err != nil {
			return DMConversation{}, err
		}
		// Consult a prior silent open only after the caller passes contact checks.
		if id, err = dmPairTx(ctx, tx, caller, peer, false); err != nil {
			return DMConversation{}, err
		}
		if id == 0 {
			id, err = insertDMPairTx(ctx, tx, caller, peer, now)
			if err != nil {
				return DMConversation{}, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO dm_viewer_state (principal_id, conversation_id, state, updated_at)
		VALUES (?, ?, 'open', ?)
		ON CONFLICT (principal_id, conversation_id) DO UPDATE SET state = 'open', updated_at = excluded.updated_at
		WHERE dm_viewer_state.state = 'hidden'`, caller, id, now.UnixMilli()); err != nil {
		return DMConversation{}, fmt.Errorf("store: open conversation %d for principal %d: %w", id, caller, err)
	}
	return dmConversationTx(ctx, tx, caller, id)
}

// dmMaxConversationID is the largest integer JavaScript represents exactly
// across the entire positive ID range.
const dmMaxConversationID int64 = 1<<53 - 1

// insertDMPairTx assigns an ID independent of when the pair was opened.
// The write lock protects pair uniqueness between lookup and insert. An ID
// collision with any other pair is retried without handing out that ID.
func insertDMPairTx(ctx context.Context, tx execer, a, b int64, now time.Time) (int64, error) {
	lo, hi := min(a, b), max(a, b)
	for {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, fmt.Errorf("store: generate conversation id: %w", err)
		}
		id := int64(binary.LittleEndian.Uint64(buf[:]) & uint64(dmMaxConversationID))
		if id == 0 {
			continue
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO dm_conversations (id, lo, hi, created_at)
			VALUES (?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, id, lo, hi, now.UnixMilli())
		if err != nil {
			return 0, fmt.Errorf("store: insert conversation of %d and %d: %w", lo, hi, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, fmt.Errorf("store: insert conversation of %d and %d: %w", lo, hi, err)
		} else if n == 1 {
			return id, nil
		}
	}
}

// setDMStateTx stores viewer's own state of convID. Only a participant may
// have one (a trigger of migration 53 says so as well).
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
	return s.setDMState(ctx, viewer, convID, nil, state, func() time.Time { return now })
}

// SetDMStateFromCredential checks the request's credential under the write lock.
func (s *Store) SetDMStateFromCredential(ctx context.Context, viewer, credential, convID int64, state schema.DMState, clock func() time.Time) (DMConversation, error) {
	return s.setDMState(ctx, viewer, convID, &credential, state, clock)
}

func (s *Store) setDMState(ctx context.Context, viewer, convID int64, credential *int64, state schema.DMState, clock func() time.Time) (DMConversation, error) {
	if !state.Valid() {
		return DMConversation{}, fmt.Errorf("store: set conversation state: %q is not a state", state)
	}
	return s.withDMWrite(ctx, viewer, credential, clock, func(tx execer, now time.Time) (DMConversation, error) {
		if _, err := dmViewTx(ctx, tx, viewer, convID); err != nil {
			return DMConversation{}, err
		}
		if err := setDMStateTx(ctx, tx, viewer, convID, state, now); err != nil {
			return DMConversation{}, err
		}
		return dmConversationTx(ctx, tx, viewer, convID)
	})
}

// DMPost is a message to send. The caller has checked that Body is not too
// long; InsertDMMessage refuses one that is empty or only white space.
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
//  0. the server's switch and the author's own are on (dmWriteGateTx,
//     ErrDMsOff), asked before the conversation is looked up, so a send to
//     one that does not exist is ErrDMsOff too (schema/dm.go's order);
//  1. the author may see the conversation (ErrDMNotFound) and the body is
//     not empty or only white space (ErrDMEmptyBody);
//  2. the author may contact the peer now (dmContactTx, ErrDMContactUnavailable);
//  3. an earlier message of this author in this conversation carries the same
//     nonce, within schema.DMClientNonceWindowSeconds: identical inputs
//     return that message as it now reads (replayed); different inputs
//     return ErrDMNonceConflict. Neither path writes anything;
//  4. every file is the author's to attach (ErrDMFileNotAttachable);
//  5. the message row, its files' attachment, and, for each participant whose
//     conversation is hidden, the reopening: a message opens a hidden
//     conversation again (an archived one stays archived: it still counts as
//     unread). The peer's view of the conversation begins with this message.
func (s *Store) InsertDMMessage(ctx context.Context, post DMPost, now time.Time) (msg DMMessage, replayed bool, err error) {
	return s.insertDMMessage(ctx, post, nil, func() time.Time { return now }, nil, nil)
}

// InsertDMMessageFromCredential sends after rechecking the credential. It
// spends the send budget before the conversation lookup and the new-pair
// budget only for the first message, under the same write lock.
func (s *Store) InsertDMMessageFromCredential(ctx context.Context, post DMPost, credential int64, clock func() time.Time, limitSend, limitNew func(time.Time) error) (DMMessage, bool, error) {
	return s.insertDMMessage(ctx, post, &credential, clock, limitSend, limitNew)
}

func (s *Store) insertDMMessage(ctx context.Context, post DMPost, credential *int64, clock func() time.Time, limitSend, limitNew func(time.Time) error) (msg DMMessage, replayed bool, err error) {
	err = s.withDMCredentialWrite(ctx, post.AuthorID, credential, clock, func(tx execer, now time.Time) error {
		if err := dmWriteGateTx(ctx, tx, post.AuthorID); err != nil {
			return err
		}
		if limitSend != nil {
			if err := limitSend(now); err != nil {
				return err
			}
		}
		now = now.Truncate(time.Millisecond)
		row, err := dmViewTx(ctx, tx, post.AuthorID, post.ConversationID)
		if err != nil {
			return err
		}
		if strings.TrimSpace(post.Body) == "" {
			return ErrDMEmptyBody
		}
		if err := dmContactTx(ctx, tx, post.AuthorID, row.peer(post.AuthorID), dmActSend); err != nil {
			return err
		}
		since := now.Add(-schema.DMClientNonceWindowSeconds * time.Second)
		prior, err := dmNonceReplayTx(ctx, tx, post, since)
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
		rev, err := dmNextRevisionTx(ctx, tx, post.ConversationID)
		if err != nil {
			return err
		}
		if rev == 1 && limitNew != nil {
			if err := limitNew(now); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO direct_messages (conversation_id, author_id, body, created_at, client_nonce, revision) VALUES (?, ?, ?, ?, ?, ?)`,
			post.ConversationID, post.AuthorID, post.Body, now.UnixMilli(), post.ClientNonce, rev)
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
//  0. the server's switch and the editor's own are on (dmWriteGateTx,
//     ErrDMsOff), asked before the conversation is looked up;
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
// the clock does. No audit row is written. Its size is the caller's check; an
// empty or whitespace-only body is ErrDMEmptyBody.
func (s *Store) EditDMMessage(ctx context.Context, editor, convID, messageID int64, body string, clock func() time.Time) (DMMessageEdit, error) {
	return s.editDMMessage(ctx, editor, convID, messageID, body, nil, clock, nil)
}

// EditDMMessageFromCredential edits after rechecking the credential and
// spends the edit budget before looking up the conversation.
func (s *Store) EditDMMessageFromCredential(ctx context.Context, editor, credential, convID, messageID int64, body string, clock func() time.Time, limitEdit func(time.Time) error) (DMMessageEdit, error) {
	return s.editDMMessage(ctx, editor, convID, messageID, body, &credential, clock, limitEdit)
}

func (s *Store) editDMMessage(ctx context.Context, editor, convID, messageID int64, body string, credential *int64, clock func() time.Time, limitEdit func(time.Time) error) (DMMessageEdit, error) {
	var out DMMessageEdit
	err := s.withDMCredentialWrite(ctx, editor, credential, clock, func(tx execer, now time.Time) error {
		if err := dmWriteGateTx(ctx, tx, editor); err != nil {
			return err
		}
		if limitEdit != nil {
			if err := limitEdit(now); err != nil {
				return err
			}
		}
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
		if strings.TrimSpace(body) == "" {
			return ErrDMEmptyBody
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
		now = now.Truncate(time.Millisecond)
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
		rev, err := dmNextRevisionTx(ctx, tx, convID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE direct_messages SET body = ?, edited_at = ?, revision = ? WHERE id = ? AND conversation_id = ? AND author_id = ?",
			body, at.UnixMilli(), rev, messageID, convID, editor)
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
	return s.removeDMMessage(ctx, remover, convID, messageID, nil, func() time.Time { return now })
}

// RemoveDMMessageFromCredential rechecks the credential before author removal.
func (s *Store) RemoveDMMessageFromCredential(ctx context.Context, remover, credential, convID, messageID int64, clock func() time.Time) (DMMessageRemoval, error) {
	return s.removeDMMessage(ctx, remover, convID, messageID, &credential, clock)
}

func (s *Store) removeDMMessage(ctx context.Context, remover, convID, messageID int64, credential *int64, clock func() time.Time) (DMMessageRemoval, error) {
	var out DMMessageRemoval
	err := s.withDMCredentialWrite(ctx, remover, credential, clock, func(tx execer, now time.Time) error {
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
			rev, err := dmNextRevisionTx(ctx, tx, convID)
			if err != nil {
				return err
			}
			// The revision moves first: a tombstone freezes the row.
			if _, err := tx.ExecContext(ctx,
				"UPDATE direct_messages SET revision = ? WHERE id = ?", rev, messageID); err != nil {
				return fmt.Errorf("store: remove message %d: %w", messageID, err)
			}
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
