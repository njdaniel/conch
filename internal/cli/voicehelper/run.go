package voicehelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

const (
	// drainGrace is how long, after the process has exited, the readers are
	// given to reach the end of its pipes before they are closed: long
	// enough for the last lines it wrote, and a bound should something it
	// started still hold a write end.
	drainGrace = 500 * time.Millisecond
	// leaveWriteTimeout bounds writing leave at the start of a stop; standard
	// input is closed straight after, whether or not it was written.
	leaveWriteTimeout = 250 * time.Millisecond
)

// run is one helper process, from start to reaped.
type run struct {
	gen     uint64
	channel string
	cmd     *exec.Cmd
	started time.Time

	// The owner's ends of the four pipes.
	stdin, events, stdout, stderr *os.File

	// writeMu keeps one command being written at a time, in id order.
	writeMu sync.Mutex
	// slots holds one token per command written and not answered.
	slots chan struct{}

	helloCh     chan struct{} // closed when the hello has been read
	stopCh      chan struct{} // closed when a stop begins
	exited      chan struct{} // closed when Wait has returned
	readersDone chan struct{} // closed when the three pipe readers have ended
	reaped      chan struct{} // closed when the run is over and accounted for

	// Guarded by Supervisor.mu.
	tr            tracker
	nextID        int64
	waiters       map[int64]*waiter
	ready         bool   // the start was acknowledged
	leaving       bool   // Leave, Close or a cancelled Join stops it: idle once reaped
	broken        bool   // its events are no longer believed
	supReason     string // the supervisor's own reason for its failure
	stopping      bool
	stopAt        time.Time
	stdinClosed   bool
	waited        bool // Wait has returned: nothing may signal it any more
	sentTerm      bool
	sentKill      bool
	resyncing     bool
	resyncPending bool
	state         *os.ProcessState
	exitAt        time.Time
}

// waiter is the caller of one command, waiting for its ack.
type waiter struct {
	kind  schema.VoiceControlCommandKind
	ch    chan ackResult
	timer *time.Timer
	done  bool
}

// ackResult is how a command ended: its ack, or why there was none.
type ackResult struct {
	ok   bool
	code schema.VoiceControlErrorCode
	err  error
}

func (a ackResult) error(kind schema.VoiceControlCommandKind) error {
	switch {
	case a.err != nil:
		return a.err
	case a.ok:
		return nil
	case a.code == schema.VoiceControlErrorStopping:
		// The helper began stopping on its own before it read the command.
		return ErrNotRunning
	default:
		return newError(ReasonHelperIncompatible, fmt.Sprintf("conch-voice refused %s (%s) and was stopped", kind, a.code), nil)
	}
}

// exitInfo is how the last helper ended, for tests and status views.
type exitInfo struct {
	gen      uint64
	pid      int
	state    *os.ProcessState
	sentTerm bool
	sentKill bool
	// stopTook is from the start of the stop to the exit; 0 if it exited
	// before any stop.
	stopTook time.Duration
}

// spawnLocked starts the helper at path: no shell, the path as argv[0] and
// "managed" after it, the environment of §9, working directory /, its own
// process group, and four pipes, the events on fd 3. Only these four
// descriptors reach it: os/exec opens every other one close-on-exec.
func (s *Supervisor) spawnLocked(path, channel string) (*run, error) {
	var opened []*os.File
	closeAll := func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			opened = append(opened, r, w)
		}
		return r, w, err
	}
	stdinR, stdinW, err := pipe()
	if err != nil {
		return nil, err
	}
	eventsR, eventsW, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	stderrR, stderrW, err := pipe()
	if err != nil {
		closeAll()
		return nil, err
	}
	cmd := &exec.Cmd{
		Path:        path,
		Args:        append([]string{path, schema.VoiceControlSubcommand}, s.opts.TestArgs...),
		Env:         helperEnv(os.Environ(), s.profile),
		Dir:         "/",
		Stdin:       stdinR,
		Stdout:      stdoutW,
		Stderr:      stderrW,
		ExtraFiles:  []*os.File{eventsW}, // fd 3 + 0 = schema.VoiceControlEventFD
		SysProcAttr: sysProcAttr(),
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	// The child has its own copies; holding ours would keep its pipes from
	// ending when it does.
	for _, f := range []*os.File{stdinR, eventsW, stdoutW, stderrW} {
		_ = f.Close()
	}
	s.gen++
	r := &run{
		gen: s.gen, channel: channel, cmd: cmd, started: time.Now(),
		stdin: stdinW, events: eventsR, stdout: stdoutR, stderr: stderrR,
		slots:   make(chan struct{}, schema.VoiceControlMaxInFlight),
		helloCh: make(chan struct{}), stopCh: make(chan struct{}), exited: make(chan struct{}),
		readersDone: make(chan struct{}), reaped: make(chan struct{}),
		waiters: make(map[int64]*waiter),
	}
	var readers sync.WaitGroup
	readers.Add(3)
	go func() { defer readers.Done(); s.readEvents(r) }()
	go func() { defer readers.Done(); s.readLog(r) }()
	go func() {
		defer readers.Done()
		// Native output is read and thrown away, so that a library writing
		// to standard output never blocks the helper.
		buf := make([]byte, 32*1024)
		for {
			if _, err := r.stdout.Read(buf); err != nil {
				return
			}
		}
	}()
	go func() { readers.Wait(); close(r.readersDone) }()
	go s.wait(r)
	return r, nil
}

// readEvents reads fd 3 until it ends.
func (s *Supervisor) readEvents(r *run) {
	lines := newLineReader(r.events)
	for {
		line, tooLong, err := lines.next()
		if err != nil {
			// A partial line at the end is discarded, not acted on.
			break
		}
		s.onEvent(r, line, tooLong)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.tr.hello == nil:
		// It ended before its hello: an old binary without managed, or one
		// that could not start.
		s.failLocked(r, ReasonHelperIncompatible)
		s.stopLocked(r, false)
	case !r.tr.final:
		// No final state can come now. Why it ended is decided once it has
		// been reaped.
		s.stopLocked(r, true)
	}
}

// onEvent applies one line from fd 3.
func (s *Supervisor) onEvent(r *run, line []byte, tooLong bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.broken {
		return
	}
	rd, err := r.tr.read(line, tooLong)
	if err != nil {
		s.noteLocked(r, err.Error())
		hello := r.tr.hello != nil
		s.failLocked(r, ReasonBadEvent)
		if hello {
			s.stopLocked(r, true)
		} else {
			// Told nothing, it can have captured nothing: kill it.
			s.killLocked(r)
		}
		return
	}
	switch ev := rd.ev; {
	case ev.Hello != nil:
		if s.cur == r {
			s.status.HelperVersion = ev.Hello.HelperVersion
			s.publishLocked()
		}
		close(r.helloCh)
	case ev.State != nil:
		if s.cur == r && r.supReason == "" {
			s.status = fromState(r.gen, r.channel, r.tr.hello.HelperVersion, ev.State)
			if !s.closed {
				s.muted, s.deafened = ev.State.Muted, ev.State.Deafened
			}
			s.publishLocked()
		}
		if ev.State.Phase.Final() {
			s.stopLocked(r, false)
		}
	case ev.Ack != nil:
		w := r.waiters[ev.Ack.ID]
		delete(r.waiters, ev.Ack.ID)
		if !ev.Ack.OK && ev.Ack.Error != schema.VoiceControlErrorStopping {
			// The owner writes only valid commands, so this is a helper that
			// disagrees about the protocol.
			s.noteLocked(r, fmt.Sprintf("the helper refused %s with %s", rd.acked.kind, ev.Ack.Error))
			s.failLocked(r, ReasonHelperIncompatible)
			s.stopLocked(r, true)
		}
		if w != nil {
			s.resolveLocked(r, w, ackResult{ok: ev.Ack.OK, code: ev.Ack.Error})
		}
	case ev.ProtocolError != nil:
		s.noteLocked(r, "the helper could not act on a command line: "+string(ev.ProtocolError.Code))
	}
	if rd.gap {
		s.resyncLocked(r)
	}
}

// resyncLocked asks for a snapshot after a gap in seq, once the start has
// been acknowledged (nothing but start may be written before).
func (s *Supervisor) resyncLocked(r *run) {
	switch {
	case r.stopping || r.broken:
		return
	case !r.ready:
		r.resyncPending = true
		return
	case r.resyncing:
		return
	}
	r.resyncing = true
	go func() {
		// The ack bound ends the exchange whatever happens.
		_ = s.exchange(context.Background(), r, schema.VoiceControlCommandGetState, getState)
		s.mu.Lock()
		r.resyncing = false
		s.mu.Unlock()
	}()
}

// readLog keeps the helper's standard error as a bounded, sanitized tail.
func (s *Supervisor) readLog(r *run) {
	lines := newLineReader(r.stderr)
	for {
		line, _, err := lines.next()
		if err == nil || len(line) > 0 {
			text := sanitizeDiag(line, s.secret())
			s.mu.Lock()
			if s.cur == r {
				s.addDiagLocked(text)
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// write writes one command and returns the waiter for its ack. The ack bound
// runs from before the write; a write that fails or does not finish in it
// resolves the waiter with the error.
func (s *Supervisor) write(ctx context.Context, r *run, kind schema.VoiceControlCommandKind, build func(int64) schema.VoiceControlMessage) (*waiter, error) {
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.stopCh:
		return nil, ErrNotRunning
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	s.mu.Lock()
	if r.stopping {
		s.mu.Unlock()
		<-r.slots
		return nil, ErrNotRunning
	}
	id := r.nextID + 1
	line, err := schema.MarshalVoiceControlLine(build(id))
	if err != nil {
		s.mu.Unlock()
		<-r.slots
		return nil, fmt.Errorf("voicehelper: %w", err)
	}
	r.nextID = id
	r.tr.written(id, kind)
	w := &waiter{kind: kind, ch: make(chan ackResult, 1)}
	r.waiters[id] = w
	w.timer = time.AfterFunc(s.t.Ack, func() { s.ackTimedOut(r, id) })
	s.mu.Unlock()

	err = r.stdin.SetWriteDeadline(time.Now().Add(s.t.Ack))
	if err == nil {
		_, err = r.stdin.Write(line)
	}
	if err != nil {
		s.writeFailed(r, id, err)
	}
	return w, nil
}

// exchange writes one command and waits for its answer.
func (s *Supervisor) exchange(ctx context.Context, r *run, kind schema.VoiceControlCommandKind, build func(int64) schema.VoiceControlMessage) error {
	w, err := s.write(ctx, r, kind, build)
	if err != nil {
		return err
	}
	select {
	case res := <-w.ch:
		return res.error(kind)
	case <-ctx.Done():
		// The ack is still read when it comes, and its bound still holds.
		return ctx.Err()
	}
}

// writeFailed handles a command that could not be written: one that did not
// finish in the ack bound is a broken helper; one the pipe refused means the
// helper has closed its standard input or gone, which its exit will explain.
func (s *Supervisor) writeFailed(r *run, id int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := r.waiters[id]
	if w == nil || w.done {
		return
	}
	delete(r.waiters, id)
	var res ackResult
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		s.failLocked(r, ReasonAckTimeout)
		s.stopLocked(r, true)
		res.err = reasonError(ReasonAckTimeout)
	case r.stopping:
		res.err = ErrNotRunning
	default:
		s.stopLocked(r, false)
		res.err = fmt.Errorf("voicehelper: the helper's control pipe is broken: %w", err)
	}
	s.resolveLocked(r, w, res)
}

// ackTimedOut is the ack bound of command id running out.
func (s *Supervisor) ackTimedOut(r *run, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := r.waiters[id]
	if w == nil || w.done {
		return
	}
	delete(r.waiters, id)
	s.failLocked(r, ReasonAckTimeout)
	s.stopLocked(r, true)
	s.resolveLocked(r, w, ackResult{err: reasonError(ReasonAckTimeout)})
}

// resolveLocked answers a waiter once and gives back its slot.
func (s *Supervisor) resolveLocked(r *run, w *waiter, res ackResult) {
	if w.done {
		return
	}
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
	<-r.slots
	w.ch <- res
}

// failLocked marks r broken for reason; the first reason stands. A run being
// left is not a failure: it becomes idle.
func (s *Supervisor) failLocked(r *run, reason string) {
	r.broken = true
	if r.supReason != "" || r.leaving {
		return
	}
	r.supReason = reason
	if s.cur == r {
		s.status = s.status.failed(reason)
		s.publishLocked()
	}
}

// stopLocked begins the stop of §8.5, once: leave (when a start has been
// written and leave is asked for), standard input closed straight away,
// SIGTERM at StopTerm and SIGKILL at StopKill if it is still there.
func (s *Supervisor) stopLocked(r *run, leave bool) {
	if r.stopping {
		return
	}
	r.stopping = true
	r.stopAt = time.Now()
	close(r.stopCh)
	go s.stopper(r, leave && r.tr.wrote)
}

// killLocked stops r with SIGKILL at once: for a helper that has been told
// nothing and so can have opened nothing.
func (s *Supervisor) killLocked(r *run) {
	s.stopLocked(r, false)
	s.closeStdinLocked(r)
	s.signalLocked(r, syscall.SIGKILL)
}

func (s *Supervisor) stopper(r *run, leave bool) {
	if leave {
		s.writeLeave(r)
	}
	s.mu.Lock()
	s.closeStdinLocked(r)
	s.mu.Unlock()
	for _, step := range []struct {
		after time.Duration
		sig   syscall.Signal
	}{{s.t.StopTerm, syscall.SIGTERM}, {s.t.StopKill, syscall.SIGKILL}} {
		timer := time.NewTimer(time.Until(r.stopAt.Add(step.after)))
		select {
		case <-r.exited:
			timer.Stop()
			return
		case <-timer.C:
		}
		s.mu.Lock()
		s.signalLocked(r, step.sig)
		s.mu.Unlock()
	}
}

// writeLeave writes leave if the control pipe is free and has room for one
// more command; a stop never waits for it.
func (s *Supervisor) writeLeave(r *run) {
	if !r.writeMu.TryLock() {
		return
	}
	defer r.writeMu.Unlock()
	select {
	case r.slots <- struct{}{}:
	default:
		return
	}
	s.mu.Lock()
	id := r.nextID + 1
	line, err := schema.MarshalVoiceControlLine(schema.VoiceControlLeaveV1{
		Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandLeave, ID: id,
	})
	if err != nil || r.stdinClosed {
		s.mu.Unlock()
		<-r.slots
		return
	}
	r.nextID = id
	r.tr.written(id, schema.VoiceControlCommandLeave)
	r.waiters[id] = &waiter{kind: schema.VoiceControlCommandLeave, ch: make(chan ackResult, 1)}
	s.mu.Unlock()
	if r.stdin.SetWriteDeadline(time.Now().Add(leaveWriteTimeout)) == nil {
		_, _ = r.stdin.Write(line)
	}
}

func (s *Supervisor) closeStdinLocked(r *run) {
	if !r.stdinClosed {
		r.stdinClosed = true
		_ = r.stdin.Close()
	}
}

// signalLocked signals the process this supervisor started, and only until
// Wait has returned, so the pid cannot have been reused. (os.Process refuses
// a signal once Wait has begun to reap, and uses a pidfd where it can.)
func (s *Supervisor) signalLocked(r *run, sig syscall.Signal) {
	if r.waited {
		return
	}
	switch sig {
	case syscall.SIGTERM:
		r.sentTerm = true
	case syscall.SIGKILL:
		r.sentKill = true
	}
	_ = r.cmd.Process.Signal(sig)
}

// wait reaps the process, once, and then accounts for the run.
func (s *Supervisor) wait(r *run) {
	_ = r.cmd.Wait()
	s.mu.Lock()
	r.waited = true
	r.state = r.cmd.ProcessState
	r.exitAt = time.Now()
	s.mu.Unlock()
	close(r.exited)

	grace := time.NewTimer(drainGrace)
	select {
	case <-r.readersDone:
	case <-grace.C:
	}
	grace.Stop()
	// Something the helper started may still hold a write end; the readers
	// end here whatever it does.
	for _, f := range []*os.File{r.events, r.stdout, r.stderr} {
		_ = f.Close()
	}
	<-r.readersDone
	s.finish(r)
}

// finish accounts for a reaped run: its waiters answered, and the status
// idle (it was left), its failure, its final state, or why it ended without
// one. Only then may another helper start.
func (s *Supervisor) finish(r *run) {
	s.mu.Lock()
	unanswered := ErrNotRunning
	if r.supReason != "" {
		unanswered = reasonError(r.supReason)
	}
	for id, w := range r.waiters {
		delete(r.waiters, id)
		s.resolveLocked(r, w, ackResult{err: unanswered})
	}
	s.closeStdinLocked(r)
	info := exitInfo{gen: r.gen, pid: r.cmd.Process.Pid, state: r.state, sentTerm: r.sentTerm, sentKill: r.sentKill}
	if r.stopping && r.exitAt.After(r.stopAt) {
		info.stopTook = r.exitAt.Sub(r.stopAt)
	}
	s.last = info
	if s.cur == r {
		switch {
		case r.leaving:
			s.status = Status{Gen: r.gen, Phase: PhaseIdle}
		case r.supReason != "" || r.tr.final:
			s.status = s.status.ended()
		default:
			reason := ReasonHelperExited
			if s.crashed(r) {
				reason = ReasonHelperCrashed
			}
			r.supReason = reason
			s.status = s.status.failed(reason).ended()
		}
		s.cur = nil
		s.publishLocked()
	}
	s.mu.Unlock()
	close(r.reaped)
}

// crashed reports whether r ended by a signal this supervisor did not send,
// or with 101, Rust's exit status for a panic.
func (s *Supervisor) crashed(r *run) bool {
	if sig, ok := endedBySignal(r.state); ok {
		ours := (sig == syscall.SIGTERM && r.sentTerm) || (sig == syscall.SIGKILL && r.sentKill)
		return !ours
	}
	return r.state != nil && r.state.ExitCode() == 101
}
