// Package tui implements the interactive Conch terminal client.
package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// API is the shared Conch client surface used by the TUI.
type API interface {
	ListMessages(context.Context, string, int64, int) (schema.ListMessagesResponseV1, error)
	SendMessage(context.Context, string, int64, string) (schema.MessageV1, error)
	Subscribe(context.Context, string, func(schema.MessageV1) error) error
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
	messages []schema.MessageV1
	err      error
}
type messageReceived struct {
	channel string
	message schema.MessageV1
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

type messageSent struct{ err error }
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
	whoErr      error
	channels    []string
	selected    int
	messages    map[string][]schema.MessageV1
	subscribed  map[string]bool
	input       string
	status      string
	width       int
	height      int
	events      chan tea.Msg
	mode        mode
	approvals   []schema.ApprovalV1
	selApproval int
	selOption   int
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
	// after schedules msg to be delivered once d has elapsed. Injectable so
	// tests can observe the delay instead of sleeping through it.
	after func(d time.Duration, msg tea.Msg) tea.Cmd
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
		messages: make(map[string][]schema.MessageV1), subscribed: make(map[string]bool),
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
	return tea.Batch(who, m.loadCurrent(), m.startSubscription(), m.waitEvent())
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
				m.status = "canceled decision"
				return m, nil
			}
			return m, tea.Quit
		case "tab":
			switch m.mode {
			case modeChannels:
				m.mode = modeInbox
				m.status = "loading approvals…"
				return m, m.loadApprovals()
			case modeInbox:
				m.mode = modeChannels
				m.status = ""
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
				body := strings.TrimSpace(m.input)
				if body == "" {
					return m, nil
				}
				if len(m.channels) == 0 {
					m.status = "no channel selected yet"
					return m, nil
				}
				if m.authorID <= 0 {
					m.status = m.noAuthorStatus("send")
					return m, nil
				}
				m.input = ""
				m.status = "sending…"
				return m, m.send(body)
			case modeInbox:
				if len(m.approvals) > 0 {
					m.mode = modeDecision
					m.selOption = 0
					m.input = ""
					m.status = "type reason to decide"
					return m, nil
				}
			case modeDecision:
				reason := strings.TrimSpace(m.input)
				if reason == "" {
					m.status = "reason is required"
					return m, nil
				}
				if m.authorID <= 0 {
					m.status = m.noAuthorStatus("decide")
					return m, nil
				}
				if len(m.approvals) == 0 || m.selApproval >= len(m.approvals) {
					m.status = "no approval selected"
					m.mode = modeInbox
					return m, nil
				}
				app := m.approvals[m.selApproval]
				if len(app.Options) == 0 || m.selOption < 0 || m.selOption >= len(app.Options) {
					m.status = "select a decision option"
					return m, nil
				}
				opt := app.Options[m.selOption]
				m.input = ""
				m.status = "casting decision…"
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
			m.status = whoErrText(msg.err)
			return m, nil
		}
		m.whoErr = nil
		m.authorID = msg.who.ID
		m.userName = msg.who.Name
		m.status = "signed in as " + msg.who.Name
	case approvalsLoaded:
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			m.approvals = msg.approvals
			m.selApproval = 0
			m.status = "inbox loaded"
			// A slower load can land after the user has already moved into
			// modeDecision on a stale (now-refreshed) list; if the refresh
			// came back empty there is nothing left to decide on.
			if m.mode == modeDecision && len(m.approvals) == 0 {
				m.mode = modeInbox
				m.status = "no open approvals"
			}
		}
	case decisionCast:
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			m.status = "decision cast"
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
		m.status = m.notice
		m.channels = names
		m.selected = 0
		m.subscribed[names[0]] = true
		return m, tea.Batch(m.loadCurrent(), m.startSubscription())
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
				m.status = m.notice
			} else {
				m.status = msg.err.Error()
			}
		} else {
			m.messages[msg.channel] = mergeMessages(m.messages[msg.channel], msg.messages)
			if stale {
				return m, nil
			}
			switch {
			case m.notice != "":
				m.status = m.notice
			case m.retryPending[msg.channel]:
				// History loaded, but the live subscription is down and a
				// retry is waiting: "connected" would be untrue.
				m.status = statusReconnecting
			default:
				m.status = "connected"
			}
		}
	case messageReceived:
		// Data flowing proves the subscription is healthy.
		delete(m.backoff, msg.channel)
		m.messages[msg.channel] = mergeMessages(m.messages[msg.channel], []schema.MessageV1{msg.message})
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
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			m.status = "sent"
		}
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
			m.status = msg.err.Error()
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
		m.status = statusReconnecting
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
	m.status = "loading…"
	commands := []tea.Cmd{m.loadCurrent()}
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
		var messages []schema.MessageV1
		var after int64
		for {
			page, err := m.api.ListMessages(m.ctx, channel, after, 100)
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
			err := m.api.Subscribe(m.ctx, channel, func(message schema.MessageV1) error {
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

func (m Model) send(body string) tea.Cmd {
	channel := m.current()
	if channel == "" {
		return nil
	}
	return func() tea.Msg {
		_, err := m.api.SendMessage(m.ctx, channel, m.authorID, body)
		return messageSent{err: err}
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

func mergeMessages(existing, incoming []schema.MessageV1) []schema.MessageV1 {
	byID := make(map[int64]schema.MessageV1, len(existing)+len(incoming))
	for _, message := range existing {
		byID[message.ID] = message
	}
	for _, message := range incoming {
		byID[message.ID] = message
	}
	merged := make([]schema.MessageV1, 0, len(byID))
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
			badge := ""
			badgeWidth := 0
			if message.Payload != nil {
				badgeText := "[" + clip(message.Payload.Schema, rightWidth/3) + "]"
				badgeWidth = utf8.RuneCountInString(badgeText) + 1
				badge = " " + badgeStyle.Render(badgeText)
			}
			prefix := fmt.Sprintf("%d", message.AuthorID)
			bodyWidth := rightWidth - utf8.RuneCountInString(prefix) - badgeWidth - 7
			messageLines = append(messageLines, fmt.Sprintf("%s%s  %s", prefix, badge, clip(strings.ReplaceAll(message.Body, "\n", " ↵ "), bodyWidth)))
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
		inputStr = lipgloss.NewStyle().Width(width).Render("> " + clipTail(m.input, width-3))
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

	status := m.status + statusKeys
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
