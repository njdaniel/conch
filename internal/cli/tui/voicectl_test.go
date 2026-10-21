package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/cli/voicehelper"
	"github.com/njdaniel/conch/pkg/schema"
)

// fakeVoice stands in for voicehelper.Supervisor: it records every call, so a
// test can tell which ran in Update (none may) and which in a command.
type fakeVoice struct {
	mu            sync.Mutex
	calls         []string
	errs          map[string]error
	updates       chan voicehelper.Status
	st            voicehelper.Status
	path          string
	diag          []string
	closeDeadline time.Time
	// closeGate, when set, holds Close until it is closed.
	closeGate chan struct{}
}

func newFakeVoice() *fakeVoice {
	return &fakeVoice{errs: map[string]error{}, updates: make(chan voicehelper.Status, 1)}
}

func (f *fakeVoice) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.errs[strings.Fields(call)[0]]
}

func (f *fakeVoice) Join(_ context.Context, channel string) error { return f.record("join " + channel) }
func (f *fakeVoice) Leave(context.Context) error                  { return f.record("leave") }
func (f *fakeVoice) Retry(context.Context) error                  { return f.record("retry") }
func (f *fakeVoice) SetMute(_ context.Context, on bool) error {
	return f.record(map[bool]string{true: "mute on", false: "mute off"}[on])
}
func (f *fakeVoice) SetDeafen(_ context.Context, on bool) error {
	return f.record(map[bool]string{true: "deafen on", false: "deafen off"}[on])
}
func (f *fakeVoice) Status() voicehelper.Status {
	_ = f.record("status")
	return f.st
}
func (f *fakeVoice) Updates() <-chan voicehelper.Status { return f.updates }
func (f *fakeVoice) Diagnostics() []string {
	_ = f.record("diagnostics")
	return f.diag
}
func (f *fakeVoice) HelperPath() string {
	_ = f.record("path")
	return f.path
}
func (f *fakeVoice) Close(ctx context.Context) error {
	f.mu.Lock()
	f.closeDeadline, _ = ctx.Deadline()
	gate := f.closeGate
	f.mu.Unlock()
	err := f.record("close")
	if gate != nil {
		// A helper that takes its time to stop.
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeVoice) taken() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

// voiceTestModel is richModel (alice, id 7, on general of general, ops and
// movie-night) with voice controls whose supervisor is fake. made records
// the principal of each supervisor made.
func voiceTestModel(t *testing.T) (Model, *fakeVoice, *[]int64) {
	t.Helper()
	m := richModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.ctx = ctx
	fake := newFakeVoice()
	made := &[]int64{}
	m.vc.owner = &voiceOwner{make: func(p int64) (voiceController, error) {
		*made = append(*made, p)
		return fake, nil
	}}
	return m, fake, made
}

// withHelper makes the supervisor and gives the model status as the latest
// it published.
func withHelper(t *testing.T, m Model, st voicehelper.Status) Model {
	t.Helper()
	if _, _, _, _, err := m.vc.owner.get(m.authorID); err != nil {
		t.Fatal(err)
	}
	m.vc.status = st
	return m
}

// drain runs cmd and the commands of a batch, each on its own goroutine, and
// returns the messages that arrive within a short wait. A command that waits
// for something that never comes (the update waiter) is left waiting until
// the test's context ends.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var cmds []tea.Cmd
	var flatten func(tea.Cmd)
	flatten = func(c tea.Cmd) {
		if c == nil {
			return
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- c() }()
		select {
		case msg := <-done:
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, b := range batch {
					flatten(b)
				}
				return
			}
			cmds = append(cmds, func() tea.Msg { return msg })
		case <-time.After(300 * time.Millisecond):
		}
	}
	flatten(cmd)
	var out []tea.Msg
	for _, c := range cmds {
		if msg := c(); msg != nil {
			out = append(out, msg)
		}
	}
	return out
}

func submitText(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	m.input = text
	return keys(t, m, "enter")
}

func connected(channel string) voicehelper.Status {
	return voicehelper.Status{Gen: 1, Channel: channel, Phase: voicehelper.PhaseConnected,
		Capture: schema.VoiceControlCaptureOn, PTT: schema.VoiceControlPTTReady, HelperVersion: "0.1.0"}
}

func failed(channel, reason string) voicehelper.Status {
	return voicehelper.Status{Gen: 1, Channel: channel, Phase: voicehelper.PhaseFailed, Reason: reason,
		Retryable: voicehelper.Retryable(reason), Capture: schema.VoiceControlCaptureOff,
		Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected}}
}

// Each /voice command calls the supervisor from a command, never from
// Update, and only when it can go ahead; every refusal says why and calls
// nothing.
func TestVoiceCommands(t *testing.T) {
	stopping := connected("general")
	stopping.Phase = voicehelper.PhaseStopping
	tests := []struct {
		name     string
		helper   *voicehelper.Status // a supervisor exists and last said this
		input    string
		want     []string // the supervisor calls the command makes
		status   string   // in the status line
		busy     string
		keepText bool // a refusal leaves the input as typed
		// repeated: the command is a second one; the first, for a channel
		// not on screen, only warns (§11) and calls nothing.
		repeated bool
	}{
		{name: "join the channel on screen", input: "/voice join", want: []string{"join general"}, status: "joining #general", busy: "joining #general"},
		{name: "join a named channel", input: "/voice join ops", want: []string{"join ops"}, status: "joining #ops", busy: "joining #ops", repeated: true},
		{name: "join a channel named with #", input: "/voice join #ops", want: []string{"join ops"}, busy: "joining #ops", repeated: true},
		{name: "join a channel not in the list", input: "/voice join nowhere", status: "is not one of your channels", keepText: true},
		{name: "join with two channels", input: "/voice join ops general", status: "usage: /voice join [channel]", keepText: true},
		{name: "join while in voice", helper: ptr(connected("general")), input: "/voice join ops", status: "already in #general voice; /voice leave first", keepText: true},
		{name: "leave", helper: ptr(connected("general")), input: "/voice leave", want: []string{"leave"}, status: "leaving", busy: "leaving"},
		{name: "leave with no helper", input: "/voice leave", status: "not in voice", keepText: true},
		{name: "leave clears a failure", helper: ptr(failed("general", "not_a_member")), input: "/voice leave", want: []string{"leave"}, busy: "leaving"},
		{name: "mute on", helper: ptr(connected("general")), input: "/voice mute on", want: []string{"mute on"}, busy: "muting"},
		{name: "mute off", helper: ptr(connected("general")), input: "/voice mute off", want: []string{"mute off"}, busy: "unmuting"},
		{name: "deafen on", helper: ptr(connected("general")), input: "/voice deafen on", want: []string{"deafen on"}, busy: "deafening"},
		{name: "deafen off", helper: ptr(connected("general")), input: "/voice deafen off", want: []string{"deafen off"}, busy: "undeafening"},
		{name: "mute with no value", helper: ptr(connected("general")), input: "/voice mute", status: "usage: /voice mute on|off", keepText: true},
		{name: "deafen with a wrong value", helper: ptr(connected("general")), input: "/voice deafen yes", status: "usage: /voice deafen on|off", keepText: true},
		{name: "mute with no helper", input: "/voice mute on", status: "not in voice; /voice join first", keepText: true},
		{name: "mute while leaving", helper: &stopping, input: "/voice mute on", status: "not in voice", keepText: true},
		{name: "retry a retryable failure", helper: ptr(failed("ops", "duplicate_identity")), input: "/voice retry", want: []string{"retry"}, status: "retrying #ops", busy: "retrying"},
		{name: "retry what a retry cannot fix", helper: ptr(failed("ops", "not_a_member")), input: "/voice retry", status: "a retry cannot fix this: not a member", keepText: true},
		{name: "retry with nothing failed", helper: ptr(connected("general")), input: "/voice retry", status: "nothing to retry", keepText: true},
		{name: "an unknown subcommand", input: "/voice talk", status: "usage: /voice join [channel]", keepText: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, _ := voiceTestModel(t)
			if tt.helper != nil {
				m = withHelper(t, m, *tt.helper)
			}
			if tt.repeated {
				var first tea.Cmd
				m, first = submitText(t, m, tt.input)
				drain(first)
				if calls := fake.taken(); len(calls) != 0 || !strings.Contains(m.status(), "may already be in #ops's voice") {
					t.Fatalf("the first %q: calls %v, status %q", tt.input, calls, m.status())
				}
			}
			m, cmd := submitText(t, m, tt.input)
			if calls := fake.taken(); len(calls) != 0 {
				t.Fatalf("Update called the supervisor: %v", calls)
			}
			if tt.status != "" && !strings.Contains(m.status(), tt.status) {
				t.Errorf("status %q, want %q in it", m.status(), tt.status)
			}
			if m.vc.busy != tt.busy {
				t.Errorf("busy %q, want %q", m.vc.busy, tt.busy)
			}
			if tt.keepText != (m.input == tt.input) {
				t.Errorf("input %q after %q (kept: want %v)", m.input, tt.input, tt.keepText)
			}
			var done []voiceDone
			for _, msg := range drain(cmd) {
				if d, ok := msg.(voiceDone); ok {
					done = append(done, d)
				}
			}
			if got := fake.taken(); strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("calls %v, want %v", got, tt.want)
			}
			for _, d := range done {
				m, _ = update(m, d)
			}
			if m.vc.busy != "" {
				t.Errorf("still busy %q after the answer", m.vc.busy)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// The voice strip says what the helper confirmed, in the words of the design
// note's table, and is one row at any width.
func TestVoiceStripWords(t *testing.T) {
	with := func(st voicehelper.Status, change func(*voicehelper.Status)) voicehelper.Status {
		change(&st)
		return st
	}
	starting := voicehelper.Status{Gen: 1, Channel: "general", Phase: voicehelper.PhaseStarting,
		Capture: schema.VoiceControlCaptureOff, PTT: schema.VoiceControlPTTUnknown,
		Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected}}
	tests := []struct {
		name    string
		st      voicehelper.Status
		busy    string
		want    string // the strip starts with it
		notWant string
	}{
		{name: "starting", st: starting, want: "voice: joining #general…"},
		{name: "connecting", st: with(starting, func(s *voicehelper.Status) {
			s.Phase, s.PTT = voicehelper.PhaseConnecting, schema.VoiceControlPTTReady
		}), want: "voice: joining #general…"},
		{name: "connected, ready", st: connected("general"), want: "voice: #general · mic on · hold the talk key to talk"},
		{name: "transmitting", st: with(connected("general"), func(s *voicehelper.Status) { s.Transmitting = true }), want: "voice: #general · TALKING", notWant: "mic on"},
		{name: "muted and deafened", st: with(connected("general"), func(s *voicehelper.Status) {
			s.Muted, s.Deafened = true, true
			s.Blocked = []schema.VoiceControlBlock{schema.VoiceControlBlockDeafened, schema.VoiceControlBlockMuted}
		}), want: "voice: #general · mic on · muted · deafened", notWant: "hold the talk key"},
		{name: "listen only", st: with(connected("general"), func(s *voicehelper.Status) {
			s.Capture, s.Blocked = schema.VoiceControlCaptureOff, []schema.VoiceControlBlock{schema.VoiceControlBlockNoPublishGrant}
		}), want: "voice: #general · listen only"},
		{name: "no microphone", st: with(connected("general"), func(s *voicehelper.Status) {
			s.Capture, s.Blocked = schema.VoiceControlCaptureUnavailable, []schema.VoiceControlBlock{schema.VoiceControlBlockNoMicrophone}
		}), want: "voice: #general · no microphone"},
		{name: "no talk key", st: with(connected("general"), func(s *voicehelper.Status) { s.PTT = schema.VoiceControlPTTNotConfigured }),
			want: "voice: #general · mic on · no talk key: set [keys] in voice.toml (conch-voice devices)"},
		{name: "talk key unavailable", st: with(connected("general"), func(s *voicehelper.Status) {
			s.PTT, s.PTTProblem = schema.VoiceControlPTTUnavailable, schema.VoiceControlPTTProblemRefused
		}), want: "voice: #general · mic on · talk key unavailable (no permission to read the key device)"},
		{name: "talk key lost", st: with(connected("general"), func(s *voicehelper.Status) {
			s.PTT, s.PTTProblem = schema.VoiceControlPTTLost, schema.VoiceControlPTTProblemEnded
			s.Blocked = []schema.VoiceControlBlock{schema.VoiceControlBlockKeyDeviceLost}
		}), want: "voice: #general · mic on · talk key lost: presses refused"},
		{name: "reconnecting with a reason", st: with(connected("general"), func(s *voicehelper.Status) {
			s.Phase, s.Reason, s.RetryIn = voicehelper.PhaseReconnecting, "connection_lost", 2500*time.Millisecond
			s.Blocked = []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected}
		}), want: "voice: #general · reconnecting (connection lost), retry in 3 s"},
		{name: "leaving", st: with(connected("general"), func(s *voicehelper.Status) { s.Phase, s.Reason = voicehelper.PhaseStopping, "left" }), want: "voice: leaving #general…"},
		{name: "failed, retryable", st: failed("general", "duplicate_identity"), want: "voice stopped: this account joined voice elsewhere (a retry disconnects the other) · /voice retry"},
		{name: "failed, not retryable", st: failed("general", "not_a_member"), want: "voice stopped: not a member of this channel", notWant: "/voice retry"},
		{name: "helper not found", st: failed("general", voicehelper.ReasonHelperNotFound), want: "voice stopped: conch-voice not found", notWant: "/voice retry"},
		{name: "stopped by a signal", st: with(failed("general", "signal"), func(s *voicehelper.Status) { s.Phase = voicehelper.PhaseStopped }), want: "voice off (stopped by a signal) · /voice join"},
		{name: "muting, not yet acknowledged", st: connected("general"), busy: "muting", want: "voice: #general · mic on · hold the talk key to talk · muting…", notWant: "· muted"},
		{name: "a join asked for, nothing started yet", st: voicehelper.Status{Phase: voicehelper.PhaseIdle}, busy: "joining #ops", want: "voice: joining #ops…"},
		{name: "a reason this TUI does not know", st: failed("general", "new_reason\x1b[2J"), want: "voice stopped: new_reason[2J"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := voiceTestModel(t)
			m.vc.status, m.vc.busy = tt.st, tt.busy
			got := m.voiceStrip(200)
			if !strings.HasPrefix(got, tt.want) {
				t.Errorf("strip %q, want it to start with %q", got, tt.want)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("strip %q has %q", got, tt.notWant)
			}
			for _, width := range []int{20, 40, 60, 80} {
				if w := cellWidth(m.voiceStrip(width)); w > width {
					t.Errorf("at %d cells the strip is %d", width, w)
				}
			}
		})
	}
}

// With no helper, the strip is presence's: "you: in … from another client"
// stays, and where this terminal could join it says how.
func TestVoiceStripIdle(t *testing.T) {
	tests := []struct {
		name     string
		controls bool
		people   []schema.VoiceParticipant
		want     string
	}{
		{"no controls, not in the room", false, []schema.VoiceParticipant{person(8, false)}, "you: not in #general voice · voice: 8 bob"},
		{"controls, not in the room", true, []schema.VoiceParticipant{person(8, false)}, "voice off · /voice join · voice: 8 bob"},
		{"controls, in the room from another client", true, []schema.VoiceParticipant{person(7, true)}, "you: in #general voice from another client, ● talking · voice: ●7 alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := voiceTestModel(t)
			if !tt.controls {
				m.vc.owner = nil
			}
			m.voice.state, m.voice.doc = voiceLive, presence(true, true, tt.people...)
			if got := m.voiceStrip(120); got != tt.want {
				t.Errorf("strip %q, want %q", got, tt.want)
			}
		})
	}
	// Presence not known and nothing asked for: no strip, as before.
	m, _, _ := voiceTestModel(t)
	m.voice.state = voiceConnecting
	if got := m.voiceStrip(120); got != "" {
		t.Errorf("strip %q with nothing to say", got)
	}
}

// Browsing text channels never moves voice: the destination stays in the
// strip, the room of the channel on screen is named apart, and no call is
// made.
func TestBrowsingDoesNotMoveVoice(t *testing.T) {
	m, fake, _ := voiceTestModel(t)
	m = withHelper(t, m, connected("general"))
	m.voice.state, m.voice.doc = voiceLive, presence(true, true, person(7, false))
	if got := m.voiceStrip(200); !strings.HasPrefix(got, "voice: #general · mic on") || !strings.Contains(got, " · room: 7 alice") {
		t.Fatalf("strip on general: %q", got)
	}
	m, cmd := keys(t, m, "down")
	drain(cmd)
	if m.current() != "ops" {
		t.Fatalf("on %q, want ops", m.current())
	}
	m.voice.state, m.voice.doc = voiceLive, presence(true, true, person(8, false))
	got := m.voiceStrip(200)
	if !strings.HasPrefix(got, "voice: #general · mic on") || !strings.Contains(got, " · #ops room: 8") {
		t.Errorf("strip on ops: %q", got)
	}
	for _, call := range fake.taken() {
		if call != "status" {
			t.Errorf("browsing called the supervisor: %s", call)
		}
	}
}

// "muting…" is shown until the ack, and "muted" only once a state says so:
// never what was asked for.
func TestVoiceShowsOnlyWhatTheHelperConfirmed(t *testing.T) {
	m, _, _ := voiceTestModel(t)
	m = withHelper(t, m, connected("general"))
	m, cmd := submitText(t, m, "/voice mute on")
	if got := m.voiceStrip(200); !strings.Contains(got, "muting…") || strings.Contains(got, "· muted") {
		t.Fatalf("before the ack: %q", got)
	}
	for _, msg := range drain(cmd) {
		m, _ = update(m, msg)
	}
	if got := m.voiceStrip(200); strings.Contains(got, "muting") || strings.Contains(got, "muted") {
		t.Fatalf("after the ack, before any state: %q", got)
	}
	_, serial, _ := m.vc.owner.current()
	st := connected("general")
	st.Muted, st.Blocked = true, []schema.VoiceControlBlock{schema.VoiceControlBlockMuted}
	m, _ = update(m, voiceStatusMsg{serial: serial, st: st})
	if got := m.voiceStrip(200); !strings.HasPrefix(got, "voice: #general · mic on · muted") {
		t.Errorf("after the state: %q", got)
	}
	// A state from a supervisor that has since been dropped changes nothing.
	m, _ = update(m, voiceStatusMsg{serial: serial + 1, st: connected("ops")})
	if m.vc.status.Channel != "general" {
		t.Errorf("a stale update was applied: %+v", m.vc.status)
	}
}

// The update waiter delivers the supervisor's states and stops when the
// supervisor is dropped.
func TestVoiceUpdatesWaiter(t *testing.T) {
	m, fake, _ := voiceTestModel(t)
	m, cmd := submitText(t, m, "/voice join")
	fake.updates <- connected("general")
	var got *voiceStatusMsg
	for _, msg := range drain(cmd) {
		if s, ok := msg.(voiceStatusMsg); ok {
			got = &s
		}
	}
	if got == nil || got.st.Phase != voicehelper.PhaseConnected {
		t.Fatalf("the waiter delivered %+v", got)
	}
	m, next := update(m, *got)
	if m.vc.status.Phase != voicehelper.PhaseConnected || next == nil {
		t.Fatalf("status %+v, waiter re-armed: %v", m.vc.status, next != nil)
	}
	ended := make(chan tea.Msg, 1)
	go func() { ended <- next() }()
	m, closing := m.voiceSignedOut()
	select {
	case msg := <-ended:
		if msg != nil {
			t.Errorf("the waiter of a dropped supervisor delivered %T", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter did not end when its supervisor was dropped")
	}
	drain(closing)
}

// The same account already in this channel's voice from another client:
// joining here would disconnect it, so it is said, and only a repeat within
// 10 s goes ahead.
func TestVoiceJoinWhereTheAccountAlreadyIs(t *testing.T) {
	now := time.Date(2026, time.October, 20, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		after  time.Duration
		joined bool
	}{
		{"repeated within 10 s", 5 * time.Second, true},
		{"repeated at 10 s", 10 * time.Second, true},
		{"repeated later", 11 * time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, _ := voiceTestModel(t)
			m.voice.state, m.voice.doc = voiceLive, presence(true, true, person(7, false))
			clock := now
			m.now = func() time.Time { return clock }
			m, cmd := submitText(t, m, "/voice join")
			if cmd != nil || m.input != "" || !strings.Contains(m.status(), "voice: joining disconnects your other #general voice; repeat /voice join within 10 s") {
				t.Fatalf("first: status %q", m.status())
			}
			// The warning is on screen, not cut by the key hints.
			for _, size := range [][2]int{{80, 24}, {140, 36}} {
				m.width, m.height = size[0], size[1]
				if !strings.Contains(m.View(), "voice: you are in #general elsewhere · /voice join again within 10 s takes over") {
					t.Errorf("%dx%d: the warning is not on screen:\n%s", size[0], size[1], m.View())
				}
			}
			clock = now.Add(tt.after)
			m, cmd = submitText(t, m, "/voice join")
			drain(cmd)
			joined := strings.Join(fake.taken(), ",") == "join general"
			if joined != tt.joined {
				t.Errorf("joined %v, want %v (status %q)", joined, tt.joined, m.status())
			}
		})
	}
	// Another channel's presence is not read here, so it is not known whether
	// this account is in its voice: the same repeat is asked for, said in the
	// strip too, and the repeat must name the same channel.
	m, fake, _ := voiceTestModel(t)
	m.voice.state = voiceConnecting
	clock := now
	m.now = func() time.Time { return clock }
	m, cmd := submitText(t, m, "/voice join ops")
	if cmd != nil || m.input != "" || !strings.Contains(m.status(), "this account may already be in #ops's voice from another client; repeat /voice join #ops within 10 s to join here") {
		t.Fatalf("another channel, first: status %q", m.status())
	}
	for _, size := range [][2]int{{80, 24}, {140, 36}} {
		m.width, m.height = size[0], size[1]
		if !strings.Contains(m.View(), "voice: maybe in #ops elsewhere · repeat /voice join #ops within 10 s") {
			t.Errorf("%dx%d: the warning is not on screen:\n%s", size[0], size[1], m.View())
		}
	}
	clock = now.Add(3 * time.Second)
	m, cmd = submitText(t, m, "/voice join movie-night")
	if cmd != nil || !strings.Contains(m.status(), "#movie-night") {
		t.Fatalf("another channel named second: status %q", m.status())
	}
	clock = now.Add(6 * time.Second)
	_, cmd = submitText(t, m, "/voice join movie-night")
	drain(cmd)
	if got := fake.taken(); strings.Join(got, ",") != "join movie-night" {
		t.Errorf("calls %v", got)
	}
}

// Right after this TUI's own /voice leave, presence can go on listing the
// session it left until the server's next sweep. That session, known by its
// join time, is not another client, so a /voice join is not warned about;
// any other entry of this account is, as before (§11).
func TestVoiceJoinRightAfterLeave(t *testing.T) {
	ours := time.Date(2026, time.October, 20, 8, 0, 0, 0, time.UTC)
	theirs := ours.Add(90 * time.Second)
	at := func(id int64, joined time.Time) schema.VoiceParticipant {
		p := person(id, false)
		p.JoinedAt = schema.NewTimestamp(joined)
		return p
	}
	tests := []struct {
		name string
		// helper is what this TUI's helper last said before the leave; nil
		// is no helper and no leave.
		helper *voicehelper.Status
		// before is presence while the helper ran; after, when set, is a
		// presence document that arrives after the leave.
		before, after schema.VoicePresenceV1
		warned        bool
	}{
		{
			name:   "leave then join within the presence lag",
			helper: ptr(connected("general")), before: presence(true, true, at(7, ours), at(8, ours)),
		},
		{
			name:   "leave, then a new document still listing the session left",
			helper: ptr(connected("general")), before: presence(true, true, at(7, ours)),
			after: presence(true, true, at(7, ours), at(8, theirs)),
		},
		{
			name:   "another client present",
			before: presence(true, true, at(7, ours)), warned: true,
		},
		{
			name:   "leave, then a new document listing this account from another client",
			helper: ptr(connected("general")), before: presence(true, true, at(7, ours)),
			after: presence(true, true, at(7, theirs)), warned: true,
		},
		{
			name:   "leave, then the session left gone and another client in",
			helper: ptr(connected("general")), before: presence(true, true, at(7, ours)),
			after: presence(true, true, at(7, theirs), at(8, ours)), warned: true,
		},
		{
			// Displaced: the entry presence shows is the client that took over.
			name:   "leave after another client displaced this one",
			helper: ptr(failed("general", "duplicate_identity")), before: presence(true, true, at(7, theirs)),
			warned: true,
		},
		{
			// Nothing tells this TUI's session from another client then.
			name:   "leave before presence showed the session",
			helper: ptr(connected("general")), before: presence(true, true),
			after: presence(true, true, at(7, ours)), warned: true,
		},
		{
			// The helper was in #ops; #general's entry is someone else's.
			name:   "leave another channel's voice",
			helper: ptr(connected("ops")), before: presence(true, true, at(7, ours)),
			warned: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, _ := voiceTestModel(t)
			m.voice.state, m.voice.doc = voiceLive, tt.before
			if tt.helper != nil {
				m = withHelper(t, m, *tt.helper)
				var cmd tea.Cmd
				m, cmd = submitText(t, m, "/voice leave")
				for _, msg := range drain(cmd) {
					m, _ = update(m, msg)
				}
				// The supervisor says voice is idle once the helper is reaped.
				_, serial, _ := m.vc.owner.current()
				m, _ = update(m, voiceStatusMsg{serial: serial, st: voicehelper.Status{Gen: 1, Phase: voicehelper.PhaseIdle}})
				if got := fake.taken(); strings.Join(got, ",") != "leave" || m.vc.busy != "" {
					t.Fatalf("leave: calls %v, busy %q", got, m.vc.busy)
				}
			}
			if tt.after.Schema != "" {
				m, _ = update(m, voicePresenceReceived{gen: m.voice.gen, doc: tt.after})
			}
			// The strip reads presence the same way.
			if strip := m.voiceStrip(140); strings.Contains(strip, "from another client") != tt.warned {
				t.Errorf("strip %q: says another client %v, want %v", strip, !tt.warned, tt.warned)
			}
			m, cmd := submitText(t, m, "/voice join")
			drain(cmd)
			calls := fake.taken()
			warned := strings.Contains(m.status(), "voice: joining disconnects your other #general voice; repeat /voice join within 10 s")
			if warned != tt.warned || (len(calls) == 0) != tt.warned {
				t.Fatalf("warned %v, want %v; calls %v; status %q", warned, tt.warned, calls, m.status())
			}
			if !tt.warned && strings.Join(calls, ",") != "join general" {
				t.Errorf("calls %v, want join general", calls)
			}
			// A join starts afresh: what the leave recorded is gone.
			if !tt.warned && m.vc.left.channel != "" {
				t.Errorf("after the join, the session left is still %+v", m.vc.left)
			}
		})
	}
}

// A failed or missing helper leaves text as it was: the draft, the history,
// the channel and the focus, and sending still works.
func TestVoiceFailureLeavesTextUsable(t *testing.T) {
	m, fake, _ := voiceTestModel(t)
	fake.errs["join"] = &voicehelper.Error{Reason: voicehelper.ReasonHelperNotFound}
	m, cmd := submitText(t, m, "/voice join")
	m.input = "half a message"
	before := len(m.messages["general"])
	for _, msg := range drain(cmd) {
		m, _ = update(m, msg)
	}
	_, serial, _ := m.vc.owner.current()
	m, _ = update(m, voiceStatusMsg{serial: serial, st: failed("general", voicehelper.ReasonHelperNotFound)})
	if m.input != "half a message" || len(m.messages["general"]) != before || m.current() != "general" || m.mode != modeChannels {
		t.Fatalf("text changed: input %q, %d messages, channel %q", m.input, len(m.messages["general"]), m.current())
	}
	if !strings.HasPrefix(m.voiceStrip(200), "voice stopped: conch-voice not found") {
		t.Errorf("strip %q", m.voiceStrip(200))
	}
	m, cmd = keys(t, m, "enter")
	if !m.sending || cmd == nil {
		t.Fatalf("enter did not send: status %q", m.status())
	}
	if _, ok := run(cmd)[0].(messageSent); !ok {
		t.Error("the send did not run")
	}
}

// Sign-out, an identity change and the TUI's exit stop the helper, in a
// command or (at exit) bounded by StopKill and a second.
func TestVoiceStopsWithTheLogin(t *testing.T) {
	t.Run("whoami says signed out", func(t *testing.T) {
		m, fake, made := voiceTestModel(t)
		m = withHelper(t, m, connected("general"))
		m, cmd := update(m, whoAmILoaded{err: cli.ErrUnauthenticated})
		if calls := fake.taken(); len(calls) != 0 {
			t.Fatalf("Update called %v", calls)
		}
		drain(cmd)
		if got := fake.taken(); strings.Join(got, ",") != "close" {
			t.Fatalf("calls %v, want close", got)
		}
		m, cmd = submitText(t, m, "/voice join")
		if cmd != nil || !strings.Contains(m.status(), "signed out") || len(*made) != 1 {
			t.Errorf("a join after sign-out: status %q, %d supervisors made", m.status(), len(*made))
		}
	})
	t.Run("the message socket says signed out", func(t *testing.T) {
		m, fake, _ := voiceTestModel(t)
		m = withHelper(t, m, connected("general"))
		_, cmd := update(m, subscriptionEnded{channel: "general", err: cli.ErrUnauthenticated})
		drain(cmd)
		if got := fake.taken(); strings.Join(got, ",") != "close" {
			t.Fatalf("calls %v, want close", got)
		}
	})
	t.Run("another account", func(t *testing.T) {
		m, fake, made := voiceTestModel(t)
		m = withHelper(t, m, connected("general"))
		m, cmd := update(m, whoAmILoaded{who: schema.WhoAmIResponseV1{ID: 8, Name: "bob"}})
		drain(cmd)
		// Bob is in general's room in richModel's presence; that is not
		// what this is about.
		m.voice.state = voiceConnecting
		if got := fake.taken(); strings.Join(got, ",") != "close" {
			t.Fatalf("calls %v, want close", got)
		}
		_, cmd = submitText(t, m, "/voice join")
		drain(cmd)
		if strings.Join(fake.taken(), ",") != "join general" || len(*made) != 2 || (*made)[1] != 8 {
			t.Errorf("the next join made supervisors for %v", *made)
		}
	})
	t.Run("the same account again", func(t *testing.T) {
		m, fake, _ := voiceTestModel(t)
		m = withHelper(t, m, connected("general"))
		_, cmd := update(m, whoAmILoaded{who: schema.WhoAmIResponseV1{ID: 7, Name: "alice"}})
		drain(cmd)
		if got := fake.taken(); len(got) != 0 {
			t.Errorf("calls %v, want none", got)
		}
	})
	t.Run("exit", func(t *testing.T) {
		m, fake, _ := voiceTestModel(t)
		m = withHelper(t, m, connected("general"))
		start := time.Now()
		m.closeVoice()
		if got := fake.taken(); strings.Join(got, ",") != "close" {
			t.Fatalf("calls %v, want close", got)
		}
		if d := fake.closeDeadline.Sub(start); d <= 0 || d > voiceCloseTimeout+time.Second {
			t.Errorf("Close was given %v, want about %v", d, voiceCloseTimeout)
		}
		if _, _, _, _, err := m.vc.owner.get(7); !errors.Is(err, errVoiceShut) {
			t.Errorf("a supervisor could be made after exit: %v", err)
		}
		m.closeVoice() // twice is nothing
		if got := fake.taken(); len(got) != 0 {
			t.Errorf("a second close called %v", got)
		}
	})
}

// /voice status reads the supervisor in a command and shows its status, the
// helper's version and path and its log, made safe.
func TestVoiceStatusPage(t *testing.T) {
	m, fake, _ := voiceTestModel(t)
	m = withHelper(t, m, connected("general"))
	st := connected("general")
	st.Muted, st.Blocked = true, []schema.VoiceControlBlock{schema.VoiceControlBlockMuted}
	fake.st, fake.path = st, "/opt/conch/bin/conch-voice"
	fake.diag = []string{"WARN joined", "evil\x1b[31mred"}
	m, cmd := submitText(t, m, "/voice status")
	if calls := fake.taken(); len(calls) != 0 {
		t.Fatalf("Update called %v", calls)
	}
	msgs := drain(cmd)
	if len(msgs) != 1 {
		t.Fatalf("messages %v", msgs)
	}
	m, _ = update(m, msgs[0])
	if !m.help || m.helpTopic != topicVoiceStatus {
		t.Fatalf("help %v topic %v", m.help, m.helpTopic)
	}
	m.width, m.height = 120, 40
	view := m.View()
	for _, want := range []string{"Voice status", "connected", "#general", "muted", "would do nothing: muted", "0.1.0", "/opt/conch/bin/conch-voice", "WARN joined", "evil[31mred"} {
		if !strings.Contains(view, want) {
			t.Errorf("the page lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "\x1b[31m") {
		t.Error("an escape sequence from the log reached the screen")
	}
	// With no supervisor yet it says so without calling anything.
	m, _, _ = voiceTestModel(t)
	m, cmd = submitText(t, m, "/voice status")
	if cmd != nil || !m.help || m.helpTopic != topicVoiceStatus {
		t.Fatalf("no supervisor: cmd %v help %v", cmd != nil, m.help)
	}
	m.width, m.height = 120, 40
	if view := m.View(); !strings.Contains(view, "off: /voice join") || !strings.Contains(view, "none started yet") {
		t.Errorf("the page:\n%s", view)
	}
}

// /voice and /voice help open the voice page: the commands, the talk key and
// the SSH note. The chat help points at it.
func TestVoiceHelp(t *testing.T) {
	for _, input := range []string{"/voice", "/voice help"} {
		m, _, _ := voiceTestModel(t)
		m, _ = submitText(t, m, input)
		if !m.help || m.helpTopic != topicVoice || m.input != "" {
			t.Fatalf("%s: help %v topic %v input %q", input, m.help, m.helpTopic, m.input)
		}
		m.width, m.height = 160, 48
		view := m.View()
		for _, want := range []string{"HELP · Voice", "/voice join [ch]", "/voice mute on|off", "/voice deafen on|off", "/voice retry", "/voice status", "conch-voice devices", "Over SSH", "not those of the computer"} {
			if !strings.Contains(view, want) {
				t.Errorf("%s: the page lacks %q:\n%s", input, want, view)
			}
		}
		// F1 afterwards is the keys of the view again.
		m, _ = keys(t, m, "esc", "f1")
		if m.helpTopic != topicKeys || !strings.Contains(m.View(), "/voice …") {
			t.Errorf("%s: F1 after it shows topic %v", input, m.helpTopic)
		}
	}
}

// /voice takes nothing from the composer's other commands, and typing never
// reaches the supervisor: only a /voice command sent with enter does.
func TestVoiceCommandCollisions(t *testing.T) {
	tests := []struct {
		input   string
		sending bool   // a message is posted
		status  string // in the status line
		help    bool
	}{
		{input: "/voicejoin", status: "unknown command"},
		{input: "/v join", status: "unknown command"},
		{input: "//voice join", sending: true},
		{input: "/w 8 /voice join", sending: true},
		{input: "/net voice", status: "looking up net voice"},
		{input: "/help", help: true},
		{input: "voice join", sending: true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			m, fake, made := voiceTestModel(t)
			m, cmd := submitText(t, m, tt.input)
			if m.sending != tt.sending || m.help != tt.help || (tt.status != "" && !strings.Contains(m.status(), tt.status)) {
				t.Errorf("sending %v help %v status %q", m.sending, m.help, m.status())
			}
			if tt.help && m.helpTopic != topicKeys {
				t.Errorf("/help opened topic %v", m.helpTopic)
			}
			drain(cmd)
			if calls := fake.taken(); len(calls) != 0 || len(*made) != 0 {
				t.Errorf("calls %v, supervisors made %v", calls, *made)
			}
		})
	}
	// Typing, keys and all, with a helper running: nothing reaches it.
	m, fake, _ := voiceTestModel(t)
	m = withHelper(t, m, connected("general"))
	var cmd tea.Cmd
	for _, k := range []string{"/", "v", "o", "i", "c", "e", " ", "m", "u", "t", "e", " ", "o", "n", "backspace", "left", "right", "up", "down", "home", "end", "pgup", "pgdown"} {
		m, cmd = keys(t, m, k)
		drain(cmd)
	}
	if calls := fake.taken(); len(calls) != 0 {
		t.Errorf("typing called the supervisor: %v", calls)
	}
}

// A terminal without voice controls says so; one with no login says voice
// needs a signed-in person, and makes no supervisor.
func TestVoiceNotOffered(t *testing.T) {
	m := richModel(t)
	m, cmd := submitText(t, m, "/voice join")
	if cmd != nil || !strings.Contains(m.status(), "does not offer voice") {
		t.Errorf("no controls: status %q", m.status())
	}
	m = richModel(t).WithVoice(VoiceSetup{server: "http://127.0.0.1:1"})
	m, cmd = submitText(t, m, "/voice join")
	if cmd != nil || m.status() != "voice needs a signed-in person: sign in with conch login" {
		t.Errorf("no login: status %q", m.status())
	}
	if c, _, _ := m.vc.owner.current(); c != nil {
		t.Error("a supervisor was made with no login")
	}
}

// VoiceSetupFor passes the login on as it came, and nothing else: the stored
// one by name, CONCH_TOKEN only when the TUI's own token is that, and none
// when there is none.
func TestVoiceSetupFor(t *testing.T) {
	const server = "http://Conch.Example:8080/"
	tests := []struct {
		name       string
		stored     string
		env        string
		credential schema.VoiceControlCredential
		token      string
	}{
		{name: "stored login", stored: "tok-stored", credential: schema.VoiceControlCredentialStored},
		{name: "CONCH_TOKEN", env: "tok-env", credential: schema.VoiceControlCredentialEnv, token: "tok-env"},
		{name: "CONCH_TOKEN over a stored login", stored: "tok-stored", env: "tok-env", credential: schema.VoiceControlCredentialEnv, token: "tok-env"},
		{name: "no login"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "conch")
			t.Setenv("CONCH_CONFIG_DIR", dir)
			t.Setenv("CONCH_TOKEN", "")
			t.Setenv("CONCH_VOICE_HELPER", "/opt/conch-voice")
			if tt.stored != "" {
				if err := cli.SaveToken(server, tt.stored); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CONCH_TOKEN", tt.env)
			s := VoiceSetupFor(server)
			if s.err != nil || s.server != "http://conch.example:8080" || s.configDir != dir || s.helperPath != "/opt/conch-voice" {
				t.Fatalf("setup %+v", s)
			}
			if s.credential != tt.credential || s.token != tt.token {
				t.Errorf("credential %q token %q, want %q %q", s.credential, s.token, tt.credential, tt.token)
			}
			_, err := s.factory()(7)
			if (tt.credential == "") != errors.Is(err, voicehelper.ErrNoCredential) {
				t.Errorf("factory: %v", err)
			}
		})
	}
	t.Run("an unusable server", func(t *testing.T) {
		if s := VoiceSetupFor("ftp://conch.example"); s.err == nil {
			t.Error("no error")
		} else if _, err := s.factory()(7); err == nil {
			t.Error("a supervisor for an unusable server")
		}
	})
}

// closing starts the close of a supervisor that update dropped, on its own
// goroutine, as Bubble Tea would run the command, and returns when it ends.
func closing(cmd tea.Cmd) <-chan struct{} {
	ended := make(chan struct{})
	go func() {
		for _, c := range flatten(cmd) {
			c()
		}
		close(ended)
	}()
	return ended
}

// flatten is cmd and the commands of a batch, without running any but the
// batch itself.
func flatten(cmd tea.Cmd) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Cmd{func() tea.Msg { return msg }}
	}
	var out []tea.Cmd
	for _, c := range batch {
		out = append(out, flatten(c)...)
	}
	return out
}

// The TUI's exit waits for a helper already being stopped (after a sign-out or
// another account), not only for the one it holds.
func TestExitWaitsForACloseUnderWay(t *testing.T) {
	m, fake, _ := voiceTestModel(t)
	m = withHelper(t, m, connected("general"))
	gate := make(chan struct{})
	fake.closeGate = gate
	m, cmd := update(m, whoAmILoaded{err: cli.ErrUnauthenticated})
	closed := closing(cmd)
	exited := make(chan struct{})
	go func() { m.closeVoice(); close(exited) }()
	select {
	case <-exited:
		t.Fatal("the TUI exited while the helper dropped at sign-out was still stopping")
	case <-time.After(300 * time.Millisecond):
	}
	close(gate)
	for _, ch := range []<-chan struct{}{closed, exited} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("the exit did not end once the helper had stopped")
		}
	}
}

// No helper starts for a new account until the previous account's has been
// closed.
func TestNewAccountWaitsForThePreviousHelper(t *testing.T) {
	m, fake, made := voiceTestModel(t)
	m = withHelper(t, m, connected("general"))
	gate := make(chan struct{})
	fake.closeGate = gate
	m, cmd := update(m, whoAmILoaded{who: schema.WhoAmIResponseV1{ID: 8, Name: "bob"}})
	closed := closing(cmd)
	m.voice.state = voiceConnecting // bob is in richModel's presence; not what this is about
	m, join := submitText(t, m, "/voice join")
	drain(join)
	if !strings.Contains(m.status(), "still stopping the voice of the previous account") || len(*made) != 1 {
		t.Fatalf("join while the old helper stops: status %q, supervisors %v", m.status(), *made)
	}
	deadline := time.Now().Add(2 * time.Second)
	var calls []string
	for len(calls) == 0 && time.Now().Before(deadline) {
		calls = fake.taken()
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Join(calls, ",") != "close" {
		t.Fatalf("calls %v, want only the close under way", calls)
	}
	close(gate)
	<-closed
	_, join = submitText(t, m, "/voice join")
	drain(join)
	if strings.Join(fake.taken(), ",") != "join general" || len(*made) != 2 || (*made)[1] != 8 {
		t.Errorf("after the close: supervisors %v", *made)
	}
}

// An answer from a supervisor dropped since changes neither the status line
// nor the command in flight.
func TestLateAnswerOfADroppedSupervisor(t *testing.T) {
	m, fake, _ := voiceTestModel(t)
	m = withHelper(t, m, connected("general"))
	m, mute := submitText(t, m, "/voice mute on")
	late := drain(mute) // answered after the drop below
	m, cmd := update(m, whoAmILoaded{who: schema.WhoAmIResponseV1{ID: 8, Name: "bob"}})
	drain(cmd)
	fake.taken()
	m.voice.state = voiceConnecting
	m, join := submitText(t, m, "/voice join")
	if m.vc.busy != "joining #general" {
		t.Fatalf("busy %q", m.vc.busy)
	}
	before := m.status()
	for _, msg := range late {
		m, _ = update(m, msg)
	}
	if m.status() != before || m.vc.busy != "joining #general" {
		t.Errorf("the late answer changed status %q -> %q, busy %q", before, m.status(), m.vc.busy)
	}
	for _, msg := range drain(join) {
		if d, ok := msg.(voiceDone); ok {
			m, _ = update(m, d)
		}
	}
	if m.vc.busy != "" || !strings.Contains(m.status(), "started for #general") {
		t.Errorf("the current supervisor's answer: busy %q, status %q", m.vc.busy, m.status())
	}
}

// fakeDevices stands in for voicehelper.OneShot. It records into the fake
// supervisor's log, so that a test sees saves, leaves and joins in one order,
// and takes its errors from there too ("devices", "set").
type fakeDevices struct {
	voice *fakeVoice
	list  voicehelper.DeviceList
}

func (f *fakeDevices) Devices(context.Context) (voicehelper.DeviceList, error) {
	if err := f.voice.record("devices"); err != nil {
		return voicehelper.DeviceList{}, err
	}
	return f.list, nil
}

func (f *fakeDevices) SetDevice(_ context.Context, key voicehelper.DeviceKey, name string) error {
	return f.voice.record("set " + string(key) + " " + name)
}

// devicesTestModel is voiceTestModel with a fake one-shot runner.
func devicesTestModel(t *testing.T) (Model, *fakeVoice, *fakeDevices) {
	t.Helper()
	m, fake, _ := voiceTestModel(t)
	runner := &fakeDevices{voice: fake}
	m.vc.owner.devices = runner
	return m, fake, runner
}

// devicesFixture is one of #649's device lists, read as the TUI reads one.
func devicesFixture(t *testing.T, name string) schema.VoiceDevicesV1 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "schema", "testdata", "voice-devices", name)) // #nosec G304 -- a fixture of this repository
	if err != nil {
		t.Fatal(err)
	}
	doc, err := schema.ParseVoiceDevices(data)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// listShown is m after /voice devices has shown doc.
func listShown(t *testing.T, m Model, runner *fakeDevices, doc schema.VoiceDevicesV1) Model {
	t.Helper()
	runner.list = voicehelper.DeviceList{Devices: doc}
	m, cmd := submitText(t, m, "/voice devices")
	for _, msg := range drain(cmd) {
		m, _ = update(m, msg)
	}
	runner.voice.taken()
	if m.vc.devices == nil {
		t.Fatalf("no list shown: status %q", m.status())
	}
	m.help = false
	return m
}

// /voice devices runs the lister in a command and opens a page of the
// numbered microphones and speakers, with their marks, the missing chosen
// device, and a plain line for each way the list is incomplete; one for each
// #649 fixture.
func TestVoiceDevicesPage(t *testing.T) {
	tests := []struct {
		fixture string
		diag    []string
		want    []string
		notWant []string
	}{
		{
			fixture: "voice-devices-v1.json",
			want: []string{
				"Microphones", "1  Built-in Audio Analog Stereo (alsa_input.pci-0000_00_1f.3.analog-stereo) [default]",
				"2  USB Audio Mono (alsa_input.usb-Generic_USB_Audio-00.mono-fallback) [chosen]",
				"Speakers", "1  Built-in Audio Analog Stereo (alsa_output.pci-0000_00_1f.3.analog-stereo) [default]",
				"2  USB Audio Analog Stereo (alsa_output.usb-Generic_USB_Audio-00.analog-stereo)",
				"chosen: PipeWire default", "/voice mic <n>|<name>|default",
			},
			notWant: []string{"analog-stereo) [default] [chosen]", "not available now", "not shown", "PipeWire could not", "conch-voice said"},
		},
		{
			// A virtual microphone, with no description, by its node.name.
			fixture: "voice-devices-v1-virtual-microphone.json",
			want:    []string{"2  speech_out", "chosen: PipeWire default"},
			notWant: []string{"speech_out (", "[chosen]"},
		},
		{
			// UX-11: the chosen microphone is not there; it is named, and
			// nothing is marked as chosen in its place.
			fixture: "voice-devices-v1-configured-input-absent.json",
			want: []string{
				"1  Built-in Audio Analog Stereo (alsa_input.pci-0000_00_1f.3.analog-stereo) [default]",
				"alsa_input.usb-Generic_USB_Headset-00.mono-fallback [chosen]",
				"chosen in voice.toml, not available now: nothing is sent until it is",
				"1  Built-in Audio Analog Stereo (alsa_output.pci-0000_00_1f.3.analog-stereo) [default] [chosen]",
			},
			notWant: []string{"alsa_input.pci-0000_00_1f.3.analog-stereo) [default] [chosen]"},
		},
		{
			fixture: "voice-devices-v1-unshown.json",
			want:    []string{"2 more microphone(s) not shown: their names cannot be saved in voice.toml", "1 more speaker(s) not shown"},
		},
		{
			fixture: "voice-devices-v1-pipewire-unavailable.json",
			want:    []string{"Microphones", "none", "PipeWire could not be reached, so nothing is listed"},
			notWant: []string{"1  "},
		},
		{
			fixture: "voice-devices-v1-config-refused.json",
			diag:    []string{"conch-voice: voice.toml may be changed by other users"},
			want:    []string{"voice.toml was refused", "conch-voice said", "voice.toml may be changed by other users"},
			notWant: []string{"[chosen]", "chosen: PipeWire default"},
		},
		{
			fixture: "voice-devices-v1-config-none.json",
			want:    []string{"There is no configuration directory, so nothing can be saved", "PipeWire did not finish listing in time"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			m, fake, runner := devicesTestModel(t)
			runner.list = voicehelper.DeviceList{Devices: devicesFixture(t, tt.fixture), Diagnostics: tt.diag}
			m, cmd := submitText(t, m, "/voice devices")
			if calls := fake.taken(); len(calls) != 0 {
				t.Fatalf("Update ran conch-voice: %v", calls)
			}
			if m.input != "" || m.vc.busy != "listing devices" {
				t.Fatalf("input %q busy %q", m.input, m.vc.busy)
			}
			msgs := drain(cmd)
			if got := fake.taken(); strings.Join(got, ",") != "devices" {
				t.Fatalf("calls %v", got)
			}
			for _, msg := range msgs {
				m, _ = update(m, msg)
			}
			if !m.help || m.helpTopic != topicVoiceDevices || m.vc.busy != "" {
				t.Fatalf("help %v topic %v busy %q status %q", m.help, m.helpTopic, m.vc.busy, m.status())
			}
			m.width, m.height = 180, 60
			view := m.View()
			for _, want := range append([]string{"HELP · Voice devices"}, tt.want...) {
				if !strings.Contains(view, want) {
					t.Errorf("the page lacks %q:\n%s", want, view)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(view, nw) {
					t.Errorf("the page has %q:\n%s", nw, view)
				}
			}
			// At any width the page fits.
			for _, cols := range []int{20, 40, 80} {
				for _, l := range m.voiceDevicesLines(cols) {
					if w := cellWidth(l); w > cols {
						t.Errorf("at %d cells a line is %d: %q", cols, w, l)
					}
				}
			}
		})
	}
}

// Device-supplied text the schema lets through (format characters, a line
// separator) and what conch-voice wrote on standard error reach the terminal
// only made safe, on the page, in the status line and on /voice status.
func TestVoiceDeviceTextIsSanitized(t *testing.T) {
	m, fake, runner := devicesTestModel(t)
	m = withHelper(t, m, voicehelper.Status{Phase: voicehelper.PhaseIdle})
	evil := schema.VoiceDevice{Name: "mic\u202e1", Description: "Evil\u202eMic\u2028next line\u200f", Default: true}
	doc := devicesFixture(t, "voice-devices-v1.json")
	doc.Microphones = []schema.VoiceDevice{evil}
	doc.ConfiguredInput = ""
	if err := doc.Validate(); err != nil {
		t.Fatalf("the schema refuses the test's device: %v", err)
	}
	runner.list = voicehelper.DeviceList{Devices: doc, Diagnostics: []string{"\x1b]0;title\x07 bell"}}
	m = listShown(t, m, runner, doc)
	m.vc.devDiag = runner.list.Diagnostics
	m.width, m.height = 160, 50
	m.help, m.helpTopic = true, topicVoiceDevices
	page := m.View()
	if !strings.Contains(page, "EvilMic ↵ next line (mic1) [default]") {
		t.Errorf("the device as shown:\n%s", page)
	}
	m.help = false
	m, cmd := submitText(t, m, "/voice mic 1")
	for _, msg := range drain(cmd) {
		m, _ = update(m, msg)
	}
	// The name saved is the device's own, not what was shown.
	if got := fake.taken(); strings.Join(got, ",") != "set audio.input "+evil.Name {
		t.Errorf("calls %q", got)
	}
	if !strings.Contains(m.status(), "microphone saved: EvilMic ↵ next line (mic1)") {
		t.Errorf("status %q", m.status())
	}
	m.help, m.helpTopic = true, topicVoiceStatus
	for _, view := range []string{page, m.status(), m.View()} {
		for _, bad := range []string{"\u202e", "\u2028", "\u200f", "\x1b", "\x07"} {
			if strings.Contains(view, bad) {
				t.Errorf("%q reached the terminal", bad)
			}
		}
	}
}

// /voice mic and /voice speakers save a number of the last list, a name or the
// default, in a command; each refusal says why and runs nothing.
func TestVoiceDeviceCommands(t *testing.T) {
	v1 := devicesFixture(t, "voice-devices-v1.json")
	tests := []struct {
		name     string
		list     *schema.VoiceDevicesV1 // shown before
		input    string
		want     []string
		status   string
		keepText bool
	}{
		{name: "mic by number", list: &v1, input: "/voice mic 2", want: []string{"set audio.input alsa_input.usb-Generic_USB_Audio-00.mono-fallback"},
			status: "voice: microphone saved: USB Audio Mono (alsa_input.usb-Generic_USB_Audio-00.mono-fallback)"},
		{name: "mic by name, no list needed", input: "/voice mic speech_out", want: []string{"set audio.input speech_out"}, status: "voice: microphone saved: speech_out"},
		{name: "mic default", input: "/voice mic default", want: []string{"set audio.input default"}, status: "voice: microphone saved: PipeWire default"},
		{name: "speakers by number", list: &v1, input: "/voice speakers 1", want: []string{"set audio.output alsa_output.pci-0000_00_1f.3.analog-stereo"},
			status: "voice: speakers saved: Built-in Audio Analog Stereo (alsa_output.pci-0000_00_1f.3.analog-stereo)"},
		{name: "speakers default", list: &v1, input: "/voice speakers default", want: []string{"set audio.output default"}, status: "voice: speakers saved: PipeWire default"},
		{name: "a name with spaces", input: "/voice speakers  Desk speakers ", want: []string{"set audio.output Desk speakers"}},
		{name: "a number with no list shown", input: "/voice mic 2", status: "voice: list them first: /voice devices", keepText: true},
		{name: "a speakers number with no list shown", input: "/voice speakers 1", status: "list them first", keepText: true},
		{name: "a number past the list", list: &v1, input: "/voice mic 3", status: "the last list has no microphone 3", keepText: true},
		{name: "number 0", list: &v1, input: "/voice speakers 0", status: "the last list has no speaker 0", keepText: true},
		{name: "a huge number", list: &v1, input: "/voice mic 99999999999999999999999", status: "the last list has no microphone", keepText: true},
		{name: "no argument", input: "/voice mic", status: "usage: /voice mic <n>|<name>|default", keepText: true},
		{name: "speakers, no argument", input: "/voice speakers", status: "usage: /voice speakers <n>|<name>|default", keepText: true},
		{name: "a name that cannot be saved", input: `/voice mic bad"name`, status: "cannot be saved in voice.toml", keepText: true},
		{name: "a backslash", input: `/voice speakers a\b`, status: "cannot be saved in voice.toml", keepText: true},
		{name: "devices with an argument", input: "/voice devices all", status: "usage: /voice devices", keepText: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, runner := devicesTestModel(t)
			if tt.list != nil {
				m = listShown(t, m, runner, *tt.list)
			}
			m, cmd := submitText(t, m, tt.input)
			if calls := fake.taken(); len(calls) != 0 {
				t.Fatalf("Update ran conch-voice: %v", calls)
			}
			if tt.keepText != (m.input == tt.input) {
				t.Errorf("input %q after %q (kept: want %v)", m.input, tt.input, tt.keepText)
			}
			msgs := drain(cmd)
			if got := fake.taken(); strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("calls %q, want %q", got, tt.want)
			}
			for _, msg := range msgs {
				m, _ = update(m, msg)
			}
			if tt.status != "" && !strings.Contains(m.status(), tt.status) {
				t.Errorf("status %q, want %q in it", m.status(), tt.status)
			}
			if m.vc.busy != "" {
				t.Errorf("still busy %q", m.vc.busy)
			}
		})
	}
	// A device command waits for the voice command in flight, and the other
	// way round.
	m, fake, runner := devicesTestModel(t)
	m = listShown(t, m, runner, v1)
	m.vc.busy = "muting"
	for _, input := range []string{"/voice devices", "/voice mic 1", "/voice speakers default"} {
		var cmd tea.Cmd
		m, cmd = submitText(t, m, input)
		if cmd != nil || !strings.Contains(m.status(), "still muting…; wait for it") {
			t.Errorf("%s while muting: status %q", input, m.status())
		}
	}
	m.vc.busy = ""
	m, _ = submitText(t, m, "/voice mic 1")
	if m.vc.busy != "saving the microphone" {
		t.Fatalf("busy %q", m.vc.busy)
	}
	m, _ = submitText(t, m, "/voice join")
	if !strings.Contains(m.status(), "still saving the microphone…") || len(fake.taken()) != 0 {
		t.Errorf("a join while saving: %q", m.status())
	}
}

// With voice joined, a save is applied by leaving and joining the same
// channel again: leave before join, nothing else asked of the supervisor (mute
// and deafen are its to keep), and the strip goes through leaving and joining
// to connected. Without voice joined, nothing is started.
func TestVoiceSaveThenRejoin(t *testing.T) {
	v1 := devicesFixture(t, "voice-devices-v1.json")
	mutedConnected := connected("general")
	mutedConnected.Muted, mutedConnected.Deafened = true, true
	mutedConnected.Blocked = []schema.VoiceControlBlock{schema.VoiceControlBlockDeafened, schema.VoiceControlBlockMuted}
	m, fake, runner := devicesTestModel(t)
	m = withHelper(t, m, mutedConnected)
	m = listShown(t, m, runner, v1)
	m, cmd := submitText(t, m, "/voice mic 1")
	if got := m.voiceStrip(200); !strings.Contains(got, "saving the microphone…") {
		t.Errorf("strip while saving: %q", got)
	}
	msgs := drain(cmd)
	if got := fake.taken(); strings.Join(got, ",") != "set audio.input alsa_input.pci-0000_00_1f.3.analog-stereo" {
		t.Fatalf("calls %v", got)
	}
	m, rejoin := update(m, msgs[0])
	if calls := fake.taken(); len(calls) != 0 {
		t.Fatalf("Update called the supervisor: %v", calls)
	}
	if rejoin == nil || m.status() != "voice: microphone saved: rejoining #general's voice…" || m.vc.busy != "rejoining #general" {
		t.Fatalf("after the save: status %q busy %q", m.status(), m.vc.busy)
	}
	if m.vc.devices.ConfiguredInput != "alsa_input.pci-0000_00_1f.3.analog-stereo" {
		t.Errorf("the list still names %q", m.vc.devices.ConfiguredInput)
	}
	done := drain(rejoin)
	if got := fake.taken(); strings.Join(got, ",") != "leave,join general" {
		t.Fatalf("calls %v, want leave then join general", got)
	}
	// The strip, as the supervisor publishes the rejoin.
	_, serial, _ := m.vc.owner.current()
	stopping := mutedConnected
	stopping.Phase, stopping.Reason = voicehelper.PhaseStopping, "left"
	starting := voicehelper.Status{Gen: 2, Channel: "general", Phase: voicehelper.PhaseStarting, Muted: true, Deafened: true,
		Capture: schema.VoiceControlCaptureOff, PTT: schema.VoiceControlPTTUnknown,
		Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected}}
	back := mutedConnected
	back.Gen = 2
	for _, step := range []struct {
		st   voicehelper.Status
		want string
	}{
		{stopping, "voice: leaving #general… · rejoining #general…"},
		{voicehelper.Status{Gen: 1, Phase: voicehelper.PhaseIdle}, "voice: rejoining #general…"},
		{starting, "voice: joining #general… · rejoining #general…"},
		{back, "voice: #general · mic on · muted · deafened · rejoining #general…"},
	} {
		m, _ = update(m, voiceStatusMsg{serial: serial, st: step.st})
		if got := m.voiceStrip(200); !strings.HasPrefix(got, step.want) {
			t.Errorf("strip %q, want it to start with %q", got, step.want)
		}
	}
	for _, msg := range done {
		m, _ = update(m, msg)
	}
	if m.vc.busy != "" || !strings.Contains(m.status(), "rejoined #general") || m.vc.rejoinCancel != nil {
		t.Errorf("after the rejoin: busy %q status %q", m.vc.busy, m.status())
	}
	if got := m.voiceStrip(200); !strings.HasPrefix(got, "voice: #general · mic on · muted · deafened") {
		t.Errorf("strip %q", got)
	}

	t.Run("not in voice", func(t *testing.T) {
		for _, st := range []voicehelper.Status{
			{Phase: voicehelper.PhaseIdle},
			failed("general", "duplicate_identity"),
			func() voicehelper.Status { s := connected("general"); s.Phase = voicehelper.PhaseStopping; return s }(),
		} {
			m, fake, runner := devicesTestModel(t)
			m = withHelper(t, m, st)
			m = listShown(t, m, runner, v1)
			m, cmd := submitText(t, m, "/voice speakers 2")
			for _, msg := range drain(cmd) {
				var next tea.Cmd
				m, next = update(m, msg)
				drain(next)
			}
			if got := fake.taken(); strings.Join(got, ",") != "set audio.output alsa_output.usb-Generic_USB_Audio-00.analog-stereo" {
				t.Errorf("%s: calls %v", st.Phase, got)
			}
			if !strings.Contains(m.status(), "speakers saved: USB Audio Analog Stereo") || m.vc.busy != "" {
				t.Errorf("%s: status %q busy %q", st.Phase, m.status(), m.vc.busy)
			}
		}
	})
	t.Run("no supervisor yet", func(t *testing.T) {
		m, fake, _ := devicesTestModel(t)
		m, cmd := submitText(t, m, "/voice mic default")
		for _, msg := range drain(cmd) {
			var next tea.Cmd
			m, next = update(m, msg)
			drain(next)
		}
		if got := fake.taken(); strings.Join(got, ",") != "set audio.input default" {
			t.Errorf("calls %v", got)
		}
		if c, _, _ := m.vc.owner.current(); c != nil {
			t.Error("a save made a supervisor")
		}
	})
	t.Run("a leave typed while saving", func(t *testing.T) {
		m, fake, runner := devicesTestModel(t)
		m = withHelper(t, m, connected("general"))
		m = listShown(t, m, runner, v1)
		m, save := submitText(t, m, "/voice mic 1")
		m, leave := submitText(t, m, "/voice leave")
		for _, msg := range drain(save) {
			var next tea.Cmd
			m, next = update(m, msg)
			if next != nil {
				t.Error("a rejoin after a leave")
			}
		}
		drain(leave)
		if got := fake.taken(); strings.Join(got, ",") != "set audio.input alsa_input.pci-0000_00_1f.3.analog-stereo,leave" {
			t.Errorf("calls %v", got)
		}
		if m.vc.busy != "leaving" {
			t.Errorf("busy %q, want the leave's", m.vc.busy)
		}
	})
	t.Run("a leave typed before the rejoin joins", func(t *testing.T) {
		m, fake, runner := devicesTestModel(t)
		m = withHelper(t, m, connected("general"))
		m = listShown(t, m, runner, v1)
		m, save := submitText(t, m, "/voice mic 1")
		m, rejoin := update(m, drain(save)[0])
		fake.taken()
		m, leave := submitText(t, m, "/voice leave")
		done := drain(rejoin)
		if got := fake.taken(); strings.Join(got, ",") != "leave" {
			t.Errorf("the rejoin after a leave called %v, want only its leave", got)
		}
		for _, msg := range done {
			m, _ = update(m, msg)
		}
		if m.vc.busy != "leaving" || strings.Contains(m.status(), "rejoined") || strings.Contains(m.status(), "canceled") {
			t.Errorf("busy %q status %q", m.vc.busy, m.status())
		}
		drain(leave)
	})
	t.Run("the rejoin fails", func(t *testing.T) {
		m, fake, runner := devicesTestModel(t)
		m = withHelper(t, m, connected("general"))
		m = listShown(t, m, runner, v1)
		fake.errs["join"] = voiceErrFrom(t, voicehelper.ReasonHelperExited)
		m, save := submitText(t, m, "/voice mic 1")
		m, rejoin := update(m, drain(save)[0])
		for _, msg := range drain(rejoin) {
			m, _ = update(m, msg)
		}
		if got := m.status(); got != "voice stopped: conch-voice ended without saying why · /voice retry" || m.vc.busy != "" {
			t.Errorf("status %q busy %q", got, m.vc.busy)
		}
		m.input = "still typing"
		if m, cmd := keys(t, m, "enter"); !m.sending || cmd == nil {
			t.Error("text does not send after a failed rejoin")
		}
	})
}

// voiceErrFrom is a supervisor error with reason, as the real supervisor makes
// one: a helper that ends before its hello.
func voiceErrFrom(t *testing.T, reason string) error {
	t.Helper()
	return &voicehelper.Error{Reason: reason}
}

// Each failure of a one-shot run is one status line, made safe, and text chat
// goes on as before.
func TestVoiceDeviceFailures(t *testing.T) {
	// Real runs that never start anything: the helper cannot be found, or is
	// refused by the trust rules.
	ctx := context.Background()
	_, notFound := voicehelper.NewOneShot(voicehelper.Profile{}, voicehelper.Options{HelperPath: filepath.Join(t.TempDir(), "conch-voice")}).Devices(ctx)
	_, untrusted := voicehelper.NewOneShot(voicehelper.Profile{}, voicehelper.Options{HelperPath: "conch-voice"}).Devices(ctx)
	tests := []struct {
		name  string
		input string
		err   error
		want  string
	}{
		{name: "not found", input: "/voice devices", err: notFound, want: "voice: the file CONCH_VOICE_HELPER names does not exist"},
		{name: "untrusted", input: "/voice devices", err: untrusted, want: "voice: CONCH_VOICE_HELPER must be an absolute path"},
		{name: "untrusted, saving", input: "/voice mic default", err: untrusted, want: "voice: CONCH_VOICE_HELPER must be an absolute path"},
		{name: "too old to list", input: "/voice devices", err: &voicehelper.RunError{Reason: voicehelper.RunUnsupported, ExitCode: 2},
			want: "voice: this conch-voice cannot list devices: install the conch-voice from the same release as conch"},
		{name: "too old to save", input: "/voice mic default", err: &voicehelper.RunError{Reason: voicehelper.RunUnsupported, ExitCode: 2},
			want: "voice: not saved: this conch-voice cannot save devices: install the conch-voice from the same release as conch"},
		{name: "config set refuses", input: "/voice mic speech_out",
			err:  &voicehelper.RunError{Reason: voicehelper.RunFailed, ExitCode: 1, Line: "conch-voice: voice.toml may be changed by other users\x1b[2J"},
			want: "voice: not saved: voice.toml may be changed by other users[2J"},
		{name: "config set fails without a word", input: "/voice speakers default", err: &voicehelper.RunError{Reason: voicehelper.RunFailed, ExitCode: 101},
			want: "voice: not saved: conch-voice failed (exit status 101)"},
		{name: "listing fails", input: "/voice devices", err: &voicehelper.RunError{Reason: voicehelper.RunFailed, ExitCode: 1, Line: "conch-voice: no"},
			want: "voice: the devices could not be listed: no"},
		{name: "listing times out", input: "/voice devices", err: &voicehelper.RunError{Reason: voicehelper.RunTimedOut, ExitCode: -1},
			want: "voice: conch-voice did not list the devices within 10 s and was stopped"},
		{name: "saving times out", input: "/voice mic default", err: &voicehelper.RunError{Reason: voicehelper.RunTimedOut, ExitCode: -1},
			want: "voice: conch-voice did not finish saving within 10 s and was stopped"},
		{name: "a bad JSON object", input: "/voice devices", err: &voicehelper.RunError{Reason: voicehelper.RunBadOutput},
			want: "voice: conch-voice's device list was not valid and was not read"},
		{name: "too much output", input: "/voice devices", err: &voicehelper.RunError{Reason: voicehelper.RunTooMuchOutput, ExitCode: -1},
			want: "voice: conch-voice wrote more than a device list can hold"},
		{name: "the TUI ends", input: "/voice devices", err: context.Canceled, want: "voice: canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, runner := devicesTestModel(t)
			before := devicesFixture(t, "voice-devices-v1.json")
			m = listShown(t, m, runner, before)
			fake.errs["devices"], fake.errs["set"] = tt.err, tt.err
			m, cmd := submitText(t, m, tt.input)
			m.input = "half a message"
			for _, msg := range drain(cmd) {
				m, _ = update(m, msg)
			}
			if got := m.status(); !strings.HasPrefix(got, tt.want) || strings.ContainsAny(got, "\n\x1b") {
				t.Errorf("status %q, want it to start with %q", got, tt.want)
			}
			if m.vc.busy != "" || m.help {
				t.Errorf("busy %q, help %v", m.vc.busy, m.help)
			}
			// The last list shown stays, with what it chose.
			if m.vc.devices == nil || m.vc.devices.ConfiguredInput != before.ConfiguredInput {
				t.Errorf("the list shown changed: %+v", m.vc.devices)
			}
			if m.input != "half a message" || m.current() != "general" {
				t.Fatalf("text changed: input %q channel %q", m.input, m.current())
			}
			m, cmd = keys(t, m, "enter")
			if !m.sending || cmd == nil {
				t.Fatalf("enter did not send: status %q", m.status())
			}
		})
	}
	// A terminal without voice, and a setup that cannot offer it, say so and
	// run nothing.
	m := richModel(t)
	if m, cmd := submitText(t, m, "/voice devices"); cmd != nil || !strings.Contains(m.status(), "does not offer voice") {
		t.Errorf("no controls: status %q", m.status())
	}
	m = richModel(t).WithVoice(VoiceSetup{err: errors.New("no configuration directory")})
	if m, cmd := submitText(t, m, "/voice mic default"); cmd != nil || m.status() != "voice: no configuration directory" {
		t.Errorf("a setup error: status %q", m.status())
	}
	// Listing and saving need no login: a terminal with none still has them.
	if m := richModel(t).WithVoice(VoiceSetup{server: "http://127.0.0.1:1"}); m.vc.owner.devices == nil {
		t.Error("no runner without a login")
	}
}

// /voice status names the chosen devices from the last list read.
func TestVoiceStatusShowsChosenDevices(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []string
	}{
		{name: "no list read", want: []string{"chosen microphone  not read yet: /voice devices", "chosen speakers    not read yet"}},
		{name: "a chosen microphone, the default speakers", fixture: "voice-devices-v1.json",
			want: []string{"chosen microphone  USB Audio Mono (alsa_input.usb-Generic_USB_Audio-00.mono-fallback)", "chosen speakers    PipeWire default"}},
		{name: "a chosen microphone that is missing", fixture: "voice-devices-v1-configured-input-absent.json",
			want: []string{"alsa_input.usb-Generic_USB_Headset-00.mono-fallback: chosen in voice.toml, not available now"}},
		{name: "voice.toml refused", fixture: "voice-devices-v1-config-refused.json", want: []string{"not known: voice.toml was refused"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, runner := devicesTestModel(t)
			if tt.fixture != "" {
				m = listShown(t, m, runner, devicesFixture(t, tt.fixture))
			}
			m, cmd := submitText(t, m, "/voice status")
			if cmd != nil {
				t.Fatal("no supervisor, yet a command")
			}
			m.width, m.height = 180, 50
			view := m.View()
			for _, want := range tt.want {
				if !strings.Contains(view, want) {
					t.Errorf("the page lacks %q:\n%s", want, view)
				}
			}
		})
	}
}

// /voice help lists the device commands and the device note, and the usage
// line names them.
func TestVoiceHelpNamesDevices(t *testing.T) {
	m, _, _ := devicesTestModel(t)
	m, _ = submitText(t, m, "/voice help")
	m.width, m.height = 160, 60
	view := m.View()
	for _, want := range []string{"/voice devices", "/voice mic n|name|default", "/voice speakers n|name|default",
		"the names are PipeWire's", "devices are those of the machine conch runs on", "virtual microphone", "the chosen devices"} {
		if !strings.Contains(view, want) {
			t.Errorf("the page lacks %q:\n%s", want, view)
		}
	}
	m, _, _ = devicesTestModel(t)
	m, _ = submitText(t, m, "/voice talk")
	for _, want := range []string{"| devices |", "| mic <n>|<name>|default |", "| speakers <n>|<name>|default |"} {
		if !strings.Contains(m.status(), want) {
			t.Errorf("usage %q lacks %q", m.status(), want)
		}
	}
}
