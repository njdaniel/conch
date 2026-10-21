package voicehelper

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// This test binary is also the fake helper: started with "managed" as its
// first argument, as the supervisor starts conch-voice, it speaks the
// protocol from scripted behaviour instead of running the tests (v1, or v2
// when its hello offers v2 and the owner's start is in v2); started with
// "devices" or "config", as a one-shot run starts it, it answers those
// commands as scripted. No microphone, PipeWire or LiveKit is involved.

// fakePath is a private copy of this binary that passes the trust checks of
// §10 wherever the build put the original.
var fakePath string

// fixtureDir is the shared fixture directory, absolute, for the fake (whose
// working directory is /). fixtureDirV2 is v2's, beside it.
var fixtureDir, fixtureDirV2 string

// v2Dir is the v2 fixture directory beside the v1 directory dir.
func v2Dir(dir string) string { return filepath.Join(filepath.Dir(dir), "voice-control-v2") }

// bothVersions is what a v2 helper's hello offers.
const bothVersions = schema.VoiceControlSchemaV1 + "," + schema.VoiceControlSchemaV2

// mismatch is what the fake writes to standard error when the supervisor did
// something the script did not allow; the tests look for it in Diagnostics.
const mismatch = "fake: MISMATCH"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case schema.VoiceControlSubcommand:
			os.Exit(fakeMain(os.Args[2:]))
		case "devices", "config":
			os.Exit(fakeOneShot(os.Args[1:]))
		}
	}
	code, err := runTests(m)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "voicehelper tests:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runTests(m *testing.M) (int, error) {
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", "pkg", "schema", "testdata", "voice-control"))
	if err != nil {
		return 0, err
	}
	fixtureDir, fixtureDirV2 = abs, v2Dir(abs)
	// Under -race a process sleeps a second as it exits; the fakes inherit
	// this, so that the stop bounds measure the supervisor and not that.
	if err := os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0")); err != nil {
		return 0, err
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	dir, err := safeTestDir(self)
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	safeBase = dir
	fakePath = filepath.Join(dir, HelperName)
	return m.Run(), nil
}

// safeBase is a directory the trust checks accept: it and every parent up to
// / are the user's or root's and not writable by group or others. The fake
// helper and the tests' own directories live in it.
var safeBase string

// safeTestDir makes a private directory where a copy of the test binary
// passes the trust checks. The system's temporary directory is tried first;
// /tmp is writable by everyone, so it fails them, and the user's cache
// directory is tried next.
func safeTestDir(self string) (string, error) {
	bases := []string{os.TempDir()}
	if cache, err := os.UserCacheDir(); err == nil {
		bases = append(bases, cache)
	}
	for _, base := range bases {
		dir, err := os.MkdirTemp(base, "conch-voicehelper-test-")
		if err != nil {
			continue
		}
		helper := filepath.Join(dir, HelperName)
		if err := copyFile(self, helper, 0o700); err == nil {
			if _, err := trusted(helper); err == nil {
				return dir, nil
			}
		}
		_ = os.RemoveAll(dir)
	}
	return "", fmt.Errorf("none of %q is a directory the trust checks accept (owned by you or root, and neither it nor a parent writable by group or others)", bases)
}

func copyFile(from, to string, mode os.FileMode) error {
	src, err := os.Open(from) // #nosec G304 -- this test binary
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) // #nosec G304 -- a file in the test's own temporary directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

// fakeOptions is the fake's script, from its arguments.
type fakeOptions struct {
	fixtures        string
	versions        string
	noHello         bool
	exitBeforeHello int
	stateFirst      bool
	replay          string
	dump            string
	noAckStart      bool
	ignore          string
	refuse          string
	after           string
	onStop          string
	ackDelay        time.Duration
	maxInFlight     int
	failWith        string
	failOn          string
	linger          time.Duration
	record          string
	talkMode        string
	absent          string
	badV2           string
}

func parseFakeOptions(args []string) (fakeOptions, error) {
	var o fakeOptions
	fs := flag.NewFlagSet("fake", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.fixtures, "fixtures", "", "the shared fixture directory")
	fs.StringVar(&o.versions, "versions", schema.VoiceControlSchemaV1, "versions the hello offers, comma separated")
	fs.BoolVar(&o.noHello, "no-hello", false, "never write a hello, and ignore SIGTERM")
	fs.IntVar(&o.exitBeforeHello, "exit-before-hello", -1, "exit with this status before the hello")
	fs.BoolVar(&o.stateFirst, "state-first", false, "write a state before the hello")
	fs.StringVar(&o.replay, "replay", "", "replay this transcript's helper side")
	fs.StringVar(&o.dump, "dump", "", "write what the fake was started with to this file")
	fs.BoolVar(&o.noAckStart, "no-ack-start", false, "never answer the start")
	fs.StringVar(&o.ignore, "ignore", "", "never answer commands of this kind")
	fs.StringVar(&o.refuse, "refuse", "", "refuse commands of kind:code")
	fs.StringVar(&o.after, "after", "", "what to do wrong once connected")
	fs.StringVar(&o.onStop, "on-stop", "normal", "normal, hang-until-term or ignore-term")
	fs.DurationVar(&o.ackDelay, "ack-delay", 0, "wait this long before handling each command")
	fs.IntVar(&o.maxInFlight, "max-in-flight", 0, "fail if more commands than this are unanswered")
	fs.StringVar(&o.failWith, "fail-with", "", "once connected, fail with this reason")
	fs.StringVar(&o.failOn, "fail-on", "", "on a command of kind, fail with reason (kind:reason), unanswered")
	fs.DurationVar(&o.linger, "linger", 0, "after failing for -fail-on, wait this long before exiting")
	fs.StringVar(&o.record, "record", "", "append every command line read to this file")
	fs.StringVar(&o.talkMode, "talk-mode", string(schema.VoiceControlTalkModePushToTalk), "in v2, the talk mode voice.toml sets")
	fs.StringVar(&o.absent, "absent", "", "in v2, device names that are not there, comma separated")
	fs.StringVar(&o.badV2, "bad-v2", "", "in v2, once connected, write the line of this v2 event case")
	return o, fs.Parse(args)
}

// fake is the fake helper's event writer and push-to-talk state.
type fake struct {
	o        fakeOptions
	events   *os.File
	seq      int64
	muted    bool
	deafened bool
	read     atomic.Int64 // command lines read
	answered atomic.Int64 // acks written
	// v2 is whether the owner's start chose conch.voice_control.v2: every
	// line after it is read and written in v2.
	v2 bool
	// In v2: the talk mode in force, the devices the owner named ("" for
	// those voice.toml names, which are there), and the names of devices
	// that are not there.
	talkMode      schema.VoiceControlTalkMode
	input, output string
	absent        []string
}

// fakeCommand is one command as the fake read it, in either version: the
// fake's own view of the schema's types, not a wire shape.
type fakeCommand struct {
	id              int64
	kind            schema.VoiceControlCommandKind
	muted, deafened bool // set_mute's or set_deafen's value, or the start's
	input, output   string
	talkMode        schema.VoiceControlTalkMode
}

// parse reads a command line with the reader of the session's version.
func (f *fake) parse(line []byte) (fakeCommand, error) {
	if f.v2 {
		c, err := schema.ParseVoiceControlCommandV2(line)
		if err != nil {
			return fakeCommand{}, err
		}
		fc := fakeCommand{id: c.ID(), kind: c.Kind()}
		switch {
		case c.Start != nil:
			fc.muted, fc.deafened = c.Start.Muted, c.Start.Deafened
		case c.SetMute != nil:
			fc.muted = c.SetMute.Muted
		case c.SetDeafen != nil:
			fc.deafened = c.SetDeafen.Deafened
		case c.SetDevices != nil:
			fc.input, fc.output = c.SetDevices.Input, c.SetDevices.Output
		case c.SetTalkMode != nil:
			fc.talkMode = c.SetTalkMode.TalkMode
		}
		return fc, nil
	}
	c, err := schema.ParseVoiceControlCommand(line)
	if err != nil {
		return fakeCommand{}, err
	}
	fc := fakeCommand{id: c.ID(), kind: c.Kind()}
	switch {
	case c.Start != nil:
		fc.muted, fc.deafened = c.Start.Muted, c.Start.Deafened
	case c.SetMute != nil:
		fc.muted = c.SetMute.Muted
	case c.SetDeafen != nil:
		fc.deafened = c.SetDeafen.Deafened
	}
	return fc, nil
}

// parseFirst reads the first command line with the reader of the version it
// names, when the hello offered that version (§20.2), and fixes the session's
// version by it.
func (f *fake) parseFirst(line []byte, versions []string) (fakeCommand, error) {
	cmd, err := f.parse(line)
	var vcErr *schema.VoiceControlError
	if errors.As(err, &vcErr) && vcErr.Code == schema.VoiceControlErrorUnknownSchema && slices.Contains(versions, schema.VoiceControlSchemaV2) {
		f.v2 = true
		cmd, err = f.parse(line)
	}
	return cmd, err
}

func fakeMain(args []string) int {
	// A backstop: a fake left behind by a failed test does not outlive it.
	time.AfterFunc(60*time.Second, func() { os.Exit(4) })
	o, err := parseFakeOptions(args)
	if err != nil {
		return fakeFail("bad arguments: %v", err)
	}
	term := make(chan os.Signal, 1)
	if o.noHello || o.onStop == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(term, syscall.SIGTERM)
	}
	f := &fake{o: o, events: os.NewFile(uintptr(schema.VoiceControlEventFD), "events")}
	if o.replay != "" {
		return f.replay(term)
	}
	switch {
	case o.exitBeforeHello >= 0:
		return o.exitBeforeHello
	case o.noHello:
		sleepForever()
	case o.stateFirst:
		f.state(schema.VoiceControlPhaseStarting, "")
		sleepForever()
	}
	versions := strings.Split(o.versions, ",")
	f.emit(&schema.VoiceControlHelloV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlEventHello, Versions: versions, HelperVersion: "fake 1.0"})
	cmds := f.readCommands()
	if !slices.Contains(versions, schema.VoiceControlSchemaV1) {
		// Told nothing: the owner must write nothing and close.
		if line, ok := <-cmds; ok {
			return fakeFail("the owner wrote %q to a helper it cannot speak to", line)
		}
		return 0
	}
	first, ok := <-cmds
	if !ok {
		return 0
	}
	cmd, err := f.parseFirst(first, versions)
	if err != nil || cmd.kind != schema.VoiceControlCommandStart {
		return fakeFail("the first command is not a valid start: %v", err)
	}
	if o.dump != "" {
		if err := writeDump(o.dump, first); err != nil {
			return fakeFail("dump: %v", err)
		}
	}
	if o.noAckStart {
		for range cmds {
		}
		return 0
	}
	f.muted, f.deafened = cmd.muted, cmd.deafened
	f.state(schema.VoiceControlPhaseStarting, "")
	f.ack(cmd.id, "")
	// voice.toml is read after the start's ack (§8.1): only now is the talk
	// mode it sets in force.
	f.talkMode = schema.VoiceControlTalkMode(o.talkMode)
	if o.absent != "" {
		f.absent = strings.Split(o.absent, ",")
	}
	f.state(schema.VoiceControlPhaseConnecting, "")
	f.state(schema.VoiceControlPhaseConnected, "")
	if code, done := f.misbehave(term); done {
		return code
	}
	if o.badV2 != "" {
		l := caseLine(f.fixtureV2("cases.json"), o.badV2)
		f.raw(l)
		// A line the owner skips, rather than refuses, sets the seq the
		// fake's next event follows.
		if ev, err := schema.ParseVoiceControlEventV2(l); err == nil {
			f.seq = max(f.seq, ev.Seq)
		}
	}
	if o.failWith != "" {
		reason := schema.VoiceControlReason(o.failWith)
		f.state(schema.VoiceControlPhaseStopping, reason)
		f.state(schema.VoiceControlPhaseFailed, reason)
		return 1
	}
	return f.serve(cmds, term)
}

// serve answers commands until the session ends.
func (f *fake) serve(cmds <-chan []byte, term <-chan os.Signal) int {
	leaving := false
	for {
		select {
		case line, ok := <-cmds:
			if !ok {
				if f.o.onStop != "normal" {
					cmds = nil
					continue
				}
				reason := schema.VoiceControlReasonOwnerLost
				if leaving {
					reason = schema.VoiceControlReasonLeft
				} else {
					f.state(schema.VoiceControlPhaseStopping, reason)
				}
				f.state(schema.VoiceControlPhaseStopped, reason)
				return 0
			}
			if f.o.ackDelay > 0 {
				time.Sleep(f.o.ackDelay)
			}
			cmd, err := f.parse(line)
			if err != nil {
				return fakeFail("an invalid command: %v", err)
			}
			if string(cmd.kind) == f.o.ignore {
				continue
			}
			if kind, reason, ok := strings.Cut(f.o.failOn, ":"); ok && kind == string(cmd.kind) {
				f.state(schema.VoiceControlPhaseStopping, schema.VoiceControlReason(reason))
				f.state(schema.VoiceControlPhaseFailed, schema.VoiceControlReason(reason))
				time.Sleep(f.o.linger)
				return 1
			}
			if kind, code, ok := strings.Cut(f.o.refuse, ":"); ok && kind == string(cmd.kind) {
				f.ack(cmd.id, schema.VoiceControlErrorCode(code))
				continue
			}
			changes := cmd.kind == schema.VoiceControlCommandSetMute || cmd.kind == schema.VoiceControlCommandSetDeafen ||
				cmd.kind == schema.VoiceControlCommandSetDevices || cmd.kind == schema.VoiceControlCommandSetTalkMode
			if changes && leaving {
				f.ack(cmd.id, schema.VoiceControlErrorStopping)
				continue
			}
			switch cmd.kind {
			case schema.VoiceControlCommandSetMute:
				if f.muted != cmd.muted {
					f.muted = cmd.muted
					f.state(schema.VoiceControlPhaseConnected, "")
				}
			case schema.VoiceControlCommandSetDeafen:
				if f.deafened != cmd.deafened {
					f.deafened = cmd.deafened
					f.state(schema.VoiceControlPhaseConnected, "")
				}
			case schema.VoiceControlCommandSetDevices:
				// A state only when what it shows changed: a device that is
				// there for another that is there changes nothing (§20.5).
				before := f.devicesMissing()
				if cmd.input != "" {
					f.input = cmd.input
				}
				if cmd.output != "" {
					f.output = cmd.output
				}
				if f.devicesMissing() != before {
					f.state(schema.VoiceControlPhaseConnected, "")
				}
			case schema.VoiceControlCommandSetTalkMode:
				if f.talkMode != cmd.talkMode {
					f.talkMode = cmd.talkMode
					f.state(schema.VoiceControlPhaseConnected, "")
				}
			case schema.VoiceControlCommandGetState:
				if f.o.after == "gap" {
					// The deafen key, pressed while the owner was not
					// reading: only this snapshot shows it.
					f.deafened = true
				}
				phase := schema.VoiceControlPhaseConnected
				if leaving {
					phase = schema.VoiceControlPhaseStopping
				}
				f.state(phase, reasonIf(leaving, schema.VoiceControlReasonLeft))
			case schema.VoiceControlCommandLeave:
				if !leaving {
					leaving = true
					f.state(schema.VoiceControlPhaseStopping, schema.VoiceControlReasonLeft)
				}
				f.ack(cmd.id, "")
				if f.o.onStop == "normal" {
					f.state(schema.VoiceControlPhaseStopped, schema.VoiceControlReasonLeft)
					return 0
				}
				continue
			}
			if f.o.after == "ack-wrong-id" {
				f.ack(cmd.id+1, "")
				continue
			}
			f.ack(cmd.id, "")
		case <-term:
			if !leaving {
				f.state(schema.VoiceControlPhaseStopping, schema.VoiceControlReasonSignal)
			}
			f.state(schema.VoiceControlPhaseStopped, schema.VoiceControlReasonSignal)
			return 0
		}
	}
}

func reasonIf(cond bool, r schema.VoiceControlReason) schema.VoiceControlReason {
	if cond {
		return r
	}
	return ""
}

// misbehave does what -after asks once connected. done says the fake has
// finished, with code.
func (f *fake) misbehave(term <-chan os.Signal) (code int, done bool) {
	switch f.o.after {
	case "oversized":
		f.raw(f.fixture("lines/event-too-long.txt"))
	case "unending":
		// A line that passes the bound and never ends, the descriptor kept
		// open: it is refused at the bound, not at a line feed.
		_, _ = f.events.Write(bytes.Repeat([]byte("u"), 2*schema.VoiceControlMaxLineBytes))
		<-term
		return 0, true
	case "native":
		f.raw([]byte("NVIDIA: could not open the device file"))
	case "seq-repeat":
		f.seq--
		f.state(schema.VoiceControlPhaseConnected, "")
	case "second-hello":
		f.seq = 0
		f.emit(&schema.VoiceControlHelloV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlEventHello, Versions: []string{schema.VoiceControlSchemaV1}, HelperVersion: "fake 1.0"})
	case "after-final":
		f.state(schema.VoiceControlPhaseFailed, schema.VoiceControlReasonInternalError)
		f.state(schema.VoiceControlPhaseFailed, schema.VoiceControlReasonInternalError)
		return 1, true
	case "unknown-kind":
		// The conformance case's roster event, seq 7: skipped.
		f.raw(caseLine(f.fixture("cases.json"), "an event of a kind this version does not know"))
		f.seq = 7
		f.state(schema.VoiceControlPhaseConnected, "")
	case "gap":
		// Three events dropped for a slow reader; the mute key was among
		// them.
		f.seq += 3
		f.muted = true
		f.state(schema.VoiceControlPhaseConnected, "")
	case "crash":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		sleepForever()
	case "exit101":
		return 101, true
	case "exit-silently":
		return 0, true
	case "close-stdin":
		_ = os.Stdin.Close()
		<-term
		return 0, true
	case "close-events":
		_ = f.events.Close()
		<-term
		return 0, true
	case "noise":
		// Native output, a protocol-looking line among it, and a partial
		// line: none of it may reach the owner.
		st := f.snapshot(schema.VoiceControlPhaseConnected, "")
		st.Seq = f.seq + 1
		st.Blocked, st.Transmitting = []schema.VoiceControlBlock{}, true
		line, _ := schema.MarshalVoiceControlLine(&st)
		junk := bytes.Repeat([]byte("NVIDIA: \x1b[31mnoise\x1b[0m without a line feed "), 8000)
		for i := 0; i < 4; i++ {
			_, _ = os.Stdout.Write(line)
			_, _ = os.Stdout.Write(junk)
		}
	case "log-spam":
		// The login, from CONCH_TOKEN, or as a helper with a stored login
		// would have read it: a stand-in file in the configuration
		// directory.
		token := os.Getenv("CONCH_TOKEN")
		if stored, err := os.ReadFile(filepath.Join(os.Getenv("CONCH_CONFIG_DIR"), "fake-stored-login")); err == nil { // #nosec G304 G703 -- the test's own configuration directory
			token = string(stored)
		}
		for i := 0; i < 100; i++ {
			_, _ = fmt.Fprintf(os.Stderr, "line %d \x1b]0;title\x07 \u202emirrored\u202c token=%s %s\n", i, token, strings.Repeat("x", 5000*(i%2)))
		}
		_, _ = fmt.Fprintln(os.Stderr, "fake: last line")
	}
	return 0, false
}

func (f *fake) fixture(name string) []byte {
	data, err := os.ReadFile(filepath.Join(f.o.fixtures, filepath.FromSlash(name))) // #nosec G304 -- the shared fixture directory, named by the test
	if err != nil {
		os.Exit(fakeFail("fixture %s: %v", name, err))
	}
	return data
}

// caseLine is the line of the conformance case called name.
func caseLine(cases []byte, name string) []byte {
	var doc struct {
		Cases []struct {
			Name string `json:"name"`
			Line string `json:"line"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(cases, &doc); err != nil {
		os.Exit(fakeFail("cases.json: %v", err))
	}
	for _, c := range doc.Cases {
		if c.Name == name {
			return []byte(c.Line)
		}
	}
	os.Exit(fakeFail("no case %q", name))
	return nil
}

// readCommands reads standard input a line at a time, eagerly, so that the
// fake knows how many commands are unanswered. With -record each line read is
// appended to that file before the fake acts on it, so that a test sees every
// byte the owner wrote.
func (f *fake) readCommands() <-chan []byte {
	out := make(chan []byte, 128)
	go func() {
		defer close(out)
		lines := newLineReader(os.Stdin)
		for {
			line, _, err := lines.next()
			if err != nil {
				if len(line) > 0 {
					f.recordLine(line)
					out <- line
				}
				return
			}
			if n := f.read.Add(1) - f.answered.Load(); f.o.maxInFlight > 0 && n > int64(f.o.maxInFlight) {
				os.Exit(fakeFail("%d commands unanswered, more than %d", n, f.o.maxInFlight))
			}
			f.recordLine(line)
			out <- line
		}
	}()
	return out
}

func (f *fake) recordLine(line []byte) {
	if f.o.record == "" {
		return
	}
	file, err := os.OpenFile(f.o.record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- a file in the test's own temporary directory
	if err != nil {
		os.Exit(fakeFail("record: %v", err))
	}
	_, err = file.Write(append(append([]byte(nil), line...), '\n'))
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Exit(fakeFail("record: %v", err))
	}
}

func (f *fake) fixtureV2(name string) []byte {
	data, err := os.ReadFile(filepath.Join(v2Dir(f.o.fixtures), filepath.FromSlash(name))) // #nosec G304 -- the shared fixture directory, named by the test
	if err != nil {
		os.Exit(fakeFail("fixture %s: %v", name, err))
	}
	return data
}

func (f *fake) emit(msg schema.VoiceControlMessage) {
	f.seq++
	switch m := msg.(type) {
	case *schema.VoiceControlHelloV1:
		m.Seq = f.seq
	case *schema.VoiceControlAckV1:
		m.Seq = f.seq
	case *schema.VoiceControlStateV1:
		m.Seq = f.seq
	case *schema.VoiceControlAckV2:
		m.Seq = f.seq
	case *schema.VoiceControlStateV2:
		m.Seq = f.seq
	}
	line, err := schema.MarshalVoiceControlLine(msg)
	if err != nil {
		os.Exit(fakeFail("the fake made a bad event: %v", err))
	}
	_, _ = f.events.Write(line)
}

func (f *fake) raw(line []byte) {
	_, _ = f.events.Write(append(bytes.TrimRight(line, "\n"), '\n'))
}

func (f *fake) ack(id int64, code schema.VoiceControlErrorCode) {
	// Counted before it is written: once written, the owner may read it and
	// write the next command before this goroutine runs again.
	f.answered.Add(1)
	if f.v2 {
		f.emit(&schema.VoiceControlAckV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlEventAck, ID: id, OK: code == "", Error: code})
		return
	}
	f.emit(&schema.VoiceControlAckV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlEventAck, ID: id, OK: code == "", Error: code})
}

func (f *fake) state(phase schema.VoiceControlPhase, reason schema.VoiceControlReason) {
	st := f.snapshot(phase, reason)
	if f.v2 {
		st2 := f.snapshotV2(st)
		f.emit(&st2)
		return
	}
	f.emit(&st)
}

// missing reports whether the device named is not there; "" is the one
// voice.toml names, which is.
func (f *fake) missing(name string) bool {
	return name != "" && slices.Contains(f.absent, name)
}

// devicesMissing is whether the microphone and the speakers in force are
// missing.
func (f *fake) devicesMissing() [2]bool {
	return [2]bool{f.missing(f.input), f.missing(f.output)}
}

// snapshot is a consistent state for phase, with the fake's mute and deafen.
func (f *fake) snapshot(phase schema.VoiceControlPhase, reason schema.VoiceControlReason) schema.VoiceControlStateV1 {
	st := schema.VoiceControlStateV1{
		Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlEventState, Phase: phase, Reason: reason,
		Capture: schema.VoiceControlCaptureOff, PTT: schema.VoiceControlPTTReady, Muted: f.muted, Deafened: f.deafened,
		Blocked: []schema.VoiceControlBlock{},
	}
	micOn := func() {
		// A microphone that is not there is unavailable, and nothing takes
		// its place (§20.5).
		st.Capture = schema.VoiceControlCaptureOn
		if f.missing(f.input) {
			st.Capture = schema.VoiceControlCaptureUnavailable
			st.Blocked = append(st.Blocked, schema.VoiceControlBlockNoMicrophone)
		}
	}
	switch phase {
	case schema.VoiceControlPhaseStarting:
		st.PTT = schema.VoiceControlPTTUnknown
		fallthrough
	case schema.VoiceControlPhaseConnecting:
		st.Blocked = append(st.Blocked, schema.VoiceControlBlockNotConnected, schema.VoiceControlBlockNoPublishGrant, schema.VoiceControlBlockNoMicrophone)
	case schema.VoiceControlPhaseConnected:
		micOn()
	case schema.VoiceControlPhaseStopping:
		st.Blocked = append(st.Blocked, schema.VoiceControlBlockNotConnected)
		micOn()
	default:
		st.Blocked = append(st.Blocked, schema.VoiceControlBlockNotConnected)
	}
	if f.deafened {
		st.Blocked = append(st.Blocked, schema.VoiceControlBlockDeafened)
	}
	if f.muted {
		st.Blocked = append(st.Blocked, schema.VoiceControlBlockMuted)
	}
	return st
}

// snapshotV2 is st in v2: the speakers' stream beside the microphone's, and
// the talk mode in force, which is push-to-talk while starting, before
// voice.toml has been read.
func (f *fake) snapshotV2(st schema.VoiceControlStateV1) schema.VoiceControlStateV2 {
	out := schema.VoiceControlStateV2{
		Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlEventState, Phase: st.Phase, Reason: st.Reason,
		RetryInMS: st.RetryInMS, Capture: st.Capture, Playback: schema.VoiceControlPlaybackOff, Transmitting: st.Transmitting,
		Muted: st.Muted, Deafened: st.Deafened, Blocked: st.Blocked, TalkMode: f.talkMode, PTT: st.PTT, PTTProblem: st.PTTProblem,
	}
	if st.Phase == schema.VoiceControlPhaseStarting || out.TalkMode == "" {
		out.TalkMode = schema.VoiceControlTalkModePushToTalk
	}
	if st.Phase != schema.VoiceControlPhaseStarting && !st.Phase.Final() {
		out.Playback = schema.VoiceControlPlaybackOn
		if f.missing(f.output) {
			out.Playback = schema.VoiceControlPlaybackUnavailable
		}
	}
	return out
}

// replay plays a transcript's helper side, and holds the owner to its side:
// each command byte for byte, the end of input where it closes standard
// input (after a leave, which the supervisor writes first), and a SIGTERM
// where it signals.
func (f *fake) replay(term <-chan os.Signal) int {
	data, err := os.ReadFile(f.o.replay) // #nosec G304 -- a transcript named by the test
	if err != nil {
		return fakeFail("transcript: %v", err)
	}
	var doc struct {
		Steps []struct {
			From    string          `json:"from"`
			Message json.RawMessage `json:"message"`
			Close   string          `json:"close"`
			Signal  string          `json:"signal"`
			Exit    *int            `json:"exit"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fakeFail("transcript: %v", err)
	}
	cmds := f.readCommands()
	started := false
	for i, step := range doc.Steps {
		var line bytes.Buffer
		if step.Message != nil {
			if err := json.Compact(&line, step.Message); err != nil {
				return fakeFail("step %d: %v", i, err)
			}
		}
		switch {
		case step.Exit != nil:
			return *step.Exit
		case step.From == "helper":
			_, _ = f.events.Write(append(line.Bytes(), '\n'))
		case step.Close != "":
			for got := range cmds {
				cmd, err := f.parse(got)
				if !started || err != nil || cmd.kind != schema.VoiceControlCommandLeave {
					return fakeFail("step %d: %q where standard input was to end", i, got)
				}
			}
		case step.Signal != "":
			<-term
		default:
			got, ok := <-cmds
			if !ok {
				return fakeFail("step %d: the end of input where %s was due", i, line.Bytes())
			}
			if !bytes.Equal(got, line.Bytes()) {
				return fakeFail("step %d: read %s, want %s", i, got, line.Bytes())
			}
			if !started {
				// The start fixes the session's version (§20.2).
				_, err := schema.ParseVoiceControlCommandV2(got)
				f.v2 = err == nil
			}
			started = true
		}
	}
	return fakeFail("the transcript has no exit")
}

// writeDump records what the fake was started with, one key=value a line.
func writeDump(path string, start []byte) error {
	var b strings.Builder
	line := func(k, v string) { b.WriteString(k + "=" + v + "\n") }
	line("pid", fmt.Sprint(os.Getpid()))
	line("pgid", fmt.Sprint(syscall.Getpgrp()))
	if wd, err := os.Getwd(); err == nil {
		line("cwd", wd)
	}
	for _, a := range os.Args {
		line("arg", a)
	}
	for _, e := range os.Environ() {
		line("env", e)
	}
	for fd := 0; fd <= 3; fd++ {
		target, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		line(fmt.Sprintf("fd%d", fd), target)
	}
	line("start", string(start))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dump is a fake's writeDump, read back.
type dump map[string][]string

func readDump(t *testing.T, path string) dump {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a file in the test's own temporary directory
	if err != nil {
		t.Fatalf("the fake left no dump: %v", err)
	}
	d := dump{}
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		k, v, _ := strings.Cut(l, "=")
		d[k] = append(d[k], v)
	}
	return d
}

func (d dump) one(k string) string {
	if len(d[k]) == 0 {
		return ""
	}
	return d[k][0]
}

// env is the dumped environment as a map.
func (d dump) env() map[string]string {
	m := map[string]string{}
	for _, kv := range d["env"] {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func fakeFail(format string, args ...any) int {
	_, _ = fmt.Fprintf(os.Stderr, mismatch+": "+format+"\n", args...)
	return 3
}

func sleepForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// oneShotOptions is the one-shot fake's script, from the arguments after the
// command's own.
type oneShotOptions struct {
	dump   string
	doc    string
	raw    string
	flood  int
	stderr string
	exit   int
	hang   bool
}

// fakeOneShot answers `devices --json` and `config set <key> <value>` as
// scripted. A command line of another shape is refused as conch-voice
// refuses one: exit status 2.
func fakeOneShot(args []string) int {
	time.AfterFunc(60*time.Second, func() { os.Exit(4) })
	var rest []string
	switch {
	case len(args) >= 2 && args[0] == "devices" && args[1] == "--json":
		rest = args[2:]
	case len(args) >= 5 && args[0] == "config" && args[1] == "set" && args[3] == "--":
		rest = args[5:]
	default:
		_, _ = fmt.Fprintf(os.Stderr, "error: unexpected arguments %q\n", args)
		return 2
	}
	var o oneShotOptions
	fs := flag.NewFlagSet("fake", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.dump, "dump", "", "write what the fake was started with to this file")
	fs.StringVar(&o.doc, "doc", "", "write this device-list fixture, encoded by MarshalVoiceDevices")
	fs.StringVar(&o.raw, "raw", "", "write this to standard output as it is")
	fs.IntVar(&o.flood, "flood", 0, "write this many bytes to standard output")
	fs.StringVar(&o.stderr, "stderr", "", "write this to standard error")
	fs.IntVar(&o.exit, "exit", 0, "exit with this status")
	fs.BoolVar(&o.hang, "hang", false, "ignore SIGTERM and never end")
	if err := fs.Parse(rest); err != nil {
		return fakeFail("bad arguments: %v", err)
	}
	if o.dump != "" {
		if err := writeDump(o.dump, nil); err != nil {
			return fakeFail("dump: %v", err)
		}
	}
	if o.stderr != "" {
		_, _ = io.WriteString(os.Stderr, o.stderr)
	}
	if o.doc != "" {
		data, err := os.ReadFile(o.doc) // #nosec G304 -- a fixture named by the test
		if err != nil {
			return fakeFail("fixture: %v", err)
		}
		doc, err := schema.ParseVoiceDevices(data)
		if err != nil {
			return fakeFail("fixture: %v", err)
		}
		line, err := schema.MarshalVoiceDevices(doc)
		if err != nil {
			return fakeFail("fixture: %v", err)
		}
		_, _ = os.Stdout.Write(line)
	}
	if o.raw != "" {
		_, _ = io.WriteString(os.Stdout, o.raw)
	}
	if o.flood > 0 {
		chunk := bytes.Repeat([]byte("x"), 32*1024)
		for left := o.flood; left > 0; left -= len(chunk) {
			if _, err := os.Stdout.Write(chunk[:min(left, len(chunk))]); err != nil {
				// The reader stopped reading and was killed or closed.
				return 0
			}
		}
	}
	if o.hang {
		signal.Ignore(syscall.SIGTERM)
		sleepForever()
	}
	return o.exit
}
