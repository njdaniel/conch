package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// voiceControlV2Dir is the fixture directory of v2, read by these tests and
// by the Rust helper's once it speaks v2 (issue #656).
const voiceControlV2Dir = "voice-control-v2"

// voiceControlV2Golden is every single-message fixture of v2 and the type it
// is. The hello is v1 (its shape is frozen) and is read with the v1 reader.
var voiceControlV2Golden = []struct {
	file  string
	event bool
	v1    bool
	new   func() any
}{
	{file: "command-start.json", new: func() any { return new(VoiceControlStartV2) }},
	{file: "command-set-mute.json", new: func() any { return new(VoiceControlSetMuteV2) }},
	{file: "command-set-deafen.json", new: func() any { return new(VoiceControlSetDeafenV2) }},
	{file: "command-get-state.json", new: func() any { return new(VoiceControlGetStateV2) }},
	{file: "command-leave.json", new: func() any { return new(VoiceControlLeaveV2) }},
	{file: "command-set-devices.json", new: func() any { return new(VoiceControlSetDevicesV2) }},
	{file: "command-set-devices-input.json", new: func() any { return new(VoiceControlSetDevicesV2) }},
	{file: "command-set-devices-default.json", new: func() any { return new(VoiceControlSetDevicesV2) }},
	{file: "command-set-talk-mode.json", new: func() any { return new(VoiceControlSetTalkModeV2) }},
	{file: "event-hello-v1-and-v2.json", event: true, v1: true, new: func() any { return new(VoiceControlHelloV1) }},
	{file: "event-ack.json", event: true, new: func() any { return new(VoiceControlAckV2) }},
	{file: "event-ack-refused.json", event: true, new: func() any { return new(VoiceControlAckV2) }},
	{file: "event-protocol-error.json", event: true, new: func() any { return new(VoiceControlProtocolErrorV2) }},
	{file: "event-state-starting.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-connected.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-transmitting.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-no-speakers.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-no-microphone.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-open-listening.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-open-transmitting.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-open-muted.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-open-key-device-lost.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-open-max-transmit.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
	{file: "event-state-stopped.json", event: true, new: func() any { return new(VoiceControlStateV2) }},
}

func readVoiceControlV2Fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", voiceControlV2Dir, filepath.FromSlash(name))) // #nosec G304 -- a fixture name from this package's own tables
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestVoiceControlV2GoldenFixtures checks each single-message fixture as the
// v1 test does: it round-trips through its type byte for byte, its compact
// form is accepted by the reader of its direction and version, and
// MarshalVoiceControlLine writes exactly that compact form.
func TestVoiceControlV2GoldenFixtures(t *testing.T) {
	for _, tt := range voiceControlV2Golden {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, voiceControlV2Dir+"/"+tt.file, tt.new)

			var compact bytes.Buffer
			if err := json.Compact(&compact, readVoiceControlV2Fixture(t, tt.file)); err != nil {
				t.Fatal(err)
			}
			var msg VoiceControlMessage
			switch {
			case tt.v1:
				ev, err := ParseVoiceControlEvent(compact.Bytes())
				if err != nil {
					t.Fatalf("the v1 reader refuses it: %v", err)
				}
				msg = ev.message()
			case tt.event:
				ev, err := ParseVoiceControlEventV2(compact.Bytes())
				if err != nil {
					t.Fatalf("the owner's reader refuses it: %v", err)
				}
				msg = ev.message()
			default:
				cmd, err := ParseVoiceControlCommandV2(compact.Bytes())
				if err != nil {
					t.Fatalf("the helper's reader refuses it: %v", err)
				}
				msg = cmd.message()
			}
			if msg == nil {
				t.Fatal("read as nothing")
			}
			line, err := MarshalVoiceControlLine(msg)
			if err != nil {
				t.Fatal(err)
			}
			if want := append(compact.Bytes(), '\n'); !bytes.Equal(line, want) {
				t.Errorf("written as\n%s\nwant\n%s", line, want)
			}
		})
	}
}

// message is the one message a v2 command holds, for the tests.
func (c VoiceControlCommandV2) message() VoiceControlMessage {
	switch {
	case c.Start != nil:
		return c.Start
	case c.SetMute != nil:
		return c.SetMute
	case c.SetDeafen != nil:
		return c.SetDeafen
	case c.GetState != nil:
		return c.GetState
	case c.Leave != nil:
		return c.Leave
	case c.SetDevices != nil:
		return c.SetDevices
	case c.SetTalkMode != nil:
		return c.SetTalkMode
	}
	return nil
}

// message is the one message a v2 event holds, for the tests.
func (e VoiceControlEventV2) message() VoiceControlMessage {
	switch {
	case e.Ack != nil:
		return e.Ack
	case e.State != nil:
		return e.State
	case e.ProtocolError != nil:
		return e.ProtocolError
	}
	return nil
}

// kind is the kind of the v2 event held, or the unknown kind skipped.
func (e VoiceControlEventV2) kind() VoiceControlEventKind {
	switch {
	case e.Ack != nil:
		return VoiceControlEventAck
	case e.State != nil:
		return VoiceControlEventState
	case e.ProtocolError != nil:
		return VoiceControlEventProtocolError
	}
	return e.Unknown
}

func loadVoiceControlV2Cases(t *testing.T) []voiceControlCase {
	t.Helper()
	var doc struct {
		Cases []voiceControlCase `json:"cases"`
	}
	if err := json.Unmarshal(readVoiceControlV2Fixture(t, "cases.json"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no cases")
	}
	return doc.Cases
}

// lineV2 is the case's line, from whichever of its three forms it has, read
// from the v2 directory.
func (c voiceControlCase) lineV2(t *testing.T) []byte {
	t.Helper()
	if c.LineFile != "" {
		return readVoiceControlV2Fixture(t, c.LineFile)
	}
	return c.line(t)
}

// TestVoiceControlV2Cases runs the v2 conformance cases: each line is read by
// the v2 reader of its direction, and accepted, refused with the code (and
// id) given, or skipped.
func TestVoiceControlV2Cases(t *testing.T) {
	names := map[string]bool{}
	for _, c := range loadVoiceControlV2Cases(t) {
		key := c.Direction + ": " + c.Name
		if names[key] {
			t.Errorf("case %q appears twice", key)
		}
		names[key] = true
		if c.Want.OffersV1 != nil {
			t.Errorf("case %q: no hello comes out of the v2 reader", key)
		}
		t.Run(key, func(t *testing.T) {
			line := c.lineV2(t)
			var kind string
			var err error
			switch c.Direction {
			case "command":
				var cmd VoiceControlCommandV2
				cmd, err = ParseVoiceControlCommandV2(line)
				kind = string(cmd.Kind())
			case "event":
				var ev VoiceControlEventV2
				ev, err = ParseVoiceControlEventV2(line)
				kind = string(ev.kind())
				if err == nil && ev.Unknown != "" && c.Want.Result != "skip" {
					t.Fatalf("skipped as an unknown kind %q, want %s", ev.Unknown, c.Want.Result)
				}
			default:
				t.Fatalf("direction %q is neither command nor event", c.Direction)
			}
			switch c.Want.Result {
			case "accept", "skip":
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if kind != c.Want.Kind {
					t.Errorf("read as %q, want %q", kind, c.Want.Kind)
				}
			case "refuse":
				var vcErr *VoiceControlError
				if !errors.As(err, &vcErr) {
					t.Fatalf("accepted as %q (error %v), want refused with %s", kind, err, c.Want.Code)
				}
				if string(vcErr.Code) != c.Want.Code || vcErr.ID != c.Want.ID {
					t.Errorf("refused with %s (id %d): %v; want %s (id %d)", vcErr.Code, vcErr.ID, err, c.Want.Code, c.Want.ID)
				}
				if len(line) > 0 && strings.Contains(err.Error(), string(line)) {
					t.Errorf("the error repeats the line: %v", err)
				}
			default:
				t.Fatalf("want.result %q is not accept, refuse or skip", c.Want.Result)
			}
		})
	}
}

// TestVoiceControlV2CasesCoverEveryCode makes sure each code a v2 reader can
// return is shown by at least one case.
func TestVoiceControlV2CasesCoverEveryCode(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range loadVoiceControlV2Cases(t) {
		if c.Want.Result == "refuse" {
			seen[c.Direction+":"+c.Want.Code] = true
		}
	}
	for _, want := range []string{
		"command:too_long", "command:malformed", "command:unknown_schema", "command:invalid", "command:unknown_kind",
		"event:too_long", "event:malformed", "event:unknown_schema", "event:invalid",
	} {
		if !seen[want] {
			t.Errorf("no case refuses with %s", want)
		}
	}
}

// TestVoiceControlV2ReadersKeepVersionsApart pins the acceptance criterion
// in both directions: a v2 line read as v1 is unknown_schema, and a v1 line
// read as v2 is unknown_schema, for commands and events alike.
func TestVoiceControlV2ReadersKeepVersionsApart(t *testing.T) {
	v1Command := readVoiceControlFixture(t, "command-set-mute.json")
	v1Event := readVoiceControlFixture(t, "event-state-connected.json")
	v2Command := readVoiceControlV2Fixture(t, "command-set-mute.json")
	v2Event := readVoiceControlV2Fixture(t, "event-state-connected.json")
	compact := func(data []byte) []byte {
		var buf bytes.Buffer
		if err := json.Compact(&buf, data); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	tests := []struct {
		name string
		err  error
	}{
		{"a v2 command read as v1", func() error { _, err := ParseVoiceControlCommand(compact(v2Command)); return err }()},
		{"a v2 event read as v1", func() error { _, err := ParseVoiceControlEvent(compact(v2Event)); return err }()},
		{"a v1 command read as v2", func() error { _, err := ParseVoiceControlCommandV2(compact(v1Command)); return err }()},
		{"a v1 event read as v2", func() error { _, err := ParseVoiceControlEventV2(compact(v1Event)); return err }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var vcErr *VoiceControlError
			if !errors.As(tt.err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownSchema || vcErr.ID != 0 {
				t.Errorf("got %v, want unknown_schema with no id", tt.err)
			}
		})
	}
	t.Run("the hello offers both and is read by v1", func(t *testing.T) {
		ev, err := ParseVoiceControlEvent(compact(readVoiceControlV2Fixture(t, "event-hello-v1-and-v2.json")))
		if err != nil || ev.Hello == nil {
			t.Fatalf("read as %+v, %v", ev, err)
		}
		if !ev.Hello.Offers(VoiceControlSchemaV1) || !ev.Hello.Offers(VoiceControlSchemaV2) {
			t.Errorf("offers %v", ev.Hello.Versions)
		}
		if ev.Hello.Offers("conch.voice_control.v3") {
			t.Error("offers v3")
		}
	})
}

// TestVoiceControlCommandKindKnownIn pins which kinds each version has: the
// v1 reader must answer a v2 kind with unknown_kind, as it did before v2
// existed, the v2 reader knows exactly v1's five and its own two, and a kind
// of neither version (one a later version might add to the vocabulary) is
// known to neither and refused by both readers rather than read.
func TestVoiceControlCommandKindKnownIn(t *testing.T) {
	v1Kinds := []VoiceControlCommandKind{
		VoiceControlCommandStart, VoiceControlCommandSetMute, VoiceControlCommandSetDeafen,
		VoiceControlCommandGetState, VoiceControlCommandLeave,
	}
	v2Only := []VoiceControlCommandKind{VoiceControlCommandSetDevices, VoiceControlCommandSetTalkMode}
	if got := Vocabularies["VoiceControlCommandKind"]; len(got) != len(v1Kinds)+len(v2Only) {
		t.Errorf("the vocabulary has %d kinds, this test knows %d: a new kind needs a version here", len(got), len(v1Kinds)+len(v2Only))
	}
	for _, kind := range Vocabularies["VoiceControlCommandKind"] {
		k := VoiceControlCommandKind(kind)
		if !k.KnownIn(VoiceControlSchemaV2) {
			t.Errorf("%s is not known in v2", k)
		}
		if got, want := k.KnownIn(VoiceControlSchemaV1), slices.Contains(v1Kinds, k); got != want {
			t.Errorf("%s known in v1: %v, want %v", k, got, want)
		}
		if k.KnownIn("conch.voice_control.v3") || k.KnownIn("") {
			t.Errorf("%s is known in a version that does not exist", k)
		}
	}
	for _, k := range []VoiceControlCommandKind{"set_volume", "", "SET_MUTE"} {
		if k.KnownIn(VoiceControlSchemaV1) || k.KnownIn(VoiceControlSchemaV2) {
			t.Errorf("%q is known in a version", k)
		}
	}
	// A kind that is in the vocabulary but in neither version would reach
	// the v2 reader's switch with no case; it must be refused, not read as
	// nothing. No such kind exists today, so the check is on the readers'
	// answer to a kind outside the vocabulary, which takes the same path.
	for _, line := range []string{
		`{"schema":"conch.voice_control.v2","kind":"set_volume","id":7,"level":3}`,
		`{"schema":"conch.voice_control.v1","kind":"set_volume","id":7,"level":3}`,
	} {
		var err error
		if strings.Contains(line, VoiceControlSchemaV2) {
			_, err = ParseVoiceControlCommandV2([]byte(line))
		} else {
			_, err = ParseVoiceControlCommand([]byte(line))
		}
		var vcErr *VoiceControlError
		if !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownKind || vcErr.ID != 7 {
			t.Errorf("%s: %v, want unknown_kind for id 7", line, err)
		}
	}
	for _, k := range v2Only {
		line := []byte(`{"schema":"conch.voice_control.v1","kind":"` + string(k) + `","id":7,"input":"default","talk_mode":"open"}`)
		_, err := ParseVoiceControlCommand(line)
		var vcErr *VoiceControlError
		if !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownKind || vcErr.ID != 7 {
			t.Errorf("the v1 reader answers %s with %v, want unknown_kind for id 7", k, err)
		}
	}
}

// voiceControlV2Transcript is the view of a transcript step the checker
// needs, whichever version the session is in: the v1 owner's transcript
// lives in this directory too, because the helper that offers both is one
// helper.
type voiceControlAnyCommand struct {
	kind        VoiceControlCommandKind
	id          int64
	setMute     *bool
	setDeafen   *bool
	setDevices  *VoiceControlSetDevicesV2
	setTalkMode *VoiceControlTalkMode
}

type voiceControlAnyState struct {
	phase        VoiceControlPhase
	reason       VoiceControlReason
	transmitting bool
	muted        bool
	deafened     bool
	talkMode     VoiceControlTalkMode
}

type voiceControlAnyEvent struct {
	seq           int64
	hello         *VoiceControlHelloV1
	ack           *VoiceControlAckV2
	state         *voiceControlAnyState
	protocolError VoiceControlErrorCode
	unknown       bool
}

// parseVoiceControlAnyCommand reads an owner's line in the version given, or,
// before the version is fixed, in whichever of the offered versions the line
// names, as a helper offering both does.
func parseVoiceControlAnyCommand(line []byte, version string, offered []string) (voiceControlAnyCommand, error) {
	versions := []string{version}
	if version == "" {
		versions = offered
	}
	var last error
	for _, v := range versions {
		var cmd voiceControlAnyCommand
		var err error
		switch v {
		case VoiceControlSchemaV1:
			var c VoiceControlCommand
			c, err = ParseVoiceControlCommand(line)
			cmd = voiceControlAnyCommand{kind: c.Kind(), id: c.ID()}
			if c.SetMute != nil {
				cmd.setMute = &c.SetMute.Muted
			}
			if c.SetDeafen != nil {
				cmd.setDeafen = &c.SetDeafen.Deafened
			}
		case VoiceControlSchemaV2:
			var c VoiceControlCommandV2
			c, err = ParseVoiceControlCommandV2(line)
			cmd = voiceControlAnyCommand{kind: c.Kind(), id: c.ID(), setDevices: c.SetDevices}
			if c.SetMute != nil {
				cmd.setMute = &c.SetMute.Muted
			}
			if c.SetDeafen != nil {
				cmd.setDeafen = &c.SetDeafen.Deafened
			}
			if c.SetTalkMode != nil {
				cmd.setTalkMode = &c.SetTalkMode.TalkMode
			}
		default:
			continue
		}
		var vcErr *VoiceControlError
		if errors.As(err, &vcErr) && vcErr.Code == VoiceControlErrorUnknownSchema {
			last = err
			continue
		}
		return cmd, err
	}
	return voiceControlAnyCommand{}, last
}

// parseVoiceControlAnyEvent reads a helper's line: the hello with the v1
// reader, everything after with the session's version.
func parseVoiceControlAnyEvent(line []byte, version string) (voiceControlAnyEvent, error) {
	if version == VoiceControlSchemaV2 {
		ev, err := ParseVoiceControlEventV2(line)
		if err != nil {
			return voiceControlAnyEvent{}, err
		}
		out := voiceControlAnyEvent{seq: ev.Seq, ack: ev.Ack, unknown: ev.Unknown != ""}
		if ev.State != nil {
			out.state = &voiceControlAnyState{
				phase: ev.State.Phase, reason: ev.State.Reason, transmitting: ev.State.Transmitting,
				muted: ev.State.Muted, deafened: ev.State.Deafened, talkMode: ev.State.TalkMode,
			}
		}
		if ev.ProtocolError != nil {
			out.protocolError = ev.ProtocolError.Code
		}
		return out, nil
	}
	ev, err := ParseVoiceControlEvent(line)
	if err != nil {
		return voiceControlAnyEvent{}, err
	}
	out := voiceControlAnyEvent{seq: ev.Seq, hello: ev.Hello, unknown: ev.Unknown != ""}
	if ev.Ack != nil {
		out.ack = &VoiceControlAckV2{ID: ev.Ack.ID, OK: ev.Ack.OK, Error: ev.Ack.Error}
	}
	if ev.State != nil {
		out.state = &voiceControlAnyState{
			phase: ev.State.Phase, reason: ev.State.Reason, transmitting: ev.State.Transmitting,
			muted: ev.State.Muted, deafened: ev.State.Deafened, talkMode: VoiceControlTalkModePushToTalk,
		}
	}
	if ev.ProtocolError != nil {
		out.protocolError = ev.ProtocolError.Code
	}
	return out, nil
}

// TestVoiceControlV2Transcripts checks every v2 transcript against the
// ordering rules of the design note (§5), as TestVoiceControlTranscripts does
// for v1, and against v2's own: the hello is v1 and offers both versions, the
// owner's start fixes the version, every later line is read in it and a line
// of the other version is unknown_schema, a microphone switch during a
// transmission ends it before its ack, and a talk mode set is in the state
// before its ack.
func TestVoiceControlV2Transcripts(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", voiceControlV2Dir, "transcript-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 8 {
		t.Fatalf("found %d transcripts", len(files))
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			var doc struct {
				Description string             `json:"description"`
				Steps       []voiceControlStep `json:"steps"`
			}
			if err := json.Unmarshal(readVoiceControlV2Fixture(t, filepath.Base(file)), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Description == "" {
				t.Error("no description")
			}
			checkVoiceControlV2Transcript(t, doc.Steps)
		})
	}
}

// pendingCommandV2 is a command with a usable id that has not been answered,
// the answer it must get when that is already known, and what the latest
// state said when it was read.
type pendingCommandV2 struct {
	id              int64
	cmd             voiceControlAnyCommand
	refusal         VoiceControlErrorCode
	wasTransmitting bool
}

func checkVoiceControlV2Transcript(t *testing.T, steps []voiceControlStep) {
	t.Helper()
	var (
		seq, lastID       int64
		version           string
		hello             *VoiceControlHelloV1
		started, final    bool
		finalPhase        VoiceControlPhase
		state             *voiceControlAnyState
		previousWasState  bool
		pending           []pendingCommandV2
		wantProtocolError VoiceControlErrorCode
		wantFailedAfterPE bool
	)
	for i, step := range steps {
		forms := 0
		for _, set := range []bool{step.Message != nil, step.Close != "", step.Signal != "", step.Exit != nil} {
			if set {
				forms++
			}
		}
		if forms != 1 {
			t.Fatalf("step %d has %d actions, want 1", i, forms)
		}
		if step.Exit != nil {
			if step.From != "helper" || i != len(steps)-1 {
				t.Fatalf("step %d: only the helper exits, as the last step", i)
			}
			want := 0
			if finalPhase == VoiceControlPhaseFailed {
				want = 1
			}
			if *step.Exit != want {
				t.Errorf("exit %d after a final phase of %q, want %d", *step.Exit, finalPhase, want)
			}
			if started && !final {
				t.Error("a started helper exits without a final state")
			}
			if len(pending) != 0 {
				t.Errorf("commands %v never answered", pending)
			}
			return
		}
		if step.From == "owner" {
			switch {
			case step.Close != "":
				if step.Close != "stdin" {
					t.Fatalf("step %d: close %q", i, step.Close)
				}
				continue
			case step.Signal != "":
				if step.Signal != "SIGTERM" && step.Signal != "SIGINT" {
					t.Fatalf("step %d: signal %q", i, step.Signal)
				}
				continue
			}
			if hello == nil {
				t.Fatalf("step %d: the owner writes before a hello", i)
			}
			var line bytes.Buffer
			if err := json.Compact(&line, step.Message); err != nil {
				t.Fatal(err)
			}
			cmd, err := parseVoiceControlAnyCommand(line.Bytes(), version, hello.Versions)
			var vcErr *VoiceControlError
			if err != nil && !errors.As(err, &vcErr) {
				t.Fatalf("step %d: %v", i, err)
			}
			id := cmd.id
			if vcErr != nil {
				id = vcErr.ID
			}
			switch {
			case !started && cmd.kind != VoiceControlCommandStart:
				wantProtocolError = VoiceControlErrorNotStarted
				if vcErr != nil && vcErr.Code.Fatal() {
					wantProtocolError = vcErr.Code
				}
			case id == 0:
				wantProtocolError = vcErr.Code
			case id <= lastID:
				wantProtocolError = VoiceControlErrorIDNotIncreasing
			default:
				lastID = id
				p := pendingCommandV2{id: id, cmd: cmd, wasTransmitting: state != nil && state.transmitting}
				switch {
				case vcErr != nil:
					p.refusal = vcErr.Code
				case cmd.kind == VoiceControlCommandStart && started:
					p.refusal = VoiceControlErrorAlreadyStarted
				case cmd.kind == VoiceControlCommandStart:
					started = true
					// The start's schema fixes the version: the line was
					// accepted by exactly one reader.
					var s struct {
						Schema string `json:"schema"`
					}
					if err := json.Unmarshal(line.Bytes(), &s); err != nil {
						t.Fatal(err)
					}
					version = s.Schema
				case cmd.setMute != nil || cmd.setDeafen != nil || cmd.setDevices != nil || cmd.setTalkMode != nil:
					if state != nil && state.phase == VoiceControlPhaseStopping {
						p.refusal = VoiceControlErrorStopping
					}
				}
				pending = append(pending, p)
			}
			continue
		}

		if step.From != "helper" || step.Message == nil {
			t.Fatalf("step %d: from %q", i, step.From)
		}
		if final {
			t.Fatalf("step %d: the helper writes after its final state", i)
		}
		var line bytes.Buffer
		if err := json.Compact(&line, step.Message); err != nil {
			t.Fatal(err)
		}
		ev, err := parseVoiceControlAnyEvent(line.Bytes(), version)
		if err != nil {
			t.Fatalf("step %d: the owner's reader refuses it: %v", i, err)
		}
		if ev.seq <= seq {
			t.Fatalf("step %d: seq %d after %d", i, ev.seq, seq)
		}
		seq = ev.seq
		if hello == nil && ev.hello == nil {
			t.Fatalf("step %d: the first event is not a hello", i)
		}
		if wantFailedAfterPE && (ev.state == nil || ev.state.phase != VoiceControlPhaseFailed || ev.state.reason != VoiceControlReasonProtocolError) {
			t.Fatalf("step %d: a protocol_error is not followed by failed with reason protocol_error", i)
		}
		wasState := previousWasState
		previousWasState = ev.state != nil
		switch {
		case ev.hello != nil:
			if hello != nil {
				t.Fatalf("step %d: a second hello", i)
			}
			if !ev.hello.Offers(VoiceControlSchemaV1) || !ev.hello.Offers(VoiceControlSchemaV2) {
				t.Fatalf("step %d: a v2 helper offers both versions, this one offers %v", i, ev.hello.Versions)
			}
			hello = ev.hello
		case ev.protocolError != "":
			if wantProtocolError == "" || ev.protocolError != wantProtocolError {
				t.Fatalf("step %d: protocol_error %s, want %q", i, ev.protocolError, wantProtocolError)
			}
			wantProtocolError, wantFailedAfterPE = "", true
		case ev.state != nil:
			if wantProtocolError != "" {
				t.Fatalf("step %d: a state where a protocol_error %s was due", i, wantProtocolError)
			}
			if !started && !wantFailedAfterPE {
				t.Fatalf("step %d: a state before start was accepted", i)
			}
			wantFailedAfterPE = false
			state = ev.state
			if state.phase.Final() {
				final, finalPhase = true, state.phase
			}
		case ev.ack != nil:
			if len(pending) == 0 || pending[0].id != ev.ack.ID {
				t.Fatalf("step %d: ack for %d, the next unanswered command is %v", i, ev.ack.ID, pending)
			}
			p := pending[0]
			pending = pending[1:]
			if p.refusal != "" {
				if ev.ack.OK || ev.ack.Error != p.refusal {
					t.Errorf("step %d: command %d answered ok=%v %q, want refused with %s", i, p.id, ev.ack.OK, ev.ack.Error, p.refusal)
				}
				continue
			}
			if !ev.ack.OK {
				t.Errorf("step %d: command %d refused with %s for no reason a transcript can see", i, p.id, ev.ack.Error)
				continue
			}
			checkVoiceControlV2Effect(t, i, p, state, wasState)
		case ev.unknown:
		}
	}
	t.Fatal("the transcript does not end with the helper's exit")
}

// checkVoiceControlV2Effect checks that the state a command changed was
// written before its ack, with v2's two additions: a microphone switch ends a
// transmission that was under way, and the talk mode set is the one in force.
func checkVoiceControlV2Effect(t *testing.T, step int, p pendingCommandV2, state *voiceControlAnyState, previousWasState bool) {
	t.Helper()
	cmd := p.cmd
	switch {
	case state == nil:
		t.Errorf("step %d: %s acknowledged before any state", step, cmd.kind)
	case cmd.setMute != nil && state.muted != *cmd.setMute:
		t.Errorf("step %d: set_mute %v acknowledged while the latest state has muted %v", step, *cmd.setMute, state.muted)
	case cmd.setDeafen != nil && state.deafened != *cmd.setDeafen:
		t.Errorf("step %d: set_deafen %v acknowledged while the latest state has deafened %v", step, *cmd.setDeafen, state.deafened)
	case cmd.kind == VoiceControlCommandLeave && state.phase != VoiceControlPhaseStopping && !state.phase.Final():
		t.Errorf("step %d: leave acknowledged in phase %s", step, state.phase)
	case cmd.kind == VoiceControlCommandGetState && !previousWasState:
		t.Errorf("step %d: get_state acknowledged without a state just before", step)
	case cmd.setDevices != nil && cmd.setDevices.Input != "" && p.wasTransmitting && state.transmitting:
		t.Errorf("step %d: the microphone was switched during a transmission and the latest state still transmits", step)
	case cmd.setTalkMode != nil && state.talkMode != *cmd.setTalkMode:
		t.Errorf("step %d: set_talk_mode %s acknowledged while the latest state has %s", step, *cmd.setTalkMode, state.talkMode)
	case cmd.setTalkMode != nil && p.wasTransmitting && state.transmitting:
		t.Errorf("step %d: the talk mode was switched during a transmission and the latest state still transmits", step)
	}
}

// TestVoiceControlV2FixturesAreAllClaimed fails on a file in the v2 fixture
// directory that no test reads.
func TestVoiceControlV2FixturesAreAllClaimed(t *testing.T) {
	claimed := map[string]bool{"cases.json": true, "sensitive.json": true}
	for _, g := range voiceControlV2Golden {
		claimed[g.file] = true
	}
	for _, c := range loadVoiceControlV2Cases(t) {
		if c.LineFile != "" {
			claimed[c.LineFile] = true
		}
	}
	for _, f := range voiceControlV2FixtureFiles(t) {
		if strings.HasPrefix(f, "transcript-") && strings.HasSuffix(f, ".json") {
			continue
		}
		if !claimed[f] {
			t.Errorf("testdata/%s/%s is read by no test", voiceControlV2Dir, f)
		}
	}
}

// voiceControlV2FixtureFiles lists every file under the v2 fixture directory,
// by its path relative to it, with forward slashes.
func voiceControlV2FixtureFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("testdata", voiceControlV2Dir)
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// voiceControlV2Types is every message type of v2.
var voiceControlV2Types = []VoiceControlMessage{
	VoiceControlStartV2{}, VoiceControlSetMuteV2{}, VoiceControlSetDeafenV2{}, VoiceControlGetStateV2{}, VoiceControlLeaveV2{},
	VoiceControlSetDevicesV2{}, VoiceControlSetTalkModeV2{},
	VoiceControlAckV2{}, VoiceControlStateV2{}, VoiceControlProtocolErrorV2{},
}

// TestVoiceControlV2CarriesNoSecrets holds v2 to its sensitive.json as the v1
// test does: no field of any message is named like a credential or a room;
// no event field can carry free text (the device names the owner sends are
// never in an event); and no fixture holds a fake secret, except the one
// case that shows an unknown field named token being ignored.
func TestVoiceControlV2CarriesNoSecrets(t *testing.T) {
	var sensitive struct {
		FakeSecrets []string `json:"fake_secrets"`
		Fragments   []string `json:"forbidden_field_fragments"`
	}
	if err := json.Unmarshal(readVoiceControlV2Fixture(t, "sensitive.json"), &sensitive); err != nil {
		t.Fatal(err)
	}
	if len(sensitive.FakeSecrets) == 0 || len(sensitive.Fragments) == 0 {
		t.Fatal("sensitive.json lists nothing")
	}

	for _, msg := range voiceControlV2Types {
		typ := reflect.TypeOf(msg)
		event := strings.HasPrefix(typ.Name(), "VoiceControlAck") ||
			strings.HasPrefix(typ.Name(), "VoiceControlState") || strings.HasPrefix(typ.Name(), "VoiceControlProtocolError")
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			for _, frag := range sensitive.Fragments {
				if strings.Contains(strings.ToLower(tag), frag) || strings.Contains(strings.ToLower(f.Name), frag) {
					t.Errorf("%s.%s (%q) is named like a secret (%q)", typ.Name(), f.Name, tag, frag)
				}
			}
			if !event {
				continue
			}
			ft := f.Type
			if ft.Kind() == reflect.Slice {
				ft = ft.Elem()
			}
			if ft.Kind() != reflect.String {
				continue
			}
			_, vocabulary := Vocabularies[ft.Name()]
			if !vocabulary && f.Name != "Schema" {
				t.Errorf("%s.%s can carry free text; an event's strings must be vocabularies", typ.Name(), f.Name)
			}
		}
	}

	exempt := "start with a field it does not declare, named token: ignored, and never repeated"
	for _, name := range voiceControlV2FixtureFiles(t) {
		if name == "sensitive.json" {
			continue
		}
		data := readVoiceControlV2Fixture(t, name)
		if name == "cases.json" {
			for _, c := range loadVoiceControlV2Cases(t) {
				if c.Name == exempt {
					continue
				}
				line := c.lineV2(t)
				for _, secret := range sensitive.FakeSecrets {
					if bytes.Contains(line, []byte(secret)) {
						t.Errorf("case %q holds a fake secret", c.Name)
					}
				}
			}
			continue
		}
		for _, secret := range sensitive.FakeSecrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds a fake secret", name)
			}
		}
	}
}

// The fields of each v2 message are an exact list, as v1's are. The two new
// owner-to-helper text fields, input and output, were reviewed as device
// names under the rule of the device list; no event gained a text field.
func TestVoiceControlV2FieldsAreAnExactList(t *testing.T) {
	head := []string{"schema", "kind"}
	want := map[string][]string{
		"VoiceControlStartV2":         append(slices.Clone(head), "id", "server", "channel", "principal_id", "credential", "muted", "deafened"),
		"VoiceControlSetMuteV2":       append(slices.Clone(head), "id", "muted"),
		"VoiceControlSetDeafenV2":     append(slices.Clone(head), "id", "deafened"),
		"VoiceControlGetStateV2":      append(slices.Clone(head), "id"),
		"VoiceControlLeaveV2":         append(slices.Clone(head), "id"),
		"VoiceControlSetDevicesV2":    append(slices.Clone(head), "id", "input", "output"),
		"VoiceControlSetTalkModeV2":   append(slices.Clone(head), "id", "talk_mode"),
		"VoiceControlAckV2":           append(slices.Clone(head), "seq", "id", "ok", "error"),
		"VoiceControlStateV2":         append(slices.Clone(head), "seq", "phase", "reason", "retry_in_ms", "capture", "playback", "transmitting", "muted", "deafened", "blocked", "talk_mode", "ptt", "ptt_problem"),
		"VoiceControlProtocolErrorV2": append(slices.Clone(head), "seq", "code"),
	}
	for _, msg := range voiceControlV2Types {
		typ := reflect.TypeOf(msg)
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
		}
		if !slices.Equal(got, want[typ.Name()]) {
			t.Errorf("%s has fields %v, the reviewed list is %v", typ.Name(), got, want[typ.Name()])
		}
	}
}

// validVoiceControlStateV2 is a connected, push-to-talk state with nothing
// blocking, for the Validate tables to vary.
func validVoiceControlStateV2() VoiceControlStateV2 {
	return VoiceControlStateV2{
		Schema: VoiceControlSchemaV2, Kind: VoiceControlEventState, Seq: 5,
		Phase: VoiceControlPhaseConnected, Capture: VoiceControlCaptureOn, Playback: VoiceControlPlaybackOn,
		Blocked: []VoiceControlBlock{}, TalkMode: VoiceControlTalkModePushToTalk, PTT: VoiceControlPTTReady,
	}
}

// TestVoiceControlStateV2Validate covers the rules v2 changed and added; the
// rules it shares with v1 are covered by the conformance cases.
func TestVoiceControlStateV2Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VoiceControlStateV2)
		wantErr string
	}{
		{name: "valid", mutate: func(*VoiceControlStateV2) {}},
		{name: "transmitting in push_to_talk with ptt ready", mutate: func(s *VoiceControlStateV2) { s.Transmitting = true }},
		{name: "transmitting in push_to_talk without ptt ready", mutate: func(s *VoiceControlStateV2) {
			s.Transmitting, s.PTT = true, VoiceControlPTTNotConfigured
		}, wantErr: "without ptt ready"},
		{name: "transmitting in open mode without a talk key", mutate: func(s *VoiceControlStateV2) {
			s.Transmitting, s.TalkMode, s.PTT = true, VoiceControlTalkModeOpen, VoiceControlPTTNotConfigured
		}},
		{name: "transmitting in open mode with the key device unavailable", mutate: func(s *VoiceControlStateV2) {
			s.Transmitting, s.TalkMode, s.PTT, s.PTTProblem = true, VoiceControlTalkModeOpen, VoiceControlPTTUnavailable, VoiceControlPTTProblemRefused
		}},
		{name: "transmitting in open mode with the key device lost", mutate: func(s *VoiceControlStateV2) {
			s.Transmitting, s.TalkMode, s.PTT, s.PTTProblem = true, VoiceControlTalkModeOpen, VoiceControlPTTLost, VoiceControlPTTProblemEnded
		}},
		{name: "transmitting in open mode while blocked", mutate: func(s *VoiceControlStateV2) {
			s.Transmitting, s.TalkMode, s.Blocked = true, VoiceControlTalkModeOpen, []VoiceControlBlock{VoiceControlBlockMaxTransmit}
		}, wantErr: "while a reason blocks it"},
		{name: "transmitting in open mode without capture", mutate: func(s *VoiceControlStateV2) {
			s.Transmitting, s.TalkMode, s.Capture, s.Blocked = true, VoiceControlTalkModeOpen, VoiceControlCaptureUnavailable, []VoiceControlBlock{VoiceControlBlockNoMicrophone}
		}, wantErr: "while a reason blocks it"},
		{name: "key_device_lost in open mode", mutate: func(s *VoiceControlStateV2) {
			s.TalkMode, s.PTT, s.PTTProblem, s.Blocked = VoiceControlTalkModeOpen, VoiceControlPTTLost, VoiceControlPTTProblemEnded, []VoiceControlBlock{VoiceControlBlockKeyDeviceLost}
		}, wantErr: "cannot block with key_device_lost"},
		{name: "key_device_lost in open mode with ptt ready", mutate: func(s *VoiceControlStateV2) {
			s.TalkMode, s.Blocked = VoiceControlTalkModeOpen, []VoiceControlBlock{VoiceControlBlockKeyDeviceLost}
		}, wantErr: "cannot block with key_device_lost"},
		{name: "ptt lost in push_to_talk without key_device_lost", mutate: func(s *VoiceControlStateV2) {
			s.PTT, s.PTTProblem = VoiceControlPTTLost, VoiceControlPTTProblemEnded
		}, wantErr: "key_device_lost"},
		{name: "key_device_lost in push_to_talk with ptt ready", mutate: func(s *VoiceControlStateV2) {
			s.Blocked = []VoiceControlBlock{VoiceControlBlockKeyDeviceLost}
		}, wantErr: "key_device_lost"},
		{name: "max_transmit in open mode", mutate: func(s *VoiceControlStateV2) {
			s.TalkMode, s.Blocked = VoiceControlTalkModeOpen, []VoiceControlBlock{VoiceControlBlockMaxTransmit}
		}},
		{name: "playback unavailable while connected", mutate: func(s *VoiceControlStateV2) { s.Playback = VoiceControlPlaybackUnavailable }},
		{name: "playback unavailable does not block transmitting", mutate: func(s *VoiceControlStateV2) {
			s.Playback, s.Transmitting = VoiceControlPlaybackUnavailable, true
		}},
		{name: "playback off while connected", mutate: func(s *VoiceControlStateV2) { s.Playback = VoiceControlPlaybackOff },
			wantErr: "playback on or unavailable"},
		{name: "playback off while connecting", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Capture, s.Playback, s.Blocked = VoiceControlPhaseConnecting, VoiceControlCaptureOff, VoiceControlPlaybackOff, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}, wantErr: "playback on or unavailable"},
		{name: "playback off while starting", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Capture, s.Playback, s.PTT, s.Blocked = VoiceControlPhaseStarting, VoiceControlCaptureOff, VoiceControlPlaybackOff, VoiceControlPTTUnknown, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}},
		{name: "playback on while starting", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Capture, s.PTT, s.Blocked = VoiceControlPhaseStarting, VoiceControlCaptureOff, VoiceControlPTTUnknown, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}, wantErr: "must have playback off"},
		{name: "playback on while stopped", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Reason, s.Capture, s.Blocked = VoiceControlPhaseStopped, VoiceControlReasonLeft, VoiceControlCaptureOff, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}, wantErr: "must have playback off"},
		{name: "playback unavailable while failed", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Reason, s.Capture, s.Playback, s.Blocked = VoiceControlPhaseFailed, VoiceControlReasonInternalError, VoiceControlCaptureOff, VoiceControlPlaybackUnavailable, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}, wantErr: "must have playback off"},
		{name: "playback on while stopping", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Reason, s.Blocked = VoiceControlPhaseStopping, VoiceControlReasonLeft, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}},
		{name: "playback off while stopping", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Reason, s.Capture, s.Playback, s.Blocked = VoiceControlPhaseStopping, VoiceControlReasonLeft, VoiceControlCaptureOff, VoiceControlPlaybackOff, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}},
		{name: "open mode while starting", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Capture, s.Playback, s.PTT, s.TalkMode, s.Blocked = VoiceControlPhaseStarting, VoiceControlCaptureOff, VoiceControlPlaybackOff, VoiceControlPTTUnknown, VoiceControlTalkModeOpen, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}, wantErr: "talk_mode push_to_talk"},
		{name: "open mode while failed", mutate: func(s *VoiceControlStateV2) {
			s.Phase, s.Reason, s.Capture, s.Playback, s.TalkMode, s.Blocked = VoiceControlPhaseFailed, VoiceControlReasonDuplicateIdentity, VoiceControlCaptureOff, VoiceControlPlaybackOff, VoiceControlTalkModeOpen, []VoiceControlBlock{VoiceControlBlockNotConnected}
		}},
		{name: "no talk_mode", mutate: func(s *VoiceControlStateV2) { s.TalkMode = "" }, wantErr: "talk_mode"},
		{name: "no playback", mutate: func(s *VoiceControlStateV2) { s.Playback = "" }, wantErr: "playback"},
		{name: "a v1 schema", mutate: func(s *VoiceControlStateV2) { s.Schema = VoiceControlSchemaV1 }, wantErr: "schema must be"},
		{name: "a reason while connected", mutate: func(s *VoiceControlStateV2) { s.Reason = VoiceControlReasonLeft }, wantErr: "must have no reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validVoiceControlStateV2()
			tt.mutate(&s)
			err := s.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestVoiceControlSetDevicesV2Validate(t *testing.T) {
	longest := strings.Repeat("n", VoiceDevicesMaxNameBytes)
	tests := []struct {
		name          string
		input, output string
		wantErr       string
	}{
		{name: "both", input: "speech_out", output: "default"},
		{name: "input only", input: "alsa_input.pci-0000_00_1f.3.analog-stereo"},
		{name: "output only", output: "alsa_output.pci-0000_00_1f.3.analog-stereo"},
		{name: "the default", input: VoiceControlDeviceDefault},
		{name: "the longest name", input: longest, output: longest},
		{name: "another script", input: "микрофон"},
		{name: "neither", wantErr: "must name input, output or both"},
		{name: "input too long", input: longest + "n", wantErr: "input must be 1 to"},
		{name: "output too long", output: longest + "n", wantErr: "output must be 1 to"},
		{name: "a quotation mark", input: `my "mic"`, wantErr: "quotation mark"},
		{name: "a backslash", output: `a\b`, wantErr: "backslash"},
		{name: "a control character", input: "\x1b[2Jspeech_out", wantErr: "control characters"},
		{name: "a line feed", output: "two\nlines", wantErr: "control characters"},
		{name: "not UTF-8", input: "\xc3\x28", wantErr: "valid UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := VoiceControlSetDevicesV2{Schema: VoiceControlSchemaV2, Kind: VoiceControlCommandSetDevices, ID: 6, Input: tt.input, Output: tt.output}
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
			// The rule is the device list's: a name the list shows is a
			// name set_devices takes, and the other way round.
			for _, name := range []string{tt.input, tt.output} {
				if name == "" {
					continue
				}
				if got, want := validateVoiceControlDeviceName("x", name) == nil, validateVoiceDeviceName("x", name) == nil; got != want {
					t.Errorf("set_devices accepts %q: %v, the device list accepts it: %v", name, got, want)
				}
			}
		})
	}
}

func TestVoiceControlSetTalkModeV2Validate(t *testing.T) {
	for _, tt := range []struct {
		mode    VoiceControlTalkMode
		wantErr bool
	}{
		{VoiceControlTalkModePushToTalk, false}, {VoiceControlTalkModeOpen, false},
		{"", true}, {"always_on", true}, {"ptt", true}, {"Open", true},
	} {
		c := VoiceControlSetTalkModeV2{Schema: VoiceControlSchemaV2, Kind: VoiceControlCommandSetTalkMode, ID: 7, TalkMode: tt.mode}
		if err := c.Validate(); (err != nil) != tt.wantErr {
			t.Errorf("talk_mode %q: Validate() = %v", tt.mode, err)
		}
	}
}

func TestMarshalVoiceControlLineV2(t *testing.T) {
	t.Run("the longest set_devices fits on a line", func(t *testing.T) {
		// Four-byte characters are written as themselves, so the longest
		// line is two names of VoiceDevicesMaxNameBytes bytes.
		name := strings.Repeat("\U0001f3a4", VoiceDevicesMaxNameBytes/4) + strings.Repeat("n", VoiceDevicesMaxNameBytes%4)
		cmd := VoiceControlSetDevicesV2{Schema: VoiceControlSchemaV2, Kind: VoiceControlCommandSetDevices, ID: VoiceControlMaxID, Input: name, Output: name}
		line, err := MarshalVoiceControlLine(cmd)
		if err != nil {
			t.Fatal(err)
		}
		if len(line)-1 > VoiceControlMaxLineBytes {
			t.Fatalf("%d bytes", len(line)-1)
		}
		got, err := ParseVoiceControlCommandV2(bytes.TrimSuffix(line, []byte{'\n'}))
		if err != nil || !reflect.DeepEqual(*got.SetDevices, cmd) {
			t.Fatalf("read back as %+v, %v", got.SetDevices, err)
		}
	})
	t.Run("a side left out is not written", func(t *testing.T) {
		line, err := MarshalVoiceControlLine(VoiceControlSetDevicesV2{Schema: VoiceControlSchemaV2, Kind: VoiceControlCommandSetDevices, ID: 6, Output: "default"})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(line, []byte(`"input"`)) {
			t.Errorf("wrote %s", line)
		}
	})
	t.Run("an invalid message is not written", func(t *testing.T) {
		for _, msg := range []VoiceControlMessage{
			VoiceControlSetDevicesV2{Schema: VoiceControlSchemaV2, Kind: VoiceControlCommandSetDevices, ID: 6},
			VoiceControlSetDevicesV2{Schema: VoiceControlSchemaV1, Kind: VoiceControlCommandSetDevices, ID: 6, Input: "default"},
			VoiceControlSetTalkModeV2{Schema: VoiceControlSchemaV2, Kind: VoiceControlCommandSetTalkMode, ID: 7, TalkMode: "always_on"},
			VoiceControlSetMuteV2{Schema: VoiceControlSchemaV1, Kind: VoiceControlCommandSetMute, ID: 1},
			VoiceControlStateV2{Schema: VoiceControlSchemaV2, Kind: VoiceControlEventState, Seq: 1, Phase: VoiceControlPhaseConnected, Capture: VoiceControlCaptureOn, Playback: VoiceControlPlaybackOn, PTT: VoiceControlPTTReady, TalkMode: VoiceControlTalkModeOpen, Blocked: []VoiceControlBlock{VoiceControlBlockKeyDeviceLost}},
		} {
			if line, err := MarshalVoiceControlLine(msg); err == nil {
				t.Errorf("wrote %s", line)
			}
		}
	})
}

func TestVoiceControlV2CommandAccessors(t *testing.T) {
	if (VoiceControlCommandV2{}).Kind() != "" || (VoiceControlCommandV2{}).ID() != 0 {
		t.Error("an empty command has a kind or an id")
	}
	for _, c := range []VoiceControlCommandV2{
		{Start: &VoiceControlStartV2{ID: 1}},
		{SetMute: &VoiceControlSetMuteV2{ID: 2}},
		{SetDeafen: &VoiceControlSetDeafenV2{ID: 3}},
		{GetState: &VoiceControlGetStateV2{ID: 4}},
		{Leave: &VoiceControlLeaveV2{ID: 5}},
		{SetDevices: &VoiceControlSetDevicesV2{ID: 6}},
		{SetTalkMode: &VoiceControlSetTalkModeV2{ID: 7}},
	} {
		if !c.Kind().KnownIn(VoiceControlSchemaV2) || c.ID() == 0 {
			t.Errorf("%+v: kind %q, id %d", c, c.Kind(), c.ID())
		}
	}
}

// TestVoiceControlV2Doc keeps the design note's v2 section and this package
// together: every v2 fixture is named in it, and so are the v2 schema name
// and the two new commands.
func TestVoiceControlV2Doc(t *testing.T) {
	data, err := os.ReadFile(voiceControlDoc) // #nosec G304 -- this repository's own design note, by a fixed path
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	for _, f := range voiceControlV2FixtureFiles(t) {
		if !strings.Contains(doc, f) {
			t.Errorf("the design note does not name the fixture %s", f)
		}
	}
	for _, want := range []string{
		"`" + VoiceControlSchemaV2 + "`", "`" + string(VoiceControlCommandSetDevices) + "`", "`" + string(VoiceControlCommandSetTalkMode) + "`",
		voiceControlV2Dir + "/",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the design note does not name %s", want)
		}
	}
}
