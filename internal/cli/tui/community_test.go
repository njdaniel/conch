package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// communityAPI is a fake server for the directory and the member view. It
// records the calls the TUI makes and answers each with what the test set.
type communityAPI struct {
	stubAPI
	mu sync.Mutex

	listing  []schema.ChannelListingV1
	listErr  error
	joinErr  error
	leaveErr error
	makeErr  error
	voiceErr error
	// pages are the pages of the member list, by the cursor that asks for them.
	pages    map[int64]schema.ServerMembersResponseV1
	pageErr  error
	voiceNow map[int64]bool

	calls []string
}

func (a *communityAPI) record(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, fmt.Sprintf(format, args...))
}

func (a *communityAPI) got() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *communityAPI) ChannelDirectory(_ context.Context, all bool) (schema.ChannelDirectoryResponseV1, error) {
	a.record("directory all=%v", all)
	return schema.ChannelDirectoryResponseV1{Channels: a.listing}, a.listErr
}

func (a *communityAPI) JoinChannel(_ context.Context, channel string) (schema.JoinChannelResponseV1, error) {
	a.record("join %s", channel)
	if a.joinErr != nil {
		return schema.JoinChannelResponseV1{}, a.joinErr
	}
	return schema.JoinChannelResponseV1{Channel: schema.ChannelV0{Name: channel}}, nil
}

func (a *communityAPI) LeaveChannel(_ context.Context, channel string) error {
	a.record("leave %s", channel)
	return a.leaveErr
}

func (a *communityAPI) CreateChannelV1(_ context.Context, r schema.CreateChannelRequestV1) (schema.CreateChannelResponseV1, error) {
	a.record("create %s %s", r.Name, r.Visibility)
	if a.makeErr != nil {
		return schema.CreateChannelResponseV1{}, a.makeErr
	}
	return schema.CreateChannelResponseV1{Channel: schema.ChannelV0{Name: r.Name}}, nil
}

func (a *communityAPI) DirectoryMembers(_ context.Context, after int64, limit int) (schema.ServerMembersResponseV1, error) {
	a.record("members after=%d limit=%d", after, limit)
	return a.pages[after], a.pageErr
}

func (a *communityAPI) SetVoicePermission(_ context.Context, id int64, allowed bool) (schema.VoicePermissionResponseV1, error) {
	a.record("voice %d %v", id, allowed)
	if a.voiceErr != nil {
		return schema.VoicePermissionResponseV1{}, a.voiceErr
	}
	prev := a.voiceNow[id]
	return schema.VoicePermissionResponseV1{PrincipalID: id, VoiceAllowed: allowed, Changed: prev != allowed}, nil
}

// refusal is a server refusal as the client decodes it; message is the
// server's own text, which must never reach the screen.
func refusal(status int, code string) error {
	return &cli.ServerError{Status: status, Code: code, Message: "\x1b[31mSERVERWORDS #secret-room\x1b[0m"}
}

func quiet(time.Duration, tea.Msg) tea.Cmd { return nil }

func communityModel(t *testing.T, api *communityAPI, role schema.PrincipalRole) Model {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := NewModel(ctx, api, 7, []string{"general", "ops"})
	m.width, m.height = 100, 30
	m.after = quiet
	m.role = role
	return m
}

func pressK(m Model, keys ...tea.KeyMsg) Model {
	for _, k := range keys {
		var cmd tea.Cmd
		m, cmd = update(m, k)
		m = feed(m, cmd)
	}
	return m
}

func typed(m Model, text string) Model {
	return pressK(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}

var (
	keyCtrlD = tea.KeyMsg{Type: tea.KeyCtrlD}
	keyCtrlP = tea.KeyMsg{Type: tea.KeyCtrlP}
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
	keyTab   = tea.KeyMsg{Type: tea.KeyTab}
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
)

func rune1(r string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r)} }

func channelRow(id int64, name string, vis schema.ChannelVisibility, joined bool) schema.ChannelListingV1 {
	return schema.ChannelListingV1{ID: id, Name: name, Visibility: vis, Joined: joined}
}

func directoryModel(t *testing.T, api *communityAPI) Model {
	t.Helper()
	m := communityModel(t, api, schema.RoleMember)
	m = pressK(m, keyCtrlD)
	if m.mode != modeDirectory {
		t.Fatalf("mode = %v after ctrl+d, want the directory", m.mode)
	}
	return m
}

func plain(m Model) string { return stripANSI(m.View()) }

func TestDirectoryListsChannelsWithTheirMarks(t *testing.T) {
	count := int64(41)
	general := channelRow(1, "general", schema.ChannelPublic, true)
	general.Default = true
	secret := channelRow(3, "ops-private", schema.ChannelPrivate, true)
	secret.Manages = true
	secret.Members = &count // a count the member view must not show
	api := &communityAPI{listing: []schema.ChannelListingV1{
		general,
		channelRow(2, "random", schema.ChannelPublic, false),
		secret,
		channelRow(4, "evil\x1b]0;pwned\x07name", schema.ChannelPublic, false),
	}}
	m := directoryModel(t, api)

	if got := api.got(); len(got) != 1 || got[0] != "directory all=false" {
		t.Fatalf("calls = %v, want one read of the member's directory", got)
	}
	view := plain(m)
	for _, want := range []string{
		"CHANNEL DIRECTORY", "#general [joined default]", "#random", "#ops-private [joined private managed]",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the directory lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "41") || strings.Contains(view, "\x1b]") || strings.Contains(view, "\x07") {
		t.Errorf("the directory shows a count or a raw escape:\n%q", view)
	}
	if strings.Contains(view, "[joined]") && strings.Contains(view, "#random [") {
		t.Errorf("an unjoined public channel carries marks:\n%s", view)
	}
}

func TestDirectoryNeedsTheClientCalls(t *testing.T) {
	m := communityModel(t, &communityAPI{}, schema.RoleMember)
	m.comm = nil
	m = pressK(m, keyCtrlD)
	if m.mode != modeChannels || m.status() != statusNoCommunity {
		t.Errorf("mode %v, status %q: want the chat, saying the client has no directory", m.mode, m.status())
	}
}

func TestDirectoryIsReachedByCommandAndLeftWithEsc(t *testing.T) {
	api := &communityAPI{listing: []schema.ChannelListingV1{channelRow(1, "general", schema.ChannelPublic, true)}}
	m := communityModel(t, api, schema.RoleMember)
	m.input = "/channels"
	m = pressK(m, keyEnter)
	if m.mode != modeDirectory || m.input != "" {
		t.Fatalf("mode %v, input %q after /channels", m.mode, m.input)
	}
	m = pressK(m, keyEsc)
	if m.mode != modeChannels {
		t.Errorf("esc left the mode as %v, want the chat (esc is not quit here)", m.mode)
	}
	m = pressK(m, keyCtrlD, keyTab)
	if m.mode != modeChannels {
		t.Errorf("tab left the mode as %v, want the chat", m.mode)
	}
}

func TestDirectoryJoin(t *testing.T) {
	tests := []struct {
		name     string
		joinErr  error
		onRow    int
		wantCall string
		want     string
		channels []string
	}{
		{name: "joins a public channel", onRow: 1, wantCall: "join random", want: "joined #random", channels: []string{"general", "ops", "random"}},
		{name: "already in it asks nothing", onRow: 0, want: "already in #general", channels: []string{"general", "ops"}},
		{name: "not joinable", onRow: 1, joinErr: refusal(403, schema.ErrorCodeChannelNotJoinable), wantCall: "join random",
			want: "you cannot join this channel: only a person in the community can, and not one removed from it", channels: []string{"general", "ops"}},
		{name: "no such channel", onRow: 1, joinErr: refusal(404, "channel_not_found"), wantCall: "join random",
			want: "no such channel, or you cannot see it", channels: []string{"general", "ops"}},
		{name: "agent refused", onRow: 1, joinErr: refusal(403, schema.ErrorCodeHumanRequired), wantCall: "join random",
			want: "only a person may do this, not an agent", channels: []string{"general", "ops"}},
		{name: "unknown code", onRow: 1, joinErr: refusal(418, "teapot"), wantCall: "join random",
			want: "the server refused this (HTTP 418)", channels: []string{"general", "ops"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &communityAPI{joinErr: tt.joinErr, listing: []schema.ChannelListingV1{
				channelRow(1, "general", schema.ChannelPublic, true), channelRow(2, "random", schema.ChannelPublic, false)}}
			m := directoryModel(t, api)
			for i := 0; i < tt.onRow; i++ {
				m = pressK(m, keyDown)
			}
			m = pressK(m, keyEnter)
			calls := api.got()[1:] // the first is the read on entry
			if tt.wantCall == "" && len(calls) != 0 {
				t.Errorf("calls = %v, want none", calls)
			}
			if tt.wantCall != "" && (len(calls) < 1 || calls[0] != tt.wantCall) {
				t.Errorf("calls = %v, want %q first", calls, tt.wantCall)
			}
			if got := m.status(); got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
			if strings.Join(m.channels, ",") != strings.Join(tt.channels, ",") {
				t.Errorf("chat channels = %v, want %v", m.channels, tt.channels)
			}
			if strings.Contains(plain(m), "SERVERWORDS") || strings.Contains(plain(m), "secret-room") {
				t.Errorf("the server's own text reached the screen:\n%s", plain(m))
			}
		})
	}
}

func TestDirectoryLeave(t *testing.T) {
	pub := channelRow(1, "general", schema.ChannelPublic, true)
	priv := channelRow(3, "ops-private", schema.ChannelPrivate, true)
	out := channelRow(2, "random", schema.ChannelPublic, false)
	tests := []struct {
		name     string
		keys     []tea.KeyMsg
		onRow    int
		leaveErr error
		want     string
		leaves   []string
	}{
		{name: "public channel leaves at once", keys: []tea.KeyMsg{rune1("l")}, onRow: 0, want: "left #general", leaves: []string{"leave general"}},
		{name: "not in it", keys: []tea.KeyMsg{rune1("l")}, onRow: 1, want: "not in #random"},
		{name: "private asks first", keys: []tea.KeyMsg{rune1("l")}, onRow: 2,
			want: "leave private #ops-private? you cannot join it again yourself: y to leave, any other key to stay"},
		{name: "private left on y", keys: []tea.KeyMsg{rune1("l"), rune1("y")}, onRow: 2, want: "left #ops-private", leaves: []string{"leave ops-private"}},
		{name: "l again does not confirm", keys: []tea.KeyMsg{rune1("l"), rune1("l")}, onRow: 2,
			want: "leave private #ops-private? you cannot join it again yourself: y to leave, any other key to stay"},
		{name: "private stays on any other key", keys: []tea.KeyMsg{rune1("l"), rune1("n"), rune1("y")}, onRow: 2, want: "channel directory (3)"},
		{name: "last member of a private channel", keys: []tea.KeyMsg{rune1("l"), rune1("y")}, onRow: 2,
			leaveErr: refusal(409, schema.ErrorCodeChannelLastMember), leaves: []string{"leave ops-private"},
			want: "you are the last member of this private channel: someone else must be added before you can leave"},
		{name: "no such channel", keys: []tea.KeyMsg{rune1("l")}, onRow: 0, leaveErr: refusal(404, "channel_not_found"),
			leaves: []string{"leave general"}, want: "no such channel, or you cannot see it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &communityAPI{leaveErr: tt.leaveErr, listing: []schema.ChannelListingV1{pub, out, priv}}
			m := directoryModel(t, api)
			for i := 0; i < tt.onRow; i++ {
				m = pressK(m, keyDown)
			}
			m = pressK(m, tt.keys...)
			var leaves []string
			for _, c := range api.got() {
				if strings.HasPrefix(c, "leave ") {
					leaves = append(leaves, c)
				}
			}
			if strings.Join(leaves, ";") != strings.Join(tt.leaves, ";") {
				t.Errorf("leave calls = %v, want %v", leaves, tt.leaves)
			}
			if got := m.status(); got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLeavingTheChannelOnScreenOpensANeighbour(t *testing.T) {
	api := &communityAPI{listing: []schema.ChannelListingV1{channelRow(1, "general", schema.ChannelPublic, true)}}
	m := communityModel(t, api, schema.RoleMember)
	m.selected = 1 // ops
	m = pressK(m, keyCtrlD)
	m.dir.sel = 1
	m, cmd := update(m, rune1("l"))
	m = feed(m, cmd)
	// Leaving "general" while "ops" is on screen keeps "ops" on screen.
	if m.current() != "general" && m.current() != "ops" {
		t.Fatalf("current = %q", m.current())
	}
	if strings.Join(m.channels, ",") != "ops" || m.current() != "ops" {
		t.Errorf("channels = %v, current %q, want ops alone and on screen", m.channels, m.current())
	}
	// Leaving the one on screen moves to what is left; leaving the last reads the list again.
	api.listing = []schema.ChannelListingV1{channelRow(2, "ops", schema.ChannelPublic, true)}
	m = pressK(m, rune1("r"))
	m.dir.sel = 2
	m, _ = update(m, rune1("l"))
	m, _ = update(m, channelActed{op: cli.CommunityOpLeave, channel: "ops"})
	if !m.loadingChannels || len(m.channels) != 0 {
		t.Errorf("channels = %v, loading %v, want the list read again after the last one", m.channels, m.loadingChannels)
	}
}

func TestCreateChannel(t *testing.T) {
	collision := "there is already a channel with that name, perhaps one you are not in; choose another name"
	tests := []struct {
		name     string
		keys     func(Model) Model
		makeErr  error
		want     string
		calls    []string
		channels string
	}{
		{name: "public by default", keys: func(m Model) Model { return pressK(typed(pressK(m, rune1("n")), "plans"), keyEnter) },
			want: "created #plans", calls: []string{"create plans public"}, channels: "general,ops,plans"},
		{name: "private with tab", keys: func(m Model) Model { return pressK(typed(pressK(m, rune1("n"), keyTab), "plans"), keyEnter) },
			want: "created #plans", calls: []string{"create plans private"}, channels: "general,ops,plans"},
		{name: "tab twice is public again", keys: func(m Model) Model { return pressK(typed(pressK(m, rune1("n"), keyTab, keyTab), "plans"), keyEnter) },
			want: "created #plans", calls: []string{"create plans public"}, channels: "general,ops,plans"},
		{name: "a collision is one line", makeErr: refusal(409, "channel_exists"), want: collision,
			keys:  func(m Model) Model { return pressK(typed(pressK(m, rune1("n")), "secret"), keyEnter) },
			calls: []string{"create secret public"}, channels: "general,ops"},
		{name: "a bad name costs no request", want: "that name will not do: schema: channel name must not be empty",
			keys:  func(m Model) Model { return pressK(typed(pressK(m, rune1("n")), "   "), keyEnter) },
			calls: nil, channels: "general,ops"},
		{name: "rate limited", makeErr: &cli.ServerError{Status: 429, Code: "rate_limited", RetryAfterSeconds: 90},
			keys:  func(m Model) Model { return pressK(typed(pressK(m, rune1("n")), "plans"), keyEnter) },
			want:  "too many tries: wait 90s and try again",
			calls: []string{"create plans public"}, channels: "general,ops"},
		{name: "esc cancels", keys: func(m Model) Model { return pressK(typed(pressK(m, rune1("n")), "plans"), keyEsc) },
			want: "channel directory (0)", calls: nil, channels: "general,ops"},
		{name: "backspace edits the name", keys: func(m Model) Model {
			return pressK(typed(pressK(m, rune1("n")), "planz"), tea.KeyMsg{Type: tea.KeyBackspace}, rune1("s"), keyEnter)
		}, want: "created #plans", calls: []string{"create plans public"}, channels: "general,ops,plans"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &communityAPI{makeErr: tt.makeErr}
			m := directoryModel(t, api)
			m = tt.keys(m)
			var made []string
			for _, c := range api.got() {
				if strings.HasPrefix(c, "create ") {
					made = append(made, c)
				}
			}
			if strings.Join(made, ";") != strings.Join(tt.calls, ";") {
				t.Errorf("create calls = %v, want %v", made, tt.calls)
			}
			if got := m.status(); got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
			if got := strings.Join(m.channels, ","); got != tt.channels {
				t.Errorf("chat channels = %q, want %q", got, tt.channels)
			}
			if strings.Contains(plain(m), "SERVERWORDS") {
				t.Errorf("the server's own text reached the screen:\n%s", plain(m))
			}
		})
	}
}

func TestCreatePromptShowsTheVisibility(t *testing.T) {
	m := directoryModel(t, &communityAPI{})
	m = typed(pressK(m, rune1("n")), "plans")
	if view := plain(m); !strings.Contains(view, "new public channel") || !strings.Contains(view, "plans") {
		t.Errorf("the prompt does not say public:\n%s", view)
	}
	m = pressK(m, keyTab)
	if view := plain(m); !strings.Contains(view, "new private channel") {
		t.Errorf("tab did not choose private:\n%s", view)
	}
	// Keys typed in the prompt are text: "?" does not open the help, "q" does not quit.
	m = typed(m, "?q")
	if m.help || m.dir.name != "plans?q" {
		t.Errorf("help %v, name %q: the prompt must take every character", m.help, m.dir.name)
	}
}

func member(id int64, kind schema.PrincipalKind, name string, role schema.PrincipalRole) schema.ServerMemberV1 {
	return schema.ServerMemberV1{ID: id, Kind: kind, Name: name, Role: role, CreatedAt: schema.NewTimestamp(testNow)}
}

func on(b bool) *bool { return &b }

func communityOf(t *testing.T, api *communityAPI, role schema.PrincipalRole) Model {
	t.Helper()
	m := communityModel(t, api, role)
	m = pressK(m, keyCtrlP)
	if m.mode != modeCommunity {
		t.Fatalf("mode = %v after ctrl+p, want the community", m.mode)
	}
	return m
}

func TestCommunityListsPeopleWithAgentsMarked(t *testing.T) {
	api := &communityAPI{pages: map[int64]schema.ServerMembersResponseV1{
		0: {Members: []schema.ServerMemberV1{
			member(1, schema.PrincipalHuman, "alice", schema.RoleOperator),
			member(2, schema.PrincipalHuman, "bob", schema.RoleMember),
			member(3, schema.PrincipalAgent, "builder", ""),
			member(4, schema.PrincipalHuman, "esc\x1b[2Japed", schema.RoleMember),
		}},
	}}
	m := communityOf(t, api, schema.RoleMember)
	view := plain(m)
	for _, want := range []string{"COMMUNITY", "alice [operator]", "bob [member]", "builder [agent]"} {
		if !strings.Contains(view, want) {
			t.Errorf("the community lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "voice") || strings.Contains(view, "guest") || strings.Contains(view, "\x1b") {
		t.Errorf("the member view shows voice, a guest or a raw escape:\n%q", view)
	}
	if got := m.status(); got != "community (4)" {
		t.Errorf("status = %q", got)
	}
}

func TestCommunityFollowsEveryPage(t *testing.T) {
	api := &communityAPI{pages: map[int64]schema.ServerMembersResponseV1{
		0: {Members: []schema.ServerMemberV1{member(1, schema.PrincipalHuman, "alice", schema.RoleMember)}, After: 1},
		1: {Members: []schema.ServerMemberV1{member(2, schema.PrincipalHuman, "bob", schema.RoleMember)}},
	}}
	m := communityOf(t, api, schema.RoleMember)
	want := []string{
		fmt.Sprintf("members after=0 limit=%d", schema.MaxListPrincipalsLimit),
		fmt.Sprintf("members after=1 limit=%d", schema.MaxListPrincipalsLimit),
	}
	if got := api.got(); strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if view := plain(m); !strings.Contains(view, "alice") || !strings.Contains(view, "bob") {
		t.Errorf("a page is missing:\n%s", view)
	}
	// A cursor that does not move would read for ever.
	api = &communityAPI{pages: map[int64]schema.ServerMembersResponseV1{
		0: {Members: []schema.ServerMemberV1{member(1, schema.PrincipalHuman, "alice", schema.RoleMember)}, After: 1},
		1: {Members: []schema.ServerMemberV1{member(1, schema.PrincipalHuman, "alice", schema.RoleMember)}, After: 1},
	}}
	m = communityOf(t, api, schema.RoleMember)
	if got := m.status(); !strings.Contains(got, "pages do not end") {
		t.Errorf("status = %q, want the loop refused", got)
	}
}

func TestCommunityReadRefused(t *testing.T) {
	api := &communityAPI{pageErr: refusal(403, schema.ErrorCodeHumanRequired)}
	m := communityOf(t, api, schema.RoleMember)
	if got, want := m.status(), "only a person may do this, not an agent"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

func operatorCommunity(t *testing.T, api *communityAPI) Model {
	t.Helper()
	api.pages = map[int64]schema.ServerMembersResponseV1{0: {Members: []schema.ServerMemberV1{
		func() schema.ServerMemberV1 {
			a := member(1, schema.PrincipalHuman, "alice", schema.RoleOperator)
			a.Owner, a.VoiceAllowed = true, on(true)
			return a
		}(),
		func() schema.ServerMemberV1 {
			b := member(2, schema.PrincipalHuman, "bob", schema.RoleMember)
			b.VoiceAllowed = on(true)
			return b
		}(),
		member(3, schema.PrincipalAgent, "builder", ""),
	}}}
	api.voiceNow = map[int64]bool{1: true, 2: true}
	return communityOf(t, api, schema.RoleOperator)
}

func TestOperatorSeesAndFlipsVoice(t *testing.T) {
	api := &communityAPI{}
	m := operatorCommunity(t, api)
	view := plain(m)
	for _, want := range []string{"alice [operator · owner · voice on]", "bob [member · voice on]", "builder [agent]", "v voice"} {
		if !strings.Contains(view, want) {
			t.Errorf("the operator's view lacks %q:\n%s", want, view)
		}
	}
	m = pressK(m, keyDown, rune1("v"))
	if got := api.got(); got[len(got)-1] != "voice 2 false" {
		t.Errorf("calls = %v, want voice switched off for 2", got)
	}
	if got, want := m.status(), "voice is now off for bob"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	if view := plain(m); !strings.Contains(view, "bob [member · voice off]") {
		t.Errorf("the row did not follow:\n%s", view)
	}
	api.voiceNow[2] = false
	m = pressK(m, rune1("v"))
	if got := api.got(); got[len(got)-1] != "voice 2 true" {
		t.Errorf("calls = %v, want voice back on", got)
	}
	if got, want := m.status(), "voice is now on for bob"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

func TestVoiceSwitchRefusals(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"reauthentication", refusal(403, schema.ErrorCodeReauthRequired),
			"this needs your password first: run 'conch reauth' in another terminal (a script credential cannot; sign in with 'conch login --password'), then try again"},
		{"owner protection", refusal(403, "forbidden"), "only an owner may change an owner's voice"},
		{"an agent", refusal(409, schema.ErrorCodeNotAPerson), "agents do not use voice: only a person has a voice switch"},
		{"gone", refusal(404, "principal_not_found"), "no such person"},
		{"no permission", &cli.ServerError{Status: 403, Code: schema.ErrorCodePermissionDenied, Permission: "manage_members\x1b[31m"},
			"you do not have the permission this needs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &communityAPI{voiceErr: tt.err}
			m := operatorCommunity(t, api)
			m = pressK(m, rune1("v"))
			if got := m.status(); got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
			if view := plain(m); strings.Contains(view, "SERVERWORDS") || !strings.Contains(view, "alice [operator · owner · voice on]") {
				t.Errorf("the screen changed or shows the server's text:\n%s", view)
			}
		})
	}
}

func TestVoiceSwitchIsForOperatorsOnAPersonsRow(t *testing.T) {
	// A member never sees the switch, even if the server sent the field.
	api := &communityAPI{}
	m := operatorCommunity(t, api)
	m.role = schema.RoleMember
	if view := plain(m); strings.Contains(view, "voice on") || strings.Contains(view, "v voice") {
		t.Errorf("a member sees the voice switch:\n%s", view)
	}
	before := len(api.got())
	m = pressK(m, rune1("v"))
	if len(api.got()) != before || m.status() != "only an operator can switch a person's voice" {
		t.Errorf("a member's v sent a request or said %q", m.status())
	}
	// An operator's v on an agent sends nothing.
	m = operatorCommunity(t, api)
	m = pressK(m, keyDown, keyDown)
	before = len(api.got())
	m = pressK(m, rune1("v"))
	if len(api.got()) != before || m.status() != "agents do not use voice: only a person has a voice switch" {
		t.Errorf("v on an agent sent a request or said %q", m.status())
	}
}

func TestHelpListsTheCommunityKeys(t *testing.T) {
	tests := []struct {
		mode  mode
		creat bool
		want  []string
	}{
		{modeChannels, false, []string{"/channels, ctrl+d", "/people, ctrl+p"}},
		{modeDirectory, false, []string{"join the channel", "leave it", "create a channel, public or private", "back to Chat"}},
		{modeDirectory, true, []string{"choose public or private", "create it"}},
		{modeCommunity, false, []string{"turn the person's voice on or off", "read the community again"}},
	}
	for _, tt := range tests {
		m := communityModel(t, &communityAPI{}, schema.RoleOperator)
		m.width, m.height = 160, 48
		m.mode, m.help = tt.mode, true
		m.dir.creating = tt.creat
		view := plain(m)
		for _, w := range tt.want {
			if !strings.Contains(view, w) {
				t.Errorf("mode %d creating %v: the help lacks %q:\n%s", tt.mode, tt.creat, w, view)
			}
		}
	}
	// ? opens the help in the lists and is text at the create prompt.
	m := directoryModel(t, &communityAPI{})
	if m = pressK(m, rune1("?")); !m.help {
		t.Error("? did not open the help in the directory")
	}
}

func TestCommunityViewsSurviveEveryTerminalSize(t *testing.T) {
	api := &communityAPI{listing: []schema.ChannelListingV1{channelRow(1, "general", schema.ChannelPublic, true)}}
	for _, size := range [][2]int{{160, 48}, {80, 24}, {60, 18}, {40, 10}} {
		m := operatorCommunity(t, api)
		m.width, m.height = size[0], size[1]
		for _, key := range []tea.KeyMsg{keyEsc, keyCtrlD} {
			view := plain(m)
			if lines := strings.Count(view, "\n") + 1; lines != size[1] {
				t.Errorf("%dx%d mode %d: %d lines, want the screen exactly:\n%s", size[0], size[1], m.mode, lines, view)
			}
			m = pressK(m, key)
		}
	}
}

func TestCtrlCQuitsFromTheCommunityViews(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyCtrlD, keyCtrlP} {
		m := pressK(communityModel(t, &communityAPI{}, schema.RoleMember), key)
		_, cmd := update(m, tea.KeyMsg{Type: tea.KeyCtrlC})
		if cmd == nil {
			t.Fatal("ctrl+c returned no command")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("ctrl+c in mode %v did not quit", m.mode)
		}
	}
}

func TestRefusalsAreOneLine(t *testing.T) {
	err := cli.CommunityRefusal(cli.CommunityOpJoin, errors.New("network down"))
	if err.Error() != "network down" {
		t.Errorf("a transport error was reworded: %q", err)
	}
	for _, op := range []string{cli.CommunityOpChannels, cli.CommunityOpJoin, cli.CommunityOpLeave, cli.CommunityOpCreate, cli.CommunityOpMembers, cli.CommunityOpVoice} {
		line := cli.CommunityRefusal(op, refusal(403, "no_such_code")).Error()
		if strings.ContainsAny(line, "\n\x1b") || strings.Contains(line, "SERVERWORDS") {
			t.Errorf("%s: refusal %q is not one clean line", op, line)
		}
	}
}
