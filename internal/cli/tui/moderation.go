package tui

import (
	"context"
	"fmt"
	"slices"
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
	// removed is, per channel, the ids known to be removed (bounded, oldest
	// forgotten first); removedOrder is the insertion order for eviction.
	removed      map[string]map[int64]bool
	removedOrder map[string][]int64
	// stale marks channels whose held messages could not be reconciled after a
	// reconnect; reconcileBackoff is the wait before the next attempt.
	stale            map[string]bool
	reconcileBackoff map[string]time.Duration
}

// maxRemembered bounds the removed ids kept per channel. Known limit: 512
// removals arriving while one history response is in flight would evict an id
// that response still carries; accepted as unrealistic.
const maxRemembered = 512

// remember records that id was removed in channel, so a copy of the original
// that arrives later (an older history read, a stream replay) shows the
// tombstone. The oldest ids are forgotten past maxRemembered.
func (m *moderationModel) remember(channel string, id int64) {
	if m.removed[channel][id] {
		return
	}
	if m.removed[channel] == nil {
		m.removed[channel] = make(map[int64]bool)
	}
	m.removed[channel][id] = true
	m.removedOrder[channel] = append(m.removedOrder[channel], id)
	for len(m.removedOrder[channel]) > maxRemembered {
		delete(m.removed[channel], m.removedOrder[channel][0])
		m.removedOrder[channel] = m.removedOrder[channel][1:]
	}
}

func (m moderationModel) knows(channel string, id int64) bool { return m.removed[channel][id] }

// tombstoned is message as a removal leaves it.
func tombstoned(message schema.MessageV2) schema.MessageV2 {
	message.Removed = true
	message.Body = schema.MessageRemovedBody
	message.Payload = nil
	message.EditedAt = nil
	return message
}

// withoutRemoved replaces every message in incoming whose id is known removed
// by its tombstone. It copies the slice only when something changes.
func (m moderationModel) withoutRemoved(channel string, incoming []schema.MessageV2) []schema.MessageV2 {
	var out []schema.MessageV2
	for i, message := range incoming {
		if message.Removed || !m.knows(channel, message.ID) {
			continue
		}
		if out == nil {
			out = slices.Clone(incoming)
		}
		out[i] = tombstoned(message)
	}
	if out == nil {
		return incoming
	}
	return out
}

type messagesReconciled struct {
	channel  string
	messages []schema.MessageV2
	err      error
	// before is where the next read continues (0 is the newest), oldest the
	// lowest id the window must reach, and done says it has.
	before, oldest int64
	done           bool
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
	m := moderationModel{removed: map[string]map[int64]bool{}, removedOrder: map[string][]int64{}, stale: map[string]bool{}, reconcileBackoff: map[string]time.Duration{}}
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
	m.setStatus(modeChannels, moderationQuestion(req)+" "+confirmKeys)
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
		m.mod.remember(req.channel, req.target)
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
		m.addMessages(channel, []schema.MessageV2{tombstoned(message)})
		return
	}
}

func (m Model) messageRemoval(msg messageRemovalReceived) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen {
		return m, wait
	}
	// Remembered even when the message is not held: a history read that began
	// before the removal may still be on its way with the original.
	m.mod.remember(msg.channel, msg.message.ID)
	if msg.channel != m.current() {
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

// reconcileMessages re-reads the held window of channel, newest first, from
// the cursor before (0 is the newest) down to the id oldest, so a removal made
// while the sockets were down (which neither replays) reaches messages below
// the stream cursor. One call reads at most maxReconcilePages pages; the
// result says where to continue.
func (m Model) reconcileMessages(channel string, before, oldest int64) tea.Cmd {
	if channel == "" || m.api == nil {
		return nil
	}
	if oldest == 0 {
		for _, message := range m.messages[channel] {
			if oldest == 0 || message.ID < oldest {
				oldest = message.ID
			}
		}
	}
	if oldest == 0 {
		return nil
	}
	return func() tea.Msg {
		result := messagesReconciled{channel: channel, oldest: oldest, before: before}
		for range maxReconcilePages {
			page, err := m.api.ListMessagesV2Page(m.ctx, channel, cli.MessagesPage{NewestFirst: true, Before: result.before}, historyLimit)
			if err != nil {
				result.err = err
				return result
			}
			result.messages = append(result.messages, page.Messages...)
			if len(page.Messages) < historyLimit {
				result.done = true
				return result
			}
			next := page.Messages[0].ID
			for _, message := range page.Messages {
				if message.ID < next {
					next = message.ID
				}
			}
			result.before = next
			if next <= oldest {
				result.done = true
				return result
			}
		}
		return result
	}
}

// maxReconcilePages bounds the pages one reconcile command reads; a longer
// window continues in the next command, from the saved cursor.
const maxReconcilePages = 20

type reconcileDue struct {
	channel        string
	before, oldest int64
}

// reconciled applies a reconcile read. The channel stays stale until the whole
// held window is covered. A failure retries from the same cursor with backoff.
func (m Model) reconciled(msg messagesReconciled) (tea.Model, tea.Cmd) {
	if msg.channel != m.current() {
		return m, nil
	}
	if msg.err != nil {
		// The pages read before the failure moved the cursor past them, so
		// they are applied now; the retry continues below them.
		m.addMessages(msg.channel, msg.messages)
		delay := m.mod.reconcileBackoff[msg.channel]
		if delay <= 0 {
			delay = minResubscribeDelay
		}
		m.mod.stale[msg.channel] = true
		m.mod.reconcileBackoff[msg.channel] = 2 * delay
		if m.mod.reconcileBackoff[msg.channel] > maxResubscribeDelay {
			m.mod.reconcileBackoff[msg.channel] = maxResubscribeDelay
		}
		m.setStatus(modeChannels, "history may be stale: removed messages might still show; retrying…")
		return m, m.after(delay, reconcileDue{channel: msg.channel, before: msg.before, oldest: msg.oldest})
	}
	m.addMessages(msg.channel, msg.messages)
	naming := m.nameAuthors(msg.channel)
	if !msg.done {
		m.mod.stale[msg.channel] = true
		return m, tea.Batch(naming, m.reconcileMessages(msg.channel, msg.before, msg.oldest))
	}
	delete(m.mod.stale, msg.channel)
	delete(m.mod.reconcileBackoff, msg.channel)
	return m, naming
}

func (m Model) reconcileRetry(msg reconcileDue) (tea.Model, tea.Cmd) {
	if !m.mod.stale[msg.channel] || msg.channel != m.current() {
		return m, nil
	}
	return m, m.reconcileMessages(msg.channel, msg.before, msg.oldest)
}

// confirmationLines is the pending moderation request as a panel of wrapped
// rows, the full consequence and the keys, in width cells. Nil when none.
func (m Model) confirmationLines(width int) []string {
	if m.mod.pending == nil || width < 1 {
		return nil
	}
	text := "Confirm: " + moderationQuestion(*m.mod.pending) + " " + confirmKeys
	var lines []string
	for _, line := range wrapWords(sanitize(text), width, width) {
		lines = append(lines, warnStyle.Render(fitCells(line, width)))
	}
	return lines
}

const confirmKeys = "y to confirm; any other key to cancel"

// startReconcile begins re-reading the held window of the selected channel,
// which is stale until the read has covered it.
func (m Model) startReconcile() tea.Cmd {
	cmd := m.reconcileMessages(m.current(), 0, 0)
	if cmd != nil {
		m.mod.stale[m.current()] = true
	}
	return cmd
}

// compactConfirmation fills a width x height screen with the pending request
// when the panes cannot share it: the target first, one row, the keys last,
// and the consequence between them, wrapped and cut with an ellipsis when it
// does not fit. The keys are never shown without the target.
func (m Model) compactConfirmation(width, height int) string {
	req := *m.mod.pending
	head := fitCells(sanitize(fmt.Sprintf("%s %d @ %s", req.op, req.target, channelLabel(req.channel))), width)
	body := wrapWords(sanitize(moderationQuestion(req)), width, width)
	room := max(height-2, 0)
	if len(body) > room {
		body = body[:room]
		if room > 0 {
			body[room-1] = fitCells(strings.TrimRight(body[room-1], " ")+"…", width)
		}
	}
	lines := make([]string, 0, height)
	lines = append(lines, warnStyle.Render(head))
	for _, line := range body {
		lines = append(lines, fitCells(line, width))
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, warnStyle.Render(fitCells(confirmKeys, width)))
	return strings.Join(lines[:min(len(lines), height)], "\n")
}
