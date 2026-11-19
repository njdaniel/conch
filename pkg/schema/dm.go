package schema

import (
	"errors"
	"fmt"
	"strings"
)

// Local direct messages (issue #2016, child of #2013; the 1:1 section of
// #1501). DRAFT: these shapes follow ADR-017 (#2038, still a proposal, PR
// #2044) and its design note docs/design/direct-messages.md, which carry
// Nick's decisions of 2026-11-19 on #2013; nothing serves them yet, and the
// ADR may still change any of them before a server does.
//
// A DM conversation is a durable, private exchange between exactly two local
// human principals on one server. There is one per pair: the pair is stored
// in canonical order (the smaller principal id first), so opening a
// conversation with someone is idempotent from either side and resumes the
// one that exists. A conversation is not a channel. It has no members list,
// no roles, no nets, no whispers, no threads and no payloads, and it is
// named by its own id, conversation_id, in an id space of its own: a DM
// document never carries channel_id, and a channel document never carries
// conversation_id. A client that holds both tells them apart by the field
// name and by the document's schema name, never by the number.
//
// Routes (REST; every one human-only: an agent gets the existing route-class
// refusal, 403 forbidden, and an act-as credential gets
// ErrorCodeOwnSignInRequired, ADR-017 Decision 7):
//
//	POST /v1/dms                               open or resume (OpenDMRequestV1 → OpenDMResponseV1)
//	GET  /v1/dms?state=&cursor=&limit=         list (ListDMConversationsResponseV1)
//	GET  /v1/dms/snapshot                      reconnect snapshot (DMSnapshotV1)
//	GET  /v1/dms/{conversation}                one conversation (DMConversationV1)
//	GET  /v1/dms/{conversation}/messages       history (ListDMMessagesResponseV1)
//	POST /v1/dms/{conversation}/messages       send (PostDMMessageRequestV1 → PostDMMessageResponseV1)
//	PATCH /v1/dms/{conversation}/messages/{message}  edit your own (EditDMMessageRequestV1 → EditDMMessageResponseV1)
//	DELETE /v1/dms/{conversation}/messages/{message} remove your own (204, no body; a repeat is 204 too)
//	PUT  /v1/dms/{conversation}/read-state     read mark (MarkReadRequestV1 → DMReadStateV1)
//	PUT  /v1/dms/{conversation}/state          archive, hide, reopen (SetDMStateRequestV1 → DMConversationV1)
//	GET  /v1/dms/{conversation}/search?q=      search one conversation (SearchDMMessagesResponseV1)
//	POST /v1/dms/{conversation}/files          upload (→ DMFileV1)
//	GET  /v1/dms/{conversation}/files/{file}   metadata (DMFileV1); /content for the bytes
//	GET, PUT /v1/dms/preferences               contact preferences (DMPreferencesV1)
//	GET, PUT /v1/dms/settings                  the server-wide switch (DMServerSettingsV1)
//	PUT  /v1/principals/{id}/dm-permission     one person's switch (DMPermissionRequestV1 → DMPermissionResponseV1)
//
// Events: the per-person events stream (#2040) carries DMMessageV1 (a new
// message, to both participants' sockets, the author's other devices
// included), DMMessageEditedV1 and DMMessageRemovedV1 (an edit and a removal
// of one already sent) and DMChangedV1 (a signal to re-read one
// conversation). On connect, and on every reconnect, the stream's first DM
// frame is a DMSnapshotV1; see its comment for the ordering promise.
//
// Not in v1 (ADR-017 Decision 5): reactions, threads, mentions, audiences and
// typed payloads. A DM carries no payload field at all, so an approval or
// admin-action card cannot be posted into one, and the approval path is not
// touched.
//
// Blocking is not here. A block is the general personal block relation of
// #1832, which DMs consult; the DM contract has no block list of its own,
// and a block of either direction is one of the reasons for
// ErrorCodeContactUnavailable.
//
// Refusals, in the order the server checks them (design §3.1), each a rule's
// name, never the input:
//
//   - 404 ErrorCodeDMNotFound: no such conversation, or the caller is not one
//     of its pair, whatever the caller's role. One answer, one body.
//   - 404 principal_not_found (existing): on open, the target does not exist,
//     has ended, or is hidden from the caller. One body for all three.
//   - 400 invalid_request (existing): the target is the caller, or an agent;
//     a malformed request or cursor; a body that tries a payload, audience or
//     thread (the request types here have no such fields and are decoded with
//     unknown fields refused).
//   - 403 ErrorCodeContactUnavailable: the target is visible but cannot be
//     contacted now (disabled, a block either way, their preference, their
//     switch, a guest, or hide-members visibility). One code, one message;
//     it never says which, and never who blocked whom.
//   - 403 ErrorCodeDMsOff: the caller's own DMs, or the server's, are
//     switched off. It says nothing about anyone else.
//   - 403 ErrorCodeOwnSignInRequired: the caller's credential is an act-as
//     credential; it is refused on every DM route, reads included.
//
// Everything here is additive: new types, constants and error codes, and the
// optional PrincipalAdminV1.DMAllowed. The one change to a published family
// is NotificationSummaryV2 (notification_v2.go), a new version beside v1.

// Schema names of the DM documents that carry one. REST request bodies carry
// none, as elsewhere in this package.
const (
	DMConversationSchemaV1 = "conch.dm_conversation.v1"
	DMMessageSchemaV1      = "conch.dm_message.v1"
	DMReadStateSchemaV1    = "conch.dm_read_state.v1"
	DMFileSchemaV1         = "conch.dm_file.v1"
	DMChangedSchemaV1      = "conch.dm_changed.v1"
	DMSnapshotSchemaV1     = "conch.dm_snapshot.v1"
	// DMMessageEditedSchemaV1 and DMMessageRemovedSchemaV1 name the frames
	// that say a DM message's text changed or that it was removed. They are
	// frames of their own, as the channel's are, because on the stream a
	// DMMessageV1 means "a new message" and a client notifies on it.
	DMMessageEditedSchemaV1  = "conch.dm_message_edited.v1"
	DMMessageRemovedSchemaV1 = "conch.dm_message_removed.v1"
)

// Bounds of the DM routes. The server enforces each; a client reads them to
// stop asking for what would be refused.
const (
	// DefaultDMPageSize is how many messages a history or search page holds
	// when the request gives no limit.
	DefaultDMPageSize = 50
	// MaxDMPageSize is the most messages one history or search page holds; a
	// larger limit is taken as this one.
	MaxDMPageSize = 100
	// MaxDMConversationsPage is the most conversations one list page holds.
	MaxDMConversationsPage = 50
	// MaxDMSnapshotConversations is the most conversations a snapshot holds.
	MaxDMSnapshotConversations = 200
	// MaxDMClientNonceBytes is the longest client nonce a send may carry.
	MaxDMClientNonceBytes = 64
	// DMClientNonceWindowSeconds is how long the server remembers a send's
	// client nonce: at least a day. A repeat inside the window is the same
	// send; after it, a new one.
	DMClientNonceWindowSeconds = 86400
	// MaxDMCursorBytes is the longest list cursor the server issues.
	MaxDMCursorBytes = 128
)

// Error codes of the DM routes, additive on Error. The existing ones are
// not redefined: principal_not_found (404), invalid_request (400, including
// a target that is the caller or an agent, a bad cursor, both before and
// after), forbidden (403, an agent caller), message_not_found (404, a
// message id that is not in this conversation), message_not_editable,
// message_edit_window_closed and message_edit_limit_reached (409, as for
// channel messages).
const (
	// ErrorCodeDMNotFound: no such conversation, or the caller is not one of
	// its two participants (HTTP 404). The two are one answer, whatever the
	// caller's role, operators included.
	ErrorCodeDMNotFound = "dm_not_found"
	// ErrorCodeContactUnavailable: the target is visible to the caller but
	// cannot be contacted (HTTP 403), on open, send or edit: disabled, a
	// block in either direction (#1832), the target's contact preference, the
	// target's per-person switch, a guest, or hide-members visibility. The
	// caller learns only "can't contact", never which, so no refusal
	// announces a block or a preference. Reading and removing one's own
	// messages in an existing conversation is not refused by it.
	ErrorCodeContactUnavailable = "contact_unavailable"
	// ErrorCodeDMsOff: the caller's own direct messages are switched off by
	// an operator, or the server-wide switch is off (HTTP 403), on open, send
	// and edit. It says nothing about anyone but the caller and the server.
	ErrorCodeDMsOff = "dms_off"
	// ErrorCodeOwnSignInRequired: the caller's credential is an act-as
	// credential (HTTP 403, ADR-017 Decision 7), so every DM route refuses
	// it, reads included. It says nothing about anyone but the caller; the
	// person signs in themself.
	ErrorCodeOwnSignInRequired = "own_sign_in_required"
	// ErrorCodeDMNonceConflict: a send repeats a client nonce the author used
	// in this conversation inside DMClientNonceWindowSeconds, with a
	// different body or file_ids (HTTP 409). An identical repeat is not an
	// error; it answers the original message with Replayed set.
	ErrorCodeDMNonceConflict = "dm_nonce_conflict"
	// ErrorCodeDMRequireAuth: conchd runs with authentication off, so there
	// is no principal to hold a conversation (HTTP 400).
	ErrorCodeDMRequireAuth = "dm_require_auth"
	// ErrorCodeDMFileNotAttachable: a send names a file that is not one the
	// author uploaded to this conversation, or one already attached
	// (HTTP 409). A file of another conversation, or of a channel, answers
	// this too and never says which.
	ErrorCodeDMFileNotAttachable = "dm_file_not_attachable"
)

// DMState is where a conversation sits in one participant's list. It is that
// participant's own: archiving or hiding a conversation changes nothing for
// the other. The vocabulary is closed.
type DMState string

const (
	// DMStateOpen: in the default list.
	DMStateOpen DMState = "open"
	// DMStateArchived: out of the default list and in ?state=archived until
	// its participant reopens it. A new message leaves it archived; it still
	// counts as unread and notifies by the person's policy.
	DMStateArchived DMState = "archived"
	// DMStateHidden: out of every list until the next message in it, which
	// opens it again. Its history is kept, and opening the conversation with
	// the same person resumes it.
	DMStateHidden DMState = "hidden"
)

// Valid reports whether s is a recognized state.
func (s DMState) Valid() bool {
	switch s {
	case DMStateOpen, DMStateArchived, DMStateHidden:
		return true
	default:
		return false
	}
}

// DMAcceptPolicy is who may contact a person: it is the recipient's
// preference and applies to every new conversation, send, edit and call
// invite, not only the first (design §3.3). It never hides history already
// said. Closing one person out is the personal block relation (#1832), not a
// preference. The vocabulary is closed.
type DMAcceptPolicy string

const (
	// DMAcceptEveryone: any local human on the server. It is the default.
	DMAcceptEveryone DMAcceptPolicy = "everyone"
	// DMAcceptSharedChannel: a local human who is in at least one channel
	// (public or private) with the person at the moment of the write.
	DMAcceptSharedChannel DMAcceptPolicy = "shared_channel"
	// DMAcceptNobody: no one may contact the person. Existing conversations
	// stay readable; a new message in them is refused like a new one.
	DMAcceptNobody DMAcceptPolicy = "nobody"
)

// Valid reports whether p is a recognized policy.
func (p DMAcceptPolicy) Valid() bool {
	switch p {
	case DMAcceptEveryone, DMAcceptSharedChannel, DMAcceptNobody:
		return true
	default:
		return false
	}
}

// DMChangeKind is what a DMChangedV1 says may have changed. A client re-reads
// the conversation whatever the kind, so the kind is a hint and a client
// tolerates one it does not know.
type DMChangeKind string

const (
	// DMChangeOpened: the conversation is new to this reader's list: they
	// opened it on another device.
	DMChangeOpened DMChangeKind = "opened"
	// DMChangeRead: the reader's own read mark moved, on another device.
	DMChangeRead DMChangeKind = "read"
	// DMChangeState: the reader's own DMState changed, on another device,
	// or a hidden conversation opened again.
	DMChangeState DMChangeKind = "state"
	// DMChangeParticipant: what the conversation shows of the peer changed:
	// their name, or their account was disabled or enabled. Never sent for
	// a change to the peer's contact preferences, a block, or an operator's
	// switch.
	DMChangeParticipant DMChangeKind = "participant"
)

// Valid reports whether k is a recognized kind.
func (k DMChangeKind) Valid() bool {
	switch k {
	case DMChangeOpened, DMChangeRead, DMChangeState, DMChangeParticipant:
		return true
	default:
		return false
	}
}

// DMConversationV1 is one conversation as one of its participants sees it.
// The participant pair is the same for both; everything from PeerID down is
// the reader's own view.
type DMConversationV1 struct {
	// Schema is the document discriminator; always DMConversationSchemaV1.
	Schema string `json:"schema"`
	// ID is the conversation id. It is not a channel id.
	ID int64 `json:"id"`
	// ParticipantIDs is the two participants, ascending and distinct.
	ParticipantIDs []int64 `json:"participant_ids"`
	// CreatedAt is when the conversation was first opened.
	CreatedAt Timestamp `json:"created_at"`
	// PeerID is the participant who is not the reader.
	PeerID int64 `json:"peer_id"`
	// PeerName is the peer's current display name as the reader may see it;
	// omitted when there is none to give.
	PeerName string `json:"peer_name,omitempty"`
	// PeerAvailable is false when the reader may not send to the peer
	// because the peer's account is disabled or gone. It is never false
	// because of the peer's contact preferences, a block or a switch: a send
	// then fails with ErrorCodeContactUnavailable instead.
	PeerAvailable bool `json:"peer_available"`
	// State is where the conversation sits in the reader's list.
	State DMState `json:"state"`
	// LastMessageID is the id of the newest message in the conversation; 0
	// when it has none.
	LastMessageID int64 `json:"last_message_id"`
	// LastMessageAt is when that message was posted; omitted when there is
	// none.
	LastMessageAt *Timestamp `json:"last_message_at,omitempty"`
	// LastReadID is the reader's read mark; 0 when they never marked it.
	LastReadID int64 `json:"last_read_id"`
	// Unread is how many messages after LastReadID the reader did not write,
	// at most MaxUnreadCount.
	Unread int `json:"unread"`
}

// Validate reports whether the document is structurally well-formed.
func (c DMConversationV1) Validate() error {
	if c.Schema != DMConversationSchemaV1 {
		return fmt.Errorf("schema: dm conversation schema must be %q, got %q", DMConversationSchemaV1, c.Schema)
	}
	if c.ID <= 0 {
		return errors.New("schema: dm conversation id must be positive")
	}
	if err := validateDMParticipants(c.ParticipantIDs); err != nil {
		return err
	}
	if c.CreatedAt.IsZero() {
		return errors.New("schema: dm conversation created_at is required")
	}
	if c.PeerID != c.ParticipantIDs[0] && c.PeerID != c.ParticipantIDs[1] {
		return errors.New("schema: dm conversation peer_id must be one of participant_ids")
	}
	if !c.State.Valid() {
		return errors.New("schema: dm conversation state is not one of open, archived, hidden")
	}
	if c.LastMessageID < 0 {
		return errors.New("schema: dm conversation last_message_id must not be negative")
	}
	if (c.LastMessageID == 0) != (c.LastMessageAt == nil) {
		return errors.New("schema: dm conversation last_message_at must be present exactly when last_message_id is")
	}
	if c.LastMessageAt != nil && c.LastMessageAt.IsZero() {
		return errors.New("schema: dm conversation last_message_at must not be zero")
	}
	if c.LastReadID < 0 || c.LastReadID > c.LastMessageID {
		return errors.New("schema: dm conversation last_read_id must be between 0 and last_message_id")
	}
	if c.Unread < 0 || c.Unread > MaxUnreadCount {
		return fmt.Errorf("schema: dm conversation unread must be between 0 and %d", MaxUnreadCount)
	}
	return nil
}

// validateDMParticipants checks a conversation's pair: exactly two positive
// ids, strictly ascending.
func validateDMParticipants(ids []int64) error {
	if len(ids) != 2 {
		return errors.New("schema: dm conversation participant_ids must hold exactly two principals")
	}
	if ids[0] <= 0 || ids[1] <= 0 {
		return errors.New("schema: dm conversation participant_ids must be positive")
	}
	if ids[0] >= ids[1] {
		return errors.New("schema: dm conversation participant_ids must be ascending and distinct")
	}
	return nil
}

// OpenDMRequestV1 is the body of POST /v1/dms: open the conversation with
// PeerID, or resume the one that exists. It is idempotent: the answer is
// 201 with Created set the first time, and 200 with the same conversation
// every time after, from either participant. Opening tells the peer nothing:
// the conversation enters the peer's list, and their stream, with its first
// message. Opening a hidden conversation sets it open again; an archived one
// stays archived.
type OpenDMRequestV1 struct {
	PeerID int64 `json:"peer_id"`
}

// Validate reports whether the request is structurally well-formed. That
// the peer is not the caller is the server's check (invalid_request).
func (r OpenDMRequestV1) Validate() error {
	if r.PeerID <= 0 {
		return errors.New("schema: open dm peer_id must be positive")
	}
	return nil
}

// OpenDMResponseV1 is the answer to POST /v1/dms.
type OpenDMResponseV1 struct {
	Conversation DMConversationV1 `json:"conversation"`
	// Created is true when this request created the conversation.
	Created bool `json:"created"`
}

// ListDMConversationsResponseV1 is the body of GET /v1/dms: one page of the
// caller's conversations in one DMState (open by default, archived by
// ?state=archived; hidden ones are never listed). The order is newest
// activity first: last_message_id descending, then the conversations with
// no message by id descending. Conversations is never null and holds at most
// MaxDMConversationsPage.
//
// NextCursor is opaque: the client passes it back as ?cursor= and never
// reads it. It is present only when there may be more, and is never a
// function of conversations the caller is not in. A cursor the server did
// not issue, or that is longer than MaxDMCursorBytes, is invalid_request.
// A conversation whose activity moves while a client pages may appear twice
// or be missed; the reconnect snapshot and DMChangedV1 are what keep a
// client's list current, not the pages.
type ListDMConversationsResponseV1 struct {
	Conversations []DMConversationV1 `json:"conversations"`
	NextCursor    string             `json:"next_cursor,omitempty"`
}

// Validate reports whether the page is well-formed: a list, not null, of at
// most MaxDMConversationsPage valid conversations, and a cursor of URL-safe
// characters within MaxDMCursorBytes.
func (r ListDMConversationsResponseV1) Validate() error {
	if r.Conversations == nil {
		return errors.New("schema: dm conversations must be a list, not null")
	}
	if len(r.Conversations) > MaxDMConversationsPage {
		return fmt.Errorf("schema: dm conversations must be at most %d", MaxDMConversationsPage)
	}
	for i, c := range r.Conversations {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("schema: dm conversations[%d]: %w", i, err)
		}
	}
	if r.NextCursor != "" && !validDMToken(r.NextCursor, MaxDMCursorBytes) {
		return fmt.Errorf("schema: dm next_cursor must be 1 to %d URL-safe characters", MaxDMCursorBytes)
	}
	return nil
}

// validDMToken reports whether s is 1 to max bytes of [A-Za-z0-9_-]: the
// alphabet of a client nonce and of a list cursor, safe in a URL and a log.
func validDMToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// DMMessageV1 is one message of a conversation. It is not a MessageV2: it
// has no channel, no payload, no audience and no thread, and its schema name
// says so before any field is read. A client that reads only conch.message.v2
// frames skips it.
//
// Message ids are an id space of their own (design §2.1; ADR-017
// Consequences): channel message 42 and DM message 42 both exist, and a
// message is named by its conversation and id together. Ids ascend within
// one conversation, which is all a history cursor and the snapshot's
// HighWater need; nothing orders them against channel messages, so the
// pending feed (ListPendingNotificationsResponseV2) has an opaque cursor of
// its own.
//
// Edit and removal are in v1 (ADR-017 Decision 5), by the author alone and
// as a channel message's are (message_edit.go, moderation.go): EditedAt marks
// an edited message, and a removed one is a tombstone that keeps its place.
type DMMessageV1 struct {
	// Schema is the document discriminator; always DMMessageSchemaV1.
	Schema string `json:"schema"`
	// ID is the server-assigned message id.
	ID int64 `json:"id"`
	// ConversationID is the conversation the message belongs to.
	ConversationID int64 `json:"conversation_id"`
	// AuthorID is the participant who wrote it.
	AuthorID int64 `json:"author_id"`
	// CreatedAt is when the server persisted it (UTC, ms precision).
	CreatedAt Timestamp `json:"created_at"`
	// Body is the text; always present, as on a channel message. For a
	// removed message it is MessageRemovedByAuthorBody.
	Body string `json:"body"`
	// FileIDs is the DM files the message attaches, in the order sent;
	// omitted when none, and always for a removed message. Each is a
	// DMFileV1 of this conversation.
	FileIDs []int64 `json:"file_ids,omitempty"`
	// EditedAt is when the author last changed Body; nil (omitted) for a
	// message never edited, and for a removed one. It is strictly after
	// CreatedAt and strictly increases with each edit, so of two copies of
	// one message the one with the later EditedAt (absent counting as
	// earliest) is the newer. A frame carrying it is an update of a message
	// already sent (DMMessageEditedV1), not a new message.
	EditedAt *Timestamp `json:"edited_at,omitempty"`
	// Removed is true for a message its author removed: the tombstone. There
	// is no moderator removal of a DM, so the body is always
	// MessageRemovedByAuthorBody; file_ids and edited_at are omitted; id,
	// conversation_id, author_id and created_at stay, so position and
	// attribution hold. Omitted when false. A removed copy is never replaced
	// by one that is not.
	Removed bool `json:"removed,omitempty"`
	// ClientNonce is the nonce the author sent it with. It is present only
	// on the author's own copies, so their client can match its pending
	// send, and never on the peer's.
	ClientNonce string `json:"client_nonce,omitempty"`
}

// Validate reports whether the message is structurally well-formed.
func (m DMMessageV1) Validate() error {
	if m.Schema != DMMessageSchemaV1 {
		return fmt.Errorf("schema: dm message schema must be %q, got %q", DMMessageSchemaV1, m.Schema)
	}
	if m.ID <= 0 {
		return errors.New("schema: dm message id must be positive")
	}
	if m.ConversationID <= 0 {
		return errors.New("schema: dm message conversation_id must be positive")
	}
	if m.AuthorID <= 0 {
		return errors.New("schema: dm message author_id must be positive")
	}
	if m.CreatedAt.IsZero() {
		return errors.New("schema: dm message created_at is required")
	}
	if m.Body == "" {
		return errors.New("schema: dm message body is required")
	}
	if m.FileIDs != nil {
		if err := (FileAttachmentV1{FileIDs: m.FileIDs}).Validate(); err != nil {
			return fmt.Errorf("schema: dm message: %w", err)
		}
	}
	if m.EditedAt != nil && !m.EditedAt.Time().After(m.CreatedAt.Time()) {
		return errors.New("schema: dm message edited_at must be after created_at")
	}
	if m.Removed {
		if m.Body != MessageRemovedByAuthorBody {
			return fmt.Errorf("schema: removed dm message body must be %q", MessageRemovedByAuthorBody)
		}
		if m.FileIDs != nil {
			return errors.New("schema: removed dm message must not carry file_ids")
		}
		if m.EditedAt != nil {
			return errors.New("schema: removed dm message must not carry edited_at")
		}
	}
	if m.ClientNonce != "" && !validDMToken(m.ClientNonce, MaxDMClientNonceBytes) {
		return fmt.Errorf("schema: dm message client_nonce must be 1 to %d URL-safe characters", MaxDMClientNonceBytes)
	}
	return nil
}

// PostDMMessageRequestV1 is the body of POST
// /v1/dms/{conversation}/messages. The author is the caller.
//
// ClientNonce makes the send idempotent. It is required, chosen by the
// client once per message (a random 128-bit value, base64url, is the
// intent), and reused unchanged on every retry of that message. The server
// keys it on (conversation, author, nonce) for DMClientNonceWindowSeconds:
// a repeat with the same body and file_ids answers 200 with the message the
// first one stored and Replayed set, and stores and sends nothing; a repeat
// that differs answers ErrorCodeDMNonceConflict. A first send answers 201.
//
// The body is bounded as a channel post's is (MaxPostRequestBytes for the
// whole request).
type PostDMMessageRequestV1 struct {
	Body        string  `json:"body"`
	FileIDs     []int64 `json:"file_ids,omitempty"`
	ClientNonce string  `json:"client_nonce"`
}

// Validate reports whether the request is structurally well-formed.
func (r PostDMMessageRequestV1) Validate() error {
	if r.Body == "" {
		return errors.New("schema: dm message body is required")
	}
	// A request's empty list is no files; a stored message never carries
	// one (DMMessageV1 omits the field instead).
	if len(r.FileIDs) > 0 {
		if err := (FileAttachmentV1{FileIDs: r.FileIDs}).Validate(); err != nil {
			return fmt.Errorf("schema: dm message: %w", err)
		}
	}
	if !validDMToken(r.ClientNonce, MaxDMClientNonceBytes) {
		return fmt.Errorf("schema: dm message client_nonce must be 1 to %d URL-safe characters", MaxDMClientNonceBytes)
	}
	return nil
}

// PostDMMessageResponseV1 is the answer to a send.
type PostDMMessageResponseV1 struct {
	Message DMMessageV1 `json:"message"`
	// Replayed is true when the request repeated an earlier send's nonce
	// and this is that send's message.
	Replayed bool `json:"replayed,omitempty"`
}

// EditDMMessageRequestV1 is the body of PATCH
// /v1/dms/{conversation}/messages/{message}: the message's new text, and
// nothing else. Files cannot be named, so cannot be changed, and the server
// decodes with unknown fields refused. Only the author may edit, and only
// where a send would be allowed: the checks of a send apply, so an edit can
// answer ErrorCodeContactUnavailable, ErrorCodeDMsOff and
// ErrorCodeOwnSignInRequired. Someone else's message is 403 forbidden, as
// removing it is; a removed one is ErrorCodeMessageNotEditable (409). The
// window and the cap are the channel's (MessageEditWindowSeconds,
// MaxMessageEdits: 409 message_edit_window_closed and
// message_edit_limit_reached). The body is bounded as a send's is.
type EditDMMessageRequestV1 struct {
	Body string `json:"body"`
}

// Validate reports whether the request is well-formed: a body that is not
// empty and not only white space.
func (r EditDMMessageRequestV1) Validate() error {
	if strings.TrimSpace(r.Body) == "" {
		return errors.New("schema: edit dm message body must not be empty")
	}
	return nil
}

// EditDMMessageResponseV1 is the answer to an edit: the message as it now is.
// After an edit that changed the text it carries EditedAt; after a request
// whose body equals the current text nothing changed, and it is the message
// as it was.
type EditDMMessageResponseV1 struct {
	Message DMMessageV1 `json:"message"`
}

// Validate reports whether the response's message is well-formed.
func (r EditDMMessageResponseV1) Validate() error {
	return r.Message.Validate()
}

// DMMessageEditedV1 is the stream frame for an edit: the edited message, for
// both participants' sockets. A client updates the copy it holds and does not
// notify, count unread or treat it as a new message.
type DMMessageEditedV1 struct {
	// Schema is the frame discriminator; always DMMessageEditedSchemaV1.
	Schema  string      `json:"schema"`
	Message DMMessageV1 `json:"message"`
}

// Validate reports whether the frame is well-formed: the schema name, and a
// valid, edited, not removed message.
func (f DMMessageEditedV1) Validate() error {
	if f.Schema != DMMessageEditedSchemaV1 {
		return fmt.Errorf("schema: dm message edited schema must be %q, got %q", DMMessageEditedSchemaV1, f.Schema)
	}
	if err := f.Message.Validate(); err != nil {
		return err
	}
	if f.Message.EditedAt == nil || f.Message.Removed {
		return errors.New("schema: dm message edited frame must carry an edited message that is not removed")
	}
	return nil
}

// DMMessageRemovedV1 is the stream frame for an author's removal: the
// tombstone, for both participants' sockets. A client replaces the copy it
// holds (a notification preview, an open view) and notifies no one. Copies a
// person already received cannot be recalled.
type DMMessageRemovedV1 struct {
	// Schema is the frame discriminator; always DMMessageRemovedSchemaV1.
	Schema  string      `json:"schema"`
	Message DMMessageV1 `json:"message"`
}

// Validate reports whether the frame is well-formed: the schema name, and a
// valid message that is removed.
func (f DMMessageRemovedV1) Validate() error {
	if f.Schema != DMMessageRemovedSchemaV1 {
		return fmt.Errorf("schema: dm message removed schema must be %q, got %q", DMMessageRemovedSchemaV1, f.Schema)
	}
	if err := f.Message.Validate(); err != nil {
		return err
	}
	if !f.Message.Removed {
		return errors.New("schema: dm message removed frame must carry a removed message")
	}
	return nil
}

// ListDMMessagesResponseV1 is the body of GET
// /v1/dms/{conversation}/messages?before=<id>|after=<id>&limit=<n>. Messages
// is never null, ascending by id, and holds at most the limit
// (DefaultDMPageSize, at most MaxDMPageSize).
//
// With neither before nor after, the page is the newest messages. With
// before, the newest messages below it; NextBefore is then the lowest id on
// the page, present only when older messages may exist. With after, the
// oldest messages above it, for catching up; NextAfter is the highest id on
// the page, present only when newer ones may exist. Both at once is
// invalid_request. A page with neither cursor is the whole story.
type ListDMMessagesResponseV1 struct {
	Messages   []DMMessageV1 `json:"messages"`
	NextBefore int64         `json:"next_before,omitempty"`
	NextAfter  int64         `json:"next_after,omitempty"`
}

// Validate reports whether the page is well-formed: a list, not null, of at
// most MaxDMPageSize valid messages of one conversation in strictly
// ascending id, at most one cursor, and that cursor at the page's edge.
func (r ListDMMessagesResponseV1) Validate() error {
	if r.Messages == nil {
		return errors.New("schema: dm messages must be a list, not null")
	}
	if len(r.Messages) > MaxDMPageSize {
		return fmt.Errorf("schema: dm messages must be at most %d", MaxDMPageSize)
	}
	if err := validateDMMessageRun(r.Messages, true); err != nil {
		return err
	}
	if r.NextBefore != 0 && r.NextAfter != 0 {
		return errors.New("schema: dm messages carry at most one of next_before and next_after")
	}
	if r.NextBefore < 0 || r.NextAfter < 0 {
		return errors.New("schema: dm messages cursors must not be negative")
	}
	if r.NextBefore != 0 && (len(r.Messages) == 0 || r.NextBefore != r.Messages[0].ID) {
		return errors.New("schema: dm messages next_before must be the lowest id on the page")
	}
	if r.NextAfter != 0 && (len(r.Messages) == 0 || r.NextAfter != r.Messages[len(r.Messages)-1].ID) {
		return errors.New("schema: dm messages next_after must be the highest id on the page")
	}
	return nil
}

// validateDMMessageRun checks a page of messages: each valid, all of one
// conversation, ids strictly ascending (or descending when ascending is
// false).
func validateDMMessageRun(msgs []DMMessageV1, ascending bool) error {
	for i, m := range msgs {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("schema: dm messages[%d]: %w", i, err)
		}
		if i == 0 {
			continue
		}
		if m.ConversationID != msgs[0].ConversationID {
			return fmt.Errorf("schema: dm messages[%d] is of another conversation", i)
		}
		if ascending && m.ID <= msgs[i-1].ID || !ascending && m.ID >= msgs[i-1].ID {
			return fmt.Errorf("schema: dm messages[%d] is out of order", i)
		}
	}
	return nil
}

// SearchDMMessagesResponseV1 is the body of GET
// /v1/dms/{conversation}/search?q=<text>&before=<id>: one page of the
// conversation's messages that match, newest first. Search is scoped to the
// one conversation in the path; no route searches the DMs of others, and
// channel search (SearchMessagesResponseV2) never returns a DM message.
// NextBefore is the id to pass as before for the next page, present only
// when there may be more.
type SearchDMMessagesResponseV1 struct {
	Messages   []DMMessageV1 `json:"messages"`
	NextBefore int64         `json:"next_before,omitempty"`
}

// Validate reports whether the page is well-formed: a list, not null, of at
// most MaxDMPageSize valid messages of one conversation, newest first, and a
// cursor that is the last message's id.
func (r SearchDMMessagesResponseV1) Validate() error {
	if r.Messages == nil {
		return errors.New("schema: dm search messages must be a list, not null")
	}
	if len(r.Messages) > MaxDMPageSize {
		return fmt.Errorf("schema: dm search messages must be at most %d", MaxDMPageSize)
	}
	if err := validateDMMessageRun(r.Messages, false); err != nil {
		return err
	}
	if r.NextBefore < 0 {
		return errors.New("schema: dm search next_before must not be negative")
	}
	if r.NextBefore != 0 && (len(r.Messages) == 0 || r.NextBefore != r.Messages[len(r.Messages)-1].ID) {
		return errors.New("schema: dm search next_before must be the id of the page's last message")
	}
	return nil
}

// DMReadStateV1 is one participant's read mark in one conversation: the
// answer to PUT /v1/dms/{conversation}/read-state, whose body is
// MarkReadRequestV1 with the channel route's rules (the mark only moves
// forward). The mark is the reader's alone: the peer is never told where it
// is.
type DMReadStateV1 struct {
	// Schema is the document discriminator; always DMReadStateSchemaV1.
	Schema string `json:"schema"`
	// ConversationID is the conversation the mark is for.
	ConversationID int64 `json:"conversation_id"`
	// LastReadID is the newest message the reader marked as read; 0 when
	// they never did.
	LastReadID int64 `json:"last_read_id"`
	// Unread is how many messages after LastReadID the reader did not write,
	// at most MaxUnreadCount.
	Unread int `json:"unread"`
	// UpdatedAt is when the mark last moved; omitted when it never has.
	UpdatedAt *Timestamp `json:"updated_at,omitempty"`
}

// Validate reports whether the document is structurally well-formed.
func (r DMReadStateV1) Validate() error {
	if r.Schema != DMReadStateSchemaV1 {
		return fmt.Errorf("schema: dm read state schema must be %q, got %q", DMReadStateSchemaV1, r.Schema)
	}
	if r.ConversationID <= 0 {
		return errors.New("schema: dm read state conversation_id must be positive")
	}
	if r.LastReadID < 0 {
		return errors.New("schema: dm read state last_read_id must not be negative")
	}
	if r.Unread < 0 || r.Unread > MaxUnreadCount {
		return fmt.Errorf("schema: dm read state unread must be between 0 and %d", MaxUnreadCount)
	}
	if r.UpdatedAt != nil && r.UpdatedAt.IsZero() {
		return errors.New("schema: dm read state updated_at must not be zero")
	}
	if r.LastReadID == 0 && r.UpdatedAt != nil {
		return errors.New("schema: dm read state that was never marked must not carry updated_at")
	}
	return nil
}

// SetDMStateRequestV1 is the body of PUT /v1/dms/{conversation}/state: the
// caller's archive, hide or reopen. It changes nothing for the peer and
// sends the peer nothing.
type SetDMStateRequestV1 struct {
	State DMState `json:"state"`
}

// Validate reports whether the request names a known state.
func (r SetDMStateRequestV1) Validate() error {
	if !r.State.Valid() {
		return errors.New("schema: dm state is not one of open, archived, hidden")
	}
	return nil
}

// DMPreferencesV1 is a person's contact preference: the answer to GET and
// PUT /v1/dms/preferences, and the body of the PUT, which replaces the whole
// object. It belongs to the principal, not to a credential, and operators do
// not see it.
//
// Accept says who may contact them (DMAcceptPolicy). There is no block list
// here: blocking one person is the general personal block relation (#1832),
// which DMs consult, so a block holds in channels and DMs alike and the DM
// contract carries no list of its own. A refused contact answers
// ErrorCodeContactUnavailable whatever the reason.
type DMPreferencesV1 struct {
	Accept DMAcceptPolicy `json:"accept"`
}

// DefaultDMPreferences is what a person has before they write any.
func DefaultDMPreferences() DMPreferencesV1 {
	return DMPreferencesV1{Accept: DMAcceptEveryone}
}

// Validate reports whether the preference is well-formed.
func (p DMPreferencesV1) Validate() error {
	if !p.Accept.Valid() {
		return errors.New("schema: dm preferences accept is not one of everyone, shared_channel, nobody")
	}
	return nil
}

// The operator's two switches (ADR-017 Decision 9; design §3.2 and §8). Both
// are content-blind: they show an operator no conversation, pair, call or
// message, only whether direct messages are on. Neither is an anti-abuse
// tool beyond that: there is no DM moderation and no "report a DM" in v1.
// Each change is audited as a setting is, naming only the person switched
// (or the server), never a counterparty.
//
// A switch off refuses new contact: open, send, edit and call invites answer
// ErrorCodeDMsOff to the person switched off (or to everyone, for the server
// switch) and ErrorCodeContactUnavailable to anyone writing to them. History
// stays readable and one's own messages removable, and a live call ends.

// DMServerSettingsV1 is the server-wide switch: the answer to GET (operators
// hold it) and PUT /v1/dms/settings, and the body of the PUT. Changing it
// needs change_server_settings, the permission of the other server-wide
// policy (PermChangeServerSettings), with recent authentication. It is on by
// default.
type DMServerSettingsV1 struct {
	// DMsEnabled is whether direct messages are on for the server.
	DMsEnabled bool `json:"dms_enabled"`
}

// DefaultDMServerSettings is what a server has before an operator writes any:
// direct messages on.
func DefaultDMServerSettings() DMServerSettingsV1 {
	return DMServerSettingsV1{DMsEnabled: true}
}

// DMPermissionRequestV1 is the body of PUT /v1/principals/{id}/dm-permission
// (manage_members, recent authentication, as for voice-permission): whether
// the person may use direct messages. The target must be a person: an agent
// is 409 not_a_person, a hidden or unknown id 404 principal_not_found.
type DMPermissionRequestV1 struct {
	DMAllowed bool `json:"dm_allowed"`
}

// DMPermissionResponseV1 is the state now and whether the request changed it
// (false on an idempotent repeat, which writes no audit row).
type DMPermissionResponseV1 struct {
	PrincipalID int64 `json:"principal_id"`
	DMAllowed   bool  `json:"dm_allowed"`
	Changed     bool  `json:"changed"`
}

// DMFileV1 is the metadata of a file uploaded to a conversation. It is
// FileV1 with the conversation in place of the channel; the filename and
// media type are as untrusted, and checked by the same rules.
//
// Ownership: the file belongs to the conversation. Both participants may
// read it while they can read the conversation; it may be attached only by
// its uploader, only in this conversation, and only once. A channel file
// cannot be attached to a DM, nor a DM file to a channel message: each
// answers its route's not-attachable error and does not say why. A file of
// a conversation the caller is not in answers
// ErrorCodeDMConversationNotFound.
type DMFileV1 struct {
	Schema         string    `json:"schema"`
	ID             int64     `json:"id"`
	ConversationID int64     `json:"conversation_id"`
	UploaderID     int64     `json:"uploader_id"`
	Filename       string    `json:"filename"`
	MediaType      string    `json:"media_type"`
	Size           int64     `json:"size"`
	SHA256         string    `json:"sha256"`
	CreatedAt      Timestamp `json:"created_at"`
}

// Validate reports whether the metadata is well-formed, by FileV1's rules.
func (f DMFileV1) Validate() error {
	if f.Schema != DMFileSchemaV1 {
		return fmt.Errorf("schema: dm file schema must be %q, got %q", DMFileSchemaV1, f.Schema)
	}
	if f.ConversationID <= 0 {
		return errors.New("schema: dm file conversation_id must be positive")
	}
	// The rest are FileV1's rules; a stand-in channel id satisfies the one
	// rule that does not apply.
	return FileV1{
		Schema: FileSchemaV1, ID: f.ID, ChannelID: 1, UploaderID: f.UploaderID,
		Filename: f.Filename, MediaType: f.MediaType, Size: f.Size, SHA256: f.SHA256,
		CreatedAt: f.CreatedAt,
	}.Validate()
}

// DMChangedV1 is the events-stream signal that one conversation may have
// changed for this reader. It is sent only to the reader's own sockets. Like
// conch.channels_changed.v1 it is a signal, not a delta: the client re-reads
// GET /v1/dms/{conversation} and replaces what it holds. A 404 there means
// the conversation is gone from the reader's view.
type DMChangedV1 struct {
	// Schema is the document discriminator; always DMChangedSchemaV1.
	Schema string `json:"schema"`
	// ConversationID is the conversation to re-read.
	ConversationID int64 `json:"conversation_id"`
	// Kind is a hint at what changed; a client re-reads whatever it is.
	Kind DMChangeKind `json:"kind"`
}

// Validate reports whether the frame is well-formed. An unknown kind is not
// refused here either: Valid is the server's check on what it sends.
func (c DMChangedV1) Validate() error {
	if c.Schema != DMChangedSchemaV1 {
		return fmt.Errorf("schema: dm changed schema must be %q, got %q", DMChangedSchemaV1, c.Schema)
	}
	if c.ConversationID <= 0 {
		return errors.New("schema: dm changed conversation_id must be positive")
	}
	if c.Kind == "" {
		return errors.New("schema: dm changed kind is required")
	}
	return nil
}

// DMSnapshotV1 is the reconnect snapshot: the first DM frame on every
// (re)connect of the per-person events stream, and the body of GET
// /v1/dms/snapshot. It is the reader's open conversations, newest activity
// first as the list orders them, at most MaxDMSnapshotConversations; More is
// true when there are others, which the list route pages.
//
// HighWater is the highest DM message id the reader may read at the moment
// the snapshot was taken; 0 when there is none. The stream promises that
// every DM message with an id at or below it is reflected in the snapshot
// (as a conversation's LastMessageID or below it), and that every one above
// it is sent as a DMMessageV1 frame after the snapshot on this connection.
// So a client replaces its list with the snapshot, fetches history with
// ?after= for each conversation whose LastMessageID is above what it holds,
// and then applies frames; nothing falls between the two. A frame at or
// below HighWater is a duplicate and is ignored.
type DMSnapshotV1 struct {
	Schema        string             `json:"schema"`
	Conversations []DMConversationV1 `json:"conversations"`
	More          bool               `json:"more"`
	HighWater     int64              `json:"high_water"`
	TakenAt       Timestamp          `json:"taken_at"`
	// DMsEnabled is whether the reader may start or send direct messages now:
	// true only when the server's switch and the reader's own are both on. It
	// says nothing about anyone else, and not which switch is off, so a client
	// can stop offering the action without learning more.
	DMsEnabled bool `json:"dms_enabled"`
}

// Validate reports whether the snapshot is well-formed: open conversations
// only, none twice, none newer than HighWater.
func (s DMSnapshotV1) Validate() error {
	if s.Schema != DMSnapshotSchemaV1 {
		return fmt.Errorf("schema: dm snapshot schema must be %q, got %q", DMSnapshotSchemaV1, s.Schema)
	}
	if s.Conversations == nil {
		return errors.New("schema: dm snapshot conversations must be a list, not null")
	}
	if len(s.Conversations) > MaxDMSnapshotConversations {
		return fmt.Errorf("schema: dm snapshot conversations must be at most %d", MaxDMSnapshotConversations)
	}
	if s.HighWater < 0 {
		return errors.New("schema: dm snapshot high_water must not be negative")
	}
	if s.TakenAt.IsZero() {
		return errors.New("schema: dm snapshot taken_at is required")
	}
	seen := make(map[int64]struct{}, len(s.Conversations))
	for i, c := range s.Conversations {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("schema: dm snapshot conversations[%d]: %w", i, err)
		}
		if c.State != DMStateOpen {
			return fmt.Errorf("schema: dm snapshot conversations[%d] is not open", i)
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("schema: dm snapshot conversations[%d] is listed twice", i)
		}
		seen[c.ID] = struct{}{}
		if c.LastMessageID > s.HighWater {
			return fmt.Errorf("schema: dm snapshot conversations[%d] has a message above high_water", i)
		}
	}
	return nil
}
