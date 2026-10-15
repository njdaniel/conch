package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

// maxTaskRunes is how much of the message a run's task quotes.
const maxTaskRunes = 120

// awaitChunk is how long one await_decision call waits. The wait as a whole is
// bounded by the reply timeout; calls are shorter so that a cancel is noticed.
const awaitChunk = 30 * time.Second

// activeRun is the run of the reply being made, as the intervention watcher
// and the reply share it.
type activeRun struct {
	id        int64
	channelID int64
	// stopCommand ends the command: it cancels the context the runner was
	// given, and the runner kills the command's process group.
	stopCommand context.CancelFunc

	mu          sync.Mutex
	cancelAsked bool
	cancelIDs   []int64 // cancel interventions not yet answered
	proc        processControl

	// stateMu orders the reports that move the run between running and
	// waiting, which the watcher (pause, resume) and the command's reader
	// (an approval) both make. paused and inApproval are guarded by it.
	stateMu    sync.Mutex
	paused     bool
	inApproval bool
}

// commandStarted is what a runner tells the run once its command has a process.
func (f *runFixture) commandStarted(p processControl) {
	f.run.mu.Lock()
	f.run.proc = p
	f.run.mu.Unlock()
}

func (a *activeRun) process() processControl {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proc
}

// requestCancel records a cancel intervention and stops the command. The
// intervention is answered once the run has been reported cancelled, not
// before.
func (a *activeRun) requestCancel(interventionID int64) {
	a.mu.Lock()
	a.cancelAsked = true
	a.cancelIDs = append(a.cancelIDs, interventionID)
	a.mu.Unlock()
	a.stopCommand()
}

func (a *activeRun) wasCancelled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancelAsked
}

func (a *activeRun) takeCancelIDs() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := a.cancelIDs
	a.cancelIDs = nil
	return ids
}

// runTask is the one line a run states as its task: the start of the message
// it answers, with line breaks and anything unprintable squeezed out.
func runTask(body string) string {
	oneLine := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsPrint(r):
			return r
		default:
			return -1
		}
	}, body)
	words := strings.Join(strings.Fields(oneLine), " ")
	if r := []rune(words); len(r) > maxTaskRunes {
		words = strings.TrimSpace(string(r[:maxTaskRunes]))
	}
	if words == "" {
		return "reply to a message"
	}
	return words
}

// reportContext is the context for telling the server about a run. It outlives
// the one that stopped the adapter, because a stopped adapter must still say
// so.
func reportContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
}

// beginRun starts the run for a reply and moves it to running. Telling the
// server about a run is not what the adapter is for: if that fails, the reply
// is made without one.
func (b *botLoop) beginRun(ctx context.Context, stopCommand context.CancelFunc, g *audienceGroup, human []schema.MessageV2) *activeRun {
	newest := human[len(human)-1]
	rctx, cancel := reportContext(ctx)
	defer cancel()
	run, err := b.mcp.StartRun(rctx, b.sessionID, schema.CreateRunRequestV1{
		Channel:   b.cfg.Channel,
		MessageID: newest.ID,
		Task:      runTask(newest.Body),
	})
	if err != nil {
		var toolErr *mcpclient.ToolError
		if errors.As(err, &toolErr) && toolErr.Code == "channel_not_found" {
			// Expected: the agent may read this audience (a net it only
			// monitors) but not start a run in it. Say so once, quietly.
			if !b.noRunAudiences[g.key] {
				if b.noRunAudiences == nil {
					b.noRunAudiences = map[string]bool{}
				}
				b.noRunAudiences[g.key] = true
				slog.Debug("conch-adapter: the server does not allow a run for this audience; replying without one", "audience", g.key)
			}
			return nil
		}
		logReportError("start_run", err)
		return nil
	}
	active := &activeRun{id: run.ID, channelID: run.ChannelID, stopCommand: stopCommand}
	b.setState(ctx, active, schema.RunStateStarting, "")
	b.setState(ctx, active, schema.RunStateRunning, "")
	return active
}

// setState moves the run to a state. It does nothing without a run.
func (b *botLoop) setState(ctx context.Context, run *activeRun, state schema.RunState, outcome string) {
	if run == nil {
		return
	}
	rctx, cancel := reportContext(ctx)
	defer cancel()
	if _, err := b.mcp.UpdateRun(rctx, run.id, schema.SetRunStateRequestV1{State: state, Outcome: outcome}); err != nil {
		logReportError("update_run", err)
	}
}

// activity adds an entry to the run's stream. Summaries say what happened and
// never quote a prompt, a reply or an intervention.
func (b *botLoop) activity(ctx context.Context, run *activeRun, kind schema.ActivityKind, summary, outcome string, data json.RawMessage) {
	if run == nil {
		return
	}
	rctx, cancel := reportContext(ctx)
	defer cancel()
	if _, err := b.mcp.PostActivity(rctx, run.id, schema.AppendActivityRequestV1{Kind: kind, Summary: summary, Outcome: outcome, Data: data}); err != nil {
		logReportError("post_activity", err)
	}
}

func (b *botLoop) ack(ctx context.Context, id int64, state schema.InterventionState, reason string) {
	rctx, cancel := reportContext(ctx)
	defer cancel()
	if _, err := b.mcp.AnswerIntervention(rctx, id, schema.AckInterventionRequestV1{State: state, Reason: reason}); err != nil {
		logReportError("ack_intervention", err)
	}
}

// runReply makes one reply as one run: starting, running, the command, the
// post, and the end state.
func (b *botLoop) runReply(ctx context.Context, g *audienceGroup, human []schema.MessageV2, prompt string) error {
	cmdCtx, stopCommand := context.WithCancel(ctx)
	defer stopCommand()

	run := b.beginRun(ctx, stopCommand, g, human)
	var (
		fx        fixtureHandler
		stopWatch = func() {}
	)
	if run != nil {
		fx = &runFixture{b: b, run: run}
		stopWatch = b.watchInterventions(ctx, run, g)
		if !b.cfg.StreamsActivity {
			b.activity(ctx, run, schema.ActivityKindCommandStarted, "started "+b.cfg.Command.name(), "", nil)
		}
	}
	reply, err := b.claude.Reply(cmdCtx, prompt, fx)
	// No intervention is taken once the command is over.
	stopWatch()
	return b.finishReply(ctx, g, run, reply, err)
}

// finishReply reports how the command ended, posts the reply if there is one,
// and puts the run in its end state. The error it returns is the one the poll
// loop backs off on; a stop asked for by a person or by shutdown is not one.
func (b *botLoop) finishReply(ctx context.Context, g *audienceGroup, run *activeRun, reply string, err error) error {
	if err != nil {
		return b.finishFailedCommand(ctx, run, err)
	}
	if !b.cfg.StreamsActivity {
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command finished", "exit 0", nil)
	}
	b.answerLateCancels(ctx, run, "the run had finished")

	reply = strings.TrimSpace(reply)
	if reply == "" {
		b.setState(ctx, run, schema.RunStateCompleted, "no reply")
		return nil
	}
	if err := b.mcp.postMessage(ctx, b.cfg.Channel, reply, g.audience); err != nil {
		if ctx.Err() != nil {
			b.setState(ctx, run, schema.RunStateCancelled, "adapter stopped")
			return nil //nolint:nilerr // the post failed because the adapter is stopping, which is not a failure to back off on
		}
		var toolErr *mcpclient.ToolError
		if g.audience != nil && errors.As(err, &toolErr) && refusalCodes[toolErr.Code] {
			slog.Warn("conch-adapter: scoped reply refused; nothing was posted", "code", toolErr.Code, "audience_kind", string(g.audience.Kind))
			b.setState(ctx, run, schema.RunStateFailed, "reply refused")
			return nil
		}
		b.setState(ctx, run, schema.RunStateFailed, "post failed")
		return fmt.Errorf("post reply: %w", err)
	}
	b.activity(ctx, run, schema.ActivityKindReplyPosted, "posted the reply", "", nil)
	b.setState(ctx, run, schema.RunStateCompleted, "replied")
	return nil
}

// finishFailedCommand reports a command that did not produce a reply.
func (b *botLoop) finishFailedCommand(ctx context.Context, run *activeRun, err error) error {
	var (
		exit    *exitError
		tooLong *pausedTooLongError
	)
	switch {
	case run != nil && run.wasCancelled() && errors.Is(err, context.Canceled):
		// The command is gone: Reply does not return before its process group
		// is. Only now is the run cancelled, and the people who asked told so.
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command stopped", "cancelled", nil)
		b.setState(ctx, run, schema.RunStateCancelled, "cancelled by request")
		for _, id := range run.takeCancelIDs() {
			b.ack(ctx, id, schema.InterventionStateApplied, "")
		}
		return nil
	case ctx.Err() != nil:
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command stopped", "adapter stopped", nil)
		b.setState(ctx, run, schema.RunStateCancelled, "adapter stopped")
		b.answerLateCancels(ctx, run, "the adapter stopped")
		return nil //nolint:nilerr // stopping the command is what shutdown asked for, not a failure to back off on
	case errors.Is(err, errNotJSONL):
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command output not understood", "error", nil)
		b.setState(ctx, run, schema.RunStateFailed, err.Error())
	case errors.Is(err, errTooMuchActivity):
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command stopped", "error", nil)
		b.setState(ctx, run, schema.RunStateFailed, err.Error())
	case errors.As(err, &tooLong):
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command stopped", "paused too long", nil)
		b.setState(ctx, run, schema.RunStateFailed, tooLong.Error())
	case errors.Is(err, errReplyTimeout):
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command timed out", "timeout", nil)
		b.setState(ctx, run, schema.RunStateFailed, "timeout")
	case errors.As(err, &exit):
		outcome := exit.outcome()
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command failed", outcome, nil)
		b.setState(ctx, run, schema.RunStateFailed, outcome)
	default:
		b.activity(ctx, run, schema.ActivityKindCommandFinished, "command could not run", "error", nil)
		b.setState(ctx, run, schema.RunStateFailed, "error")
	}
	b.answerLateCancels(ctx, run, "the run had ended")
	return fmt.Errorf("generate reply: %w", err)
}

// answerLateCancels answers cancel interventions that came in too late to
// matter: the run ended some other way first.
func (b *botLoop) answerLateCancels(ctx context.Context, run *activeRun, reason string) {
	if run == nil {
		return
	}
	for _, id := range run.takeCancelIDs() {
		b.ack(ctx, id, schema.InterventionStateFailed, reason)
	}
}

// watchInterventions polls for interventions on the session while the run is
// open, and returns the function that stops it and waits for it to finish.
func (b *botLoop) watchInterventions(ctx context.Context, run *activeRun, g *audienceGroup) func() {
	// Created before the goroutine starts, so a tick sent after this returns
	// is not lost.
	tick := b.time().newTicker(interventionPollInterval)
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer tick.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-tick.C():
				b.takeInterventions(watchCtx, run, g)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// takeInterventions reads the interventions waiting for an answer and acts on
// each.
func (b *botLoop) takeInterventions(ctx context.Context, run *activeRun, g *audienceGroup) {
	list, err := b.mcp.PollInterventions(ctx, b.sessionID, schema.InterventionStateRequested, b.ivAfter)
	if err != nil {
		if ctx.Err() == nil {
			logReportError("poll_interventions", err)
		}
		return
	}
	for _, iv := range list.Interventions {
		if iv.ID > b.ivAfter {
			b.ivAfter = iv.ID
		}
		b.handleIntervention(ctx, run, g, iv)
	}
}

func (b *botLoop) handleIntervention(ctx context.Context, run *activeRun, g *audienceGroup, iv schema.InterventionV1) {
	switch {
	case iv.RunID != run.id:
		b.ack(ctx, iv.ID, schema.InterventionStateFailed, "the run is not open")
	case iv.Kind == schema.InterventionKindMessage:
		b.ack(ctx, iv.ID, schema.InterventionStateApplied, "")
		// The body goes into the next prompt of this audience only. It is
		// never logged or put in an activity entry.
		b.addNote(g.key, iv.Body)
		b.activity(ctx, run, schema.ActivityKindInterventionReceived, "message received", "", nil)
	case iv.Kind == schema.InterventionKindCancel:
		run.requestCancel(iv.ID)
	case iv.Kind == schema.InterventionKindPause && canPauseGroup:
		b.pauseRun(ctx, run, iv.ID)
	case iv.Kind == schema.InterventionKindResume && canPauseGroup:
		b.resumeRun(ctx, run, iv.ID)
	default:
		b.ack(ctx, iv.ID, schema.InterventionStateFailed, "unsupported")
	}
}

// pauseRun freezes the command. The run is reported waiting, with the outcome
// paused, only after the command is stopped; it is parked in waiting already
// when an approval is being waited for, and nothing is reported then.
func (b *botLoop) pauseRun(ctx context.Context, run *activeRun, id int64) {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	proc := run.process()
	if proc == nil {
		b.ack(ctx, id, schema.InterventionStateFailed, "not running")
		return
	}
	if err := proc.pause(); err != nil {
		b.ack(ctx, id, schema.InterventionStateFailed, pauseFailure(err, "already paused"))
		return
	}
	run.paused = true
	b.ack(ctx, id, schema.InterventionStateApplied, "")
	if !run.inApproval {
		b.setState(ctx, run, schema.RunStateWaiting, "paused")
	}
	b.activity(ctx, run, schema.ActivityKindInterventionReceived, "paused", "", nil)
}

// resumeRun lets the command go on and puts the run back to running, unless
// an approval it is waiting for is still open: that puts it back itself.
func (b *botLoop) resumeRun(ctx context.Context, run *activeRun, id int64) {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	proc := run.process()
	if proc == nil {
		b.ack(ctx, id, schema.InterventionStateFailed, "not running")
		return
	}
	if err := proc.resume(); err != nil {
		b.ack(ctx, id, schema.InterventionStateFailed, pauseFailure(err, "not paused"))
		return
	}
	run.paused = false
	if !run.inApproval {
		b.setState(ctx, run, schema.RunStateRunning, "")
	}
	b.ack(ctx, id, schema.InterventionStateApplied, "")
	b.activity(ctx, run, schema.ActivityKindInterventionReceived, "resumed", "", nil)
}

// pauseFailure is the reason an intervention failed for. It is one of a few
// fixed words: the error of a signal is logged, not sent.
func pauseFailure(err error, wrongState string) string {
	switch {
	case errors.Is(err, errAlreadyPaused), errors.Is(err, errNotPaused):
		return wrongState
	case errors.Is(err, errNotRunning):
		return "not running"
	}
	slog.Warn("conch-adapter: could not signal the command", "error", err)
	return "could not signal the command"
}

// runFixture is what the fixture markers ask of a run. It is also where the
// reader of a streaming profile reports the command's steps.
type runFixture struct {
	b   *botLoop
	run *activeRun
}

// activity adds an entry the command's output told of.
func (f *runFixture) activity(ctx context.Context, kind schema.ActivityKind, summary, outcome string) {
	f.b.activity(ctx, f.run, kind, summary, outcome, nil)
}

// approval raises an approval, parks the run in waiting until it is decided,
// and puts it back to running. Whatever the decision, the command goes on: the
// fixture scripts only exist to make the approval happen.
func (f *runFixture) approval(ctx context.Context, title string) error {
	b := f.b
	timeout := b.cfg.ReplyTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := b.time().now().Add(timeout)
	req := schema.CreateApprovalRequestV1{
		RequesterID: b.cfg.PrincipalID,
		ChannelID:   f.run.channelID,
		Title:       title,
		Body:        "Requested by a fixture script during a run of the adapter.",
		Options: []schema.Option{
			{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
		},
		Deadline: schema.NewTimestamp(deadline),
	}
	created, err := b.mcp.RequestApproval(ctx, req)
	if err != nil {
		return fmt.Errorf("request approval: %w", err)
	}
	f.run.stateMu.Lock()
	f.run.inApproval = true
	if !f.run.paused { // a paused run is waiting already
		b.setState(ctx, f.run, schema.RunStateWaiting, "")
	}
	f.run.stateMu.Unlock()
	data, _ := json.Marshal(map[string]int64{"approval_id": created.ID}) // a map of one integer always encodes
	b.activity(ctx, f.run, schema.ActivityKindApprovalRequested, "asked for approval", "", data)

	var outcome string
	for {
		remaining := deadline.Sub(b.time().now())
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		wait := min(remaining, awaitChunk)
		decision, err := b.mcp.AwaitDecision(ctx, created.ID, wait.Milliseconds())
		if err != nil {
			return fmt.Errorf("await decision: %w", err)
		}
		if decision.State.IsTerminal() {
			outcome = string(decision.State)
			if decision.Resolution != nil {
				outcome = string(decision.Resolution.Outcome)
			}
			break
		}
	}
	b.activity(ctx, f.run, schema.ActivityKindApprovalResolved, "approval decided", outcome, data)
	f.run.stateMu.Lock()
	f.run.inApproval = false
	if !f.run.paused { // a paused run goes back to running when it is resumed
		b.setState(ctx, f.run, schema.RunStateRunning, "")
	}
	f.run.stateMu.Unlock()
	return nil
}
