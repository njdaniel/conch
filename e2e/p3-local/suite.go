package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// The automated P3 suite (issue #284): the thirteen rows of the brief's test
// table (docs/design/p3-local-prototype.md, section 4) asserted against the
// binaries this run built. The terminal clients run on pseudo-terminals and
// are driven with keys, the agents are conch-adapter processes running the
// fixture scripts, and the API is called to look at what the server shows,
// and to see the requests no client makes refused.
//
// The rows share one community and run in order: a later row starts from what
// the earlier ones left, which is stated where it matters. The server runs
// with --session-stale 2s --session-offline 5s so that P3-07 takes seconds
// (see suiteSessionStale); that makes a TUI's session, which is refreshed
// every ten seconds, go stale between refreshes, so no row asserts that a
// human is online, only that the session is there. Agents beat every second.

// The participants, by the name their environment file and credential have.
const (
	partNick     = "tui-a"
	partGuest    = "tui-b"
	partNick2    = "tui-a2"
	partBrowser  = "browser"
	partWorker   = "agent-a"
	partObserver = "agent-b"
)

// suite is one run of the scenarios.
type suite struct {
	r        *runner
	cleanups []func()
	notRun   []string

	// The long-lived participants. A row that needs one starts it if it is not
	// there, and a later row finds it as the earlier one left it.
	tuiA, tuiB *tui
	worker     *agent
	observer   *agent
	browserSes int64

	// sweep is when the server's session sweeper last ran, as the probe saw it
	// (see calibrate); zero until it has.
	sweep      time.Time
	sweepReady chan struct{}

	// deferred are checks a row hands over to the end of the suite because
	// they wait on the server's clock and nothing else needs to wait with
	// them.
	deferred []deferredCheck
}

type deferredCheck struct {
	id, what string
	run      func() error
}

// later hands a check to the end of the suite. It runs there, and fails the
// suite under its row's id.
func (s *suite) later(id, what string, run func() error) {
	s.deferred = append(s.deferred, deferredCheck{id, what, run})
}

// cleanup registers fn to run when the suite ends, last registered first.
func (s *suite) cleanup(fn func()) { s.cleanups = append(s.cleanups, fn) }

func (s *suite) stopAll() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
	s.cleanups = nil
}

// skip records a row that was not run, and says so where its line would be.
func (s *suite) skip(id, why string) {
	s.notRun = append(s.notRun, id)
	fmt.Printf("NOT RUN  %s: %s\n", id, why)
}

// token is the credential of a participant.
func (s *suite) token(part string) string { return s.r.fx.creds[part].token }

// as is a REST client acting as part.
func (s *suite) as(part string) api { return api{base: s.r.baseURL(), token: s.token(part)} }

// pid is the principal id of a person or agent by name.
func (s *suite) pid(name string) int64 { return s.r.fx.principals[name] }

// until polls f every few milliseconds until it reports true, an error, or d
// has passed. what names what is being waited for.
func (s *suite) until(what string, d time.Duration, f func() (bool, error)) error {
	deadline := time.Now().Add(d)
	for {
		ok, err := f()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not happen within %s", what, d)
		}
		if err := pause(40 * time.Millisecond); err != nil {
			return err
		}
	}
}

// ------------------------------------------------------------ the CLI

// cli runs the conch CLI as nick-test, through a shell with the participant's
// environment file, as the walkthrough's commands do. Arguments are quoted.
func (s *suite) cli(args ...string) (string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shq(a)
	}
	out, err := s.r.shell(s.r.asConch(partNick, quoted...))
	if err != nil {
		return "", fmt.Errorf("conch %s: %w", strings.Join(args[:min(len(args), 2)], " "), err)
	}
	return out, nil
}

// say posts body to channel as nick-test with `conch send`, with the flags
// that give it an audience.
func (s *suite) say(channel, body string, flags ...string) error {
	_, err := s.cli(append(append([]string{"send"}, flags...), channel, body)...)
	return err
}

// admin runs `conch admin ...` as the operator.
func (s *suite) admin(args ...string) error {
	_, err := s.r.conch("", append([]string{"admin"}, args...)...)
	return err
}

// ------------------------------------------------------------- reading

// history is every message part may read in channel above after, oldest first.
func (s *suite) history(part, channel string, after int64) ([]schema.MessageV2, error) {
	c, err := s.r.client(s.token(part))
	if err != nil {
		return nil, err
	}
	var all []schema.MessageV2
	for {
		page, err := c.ListMessagesV2(runCtx, channel, after, 100)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		if page.NextAfter == 0 {
			return all, nil
		}
		after = page.NextAfter
	}
}

// lastID is the id of the newest message nick can read in channel, 0 when
// there is none: the point a row counts from.
func (s *suite) lastID(channel string) (int64, error) {
	msgs, err := s.history(partNick, channel, 0)
	if err != nil || len(msgs) == 0 {
		return 0, err
	}
	return msgs[len(msgs)-1].ID, nil
}

// withBody is the messages whose body contains word.
func withBody(msgs []schema.MessageV2, word string) []schema.MessageV2 {
	var out []schema.MessageV2
	for _, m := range msgs {
		if strings.Contains(m.Body, word) {
			out = append(out, m)
		}
	}
	return out
}

// waitMessage polls until part can read exactly one message in channel whose
// body contains word, and returns it.
func (s *suite) waitMessage(part, channel, word string) (schema.MessageV2, error) {
	var found schema.MessageV2
	err := s.until(fmt.Sprintf("%s to read a message with %q in %s", part, word, channel), 20*time.Second, func() (bool, error) {
		msgs, err := s.history(part, channel, 0)
		if err != nil {
			return false, err
		}
		got := withBody(msgs, word)
		if len(got) > 1 {
			return false, fmt.Errorf("%s reads %d messages with %q in %s, want one", part, len(got), word, channel)
		}
		if len(got) == 1 {
			found = got[0]
		}
		return len(got) == 1, nil
	})
	return found, err
}

// roster is the sessions of channel as part sees them.
func (s *suite) roster(part, channel string) ([]schema.SessionV1, error) {
	return s.r.sessions(s.token(part), channel)
}

// sessionOf is the session with id in the roster, if it is there.
func sessionOf(roster []schema.SessionV1, id int64) (schema.SessionV1, bool) {
	for _, sess := range roster {
		if sess.ID == id {
			return sess, true
		}
	}
	return schema.SessionV1{}, false
}

// sessionsNamed is the sessions in roster that belong to name.
func sessionsNamed(roster []schema.SessionV1, name string) []schema.SessionV1 {
	var out []schema.SessionV1
	for _, sess := range roster {
		if sess.Name == name {
			out = append(out, sess)
		}
	}
	return out
}

// runsOf is the runs part can see in channel.
func (s *suite) runsOf(part, channel string) ([]schema.RunV1, error) {
	c, err := s.r.client(s.token(part))
	if err != nil {
		return nil, err
	}
	var all []schema.RunV1
	after := int64(0)
	for {
		page, err := c.ListRuns(runCtx, channel, 0, cli.RunsPage{After: after}, 100)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Runs...)
		if page.NextAfter == 0 {
			return all, nil
		}
		after = page.NextAfter
	}
}

// getRun is run id as nick-test sees it.
func (s *suite) getRun(id int64) (schema.RunV1, error) {
	c, err := s.r.client(s.token(partNick))
	if err != nil {
		return schema.RunV1{}, err
	}
	return c.GetRun(runCtx, id)
}

// runAnswering waits for the run an agent started in answer to message id, as
// nick-test sees it, and returns it.
func (s *suite) runAnswering(channel string, messageID int64) (schema.RunV1, error) {
	var found schema.RunV1
	var seen []string
	err := s.until(fmt.Sprintf("a run answering message %d in %s", messageID, channel), 20*time.Second, func() (bool, error) {
		runs, err := s.runsOf(partNick, channel)
		if err != nil {
			return false, err
		}
		seen = seen[:0]
		for _, r := range runs {
			seen = append(seen, fmt.Sprintf("run %d answers %d (%s)", r.ID, r.MessageID, r.State))
			if r.MessageID == messageID {
				found = r
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		err = fmt.Errorf("%w; the runs nick-test can see in %s: %s", err, channel, strings.Join(seen, ", "))
	}
	return found, err
}

// waitRunState polls until run id, read as nick-test, is in state.
func (s *suite) waitRunState(id int64, state schema.RunState, d time.Duration) (schema.RunV1, error) {
	var last schema.RunV1
	err := s.until(fmt.Sprintf("run %d to be %s", id, state), d, func() (bool, error) {
		r, err := s.getRun(id)
		if err != nil {
			return false, err
		}
		last = r
		return r.State == state, nil
	})
	if err != nil {
		return last, fmt.Errorf("%w (it is %q)", err, last.State)
	}
	return last, nil
}

// ------------------------------------------------------------- agents

// agent is a conch-adapter process running a fixture script.
type agent struct {
	part    string
	label   string
	c       *child
	session int64
	channel string
}

// agentSpec says how to start an adapter.
type agentSpec struct {
	part, script string
	// channel replaces the one the environment file names; "" keeps it.
	channel string
	// env is more settings, as KEY=value.
	env []string
}

var agentCount int

// launchAgent starts an adapter the way the walkthrough prints it, with the
// participant's environment file, the fixture script, and a heartbeat every
// second: the server's thresholds are short here, and an adapter's default
// beat of ten seconds would let it go stale between two.
//
// With --real-adapter agent-a's environment file names no fixture, so these
// settings are given here as well, and every launch is a fixture.
func (s *suite) launchAgent(spec agentSpec) (*agent, error) {
	r := s.r
	agentCount++
	label := fmt.Sprintf("%s-run%d", spec.part, agentCount)
	channel := chGeneral
	vars := []string{
		"CONCH_ADAPTER_RUNTIME=fixture",
		"CONCH_ADAPTER_MODEL=fixture",
		"CONCH_ADAPTER_CMD=" + r.adapterCmd(spec.script),
		"CONCH_ADAPTER_FIXTURE=1",
		"CONCH_ADAPTER_HEARTBEAT=1s",
	}
	if spec.channel != "" {
		channel = spec.channel
		vars = append(vars, "CONCH_ADAPTER_CHANNEL="+spec.channel, "CONCH_ADAPTER_LOCK_FILE="+r.path(label+".lock"))
	}
	vars = append(vars, spec.env...)
	var exports strings.Builder
	for _, kv := range vars {
		k, v, _ := strings.Cut(kv, "=")
		exports.WriteString(" " + k + "=" + shq(v))
	}
	name := r.fx.creds[spec.part].principal
	before, err := s.roster(partNick, channel)
	if err != nil {
		return nil, err
	}
	known := make(map[int64]bool)
	for _, sess := range before {
		known[sess.ID] = true
	}
	// exec, so that the process the suite signals is the adapter itself.
	c, err := r.spawn(label, "sh", []string{"-c", r.source(spec.part+".env") + "export" + exports.String() + "; exec " + shq(r.bin.adapter)}, r.isolatedEnv())
	if err != nil {
		return nil, err
	}
	a := &agent{part: spec.part, label: label, c: c, channel: channel}
	s.cleanup(c.kill)
	err = s.until(spec.part+"'s adapter to open a session", 20*time.Second, func() (bool, error) {
		if !c.running() {
			return false, fmt.Errorf("the adapter exited at once; its log is %s", c.log)
		}
		roster, err := s.roster(partNick, channel)
		if err != nil {
			return false, err
		}
		for _, sess := range sessionsNamed(roster, name) {
			if !known[sess.ID] && sess.Client == "conch-adapter" {
				a.session = sess.ID
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// stop ends the adapter the way ctrl-c does and waits until its session has
// left the roster.
func (s *suite) stopAgent(a *agent) error {
	if a == nil {
		return nil
	}
	a.c.stop()
	return s.until(a.part+"'s session to end", 15*time.Second, func() (bool, error) {
		roster, err := s.roster(partNick, a.channel)
		if err != nil {
			return false, err
		}
		_, there := sessionOf(roster, a.session)
		return !there, nil
	})
}

// signal sends sig to the adapter.
func (a *agent) signal(sig string) error {
	cmd := exec.Command("kill", "-"+sig, strconv.Itoa(a.c.pid())) // #nosec G204 -- a signal name this file chooses, to a process this run started
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kill -%s %d: %w: %s", sig, a.c.pid(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// childrenOf lists the live processes whose parent is pid, by command line.
func childrenOf(pid int) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat") // #nosec G304 -- a /proc path built from a number
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...; comm may hold spaces and brackets.
		i := strings.LastIndexByte(string(stat), ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(stat)[i+1:])
		if len(fields) < 2 || fields[0] == "Z" {
			continue
		}
		if ppid, _ := strconv.Atoi(fields[1]); ppid == pid {
			out = append(out, strings.Join(argv(child), " "))
		}
	}
	return out
}

// ---------------------------------------------------------- the browser

// startBrowser makes the browser a participant the way the web client does
// it: a session of its own, kept online by an events socket opened with a
// ticket. There is no browser in this suite (E2b, issue #285, drives one); this
// is the browser's part of the same rows.
func (s *suite) startBrowser() error {
	var sess schema.SessionV1
	if err := s.as(partBrowser).call(http.MethodPost, "/v1/sessions", schema.CreateSessionRequestV1{Client: "web"}, &sess); err != nil {
		return err
	}
	s.browserSes = sess.ID
	sock, err := s.holdTicketSocket(partBrowser, "/v1/events/ws", map[string][]string{"channel": {chGeneral}, "session": {strconv.FormatInt(sess.ID, 10)}})
	if err != nil {
		return err
	}
	return s.until("the browser's events socket to send its roster", 10*time.Second, func() (bool, error) { return sock.framesSeen() > 0, nil })
}

// showAgentLogs prints the end of each adapter's log, credentials hidden: after
// a failure it is what says why an agent did not do what the row expected.
func (s *suite) showAgentLogs() {
	for _, c := range s.r.snapshot() {
		if !strings.Contains(c.name, "-run") {
			continue
		}
		data, err := os.ReadFile(c.log) // #nosec G304 -- a log under the run's root
		if err != nil || len(data) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "--- the end of the log of %s ---\n%s\n--- end ---\n", c.name, s.r.redact(tailLines(strings.TrimRight(string(data), "\n"), 12)))
	}
}

// ----------------------------------------------------------------- run

// runSuite runs the scenarios in order. The first failure stops the suite and
// names its row.
func (r *runner) runSuite() error {
	s := &suite{r: r}
	defer s.stopAll()
	if runtime.GOOS != "linux" {
		s.skip("P3-01 to P3-13", "the scenarios drive the terminal clients on a Linux pseudo-terminal")
		if os.Getenv("CI") != "" {
			return errors.New("the scenarios could not run on this system, and CI is set")
		}
		return nil
	}
	step("P3-12 the setup of issue #283 passed: the community reads back as specified and every participant connected and sent")
	rows := []struct {
		id, title string
		run       func() error
	}{
		{"P3-01", "five participants, five credentials, one roster", s.p301Roster},
		{"P3-02", "a message from each client arrives once, in order, everywhere", s.p302Send},
		{"P3-03", "two terminals, two channels, two drafts", s.p303Drafts},
		{"P3-04", "one person in two terminals", s.p304TwoSessions},
		{"P3-05", "a restricted channel, a net and a whisper", s.p305Scope},
		// P3-09 and P3-10 come before the rows about agents: a revoked
		// credential's sockets close up to half a minute after the revoke,
		// and P3-06 to P3-08 are the rows that fill the wait. The restart
		// also stops the agents, which those rows start afresh.
		{"P3-09", "the connection drops and the server restarts", s.p309Restart},
		{"P3-10", "a removed member and a revoked credential", s.p310Access},
		{"P3-06", "an agent through its states, as the API and the TUI show them", s.p306Runs},
		{"P3-07", "a frozen agent goes stale, then offline, and comes back", s.p307Freeze},
		{"P3-08", "instructions and cancel", s.p308Interventions},
		{"P3-11", "no LiveKit, no model", s.p311Core},
		{"P3-13", "one real adapter reply, opt in", s.p313Real},
	}
	// CONCH_P3_ROWS=P3-06,P3-07 runs only those rows, after P3-01 which starts
	// everyone. It is for working on one row, not for a result: the rows lean
	// on each other, and the line at the end says only some ran.
	only := os.Getenv("CONCH_P3_ROWS")
	began := time.Now()
	for _, row := range rows {
		if only != "" && row.id != "P3-01" && !strings.Contains(only, row.id) {
			continue
		}
		fmt.Printf("== %s %s ==\n", row.id, row.title)
		t0 := time.Now()
		if err := row.run(); err != nil {
			s.showAgentLogs()
			return fmt.Errorf("%s %s: %w", row.id, row.title, err)
		}
		if slices.Contains(s.notRun, row.id) {
			continue // it said so already
		}
		step("%s %s (%.1fs)", row.id, row.title, time.Since(t0).Seconds())
	}
	for _, d := range s.deferred {
		if err := d.run(); err != nil {
			return fmt.Errorf("%s %s: %w", d.id, d.what, err)
		}
	}
	if only == "" {
		if err := r.checkPublicRoutes(); err != nil {
			return err
		}
	}
	fmt.Printf("%s: %d scenarios in %.1fs\n", name, len(rows), time.Since(began).Seconds())
	if len(s.notRun) > 0 {
		fmt.Printf("%s: NOT RUN: %s\n", name, strings.Join(s.notRun, ", "))
	}
	return nil
}

// ---------------------------------------------------------- the sweeper

// sweepEvery is how often the server's session sweeper runs. It is the
// server's own default (defaultSessionSweepInterval, 5s) and has no flag.
const sweepEvery = 5 * time.Second

// startSweepProbe opens a session that nothing keeps alive, and watches the
// roster for the moment the sweeper first changes it. That moment is a sweep:
// from it, every sweep is a whole number of sweepEvery later. P3-07 needs
// this. With a stale threshold of 2s, an offline threshold of 5s and a sweep
// every 5s, a session whose heartbeats stop is seen stale only if a sweep
// happens to fall between 2s and 5s after its last heartbeat; at any other
// time the first sweep that notices it marks it offline, with no stale state
// in between. To see stale every time, the freeze is timed to the sweeps.
func (s *suite) startSweepProbe() error {
	var sess schema.SessionV1
	if err := s.as(partNick).call(http.MethodPost, "/v1/sessions", schema.CreateSessionRequestV1{Client: "probe"}, &sess); err != nil {
		return err
	}
	s.sweepReady = make(chan struct{})
	stop := make(chan struct{})
	s.cleanup(func() { close(stop) })
	go func() {
		defer close(s.sweepReady)
		for runCtx.Err() == nil {
			select {
			case <-stop:
				return
			default:
			}
			roster, err := s.roster(partNick, chGeneral)
			if err == nil {
				if got, ok := sessionOf(roster, sess.ID); ok && got.State != schema.SessionStateOnline {
					s.sweep = time.Now()
					break
				}
			}
			if pause(20*time.Millisecond) != nil {
				return
			}
		}
		_ = s.as(partNick).call(http.MethodDelete, "/v1/sessions/"+id(sess.ID), nil, nil)
	}()
	return nil
}

// nextFreeze is the first instant after now that falls two seconds after a
// sweep: a freeze then is followed by a sweep three seconds later, when the
// session's last heartbeat is between 3 and 4 seconds old, which is stale
// and not yet offline.
func (s *suite) nextFreeze(now time.Time) (time.Time, error) {
	if s.sweepReady == nil { // P3-06 has not started it (rows were filtered): learn it now
		if err := s.startSweepProbe(); err != nil {
			return time.Time{}, err
		}
	}
	select {
	case <-s.sweepReady:
	case <-time.After(30 * time.Second):
		return time.Time{}, errors.New("the probe session was not swept within 30s")
	case <-runCtx.Done():
		return time.Time{}, runCtx.Err()
	}
	if s.sweep.IsZero() {
		return time.Time{}, errors.New("the probe session was never swept")
	}
	at := s.sweep.Add(2 * time.Second)
	for at.Before(now.Add(200 * time.Millisecond)) {
		at = at.Add(sweepEvery)
	}
	return at, nil
}
