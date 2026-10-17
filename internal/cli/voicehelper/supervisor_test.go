package voicehelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/njdaniel/conch/pkg/schema"
)

const (
	// The transcripts' server, channel and user, so that the supervisor's
	// commands are byte for byte the transcripts' own.
	testServer    = "https://conch.example.org"
	testChannel   = "general"
	testPrincipal = 7
	// fakeToken is one of sensitive.json's fake secrets.
	fakeToken = "conch_FAKE_login_token_do_not_print" // #nosec G101 -- a fake secret of sensitive.json
)

// stopTimings shorten the stop's bounds for the tests that wait them out.
var stopTimings = Timings{StopTerm: 400 * time.Millisecond, StopKill: 1200 * time.Millisecond}

func storedProfile(t *testing.T) Profile {
	return Profile{Server: testServer, ConfigDir: t.TempDir(), PrincipalID: testPrincipal, Credential: schema.VoiceControlCredentialStored}
}

// newSupervisor makes a supervisor of the fake helper, scripted by args, and
// closes it when the test ends, checking that nothing is left running.
func newSupervisor(t *testing.T, p Profile, timings Timings, args ...string) *Supervisor {
	t.Helper()
	s := New(p, Options{HelperPath: fakePath, TestArgs: append([]string{"-fixtures", fixtureDir}, args...), Timings: timings})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("close: %v", err)
		}
		if s.current() != nil {
			t.Error("a helper is still running after Close")
		}
	})
	return s
}

func (s *Supervisor) current() *run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *Supervisor) lastExit() exitInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// waitFor waits for the status to satisfy cond.
func waitFor(t *testing.T, s *Supervisor, what string, cond func(Status) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := s.Status()
		if cond(st) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: status %+v, diagnostics %q", what, st, s.Diagnostics())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitInside waits for cond on the supervisor's own state.
func waitInside(t *testing.T, s *Supervisor, what string, cond func(*Supervisor) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s.mu.Lock()
		ok := cond(s)
		s.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: status %+v, diagnostics %q", what, s.Status(), s.Diagnostics())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitReaped(t *testing.T, s *Supervisor) {
	t.Helper()
	waitInside(t, s, "the helper to be reaped", func(s *Supervisor) bool { return s.cur == nil })
}

// seqAtLeast waits until the supervisor has read the event with seq n.
func seqAtLeast(t *testing.T, s *Supervisor, n int64) {
	t.Helper()
	waitInside(t, s, fmt.Sprintf("seq %d", n), func(s *Supervisor) bool { return s.cur != nil && s.cur.tr.lastSeq >= n })
}

// assertNoMismatch: the fake found nothing wrong in what the supervisor did.
func assertNoMismatch(t *testing.T, s *Supervisor) {
	t.Helper()
	for _, line := range s.Diagnostics() {
		if strings.Contains(line, mismatch) {
			t.Errorf("the fake objected: %s", line)
		}
	}
}

func reasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// assertGone checks that pid has been reaped: a zombie would still answer.
func assertGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d still exists (signal 0: %v): it was not reaped", pid, err)
	}
}

func exitCode(t *testing.T, s *Supervisor) int {
	t.Helper()
	info := s.lastExit()
	if info.state == nil {
		t.Fatal("no helper has been reaped")
	}
	return info.state.ExitCode()
}

// privateDir is a temporary directory the trust checks accept, parents and
// all. (t.TempDir is under /tmp, which everyone can write to, and is 0775
// under a umask of 002.)
func privateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(safeBase, "t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestNewStartsNothing(t *testing.T) {
	dumpFile := filepath.Join(t.TempDir(), "dump")
	s := newSupervisor(t, storedProfile(t), Timings{}, "-dump", dumpFile)
	ctx := ctxFor(t)
	if st := s.Status(); st.Phase != PhaseIdle || st.Gen != 0 {
		t.Fatalf("status %+v, want idle and no helper", st)
	}
	for name, err := range map[string]error{
		"set_mute":   s.SetMute(ctx, true),
		"set_deafen": s.SetDeafen(ctx, true),
		"refresh":    s.Refresh(ctx),
		"leave":      s.Leave(ctx),
	} {
		if !errors.Is(err, ErrNotRunning) {
			t.Errorf("%s: %v, want ErrNotRunning", name, err)
		}
	}
	if err := s.Retry(ctx); !errors.Is(err, ErrNoRetry) {
		t.Errorf("retry: %v, want ErrNoRetry", err)
	}
	if _, err := os.Stat(dumpFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a helper was started: %v", err)
	}
}

func TestJoinRefusesBeforeStartingAnything(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*Profile)
		channel string
		want    error
	}{
		{name: "no credential", change: func(p *Profile) { p.Credential = "" }, want: ErrNoCredential},
		{name: "an unknown credential source", change: func(p *Profile) { p.Credential = "keyring" }, want: ErrNoCredential},
		{name: "env with no token", change: func(p *Profile) { p.Credential = schema.VoiceControlCredentialEnv }, want: ErrNoCredential},
		{name: "env with a blank token", change: func(p *Profile) { p.Credential, p.Token = schema.VoiceControlCredentialEnv, " \t" }, want: ErrNoCredential},
		{name: "a relative configuration directory", change: func(p *Profile) { p.ConfigDir = "conch" }},
		{name: "a server that is not normalized", change: func(p *Profile) { p.Server = "https://Conch.example.org/" }},
		{name: "no principal", change: func(p *Profile) { p.PrincipalID = 0 }},
		{name: "a blank channel", channel: " "},
		{name: "a channel with a control character", channel: "gen\x1beral"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := storedProfile(t)
			if tt.change != nil {
				tt.change(&p)
			}
			dumpFile := filepath.Join(t.TempDir(), "dump")
			s := newSupervisor(t, p, Timings{}, "-dump", dumpFile)
			channel := tt.channel
			if channel == "" {
				channel = testChannel
			}
			err := s.Join(ctxFor(t), channel)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("Join: %v, want %v", err, tt.want)
			}
			if st := s.Status(); st.Phase != PhaseIdle || st.Gen != 0 || s.current() != nil {
				t.Errorf("status %+v after a refused join, want idle with no helper", st)
			}
			if _, err := os.Stat(dumpFile); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a helper was started: %v", err)
			}
		})
	}
}

// TestTranscriptsEndToEnd replays the helper side of each transcript the
// supervisor can produce the owner side of, and the fake holds the
// supervisor's commands to the transcript byte for byte.
func TestTranscriptsEndToEnd(t *testing.T) {
	tests := []struct {
		file     string
		timings  Timings
		exit     int
		drive    func(t *testing.T, ctx context.Context, s *Supervisor)
		wantIdle bool
	}{
		{
			file: "transcript-join-mute-leave.json", wantIdle: true,
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				if err := s.Join(ctx, testChannel); err != nil {
					t.Fatalf("join: %v", err)
				}
				if err := s.SetMute(ctx, true); err != nil {
					t.Fatalf("mute: %v", err)
				}
				// The state a command changed comes before its ack.
				if st := s.Status(); !st.Muted || !slices.Contains(st.Blocked, schema.VoiceControlBlockMuted) {
					t.Fatalf("after the ack of set_mute true: %+v", st)
				}
				for i, step := range []func() error{
					func() error { return s.SetMute(ctx, true) },
					func() error { return s.SetDeafen(ctx, true) },
					func() error { return s.SetDeafen(ctx, false) },
					func() error { return s.Refresh(ctx) },
				} {
					if err := step(); err != nil {
						t.Fatalf("step %d: %v", i, err)
					}
				}
				if st := s.Status(); st.Phase != PhaseConnected || !st.Muted || st.Deafened || st.Capture != schema.VoiceControlCaptureOn {
					t.Fatalf("before leave: %+v", st)
				}
				if err := s.Leave(ctx); err != nil {
					t.Fatalf("leave: %v", err)
				}
			},
		},
		{
			file: "transcript-gap-resync.json", wantIdle: true,
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				if err := s.Join(ctx, testChannel); err != nil {
					t.Fatalf("join: %v", err)
				}
				// The gap at seq 9 makes the supervisor ask for a snapshot
				// itself: seq 11 is the ack of that get_state.
				seqAtLeast(t, s, 11)
				if st := s.Status(); !st.Muted {
					t.Fatalf("after the resync: %+v", st)
				}
				if err := s.Leave(ctx); err != nil {
					t.Fatalf("leave: %v", err)
				}
			},
		},
		{
			file: "transcript-owner-lost.json", wantIdle: true,
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				if err := s.Join(ctx, testChannel); err != nil {
					t.Fatalf("join: %v", err)
				}
				seqAtLeast(t, s, 6)
				if err := s.Leave(ctx); err != nil {
					t.Fatalf("leave: %v", err)
				}
			},
		},
		{
			file: "transcript-signal.json", timings: stopTimings, wantIdle: true,
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				if err := s.Join(ctx, testChannel); err != nil {
					t.Fatalf("join: %v", err)
				}
				seqAtLeast(t, s, 5)
				if err := s.Leave(ctx); err != nil {
					t.Fatalf("leave: %v", err)
				}
				if info := s.lastExit(); !info.sentTerm || info.sentKill || info.stopTook < stopTimings.StopTerm {
					t.Errorf("exit %+v: want SIGTERM at the bound and no SIGKILL", info)
				}
			},
		},
		{
			file: "transcript-version-mismatch.json",
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				if err := s.Join(ctx, testChannel); reasonOf(err) != ReasonHelperIncompatible {
					t.Fatalf("join: %v, want %s", err, ReasonHelperIncompatible)
				}
				waitReaped(t, s)
				if st := s.Status(); st.Phase != PhaseFailed || st.Reason != ReasonHelperIncompatible || st.Retryable {
					t.Errorf("status %+v", st)
				}
			},
		},
		{
			file: "transcript-identity-mismatch.json", exit: 1,
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				// The failure may already be known when Join returns.
				if err := s.Join(ctx, testChannel); err != nil && reasonOf(err) != "identity_mismatch" {
					t.Fatalf("join: %v", err)
				}
				waitFor(t, s, "identity_mismatch", func(st Status) bool { return st.Reason == "identity_mismatch" })
				waitReaped(t, s)
				if st := s.Status(); st.Phase != PhaseFailed || st.Retryable || st.Capture != schema.VoiceControlCaptureOff {
					t.Errorf("status %+v", st)
				}
				if err := s.Retry(ctx); !errors.Is(err, ErrNoRetry) {
					t.Errorf("retry after identity_mismatch: %v, want ErrNoRetry", err)
				}
			},
		},
		{
			file: "transcript-rotation-then-duplicate.json", exit: 1,
			drive: func(t *testing.T, ctx context.Context, s *Supervisor) {
				if err := s.Join(ctx, testChannel); err != nil && reasonOf(err) != "duplicate_identity" {
					t.Fatalf("join: %v", err)
				}
				waitFor(t, s, "duplicate_identity", func(st Status) bool { return st.Reason == "duplicate_identity" })
				waitReaped(t, s)
				st := s.Status()
				if st.Phase != PhaseFailed || !st.Retryable {
					t.Errorf("status %+v", st)
				}
				// The server's rule stands: no second attempt by itself.
				time.Sleep(200 * time.Millisecond)
				if now := s.Status(); now.Gen != 1 || s.current() != nil {
					t.Errorf("a helper was started again by itself: %+v", now)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			s := newSupervisor(t, storedProfile(t), tt.timings, "-replay", filepath.Join(fixtureDir, tt.file))
			ctx := ctxFor(t)
			tt.drive(t, ctx, s)
			waitReaped(t, s)
			assertNoMismatch(t, s)
			if got := exitCode(t, s); got != tt.exit {
				t.Errorf("the helper exited %d, want %d (diagnostics %q)", got, tt.exit, s.Diagnostics())
			}
			if tt.wantIdle {
				if st := s.Status(); st.Phase != PhaseIdle {
					t.Errorf("after leave: %+v, want idle", st)
				}
			}
			assertGone(t, s.lastExit().pid)
		})
	}
}

func TestHandshakeFailures(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		timings  Timings
		reason   string
		killed   bool
		exitZero bool
	}{
		{name: "no hello in time", args: []string{"-no-hello"}, timings: Timings{Hello: 400 * time.Millisecond}, reason: ReasonHelloTimeout, killed: true},
		{name: "exits before its hello", args: []string{"-exit-before-hello", "2"}, reason: ReasonHelperIncompatible},
		{name: "a state before its hello", args: []string{"-state-first"}, reason: ReasonBadEvent, killed: true},
		{name: "a hello without v1", args: []string{"-versions", "conch.voice_control.v2"}, reason: ReasonHelperIncompatible, exitZero: true},
		{name: "no ack for the start", args: []string{"-no-ack-start"}, timings: Timings{Ack: 400 * time.Millisecond}, reason: ReasonAckTimeout, exitZero: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSupervisor(t, storedProfile(t), tt.timings, tt.args...)
			bound := s.t.Hello + s.t.Ack + time.Second
			began := time.Now()
			err := s.Join(ctxFor(t), testChannel)
			if took := time.Since(began); took > bound {
				t.Errorf("Join took %v, more than hello and ack (%v)", took, bound)
			}
			if reasonOf(err) != tt.reason {
				t.Fatalf("Join: %v, want %s", err, tt.reason)
			}
			waitReaped(t, s)
			st := s.Status()
			if st.Phase != PhaseFailed || st.Reason != tt.reason || st.Retryable != Retryable(tt.reason) || st.Capture != schema.VoiceControlCaptureOff {
				t.Errorf("status %+v", st)
			}
			info := s.lastExit()
			if info.sentKill != tt.killed {
				t.Errorf("SIGKILL sent: %v, want %v", info.sentKill, tt.killed)
			}
			if tt.exitZero && info.state.ExitCode() != 0 {
				t.Errorf("the helper exited %d, want 0", info.state.ExitCode())
			}
			assertNoMismatch(t, s)
			assertGone(t, info.pid)
		})
	}
}

// TestBadEvents: anything the owner's reader refuses stops the helper and is
// reported as bad_event, whatever it was.
func TestBadEvents(t *testing.T) {
	for _, after := range []string{"oversized", "unending", "native", "seq-repeat", "second-hello", "after-final", "ack-wrong-id"} {
		t.Run(after, func(t *testing.T) {
			s := newSupervisor(t, storedProfile(t), stopTimings, "-after", after)
			ctx := ctxFor(t)
			// Join may already see the failure, or, after a final state,
			// the reason that state gave.
			if err := s.Join(ctx, testChannel); err != nil && reasonOf(err) != ReasonBadEvent && reasonOf(err) != "internal_error" {
				t.Fatalf("join: %v", err)
			}
			if after == "ack-wrong-id" {
				if err := s.SetMute(ctx, true); reasonOf(err) != ReasonBadEvent {
					t.Fatalf("set_mute answered by an ack for another id: %v, want %s", err, ReasonBadEvent)
				}
			}
			waitFor(t, s, "bad_event", func(st Status) bool { return st.Reason == ReasonBadEvent })
			waitReaped(t, s)
			st := s.Status()
			if st.Phase != PhaseFailed || !st.Retryable || st.Capture != schema.VoiceControlCaptureOff || st.Transmitting {
				t.Errorf("status %+v", st)
			}
			if !slices.ContainsFunc(s.Diagnostics(), func(l string) bool { return strings.Contains(l, "bad event") }) {
				t.Errorf("diagnostics %q do not say why", s.Diagnostics())
			}
			assertGone(t, s.lastExit().pid)
		})
	}
}

func TestUnknownEventKindIsSkipped(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), Timings{}, "-after", "unknown-kind")
	ctx := ctxFor(t)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMute(ctx, true); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.Phase != PhaseConnected || !st.Muted {
		t.Fatalf("status %+v", st)
	}
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoMismatch(t, s)
}

// TestGapAsksForSnapshot: events lost to a slow reader are made up by asking
// for a snapshot; only the snapshot shows the deafen key the fake pressed.
func TestGapAsksForSnapshot(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), Timings{}, "-after", "gap")
	ctx := ctxFor(t)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "the snapshot", func(st Status) bool { return st.Muted && st.Deafened })
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoMismatch(t, s)
}

// TestNativeOutputIsIgnored: standard output is drained and never read as
// protocol, even a line that would be a valid event.
func TestNativeOutputIsIgnored(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), Timings{}, "-after", "noise")
	ctx := ctxFor(t)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMute(ctx, true); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.Phase != PhaseConnected || st.Transmitting || !st.Muted {
		t.Fatalf("status %+v", st)
	}
	for _, l := range s.Diagnostics() {
		if strings.Contains(l, "NVIDIA") {
			t.Errorf("native output reached the diagnostics: %q", l)
		}
	}
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsTail(t *testing.T) {
	t.Run("a token passed on", func(t *testing.T) {
		p := storedProfile(t)
		p.Credential, p.Token = schema.VoiceControlCredentialEnv, fakeToken
		checkDiagnosticsTail(t, p)
	})
	t.Run("a stored login whose token the TUI also holds", func(t *testing.T) {
		p := storedProfile(t)
		p.Token = fakeToken
		if err := os.WriteFile(filepath.Join(p.ConfigDir, "fake-stored-login"), []byte(fakeToken), 0o600); err != nil {
			t.Fatal(err)
		}
		checkDiagnosticsTail(t, p)
	})
}

func checkDiagnosticsTail(t *testing.T, p Profile) {
	s := newSupervisor(t, p, Timings{}, "-after", "log-spam")
	ctx := ctxFor(t)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	waitInside(t, s, "the last log line", func(s *Supervisor) bool {
		return len(s.diag) > 0 && s.diag[len(s.diag)-1] == "fake: last line"
	})
	diag := s.Diagnostics()
	if len(diag) != diagLines {
		t.Errorf("%d lines kept, want %d", len(diag), diagLines)
	}
	redactions := 0
	for _, l := range diag {
		if len(l) > diagLineBytes {
			t.Errorf("a line of %d bytes: %q", len(l), l)
		}
		if strings.ContainsFunc(l, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
			t.Errorf("a control or formatting character in %q", l)
		}
		if strings.Contains(l, fakeToken) {
			t.Errorf("the token in %q", l)
		}
		redactions += strings.Count(l, redacted)
	}
	if redactions == 0 {
		t.Errorf("no line shows the token replaced: %q", diag)
	}
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenPipes(t *testing.T) {
	t.Run("the helper closes its standard input", func(t *testing.T) {
		s := newSupervisor(t, storedProfile(t), Timings{Ack: time.Second, StopTerm: stopTimings.StopTerm, StopKill: stopTimings.StopKill}, "-after", "close-stdin")
		ctx := ctxFor(t)
		if err := s.Join(ctx, testChannel); err != nil {
			t.Fatal(err)
		}
		waitFor(t, s, "connected", func(st Status) bool { return st.Phase == PhaseConnected })
		time.Sleep(100 * time.Millisecond)
		if err := s.SetMute(ctx, true); err == nil {
			t.Fatal("set_mute to a helper that closed its input succeeded")
		}
		waitReaped(t, s)
		if st := s.Status(); st.Phase != PhaseFailed || (st.Reason != ReasonHelperExited && st.Reason != ReasonAckTimeout) {
			t.Errorf("status %+v", st)
		}
		if err := s.SetMute(ctx, false); !errors.Is(err, ErrNotRunning) {
			t.Errorf("a command after it was reaped: %v, want ErrNotRunning", err)
		}
	})
	t.Run("the helper closes its event descriptor", func(t *testing.T) {
		s := newSupervisor(t, storedProfile(t), stopTimings, "-after", "close-events")
		// The end may already be seen when Join returns.
		if err := s.Join(ctxFor(t), testChannel); err != nil && reasonOf(err) != ReasonHelperExited {
			t.Fatal(err)
		}
		waitReaped(t, s)
		if st := s.Status(); st.Phase != PhaseFailed || st.Reason != ReasonHelperExited || !st.Retryable {
			t.Errorf("status %+v", st)
		}
		if !s.lastExit().sentTerm {
			t.Error("a helper that stopped reporting was not stopped")
		}
	})
}

// TestCrashes: a helper that ends without a final state leaves voice failed
// and retryable, and nothing starts again by itself.
func TestCrashes(t *testing.T) {
	tests := []struct {
		after  string
		reason string
	}{
		{after: "crash", reason: ReasonHelperCrashed},
		{after: "exit101", reason: ReasonHelperCrashed},
		{after: "exit-silently", reason: ReasonHelperExited},
	}
	for _, tt := range tests {
		t.Run(tt.after, func(t *testing.T) {
			s := newSupervisor(t, storedProfile(t), Timings{}, "-after", tt.after)
			ctx := ctxFor(t)
			// Join may already see the end, before or after it is reaped.
			if err := s.Join(ctx, testChannel); err != nil && reasonOf(err) != ReasonHelperExited && reasonOf(err) != tt.reason {
				t.Fatalf("join: %v", err)
			}
			waitReaped(t, s)
			st := s.Status()
			if st.Phase != PhaseFailed || st.Reason != tt.reason || !st.Retryable || st.Capture != schema.VoiceControlCaptureOff {
				t.Errorf("status %+v, want failed %s", st, tt.reason)
			}
			time.Sleep(200 * time.Millisecond)
			if s.current() != nil || s.Status().Gen != 1 {
				t.Error("a helper was started again by itself")
			}
			if err := s.SetMute(ctx, true); !errors.Is(err, ErrNotRunning) {
				t.Errorf("set_mute: %v, want ErrNotRunning", err)
			}
			assertGone(t, s.lastExit().pid)
		})
	}
}

// TestStopEscalation: leave and standard input closed; SIGTERM at StopTerm;
// SIGKILL at StopKill; and in each case the process reaped.
func TestStopEscalation(t *testing.T) {
	tests := []struct {
		onStop     string
		term, kill bool
		atLeast    time.Duration
	}{
		{onStop: "normal"},
		{onStop: "hang-until-term", term: true, atLeast: stopTimings.StopTerm},
		{onStop: "ignore-term", term: true, kill: true, atLeast: stopTimings.StopKill},
	}
	for _, tt := range tests {
		t.Run(tt.onStop, func(t *testing.T) {
			s := newSupervisor(t, storedProfile(t), stopTimings, "-on-stop", tt.onStop)
			ctx := ctxFor(t)
			if err := s.Join(ctx, testChannel); err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			if err := s.Leave(ctx); err != nil {
				t.Fatal(err)
			}
			if took := time.Since(began); took > stopTimings.StopKill+drainGrace+time.Second {
				t.Errorf("leave took %v", took)
			}
			info := s.lastExit()
			if info.sentTerm != tt.term || info.sentKill != tt.kill {
				t.Errorf("SIGTERM %v, SIGKILL %v; want %v, %v", info.sentTerm, info.sentKill, tt.term, tt.kill)
			}
			if info.stopTook < tt.atLeast || (!tt.term && info.stopTook >= stopTimings.StopTerm) {
				t.Errorf("exited %v after the stop began", info.stopTook)
			}
			if st := s.Status(); st.Phase != PhaseIdle {
				t.Errorf("status %+v, want idle", st)
			}
			assertGone(t, info.pid)
		})
	}
}

func TestRepeatedJoinAndLeave(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), Timings{})
	ctx := ctxFor(t)
	for i := 1; i <= 4; i++ {
		if err := s.Join(ctx, testChannel); err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		if err := s.Join(ctx, testChannel); !errors.Is(err, ErrBusy) {
			t.Fatalf("a second join while running: %v, want ErrBusy", err)
		}
		on := i%2 == 1
		if err := s.SetMute(ctx, on); err != nil {
			t.Fatalf("mute %d: %v", i, err)
		}
		if st := s.Status(); st.Gen != uint64(i) || st.Muted != on {
			t.Fatalf("round %d: %+v", i, st)
		}
		if err := s.Leave(ctx); err != nil {
			t.Fatalf("leave %d: %v", i, err)
		}
		if err := s.Leave(ctx); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("a second leave: %v, want ErrNotRunning", err)
		}
		info := s.lastExit()
		if info.gen != uint64(i) {
			t.Fatalf("round %d reaped helper %d", i, info.gen)
		}
		assertGone(t, info.pid)
	}
}

// TestJoinIsBusyUntilReaped: no helper starts until the one before has been
// reaped.
func TestJoinIsBusyUntilReaped(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), stopTimings, "-on-stop", "hang-until-term")
	ctx := ctxFor(t)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	left := make(chan error, 1)
	go func() { left <- s.Leave(ctx) }()
	waitInside(t, s, "the stop", func(s *Supervisor) bool { return s.cur != nil && s.cur.stopping })
	if err := s.Join(ctx, testChannel); !errors.Is(err, ErrBusy) {
		t.Fatalf("join while stopping: %v, want ErrBusy", err)
	}
	if err := s.SetMute(ctx, true); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("mute while stopping: %v, want ErrNotRunning", err)
	}
	if err := <-left; err != nil {
		t.Fatal(err)
	}
	first := s.lastExit()
	assertGone(t, first.pid)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatalf("join once reaped: %v", err)
	}
	if st := s.Status(); st.Gen != 2 {
		t.Fatalf("status %+v", st)
	}
}

func TestLeaveDuringJoin(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), Timings{Hello: 10 * time.Second, StopTerm: stopTimings.StopTerm, StopKill: stopTimings.StopKill}, "-no-hello")
	ctx := ctxFor(t)
	joined := make(chan error, 1)
	go func() { joined <- s.Join(ctx, testChannel) }()
	waitInside(t, s, "the start", func(s *Supervisor) bool { return s.cur != nil })
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-joined; !errors.Is(err, ErrStopped) {
		t.Fatalf("join: %v, want ErrStopped", err)
	}
	if st := s.Status(); st.Phase != PhaseIdle {
		t.Fatalf("status %+v, want idle", st)
	}
	assertGone(t, s.lastExit().pid)
}

func TestJoinContextEnds(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), stopTimings, "-no-hello")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.Join(ctx, testChannel); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("join: %v, want the context's error", err)
	}
	waitReaped(t, s)
	if st := s.Status(); st.Phase != PhaseIdle {
		t.Fatalf("status %+v, want idle", st)
	}
	assertGone(t, s.lastExit().pid)
}

func TestCloseRefusesEverything(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), Timings{})
	ctx := ctxFor(t)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMute(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.Phase != PhaseIdle {
		t.Fatalf("status %+v, want idle", st)
	}
	s.mu.Lock()
	kept := s.muted || s.deafened
	s.mu.Unlock()
	if kept {
		t.Error("the mute kept for a retry outlived Close")
	}
	assertGone(t, s.lastExit().pid)
	for name, err := range map[string]error{
		"join":       s.Join(ctx, testChannel),
		"retry":      s.Retry(ctx),
		"leave":      s.Leave(ctx),
		"set_mute":   s.SetMute(ctx, false),
		"set_deafen": s.SetDeafen(ctx, false),
		"refresh":    s.Refresh(ctx),
		"close":      s.Close(ctx),
	} {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close: %v, want ErrClosed", name, err)
		}
	}
}

// TestRetry: a retry is a new process with a new handshake, for the same
// channel, with the mute and deafen last confirmed.
func TestRetry(t *testing.T) {
	dumpFile := filepath.Join(t.TempDir(), "dump")
	s := newSupervisor(t, storedProfile(t), Timings{}, "-dump", dumpFile, "-fail-on", "get_state:duplicate_identity")
	ctx := ctxFor(t)
	if err := s.Retry(ctx); !errors.Is(err, ErrNoRetry) {
		t.Fatalf("retry before anything: %v", err)
	}
	if err := s.Join(ctx, "voice-room"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMute(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeafen(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx); err == nil {
		t.Fatal("refresh of a helper that failed instead of answering succeeded")
	}
	waitReaped(t, s)
	if st := s.Status(); st.Phase != PhaseFailed || st.Reason != "duplicate_identity" || !st.Retryable {
		t.Fatalf("status %+v", st)
	}
	if err := s.Retry(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	st := s.Status()
	if st.Gen != 2 || st.Channel != "voice-room" || !st.Muted || !st.Deafened {
		t.Fatalf("after retry: %+v", st)
	}
	cmd, err := schema.ParseVoiceControlCommand([]byte(readDump(t, dumpFile).one("start")))
	if err != nil || cmd.Start == nil || !cmd.Start.Muted || !cmd.Start.Deafened || cmd.Start.Channel != "voice-room" {
		t.Fatalf("the retry's start: %+v, %v", cmd.Start, err)
	}
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(ctx); !errors.Is(err, ErrNoRetry) {
		t.Fatalf("retry after leave: %v, want ErrNoRetry", err)
	}
}

// TestInFlightBound: however many callers there are, at most
// VoiceControlMaxInFlight commands are unanswered (the fake counts).
func TestInFlightBound(t *testing.T) {
	s := newSupervisor(t, storedProfile(t), Timings{}, "-ack-delay", "30ms", "-max-in-flight", fmt.Sprint(schema.VoiceControlMaxInFlight))
	ctx := ctxFor(t)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3*schema.VoiceControlMaxInFlight)
	for i := 0; i < 3*schema.VoiceControlMaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.SetMute(ctx, i%2 == 0)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoMismatch(t, s)
}

func TestRefusedCommands(t *testing.T) {
	t.Run("a refusal of a valid command stops the helper", func(t *testing.T) {
		s := newSupervisor(t, storedProfile(t), stopTimings, "-refuse", "set_mute:invalid")
		ctx := ctxFor(t)
		if err := s.Join(ctx, testChannel); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMute(ctx, true); reasonOf(err) != ReasonHelperIncompatible {
			t.Fatalf("set_mute: %v, want %s", err, ReasonHelperIncompatible)
		}
		waitReaped(t, s)
		if st := s.Status(); st.Phase != PhaseFailed || st.Reason != ReasonHelperIncompatible {
			t.Errorf("status %+v", st)
		}
		if !slices.ContainsFunc(s.Diagnostics(), func(l string) bool { return strings.Contains(l, "refused set_mute with invalid") }) {
			t.Errorf("diagnostics %q", s.Diagnostics())
		}
	})
	t.Run("stopping is not a fault", func(t *testing.T) {
		s := newSupervisor(t, storedProfile(t), Timings{}, "-refuse", "set_mute:stopping")
		ctx := ctxFor(t)
		if err := s.Join(ctx, testChannel); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMute(ctx, true); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("set_mute: %v, want ErrNotRunning", err)
		}
		if err := s.Refresh(ctx); err != nil {
			t.Fatalf("the helper was stopped: %v", err)
		}
		if st := s.Status(); st.Phase != PhaseConnected {
			t.Errorf("status %+v", st)
		}
	})
}

// TestTwoSupervisors: two owners, each with its own profile, cannot see,
// stop or reconfigure each other's helper.
func TestTwoSupervisors(t *testing.T) {
	dir := t.TempDir()
	pa, pb := storedProfile(t), storedProfile(t)
	pb.Credential, pb.Token, pb.PrincipalID = schema.VoiceControlCredentialEnv, fakeToken, 8
	dumpA, dumpB := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	a := newSupervisor(t, pa, Timings{}, "-dump", dumpA)
	b := newSupervisor(t, pb, Timings{}, "-dump", dumpB)
	ctx := ctxFor(t)
	if err := a.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	da, db := readDump(t, dumpA), readDump(t, dumpB)
	if da.one("pid") == db.one("pid") {
		t.Fatal("one process for two owners")
	}
	ea, eb := da.env(), db.env()
	if ea[configDirEnv] != pa.ConfigDir || eb[configDirEnv] != pb.ConfigDir {
		t.Errorf("configuration directories %q and %q, want %q and %q", ea[configDirEnv], eb[configDirEnv], pa.ConfigDir, pb.ConfigDir)
	}
	if _, ok := ea[tokenEnv]; ok || eb[tokenEnv] != fakeToken {
		t.Error("each helper must have only its own owner's credential")
	}

	if err := a.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	assertGone(t, a.lastExit().pid)
	if err := b.SetMute(ctx, true); err != nil {
		t.Fatalf("the other owner's helper was disturbed: %v", err)
	}
	if st := b.Status(); st.Phase != PhaseConnected || !st.Muted || st.Gen != 1 {
		t.Fatalf("the other owner's status: %+v", st)
	}
	if err := a.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Refresh(ctx); err != nil {
		t.Fatalf("closing one owner stopped the other's helper: %v", err)
	}
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestHelperEnvironmentAndProcess checks the process the supervisor starts:
// argv, environment (§9), working directory, process group and descriptors.
func TestHelperEnvironmentAndProcess(t *testing.T) {
	inherited := map[string]string{
		"CONCH_TOKEN": "parent-token", "CONCH_SERVER": "https://other.example.org", "CONCH_CONFIG_DIR": "/parent/conch",
		"CONCH_CHANNELS": "a,b", "CONCH_AUTHOR": "3", "CONCH_MOUSE": "1", HelperEnv: "/elsewhere/conch-voice",
		"CONCH_A_LATER_ONE": "x", "RUST_LOG": "trace", "RUST_BACKTRACE": "1", "RUST_LIB_BACKTRACE": "1", "KEEP_THIS": "kept",
	}
	for k, v := range inherited {
		t.Setenv(k, v)
	}
	helper, err := filepath.EvalSymlinks(fakePath)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		change     func(*Profile)
		credential schema.VoiceControlCredential
		token      string
		configDir  bool
	}{
		{name: "a stored login", credential: schema.VoiceControlCredentialStored, configDir: true},
		{name: "CONCH_TOKEN passed on", change: func(p *Profile) { p.Credential, p.Token = schema.VoiceControlCredentialEnv, fakeToken }, credential: schema.VoiceControlCredentialEnv, token: fakeToken, configDir: true},
		{name: "no configuration directory", change: func(p *Profile) { p.ConfigDir = "" }, credential: schema.VoiceControlCredentialStored},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := storedProfile(t)
			if tt.change != nil {
				tt.change(&p)
			}
			dumpFile := filepath.Join(t.TempDir(), "dump")
			s := newSupervisor(t, p, Timings{}, "-dump", dumpFile)
			ctx := ctxFor(t)
			if err := s.Join(ctx, testChannel); err != nil {
				t.Fatal(err)
			}
			d := readDump(t, dumpFile)
			args := d["arg"]
			if len(args) < 2 || args[0] != helper || args[1] != schema.VoiceControlSubcommand {
				t.Errorf("argv %q, want %q then %q", args, helper, schema.VoiceControlSubcommand)
			}
			for _, a := range args {
				if strings.Contains(a, fakeToken) || strings.Contains(a, "parent-token") {
					t.Errorf("a secret in argv: %q", a)
				}
			}
			env := d.env()
			for k, v := range env {
				switch {
				case k == configDirEnv:
					if !tt.configDir || v != p.ConfigDir {
						t.Errorf("%s=%q", k, v)
					}
				case k == tokenEnv:
					if tt.token == "" || v != tt.token {
						t.Errorf("%s is %q, want it only with env, as the owner's own", k, v)
					}
				case strings.HasPrefix(k, "CONCH_") || strings.HasPrefix(k, "RUST_"):
					t.Errorf("inherited %s", k)
				}
			}
			if _, ok := env[configDirEnv]; ok != tt.configDir {
				t.Errorf("%s set: %v, want %v", configDirEnv, ok, tt.configDir)
			}
			if _, ok := env[tokenEnv]; ok != (tt.token != "") {
				t.Errorf("%s set: %v", tokenEnv, ok)
			}
			if env["KEEP_THIS"] != "kept" {
				t.Error("an unrelated variable was dropped")
			}
			if d.one("cwd") != "/" {
				t.Errorf("working directory %q, want /", d.one("cwd"))
			}
			if d.one("pgid") != d.one("pid") {
				t.Errorf("process group %s, want its own (%s)", d.one("pgid"), d.one("pid"))
			}
			seen := map[string]bool{}
			for fd := 0; fd <= 3; fd++ {
				target := d.one(fmt.Sprintf("fd%d", fd))
				if !strings.HasPrefix(target, "pipe:") || seen[target] {
					t.Errorf("fd %d is %q, want a pipe of its own", fd, target)
				}
				seen[target] = true
			}
			cmd, err := schema.ParseVoiceControlCommand([]byte(d.one("start")))
			if err != nil || cmd.Start.Credential != tt.credential || cmd.Start.Server != testServer || cmd.Start.PrincipalID != testPrincipal {
				t.Errorf("start %+v, %v", cmd.Start, err)
			}
			if strings.Contains(d.one("start"), fakeToken) {
				t.Error("the token crossed the control pipe")
			}
			if strings.Contains(fmt.Sprintf("%+v", s.Status()), fakeToken) || strings.Contains(strings.Join(s.Diagnostics(), "\n"), fakeToken) {
				t.Error("the token in the status or the diagnostics")
			}
			if err := s.Leave(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestStartFailuresKeepTheTokenOut: helper_not_found and spawn_failed, with
// a token in the profile that must not appear anywhere.
func TestStartFailuresKeepTheTokenOut(t *testing.T) {
	dir := privateDir(t)
	notAProgram := filepath.Join(dir, "conch-voice")
	if err := os.WriteFile(notAProgram, []byte("not a program\n"), 0o700); err != nil { // #nosec G306 -- an executable the test means to fail to run
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		path   string
		reason string
	}{
		{name: "missing", path: filepath.Join(dir, "missing"), reason: ReasonHelperNotFound},
		{name: "relative", path: "conch-voice", reason: ReasonHelperUntrusted},
		{name: "not a program", path: notAProgram, reason: ReasonSpawnFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := storedProfile(t)
			p.Credential, p.Token = schema.VoiceControlCredentialEnv, fakeToken
			s := New(p, Options{HelperPath: tt.path})
			err := s.Join(ctxFor(t), testChannel)
			if reasonOf(err) != tt.reason {
				t.Fatalf("join: %v, want %s", err, tt.reason)
			}
			st := s.Status()
			if st.Phase != PhaseFailed || st.Reason != tt.reason || st.Gen != 0 || st.Channel != testChannel || st.Retryable != Retryable(tt.reason) {
				t.Errorf("status %+v", st)
			}
			for _, text := range append([]string{err.Error(), fmt.Sprintf("%+v", st)}, s.Diagnostics()...) {
				if strings.Contains(text, fakeToken) {
					t.Errorf("the token in %q", text)
				}
			}
			if s.current() != nil {
				t.Error("a helper is recorded as running")
			}
		})
	}
}
