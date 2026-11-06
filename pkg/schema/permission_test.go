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
		// The twelve-row document of a server with use_camera (ADR-015).
		{"permissions-response-v1-camera.json", func() any { return new(PermissionsResponseV1) }},
		{"put-permissions-request-v1.json", func() any { return new(PutPermissionsRequestV1) }},
		{"put-permissions-request-v1-preset.json", func() any { return new(PutPermissionsRequestV1) }},
		{"permissions-changed-v1.json", func() any { return new(PermissionsChangedV1) }},
		{"whoami-response-v1-permissions.json", func() any { return new(WhoAmIResponseV1) }},
		{"whoami-response-v1-permissions-none.json", func() any { return new(WhoAmIResponseV1) }},
		{"whoami-response-v1-permissions-camera.json", func() any { return new(WhoAmIResponseV1) }},
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
	// Each response fixture's rows are a prefix of the catalog, in order,
	// each at most once, and agree with the fixed cells and the preset table.
	// A prefix, not the whole catalog: the two older fixtures are what a
	// server before use_camera answered, eleven rows, and they stay valid
	// and byte-identical (ADR-015 decision 10). Only published fixtures get
	// that latitude; conchd itself always answers the full catalog, which
	// internal/server's TestPermissionsGet pins.
	for _, file := range []string{"permissions-response-v1.json", "permissions-response-v1-personal.json", "permissions-response-v1-camera.json"} {
		t.Run(file, func(t *testing.T) {
			var r PermissionsResponseV1
			readFixture(t, file, &r)
			if err := catalogPrefix(rowPermissions(r.Rows)); err != nil {
				t.Fatal(err)
			}
			var member []Permission
			for i, row := range r.Rows {
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
			// The preset table as this fixture's server knew it: only the
			// permissions its rows carry. An eleven-row fixture predates
			// use_camera, and so its presets do (issue #1361).
			inRows := func(ps []Permission) []Permission {
				var out []Permission
				for _, p := range ps {
					if slices.Contains(rowPermissions(r.Rows), p) {
						out = append(out, p)
					}
				}
				return out
			}
			if want := inRows(PresetMemberPermissions[r.Preset]); !slices.Equal(member, want) {
				t.Errorf("member column %v, preset %q is %v", member, r.Preset, want)
			}
			for _, p := range r.Presets {
				if want := inRows(PresetMemberPermissions[p.Preset]); !slices.Equal(p.Member, want) {
					t.Errorf("presets[%s] = %v, want %v", p.Preset, p.Member, want)
				}
			}
		})
	}
}

// rowPermissions is the permission of each row, in the rows' order.
func rowPermissions(rows []PermissionRowV1) []Permission {
	out := make([]Permission, len(rows))
	for i, row := range rows {
		out[i] = row.Permission
	}
	return out
}

// catalogPrefix is the rule a published permissions document is held to:
// its permissions are a prefix of the catalog, in catalog order, each at
// most once. It accepts the full catalog and any document from a server
// whose catalog was shorter, and refuses a reordered, a repeated or a gapped
// list, and a permission outside the catalog.
func catalogPrefix(got []Permission) error {
	if len(got) > len(Permissions) {
		return fmt.Errorf("%d rows, the catalog has %d", len(got), len(Permissions))
	}
	for i, p := range got {
		if p != Permissions[i] {
			return fmt.Errorf("rows[%d] = %s, want %s: rows are a prefix of the catalog, in order, each once", i, p, Permissions[i])
		}
	}
	return nil
}

// The fixture rule itself: an eleven-row document (a server before
// use_camera) and the full twelve-row one pass; anything that is not a prefix
// of the catalog in order fails.
func TestPermissionFixtureRowRule(t *testing.T) {
	eleven := Permissions[:11]
	tests := []struct {
		name    string
		rows    []Permission
		wantErr string
	}{
		{name: "the full catalog", rows: Permissions},
		{name: "eleven rows, from before use_camera", rows: eleven},
		{name: "one row", rows: Permissions[:1]},
		{name: "no rows", rows: nil},
		{name: "reordered", rows: []Permission{PermCreatePrivateChannels, PermCreatePublicChannels}, wantErr: "rows[0] = create_private_channels, want create_public_channels"},
		{name: "repeated", rows: []Permission{PermCreatePublicChannels, PermCreatePublicChannels}, wantErr: "rows[1] = create_public_channels, want create_private_channels"},
		{name: "gapped", rows: append(slices.Clone(Permissions[:10]), PermUseCamera), wantErr: "rows[10] = use_camera, want read_audit_log"},
		{name: "not a prefix: use_camera first", rows: []Permission{PermUseCamera}, wantErr: "rows[0] = use_camera, want create_public_channels"},
		{name: "a thirteenth row", rows: append(slices.Clone(Permissions), "pin_messages"), wantErr: "13 rows, the catalog has 12"},
		{name: "a row outside the catalog", rows: append(slices.Clone(eleven), "pin_messages"), wantErr: "rows[11] = pin_messages, want use_camera"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := catalogPrefix(tt.rows)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("catalogPrefix() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Errorf("catalogPrefix() = nil, want an error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("catalogPrefix() = %v, want an error containing %q", err, tt.wantErr)
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
		"change_server_settings", "read_audit_log", "use_camera",
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
	// ADR-015: off for both, member-grantable, nothing fixed.
	PermUseCamera: {member: true, operator: true}, // on by default since issue #1361
}

// TestUseCameraDefaults is the dedicated test the security review of
// ADR-015 asked for (design §11.1, §15), as amended by issue #1361: use_camera
// is on by default for both roles on a new server (an existing one keeps its
// cells off: store migration 44), is in both presets, is not operator-only,
// not fixed in either column, and requires exactly use_voice. It is written
// out, not derived from the table above, so that it fails on its own if the
// value or its treatment changes, whichever way the methods are reshaped.
func TestUseCameraDefaults(t *testing.T) {
	if !PermUseCamera.Valid() {
		t.Fatal("use_camera is not in the catalog")
	}
	if got := Permissions[len(Permissions)-1]; got != PermUseCamera {
		t.Errorf("the last permission is %s, want use_camera", got)
	}
	for _, r := range []PermissionRole{PermissionRoleMember, PermissionRoleOperator} {
		if !PermUseCamera.Default(r) {
			t.Errorf("Default(%s) = false: use_camera is on by default on a new server (issue #1361)", r)
		}
		if fixed, value := PermUseCamera.Fixed(r); fixed || value {
			t.Errorf("Fixed(%s) = (%v, %v), want (false, false): the cell is configurable", r, fixed, value)
		}
	}
	if PermUseCamera.OperatorOnly() {
		t.Error("OperatorOnly() = true: members may be granted use_camera")
	}
	if got := PermUseCamera.Requires(); !slices.Equal(got, []Permission{PermUseVoice}) {
		t.Errorf("Requires() = %v, want exactly [use_voice]", got)
	}
	for preset, on := range PresetMemberPermissions {
		if !slices.Contains(on, PermUseCamera) {
			t.Errorf("preset %s leaves use_camera off: %v", preset, on)
		}
	}
	// The reshaped Default falls through to off, and OperatorOnly to
	// operator-only: a permission the methods do not name is granted to
	// nobody. Valid is bypassed by a value that is in the catalog but
	// unnamed only in a future edit, so this guards the fall-throughs with
	// an unknown value, the one case that reaches them today.
	for _, unknown := range []Permission{"pin_messages", ""} {
		for _, r := range []PermissionRole{PermissionRoleMember, PermissionRoleOperator} {
			if unknown.Default(r) {
				t.Errorf("Default(%q, %s) = true, want off", unknown, r)
			}
		}
		if !unknown.OperatorOnly() {
			t.Errorf("OperatorOnly(%q) = false, want operator-only", unknown)
		}
	}
}

// TestPermissionDefaultsUnchanged pins every existing permission's default,
// by name, against the reshaped Default: the six on for both, the three on
// for operators, the two off for both, and use_camera off for both. The
// table above drives TestPermissionCells; this list is a second, independent
// statement of the same defaults.
func TestPermissionDefaultsUnchanged(t *testing.T) {
	type defaults struct{ member, operator bool }
	want := map[Permission]defaults{
		PermCreatePublicChannels:  {true, true},
		PermCreatePrivateChannels: {true, true},
		PermMakeMemberLinks:       {true, true},
		PermSeeMemberDirectory:    {true, true},
		PermUseVoice:              {true, true},
		PermShareScreen:           {true, true},
		PermManageMembers:         {false, true},
		PermActAsMembers:          {false, true},
		PermManageChannels:        {false, true},
		PermChangeServerSettings:  {false, false},
		PermReadAuditLog:          {false, false},
		PermUseCamera:             {true, true}, // issue #1361
	}
	if len(want) != len(Permissions) {
		t.Fatalf("the list has %d rows, the catalog %d", len(want), len(Permissions))
	}
	for _, p := range Permissions {
		w := want[p]
		if got := p.Default(PermissionRoleMember); got != w.member {
			t.Errorf("%s: Default(member) = %v, want %v", p, got, w.member)
		}
		if got := p.Default(PermissionRoleOperator); got != w.operator {
			t.Errorf("%s: Default(operator) = %v, want %v", p, got, w.operator)
		}
	}
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
		PermUseCamera:    {PermUseVoice},
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
	if got, want := PresetMemberPermissions[PresetPersonal], []Permission{PermUseVoice, PermShareScreen, PermUseCamera}; !slices.Equal(got, want) {
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
// 18 today (7 member cells, 11 operator cells). With no pair twice, that is
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
	t.Run("whoami of an owner with use_camera", func(t *testing.T) {
		var w WhoAmIResponseV1
		readFixture(t, "whoami-response-v1-permissions-camera.json", &w)
		if err := w.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
		if !slices.Equal(w.Permissions, Permissions) {
			t.Errorf("permissions = %v, want the whole catalog in order", w.Permissions)
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

// AgentGrantable is exactly the five administrative permissions that speak
// as nobody (ai-administrators.md §3.1): never act_as_members, never a
// member permission, never a value outside the catalog.
func TestPermissionAgentGrantable(t *testing.T) {
	want := map[Permission]bool{
		PermManageMembers:        true,
		PermManageChannels:       true,
		PermMakeMemberLinks:      true,
		PermChangeServerSettings: true,
		PermReadAuditLog:         true,
	}
	var grantable []Permission
	for _, p := range Permissions {
		if got := p.AgentGrantable(); got != want[p] {
			t.Errorf("%s.AgentGrantable() = %v, want %v", p, got, want[p])
		}
		if p.AgentGrantable() {
			grantable = append(grantable, p)
		}
	}
	if len(grantable) != 5 {
		t.Errorf("agent-grantable permissions = %v, want five", grantable)
	}
	if PermActAsMembers.AgentGrantable() {
		t.Error("act_as_members is agent-grantable; speaking as people is the one power the design rules out")
	}
	for _, unknown := range []Permission{"", "*", "owner", "manages", "can_decide", "MANAGE_MEMBERS", "manage_members "} {
		if unknown.AgentGrantable() {
			t.Errorf("AgentGrantable(%q) = true, want false", unknown)
		}
	}
}
