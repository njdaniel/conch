package voicehelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// One-shot runs (issue #652): the TUI lists the microphones and speakers with
// `conch-voice devices --json` and saves a choice with `conch-voice config set`,
// each a short process of its own beside the managed helper. They are found
// and trusted exactly as the managed helper is (§10), started the same way (no
// shell, working directory /, a process group of their own), and given the
// helper's environment (§9) with one difference: CONCH_TOKEN is never passed,
// because neither command needs a login. Standard input is /dev/null, standard
// output is read up to schema.VoiceDevicesMaxBytes and no further, standard
// error is kept as a short, sanitized tail, and a run that has not ended within
// OneShotTimeout is killed and reaped.

// OneShotTimeout bounds a one-shot run, from its start to its exit.
const OneShotTimeout = 10 * time.Second

// DeviceKey is the device `config set` saves.
type DeviceKey string

// The devices `config set` saves, as its command line spells them.
const (
	DeviceInput  DeviceKey = "audio.input"
	DeviceOutput DeviceKey = "audio.output"
)

// DefaultDevice is the name that saves PipeWire's default, which follows the
// default as it changes.
const DefaultDevice = "default"

// Ways a one-shot run can end without what was asked (RunError.Reason).
const (
	// RunTimedOut: it had not ended within OneShotTimeout, and was killed.
	RunTimedOut = "timed_out"
	// RunTooMuchOutput: it wrote more than schema.VoiceDevicesMaxBytes to
	// standard output, and was killed.
	RunTooMuchOutput = "too_much_output"
	// RunUnsupported: it refused its command line (exit status 2): a
	// conch-voice older than the command.
	RunUnsupported = "unsupported"
	// RunFailed: any other exit status but 0, or a signal.
	RunFailed = "failed"
	// RunBadOutput: it exited 0, and what it wrote is not a device list
	// ParseVoiceDevices accepts.
	RunBadOutput = "bad_output"
)

// usageExit is the exit status conch-voice gives for a command line it does
// not take, an unknown subcommand or option included.
const usageExit = 2

// RunError is a one-shot run that started and did not give what was asked.
// Its text never holds the token or the output.
type RunError struct {
	// Reason is one of the Run constants.
	Reason string
	// ExitCode is the run's exit status, or -1 when it did not exit by
	// itself.
	ExitCode int
	// Line is the last line it wrote to standard error, sanitized; "" for
	// none.
	Line string
}

func (e *RunError) Error() string {
	msg := "voicehelper: conch-voice " + e.Reason
	if e.ExitCode >= 0 {
		msg += " (exit status " + strconv.Itoa(e.ExitCode) + ")"
	}
	if e.Line != "" {
		msg += ": " + e.Line
	}
	return msg
}

// ErrBadDeviceName: the name cannot be saved in voice.toml as it stands. It is
// refused before anything runs.
var ErrBadDeviceName = errors.New("voicehelper: a device name must be 1 to 255 bytes, one line, with no quotation mark, backslash or control character")

// DeviceList is what `devices --json` gave.
type DeviceList struct {
	// Devices is the document, read by schema.ParseVoiceDevices.
	Devices schema.VoiceDevicesV1
	// Diagnostics is the last lines it wrote to standard error, sanitized:
	// why voice.toml was refused, for one.
	Diagnostics []string
}

// OneShot runs conch-voice for one command and waits for it. Its methods are
// safe to call from any goroutine, and each run is independent of the others
// and of any Supervisor.
type OneShot struct {
	configDir  string
	token      string
	helperPath string
	find       finder
	// timeout and testArgs are for tests: a shorter bound, and arguments put
	// after the command's own to script the fake helper.
	timeout  time.Duration
	testArgs []string
}

// NewOneShot makes a runner for p's configuration directory, finding the
// helper as o says. Only p.ConfigDir is passed on; p.Token, whatever p's
// credential, is used only to keep it out of what the runs report.
func NewOneShot(p Profile, o Options) *OneShot {
	return &OneShot{
		configDir:  p.ConfigDir,
		token:      p.Token,
		helperPath: o.HelperPath,
		find:       defaultFinder,
		timeout:    OneShotTimeout,
	}
}

// Devices runs `conch-voice devices --json` and reads what it writes.
func (o *OneShot) Devices(ctx context.Context) (DeviceList, error) {
	res, err := o.exec(ctx, "devices", "--json")
	if err != nil {
		return DeviceList{}, err
	}
	if err := res.failure(); err != nil {
		return DeviceList{}, err
	}
	doc, err := schema.ParseVoiceDevices(res.stdout)
	if err != nil {
		return DeviceList{}, &RunError{Reason: RunBadOutput, ExitCode: res.exitCode, Line: res.lastLine()}
	}
	return DeviceList{Devices: doc, Diagnostics: res.stderr}, nil
}

// SetDevice runs `conch-voice config set <key> <name>`, which saves name, or
// PipeWire's default for DefaultDevice, in voice.toml. A key or a name that
// could not be saved is refused before anything runs.
func (o *OneShot) SetDevice(ctx context.Context, key DeviceKey, name string) error {
	if key != DeviceInput && key != DeviceOutput {
		return fmt.Errorf("voicehelper: %q is not a device config set saves", key)
	}
	if !DeviceNameFits(name) {
		return ErrBadDeviceName
	}
	// "--" ends the options: a device name may begin with "-" (PipeWire lets any
	// program name a node), and without it clap would read "-h" as help, print it
	// and exit 0, which would look like a save.
	res, err := o.exec(ctx, "config", "set", string(key), "--", name)
	if err != nil {
		return err
	}
	return res.failure()
}

// talkModeKey is the key `config set` saves the talk mode under (#654).
const talkModeKey = "audio.talk_mode"

// SetTalkMode runs `conch-voice config set audio.talk_mode <mode>`, which saves
// the talk mode in voice.toml: the next helper reads it as it starts, and a
// running v2 helper is told with Supervisor.SetTalkMode. A mode of no version
// is refused before anything runs. A conch-voice older than the key refuses
// its command line (RunUnsupported).
func (o *OneShot) SetTalkMode(ctx context.Context, mode schema.VoiceControlTalkMode) error {
	if !mode.Valid() {
		return fmt.Errorf("voicehelper: %q is not a talk mode: push_to_talk or open", mode)
	}
	res, err := o.exec(ctx, "config", "set", talkModeKey, "--", string(mode))
	if err != nil {
		return err
	}
	return res.failure()
}

// DeviceNameFits reports whether name can be saved in voice.toml as it
// stands: DefaultDevice, or a name under the device list's own rule, which is
// the rule conch-voice's command line holds it to.
func DeviceNameFits(name string) bool {
	if name == DefaultDevice {
		return true
	}
	// The schema's rule for a listed name, applied through its own check
	// so that there is one rule, not two.
	doc := schema.VoiceDevicesV1{
		Schema: schema.VoiceDevicesSchemaV1, PipeWire: schema.VoiceDevicesPipeWireOK,
		Config: schema.VoiceDevicesConfigOK, Microphones: []schema.VoiceDevice{{Name: name}}, Speakers: []schema.VoiceDevice{},
	}
	return doc.Validate() == nil
}

// oneShotResult is how a run that ended by itself ended.
type oneShotResult struct {
	stdout   []byte
	exitCode int
	// stderr is the last diagLines lines of standard error, sanitized.
	stderr []string
}

// failure is the RunError for a run that did not exit 0.
func (r oneShotResult) failure() error {
	switch r.exitCode {
	case 0:
		return nil
	case usageExit:
		return &RunError{Reason: RunUnsupported, ExitCode: r.exitCode, Line: r.lastLine()}
	}
	return &RunError{Reason: RunFailed, ExitCode: r.exitCode, Line: r.lastLine()}
}

// lastLine is the last line of standard error that says something.
func (r oneShotResult) lastLine() string {
	for i := len(r.stderr) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(r.stderr[i]); line != "" {
			return line
		}
	}
	return ""
}

// exec finds the helper and runs it with args: no shell, the resolved path as
// argv[0], working directory /, standard input /dev/null, its own process
// group, and only descriptors 0 to 2. It returns once the process has been
// reaped, whatever happened. A run that has not ended within the bound, that
// writes too much, or whose ctx ends, is killed first; only a run that ended
// by itself gives a result.
func (o *OneShot) exec(ctx context.Context, args ...string) (oneShotResult, error) {
	if err := ctx.Err(); err != nil {
		return oneShotResult{}, err
	}
	path, err := o.find.find(o.helperPath)
	if err != nil {
		return oneShotResult{}, err
	}
	var opened []*os.File
	closeAll := func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return oneShotResult{}, newError(ReasonSpawnFailed, "conch-voice could not be started: "+err.Error(), err)
	}
	opened = append(opened, stdoutR, stdoutW)
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll()
		return oneShotResult{}, newError(ReasonSpawnFailed, "conch-voice could not be started: "+err.Error(), err)
	}
	opened = append(opened, stderrR, stderrW)
	argv := append([]string{path}, args...)
	argv = append(argv, o.testArgs...)
	cmd := &exec.Cmd{
		Path: path,
		Args: argv,
		// The managed helper's environment, for a profile with no token to
		// pass on: whatever the TUI's credential, CONCH_TOKEN is not set.
		Env:         helperEnv(os.Environ(), Profile{ConfigDir: o.configDir}),
		Dir:         "/",
		Stdout:      stdoutW,
		Stderr:      stderrW,
		SysProcAttr: sysProcAttr(),
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return oneShotResult{}, newError(ReasonSpawnFailed, "conch-voice could not be started: "+err.Error(), err)
	}
	// The child has its own copies; holding ours would keep its pipes from
	// ending when it does.
	_ = stdoutW.Close()
	_ = stderrW.Close()

	overflow := make(chan struct{})
	var stdout []byte
	var tail []string
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		// One byte more than the bound tells a document at the bound from one
		// past it; the rest is never read into memory.
		buf, err := io.ReadAll(io.LimitReader(stdoutR, schema.VoiceDevicesMaxBytes+1))
		stdout = buf
		if err == nil && len(buf) > schema.VoiceDevicesMaxBytes {
			close(overflow)
		}
	}()
	go func() {
		defer readers.Done()
		lines := newLineReader(stderrR)
		for {
			line, _, err := lines.next()
			if err == nil || len(line) > 0 {
				if text := sanitizeDiag(line, o.token); text != "" {
					tail = append(tail, text)
					if len(tail) > diagLines {
						tail = tail[len(tail)-diagLines:]
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	timer := time.NewTimer(o.timeout)
	defer timer.Stop()
	var stopped error
	select {
	case <-exited:
	case <-timer.C:
		stopped = &RunError{Reason: RunTimedOut, ExitCode: -1}
	case <-overflow:
		stopped = &RunError{Reason: RunTooMuchOutput, ExitCode: -1}
	case <-ctx.Done():
		stopped = ctx.Err()
	}
	if stopped != nil {
		// Only the process this run started, and only until Wait has reaped
		// it: os.Process refuses a signal after that, so the pid cannot have
		// been reused.
		_ = cmd.Process.Kill()
		<-exited
	}

	// Something the helper started may still hold a write end; the readers
	// end here whatever it does.
	readersDone := make(chan struct{})
	go func() { readers.Wait(); close(readersDone) }()
	grace := time.NewTimer(drainGrace)
	select {
	case <-readersDone:
	case <-grace.C:
	}
	grace.Stop()
	_ = stdoutR.Close()
	_ = stderrR.Close()
	<-readersDone

	if stopped != nil {
		var re *RunError
		if errors.As(stopped, &re) {
			re.Line = oneShotResult{stderr: tail}.lastLine()
		}
		return oneShotResult{}, stopped
	}
	if len(stdout) > schema.VoiceDevicesMaxBytes {
		// It wrote too much and exited before the overflow was seen.
		return oneShotResult{}, &RunError{Reason: RunTooMuchOutput, ExitCode: cmd.ProcessState.ExitCode(), Line: oneShotResult{stderr: tail}.lastLine()}
	}
	return oneShotResult{stdout: stdout, exitCode: cmd.ProcessState.ExitCode(), stderr: tail}, nil
}
