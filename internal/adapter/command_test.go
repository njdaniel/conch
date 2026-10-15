package adapter

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{"words", "claude -p {prompt}", []string{"claude", "-p", "{prompt}"}},
		{"extra whitespace", "  a \t b\n c ", []string{"a", "b", "c"}},
		{"single quotes are literal", `echo 'a b' 'c"d' '$HOME'`, []string{"echo", "a b", `c"d`, "$HOME"}},
		{"double quotes group", `echo "a b" "c'd"`, []string{"echo", "a b", "c'd"}},
		{"escapes in double quotes", `echo "a\"b" "c\\d" "e\nf"`, []string{"echo", `a"b`, `c\d`, `e\nf`}},
		{"backslash outside quotes", `echo a\ b c\"d`, []string{"echo", "a b", `c"d`}},
		{"adjacent quoted pieces are one word", `echo a"b c"'d e'f`, []string{"echo", "ab cd ef"}},
		{"an empty quoted word is a word", `echo "" ''`, []string{"echo", "", ""}},
		{"placeholders stay as written", `x --model {model} '{prompt}'`, []string{"x", "--model", "{model}", "{prompt}"}},
		// Hostile input: all of it is characters of arguments, and nothing
		// reaches a shell, so none of it is run.
		{"command substitution", `echo $(rm -rf /) ${HOME}`, []string{"echo", "$(rm", "-rf", "/)", "${HOME}"}},
		{"command substitution quoted", `echo "$(rm -rf /)" '$(id)'`, []string{"echo", "$(rm -rf /)", "$(id)"}},
		{"semicolon and pipe", `echo a;b && c | d > e`, []string{"echo", "a;b", "&&", "c", "|", "d", ">", "e"}},
		{"semicolon in quotes", `echo "a; rm -rf /"`, []string{"echo", "a; rm -rf /"}},
		{"backticks", "echo `id` \"`id`\"", []string{"echo", "`id`", "`id`"}},
		{"glob", `echo * ?`, []string{"echo", "*", "?"}},
		{"redirect and background", `echo hi >/etc/passwd &`, []string{"echo", "hi", ">/etc/passwd", "&"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitCommand(tt.line)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("splitCommand(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestSplitCommandErrors(t *testing.T) {
	for name, line := range map[string]string{
		"empty":                "",
		"only space":           "  \t ",
		"unterminated single":  `echo 'a`,
		"unterminated double":  `echo "a`,
		"trailing backslash":   `echo a\`,
		"quote ends in escape": `echo "a\`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := splitCommand(line); err == nil {
				t.Fatalf("splitCommand(%q) = %q, want an error", line, got)
			}
		})
	}
}

func TestParseCommandAndArgv(t *testing.T) {
	const prompt = "line one\nsays {model} and 'quotes' $(id); `x`"
	tests := []struct {
		name       string
		line       string
		binary     string
		wantArgv   []string
		wantPrompt bool // {prompt} is an argument
	}{
		{"default command", defaultCommand, "", []string{"claude", "-p", prompt, "--model", "sonnet", "--dangerously-skip-permissions", "--output-format", "text"}, true},
		{"CLAUDE_BIN replaces the first word", defaultCommand, "/opt/claude", []string{"/opt/claude", "-p", prompt, "--model", "sonnet", "--dangerously-skip-permissions", "--output-format", "text"}, true},
		{"the prompt is one argument whatever it holds", "tool {prompt}", "", []string{"tool", prompt}, true},
		{"a placeholder inside a word is not substituted", "tool --model={model} x{prompt}", "", []string{"tool", "--model={model}", "x{prompt}"}, false},
		{"no prompt argument means stdin", "tool --model {model}", "", []string{"tool", "--model", "sonnet"}, false},
		{"a value is not searched for placeholders", "tool {model} {prompt}", "", []string{"tool", "sonnet", prompt}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := parseCommand(tt.line, tt.binary)
			if err != nil {
				t.Fatal(err)
			}
			if got := spec.argv(prompt, "sonnet"); !reflect.DeepEqual(got, tt.wantArgv) {
				t.Fatalf("argv = %q, want %q", got, tt.wantArgv)
			}
			if got := spec.usesPromptArgument(); got != tt.wantPrompt {
				t.Fatalf("usesPromptArgument = %v, want %v", got, tt.wantPrompt)
			}
		})
	}
	// Substitution does not change the template for the next reply.
	spec, _ := parseCommand("tool {prompt}", "")
	_ = spec.argv("first", "m")
	if got := spec.argv("second", "m"); got[1] != "second" {
		t.Fatalf("argv = %q", got)
	}
	if got := (commandSpec{args: []string{"/usr/bin/claude", "-p"}}).name(); got != "claude" {
		t.Fatalf("name = %q, want the first word without its directory", got)
	}
}

// recordingFixture is the run a fixture marker talks to.
type recordingFixture struct {
	titles []string
	err    error
	onCall func(context.Context) error
}

func (r *recordingFixture) approval(ctx context.Context, title string) error {
	r.titles = append(r.titles, title)
	if r.onCall != nil {
		return r.onCall(ctx)
	}
	return r.err
}

func scriptRunner(t *testing.T, line string, fixture bool) (commandRunner, *[]time.Duration) {
	t.Helper()
	spec, err := parseCommand(line, "")
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	return commandRunner{
		spec: spec, model: "sonnet", timeout: time.Minute, fixture: fixture,
		sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
	}, &slept
}

// Arguments reach the command as the characters they are: nothing in the
// template or in the prompt is run by a shell.
func TestCommandRunnerArgumentsAreLiteral(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	const hostilePrompt = "'; touch " + "MARKER" + "; echo '$(touch MARKER) `touch MARKER` \"\n--flag"
	prompt := strings.ReplaceAll(hostilePrompt, "MARKER", marker)
	tests := []struct {
		name string
		line string
		want string
	}{
		{"hostile template words", `printf '%s\n' 'a;b' '$(touch ` + marker + `)' '` + "`touch " + marker + "`" + `' "x y"`,
			"a;b\n$(touch " + marker + ")\n`touch " + marker + "`\nx y\n"},
		{"hostile prompt as one argument", `printf '%s\n' {prompt}`, prompt + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, _ := scriptRunner(t, tt.line, false)
			got, err := runner.Reply(context.Background(), prompt, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != strings.TrimSuffix(tt.want, "\n") {
				t.Fatalf("output = %q, want %q", got, tt.want)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a shell ran part of the command")
			}
		})
	}
}

func TestCommandRunnerPromptOnStdinWithoutPlaceholder(t *testing.T) {
	runner, _ := scriptRunner(t, "cat", false)
	got, err := runner.Reply(context.Background(), "the whole\nprompt", nil)
	if err != nil || got != "the whole\nprompt" {
		t.Fatalf("Reply = %q, %v", got, err)
	}
}

func TestCommandRunnerErrors(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		timeout  time.Duration
		wantExit int
		wantErr  error
	}{
		{"non-zero exit", `sh -c 'echo partial; exit 3' sh`, time.Minute, 3, nil},
		{"zero exit", `echo ok`, time.Minute, 0, nil},
		{"timeout", `sleep 30`, 50 * time.Millisecond, -1, errReplyTimeout},
		{"too much output", `sh -c 'yes | head -c 3000000' sh`, time.Minute, -1, errOutputTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, _ := scriptRunner(t, tt.line, false)
			runner.timeout = tt.timeout
			_, err := runner.Reply(context.Background(), "p", nil)
			var exit *exitError
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
			case tt.wantExit > 0:
				if !errors.As(err, &exit) || exit.code != tt.wantExit {
					t.Fatalf("error = %v, want exit %d", err, tt.wantExit)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	t.Run("a command that cannot start", func(t *testing.T) {
		runner, _ := scriptRunner(t, `/nonexistent/conch-adapter-test`, false)
		_, err := runner.Reply(context.Background(), "SECRET-PROMPT", nil)
		if err == nil || strings.Contains(err.Error(), "SECRET-PROMPT") {
			t.Fatalf("error = %v, want one that does not quote the prompt", err)
		}
	})
}

// The markers are acted on with the fixture on and are plain reply text with
// it off: a model's output can never raise an approval.
func TestFixtureMarkersOnAndOff(t *testing.T) {
	const script = `printf '%s\n' 'first' '@approval do something' '@sleep 0.25' 'last'`
	tests := []struct {
		name       string
		fixture    bool
		wantReply  string
		wantTitles []string
		wantSlept  []time.Duration
	}{
		{"off", false, "first\n@approval do something\n@sleep 0.25\nlast", nil, nil},
		{"on", true, "first\nlast", []string{"do something"}, []time.Duration{250 * time.Millisecond}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, slept := scriptRunner(t, script, tt.fixture)
			fx := &recordingFixture{}
			got, err := runner.Reply(context.Background(), "p", fx)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.wantReply {
				t.Errorf("reply = %q, want %q", got, tt.wantReply)
			}
			if !reflect.DeepEqual(fx.titles, tt.wantTitles) {
				t.Errorf("approvals asked for = %q, want %q", fx.titles, tt.wantTitles)
			}
			if !reflect.DeepEqual(*slept, tt.wantSlept) {
				t.Errorf("slept = %v, want %v", *slept, tt.wantSlept)
			}
		})
	}
}

// A line that only looks like a marker is text even with the fixture on.
func TestFixtureMalformedMarkersAreText(t *testing.T) {
	for _, line := range []string{"@approval", "@approval ", "@sleep", "@sleep soon", "@sleep -1", "@sleep 99999", " @approval x", "@approvals x", "text @sleep 1"} {
		t.Run(line, func(t *testing.T) {
			runner, slept := scriptRunner(t, `printf '%s\n' '`+line+`'`, true)
			fx := &recordingFixture{}
			got, err := runner.Reply(context.Background(), "p", fx)
			if err != nil {
				t.Fatal(err)
			}
			if got != line || len(fx.titles) != 0 || len(*slept) != 0 {
				t.Fatalf("reply = %q, approvals %q, slept %v; want the line as text", got, fx.titles, *slept)
			}
		})
	}
	t.Run("approval marker without a run", func(t *testing.T) {
		runner, _ := scriptRunner(t, `printf '%s\n' '@approval x'`, true)
		if got, err := runner.Reply(context.Background(), "p", nil); err != nil || got != "@approval x" {
			t.Fatalf("Reply = %q, %v", got, err)
		}
	})
}

func TestFixtureApprovalFailureStopsTheCommand(t *testing.T) {
	runner, _ := scriptRunner(t, `sh -c 'echo "@approval x"; sleep 30' sh`, true)
	_, err := runner.Reply(context.Background(), "p", &recordingFixture{err: errors.New("denied")})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %v, want the approval's", err)
	}
}

// Ending the context kills the command's whole process group and Reply does
// not return before it is gone.
func TestCommandRunnerKillsTheProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := `sh -c 'sleep 300 & echo $! > ` + pidFile + `; echo "@approval go"; wait' sh`
	runner, _ := scriptRunner(t, script, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx := &recordingFixture{onCall: func(ctx context.Context) error {
		// The grandchild is running (its pid is written before the marker).
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}}
	_, err := runner.Reply(ctx, "p", fx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	raw, err := os.ReadFile(pidFile) //nolint:gosec // a file this test made in its own temp directory
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if processAlive(pid) {
		t.Fatalf("the grandchild %d is still running after Reply returned", pid)
	}
}

// processAlive reports whether pid is a running process; a zombie waiting to
// be reaped by init is not running.
func processAlive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// "pid (comm) S ...": the state follows the last closing parenthesis.
	rest := stat[strings.LastIndexByte(string(stat), ')')+2:]
	return len(rest) > 0 && rest[0] != 'Z'
}

// A process that left the command's group and kept the output pipe open must
// not hold Reply past the timeout: Reply stops reading when the reply context
// ends, whatever is still holding the pipe.
func TestCommandRunnerReturnsWhileAnEscapedChildHoldsThePipe(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	runner, _ := scriptRunner(t, `sh -c 'setsid sleep 30 & echo $! > `+pidFile+`; echo hi' sh`, false)
	runner.timeout = 300 * time.Millisecond
	t.Cleanup(func() { // the escaped sleep is in no group of ours: stop it here
		if raw, err := os.ReadFile(pidFile); err == nil { //nolint:gosec // a file this test made in its own temp directory
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	done := make(chan error, 1)
	go func() { _, err := runner.Reply(context.Background(), "p", nil); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, errReplyTimeout) {
			t.Fatalf("error = %v, want errReplyTimeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Reply did not return while an escaped child held the output pipe")
	}
}

func TestExitErrorOutcome(t *testing.T) {
	for code, want := range map[int]string{0: "exit 0", 3: "exit 3", -1: "signal"} {
		if got := (&exitError{code: code}).outcome(); got != want {
			t.Errorf("outcome(%d) = %q, want %q", code, got, want)
		}
	}
	runner, _ := scriptRunner(t, `sh -c 'kill -TERM $$' sh`, false)
	_, err := runner.Reply(context.Background(), "p", nil)
	var exit *exitError
	if !errors.As(err, &exit) || exit.outcome() != "signal" {
		t.Fatalf("error = %v, want a command ended by a signal", err)
	}
}

// The command never sees the agent's Conch credentials or settings, and every
// other variable reaches it unchanged. The child is a real process printing its
// environment.
func TestCommandRunnerChildEnvironment(t *testing.T) {
	stripped := []string{
		"CONCH_ADAPTER_TOKEN", "CONCH_BOT_TOKEN", "CONCH_TOKEN",
		"CONCH_ADAPTER_PRINCIPAL_ID", "CONCH_BOT_CHANNEL", "CONCH_ADAPTER_CMD",
		"CONCH_ADAPTER_RUNTIME", "CONCH_ADAPTER_WORKDIR", "CONCH_BOT_SERVER",
	}
	kept := map[string]string{
		"CONCH_FOO":     "not an adapter name",
		"CONCHD_TOKEN":  "different prefix",
		"FOO":           "bar",
		"FIXTURE_SLEEP": "1",
		"CLAUDE_BIN":    "claude",
		"CODEX_BIN":     "codex",
	}
	for _, name := range stripped {
		t.Setenv(name, "secret-"+name)
	}
	for name, value := range kept {
		t.Setenv(name, value)
	}
	runner, _ := scriptRunner(t, "env", false)
	out, err := runner.Reply(context.Background(), "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			got[name] = value
		}
	}
	for _, name := range stripped {
		if _, ok := got[name]; ok {
			t.Errorf("%s reached the command", name)
		}
	}
	if strings.Contains(out, "secret-") {
		t.Error("a secret value reached the command")
	}
	for name, want := range kept {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	for _, name := range []string{"HOME", "PATH"} {
		if got[name] != os.Getenv(name) {
			t.Errorf("%s = %q, want %q", name, got[name], os.Getenv(name))
		}
	}
}

func TestStripControl(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "hello, world: ü 日本", "hello, world: ü 日本"},
		{"empty", "", ""},
		{"cursor back and erase", "sonne\x1b[13D\x1b[K", "sonne"},
		{"erase line", "a\x1b[Kb", "ab"},
		{"colour with parameters", "\x1b[1;31mred\x1b[0m", "red"},
		{"private csi", "\x1b[?25lx\x1b[?25h", "x"},
		{"csi with intermediate", "a\x1b[0 qb", "ab"},
		{"osc title, BEL", "a\x1b]0;title\x07b", "ab"},
		{"osc title, ST", "a\x1b]0;title\x1b\\b", "ab"},
		{"unterminated osc", "a\x1b]0;title", "a"},
		{"truncated csi", "a\x1b[12", "a"},
		{"bare ESC", "a\x1bb", "ab"},
		{"trailing ESC", "a\x1b", "a"},
		{"BEL", "a\x07b", "ab"},
		{"CR and others", "a\rb\x00c\x08d\x7fe", "abcde"},
		{"tab kept", "a\tb", "a\tb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripControl(tt.in); got != tt.want {
				t.Fatalf("stripControl(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
