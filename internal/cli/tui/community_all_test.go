package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

func i64(n int64) *int64 { return &n }

// The every-channel view is offered by manage_channels, as canDo decides it,
// and only then: the hint, the help line and the key all follow it.
func TestAllChannelsViewIsOfferedByPermission(t *testing.T) {
	manage := []schema.Permission{schema.PermManageChannels}
	tests := []struct {
		name string
		who  schema.WhoAmIResponseV1
		want bool
	}{
		{"owner", schema.WhoAmIResponseV1{Role: schema.RoleOperator, Owner: true, Permissions: []schema.Permission{schema.PermUseVoice}, PermissionsRevision: 2}, true},
		{"operator on a server without permissions", schema.WhoAmIResponseV1{Role: schema.RoleOperator}, true},
		{"member on a server without permissions", schema.WhoAmIResponseV1{Role: schema.RoleMember}, false},
		{"operator without manage_channels", schema.WhoAmIResponseV1{Role: schema.RoleOperator, Permissions: []schema.Permission{schema.PermManageMembers}, PermissionsRevision: 2}, false},
		{"member with manage_channels", schema.WhoAmIResponseV1{Role: schema.RoleMember, Permissions: manage, PermissionsRevision: 2}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &communityAPI{listing: []schema.ChannelListingV1{channelRow(1, "general", schema.ChannelPublic, true)}}
			m := withWho(directoryModel(t, api), tt.who)
			m.width, m.height = 160, 48
			if shown := strings.Contains(plain(m), "a all"); shown != tt.want {
				t.Errorf("hint shown = %v, want %v:\n%s", shown, tt.want, plain(m))
			}
			m.help = true
			if shown := strings.Contains(plain(m), "every channel"); shown != tt.want {
				t.Errorf("help line shown = %v, want %v", shown, tt.want)
			}
			m.help = false
			before := len(api.got())
			m = pressK(m, rune1("a"))
			if sent := len(api.got()) > before; sent != tt.want {
				t.Errorf("a sent a request = %v, want %v", sent, tt.want)
			}
			if m.dir.all != tt.want {
				t.Errorf("all = %v, want %v", m.dir.all, tt.want)
			}
		})
	}
}

// Counts are shown only in the every-channel view, and switching back asks
// for the caller's own view again.
func TestAllChannelsViewShowsCountsOnlyThere(t *testing.T) {
	api := &communityAPI{listing: []schema.ChannelListingV1{
		{ID: 1, Name: "general", Visibility: schema.ChannelPublic, Joined: true, Members: i64(7)},
	}}
	m := withWho(directoryModel(t, api), schema.WhoAmIResponseV1{Role: schema.RoleOperator})
	m.width, m.height = 160, 48
	if strings.Contains(plain(m), "7 members") {
		t.Fatalf("own view shows a count:\n%s", plain(m))
	}
	m = pressK(m, rune1("a"))
	if !strings.Contains(plain(m), "7 members") || !strings.Contains(plain(m), "ALL CHANNELS") {
		t.Errorf("all view lacks the count:\n%s", plain(m))
	}
	m = pressK(m, rune1("a"))
	if strings.Contains(plain(m), "7 members") {
		t.Errorf("own view shows a count after switching back:\n%s", plain(m))
	}
	want := []string{"directory all=false", "directory all=true", "directory all=false"}
	if got := api.got(); !slices.Equal(got[len(got)-3:], want) {
		t.Errorf("calls = %v, want ...%v", got, want)
	}
}

// A refusal of the every-channel view is one line in the client's words, never
// the server's text, and the pane returns to the caller's own view.
func TestAllChannelsRefusalIsInTheClientsWords(t *testing.T) {
	for _, code := range []string{"forbidden", schema.ErrorCodePermissionDenied} {
		t.Run(code, func(t *testing.T) {
			api := &communityAPI{listing: []schema.ChannelListingV1{channelRow(1, "general", schema.ChannelPublic, true)}}
			m := withWho(directoryModel(t, api), schema.WhoAmIResponseV1{Role: schema.RoleOperator})
			m.width, m.height = 160, 48
			api.allErr = refusal(403, code)
			m = pressK(m, rune1("a"))
			if m.dir.all {
				t.Error("still in the all view after a refusal")
			}
			view := plain(m)
			want := cli.CommunityRefusal(cli.CommunityOpChannelsAll, refusal(403, code)).Error()
			if !strings.Contains(view, want[:min(len(want), 40)]) {
				t.Errorf("status lacks %q:\n%s", want, view)
			}
			if strings.Contains(view, "SERVERWORDS") {
				t.Errorf("the server's own text is shown:\n%s", view)
			}
		})
	}
}
