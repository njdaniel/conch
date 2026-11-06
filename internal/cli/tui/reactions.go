package tui

import (
	"cmp"
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

type reactionAPI interface {
	SetReaction(context.Context, string, int64, string) (schema.ReactionSummaryV1, error)
	RemoveReaction(context.Context, string, int64, string) (schema.ReactionSummaryV1, error)
	ReactionSummaries(context.Context, string, []int64) (schema.ReactionSummariesResponseV1, error)
	ListReactors(context.Context, string, int64, string, int64, int) (schema.ListReactorsResponseV1, error)
}

// Confirmation state is owned by the update loop, including during retry waits.
type reactionConfirmation struct {
	inFlight bool
	dirty    bool
	newest   schema.ReactionSummaryV1
	backoff  time.Duration
}

type reactionConfirmationDue struct {
	channel   string
	messageID int64
}

type reactionModel struct {
	confirming  map[string]map[int64]*reactionConfirmation
	api         reactionAPI
	messages    map[string]map[int64]schema.ReactionSummaryV1
	revisions   map[string]map[int64]uint64
	unversioned map[string]map[int64]bool
	pending     map[string]map[int64]schema.MessageV2
	unsupported map[string]bool
	text        bool
	writing     bool
}

type reactionsLoaded struct {
	channel    string
	requested  []int64
	confirming map[int64]bool
	revisions  map[int64]uint64
	summaries  []schema.ReactionSummaryV1
	err        error
}

type reactionHydrationDue struct {
	channel string
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
	r := reactionModel{
		confirming:  make(map[string]map[int64]*reactionConfirmation),
		messages:    make(map[string]map[int64]schema.ReactionSummaryV1),
		revisions:   make(map[string]map[int64]uint64),
		unversioned: make(map[string]map[int64]bool),
		pending:     make(map[string]map[int64]schema.MessageV2),
		unsupported: make(map[string]bool),
	}
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
		if work := m.react.confirming[channel][message.ID]; work != nil {
			if work.inFlight {
				work.dirty = true
				continue
			}
			work.inFlight = true
			work.dirty = false
		}
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
		// Snapshot applied-state revisions; only omissions need this guard.
		confirming := make(map[int64]bool)
		revisions := make(map[int64]uint64, len(batch))
		current := m.reactionRevisions(channel)
		for _, id := range batch {
			revisions[id] = current[id]
			confirming[id] = m.react.confirming[channel][id] != nil
		}
		commands = append(commands, func() tea.Msg {
			response, err := m.react.api.ReactionSummaries(m.ctx, channel, batch)
			return reactionsLoaded{channel: channel, requested: batch, confirming: confirming, revisions: revisions, summaries: response.Summaries, err: err}
		})
	}
	return tea.Batch(commands...)
}

func (m Model) reloadReactions(channel string) tea.Cmd {
	return m.loadReactions(channel, m.messages[channel])
}

func (m Model) reactionsHydrated(msg reactionsLoaded) Model {
	current := m.reactionRevisions(msg.channel)
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
	summaries := make(map[int64]schema.ReactionSummaryV1, len(msg.summaries))
	for _, summary := range msg.summaries {
		summaries[summary.MessageID] = summary
	}
	for _, id := range msg.requested {
		if summary, ok := summaries[id]; ok {
			// Present summaries are ordered solely by as_of, even if another
			// request, event or write changed state while this batch ran.
			if applyReactionSummary(byID, summary) {
				current[id]++
				delete(m.react.unversioned[msg.channel], id)
			}
		} else if current[id] == msg.revisions[id] {
			if summary, ok := byID[id]; ok {
				// Keep the timestamp watermark, but an omission has no timestamp
				// of its own. Non-batch frames need a confirming batch first.
				summary.Reactions = nil
				byID[id] = summary
			}
			current[id]++
			if m.react.unversioned[msg.channel] == nil {
				m.react.unversioned[msg.channel] = make(map[int64]bool)
			}
			if m.react.unversioned[msg.channel][id] {
				// A second, confirming omission settles the unversioned clear.
				delete(m.react.unversioned[msg.channel], id)
			} else {
				m.react.unversioned[msg.channel][id] = true
			}
		}
	}
	return m
}

// applyReactionSummary replaces whole state; an older as_of never rolls it back.
func applyReactionSummary(byID map[int64]schema.ReactionSummaryV1, summary schema.ReactionSummaryV1) bool {
	if summary.MessageID <= 0 {
		return false
	}
	if current, ok := byID[summary.MessageID]; ok && current.AsOf.Time().After(summary.AsOf.Time()) {
		return false
	}
	byID[summary.MessageID] = summary
	return true
}

func (m Model) reactionEvent(msg reactionSummaryReceived) (Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen || msg.channel != m.current() {
		return m, wait
	}
	confirm := m.receiveReactionSummary(msg.channel, msg.summary)
	return m, tea.Batch(wait, confirm)
}

// receiveReactionSummary handles events and own-write responses. An unversioned
// clear cannot order these frames, so coalesce an authoritative batch read.
func (m Model) receiveReactionSummary(channel string, summary schema.ReactionSummaryV1) tea.Cmd {
	if work := m.react.confirming[channel][summary.MessageID]; work != nil {
		work.dirty = true
		if !work.newest.AsOf.Time().After(summary.AsOf.Time()) {
			work.newest = summary
		}
		return nil
	}
	if m.react.unversioned[channel][summary.MessageID] {
		if m.react.confirming[channel] == nil {
			m.react.confirming[channel] = make(map[int64]*reactionConfirmation)
		}
		m.react.confirming[channel][summary.MessageID] = &reactionConfirmation{newest: summary}
		return m.queueReactionHydration(channel, schema.MessageV2{ID: summary.MessageID})
	}
	byID := m.react.messages[channel]
	if byID == nil {
		byID = make(map[int64]schema.ReactionSummaryV1)
		m.react.messages[channel] = byID
	}
	if applyReactionSummary(byID, summary) {
		m.reactionRevisions(channel)[summary.MessageID]++
	}
	return nil
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
	if m.input != "" {
		return m
	}
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

func (m Model) reactionDone(msg reactionWritten) (Model, tea.Cmd) {
	m.react.writing = false
	if msg.err != nil {
		if cli.ReactionsUnsupported(msg.err) {
			m.react.unsupported[msg.channel] = true
		}
		if msg.channel == m.current() {
			m.setStatus(modeChannels, msg.err.Error())
		}
		return m, nil
	}
	if msg.summary.MessageID != msg.messageID {
		if msg.channel == m.current() {
			m.setStatus(modeChannels, "server answered about a different message; reaction not displayed")
		}
		return m, nil
	}
	confirm := m.receiveReactionSummary(msg.channel, msg.summary)
	if msg.channel == m.current() {
		m.input = strings.TrimPrefix(m.input, msg.raw)
		if msg.set {
			m.setStatus(modeChannels, "reaction added")
		} else {
			m.setStatus(modeChannels, "reaction removed")
		}
	}
	return m, confirm
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

// reactionRevisions is only accessed on the model update loop, never by commands.
func (m Model) reactionRevisions(channel string) map[int64]uint64 {
	if m.react.revisions[channel] == nil {
		m.react.revisions[channel] = make(map[int64]uint64)
	}
	return m.react.revisions[channel]
}

// queueReactionHydration coalesces live and replayed messages over a short window.
// The timer carries only the channel; the update loop owns the pending IDs.
func (m Model) queueReactionHydration(channel string, message schema.MessageV2) tea.Cmd {
	if !m.reactionsAvailable(channel) || message.ID <= 0 {
		return nil
	}
	first := len(m.react.pending[channel]) == 0
	if first {
		m.react.pending[channel] = make(map[int64]schema.MessageV2)
	}
	m.react.pending[channel][message.ID] = message
	if first {
		return tickAfter(50*time.Millisecond, reactionHydrationDue{channel: channel})
	}
	return nil
}

func (m Model) flushReactionHydration(channel string) tea.Cmd {
	pending := m.react.pending[channel]
	delete(m.react.pending, channel)
	messages := make([]schema.MessageV2, 0, len(pending))
	for _, message := range pending {
		messages = append(messages, message)
	}
	slices.SortFunc(messages, func(a, b schema.MessageV2) int { return cmp.Compare(a.ID, b.ID) })
	return m.loadReactions(channel, messages)
}

// reactionLoadDone settles each serialized confirmation after applying its read.
func (m Model) reactionLoadDone(msg reactionsLoaded) (Model, tea.Cmd) {
	m = m.reactionsHydrated(msg)
	var commands []tea.Cmd
	for id, confirming := range msg.confirming {
		if !confirming {
			continue
		}
		work := m.react.confirming[msg.channel][id]
		if work == nil {
			continue
		}
		work.inFlight = false
		if m.ctx.Err() != nil || !slices.Contains(m.channels, msg.channel) || !m.reactionsAvailable(msg.channel) {
			delete(m.react.confirming[msg.channel], id)
			continue
		}
		if msg.err != nil {
			delay := work.backoff
			if delay == 0 {
				delay = minResubscribeDelay
			}
			work.backoff = delay * 2
			if work.backoff > maxResubscribeDelay {
				work.backoff = maxResubscribeDelay
			}
			commands = append(commands, m.after(delay, reactionConfirmationDue{channel: msg.channel, messageID: id}))
		} else if work.dirty {
			work.dirty = false
			work.backoff = 0
			commands = append(commands, m.loadReactions(msg.channel, []schema.MessageV2{{ID: id}}))
		} else {
			delete(m.react.confirming[msg.channel], id)
		}
	}
	return m, tea.Batch(commands...)
}

func (m Model) retryReactionConfirmation(msg reactionConfirmationDue) tea.Cmd {
	work := m.react.confirming[msg.channel][msg.messageID]
	if work == nil || work.inFlight {
		return nil
	}
	if m.ctx.Err() != nil || !slices.Contains(m.channels, msg.channel) || !m.reactionsAvailable(msg.channel) {
		delete(m.react.confirming[msg.channel], msg.messageID)
		return nil
	}
	// The retry itself covers all frames received during the backoff.
	work.dirty = false
	return m.loadReactions(msg.channel, []schema.MessageV2{{ID: msg.messageID}})
}
