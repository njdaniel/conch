package schema

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
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
