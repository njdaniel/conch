package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// P3-06, 07, 08 and 13: an agent through its states, a frozen agent, the
// instructions a person gives, and one real adapter.

// ------------------------------------------------------------ the views

// openAgentsView goes from the channels view to the Agents view, with the
// cursor on the first agent, and waits for it.
func (s *suite) openAgentsView(t *tui) error {
	for range 2 { // channels, inbox, agents
		if err := t.press(keyTab); err != nil {
			return err
		}
	}
	if err := t.waitFor("the Agents view", 15*time.Second, func(r []string) bool { return showing(r, "tab channels") && showing(r, "[agent]") }); err != nil {
		return err
	}
	for range 6 {
		if err := t.press(keyUp); err != nil {
			return err
		}
	}
	return nil
}

// closeAgentsView goes back to the channels view.
func (s *suite) closeAgentsView(t *tui) error {
	if err := t.press(keyTab); err != nil {
		return err
	}
	return t.waitFor("the channels view", 15*time.Second, func(r []string) bool { return showing(r, "enter send") })
}

// refold makes the TUI read the runs of the first agent's sessions, and waits
// until it lists session. The runs of a session are read when the agent is
// unfolded, and only for the sessions the TUI has at that moment; a session
// that joined after has no list, and the runs it starts do not show. So the
// agent is unfolded until the session is listed, and then folded and unfolded
// once more, when the TUI certainly has it.
func (s *suite) refold(t *tui, session int64) error {
	listed := func(r []string) bool { return showing(r, "▾ [agent]") && showing(r, " #"+id(session)+" ") }
	toggle := func(unfolded bool) error {
		if err := t.press(keyEnter); err != nil {
			return err
		}
		return t.waitFor(map[bool]string{true: "the agent unfolded", false: "the agent folded"}[unfolded], 10*time.Second, func(r []string) bool {
			return showing(r, "▾ [agent]") == unfolded
		})
	}
	if !showing(t.rows(), "▾ [agent]") {
		if err := toggle(true); err != nil {
			return err
		}
	}
	if err := t.waitFor("session #"+id(session)+" under the agent", 15*time.Second, listed); err != nil {
		return err
	}
	if err := toggle(false); err != nil {
		return err
	}
	if err := toggle(true); err != nil {
		return err
	}
	return t.waitFor("session #"+id(session)+" under the agent again", 15*time.Second, listed)
}

// agree waits until the API and the Agents view say the same of a session and
// a run: the API's roster and run, and the TUI's lines for them. run 0 means
// there is no run to compare.
func (s *suite) agree(t *tui, session int64, status schema.SessionStatus, run int64, state schema.RunState) error {
	var apiSaw, tuiSaw string
	err := s.until(fmt.Sprintf("the API and the TUI to agree: session #%d %s, run #%d %s", session, status, run, state), 25*time.Second, func() (bool, error) {
		roster, err := s.roster(partNick, chGeneral)
		if err != nil {
			return false, err
		}
		sess, ok := sessionOf(roster, session)
		apiSaw = "session not in the roster"
		if ok {
			apiSaw = fmt.Sprintf("session #%d %s", session, sess.Status)
		}
		apiOK := ok && sess.Status == status
		if run != 0 {
			r, err := s.getRun(run)
			if err != nil {
				return false, err
			}
			apiSaw += fmt.Sprintf(", run #%d %s", run, r.State)
			apiOK = apiOK && r.State == state
		}
		rows := t.rows()
		tuiOK := showing(rows, " #"+id(session)+" "+string(status))
		if run != 0 {
			tuiOK = tuiOK && showing(rows, "run #"+id(run)+" "+string(state))
		}
		tuiSaw = "as expected"
		if !tuiOK {
			tuiSaw = "not showing it"
		}
		return apiOK && tuiOK, nil
	})
	if err != nil {
		return fmt.Errorf("%w; the API has %s; the TUI is %s:\n%s", err, apiSaw, tuiSaw, dump(t.rows()))
	}
	return nil
}

// ---------------------------------------------------------------- P3-06

// p306Runs runs worker-test through approval.sh and fail.sh and checks, at each
// step, that the API and the Agents view show the same session and run ids and
// states.
func (s *suite) p306Runs() error {
	t := s.tuiA
	// P3-07 will need to know when the server sweeps (the server restarted in
	// P3-09, so it is learned here); the probe takes a few seconds, and this
	// row is where they are free.
	if err := s.startSweepProbe(); err != nil {
		return err
	}
	if err := s.stopAgent(s.worker); err != nil {
		return err
	}
	var err error
	if s.worker, err = s.launchAgent(agentSpec{part: partWorker, script: "approval.sh"}); err != nil {
		return err
	}
	if err := t.selectChannel(chGeneral); err != nil {
		return err
	}
	if err := s.openAgentsView(t); err != nil {
		return err
	}
	if err := s.refold(t, s.worker.session); err != nil {
		return err
	}
	session := s.worker.session

	// Idle, then a message: running (too brief to hold still), waiting for the
	// approval, which stays until a person decides.
	if err := s.agree(t, session, schema.SessionStatusIdle, 0, ""); err != nil {
		return err
	}
	if err := s.say(chGeneral, "p306 approve this"); err != nil {
		return err
	}
	msg, err := s.waitMessage(partNick, chGeneral, "p306 approve this")
	if err != nil {
		return err
	}
	run, err := s.runAnswering(chGeneral, msg.ID)
	if err != nil {
		return err
	}
	if err := s.agree(t, session, schema.SessionStatusWaiting, run.ID, schema.RunStateWaiting); err != nil {
		return err
	}

	// Only the run's own agent moves it along.
	for _, p := range []string{partNick, partObserver} {
		err := s.as(p).refused(p+" setting the state of worker-test's run", http.MethodPost, "/v1/runs/"+id(run.ID)+"/state",
			schema.SetRunStateRequestV1{State: schema.RunStateCompleted}, http.StatusNotFound, schema.ErrorCodeRunNotFound)
		if err != nil {
			return err
		}
	}
	got, err := s.getRun(run.ID)
	if err != nil {
		return err
	}
	if got.State != schema.RunStateWaiting {
		return fmt.Errorf("after those refusals run %d is %q, want waiting", run.ID, got.State)
	}

	// The approval decides it.
	var open schema.ListApprovalsResponseV1
	if err := s.as(partNick).call(http.MethodGet, "/v1/approvals", nil, &open); err != nil {
		return err
	}
	var approval int64
	for _, a := range open.Approvals {
		if a.Title == "Approve the fixture action" {
			approval = a.ID
		}
	}
	if approval == 0 {
		return fmt.Errorf("the run is waiting, and no approval is open (%d are)", len(open.Approvals))
	}
	if _, err := s.cli("approve", "--reason", "p306 fine", id(approval)); err != nil {
		return err
	}
	if err := s.agree(t, session, schema.SessionStatusIdle, run.ID, schema.RunStateCompleted); err != nil {
		return err
	}
	done, err := s.getRun(run.ID)
	if err != nil {
		return err
	}
	if done.Outcome != "replied" {
		return fmt.Errorf("run %d ended with the outcome %q, want replied", run.ID, done.Outcome)
	}
	// The run went through running, waiting and running again: the adapter
	// reports each with the activity beside it.
	var acts schema.ListActivityResponseV1
	if err := s.as(partNick).call(http.MethodGet, "/v1/runs/"+id(run.ID)+"/activity", nil, &acts); err != nil {
		return err
	}
	var kinds []schema.ActivityKind
	for _, a := range acts.Activity {
		kinds = append(kinds, a.Kind)
	}
	wantKinds := []schema.ActivityKind{schema.ActivityKindCommandStarted, schema.ActivityKindApprovalRequested, schema.ActivityKindApprovalResolved, schema.ActivityKindCommandFinished, schema.ActivityKindReplyPosted}
	if !slices.Equal(kinds, wantKinds) {
		return fmt.Errorf("run %d has the activity %v, want %v", run.ID, kinds, wantKinds)
	}

	// A command that fails: the run is failed with the exit status, the
	// session shows failed, and nothing is posted.
	if err := s.stopAgent(s.worker); err != nil {
		return err
	}
	if s.worker, err = s.launchAgent(agentSpec{part: partWorker, script: "fail.sh"}); err != nil {
		return err
	}
	session = s.worker.session
	if err := s.refold(t, session); err != nil {
		return err
	}
	if err := s.say(chGeneral, "p306 fail this"); err != nil {
		return err
	}
	failing, err := s.waitMessage(partNick, chGeneral, "p306 fail this")
	if err != nil {
		return err
	}
	bad, err := s.runAnswering(chGeneral, failing.ID)
	if err != nil {
		return err
	}
	if err := s.agree(t, session, schema.SessionStatusFailed, bad.ID, schema.RunStateFailed); err != nil {
		return err
	}
	if bad, err = s.getRun(bad.ID); err != nil {
		return err
	}
	if bad.Outcome != "exit 3" {
		return fmt.Errorf("the failed run has the outcome %q, want exit 3", bad.Outcome)
	}
	after, err := s.history(partNick, chGeneral, failing.ID)
	if err != nil {
		return err
	}
	for _, m := range after {
		if m.AuthorID == s.pid("worker-test") {
			return fmt.Errorf("worker-test posted after its command failed")
		}
	}
	return s.closeAgentsView(t)
}

// ---------------------------------------------------------------- P3-07

// p307Freeze freezes worker-test in the middle of a run. Its session must go
// stale and then offline with the outcome unknown, the run must be left as it
// was reported, and the same session must come back when the agent goes on.
func (s *suite) p307Freeze() error {
	t := s.tuiA
	if err := s.stopAgent(s.worker); err != nil {
		return err
	}
	var err error
	// Long enough to span this row and the next.
	if s.worker, err = s.launchAgent(agentSpec{part: partWorker, script: "hang.sh", env: []string{"FIXTURE_SLEEP=90"}}); err != nil {
		return err
	}
	session := s.worker.session
	if err := t.waitFor("worker-test online in the members line", 20*time.Second, func(r []string) bool { return showing(r, "worker-test ●") }); err != nil {
		return err
	}
	if err := s.say(chGeneral, "p307 hold on"); err != nil {
		return err
	}
	msg, err := s.waitMessage(partNick, chGeneral, "p307 hold on")
	if err != nil {
		return err
	}
	run, err := s.runAnswering(chGeneral, msg.ID)
	if err != nil {
		return err
	}
	if _, err := s.waitRunState(run.ID, schema.RunStateRunning, 20*time.Second); err != nil {
		return err
	}

	at, err := s.nextFreeze(time.Now())
	if err != nil {
		return err
	}
	if err := s.until("the moment to freeze the agent", 15*time.Second, func() (bool, error) { return !time.Now().Before(at), nil }); err != nil {
		return err
	}
	if err := s.worker.signal("STOP"); err != nil {
		return err
	}
	adapter := s.worker
	s.cleanup(func() { _ = adapter.signal("CONT") })

	// Stale first (the API and the TUI's members line both say so), then
	// offline with the outcome unknown. The run is not touched.
	var staleAPI, staleTUI bool
	err = s.until("worker-test's session to go stale and then offline", 40*time.Second, func() (bool, error) {
		roster, err := s.roster(partNick, chGeneral)
		if err != nil {
			return false, err
		}
		sess, ok := sessionOf(roster, session)
		if !ok {
			return false, fmt.Errorf("session #%d left the roster while the agent was frozen", session)
		}
		switch sess.State {
		case schema.SessionStateStale:
			if sess.Status != schema.SessionStatusStale {
				return false, fmt.Errorf("the stale session shows the status %q", sess.Status)
			}
			staleAPI = true
			staleTUI = staleTUI || showing(t.rows(), "worker-test ◐")
		case schema.SessionStateOffline:
			if !staleAPI {
				return false, fmt.Errorf("the session went offline without being seen stale")
			}
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	if !staleTUI {
		return fmt.Errorf("the members line never showed worker-test stale (◐):\n%s", dump(t.rows()))
	}
	if err := t.waitFor("worker-test offline in the members line", 10*time.Second, func(r []string) bool { return showing(r, "worker-test ○") }); err != nil {
		return err
	}
	roster, err := s.roster(partNick, chGeneral)
	if err != nil {
		return err
	}
	sess, _ := sessionOf(roster, session)
	if sess.Status != schema.SessionStatusOffline || !sess.OutcomeUnknown || sess.CurrentRun != run.ID {
		return fmt.Errorf("the offline session shows status %q, outcome unknown %v, run %d; want offline, true, %d", sess.Status, sess.OutcomeUnknown, sess.CurrentRun, run.ID)
	}
	frozen, err := s.getRun(run.ID)
	if err != nil {
		return err
	}
	if frozen.State != schema.RunStateRunning || frozen.EndedAt != nil || frozen.Outcome != "" {
		return fmt.Errorf("run %d is %q with outcome %q (ended %v) while its agent is offline; it must stay as reported (running)", run.ID, frozen.State, frozen.Outcome, frozen.EndedAt != nil)
	}

	// Going on: the same session, online again, and no second identity.
	if err := s.worker.signal("CONT"); err != nil {
		return err
	}
	err = s.until("the same session to come back online", 30*time.Second, func() (bool, error) {
		roster, err := s.roster(partNick, chGeneral)
		if err != nil {
			return false, err
		}
		mine := sessionsNamed(roster, "worker-test")
		if len(mine) != 1 {
			return false, fmt.Errorf("worker-test has %d sessions on the roster, want one", len(mine))
		}
		if mine[0].ID != session {
			return false, fmt.Errorf("worker-test came back as session #%d, want #%d", mine[0].ID, session)
		}
		return mine[0].State == schema.SessionStateOnline, nil
	})
	if err != nil {
		return err
	}
	roster, _ = s.roster(partNick, chGeneral)
	if back, _ := sessionOf(roster, session); back.OutcomeUnknown || back.Status != schema.SessionStatusRunning {
		return fmt.Errorf("the revived session shows status %q, outcome unknown %v; want running, false", back.Status, back.OutcomeUnknown)
	}
	return t.waitFor("worker-test online again in the members line", 15*time.Second, func(r []string) bool { return showing(r, "worker-test ●") })
}

// ---------------------------------------------------------------- P3-08

// p308Interventions asks the agent of the open run to take an instruction and
// then to cancel it, from the TUI, and checks what is acknowledged, what is
// refused, and who may ask.
func (s *suite) p308Interventions() error {
	t := s.tuiA
	runs, err := s.runsOf(partNick, chGeneral)
	if err != nil {
		return err
	}
	var run schema.RunV1
	for _, r := range runs {
		if r.State == schema.RunStateRunning && r.SessionID == s.worker.session {
			run = r
		}
	}
	if run.ID == 0 {
		return fmt.Errorf("no run of session #%d is open to intervene in", s.worker.session)
	}
	runPath := "/v1/runs/" + id(run.ID)

	// Who may ask. An agent never does. (Every kind in the vocabulary is
	// declared by this adapter on Linux, so there is no undeclared kind to
	// be refused for; the server's refusal is the server tests' row.)
	if err := s.as(partObserver).refused("an agent asking", http.MethodPost, runPath+"/interventions",
		schema.CreateInterventionRequestV1{Kind: schema.InterventionKindMessage, Body: "p308 from an agent"}, http.StatusForbidden, schema.ErrorCodeHumanRequired); err != nil {
		return err
	}

	// Into the run's detail.
	if err := s.openAgentsView(t); err != nil {
		return err
	}
	if err := s.refold(t, s.worker.session); err != nil {
		return err
	}
	for range 2 { // the session, then its run
		if err := t.press(keyDown); err != nil {
			return err
		}
	}
	if err := t.waitFor("the cursor on run #"+id(run.ID), 10*time.Second, func(r []string) bool { return showing(r, "› ", "run #"+id(run.ID)) }); err != nil {
		return err
	}
	if err := t.press(keyEnter); err != nil {
		return err
	}
	if err := t.waitFor("the run's detail with its keys", 15*time.Second, func(r []string) bool {
		return showing(r, "m message", "x cancel", "pause (unsupported)", "resume (unsupported)")
	}); err != nil {
		return err
	}

	// The command is alive: hang.sh keeps a process of its own.
	if kids := childrenOf(s.worker.c.pid()); len(kids) == 0 {
		return errors.New("the adapter has no command running during the run, so a cancel has nothing to kill")
	}

	// An instruction.
	if err := t.press("m"); err != nil {
		return err
	}
	if err := t.waitFor("the message prompt", 10*time.Second, func(r []string) bool { return showing(r, "message>") }); err != nil {
		return err
	}
	if err := t.sendLine("p308 take note"); err != nil {
		return err
	}
	if err := t.waitFor("the instruction applied", 30*time.Second, func(r []string) bool { return showing(r, "↳ #", "applied") }); err != nil {
		return err
	}
	ivs, err := s.interventionsOf(run.ID)
	if err != nil {
		return err
	}
	if len(ivs) != 1 || ivs[0].Kind != schema.InterventionKindMessage || ivs[0].State != schema.InterventionStateApplied ||
		ivs[0].Body != "p308 take note" || ivs[0].RequestedBy != s.pid("nick-test") || ivs[0].AnsweredAt == nil {
		return fmt.Errorf("the run's interventions are %+v, want one message applied for nick-test", ivs)
	}

	// Pause and resume. The people's keys for them are F2b (#346); the API is
	// the same. The command stops, the run is waiting with the outcome paused
	// only once the adapter says so, and resume lets the command go on.
	if err := s.pauseAndResume(run.ID, runPath); err != nil {
		return err
	}

	// Cancel: asked with x and answered y. The run is cancelled when the
	// adapter has stopped the command and says so, and the intervention is
	// answered after that.
	if err := t.press("x"); err != nil {
		return err
	}
	if err := t.waitFor("the question", 10*time.Second, func(r []string) bool { return showing(r, "cancel this run? y/n") }); err != nil {
		return err
	}
	if err := t.press("y"); err != nil {
		return err
	}
	if err := t.waitFor("the cancel applied and the run cancelled", 30*time.Second, func(r []string) bool {
		// The timeline has an answer line for each intervention: the message's,
		// the pause's, the resume's and now the cancel's.
		return countRows(r, "↳ #", "applied") >= 4 && showing(r, "cancelled")
	}); err != nil {
		return err
	}
	ended, err := s.waitRunState(run.ID, schema.RunStateCancelled, 15*time.Second)
	if err != nil {
		return err
	}
	if ivs, err = s.interventionsOf(run.ID); err != nil {
		return err
	}
	if len(ivs) != 6 {
		return fmt.Errorf("the run has %d interventions, want 6 (message, a resume that fails, pause, a pause that fails, resume, cancel)", len(ivs))
	}
	cancel := ivs[5]
	if cancel.Kind != schema.InterventionKindCancel || cancel.State != schema.InterventionStateApplied || cancel.AnsweredAt == nil || ended.EndedAt == nil {
		return fmt.Errorf("the cancel is %q %q, answered %v; the run ended %v", cancel.Kind, cancel.State, cancel.AnsweredAt != nil, ended.EndedAt != nil)
	}
	if cancel.AnsweredAt.Time().Before(ended.EndedAt.Time()) {
		return fmt.Errorf("the cancel was answered at %s, before the run ended at %s", cancel.AnsweredAt.Time(), ended.EndedAt.Time())
	}
	// The command is gone, with whatever it started.
	if kids := childrenOf(s.worker.c.pid()); len(kids) != 0 {
		return fmt.Errorf("the adapter still has child processes after the cancel: %v", kids)
	}
	if left := s.r.processesOfRun(s.r.path("fixtures")); len(left) != 0 {
		return fmt.Errorf("a fixture script is still running after the cancel: %v", left)
	}
	if left := s.r.processesOfRun("sleep 90"); len(left) != 0 {
		return fmt.Errorf("the command's sleep is still running after the cancel: %v", left)
	}
	// The key line of a run that ended has no keys, and the session is idle.
	if err := t.waitFor("the run has ended", 10*time.Second, func(r []string) bool { return showing(r, "the run has ended") }); err != nil {
		return err
	}
	if err := t.press(keyEsc); err != nil {
		return err
	}
	if err := s.agree(t, s.worker.session, schema.SessionStatusIdle, run.ID, schema.RunStateCancelled); err != nil {
		return err
	}
	if err := s.closeAgentsView(t); err != nil {
		return err
	}

	return s.privateRunIsOffLimits()
}

// pauseAndResume pauses the open run through the API, checks that the
// adapter's command and what it started are stopped and the run waiting with the outcome paused,
// refuses nothing it should accept and accepts nothing it should refuse, and
// resumes it.
func (s *suite) pauseAndResume(runID int64, runPath string) error {
	ask := func(kind schema.InterventionKind) (schema.InterventionV1, error) {
		var iv schema.InterventionV1
		err := s.as(partNick).call(http.MethodPost, runPath+"/interventions", schema.CreateInterventionRequestV1{Kind: kind}, &iv)
		return iv, err
	}
	answered := func(iv schema.InterventionV1, state schema.InterventionState, reason string) error {
		return s.until(fmt.Sprintf("intervention %d to be %s", iv.ID, state), 30*time.Second, func() (bool, error) {
			ivs, err := s.interventionsOf(runID)
			if err != nil {
				return false, err
			}
			for _, got := range ivs {
				if got.ID == iv.ID {
					if got.State == state && (reason == "" || got.Reason == reason) {
						return true, nil
					}
					if (got.State == schema.InterventionStateApplied || got.State == schema.InterventionStateFailed) && got.State != state {
						return false, fmt.Errorf("intervention %d is %q (%q), want %q", iv.ID, got.State, got.Reason, state)
					}
				}
			}
			return false, nil
		})
	}

	// A resume of a run that is not paused fails and changes nothing.
	early, err := ask(schema.InterventionKindResume)
	if err != nil {
		return err
	}
	if err := answered(early, schema.InterventionStateFailed, "not paused"); err != nil {
		return err
	}
	r, err := s.getRun(runID)
	if err != nil {
		return err
	}
	if r.State != schema.RunStateRunning {
		return fmt.Errorf("after a resume of a run not paused the run is %q, want running", r.State)
	}

	paused, err := ask(schema.InterventionKindPause)
	if err != nil {
		return err
	}
	if err := answered(paused, schema.InterventionStateApplied, ""); err != nil {
		return err
	}
	r, err = s.waitRunState(runID, schema.RunStateWaiting, 15*time.Second)
	if err != nil {
		return err
	}
	if r.Outcome != "paused" {
		return fmt.Errorf("the paused run has outcome %q, want paused", r.Outcome)
	}
	if states := childStatesOf(s.worker.c.pid()); len(states) < 2 || strings.Join(states, "") != strings.Repeat("T", len(states)) {
		return fmt.Errorf("the command's process states after the pause are %v, want all T", states)
	}
	// A second pause fails and changes nothing.
	again, err := ask(schema.InterventionKindPause)
	if err != nil {
		return err
	}
	if err := answered(again, schema.InterventionStateFailed, "already paused"); err != nil {
		return err
	}

	resumed, err := ask(schema.InterventionKindResume)
	if err != nil {
		return err
	}
	if err := answered(resumed, schema.InterventionStateApplied, ""); err != nil {
		return err
	}
	if _, err := s.waitRunState(runID, schema.RunStateRunning, 15*time.Second); err != nil {
		return err
	}
	if states := childStatesOf(s.worker.c.pid()); len(states) < 2 || strings.ContainsRune(strings.Join(states, ""), 'T') {
		return fmt.Errorf("the command's process states after the resume are %v, want none T", states)
	}
	return nil
}

// childStatesOf is the /proc state letter of every live descendant of pid
// (its children, theirs, and so on): a pause must reach what the command
// started, not only the command.
func childStatesOf(pid int) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	type proc struct {
		ppid  int
		state string
	}
	procs := map[int]proc{}
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat") // #nosec G304 -- a /proc path built from a number
		if err != nil {
			continue
		}
		i := strings.LastIndexByte(string(stat), ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(stat)[i+1:])
		if len(fields) < 2 || fields[0] == "Z" {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		procs[n] = proc{ppid: ppid, state: fields[0]}
	}
	var out []string
	frontier := []int{pid}
	for len(frontier) > 0 {
		var next []int
		for n, p := range procs {
			if slices.Contains(frontier, p.ppid) {
				out = append(out, p.state)
				next = append(next, n)
			}
		}
		frontier = next
	}
	return out
}

// processesOfRun lists the live processes of this run (the ones whose
// environment carries its run id) whose command line mentions needle.
func (r *runner) processesOfRun(needle string) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	mark := "CONCH_P3_RUN_ID=" + r.runID
	var out []string
	for _, e := range entries {
		pid := atoi(e.Name())
		cmdline := strings.Join(argv(pid), " ")
		if pid == 0 || !strings.Contains(cmdline, needle) || !processRunning(pid) {
			continue
		}
		env, err := os.ReadFile("/proc/" + e.Name() + "/environ") // #nosec G304 -- a /proc path built from a number
		if err == nil && strings.Contains("\x00"+string(env), "\x00"+mark+"\x00") {
			out = append(out, cmdline)
		}
	}
	return out
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// interventionsOf lists the interventions of a run as nick-test sees them.
func (s *suite) interventionsOf(run int64) ([]schema.InterventionV1, error) {
	c, err := s.r.client(s.token(partNick))
	if err != nil {
		return nil, err
	}
	list, err := c.ListInterventions(runCtx, run, 0, 100)
	if err != nil {
		return nil, err
	}
	return list.Interventions, nil
}

// privateRunIsOffLimits has worker-test answer a message in test-private, and
// checks that guest-test, who is not in the channel, cannot
// intervene in the run or even see it.
func (s *suite) privateRunIsOffLimits() error {
	a, err := s.launchAgent(agentSpec{part: partWorker, script: "slow.sh", channel: chPrivate, env: []string{"FIXTURE_SLEEP=60"}})
	if err != nil {
		return err
	}
	defer func() { _ = s.stopAgent(a) }()
	if err := s.say(chPrivate, "p308 private work"); err != nil {
		return err
	}
	msg, err := s.waitMessage(partNick, chPrivate, "p308 private work")
	if err != nil {
		return err
	}
	run, err := s.runAnswering(chPrivate, msg.ID)
	if err != nil {
		return err
	}
	runPath := "/v1/runs/" + id(run.ID)
	ask := schema.CreateInterventionRequestV1{Kind: schema.InterventionKindMessage, Body: "p308 let me in"}
	for _, p := range []string{partGuest} {
		as := s.as(p)
		if err := as.refused(p+" intervening in a run of test-private", http.MethodPost, runPath+"/interventions", ask, http.StatusNotFound, schema.ErrorCodeRunNotFound); err != nil {
			return err
		}
		if err := as.refused(p+" reading a run of test-private", http.MethodGet, runPath, nil, http.StatusNotFound, schema.ErrorCodeRunNotFound); err != nil {
			return err
		}
	}
	ivs, err := s.interventionsOf(run.ID)
	if err != nil {
		return err
	}
	if len(ivs) != 0 {
		return fmt.Errorf("after those refusals the run has %d interventions, want none", len(ivs))
	}
	// Its own member may.
	iv := schema.CreateInterventionRequestV1{Kind: schema.InterventionKindCancel}
	if err := s.as(partNick).call(http.MethodPost, runPath+"/interventions", iv, nil); err != nil {
		return fmt.Errorf("nick-test, a member of test-private, cancelling its run: %w", err)
	}
	_, err = s.waitRunState(run.ID, schema.RunStateCancelled, 20*time.Second)
	return err
}

// ---------------------------------------------------------------- P3-13

// realAdapterPlan says whether the real adapter row runs, and if not why. The
// real command runs as the person who started the suite, with its permission
// checks off, and spends their quota: it runs only when they asked for it with
// --real-adapter and the claude program is there to run.
func realAdapterPlan(flag bool, lookPath func(string) (string, error)) (run bool, why string) {
	return realAdapterPlanFor(flag, "claude", "sonnet", lookPath)
}

// realAdapterPlanFor is realAdapterPlan for the program --runtime chose. Codex
// has no default model, so it also needs the one the adapter will be given.
func realAdapterPlanFor(flag bool, program, model string, lookPath func(string) (string, error)) (run bool, why string) {
	if !flag {
		return false, "it runs the real " + realProgramLabel(program) + " command as you; pass --real-adapter to run it"
	}
	if _, err := lookPath(program); err != nil {
		return false, "the " + program + " program is not on PATH"
	}
	if model == "" {
		return false, "CONCH_ADAPTER_MODEL names no model for the " + program + " runtime"
	}
	return true, ""
}

func realProgramLabel(program string) string {
	if program == "codex" {
		return "Codex"
	}
	return "Claude Code"
}

// p313Real runs one reply of the real adapter command, when asked to, and
// writes what ran and how it ended. The row has no assertion about the model's
// words: that a reply came, and from what, is the result.
func (s *suite) p313Real() error {
	program, model := "claude", "sonnet"
	if s.r.runtime == "codex" {
		program, model = "codex", os.Getenv("CONCH_ADAPTER_MODEL")
	}
	run, why := realAdapterPlanFor(s.r.realAdapter, program, model, exec.LookPath)
	if !run {
		s.skip("P3-13", why)
		return nil
	}
	r := s.r
	if err := s.stopAgent(s.worker); err != nil {
		return err
	}
	// The environment file of agent-a under --real-adapter names no fixture:
	// the adapter runs its default command. It runs in a directory of the
	// root, with the caller's own home so that the program is signed in.
	before, err := s.roster(partNick, chGeneral)
	if err != nil {
		return err
	}
	known := map[int64]bool{}
	for _, sess := range before {
		known[sess.ID] = true
	}
	env := append(r.isolatedEnv(), "HOME="+os.Getenv("HOME"))
	c, err := r.spawn("agent-a-real", "sh", []string{"-c", r.source("agent-a.env") + "cd " + shq(r.path("tmp")) + " && exec " + shq(r.bin.adapter)}, env)
	if err != nil {
		return err
	}
	s.cleanup(c.kill)
	// Its runtime and model are shown to the agent itself and to the operator,
	// so the agent's own credential reads them.
	var session schema.SessionV1
	if err := s.until("the real adapter to open a session", 30*time.Second, func() (bool, error) {
		roster, err := s.roster(partWorker, chGeneral)
		if err != nil {
			return false, err
		}
		for _, sess := range sessionsNamed(roster, "worker-test") {
			if !known[sess.ID] {
				session = sess
			}
		}
		return session.ID != 0, nil
	}); err != nil {
		return err
	}
	if err := s.say(chGeneral, "p313 reply with the single word ok"); err != nil {
		return err
	}
	msg, err := s.waitMessage(partNick, chGeneral, "p313 reply with")
	if err != nil {
		return err
	}
	run1, err := s.runAnswering(chGeneral, msg.ID)
	if err != nil {
		return err
	}
	var ended schema.RunV1
	if err := s.until("the real adapter's run to end", 150*time.Second, func() (bool, error) {
		var err error
		if ended, err = s.getRun(run1.ID); err != nil {
			return false, err
		}
		return schema.RunStateTerminal(ended.State), nil
	}); err != nil {
		return err
	}
	// A run that failed (the program not signed in, say) is an answer too: it says
	// so in the outcome, and nothing falls back to anything else.
	fmt.Printf("P3-13 runtime=%s model=%s outcome=%q state=%s\n", session.Runtime, session.Model, ended.Outcome, ended.State)
	return nil
}

// ---------------------------------------------------------------- P3-14

// threeRuntimes are the adapters of P3-14: one per runtime the roadmap names,
// each with a stub in place of the program. The profile decides how the output
// is read, so the codex stub prints Codex's events and the others plain text.
var threeRuntimes = []struct{ runtime, model, script, tag string }{
	{"claude-code", "stub-claude", "claude-stub.sh", "claude-stub: "},
	{"codex", "stub-codex", "codex-stub.sh", "codex-stub: "},
	{"ollama", "stub-ollama", "ollama-stub.sh", "ollama-stub: "},
}

// p314Three runs three adapters at once against one server, one for each
// runtime, with stubs for the programs, and checks that the roster shows three
// sessions with three runtimes, that each answers one message with a run of its
// own, and that the events socket delivers the activity of all three runs. No
// model is involved; the walkthrough's P3-14 is the same with real ones.
func (s *suite) p314Three() error {
	if err := s.stopAgent(s.worker); err != nil {
		return err
	}
	s.worker = nil
	feed, err := s.followEvents(partNick, chGeneral)
	if err != nil {
		return err
	}
	var started []*agent
	defer func() {
		for _, a := range started {
			_ = s.stopAgent(a)
		}
	}()
	sessions := map[int64]string{} // session id -> runtime
	for _, rt := range threeRuntimes {
		a, err := s.launchAgent(agentSpec{part: partWorker, script: rt.script, env: []string{
			"CONCH_ADAPTER_RUNTIME=" + rt.runtime,
			"CONCH_ADAPTER_MODEL=" + rt.model,
			"CONCH_ADAPTER_FIXTURE=0",
			"CONCH_ADAPTER_LOCK_FILE=" + s.r.path("p314-"+rt.runtime+".lock"),
		}})
		if err != nil {
			return fmt.Errorf("the %s adapter: %w", rt.runtime, err)
		}
		started = append(started, a)
		sessions[a.session] = rt.runtime
	}

	// The roster: three sessions, three runtimes. The runtime is for the
	// operator and for the agent itself; a member sees the model.
	roster, err := s.roster(partWorker, chGeneral)
	if err != nil {
		return err
	}
	for id, runtime := range sessions {
		sess, ok := sessionOf(roster, id)
		if !ok || sess.Runtime != runtime || sess.Status == schema.SessionStatusOffline {
			return fmt.Errorf("session #%d should be a live %s session in the roster; it is %+v", id, runtime, sess)
		}
	}
	member, err := s.roster(partNick, chGeneral)
	if err != nil {
		return err
	}
	for id, runtime := range sessions {
		sess, ok := sessionOf(member, id)
		if !ok || sess.Model == "" || sess.Runtime != "" {
			return fmt.Errorf("nick-test should see a model and no runtime for the %s session #%d; it sees %+v", runtime, id, sess)
		}
	}
	step("P3-14 the roster shows three sessions: claude-code, codex and ollama; a member sees their models")

	if err := s.say(chGeneral, "p314 three runtimes, answer please"); err != nil {
		return err
	}
	msg, err := s.waitMessage(partNick, chGeneral, "p314 three runtimes")
	if err != nil {
		return err
	}
	runs := map[int64]schema.RunV1{} // by session
	if err := s.until("one completed run from each of the three sessions", 40*time.Second, func() (bool, error) {
		all, err := s.runsOf(partNick, chGeneral)
		if err != nil {
			return false, err
		}
		for _, r := range all {
			if _, ours := sessions[r.SessionID]; ours && r.MessageID == msg.ID && r.State == schema.RunStateCompleted {
				runs[r.SessionID] = r
			}
		}
		return len(runs) == len(sessions), nil
	}); err != nil {
		return err
	}
	// Each runtime answered in its own words.
	history, err := s.history(partNick, chGeneral, msg.ID)
	if err != nil {
		return err
	}
	for _, rt := range threeRuntimes {
		// A stub answers with the last line of the prompt, which is the
		// message behind its author's id.
		if !slices.ContainsFunc(history, func(m schema.MessageV2) bool {
			return m.AuthorID == s.pid("worker-test") && strings.HasPrefix(m.Body, rt.tag) && strings.Contains(m.Body, "p314 three runtimes")
		}) {
			return fmt.Errorf("no reply from the %s adapter's stub: want one starting %q", rt.runtime, rt.tag)
		}
	}
	// The events socket delivered the activity of all three runs.
	if err := s.until("the activity of all three runs on the events socket", 20*time.Second, func() (bool, error) {
		for _, run := range runs {
			kinds := map[schema.ActivityKind]bool{}
			for _, a := range feed.activitySeen() {
				if a.RunID == run.ID {
					kinds[a.Kind] = true
				}
			}
			if !kinds[schema.ActivityKindCommandStarted] || !kinds[schema.ActivityKindReplyPosted] {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return err
	}
	step("P3-14 each of the three adapters answered with a run of its own, and the events socket carried the activity of all three")
	return nil
}
