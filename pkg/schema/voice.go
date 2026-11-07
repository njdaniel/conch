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
// fields of a participant in presence. Issue #487 added why the server ended
// a share, an optional field of the sharer's own presence, and the parameter
// by which a release names the share it means. The camera (ADR-015, issue
// #1238; docs/design/webcam-video.md §4.2, §5, §10) added the last, on the
// share's pattern: the claim a client is given when it may publish a camera,
// an optional field of a participant, why the server ended a camera, in the
// person's own presence alone, and the parameters of the camera routes. All
// of it is additive; no published shape changed.
//
// Several shares at once (issue #1483; docs/design/voice-multi-share.md §2,
// §3.2, §4, §5.2; the schema half is issue #1507) is the one change that is
// not: v1's Validate says a room has one share at a time, so a document with
// two sharers is not a v1 document, and lifting the rule in place would have
// broken every reader that validates. v1 stays as it is, published and
// immutable, and conch.voice_presence.v2 is v1 without that rule, asked for by
// VoicePresenceSchemaParam. Additive to both: when a participant's share
// started, the quality the sharer declared, the optional body of the share
// claim that declares it, the echo on the claim's answer, the refusal when the
// room's streams are all taken, and the parameter by which a moderator names
// whose stream to stop. The cap on streams is the server's, as the camera's
// is (ADR-015 N3): no count appears in either version.
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

// VoicePresenceSchemaV2 is the versioned name identifying the v2 voice
// presence document (VoicePresenceV2; issue #1483, design §2.2): v1 without
// the one-share rule. Any number of participants may be sharing; how many is
// the server's cap, not the schema's (ADR-015 N3). A reader asks for it with
// VoicePresenceSchemaParam; without asking it gets v1, built so that it still
// validates (design §2.3).
const VoicePresenceSchemaV2 = "conch.voice_presence.v2"

// VoicePresenceSchemaParam is the optional query parameter of
// GET /v1/channels/{channel}/voice and GET /v1/voice/ws naming the presence
// version the caller reads: VoicePresenceSchemaV1 or VoicePresenceSchemaV2,
// the same string the document carries in its schema field. Absent means v1.
// Any other value is 400 invalid_request, read before the channel, so it says
// nothing about the channel. The occupancy document takes no parameter.
const VoicePresenceSchemaParam = "schema"

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
	//
	// Answered by no conchd from the version that implements several shares
	// at once (issue #1483, design §5.1): a room holds up to its cap of
	// streams, one per person, and a room with no place left answers
	// ErrorCodeVoiceShareFull instead. The constant is kept for readers:
	// removing a published code breaks Go and TypeScript clients at compile
	// time for no gain, and an older conchd still sends it.
	ErrorCodeVoiceShareBusy = "voice_share_busy"
	// ErrorCodeVoiceShareStale: a release named a share (VoiceShareClaimParam)
	// that is no longer the caller's current one, and nothing was changed
	// (HTTP 409, issue #487). The share it meant has ended already; whatever
	// share the room has now, someone else's or a newer one of the caller's,
	// goes on.
	ErrorCodeVoiceShareStale = "voice_share_stale"
	// ErrorCodeVoiceCameraFull: the room already holds as many camera claims
	// as it allows, and the caller holds none (HTTP 409, ADR-015; design
	// §4.2 step 10). The answer names nobody. The cap is the server's
	// constant, not the schema's: presence reports whatever was granted.
	ErrorCodeVoiceCameraFull = "voice_camera_full"
	// ErrorCodeVoiceCameraStale: a camera request named a claim
	// (VoiceCameraClaimParam) that is not the caller's current one, and
	// nothing was changed (HTTP 409, design §4.2). On a POST that is any
	// claim the server no longer holds, other than the adoption of one the
	// room's rotation ended moments ago; on a DELETE it is as
	// voice_share_stale. A client that gets it stops its capture.
	ErrorCodeVoiceCameraStale = "voice_camera_stale"
	// ErrorCodeVoiceCameraRateLimited: the caller made more camera claims
	// than the per-principal bound allows (HTTP 429, design §4.2). A
	// continued claim and an un-named request while holding one count too;
	// releases are never bounded.
	ErrorCodeVoiceCameraRateLimited = "voice_camera_rate_limited"
	// ErrorCodeVoiceShareFull: the room already holds as many screen shares
	// as it allows, and the caller holds none (HTTP 409, issue #1483; design
	// §4). The answer names nobody. As for the camera, the cap is the
	// server's constant, not the schema's: presence reports whatever was
	// granted, and a holder asking again is answered with the stream held,
	// never with this.
	ErrorCodeVoiceShareFull = "voice_share_full"
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
	// Sharing is whether the participant holds a screen share (issue #443):
	// conchd granted it and it has not ended. It is what conchd decided, not
	// what LiveKit last showed, so it is true from the moment the claim is
	// answered, before any track is published. Omitted when false, so a
	// document with nobody sharing is what it was before. In a v1 room at
	// most one participant is sharing; in a v2 room any number (issue #1483).
	Sharing bool `json:"sharing,omitempty"`
	// SharingAudio is whether the share carries sound: the participant holds
	// the share and a screen-share audio track was published on the last
	// pass. Omitted when false.
	SharingAudio bool `json:"sharing_audio,omitempty"`
	// ShareStartedAt is when conchd granted the participant's share (UTC, ms
	// precision): the started_at of the claim, as VoiceShareV1 gave the
	// sharer (issue #1483, design §2.2). With several streams in a room it is
	// the one order every reader agrees on, earliest first, and it tells one
	// stream of a person's from the next: a share that stopped and started
	// again has a new value. Present exactly when Sharing in a v2 document;
	// in v1 it is present whenever the server has the field, and a v1
	// document from before it, sharing with no time, still reads. Omitted
	// when not sharing, so a document with nobody sharing is what it was
	// before, and a reader that does not know it skips it.
	ShareStartedAt *Timestamp `json:"share_started_at,omitempty"`
	// ShareQuality is the resolution and frame rate the sharer declared for
	// their stream (design §3.2), present only when Sharing and the sharer
	// declared one (VoiceShareRequestV1). It is a label, not enforcement:
	// conchd repeats what the sharer said so a viewer can decide whether to
	// watch, and polices neither bitrate nor size. Omitted when not sharing
	// or not declared.
	ShareQuality *VoiceShareQuality `json:"share_quality,omitempty"`
	// Camera is whether the participant holds a camera claim (ADR-015,
	// design §5): conchd granted it and it has not ended. As with Sharing it
	// is what conchd decided, not what LiveKit last showed, so it is true
	// from the moment the claim is answered. Any number of participants may
	// hold one at a time; how many is the server's cap, not the schema's.
	// Omitted when false, so a document with no camera is what it was
	// before.
	Camera bool `json:"camera,omitempty"`
}

// Validate reports whether the participant is structurally well-formed. A
// participant who cannot publish cannot be transmitting, sharing or holding
// a camera: if presence ever said so, either the grant or the observation is
// wrong, and a reader must not be left to guess which. Sound of a share needs
// a share, so sharing_audio needs sharing; so do share_started_at, which is a
// time when present, and share_quality, which is one of the picker's when
// present. Whether sharing needs share_started_at is the room's rule, by
// version: a v2 room requires it (VoicePresenceRoomV2.Validate), a v1 room
// does not, because a v1 document from before the field is still a v1
// document.
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
	if p.ShareStartedAt != nil {
		if !p.Sharing {
			return fmt.Errorf("schema: voice participant %d cannot have share_started_at without sharing", p.PrincipalID)
		}
		if p.ShareStartedAt.IsZero() {
			return fmt.Errorf("schema: voice participant %d share_started_at is zero", p.PrincipalID)
		}
	}
	if p.ShareQuality != nil {
		if !p.Sharing {
			return fmt.Errorf("schema: voice participant %d cannot have share_quality without sharing", p.PrincipalID)
		}
		if err := p.ShareQuality.Validate(); err != nil {
			return err
		}
	}
	if p.Camera && !p.CanPublish {
		return fmt.Errorf("schema: voice participant %d cannot have a camera without can_publish", p.PrincipalID)
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
// audience satisfies Audience.Validate, every participant is well-formed (a
// camera, like a share, needs can_publish), no principal is listed twice, and
// at most one participant is sharing. One principal holds one own connection
// per room (a second device displaces the first; a companion or viewer under
// a derived identity is folded into the person, issue #933), and a v1 room
// has one screen share at a time, so either duplicate is a conchd bug, not a
// state. Order is not checked.
//
// The one-share rule is v1's contract and never changes (issue #1483): a
// server with several streams builds a v1 document that lists exactly one of
// them (design §2.3), and a reader that wants them all reads
// VoicePresenceRoomV2. A sharer need not carry share_started_at here: a v1
// document from before that field is still a v1 document.
//
// How many participants hold a camera is not checked (ADR-015, design §5):
// the cap is a server constant that can change without a schema change, and
// a check here would fail the server's own snapshot, and every older
// client's, the day it did.
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

// roomAudience is the room's audience, for the one-room-per-audience rule of
// the document that lists it.
func (r VoicePresenceRoom) roomAudience() *Audience { return r.Audience }

// VoicePresenceRoomV2 is one voice room as v2 presence shows it (issue
// #1483, design §2.2): the fields of VoicePresenceRoom, exactly, with no
// one-share rule. Any number of participants may be sharing, each since its
// own ShareStartedAt; how many is the server's cap (voice_share_full at the
// claim), never a check here (ADR-015 N3), so the cap can move without a
// schema change. As in v1 it has no room name and no token.
//
// Participants is always a JSON array, ordered by principal_id; a room with
// nobody in it is an empty array. A reader that wants the streams in order
// sorts the sharing participants by share_started_at, then principal_id.
type VoicePresenceRoomV2 struct {
	// Audience is the audience the room carries; nil (omitted) means the
	// whole channel.
	Audience *Audience `json:"audience,omitempty"`
	// Participants lists every principal connected to the room.
	Participants []VoiceParticipant `json:"participants"`
}

// Validate reports whether the room is structurally well-formed: a present
// audience satisfies Audience.Validate, every participant is well-formed, no
// principal is listed twice, and every sharing participant says when their
// share started. It does not count sharers: that is the one check v1 has and
// v2 does not. share_started_at is required of a sharer here and not in v1
// because every v2 document comes from a server that has the field, while a
// v1 document may predate it.
func (r VoicePresenceRoomV2) Validate() error {
	if r.Audience != nil {
		if err := r.Audience.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[int64]struct{}, len(r.Participants))
	for _, p := range r.Participants {
		if err := p.Validate(); err != nil {
			return err
		}
		if _, dup := seen[p.PrincipalID]; dup {
			return fmt.Errorf("schema: voice room lists principal %d more than once", p.PrincipalID)
		}
		seen[p.PrincipalID] = struct{}{}
		if p.Sharing && p.ShareStartedAt == nil {
			return fmt.Errorf("schema: voice participant %d is sharing without share_started_at", p.PrincipalID)
		}
	}
	return nil
}

// roomAudience is the room's audience, for the one-room-per-audience rule of
// the document that lists it.
func (r VoicePresenceRoomV2) roomAudience() *Audience { return r.Audience }

// presenceRoom is a room of either presence version as the document-level
// checks see it: its own Validate, and its audience for the
// one-room-per-audience rule.
type presenceRoom interface {
	Validate() error
	roomAudience() *Audience
}

// validatePresence is the document-level validation shared by VoicePresenceV1
// and VoicePresenceV2, which differ only in the schema name they must carry
// and the room type they list. The checks and their order are v1's, exactly,
// so that the two versions can never drift apart in anything but the room
// rule.
func validatePresence[R presenceRoom](schema, want string, channelID int64, configured, available bool, rooms []R, shareEnded *VoiceShareEnded, cameraEnded *VoiceCameraEnded) error {
	if schema != want {
		return fmt.Errorf("schema: voice presence schema must be %q, got %q", want, schema)
	}
	if channelID <= 0 {
		return fmt.Errorf("schema: voice presence channel_id must be positive, got %d", channelID)
	}
	if !configured && available {
		return errors.New("schema: voice presence cannot be available when not configured")
	}
	if !available && len(rooms) != 0 {
		return errors.New("schema: voice presence must list no rooms when not available")
	}
	seen := make(map[string]struct{}, len(rooms))
	for _, r := range rooms {
		if err := r.Validate(); err != nil {
			return err
		}
		key := audienceKey(r.roomAudience())
		if _, dup := seen[key]; dup {
			return errors.New("schema: voice presence lists more than one room for the same audience")
		}
		seen[key] = struct{}{}
	}
	if shareEnded != nil {
		// A share needs voice. It does not need LiveKit to be reachable
		// now: a share stopped during an outage has ended all the same.
		if !configured {
			return errors.New("schema: voice presence cannot carry share_ended when not configured")
		}
		if err := shareEnded.Validate(); err != nil {
			return err
		}
	}
	if cameraEnded != nil {
		// As for a share: a camera needs voice, not a reachable LiveKit.
		if !configured {
			return errors.New("schema: voice presence cannot carry camera_ended when not configured")
		}
		if err := cameraEnded.Validate(); err != nil {
			return err
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
	// CameraEnded is why the reader's own last camera claim ended (ADR-015,
	// design §5), on the same terms as ShareEnded: in the document sent to
	// the person who held the camera and in no one else's, in the frame that
	// takes their camera away, kept until that person claims a camera again,
	// and dropped by a restart. A reader matches its started_at with the
	// claim it held (VoiceCameraV1.StartedAt). It never names who ended the
	// camera.
	CameraEnded *VoiceCameraEnded `json:"camera_ended,omitempty"`
}

// Validate reports whether the snapshot is structurally well-formed: the v1
// schema name, a positive channel_id, every room well-formed, no two rooms for
// the same audience, the configured/available/rooms states consistent with
// each other, and a present share_ended or camera_ended well-formed, on a
// server with voice configured.
//
// An unknown audience kind in any room fails the whole document, as it does
// for a message (ADR-005: an audience a reader does not understand must never
// widen what it shows). A new audience kind is therefore a new version of
// these shapes, not an addition to this one. Not
// configured implies not available, and not available implies no rooms.
func (p VoicePresenceV1) Validate() error {
	return validatePresence(p.Schema, VoicePresenceSchemaV1, p.ChannelID, p.Configured, p.Available, p.Rooms, p.ShareEnded, p.CameraEnded)
}

// VoicePresenceV2 is the v2 whole-state voice presence snapshot for one
// channel (issue #1483, design §2.2): the body of
// GET /v1/channels/{channel}/voice?schema=conch.voice_presence.v2 and the
// document streamed on GET /v1/voice/ws with the same parameter. It has the
// fields of VoicePresenceV1, exactly, with Schema always VoicePresenceSchemaV2
// and rooms of VoicePresenceRoomV2, in which any number of participants may
// be sharing. ShareEnded and CameraEnded are as in v1: the reader's own, in
// their document alone, and naming nobody.
//
// As in v1 no field of this document or of the types it embeds carries a
// token or a room name, and voice_test.go asserts that; unknown fields are
// tolerated, as a client of a newer server must.
type VoicePresenceV2 struct {
	// Schema is the document discriminator; always VoicePresenceSchemaV2.
	Schema string `json:"schema"`
	// ChannelID references the channel the snapshot describes.
	ChannelID int64 `json:"channel_id"`
	// Configured is whether this conchd has LiveKit configured at all.
	Configured bool `json:"configured"`
	// Available is whether LiveKit answered on the last pass. Always false
	// when Configured is false.
	Available bool `json:"available"`
	// Rooms lists the rooms the caller may see with their participants.
	Rooms []VoicePresenceRoomV2 `json:"rooms"`
	// ShareEnded is why the reader's own last screen share ended, on v1's
	// terms (VoicePresenceV1.ShareEnded). With several streams it is still
	// the reader's own: another person's share ending is seen as their
	// participant losing sharing, nothing more.
	ShareEnded *VoiceShareEnded `json:"share_ended,omitempty"`
	// CameraEnded is why the reader's own last camera claim ended, on v1's
	// terms (VoicePresenceV1.CameraEnded).
	CameraEnded *VoiceCameraEnded `json:"camera_ended,omitempty"`
}

// Validate reports whether the snapshot is structurally well-formed, by the
// rules of VoicePresenceV1.Validate with the v2 schema name and v2 rooms: a
// document that says conch.voice_presence.v1 is refused here as one that says
// v2 is refused there, so neither version is ever read as the other.
func (p VoicePresenceV2) Validate() error {
	return validatePresence(p.Schema, VoicePresenceSchemaV2, p.ChannelID, p.Configured, p.Available, p.Rooms, p.ShareEnded, p.CameraEnded)
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
	// Quality echoes the resolution and frame rate the caller declared
	// (VoiceShareRequestV1; issue #1483, design §3.1), as recorded with the
	// claim: the request's, or on a repeated request with a body the new
	// one. Omitted when nothing was declared, so the answer to a claim
	// without a body is what it was before the field.
	Quality *VoiceShareQuality `json:"quality,omitempty"`
}

// Validate reports whether the share is structurally well-formed: a positive
// principal_id, a started_at, and a present quality that is one of the
// picker's.
func (s VoiceShareV1) Validate() error {
	if s.PrincipalID <= 0 {
		return fmt.Errorf("schema: voice share principal_id must be positive, got %d", s.PrincipalID)
	}
	if s.StartedAt.IsZero() {
		return errors.New("schema: voice share started_at is required")
	}
	if s.Quality != nil {
		if err := s.Quality.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// VoiceShareResolution is the sharer's declared resolution: the picker's
// vocabulary (issue #1488, web/src/voice/share.ts; design §3.2). Closed and
// case-sensitive; a value outside it fails validation. "source" is the
// capture's own size, so it already covers anything larger than 1080p. A new
// value is a new version of the presence document, as a new share end reason
// is: the generated TypeScript refuses what it does not know.
type VoiceShareResolution string

// Voice share resolutions, the picker's three (issue #1488).
const (
	// VoiceShareResolution720p: the stream's top layer is 1280 by 720.
	VoiceShareResolution720p VoiceShareResolution = "720p"
	// VoiceShareResolution1080p: the stream's top layer is 1920 by 1080.
	VoiceShareResolution1080p VoiceShareResolution = "1080p"
	// VoiceShareResolutionSource: the stream's top layer is the capture's own
	// size, whatever that is.
	VoiceShareResolutionSource VoiceShareResolution = "source"
)

// Valid reports whether r is a recognized resolution.
func (r VoiceShareResolution) Valid() bool {
	switch r {
	case VoiceShareResolution720p, VoiceShareResolution1080p, VoiceShareResolutionSource:
		return true
	default:
		return false
	}
}

// VoiceShareQuality is what the sharer declared for their stream: a
// resolution and a frame rate, the picker's three by three (issue #1488;
// design §3.2). It is a label, not enforcement: conchd records it with the
// claim, echoes it on the claim's answer and repeats it in presence
// (VoiceParticipant.ShareQuality) so a viewer sees "1080p at 60 fps" before
// pressing Watch; the sharer's own client applies it to its publication, and
// the server polices neither bitrate nor size (private-alpha.md O6). A frame
// rate outside 15, 30 and 60 fails validation; as for the resolution, a new
// one is a new version of presence.
type VoiceShareQuality struct {
	// Resolution is the declared resolution of the stream's top layer.
	Resolution VoiceShareResolution `json:"resolution"`
	// FrameRate is the declared frame rate, in frames per second: 15, 30 or
	// 60.
	FrameRate int `json:"frame_rate"`
}

// Validate reports whether the quality is one of the picker's: a known
// resolution and a frame rate of 15, 30 or 60.
func (q VoiceShareQuality) Validate() error {
	if !q.Resolution.Valid() {
		return fmt.Errorf("schema: voice share resolution %q is not one of 720p, 1080p, source", q.Resolution)
	}
	switch q.FrameRate {
	case 15, 30, 60:
		return nil
	default:
		return fmt.Errorf("schema: voice share frame_rate %d is not one of 15, 30, 60", q.FrameRate)
	}
}

// VoiceShareRequestV1 is the optional body of POST
// /v1/channels/{channel}/voice/share (issue #1483, design §3.1): the quality
// the caller declares for their stream. No body, or an empty object, declares
// nothing, so a client from before the quality is unchanged. On a first claim
// the quality is recorded with the claim; a holder asking again with a body
// changes it, and presence says so; a holder asking again without a body
// keeps what was declared.
//
// What it leaves out is part of the contract: no principal (the authenticated
// caller), no room or audience (the channel-wide room, the only room with a
// share), no time (conchd's clock stamps the claim), no sound (the audio is
// what the caller's grant allows, as before). It is decoded strictly
// (DecodeVoiceShareRequestV1): what it says is recorded with the claim and
// repeated to every viewer, so a field this type does not declare is refused
// rather than dropped.
type VoiceShareRequestV1 struct {
	// Quality is the declared resolution and frame rate; nil declares
	// nothing.
	Quality *VoiceShareQuality `json:"quality,omitempty"`
}

// Validate reports whether the request is well-formed: a present quality is
// one of the picker's. A request with nothing declared is valid.
func (r VoiceShareRequestV1) Validate() error {
	if r.Quality != nil {
		return r.Quality.Validate()
	}
	return nil
}

// DecodeVoiceShareRequestV1 reads one request from r and is the only way a
// body should become a VoiceShareRequestV1. Like DecodeVoiceCompanionRequestV1
// it rejects, in this order: a body that is not a JSON object of the declared
// fields (any unknown field at any depth fails); a JSON null; anything after
// that one object; and a request that does not satisfy Validate. Unlike every
// other strict body, no body at all (nothing but whitespace) is accepted as
// the zero request: a claim sent by a client from before the quality has no
// body, and declares nothing.
//
// It does not bound how much it reads; the caller wraps the body (for a
// handler, http.MaxBytesReader). An error from r is wrapped, so errors.As
// still finds an *http.MaxBytesError; an error before the first byte is the
// reader's, never an empty body.
func DecodeVoiceShareRequestV1(r io.Reader) (VoiceShareRequestV1, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	// Decoded through a pointer so that a JSON null, which leaves the pointer
	// nil, can be told from an empty object, which allocates the zero request.
	var req *VoiceShareRequestV1
	switch err := dec.Decode(&req); {
	case errors.Is(err, io.EOF):
		return VoiceShareRequestV1{}, nil
	case err != nil:
		return VoiceShareRequestV1{}, fmt.Errorf("schema: decode voice share request: %w", err)
	case req == nil:
		return VoiceShareRequestV1{}, errors.New("schema: voice share request body must be a JSON object, not null")
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case err == nil:
		return VoiceShareRequestV1{}, errors.New("schema: voice share request body must contain one JSON object")
	case !errors.Is(err, io.EOF):
		return VoiceShareRequestV1{}, fmt.Errorf("schema: decode voice share request: %w", err)
	}
	if err := req.Validate(); err != nil {
		return VoiceShareRequestV1{}, err
	}
	return *req, nil
}

// Query parameters of DELETE /v1/channels/{channel}/voice/share.
const (
	// VoiceShareClaimParam is the optional query parameter by which a release
	// names the share it means (issue #487): the started_at of the caller's
	// own claim, as VoiceShareV1 gave it. With it, the request ends that
	// share only if it is still the caller's current one; otherwise it is
	// refused 409 voice_share_stale and changes nothing, so a release sent
	// late cannot end a newer share, least of all someone else's. Without it
	// the request ends whatever share the caller holds, or does nothing.
	//
	// Before several shares (issue #1483, design §5.2) the bare request was
	// also an operator's stop of whoever shared; with several streams in a
	// room that reading is ambiguous, and a moderator names the person with
	// VoiceSharePrincipalParam instead. No published client sent the bare
	// moderator's stop: the browser's own sharer always names its started_at.
	VoiceShareClaimParam = "started_at"
	// VoiceSharePrincipalParam is the optional query parameter by which a
	// holder of manage_channels, or a manager of the channel, names whose
	// stream to stop: a principal id, as VoiceCameraPrincipalParam names
	// whose camera (issue #1483, design §5.2). Absent, or naming the caller,
	// the request is the caller's own stop. It is not combined with
	// VoiceShareClaimParam when it names someone else: a moderator stops a
	// person's stream, not a particular claim, and the pair is 400
	// invalid_request. Naming a person who holds no stream in the room is
	// 204 with nothing written, the same answer as for an id that never
	// existed.
	VoiceSharePrincipalParam = "principal"
)

// ParseVoiceShareClaim reads the value of VoiceShareClaimParam: an RFC 3339
// timestamp, normalized as every wire timestamp is (UTC, millisecond
// precision), so that it compares equal to the started_at it was copied from.
func ParseVoiceShareClaim(s string) (Timestamp, error) {
	return parseTimestamp(s)
}

// VoiceCameraEndReason is why a camera claim ended: the reason conchd records
// on voice_camera_stopped (ADR-015, design §5). The vocabulary is closed and
// case-sensitive; a value outside it fails validation, so a new reason is a
// new version of the presence document, as it is for VoiceShareEndReason. It
// is its own type, not the share's, so that the two can grow apart.
type VoiceCameraEndReason string

// Voice camera end reasons (docs/design/webcam-video.md §5, §6.2).
const (
	// VoiceCameraEndSelf: the person turned it off.
	VoiceCameraEndSelf VoiceCameraEndReason = "self"
	// VoiceCameraEndOperator: a holder of manage_channels turned it off.
	// Which one is not said.
	VoiceCameraEndOperator VoiceCameraEndReason = "operator"
	// VoiceCameraEndLeft: the person was no longer in the room.
	VoiceCameraEndLeft VoiceCameraEndReason = "left"
	// VoiceCameraEndAccessLost: the person lost their place in the channel,
	// their voice, or use_camera.
	VoiceCameraEndAccessLost VoiceCameraEndReason = "access_lost"
	// VoiceCameraEndRotated: the room was replaced by a new one, which drops
	// its claims. A claim that ended this way may be adopted by a named
	// request for a short while (design §4.2).
	VoiceCameraEndRotated VoiceCameraEndReason = "rotated"
	// VoiceCameraEndIdle: nothing was sent for a while: no camera track soon
	// enough after the claim, or none any more.
	VoiceCameraEndIdle VoiceCameraEndReason = "idle"
)

// Valid reports whether r is a recognized end reason.
func (r VoiceCameraEndReason) Valid() bool {
	switch r {
	case VoiceCameraEndSelf, VoiceCameraEndOperator, VoiceCameraEndLeft, VoiceCameraEndAccessLost, VoiceCameraEndRotated, VoiceCameraEndIdle:
		return true
	default:
		return false
	}
}

// VoiceCameraEnded is why the reader's own camera claim ended, carried in
// their presence alone (VoicePresenceV1.CameraEnded, ADR-015). It names no
// principal: the reader is the person whose camera it was, and who stopped it
// is not told.
type VoiceCameraEnded struct {
	// Reason is why the camera ended.
	Reason VoiceCameraEndReason `json:"reason"`
	// StartedAt is the started_at of the claim that ended, as its claim
	// answered it (VoiceCameraV1.StartedAt): which claim this was.
	StartedAt Timestamp `json:"started_at"`
	// EndedAt is when conchd ended it (UTC, ms precision).
	EndedAt Timestamp `json:"ended_at"`
}

// Validate reports whether the end is structurally well-formed: a known
// reason, both times, and an end no earlier than the start.
func (e VoiceCameraEnded) Validate() error {
	if !e.Reason.Valid() {
		return fmt.Errorf("schema: voice camera end reason %q is not one of self, operator, left, access_lost, rotated, idle", e.Reason)
	}
	if e.StartedAt.IsZero() {
		return errors.New("schema: voice camera end started_at is required")
	}
	if e.EndedAt.IsZero() {
		return errors.New("schema: voice camera end ended_at is required")
	}
	if e.EndedAt.Time().Before(e.StartedAt.Time()) {
		return errors.New("schema: voice camera end ended_at is before started_at")
	}
	return nil
}

// VoiceCameraV1 is the body of a successful POST
// /v1/channels/{channel}/voice/camera[?started_at=…] (ADR-015; design
// docs/design/webcam-video.md §4.2): conchd holds a camera claim for the
// caller, and LiveKit lets the caller's connection publish a camera. Like the
// share it is a REST body with no schema-name field, versioned by the V1 type
// suffix, and it carries no room name and no token: the camera is granted to
// the connection the caller already has.
//
// A request without VoiceCameraClaimParam starts a claim. One that names the
// caller's current claim continues it, and is answered with the claim already
// held; one that names any other claim is 409 voice_camera_stale, except the
// adoption of a claim the room's rotation ended moments ago, which is a new
// claim with a new started_at. The adoption window is the server's constant
// and is not published.
type VoiceCameraV1 struct {
	// PrincipalID is the principal who holds the claim: the caller.
	PrincipalID int64 `json:"principal_id"`
	// StartedAt is when conchd granted the claim (UTC, ms precision). Asking
	// again while holding it answers with the same time; a client names it
	// (VoiceCameraClaimParam) on every request that means this claim.
	StartedAt Timestamp `json:"started_at"`
}

// Validate reports whether the claim is structurally well-formed: a positive
// principal_id and a started_at.
func (c VoiceCameraV1) Validate() error {
	if c.PrincipalID <= 0 {
		return fmt.Errorf("schema: voice camera principal_id must be positive, got %d", c.PrincipalID)
	}
	if c.StartedAt.IsZero() {
		return errors.New("schema: voice camera started_at is required")
	}
	return nil
}

// Query parameters of the camera routes (design §4.2).
const (
	// VoiceCameraClaimParam is the optional query parameter of both POST and
	// DELETE /v1/channels/{channel}/voice/camera by which a request names
	// the claim it means: the started_at of the caller's own claim, as
	// VoiceCameraV1 gave it. On a POST it continues that claim (a named
	// request never starts a new one, except the rotation adoption); on a
	// DELETE it ends that claim only if it is still the caller's current
	// one, as VoiceShareClaimParam does. A name that is not the caller's
	// current claim is 409 voice_camera_stale and changes nothing. A DELETE
	// without it ends whatever the caller holds.
	VoiceCameraClaimParam = "started_at"
	// VoiceCameraPrincipalParam is the optional query parameter of DELETE
	// /v1/channels/{channel}/voice/camera by which a holder of
	// manage_channels names whose camera to stop: a principal id. Absent, or
	// naming the caller, the request is the caller's own stop. It is not
	// combined with VoiceCameraClaimParam when it names someone else: the
	// operator stops the camera, not a particular claim, and the pair is
	// 400 invalid_request.
	VoiceCameraPrincipalParam = "principal"
)

// ParseVoiceCameraClaim reads the value of VoiceCameraClaimParam: an RFC 3339
// timestamp, normalized as every wire timestamp is (UTC, millisecond
// precision), so that it compares equal to the started_at it was copied from.
func ParseVoiceCameraClaim(s string) (Timestamp, error) {
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
