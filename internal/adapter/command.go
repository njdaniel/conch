package adapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultCommand is what the adapter runs when CONCH_ADAPTER_CMD is not set:
// today's behaviour, the local Claude Code CLI.
const defaultCommand = "claude -p {prompt} --model {model} --dangerously-skip-permissions --output-format text"

const (
	promptPlaceholder = "{prompt}"
	modelPlaceholder  = "{model}"

	// maxReplyBytes bounds how much of a subprocess's output is kept. A post
	// is refused above 1 MiB anyway; this keeps a runaway command from
	// filling the adapter's memory first.
	maxReplyBytes = 1 << 20
)

// errReplyTimeout is what a runner returns when the command, or a fixture
// marker's wait, outlasted the reply timeout. The command has been killed.
var errReplyTimeout = errors.New("reply timed out")

// errOutputTooLarge is returned when the command wrote more than maxReplyBytes.
var errOutputTooLarge = errors.New("command output too large")

// exitError is a command that ran and exited non-zero.
type exitError struct{ code int }

func (e *exitError) Error() string { return "command exited with status " + strconv.Itoa(e.code) }

// outcome is how a run reports the exit: the status, or "signal" for a command
// a signal the adapter did not send ended (the status is then -1).
func (e *exitError) outcome() string {
	if e.code < 0 {
		return "signal"
	}
	return "exit " + strconv.Itoa(e.code)
}

// splitCommand splits a command line into arguments the way a POSIX shell
// would read words, and does nothing else: no variable, command or glob
// expansion, no operators. A quote or a backslash only groups or escapes, so
// text such as $(rm -rf /) or `x` or ; stays literal characters of an argument.
// The caller runs the result with exec, never through a shell.
func splitCommand(line string) ([]string, error) {
	var (
		args    []string
		current strings.Builder
		inWord  bool
		quote   rune // 0, '\'' or '"'
		escaped bool
	)
	flush := func() {
		if inWord {
			args = append(args, current.String())
			current.Reset()
			inWord = false
		}
	}
	for _, r := range line {
		switch {
		case escaped:
			// Inside double quotes a backslash only escapes " and \; elsewhere
			// a backslash that does not escape them stays a backslash.
			if quote == '"' && r != '"' && r != '\\' {
				current.WriteRune('\\')
			}
			current.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				current.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in command")
	}
	if escaped {
		return nil, errors.New("trailing backslash in command")
	}
	flush()
	if len(args) == 0 {
		return nil, errors.New("command is empty")
	}
	return args, nil
}

// commandSpec is a parsed command: its arguments with the placeholders still
// in place.
type commandSpec struct {
	args []string
}

// parseCommand splits line and, when binary is not empty, replaces the first
// word with it (CLAUDE_BIN).
func parseCommand(line, binary string) (commandSpec, error) {
	args, err := splitCommand(line)
	if err != nil {
		return commandSpec{}, err
	}
	if binary != "" {
		args[0] = binary
	}
	return commandSpec{args: args}, nil
}

// usesPromptArgument reports whether the prompt travels as an argument; if
// not, it is written to the command's standard input.
func (c commandSpec) usesPromptArgument() bool {
	for _, arg := range c.args {
		if arg == promptPlaceholder {
			return true
		}
	}
	return false
}

// argv substitutes the placeholders. Only an argument that is exactly a
// placeholder is replaced, and with the value as one argument whatever it
// contains; a value is never searched for placeholders itself.
func (c commandSpec) argv(prompt, model string) []string {
	out := make([]string, len(c.args))
	for i, arg := range c.args {
		switch arg {
		case promptPlaceholder:
			out[i] = prompt
		case modelPlaceholder:
			out[i] = model
		default:
			out[i] = arg
		}
	}
	return out
}

// name is the command's first word without its directory: what an activity
// entry may say about the command, never its arguments.
func (c commandSpec) name() string {
	if len(c.args) == 0 {
		return "command"
	}
	return filepath.Base(c.args[0])
}

// fixtureHandler is what a fixture marker asks of the run it appears in.
type fixtureHandler interface {
	// approval raises an approval titled title and returns once it is decided.
	// It returns an error when ctx ends first or the approval cannot be raised.
	approval(ctx context.Context, title string) error
}

// ClaudeRunner runs the command for one reply.
type ClaudeRunner interface {
	// Reply runs the command with prompt and returns what it wrote. fx is the
	// run the reply belongs to; a runner that acts on fixture markers calls it.
	// It returns errReplyTimeout, an *exitError, or ctx's error when ctx ended
	// first, and in each case the command, with everything it started, is gone
	// by the time Reply returns.
	Reply(ctx context.Context, prompt string, fx fixtureHandler) (string, error)
}

// commandRunner runs a commandSpec as a subprocess.
type commandRunner struct {
	spec    commandSpec
	model   string
	timeout time.Duration
	// fixture turns the markers on. Without it every line of output is reply
	// text: no output of a model can raise an approval.
	fixture bool
	sleep   func(context.Context, time.Duration) error
}

func (c commandRunner) Reply(ctx context.Context, prompt string, fx fixtureHandler) (string, error) {
	replyCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	argv := c.spec.argv(prompt, c.model)
	cmd := exec.CommandContext(replyCtx, argv[0], argv[1:]...) // #nosec G204 -- the operator explicitly configures the command; it is split without a shell
	ownProcessGroup(cmd)
	// A grandchild that kept the output pipe open must not hold Wait after the
	// group has been killed.
	cmd.WaitDelay = 2 * time.Second
	if !c.spec.usesPromptArgument() {
		cmd.Stdin = strings.NewReader(prompt)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("command stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start command: %w", err)
	}
	// When the reply context ends, the group is killed (cmd.Cancel); close the
	// read end as well. A process that escaped the group and kept the pipe open
	// would otherwise hold the scan below for as long as it lives.
	go func() {
		<-replyCtx.Done()
		_ = stdout.Close()
	}()
	reply, readErr := c.readReply(replyCtx, stdout, fx)
	if readErr != nil {
		// Stop the command now, whatever stopped the reading: a marker's wait
		// ended, the output was too large, or the context ended.
		cancel()
	}
	waitErr := cmd.Wait()
	if replyCtx.Err() != nil {
		// The group was killed. Wait has reaped the command; make sure what it
		// started is gone too before anyone reports the run over.
		waitGroupGone(cmd.Process.Pid, 2*time.Second)
	}
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.Is(replyCtx.Err(), context.DeadlineExceeded), errors.Is(readErr, context.DeadlineExceeded):
		return "", errReplyTimeout
	case readErr != nil:
		return "", readErr
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			return "", &exitError{code: exit.ExitCode()}
		}
		return "", fmt.Errorf("wait for command: %w", waitErr)
	}
	return reply, nil
}

// readReply reads the command's output line by line. With the fixture on, a
// line that is a marker is acted on and left out of the reply.
func (c commandRunner) readReply(ctx context.Context, stdout io.Reader, fx fixtureHandler) (string, error) {
	var (
		lines []string
		size  int
	)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), maxReplyBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if c.fixture {
			handled, err := c.fixtureMarker(ctx, line, fx)
			if err != nil {
				return "", err
			}
			if handled {
				continue
			}
		}
		size += len(line) + 1
		if size > maxReplyBytes {
			return "", errOutputTooLarge
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return "", errOutputTooLarge
		}
		return "", fmt.Errorf("read command output: %w", err)
	}
	return strings.Join(lines, "\n"), nil
}

// fixtureMarker acts on a marker line and reports whether the line was one. A
// line that starts like a marker but is malformed is ordinary text.
func (c commandRunner) fixtureMarker(ctx context.Context, line string, fx fixtureHandler) (bool, error) {
	if title, ok := strings.CutPrefix(line, "@approval "); ok {
		title = strings.TrimSpace(title)
		if title == "" || fx == nil {
			return false, nil
		}
		return true, fx.approval(ctx, title)
	}
	if seconds, ok := strings.CutPrefix(line, "@sleep "); ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(seconds), 64)
		if err != nil || n < 0 || n > 3600 {
			return false, nil //nolint:nilerr // a malformed marker is ordinary reply text, not a failure
		}
		return true, c.sleep(ctx, time.Duration(n*float64(time.Second)))
	}
	return false, nil
}
