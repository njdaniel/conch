//go:build unix

package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// answeringAPI is a fake server with one agent in a running run. Its events
// socket sends the roster and then whatever the test pushes, and a
// cancellation it is asked for is acknowledged and then applied by the "agent",
// as frames: the run itself changes only when the test pushes its frame.
type answeringAPI struct {
	*runAPI
	roster   schema.SessionRosterV1
	handlers chan cli.EventHandler
}

func (a *answeringAPI) ListChannelMembers(context.Context, string) (schema.ListChannelMembersResponseV1, error) {
	return schema.ListChannelMembersResponseV1{Members: []schema.ChannelMemberV1{
		{PrincipalID: 5, Name: "alice", Kind: schema.PrincipalHuman},
		{PrincipalID: 7, Name: "worker", Kind: schema.PrincipalAgent},
	}}, nil
}

func (a *answeringAPI) SubscribeEvents(ctx context.Context, _ string, _ int64, h cli.EventHandler) error {
	a.handlers <- h
	if err := h.Roster(a.roster); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (a *answeringAPI) CreateIntervention(ctx context.Context, run int64, req schema.CreateInterventionRequestV1) (schema.InterventionV1, error) {
	iv, err := a.runAPI.CreateIntervention(ctx, run, req)
	if err != nil {
		return iv, err
	}
	h := <-a.handlers
	a.handlers <- h
	go func() {
		for _, state := range []schema.InterventionState{schema.InterventionStateAcknowledged, schema.InterventionStateApplied} {
			time.Sleep(150 * time.Millisecond)
			answered := intervention(iv.ID, run, req.Kind, req.Body, state, "")
			_ = h.Intervention(answered)
		}
	}()
	return iv, nil
}

// The whole path on a screen: open the Agents view, a run, ask for it to be
// cancelled, and watch the request be acknowledged and applied. Through all of
// it the run reads running; it reads cancelled only once its own frame says so.
func TestRunDetailCancelOnAScreen(t *testing.T) {
	worker := sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)
	worker.CurrentRun, worker.Task = 41, "fix the parser"
	worker.Interventions = bothKinds
	worker.LastSeenAt = schema.NewTimestamp(time.Now())
	run := testRun(41, schema.RunStateRunning)
	run.StartedAt = schema.NewTimestamp(time.Now().Add(-time.Minute))
	api := &answeringAPI{runAPI: newRunAPI(run), roster: rosterOf(worker), handlers: make(chan cli.EventHandler, 4)}
	api.activity = []schema.AgentActivityV1{activityAt(1, 41, schema.ActivityKindRunStarted, "started the job")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keys, keyboard := io.Pipe()
	defer func() { _ = keyboard.Close() }()
	out := &screen{}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, api, 0, false, []string{"general"}, false, nil, keys, out) }()

	send := func(k string) {
		t.Helper()
		if _, err := keyboard.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	expect := func(what string) {
		t.Helper()
		if !out.waitFor(what, 5*time.Second) {
			t.Fatalf("the screen never showed %q; output %q", what, out.String())
		}
	}
	if !out.waitFor("online: ", 10*time.Second) {
		t.Fatalf("the TUI never drew the members line; output %q", out.String())
	}
	send("\t") // inbox
	send("\t") // agents
	expect("[agent] worker")
	send("\r") // unfold
	expect("#1 running")
	send("\x1b[B") // the session
	send("\r")     // its run
	expect("run #41 · running")
	expect("started the job")
	expect("x cancel")

	send("x")
	expect("cancel this run? y/n")
	send("y")
	expect("requested…")
	expect("acknowledged")
	expect("applied")
	if strings.Contains(out.String(), "cancelled") {
		t.Fatalf("the screen says cancelled before the run's frame; output %q", out.String())
	}

	// The agent reports the run cancelled.
	h := <-api.handlers
	api.handlers <- h
	cancelled := testRun(41, schema.RunStateCancelled)
	if err := h.Run(cancelled); err != nil {
		t.Fatal(err)
	}
	expect("run #41 · cancelled")
	expect("the run has ended")

	send("\x1b") // back to the agents
	expect("agents: general")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run had not returned")
	}
}
