package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli/voicehelper"
	"github.com/njdaniel/conch/pkg/schema"
)

// The TUI with a helper that speaks conch.voice_control.v2 (issue #657): the
// microphone and speakers switched live, the open microphone, and the words
// for both, beside the v1 behaviour the tests in voicectl_test.go keep.

// v2Connected is a connected v2 helper on #general, push-to-talk, with its
// speakers.
func v2Connected() voicehelper.Status {
	st := connected("general")
	st.Protocol, st.TalkMode, st.Playback = schema.VoiceControlSchemaV2, schema.VoiceControlTalkModePushToTalk, schema.VoiceControlPlaybackOn
	return st
}

// v1Connected is a connected helper on #general that chose v1.
func v1Connected() voicehelper.Status {
	st := connected("general")
	st.Protocol = schema.VoiceControlSchemaV1
	return st
}

func openMode(st voicehelper.Status) voicehelper.Status {
	st.TalkMode = schema.VoiceControlTalkModeOpen
	return st
}

// The strip's words for the states v2 adds, from the state alone.
func TestVoiceStripWordsV2(t *testing.T) {
	with := func(st voicehelper.Status, change func(*voicehelper.Status)) voicehelper.Status {
		change(&st)
		return st
	}
	open := openMode(v2Connected())
	tests := []struct {
		name    string
		st      voicehelper.Status
		want    string // the strip starts with it
		notWant []string
	}{
		{name: "open, nothing blocked", st: open, want: "voice: #general · mic on · open mic", notWant: []string{"talk key", "hold"}},
		{name: "open, transmitting", st: with(open, func(s *voicehelper.Status) { s.Transmitting = true }), want: "voice: #general · TALKING", notWant: []string{"open mic", "mic on"}},
		{name: "open, the limit reached", st: with(open, func(s *voicehelper.Status) {
			s.Blocked = []schema.VoiceControlBlock{schema.VoiceControlBlockMaxTransmit}
		}),
			want: "voice: #general · mic on · open mic paused: limit reached, waiting for quiet", notWant: []string{"release the key"}},
		{name: "open, muted", st: with(open, func(s *voicehelper.Status) {
			s.Muted, s.Blocked = true, []schema.VoiceControlBlock{schema.VoiceControlBlockMuted}
		}), want: "voice: #general · mic on · muted", notWant: []string{"open mic"}},
		{name: "open, no talk key configured: nothing about the key", st: with(open, func(s *voicehelper.Status) { s.PTT = schema.VoiceControlPTTNotConfigured }),
			want: "voice: #general · mic on · open mic", notWant: []string{"talk key"}},
		{name: "open, the key device lost", st: with(open, func(s *voicehelper.Status) {
			s.PTT, s.PTTProblem = schema.VoiceControlPTTLost, schema.VoiceControlPTTProblemEnded
		}), want: "voice: #general · mic on · open mic", notWant: []string{"talk key", "presses refused"}},
		{name: "open, the microphone missing after a switch", st: with(open, func(s *voicehelper.Status) {
			s.Capture, s.Blocked = schema.VoiceControlCaptureUnavailable, []schema.VoiceControlBlock{schema.VoiceControlBlockNoMicrophone}
		}), want: "voice: #general · no microphone", notWant: []string{"open mic", "mic on"}},
		{name: "push-to-talk, the speakers missing", st: with(v2Connected(), func(s *voicehelper.Status) { s.Playback = schema.VoiceControlPlaybackUnavailable }),
			want: "voice: #general · mic on · no speakers · hold the talk key to talk"},
		{name: "open, the speakers missing", st: with(open, func(s *voicehelper.Status) { s.Playback = schema.VoiceControlPlaybackUnavailable }),
			want: "voice: #general · mic on · no speakers · open mic"},
		{name: "push-to-talk in v2 is v1's words", st: v2Connected(), want: "voice: #general · mic on · hold the talk key to talk", notWant: []string{"open mic", "speakers"}},
		{name: "push-to-talk, the limit reached", st: with(v2Connected(), func(s *voicehelper.Status) {
			s.Blocked = []schema.VoiceControlBlock{schema.VoiceControlBlockMaxTransmit}
		}), want: "voice: #general · mic on · talk limit reached: release the key", notWant: []string{"open mic"}},
		{name: "a v1 helper: no talk mode, no speakers reported", st: v1Connected(), want: "voice: #general · mic on · hold the talk key to talk", notWant: []string{"speakers", "open"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := voiceTestModel(t)
			m.vc.status = tt.st
			got := m.voiceStrip(200)
			if !strings.HasPrefix(got, tt.want) {
				t.Errorf("strip %q, want it to start with %q", got, tt.want)
			}
			for _, nw := range tt.notWant {
				if strings.Contains(strings.TrimPrefix(got, "voice: #general"), nw) {
					t.Errorf("strip %q has %q", got, nw)
				}
			}
			for _, width := range []int{20, 40, 60, 80} {
				if w := cellWidth(m.voiceStrip(width)); w > width {
					t.Errorf("at %d cells the strip is %d", width, w)
				}
			}
		})
	}
}

// step runs the commands of cmd and feeds their answers to Update, checking
// that Update itself called nothing; it returns the calls the commands made
// and the command the last answer returned.
func step(t *testing.T, m Model, fake *fakeVoice, cmd tea.Cmd) (Model, []string, tea.Cmd) {
	t.Helper()
	msgs := drain(cmd)
	calls := fake.taken()
	var next tea.Cmd
	for _, msg := range msgs {
		m, next = update(m, msg)
		if got := fake.taken(); len(got) != 0 {
			t.Fatalf("Update called %v", got)
		}
	}
	return m, calls, next
}

// In a v2 session a saved device is switched live: saved first, then
// set_devices, with no leave and no join. "switching microphone…" is shown
// until the ack, and the strip changes only on the state that follows.
func TestVoiceDeviceSwitchV2(t *testing.T) {
	v1 := devicesFixture(t, "voice-devices-v1.json")
	tests := []struct {
		name      string
		input     string
		saveCall  string
		switchTo  string
		busy      string
		done      string
		missing   func(*voicehelper.Status)
		stripThen string
	}{
		{name: "the microphone, by number", input: "/voice mic 2",
			saveCall: "set audio.input alsa_input.usb-Generic_USB_Audio-00.mono-fallback", switchTo: "switch input alsa_input.usb-Generic_USB_Audio-00.mono-fallback",
			busy: "switching microphone", done: "voice: microphone switched; the strip shows whether it is there",
			missing: func(s *voicehelper.Status) {
				s.Capture, s.Blocked = schema.VoiceControlCaptureUnavailable, []schema.VoiceControlBlock{schema.VoiceControlBlockNoMicrophone}
			},
			stripThen: "voice: #general · no microphone"},
		{name: "the microphone, by name", input: "/voice mic speech_out",
			saveCall: "set audio.input speech_out", switchTo: "switch input speech_out",
			busy: "switching microphone", done: "voice: microphone switched",
			missing: func(s *voicehelper.Status) {
				s.Capture, s.Blocked = schema.VoiceControlCaptureUnavailable, []schema.VoiceControlBlock{schema.VoiceControlBlockNoMicrophone}
			},
			stripThen: "voice: #general · no microphone"},
		{name: "the microphone, PipeWire's default", input: "/voice mic default",
			saveCall: "set audio.input default", switchTo: "switch input default", busy: "switching microphone", done: "voice: microphone switched"},
		{name: "the speakers", input: "/voice speakers 2",
			saveCall: "set audio.output alsa_output.usb-Generic_USB_Audio-00.analog-stereo", switchTo: "switch output alsa_output.usb-Generic_USB_Audio-00.analog-stereo",
			busy: "switching speakers", done: "voice: speakers switched; the strip shows whether they are there",
			missing:   func(s *voicehelper.Status) { s.Playback = schema.VoiceControlPlaybackUnavailable },
			stripThen: "voice: #general · mic on · no speakers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, runner := devicesTestModel(t)
			m = withHelper(t, m, v2Connected())
			m = listShown(t, m, runner, v1)
			m, cmd := submitText(t, m, tt.input)
			if calls := fake.taken(); len(calls) != 0 {
				t.Fatalf("Update called %v", calls)
			}
			m, calls, apply := step(t, m, fake, cmd)
			if strings.Join(calls, ",") != tt.saveCall {
				t.Fatalf("calls %v, want the save %q first", calls, tt.saveCall)
			}
			if apply == nil || m.vc.busy != tt.busy || !strings.HasSuffix(m.status(), tt.busy+"…") {
				t.Fatalf("after the save: busy %q status %q", m.vc.busy, m.status())
			}
			if got := m.voiceStrip(200); !strings.Contains(got, tt.busy+"…") {
				t.Errorf("strip while switching: %q", got)
			}
			m, calls, _ = step(t, m, fake, apply)
			if strings.Join(calls, ",") != tt.switchTo {
				t.Fatalf("calls %v, want only %q (no leave, no join)", calls, tt.switchTo)
			}
			if m.vc.busy != "" || !strings.HasPrefix(m.status(), tt.done) {
				t.Errorf("after the ack: busy %q status %q", m.vc.busy, m.status())
			}
			// Nothing about the device is shown from what was asked: the strip
			// is the last state's until the next one.
			if got := m.voiceStrip(200); !strings.HasPrefix(got, "voice: #general · mic on · hold the talk key to talk") {
				t.Errorf("strip after the ack, before a state: %q", got)
			}
			if tt.missing != nil {
				_, serial, _ := m.vc.owner.current()
				st := v2Connected()
				tt.missing(&st)
				m, _ = update(m, voiceStatusMsg{serial: serial, st: st})
				if got := m.voiceStrip(200); !strings.HasPrefix(got, tt.stripThen) {
					t.Errorf("strip after the state: %q, want it to start with %q", got, tt.stripThen)
				}
				if got := m.voiceStrip(200); strings.Contains(got, "alsa_") || strings.Contains(got, "speech_out") {
					t.Errorf("the strip names a device: %q", got)
				}
			}
		})
	}
}

// A v1 helper, Protocol said, still gets #652's save then rejoin, and is
// never sent set_devices.
func TestVoiceDeviceSaveV1Rejoins(t *testing.T) {
	m, fake, runner := devicesTestModel(t)
	m = withHelper(t, m, v1Connected())
	m = listShown(t, m, runner, devicesFixture(t, "voice-devices-v1.json"))
	m, cmd := submitText(t, m, "/voice mic 1")
	m, calls, rejoin := step(t, m, fake, cmd)
	if strings.Join(calls, ",") != "set audio.input alsa_input.pci-0000_00_1f.3.analog-stereo" {
		t.Fatalf("calls %v", calls)
	}
	if m.vc.busy != "rejoining #general" || m.status() != "voice: microphone saved: rejoining #general's voice…" {
		t.Fatalf("busy %q status %q", m.vc.busy, m.status())
	}
	_, calls, _ = step(t, m, fake, rejoin)
	if strings.Join(calls, ",") != "leave,join general" {
		t.Fatalf("calls %v, want leave then join", calls)
	}
}

// /voice mode saves audio.talk_mode, and then: in a v2 session, set_talk_mode
// and, for open, the echo warning once; in a v1 session, nothing live, and for
// open the words that say a newer conch-voice is needed; with no voice, the
// save alone.
func TestVoiceMode(t *testing.T) {
	tests := []struct {
		name      string
		helper    *voicehelper.Status
		input     string
		calls     []string // the save, then any live change
		afterSave string   // the status line once saved
		afterAck  string   // the status line once the live change is acknowledged
		keepText  bool
	}{
		{name: "open, in a v2 session", helper: ptr(v2Connected()), input: "/voice mode open",
			calls:     []string{"set audio.talk_mode open", "talkmode open", "status"},
			afterSave: "voice: open microphone saved: switching to the open microphone…", afterAck: voiceEchoWarning},
		{name: "ptt, in a v2 session in open mode", helper: ptr(openMode(v2Connected())), input: "/voice mode ptt",
			calls:     []string{"set audio.talk_mode push_to_talk", "talkmode push_to_talk", "status"},
			afterSave: "voice: push-to-talk saved: switching to push-to-talk…", afterAck: "voice: push-to-talk; the strip shows its state"},
		{name: "open, in a v1 session", helper: ptr(v1Connected()), input: "/voice mode open",
			calls:     []string{"set audio.talk_mode open"},
			afterSave: "voice: open microphone needs a newer conch-voice: install the one from the same release (saved in voice.toml for it)"},
		{name: "ptt, in a v1 session", helper: ptr(v1Connected()), input: "/voice mode ptt",
			calls: []string{"set audio.talk_mode push_to_talk"}, afterSave: "voice: push-to-talk saved; this conch-voice is always push-to-talk"},
		{name: "open, with no voice", input: "/voice mode open",
			calls: []string{"set audio.talk_mode open"}, afterSave: "voice: open microphone saved: used from the next /voice join"},
		{name: "open, after a failure", helper: ptr(failed("general", "duplicate_identity")), input: "/voice mode open",
			calls: []string{"set audio.talk_mode open"}, afterSave: "voice: open microphone saved: used from the next /voice join"},
		{name: "no mode", input: "/voice mode", afterSave: "usage: /voice mode ptt|open", keepText: true},
		{name: "a mode that does not exist", input: "/voice mode always", afterSave: "usage: /voice mode ptt|open", keepText: true},
		{name: "two modes", input: "/voice mode open ptt", afterSave: "usage: /voice mode ptt|open", keepText: true},
		{name: "the wire name is not the command's", input: "/voice mode push_to_talk", afterSave: "usage: /voice mode ptt|open", keepText: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, _ := devicesTestModel(t)
			if tt.helper != nil {
				m = withHelper(t, m, *tt.helper)
			}
			m, cmd := submitText(t, m, tt.input)
			if calls := fake.taken(); len(calls) != 0 {
				t.Fatalf("Update called %v", calls)
			}
			if tt.keepText != (m.input == tt.input) {
				t.Errorf("input %q after %q (kept: want %v)", m.input, tt.input, tt.keepText)
			}
			m, calls, apply := step(t, m, fake, cmd)
			if m.status() != tt.afterSave {
				t.Errorf("status %q, want %q", m.status(), tt.afterSave)
			}
			if apply != nil {
				var more []string
				m, more, _ = step(t, m, fake, apply)
				calls = append(calls, more...)
				if m.status() != tt.afterAck {
					t.Errorf("status after the ack %q, want %q", m.status(), tt.afterAck)
				}
			}
			if strings.Join(calls, ",") != strings.Join(tt.calls, ",") {
				t.Errorf("calls %v, want %v", calls, tt.calls)
			}
			if m.vc.busy != "" {
				t.Errorf("still busy %q", m.vc.busy)
			}
		})
	}
}

// The strip says "open mic" only once the helper's state says the open
// microphone is in force, never on the ack alone, and goes back to the
// push-to-talk words on the state that says so.
func TestVoiceModeStripFollowsTheState(t *testing.T) {
	m, fake, _ := devicesTestModel(t)
	m = withHelper(t, m, v2Connected())
	m, cmd := submitText(t, m, "/voice mode open")
	m, _, apply := step(t, m, fake, cmd)
	if got := m.voiceStrip(200); !strings.Contains(got, "switching to the open microphone…") || strings.Contains(got, "open mic ") {
		t.Errorf("strip while switching: %q", got)
	}
	m, _, _ = step(t, m, fake, apply)
	if got := m.voiceStrip(200); strings.Contains(got, "open mic") || !strings.Contains(got, "hold the talk key to talk") {
		t.Errorf("strip after the ack, before the state: %q", got)
	}
	_, serial, _ := m.vc.owner.current()
	m, _ = update(m, voiceStatusMsg{serial: serial, st: openMode(v2Connected())})
	if got := m.voiceStrip(200); !strings.HasPrefix(got, "voice: #general · mic on · open mic") {
		t.Errorf("strip after the state: %q", got)
	}
	// The warning was shown with the ack; the state that confirms the
	// switch does not show it a second time.
	m.setStatus(modeChannels, "typing on")
	m, _ = update(m, voiceStatusMsg{serial: serial, st: openMode(v2Connected())})
	if m.status() != "typing on" {
		t.Errorf("the warning again for the same switch: %q", m.status())
	}
	// Open again while open is no switch: no warning, and no "switching…"
	// left behind.
	m, cmd = submitText(t, m, "/voice mode open")
	m, _, apply = step(t, m, fake, cmd)
	m, _, _ = step(t, m, fake, apply)
	if m.status() != "voice: open microphone; the strip shows its state" {
		t.Errorf("open while open: %q", m.status())
	}
	m, cmd = submitText(t, m, "/voice mode ptt")
	m, _, apply = step(t, m, fake, cmd)
	m, _, _ = step(t, m, fake, apply)
	m, _ = update(m, voiceStatusMsg{serial: serial, st: v2Connected()})
	if got := m.voiceStrip(200); !strings.HasPrefix(got, "voice: #general · mic on · hold the talk key to talk") {
		t.Errorf("strip back in push-to-talk: %q", got)
	}
}

// The echo warning is shown once for every switch to open that the helper
// confirms, a session started open from voice.toml included, and again on the
// next switch after push-to-talk.
func TestVoiceEchoWarningOncePerSwitch(t *testing.T) {
	m, _, _ := devicesTestModel(t)
	m = withHelper(t, m, v2Connected())
	_, serial, _ := m.vc.owner.current()
	feed := func(st voicehelper.Status) {
		m.setStatus(modeChannels, "before")
		m, _ = update(m, voiceStatusMsg{serial: serial, st: st})
	}
	for i, s := range []struct {
		st   voicehelper.Status
		warn bool
	}{
		{openMode(v2Connected()), true},  // voice.toml says open
		{openMode(v2Connected()), false}, // the same switch, a later state
		{func() voicehelper.Status {
			s := openMode(v2Connected())
			s.Transmitting = true
			return s
		}(), false},
		{v2Connected(), false},          // back to push-to-talk
		{openMode(v2Connected()), true}, // a new switch
		{func() voicehelper.Status { s := openMode(v2Connected()); s.Gen = 2; return s }(), true}, // a new session, open from the start
	} {
		feed(s.st)
		if warned := m.status() == voiceEchoWarning; warned != s.warn {
			t.Errorf("state %d: warned %v, want %v (status %q)", i, warned, s.warn, m.status())
		}
	}
}

// Every new call runs in a command; a failure is one status line and leaves
// text as it was.
func TestVoiceV2Failures(t *testing.T) {
	tests := []struct {
		name   string
		helper voicehelper.Status
		input  string
		errKey string
		err    error
		want   string
	}{
		{name: "the mode cannot be saved by an older conch-voice", helper: v2Connected(), input: "/voice mode open", errKey: "set",
			err:  &voicehelper.RunError{Reason: voicehelper.RunUnsupported, ExitCode: 2},
			want: "voice: not saved: this conch-voice cannot save the talk mode: install the conch-voice from the same release as conch"},
		{name: "saving the mode times out", helper: v2Connected(), input: "/voice mode open", errKey: "set",
			err: &voicehelper.RunError{Reason: voicehelper.RunTimedOut, ExitCode: -1}, want: "voice: conch-voice did not finish saving within 10 s and was stopped"},
		{name: "the helper refuses set_talk_mode", helper: v2Connected(), input: "/voice mode open", errKey: "talkmode",
			err: voiceErrFrom(t, voicehelper.ReasonHelperIncompatible), want: "voice stopped: conch-voice does not match this conch: install the same release"},
		{name: "the helper refuses set_devices", helper: v2Connected(), input: "/voice mic speech_out", errKey: "switch",
			err: voiceErrFrom(t, voicehelper.ReasonHelperIncompatible), want: "voice stopped: conch-voice does not match this conch"},
		{name: "the helper stops answering", helper: v2Connected(), input: "/voice speakers default", errKey: "switch",
			err: voiceErrFrom(t, voicehelper.ReasonAckTimeout), want: "voice stopped: conch-voice stopped answering"},
		{name: "a v1 helper after all", helper: v2Connected(), input: "/voice mode open", errKey: "talkmode",
			err: voicehelper.ErrNeedsNewerHelper, want: "voice: this needs a newer conch-voice: install the one from the same release as conch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, _ := devicesTestModel(t)
			m = withHelper(t, m, tt.helper)
			fake.errs[tt.errKey] = tt.err
			m, cmd := submitText(t, m, tt.input)
			m.input = "half a message"
			m, _, apply := step(t, m, fake, cmd)
			if apply != nil {
				m, _, _ = step(t, m, fake, apply)
			}
			if got := m.status(); !strings.HasPrefix(got, tt.want) {
				t.Errorf("status %q, want it to start with %q", got, tt.want)
			}
			if m.vc.busy != "" || m.input != "half a message" {
				t.Errorf("busy %q input %q", m.vc.busy, m.input)
			}
			m, cmd = keys(t, m, "enter")
			if !m.sending || cmd == nil {
				t.Fatalf("enter did not send: status %q", m.status())
			}
		})
	}
	// Busy: a mode change waits for the command in flight, as a save does.
	m, _, _ := devicesTestModel(t)
	m = withHelper(t, m, v2Connected())
	m.vc.busy = "muting"
	if m, cmd := submitText(t, m, "/voice mode open"); cmd != nil || !strings.Contains(m.status(), "still muting…; wait for it") {
		t.Errorf("a mode change while muting: status %q", m.status())
	}
	// A terminal that cannot run conch-voice cannot save a mode.
	m = richModel(t).WithVoice(VoiceSetup{err: errors.New("no configuration directory")})
	if m, cmd := submitText(t, m, "/voice mode open"); cmd != nil || m.status() != "voice: no configuration directory" {
		t.Errorf("a setup error: status %q", m.status())
	}
}

// A /voice leave typed while a live change is in flight keeps its own words:
// the change's ack does not overwrite them.
func TestVoiceLeaveDuringASwitch(t *testing.T) {
	m, fake, _ := devicesTestModel(t)
	m = withHelper(t, m, v2Connected())
	m, cmd := submitText(t, m, "/voice mic speech_out")
	m, _, apply := step(t, m, fake, cmd)
	m, leave := submitText(t, m, "/voice leave")
	m, _, _ = step(t, m, fake, apply)
	if m.vc.busy != "leaving" || m.status() != "voice: leaving…" {
		t.Errorf("busy %q status %q", m.vc.busy, m.status())
	}
	drain(leave)
}

// /voice status names the protocol, the talk mode and the speakers.
func TestVoiceStatusPageV2(t *testing.T) {
	tests := []struct {
		name string
		st   voicehelper.Status
		want []string
	}{
		{name: "v2, open, no speakers", st: func() voicehelper.Status {
			s := openMode(v2Connected())
			s.Playback = schema.VoiceControlPlaybackUnavailable
			s.Blocked = []schema.VoiceControlBlock{schema.VoiceControlBlockMaxTransmit}
			return s
		}(), want: []string{"conch.voice_control.v2 (devices switch live; open microphone)", "open microphone: its own sound is sent",
			"unavailable: nothing is played until they are back", "a sound now", "limit reached, waiting for quiet", "ignored for talking"}},
		{name: "v2, push-to-talk", st: v2Connected(), want: []string{"conch.voice_control.v2", "talk mode", "push-to-talk", "a press now"}},
		{name: "v1", st: v1Connected(), want: []string{"conch.voice_control.v1 (devices change by rejoining; push-to-talk only)",
			"push-to-talk (this conch-voice has no open microphone)", "not reported by this conch-voice"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, _ := devicesTestModel(t)
			m = withHelper(t, m, tt.st)
			fake.st = tt.st
			m, cmd := submitText(t, m, "/voice status")
			for _, msg := range drain(cmd) {
				m, _ = update(m, msg)
			}
			m.width, m.height = 200, 60
			view := m.View()
			for _, want := range tt.want {
				if !strings.Contains(view, want) {
					t.Errorf("the page lacks %q:\n%s", want, view)
				}
			}
		})
	}
}

// /voice help names /voice mode and carries the echo warning; the usage line
// names the command.
func TestVoiceHelpOpenMicrophone(t *testing.T) {
	notes := strings.Join(voiceHelpNotes(), "\n")
	if !strings.Contains(notes, voiceEchoWarning) {
		t.Errorf("the help notes lack the echo warning:\n%s", notes)
	}
	m, _, _ := devicesTestModel(t)
	m, _ = submitText(t, m, "/voice help")
	m.width, m.height = 200, 70
	view := m.View()
	for _, want := range []string{"/voice mode ptt|open", "Open microphone: whatever your microphone hears", "use headphones"} {
		if !strings.Contains(view, want) {
			t.Errorf("the page lacks %q:\n%s", want, view)
		}
	}
	m, _, _ = devicesTestModel(t)
	m, _ = submitText(t, m, "/voice talk")
	if !strings.Contains(m.status(), "| mode ptt|open |") {
		t.Errorf("usage %q lacks the mode command", m.status())
	}
}

// An ack of set_talk_mode with no state saying the new mode before it (a
// helper out of step with §20.3) is followed by a snapshot, asked for in the
// same command; one whose state came first asks for nothing more.
func TestVoiceModeAckWithoutStateRefreshes(t *testing.T) {
	tests := []struct {
		name            string
		ackWithoutState bool
		want            []string
	}{
		{"state came first", false, []string{"talkmode open", "status"}},
		{"no state yet", true, []string{"talkmode open", "status", "refresh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake, _ := devicesTestModel(t)
			m = withHelper(t, m, v2Connected())
			fake.st = v2Connected()
			fake.ackWithoutState = tt.ackWithoutState
			m, cmd := submitText(t, m, "/voice mode open")
			m, _, apply := step(t, m, fake, cmd)
			_, calls, _ := step(t, m, fake, apply)
			if !slices.Equal(calls, tt.want) {
				t.Errorf("calls in the command: %v, want %v", calls, tt.want)
			}
		})
	}
}
