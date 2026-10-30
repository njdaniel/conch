package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Voice control-plane wire shapes (issue #124, ADR-004; design note
// docs/design/voice-control-plane.md §4, §6, §8). conchd is the only authority
// on who may speak and who may hear; LiveKit only moves audio. These shapes
// are what conchd hands a client so it can join (the session response) and
// what it tells everyone about who is there (the presence document). All of
// them are new types; nothing published changes.
//
// One shape travels the other way: the transmit report (issue #179), which a
// client sends conchd when a press starts and stops. It was added later, also
// as a new type, and is near the end of this file. The screen share (issue
// #443) added the claim a client is given when it may share, and two optional
// fields of a participant in presence. Issue #487 added the last: why the
// server ended a share, an optional field of the sharer's own presence, and the
// parameter by which a release names the share it means.
//
// Room names and tokens are opaque strings. The schema checks that they are
// present, never what they contain: a token is a credential conchd signed and
// a room name is random, and neither has structure a client should depend on.

// VoicePresenceSchemaV1 is the versioned name identifying the v1 voice
// presence document on the wire. It is carried in VoicePresenceV1.Schema so
// the presence socket's reader can recognize the document without
// out-of-band context, and so that the document is never mistaken for a
// message envelope.
const VoicePresenceSchemaV1 = "conch.voice_presence.v1"

// Error codes returned by the voice endpoints in Error.Code. The package
// otherwise has no error-code constants (handlers write their codes as
// literals), so these live here next to the shapes they accompany.
const (
	// ErrorCodeVoiceNotConfigured: LiveKit is not configured on this conchd.
	// Only a caller who has already passed authentication and the channel
	// membership check sees it; a non-member gets the unknown-channel 404.
	ErrorCodeVoiceNotConfigured = "voice_not_configured"
	// ErrorCodeVoiceUnavailable: LiveKit is configured but could not be
	// reached, so no session can be issued right now.
	ErrorCodeVoiceUnavailable = "voice_unavailable"
	// ErrorCodeVoiceRequiresAuth: the caller is not a verified principal
	// (conchd runs with authentication off). Voice is never anonymous.
	ErrorCodeVoiceRequiresAuth = "voice_requires_auth"
	// ErrorCodeVoiceNoSession: the caller holds no session for this channel's
	// current room, so it has nothing to report a transmission in (HTTP 409).
	// The room was rotated or the session is otherwise gone; the client goes
	// back for a session, it does not retry the report.
	ErrorCodeVoiceNoSession = "voice_no_session"
	// ErrorCodeVoiceReportRateLimited: the caller sent more transmit reports
	// than the per-principal bound allows (HTTP 429). Neither the package nor
	// the server had a rate-limit code before this one, so it is named for
	// the one endpoint that has a bound rather than as a general code.
	ErrorCodeVoiceReportRateLimited = "voice_report_rate_limited"
	// ErrorCodeVoiceNotAllowed: the caller is a member of the channel but
	// holds no voice, because the invite it joined through did not grant it
	// (HTTP 403, issue #437). A non-member gets the unknown-channel 404
	// instead, so this answer reveals nothing to someone outside.
	ErrorCodeVoiceNotAllowed = "voice_not_allowed"
	// ErrorCodeVoiceShareBusy: another principal holds the room's screen
	// share, and a room has one share at a time (HTTP 409, issue #443). The
	// answer names nobody: who is sharing is in presence, for those who may
	// read it.
	ErrorCodeVoiceShareBusy = "voice_share_busy"
	// ErrorCodeVoiceShareStale: a release named a share (VoiceShareClaimParam)
	// that is no longer the caller's current one, and nothing was changed
	// (HTTP 409, issue #487). The share it meant has ended already; whatever
	// share the room has now, someone else's or a newer one of the caller's,
	// goes on.
	ErrorCodeVoiceShareStale = "voice_share_stale"
)

// VoiceRoomGrant is one room a voice session may join: the LiveKit room name,
// the token that admits the caller to it, what the token lets the caller do,
// and when it stops being accepted. The token is the gate; the room name is
// not a secret but is unguessable so a mis-set LiveKit cannot be probed.
//
// Audience says which audience the room carries, in the vocabulary of
// ADR-005: nil (omitted) is the whole channel, exactly as on messages. In V3
// every grant is channel-wide; a net audience arrives with V5. A principals
// audience is accepted because whisper mapping is undecided (ADR-004), not
// because a room for one exists today.
type VoiceRoomGrant struct {
	// Room is the LiveKit room name conchd created; opaque to clients.
	Room string `json:"room"`
	// Token is the signed LiveKit access token for this one room; opaque to
	// clients, never logged or audited.
	Token string `json:"token"`
	// CanPublish is whether the token lets the caller publish a microphone
	// track. False is a listen-only grant (a net monitor, from V5).
	CanPublish bool `json:"can_publish"`
	// ExpiresAt is the token's expiry (UTC, ms precision). It is deliberately
	// short, so clients ask for a session immediately before connecting.
	// LiveKit checks it only when a connection starts and allows about a
	// minute of leeway (design note §10), so treat it as "connect now", not as
	// a deadline to schedule against.
	ExpiresAt Timestamp `json:"expires_at"`
	// Audience is the audience the room carries; nil (omitted) means the
	// whole channel.
	Audience *Audience `json:"audience,omitempty"`
}

// Validate reports whether the grant is structurally well-formed: a room and
// a token are present, expires_at is set, and a present audience satisfies
// Audience.Validate. It does not look inside the room name or the token.
func (g VoiceRoomGrant) Validate() error {
	if g.Room == "" {
		return errors.New("schema: voice room grant room is required")
	}
	if g.Token == "" {
		return errors.New("schema: voice room grant token is required")
	}
	if g.ExpiresAt.IsZero() {
		return errors.New("schema: voice room grant expires_at is required")
	}
	if g.Audience != nil {
		if err := g.Audience.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// VoiceSessionResponseV1 is the body of POST /v1/channels/{channel}/voice/session:
// everything a client needs to connect. Like the net shapes it is a REST body
// with no schema-name field, versioned by the V1 type suffix.
//
// Rooms is always a JSON array with at least one grant: a session that
// admits the caller to no room is not issued, it is an error. In V3 there is
// exactly one grant, for the channel-wide room; with nets (V5) there is one
// per room the caller may hear, publishing or listen-only according to the
// caller's role.
type VoiceSessionResponseV1 struct {
	// LivekitURL is the ws:// or wss:// address the client connects to,
	// returned exactly as configured.
	LivekitURL string `json:"livekit_url"`
	// Identity is the caller's LiveKit participant identity, the same in
	// every room. One principal holds one own connection per room; a second
	// connection with the same identity displaces the first. A companion or
	// viewer under a derived identity (VoiceCompanionResponseV1) is folded
	// into the person, so the wire invariant of one participant entry per
	// principal stands.
	Identity string `json:"identity"`
	// Rooms lists one grant per room the caller may join.
	Rooms []VoiceRoomGrant `json:"rooms"`
	// CompanionIdentity is the identity conchd would issue the caller's
	// companion under in these rooms (p<id>.screen), present whenever a
	// session is issued (issue #933). A voice client that plays remote audio
	// never subscribes to a track of this participant: it is the person's own
	// screen share, and playing it back would only duplicate it. It is not a
	// credential and admits nobody to anything. Omitted by a server older than
	// this field, so a client reading an absent value needs no rule for it.
	CompanionIdentity string `json:"companion_identity,omitempty"`
}

// Validate reports whether the response is structurally well-formed: a
// non-empty address and identity, at least one well-formed grant, and no two
// grants for the same audience. It does
// not check that the address parses as a URL; the operator configured it and
// conchd returns it as is.
func (r VoiceSessionResponseV1) Validate() error {
	if r.LivekitURL == "" {
		return errors.New("schema: voice session livekit_url is required")
	}
	if r.Identity == "" {
		return errors.New("schema: voice session identity is required")
	}
	if r.CompanionIdentity != "" && r.CompanionIdentity == r.Identity {
		return errors.New("schema: voice session companion_identity must differ from identity")
	}
	if len(r.Rooms) == 0 {
		return errors.New("schema: voice session rooms must list at least one grant")
	}
	seen := make(map[string]struct{}, len(r.Rooms))
	for _, g := range r.Rooms {
		if err := g.Validate(); err != nil {
			return err
		}
		key := audienceKey(g.Audience)
		if _, dup := seen[key]; dup {
			return errors.New("schema: voice session lists more than one grant for the same audience")
		}
		seen[key] = struct{}{}
	}
	return nil
}

// audienceKey names an audience for the one-room-per-audience rule: there is
// one voice room for the whole channel, one per net, and (if whispers are ever
// mapped to rooms) one per set of principals. Two grants or two presence rooms
// with the same audience would leave a reader unable to say which is the
// channel's room. The key is for comparison only and never leaves the package.
func audienceKey(a *Audience) string {
	if a == nil {
		return "channel"
	}
	if a.Kind == AudienceKindNet {
		return "net:" + strconv.FormatInt(a.NetID, 10)
	}
	ids := slices.Clone(a.PrincipalIDs)
	slices.Sort(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return string(a.Kind) + ":" + strings.Join(parts, ",")
}

// VoiceParticipant is one principal connected to a voice room, as conchd last
// observed it. It names the principal, not a LiveKit identity, so a presence
// reader needs nothing but Conch's own vocabulary to understand it.
type VoiceParticipant struct {
	// PrincipalID references the connected principal.
	PrincipalID int64 `json:"principal_id"`
	// CanPublish is whether the participant may transmit in this room.
	CanPublish bool `json:"can_publish"`
	// Transmitting is whether the participant's microphone track was
	// published and unmuted on the last pass: whether they are talking.
	Transmitting bool `json:"transmitting"`
	// JoinedAt is when the participant connected (UTC, ms precision).
	JoinedAt Timestamp `json:"joined_at"`
	// Sharing is whether the participant holds the room's screen share
	// (issue #443): conchd granted it and it has not ended. It is what conchd
	// decided, not what LiveKit last showed, so it is true from the moment
	// the claim is answered, before any track is published. Omitted when
	// false, so a document with nobody sharing is what it was before.
	Sharing bool `json:"sharing,omitempty"`
	// SharingAudio is whether the share carries sound: the participant holds
	// the share and a screen-share audio track was published on the last
	// pass. Omitted when false.
	SharingAudio bool `json:"sharing_audio,omitempty"`
}

// Validate reports whether the participant is structurally well-formed. A
// participant who cannot publish cannot be transmitting or sharing: if
// presence ever said so, either the grant or the observation is wrong, and a
// reader must not be left to guess which. Sound of a share needs a share, so
// sharing_audio needs sharing.
func (p VoiceParticipant) Validate() error {
	if p.PrincipalID <= 0 {
		return fmt.Errorf("schema: voice participant principal_id must be positive, got %d", p.PrincipalID)
	}
	if p.Transmitting && !p.CanPublish {
		return fmt.Errorf("schema: voice participant %d cannot be transmitting without can_publish", p.PrincipalID)
	}
	if p.Sharing && !p.CanPublish {
		return fmt.Errorf("schema: voice participant %d cannot be sharing without can_publish", p.PrincipalID)
	}
	if p.SharingAudio && !p.Sharing {
		return fmt.Errorf("schema: voice participant %d cannot have sharing_audio without sharing", p.PrincipalID)
	}
	if p.JoinedAt.IsZero() {
		return errors.New("schema: voice participant joined_at is required")
	}
	return nil
}

// VoicePresenceRoom is one voice room as presence shows it: which audience it
// carries and who is in it. It deliberately has no room name and no token;
// the audience identifies the room to a reader, and nothing here helps anyone
// join it. The Audience is nil (omitted) for the channel-wide room.
//
// Participants is always a JSON array, ordered by principal_id; a room with
// nobody in it is an empty array.
type VoicePresenceRoom struct {
	// Audience is the audience the room carries; nil (omitted) means the
	// whole channel.
	Audience *Audience `json:"audience,omitempty"`
	// Participants lists every principal connected to the room.
	Participants []VoiceParticipant `json:"participants"`
}

// Validate reports whether the room is structurally well-formed: a present
// audience satisfies Audience.Validate, every participant is well-formed, no
// principal is listed twice, and at most one participant is sharing. One
// principal holds one own connection per room (a second device displaces the
// first; a companion or viewer under a derived identity is folded into the
// person, issue #933), and a room has one screen share at a time, so either duplicate is a
// conchd bug, not a state. Order is not checked.
func (r VoicePresenceRoom) Validate() error {
	if r.Audience != nil {
		if err := r.Audience.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[int64]struct{}, len(r.Participants))
	sharer := int64(0)
	for _, p := range r.Participants {
		if err := p.Validate(); err != nil {
			return err
		}
		if _, dup := seen[p.PrincipalID]; dup {
			return fmt.Errorf("schema: voice room lists principal %d more than once", p.PrincipalID)
		}
		seen[p.PrincipalID] = struct{}{}
		if p.Sharing {
			if sharer != 0 {
				return fmt.Errorf("schema: voice room lists principals %d and %d as sharing; a room has one share at a time", sharer, p.PrincipalID)
			}
			sharer = p.PrincipalID
		}
	}
	return nil
}

// VoicePresenceV1 is the whole-state voice presence snapshot for one channel:
// the body of GET /v1/channels/{channel}/voice and the document streamed on
// the presence socket whenever it changes. A snapshot is the whole state, not
// a delta, so a reader that misses one loses nothing.
//
// A reader of presence learns who is connected and who is talking, and
// nothing that helps join a room: no field of this document or of the types
// it embeds carries a token or a room name, and voice_test.go asserts that.
//
// Rooms is always a JSON array of the rooms the caller may see, and is empty
// unless voice is both configured and available: the design does not report
// participants last seen while LiveKit cannot be reached.
type VoicePresenceV1 struct {
	// Schema is the document discriminator; always VoicePresenceSchemaV1.
	Schema string `json:"schema"`
	// ChannelID references the channel the snapshot describes.
	ChannelID int64 `json:"channel_id"`
	// Configured is whether this conchd has LiveKit configured at all.
	Configured bool `json:"configured"`
	// Available is whether LiveKit answered on the last pass. Always false
	// when Configured is false.
	Available bool `json:"available"`
	// Rooms lists the rooms the caller may see with their participants.
	Rooms []VoicePresenceRoom `json:"rooms"`
	// ShareEnded is why the reader's own last screen share ended (issue
	// #487). It is in the document sent to the person who shared, and in no
	// one else's: the same snapshot read by anyone else omits it, so their
	// document is what it was before. It is kept until that person claims a
	// share again, so it may describe a share that ended some time ago; a
	// reader matches its started_at with the claim it held
	// (VoiceShareV1.StartedAt). It never names who ended the share.
	ShareEnded *VoiceShareEnded `json:"share_ended,omitempty"`
}

// Validate reports whether the snapshot is structurally well-formed: the v1
// schema name, a positive channel_id, every room well-formed, no two rooms for
// the same audience, the configured/available/rooms states consistent with
// each other, and a present share_ended well-formed, on a server with voice
// configured.
//
// An unknown audience kind in any room fails the whole document, as it does
// for a message (ADR-005: an audience a reader does not understand must never
// widen what it shows). A new audience kind is therefore a new version of
// these shapes, not an addition to this one. Not
// configured implies not available, and not available implies no rooms.
func (p VoicePresenceV1) Validate() error {
	if p.Schema != VoicePresenceSchemaV1 {
		return fmt.Errorf("schema: voice presence schema must be %q, got %q", VoicePresenceSchemaV1, p.Schema)
	}
	if p.ChannelID <= 0 {
		return fmt.Errorf("schema: voice presence channel_id must be positive, got %d", p.ChannelID)
	}
	if !p.Configured && p.Available {
		return errors.New("schema: voice presence cannot be available when not configured")
	}
	if !p.Available && len(p.Rooms) != 0 {
		return errors.New("schema: voice presence must list no rooms when not available")
	}
	seen := make(map[string]struct{}, len(p.Rooms))
	for _, r := range p.Rooms {
		if err := r.Validate(); err != nil {
			return err
		}
		key := audienceKey(r.Audience)
		if _, dup := seen[key]; dup {
			return errors.New("schema: voice presence lists more than one room for the same audience")
		}
		seen[key] = struct{}{}
	}
	if p.ShareEnded != nil {
		// A share needs voice. It does not need LiveKit to be reachable
		// now: a share stopped during an outage has ended all the same.
		if !p.Configured {
			return errors.New("schema: voice presence cannot carry share_ended when not configured")
		}
		if err := p.ShareEnded.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// VoiceShareEndReason is why a screen share ended: the reason conchd records
// on voice_share_stopped. The vocabulary is closed and case-sensitive; a value
// outside it fails validation, so a new reason is a new version of the
// presence document, as a new audience kind is.
type VoiceShareEndReason string

// Voice share end reasons (docs/design/voice-control-plane.md §12).
const (
	// VoiceShareEndSelf: the sharer stopped it.
	VoiceShareEndSelf VoiceShareEndReason = "self"
	// VoiceShareEndOperator: an operator stopped it. Which one is not said.
	VoiceShareEndOperator VoiceShareEndReason = "operator"
	// VoiceShareEndLeft: the sharer was no longer in the room.
	VoiceShareEndLeft VoiceShareEndReason = "left"
	// VoiceShareEndAccessLost: the sharer lost their place in the channel or
	// their voice.
	VoiceShareEndAccessLost VoiceShareEndReason = "access_lost"
	// VoiceShareEndRotated: the room was replaced by a new one, which drops
	// its share.
	VoiceShareEndRotated VoiceShareEndReason = "rotated"
	// VoiceShareEndIdle: nothing was shared for a while: no screen-share
	// track soon enough after the claim, or none any more.
	VoiceShareEndIdle VoiceShareEndReason = "idle"
)

// Valid reports whether r is a recognized end reason.
func (r VoiceShareEndReason) Valid() bool {
	switch r {
	case VoiceShareEndSelf, VoiceShareEndOperator, VoiceShareEndLeft, VoiceShareEndAccessLost, VoiceShareEndRotated, VoiceShareEndIdle:
		return true
	default:
		return false
	}
}

// VoiceShareEnded is why the reader's own screen share ended, carried in
// their presence alone (VoicePresenceV1.ShareEnded, issue #487). It names no
// principal: the reader is the sharer, and who stopped it is not told.
type VoiceShareEnded struct {
	// Reason is why the share ended.
	Reason VoiceShareEndReason `json:"reason"`
	// StartedAt is the started_at of the share that ended, as its claim
	// answered it (VoiceShareV1.StartedAt): which share this was.
	StartedAt Timestamp `json:"started_at"`
	// EndedAt is when conchd ended it (UTC, ms precision).
	EndedAt Timestamp `json:"ended_at"`
}

// Validate reports whether the end is structurally well-formed: a known
// reason, both times, and an end no earlier than the start.
func (e VoiceShareEnded) Validate() error {
	if !e.Reason.Valid() {
		return fmt.Errorf("schema: voice share end reason %q is not one of self, operator, left, access_lost, rotated, idle", e.Reason)
	}
	if e.StartedAt.IsZero() {
		return errors.New("schema: voice share end started_at is required")
	}
	if e.EndedAt.IsZero() {
		return errors.New("schema: voice share end ended_at is required")
	}
	if e.EndedAt.Time().Before(e.StartedAt.Time()) {
		return errors.New("schema: voice share end ended_at is before started_at")
	}
	return nil
}

// VoiceShareV1 is the body of a successful POST
// /v1/channels/{channel}/voice/share (issue #443; design note
// docs/design/voice-control-plane.md §12): the room's screen share is now the
// caller's, and LiveKit lets the caller's connection publish a screen and its
// sound. Like the session response it is a REST body with no schema-name
// field, versioned by the V1 type suffix.
//
// It carries no room name and no token: the share is granted to the
// connection the caller already has, so nothing new is needed to publish.
type VoiceShareV1 struct {
	// PrincipalID is the principal who holds the share: the caller.
	PrincipalID int64 `json:"principal_id"`
	// StartedAt is when conchd granted the share (UTC, ms precision). Asking
	// again while holding it answers with the same time.
	StartedAt Timestamp `json:"started_at"`
	// Audio is whether the share may carry sound: whether LiveKit lets the
	// connection publish a screen-share audio track as well as the screen.
	// Always written out, so a reader never has to know that an omitted
	// value means false.
	Audio bool `json:"audio"`
}

// Validate reports whether the share is structurally well-formed: a positive
// principal_id and a started_at.
func (s VoiceShareV1) Validate() error {
	if s.PrincipalID <= 0 {
		return fmt.Errorf("schema: voice share principal_id must be positive, got %d", s.PrincipalID)
	}
	if s.StartedAt.IsZero() {
		return errors.New("schema: voice share started_at is required")
	}
	return nil
}

// VoiceShareClaimParam is the optional query parameter of DELETE
// /v1/channels/{channel}/voice/share by which a release names the share it
// means (issue #487): the started_at of the caller's own claim, as
// VoiceShareV1 gave it. With it, the request ends that share only if it is
// still the room's share and the caller's; otherwise it is refused 409
// voice_share_stale and changes nothing, so a release sent late cannot end a
// newer share, least of all someone else's. Without it the request is what it
// always was: the sharer's stop, or an operator's stop of whoever shares.
const VoiceShareClaimParam = "started_at"

// ParseVoiceShareClaim reads the value of VoiceShareClaimParam: an RFC 3339
// timestamp, normalized as every wire timestamp is (UTC, millisecond
// precision), so that it compares equal to the started_at it was copied from.
func ParseVoiceShareClaim(s string) (Timestamp, error) {
	return parseTimestamp(s)
}

// VoiceTransmitState is what a transmit report says about the caller's own
// transmission: that it began or that it ended. The vocabulary is closed and
// case-sensitive; a value outside it fails validation, so a report conchd does
// not understand is never recorded as either.
type VoiceTransmitState string

// Voice transmit states.
const (
	// VoiceTransmitStateStarted reports that the caller began transmitting
	// (the push-to-talk gate opened).
	VoiceTransmitStateStarted VoiceTransmitState = "started"
	// VoiceTransmitStateStopped reports that the caller stopped transmitting
	// (the gate shut).
	VoiceTransmitStateStopped VoiceTransmitState = "stopped"
)

// Valid reports whether s is a recognized transmit state.
func (s VoiceTransmitState) Valid() bool {
	switch s {
	case VoiceTransmitStateStarted, VoiceTransmitStateStopped:
		return true
	default:
		return false
	}
}

// VoiceTransmitReportV1 is the body of POST /v1/channels/{channel}/voice/transmit
// (issue #179; design note docs/design/conch-voice.md §6): what the voice
// client sends when a press starts and when it stops, so that a press shorter
// than the poller's interval is still in the audit log. Like the session
// response it is a REST body with no schema-name field, versioned by the V1
// type suffix. A successful report is answered 204 with no body; there is no
// response type.
//
// The shape is deliberately this small, and what it leaves out is part of the
// contract:
//
//   - No time. conchd stamps the moment it receives the report, so a client
//     cannot back-date or post-date its own audit record; the server's clock
//     is the only one in the audit log.
//   - No sequence number. The client sends its reports one at a time and
//     conchd takes them in arrival order (design note §6 says why numbering
//     was left out).
//   - No principal, room or token. Who is reporting is the authenticated
//     caller, and the room is the channel's current room for the audience;
//     neither is the client's to assert.
//
// A field of that kind in the body would otherwise be dropped in silence, and
// could later be trusted by a reader that finds it in a captured request. So a
// report is decoded with DecodeVoiceTransmitReportV1, which rejects any field
// this type does not declare. That is stricter than presence, which tolerates
// unknown fields: presence is read by clients of a newer server, while a
// report is written by a client into the audit trail. A report that needs
// another field is a new version of this shape.
type VoiceTransmitReportV1 struct {
	// State is what the caller reports about its transmission: started or
	// stopped. A report that does not change the state conchd holds (a second
	// started) is still a valid report; the schema does not know the state.
	State VoiceTransmitState `json:"state"`
	// Audience is the audience transmitted to; nil (omitted) means the whole
	// channel, exactly as on VoiceRoomGrant. V4 reports are all channel-wide;
	// a net audience arrives with V5.
	Audience *Audience `json:"audience,omitempty"`
}

// Validate reports whether the report is structurally well-formed: a state of
// started or stopped, and a present audience that satisfies
// Audience.Validate, so an unknown audience kind fails the report as it fails
// a grant or a message. It does not check that the caller may transmit to the
// audience, holds a session, or is within the report bound; those are the
// endpoint's to decide.
func (r VoiceTransmitReportV1) Validate() error {
	if !r.State.Valid() {
		return fmt.Errorf("schema: voice transmit report state %q is not one of started, stopped", r.State)
	}
	if r.Audience != nil {
		if err := r.Audience.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// DecodeVoiceTransmitReportV1 reads one transmit report from r and is the
// only way a request body should become a VoiceTransmitReportV1. It rejects,
// in this order: a body that is not a JSON object of the declared fields (any
// unknown field at any depth fails, including at, time, seq, principal_id,
// room and token, and an unknown field inside audience); anything after that
// one object; and a report that does not satisfy Validate. A returned report
// is therefore one a handler can act on without further structural checks.
//
// It does not bound how much it reads; the caller wraps the body (for a
// handler, http.MaxBytesReader). An error from r is wrapped, so errors.As
// still finds an *http.MaxBytesError. A JSON null for audience is the same as
// leaving it out.
func DecodeVoiceTransmitReportV1(r io.Reader) (VoiceTransmitReportV1, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var report VoiceTransmitReportV1
	if err := dec.Decode(&report); err != nil {
		return VoiceTransmitReportV1{}, fmt.Errorf("schema: decode voice transmit report: %w", err)
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case err == nil:
		return VoiceTransmitReportV1{}, errors.New("schema: voice transmit report body must contain one JSON object")
	case !errors.Is(err, io.EOF):
		return VoiceTransmitReportV1{}, fmt.Errorf("schema: decode voice transmit report: %w", err)
	}
	if err := report.Validate(); err != nil {
		return VoiceTransmitReportV1{}, err
	}
	return report, nil
}
