package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// P3-01, 02, 03, 04, 09, 10 and 11: people, messages, sessions and access.

// ---------------------------------------------------------------- P3-01

// p301Roster starts everyone, and checks that the roster of general shows
// exactly the five participants, with the right kinds, and each of them the
// same roster.
func (s *suite) p301Roster() error {
	var err error
	if s.tuiA, err = s.startTUI(partNick); err != nil {
		return err
	}
	if s.tuiB, err = s.startTUI(partGuest); err != nil {
		return err
	}
	if s.worker, err = s.launchAgent(agentSpec{part: partWorker, script: "echo.sh"}); err != nil {
		return err
	}
	if s.observer, err = s.launchAgent(agentSpec{part: partObserver, script: "echo.sh"}); err != nil {
		return err
	}
	if err := s.startBrowser(); err != nil {
		return err
	}

	type who struct {
		kind   schema.PrincipalKind
		client string
	}
	want := map[string]who{
		"nick-test":     {schema.PrincipalHuman, "tui"},
		"guest-test":    {schema.PrincipalHuman, "tui"},
		"browser-test":  {schema.PrincipalHuman, "web"},
		"worker-test":   {schema.PrincipalAgent, "conch-adapter"},
		"observer-test": {schema.PrincipalAgent, "conch-adapter"},
	}
	check := func(viewer string) (bool, error) {
		roster, err := s.roster(viewer, chGeneral)
		if err != nil {
			return false, err
		}
		if len(roster) < len(want) {
			return false, nil
		}
		if len(roster) != len(want) {
			return false, fmt.Errorf("the roster has %d sessions, want %d", len(roster), len(want))
		}
		seen := map[string]bool{}
		for _, sess := range roster {
			w, ok := want[sess.Name]
			if !ok {
				return false, fmt.Errorf("the roster has a session of %q, who is not a participant", sess.Name)
			}
			if seen[sess.Name] {
				return false, fmt.Errorf("the roster lists %s twice", sess.Name)
			}
			seen[sess.Name] = true
			if sess.PrincipalID != s.pid(sess.Name) || sess.Kind != w.kind || sess.Client != w.client || sess.EndedAt != nil {
				return false, fmt.Errorf("the session of %s is principal %d, kind %q, client %q; want principal %d, kind %q, client %q",
					sess.Name, sess.PrincipalID, sess.Kind, sess.Client, s.pid(sess.Name), w.kind, w.client)
			}
		}
		return true, nil
	}
	for _, viewer := range []string{partNick, partGuest, partBrowser, partWorker, partObserver} {
		if err := s.until("the roster of general, read as "+viewer+", to show the five participants", 20*time.Second, func() (bool, error) { return check(viewer) }); err != nil {
			return err
		}
	}

	// The terminal clients show the same people, from the same sessions.
	for _, t := range []*tui{s.tuiA, s.tuiB} {
		if err := t.selectChannel(chGeneral); err != nil {
			return err
		}
		if err := t.waitFor("both agents online in the members line", 20*time.Second, func(rows []string) bool {
			return showing(rows, "agents:", "worker-test ●", "observer-test ●")
		}); err != nil {
			return err
		}
	}
	if err := s.tuiA.waitFor("the three channels nick-test may use", 10*time.Second, func(rows []string) bool {
		return showing(rows, chGeneral) && showing(rows, chNets) && showing(rows, chPrivate)
	}); err != nil {
		return err
	}
	if showing(s.tuiB.rows(), chPrivate) {
		return fmt.Errorf("the TUI of guest-test lists %s, which it is not in", chPrivate)
	}
	return nil
}

// ---------------------------------------------------------------- P3-02

// p302Send types a message in each terminal, lets worker-test answer each, and
// checks that every participant who may read general has the same four
// messages in the same order, in its history and on its live socket, and that
// each terminal shows each once.
func (s *suite) p302Send() error {
	mark, err := s.lastID(chGeneral)
	if err != nil {
		return err
	}
	readers := []string{partNick, partGuest, partBrowser, partWorker, partObserver}
	feeds := map[string]*messageFeed{}
	for _, p := range readers {
		if feeds[p], err = s.followMessages(p, chGeneral, mark); err != nil {
			return fmt.Errorf("the live socket of %s: %w", p, err)
		}
	}
	for _, t := range []*tui{s.tuiA, s.tuiB} {
		if err := t.selectChannel(chGeneral); err != nil {
			return err
		}
	}

	messages := func(n int) ([]schema.MessageV2, error) {
		var got []schema.MessageV2
		err := s.until(fmt.Sprintf("%d new messages in general", n), 30*time.Second, func() (bool, error) {
			var err error
			if got, err = s.history(partNick, chGeneral, mark); err != nil {
				return false, err
			}
			if len(got) > n {
				return false, fmt.Errorf("general has %d new messages, want %d", len(got), n)
			}
			return len(got) == n, nil
		})
		return got, err
	}
	if err := s.tuiA.sendLine("p302nick hello"); err != nil {
		return err
	}
	if _, err := messages(2); err != nil {
		return err
	}
	if err := s.tuiB.sendLine("p302guest hello"); err != nil {
		return err
	}
	want, err := messages(4)
	if err != nil {
		return err
	}

	authors := []int64{s.pid("nick-test"), s.pid("worker-test"), s.pid("guest-test"), s.pid("worker-test")}
	var ids []int64
	for i, m := range want {
		if m.AuthorID != authors[i] {
			return fmt.Errorf("new message %d was written by principal %d, want %d", i+1, m.AuthorID, authors[i])
		}
		ids = append(ids, m.ID)
	}
	if want[0].Body != "p302nick hello" || want[2].Body != "p302guest hello" {
		return errors.New("the typed messages do not read as typed")
	}

	for _, p := range readers {
		got, err := s.history(p, chGeneral, mark)
		if err != nil {
			return fmt.Errorf("history as %s: %w", p, err)
		}
		var gotIDs []int64
		for _, m := range got {
			gotIDs = append(gotIDs, m.ID)
		}
		if !slices.Equal(gotIDs, ids) {
			return fmt.Errorf("the history of %s is messages %v, nick-test's is %v", p, gotIDs, ids)
		}
		if err := s.until("the live socket of "+p+" to carry the four messages", 20*time.Second, func() (bool, error) {
			return len(feeds[p].ids()) >= len(ids), nil
		}); err != nil {
			return err
		}
		if live := feeds[p].ids(); !slices.Equal(live, ids) {
			return fmt.Errorf("the live socket of %s carried messages %v, history has %v", p, live, ids)
		}
	}

	for _, t := range []*tui{s.tuiA, s.tuiB} {
		if err := t.waitFor("all four messages", 20*time.Second, func(rows []string) bool {
			return countRows(rows, "worker-test [agent]", "p302") >= 2 && showing(rows, "guest-test", "p302guest")
		}); err != nil {
			return err
		}
		rows := t.rows()
		for _, c := range []struct {
			parts []string
			want  int
		}{
			{[]string{"nick-test", "p302nick"}, 1},
			{[]string{"guest-test", "p302guest"}, 1},
			{[]string{"worker-test [agent]", "p302"}, 2},
		} {
			if got := countRows(rows, c.parts...); got != c.want {
				return fmt.Errorf("the TUI of %s shows %v on %d rows, want %d:\n%s", t.part, c.parts, got, c.want, dump(rows))
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- P3-03

// p303Drafts puts the two terminals in different channels with different
// unsent lines and checks that typing in one changes nothing on the other.
func (s *suite) p303Drafts() error {
	a, b := s.tuiA, s.tuiB
	if err := a.selectChannel(chNets); err != nil {
		return err
	}
	if err := b.selectChannel(chGeneral); err != nil {
		return err
	}
	if err := a.typeText("draftone"); err != nil {
		return err
	}
	if err := b.typeText("drafttwo"); err != nil {
		return err
	}
	if err := a.waitFor("its own draft", 10*time.Second, func(r []string) bool { return showing(r, chNets+" > draftone") }); err != nil {
		return err
	}
	if err := b.waitFor("its own draft", 10*time.Second, func(r []string) bool { return showing(r, chGeneral+" > drafttwo") }); err != nil {
		return err
	}

	// More typing on A, a channel change on A and back, and then a message
	// that B must receive: it arrives after everything A did, so whatever A
	// could have done to B has been delivered by then.
	if err := a.typeText("more"); err != nil {
		return err
	}
	if err := a.selectChannel(chPrivate); err != nil {
		return err
	}
	if err := a.selectChannel(chNets); err != nil {
		return err
	}
	if err := s.say(chGeneral, "p303barrier"); err != nil {
		return err
	}
	if err := b.waitFor("the barrier message", 20*time.Second, func(r []string) bool { return showing(r, "p303barrier") }); err != nil {
		return err
	}
	rows := b.rows()
	if got := selectedChannel(rows); got != chGeneral {
		return fmt.Errorf("the TUI of guest-test moved to %q while the other terminal was used", got)
	}
	if !showing(rows, chGeneral+" > drafttwo") || showing(rows, "draftone") {
		return fmt.Errorf("the draft of guest-test changed while the other terminal was used:\n%s", dump(rows))
	}
	rows = a.rows()
	if got := selectedChannel(rows); got != chNets {
		return fmt.Errorf("the TUI of nick-test is in %q, want %s", got, chNets)
	}
	if showing(rows, "drafttwo") {
		return fmt.Errorf("the draft of guest-test appears in the other terminal:\n%s", dump(rows))
	}

	// Leave both as they were: empty prompts, both in general.
	for _, t := range []*tui{a, b} {
		for range 40 {
			if err := t.press("\x7f"); err != nil {
				return err
			}
		}
	}
	if err := a.selectChannel(chGeneral); err != nil {
		return err
	}
	for _, t := range []*tui{a, b} {
		if err := t.waitFor("an empty prompt", 10*time.Second, func(r []string) bool {
			return showing(r, chGeneral+" >") && !showing(r, "draft")
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- P3-04

// p304TwoSessions signs nick-test in a second time and checks that the roster
// has one person with two sessions, that a credential cannot touch the other's
// session, and that revoking the second credential ends the second session and
// leaves the first terminal working.
func (s *suite) p304TwoSessions() error {
	second, err := s.startTUI(partNick2)
	if err != nil {
		return err
	}
	if err := second.selectChannel(chGeneral); err != nil {
		return err
	}
	nickSessions := func() ([]schema.SessionV1, error) {
		roster, err := s.roster(partGuest, chGeneral)
		return sessionsNamed(roster, "nick-test"), err
	}
	var mine []schema.SessionV1
	if err := s.until("two sessions of nick-test", 20*time.Second, func() (bool, error) {
		var err error
		mine, err = nickSessions()
		return len(mine) == 2, err
	}); err != nil {
		return err
	}
	first, other := mine[0], mine[1] // the older one is the first terminal's
	if first.ID == other.ID || first.Client != "tui" || other.Client != "tui" {
		return fmt.Errorf("nick-test's sessions are %d (%s) and %d (%s), want two TUI sessions", first.ID, first.Client, other.ID, other.Client)
	}
	members, err := s.r.client(s.token(partGuest))
	if err != nil {
		return err
	}
	list, err := members.ListChannelMembers(runCtx, chGeneral)
	if err != nil {
		return err
	}
	count := 0
	for _, m := range list.Members {
		if m.Name == "nick-test" {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("general lists nick-test %d times as a member, want once", count)
	}

	// A credential acts on its own sessions and no others: the second
	// terminal's credential, which is nick-test's too, and guest-test's.
	for _, c := range []struct{ by, what string }{{partNick2, "the other terminal's"}, {partGuest, "another person's"}} {
		for _, call := range []struct{ method, path string }{
			{http.MethodPost, "/v1/sessions/" + strconv.FormatInt(first.ID, 10) + "/heartbeat"},
			{http.MethodDelete, "/v1/sessions/" + strconv.FormatInt(first.ID, 10)},
		} {
			if err := s.as(c.by).refused(c.what+" credential on a session that is not its own", call.method, call.path, nil, http.StatusNotFound, schema.ErrorCodeSessionNotFound); err != nil {
				return err
			}
		}
	}
	if mine, err = nickSessions(); err != nil {
		return err
	}
	if len(mine) != 2 {
		return fmt.Errorf("after those refusals nick-test has %d sessions, want 2", len(mine))
	}

	// Quit the second terminal: one session is left, and the first is not
	// affected. Start it again, and revoke its credential.
	if err := second.quit(); err != nil {
		return err
	}
	if err := s.until("the second session to end with the second terminal", 15*time.Second, func() (bool, error) {
		mine, err := nickSessions()
		return len(mine) == 1 && mine[0].ID == first.ID, err
	}); err != nil {
		return err
	}
	second, err = s.startTUI(partNick2)
	if err != nil {
		return err
	}
	if err := second.selectChannel(chGeneral); err != nil {
		return err
	}
	if err := s.until("the second terminal's new session", 20*time.Second, func() (bool, error) {
		mine, err := nickSessions()
		return len(mine) == 2, err
	}); err != nil {
		return err
	}
	if err := s.admin("credential", "revoke", id(s.r.fx.creds[partNick2].id)); err != nil {
		return err
	}
	if err := s.until("the revoked credential's session to end", 20*time.Second, func() (bool, error) {
		mine, err := nickSessions()
		return len(mine) == 1 && mine[0].ID == first.ID, err
	}); err != nil {
		return err
	}
	// The terminal whose credential is gone is refused at its next request and
	// says to sign in; the other carries on.
	if err := second.sendLine("p304gone"); err != nil {
		return err
	}
	if err := second.waitFor("a request to sign in", 20*time.Second, func(r []string) bool { return showing(r, "not logged in") }); err != nil {
		return err
	}
	if err := s.tuiA.sendLine("p304alive"); err != nil {
		return err
	}
	msg, err := s.waitMessage(partGuest, chGeneral, "p304alive")
	if err != nil {
		return err
	}
	if msg.AuthorID != s.pid("nick-test") {
		return fmt.Errorf("the message from the first terminal was written by principal %d", msg.AuthorID)
	}
	posted, err := s.history(partNick, chGeneral, 0)
	if err != nil {
		return err
	}
	if len(withBody(posted, "p304gone")) != 0 {
		return errors.New("the revoked terminal posted a message")
	}
	if err := s.tuiA.waitFor("its own message, once", 15*time.Second, func(r []string) bool {
		return countRows(r, "nick-test", "p304alive") == 1
	}); err != nil {
		return err
	}
	creds, err := s.r.client(s.r.fx.operatorToken)
	if err != nil {
		return err
	}
	list2, err := creds.ListCredentials(runCtx, s.pid("nick-test"))
	if err != nil {
		return err
	}
	for _, c := range list2.Credentials {
		switch c.ID {
		case s.r.fx.creds[partNick].id:
			if c.RevokedAt != nil {
				return errors.New("the first terminal's credential is revoked")
			}
		case s.r.fx.creds[partNick2].id:
			if c.RevokedAt == nil {
				return errors.New("the second terminal's credential is not revoked")
			}
		}
	}
	_ = second.quit()
	return nil
}

// ---------------------------------------------------------------- P3-09

// refuser holds an address and closes every connection made to it, counting
// them: it stands where the server was, so that what a client does while the
// server is down can be counted.
type refuser struct {
	ln    net.Listener
	mu    sync.Mutex
	times []time.Time
	done  chan struct{}
}

func refuseAt(addr string) (*refuser, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	r := &refuser{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			r.mu.Lock()
			r.times = append(r.times, time.Now())
			r.mu.Unlock()
			_ = conn.Close()
		}
	}()
	return r, nil
}

func (r *refuser) attempts() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.times...)
}

func (r *refuser) close() {
	_ = r.ln.Close()
	<-r.done
}

// restartServer starts conchd again on the address and data directory of the
// one that stopped, as the walkthrough's last command does.
func (r *runner) restartServer() error {
	c, err := r.spawn("conchd-restart", r.bin.conchd, r.serveArgs(r.addr), r.isolatedEnv())
	if err != nil {
		return err
	}
	r.server = c
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !c.running() {
			return fmt.Errorf("conchd exited at once after the restart; its log is %s", c.log)
		}
		resp, err := httpClient.Get(r.baseURL() + "/healthz") // #nosec G107 -- this run's own loopback conchd
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if err := pause(50 * time.Millisecond); err != nil {
			return err
		}
	}
	return errors.New("conchd was not healthy within 30s of the restart")
}

// p309Restart stops the server under a terminal, types a line while it is down,
// restarts the server on the same data and checks that the terminal says what
// is happening, retries a bounded number of times, recovers by itself, shows
// the history once, and does not send the line that failed.
func (s *suite) p309Restart() error {
	// No agent takes part in this row: the server goes away under all of them.
	for _, a := range []*agent{s.worker, s.observer} {
		if err := s.stopAgent(a); err != nil {
			return err
		}
	}
	s.worker, s.observer = nil, nil

	t := s.tuiB
	if err := t.selectChannel(chNets); err != nil {
		return err
	}
	for _, seed := range []string{"p309seedone", "p309seedtwo"} {
		if err := s.say(chNets, seed); err != nil {
			return err
		}
	}
	if err := t.waitFor("the two messages sent before the cut", 20*time.Second, func(r []string) bool {
		return showing(r, "p309seedone") && showing(r, "p309seedtwo")
	}); err != nil {
		return err
	}

	// The cut: the server stops, and something that only refuses stands in
	// its place.
	s.r.server.stop()
	standIn, err := refuseAt(s.r.addr)
	if err != nil {
		return fmt.Errorf("hold the server's address: %w", err)
	}
	defer standIn.close()
	if err := t.waitFor("live updates to be marked as reconnecting", 30*time.Second, func(r []string) bool {
		return showing(r, "reconnecting")
	}); err != nil {
		return err
	}
	if err := t.waitFor("the members line to say it is unavailable", 30*time.Second, func(r []string) bool {
		return showing(r, "members: unavailable")
	}); err != nil {
		return err
	}
	if !showing(t.rows(), "p309seedone") {
		return errors.New("the TUI dropped the history it had while the server was down")
	}

	// A line typed now cannot be sent. It stays in the prompt, the TUI says it
	// was not sent, and nothing sends it later.
	if err := t.sendLine("p309interrupted"); err != nil {
		return err
	}
	if err := t.waitFor("the failure of the send", 20*time.Second, func(r []string) bool { return showing(r, "cli:") }); err != nil {
		return err
	}
	// Held down long enough for two retries, which come one second and then
	// two seconds after each other.
	cut := time.Now()
	if err := s.until("the server to have been down for 4s", 30*time.Second, func() (bool, error) {
		return time.Since(cut) >= 4*time.Second, nil
	}); err != nil {
		return err
	}
	attempts := standIn.attempts()
	standIn.close()
	down := time.Since(cut)
	var offsets []string
	for _, w := range retryWaves(attempts) {
		offsets = append(offsets, fmt.Sprintf("%dx at +%.1fs", len(w), w[0].Sub(cut).Seconds()))
	}
	fmt.Printf("    the TUI connected %s while the server was down\n", strings.Join(offsets, ", "))

	if err := s.r.restartServer(); err != nil {
		return err
	}
	if err := boundedRetries(attempts, down); err != nil {
		return err
	}

	if err := t.waitFor("the TUI to recover by itself", 60*time.Second, func(r []string) bool {
		return showing(r, "online:") && !showing(r, "reconnecting") && !showing(r, "members: unavailable")
	}); err != nil {
		return err
	}
	if err := s.say(chNets, "p309after"); err != nil {
		return err
	}
	if err := t.waitFor("a message sent after the restart", 30*time.Second, func(r []string) bool { return showing(r, "p309after") }); err != nil {
		return err
	}
	rows := t.rows()
	for _, word := range []string{"p309seedone", "p309seedtwo", "p309after"} {
		if n := countRows(rows, word); n != 1 {
			return fmt.Errorf("after the restart the TUI shows %s on %d rows, want once:\n%s", word, n, dump(rows))
		}
	}
	if !showing(rows, chNets+" > p309interrupted") {
		return fmt.Errorf("the line that was not sent is gone from the prompt:\n%s", dump(rows))
	}
	// The history on the server: each message once, and the interrupted one
	// never.
	got, err := s.history(partGuest, chNets, 0)
	if err != nil {
		return err
	}
	for word, want := range map[string]int{"p309seedone": 1, "p309seedtwo": 1, "p309after": 1, "p309interrupted": 0} {
		if n := len(withBody(got, word)); n != want {
			return fmt.Errorf("the server holds %s %d times, want %d", word, n, want)
		}
	}
	for range 40 {
		if err := t.press("\x7f"); err != nil {
			return err
		}
	}
	return t.selectChannel(chGeneral)
}

// retryWaves groups connection attempts into waves: a client that retries
// opens several connections at once (its message socket, its events socket, its
// reads), then waits.
func retryWaves(times []time.Time) [][]time.Time {
	var waves [][]time.Time
	for _, at := range times {
		if n := len(waves); n > 0 && at.Sub(waves[n-1][len(waves[n-1])-1]) < 500*time.Millisecond {
			waves[n-1] = append(waves[n-1], at)
			continue
		}
		waves = append(waves, []time.Time{at})
	}
	return waves
}

// boundedRetries checks that a client that was refused for down tried again,
// and not too often: the waits between its waves of attempts do not shrink and
// start near the minimum the TUI documents (one second), so doubling from one
// second the waves fit in down with room to spare, and each wave is the size of
// what the TUI keeps open and no more.
func boundedRetries(attempts []time.Time, down time.Duration) error {
	waves := retryWaves(attempts)
	if len(waves) < 2 {
		return fmt.Errorf("the TUI tried the server %d times in the %s it was down, and did not retry", len(attempts), down.Round(time.Second))
	}
	// Doublings of one second that fit in the time, and the first attempt and
	// the retry after the first second.
	if limit := doublings(down) + 3; len(waves) > limit {
		return fmt.Errorf("the TUI tried the server in %d waves in the %s it was down; with delays doubling from a second that is more than %d", len(waves), down.Round(time.Second), limit)
	}
	prev := time.Duration(0)
	for i := 1; i < len(waves); i++ {
		gap := waves[i][0].Sub(waves[i-1][0])
		if i > 1 && gap+300*time.Millisecond < prev {
			return fmt.Errorf("the TUI's retries came closer together (%s, then %s) instead of further apart", prev.Round(100*time.Millisecond), gap.Round(100*time.Millisecond))
		}
		prev = gap
	}
	for _, w := range waves {
		if len(w) > 16 {
			return fmt.Errorf("the TUI opened %d connections at once while the server was down", len(w))
		}
	}
	return nil
}

// doublings is how many times a delay that starts at one second can double
// and still have run out before d.
func doublings(d time.Duration) int {
	n := 0
	for total, delay := time.Second, time.Second; total <= d; delay *= 2 {
		n++
		total += delay * 2
	}
	return n
}

// ---------------------------------------------------------------- P3-10

// p310Access removes guest-test from general and revokes browser-test's
// credential while both have sockets open. The sockets must close with 1008, the
// next read, write and reconnect must be refused, and nobody else is affected.
// A socket outlives a revoked credential by up to the server's recheck
// interval (30 s), so the browser's part is finished at the end of the suite;
// the rest of this row does not wait for it.
func (s *suite) p310Access() error {
	// guest-test's sockets: messages and events, opened as the clients open
	// them.
	guestMsgs, err := s.followMessages(partGuest, chGeneral, 0)
	if err != nil {
		return err
	}
	guestEvents, err := s.followEvents(partGuest, chGeneral)
	if err != nil {
		return err
	}
	// The browser's: a ticket for each, spent once. A ticket cannot be spent
	// twice.
	ticket, err := s.newTicket(partBrowser)
	if err != nil {
		return err
	}
	query := url.Values{"channel": {chGeneral}}
	conn, status, err := s.dialTicket("/v2/ws", query, ticket)
	if err != nil {
		return fmt.Errorf("the browser's first use of a ticket: status %d: %w", status, err)
	}
	browserMsgs := s.holdConn(conn)
	if _, status, err := s.dialTicket("/v2/ws", query, ticket); err == nil || status != http.StatusUnauthorized {
		return fmt.Errorf("a ticket spent a second time was answered with status %d (%s), want 401", status, describe(err))
	}
	if _, status, err := s.dialTicket("/v1/events/ws", query, ticket); err == nil || status != http.StatusUnauthorized {
		return fmt.Errorf("a spent ticket on another socket was answered with status %d (%s), want 401", status, describe(err))
	}
	browserEvents, err := s.holdTicketSocket(partBrowser, "/v1/events/ws", query)
	if err != nil {
		return err
	}
	spare, err := s.newTicket(partBrowser)
	if err != nil {
		return err
	}

	// The two changes.
	if err := s.admin("member", "remove", chGeneral, id(s.pid("guest-test"))); err != nil {
		return err
	}
	if err := s.admin("credential", "revoke", id(s.r.fx.creds[partBrowser].id)); err != nil {
		return err
	}
	revoked := time.Now()

	// guest-test: both sockets close with 1008 at once.
	if err := s.until("guest-test's sockets to close", 15*time.Second, func() (bool, error) {
		return guestMsgs.closed() != nil && guestEvents.closed() != nil, nil
	}); err != nil {
		return err
	}
	for what, err := range map[string]error{"message socket": guestMsgs.closed(), "events socket": guestEvents.closed()} {
		if code := closeCode(err); code != websocket.StatusPolicyViolation {
			return fmt.Errorf("guest-test's %s closed with %d (%s), want 1008", what, code, describe(err))
		}
	}
	// ... and read, write and reconnect are refused, as a stranger's would be.
	guest := s.as(partGuest)
	path := "/v2/channels/" + chGeneral
	for _, c := range []struct {
		what, method, path string
		body               any
	}{
		{"a read", http.MethodGet, path + "/messages", nil},
		{"a write", http.MethodPost, path + "/messages", schema.PostMessageRequestV2{Body: "p310gone"}},
		{"the roster", http.MethodGet, "/v1/sessions?channel=" + chGeneral, nil},
	} {
		if err := guest.refused("guest-test's "+c.what+" after its removal", c.method, c.path, c.body, http.StatusNotFound, "channel_not_found"); err != nil {
			return err
		}
	}
	if _, err := s.followMessages(partGuest, chGeneral, 0); !channelNotFound(err) {
		return fmt.Errorf("guest-test's reconnect of the message socket: %s, want 404 channel_not_found", describe(err))
	}
	if _, err := s.followEvents(partGuest, chGeneral); !channelNotFound(err) {
		return fmt.Errorf("guest-test's reconnect of the events socket: %s, want 404 channel_not_found", describe(err))
	}

	// browser-test: nothing it holds works. A new request, a new ticket, and
	// the ticket it had in hand are all refused.
	browser := s.as(partBrowser)
	for _, c := range []struct{ what, method, path string }{
		{"a read", http.MethodGet, path + "/messages"},
		{"a new ticket", http.MethodPost, "/v1/ws-tickets"},
	} {
		if err := browser.refused("browser-test's "+c.what+" after the revoke", c.method, c.path, nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
			return err
		}
	}
	if err := browser.refused("browser-test's write after the revoke", http.MethodPost, path+"/messages", schema.PostMessageRequestV2{Body: "p310gone"}, http.StatusUnauthorized, "unauthenticated"); err != nil {
		return err
	}
	if _, status, err := s.dialTicket("/v2/ws", query, spare); err == nil || status != http.StatusUnauthorized {
		return fmt.Errorf("browser-test's reconnect with a ticket it held: status %d (%s), want 401", status, describe(err))
	}

	// Everyone else carries on: nick-test sends from its terminal and the two
	// agents, who were not removed or revoked, read it.
	if err := s.tuiA.selectChannel(chGeneral); err != nil {
		return err
	}
	if err := s.tuiA.sendLine("p310carryon"); err != nil {
		return err
	}
	for _, p := range []string{partNick, partWorker, partObserver} {
		if _, err := s.waitMessage(p, chGeneral, "p310carryon"); err != nil {
			return err
		}
	}
	general, err := s.history(partNick, chGeneral, 0)
	if err != nil {
		return err
	}
	if len(withBody(general, "p310gone")) != 0 {
		return errors.New("a refused write reached general")
	}

	// The sockets of the revoked credential close when the server next checks
	// the credential: that is finished at the end of the suite.
	s.later("P3-10", "browser-test's sockets close with 1008 after the revoke", func() error {
		if err := s.until("browser-test's sockets to close", 50*time.Second, func() (bool, error) {
			_, a := browserMsgs.closeStatus()
			_, b := browserEvents.closeStatus()
			return a && b, nil
		}); err != nil {
			return err
		}
		for what, sock := range map[string]*ticketSocket{"message socket": browserMsgs, "events socket": browserEvents} {
			if code, _ := sock.closeStatus(); code != websocket.StatusPolicyViolation {
				return fmt.Errorf("browser-test's %s closed with %d, want 1008", what, code)
			}
		}
		step("P3-10 browser-test's sockets closed with 1008 %s after the revoke", time.Since(revoked).Round(time.Second))
		return nil
	})
	return nil
}

// ---------------------------------------------------------------- P3-11

// p311Core runs with no LiveKit and no model, and checks that chat, sessions,
// runs, an approval raised by approval.sh and decided in the TUI, and the audit
// export all work, and that voice says it is not configured.
func (s *suite) p311Core() error {
	_, err := s.cli("voice", "status", chGeneral)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		return fmt.Errorf("conch voice status with no LiveKit answered %q, want a refusal that says it is not configured", describe(err))
	}
	out, err := s.cli("whoami")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "name: nick-test") {
		return fmt.Errorf("conch whoami does not say it is nick-test: %q", out)
	}

	if err := s.stopAgent(s.worker); err != nil {
		return err
	}
	if s.worker, err = s.launchAgent(agentSpec{part: partWorker, script: "approval.sh"}); err != nil {
		return err
	}
	t := s.tuiA
	if err := t.selectChannel(chGeneral); err != nil {
		return err
	}
	if err := t.sendLine("p311 please approve"); err != nil {
		return err
	}
	msg, err := s.waitMessage(partNick, chGeneral, "p311 please approve")
	if err != nil {
		return err
	}
	run, err := s.runAnswering(chGeneral, msg.ID)
	if err != nil {
		return err
	}
	const title = "Approve the fixture action"
	var approvalID int64
	if err := s.until("the approval the fixture script raised", 20*time.Second, func() (bool, error) {
		var open schema.ListApprovalsResponseV1
		if err := s.as(partNick).call(http.MethodGet, "/v1/approvals", nil, &open); err != nil {
			return false, err
		}
		for _, a := range open.Approvals {
			if a.Title == title {
				approvalID = a.ID
			}
		}
		return approvalID != 0, nil
	}); err != nil {
		return err
	}
	if _, err := s.waitRunState(run.ID, schema.RunStateWaiting, 20*time.Second); err != nil {
		return err
	}

	// Decide it in the TUI: inbox, the first approval, the first option
	// (approve), a reason, enter.
	if err := t.press(keyTab); err != nil {
		return err
	}
	if err := t.waitFor("the approval in the inbox", 20*time.Second, func(r []string) bool { return showing(r, title) }); err != nil {
		return err
	}
	if err := t.press(keyEnter); err != nil {
		return err
	}
	if err := t.waitFor("the decision prompt", 10*time.Second, func(r []string) bool { return showing(r, "type reason to decide") }); err != nil {
		return err
	}
	if err := t.sendLine("p311 looks fine"); err != nil {
		return err
	}
	var resolved schema.GetApprovalResponseV1
	if err := s.until("the approval to be resolved", 20*time.Second, func() (bool, error) {
		if err := s.as(partNick).call(http.MethodGet, "/v1/approvals/"+id(approvalID), nil, &resolved); err != nil {
			return false, err
		}
		return resolved.Approval.State == schema.ApprovalStateResolved, nil
	}); err != nil {
		return err
	}
	if resolved.Resolution == nil || resolved.Resolution.Outcome != schema.OutcomeApproved {
		return fmt.Errorf("the approval was resolved with %+v, want it approved", resolved.Resolution)
	}
	if _, err := s.waitRunState(run.ID, schema.RunStateCompleted, 20*time.Second); err != nil {
		return err
	}
	if _, err := s.history(partNick, chGeneral, msg.ID); err != nil {
		return err
	}
	// Back to the channels view.
	for range 2 {
		if err := t.press(keyTab); err != nil {
			return err
		}
	}

	// The audit trail has the chain, whatever else (a notification, with ntfy
	// not configured) it has not.
	events, err := s.audit()
	if err != nil {
		return err
	}
	subject := "approval:" + id(approvalID)
	var chain []string
	for _, e := range events {
		if e.Subject == subject {
			chain = append(chain, e.Action)
		}
	}
	want := []string{"approval_created", "decision_cast", "approval_resolved"}
	var kept []string
	for _, a := range chain {
		if slices.Contains(want, a) {
			kept = append(kept, a)
		}
	}
	if !reflect.DeepEqual(kept, want) {
		return fmt.Errorf("the audit trail for %s holds %v, want %v in that order", subject, chain, want)
	}
	return nil
}

// audit exports the audit trail as the operator and decodes it.
func (s *suite) audit() ([]schema.AuditEventV1, error) {
	resp, err := api{base: s.r.baseURL(), token: s.r.fx.operatorToken}.send(http.MethodGet, "/v1/audit", nil)
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/audit: status %d (%s)", resp.status, errorCode(resp.body))
	}
	var events []schema.AuditEventV1
	for n, line := range strings.Split(strings.TrimRight(string(resp.body), "\n"), "\n") {
		var e schema.AuditEventV1
		if err := decodeStrict([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("audit export line %d: %w", n+1, err)
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("audit export line %d: %w", n+1, err)
		}
		events = append(events, e)
	}
	return events, nil
}
