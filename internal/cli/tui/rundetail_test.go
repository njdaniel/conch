package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// runAPI is a fake server for run detail: one run with its activity and
// interventions, served in small pages so paging is exercised.
type runAPI struct {
	*sessionAPI
	state         sync.Mutex
	run           schema.RunV1
	runErr        error
	activity      []schema.AgentActivityV1
	interventions []schema.InterventionV1
	// runs is the session's run list, ascending by id.
	runs      []schema.RunV1
	pageSize  int
	createErr error
	created   []schema.CreateInterventionRequestV1
	gets      int
	listed    int
	// listRequests is every runs read, in order.
	listRequests []runsRequest
}

// runsRequest is one ListRuns call as the fake saw it.
type runsRequest struct {
	session int64
	page    cli.RunsPage
	limit   int
}

func newRunAPI(run schema.RunV1) *runAPI {
	return &runAPI{sessionAPI: newSessionAPI(), run: run, pageSize: 2}
}

func (a *runAPI) GetRun(context.Context, int64) (schema.RunV1, error) {
	a.state.Lock()
	defer a.state.Unlock()
	a.gets++
	return a.run, a.runErr
}

func (a *runAPI) ListActivity(_ context.Context, _, after int64, limit int) (schema.ListActivityResponseV1, error) {
	a.state.Lock()
	defer a.state.Unlock()
	if a.runErr != nil {
		return schema.ListActivityResponseV1{}, a.runErr
	}
	resp := schema.ListActivityResponseV1{Activity: []schema.AgentActivityV1{}}
	size := min(limit, a.pageSize)
	for _, e := range a.activity {
		if e.ID <= after {
			continue
		}
		if len(resp.Activity) == size {
			resp.NextAfter = resp.Activity[len(resp.Activity)-1].ID
			break
		}
		resp.Activity = append(resp.Activity, e)
	}
	return resp, nil
}

func (a *runAPI) ListInterventions(_ context.Context, _, after int64, limit int) (schema.ListInterventionsResponseV1, error) {
	a.state.Lock()
	defer a.state.Unlock()
	if a.runErr != nil {
		return schema.ListInterventionsResponseV1{}, a.runErr
	}
	resp := schema.ListInterventionsResponseV1{Interventions: []schema.InterventionV1{}}
	for _, iv := range a.interventions {
		if iv.ID > after && len(resp.Interventions) < min(limit, a.pageSize) {
			resp.Interventions = append(resp.Interventions, iv)
		}
	}
	return resp, nil
}

func (a *runAPI) CreateIntervention(_ context.Context, run int64, req schema.CreateInterventionRequestV1) (schema.InterventionV1, error) {
	a.state.Lock()
	defer a.state.Unlock()
	a.created = append(a.created, req)
	if a.createErr != nil {
		return schema.InterventionV1{}, a.createErr
	}
	return intervention(int64(100+len(a.created)), run, req.Kind, req.Body, schema.InterventionStateRequested, ""), nil
}

func (a *runAPI) ListRuns(_ context.Context, _ string, session int64, page cli.RunsPage, limit int) (schema.ListRunsResponseV1, error) {
	a.state.Lock()
	defer a.state.Unlock()
	a.listed++
	a.listRequests = append(a.listRequests, runsRequest{session: session, page: page, limit: limit})
	resp := schema.ListRunsResponseV1{Runs: []schema.RunV1{}}
	if page.NewestFirst {
		// The server's newest page: up to limit runs, whatever the page size
		// the other fake reads use.
		for i := len(a.runs) - 1; i >= 0 && len(resp.Runs) < limit; i-- {
			if page.Before == 0 || a.runs[i].ID < page.Before {
				resp.Runs = append(resp.Runs, a.runs[i])
			}
		}
		return resp, nil
	}
	after := page.After
	for _, r := range a.runs {
		if r.ID <= after {
			continue
		}
		if len(resp.Runs) == min(limit, a.pageSize) {
			resp.NextAfter = resp.Runs[len(resp.Runs)-1].ID
			break
		}
		resp.Runs = append(resp.Runs, r)
	}
	return resp, nil
}

func (a *runAPI) createdRequests() []schema.CreateInterventionRequestV1 {
	a.state.Lock()
	defer a.state.Unlock()
	return append([]schema.CreateInterventionRequestV1(nil), a.created...)
}

// testRun is a run of session 1 started five minutes before testNow.
func testRun(id int64, state schema.RunState) schema.RunV1 {
	r := schema.RunV1{
		Schema: schema.RunSchemaV1, ID: id, SessionID: 1, PrincipalID: 7, ChannelID: 1, State: state,
		Task: "fix the parser", Link: "https://example.com/pr/1",
		StartedAt: schema.NewTimestamp(testNow.Add(-5 * time.Minute)),
	}
	if schema.RunStateTerminal(state) {
		end := schema.NewTimestamp(testNow.Add(-time.Minute))
		r.EndedAt = &end
	}
	return r
}

func activityAt(id, run int64, kind schema.ActivityKind, summary string) schema.AgentActivityV1 {
	return schema.AgentActivityV1{
		Schema: schema.AgentActivitySchemaV1, ID: id, RunID: run, PrincipalID: 7, Kind: kind, Summary: summary,
		At: schema.NewTimestamp(testNow.Add(-time.Duration(100-id) * time.Second)),
	}
}

func intervention(id, run int64, kind schema.InterventionKind, body string, state schema.InterventionState, reason string) schema.InterventionV1 {
	iv := schema.InterventionV1{
		Schema: schema.InterventionSchemaV1, ID: id, RunID: run, Kind: kind, Body: body, State: state,
		RequestedBy: 5, RequestedAt: schema.NewTimestamp(testNow.Add(-time.Minute)), Reason: reason,
	}
	if state != schema.InterventionStateRequested {
		at := schema.NewTimestamp(testNow.Add(-30 * time.Second))
		iv.AnsweredAt = &at
	}
	return iv
}

// collect runs a command and the commands it batches, the way the runtime
// would, and returns what they produce. A command that blocks (the wait for
// the next event, a retry's timer) is given up on after a second: the model
// tests push events by hand. The second is generous so that a read command
// scheduled late on a loaded machine is still waited for.
func collect(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range batch {
				out = append(out, collect(c)...)
			}
			return out
		}
		if msg == nil {
			return nil
		}
		return []tea.Msg{msg}
	case <-time.After(time.Second):
		return nil
	}
}

// feed gives every result of cmd to the model, and the results of those, until
// nothing is left.
func feed(m Model, cmd tea.Cmd) Model {
	for _, msg := range collect(cmd) {
		var next tea.Cmd
		m, next = update(m, msg)
		m = feed(m, next)
	}
	return m
}

func keyOf(m Model, k tea.KeyMsg) (Model, tea.Cmd) { return update(m, k) }

func pressKey(m Model, keys ...string) Model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		var cmd tea.Cmd
		m, cmd = keyOf(m, msg)
		m = feed(m, cmd)
	}
	return m
}

// runModel is a model in the Agents view of one agent whose session 1 is in
// run 41 and declares the given interventions.
func runModel(t *testing.T, api *runAPI, declared ...schema.InterventionKind) Model {
	t.Helper()
	worker := sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)
	worker.CurrentRun, worker.Task = 41, "fix the parser"
	worker.Interventions = declared
	if worker.Interventions == nil {
		worker.Interventions = []schema.InterventionKind{}
	}
	m := agentModel(t, []schema.ChannelMemberV1{agentMember(7, "worker")}, worker)
	m.api = api
	m.loc = time.UTC
	m.width, m.height = 100, 30
	return m
}

// openCurrent opens the current run of the first session: enter unfolds the
// agent, down moves to its session, enter opens the run.
func openCurrent(m Model) Model { return pressKey(m, "enter", "down", "enter") }

func frameRun(m Model, run schema.RunV1) Model {
	m, _ = update(m, eventsRunReceived{gen: m.ev.gen, run: run})
	return m
}

func frameActivity(m Model, e schema.AgentActivityV1) Model {
	m, _ = update(m, eventsActivityReceived{gen: m.ev.gen, entry: e})
	return m
}

func frameIntervention(m Model, iv schema.InterventionV1) Model {
	m, _ = update(m, eventsInterventionReceived{gen: m.ev.gen, intervention: iv})
	return m
}

var bothKinds = []schema.InterventionKind{schema.InterventionKindMessage, schema.InterventionKindCancel}

func TestRunDetailShowsFieldsActivityAndHistory(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	for i := int64(1); i <= 5; i++ {
		api.activity = append(api.activity, activityAt(i, 41, schema.ActivityKindCommandStarted, fmt.Sprintf("step %d", i)))
	}
	api.activity[4].Outcome = "ok"
	api.interventions = []schema.InterventionV1{
		intervention(3, 41, schema.InterventionKindMessage, "use the cache", schema.InterventionStateApplied, ""),
		intervention(4, 41, schema.InterventionKindCancel, "", schema.InterventionStateFailed, "cannot stop now"),
	}
	m := openCurrent(runModel(t, api, bothKinds...))
	if m.mode != modeRun {
		t.Fatalf("mode = %v, want run detail", m.mode)
	}
	view := m.View()
	for _, want := range []string{
		"run #41 · running", "task: fix the parser", "link: https://example.com/pr/1", "started: 11:55:00 (5m ago)",
		// Five entries over pages of two: every page was read.
		"step 1", "step 2", "step 3", "step 4", "step 5 → ok", "activity (5)",
		"#3 message · applied · use the cache", "#4 cancel · failed: cannot stop now", "interventions (2)",
		"m message · x cancel",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("run detail lacks %q:\n%s", want, view)
		}
	}
	// Live: an entry and an intervention arrive on the socket.
	m = frameActivity(m, activityAt(6, 41, schema.ActivityKindReplyPosted, "posted the reply"))
	m = frameIntervention(m, intervention(5, 41, schema.InterventionKindMessage, "and the tests", schema.InterventionStateRequested, ""))
	view = m.View()
	for _, want := range []string{"posted the reply", "activity (6)", "#5 message · requested… · and the tests"} {
		if !strings.Contains(view, want) {
			t.Errorf("after the frames, run detail lacks %q:\n%s", want, view)
		}
	}
	// esc goes back to the agents.
	m = pressKey(m, "esc")
	if m.mode != modeAgents || m.rd.open() {
		t.Errorf("after esc: mode %v, open %v; want the Agents view", m.mode, m.rd.open())
	}
}

func TestRunDetailOpensFromRecentRuns(t *testing.T) {
	api := newRunAPI(testRun(31, schema.RunStateCompleted))
	// Twelve runs over pages of two: only the newest ten are listed.
	for i := int64(20); i < 32; i++ {
		r := testRun(i, schema.RunStateCompleted)
		r.Task = fmt.Sprintf("job %d", i)
		api.runs = append(api.runs, r)
	}
	m := runModel(t, api, bothKinds...)
	m = pressKey(m, "enter")
	if got := len(m.sessionRuns[sessionRunKey{"general", 1}]); got != 10 {
		t.Fatalf("listed %d runs, want the newest 10", got)
	}
	// One request for the session, from the newest, ten of them (issue #317).
	api.state.Lock()
	requests := append([]runsRequest(nil), api.listRequests...)
	api.state.Unlock()
	if want := []runsRequest{{session: 1, page: cli.RunsPage{NewestFirst: true}, limit: 10}}; !reflect.DeepEqual(requests, want) {
		t.Errorf("unfolding the agent made the runs requests %+v, want %+v", requests, want)
	}
	view := m.View()
	if !strings.Contains(view, "run #31 completed · task: job 31") || strings.Contains(view, "job 21") || strings.Contains(view, "job 20") {
		t.Errorf("the run list is not the newest ten, newest first:\n%s", view)
	}
	// down: the session; down: the newest run; enter opens it.
	m = pressKey(m, "down", "down", "enter")
	if m.mode != modeRun || m.rd.runID != 31 {
		t.Fatalf("mode %v, run %d; want run detail on run 31", m.mode, m.rd.runID)
	}
	view = m.View()
	for _, want := range []string{"run #31 · completed", "ended: 11:59:00", "the run has ended"} {
		if !strings.Contains(view, want) {
			t.Errorf("run detail lacks %q:\n%s", want, view)
		}
	}
}

func TestRunDetailKeysFollowTheDeclaredKinds(t *testing.T) {
	tests := []struct {
		name         string
		declared     []schema.InterventionKind
		wantHint     string
		wantMessage  bool
		wantCancel   bool
		wantAbsentIn string
	}{
		{"both", bothKinds, "m message · x cancel · pause (unsupported) · resume (unsupported)", true, true, ""},
		{"message only", []schema.InterventionKind{schema.InterventionKindMessage},
			"m message · cancel (unsupported) · pause (unsupported) · resume (unsupported)", true, false, "x cancel"},
		{"cancel only", []schema.InterventionKind{schema.InterventionKindCancel},
			"message (unsupported) · x cancel · pause (unsupported) · resume (unsupported)", false, true, "m message"},
		{"none", nil, "message (unsupported) · cancel (unsupported) · pause (unsupported) · resume (unsupported)", false, false, "m message"},
		// Pause is out of scope: declaring it earns no key.
		{"pause declared", []schema.InterventionKind{schema.InterventionKindPause},
			"message (unsupported) · cancel (unsupported) · pause (unsupported) · resume (unsupported)", false, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newRunAPI(testRun(41, schema.RunStateRunning))
			m := openCurrent(runModel(t, api, tt.declared...))
			if view := m.View(); !strings.Contains(view, tt.wantHint) {
				t.Errorf("hint line lacks %q:\n%s", tt.wantHint, view)
			}
			opened := pressKey(m, "m")
			if got := opened.rd.input == inputMessage; got != tt.wantMessage {
				t.Errorf("m opened the input = %v, want %v", got, tt.wantMessage)
			}
			asked := pressKey(m, "x")
			if got := asked.rd.input == inputCancel; got != tt.wantCancel {
				t.Errorf("x asked to cancel = %v, want %v", got, tt.wantCancel)
			}
			if !tt.wantMessage && !tt.wantCancel && (opened.rd.status != "" || asked.rd.status != "") {
				t.Errorf("an undeclared key did something: %q / %q", opened.rd.status, asked.rd.status)
			}
			if got := api.createdRequests(); len(got) != 0 {
				t.Errorf("requests sent without a confirmation: %+v", got)
			}
		})
	}
}

func TestInterventionStatesRenderFromFrames(t *testing.T) {
	tests := []struct {
		name  string
		state schema.InterventionState
		why   string
		want  string
	}{
		{"requested", schema.InterventionStateRequested, "", "#101 message · requested… · hello"},
		{"acknowledged", schema.InterventionStateAcknowledged, "", "#101 message · acknowledged · hello"},
		{"applied", schema.InterventionStateApplied, "", "#101 message · applied · hello"},
		{"failed", schema.InterventionStateFailed, "no such file", "#101 message · failed: no such file · hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newRunAPI(testRun(41, schema.RunStateRunning))
			m := openCurrent(runModel(t, api, bothKinds...))
			m = pressKey(m, "m", "hello", "enter")
			if got := api.createdRequests(); len(got) != 1 || got[0].Kind != schema.InterventionKindMessage || got[0].Body != "hello" {
				t.Fatalf("requests = %+v, want one message \"hello\"", got)
			}
			// Right after the send it is requested, whatever the fake could do next.
			if view := m.View(); !strings.Contains(view, "#101 message · requested… · hello") {
				t.Fatalf("after the send:\n%s", view)
			}
			if m.rd.input != inputNone || m.rd.draft != "" {
				t.Errorf("the input is still open: %v %q", m.rd.input, m.rd.draft)
			}
			m = frameIntervention(m, intervention(101, 41, schema.InterventionKindMessage, "hello", tt.state, tt.why))
			if view := m.View(); !strings.Contains(view, tt.want) {
				t.Errorf("view lacks %q:\n%s", tt.want, view)
			}
		})
	}
}

// The cancel is confirmed with y; an applied cancel does not make the run
// cancelled: only the run's own frame does.
func TestRunIsNotCancelledUntilItsFrameSaysSo(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	m := openCurrent(runModel(t, api, bothKinds...))
	m = pressKey(m, "x")
	if got := m.runPrompt(80); got != "cancel this run? y/n" {
		t.Fatalf("prompt = %q, want the question", got)
	}
	if view := m.View(); !strings.Contains(view, "cancel this run? y/n") {
		t.Errorf("the question is not on the screen:\n%s", view)
	}
	// Anything but y keeps the run.
	if after := pressKey(m, "z"); after.rd.input != inputCancel || len(api.createdRequests()) != 0 {
		t.Error("an unrelated key answered the question")
	}
	if after := pressKey(m, "n"); after.rd.input != inputNone || len(api.createdRequests()) != 0 {
		t.Error("n did not drop the question")
	}
	m = pressKey(m, "y")
	if got := api.createdRequests(); len(got) != 1 || got[0].Kind != schema.InterventionKindCancel || got[0].Body != "" {
		t.Fatalf("requests = %+v, want one cancel", got)
	}
	for _, state := range []schema.InterventionState{schema.InterventionStateAcknowledged, schema.InterventionStateApplied} {
		m = frameIntervention(m, intervention(101, 41, schema.InterventionKindCancel, "", state, ""))
		view := m.View()
		if strings.Contains(view, "cancelled") || !strings.Contains(view, "run #41 · running") {
			t.Fatalf("after the cancel was %s the run does not read running:\n%s", state, view)
		}
	}
	// A re-read does not make it so either: the server says what it says.
	m = frameRun(m, testRun(41, schema.RunStateCancelled))
	view := m.View()
	for _, want := range []string{"run #41 · cancelled", "ended:", "the run has ended"} {
		if !strings.Contains(view, want) {
			t.Errorf("after the run frame the view lacks %q:\n%s", want, view)
		}
	}
	// A terminal run offers no keys.
	if m = pressKey(m, "m"); m.rd.input != inputNone {
		t.Error("m opened the input on an ended run")
	}
	if m = pressKey(m, "x"); m.rd.input != inputNone {
		t.Error("x asked on an ended run")
	}
}

func TestRunFramesAreIdempotentAndScoped(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	api.activity = []schema.AgentActivityV1{activityAt(1, 41, schema.ActivityKindRunStarted, "started")}
	m := openCurrent(runModel(t, api, bothKinds...))

	// The same activity frame twice, and one the REST page already gave.
	e2 := activityAt(2, 41, schema.ActivityKindCommandStarted, "go test")
	m = frameActivity(frameActivity(m, e2), e2)
	m = frameActivity(m, activityAt(1, 41, schema.ActivityKindRunStarted, "started"))
	if got := len(m.rd.activity); got != 2 {
		t.Errorf("activity entries = %d, want 2 (no duplicates by id)", got)
	}
	if got := strings.Count(m.View(), "go test"); got != 1 {
		t.Errorf("\"go test\" drawn %d times, want once", got)
	}

	// Frames of another run change nothing.
	other := testRun(42, schema.RunStateFailed)
	other.Task = "other task"
	m = frameRun(m, other)
	m = frameActivity(m, activityAt(7, 42, schema.ActivityKindReplyPosted, "other run entry"))
	m = frameIntervention(m, intervention(9, 42, schema.InterventionKindCancel, "", schema.InterventionStateRequested, ""))
	view := m.View()
	for _, bad := range []string{"other task", "other run entry", "#9 "} {
		if strings.Contains(view, bad) {
			t.Errorf("a frame of another run shows (%q):\n%s", bad, view)
		}
	}
	if m.rd.run.ID != 41 || len(m.rd.interventions) != 0 {
		t.Errorf("run %d, %d interventions; another run's frames were taken", m.rd.run.ID, len(m.rd.interventions))
	}

	// A repeated or older intervention document does not take the state back.
	applied := intervention(3, 41, schema.InterventionKindMessage, "x", schema.InterventionStateApplied, "")
	m = frameIntervention(frameIntervention(m, applied), applied)
	m = frameIntervention(m, intervention(3, 41, schema.InterventionKindMessage, "x", schema.InterventionStateRequested, ""))
	if len(m.rd.interventions) != 1 || m.rd.interventions[0].State != schema.InterventionStateApplied {
		t.Errorf("interventions = %+v, want one, applied", m.rd.interventions)
	}

	// A run does not leave a terminal state for an older document.
	m = frameRun(m, testRun(41, schema.RunStateCompleted))
	m = frameRun(m, testRun(41, schema.RunStateRunning))
	if m.rd.run.State != schema.RunStateCompleted {
		t.Errorf("run state = %s, want completed to stay", m.rd.run.State)
	}
}

// A read that was in flight when a frame landed is the older word.
func TestRunFrameBeatsAnInFlightRead(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	m := runModel(t, api, bothKinds...)
	m = pressKey(m, "enter", "down")
	m, cmd := update(m, tea.KeyMsg{Type: tea.KeyEnter})
	read := collect(cmd)
	// The frame lands before the results of the reads do.
	m = frameRun(m, testRun(41, schema.RunStateBlocked))
	for _, msg := range read {
		m, _ = update(m, msg)
	}
	if m.rd.run.State != schema.RunStateBlocked {
		t.Errorf("run state = %s, want the frame's blocked, not the read's running", m.rd.run.State)
	}
}

func TestRunDetailErrors(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantLine  string
		wantClose bool
	}{
		{"unsupported", &cli.ServerError{Status: 409, Code: schema.ErrorCodeInterventionUnsupported, Message: "no"},
			"run: this session does not accept that intervention", false},
		{"too many", &cli.ServerError{Status: 429, Code: schema.ErrorCodeTooManyInterventions, Message: "no"},
			"run: too many requests are waiting on this run; wait for the agent to answer", false},
		{"ended", &cli.ServerError{Status: 409, Code: schema.ErrorCodeInvalidTransition, Message: "no"},
			"run: the run has ended", false},
		{"not found", &cli.ServerError{Status: 404, Code: schema.ErrorCodeRunNotFound, Message: "no"},
			"run not found: it is gone, or you can no longer see it", true},
		{"anything else", errors.New("connection refused"), "run: connection refused", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newRunAPI(testRun(41, schema.RunStateRunning))
			m := openCurrent(runModel(t, api, bothKinds...))
			api.createErr = tt.err
			m = pressKey(m, "x", "y")
			if tt.wantClose {
				if m.mode != modeAgents || m.rd.open() {
					t.Fatalf("mode %v, open %v; the pane stays on a run that is gone", m.mode, m.rd.open())
				}
			} else if m.mode != modeRun {
				t.Fatalf("mode = %v, want the pane to stay", m.mode)
			}
			if got := m.status(); got != tt.wantLine {
				t.Errorf("status = %q, want %q", got, tt.wantLine)
			}
			if m.rd.sending {
				t.Error("still sending after the answer")
			}
			// An error is not a state change: the run line is as it was.
			if !tt.wantClose && !strings.Contains(m.View(), "run #41 · running") {
				t.Errorf("the run line changed:\n%s", m.View())
			}
		})
	}
}

func TestRunDetailReadsThatFindNoRunClose(t *testing.T) {
	gone := &cli.ServerError{Status: 404, Code: schema.ErrorCodeRunNotFound, Message: "no"}
	t.Run("on open", func(t *testing.T) {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		api.runErr = gone
		m := openCurrent(runModel(t, api, bothKinds...))
		if m.mode != modeAgents || !strings.Contains(m.status(), "run not found") {
			t.Errorf("mode %v, status %q; want the Agents view and one line", m.mode, m.status())
		}
		// The line is read once.
		if m = pressKey(m, "down"); m.status() != "agents: general" {
			t.Errorf("status after a key = %q", m.status())
		}
	})
	t.Run("while open, when the events socket is refused", func(t *testing.T) {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		m := openCurrent(runModel(t, api, bothKinds...))
		api.state.Lock()
		api.runErr = gone
		api.state.Unlock()
		// Membership removed: the socket's retry answers 404, and the pane
		// asks for the run once more.
		m, cmd := update(m, eventsEnded{gen: m.ev.gen, err: &cli.ServerError{Status: 404, Code: "channel_not_found", Message: "no"}})
		m = feed(m, cmd)
		if m.mode != modeAgents || m.rd.open() || !strings.Contains(m.status(), "run not found") {
			t.Errorf("mode %v, status %q; want the pane closed with one line", m.mode, m.status())
		}
	})
	t.Run("while open, on a reconnect", func(t *testing.T) {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		m := openCurrent(runModel(t, api, bothKinds...))
		before := api.gets
		m, _ = update(m, eventsEnded{gen: m.ev.gen, err: errors.New("dropped"), lived: time.Second})
		m, cmd := update(m, eventsRetryDue{gen: m.ev.gen})
		m = feed(m, cmd)
		// The new socket's first roster: everything is read again.
		worker := sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)
		worker.CurrentRun, worker.Interventions = 41, bothKinds
		api.state.Lock()
		api.run = testRun(41, schema.RunStateBlocked)
		api.state.Unlock()
		m, cmd = update(m, eventsRosterReceived{gen: m.ev.gen, roster: rosterOf(worker)})
		m = feed(m, cmd)
		if api.gets <= before || m.rd.run.State != schema.RunStateBlocked {
			t.Errorf("reads %d -> %d, state %s; want the run read again after the reconnect", before, api.gets, m.rd.run.State)
		}
		// A second roster of the same socket reads nothing.
		reads := api.gets
		m, cmd = update(m, eventsRosterReceived{gen: m.ev.gen, roster: rosterOf(worker)})
		feed(m, cmd)
		if api.gets != reads {
			t.Errorf("a roster of the same socket read the run again")
		}
	})
}

func TestMessageInput(t *testing.T) {
	tests := []struct {
		name       string
		typed      []string
		wantBody   string // "" for not sent
		wantStatus string
	}{
		{"a line", []string{"hello there"}, "hello there", ""},
		{"a tab is allowed", []string{"a\tb"}, "a\tb", ""},
		{"empty is not sent", nil, "", "run: type a message to send, or esc"},
		{"blanks are not sent", []string{"   "}, "", "run: type a message to send, or esc"},
		{"a newline is refused", []string{"one\ntwo"}, "", "run: a message holds no control characters other than tab"},
		{"an escape is refused", []string{"hi\x1b[31m"}, "", "run: a message holds no control characters other than tab"},
		{"a bell is refused", []string{"hi\a"}, "", "run: a message holds no control characters other than tab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newRunAPI(testRun(41, schema.RunStateRunning))
			m := openCurrent(runModel(t, api, bothKinds...))
			m = pressKey(m, "m")
			for _, s := range tt.typed {
				m, _ = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
			}
			m = pressKey(m, "enter")
			got := api.createdRequests()
			if tt.wantBody == "" {
				if len(got) != 0 {
					t.Fatalf("sent %+v, want nothing", got)
				}
				if m.status() != tt.wantStatus || m.rd.input != inputMessage {
					t.Errorf("status %q, input %v; want %q and the input still open", m.status(), m.rd.input, tt.wantStatus)
				}
				return
			}
			if len(got) != 1 || got[0].Body != tt.wantBody {
				t.Errorf("sent %+v, want one with body %q", got, tt.wantBody)
			}
		})
	}

	t.Run("esc drops the input, backspace edits it", func(t *testing.T) {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		m := openCurrent(runModel(t, api, bothKinds...))
		m = pressKey(m, "m", "abc", "backspace")
		if m.rd.draft != "ab" {
			t.Errorf("draft = %q, want ab", m.rd.draft)
		}
		if view := m.View(); !strings.Contains(view, "message> ab") {
			t.Errorf("the input is not drawn:\n%s", view)
		}
		m = pressKey(m, "esc")
		if m.rd.input != inputNone || m.rd.draft != "" || m.mode != modeRun {
			t.Errorf("after esc: input %v, draft %q, mode %v", m.rd.input, m.rd.draft, m.mode)
		}
		// The channel's draft was never touched.
		if m.input != "" {
			t.Errorf("channel draft = %q", m.input)
		}
	})
	t.Run("a second enter does not send twice", func(t *testing.T) {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		m := openCurrent(runModel(t, api, bothKinds...))
		m = pressKey(m, "m", "hi")
		m, cmd := update(m, tea.KeyMsg{Type: tea.KeyEnter})
		// While it is in flight the keys ask for nothing.
		m = pressKey(m, "m")
		m = pressKey(m, "x")
		if m.rd.input != inputNone || !m.rd.sending {
			t.Errorf("input %v, sending %v; want a send in flight and no new ask", m.rd.input, m.rd.sending)
		}
		feed(m, cmd)
		if got := len(api.createdRequests()); got != 1 {
			t.Errorf("sent %d requests, want 1", got)
		}
	})
	t.Run("the run ending while typing drops the input", func(t *testing.T) {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		m := openCurrent(runModel(t, api, bothKinds...))
		m = pressKey(m, "m", "hi")
		m = frameRun(m, testRun(41, schema.RunStateCompleted))
		m = pressKey(m, "enter")
		if m.rd.input != inputNone || len(api.createdRequests()) != 0 {
			t.Errorf("input %v, requests %+v; a message went to an ended run", m.rd.input, api.createdRequests())
		}
	})
}

func TestRunDetailOutcomeUnknown(t *testing.T) {
	tests := []struct {
		name   string
		status schema.SessionStatus
		unk    bool
		want   bool
	}{
		{"online session", schema.SessionStatusRunning, false, false},
		{"stale session", schema.SessionStatusStale, true, true},
		{"offline session", schema.SessionStatusOffline, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newRunAPI(testRun(41, schema.RunStateRunning))
			m := openCurrent(runModel(t, api, bothKinds...))
			s := sess(1, 7, schema.PrincipalAgent, "worker", tt.status)
			s.CurrentRun, s.OutcomeUnknown, s.Interventions = 41, tt.unk, bothKinds
			m = frame(m, rosterOf(s))
			if got := strings.Contains(m.View(), "run #41 · running · outcome unknown"); got != tt.want {
				t.Errorf("outcome unknown shown = %v, want %v:\n%s", got, tt.want, m.View())
			}
		})
	}
}

// Everything another person or agent wrote is printed as text.
func TestRunDetailPrintsHostileTextSafely(t *testing.T) {
	esc := "\x1b]0;pwned\x07\x1b[31m"
	run := testRun(41, schema.RunStateFailed)
	run.Task, run.Link, run.Outcome = "task"+esc+"end", "https://example.com/"+esc, "out"+esc+"come"
	api := newRunAPI(run)
	e := activityAt(1, 41, schema.ActivityKindReplyPosted, "sum"+esc+"mary")
	e.Outcome = "res" + esc + "ult"
	api.activity = []schema.AgentActivityV1{e}
	api.interventions = []schema.InterventionV1{
		intervention(3, 41, schema.InterventionKindMessage, "bo"+esc+"dy\nsecond", schema.InterventionStateFailed, "rea"+esc+"son"),
	}
	api.runs = []schema.RunV1{run}
	m := runModel(t, api, bothKinds...)
	m = pressKey(m, "enter")
	if view := m.View(); strings.Contains(view, "\x1b") || !strings.Contains(view, "task: ") {
		t.Errorf("the run list prints raw text:\n%q", view)
	}
	m = pressKey(m, "down", "down", "enter")
	view := m.View()
	if strings.ContainsAny(view, "\x1b\a") {
		t.Errorf("the view holds a control character:\n%q", view)
	}
	for _, want := range []string{"task: task", "outcome: out", "come", "sum", "mary", "res", "ult", "bo", "dy ↵ second", "failed: rea", "son"} {
		if !strings.Contains(view, want) {
			t.Errorf("run detail lacks %q:\n%s", want, view)
		}
	}
	// The status line is text too, whatever the server says.
	m.rd.status = "bad" + esc
	if strings.Contains(m.View(), "\x1b") {
		t.Error("the status line prints raw text")
	}
}

func TestRunDetailFitsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{40, 10}, {60, 14}, {80, 24}, {120, 40}} {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		for i := int64(1); i <= 40; i++ {
			api.activity = append(api.activity, activityAt(i, 41, schema.ActivityKindCommandStarted, strings.Repeat("long summary ", 10)))
		}
		for i := int64(1); i <= 6; i++ {
			api.interventions = append(api.interventions, intervention(i, 41, schema.InterventionKindMessage, strings.Repeat("body ", 40), schema.InterventionStateApplied, ""))
		}
		m := openCurrent(runModel(t, api, bothKinds...))
		m.width, m.height = size[0], size[1]
		m = pressKey(m, "m", "typing")
		lines := strings.Split(m.View(), "\n")
		if len(lines) != size[1] {
			t.Errorf("%dx%d: %d lines, want %d", size[0], size[1], len(lines), size[1])
		}
		for i, line := range lines {
			if w := cellWidth(line); w > size[0] {
				t.Errorf("%dx%d: line %d is %d cells: %q", size[0], size[1], i, w, line)
			}
		}
		if !strings.Contains(m.View(), "enter send") || !strings.Contains(m.View(), "message> typing") {
			t.Errorf("%dx%d: the input and its hint are not both shown:\n%s", size[0], size[1], m.View())
		}
	}
}

func TestRunDetailScrollsActivity(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	for i := int64(1); i <= 60; i++ {
		api.activity = append(api.activity, activityAt(i, 41, schema.ActivityKindCommandStarted, fmt.Sprintf("entry-%02d", i)))
	}
	m := openCurrent(runModel(t, api, bothKinds...))
	if view := m.View(); !strings.Contains(view, "entry-60") || strings.Contains(view, "entry-01") {
		t.Errorf("the newest entries are not what shows first:\n%s", view)
	}
	for range 20 {
		m = pressKey(m, "up")
	}
	if view := m.View(); strings.Contains(view, "entry-60") || !strings.Contains(view, "entry-40") {
		t.Errorf("up did not scroll back:\n%s", view)
	}
	for range 30 {
		m = pressKey(m, "down")
	}
	if m.rd.scroll != 0 {
		t.Errorf("scroll = %d, want back at the newest", m.rd.scroll)
	}
}

func TestSessionWithoutARunOpensNothing(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	idle := sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusIdle)
	m := agentModel(t, []schema.ChannelMemberV1{agentMember(7, "worker")}, idle)
	m.api = api
	m = pressKey(m, "enter", "down", "enter")
	if m.mode != modeAgents || m.status() != "session #1 has no run open" {
		t.Errorf("mode %v, status %q; want the Agents view and a line", m.mode, m.status())
	}
}

// The run list is kept by the run frames: a new run joins it, a changed one is
// replaced, and a list never read is not invented.
func TestRunFramesKeepTheRunList(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	api.runs = []schema.RunV1{testRun(30, schema.RunStateCompleted)}
	m := runModel(t, api, bothKinds...)
	key := sessionRunKey{"general", 1}
	m = frameRun(m, testRun(40, schema.RunStateRunning))
	if _, ok := m.sessionRuns[key]; ok {
		t.Fatal("a list that was never read appeared from a frame")
	}
	m = pressKey(m, "enter")
	m = frameRun(m, testRun(41, schema.RunStateRunning))
	m = frameRun(m, testRun(41, schema.RunStateCompleted))
	runs := m.sessionRuns[key]
	if len(runs) != 2 || runs[1].ID != 41 || runs[1].State != schema.RunStateCompleted {
		t.Errorf("runs = %+v, want 30 and 41 completed", runs)
	}
}

func TestRunFramesFromAnotherSocketAreDropped(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	m := openCurrent(runModel(t, api, bothKinds...))
	m, _ = update(m, eventsRunReceived{gen: m.ev.gen + 1, run: testRun(41, schema.RunStateCancelled)})
	m, _ = update(m, eventsActivityReceived{gen: m.ev.gen + 1, entry: activityAt(9, 41, schema.ActivityKindReplyPosted, "stale")})
	m, _ = update(m, eventsInterventionReceived{gen: m.ev.gen + 1, intervention: intervention(9, 41, schema.InterventionKindCancel, "", schema.InterventionStateRequested, "")})
	if m.rd.run.State != schema.RunStateRunning || len(m.rd.interventions) != 0 || strings.Contains(m.View(), "stale") {
		t.Errorf("frames of a closed socket were taken: %s, %d interventions", m.rd.run.State, len(m.rd.interventions))
	}
}

// A transport failure of a later read (the server down while the events
// socket retries) does not overwrite the server's reason for refusing a send:
// the header already says live updates are paused.
func TestRunDetailRefusalOutlivesTransportErrors(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	m := openCurrent(runModel(t, api, bothKinds...))
	api.createErr = &cli.ServerError{Status: 409, Code: schema.ErrorCodeInterventionUnsupported, Message: "no"}
	m = pressKey(m, "x", "y")
	const want = "run: this session does not accept that intervention"
	if got := m.status(); got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	m = m.runReadFailed(errors.New("connection refused"))
	if got := m.status(); got != want {
		t.Errorf("after a transport error the status = %q, want the refusal %q kept", got, want)
	}
	// Another refusal from the server does replace it.
	m = m.runReadFailed(&cli.ServerError{Status: 429, Code: schema.ErrorCodeTooManyInterventions, Message: "no"})
	if got := m.status(); !strings.Contains(got, "too many requests") {
		t.Errorf("a new refusal did not replace the old: %q", got)
	}
}
