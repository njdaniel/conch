package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

var runT0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// runFixture is the audience fixture (channel "general" with alice, bob, dave,
// erin and frank; carol outside; nets n1 = alice, bob (monitor), dave and
// n2 = alice, erin) plus an agent, agt, who is a member of general and of
// "other", sits on n1, and has a credential and a live session. Its runs:
//
//	runWide     general, answers nothing
//	runWideMsg  general, answers a channel-wide message
//	runNet      general, answers a message to n1
//	runWhisper  general, answers erin's whisper to agt
//	runElse     other, answers nothing (alice is the other member there)
type runFixture struct {
	*audienceFixture
	agt                                              Principal
	agtCred, agtCred2                                int64
	sess                                             Session
	runWide, runWideMsg, runNet, runWhisper, runElse int64
	whisperMsg, netMsg                               int64
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	ctx := context.Background()
	f := &runFixture{audienceFixture: newAudienceFixture(t)}
	var err error
	if f.agt, err = f.s.CreatePrincipal(ctx, PrincipalAgent, "agt"); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []int64{f.ch.ID, f.other.ID} {
		if _, err := f.s.AddChannelMember(ctx, "system", ch, f.agt.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.AddChannelMember(ctx, "system", f.other.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	f.put(t, "n1", f.agt, schema.NetRoleMember)
	for _, dst := range []*int64{&f.agtCred, &f.agtCred2} {
		c, _, err := f.s.CreateCredential(ctx, "system", f.agt.ID, "t", nil)
		if err != nil {
			t.Fatal(err)
		}
		*dst = c.ID
	}
	if f.sess, err = f.s.CreateSession(ctx, principalActor(f.agt.ID), SessionParams{PrincipalID: f.agt.ID, CredentialID: f.agtCred, Client: "adapter"}, runT0); err != nil {
		t.Fatal(err)
	}
	f.netMsg, _ = f.toNet(t, f.alice, f.n1, "net question")
	f.whisperMsg = f.whisper(t, f.erin, "secret question", f.agt)
	f.runWide = f.mustRun(t, f.ch.ID, 0, "wide")
	f.runWideMsg = f.mustRun(t, f.ch.ID, f.wide(t, f.frank, "open question"), "on a channel-wide message")
	f.runNet = f.mustRun(t, f.ch.ID, f.netMsg, "on the net")
	f.runWhisper = f.mustRun(t, f.ch.ID, f.whisperMsg, "answering the secret question")
	f.runElse = f.mustRun(t, f.other.ID, 0, "elsewhere")
	return f
}

// agtReader is the agent reading as itself on a protocol that can express an
// audience, which is what the server passes.
func (f *runFixture) agtReader() Reader { return Reader{PrincipalID: f.agt.ID, Scoped: true} }

func (f *runFixture) start(channelID, messageID int64, task string) (Run, error) {
	return f.s.CreateRun(context.Background(), principalActor(f.agt.ID), RunParams{
		SessionID: f.sess.ID, CredentialID: f.agtCred, ChannelID: channelID, MessageID: messageID, Task: task,
	}, f.agtReader(), runT0)
}

func (f *runFixture) mustRun(t *testing.T, channelID, messageID int64, task string) int64 {
	t.Helper()
	r, err := f.start(channelID, messageID, task)
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", task, err)
	}
	return r.ID
}

func (f *runFixture) mustActivity(t *testing.T, runID int64, summary string) int64 {
	t.Helper()
	a, err := f.s.AppendRunActivity(context.Background(), runID, f.agtCred, f.agtReader(),
		ActivityParams{Kind: schema.ActivityKindCommandStarted, Summary: summary}, runT0)
	if err != nil {
		t.Fatalf("AppendRunActivity(%d): %v", runID, err)
	}
	return a.ID
}

func (f *runFixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// visibleRunIDs is every run of channelID that r may see, by the listing.
func visibleRunIDs(t *testing.T, s *Store, channelID int64, r Reader) []int64 {
	t.Helper()
	runs, err := s.ListVisibleRuns(context.Background(), channelID, r, 0, 0, 100)
	if err != nil {
		t.Fatalf("ListVisibleRuns: %v", err)
	}
	ids := []int64{}
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

// assertRunSet checks that every read of a run gives r exactly want among
// all: the listing of the run's channel, the single read, the activity
// listing and the roster's current run.
func (f *runFixture) assertRunSet(t *testing.T, r Reader, want []int64) {
	t.Helper()
	ctx := context.Background()
	all := []int64{f.runWide, f.runWideMsg, f.runNet, f.runWhisper, f.runElse}
	in := map[int64]bool{}
	for _, id := range want {
		in[id] = true
	}
	var wantGeneral, wantOther []int64
	for _, id := range all {
		if !in[id] {
			continue
		}
		if id == f.runElse {
			wantOther = append(wantOther, id)
		} else {
			wantGeneral = append(wantGeneral, id)
		}
	}
	if wantGeneral == nil {
		wantGeneral = []int64{}
	}
	if wantOther == nil {
		wantOther = []int64{}
	}
	if got := visibleRunIDs(t, f.s, f.ch.ID, r); !reflect.DeepEqual(got, wantGeneral) {
		t.Errorf("listing general = %v, want %v", got, wantGeneral)
	}
	if got := visibleRunIDs(t, f.s, f.other.ID, r); !reflect.DeepEqual(got, wantOther) {
		t.Errorf("listing other = %v, want %v", got, wantOther)
	}
	for _, id := range all {
		_, err := f.s.RunVisible(ctx, id, r)
		if in[id] != (err == nil) || (err != nil && !errors.Is(err, ErrRunNotFound)) {
			t.Errorf("RunVisible(%d) = %v, want visible %v", id, err, in[id])
		}
		acts, err := f.s.ListVisibleActivity(ctx, id, r, 0, 100)
		switch {
		case in[id] && (err != nil || len(acts) != 1):
			t.Errorf("activity of visible run %d = %v, %v; want its one entry", id, acts, err)
		case !in[id] && (!errors.Is(err, ErrRunNotFound) || acts != nil):
			t.Errorf("activity of hidden run %d = %v, %v; want ErrRunNotFound", id, acts, err)
		}
	}
	// The roster shows the session's open run (the most recent, elsewhere)
	// only to a reader who may see it.
	views, err := f.s.SessionRunViews(ctx, []int64{f.sess.ID}, r)
	if err != nil {
		t.Fatal(err)
	}
	v := views[f.sess.ID]
	if v.OpenState != schema.RunStateQueued {
		t.Errorf("open state = %q, want queued for every reader", v.OpenState)
	}
	if in[f.runElse] != (v.CurrentRun == f.runElse && v.Task == "elsewhere") || (!in[f.runElse] && (v.CurrentRun != 0 || v.Task != "")) {
		t.Errorf("roster view = %+v; the reader may see the open run: %v", v, in[f.runElse])
	}
}

// The run rule (D8) gives each reader exactly these runs, on every read: a
// channel-wide run to the channel's members, a run on a net message to the
// net's recipients, a run on a whisper to the whisper's readers, and nothing
// to anyone outside the channel. Disabling a reader and removing them from
// the channel hide everything, including runs they saw before.
func TestRunVisibleExactSets(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	for _, id := range []int64{f.runWide, f.runWideMsg, f.runNet, f.runWhisper, f.runElse} {
		f.mustActivity(t, id, fmt.Sprintf("entry of %d", id))
	}
	tests := []struct {
		name   string
		reader Reader
		want   []int64
	}{
		{"the agent itself", f.agtReader(), []int64{f.runWide, f.runWideMsg, f.runNet, f.runWhisper, f.runElse}},
		{"net member, in other too", Reader{f.alice.ID, true}, []int64{f.runWide, f.runWideMsg, f.runNet, f.runElse}},
		{"net monitor", Reader{f.bob.ID, true}, []int64{f.runWide, f.runWideMsg, f.runNet}},
		{"net member", Reader{f.dave.ID, true}, []int64{f.runWide, f.runWideMsg, f.runNet}},
		{"whisper author", Reader{f.erin.ID, true}, []int64{f.runWide, f.runWideMsg, f.runWhisper}},
		{"member of general outside every audience", Reader{f.frank.ID, true}, []int64{f.runWide, f.runWideMsg}},
		{"not a member of any channel", Reader{f.carol.ID, true}, nil},
		{"no verified reader", Reader{0, true}, nil},
		{"channel-wide only", ChannelWideOnly, nil},
		{"unknown principal", Reader{9999, true}, nil},
		{"whisper author on a v0/v1 protocol", Reader{f.erin.ID, false}, []int64{f.runWide, f.runWideMsg}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { f.assertRunSet(t, tt.reader, tt.want) })
	}

	t.Run("disabled reader", func(t *testing.T) {
		if _, err := f.s.DisablePrincipal(ctx, "system", f.erin.ID); err != nil {
			t.Fatal(err)
		}
		f.assertRunSet(t, Reader{f.erin.ID, true}, nil)
		if _, err := f.s.EnablePrincipal(ctx, "system", f.erin.ID); err != nil {
			t.Fatal(err)
		}
		f.assertRunSet(t, Reader{f.erin.ID, true}, []int64{f.runWide, f.runWideMsg, f.runWhisper})
	})

	// As for scoped messages, removal hides everything, the whisper run erin
	// saw a moment ago included.
	t.Run("removed from the channel", func(t *testing.T) {
		if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.erin.ID); err != nil {
			t.Fatal(err)
		}
		f.assertRunSet(t, Reader{f.erin.ID, true}, nil)
		// alice leaves other: elsewhere goes, general stays.
		if _, err := f.s.RemoveChannelMember(ctx, "system", f.other.ID, f.alice.ID); err != nil {
			t.Fatal(err)
		}
		f.assertRunSet(t, Reader{f.alice.ID, true}, []int64{f.runWide, f.runWideMsg, f.runNet})
	})
}

// Paging steps over hidden runs without skipping visible ones, and session=
// narrows to one session.
func TestListVisibleRunsPaging(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	// frank sees wide and wideMsg of the fixture, then one in three of these.
	franks := []int64{f.runWide, f.runWideMsg}
	for i := 0; i < 5; i++ {
		franks = append(franks, f.mustRun(t, f.ch.ID, 0, fmt.Sprintf("visible %d", i)))
		f.mustRun(t, f.ch.ID, f.whisperMsg, "hidden")
		f.mustRun(t, f.ch.ID, f.netMsg, "hidden")
	}
	for _, limit := range []int{1, 2, 3, 100} {
		var got []int64
		after := int64(0)
		for pages := 0; ; pages++ {
			if pages > 100 {
				t.Fatalf("limit %d: paging did not terminate", limit)
			}
			page, err := f.s.ListVisibleRuns(ctx, f.ch.ID, Reader{f.frank.ID, true}, 0, after, limit+1)
			if err != nil {
				t.Fatal(err)
			}
			more := len(page) > limit
			if more {
				page = page[:limit]
			}
			for _, r := range page {
				got = append(got, r.ID)
				after = r.ID
			}
			if !more {
				break
			}
		}
		if !reflect.DeepEqual(got, franks) {
			t.Errorf("limit %d: paged %v, want %v", limit, got, franks)
		}
	}

	// A second session of the agent: session= narrows to it.
	other, err := f.s.CreateSession(ctx, principalActor(f.agt.ID), SessionParams{PrincipalID: f.agt.ID, CredentialID: f.agtCred, Client: "adapter"}, runT0)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.CreateRun(ctx, "system", RunParams{SessionID: other.ID, CredentialID: f.agtCred, ChannelID: f.ch.ID, Task: "second session"}, f.agtReader(), runT0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.ListVisibleRuns(ctx, f.ch.ID, Reader{f.frank.ID, true}, other.ID, 0, 100)
	if err != nil || len(got) != 1 || got[0].ID != r.ID {
		t.Errorf("session filter = %+v, %v; want only run %d", got, err, r.ID)
	}
}

// Every refusal of CreateRun stores nothing and audits nothing; the error
// says which kind of refusal it is, for the server to map.
func TestCreateRunRules(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	ended, err := f.s.CreateSession(ctx, "system", SessionParams{PrincipalID: f.agt.ID, CredentialID: f.agtCred, Client: "x"}, runT0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.EndSession(ctx, "system", ended.ID, f.agtCred, runT0); err != nil {
		t.Fatal(err)
	}
	// A message in other, and a whisper in general agt is not in.
	otherMsg, err := f.s.InsertMessageV1(ctx, f.other.ID, f.alice.ID, "elsewhere", nil)
	if err != nil {
		t.Fatal(err)
	}
	notForAgt := f.whisper(t, f.erin, "not for the agent", f.frank)
	n2Msg, _ := f.toNet(t, f.erin, f.n2, "not on the agent's net")
	third, err := f.s.CreateChannel(ctx, "third")
	if err != nil {
		t.Fatal(err)
	}
	// A net agt only monitors: it receives the message, and may not speak.
	watchNet := f.mustNet(t, "watch")
	f.put(t, "watch", f.erin, schema.NetRoleMember)
	f.put(t, "watch", f.agt, schema.NetRoleMonitor)
	monitored, rec := f.toNet(t, f.erin, watchNet, "agt only listens here")
	if !reflect.DeepEqual(rec, []int64{f.erin.ID, f.agt.ID}) && !reflect.DeepEqual(rec, []int64{f.agt.ID, f.erin.ID}) {
		t.Fatalf("the monitored message reached %v; agt must be a recipient for the case to mean anything", rec)
	}
	// A net agt was a member of, archived since.
	goneNet := f.mustNet(t, "gone")
	f.put(t, "gone", f.erin, schema.NetRoleMember)
	f.put(t, "gone", f.agt, schema.NetRoleMember)
	onArchived, _ := f.toNet(t, f.erin, goneNet, "said before the net was archived")
	if _, err := f.s.ArchiveNet(ctx, "system", f.ch.ID, "gone"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		session int64
		cred    int64
		channel int64
		message int64
		reader  Reader
		want    error
	}{
		{"a channel-wide run", f.sess.ID, f.agtCred, f.ch.ID, 0, f.agtReader(), nil},
		{"answering a visible whisper", f.sess.ID, f.agtCred, f.ch.ID, f.whisperMsg, f.agtReader(), nil},
		{"answering a message on a net the agent is a member of", f.sess.ID, f.agtCred, f.ch.ID, f.netMsg, f.agtReader(), nil},
		{"a message on a net the agent only monitors", f.sess.ID, f.agtCred, f.ch.ID, monitored, f.agtReader(), ErrRunAnchorNotFound},
		{"a message on a net archived since", f.sess.ID, f.agtCred, f.ch.ID, onArchived, f.agtReader(), ErrRunAnchorNotFound},
		{"another credential's session", f.sess.ID, f.agtCred2, f.ch.ID, 0, f.agtReader(), ErrNotFound},
		{"an ended session", ended.ID, f.agtCred, f.ch.ID, 0, f.agtReader(), ErrNotFound},
		{"an unknown session", 9999, f.agtCred, f.ch.ID, 0, f.agtReader(), ErrNotFound},
		{"a reader that is not the session's principal", f.sess.ID, f.agtCred, f.ch.ID, 0, Reader{f.alice.ID, true}, ErrNotFound},
		{"no verified reader", f.sess.ID, f.agtCred, f.ch.ID, 0, Reader{}, ErrNotFound},
		{"a channel the agent is not in", f.sess.ID, f.agtCred, third.ID, 0, f.agtReader(), ErrNotChannelMember},
		{"a message of another channel", f.sess.ID, f.agtCred, f.ch.ID, otherMsg.ID, f.agtReader(), ErrRunAnchorNotFound},
		{"a whisper the agent is not in", f.sess.ID, f.agtCred, f.ch.ID, notForAgt, f.agtReader(), ErrRunAnchorNotFound},
		{"a net message the agent is not on", f.sess.ID, f.agtCred, f.ch.ID, n2Msg, f.agtReader(), ErrRunAnchorNotFound},
		{"a message that does not exist", f.sess.ID, f.agtCred, f.ch.ID, 99999, f.agtReader(), ErrRunAnchorNotFound},
		{"a whisper read on a v0/v1 protocol", f.sess.ID, f.agtCred, f.ch.ID, f.whisperMsg, Reader{f.agt.ID, false}, ErrRunAnchorNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs, audit := f.count(t, "runs"), f.count(t, "audit_events")
			r, err := f.s.CreateRun(ctx, "principal:9", RunParams{SessionID: tt.session, CredentialID: tt.cred, ChannelID: tt.channel, MessageID: tt.message, Task: "t"}, tt.reader, runT0)
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				if r.ID != 0 || f.count(t, "runs") != runs || f.count(t, "audit_events") != audit {
					t.Errorf("a refused run wrote something: %+v", r)
				}
				return
			}
			if r.State != schema.RunStateQueued || r.EndedAt != nil || r.PrincipalID != f.agt.ID || r.MessageID != tt.message {
				t.Errorf("run = %+v", r)
			}
			if f.count(t, "runs") != runs+1 || f.count(t, "audit_events") != audit+1 {
				t.Errorf("a run wrote %d runs and %d audit rows, want 1 and 1", f.count(t, "runs")-runs, f.count(t, "audit_events")-audit)
			}
		})
	}
}

// pathTo is a legal way from queued to each state, so the transition table
// can be tried from every state.
var pathTo = map[schema.RunState][]schema.RunState{
	schema.RunStateQueued:    nil,
	schema.RunStateStarting:  {schema.RunStateStarting},
	schema.RunStateRunning:   {schema.RunStateRunning},
	schema.RunStateBlocked:   {schema.RunStateRunning, schema.RunStateBlocked},
	schema.RunStateWaiting:   {schema.RunStateRunning, schema.RunStateWaiting},
	schema.RunStateCompleted: {schema.RunStateRunning, schema.RunStateCompleted},
	schema.RunStateFailed:    {schema.RunStateFailed},
	schema.RunStateCancelled: {schema.RunStateCancelled},
}

var allRunStates = []schema.RunState{
	schema.RunStateQueued, schema.RunStateStarting, schema.RunStateRunning, schema.RunStateBlocked,
	schema.RunStateWaiting, schema.RunStateCompleted, schema.RunStateFailed, schema.RunStateCancelled,
}

// The whole transition table, from schema.RunTransitionAllowed: every legal
// change succeeds, and every illegal one is ErrInvalidTransition and changes
// nothing. Entering a terminal state sets ended_at and writes one run_ended
// naming the state; nothing else writes an audit row.
func TestSetRunStateTransitionTable(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	if len(pathTo) != len(allRunStates) {
		t.Fatalf("pathTo covers %d states, want %d", len(pathTo), len(allRunStates))
	}
	for _, from := range allRunStates {
		for _, to := range allRunStates {
			t.Run(fmt.Sprintf("%s to %s", from, to), func(t *testing.T) {
				id := f.mustRun(t, f.ch.ID, 0, "t")
				for _, step := range pathTo[from] {
					if _, err := f.s.SetRunState(ctx, "system", id, f.agtCred, f.agtReader(), step, "", runT0); err != nil {
						t.Fatalf("path to %s: %v", from, err)
					}
				}
				audit := f.count(t, "audit_events")
				later := runT0.Add(time.Minute)
				got, err := f.s.SetRunState(ctx, "principal:7", id, f.agtCred, f.agtReader(), to, "said so", later)
				after, rerr := f.s.RunVisible(ctx, id, f.agtReader())
				if rerr != nil {
					t.Fatal(rerr)
				}
				if !schema.RunTransitionAllowed(from, to) {
					if !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("err = %v, want ErrInvalidTransition", err)
					}
					if after.State != from || after.Outcome != "" || f.count(t, "audit_events") != audit {
						t.Errorf("an illegal change wrote: %+v", after)
					}
					return
				}
				if err != nil {
					t.Fatalf("legal change: %v", err)
				}
				if !reflect.DeepEqual(got, after) || after.State != to || after.Outcome != "said so" {
					t.Errorf("returned %+v, stored %+v", got, after)
				}
				terminal := schema.RunStateTerminal(to)
				if terminal != (after.EndedAt != nil) || (terminal && !after.EndedAt.Equal(later)) {
					t.Errorf("ended_at = %v, want set to %v exactly when terminal", after.EndedAt, later)
				}
				wantAudit := audit
				if terminal {
					wantAudit++
					events, _ := f.s.ListAuditEvents(ctx, 0, 100000)
					last := events[len(events)-1]
					if last.Action != AuditRunEnded || last.Subject != fmt.Sprintf("run:%d", id) || last.Actor != "principal:7" ||
						!strings.HasPrefix(last.Detail, "state="+string(to)+" ") || strings.Contains(last.Detail, "said so") {
						t.Errorf("run_ended = %+v", last)
					}
				}
				if f.count(t, "audit_events") != wantAudit {
					t.Errorf("audit rows %d -> %d, want %d", audit, f.count(t, "audit_events"), wantAudit)
				}
			})
		}
	}
}

// A report reaches the run's audience as starting the run did, so the rule a
// run is started under is applied again to every state change and activity
// entry, in their transactions. An agent that was a member of the net when it
// started a run there, and has since been demoted to monitor, taken off the
// net, or seen the net archived, gets ErrRunNotFound and changes nothing. Its
// run on a whisper, which none of that touches, stays reportable.
func TestReportsReapplyTheAnchorRule(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		change func(t *testing.T, f *runFixture)
	}{
		{"demoted to monitor", func(t *testing.T, f *runFixture) { f.put(t, "n1", f.agt, schema.NetRoleMonitor) }},
		{"taken off the net", func(t *testing.T, f *runFixture) {
			if _, err := f.s.RemoveNetMember(ctx, "system", f.ch.ID, "n1", f.agt.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"the net archived", func(t *testing.T, f *runFixture) {
			if _, err := f.s.ArchiveNet(ctx, "system", f.ch.ID, "n1"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRunFixture(t)
			// runNet was started as a member of n1, and is reportable then.
			if _, err := f.s.SetRunState(ctx, "system", f.runNet, f.agtCred, f.agtReader(), schema.RunStateRunning, "", runT0); err != nil {
				t.Fatalf("before the change: %v", err)
			}
			tt.change(t, f)
			runs, activity, audit := f.count(t, "runs"), f.count(t, "run_activity"), f.count(t, "audit_events")
			if _, err := f.s.SetRunState(ctx, "system", f.runNet, f.agtCred, f.agtReader(), schema.RunStateCompleted, "done", runT0); !errors.Is(err, ErrRunNotFound) {
				t.Errorf("SetRunState = %v, want ErrRunNotFound", err)
			}
			if _, err := f.s.AppendRunActivity(ctx, f.runNet, f.agtCred, f.agtReader(), ActivityParams{Kind: schema.ActivityKindReplyPosted, Summary: "still talking"}, runT0); !errors.Is(err, ErrRunNotFound) {
				t.Errorf("AppendRunActivity = %v, want ErrRunNotFound", err)
			}
			r, err := f.s.RunVisible(ctx, f.runNet, Reader{f.alice.ID, true})
			if err != nil || r.State != schema.RunStateRunning || r.Outcome != "" || r.EndedAt != nil {
				t.Errorf("run after the refused reports = %+v, %v; want running and untouched", r, err)
			}
			if f.count(t, "runs") != runs || f.count(t, "run_activity") != activity || f.count(t, "audit_events") != audit {
				t.Error("a refused report wrote something")
			}
			// The agent's run on a whisper is untouched by any of this.
			if _, err := f.s.SetRunState(ctx, "system", f.runWhisper, f.agtCred, f.agtReader(), schema.RunStateRunning, "", runT0); err != nil {
				t.Errorf("the whisper run = %v, want reportable", err)
			}
		})
	}
}

// Two changes raced on one running run, to two different terminal states:
// exactly one wins, the other is ErrInvalidTransition (the run had already
// ended when its transaction read it), the run carries the winner's state, and
// there is exactly one run_ended. Repeated so an interleaving that lets both
// through has many chances to show.
func TestConcurrentTerminalTransitionsExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	for round := 0; round < 20; round++ {
		id := f.mustRun(t, f.ch.ID, 0, "raced")
		if _, err := f.s.SetRunState(ctx, "system", id, f.agtCred, f.agtReader(), schema.RunStateRunning, "", runT0); err != nil {
			t.Fatal(err)
		}
		targets := []schema.RunState{schema.RunStateCompleted, schema.RunStateFailed}
		var wg sync.WaitGroup
		wins := make(chan schema.RunState, len(targets))
		errs := make(chan error, len(targets))
		start := make(chan struct{})
		for _, to := range targets {
			wg.Add(1)
			go func(to schema.RunState) {
				defer wg.Done()
				<-start
				if _, err := f.s.SetRunState(ctx, "system", id, f.agtCred, f.agtReader(), to, "", runT0); err != nil {
					errs <- err
					return
				}
				wins <- to
			}(to)
		}
		close(start)
		wg.Wait()
		close(wins)
		close(errs)

		var won []schema.RunState
		for to := range wins {
			won = append(won, to)
		}
		if len(won) != 1 {
			t.Fatalf("round %d: %d changes won (%v), want exactly 1", round, len(won), won)
		}
		for err := range errs {
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("round %d: the losing change = %v, want ErrInvalidTransition", round, err)
			}
		}
		r, err := f.s.RunVisible(ctx, id, f.agtReader())
		if err != nil || r.State != won[0] || r.EndedAt == nil {
			t.Errorf("round %d: run = %+v, %v; want %s and ended", round, r, err, won[0])
		}
		events, err := f.s.ListAuditEvents(ctx, 0, 100000)
		if err != nil {
			t.Fatal(err)
		}
		ended := 0
		for _, e := range events {
			if e.Action == AuditRunEnded && e.Subject == fmt.Sprintf("run:%d", id) {
				ended++
			}
		}
		if ended != 1 {
			t.Errorf("round %d: run_ended rows = %d, want 1", round, ended)
		}
	}
}

// Only the credential that owns the run's session reports on it, and it must
// still see the run; activity on an ended run is refused; no activity entry
// is audited; the data is kept as given.
func TestRunReportingRules(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	running := func() int64 {
		id := f.mustRun(t, f.ch.ID, f.whisperMsg, "t")
		if _, err := f.s.SetRunState(ctx, "system", id, f.agtCred, f.agtReader(), schema.RunStateRunning, "", runT0); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := running()
	audit := f.count(t, "audit_events")
	a, err := f.s.AppendRunActivity(ctx, id, f.agtCred, f.agtReader(), ActivityParams{
		Kind: schema.ActivityKindCommandFinished, Summary: "ran tests", Outcome: "exit 0", Data: json.RawMessage(`{"n":1}`),
	}, runT0)
	if err != nil || a.PrincipalID != f.agt.ID || string(a.Data) != `{"n":1}` {
		t.Fatalf("append = %+v, %v", a, err)
	}
	if f.count(t, "audit_events") != audit {
		t.Error("an activity entry wrote an audit row")
	}
	got, err := f.s.ListVisibleActivity(ctx, id, Reader{f.erin.ID, true}, 0, 10)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], a) {
		t.Errorf("activity read back = %+v, %v; want %+v", got, err, a)
	}

	tests := []struct {
		name   string
		cred   int64
		reader Reader
	}{
		{"another credential of the same agent", f.agtCred2, f.agtReader()},
		{"the owning credential with someone else's reader", f.agtCred, Reader{f.erin.ID, true}},
		{"no credential", 0, f.agtReader()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.s.SetRunState(ctx, "system", id, tt.cred, tt.reader, schema.RunStateCompleted, "", runT0); !errors.Is(err, ErrRunNotFound) {
				t.Errorf("SetRunState = %v, want ErrRunNotFound", err)
			}
			if _, err := f.s.AppendRunActivity(ctx, id, tt.cred, tt.reader, ActivityParams{Kind: schema.ActivityKindReplyPosted, Summary: "x"}, runT0); !errors.Is(err, ErrRunNotFound) {
				t.Errorf("AppendRunActivity = %v, want ErrRunNotFound", err)
			}
			if _, err := f.s.OwnedRun(ctx, id, tt.cred, tt.reader); !errors.Is(err, ErrRunNotFound) {
				t.Errorf("OwnedRun = %v, want ErrRunNotFound", err)
			}
		})
	}

	// The agent removed from the channel loses its runs there too.
	t.Run("the owner removed from the channel", func(t *testing.T) {
		g := newRunFixture(t)
		if _, err := g.s.RemoveChannelMember(ctx, "system", g.ch.ID, g.agt.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := g.s.SetRunState(ctx, "system", g.runWide, g.agtCred, g.agtReader(), schema.RunStateRunning, "", runT0); !errors.Is(err, ErrRunNotFound) {
			t.Errorf("SetRunState = %v, want ErrRunNotFound", err)
		}
	})

	for _, end := range []schema.RunState{schema.RunStateCompleted, schema.RunStateFailed, schema.RunStateCancelled} {
		t.Run("activity after "+string(end), func(t *testing.T) {
			id := running()
			if _, err := f.s.SetRunState(ctx, "system", id, f.agtCred, f.agtReader(), end, "", runT0); err != nil {
				t.Fatal(err)
			}
			n := f.count(t, "run_activity")
			if _, err := f.s.AppendRunActivity(ctx, id, f.agtCred, f.agtReader(), ActivityParams{Kind: schema.ActivityKindRunEnded, Summary: "x"}, runT0); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("err = %v, want ErrInvalidTransition", err)
			}
			if f.count(t, "run_activity") != n {
				t.Error("activity was stored on an ended run")
			}
		})
	}
}

// What the roster is told about a session's runs: the most recent open run's
// state, the most recent run's state, and the open run only when the reader
// may see it. Nothing changes a run when its session goes quiet.
func TestSessionRunViews(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	// A fresh session for the agent, so the fixture's runs do not count.
	sess, err := f.s.CreateSession(ctx, "system", SessionParams{PrincipalID: f.agt.ID, CredentialID: f.agtCred, Client: "x"}, runT0)
	if err != nil {
		t.Fatal(err)
	}
	view := func(r Reader) SessionRuns {
		t.Helper()
		v, err := f.s.SessionRunViews(ctx, []int64{sess.ID}, r)
		if err != nil {
			t.Fatal(err)
		}
		return v[sess.ID]
	}
	if v := view(f.agtReader()); v != (SessionRuns{}) {
		t.Errorf("no runs = %+v, want empty", v)
	}
	start := func(message int64) int64 {
		r, err := f.s.CreateRun(ctx, "system", RunParams{SessionID: sess.ID, CredentialID: f.agtCred, ChannelID: f.ch.ID, MessageID: message, Task: "the task"}, f.agtReader(), runT0)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	set := func(id int64, to schema.RunState) {
		if _, err := f.s.SetRunState(ctx, "system", id, f.agtCred, f.agtReader(), to, "", runT0); err != nil {
			t.Fatal(err)
		}
	}
	failed := start(0)
	set(failed, schema.RunStateFailed)
	if v := view(f.agtReader()); v != (SessionRuns{LastState: schema.RunStateFailed}) {
		t.Errorf("after a failed run = %+v", v)
	}
	secret := start(f.whisperMsg)
	set(secret, schema.RunStateRunning)
	set(secret, schema.RunStateBlocked)
	if v := view(Reader{f.erin.ID, true}); v != (SessionRuns{OpenState: schema.RunStateBlocked, LastState: schema.RunStateBlocked, CurrentRun: secret, Task: "the task"}) {
		t.Errorf("whisper reader = %+v", v)
	}
	// frank is in the channel and not in the whisper: the state, never the run.
	if v := view(Reader{f.frank.ID, true}); v != (SessionRuns{OpenState: schema.RunStateBlocked, LastState: schema.RunStateBlocked}) {
		t.Errorf("reader outside the whisper = %+v", v)
	}
	if v := view(Reader{}); v.CurrentRun != 0 || v.Task != "" || v.OpenState != schema.RunStateBlocked {
		t.Errorf("no reader = %+v", v)
	}

	// The session goes stale, then offline, then ends: the run is untouched.
	if _, err := f.s.SweepSessions(ctx, runT0.Add(40*time.Second), 30*time.Second, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SweepSessions(ctx, runT0.Add(2*time.Minute), 30*time.Second, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := f.s.EndSession(ctx, "system", sess.ID, f.agtCred, runT0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.RunVisible(ctx, secret, f.agtReader())
	if err != nil || r.State != schema.RunStateBlocked || r.EndedAt != nil {
		t.Errorf("run after its session went offline and ended = %+v, %v; want blocked and open", r, err)
	}
	// An ended session's run is still the owner's to close.
	set(secret, schema.RunStateRunning)
	set(secret, schema.RunStateCompleted)
	if v := view(f.agtReader()); v != (SessionRuns{LastState: schema.RunStateCompleted}) {
		t.Errorf("after completing = %+v", v)
	}
}

// Migrations 19 and 20 over a database at schema 18 that holds a session: the
// two tables and their indexes are added, nothing else is touched, and a run
// can be started on the session afterwards.
func TestRunsMigrationFromSchema18(t *testing.T) {
	const pre = 18
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pre; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'agent', 'bot', 'member', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, NULL, 4)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old', 5)`,
		`INSERT INTO credentials (id, principal_id, label, token_hash, created_at) VALUES (1, 1, 'a', 'h1', 6), (2, 2, 'b', 'h2', 6)`,
		`INSERT INTO sessions (id, principal_id, credential_id, client, started_at, last_seen_at, state) VALUES (1, 2, 2, 'adapter', 7, 7, 'online')`,
		`PRAGMA user_version = 18`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer func() { _ = s.Close() }()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 20 {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	for _, idx := range []string{"runs_by_channel", "runs_by_session", "run_activity_by_run"} {
		var name string
		if err := s.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?", idx).Scan(&name); err != nil {
			t.Errorf("index %s missing: %v", idx, err)
		}
	}
	for _, c := range []struct {
		table string
		want  int
	}{{"principals", 2}, {"credentials", 2}, {"messages", 1}, {"channel_members", 2}, {"sessions", 1}, {"runs", 0}, {"run_activity", 0}} {
		var got int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + c.table).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s rows after migration = %d, %v; want %d", c.table, got, err, c.want)
		}
	}
	reader := Reader{PrincipalID: 2, Scoped: true}
	r, err := s.CreateRun(ctx, "principal:2", RunParams{SessionID: 1, CredentialID: 2, ChannelID: 1, MessageID: 1, Task: "answer the old message"}, reader, runT0)
	if err != nil || r.State != schema.RunStateQueued {
		t.Fatalf("run after migration = %+v, %v", r, err)
	}
	if got, err := s.RunVisible(ctx, r.ID, Reader{PrincipalID: 1, Scoped: true}); err != nil || got.ID != r.ID {
		t.Errorf("the other member reads %+v, %v", got, err)
	}
	// The table refuses a terminal state without ended_at, and the reverse.
	for _, stmt := range []string{
		`INSERT INTO runs (session_id, principal_id, channel_id, state, task, started_at) VALUES (1, 2, 1, 'completed', 't', 8)`,
		`INSERT INTO runs (session_id, principal_id, channel_id, state, task, started_at, ended_at) VALUES (1, 2, 1, 'running', 't', 8, 9)`,
		`INSERT INTO runs (session_id, principal_id, channel_id, state, task, started_at) VALUES (1, 2, 1, 'paused', 't', 8)`,
	} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Errorf("the runs table accepted %q", stmt)
		}
	}
}
