package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestChannelMemberGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"channel-member-v1.json", func() any { return new(ChannelMemberV1) }},
		{"channel-member-v1-named.json", func() any { return new(ChannelMemberV1) }},
		{"channel-member-v1-no-adder.json", func() any { return new(ChannelMemberV1) }},
		{"channel-member-v1-can-decide.json", func() any { return new(ChannelMemberV1) }},
		{"put-channel-member-request-v1.json", func() any { return new(PutChannelMemberRequestV1) }},
		{"list-channel-members-response-v1.json", func() any { return new(ListChannelMembersResponseV1) }},
		{"list-channel-members-response-v1-empty.json", func() any { return new(ListChannelMembersResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestListChannelMembersEmptyIsArray(t *testing.T) {
	raw, err := json.Marshal(ListChannelMembersResponseV1{Members: []ChannelMemberV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"members":[]`) {
		t.Errorf("empty list = %s, want members:[]", raw)
	}
}

func TestChannelMembersGoldenFixtures(t *testing.T) {
	for _, file := range []string{"channel-members-v1.json", "channel-members-v1-empty.json"} {
		t.Run(file, func(t *testing.T) {
			assertP3GoldenFixture(t, file, func() any { return new(ChannelMembersV1) })
		})
	}
}

func TestChannelMembersSchemaName(t *testing.T) {
	if ChannelMembersSchemaV1 != "conch.channel_members.v1" {
		t.Errorf("ChannelMembersSchemaV1 = %q", ChannelMembersSchemaV1)
	}
}

func fixtureChannelMembers() ChannelMembersV1 {
	at := NewTimestamp(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	yes := true
	return ChannelMembersV1{
		Schema:    ChannelMembersSchemaV1,
		ChannelID: 7,
		Members: []ChannelMemberV1{
			{PrincipalID: 2, AddedBy: 1, CreatedAt: at, Name: "ada", Kind: PrincipalHuman, CanDecide: &yes},
			{PrincipalID: 3, AddedBy: 1, CreatedAt: at, Name: "bot", Kind: PrincipalAgent},
		},
	}
}

func TestChannelMembersV1Validate(t *testing.T) {
	no := false
	tests := []struct {
		name    string
		mutate  func(*ChannelMembersV1)
		wantErr string
	}{
		{name: "valid, a human and an agent", mutate: func(*ChannelMembersV1) {}},
		{name: "valid, empty members", mutate: func(d *ChannelMembersV1) { d.Members = []ChannelMemberV1{} }},
		{name: "valid, a human who may not decide", mutate: func(d *ChannelMembersV1) { d.Members[0].CanDecide = &no }},
		// can_decide is shown to operators and to each member about themselves.
		{name: "valid, a human without can_decide", mutate: func(d *ChannelMembersV1) { d.Members[0].CanDecide = nil }},

		{name: "empty schema", mutate: func(d *ChannelMembersV1) { d.Schema = "" }, wantErr: `channel members schema must be "conch.channel_members.v1", got ""`},
		{name: "roster schema", mutate: func(d *ChannelMembersV1) { d.Schema = SessionRosterSchemaV1 }, wantErr: `got "conch.session_roster.v1"`},
		{name: "zero channel_id", mutate: func(d *ChannelMembersV1) { d.ChannelID = 0 }, wantErr: "channel members channel_id must be positive, got 0"},
		// The list is always a list: a channel nobody is in is [].
		{name: "nil members", mutate: func(d *ChannelMembersV1) { d.Members = nil }, wantErr: "channel members members must be a list"},
		{name: "zero principal_id", mutate: func(d *ChannelMembersV1) { d.Members[1].PrincipalID = 0 }, wantErr: "channel member principal_id must be positive, got 0"},
		{name: "duplicate principal", mutate: func(d *ChannelMembersV1) { d.Members[1].PrincipalID = 2 }, wantErr: "channel members lists principal 2 more than once"},
		{name: "no created_at", mutate: func(d *ChannelMembersV1) { d.Members[0].CreatedAt = Timestamp{} }, wantErr: "channel member 2 created_at is required"},
		{name: "blank name", mutate: func(d *ChannelMembersV1) { d.Members[0].Name = " " }, wantErr: "channel member 2 name is required"},
		{name: "no kind", mutate: func(d *ChannelMembersV1) { d.Members[1].Kind = "" }, wantErr: `channel member 3 kind "" is not one of human, agent`},
		{name: "agent with can_decide", mutate: func(d *ChannelMembersV1) { d.Members[1].CanDecide = &no }, wantErr: "channel member 3 is an agent and must not carry can_decide"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fixtureChannelMembers()
			tt.mutate(&d)
			assertValidation(t, d.Validate(), tt.wantErr)
		})
	}
}
