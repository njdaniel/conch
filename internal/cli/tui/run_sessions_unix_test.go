//go:build unix

package tui

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// rosterAPI is a fake server with one agent, whose events socket sends one
// roster and then stays open. It needs no network.
type rosterAPI struct {
	*sessionAPI
	roster schema.SessionRosterV1
}

func (a rosterAPI) ListChannelMembers(context.Context, string) (schema.ListChannelMembersResponseV1, error) {
	return schema.ListChannelMembersResponseV1{Members: []schema.ChannelMemberV1{
		{PrincipalID: 5, Name: "alice", Kind: schema.PrincipalHuman},
		{PrincipalID: 7, Name: "worker-test", Kind: schema.PrincipalAgent},
		{PrincipalID: 8, Name: "observer-test", Kind: schema.PrincipalAgent},
	}}, nil
}

func (a rosterAPI) SubscribeEvents(ctx context.Context, channel string, session int64, handler cli.EventHandler) error {
	a.mu.Lock()
	a.events = append(a.events, eventsCall{channel: channel, session: session, ctx: ctx})
	a.mu.Unlock()
	if err := handler.Roster(a.roster); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (a *sessionAPI) endedIDs() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int64(nil), a.ended...)
}

func awaitEnded(t *testing.T, a *sessionAPI) []int64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ended := a.endedIDs(); len(ended) > 0 {
			return ended
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

// The Agents view, opened with the keys against a fake server, shows a roster
// line on the screen: the agent's name, its status and its task.
func TestRunAgentsViewShowsRoster(t *testing.T) {
	worker := sess(1, 7, schema.PrincipalAgent, "worker-test", schema.SessionStatusRunning)
	worker.Runtime, worker.Model = "claude-code", "opus"
	worker.CurrentRun, worker.Task = 41, "fix the parser"
	worker.LastSeenAt = schema.NewTimestamp(time.Now())
	api := rosterAPI{sessionAPI: newSessionAPI(), roster: rosterOf(
		sess(2, 5, schema.PrincipalHuman, "alice", schema.SessionStatusOnline), worker)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keys, keyboard := io.Pipe()
	defer func() { _ = keyboard.Close() }()
	out := &screen{}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, api, 0, false, []string{"general"}, false, keys, out) }()

	// The members line, in the channels view.
	if !out.waitFor("online: alice · agents: worker-test ●", 10*time.Second) {
		t.Fatalf("no members line on the screen; output %q", out.String())
	}
	// tab, tab: inbox, then agents.
	for _, k := range []string{"\t", "\t"} {
		if _, err := keyboard.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{"[agent] worker-test", "running · task: fix the parser", "[agent] observer-test", "offline", "agents: general"} {
		if !out.waitFor(want, 5*time.Second) {
			t.Fatalf("the Agents view never showed %q; output %q", want, out.String())
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.created) != 1 || api.created[0].Client != "tui" {
		t.Errorf("sessions created = %+v, want one with client tui", api.created)
	}
}

// The TUI's own session ends with the program, however the program ends: a
// key, or the context ending on SIGINT or SIGTERM (the caller turns those into
// the end of ctx; tui.Run says why). The end call is made after the context
// has ended, so it needs a context of its own.
func TestRunEndsItsSession(t *testing.T) {
	tests := []struct {
		name string
		stop func(t *testing.T, keyboard io.Writer, cancel context.CancelFunc)
	}{
		{"ctrl-c", func(t *testing.T, keyboard io.Writer, _ context.CancelFunc) {
			if _, err := keyboard.Write([]byte{3}); err != nil {
				t.Fatal(err)
			}
		}},
		{"esc", func(t *testing.T, keyboard io.Writer, _ context.CancelFunc) {
			if _, err := keyboard.Write([]byte{0x1b}); err != nil {
				t.Fatal(err)
			}
		}},
		{"SIGINT", signalStop(syscall.SIGINT)},
		{"SIGTERM", signalStop(syscall.SIGTERM)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newSessionAPI()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			keys, keyboard := io.Pipe()
			defer func() { _ = keyboard.Close() }()
			out := &screen{}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, api, 0, false, []string{"general"}, false, keys, out) }()
			if !out.waitFor("esc quit", 5*time.Second) {
				t.Fatalf("the TUI never drew its key hints; output %q", out.String())
			}
			// The session exists while the program runs.
			deadline := time.Now().Add(3 * time.Second)
			for len(api.eventsCalls()) == 0 && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			if len(api.eventsCalls()) == 0 {
				t.Fatal("the events socket never opened for the session")
			}
			if ended := api.endedIDs(); len(ended) != 0 {
				t.Fatalf("session ended while the program was running: %v", ended)
			}

			tt.stop(t, keyboard, cancel)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run = %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run had not returned")
			}
			ended := awaitEnded(t, api)
			if len(ended) != 1 || ended[0] != 9 {
				t.Fatalf("ended sessions = %v, want session 9 once", ended)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.endCtxErrs[0] != nil {
				t.Errorf("the end call's context was already %v", api.endCtxErrs[0])
			}
		})
	}
}

// signalStop delivers sig to this process, as a supervisor would, and ends the
// context the way cmd/conch does on that signal.
func signalStop(sig syscall.Signal) func(*testing.T, io.Writer, context.CancelFunc) {
	return func(t *testing.T, _ io.Writer, cancel context.CancelFunc) {
		delivered := make(chan os.Signal, 1)
		signal.Notify(delivered, sig)
		defer signal.Stop(delivered)
		if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
			t.Fatalf("signal this process: %v", err)
		}
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatalf("%v was never delivered", sig)
		}
		cancel()
	}
}
