// Package voicehelper starts, drives and stops the managed conch-voice helper
// that gives the TUI voice (docs/design/tui-voice.md, §14 for this side).
//
// A Supervisor owns at most one helper process at a time, started by Join for
// one voice session and stopped by Leave or Close. It speaks
// conch.voice_control.v1 over two private pipes: commands to the helper's
// standard input, events from its file descriptor 3. Its standard output is
// drained unread and its standard error kept as a short, sanitized tail
// (Diagnostics). Status reports only what the helper confirmed and what the
// supervisor saw itself, never what was asked for.
//
// The process rules are the security core: no shell, the discovery and trust
// checks of §10, the environment of §9 (the token never in argv, Status,
// errors or Diagnostics), a process group of its own, signals only to the
// *os.Process this supervisor started and only before Wait has returned, and
// no new helper until the previous one has been reaped. Nothing restarts by
// itself, so nothing reopens a microphone by itself.
//
// There is no TUI code here; the TUI calls these methods from tea.Cmds.
package voicehelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Profile is the account a helper joins as. The TUI builds one when it has a
// signed-in person.
type Profile struct {
	// Server is the conchd address as cli.NormalizeServer wrote it.
	Server string
	// ConfigDir is the TUI's configuration directory (cli.ConfigDir), always
	// absolute; "" when it has none.
	ConfigDir string
	// PrincipalID is the TUI's user, from its own whoami.
	PrincipalID int64
	// Credential is stored, or env when the TUI's token came from
	// CONCH_TOKEN (cli.LoadCredential).
	Credential schema.VoiceControlCredential
	// Token is the TUI's token, used only with env: it is passed to the
	// helper as CONCH_TOKEN and never logged, put in a Status or an error,
	// or shown in Diagnostics.
	Token string
}

// Options configures how the helper is found and timed.
type Options struct {
	// HelperPath is the value of CONCH_VOICE_HELPER (HelperPathFromEnv): an
	// absolute path, used exactly. "" (or white space) searches beside conch
	// and then PATH.
	HelperPath string
	// TestArgs are extra helper arguments after "managed". For tests only;
	// the TUI never sets them.
	TestArgs []string
	// Timings shortens the protocol's bounds. For tests only; zero fields
	// take the contract's values.
	Timings Timings
}

// HelperPathFromEnv is the CONCH_VOICE_HELPER of conch's own environment, for
// Options.HelperPath. The helper never inherits the variable.
func HelperPathFromEnv() string { return os.Getenv(HelperEnv) }

// Timings are the owner's bounds of the contract (design note §3).
type Timings struct {
	// Hello is from starting the process to reading its hello.
	Hello time.Duration
	// Ack is from writing a command (the write included) to reading its ack.
	Ack time.Duration
	// StopTerm and StopKill are when, from the start of a stop, SIGTERM and
	// then SIGKILL are sent to a helper that has not exited.
	StopTerm time.Duration
	StopKill time.Duration
}

func (t Timings) orDefaults() Timings {
	ms := func(d *time.Duration, def int) {
		if *d <= 0 {
			*d = time.Duration(def) * time.Millisecond
		}
	}
	ms(&t.Hello, schema.VoiceControlHelloTimeoutMS)
	ms(&t.Ack, schema.VoiceControlAckTimeoutMS)
	ms(&t.StopTerm, schema.VoiceControlStopTermAfterMS)
	ms(&t.StopKill, schema.VoiceControlStopKillAfterMS)
	if t.StopKill < t.StopTerm {
		t.StopKill = t.StopTerm
	}
	return t
}

var (
	// ErrBusy: a helper is running, starting or stopping; leave first.
	ErrBusy = errors.New("voicehelper: voice is already running or stopping; leave first")
	// ErrNotRunning: no helper is running that could take the command.
	ErrNotRunning = errors.New("voicehelper: voice is not running")
	// ErrClosed: the supervisor was closed.
	ErrClosed = errors.New("voicehelper: voice is closed")
	// ErrNoCredential: voice needs a signed-in person, and the profile has no
	// login to pass on.
	ErrNoCredential = errors.New("voicehelper: voice needs a signed-in person: sign in with conch login")
	// ErrNoRetry: there is no failure that a retry could fix.
	ErrNoRetry = errors.New("voicehelper: there is no failed voice session to retry")
	// ErrStopped: Leave stopped the helper before Join finished.
	ErrStopped = errors.New("voicehelper: voice was left before it had started")
)

// Error is a failure with the reason a Status reports for it: one of the
// Reason constants, or a wire reason when the helper itself failed while
// starting. Its text never holds the token.
type Error struct {
	Reason string
	msg    string
	err    error
}

func (e *Error) Error() string { return "voicehelper: " + e.msg }

func (e *Error) Unwrap() error { return e.err }

func newError(reason, msg string, err error) *Error {
	return &Error{Reason: reason, msg: msg, err: err}
}

// reasonError is the error for a failure the supervisor saw.
func reasonError(reason string) *Error {
	msg := map[string]string{
		ReasonHelperIncompatible: "this conch-voice does not speak " + schema.VoiceControlSchemaV1 + ": install the conch-voice of the same release as conch",
		ReasonHelloTimeout:       "conch-voice did not start in time and was stopped",
		ReasonAckTimeout:         "conch-voice did not answer in time and was stopped",
		ReasonBadEvent:           "conch-voice broke the control protocol and was stopped",
		ReasonHelperExited:       "conch-voice ended without saying why",
		ReasonHelperCrashed:      "conch-voice crashed",
	}[reason]
	if msg == "" {
		msg = "voice stopped: " + reason
	}
	return newError(reason, msg, nil)
}

// Supervisor owns at most one helper at a time. Its methods are safe to call
// from any goroutine.
type Supervisor struct {
	profile Profile
	opts    Options
	t       Timings
	find    finder
	updates chan Status

	mu          sync.Mutex
	closed      bool
	cur         *run // the helper started and not yet reaped
	gen         uint64
	status      Status
	lastChannel string
	// muted and deafened are what the helper last confirmed, kept for the
	// next start, and forgotten on Close.
	muted, deafened bool
	diag            []string
	last            exitInfo
	// path is the helper last found and tried, links resolved; "" before
	// the first.
	path string
}

// New makes a supervisor for one signed-in profile. It starts nothing.
func New(p Profile, o Options) *Supervisor {
	o.TestArgs = slices.Clone(o.TestArgs)
	return &Supervisor{
		profile: p,
		opts:    o,
		t:       o.Timings.orDefaults(),
		find:    defaultFinder,
		updates: make(chan Status, 1),
		status:  Status{Phase: PhaseIdle},
	}
}

// Join starts one helper for channel and returns once it has accepted the
// start, within the hello and ack bounds. Capture can begin only after that,
// and only by the helper's own rules. With a helper running or stopping it is
// ErrBusy; one that has already ended (its final state read, or its process
// gone) is first waited for until it has been reaped, within ctx.
func (s *Supervisor) Join(ctx context.Context, channel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if err := s.awaitEndedLocked(ctx); err != nil {
		s.mu.Unlock()
		return err
	}
	switch {
	case s.closed:
		s.mu.Unlock()
		return ErrClosed
	case s.cur != nil:
		s.mu.Unlock()
		return ErrBusy
	}
	if err := s.profile.check(); err != nil {
		s.mu.Unlock()
		return err
	}
	muted, deafened := s.muted, s.deafened
	start := func(id int64) schema.VoiceControlMessage {
		return schema.VoiceControlStartV1{
			Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandStart, ID: id,
			Server: s.profile.Server, Channel: channel, PrincipalID: s.profile.PrincipalID,
			Credential: s.profile.Credential, Muted: muted, Deafened: deafened,
		}
	}
	// A start the schema refuses is a caller's mistake: nothing is started.
	if _, err := schema.MarshalVoiceControlLine(start(1)); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("voicehelper: cannot ask for voice: %w", err)
	}
	s.lastChannel = channel
	s.diag = nil
	r, err := s.launchLocked(channel)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	hello := time.NewTimer(time.Until(r.started.Add(s.t.Hello)))
	defer hello.Stop()
	select {
	case <-r.helloCh:
	case <-r.stopCh:
	case <-hello.C:
		s.mu.Lock()
		if r.tr.hello == nil {
			s.failLocked(r, ReasonHelloTimeout)
			s.killLocked(r)
		}
		s.mu.Unlock()
	case <-ctx.Done():
		s.mu.Lock()
		r.leaving = true
		s.stopLocked(r, false)
		s.mu.Unlock()
		return ctx.Err()
	}
	s.mu.Lock()
	if err := s.joinErrLocked(r); err != nil {
		s.mu.Unlock()
		return err
	}
	if !r.tr.hello.Offers(schema.VoiceControlSchemaV1) {
		// Write nothing and close standard input: the helper, told
		// nothing, exits without opening anything.
		s.noteLocked(r, "the helper does not offer "+schema.VoiceControlSchemaV1)
		s.failLocked(r, ReasonHelperIncompatible)
		s.stopLocked(r, false)
		s.mu.Unlock()
		return reasonError(ReasonHelperIncompatible)
	}
	s.mu.Unlock()

	w, err := s.write(ctx, r, schema.VoiceControlCommandStart, start)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if e := s.joinErrLocked(r); e != nil {
			return e
		}
		// Given up on before the start was written: this helper is not
		// wanted any more.
		r.leaving = true
		s.stopLocked(r, true)
		return err
	}
	var res ackResult
	select {
	case res = <-w.ch:
	case <-ctx.Done():
		s.mu.Lock()
		r.leaving = true
		s.stopLocked(r, true)
		s.mu.Unlock()
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.joinErrLocked(r); err != nil {
		return err
	}
	if err := res.error(schema.VoiceControlCommandStart); err != nil {
		return err
	}
	r.ready = true
	if r.resyncPending {
		r.resyncPending = false
		s.resyncLocked(r)
	}
	return nil
}

// launchLocked finds and starts the helper, or records why it could not.
func (s *Supervisor) launchLocked(channel string) (*run, error) {
	fail := func(err *Error) (*run, error) {
		s.status = Status{
			Gen: s.gen, Channel: channel, Capture: schema.VoiceControlCaptureOff,
			Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected},
		}.failed(err.Reason)
		s.addDiagLocked(sanitizeDiag([]byte("conch: "+err.msg), s.secret()))
		s.publishLocked()
		return nil, err
	}
	path, err := s.find.find(s.opts.HelperPath)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return fail(e)
		}
		return fail(newError(ReasonHelperNotFound, err.Error(), err))
	}
	s.path = path
	r, err := s.spawnLocked(path, channel)
	if err != nil {
		return fail(newError(ReasonSpawnFailed, "conch-voice could not be started: "+err.Error(), err))
	}
	s.cur = r
	s.status = Status{
		Gen: r.gen, Channel: channel, Phase: PhaseStarting, Capture: schema.VoiceControlCaptureOff,
		Blocked: []schema.VoiceControlBlock{schema.VoiceControlBlockNotConnected}, PTT: schema.VoiceControlPTTUnknown,
	}
	s.publishLocked()
	return r, nil
}

// joinErrLocked is why Join cannot go on with r, if it cannot.
func (s *Supervisor) joinErrLocked(r *run) error {
	switch {
	case r.leaving && s.closed:
		return ErrClosed
	case r.leaving:
		return ErrStopped
	case r.supReason != "":
		return reasonError(r.supReason)
	case r.stopping:
		if s.status.Phase == PhaseFailed || s.status.Phase == PhaseStopped {
			return reasonError(s.status.Reason)
		}
		return reasonError(ReasonHelperExited)
	}
	return nil
}

// Retry joins the last channel again after a failure that a new attempt
// could fix, with the mute and deafen last confirmed. It is a new process
// with a new handshake. Like Join, it first waits, within ctx, for a failed
// helper that has ended to be reaped.
func (s *Supervisor) Retry(ctx context.Context) error {
	s.mu.Lock()
	if err := s.awaitEndedLocked(ctx); err != nil {
		s.mu.Unlock()
		return err
	}
	switch {
	case s.closed:
		s.mu.Unlock()
		return ErrClosed
	case s.cur != nil:
		s.mu.Unlock()
		return ErrBusy
	case s.status.Phase != PhaseFailed || !s.status.Retryable || s.lastChannel == "":
		s.mu.Unlock()
		return ErrNoRetry
	}
	channel := s.lastChannel
	s.mu.Unlock()
	return s.Join(ctx, channel)
}

// awaitEndedLocked waits, with s.mu held and given up while waiting, for a
// helper that has already ended to be reaped: one whose final state has been
// read or whose process has exited, and which is only having its pipes
// drained (at most drainGrace) or, if it hangs after its final state, the stop
// escalation. A person who joins or retries as a failure is shown should not
// be told that voice is busy. A helper still running, or one being stopped,
// is not waited for: Join and Retry are ErrBusy for it. ctx bounds the wait.
func (s *Supervisor) awaitEndedLocked(ctx context.Context) error {
	for r := s.cur; r != nil && (r.waited || r.tr.final); r = s.cur {
		s.mu.Unlock()
		select {
		case <-r.reaped:
		case <-ctx.Done():
			s.mu.Lock()
			return ctx.Err()
		}
		s.mu.Lock()
	}
	return nil
}

// Leave stops the helper (design note §8.5) and returns once it has been
// reaped; voice is then idle. With no helper it clears a failure that is
// still shown, and is ErrNotRunning when there is nothing to clear. If ctx
// ends first the stop goes on, and Join stays ErrBusy until it is done.
func (s *Supervisor) Leave(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	r := s.cur
	if r == nil {
		defer s.mu.Unlock()
		if s.status.Phase == PhaseIdle {
			return ErrNotRunning
		}
		s.status = Status{Gen: s.gen, Phase: PhaseIdle}
		s.publishLocked()
		return nil
	}
	r.leaving = true
	s.stopLocked(r, true)
	s.mu.Unlock()
	select {
	case <-r.reaped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close leaves, forgets the mute and deafen kept for a retry, and refuses
// every later call. Callers bound ctx by StopKill plus a second.
func (s *Supervisor) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.closed = true
	s.muted, s.deafened = false, false
	r := s.cur
	if r == nil {
		s.status = Status{Gen: s.gen, Phase: PhaseIdle}
		s.publishLocked()
		s.mu.Unlock()
		return nil
	}
	r.leaving = true
	s.stopLocked(r, true)
	s.mu.Unlock()
	select {
	case <-r.reaped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetMute asks the helper to set mute to on, and returns after its ack; the
// latest Status then shows the result.
func (s *Supervisor) SetMute(ctx context.Context, on bool) error {
	return s.command(ctx, schema.VoiceControlCommandSetMute, func(id int64) schema.VoiceControlMessage {
		return schema.VoiceControlSetMuteV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandSetMute, ID: id, Muted: on}
	})
}

// SetDeafen asks the helper to set deafen to on, and returns after its ack.
func (s *Supervisor) SetDeafen(ctx context.Context, on bool) error {
	return s.command(ctx, schema.VoiceControlCommandSetDeafen, func(id int64) schema.VoiceControlMessage {
		return schema.VoiceControlSetDeafenV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandSetDeafen, ID: id, Deafened: on}
	})
}

// Refresh asks the helper for a state snapshot, and returns after its ack.
func (s *Supervisor) Refresh(ctx context.Context) error {
	return s.command(ctx, schema.VoiceControlCommandGetState, getState)
}

func getState(id int64) schema.VoiceControlMessage {
	return schema.VoiceControlGetStateV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandGetState, ID: id}
}

// command sends one command to a helper that has accepted its start and is
// not stopping.
func (s *Supervisor) command(ctx context.Context, kind schema.VoiceControlCommandKind, build func(int64) schema.VoiceControlMessage) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	r := s.cur
	if r == nil || !r.ready || r.stopping || r.broken || s.status.Phase == PhaseStopping {
		s.mu.Unlock()
		return ErrNotRunning
	}
	s.mu.Unlock()
	return s.exchange(ctx, r, kind, build)
}

// Status is the latest snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status.clone()
}

// Updates delivers each new Status. It holds one, the latest: a reader that
// falls behind misses only snapshots that were already out of date, and the
// supervisor never waits for it.
func (s *Supervisor) Updates() <-chan Status { return s.updates }

// Diagnostics is the last lines of the current (or last) helper's log, and
// the supervisor's own notes about it, sanitized for a terminal.
func (s *Supervisor) Diagnostics() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.diag)
}

// HelperPath is the helper this supervisor last found and tried to start, its
// links resolved: what ran, or what could not be started, for a status view.
// It is "" until a helper has been found; one that was not found or not
// trusted leaves it as it was.
func (s *Supervisor) HelperPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// publishLocked offers the status to Updates, replacing one not yet read.
// Every send happens under s.mu, so after the drain there is room.
func (s *Supervisor) publishLocked() {
	st := s.status.clone()
	select {
	case <-s.updates:
	default:
	}
	select {
	case s.updates <- st:
	default:
	}
}

// addDiagLocked keeps line among the last diagLines.
func (s *Supervisor) addDiagLocked(line string) {
	if line == "" {
		return
	}
	s.diag = append(s.diag, line)
	if len(s.diag) > diagLines {
		s.diag = slices.Clone(s.diag[len(s.diag)-diagLines:])
	}
}

// noteLocked records something the supervisor saw about r.
func (s *Supervisor) noteLocked(r *run, text string) {
	if s.cur == r {
		s.addDiagLocked(sanitizeDiag([]byte("conch: "+text), s.secret()))
	}
}

// secret is the token to keep out of Diagnostics: any the profile holds,
// whether or not it is passed on.
func (s *Supervisor) secret() string { return s.profile.Token }
