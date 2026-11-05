package tui

import (
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

func whoWith(role schema.PrincipalRole, rev int64, perms ...schema.Permission) schema.WhoAmIResponseV1 {
	return schema.WhoAmIResponseV1{Role: role, Permissions: perms, PermissionsRevision: rev}
}

// The status line lists what whoami said the caller holds, and a server that
// predates the field shows nothing extra.
func TestStatusLineShowsPermissions(t *testing.T) {
	tests := []struct {
		name string
		who  schema.WhoAmIResponseV1
		want string // "" means no permissions text at all
	}{
		{"listed", whoWith(schema.RoleOperator, 3, schema.PermManageMembers, schema.PermUseVoice), "perms: manage_members, use_voice"},
		{"holding none", whoWith(schema.RoleOperator, 3), "perms: none"},
		{"server without the field", schema.WhoAmIResponseV1{Role: schema.RoleOperator}, ""},
		{"a name from the server is made safe", whoWith(schema.RoleMember, 3, schema.Permission("x\x1b[31my")), "perms: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := withWho(communityModel(t, &communityAPI{}, schema.RoleMember), tt.who)
			m.userName = "operator-1"
			m.width = 150
			view := plain(m)
			if tt.want == "" {
				if strings.Contains(view, "perms") {
					t.Errorf("view shows permissions for a server that sent none:\n%s", view)
				}
				return
			}
			if !strings.Contains(view, tt.want) {
				t.Errorf("view lacks %q:\n%s", tt.want, view)
			}
			if strings.Contains(m.View(), "\x1b[31my") {
				t.Errorf("a server's escape reached the screen")
			}
		})
	}
}

func TestStatusLineShowsPermissionsInOtherViews(t *testing.T) {
	m := withWho(operatorCommunity(t, &communityAPI{}), whoWith(schema.RoleOperator, 2, schema.PermManageMembers))
	m.userName = "operator-1"
	if view := plain(m); !strings.Contains(view, "perms: manage_members") {
		t.Errorf("community view lacks the permissions:\n%s", view)
	}
}

func TestHelpListsPermissions(t *testing.T) {
	tests := []struct {
		name string
		who  schema.WhoAmIResponseV1
		want bool
	}{
		{"known", whoWith(schema.RoleMember, 2, schema.PermUseVoice), true},
		{"absent", schema.WhoAmIResponseV1{Role: schema.RoleMember}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := withWho(communityModel(t, &communityAPI{}, schema.RoleMember), tt.who)
			joined := strings.Join(m.helpNotes(), "\n")
			if got := strings.Contains(joined, "Your permissions"); got != tt.want {
				t.Errorf("help lists permissions = %v, want %v", got, tt.want)
			}
		})
	}
}

func helpKeys(m Model) string {
	var out []string
	for _, e := range m.helpEntries() {
		out = append(out, e.keys+" "+e.does)
	}
	return strings.Join(out, "\n")
}

// Commands are offered by permission: manage_members offers the voice switch
// and manage_channels the every-channel directory view, in the hints and the
// help; a server without the field leaves it to the role.
func TestOfferedCommandsFollowPermissions(t *testing.T) {
	tests := []struct {
		name         string
		who          schema.WhoAmIResponseV1
		members, all bool
	}{
		{"member", whoWith(schema.RoleMember, 2, schema.PermUseVoice), false, false},
		{"manage_members only", whoWith(schema.RoleOperator, 2, schema.PermManageMembers), true, false},
		{"manage_channels only", whoWith(schema.RoleOperator, 2, schema.PermManageChannels), false, true},
		{"both", whoWith(schema.RoleOperator, 2, schema.PermManageMembers, schema.PermManageChannels), true, true},
		{"operator on an older server", schema.WhoAmIResponseV1{Role: schema.RoleOperator}, true, true},
		{"member on an older server", schema.WhoAmIResponseV1{Role: schema.RoleMember}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := withWho(operatorCommunity(t, &communityAPI{}), tt.who)
			if got := strings.Contains(plain(m), "v voice"); got != tt.members {
				t.Errorf("voice switch offered = %v, want %v", got, tt.members)
			}
			d := withWho(directoryModel(t, &communityAPI{}), tt.who)
			if got := strings.Contains(plain(d), "a all"); got != tt.all {
				t.Errorf("every-channel view offered = %v, want %v:\n%s", got, tt.all, plain(d))
			}
			if got := strings.Contains(helpKeys(d), "every channel"); got != tt.all {
				t.Errorf("help offers every-channel view = %v, want %v", got, tt.all)
			}
		})
	}
}

// A permissions_changed frame re-reads whoami; the status line and the
// offered commands follow the answer, both ways. A frame from an old socket
// changes nothing.
func TestPermissionsChangedRereadsWhoami(t *testing.T) {
	granted := whoWith(schema.RoleOperator, 5, schema.PermManageMembers, schema.PermManageChannels)
	revoked := whoWith(schema.RoleOperator, 6, schema.PermUseVoice)
	tests := []struct {
		name      string
		start     schema.WhoAmIResponseV1
		gen       int // added to the live socket's generation
		now       schema.WhoAmIResponseV1
		wantRead  bool
		wantPerms string
		wantVoice bool
	}{
		{"granted", revoked, 0, granted, true, "perms: manage_members, manage_channels", true},
		{"revoked", granted, 0, revoked, true, "perms: use_voice", false},
		{"an old socket's frame", granted, -1, revoked, false, "perms: manage_members, manage_channels", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &communityAPI{who: tt.now}
			m := withWho(operatorCommunity(t, api), tt.start)
			m.userName = "operator-1"
			m.width = 200
			m.ev.gen = 4
			m, cmd := update(m, eventsPermissionsReceived{gen: m.ev.gen + tt.gen})
			m = feed(m, cmd)
			read := false
			for _, c := range api.got() {
				read = read || c == "whoami"
			}
			if read != tt.wantRead {
				t.Fatalf("whoami read = %v, want %v (calls %v)", read, tt.wantRead, api.got())
			}
			view := plain(m)
			if !strings.Contains(view, tt.wantPerms) {
				t.Errorf("view lacks %q:\n%s", tt.wantPerms, view)
			}
			if got := strings.Contains(view, "v voice"); got != tt.wantVoice {
				t.Errorf("voice switch offered = %v, want %v", got, tt.wantVoice)
			}
		})
	}
}

func TestPermissionsRereadKeepsTheStatusAndSurvivesFailure(t *testing.T) {
	m := withWho(communityModel(t, &communityAPI{}, schema.RoleOperator), whoWith(schema.RoleOperator, 2, schema.PermManageChannels))
	m.setStatus(modeChannels, "sent")
	m, _ = update(m, whoAmILoaded{refresh: true, who: whoWith(schema.RoleMember, 3)})
	if m.channelStatus != "sent" {
		t.Errorf("status = %q, want the re-read to leave it", m.channelStatus)
	}
	m, _ = update(m, whoAmILoaded{refresh: true, err: refusal(500, "internal_error")})
	if m.whoErr != nil || m.permissionsSummary() != "perms: none" {
		t.Errorf("a failed re-read changed what the caller holds: %q, %v", m.permissionsSummary(), m.whoErr)
	}
}

// Losing manage_channels while looking at every channel puts the directory
// back on the caller's own.
func TestLosingManageChannelsLeavesTheEveryChannelView(t *testing.T) {
	m := withWho(directoryModel(t, &communityAPI{}), whoWith(schema.RoleOperator, 2, schema.PermManageChannels))
	m.dir.all = true
	m, _ = update(m, whoAmILoaded{refresh: true, who: whoWith(schema.RoleOperator, 3)})
	if m.dir.all {
		t.Error("the every-channel view is still on without manage_channels")
	}
}

// A refused command shows the server's refusal naming the permission, in the
// client's words (as the CLI does), never the server's text.
func TestRefusalNamesThePermission(t *testing.T) {
	denied := &cli.ServerError{Status: 403, Code: schema.ErrorCodePermissionDenied, Permission: string(schema.PermManageMembers), Message: "SERVERWORDS"}
	api := &communityAPI{voiceErr: denied}
	m := operatorCommunity(t, api)
	m = pressK(m, rune1("v"))
	want := "you do not have the manage_members permission, which this needs"
	if got := m.status(); got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	if strings.Contains(plain(m), "SERVERWORDS") {
		t.Error("the server's text reached the screen")
	}
}

// Many permissions never widen the status line past the terminal, and the
// key hints stay.
func TestStatusLineWithManyPermissionsFitsTheTerminal(t *testing.T) {
	for _, width := range []int{40, 60, 100, 160} {
		m := withWho(communityModel(t, &communityAPI{}, schema.RoleOperator), whoWith(schema.RoleOperator, 2,
			schema.PermManageMembers, schema.PermManageChannels, schema.PermUseVoice, schema.PermShareScreen, schema.PermActAsMembers))
		m.width = width
		m.userName = "operator-1"
		m.setStatus(modeChannels, "signed in as operator-1")
		lines := strings.Split(plain(m), "\n")
		last := lines[len(lines)-1]
		if w := cellWidth(last); w > width {
			t.Errorf("width %d: status line is %d cells: %q", width, w, last)
		}
		if !strings.Contains(last, "esc quit") && width >= 60 {
			t.Errorf("width %d: key hints lost: %q", width, last)
		}
	}
}

// The permissions take only the room the status message and the hints leave:
// a message the person has to read (the e2e P3-04 waits for one) is never cut
// for them, and a count stands in when the list does not fit.
func TestPermissionsNeverCutTheStatusMessage(t *testing.T) {
	tests := []struct {
		name      string
		width     int
		wantPerms string
		shown     bool
	}{
		{"a wide terminal lists them", 200, "perms: manage_members, manage_channels, use_voice", true},
		{"a middling one counts them", 125, "perms: 3", true},
		{"a narrow one shows none", 100, "perms", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := withWho(communityModel(t, &communityAPI{}, schema.RoleOperator), whoWith(schema.RoleOperator, 2,
				schema.PermManageMembers, schema.PermManageChannels, schema.PermUseVoice))
			m.width = tt.width
			m.userName = "operator-1"
			m.setStatus(modeChannels, "not logged in: run conch login")
			lines := strings.Split(plain(m), "\n")
			last := lines[len(lines)-1]
			if !strings.Contains(last, "not logged in: run conch login") {
				t.Errorf("the status message was cut: %q", last)
			}
			if got := strings.Contains(last, tt.wantPerms); got != tt.shown {
				t.Errorf("status line %q: has %q = %v, want %v", last, tt.wantPerms, got, tt.shown)
			}
		})
	}
}
