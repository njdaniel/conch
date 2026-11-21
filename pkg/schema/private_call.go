package schema

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Private calls (issue #2029, child of #2013; the 1:1 section of #1501).
// DRAFT: these shapes follow ADR-017 (#2038, still a proposal, PR #2044) and
// its design note docs/design/direct-messages.md §4, which carry Nick's
// decisions of 2026-11-19 on #2013; nothing serves them yet.
//
// A private call is one attempt by one local human principal to talk, by
// voice only, to the other person of a DM conversation (dm.go). Each attempt
// is its own document with its own opaque id, a revision and a state, and
// gets its own fresh LiveKit room, which exists only after the callee
// accepts. Calls carry no camera and no screen share: no document here has a
// field for either, and a client never asks for one.
//
// Routes (REST; every one human-only, with the DM routes' refusals for an
// agent and for an act-as credential):
//
//	POST /v1/dms/{conversation}/calls    invite (InvitePrivateCallRequestV1 → PrivateCallResponseV1)
//	GET  /v1/dms/{conversation}/calls    missed and finished calls (ListPrivateCallsResponseV1)
//	GET  /v1/calls/current               reconnect snapshot (PrivateCallSnapshotV1)
//	POST /v1/calls/{call}/accept         callee answers   ┐
//	POST /v1/calls/{call}/decline        callee refuses   │ PrivateCallActionRequestV1
//	POST /v1/calls/{call}/cancel         caller gives up  │   → PrivateCallResponseV1
//	POST /v1/calls/{call}/hangup         either side ends ┘
//	POST /v1/calls/{call}/session        media join (no body → PrivateCallSessionV1)
//
// Events: the per-person events stream (#2040) carries, beside the DM frames,
// PrivateCallRingingV1 (a call is ringing for you: every one of your sockets),
// PrivateCallStopRingingV1 (stop ringing: another device of yours answered or
// declined, or the caller gave up), PrivateCallStateV1 (the attempt changed:
// both people's sockets for an attempt that rang, subject to current access)
// and, on every (re)connect, a PrivateCallSnapshotV1 right after the DM
// snapshot.
//
// What no client-visible shape carries: a LiveKit room name, a token, or a
// URL. They exist in PrivateCallSessionV1 alone, the grant answer to the
// media-join route, issued only to the credential selected for that side. A
// call document, a frame, a history page and a snapshot name none of them,
// and a test pins that by walking the types' field names.
//
// An invite that starts ringing answers HTTP 201. An invite to a callee
// who cannot be rung (busy, offline, blocked either way, opted out, switched
// off, hidden) answers HTTP 200 with an attempt in state
// PrivateCallStateUnavailable and no reason (ADR-017 Decision 10). There is
// no ringing attempt for the caller to act on. Only the inviting credential's
// HTTP response and authorized retries of that invite carry the attempt. It
// produces no stream fan-out to either side, snapshot, call history entry
// or notification.
// Every {call} route treats its id as absent. Only an invite that actually
// rings can publish a DM conversation to the peer. An unavailable invite
// leaves DM inboxes, conversation timestamps, revisions and unread counts
// untouched, and emits no DM event or push. The route's real errors are
// the DM ones (ErrorCodeDMsOff, ErrorCodeOwnSignInRequired, 404 dm_not_found,
// 400 invalid_request) and the call ones below; none names a reason about the
// other person.
//
// Everything here is additive: new types, constants and error codes. No
// published type changes, and voice_control stays at v3 (v4 is #2039).

// Schema names of the private call documents that carry one. REST request
// bodies carry none, as elsewhere in this package.
const (
	PrivateCallSchemaV1            = "conch.private_call.v1"
	PrivateCallRingingSchemaV1     = "conch.private_call_ringing.v1"
	PrivateCallStopRingingSchemaV1 = "conch.private_call_stop_ringing.v1"
	PrivateCallStateSchemaV1       = "conch.private_call_state.v1"
	PrivateCallSnapshotSchemaV1    = "conch.private_call_snapshot.v1"
	PrivateCallSessionSchemaV1     = "conch.private_call_session.v1"
)

// The v1 timings (design §4.1). They are the server's to enforce; the
// documents carry the deadlines they produce, so a client never computes one.
// #2030 may change them with a recorded reason in the design note.
const (
	// PrivateCallRingSeconds is how long a call rings before it is missed.
	PrivateCallRingSeconds = 40
	// PrivateCallJoinSeconds is how long both selected endpoints have to reach
	// the room after the callee accepts before the call fails.
	PrivateCallJoinSeconds = 30
	// PrivateCallReconnectGraceSeconds is how long an endpoint may be absent
	// from an active call's room before the call ends (reason gone).
	PrivateCallReconnectGraceSeconds = 20
	// PrivateCallActionWindowSeconds is how long the server remembers an
	// action id and the request it identifies, scoped to the credential.
	PrivateCallActionWindowSeconds = 3600
)

// Bounds of the call routes.
const (
	// MinPrivateCallIDBytes and MaxPrivateCallIDBytes bound an attempt id: an
	// opaque URL-safe token minted by NewPrivateCallID. A client never parses it.
	MinPrivateCallIDBytes = 16
	MaxPrivateCallIDBytes = 64
	// MaxPrivateCallActionIDBytes is the longest action id a request may carry.
	MaxPrivateCallActionIDBytes = 64
	// MaxPrivateCallHistoryPage is the most calls one history page holds.
	MaxPrivateCallHistoryPage = 50
)

// NewPrivateCallID mints an opaque attempt id from 128 random bits encoded as
// 22 unpadded base64url characters. Conch reserves LiveKit room names matching
// ^[a-z0-9-]{0,31}-[a-z2-7]{26}$, including the default conch- prefix and any
// current or former configured prefix. Every reserved name is at least 27
// characters long, so issued ids can cross the voice_control v4 boundary.
func NewPrivateCallID() string {
	var random [16]byte
	_, _ = rand.Read(random[:]) // rand.Read fills the buffer or terminates the process.
	return base64.RawURLEncoding.EncodeToString(random[:])
}

// Error codes of the call routes, additive on Error. The DM codes
// ErrorCodeDMNotFound, ErrorCodeDMsOff and ErrorCodeOwnSignInRequired apply
// to the invite route as to the others. ErrorCodeContactUnavailable does not
// appear: an unreachable callee is the state unavailable, not an error.
const (
	// ErrorCodeCallNotFound: no such visible attempt (HTTP 404). The same
	// response covers an unknown id, a requester outside the pair and an
	// unavailable attempt, including when its inviter supplies its id.
	ErrorCodeCallNotFound = "call_not_found"
	// ErrorCodeCallStateChanged: an action named a stale revision and is
	// not an exact replay of a successful action (HTTP 409). Its body is
	// PrivateCallConflictV1, which carries the current attempt; the client
	// follows it and never retries blindly. Also the answer to a media join
	// for a visible attempt that is not accepted or active.
	ErrorCodeCallStateChanged = "call_state_changed"
	// ErrorCodeCallInProgress: the caller already has a non-terminal attempt
	// of their own (HTTP 409), and this invite is not the glare case. It says
	// nothing about the callee.
	ErrorCodeCallInProgress = "call_in_progress"
	// ErrorCodeCallActionConflict: the same credential used this action id
	// inside PrivateCallActionWindowSeconds for a different route, target or
	// request revision (HTTP 409).
	ErrorCodeCallActionConflict = "call_action_conflict"
	// ErrorCodeCallWrongDevice: the caller's credential is not the endpoint
	// selected for the caller's side of this attempt (HTTP 403), on a media
	// join. It is about the caller's own credential only.
	ErrorCodeCallWrongDevice = "call_wrong_device"
)

// PrivateCallState is where an attempt is. The vocabulary is closed. The
// states unavailable, declined, cancelled, missed, failed and ended are
// terminal and final: no transition leaves one and no attempt reopens.
type PrivateCallState string

const (
	// PrivateCallStateUnavailable: the callee could not be rung. Terminal;
	// created in this state and seen by the caller alone.
	PrivateCallStateUnavailable PrivateCallState = "unavailable"
	// PrivateCallStateRinging: invited, rings on the callee's devices until
	// ring_deadline.
	PrivateCallStateRinging PrivateCallState = "ringing"
	// PrivateCallStateAccepted: the callee answered on one device; both
	// selected endpoints may now join the room until join_deadline.
	PrivateCallStateAccepted PrivateCallState = "accepted"
	// PrivateCallStateActive: both selected endpoints are in the room.
	PrivateCallStateActive PrivateCallState = "active"
	// PrivateCallStateDeclined: the callee declined. Terminal.
	PrivateCallStateDeclined PrivateCallState = "declined"
	// PrivateCallStateCancelled: the caller gave up while it rang, or an
	// access change ended it while it rang (no reason shown). Terminal.
	PrivateCallStateCancelled PrivateCallState = "cancelled"
	// PrivateCallStateMissed: nobody answered before ring_deadline. Terminal;
	// the callee alone gets a missed-call entry.
	PrivateCallStateMissed PrivateCallState = "missed"
	// PrivateCallStateEnded: a call that was accepted finished. Terminal.
	PrivateCallStateEnded PrivateCallState = "ended"
	// PrivateCallStateFailed: the call could not be completed: a join that did
	// not happen in time, or conchd starting. Terminal.
	PrivateCallStateFailed PrivateCallState = "failed"
)

// PrivateCallFromNone is the "from" of an invite in the transition table:
// there is no attempt yet. It is not a state any document carries.
const PrivateCallFromNone = "none"

// Valid reports whether s is a state a document may carry.
func (s PrivateCallState) Valid() bool {
	switch s {
	case PrivateCallStateUnavailable, PrivateCallStateRinging, PrivateCallStateAccepted,
		PrivateCallStateActive, PrivateCallStateDeclined, PrivateCallStateCancelled,
		PrivateCallStateMissed, PrivateCallStateEnded, PrivateCallStateFailed:
		return true
	default:
		return false
	}
}

// Terminal reports whether s is final: the attempt can never change again,
// and a client must never treat a document in a terminal state as live.
func (s PrivateCallState) Terminal() bool {
	switch s {
	case PrivateCallStateUnavailable, PrivateCallStateDeclined, PrivateCallStateCancelled,
		PrivateCallStateMissed, PrivateCallStateEnded, PrivateCallStateFailed:
		return true
	default:
		return false
	}
}

// PrivateCallReason says why an attempt ended or failed, for the states that
// have one. The vocabulary is closed and holds no access reason: when a
// block, a disable, a revoked credential or a switch ends a call, the server
// sends the plain terminal state with no reason, so neither side learns
// which (design §4.1).
type PrivateCallReason string

const (
	// PrivateCallReasonHangup: a side hung up. State ended.
	PrivateCallReasonHangup PrivateCallReason = "hangup"
	// PrivateCallReasonGone: an endpoint was absent from an active call's room
	// longer than the reconnect grace. State ended.
	PrivateCallReasonGone PrivateCallReason = "gone"
	// PrivateCallReasonJoinTimeout: after acceptance, both endpoints were not
	// in the room by join_deadline. State failed.
	PrivateCallReasonJoinTimeout PrivateCallReason = "join_timeout"
	// PrivateCallReasonServerRestart: conchd started, or a restore was
	// applied, with the attempt not terminal. State failed.
	PrivateCallReasonServerRestart PrivateCallReason = "server_restart"
)

// Valid reports whether r is a recognized reason.
func (r PrivateCallReason) Valid() bool {
	switch r {
	case PrivateCallReasonHangup, PrivateCallReasonGone,
		PrivateCallReasonJoinTimeout, PrivateCallReasonServerRestart:
		return true
	default:
		return false
	}
}

// PrivateCallRole is the reader's side of an attempt. The vocabulary is
// closed.
type PrivateCallRole string

const (
	// PrivateCallRoleCaller: the reader placed the call.
	PrivateCallRoleCaller PrivateCallRole = "caller"
	// PrivateCallRoleCallee: the reader was rung.
	PrivateCallRoleCallee PrivateCallRole = "callee"
)

// Valid reports whether r is a recognized role.
func (r PrivateCallRole) Valid() bool {
	switch r {
	case PrivateCallRoleCaller, PrivateCallRoleCallee:
		return true
	default:
		return false
	}
}

// The events of the transition table: what moves an attempt, whether a
// person's act or the server's own. They are plain strings, not a vocabulary
// a document carries: no wire shape has an event field.
const (
	// PrivateCallEventInvite: the caller invites a callee who can be rung.
	PrivateCallEventInvite = "invite"
	// PrivateCallEventInviteUnavailable: the caller invites one who cannot be
	// rung (ineligible, busy or with no signed-in device).
	PrivateCallEventInviteUnavailable = "invite_unavailable"
	// PrivateCallEventAccept: the callee's first valid accept.
	PrivateCallEventAccept = "accept"
	// PrivateCallEventDecline: the callee declines.
	PrivateCallEventDecline = "decline"
	// PrivateCallEventCancel: the caller cancels while it rings.
	PrivateCallEventCancel = "cancel"
	// PrivateCallEventRingExpired: ring_deadline passed.
	PrivateCallEventRingExpired = "ring_expired"
	// PrivateCallEventJoined: both selected endpoints were seen in the room.
	PrivateCallEventJoined = "joined"
	// PrivateCallEventJoinExpired: join_deadline passed without both.
	PrivateCallEventJoinExpired = "join_expired"
	// PrivateCallEventHangup: either side hangs up.
	PrivateCallEventHangup = "hangup"
	// PrivateCallEventGraceExpired: an endpoint was absent from an active
	// call's room longer than the reconnect grace.
	PrivateCallEventGraceExpired = "grace_expired"
	// PrivateCallEventRevoked: a block either way, a disabled side, a revoked
	// selected credential, or a switch off for either side or the server.
	PrivateCallEventRevoked = "revoked"
	// PrivateCallEventRestart: conchd started, or a restore was applied.
	PrivateCallEventRestart = "restart"
)

// Who may cause an event, the "Actor" column of the table.
const (
	PrivateCallActorCaller = "caller"
	PrivateCallActorCallee = "callee"
	// PrivateCallActorEither: the caller or the callee.
	PrivateCallActorEither = "either"
	// PrivateCallActorServer: no person; a deadline, the media poller or an
	// access change.
	PrivateCallActorServer = "server"
)

// PrivateCallTransition is one row of the transition table (design §4.1).
type PrivateCallTransition struct {
	// From is a state's name, or PrivateCallFromNone for an invite.
	From   string            `json:"from"`
	Event  string            `json:"event"`
	To     PrivateCallState  `json:"to"`
	Reason PrivateCallReason `json:"reason,omitempty"`
	Actor  string            `json:"actor"`
}

// PrivateCallTransitions is the whole transition table and nothing else is a
// transition: a (state, event) pair absent here is refused. Every row from a
// non-terminal state; no row leaves a terminal state, so a terminal attempt
// cannot be read as active again. The "revoked" rows carry no reason (the
// reason is shown to neither side), and the glare case is not a row because
// it creates no attempt: an invite while the callee is ringing the caller
// returns that attempt as the caller's incoming call
// (PrivateCallResponseV1.Glare). The invite_unavailable row records the
// HTTP result only and does not trigger fan-out or DM publication.
var PrivateCallTransitions = []PrivateCallTransition{
	{PrivateCallFromNone, PrivateCallEventInvite, PrivateCallStateRinging, "", PrivateCallActorCaller},
	{PrivateCallFromNone, PrivateCallEventInviteUnavailable, PrivateCallStateUnavailable, "", PrivateCallActorCaller},
	{string(PrivateCallStateRinging), PrivateCallEventAccept, PrivateCallStateAccepted, "", PrivateCallActorCallee},
	{string(PrivateCallStateRinging), PrivateCallEventDecline, PrivateCallStateDeclined, "", PrivateCallActorCallee},
	{string(PrivateCallStateRinging), PrivateCallEventCancel, PrivateCallStateCancelled, "", PrivateCallActorCaller},
	{string(PrivateCallStateRinging), PrivateCallEventRingExpired, PrivateCallStateMissed, "", PrivateCallActorServer},
	{string(PrivateCallStateAccepted), PrivateCallEventJoined, PrivateCallStateActive, "", PrivateCallActorServer},
	{string(PrivateCallStateAccepted), PrivateCallEventJoinExpired, PrivateCallStateFailed, PrivateCallReasonJoinTimeout, PrivateCallActorServer},
	{string(PrivateCallStateAccepted), PrivateCallEventHangup, PrivateCallStateEnded, PrivateCallReasonHangup, PrivateCallActorEither},
	{string(PrivateCallStateActive), PrivateCallEventHangup, PrivateCallStateEnded, PrivateCallReasonHangup, PrivateCallActorEither},
	{string(PrivateCallStateActive), PrivateCallEventGraceExpired, PrivateCallStateEnded, PrivateCallReasonGone, PrivateCallActorServer},
	{string(PrivateCallStateRinging), PrivateCallEventRevoked, PrivateCallStateCancelled, "", PrivateCallActorServer},
	{string(PrivateCallStateAccepted), PrivateCallEventRevoked, PrivateCallStateEnded, "", PrivateCallActorServer},
	{string(PrivateCallStateActive), PrivateCallEventRevoked, PrivateCallStateEnded, "", PrivateCallActorServer},
	{string(PrivateCallStateRinging), PrivateCallEventRestart, PrivateCallStateFailed, PrivateCallReasonServerRestart, PrivateCallActorServer},
	{string(PrivateCallStateAccepted), PrivateCallEventRestart, PrivateCallStateFailed, PrivateCallReasonServerRestart, PrivateCallActorServer},
	{string(PrivateCallStateActive), PrivateCallEventRestart, PrivateCallStateFailed, PrivateCallReasonServerRestart, PrivateCallActorServer},
}

// NextPrivateCall returns the table row for an event in a state, and false
// when the table has none: the event is refused in that state. A terminal
// state never has one.
func NextPrivateCall(from, event string) (PrivateCallTransition, bool) {
	for _, t := range PrivateCallTransitions {
		if t.From == from && t.Event == event {
			return t, true
		}
	}
	return PrivateCallTransition{}, false
}

// PrivateCallV1 is one attempt as one of its two people sees it. The pair of
// people is the same for both; Role and SelectedHere are the reader's own.
//
// The id is opaque. The revision starts at 1 when the attempt is created and
// rises by exactly 1 on every transition (compare-and-swap: an action names
// the revision it saw). A copy with a lower revision of the same id is stale
// and a client drops it; the server's copy always wins, and a client never
// infers a state locally.
//
// Selected devices. Each side has at most one selected endpoint, a
// credential: the caller's is the credential that invited, the callee's the
// one whose accept won. Credential ids are never sent, to either side;
// SelectedHere says only whether the credential reading this document is
// its own side's selected endpoint, which is how a second tab or a second
// device knows not to join. Only the selected credentials are ever issued a
// join grant.
//
// What it does not carry: a room, a token, a URL, a camera or share flag, the
// other person's devices, or why an access change ended it.
type PrivateCallV1 struct {
	// Schema is the document discriminator; always PrivateCallSchemaV1.
	Schema string `json:"schema"`
	// ID is the opaque attempt id.
	ID string `json:"id"`
	// ConversationID is the DM conversation the attempt belongs to.
	ConversationID int64 `json:"conversation_id"`
	// CallerID and CalleeID are the two people, distinct.
	CallerID int64 `json:"caller_id"`
	CalleeID int64 `json:"callee_id"`
	// Role is the reader's side.
	Role PrivateCallRole `json:"role"`
	// State is where the attempt is.
	State PrivateCallState `json:"state"`
	// Reason is why it ended or failed, for those states; omitted otherwise,
	// and omitted when an access change ended it.
	Reason PrivateCallReason `json:"reason,omitempty"`
	// Revision is 1 at creation and rises by 1 on every transition.
	Revision int64 `json:"revision"`
	// CreatedAt is when the invite was made.
	CreatedAt Timestamp `json:"created_at"`
	// RingDeadline is when it stops ringing: CreatedAt plus
	// PrivateCallRingSeconds. Present only while ringing.
	RingDeadline *Timestamp `json:"ring_deadline,omitempty"`
	// JoinDeadline is when both endpoints must be in the room: acceptance plus
	// PrivateCallJoinSeconds. Present only while accepted.
	JoinDeadline *Timestamp `json:"join_deadline,omitempty"`
	// EndedAt is when it became terminal. Present exactly when it is terminal.
	EndedAt *Timestamp `json:"ended_at,omitempty"`
	// SelectedHere is whether the credential reading this is its side's
	// selected endpoint. Always false for a terminal attempt: nobody joins one.
	SelectedHere bool `json:"selected_here"`
}

// Validate reports whether the document is structurally well-formed. It
// checks one document; ValidatePrivateCallStep checks that two are one
// attempt's consecutive revisions.
func (c PrivateCallV1) Validate() error {
	if c.Schema != PrivateCallSchemaV1 {
		return fmt.Errorf("schema: private call schema must be %q, got %q", PrivateCallSchemaV1, c.Schema)
	}
	if !validDMToken(c.ID, MaxPrivateCallIDBytes) || len(c.ID) < MinPrivateCallIDBytes {
		return fmt.Errorf("schema: private call id must be %d to %d URL-safe characters", MinPrivateCallIDBytes, MaxPrivateCallIDBytes)
	}
	if c.ConversationID <= 0 {
		return errors.New("schema: private call conversation_id must be positive")
	}
	if c.CallerID <= 0 || c.CalleeID <= 0 || c.CallerID == c.CalleeID {
		return errors.New("schema: private call caller_id and callee_id must be distinct positive ids")
	}
	if !c.Role.Valid() {
		return errors.New("schema: private call role is not one of caller, callee")
	}
	if !c.State.Valid() {
		return errors.New("schema: private call state is not a known state")
	}
	if err := c.validateReason(); err != nil {
		return err
	}
	if c.CreatedAt.IsZero() {
		return errors.New("schema: private call created_at is required")
	}
	if err := c.validateRevision(); err != nil {
		return err
	}
	if err := c.validateDeadlines(); err != nil {
		return err
	}
	if c.State == PrivateCallStateUnavailable && c.Role != PrivateCallRoleCaller {
		return errors.New("schema: an unavailable call is the caller's alone")
	}
	if c.SelectedHere {
		if c.State.Terminal() {
			return errors.New("schema: a terminal private call has no selected endpoint")
		}
		if c.Role == PrivateCallRoleCallee && c.State == PrivateCallStateRinging {
			return errors.New("schema: a ringing call has no selected callee endpoint")
		}
	}
	return nil
}

func (c PrivateCallV1) validateReason() error {
	switch c.State {
	case PrivateCallStateEnded:
		if c.Reason != "" && c.Reason != PrivateCallReasonHangup && c.Reason != PrivateCallReasonGone {
			return errors.New("schema: an ended private call's reason must be hangup, gone, or absent")
		}
	case PrivateCallStateFailed:
		if c.Reason != PrivateCallReasonJoinTimeout && c.Reason != PrivateCallReasonServerRestart {
			return errors.New("schema: a failed private call's reason must be join_timeout or server_restart")
		}
	default:
		if c.Reason != "" {
			return fmt.Errorf("schema: a %s private call carries no reason", c.State)
		}
	}
	return nil
}

func (c PrivateCallV1) validateRevision() error {
	min := int64(2)
	switch c.State {
	case PrivateCallStateUnavailable, PrivateCallStateRinging:
		if c.Revision != 1 {
			return fmt.Errorf("schema: a %s private call's revision must be 1", c.State)
		}
		return nil
	case PrivateCallStateActive:
		min = 3
	case PrivateCallStateEnded:
		// From accepted or active: revision 3 at the least.
		min = 3
	}
	if c.Revision < min {
		return fmt.Errorf("schema: a %s private call's revision must be at least %d", c.State, min)
	}
	return nil
}

func (c PrivateCallV1) validateDeadlines() error {
	for _, d := range []struct {
		name string
		ts   *Timestamp
		want bool
	}{
		{"ring_deadline", c.RingDeadline, c.State == PrivateCallStateRinging},
		{"join_deadline", c.JoinDeadline, c.State == PrivateCallStateAccepted},
		{"ended_at", c.EndedAt, c.State.Terminal()},
	} {
		if (d.ts != nil) != d.want {
			if d.want {
				return fmt.Errorf("schema: a %s private call requires %s", c.State, d.name)
			}
			return fmt.Errorf("schema: a %s private call must not carry %s", c.State, d.name)
		}
		if d.ts != nil && d.ts.Time().Before(c.CreatedAt.Time()) {
			return fmt.Errorf("schema: private call %s must not be before created_at", d.name)
		}
	}
	if c.RingDeadline != nil {
		want := c.CreatedAt.Time().Add(PrivateCallRingSeconds * time.Second)
		if !c.RingDeadline.Time().Equal(want) {
			return fmt.Errorf("schema: private call ring_deadline must be created_at plus %d seconds", PrivateCallRingSeconds)
		}
	}
	if c.JoinDeadline != nil {
		max := c.CreatedAt.Time().Add((PrivateCallRingSeconds + PrivateCallJoinSeconds) * time.Second)
		if c.JoinDeadline.Time().After(max) {
			return errors.New("schema: private call join_deadline is later than the ring and join times allow")
		}
	}
	return nil
}

// ValidatePrivateCallStep reports whether next is a legal successor of prev:
// the same attempt as seen by the same credential, revision exactly one
// higher, and a transition the table has, with the reason it gives. The
// caller's selection is fixed for the life of the attempt. A callee can
// become selected only on acceptance. Terminal documents clear selection.
func ValidatePrivateCallStep(prev, next PrivateCallV1) error {
	if err := prev.Validate(); err != nil {
		return fmt.Errorf("previous: %w", err)
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("next: %w", err)
	}
	if prev.State.Terminal() {
		return fmt.Errorf("schema: private call is %s, a final state; nothing follows it", prev.State)
	}
	if prev.ID != next.ID || prev.ConversationID != next.ConversationID ||
		prev.CallerID != next.CallerID || prev.CalleeID != next.CalleeID || prev.Role != next.Role ||
		!prev.CreatedAt.Time().Equal(next.CreatedAt.Time()) {
		return errors.New("schema: private call steps must be of one attempt and one reader")
	}
	if next.Revision != prev.Revision+1 {
		return errors.New("schema: private call revision must rise by exactly 1")
	}
	selectsCallee := prev.Role == PrivateCallRoleCallee &&
		prev.State == PrivateCallStateRinging && next.State == PrivateCallStateAccepted
	if prev.SelectedHere != next.SelectedHere && !next.State.Terminal() && !selectsCallee {
		return errors.New("schema: private call selected_here cannot change for this transition")
	}
	for _, t := range PrivateCallTransitions {
		if t.From == string(prev.State) && t.To == next.State && t.Reason == next.Reason {
			return nil
		}
	}
	return fmt.Errorf("schema: private call may not go from %s to %s", prev.State, next.State)
}

// InvitePrivateCallRequestV1 is the body of POST
// /v1/dms/{conversation}/calls. The caller is the credential's person and
// the callee the conversation's other person. ActionID makes the invite
// idempotent: the client chooses it once per invite and reuses it on every
// retry. Within PrivateCallActionWindowSeconds, an authorized retry with the
// same credential, action id and conversation returns the attempt associated
// with the original invite, with Replayed set and no new fan-out.
// See PrivateCallResponseV1.InviteHTTPStatus for the success status.
type InvitePrivateCallRequestV1 struct {
	ActionID string `json:"action_id"`
}

// Validate reports whether the request is well-formed.
func (r InvitePrivateCallRequestV1) Validate() error { return validateCallActionID(r.ActionID) }

// PrivateCallActionRequestV1 is the body of the four action routes. Revision
// is the attempt's revision the client last saw: the change is a
// compare-and-swap on it. Within PrivateCallActionWindowSeconds, an
// authorized request matching the credential, action id, route, call id and
// submitted revision of a successful action replays with HTTP 200 and the
// current attempt, with Replayed set. It performs no transition or fan-out.
// Reusing the same credential's action id with a different route, target or
// revision is ErrorCodeCallActionConflict. After the replay and action id
// checks, a stale request gets HTTP 409 ErrorCodeCallStateChanged with the
// current attempt, even if the requested state has already been reached.
//
// For competing accepts at revision 1, the winning credential's exact
// retry gets 200 with selected_here true while the call is still accepted.
// An accept at revision 1 from another credential gets 409 with the accepted
// attempt at revision 2 and selected_here false, even with the same action
// id. That device stops ringing and does not request a join grant.
type PrivateCallActionRequestV1 struct {
	ActionID string `json:"action_id"`
	Revision int64  `json:"revision"`
}

// Validate reports whether the request is well-formed.
func (r PrivateCallActionRequestV1) Validate() error {
	if err := validateCallActionID(r.ActionID); err != nil {
		return err
	}
	if r.Revision <= 0 {
		return errors.New("schema: private call action revision must be positive")
	}
	return nil
}

func validateCallActionID(id string) error {
	if !validDMToken(id, MaxPrivateCallActionIDBytes) {
		return fmt.Errorf("schema: private call action_id must be 1 to %d URL-safe characters", MaxPrivateCallActionIDBytes)
	}
	return nil
}

// PrivateCallResponseV1 is the answer to an invite or an action: the attempt
// as it now is. A fresh invite that starts ringing answers HTTP 201. An
// unavailable invite answers HTTP 200 without a ringing attempt the caller
// can act on. Glare, an action success and a replay also answer HTTP 200.
//
// Glare: when the caller invites a person whose own invite to the caller is
// ringing, no attempt is made. The answer is that other attempt, as the
// caller's incoming call (Role callee, state ringing), with Glare set, and
// the caller may accept it. Neither side is shown "busy".
type PrivateCallResponseV1 struct {
	Call PrivateCallV1 `json:"call"`
	// Replayed is true for an exact retry of a successful request by the
	// same credential within the action window.
	Replayed bool `json:"replayed,omitempty"`
	// Glare is true when Call is the other person's ringing invite to the
	// caller, returned in place of a new attempt.
	Glare bool `json:"glare,omitempty"`
}

// InviteHTTPStatus returns the HTTP status for a successful invite response.
func (r PrivateCallResponseV1) InviteHTTPStatus() int {
	if r.Call.State == PrivateCallStateRinging && !r.Replayed && !r.Glare {
		return http.StatusCreated
	}
	return http.StatusOK
}

// Validate reports whether the response is well-formed.
func (r PrivateCallResponseV1) Validate() error {
	if err := r.Call.Validate(); err != nil {
		return err
	}
	if r.Glare && (r.Call.Role != PrivateCallRoleCallee || r.Call.State != PrivateCallStateRinging) {
		return errors.New("schema: a glare answer must be a ringing incoming call")
	}
	return nil
}

// PrivateCallConflictV1 is the body of 409 ErrorCodeCallStateChanged: the
// refusal and the attempt as it is now, so the client follows the server
// instead of guessing. It names the attempt of the two people only.
type PrivateCallConflictV1 struct {
	Code    string        `json:"code"`
	Message string        `json:"message"`
	Call    PrivateCallV1 `json:"call"`
}

// Validate reports whether the body is well-formed.
func (r PrivateCallConflictV1) Validate() error {
	if r.Code != ErrorCodeCallStateChanged {
		return fmt.Errorf("schema: private call conflict code must be %q", ErrorCodeCallStateChanged)
	}
	if r.Message == "" {
		return errors.New("schema: private call conflict message is required")
	}
	if r.Call.State == PrivateCallStateUnavailable {
		return errors.New("schema: an unavailable call has no action or session conflict response")
	}
	return r.Call.Validate()
}

// PrivateCallSessionV1 is the answer to POST /v1/calls/{call}/session, the
// explicit media join: the one place a room name and a token appear. It is
// issued only when the attempt is accepted or active and the caller's
// credential is its side's selected endpoint (ErrorCodeCallWrongDevice
// otherwise; a terminal or not yet accepted attempt is 409
// ErrorCodeCallStateChanged), after the voice entitlement check, and never
// by any other route, frame or snapshot. The token's lifetime is the
// channel voice grant's (15 s): a client asks immediately before connecting
// and asks again to reconnect, within the grace. Opening a conversation,
// receiving a ring and answering start no capture; joining is this call.
// Nothing in a grant is audited or logged.
type PrivateCallSessionV1 struct {
	// Schema is the document discriminator; always PrivateCallSessionSchemaV1.
	Schema string `json:"schema"`
	// CallID and Revision name the attempt and the revision the grant was
	// issued at.
	CallID   string `json:"call_id"`
	Revision int64  `json:"revision"`
	// LivekitURL is the ws:// or wss:// address to connect to.
	LivekitURL string `json:"livekit_url"`
	// Identity is the caller's LiveKit participant identity.
	Identity string `json:"identity"`
	// Room is this attempt's room, fresh for it, never reused.
	Room string `json:"room"`
	// Token is the room-scoped LiveKit access token; opaque, never logged.
	Token string `json:"token"`
	// ExpiresAt is the token's expiry; "connect now", not a deadline.
	ExpiresAt Timestamp `json:"expires_at"`
}

// Validate reports whether the grant is well-formed.
func (s PrivateCallSessionV1) Validate() error {
	if s.Schema != PrivateCallSessionSchemaV1 {
		return fmt.Errorf("schema: private call session schema must be %q, got %q", PrivateCallSessionSchemaV1, s.Schema)
	}
	if !validDMToken(s.CallID, MaxPrivateCallIDBytes) || len(s.CallID) < MinPrivateCallIDBytes {
		return errors.New("schema: private call session call_id is not a well-formed attempt id")
	}
	if s.Revision < 2 {
		return errors.New("schema: private call session revision must be at least 2: a grant follows acceptance")
	}
	if s.LivekitURL == "" || s.Identity == "" || s.Room == "" || s.Token == "" {
		return errors.New("schema: private call session livekit_url, identity, room and token are required")
	}
	if s.ExpiresAt.IsZero() {
		return errors.New("schema: private call session expires_at is required")
	}
	return nil
}

// PrivateCallRingingV1 is the stream frame that a call is ringing for the
// reader: sent to every one of the callee's sockets at invite, and only to
// them. The attempt is in state ringing with Role callee. A client rings,
// and stops on PrivateCallStopRingingV1 or a PrivateCallStateV1 that leaves
// ringing. It never says anything about the caller's side beyond the id.
type PrivateCallRingingV1 struct {
	// Schema is the frame discriminator; always PrivateCallRingingSchemaV1.
	Schema string        `json:"schema"`
	Call   PrivateCallV1 `json:"call"`
}

// Validate reports whether the frame is well-formed.
func (f PrivateCallRingingV1) Validate() error {
	if f.Schema != PrivateCallRingingSchemaV1 {
		return fmt.Errorf("schema: private call ringing schema must be %q, got %q", PrivateCallRingingSchemaV1, f.Schema)
	}
	if err := f.Call.Validate(); err != nil {
		return err
	}
	if f.Call.State != PrivateCallStateRinging || f.Call.Role != PrivateCallRoleCallee {
		return errors.New("schema: a ringing frame carries a ringing incoming call")
	}
	return nil
}

// PrivateCallStopRingingV1 is the stream frame to a callee's sockets that
// were ringing and must stop: another of their devices accepted or declined,
// the caller cancelled, or it was missed. State is the state the attempt
// moved to. It is a hint that dismisses the ring at once; the full change
// follows (or came) as a PrivateCallStateV1. A socket that did not send the
// accept never joins by itself.
type PrivateCallStopRingingV1 struct {
	// Schema is the frame discriminator; always PrivateCallStopRingingSchemaV1.
	Schema string `json:"schema"`
	// CallID and Revision name the attempt and the revision that ended the
	// ringing.
	CallID   string           `json:"call_id"`
	Revision int64            `json:"revision"`
	State    PrivateCallState `json:"state"`
}

// Validate reports whether the frame is well-formed: State is one a ringing
// attempt can move to.
func (f PrivateCallStopRingingV1) Validate() error {
	if f.Schema != PrivateCallStopRingingSchemaV1 {
		return fmt.Errorf("schema: private call stop ringing schema must be %q, got %q", PrivateCallStopRingingSchemaV1, f.Schema)
	}
	if !validDMToken(f.CallID, MaxPrivateCallIDBytes) || len(f.CallID) < MinPrivateCallIDBytes {
		return errors.New("schema: private call stop ringing call_id is not a well-formed attempt id")
	}
	if f.Revision < 2 {
		return errors.New("schema: private call stop ringing revision must be at least 2")
	}
	switch f.State {
	case PrivateCallStateAccepted, PrivateCallStateDeclined, PrivateCallStateCancelled,
		PrivateCallStateMissed, PrivateCallStateFailed:
		return nil
	default:
		return errors.New("schema: private call stop ringing state is not one a ringing call moves to")
	}
}

// PrivateCallStateV1 is the stream frame for a transition of an attempt that
// rang. The server checks current access for each recipient at fan-out and
// builds the frame from the receiving credential's view. The acting device
// can receive it too. An unavailable attempt never produces a frame, even
// for the inviter. A client replaces what it holds for that id when Revision
// is higher, drops a frame at or below the revision it holds, and notifies
// no one on a terminal state beyond what the product says. A frame for an
// attempt the client has never held is a new one (a second device that was
// signed out when it started): it is shown by its state.
type PrivateCallStateV1 struct {
	// Schema is the frame discriminator; always PrivateCallStateSchemaV1.
	Schema string        `json:"schema"`
	Call   PrivateCallV1 `json:"call"`
}

// Validate reports whether the frame is well-formed.
func (f PrivateCallStateV1) Validate() error {
	if f.Schema != PrivateCallStateSchemaV1 {
		return fmt.Errorf("schema: private call state schema must be %q, got %q", PrivateCallStateSchemaV1, f.Schema)
	}
	if f.Call.State == PrivateCallStateUnavailable {
		return errors.New("schema: an unavailable call cannot appear in a state frame")
	}
	return f.Call.Validate()
}

// PrivateCallSnapshotV1 is the reconnect snapshot: the body of GET
// /v1/calls/current and the first call frame on every (re)connect of the
// per-person events stream, right after the DM snapshot (DMSnapshotV1). It
// is a document of its own and DMSnapshotV1 is unchanged: a call has a
// revision, not a message high-water, and a client that reads only DMs
// skips it.
//
// Call is the reader's one non-terminal attempt, if any (a person has at
// most one); omitted when there is none. A client replaces whatever call
// state it holds with the snapshot, then applies frames after it by
// revision: a frame for the snapshot's id at or below its revision is a
// duplicate. If a previously held call is absent, the client clears its live
// UI and stops ringing or joining. It can show the final state from a visible
// history entry for that id. With no such entry, it uses the generic
// "Call ended" status if a status is shown. For example, when an outgoing
// ring expires during a disconnect, only the callee has a missed history
// entry. The caller must not infer a reason or create a missed entry.
type PrivateCallSnapshotV1 struct {
	Schema string         `json:"schema"`
	Call   *PrivateCallV1 `json:"call,omitempty"`
	// TakenAt is when the snapshot was taken.
	TakenAt Timestamp `json:"taken_at"`
}

// Validate reports whether the snapshot is well-formed: a present call is not
// terminal.
func (s PrivateCallSnapshotV1) Validate() error {
	if s.Schema != PrivateCallSnapshotSchemaV1 {
		return fmt.Errorf("schema: private call snapshot schema must be %q, got %q", PrivateCallSnapshotSchemaV1, s.Schema)
	}
	if s.TakenAt.IsZero() {
		return errors.New("schema: private call snapshot taken_at is required")
	}
	if s.Call != nil {
		if err := s.Call.Validate(); err != nil {
			return err
		}
		if s.Call.State.Terminal() {
			return errors.New("schema: a private call snapshot lists a live call, not a terminal one")
		}
	}
	return nil
}

// ListPrivateCallsResponseV1 is the body of GET
// /v1/dms/{conversation}/calls?cursor=: one page of the conversation's
// finished calls as the reader sees them, newest first. It is the missed
// and call history, and shows the final state of listed attempts. Calls is
// never null and holds at most MaxPrivateCallHistoryPage.
//
// What is listed: attempts that rang and are terminal: declined, cancelled,
// missed, ended and failed. An unavailable attempt is never listed: the
// caller saw it in the invite's answer and the callee was never told. A
// missed call is the callee's entry alone; the caller's list does not carry
// it. An access change shows as a plain ended or cancelled with no reason.
// NextCursor is opaque and present only when there may be more.
type ListPrivateCallsResponseV1 struct {
	Calls      []PrivateCallV1 `json:"calls"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// Validate reports whether the page is well-formed.
func (r ListPrivateCallsResponseV1) Validate() error {
	if r.Calls == nil {
		return errors.New("schema: private calls must be a list, not null")
	}
	if len(r.Calls) > MaxPrivateCallHistoryPage {
		return fmt.Errorf("schema: private calls must be at most %d", MaxPrivateCallHistoryPage)
	}
	for i, c := range r.Calls {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("schema: private calls[%d]: %w", i, err)
		}
		if !c.State.Terminal() || c.State == PrivateCallStateUnavailable {
			return fmt.Errorf("schema: private calls[%d] is not a finished call that rang", i)
		}
		if c.State == PrivateCallStateMissed && c.Role != PrivateCallRoleCallee {
			return fmt.Errorf("schema: private calls[%d] is a missed call listed to the caller", i)
		}
		if c.ConversationID != r.Calls[0].ConversationID {
			return fmt.Errorf("schema: private calls[%d] is of another conversation", i)
		}
		if i > 0 && c.CreatedAt.Time().After(r.Calls[i-1].CreatedAt.Time()) {
			return fmt.Errorf("schema: private calls[%d] is out of order", i)
		}
	}
	if r.NextCursor != "" && !validDMToken(r.NextCursor, MaxDMCursorBytes) {
		return fmt.Errorf("schema: private calls next_cursor must be 1 to %d URL-safe characters", MaxDMCursorBytes)
	}
	return nil
}
