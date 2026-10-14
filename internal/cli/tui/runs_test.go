package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// runsModel is a model in the Runs view of "general" with the given runs on the
// server, read through the keys: r in the Agents view.
func runsModel(t *testing.T, runs ...schema.RunV1) (Model, *runAPI) {
	t.Helper()
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	api.runs = runs
	m := pressKey(runModel(t, api, bothKinds...), "r")
	if m.mode != modeRuns {
		t.Fatalf("mode = %v after r, want the Runs view", m.mode)
	}
	return m, api
}

func taskRun(id int64, state schema.RunState, task string) schema.RunV1 {
	r := testRun(id, state)
	r.Task = task
	return r
}

// order is the position of each text in s, -1 for one that is not there.
func order(s string, texts ...string) []int {
	out := make([]int, len(texts))
	for i, text := range texts {
		out[i] = strings.Index(s, text)
	}
	return out
}

func TestRunsViewListsRunsNewestFirst(t *testing.T) {
	done := taskRun(12, schema.RunStateFailed, "job twelve")
	done.Outcome = "tests red"
	m, api := runsModel(t,
		taskRun(10, schema.RunStateCompleted, "job ten"),
		taskRun(11, schema.RunStateRunning, "job eleven"),
		done)

	if len(api.listRequests) != 1 {
		t.Fatalf("reads = %+v, want one", api.listRequests)
	}
	// Every run of the channel: no session filter; the newest page, 50 long.
	if got, want := api.listRequests[0], (runsRequest{session: 0, page: cli.RunsPage{NewestFirst: true}, limit: runsPageSize}); got != want {
		t.Errorf("read = %+v, want %+v", got, want)
	}
	view := m.View()
	pos := order(view, "job twelve", "job eleven", "job ten")
	if pos[0] < 0 || pos[0] > pos[1] || pos[1] > pos[2] {
		t.Errorf("runs are not newest first (positions %v):\n%s", pos, view)
	}
	for _, want := range []string{
		// state, the agent's name from the members list, task, start, and for
		// an open run its live duration, for an ended one its end and outcome.
		"failed", "worker", "job twelve", "11:55:00", "ended 11:59:00", "tests red",
		"running", "for 5m00s",
		"enter open", "f filter", "a agents",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the Runs view lacks %q:\n%s", want, view)
		}
	}
	if got := m.status(); got != "runs: general · all (3)" {
		t.Errorf("status = %q", got)
	}
	// Opening it asks again for nothing: the list is held.
	pressKey(m, "a", "r")
	if len(api.listRequests) != 1 {
		t.Errorf("going back and forth read %d times, want the one", len(api.listRequests))
	}
}

func TestRunsViewNamesAnAgentTheListLacks(t *testing.T) {
	r := taskRun(10, schema.RunStateRunning, "orphan")
	r.PrincipalID = 99
	m, _ := runsModel(t, r)
	if view := m.View(); !strings.Contains(view, "#99") {
		t.Errorf("an author the members list lacks is not #id:\n%s", view)
	}
}

func TestRunsViewFilter(t *testing.T) {
	m, _ := runsModel(t,
		taskRun(10, schema.RunStateCompleted, "job ten"),
		taskRun(11, schema.RunStateRunning, "job eleven"),
		taskRun(12, schema.RunStateBlocked, "job twelve"),
		taskRun(13, schema.RunStateCancelled, "job thirteen"))
	tests := []struct {
		keys   []string
		filter runFilter
		want   []string
		not    []string
	}{
		{nil, runsAll, []string{"job ten", "job eleven", "job twelve", "job thirteen"}, nil},
		{[]string{"f"}, runsOpen, []string{"job eleven", "job twelve"}, []string{"job ten", "job thirteen"}},
		{[]string{"f", "f"}, runsEnded, []string{"job ten", "job thirteen"}, []string{"job eleven", "job twelve"}},
		{[]string{"f", "f", "f"}, runsAll, []string{"job ten", "job eleven", "job twelve", "job thirteen"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.filter.String(), func(t *testing.T) {
			got := pressKey(m, tt.keys...)
			if got.runs.filter != tt.filter {
				t.Fatalf("filter = %v, want %v", got.runs.filter, tt.filter)
			}
			view := got.View()
			for _, want := range tt.want {
				if !strings.Contains(view, want) {
					t.Errorf("filter %v lacks %q:\n%s", tt.filter, want, view)
				}
			}
			for _, not := range tt.not {
				if strings.Contains(view, not) {
					t.Errorf("filter %v shows %q:\n%s", tt.filter, not, view)
				}
			}
			if want := fmt.Sprintf("runs: general · %v (%d)", tt.filter, len(tt.want)); got.status() != want {
				t.Errorf("status = %q, want %q", got.status(), want)
			}
		})
	}
	t.Run("a filter with nothing in it says so", func(t *testing.T) {
		m, _ := runsModel(t, taskRun(10, schema.RunStateCompleted, "job ten"))
		if view := pressKey(m, "f").View(); !strings.Contains(view, "no open runs here") {
			t.Errorf("an empty filter is silent:\n%s", view)
		}
	})
	t.Run("the selection stays on a run the filter keeps", func(t *testing.T) {
		got := pressKey(m, "down", "down", "f") // job eleven
		vis := got.runs.visible()
		if vis[got.runs.selected(vis)].ID != 11 {
			t.Errorf("selected %d after the filter, want 11", vis[got.runs.selected(vis)].ID)
		}
	})
	t.Run("the keys do not touch the channel's draft", func(t *testing.T) {
		got := pressKey(m, "f", "a", "r", "backspace")
		if got.input != "" {
			t.Errorf("draft = %q", got.input)
		}
	})
}

func TestRunsViewPagesOnScroll(t *testing.T) {
	var runs []schema.RunV1
	for i := int64(1); i <= 120; i++ {
		runs = append(runs, taskRun(i, schema.RunStateCompleted, fmt.Sprintf("job %03d", i)))
	}
	m, api := runsModel(t, runs...)
	if len(m.runs.list) != runsPageSize || !m.runs.more {
		t.Fatalf("first page: %d runs, more %v; want %d and more", len(m.runs.list), m.runs.more, runsPageSize)
	}
	if m.runs.list[0].ID != 120 || m.runs.list[runsPageSize-1].ID != 71 {
		t.Fatalf("first page is %d..%d, want 120..71", m.runs.list[0].ID, m.runs.list[runsPageSize-1].ID)
	}
	// Scrolling to the end reads the next page, below the smallest id held.
	for range runsPageSize - 1 {
		m = pressKey(m, "down")
	}
	if len(api.listRequests) != 2 || api.listRequests[1].page != (cli.RunsPage{NewestFirst: true, Before: 71}) {
		t.Fatalf("reads = %+v, want a second one before 71", api.listRequests)
	}
	if len(m.runs.list) != 100 {
		t.Fatalf("%d runs after the second page, want 100", len(m.runs.list))
	}
	for range 60 {
		m = pressKey(m, "down")
	}
	// The last page is short: nothing more is asked for.
	if len(m.runs.list) != 120 || m.runs.more {
		t.Errorf("%d runs, more %v; want all 120 and no more", len(m.runs.list), m.runs.more)
	}
	reads := len(api.listRequests)
	m = pressKey(m, "down", "down")
	if len(api.listRequests) != reads {
		t.Errorf("scrolling past the end read again")
	}
	if view := pressKey(m, "pgup").View(); !strings.Contains(view, "job") {
		t.Errorf("pgup left no runs on the screen:\n%s", view)
	}
}

func TestRunsViewFilterPullsPagesUntilItFills(t *testing.T) {
	// One open run among old ended ones: the open filter must reach it.
	var runs []schema.RunV1
	runs = append(runs, taskRun(1, schema.RunStateRunning, "the one open"))
	for i := int64(2); i <= 120; i++ {
		runs = append(runs, taskRun(i, schema.RunStateCompleted, fmt.Sprintf("done %03d", i)))
	}
	m, _ := runsModel(t, runs...)
	m = pressKey(m, "f")
	if view := m.View(); !strings.Contains(view, "the one open") {
		t.Errorf("the open filter did not read on to the open run:\n%s", view)
	}
}

func TestRunsViewFrames(t *testing.T) {
	m, _ := runsModel(t,
		taskRun(10, schema.RunStateRunning, "job ten"),
		taskRun(11, schema.RunStateRunning, "job eleven"))
	t.Run("a new run joins at the top", func(t *testing.T) {
		got := frameRun(m, taskRun(12, schema.RunStateQueued, "job twelve"))
		view := got.View()
		pos := order(view, "job twelve", "job eleven")
		if pos[0] < 0 || pos[0] > pos[1] || !strings.Contains(view, "queued") || got.status() != "runs: general · all (3)" {
			t.Errorf("the new run is not at the top of the next render:\n%s", view)
		}
	})
	t.Run("a changed run is replaced", func(t *testing.T) {
		ended := taskRun(10, schema.RunStateCompleted, "job ten")
		ended.Outcome = "all green"
		view := frameRun(m, ended).View()
		if !strings.Contains(view, "all green") || !strings.Contains(view, "completed") || strings.Count(view, "job ten") != 1 {
			t.Errorf("the changed run is not replaced:\n%s", view)
		}
	})
	t.Run("an older document of an ended run is not taken", func(t *testing.T) {
		got := frameRun(m, taskRun(10, schema.RunStateCompleted, "job ten"))
		got = frameRun(got, taskRun(10, schema.RunStateRunning, "job ten"))
		if view := got.View(); !strings.Contains(view, "completed") {
			t.Errorf("an ended run came back to life:\n%s", view)
		}
	})
	t.Run("the filter follows the frame", func(t *testing.T) {
		got := pressKey(m, "f") // open
		got = frameRun(got, taskRun(10, schema.RunStateFailed, "job ten"))
		if view := got.View(); strings.Contains(view, "job ten") || !strings.Contains(view, "job eleven") {
			t.Errorf("an ended run stays in the open filter:\n%s", view)
		}
	})
	t.Run("the selection does not move", func(t *testing.T) {
		got := pressKey(m, "down") // job ten
		got = frameRun(got, taskRun(12, schema.RunStateQueued, "job twelve"))
		vis := got.runs.visible()
		if vis[got.runs.selected(vis)].ID != 10 {
			t.Errorf("selected %d after a new run, want 10", vis[got.runs.selected(vis)].ID)
		}
	})
	t.Run("a frame from the other socket is dropped", func(t *testing.T) {
		got, _ := update(m, eventsRunReceived{gen: m.ev.gen + 1, run: taskRun(12, schema.RunStateQueued, "job twelve")})
		if strings.Contains(got.View(), "job twelve") {
			t.Error("a frame of a closed socket joined the list")
		}
	})
	t.Run("a run older than the pages read is left to paging", func(t *testing.T) {
		got := m
		got.runs.more, got.runs.before = true, 10
		got = frameRun(got, taskRun(5, schema.RunStateCompleted, "job five"))
		if strings.Contains(got.View(), "job five") {
			t.Error("a run below the pages read was added, which would skip the ones between")
		}
	})
	t.Run("a channel the view is not of is not touched", func(t *testing.T) {
		got := m
		got.runs.channel = "ops"
		got = got.runsFrame(taskRun(12, schema.RunStateQueued, "job twelve"))
		if len(got.runs.list) != 2 {
			t.Errorf("%d runs, want the 2 it had", len(got.runs.list))
		}
	})
}

func TestRunsViewIsPerChannel(t *testing.T) {
	m, api := runsModel(t, taskRun(10, schema.RunStateRunning, "job ten"))
	m = pressKey(m, "tab", "down") // channels, then ops
	if m.runs.channel != "ops" || len(m.runs.list) != 0 {
		t.Fatalf("runs of %q: %d held; want an empty list for ops", m.runs.channel, len(m.runs.list))
	}
	reads := len(api.listRequests)
	m = pressKey(m, "tab", "tab", "r") // inbox, agents, runs
	if len(api.listRequests) != reads+1 {
		t.Errorf("the other channel's runs were not read: %d reads, had %d", len(api.listRequests), reads)
	}
}

func TestRunsViewReadFailure(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	api.listErr = &cli.ServerError{Status: 500, Code: "internal", Message: "boom"}
	m := pressKey(runModel(t, api, bothKinds...), "r")
	if view := m.View(); !strings.Contains(view, "could not list runs") {
		t.Errorf("a failed read is silent:\n%s", view)
	}
	// Going back in reads again.
	api.listErr = nil
	api.runs = []schema.RunV1{taskRun(10, schema.RunStateRunning, "job ten")}
	m = pressKey(m, "a", "r")
	if view := m.View(); !strings.Contains(view, "job ten") {
		t.Errorf("the retry did not list the run:\n%s", view)
	}
}

func TestRunsViewReadsAgainWhenTheSocketComesBack(t *testing.T) {
	m, api := runsModel(t, taskRun(10, schema.RunStateRunning, "job ten"))
	m, _ = update(m, eventsEnded{gen: m.ev.gen, err: fmt.Errorf("dropped"), lived: time.Second})
	m, cmd := update(m, eventsRetryDue{gen: m.ev.gen})
	m = feed(m, cmd)
	if view := m.View(); !strings.Contains(view, "live updates paused") {
		t.Errorf("a dropped socket is not said:\n%s", view)
	}
	// While it was down job ten ended and job eleven began.
	api.state.Lock()
	api.runs = []schema.RunV1{taskRun(10, schema.RunStateCompleted, "job ten"), taskRun(11, schema.RunStateRunning, "job eleven")}
	api.state.Unlock()
	worker := sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)
	m, cmd = update(m, eventsRosterReceived{gen: m.ev.gen, roster: rosterOf(worker)})
	m = feed(m, cmd)
	view := m.View()
	if !strings.Contains(view, "job eleven") || !strings.Contains(view, "completed") || strings.Contains(view, "live updates paused") {
		t.Errorf("the new socket's first roster did not bring the list up to date:\n%s", view)
	}
}

// Opening a run from the Runs view and from the Agents view reaches the same
// run detail; esc goes back to where each came from.
func TestRunDetailOpensFromRunsAndAgentsTheSame(t *testing.T) {
	build := func() *runAPI {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		api.runs = []schema.RunV1{testRun(41, schema.RunStateRunning)}
		api.activity = []schema.AgentActivityV1{activityAt(1, 41, schema.ActivityKindRunStarted, "started the job")}
		return api
	}
	fromAgents := openCurrent(runModel(t, build(), bothKinds...))

	api := build()
	fromRuns := pressKey(runModel(t, api, bothKinds...), "r", "enter")

	if fromAgents.mode != modeRun || fromRuns.mode != modeRun || fromAgents.rd.runID != 41 || fromRuns.rd.runID != 41 {
		t.Fatalf("modes %v and %v, runs %d and %d; want run detail on 41 both ways", fromAgents.mode, fromRuns.mode, fromAgents.rd.runID, fromRuns.rd.runID)
	}
	if a, b := fromAgents.View(), fromRuns.View(); a != b {
		t.Errorf("the two ways in show different panes:\nagents:\n%s\nruns:\n%s", a, b)
	}
	if back := pressKey(fromRuns, "esc"); back.mode != modeRuns || back.rd.open() || back.runs.sel != 41 {
		t.Errorf("esc from a run opened in the Runs view: mode %v, open %v; want the Runs view", back.mode, back.rd.open())
	}
	if back := pressKey(fromAgents, "esc"); back.mode != modeAgents {
		t.Errorf("esc from a run opened in the Agents view: mode %v", back.mode)
	}
}

func TestRunsViewRunLostClosesThePaneWithOneLine(t *testing.T) {
	m, api := runsModel(t, taskRun(10, schema.RunStateRunning, "job ten"))
	api.runErr = &cli.ServerError{Status: 404, Code: schema.ErrorCodeRunNotFound, Message: "no"}
	m = pressKey(m, "enter")
	if m.mode != modeRuns || m.rd.open() || !strings.Contains(m.status(), "run not found") {
		t.Fatalf("mode %v, open %v, status %q; want the Runs view and one line", m.mode, m.rd.open(), m.status())
	}
	// The line is read once.
	if m = pressKey(m, "down"); strings.Contains(m.status(), "run not found") {
		t.Errorf("status after a key = %q", m.status())
	}
}

func TestRunsViewKeys(t *testing.T) {
	m, _ := runsModel(t, taskRun(10, schema.RunStateRunning, "job ten"))
	if got := pressKey(m, "a"); got.mode != modeAgents {
		t.Errorf("a: mode %v, want the Agents view", got.mode)
	}
	if got := pressKey(m, "tab"); got.mode != modeChannels {
		t.Errorf("tab: mode %v, want the channels", got.mode)
	}
	if _, cmd := update(m, tea.KeyMsg{Type: tea.KeyEsc}); cmd == nil {
		t.Error("esc did not quit")
	}
	// The hint line says how to get in and around.
	agents := pressKey(m, "a")
	if view := agents.View(); !strings.Contains(view, "r runs") {
		t.Errorf("the Agents view does not say how to reach the Runs view:\n%s", view)
	}
}

func TestRunsViewFitsTheTerminal(t *testing.T) {
	var runs []schema.RunV1
	for i := int64(1); i <= 30; i++ {
		r := taskRun(i, schema.RunStateCompleted, strings.Repeat("long task ", 12))
		r.Outcome = strings.Repeat("outcome ", 10)
		runs = append(runs, r)
	}
	for _, size := range [][2]int{{40, 10}, {60, 14}, {80, 24}, {120, 40}} {
		m, _ := runsModel(t, runs...)
		m.width, m.height = size[0], size[1]
		lines := strings.Split(m.View(), "\n")
		if len(lines) != size[1] {
			t.Errorf("%dx%d: %d lines, want %d", size[0], size[1], len(lines), size[1])
		}
		for i, line := range lines {
			if w := cellWidth(line); w > size[0] {
				t.Errorf("%dx%d: line %d is %d cells: %q", size[0], size[1], i, w, line)
			}
		}
		if !strings.Contains(m.View(), "completed") {
			t.Errorf("%dx%d: no run on the screen:\n%s", size[0], size[1], m.View())
		}
	}
}

func TestRunsViewPrintsHostileTextSafely(t *testing.T) {
	esc := "\x1b]0;pwned\x07\x1b[31m"
	r := taskRun(10, schema.RunStateFailed, "task"+esc+"end")
	r.Outcome = "out" + esc + "come"
	m, _ := runsModel(t, r)
	if view := m.View(); strings.ContainsAny(view, "\x1b\a") || !strings.Contains(view, "task") {
		t.Errorf("the Runs view prints raw text:\n%q", view)
	}
}

func TestSpanText(t *testing.T) {
	tests := []struct {
		d       time.Duration
		tenths  bool
		want    string
		comment string
	}{
		{0, true, "0.0s", ""},
		{12400 * time.Millisecond, true, "12.4s", ""},
		{12400 * time.Millisecond, false, "12s", "whole seconds in a gap"},
		{59*time.Second + 990*time.Millisecond, true, "59.9s", "never rounds up to 60"},
		{3*time.Minute + 2*time.Second, true, "3m02s", ""},
		{time.Hour + 2*time.Minute + 3*time.Second, true, "1h02m03s", ""},
		{-time.Second, true, "0.0s", "clocks that disagree read as none"},
	}
	for _, tt := range tests {
		if got := spanText(tt.d, tt.tenths); got != tt.want {
			t.Errorf("spanText(%v, %v) = %q, want %q (%s)", tt.d, tt.tenths, got, tt.want, tt.comment)
		}
	}
}

// An open run's duration moves because the views redraw once a second while
// they are up, and only then.
func TestClockTicksOnlyInTheViewsThatShowADuration(t *testing.T) {
	m, _ := runsModel(t, taskRun(10, schema.RunStateRunning, "job ten"))
	if !m.clockTicking {
		t.Fatal("entering the Runs view did not start the tick")
	}
	if _, cmd := update(m, clockTick{}); cmd == nil {
		t.Error("a tick in the Runs view did not arm the next")
	}
	// Run detail keeps it going; opening it does not start a second chain.
	detail := pressKey(m, "enter")
	if _, cmd := update(detail, clockTick{}); cmd == nil {
		t.Error("a tick in run detail did not arm the next")
	}
	// Elsewhere the next tick ends it, and coming back starts one again.
	away := pressKey(m, "tab")
	away, cmd := update(away, clockTick{})
	if cmd != nil || away.clockTicking {
		t.Errorf("a tick in the channels view carried on (cmd %v, ticking %v)", cmd != nil, away.clockTicking)
	}
	back := pressKey(pressKey(away, "tab", "tab"), "r")
	if !back.clockTicking {
		t.Error("coming back did not start the tick")
	}
	// The duration it redraws is from the clock the model holds.
	m.now = func() time.Time { return testNow.Add(30 * time.Second) }
	if view := m.View(); !strings.Contains(view, "for 5m30s") {
		t.Errorf("the open run's duration did not move:\n%s", view)
	}
}
