package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// voiceAPI records presence subscriptions. Each one blocks until its context
// ends, like a healthy socket.
type voiceAPI struct {
	stubAPI
	mu      sync.Mutex
	subs    []voiceSub
	started chan voiceSub
}

type voiceSub struct {
	channel string
	ctx     context.Context
}

func newVoiceAPI() *voiceAPI { return &voiceAPI{started: make(chan voiceSub, 32)} }

func (a *voiceAPI) SubscribeVoicePresence(ctx context.Context, channel string, _ func(schema.VoicePresenceV1) error) error {
	sub := voiceSub{channel: channel, ctx: ctx}
	a.mu.Lock()
	a.subs = append(a.subs, sub)
	a.mu.Unlock()
	a.started <- sub
	<-ctx.Done()
	return ctx.Err()
}

func awaitVoice(t *testing.T, a *voiceAPI, channel string) voiceSub {
	t.Helper()
	select {
	case sub := <-a.started:
		if sub.channel != channel {
			t.Fatalf("voice subscription for %q, want %q", sub.channel, channel)
		}
		return sub
	case <-time.After(2 * time.Second):
		t.Fatalf("no voice subscription started for %q", channel)
	}
	return voiceSub{}
}

// voiceModelFor is a model on "general" and "ops" whose first presence
// subscription is open, with a recording clock instead of real delays.
func voiceModelFor(t *testing.T) (Model, *voiceAPI, *[]recordedTimer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	api := newVoiceAPI()
	m := NewModel(ctx, api, 5, []string{"general", "ops"})
	m.userName = "alice"
	timers := &[]recordedTimer{}
	m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		*timers = append(*timers, recordedTimer{d, msg})
		return func() tea.Msg { return nil }
	}
	m, cmd := update(m, voiceKick{})
	run(cmd)
	awaitVoice(t, api, "general")
	return m, api, timers
}

func presence(configured, available bool, people ...schema.VoiceParticipant) schema.VoicePresenceV1 {
	return schema.VoicePresenceV1{
		Schema: schema.VoicePresenceSchemaV1, ChannelID: 1, Configured: configured, Available: available,
		Rooms: []schema.VoicePresenceRoom{{Participants: people}},
	}
}

func person(id int64, talking bool) schema.VoiceParticipant {
	return schema.VoiceParticipant{PrincipalID: id, CanPublish: true, Transmitting: talking, JoinedAt: schema.NewTimestamp(time.Unix(1700000000, 0))}
}

func TestVoiceLineStates(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.Msg // delivered with the current subscription's gen; nil = none
		want string
	}{
		{"before the first document", nil, ""},
		{"nobody connected", presence(true, true), "voice: nobody"},
		{"several, one transmitting", presence(true, true, person(9, false), person(5, true), person(3, false)), "voice: 3  5 alice●  9"},
		{"not configured shows nothing", presence(false, false), ""},
		{"unavailable is one quiet word", presence(true, false), "voice: unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, timers := voiceModelFor(t)
			before := m.status()
			if doc, ok := tt.msg.(schema.VoicePresenceV1); ok {
				m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: doc})
			} else if tt.msg == nil {
				// nothing arrives
			} else {
				t.Fatalf("bad case %T", tt.msg)
			}
			if got := m.voiceLine(80); got != tt.want {
				t.Errorf("voice line = %q, want %q", got, tt.want)
			}
			if m.status() != before {
				t.Errorf("status changed to %q", m.status())
			}
			if len(*timers) != 0 {
				t.Errorf("a presence document scheduled %d timers", len(*timers))
			}
		})
	}
}

func TestVoiceEndedClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wasLive   bool
		want      string
		wantRetry bool
	}{
		{"voice not configured (503)", &cli.ServerError{Status: 503, Code: schema.ErrorCodeVoiceNotConfigured}, false, "", false},
		{"--auth off: 400 voice_requires_auth", &cli.ServerError{Status: 400, Code: schema.ErrorCodeVoiceRequiresAuth, Message: "voice needs auth"}, false, "", false},
		{"not a member (404)", &cli.ServerError{Status: 404, Code: "not_found"}, false, "", false},
		{"agent caller (403)", &cli.ServerError{Status: 403, Code: "forbidden"}, false, "", false},
		{"not logged in", &cli.UnauthenticatedError{Server: "http://h:1"}, false, "", false},
		{"removed from the channel", fmt.Errorf("read: %w", websocket.CloseError{Code: websocket.StatusPolicyViolation}), true, "", false},
		{"voice unavailable refusal", &cli.ServerError{Status: 503, Code: schema.ErrorCodeVoiceUnavailable}, false, "voice: unavailable", true},
		{"network drop, was live", errors.New("eof"), true, "voice: reconnecting", true},
		{"network drop, never connected", errors.New("connection refused"), false, "", true},
		{"server shutting down, was live", fmt.Errorf("read: %w", websocket.CloseError{Code: websocket.StatusGoingAway}), true, "voice: reconnecting", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, timers := voiceModelFor(t)
			if tt.wasLive {
				m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true, person(3, false))})
			}
			before := m.status()
			m, _ = update(m, voiceEnded{gen: m.voice.gen, err: tt.err})
			if got := m.voiceLine(80); got != tt.want {
				t.Errorf("voice line = %q, want %q", got, tt.want)
			}
			if (len(*timers) == 1) != tt.wantRetry || len(*timers) > 1 {
				t.Errorf("timers = %d, want retry=%v", len(*timers), tt.wantRetry)
			}
			if m.status() != before {
				t.Errorf("a voice failure wrote to the status line: %q", m.status())
			}
		})
	}
}

func TestVoiceChannelSwitchSwapsSubscriptions(t *testing.T) {
	m, api, _ := voiceModelFor(t)
	general := api.subs[0]
	m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true, person(3, true))})
	oldGen := m.voice.gen

	m, cmd := update(m, tea.KeyMsg{Type: tea.KeyDown})
	if general.ctx.Err() == nil {
		t.Error("the old presence subscription was not cancelled on the switch")
	}
	run(cmd)
	ops := awaitVoice(t, api, "ops")
	if ops.ctx.Err() != nil {
		t.Error("the new presence subscription is already cancelled")
	}
	if got := m.voiceLine(80); got != "" {
		t.Errorf("the old channel's roster is still shown: %q", got)
	}

	// A late document and a late ending from the old socket change nothing.
	m, _ = update(m, voicePresenceReceived{gen: oldGen, doc: presence(true, true, person(3, true))})
	m, _ = update(m, voiceEnded{gen: oldGen, err: errors.New("eof")})
	if got := m.voiceLine(80); got != "" || m.voice.retryPending {
		t.Errorf("a stale event leaked: line %q, retry pending %v", got, m.voice.retryPending)
	}

	// Back again: ops is cancelled and general opens anew.
	m, cmd = update(m, tea.KeyMsg{Type: tea.KeyUp})
	if ops.ctx.Err() == nil {
		t.Error("the ops subscription was not cancelled when switching back")
	}
	run(cmd)
	awaitVoice(t, api, "general")
	if m.current() != "general" {
		t.Errorf("current = %q", m.current())
	}
}

func TestVoiceDropIsRetriedWithBackoff(t *testing.T) {
	m, api, timers := voiceModelFor(t)
	var delays []time.Duration
	for i := 0; i < 7; i++ {
		m, _ = update(m, voiceEnded{gen: m.voice.gen, err: errors.New("eof")})
		last := (*timers)[len(*timers)-1]
		delays = append(delays, last.d)
		due, ok := last.msg.(voiceRetryDue)
		if !ok {
			t.Fatalf("timer carries %T", last.msg)
		}
		var cmd tea.Cmd
		m, cmd = update(m, due)
		run(cmd)
		awaitVoice(t, api, "general")
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, d := range delays {
		if d != want[i]*time.Second {
			t.Fatalf("delays = %v, want %v seconds", delays, want)
		}
	}

	// A socket that stayed up for a while starts the backoff over.
	m, _ = update(m, voiceEnded{gen: m.voice.gen, err: errors.New("eof"), lived: time.Minute})
	if d := (*timers)[len(*timers)-1].d; d != time.Second {
		t.Errorf("delay after a healthy socket = %v, want 1s", d)
	}
	// A document is the socket working: the next drop starts over as well.
	m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true)})
	m, _ = update(m, voiceEnded{gen: m.voice.gen, err: errors.New("eof")})
	if d := (*timers)[len(*timers)-1].d; d != time.Second {
		t.Errorf("delay after a document = %v, want 1s", d)
	}
}

func TestVoiceRetryTimerIgnoredWhenSuperseded(t *testing.T) {
	m, api, timers := voiceModelFor(t)
	m, _ = update(m, voiceEnded{gen: m.voice.gen, err: errors.New("eof")})
	due := (*timers)[0].msg
	// The user switches channel before the timer fires.
	m, cmd := update(m, tea.KeyMsg{Type: tea.KeyDown})
	run(cmd)
	awaitVoice(t, api, "ops")
	_, cmd = update(m, due)
	if cmd != nil {
		t.Error("a retry for a channel the user left started a subscription")
	}
}

func TestVoiceDownDoesNotBlockTypingOrMessages(t *testing.T) {
	m, _, _ := voiceModelFor(t)
	m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true, person(3, false))})
	m, _ = update(m, voiceEnded{gen: m.voice.gen, err: errors.New("eof")})
	if m.voiceLine(80) != "voice: reconnecting" {
		t.Fatalf("voice line = %q", m.voiceLine(80))
	}

	m, _ = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")})
	if m.input != "hi" {
		t.Errorf("input = %q while voice is down", m.input)
	}
	m, _ = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: 1, AuthorID: 3, Body: "hello"}})
	if got := m.messages["general"]; len(got) != 1 || got[0].Body != "hello" {
		t.Errorf("messages = %+v while voice is down", got)
	}
	if view := m.View(); !strings.Contains(view, "hello") || !strings.Contains(view, "voice: reconnecting") {
		t.Errorf("view lacks the message or the quiet voice note:\n%s", view)
	}
}

func TestVoiceLayoutDoesNotJump(t *testing.T) {
	var people []schema.VoiceParticipant
	for i := int64(1); i <= 50; i++ {
		people = append(people, person(i, i%2 == 0))
	}
	counts := []int{0, 1, 5, 50}
	heights := map[int]int{}
	for _, n := range counts {
		m, _, _ := voiceModelFor(t)
		m.width, m.height = 80, 24
		m.messages["general"] = []schema.MessageV2{{ID: 1, AuthorID: 3, Body: "hello"}}
		m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true, people[:n]...)})
		view := m.View()
		lines := strings.Split(view, "\n")
		heights[n] = len(lines)
		paneRows := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "│") || strings.HasPrefix(l, "╭") || strings.HasPrefix(l, "╰") {
				paneRows++
			}
		}
		heights[1000+n] = paneRows
		if got := strings.Count(view, "voice: "); got != 1 {
			t.Errorf("%d people: %d voice lines", n, got)
		}
		for i, l := range lines {
			if w := len([]rune(l)); strings.Contains(l, "voice: ") && w > 80 {
				t.Errorf("%d people: voice line %d is %d runes wide", n, i, w)
			}
		}
	}
	for _, n := range counts[1:] {
		if heights[n] != heights[0] || heights[1000+n] != heights[1000] {
			t.Errorf("layout moved: %d people -> %d lines, %d pane rows; nobody -> %d, %d",
				n, heights[n], heights[1000+n], heights[0], heights[1000])
		}
	}
	if heights[0] != 24 {
		t.Errorf("screen is %d lines tall, want 24", heights[0])
	}
}

func TestVoiceLineTruncatesWithCount(t *testing.T) {
	m, _, _ := voiceModelFor(t)
	var people []schema.VoiceParticipant
	for i := int64(1); i <= 8; i++ {
		people = append(people, person(i, false))
	}
	m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true, people...)})
	tests := []struct {
		width int
		want  string
	}{
		{80, "voice: 1  2  3  4  5 alice  6  7  8"},
		{20, "voice: 1  2  3  +5"},
		{12, "voice: 1  +7"},
	}
	for _, tt := range tests {
		if got := m.voiceLine(tt.width); got != tt.want {
			t.Errorf("width %d: %q, want %q", tt.width, got, tt.want)
		}
		if n := len([]rune(m.voiceLine(tt.width))); n > tt.width {
			t.Errorf("width %d: line is %d runes", tt.width, n)
		}
	}
}

func TestVoiceNameIsSafeForOneLine(t *testing.T) {
	hostile := "ev\x1b[2Jil\u202e\nname \"x\""
	m, _, _ := voiceModelFor(t)
	m.userName = hostile
	m.width, m.height = 200, 24
	m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: presence(true, true, person(5, true))})

	want := "voice: 5 " + cli.VoiceName(hostile) + "●"
	if got := m.voiceLine(200); got != want {
		t.Errorf("voice line = %q, want %q", got, want)
	}
	line := m.voiceLine(200)
	for _, r := range line {
		if r == '\x1b' || r == '\n' || r == '\u202e' {
			t.Fatalf("voice line carries %U: %q", r, line)
		}
	}
	// The same escaping `conch voice status` applies.
	if !strings.Contains(line, "\\u202e") || !strings.Contains(line, `\x1b`) {
		t.Errorf("hostile characters are not shown as visible escapes: %q", line)
	}
}

func TestVoiceNeedsAnAPIThatOffersIt(t *testing.T) {
	m := NewModel(context.Background(), stubAPI{}, 5, []string{"general"})
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init returned no command")
	}
	m, cmd := update(m, voiceKick{})
	if cmd != nil || m.voiceLine(80) != "" {
		t.Errorf("a client without voice presence started one: cmd %v, line %q", cmd != nil, m.voiceLine(80))
	}
}

// A room with a narrower audience than the channel is not merged into the
// roster: its people are not in the channel-wide room.
func TestVoiceRosterListsOnlyTheChannelWideRoom(t *testing.T) {
	m, _, _ := voiceModelFor(t)
	doc := presence(true, true, person(5, false), person(9, true))
	doc.Rooms = append(doc.Rooms, schema.VoicePresenceRoom{
		Audience:     &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3},
		Participants: []schema.VoiceParticipant{person(5, true), person(12, true)},
	})
	m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: doc})
	got := m.voiceLine(80)
	if want := "voice: 5 alice  9●"; got != want {
		t.Fatalf("voice line = %q, want %q", got, want)
	}
}
