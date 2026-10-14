package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// codexSample is what `codex exec --json` printed for one real run in a scratch
// directory (codex-cli 0.162.1, prompt "run echo hello, then reply done"): two
// agent messages, one command.
const codexSample = "testdata/codex-events.jsonl"

var adapterBase = map[string]string{"CONCH_ADAPTER_TOKEN": "t", "CONCH_ADAPTER_PRINCIPAL_ID": "7", "CONCH_ADAPTER_CHANNEL": "ops"}

func withEnv(extra map[string]string) envLookup {
	values := map[string]string{}
	for k, v := range adapterBase {
		values[k] = v
	}
	for k, v := range extra {
		values[k] = v
	}
	return envOf(values)
}

func TestRuntimeProfileSelection(t *testing.T) {
	codexDefault := strings.Fields(codexCommand)
	tests := []struct {
		name         string
		env          map[string]string
		wantRuntime  string
		wantModel    string
		wantReader   readerKind
		wantStream   bool
		wantArgs     []string
		wantFirstArg string // when wantArgs is nil
	}{
		{"unset", nil, "claude-code", "sonnet", readerText, false, strings.Fields(defaultCommand), ""},
		{"claude-code", map[string]string{"CONCH_ADAPTER_RUNTIME": "claude-code"}, "claude-code", "sonnet", readerText, false, strings.Fields(defaultCommand), ""},
		{"claude-code with CLAUDE_BIN", map[string]string{"CLAUDE_BIN": "/x/claude"}, "claude-code", "sonnet", readerText, false, nil, "/x/claude"},
		{"claude-code with a command", map[string]string{"CONCH_ADAPTER_CMD": "/x/s.sh", "CLAUDE_BIN": "/x/claude"}, "claude-code", "sonnet", readerText, false, []string{"/x/s.sh"}, ""},
		{"codex", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m1"}, "codex", "m1", readerCodexJSONL, true, codexDefault, ""},
		{"codex with CODEX_BIN", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m1", "CODEX_BIN": "/x/codex"}, "codex", "m1", readerCodexJSONL, true, nil, "/x/codex"},
		// CLAUDE_BIN belongs to the Claude profile alone.
		{"codex ignores CLAUDE_BIN", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m1", "CLAUDE_BIN": "/x/claude"}, "codex", "m1", readerCodexJSONL, true, codexDefault, ""},
		// A command someone configured is run as written, and read as the profile reads.
		{"codex with a command", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m1", "CONCH_ADAPTER_CMD": "/x/stub.sh --flag", "CODEX_BIN": "/x/codex"}, "codex", "m1", readerCodexJSONL, true, []string{"/x/stub.sh", "--flag"}, ""},
		{"unknown label", map[string]string{"CONCH_ADAPTER_RUNTIME": "fixture"}, "fixture", "sonnet", readerText, false, strings.Fields(defaultCommand), ""},
		{"unknown label with a command", map[string]string{"CONCH_ADAPTER_RUNTIME": "fixture", "CONCH_ADAPTER_CMD": "/x/echo.sh", "CONCH_ADAPTER_FIXTURE": "1"}, "fixture", "sonnet", readerText, false, []string{"/x/echo.sh"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(withEnv(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Runtime != tt.wantRuntime || cfg.Model != tt.wantModel || cfg.Reader != tt.wantReader || cfg.StreamsActivity != tt.wantStream {
				t.Fatalf("config = runtime %q model %q reader %d stream %v", cfg.Runtime, cfg.Model, cfg.Reader, cfg.StreamsActivity)
			}
			if tt.wantArgs != nil && !reflect.DeepEqual(cfg.Command.args, tt.wantArgs) {
				t.Fatalf("command = %q, want %q", cfg.Command.args, tt.wantArgs)
			}
			if tt.wantArgs == nil && cfg.Command.args[0] != tt.wantFirstArg {
				t.Fatalf("first word = %q, want %q", cfg.Command.args[0], tt.wantFirstArg)
			}
		})
	}
}

// The default Codex command is exactly the one the issue decided, with the
// read-only sandbox, and takes the prompt on standard input.
func TestCodexDefaultCommandIsReadOnly(t *testing.T) {
	cfg, err := loadConfig(withEnv(map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "exec", "--json", "--model", "m1", "--sandbox", "read-only", "--skip-git-repo-check", "--ephemeral", "--color", "never"}
	if got := cfg.Command.argv("PROMPT", "m1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if cfg.Command.usesPromptArgument() {
		t.Fatal("the prompt must go to standard input, not into the arguments")
	}
}

func TestCodexRequiresAModel(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"none", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex"}, false},
		{"empty", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": ""}, false},
		{"none with a command", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_CMD": "/x/stub.sh"}, false},
		{"legacy name", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_BOT_MODEL": "m1"}, true},
		{"given", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m1"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(withEnv(tt.env))
			if tt.ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "CONCH_ADAPTER_MODEL") || strings.Contains(err.Error(), "\n") {
				t.Fatalf("error = %v, want one line naming CONCH_ADAPTER_MODEL", err)
			}
		})
	}
}

func TestWorkDir(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		env      map[string]string
		wantDir  string
		wantTail []string // the end of the command
	}{
		{"unset", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m"}, "", []string{"--color", "never"}},
		{"codex default gets -C", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m", "CONCH_ADAPTER_WORKDIR": dir}, dir, []string{"never", "-C", dir}},
		{"codex with a command does not", map[string]string{"CONCH_ADAPTER_RUNTIME": "codex", "CONCH_ADAPTER_MODEL": "m", "CONCH_ADAPTER_WORKDIR": dir, "CONCH_ADAPTER_CMD": "/x/s.sh"}, dir, []string{"/x/s.sh"}},
		{"claude default sets the directory only", map[string]string{"CONCH_ADAPTER_WORKDIR": dir}, dir, []string{"--output-format", "text"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(withEnv(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			args := cfg.Command.args
			if cfg.WorkDir != tt.wantDir || !reflect.DeepEqual(args[len(args)-len(tt.wantTail):], tt.wantTail) {
				t.Fatalf("workdir %q, command %q", cfg.WorkDir, args)
			}
		})
	}
	t.Run("relative becomes absolute", func(t *testing.T) {
		cfg, err := loadConfig(withEnv(map[string]string{"CONCH_ADAPTER_WORKDIR": "rel"}))
		if err != nil || !filepath.IsAbs(cfg.WorkDir) {
			t.Fatalf("workdir %q, %v", cfg.WorkDir, err)
		}
	})
	t.Run("the command runs there", func(t *testing.T) {
		runner, _ := scriptRunner(t, "pwd", false)
		runner.dir = dir
		got, err := runner.Reply(context.Background(), "p", nil)
		if want, _ := filepath.EvalSymlinks(dir); err != nil || got != want {
			t.Fatalf("pwd = %q, %v, want %q", got, err, want)
		}
	})
}

// sinkFixture records what a reader reports and is a fixture handler that
// fails the test if a marker ever reaches it.
type sinkFixture struct {
	t       *testing.T
	entries []string
}

func (s *sinkFixture) approval(context.Context, string) error {
	s.t.Error("the Codex reader acted on a fixture marker")
	return nil
}

func (s *sinkFixture) activity(_ context.Context, kind schema.ActivityKind, summary, outcome string) {
	s.entries = append(s.entries, string(kind)+"|"+summary+"|"+outcome)
}

func readCodexLines(t *testing.T, input string, fixture bool) (string, []string, error) {
	t.Helper()
	fx := &sinkFixture{t: t}
	runner := commandRunner{reader: readerCodexJSONL, fixture: fixture}
	reply, err := runner.readReply(context.Background(), strings.NewReader(input), fx)
	return reply, fx.entries, err
}

func TestCodexReaderOnTheRecordedSample(t *testing.T) {
	raw, err := os.ReadFile(codexSample)
	if err != nil {
		t.Fatal(err)
	}
	reply, entries, err := readCodexLines(t, string(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	// The first agent message is narration; the last is the reply. The command
	// is told by its first word, never by its arguments.
	want := []string{"command_started|started bash|", "command_finished|command finished|exit 0"}
	if reply != "done" || !reflect.DeepEqual(entries, want) {
		t.Fatalf("reply %q, entries %q, want %q and %q", reply, entries, "done", want)
	}
}

func TestCodexReader(t *testing.T) {
	notJSON := strings.Repeat("not json\n", maxNonJSONLines)
	tests := []struct {
		name        string
		input       string
		fixture     bool
		wantReply   string
		wantEntries []string
		wantErr     error
	}{
		{"empty", "", false, "", nil, nil},
		{"only the reply", `{"type":"item.completed","item":{"id":"a","type":"agent_message","text":"hi"}}` + "\n", false, "hi", nil, nil},
		{"the last message wins", `{"type":"item.completed","item":{"type":"agent_message","text":"one"}}` + "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"two"}}`, false, "two", nil, nil},
		{"a started message is not a reply", `{"type":"item.started","item":{"type":"agent_message","text":"early"}}` + "\n", false, "", nil, nil},
		{
			"a failing command and a tool call",
			`{"type":"item.started","item":{"id":"1","type":"command_execution","command":"/usr/bin/git push --force secret-arg","status":"in_progress"}}` + "\n" +
				`{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"/usr/bin/git push --force secret-arg","exit_code":2,"status":"failed"}}` + "\n" +
				`{"type":"item.started","item":{"id":"2","type":"mcp_tool_call","server":"s","tool":"lookup","status":"in_progress"}}` + "\n" +
				`{"type":"item.completed","item":{"id":"2","type":"mcp_tool_call","server":"s","tool":"lookup","status":"completed"}}` + "\n",
			false, "",
			[]string{"command_started|started git|", "command_finished|command finished|exit 2", "command_started|started lookup|", "command_finished|command finished|completed"}, nil,
		},
		{
			"a command that never announced itself still pairs",
			`{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"ls -la","exit_code":0}}` + "\n",
			false, "", []string{"command_started|started ls|", "command_finished|command finished|exit 0"}, nil,
		},
		{
			"no exit code and no status",
			`{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"ls"}}` + "\n",
			false, "", []string{"command_started|started ls|", "command_finished|command finished|unknown"}, nil,
		},
		{"other events are ignored", `{"type":"thread.started","thread_id":"x"}` + "\n" + `{"type":"turn.started"}` + "\n" + `{"type":"item.completed","item":{"type":"reasoning","text":"hmm"}}` + "\n" + `{"type":"turn.completed","usage":{}}` + "\n", false, "", nil, nil},
		// A marker is not JSON, so it is an ignored line, whether the fixture is on or not.
		{"a marker line", "@approval do something\n@sleep 1\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}` + "\n", true, "ok", nil, nil},
		{"non-JSON lines up to the bound", notJSON + `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}` + "\n", false, "ok", nil, nil},
		{"one more than the bound", notJSON + "one too many\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}` + "\n", false, "", nil, errNotJSONL},
		{"blank lines are not counted", strings.Repeat("\n", 500) + `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`, false, "ok", nil, nil},
		{"a JSON value that is no object is counted", strings.Repeat("5\n", maxNonJSONLines+1), false, "", nil, errNotJSONL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply, entries, err := readCodexLines(t, tt.input, tt.fixture)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if reply != tt.wantReply || !reflect.DeepEqual(entries, tt.wantEntries) {
				t.Fatalf("reply %q, entries %q; want %q, %q", reply, entries, tt.wantReply, tt.wantEntries)
			}
		})
	}
}

// A line longer than a reply may be is dropped, not fatal: a tool's huge
// output must not fail a run that otherwise worked.
func TestCodexReaderDropsAnOverlongLine(t *testing.T) {
	huge := `{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"` + strings.Repeat("x", maxReplyBytes+10) + `"}}` + "\n"
	reply, entries, err := readCodexLines(t, huge+`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`+"\n", false)
	if err != nil || reply != "ok" || len(entries) != 0 {
		t.Fatalf("reply %q, entries %q, err %v", reply, entries, err)
	}
}

func TestCodexReaderWithoutASink(t *testing.T) {
	runner := commandRunner{reader: readerCodexJSONL}
	reply, err := runner.readReply(context.Background(), strings.NewReader(`{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"ls","exit_code":0}}`+"\n"+`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`), nil)
	if err != nil || reply != "ok" {
		t.Fatalf("reply %q, %v", reply, err)
	}
}

// codexLoop is a loop whose command is the recorded sample replayed by cat,
// read by the Codex reader: the same path a real run takes.
func codexLoop(t *testing.T, mcp *fakeMCP, script string, clk timeSource) *botLoop {
	t.Helper()
	runner, _ := scriptRunner(t, script, true)
	runner.reader = readerCodexJSONL
	loop := loopWithSession(mcp, runner, clk)
	loop.cfg.StreamsActivity = true
	loop.cfg.Command = runner.spec
	return loop
}

func TestCodexRunThroughTheLoop(t *testing.T) {
	const secretMessage = "SECRET-MESSAGE-BODY"
	mcp := &fakeMCP{}
	loop := codexLoop(t, mcp, "cat "+codexSample, newFakeClock())
	if err := answer(t, loop, message(1, 2, secretMessage)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start_run", "update_run:starting", "update_run:running",
		"activity:command_started", "activity:command_finished",
		"post_message", "activity:reply_posted", "update_run:completed:replied",
	}
	if got := mcp.eventLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
	if !reflect.DeepEqual(mcp.posts, []string{"done"}) {
		t.Fatalf("posted %q, want the sample's final agent message", mcp.posts)
	}
	if a := mcp.activities; a[0].summary != "started bash" || a[1].outcome != "exit 0" {
		t.Fatalf("activity = %+v", a)
	}
	// Nothing of the prompt, the command's arguments or its output.
	for _, a := range mcp.activities {
		for _, text := range []string{a.summary, a.outcome, a.data} {
			for _, secret := range []string{secretMessage, "hello", "echo", "-lc", "I’ll run"} {
				if strings.Contains(text, secret) {
					t.Errorf("%q appears in an activity entry: %+v", secret, a)
				}
			}
		}
	}
	for _, state := range mcp.runStates {
		if strings.Contains(state, secretMessage) {
			t.Errorf("the prompt is in a run state: %q", state)
		}
	}
}

// Codex printing something that is not JSONL fails the run with that stated.
func TestCodexRunFailsOnOutputThatIsNotJSONL(t *testing.T) {
	mcp := &fakeMCP{}
	loop := codexLoop(t, mcp, `sh -c 'i=0; while [ $i -le 100 ]; do echo plain; i=$((i+1)); done' sh`, newFakeClock())
	err := answer(t, loop, message(1, 2, "q"))
	if !errors.Is(err, errNotJSONL) {
		t.Fatalf("error = %v, want errNotJSONL", err)
	}
	if last := mcp.runStates[len(mcp.runStates)-1]; last != "failed:codex output not JSONL" {
		t.Fatalf("run states = %q", mcp.runStates)
	}
	if len(mcp.posts) != 0 {
		t.Fatalf("posted %q", mcp.posts)
	}
}

// A cancel while Codex runs kills the whole process group and the run ends
// cancelled.
func TestCodexCancelKillsTheProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	started := `{"type":"item.started","item":{"id":"1","type":"command_execution","command":"sleep 300","status":"in_progress"}}`
	script := `sh -c 'sleep 300 & echo $! > ` + pidFile + `; echo ` + "'\"'\"'" + started + "'\"'\"'" + `; wait' sh`
	mcp := &fakeMCP{interv: []schema.InterventionV1{intervention(5, 1, schema.InterventionKindCancel, "")}, polled: make(chan struct{}, 4)}
	clk := newFakeClock()
	loop := codexLoop(t, mcp, script, clk)

	done := make(chan error, 1)
	go func() { done <- answer(t, loop, message(1, 2, "q")) }()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		if raw, err := os.ReadFile(pidFile); err == nil { //nolint:gosec // a file this test made in its own temp directory
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
	}
	// Both the grandchild and the streamed started entry exist before the cancel.
	for deadline := time.Now().Add(10 * time.Second); countOf(mcp.eventLog(), "activity:command_started") == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command's started entry never arrived")
		}
	}
	clk.fire(interventionPollInterval)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancel is not an error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run did not end after the cancel")
	}
	if processAlive(pid) {
		t.Fatalf("the grandchild %d survived the cancel", pid)
	}
	if got := mcp.runStates[len(mcp.runStates)-1]; got != "cancelled:cancelled by request" {
		t.Fatalf("run states = %q", mcp.runStates)
	}
}
