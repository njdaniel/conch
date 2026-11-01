package schema

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDirectoryGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"server-member-v1.json", func() any { return new(ServerMemberV1) }},
		{"server-member-v1-manage-members.json", func() any { return new(ServerMemberV1) }},
		{"server-member-v1-owner.json", func() any { return new(ServerMemberV1) }},
		{"server-member-v1-agent.json", func() any { return new(ServerMemberV1) }},
		{"server-members-response-v1.json", func() any { return new(ServerMembersResponseV1) }},
		{"server-members-response-v1-shared-channels.json", func() any { return new(ServerMembersResponseV1) }},
		{"server-members-response-v1-empty.json", func() any { return new(ServerMembersResponseV1) }},
		{"directory-changed-v1.json", func() any { return new(DirectoryChangedV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// The fixtures are pages and frames the schema accepts.
func TestDirectoryFixturesValidate(t *testing.T) {
	for _, file := range []string{"server-members-response-v1.json", "server-members-response-v1-shared-channels.json", "server-members-response-v1-empty.json"} {
		t.Run(file, func(t *testing.T) {
			var d ServerMembersResponseV1
			decodeFixture(t, file, &d)
			if err := d.Validate(); err != nil {
				t.Errorf("Validate = %v, want nil", err)
			}
		})
	}
	t.Run("directory-changed-v1.json", func(t *testing.T) {
		var d DirectoryChangedV1
		decodeFixture(t, "directory-changed-v1.json", &d)
		if err := d.Validate(); err != nil {
			t.Errorf("Validate = %v, want nil", err)
		}
	})
}

// A member sees id, kind, name, role and created_at and nothing more: the
// manage_members fields are absent from the wire when unset, and an agent
// carries no role.
func TestServerMemberV1OmitsWhatAMemberMayNotSee(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	raw, err := json.Marshal(ServerMemberV1{ID: 3, Kind: PrincipalHuman, Name: "wren", Role: RoleMember, CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":3,"kind":"human","name":"wren","role":"member","created_at":"2026-10-30T12:00:00.000Z"}`; string(raw) != want {
		t.Errorf("member view = %s, want %s", raw, want)
	}
	raw, err = json.Marshal(ServerMemberV1{ID: 5, Kind: PrincipalAgent, Name: "leviathan", CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":5,"kind":"agent","name":"leviathan","created_at":"2026-10-30T12:00:00.000Z"}`; string(raw) != want {
		t.Errorf("agent row = %s, want %s", raw, want)
	}
}

func TestServerMembersResponseV1Validate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	yes := true
	two := int64(2)
	human := func(id int64, name string) ServerMemberV1 {
		return ServerMemberV1{ID: id, Kind: PrincipalHuman, Name: name, Role: RoleMember, CreatedAt: at}
	}
	agent := ServerMemberV1{ID: 5, Kind: PrincipalAgent, Name: "leviathan", CreatedAt: at}
	tests := []struct {
		name    string
		page    ServerMembersResponseV1
		wantErr string
	}{
		{"empty", ServerMembersResponseV1{Members: []ServerMemberV1{}}, ""},
		{"members and an agent", ServerMembersResponseV1{Members: []ServerMemberV1{human(1, "nick"), human(3, "wren"), agent}}, ""},
		{"a human with no role", ServerMembersResponseV1{Members: []ServerMemberV1{{ID: 3, Kind: PrincipalHuman, Name: "wren", CreatedAt: at}}}, ""},
		{"an operator view", ServerMembersResponseV1{Members: []ServerMemberV1{{ID: 3, Kind: PrincipalHuman, Name: "wren", Role: RoleMember, CreatedAt: at, Owner: false, VoiceAllowed: &yes, MayInvite: &yes, InvitedBy: &two}}}, ""},
		{"a cursor on a listed row", ServerMembersResponseV1{Members: []ServerMemberV1{human(1, "nick"), human(3, "wren")}, After: 3}, ""},
		{"scope everyone", ServerMembersResponseV1{Members: []ServerMemberV1{human(1, "nick")}, Scope: DirectoryScopeEveryone}, ""},
		{"scope shared_channels", ServerMembersResponseV1{Members: []ServerMemberV1{human(1, "nick")}, Scope: DirectoryScopeSharedChannels}, ""},

		{"zero id", ServerMembersResponseV1{Members: []ServerMemberV1{human(0, "x")}}, "members[0] id must be positive, got 0"},
		{"negative id", ServerMembersResponseV1{Members: []ServerMemberV1{human(-1, "x")}}, "members[0] id must be positive, got -1"},
		{"an id twice", ServerMembersResponseV1{Members: []ServerMemberV1{human(3, "wren"), human(3, "wren")}}, "members[1] id 3 is listed twice"},
		{"no name", ServerMembersResponseV1{Members: []ServerMemberV1{human(3, "")}}, "members[0] name is required"},
		{"a blank name", ServerMembersResponseV1{Members: []ServerMemberV1{human(3, " ")}}, "members[0] name is required"},
		{"an unknown kind", ServerMembersResponseV1{Members: []ServerMemberV1{{ID: 3, Kind: "robot", Name: "x", CreatedAt: at}}}, "members[0] kind must be one of: human, agent"},
		{"no kind", ServerMembersResponseV1{Members: []ServerMemberV1{{ID: 3, Name: "x", CreatedAt: at}}}, "members[0] kind must be one of: human, agent"},
		{"an unknown role", ServerMembersResponseV1{Members: []ServerMemberV1{{ID: 3, Kind: PrincipalHuman, Name: "x", Role: "admin", CreatedAt: at}}}, "members[0] role must be one of: operator, member"},
		{"an agent with a role", ServerMembersResponseV1{Members: []ServerMemberV1{{ID: 5, Kind: PrincipalAgent, Name: "bot", Role: RoleMember, CreatedAt: at}}}, "members[0] is an agent and carries a role"},
		{"an agent with voice", ServerMembersResponseV1{Members: []ServerMemberV1{{ID: 5, Kind: PrincipalAgent, Name: "bot", CreatedAt: at, VoiceAllowed: &yes}}}, "members[0] is an agent and carries a person's fields"},
		{"an agent as owner", ServerMembersResponseV1{Members: []ServerMemberV1{{ID: 5, Kind: PrincipalAgent, Name: "bot", CreatedAt: at, Owner: true}}}, "members[0] is an agent and carries a person's fields"},
		{"an unknown scope", ServerMembersResponseV1{Members: []ServerMemberV1{}, Scope: "server"}, "scope must be one of: everyone, shared_channels"},
		{"a cursor naming an unlisted id", ServerMembersResponseV1{Members: []ServerMemberV1{human(1, "nick")}, After: 9}, "after must be the id of a listed member"},
		{"a cursor on an empty page", ServerMembersResponseV1{Members: []ServerMemberV1{}, After: 9}, "after must be the id of a listed member"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.page.Validate(), tt.wantErr)
		})
	}
}

func TestDirectoryScope(t *testing.T) {
	if got := Vocabularies["DirectoryScope"]; !reflect.DeepEqual(got, []string{"everyone", "shared_channels"}) {
		t.Errorf("DirectoryScope = %q", got)
	}
	for v, want := range map[DirectoryScope]bool{DirectoryScopeEveryone: true, DirectoryScopeSharedChannels: true, "server": false, "": false} {
		if got := v.Valid(); got != want {
			t.Errorf("DirectoryScope(%q).Valid() = %v, want %v", v, got, want)
		}
	}
}

func TestDirectoryChangedV1(t *testing.T) {
	if DirectoryChangedSchemaV1 != "conch.directory_changed.v1" {
		t.Errorf("DirectoryChangedSchemaV1 = %q", DirectoryChangedSchemaV1)
	}
	tests := []struct {
		name    string
		doc     DirectoryChangedV1
		wantErr string
	}{
		{"valid", DirectoryChangedV1{Schema: DirectoryChangedSchemaV1}, ""},
		{"empty schema", DirectoryChangedV1{}, `directory changed schema must be "conch.directory_changed.v1", got ""`},
		{"the channels signal", DirectoryChangedV1{Schema: ChannelsChangedSchemaV1}, `got "conch.channels_changed.v1"`},
		{"the never-published whole list", DirectoryChangedV1{Schema: "conch.directory_members.v1"}, `got "conch.directory_members.v1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.doc.Validate(), tt.wantErr)
		})
	}
	// The frame carries nothing but its schema: a reader with unknown
	// fields refused cannot be given content.
	raw, err := json.Marshal(DirectoryChangedV1{Schema: DirectoryChangedSchemaV1})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"schema":"conch.directory_changed.v1"}` {
		t.Errorf("frame = %s", raw)
	}
}

func TestServerMembersResponseV1EmptyEncodesArray(t *testing.T) {
	got, err := json.Marshal(ServerMembersResponseV1{Members: []ServerMemberV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"members":[]}` {
		t.Fatalf("got %s", got)
	}
}
