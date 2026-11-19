package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// voiceControlV4Dir is the fixture directory of v4 (issue #2039). v4 is v3
// plus the start's destination; v3's own tests and fixtures are untouched.
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
// Besides the start, they are v3's, with v4's schema name.
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
		{file: voiceControlV4Hello, event: true, v1: true, new: func() any { return new(VoiceControlHelloV1) }},
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
	}
	return nil
}

func (e VoiceControlEventV4) kind() VoiceControlEventKind {
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

// TestVoiceControlV4GoldenFixturesAreV3s pins "everything else carried over
// unchanged" on the fixtures: every v3 single-message fixture but the start
// has a v4 twin that is the same bytes with the schema name changed, and the
// v4 set holds nothing else but the two starts and the hello.
func TestVoiceControlV4GoldenFixturesAreV3s(t *testing.T) {
	twins := 0
	for _, g := range voiceControlV3Golden {
		if g.file == "command-start.json" || g.v1 {
			continue
		}
		twins++
		want := strings.ReplaceAll(string(readVoiceControlV3Fixture(t, g.file)), VoiceControlSchemaV3, VoiceControlSchemaV4)
		if got := string(readVoiceControlV4Fixture(t, g.file)); got != want {
			t.Errorf("%s differs from its v3 fixture beyond the schema name", g.file)
		}
	}
	if len(voiceControlV4Golden) != twins+3 {
		t.Errorf("%d golden fixtures, want the %d twins and the three of v4's own", len(voiceControlV4Golden), twins)
	}
	start := strings.ReplaceAll(string(readVoiceControlV3Fixture(t, "command-start.json")), VoiceControlSchemaV3, VoiceControlSchemaV4)
	if string(readVoiceControlV4Fixture(t, "command-start-channel.json")) != start {
		t.Error("command-start-channel.json is not v3's start with the schema name changed")
	}
}

func loadVoiceControlV4Cases(t *testing.T) []voiceControlCase {
	t.Helper()
	var doc struct {
		Cases []voiceControlCase `json:"cases"`
	}
	if err := json.Unmarshal(readVoiceControlV4Fixture(t, "cases.json"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no cases")
	}
	return doc.Cases
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

// TestVoiceControlV4Cases runs the destination cases.
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
// readers, written under v4's name, with the result the v3 readers gave it.
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

// TestVoiceControlCommandKindV4 pins the kinds: v3's nine and no others.
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
// session is in.
var voiceControlV4Transcripts = map[string]string{
	"transcript-negotiation-v4-owner-channel.json": VoiceControlSchemaV4,
	"transcript-negotiation-v4-owner-call.json":    VoiceControlSchemaV4,
	"transcript-negotiation-v3-owner.json":         VoiceControlSchemaV3,
	"transcript-version-fixed.json":                VoiceControlSchemaV4,
}

// TestVoiceControlV4Transcripts reads every line of every transcript with the
// reader of its direction and the session's version. A v4 session that has
// nothing to do with a call (every line but the start's destination) is then
// run through v3's ordering rules, written as v3: v4 changes no rule of the
// session, so the rules of v3 are the rules of v4, kept once. The v3 owner's
// session is v3's and is run as it is.
func TestVoiceControlV4Transcripts(t *testing.T) {
	for name, version := range voiceControlV4Transcripts {
		t.Run(name, func(t *testing.T) {
			var doc struct {
				Description string             `json:"description"`
				Steps       []voiceControlStep `json:"steps"`
			}
			if err := json.Unmarshal(readVoiceControlV4Fixture(t, name), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Description == "" {
				t.Error("no description")
			}
			callStarts := 0
			asV3 := make([]voiceControlStep, len(doc.Steps))
			for i, s := range doc.Steps {
				asV3[i] = s
				if len(s.Message) == 0 {
					continue
				}
				line := compactVoiceControlFixture(t, s.Message)
				var f map[string]json.RawMessage
				if err := json.Unmarshal(line, &f); err != nil {
					t.Fatal(err)
				}
				schema, _ := fieldString(f, "schema")
				switch {
				case s.From == "helper" && schema == VoiceControlSchemaV1:
					if _, err := ParseVoiceControlEvent(line); err != nil {
						t.Errorf("step %d: %v", i, err)
					}
				case s.From == "helper" && schema == version:
					read := func() error { _, err := ParseVoiceControlEventV4(line); return err }
					if version == VoiceControlSchemaV3 {
						read = func() error { _, err := ParseVoiceControlEventV3(line); return err }
					}
					if err := read(); err != nil {
						t.Errorf("step %d: %v", i, err)
					}
				case s.From == "owner" && schema == version:
					read := func() error { _, err := ParseVoiceControlCommandV4(line); return err }
					if version == VoiceControlSchemaV3 {
						read = func() error { _, err := ParseVoiceControlCommandV3(line); return err }
					}
					if err := read(); err != nil {
						t.Errorf("step %d: %v", i, err)
					}
				case s.From == "owner":
					// A line of another version in the session: refused.
					if _, err := ParseVoiceControlCommandV4(line); err == nil {
						t.Errorf("step %d: a line of %s was accepted in a session of %s", i, schema, version)
					}
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
			if strings.Contains(name, "version-fixed") {
				// The v3 line in a v4 session is the point; it has no
				// v3 reading to run.
				return
			}
			if got := checkVoiceControlV3Transcript(t, asV3); got != VoiceControlSchemaV3 {
				t.Errorf("the session read as v3 is in %q", got)
			}
		})
	}
	files, err := filepath.Glob(filepath.Join("testdata", voiceControlV4Dir, "transcript-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(voiceControlV4Transcripts) {
		t.Errorf("%d transcripts on disk, %d listed", len(files), len(voiceControlV4Transcripts))
	}
}

// TestVoiceControlV4CallSessionHasNoShare: a call has no screen share, so a
// share's sound is refused into the state with the problem a session with no
// companion gets, with no word v3 does not have.
func TestVoiceControlV4CallSessionHasNoShare(t *testing.T) {
	st := VoiceControlStateV4{
		Schema: VoiceControlSchemaV4, Kind: VoiceControlEventState, Seq: 5,
		Phase: VoiceControlPhaseConnected, Capture: VoiceControlCaptureOn, Playback: VoiceControlPlaybackOn,
		Blocked: []VoiceControlBlock{}, TalkMode: VoiceControlTalkModePushToTalk, PTT: VoiceControlPTTReady,
		ShareAudio: VoiceControlShareAudioUnavailable, ShareAudioProblem: VoiceControlShareAudioProblemUnsupported,
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("a call session's refusal of a share's sound is invalid: %v", err)
	}
}

// TestVoiceControlV4FixturesAreAllClaimed: every file under the v4 directory
// is read by a test.
func TestVoiceControlV4FixturesAreAllClaimed(t *testing.T) {
	claimed := map[string]bool{"cases.json": true}
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

// voiceControlV4Types is every message type of v4.
var voiceControlV4Types = []VoiceControlMessage{
	VoiceControlStartV4{}, VoiceControlSetMuteV4{}, VoiceControlGetStateV4{}, VoiceControlLeaveV4{}, VoiceControlSetDeafenV4{},
	VoiceControlSetDevicesV4{}, VoiceControlSetTalkModeV4{},
	VoiceControlSetShareAudioV4{}, VoiceControlSetShareAudioMuteV4{},
	VoiceControlAckV4{}, VoiceControlStateV4{}, VoiceControlProtocolErrorV4{},
}

// TestVoiceControlV4FieldsAreV3sPlusTheDestination: each v4 message has the
// JSON fields of its v3 twin, by name, type and order, except the start,
// which has "call" beside "channel" and a channel that may be absent. No field
// of any v4 message is named like a credential or a room, and no fake secret
// is in any fixture.
func TestVoiceControlV4FieldsAreV3sPlusTheDestination(t *testing.T) {
	v3 := []any{
		VoiceControlStartV3{}, VoiceControlSetMuteV3{}, VoiceControlGetStateV3{}, VoiceControlLeaveV3{}, VoiceControlSetDeafenV3{},
		VoiceControlSetDevicesV3{}, VoiceControlSetTalkModeV3{},
		VoiceControlSetShareAudioV3{}, VoiceControlSetShareAudioMuteV3{},
		VoiceControlAckV3{}, VoiceControlStateV3{}, VoiceControlProtocolErrorV3{},
	}
	fields := func(v any) (names []string) {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			typ := strings.ReplaceAll(rt.Field(i).Type.String(), "V3", "V4")
			names = append(names, rt.Field(i).Tag.Get("json")+" "+typ)
		}
		return names
	}
	for i, m := range voiceControlV4Types {
		got, want := fields(m), fields(v3[i])
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
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%T has the fields %v, want %v", m, got, want)
		}
	}
	var sensitive struct {
		FakeSecrets []string `json:"fake_secrets"`
		Fragments   []string `json:"forbidden_field_fragments"`
	}
	if err := json.Unmarshal(readVoiceControlV3Fixture(t, "sensitive.json"), &sensitive); err != nil {
		t.Fatal(err)
	}
	for _, m := range voiceControlV4Types {
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
		{"both", func(s *VoiceControlStartV4) { s.Call = voiceControlV4CallID }, false},
		{"neither", func(s *VoiceControlStartV4) { s.Channel = "" }, false},
		{"call too short", func(s *VoiceControlStartV4) { s.Channel, s.Call = "", "abc" }, false},
		{"call with a slash", func(s *VoiceControlStartV4) { s.Channel, s.Call = "", "room/"+voiceControlV4CallID }, false},
		{"v3 schema", func(s *VoiceControlStartV4) { s.Schema = VoiceControlSchemaV3 }, false},
		{"no principal", func(s *VoiceControlStartV4) { s.PrincipalID = 0 }, false},
		{"bad credential", func(s *VoiceControlStartV4) { s.Credential = "" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := good(tt.mod).Validate()
			if (err == nil) != tt.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && strings.Contains(err.Error(), voiceControlV4CallID) {
				t.Errorf("the error repeats the call id: %v", err)
			}
		})
	}
}
