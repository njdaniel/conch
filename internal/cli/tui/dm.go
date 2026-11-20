package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// dmAPI is separate from the channel API, including its event streams. Until
// the per-person stream is available, refresh the visible REST view directly.
// Re-reading the page also picks up edits and removals after a disconnect.
type dmAPI interface {
	ListDMs(context.Context, schema.DMState, string) (schema.ListDMConversationsResponseV1, error)
	OpenDM(context.Context, int64) (schema.DMConversationV1, error)
	GetDM(context.Context, int64) (schema.DMConversationV1, error)
	ListDMMessages(context.Context, int64, int64, int) (schema.ListDMMessagesResponseV1, error)
	SendDM(context.Context, int64, schema.PostDMMessageRequestV1) (schema.DMMessageV1, error)
	MarkDMRead(context.Context, int64, int64) (schema.DMReadStateV1, error)
}

var _ dmAPI = (*cli.Client)(nil)

type dmView struct {
	ctx    context.Context
	cancel context.CancelFunc
	busy   bool
	status string

	inbox   []schema.DMConversationV1
	index   int
	archive bool

	id           int64
	conversation schema.DMConversationV1
	page         schema.ListDMMessagesResponseV1
	before       int64
	scroll       int

	opening bool
	draft   string
	retry   schema.PostDMMessageRequestV1
}

type dmPollDue struct{ ctx context.Context }

type dmLoaded struct {
	ctx          context.Context
	inbox        []schema.DMConversationV1
	conversation schema.DMConversationV1
	page         schema.ListDMMessagesResponseV1
	err          error
}

type dmOpened struct {
	ctx          context.Context
	conversation schema.DMConversationV1
	err          error
}

type dmWritten struct {
	ctx     context.Context
	request schema.PostDMMessageRequestV1
	read    schema.DMReadStateV1
	err     error
}

const dmRefreshInterval = 15 * time.Second

func (m Model) enterDM(peerID int64) (Model, tea.Cmd) {
	if _, ok := m.api.(dmAPI); !ok {
		m.setStatus(modeChannels, "direct messages unavailable")
		return m, nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.mode = modeDM
	m.dm = dmView{ctx: ctx, cancel: cancel, status: "loading direct messages…"}
	var cmd tea.Cmd
	if peerID > 0 {
		cmd = m.openDM(peerID)
	} else {
		cmd = m.refreshDM()
	}
	return m, tea.Batch(cmd, dmPoll(ctx))
}

func dmPoll(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(dmRefreshInterval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return dmPollDue{ctx}
		}
	}
}

func (m *Model) refreshDM() tea.Cmd {
	if m.dm.busy {
		return nil
	}
	m.dm.busy = true
	view, api := m.dm, m.api.(dmAPI)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(view.ctx, dmRefreshInterval)
		defer cancel()
		result := dmLoaded{ctx: view.ctx}
		if view.id == 0 {
			result.inbox, result.err = loadDMInbox(ctx, api, view.archive)
		} else {
			result.conversation, result.err = api.GetDM(ctx, view.id)
			if result.err == nil {
				result.page, result.err = api.ListDMMessages(ctx, view.id, view.before, schema.DefaultDMPageSize)
			}
		}
		return result
	}
}

func loadDMInbox(ctx context.Context, api dmAPI, archive bool) ([]schema.DMConversationV1, error) {
	state := schema.DMStateOpen
	if archive {
		state = schema.DMStateArchived
	}
	var inbox []schema.DMConversationV1
	seenIDs := make(map[int64]bool)
	seenCursors := make(map[string]bool)
	for cursor := ""; ; {
		page, err := api.ListDMs(ctx, state, cursor)
		if err != nil {
			return nil, err
		}
		for _, conversation := range page.Conversations {
			if !seenIDs[conversation.ID] {
				seenIDs[conversation.ID] = true
				inbox = append(inbox, conversation)
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			return inbox, nil
		}
		if seenCursors[cursor] {
			return nil, fmt.Errorf("the server repeated a direct message list cursor")
		}
		seenCursors[cursor] = true
	}
}

func (m *Model) openDM(peerID int64) tea.Cmd {
	m.dm.busy = true
	m.dm.status = "opening conversation…"
	ctx, api := m.dm.ctx, m.api.(dmAPI)
	return func() tea.Msg {
		requestCtx, cancel := context.WithTimeout(ctx, dmRefreshInterval)
		defer cancel()
		conversation, err := api.OpenDM(requestCtx, peerID)
		return dmOpened{ctx, conversation, err}
	}
}

func (m Model) dmLoadDone(msg dmLoaded) Model {
	if m.mode != modeDM || m.dm.ctx != msg.ctx {
		return m
	}
	m.dm.busy = false
	if msg.err != nil {
		// Drop the cached view on a failed fetch, including a lost sign-in
		// or conversation access. The next poll re-reads it from the server.
		m.dm.inbox = nil
		m.dm.conversation = schema.DMConversationV1{}
		m.dm.page = schema.ListDMMessagesResponseV1{}
		m.dm.status = sanitize(msg.err.Error())
		return m
	}
	if m.dm.id == 0 {
		var selectedID int64
		if m.dm.index < len(m.dm.inbox) {
			selectedID = m.dm.inbox[m.dm.index].ID
		}
		m.dm.inbox = msg.inbox
		m.dm.index = min(m.dm.index, max(0, len(msg.inbox)-1))
		for i, conversation := range msg.inbox {
			if conversation.ID == selectedID {
				m.dm.index = i
				break
			}
		}
		m.dm.status = ""
	} else {
		m.dm.conversation, m.dm.page = msg.conversation, msg.page
		m.dm.scroll = min(m.dm.scroll, m.dmMaxScroll())
		m.dm.status = ""
	}
	return m
}

func (m Model) dmOpenDone(msg dmOpened) (Model, tea.Cmd) {
	if m.mode != modeDM || m.dm.ctx != msg.ctx {
		return m, nil
	}
	m.dm.busy = false
	if msg.err != nil {
		m.dm.status = sanitize(msg.err.Error())
		return m, nil
	}
	m.dm.opening, m.dm.draft = false, ""
	m.dm.id = msg.conversation.ID
	cmd := m.refreshDM()
	return m, cmd
}

func (m Model) dmWriteDone(msg dmWritten) (Model, tea.Cmd) {
	if m.mode != modeDM || m.dm.ctx != msg.ctx {
		return m, nil
	}
	m.dm.busy = false
	if msg.err != nil {
		m.dm.status = sanitize(msg.err.Error())
		return m, nil
	}
	if msg.request.ClientNonce != "" {
		if strings.TrimSpace(m.dm.draft) == msg.request.Body {
			m.dm.draft = ""
		}
		m.dm.retry = schema.PostDMMessageRequestV1{}
		m.dm.before, m.dm.scroll = 0, 0
	} else {
		m.dm.conversation.Unread = msg.read.Unread
		m.dm.conversation.LastReadID = msg.read.LastReadID
	}
	cmd := m.refreshDM()
	return m, cmd
}

func (m Model) dmKey(key tea.KeyMsg) (Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		m.dm.cancel()
		return m, tea.Quit
	case "esc":
		if m.dm.opening && !m.dm.busy {
			m.dm.opening, m.dm.draft, m.dm.status = false, "", ""
			return m, nil
		}
		if m.dm.id != 0 {
			// Supersede any in-flight result for the previous conversation,
			// and give the list a fresh poll and its own lifetime.
			archive := m.dm.archive
			m.dm.cancel()
			ctx, cancel := context.WithCancel(m.ctx)
			m.dm = dmView{ctx: ctx, cancel: cancel, archive: archive}
			cmd := m.refreshDM()
			return m, tea.Batch(cmd, dmPoll(ctx))
		}
		m.dm.cancel()
		m.dm = dmView{}
		m.mode = modeChannels
		return m, m.markReading()
	case "ctrl+l":
		cmd := m.refreshDM()
		return m, cmd
	}
	if m.dm.id != 0 {
		return m.dmHistoryKey(key)
	}
	return m.dmInboxKey(key)
}

func (m Model) dmInboxKey(key tea.KeyMsg) (Model, tea.Cmd) {
	if m.dm.opening {
		if key.String() != "enter" {
			m.dm.draft = editDMDraft(m.dm.draft, key)
			return m, nil
		}
		if m.dm.busy {
			return m, nil
		}
		id, err := strconv.ParseInt(strings.TrimSpace(m.dm.draft), 10, 64)
		if err != nil || id <= 0 {
			m.dm.status = "enter a positive principal ID"
			return m, nil
		}
		cmd := m.openDM(id)
		return m, cmd
	}
	switch key.String() {
	case "n":
		m.dm.opening, m.dm.status = true, ""
	case "a":
		if !m.dm.busy {
			m.dm.archive = !m.dm.archive
			m.dm.inbox, m.dm.index = nil, 0
			cmd := m.refreshDM()
			return m, cmd
		}
	case "up":
		m.dm.index = max(0, m.dm.index-1)
	case "down":
		m.dm.index = min(max(0, len(m.dm.inbox)-1), m.dm.index+1)
	case "enter":
		if !m.dm.busy && m.dm.index < len(m.dm.inbox) {
			m.dm.id = m.dm.inbox[m.dm.index].ID
			cmd := m.refreshDM()
			return m, cmd
		}
	}
	return m, nil
}

func (m Model) dmHistoryKey(key tea.KeyMsg) (Model, tea.Cmd) {
	switch key.String() {
	case "enter":
		return m.dmSend()
	case "ctrl+r":
		return m.dmMarkRead()
	case "up":
		m.dm.scroll = min(m.dmMaxScroll(), m.dm.scroll+1)
	case "down":
		m.dm.scroll = max(0, m.dm.scroll-1)
	case "pgup":
		if m.dm.scroll == m.dmMaxScroll() && m.dm.page.NextBefore > 0 && !m.dm.busy {
			m.dm.before = m.dm.page.NextBefore
			m.dm.page = schema.ListDMMessagesResponseV1{}
			m.dm.scroll = 0
			cmd := m.refreshDM()
			return m, cmd
		}
		m.dm.scroll = min(m.dmMaxScroll(), m.dm.scroll+m.layout().contentH)
	case "pgdown":
		m.dm.scroll = max(0, m.dm.scroll-m.layout().contentH)
	case "end":
		if m.dm.before > 0 && !m.dm.busy {
			m.dm.before, m.dm.scroll = 0, 0
			m.dm.page = schema.ListDMMessagesResponseV1{}
			cmd := m.refreshDM()
			return m, cmd
		}
		m.dm.scroll = 0
	default:
		m.dm.draft = editDMDraft(m.dm.draft, key)
	}
	return m, nil
}

func editDMDraft(draft string, key tea.KeyMsg) string {
	if key.Type == tea.KeyBackspace && draft != "" {
		clusters := textClusters(draft)
		return strings.TrimSuffix(draft, clusters[len(clusters)-1].text)
	}
	if key.Type == tea.KeyRunes || key.Type == tea.KeySpace {
		return draft + string(key.Runes)
	}
	return draft
}

func (m Model) dmSend() (Model, tea.Cmd) {
	body := strings.TrimSpace(m.dm.draft)
	if body == "" || m.dm.busy || m.dm.conversation.ID == 0 {
		return m, nil
	}
	if body != m.dm.retry.Body {
		m.dm.retry = cli.NewDMText(body)
	}
	m.dm.busy, m.dm.status = true, "sending…"
	ctx, api, id, request := m.dm.ctx, m.api.(dmAPI), m.dm.id, m.dm.retry
	return m, func() tea.Msg {
		requestCtx, cancel := context.WithTimeout(ctx, dmRefreshInterval)
		defer cancel()
		_, err := api.SendDM(requestCtx, id, request)
		return dmWritten{ctx: ctx, request: request, err: err}
	}
}

func (m Model) dmMarkRead() (Model, tea.Cmd) {
	if m.dm.busy || m.dm.before != 0 || m.dm.scroll != 0 || len(m.dm.page.Messages) == 0 {
		return m, nil
	}
	m.dm.busy, m.dm.status = true, "marking read…"
	ctx, api, id, last := m.dm.ctx, m.api.(dmAPI), m.dm.id, m.dm.page.Messages[0].ID
	return m, func() tea.Msg {
		requestCtx, cancel := context.WithTimeout(ctx, dmRefreshInterval)
		defer cancel()
		read, err := api.MarkDMRead(requestCtx, id, last)
		return dmWritten{ctx: ctx, read: read, err: err}
	}
}

func dmPeerLabel(conversation schema.DMConversationV1) string {
	name := strings.TrimSpace(sanitize(conversation.PeerName))
	if name == "" {
		name = "person"
	}
	return fmt.Sprintf("%s (#%d)", name, conversation.PeerID)
}

func (m Model) dmHistoryLines(cols int) []string {
	var lines []string
	for i := len(m.dm.page.Messages) - 1; i >= 0; i-- {
		msg := m.dm.page.Messages[i]
		author := fmt.Sprintf("#%d", msg.AuthorID)
		if msg.AuthorID == m.dm.conversation.PeerID {
			author = dmPeerLabel(m.dm.conversation)
		}
		if msg.AuthorID == m.authorID {
			author = "you"
		}
		for j, line := range strings.Split(sanitizeMultiline(msg.Body), "\n") {
			if j == 0 {
				line = author + ": " + line
			}
			lines = append(lines, wrapCells(line, cols)...)
		}
	}
	return lines
}

func (m Model) dmMaxScroll() int {
	l := m.layout()
	return max(0, len(m.dmHistoryLines(l.rightW-2))-l.contentH)
}

func (m Model) dmPanes(l layout) string {
	view := m.dm
	listTitle := "DIRECT MESSAGES"
	if view.archive {
		listTitle = "ARCHIVED DMS"
	}
	var list []string
	start, end := window(len(view.inbox), view.index, l.contentH)
	for i := start; i < end; i++ {
		conversation := view.inbox[i]
		if conversation.ID == view.conversation.ID {
			conversation = view.conversation
		}
		prefix := "  "
		if i == view.index {
			prefix = "› "
		}
		suffix := fmt.Sprintf(" · %d unread", conversation.Unread)
		list = append(list, prefix+fitCells(dmPeerLabel(conversation), l.leftW-4-cellWidth(suffix))+suffix)
	}
	if len(list) == 0 && view.id == 0 && !view.busy && view.status == "" {
		list = []string{"No conversations"}
	}
	left := paneBox(listTitle, view.id == 0, list, l.leftW, l.contentH, "")
	title, foot := "CONVERSATION", ""
	var history []string
	if view.conversation.ID > 0 {
		title = fmt.Sprintf("%d unread · %s", view.conversation.Unread, dmPeerLabel(view.conversation))
		history = m.dmHistoryLines(l.rightW - 2)
		if len(history) > 0 {
			end := len(history) - min(view.scroll, max(0, len(history)-l.contentH))
			history = history[max(0, end-l.contentH):end]
		} else if !view.busy {
			history = []string{"No messages"}
		}
		if view.page.NextBefore > 0 {
			foot = "pgup older"
		}
		if view.before > 0 {
			foot = "end latest"
		}
	}
	right := paneBox(title, view.id != 0, history, l.rightW, l.contentH, foot)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) dmPrompt(width int, cursor string) string {
	prompt := "DM >"
	switch {
	case m.dm.opening:
		prompt = "Principal ID >"
	case m.dm.conversation.ID == 0:
		return ""
	}
	return prompt + " " + clipTail(sanitize(m.dm.draft), width-cellWidth(prompt)-3) + cursor
}
