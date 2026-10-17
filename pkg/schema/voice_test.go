package schema

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestVoiceGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "session, one channel-wide grant", file: "voice-session-response-v1.json", new: func() any { return new(VoiceSessionResponseV1) }},
		{name: "session, channel grant and listen-only net grant", file: "voice-session-response-v1-net.json", new: func() any { return new(VoiceSessionResponseV1) }},
		{name: "presence, not configured", file: "voice-presence-v1-not-configured.json", new: func() any { return new(VoicePresenceV1) }},
		{name: "presence, configured but unavailable", file: "voice-presence-v1-unavailable.json", new: func() any { return new(VoicePresenceV1) }},
		{name: "presence, two participants, one transmitting", file: "voice-presence-v1.json", new: func() any { return new(VoicePresenceV1) }},
		{name: "transmit report, started, channel-wide", file: "voice-transmit-report-v1-started.json", new: func() any { return new(VoiceTransmitReportV1) }},
		{name: "transmit report, stopped, channel-wide", file: "voice-transmit-report-v1-stopped.json", new: func() any { return new(VoiceTransmitReportV1) }},
		{name: "transmit report, started, net audience", file: "voice-transmit-report-v1-net.json", new: func() any { return new(VoiceTransmitReportV1) }},
		{name: "presence, one participant sharing with sound", file: "voice-presence-v1-sharing.json", new: func() any { return new(VoicePresenceV1) }},
		{name: "share claim", file: "voice-share-v1.json", new: func() any { return new(VoiceShareV1) }},
		{name: "presence, as the sharer reads it after an operator stopped the share", file: "voice-presence-v1-share-ended.json", new: func() any { return new(VoicePresenceV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			assertFixtureValidates(t, tt.file, tt.new)
		})
	}
}

func TestVoicePresenceSchemaName(t *testing.T) {
	if VoicePresenceSchemaV1 != "conch.voice_presence.v1" {
		t.Fatalf("VoicePresenceSchemaV1 = %q", VoicePresenceSchemaV1)
	}
}

func TestVoiceErrorCodes(t *testing.T) {
	tests := []struct {
		got  string
		want string
	}{
		{ErrorCodeVoiceNotConfigured, "voice_not_configured"},
		{ErrorCodeVoiceUnavailable, "voice_unavailable"},
		{ErrorCodeVoiceRequiresAuth, "voice_requires_auth"},
		{ErrorCodeVoiceNoSession, "voice_no_session"},
		{ErrorCodeVoiceReportRateLimited, "voice_report_rate_limited"},
		{ErrorCodeVoiceNotAllowed, "voice_not_allowed"},
		{ErrorCodeVoiceShareBusy, "voice_share_busy"},
		{ErrorCodeVoiceShareStale, "voice_share_stale"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("error code = %q, want %q", tt.got, tt.want)
		}
		// The codes travel in the existing Error body, so they must survive
		// it unchanged.
		raw, err := json.Marshal(Error{Code: tt.got, Message: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"code":"`+tt.want+`"`) {
			t.Errorf("Error with code %q encodes as %s", tt.got, raw)
		}
	}
}

var voiceExpiresAt = NewTimestamp(time.Date(2026, 10, 9, 12, 0, 45, 0, time.UTC))

func validVoiceRoomGrant() VoiceRoomGrant {
	return VoiceRoomGrant{
		Room:       "conch-k3m7q2x9v4t1b8n6w5z0r2c4e6",
		Token:      "fixture-token-not-a-real-credential",
		CanPublish: true,
		ExpiresAt:  voiceExpiresAt,
	}
}

func TestVoiceRoomGrantValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VoiceRoomGrant)
		wantErr string
	}{
		{name: "valid channel-wide publishing grant", mutate: func(*VoiceRoomGrant) {}},
		{name: "valid listen-only grant", mutate: func(g *VoiceRoomGrant) { g.CanPublish = false }},
		{name: "valid net audience", mutate: func(g *VoiceRoomGrant) { g.Audience = &Audience{Kind: AudienceKindNet, NetID: 3} }},
		{
			// Whisper mapping is undecided (ADR-004); the schema does not
			// pre-empt it by refusing a principals audience on a grant.
			name: "valid principals audience",
			mutate: func(g *VoiceRoomGrant) {
				g.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 7}}
			},
		},
		{name: "room names are opaque: any non-empty string", mutate: func(g *VoiceRoomGrant) { g.Room = "x" }},
		{name: "tokens are opaque: any non-empty string", mutate: func(g *VoiceRoomGrant) { g.Token = "." }},

		{name: "empty room", mutate: func(g *VoiceRoomGrant) { g.Room = "" }, wantErr: "voice room grant room is required"},
		{name: "empty token", mutate: func(g *VoiceRoomGrant) { g.Token = "" }, wantErr: "voice room grant token is required"},
		{name: "zero expires_at", mutate: func(g *VoiceRoomGrant) { g.ExpiresAt = Timestamp{} }, wantErr: "voice room grant expires_at is required"},
		{name: "audience unknown kind", mutate: func(g *VoiceRoomGrant) { g.Audience = &Audience{Kind: "channel"} }, wantErr: `audience kind "channel" is not one of net, principals`},
		{name: "audience empty kind", mutate: func(g *VoiceRoomGrant) { g.Audience = &Audience{} }, wantErr: `audience kind "" is not one of net, principals`},
		{name: "audience net without net_id", mutate: func(g *VoiceRoomGrant) { g.Audience = &Audience{Kind: AudienceKindNet} }, wantErr: "audience net_id must be positive, got 0"},
		{
			name: "audience net with principal_ids",
			mutate: func(g *VoiceRoomGrant) {
				g.Audience = &Audience{Kind: AudienceKindNet, NetID: 3, PrincipalIDs: []int64{3}}
			},
			wantErr: "audience of kind net must not list principal_ids",
		},
		{
			name:    "audience principals without ids",
			mutate:  func(g *VoiceRoomGrant) { g.Audience = &Audience{Kind: AudienceKindPrincipals} },
			wantErr: "audience of kind principals must list at least one principal_id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := validVoiceRoomGrant()
			tt.mutate(&g)
			assertValidation(t, g.Validate(), tt.wantErr)
		})
	}
}

func validVoiceSessionResponseV1() VoiceSessionResponseV1 {
	return VoiceSessionResponseV1{
		LivekitURL: "wss://voice.example",
		Identity:   "p7",
		Rooms:      []VoiceRoomGrant{validVoiceRoomGrant()},
	}
}

func TestVoiceSessionResponseV1Validate(t *testing.T) {
	listenOnlyNet := validVoiceRoomGrant()
	listenOnlyNet.Room = "conch-a1s2d3f4g5h6j7k8l9q0w1e2r3"
	listenOnlyNet.CanPublish = false
	listenOnlyNet.Audience = &Audience{Kind: AudienceKindNet, NetID: 3}

	badGrant := validVoiceRoomGrant()
	badGrant.Token = ""

	tests := []struct {
		name    string
		mutate  func(*VoiceSessionResponseV1)
		wantErr string
	}{
		{name: "valid, one channel-wide grant", mutate: func(*VoiceSessionResponseV1) {}},
		{name: "valid, channel grant and listen-only net grant", mutate: func(r *VoiceSessionResponseV1) { r.Rooms = append(r.Rooms, listenOnlyNet) }},
		{name: "valid, ws scheme", mutate: func(r *VoiceSessionResponseV1) { r.LivekitURL = "ws://localhost:7880" }},
		{name: "address is returned as configured, not parsed", mutate: func(r *VoiceSessionResponseV1) { r.LivekitURL = "not a url" }},

		{name: "empty livekit_url", mutate: func(r *VoiceSessionResponseV1) { r.LivekitURL = "" }, wantErr: "voice session livekit_url is required"},
		{name: "empty identity", mutate: func(r *VoiceSessionResponseV1) { r.Identity = "" }, wantErr: "voice session identity is required"},
		{name: "nil rooms", mutate: func(r *VoiceSessionResponseV1) { r.Rooms = nil }, wantErr: "voice session rooms must list at least one grant"},
		{name: "empty rooms", mutate: func(r *VoiceSessionResponseV1) { r.Rooms = []VoiceRoomGrant{} }, wantErr: "voice session rooms must list at least one grant"},
		{name: "first grant invalid", mutate: func(r *VoiceSessionResponseV1) { r.Rooms = []VoiceRoomGrant{badGrant} }, wantErr: "voice room grant token is required"},
		{
			name:    "later grant invalid",
			mutate:  func(r *VoiceSessionResponseV1) { r.Rooms = append(r.Rooms, badGrant) },
			wantErr: "voice room grant token is required",
		},
		{
			name: "grant with malformed audience",
			mutate: func(r *VoiceSessionResponseV1) {
				r.Rooms[0].Audience = &Audience{Kind: AudienceKindNet}
			},
			wantErr: "audience net_id must be positive, got 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validVoiceSessionResponseV1()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

var voiceJoinedAt = NewTimestamp(time.Date(2026, 10, 9, 11, 58, 2, 250_000_000, time.UTC))

func validVoiceParticipant() VoiceParticipant {
	return VoiceParticipant{PrincipalID: 3, CanPublish: true, Transmitting: true, JoinedAt: voiceJoinedAt}
}

func TestVoiceParticipantValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VoiceParticipant)
		wantErr string
	}{
		{name: "valid, publishing and transmitting", mutate: func(*VoiceParticipant) {}},
		{name: "valid, publishing and silent", mutate: func(p *VoiceParticipant) { p.Transmitting = false }},
		{name: "valid, listen-only and silent", mutate: func(p *VoiceParticipant) { p.CanPublish, p.Transmitting = false, false }},

		{name: "zero principal", mutate: func(p *VoiceParticipant) { p.PrincipalID = 0 }, wantErr: "voice participant principal_id must be positive, got 0"},
		{name: "negative principal", mutate: func(p *VoiceParticipant) { p.PrincipalID = -3 }, wantErr: "voice participant principal_id must be positive, got -3"},
		{
			name:    "listen-only but transmitting",
			mutate:  func(p *VoiceParticipant) { p.CanPublish = false },
			wantErr: "voice participant 3 cannot be transmitting without can_publish",
		},
		{name: "zero joined_at", mutate: func(p *VoiceParticipant) { p.JoinedAt = Timestamp{} }, wantErr: "voice participant joined_at is required"},

		// The screen share (issue #443).
		{name: "valid, sharing", mutate: func(p *VoiceParticipant) { p.Sharing = true }},
		{name: "valid, sharing with sound", mutate: func(p *VoiceParticipant) { p.Sharing, p.SharingAudio = true, true }},
		{name: "valid, sharing and silent", mutate: func(p *VoiceParticipant) { p.Transmitting, p.Sharing = false, true }},
		{
			name:    "listen-only but sharing",
			mutate:  func(p *VoiceParticipant) { p.CanPublish, p.Transmitting, p.Sharing = false, false, true },
			wantErr: "voice participant 3 cannot be sharing without can_publish",
		},
		{
			name:    "sound of a share without the share",
			mutate:  func(p *VoiceParticipant) { p.SharingAudio = true },
			wantErr: "voice participant 3 cannot have sharing_audio without sharing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validVoiceParticipant()
			tt.mutate(&p)
			assertValidation(t, p.Validate(), tt.wantErr)
		})
	}
}

func validVoicePresenceRoom() VoicePresenceRoom {
	second := validVoiceParticipant()
	second.PrincipalID = 7
	second.Transmitting = false
	second.JoinedAt = NewTimestamp(time.Date(2026, 10, 9, 12, 0, 31, 0, time.UTC))
	return VoicePresenceRoom{Participants: []VoiceParticipant{validVoiceParticipant(), second}}
}

func TestVoicePresenceRoomValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VoicePresenceRoom)
		wantErr string
	}{
		{name: "valid channel-wide room", mutate: func(*VoicePresenceRoom) {}},
		{name: "valid net room", mutate: func(r *VoicePresenceRoom) { r.Audience = &Audience{Kind: AudienceKindNet, NetID: 3} }},
		{name: "valid nil participants", mutate: func(r *VoicePresenceRoom) { r.Participants = nil }},
		{name: "valid empty participants", mutate: func(r *VoicePresenceRoom) { r.Participants = []VoiceParticipant{} }},

		{name: "audience unknown kind", mutate: func(r *VoicePresenceRoom) { r.Audience = &Audience{Kind: "room"} }, wantErr: `audience kind "room" is not one of net, principals`},
		{name: "audience net without net_id", mutate: func(r *VoicePresenceRoom) { r.Audience = &Audience{Kind: AudienceKindNet} }, wantErr: "audience net_id must be positive, got 0"},
		{name: "participant zero principal", mutate: func(r *VoicePresenceRoom) { r.Participants[0].PrincipalID = 0 }, wantErr: "voice participant principal_id must be positive, got 0"},
		{name: "participant zero joined_at", mutate: func(r *VoicePresenceRoom) { r.Participants[1].JoinedAt = Timestamp{} }, wantErr: "voice participant joined_at is required"},
		{
			name:    "participant listen-only but transmitting",
			mutate:  func(r *VoicePresenceRoom) { r.Participants[0].CanPublish = false },
			wantErr: "voice participant 3 cannot be transmitting without can_publish",
		},
		{
			name:    "duplicate principal",
			mutate:  func(r *VoicePresenceRoom) { r.Participants[1].PrincipalID = 3 },
			wantErr: "voice room lists principal 3 more than once",
		},
		{name: "valid, one participant sharing", mutate: func(r *VoicePresenceRoom) { r.Participants[1].Sharing = true }},
		{
			name: "two participants sharing",
			mutate: func(r *VoicePresenceRoom) {
				r.Participants[0].Sharing, r.Participants[1].Sharing = true, true
			},
			wantErr: "voice room lists principals 3 and 7 as sharing; a room has one share at a time",
		},
		{
			name:    "a participant's share is checked in the room",
			mutate:  func(r *VoicePresenceRoom) { r.Participants[1].SharingAudio = true },
			wantErr: "voice participant 7 cannot have sharing_audio without sharing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validVoicePresenceRoom()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

func validVoicePresenceV1() VoicePresenceV1 {
	return VoicePresenceV1{
		Schema:     VoicePresenceSchemaV1,
		ChannelID:  7,
		Configured: true,
		Available:  true,
		Rooms:      []VoicePresenceRoom{validVoicePresenceRoom()},
	}
}

func TestVoicePresenceV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VoicePresenceV1)
		wantErr string
	}{
		{name: "valid, two participants, one transmitting", mutate: func(*VoicePresenceV1) {}},
		{name: "valid, available with no rooms", mutate: func(p *VoicePresenceV1) { p.Rooms = []VoicePresenceRoom{} }},
		{name: "valid, available with nil rooms", mutate: func(p *VoicePresenceV1) { p.Rooms = nil }},
		{name: "valid, not configured", mutate: func(p *VoicePresenceV1) { p.Configured, p.Available, p.Rooms = false, false, nil }},
		{name: "valid, configured but unavailable", mutate: func(p *VoicePresenceV1) { p.Available, p.Rooms = false, nil }},
		{
			// The same principal may be in several rooms (one connection per
			// room); uniqueness is per room.
			name: "valid, same principal in two rooms",
			mutate: func(p *VoicePresenceV1) {
				net := validVoicePresenceRoom()
				net.Audience = &Audience{Kind: AudienceKindNet, NetID: 3}
				p.Rooms = append(p.Rooms, net)
			},
		},

		{name: "empty schema", mutate: func(p *VoicePresenceV1) { p.Schema = "" }, wantErr: `voice presence schema must be "conch.voice_presence.v1", got ""`},
		{name: "message schema", mutate: func(p *VoicePresenceV1) { p.Schema = MessageSchemaV2 }, wantErr: `voice presence schema must be "conch.voice_presence.v1", got "conch.message.v2"`},
		{name: "future schema version", mutate: func(p *VoicePresenceV1) { p.Schema = "conch.voice_presence.v2" }, wantErr: `got "conch.voice_presence.v2"`},
		{name: "zero channel", mutate: func(p *VoicePresenceV1) { p.ChannelID = 0 }, wantErr: "voice presence channel_id must be positive, got 0"},
		{name: "negative channel", mutate: func(p *VoicePresenceV1) { p.ChannelID = -7 }, wantErr: "voice presence channel_id must be positive, got -7"},
		{
			name:    "available but not configured",
			mutate:  func(p *VoicePresenceV1) { p.Configured, p.Rooms = false, nil },
			wantErr: "voice presence cannot be available when not configured",
		},
		{
			name:    "not configured with rooms",
			mutate:  func(p *VoicePresenceV1) { p.Configured, p.Available = false, false },
			wantErr: "voice presence must list no rooms when not available",
		},
		{
			name:    "unavailable with rooms",
			mutate:  func(p *VoicePresenceV1) { p.Available = false },
			wantErr: "voice presence must list no rooms when not available",
		},
		{
			name:    "unavailable with an empty room",
			mutate:  func(p *VoicePresenceV1) { p.Available, p.Rooms = false, []VoicePresenceRoom{{}} },
			wantErr: "voice presence must list no rooms when not available",
		},
		{name: "room participant zero principal", mutate: func(p *VoicePresenceV1) { p.Rooms[0].Participants[0].PrincipalID = 0 }, wantErr: "voice participant principal_id must be positive, got 0"},
		{name: "room participant zero joined_at", mutate: func(p *VoicePresenceV1) { p.Rooms[0].Participants[0].JoinedAt = Timestamp{} }, wantErr: "voice participant joined_at is required"},
		{name: "room duplicate principal", mutate: func(p *VoicePresenceV1) { p.Rooms[0].Participants[1].PrincipalID = 3 }, wantErr: "voice room lists principal 3 more than once"},
		{
			name:    "room participant listen-only but transmitting",
			mutate:  func(p *VoicePresenceV1) { p.Rooms[0].Participants[0].CanPublish = false },
			wantErr: "voice participant 3 cannot be transmitting without can_publish",
		},
		{name: "room audience malformed", mutate: func(p *VoicePresenceV1) { p.Rooms[0].Audience = &Audience{Kind: AudienceKindNet} }, wantErr: "audience net_id must be positive, got 0"},

		{name: "valid, the sharer's own share ended", mutate: func(p *VoicePresenceV1) { p.ShareEnded = validVoiceShareEnded() }},
		{
			// A share stopped during an outage has ended all the same.
			name:   "valid, share ended while unavailable",
			mutate: func(p *VoicePresenceV1) { p.Available, p.Rooms, p.ShareEnded = false, nil, validVoiceShareEnded() },
		},
		{
			name: "share ended when not configured",
			mutate: func(p *VoicePresenceV1) {
				p.Configured, p.Available, p.Rooms, p.ShareEnded = false, false, nil, validVoiceShareEnded()
			},
			wantErr: "voice presence cannot carry share_ended when not configured",
		},
		{
			name:    "share ended with an unknown reason",
			mutate:  func(p *VoicePresenceV1) { p.ShareEnded = validVoiceShareEnded(); p.ShareEnded.Reason = "kicked" },
			wantErr: `voice share end reason "kicked" is not one of`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validVoicePresenceV1()
			tt.mutate(&p)
			assertValidation(t, p.Validate(), tt.wantErr)
		})
	}
}

// forbiddenPresenceKeys are the JSON keys that must never appear in a
// presence document: a reader of presence must learn nothing that helps join
// a room (ADR-004; design §6).
var forbiddenPresenceKeys = map[string]bool{"token": true, "room": true}

// forbiddenPresenceFields are the Go field names the presence types must not
// declare, so that no future edit can smuggle one in under another JSON tag.
var forbiddenPresenceFields = map[string]bool{"Token": true, "Room": true, "RoomName": true}

// TestVoicePresenceCarriesNoTokenOrRoom checks the presence document two
// ways: the JSON a fully populated snapshot (channel-wide and net rooms, every
// field set) marshals to has no key named token or room at any depth, and the
// Go types reachable from VoicePresenceV1 declare no field that could carry
// one.
func TestVoicePresenceCarriesNoTokenOrRoom(t *testing.T) {
	p := validVoicePresenceV1()
	net := validVoicePresenceRoom()
	net.Audience = &Audience{Kind: AudienceKindNet, NetID: 3}
	whisper := validVoicePresenceRoom()
	whisper.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 7}}
	p.Rooms = append(p.Rooms, net, whisper)
	if err := p.Validate(); err != nil {
		t.Fatalf("fully populated presence does not validate: %v", err)
	}

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	walkJSONKeys(doc, "$", func(path, key string) {
		if forbiddenPresenceKeys[key] {
			t.Errorf("presence JSON has forbidden key %q at %s", key, path)
		}
	})

	walkStructFields(reflect.TypeOf(VoicePresenceV1{}), map[reflect.Type]bool{}, func(owner reflect.Type, f reflect.StructField) {
		if forbiddenPresenceFields[f.Name] {
			t.Errorf("%s declares forbidden field %s", owner, f.Name)
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if forbiddenPresenceKeys[tag] {
			t.Errorf("%s.%s has forbidden json tag %q", owner, f.Name, tag)
		}
	})
}

// walkJSONKeys calls visit for every object key in a decoded JSON document,
// at any depth, with a JSONPath-like location for the error message.
func walkJSONKeys(v any, path string, visit func(path, key string)) {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			visit(path, k)
			walkJSONKeys(child, path+"."+k, visit)
		}
	case []any:
		for i, child := range node {
			walkJSONKeys(child, path+"["+strconv.Itoa(i)+"]", visit)
		}
	}
}

// walkStructFields calls visit for every field of t and of every struct type
// reachable from it through fields, pointers, slices, arrays and maps, each
// struct type once.
func walkStructFields(t reflect.Type, seen map[reflect.Type]bool, visit func(owner reflect.Type, f reflect.StructField)) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		visit(t, f)
		walkStructFields(f.Type, seen, visit)
	}
}

func TestVoicePresenceTypesAreReached(t *testing.T) {
	// Guard the reflection walk itself: if it ever stopped descending, the
	// no-token test above would pass vacuously.
	seen := map[reflect.Type]bool{}
	walkStructFields(reflect.TypeOf(VoicePresenceV1{}), seen, func(reflect.Type, reflect.StructField) {})
	for _, want := range []reflect.Type{
		reflect.TypeOf(VoicePresenceV1{}),
		reflect.TypeOf(VoicePresenceRoom{}),
		reflect.TypeOf(VoiceParticipant{}),
		reflect.TypeOf(VoiceShareEnded{}),
		reflect.TypeOf(Audience{}),
		reflect.TypeOf(Timestamp{}),
	} {
		if !seen[want] {
			t.Errorf("walk did not reach %s", want)
		}
	}
}

func TestVoiceEmptyListsEncodeAsArrays(t *testing.T) {
	p := validVoicePresenceV1()
	p.Rooms = []VoicePresenceRoom{}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"rooms":[]`) {
		t.Errorf("presence with no rooms = %s, want rooms:[]", raw)
	}

	room := VoicePresenceRoom{Participants: []VoiceParticipant{}}
	raw, err = json.Marshal(room)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"participants":[]}` {
		t.Errorf("room with nobody in it = %s, want participants:[]", raw)
	}

	// A grant and a participant always spell out their booleans: a reader
	// must not have to know that an omitted can_publish means false.
	raw, err = json.Marshal(VoiceRoomGrant{Room: "r", Token: "t", ExpiresAt: voiceExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"can_publish":false`) {
		t.Errorf("listen-only grant = %s, want can_publish:false", raw)
	}
	raw, err = json.Marshal(VoiceParticipant{PrincipalID: 3, JoinedAt: voiceJoinedAt})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"can_publish":false`) || !strings.Contains(string(raw), `"transmitting":false`) {
		t.Errorf("silent listen-only participant = %s, want can_publish:false and transmitting:false", raw)
	}
}

func TestVoicePresenceUnknownFieldsTolerated(t *testing.T) {
	// Forward compatibility: a v1 reader must accept a document from a newer
	// server that added an optional field, and still validate it.
	raw := `{"schema":"conch.voice_presence.v1","channel_id":7,"configured":true,"available":true,` +
		`"rooms":[{"participants":[{"principal_id":3,"can_publish":true,"transmitting":false,"joined_at":"2026-10-09T12:00:00.000Z","muted_by":0}],"topic":"ops"}],"poll_interval_ms":500}`
	var p VoicePresenceV1
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if len(p.Rooms) != 1 || len(p.Rooms[0].Participants) != 1 || p.Rooms[0].Participants[0].PrincipalID != 3 {
		t.Errorf("decoded presence lost known fields: %+v", p)
	}
}

// One room per audience: a session with two grants for the same audience, or
// a presence document with two rooms for it, would leave a reader unable to
// say which is the channel's room. Audiences are compared by meaning, so the
// same principals in another order are the same audience.
func TestVoiceOneRoomPerAudience(t *testing.T) {
	net3 := &Audience{Kind: AudienceKindNet, NetID: 3}
	net4 := &Audience{Kind: AudienceKindNet, NetID: 4}
	whisperA := &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 7}}
	whisperB := &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{7, 3}}
	whisperC := &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 9}}
	tests := []struct {
		name      string
		audiences []*Audience
		wantErr   bool
	}{
		{"channel only", []*Audience{nil}, false},
		{"channel and two nets", []*Audience{nil, net3, net4}, false},
		{"channel, net and whisper", []*Audience{nil, net3, whisperA}, false},
		{"two different whispers", []*Audience{whisperA, whisperC}, false},
		{"channel twice", []*Audience{nil, nil}, true},
		{"same net twice", []*Audience{nil, net3, net3}, true},
		{"same principals in another order", []*Audience{whisperA, whisperB}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := validVoiceSessionResponseV1()
			session.Rooms = nil
			presence := validVoicePresenceV1()
			presence.Rooms = nil
			for _, a := range tt.audiences {
				g := validVoiceRoomGrant()
				g.Audience = a
				session.Rooms = append(session.Rooms, g)
				r := validVoicePresenceRoom()
				r.Audience = a
				presence.Rooms = append(presence.Rooms, r)
			}
			for what, err := range map[string]error{"session": session.Validate(), "presence": presence.Validate()} {
				if tt.wantErr && (err == nil || !strings.Contains(err.Error(), "the same audience")) {
					t.Errorf("%s: err = %v, want a same-audience error", what, err)
				}
				if !tt.wantErr && err != nil {
					t.Errorf("%s: err = %v, want nil", what, err)
				}
			}
		})
	}
}

// The fields a presence document can carry are an exact list. Checking only
// for fields named "token" or "room" would miss a credential added under
// another name, so any new field has to be added here on purpose, by someone
// who has asked whether a presence reader should see it.
func TestVoicePresenceFieldsAreAnExactList(t *testing.T) {
	got := map[string]bool{}
	walkStructFields(reflect.TypeOf(VoicePresenceV1{}), map[reflect.Type]bool{}, func(_ reflect.Type, f reflect.StructField) {
		if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
			got[tag] = true
		}
	})
	want := []string{
		"schema", "channel_id", "configured", "available", "rooms", // the document
		"audience", "participants", // a room
		"kind", "net_id", "principal_ids", // an audience
		"principal_id", "can_publish", "transmitting", "joined_at", // a participant
		"sharing", "sharing_audio", // a participant's screen share (issue #443)
		// Why the reader's own share ended (issue #487): in the sharer's
		// document alone, and naming nobody.
		"share_ended", "reason", "started_at", "ended_at",
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("presence no longer carries %q; update this list if that is intended", k)
		}
		delete(got, k)
	}
	for k := range got {
		t.Errorf("presence can now carry %q, which is not on the reviewed list", k)
	}
}

func TestVoiceTransmitStateValues(t *testing.T) {
	tests := []struct {
		state VoiceTransmitState
		want  string
	}{
		{VoiceTransmitStateStarted, "started"},
		{VoiceTransmitStateStopped, "stopped"},
	}
	for _, tt := range tests {
		if string(tt.state) != tt.want {
			t.Errorf("state = %q, want %q", tt.state, tt.want)
		}
		if !tt.state.Valid() {
			t.Errorf("%q.Valid() = false, want true", tt.state)
		}
	}
}

func TestVoiceTransmitReportV1Validate(t *testing.T) {
	net3 := &Audience{Kind: AudienceKindNet, NetID: 3}
	tests := []struct {
		name    string
		report  VoiceTransmitReportV1
		wantErr string
	}{
		{name: "valid started, channel-wide", report: VoiceTransmitReportV1{State: VoiceTransmitStateStarted}},
		{name: "valid stopped, channel-wide", report: VoiceTransmitReportV1{State: VoiceTransmitStateStopped}},
		{name: "valid started, net audience", report: VoiceTransmitReportV1{State: VoiceTransmitStateStarted, Audience: net3}},
		{name: "valid stopped, net audience", report: VoiceTransmitReportV1{State: VoiceTransmitStateStopped, Audience: net3}},
		{
			// As on VoiceRoomGrant: whisper mapping is undecided (ADR-004), so
			// the schema does not pre-empt it by refusing a principals audience.
			name: "valid principals audience",
			report: VoiceTransmitReportV1{
				State:    VoiceTransmitStateStarted,
				Audience: &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 7}},
			},
		},

		{name: "empty state", report: VoiceTransmitReportV1{}, wantErr: `voice transmit report state "" is not one of started, stopped`},
		{name: "empty state with a valid audience", report: VoiceTransmitReportV1{Audience: net3}, wantErr: `voice transmit report state "" is not one of started, stopped`},
		{name: "unknown state", report: VoiceTransmitReportV1{State: "paused"}, wantErr: `voice transmit report state "paused" is not one of started, stopped`},
		{name: "presence's word is not a state", report: VoiceTransmitReportV1{State: "transmitting"}, wantErr: `voice transmit report state "transmitting" is not one of started, stopped`},
		{name: "wrong-case state", report: VoiceTransmitReportV1{State: "Started"}, wantErr: `voice transmit report state "Started" is not one of started, stopped`},
		{name: "upper-case state", report: VoiceTransmitReportV1{State: "STOPPED"}, wantErr: `voice transmit report state "STOPPED" is not one of started, stopped`},
		{name: "state with surrounding space", report: VoiceTransmitReportV1{State: " started"}, wantErr: `voice transmit report state " started" is not one of started, stopped`},

		{
			name:    "audience unknown kind",
			report:  VoiceTransmitReportV1{State: VoiceTransmitStateStarted, Audience: &Audience{Kind: "channel"}},
			wantErr: `audience kind "channel" is not one of net, principals`,
		},
		{
			name:    "audience empty kind",
			report:  VoiceTransmitReportV1{State: VoiceTransmitStateStarted, Audience: &Audience{}},
			wantErr: `audience kind "" is not one of net, principals`,
		},
		{
			name:    "audience net without net_id",
			report:  VoiceTransmitReportV1{State: VoiceTransmitStateStarted, Audience: &Audience{Kind: AudienceKindNet}},
			wantErr: "audience net_id must be positive, got 0",
		},
		{
			name:    "audience net with negative net_id",
			report:  VoiceTransmitReportV1{State: VoiceTransmitStateStopped, Audience: &Audience{Kind: AudienceKindNet, NetID: -3}},
			wantErr: "audience net_id must be positive, got -3",
		},
		{
			name: "audience net with principal_ids",
			report: VoiceTransmitReportV1{
				State:    VoiceTransmitStateStarted,
				Audience: &Audience{Kind: AudienceKindNet, NetID: 3, PrincipalIDs: []int64{3}},
			},
			wantErr: "audience of kind net must not list principal_ids",
		},
		{
			name:    "audience principals without ids",
			report:  VoiceTransmitReportV1{State: VoiceTransmitStateStarted, Audience: &Audience{Kind: AudienceKindPrincipals}},
			wantErr: "audience of kind principals must list at least one principal_id",
		},
		{
			name: "audience principals with net_id",
			report: VoiceTransmitReportV1{
				State:    VoiceTransmitStateStarted,
				Audience: &Audience{Kind: AudienceKindPrincipals, NetID: 3, PrincipalIDs: []int64{3, 7}},
			},
			wantErr: "audience of kind principals must not carry net_id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.report.Validate(), tt.wantErr)
		})
	}
}

func TestDecodeVoiceTransmitReportV1Accepts(t *testing.T) {
	net3 := &Audience{Kind: AudienceKindNet, NetID: 3}
	tests := []struct {
		name string
		body string
		want VoiceTransmitReportV1
	}{
		{name: "only state, started", body: `{"state":"started"}`, want: VoiceTransmitReportV1{State: VoiceTransmitStateStarted}},
		{name: "only state, stopped", body: `{"state":"stopped"}`, want: VoiceTransmitReportV1{State: VoiceTransmitStateStopped}},
		{
			name: "net audience",
			body: `{"state":"started","audience":{"kind":"net","net_id":3}}`,
			want: VoiceTransmitReportV1{State: VoiceTransmitStateStarted, Audience: net3},
		},
		{
			name: "audience before state",
			body: `{"audience":{"kind":"net","net_id":3},"state":"stopped"}`,
			want: VoiceTransmitReportV1{State: VoiceTransmitStateStopped, Audience: net3},
		},
		{name: "null audience is the whole channel", body: `{"state":"started","audience":null}`, want: VoiceTransmitReportV1{State: VoiceTransmitStateStarted}},
		{name: "surrounding whitespace", body: "\n  {\n  \"state\": \"stopped\"\n}\n", want: VoiceTransmitReportV1{State: VoiceTransmitStateStopped}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeVoiceTransmitReportV1(strings.NewReader(tt.body))
			if err != nil {
				t.Fatalf("DecodeVoiceTransmitReportV1(%s) = %v, want nil", tt.body, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DecodeVoiceTransmitReportV1(%s) = %+v, want %+v", tt.body, got, tt.want)
			}
		})
	}
}

// A report is the one voice shape decoded strictly. Each rejected body here
// is one a lenient decoder would have accepted while dropping the extra field,
// leaving a captured request that appears to say when a transmission
// happened, who made it, or where.
func TestDecodeVoiceTransmitReportV1RejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{name: "at", body: `{"state":"started","at":"2026-10-09T12:00:00.000Z"}`, field: "at"},
		{name: "time", body: `{"state":"started","time":"2026-10-09T12:00:00.000Z"}`, field: "time"},
		{name: "principal_id", body: `{"state":"started","principal_id":3}`, field: "principal_id"},
		{name: "room", body: `{"state":"started","room":"conch-k3m7q2x9v4t1b8n6w5z0r2c4e6"}`, field: "room"},
		{name: "token", body: `{"state":"started","token":"fixture-token-not-a-real-credential"}`, field: "token"},
		{name: "seq", body: `{"state":"started","seq":7}`, field: "seq"},

		{name: "started_at", body: `{"state":"started","started_at":"2026-10-09T12:00:00.000Z"}`, field: "started_at"},
		{name: "identity", body: `{"state":"started","identity":"p7"}`, field: "identity"},
		{name: "channel_id", body: `{"state":"started","channel_id":7}`, field: "channel_id"},
		{name: "schema", body: `{"schema":"conch.voice_transmit_report.v1","state":"started"}`, field: "schema"},

		{name: "extra field on a stopped report", body: `{"state":"stopped","at":"2026-10-09T12:00:00.000Z"}`, field: "at"},
		{name: "extra field before state", body: `{"token":"x","state":"started"}`, field: "token"},
		{name: "extra field with a null value", body: `{"state":"started","at":null}`, field: "at"},
		{name: "extra field beside a valid audience", body: `{"state":"started","audience":{"kind":"net","net_id":3},"seq":1}`, field: "seq"},
		{name: "extra field inside audience", body: `{"state":"started","audience":{"kind":"net","net_id":3,"room":"x"}}`, field: "room"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeVoiceTransmitReportV1(strings.NewReader(tt.body))
			assertVoiceTransmitReportRejected(t, got, err, `unknown field "`+tt.field+`"`)
		})
	}
}

func TestDecodeVoiceTransmitReportV1RejectsMalformedBodies(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "empty body", body: ``, wantErr: "decode voice transmit report: EOF"},
		{name: "truncated object", body: `{"state":"started"`, wantErr: "decode voice transmit report: unexpected EOF"},
		{name: "not JSON", body: `started`, wantErr: "decode voice transmit report: invalid character"},
		{name: "an array", body: `[{"state":"started"}]`, wantErr: "decode voice transmit report: json: cannot unmarshal array"},
		{name: "a string", body: `"started"`, wantErr: "decode voice transmit report: json: cannot unmarshal string"},
		{name: "state of the wrong type", body: `{"state":1}`, wantErr: "decode voice transmit report: json: cannot unmarshal number"},
		{name: "audience of the wrong type", body: `{"state":"started","audience":"net"}`, wantErr: "decode voice transmit report: json: cannot unmarshal string"},

		{name: "two reports", body: `{"state":"started"}{"state":"stopped"}`, wantErr: "voice transmit report body must contain one JSON object"},
		{name: "a report then a value", body: `{"state":"started"} null`, wantErr: "voice transmit report body must contain one JSON object"},
		{name: "a report then garbage", body: `{"state":"started"} x`, wantErr: "decode voice transmit report: invalid character"},

		{name: "null", body: `null`, wantErr: `voice transmit report state "" is not one of started, stopped`},
		{name: "empty object", body: `{}`, wantErr: `voice transmit report state "" is not one of started, stopped`},
		{name: "null state", body: `{"state":null}`, wantErr: `voice transmit report state "" is not one of started, stopped`},
		{name: "empty state", body: `{"state":""}`, wantErr: `voice transmit report state "" is not one of started, stopped`},
		{name: "unknown state", body: `{"state":"paused"}`, wantErr: `voice transmit report state "paused" is not one of started, stopped`},
		{name: "wrong-case state", body: `{"state":"Started"}`, wantErr: `voice transmit report state "Started" is not one of started, stopped`},
		{name: "audience only", body: `{"audience":{"kind":"net","net_id":3}}`, wantErr: `voice transmit report state "" is not one of started, stopped`},
		{name: "audience unknown kind", body: `{"state":"started","audience":{"kind":"channel"}}`, wantErr: `audience kind "channel" is not one of net, principals`},
		{name: "audience empty object", body: `{"state":"started","audience":{}}`, wantErr: `audience kind "" is not one of net, principals`},
		{name: "audience net without net_id", body: `{"state":"started","audience":{"kind":"net"}}`, wantErr: "audience net_id must be positive, got 0"},
		{name: "audience net with principal_ids", body: `{"state":"started","audience":{"kind":"net","net_id":3,"principal_ids":[3]}}`, wantErr: "audience of kind net must not list principal_ids"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeVoiceTransmitReportV1(strings.NewReader(tt.body))
			assertVoiceTransmitReportRejected(t, got, err, tt.wantErr)
		})
	}
}

// assertVoiceTransmitReportRejected checks a failed decode: an error with the
// package prefix that contains wantErr, and the zero report, so a caller that
// ignored the error is not left holding a half-decoded one.
func assertVoiceTransmitReportRejected(t *testing.T, got VoiceTransmitReportV1, err error, wantErr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("DecodeVoiceTransmitReportV1 = %+v, nil; want error containing %q", got, wantErr)
	}
	if !strings.HasPrefix(err.Error(), "schema: ") {
		t.Errorf("error %q lacks the schema: prefix", err)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("error = %q, want it to contain %q", err, wantErr)
	}
	if !reflect.DeepEqual(got, VoiceTransmitReportV1{}) {
		t.Errorf("rejected decode returned %+v, want the zero report", got)
	}
}

// The decoder does not bound its input; the handler does, and has to be able
// to tell "too large" from "malformed". So an error from the reader must
// survive wrapping, whether it arrives inside the report or after it.
func TestDecodeVoiceTransmitReportV1WrapsReaderErrors(t *testing.T) {
	errRead := errors.New("read failed")
	tests := []struct {
		name   string
		reader io.Reader
	}{
		{name: "before the report", reader: iotest.ErrReader(errRead)},
		{name: "inside the report", reader: io.MultiReader(strings.NewReader(`{"state":`), iotest.ErrReader(errRead))},
		{name: "after the report", reader: io.MultiReader(strings.NewReader(`{"state":"started"}`), iotest.ErrReader(errRead))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeVoiceTransmitReportV1(tt.reader)
			if !errors.Is(err, errRead) {
				t.Fatalf("err = %v, want it to wrap the reader's error", err)
			}
			assertVoiceTransmitReportRejected(t, got, err, "decode voice transmit report: read failed")
		})
	}

	for _, body := range []string{
		`{"state":"started","audience":{"kind":"principals","principal_ids":[` + strings.Repeat("3,", 64) + `3]}}`,
		`{"state":"started"}` + strings.Repeat(" ", 128),
	} {
		limited := http.MaxBytesReader(nil, io.NopCloser(strings.NewReader(body)), 64)
		_, err := DecodeVoiceTransmitReportV1(limited)
		var tooLarge *http.MaxBytesError
		if !errors.As(err, &tooLarge) {
			t.Errorf("body of %d bytes behind a 64-byte limit: err = %v, want an *http.MaxBytesError", len(body), err)
		}
	}
}

// Every published report fixture must pass the decoder a handler will use,
// not only the round trip of the golden test.
func TestVoiceTransmitReportFixturesDecodeStrictly(t *testing.T) {
	tests := []struct {
		file string
		want VoiceTransmitReportV1
	}{
		{file: "voice-transmit-report-v1-started.json", want: VoiceTransmitReportV1{State: VoiceTransmitStateStarted}},
		{file: "voice-transmit-report-v1-stopped.json", want: VoiceTransmitReportV1{State: VoiceTransmitStateStopped}},
		{
			file: "voice-transmit-report-v1-net.json",
			want: VoiceTransmitReportV1{State: VoiceTransmitStateStarted, Audience: &Audience{Kind: AudienceKindNet, NetID: 3}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tt.file)) // #nosec G304 -- fixture name comes from this package's own test table
			if err != nil {
				t.Fatalf("open fixture: %v", err)
			}
			defer func() { _ = f.Close() }()
			got, err := DecodeVoiceTransmitReportV1(f)
			if err != nil {
				t.Fatalf("DecodeVoiceTransmitReportV1 = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DecodeVoiceTransmitReportV1 = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestVoiceTransmitReportEncoding(t *testing.T) {
	tests := []struct {
		name   string
		report VoiceTransmitReportV1
		want   string
	}{
		// A channel-wide report has no audience key at all, as on a grant and
		// a message: there is no "channel" kind and no null.
		{name: "channel-wide started", report: VoiceTransmitReportV1{State: VoiceTransmitStateStarted}, want: `{"state":"started"}`},
		{name: "channel-wide stopped", report: VoiceTransmitReportV1{State: VoiceTransmitStateStopped}, want: `{"state":"stopped"}`},
		{
			name:   "net audience",
			report: VoiceTransmitReportV1{State: VoiceTransmitStateStarted, Audience: &Audience{Kind: AudienceKindNet, NetID: 3}},
			want:   `{"state":"started","audience":{"kind":"net","net_id":3}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.report)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tt.want {
				t.Errorf("report encodes as %s, want %s", raw, tt.want)
			}
			// What the client's encoder writes, the server's decoder accepts.
			back, err := DecodeVoiceTransmitReportV1(strings.NewReader(string(raw)))
			if err != nil {
				t.Fatalf("decode of %s = %v, want nil", raw, err)
			}
			if !reflect.DeepEqual(back, tt.report) {
				t.Errorf("round trip = %+v, want %+v", back, tt.report)
			}
		})
	}
}

// The fields a report can carry are an exact list, as for presence. A report
// has no time and no sequence number on purpose (design note conch-voice.md
// §6 and §11 decision 7), and names no principal, room or token. A field
// added to VoiceTransmitReportV1 or to Audience shows up here, so it has to
// be added on purpose by someone who has asked whether a client should be
// able to assert it.
func TestVoiceTransmitReportFieldsAreAnExactList(t *testing.T) {
	got := map[string]bool{}
	walkStructFields(reflect.TypeOf(VoiceTransmitReportV1{}), map[reflect.Type]bool{}, func(_ reflect.Type, f reflect.StructField) {
		if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
			got[tag] = true
		}
	})
	want := []string{
		"state", "audience", // the report
		"kind", "net_id", "principal_ids", // an audience
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("a transmit report no longer carries %q; update this list if that is intended", k)
		}
		delete(got, k)
	}
	for k := range got {
		t.Errorf("a transmit report can now carry %q, which is not on the reviewed list", k)
	}
}

func TestVoiceShareV1Validate(t *testing.T) {
	valid := func() VoiceShareV1 {
		return VoiceShareV1{PrincipalID: 3, StartedAt: NewTimestamp(time.Date(2026, 10, 16, 12, 0, 4, 125e6, time.UTC)), Audio: true}
	}
	tests := []struct {
		name    string
		mutate  func(*VoiceShareV1)
		wantErr string
	}{
		{name: "valid, with sound", mutate: func(*VoiceShareV1) {}},
		{name: "valid, without sound", mutate: func(s *VoiceShareV1) { s.Audio = false }},
		{name: "zero principal", mutate: func(s *VoiceShareV1) { s.PrincipalID = 0 }, wantErr: "voice share principal_id must be positive, got 0"},
		{name: "negative principal", mutate: func(s *VoiceShareV1) { s.PrincipalID = -1 }, wantErr: "voice share principal_id must be positive, got -1"},
		{name: "zero started_at", mutate: func(s *VoiceShareV1) { s.StartedAt = Timestamp{} }, wantErr: "voice share started_at is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := valid()
			tt.mutate(&s)
			assertValidation(t, s.Validate(), tt.wantErr)
		})
	}
}

// TestVoiceShareEncoding: the claim always spells out audio, and its fields
// are an exact list with no room and no token at any depth: it grants the
// connection the caller already has, and nothing in it helps anyone join a
// room.
func TestVoiceShareEncoding(t *testing.T) {
	raw, err := json.Marshal(VoiceShareV1{PrincipalID: 3, StartedAt: voiceJoinedAt})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"audio":false`) {
		t.Errorf("share without sound = %s, want audio:false", raw)
	}
	got := map[string]bool{}
	walkStructFields(reflect.TypeOf(VoiceShareV1{}), map[reflect.Type]bool{}, func(_ reflect.Type, f reflect.StructField) {
		if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
			got[tag] = true
		}
	})
	want := map[string]bool{"principal_id": true, "started_at": true, "audio": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a share carries %v, want exactly %v", got, want)
	}
}

// TestVoiceParticipantSharingIsOptional: a participant who is not sharing is
// encoded exactly as before issue #443, so a document with nobody sharing is
// byte for byte what an older server sent, and an older document reads as
// nobody sharing.
func TestVoiceParticipantSharingIsOptional(t *testing.T) {
	raw, err := json.Marshal(validVoiceParticipant())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sharing") {
		t.Errorf("a participant who is not sharing encodes as %s, want no sharing fields", raw)
	}
	var back VoiceParticipant
	if err := json.Unmarshal([]byte(`{"principal_id":3,"can_publish":true,"transmitting":false,"joined_at":"2026-10-09T12:00:00.000Z"}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Sharing || back.SharingAudio {
		t.Errorf("an older participant decodes as sharing: %+v", back)
	}
}

func validVoiceShareEnded() *VoiceShareEnded {
	return &VoiceShareEnded{
		Reason:    VoiceShareEndOperator,
		StartedAt: NewTimestamp(time.Date(2026, 10, 16, 12, 0, 4, 125e6, time.UTC)),
		EndedAt:   NewTimestamp(time.Date(2026, 10, 16, 12, 3, 40, 500e6, time.UTC)),
	}
}

func TestVoiceShareEndReasonValues(t *testing.T) {
	tests := []struct {
		reason VoiceShareEndReason
		want   string
		valid  bool
	}{
		{VoiceShareEndSelf, "self", true},
		{VoiceShareEndOperator, "operator", true},
		{VoiceShareEndLeft, "left", true},
		{VoiceShareEndAccessLost, "access_lost", true},
		{VoiceShareEndRotated, "rotated", true},
		{VoiceShareEndIdle, "idle", true},
		{"", "", false},
		{"Operator", "Operator", false},
		{"kicked", "kicked", false},
	}
	for _, tt := range tests {
		if string(tt.reason) != tt.want {
			t.Errorf("reason = %q, want %q", tt.reason, tt.want)
		}
		if got := tt.reason.Valid(); got != tt.valid {
			t.Errorf("%q.Valid() = %v, want %v", tt.reason, got, tt.valid)
		}
	}
}

func TestVoiceShareEndedValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VoiceShareEnded)
		wantErr string
	}{
		{name: "valid", mutate: func(*VoiceShareEnded) {}},
		{name: "valid, ended the moment it started", mutate: func(e *VoiceShareEnded) { e.EndedAt = e.StartedAt }},
		{name: "empty reason", mutate: func(e *VoiceShareEnded) { e.Reason = "" }, wantErr: `voice share end reason "" is not one of`},
		{name: "zero started_at", mutate: func(e *VoiceShareEnded) { e.StartedAt = Timestamp{} }, wantErr: "voice share end started_at is required"},
		{name: "zero ended_at", mutate: func(e *VoiceShareEnded) { e.EndedAt = Timestamp{} }, wantErr: "voice share end ended_at is required"},
		{
			name:    "ended before it started",
			mutate:  func(e *VoiceShareEnded) { e.EndedAt = NewTimestamp(e.StartedAt.Time().Add(-time.Millisecond)) },
			wantErr: "voice share end ended_at is before started_at",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validVoiceShareEnded()
			tt.mutate(e)
			assertValidation(t, e.Validate(), tt.wantErr)
		})
	}
}

// TestVoicePresenceShareEndedIsOptional: a document without share_ended, which
// is every reader's but the sharer's, is encoded exactly as before issue #487,
// and an older document reads as no share ended.
func TestVoicePresenceShareEndedIsOptional(t *testing.T) {
	raw, err := json.Marshal(validVoicePresenceV1())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "share_ended") {
		t.Errorf("presence with no share ended encodes as %s, want no share_ended", raw)
	}
	var back VoicePresenceV1
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ShareEnded != nil {
		t.Errorf("an older document decodes with share_ended %+v", back.ShareEnded)
	}
}

// TestParseVoiceShareClaim: the release parameter reads the started_at a claim
// gave, in any RFC 3339 form, as the same instant at millisecond precision,
// and refuses anything else.
func TestParseVoiceShareClaim(t *testing.T) {
	want := NewTimestamp(time.Date(2026, 10, 16, 12, 0, 4, 125e6, time.UTC))
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "as the claim wrote it", in: "2026-10-16T12:00:04.125Z"},
		{name: "another offset", in: "2026-10-16T14:00:04.125+02:00"},
		{name: "finer precision", in: "2026-10-16T12:00:04.125999Z"},
		{name: "empty", in: "", wantErr: "invalid timestamp"},
		{name: "not a time", in: "yesterday", wantErr: "invalid timestamp"},
		{name: "a number", in: "1792152004125", wantErr: "invalid timestamp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVoiceShareClaim(tt.in)
			assertValidation(t, err, tt.wantErr)
			if tt.wantErr == "" && got != want {
				t.Errorf("ParseVoiceShareClaim(%q) = %v, want %v", tt.in, got.Time(), want.Time())
			}
		})
	}
	if VoiceShareClaimParam != "started_at" {
		t.Errorf("VoiceShareClaimParam = %q", VoiceShareClaimParam)
	}
}
