package adapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// pausedTooLongError is what a runner returns when the command was paused for
// longer than the bound. The command has been killed.
type pausedTooLongError struct{ limit time.Duration }

func (e *pausedTooLongError) Error() string { return "paused longer than " + e.limit.String() }

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

// processControl is what an intervention can do to the command of a run.
type processControl interface {
	// pause freezes the command and everything it started; resume lets them
	// go on. They answer errNotRunning, errAlreadyPaused or errNotPaused when
	// there is nothing to do.
	pause() error
	resume() error
}

// commandHolder is a fixtureHandler that wants to know about the command's
// process once it is started: the run, so that an intervention can reach it.
type commandHolder interface {
	commandStarted(processControl)
}

var (
	errNotRunning    = errors.New("command is not running")
	errAlreadyPaused = errors.New("command is already paused")
	errNotPaused     = errors.New("command is not paused")
)

// commandControl owns the reply timeout of one command and the command's
// pause. Time spent paused does not count against the reply timeout: pausing
// stops its timer, resuming starts it again with what was left. A command
// paused for longer than maxPause, all its pauses together, is killed.
type commandControl struct {
	// kill ends the command, saying why; it is the cancel of the reply context.
	kill     func(cause error)
	maxPause time.Duration

	mu          sync.Mutex
	pid         int // 0 until the command is started
	ended       bool
	paused      bool
	pausedAt    time.Time     // when the current pause began
	pausedTotal time.Duration // of the pauses already resumed
	deadline    time.Time
	remaining   time.Duration // of the timeout, while paused
	timeout     *time.Timer
	pauseTimer  *time.Timer
}

func newCommandControl(timeout, maxPause time.Duration, kill func(error)) *commandControl {
	if maxPause <= 0 {
		maxPause = timeout
	}
	c := &commandControl{kill: kill, maxPause: maxPause, deadline: time.Now().Add(timeout)}
	c.timeout = time.AfterFunc(timeout, func() { kill(errReplyTimeout) })
	return c
}

// started records the command's process; before that nothing can be paused.
func (c *commandControl) started(pid int) {
	c.mu.Lock()
	c.pid = pid
	c.mu.Unlock()
}

// end stops the timers. After it nothing is signalled: the group's id may be
// given to another process once the command has been reaped.
func (c *commandControl) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = true
	c.timeout.Stop()
	if c.pauseTimer != nil {
		c.pauseTimer.Stop()
	}
}

func (c *commandControl) pause() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.pid == 0 || c.ended:
		return errNotRunning
	case c.paused:
		return errAlreadyPaused
	}
	if err := stopGroup(c.pid); err != nil {
		if errors.Is(err, errNotRunning) {
			return err
		}
		return fmt.Errorf("stop command: %w", err)
	}
	c.paused = true
	// A timer that already fired has killed the command; remaining is then
	// zero and resuming changes nothing.
	c.remaining = 0
	if c.timeout.Stop() {
		c.remaining = max(time.Until(c.deadline), 0)
	}
	// The bound is for the run, not for each pause: cycling pause and resume
	// must not stretch a reply for ever.
	c.pausedAt = time.Now()
	limit := c.maxPause
	tooLong := &pausedTooLongError{limit: limit}
	left := limit - c.pausedTotal
	if left <= 0 {
		c.kill(tooLong)
		return nil
	}
	c.pauseTimer = time.AfterFunc(left, func() { c.kill(tooLong) })
	return nil
}

func (c *commandControl) resume() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.pid == 0 || c.ended:
		return errNotRunning
	case !c.paused:
		return errNotPaused
	}
	if err := continueGroup(c.pid); err != nil {
		if errors.Is(err, errNotRunning) {
			return err
		}
		return fmt.Errorf("continue command: %w", err)
	}
	c.paused = false
	c.pausedTotal += time.Since(c.pausedAt)
	if c.pauseTimer != nil {
		c.pauseTimer.Stop()
	}
	c.deadline = time.Now().Add(c.remaining)
	c.timeout.Reset(c.remaining)
	return nil
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
	// reader says how the output is read; the zero value is plain text.
	reader readerKind
	// dir is the command's working directory; empty means the adapter's own.
	dir string
	// maxPause is how long a paused command may stay paused; zero means the
	// reply timeout.
	maxPause time.Duration
}

func (c commandRunner) Reply(ctx context.Context, prompt string, fx fixtureHandler) (string, error) {
	replyCtx, cancelReply := context.WithCancelCause(ctx)
	cancel := func() { cancelReply(nil) }
	defer cancel()
	ctl := newCommandControl(c.timeout, c.maxPause, cancelReply)
	defer ctl.end()
	argv := c.spec.argv(prompt, c.model)
	cmd := exec.CommandContext(replyCtx, argv[0], argv[1:]...) // #nosec G204 -- the operator explicitly configures the command; it is split without a shell
	ownProcessGroup(cmd)
	cmd.Dir = c.dir
	cmd.Env = childEnv(os.Environ())
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
	ctl.started(cmd.Process.Pid)
	if holder, ok := fx.(commandHolder); ok {
		holder.commandStarted(ctl)
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
	// The command is reaped: its group id is no longer ours to signal.
	ctl.end()
	if replyCtx.Err() != nil {
		// The group was killed. Wait has reaped the command; make sure what it
		// started is gone too before anyone reports the run over.
		waitGroupGone(cmd.Process.Pid, 2*time.Second)
	}
	cause := context.Cause(replyCtx)
	var tooLong *pausedTooLongError
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.As(cause, &tooLong):
		return "", tooLong
	case errors.Is(cause, errReplyTimeout), errors.Is(readErr, context.DeadlineExceeded):
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

// childEnv is environ without the agent's Conch credentials and settings. The
// runtime's output is posted to a channel and a person's message can ask the
// model to print its environment, so the command must not hold the token.
// Everything else (HOME, PATH, the runtime's own login) passes unchanged.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "CONCH_ADAPTER_") || strings.HasPrefix(name, "CONCH_BOT_") || name == "CONCH_TOKEN" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// readReply reads the command's output the way its profile says.
func (c commandRunner) readReply(ctx context.Context, stdout io.Reader, fx fixtureHandler) (string, error) {
	if c.reader == readerCodexJSONL {
		return c.readCodex(ctx, stdout, fx)
	}
	return c.readText(ctx, stdout, fx)
}

// readText reads the command's output line by line. With the fixture on, a
// line that is a marker is acted on and left out of the reply.
func (c commandRunner) readText(ctx context.Context, stdout io.Reader, fx fixtureHandler) (string, error) {
	var (
		lines []string
		size  int
	)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), maxReplyBytes)
	for scanner.Scan() {
		line := stripControl(scanner.Text())
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

// stripControl removes terminal control sequences from a line of reply text:
// CSI and OSC sequences whole, then any other C0 control (a bare ESC, BEL,
// CR) and DEL, keeping tab. Nothing is interpreted: a cursor move is dropped,
// not applied. The newline never reaches it, the scanner has split on it.
func stripControl(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == 0x1b && i+1 < len(s) && s[i+1] == '[':
			// CSI: parameter bytes, intermediate bytes, one final byte.
			i += 2
			for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
				i++
			}
			for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
				i++
			}
			// s[i] is the final byte, which the loop's i++ skips. A byte out
			// of the final range (a UTF-8 lead byte, another ESC) ends a
			// malformed sequence without being eaten, so it is looked at again.
			if i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i--
			}
		case ch == 0x1b && i+1 < len(s) && s[i+1] == ']':
			// OSC: up to BEL or ESC \; an unterminated one runs to the line end.
			i += 2
			for i < len(s) && s[i] != 0x07 {
				if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
					i++
					break
				}
				i++
			}
		case ch == '\t' || (ch >= 0x20 && ch != 0x7f):
			b.WriteByte(ch)
		}
	}
	return b.String()
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
