package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errNoProbe is a wait or a command that failed because the probe process had
// already ended.
var errNoProbe = errors.New("the probe exited")

// event is one line a probe wrote.
type event struct {
	T      float64        // the probe's monotonic clock: milliseconds since it started
	Ev     string         // what happened
	Fields map[string]any // everything on the line
	At     time.Time      // T on the wall clock, through the probe's first line
}

func (e event) str(key string) string {
	s, _ := e.Fields[key].(string)
	return s
}

func (e event) num(key string) float64 {
	f, ok := e.Fields[key].(float64)
	if !ok {
		return math.NaN()
	}
	return f
}

func (e event) flag(key string) bool {
	b, _ := e.Fields[key].(bool)
	return b
}

// probe is one running sdk_probe process.
type probe struct {
	r     *run
	name  string // for messages: never a room name
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{} // closed once the process has exited and its output is read

	mu      sync.Mutex
	events  []event
	changed chan struct{} // closed and replaced when events grows or the process ends
	exited  bool
	startT  float64   // the probe's clock on its first line
	startAt time.Time // the wall clock on its first line
	errOut  bytes.Buffer
}

// start runs the probe in the given mode. The token and the room name reach
// it in its environment; args are options that hold nothing secret. Its home
// and configuration directories are a throwaway directory of this run, and it
// has no session bus or audio socket to find, so nothing it links can read or
// write the user's own configuration or open a device.
func (r *run) start(name, mode, token, room string, args ...string) (*probe, error) {
	if stopCtx.Err() != nil {
		return nil, errInterrupted
	}
	home := filepath.Join(r.tmp, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	cmd := exec.Command(r.probeBin, append([]string{mode}, args...)...) // #nosec G204 -- the binary this run built; args are this program's own
	cmd.Dir = home
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"SDK_PROBE_URL=" + r.srv.wsURL,
		"SDK_PROBE_TOKEN=" + token,
		"SDK_PROBE_ROOM=" + room,
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.serial++
	serial := r.serial
	r.probeCount++
	r.mu.Unlock()
	p := &probe{r: r, name: fmt.Sprintf("%s#%d", name, serial), cmd: cmd, stdin: stdin, done: make(chan struct{}), changed: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the probe: %w", err)
	}
	r.mu.Lock()
	r.probes = append(r.probes, p)
	r.mu.Unlock()

	var raw *os.File
	if r.rawDir != "" {
		raw, _ = os.Create(filepath.Join(r.rawDir, fmt.Sprintf("%03d-%s.jsonl", serial, name))) // #nosec G304 -- a directory the caller named
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		p.readLines(stdout, raw)
	}()
	go func() {
		defer readers.Done()
		p.readErr(stderr)
	}()
	go func() {
		readers.Wait()
		_ = cmd.Wait()
		if raw != nil {
			_ = raw.Close()
		}
		p.mu.Lock()
		p.exited = true
		close(p.changed)
		p.changed = make(chan struct{})
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// readLines turns the probe's standard output into events. A line that holds
// a token, the secret or a room name is recorded as a leak and dropped.
func (p *probe) readLines(out io.Reader, raw *os.File) {
	reader := bufio.NewReaderSize(out, 1<<16)
	for {
		line, err := reader.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			p.line(line, raw)
		}
		if err != nil {
			return
		}
	}
}

func (p *probe) line(line string, raw *os.File) {
	p.r.mu.Lock()
	p.r.lines++
	p.r.mu.Unlock()
	if what, found := p.r.leak(line); found {
		p.r.noteLeak(what, "the standard output of a probe")
		return
	}
	if raw != nil {
		_, _ = raw.WriteString(line + "\n")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		// Not the probe's own: something it links wrote to standard output.
		p.r.mu.Lock()
		p.r.stray[line]++
		p.r.mu.Unlock()
		return
	}
	e := event{Fields: fields}
	e.Ev, _ = fields["ev"].(string)
	e.T, _ = fields["t_ms"].(float64)
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.Ev == "start" {
		p.startT = e.T
		p.startAt = time.UnixMicro(int64(e.num("unix_us")))
	}
	e.At = p.startAt.Add(time.Duration((e.T - p.startT) * float64(time.Millisecond)))
	if e.Ev == "exit" {
		// How far the probe's monotonic clock and the wall clock moved apart
		// over its life: the error of putting two probes on one timeline.
		drift := math.Abs(float64(time.UnixMicro(int64(e.num("unix_us"))).Sub(e.At)) / float64(time.Millisecond))
		p.r.mu.Lock()
		p.r.maxDriftMS = math.Max(p.r.maxDriftMS, drift)
		p.r.mu.Unlock()
	}
	p.events = append(p.events, e)
	close(p.changed)
	p.changed = make(chan struct{})
}

// readErr keeps the start of what the probe wrote to standard error, which is
// where native code that bypasses the log facade would write, and searches all
// of it for secrets.
func (p *probe) readErr(errOut io.Reader) {
	reader := bufio.NewReaderSize(errOut, 1<<16)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if what, found := p.r.leak(line); found {
				p.r.noteLeak(what, "the standard error of a probe")
			} else {
				p.mu.Lock()
				if p.errOut.Len() < 4096 {
					p.errOut.WriteString(line)
				}
				p.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// stderrText is what the probe wrote to standard error (the first 4 kB).
func (p *probe) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.TrimSpace(p.errOut.String())
}

// mark is the number of events so far: pass it to next or since to look only
// at what comes after this moment.
func (p *probe) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

// since returns a copy of the events from index from on.
func (p *probe) since(from int) []event {
	p.mu.Lock()
	defer p.mu.Unlock()
	if from > len(p.events) {
		from = len(p.events)
	}
	return append([]event(nil), p.events[from:]...)
}

// next waits for the first event at or after index from that pred accepts.
// It fails when the time runs out, the probe exits, or a signal arrives.
func (p *probe) next(from int, within time.Duration, what string, pred func(event) bool) (event, error) {
	timeout := time.NewTimer(within)
	defer timeout.Stop()
	for {
		p.mu.Lock()
		for i := from; i < len(p.events); i++ {
			if pred(p.events[i]) {
				e := p.events[i]
				p.mu.Unlock()
				return e, nil
			}
		}
		from = len(p.events)
		exited, changed := p.exited, p.changed
		p.mu.Unlock()
		if exited {
			return event{}, fmt.Errorf("%s: waiting for %s: %w", p.name, what, errNoProbe)
		}
		select {
		case <-changed:
		case <-timeout.C:
			return event{}, fmt.Errorf("%s: no %s within %s", p.name, what, within)
		case <-stopCtx.Done():
			return event{}, errInterrupted
		}
	}
}

// wait is next for an event by name.
func (p *probe) wait(from int, within time.Duration, name string) (event, error) {
	return p.next(from, within, name, func(e event) bool { return e.Ev == name })
}

// first returns the first event since from that pred accepts, without waiting.
func (p *probe) first(from int, pred func(event) bool) (event, bool) {
	for _, e := range p.since(from) {
		if pred(e) {
			return e, true
		}
	}
	return event{}, false
}

// send writes one command to the probe.
func (p *probe) send(command string) error {
	if _, err := io.WriteString(p.stdin, command+"\n"); err != nil {
		return fmt.Errorf("%s: sending %q: %w", p.name, command, errNoProbe)
	}
	return nil
}

// ask sends a command and waits for the event it is answered with.
func (p *probe) ask(command, answer string) (event, error) {
	from := p.mark()
	if err := p.send(command); err != nil {
		return event{}, err
	}
	return p.wait(from, 5*time.Second, answer)
}

// quit asks the probe to leave and waits for it; a probe that does not go is
// killed, with everything it started.
func (p *probe) quit() {
	_ = p.send("quit")
	select {
	case <-p.done:
	case <-time.After(75 * time.Second):
		p.kill()
	}
}

func (p *probe) kill() {
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = p.stdin.Close()
	<-p.done
}

// stopProbes ends every probe still running: asked first, all at once.
func (r *run) stopProbes() {
	r.mu.Lock()
	probes := r.probes
	r.probes = nil
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range probes {
		select {
		case <-p.done:
			continue
		default:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if stopCtx.Err() != nil {
				p.kill()
				return
			}
			p.quit()
		}()
	}
	wg.Wait()
}

// joined starts a probe on a fresh token for identity in room and waits until
// it is connected and, unless it only listens, has published its track.
func (r *run) joined(name, mode, identity, room string, args ...string) (*probe, error) {
	tok, err := r.token(identity, room)
	if err != nil {
		return nil, err
	}
	p, err := r.start(name, mode, tok, room, args...)
	if err != nil {
		return nil, err
	}
	ready := "published"
	if mode == "listen" {
		ready = "connected"
	}
	if _, err := p.next(0, 30*time.Second, ready, func(e event) bool {
		return e.Ev == ready || e.Ev == "connect_failed" || e.Ev == "error"
	}); err != nil {
		return nil, err
	}
	if e, bad := p.first(0, func(e event) bool { return e.Ev == "connect_failed" || e.Ev == "error" }); bad {
		return nil, fmt.Errorf("%s: %s: %s", p.name, e.Ev, e.str("error"))
	}
	return p, nil
}
