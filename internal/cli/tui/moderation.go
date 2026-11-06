package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

type moderationAPI interface {
	KickVoiceParticipant(context.Context, string, int64) error
	RemoveChannelMember(context.Context, string, int64) error
	ListChannelBans(context.Context, string) (schema.ListChannelBansResponseV1, error)
	BanChannelMember(context.Context, string, int64) error
	UnbanChannelMember(context.Context, string, int64) error
	RemoveMessage(context.Context, string, int64) error
}

type moderationOp string

const (
	moderateVoice   moderationOp = "voice-kick"
	moderateMember  moderationOp = "member-remove"
	moderateBan     moderationOp = "ban"
	moderateUnban   moderationOp = "unban"
	moderateBans    moderationOp = "bans"
	moderateMessage moderationOp = "remove-message"
)

type moderationRequest struct {
	op      moderationOp
	channel string
	target  int64
}

type moderationModel struct {
	api     moderationAPI
	pending *moderationRequest
	writing bool
}

type moderationDone struct {
	request moderationRequest
	bans    schema.ListChannelBansResponseV1
	err     error
}

type messageRemovalReceived struct {
	gen     int
	channel string
	message schema.MessageV2
}

func newModerationModel(api API) moderationModel {
	m := moderationModel{}
	m.api, _ = api.(moderationAPI)
	return m
}

func (m Model) moderationAvailable() bool { return m.mod.api != nil }

func (m Model) moderationStanding() bool {
	if m.visit != nil || !m.moderationAvailable() {
		return false
	}
	if m.canManageChannels() {
		return true
	}
	member, ok := m.members[m.current()][m.authorID]
	return ok && member.Manages != nil && *member.Manages
}

func (m Model) moderationCommand(command, rest string) (Model, tea.Cmd) {
	if !m.moderationAvailable() {
		m.setStatus(modeChannels, "moderation is not available on this client")
		return m, nil
	}
	if m.mod.writing {
		m.setStatus(modeChannels, "a moderation request is still running…")
		return m, nil
	}
	fields := strings.Fields(rest)
	req := moderationRequest{op: moderationOp(command), channel: m.current()}
	if req.op == moderateBans {
		if len(fields) != 0 {
			m.setStatus(modeChannels, "usage: /bans")
			return m, nil
		}
		m.input = ""
		m.mod.writing = true
		m.setStatus(modeChannels, "loading bans…")
		return m, m.runModeration(req)
	}
	if req.op == moderateMessage && len(fields) == 0 {
		id, ok := m.reactionTarget()
		if !ok {
			m.setStatus(modeChannels, "no message selected")
			return m, nil
		}
		req.target = id
	} else {
		if len(fields) != 1 {
			m.setStatus(modeChannels, "usage: /"+command+" <id>; /remove-message also accepts no id for the bottom visible message")
			return m, nil
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || id <= 0 {
			m.setStatus(modeChannels, "moderation id must be a positive integer")
			return m, nil
		}
		req.target = id
	}
	m.input = ""
	m.mod.pending = &req
	m.setStatus(modeChannels, moderationQuestion(req)+" y to confirm; any other key to cancel")
	return m, nil
}

func moderationQuestion(req moderationRequest) string {
	channel := channelLabel(req.channel)
	switch req.op {
	case moderateVoice:
		return fmt.Sprintf("remove principal %d from voice in %s now; they can rejoin and chat access is unchanged;", req.target, channel)
	case moderateMember:
		return fmt.Sprintf("remove principal %d from %s and its nets; this does not ban them or revoke their account, and an authorized actor can re-add them;", req.target, channel)
	case moderateBan:
		return fmt.Sprintf("remove principal %d from %s and block this principal from being added again; other channels are unaffected; a new account or guest visit is not identified; if this is you, an operator must unban and re-add you;", req.target, channel)
	case moderateUnban:
		return fmt.Sprintf("lift the ban on principal %d in %s; this does not re-add them or restore any rights;", req.target, channel)
	case moderateMessage:
		return fmt.Sprintf("remove message %d in %s for everyone in its audience; future reads show a tombstone; server backups and copies already received cannot be recalled;", req.target, channel)
	}
	return "moderate " + channel + ";"
}

func (m Model) moderationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	req := *m.mod.pending
	m.mod.pending = nil
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if msg.String() != "y" {
		m.setStatus(modeChannels, "moderation canceled; nothing changed")
		return m, nil
	}
	m.mod.writing = true
	m.setStatus(modeChannels, "sending moderation request…")
	return m, m.runModeration(req)
}

func (m Model) runModeration(req moderationRequest) tea.Cmd {
	api := m.mod.api
	return func() tea.Msg {
		result := moderationDone{request: req}
		switch req.op {
		case moderateVoice:
			result.err = api.KickVoiceParticipant(m.ctx, req.channel, req.target)
		case moderateMember:
			result.err = api.RemoveChannelMember(m.ctx, req.channel, req.target)
		case moderateBan:
			result.err = api.BanChannelMember(m.ctx, req.channel, req.target)
		case moderateUnban:
			result.err = api.UnbanChannelMember(m.ctx, req.channel, req.target)
		case moderateBans:
			result.bans, result.err = api.ListChannelBans(m.ctx, req.channel)
		case moderateMessage:
			result.err = api.RemoveMessage(m.ctx, req.channel, req.target)
		}
		return result
	}
}

func (m Model) moderationFinished(msg moderationDone) Model {
	m.mod.writing = false
	req := msg.request
	if msg.err != nil {
		if req.channel == m.current() {
			if req.op == moderateVoice && cli.IsServerCode(msg.err, schema.ErrorCodeVoiceUnavailable) {
				m.setStatus(modeChannels, "voice removal incomplete; the participant may not have been removed; retry /voice-kick "+strconv.FormatInt(req.target, 10))
			} else {
				m.setStatus(modeChannels, msg.err.Error())
			}
		}
		return m
	}
	if req.op == moderateMessage {
		m.tombstoneHeld(req.channel, req.target)
	}
	if req.channel != m.current() {
		return m
	}
	channel := channelLabel(req.channel)
	switch req.op {
	case moderateVoice:
		m.setStatus(modeChannels, fmt.Sprintf("voice removal completed for principal %d in %s (they may already have been absent); they can rejoin; chat access is unchanged", req.target, channel))
	case moderateMember:
		m.setStatus(modeChannels, fmt.Sprintf("principal %d is removed or was already absent from %s and its nets; this is not a ban", req.target, channel))
	case moderateBan:
		m.setStatus(modeChannels, fmt.Sprintf("ban is in force for principal %d in %s (it may already have been); membership and nets removed", req.target, channel))
	case moderateUnban:
		m.setStatus(modeChannels, fmt.Sprintf("principal %d is unbanned or was already not banned in %s; membership and rights are not restored", req.target, channel))
	case moderateMessage:
		m.setStatus(modeChannels, fmt.Sprintf("message %d is removed or was already removed; received copies are not erased", req.target))
	case moderateBans:
		m.setStatus(modeChannels, banListStatus(req.channel, msg.bans))
	}
	return m
}

func banListStatus(channel string, list schema.ListChannelBansResponseV1) string {
	if len(list.Bans) == 0 {
		return "no bans in " + channelLabel(channel)
	}
	parts := make([]string, 0, len(list.Bans))
	for _, ban := range list.Bans {
		creator := "unknown"
		if ban.CreatedBy > 0 {
			creator = strconv.FormatInt(ban.CreatedBy, 10)
		}
		parts = append(parts, fmt.Sprintf("%d %s banned-by %s at %s", ban.PrincipalID, ban.Name, creator, ban.CreatedAt.Time().Format(time.RFC3339)))
	}
	return "bans in " + channelLabel(channel) + ": " + strings.Join(parts, "; ")
}

func (m *Model) tombstoneHeld(channel string, id int64) {
	for _, message := range m.messages[channel] {
		if message.ID != id {
			continue
		}
		message.Removed = true
		message.Body = schema.MessageRemovedBody
		message.Payload = nil
		message.EditedAt = nil
		m.addMessages(channel, []schema.MessageV2{message})
		return
	}
}

func (m Model) messageRemoval(msg messageRemovalReceived) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen || msg.channel != m.current() {
		return m, wait
	}
	for _, message := range m.messages[msg.channel] {
		if message.ID == msg.message.ID {
			m.addMessages(msg.channel, []schema.MessageV2{msg.message})
			break
		}
	}
	return m, wait
}
