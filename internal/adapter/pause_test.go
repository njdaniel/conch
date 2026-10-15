package adapter

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// fakeProc stands in for the command's process: it records what an
// intervention did to it and refuses like the real control does.
type fakeProc struct {
	mu     sync.Mutex
	paused bool
	err    error // returned by pause and resume when set
}

func (p *fakeProc) pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.err != nil:
		return p.err
	case p.paused:
		return errAlreadyPaused
	}
	p.paused = true
	return nil
}

func (p *fakeProc) resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.err != nil:
		return p.err
	case !p.paused:
		return errNotPaused
	}
	p.paused = false
	return nil
}

func (p *fakeProc) isPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// Pause and resume reach the command through the run, and the run is reported
// only after the command has been stopped or let go.
func TestPauseAndResumeInterventions(t *testing.T) {
	if !canPauseGroup {
		t.Skip("pause and resume are not declared on this platform")
	}
	pause := func(id int64) schema.InterventionV1 { return intervention(id, 1, schema.InterventionKindPause, "") }
	resume := func(id int64) schema.InterventionV1 { return intervention(id, 1, schema.InterventionKindResume, "") }
	started := []string{"start_run", "update_run:starting", "update_run:running", "activity:command_started"}
	finish := []string{"activity:command_finished", "post_message", "activity:reply_posted", "update_run:completed:replied"}
	tests := []struct {
		name       string
		queued     []schema.InterventionV1
		noProcess  bool  // the command has not started
		procErr    error // what the process control answers
		wantEvents []string
		wantAcks   []ackCall
		wantPaused bool
	}{
		{
			name:   "pause stops the command, then waiting with the outcome paused",
			queued: []schema.InterventionV1{pause(1)},
			wantEvents: []string{
				"ack:1:applied", "update_run:waiting:paused", "activity:intervention_received",
			},
			wantAcks:   []ackCall{{1, schema.InterventionStateApplied, ""}},
			wantPaused: true,
		},
		{
			name:   "resume continues it, reports running, then answers",
			queued: []schema.InterventionV1{pause(1), resume(2)},
			wantEvents: []string{
				"ack:1:applied", "update_run:waiting:paused", "activity:intervention_received",
				"update_run:running", "ack:2:applied", "activity:intervention_received",
			},
			wantAcks: []ackCall{{1, schema.InterventionStateApplied, ""}, {2, schema.InterventionStateApplied, ""}},
		},
		{
			name:       "pause when paused fails and changes nothing",
			queued:     []schema.InterventionV1{pause(1), pause(2)},
			wantEvents: []string{"ack:1:applied", "update_run:waiting:paused", "activity:intervention_received", "ack:2:failed"},
			wantAcks:   []ackCall{{1, schema.InterventionStateApplied, ""}, {2, schema.InterventionStateFailed, "already paused"}},
			wantPaused: true,
		},
		{
			name:       "resume when not paused fails and changes nothing",
			queued:     []schema.InterventionV1{resume(1)},
			wantEvents: []string{"ack:1:failed"},
			wantAcks:   []ackCall{{1, schema.InterventionStateFailed, "not paused"}},
		},
		{
			name:       "pause before the command is running fails",
			queued:     []schema.InterventionV1{pause(1)},
			noProcess:  true,
			wantEvents: []string{"ack:1:failed"},
			wantAcks:   []ackCall{{1, schema.InterventionStateFailed, "not running"}},
		},
		{
			name:       "resume before the command is running fails",
			queued:     []schema.InterventionV1{resume(1)},
			noProcess:  true,
			wantEvents: []string{"ack:1:failed"},
			wantAcks:   []ackCall{{1, schema.InterventionStateFailed, "not running"}},
		},
		{
			name:       "a command that is gone cannot be paused",
			queued:     []schema.InterventionV1{pause(1)},
			procErr:    errNotRunning,
			wantEvents: []string{"ack:1:failed"},
			wantAcks:   []ackCall{{1, schema.InterventionStateFailed, "not running"}},
		},
		{
			name:       "a signal that fails is a fixed reason, not the error",
			queued:     []schema.InterventionV1{pause(1)},
			procErr:    errors.New("operation not permitted"),
			wantEvents: []string{"ack:1:failed"},
			wantAcks:   []ackCall{{1, schema.InterventionStateFailed, "could not signal the command"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{interv: tt.queued, polled: make(chan struct{}, 16)}
			clk := newFakeClock()
			proc := &fakeProc{err: tt.procErr}
			claude := &fakeClaude{fn: func(_ context.Context, _ string, fx fixtureHandler) (string, error) {
				if !tt.noProcess {
					fx.(commandHolder).commandStarted(proc)
				}
				clk.fire(interventionPollInterval)
				<-mcp.polled
				return "done", nil
			}}
			loop := loopWithSession(mcp, claude, clk)
			if err := answer(t, loop, message(1, 2, "q")); err != nil {
				t.Fatal(err)
			}
			want := append(append(append([]string(nil), started...), tt.wantEvents...), finish...)
			if got := mcp.eventLog(); !reflect.DeepEqual(got, want) {
				t.Fatalf("events:\n got %q\nwant %q", got, want)
			}
			mcp.mu.Lock()
			acks := append([]ackCall(nil), mcp.acks...)
			mcp.mu.Unlock()
			if !reflect.DeepEqual(acks, tt.wantAcks) {
				t.Fatalf("acks = %+v, want %+v", acks, tt.wantAcks)
			}
			if proc.isPaused() != tt.wantPaused {
				t.Fatalf("command paused = %v, want %v", proc.isPaused(), tt.wantPaused)
			}
		})
	}
}

// A cancel reaches a paused run: it ends cancelled, and the cancel is answered
// after that.
func TestCancelWhilePaused(t *testing.T) {
	if !canPauseGroup {
		t.Skip("pause and resume are not declared on this platform")
	}
	mcp := &fakeMCP{interv: []schema.InterventionV1{
		intervention(1, 1, schema.InterventionKindPause, ""),
		intervention(2, 1, schema.InterventionKindCancel, ""),
	}, polled: make(chan struct{}, 4)}
	clk := newFakeClock()
	claude := &fakeClaude{fn: func(ctx context.Context, _ string, fx fixtureHandler) (string, error) {
		fx.(commandHolder).commandStarted(&fakeProc{})
		clk.fire(interventionPollInterval)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	loop := loopWithSession(mcp, claude, clk)
	if err := answer(t, loop, message(1, 2, "q")); err != nil {
		t.Fatalf("a cancel is not an error: %v", err)
	}
	want := []string{
		"start_run", "update_run:starting", "update_run:running", "activity:command_started",
		"ack:1:applied", "update_run:waiting:paused", "activity:intervention_received",
		"activity:command_finished", "update_run:cancelled:cancelled by request", "ack:2:applied",
	}
	if got := mcp.eventLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
}

// A command paused for longer than the bound fails the run with the outcome
// that says so.
func TestPausedTooLongFailsTheRun(t *testing.T) {
	mcp := &fakeMCP{}
	claude := &fakeClaude{err: &pausedTooLongError{limit: 2 * time.Minute}}
	loop := loopWithSession(mcp, claude, newFakeClock())
	if err := answer(t, loop, message(1, 2, "q")); err == nil {
		t.Fatal("a run failed by a long pause is a failure to back off on")
	}
	if want := []string{"failed:paused longer than 2m0s"}; !reflect.DeepEqual(mcp.runStates[2:], want) {
		t.Fatalf("run states = %q, want %q after starting and running", mcp.runStates, want)
	}
}

// A pause during an approval wait: the run is waiting already, so nothing is
// reported then or at the decision; the resume puts it back to running.
func TestPauseDuringAnApprovalWait(t *testing.T) {
	if !canPauseGroup {
		t.Skip("pause and resume are not declared on this platform")
	}
	mcp := &fakeMCP{interv: []schema.InterventionV1{intervention(1, 1, schema.InterventionKindPause, "")}, polled: make(chan struct{}, 4)}
	clk := newFakeClock()
	claude := &fakeClaude{fn: func(ctx context.Context, _ string, fx fixtureHandler) (string, error) {
		fx.(commandHolder).commandStarted(&fakeProc{})
		run := fx.(*runFixture).run
		// Hold the approval open, as a wait for a decision does, while the
		// watcher takes the pause.
		run.stateMu.Lock()
		run.inApproval = true
		run.stateMu.Unlock()
		clk.fire(interventionPollInterval)
		<-mcp.polled
		return "done", nil
	}}
	loop := loopWithSession(mcp, claude, clk)
	if err := answer(t, loop, message(1, 2, "q")); err != nil {
		t.Fatal(err)
	}
	for _, s := range mcp.runStates {
		if s == "waiting:paused" {
			t.Fatalf("a run already waiting was reported waiting again: %q", mcp.runStates)
		}
	}
	if len(mcp.acks) != 1 || mcp.acks[0].state != schema.InterventionStateApplied {
		t.Fatalf("acks = %+v", mcp.acks)
	}
}
