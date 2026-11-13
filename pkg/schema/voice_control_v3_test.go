package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// voiceControlV3Dir is the fixture directory of v3, read by these tests, by
// the Rust copy (voice/crates/conch-voice-api/src/control_v3.rs) and by the
// desktop's (desktop/tests/unit/voice-protocol.test.ts).
const voiceControlV3Dir = "voice-control-v3"

// voiceControlV3Hello is the hello of a helper that offers v1, v2 and v3. It
// is v1 (its shape is frozen) and is read with the v1 reader.
const voiceControlV3Hello = "event-hello-v1-v2-and-v3.json"

// voiceControlV3Golden is every single-message fixture of v3 and the type it
// is.
var voiceControlV3Golden = []struct {
	file  string
	event bool
	v1    bool
	new   func() any
}{
	{file: "command-start.json", new: func() any { return new(VoiceControlStartV3) }},
	{file: "command-set-mute.json", new: func() any { return new(VoiceControlSetMuteV3) }},
	{file: "command-set-deafen.json", new: func() any { return new(VoiceControlSetDeafenV3) }},
	{file: "command-get-state.json", new: func() any { return new(VoiceControlGetStateV3) }},
	{file: "command-leave.json", new: func() any { return new(VoiceControlLeaveV3) }},
	{file: "command-set-devices.json", new: func() any { return new(VoiceControlSetDevicesV3) }},
	{file: "command-set-devices-input.json", new: func() any { return new(VoiceControlSetDevicesV3) }},
	{file: "command-set-devices-default.json", new: func() any { return new(VoiceControlSetDevicesV3) }},
	{file: "command-set-talk-mode.json", new: func() any { return new(VoiceControlSetTalkModeV3) }},
	{file: "command-set-share-audio.json", new: func() any { return new(VoiceControlSetShareAudioV3) }},
	// The application scope is a valid command: a helper that cannot capture
	// one application refuses it into the state
	// (event-state-share-audio-unsupported-scope.json), not with an error.
	{file: "command-set-share-audio-application.json", new: func() any { return new(VoiceControlSetShareAudioV3) }},
	{file: "command-set-share-audio-stop.json", new: func() any { return new(VoiceControlSetShareAudioV3) }},
	{file: "command-set-share-audio-mute.json", new: func() any { return new(VoiceControlSetShareAudioMuteV3) }},
	{file: "command-set-share-audio-mute-off.json", new: func() any { return new(VoiceControlSetShareAudioMuteV3) }},
	{file: voiceControlV3Hello, event: true, v1: true, new: func() any { return new(VoiceControlHelloV1) }},
	{file: "event-ack.json", event: true, new: func() any { return new(VoiceControlAckV3) }},
	{file: "event-ack-refused.json", event: true, new: func() any { return new(VoiceControlAckV3) }},
	{file: "event-protocol-error.json", event: true, new: func() any { return new(VoiceControlProtocolErrorV3) }},
	// A state in each phase, with no share's sound asked for.
	{file: "event-state-starting.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-connecting.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-connected.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-reconnecting.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-stopping.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-stopped.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-failed.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	// The share's sound: on, each reason it is not, and its mute.
	{file: "event-state-share-audio-on.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-muted.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-no-sources.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-transmitting.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-muted-while-off.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-unsupported-scope.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-unsupported.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-no-pipewire.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-no-process-tree.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-publish-refused.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
	{file: "event-state-share-audio-not-connected.json", event: true, new: func() any { return new(VoiceControlStateV3) }},
}

func readVoiceControlV3Fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", voiceControlV3Dir, filepath.FromSlash(name))) // #nosec G304 -- a fixture name from this package's own tables
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// voiceControlReporter is what the transcript checker needs of a test, so
// that TestVoiceControlV3TranscriptRules can run it on a transcript that must
// fail and read why it did.
type voiceControlReporter interface {
	Helper()
	Error(args ...any)
	Errorf(format string, args ...any)
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

// compactVoiceControlFixture is a fixture as the one line it is written as.
func compactVoiceControlFixture(t voiceControlReporter, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestVoiceControlV3GoldenFixtures checks each single-message fixture as the
// v1 and v2 tests do: it round-trips through its type byte for byte, its
// compact form is accepted by the reader of its direction and version, and
// MarshalVoiceControlLine writes exactly that compact form.
func TestVoiceControlV3GoldenFixtures(t *testing.T) {
	for _, tt := range voiceControlV3Golden {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, voiceControlV3Dir+"/"+tt.file, tt.new)

			compact := compactVoiceControlFixture(t, readVoiceControlV3Fixture(t, tt.file))
			var msg VoiceControlMessage
			switch {
			case tt.v1:
				ev, err := ParseVoiceControlEvent(compact)
				if err != nil {
					t.Fatalf("the v1 reader refuses it: %v", err)
				}
				msg = ev.message()
			case tt.event:
				ev, err := ParseVoiceControlEventV3(compact)
				if err != nil {
					t.Fatalf("the owner's reader refuses it: %v", err)
				}
				msg = ev.message()
			default:
				cmd, err := ParseVoiceControlCommandV3(compact)
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
			if want := append(compact, '\n'); !bytes.Equal(line, want) {
				t.Errorf("written as\n%s\nwant\n%s", line, want)
			}
		})
	}
}

// TestVoiceControlV3GoldenFixturesCoverTheVocabularies makes sure the golden
// set shows what the design note asks of it: each command kind, a state in
// each phase, each share-audio state and each share-audio problem.
func TestVoiceControlV3GoldenFixturesCoverTheVocabularies(t *testing.T) {
	seen := map[string]map[string]bool{"kind": {}, "phase": {}, "share_audio": {}, "share_audio_problem": {}}
	for _, g := range voiceControlV3Golden {
		if g.v1 {
			continue
		}
		compact := compactVoiceControlFixture(t, readVoiceControlV3Fixture(t, g.file))
		if !g.event {
			cmd, err := ParseVoiceControlCommandV3(compact)
			if err != nil {
				t.Fatal(err)
			}
			seen["kind"][string(cmd.Kind())] = true
			continue
		}
		ev, err := ParseVoiceControlEventV3(compact)
		if err != nil {
			t.Fatal(err)
		}
		if ev.State != nil {
			seen["phase"][string(ev.State.Phase)] = true
			seen["share_audio"][string(ev.State.ShareAudio)] = true
			seen["share_audio_problem"][string(ev.State.ShareAudioProblem)] = true
		}
	}
	for field, vocabulary := range map[string]string{
		"kind": "VoiceControlCommandKindV3", "phase": "VoiceControlPhase",
		"share_audio": "VoiceControlShareAudio", "share_audio_problem": "VoiceControlShareAudioProblem",
	} {
		for _, word := range Vocabularies[vocabulary] {
			if !seen[field][word] {
				t.Errorf("no golden fixture has %s %q", field, word)
			}
		}
	}
}

// message is the one message a v3 command holds, for the tests.
func (c VoiceControlCommandV3) message() VoiceControlMessage {
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
	case c.SetShareAudio != nil:
		return c.SetShareAudio
	case c.SetShareAudioMute != nil:
		return c.SetShareAudioMute
	}
	return nil
}

// message is the one message a v3 event holds, for the tests.
func (e VoiceControlEventV3) message() VoiceControlMessage {
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

// kind is the kind of the v3 event held, or the unknown kind skipped.
func (e VoiceControlEventV3) kind() VoiceControlEventKind {
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

// voiceControlV3CaseFiles are the conformance documents of the v3 readers,
// read as one: what a string may hold is in a file of its own, as in v1 and
// v2.
var voiceControlV3CaseFiles = []string{"cases.json", "cases-strings.json"}

func loadVoiceControlV3Cases(t *testing.T) []voiceControlCase {
	t.Helper()
	var cases []voiceControlCase
	for _, file := range voiceControlV3CaseFiles {
		cases = append(cases, loadVoiceControlV3CasesFrom(t, file)...)
	}
	return cases
}

// loadVoiceControlV3CasesFrom reads the cases of one document.
func loadVoiceControlV3CasesFrom(t *testing.T, file string) []voiceControlCase {
	t.Helper()
	var doc struct {
		Cases []voiceControlCase `json:"cases"`
	}
	if err := json.Unmarshal(readVoiceControlV3Fixture(t, file), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no cases")
	}
	return doc.Cases
}

// lineV3 is the case's line, from whichever of its three forms it has, read
// from the v3 directory.
func (c voiceControlCase) lineV3(t *testing.T) []byte {
	t.Helper()
	if c.LineFile != "" {
		return readVoiceControlV3Fixture(t, c.LineFile)
	}
	return c.line(t)
}

// readVoiceControlV3Case reads a case's line with the v3 reader of its
// direction and returns the kind it was read as and the error.
func readVoiceControlV3Case(t *testing.T, direction string, line []byte) (kind string, skipped bool, err error) {
	t.Helper()
	switch direction {
	case "command":
		var cmd VoiceControlCommandV3
		cmd, err = ParseVoiceControlCommandV3(line)
		return string(cmd.Kind()), false, err
	case "event":
		var ev VoiceControlEventV3
		ev, err = ParseVoiceControlEventV3(line)
		return string(ev.kind()), err == nil && ev.Unknown != "", err
	}
	t.Fatalf("direction %q is neither command nor event", direction)
	return "", false, nil
}

// checkVoiceControlV3Case holds what a v3 reader made of a line to what a
// case wants.
func checkVoiceControlV3Case(t *testing.T, c voiceControlCase, line []byte) {
	t.Helper()
	kind, skipped, err := readVoiceControlV3Case(t, c.Direction, line)
	if skipped && c.Want.Result != "skip" {
		t.Fatalf("skipped as an unknown kind %q, want %s", kind, c.Want.Result)
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
}

// TestVoiceControlV3Cases runs the v3 conformance cases: each line is read by
// the v3 reader of its direction, and accepted, refused with the code (and
// id) given, or skipped.
func TestVoiceControlV3Cases(t *testing.T) {
	names := map[string]bool{}
	for _, c := range loadVoiceControlV3Cases(t) {
		key := c.Direction + ": " + c.Name
		if names[key] {
			t.Errorf("case %q appears twice", key)
		}
		names[key] = true
		if c.Want.OffersV1 != nil {
			t.Errorf("case %q: no hello comes out of the v3 reader", key)
		}
		t.Run(key, func(t *testing.T) {
			checkVoiceControlV3Case(t, c, c.lineV3(t))
		})
	}
}

// TestVoiceControlV3CasesCoverEveryCode makes sure each code a v3 reader can
// return is shown by at least one case.
func TestVoiceControlV3CasesCoverEveryCode(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range loadVoiceControlV3Cases(t) {
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

// TestVoiceControlV3IsV2AndMore pins "a v3 session is v2 plus": every v2
// conformance case that is about a message, written under v3's name (and,
// for a state, with the share's sound off), is read by the v3 readers with
// the result the v2 readers give it. So no rule of v2's was lost or changed
// on the way, in either direction, without a list of rules kept twice.
func TestVoiceControlV3IsV2AndMore(t *testing.T) {
	commands, events := 0, 0
	for _, c := range loadVoiceControlV2Cases(t) {
		// The framing cases (length, UTF-8, a key twice, a lone surrogate)
		// and the cases about other versions are not about a message, and
		// v3 has cases of its own for them.
		if c.Line == nil || (c.Want.Result == "refuse" && c.Want.Code != "invalid" && c.Want.Code != "unknown_kind") {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(*c.Line), &fields) != nil {
			continue
		}
		if schema, _ := fieldString(fields, "schema"); schema != VoiceControlSchemaV2 {
			continue
		}
		fields["schema"] = json.RawMessage(strconv.Quote(VoiceControlSchemaV3))
		if kind, _ := fieldString(fields, "kind"); c.Direction == "event" && kind == string(VoiceControlEventState) {
			fields["share_audio"] = json.RawMessage(`"off"`)
			fields["share_audio_muted"] = json.RawMessage(`false`)
			fields["share_audio_sources"] = json.RawMessage(`0`)
		}
		line, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if c.Direction == "command" {
			commands++
		} else {
			events++
		}
		t.Run(c.Direction+": "+c.Name, func(t *testing.T) {
			checkVoiceControlV3Case(t, c, line)
		})
	}
	if commands < 30 || events < 30 {
		t.Errorf("only %d command and %d event cases of v2 were tried in v3", commands, events)
	}
}

// TestVoiceControlV3ReadersKeepVersionsApart pins the acceptance criterion in
// every direction: a v3 line read as v1 or as v2 is unknown_schema, and a v1
// or a v2 line read as v3 is unknown_schema, for commands and events alike.
// In particular a set_share_audio in a v2 session is unknown_schema, not
// unknown_kind: the line is of another version before it is of any kind.
func TestVoiceControlV3ReadersKeepVersionsApart(t *testing.T) {
	type reader struct {
		version string
		command func([]byte) error
		event   func([]byte) error
	}
	readers := []reader{
		{VoiceControlSchemaV1,
			func(b []byte) error { _, err := ParseVoiceControlCommand(b); return err },
			func(b []byte) error { _, err := ParseVoiceControlEvent(b); return err }},
		{VoiceControlSchemaV2,
			func(b []byte) error { _, err := ParseVoiceControlCommandV2(b); return err },
			func(b []byte) error { _, err := ParseVoiceControlEventV2(b); return err }},
		{VoiceControlSchemaV3,
			func(b []byte) error { _, err := ParseVoiceControlCommandV3(b); return err },
			func(b []byte) error { _, err := ParseVoiceControlEventV3(b); return err }},
	}
	type line struct {
		version string
		name    string
		event   bool
		data    []byte
	}
	lines := []line{
		{VoiceControlSchemaV1, "command-set-mute.json", false, readVoiceControlFixture(t, "command-set-mute.json")},
		{VoiceControlSchemaV1, "event-state-connected.json", true, readVoiceControlFixture(t, "event-state-connected.json")},
		{VoiceControlSchemaV2, "command-set-mute.json", false, readVoiceControlV2Fixture(t, "command-set-mute.json")},
		{VoiceControlSchemaV2, "command-set-devices.json", false, readVoiceControlV2Fixture(t, "command-set-devices.json")},
		{VoiceControlSchemaV2, "event-state-connected.json", true, readVoiceControlV2Fixture(t, "event-state-connected.json")},
	}
	for _, g := range voiceControlV3Golden {
		if !g.v1 {
			lines = append(lines, line{VoiceControlSchemaV3, g.file, g.event, readVoiceControlV3Fixture(t, g.file)})
		}
	}
	for _, l := range lines {
		for _, r := range readers {
			t.Run(l.version+" "+l.name+" read as "+r.version, func(t *testing.T) {
				parse := r.command
				if l.event {
					parse = r.event
				}
				err := parse(compactVoiceControlFixture(t, l.data))
				if l.version == r.version {
					if err != nil {
						t.Errorf("its own reader refuses it: %v", err)
					}
					return
				}
				var vcErr *VoiceControlError
				if !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownSchema || vcErr.ID != 0 {
					t.Errorf("got %v, want unknown_schema with no id", err)
				}
			})
		}
	}
}

// TestVoiceControlV3Hello pins the negotiation: the hello is v1's, it offers
// the three versions and validates, the v1 reader reads it, and the v2 and v3
// readers do not: a hello inside a session of either is not a hello.
func TestVoiceControlV3Hello(t *testing.T) {
	hello := compactVoiceControlFixture(t, readVoiceControlV3Fixture(t, voiceControlV3Hello))
	ev, err := ParseVoiceControlEvent(hello)
	if err != nil || ev.Hello == nil {
		t.Fatalf("read as %+v, %v", ev, err)
	}
	if err := ev.Hello.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	want := []string{VoiceControlSchemaV1, VoiceControlSchemaV2, VoiceControlSchemaV3}
	if !slices.Equal(ev.Hello.Versions, want) {
		t.Errorf("offers %v, want %v", ev.Hello.Versions, want)
	}
	for _, v := range want {
		if !ev.Hello.Offers(v) {
			t.Errorf("does not offer %s", v)
		}
	}
	if ev.Hello.Offers("conch.voice_control.v4") {
		t.Error("offers v4")
	}
	for name, parse := range map[string]func([]byte) error{
		"v2": func(b []byte) error { _, err := ParseVoiceControlEventV2(b); return err },
		"v3": func(b []byte) error { _, err := ParseVoiceControlEventV3(b); return err },
	} {
		var vcErr *VoiceControlError
		if err := parse(hello); !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownSchema {
			t.Errorf("the %s reader reads the hello: %v", name, err)
		}
	}
	// And one written under v3's name is invalid, as one under v2's is.
	inV3 := bytes.Replace(hello, []byte(`"schema":"`+VoiceControlSchemaV1+`"`), []byte(`"schema":"`+VoiceControlSchemaV3+`"`), 1)
	var vcErr *VoiceControlError
	if _, err := ParseVoiceControlEventV3(inV3); !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorInvalid {
		t.Errorf("a hello in v3: %v, want invalid", err)
	}
	// The v2 directory's hello, offering two, is unchanged and still read.
	both, err := ParseVoiceControlEvent(compactVoiceControlFixture(t, readVoiceControlV2Fixture(t, "event-hello-v1-and-v2.json")))
	if err != nil || both.Hello == nil || both.Hello.Offers(VoiceControlSchemaV3) {
		t.Errorf("the v2 hello: %+v, %v", both.Hello, err)
	}
}

// TestVoiceControlCommandKindV3 pins what v3's kinds are and that they are
// v3's alone: v2's seven, word for word and in order, then the two of the
// share's sound; the published vocabulary of v1 and v2 has neither of the
// two, so the v1 and the v2 readers answer them with unknown_kind, as they
// answer any kind they do not have.
func TestVoiceControlCommandKindV3(t *testing.T) {
	v2 := Vocabularies["VoiceControlCommandKind"]
	v3 := Vocabularies["VoiceControlCommandKindV3"]
	added := []string{string(VoiceControlCommandV3SetShareAudio), string(VoiceControlCommandV3SetShareAudioMute)}
	if !slices.Equal(v3, append(slices.Clone(v2), added...)) {
		t.Errorf("v3's kinds are %q,\nwant v2's %q and then %q", v3, v2, added)
	}
	for _, kind := range v2 {
		if !VoiceControlCommandKind(kind).KnownIn(VoiceControlSchemaV2) {
			t.Errorf("%s is a kind of v3 and not of v2: v3 is v2 and two more", kind)
		}
	}
	for _, kind := range added {
		k := VoiceControlCommandKind(kind)
		if k.Valid() || k.KnownIn(VoiceControlSchemaV1) || k.KnownIn(VoiceControlSchemaV2) || k.KnownIn(VoiceControlSchemaV3) {
			t.Errorf("%s is in the vocabulary of the published versions", kind)
		}
		for version, parse := range map[string]func([]byte) error{
			VoiceControlSchemaV1: func(b []byte) error { _, err := ParseVoiceControlCommand(b); return err },
			VoiceControlSchemaV2: func(b []byte) error { _, err := ParseVoiceControlCommandV2(b); return err },
		} {
			line := []byte(`{"schema":"` + version + `","kind":"` + kind + `","id":7,"capturing":false,"muted":true}`)
			var vcErr *VoiceControlError
			if err := parse(line); !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownKind || vcErr.ID != 7 {
				t.Errorf("the %s reader answers %s with %v, want unknown_kind for id 7", version, kind, err)
			}
		}
	}
	for _, k := range []VoiceControlCommandKindV3{"set_volume", "", "SET_MUTE", "press"} {
		if k.Valid() {
			t.Errorf("%q is a kind of v3", k)
		}
	}
	line := []byte(`{"schema":"` + VoiceControlSchemaV3 + `","kind":"set_volume","id":7,"level":3}`)
	var vcErr *VoiceControlError
	if _, err := ParseVoiceControlCommandV3(line); !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownKind || vcErr.ID != 7 {
		t.Errorf("%s: %v, want unknown_kind for id 7", line, err)
	}
}

// voiceControlV3Command is the view of an owner's line the v3 transcript
// checker needs, whichever version the session is in: the v1 and the v2
// owner's transcripts live in this directory too, because the helper that
// offers the three is one helper.
type voiceControlV3Command struct {
	kind           string
	id             int64
	setMute        *bool
	setDeafen      *bool
	setDevices     bool
	switchesInput  bool
	setTalkMode    *VoiceControlTalkMode
	shareAudio     *bool
	shareAudioMute *bool
}

// desired reports whether the command sets a desired state, which a helper
// that has begun to leave refuses with stopping.
func (c voiceControlV3Command) desired() bool {
	return c.setMute != nil || c.setDeafen != nil || c.setDevices || c.setTalkMode != nil ||
		c.shareAudio != nil || c.shareAudioMute != nil
}

// voiceControlV3State is the view of a state the checker needs. A state of
// v1 or v2 has no share's sound: it reads as off.
type voiceControlV3State struct {
	voiceControlAnyState
	shareAudio      VoiceControlShareAudio
	shareAudioMuted bool
}

type voiceControlV3Event struct {
	seq           int64
	hello         *VoiceControlHelloV1
	ack           *VoiceControlAckV2
	state         *voiceControlV3State
	protocolError VoiceControlErrorCode
	unknown       bool
}

// parseVoiceControlV3AnyCommand reads an owner's line in the version given,
// or, before the version is fixed, in whichever of the offered versions the
// line names, as a helper offering the three does.
func parseVoiceControlV3AnyCommand(line []byte, version string, offered []string) (voiceControlV3Command, error) {
	versions := []string{version}
	if version == "" {
		versions = offered
	}
	var last error
	for _, v := range versions {
		var cmd voiceControlV3Command
		var err error
		switch v {
		case VoiceControlSchemaV1, VoiceControlSchemaV2:
			var c voiceControlAnyCommand
			c, err = parseVoiceControlAnyCommand(line, v, nil)
			cmd = voiceControlV3Command{
				kind: string(c.kind), id: c.id, setMute: c.setMute, setDeafen: c.setDeafen,
				setDevices: c.setDevices != nil, switchesInput: c.setDevices != nil && c.setDevices.Input != "",
				setTalkMode: c.setTalkMode,
			}
		case VoiceControlSchemaV3:
			var c VoiceControlCommandV3
			c, err = ParseVoiceControlCommandV3(line)
			cmd = voiceControlV3Command{
				kind: string(c.Kind()), id: c.ID(),
				setDevices: c.SetDevices != nil, switchesInput: c.SetDevices != nil && c.SetDevices.Input != "",
			}
			if c.SetMute != nil {
				cmd.setMute = &c.SetMute.Muted
			}
			if c.SetDeafen != nil {
				cmd.setDeafen = &c.SetDeafen.Deafened
			}
			if c.SetTalkMode != nil {
				cmd.setTalkMode = &c.SetTalkMode.TalkMode
			}
			if c.SetShareAudio != nil {
				cmd.shareAudio = &c.SetShareAudio.Capturing
			}
			if c.SetShareAudioMute != nil {
				cmd.shareAudioMute = &c.SetShareAudioMute.Muted
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
	if last == nil {
		last = voiceControlErr(VoiceControlErrorUnknownSchema, 0, "no version to read the line in")
	}
	return voiceControlV3Command{}, last
}

// parseVoiceControlV3AnyEvent reads a helper's line: the hello with the v1
// reader, everything after with the session's version.
func parseVoiceControlV3AnyEvent(line []byte, version string) (voiceControlV3Event, error) {
	if version != VoiceControlSchemaV3 {
		ev, err := parseVoiceControlAnyEvent(line, version)
		if err != nil {
			return voiceControlV3Event{}, err
		}
		out := voiceControlV3Event{seq: ev.seq, hello: ev.hello, ack: ev.ack, protocolError: ev.protocolError, unknown: ev.unknown}
		if ev.state != nil {
			out.state = &voiceControlV3State{voiceControlAnyState: *ev.state, shareAudio: VoiceControlShareAudioOff}
		}
		return out, nil
	}
	ev, err := ParseVoiceControlEventV3(line)
	if err != nil {
		return voiceControlV3Event{}, err
	}
	out := voiceControlV3Event{seq: ev.Seq, unknown: ev.Unknown != ""}
	if ev.Ack != nil {
		out.ack = &VoiceControlAckV2{ID: ev.Ack.ID, OK: ev.Ack.OK, Error: ev.Ack.Error}
	}
	if ev.State != nil {
		out.state = &voiceControlV3State{
			voiceControlAnyState: voiceControlAnyState{
				phase: ev.State.Phase, reason: ev.State.Reason, transmitting: ev.State.Transmitting,
				muted: ev.State.Muted, deafened: ev.State.Deafened, talkMode: ev.State.TalkMode,
			},
			shareAudio: ev.State.ShareAudio, shareAudioMuted: ev.State.ShareAudioMuted,
		}
	}
	if ev.ProtocolError != nil {
		out.protocolError = ev.ProtocolError.Code
	}
	return out, nil
}

// voiceControlV3Transcripts is every transcript the issue asks for, and the
// version its session is in.
var voiceControlV3Transcripts = map[string]string{
	"transcript-negotiation-v3-owner.json":          VoiceControlSchemaV3,
	"transcript-negotiation-v2-owner.json":          VoiceControlSchemaV2,
	"transcript-negotiation-v1-owner.json":          VoiceControlSchemaV1,
	"transcript-share-audio.json":                   VoiceControlSchemaV3,
	"transcript-share-audio-publish-refused.json":   VoiceControlSchemaV3,
	"transcript-share-audio-application-scope.json": VoiceControlSchemaV3,
	"transcript-refused-commands.json":              VoiceControlSchemaV3,
	"transcript-version-fixed.json":                 VoiceControlSchemaV3,
	"transcript-version-fixed-v2-session.json":      VoiceControlSchemaV2,
}

// TestVoiceControlV3Transcripts checks every v3 transcript against the
// ordering rules of the control protocol (docs/design/tui-voice.md §5), as
// the v1 and v2 tests do, and against v3's own: the hello is v1 and offers
// the three versions, the owner's start fixes the version, every later line
// is read in it and a line of another version is unknown_schema; the share's
// sound is never on or unavailable unless the owner asked for it; and what a
// share-audio command changed is in the state before its ack.
func TestVoiceControlV3Transcripts(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", voiceControlV3Dir, "transcript-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, file := range files {
		name := filepath.Base(file)
		found[name] = true
		t.Run(name, func(t *testing.T) {
			var doc struct {
				Description string             `json:"description"`
				Steps       []voiceControlStep `json:"steps"`
			}
			if err := json.Unmarshal(readVoiceControlV3Fixture(t, name), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Description == "" {
				t.Error("no description")
			}
			version, ok := voiceControlV3Transcripts[name]
			if !ok {
				t.Fatal("not in voiceControlV3Transcripts: say which version its session is in")
			}
			if got := checkVoiceControlV3Transcript(t, doc.Steps); got != version {
				t.Errorf("the session is in %q, want %q", got, version)
			}
		})
	}
	for name := range voiceControlV3Transcripts {
		if !found[name] {
			t.Errorf("testdata/%s/%s is missing", voiceControlV3Dir, name)
		}
	}
}

// pendingCommandV3 is a command with a usable id that has not been answered,
// the answer it must get when that is already known, and what the latest
// state said when it was read.
type pendingCommandV3 struct {
	id              int64
	cmd             voiceControlV3Command
	refusal         VoiceControlErrorCode
	wasTransmitting bool
}

// checkVoiceControlV3Transcript checks the steps and returns the version the
// owner's start fixed.
func checkVoiceControlV3Transcript(t voiceControlReporter, steps []voiceControlStep) string {
	t.Helper()
	var (
		seq, lastID       int64
		version           string
		hello             *VoiceControlHelloV1
		started, final    bool
		finalPhase        VoiceControlPhase
		state             *voiceControlV3State
		previousWasState  bool
		pending           []pendingCommandV3
		wantProtocolError VoiceControlErrorCode
		wantFailedAfterPE bool
		shareAsked        bool
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
			return version
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
				return version
			}
			line := compactVoiceControlFixture(t, step.Message)
			cmd, err := parseVoiceControlV3AnyCommand(line, version, hello.Versions)
			var vcErr *VoiceControlError
			if err != nil && !errors.As(err, &vcErr) {
				t.Fatalf("step %d: %v", i, err)
			}
			id := cmd.id
			if vcErr != nil {
				id = vcErr.ID
			}
			switch {
			case !started && cmd.kind != string(VoiceControlCommandStart):
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
				p := pendingCommandV3{id: id, cmd: cmd, wasTransmitting: state != nil && state.transmitting}
				switch {
				case vcErr != nil:
					p.refusal = vcErr.Code
				case cmd.kind == string(VoiceControlCommandStart) && started:
					p.refusal = VoiceControlErrorAlreadyStarted
				case cmd.kind == string(VoiceControlCommandStart):
					started = true
					// The start's schema fixes the version: the line was
					// accepted by exactly one reader.
					var s struct {
						Schema string `json:"schema"`
					}
					if err := json.Unmarshal(line, &s); err != nil {
						t.Fatal(err)
					}
					version = s.Schema
				case cmd.desired():
					if state != nil && state.phase == VoiceControlPhaseStopping {
						p.refusal = VoiceControlErrorStopping
					}
				}
				if p.refusal == "" && cmd.shareAudio != nil {
					shareAsked = *cmd.shareAudio
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
		ev, err := parseVoiceControlV3AnyEvent(compactVoiceControlFixture(t, step.Message), version)
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
			for _, v := range []string{VoiceControlSchemaV1, VoiceControlSchemaV2, VoiceControlSchemaV3} {
				if !ev.hello.Offers(v) {
					t.Fatalf("step %d: a v3 helper offers the three versions, this one offers %v", i, ev.hello.Versions)
				}
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
			if state.shareAudio != VoiceControlShareAudioOff && !shareAsked {
				t.Fatalf("step %d: share_audio is %s and the owner has not asked for the share's sound", i, state.shareAudio)
			}
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
			checkVoiceControlV3Effect(t, i, p, state, wasState)
		case ev.unknown:
		}
	}
	t.Fatal("the transcript does not end with the helper's exit")
	return ""
}

// checkVoiceControlV3Effect checks that the state a command changed was
// written before its ack: v2's rules, and the share's sound. A
// set_share_audio that asks for the sound is acknowledged with the outcome
// already in the state, on or unavailable; one that stops it, with off; and a
// set_share_audio_mute with the mute it asked for.
func checkVoiceControlV3Effect(t voiceControlReporter, step int, p pendingCommandV3, state *voiceControlV3State, previousWasState bool) {
	t.Helper()
	cmd := p.cmd
	switch {
	case state == nil:
		t.Errorf("step %d: %s acknowledged before any state", step, cmd.kind)
	case cmd.setMute != nil && state.muted != *cmd.setMute:
		t.Errorf("step %d: set_mute %v acknowledged while the latest state has muted %v", step, *cmd.setMute, state.muted)
	case cmd.setDeafen != nil && state.deafened != *cmd.setDeafen:
		t.Errorf("step %d: set_deafen %v acknowledged while the latest state has deafened %v", step, *cmd.setDeafen, state.deafened)
	case cmd.kind == string(VoiceControlCommandLeave) && state.phase != VoiceControlPhaseStopping && !state.phase.Final():
		t.Errorf("step %d: leave acknowledged in phase %s", step, state.phase)
	case cmd.kind == string(VoiceControlCommandGetState) && !previousWasState:
		t.Errorf("step %d: get_state acknowledged without a state just before", step)
	case cmd.switchesInput && p.wasTransmitting && state.transmitting:
		t.Errorf("step %d: the microphone was switched during a transmission and the latest state still transmits", step)
	case cmd.setTalkMode != nil && state.talkMode != *cmd.setTalkMode:
		t.Errorf("step %d: set_talk_mode %s acknowledged while the latest state has %s", step, *cmd.setTalkMode, state.talkMode)
	case cmd.setTalkMode != nil && p.wasTransmitting && state.transmitting:
		t.Errorf("step %d: the talk mode was switched during a transmission and the latest state still transmits", step)
	case cmd.shareAudio != nil && *cmd.shareAudio && state.shareAudio == VoiceControlShareAudioOff:
		t.Errorf("step %d: set_share_audio acknowledged while the latest state has share_audio off: the outcome, on or unavailable, comes before the ack", step)
	case cmd.shareAudio != nil && !*cmd.shareAudio && state.shareAudio != VoiceControlShareAudioOff:
		t.Errorf("step %d: the share's sound was stopped and the latest state has share_audio %s", step, state.shareAudio)
	case cmd.shareAudioMute != nil && state.shareAudioMuted != *cmd.shareAudioMute:
		t.Errorf("step %d: set_share_audio_mute %v acknowledged while the latest state has share_audio_muted %v", step, *cmd.shareAudioMute, state.shareAudioMuted)
	}
}

// TestVoiceControlV3TranscriptRules makes sure the checker above refuses
// what it is there to refuse, and says why, on the share-audio transcript
// with one step taken out each time.
func TestVoiceControlV3TranscriptRules(t *testing.T) {
	var doc struct {
		Steps []voiceControlStep `json:"steps"`
	}
	if err := json.Unmarshal(readVoiceControlV3Fixture(t, "transcript-share-audio.json"), &doc); err != nil {
		t.Fatal(err)
	}
	// The steps taken out: the command that asks for the share's sound, the
	// state that answers it, and the state that answers the stop.
	ask, stop := -1, -1
	for i, step := range doc.Steps {
		if step.From != "owner" || step.Message == nil {
			continue
		}
		cmd, err := ParseVoiceControlCommandV3(compactVoiceControlFixture(t, step.Message))
		if err != nil || cmd.SetShareAudio == nil {
			continue
		}
		if cmd.SetShareAudio.Capturing && ask < 0 {
			ask = i
		}
		if !cmd.SetShareAudio.Capturing && stop < 0 {
			stop = i
		}
	}
	if ask < 0 || stop < ask {
		t.Fatalf("the transcript asks for the share's sound at step %d and stops it at step %d", ask, stop)
	}
	without := func(i int) []voiceControlStep {
		return slices.Delete(slices.Clone(doc.Steps), i, i+1)
	}
	tests := []struct {
		name  string
		steps []voiceControlStep
		want  string
	}{
		{"the transcript as it is", doc.Steps, ""},
		{"the share's sound on without having been asked for", without(ask), "the owner has not asked for the share's sound"},
		{"asked for, and acknowledged with the state still off", without(ask + 1), "the outcome, on or unavailable, comes before the ack"},
		{"still on after the stop was acknowledged", without(stop + 1), "the share's sound was stopped and the latest state has share_audio on"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Events after a step taken out keep their seq, which only ever
			// leaves a gap, so the missing step is the only fault.
			failures := voiceControlV3TranscriptFailures(tt.steps)
			switch {
			case tt.want == "" && len(failures) != 0:
				t.Errorf("the checker refuses it: %q", failures)
			case tt.want != "" && !slices.ContainsFunc(failures, func(f string) bool { return strings.Contains(f, tt.want) }):
				t.Errorf("the checker says %q, want a failure containing %q", failures, tt.want)
			}
		})
	}
}

// failureRecorder is a voiceControlReporter that keeps what a check reports
// instead of failing a test. A fatal failure ends the check by a panic that
// voiceControlV3TranscriptFailures takes back.
type failureRecorder struct {
	failures []string
}

func (*failureRecorder) Helper() {}

func (r *failureRecorder) Error(args ...any) {
	r.failures = append(r.failures, fmt.Sprint(args...))
}

func (r *failureRecorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *failureRecorder) Fatal(args ...any) {
	r.Error(args...)
	panic(r)
}

func (r *failureRecorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	panic(r)
}

// voiceControlV3TranscriptFailures runs the transcript checker on steps and
// returns what it reported.
func voiceControlV3TranscriptFailures(steps []voiceControlStep) (failures []string) {
	recorder := &failureRecorder{}
	defer func() {
		if p := recover(); p != nil && p != any(recorder) {
			panic(p)
		}
		failures = recorder.failures
	}()
	checkVoiceControlV3Transcript(recorder, steps)
	return recorder.failures
}

// TestVoiceControlV3FixturesAreAllClaimed fails on a file in the v3 fixture
// directory that no test reads.
func TestVoiceControlV3FixturesAreAllClaimed(t *testing.T) {
	claimed := map[string]bool{"sensitive.json": true}
	for _, file := range voiceControlV3CaseFiles {
		claimed[file] = true
	}
	for _, g := range voiceControlV3Golden {
		claimed[g.file] = true
	}
	for _, c := range loadVoiceControlV3Cases(t) {
		if c.LineFile != "" {
			claimed[c.LineFile] = true
		}
	}
	for name := range voiceControlV3Transcripts {
		claimed[name] = true
	}
	for _, f := range fixtureFilesUnder(t, voiceControlV3Dir) {
		if !claimed[f] {
			t.Errorf("testdata/%s/%s is read by no test", voiceControlV3Dir, f)
		}
	}
}

// voiceControlV3Types is every message type of v3.
var voiceControlV3Types = []VoiceControlMessage{
	VoiceControlStartV3{}, VoiceControlSetMuteV3{}, VoiceControlSetDeafenV3{}, VoiceControlGetStateV3{}, VoiceControlLeaveV3{},
	VoiceControlSetDevicesV3{}, VoiceControlSetTalkModeV3{},
	VoiceControlSetShareAudioV3{}, VoiceControlSetShareAudioMuteV3{},
	VoiceControlAckV3{}, VoiceControlStateV3{}, VoiceControlProtocolErrorV3{},
}

// TestVoiceControlV3CarriesNoSecrets holds v3 to its sensitive.json as the v1
// and v2 tests do: no field of any message is named like a credential or a
// room; no event field can carry free text (the application name the owner
// sends is never in an event, and the state counts the applications captured
// without naming one); and no fixture holds a fake secret, except the one
// case that shows an unknown field named token being ignored.
func TestVoiceControlV3CarriesNoSecrets(t *testing.T) {
	var sensitive struct {
		FakeSecrets []string `json:"fake_secrets"`
		Fragments   []string `json:"forbidden_field_fragments"`
	}
	if err := json.Unmarshal(readVoiceControlV3Fixture(t, "sensitive.json"), &sensitive); err != nil {
		t.Fatal(err)
	}
	if len(sensitive.FakeSecrets) == 0 || len(sensitive.Fragments) == 0 {
		t.Fatal("sensitive.json lists nothing")
	}
	// v3 forbids what v2 forbids: the lists are the same.
	var v2 struct {
		FakeSecrets []string `json:"fake_secrets"`
		Fragments   []string `json:"forbidden_field_fragments"`
	}
	if err := json.Unmarshal(readVoiceControlV2Fixture(t, "sensitive.json"), &v2); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sensitive.FakeSecrets, v2.FakeSecrets) || !slices.Equal(sensitive.Fragments, v2.Fragments) {
		t.Error("sensitive.json does not list what voice-control-v2/sensitive.json lists")
	}

	for _, msg := range voiceControlV3Types {
		typ := reflect.TypeOf(msg)
		event := strings.HasPrefix(typ.Name(), "VoiceControlAck") ||
			strings.HasPrefix(typ.Name(), "VoiceControlState") || strings.HasPrefix(typ.Name(), "VoiceControlProtocolError")
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			for _, frag := range sensitive.Fragments {
				if fieldNamedLike(f, frag) {
					t.Errorf("%s.%s is named like a secret (%q)", typ.Name(), f.Name, frag)
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
	for _, name := range fixtureFilesUnder(t, voiceControlV3Dir) {
		if name == "sensitive.json" {
			continue
		}
		data := readVoiceControlV3Fixture(t, name)
		if slices.Contains(voiceControlV3CaseFiles, name) {
			for _, c := range loadVoiceControlV3CasesFrom(t, name) {
				if c.Name == exempt {
					continue
				}
				line := c.lineV3(t)
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

// TestVoiceControlV3AndThePipe holds v3 to the attach line's rules
// (voice_control_pipe.go), as v1 and v2 are held: the attach line is not an
// event of v3, no field of a v3 message is named like the nonce or the
// attach, and no v3 fixture holds a fake nonce.
func TestVoiceControlV3AndThePipe(t *testing.T) {
	attached := compactVoiceControlFixture(t, readVoiceControlPipeFixture(t, "attached.json"))
	var vcErr *VoiceControlError
	if _, err := ParseVoiceControlEventV3(attached); !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownSchema {
		t.Errorf("the v3 event reader does not refuse the attach line as unknown_schema: %v", err)
	}
	sensitive := loadVoiceControlPipeSensitive(t)
	for _, msg := range voiceControlV3Types {
		typ := reflect.TypeOf(msg)
		for i := 0; i < typ.NumField(); i++ {
			for _, frag := range sensitive.NeverInEvents {
				if fieldNamedLike(typ.Field(i), frag) {
					t.Errorf("%s.%s is named like the attach (%q); the nonce is never echoed", typ.Name(), typ.Field(i).Name, frag)
				}
			}
		}
	}
	for _, name := range fixtureFilesUnder(t, voiceControlV3Dir) {
		data := readVoiceControlV3Fixture(t, name)
		for _, nonce := range sensitive.FakeNonces {
			if bytes.Contains(data, []byte(nonce)) {
				t.Errorf("%s/%s holds a fake nonce: the nonce is never on the control channel", voiceControlV3Dir, name)
			}
		}
	}
}

// The fields of each v3 message are an exact list, as v1's and v2's are. The
// one new owner-to-helper text field, application_name, was reviewed as
// printable ASCII that the helper never repeats; the state gained three
// vocabulary words and a number, and no text.
func TestVoiceControlV3FieldsAreAnExactList(t *testing.T) {
	head := []string{"schema", "kind"}
	want := map[string][]string{
		"VoiceControlStartV3":             append(slices.Clone(head), "id", "server", "channel", "principal_id", "credential", "muted", "deafened"),
		"VoiceControlSetMuteV3":           append(slices.Clone(head), "id", "muted"),
		"VoiceControlSetDeafenV3":         append(slices.Clone(head), "id", "deafened"),
		"VoiceControlGetStateV3":          append(slices.Clone(head), "id"),
		"VoiceControlLeaveV3":             append(slices.Clone(head), "id"),
		"VoiceControlSetDevicesV3":        append(slices.Clone(head), "id", "input", "output"),
		"VoiceControlSetTalkModeV3":       append(slices.Clone(head), "id", "talk_mode"),
		"VoiceControlSetShareAudioV3":     append(slices.Clone(head), "id", "capturing", "scope", "application_name"),
		"VoiceControlSetShareAudioMuteV3": append(slices.Clone(head), "id", "muted"),
		"VoiceControlAckV3":               append(slices.Clone(head), "seq", "id", "ok", "error"),
		"VoiceControlStateV3": append(slices.Clone(head), "seq", "phase", "reason", "retry_in_ms", "capture", "playback", "transmitting", "muted", "deafened", "blocked", "talk_mode", "ptt", "ptt_problem",
			"share_audio", "share_audio_muted", "share_audio_problem", "share_audio_sources"),
		"VoiceControlProtocolErrorV3": append(slices.Clone(head), "seq", "code"),
	}
	for _, msg := range voiceControlV3Types {
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

// TestVoiceControlV3MessagesAreV2sAndMore holds each v3 type to the v2 type
// it carries on: the same fields, in the same order, with the same tags and
// the same types, the kind of a command aside (it is v3's vocabulary), and,
// for the state, four more at the end. So a v2 field cannot be renamed,
// retyped, made optional or dropped in v3 by accident.
func TestVoiceControlV3MessagesAreV2sAndMore(t *testing.T) {
	pairs := []struct {
		v2, v3 any
		more   int
	}{
		{VoiceControlStartV2{}, VoiceControlStartV3{}, 0},
		{VoiceControlSetMuteV2{}, VoiceControlSetMuteV3{}, 0},
		{VoiceControlSetDeafenV2{}, VoiceControlSetDeafenV3{}, 0},
		{VoiceControlGetStateV2{}, VoiceControlGetStateV3{}, 0},
		{VoiceControlLeaveV2{}, VoiceControlLeaveV3{}, 0},
		{VoiceControlSetDevicesV2{}, VoiceControlSetDevicesV3{}, 0},
		{VoiceControlSetTalkModeV2{}, VoiceControlSetTalkModeV3{}, 0},
		{VoiceControlAckV2{}, VoiceControlAckV3{}, 0},
		{VoiceControlStateV2{}, VoiceControlStateV3{}, 4},
		{VoiceControlProtocolErrorV2{}, VoiceControlProtocolErrorV3{}, 0},
	}
	commandKinds := [2]reflect.Type{reflect.TypeFor[VoiceControlCommandKind](), reflect.TypeFor[VoiceControlCommandKindV3]()}
	for _, p := range pairs {
		v2, v3 := reflect.TypeOf(p.v2), reflect.TypeOf(p.v3)
		if v3.NumField() != v2.NumField()+p.more {
			t.Errorf("%s has %d fields, %s has %d: want %d more", v3.Name(), v3.NumField(), v2.Name(), v2.NumField(), p.more)
			continue
		}
		for i := 0; i < v2.NumField(); i++ {
			a, b := v2.Field(i), v3.Field(i)
			sameType := a.Type == b.Type || (a.Name == "Kind" && a.Type == commandKinds[0] && b.Type == commandKinds[1])
			if a.Name != b.Name || a.Tag != b.Tag || !sameType {
				t.Errorf("%s.%s is %v `%s`, %s.%s is %v `%s`", v2.Name(), a.Name, a.Type, a.Tag, v3.Name(), b.Name, b.Type, b.Tag)
			}
		}
	}

	// And the state's v2 fields reach v2's rules, every one of them.
	retry := int64(7)
	s := VoiceControlStateV3{
		Schema: VoiceControlSchemaV3, Kind: "k", Seq: 9, Phase: "p", Reason: "r", RetryInMS: &retry,
		Capture: "c", Playback: "pl", Transmitting: true, Muted: true, Deafened: true,
		Blocked: []VoiceControlBlock{"b"}, TalkMode: "t", PTT: "ptt", PTTProblem: "pp",
		ShareAudio: "sa", ShareAudioMuted: true, ShareAudioProblem: "sp", ShareAudioSources: 3,
	}
	got := reflect.ValueOf(s.v2())
	for i := 0; i < got.NumField(); i++ {
		name := got.Type().Field(i).Name
		if got.Field(i).IsZero() {
			t.Errorf("v2() leaves %s out", name)
		}
		if name == "Schema" {
			continue
		}
		if want := reflect.ValueOf(s).FieldByName(name); !reflect.DeepEqual(got.Field(i).Interface(), want.Interface()) {
			t.Errorf("v2() has %s %v, the state has %v", name, got.Field(i).Interface(), want.Interface())
		}
	}
	if s.v2().Schema != VoiceControlSchemaV2 {
		t.Errorf("v2() has schema %q", s.v2().Schema)
	}
}

// validVoiceControlStateV3 is a connected, push-to-talk state with nothing
// blocking and no share's sound asked for, for the Validate tables to vary.
func validVoiceControlStateV3() VoiceControlStateV3 {
	return VoiceControlStateV3{
		Schema: VoiceControlSchemaV3, Kind: VoiceControlEventState, Seq: 5,
		Phase: VoiceControlPhaseConnected, Capture: VoiceControlCaptureOn, Playback: VoiceControlPlaybackOn,
		Blocked: []VoiceControlBlock{}, TalkMode: VoiceControlTalkModePushToTalk, PTT: VoiceControlPTTReady,
		ShareAudio: VoiceControlShareAudioOff,
	}
}

// inVoiceControlPhaseV3 moves a valid connected state into phase, with what
// the phase needs of v2's fields.
func inVoiceControlPhaseV3(s *VoiceControlStateV3, phase VoiceControlPhase) {
	s.Phase = phase
	switch phase {
	case VoiceControlPhaseStarting:
		s.Capture, s.Playback, s.PTT = VoiceControlCaptureOff, VoiceControlPlaybackOff, VoiceControlPTTUnknown
	case VoiceControlPhaseConnecting:
		s.Capture = VoiceControlCaptureOff
	case VoiceControlPhaseReconnecting:
	case VoiceControlPhaseStopping:
		s.Reason = VoiceControlReasonLeft
	case VoiceControlPhaseStopped:
		s.Reason, s.Capture, s.Playback = VoiceControlReasonLeft, VoiceControlCaptureOff, VoiceControlPlaybackOff
	case VoiceControlPhaseFailed:
		s.Reason, s.Capture, s.Playback = VoiceControlReasonInternalError, VoiceControlCaptureOff, VoiceControlPlaybackOff
	}
	if phase != VoiceControlPhaseConnected {
		s.Blocked = []VoiceControlBlock{VoiceControlBlockNotConnected}
	}
}

// TestVoiceControlStateV3Validate covers the rules v3 adds, one by one and in
// every phase; the rules it has from v2 are covered by
// TestVoiceControlV3IsV2AndMore and the conformance cases.
func TestVoiceControlStateV3Validate(t *testing.T) {
	on := func(sources int64) func(*VoiceControlStateV3) {
		return func(s *VoiceControlStateV3) {
			s.ShareAudio, s.ShareAudioSources = VoiceControlShareAudioOn, sources
			if sources == 0 {
				s.ShareAudioProblem = VoiceControlShareAudioProblemNoSources
			}
		}
	}
	unavailable := func(problem VoiceControlShareAudioProblem) func(*VoiceControlStateV3) {
		return func(s *VoiceControlStateV3) {
			s.ShareAudio, s.ShareAudioProblem = VoiceControlShareAudioUnavailable, problem
		}
	}
	tests := []struct {
		name    string
		mutate  func(*VoiceControlStateV3)
		wantErr string
	}{
		{name: "off", mutate: func(*VoiceControlStateV3) {}},
		{name: "on with sources", mutate: on(3)},
		{name: "on with the most sources", mutate: on(VoiceControlMaxShareAudioSources)},
		{name: "on with nothing playing", mutate: on(0)},
		{name: "on and muted", mutate: func(s *VoiceControlStateV3) { on(2)(s); s.ShareAudioMuted = true }},
		{name: "off and muted", mutate: func(s *VoiceControlStateV3) { s.ShareAudioMuted = true }},
		{name: "unavailable and muted", mutate: func(s *VoiceControlStateV3) {
			unavailable(VoiceControlShareAudioProblemNoPipeWire)(s)
			s.ShareAudioMuted = true
		}},
		{name: "unavailable, no_pipewire", mutate: unavailable(VoiceControlShareAudioProblemNoPipeWire)},
		{name: "unavailable, no_process_tree", mutate: unavailable(VoiceControlShareAudioProblemNoProcessTree)},
		{name: "unavailable, publish_refused", mutate: unavailable(VoiceControlShareAudioProblemPublishRefused)},
		{name: "unavailable, unsupported_scope", mutate: unavailable(VoiceControlShareAudioProblemUnsupportedScope)},
		{name: "unavailable, unsupported", mutate: unavailable(VoiceControlShareAudioProblemUnsupported)},
		{name: "unavailable, not_connected", mutate: unavailable(VoiceControlShareAudioProblemNotConnected)},

		// transmitting is unaffected by all four, in both directions.
		{name: "transmitting with the share's sound on", mutate: func(s *VoiceControlStateV3) { on(2)(s); s.Transmitting = true }},
		{name: "transmitting with the share's sound unavailable", mutate: func(s *VoiceControlStateV3) {
			unavailable(VoiceControlShareAudioProblemPublishRefused)(s)
			s.Transmitting = true
		}},
		{name: "transmitting with the share's sound muted", mutate: func(s *VoiceControlStateV3) {
			on(2)(s)
			s.Transmitting, s.ShareAudioMuted = true, true
		}},
		{name: "the share's sound on while the microphone is muted", mutate: func(s *VoiceControlStateV3) {
			on(2)(s)
			s.Muted, s.Blocked = true, []VoiceControlBlock{VoiceControlBlockMuted}
		}},
		{name: "the share's sound on with no microphone", mutate: func(s *VoiceControlStateV3) {
			on(2)(s)
			s.Capture, s.Blocked = VoiceControlCaptureUnavailable, []VoiceControlBlock{VoiceControlBlockNoMicrophone}
		}},
		{name: "the share's sound on with no speakers", mutate: func(s *VoiceControlStateV3) { on(2)(s); s.Playback = VoiceControlPlaybackUnavailable }},

		{name: "no share_audio", mutate: func(s *VoiceControlStateV3) { s.ShareAudio = "" }, wantErr: "share_audio \"\" is not one of"},
		{name: "a share_audio of no version", mutate: func(s *VoiceControlStateV3) { s.ShareAudio = "muted" }, wantErr: "is not one of off, on, unavailable"},
		{name: "a share_audio_problem of no version", mutate: unavailable("no_consent"), wantErr: "share_audio_problem \"no_consent\" is not recognized"},
		{name: "sources below zero", mutate: func(s *VoiceControlStateV3) { on(1)(s); s.ShareAudioSources = -1 }, wantErr: "must be 0 to 1024"},
		{name: "one source too many", mutate: on(VoiceControlMaxShareAudioSources + 1), wantErr: "must be 0 to 1024"},

		{name: "off with a problem", mutate: func(s *VoiceControlStateV3) { s.ShareAudioProblem = VoiceControlShareAudioProblemNotConnected },
			wantErr: "off must have no share_audio_problem"},
		{name: "off with no_sources", mutate: func(s *VoiceControlStateV3) { s.ShareAudioProblem = VoiceControlShareAudioProblemNoSources },
			wantErr: "off must have no share_audio_problem"},
		{name: "off with sources", mutate: func(s *VoiceControlStateV3) { s.ShareAudioSources = 2 }, wantErr: "must be 0 unless share_audio is on"},
		{name: "unavailable with no problem", mutate: unavailable(""), wantErr: "needs a share_audio_problem other than no_sources"},
		{name: "unavailable with no_sources", mutate: unavailable(VoiceControlShareAudioProblemNoSources),
			wantErr: "needs a share_audio_problem other than no_sources"},
		{name: "unavailable with sources", mutate: func(s *VoiceControlStateV3) {
			unavailable(VoiceControlShareAudioProblemNoPipeWire)(s)
			s.ShareAudioSources = 2
		}, wantErr: "must be 0 unless share_audio is on"},
		{name: "on with a problem that is not no_sources", mutate: func(s *VoiceControlStateV3) {
			on(2)(s)
			s.ShareAudioProblem = VoiceControlShareAudioProblemPublishRefused
		}, wantErr: "can have no share_audio_problem but no_sources"},
		{name: "on with no_sources and sources", mutate: func(s *VoiceControlStateV3) {
			on(2)(s)
			s.ShareAudioProblem = VoiceControlShareAudioProblemNoSources
		}, wantErr: "must agree with share_audio_sources being 0"},
		{name: "on with no sources and no no_sources", mutate: func(s *VoiceControlStateV3) { s.ShareAudio = VoiceControlShareAudioOn },
			wantErr: "must agree with share_audio_sources being 0"},

		{name: "a v2 schema", mutate: func(s *VoiceControlStateV3) { s.Schema = VoiceControlSchemaV2 }, wantErr: "schema must be"},
		{name: "a rule of v2's: playback off while connected", mutate: func(s *VoiceControlStateV3) { on(2)(s); s.Playback = VoiceControlPlaybackOff },
			wantErr: "playback on or unavailable"},
		{name: "a rule of v2's: transmitting without ptt ready", mutate: func(s *VoiceControlStateV3) {
			on(2)(s)
			s.Transmitting, s.PTT = true, VoiceControlPTTNotConfigured
		}, wantErr: "without ptt ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validVoiceControlStateV3()
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

	// By phase: off everywhere; on and unavailable everywhere but in starting
	// and the final phases; the mute, a desired state, everywhere.
	for _, name := range Vocabularies["VoiceControlPhase"] {
		phase := VoiceControlPhase(name)
		closed := phase == VoiceControlPhaseStarting || phase.Final()
		for _, tt := range []struct {
			name   string
			mutate func(*VoiceControlStateV3)
			ok     bool
		}{
			{"off", func(*VoiceControlStateV3) {}, true},
			{"off and muted", func(s *VoiceControlStateV3) { s.ShareAudioMuted = true }, true},
			{"on", on(2), !closed},
			{"on with nothing playing", on(0), !closed},
			{"unavailable", unavailable(VoiceControlShareAudioProblemNotConnected), !closed},
		} {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				s := validVoiceControlStateV3()
				inVoiceControlPhaseV3(&s, phase)
				if err := s.Validate(); err != nil {
					t.Fatalf("the state is not valid before the share's sound is set: %v", err)
				}
				tt.mutate(&s)
				err := s.Validate()
				switch {
				case tt.ok && err != nil:
					t.Errorf("Validate() = %v, want nil", err)
				case !tt.ok && (err == nil || !strings.Contains(err.Error(), "must have share_audio off")):
					t.Errorf("Validate() = %v, want it refused: share_audio is off in this phase", err)
				}
			})
		}
	}
}

func TestVoiceControlSetShareAudioV3Validate(t *testing.T) {
	longest := strings.Repeat("n", VoiceControlMaxApplicationNameBytes)
	const system, application = VoiceShareAudioScopeSystemExcludingCall, VoiceShareAudioScopeApplication
	tests := []struct {
		name      string
		capturing bool
		scope     VoiceShareAudioScope
		app       string
		wantErr   string
	}{
		{name: "the system's sound", capturing: true, scope: system, app: "Conch"},
		{name: "one application's sound: valid, the helper says whether it can", capturing: true, scope: application, app: "Conch"},
		{name: "a stop", capturing: false},
		{name: "the longest name", capturing: true, scope: system, app: longest},
		{name: "every printable ASCII character", capturing: true, scope: system, app: ` !"#$%&'()*+,-./09:;<=>?@AZ[\]^_` + "`" + `az{|}~`},
		{name: "no scope", capturing: true, app: "Conch", wantErr: "scope \"\" is not one of"},
		{name: "a scope of no version", capturing: true, scope: "system", app: "Conch", wantErr: "is not one of application, system_excluding_call"},
		{name: "no name", capturing: true, scope: system, wantErr: "application_name must be 1 to 128 bytes"},
		{name: "a name too long", capturing: true, scope: system, app: longest + "n", wantErr: "application_name must be 1 to 128 bytes"},
		{name: "an escape sequence", capturing: true, scope: system, app: "\x1b[2JConch", wantErr: "printable ASCII"},
		{name: "a line feed", capturing: true, scope: system, app: "two\nlines", wantErr: "printable ASCII"},
		{name: "a tab", capturing: true, scope: system, app: "Conch\tDesktop", wantErr: "printable ASCII"},
		{name: "the delete character", capturing: true, scope: system, app: "Conch\x7f", wantErr: "printable ASCII"},
		{name: "a letter outside ASCII", capturing: true, scope: system, app: "Cönch", wantErr: "printable ASCII"},
		{name: "not UTF-8", capturing: true, scope: system, app: "\xc3\x28", wantErr: "printable ASCII"},
		{name: "a stop with a scope", scope: system, wantErr: "capturing false must carry no scope"},
		{name: "a stop with a scope of no version", scope: "system", wantErr: "capturing false must carry no scope"},
		{name: "a stop with a name", app: "Conch", wantErr: "capturing false must carry no application_name"},
		{name: "a stop with both", scope: system, app: "Conch", wantErr: "capturing false must carry no scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := VoiceControlSetShareAudioV3{
				Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetShareAudio, ID: 8,
				Capturing: tt.capturing, Scope: tt.scope, ApplicationName: tt.app,
			}
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
			// The name is the owner's text: no error repeats it.
			if err != nil && len(tt.app) > 3 && strings.Contains(err.Error(), tt.app) {
				t.Errorf("the error repeats the application name: %v", err)
			}
		})
	}
	// The scopes a set_share_audio takes are the consent's, all of them.
	for _, scope := range Vocabularies["VoiceShareAudioScope"] {
		c := VoiceControlSetShareAudioV3{
			Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetShareAudio, ID: 8,
			Capturing: true, Scope: VoiceShareAudioScope(scope), ApplicationName: "Conch",
		}
		if err := c.Validate(); err != nil {
			t.Errorf("scope %s: Validate() = %v", scope, err)
		}
	}
}

func TestMarshalVoiceControlLineV3(t *testing.T) {
	t.Run("the longest set_share_audio fits on a line", func(t *testing.T) {
		// Every character of the name written as a two-byte escape.
		cmd := VoiceControlSetShareAudioV3{
			Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetShareAudio, ID: VoiceControlMaxID,
			Capturing: true, Scope: VoiceShareAudioScopeSystemExcludingCall,
			ApplicationName: strings.Repeat(`"`, VoiceControlMaxApplicationNameBytes),
		}
		line, err := MarshalVoiceControlLine(cmd)
		if err != nil {
			t.Fatal(err)
		}
		if len(line)-1 > VoiceControlMaxLineBytes {
			t.Fatalf("%d bytes", len(line)-1)
		}
		got, err := ParseVoiceControlCommandV3(bytes.TrimSuffix(line, []byte{'\n'}))
		if err != nil || !reflect.DeepEqual(*got.SetShareAudio, cmd) {
			t.Fatalf("read back as %+v, %v", got.SetShareAudio, err)
		}
	})
	t.Run("a stop writes no scope and no name", func(t *testing.T) {
		line, err := MarshalVoiceControlLine(VoiceControlSetShareAudioV3{
			Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetShareAudio, ID: 9,
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"schema":"conch.voice_control.v3","kind":"set_share_audio","id":9,"capturing":false}` + "\n"; string(line) != want {
			t.Errorf("wrote %s", line)
		}
	})
	t.Run("a state always writes three of the four, and the problem only when there is one", func(t *testing.T) {
		line, err := MarshalVoiceControlLine(validVoiceControlStateV3())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(line, []byte(`"ptt":"ready","share_audio":"off","share_audio_muted":false,"share_audio_sources":0}`+"\n")) {
			t.Errorf("wrote %s", line)
		}
	})
	t.Run("an invalid message is not written", func(t *testing.T) {
		on := validVoiceControlStateV3()
		on.ShareAudio = VoiceControlShareAudioOn
		for _, msg := range []VoiceControlMessage{
			VoiceControlSetShareAudioV3{Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetShareAudio, ID: 8, Capturing: true},
			VoiceControlSetShareAudioV3{Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetShareAudio, ID: 8, Scope: VoiceShareAudioScopeApplication},
			VoiceControlSetShareAudioV3{Schema: VoiceControlSchemaV2, Kind: VoiceControlCommandV3SetShareAudio, ID: 8},
			VoiceControlSetShareAudioMuteV3{Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetShareAudio, ID: 10},
			VoiceControlSetShareAudioMuteV3{Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetShareAudioMute},
			VoiceControlSetMuteV3{Schema: VoiceControlSchemaV2, Kind: VoiceControlCommandV3SetMute, ID: 1},
			VoiceControlSetDevicesV3{Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetDevices, ID: 6},
			VoiceControlSetTalkModeV3{Schema: VoiceControlSchemaV3, Kind: VoiceControlCommandV3SetTalkMode, ID: 7, TalkMode: "always_on"},
			VoiceControlAckV3{Schema: VoiceControlSchemaV3, Kind: VoiceControlEventAck, Seq: 3, ID: 1, Error: VoiceControlErrorTooLong},
			VoiceControlProtocolErrorV3{Schema: VoiceControlSchemaV3, Kind: VoiceControlEventProtocolError, Seq: 3, Code: VoiceControlErrorStopping},
			on,
		} {
			if line, err := MarshalVoiceControlLine(msg); err == nil {
				t.Errorf("wrote %s", line)
			}
		}
	})
}

func TestVoiceControlV3CommandAccessors(t *testing.T) {
	if (VoiceControlCommandV3{}).Kind() != "" || (VoiceControlCommandV3{}).ID() != 0 {
		t.Error("an empty command has a kind or an id")
	}
	commands := []VoiceControlCommandV3{
		{Start: &VoiceControlStartV3{ID: 1}},
		{SetMute: &VoiceControlSetMuteV3{ID: 2}},
		{SetDeafen: &VoiceControlSetDeafenV3{ID: 3}},
		{GetState: &VoiceControlGetStateV3{ID: 4}},
		{Leave: &VoiceControlLeaveV3{ID: 5}},
		{SetDevices: &VoiceControlSetDevicesV3{ID: 6}},
		{SetTalkMode: &VoiceControlSetTalkModeV3{ID: 7}},
		{SetShareAudio: &VoiceControlSetShareAudioV3{ID: 8}},
		{SetShareAudioMute: &VoiceControlSetShareAudioMuteV3{ID: 9}},
	}
	var kinds []string
	for i, c := range commands {
		if !c.Kind().Valid() || c.ID() != int64(i+1) || c.message() == nil {
			t.Errorf("%+v: kind %q, id %d", c, c.Kind(), c.ID())
		}
		kinds = append(kinds, string(c.Kind()))
	}
	if !slices.Equal(kinds, Vocabularies["VoiceControlCommandKindV3"]) {
		t.Errorf("the commands have kinds %q, the vocabulary is %q", kinds, Vocabularies["VoiceControlCommandKindV3"])
	}
}
