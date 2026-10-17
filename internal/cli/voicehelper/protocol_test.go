package voicehelper

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, filepath.FromSlash(name))) // #nosec G304 -- a fixture of the shared directory
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLineReader(t *testing.T) {
	limit := schema.VoiceControlMaxLineBytes
	type line struct {
		text    string
		tooLong bool
	}
	tests := []struct {
		name  string
		input string
		want  []line
		// tail is the partial line returned with the end of input.
		tail string
	}{
		{name: "lines", input: "a\nbc\n\n", want: []line{{text: "a"}, {text: "bc"}, {text: ""}}},
		{name: "a carriage return is part of the line", input: "a\r\n", want: []line{{text: "a\r"}}},
		{name: "exactly the limit", input: strings.Repeat("x", limit) + "\nnext\n", want: []line{{text: strings.Repeat("x", limit)}, {text: "next"}}},
		{
			name:  "one byte over: its head, and the rest discarded up to the line feed",
			input: strings.Repeat("y", limit+1) + "\nnext\n",
			want:  []line{{text: strings.Repeat("y", limit), tooLong: true}, {text: "next"}},
		},
		{
			name:  "far over",
			input: strings.Repeat("z", 5*limit) + "\nnext\n",
			want:  []line{{text: strings.Repeat("z", limit), tooLong: true}, {text: "next"}},
		},
		{name: "a partial line at the end", input: "a\npartial", want: []line{{text: "a"}}, tail: "partial"},
		{
			name:  "a long partial line at the end: refused at the bound, the rest discarded",
			input: strings.Repeat("p", 2*limit),
			want:  []line{{text: strings.Repeat("p", limit), tooLong: true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lr := newLineReader(strings.NewReader(tt.input))
			var got []line
			for {
				text, tooLong, err := lr.next()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						t.Fatal(err)
					}
					if string(text) != tt.tail {
						t.Errorf("tail %d bytes, want %d", len(text), len(tt.tail))
					}
					break
				}
				got = append(got, line{text: string(text), tooLong: tooLong})
			}
			if len(got) != len(tt.want) {
				t.Fatalf("%d lines, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("line %d: %d bytes (too long %v), want %d (%v)", i, len(got[i].text), got[i].tooLong, len(tt.want[i].text), tt.want[i].tooLong)
				}
			}
		})
	}
}

// TestLineReaderUnendingLine: a line past the bound is reported at the bound,
// while the writer still holds it open and no line feed has come.
func TestLineReaderUnendingLine(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = r.Close() }()
	go func() {
		_, _ = w.Write([]byte(strings.Repeat("u", schema.VoiceControlMaxLineBytes+100)))
		// Then nothing: no line feed, and the pipe left open until the test
		// ends.
	}()
	got := make(chan bool, 1)
	go func() {
		_, tooLong, err := newLineReader(r).next()
		got <- tooLong && err == nil
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the line was not reported as too long")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reader waited for a line feed that never comes")
	}
}

// seededTracker has read a hello and written a start, so that any event may
// come next.
func seededTracker(t *testing.T) *tracker {
	t.Helper()
	tr := &tracker{}
	if _, err := tr.read(readCompact(t, "event-hello.json"), false); err != nil {
		t.Fatal(err)
	}
	tr.written(1, schema.VoiceControlCommandStart)
	tr.lastSeq = 0
	return tr
}

func readCompact(t *testing.T, name string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, readFixture(t, name)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestEventCases runs every event case of cases.json through the owner's
// reading: the bounded line reader, schema.ParseVoiceControlEvent and the
// tracker. Accepted events are accepted, skipped ones skipped, and refused
// ones are a bad event naming the case's code.
func TestEventCases(t *testing.T) {
	var doc struct {
		Cases []struct {
			Name      string  `json:"name"`
			Direction string  `json:"direction"`
			Line      *string `json:"line"`
			LineHex   string  `json:"line_hex"`
			LineFile  string  `json:"line_file"`
			Want      struct {
				Result   string `json:"result"`
				Kind     string `json:"kind"`
				Code     string `json:"code"`
				OffersV1 *bool  `json:"offers_v1"`
			} `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(readFixture(t, "cases.json"), &doc); err != nil {
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
				raw = readFixture(t, c.LineFile)
			}
			lr := newLineReader(bytes.NewReader(append(raw, '\n')))
			line, tooLong, err := lr.next()
			if err != nil {
				t.Fatal(err)
			}
			// A hello is read first; anything else after a hello and a
			// start, with an ack's command the one written.
			tr := seededTracker(t)
			if ev, perr := schema.ParseVoiceControlEvent(line); perr == nil {
				switch {
				case ev.Hello != nil:
					tr = &tracker{}
				case ev.Ack != nil:
					tr.pending = []pendingCommand{{id: ev.Ack.ID, kind: schema.VoiceControlCommandSetMute}}
				}
			}
			rd, err := tr.read(line, tooLong)
			switch c.Want.Result {
			case "accept", "skip":
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				kind := string(rd.ev.Unknown)
				switch {
				case rd.ev.Hello != nil:
					kind = "hello"
				case rd.ev.Ack != nil:
					kind = "ack"
				case rd.ev.State != nil:
					kind = "state"
				case rd.ev.ProtocolError != nil:
					kind = "protocol_error"
				}
				if kind != c.Want.Kind || (c.Want.Result == "skip") != (rd.ev.Unknown != "") {
					t.Errorf("read as %q (unknown %q), want %s %q", kind, rd.ev.Unknown, c.Want.Result, c.Want.Kind)
				}
				if c.Want.OffersV1 != nil && rd.ev.Hello.Offers(schema.VoiceControlSchemaV1) != *c.Want.OffersV1 {
					t.Errorf("offers v1: want %v", *c.Want.OffersV1)
				}
			case "refuse":
				var bad *badEventError
				if !errors.As(err, &bad) || !strings.Contains(bad.detail, c.Want.Code) {
					t.Fatalf("%v, want a bad event with %s", err, c.Want.Code)
				}
				if len(line) > 0 && strings.Contains(err.Error(), string(line)) {
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

// TestTranscriptsOwnerSide reads every transcript as the owner does: each
// command it shows as written, each event through the tracker. Every event
// must be accepted, every ack must answer the next command, and the gap of
// the resync transcript must be seen.
func TestTranscriptsOwnerSide(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixtureDir, "transcript-*.json"))
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
			if err := json.Unmarshal(readFixture(t, filepath.Base(file)), &doc); err != nil {
				t.Fatal(err)
			}
			tr := &tracker{}
			gaps := 0
			for i, step := range doc.Steps {
				if step.Message == nil {
					continue
				}
				var line bytes.Buffer
				if err := json.Compact(&line, step.Message); err != nil {
					t.Fatal(err)
				}
				if step.From == "owner" {
					cmd, err := schema.ParseVoiceControlCommand(line.Bytes())
					id, kind := cmd.ID(), cmd.Kind()
					var vcErr *schema.VoiceControlError
					if errors.As(err, &vcErr) {
						id, kind = vcErr.ID, ""
					}
					tr.written(id, kind)
					continue
				}
				rd, err := tr.read(line.Bytes(), false)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if rd.gap {
					gaps++
				}
				if rd.ev.Ack != nil && rd.acked.id != rd.ev.Ack.ID {
					t.Fatalf("step %d: the ack of %d answered %d", i, rd.ev.Ack.ID, rd.acked.id)
				}
			}
			if want := strings.Contains(file, "gap-resync"); (gaps > 0) != want {
				t.Errorf("%d gaps seen", gaps)
			}
			last := doc.Steps[len(doc.Steps)-1]
			if last.Exit == nil {
				t.Fatal("no exit")
			}
			if tr.hello != nil && tr.wrote && !tr.final && *last.Exit != 0 {
				t.Error("a helper that failed without a final state")
			}
		})
	}
}

// TestTrackerRules: the owner's reading rules of §5 and §7, one at a time.
func TestTrackerRules(t *testing.T) {
	hello := func(seq int64) schema.VoiceControlMessage {
		return &schema.VoiceControlHelloV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlEventHello, Seq: seq, Versions: []string{schema.VoiceControlSchemaV1}, HelperVersion: "fake"}
	}
	state := func(seq int64, phase schema.VoiceControlPhase, reason schema.VoiceControlReason) schema.VoiceControlMessage {
		blocked := []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected}
		capture := schema.VoiceControlCaptureOff
		if phase == schema.VoiceControlPhaseConnected {
			blocked, capture = []schema.VoiceControlBlock{}, schema.VoiceControlCaptureOn
		}
		return &schema.VoiceControlStateV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlEventState, Seq: seq, Phase: phase, Reason: reason, Capture: capture, Blocked: blocked, PTT: schema.VoiceControlPTTReady}
	}
	ack := func(seq, id int64) schema.VoiceControlMessage {
		return &schema.VoiceControlAckV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlEventAck, Seq: seq, ID: id, OK: true}
	}
	connected := schema.VoiceControlPhaseConnected
	type step struct {
		write int64 // a command id the owner writes
		event schema.VoiceControlMessage
		bad   bool
		gap   bool
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{name: "an ordinary session", steps: []step{{event: hello(1)}, {write: 1}, {event: state(2, connected, "")}, {event: ack(3, 1)}, {write: 2}, {write: 3}, {event: ack(4, 2)}, {event: ack(5, 3)}}},
		{name: "the first event is not a hello", steps: []step{{event: state(1, connected, ""), bad: true}}},
		{name: "a second hello", steps: []step{{event: hello(1)}, {event: hello(1), bad: true}}},
		{name: "seq repeated", steps: []step{{event: hello(1)}, {write: 1}, {event: state(2, connected, "")}, {event: state(2, connected, ""), bad: true}}},
		{name: "seq going back", steps: []step{{event: hello(1)}, {write: 1}, {event: state(5, connected, ""), gap: true}, {event: state(4, connected, ""), bad: true}}},
		{name: "a gap is not an error", steps: []step{{event: hello(1)}, {write: 1}, {event: state(2, connected, "")}, {event: state(6, connected, ""), gap: true}}},
		{name: "a state before any command", steps: []step{{event: hello(1)}, {event: state(2, connected, ""), bad: true}}},
		{name: "an ack with nothing written", steps: []step{{event: hello(1)}, {write: 1}, {event: ack(2, 1)}, {event: ack(3, 2), bad: true}}},
		{name: "an ack out of order", steps: []step{{event: hello(1)}, {write: 1}, {write: 2}, {event: ack(2, 2), bad: true}}},
		{name: "an event after the final state", steps: []step{{event: hello(1)}, {write: 1}, {event: state(2, schema.VoiceControlPhaseStopped, schema.VoiceControlReasonLeft)}, {event: state(3, schema.VoiceControlPhaseStopped, schema.VoiceControlReasonLeft), bad: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &tracker{}
			for i, s := range tt.steps {
				if s.event == nil {
					tr.written(s.write, schema.VoiceControlCommandGetState)
					continue
				}
				line, err := schema.MarshalVoiceControlLine(s.event)
				if err != nil {
					t.Fatal(err)
				}
				rd, err := tr.read(bytes.TrimSuffix(line, []byte{'\n'}), false)
				if (err != nil) != s.bad {
					t.Fatalf("step %d: %v, want bad %v", i, err, s.bad)
				}
				if err == nil && rd.gap != s.gap {
					t.Errorf("step %d: gap %v, want %v", i, rd.gap, s.gap)
				}
			}
		})
	}
}
