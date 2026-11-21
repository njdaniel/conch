package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// voiceControlV4Dir is the fixture directory for the v4 start, diagnostics
// and call quality (issues #2039 and #2085).
const voiceControlV4Dir = "voice-control-v4"

// voiceControlV4Hello is the hello of a helper that offers v1 to v4: v1, in
// its frozen shape.
const voiceControlV4Hello = "event-hello-v1-v2-v3-and-v4.json"

// voiceControlV4CallID is the attempt id the call fixtures name.
const voiceControlV4CallID = "k3Jx9dQ0mT7vB2nYw5LcRg"

func readVoiceControlV4Fixture(t voiceControlReporter, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", voiceControlV4Dir, filepath.FromSlash(name))) // #nosec G304 -- a fixture name from this package's own tables
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// voiceControlV4Golden is every single-message fixture of v4 and its type.
var voiceControlV4Golden = func() []struct {
	file  string
	event bool
	v1    bool
	new   func() any
} {
	type row = struct {
		file  string
		event bool
		v1    bool
		new   func() any
	}
	rows := []row{
		{file: "command-start-channel.json", new: func() any { return new(VoiceControlStartV4) }},
		{file: "command-start-call.json", new: func() any { return new(VoiceControlStartV4) }},
		{file: "command-set-diagnostics.json", new: func() any { return new(VoiceControlSetDiagnosticsV4) }},
		{file: "command-set-diagnostics-off.json", new: func() any { return new(VoiceControlSetDiagnosticsV4) }},
		{file: "command-get-call-quality.json", new: func() any { return new(VoiceControlGetCallQualityV4) }},
		{file: voiceControlV4Hello, event: true, v1: true, new: func() any { return new(VoiceControlHelloV1) }},
		{file: "event-levels.json", event: true, new: func() any { return new(VoiceControlLevelsV4) }},
		{file: "event-gate-event.json", event: true, new: func() any { return new(VoiceControlGateEventV4) }},
		{file: "event-gate-event-close.json", event: true, new: func() any { return new(VoiceControlGateEventV4) }},
		{file: "event-call-quality.json", event: true, new: func() any { return new(VoiceControlCallQualityV4) }},
		{file: "event-call-quality-partial.json", event: true, new: func() any { return new(VoiceControlCallQualityV4) }},
		{file: "event-call-quality-no-metrics.json", event: true, new: func() any { return new(VoiceControlCallQualityV4) }},
	}
	for _, g := range voiceControlV3Golden {
		if g.file == "command-start.json" || g.v1 {
			continue
		}
		var n func() any
		switch {
		case strings.HasPrefix(g.file, "event-state"):
			n = func() any { return new(VoiceControlStateV4) }
		case strings.HasPrefix(g.file, "event-ack"):
			n = func() any { return new(VoiceControlAckV4) }
		case strings.HasPrefix(g.file, "event-protocol"):
			n = func() any { return new(VoiceControlProtocolErrorV4) }
		default:
			n = map[string]func() any{
				"command-set-mute.json":        func() any { return new(VoiceControlSetMuteV4) },
				"command-set-deafen.json":      func() any { return new(VoiceControlSetDeafenV4) },
				"command-get-state.json":       func() any { return new(VoiceControlGetStateV4) },
				"command-leave.json":           func() any { return new(VoiceControlLeaveV4) },
				"command-set-talk-mode.json":   func() any { return new(VoiceControlSetTalkModeV4) },
				"command-set-devices.json":     func() any { return new(VoiceControlSetDevicesV4) },
				"command-set-share-audio.json": func() any { return new(VoiceControlSetShareAudioV4) },
			}[g.file]
			switch {
			case n != nil:
			case strings.HasPrefix(g.file, "command-set-devices"):
				n = func() any { return new(VoiceControlSetDevicesV4) }
			case strings.HasPrefix(g.file, "command-set-share-audio-mute"):
				n = func() any { return new(VoiceControlSetShareAudioMuteV4) }
			case strings.HasPrefix(g.file, "command-set-share-audio"):
				n = func() any { return new(VoiceControlSetShareAudioV4) }
			}
		}
		rows = append(rows, row{file: g.file, event: g.event, new: n})
	}
	return rows
}()

func (c VoiceControlCommandV4) message() VoiceControlMessage {
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
	case c.SetDiagnostics != nil:
		return c.SetDiagnostics
	case c.GetCallQuality != nil:
		return c.GetCallQuality
	}
	return nil
}

func (e VoiceControlEventV4) message() VoiceControlMessage {
	switch {
	case e.Ack != nil:
		return e.Ack
	case e.State != nil:
		return e.State
	case e.ProtocolError != nil:
		return e.ProtocolError
	case e.Levels != nil:
		return e.Levels
	case e.GateEvent != nil:
		return e.GateEvent
	case e.CallQuality != nil:
		return e.CallQuality
	}
	return nil
}

func (e VoiceControlEventV4) kind() VoiceControlEventKindV4 {
	switch {
	case e.Ack != nil:
		return VoiceControlEventV4Ack
	case e.State != nil:
		return VoiceControlEventV4State
	case e.ProtocolError != nil:
		return VoiceControlEventV4ProtocolError
	case e.Levels != nil:
		return VoiceControlEventV4Levels
	case e.GateEvent != nil:
		return VoiceControlEventV4GateEvent
	case e.CallQuality != nil:
		return VoiceControlEventV4CallQuality
	}
	return e.Unknown
}

// TestVoiceControlV4GoldenFixtures checks each single-message fixture as the
// v3 test does: it round-trips through its type byte for byte, its compact
// form is accepted by the reader of its direction and version, and
// MarshalVoiceControlLine writes exactly that compact form.
func TestVoiceControlV4GoldenFixtures(t *testing.T) {
	for _, tt := range voiceControlV4Golden {
		t.Run(tt.file, func(t *testing.T) {
			if tt.new == nil {
				t.Fatal("no type for the fixture")
			}
			assertGoldenFixture(t, voiceControlV4Dir+"/"+tt.file, tt.new)
			compact := compactVoiceControlFixture(t, readVoiceControlV4Fixture(t, tt.file))
			var msg VoiceControlMessage
			switch {
			case tt.v1:
				ev, err := ParseVoiceControlEvent(compact)
				if err != nil {
					t.Fatalf("the v1 reader refuses it: %v", err)
				}
				msg = ev.message()
			case tt.event:
				ev, err := ParseVoiceControlEventV4(compact)
				if err != nil {
					t.Fatalf("the owner's reader refuses it: %v", err)
				}
				msg = ev.message()
			default:
				cmd, err := ParseVoiceControlCommandV4(compact)
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

// TestVoiceControlV4GoldenFixturesAreV3s pins the carried-over wire shapes:
// v3 states gain processing and every v3 command still has a v4 equivalent.
func TestVoiceControlV4GoldenFixturesAreV3s(t *testing.T) {
	for _, g := range voiceControlV3Golden {
		if g.file == "command-start.json" || g.v1 {
			continue
		}
		want := strings.ReplaceAll(string(readVoiceControlV3Fixture(t, g.file)), VoiceControlSchemaV3, VoiceControlSchemaV4)
		got := string(readVoiceControlV4Fixture(t, g.file))
		if strings.HasPrefix(g.file, "event-state-") {
			prefix, _, found := strings.Cut(got, ",\n  \"processing\": ")
			if !found {
				t.Fatalf("%s has no processing field", g.file)
			}
			got = prefix + "\n}\n"
		}
		if got != want {
			t.Errorf("%s differs from its v3 fixture beyond the v4 additions", g.file)
		}
	}
	start := strings.ReplaceAll(string(readVoiceControlV3Fixture(t, "command-start.json")), VoiceControlSchemaV3, VoiceControlSchemaV4)
	if string(readVoiceControlV4Fixture(t, "command-start-channel.json")) != start {
		t.Error("command-start-channel.json is not v3's start with the schema name changed")
	}
}

var voiceControlV4CaseFiles = []string{"cases.json", "cases-diagnostics-and-call-quality.json"}

func loadVoiceControlV4Cases(t *testing.T) []voiceControlCase {
	t.Helper()
	var cases []voiceControlCase
	for _, name := range voiceControlV4CaseFiles {
		var doc struct {
			Cases []voiceControlCase `json:"cases"`
		}
		if err := json.Unmarshal(readVoiceControlV4Fixture(t, name), &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Cases) == 0 {
			t.Fatalf("%s: no cases", name)
		}
		cases = append(cases, doc.Cases...)
	}
	return cases
}

// checkVoiceControlV4Case holds what a v4 reader made of a line to what a
// case wants.
func checkVoiceControlV4Case(t *testing.T, c voiceControlCase, line []byte) {
	t.Helper()
	var kind string
	var err error
	var skipped bool
	switch c.Direction {
	case "command":
		var cmd VoiceControlCommandV4
		cmd, err = ParseVoiceControlCommandV4(line)
		kind = string(cmd.Kind())
	case "event":
		var ev VoiceControlEventV4
		ev, err = ParseVoiceControlEventV4(line)
		kind, skipped = string(ev.kind()), err == nil && ev.Unknown != ""
	default:
		t.Fatalf("direction %q is neither command nor event", c.Direction)
	}
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

// TestVoiceControlV4Cases runs the destination, diagnostics and call quality cases.
func TestVoiceControlV4Cases(t *testing.T) {
	names := map[string]bool{}
	for _, c := range loadVoiceControlV4Cases(t) {
		key := c.Direction + ": " + c.Name
		if names[key] {
			t.Errorf("case %q appears twice", key)
		}
		names[key] = true
		t.Run(key, func(t *testing.T) {
			checkVoiceControlV4Case(t, c, c.line(t))
		})
	}
}

// TestVoiceControlV4IsV3 runs every conformance case of v3 against the v4
// readers under v4's name and with processing added to states.
// So no rule of v3's was lost or changed on the way, in either direction,
// without a list of rules kept twice. The cases that are about a line of
// another version being refused are left out: what a v3 line is to a v4
// reader is TestVoiceControlV4ReadersKeepVersionsApart's.
func TestVoiceControlV4IsV3(t *testing.T) {
	tried := 0
	for _, c := range loadVoiceControlV3Cases(t) {
		line := c.lineV3(t)
		if c.Want.Result == "refuse" && c.Want.Code == "unknown_schema" {
			continue
		}
		line = bytes.ReplaceAll(line, []byte(VoiceControlSchemaV3), []byte(VoiceControlSchemaV4))
		// Keep malformed framing as is, and preserve the original fields
		// of well-framed states, including their original number spellings.
		if fields, err := decodeVoiceControlObject(line); err == nil && c.Direction == "event" {
			if kind, _ := fieldString(fields, "kind"); kind == "state" {
				const processing = `,"processing":{"echo_cancellation":false,"noise_suppression":false}`
				if c.LineFile == "lines/event-at-limit.txt" {
					// Shorten the ignored padding to keep this case at the limit.
					line = bytes.Replace(line, bytes.Repeat([]byte("x"), len(processing)), nil, 1)
				}
				line = append(bytes.TrimSuffix(bytes.TrimSpace(line), []byte("}")),
					[]byte(processing+"}")...)
			}
		}
		tried++
		t.Run(c.Direction+": "+c.Name, func(t *testing.T) {
			checkVoiceControlV4Case(t, c, line)
		})
	}
	if tried < 100 {
		t.Errorf("only %d cases of v3 were tried in v4", tried)
	}
}

// TestVoiceControlV4ReadersKeepVersionsApart: a v4 line read by the readers
// of v1, v2 or v3 is unknown_schema, a v1, v2 or v3 line read by the v4
// readers is, and a hello in v4 is invalid. So v3 keeps working exactly as it
// does, with no helper or owner changed, and v4 is used only when both sides
// choose it.
func TestVoiceControlV4ReadersKeepVersionsApart(t *testing.T) {
	cmd := func(version string) []byte {
		return []byte(`{"schema":"` + version + `","kind":"get_state","id":1}`)
	}
	ev := func(version string) []byte {
		return []byte(`{"schema":"` + version + `","kind":"protocol_error","seq":2,"code":"malformed"}`)
	}
	wantSchema := func(t *testing.T, err error) {
		t.Helper()
		var vcErr *VoiceControlError
		if !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownSchema {
			t.Errorf("got %v, want unknown_schema", err)
		}
	}
	for _, other := range []string{VoiceControlSchemaV1, VoiceControlSchemaV2, VoiceControlSchemaV3} {
		_, err := ParseVoiceControlCommandV4(cmd(other))
		wantSchema(t, err)
		_, err = ParseVoiceControlEventV4(ev(other))
		wantSchema(t, err)
	}
	_, err := ParseVoiceControlCommand(cmd(VoiceControlSchemaV4))
	wantSchema(t, err)
	_, err = ParseVoiceControlCommandV2(cmd(VoiceControlSchemaV4))
	wantSchema(t, err)
	_, err = ParseVoiceControlCommandV3(cmd(VoiceControlSchemaV4))
	wantSchema(t, err)
	_, err = ParseVoiceControlEvent(ev(VoiceControlSchemaV4))
	wantSchema(t, err)
	_, err = ParseVoiceControlEventV2(ev(VoiceControlSchemaV4))
	wantSchema(t, err)
	_, err = ParseVoiceControlEventV3(ev(VoiceControlSchemaV4))
	wantSchema(t, err)
	// A v3 reader, handed the call start, refuses it by its schema before
	// it could read a channel that is not there.
	_, err = ParseVoiceControlCommandV3(readVoiceControlV4Fixture(t, "command-start-call.json"))
	if err == nil {
		t.Error("the v3 reader accepted a v4 call start")
	}
	hello := []byte(`{"schema":"` + VoiceControlSchemaV4 + `","kind":"hello","seq":1,"versions":["` + VoiceControlSchemaV4 + `"],"helper_version":"0.4.0"}`)
	var vcErr *VoiceControlError
	if _, err := ParseVoiceControlEventV4(hello); !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorInvalid {
		t.Errorf("a hello in v4: %v, want invalid", err)
	}
}

// TestVoiceControlV4Hello: the hello that offers v4 is a well-formed v1
// hello, and it offers every version, so an owner that speaks any of them
// picks its own. A helper that offers v3 and not v4 is a v3 helper, and an
// owner reading its hello sees that v4 is not offered.
func TestVoiceControlV4Hello(t *testing.T) {
	ev, err := ParseVoiceControlEvent(compactVoiceControlFixture(t, readVoiceControlV4Fixture(t, voiceControlV4Hello)))
	if err != nil || ev.Hello == nil {
		t.Fatalf("the hello is refused: %v", err)
	}
	for _, v := range []string{VoiceControlSchemaV1, VoiceControlSchemaV2, VoiceControlSchemaV3, VoiceControlSchemaV4} {
		if !ev.Hello.Offers(v) {
			t.Errorf("the hello does not offer %s", v)
		}
	}
	old, err := ParseVoiceControlEvent(compactVoiceControlFixture(t, readVoiceControlV3Fixture(t, voiceControlV3Hello)))
	if err != nil || old.Hello == nil {
		t.Fatalf("the v3 hello is refused: %v", err)
	}
	if old.Hello.Offers(VoiceControlSchemaV4) {
		t.Error("a v3 helper's hello offers v4")
	}
}

// TestVoiceControlCommandKindV4 pins the carried-over kinds and the v4 additions.
func TestVoiceControlCommandKindV4(t *testing.T) {
	for _, k := range []VoiceControlCommandKindV3{
		VoiceControlCommandV3Start, VoiceControlCommandV3SetMute, VoiceControlCommandV3SetDeafen,
		VoiceControlCommandV3GetState, VoiceControlCommandV3Leave, VoiceControlCommandV3SetDevices,
		VoiceControlCommandV3SetTalkMode, VoiceControlCommandV3SetShareAudio, VoiceControlCommandV3SetShareAudioMute,
	} {
		if !VoiceControlCommandKindV4(k).Valid() {
			t.Errorf("%q is a kind of v3 and not of v4", k)
		}
	}
	for _, k := range []VoiceControlCommandKindV4{VoiceControlCommandV4SetDiagnostics, VoiceControlCommandV4GetCallQuality} {
		if !k.Valid() || VoiceControlCommandKindV3(k).Valid() {
			t.Errorf("%q must be a v4-only command kind", k)
		}
	}
	for _, k := range []VoiceControlCommandKindV4{"", "place_call", "set_volume", "START"} {
		if k.Valid() {
			t.Errorf("%q is a kind of v4", k)
		}
	}
	if !strings.Contains(string(VoiceControlCommandV4Start), "start") {
		t.Error("the start kind is not start")
	}
}

// voiceControlV4Transcripts is every transcript of v4 and the version its
// session is in, except the diagnostics and quality transcripts checked below.
var voiceControlV4Transcripts = map[string]string{
	"transcript-negotiation-v4-owner-channel.json": VoiceControlSchemaV4,
	"transcript-negotiation-v4-owner-call.json":    VoiceControlSchemaV4,
	"transcript-negotiation-v3-owner.json":         VoiceControlSchemaV3,
	"transcript-negotiation-v3-helper.json":        VoiceControlSchemaV3,
	"transcript-version-fixed.json":                VoiceControlSchemaV4,
}

const (
	voiceControlV4DiagnosticsTranscript = "transcript-diagnostics-and-call-quality.json"
	voiceControlV4OverlappingPolls      = "transcript-overlapping-call-quality-polls.json"
)

func readVoiceControlV4Transcript(t *testing.T, name string) []voiceControlStep {
	t.Helper()
	var doc struct {
		Description string             `json:"description"`
		Steps       []voiceControlStep `json:"steps"`
	}
	if err := json.Unmarshal(readVoiceControlV4Fixture(t, name), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Description == "" || len(doc.Steps) == 0 {
		t.Fatal("transcript needs a description and steps")
	}
	return doc.Steps
}

// TestVoiceControlV4Transcripts reads every line of every transcript with the
// reader of its direction and the session's version. It checks the outcome
// of sharing sound in a call, then runs the stream through v3's ordering
// rules with the version boundary and destination translated.
func TestVoiceControlV4Transcripts(t *testing.T) {
	for name, version := range voiceControlV4Transcripts {
		t.Run(name, func(t *testing.T) {
			steps := readVoiceControlV4Transcript(t, name)
			callStarts := 0
			shareRequest := -1
			versionMismatch := false
			asV3 := make([]voiceControlStep, len(steps))
			for i, s := range steps {
				asV3[i] = s
				isShareOutcome := shareRequest >= 0 && i == shareRequest+1
				if isShareOutcome && (s.From != "helper" || len(s.Message) == 0) {
					t.Fatalf("step %d: want helper's share audio outcome", i)
				}
				if len(s.Message) == 0 {
					continue
				}
				line := compactVoiceControlFixture(t, s.Message)
				var f map[string]json.RawMessage
				if err := json.Unmarshal(line, &f); err != nil {
					t.Fatal(err)
				}
				schema, _ := fieldString(f, "schema")
				if version == VoiceControlSchemaV3 {
					if _, present := f["processing"]; present {
						t.Errorf("step %d: processing on a v3 line", i)
					}
				}
				switch {
				case s.From == "helper" && schema == VoiceControlSchemaV1:
					ev, err := ParseVoiceControlEvent(line)
					if err != nil || ev.Hello == nil || i != 0 {
						t.Fatalf("step %d: want initial hello, got %v", i, err)
					}
					if ev.Hello.Offers(VoiceControlSchemaV4) != (name != "transcript-negotiation-v3-helper.json") {
						t.Errorf("step %d: unexpected versions in hello: %v", i, ev.Hello.Versions)
					}
				case s.From == "helper" && schema == version:
					if version == VoiceControlSchemaV3 {
						ev, err := ParseVoiceControlEventV3(line)
						if err != nil || ev.Unknown != "" {
							t.Fatalf("step %d: want a recognized v3 event, got %q, %v", i, ev.Unknown, err)
						}
						break
					}
					ev, err := ParseVoiceControlEventV4(line)
					if err != nil {
						t.Fatalf("step %d: %v", i, err)
					}
					if callStarts > 0 && ev.State != nil && ev.State.ShareAudio == VoiceControlShareAudioOn {
						t.Errorf("step %d: share audio is on during a private call", i)
					}
					if isShareOutcome &&
						(ev.State == nil || ev.State.ShareAudio != VoiceControlShareAudioUnavailable ||
							ev.State.ShareAudioProblem != VoiceControlShareAudioProblemUnsupported ||
							ev.State.ShareAudioSources != 0) {
						t.Errorf("step %d: want share audio unavailable/unsupported with no sources before the ack", i)
					}
				case s.From == "owner" && schema == version:
					if version == VoiceControlSchemaV3 {
						if _, err := ParseVoiceControlCommandV3(line); err != nil {
							t.Fatalf("step %d: %v", i, err)
						}
						break
					}
					cmd, err := ParseVoiceControlCommandV4(line)
					if err != nil {
						t.Fatalf("step %d: %v", i, err)
					}
					if callStarts > 0 && cmd.SetShareAudio != nil && cmd.SetShareAudio.Capturing {
						if shareRequest >= 0 {
							t.Fatalf("step %d: a second request to share audio", i)
						}
						shareRequest = i
					}
				case s.From == "owner" && version == VoiceControlSchemaV4 && schema == VoiceControlSchemaV3:
					if name != "transcript-version-fixed.json" || i != len(steps)-4 {
						t.Fatalf("step %d: expected the wrong-version command immediately before the failure sequence", i)
					}
					_, err := ParseVoiceControlCommandV4(line)
					var refusal *VoiceControlError
					if !errors.As(err, &refusal) || refusal.Code != VoiceControlErrorUnknownSchema || refusal.ID != 0 {
						t.Fatalf("step %d: got %v, want unknown_schema with no id", i, err)
					}
					versionMismatch = true
					// In the translated v3 session, a v4 command has the
					// same fatal error as the original v3 command in v4.
					f["schema"] = json.RawMessage(`"` + VoiceControlSchemaV4 + `"`)
				default:
					t.Fatalf("step %d: unexpected schema %q from %q", i, schema, s.From)
				}
				if version == VoiceControlSchemaV4 && s.From == "owner" && schema == version {
					if _, ok := f["call"]; ok {
						callStarts++
						f["channel"] = json.RawMessage(`"general"`)
						delete(f, "call")
					}
				}
				if version == VoiceControlSchemaV4 && schema == VoiceControlSchemaV4 {
					f["schema"] = json.RawMessage(`"` + VoiceControlSchemaV3 + `"`)
					delete(f, "processing")
				}
				if schema == VoiceControlSchemaV1 {
					// The hello is carried over as it is.
					continue
				}
				out, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				asV3[i].Message = out
			}
			if strings.Contains(name, "call") != (callStarts == 1) {
				t.Errorf("%d call starts in %s", callStarts, name)
			}
			if callStarts == 1 && shareRequest < 0 {
				t.Error("the call session never requests share audio")
			}
			if name == "transcript-version-fixed.json" && !versionMismatch {
				t.Error("the session never receives a wrong-version command")
			}
			if got := checkVoiceControlV3Transcript(t, asV3); got != VoiceControlSchemaV3 {
				t.Errorf("the session read as v3 is in %q", got)
			}
		})
	}
}

// TestVoiceControlV4DiagnosticsTranscript pins the opt-in diagnostics and
// the per-participant sample followed by an ack, including the empty sample.
func TestVoiceControlV4DiagnosticsTranscript(t *testing.T) {
	checkVoiceControlV4QualityTranscript(t, voiceControlV4DiagnosticsTranscript, []string{
		"ack 1 true",
		"diagnostics 2 true", "ack 2 true", "levels false", "gate true sound", "levels true", "gate false hang_ended",
		"diagnostics 3 false", "ack 3 true",
		"poll 4", "quality 9", "quality 11", "quality 14", "ack 4 true",
		"poll 5", "ack 5 true",
		"ack 6 true",
	})
}

func TestVoiceControlV4OverlappingQualityPolls(t *testing.T) {
	checkVoiceControlV4QualityTranscript(t, voiceControlV4OverlappingPolls, []string{
		"ack 1 true",
		"poll 2", "poll 3", "ack 2 true", "ack 3 false invalid",
		"poll 4", "quality 9", "ack 4 true",
		"ack 5 true",
	})
}

func checkVoiceControlV4QualityTranscript(t *testing.T, name string, want []string) {
	t.Helper()
	steps := readVoiceControlV4Transcript(t, name)
	hello, err := ParseVoiceControlEvent(compactVoiceControlFixture(t, steps[0].Message))
	if err != nil || hello.Hello == nil || !hello.Hello.Offers(VoiceControlSchemaV4) || steps[0].From != "helper" {
		t.Fatalf("want the v1 hello offering v4, got %v", err)
	}

	var observed []string
	var lastID int64
	lastSeq := hello.Hello.Seq
	for i, step := range steps[1:] {
		if len(step.Message) == 0 {
			continue
		}
		line := compactVoiceControlFixture(t, step.Message)
		if step.From == "owner" {
			cmd, err := ParseVoiceControlCommandV4(line)
			if err != nil || cmd.ID() != lastID+1 {
				t.Fatalf("step %d: want v4 command %d, got %d, %v", i+1, lastID+1, cmd.ID(), err)
			}
			lastID = cmd.ID()
			switch {
			case cmd.SetDiagnostics != nil:
				observed = append(observed, fmt.Sprintf("diagnostics %d %t", lastID, cmd.SetDiagnostics.On))
			case cmd.GetCallQuality != nil:
				observed = append(observed, fmt.Sprintf("poll %d", lastID))
			}
			continue
		}
		ev, err := ParseVoiceControlEventV4(line)
		if step.From != "helper" || err != nil || ev.Unknown != "" || ev.Seq != lastSeq+1 {
			t.Fatalf("step %d: want v4 helper event %d, got %s at %d, %v", i+1, lastSeq+1, ev.kind(), ev.Seq, err)
		}
		lastSeq = ev.Seq
		switch {
		case ev.Levels != nil:
			observed = append(observed, fmt.Sprintf("levels %t", ev.Levels.GateOpen))
		case ev.GateEvent != nil:
			observed = append(observed, fmt.Sprintf("gate %t %s", ev.GateEvent.Open, ev.GateEvent.Why))
		case ev.CallQuality != nil:
			observed = append(observed, fmt.Sprintf("quality %d", ev.CallQuality.PrincipalID))
		case ev.Ack != nil:
			ack := fmt.Sprintf("ack %d %t", ev.Ack.ID, ev.Ack.OK)
			if !ev.Ack.OK {
				ack += " " + string(ev.Ack.Error)
			}
			observed = append(observed, ack)
		}
	}
	if !reflect.DeepEqual(observed, want) {
		t.Errorf("got %q, want %q", observed, want)
	}
}

// TestVoiceControlV4FixturesAreAllClaimed: every file under the v4 directory
// is read by a test.
func TestVoiceControlV4FixturesAreAllClaimed(t *testing.T) {
	claimed := map[string]bool{voiceControlV4DiagnosticsTranscript: true, voiceControlV4OverlappingPolls: true}
	for _, name := range voiceControlV4CaseFiles {
		claimed[name] = true
	}
	for _, g := range voiceControlV4Golden {
		claimed[g.file] = true
	}
	for name := range voiceControlV4Transcripts {
		claimed[name] = true
	}
	for _, f := range fixtureFilesUnder(t, voiceControlV4Dir) {
		if !claimed[f] {
			t.Errorf("testdata/%s/%s is read by no test", voiceControlV4Dir, f)
		}
	}
}

// voiceControlV4Types holds the messages carried over from v3.
var voiceControlV4Types = []VoiceControlMessage{
	VoiceControlStartV4{}, VoiceControlSetMuteV4{}, VoiceControlGetStateV4{}, VoiceControlLeaveV4{}, VoiceControlSetDeafenV4{},
	VoiceControlSetDevicesV4{}, VoiceControlSetTalkModeV4{},
	VoiceControlSetShareAudioV4{}, VoiceControlSetShareAudioMuteV4{},
	VoiceControlAckV4{}, VoiceControlStateV4{}, VoiceControlProtocolErrorV4{},
}

// TestVoiceControlV4FieldSets pins the legacy and new wire fields by name,
// type and order. It also checks the field names and fixtures for secrets.
func TestVoiceControlV4FieldSets(t *testing.T) {
	v3 := []any{
		VoiceControlStartV3{}, VoiceControlSetMuteV3{}, VoiceControlGetStateV3{}, VoiceControlLeaveV3{}, VoiceControlSetDeafenV3{},
		VoiceControlSetDevicesV3{}, VoiceControlSetTalkModeV3{},
		VoiceControlSetShareAudioV3{}, VoiceControlSetShareAudioMuteV3{},
		VoiceControlAckV3{}, VoiceControlStateV3{}, VoiceControlProtocolErrorV3{},
	}
	fields := func(v any) (names []string) {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			typ := strings.TrimPrefix(rt.Field(i).Type.String(), "schema.")
			names = append(names, rt.Field(i).Tag.Get("json")+" "+typ)
		}
		return names
	}
	for i, m := range voiceControlV4Types {
		got, want := fields(m), fields(v3[i])
		for j, field := range want {
			switch field {
			case "kind VoiceControlEventKind":
				want[j] = "kind VoiceControlEventKindV4"
			case "kind VoiceControlCommandKindV3":
				want[j] = "kind VoiceControlCommandKindV4"
			}
		}
		if _, isStart := m.(VoiceControlStartV4); isStart {
			// channel stays (now optional) and call is new, after it.
			var withCall []string
			for _, n := range want {
				withCall = append(withCall, n)
				if n == "channel string" {
					withCall = append(withCall, "call,omitempty string")
				}
			}
			want = withCall
			for j, n := range want {
				if n == "channel string" {
					want[j] = "channel,omitempty string"
				}
			}
		}
		if _, isState := m.(VoiceControlStateV4); isState {
			want = append(want, "processing VoiceControlProcessingV4")
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%T has the fields %v, want %v", m, got, want)
		}
	}
	newShapes := []struct {
		value any
		want  []string
	}{
		{VoiceControlSetDiagnosticsV4{}, []string{"schema string", "kind VoiceControlCommandKindV4", "id int64", "on bool"}},
		{VoiceControlGetCallQualityV4{}, []string{"schema string", "kind VoiceControlCommandKindV4", "id int64"}},
		{VoiceControlProcessingV4{}, []string{"echo_cancellation bool", "noise_suppression bool"}},
		{VoiceControlLevelsV4{}, []string{
			"schema string", "kind VoiceControlEventKindV4", "seq int64",
			"level_db int64", "open_threshold_db int64", "close_threshold_db int64", "gate_open bool",
		}},
		{VoiceControlGateEventV4{}, []string{
			"schema string", "kind VoiceControlEventKindV4", "seq int64", "at_ms int64", "open bool", "why VoiceControlGateWhyV4",
		}},
		{VoiceControlCallQualityV4{}, []string{
			"schema string", "kind VoiceControlEventKindV4", "seq int64", "principal_id int64",
			"loss_basis_points,omitempty *int64", "jitter_us,omitempty *int64", "rtt_us,omitempty *int64",
		}},
	}
	for _, shape := range newShapes {
		if got := fields(shape.value); !reflect.DeepEqual(got, shape.want) {
			t.Errorf("%T has the fields %v, want %v", shape.value, got, shape.want)
		}
	}
	var sensitive struct {
		FakeSecrets []string `json:"fake_secrets"`
		Fragments   []string `json:"forbidden_field_fragments"`
	}
	if err := json.Unmarshal(readVoiceControlV3Fixture(t, "sensitive.json"), &sensitive); err != nil {
		t.Fatal(err)
	}
	shapes := make([]any, 0, len(voiceControlV4Types)+len(newShapes))
	for _, m := range voiceControlV4Types {
		shapes = append(shapes, m)
	}
	for _, shape := range newShapes {
		shapes = append(shapes, shape.value)
	}
	for _, m := range shapes {
		rt := reflect.TypeOf(m)
		for i := 0; i < rt.NumField(); i++ {
			name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			for _, frag := range sensitive.Fragments {
				if strings.Contains(name, frag) {
					t.Errorf("%T field %q is named like %q", m, name, frag)
				}
			}
		}
	}
	for _, f := range fixtureFilesUnder(t, voiceControlV4Dir) {
		data := readVoiceControlV4Fixture(t, f)
		for _, secret := range sensitive.FakeSecrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds a fake secret", f)
			}
		}
	}
}

// TestVoiceControlStartV4Validate is the destination rule, as a table.
func TestVoiceControlStartV4Validate(t *testing.T) {
	good := func(mod func(*VoiceControlStartV4)) VoiceControlStartV4 {
		s := VoiceControlStartV4{
			Schema: VoiceControlSchemaV4, Kind: VoiceControlCommandV4Start, ID: 1,
			Server: "https://conch.example.org", Channel: "general", PrincipalID: 7,
			Credential: VoiceControlCredentialStored,
		}
		mod(&s)
		return s
	}
	tests := []struct {
		name string
		mod  func(*VoiceControlStartV4)
		ok   bool
	}{
		{"channel", func(*VoiceControlStartV4) {}, true},
		{"call", func(s *VoiceControlStartV4) { s.Channel, s.Call = "", voiceControlV4CallID }, true},
		{"server-issued call", func(s *VoiceControlStartV4) { s.Channel, s.Call = "", NewPrivateCallID() }, true},
		{"both", func(s *VoiceControlStartV4) { s.Call = voiceControlV4CallID }, false},
		{"neither", func(s *VoiceControlStartV4) { s.Channel = "" }, false},
		{"call too short", func(s *VoiceControlStartV4) { s.Channel, s.Call = "", "abc" }, false},
		{"call with a slash", func(s *VoiceControlStartV4) { s.Channel, s.Call = "", "room/"+voiceControlV4CallID }, false},
		{"call is a default room name", func(s *VoiceControlStartV4) { s.Channel, s.Call = "", "conch-mfqwcylbmfqwcylbmfqwcylbme" }, false},
		{"call is a custom room name", func(s *VoiceControlStartV4) { s.Channel, s.Call = "", "alpha-2-mfqwcylbmfqwcylbmfqwcylbme" }, false},
		{"call is a bearer credential", func(s *VoiceControlStartV4) {
			s.Channel, s.Call = "", CredentialTokenPrefix+strings.Repeat("A", CredentialTokenLength-len(CredentialTokenPrefix))
		}, false},
		{"v3 schema", func(s *VoiceControlStartV4) { s.Schema = VoiceControlSchemaV3 }, false},
		{"no principal", func(s *VoiceControlStartV4) { s.PrincipalID = 0 }, false},
		{"bad credential", func(s *VoiceControlStartV4) { s.Credential = "" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := good(tt.mod)
			err := start.Validate()
			if (err == nil) != tt.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tt.ok)
			}
			line, err := MarshalVoiceControlLine(start)
			if (err == nil) != tt.ok || (len(line) != 0) != tt.ok {
				t.Errorf("MarshalVoiceControlLine() wrote %d bytes, error %v, want ok=%v", len(line), err, tt.ok)
			}
			if err != nil && start.Call != "" && strings.Contains(err.Error(), start.Call) {
				t.Error("the writer's error repeats the call value")
			}
		})
	}
}
