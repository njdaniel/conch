// Package tui implements the interactive Conch terminal client.
package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// API is the shared Conch client surface used by the TUI.
type API interface {
	// Messages travel through the v2 methods only: a v1 reader would render a
	// scoped message as an open one, so the v1 methods are deliberately not
	// part of this interface and the TUI cannot call them.
	ListMessagesV2(context.Context, string, int64, int) (schema.ListMessagesResponseV2, error)
	PostMessageV2(ctx context.Context, channel string, authorID int64, body string, audience *schema.Audience) (schema.MessageV2, error)
	SubscribeV2(context.Context, string, func(schema.MessageV2) error) error
	ListNets(context.Context, string) (schema.ListNetsResponseV1, error)
	ListChannels(context.Context) (schema.ListChannelsResponse, error)
	ListApprovals(context.Context) (schema.ListApprovalsResponseV1, error)
	CastDecision(context.Context, int64, schema.CastDecisionRequestV1) (schema.CastDecisionResponseV1, error)
	WhoAmI(context.Context) (schema.WhoAmIResponseV1, error)
}

type channelsLoaded struct {
	channels []schema.ChannelV0
	err      error
}
type messagesLoaded struct {
	channel  string
	messages []schema.MessageV2
	err      error
}
type messageReceived struct {
	channel string
	message schema.MessageV2
}

// netsLoaded carries a channel's net roster. resolve is set when the load was
// started by "/net <want>" because the name was not in the roster we had.
type netsLoaded struct {
	channel string
	nets    []schema.NetV1
	err     error
	resolve bool
	want    string
}

// netTarget is where plain messages go instead of the whole channel.
type netTarget struct {
	channel string
	id      int64
	name    string
}
type subscriptionEnded struct {
	channel string
	err     error
	// lived is how long the subscription ran; one that stayed up for a while
	// was healthy, so the next drop starts the backoff over.
	lived time.Duration
}

// resubscribeDue fires when a dropped subscription's backoff delay elapses.
type resubscribeDue struct{ channel string }

// statusReconnecting is shown while the selected channel's live subscription
// is down and a retry is scheduled.
const statusReconnecting = "live updates: reconnecting…"

const (
	minResubscribeDelay = time.Second
	maxResubscribeDelay = 30 * time.Second
)

// messageSent is the result of one post. raw is the text as typed, so the input
// is cleared only if the user has not started something new since.
type messageSent struct {
	channel string
	raw     string
	whisper bool
	message schema.MessageV2
	err     error
}
type approvalsLoaded struct {
	approvals []schema.ApprovalV1
	err       error
}
type whoAmILoaded struct {
	who schema.WhoAmIResponseV1
	err error
}
type decisionCast struct {
	err error
}

type mode int

const (
	modeChannels mode = iota
	modeInbox
	modeDecision
)

// Model is the root Bubble Tea model. Network results enter Update as messages,
// keeping state transitions deterministic and independently testable.
type Model struct {
	ctx      context.Context
	api      API
	authorID int64
	// authenticated is true when the client sends a credential; the author is
	// then whoever the server says it is, learned from whoami.
	authenticated bool
	userName      string
	// whoErr is why whoami failed; nil while it is pending or after success.
	whoErr     error
	channels   []string
	selected   int
	messages   map[string][]schema.MessageV2
	subscribed map[string]bool
	input      string
	// channelStatus and inboxStatus are the status line of each mode. A
	// background result writes the status of the mode it belongs to, so it
	// cannot replace what the user is looking at in another mode. The decision
	// prompt shares inboxStatus: it is entered from the inbox, esc returns to
	// it, and a refresh of the approvals it decides on reports there.
	channelStatus string
	inboxStatus   string
	width         int
	height        int
	events        chan tea.Msg
	mode          mode
	approvals     []schema.ApprovalV1
	selApproval   int
	selOption     int
	// loadingChannels is true while the channel list is being fetched from the
	// server; the model has no channels until channelsLoaded arrives.
	loadingChannels bool
	// notice is a channel-list problem that must survive the "connected"
	// status the first backfill would otherwise write.
	notice string
	// backoff is the delay the next failure of a channel's subscription will
	// wait; absent means minResubscribeDelay.
	backoff map[string]time.Duration
	// retryPending marks channels whose resubscribeDue timer is in flight. It
	// is what keeps "select away and back" from starting a second subscription
	// while the timer is still going to start one.
	retryPending map[string]bool
	// nets is each channel's net roster as last loaded; it resolves "/net
	// <name>" and the names shown in scope badges.
	nets map[string][]schema.NetV1
	// target is the net plain messages are sent to; nil means the channel.
	// It is never carried across channels, and nothing ever falls back from a
	// scoped send to a channel-wide one.
	target *netTarget
	// sending is true while a post is in flight, so a second enter cannot
	// send the same text twice.
	sending bool
	// whisperNoted records that the audit-log notice was shown this session.
	whisperNoted bool
	// after schedules msg to be delivered once d has elapsed. Injectable so
	// tests can observe the delay instead of sleeping through it.
	after func(d time.Duration, msg tea.Msg) tea.Cmd
}

// setStatus records status for mode; modeDecision shares the inbox's.
func (m *Model) setStatus(mode mode, status string) {
	if mode == modeChannels {
		m.channelStatus = status
		return
	}
	m.inboxStatus = status
}

// status is the status line of the mode the user is in.
func (m Model) status() string {
	if m.mode == modeChannels {
		return m.channelStatus
	}
	return m.inboxStatus
}

func tickAfter(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// NewModel constructs a model for the configured channels. When channels is
// empty (after trimming) the model starts with no channels and asks the server
// for the list in Init.
func NewModel(ctx context.Context, api API, authorID int64, channels []string) Model {
	clean := make([]string, 0, len(channels))
	seen := make(map[string]bool)
	for _, channel := range channels {
		channel = strings.TrimSpace(channel)
		if channel != "" && !seen[channel] {
			clean = append(clean, channel)
			seen[channel] = true
		}
	}
	m := Model{ctx: ctx, api: api, authorID: authorID,
		messages: make(map[string][]schema.MessageV2), nets: make(map[string][]schema.NetV1), subscribed: make(map[string]bool),
		events: make(chan tea.Msg, 64), mode: modeChannels,
		backoff: make(map[string]time.Duration), retryPending: make(map[string]bool), after: tickAfter}
	if len(clean) == 0 {
		m.loadingChannels = true
		return m
	}
	m.channels = clean
	m.subscribed[clean[0]] = true
	return m
}

// WithCredential marks the model as running with a bearer credential: Init
// asks the server who the user is, and that answer replaces the author ID.
func (m Model) WithCredential() Model {
	m.authenticated = true
	// Identity comes from the server only; never act as a caller-supplied id.
	m.authorID = 0
	return m
}

// Init starts REST backfill and the live subscription for the selected
// channel, or fetches the channel list first when none was configured.
func (m Model) Init() tea.Cmd {
	var who tea.Cmd
	if m.authenticated {
		who = m.loadWhoAmI()
	}
	if m.loadingChannels {
		return tea.Batch(who, m.loadChannels(), m.waitEvent())
	}
	return tea.Batch(who, m.loadCurrent(), m.loadNets(false, ""), m.startSubscription(), m.waitEvent())
}

// Update applies keyboard, window, and injected API-result messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			if m.mode == modeDecision {
				m.mode = modeInbox
				m.input = ""
				m.setStatus(modeInbox, "canceled decision")
				return m, nil
			}
			return m, tea.Quit
		case "tab":
			switch m.mode {
			case modeChannels:
				m.mode = modeInbox
				m.setStatus(modeInbox, "loading approvals…")
				return m, m.loadApprovals()
			case modeInbox:
				// The channel status is left as it is: results that landed
				// while the user was away are already recorded in it.
				m.mode = modeChannels
				return m, nil
			}
		case "up":
			switch m.mode {
			case modeChannels:
				return m.selectChannel(-1)
			case modeInbox:
				m.selApproval = max(0, m.selApproval-1)
				return m, nil
			case modeDecision:
				m.selOption = max(0, m.selOption-1)
				return m, nil
			}
		case "down":
			switch m.mode {
			case modeChannels:
				return m.selectChannel(1)
			case modeInbox:
				if len(m.approvals) > 0 {
					m.selApproval = min(len(m.approvals)-1, m.selApproval+1)
				}
				return m, nil
			case modeDecision:
				if len(m.approvals) == 0 || m.selApproval >= len(m.approvals) {
					return m, nil
				}
				app := m.approvals[m.selApproval]
				if len(app.Options) == 0 {
					return m, nil
				}
				m.selOption = min(len(app.Options)-1, m.selOption+1)
				return m, nil
			}
		case "enter":
			switch m.mode {
			case modeChannels:
				return m.submit()
			case modeInbox:
				if len(m.approvals) > 0 {
					m.mode = modeDecision
					m.selOption = 0
					m.input = ""
					m.setStatus(modeInbox, "type reason to decide")
					return m, nil
				}
			case modeDecision:
				reason := strings.TrimSpace(m.input)
				if reason == "" {
					m.setStatus(modeInbox, "reason is required")
					return m, nil
				}
				if m.authorID <= 0 {
					m.setStatus(modeInbox, m.noAuthorStatus("decide"))
					return m, nil
				}
				if len(m.approvals) == 0 || m.selApproval >= len(m.approvals) {
					m.setStatus(modeInbox, "no approval selected")
					m.mode = modeInbox
					return m, nil
				}
				app := m.approvals[m.selApproval]
				if len(app.Options) == 0 || m.selOption < 0 || m.selOption >= len(app.Options) {
					m.setStatus(modeInbox, "select a decision option")
					return m, nil
				}
				opt := app.Options[m.selOption]
				m.input = ""
				m.setStatus(modeInbox, "casting decision…")
				return m, m.castDecision(app.ID, opt.ID, reason)
			}
		case "backspace":
			if m.input != "" {
				_, size := utf8.DecodeLastRuneInString(m.input)
				m.input = m.input[:len(m.input)-size]
			}
		default:
			// A lone space arrives as KeySpace, not KeyRunes; both carry Runes.
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				if m.mode == modeChannels || m.mode == modeDecision {
					m.input += string(msg.Runes)
				}
			}
		}
	case whoAmILoaded:
		if msg.err != nil {
			m.whoErr = msg.err
			m.setStatus(modeChannels, whoErrText(msg.err))
			return m, nil
		}
		m.whoErr = nil
		m.authorID = msg.who.ID
		m.userName = msg.who.Name
		m.setStatus(modeChannels, "signed in as "+msg.who.Name)
	case approvalsLoaded:
		if msg.err != nil {
			m.setStatus(modeInbox, msg.err.Error())
		} else {
			m.approvals = msg.approvals
			m.selApproval = 0
			// A slower load can land after the user has already moved into
			// modeDecision on a stale (now-refreshed) list. If the refresh
			// came back empty there is nothing left to decide on; otherwise
			// the prompt they are answering stays on screen, since the
			// decision prompt shares the inbox's status.
			switch {
			case m.mode == modeDecision && len(m.approvals) == 0:
				m.mode = modeInbox
				m.setStatus(modeInbox, "no open approvals")
			case m.mode != modeDecision:
				m.setStatus(modeInbox, "inbox loaded")
			}
		}
	case decisionCast:
		if msg.err != nil {
			m.setStatus(modeInbox, msg.err.Error())
		} else {
			m.setStatus(modeInbox, "decision cast")
			m.mode = modeInbox
			return m, m.loadApprovals()
		}
	case channelsLoaded:
		if !m.loadingChannels {
			return m, nil
		}
		m.loadingChannels = false
		names := make([]string, 0, len(msg.channels))
		for _, channel := range msg.channels {
			if name := strings.TrimSpace(channel.Name); name != "" {
				names = append(names, name)
			}
		}
		switch {
		case msg.err != nil:
			m.notice = "channel list: " + msg.err.Error()
			if errors.Is(msg.err, cli.ErrUnauthenticated) {
				m.notice = msg.err.Error()
			}
			names = []string{"general"}
		case len(names) == 0:
			m.notice = "no channels on server"
			names = []string{"general"}
		}
		m.setStatus(modeChannels, m.notice)
		m.channels = names
		m.selected = 0
		m.subscribed[names[0]] = true
		return m, tea.Batch(m.loadCurrent(), m.loadNets(false, ""), m.startSubscription())
	case netsLoaded:
		return m.netsLoaded(msg)
	case messagesLoaded:
		// The result of a channel the user has since left must not overwrite
		// the status of the channel they are looking at.
		stale := msg.channel != m.current()
		if msg.err != nil {
			if stale {
				return m, nil
			}
			// A channel-list notice explains the failure better than the
			// fallback channel's own load error does.
			if m.notice != "" {
				m.setStatus(modeChannels, m.notice)
			} else {
				m.setStatus(modeChannels, msg.err.Error())
			}
		} else {
			m.messages[msg.channel] = mergeMessages(m.messages[msg.channel], msg.messages)
			if stale {
				return m, nil
			}
			switch {
			case m.notice != "":
				m.setStatus(modeChannels, m.notice)
			case m.retryPending[msg.channel]:
				// History loaded, but the live subscription is down and a
				// retry is waiting: "connected" would be untrue.
				m.setStatus(modeChannels, statusReconnecting)
			default:
				m.setStatus(modeChannels, "connected")
			}
		}
	case messageReceived:
		// Data flowing proves the subscription is healthy.
		delete(m.backoff, msg.channel)
		m.messages[msg.channel] = mergeMessages(m.messages[msg.channel], []schema.MessageV2{msg.message})
		return m, m.waitEvent()
	case subscriptionEnded:
		return m.subscriptionEnded(msg)
	case resubscribeDue:
		if !m.retryPending[msg.channel] {
			// Superseded: the channel already resubscribed some other way.
			return m, nil
		}
		delete(m.retryPending, msg.channel)
		if msg.channel != m.current() || m.subscribed[msg.channel] {
			// The user left; selectChannel resubscribes when they return.
			return m, nil
		}
		m.subscribed[msg.channel] = true
		// Backfill the gap; mergeMessages de-duplicates by id.
		return m, tea.Batch(m.loadCurrent(), m.startSubscription())
	case messageSent:
		m.sending = false
		if msg.err != nil {
			// The typed text stays so it can be fixed or retried by hand;
			// nothing else is sent in its place.
			m.setStatus(modeChannels, msg.err.Error())
			return m, nil
		}
		if msg.message.ID > 0 {
			// The same message also arrives on the subscription; ids dedupe.
			m.messages[msg.channel] = mergeMessages(m.messages[msg.channel], []schema.MessageV2{msg.message})
		}
		if m.input == msg.raw {
			m.input = ""
		}
		status := "sent"
		if msg.whisper && !m.whisperNoted {
			m.whisperNoted = true
			status += ". " + whisperNotice
		}
		m.setStatus(modeChannels, status)
	}
	return m, nil
}

// subscriptionEnded clears the channel's subscribed flag so it can be started
// again, and for the selected channel schedules that restart after a backoff
// delay so a subscription that fails immediately cannot spin.
func (m Model) subscriptionEnded(msg subscriptionEnded) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if errors.Is(msg.err, context.Canceled) || m.ctx.Err() != nil {
		return m, wait
	}
	m.subscribed[msg.channel] = false
	selected := msg.channel == m.current()
	if errors.Is(msg.err, cli.ErrUnauthenticated) {
		// Retrying cannot fix a missing login; the user must act, and
		// re-selecting the channel will try again afterwards.
		if selected && m.notice == "" {
			m.setStatus(modeChannels, msg.err.Error())
		}
		return m, wait
	}
	if msg.lived >= maxResubscribeDelay {
		delete(m.backoff, msg.channel)
	}
	if !selected {
		// Nobody is looking; selectChannel resubscribes on return.
		return m, wait
	}
	if m.notice == "" {
		m.setStatus(modeChannels, statusReconnecting)
	}
	delay, ok := m.backoff[msg.channel]
	if !ok {
		delay = minResubscribeDelay
	}
	m.backoff[msg.channel] = delay * 2
	if m.backoff[msg.channel] > maxResubscribeDelay {
		m.backoff[msg.channel] = maxResubscribeDelay
	}
	m.retryPending[msg.channel] = true
	return m, tea.Batch(wait, m.after(delay, resubscribeDue{channel: msg.channel}))
}

func (m Model) selectChannel(delta int) (tea.Model, tea.Cmd) {
	next := m.selected + delta
	if next < 0 || next >= len(m.channels) || next == m.selected {
		return m, nil
	}
	m.selected = next
	// A target belongs to the channel it was chosen in.
	m.target = nil
	m.setStatus(modeChannels, "loading…")
	commands := []tea.Cmd{m.loadCurrent(), m.loadNets(false, "")}
	// A pending retry timer will start the subscription itself.
	if !m.subscribed[m.current()] && !m.retryPending[m.current()] {
		m.subscribed[m.current()] = true
		commands = append(commands, m.startSubscription())
	}
	return m, tea.Batch(commands...)
}

// current returns the selected channel, or "" while there are no channels.
func (m Model) current() string {
	if m.selected < 0 || m.selected >= len(m.channels) {
		return ""
	}
	return m.channels[m.selected]
}

func (m Model) noAuthorStatus(action string) string {
	if m.authenticated {
		if m.whoErr != nil {
			return whoErrText(m.whoErr)
		}
		return "waiting for your identity from the server; cannot " + action + " yet"
	}
	return "set CONCH_AUTHOR to " + action
}

func whoErrText(err error) string {
	if errors.Is(err, cli.ErrUnauthenticated) {
		return err.Error()
	}
	return "whoami: " + err.Error()
}

func (m Model) loadWhoAmI() tea.Cmd {
	return func() tea.Msg {
		who, err := m.api.WhoAmI(m.ctx)
		return whoAmILoaded{who: who, err: err}
	}
}

func (m Model) loadChannels() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.api.ListChannels(m.ctx)
		return channelsLoaded{channels: resp.Channels, err: err}
	}
}

func (m Model) loadCurrent() tea.Cmd {
	channel := m.current()
	if channel == "" {
		return nil
	}
	return func() tea.Msg {
		var messages []schema.MessageV2
		var after int64
		for {
			page, err := m.api.ListMessagesV2(m.ctx, channel, after, 100)
			if err != nil {
				return messagesLoaded{channel: channel, err: err}
			}
			messages = append(messages, page.Messages...)
			if page.NextAfter == 0 || page.NextAfter <= after {
				return messagesLoaded{channel: channel, messages: messages}
			}
			after = page.NextAfter
		}
	}
}

func (m Model) startSubscription() tea.Cmd {
	channel := m.current()
	if channel == "" {
		return nil
	}
	return func() tea.Msg {
		go func() {
			started := time.Now()
			err := m.api.SubscribeV2(m.ctx, channel, func(message schema.MessageV2) error {
				select {
				case m.events <- messageReceived{channel: channel, message: message}:
					return nil
				case <-m.ctx.Done():
					return m.ctx.Err()
				}
			})
			select {
			case m.events <- subscriptionEnded{channel: channel, err: err, lived: time.Since(started)}:
			case <-m.ctx.Done():
			}
		}()
		return nil
	}
}

func (m Model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-m.events:
			return msg
		case <-m.ctx.Done():
			return tea.Quit()
		}
	}
}

// send posts body to channel with the given audience, once. There is no retry
// and no fallback: whatever the server answers is reported as is.
func (m Model) send(channel, raw, body string, audience *schema.Audience, whisper bool) tea.Cmd {
	author := m.authorID
	return func() tea.Msg {
		posted, err := m.api.PostMessageV2(m.ctx, channel, author, body, audience)
		return messageSent{channel: channel, raw: raw, whisper: whisper, message: posted, err: err}
	}
}

func (m Model) loadApprovals() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.api.ListApprovals(m.ctx)
		return approvalsLoaded{approvals: resp.Approvals, err: err}
	}
}

func (m Model) castDecision(approvalID int64, optionID string, reason string) tea.Cmd {
	return func() tea.Msg {
		_, err := m.api.CastDecision(m.ctx, approvalID, schema.CastDecisionRequestV1{PrincipalID: m.authorID, OptionID: optionID, Reason: reason})
		return decisionCast{err: err}
	}
}

func mergeMessages(existing, incoming []schema.MessageV2) []schema.MessageV2 {
	byID := make(map[int64]schema.MessageV2, len(existing)+len(incoming))
	for _, message := range existing {
		byID[message.ID] = message
	}
	for _, message := range incoming {
		byID[message.ID] = message
	}
	merged := make([]schema.MessageV2, 0, len(byID))
	for _, message := range byID {
		merged = append(merged, message)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return merged
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var (
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	activeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	badgeStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// View renders a compact two-pane layout that fits an 80x24 terminal.
func (m Model) View() string {
	width, height := m.width, m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	leftWidth := 18
	if width < 60 {
		leftWidth = 14
	}
	rightWidth := width - leftWidth - 1
	contentHeight := height - 4
	if contentHeight < 4 {
		contentHeight = 4
	}

	var panes string
	if m.mode == modeInbox || m.mode == modeDecision {
		inboxLines := []string{}
		for i, app := range m.approvals {
			prefix := "  "
			if i == m.selApproval {
				prefix = activeStyle.Render("› ")
			}
			esc := ""
			if app.State == schema.ApprovalStateEscalated {
				esc = badgeStyle.Render(" [ESC]")
			}
			title := clip(fmt.Sprintf("%d: %s", app.RequesterID, app.Title), leftWidth-4-utf8.RuneCountInString(esc))
			inboxLines = append(inboxLines, prefix+title+esc)
		}
		if len(inboxLines) == 0 {
			inboxLines = append(inboxLines, statusStyle.Render("No pending approvals"))
		}
		inbox := borderStyle.Width(leftWidth - 2).Height(contentHeight).Render(strings.Join(inboxLines, "\n"))

		detailsLines := []string{}
		if len(m.approvals) > 0 && m.selApproval < len(m.approvals) {
			app := m.approvals[m.selApproval]
			detailsLines = append(detailsLines, activeStyle.Render(app.Title))
			detailsLines = append(detailsLines, fmt.Sprintf("Requester: %d  Deadline: %s", app.RequesterID, app.Deadline.Time().Format("Jan 02 15:04")))
			if app.Payload != nil {
				detailsLines = append(detailsLines, badgeStyle.Render(fmt.Sprintf("[%s]", app.Payload.Schema)))
			}
			detailsLines = append(detailsLines, "", app.Body, "")
			if m.mode == modeDecision {
				detailsLines = append(detailsLines, activeStyle.Render("Decision Options:"))
				for i, opt := range app.Options {
					prefix := "  "
					if i == m.selOption {
						prefix = activeStyle.Render("› ")
					}
					detailsLines = append(detailsLines, prefix+opt.Label)
				}
			} else {
				for _, opt := range app.Options {
					detailsLines = append(detailsLines, "  - "+opt.Label)
				}
			}
		}
		visible := contentHeight - 1
		if len(detailsLines) > visible {
			detailsLines = detailsLines[:visible]
		}
		details := borderStyle.Width(rightWidth - 2).Height(contentHeight).Render(strings.Join(detailsLines, "\n"))
		panes = lipgloss.JoinHorizontal(lipgloss.Top, inbox, details)
	} else {
		channelLines := make([]string, len(m.channels))
		for i, channel := range m.channels {
			channel = clip(channel, leftWidth-4)
			prefix := "  "
			if i == m.selected {
				prefix = activeStyle.Render("› ")
			}
			channelLines[i] = prefix + channel
		}
		if len(channelLines) == 0 && m.loadingChannels {
			channelLines = append(channelLines, statusStyle.Render("loading…"))
		}
		channels := borderStyle.Width(leftWidth - 2).Height(contentHeight).Render(strings.Join(channelLines, "\n"))
		messageLines := make([]string, 0, len(m.messages[m.current()]))
		for _, message := range m.messages[m.current()] {
			messageLines = append(messageLines, m.messageLine(m.current(), message, rightWidth))
		}
		if len(messageLines) == 0 {
			messageLines = append(messageLines, statusStyle.Render("No messages"))
		}
		visible := contentHeight - 1
		if len(messageLines) > visible {
			messageLines = messageLines[len(messageLines)-visible:]
		}
		messages := borderStyle.Width(rightWidth - 2).Height(contentHeight).Render(strings.Join(messageLines, "\n"))
		panes = lipgloss.JoinHorizontal(lipgloss.Top, channels, messages)
	}

	var inputStr string
	if m.mode == modeDecision || m.mode == modeChannels {
		prompt := ">"
		if m.mode == modeChannels {
			prompt = m.promptWithin(width / 2)
		}
		inputStr = lipgloss.NewStyle().Width(width).Render(prompt + " " + clipTail(m.input, width-utf8.RuneCountInString(prompt)-2))
	} else {
		inputStr = lipgloss.NewStyle().Width(width).Render("")
	}

	var statusKeys string
	switch m.mode {
	case modeDecision:
		statusKeys = "  ↑/↓ options • enter confirm • esc cancel"
	case modeInbox:
		statusKeys = "  ↑/↓ approvals • enter decide • tab channels • esc quit"
	default:
		statusKeys = "  ↑/↓ channels • enter send • tab inbox • esc quit"
	}

	status := m.status() + statusKeys
	if m.userName != "" {
		status = m.userName + " | " + status
	}
	status = statusStyle.Width(width).Render(status)
	return panes + "\n" + inputStr + "\n" + status
}

func clip(value string, width int) string {
	if width < 1 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func clipTail(value string, width int) string {
	if width < 1 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return "…" + string(runes[len(runes)-width+1:])
}

// whisperNotice mirrors the plain CLI's note: a whisper is discretion, not
// secrecy, and users should not mistake it for the latter.
const whisperNotice = "note: whispers are recorded in the audit log"

const slashHint = `commands: /net [name], /w <ids> <text>; start a message with // to send a literal /`

// submit handles enter in the channel pane: a slash command, or a message sent
// to the current target. Every refusal leaves the input as typed and sends
// nothing.
func (m Model) submit() (tea.Model, tea.Cmd) {
	body := strings.TrimSpace(m.input)
	if body == "" {
		return m, nil
	}
	if len(m.channels) == 0 {
		m.setStatus(modeChannels, "no channel selected yet")
		return m, nil
	}
	if m.sending {
		m.setStatus(modeChannels, "still sending the previous message…")
		return m, nil
	}
	if !strings.HasPrefix(body, "/") {
		return m.sendToTarget(body)
	}
	// "//text" is the escape for a message that really starts with "/".
	if strings.HasPrefix(body, "//") {
		return m.sendToTarget(body[1:])
	}
	name, rest := splitCommand(body)
	switch name {
	case "/net":
		return m.netCommand(rest)
	case "/w":
		return m.whisperCommand(rest)
	default:
		m.setStatus(modeChannels, fmt.Sprintf("unknown command %q; %s", sanitize(name), slashHint))
		return m, nil
	}
}

// splitCommand cuts "/cmd rest" at the first whitespace.
func splitCommand(body string) (name, rest string) {
	i := strings.IndexFunc(body, unicode.IsSpace)
	if i < 0 {
		return body, ""
	}
	return body[:i], strings.TrimSpace(body[i:])
}

// sendToTarget posts text to the net target, or to the whole channel when
// there is none. A target that does not belong to the channel on screen is
// refused rather than sent anywhere else.
func (m Model) sendToTarget(text string) (tea.Model, tea.Cmd) {
	var audience *schema.Audience
	if m.target != nil {
		if m.target.channel != m.current() {
			m.setStatus(modeChannels, "transmit target is not in this channel; send refused (use /net)")
			return m, nil
		}
		audience = &schema.Audience{Kind: schema.AudienceKindNet, NetID: m.target.id}
	}
	return m.post(text, audience, false)
}

// post starts one send. The input is cleared only when the server accepts it.
func (m Model) post(text string, audience *schema.Audience, whisper bool) (tea.Model, tea.Cmd) {
	if m.authorID <= 0 {
		m.setStatus(modeChannels, m.noAuthorStatus("send"))
		return m, nil
	}
	m.sending = true
	m.setStatus(modeChannels, "sending…")
	return m, m.send(m.current(), m.input, text, audience, whisper)
}

// whisperCommand handles "/w <ids> <text>". The target is left alone: a
// whisper is one-shot.
func (m Model) whisperCommand(rest string) (tea.Model, tea.Cmd) {
	idList, text := splitCommand(rest)
	if idList == "" || text == "" {
		m.setStatus(modeChannels, "usage: /w <id>[,<id>...] <text>")
		return m, nil
	}
	ids, err := parsePrincipalIDs(idList)
	if err != nil {
		m.setStatus(modeChannels, "/w: "+err.Error())
		return m, nil
	}
	if m.authorID > 0 && len(ids) == 1 && ids[0] == m.authorID {
		m.setStatus(modeChannels, "/w: a whisper needs someone other than you")
		return m, nil
	}
	return m.post(text, &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: ids}, true)
}

// parsePrincipalIDs reads a comma-separated list of positive, distinct
// principal ids. It repeats internal/cli's helper of the same name, which is
// unexported and which this change may not touch.
func parsePrincipalIDs(list string) ([]int64, error) {
	var ids []int64
	seen := make(map[int64]bool)
	for _, field := range strings.Split(list, ",") {
		field = strings.TrimSpace(field)
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a positive principal id", sanitize(field))
		}
		if seen[id] {
			return nil, fmt.Errorf("principal id %d is listed twice", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	audience := schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: ids}
	if err := audience.Validate(); err != nil {
		return nil, err
	}
	return ids, nil
}

// netCommand handles "/net" (back to the channel) and "/net <name>".
func (m Model) netCommand(rest string) (tea.Model, tea.Cmd) {
	args := strings.Fields(rest)
	switch len(args) {
	case 0:
		m.target = nil
		m.input = ""
		m.setStatus(modeChannels, "transmitting to the whole channel")
		return m, nil
	case 1:
	default:
		m.setStatus(modeChannels, "usage: /net [name]")
		return m, nil
	}
	name := args[0]
	if err := schema.ValidateNetName(name); err != nil {
		m.setStatus(modeChannels, "/net: "+err.Error())
		return m, nil
	}
	if n, ok := findNet(m.nets[m.current()], name); ok {
		return m.useNet(n), nil
	}
	// Not in the roster we have: it may have been created since, so ask once
	// before calling it unknown.
	m.setStatus(modeChannels, "looking up net "+name+"…")
	return m, m.loadNets(true, name)
}

func findNet(nets []schema.NetV1, name string) (schema.NetV1, bool) {
	for _, n := range nets {
		if n.Name == name {
			return n, true
		}
	}
	return schema.NetV1{}, false
}

// useNet makes n the target unless the caller only monitors it. A caller the
// roster does not list at all is let through: the server decides, and its
// refusal is shown on send.
func (m Model) useNet(n schema.NetV1) Model {
	for _, member := range n.Members {
		if member.PrincipalID == m.authorID && member.Role == schema.NetRoleMonitor {
			m.setStatus(modeChannels, fmt.Sprintf("you only monitor net %s; you cannot transmit to it", n.Name))
			return m
		}
	}
	m.target = &netTarget{channel: m.current(), id: n.ID, name: n.Name}
	if f := strings.Fields(m.input); len(f) == 2 && f[0] == "/net" && f[1] == n.Name {
		m.input = ""
	}
	m.setStatus(modeChannels, "transmitting to net "+n.Name)
	return m
}

func (m Model) loadNets(resolve bool, want string) tea.Cmd {
	channel := m.current()
	if channel == "" {
		return nil
	}
	return func() tea.Msg {
		resp, err := m.api.ListNets(m.ctx, channel)
		return netsLoaded{channel: channel, nets: resp.Nets, err: err, resolve: resolve, want: want}
	}
}

// netsLoaded records the roster and, for a "/net <name>" that had to wait for
// it, finishes the command. A plain refresh that fails is silent: the roster
// only names badges and resolves /net, and /net reports its own failures.
func (m Model) netsLoaded(msg netsLoaded) (tea.Model, tea.Cmd) {
	if msg.err == nil {
		m.nets[msg.channel] = msg.nets
	}
	if !msg.resolve || msg.channel != m.current() {
		return m, nil
	}
	if msg.err != nil {
		m.setStatus(modeChannels, "/net: "+msg.err.Error())
		return m, nil
	}
	if n, ok := findNet(msg.nets, msg.want); ok {
		return m.useNet(n), nil
	}
	m.setStatus(modeChannels, fmt.Sprintf("no net %q in channel %s", msg.want, sanitize(msg.channel)))
	return m, nil
}

// sanitize makes server- or author-supplied text safe to put on one terminal
// line: line breaks become a visible mark, tabs a space, and every other
// control character (ESC included, so no escape sequence survives) is dropped.
func sanitize(text string) string {
	text = strings.NewReplacer("\r\n", " ↵ ", "\n", " ↵ ", "\r", " ↵ ", "\t", " ").Replace(text)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

// netNames maps net ids of channel to names, from the roster.
func (m Model) netNames(channel string) map[int64]string {
	names := make(map[int64]string, len(m.nets[channel]))
	for _, n := range m.nets[channel] {
		names[n.ID] = n.Name
	}
	return names
}

// scopeText is the inside of a message's scope badge, "" for a channel-wide
// message. A net shows its name (or its id when the roster lacks it); a
// whisper lists the participants other than the viewer. An audience kind this
// client does not know is still shown as scoped, never as channel-wide.
func (m Model) scopeText(channel string, a *schema.Audience) string {
	if a == nil {
		return ""
	}
	switch a.Kind {
	case schema.AudienceKindNet:
		if name, ok := m.netNames(channel)[a.NetID]; ok {
			return "net:" + sanitize(name)
		}
		return "net:" + strconv.FormatInt(a.NetID, 10)
	case schema.AudienceKindPrincipals:
		others := make([]string, 0, len(a.PrincipalIDs))
		for _, id := range a.PrincipalIDs {
			if m.authorID <= 0 || id != m.authorID {
				others = append(others, strconv.FormatInt(id, 10))
			}
		}
		if len(others) == 0 { // only the viewer: show the whole list rather than nothing
			for _, id := range a.PrincipalIDs {
				others = append(others, strconv.FormatInt(id, 10))
			}
		}
		return "whisper:" + strings.Join(others, ",")
	default:
		return sanitize(string(a.Kind))
	}
}

// messageLine renders one message on exactly one line of at most width cells.
//
// A message body must not be able to pass for a scope badge. That is
// guaranteed three ways, none of which depends on colour (a pipe, NO_COLOR or
// a monochrome terminal show no styling):
//  1. Position: the scope badge is the first thing on the line, before the
//     author id. Every other line starts with the author's digits, and the
//     body is always rendered after the author id, never at column 0.
//  2. One line: sanitize removes line breaks and every control character, so
//     a body cannot start a second line (or an escape sequence) of its own.
//  3. Escape: a body that starts with "[" is shown with a leading backslash,
//     so even in the body's own position it never reads as a badge or as the
//     payload badge.
func (m Model) messageLine(channel string, message schema.MessageV2, width int) string {
	badge, badgeWidth := "", 0
	if inner := m.scopeText(channel, message.Audience); inner != "" {
		text := "[" + clip(inner, width/3) + "]"
		badgeWidth = utf8.RuneCountInString(text) + 1
		badge = badgeStyle.Render(text) + " "
	}
	payload, payloadWidth := "", 0
	if message.Payload != nil {
		text := "[" + clip(sanitize(message.Payload.Schema), width/3) + "]"
		payloadWidth = utf8.RuneCountInString(text) + 1
		payload = " " + badgeStyle.Render(text)
	}
	author := strconv.FormatInt(message.AuthorID, 10)
	body := sanitize(message.Body)
	if strings.HasPrefix(body, "[") {
		body = "\\" + body
	}
	bodyWidth := width - utf8.RuneCountInString(author) - badgeWidth - payloadWidth - 7
	return badge + author + payload + "  " + clip(body, bodyWidth)
}

// promptWithin is prompt fitted to max cells. Only the channel name is ever
// shortened: a clipped prompt that lost its "/net" would read as the whole
// channel, which is the one mistake the prompt exists to prevent. On a
// terminal too narrow for even that, the net is what stays.
func (m Model) promptWithin(maxWidth int) string {
	full := m.prompt()
	if m.target == nil || utf8.RuneCountInString(full) <= maxWidth {
		return clip(full, maxWidth)
	}
	suffix := "/" + sanitize(m.target.name) + " >"
	room := maxWidth - utf8.RuneCountInString(suffix)
	if room < 1 {
		room = 1
	}
	return clip(sanitize(m.target.channel), room) + suffix
}

// prompt is the input prompt of the channel pane. It always names where the
// next plain message goes: the channel, or channel/net. The channel name comes
// from the server, so it is sanitized like everything else shown.
func (m Model) prompt() string {
	channel := sanitize(m.current())
	if channel == "" {
		channel = "(no channel)"
	}
	if m.target != nil {
		return sanitize(m.target.channel) + "/" + sanitize(m.target.name) + " >"
	}
	return channel + " >"
}
