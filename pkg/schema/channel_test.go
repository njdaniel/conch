package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestChannelGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "channel", file: "channel-v0.json", new: func() any { return &ChannelV0{} }},
		{name: "channel manages", file: "channel-v0-manages.json", new: func() any { return &ChannelV0{} }},
		{name: "create request", file: "create-channel-request-v0.json", new: func() any { return &CreateChannelRequest{} }},
		{name: "create response", file: "create-channel-response-v0.json", new: func() any { return &CreateChannelResponse{} }},
		{name: "list response", file: "list-channels-response.json", new: func() any { return &ListChannelsResponse{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// The rename shapes (issue #727): the request and the channel document
// validate as well as round-trip.
func TestRenameChannelGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"rename-channel-request-v1.json", func() any { return new(RenameChannelRequestV1) }},
		{"channel-v1.json", func() any { return new(ChannelDocV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertP3GoldenFixture(t, tt.file, tt.new)
		})
	}
	t.Run("rename-channel-response-v1.json", func(t *testing.T) {
		assertGoldenFixture(t, "rename-channel-response-v1.json", func() any { return new(RenameChannelResponseV1) })
	})
}

func TestChannelSchemaName(t *testing.T) {
	if ChannelSchemaV1 != "conch.channel.v1" {
		t.Errorf("ChannelSchemaV1 = %q", ChannelSchemaV1)
	}
	if ErrorCodeChannelNameStale != "channel_name_stale" {
		t.Errorf("ErrorCodeChannelNameStale = %q", ErrorCodeChannelNameStale)
	}
}

func TestRenameChannelRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     RenameChannelRequestV1
		wantErr string
	}{
		{name: "valid", req: RenameChannelRequestV1{Name: "ops-old", From: "ops"}},
		// A retry of a rename that already happened is the same string twice.
		{name: "valid, the same name", req: RenameChannelRequestV1{Name: "ops", From: "ops"}},
		{name: "valid, a name of another script", req: RenameChannelRequestV1{Name: "\u5c71\u7530 \u592a\u90ce", From: "ops"}},
		// From is compared, never stored: a name from before the rule is fine.
		{name: "valid, a from that breaks the create rule", req: RenameChannelRequestV1{Name: "ops", From: " ops "}},

		{name: "empty name", req: RenameChannelRequestV1{Name: "", From: "ops"}, wantErr: "channel name must not be empty"},
		{name: "blank name", req: RenameChannelRequestV1{Name: "   ", From: "ops"}, wantErr: "channel name must not be empty"},
		{name: "a name with a control character", req: RenameChannelRequestV1{Name: "ops\x1b[31m", From: "ops"}, wantErr: "control characters"},
		{name: "a name with a bidi control", req: RenameChannelRequestV1{Name: "a\u202eb", From: "ops"}, wantErr: "bidi controls"},
		{name: "a name with fringe whitespace", req: RenameChannelRequestV1{Name: "ops ", From: "ops"}, wantErr: "whitespace"},
		{name: "a name too long", req: RenameChannelRequestV1{Name: strings.Repeat("a", MaxChannelNameLength+1), From: "ops"}, wantErr: "at most"},
		{name: "empty from", req: RenameChannelRequestV1{Name: "ops-old"}, wantErr: "rename from must not be empty"},
		{name: "blank from", req: RenameChannelRequestV1{Name: "ops-old", From: " \t"}, wantErr: "rename from must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

func TestChannelDocV1Validate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 7, 13, 12, 34, 56, 0, time.UTC))
	tests := []struct {
		name    string
		mutate  func(*ChannelDocV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*ChannelDocV1) {}},
		// A name stored before the create rule existed still travels.
		{name: "valid, a name from before the rule", mutate: func(d *ChannelDocV1) { d.Name = " ops" }},

		{name: "empty schema", mutate: func(d *ChannelDocV1) { d.Schema = "" }, wantErr: `channel schema must be "conch.channel.v1", got ""`},
		{name: "members schema", mutate: func(d *ChannelDocV1) { d.Schema = ChannelMembersSchemaV1 }, wantErr: `got "conch.channel_members.v1"`},
		{name: "zero channel_id", mutate: func(d *ChannelDocV1) { d.ChannelID = 0 }, wantErr: "channel channel_id must be positive, got 0"},
		{name: "negative channel_id", mutate: func(d *ChannelDocV1) { d.ChannelID = -1 }, wantErr: "channel channel_id must be positive, got -1"},
		{name: "blank name", mutate: func(d *ChannelDocV1) { d.Name = " " }, wantErr: "channel name is required"},
		{name: "no created_at", mutate: func(d *ChannelDocV1) { d.CreatedAt = Timestamp{} }, wantErr: "channel created_at is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := ChannelDocV1{Schema: ChannelSchemaV1, ChannelID: 7, Name: "ops-old", CreatedAt: at}
			tt.mutate(&d)
			assertValidation(t, d.Validate(), tt.wantErr)
		})
	}
}

// The community model (ADR-013, issue #978): the additive fields on
// ChannelV0 and ChannelDocV1, and the new channel shapes.
func TestCommunityChannelGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"channel-v0-public.json", func() any { return new(ChannelV0) }},
		{"channel-v0-private.json", func() any { return new(ChannelV0) }},
		{"create-channel-request-v1.json", func() any { return new(CreateChannelRequestV1) }},
		{"create-channel-response-v1.json", func() any { return new(CreateChannelResponseV1) }},
		{"channel-listing-v1.json", func() any { return new(ChannelListingV1) }},
		{"channel-listing-v1-manages.json", func() any { return new(ChannelListingV1) }},
		{"channel-listing-v1-not-joined.json", func() any { return new(ChannelListingV1) }},
		{"channel-listing-v1-scope-all.json", func() any { return new(ChannelListingV1) }},
		{"channel-directory-response-v1.json", func() any { return new(ChannelDirectoryResponseV1) }},
		{"channel-directory-response-v1-empty.json", func() any { return new(ChannelDirectoryResponseV1) }},
		{"join-channel-response-v1.json", func() any { return new(JoinChannelResponseV1) }},
		{"channel-settings-request-v1.json", func() any { return new(ChannelSettingsRequestV1) }},
		{"channel-settings-request-v1-default.json", func() any { return new(ChannelSettingsRequestV1) }},
		{"channel-settings-response-v1.json", func() any { return new(ChannelSettingsResponseV1) }},
		{"channel-settings-response-v1-open-signup-off.json", func() any { return new(ChannelSettingsResponseV1) }},
		{"channel-settings-response-v1-warning.json", func() any { return new(ChannelSettingsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
	// The channel document with the new fields validates as well.
	t.Run("channel-v1-public.json", func(t *testing.T) {
		assertP3GoldenFixture(t, "channel-v1-public.json", func() any { return new(ChannelDocV1) })
	})
	// And the request fixtures are requests the schema accepts.
	t.Run("create-channel-request-v1.json validates", func(t *testing.T) {
		var r CreateChannelRequestV1
		decodeFixture(t, "create-channel-request-v1.json", &r)
		assertValidation(t, r.Validate(), "")
	})
	for _, file := range []string{"channel-settings-request-v1.json", "channel-settings-request-v1-default.json"} {
		t.Run(file+" validates", func(t *testing.T) {
			var r ChannelSettingsRequestV1
			decodeFixture(t, file, &r)
			assertValidation(t, r.Validate(), "")
		})
	}
}

// A channel from before the community model carries neither field, so the
// published fixtures are unchanged; one with them says both.
func TestChannelV0VisibilityOmittedWhenUnset(t *testing.T) {
	at := time.Date(2026, 7, 13, 12, 34, 56, 0, time.UTC)
	raw, err := json.Marshal(ChannelV0{ID: 7, Name: "general", CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":7,"name":"general","created_at":"2026-07-13T12:34:56Z"}`; string(raw) != want {
		t.Errorf("ChannelV0 = %s, want %s", raw, want)
	}
	raw, err = json.Marshal(ChannelV0{ID: 7, Name: "general", CreatedAt: at, Visibility: ChannelPublic, Default: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":7,"name":"general","created_at":"2026-07-13T12:34:56Z","visibility":"public","default":true}`; string(raw) != want {
		t.Errorf("ChannelV0 = %s, want %s", raw, want)
	}
}

func TestChannelVisibility(t *testing.T) {
	if got := strings.Join(Vocabularies["ChannelVisibility"], ","); got != "public,private" {
		t.Errorf("ChannelVisibility = %q", got)
	}
	for v, want := range map[ChannelVisibility]bool{ChannelPublic: true, ChannelPrivate: true, "hidden": false, "": false} {
		if got := v.Valid(); got != want {
			t.Errorf("ChannelVisibility(%q).Valid() = %v, want %v", v, got, want)
		}
	}
	want := map[string]string{
		ErrorCodeChannelNotJoinable:      "channel_not_joinable",
		ErrorCodeChannelSettingsConflict: "channel_settings_conflict",
		ErrorCodeChannelLastMember:       "channel_last_member",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("code = %q, want %q", got, w)
		}
	}
}

// The create request applies the name rule of CreateChannelRequest.Validate
// and requires a known visibility.
func TestCreateChannelRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     CreateChannelRequestV1
		wantErr string
	}{
		{"public", CreateChannelRequestV1{Name: "music", Visibility: ChannelPublic}, ""},
		{"private", CreateChannelRequestV1{Name: "belgium", Visibility: ChannelPrivate}, ""},
		{"a name of another script", CreateChannelRequestV1{Name: "山田 太郎", Visibility: ChannelPublic}, ""},

		{"empty name", CreateChannelRequestV1{Visibility: ChannelPublic}, "channel name must not be empty"},
		{"blank name", CreateChannelRequestV1{Name: "  ", Visibility: ChannelPublic}, "channel name must not be empty"},
		{"a control character", CreateChannelRequestV1{Name: "ops\x1b[31m", Visibility: ChannelPublic}, "control characters"},
		{"a bidi control", CreateChannelRequestV1{Name: "a\u202eb", Visibility: ChannelPublic}, "bidi controls"},
		{"fringe whitespace", CreateChannelRequestV1{Name: "ops ", Visibility: ChannelPublic}, "whitespace"},
		{"too long", CreateChannelRequestV1{Name: strings.Repeat("a", MaxChannelNameLength+1), Visibility: ChannelPublic}, "at most"},
		{"no visibility", CreateChannelRequestV1{Name: "music"}, "channel visibility must be one of: public, private"},
		{"unknown visibility", CreateChannelRequestV1{Name: "music", Visibility: "secret"}, "channel visibility must be one of: public, private"},
		{"the name checked first", CreateChannelRequestV1{Name: "", Visibility: "secret"}, "channel name must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

// The settings request needs at least one field and a known visibility.
func TestChannelSettingsRequestV1Validate(t *testing.T) {
	vis := func(v ChannelVisibility) *ChannelVisibility { return &v }
	b := func(v bool) *bool { return &v }
	tests := []struct {
		name    string
		req     ChannelSettingsRequestV1
		wantErr string
	}{
		{"visibility", ChannelSettingsRequestV1{Visibility: vis(ChannelPrivate)}, ""},
		{"default", ChannelSettingsRequestV1{Default: b(true)}, ""},
		{"default off", ChannelSettingsRequestV1{Default: b(false)}, ""},
		{"both", ChannelSettingsRequestV1{Visibility: vis(ChannelPublic), Default: b(true)}, ""},
		// A private default channel is the server's 409, not this rule's.
		{"private and default", ChannelSettingsRequestV1{Visibility: vis(ChannelPrivate), Default: b(true)}, ""},

		{"nothing", ChannelSettingsRequestV1{}, "the request changes no setting"},
		{"empty visibility", ChannelSettingsRequestV1{Visibility: vis("")}, "channel visibility must be one of: public, private"},
		{"unknown visibility", ChannelSettingsRequestV1{Visibility: vis("secret"), Default: b(true)}, "channel visibility must be one of: public, private"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

// The channel requests decode strictly.
func TestCommunityChannelRequestsRefuseUnknownFields(t *testing.T) {
	tests := []struct {
		raw  string
		into func() any
	}{
		{`{"name":"music","visibility":"public","default":true}`, func() any { return new(CreateChannelRequestV1) }},
		{`{"name":"music","visibility":"public","manages":true}`, func() any { return new(CreateChannelRequestV1) }},
		{`{"visibility":"public","name":"music"}`, func() any { return new(ChannelSettingsRequestV1) }},
		{`{"default":true,"open_signup":true}`, func() any { return new(ChannelSettingsRequestV1) }},
	}
	for _, tt := range tests {
		dec := json.NewDecoder(strings.NewReader(tt.raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(tt.into()); err == nil {
			t.Errorf("%s decoded", tt.raw)
		}
	}
}

// The channel document accepts the new fields and refuses an unknown
// visibility.
func TestChannelDocV1Visibility(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 7, 13, 12, 34, 56, 0, time.UTC))
	d := ChannelDocV1{Schema: ChannelSchemaV1, ChannelID: 7, Name: "general", CreatedAt: at}
	assertValidation(t, d.Validate(), "")
	d.Visibility, d.Default = ChannelPublic, true
	assertValidation(t, d.Validate(), "")
	d.Visibility = "secret"
	assertValidation(t, d.Validate(), "channel visibility must be one of: public, private")
}

func TestChannelDirectoryResponseV1EmptyEncodesArray(t *testing.T) {
	got, err := json.Marshal(ChannelDirectoryResponseV1{Channels: []ChannelListingV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"channels":[]}` {
		t.Fatalf("got %s", got)
	}
}

func TestListChannelsResponseEmptyEncodesArray(t *testing.T) {
	got, err := json.Marshal(ListChannelsResponse{Channels: []ChannelV0{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"channels":[]}` {
		t.Fatalf("got %s", got)
	}
}
