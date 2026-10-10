package tui

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// VoiceAPI is the part of the client that follows a channel's voice presence
// socket. It is optional: a model whose API does not provide it shows no voice
// line at all. The TUI only reads presence; it never joins voice.
type VoiceAPI interface {
	SubscribeVoicePresence(ctx context.Context, channel string, receive func(schema.VoicePresenceV1) error) error
}

// voiceKick asks Update to open the first presence subscription; Init cannot
// change the model, so it cannot do that itself.
type voiceKick struct{}

// voicePresenceReceived carries one whole-state presence document. gen tells
// which subscription sent it, so a late document from a channel the user has
// left is dropped.
type voicePresenceReceived struct {
	gen int
	doc schema.VoicePresenceV1
}

// voiceEnded is the end of one presence subscription.
type voiceEnded struct {
	gen   int
	err   error
	lived time.Duration
}

// voiceRetryDue fires when a dropped presence socket's backoff delay elapses.
type voiceRetryDue struct{ gen int }

type voiceState int

const (
	// voiceConnecting is the state before the first document: nothing is
	// shown, because the server may turn out to have no voice at all.
	voiceConnecting voiceState = iota
	// voiceOff means voice is not for this caller (not configured, no auth,
	// not allowed): nothing is shown and nothing is retried.
	voiceOff
	// voiceLive means doc is the latest presence.
	voiceLive
	// voiceReconnecting means the socket dropped after it had worked.
	voiceReconnecting
)

// voiceModel is the presence subscription of the selected channel and what it
// last said.
type voiceModel struct {
	api VoiceAPI
	// gen numbers subscriptions; only the newest one's results are kept.
	gen    int
	cancel context.CancelFunc
	state  voiceState
	doc    schema.VoicePresenceV1
	// backoff is the delay the next drop will wait; zero means the minimum.
	backoff      time.Duration
	retryPending bool
}

// beginVoice closes the running presence subscription, if any, and opens one
// for the selected channel. Switching channels always goes through here, so
// the old socket never outlives the channel it belonged to.
func (m *Model) beginVoice() tea.Cmd {
	v := &m.voice
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
	v.gen++
	v.state = voiceConnecting
	v.doc = schema.VoicePresenceV1{}
	v.backoff = 0
	v.retryPending = false
	channel := m.current()
	if v.api == nil || channel == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	v.cancel = cancel
	return m.startVoice(ctx, v.gen, channel)
}

func (m Model) startVoice(ctx context.Context, gen int, channel string) tea.Cmd {
	api, events := m.voice.api, m.events
	return func() tea.Msg {
		go func() {
			started := time.Now()
			err := api.SubscribeVoicePresence(ctx, channel, func(doc schema.VoicePresenceV1) error {
				select {
				case events <- voicePresenceReceived{gen: gen, doc: doc}:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			select {
			case events <- voiceEnded{gen: gen, err: err, lived: time.Since(started)}:
			case <-ctx.Done():
			}
		}()
		return nil
	}
}

// kickVoice is the Init command that opens the first presence subscription.
func (m Model) kickVoice() tea.Cmd {
	if m.voice.api == nil {
		return nil
	}
	return func() tea.Msg { return voiceKick{} }
}

func (m Model) voicePresence(msg voicePresenceReceived) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.voice.gen {
		return m, wait
	}
	// A document is data flowing: the socket is healthy.
	m.voice.backoff = 0
	m.voice.doc = msg.doc
	m.voice.state = voiceLive
	if !msg.doc.Configured {
		m.voice.state = voiceOff
	}
	return m, wait
}

// voiceEnded decides what the end of a presence socket means. A refusal that
// retrying cannot change turns the voice line off for good; anything else is
// retried after a backoff, on the same schedule as the message subscription.
// Nothing here touches the status line or the input.
func (m Model) voiceEnded(msg voiceEnded) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.voice.gen || errors.Is(msg.err, context.Canceled) || m.ctx.Err() != nil {
		return m, wait
	}
	v := &m.voice
	var serverErr *cli.ServerError
	switch {
	case errors.Is(msg.err, cli.ErrUnauthenticated), websocket.CloseStatus(msg.err) == websocket.StatusPolicyViolation:
		v.state = voiceOff
		return m, wait
	case errors.As(msg.err, &serverErr):
		switch {
		case serverErr.Code == schema.ErrorCodeVoiceUnavailable:
			// Configured but LiveKit cannot be reached: say so once, quietly,
			// and keep trying.
			v.doc = schema.VoicePresenceV1{Configured: true}
			v.state = voiceLive
		case serverErr.Code == schema.ErrorCodeVoiceNotConfigured, serverErr.Code == schema.ErrorCodeVoiceRequiresAuth,
			serverErr.Status >= 400 && serverErr.Status < 500:
			v.state = voiceOff
			return m, wait
		default:
			m.voiceDropped()
		}
	default:
		m.voiceDropped()
	}
	if msg.lived >= maxResubscribeDelay {
		v.backoff = 0
	}
	delay := v.backoff
	if delay <= 0 {
		delay = minResubscribeDelay
	}
	v.backoff = delay * 2
	if v.backoff > maxResubscribeDelay {
		v.backoff = maxResubscribeDelay
	}
	v.retryPending = true
	return m, tea.Batch(wait, m.after(delay, voiceRetryDue{gen: v.gen}))
}

// voiceDropped marks a socket that was working and is not any more. One that
// never worked stays out of sight: the server may simply have no voice.
func (m *Model) voiceDropped() {
	if m.voice.state == voiceLive {
		m.voice.state = voiceReconnecting
	}
}

func (m Model) voiceRetry(msg voiceRetryDue) (tea.Model, tea.Cmd) {
	if msg.gen != m.voice.gen || !m.voice.retryPending {
		return m, nil
	}
	state, doc, backoff := m.voice.state, m.voice.doc, m.voice.backoff
	cmd := m.beginVoice()
	// A retry is the same subscription continuing: what was on screen stays
	// until the new socket says something, and the backoff keeps growing.
	m.voice.state, m.voice.doc, m.voice.backoff = state, doc, backoff
	return m, cmd
}

// voiceNames maps the principals the TUI knows by name to their names. The API
// offers a member only whoami, exactly the case `conch voice status` has.
func (m Model) voiceNames() map[int64]string {
	if m.authorID > 0 && m.userName != "" {
		return map[int64]string{m.authorID: m.userName}
	}
	return nil
}

// voiceLine is the one line of voice roster, "" when nothing is to be shown.
// It never exceeds width cells.
func (m Model) voiceLine(width int) string {
	const prefix = "voice: "
	v := m.voice
	var text string
	switch v.state {
	case voiceLive:
		switch {
		case !v.doc.Available:
			text = "unavailable"
		default:
			text = m.roster(width - utf8.RuneCountInString(prefix))
		}
	case voiceReconnecting:
		text = "reconnecting"
	default:
		return ""
	}
	return clip(prefix+text, width)
}

// roster lists the people in the channel-wide voice room as `conch voice
// status` names them (id, then the name made safe for one line), a mark after whoever
// is transmitting, and "+N" for those that do not fit in width.
func (m Model) roster(width int) string {
	// Only the channel-wide room. A room with a narrower audience (a net, from
	// V5) is left out rather than merged in: listed here, its people would read
	// as being in the channel's room, and someone in both would appear twice.
	var people []schema.VoiceParticipant
	for _, room := range m.voice.doc.Rooms {
		if room.Audience != nil {
			continue
		}
		people = append(people, room.Participants...)
	}
	if len(people) == 0 {
		return "nobody"
	}
	sort.SliceStable(people, func(i, j int) bool { return people[i].PrincipalID < people[j].PrincipalID })
	names := m.voiceNames()
	const sep = "  "
	var b strings.Builder
	used := 0
	shown := 0
	for i, p := range people {
		entry := strconv.FormatInt(p.PrincipalID, 10)
		if name, ok := names[p.PrincipalID]; ok {
			entry += " " + cli.VoiceName(name)
		}
		if p.Transmitting {
			entry += "●"
		}
		cost := utf8.RuneCountInString(entry)
		if i > 0 {
			cost += len(sep)
		}
		reserve := 0
		if rest := len(people) - i - 1; rest > 0 {
			reserve = len(sep) + 1 + len(strconv.Itoa(rest))
		}
		if used+cost+reserve > width {
			break
		}
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(entry)
		used += cost
		shown++
	}
	if more := len(people) - shown; more > 0 {
		if shown > 0 {
			b.WriteString(sep)
		}
		b.WriteString("+" + strconv.Itoa(more))
	}
	return b.String()
}
