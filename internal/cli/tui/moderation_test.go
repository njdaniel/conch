package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

type moderationStub struct {
	stubAPI
	calls       []moderationRequest
	err         error
	bans        schema.ListChannelBansResponseV1
	memberReads int
	history     schema.ListMessagesResponseV2
	historyErr  error
	historyRead int
	// all, when set, is the channel ascending by id and is paged newest first.
	all []schema.MessageV2
}

func (a *moderationStub) ListMessagesV2Page(_ context.Context, _ string, page cli.MessagesPage, limit int) (schema.ListMessagesResponseV2, error) {
	a.historyRead++
	if a.all != nil {
		var out []schema.MessageV2
		for i := len(a.all) - 1; i >= 0 && len(out) < limit; i-- {
			if page.Before == 0 || a.all[i].ID < page.Before {
				out = append(out, a.all[i])
			}
		}
		return schema.ListMessagesResponseV2{Messages: out}, nil
	}
	return a.history, a.historyErr
}

func (a *moderationStub) KickVoiceParticipant(_ context.Context, channel string, id int64) error {
	a.calls = append(a.calls, moderationRequest{op: moderateVoice, channel: channel, target: id})
	return a.err
}
func (a *moderationStub) RemoveChannelMember(_ context.Context, channel string, id int64) error {
	a.calls = append(a.calls, moderationRequest{op: moderateMember, channel: channel, target: id})
	return a.err
}
func (a *moderationStub) ListChannelBans(_ context.Context, channel string) (schema.ListChannelBansResponseV1, error) {
	a.calls = append(a.calls, moderationRequest{op: moderateBans, channel: channel})
	return a.bans, a.err
}
func (a *moderationStub) BanChannelMember(_ context.Context, channel string, id int64) error {
	a.calls = append(a.calls, moderationRequest{op: moderateBan, channel: channel, target: id})
	return a.err
}
func (a *moderationStub) UnbanChannelMember(_ context.Context, channel string, id int64) error {
	a.calls = append(a.calls, moderationRequest{op: moderateUnban, channel: channel, target: id})
	return a.err
}
func (a *moderationStub) RemoveMessage(_ context.Context, channel string, id int64) error {
	a.calls = append(a.calls, moderationRequest{op: moderateMessage, channel: channel, target: id})
	return a.err
}
func (a *moderationStub) ListChannelMembers(context.Context, string) (schema.ListChannelMembersResponseV1, error) {
	a.memberReads++
	return schema.ListChannelMembersResponseV1{Members: []schema.ChannelMemberV1{}}, nil
}

func moderationModelForTest() (Model, *moderationStub) {
	api := &moderationStub{bans: schema.ListChannelBansResponseV1{Bans: []schema.ChannelBanV1{}}}
	m := NewModel(context.Background(), api, 7, []string{"general", "ops"})
	m.width, m.height = 100, 30
	m.role, m.owner = schema.RoleOperator, true
	m.messages["general"] = []schema.MessageV2{{ID: 41, AuthorID: 8, Body: "original"}}
	m.loaded["general"] = true
	m.members["general"] = map[int64]schema.ChannelMemberV1{7: {PrincipalID: 7, Name: "manager"}}
	return m, api
}

func TestModerationCommandsConfirmAndCall(t *testing.T) {
	tests := []struct {
		name, command, scope, done string
		op                         moderationOp
		target                     int64
	}{
		{"voice", "/voice-kick 9", "they can rejoin and chat access is unchanged", "voice removal completed", moderateVoice, 9},
		{"member", "/member-remove 9", "does not ban them or revoke their account", "removed or was already absent", moderateMember, 9},
		{"ban", "/ban 9", "a new account or guest visit is not identified", "ban is in force", moderateBan, 9},
		{"unban", "/unban 9", "does not re-add them or restore any rights", "membership and rights are not restored", moderateUnban, 9},
		{"message", "/remove-message 41", "copies already received cannot be recalled", "received copies are not erased", moderateMessage, 41},
		{"selected message", "/remove-message", "remove message 41", "removed or was already removed", moderateMessage, 41},
	}
	for _, tt := range tests {
		for _, confirm := range []bool{false, true} {
			t.Run(tt.name+"/"+strconvBool(confirm), func(t *testing.T) {
				m, api := moderationModelForTest()
				m, cmd := submitText(t, m, tt.command)
				if cmd != nil || m.mod.pending == nil || len(api.calls) != 0 || !strings.Contains(m.status(), tt.scope) {
					t.Fatalf("pending=%v cmd=%v calls=%v status=%q", m.mod.pending, cmd != nil, api.calls, m.status())
				}
				key := rune1("n")
				if confirm {
					key = rune1("y")
				}
				m, cmd = update(m, key)
				if !confirm {
					if cmd != nil || len(api.calls) != 0 || !strings.Contains(m.status(), "canceled") {
						t.Fatalf("cancel sent a request: cmd=%v calls=%v status=%q", cmd != nil, api.calls, m.status())
					}
					return
				}
				if cmd == nil {
					t.Fatal("confirmation produced no request")
				}
				result := cmd()
				if len(api.calls) != 1 || api.calls[0].op != tt.op || api.calls[0].channel != "general" || api.calls[0].target != tt.target {
					t.Fatalf("calls = %+v", api.calls)
				}
				m, _ = update(m, result)
				if m.mod.writing || !strings.Contains(m.status(), tt.done) {
					t.Fatalf("writing=%v status=%q", m.mod.writing, m.status())
				}
			})
		}
	}
}

func strconvBool(v bool) string {
	if v {
		return "confirmed"
	}
	return "canceled"
}

func TestModerationBansList(t *testing.T) {
	stamp := schema.NewTimestamp(time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC))
	tests := []struct {
		name string
		bans []schema.ChannelBanV1
		want string
	}{
		{"empty", []schema.ChannelBanV1{}, "no bans in #general"},
		{"escaped", []schema.ChannelBanV1{{PrincipalID: 9, Name: "bad\x1b[31m\u202e", CreatedBy: 7, CreatedAt: stamp}}, "9 bad[31m banned-by 7 at 2026-11-06T12:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, api := moderationModelForTest()
			api.bans.Bans = tt.bans
			m, cmd := submitText(t, m, "/bans")
			if cmd == nil || m.mod.pending != nil {
				t.Fatalf("list required confirmation or made no request")
			}
			m, _ = update(m, cmd())
			if len(api.calls) != 1 || api.calls[0].op != moderateBans || !strings.Contains(m.status(), tt.want) {
				t.Fatalf("calls=%+v status=%q", api.calls, m.status())
			}
			if strings.Contains(m.status(), "\x1b") || strings.Contains(m.status(), "\u202e") {
				t.Fatalf("unsafe status %q", m.status())
			}
		})
	}
}

func TestModerationVoiceUnavailableOffersRetry(t *testing.T) {
	m, api := moderationModelForTest()
	api.err = &cli.ServerError{Status: 503, Code: schema.ErrorCodeVoiceUnavailable, Message: "SERVERWORDS"}
	m, _ = submitText(t, m, "/voice-kick 9")
	m, cmd := update(m, rune1("y"))
	m, _ = update(m, cmd())
	if !strings.Contains(m.status(), "incomplete") || !strings.Contains(m.status(), "retry /voice-kick 9") || strings.Contains(m.status(), "completed") || strings.Contains(m.status(), "SERVERWORDS") {
		t.Fatalf("status = %q", m.status())
	}
}

func TestModerationHelpRequiresStanding(t *testing.T) {
	tests := []struct {
		name  string
		prep  func(*Model)
		offer bool
	}{
		{"owner", func(m *Model) {}, true},
		{"permitted operator", func(m *Model) {
			m.owner = false
			m.permissions = []schema.Permission{schema.PermManageChannels}
			m.permsKnown = true
		}, true},
		{"channel manager", func(m *Model) {
			m.role = schema.RoleMember
			m.owner = false
			member := m.members["general"][7]
			member.Manages = on(true)
			m.members["general"][7] = member
		}, true},
		{"plain member", func(m *Model) { m.role = schema.RoleMember; m.owner = false }, false},
		{"guest", func(m *Model) { m.visit = &schema.GuestV1{} }, false},
		{"operator without permission", func(m *Model) { m.owner = false; m.permissions = []schema.Permission{}; m.permsKnown = true }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := moderationModelForTest()
			tt.prep(&m)
			var help strings.Builder
			for _, entry := range m.helpEntries() {
				help.WriteString(entry.keys + " " + entry.does + "\n")
			}
			if got := strings.Contains(help.String(), "/voice-kick"); got != tt.offer {
				t.Fatalf("moderation help offered=%v, want %v:\n%s", got, tt.offer, help.String())
			}
		})
	}
	t.Run("no client API", func(t *testing.T) {
		m := NewModel(context.Background(), stubAPI{}, 7, []string{"general"})
		m.role, m.owner = schema.RoleOperator, true
		for _, entry := range m.helpEntries() {
			if strings.Contains(entry.keys, "voice-kick") {
				t.Fatal("moderation offered without the client API")
			}
		}
	})
}

func TestModerationDirectRefusalIsShown(t *testing.T) {
	m, api := moderationModelForTest()
	m.role, m.owner = schema.RoleMember, false
	api.err = errors.New("denied\x1b[31m\u202e")
	m, _ = submitText(t, m, "/ban 9")
	m, cmd := update(m, rune1("y"))
	m, _ = update(m, cmd())
	if len(api.calls) != 1 || !strings.Contains(m.status(), "denied") || strings.Contains(m.status(), "\x1b") || strings.Contains(m.status(), "\u202e") {
		t.Fatalf("calls=%v status=%q", api.calls, m.status())
	}
}

func TestModerationMessageSuccessTombstonesHeldCopy(t *testing.T) {
	m, _ := moderationModelForTest()
	edited := schema.NewTimestamp(time.Now())
	m.messages["general"][0].Payload = &schema.Payload{}
	m.messages["general"][0].EditedAt = &edited
	m, _ = submitText(t, m, "/remove-message 41")
	m, cmd := update(m, rune1("y"))
	m, _ = update(m, cmd())
	got := m.messages["general"][0]
	if !got.Removed || got.Body != schema.MessageRemovedBody || got.Payload != nil || got.EditedAt != nil || len(m.messages["general"]) != 1 {
		t.Fatalf("message = %+v", got)
	}
	if view := m.View(); !strings.Contains(view, schema.MessageRemovedBody) || strings.Contains(view, "original") {
		t.Fatalf("rendered tombstone is unsafe or missing:\n%s", view)
	}
}

func TestTombstonesUpsertWithoutArrivalOrResurrection(t *testing.T) {
	m, _ := moderationModelForTest()
	m.authorID = 7
	m.mode = modeAgents
	m.rs.unread["general"] = 2
	removed := m.messages["general"][0]
	removed.Removed = true
	removed.Body = schema.MessageRemovedBody
	m, _ = update(m, messageReceived{channel: "general", message: removed})
	if len(m.messages["general"]) != 1 || !m.messages["general"][0].Removed || m.rs.unread["general"] != 2 || m.streamed["general"] != 41 || len(m.react.pending["general"]) != 0 {
		t.Fatalf("messages=%+v unread=%d streamed=%d pending=%v", m.messages["general"], m.rs.unread["general"], m.streamed["general"], m.react.pending["general"])
	}
	original := removed
	original.Removed = false
	original.Body = "original"
	m, _ = update(m, messageReceived{channel: "general", message: original})
	if !m.messages["general"][0].Removed || m.messages["general"][0].Body != schema.MessageRemovedBody || m.rs.unread["general"] != 2 {
		t.Fatalf("stale original resurrected or notified: %+v, unread %d", m.messages["general"][0], m.rs.unread["general"])
	}
}

func TestRemovalEventUpdatesOnlyHeldWithoutCursorOrUnread(t *testing.T) {
	m, _ := moderationModelForTest()
	m.ev.gen = 3
	m.streamed["general"] = 50
	m.rs.unread["general"] = 2
	removed := m.messages["general"][0]
	removed.Removed = true
	removed.Body = schema.MessageRemovedBody
	m, _ = update(m, messageRemovalReceived{gen: 3, channel: "general", message: removed})
	if !m.messages["general"][0].Removed || m.streamed["general"] != 50 || m.rs.unread["general"] != 2 {
		t.Fatalf("message=%+v streamed=%d unread=%d", m.messages["general"][0], m.streamed["general"], m.rs.unread["general"])
	}
	unknown := removed
	unknown.ID = 99
	m, _ = update(m, messageRemovalReceived{gen: 3, channel: "general", message: unknown})
	if len(m.messages["general"]) != 1 {
		t.Fatalf("unknown removal added state: %+v", m.messages["general"])
	}
	m, _ = update(m, messageRemovalReceived{gen: 2, channel: "general", message: originalMessage(41)})
	if !m.messages["general"][0].Removed {
		t.Fatal("stale event replaced the tombstone")
	}
}

func originalMessage(id int64) schema.MessageV2 {
	return schema.MessageV2{ID: id, Body: "original"}
}

func TestMemberEventReplacesStateAndCancelsStandingConfirmation(t *testing.T) {
	m, _ := moderationModelForTest()
	m.role, m.owner = schema.RoleMember, false
	member := m.members["general"][7]
	member.Manages = on(true)
	m.members["general"][7] = member
	m.ev.gen = 4
	m, _ = submitText(t, m, "/ban 9")
	if m.mod.pending == nil {
		t.Fatal("manager was not offered confirmation")
	}
	m, _ = update(m, membersEventReceived{gen: 4, channel: "general", members: []schema.ChannelMemberV1{{PrincipalID: 8, Name: "other"}}})
	if m.mod.pending != nil || len(m.members["general"]) != 1 || m.members["general"][8].Name != "other" {
		t.Fatalf("pending=%v members=%+v", m.mod.pending, m.members["general"])
	}
	m, _ = update(m, membersEventReceived{gen: 3, channel: "general", members: []schema.ChannelMemberV1{{PrincipalID: 9, Name: "stale"}}})
	if len(m.members["general"]) != 1 || m.members["general"][8].Name != "other" {
		t.Fatalf("stale roster applied: %+v", m.members["general"])
	}
}

func TestMemberReadCannotOverwriteNewerEvent(t *testing.T) {
	m, _ := moderationModelForTest()
	m.ev.gen = 4
	read := m.loadMembers()().(membersLoaded)
	if read.revision != 0 {
		t.Fatalf("read revision = %d", read.revision)
	}
	m, _ = update(m, membersEventReceived{gen: 4, channel: "general", members: []schema.ChannelMemberV1{{PrincipalID: 8, Name: "new"}}})
	m, _ = update(m, read)
	if len(m.members["general"]) != 1 || m.members["general"][8].Name != "new" {
		t.Fatalf("stale read rolled back the event: %+v", m.members["general"])
	}
	m, _ = update(m, membersLoaded{channel: "general", revision: 1, members: []schema.ChannelMemberV1{{PrincipalID: 9, Name: "current read"}}})
	if len(m.members["general"]) != 1 || m.members["general"][9].Name != "current read" {
		t.Fatalf("current read did not apply: %+v", m.members["general"])
	}
}

func TestEventsReconnectReloadsMembers(t *testing.T) {
	m, api := moderationModelForTest()
	m.ev.gen, m.ev.state = 2, eventsDown
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.ctx = ctx
	_, cmd := m.eventsRoster(eventsRosterReceived{gen: 2})
	found := false
	for _, msg := range collect(cmd) {
		if _, ok := msg.(membersLoaded); ok {
			found = true
		}
	}
	if !found || api.memberReads != 1 {
		t.Fatalf("member reload found=%v reads=%d", found, api.memberReads)
	}
}

func removedCopy(id int64) schema.MessageV2 {
	return schema.MessageV2{ID: id, Removed: true, Body: schema.MessageRemovedBody}
}

func TestRemovalRememberedBeforeOriginalArrives(t *testing.T) {
	tests := []struct {
		name  string
		later func(Model) (Model, tea.Cmd)
	}{
		{"history read before removal lands after it", func(m Model) (Model, tea.Cmd) {
			return update(m, messagesLoaded{channel: "general", messages: []schema.MessageV2{originalMessage(42)}})
		}},
		{"stream original after removal", func(m Model) (Model, tea.Cmd) {
			return update(m, messageReceived{channel: "general", message: originalMessage(42)})
		}},
		{"reconcile read", func(m Model) (Model, tea.Cmd) {
			return update(m, messagesReconciled{channel: "general", done: true, messages: []schema.MessageV2{originalMessage(42)}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := moderationModelForTest()
			m.mode = modeAgents
			m.rs.unread["general"] = 2
			m, _ = update(m, messageRemovalReceived{gen: m.ev.gen, channel: "general", message: removedCopy(42)})
			if len(m.messages["general"]) != 1 {
				t.Fatalf("removal of an unheld id added state: %+v", m.messages["general"])
			}
			m, _ = tt.later(m)
			var got *schema.MessageV2
			for i := range m.messages["general"] {
				if m.messages["general"][i].ID == 42 {
					got = &m.messages["general"][i]
				}
			}
			if got == nil || !got.Removed || got.Body != schema.MessageRemovedBody {
				t.Fatalf("original shown after its removal: %+v", got)
			}
			if m.rs.unread["general"] != 2 {
				t.Fatalf("a removed original counted as an arrival: unread %d", m.rs.unread["general"])
			}
		})
	}
}

func TestRemovedIDsAreBoundedPerChannel(t *testing.T) {
	m, _ := moderationModelForTest()
	for id := int64(1); id <= maxRemembered+10; id++ {
		m.mod.remember("general", id)
	}
	if n := len(m.mod.removed["general"]); n != maxRemembered {
		t.Fatalf("remembered %d ids, want %d", n, maxRemembered)
	}
	if m.mod.knows("general", 1) || !m.mod.knows("general", maxRemembered+10) || m.mod.knows("ops", maxRemembered+10) {
		t.Fatal("eviction or per-channel scoping wrong")
	}
}

func TestReconnectReconcilesHeldMessages(t *testing.T) {
	tests := []struct {
		name      string
		history   []schema.MessageV2
		err       error
		wantBody  string
		wantStale bool
	}{
		{"removal while disconnected shows tombstone", []schema.MessageV2{removedCopy(41)}, nil, schema.MessageRemovedBody, false},
		{"unchanged message stays", []schema.MessageV2{originalMessage(41)}, nil, "original", false},
		{"failed read marks the view stale", nil, errors.New("down"), "original", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, api := moderationModelForTest()
			api.history = schema.ListMessagesResponseV2{Messages: tt.history}
			api.historyErr = tt.err
			var delays []time.Duration
			m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
				delays = append(delays, d)
				return func() tea.Msg { return msg }
			}
			m.streamed["general"] = 50
			m.ev.gen, m.ev.state = 2, eventsDown
			m, cmd := update(m, eventsRosterReceived{gen: 2})
			for _, msg := range collect(cmd) {
				if _, ok := msg.(messagesReconciled); ok {
					m, _ = update(m, msg)
				}
			}
			if api.historyRead != 1 {
				t.Fatalf("history reads = %d, want 1", api.historyRead)
			}
			if got := m.messages["general"][0].Body; got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
			if m.streamed["general"] != 50 {
				t.Fatalf("reconcile moved the stream cursor to %d", m.streamed["general"])
			}
			if m.mod.stale["general"] != tt.wantStale {
				t.Fatalf("stale = %v, want %v", m.mod.stale["general"], tt.wantStale)
			}
			if tt.wantStale {
				if !strings.Contains(m.View(), "stale") || len(delays) != 1 || delays[0] != minResubscribeDelay {
					t.Fatalf("stale not shown or no backoff retry: delays=%v", delays)
				}
				// The retry doubles its wait and, once the read works, clears the flag.
				m, _ = update(m, messagesReconciled{channel: "general", err: errors.New("down")})
				if len(delays) != 2 || delays[1] != 2*minResubscribeDelay {
					t.Fatalf("backoff did not grow: %v", delays)
				}
				m, _ = update(m, messagesReconciled{channel: "general", done: true, messages: []schema.MessageV2{removedCopy(41)}})
				if m.mod.stale["general"] || m.messages["general"][0].Body != schema.MessageRemovedBody || strings.Contains(m.View(), "[stale") {
					t.Fatalf("recovery left stale state: %+v", m.messages["general"])
				}
			}
		})
	}
}

func TestConfirmationPanelShowsFullConsequenceAtNarrowWidths(t *testing.T) {
	tests := []struct {
		command string
		want    []string
	}{
		{"/ban 9", []string{"an operator must unban and re-add you", "y to confirm", "any other key to cancel"}},
		{"/remove-message 41", []string{"cannot be recalled", "y to confirm", "any other key to cancel"}},
	}
	for _, tt := range tests {
		for _, width := range []int{80, 100} {
			t.Run(fmt.Sprintf("%s/%d", tt.command, width), func(t *testing.T) {
				m, _ := moderationModelForTest()
				m.width, m.height = width, 30
				m, _ = submitText(t, m, tt.command)
				view := m.View()
				flat := strings.Join(strings.Fields(view), " ")
				for _, want := range tt.want {
					if !strings.Contains(flat, want) {
						t.Errorf("confirmation lacks %q at %d columns:\n%s", want, width, view)
					}
				}
				lines := strings.Split(view, "\n")
				if len(lines) != 30 {
					t.Errorf("view has %d rows, want 30", len(lines))
				}
				for _, line := range lines {
					if w := cellWidth(line); w > width {
						t.Errorf("row of %d cells at width %d: %q", w, width, line)
					}
				}
			})
		}
	}
}

func TestReconcilePagesPastTheBatchCap(t *testing.T) {
	m, api := moderationModelForTest()
	total := int64(maxReconcilePages*historyLimit + 300)
	for id := int64(1); id <= total; id++ {
		message := originalMessage(id)
		if id == 1 {
			message = removedCopy(1)
		}
		api.all = append(api.all, message)
	}
	m.messages["general"] = []schema.MessageV2{originalMessage(1), originalMessage(total)}
	m.ev.gen, m.ev.state = 2, eventsDown
	m, cmd := update(m, eventsRosterReceived{gen: 2})
	sawStaleMidway := false
	for rounds := 0; cmd != nil && rounds < 20; rounds++ {
		var next []tea.Cmd
		for _, msg := range collect(cmd) {
			if _, ok := msg.(messagesReconciled); !ok {
				continue
			}
			var c tea.Cmd
			m, c = update(m, msg)
			next = append(next, c)
		}
		if m.mod.stale["general"] {
			sawStaleMidway = true
		}
		cmd = tea.Batch(next...)
	}
	if !sawStaleMidway {
		t.Error("view was never stale while the window was only partly covered")
	}
	if m.messages["general"][0].ID != 1 || !m.messages["general"][0].Removed {
		t.Fatalf("oldest held message not reconciled: %+v", m.messages["general"][0])
	}
	if m.mod.stale["general"] || strings.Contains(m.View(), "[stale") {
		t.Fatal("still stale after the window was covered")
	}
	if api.historyRead <= maxReconcilePages {
		t.Fatalf("only %d pages read; did not go past the cap", api.historyRead)
	}
}

func TestConfirmationFitsShortTerminalsAndLongNames(t *testing.T) {
	long := strings.Repeat("字", 100)
	tests := []struct {
		name          string
		width, height int
		channel       string
	}{
		{"40x10", 40, 10, "general"},
		{"40x14", 40, 14, "general"},
		{"60x12", 60, 12, "general"},
		{"80x10", 80, 10, "general"},
		{"80x24 long name", 80, 24, long},
		{"40x10 long name", 40, 10, long},
		{"120x30 long name", 120, 30, long},
	}
	for _, tt := range tests {
		for _, command := range []string{"/ban 9", "/remove-message 41"} {
			t.Run(tt.name+command, func(t *testing.T) {
				m, _ := moderationModelForTest()
				m.channels = []string{tt.channel}
				m.selected = 0
				m.messages[tt.channel] = m.messages["general"]
				m.width, m.height = tt.width, tt.height
				m, _ = submitText(t, m, command)
				if m.mod.pending == nil {
					t.Fatal("no confirmation pending")
				}
				view := m.View()
				lines := strings.Split(view, "\n")
				if len(lines) != tt.height {
					t.Errorf("rows = %d, want %d", len(lines), tt.height)
				}
				for _, line := range lines {
					if w := cellWidth(line); w > tt.width {
						t.Errorf("row of %d cells at width %d: %q", w, tt.width, line)
					}
				}
				flat := strings.Join(strings.Fields(view), " ")
				target := "principal 9"
				short := "ban 9"
				if command != "/ban 9" {
					target, short = "message 41", "remove-message 41"
				}
				if strings.Contains(flat, "y to confirm") && !strings.Contains(flat, target) && !strings.Contains(flat, short) {
					t.Errorf("keys shown without the target:\n%s", view)
				}
				if !strings.Contains(flat, "y to confirm") {
					t.Errorf("keys missing:\n%s", view)
				}
				if !strings.Contains(flat, target) && !strings.Contains(flat, short) {
					t.Errorf("target missing:\n%s", view)
				}
			})
		}
	}
}
