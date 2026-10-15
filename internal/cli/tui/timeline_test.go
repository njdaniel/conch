package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// runStart is when the timeline tests' runs began; testRun starts at
// testNow-5m, and these offsets are from there.
var runStart = testNow.Add(-5 * time.Minute)

func entryAt(id int64, offset time.Duration, summary string) schema.AgentActivityV1 {
	e := activityAt(id, 41, schema.ActivityKindCommandStarted, summary)
	e.At = schema.NewTimestamp(runStart.Add(offset))
	return e
}

func ivAt(id int64, ask time.Duration, state schema.InterventionState, answered time.Duration) schema.InterventionV1 {
	iv := intervention(id, 41, schema.InterventionKindMessage, "use the cache", state, "")
	iv.RequestedAt = schema.NewTimestamp(runStart.Add(ask))
	if state != schema.InterventionStateRequested {
		at := schema.NewTimestamp(runStart.Add(answered))
		iv.AnsweredAt = &at
	}
	return iv
}

// lineWith is the line of view that holds text, "" when none does.
func lineWith(view, text string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, text) {
			return line
		}
	}
	return ""
}

func TestRunDetailTimelineOffsetsAndGaps(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	api.activity = []schema.AgentActivityV1{
		entryAt(1, 0, "first"),
		entryAt(2, 400*time.Millisecond, "second"),
		entryAt(3, 12400*time.Millisecond, "third"),
		entryAt(4, 3*time.Minute+2*time.Second, "fourth"),
		// Before the run's start by the server's clock: reads as the start.
		entryAt(5, -time.Second, "skewed"),
	}
	m := openCurrent(runModel(t, api, bothKinds...))
	view := m.View()
	tests := []struct {
		text, offset, gap string
	}{
		{"skewed", "+0.0s", ""},
		// The skewed entry sorts before it, a second earlier.
		{"first", "+0.0s", "(+1s)"},
		// Under a second: no gap shown.
		{"second", "+0.4s", ""},
		{"third", "+12.4s", "(+12s)"},
		// Gaps use the same minutes-and-seconds form as offsets.
		{"fourth", "+3m02s", "(+2m49s)"},
	}
	for _, tt := range tests {
		line := lineWith(view, tt.text)
		if line == "" || !strings.Contains(line, tt.offset+" ") {
			t.Errorf("%s: line %q lacks offset %s\n%s", tt.text, line, tt.offset, view)
			continue
		}
		if tt.gap != "" && !strings.Contains(line, tt.gap) {
			t.Errorf("%s: line %q lacks gap %s", tt.text, line, tt.gap)
		}
		if tt.gap == "" && strings.Contains(line, "(+") {
			t.Errorf("%s: line %q shows a gap, want none", tt.text, line)
		}
	}
}

func TestRunDetailShowsDuration(t *testing.T) {
	tests := []struct {
		name  string
		run   schema.RunV1
		want  string
		notIn string
	}{
		// The clock is fixed at testNow: an open run's duration is to it.
		{"open", testRun(41, schema.RunStateRunning), "duration: 5m00s so far", ""},
		// An ended run's is final: started at -5m, ended at -1m.
		{"ended", testRun(41, schema.RunStateCompleted), "duration: 4m00s", "so far"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := openCurrent(runModel(t, newRunAPI(tt.run), bothKinds...))
			view := m.View()
			if !strings.Contains(view, tt.want) || (tt.notIn != "" && strings.Contains(view, tt.notIn)) {
				t.Errorf("view lacks %q (and must lack %q):\n%s", tt.want, tt.notIn, view)
			}
		})
	}
	t.Run("an open run's duration follows the clock", func(t *testing.T) {
		m := openCurrent(runModel(t, newRunAPI(testRun(41, schema.RunStateRunning)), bothKinds...))
		m.now = func() time.Time { return testNow.Add(90 * time.Second) }
		if view := m.View(); !strings.Contains(view, "duration: 6m30s so far") {
			t.Errorf("the duration did not move with the clock:\n%s", view)
		}
	})
	t.Run("a run frame ends it", func(t *testing.T) {
		m := openCurrent(runModel(t, newRunAPI(testRun(41, schema.RunStateRunning)), bothKinds...))
		m = frameRun(m, testRun(41, schema.RunStateFailed))
		if view := m.View(); !strings.Contains(view, "duration: 4m00s") || strings.Contains(view, "so far") {
			t.Errorf("an ended run's duration is not final:\n%s", view)
		}
	})
}

// Requests and answers sit among the activity in the order of time, each
// marked ↳, and the separate history section is gone.
func TestRunDetailInterleavesInterventionsInTime(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	api.activity = []schema.AgentActivityV1{
		entryAt(1, 1*time.Second, "act-one"),
		entryAt(2, 20*time.Second, "act-two"),
		entryAt(3, 40*time.Second, "act-three"),
	}
	api.interventions = []schema.InterventionV1{
		// Asked between act-one and act-two, answered between act-two and
		// act-three.
		ivAt(7, 10*time.Second, schema.InterventionStateApplied, 30*time.Second),
		// Asked after everything and not yet answered.
		ivAt(8, 50*time.Second, schema.InterventionStateRequested, 0),
	}
	m := openCurrent(runModel(t, api, bothKinds...))
	view := m.View()
	want := []string{"act-one", "↳ #7 message · asked · use the cache", "act-two", "↳ #7 applied", "act-three", "↳ #8 message · requested… · use the cache"}
	pos := order(view, want...)
	for i := range pos {
		if pos[i] < 0 || (i > 0 && pos[i] < pos[i-1]) {
			t.Fatalf("the timeline is not in time order (positions %v of %q):\n%s", pos, want, view)
		}
	}
	if strings.Contains(view, "interventions (") && !strings.Contains(view, "↳ interventions (2)") {
		t.Errorf("a separate interventions section is still there:\n%s", view)
	}
	// A live answer moves the pending request on to its answer line.
	m = frameIntervention(m, ivAt(8, 50*time.Second, schema.InterventionStateFailed, 55*time.Second))
	got := m.View()
	if strings.Contains(got, "requested…") || !strings.Contains(got, "↳ #8 failed") {
		t.Errorf("the answer did not replace the pending request:\n%s", got)
	}
	// Offsets of the interventions are from the run's start like any entry's.
	if line := lineWith(got, "↳ #7 applied"); !strings.Contains(line, "+30.0s") || !strings.Contains(line, "(+10s)") {
		t.Errorf("answer line %q lacks its offset and gap", line)
	}
	// The cursor walks through all of it: nine lines back from act-two.
	if n := len(m.rd.timeline()); n != 3+2+2 {
		t.Errorf("timeline has %d lines, want 7", n)
	}
}

func TestRunDetailFoldsAndUnfoldsData(t *testing.T) {
	small := entryAt(2, 5*time.Second, "has-data")
	small.Data = json.RawMessage(`{"cmd":"go test","exit":0}`)
	big := entryAt(3, 6*time.Second, "has-big-data")
	big.Data = json.RawMessage(`{"blob":"` + strings.Repeat("x", 5000) + `"}`)
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	api.activity = []schema.AgentActivityV1{entryAt(1, time.Second, "no-data"), small, big}
	m := openCurrent(runModel(t, api, bothKinds...))
	if view := m.View(); !strings.Contains(view, "▸ command_started has-data") {
		t.Fatalf("an entry with data is not marked as foldable:\n%s", view)
	}

	t.Run("pretty-printed, then folded again", func(t *testing.T) {
		got := pressKey(m, "up", "enter") // has-data
		view := got.View()
		for _, want := range []string{"▾ command_started has-data", `┊ {`, `┊   "cmd": "go test",`, `┊   "exit": 0`} {
			if !strings.Contains(view, want) {
				t.Errorf("unfolded view lacks %q:\n%s", want, view)
			}
		}
		// The body sits right under its entry, which stays the cursor's line.
		pos := order(view, "has-data", `"cmd"`)
		if pos[0] < 0 || pos[1] < pos[0] {
			t.Errorf("the data is not under its entry:\n%s", view)
		}
		got = pressKey(got, "enter")
		if view := got.View(); strings.Contains(view, `"cmd"`) || !strings.Contains(view, "▸ command_started has-data") || got.rd.unfold != 0 {
			t.Errorf("a second enter did not fold it:\n%s", view)
		}
	})
	t.Run("a body over 2 KiB is cut with an ellipsis", func(t *testing.T) {
		got := pressKey(m, "enter") // has-big-data, the newest
		body := got.dataLines(98)
		if len(body) == 0 {
			t.Fatalf("no data lines:\n%s", got.View())
		}
		text := strings.Join(body, "")
		x := strings.Count(text, "x")
		// 2 KiB of pretty-printed JSON: the opening brace, the key and quote
		// are in it, so the x's are fewer than the 2048 bytes.
		if x >= 2048 || x < 2000 {
			t.Errorf("%d x's shown, want just under 2048", x)
		}
		if !strings.HasSuffix(strings.TrimRight(text, " "), "…") {
			t.Errorf("a cut body does not end in …: %q", body[len(body)-1])
		}
		// A body that is not cut has no ellipsis.
		small := dataBody([]byte(`{"a":1}`), 98)
		if strings.Contains(strings.Join(small, ""), "…") {
			t.Errorf("a short body is marked as cut: %q", small)
		}
	})
	t.Run("a body longer than the pane is scrolled with pgdn", func(t *testing.T) {
		got := pressKey(m, "enter")
		view := got.View()
		if len(strings.Split(view, "\n")) != got.height || !strings.Contains(view, "more data (pgdn)") {
			t.Fatalf("a long body does not say there is more (or breaks the layout):\n%s", view)
		}
		first := got.rd.bodyScroll
		got = pressKey(got, "pgdown")
		if got.rd.bodyScroll <= first || got.rd.scroll != 0 {
			t.Errorf("pgdn scrolled the body to %d (timeline %d), want the body down and the cursor kept", got.rd.bodyScroll, got.rd.scroll)
		}
		got = pressKey(got, "pgup")
		if got.rd.bodyScroll != first {
			t.Errorf("pgup left the body at %d, want %d", got.rd.bodyScroll, first)
		}
	})
	t.Run("an entry without data says so", func(t *testing.T) {
		got := pressKey(m, "up", "up", "enter") // no-data
		if got.status() != "run: this entry has no data" || got.rd.unfold != 0 {
			t.Errorf("status = %q, unfold %d; want the line and nothing unfolded", got.status(), got.rd.unfold)
		}
		// The line is read once.
		if got = pressKey(got, "down"); got.status() != "run #41" {
			t.Errorf("status after a key = %q", got.status())
		}
	})
	t.Run("an intervention has no data either", func(t *testing.T) {
		api := newRunAPI(testRun(41, schema.RunStateRunning))
		api.interventions = []schema.InterventionV1{ivAt(7, time.Second, schema.InterventionStateRequested, 0)}
		got := pressKey(openCurrent(runModel(t, api, bothKinds...)), "enter")
		if got.status() != "run: this entry has no data" {
			t.Errorf("status = %q", got.status())
		}
	})
	t.Run("moving the cursor folds it", func(t *testing.T) {
		got := pressKey(m, "up", "enter", "down")
		if got.rd.unfold != 0 || strings.Contains(got.View(), `"cmd"`) {
			t.Errorf("the data stayed open when the cursor moved:\n%s", got.View())
		}
	})
	t.Run("an empty timeline says there is no data", func(t *testing.T) {
		got := pressKey(openCurrent(runModel(t, newRunAPI(testRun(41, schema.RunStateRunning)), bothKinds...)), "enter")
		if got.status() != "run: this entry has no data" {
			t.Errorf("status = %q", got.status())
		}
	})
	t.Run("data that is not JSON is shown as it came, safely", func(t *testing.T) {
		lines := dataBody([]byte("plain \x1b[31mtext\nsecond"), 60)
		text := strings.Join(lines, "\n")
		if strings.Contains(text, "\x1b") || !strings.Contains(text, "plain [31mtext") || !strings.Contains(text, "second") {
			t.Errorf("lines = %q", lines)
		}
	})
}

func TestRunDetailDataBodyCutsOnARuneBoundary(t *testing.T) {
	// A body whose 2048th byte falls inside a character.
	data := `"` + strings.Repeat("é", 1500) + `"`
	lines := dataBody([]byte(data), 200)
	text := strings.Join(lines, "")
	if !strings.HasSuffix(text, "…") {
		t.Fatalf("not cut: %d bytes", len(text))
	}
	for _, r := range text {
		if r == '\uFFFD' {
			t.Fatal("the cut split a character")
		}
	}
}

func TestRunDetailGoToMessage(t *testing.T) {
	var messages []schema.MessageV2
	for i := int64(1); i <= 60; i++ {
		messages = append(messages, schema.MessageV2{
			Schema: schema.MessageSchemaV2, ID: i, ChannelID: 1, AuthorID: 5,
			CreatedAt: schema.NewTimestamp(testNow.Add(-time.Duration(100-i) * time.Minute)),
			Body:      fmt.Sprintf("msg-%02d", i),
		})
	}
	open := func(messageID int64) Model {
		run := testRun(41, schema.RunStateRunning)
		run.MessageID = messageID
		m := openCurrent(runModel(t, newRunAPI(run), bothKinds...))
		m.width, m.height = 100, 14
		m.messages["general"] = messages
		return m
	}
	t.Run("lands on the answered message", func(t *testing.T) {
		m := pressKey(open(10), "g")
		if m.mode != modeChannels || m.rd.open() {
			t.Fatalf("mode %v, open %v; want the channels view", m.mode, m.rd.open())
		}
		view := m.View()
		if !strings.Contains(view, "msg-10") || strings.Contains(view, "msg-60") {
			t.Errorf("the message pane is not at message 10:\n%s", view)
		}
		if got := m.status(); !strings.Contains(got, "run answers message #10") {
			t.Errorf("status = %q", got)
		}
	})
	t.Run("the newest message needs no scroll", func(t *testing.T) {
		m := pressKey(open(60), "g")
		if m.scroll != 0 || !strings.Contains(m.View(), "msg-60") {
			t.Errorf("scroll %d; want the newest in view:\n%s", m.scroll, m.View())
		}
	})
	t.Run("a run that answers no message says so", func(t *testing.T) {
		m := pressKey(open(0), "g")
		if m.mode != modeRun || m.status() != "run: no message" {
			t.Errorf("mode %v, status %q; want run detail and %q", m.mode, m.status(), "run: no message")
		}
		// The line is read once.
		if m = pressKey(m, "up"); strings.Contains(m.status(), "no message") {
			t.Errorf("status after a key = %q", m.status())
		}
	})
	t.Run("a message older than the history held says so", func(t *testing.T) {
		m := pressKey(open(999), "g")
		if !strings.Contains(m.status(), "#999") || !strings.Contains(m.status(), "older") {
			t.Errorf("status = %q", m.status())
		}
	})
	t.Run("typing a g in a message is not the key", func(t *testing.T) {
		run := testRun(41, schema.RunStateRunning)
		run.MessageID = 10
		m := openCurrent(runModel(t, newRunAPI(run), bothKinds...))
		m = pressKey(m, "m", "g")
		if m.mode != modeRun || m.rd.draft != "g" {
			t.Errorf("mode %v, draft %q; want the g typed", m.mode, m.rd.draft)
		}
	})
}

func TestRunDetailEnterWhileTypingStillSends(t *testing.T) {
	api := newRunAPI(testRun(41, schema.RunStateRunning))
	m := openCurrent(runModel(t, api, bothKinds...))
	pressKey(m, "m", "hi", "enter")
	if got := api.createdRequests(); len(got) != 1 || got[0].Body != "hi" {
		t.Errorf("requests = %+v, want the message sent, not the data toggled", got)
	}
}
