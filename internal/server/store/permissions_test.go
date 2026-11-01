package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The catalog as the design writes it (docs/design/admin-roles.md §4.1),
// spelled out here rather than read from pkg/schema, so these tests check
// the store against the design and not against itself.
var (
	designDefaults = map[schema.Permission][2]bool{ // {member, operator}
		schema.PermCreatePublicChannels:  {true, true},
		schema.PermCreatePrivateChannels: {true, true},
		schema.PermMakeMemberLinks:       {true, true},
		schema.PermSeeMemberDirectory:    {true, true},
		schema.PermUseVoice:              {true, true},
		schema.PermShareScreen:           {true, true},
		schema.PermManageMembers:         {false, true},
		schema.PermActAsMembers:          {false, true},
		schema.PermManageChannels:        {false, true},
		schema.PermChangeServerSettings:  {false, false},
		schema.PermReadAuditLog:          {false, false},
	}
	designFixed = map[permissionCell]bool{
		{schema.PermissionRoleMember, schema.PermManageMembers}:        false,
		{schema.PermissionRoleMember, schema.PermActAsMembers}:         false,
		{schema.PermissionRoleMember, schema.PermManageChannels}:       false,
		{schema.PermissionRoleMember, schema.PermChangeServerSettings}: false,
		{schema.PermissionRoleMember, schema.PermReadAuditLog}:         false,
		{schema.PermissionRoleOperator, schema.PermSeeMemberDirectory}: true,
	}
	designRequires = map[schema.Permission]schema.Permission{
		schema.PermShareScreen:  schema.PermUseVoice,
		schema.PermActAsMembers: schema.PermManageMembers,
	}
	// designPresets are the member cells each preset turns on (§6).
	designPresets = map[schema.PermissionPreset][]schema.Permission{
		schema.PresetPersonal: {schema.PermUseVoice, schema.PermShareScreen},
		schema.PresetGroup: {schema.PermCreatePublicChannels, schema.PermCreatePrivateChannels, schema.PermMakeMemberLinks,
			schema.PermSeeMemberDirectory, schema.PermUseVoice, schema.PermShareScreen},
	}
)

func roleIndex(r schema.PermissionRole) int {
	if r == schema.PermissionRoleOperator {
		return 1
	}
	return 0
}

// storedRow is a role_permissions row as written by hand.
type storedRow struct {
	role, perm string
	granted    int
}

// designCell is a cell's value by the design: fixed, else a known stored
// row, else the default.
func designCell(rows []storedRow, r schema.PermissionRole, p schema.Permission) bool {
	if v, ok := designFixed[permissionCell{r, p}]; ok {
		return v
	}
	for _, row := range rows {
		if row.role == string(r) && row.perm == string(p) {
			return row.granted == 1
		}
	}
	return designDefaults[p][roleIndex(r)]
}

// designRoleHolds: the cell, operators' cell under a member's (precedence),
// and the dependency.
func designRoleHolds(rows []storedRow, r schema.PermissionRole, p schema.Permission) bool {
	if !designCell(rows, r, p) {
		return false
	}
	if r == schema.PermissionRoleMember && !designRoleHolds(rows, schema.PermissionRoleOperator, p) {
		return false
	}
	if req, ok := designRequires[p]; ok && !designRoleHolds(rows, r, req) {
		return false
	}
	return true
}

// permSubject is one principal of the permittedTx table.
type permSubject struct {
	name             string
	agent, guest     bool
	owner, operator  bool
	voice, invite    bool
	disabled, hidden bool
	unknown          bool
}

// designHolds is §7.1's answer for subject s.
func designHolds(rows []storedRow, s permSubject, p schema.Permission) bool {
	if s.unknown || s.agent || s.guest || s.disabled || s.hidden {
		return false
	}
	if s.owner {
		return true
	}
	r := schema.PermissionRoleMember
	if s.operator {
		r = schema.PermissionRoleOperator
	}
	if !designRoleHolds(rows, r, p) {
		return false
	}
	if (p == schema.PermUseVoice || p == schema.PermShareScreen) && !s.voice {
		return false
	}
	if p == schema.PermMakeMemberLinks && r == schema.PermissionRoleMember && !s.invite {
		return false
	}
	if req, ok := designRequires[p]; ok && !designHolds(rows, s, req) {
		return false
	}
	return true
}

type permFixture struct {
	t   *testing.T
	s   *Store
	ctx context.Context
	ids map[string]int64
}

// newPermFixture seeds, through the store, one principal of every kind the
// permission tests need. Brakes the routes refuse to set (on an owner) are
// written by hand, as a database edit would.
func newPermFixture(t *testing.T) *permFixture {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	f := &permFixture{t: t, s: s, ctx: ctx, ids: map[string]int64{}}
	owner, _, _, err := s.BootstrapOperator(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	f.ids["owner"] = owner.ID
	human := func(name string) int64 {
		t.Helper()
		p, err := s.CreatePrincipal(ctx, PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		f.ids[name] = p.ID
		if _, err := s.SetMayInvite(ctx, "system", p.ID, true); err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	operator := func(name string) int64 {
		t.Helper()
		id := human(name)
		mark := false
		if _, err := s.SetRole(ctx, principalActor(owner.ID), owner.ID, id, RoleOperator, &mark); err != nil {
			t.Fatal(err)
		}
		return id
	}
	raw := func(stmt string, id int64) {
		t.Helper()
		if _, err := s.db.Exec(stmt, id); err != nil {
			t.Fatal(err)
		}
	}
	id := human("owner-braked")
	promoteToOwner(t, s, owner.ID, id)
	raw("UPDATE principals SET voice_allowed = 0, may_invite = 0 WHERE id = ?", id)
	operator("operator-1")
	raw("UPDATE principals SET may_invite = 0 WHERE id = ?", operator("operator-no-invite"))
	raw("UPDATE principals SET voice_allowed = 0 WHERE id = ?", operator("operator-no-voice"))
	operator("operator-no-settings")
	human("member-1")
	raw("UPDATE principals SET voice_allowed = 0 WHERE id = ?", human("member-no-voice"))
	if _, err := s.SetMayInvite(ctx, "system", human("member-no-invite"), false); err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	f.ids["agent-1"] = agent.ID
	if _, err := s.DisablePrincipal(ctx, "system", operator("operator-disabled")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisablePrincipal(ctx, "system", human("member-disabled")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HidePrincipal(ctx, "system", human("member-hidden"), HideOptions{}); err != nil {
		t.Fatal(err)
	}
	ch, err := s.CreateChannel(ctx, "channel-a")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGuest(ctx, "system", NewGuest{Origin: schema.GuestOriginAddress,
		Channels: []int64{ch.ID}, Idle: time.Hour, Absolute: 24 * time.Hour, VoiceAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	f.ids["guest-1"] = g.Principal.ID
	return f
}

func (f *permFixture) setRows(rows []storedRow) {
	f.t.Helper()
	if _, err := f.s.db.Exec("DELETE FROM role_permissions"); err != nil {
		f.t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := f.s.db.Exec("INSERT INTO role_permissions (role, permission, granted, updated_at) VALUES (?, ?, ?, 1)",
			r.role, r.perm, r.granted); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *permFixture) revision() int64 {
	f.t.Helper()
	doc, err := f.s.Permissions(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return doc.Revision
}

func (f *permFixture) put(writer string, req schema.PutPermissionsRequestV1) (PermissionsWrite, error) {
	id := f.ids[writer] // an unknown name is actor 0
	return f.s.PutPermissions(f.ctx, principalActor(id), id, req, time.Now())
}

// ownerSets applies changes as the owner at the current revision.
func (f *permFixture) ownerSets(changes ...schema.PermissionChangeV1) {
	f.t.Helper()
	if _, err := f.put("owner", schema.PutPermissionsRequestV1{Revision: f.revision(), Changes: changes}); err != nil {
		f.t.Fatalf("owner sets %+v: %v", changes, err)
	}
}

func (f *permFixture) cells() map[permissionCell]bool {
	f.t.Helper()
	doc, err := f.s.Permissions(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[permissionCell]bool{}
	for _, r := range doc.Rows {
		out[permissionCell{schema.PermissionRoleMember, r.Permission}] = r.Member
		out[permissionCell{schema.PermissionRoleOperator, r.Permission}] = r.Operator
	}
	return out
}

func member(p schema.Permission, on bool) schema.PermissionChangeV1 {
	return schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleMember, Granted: on}
}

func operatorCell(p schema.Permission, on bool) schema.PermissionChangeV1 {
	return schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleOperator, Granted: on}
}

var permSubjects = []permSubject{
	{name: "owner", owner: true, operator: true, voice: true},
	{name: "owner-braked", owner: true, operator: true},
	{name: "operator-1", operator: true, voice: true, invite: true},
	{name: "operator-no-invite", operator: true, voice: true},
	{name: "operator-no-voice", operator: true, invite: true},
	{name: "member-1", voice: true, invite: true},
	{name: "member-no-voice", invite: true},
	{name: "member-no-invite", voice: true},
	{name: "agent-1", agent: true, voice: true},
	{name: "member-disabled", disabled: true, voice: true, invite: true},
	{name: "operator-disabled", operator: true, disabled: true, voice: true, invite: true},
	{name: "member-hidden", hidden: true, voice: true, invite: true},
	{name: "guest-1", guest: true, voice: true},
	{name: "unknown", unknown: true},
}

// permDocuments are the documents of the permittedTx table: none, each
// configurable cell flipped from its default by a row of its own, and the
// rows a store never writes (an unknown permission, a fixed cell
// contradicted, a dependency broken).
func permDocuments() []struct {
	name string
	rows []storedRow
} {
	docs := []struct {
		name string
		rows []storedRow
	}{{name: "no rows"}}
	for _, p := range schema.Permissions {
		for _, r := range permissionColumns {
			if _, fixed := designFixed[permissionCell{r, p}]; fixed {
				continue
			}
			g := 1
			if designDefaults[p][roleIndex(r)] {
				g = 0
			}
			docs = append(docs, struct {
				name string
				rows []storedRow
			}{fmt.Sprintf("%s.%s flipped", r, p), []storedRow{{string(r), string(p), g}}})
		}
	}
	docs = append(docs, []struct {
		name string
		rows []storedRow
	}{
		{"unknown permission rows", []storedRow{{"member", "teleport", 1}, {"operator", "teleport", 0}}},
		{"member manage_members on by hand", []storedRow{{"member", "manage_members", 1}, {"member", "read_audit_log", 1}}},
		{"operator see_member_directory off by hand", []storedRow{{"operator", "see_member_directory", 0}}},
		{"member use_voice off with share_screen on", []storedRow{{"member", "use_voice", 0}, {"member", "share_screen", 1}}},
		{"operator use_voice off with share_screen on", []storedRow{{"operator", "use_voice", 0}, {"operator", "share_screen", 1}}},
		{"operator manage_members off with act_as_members on", []storedRow{{"operator", "manage_members", 0}, {"operator", "act_as_members", 1}}},
		{"members above operators by hand", []storedRow{{"operator", "create_public_channels", 0}, {"member", "create_public_channels", 1}}},
		{"settings and audit for operators", []storedRow{{"operator", "change_server_settings", 1}, {"operator", "read_audit_log", 1}}},
	}...)
	return docs
}

// TestPermitted checks permittedTx, Permitted and EffectivePermissions for
// every principal kind, role and brake, every permission, and every
// document of permDocuments, against §7.1 as written in designHolds.
func TestPermitted(t *testing.T) {
	f := newPermFixture(t)
	for _, doc := range permDocuments() {
		t.Run(doc.name, func(t *testing.T) {
			f.setRows(doc.rows)
			for _, subj := range permSubjects {
				id, ok := f.ids[subj.name]
				if !ok {
					if !subj.unknown {
						t.Fatalf("no principal %s", subj.name)
					}
					id = 1 << 40
				}
				var want []schema.Permission
				for _, p := range schema.Permissions {
					w := designHolds(doc.rows, subj, p)
					if w {
						want = append(want, p)
					}
					got, err := f.s.Permitted(f.ctx, id, p)
					if err != nil || got != w {
						t.Errorf("%s holds %s = %v, %v; want %v", subj.name, p, got, err, w)
					}
				}
				got, rev, err := f.s.EffectivePermissions(f.ctx, id)
				if err != nil || rev != 1 || got == nil || !reflect.DeepEqual(append([]schema.Permission{}, want...), got) {
					t.Errorf("%s effective = %v (revision %d), %v; want %v", subj.name, got, rev, err, want)
				}
			}
		})
	}
	// An unknown permission and actor 0 hold nothing, even for an owner.
	f.setRows(nil)
	for _, c := range []struct {
		id int64
		p  schema.Permission
	}{{f.ids["owner"], "teleport"}, {f.ids["owner"], ""}, {0, schema.PermUseVoice}} {
		if got, err := f.s.Permitted(f.ctx, c.id, c.p); err != nil || got {
			t.Errorf("principal %d holds %q = %v, %v; want false", c.id, c.p, got, err)
		}
	}
}

// Pinned answers, written out by hand, for the rules TestPermitted's
// oracle encodes: so the oracle cannot drift with the code.
func TestPermittedPinned(t *testing.T) {
	f := newPermFixture(t)
	for _, tt := range []struct {
		name string
		rows []storedRow
		who  string
		p    schema.Permission
		want bool
	}{
		{"owner skips the voice brake", nil, "owner-braked", schema.PermShareScreen, true},
		{"owner skips the link brake", nil, "owner-braked", schema.PermMakeMemberLinks, true},
		{"owner holds what no column does", nil, "owner", schema.PermReadAuditLog, true},
		{"operator lacks settings by default", nil, "operator-1", schema.PermChangeServerSettings, false},
		{"link brake spares operators", nil, "operator-no-invite", schema.PermMakeMemberLinks, true},
		{"link brake stops members", nil, "member-no-invite", schema.PermMakeMemberLinks, false},
		{"voice brake stops operators", nil, "operator-no-voice", schema.PermUseVoice, false},
		{"voice brake stops share for operators", nil, "operator-no-voice", schema.PermShareScreen, false},
		{"voice brake stops members", nil, "member-no-voice", schema.PermShareScreen, false},
		{"member holds the defaults", nil, "member-1", schema.PermSeeMemberDirectory, true},
		{"member never holds manage_members", []storedRow{{"member", "manage_members", 1}}, "member-1", schema.PermManageMembers, false},
		{"operator always sees the directory", []storedRow{{"operator", "see_member_directory", 0}}, "operator-1", schema.PermSeeMemberDirectory, true},
		{"share needs voice", []storedRow{{"member", "use_voice", 0}, {"member", "share_screen", 1}}, "member-1", schema.PermShareScreen, false},
		{"act_as needs manage", []storedRow{{"operator", "manage_members", 0}, {"operator", "act_as_members", 1}}, "operator-1", schema.PermActAsMembers, false},
		{"members never above operators", []storedRow{{"operator", "use_voice", 0}, {"operator", "share_screen", 0}}, "member-1", schema.PermUseVoice, false},
		{"an unknown row grants nothing", []storedRow{{"member", "teleport", 1}}, "member-1", schema.PermManageChannels, false},
		{"granted settings", []storedRow{{"operator", "change_server_settings", 1}}, "operator-1", schema.PermChangeServerSettings, true},
		{"agent holds nothing", nil, "agent-1", schema.PermUseVoice, false},
		{"guest holds nothing", nil, "guest-1", schema.PermUseVoice, false},
		{"disabled operator holds nothing", nil, "operator-disabled", schema.PermManageMembers, false},
		{"hidden member holds nothing", nil, "member-hidden", schema.PermUseVoice, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.setRows(tt.rows)
			if got, err := f.s.Permitted(f.ctx, f.ids[tt.who], tt.p); err != nil || got != tt.want {
				t.Errorf("%s holds %s = %v, %v; want %v", tt.who, tt.p, got, err, tt.want)
			}
		})
	}
}

// Reads apply fixed cells in code and never return a row the build does not
// know; answers are new slices, never the schema's own.
func TestPermissionsRead(t *testing.T) {
	f := newPermFixture(t)
	f.setRows([]storedRow{{"member", "teleport", 1}, {"member", "manage_members", 1}, {"operator", "see_member_directory", 0},
		{"member", "create_public_channels", 0}})
	doc, err := f.s.Permissions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Revision != 1 || len(doc.Rows) != len(schema.Permissions) {
		t.Fatalf("document = %+v", doc)
	}
	for i, r := range doc.Rows {
		p := schema.Permissions[i]
		mFixed, mFixedOK := designFixed[permissionCell{schema.PermissionRoleMember, p}]
		oFixed, oFixedOK := designFixed[permissionCell{schema.PermissionRoleOperator, p}]
		want := schema.PermissionRowV1{Permission: p, Member: designDefaults[p][0], Operator: designDefaults[p][1],
			MemberFixed: mFixedOK, OperatorFixed: oFixedOK}
		if mFixedOK {
			want.Member = mFixed
		}
		if oFixedOK {
			want.Operator = oFixed
		}
		if p == schema.PermCreatePublicChannels {
			want.Member = false
		}
		if r != want {
			t.Errorf("row %d = %+v, want %+v", i, r, want)
		}
	}
	if doc.Preset != "" {
		t.Errorf("preset = %q, want none (create_public_channels is off)", doc.Preset)
	}
	f.setRows(nil)
	if doc, err := f.s.Permissions(f.ctx); err != nil || doc.Preset != schema.PresetGroup {
		t.Errorf("defaults' preset = %q, %v; want group", doc.Preset, err)
	}

	// Mutating an answer leaves the catalog and the next answer alone.
	before := append([]schema.Permission(nil), schema.Permissions...)
	perms, _, err := f.s.EffectivePermissions(f.ctx, f.ids["owner"])
	if err != nil || len(perms) != len(schema.Permissions) {
		t.Fatalf("owner effective = %v, %v", perms, err)
	}
	perms[0] = "teleport"
	doc.Rows[0].Permission = "teleport"
	if !reflect.DeepEqual(before, schema.Permissions) {
		t.Fatalf("schema.Permissions changed to %v", schema.Permissions)
	}
	again, _, err := f.s.EffectivePermissions(f.ctx, f.ids["owner"])
	if err != nil || again[0] != schema.PermCreatePublicChannels {
		t.Errorf("owner effective after a caller's edit = %v, %v", again, err)
	}
	if doc, err := f.s.Permissions(f.ctx); err != nil || doc.Rows[0].Permission != schema.PermCreatePublicChannels {
		t.Errorf("rows after a caller's edit = %+v, %v", doc.Rows, err)
	}
	if !reflect.DeepEqual(designPresets[schema.PresetPersonal], schema.PresetMemberPermissions[schema.PresetPersonal]) ||
		!reflect.DeepEqual(designPresets[schema.PresetGroup], schema.PresetMemberPermissions[schema.PresetGroup]) {
		t.Errorf("schema.PresetMemberPermissions = %v, want the design's %v", schema.PresetMemberPermissions, designPresets)
	}
}

func lastPermissionsChanged(t *testing.T, s *Store) (AuditEvent, int) {
	t.Helper()
	evs, err := s.ListAuditEvents(context.Background(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	var last AuditEvent
	n := 0
	for _, e := range evs {
		if e.Action == AuditPermissionsChanged {
			last, n = e, n+1
		}
	}
	return last, n
}

// TestPutPermissions checks every §4.4 invariant, the writer rule, the
// stale revision, the no-change rule and presets, each inside the write.
func TestPutPermissions(t *testing.T) {
	grantSettings := operatorCell(schema.PermChangeServerSettings, true)
	type result struct {
		err  error
		perm schema.Permission
	}
	tests := []struct {
		name   string
		setup  []schema.PermissionChangeV1 // the owner's, before the write
		writer string
		req    func(rev int64) schema.PutPermissionsRequestV1
		want   result
		// cells the document holds after a write that succeeded
		after map[permissionCell]bool
		// detail is the permissions_changed row's detail; "" means the
		// write wrote none and kept the revision
		detail string
	}{
		{
			name: "owner turns a member cell off", writer: "owner",
			req:    changes(member(schema.PermCreatePublicChannels, false)),
			after:  map[permissionCell]bool{{schema.PermissionRoleMember, schema.PermCreatePublicChannels}: false},
			detail: "revision=2;member.create_public_channels=true->false",
		},
		{
			name: "owner sets both columns at once", writer: "owner",
			req: changes(operatorCell(schema.PermReadAuditLog, true), member(schema.PermMakeMemberLinks, false), operatorCell(schema.PermManageChannels, false)),
			after: map[permissionCell]bool{
				{schema.PermissionRoleOperator, schema.PermReadAuditLog}:   true,
				{schema.PermissionRoleMember, schema.PermMakeMemberLinks}:  false,
				{schema.PermissionRoleOperator, schema.PermManageChannels}: false,
			},
			detail: "revision=2;member.make_member_links=true->false;operator.manage_channels=true->false;operator.read_audit_log=false->true",
		},
		{
			name: "precedence: operator cell off under a member cell", writer: "owner",
			req:  changes(operatorCell(schema.PermCreatePrivateChannels, false)),
			want: result{ErrPermissionsConflict, schema.PermCreatePrivateChannels},
		},
		{
			name:   "precedence: member cell on over an operator cell",
			setup:  []schema.PermissionChangeV1{member(schema.PermMakeMemberLinks, false), operatorCell(schema.PermMakeMemberLinks, false)},
			writer: "owner",
			req:    changes(member(schema.PermMakeMemberLinks, true)),
			want:   result{ErrPermissionsConflict, schema.PermMakeMemberLinks},
		},
		{
			name: "precedence holds when both cells go off together", writer: "owner",
			req: changes(operatorCell(schema.PermCreatePublicChannels, false), member(schema.PermCreatePublicChannels, false)),
			after: map[permissionCell]bool{
				{schema.PermissionRoleMember, schema.PermCreatePublicChannels}:   false,
				{schema.PermissionRoleOperator, schema.PermCreatePublicChannels}: false,
			},
			detail: "revision=2;member.create_public_channels=true->false;operator.create_public_channels=true->false",
		},
		{
			name: "fixed: a write naming the operator's directory cell, even on", writer: "owner",
			req:  changes(operatorCell(schema.PermSeeMemberDirectory, true), member(schema.PermCreatePublicChannels, false)),
			want: result{ErrPermissionsInvalid, ""},
		},
		{
			name:   "dependency: share on without voice",
			setup:  []schema.PermissionChangeV1{member(schema.PermShareScreen, false), member(schema.PermUseVoice, false)},
			writer: "owner",
			req:    changes(member(schema.PermShareScreen, true)),
			want:   result{ErrPermissionsConflict, schema.PermShareScreen},
		},
		{
			name: "dependency: voice off under share", writer: "owner",
			req:  changes(member(schema.PermUseVoice, false)),
			want: result{ErrPermissionsConflict, schema.PermShareScreen},
		},
		{
			name: "dependency: operator voice off under share", writer: "owner",
			setup: []schema.PermissionChangeV1{member(schema.PermShareScreen, false), member(schema.PermUseVoice, false)},
			req:   changes(operatorCell(schema.PermUseVoice, false)),
			want:  result{ErrPermissionsConflict, schema.PermShareScreen},
		},
		{
			name: "dependency: manage_members off under act_as_members", writer: "owner",
			req:  changes(operatorCell(schema.PermManageMembers, false)),
			want: result{ErrPermissionsConflict, schema.PermActAsMembers},
		},
		{
			name:   "dependency: act_as_members on without manage_members",
			setup:  []schema.PermissionChangeV1{operatorCell(schema.PermActAsMembers, false), operatorCell(schema.PermManageMembers, false)},
			writer: "owner",
			req:    changes(operatorCell(schema.PermActAsMembers, true)),
			want:   result{ErrPermissionsConflict, schema.PermActAsMembers},
		},
		{
			name: "dependency: both off together is fine", writer: "owner",
			req: changes(operatorCell(schema.PermManageMembers, false), operatorCell(schema.PermActAsMembers, false)),
			after: map[permissionCell]bool{
				{schema.PermissionRoleOperator, schema.PermManageMembers}: false,
				{schema.PermissionRoleOperator, schema.PermActAsMembers}:  false,
			},
			detail: "revision=2;operator.manage_members=true->false;operator.act_as_members=true->false",
		},
		{
			name: "fixed: a member's operator-only cell", writer: "owner",
			req:  changes(member(schema.PermManageMembers, false)),
			want: result{ErrPermissionsInvalid, ""},
		},
		{
			name: "fixed: a member granted an operator-only permission", writer: "owner",
			req:  changes(member(schema.PermReadAuditLog, true)),
			want: result{ErrPermissionsInvalid, ""},
		},
		{
			name: "fixed: the operator's directory cell", writer: "owner",
			req:  changes(operatorCell(schema.PermSeeMemberDirectory, false)),
			want: result{ErrPermissionsInvalid, ""},
		},
		{
			name: "unknown permission", writer: "owner",
			req:  changes(member("teleport", false)),
			want: result{ErrPermissionsInvalid, ""},
		},
		{
			name: "unknown role", writer: "owner",
			req:  changes(schema.PermissionChangeV1{Permission: schema.PermUseVoice, Role: "guest"}),
			want: result{ErrPermissionsInvalid, ""},
		},
		{
			name: "both changes and a preset", writer: "owner",
			req: func(rev int64) schema.PutPermissionsRequestV1 {
				return schema.PutPermissionsRequestV1{Revision: rev, Preset: schema.PresetPersonal, Changes: []schema.PermissionChangeV1{member(schema.PermUseVoice, true)}}
			},
			want: result{ErrPermissionsInvalid, ""},
		},
		{
			name: "operator writes the operator column", setup: []schema.PermissionChangeV1{grantSettings}, writer: "operator-1",
			req:  changes(operatorCell(schema.PermManageChannels, false)),
			want: result{ErrPermissionsOwnerRequired, schema.PermManageChannels},
		},
		{
			name: "operator writes the operator column to what it is", setup: []schema.PermissionChangeV1{grantSettings}, writer: "operator-1",
			req:  changes(member(schema.PermUseVoice, true), operatorCell(schema.PermUseVoice, true)),
			want: result{ErrPermissionsOwnerRequired, schema.PermUseVoice},
		},
		{
			name: "operator without change_server_settings", writer: "operator-1",
			req:  changes(member(schema.PermCreatePublicChannels, false)),
			want: result{ErrPermissionDenied, schema.PermChangeServerSettings},
		},
		{
			name: "member writer", writer: "member-1",
			req:  changes(member(schema.PermCreatePublicChannels, false)),
			want: result{ErrPermissionDenied, schema.PermChangeServerSettings},
		},
		{
			name: "agent writer", writer: "agent-1",
			req:  changes(member(schema.PermCreatePublicChannels, false)),
			want: result{ErrPermissionDenied, schema.PermChangeServerSettings},
		},
		{
			name: "disabled operator with change_server_settings", setup: []schema.PermissionChangeV1{grantSettings}, writer: "operator-disabled",
			req:  changes(member(schema.PermCreatePublicChannels, false)),
			want: result{ErrPermissionDenied, schema.PermChangeServerSettings},
		},
		{
			name: "guest writer", writer: "guest-1",
			req:  changes(member(schema.PermCreatePublicChannels, false)),
			want: result{ErrPermissionDenied, schema.PermChangeServerSettings},
		},
		{
			name: "actor 0", writer: "",
			req:  changes(member(schema.PermCreatePublicChannels, false)),
			want: result{ErrPermissionDenied, schema.PermChangeServerSettings},
		},
		{
			name: "operator with change_server_settings writes the member column", setup: []schema.PermissionChangeV1{grantSettings}, writer: "operator-1",
			req:    changes(member(schema.PermSeeMemberDirectory, false)),
			after:  map[permissionCell]bool{{schema.PermissionRoleMember, schema.PermSeeMemberDirectory}: false},
			detail: "revision=3;member.see_member_directory=true->false",
		},
		{
			name:   "operator turns on a member cell it holds",
			setup:  []schema.PermissionChangeV1{grantSettings, member(schema.PermSeeMemberDirectory, false)},
			writer: "operator-1",
			req:    changes(member(schema.PermSeeMemberDirectory, true)),
			after:  map[permissionCell]bool{{schema.PermissionRoleMember, schema.PermSeeMemberDirectory}: true},
			detail: "revision=3;member.see_member_directory=false->true",
		},
		{
			name:   "operator turns on a member cell its own voice brake withholds",
			setup:  []schema.PermissionChangeV1{grantSettings, member(schema.PermShareScreen, false), member(schema.PermUseVoice, false)},
			writer: "operator-no-voice",
			req:    changes(member(schema.PermUseVoice, true)),
			want:   result{ErrPermissionDenied, schema.PermUseVoice},
		},
		{
			name:   "operator turns on share its own voice brake withholds",
			setup:  []schema.PermissionChangeV1{grantSettings, member(schema.PermShareScreen, false)},
			writer: "operator-no-voice",
			req:    changes(member(schema.PermShareScreen, true)),
			want:   result{ErrPermissionDenied, schema.PermShareScreen},
		},
		{
			name:   "operator turns on a member cell the operator column lacks",
			setup:  []schema.PermissionChangeV1{grantSettings, member(schema.PermCreatePrivateChannels, false), operatorCell(schema.PermCreatePrivateChannels, false)},
			writer: "operator-1",
			req:    changes(member(schema.PermCreatePrivateChannels, true)),
			want:   result{ErrPermissionsConflict, schema.PermCreatePrivateChannels},
		},
		{
			name: "restricted operator turns cells off", setup: []schema.PermissionChangeV1{grantSettings}, writer: "operator-no-voice",
			req: changes(member(schema.PermShareScreen, false), member(schema.PermUseVoice, false)),
			after: map[permissionCell]bool{
				{schema.PermissionRoleMember, schema.PermShareScreen}: false,
				{schema.PermissionRoleMember, schema.PermUseVoice}:    false,
			},
			detail: "revision=3;member.use_voice=true->false;member.share_screen=true->false",
		},
		{
			name: "stale revision", writer: "owner",
			req: func(rev int64) schema.PutPermissionsRequestV1 {
				return schema.PutPermissionsRequestV1{Revision: rev - 1, Changes: []schema.PermissionChangeV1{member(schema.PermUseVoice, true)}}
			},
			setup: []schema.PermissionChangeV1{member(schema.PermCreatePublicChannels, false)},
			want:  result{ErrPermissionsStale, ""},
		},
		{
			name: "revision from the future", writer: "owner",
			req: func(rev int64) schema.PutPermissionsRequestV1 {
				return schema.PutPermissionsRequestV1{Revision: rev + 1, Changes: []schema.PermissionChangeV1{member(schema.PermUseVoice, false)}}
			},
			want: result{ErrPermissionsStale, ""},
		},
		{
			name: "no-change write", writer: "owner",
			req:   changes(member(schema.PermUseVoice, true), operatorCell(schema.PermReadAuditLog, false)),
			after: map[permissionCell]bool{{schema.PermissionRoleMember, schema.PermUseVoice}: true},
		},
		{
			name: "preset personal from defaults", writer: "owner",
			req: preset(schema.PresetPersonal),
			after: map[permissionCell]bool{
				{schema.PermissionRoleMember, schema.PermCreatePublicChannels}:  false,
				{schema.PermissionRoleMember, schema.PermCreatePrivateChannels}: false,
				{schema.PermissionRoleMember, schema.PermMakeMemberLinks}:       false,
				{schema.PermissionRoleMember, schema.PermSeeMemberDirectory}:    false,
				{schema.PermissionRoleMember, schema.PermUseVoice}:              true,
				{schema.PermissionRoleMember, schema.PermShareScreen}:           true,
			},
			detail: "revision=2;preset=personal;member.create_public_channels=true->false;member.create_private_channels=true->false;" +
				"member.make_member_links=true->false;member.see_member_directory=true->false",
		},
		{
			name: "preset group from defaults changes nothing", writer: "owner",
			req: preset(schema.PresetGroup),
		},
		{
			name:   "preset group from a customised column",
			setup:  []schema.PermissionChangeV1{member(schema.PermSeeMemberDirectory, false), member(schema.PermShareScreen, false), operatorCell(schema.PermReadAuditLog, true)},
			writer: "owner",
			req:    preset(schema.PresetGroup),
			after: map[permissionCell]bool{
				{schema.PermissionRoleMember, schema.PermSeeMemberDirectory}: true,
				{schema.PermissionRoleMember, schema.PermShareScreen}:        true,
				{schema.PermissionRoleOperator, schema.PermReadAuditLog}:     true,
			},
			detail: "revision=3;preset=group;member.see_member_directory=false->true;member.share_screen=false->true",
		},
		{
			name:   "preset personal from a customised column",
			setup:  []schema.PermissionChangeV1{member(schema.PermShareScreen, false), member(schema.PermUseVoice, false), member(schema.PermMakeMemberLinks, false)},
			writer: "owner",
			req:    preset(schema.PresetPersonal),
			after: map[permissionCell]bool{
				{schema.PermissionRoleMember, schema.PermUseVoice}:        true,
				{schema.PermissionRoleMember, schema.PermShareScreen}:     true,
				{schema.PermissionRoleMember, schema.PermMakeMemberLinks}: false,
			},
			detail: "revision=3;preset=personal;member.create_public_channels=true->false;member.create_private_channels=true->false;" +
				"member.see_member_directory=true->false;member.use_voice=false->true;member.share_screen=false->true",
		},
		{
			name: "preset personal after the operators' voice was cut",
			setup: []schema.PermissionChangeV1{member(schema.PermShareScreen, false), member(schema.PermUseVoice, false),
				operatorCell(schema.PermShareScreen, false), operatorCell(schema.PermUseVoice, false)},
			writer: "owner",
			req:    preset(schema.PresetPersonal),
			want:   result{ErrPermissionsConflict, schema.PermUseVoice},
		},
		{
			name:   "preset group after the operators' links were cut",
			setup:  []schema.PermissionChangeV1{member(schema.PermMakeMemberLinks, false), operatorCell(schema.PermMakeMemberLinks, false)},
			writer: "owner",
			req:    preset(schema.PresetGroup),
			want:   result{ErrPermissionsConflict, schema.PermMakeMemberLinks},
		},
		{
			name:   "preset by an operator whose voice is braked",
			setup:  []schema.PermissionChangeV1{grantSettings, member(schema.PermShareScreen, false), member(schema.PermUseVoice, false)},
			writer: "operator-no-voice",
			req:    preset(schema.PresetPersonal),
			want:   result{ErrPermissionDenied, schema.PermUseVoice},
		},
		{
			name: "preset by an operator with change_server_settings", setup: []schema.PermissionChangeV1{grantSettings}, writer: "operator-1",
			req:   preset(schema.PresetPersonal),
			after: map[permissionCell]bool{{schema.PermissionRoleMember, schema.PermSeeMemberDirectory}: false},
			detail: "revision=3;preset=personal;member.create_public_channels=true->false;member.create_private_channels=true->false;" +
				"member.make_member_links=true->false;member.see_member_directory=true->false",
		},
		{
			name: "preset by an operator without change_server_settings", writer: "operator-1",
			req:  preset(schema.PresetPersonal),
			want: result{ErrPermissionDenied, schema.PermChangeServerSettings},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPermFixture(t)
			if len(tt.setup) > 0 {
				f.ownerSets(tt.setup...)
			}
			rev := f.revision()
			beforeCells := f.cells()
			_, beforeAudits := lastPermissionsChanged(t, f.s)
			beforeRows := countRows(t, f.s, "role_permissions")

			w, err := f.put(tt.writer, tt.req(rev))
			if tt.want.err != nil {
				var pe *PermissionError
				if !errors.Is(err, tt.want.err) || !errors.As(err, &pe) || pe.Permission != tt.want.perm {
					t.Fatalf("PutPermissions = %v (%+v); want %v naming %q", err, pe, tt.want.err, tt.want.perm)
				}
				if got := f.cells(); !reflect.DeepEqual(got, beforeCells) || f.revision() != rev {
					t.Errorf("a refused write changed the document: %v at %d, was %v at %d", got, f.revision(), beforeCells, rev)
				}
				if _, n := lastPermissionsChanged(t, f.s); n != beforeAudits {
					t.Errorf("a refused write audited")
				}
				if n := countRows(t, f.s, "role_permissions"); n != beforeRows {
					t.Errorf("a refused write wrote %d rows", n-beforeRows)
				}
				return
			}
			if err != nil {
				t.Fatalf("PutPermissions: %v", err)
			}
			got := f.cells()
			for c, v := range tt.after {
				if got[c] != v {
					t.Errorf("%s.%s = %v, want %v", c.role, c.perm, got[c], v)
				}
			}
			if !reflect.DeepEqual(got, w.Document.cellsOf()) {
				t.Errorf("the write's answer %v differs from a read %v", w.Document.cellsOf(), got)
			}
			last, n := lastPermissionsChanged(t, f.s)
			if tt.detail == "" {
				if n != beforeAudits || f.revision() != rev || len(w.Changes) != 0 || w.Document.Revision != rev ||
					countRows(t, f.s, "role_permissions") != beforeRows {
					t.Errorf("a no-change write: audits %d->%d, revision %d->%d, changes %v, rows %d->%d",
						beforeAudits, n, rev, f.revision(), w.Changes, beforeRows, countRows(t, f.s, "role_permissions"))
				}
				return
			}
			if n != beforeAudits+1 || last.Detail != tt.detail || last.Subject != "server" ||
				last.Actor != principalActor(f.ids[tt.writer]) {
				t.Errorf("audit = %d rows, last %+v; want one more with detail %q", n, last, tt.detail)
			}
			if f.revision() != rev+1 || w.Document.Revision != rev+1 {
				t.Errorf("revision %d -> %d (answer %d), want +1", rev, f.revision(), w.Document.Revision)
			}
			if w.Document.UpdatedAt.IsZero() {
				t.Errorf("updated_at is zero after a change")
			}
			for _, c := range w.Changes {
				if beforeCells[permissionCell{c.Role, c.Permission}] == c.Granted || got[permissionCell{c.Role, c.Permission}] != c.Granted {
					t.Errorf("change %+v is not a change from %v to %v", c, beforeCells, got)
				}
			}
		})
	}
}

func (d PermissionsDocument) cellsOf() map[permissionCell]bool {
	out := map[permissionCell]bool{}
	for _, r := range d.Rows {
		out[permissionCell{schema.PermissionRoleMember, r.Permission}] = r.Member
		out[permissionCell{schema.PermissionRoleOperator, r.Permission}] = r.Operator
	}
	return out
}

func changes(cs ...schema.PermissionChangeV1) func(int64) schema.PutPermissionsRequestV1 {
	return func(rev int64) schema.PutPermissionsRequestV1 {
		return schema.PutPermissionsRequestV1{Revision: rev, Changes: cs}
	}
}

func preset(p schema.PermissionPreset) func(int64) schema.PutPermissionsRequestV1 {
	return func(rev int64) schema.PutPermissionsRequestV1 {
		return schema.PutPermissionsRequestV1{Revision: rev, Preset: p}
	}
}

// A preset is checked exactly as the same cells sent by hand: from each
// starting column, by each writer, both answer alike and leave alike.
func TestPermissionPresetIsItsCells(t *testing.T) {
	starts := map[string][]schema.PermissionChangeV1{
		"defaults":         nil,
		"voice cut":        {member(schema.PermShareScreen, false), member(schema.PermUseVoice, false)},
		"operators' voice": {member(schema.PermShareScreen, false), member(schema.PermUseVoice, false), operatorCell(schema.PermShareScreen, false), operatorCell(schema.PermUseVoice, false)},
		"operators' links": {member(schema.PermMakeMemberLinks, false), operatorCell(schema.PermMakeMemberLinks, false)},
		"directory off":    {member(schema.PermSeeMemberDirectory, false)},
	}
	for name, start := range starts {
		for _, pr := range []schema.PermissionPreset{schema.PresetPersonal, schema.PresetGroup} {
			for _, writer := range []string{"owner", "operator-1", "operator-no-voice", "operator-no-settings"} {
				t.Run(fmt.Sprintf("%s/%s/%s", name, pr, writer), func(t *testing.T) {
					run := func(byHand bool) (map[permissionCell]bool, []PermissionCellChange, error) {
						f := newPermFixture(t)
						setup := append([]schema.PermissionChangeV1{}, start...)
						if writer != "operator-no-settings" {
							setup = append(setup, operatorCell(schema.PermChangeServerSettings, true))
						}
						if len(setup) > 0 {
							f.ownerSets(setup...)
						}
						rev := f.revision()
						req := schema.PutPermissionsRequestV1{Revision: rev, Preset: pr}
						if byHand {
							req.Preset = ""
							for _, p := range schema.Permissions {
								if _, fixed := designFixed[permissionCell{schema.PermissionRoleMember, p}]; fixed {
									continue
								}
								on := false
								for _, q := range designPresets[pr] {
									on = on || q == p
								}
								req.Changes = append(req.Changes, member(p, on))
							}
						}
						w, err := f.put(writer, req)
						return f.cells(), w.Changes, err
					}
					gotCells, gotChanges, gotErr := run(false)
					wantCells, wantChanges, wantErr := run(true)
					var ge, we *PermissionError
					errors.As(gotErr, &ge)
					errors.As(wantErr, &we)
					if (gotErr == nil) != (wantErr == nil) || (ge != nil && (we == nil || !errors.Is(ge.Err, we.Err) || ge.Permission != we.Permission)) {
						t.Fatalf("preset = %v, by hand = %v", gotErr, wantErr)
					}
					if !reflect.DeepEqual(gotCells, wantCells) || !reflect.DeepEqual(gotChanges, wantChanges) {
						t.Errorf("preset left %v (%v), by hand %v (%v)", gotCells, gotChanges, wantCells, wantChanges)
					}
				})
			}
		}
	}
}

// Two writers on one revision: the write lock orders them, and the second
// finds the revision moved on.
func TestPutPermissionsRace(t *testing.T) {
	f := newPermFixture(t)
	f.ownerSets(operatorCell(schema.PermChangeServerSettings, true))
	for round := range 10 {
		rev, cells := f.revision(), f.cells()
		// Each writer flips a cell of its own, so either write is a change
		// whichever runs first.
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, w := range []struct {
			who string
			c   schema.PermissionChangeV1
		}{
			{"owner", member(schema.PermCreatePublicChannels, !cells[permissionCell{schema.PermissionRoleMember, schema.PermCreatePublicChannels}])},
			{"operator-1", member(schema.PermCreatePrivateChannels, !cells[permissionCell{schema.PermissionRoleMember, schema.PermCreatePrivateChannels}])},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = f.put(w.who, schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{w.c}})
			}()
		}
		close(start)
		wg.Wait()
		won, stale := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrPermissionsStale):
				stale++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if won != 1 || stale != 1 || f.revision() != rev+1 {
			t.Fatalf("round %d: %d won, %d stale, revision %d -> %d; want one each and +1", round, won, stale, rev, f.revision())
		}
	}
}

// migrateTo builds a database at schema version pre, seeds it, and opens it
// with this build.
func migrateTo(t *testing.T, pre int, seed []string) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := range pre {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
		if step := migrationSteps[i+1]; step != nil {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := step(ctx, tx); err != nil {
				t.Fatalf("migration step %d: %v", i+1, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, stmt := range append(seed, fmt.Sprintf(`PRAGMA user_version = %d`, pre)) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	return s, path
}

// The permissions migration, from the schema before it: the table, the
// revision, one permissions_initialized row, and a hide_members setting
// mapped where one exists. Reopening is a no-op.
func TestPermissionsMigration(t *testing.T) {
	if len(migrations) < permissionsMigration {
		t.Fatalf("%d migrations, the permissions migration is %d", len(migrations), permissionsMigration)
	}
	base := []string{
		`INSERT INTO principals (id, kind, name, name_key, name_skel, role, owner, voice_allowed, may_invite, created_at) VALUES
			(1, 'human', 'owner', 'owner', 'owner', 'operator', 1, 1, 0, 1),
			(2, 'human', 'member-1', 'member-1', 'member-1', 'member', 0, 1, 1, 2)`,
	}
	for _, tt := range []struct {
		name      string
		hide      string // the hide_members row's value; "" for none
		directory bool   // member see_member_directory after
		detail    string
		rows      int
	}{
		{"no hide_members row", "", true, "revision=1;source=defaults", 0},
		{"hide_members on", "true", false, "revision=1;hide_members=true->member.see_member_directory=false", 1},
		{"hide_members off", "false", true, "revision=1;hide_members=false->member.see_member_directory=true", 1},
		{"hide_members not a boolean", "sometimes", false, "revision=1;hide_members=true->member.see_member_directory=false", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seed := append([]string{}, base...)
			if tt.hide != "" {
				seed = append(seed, fmt.Sprintf(`INSERT INTO server_settings (key, value, updated_at) VALUES ('hide_members', '%s', 3)`, tt.hide))
			}
			s, path := migrateTo(t, permissionsMigration-1, seed)
			check := func(pass int) {
				t.Helper()
				ctx := context.Background()
				var version int
				if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
					t.Errorf("pass %d: user_version = %d, %v; want %d", pass, version, err, len(migrations))
				}
				if n := countRows(t, s, "role_permissions"); n != tt.rows {
					t.Errorf("pass %d: %d role_permissions rows, want %d", pass, n, tt.rows)
				}
				var rev string
				if err := s.db.QueryRow("SELECT value FROM server_settings WHERE key = 'permissions_revision'").Scan(&rev); err != nil || rev != "1" {
					t.Errorf("pass %d: permissions_revision = %q, %v; want 1", pass, rev, err)
				}
				all, err := s.ListAuditEvents(ctx, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				// The community migration (40) writes its own row after this one.
				var evs []AuditEvent
				for _, e := range all {
					if e.Action != AuditCommunityMigrated {
						evs = append(evs, e)
					}
				}
				if len(evs) != 1 || evs[0].Action != AuditPermissionsInitialized || evs[0].Actor != "system" ||
					evs[0].Subject != "server" || evs[0].Detail != tt.detail {
					t.Errorf("pass %d: audit = %+v; want one permissions_initialized with %q", pass, evs, tt.detail)
				}
				doc, err := s.Permissions(ctx)
				if err != nil || doc.Revision != 1 || doc.Rows[3].Permission != schema.PermSeeMemberDirectory || doc.Rows[3].Member != tt.directory {
					t.Errorf("pass %d: document = %+v, %v; want revision 1, member directory %v", pass, doc, err, tt.directory)
				}
				if got, err := s.Permitted(ctx, 2, schema.PermSeeMemberDirectory); err != nil || got != tt.directory {
					t.Errorf("pass %d: member-1 sees the directory = %v, %v; want %v", pass, got, err, tt.directory)
				}
				// The old row stays, unread.
				if tt.hide != "" {
					var v string
					if err := s.db.QueryRow("SELECT value FROM server_settings WHERE key = 'hide_members'").Scan(&v); err != nil || v != tt.hide {
						t.Errorf("pass %d: hide_members = %q, %v; want it kept", pass, v, err)
					}
				}
			}
			check(1)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			if s, err = Open(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			check(2)
		})
	}
}

// A new database writes nothing: the defaults apply and the revision reads
// 1 (design §9).
func TestPermissionsMigrationNewDatabase(t *testing.T) {
	s := openFreshTestStore(t)
	if n := countRows(t, s, "audit_events"); n != 0 {
		t.Errorf("a new database has %d audit rows, want none", n)
	}
	if n := countRows(t, s, "server_settings"); n != 0 {
		t.Errorf("a new database has %d settings, want none", n)
	}
	doc, err := s.Permissions(context.Background())
	if err != nil || doc.Revision != 1 || doc.Preset != schema.PresetGroup || !doc.UpdatedAt.IsZero() {
		t.Errorf("a new database's document = %+v, %v; want revision 1, the group preset, never changed", doc, err)
	}
}

// A revision the store cannot read fails every read and write closed.
func TestPermissionsRevisionUnreadable(t *testing.T) {
	f := newPermFixture(t)
	if _, err := f.s.db.Exec(`INSERT INTO server_settings (key, value, updated_at) VALUES ('permissions_revision', 'two', 1)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Permissions(f.ctx); err == nil || !strings.Contains(err.Error(), "not a positive integer") {
		t.Errorf("Permissions = %v, want an error", err)
	}
	if _, err := f.put("owner", schema.PutPermissionsRequestV1{Revision: 1, Changes: []schema.PermissionChangeV1{member(schema.PermUseVoice, false)}}); err == nil {
		t.Errorf("PutPermissions over an unreadable revision succeeded")
	}
}
