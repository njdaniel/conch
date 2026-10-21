package voicehelper

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

// devicesFixture is a device-list fixture of #649, by its absolute path, for
// the fake (whose working directory is /).
func devicesFixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "pkg", "schema", "testdata", "voice-devices", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// newOneShot is a runner of the fake helper for p, scripted by args.
func newOneShot(p Profile, args ...string) *OneShot {
	o := NewOneShot(p, Options{HelperPath: fakePath})
	o.testArgs = args
	return o
}

func runErrOf(err error) *RunError {
	var re *RunError
	if errors.As(err, &re) {
		return re
	}
	return nil
}

// Each fixture comes back as the reader reads it, whatever the fake re-encoded.
func TestOneShotDevicesFromFixtures(t *testing.T) {
	for _, name := range []string{
		"voice-devices-v1.json", "voice-devices-v1-virtual-microphone.json", "voice-devices-v1-configured-input-absent.json",
		"voice-devices-v1-unshown.json", "voice-devices-v1-pipewire-unavailable.json", "voice-devices-v1-config-refused.json",
		"voice-devices-v1-config-none.json",
	} {
		t.Run(name, func(t *testing.T) {
			path := devicesFixture(t, name)
			data, err := os.ReadFile(path) // #nosec G304 -- a fixture of this repository
			if err != nil {
				t.Fatal(err)
			}
			want, err := schema.ParseVoiceDevices(data)
			if err != nil {
				t.Fatal(err)
			}
			got, err := newOneShot(storedProfile(t), "-doc", path).Devices(ctxFor(t))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Devices, want) {
				t.Errorf("devices %+v, want %+v", got.Devices, want)
			}
		})
	}
}

// The one-shot process: argv, the environment of §9 without CONCH_TOKEN
// whatever the credential, working directory /, its own process group, and
// standard input /dev/null.
func TestOneShotEnvironmentAndProcess(t *testing.T) {
	inherited := map[string]string{
		"CONCH_TOKEN": "parent-token", "CONCH_SERVER": "https://other.example.org", "CONCH_CONFIG_DIR": "/parent/conch",
		HelperEnv: "/elsewhere/conch-voice", "CONCH_A_LATER_ONE": "x", "RUST_LOG": "trace", "RUST_BACKTRACE": "1",
		"RUST_LIB_BACKTRACE": "1", "KEEP_THIS": "kept",
	}
	for k, v := range inherited {
		t.Setenv(k, v)
	}
	helper, err := filepath.EvalSymlinks(fakePath)
	if err != nil {
		t.Fatal(err)
	}
	envProfile := func(t *testing.T) Profile {
		p := storedProfile(t)
		p.Credential, p.Token = schema.VoiceControlCredentialEnv, fakeToken
		return p
	}
	noDir := func(t *testing.T) Profile {
		p := storedProfile(t)
		p.ConfigDir = ""
		return p
	}
	tests := []struct {
		name    string
		profile func(*testing.T) Profile
		run     func(context.Context, *OneShot) error
		argv    []string
	}{
		{name: "devices, a stored login", profile: storedProfile, argv: []string{"devices", "--json"},
			run: func(ctx context.Context, o *OneShot) error { _, err := o.Devices(ctx); return err }},
		{name: "devices, the TUI's login from CONCH_TOKEN", profile: envProfile, argv: []string{"devices", "--json"},
			run: func(ctx context.Context, o *OneShot) error { _, err := o.Devices(ctx); return err }},
		{name: "devices, no configuration directory", profile: noDir, argv: []string{"devices", "--json"},
			run: func(ctx context.Context, o *OneShot) error { _, err := o.Devices(ctx); return err }},
		{name: "config set, the TUI's login from CONCH_TOKEN", profile: envProfile, argv: []string{"config", "set", "audio.input", "--", "speech_out"},
			run: func(ctx context.Context, o *OneShot) error { return o.SetDevice(ctx, DeviceInput, "speech_out") }},
		{name: "config set default", profile: storedProfile, argv: []string{"config", "set", "audio.output", "--", "default"},
			run: func(ctx context.Context, o *OneShot) error { return o.SetDevice(ctx, DeviceOutput, DefaultDevice) }},
		// A name may begin with "-": after "--" it is the name, never an option.
		{name: "config set, a name that looks like an option", profile: storedProfile, argv: []string{"config", "set", "audio.input", "--", "-h"},
			run: func(ctx context.Context, o *OneShot) error { return o.SetDevice(ctx, DeviceInput, "-h") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.profile(t)
			dumpFile := filepath.Join(t.TempDir(), "dump")
			o := newOneShot(p, "-dump", dumpFile, "-doc", devicesFixture(t, "voice-devices-v1.json"))
			if err := tt.run(ctxFor(t), o); err != nil {
				t.Fatal(err)
			}
			d := readDump(t, dumpFile)
			wantArgv := append(append([]string{helper}, tt.argv...), o.testArgs...)
			if !reflect.DeepEqual(d["arg"], wantArgv) {
				t.Errorf("argv %q, want %q", d["arg"], wantArgv)
			}
			env := d.env()
			for k, v := range env {
				switch {
				case k == configDirEnv:
					if p.ConfigDir == "" || v != p.ConfigDir {
						t.Errorf("%s=%q", k, v)
					}
				case strings.HasPrefix(k, "CONCH_") || strings.HasPrefix(k, "RUST_"):
					t.Errorf("the one-shot run has %s", k)
				}
				if strings.Contains(v, fakeToken) || strings.Contains(v, "parent-token") {
					t.Errorf("a token in %s", k)
				}
			}
			if _, ok := env[configDirEnv]; ok != (p.ConfigDir != "") {
				t.Errorf("%s set: %v, want %v", configDirEnv, ok, p.ConfigDir != "")
			}
			if env["KEEP_THIS"] != "kept" {
				t.Error("an unrelated variable was dropped")
			}
			if d.one("cwd") != "/" {
				t.Errorf("working directory %q, want /", d.one("cwd"))
			}
			if d.one("pgid") != d.one("pid") {
				t.Errorf("process group %s, want its own (%s)", d.one("pgid"), d.one("pid"))
			}
			if d.one("fd0") != os.DevNull {
				t.Errorf("standard input is %q, want %s", d.one("fd0"), os.DevNull)
			}
			if !strings.HasPrefix(d.one("fd1"), "pipe:") || !strings.HasPrefix(d.one("fd2"), "pipe:") || d.one("fd1") == d.one("fd2") {
				t.Errorf("standard output %q and error %q, want a pipe each", d.one("fd1"), d.one("fd2"))
			}
			// The fake's own runtime may open a file there (the Go runtime
			// reads its cgroup's limits); a pipe or a socket would be one of
			// conch's.
			if fd3 := d.one("fd3"); strings.HasPrefix(fd3, "pipe:") || strings.HasPrefix(fd3, "socket:") {
				t.Errorf("descriptor 3 reached the run: %q", fd3)
			}
			pid, err := strconv.Atoi(d.one("pid"))
			if err != nil {
				t.Fatal(err)
			}
			assertGone(t, pid)
		})
	}
}

// A run that does not end in time is killed and reaped, and so is one whose
// context ends; neither gives a result.
func TestOneShotTimeout(t *testing.T) {
	tests := []struct {
		name   string
		cancel bool
	}{
		{name: "the bound"},
		{name: "the context ends", cancel: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dumpFile := filepath.Join(t.TempDir(), "dump")
			o := newOneShot(storedProfile(t), "-dump", dumpFile, "-stderr", "still listing\n", "-hang")
			o.timeout = 400 * time.Millisecond
			ctx, cancel := context.WithCancel(ctxFor(t))
			defer cancel()
			if tt.cancel {
				o.timeout = time.Minute
				time.AfterFunc(400*time.Millisecond, cancel)
			}
			start := time.Now()
			_, err := o.Devices(ctx)
			took := time.Since(start)
			if tt.cancel {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error %v, want context.Canceled", err)
				}
			} else if re := runErrOf(err); re == nil || re.Reason != RunTimedOut || re.ExitCode != -1 || re.Line != "still listing" {
				t.Fatalf("error %#v, want %s with its last line", err, RunTimedOut)
			}
			if took > 5*time.Second {
				t.Errorf("the run took %v to end", took)
			}
			pid, err := strconv.Atoi(readDump(t, dumpFile).one("pid"))
			if err != nil {
				t.Fatal(err)
			}
			assertGone(t, pid)
		})
	}
}

// Standard output is read up to schema.VoiceDevicesMaxBytes and no further:
// more is refused, and a run still writing is killed.
func TestOneShotOversizedOutput(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		reason string
	}{
		{name: "at the bound, read and parsed", args: []string{"-flood", strconv.Itoa(schema.VoiceDevicesMaxBytes)}, reason: RunBadOutput},
		{name: "one byte past it, then exit", args: []string{"-flood", strconv.Itoa(schema.VoiceDevicesMaxBytes + 1)}, reason: RunTooMuchOutput},
		{name: "far past it, still writing", args: []string{"-flood", strconv.Itoa(64 * schema.VoiceDevicesMaxBytes), "-hang"}, reason: RunTooMuchOutput},
		{name: "past it, then never ending", args: []string{"-flood", strconv.Itoa(schema.VoiceDevicesMaxBytes + 1), "-hang"}, reason: RunTooMuchOutput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dumpFile := filepath.Join(t.TempDir(), "dump")
			o := newOneShot(storedProfile(t), append([]string{"-dump", dumpFile}, tt.args...)...)
			start := time.Now()
			_, err := o.Devices(ctxFor(t))
			if re := runErrOf(err); re == nil || re.Reason != tt.reason {
				t.Fatalf("error %v, want %s", err, tt.reason)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("the run took %v to end", took)
			}
			pid, err := strconv.Atoi(readDump(t, dumpFile).one("pid"))
			if err != nil {
				t.Fatal(err)
			}
			assertGone(t, pid)
		})
	}
}

// A run that ends without what was asked says how, with its last line of
// standard error made safe; the line never holds the token.
func TestOneShotFailures(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		save   bool // config set, not devices
		reason string
		code   int
		line   string
	}{
		{name: "devices exits 1 with a line", args: []string{"-stderr", "WARN something\nconch-voice: PipeWire said no\n", "-exit", "1"},
			reason: RunFailed, code: 1, line: "conch-voice: PipeWire said no"},
		{name: "an older conch-voice without devices --json", args: []string{"-stderr", "error: unexpected argument '--json' found\n\nFor more information, try '--help'.\n", "-exit", "2"},
			reason: RunUnsupported, code: 2, line: "For more information, try '--help'."},
		{name: "config set refuses", save: true, args: []string{"-stderr", "conch-voice: voice.toml may be changed by other users\n", "-exit", "1"},
			reason: RunFailed, code: 1, line: "conch-voice: voice.toml may be changed by other users"},
		{name: "an older conch-voice without config set", save: true, args: []string{"-exit", "2"}, reason: RunUnsupported, code: 2},
		{name: "a crash", args: []string{"-exit", "101"}, reason: RunFailed, code: 101},
		{name: "not JSON", args: []string{"-raw", "{\"schema\":"}, reason: RunBadOutput},
		{name: "two objects", args: []string{"-raw", "{}\n{}\n"}, reason: RunBadOutput},
		{name: "another version", args: []string{"-raw", `{"schema":"conch.voice_devices.v2"}` + "\n"}, reason: RunBadOutput},
		{name: "a control character in a description", args: []string{"-raw",
			`{"schema":"conch.voice_devices.v1","pipewire":"ok","synced":true,"microphones":[{"name":"m","description":"\u001b[2Jx","default":false}],"speakers":[],"unshown_microphones":0,"unshown_speakers":0,"config":"missing"}` + "\n"},
			reason: RunBadOutput},
		{name: "escapes and the token on standard error", args: []string{"-stderr", "conch-voice: \x1b[31mred\x1b[0m " + fakeToken + string(rune(0x202e)) + "\n", "-exit", "1"},
			reason: RunFailed, code: 1, line: "conch-voice: [31mred[0m " + redacted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := storedProfile(t)
			p.Credential, p.Token = schema.VoiceControlCredentialEnv, fakeToken
			o := newOneShot(p, tt.args...)
			var err error
			if tt.save {
				err = o.SetDevice(ctxFor(t), DeviceInput, "speech_out")
			} else {
				_, err = o.Devices(ctxFor(t))
			}
			re := runErrOf(err)
			if re == nil || re.Reason != tt.reason || re.ExitCode != tt.code || re.Line != tt.line {
				t.Fatalf("error %#v, want reason %s, code %d, line %q", err, tt.reason, tt.code, tt.line)
			}
			if strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), "\x1b") {
				t.Errorf("the error shows the token or an escape: %q", err)
			}
		})
	}
	// What a run that worked wrote to standard error comes back, made safe.
	o := newOneShot(storedProfile(t), "-doc", devicesFixture(t, "voice-devices-v1-config-refused.json"),
		"-stderr", "conch-voice: voice.toml is \x1b[1mrefused\n")
	list, err := o.Devices(ctxFor(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list.Diagnostics, []string{"conch-voice: voice.toml is [1mrefused"}) || list.Devices.Config != schema.VoiceDevicesConfigRefused {
		t.Errorf("list %+v", list)
	}
}

// A helper that is not found or not trusted is refused before anything runs,
// by the managed helper's rules.
func TestOneShotFindsAsTheSupervisorDoes(t *testing.T) {
	shared := privateDir(t)
	if err := os.Chmod(shared, 0o777); err != nil { // #nosec G302 -- the directory others can write, under test
		t.Fatal(err)
	}
	planted := filepath.Join(shared, HelperName)
	if err := copyFile(fakePath, planted, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		path   string
		reason string
	}{
		{name: "in a directory others can write", path: planted, reason: ReasonHelperUntrusted},
		{name: "a relative path", path: "conch-voice", reason: ReasonHelperUntrusted},
		{name: "not there", path: filepath.Join(privateDir(t), HelperName), reason: ReasonHelperNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dumpFile := filepath.Join(t.TempDir(), "dump")
			o := NewOneShot(storedProfile(t), Options{HelperPath: tt.path})
			o.testArgs = []string{"-dump", dumpFile}
			_, err := o.Devices(ctxFor(t))
			if reasonOf(err) != tt.reason {
				t.Fatalf("devices: %v, want %s", err, tt.reason)
			}
			if err := o.SetDevice(ctxFor(t), DeviceInput, "speech_out"); reasonOf(err) != tt.reason {
				t.Fatalf("config set: %v, want %s", err, tt.reason)
			}
			if _, err := os.Stat(dumpFile); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("something ran: %v", err)
			}
		})
	}
}

// A key or a name config set could not save is refused before anything runs.
func TestSetDeviceRefusesBeforeRunning(t *testing.T) {
	tests := []struct {
		name string
		key  DeviceKey
		dev  string
		ok   bool
	}{
		{name: "a listed name", key: DeviceInput, dev: "alsa_input.pci-0000_00_1f.3.analog-stereo", ok: true},
		{name: "a name with a space", key: DeviceOutput, dev: "my speakers", ok: true},
		{name: "default", key: DeviceOutput, dev: DefaultDevice, ok: true},
		{name: "empty", key: DeviceInput, dev: ""},
		{name: "a quotation mark", key: DeviceInput, dev: `mic"`},
		{name: "a backslash", key: DeviceInput, dev: `mic\`},
		{name: "a line feed", key: DeviceInput, dev: "mic\n[keys]"},
		{name: "an escape", key: DeviceInput, dev: "mic\x1b[2J"},
		{name: "too long", key: DeviceInput, dev: strings.Repeat("m", schema.VoiceDevicesMaxNameBytes+1)},
		{name: "not UTF-8", key: DeviceInput, dev: "mic\xff"},
		{name: "another key", key: "server", dev: "https://evil.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Nothing is there to run: a run would be helper_not_found.
			o := NewOneShot(storedProfile(t), Options{HelperPath: filepath.Join(privateDir(t), HelperName)})
			err := o.SetDevice(ctxFor(t), tt.key, tt.dev)
			ran := reasonOf(err) == ReasonHelperNotFound
			if ran != tt.ok {
				t.Errorf("error %v: ran %v, want %v", err, ran, tt.ok)
			}
			if tt.key == DeviceInput && !tt.ok && !errors.Is(err, ErrBadDeviceName) {
				t.Errorf("error %v, want ErrBadDeviceName", err)
			}
		})
	}
}

// The TUI applies a saved device by leaving and joining again: the start of
// the new helper keeps the mute and deafen the old one confirmed.
func TestSaveThenRejoinKeepsMuteAndDeafen(t *testing.T) {
	dumpFile := filepath.Join(t.TempDir(), "dump")
	p := storedProfile(t)
	s := newSupervisor(t, p, Timings{}, "-dump", dumpFile)
	ctx := ctxFor(t)
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMute(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeafen(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := newOneShot(p).SetDevice(ctx, DeviceInput, "speech_out"); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Join(ctx, testChannel); err != nil {
		t.Fatal(err)
	}
	cmd, err := schema.ParseVoiceControlCommand([]byte(readDump(t, dumpFile).one("start")))
	if err != nil || cmd.Start == nil || !cmd.Start.Muted || !cmd.Start.Deafened || cmd.Start.Channel != testChannel {
		t.Fatalf("the rejoin's start: %+v, %v", cmd.Start, err)
	}
	if st := s.Status(); st.Gen != 2 || !st.Muted || !st.Deafened {
		t.Errorf("after the rejoin: %+v", st)
	}
}
