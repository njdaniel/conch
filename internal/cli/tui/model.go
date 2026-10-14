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
	// SubscribeV2After sends the visible messages with an id above the given
	// one, then carries on live. The TUI loads a page over REST first and
	// follows from the last id it holds, so a reconnect needs no reload.
	// opened is called once the server has accepted the socket.
	SubscribeV2After(ctx context.Context, channel string, after int64, opened func(), receive func(schema.MessageV2) error) error
	ListNets(context.Context, string) (schema.ListNetsResponseV1, error)
	ListChannelMembers(context.Context, string) (schema.ListChannelMembersResponseV1, error)
	ListChannels(context.Context) (schema.ListChannelsResponse, error)
	ListApprovals(context.Context) (schema.ListApprovalsResponseV1, error)
	CastDecision(context.Context, int64, schema.CastDecisionRequestV1) (schema.CastDecisionResponseV1, error)
	WhoAmI(context.Context) (schema.WhoAmIResponseV1, error)
	// The session calls: the TUI's own session, the events socket that keeps
	// it online and carries the roster, and the runs of a channel.
	CreateSession(context.Context, schema.CreateSessionRequestV1) (schema.SessionV1, error)
	EndSession(context.Context, int64) error
	SubscribeEvents(ctx context.Context, channel string, session int64, receive func(schema.SessionRosterV1) error) error
	ListRuns(ctx context.Context, channel string, session, after int64, limit int) (schema.ListRunsResponseV1, error)
}

type channelsLoaded struct {
	channels []schema.ChannelV0
	err      error
}
type messagesLoaded struct {
	channel  string
	messages []schema.MessageV2
	err      error
	// following is set on a load that opens the channel's message socket once
	// the page is in (loadAndFollow); when that load fails there is no socket
	// and the model retries it.
	following bool
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

// membersLoaded carries a channel's member list, which names the authors of its
// messages.
type membersLoaded struct {
	channel string
	members []schema.ChannelMemberV1
	err     error
}

// netTarget is where plain messages go instead of the whole channel.
type netTarget struct {
	channel string
	id      int64
	name    string
}

// subscriptionOpened says the server accepted channel's message socket. A
// resumed stream may have nothing to send for a long time, so this, not a
// message, is what ends "reconnecting".
type subscriptionOpened struct{ channel string }
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
	// modeAgents follows modeDecision so the modes that existed keep their
	// values; tab reaches it from the inbox.
	modeAgents
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
	// bodyOffset is the first line of the selected approval's body shown in
	// the details pane. Only pgup/pgdn move it, and it is back to 0 whenever
	// the selected approval changes.
	bodyOffset int
	// scroll is how many lines the message pane is scrolled up from the newest
	// message of the selected channel; 0 follows the newest. It is counted from
	// the bottom so that a message arriving while it is above 0 can move it
	// along (addMessages) and the lines on screen stay where they are.
	scroll int
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
	// members is each channel's member names as last loaded, by principal id.
	// A channel whose list never loaded has no entry, and its authors show as
	// numbers.
	members map[string]map[int64]schema.ChannelMemberV1
	// now and loc give the clock and the time zone message times are shown in.
	// Injectable so tests do not depend on the machine or on today's date; a nil
	// loc means the local zone.
	now func() time.Time
	loc *time.Location
	// target is the net plain messages are sent to; nil means the channel.
	// It is never carried across channels, and nothing ever falls back from a
	// scoped send to a channel-wide one.
	target *netTarget
	// pendingNet is the name a "/net <name>" lookup is waiting on, "" for
	// none. Only the lookup the user last asked for may change the target: a
	// later "/net", another "/net <name>" or a channel switch clears or
	// replaces it, and a result that arrives after that is only a roster
	// refresh.
	pendingNet string
	// sending is true while a post is in flight, so a second enter cannot
	// send the same text twice.
	sending bool
	// whisperNoted records that the audit-log notice was shown this session.
	whisperNoted bool
	// after schedules msg to be delivered once d has elapsed. Injectable so
	// tests can observe the delay instead of sleeping through it.
	after func(d time.Duration, msg tea.Msg) tea.Cmd
	// voice is the selected channel's presence subscription and roster. It is
	// display only and never gets in the way of typing or messages.
	voice voiceModel
	// wrapped holds the selected approval's body as last laid out. It is a
	// pointer so View, which cannot change the model, can fill it: a body
	// can be megabytes, too much to lay out again on every keystroke.
	wrapped *wrappedBody
	// own is the TUI's own session, shared by every copy of the model so that
	// Run can end it after the program has returned. session is its id once
	// created; sessionFailed says creating it failed, which leaves the roster
	// unavailable without stopping anything else.
	own           *ownSession
	session       int64
	sessionFailed bool
	// ev is the events socket of the selected channel and its roster. Roster
	// frames change nothing but this: not the draft, not the selection.
	ev eventsModel
	// loaded marks channels whose first page of history has arrived: from then
	// on the message socket is opened after the stream mark, with no reload.
	loaded map[string]bool
	// streamed is, per channel, the highest message id that came from the
	// server's stream of the channel: a loaded page or a subscription frame.
	// A dropped subscription resumes after it. A message this client sent
	// is not counted: its reply can carry a higher id than messages others
	// posted while the socket was down, and resuming after it would skip
	// them.
	streamed map[string]int64
	// selAgent is the principal id of the selected agent in the Agents view,
	// and expanded the agents whose sessions are shown.
	selAgent int64
	expanded map[int64]bool
}

// setStatus records status for mode; modeDecision shares the inbox's.
func (m *Model) setStatus(mode mode, status string) {
	status = sanitize(status)
	if mode == modeChannels {
		m.channelStatus = status
		return
	}
	m.inboxStatus = status
}

// status is the status line of the mode the user is in.
func (m Model) status() string {
	switch m.mode {
	case modeChannels:
		return m.channelStatus
	case modeAgents:
		// The mode is named, and so is the channel it is about.
		return "agents: " + sanitize(m.current())
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
		messages: make(map[string][]schema.MessageV2), nets: make(map[string][]schema.NetV1), members: make(map[string]map[int64]schema.ChannelMemberV1), now: time.Now, subscribed: make(map[string]bool),
		events: make(chan tea.Msg, 64), mode: modeChannels,
		backoff: make(map[string]time.Duration), retryPending: make(map[string]bool), after: tickAfter, wrapped: &wrappedBody{},
		own: &ownSession{}, loaded: make(map[string]bool), streamed: make(map[string]int64), expanded: make(map[int64]bool)}
	if v, ok := api.(VoiceAPI); ok {
		m.voice.api = v
	}
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
		return tea.Batch(who, m.startSession(), m.loadChannels(), m.waitEvent())
	}
	return tea.Batch(who, m.startSession(), m.loadAndFollow(), m.loadNets(false, ""), m.loadMembers(), m.kickVoice(), m.waitEvent())
}

// Update applies keyboard, window, and injected API-result messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.MouseMsg:
		return m.mouse(msg)
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
				m.mode = modeAgents
				return m, nil
			case modeAgents:
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
				m.bodyOffset = 0
				return m, nil
			case modeDecision:
				m.selOption = max(0, m.selOption-1)
				return m, nil
			case modeAgents:
				return m.moveAgent(-1), nil
			}
		case "down":
			switch m.mode {
			case modeChannels:
				return m.selectChannel(1)
			case modeInbox:
				if len(m.approvals) > 0 {
					m.selApproval = min(len(m.approvals)-1, m.selApproval+1)
				}
				m.bodyOffset = 0
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
			case modeAgents:
				return m.moveAgent(1), nil
			}
		case "pgup", "pgdown":
			if m.mode == modeInbox || m.mode == modeDecision {
				return m.scrollBody(msg.String() == "pgdown"), nil
			}
			if m.mode == modeChannels {
				step := max(1, m.layout().messageRows()-1)
				if msg.String() == "pgdown" {
					step = -step
				}
				return m.scrollMessages(step), nil
			}
		case "enter":
			switch m.mode {
			case modeChannels:
				return m.submit()
			case modeAgents:
				return m.toggleAgent(), nil
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
				// Only an option the person can see may be cast: on a terminal
				// too small to lay the decision out, nothing is chosen.
				if d, ok := m.selectedDetails(); !ok || !d.optionShown {
					m.setStatus(modeInbox, "terminal too small to decide; enlarge it")
					return m, nil
				}
				opt := app.Options[m.selOption]
				m.input = ""
				m.setStatus(modeInbox, "casting decision…")
				return m, m.castDecision(app.ID, opt.ID, reason)
			}
		case "backspace":
			// The Agents view has no input: the draft is the channel's.
			if m.input != "" && m.mode != modeAgents {
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
	case voiceKick:
		return m, m.beginVoice()
	case voicePresenceReceived:
		return m.voicePresence(msg)
	case voiceEnded:
		return m.voiceEnded(msg)
	case voiceRetryDue:
		return m.voiceRetry(msg)
	case sessionStarted:
		return m.sessionStarted(msg)
	case eventsRosterReceived:
		return m.eventsRoster(msg)
	case eventsEnded:
		return m.eventsEnded(msg)
	case eventsRetryDue:
		return m.eventsRetry(msg)
	case whoAmILoaded:
		if msg.err != nil {
			m.whoErr = msg.err
			m.setStatus(modeChannels, whoErrText(msg.err))
			return m, nil
		}
		m.whoErr = nil
		m.authorID = msg.who.ID
		m.userName = msg.who.Name
		m.setStatus(modeChannels, "signed in as "+sanitize(msg.who.Name))
	case approvalsLoaded:
		if msg.err != nil {
			m.setStatus(modeInbox, msg.err.Error())
		} else {
			m.approvals = msg.approvals
			m.selApproval = 0
			m.bodyOffset = 0
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
		// The channel on screen may have changed under the user.
		m.target = nil
		m.pendingNet = ""
		m.subscribed[names[0]] = true
		return m, tea.Batch(m.loadAndFollow(), m.loadNets(false, ""), m.loadMembers(), m.beginVoice(), m.beginEvents())
	case netsLoaded:
		return m.netsLoaded(msg)
	case membersLoaded:
		// A failure changes nothing: names already known stay, and without any
		// the authors show as numbers. The next open or reconnect asks again.
		if msg.err == nil {
			names := make(map[int64]schema.ChannelMemberV1, len(msg.members))
			for _, member := range msg.members {
				names[member.PrincipalID] = member
			}
			m.members[msg.channel] = names
		}
	case messagesLoaded:
		// The result of a channel the user has since left must not overwrite
		// the status of the channel they are looking at.
		stale := msg.channel != m.current()
		if msg.err != nil {
			// The socket was to open after this page: without it there is
			// nothing to follow from, so it retries like a dropped one.
			var retry tea.Cmd
			if msg.following {
				m.subscribed[msg.channel] = false
				if !stale {
					retry = m.scheduleResubscribe(msg.channel)
				}
			}
			if stale {
				return m, retry
			}
			// A channel-list notice explains the failure better than the
			// fallback channel's own load error does.
			if m.notice != "" {
				m.setStatus(modeChannels, m.notice)
			} else {
				m.setStatus(modeChannels, msg.err.Error())
			}
			return m, retry
		}
		m.loaded[msg.channel] = true
		m.addMessages(msg.channel, msg.messages)
		m.markStreamed(msg.channel, msg.messages...)
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
		return m, nil
	case messageReceived:
		// Data flowing proves the subscription is healthy.
		delete(m.backoff, msg.channel)
		m.addMessages(msg.channel, []schema.MessageV2{msg.message})
		m.markStreamed(msg.channel, msg.message)
		return m, m.waitEvent()
	case subscriptionOpened:
		if msg.channel == m.current() && m.channelStatus == statusReconnecting && m.notice == "" {
			m.setStatus(modeChannels, "connected")
		}
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
		// Resume from the last id held: the server sends what came after it,
		// so nothing is missed and the channel is not loaded again. Only a
		// channel whose first page never arrived loads it now.
		if !m.loaded[msg.channel] {
			return m, tea.Batch(m.loadAndFollow(), m.loadMembers())
		}
		return m, tea.Batch(m.loadMembers(), m.startSubscription(msg.channel))
	case messageSent:
		m.sending = false
		if msg.err != nil {
			// The typed text stays so it can be fixed or retried by hand;
			// nothing else is sent in its place. The error text comes from
			// the server, so it is made safe for the terminal like a body.
			m.setStatus(modeChannels, sanitize(msg.err.Error()))
			return m, nil
		}
		if msg.message.ID > 0 {
			// The same message also arrives on the subscription; ids dedupe.
			m.addMessages(msg.channel, []schema.MessageV2{msg.message})
		}
		// What was sent leaves the input; anything typed after enter stays.
		m.input = strings.TrimPrefix(m.input, msg.raw)
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
	return m, tea.Batch(wait, m.scheduleResubscribe(msg.channel))
}

// scheduleResubscribe waits out the channel's backoff delay, which doubles up
// to a limit, and then asks to open the subscription again.
func (m Model) scheduleResubscribe(channel string) tea.Cmd {
	delay, ok := m.backoff[channel]
	if !ok {
		delay = minResubscribeDelay
	}
	m.backoff[channel] = delay * 2
	if m.backoff[channel] > maxResubscribeDelay {
		m.backoff[channel] = maxResubscribeDelay
	}
	m.retryPending[channel] = true
	return m.after(delay, resubscribeDue{channel: channel})
}

func (m Model) selectChannel(delta int) (tea.Model, tea.Cmd) {
	next := m.selected + delta
	if next < 0 || next >= len(m.channels) || next == m.selected {
		return m, nil
	}
	m.selected = next
	m.scroll = 0
	// A target belongs to the channel it was chosen in.
	m.target = nil
	m.pendingNet = ""
	m.setStatus(modeChannels, "loading…")
	commands := []tea.Cmd{m.loadNets(false, ""), m.loadMembers(), m.beginVoice(), m.beginEvents()}
	// A pending retry timer will start the subscription itself.
	switch {
	case m.subscribed[m.current()] || m.retryPending[m.current()]:
		commands = append(commands, m.loadCurrent())
	case m.loaded[m.current()]:
		// Held from an earlier visit: follow from the last id held while the
		// page is brought up to date.
		m.subscribed[m.current()] = true
		commands = append(commands, m.loadCurrent(), m.startSubscription(m.current()))
	default:
		m.subscribed[m.current()] = true
		commands = append(commands, m.loadAndFollow())
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

// loadAndFollow loads the selected channel's history over REST and, once the
// page is in, opens the live subscription from the last id on it: the page and
// the socket meet with no gap and no replay of what the page holds.
func (m Model) loadAndFollow() tea.Cmd {
	channel := m.current()
	load := m.loadCurrent()
	if load == nil {
		return nil
	}
	return func() tea.Msg {
		loaded := load().(messagesLoaded)
		loaded.following = true
		if loaded.err == nil {
			var after int64
			for _, message := range loaded.messages {
				if message.ID > after {
					after = message.ID
				}
			}
			m.follow(channel, after)
		}
		return loaded
	}
}

// lastID is where a dropped subscription of channel resumes: the highest id
// the server streamed for it (streamed), 0 for none.
func (m Model) lastID(channel string) int64 {
	return m.streamed[channel]
}

// markStreamed raises channel's stream mark to the highest id in messages.
func (m *Model) markStreamed(channel string, messages ...schema.MessageV2) {
	for _, message := range messages {
		if message.ID > m.streamed[channel] {
			m.streamed[channel] = message.ID
		}
	}
}

// startSubscription follows channel from the last message held. A socket that
// dropped is opened the same way, so it asks for exactly what it missed.
func (m Model) startSubscription(channel string) tea.Cmd {
	if channel == "" {
		return nil
	}
	after := m.lastID(channel)
	return func() tea.Msg {
		m.follow(channel, after)
		return nil
	}
}

// follow opens channel's message socket after the message with id after, in a
// goroutine that reports every message, and its end, as events.
func (m Model) follow(channel string, after int64) {
	go func() {
		started := time.Now()
		opened := func() {
			select {
			case m.events <- subscriptionOpened{channel: channel}:
			case <-m.ctx.Done():
			}
		}
		err := m.api.SubscribeV2After(m.ctx, channel, after, opened, func(message schema.MessageV2) error {
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
	if w, h := m.size(); w < minWidth || h < minHeight {
		return tooSmallScreen(w, h)
	}
	l := m.layout()
	width, height := l.width, l.height
	leftWidth, rightWidth, contentHeight := l.leftW, l.rightW, l.contentH
	voice := ""
	if l.members != "" {
		voice = statusStyle.Width(width).Render(l.members) + "\n"
	}
	if l.voice != "" {
		voice += statusStyle.Width(width).Render(l.voice) + "\n"
	}

	var panes string
	switch m.mode {
	case modeInbox, modeDecision:
		if width < minApprovalWidth || height < minApprovalHeight {
			return tooSmall(width, height)
		}
		inbox := borderStyle.Width(leftWidth - 2).Height(contentHeight).Render(strings.Join(m.inboxLines(leftWidth-2, contentHeight), "\n"))
		var detailsLines []string
		if d, ok := m.selectedDetails(); ok {
			detailsLines = d.lines
		}
		details := borderStyle.Width(rightWidth - 2).Height(contentHeight).Render(strings.Join(detailsLines, "\n"))
		panes = lipgloss.JoinHorizontal(lipgloss.Top, inbox, details)
	case modeAgents:
		panes = m.agentsPanes(l)
	default:
		channels := borderStyle.Width(leftWidth - 2).Height(contentHeight).Render(strings.Join(m.channelLines(leftWidth), "\n"))
		messageLines := make([]string, 0, len(m.messages[m.current()]))
		for _, message := range m.messages[m.current()] {
			messageLines = append(messageLines, m.messageLine(m.current(), message, rightWidth))
		}
		if len(messageLines) == 0 {
			messageLines = append(messageLines, statusStyle.Render("No messages"))
		}
		visible := l.messageRows()
		if len(messageLines) > visible {
			end := len(messageLines) - min(m.scroll, len(messageLines)-visible)
			messageLines = messageLines[end-visible : end]
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
	case modeAgents:
		statusKeys = "  ↑/↓ agents • enter sessions • tab channels • esc quit"
	case modeInbox:
		statusKeys = "  ↑/↓ approvals • enter decide • tab agents • esc quit"
	default:
		statusKeys = "  ↑/↓ channels • enter send • tab inbox • esc quit"
	}

	status := m.status()
	if m.mode == modeChannels {
		// One line whatever the message holds: on a narrow terminal it is the
		// message that is cut, never the key hints.
		status = fitCells(status, width-cellWidth(statusKeys)) + statusKeys
		if name := sanitize(m.userName); name != "" {
			// The name gets the room the status and key hints leave; when they
			// already fill the line the name goes, never the hints. lipgloss's
			// Width only pads, so without this clip a long name wraps and breaks
			// the layout.
			if room := width - utf8.RuneCountInString(status) - 3; room > 0 {
				status = clip(name, room) + " | " + status
			}
		}
	} else {
		if name := sanitize(m.userName); name != "" {
			status = name + " | " + status
		}
		// One line whatever it holds, so the panes are never pushed up off
		// the screen: the message is what is cut, never the key hints.
		status = fitCells(sanitize(status), width-cellWidth(statusKeys)) + statusKeys
		status = fitCells(status, width)
	}
	// One row at the smallest terminal too, where the key hints alone are
	// wider than the screen.
	status = statusStyle.Width(width).Render(fitCells(status, width))
	return panes + "\n" + voice + inputStr + "\n" + status
}

// channelLines is the channel list in a pane of the given outer width.
func (m Model) channelLines(leftWidth int) []string {
	lines := make([]string, len(m.channels))
	for i, channel := range m.channels {
		channel = clip(sanitize(channel), leftWidth-4)
		prefix := "  "
		if i == m.selected {
			prefix = activeStyle.Render("› ")
		}
		lines[i] = prefix + channel
	}
	if len(lines) == 0 && m.loadingChannels {
		lines = append(lines, statusStyle.Render("loading…"))
	}
	return lines
}

// agentsPanes is the Agents view: the channel list beside the agents from
// 80x24 up, the agents alone, full width, below it.
func (m Model) agentsPanes(l layout) string {
	if l.width < fullWidth || l.height < fullHeight {
		lines, sel := m.agentLines(l.width - 2)
		return borderStyle.Width(l.width - 2).Height(l.contentH).Render(strings.Join(agentsWindow(lines, sel, l.contentH), "\n"))
	}
	channels := borderStyle.Width(l.leftW - 2).Height(l.contentH).Render(strings.Join(m.channelLines(l.leftW), "\n"))
	lines, sel := m.agentLines(l.rightW - 2)
	agents := borderStyle.Width(l.rightW - 2).Height(l.contentH).Render(strings.Join(agentsWindow(lines, sel, l.contentH), "\n"))
	return lipgloss.JoinHorizontal(lipgloss.Top, channels, agents)
}

// size is the terminal size, or 80x24 before the first WindowSizeMsg.
func (m Model) size() (width, height int) {
	width, height = m.width, m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	return width, height
}

// paneWidths is the outer width of the left and right panes; together with
// the gap between them they fill the terminal's width.
func paneWidths(width int) (left, right int) {
	left = 18
	if width < 60 {
		left = 14
	}
	return left, width - left - 1
}

// layout is where everything is on the screen. View draws from it and mouse
// finds what was clicked with it, so the two cannot disagree. Rows and columns
// are 0-based screen cells.
//
// The two panes sit side by side from row 0, each in a rounded border: the
// border is row 0 and row contentH+1, and for the left pane column 0 and
// leftW-1; a pane's content is inside it. Under them come the voice line, if
// there is one, the input line and the status line.
type layout struct {
	width, height int
	// leftW and rightW are the outer widths of the panes.
	leftW, rightW int
	// contentH is the number of content rows of each pane.
	contentH int
	// voice is the voice line, "" when there is none.
	voice string
	// members is the members line, "" outside the channels view.
	members string
}

func (m Model) layout() layout {
	width, height := m.size()
	l := layout{width: width, height: height}
	l.leftW, l.rightW = paneWidths(width)
	// The voice line, when there is one, takes a row from the panes rather
	// than adding one to the screen, and it is one row however many people
	// are in voice, so the layout does not move as the roster changes.
	if m.mode == modeChannels {
		l.voice = m.voiceLine(width)
		l.members = m.membersLine(width)
	}
	l.contentH = height - 4
	if l.voice != "" {
		l.contentH--
	}
	if l.members != "" {
		l.contentH--
	}
	if l.contentH < 4 {
		l.contentH = 4
	}
	return l
}

// messageRows is how many message lines the message pane shows at once.
func (l layout) messageRows() int { return l.contentH - 1 }

// leftContent is the content row of the left pane at (x, y), if it is inside it.
func (l layout) leftContent(x, y int) (row int, ok bool) {
	if x < 1 || x > l.leftW-2 || y < 1 || y > l.contentH {
		return 0, false
	}
	return y - 1, true
}

// rightContent is the content row of the right pane at (x, y), if it is inside it.
func (l layout) rightContent(x, y int) (row int, ok bool) {
	if x < l.leftW+1 || x > l.leftW+l.rightW-2 || y < 1 || y > l.contentH {
		return 0, false
	}
	return y - 1, true
}

// inLeft and inRight say whether (x, y) is on that pane, its border included.
func (l layout) inLeft(x, y int) bool {
	return x >= 0 && x < l.leftW && y >= 0 && y < l.contentH+2
}

func (l layout) inRight(x, y int) bool {
	return x >= l.leftW && x < l.leftW+l.rightW && y >= 0 && y < l.contentH+2
}

// mouse handles one mouse event. The mouse only selects and scrolls: it never
// sends a message, casts a decision or quits. The one command it can return is
// the one selecting a channel starts (its history and subscription).
func (m Model) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if m.mode == modeAgents {
		// A list that is read with the keys; there is nothing to click.
		return m, nil
	}
	if m.mode != modeChannels {
		// Below this size View shows tooSmall, with nothing to point at.
		if width, height := m.size(); width < minApprovalWidth || height < minApprovalHeight {
			return m, nil
		}
	}
	l := m.layout()
	switch msg.Button {
	case tea.MouseButtonLeft:
		return m.click(l, msg.X, msg.Y)
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		step := 1
		if msg.Button == tea.MouseButtonWheelDown {
			step = -1
		}
		switch m.mode {
		case modeChannels:
			if l.inRight(msg.X, msg.Y) {
				return m.scrollMessages(step), nil
			}
		case modeInbox:
			if l.inLeft(msg.X, msg.Y) {
				m.selApproval = min(max(0, m.selApproval-step), max(0, len(m.approvals)-1))
				m.bodyOffset = 0
			}
		}
	}
	return m, nil
}

// click selects what is at (x, y), as the arrow keys would.
func (m Model) click(l layout, x, y int) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeChannels:
		if row, ok := l.leftContent(x, y); ok && row < len(m.channels) {
			return m.selectChannel(row - m.selected)
		}
	case modeInbox:
		if row, ok := l.leftContent(x, y); ok {
			if i, ok := m.approvalAt(row, l.contentH); ok {
				m.selApproval = i
				m.bodyOffset = 0
			}
		}
	case modeDecision:
		if row, ok := l.rightContent(x, y); ok {
			if d, ok := m.selectedDetails(); ok {
				if i, ok := d.optionRows[row]; ok {
					m.selOption = i
				}
			}
		}
	}
	return m, nil
}

// scrollMessages moves the message pane up by lines (down when negative),
// stopping at the oldest and at the newest message.
func (m Model) scrollMessages(lines int) Model {
	top := max(0, len(m.messages[m.current()])-m.layout().messageRows())
	m.scroll = min(max(min(m.scroll, top)+lines, 0), top)
	return m
}

// addMessages merges incoming into channel's history. While the pane is
// scrolled up, messages that arrive push the scroll along with them, so the
// lines on screen do not move.
func (m *Model) addMessages(channel string, incoming []schema.MessageV2) {
	before := len(m.messages[channel])
	m.messages[channel] = mergeMessages(m.messages[channel], incoming)
	if channel == m.current() && m.scroll > 0 {
		m.scroll += len(m.messages[channel]) - before
	}
}

// The smallest terminal the inbox and decision views are laid out for. On a
// smaller one they show tooSmall instead, and no decision can be cast.
const (
	minApprovalWidth  = 40
	minApprovalHeight = 14
)

// tooSmall fills the terminal, exactly, with a request to enlarge it.
func tooSmall(width, height int) string {
	lines := make([]string, height)
	text := []string{"Terminal too small for approvals.", fmt.Sprintf("Need at least %dx%d.", minApprovalWidth, minApprovalHeight), "tab: channels  esc: quit/cancel"}
	for i := range lines {
		if i < len(text) {
			lines[i] = fitCells(text[i], width)
		}
	}
	return strings.Join(lines, "\n")
}

// inboxLines is the approval list in rows lines of at most cols cells. A list
// longer than that shows the part around the selection, with how many
// approvals are above and below it.
func (m Model) inboxLines(cols, rows int) []string {
	if len(m.approvals) == 0 {
		return []string{statusStyle.Render(fitCells("No pending approvals", cols))}
	}
	start, end, marks := m.inboxWindow(rows)
	lines := make([]string, 0, rows)
	if marks {
		lines = append(lines, moreMark("↑", start, "", cols))
	}
	for i := start; i < end; i++ {
		app := m.approvals[i]
		prefix := "  "
		if i == m.selApproval {
			prefix = activeStyle.Render("› ")
		}
		esc, escWidth := "", 0
		if app.State == schema.ApprovalStateEscalated {
			esc, escWidth = badgeStyle.Render(" [ESC]"), 6
		}
		title := fitCells(fmt.Sprintf("%d: %s", app.RequesterID, sanitize(app.Title)), cols-2-escWidth)
		lines = append(lines, prefix+title+esc)
	}
	if marks {
		lines = append(lines, moreMark("↓", len(m.approvals)-end, "", cols))
	}
	return lines
}

// inboxWindow is the part [start, end) of the approvals that inboxLines shows
// in rows lines, and whether it has a mark line above and below it.
func (m Model) inboxWindow(rows int) (start, end int, marks bool) {
	start, end = 0, len(m.approvals)
	if len(m.approvals) > rows {
		marks = true
		start, end = window(len(m.approvals), m.selApproval, rows-2)
	}
	return start, end, marks
}

// approvalAt is the approval on content row of the inbox list, if there is
// one: a mark line or an empty row is not.
func (m Model) approvalAt(row, rows int) (int, bool) {
	start, end, marks := m.inboxWindow(rows)
	if marks {
		row--
	}
	if row < 0 || start+row >= end {
		return 0, false
	}
	return start + row, true
}

// moreMark is the line standing for n items that are not shown, or an empty
// line when there are none.
func moreMark(arrow string, n int, what string, cols int) string {
	if n <= 0 {
		return ""
	}
	return statusStyle.Render(fitCells(fmt.Sprintf("%s %d more%s", arrow, n, what), cols))
}

// window is the part [start, end) of n items, rows long, that shows sel.
func window(n, sel, rows int) (start, end int) {
	if n == 0 {
		return 0, 0
	}
	rows = max(1, min(rows, n))
	if sel >= rows {
		start = min(sel-rows+1, n-rows)
	}
	return start, start + rows
}

// details is the details pane of one approval as it is drawn.
type details struct {
	lines []string
	// bodyRows is how many body lines are shown at once, maxOffset the
	// largest useful bodyOffset (0 when the whole body is shown).
	bodyRows, maxOffset int
	// optionShown is true when, in decision mode, the selected option is one
	// of the lines.
	optionShown bool
	// optionRows maps a line of lines to the option it shows, in decision
	// mode; mark lines are not in it.
	optionRows map[int]int
}

// bodyGutter starts every line of an approval's body. Nothing else in the
// pane starts with it, so a body cannot pass for the title, the requester,
// a mark, or an option, whatever text it contains.
const bodyGutter = "┊ "

// selectedDetails lays out the selected approval for the current terminal.
// View draws it and Update acts on it, so a key can only reach what is on the
// screen. ok is false when nothing is selected or the terminal is too small.
func (m Model) selectedDetails() (details, bool) {
	width, height := m.size()
	if len(m.approvals) == 0 || m.selApproval < 0 || m.selApproval >= len(m.approvals) ||
		width < minApprovalWidth || height < minApprovalHeight {
		return details{}, false
	}
	_, rightWidth := paneWidths(width)
	return m.approvalDetails(m.approvals[m.selApproval], rightWidth-2, height-4)
}

// approvalDetails lays out app in rows lines of at most cols cells. The
// title, the requester, the deadline and the options are always shown, in the
// same places; the body gets the rows left over. A body that does not fit is
// shown from bodyOffset, followed by a line saying how much is not shown, and
// options that do not fit are shown around the selected one, between lines
// saying how many are above and below. ok is false if even that does not fit.
func (m Model) approvalDetails(app schema.ApprovalV1, cols, rows int) (details, bool) {
	header := []string{activeStyle.Render(fitCells(sanitize(app.Title), cols))}
	requester := fmt.Sprintf("Requester: %d", app.RequesterID)
	deadline := "Deadline: " + app.Deadline.Time().Format("Jan 02 15:04")
	if both := requester + "  " + deadline; cellWidth(both) <= cols {
		header = append(header, both)
	} else {
		header = append(header, fitCells(requester, cols), fitCells(deadline, cols))
	}
	if app.Payload != nil {
		header = append(header, badgeStyle.Render("["+fitCells(sanitize(app.Payload.Schema), cols-2)+"]"))
	}

	decision := m.mode == modeDecision
	optHead := 0
	if decision {
		optHead = 1
	}
	body := m.wrapped.lines(app.Body, cols-cellWidth(bodyGutter))
	avail := rows - len(header)
	bodyRows, optRows := avail-optHead-len(app.Options), len(app.Options)
	windowed := bodyRows < min(len(body), 2)
	if windowed {
		// Every option cannot be shown with room for the body: the body gets
		// two lines (one and the mark), the options the rest, less two marks.
		bodyRows = min(len(body), 2)
		optRows = avail - bodyRows - optHead - 2
		if optRows < 1 {
			return details{}, false
		}
	}

	d := details{lines: header, bodyRows: bodyRows}
	switch {
	case len(body) <= bodyRows:
		spare := bodyRows - len(body)
		if spare >= 2 && len(body) > 0 {
			d.lines = append(d.lines, "")
		}
		d.lines = append(d.lines, gutter(body)...)
		if spare >= 1 {
			d.lines = append(d.lines, "")
		}
	default:
		shown := bodyRows - 1
		d.bodyRows, d.maxOffset = shown, len(body)-shown
		offset := min(max(m.bodyOffset, 0), d.maxOffset)
		d.lines = append(d.lines, gutter(body[offset:offset+shown])...)
		d.lines = append(d.lines, bodyMark(offset, len(body)-offset-shown, cols))
	}

	if decision {
		d.lines = append(d.lines, activeStyle.Render("Decision Options:"))
	}
	sel := 0
	if decision {
		sel = m.selOption
	}
	start, end := window(len(app.Options), sel, optRows)
	if windowed {
		d.lines = append(d.lines, moreMark("↑", start, " options", cols))
	}
	for i := start; i < end; i++ {
		label := sanitize(app.Options[i].Label)
		if decision {
			if d.optionRows == nil {
				d.optionRows = make(map[int]int)
			}
			d.optionRows[len(d.lines)] = i
		}
		switch {
		case !decision:
			d.lines = append(d.lines, "  - "+fitCells(label, cols-4))
		case i == m.selOption:
			d.optionShown = true
			d.lines = append(d.lines, activeStyle.Render("› ")+fitCells(label, cols-2))
		default:
			d.lines = append(d.lines, "  "+fitCells(label, cols-2))
		}
	}
	if windowed {
		d.lines = append(d.lines, moreMark("↓", len(app.Options)-end, " options", cols))
	}
	return d, true
}

// bodyMark says how much of a cut body is above and below what is shown.
func bodyMark(above, below, cols int) string {
	parts := []string{}
	if below > 0 {
		parts = append(parts, fmt.Sprintf("%d more lines", below))
	}
	if above > 0 {
		parts = append(parts, fmt.Sprintf("%d above", above))
	}
	return statusStyle.Render(fitCells("… "+strings.Join(parts, ", ")+" (pgup/pgdn)", cols))
}

// bodyLines is an approval body as lines of at most cols cells: its own line
// breaks are kept, long lines are wrapped, and control characters removed.
// wrappedBody is bodyLines remembered for the last body and width asked for.
type wrappedBody struct {
	body   string
	cols   int
	result []string
	valid  bool
}

// lines is bodyLines(body, cols), laid out again only when either changed. A
// nil cache (a Model not made by NewModel) lays it out every time.
func (w *wrappedBody) lines(body string, cols int) []string {
	if w == nil {
		return bodyLines(body, cols)
	}
	if !w.valid || w.cols != cols || w.body != body {
		*w = wrappedBody{body: body, cols: cols, result: bodyLines(body, cols), valid: true}
	}
	return w.result
}

func bodyLines(body string, cols int) []string {
	body = sanitizeMultiline(body)
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		lines = append(lines, wrapCells(line, cols)...)
	}
	return lines
}

// gutter is body lines as drawn, each behind bodyGutter. Only the lines on
// screen are styled: a long body would otherwise cost a render per line.
func gutter(lines []string) []string {
	mark := statusStyle.Render(bodyGutter)
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = mark + line
	}
	return out
}

// scrollBody moves the selected approval's body one page down (or up).
func (m Model) scrollBody(down bool) Model {
	d, ok := m.selectedDetails()
	if !ok || d.maxOffset == 0 {
		return m
	}
	step := max(1, d.bodyRows-1)
	if !down {
		step = -step
	}
	m.bodyOffset = min(max(m.bodyOffset+step, 0), d.maxOffset)
	return m
}

// runeCells is how many cells r takes on a terminal, rounded up: never less
// than lipgloss measures it as, so text fitted with it cannot wrap.
func runeCells(r rune) int {
	if r < utf8.RuneSelf {
		return 1
	}
	return max(1, lipgloss.Width(string(r)))
}

// cellWidth is the width of s as runeCells counts it.
func cellWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeCells(r)
	}
	return n
}

// fitCells is s in at most width cells, ending in "…" when it was cut.
func fitCells(s string, width int) string {
	if width < 1 {
		return ""
	}
	if cellWidth(s) <= width {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		if used+runeCells(r) > width-1 {
			break
		}
		used += runeCells(r)
		b.WriteRune(r)
	}
	return b.String() + "…"
}

// wrapCells cuts s into lines of at most width cells; "" is one empty line.
func wrapCells(s string, width int) []string {
	width = max(width, 1)
	if cellWidth(s) <= width {
		return []string{s}
	}
	var lines []string
	var b strings.Builder
	used := 0
	for _, r := range s {
		if w := runeCells(r); used+w > width && used > 0 {
			lines = append(lines, b.String())
			b.Reset()
			used = 0
		}
		used += runeCells(r)
		b.WriteRune(r)
	}
	return append(lines, b.String())
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
		m.pendingNet = ""
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
		m.pendingNet = ""
		return m.useNet(n), nil
	}
	// Not in the roster we have: it may have been created since, so ask once
	// before calling it unknown.
	m.pendingNet = name
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

// loadMembers asks for the selected channel's member list. It is its own
// command, so a slow or refused list never holds up messages; its failure is
// dropped in Update.
func (m Model) loadMembers() tea.Cmd {
	channel := m.current()
	if channel == "" {
		return nil
	}
	return func() tea.Msg {
		resp, err := m.api.ListChannelMembers(m.ctx, channel)
		return membersLoaded{channel: channel, members: resp.Members, err: err}
	}
}

// netsLoaded records the roster and, for a "/net <name>" that had to wait for
// it, finishes the command. A plain refresh that fails is silent: the roster
// only names badges and resolves /net, and /net reports its own failures.
func (m Model) netsLoaded(msg netsLoaded) (tea.Model, tea.Cmd) {
	if msg.err == nil {
		m.nets[msg.channel] = msg.nets
	}
	// A lookup the user has since overridden changes nothing but the roster.
	if !msg.resolve || msg.channel != m.current() || msg.want != m.pendingNet {
		return m, nil
	}
	m.pendingNet = ""
	if msg.err != nil {
		m.setStatus(modeChannels, "/net: "+sanitize(msg.err.Error()))
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
// control character (ESC included, so no escape sequence survives) or bidi
// control is dropped. Dropping Bidi_Control characters deliberately strips
// all direction marks (including LRM, RLM) for safety against display spoofing.
func sanitize(text string) string {
	text = strings.NewReplacer("\r\n", " ↵ ", "\n", " ↵ ", "\r", " ↵ ", "\u2028", " ↵ ", "\u2029", " ↵ ", "\t", " ").Replace(text)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, text)
}

// sanitizeMultiline strips control characters and bidi controls from
// multiline text while preserving newlines. Tabs are mapped to a space,
// and lone carriage returns as well as U+2028 and U+2029 are normalized to newlines.
func sanitizeMultiline(text string) string {
	text = strings.NewReplacer(
		"\r\n", "\n",
		"\r", "\n",
		"\u2028", "\n",
		"\u2029", "\n",
		"\t", " ",
	).Replace(text)
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
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
		// Never empty: an empty badge would show a scoped message as open.
		if kind := sanitize(string(a.Kind)); strings.TrimSpace(kind) != "" {
			return kind
		}
		return "scoped"
	}
}

// messageLine renders one message on exactly one line of at most width cells:
//
//	[scope] HH:MM name [agent] [payload]: body
//
// A message body or an author name must not be able to pass for a scope badge,
// the agent badge or the payload badge. That is guaranteed three ways, none of
// which depends on colour (a pipe, NO_COLOR or a monochrome terminal show no
// styling):
//  1. Position: the scope badge is the first thing on the line, before the
//     time and the author. Every other line starts with the time or the author,
//     and the body is always rendered after the author, never at column 0.
//  2. One line: sanitize removes line breaks and every control character, so
//     neither can start a second line (or an escape sequence) of its own.
//  3. Escape: a body or a name that starts with "[" is shown with a leading
//     backslash, and a name has any later "[" escaped too, so neither reads as
//     a badge, least of all as the "[agent]" badge that follows a name.
func (m Model) messageLine(channel string, message schema.MessageV2, width int) string {
	inner := width - 2 // the pane's border
	var scope, payload string
	if text := m.scopeText(channel, message.Audience); text != "" {
		scope = "[" + fitCells(text, width/3) + "]"
	}
	if message.Payload != nil {
		payload = "[" + fitCells(sanitize(message.Payload.Schema), width/3) + "]"
	}
	when := m.messageTime(message)
	name, agent := m.authorName(channel, message.AuthorID)
	agentBadge := ""
	if agent {
		agentBadge = "[agent]"
	}
	// fixed is what the line spends on everything but the name and the body:
	// the badges, the time, the separators and ": ".
	fixed := func(when string) int {
		n := 2
		for _, part := range []string{scope, when, agentBadge, payload} {
			if part != "" {
				n += cellWidth(part) + 1
			}
		}
		return n
	}
	// The name gets what the rest leaves, at most 24 cells. When that is under
	// 4 the time goes first: the name is what says who is speaking.
	nameCells := min(24, inner-fixed(when))
	if nameCells < 4 && when != "" {
		when = ""
		nameCells = min(24, inner-fixed(when))
	}
	name = fitCells(name, nameCells)

	body := sanitize(message.Body)
	if strings.HasPrefix(body, "[") {
		body = "\\" + body
	}
	plain := strings.Join(nonEmpty(scope, when, name, agentBadge, payload), " ")
	if cellWidth(plain)+2 > inner {
		// Not even the prefix fits (a very narrow pane): keep as much of it as
		// does, unstyled, and no body, so the line is still one row.
		return fitCells(plain+": ", inner)
	}
	var out strings.Builder
	for _, part := range []struct {
		text  string
		badge bool
	}{{scope, true}, {when, false}, {name, false}, {agentBadge, true}, {payload, true}} {
		switch {
		case part.text == "":
		case part.badge:
			out.WriteString(badgeStyle.Render(part.text) + " ")
		default:
			out.WriteString(part.text + " ")
		}
	}
	// The space after the last part is the one before ": " in the format, and
	// ": " itself follows the name or badge directly.
	line := strings.TrimSuffix(out.String(), " ") + ": "
	return line + fitCells(body, inner-cellWidth(plain)-2)
}

func nonEmpty(parts ...string) []string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// narrowWidth is the terminal width below which messages carry no time.
const narrowWidth = 60

// messageTime is the time a message is shown with, in the model's time zone:
// "HH:MM" for today and "MM-DD HH:MM" for another day. It is "" for a message
// without a time, and on a terminal narrower than narrowWidth, where the time
// goes before the name does.
func (m Model) messageTime(message schema.MessageV2) string {
	if width, _ := m.size(); width < narrowWidth || message.CreatedAt.IsZero() {
		return ""
	}
	loc := m.loc
	if loc == nil {
		loc = time.Local
	}
	now := m.now
	if now == nil {
		now = time.Now
	}
	at, today := message.CreatedAt.Time().In(loc), now().In(loc)
	if at.Year() == today.Year() && at.YearDay() == today.YearDay() {
		return at.Format("15:04")
	}
	return at.Format("01-02 15:04")
}

// authorName is how an author is shown: the name the member list gives, and
// whether it is an agent. An author the list does not have (they left, or it
// failed to load) is "#<id>".
func (m Model) authorName(channel string, id int64) (name string, agent bool) {
	if member, ok := m.members[channel][id]; ok {
		name = safeName(member.Name)
		agent = member.Kind == schema.PrincipalAgent
	}
	if name == "" {
		return "#" + strconv.FormatInt(id, 10), false
	}
	return name, agent
}

// safeName makes a principal's name fit to print. Names stored before the
// server's display-name rule existed can hold anything, so it goes through
// sanitize like a body, loses every Unicode format character (Cf: zero-width
// and soft-hyphen characters, tag characters and the like, which would let two
// different names look alike) and has "[" escaped so it cannot pose as a badge.
// ZWJ and ZWNJ are format characters too and go with them: a terminal does not
// shape the scripts that need them, so removing them is the conservative
// choice. A name of nothing but combining marks would draw on the space before
// it, so it counts as empty.
func safeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, sanitize(name))
	name = strings.TrimSpace(name)
	if strings.IndexFunc(name, func(r rune) bool { return !unicode.Is(unicode.Mn, r) }) < 0 {
		return ""
	}
	return strings.ReplaceAll(name, "[", "\\[")
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
