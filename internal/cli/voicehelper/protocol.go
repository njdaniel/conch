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
// The hello is always read with the v1 reader. Every later event is read with
// the reader of the version the owner chose, and with that reader alone
// (§20.2): in a v2 session a v1 line is unknown_schema and a hello is invalid,
// a bad event like anything else the reader refuses.
type tracker struct {
	lastSeq int64
	hello   *schema.VoiceControlHelloV1
	wrote   bool
	final   bool
	pending []pendingCommand
	// version is the session's schema, set before the start is written; ""
	// is read as v1.
	version string
}

// reading is one event the tracker accepted.
type reading struct {
	// ev is what the v1 reader read: the hello, and every event of a v1
	// session. ev2 is what the v2 reader read: every event of a v2 session
	// after the hello. Exactly one of the two holds the event.
	ev  schema.VoiceControlEvent
	ev2 schema.VoiceControlEventV2
	// gap: one or more events were dropped before this one; the owner asks
	// for a snapshot.
	gap bool
	// acked is the command an ack answers.
	acked pendingCommand
}

// eventHead is what the reading rules need of an event, whichever reader read
// it.
type eventHead struct {
	seq                              int64
	hello, ack, state, protocolError bool
	ackID                            int64
	final                            bool
}

func (r reading) head() eventHead {
	if r.ev.Seq != 0 {
		ev := r.ev
		h := eventHead{seq: ev.Seq, hello: ev.Hello != nil, ack: ev.Ack != nil, state: ev.State != nil, protocolError: ev.ProtocolError != nil}
		if ev.Ack != nil {
			h.ackID = ev.Ack.ID
		}
		if ev.State != nil {
			h.final = ev.State.Phase.Final()
		}
		return h
	}
	ev := r.ev2
	h := eventHead{seq: ev.Seq, ack: ev.Ack != nil, state: ev.State != nil, protocolError: ev.ProtocolError != nil}
	if ev.Ack != nil {
		h.ackID = ev.Ack.ID
	}
	if ev.State != nil {
		h.final = ev.State.Phase.Final()
	}
	return h
}

// ackOf is the outcome of the ack read, whichever reader read it.
func (r reading) ackOf() (ok bool, code schema.VoiceControlErrorCode) {
	if r.ev.Ack != nil {
		return r.ev.Ack.OK, r.ev.Ack.Error
	}
	return r.ev2.Ack.OK, r.ev2.Ack.Error
}

// protocolErrorCode is the code of the protocol_error read, whichever reader
// read it.
func (r reading) protocolErrorCode() schema.VoiceControlErrorCode {
	if r.ev.ProtocolError != nil {
		return r.ev.ProtocolError.Code
	}
	return r.ev2.ProtocolError.Code
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

// read checks one line from the helper. An event of a kind this version does
// not know comes back with ev.Unknown (or ev2.Unknown) set, to be skipped.
func (t *tracker) read(line []byte, tooLong bool) (reading, error) {
	if tooLong {
		return reading{}, badEvent("%s", schema.VoiceControlErrorTooLong)
	}
	var r reading
	var err error
	if t.hello != nil && t.version == schema.VoiceControlSchemaV2 {
		r.ev2, err = schema.ParseVoiceControlEventV2(line)
	} else {
		r.ev, err = schema.ParseVoiceControlEvent(line)
	}
	if err != nil {
		var vcErr *schema.VoiceControlError
		if errors.As(err, &vcErr) {
			return reading{}, badEvent("%s", vcErr.Code)
		}
		return reading{}, badEvent("unreadable")
	}
	h := r.head()
	switch {
	case t.final:
		return reading{}, badEvent("an event after the final state")
	case t.hello == nil && !h.hello:
		return reading{}, badEvent("the first event is not a hello")
	case h.seq <= t.lastSeq:
		return reading{}, badEvent("seq %d after %d", h.seq, t.lastSeq)
	case h.hello && t.hello != nil:
		return reading{}, badEvent("a second hello")
	case (h.state || h.protocolError || h.ack) && !t.wrote:
		return reading{}, badEvent("an event other than hello before any command")
	}
	r.gap = t.lastSeq != 0 && h.seq > t.lastSeq+1
	switch {
	case h.hello:
		t.hello = r.ev.Hello
	case h.ack:
		if len(t.pending) == 0 || t.pending[0].id != h.ackID {
			return reading{}, badEvent("an ack for id %d, which is not the next command", h.ackID)
		}
		r.acked = t.pending[0]
		t.pending = t.pending[1:]
	case h.state:
		t.final = h.final
	}
	t.lastSeq = h.seq
	return r, nil
}
