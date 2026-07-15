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

// Model is the root Bubble Tea model. Network results enter Update as messages,
// keeping state transitions deterministic and independently testable.
type Model struct {
	ctx        context.Context
	api        API
	authorID   int64
	channels   []string
	selected   int
	messages   map[string][]schema.MessageV1
	subscribed map[string]bool
	input      string
	status     string
	width      int
	height     int
	events     chan tea.Msg
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
		events: make(chan tea.Msg, 64)}
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
			return m, tea.Quit
		case "up":
			return m.selectChannel(-1)
		case "down":
			return m.selectChannel(1)
		case "enter":
			body := strings.TrimSpace(m.input)
			if body == "" {
				return m, nil
			}
			if cmd, ok := strings.CutPrefix(body, "/"); ok {
				m.input = ""
				m.status = m.handleSlashCommand(cmd)
				return m, nil
			}
			if m.authorID <= 0 {
				m.status = "set CONCH_AUTHOR to send"
				return m, nil
			}
			m.input = ""
			m.status = "sending…"
			return m, m.send(body)
		case "backspace":
			if m.input != "" {
				_, size := utf8.DecodeLastRuneInString(m.input)
				m.input = m.input[:len(m.input)-size]
			}
		default:
			// A lone space arrives as KeySpace, not KeyRunes; both carry Runes.
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				m.input += string(msg.Runes)
			}
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

// handleSlashCommand processes a slash command (the leading "/" has been
// stripped). It returns the status string to display.
func (m Model) handleSlashCommand(cmd string) string {
	switch strings.TrimSpace(cmd) {
	case "chronicle tips":
		return m.chronicleTips()
	default:
		return fmt.Sprintf("unknown command: /%s", cmd)
	}
}

// chronicleTips inspects the messages already loaded for the current channel
// and returns a personalised usage tip based on the author's patterns.
func (m Model) chronicleTips() string {
	msgs := m.messages[m.current()]
	total := len(msgs)
	var own, totalLen int
	for _, msg := range msgs {
		if msg.AuthorID == m.authorID {
			own++
			totalLen += len(msg.Body)
		}
	}

	switch {
	case own == 0:
		return "chronicle: no messages from you yet — type something and press Enter to send"
	case total > 0 && own*5 < total:
		return fmt.Sprintf("chronicle: you sent %d of %d messages in #%s — try ↑/↓ to explore other channels", own, total, m.current())
	case own > 0 && totalLen/own > 80:
		return fmt.Sprintf("chronicle: avg message length %d chars — longer content wraps in the messages pane", totalLen/own)
	case len(m.channels) > 1:
		return fmt.Sprintf("chronicle: %d channels available — use ↑/↓ to switch, Enter to send", len(m.channels))
	default:
		return fmt.Sprintf("chronicle: %d messages sent in #%s — Esc or Ctrl+C to quit", own, m.current())
	}
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
	panes := lipgloss.JoinHorizontal(lipgloss.Top, channels, messages)
	input := lipgloss.NewStyle().Width(width).Render("> " + clipTail(m.input, width-3))
	status := statusStyle.Width(width).Render(m.status + "  ↑/↓ channels • enter send • esc quit")
	return panes + "\n" + input + "\n" + status
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
