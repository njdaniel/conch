package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNetGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"net-v1.json", func() any { return new(NetV1) }},
		{"net-v1-empty.json", func() any { return new(NetV1) }},
		{"create-net-request-v1.json", func() any { return new(CreateNetRequestV1) }},
		{"create-net-response-v1.json", func() any { return new(CreateNetResponseV1) }},
		{"list-nets-response-v1.json", func() any { return new(ListNetsResponseV1) }},
		{"list-nets-response-v1-empty.json", func() any { return new(ListNetsResponseV1) }},
		{"put-net-member-request-v1.json", func() any { return new(PutNetMemberRequestV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			assertFixtureValidates(t, tt.file, tt.new)
		})
	}
}

func TestValidateNetName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "simple", input: "alpha"},
		{name: "digits", input: "42"},
		{name: "starts with digit", input: "2nd-squad"},
		{name: "hyphen and underscore", input: "alpha-team_1"},
		{name: "single character", input: "a"},
		{name: "maximum length", input: strings.Repeat("a", MaxNetNameLength)},

		{name: "empty", input: "", wantErr: "net name is required"},
		{name: "too long", input: strings.Repeat("a", MaxNetNameLength+1), wantErr: "longer than 32 characters"},
		{name: "starts with hyphen", input: "-alpha", wantErr: "must start with a letter or digit"},
		{name: "starts with underscore", input: "_alpha", wantErr: "must start with a letter or digit"},
		{name: "uppercase", input: "Alpha", wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "space", input: "alpha team", wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "slash", input: "alpha/1", wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "dot", input: "alpha.1", wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "at sign", input: "@alpha", wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "unicode letter", input: "équipe", wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "leading space", input: " alpha", wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "32 multibyte characters exceed the byte limit", input: strings.Repeat("é", 32), wantErr: "longer than 32 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, ValidateNetName(tt.input), tt.wantErr)
		})
	}
}

func TestNetRoleValid(t *testing.T) {
	for _, r := range []NetRole{NetRoleMember, NetRoleMonitor} {
		if !r.Valid() {
			t.Errorf("net role %q should be valid", r)
		}
	}
	for _, r := range []NetRole{"", "MEMBER", "owner", "listener", "operator", "*"} {
		if r.Valid() {
			t.Errorf("net role %q should be invalid", r)
		}
	}
}

func TestNetMemberValidate(t *testing.T) {
	tests := []struct {
		name    string
		member  NetMember
		wantErr string
	}{
		{name: "valid member", member: NetMember{PrincipalID: 4, Role: NetRoleMember}},
		{name: "valid monitor", member: NetMember{PrincipalID: 9, Role: NetRoleMonitor}},
		{name: "zero principal", member: NetMember{Role: NetRoleMember}, wantErr: "principal_id must be positive, got 0"},
		{name: "negative principal", member: NetMember{PrincipalID: -4, Role: NetRoleMember}, wantErr: "principal_id must be positive, got -4"},
		{name: "empty role", member: NetMember{PrincipalID: 4}, wantErr: `net role "" is not one of member, monitor`},
		{name: "unknown role", member: NetMember{PrincipalID: 4, Role: "owner"}, wantErr: `net role "owner" is not one of member, monitor`},
		{name: "uppercase role", member: NetMember{PrincipalID: 4, Role: "Member"}, wantErr: `net role "Member" is not one of member, monitor`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.member.Validate(), tt.wantErr)
		})
	}
}

func validNetV1() NetV1 {
	return NetV1{
		ID:        3,
		ChannelID: 7,
		Name:      "alpha",
		Members: []NetMember{
			{PrincipalID: 3, Role: NetRoleMember},
			{PrincipalID: 4, Role: NetRoleMember},
			{PrincipalID: 9, Role: NetRoleMonitor},
		},
		CreatedAt: NewTimestamp(time.Date(2026, 10, 8, 11, 0, 0, 0, time.UTC)),
	}
}

// netMemberCases are the member-list rules of NetV1.
var netMemberCases = []struct {
	name    string
	members []NetMember
	wantErr string
}{
	{name: "valid", members: validNetV1().Members},
	{name: "valid nil members", members: nil},
	{name: "valid empty members", members: []NetMember{}},
	{name: "valid monitors only", members: []NetMember{{PrincipalID: 9, Role: NetRoleMonitor}}},
	{name: "member zero principal", members: []NetMember{{Role: NetRoleMember}}, wantErr: "principal_id must be positive, got 0"},
	{name: "member unknown role", members: []NetMember{{PrincipalID: 4, Role: "owner"}}, wantErr: `net role "owner" is not one of member, monitor`},
	{name: "member empty role", members: []NetMember{{PrincipalID: 4}}, wantErr: `net role "" is not one of member, monitor`},
	{
		name:    "duplicate principal, same role",
		members: []NetMember{{PrincipalID: 4, Role: NetRoleMember}, {PrincipalID: 4, Role: NetRoleMember}},
		wantErr: "lists principal 4 more than once",
	},
	{
		name:    "duplicate principal, different roles",
		members: []NetMember{{PrincipalID: 4, Role: NetRoleMember}, {PrincipalID: 4, Role: NetRoleMonitor}},
		wantErr: "lists principal 4 more than once",
	},
}

func TestNetV1Validate(t *testing.T) {
	for _, tt := range netMemberCases {
		t.Run("members: "+tt.name, func(t *testing.T) {
			n := validNetV1()
			n.Members = tt.members
			assertValidation(t, n.Validate(), tt.wantErr)
		})
	}

	tests := []struct {
		name    string
		mutate  func(*NetV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*NetV1) {}},
		{name: "zero id", mutate: func(n *NetV1) { n.ID = 0 }, wantErr: "net id must be positive, got 0"},
		{name: "negative id", mutate: func(n *NetV1) { n.ID = -3 }, wantErr: "net id must be positive, got -3"},
		{name: "zero channel", mutate: func(n *NetV1) { n.ChannelID = 0 }, wantErr: "net channel_id must be positive, got 0"},
		{name: "empty name", mutate: func(n *NetV1) { n.Name = "" }, wantErr: "net name is required"},
		{name: "bad name", mutate: func(n *NetV1) { n.Name = "Alpha Team" }, wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "name starts with hyphen", mutate: func(n *NetV1) { n.Name = "-alpha" }, wantErr: "must start with a letter or digit"},
		{name: "name too long", mutate: func(n *NetV1) { n.Name = strings.Repeat("x", 33) }, wantErr: "longer than 32 characters"},
		{name: "zero created_at", mutate: func(n *NetV1) { n.CreatedAt = Timestamp{} }, wantErr: "net created_at is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := validNetV1()
			tt.mutate(&n)
			assertValidation(t, n.Validate(), tt.wantErr)
		})
	}
}

func TestCreateNetRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     CreateNetRequestV1
		wantErr string
	}{
		{name: "valid", req: CreateNetRequestV1{Name: "alpha"}},
		{name: "empty name", req: CreateNetRequestV1{}, wantErr: "net name is required"},
		{name: "bad name", req: CreateNetRequestV1{Name: "Alpha"}, wantErr: "may contain only a-z, 0-9, - and _"},
		{name: "name starts with underscore", req: CreateNetRequestV1{Name: "_alpha"}, wantErr: "must start with a letter or digit"},
		{name: "name too long", req: CreateNetRequestV1{Name: strings.Repeat("a", 33)}, wantErr: "longer than 32 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

func TestPutNetMemberRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     PutNetMemberRequestV1
		wantErr string
	}{
		{name: "member", req: PutNetMemberRequestV1{Role: NetRoleMember}},
		{name: "monitor", req: PutNetMemberRequestV1{Role: NetRoleMonitor}},
		{name: "empty role", req: PutNetMemberRequestV1{}, wantErr: `net role "" is not one of member, monitor`},
		{name: "unknown role", req: PutNetMemberRequestV1{Role: "owner"}, wantErr: `net role "owner" is not one of member, monitor`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

func TestNetEmptyListsEncodeAsArrays(t *testing.T) {
	got, err := json.Marshal(ListNetsResponseV1{Nets: []NetV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"nets":[]}` {
		t.Fatalf("empty list = %s, want nets:[]", got)
	}

	n := validNetV1()
	n.Members = []NetMember{}
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"members":[]`) {
		t.Errorf("net with no members = %s, want members:[]", raw)
	}
}
