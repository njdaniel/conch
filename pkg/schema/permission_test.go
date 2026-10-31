package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPermissionGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"permissions-response-v1.json", func() any { return new(PermissionsResponseV1) }},
		{"permissions-response-v1-personal.json", func() any { return new(PermissionsResponseV1) }},
		{"put-permissions-request-v1.json", func() any { return new(PutPermissionsRequestV1) }},
		{"put-permissions-request-v1-preset.json", func() any { return new(PutPermissionsRequestV1) }},
		{"permissions-changed-v1.json", func() any { return new(PermissionsChangedV1) }},
		{"whoami-response-v1-permissions.json", func() any { return new(WhoAmIResponseV1) }},
		{"whoami-response-v1-permissions-none.json", func() any { return new(WhoAmIResponseV1) }},
		{"error-permission-denied.json", func() any { return new(Error) }},
		{"error-invite-not-allowed-permission.json", func() any { return new(Error) }},
		{"invite-v1-other-channels.json", func() any { return new(InviteV1) }},
		{"list-hooks-response-v1-other-hooks.json", func() any { return new(ListHooksResponseV1) }},
		{"join-settings-v1-other-open-channels.json", func() any { return new(JoinSettingsV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// The fixtures are valid documents, not merely well-formed JSON.
func TestPermissionFixturesValidate(t *testing.T) {
	for _, file := range []string{"put-permissions-request-v1.json", "put-permissions-request-v1-preset.json"} {
		t.Run(file, func(t *testing.T) {
			var r PutPermissionsRequestV1
			readFixture(t, file, &r)
			if err := r.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
	t.Run("permissions-changed-v1.json", func(t *testing.T) {
		var d PermissionsChangedV1
		readFixture(t, "permissions-changed-v1.json", &d)
		if err := d.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
	// Each response fixture's rows are the catalog, in order, and agree with
	// the fixed cells and the preset table.
	for _, file := range []string{"permissions-response-v1.json", "permissions-response-v1-personal.json"} {
		t.Run(file, func(t *testing.T) {
			var r PermissionsResponseV1
			readFixture(t, file, &r)
			if len(r.Rows) != len(Permissions) {
				t.Fatalf("%d rows, want %d", len(r.Rows), len(Permissions))
			}
			var member []Permission
			for i, row := range r.Rows {
				if row.Permission != Permissions[i] {
					t.Errorf("rows[%d] = %s, want %s (catalog order)", i, row.Permission, Permissions[i])
				}
				for _, c := range []struct {
					role       PermissionRole
					value, fix bool
				}{
					{PermissionRoleMember, row.Member, row.MemberFixed},
					{PermissionRoleOperator, row.Operator, row.OperatorFixed},
				} {
					fixed, value := row.Permission.Fixed(c.role)
					if c.fix != fixed || (fixed && c.value != value) {
						t.Errorf("rows[%d] %s cell = %v fixed %v, want fixed %v value %v", i, c.role, c.value, c.fix, fixed, value)
					}
				}
				if row.Member {
					member = append(member, row.Permission)
				}
			}
			if !slices.Equal(member, PresetMemberPermissions[r.Preset]) {
				t.Errorf("member column %v, preset %q is %v", member, r.Preset, PresetMemberPermissions[r.Preset])
			}
			for _, p := range r.Presets {
				if !slices.Equal(p.Member, PresetMemberPermissions[p.Preset]) {
					t.Errorf("presets[%s] = %v, want %v", p.Preset, p.Member, PresetMemberPermissions[p.Preset])
				}
			}
		})
	}
}

func readFixture(t *testing.T, file string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name comes from this file's own tests
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionsCatalog(t *testing.T) {
	want := []string{
		"create_public_channels", "create_private_channels", "make_member_links", "see_member_directory",
		"use_voice", "share_screen", "manage_members", "act_as_members", "manage_channels",
		"change_server_settings", "read_audit_log",
	}
	if got := values(Permissions...); !reflect.DeepEqual(got, want) {
		t.Errorf("Permissions = %q,\nwant %q", got, want)
	}
	if got := Vocabularies["Permission"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Vocabularies[Permission] = %q,\nwant %q", got, want)
	}
	for name, want := range map[string][]string{
		"PermissionRole":     {"member", "operator"},
		"PermissionPreset":   {"personal", "group"},
		"PermissionsWarning": {"default_channel_exposes_members", "open_signup_on"},
		"IssuedKind":         {"credential", "hook"},
	} {
		if got := Vocabularies[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("Vocabularies[%s] = %q, want %q", name, got, want)
		}
	}
}

// permissionCell is one row of the §4.1 table, written out by hand so that a
// change to the methods has to change this too.
type permissionCell struct {
	member, operator           bool // the defaults
	memberFixed, operatorFixed bool
	operatorOnly               bool
}

var permissionTable = map[Permission]permissionCell{
	PermCreatePublicChannels:  {member: true, operator: true},
	PermCreatePrivateChannels: {member: true, operator: true},
	PermMakeMemberLinks:       {member: true, operator: true},
	PermSeeMemberDirectory:    {member: true, operator: true, operatorFixed: true},
	PermUseVoice:              {member: true, operator: true},
	PermShareScreen:           {member: true, operator: true},
	PermManageMembers:         {operator: true, memberFixed: true, operatorOnly: true},
	PermActAsMembers:          {operator: true, memberFixed: true, operatorOnly: true},
	PermManageChannels:        {operator: true, memberFixed: true, operatorOnly: true},
	PermChangeServerSettings:  {memberFixed: true, operatorOnly: true},
	PermReadAuditLog:          {memberFixed: true, operatorOnly: true},
}

func TestPermissionCells(t *testing.T) {
	if len(permissionTable) != len(Permissions) {
		t.Fatalf("the table has %d rows, the catalog %d", len(permissionTable), len(Permissions))
	}
	for _, p := range Permissions {
		row, ok := permissionTable[p]
		if !ok {
			t.Fatalf("%s has no row in the table", p)
		}
		t.Run(string(p), func(t *testing.T) {
			if !p.Valid() {
				t.Errorf("Valid() = false")
			}
			if got := p.OperatorOnly(); got != row.operatorOnly {
				t.Errorf("OperatorOnly() = %v, want %v", got, row.operatorOnly)
			}
			for _, c := range []struct {
				role         PermissionRole
				def, isFixed bool
			}{
				{PermissionRoleMember, row.member, row.memberFixed},
				{PermissionRoleOperator, row.operator, row.operatorFixed},
			} {
				t.Run(string(c.role), func(t *testing.T) {
					if got := p.Default(c.role); got != c.def {
						t.Errorf("Default(%s) = %v, want %v", c.role, got, c.def)
					}
					fixed, value := p.Fixed(c.role)
					if fixed != c.isFixed {
						t.Errorf("Fixed(%s) fixed = %v, want %v", c.role, fixed, c.isFixed)
					}
					// A fixed cell's value is its default; a configurable one
					// reports false.
					if want := c.isFixed && c.def; value != want {
						t.Errorf("Fixed(%s) value = %v, want %v", c.role, value, want)
					}
				})
			}
		})
	}
}

// Unknown values fail closed: nothing grants a permission or a column the
// catalog does not have.
func TestPermissionUnknownFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		p Permission
		r PermissionRole
	}{
		{"", PermissionRoleMember},
		{"invent_roles", PermissionRoleOperator},
		{"Use_Voice", PermissionRoleMember},
		{PermUseVoice, ""},
		{PermUseVoice, "owner"},
		{PermUseVoice, "guest"},
	} {
		t.Run(fmt.Sprintf("%s/%s", tc.p, tc.r), func(t *testing.T) {
			if tc.p.Valid() && tc.r.Valid() {
				t.Fatal("test case is known")
			}
			if tc.p.Default(tc.r) {
				t.Errorf("Default = true, want false")
			}
			if fixed, value := tc.p.Fixed(tc.r); !fixed || value {
				t.Errorf("Fixed = (%v, %v), want (true, false)", fixed, value)
			}
			if !tc.p.Valid() && !tc.p.OperatorOnly() {
				t.Errorf("OperatorOnly = false for an unknown permission, want true")
			}
		})
	}
}

func TestPermissionRequires(t *testing.T) {
	want := map[Permission][]Permission{
		PermShareScreen:  {PermUseVoice},
		PermActAsMembers: {PermManageMembers},
	}
	for _, p := range Permissions {
		t.Run(string(p), func(t *testing.T) {
			if got := p.Requires(); !slices.Equal(got, want[p]) {
				t.Errorf("Requires() = %v, want %v", got, want[p])
			}
		})
	}
	t.Run("the slice is the caller's", func(t *testing.T) {
		PermShareScreen.Requires()[0] = PermReadAuditLog
		if got := PermShareScreen.Requires(); !slices.Equal(got, []Permission{PermUseVoice}) {
			t.Errorf("after a caller's write, Requires() = %v", got)
		}
	})
	// The defaults, and every preset, satisfy the dependencies and the fixed
	// cells, and members never exceed operators: a fresh server, and one
	// with a preset applied, is a document the store would accept.
	columns := map[string]func(Permission) bool{
		"member defaults":   func(p Permission) bool { return p.Default(PermissionRoleMember) },
		"operator defaults": func(p Permission) bool { return p.Default(PermissionRoleOperator) },
	}
	for preset, on := range PresetMemberPermissions {
		columns["preset "+string(preset)] = func(p Permission) bool { return slices.Contains(on, p) }
	}
	for name, holds := range columns {
		t.Run(name, func(t *testing.T) {
			role := PermissionRoleMember
			if name == "operator defaults" {
				role = PermissionRoleOperator
			}
			for _, p := range Permissions {
				if !holds(p) {
					continue
				}
				for _, req := range p.Requires() {
					if !holds(req) {
						t.Errorf("%s holds %s but not %s, which it requires", name, p, req)
					}
				}
				if role == PermissionRoleMember && !p.Default(PermissionRoleOperator) {
					t.Errorf("%s grants members %s, which operators lack by default", name, p)
				}
			}
			for _, p := range Permissions {
				if fixed, value := p.Fixed(role); fixed && holds(p) != value {
					t.Errorf("%s has %s = %v, but the cell is fixed %v", name, p, holds(p), value)
				}
			}
		})
	}
}

func TestPresetMemberPermissions(t *testing.T) {
	if got, want := len(PresetMemberPermissions), len(Vocabularies["PermissionPreset"]); got != want {
		t.Fatalf("%d presets in the table, %d in the vocabulary", got, want)
	}
	var defaults []Permission
	for _, p := range Permissions {
		if p.Default(PermissionRoleMember) {
			defaults = append(defaults, p)
		}
	}
	if got := PresetMemberPermissions[PresetGroup]; !slices.Equal(got, defaults) {
		t.Errorf("group = %v, want the member defaults %v", got, defaults)
	}
	if got, want := PresetMemberPermissions[PresetPersonal], []Permission{PermUseVoice, PermShareScreen}; !slices.Equal(got, want) {
		t.Errorf("personal = %v, want %v", got, want)
	}
	for preset, on := range PresetMemberPermissions {
		if !preset.Valid() {
			t.Errorf("preset %q is not in the vocabulary", preset)
		}
		// Catalog order, so the GET answer needs no sort.
		last := -1
		for _, p := range on {
			i := slices.Index(Permissions, p)
			if i <= last {
				t.Errorf("preset %s: %v is not in catalog order", preset, on)
			}
			last = i
		}
	}
}

func TestPutPermissionsRequestV1Validate(t *testing.T) {
	change := func(p Permission, r PermissionRole, granted bool) PermissionChangeV1 {
		return PermissionChangeV1{Permission: p, Role: r, Granted: granted}
	}
	patch := func(cs ...PermissionChangeV1) PutPermissionsRequestV1 {
		return PutPermissionsRequestV1{Revision: 3, Changes: cs}
	}
	valid := change(PermMakeMemberLinks, PermissionRoleMember, false)
	many := func(n int) []PermissionChangeV1 {
		out := make([]PermissionChangeV1, n)
		for i := range out {
			out[i] = valid
		}
		return out
	}
	type tc struct {
		name    string
		req     PutPermissionsRequestV1
		wantErr string
	}
	tests := []tc{
		{name: "valid patch", req: patch(
			change(PermActAsMembers, PermissionRoleOperator, false),
			change(PermMakeMemberLinks, PermissionRoleMember, false),
			change(PermMakeMemberLinks, PermissionRoleOperator, true),
		)},
		{name: "valid preset", req: PutPermissionsRequestV1{Revision: 1, Preset: PresetPersonal}},
		{name: "valid preset group", req: PutPermissionsRequestV1{Revision: 9, Preset: PresetGroup}},
		{name: "a member revoke of a configurable cell", req: patch(change(PermSeeMemberDirectory, PermissionRoleMember, false))},
		{name: "an operator grant of an operator-only permission", req: patch(change(PermReadAuditLog, PermissionRoleOperator, true))},
		{name: "every configurable cell", req: PutPermissionsRequestV1{Revision: 1, Changes: everyConfigurableCell()}},

		{name: "zero revision", req: PutPermissionsRequestV1{Preset: PresetGroup}, wantErr: "revision must be positive"},
		{name: "negative revision", req: PutPermissionsRequestV1{Revision: -1, Changes: []PermissionChangeV1{valid}}, wantErr: "revision must be positive"},
		{name: "both changes and preset", req: PutPermissionsRequestV1{Revision: 1, Changes: []PermissionChangeV1{valid}, Preset: PresetGroup}, wantErr: "exactly one of changes and preset"},
		{name: "empty changes and preset", req: PutPermissionsRequestV1{Revision: 1, Changes: []PermissionChangeV1{}, Preset: PresetGroup}, wantErr: "exactly one of changes and preset"},
		{name: "neither", req: PutPermissionsRequestV1{Revision: 1}, wantErr: "exactly one of changes and preset"},
		{name: "0 changes", req: PutPermissionsRequestV1{Revision: 1, Changes: []PermissionChangeV1{}}, wantErr: "changes must have 1 to 32 entries"},
		{name: "33 changes", req: PutPermissionsRequestV1{Revision: 1, Changes: many(33)}, wantErr: "changes must have 1 to 32 entries"},
		{name: "a duplicate pair", req: patch(valid, change(PermUseVoice, PermissionRoleMember, true), change(PermMakeMemberLinks, PermissionRoleMember, true)),
			wantErr: "changes[2] sets make_member_links for member a second time"},
		{name: "unknown permission", req: patch(change("invent_roles", PermissionRoleOperator, true)), wantErr: "changes[0].permission must be one of: create_public_channels,"},
		{name: "empty permission", req: patch(change("", PermissionRoleOperator, true)), wantErr: "changes[0].permission must be one of"},
		{name: "unknown role", req: patch(valid, change(PermUseVoice, "owner", true)), wantErr: "changes[1].role must be one of: member, operator"},
		{name: "unknown role guest", req: patch(change(PermUseVoice, "guest", true)), wantErr: "changes[0].role must be one of: member, operator"},
		{name: "empty role", req: patch(change(PermUseVoice, "", true)), wantErr: "changes[0].role must be one of"},
		{name: "unknown preset", req: PutPermissionsRequestV1{Revision: 1, Preset: "lockdown"}, wantErr: "preset must be one of: personal, group"},
		// Written out, so these do not depend on Fixed being right; the
		// loops below cover every fixed cell by Fixed.
		{name: "operator directory off", req: patch(change(PermSeeMemberDirectory, PermissionRoleOperator, false)),
			wantErr: "changes[0]: the operator cell of see_member_directory is fixed"},
		{name: "operator directory on", req: patch(change(PermSeeMemberDirectory, PermissionRoleOperator, true)),
			wantErr: "changes[0]: the operator cell of see_member_directory is fixed"},
		{name: "member audit log off", req: patch(change(PermReadAuditLog, PermissionRoleMember, false)),
			wantErr: "changes[0]: the member cell of read_audit_log is fixed"},
	}
	for _, p := range Permissions {
		if !p.OperatorOnly() {
			continue
		}
		tests = append(tests, tc{
			name:    "member grant of " + string(p),
			req:     patch(change(p, PermissionRoleMember, true)),
			wantErr: "changes[0]: " + string(p) + " is operator-only and cannot be granted to members",
		})
	}
	for _, p := range Permissions {
		for _, r := range []PermissionRole{PermissionRoleMember, PermissionRoleOperator} {
			fixed, value := p.Fixed(r)
			if !fixed {
				continue
			}
			// Writing a fixed cell is refused whatever the value, its own
			// fixed value included. A member grant of an operator-only
			// permission is refused above, with its own message.
			for _, granted := range []bool{value, !value} {
				if r == PermissionRoleMember && granted {
					continue
				}
				tests = append(tests, tc{
					name:    fmt.Sprintf("write fixed %s cell of %s to %v", r, p, granted),
					req:     patch(change(p, r, granted)),
					wantErr: fmt.Sprintf("changes[0]: the %s cell of %s is fixed and cannot be changed", r, p),
				})
			}
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

// everyConfigurableCell is one change per configurable cell of the catalog:
// 16 today (6 member cells, 10 operator cells). With no pair twice, that is
// the longest valid patch the catalog allows now; MaxPermissionChanges
// leaves room for it to grow.
func everyConfigurableCell() []PermissionChangeV1 {
	var cells []PermissionChangeV1
	for _, p := range Permissions {
		for _, r := range []PermissionRole{PermissionRoleMember, PermissionRoleOperator} {
			if fixed, _ := p.Fixed(r); !fixed {
				cells = append(cells, PermissionChangeV1{Permission: p, Role: r, Granted: true})
			}
		}
	}
	return cells
}

func TestPermissionsResponseV1NeverNull(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	raw, err := json.Marshal(PermissionsResponseV1{Revision: 1, UpdatedAt: at, Presets: []PermissionPresetV1{{Preset: PresetPersonal}}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"rows":[]`, `"editable":[]`, `"warnings":[]`, `"member":[]`} {
		if !strings.Contains(s, want) {
			t.Errorf("Marshal = %s, want %s", s, want)
		}
	}
	if strings.Contains(s, "null") {
		t.Errorf("Marshal = %s, want no null", s)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"preset", "issued_to_others"} {
		if _, ok := top[absent]; ok {
			t.Errorf("Marshal = %s, want %s omitted", s, absent)
		}
	}
	raw, err = json.Marshal(PermissionsResponseV1{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"presets":[]`) {
		t.Errorf("Marshal(zero) = %s, want presets:[]", raw)
	}
}

// The optional fields this issue adds to published shapes are omitted when
// unset, so every answer that does not use them is byte-for-byte unchanged.
func TestPermissionOptionalFieldsOmitted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  any
		absent []string
	}{
		{"whoami", WhoAmIResponseV1{ID: 1, Kind: PrincipalHuman, Name: "member-1", Role: RoleMember}, []string{"permissions", "permissions_revision"}},
		{"error", Error{Code: "invalid_request", Message: "no"}, []string{"permission"}},
		{"invite", InviteV1{Channels: []InviteChannelV1{}}, []string{"other_channels"}},
		{"hooks", ListHooksResponseV1{Hooks: []HookV1{}}, []string{"other_hooks"}},
		{"join settings", DefaultJoinSettings(), []string{"other_open_channels"}},
		{"permission row", PermissionRowV1{Permission: PermUseVoice}, []string{"member_fixed", "operator_fixed"}},
		{"put request", PutPermissionsRequestV1{Revision: 1, Preset: PresetGroup}, []string{"changes"}},
		{"put request", PutPermissionsRequestV1{Revision: 1, Changes: []PermissionChangeV1{{}}}, []string{"preset"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range tc.absent {
				if strings.Contains(string(raw), `"`+key+`"`) {
					t.Errorf("Marshal = %s, want %q omitted", raw, key)
				}
			}
		})
	}
}

// Forward compatibility: a reader accepts a document from a newer server
// that added a field, or a permission, it does not know; and a document from
// a server that predates ADR-014 decodes with the new fields at their zero.
func TestPermissionUnknownFieldsTolerated(t *testing.T) {
	t.Run("permissions response with a later field and permission", func(t *testing.T) {
		raw := `{"revision":5,"rows":[{"permission":"use_voice","member":true,"operator":true,"guest":false},` +
			`{"permission":"pin_messages","member":false,"operator":true}],"presets":[],"editable":["member"],` +
			`"warnings":["a_later_warning"],"updated_at":"2026-10-31T09:00:00.000Z","guest_column":true}`
		var r PermissionsResponseV1
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(r.Rows) != 2 || r.Rows[1].Permission.Valid() || r.Rows[1].Permission != "pin_messages" {
			t.Errorf("rows = %+v, want the unknown permission kept by name", r.Rows)
		}
		if len(r.Warnings) != 1 || r.Warnings[0].Valid() {
			t.Errorf("warnings = %v, want the unknown warning kept by name", r.Warnings)
		}
	})
	t.Run("whoami with a later permission", func(t *testing.T) {
		raw := `{"id":5,"kind":"human","name":"member-1","role":"member","permissions":["use_voice","pin_messages"],` +
			`"permissions_revision":7,"a_later_field":1}`
		var w WhoAmIResponseV1
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !slices.Equal(w.Permissions, []Permission{PermUseVoice, "pin_messages"}) || w.PermissionsRevision != 7 {
			t.Errorf("whoami = %+v", w)
		}
		if err := w.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
	t.Run("whoami from before ADR-014", func(t *testing.T) {
		var w WhoAmIResponseV1
		readFixture(t, "whoami-response-v1.json", &w)
		if w.Permissions != nil || w.PermissionsRevision != 0 {
			t.Errorf("whoami = %+v, want no permissions and revision 0", w)
		}
	})
	t.Run("error from before ADR-014", func(t *testing.T) {
		var e Error
		readFixture(t, "error.json", &e)
		if e.Permission != "" {
			t.Errorf("permission = %q, want empty", e.Permission)
		}
	})
	t.Run("error naming a later permission", func(t *testing.T) {
		var e Error
		if err := json.Unmarshal([]byte(`{"code":"permission_denied","message":"no","permission":"pin_messages"}`), &e); err != nil {
			t.Fatal(err)
		}
		if e.Permission != "pin_messages" || e.Permission.Valid() {
			t.Errorf("permission = %q", e.Permission)
		}
	})
	t.Run("counts from before ADR-014", func(t *testing.T) {
		var i InviteV1
		readFixture(t, "invite-v1.json", &i)
		var h ListHooksResponseV1
		readFixture(t, "list-hooks-response-v1.json", &h)
		var j JoinSettingsV1
		readFixture(t, "join-settings-v1-open.json", &j)
		if i.OtherChannels != 0 || h.OtherHooks != 0 || j.OtherOpenChannels != 0 {
			t.Errorf("counts = %d, %d, %d, want 0", i.OtherChannels, h.OtherHooks, j.OtherOpenChannels)
		}
	})
	t.Run("events document with a later field", func(t *testing.T) {
		var d PermissionsChangedV1
		if err := json.Unmarshal([]byte(`{"schema":"conch.permissions_changed.v1","a_later_field":true}`), &d); err != nil {
			t.Fatal(err)
		}
		if err := d.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
}

func TestPermissionsChangedV1Validate(t *testing.T) {
	for _, tc := range []struct {
		schema, wantErr string
	}{
		{PermissionsChangedSchemaV1, ""},
		{"", "permissions changed schema must be"},
		{"conch.permissions_changed.v2", "permissions changed schema must be"},
		{"conch.directory_changed.v1", "permissions changed schema must be"},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			assertValidation(t, PermissionsChangedV1{Schema: tc.schema}.Validate(), tc.wantErr)
		})
	}
}

func TestPermissionErrorCodes(t *testing.T) {
	for got, want := range map[string]string{
		ErrorCodePermissionDenied:    "permission_denied",
		ErrorCodePermissionsStale:    "permissions_stale",
		ErrorCodePermissionsConflict: "permissions_conflict",
		PermissionsChangedSchemaV1:   "conch.permissions_changed.v1",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
