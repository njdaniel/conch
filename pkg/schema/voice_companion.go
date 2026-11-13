package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Companion participants (issue #933, part B of #897;
// docs/design/voice-control-plane.md §13). A person in voice is one LiveKit
// participant, p<id>, and a second connection under that identity displaces
// the first. A desktop whose voice is in the Rust helper and whose screen is
// in Electron therefore needs a second participant that conchd issues under a
// derived identity and recognises as the same person: the companion,
// p<id>.screen, which may publish a screen and nothing else; and the viewer,
// p<id>.view, which may publish nothing and lets a browser tab watch beside
// native voice. One route serves both; the publish set is the parameter.
//
// Like the session response these are REST bodies with no schema-name field,
// versioned by the V1 type suffix. The request is decoded strictly
// (DecodeVoiceCompanionRequestV1): what it says is recorded in the audit log
// and signed into a grant, so a field this type does not declare is refused
// rather than dropped. The response tolerates unknown fields, as a client of a
// newer server must.

// ErrorCodeVoiceCompanionBusy: the caller already has a companion in this room
// (HTTP 409). A room has one companion per person at a time, and a second
// request never displaces the first: the person who wants to move a share
// stops it, or removes the companion, first. The answer names no identity and
// no room.
const ErrorCodeVoiceCompanionBusy = "voice_companion_busy"

// VoiceCompanionPublish is what a companion grant lets its holder publish. The
// vocabulary is closed and case-sensitive; a value outside it fails
// validation. A new publish set is a new value here, and so a change the web
// client's generated validator refuses until it is regenerated.
type VoiceCompanionPublish string

const (
	// VoiceCompanionPublishScreen: a screen-share video track, and nothing
	// else by the grant. The companion, identity p<id>.screen. Its sound is
	// never in the grant.
	VoiceCompanionPublishScreen VoiceCompanionPublish = "screen"
	// VoiceCompanionPublishNone: nothing. The viewer, identity p<id>.view: a
	// participant that only receives.
	VoiceCompanionPublishNone VoiceCompanionPublish = "none"
)

// Valid reports whether p is a recognized publish set.
func (p VoiceCompanionPublish) Valid() bool {
	switch p {
	case VoiceCompanionPublishScreen, VoiceCompanionPublishNone:
		return true
	default:
		return false
	}
}

// VoiceCompanionRequestV1 is the body of
// POST /v1/channels/{channel}/voice/companion: which second participant the
// caller wants in the channel's voice room.
//
// What it leaves out is part of the contract: no principal (the authenticated
// caller), no room or audience (a companion joins the channel-wide room, the
// only room with a share), no identity (conchd derives it), no lifetime
// (15 s, as for a session).
type VoiceCompanionRequestV1 struct {
	// Publish is what the participant may publish: screen (the companion) or
	// none (the viewer). Required.
	Publish VoiceCompanionPublish `json:"publish"`
	// Subscribe is whether the participant may receive the room's tracks.
	// Always written. True lets the companion watch (it must then connect with
	// autoSubscribe off and take only screen tracks, which is the client's
	// honesty, not conchd's enforcement); false is publish-only, enforced by
	// LiveKit. A viewer that could not subscribe would do nothing, so publish
	// none requires subscribe true.
	Subscribe bool `json:"subscribe"`
}

// Validate reports whether the request is well-formed: a known publish set,
// and subscribe true when publish is none.
func (r VoiceCompanionRequestV1) Validate() error {
	if !r.Publish.Valid() {
		return fmt.Errorf("schema: voice companion publish %q is not one of screen, none", r.Publish)
	}
	if r.Publish == VoiceCompanionPublishNone && !r.Subscribe {
		return errors.New("schema: a voice companion that publishes none must subscribe")
	}
	return nil
}

// DecodeVoiceCompanionRequestV1 reads one request from r and is the only way a
// body should become a VoiceCompanionRequestV1. Like
// DecodeVoiceTransmitReportV1 it rejects, in this order: a body that is not a
// JSON object of the declared fields (any unknown field at any depth fails);
// anything after that one object; and a request that does not satisfy
// Validate (a JSON null decodes to the zero request, which does not).
//
// It does not bound how much it reads; the caller wraps the body (for a
// handler, http.MaxBytesReader). An error from r is wrapped, so errors.As
// still finds an *http.MaxBytesError.
func DecodeVoiceCompanionRequestV1(r io.Reader) (VoiceCompanionRequestV1, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var req VoiceCompanionRequestV1
	if err := dec.Decode(&req); err != nil {
		return VoiceCompanionRequestV1{}, fmt.Errorf("schema: decode voice companion request: %w", err)
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case err == nil:
		return VoiceCompanionRequestV1{}, errors.New("schema: voice companion request body must contain one JSON object")
	case !errors.Is(err, io.EOF):
		return VoiceCompanionRequestV1{}, fmt.Errorf("schema: decode voice companion request: %w", err)
	}
	if err := req.Validate(); err != nil {
		return VoiceCompanionRequestV1{}, err
	}
	return req, nil
}

// VoiceCompanionResponseV1 is the body of a successful
// POST /v1/channels/{channel}/voice/companion: everything the second
// participant needs to connect, and nothing the person's own connection needs.
// The renderer that holds it holds no other credential, so it carries no
// bearer and no session of the person's.
type VoiceCompanionResponseV1 struct {
	// LivekitURL is the ws:// or wss:// address, exactly as configured.
	LivekitURL string `json:"livekit_url"`
	// Identity is the participant identity conchd derived: p<id>.screen for
	// publish screen, p<id>.view for publish none. Opaque to clients.
	Identity string `json:"identity"`
	// Room is the LiveKit room name of the channel-wide room; opaque.
	Room string `json:"room"`
	// Token is the signed join token for this one participant and room;
	// opaque, never logged or audited.
	Token string `json:"token"`
	// Publish and Subscribe echo what the token grants, so a reader never has
	// to remember what it asked for.
	Publish   VoiceCompanionPublish `json:"publish"`
	Subscribe bool                  `json:"subscribe"`
	// ExpiresAt is the token's expiry (UTC, ms precision), 15 s after issue as
	// for a session: "connect now", not a deadline.
	ExpiresAt Timestamp `json:"expires_at"`
}

// Validate reports whether the response is well-formed: an address, an
// identity, a room, a token and an expiry present; a known publish set; and
// subscribe true when publish is none. It does not look inside the identity,
// the room name or the token.
func (r VoiceCompanionResponseV1) Validate() error {
	switch {
	case r.LivekitURL == "":
		return errors.New("schema: voice companion livekit_url is required")
	case r.Identity == "":
		return errors.New("schema: voice companion identity is required")
	case r.Room == "":
		return errors.New("schema: voice companion room is required")
	case r.Token == "":
		return errors.New("schema: voice companion token is required")
	case r.ExpiresAt.IsZero():
		return errors.New("schema: voice companion expires_at is required")
	}
	return VoiceCompanionRequestV1{Publish: r.Publish, Subscribe: r.Subscribe}.Validate()
}

// The companion's share audio (issue #898, part C of #897;
// docs/design/voice-control-plane.md §13). A companion's grant never names
// screen_share_audio (linux-desktop.md §6.4): a renderer could otherwise
// publish synthesised speech as share audio with no press and no gate. The
// sound is added to the connected companion by a permission update, only
// when the share's holder asks through this route, which records an explicit
// audio-scope consent, and it is withdrawn on DELETE and when the share ends
// for any reason. The scopes are the desktop designs' vocabulary
// (windows-desktop.md §8, linux-desktop.md §8), off by default; "none" is the
// absence of a consent, which is DELETE, not a value.
//
// Like the companion request the request is decoded strictly
// (DecodeVoiceCompanionAudioRequestV1): the scope is a consent, recorded in
// the audit log as it was given. The response tolerates unknown fields.

// VoiceShareAudioScope is what sound a person consents to their companion
// publishing as the share's audio. The vocabulary is closed and
// case-sensitive; a value outside it fails validation. Both scopes grant the
// same LiveKit source: the scope is consent, recorded, and the capture it
// names is the desktop's to honour.
type VoiceShareAudioScope string

const (
	// VoiceShareAudioScopeApplication: the sound of the shared application
	// or window alone.
	VoiceShareAudioScopeApplication VoiceShareAudioScope = "application"
	// VoiceShareAudioScopeSystemExcludingCall: everything the system plays,
	// except the call itself (the room's own voice is never fed back).
	VoiceShareAudioScopeSystemExcludingCall VoiceShareAudioScope = "system_excluding_call"
)

// Valid reports whether s is a recognized scope.
func (s VoiceShareAudioScope) Valid() bool {
	switch s {
	case VoiceShareAudioScopeApplication, VoiceShareAudioScopeSystemExcludingCall:
		return true
	default:
		return false
	}
}

// VoiceShareAudioPublisher names which participant publishes the share's
// sound, the one the consent is for (docs/design/linux-share-audio.md §8,
// decision D-3, issue #1853). The vocabulary is closed and case-sensitive;
// absent on the wire means companion.
type VoiceShareAudioPublisher string

const (
	// VoiceShareAudioPublisherCompanion: the person's companion (p<id>.screen)
	// publishes the sound, and is granted screen_share_audio for it. The
	// default, and the route Windows uses.
	VoiceShareAudioPublisherCompanion VoiceShareAudioPublisher = "companion"
	// VoiceShareAudioPublisherPerson: the person's own participant (p<id>)
	// publishes the sound, which its share grant already carries. The consent
	// is recorded and the companion is granted nothing.
	VoiceShareAudioPublisherPerson VoiceShareAudioPublisher = "person"
)

// Valid reports whether p is a recognized publisher. The empty value is not
// one: it is how a struct says the field is absent (omitempty), which the
// request and response Validate accept.
func (p VoiceShareAudioPublisher) Valid() bool {
	switch p {
	case VoiceShareAudioPublisherCompanion, VoiceShareAudioPublisherPerson:
		return true
	default:
		return false
	}
}

// VoiceCompanionAudioRequestV1 is the body of
// PUT /v1/channels/{channel}/voice/companion/audio: the share holder's
// consent to their companion publishing the share's sound, and to which
// sound. The share and the companion it applies to are the caller's own in
// the channel's room; nothing else is named.
type VoiceCompanionAudioRequestV1 struct {
	// Scope is the sound consented to. Required.
	Scope VoiceShareAudioScope `json:"scope"`
	// Publisher says who publishes the sound. Optional; absent means
	// companion. A server from before issue #1853 refuses the field (the
	// request is decoded strictly), which a client reads as "no sound route
	// on this server".
	Publisher VoiceShareAudioPublisher `json:"publisher,omitempty"`
}

// Validate reports whether the request is well-formed: a known scope, and a
// known publisher when one is named.
func (r VoiceCompanionAudioRequestV1) Validate() error {
	if !r.Scope.Valid() {
		return fmt.Errorf("schema: voice share audio scope %q is not one of application, system_excluding_call", r.Scope)
	}
	if r.Publisher != "" && !r.Publisher.Valid() {
		return fmt.Errorf("schema: voice share audio publisher %q is not one of companion, person", r.Publisher)
	}
	return nil
}

// EffectivePublisher is the publisher the request means: the one named, or
// companion when it names none.
func (r VoiceCompanionAudioRequestV1) EffectivePublisher() VoiceShareAudioPublisher {
	if r.Publisher == "" {
		return VoiceShareAudioPublisherCompanion
	}
	return r.Publisher
}

// DecodeVoiceCompanionAudioRequestV1 reads one request from r and is the only
// way a body should become a VoiceCompanionAudioRequestV1. Like
// DecodeVoiceCompanionRequestV1 it rejects, in this order: a body that is not
// a JSON object of the declared fields (any unknown field at any depth
// fails); anything after that one object; and a request that does not satisfy
// Validate (a JSON null decodes to the zero request, which does not).
//
// It does not bound how much it reads; the caller wraps the body (for a
// handler, http.MaxBytesReader). An error from r is wrapped, so errors.As
// still finds an *http.MaxBytesError.
func DecodeVoiceCompanionAudioRequestV1(r io.Reader) (VoiceCompanionAudioRequestV1, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var req VoiceCompanionAudioRequestV1
	if err := dec.Decode(&req); err != nil {
		return VoiceCompanionAudioRequestV1{}, fmt.Errorf("schema: decode voice companion audio request: %w", err)
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case err == nil:
		return VoiceCompanionAudioRequestV1{}, errors.New("schema: voice companion audio request body must contain one JSON object")
	case !errors.Is(err, io.EOF):
		return VoiceCompanionAudioRequestV1{}, fmt.Errorf("schema: decode voice companion audio request: %w", err)
	}
	if err := req.Validate(); err != nil {
		return VoiceCompanionAudioRequestV1{}, err
	}
	return req, nil
}

// VoiceCompanionAudioV1 is the body of a successful
// PUT /v1/channels/{channel}/voice/companion/audio: the consent as recorded,
// and which share it belongs to. It carries no token, no identity and no
// room name.
type VoiceCompanionAudioV1 struct {
	// Scope is the consent recorded: the request's, or on a repeated request
	// the new one.
	Scope VoiceShareAudioScope `json:"scope"`
	// Publisher echoes the publisher recorded, when the request named one;
	// absent means companion, as on the request (so a response to a request
	// that named none is byte-identical to what v1 always sent).
	Publisher VoiceShareAudioPublisher `json:"publisher,omitempty"`
	// StartedAt is the started_at of the caller's share (VoiceShareV1), the
	// one the consent is part of: it ends with that share. UTC, ms
	// precision.
	StartedAt Timestamp `json:"started_at"`
}

// Validate reports whether the response is well-formed: a known scope and a
// started_at present.
func (a VoiceCompanionAudioV1) Validate() error {
	if !a.Scope.Valid() {
		return fmt.Errorf("schema: voice share audio scope %q is not one of application, system_excluding_call", a.Scope)
	}
	if a.Publisher != "" && !a.Publisher.Valid() {
		return fmt.Errorf("schema: voice share audio publisher %q is not one of companion, person", a.Publisher)
	}
	if a.StartedAt.IsZero() {
		return errors.New("schema: voice companion audio started_at is required")
	}
	return nil
}
