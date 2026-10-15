package adapter

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// fakeOllama writes an `ollama` that runs script and puts its directory alone
// on PATH.
func fakeOllama(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ollama"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // a program the test must be able to run, in its own temp directory
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
}

func TestOllamaProfile(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantErr   string
		wantArgs  []string
		wantProbe string // the program probed; empty means no probe
	}{
		{"default", map[string]string{"CONCH_ADAPTER_RUNTIME": "ollama", "CONCH_ADAPTER_MODEL": "m1"}, "", []string{"ollama", "run", "--nowordwrap", "m1"}, "ollama"},
		{"OLLAMA_BIN", map[string]string{"CONCH_ADAPTER_RUNTIME": "ollama", "CONCH_ADAPTER_MODEL": "m1", "OLLAMA_BIN": "/x/ollama"}, "", []string{"/x/ollama", "run", "--nowordwrap", "m1"}, "/x/ollama"},
		{"OLLAMA_BIN relative", map[string]string{"CONCH_ADAPTER_RUNTIME": "ollama", "CONCH_ADAPTER_MODEL": "m1", "OLLAMA_BIN": "x/ollama"}, "OLLAMA_BIN", nil, ""},
		{"CLAUDE_BIN is not its", map[string]string{"CONCH_ADAPTER_RUNTIME": "ollama", "CONCH_ADAPTER_MODEL": "m1", "CLAUDE_BIN": "/x/claude"}, "", []string{"ollama", "run", "--nowordwrap", "m1"}, "ollama"},
		{"no model", map[string]string{"CONCH_ADAPTER_RUNTIME": "ollama"}, "CONCH_ADAPTER_MODEL", nil, ""},
		// A command someone configured is run as written and not probed.
		{"with a command", map[string]string{"CONCH_ADAPTER_RUNTIME": "ollama", "CONCH_ADAPTER_MODEL": "m1", "CONCH_ADAPTER_CMD": "/x/stub.sh", "OLLAMA_BIN": "/x/ollama"}, "", []string{"/x/stub.sh"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(withEnv(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Runtime != "ollama" || cfg.Model != "m1" || cfg.Reader != readerText || cfg.StreamsActivity || cfg.Fixture {
				t.Fatalf("config = runtime %q model %q reader %d stream %v fixture %v", cfg.Runtime, cfg.Model, cfg.Reader, cfg.StreamsActivity, cfg.Fixture)
			}
			if got := cfg.Command.argv("PROMPT", "m1"); !reflect.DeepEqual(got, tt.wantArgs) {
				t.Fatalf("argv = %q, want %q", got, tt.wantArgs)
			}
			if cfg.Command.usesPromptArgument() {
				t.Fatal("the prompt must go to standard input")
			}
			switch {
			case tt.wantProbe == "" && cfg.Probe != nil:
				t.Fatalf("probe = %+v, want none", *cfg.Probe)
			case tt.wantProbe != "" && (cfg.Probe == nil || cfg.Probe.bin != tt.wantProbe || cfg.Probe.model != "m1"):
				t.Fatalf("probe = %+v, want %s show m1", cfg.Probe, tt.wantProbe)
			}
		})
	}
}

func TestOllamaProbe(t *testing.T) {
	tests := []struct {
		name    string
		script  string // empty: no ollama on PATH
		wantErr string
	}{
		{"available", `[ "$1" = show ] && [ "$2" = m1 ] || exit 9`, ""},
		{"not on PATH", "", "ollama: not on PATH"},
		{"daemon down", `echo "Error: could not connect to ollama server, run 'ollama serve' to start it" >&2; exit 1`, "ollama: daemon not reachable"},
		{"model not pulled", `echo "Error: model 'm1' not found" >&2; exit 1`, "ollama: model m1 is not pulled; run: ollama pull m1"},
		{"silent failure", `exit 3`, "ollama: daemon not reachable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.script == "" {
				t.Setenv("PATH", t.TempDir())
			} else {
				fakeOllama(t, tt.script)
			}
			err := probeSpec{bin: "ollama", model: "m1"}.check(context.Background(), newFakeClock())
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatal(err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// A daemon that does not answer is given probeTimeout on the clock the loop
// has, then the probe stops it and says so.
func TestOllamaProbeTimesOut(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived")
	fakeOllama(t, "sleep 30; touch "+marker)
	clk := newFakeClock()
	errc := make(chan error, 1)
	go func() { errc <- probeSpec{bin: "ollama", model: "m1"}.check(context.Background(), clk) }()
	clk.waitFor(t, probeTimeout)
	select {
	case err := <-errc:
		t.Fatalf("probe ended before the timeout: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	clk.fire(probeTimeout)
	select {
	case err := <-errc:
		if err == nil || !strings.HasPrefix(err.Error(), "ollama: daemon not reachable") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("probe did not end after the timeout")
	}
}

// Each unavailable case stops the loop before a cursor is read, a session is
// registered or anything is posted.
func TestOllamaUnavailableStopsBeforeRegistering(t *testing.T) {
	fakeOllama(t, `echo "Error: model 'm1' not found" >&2; exit 1`)
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{0: {}}}
	cfg := testConfig()
	cfg.Probe = &probeSpec{bin: "ollama", model: "m1"}
	loop := &botLoop{cfg: cfg, mcp: mcp, claude: &fakeClaude{reply: "never"}}
	err := loop.run(context.Background())
	if err == nil || err.Error() != "ollama: model m1 is not pulled; run: ollama pull m1" {
		t.Fatalf("error = %v", err)
	}
	if len(mcp.registered) != 0 || len(mcp.posts) != 0 || len(mcp.reads) != 0 || len(mcp.eventLog()) != 0 {
		t.Fatalf("the adapter reached the server: registered %v, posts %v, reads %v, events %v", mcp.registered, mcp.posts, mcp.reads, mcp.eventLog())
	}
}

// The reply is what the model printed after the prompt on standard input, and
// the run reports the process as one started/finished pair.
func TestOllamaRunThroughTheLoop(t *testing.T) {
	mcp := &fakeMCP{}
	runner, _ := scriptRunner(t, `sh -c 'echo heard: $(cat)'`, false)
	loop := loopWithSession(mcp, runner, newFakeClock())
	if err := answer(t, loop, message(1, 2, "ping")); err != nil {
		t.Fatal(err)
	}
	if len(mcp.posts) != 1 || !strings.HasPrefix(mcp.posts[0], "heard: ") || !strings.Contains(mcp.posts[0], "ping") {
		t.Fatalf("posts = %q", mcp.posts)
	}
	want := []string{
		"start_run", "update_run:starting", "update_run:running",
		"activity:command_started", "activity:command_finished",
		"post_message", "activity:reply_posted", "update_run:completed:replied",
	}
	if got := mcp.eventLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// A model is one argument of the command and of the probe: one that a program
// would read as a flag, or that is not one word, is refused with one line, for
// every profile.
func TestModelIsRefusedUnlessOneWord(t *testing.T) {
	tests := []struct {
		name, runtime, model string
		ok                   bool
	}{
		{"help", "ollama", "--help", false},
		{"short flag", "ollama", "-v", false},
		{"space", "ollama", "a b", false},
		{"newline", "ollama", "a\nb", false},
		{"tab", "codex", "a\tb", false},
		{"flag for claude", "claude-code", "-p", false},
		{"flag for codex", "codex", "--oss", false},
		{"tag", "ollama", "llama3.1:8b", true},
		{"path-like", "codex", "org/model-1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(withEnv(map[string]string{"CONCH_ADAPTER_RUNTIME": tt.runtime, "CONCH_ADAPTER_MODEL": tt.model}))
			switch {
			case tt.ok && err != nil:
				t.Fatal(err)
			case !tt.ok && (err == nil || !strings.Contains(err.Error(), "CONCH_ADAPTER_MODEL") || strings.Contains(err.Error(), "\n")):
				t.Fatalf("error = %v, want one line naming CONCH_ADAPTER_MODEL", err)
			}
		})
	}
}

// A reply with terminal control sequences in it is posted without them, with
// its newlines and tabs, whatever the program that wrote it.
func TestOllamaReplyIsPostedWithoutControlSequences(t *testing.T) {
	fakeOllama(t, `cat >/dev/null; printf 'running (claude-sonne\033[13D\033[K\n(claude-sonnet-5-5).\n\ttabbed\033]0;title\007 end\n'`)
	cfg, err := loadConfig(withEnv(map[string]string{"CONCH_ADAPTER_RUNTIME": "ollama", "CONCH_ADAPTER_MODEL": "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	mcp := &fakeMCP{}
	runner := commandRunner{spec: cfg.Command, model: cfg.Model, timeout: 10 * time.Second, reader: cfg.Reader, sleep: sleepContext}
	loop := loopWithSession(mcp, runner, newFakeClock())
	if err := answer(t, loop, message(1, 2, "ping")); err != nil {
		t.Fatal(err)
	}
	want := "running (claude-sonne\n(claude-sonnet-5-5).\n\ttabbed end"
	if len(mcp.posts) != 1 || mcp.posts[0] != want {
		t.Fatalf("posts = %q, want [%q]", mcp.posts, want)
	}
}
