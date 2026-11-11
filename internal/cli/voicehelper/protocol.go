package voicehelper

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/njdaniel/conch/pkg/schema"
)

// lineReader reads lines of at most schema.VoiceControlMaxLineBytes bytes
// (not counting the line feed) and never holds more than that: the rest of a
// longer line is discarded as it is read (design note §3).
type lineReader struct {
	br *bufio.Reader
	// skipping: the line before was too long, and the rest of it, up to its
	// line feed, is still to be discarded.
	skipping bool
}

func newLineReader(r io.Reader) *lineReader {
	// One byte more than a line, for its line feed.
	return &lineReader{br: bufio.NewReaderSize(r, schema.VoiceControlMaxLineBytes+1)}
}

// next returns the next line without its line feed. For a line that is too
// long it returns the first VoiceControlMaxLineBytes bytes and tooLong as
// soon as the bound is passed, without waiting for a line feed that may never
// come; the rest is discarded by the next call. At the end of input it
// returns the error, and with it whatever partial line came before (which a
// protocol reader discards).
func (l *lineReader) next() (line []byte, tooLong bool, err error) {
	for l.skipping {
		_, err := l.br.ReadSlice('\n')
		switch {
		case err == nil:
			l.skipping = false
		case !errors.Is(err, bufio.ErrBufferFull):
			return nil, false, err
		}
	}
	b, err := l.br.ReadSlice('\n')
	switch {
	case err == nil:
		return append([]byte(nil), b[:len(b)-1]...), false, nil
	case errors.Is(err, bufio.ErrBufferFull):
		l.skipping = true
		return append([]byte(nil), b[:schema.VoiceControlMaxLineBytes]...), true, nil
	default:
		return append([]byte(nil), b...), false, err
	}
}

// pendingCommand is a command written and not yet answered.
type pendingCommand struct {
	id   int64
	kind schema.VoiceControlCommandKind
}

// tracker holds one helper's event stream to the owner's reading rules
// (design note §5 and §7): the hello first and only once, seq always
// increasing, nothing before the owner has written, acks in the order the
// commands were written, nothing after the final state. It does no I/O, so
// the fixtures can drive it directly.
//
// The hello is always v1's shape; the owner then chooses a version (choose)
// before it writes its start, and every later line is read in that version
// (design note §20.2). The zero tracker reads v1.
type tracker struct {
	lastSeq int64
	hello   *schema.VoiceControlHelloV1
	// version is the schema name the session speaks, as choose set it:
	// "" (read as v1) until then.
	version string
	wrote   bool
	final   bool
	pending []pendingCommand
}

// reading is one event the tracker accepted.
type reading struct {
	// ev is the event in v1's shapes. In a v2 session an ack, a state and a
	// protocol error are projected onto them, so the owner's rules are
	// written once; v2 carries only what v1 does and the fields in v2 below.
	ev schema.VoiceControlEvent
	// v2 is the state as the helper wrote it, in a v2 session; nil otherwise.
	// It holds the fields v1 has no place for (playback, talk_mode).
	v2 *schema.VoiceControlStateV2
	// gap: one or more events were dropped before this one; the owner asks
	// for a snapshot.
	gap bool
	// acked is the command an ack answers.
	acked pendingCommand
}

// badEventError is why the tracker refused an event. Its text names only the
// rule and vocabulary values, never the line.
type badEventError struct{ detail string }

func (e *badEventError) Error() string { return "the helper wrote a bad event: " + e.detail }

func badEvent(format string, args ...any) error {
	return &badEventError{detail: fmt.Sprintf(format, args...)}
}

// written records a command the owner has written.
func (t *tracker) written(id int64, kind schema.VoiceControlCommandKind) {
	t.wrote = true
	t.pending = append(t.pending, pendingCommand{id: id, kind: kind})
}

// choose fixes the version of the session from the hello the tracker has
// read: v2 when the helper offers it, else v1. ok is false when the helper
// offers neither, a helper the owner cannot speak to. It must be called
// before the owner writes its start.
func (t *tracker) choose() (version string, ok bool) {
	switch {
	case t.hello == nil:
		return "", false
	case t.hello.Offers(schema.VoiceControlSchemaV2):
		t.version = schema.VoiceControlSchemaV2
	case t.hello.Offers(schema.VoiceControlSchemaV1):
		t.version = schema.VoiceControlSchemaV1
	default:
		return "", false
	}
	return t.version, true
}

// v2 reports whether the session speaks v2.
func (t *tracker) v2() bool { return t.version == schema.VoiceControlSchemaV2 }

// parse reads one line in the version in force: the hello, which is v1's
// shape in both, until the hello has been read, and the chosen version after.
func (t *tracker) parse(line []byte) (schema.VoiceControlEvent, *schema.VoiceControlStateV2, error) {
	if t.hello == nil || !t.v2() {
		ev, err := schema.ParseVoiceControlEvent(line)
		return ev, nil, err
	}
	ev2, err := schema.ParseVoiceControlEventV2(line)
	if err != nil {
		return schema.VoiceControlEvent{}, nil, err
	}
	ev := schema.VoiceControlEvent{Seq: ev2.Seq, Unknown: ev2.Unknown}
	switch {
	case ev2.Ack != nil:
		ev.Ack = &schema.VoiceControlAckV1{
			Schema: schema.VoiceControlSchemaV1, Kind: ev2.Ack.Kind, Seq: ev2.Ack.Seq,
			ID: ev2.Ack.ID, OK: ev2.Ack.OK, Error: ev2.Ack.Error,
		}
	case ev2.State != nil:
		ev.State = stateV1Of(ev2.State)
	case ev2.ProtocolError != nil:
		ev.ProtocolError = &schema.VoiceControlProtocolErrorV1{
			Schema: schema.VoiceControlSchemaV1, Kind: ev2.ProtocolError.Kind, Seq: ev2.ProtocolError.Seq,
			Code: ev2.ProtocolError.Code,
		}
	}
	return ev, ev2.State, nil
}

// stateV1Of is the v1 part of a v2 state: what the owner's rules and the
// status read the same way in both versions. The result is never put on the
// wire and does not pass v1's Validate (a v1 state cannot carry
// voice_removed).
func stateV1Of(st *schema.VoiceControlStateV2) *schema.VoiceControlStateV1 {
	return &schema.VoiceControlStateV1{
		Schema: schema.VoiceControlSchemaV1, Kind: st.Kind, Seq: st.Seq,
		Phase: st.Phase, Reason: st.Reason, RetryInMS: st.RetryInMS,
		Capture: st.Capture, Transmitting: st.Transmitting, Muted: st.Muted, Deafened: st.Deafened,
		Blocked: st.Blocked, PTT: st.PTT, PTTProblem: st.PTTProblem,
	}
}

// read checks one line from the helper. An event of a kind this version does
// not know comes back with ev.Unknown set, to be skipped.
func (t *tracker) read(line []byte, tooLong bool) (reading, error) {
	if tooLong {
		return reading{}, badEvent("%s", schema.VoiceControlErrorTooLong)
	}
	ev, state2, err := t.parse(line)
	if err != nil {
		var vcErr *schema.VoiceControlError
		if errors.As(err, &vcErr) {
			return reading{}, badEvent("%s", vcErr.Code)
		}
		return reading{}, badEvent("unreadable")
	}
	switch {
	case t.final:
		return reading{}, badEvent("an event after the final state")
	case t.hello == nil && ev.Hello == nil:
		return reading{}, badEvent("the first event is not a hello")
	case ev.Seq <= t.lastSeq:
		return reading{}, badEvent("seq %d after %d", ev.Seq, t.lastSeq)
	case ev.Hello != nil && t.hello != nil:
		return reading{}, badEvent("a second hello")
	case (ev.State != nil || ev.ProtocolError != nil || ev.Ack != nil) && !t.wrote:
		return reading{}, badEvent("an event other than hello before any command")
	}
	r := reading{ev: ev, v2: state2, gap: t.lastSeq != 0 && ev.Seq > t.lastSeq+1}
	switch {
	case ev.Hello != nil:
		t.hello = ev.Hello
	case ev.Ack != nil:
		if len(t.pending) == 0 || t.pending[0].id != ev.Ack.ID {
			return reading{}, badEvent("an ack for id %d, which is not the next command", ev.Ack.ID)
		}
		r.acked = t.pending[0]
		t.pending = t.pending[1:]
	case ev.State != nil:
		t.final = ev.State.Phase.Final()
	}
	t.lastSeq = ev.Seq
	return r, nil
}
