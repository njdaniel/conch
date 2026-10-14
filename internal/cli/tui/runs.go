package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// runsPageSize is how many runs one read of the Runs view asks for.
const runsPageSize = 50

// runFilter is which runs the Runs view lists: f cycles it.
type runFilter int

const (
	runsAll runFilter = iota
	runsOpen
	runsEnded
)

func (f runFilter) String() string {
	switch f {
	case runsOpen:
		return "open"
	case runsEnded:
		return "ended"
	}
	return "all"
}

func (f runFilter) next() runFilter { return (f + 1) % 3 }

// keeps reports whether the filter lists r. A run is open until its state is
// one a run never leaves.
func (f runFilter) keeps(r schema.RunV1) bool {
	switch f {
	case runsOpen:
		return !schema.RunStateTerminal(r.State)
	case runsEnded:
		return schema.RunStateTerminal(r.State)
	}
	return true
}

// runsView is the Runs view: the runs of one channel, newest first, read a page
// at a time and kept current by the run frames. The list is the channel's, so
// switching channels drops it.
type runsView struct {
	channel string
	// list is every run held, by id, newest first.
	list []schema.RunV1
	// loaded is true once the first page is in; loading while a page is asked
	// for; more while an older page may exist.
	loaded, loading, more bool
	// before is the smallest id the pages read so far reached: the next page
	// starts below it. A run that a frame brought in does not move it, or the
	// runs between would never be read.
	before int64
	pages  int
	filter runFilter
	// sel is the selected run, by id, so the selection does not move when the
	// list does.
	sel int64
	// note is the one line shown in place of the status until the next key:
	// why run detail closed, or a failed read.
	note string
}

// runsLoaded is a page of the Runs view. before is the page it was asked for, 0
// for the newest.
type runsLoaded struct {
	channel string
	before  int64
	runs    []schema.RunV1
	err     error
}

// visible is the runs the filter lists.
func (rv runsView) visible() []schema.RunV1 {
	if rv.filter == runsAll {
		return rv.list
	}
	var out []schema.RunV1
	for _, r := range rv.list {
		if rv.filter.keeps(r) {
			out = append(out, r)
		}
	}
	return out
}

// selected is the index in vis of the selected run: the one the user chose, or
// the first when that one is gone or filtered out.
func (rv runsView) selected(vis []schema.RunV1) int {
	for i, r := range vis {
		if r.ID == rv.sel {
			return i
		}
	}
	return 0
}

// mergeRuns puts incoming into list by id, newest first. A run does not leave a
// terminal state, so a document that would take it back is an older one.
func mergeRuns(list, incoming []schema.RunV1) []schema.RunV1 {
	byID := make(map[int64]schema.RunV1, len(list)+len(incoming))
	for _, r := range list {
		byID[r.ID] = r
	}
	for _, r := range incoming {
		if old, ok := byID[r.ID]; ok && schema.RunStateTerminal(old.State) && !schema.RunStateTerminal(r.State) {
			continue
		}
		byID[r.ID] = r
	}
	out := make([]schema.RunV1, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// loadRuns reads one page of the channel's runs, newest first, below before
// (0 for the newest).
func (m Model) loadRuns(before int64) tea.Cmd {
	api, ctx, channel := m.api, m.ctx, m.runs.channel
	return func() tea.Msg {
		page, err := api.ListRuns(ctx, channel, 0, cli.RunsPage{NewestFirst: true, Before: before}, runsPageSize)
		return runsLoaded{channel: channel, before: before, runs: page.Runs, err: err}
	}
}

// clockTick redraws the Runs view and run detail once a second, so the duration
// of an open run moves. It is not an event of the server and reads nothing.
type clockTick struct{}

// tickClock starts the redraw ticks unless they are running. They stop
// themselves on the first tick that finds the user in another view.
func (m Model) tickClock() (Model, tea.Cmd) {
	if m.clockTicking || m.after == nil {
		return m, nil
	}
	m.clockTicking = true
	return m, m.after(time.Second, clockTick{})
}

// clockTicked re-arms the tick while a view that shows a duration is up.
func (m Model) clockTicked() (tea.Model, tea.Cmd) {
	if m.mode != modeRuns && m.mode != modeRun {
		m.clockTicking = false
		return m, nil
	}
	return m, m.after(time.Second, clockTick{})
}

// enterRuns shows the Runs view and reads its first page when it has none.
func (m Model) enterRuns() (Model, tea.Cmd) {
	m, tick := m.tickClock()
	m.mode = modeRuns
	if m.runs.channel != m.current() {
		m.runs = runsView{channel: m.current(), filter: m.runs.filter}
	}
	m.runs.note = ""
	if m.runs.loaded || m.runs.loading || m.runs.channel == "" {
		return m, tick
	}
	m.runs.loading = true
	return m, tea.Batch(tick, m.loadRuns(0))
}

// runsLoaded takes a page. The first page read again (after the events socket
// came back, when frames were missed) is merged in and leaves the paging alone.
func (m Model) runsLoaded(msg runsLoaded) (tea.Model, tea.Cmd) {
	rv := m.runs
	if msg.channel != rv.channel {
		return m, nil
	}
	refresh := msg.before == 0 && rv.loaded
	if !refresh {
		rv.loading = false
	}
	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) && m.ctx.Err() == nil {
			rv.note = "could not list runs: " + runError(msg.err)
		}
		m.runs = rv
		return m, nil
	}
	rv.list = mergeRuns(rv.list, msg.runs)
	if !refresh {
		rv.loaded = true
		rv.pages++
		// The server returns a full page when it has more to give.
		rv.more = len(msg.runs) >= runsPageSize
		for _, r := range msg.runs {
			if rv.before == 0 || r.ID < rv.before {
				rv.before = r.ID
			}
		}
	}
	m.runs = rv
	return m.runsMore()
}

// runsMore reads the next page when the selection is within a few lines of the
// end of what is listed (or nothing is listed and there is more).
func (m Model) runsMore() (tea.Model, tea.Cmd) {
	rv := m.runs
	if !rv.more || rv.loading || rv.pages >= maxRunPages || rv.channel != m.current() {
		return m, nil
	}
	vis := rv.visible()
	if len(vis) > 0 && rv.selected(vis) < len(vis)-3 {
		return m, nil
	}
	m.runs.loading = true
	return m, m.loadRuns(rv.before)
}

// runsFrame keeps the Runs view current from a run frame: a run it lists is
// replaced, a new one joins. A run older than the pages read so far is not
// added, since paging will reach it.
func (m Model) runsFrame(run schema.RunV1) Model {
	rv := m.runs
	if rv.channel == "" || rv.channel != m.current() || (!rv.loaded && !rv.loading) {
		return m
	}
	known := false
	for _, r := range rv.list {
		if r.ID == run.ID {
			known = true
			break
		}
	}
	if !known && rv.loaded && rv.more && run.ID < rv.before {
		return m
	}
	rv.list = mergeRuns(rv.list, []schema.RunV1{run})
	m.runs = rv
	return m
}

// runsKey handles a key in the Runs view. ok is false for a key it does not
// own, which the common handling then sees.
func (m Model) runsKey(key string) (next tea.Model, cmd tea.Cmd, ok bool) {
	m.runs.note = ""
	switch key {
	case "up":
		next, cmd = m.moveRuns(-1)
	case "down":
		next, cmd = m.moveRuns(1)
	case "pgup":
		next, cmd = m.moveRuns(-max(1, m.layout().contentH-1))
	case "pgdown":
		next, cmd = m.moveRuns(max(1, m.layout().contentH-1))
	case "enter":
		next, cmd = m.activateRun()
	case "f":
		m.runs.filter = m.runs.filter.next()
		// The selection stays when its run is still listed, else it is the first.
		if vis := m.runs.visible(); len(vis) > 0 {
			m.runs.sel = vis[m.runs.selected(vis)].ID
		}
		next, cmd = m.runsMore()
	case "a":
		m.mode = modeAgents
		next = m
	default:
		return m, nil, false
	}
	return next, cmd, true
}

// moveRuns moves the selection by delta runs, stopping at the ends.
func (m Model) moveRuns(delta int) (tea.Model, tea.Cmd) {
	vis := m.runs.visible()
	if len(vis) == 0 {
		return m.runsMore()
	}
	i := min(max(m.runs.selected(vis)+delta, 0), len(vis)-1)
	m.runs.sel = vis[i].ID
	return m.runsMore()
}

// activateRun is enter: it opens the selected run in run detail, the same pane
// the Agents view opens.
func (m Model) activateRun() (tea.Model, tea.Cmd) {
	vis := m.runs.visible()
	if len(vis) == 0 {
		return m, nil
	}
	r := vis[m.runs.selected(vis)]
	m.runs.sel = r.ID
	opened, cmd := m.openRun(r.ID, r.SessionID, r)
	opened.rd.fromRuns = true
	return opened, cmd
}

// spanText is d as the views write a stretch of time: 12.4s, 3m02s, 1h02m03s.
// With tenths false a span under a minute is whole seconds. A negative span
// (clocks that disagree) reads as none.
func spanText(d time.Duration, tenths bool) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		if tenths {
			return fmt.Sprintf("%d.%ds", d/time.Second, d%time.Second/(100*time.Millisecond))
		}
		return fmt.Sprintf("%ds", d/time.Second)
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", d/time.Minute, d%time.Minute/time.Second)
	}
	return fmt.Sprintf("%dh%02dm%02ds", d/time.Hour, d%time.Hour/time.Minute, d%time.Minute/time.Second)
}

// runDuration is how long r ran: to its end, or to now while it is open.
func (m Model) runDuration(r schema.RunV1) time.Duration {
	end := m.nowTime()
	if r.EndedAt != nil {
		end = r.EndedAt.Time()
	}
	return end.Sub(r.StartedAt.Time())
}

// padCells is s padded with spaces to width cells.
func padCells(s string, width int) string {
	if n := width - cellWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// runRow is one run of the list in at most cols cells:
//
//	state  agent  task  started  for <duration> | ended <time>  outcome
//
// The state and the agent come first and the task takes what the rest leaves;
// when the line is narrow the outcome goes first, then the end, then the start,
// so who is doing what stays.
func (m Model) runRow(r schema.RunV1, cols int) string {
	name, _ := m.authorName(m.runs.channel, r.PrincipalID)
	head := padCells(fitCells(sanitize(string(r.State)), 9), 9) + " " + padCells(fitCells(name, 10), 10) + " "
	span := "for " + spanText(m.runDuration(r), false)
	if r.EndedAt != nil {
		span = "ended " + m.clock(*r.EndedAt)
	}
	parts := []string{m.clock(r.StartedAt), span}
	if outcome := sanitize(r.Outcome); outcome != "" {
		parts = append(parts, fitCells(outcome, 16))
	}
	const minTask = 12
	taskW := func() int {
		w := cols - cellWidth(head)
		for _, p := range parts {
			w -= 1 + cellWidth(p)
		}
		return w
	}
	for len(parts) > 0 && taskW() < minTask {
		parts = parts[:len(parts)-1]
	}
	task := sanitize(r.Task)
	if len(parts) == 0 {
		return fitCells(head+task, cols)
	}
	line := head + padCells(fitCells(task, taskW()), taskW())
	for _, p := range parts {
		line += " " + p
	}
	return line
}

// runsNote is what the pane says instead of a list: why there is none.
func (m Model) runsNote() string {
	rv := m.runs
	vis := rv.visible()
	switch {
	case len(vis) > 0:
		return ""
	case !rv.loaded && rv.loading:
		return "loading runs…"
	case !rv.loaded && rv.note != "":
		return rv.note
	case rv.loading:
		return "loading runs…"
	case rv.filter != runsAll && len(rv.list) > 0:
		return "no " + rv.filter.String() + " runs here (f: filter)"
	}
	return "No runs in this channel"
}

// runsLines is the Runs list in lines of at most cols cells, and the line of the
// selected run.
func (m Model) runsLines(cols int) (lines []string, selLine int) {
	if note := m.runsNote(); note != "" {
		return []string{statusStyle.Render(fitCells(note, cols))}, 0
	}
	rv := m.runs
	vis := rv.visible()
	sel := rv.selected(vis)
	// The list is from reads and frames; while the socket is down it may miss
	// frames, and the pane says so on its first line, where the status line
	// has no room for it.
	if m.ev.state != eventsLive {
		lines = append(lines, statusStyle.Render(fitCells("live updates paused: reconnecting…", cols)))
	}
	for i, r := range vis {
		prefix := "  "
		if i == sel {
			prefix = activeStyle.Render("› ")
			selLine = len(lines)
		}
		lines = append(lines, prefix+m.runRow(r, cols-2))
	}
	switch {
	case rv.loading:
		lines = append(lines, statusStyle.Render(fitCells("  loading…", cols)))
	case rv.more:
		lines = append(lines, statusStyle.Render(fitCells("  more below", cols)))
	}
	return lines, selLine
}

// runsStatus is the status line of the Runs view.
func (m Model) runsStatus() string {
	rv := m.runs
	if rv.note != "" {
		return rv.note
	}
	status := "runs: " + sanitize(m.current()) + " · " + rv.filter.String()
	if rv.loaded {
		status += " (" + strconv.Itoa(len(rv.visible())) + ")"
	}
	return status
}
