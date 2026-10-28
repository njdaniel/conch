package schema

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// voiceControlDir is the shared fixture directory of the managed-helper
// protocol, read by these tests and by the Rust helper's.
const voiceControlDir = "voice-control"

// voiceControlGolden is every single-message fixture and the type it is.
var voiceControlGolden = []struct {
	file  string
	event bool
	new   func() any
}{
	{file: "command-start.json", new: func() any { return new(VoiceControlStartV1) }},
	{file: "command-start-env.json", new: func() any { return new(VoiceControlStartV1) }},
	{file: "command-set-mute.json", new: func() any { return new(VoiceControlSetMuteV1) }},
	{file: "command-set-deafen.json", new: func() any { return new(VoiceControlSetDeafenV1) }},
	{file: "command-get-state.json", new: func() any { return new(VoiceControlGetStateV1) }},
	{file: "command-leave.json", new: func() any { return new(VoiceControlLeaveV1) }},
	{file: "event-hello.json", event: true, new: func() any { return new(VoiceControlHelloV1) }},
	{file: "event-ack.json", event: true, new: func() any { return new(VoiceControlAckV1) }},
	{file: "event-ack-refused.json", event: true, new: func() any { return new(VoiceControlAckV1) }},
	{file: "event-state-starting.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-connected.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-transmitting.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-muted-deafened.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-listen-only.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-no-microphone.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-no-talk-key.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-key-device-lost.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-reconnecting.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-reconnecting-backoff.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-reconnecting-sdk.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-stopping.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-stopped.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-stopped-owner-lost.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-failed-duplicate-identity.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-state-failed-identity-mismatch.json", event: true, new: func() any { return new(VoiceControlStateV1) }},
	{file: "event-protocol-error.json", event: true, new: func() any { return new(VoiceControlProtocolErrorV1) }},
}

func readVoiceControlFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", voiceControlDir, filepath.FromSlash(name))) // #nosec G304 -- a fixture name from this package's own tables
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestVoiceControlGoldenFixtures checks each single-message fixture three
// ways: it round-trips through its type byte for byte, its compact form is
// accepted by the reader of its direction, and MarshalVoiceControlLine writes
// exactly that compact form.
func TestVoiceControlGoldenFixtures(t *testing.T) {
	for _, tt := range voiceControlGolden {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, voiceControlDir+"/"+tt.file, tt.new)

			var compact bytes.Buffer
			if err := json.Compact(&compact, readVoiceControlFixture(t, tt.file)); err != nil {
				t.Fatal(err)
			}
			var msg VoiceControlMessage
			if tt.event {
				ev, err := ParseVoiceControlEvent(compact.Bytes())
				if err != nil {
					t.Fatalf("the owner's reader refuses it: %v", err)
				}
				msg = ev.message()
			} else {
				cmd, err := ParseVoiceControlCommand(compact.Bytes())
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

// message is the one message a command holds, for the tests.
func (c VoiceControlCommand) message() VoiceControlMessage {
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
	}
	return nil
}

// message is the one message an event holds, for the tests.
func (e VoiceControlEvent) message() VoiceControlMessage {
	switch {
	case e.Hello != nil:
		return e.Hello
	case e.Ack != nil:
		return e.Ack
	case e.State != nil:
		return e.State
	case e.ProtocolError != nil:
		return e.ProtocolError
	}
	return nil
}

// kind is the kind of the event held, or the unknown kind skipped.
func (e VoiceControlEvent) kind() VoiceControlEventKind {
	switch {
	case e.Hello != nil:
		return VoiceControlEventHello
	case e.Ack != nil:
		return VoiceControlEventAck
	case e.State != nil:
		return VoiceControlEventState
	case e.ProtocolError != nil:
		return VoiceControlEventProtocolError
	}
	return e.Unknown
}

type voiceControlCase struct {
	Name      string  `json:"name"`
	Direction string  `json:"direction"`
	Line      *string `json:"line"`
	LineHex   string  `json:"line_hex"`
	LineFile  string  `json:"line_file"`
	Want      struct {
		Result   string `json:"result"`
		Kind     string `json:"kind"`
		Code     string `json:"code"`
		ID       int64  `json:"id"`
		OffersV1 *bool  `json:"offers_v1"`
	} `json:"want"`
}

// voiceControlCaseFiles are the conformance documents of the v1 readers, read
// as one: cases.json is published and does not change
// (scripts/schema-compat.sh), so the cases pinned after it are in the files
// beside it.
var voiceControlCaseFiles = []string{"cases.json", "cases-strings.json"}

func loadVoiceControlCases(t *testing.T) []voiceControlCase {
	t.Helper()
	var cases []voiceControlCase
	for _, file := range voiceControlCaseFiles {
		cases = append(cases, loadVoiceControlCasesFrom(t, file)...)
	}
	return cases
}

// loadVoiceControlCasesFrom reads the cases of one document.
func loadVoiceControlCasesFrom(t *testing.T, file string) []voiceControlCase {
	t.Helper()
	var doc struct {
		Cases []voiceControlCase `json:"cases"`
	}
	dec := json.NewDecoder(bytes.NewReader(readVoiceControlFixture(t, file)))
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no cases")
	}
	return doc.Cases
}

// line is the case's line, from whichever of its three forms it has.
func (c voiceControlCase) line(t *testing.T) []byte {
	t.Helper()
	forms := 0
	var line []byte
	if c.Line != nil {
		forms++
		line = []byte(*c.Line)
	}
	if c.LineHex != "" {
		forms++
		b, err := hex.DecodeString(c.LineHex)
		if err != nil {
			t.Fatal(err)
		}
		line = b
	}
	if c.LineFile != "" {
		forms++
		line = readVoiceControlFixture(t, c.LineFile)
	}
	if forms != 1 {
		t.Fatalf("case %q has %d forms of its line, want 1", c.Name, forms)
	}
	return line
}

// TestVoiceControlCases runs the conformance cases the Rust helper runs too:
// each line is read by the reader of its direction, and accepted, refused
// with the code (and id) given, or skipped.
func TestVoiceControlCases(t *testing.T) {
	names := map[string]bool{}
	for _, c := range loadVoiceControlCases(t) {
		key := c.Direction + ": " + c.Name
		if names[key] {
			t.Errorf("case %q appears twice", key)
		}
		names[key] = true
		t.Run(key, func(t *testing.T) {
			line := c.line(t)
			var kind string
			var err error
			var offersV1 *bool
			switch c.Direction {
			case "command":
				var cmd VoiceControlCommand
				cmd, err = ParseVoiceControlCommand(line)
				kind = string(cmd.Kind())
			case "event":
				var ev VoiceControlEvent
				ev, err = ParseVoiceControlEvent(line)
				kind = string(ev.kind())
				if err == nil && ev.Unknown != "" {
					if c.Want.Result != "skip" {
						t.Fatalf("skipped as an unknown kind %q, want %s", ev.Unknown, c.Want.Result)
					}
				}
				if ev.Hello != nil {
					offers := ev.Hello.Offers(VoiceControlSchemaV1)
					offersV1 = &offers
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
				if c.Want.OffersV1 != nil && (offersV1 == nil || *offersV1 != *c.Want.OffersV1) {
					t.Errorf("offers v1: %v, want %v", offersV1, *c.Want.OffersV1)
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

// TestVoiceControlCasesCoverEveryCode makes sure each code a reader can
// return is shown by at least one case, so that the Rust side is held to it.
func TestVoiceControlCasesCoverEveryCode(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range loadVoiceControlCases(t) {
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

type voiceControlStep struct {
	From    string          `json:"from"`
	Message json.RawMessage `json:"message"`
	Close   string          `json:"close"`
	Signal  string          `json:"signal"`
	Exit    *int            `json:"exit"`
}

// TestVoiceControlTranscripts checks every transcript against the ordering
// rules of the design note (§5): the hello first, seq increasing, start
// first, ids increasing, one ack per command in order, each state a command
// changed written before its ack, a protocol_error and a failed state after
// a line that cannot be acted on, nothing after the final state, and the exit
// code that state implies.
func TestVoiceControlTranscripts(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", voiceControlDir, "transcript-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("found %d transcripts", len(files))
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			var doc struct {
				Description string             `json:"description"`
				Steps       []voiceControlStep `json:"steps"`
			}
			if err := json.Unmarshal(readVoiceControlFixture(t, filepath.Base(file)), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Description == "" {
				t.Error("no description")
			}
			checkVoiceControlTranscript(t, doc.Steps)
		})
	}
}

// pendingCommand is a command with a usable id that has not been answered,
// and the answer it must get when that is already known.
type pendingCommand struct {
	id      int64
	cmd     VoiceControlCommand
	refusal VoiceControlErrorCode
}

func checkVoiceControlTranscript(t *testing.T, steps []voiceControlStep) {
	t.Helper()
	var (
		seq, lastID       int64
		hello             *VoiceControlHelloV1
		started, final    bool
		finalPhase        VoiceControlPhase
		state             *VoiceControlStateV1
		previousWasState  bool
		pending           []pendingCommand
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
			if hello == nil || !hello.Offers(VoiceControlSchemaV1) {
				t.Fatalf("step %d: the owner writes before a hello offering v1", i)
			}
			var line bytes.Buffer
			if err := json.Compact(&line, step.Message); err != nil {
				t.Fatal(err)
			}
			cmd, err := ParseVoiceControlCommand(line.Bytes())
			var vcErr *VoiceControlError
			if err != nil && !errors.As(err, &vcErr) {
				t.Fatalf("step %d: %v", i, err)
			}
			id := cmd.ID()
			if vcErr != nil {
				id = vcErr.ID
			}
			switch {
			case !started && cmd.Start == nil:
				// Before start, a refused line is reported with its own code
				// when that code ends a session, and anything else that is
				// not a valid start as not_started.
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
				p := pendingCommand{id: id, cmd: cmd}
				switch {
				case vcErr != nil:
					p.refusal = vcErr.Code
				case cmd.Start != nil && started:
					p.refusal = VoiceControlErrorAlreadyStarted
				case cmd.Start != nil:
					started = true
				case (cmd.SetMute != nil || cmd.SetDeafen != nil) && state != nil && state.Phase == VoiceControlPhaseStopping:
					p.refusal = VoiceControlErrorStopping
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
		ev, err := ParseVoiceControlEvent(line.Bytes())
		if err != nil {
			t.Fatalf("step %d: the owner's reader refuses it: %v", i, err)
		}
		if ev.Seq <= seq {
			t.Fatalf("step %d: seq %d after %d", i, ev.Seq, seq)
		}
		seq = ev.Seq
		if hello == nil && ev.Hello == nil {
			t.Fatalf("step %d: the first event is not a hello", i)
		}
		if wantFailedAfterPE && (ev.State == nil || ev.State.Phase != VoiceControlPhaseFailed || ev.State.Reason != VoiceControlReasonProtocolError) {
			t.Fatalf("step %d: a protocol_error is not followed by failed with reason protocol_error", i)
		}
		wasState := previousWasState
		previousWasState = ev.State != nil
		switch {
		case ev.Hello != nil:
			if hello != nil {
				t.Fatalf("step %d: a second hello", i)
			}
			hello = ev.Hello
		case ev.ProtocolError != nil:
			if wantProtocolError == "" || ev.ProtocolError.Code != wantProtocolError {
				t.Fatalf("step %d: protocol_error %s, want %q", i, ev.ProtocolError.Code, wantProtocolError)
			}
			wantProtocolError, wantFailedAfterPE = "", true
		case ev.State != nil:
			if wantProtocolError != "" {
				t.Fatalf("step %d: a state where a protocol_error %s was due", i, wantProtocolError)
			}
			if !started && !wantFailedAfterPE {
				t.Fatalf("step %d: a state before start was accepted", i)
			}
			wantFailedAfterPE = false
			state = ev.State
			if state.Phase.Final() {
				final, finalPhase = true, state.Phase
			}
		case ev.Ack != nil:
			if len(pending) == 0 || pending[0].id != ev.Ack.ID {
				t.Fatalf("step %d: ack for %d, the next unanswered command is %v", i, ev.Ack.ID, pending)
			}
			p := pending[0]
			pending = pending[1:]
			if p.refusal != "" {
				if ev.Ack.OK || ev.Ack.Error != p.refusal {
					t.Errorf("step %d: command %d answered ok=%v %q, want refused with %s", i, p.id, ev.Ack.OK, ev.Ack.Error, p.refusal)
				}
				continue
			}
			if !ev.Ack.OK {
				t.Errorf("step %d: command %d refused with %s for no reason a transcript can see", i, p.id, ev.Ack.Error)
				continue
			}
			checkVoiceControlEffect(t, i, p.cmd, state, wasState)
		}
	}
	t.Fatal("the transcript does not end with the helper's exit")
}

// checkVoiceControlEffect checks that the state a command changed was written
// before its ack: the latest state already shows the command's effect.
func checkVoiceControlEffect(t *testing.T, step int, cmd VoiceControlCommand, state *VoiceControlStateV1, previousWasState bool) {
	t.Helper()
	switch {
	case state == nil:
		t.Errorf("step %d: %s acknowledged before any state", step, cmd.Kind())
	case cmd.SetMute != nil && state.Muted != cmd.SetMute.Muted:
		t.Errorf("step %d: set_mute %v acknowledged while the latest state has muted %v", step, cmd.SetMute.Muted, state.Muted)
	case cmd.SetDeafen != nil && state.Deafened != cmd.SetDeafen.Deafened:
		t.Errorf("step %d: set_deafen %v acknowledged while the latest state has deafened %v", step, cmd.SetDeafen.Deafened, state.Deafened)
	case cmd.Leave != nil && state.Phase != VoiceControlPhaseStopping && !state.Phase.Final():
		t.Errorf("step %d: leave acknowledged in phase %s", step, state.Phase)
	case cmd.GetState != nil && !previousWasState:
		t.Errorf("step %d: get_state acknowledged without a state just before", step)
	}
}

// TestVoiceControlLoneSurrogateEscape pins the scan both readers apply to a
// line's strings (#690): the same table as the Rust side's
// a_lone_surrogate_escape_is_found_wherever_it_is_and_a_pair_is_not.
func TestVoiceControlLoneSurrogateEscape(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{`{"a":"plain"}`, false},
		{`{"a":"\ud800"}`, true},
		{`{"a":"\uD800"}`, true},
		{`{"a":"\udc00"}`, true},
		{`{"a":"\udfff"}`, true},
		{`{"a":"x\ud800"}`, true},
		{`{"a":"\ud800A"}`, true},
		{`{"a":"\ud800\\"}`, true},
		{`{"a":"😀"}`, false},
		{`{"a":"😀A"}`, false},
		{`{"a":"\\ud800"}`, false},
		{`{"a":"\"\ud800"}`, true},
		{`{"\ud800":1}`, true},
		{`{"a":"� é \u001b"}`, false},
		{`{"a":"\ud800","b":"\udc00"}`, true},
		{`{"\ud800":"\udc00"}`, true},
		{`{"a":"\ud800\ud800"}`, true},
		{`"\`, false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := hasLoneSurrogateEscape([]byte(tt.text)); got != tt.want {
				t.Errorf("hasLoneSurrogateEscape(%s) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestVoiceControlFixturesAreAllClaimed fails on a file in the fixture
// directory that no test reads, so nothing there goes unchecked.
func TestVoiceControlFixturesAreAllClaimed(t *testing.T) {
	claimed := map[string]bool{"sensitive.json": true}
	for _, file := range voiceControlCaseFiles {
		claimed[file] = true
	}
	for _, g := range voiceControlGolden {
		claimed[g.file] = true
	}
	for _, c := range loadVoiceControlCases(t) {
		if c.LineFile != "" {
			claimed[c.LineFile] = true
		}
	}
	for _, f := range voiceControlFixtureFiles(t) {
		if strings.HasPrefix(f, "transcript-") && strings.HasSuffix(f, ".json") {
			continue
		}
		if !claimed[f] {
			t.Errorf("testdata/%s/%s is read by no test", voiceControlDir, f)
		}
	}
}

// voiceControlFixtureFiles lists every file under the fixture directory, by
// its path relative to it, with forward slashes.
func voiceControlFixtureFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("testdata", voiceControlDir)
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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

// voiceControlTypes is every message type of the protocol.
var voiceControlTypes = []VoiceControlMessage{
	VoiceControlStartV1{}, VoiceControlSetMuteV1{}, VoiceControlSetDeafenV1{}, VoiceControlGetStateV1{}, VoiceControlLeaveV1{},
	VoiceControlHelloV1{}, VoiceControlAckV1{}, VoiceControlStateV1{}, VoiceControlProtocolErrorV1{},
}

// TestVoiceControlCarriesNoSecrets holds the protocol to sensitive.json: no
// field of any message is named like a credential or a room; no event field
// can carry free text (every string is a schema name, a closed vocabulary or
// the checked helper_version); and no fixture holds a fake secret, except the
// one case that shows an unknown field named token being ignored.
func TestVoiceControlCarriesNoSecrets(t *testing.T) {
	var sensitive struct {
		FakeSecrets []string `json:"fake_secrets"`
		Fragments   []string `json:"forbidden_field_fragments"`
	}
	if err := json.Unmarshal(readVoiceControlFixture(t, "sensitive.json"), &sensitive); err != nil {
		t.Fatal(err)
	}
	if len(sensitive.FakeSecrets) == 0 || len(sensitive.Fragments) == 0 {
		t.Fatal("sensitive.json lists nothing")
	}

	for _, msg := range voiceControlTypes {
		typ := reflect.TypeOf(msg)
		event := strings.HasPrefix(typ.Name(), "VoiceControlHello") || strings.HasPrefix(typ.Name(), "VoiceControlAck") ||
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
			free := map[string]bool{"Schema": true, "Versions": true, "HelperVersion": true}
			if !vocabulary && !free[f.Name] {
				t.Errorf("%s.%s can carry free text; an event's strings must be vocabularies", typ.Name(), f.Name)
			}
		}
	}

	exempt := "start with a field it does not declare, named token: ignored, and never repeated"
	for _, name := range voiceControlFixtureFiles(t) {
		if name == "sensitive.json" {
			continue
		}
		data := readVoiceControlFixture(t, name)
		if slices.Contains(voiceControlCaseFiles, name) {
			for _, c := range loadVoiceControlCasesFrom(t, name) {
				if c.Name == exempt {
					continue
				}
				line := c.line(t)
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

// The fields of each message are an exact list. A new field has to be added
// here on purpose, by someone who has asked whether it may cross the control
// channel and whether it could carry a secret or text a terminal would act on.
func TestVoiceControlFieldsAreAnExactList(t *testing.T) {
	head := []string{"schema", "kind"}
	want := map[string][]string{
		"VoiceControlStartV1":         append(slices.Clone(head), "id", "server", "channel", "principal_id", "credential", "muted", "deafened"),
		"VoiceControlSetMuteV1":       append(slices.Clone(head), "id", "muted"),
		"VoiceControlSetDeafenV1":     append(slices.Clone(head), "id", "deafened"),
		"VoiceControlGetStateV1":      append(slices.Clone(head), "id"),
		"VoiceControlLeaveV1":         append(slices.Clone(head), "id"),
		"VoiceControlHelloV1":         append(slices.Clone(head), "seq", "versions", "helper_version"),
		"VoiceControlAckV1":           append(slices.Clone(head), "seq", "id", "ok", "error"),
		"VoiceControlStateV1":         append(slices.Clone(head), "seq", "phase", "reason", "retry_in_ms", "capture", "transmitting", "muted", "deafened", "blocked", "ptt", "ptt_problem"),
		"VoiceControlProtocolErrorV1": append(slices.Clone(head), "seq", "code"),
	}
	for _, msg := range voiceControlTypes {
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

// TestVoiceControlErrorCodeClasses pins which codes end the session and which
// an ack carries: every code is one or the other, and only invalid is both
// (fatal when the line has no id to answer).
func TestVoiceControlErrorCodeClasses(t *testing.T) {
	for _, code := range Vocabularies["VoiceControlErrorCode"] {
		c := VoiceControlErrorCode(code)
		if !c.Fatal() && !c.AckRefusal() {
			t.Errorf("%s is neither fatal nor an ack refusal", c)
		}
		if c.Fatal() && c.AckRefusal() && c != VoiceControlErrorInvalid {
			t.Errorf("%s is both fatal and an ack refusal", c)
		}
	}
	if VoiceControlErrorCode("").Fatal() || VoiceControlErrorCode("").AckRefusal() {
		t.Error("the empty code belongs to a class")
	}
}

func TestMarshalVoiceControlLine(t *testing.T) {
	t.Run("the longest start fits on a line", func(t *testing.T) {
		server := "https://example.org/" + strings.Repeat("a", VoiceControlMaxServerBytes-len("https://example.org/"))
		start := VoiceControlStartV1{
			Schema: VoiceControlSchemaV1, Kind: VoiceControlCommandStart, ID: VoiceControlMaxID,
			Server: server, Channel: strings.Repeat(`"`, VoiceControlMaxChannelBytes),
			PrincipalID: 1 << 62, Credential: VoiceControlCredentialStored,
		}
		line, err := MarshalVoiceControlLine(start)
		if err != nil {
			t.Fatal(err)
		}
		if len(line)-1 > VoiceControlMaxLineBytes {
			t.Fatalf("%d bytes", len(line)-1)
		}
		cmd, err := ParseVoiceControlCommand(bytes.TrimSuffix(line, []byte{'\n'}))
		if err != nil || !reflect.DeepEqual(*cmd.Start, start) {
			t.Fatalf("read back as %+v, %v", cmd.Start, err)
		}
	})
	t.Run("one line feed, at the end", func(t *testing.T) {
		line, err := MarshalVoiceControlLine(VoiceControlStartV1{
			Schema: VoiceControlSchemaV1, Kind: VoiceControlCommandStart, ID: 1,
			Server: "https://example.org", Channel: "a<b>&c", PrincipalID: 3, Credential: VoiceControlCredentialEnv,
		})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(line, []byte{'\n'}) != 1 || line[len(line)-1] != '\n' {
			t.Fatalf("%q", line)
		}
		if !bytes.Contains(line, []byte(`"a<b>&c"`)) {
			t.Errorf("HTML characters escaped: %s", line)
		}
	})
	t.Run("an invalid message is not written", func(t *testing.T) {
		for _, msg := range []VoiceControlMessage{
			VoiceControlSetMuteV1{Schema: VoiceControlSchemaV1, Kind: VoiceControlCommandSetMute},
			VoiceControlSetMuteV1{Schema: "conch.voice_control.v2", Kind: VoiceControlCommandSetMute, ID: 1},
			VoiceControlSetMuteV1{Schema: VoiceControlSchemaV1, Kind: VoiceControlCommandLeave, ID: 1},
			VoiceControlStateV1{Schema: VoiceControlSchemaV1, Kind: VoiceControlEventState, Seq: 1, Phase: VoiceControlPhaseConnected, Capture: VoiceControlCaptureOn, PTT: VoiceControlPTTReady},
		} {
			if line, err := MarshalVoiceControlLine(msg); err == nil {
				t.Errorf("wrote %s", line)
			}
		}
	})
}

func TestVoiceControlCommandAccessors(t *testing.T) {
	if (VoiceControlCommand{}).Kind() != "" || (VoiceControlCommand{}).ID() != 0 {
		t.Error("an empty command has a kind or an id")
	}
	for _, c := range []VoiceControlCommand{
		{Start: &VoiceControlStartV1{ID: 1}},
		{SetMute: &VoiceControlSetMuteV1{ID: 2}},
		{SetDeafen: &VoiceControlSetDeafenV1{ID: 3}},
		{GetState: &VoiceControlGetStateV1{ID: 4}},
		{Leave: &VoiceControlLeaveV1{ID: 5}},
	} {
		if !c.Kind().Valid() || c.ID() == 0 {
			t.Errorf("%+v: kind %q, id %d", c, c.Kind(), c.ID())
		}
	}
}

// voiceControlDoc is the design note this protocol is specified in.
var voiceControlDoc = filepath.Join("..", "..", "docs", "design", "tui-voice.md")

// TestVoiceControlDoc keeps the design note and this package together: every
// relative link in it leads to a file, every fixture is named in it, and
// every number of the contract is written in its table as the code has it.
func TestVoiceControlDoc(t *testing.T) {
	data, err := os.ReadFile(voiceControlDoc) // #nosec G304 -- this repository's own design note, by a fixed path
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)

	link := regexp.MustCompile(`\]\(([^)#\s]+)(#[^)]*)?\)`)
	for _, m := range link.FindAllStringSubmatch(doc, -1) {
		target := m[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		// #nosec G703 -- a link in this repository's own design note, only looked at
		if _, err := os.Stat(filepath.Join(filepath.Dir(voiceControlDoc), filepath.FromSlash(target))); err != nil {
			t.Errorf("the link to %s leads nowhere: %v", target, err)
		}
	}

	for _, f := range voiceControlFixtureFiles(t) {
		if !strings.Contains(doc, f) {
			t.Errorf("the design note does not name the fixture %s", f)
		}
	}

	numbers := map[string]int64{
		"VoiceControlEventFD":               VoiceControlEventFD,
		"VoiceControlMaxLineBytes":          VoiceControlMaxLineBytes,
		"VoiceControlMaxID":                 VoiceControlMaxID,
		"VoiceControlMaxInFlight":           VoiceControlMaxInFlight,
		"VoiceControlEventQueueLines":       VoiceControlEventQueueLines,
		"VoiceControlMaxVersions":           VoiceControlMaxVersions,
		"VoiceControlMaxHelperVersionBytes": VoiceControlMaxHelperVersionBytes,
		"VoiceControlMaxServerBytes":        VoiceControlMaxServerBytes,
		"VoiceControlMaxChannelBytes":       VoiceControlMaxChannelBytes,
		"VoiceControlMaxRetryInMS":          VoiceControlMaxRetryInMS,
		"VoiceControlHelloTimeoutMS":        VoiceControlHelloTimeoutMS,
		"VoiceControlStartTimeoutMS":        VoiceControlStartTimeoutMS,
		"VoiceControlAckTimeoutMS":          VoiceControlAckTimeoutMS,
		"VoiceControlOwnerPollMS":           VoiceControlOwnerPollMS,
		"VoiceControlGateShutMS":            VoiceControlGateShutMS,
		"VoiceControlCaptureStopMS":         VoiceControlCaptureStopMS,
		"VoiceControlOwnerLossExitMS":       VoiceControlOwnerLossExitMS,
		"VoiceControlLeaveExitMS":           VoiceControlLeaveExitMS,
		"VoiceControlStopTermAfterMS":       VoiceControlStopTermAfterMS,
		"VoiceControlStopKillAfterMS":       VoiceControlStopKillAfterMS,
	}
	for name, value := range numbers {
		row := "| `" + name + "` | " + strconv.FormatInt(value, 10) + " |"
		if !strings.Contains(doc, row) {
			t.Errorf("the design note's table has no row %q", row)
		}
	}
	if !strings.Contains(doc, "`"+VoiceControlSchemaV1+"`") || !strings.Contains(doc, "`"+VoiceControlSubcommand+"`") {
		t.Error("the design note does not name the schema or the subcommand")
	}
}
