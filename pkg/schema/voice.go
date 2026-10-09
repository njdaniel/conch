package schema

import (
	"errors"
	"fmt"
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
	// every room. One principal holds one connection per room; a second
	// connection with the same identity displaces the first.
	Identity string `json:"identity"`
	// Rooms lists one grant per room the caller may join.
	Rooms []VoiceRoomGrant `json:"rooms"`
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
}

// Validate reports whether the participant is structurally well-formed. A
// participant who cannot publish cannot be transmitting: if presence ever
// said so, either the grant or the observation is wrong, and a reader must
// not be left to guess which.
func (p VoiceParticipant) Validate() error {
	if p.PrincipalID <= 0 {
		return fmt.Errorf("schema: voice participant principal_id must be positive, got %d", p.PrincipalID)
	}
	if p.Transmitting && !p.CanPublish {
		return fmt.Errorf("schema: voice participant %d cannot be transmitting without can_publish", p.PrincipalID)
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
// audience satisfies Audience.Validate, every participant is well-formed, and
// no principal is listed twice. One principal holds one connection per room
// (a second device displaces the first), so a duplicate is a conchd bug, not
// a state. Order is not checked.
func (r VoicePresenceRoom) Validate() error {
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
}

// Validate reports whether the snapshot is structurally well-formed: the v1
// schema name, a positive channel_id, every room well-formed, no two rooms for
// the same audience, and the configured/available/rooms states consistent
// with each other.
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
	return nil
}
