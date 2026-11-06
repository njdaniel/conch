package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

type reactionAPI interface {
	SetReaction(context.Context, string, int64, string) (schema.ReactionSummaryV1, error)
	RemoveReaction(context.Context, string, int64, string) (schema.ReactionSummaryV1, error)
	ReactionSummaries(context.Context, string, []int64) (schema.ReactionSummariesResponseV1, error)
	ListReactors(context.Context, string, int64, string, int64, int) (schema.ListReactorsResponseV1, error)
}

type reactionModel struct {
	api         reactionAPI
	messages    map[string]map[int64]schema.ReactionSummaryV1
	unsupported map[string]bool
	text        bool
	writing     bool
}

type reactionsLoaded struct {
	channel   string
	requested []int64
	summaries []schema.ReactionSummaryV1
	err       error
}

type reactionSummaryReceived struct {
	gen     int
	channel string
	summary schema.ReactionSummaryV1
}

type reactionWritten struct {
	channel   string
	messageID int64
	raw       string
	set       bool
	summary   schema.ReactionSummaryV1
	err       error
}

type reactorsListed struct {
	channel   string
	messageID int64
	emoji     string
	reactors  []schema.ReactionV1
	err       error
}

func newReactionModel(api API) reactionModel {
	r := reactionModel{messages: make(map[string]map[int64]schema.ReactionSummaryV1), unsupported: make(map[string]bool)}
	r.api, _ = api.(reactionAPI)
	return r
}

func (m Model) reactionsAvailable(channel string) bool {
	return m.react.api != nil && !m.react.unsupported[channel]
}

func (m Model) loadReactions(channel string, messages []schema.MessageV2) tea.Cmd {
	if !m.reactionsAvailable(channel) || len(messages) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(messages))
	for _, message := range messages {
		if message.ID > 0 {
			ids = append(ids, message.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var commands []tea.Cmd
	for start := 0; start < len(ids); start += schema.MaxReactionBatchMessages {
		batch := slices.Clone(ids[start:min(start+schema.MaxReactionBatchMessages, len(ids))])
		commands = append(commands, func() tea.Msg {
			response, err := m.react.api.ReactionSummaries(m.ctx, channel, batch)
			return reactionsLoaded{channel: channel, requested: batch, summaries: response.Summaries, err: err}
		})
	}
	return tea.Batch(commands...)
}

func (m Model) reloadReactions(channel string) tea.Cmd {
	return m.loadReactions(channel, m.messages[channel])
}

func (m Model) reactionsHydrated(msg reactionsLoaded) Model {
	if cli.ReactionsUnsupported(msg.err) {
		m.react.unsupported[msg.channel] = true
		delete(m.react.messages, msg.channel)
		if msg.channel == m.current() {
			m.setStatus(modeChannels, "reactions are not available on this server")
		}
		return m
	}
	if msg.err != nil {
		if msg.channel == m.current() {
			m.setStatus(modeChannels, msg.err.Error())
		}
		return m
	}
	byID := m.react.messages[msg.channel]
	if byID == nil {
		byID = make(map[int64]schema.ReactionSummaryV1)
		m.react.messages[msg.channel] = byID
	}
	present := make(map[int64]bool, len(msg.summaries))
	for _, summary := range msg.summaries {
		present[summary.MessageID] = true
		applyReactionSummary(byID, summary)
	}
	for _, id := range msg.requested {
		if !present[id] {
			delete(byID, id)
		}
	}
	return m
}

// applyReactionSummary replaces whole state; an older as_of never rolls it back.
func applyReactionSummary(byID map[int64]schema.ReactionSummaryV1, summary schema.ReactionSummaryV1) {
	if summary.MessageID <= 0 {
		return
	}
	if current, ok := byID[summary.MessageID]; ok && current.AsOf.Time().After(summary.AsOf.Time()) {
		return
	}
	if len(summary.Reactions) == 0 {
		delete(byID, summary.MessageID)
		return
	}
	byID[summary.MessageID] = summary
}

func (m Model) reactionEvent(msg reactionSummaryReceived) (Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen || msg.channel != m.current() {
		return m, wait
	}
	byID := m.react.messages[msg.channel]
	if byID == nil {
		byID = make(map[int64]schema.ReactionSummaryV1)
		m.react.messages[msg.channel] = byID
	}
	applyReactionSummary(byID, msg.summary)
	return m, wait
}

func (m Model) reactionLines(channel string, messageID int64, width int) []string {
	if !m.reactionsAvailable(channel) {
		return nil
	}
	summary := m.react.messages[channel][messageID]
	if len(summary.Reactions) == 0 {
		return nil
	}
	parts := make([]string, 0, len(summary.Reactions))
	for _, count := range summary.Reactions {
		label := sanitize(cli.ReactionLabel(count.Emoji, m.react.text))
		part := fmt.Sprintf("%s x%d", label, count.Count)
		if count.Me {
			part += " (you)"
		}
		parts = append(parts, part)
	}
	prefix := "  " + statusStyle.Render(contMark)
	lines := wrapWords(strings.Join(parts, "  "), max(1, width-cellWidth(prefix)), max(1, width-cellWidth(prefix)))
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return lines
}

func (m Model) reactionTarget() (int64, bool) {
	messages := m.messages[m.current()]
	owners := m.historyOwners(m.chatLayout().rightW)
	at := len(owners) - 1 - m.scroll
	if at < 0 || at >= len(owners) {
		return 0, false
	}
	index := owners[at]
	if index < 0 || index >= len(messages) {
		return 0, false
	}
	return messages[index].ID, true
}

func (m Model) reactionKeyCommand(command string) Model {
	if !m.reactionsAvailable(m.current()) {
		m.setStatus(modeChannels, "reactions are not available on this server")
		return m
	}
	id, ok := m.reactionTarget()
	if !ok {
		m.setStatus(modeChannels, "no message selected")
		return m
	}
	m.input = fmt.Sprintf("/%s %d ", command, id)
	return m
}

func (m Model) reactionCommand(command, rest string) (Model, tea.Cmd) {
	if !m.reactionsAvailable(m.current()) {
		m.setStatus(modeChannels, "reactions are not available on this server")
		return m, nil
	}
	fields := strings.Fields(rest)
	if command == "reactors" {
		return m.reactorsCommand(fields)
	}
	if len(fields) < 1 || len(fields) > 2 {
		m.setStatus(modeChannels, "usage: /"+command+" [message-id] <emoji-or-shortcode>")
		return m, nil
	}
	channel := m.current()
	messageID, ok := m.reactionTarget()
	emoji := fields[0]
	if len(fields) == 2 {
		var err error
		messageID, err = strconv.ParseInt(fields[0], 10, 64)
		if err != nil || messageID <= 0 {
			m.setStatus(modeChannels, "message id must be a positive integer")
			return m, nil
		}
		emoji = fields[1]
		ok = true
	}
	if !ok || messageID <= 0 {
		m.setStatus(modeChannels, "no message selected")
		return m, nil
	}
	key, err := cli.ResolveReactionKey(emoji)
	if err != nil {
		m.setStatus(modeChannels, err.Error())
		return m, nil
	}
	if m.react.writing {
		m.setStatus(modeChannels, "a reaction is still being written…")
		return m, nil
	}
	m.react.writing = true
	raw := m.input
	m.setStatus(modeChannels, "writing reaction…")
	return m, m.writeReaction(channel, messageID, key, raw, command == "react")
}

func (m Model) writeReaction(channel string, messageID int64, key, raw string, set bool) tea.Cmd {
	return func() tea.Msg {
		var summary schema.ReactionSummaryV1
		var err error
		if set {
			summary, err = m.react.api.SetReaction(m.ctx, channel, messageID, key)
		} else {
			summary, err = m.react.api.RemoveReaction(m.ctx, channel, messageID, key)
		}
		return reactionWritten{channel: channel, messageID: messageID, raw: raw, set: set, summary: summary, err: err}
	}
}

func (m Model) reactionDone(msg reactionWritten) Model {
	m.react.writing = false
	if msg.err != nil {
		if cli.ReactionsUnsupported(msg.err) {
			m.react.unsupported[msg.channel] = true
		}
		if msg.channel == m.current() {
			m.setStatus(modeChannels, msg.err.Error())
		}
		return m
	}
	if msg.summary.MessageID != msg.messageID {
		if msg.channel == m.current() {
			m.setStatus(modeChannels, "server answered about a different message; reaction not displayed")
		}
		return m
	}
	byID := m.react.messages[msg.channel]
	if byID == nil {
		byID = make(map[int64]schema.ReactionSummaryV1)
		m.react.messages[msg.channel] = byID
	}
	applyReactionSummary(byID, msg.summary)
	if msg.channel == m.current() {
		m.input = strings.TrimPrefix(m.input, msg.raw)
		if msg.set {
			m.setStatus(modeChannels, "reaction added")
		} else {
			m.setStatus(modeChannels, "reaction removed")
		}
	}
	return m
}

func (m Model) reactorsCommand(fields []string) (Model, tea.Cmd) {
	if len(fields) < 1 || len(fields) > 2 {
		m.setStatus(modeChannels, "usage: /reactors [message-id] <emoji-or-shortcode>")
		return m, nil
	}
	messageID, ok := m.reactionTarget()
	emoji := fields[0]
	if len(fields) == 2 {
		var err error
		messageID, err = strconv.ParseInt(fields[0], 10, 64)
		if err != nil || messageID <= 0 {
			m.setStatus(modeChannels, "message id must be a positive integer")
			return m, nil
		}
		emoji = fields[1]
		ok = true
	}
	if !ok || messageID <= 0 {
		m.setStatus(modeChannels, "no message selected")
		return m, nil
	}
	key, err := cli.ResolveReactionKey(emoji)
	if err != nil {
		m.setStatus(modeChannels, err.Error())
		return m, nil
	}
	channel := m.current()
	m.setStatus(modeChannels, "loading reactors…")
	return m, func() tea.Msg {
		var all []schema.ReactionV1
		var after int64
		for {
			page, err := m.react.api.ListReactors(m.ctx, channel, messageID, key, after, schema.MaxReactorLimit)
			if err != nil {
				return reactorsListed{channel: channel, messageID: messageID, emoji: key, err: err}
			}
			all = append(all, page.Reactors...)
			if page.NextAfter == 0 {
				break
			}
			after = page.NextAfter
		}
		return reactorsListed{channel: channel, messageID: messageID, emoji: key, reactors: all}
	}
}

func (m Model) reactorsDone(msg reactorsListed) Model {
	if msg.channel != m.current() {
		return m
	}
	if msg.err != nil {
		m.setStatus(modeChannels, msg.err.Error())
		return m
	}
	if len(msg.reactors) == 0 {
		m.setStatus(modeChannels, "no "+cli.ReactionLabel(msg.emoji, true)+" reactions on message "+strconv.FormatInt(msg.messageID, 10))
		return m
	}
	names := make([]string, 0, len(msg.reactors))
	for _, reactor := range msg.reactors {
		name := "hidden-" + strconv.FormatInt(reactor.PrincipalID, 10)
		if member, ok := m.members[msg.channel][reactor.PrincipalID]; ok {
			name = member.Name
			if member.Guest != nil {
				name = schema.GuestDisplayName(member.Guest.Label, member.Guest.Handle)
			}
		}
		if reactor.GuestReactor != nil {
			name = schema.GuestDisplayName(reactor.GuestReactor.Label, reactor.GuestReactor.Handle)
		}
		names = append(names, name)
	}
	m.setStatus(modeChannels, "reactors "+cli.ReactionLabel(msg.emoji, true)+" on message "+strconv.FormatInt(msg.messageID, 10)+": "+strings.Join(names, ", "))
	return m
}

func (m Model) emojiCommand(rest string) Model {
	switch rest {
	case "text":
		m.react.text = true
		m.setStatus(modeChannels, "reaction text fallback on; compound emoji stay intact in Unicode mode")
	case "unicode":
		m.react.text = false
		m.setStatus(modeChannels, "reaction Unicode glyphs on")
	default:
		m.setStatus(modeChannels, "usage: /emoji text|unicode")
	}
	m.input = ""
	return m
}
