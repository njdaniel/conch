package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// P3-05: a restricted channel, a net and a whisper.

// scopeCase is one message with a limited audience: where it is, who may see
// it and who may not.
type scopeCase struct {
	what     string
	channel  string
	word     string
	flags    []string // the conch send flags that give it its audience
	insiders []string
	outside  []string // members of the channel, or not, who must not see it
	// hidden says the channel itself is hidden from the outsiders: every
	// route answers channel_not_found, rather than answering without the
	// message.
	hidden bool
	// control is a channel-wide message sent after the scoped one. An outsider's
	// socket that has received it is known to be working, so that not having
	// received the scoped one means something.
	control string
}

// p305Scope posts a message to a restricted channel, one to a net and one
// whisper, and checks who can read each in history, on a live socket, on a
// replaying socket and in search. The whisper is answered by worker-test with
// a run, which the outsiders must not be able to see either.
func (s *suite) p305Scope() error {
	// worker-test answers the whisper slowly, so that its run is open while it
	// is looked at.
	if err := s.stopAgent(s.worker); err != nil {
		return err
	}
	var err error
	if s.worker, err = s.launchAgent(agentSpec{part: partWorker, script: "slow.sh", env: []string{"FIXTURE_SLEEP=25"}}); err != nil {
		return err
	}
	worker := id(s.pid("worker-test"))

	cases := []scopeCase{
		{what: "a message in a restricted channel", channel: chPrivate, word: "p305private",
			insiders: []string{partNick, partWorker}, outside: []string{partGuest, partBrowser}, hidden: true},
		{what: "a message to a net", channel: chNets, word: "p305red", flags: []string{"--net", "red"},
			insiders: []string{partNick, partGuest}, outside: []string{partBrowser, partWorker}, control: "p305netcontrol"},
		{what: "a whisper", channel: chGeneral, word: "p305whisper", flags: []string{"--to", worker},
			insiders: []string{partNick, partWorker}, outside: []string{partGuest, partBrowser, partObserver}, control: "p305gencontrol"},
	}

	// Sockets are open before anything is posted: "live" means the message
	// arrives on a socket that was already there.
	marks := map[string]int64{}
	live := map[string]*messageFeed{} // by case word and participant
	var feeds []*messageFeed
	for _, c := range cases {
		if marks[c.channel], err = s.lastID(c.channel); err != nil {
			return err
		}
		for _, p := range c.insiders {
			f, err := s.followMessages(p, c.channel, marks[c.channel])
			if err != nil {
				return fmt.Errorf("%s: the live socket of %s, an insider: %w", c.what, p, err)
			}
			live[c.word+"/"+p] = f
			feeds = append(feeds, f)
		}
		for _, p := range c.outside {
			f, err := s.followMessages(p, c.channel, marks[c.channel])
			if c.hidden {
				// An outsider cannot even open the socket, and cannot tell
				// the channel exists.
				if !channelNotFound(err) {
					return fmt.Errorf("%s: the live socket of %s, an outsider: %s, want 404 channel_not_found", c.what, p, describe(err))
				}
				if _, err := s.followEvents(p, c.channel); !channelNotFound(err) {
					return fmt.Errorf("%s: the events socket of %s, an outsider: %s, want 404 channel_not_found", c.what, p, describe(err))
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("%s: the live socket of %s, an outsider: %w", c.what, p, err)
			}
			live[c.word+"/"+p] = f
			feeds = append(feeds, f)
		}
	}
	defer func() {
		for _, f := range feeds {
			f.close()
		}
	}()
	nickEvents, err := s.followEvents(partNick, chGeneral)
	if err != nil {
		return err
	}
	guestEvents, err := s.followEvents(partGuest, chGeneral)
	if err != nil {
		return err
	}

	// nick-test posts each, with the client a person uses.
	ids := map[string]int64{}
	for _, c := range cases {
		if err := s.say(c.channel, c.word, c.flags...); err != nil {
			return err
		}
	}
	for _, c := range cases {
		if c.control == "" {
			continue
		}
		if err := s.say(c.channel, c.control); err != nil {
			return err
		}
	}
	controls := map[string]int64{}
	for _, c := range cases {
		m, err := s.waitMessage(partNick, c.channel, c.word)
		if err != nil {
			return err
		}
		if c.flags != nil && m.Audience == nil {
			return fmt.Errorf("%s: nick-test reads its own message without an audience", c.what)
		}
		ids[c.word] = m.ID
		if c.control != "" {
			cm, err := s.waitMessage(partNick, c.channel, c.control)
			if err != nil {
				return err
			}
			controls[c.word] = cm.ID
		}
	}

	for _, c := range cases {
		want := ids[c.word]
		for _, p := range c.insiders {
			if err := s.seesScoped(p, c, want, live[c.word+"/"+p], marks[c.channel]); err != nil {
				return fmt.Errorf("%s, for %s, an insider: %w", c.what, p, err)
			}
		}
		for _, p := range c.outside {
			if err := s.blindToScoped(p, c, want, controls[c.word], live[c.word+"/"+p], marks[c.channel]); err != nil {
				return fmt.Errorf("%s, for %s, an outsider: %w", c.what, p, err)
			}
		}
	}

	// The run that answers the whisper.
	run, err := s.runAnswering(chGeneral, ids["p305whisper"])
	if err != nil {
		return err
	}
	if err := s.runHiddenFromOutsiders(run, ids["p305whisper"], nickEvents, guestEvents); err != nil {
		return fmt.Errorf("the run answering the whisper: %w", err)
	}
	// Stopping the agent cancels what it was doing.
	return s.stopAgent(s.worker)
}

// seesScoped checks that an insider has the message in its history, on its
// live socket and in a search.
func (s *suite) seesScoped(part string, c scopeCase, want int64, f *messageFeed, mark int64) error {
	got, err := s.history(part, c.channel, mark)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(got, func(m schema.MessageV2) bool { return m.ID == want }) {
		return fmt.Errorf("its history of %s does not have message %d", c.channel, want)
	}
	if err := s.until(part+"'s live socket to carry message "+id(want), 20*time.Second, func() (bool, error) { return f.has(want), nil }); err != nil {
		return err
	}
	found, err := s.search(part, c.channel, c.word)
	if err != nil {
		return err
	}
	if !slices.Contains(found, want) {
		return fmt.Errorf("a search of %s for %q does not find message %d", c.channel, c.word, want)
	}
	return nil
}

// search is the ids of the messages part's search of channel for word finds.
func (s *suite) search(part, channel, word string) ([]int64, error) {
	c, err := s.r.client(s.token(part))
	if err != nil {
		return nil, err
	}
	resp, err := c.SearchMessagesV2(runCtx, channel, word, 0, 50)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, m := range resp.Messages {
		out = append(out, m.ID)
	}
	return out, nil
}

// blindToScoped checks that an outsider gets channel_not_found or nothing from
// history, search, a live socket and a socket that replays from before the
// message. control is the id of a channel-wide message sent after it: an
// outsider's sockets must have it, which shows they work.
func (s *suite) blindToScoped(part string, c scopeCase, want, control int64, f *messageFeed, mark int64) error {
	notFound := func(what, method, path string) error {
		return s.as(part).refused(what, method, path, nil, http.StatusNotFound, "channel_not_found")
	}
	path := "/v2/channels/" + c.channel + "/messages"
	if c.hidden {
		if err := notFound("history", http.MethodGet, path); err != nil {
			return err
		}
		if err := notFound("search", http.MethodGet, path+"/search?q="+c.word); err != nil {
			return err
		}
		if err := notFound("the roster", http.MethodGet, "/v1/sessions?channel="+c.channel); err != nil {
			return err
		}
		return nil
	}
	got, err := s.history(part, c.channel, 0)
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	if slices.ContainsFunc(got, func(m schema.MessageV2) bool { return m.ID == want || strings.Contains(m.Body, c.word) }) {
		return fmt.Errorf("its history of %s has message %d", c.channel, want)
	}
	if !slices.ContainsFunc(got, func(m schema.MessageV2) bool { return m.ID == control }) {
		return fmt.Errorf("its history of %s lacks the channel-wide message %d that followed", c.channel, control)
	}
	found, err := s.search(part, c.channel, c.word)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	if len(found) != 0 {
		return fmt.Errorf("a search of %s for %q finds %v", c.channel, c.word, found)
	}
	// Live: the control message is on its socket and the scoped one is not.
	if err := s.until(part+"'s live socket to carry the channel-wide message", 20*time.Second, func() (bool, error) { return f.has(control), nil }); err != nil {
		return err
	}
	if f.has(want) {
		return fmt.Errorf("its live socket carried message %d", want)
	}
	// A socket opened now, replaying from before the message, must not carry
	// it either.
	replay, err := s.followMessages(part, c.channel, mark)
	if err != nil {
		return fmt.Errorf("a replaying socket: %w", err)
	}
	defer replay.close()
	if err := s.until(part+"'s replaying socket to carry the channel-wide message", 20*time.Second, func() (bool, error) { return replay.has(control), nil }); err != nil {
		return err
	}
	if replay.has(want) {
		return fmt.Errorf("its replaying socket carried message %d", want)
	}
	return nil
}

// runHiddenFromOutsiders checks that the run answering the whisper is there
// for the whisper's two ends and is not for anyone else: not by id, not in the
// run list, not in the activity of the run, not on the roster's task line and
// not on the events socket.
func (s *suite) runHiddenFromOutsiders(run schema.RunV1, whisper int64, nickEvents, guestEvents *eventFeed) error {
	if run.MessageID != whisper {
		return fmt.Errorf("run %d answers message %d, want %d", run.ID, run.MessageID, whisper)
	}
	runPath := "/v1/runs/" + id(run.ID)

	// The insider sees all of it.
	var worker schema.SessionV1
	if err := s.until("nick-test to see the run on worker-test's roster line", 20*time.Second, func() (bool, error) {
		roster, err := s.roster(partNick, chGeneral)
		if err != nil {
			return false, err
		}
		for _, sess := range sessionsNamed(roster, "worker-test") {
			if sess.CurrentRun == run.ID {
				worker = sess
			}
		}
		return worker.ID != 0, nil
	}); err != nil {
		return err
	}
	if !strings.Contains(worker.Task, "p305whisper") {
		return fmt.Errorf("nick-test's roster line for worker-test has the task %q, want the whisper's words", worker.Task)
	}
	var acts schema.ListActivityResponseV1
	if err := s.until("the run to have activity", 20*time.Second, func() (bool, error) {
		if err := s.as(partNick).call(http.MethodGet, runPath+"/activity", nil, &acts); err != nil {
			return false, err
		}
		return len(acts.Activity) > 0, nil
	}); err != nil {
		return err
	}
	if err := s.until("nick-test's events socket to carry the run and its activity", 20*time.Second, func() (bool, error) {
		return slices.ContainsFunc(nickEvents.runsSeen(), func(r schema.RunV1) bool { return r.ID == run.ID }) &&
			slices.ContainsFunc(nickEvents.activitySeen(), func(a schema.AgentActivityV1) bool { return a.RunID == run.ID }), nil
	}); err != nil {
		return err
	}

	// The outsiders see none of it.
	for _, part := range []string{partGuest, partBrowser, partObserver} {
		as := s.as(part)
		if err := as.refused(part+" reads the run", http.MethodGet, runPath, nil, http.StatusNotFound, schema.ErrorCodeRunNotFound); err != nil {
			return err
		}
		if err := as.refused(part+" reads the run's activity", http.MethodGet, runPath+"/activity", nil, http.StatusNotFound, schema.ErrorCodeRunNotFound); err != nil {
			return err
		}
		if err := as.refused(part+" reads the run's interventions", http.MethodGet, runPath+"/interventions", nil, http.StatusNotFound, schema.ErrorCodeRunNotFound); err != nil {
			return err
		}
		runs, err := s.runsOf(part, chGeneral)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(runs, func(r schema.RunV1) bool { return r.ID == run.ID }) {
			return fmt.Errorf("the run list of %s has run %d", part, run.ID)
		}
		roster, err := s.roster(part, chGeneral)
		if err != nil {
			return err
		}
		for _, sess := range sessionsNamed(roster, "worker-test") {
			if sess.CurrentRun != 0 || sess.Task != "" {
				return fmt.Errorf("the roster line of worker-test for %s has run %d and task %q", part, sess.CurrentRun, sess.Task)
			}
		}
	}

	// jamie-test's events socket is working (it has seen the worker busy) and
	// has not carried the run or anything about it.
	if err := s.until("jamie-test's events socket to show worker-test running", 20*time.Second, func() (bool, error) {
		return guestEvents.anyRoster(func(r schema.SessionRosterV1) bool {
			return slices.ContainsFunc(r.Sessions, func(sess schema.SessionV1) bool {
				return sess.Name == "worker-test" && sess.Status == schema.SessionStatusRunning
			})
		}), nil
	}); err != nil {
		return err
	}
	if slices.ContainsFunc(guestEvents.runsSeen(), func(r schema.RunV1) bool { return r.ID == run.ID }) {
		return fmt.Errorf("jamie-test's events socket carried run %d", run.ID)
	}
	if slices.ContainsFunc(guestEvents.activitySeen(), func(a schema.AgentActivityV1) bool { return a.RunID == run.ID }) {
		return fmt.Errorf("jamie-test's events socket carried activity of run %d", run.ID)
	}
	if guestEvents.anyRoster(func(r schema.SessionRosterV1) bool {
		return slices.ContainsFunc(r.Sessions, func(sess schema.SessionV1) bool {
			return sess.CurrentRun == run.ID || strings.Contains(sess.Task, "p305whisper")
		})
	}) {
		return fmt.Errorf("jamie-test's events socket carried the run on a roster line")
	}
	return nil
}
