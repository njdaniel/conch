package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/pkg/schema"
)

func withWho(m Model, who schema.WhoAmIResponseV1) Model {
	m.role, m.owner, m.permissions = who.Role, who.Owner, who.Permissions
	m.permsKnown = who.Permissions != nil || who.PermissionsRevision > 0
	return m
}

// The switch is offered by the permission, as the web's canDo decides it:
// the owner always; no permissions sent, an operator; else manage_members.
func TestCanManageMembersFollowsWhoami(t *testing.T) {
	useVoice := []schema.Permission{schema.PermUseVoice}
	tests := []struct {
		name string
		who  schema.WhoAmIResponseV1
		want bool
	}{
		{"owner", schema.WhoAmIResponseV1{Role: schema.RoleOperator, Owner: true, Permissions: useVoice, PermissionsRevision: 3}, true},
		{"operator on a server without permissions", schema.WhoAmIResponseV1{Role: schema.RoleOperator}, true},
		{"member on a server without permissions", schema.WhoAmIResponseV1{Role: schema.RoleMember}, false},
		{"operator without manage_members", schema.WhoAmIResponseV1{Role: schema.RoleOperator, Permissions: useVoice, PermissionsRevision: 2}, false},
		{"operator holding none", schema.WhoAmIResponseV1{Role: schema.RoleOperator, PermissionsRevision: 2}, false},
		{"member with manage_members", schema.WhoAmIResponseV1{Role: schema.RoleMember, Permissions: []schema.Permission{schema.PermManageMembers}, PermissionsRevision: 2}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &communityAPI{}
			m := withWho(operatorCommunity(t, api), tt.who)
			view := plain(m)
			if got := m.canManageMembers(); got != tt.want {
				t.Errorf("canManageMembers = %v, want %v", got, tt.want)
			}
			if shown := strings.Contains(view, "v voice"); shown != tt.want {
				t.Errorf("hint shown = %v, want %v:\n%s", shown, tt.want, view)
			}
			if shown := strings.Contains(view, "voice on"); shown != tt.want {
				t.Errorf("voice marks shown = %v, want %v", shown, tt.want)
			}
			before := len(api.got())
			pressK(m, rune1("v"))
			if sent := len(api.got()) > before; sent != tt.want {
				t.Errorf("v sent a request = %v, want %v", sent, tt.want)
			}
		})
	}
}

// A forbidden from the voice switch (owner protection, or a caller no longer
// allowed) re-reads whoami, so a switch that is no longer allowed stops being
// offered. Other refusals do not.
func TestForbiddenVoiceSwitchRereadsWhoami(t *testing.T) {
	api := &communityAPI{voiceErr: refusal(403, "forbidden"),
		who: schema.WhoAmIResponseV1{Role: schema.RoleOperator, Permissions: []schema.Permission{schema.PermUseVoice}, PermissionsRevision: 3}}
	m := operatorCommunity(t, api)
	m.authenticated = true
	if !m.canManageMembers() || !strings.Contains(plain(m), "v voice") {
		t.Fatal("the switch is not offered to begin with")
	}
	m = pressK(m, rune1("v"))
	calls := api.got()
	if calls[len(calls)-1] != "whoami" {
		t.Errorf("calls = %v, want whoami read after the refusal", calls)
	}
	if m.canManageMembers() || strings.Contains(plain(m), "v voice") {
		t.Errorf("the switch is still offered after whoami said it is not:\n%s", plain(m))
	}

	api = &communityAPI{voiceErr: refusal(409, schema.ErrorCodeNotAPerson)}
	m = operatorCommunity(t, api)
	m.authenticated = true
	pressK(m, rune1("v"))
	for _, c := range api.got() {
		if c == "whoami" {
			t.Errorf("a %s refusal re-read whoami", schema.ErrorCodeNotAPerson)
		}
	}
}

// A progress note stays while its request is in flight.
func TestProgressNotesSurviveKeysWhileBusy(t *testing.T) {
	m := operatorCommunity(t, &communityAPI{})
	m.community.busy, m.community.note = true, "switching voice…"
	m, _ = update(m, keyDown)
	if m.status() != "switching voice…" {
		t.Errorf("community status = %q, want the progress note kept", m.status())
	}
	d := directoryModel(t, &communityAPI{listing: []schema.ChannelListingV1{channelRow(1, "general", schema.ChannelPublic, true)}})
	d.dir.busy, d.dir.note = true, "joining…"
	d, _ = update(d, keyDown)
	if d.status() != "joining…" {
		t.Errorf("directory status = %q, want the progress note kept", d.status())
	}
}

func TestTitleBarNamesTheCommunityViews(t *testing.T) {
	m := communityModel(t, &communityAPI{}, schema.RoleMember)
	for _, tt := range []struct {
		key  tea.KeyMsg
		want string
	}{{keyCtrlD, "[directory]"}, {keyCtrlP, "[community]"}} {
		got := pressK(m, tt.key)
		if tabs, _ := got.viewTabs(); !strings.Contains(stripANSI(tabs), tt.want) {
			t.Errorf("tabs = %q, want %q", stripANSI(tabs), tt.want)
		}
	}
	if tabs, _ := m.viewTabs(); strings.Contains(tabs, "[directory]") || strings.Contains(tabs, "[community]") {
		t.Errorf("chat tabs name a community view: %q", tabs)
	}
}
