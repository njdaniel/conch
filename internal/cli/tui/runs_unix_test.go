//go:build unix

package tui

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// The Runs view and a run from it, on a screen: r in the Agents view lists the
// channel's runs, a run frame adds a line, enter opens a run, esc comes back.
func TestRunsViewOnAScreen(t *testing.T) {
	worker := sess(1, 7, schema.PrincipalAgent, "worker", schema.SessionStatusRunning)
	worker.CurrentRun, worker.Task = 41, "fix the parser"
	worker.Interventions = bothKinds
	worker.LastSeenAt = schema.NewTimestamp(time.Now())
	old := testRun(40, schema.RunStateCompleted)
	old.Task = "earlier job"
	old.StartedAt = schema.NewTimestamp(time.Now().Add(-time.Hour))
	end := schema.NewTimestamp(time.Now().Add(-50 * time.Minute))
	old.EndedAt = &end
	run := testRun(41, schema.RunStateRunning)
	run.StartedAt = schema.NewTimestamp(time.Now().Add(-time.Minute))
	api := &answeringAPI{runAPI: newRunAPI(run), roster: rosterOf(worker), handlers: make(chan cli.EventHandler, 4)}
	api.runs = []schema.RunV1{old, run}
	api.activity = []schema.AgentActivityV1{activityAt(1, 41, schema.ActivityKindRunStarted, "started the job")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keys, keyboard := io.Pipe()
	defer func() { _ = keyboard.Close() }()
	out := &screen{}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, api, 0, false, []string{"general"}, false, keys, out) }()

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
	expect("r runs")
	send("r")
	expect("fix the parser")
	expect("earlier job")
	expect("runs: general")
	expect("f filter")

	// A run frame adds a line within a render.
	h := <-api.handlers
	api.handlers <- h
	fresh := testRun(42, schema.RunStateQueued)
	fresh.Task = "new work"
	fresh.StartedAt = schema.NewTimestamp(time.Now())
	if err := h.Run(fresh); err != nil {
		t.Fatal(err)
	}
	expect("new work")

	// f: open runs only.
	send("f")
	expect("runs: general · open (2)")

	// enter opens the selected run (the newest, 42), esc comes back.
	send("\r")
	expect("run #42 · queued")
	expect("g message")
	send("\x1b")
	expect("runs: general · open (2)")
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
