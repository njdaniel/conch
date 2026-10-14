package adapter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

// fakeClock hands out tickers that fire when a test says so and a time that
// moves when a test says so.
type fakeClock struct {
	mu      sync.Mutex
	t       time.Time
	tickers []*fakeTicker
	created chan time.Duration
}

type fakeTicker struct {
	period  time.Duration
	c       chan time.Time
	stopped bool
	clock   *fakeClock
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), created: make(chan time.Duration, 64)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) newTicker(d time.Duration) ticker {
	c.mu.Lock()
	tk := &fakeTicker{period: d, c: make(chan time.Time, 1), clock: c}
	c.tickers = append(c.tickers, tk)
	c.mu.Unlock()
	c.created <- d
	return tk
}

func (t *fakeTicker) C() <-chan time.Time { return t.c }
func (t *fakeTicker) Stop() {
	t.clock.mu.Lock()
	t.stopped = true
	t.clock.mu.Unlock()
}

// waitFor returns once a ticker with the period exists.
func (c *fakeClock) waitFor(t *testing.T, period time.Duration) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case d := <-c.created:
			if d == period {
				return
			}
		case <-timeout:
			t.Fatalf("no ticker of %v was created", period)
		}
	}
}

// fire ticks every live ticker with the period.
func (c *fakeClock) fire(period time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tk := range c.tickers {
		if tk.period == period && !tk.stopped {
			select {
			case tk.c <- c.t:
			default:
			}
		}
	}
}

func intervention(id, runID int64, kind schema.InterventionKind, body string) schema.InterventionV1 {
	return schema.InterventionV1{ID: id, RunID: runID, Kind: kind, Body: body, State: schema.InterventionStateRequested}
}

// loopWithSession is a loop that has registered, with one human message to
// answer per group.
func loopWithSession(mcp *fakeMCP, claude ClaudeRunner, clk timeSource) *botLoop {
	return &botLoop{cfg: testConfig(), mcp: mcp, claude: claude, clk: clk, sessionID: 7}
}

func answer(t *testing.T, loop *botLoop, messages ...schema.MessageV2) error {
	t.Helper()
	mcp := loop.mcp.(*fakeMCP)
	mcp.pages = pagesFor(nil, messages)
	return loop.pollOnce(context.Background())
}

// A reply is a run, and the run's states and activity are the same order for
// every way a command can end.
func TestRunStatesAndActivityPerOutcome(t *testing.T) {
	tests := []struct {
		name       string
		reply      string
		err        error
		wantEvents []string
		wantErr    bool
	}{
		{
			name: "success", reply: "an answer",
			wantEvents: []string{
				"start_run", "update_run:starting", "update_run:running",
				"activity:command_started", "activity:command_finished",
				"post_message", "activity:reply_posted", "update_run:completed:replied",
			},
		},
		{
			name: "an empty reply posts nothing", reply: " \n",
			wantEvents: []string{
				"start_run", "update_run:starting", "update_run:running",
				"activity:command_started", "activity:command_finished", "update_run:completed:no reply",
			},
		},
		{
			name: "non-zero exit", err: &exitError{code: 3}, wantErr: true,
			wantEvents: []string{
				"start_run", "update_run:starting", "update_run:running",
				"activity:command_started", "activity:command_finished", "update_run:failed:exit 3",
			},
		},
		{
			name: "timeout", err: errReplyTimeout, wantErr: true,
			wantEvents: []string{
				"start_run", "update_run:starting", "update_run:running",
				"activity:command_started", "activity:command_finished", "update_run:failed:timeout",
			},
		},
		{
			name: "the command could not run", err: errors.New("start command: no such file"), wantErr: true,
			wantEvents: []string{
				"start_run", "update_run:starting", "update_run:running",
				"activity:command_started", "activity:command_finished", "update_run:failed:error",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{}
			loop := loopWithSession(mcp, &fakeClaude{reply: tt.reply, err: tt.err}, newFakeClock())
			err := answer(t, loop, message(1, 2, "a question"))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := mcp.eventLog(); !reflect.DeepEqual(got, tt.wantEvents) {
				t.Fatalf("events:\n got %q\nwant %q", got, tt.wantEvents)
			}
			if tt.wantErr && len(mcp.posts) != 0 {
				t.Fatalf("posted %q after a failure", mcp.posts)
			}
		})
	}
}

func TestRunActivityKindsAndOutcomes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []activityCall
	}{
		{"success", nil, []activityCall{
			{runID: 1, kind: schema.ActivityKindCommandStarted, summary: "started command"},
			{runID: 1, kind: schema.ActivityKindCommandFinished, summary: "command finished", outcome: "exit 0"},
			{runID: 1, kind: schema.ActivityKindReplyPosted, summary: "posted the reply"},
		}},
		{"exit", &exitError{code: 3}, []activityCall{
			{runID: 1, kind: schema.ActivityKindCommandStarted, summary: "started command"},
			{runID: 1, kind: schema.ActivityKindCommandFinished, summary: "command failed", outcome: "exit 3"},
		}},
		{"timeout", errReplyTimeout, []activityCall{
			{runID: 1, kind: schema.ActivityKindCommandStarted, summary: "started command"},
			{runID: 1, kind: schema.ActivityKindCommandFinished, summary: "command timed out", outcome: "timeout"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{}
			loop := loopWithSession(mcp, &fakeClaude{reply: "an answer", err: tt.err}, newFakeClock())
			_ = answer(t, loop, message(1, 2, "a question"))
			if !reflect.DeepEqual(mcp.activities, tt.want) {
				t.Fatalf("activities:\n got %+v\nwant %+v", mcp.activities, tt.want)
			}
		})
	}
}

// The run names the message it answers, the newest of the audience's, and the
// start of that message as its task; a whisper's run carries the whisper's id.
func TestRunCarriesTheMessageAndTask(t *testing.T) {
	long := strings.Repeat("x", 200)
	tests := []struct {
		name     string
		messages []schema.MessageV2
		wantID   []int64
		wantTask []string
	}{
		{"one message", []schema.MessageV2{message(1, 2, "hello there")}, []int64{1}, []string{"hello there"}},
		{"the newest of several", []schema.MessageV2{message(1, 2, "older"), message(2, 3, "newer")}, []int64{2}, []string{"newer"}},
		{"a whisper's run has the whisper's id", []schema.MessageV2{message(1, 2, "open"), scoped(2, 2, "private", testWhisper)}, []int64{1, 2}, []string{"open", "private"}},
		{"line breaks and control characters are squeezed out", []schema.MessageV2{message(1, 2, "a\n\nb\tc\x1b[2Jd\u202e e")}, []int64{1}, []string{"a b c[2Jd e"}},
		{"cut at 120 characters", []schema.MessageV2{message(1, 2, long)}, []int64{1}, []string{long[:120]}},
		{"multibyte text is cut on a character", []schema.MessageV2{message(1, 2, strings.Repeat("é", 130))}, []int64{1}, []string{strings.Repeat("é", 120)}},
		{"nothing printable", []schema.MessageV2{message(1, 2, "\n\x00 ")}, []int64{1}, []string{"reply to a message"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{}
			loop := loopWithSession(mcp, &fakeClaude{reply: "ok"}, newFakeClock())
			if err := answer(t, loop, tt.messages...); err != nil {
				t.Fatal(err)
			}
			var ids []int64
			var tasks []string
			for _, run := range mcp.runs {
				if err := run.Validate(); err != nil {
					t.Errorf("request does not validate: %v", err)
				}
				if run.Channel != "ops" {
					t.Errorf("channel = %q", run.Channel)
				}
				ids = append(ids, run.MessageID)
				tasks = append(tasks, run.Task)
			}
			if !reflect.DeepEqual(ids, tt.wantID) || !reflect.DeepEqual(tasks, tt.wantTask) {
				t.Fatalf("message ids %v tasks %q, want %v %q", ids, tasks, tt.wantID, tt.wantTask)
			}
		})
	}
}

// Nothing a person wrote reaches an activity entry or a run outcome, and
// nothing reaches the log either, even when reporting fails.
func TestNoPromptOrInterventionTextInReportsOrLogs(t *testing.T) {
	const (
		secretMessage = "SECRET-MESSAGE-BODY"
		secretNote    = "SECRET-INTERVENTION-BODY"
		secretToken   = "SECRET-TOKEN"
	)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// Every report fails, so the log path is exercised as well.
	mcp := &fakeMCP{
		interv:      []schema.InterventionV1{intervention(1, 1, schema.InterventionKindMessage, secretNote)},
		polled:      make(chan struct{}, 4),
		activityErr: &mcpclient.ToolError{Method: "tools/call", Code: "rate_limited"},
	}
	clk := newFakeClock()
	loop := loopWithSession(mcp, nil, clk)
	loop.cfg.Token = secretToken
	loop.cfg.Command = commandSpec{args: []string{"/opt/bin/claude", "-p", "{prompt}"}}
	loop.claude = &fakeClaude{fn: func(context.Context, string, fixtureHandler) (string, error) {
		clk.fire(interventionPollInterval)
		<-mcp.polled
		return "", &exitError{code: 3}
	}}
	_ = answer(t, loop, message(1, 2, secretMessage))
	if !strings.Contains(logs.String(), "report failed") {
		t.Fatalf("no report failure was logged, so the log check proves nothing: %q", logs.String())
	}

	var seen []string
	for _, a := range mcp.activities {
		seen = append(seen, a.summary, a.outcome, a.data)
	}
	seen = append(seen, mcp.runStates...)
	seen = append(seen, logs.String())
	for _, text := range seen {
		for _, secret := range []string{secretMessage, secretNote, secretToken, "-p"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q appears in a report or a log line: %q", secret, text)
			}
		}
	}
	if mcp.activities[0].summary != "started claude" {
		t.Errorf("command_started summary = %q, want the command's first word only", mcp.activities[0].summary)
	}
}

// A run that could not be started does not stop the reply.
func TestReplyWithoutARunWhenStartRunFails(t *testing.T) {
	mcp := &fakeMCP{startErr: errors.New("boom")}
	loop := loopWithSession(mcp, &fakeClaude{reply: "an answer"}, newFakeClock())
	if err := answer(t, loop, message(1, 2, "q")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mcp.posts, []string{"an answer"}) {
		t.Fatalf("posts = %q", mcp.posts)
	}
	if got := mcp.eventLog(); !reflect.DeepEqual(got, []string{"start_run", "post_message"}) {
		t.Fatalf("events = %q", got)
	}
}

func TestRefusedScopedReplyFailsTheRun(t *testing.T) {
	mcp := &fakeMCP{postErr: &mcpclient.ToolError{Method: "tools/call", Code: "forbidden"}}
	loop := loopWithSession(mcp, &fakeClaude{reply: "an answer"}, newFakeClock())
	if err := answer(t, loop, scoped(1, 2, "q", testNet)); err != nil {
		t.Fatal(err)
	}
	if got := mcp.runStates; !reflect.DeepEqual(got, []string{"starting", "running", "failed:reply refused"}) {
		t.Fatalf("run states = %q", got)
	}
}

// While a run is open, interventions are taken on the 2 s tick. Each case
// queues interventions for run 1 and checks how they were answered.
func TestInterventions(t *testing.T) {
	tests := []struct {
		name      string
		queued    []schema.InterventionV1
		wantAcks  []ackCall
		wantNote  string // in the next prompt of the same audience
		wantTypes []schema.ActivityKind
	}{
		{
			name:      "message is applied, kept for the next prompt and noted",
			queued:    []schema.InterventionV1{intervention(1, 1, schema.InterventionKindMessage, "use tabs")},
			wantAcks:  []ackCall{{1, schema.InterventionStateApplied, ""}},
			wantNote:  "use tabs",
			wantTypes: []schema.ActivityKind{schema.ActivityKindCommandStarted, schema.ActivityKindInterventionReceived},
		},
		{
			name:     "an unsupported kind fails",
			queued:   []schema.InterventionV1{intervention(1, 1, schema.InterventionKindPause, "")},
			wantAcks: []ackCall{{1, schema.InterventionStateFailed, "unsupported"}},
		},
		{
			name:     "an intervention for another run fails",
			queued:   []schema.InterventionV1{intervention(1, 99, schema.InterventionKindMessage, "stale")},
			wantAcks: []ackCall{{1, schema.InterventionStateFailed, "the run is not open"}},
		},
		{
			name: "several are taken in order",
			queued: []schema.InterventionV1{
				intervention(1, 1, schema.InterventionKindMessage, "one"),
				intervention(2, 1, schema.InterventionKindResume, ""),
				intervention(3, 1, schema.InterventionKindMessage, "three"),
			},
			wantAcks: []ackCall{{1, schema.InterventionStateApplied, ""}, {2, schema.InterventionStateFailed, "unsupported"}, {3, schema.InterventionStateApplied, ""}},
			wantNote: "three",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{interv: tt.queued, polled: make(chan struct{}, 16)}
			clk := newFakeClock()
			claude := &fakeClaude{}
			loop := loopWithSession(mcp, claude, clk)
			claude.fn = func(context.Context, string, fixtureHandler) (string, error) {
				clk.fire(interventionPollInterval)
				<-mcp.polled // the watcher has read the interventions
				return "done", nil
			}
			if err := answer(t, loop, scoped(1, 2, "first question", testNet)); err != nil {
				t.Fatal(err)
			}
			// The watcher is stopped before the run ends; a poll that was
			// already running has finished its acks.
			mcp.mu.Lock()
			acks := append([]ackCall(nil), mcp.acks...)
			mcp.mu.Unlock()
			if !reflect.DeepEqual(acks, tt.wantAcks) {
				t.Fatalf("acks = %+v, want %+v", acks, tt.wantAcks)
			}
			if tt.wantTypes != nil {
				var kinds []schema.ActivityKind
				for _, a := range mcp.activities {
					kinds = append(kinds, a.kind)
				}
				if len(kinds) < len(tt.wantTypes) || !reflect.DeepEqual(kinds[:len(tt.wantTypes)], tt.wantTypes) {
					t.Fatalf("activity kinds = %v, want to start with %v", kinds, tt.wantTypes)
				}
			}
			// The next prompt of the same audience carries the note; another
			// audience's never does.
			mcp.pages = pagesFor(nil, []schema.MessageV2{scoped(1, 2, "first question", testNet)}, []schema.MessageV2{
				scoped(2, 2, "second question", testNet), scoped(3, 2, "unrelated", otherNet),
			})
			loop.claude = &fakeClaude{reply: "ok"}
			if err := loop.pollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			prompts := loop.claude.(*fakeClaude).prompts
			if len(prompts) != 2 {
				t.Fatalf("prompts = %d, want 2", len(prompts))
			}
			if tt.wantNote != "" && !strings.Contains(prompts[0], tt.wantNote) {
				t.Errorf("the next prompt of the audience lacks %q:\n%s", tt.wantNote, prompts[0])
			}
			if strings.Contains(prompts[1], "use tabs") || strings.Contains(prompts[1], "three") {
				t.Errorf("another audience's prompt carries an intervention:\n%s", prompts[1])
			}
		})
	}
}

// A cancel kills the command, and only after it is gone is the run reported
// cancelled and the intervention answered applied.
func TestCancelInterventionOrder(t *testing.T) {
	mcp := &fakeMCP{interv: []schema.InterventionV1{intervention(5, 1, schema.InterventionKindCancel, "")}, polled: make(chan struct{}, 4)}
	clk := newFakeClock()
	var killed bool
	claude := &fakeClaude{fn: func(ctx context.Context, _ string, _ fixtureHandler) (string, error) {
		clk.fire(interventionPollInterval)
		<-ctx.Done() // the runner's kill
		mcp.mu.Lock()
		mcp.record("command killed")
		killed = true
		mcp.mu.Unlock()
		return "", ctx.Err()
	}}
	loop := loopWithSession(mcp, claude, clk)
	if err := answer(t, loop, message(1, 2, "q")); err != nil {
		t.Fatalf("a cancel is not an error: %v", err)
	}
	if !killed {
		t.Fatal("the command was not stopped")
	}
	want := []string{
		"start_run", "update_run:starting", "update_run:running", "activity:command_started",
		"command killed", "activity:command_finished", "update_run:cancelled:cancelled by request", "ack:5:applied",
	}
	if got := mcp.eventLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
	if len(mcp.posts) != 0 {
		t.Fatalf("a cancelled run posted %q", mcp.posts)
	}
}

// A cancel that arrives after the command finished cannot cancel anything; it
// is answered failed, not applied.
func TestLateCancelIsAnsweredFailed(t *testing.T) {
	mcp := &fakeMCP{interv: []schema.InterventionV1{intervention(5, 1, schema.InterventionKindCancel, "")}}
	run := &activeRun{id: 1, stopCommand: func() {}}
	loop := loopWithSession(mcp, &fakeClaude{}, newFakeClock())
	loop.handleIntervention(context.Background(), run, &audienceGroup{key: "channel"}, mcp.interv[0])
	if err := loop.finishReply(context.Background(), &audienceGroup{key: "channel"}, run, "fine", nil); err != nil {
		t.Fatal(err)
	}
	if want := []ackCall{{5, schema.InterventionStateFailed, "the run had finished"}}; !reflect.DeepEqual(mcp.acks, want) {
		t.Fatalf("acks = %+v, want %+v", mcp.acks, want)
	}
	if got := mcp.runStates; !reflect.DeepEqual(got, []string{"completed:replied"}) {
		t.Fatalf("run states = %q", got)
	}
}

func TestRegisterSession(t *testing.T) {
	mcp := &fakeMCP{}
	loop := &botLoop{cfg: testConfig(), mcp: mcp}
	loop.cfg.Runtime, loop.cfg.Model = "claude-code", "sonnet"
	if err := loop.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	want := schema.CreateSessionRequestV1{
		Client: "conch-adapter", Runtime: "claude-code", Model: "sonnet", Host: host,
		Interventions: []schema.InterventionKind{schema.InterventionKindMessage, schema.InterventionKindCancel},
	}
	if len(mcp.registered) != 1 || !reflect.DeepEqual(mcp.registered[0], want) {
		t.Fatalf("registered %+v, want %+v", mcp.registered, want)
	}
	if err := mcp.registered[0].Validate(); err != nil {
		t.Fatal(err)
	}
	if loop.sessionID != 7 {
		t.Fatalf("sessionID = %d", loop.sessionID)
	}
}

// The heartbeat has a goroutine of its own: it keeps going while a reply is in
// flight, and stops before the session ends.
func TestHeartbeatWhileAReplyIsInFlight(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{0: {}, 1: {Messages: []schema.MessageV2{message(2, 3, "q")}}}, beats: make(chan struct{}, 16)}
	// The seed reads 0; the message arrives with the first poll.
	mcp.pages[0] = schema.ListMessagesResponseV2{Messages: []schema.MessageV2{message(1, 3, "old")}}
	clk := newFakeClock()
	inFlight, release := make(chan struct{}), make(chan struct{})
	claude := &fakeClaude{fn: func(context.Context, string, fixtureHandler) (string, error) {
		close(inFlight)
		<-release
		return "an answer", nil
	}}
	loop := loopWithSession(mcp, claude, clk)
	loop.cfg.Heartbeat = 10 * time.Second
	loop.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.run(ctx) }()

	<-inFlight // the reply is being made
	clk.waitFor(t, 10*time.Second)
	for i := 0; i < 3; i++ {
		clk.fire(10 * time.Second)
		<-mcp.beats
	}
	close(release)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	log := mcp.eventLog()
	if got := countOf(log, "heartbeat"); got != 3 {
		t.Fatalf("heartbeats = %d, want 3; events %q", got, log)
	}
	if last := log[len(log)-1]; last != "end_session" {
		t.Fatalf("last event = %q, want end_session; events %q", last, log)
	}
}

func countOf(events []string, want string) int {
	n := 0
	for _, e := range events {
		if e == want {
			n++
		}
	}
	return n
}

// On shutdown with a run open: the command is stopped, the run is reported
// cancelled with the outcome "adapter stopped", and only then the session ends.
func TestShutdownWithARunOpen(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{0: {Messages: []schema.MessageV2{message(1, 3, "old")}}, 1: {Messages: []schema.MessageV2{message(2, 3, "q")}}}}
	started := make(chan struct{})
	claude := &fakeClaude{fn: func(ctx context.Context, _ string, _ fixtureHandler) (string, error) {
		close(started)
		<-ctx.Done()
		mcp.mu.Lock()
		mcp.record("command killed")
		mcp.mu.Unlock()
		return "", ctx.Err()
	}}
	loop := loopWithSession(mcp, claude, newFakeClock())
	loop.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.run(ctx) }()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("shutdown is not an error: %v", err)
	}
	log := mcp.eventLog()
	tail := log[len(log)-4:]
	want := []string{"command killed", "activity:command_finished", "update_run:cancelled:adapter stopped", "end_session"}
	if !reflect.DeepEqual(tail, want) {
		t.Fatalf("events end with %q, want %q (all: %q)", tail, want, log)
	}
	if mcp.endCalls != 1 || len(mcp.posts) != 0 {
		t.Fatalf("end_session calls %d, posts %q", mcp.endCalls, mcp.posts)
	}
}

func TestReplyLimitPerMinute(t *testing.T) {
	clk := newFakeClock()
	loop := &botLoop{cfg: testConfig(), clk: clk}
	loop.cfg.MaxRepliesPerMinute = 3
	step := func(d time.Duration) bool { clk.advance(d); return loop.allowReply() }
	// after is the time since the previous step; the comment gives the time
	// since the start.
	tests := []struct {
		after time.Duration
		want  bool
	}{
		{0, true}, {10 * time.Second, true}, {10 * time.Second, true}, // 0, 10, 20: the third in the minute
		{10 * time.Second, false}, // 30: three in the last minute
		{29 * time.Second, false}, // 59: the first is 59 s old
		{time.Second, true},       // 60: the first has aged out
		{time.Second, false},      // 61: three again (10, 20, 60)
		{9 * time.Second, true},   // 70: the one at 10 has aged out
		{130 * time.Second, true}, // 200: everything has
	}
	for i, tt := range tests {
		if got := step(tt.after); got != tt.want {
			t.Fatalf("step %d: allowReply = %v, want %v", i, got, tt.want)
		}
	}
	t.Run("zero is no cap", func(t *testing.T) {
		loop := &botLoop{cfg: testConfig(), clk: clk}
		for i := 0; i < 100; i++ {
			if !loop.allowReply() {
				t.Fatal("capped")
			}
		}
	})
}

// sharedChannel is one channel two adapters talk in.
type sharedChannel struct {
	mu       sync.Mutex
	messages []schema.MessageV2
}

type chatMCP struct {
	*fakeMCP
	channel *sharedChannel
	self    int64
}

func (c *chatMCP) readChannel(_ context.Context, _ string, after int64, _ int) (schema.ListMessagesResponseV2, error) {
	c.channel.mu.Lock()
	defer c.channel.mu.Unlock()
	var page schema.ListMessagesResponseV2
	for _, m := range c.channel.messages {
		if m.ID > after {
			page.Messages = append(page.Messages, m)
		}
	}
	return page, nil
}

func (c *chatMCP) postMessage(_ context.Context, _, body string, _ *schema.Audience) error {
	c.channel.mu.Lock()
	defer c.channel.mu.Unlock()
	c.channel.messages = append(c.channel.messages, message(int64(len(c.channel.messages)+1), c.self, body))
	return nil
}

// Two adapters that answer each other's every message would never stop; the
// replies-per-minute cap does stop them.
func TestReplyLimitStopsAnEchoLoop(t *testing.T) {
	const perMinute = 4
	channel := &sharedChannel{messages: []schema.MessageV2{message(1, 100, "hello")}}
	clk := newFakeClock()
	var loops []*botLoop
	for _, id := range []int64{1, 2} {
		cfg := testConfig()
		cfg.PrincipalID, cfg.MaxRepliesPerMinute = id, perMinute
		loops = append(loops, &botLoop{
			cfg: cfg, clk: clk, sessionID: 7,
			mcp:    &chatMCP{fakeMCP: &fakeMCP{}, channel: channel, self: id},
			claude: &fakeClaude{reply: "echo"},
		})
	}
	for round := 0; round < 50; round++ { // all within one minute of the fake clock
		for _, loop := range loops {
			if err := loop.pollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, max := len(channel.messages)-1, 2*perMinute; got > max {
		t.Fatalf("%d replies in a minute, want at most %d", got, max)
	}
	before := len(channel.messages)
	for round := 0; round < 10; round++ {
		for _, loop := range loops {
			_ = loop.pollOnce(context.Background())
		}
	}
	if len(channel.messages) != before {
		t.Fatal("the loop went on after the cap was reached")
	}
}

// The fixture approval: the run waits, the approval is raised in the run's
// channel with a deadline from the clock, and the run goes back to running.
func TestFixtureApprovalFlow(t *testing.T) {
	pending := schema.AwaitDecisionOutput{State: schema.ApprovalStatePending}
	resolved := schema.AwaitDecisionOutput{State: schema.ApprovalStateResolved, Resolution: &schema.ApprovalResolutionV1{ApprovalID: 41, Outcome: schema.OutcomeRejected}}
	mcp := &fakeMCP{decisions: []schema.AwaitDecisionOutput{pending, pending, resolved}}
	clk := newFakeClock()
	claude := &fakeClaude{fn: func(ctx context.Context, _ string, fx fixtureHandler) (string, error) {
		if err := fx.approval(ctx, "Deploy it"); err != nil {
			return "", err
		}
		return "after the approval", nil
	}}
	loop := loopWithSession(mcp, claude, clk)
	loop.cfg.PrincipalID, loop.cfg.ReplyTimeout = 9, time.Minute
	if err := answer(t, loop, message(1, 2, "q")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start_run", "update_run:starting", "update_run:running", "activity:command_started",
		"request_approval", "update_run:waiting", "activity:approval_requested",
		"await_decision", "await_decision", "await_decision",
		"activity:approval_resolved", "update_run:running",
		"activity:command_finished", "post_message", "activity:reply_posted", "update_run:completed:replied",
	}
	if got := mcp.eventLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
	req := mcp.approvals[0]
	if req.ChannelID != 3 || req.Title != "Deploy it" || req.RequesterID != 9 || !req.Deadline.Time().Equal(clk.now().Add(time.Minute)) {
		t.Fatalf("approval request = %+v", req)
	}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := mcp.activities[len(mcp.activities)-3]; got.kind != schema.ActivityKindApprovalResolved || got.outcome != "rejected" {
		t.Fatalf("approval_resolved = %+v", got)
	}
}

// With the fixture off, a reply with @approval in it is posted as text and no
// approval is raised; with it on, the marker raises one and the run passes
// through waiting. This is the real runner with a real script.
func TestFixtureMarkerThroughTheLoop(t *testing.T) {
	tests := []struct {
		name         string
		fixture      bool
		wantPost     string
		wantApproval bool
		wantStates   []string
	}{
		{"off", false, "@approval do something\nthe reply", false, []string{"starting", "running", "completed:replied"}},
		{"on", true, "the reply", true, []string{"starting", "running", "waiting", "running", "completed:replied"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{}
			spec, err := parseCommand(`printf '%s\n' '@approval do something' 'the reply'`, "")
			if err != nil {
				t.Fatal(err)
			}
			runner := commandRunner{spec: spec, model: "m", timeout: time.Minute, fixture: tt.fixture, sleep: sleepContext}
			loop := loopWithSession(mcp, runner, newFakeClock())
			loop.cfg.ReplyTimeout = time.Minute
			if err := answer(t, loop, message(1, 2, "q")); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(mcp.posts, []string{tt.wantPost}) {
				t.Errorf("posts = %q, want %q", mcp.posts, tt.wantPost)
			}
			if got := len(mcp.approvals) > 0; got != tt.wantApproval {
				t.Errorf("request_approval called = %v, want %v", got, tt.wantApproval)
			}
			if !reflect.DeepEqual(mcp.runStates, tt.wantStates) {
				t.Errorf("run states = %q, want %q", mcp.runStates, tt.wantStates)
			}
		})
	}
}

func TestRunTaskFitsTheSchema(t *testing.T) {
	for _, body := range []string{"", "plain", strings.Repeat("é", 500), "tabs\tand\nnewlines", "\u200b\u202e", "emoji 🙂 text"} {
		req := schema.CreateRunRequestV1{Channel: "ops", Task: runTask(body)}
		if err := req.Validate(); err != nil {
			t.Errorf("runTask(%q) = %q: %v", body, req.Task, err)
		}
	}
}

// The server refusing a run for an audience the agent may only read (a net it
// monitors) is expected: the reply goes on without a run, and it is logged at
// debug, once for each audience, never as a warning.
func TestStartRunRefusedForAnAudienceIsQuiet(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	mcp := &fakeMCP{startErr: &mcpclient.ToolError{Method: "tools/call", Code: "channel_not_found"}}
	loop := loopWithSession(mcp, &fakeClaude{reply: "an answer"}, newFakeClock())
	if err := answer(t, loop, scoped(1, 2, "q", testNet), scoped(2, 2, "q2", testNet), message(3, 2, "open")); err != nil {
		t.Fatal(err)
	}
	if len(mcp.posts) != 2 {
		t.Fatalf("posts = %q, want both replies", mcp.posts)
	}
	out := logs.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("a warning was logged: %s", out)
	}
	if got := strings.Count(out, "replying without one"); got != 2 {
		t.Errorf("debug lines = %d, want one for each of the two audiences:\n%s", got, out)
	}
	// Another message on the same net adds nothing.
	mcp.pages = pagesFor(nil, []schema.MessageV2{scoped(1, 2, "q", testNet), scoped(2, 2, "q2", testNet), message(3, 2, "open")}, []schema.MessageV2{scoped(4, 2, "again", testNet)})
	if err := loop.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(logs.String(), "replying without one"); got != 2 {
		t.Errorf("debug lines after another message = %d, want still 2", got)
	}
}
