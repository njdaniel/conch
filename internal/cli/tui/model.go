// Package tui implements the interactive Conch terminal client.
package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/njdaniel/conch/pkg/schema"
)

// API is the shared Conch client surface used by the TUI.
type API interface {
	ListMessages(context.Context, string, int64, int) (schema.ListMessagesResponseV1, error)
	SendMessage(context.Context, string, int64, string) (schema.MessageV1, error)
	Subscribe(context.Context, string, func(schema.MessageV1) error) error
	ListApprovals(context.Context) (schema.ListApprovalsResponseV1, error)
	CastDecision(context.Context, int64, schema.CastDecisionRequestV1) (schema.CastDecisionResponseV1, error)
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
}
type messageSent struct{ err error }
type approvalsLoaded struct {
	approvals []schema.ApprovalV1
	err       error
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
	ctx         context.Context
	api         API
	authorID    int64
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
}

// NewModel constructs a model for the configured channels.
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
	if len(clean) == 0 {
		clean = []string{"general"}
	}
	return Model{ctx: ctx, api: api, authorID: authorID, channels: clean,
		messages: make(map[string][]schema.MessageV1), subscribed: map[string]bool{clean[0]: true},
		events: make(chan tea.Msg, 64), mode: modeChannels}
}

// Init starts REST backfill and the live subscription for the selected channel.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadCurrent(), m.startSubscription(), m.waitEvent())
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
			if m.mode == modeChannels {
				m.mode = modeInbox
				m.status = "loading approvals…"
				return m, m.loadApprovals()
			} else if m.mode == modeInbox {
				m.mode = modeChannels
				m.status = ""
				return m, nil
			}
		case "up":
			if m.mode == modeChannels {
				return m.selectChannel(-1)
			} else if m.mode == modeInbox {
				m.selApproval = max(0, m.selApproval-1)
				return m, nil
			} else if m.mode == modeDecision {
				m.selOption = max(0, m.selOption-1)
				return m, nil
			}
		case "down":
			if m.mode == modeChannels {
				return m.selectChannel(1)
			} else if m.mode == modeInbox {
				if len(m.approvals) > 0 {
					m.selApproval = min(len(m.approvals)-1, m.selApproval+1)
				}
				return m, nil
			} else if m.mode == modeDecision {
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
			if m.mode == modeChannels {
				body := strings.TrimSpace(m.input)
				if body == "" {
					return m, nil
				}
				if m.authorID <= 0 {
					m.status = "set CONCH_AUTHOR to send"
					return m, nil
				}
				m.input = ""
				m.status = "sending…"
				return m, m.send(body)
			} else if m.mode == modeInbox {
				if len(m.approvals) > 0 {
					m.mode = modeDecision
					m.selOption = 0
					m.input = ""
					m.status = "type reason to decide"
					return m, nil
				}
			} else if m.mode == modeDecision {
				reason := strings.TrimSpace(m.input)
				if reason == "" {
					m.status = "reason is required"
					return m, nil
				}
				if m.authorID <= 0 {
					m.status = "set CONCH_AUTHOR to decide"
					return m, nil
				}
				if len(m.approvals) == 0 || m.selApproval >= len(m.approvals) {
					m.status = "no approval selected"
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
	case approvalsLoaded:
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			m.approvals = msg.approvals
			m.selApproval = 0
			m.status = "inbox loaded"
		}
	case decisionCast:
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			m.status = "decision cast"
			m.mode = modeInbox
			return m, m.loadApprovals()
		}
	case messagesLoaded:
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			m.messages[msg.channel] = mergeMessages(m.messages[msg.channel], msg.messages)
			m.status = "connected"
		}
	case messageReceived:
		m.messages[msg.channel] = mergeMessages(m.messages[msg.channel], []schema.MessageV1{msg.message})
		return m, m.waitEvent()
	case subscriptionEnded:
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			m.status = "live updates: " + msg.err.Error()
		}
		return m, m.waitEvent()
	case messageSent:
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			m.status = "sent"
		}
	}
	return m, nil
}

func (m Model) selectChannel(delta int) (tea.Model, tea.Cmd) {
	next := m.selected + delta
	if next < 0 || next >= len(m.channels) || next == m.selected {
		return m, nil
	}
	m.selected = next
	m.status = "loading…"
	commands := []tea.Cmd{m.loadCurrent()}
	if !m.subscribed[m.current()] {
		m.subscribed[m.current()] = true
		commands = append(commands, m.startSubscription())
	}
	return m, tea.Batch(commands...)
}

func (m Model) current() string { return m.channels[m.selected] }

func (m Model) loadCurrent() tea.Cmd {
	channel := m.current()
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
	return func() tea.Msg {
		go func() {
			err := m.api.Subscribe(m.ctx, channel, func(message schema.MessageV1) error {
				select {
				case m.events <- messageReceived{channel: channel, message: message}:
					return nil
				case <-m.ctx.Done():
					return m.ctx.Err()
				}
			})
			select {
			case m.events <- subscriptionEnded{channel: channel, err: err}:
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
			detailsLines = append(detailsLines, "", strings.ReplaceAll(app.Body, "\n", " ↵ "), "")
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
	if m.mode == modeDecision {
		statusKeys = "  ↑/↓ options • enter confirm • esc cancel"
	} else if m.mode == modeInbox {
		statusKeys = "  ↑/↓ approvals • enter decide • tab channels • esc quit"
	} else {
		statusKeys = "  ↑/↓ channels • enter send • tab inbox • esc quit"
	}

	status := statusStyle.Width(width).Render(m.status + statusKeys)
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
