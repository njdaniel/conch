package voicehelper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The tests of conch.voice_control.v2 (issue #657; design note §20): the
// version chosen from the hello, the two commands only v2 has, the v2 state in
// Status, and v2's events read with v2's reader alone. The v1 suite is
// unchanged beside them.

// recorded is every command line the fake read, from its -record file.
func recorded(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a file in the test's own temporary directory
	if err != nil {
		t.Fatalf("the fake recorded nothing: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// line is msg as the supervisor writes it, without its line feed.
func line(t *testing.T, msg schema.VoiceControlMessage) string {
	t.Helper()
	b, err := schema.MarshalVoiceControlLine(msg)
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSuffix(b, []byte{'\n'}))
}

// TestNegotiation: the start is written in v2 when the hello offers it, and in
// v1 otherwise; Status says which, and only a v2 helper reports the talk mode
// and the speakers.
func TestNegotiation(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		version  string
		talkMode schema.VoiceControlTalkMode
		playback schema.VoiceControlPlayback
	}{
		{name: "a hello offering v1 only", args: []string{"-versions", schema.VoiceControlSchemaV1}, version: schema.VoiceControlSchemaV1},
		{name: "a hello offering v1 and v2", args: []string{"-versions", bothVersions}, version: schema.VoiceControlSchemaV2,
			talkMode: schema.VoiceControlTalkModePushToTalk, playback: schema.VoiceControlPlaybackOn},
		{name: "v2, with voice.toml choosing the open microphone", args: []string{"-versions", bothVersions, "-talk-mode", "open"},
			version: schema.VoiceControlSchemaV2, talkMode: schema.VoiceControlTalkModeOpen, playback: schema.VoiceControlPlaybackOn},
		{name: "a later version offered beside v1 and v2", args: []string{"-versions", bothVersions + ",conch.voice_control.v3"}, version: schema.VoiceControlSchemaV2,
			talkMode: schema.VoiceControlTalkModePushToTalk, playback: schema.VoiceControlPlaybackOn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "commands")
			s := newSupervisor(t, storedProfile(t), Timings{}, append(tt.args, "-record", record)...)
			ctx := ctxFor(t)
			if err := s.Join(ctx, testChannel); err != nil {
				t.Fatal(err)
			}
			waitFor(t, s, "connected", func(st Status) bool { return st.Phase == PhaseConnected })
			st := s.Status()
			if st.Protocol != tt.version || st.TalkMode != tt.talkMode || st.Playback != tt.playback {
				t.Errorf("protocol %q talk mode %q playback %q, want %q %q %q", st.Protocol, st.TalkMode, st.Playback, tt.version, tt.talkMode, tt.playback)
			}
			start := recorded(t, record)[0]
			var wantStart schema.VoiceControlMessage = schema.VoiceControlStartV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandStart, ID: 1,
				Server: testServer, Channel: testChannel, PrincipalID: testPrincipal, Credential: schema.VoiceControlCredentialStored}
			if tt.version == schema.VoiceControlSchemaV2 {
				wantStart = schema.VoiceControlStartV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandStart, ID: 1,
					Server: testServer, Channel: testChannel, PrincipalID: testPrincipal, Credential: schema.VoiceControlCredentialStored}
			}
			if want := line(t, wantStart); start != want {
				t.Errorf("start %s, want %s", start, want)
			}
			// Every later command is in the same version, leave included.
			if err := s.SetMute(ctx, true); err != nil {
				t.Fatal(err)
			}
			if err := s.Leave(ctx); err != nil {
				t.Fatal(err)
			}
			for _, l := range recorded(t, record)[1:] {
				if !strings.Contains(l, `"schema":"`+tt.version+`"`) {
					t.Errorf("a command in another version: %s", l)
				}
			}
			assertNoMismatch(t, s)
		})
	}
}

// TestV2CommandsInAV1Session: with a helper that speaks only v1, SetDevices
// and SetTalkMode say a newer conch-voice is needed and write nothing at all,
// so that a v1 helper is never sent a command it would refuse. The session
// goes on.
func TestV2CommandsInAV1Session(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Supervisor) error
	}{
		{"set_devices", func(ctx context.Context, s *Supervisor) error { return s.SetDevices(ctx, "speech_out", "") }},
		{"set_devices, the speakers", func(ctx context.Context, s *Supervisor) error {
			return s.SetDevices(ctx, "", schema.VoiceControlDeviceDefault)
		}},
		{"set_talk_mode open", func(ctx context.Context, s *Supervisor) error {
			return s.SetTalkMode(ctx, schema.VoiceControlTalkModeOpen)
		}},
		{"set_talk_mode push_to_talk", func(ctx context.Context, s *Supervisor) error {
			return s.SetTalkMode(ctx, schema.VoiceControlTalkModePushToTalk)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "commands")
			s := newSupervisor(t, storedProfile(t), Timings{}, "-record", record)
			ctx := ctxFor(t)
			if err := s.Join(ctx, testChannel); err != nil {
				t.Fatal(err)
			}
			if err := tt.call(ctx, s); !errors.Is(err, ErrNeedsNewerHelper) {
				t.Fatalf("%s in a v1 session: %v, want ErrNeedsNewerHelper", tt.name, err)
			}
			// A command written after it is answered only once the helper has
			// read everything before it: the record is then complete.
			if err := s.Refresh(ctx); err != nil {
				t.Fatalf("the session did not go on: %v", err)
			}
			want := []string{
				line(t, schema.VoiceControlStartV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandStart, ID: 1,
					Server: testServer, Channel: testChannel, PrincipalID: testPrincipal, Credential: schema.VoiceControlCredentialStored}),
				line(t, schema.VoiceControlGetStateV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandGetState, ID: 2}),
			}
			if got := recorded(t, record); !slices.Equal(got, want) {
				t.Errorf("the helper read %q, want only %q", got, want)
			}
			if st := s.Status(); st.Phase != PhaseConnected || st.Protocol != schema.VoiceControlSchemaV1 {
				t.Errorf("status %+v", st)
			}
			assertNoMismatch(t, s)
		})
	}
}

// TestV2Commands: in a v2 session SetDevices and SetTalkMode write the v2
// command and return after its ack, and the Status the ack follows shows what
// the helper confirmed: a missing microphone or speakers as unavailable, never
// a device in their place, and the talk mode in force. A command the schema
// refuses is refused here, with nothing written.
func TestV2Commands(t *testing.T) {
	const absent = "alsa_input.usb-Generic_USB_Headset-00.mono-fallback"
	const absentOut = "alsa_output.usb-Generic_USB_Headset-00.analog-stereo"
	tests := []struct {
		name     string
		args     []string
		call     func(context.Context, *Supervisor) error
		wantErr  bool
		written  schema.VoiceControlMessage // the command the helper read, nil for none
		check    func(Status) bool
		describe string
	}{
		{
			name: "a microphone that is there", call: func(ctx context.Context, s *Supervisor) error { return s.SetDevices(ctx, "speech_out", "") },
			written: schema.VoiceControlSetDevicesV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandSetDevices, ID: 2, Input: "speech_out"},
			check:   func(st Status) bool { return st.Capture == schema.VoiceControlCaptureOn && len(st.Blocked) == 0 }, describe: "capture on",
		},
		{
			name: "a microphone that is not there", args: []string{"-absent", absent},
			call:    func(ctx context.Context, s *Supervisor) error { return s.SetDevices(ctx, absent, "") },
			written: schema.VoiceControlSetDevicesV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandSetDevices, ID: 2, Input: absent},
			check: func(st Status) bool {
				return st.Capture == schema.VoiceControlCaptureUnavailable && slices.Equal(st.Blocked, []schema.VoiceControlBlock{schema.VoiceControlBlockNoMicrophone}) &&
					st.Playback == schema.VoiceControlPlaybackOn
			},
			describe: "capture unavailable, no_microphone, the speakers untouched",
		},
		{
			name: "speakers that are not there", args: []string{"-absent", absentOut},
			call:    func(ctx context.Context, s *Supervisor) error { return s.SetDevices(ctx, "", absentOut) },
			written: schema.VoiceControlSetDevicesV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandSetDevices, ID: 2, Output: absentOut},
			check: func(st Status) bool {
				return st.Playback == schema.VoiceControlPlaybackUnavailable && st.Capture == schema.VoiceControlCaptureOn && len(st.Blocked) == 0
			},
			describe: "playback unavailable, nothing blocked",
		},
		{
			name: "both, back to PipeWire's default", call: func(ctx context.Context, s *Supervisor) error {
				return s.SetDevices(ctx, schema.VoiceControlDeviceDefault, schema.VoiceControlDeviceDefault)
			},
			written: schema.VoiceControlSetDevicesV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandSetDevices, ID: 2, Input: "default", Output: "default"},
			check: func(st Status) bool {
				return st.Capture == schema.VoiceControlCaptureOn && st.Playback == schema.VoiceControlPlaybackOn
			},
			describe: "both on",
		},
		{
			name: "the open microphone", call: func(ctx context.Context, s *Supervisor) error {
				return s.SetTalkMode(ctx, schema.VoiceControlTalkModeOpen)
			},
			written: schema.VoiceControlSetTalkModeV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandSetTalkMode, ID: 2, TalkMode: schema.VoiceControlTalkModeOpen},
			check:   func(st Status) bool { return st.TalkMode == schema.VoiceControlTalkModeOpen && !st.Transmitting }, describe: "open, not transmitting",
		},
		{
			name: "back to push-to-talk", args: []string{"-talk-mode", "open"},
			call: func(ctx context.Context, s *Supervisor) error {
				return s.SetTalkMode(ctx, schema.VoiceControlTalkModePushToTalk)
			},
			written: schema.VoiceControlSetTalkModeV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandSetTalkMode, ID: 2, TalkMode: schema.VoiceControlTalkModePushToTalk},
			check:   func(st Status) bool { return st.TalkMode == schema.VoiceControlTalkModePushToTalk }, describe: "push_to_talk",
		},
		{
			name: "neither device named", call: func(ctx context.Context, s *Supervisor) error { return s.SetDevices(ctx, "", "") }, wantErr: true,
			check: func(st Status) bool { return st.Phase == PhaseConnected }, describe: "still connected",
		},
		{
			name: "a name voice.toml cannot hold", call: func(ctx context.Context, s *Supervisor) error { return s.SetDevices(ctx, `my "mic"`, "") }, wantErr: true,
			check: func(st Status) bool { return st.Phase == PhaseConnected }, describe: "still connected",
		},
		{
			name: "a name with a line feed", call: func(ctx context.Context, s *Supervisor) error { return s.SetDevices(ctx, "", "out\n[keys]") }, wantErr: true,
			check: func(st Status) bool { return st.Phase == PhaseConnected }, describe: "still connected",
		},
		{
			name: "a talk mode of no version", call: func(ctx context.Context, s *Supervisor) error { return s.SetTalkMode(ctx, "always_on") }, wantErr: true,
			check: func(st Status) bool { return st.TalkMode == schema.VoiceControlTalkModePushToTalk }, describe: "push_to_talk",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "commands")
			s := newSupervisor(t, storedProfile(t), Timings{}, append([]string{"-versions", bothVersions, "-record", record}, tt.args...)...)
			ctx := ctxFor(t)
			if err := s.Join(ctx, testChannel); err != nil {
				t.Fatal(err)
			}
			waitFor(t, s, "connected", func(st Status) bool { return st.Phase == PhaseConnected })
			err := tt.call(ctx, s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("call: %v, want an error %v", err, tt.wantErr)
			}
			// The state a command changed is written before its ack, so the
			// status is already the result when the call returns.
			if st := s.Status(); !tt.check(st) {
				t.Errorf("status %+v, want %s", st, tt.describe)
			}
			if err := s.Refresh(ctx); err != nil {
				t.Fatalf("the session did not go on: %v", err)
			}
			got := recorded(t, record)
			want := 2 // the start, and the get_state
			if tt.written != nil {
				want++
				if w := line(t, tt.written); len(got) < 2 || got[1] != w {
					t.Errorf("the helper read %q, want %s second", got, w)
				}
			}
			if len(got) != want {
				t.Errorf("the helper read %d commands, want %d: %q", len(got), want, got)
			}
			if err := s.Leave(ctx); err != nil {
				t.Fatal(err)
			}
			assertNoMismatch(t, s)
		})
	}
}

// TestV2CommandFailures: refusals, a helper that stops answering, and the
// commands with no helper, as for set_mute.
func TestV2CommandFailures(t *testing.T) {
	setDevices := func(ctx context.Context, s *Supervisor) error { return s.SetDevices(ctx, "speech_out", "") }
	setTalkMode := func(ctx context.Context, s *Supervisor) error {
		return s.SetTalkMode(ctx, schema.VoiceControlTalkModeOpen)
	}
	tests := []struct {
		name    string
		args    []string
		timings Timings
		call    func(context.Context, *Supervisor) error
		reason  string // the supervisor's reason, for a helper it stopped
		notRun  bool   // ErrNotRunning, and the session goes on
	}{
		{name: "set_devices refused", args: []string{"-refuse", "set_devices:invalid"}, timings: stopTimings, call: setDevices, reason: ReasonHelperIncompatible},
		{name: "set_talk_mode refused", args: []string{"-refuse", "set_talk_mode:unknown_kind"}, timings: stopTimings, call: setTalkMode, reason: ReasonHelperIncompatible},
		{name: "set_devices refused while stopping", args: []string{"-refuse", "set_devices:stopping"}, call: setDevices, notRun: true},
		{name: "set_talk_mode refused while stopping", args: []string{"-refuse", "set_talk_mode:stopping"}, call: setTalkMode, notRun: true},
		{name: "set_devices never answered", args: []string{"-ignore", "set_devices"},
			timings: Timings{Ack: 400 * time.Millisecond, StopTerm: stopTimings.StopTerm, StopKill: stopTimings.StopKill}, call: setDevices, reason: ReasonAckTimeout},
		{name: "set_talk_mode never answered", args: []string{"-ignore", "set_talk_mode"},
			timings: Timings{Ack: 400 * time.Millisecond, StopTerm: stopTimings.StopTerm, StopKill: stopTimings.StopKill}, call: setTalkMode, reason: ReasonAckTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSupervisor(t, storedProfile(t), tt.timings, append([]string{"-versions", bothVersions}, tt.args...)...)
			ctx := ctxFor(t)
			if err := s.Join(ctx, testChannel); err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			err := tt.call(ctx, s)
			if tt.notRun {
				if !errors.Is(err, ErrNotRunning) {
					t.Fatalf("%v, want ErrNotRunning", err)
				}
				if err := s.Refresh(ctx); err != nil {
					t.Fatalf("the helper was stopped: %v", err)
				}
				return
			}
			if reasonOf(err) != tt.reason {
				t.Fatalf("%v, want %s", err, tt.reason)
			}
			if took := time.Since(began); tt.reason == ReasonAckTimeout && took > s.t.Ack+time.Second {
				t.Errorf("the call took %v, more than the ack bound", took)
			}
			waitReaped(t, s)
			if st := s.Status(); st.Phase != PhaseFailed || st.Reason != tt.reason || st.Capture != schema.VoiceControlCaptureOff ||
				st.Playback != schema.VoiceControlPlaybackOff || st.Transmitting {
				t.Errorf("status %+v", st)
			}
			assertGone(t, s.lastExit().pid)
		})
	}
	t.Run("no helper, and after Close", func(t *testing.T) {
		s := newSupervisor(t, storedProfile(t), Timings{})
		ctx := ctxFor(t)
		if err := setDevices(ctx, s); !errors.Is(err, ErrNotRunning) {
			t.Errorf("set_devices with no helper: %v", err)
		}
		if err := setTalkMode(ctx, s); !errors.Is(err, ErrNotRunning) {
			t.Errorf("set_talk_mode with no helper: %v", err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := setDevices(ctx, s); !errors.Is(err, ErrClosed) {
			t.Errorf("set_devices after Close: %v", err)
		}
		if err := setTalkMode(ctx, s); !errors.Is(err, ErrClosed) {
			t.Errorf("set_talk_mode after Close: %v", err)
		}
	})
}

// TestBadEventsV2: in a v2 session every event is read with v2's reader, and
// anything it refuses (a v1 line, a hello, a state that does not hold
// together) stops the helper as bad_event, as in v1.
func TestBadEventsV2(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"a v1 state", []string{"-bad-v2", "a v1 state: not read as v2"}},
		{"a hello written in v2", []string{"-bad-v2", "a hello written in v2: there is no such thing"}},
		{"a second v1 hello", []string{"-after", "second-hello"}},
		{"a state without talk_mode", []string{"-bad-v2", "state without talk_mode"}},
		{"a state without playback", []string{"-bad-v2", "state without playback"}},
		{"playback off while connected", []string{"-bad-v2", "playback off while connected: the speakers are opened in starting, and absent ones are unavailable"}},
		{"key_device_lost in open mode", []string{"-bad-v2", "key_device_lost in open mode"}},
		{"transmitting in open mode while muted", []string{"-bad-v2", "transmitting in open mode while muted"}},
		{"an ack with a code only a protocol_error carries", []string{"-bad-v2", "ack with a code that only a protocol_error may carry"}},
		{"an event of a later version", []string{"-bad-v2", "an event of a later version"}},
		{"oversized", []string{"-after", "oversized"}},
		{"seq repeated", []string{"-after", "seq-repeat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSupervisor(t, storedProfile(t), stopTimings, append([]string{"-versions", bothVersions}, tt.args...)...)
			if err := s.Join(ctxFor(t), testChannel); err != nil && reasonOf(err) != ReasonBadEvent {
				t.Fatalf("join: %v", err)
			}
			waitFor(t, s, "bad_event", func(st Status) bool { return st.Reason == ReasonBadEvent })
			waitReaped(t, s)
			st := s.Status()
			if st.Phase != PhaseFailed || !st.Retryable || st.Capture != schema.VoiceControlCaptureOff || st.Transmitting {
				t.Errorf("status %+v", st)
			}
			if !slices.ContainsFunc(s.Diagnostics(), func(l string) bool { return strings.Contains(l, "bad event") }) {
				t.Errorf("diagnostics %q do not say why", s.Diagnostics())
			}
			assertGone(t, s.lastExit().pid)
		})
	}
}

// TestV2Skips: an event of a kind v2 does not know is skipped, and a gap in
// seq is made up with a v2 get_state, as in v1.
func TestV2Skips(t *testing.T) {
	t.Run("an unknown kind", func(t *testing.T) {
		s := newSupervisor(t, storedProfile(t), Timings{}, "-versions", bothVersions, "-bad-v2", "an event of a kind this version does not know")
		ctx := ctxFor(t)
		if err := s.Join(ctx, testChannel); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTalkMode(ctx, schema.VoiceControlTalkModeOpen); err != nil {
			t.Fatal(err)
		}
		if st := s.Status(); st.Phase != PhaseConnected || st.TalkMode != schema.VoiceControlTalkModeOpen {
			t.Fatalf("status %+v", st)
		}
		assertNoMismatch(t, s)
	})
	t.Run("a gap", func(t *testing.T) {
		s := newSupervisor(t, storedProfile(t), Timings{}, "-versions", bothVersions, "-after", "gap")
		ctx := ctxFor(t)
		if err := s.Join(ctx, testChannel); err != nil {
			t.Fatal(err)
		}
		waitFor(t, s, "the snapshot", func(st Status) bool { return st.Muted && st.Deafened && st.Protocol == schema.VoiceControlSchemaV2 })
		if err := s.Leave(ctx); err != nil {
			t.Fatal(err)
		}
		assertNoMismatch(t, s)
	})
}

// TestFromStateV2 maps a v2 state: v1's fields as v1's are mapped, and the
// talk mode and the speakers.
func TestFromStateV2(t *testing.T) {
	tests := []struct {
		fixture string
		want    Status
	}{
		{fixture: "event-state-open-listening.json", want: Status{Gen: 2, Channel: "c", Phase: PhaseConnected, Capture: schema.VoiceControlCaptureOn,
			Playback: schema.VoiceControlPlaybackOn, TalkMode: schema.VoiceControlTalkModeOpen, PTT: schema.VoiceControlPTTNotConfigured, HelperVersion: "v"}},
		{fixture: "event-state-open-transmitting.json", want: Status{Gen: 2, Channel: "c", Phase: PhaseConnected, Capture: schema.VoiceControlCaptureOn,
			Transmitting: true, Playback: schema.VoiceControlPlaybackOn, TalkMode: schema.VoiceControlTalkModeOpen, PTT: schema.VoiceControlPTTNotConfigured, HelperVersion: "v"}},
		{fixture: "event-state-open-max-transmit.json", want: Status{Gen: 2, Channel: "c", Phase: PhaseConnected, Capture: schema.VoiceControlCaptureOn,
			Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockMaxTransmit}, Playback: schema.VoiceControlPlaybackOn,
			TalkMode: schema.VoiceControlTalkModeOpen, PTT: schema.VoiceControlPTTNotConfigured, HelperVersion: "v"}},
		{fixture: "event-state-no-speakers.json", want: Status{Gen: 2, Channel: "c", Phase: PhaseConnected, Capture: schema.VoiceControlCaptureOn,
			Playback: schema.VoiceControlPlaybackUnavailable, TalkMode: schema.VoiceControlTalkModePushToTalk, PTT: schema.VoiceControlPTTReady, HelperVersion: "v"}},
		{fixture: "event-state-no-microphone.json", want: Status{Gen: 2, Channel: "c", Phase: PhaseConnected, Capture: schema.VoiceControlCaptureUnavailable,
			Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockNoMicrophone}, Playback: schema.VoiceControlPlaybackOn,
			TalkMode: schema.VoiceControlTalkModePushToTalk, PTT: schema.VoiceControlPTTReady, HelperVersion: "v"}},
		{fixture: "event-state-starting.json", want: Status{Gen: 2, Channel: "c", Phase: PhaseStarting, Capture: schema.VoiceControlCaptureOff,
			Blocked:  []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected, schema.VoiceControlBlockNoPublishGrant, schema.VoiceControlBlockNoMicrophone},
			Playback: schema.VoiceControlPlaybackOff, TalkMode: schema.VoiceControlTalkModePushToTalk, PTT: schema.VoiceControlPTTUnknown, HelperVersion: "v"}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(fixtureDirV2, tt.fixture)) // #nosec G304 -- a fixture of the shared directory
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, data); err != nil {
				t.Fatal(err)
			}
			ev, err := schema.ParseVoiceControlEventV2(compact.Bytes())
			if err != nil || ev.State == nil {
				t.Fatalf("%v", err)
			}
			got := fromStateV2(2, "c", "v", ev.State)
			if !statusEqual(got, tt.want) {
				t.Errorf("fromStateV2 = %+v\nwant %+v", got, tt.want)
			}
		})
	}
	ended := Status{Phase: PhaseConnected, Capture: schema.VoiceControlCaptureOn, Playback: schema.VoiceControlPlaybackUnavailable}.failed(ReasonHelperCrashed).ended()
	if ended.Playback != schema.VoiceControlPlaybackOff || ended.Capture != schema.VoiceControlCaptureOff {
		t.Errorf("ended %+v: the speakers and the microphone are closed with the process", ended)
	}
	if v1 := (Status{Phase: PhaseConnected}).ended(); v1.Playback != "" {
		t.Errorf("a v1 status ended with playback %q, which v1 never reports", v1.Playback)
	}
}

// v2Tracker has read a v1 hello offering both versions and written a v2
// start, so that any v2 event may come next.
func v2Tracker(t *testing.T) *tracker {
	t.Helper()
	tr := &tracker{version: schema.VoiceControlSchemaV2}
	var hello bytes.Buffer
	data, err := os.ReadFile(filepath.Join(fixtureDirV2, "event-hello-v1-and-v2.json")) // #nosec G304 -- a fixture of the shared directory
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&hello, data); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.read(hello.Bytes(), false); err != nil {
		t.Fatal(err)
	}
	tr.written(1, schema.VoiceControlCommandStart)
	tr.lastSeq = 0
	return tr
}

// TestEventCasesV2 runs every event case of v2's cases.json through the
// owner's reading of a v2 session: the bounded line reader, the v2 reader and
// the tracker.
func TestEventCasesV2(t *testing.T) {
	var doc struct {
		Cases []struct {
			Name      string  `json:"name"`
			Direction string  `json:"direction"`
			Line      *string `json:"line"`
			LineHex   string  `json:"line_hex"`
			LineFile  string  `json:"line_file"`
			Want      struct {
				Result string `json:"result"`
				Kind   string `json:"kind"`
				Code   string `json:"code"`
			} `json:"want"`
		} `json:"cases"`
	}
	data, err := os.ReadFile(filepath.Join(fixtureDirV2, "cases.json")) // #nosec G304 -- a fixture of the shared directory
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, c := range doc.Cases {
		if c.Direction != "event" {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			var raw []byte
			switch {
			case c.Line != nil:
				raw = []byte(*c.Line)
			case c.LineHex != "":
				b, err := hex.DecodeString(c.LineHex)
				if err != nil {
					t.Fatal(err)
				}
				raw = b
			case c.LineFile != "":
				b, err := os.ReadFile(filepath.Join(fixtureDirV2, filepath.FromSlash(c.LineFile))) // #nosec G304 -- a fixture of the shared directory
				if err != nil {
					t.Fatal(err)
				}
				raw = b
			}
			lr := newLineReader(bytes.NewReader(append(raw, '\n')))
			l, tooLong, err := lr.next()
			if err != nil {
				t.Fatal(err)
			}
			tr := v2Tracker(t)
			if ev, perr := schema.ParseVoiceControlEventV2(l); perr == nil && ev.Ack != nil {
				tr.pending = []pendingCommand{{id: ev.Ack.ID, kind: schema.VoiceControlCommandSetDevices}}
			}
			rd, err := tr.read(l, tooLong)
			switch c.Want.Result {
			case "accept", "skip":
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if rd.ev.Seq != 0 {
					t.Fatal("read with the v1 reader in a v2 session")
				}
				kind := string(rd.ev2.Unknown)
				switch h := rd.head(); {
				case h.ack:
					kind = "ack"
				case h.state:
					kind = "state"
				case h.protocolError:
					kind = "protocol_error"
				}
				if kind != c.Want.Kind || (c.Want.Result == "skip") != (rd.ev2.Unknown != "") {
					t.Errorf("read as %q (unknown %q), want %s %q", kind, rd.ev2.Unknown, c.Want.Result, c.Want.Kind)
				}
			case "refuse":
				var bad *badEventError
				if !errors.As(err, &bad) || !strings.Contains(bad.detail, c.Want.Code) {
					t.Fatalf("%v, want a bad event with %s", err, c.Want.Code)
				}
				if len(l) > 0 && strings.Contains(err.Error(), string(l)) {
					t.Errorf("the error repeats the line")
				}
			default:
				t.Fatalf("want.result %q", c.Want.Result)
			}
		})
	}
	if ran < 20 {
		t.Fatalf("only %d event cases", ran)
	}
}

// TestTranscriptsOwnerSideV2 reads every v2 transcript as the owner does: the
// hello with the v1 reader, then each event with the reader of the version
// the transcript's start chose.
func TestTranscriptsOwnerSideV2(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixtureDirV2, "transcript-*.json"))
	if err != nil || len(files) < 9 {
		t.Fatalf("transcripts: %d, %v", len(files), err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			var doc struct {
				Steps []struct {
					From    string          `json:"from"`
					Message json.RawMessage `json:"message"`
					Exit    *int            `json:"exit"`
				} `json:"steps"`
			}
			data, err := os.ReadFile(file) // #nosec G304 -- a fixture of the shared directory
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			tr := &tracker{}
			for i, step := range doc.Steps {
				if step.Message == nil {
					continue
				}
				var l bytes.Buffer
				if err := json.Compact(&l, step.Message); err != nil {
					t.Fatal(err)
				}
				if step.From == "owner" {
					if !tr.wrote {
						// The start fixes the version.
						tr.version = schema.VoiceControlSchemaV1
						if _, err := schema.ParseVoiceControlCommandV2(l.Bytes()); err == nil {
							tr.version = schema.VoiceControlSchemaV2
						}
					}
					id, kind := commandHead(l.Bytes(), tr.version)
					tr.written(id, kind)
					continue
				}
				rd, err := tr.read(l.Bytes(), false)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if h := rd.head(); h.ack && rd.acked.id != h.ackID {
					t.Fatalf("step %d: the ack of %d answered %d", i, h.ackID, rd.acked.id)
				}
			}
			last := doc.Steps[len(doc.Steps)-1]
			if last.Exit == nil || !tr.final {
				t.Fatal("the transcript does not end with a final state and an exit")
			}
		})
	}
}

// commandHead is the id and kind of a command line in version, or the id the
// reader's refusal names.
func commandHead(l []byte, version string) (int64, schema.VoiceControlCommandKind) {
	var vcErr *schema.VoiceControlError
	if version == schema.VoiceControlSchemaV2 {
		cmd, err := schema.ParseVoiceControlCommandV2(l)
		if errors.As(err, &vcErr) {
			return vcErr.ID, ""
		}
		return cmd.ID(), cmd.Kind()
	}
	cmd, err := schema.ParseVoiceControlCommand(l)
	if errors.As(err, &vcErr) {
		return vcErr.ID, ""
	}
	return cmd.ID(), cmd.Kind()
}

// TestTranscriptsEndToEndV2 replays the helper side of each v2 transcript the
// supervisor can produce the owner side of, and the fake holds the
// supervisor's commands to the transcript byte for byte. (The others show an
// owner the supervisor is not: one that speaks only v1, one that writes a v1
// line in a v2 session, and one that writes commands the schema refuses.)
//
// The replay writes the helper's steps as soon as the owner's step before them
// has come, so the status is checked only where the transcript waits for the
// owner; TestV2Commands checks the status each ack follows.
func TestTranscriptsEndToEndV2(t *testing.T) {
	const usbOut = "alsa_output.usb-Generic_USB_Audio-00.analog-stereo"
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		file  string
		drive func(t *testing.T, ctx context.Context, s *Supervisor)
	}{
		{
			file: "transcript-negotiation-v2-owner.json",
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				must(t, s.Join(ctx, testChannel))
				must(t, s.Refresh(ctx))
				if st := s.Status(); st.Protocol != schema.VoiceControlSchemaV2 || st.Playback != schema.VoiceControlPlaybackOn || st.TalkMode != schema.VoiceControlTalkModePushToTalk {
					t.Errorf("status %+v", st)
				}
			},
		},
		{
			file: "transcript-device-missing-after-switch.json",
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				must(t, s.Join(ctx, testChannel))
				must(t, s.SetDevices(ctx, "alsa_input.usb-Generic_USB_Audio-00.mono-fallback", ""))
				seqAtLeast(t, s, 8)
				if st := s.Status(); st.Capture != schema.VoiceControlCaptureOn || len(st.Blocked) != 0 {
					t.Errorf("once the microphone appeared: %+v", st)
				}
				must(t, s.SetDevices(ctx, "", usbOut))
				seqAtLeast(t, s, 11)
				if st := s.Status(); st.Playback != schema.VoiceControlPlaybackOn {
					t.Errorf("once the speakers appeared: %+v", st)
				}
				must(t, s.SetDevices(ctx, "", schema.VoiceControlDeviceDefault))
			},
		},
		{
			file: "transcript-microphone-switch-while-transmitting.json",
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				must(t, s.Join(ctx, testChannel))
				seqAtLeast(t, s, 6)
				must(t, s.SetDevices(ctx, "", usbOut))
				if st := s.Status(); !st.Transmitting {
					t.Errorf("switching the speakers ended the transmission: %+v", st)
				}
				must(t, s.SetDevices(ctx, "speech_out", ""))
				seqAtLeast(t, s, 11)
				if st := s.Status(); st.Transmitting {
					t.Errorf("after the press on the new microphone ended: %+v", st)
				}
			},
		},
		{
			file: "transcript-mode-switch-while-transmitting.json",
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				must(t, s.Join(ctx, testChannel))
				seqAtLeast(t, s, 6)
				must(t, s.SetTalkMode(ctx, schema.VoiceControlTalkModeOpen))
				seqAtLeast(t, s, 11)
				if st := s.Status(); st.TalkMode != schema.VoiceControlTalkModeOpen || st.Transmitting || st.PTT != schema.VoiceControlPTTLost || len(st.Blocked) != 0 {
					t.Errorf("open, the key device lost, quiet: %+v", st)
				}
				must(t, s.SetTalkMode(ctx, schema.VoiceControlTalkModePushToTalk))
				seqAtLeast(t, s, 15)
				if st := s.Status(); st.TalkMode != schema.VoiceControlTalkModePushToTalk || !st.Transmitting {
					t.Errorf("push-to-talk, a press: %+v", st)
				}
				must(t, s.SetTalkMode(ctx, schema.VoiceControlTalkModeOpen))
				must(t, s.SetTalkMode(ctx, schema.VoiceControlTalkModeOpen))
			},
		},
		{
			file: "transcript-open-mode-entered-loud.json",
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				must(t, s.Join(ctx, testChannel))
				must(t, s.SetTalkMode(ctx, schema.VoiceControlTalkModeOpen))
				if st := s.Status(); st.Transmitting || st.TalkMode != schema.VoiceControlTalkModeOpen {
					t.Errorf("the switch to open: %+v", st)
				}
				must(t, s.Refresh(ctx))
				seqAtLeast(t, s, 10)
				must(t, s.SetDevices(ctx, "speech_out", ""))
				if st := s.Status(); st.Transmitting {
					t.Errorf("the switch of microphone left it transmitting: %+v", st)
				}
				must(t, s.Refresh(ctx))
				seqAtLeast(t, s, 16)
			},
		},
		{
			file: "transcript-open-mode.json",
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				must(t, s.Join(ctx, testChannel))
				seqAtLeast(t, s, 8)
				must(t, s.SetMute(ctx, true))
				if st := s.Status(); !st.Muted || st.Transmitting || st.TalkMode != schema.VoiceControlTalkModeOpen {
					t.Errorf("muted in open mode: %+v", st)
				}
				must(t, s.SetMute(ctx, false))
				seqAtLeast(t, s, 16)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			s := newSupervisor(t, storedProfile(t), Timings{}, "-replay", filepath.Join(fixtureDirV2, tt.file))
			ctx := ctxFor(t)
			tt.drive(t, ctx, s)
			must(t, s.Leave(ctx))
			waitReaped(t, s)
			assertNoMismatch(t, s)
			if got := exitCode(t, s); got != 0 {
				t.Errorf("the helper exited %d (diagnostics %q)", got, s.Diagnostics())
			}
			if st := s.Status(); st.Phase != PhaseIdle {
				t.Errorf("after leave: %+v, want idle", st)
			}
			assertGone(t, s.lastExit().pid)
		})
	}
}

// TestSetTalkModeSaves: the talk mode is saved with `config set
// audio.talk_mode -- <mode>`, after "--" as a device name is; a mode of no
// version is refused before anything runs; a conch-voice older than the key
// is RunUnsupported.
func TestSetTalkModeSaves(t *testing.T) {
	tests := []struct {
		name    string
		mode    schema.VoiceControlTalkMode
		args    []string
		argv    []string // what the fake was run with, after its path; nil if it must not run
		wantRun string   // the RunError reason, "" for success
	}{
		{name: "open", mode: schema.VoiceControlTalkModeOpen, argv: []string{"config", "set", "audio.talk_mode", "--", "open"}},
		{name: "push-to-talk", mode: schema.VoiceControlTalkModePushToTalk, argv: []string{"config", "set", "audio.talk_mode", "--", "push_to_talk"}},
		{name: "an older conch-voice", mode: schema.VoiceControlTalkModeOpen, args: []string{"-exit", "2"},
			argv: []string{"config", "set", "audio.talk_mode", "--", "open"}, wantRun: RunUnsupported},
		{name: "a mode of no version", mode: "always_on"},
		{name: "no mode", mode: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dumpFile := filepath.Join(t.TempDir(), "dump")
			o := newOneShot(storedProfile(t), append([]string{"-dump", dumpFile}, tt.args...)...)
			err := o.SetTalkMode(ctxFor(t), tt.mode)
			_, statErr := os.Stat(dumpFile)
			if tt.argv == nil {
				if err == nil || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("%v, and ran: %v; want refused before running", err, statErr == nil)
				}
				return
			}
			switch re := runErrOf(err); {
			case tt.wantRun == "" && err != nil:
				t.Fatal(err)
			case tt.wantRun != "" && (re == nil || re.Reason != tt.wantRun):
				t.Fatalf("%v, want %s", err, tt.wantRun)
			}
			got := readDump(t, dumpFile)["arg"]
			if len(got) < 1+len(tt.argv) || !slices.Equal(got[1:1+len(tt.argv)], tt.argv) {
				t.Errorf("argv %q, want %q after the path", got, tt.argv)
			}
		})
	}
}
