package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

type moderationStub struct {
	stubAPI
	calls       []moderationRequest
	err         error
	bans        schema.ListChannelBansResponseV1
	memberReads int
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
