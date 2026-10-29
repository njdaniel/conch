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
