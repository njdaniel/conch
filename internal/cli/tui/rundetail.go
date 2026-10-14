package tui

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// Reads page with the server's largest page; the caps keep a runaway run from
// holding the program: past them the oldest entries are what is missing.
const (
	pageLimit        = 100
	maxActivityPages = 100
	maxRunPages      = 100
	// recentRuns is how many of a session's runs the Agents view lists.
	recentRuns = 10
	// keptActivity is how many activity entries run detail holds.
	keptActivity = 1000
)

// runInput is what the keys of run detail are doing.
type runInput int

const (
	// inputNone: the keys are the pane's own (m, x, esc, scrolling).
	inputNone runInput = iota
	// inputMessage: a one-line message is being typed.
	inputMessage
	// inputCancel: "cancel this run? y/n" is asked.
	inputCancel
)

// runDetail is the open run of run detail. Everything on it comes from reads
// and from events frames, never from what the user has asked: a request that
// was sent is "requested…" until its frame says otherwise, and the run's state
// is only what a RunV1 says. The zero value is "no run open", which no frame
// matches.
type runDetail struct {
	runID     int64
	sessionID int64
	run       schema.RunV1
	haveRun   bool
	// runStamp counts the run frames applied. A read of the run lands only if
	// no frame did since it was asked for: the frame is the newer word.
	runStamp      int
	activity      []schema.AgentActivityV1
	interventions []schema.InterventionV1
	// scroll is how many activity lines the view is scrolled up from the
	// newest.
	scroll int
	input  runInput
	draft  string
	// sending is true while an intervention is being posted, so a second key
	// cannot send the same ask twice.
	sending bool
	status  string
	// refusal is status while it holds the server's reason for refusing a
	// send, which a transport error must not overwrite (runReadFailed).
	refusal string
	// syncGen is the events socket the pane last read from; another socket
	// means frames may have been missed, and everything is read again.
	syncGen int
}

func (r runDetail) open() bool { return r.runID != 0 }

// terminal reports whether the run is known to have ended.
func (r runDetail) terminal() bool { return r.haveRun && schema.RunStateTerminal(r.run.State) }

// Results of the reads and of a send. They are command results, not events:
// handling one never waits for another event.
type (
	runRead struct {
		runID int64
		stamp int
		run   schema.RunV1
		err   error
	}
	activityRead struct {
		runID   int64
		entries []schema.AgentActivityV1
		err     error
	}
	interventionsRead struct {
		runID int64
		list  []schema.InterventionV1
		err   error
	}
	interventionSent struct {
		runID        int64
		intervention schema.InterventionV1
		err          error
	}
	// sessionRunsLoaded is the newest runs of one session.
	sessionRunsLoaded struct {
		channel string
		session int64
		runs    []schema.RunV1
		err     error
	}
)

// Frames of the events socket other than the roster. gen tells which socket
// sent them.
type (
	eventsRunReceived struct {
		gen int
		run schema.RunV1
	}
	eventsActivityReceived struct {
		gen   int
		entry schema.AgentActivityV1
	}
	eventsInterventionReceived struct {
		gen          int
		intervention schema.InterventionV1
	}
)

// sessionRunKey identifies a session's run list: session ids are the server's, but
// the list is of one channel.
type sessionRunKey struct {
	channel string
	session int64
}

// openRun opens run detail on run id, of session sessionID, and reads it. run
// is what the Agents view already holds about it, zero when only the id is
// known.
func (m Model) openRun(id, sessionID int64, run schema.RunV1) (Model, tea.Cmd) {
	rd := runDetail{runID: id, sessionID: sessionID, syncGen: m.ev.gen}
	if run.ID == id {
		rd.run, rd.haveRun = run, true
	}
	m.rd = rd
	m.mode = modeRun
	m.agentsStatus = ""
	return m, m.readRun(0)
}

// readRun reads the run, its activity after the last entry held and its
// interventions. after is where the activity starts: 0 on open, the last id
// held on a re-read.
func (m Model) readRun(after int64) tea.Cmd {
	api, ctx, id := m.api, m.ctx, m.rd.runID
	return tea.Batch(
		m.checkRun(),
		func() tea.Msg {
			var entries []schema.AgentActivityV1
			for range maxActivityPages {
				page, err := api.ListActivity(ctx, id, after, pageLimit)
				if err != nil {
					return activityRead{runID: id, err: err}
				}
				entries = append(entries, page.Activity...)
				if page.NextAfter == 0 || len(page.Activity) == 0 {
					break
				}
				after = page.NextAfter
			}
			return activityRead{runID: id, entries: entries}
		},
		func() tea.Msg {
			var all []schema.InterventionV1
			var after int64
			for range maxRunPages {
				page, err := api.ListInterventions(ctx, id, after, pageLimit)
				if err != nil {
					return interventionsRead{runID: id, err: err}
				}
				all = append(all, page.Interventions...)
				// No cursor on this list: a page as long as the limit may have
				// a successor.
				if len(page.Interventions) < pageLimit {
					break
				}
				after = page.Interventions[len(page.Interventions)-1].ID
			}
			return interventionsRead{runID: id, list: all}
		},
	)
}

// checkRun reads the run alone. It is how the pane learns that the run can no
// longer be seen, and what is read after an event the pane cannot interpret.
func (m Model) checkRun() tea.Cmd {
	api, ctx, id, stamp := m.api, m.ctx, m.rd.runID, m.rd.runStamp
	return func() tea.Msg {
		run, err := api.GetRun(ctx, id)
		return runRead{runID: id, stamp: stamp, run: run, err: err}
	}
}

// closeRun leaves run detail for the Agents view, saying why in one line when
// there is something to say.
func (m Model) closeRun(status string) Model {
	m.rd = runDetail{}
	m.mode = modeAgents
	m.agentsStatus = sanitize(status)
	return m
}

const runGone = "run not found: it is gone, or you can no longer see it"

// isCode reports whether err is the server's refusal with this code.
func isCode(err error, code string) bool {
	var se *cli.ServerError
	return errors.As(err, &se) && se.Code == code
}

// runError is a failed read or send as one clear line.
func runError(err error) string {
	switch {
	case isCode(err, schema.ErrorCodeInterventionUnsupported):
		return "this session does not accept that intervention"
	case isCode(err, schema.ErrorCodeRunNotFound):
		return runGone
	case isCode(err, schema.ErrorCodeTooManyInterventions):
		return "too many requests are waiting on this run; wait for the agent to answer"
	case isCode(err, schema.ErrorCodeInvalidTransition):
		return "the run has ended"
	case isCode(err, schema.ErrorCodeHumanRequired):
		return "only a person can ask a run to do something"
	}
	return sanitize(err.Error())
}

// runReadFailed handles a failed read or send of the open run: a run that is
// gone closes the pane, anything else is a line in it.
func (m Model) runReadFailed(err error) Model {
	if errors.Is(err, context.Canceled) || m.ctx.Err() != nil {
		return m
	}
	if isCode(err, schema.ErrorCodeRunNotFound) {
		return m.closeRun(runGone)
	}
	// A transport failure (the server down, being retried) is already said by
	// the header's "live updates paused"; it does not overwrite the server's
	// reason for refusing a send, which is more use to the reader.
	var serverErr *cli.ServerError
	refusal := errors.As(err, &serverErr)
	if !refusal && m.rd.status != "" && m.rd.status == m.rd.refusal {
		return m
	}
	m.rd.status = runError(err)
	m.rd.refusal = ""
	if refusal {
		m.rd.refusal = m.rd.status
	}
	return m
}

// runRead takes the result of reading the run.
func (m Model) runRead(msg runRead) (tea.Model, tea.Cmd) {
	if msg.runID != m.rd.runID {
		return m, nil
	}
	if msg.err != nil {
		return m.runReadFailed(msg.err), nil
	}
	// A frame landed while this read was in flight and is the newer word.
	if msg.stamp == m.rd.runStamp {
		m = m.applyRun(msg.run)
	}
	return m, nil
}

// applyRun takes a whole run, from a frame or a read. A run does not leave a
// terminal state, so a document that would take it back is an older one.
func (m Model) applyRun(run schema.RunV1) Model {
	rd := m.rd
	if run.ID != rd.runID || (rd.terminal() && !schema.RunStateTerminal(run.State)) {
		return m
	}
	rd.run, rd.haveRun = run, true
	rd.sessionID = run.SessionID
	if schema.RunStateTerminal(run.State) {
		// The ask that was being typed is for a run that no longer takes it.
		rd.input, rd.draft = inputNone, ""
	}
	m.rd = rd
	return m
}

// interventionRank orders the states of an intervention: it only moves
// forward, so an older document of one never takes it back.
func interventionRank(s schema.InterventionState) int {
	switch s {
	case schema.InterventionStateRequested:
		return 1
	case schema.InterventionStateAcknowledged:
		return 2
	default:
		return 3
	}
}

// upsertIntervention puts iv in the history by id. The server sends documents
// whole and may repeat one, so applying the same one again changes nothing.
func upsertIntervention(list []schema.InterventionV1, iv schema.InterventionV1) []schema.InterventionV1 {
	out := make([]schema.InterventionV1, len(list), len(list)+1)
	copy(out, list)
	for i := range out {
		if out[i].ID != iv.ID {
			continue
		}
		if interventionRank(iv.State) >= interventionRank(out[i].State) {
			out[i] = iv
		}
		return out
	}
	out = append(out, iv)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// mergeActivity merges incoming into the entries held, by id, oldest first,
// keeping the newest keptActivity.
func mergeActivity(existing, incoming []schema.AgentActivityV1) []schema.AgentActivityV1 {
	byID := make(map[int64]schema.AgentActivityV1, len(existing)+len(incoming))
	for _, e := range existing {
		byID[e.ID] = e
	}
	for _, e := range incoming {
		byID[e.ID] = e
	}
	merged := make([]schema.AgentActivityV1, 0, len(byID))
	for _, e := range byID {
		merged = append(merged, e)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	if len(merged) > keptActivity {
		merged = merged[len(merged)-keptActivity:]
	}
	return merged
}

func (m Model) addActivity(runID int64, entries []schema.AgentActivityV1) Model {
	if runID != m.rd.runID {
		return m
	}
	var mine []schema.AgentActivityV1
	for _, e := range entries {
		if e.RunID == runID {
			mine = append(mine, e)
		}
	}
	if len(mine) == 0 {
		return m
	}
	rd := m.rd
	rd.activity = mergeActivity(rd.activity, mine)
	m.rd = rd
	return m
}

func (m Model) addIntervention(iv schema.InterventionV1) Model {
	if iv.RunID != m.rd.runID {
		return m
	}
	rd := m.rd
	rd.interventions = upsertIntervention(rd.interventions, iv)
	m.rd = rd
	return m
}

// The handlers of the events frames. Each reads the next event, as the roster's
// does, and drops a frame of a socket that is no longer the current one.

func (m Model) eventsRun(msg eventsRunReceived) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen {
		return m, wait
	}
	if m.rd.open() && msg.run.ID == m.rd.runID {
		m.rd.runStamp++
		m = m.applyRun(msg.run)
	}
	m = m.cacheRun(msg.run)
	return m, wait
}

func (m Model) eventsActivity(msg eventsActivityReceived) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen {
		return m, wait
	}
	m = m.addActivity(msg.entry.RunID, []schema.AgentActivityV1{msg.entry})
	return m, wait
}

func (m Model) eventsIntervention(msg eventsInterventionReceived) (tea.Model, tea.Cmd) {
	wait := m.waitEvent()
	if msg.gen != m.ev.gen {
		return m, wait
	}
	m = m.addIntervention(msg.intervention)
	return m, wait
}

func (m Model) activityRead(msg activityRead) (tea.Model, tea.Cmd) {
	if msg.runID != m.rd.runID {
		return m, nil
	}
	if msg.err != nil {
		return m.runReadFailed(msg.err), nil
	}
	return m.addActivity(msg.runID, msg.entries), nil
}

func (m Model) interventionsRead(msg interventionsRead) (tea.Model, tea.Cmd) {
	if msg.runID != m.rd.runID {
		return m, nil
	}
	if msg.err != nil {
		return m.runReadFailed(msg.err), nil
	}
	for _, iv := range msg.list {
		m = m.addIntervention(iv)
	}
	return m, nil
}

// interventionSent takes the server's answer to a request. The document is the
// intervention as stored, "requested"; a frame that got here first keeps its
// further state.
func (m Model) interventionSent(msg interventionSent) (tea.Model, tea.Cmd) {
	if msg.runID != m.rd.runID {
		return m, nil
	}
	m.rd.sending = false
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) || m.ctx.Err() != nil {
			return m, nil
		}
		m = m.runReadFailed(msg.err)
		if m.rd.open() && isCode(msg.err, schema.ErrorCodeInvalidTransition) {
			// The run ended; what state it is in is for a read to say.
			return m, m.checkRun()
		}
		return m, nil
	}
	m.rd.status = ""
	return m.addIntervention(msg.intervention), nil
}

// resyncRun reads the open run again when the events socket is not the one it
// last read from: frames in between were not replayed.
func (m Model) resyncRun() (Model, tea.Cmd) {
	if !m.rd.open() || m.rd.syncGen == m.ev.gen {
		return m, nil
	}
	m.rd.syncGen = m.ev.gen
	var last int64
	if n := len(m.rd.activity); n > 0 {
		last = m.rd.activity[n-1].ID
	}
	return m, m.readRun(last)
}

// runSession is the roster's entry for the open run's session, if the roster can
// be trusted and has it.
func (m Model) runSession() (schema.SessionV1, bool) {
	for _, s := range m.rosterSessions() {
		if s.ID == m.rd.sessionID {
			return s, true
		}
	}
	return schema.SessionV1{}, false
}

// declares reports whether the run's session lists kind among the
// interventions it takes.
func (m Model) declares(kind schema.InterventionKind) bool {
	s, ok := m.runSession()
	if !ok {
		return false
	}
	for _, k := range s.Interventions {
		if k == kind {
			return true
		}
	}
	return false
}

// canAsk reports whether the key for kind does anything: the run is open and
// its session declared the kind.
func (m Model) canAsk(kind schema.InterventionKind) bool {
	return m.rd.haveRun && !m.rd.terminal() && !m.rd.sending && m.declares(kind)
}

// runKey handles a key in run detail.
func (m Model) runKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rd := &m.rd
	key := msg.String()
	switch rd.input {
	case inputMessage:
		return m.messageKey(msg)
	case inputCancel:
		switch key {
		case "y", "Y":
			rd.input = inputNone
			return m.ask(schema.CreateInterventionRequestV1{Kind: schema.InterventionKindCancel})
		case "n", "N", "esc", "ctrl+c":
			rd.input = inputNone
		}
		return m, nil
	}
	switch key {
	case "esc":
		return m.closeRun(""), nil
	case "ctrl+c":
		return m, tea.Quit
	case "m":
		if m.canAsk(schema.InterventionKindMessage) {
			rd.input, rd.draft, rd.status = inputMessage, "", ""
		}
	case "x":
		if m.canAsk(schema.InterventionKindCancel) {
			rd.input, rd.status = inputCancel, ""
		}
	case "up":
		rd.scroll = min(rd.scroll+1, max(0, len(rd.activity)-1))
	case "down":
		rd.scroll = max(0, rd.scroll-1)
	case "pgup":
		rd.scroll = min(rd.scroll+m.runPage(), max(0, len(rd.activity)-1))
	case "pgdown":
		rd.scroll = max(0, rd.scroll-m.runPage())
	}
	return m, nil
}

func (m Model) runPage() int { return max(1, m.layout().contentH/2) }

// messageKey handles a key while the message is being typed.
func (m Model) messageKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rd := &m.rd
	switch msg.String() {
	case "esc", "ctrl+c":
		rd.input, rd.draft = inputNone, ""
	case "enter":
		body := rd.draft
		switch {
		case strings.TrimSpace(body) == "":
			rd.status = "type a message to send, or esc"
		case hasControl(body):
			rd.status = "a message holds no control characters other than tab"
		case !m.canAsk(schema.InterventionKindMessage):
			// The run ended, or the session stopped taking messages, while
			// it was being typed.
			rd.input, rd.draft = inputNone, ""
		default:
			rd.input, rd.draft = inputNone, ""
			return m.ask(schema.CreateInterventionRequestV1{Kind: schema.InterventionKindMessage, Body: body})
		}
	case "backspace":
		if rd.draft != "" {
			runes := []rune(rd.draft)
			rd.draft = string(runes[:len(runes)-1])
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			rd.draft += string(msg.Runes)
		}
	}
	return m, nil
}

// hasControl reports whether s holds a control character other than tab. The
// server refuses them too; a newline is one, as the input is one line.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\t' })
}

// ask posts an intervention for the open run. Nothing shows as done until its
// frame says so.
func (m Model) ask(req schema.CreateInterventionRequestV1) (tea.Model, tea.Cmd) {
	m.rd.sending = true
	m.rd.status = "sending…"
	api, ctx, id := m.api, m.ctx, m.rd.runID
	return m, func() tea.Msg {
		iv, err := api.CreateIntervention(ctx, id, req)
		return interventionSent{runID: id, intervention: iv, err: err}
	}
}

// The newest runs of a session, for the Agents view.

// loadSessionRuns reads the newest runs of one session in the selected
// channel. The list is ascending by id, so the newest are at the end of the
// last page.
func (m Model) loadSessionRuns(session int64) tea.Cmd {
	api, ctx, channel := m.api, m.ctx, m.current()
	return func() tea.Msg {
		var newest []schema.RunV1
		var after int64
		for range maxRunPages {
			page, err := api.ListRuns(ctx, channel, session, after, pageLimit)
			if err != nil {
				return sessionRunsLoaded{channel: channel, session: session, err: err}
			}
			newest = append(newest, page.Runs...)
			if len(newest) > recentRuns {
				newest = newest[len(newest)-recentRuns:]
			}
			if page.NextAfter == 0 || len(page.Runs) == 0 {
				break
			}
			after = page.NextAfter
		}
		return sessionRunsLoaded{channel: channel, session: session, runs: newest}
	}
}

func (m Model) sessionRunsLoaded(msg sessionRunsLoaded) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) && m.ctx.Err() == nil && msg.channel == m.current() {
			m.agentsStatus = "could not list runs: " + runError(msg.err)
		}
		return m, nil
	}
	m.sessionRuns[sessionRunKey{msg.channel, msg.session}] = msg.runs
	return m, nil
}

// cacheRun keeps the Agents view's run list of the run's session current. A
// session whose list was never read has none to keep.
func (m Model) cacheRun(run schema.RunV1) Model {
	key := sessionRunKey{m.current(), run.SessionID}
	list, ok := m.sessionRuns[key]
	if !ok {
		return m
	}
	out := make([]schema.RunV1, len(list), len(list)+1)
	copy(out, list)
	found := false
	for i := range out {
		if out[i].ID == run.ID {
			found = true
			if !schema.RunStateTerminal(out[i].State) || schema.RunStateTerminal(run.State) {
				out[i] = run
			}
		}
	}
	if !found {
		out = append(out, run)
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		if len(out) > recentRuns {
			out = out[len(out)-recentRuns:]
		}
	}
	m.sessionRuns[key] = out
	return m
}

// Drawing.

// clock is t as a time of day in the model's zone.
func (m Model) clock(t schema.Timestamp) string {
	loc := m.loc
	if loc == nil {
		loc = time.Local
	}
	return t.Time().In(loc).Format("15:04:05")
}

func (m Model) nowTime() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// runHead is the fields of the run, one per line.
func (m Model) runHead(cols int) []string {
	rd := m.rd
	id := "run #" + strconv.FormatInt(rd.runID, 10)
	if !rd.haveRun {
		return []string{fitCells(id+" · loading…", cols)}
	}
	r := rd.run
	state := id + " · " + sanitize(string(r.State))
	if s, ok := m.runSession(); ok && s.OutcomeUnknown && !rd.terminal() {
		state += " · outcome unknown"
	}
	lines := []string{state, "task: " + sanitize(r.Task)}
	if r.Link != "" {
		lines = append(lines, "link: "+sanitize(r.Link))
	}
	times := "started: " + m.clock(r.StartedAt) + " (" + ago(m.nowTime(), r.StartedAt.Time()) + ")"
	if r.EndedAt != nil {
		times += " · ended: " + m.clock(*r.EndedAt)
	}
	lines = append(lines, times)
	if r.Outcome != "" {
		lines = append(lines, "outcome: "+sanitize(r.Outcome))
	}
	if m.ev.state != eventsLive {
		lines = append(lines, "live updates paused: reconnecting…")
	}
	for i := range lines {
		lines[i] = fitCells(lines[i], cols)
	}
	return lines
}

func activityLine(m Model, e schema.AgentActivityV1, cols int) string {
	line := m.clock(e.At) + " " + sanitize(string(e.Kind)) + " " + sanitize(e.Summary)
	if e.Outcome != "" {
		line += " → " + sanitize(e.Outcome)
	}
	return fitCells(line, cols)
}

// interventionState is how the history words the state of an intervention:
// what the last document said, and "requested…" until one says more.
func interventionState(iv schema.InterventionV1) string {
	switch iv.State {
	case schema.InterventionStateRequested:
		return "requested…"
	case schema.InterventionStateFailed:
		return "failed: " + sanitize(iv.Reason)
	}
	return sanitize(string(iv.State))
}

func interventionLine(iv schema.InterventionV1, cols int) string {
	// The state comes before the body so a long body is what is cut.
	line := "#" + strconv.FormatInt(iv.ID, 10) + " " + sanitize(string(iv.Kind)) + " · " + interventionState(iv)
	if iv.Body != "" {
		line += " · " + sanitize(iv.Body)
	}
	return fitCells(line, cols)
}

// keyHints is the line of what the keys do: those the session declared, and the
// rest as unsupported. A run that ended has no keys.
func (m Model) keyHints(cols int) string {
	rd := m.rd
	switch {
	case rd.input == inputMessage:
		return fitCells("enter send · esc cancel", cols)
	case rd.input == inputCancel:
		return fitCells("y cancel the run · n keep it", cols)
	case rd.terminal():
		return fitCells("the run has ended", cols)
	case !rd.haveRun:
		return ""
	}
	if _, ok := m.runSession(); !ok {
		return fitCells("interventions unavailable: the session is not in the roster", cols)
	}
	hint := func(key string, kind schema.InterventionKind) string {
		if m.declares(kind) {
			return key + " " + string(kind)
		}
		return string(kind) + " (unsupported)"
	}
	return fitCells(strings.Join([]string{
		hint("m", schema.InterventionKindMessage), hint("x", schema.InterventionKindCancel),
		"pause (unsupported)", "resume (unsupported)",
	}, " · "), cols)
}

// runLines is run detail in lines of at most cols cells, exactly rows of them:
// the fields, the newest activity and interventions that fit, and the keys.
func (m Model) runLines(cols, rows int) []string {
	rd := m.rd
	lines := m.runHead(cols)
	// The key line is the last row; two more are the section titles.
	if body := rows - 1 - len(lines) - 2; rd.haveRun && body >= 1 {
		nIv := min(len(rd.interventions), max(body/3, 1))
		if body < 2 {
			nIv = 0
		}
		lines = append(lines, statusStyle.Render(fitCells("activity ("+strconv.Itoa(len(rd.activity))+")", cols)))
		lines = append(lines, m.activityWindow(cols, body-nIv)...)
		lines = append(lines, statusStyle.Render(fitCells("interventions ("+strconv.Itoa(len(rd.interventions))+")", cols)))
		lines = append(lines, interventionWindow(rd.interventions, cols, nIv)...)
	}
	if len(lines) > rows-1 {
		lines = lines[:max(rows-1, 0)]
	}
	for len(lines) < rows-1 {
		lines = append(lines, "")
	}
	return append(lines, statusStyle.Render(m.keyHints(cols)))
}

// activityWindow is n lines of activity ending scroll lines before the newest.
func (m Model) activityWindow(cols, n int) []string {
	if n <= 0 {
		return nil
	}
	entries := m.rd.activity
	end := len(entries) - min(m.rd.scroll, max(0, len(entries)-1))
	start := max(0, end-n)
	lines := make([]string, 0, n)
	for _, e := range entries[start:end] {
		lines = append(lines, activityLine(m, e, cols))
	}
	return lines
}

func interventionWindow(list []schema.InterventionV1, cols, n int) []string {
	if n <= 0 {
		return nil
	}
	list = list[max(0, len(list)-n):]
	lines := make([]string, 0, n)
	for _, iv := range list {
		lines = append(lines, interventionLine(iv, cols))
	}
	return lines
}

// runPrompt is the input row of run detail: the message being typed, the
// question being asked, or nothing.
func (m Model) runPrompt(width int) string {
	switch m.rd.input {
	case inputMessage:
		return "message> " + clipTail(sanitize(m.rd.draft), width-12)
	case inputCancel:
		return "cancel this run? y/n"
	}
	return ""
}

// runStatus is the status line of run detail.
func (m Model) runStatus() string {
	if m.rd.status != "" {
		return "run: " + m.rd.status
	}
	return "run #" + strconv.FormatInt(m.rd.runID, 10)
}
