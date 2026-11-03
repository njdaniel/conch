package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestVoiceOccupancyGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{name: "two channels, one with people and one without", file: "voice-occupancy-v1.json"},
		{name: "two channels, with who is in each (issue #1103)", file: "voice-occupancy-v1-people.json"},
		{name: "configured but unavailable", file: "voice-occupancy-v1-unavailable.json"},
		{name: "not configured", file: "voice-occupancy-v1-not-configured.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newDoc := func() any { return new(VoiceOccupancyV1) }
			assertGoldenFixture(t, tt.file, newDoc)
			assertFixtureValidates(t, tt.file, newDoc)
		})
	}
}

func TestVoiceOccupancySchemaName(t *testing.T) {
	if VoiceOccupancySchemaV1 != "conch.voice_occupancy.v1" {
		t.Fatalf("VoiceOccupancySchemaV1 = %q", VoiceOccupancySchemaV1)
	}
	if VoiceOccupancySchemaV1 == VoicePresenceSchemaV1 {
		t.Fatal("occupancy and presence share a schema name: a reader could not tell them apart")
	}
}

func validVoiceOccupancyV1() VoiceOccupancyV1 {
	return VoiceOccupancyV1{
		Schema:     VoiceOccupancySchemaV1,
		Configured: true,
		Available:  true,
		Channels: []VoiceChannelOccupancy{
			{ChannelID: 7, InVoice: 3, PrincipalIDs: []int64{3, 5, 12}},
			{ChannelID: 9, InVoice: 0, PrincipalIDs: []int64{}},
		},
	}
}

func TestVoiceOccupancyV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VoiceOccupancyV1)
		wantErr string
	}{
		{name: "valid, two channels", mutate: func(*VoiceOccupancyV1) {}},
		{name: "valid, available with no channels", mutate: func(o *VoiceOccupancyV1) { o.Channels = []VoiceChannelOccupancy{} }},
		{name: "valid, every count zero", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].InVoice, o.Channels[0].PrincipalIDs = 0, []int64{} }},
		// A document from before principal_ids (issue #1103): the count
		// stands alone, as the published fixture voice-occupancy-v1.json has it.
		{name: "valid, principal_ids not stated", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].PrincipalIDs, o.Channels[1].PrincipalIDs = nil, nil }},
		{name: "valid, one person", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].InVoice, o.Channels[0].PrincipalIDs = 1, []int64{3} }},
		{name: "valid, configured but unavailable", mutate: func(o *VoiceOccupancyV1) { o.Available, o.Channels = false, []VoiceChannelOccupancy{} }},
		{name: "valid, not configured", mutate: func(o *VoiceOccupancyV1) {
			o.Configured, o.Available, o.Channels = false, false, []VoiceChannelOccupancy{}
		}},

		{name: "empty schema", mutate: func(o *VoiceOccupancyV1) { o.Schema = "" }, wantErr: `voice occupancy schema must be "conch.voice_occupancy.v1", got ""`},
		{name: "presence schema", mutate: func(o *VoiceOccupancyV1) { o.Schema = VoicePresenceSchemaV1 }, wantErr: `got "conch.voice_presence.v1"`},
		{name: "future schema version", mutate: func(o *VoiceOccupancyV1) { o.Schema = "conch.voice_occupancy.v2" }, wantErr: `got "conch.voice_occupancy.v2"`},
		{name: "available when not configured", mutate: func(o *VoiceOccupancyV1) { o.Configured = false }, wantErr: "voice occupancy cannot be available when not configured"},
		{name: "null channels", mutate: func(o *VoiceOccupancyV1) { o.Channels = nil }, wantErr: "voice occupancy channels must be an array, not null"},
		{name: "null channels when not configured", mutate: func(o *VoiceOccupancyV1) {
			o.Configured, o.Available, o.Channels = false, false, nil
		}, wantErr: "voice occupancy channels must be an array, not null"},
		{name: "counts while unavailable", mutate: func(o *VoiceOccupancyV1) { o.Available = false }, wantErr: "voice occupancy must list no channels when not available"},
		{name: "counts while not configured", mutate: func(o *VoiceOccupancyV1) { o.Configured, o.Available = false, false }, wantErr: "voice occupancy must list no channels when not available"},
		{name: "zero channel", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].ChannelID = 0 }, wantErr: "voice occupancy channel_id must be positive, got 0"},
		{name: "negative channel", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].ChannelID = -7 }, wantErr: "voice occupancy channel_id must be positive, got -7"},
		{name: "channel twice", mutate: func(o *VoiceOccupancyV1) { o.Channels[1].ChannelID = 7 }, wantErr: "voice occupancy lists channel 7 more than once"},
		{name: "channels out of order", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].ChannelID = 11 }, wantErr: "ascending channel_id order, got 9 after 11"},
		{name: "negative count", mutate: func(o *VoiceOccupancyV1) { o.Channels[1].InVoice, o.Channels[1].PrincipalIDs = -1, nil }, wantErr: "voice occupancy in_voice of channel 9 must not be negative, got -1"},

		// principal_ids (issue #1103): stated, it is exactly the count.
		{name: "fewer principals than the count", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].PrincipalIDs = []int64{3, 5} }, wantErr: "voice occupancy of channel 7 lists 2 principal_ids for in_voice 3"},
		{name: "more principals than the count", mutate: func(o *VoiceOccupancyV1) { o.Channels[1].PrincipalIDs = []int64{4} }, wantErr: "voice occupancy of channel 9 lists 1 principal_ids for in_voice 0"},
		{name: "zero principal", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].PrincipalIDs = []int64{0, 5, 12} }, wantErr: "voice occupancy of channel 7 has a principal_id that is not positive: 0"},
		{name: "negative principal", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].PrincipalIDs = []int64{-3, 5, 12} }, wantErr: "voice occupancy of channel 7 has a principal_id that is not positive: -3"},
		{name: "principal twice", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].PrincipalIDs = []int64{3, 5, 5} }, wantErr: "voice occupancy of channel 7 lists principal 5 more than once"},
		{name: "principals out of order", mutate: func(o *VoiceOccupancyV1) { o.Channels[0].PrincipalIDs = []int64{3, 12, 5} }, wantErr: "voice occupancy of channel 7 must list principal_ids in ascending order, got 5 after 12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := validVoiceOccupancyV1()
			tt.mutate(&o)
			assertValidation(t, o.Validate(), tt.wantErr)
		})
	}
}

// TestVoiceOccupancyEncoding: a reader is given [] for no
// channels, never null, and a zero count is written out rather than omitted,
// so that "nobody" and "no information" are told apart.
func TestVoiceOccupancyEncoding(t *testing.T) {
	raw, err := json.Marshal(VoiceOccupancyV1{Schema: VoiceOccupancySchemaV1, Channels: []VoiceChannelOccupancy{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"channels":[]`) {
		t.Errorf("empty occupancy encodes as %s, want channels as []", raw)
	}
	raw, err = json.Marshal(VoiceChannelOccupancy{ChannelID: 9})
	if err != nil {
		t.Fatal(err)
	}
	// A row that does not state who is there (nil) leaves principal_ids out,
	// which is how the published fixture reads and how a writer from before
	// issue #1103 wrote; a row that states nobody writes [].
	if string(raw) != `{"channel_id":9,"in_voice":0}` {
		t.Errorf("a zero count encodes as %s, want in_voice written out and no principal_ids", raw)
	}
	raw, err = json.Marshal(VoiceChannelOccupancy{ChannelID: 9, PrincipalIDs: []int64{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"channel_id":9,"in_voice":0,"principal_ids":[]}` {
		t.Errorf("nobody encodes as %s, want principal_ids written out as []", raw)
	}
}

// forbiddenOccupancyKeys are the JSON keys the occupancy document must never
// carry at any depth: presence's (no token, no room), and anything that names
// someone in text. Who is there is said by principal id alone (issue #1103),
// as presence says it, never by name, and never as presence's participant
// objects, which carry what a person does.
var forbiddenOccupancyKeys = map[string]bool{"token": true, "room": true, "name": true, "display_name": true, "participants": true}

// forbiddenOccupancyFields are the Go field names the occupancy types must
// not declare, whatever their JSON tag.
var forbiddenOccupancyFields = map[string]bool{"Token": true, "Room": true, "RoomName": true, "Name": true, "DisplayName": true, "Participants": true}

// TestVoiceOccupancyCarriesNoTokenRoomOrName checks the document two ways, as
// TestVoicePresenceCarriesNoTokenOrRoom checks presence: the JSON of a
// populated document has no forbidden key at any depth, and the Go types
// reachable from VoiceOccupancyV1 declare no forbidden field.
func TestVoiceOccupancyCarriesNoTokenRoomOrName(t *testing.T) {
	o := validVoiceOccupancyV1()
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	walkJSONKeys(doc, "$", func(path, key string) {
		if forbiddenOccupancyKeys[key] {
			t.Errorf("occupancy JSON has forbidden key %q at %s", key, path)
		}
	})
	seen := map[reflect.Type]bool{}
	walkStructFields(reflect.TypeOf(VoiceOccupancyV1{}), seen, func(owner reflect.Type, f reflect.StructField) {
		if forbiddenOccupancyFields[f.Name] {
			t.Errorf("%s declares forbidden field %s", owner, f.Name)
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if forbiddenOccupancyKeys[tag] {
			t.Errorf("%s.%s has forbidden json tag %q", owner, f.Name, tag)
		}
	})
	// Guard the walk itself, so the check above cannot pass vacuously.
	for _, want := range []reflect.Type{reflect.TypeOf(VoiceOccupancyV1{}), reflect.TypeOf(VoiceChannelOccupancy{})} {
		if !seen[want] {
			t.Errorf("walk did not reach %s", want)
		}
	}
}
