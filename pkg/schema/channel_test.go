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

func TestListChannelsResponseEmptyEncodesArray(t *testing.T) {
	got, err := json.Marshal(ListChannelsResponse{Channels: []ChannelV0{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"channels":[]}` {
		t.Fatalf("got %s", got)
	}
}
