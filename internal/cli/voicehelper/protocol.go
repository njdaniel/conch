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
type tracker struct {
	lastSeq int64
	hello   *schema.VoiceControlHelloV1
	wrote   bool
	final   bool
	pending []pendingCommand
}

// reading is one event the tracker accepted.
type reading struct {
	ev schema.VoiceControlEvent
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

// read checks one line from the helper. An event of a kind this version does
// not know comes back with ev.Unknown set, to be skipped.
func (t *tracker) read(line []byte, tooLong bool) (reading, error) {
	if tooLong {
		return reading{}, badEvent("%s", schema.VoiceControlErrorTooLong)
	}
	ev, err := schema.ParseVoiceControlEvent(line)
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
	r := reading{ev: ev, gap: t.lastSeq != 0 && ev.Seq > t.lastSeq+1}
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
