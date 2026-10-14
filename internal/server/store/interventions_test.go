package store

import (
	"context"
	"database/sql"
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

// Interventions in the store (issue #287, decision D10). They are built on
// runFixture (runs_test.go): the agent agt, its credential agtCred (and a
// second one, agtCred2), and the five runs on the channel-wide, net and
// whisper contexts whose audiences the run rule is tested against. The
// fixture's own session declares no kinds, so the interventions here are
// asked of runs on a second session that declares message and cancel.

var interventionT0 = runT0.Add(time.Hour)

// interventionWorld is the run fixture with a session that declares message
// and cancel and one run of each kind of audience on it.
type interventionWorld struct {
	*runFixture
	sess2 Session
	// The runs are the fixture's five contexts again, on sess2.
	rWide, rWideMsg, rNet, rWhisper, rElse int64
}

func newInterventionWorld(t *testing.T) *interventionWorld {
	t.Helper()
	ctx := context.Background()
	w := &interventionWorld{runFixture: newRunFixture(t)}
	var err error
	w.sess2, err = w.s.CreateSession(ctx, principalActor(w.agt.ID), SessionParams{
		PrincipalID: w.agt.ID, CredentialID: w.agtCred, Client: "adapter",
		Interventions: []schema.InterventionKind{schema.InterventionKindMessage, schema.InterventionKindCancel},
	}, runT0)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(channelID, messageID int64, task string) int64 {
		r, err := w.s.CreateRun(ctx, principalActor(w.agt.ID), RunParams{
			SessionID: w.sess2.ID, CredentialID: w.agtCred, ChannelID: channelID, MessageID: messageID, Task: task,
		}, w.agtReader(), runT0)
		if err != nil {
			t.Fatalf("CreateRun(%s): %v", task, err)
		}
		return r.ID
	}
	w.rWide = mk(w.ch.ID, 0, "wide")
	w.rWideMsg = mk(w.ch.ID, w.wide(t, w.frank, "an open question"), "wide message")
	w.rNet = mk(w.ch.ID, w.netMsg, "net")
	w.rWhisper = mk(w.ch.ID, w.whisperMsg, "whisper")
	w.rElse = mk(w.other.ID, 0, "elsewhere")
	return w
}

func (w *interventionWorld) ask(runID int64, who Principal, kind schema.InterventionKind, body string) (Intervention, error) {
	return w.s.CreateIntervention(context.Background(), principalActor(who.ID),
		InterventionParams{RunID: runID, Kind: kind, Body: body}, Reader{PrincipalID: who.ID, Scoped: true}, interventionT0)
}

func (w *interventionWorld) mustAsk(t *testing.T, runID int64, who Principal, kind schema.InterventionKind) Intervention {
	t.Helper()
	body := ""
	if kind == schema.InterventionKindMessage {
		body = "please also run the linter"
	}
	iv, err := w.ask(runID, who, kind, body)
	if err != nil {
		t.Fatalf("ask %s of run %d as %s: %v", kind, runID, who.Name, err)
	}
	return iv
}

func (w *interventionWorld) ack(id int64, to schema.InterventionState, reason string) (Intervention, error) {
	return w.s.AckIntervention(context.Background(), principalActor(w.agt.ID), id, w.agtCred, w.agtReader(), to, reason, interventionT0.Add(time.Minute))
}

func (w *interventionWorld) auditOf(t *testing.T, action string) []AuditEvent {
	t.Helper()
	all, err := w.s.ListAuditEvents(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []AuditEvent
	for _, e := range all {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// An intervention is visible to exactly the readers who see its run, on every
// read: asking, the single read and the run's list. The sets are the run
// rule's (TestRunVisibleExactSets), reader by reader.
func TestInterventionsFollowTheirRunsVisibility(t *testing.T) {
	ctx := context.Background()
	w := newInterventionWorld(t)
	runs := map[string]int64{"wide": w.rWide, "wide message": w.rWideMsg, "net": w.rNet, "whisper": w.rWhisper, "elsewhere": w.rElse}
	humans := []struct {
		name string
		p    Principal
	}{{"alice", w.alice}, {"bob (net monitor)", w.bob}, {"dave", w.dave}, {"erin", w.erin}, {"frank", w.frank}, {"carol (in no channel)", w.carol}}

	// One intervention per run, asked by someone who may see it.
	asker := map[string]Principal{"wide": w.frank, "wide message": w.frank, "net": w.dave, "whisper": w.erin, "elsewhere": w.alice}
	ids := map[string]int64{}
	for name, run := range runs {
		ids[name] = w.mustAsk(t, run, asker[name], schema.InterventionKindCancel).ID
	}
	for _, h := range humans {
		reader := Reader{PrincipalID: h.p.ID, Scoped: true}
		for name, run := range runs {
			t.Run(h.name+"/"+name, func(t *testing.T) {
				_, runErr := w.s.RunVisible(ctx, run, reader)
				sees := runErr == nil

				iv, err := w.s.InterventionVisible(ctx, ids[name], reader)
				if sees != (err == nil) || (err != nil && !errors.Is(err, ErrInterventionNotFound)) {
					t.Errorf("InterventionVisible = %+v, %v; the reader sees the run: %v", iv, err, sees)
				}
				list, err := w.s.ListVisibleInterventions(ctx, run, reader, 0, 100)
				switch {
				case sees && (err != nil || !hasIntervention(list, ids[name])):
					t.Errorf("list of a visible run = %+v, %v; want it to hold intervention %d", list, err, ids[name])
				case !sees && (!errors.Is(err, ErrRunNotFound) || list != nil):
					t.Errorf("list of a hidden run = %+v, %v; want ErrRunNotFound", list, err)
				}
				// Asking is held to the same rule. The answer for a hidden
				// run is the answer for a missing one.
				_, err = w.s.CreateIntervention(ctx, principalActor(h.p.ID),
					InterventionParams{RunID: run, Kind: schema.InterventionKindCancel}, reader, interventionT0)
				if sees != (err == nil) || (err != nil && !errors.Is(err, ErrRunNotFound)) {
					t.Errorf("CreateIntervention = %v; the reader sees the run: %v", err, sees)
				}
			})
		}
	}

	// Disabled and removed readers lose sight of what they saw.
	if _, err := w.s.DisablePrincipal(ctx, "system", w.erin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.InterventionVisible(ctx, ids["whisper"], Reader{PrincipalID: w.erin.ID, Scoped: true}); !errors.Is(err, ErrInterventionNotFound) {
		t.Errorf("a disabled reader reads %v, want ErrInterventionNotFound", err)
	}
	if _, err := w.s.EnablePrincipal(ctx, "system", w.erin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.RemoveChannelMember(ctx, "system", w.ch.ID, w.erin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.InterventionVisible(ctx, ids["whisper"], Reader{PrincipalID: w.erin.ID, Scoped: true}); !errors.Is(err, ErrInterventionNotFound) {
		t.Errorf("a removed member reads %v, want ErrInterventionNotFound", err)
	}
	// A reader with no verified identity, and a v0/v1 reader of a whisper run.
	for name, reader := range map[string]Reader{"no reader": {}, "channel-wide only": ChannelWideOnly, "unknown": {PrincipalID: 9999, Scoped: true}} {
		if _, err := w.s.InterventionVisible(ctx, ids["wide"], reader); !errors.Is(err, ErrInterventionNotFound) {
			t.Errorf("%s reads %v, want ErrInterventionNotFound", name, err)
		}
	}
	if _, err := w.s.InterventionVisible(ctx, ids["whisper"], Reader{PrincipalID: w.alice.ID, Scoped: false}); !errors.Is(err, ErrInterventionNotFound) {
		t.Errorf("a v0/v1 reader reads the whisper run's intervention: %v", err)
	}
}

// The requests the agent sees are only those on its own runs, through its own
// credential, and only while it may see the run.
func TestSessionInterventionsAreTheCredentialsOwn(t *testing.T) {
	ctx := context.Background()
	w := newInterventionWorld(t)
	a := w.mustAsk(t, w.rWide, w.alice, schema.InterventionKindCancel)
	b := w.mustAsk(t, w.rWhisper, w.erin, schema.InterventionKindMessage)
	c := w.mustAsk(t, w.rWide, w.alice, schema.InterventionKindMessage)
	if _, err := w.ack(c.ID, schema.InterventionStateApplied, ""); err != nil {
		t.Fatal(err)
	}
	ids := func(list []Intervention) []int64 {
		out := []int64{}
		for _, iv := range list {
			out = append(out, iv.ID)
		}
		return out
	}
	list := func(cred int64, reader Reader, state schema.InterventionState, after int64, limit int) []int64 {
		t.Helper()
		got, err := w.s.ListSessionInterventions(ctx, w.sess2.ID, cred, reader, state, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		return ids(got)
	}
	tests := []struct {
		name string
		got  []int64
		want []int64
	}{
		{"every state", list(w.agtCred, w.agtReader(), "", 0, 100), []int64{a.ID, b.ID, c.ID}},
		{"requested", list(w.agtCred, w.agtReader(), schema.InterventionStateRequested, 0, 100), []int64{a.ID, b.ID}},
		{"applied", list(w.agtCred, w.agtReader(), schema.InterventionStateApplied, 0, 100), []int64{c.ID}},
		{"failed", list(w.agtCred, w.agtReader(), schema.InterventionStateFailed, 0, 100), []int64{}},
		{"after", list(w.agtCred, w.agtReader(), "", a.ID, 100), []int64{b.ID, c.ID}},
		{"limit", list(w.agtCred, w.agtReader(), "", 0, 2), []int64{a.ID, b.ID}},
		{"another credential of the same principal", list(w.agtCred2, w.agtReader(), "", 0, 100), []int64{}},
		{"a human reader", list(w.agtCred, Reader{PrincipalID: w.alice.ID, Scoped: true}, "", 0, 100), []int64{}},
		{"no reader", list(w.agtCred, Reader{}, "", 0, 100), []int64{}},
		{"no credential", list(0, w.agtReader(), "", 0, 100), []int64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(tt.got, tt.want) {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
	// The fixture's first session is another session: nothing of sess2's.
	if got, err := w.s.ListSessionInterventions(ctx, w.sess.ID, w.agtCred, w.agtReader(), "", 0, 100); err != nil || len(got) != 0 {
		t.Errorf("another session's list = %+v, %v; want none", got, err)
	}
	// Taken off the net, the agent no longer sees the run it started there,
	// and so is not handed what people say to it.
	n := w.mustAsk(t, w.rNet, w.dave, schema.InterventionKindCancel)
	if !containsInt64(list(w.agtCred, w.agtReader(), "", 0, 100), n.ID) {
		t.Fatal("the agent does not see an intervention on a run it may see")
	}
	if _, err := w.s.RemoveChannelMember(ctx, "system", w.ch.ID, w.agt.ID); err != nil {
		t.Fatal(err)
	}
	if got := list(w.agtCred, w.agtReader(), "", 0, 100); len(got) != 0 {
		t.Errorf("an agent that left the channel is handed %v", got)
	}
}

func hasIntervention(list []Intervention, id int64) bool {
	for _, iv := range list {
		if iv.ID == id {
			return true
		}
	}
	return false
}

func containsInt64(list []int64, id int64) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

// The refusals of a request, each leaving no intervention behind: an agent,
// a kind the session did not declare (audited, once), a run that has ended,
// and a run with sixteen waiting.
func TestCreateInterventionRefusals(t *testing.T) {
	ctx := context.Background()
	w := newInterventionWorld(t)
	finished := w.mustRunOn(t, w.sess2.ID, "finished")
	for _, to := range []schema.RunState{schema.RunStateRunning, schema.RunStateCompleted} {
		if _, err := w.s.SetRunState(ctx, "system", finished, w.agtCred, w.agtReader(), to, "done", interventionT0); err != nil {
			t.Fatal(err)
		}
	}
	// A run whose session has ended: nothing will ever poll for a request.
	ended, err := w.s.CreateSession(ctx, "system", SessionParams{PrincipalID: w.agt.ID, CredentialID: w.agtCred, Client: "x",
		Interventions: []schema.InterventionKind{schema.InterventionKindCancel}}, interventionT0)
	if err != nil {
		t.Fatal(err)
	}
	orphan := w.mustRunOn(t, ended.ID, "orphan")
	if err := w.s.EndSession(ctx, "system", ended.ID, w.agtCred, interventionT0); err != nil {
		t.Fatal(err)
	}
	full := w.mustRunOn(t, w.sess2.ID, "full")
	for i := 0; i < MaxRequestedInterventions; i++ {
		w.mustAsk(t, full, w.alice, schema.InterventionKindCancel)
	}
	tests := []struct {
		name  string
		run   int64
		who   Principal
		kind  schema.InterventionKind
		body  string
		want  error
		audit int // intervention_unsupported rows added
	}{
		{"an agent, on its own run", w.rWide, w.agt, schema.InterventionKindCancel, "", ErrNotHuman, 0},
		{"pause is not declared", w.rWide, w.alice, schema.InterventionKindPause, "", ErrInterventionUnsupported, 1},
		{"resume is not declared", w.rWide, w.alice, schema.InterventionKindResume, "", ErrInterventionUnsupported, 1},
		{"a session that declared nothing", w.runWide, w.alice, schema.InterventionKindCancel, "", ErrInterventionUnsupported, 1},
		{"an ended run", finished, w.alice, schema.InterventionKindCancel, "", ErrInvalidTransition, 0},
		{"an ended run, an undeclared kind", finished, w.alice, schema.InterventionKindPause, "", ErrInvalidTransition, 0},
		{"a full run", full, w.alice, schema.InterventionKindCancel, "", ErrTooManyInterventions, 0},
		{"a run whose session has ended", orphan, w.alice, schema.InterventionKindCancel, "", ErrInvalidTransition, 0},
		{"a run that is not there", 99999, w.alice, schema.InterventionKindCancel, "", ErrRunNotFound, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, auditBefore := w.count(t, "interventions"), len(w.auditOf(t, AuditInterventionUnsupported))
			requested := len(w.auditOf(t, AuditInterventionRequested))
			_, err := w.ask(tt.run, tt.who, tt.kind, tt.body)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if got := w.count(t, "interventions"); got != before {
				t.Errorf("a refused request left %d new rows", got-before)
			}
			if got := len(w.auditOf(t, AuditInterventionRequested)); got != requested {
				t.Errorf("a refused request wrote intervention_requested")
			}
			rows := w.auditOf(t, AuditInterventionUnsupported)
			if len(rows)-auditBefore != tt.audit {
				t.Fatalf("unsupported audit rows added = %d, want %d", len(rows)-auditBefore, tt.audit)
			}
			if tt.audit == 1 {
				last := rows[len(rows)-1]
				if last.Actor != principalActor(tt.who.ID) || !strings.Contains(last.Detail, fmt.Sprintf("kind=%s", tt.kind)) ||
					!strings.Contains(last.Detail, fmt.Sprintf("run=%d", tt.run)) || !strings.Contains(last.Detail, fmt.Sprintf("requested_by=%d", tt.who.ID)) {
					t.Errorf("audit row = %+v", last)
				}
			}
		})
	}
	// Answering one makes room for exactly one more.
	first, err := w.s.ListVisibleInterventions(ctx, full, w.agtReader(), 0, 1)
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	if _, err := w.s.AckIntervention(ctx, "system", first[0].ID, w.agtCred, w.agtReader(), schema.InterventionStateAcknowledged, "", interventionT0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ask(full, w.alice, schema.InterventionKindCancel, ""); err != nil {
		t.Errorf("after one was answered: %v", err)
	}
	if _, err := w.ask(full, w.alice, schema.InterventionKindCancel, ""); !errors.Is(err, ErrTooManyInterventions) {
		t.Errorf("past the bound again: %v", err)
	}
	// Another run is not affected by the bound of this one.
	if _, err := w.ask(w.rWide, w.alice, schema.InterventionKindCancel, ""); err != nil {
		t.Errorf("another run: %v", err)
	}
}

func (w *interventionWorld) mustRunOn(t *testing.T, sessionID int64, task string) int64 {
	t.Helper()
	r, err := w.s.CreateRun(context.Background(), principalActor(w.agt.ID), RunParams{
		SessionID: sessionID, CredentialID: w.agtCred, ChannelID: w.ch.ID, Task: task,
	}, w.agtReader(), runT0)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// The answer's moves, every pair, and the transaction's other rules: the
// owner only, the anchor rule of a run's own reports, answered_at never before
// the request, and a run that has ended still takes the adapter's answer.
func TestAckInterventionMoves(t *testing.T) {
	ctx := context.Background()
	states := []schema.InterventionState{
		schema.InterventionStateRequested, schema.InterventionStateAcknowledged,
		schema.InterventionStateApplied, schema.InterventionStateFailed,
	}
	// A legal way to each state from requested.
	via := map[schema.InterventionState][]schema.InterventionState{
		schema.InterventionStateRequested:    nil,
		schema.InterventionStateAcknowledged: {schema.InterventionStateAcknowledged},
		schema.InterventionStateApplied:      {schema.InterventionStateApplied},
		schema.InterventionStateFailed:       {schema.InterventionStateFailed},
	}
	allowed := map[[2]schema.InterventionState]bool{
		{schema.InterventionStateRequested, schema.InterventionStateAcknowledged}: true,
		{schema.InterventionStateRequested, schema.InterventionStateApplied}:      true,
		{schema.InterventionStateRequested, schema.InterventionStateFailed}:       true,
		{schema.InterventionStateAcknowledged, schema.InterventionStateApplied}:   true,
		{schema.InterventionStateAcknowledged, schema.InterventionStateFailed}:    true,
	}
	w := newInterventionWorld(t)
	for _, from := range states {
		for _, to := range states {
			t.Run(fmt.Sprintf("%s to %s", from, to), func(t *testing.T) {
				iv := w.mustAsk(t, w.rWide, w.alice, schema.InterventionKindCancel)
				for _, step := range via[from] {
					if _, err := w.ack(iv.ID, step, "reason "+string(step)); err != nil {
						t.Fatal(err)
					}
				}
				answered := len(w.auditOf(t, AuditInterventionAnswered))
				got, err := w.ack(iv.ID, to, "because")
				if !allowed[[2]schema.InterventionState{from, to}] {
					if !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("err = %v, want ErrInvalidTransition", err)
					}
					if n := len(w.auditOf(t, AuditInterventionAnswered)); n != answered {
						t.Error("a refused move was audited as an answer")
					}
					again, err := w.s.InterventionVisible(ctx, iv.ID, w.agtReader())
					if err != nil || again.State != from {
						t.Errorf("after the refused move: %+v, %v; want still %s", again, err, from)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.State != to || got.Reason != "because" || got.AnsweredAt == nil || got.AnsweredAt.Before(got.RequestedAt) {
					t.Errorf("answered = %+v", got)
				}
				if n := len(w.auditOf(t, AuditInterventionAnswered)); n != answered+1 {
					t.Errorf("audit rows for answers went %d to %d, want one more", answered, n)
				}
			})
		}
	}

	t.Run("the owner only", func(t *testing.T) {
		iv := w.mustAsk(t, w.rWide, w.alice, schema.InterventionKindCancel)
		humanReader := Reader{PrincipalID: w.alice.ID, Scoped: true}
		for name, call := range map[string]func() error{
			"another credential of the same principal": func() error {
				_, err := w.s.AckIntervention(ctx, "x", iv.ID, w.agtCred2, w.agtReader(), schema.InterventionStateApplied, "", interventionT0)
				return err
			},
			"a human, whatever the credential": func() error {
				_, err := w.s.AckIntervention(ctx, "x", iv.ID, w.agtCred, humanReader, schema.InterventionStateApplied, "", interventionT0)
				return err
			},
			"no credential": func() error {
				_, err := w.s.AckIntervention(ctx, "x", iv.ID, 0, w.agtReader(), schema.InterventionStateApplied, "", interventionT0)
				return err
			},
			"no reader": func() error {
				_, err := w.s.AckIntervention(ctx, "x", iv.ID, w.agtCred, Reader{}, schema.InterventionStateApplied, "", interventionT0)
				return err
			},
			"an intervention that is not there": func() error {
				_, err := w.s.AckIntervention(ctx, "x", 99999, w.agtCred, w.agtReader(), schema.InterventionStateApplied, "", interventionT0)
				return err
			},
		} {
			if err := call(); !errors.Is(err, ErrInterventionNotFound) {
				t.Errorf("%s: %v, want ErrInterventionNotFound", name, err)
			}
		}
		if got, err := w.s.InterventionVisible(ctx, iv.ID, humanReader); err != nil || got.State != schema.InterventionStateRequested {
			t.Errorf("after the refused answers: %+v, %v", got, err)
		}
	})

	t.Run("answered_at is never before requested_at", func(t *testing.T) {
		iv := w.mustAsk(t, w.rWide, w.alice, schema.InterventionKindCancel)
		got, err := w.s.AckIntervention(ctx, "x", iv.ID, w.agtCred, w.agtReader(), schema.InterventionStateApplied, "", interventionT0.Add(-time.Hour))
		if err != nil || got.AnsweredAt == nil || !got.AnsweredAt.Equal(iv.RequestedAt) {
			t.Errorf("answered = %+v, %v; want answered_at = requested_at %v", got, err, iv.RequestedAt)
		}
	})

	t.Run("a run that has ended still takes the answer, and is not changed", func(t *testing.T) {
		run := w.mustRunOn(t, w.sess2.ID, "ends")
		iv := w.mustAsk(t, run, w.alice, schema.InterventionKindCancel)
		if _, err := w.s.SetRunState(ctx, "system", run, w.agtCred, w.agtReader(), schema.RunStateCancelled, "stopped", interventionT0); err != nil {
			t.Fatal(err)
		}
		before, _ := w.s.RunVisible(ctx, run, w.agtReader())
		if _, err := w.ack(iv.ID, schema.InterventionStateApplied, ""); err != nil {
			t.Fatal(err)
		}
		after, _ := w.s.RunVisible(ctx, run, w.agtReader())
		if !reflect.DeepEqual(before, after) {
			t.Errorf("run changed by the answer: %+v -> %+v", before, after)
		}
	})
}

// An answer reaches the run's audience as a report does, so the anchor rule
// of a run's reports applies to it: an agent demoted to monitor on the net a
// run answers, taken off it, or whose net was archived, cannot answer for the
// run, and the intervention is left as it was.
func TestAckInterventionReappliesTheAnchorRule(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		change func(t *testing.T, w *interventionWorld)
	}{
		{"demoted to monitor", func(t *testing.T, w *interventionWorld) { w.put(t, "n1", w.agt, schema.NetRoleMonitor) }},
		{"taken off the net", func(t *testing.T, w *interventionWorld) {
			if _, err := w.s.RemoveNetMember(ctx, "system", w.ch.ID, "n1", w.agt.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"the net archived", func(t *testing.T, w *interventionWorld) {
			if _, err := w.s.ArchiveNet(ctx, "system", w.ch.ID, "n1"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newInterventionWorld(t)
			netAsk := w.mustAsk(t, w.rNet, w.dave, schema.InterventionKindCancel)
			whisperAsk := w.mustAsk(t, w.rWhisper, w.erin, schema.InterventionKindCancel)
			tt.change(t, w)
			audit := len(w.auditOf(t, AuditInterventionAnswered))
			if _, err := w.ack(netAsk.ID, schema.InterventionStateApplied, ""); !errors.Is(err, ErrInterventionNotFound) {
				t.Errorf("answer on the net's run = %v, want ErrInterventionNotFound", err)
			}
			if len(w.auditOf(t, AuditInterventionAnswered)) != audit {
				t.Error("a refused answer was audited")
			}
			// The run on a whisper is untouched by any of this.
			if _, err := w.ack(whisperAsk.ID, schema.InterventionStateApplied, ""); err != nil {
				t.Errorf("answer on the whisper's run = %v", err)
			}
		})
	}
}

// Two answers raced on one request: exactly one is recorded and audited, the
// other is told the move is not allowed (its transaction read the new state).
func TestConcurrentAnswersExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	w := newInterventionWorld(t)
	for round := 0; round < 20; round++ {
		iv := w.mustAsk(t, w.rWide, w.alice, schema.InterventionKindCancel)
		answered := len(w.auditOf(t, AuditInterventionAnswered))
		results := make([]error, 2)
		var wg sync.WaitGroup
		for i, to := range []schema.InterventionState{schema.InterventionStateApplied, schema.InterventionStateFailed} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, results[i] = w.s.AckIntervention(ctx, "x", iv.ID, w.agtCred, w.agtReader(), to, "because", interventionT0)
			}()
		}
		wg.Wait()
		wins := 0
		for _, err := range results {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, ErrInvalidTransition):
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d winners, want 1 (%v)", round, wins, results)
		}
		if got := len(w.auditOf(t, AuditInterventionAnswered)); got != answered+1 {
			t.Fatalf("round %d: %d answer rows added, want 1", round, got-answered)
		}
	}
}

func TestInterventionMoveAllowedIsClosed(t *testing.T) {
	// Nothing moves to requested, and an answered intervention is final.
	for _, from := range []schema.InterventionState{schema.InterventionStateRequested, schema.InterventionStateAcknowledged, schema.InterventionStateApplied, schema.InterventionStateFailed} {
		if InterventionMoveAllowed(from, schema.InterventionStateRequested) {
			t.Errorf("%s may move back to requested", from)
		}
	}
	for _, from := range []schema.InterventionState{schema.InterventionStateApplied, schema.InterventionStateFailed, "nonsense"} {
		for _, to := range []schema.InterventionState{schema.InterventionStateAcknowledged, schema.InterventionStateApplied, schema.InterventionStateFailed} {
			if InterventionMoveAllowed(from, to) {
				t.Errorf("%s may move to %s", from, to)
			}
		}
	}
}

// Migration 21 over a database at schema 20 that holds a run: the table and
// its index are added, nothing else is touched, a request can be made of the
// run afterwards, and the table refuses rows that go round the server.
func TestInterventionsMigrationFromSchema20(t *testing.T) {
	const pre = 20
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
		`INSERT INTO sessions (id, principal_id, credential_id, client, interventions, started_at, last_seen_at, state) VALUES (1, 2, 2, 'adapter', '["cancel"]', 7, 7, 'online')`,
		`INSERT INTO runs (id, session_id, principal_id, channel_id, message_id, state, task, started_at) VALUES (1, 1, 2, 1, 1, 'running', 'old work', 8)`,
		`PRAGMA user_version = 20`,
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
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 21 {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	var name string
	if err := s.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'interventions_by_run'").Scan(&name); err != nil {
		t.Errorf("index interventions_by_run missing: %v", err)
	}
	for _, c := range []struct {
		table string
		want  int
	}{{"principals", 2}, {"credentials", 2}, {"messages", 1}, {"sessions", 1}, {"runs", 1}, {"interventions", 0}} {
		var got int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + c.table).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s rows after migration = %d, %v; want %d", c.table, got, err, c.want)
		}
	}
	iv, err := s.CreateIntervention(ctx, "principal:1", InterventionParams{RunID: 1, Kind: schema.InterventionKindCancel},
		Reader{PrincipalID: 1, Scoped: true}, interventionT0)
	if err != nil || iv.State != schema.InterventionStateRequested || iv.ChannelID != 1 {
		t.Fatalf("request after migration = %+v, %v", iv, err)
	}
	got, err := s.AckIntervention(ctx, "principal:2", iv.ID, 2, Reader{PrincipalID: 2, Scoped: true}, schema.InterventionStateApplied, "", interventionT0)
	if err != nil || got.State != schema.InterventionStateApplied {
		t.Fatalf("answer after migration = %+v, %v", got, err)
	}
	// The run was not touched by the request or the answer.
	if run, err := s.RunVisible(ctx, 1, Reader{PrincipalID: 2, Scoped: true}); err != nil || run.State != schema.RunStateRunning {
		t.Errorf("run after migration = %+v, %v; want running", run, err)
	}
	// The table's own constraints hold against writes that go round the server.
	for _, stmt := range []string{
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at) VALUES (1, 'reboot', 'requested', 1, 9)`,
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at) VALUES (1, 'cancel', 'done', 1, 9)`,
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at) VALUES (99, 'cancel', 'requested', 1, 9)`,
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at) VALUES (1, 'cancel', 'requested', 99, 9)`,
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at, answered_at) VALUES (1, 'cancel', 'requested', 1, 9, 10)`,
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at) VALUES (1, 'cancel', 'applied', 1, 9)`,
		`INSERT INTO interventions (run_id, kind, body, state, requested_by, requested_at) VALUES (1, 'cancel', 'stop it', 'requested', 1, 9)`,
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at) VALUES (1, 'message', 'requested', 1, 9)`,
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at, reason) VALUES (1, 'cancel', 'requested', 1, 9, 'why')`,
		`INSERT INTO interventions (run_id, kind, state, requested_by, requested_at, answered_at) VALUES (1, 'cancel', 'failed', 1, 9, 10)`,
	} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Errorf("the interventions table accepted %q", stmt)
		}
	}
}
