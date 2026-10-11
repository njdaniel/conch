package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The V4 scenario (issue #188, docs/design/conch-voice.md §10): three real
// conch-voice processes in one channel, each with a tone of its own for a
// microphone and a sink that counts for speakers, driven through standard
// input. It is the automated half of V4's exit: everything about "three
// people talking in a channel" that does not need a person.
//
// What a client hears is read from the stats object it prints once a second
// with --json: for each remote speaker, the frames received in that second,
// how many were audible, and which of the named tones was strongest. A stats
// line read at time t describes (t-1s, t], so a line counts for a press only
// if that whole second lies inside it, with a margin for the network and the
// jitter buffer.

// The tone each client's microphone plays, and every client's sink looks for.
const (
	toneAnn = 440
	toneBen = 880
	toneCy  = 1320
)

const (
	// hearMargin is how long after a client says it is transmitting its audio
	// is expected to be arriving everywhere, and how long after it says it
	// stopped the last of it has drained: the release tail (150 ms), the SDK's
	// send queue (100 ms), the network and the listener's jitter buffer.
	hearMargin = 700 * time.Millisecond
	// A stats line covers this long.
	statsEvery = time.Second
)

// voiceEvent is one JSON object a client printed, and when it was read.
type voiceEvent struct {
	at  time.Time
	obj map[string]any
}

// voiceClient is one running conch-voice.
type voiceClient struct {
	h    *harness
	who  *person
	tone int

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	errPath string

	mu     sync.Mutex
	events []voiceEvent
	done   chan struct{}
	exit   error // valid once done is closed
}

// buildVoiceClient builds conch-voice with the pinned toolchain and returns
// the binary. skip is why it could not be built on this machine (the
// toolchain has not been fetched); anything else that goes wrong is an error.
func (h *harness) buildVoiceClient() (bin, skip string, err error) {
	ctx, cancel := context.WithTimeout(stopCtx, 30*time.Minute)
	defer cancel()
	check := groupCommand(ctx, "bash", "-c", ". ./scripts/voice-env.sh")
	check.Dir = h.repo
	if out, err := check.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return "", "", errInterrupted
		}
		return "", "the pinned Rust toolchain is not set up: " + oneLine(string(out), err), nil
	}
	build := groupCommand(ctx, "bash", "-c", ". ./scripts/voice-env.sh && cd voice && cargo build --locked -p conch-voice")
	build.Dir = h.repo
	if out, err := build.CombinedOutput(); err != nil {
		if stopCtx.Err() != nil {
			return "", "", errInterrupted
		}
		text := string(out)
		if len(text) > 3000 {
			text = text[len(text)-3000:]
		}
		return "", "", fmt.Errorf("cargo build -p conch-voice: %w\n%s", err, text)
	}
	return filepath.Join(h.repo, "voice", "target", "debug", "conch-voice"), "", nil
}

// startVoiceClient runs `conch-voice join channel` as p, with the stored
// login `conch login` wrote for p and nothing of the caller's environment
// that could change who it is. mic is the --mic value.
func (l *live) startVoiceClient(p *person, channel, mic string, tone int) (*voiceClient, error) {
	h := l.h
	dir, err := h.mkdir("conch-voice-" + p.name + "-")
	if err != nil {
		return nil, err
	}
	outPath, errPath := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- a path under this program's temp dir
	if err != nil {
		return nil, err
	}
	errFile, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- a path under this program's temp dir
	if err != nil {
		_ = outFile.Close()
		return nil, err
	}
	cmd := groupStart(l.voiceBin, "join", channel, "--json", "--mic", mic,
		"--sink", fmt.Sprintf("count:%d,%d,%d", toneAnn, toneBen, toneCy))
	cmd.Env = p.cli.env
	cmd.Stderr = errFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_, _ = outFile.Close(), errFile.Close()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_, _, _ = outFile.Close(), errFile.Close(), stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_, _, _ = outFile.Close(), errFile.Close(), stdin.Close()
		return nil, err
	}
	c := &voiceClient{h: h, who: p, tone: tone, cmd: cmd, stdin: stdin, errPath: errPath, done: make(chan struct{})}
	read := make(chan struct{})
	go func() {
		defer close(read)
		lines := bufio.NewScanner(stdout)
		lines.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for lines.Scan() {
			line := lines.Text()
			at := time.Now()
			// Every line is kept for the leak scan, whatever it is.
			_, _ = outFile.WriteString(line + "\n")
			// libwebrtc prints a line of its own on a machine with an NVIDIA
			// GPU, so a line that is not an object is skipped, not an error.
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line), &obj) != nil {
				continue
			}
			c.mu.Lock()
			c.events = append(c.events, voiceEvent{at, obj})
			c.mu.Unlock()
		}
		// If the scanner gave up (a line longer than its buffer), the rest is
		// still read, into the file the leak scan reads: a client must never
		// block on its own output because this program stopped listening.
		_, _ = io.Copy(outFile, stdout)
	}()
	go func() {
		<-read
		c.exit = cmd.Wait()
		_, _ = outFile.Close(), errFile.Close()
		close(c.done)
	}()
	h.onCleanup(c.kill)
	h.mu.Lock()
	h.logs = append(h.logs,
		namedFile{"conch-voice's standard output (" + p.name + ")", outPath, true},
		namedFile{"conch-voice's standard error (" + p.name + ")", errPath, true})
	h.mu.Unlock()
	return c, nil
}

func (c *voiceClient) kill() {
	_ = killGroup(c.cmd)
	<-c.done
}

// say writes one command line to the client's standard input.
func (c *voiceClient) say(command string) error {
	if _, err := io.WriteString(c.stdin, command+"\n"); err != nil {
		return fmt.Errorf("writing %q to %s's conch-voice: %w", command, c.who.name, err)
	}
	return nil
}

func (c *voiceClient) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// stderrTail is the end of what the client wrote to standard error, redacted,
// for a failure message.
func (c *voiceClient) stderrTail() string {
	data, _ := os.ReadFile(c.errPath) // #nosec G304 -- a path under this program's temp dir
	s := strings.TrimSpace(string(data))
	if len(s) > 800 {
		s = s[len(s)-800:]
	}
	return c.h.redact(s)
}

// snapshot is the events read so far.
func (c *voiceClient) snapshot() []voiceEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]voiceEvent(nil), c.events...)
}

// wait returns the time of the first event at or after since that pred
// accepts. It fails at once if the client has exited.
func (c *voiceClient) wait(what string, within time.Duration, since time.Time, pred func(map[string]any) bool) (time.Time, error) {
	var at time.Time
	err := waitForOrFail(c.who.name+"'s conch-voice: "+what, within, func() (bool, string, error) {
		last := "no events"
		for _, e := range c.snapshot() {
			if e.at.Before(since) {
				continue
			}
			if pred(e.obj) {
				at = e.at
				return true, "", nil
			}
			if name, _ := e.obj["event"].(string); name != "stats" && name != "presence" {
				encoded, _ := json.Marshal(e.obj)
				last = "last saw " + c.h.redact(string(encoded))
			}
		}
		if c.exited() {
			return false, "", fmt.Errorf("the client exited (%s); standard error: %s", exitText(c.exit), c.stderrTail())
		}
		return false, last, nil
	})
	return at, err
}

func connectionIs(state string) func(map[string]any) bool {
	return func(o map[string]any) bool { return o["event"] == "connection" && o["state"] == state }
}

func transmittingIs(on bool) func(map[string]any) bool {
	return func(o map[string]any) bool { return o["event"] == "self" && o["transmitting"] == on }
}

// readyToTalk accepts the client's own state when a press would transmit.
func readyToTalk(o map[string]any) bool {
	blocked, _ := o["blocked"].([]any)
	return o["event"] == "self" && o["transmitting"] == false && len(blocked) == 0
}

// statsWithin returns the stats objects whose whole second lies in [from, to].
func (c *voiceClient) statsWithin(from, to time.Time) []map[string]any {
	var lines []map[string]any
	for _, e := range c.snapshot() {
		if e.obj["event"] == "stats" && !e.at.Add(-statsEvery).Before(from) && !e.at.After(to) {
			lines = append(lines, e.obj)
		}
	}
	return lines
}

// waitStats waits until the client has printed n stats lines whose whole
// second began at or after from, and returns the time it stopped waiting.
func (c *voiceClient) waitStats(n int, from time.Time) (time.Time, error) {
	err := waitForOrFail(fmt.Sprintf("%s's conch-voice: %d stats lines", c.who.name, n), 20*time.Second, func() (bool, string, error) {
		got := len(c.statsWithin(from, time.Now()))
		if got >= n {
			return true, "", nil
		}
		if c.exited() {
			return false, "", fmt.Errorf("the client exited (%s); standard error: %s", exitText(c.exit), c.stderrTail())
		}
		return false, fmt.Sprintf("%d of %d", got, n), nil
	})
	return time.Now(), err
}

func number(v any) float64 {
	f, _ := v.(float64)
	return f
}

// heard checks every stats line of the listener whose second lies in
// [from, to]: each speaker in want (identity -> tone) is counted, audibly,
// with that tone as the strongest; every other speaker is counted as silence;
// the listener itself is not among its own speakers. With nobody in want, the
// mix is silent too. At least `lines` such lines must exist.
func (c *voiceClient) heard(what string, from, to time.Time, lines int, want map[string]int) error {
	stats := c.statsWithin(from, to)
	if len(stats) < lines {
		return fmt.Errorf("%s: %s printed %d stats lines in the window, want at least %d", what, c.who.name, len(stats), lines)
	}
	self := identity(c.who.id)
	for _, line := range stats {
		encoded, _ := json.Marshal(line)
		saw := c.h.redact(string(encoded))
		speakers, _ := line["speakers"].([]any)
		found := map[string]bool{}
		for _, raw := range speakers {
			s, _ := raw.(map[string]any)
			id, _ := s["speaker"].(string)
			frames, audible := number(s["frames"]), number(s["audible_frames"])
			if id == self {
				return fmt.Errorf("%s: %s counts itself among its speakers: %s", what, c.who.name, saw)
			}
			tone, wanted := want[id]
			if !wanted {
				if audible != 0 {
					return fmt.Errorf("%s: %s counted %v audible frames from %s, who is not holding a key: %s", what, c.who.name, audible, id, saw)
				}
				continue
			}
			found[id] = true
			// Four fifths, not all: a loaded machine drops a frame now and
			// then, and that is not what this checks.
			if frames < 50 || audible*5 < frames*4 {
				return fmt.Errorf("%s: %s counted %v frames from %s, %v audible; want at least 50, four fifths of them audible: %s", what, c.who.name, frames, id, audible, saw)
			}
			if number(s["dominant_hz"]) != float64(tone) {
				return fmt.Errorf("%s: %s hears %v Hz strongest from %s, want %d: %s", what, c.who.name, s["dominant_hz"], id, tone, saw)
			}
		}
		for id := range want {
			if !found[id] {
				return fmt.Errorf("%s: %s has no count for %s, who is holding a key: %s", what, c.who.name, id, saw)
			}
		}
		mix, _ := line["mix"].(map[string]any)
		if len(want) == 0 && number(mix["audible_frames"]) != 0 {
			return fmt.Errorf("%s: %s's mix has %v audible frames with nobody holding a key: %s", what, c.who.name, mix["audible_frames"], saw)
		}
		if len(want) > 0 && number(mix["audible_frames"]) == 0 {
			return fmt.Errorf("%s: %s's mix is silent while someone is holding a key: %s", what, c.who.name, saw)
		}
	}
	return nil
}

// sending checks the client's own frames_sent in the stats lines of the
// window: at least four fifths of a second's worth in each when it should be
// sending, and none at all when it should not.
func (c *voiceClient) sending(what string, from, to time.Time, on bool) error {
	for _, line := range c.statsWithin(from, to) {
		sent := number(line["frames_sent"])
		if on && sent < 80 || !on && sent != 0 {
			return fmt.Errorf("%s: %s handed %v frames to its track in a second, want %s", what, c.who.name, sent,
				map[bool]string{true: "at least 80", false: "none"}[on])
		}
	}
	return nil
}

// squad is the state of the V4 scenario.
type squad struct {
	l         *live
	channelID int64
	subject   string
	clients   []*voiceClient // those still running
	// presses counts the reported press and release pairs each principal's
	// audit rows must show, by principal id.
	presses map[int64]int
	// Since when each principal's rows are counted (reset after a rotation).
	since time.Time
	// Since when nobody has been transmitting, or zero while someone is.
	// Every stats line from then until the next key goes down must be silent
	// (stillQuiet), not only the first one after a release.
	quietFrom time.Time
	// Unreported rows allowed for a principal: the one the scenario provokes.
	provoked map[int64]int
}

// press holds the key of each talker until every client has printed two stats
// lines that lie inside the press, checks what each heard and sent, releases,
// and checks that everything is silent again.
func (s *squad) press(what string, talkers ...*voiceClient) error {
	if err := s.stillQuiet("before " + what); err != nil {
		return err
	}
	want := map[string]int{}
	var on time.Time
	for _, t := range talkers {
		asked := time.Now()
		if err := t.say("down"); err != nil {
			return err
		}
		at, err := t.wait("transmitting after down", 15*time.Second, asked, transmittingIs(true))
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		on = at
		want[identity(t.who.id)] = t.tone
		s.presses[t.who.id]++
	}
	from := on.Add(hearMargin)
	var to time.Time
	for _, c := range s.clients {
		at, err := c.waitStats(2, from)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		to = at
	}
	// The window ends before any key is released.
	for _, c := range s.clients {
		others := map[string]int{}
		for id, tone := range want {
			if id != identity(c.who.id) {
				others[id] = tone
			}
		}
		if err := c.heard(what, from, to, 2, others); err != nil {
			return err
		}
	}
	for _, c := range s.clients {
		_, talking := want[identity(c.who.id)]
		if err := c.sending(what, from, to, talking); err != nil {
			return err
		}
	}
	var off time.Time
	for _, t := range talkers {
		asked := time.Now()
		if err := t.say("up"); err != nil {
			return err
		}
		at, err := t.wait("not transmitting after up", 15*time.Second, asked, transmittingIs(false))
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		off = at
	}
	return s.quiet(what+", after the release", off)
}

// stillQuiet checks every stats line since the last release (quiet set the
// time) up to now: nobody was heard by anyone and nobody sent a frame, for
// the whole of the time no key was down. It is called just before a key goes
// down, and ends the quiet period.
func (s *squad) stillQuiet(what string) error {
	if s.quietFrom.IsZero() {
		return nil
	}
	from, to := s.quietFrom, time.Now()
	s.quietFrom = time.Time{}
	for _, c := range s.clients {
		if err := c.heard(what+", in all the time no key was down", from, to, 0, nil); err != nil {
			return err
		}
		if err := c.sending(what+", in all the time no key was down", from, to, false); err != nil {
			return err
		}
	}
	return nil
}

// quiet checks that from a moment after `since` every client counts no audio
// from anyone and sends none, for one whole stats line each, and starts the
// quiet period that stillQuiet checks to its end.
func (s *squad) quiet(what string, since time.Time) error {
	from := since.Add(hearMargin)
	s.quietFrom = from
	var to time.Time
	for _, c := range s.clients {
		at, err := c.waitStats(1, from)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		to = at
	}
	// What the others received first: that is the evidence that does not
	// depend on the sender's own count of what it sent.
	for _, c := range s.clients {
		if err := c.heard(what, from, to, 1, nil); err != nil {
			return err
		}
	}
	for _, c := range s.clients {
		if err := c.sending(what, from, to, false); err != nil {
			return err
		}
	}
	return nil
}

// transmitAudit reads a principal's transmit rows since s.since. It returns
// the number of reported press and release pairs, and fails on anything that
// is not one: a row out of order, a reported row with another detail, or an
// unreported transmission beyond those the scenario provoked and the one
// late report the rule is known to record now and then (see lateReports).
func (s *squad) transmitAudit(c *voiceClient) (pairs, late int, err error) {
	all, err := s.l.d.audit()
	if err != nil {
		return 0, 0, err
	}
	actor := fmt.Sprintf("principal:%d", c.who.id)
	reported := fmt.Sprintf("channel=%d audience=channel source=reported", s.channelID)
	since := s.since.Truncate(time.Millisecond)
	open, unreported := false, 0
	for _, e := range all {
		if e.Subject != s.subject || e.Actor != actor || e.CreatedAt.Before(since) {
			continue
		}
		switch e.Action {
		case store.AuditVoiceTransmitStarted:
			if open || e.Detail != reported {
				return 0, 0, fmt.Errorf("%s's audit rows: a voice_transmit_started with detail %q while open=%v; want %q after a stop", c.who.name, e.Detail, open, reported)
			}
			open = true
		case store.AuditVoiceTransmitStopped:
			switch {
			case strings.Contains(e.Detail, "source=observed"):
				// The poller closing an unreported transmission. One closed
				// because the report then arrived is a late report.
				if strings.HasSuffix(e.Detail, "reason=reported") {
					late++
				}
			case open && e.Detail == reported:
				open = false
				pairs++
			default:
				return 0, 0, fmt.Errorf("%s's audit rows: a voice_transmit_stopped with detail %q while open=%v; want %q after a start", c.who.name, e.Detail, open, reported)
			}
		case store.AuditVoiceTransmitUnreported:
			unreported++
		}
	}
	if extra := unreported - s.provoked[c.who.id]; extra != late {
		return 0, 0, fmt.Errorf("%s's audit rows: %d voice_transmit_unreported rows, %d provoked by this scenario and %d closed as late reports; a conch-voice client that reports must have no other", c.who.name, unreported, s.provoked[c.who.id], late)
	}
	if open {
		return pairs, late, errStillOpen
	}
	return pairs, late, nil
}

var errStillOpen = errors.New("a reported transmission has no closing row yet")

// lateReports is how many honest presses in one run may be recorded as
// reported late (an unreported row closed with reason=reported). The rule
// records one when two poller passes fall between an unmute and its report:
// about one press in 1,600 when reports take 50 ms (docs/design/conch-voice.md
// §6). A run has about a dozen presses, so one is allowed and said; two is a
// failure.
const lateReports = 1

// audited waits until every running client's rows show exactly the presses
// made, each as one reported pair in order.
func (s *squad) audited(what string) (late int, err error) {
	err = waitForOrFail(what+": one reported voice_transmit_started and voice_transmit_stopped per press, in order", 30*time.Second, func() (bool, string, error) {
		late = 0
		for _, c := range s.clients {
			pairs, l, err := s.transmitAudit(c)
			if err != nil && !errors.Is(err, errStillOpen) {
				return false, "", err
			}
			late += l
			if err != nil || pairs != s.presses[c.who.id] {
				return false, fmt.Sprintf("%s has %d pairs, want %d (%v)", c.who.name, pairs, s.presses[c.who.id], err), nil
			}
		}
		return true, "", nil
	})
	if err == nil && late > lateReports {
		err = fmt.Errorf("%s: %d presses were recorded as reported late; at most %d is expected in one run", what, late, lateReports)
	}
	return late, err
}

// voiceClients is the V4 scenario.
func (l *live) voiceClients(ctx context.Context) error {
	h, d := l.h, l.d
	if l.voiceBin == "" {
		h.say("SKIP conch-voice clients: %s", l.voiceSkip)
		return nil
	}
	channelID, err := d.createChannel("squad")
	if err != nil {
		return err
	}
	s := &squad{l: l, channelID: channelID, subject: fmt.Sprintf("channel:%d", channelID), presses: map[int64]int{}, provoked: map[int64]int{}, since: time.Now()}
	var people []*person
	for _, name := range []string{"ann", "ben", "cy"} {
		p, err := h.newPerson(d, name, "squad")
		if err != nil {
			return err
		}
		people = append(people, p)
	}
	dee, err := h.newPerson(d, "dee", "side")
	if err != nil {
		return err
	}

	// ---- joining
	started := time.Now()
	for i, tone := range []int{toneAnn, toneBen, toneCy} {
		c, err := l.startVoiceClient(people[i], "squad", fmt.Sprintf("tone:%d", tone), tone)
		if err != nil {
			return err
		}
		s.clients = append(s.clients, c)
	}
	ann, ben, cy := s.clients[0], s.clients[1], s.clients[2]
	for _, c := range s.clients {
		if _, err := c.wait("connected", 60*time.Second, started, connectionIs("connected")); err != nil {
			return err
		}
		if _, err := c.wait("ready to talk", 30*time.Second, started, readyToTalk); err != nil {
			return err
		}
	}
	// Each is in the room, by LiveKit's own account, with a muted microphone.
	var room string
	if err := waitFor("LiveKit to list ann, ben and cy in one room with muted microphones", 30*time.Second, func() (bool, string) {
		rooms, err := d.voiceRooms(channelID)
		if err != nil || len(rooms) != 1 {
			return false, fmt.Sprintf("%d rooms stored for squad (%v)", len(rooms), err)
		}
		room = rooms[0]
		for _, c := range s.clients {
			ok, p, saw := l.lkHas(room, c.who.id)
			if !ok {
				return false, saw
			}
			if t, has := p.mic(); !has || !t.Muted {
				return false, fmt.Sprintf("%s's microphone track: published=%v muted=%v", c.who.name, has, t.Muted)
			}
		}
		return true, ""
	}); err != nil {
		return err
	}
	h.room(room)

	// A fourth principal, who is not a member, is told why and stops.
	outsider, err := l.startVoiceClient(dee, "squad", "none", 0)
	if err != nil {
		return err
	}
	select {
	case <-outsider.done:
	case <-time.After(30 * time.Second):
		return errors.New("dee, who is not a member of squad, ran conch-voice join squad and it was still running after 30 s; it must stop with a message")
	case <-ctx.Done():
		return errInterrupted
	}
	var exitErr *exec.ExitError
	if !errors.As(outsider.exit, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(outsider.stderrTail(), "not a member of this channel") {
		return fmt.Errorf("dee's conch-voice join squad ended with %s and said %q; want exit status 1 and a line saying she is not a member of this channel", exitText(outsider.exit), outsider.stderrTail())
	}
	if ok, _, _ := l.lkHas(room, dee.id); ok {
		return errors.New("dee, who is not a member of squad, is in its room according to LiveKit")
	}
	h.say("ok   conch-voice: ann, ben and cy joined squad with microphones muted; dee, not a member, was refused with exit status 1 and told why")

	// ---- nobody holds a key: every source is playing, and nothing is heard
	if err := s.quiet("with nobody holding a key", time.Now()); err != nil {
		return err
	}
	// ---- each in turn, then two at once
	for _, c := range s.clients {
		if err := s.press(c.who.name+" holding the key", c); err != nil {
			return err
		}
	}
	if err := s.press("ann and ben holding their keys at once", ann, ben); err != nil {
		return err
	}
	// ---- a press shorter than the gap between two passes of the poller
	if err := s.stillQuiet("before the 30 ms press"); err != nil {
		return err
	}
	if err := cy.say("down"); err != nil {
		return err
	}
	time.Sleep(30 * time.Millisecond) // the length of the press is the thing under test
	if err := cy.say("up"); err != nil {
		return err
	}
	s.presses[cy.who.id]++
	late, err := s.audited("after the presses")
	if err != nil {
		return err
	}
	h.say("ok   conch-voice: each in turn, and ann and ben at once, were heard by the others by their own tone and by nobody else; a speaker never counted itself; with every key up all three counted silence, for the whole of the time between presses, though every source kept playing")
	h.say("ok   conch-voice audit: every press, a 30 ms one included, is one reported voice_transmit_started and voice_transmit_stopped in order; no voice_transmit_unreported (presses recorded as reported late: %d, at most %d expected)", late, lateReports)

	// ---- a client whose reports say stopped while it keeps sending
	if err := s.misreported(ben, ann); err != nil {
		return err
	}
	// ---- a member is removed while another holds a key
	if err := s.removal(ctx, ann, ben, cy); err != nil {
		return err
	}
	// ---- LiveKit stops and starts again
	if err := s.outage(ctx, ann, ben); err != nil {
		return err
	}

	// ---- leaving
	for _, c := range s.clients {
		if err := c.say("quit"); err != nil {
			return err
		}
	}
	for _, c := range s.clients {
		select {
		case <-c.done:
		case <-time.After(30 * time.Second):
			return fmt.Errorf("%s's conch-voice was still running 30 s after quit", c.who.name)
		case <-ctx.Done():
			return errInterrupted
		}
		if c.exit != nil {
			return fmt.Errorf("%s's conch-voice ended with %s after quit, want exit status 0; standard error: %s", c.who.name, exitText(c.exit), c.stderrTail())
		}
	}
	h.say("ok   conch-voice: quit left the room and exited 0")
	return nil
}

// misreported makes liar's reports false: while liar holds the key, a
// `stopped` report is sent with liar's own credential, as a client that lies
// would send it, and liar goes on transmitting. conchd must record what it
// then sees as an unreported transmission.
//
// The report is sent by this program and not by conch-voice: the shipped
// client has no option to misreport, test-only or otherwise, and the server
// cannot tell who holds the credential.
func (s *squad) misreported(liar, listener *voiceClient) error {
	l := s.l
	if err := s.stillQuiet("before the misreported press"); err != nil {
		return err
	}
	asked := time.Now()
	if err := liar.say("down"); err != nil {
		return err
	}
	on, err := liar.wait("transmitting after down", 15*time.Second, asked, transmittingIs(true))
	if err != nil {
		return err
	}
	s.presses[liar.who.id]++
	if _, err := listener.waitStats(1, on.Add(hearMargin)); err != nil {
		return err
	}
	lied := time.Now()
	status, body, err := liar.who.api.do(http.MethodPost, "/v1/channels/squad/voice/transmit", schema.VoiceTransmitReportV1{State: schema.VoiceTransmitStateStopped})
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("a stopped report with %s's credential while %s transmits: HTTP %d %s, want 204", liar.who.name, liar.who.name, status, l.h.redact(truncate(string(body), 200)))
	}
	actor := fmt.Sprintf("principal:%d", liar.who.id)
	if err := waitFor("a voice_transmit_unreported row for the transmission "+liar.who.name+" reported as stopped", 30*time.Second, func() (bool, string) {
		unreported, _, _, err := l.transmitRows(s.subject, actor, lied)
		if err != nil {
			return false, err.Error()
		}
		return len(unreported) == 1, fmt.Sprintf("%d voice_transmit_unreported rows since the false report, want 1", len(unreported))
	}); err != nil {
		return err
	}
	s.provoked[liar.who.id]++
	// It was still being heard all the while.
	heardUntil := time.Now()
	if err := listener.heard("while "+liar.who.name+"'s reports said stopped", on.Add(hearMargin), heardUntil, 1, map[string]int{identity(liar.who.id): liar.tone}); err != nil {
		return err
	}
	released := time.Now()
	if err := liar.say("up"); err != nil {
		return err
	}
	off, err := liar.wait("not transmitting after up", 15*time.Second, released, transmittingIs(false))
	if err != nil {
		return err
	}
	if err := waitFor("the unreported transmission to be closed when "+liar.who.name+" mutes", 30*time.Second, func() (bool, string) {
		unreported, stops, _, err := l.transmitRows(s.subject, actor, lied)
		if err != nil {
			return false, err.Error()
		}
		return len(unreported) == 1 && len(stops) == 1 && strings.HasSuffix(stops[0].Detail, "reason=muted"),
			fmt.Sprintf("%d unreported rows and %d observed stops since the false report", len(unreported), len(stops))
	}); err != nil {
		return err
	}
	if err := s.quiet("after the misreported press", off); err != nil {
		return err
	}
	l.h.say("ok   conch-voice audit: a stopped report sent while ben kept transmitting was recorded as voice_transmit_unreported (source=observed) and closed with reason=muted when he released")
	return nil
}

// removal removes gone from the channel while holder holds the key: the room
// is rotated, holder and other join the new room without being restarted, the
// press ends with the room, a new press is heard in the new room, and gone's
// client has stopped with a message.
func (s *squad) removal(ctx context.Context, holder, other, gone *voiceClient) error {
	l, h := s.l, s.l.h
	if _, err := s.audited("before the removal"); err != nil {
		return err
	}
	if err := s.stillQuiet("before the press held across the removal"); err != nil {
		return err
	}
	asked := time.Now()
	if err := holder.say("down"); err != nil {
		return err
	}
	if _, err := holder.wait("transmitting after down", 15*time.Second, asked, transmittingIs(true)); err != nil {
		return err
	}
	before, err := l.rotations()
	if err != nil {
		return err
	}
	oldRooms, err := l.d.voiceRooms(s.channelID)
	if err != nil {
		return err
	}
	removedAt := time.Now()
	if err := l.d.operator.call(http.MethodDelete, fmt.Sprintf("/v1/channels/squad/members/%d", gone.who.id), nil, nil); err != nil {
		return err
	}
	if err := l.expectRotation(gone.who.name+"'s removal from squad", before, s.subject, store.VoiceRotateMemberRemoved, 30*time.Second); err != nil {
		return err
	}
	// The removed client stops, with a message and a status that is not 0.
	select {
	case <-gone.done:
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s's conch-voice was still running 30 s after %s was removed from the channel", gone.who.name, gone.who.name)
	case <-ctx.Done():
		return errInterrupted
	}
	var exitErr *exec.ExitError
	if !errors.As(gone.exit, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(gone.stderrTail(), "not a member of this channel") {
		return fmt.Errorf("after the removal %s's conch-voice ended with %s and said %q; want exit status 1 and a line saying they are not a member", gone.who.name, exitText(gone.exit), gone.stderrTail())
	}
	// The two who remain are told the room is gone and are in the new one,
	// the same processes.
	s.clients = []*voiceClient{holder, other}
	var back time.Time
	for _, c := range s.clients {
		if _, err := c.wait("told the room was deleted", 30*time.Second, removedAt, func(o map[string]any) bool {
			return o["event"] == "connection" && o["state"] == "waiting" && o["reason"] == "room_deleted"
		}); err != nil {
			return err
		}
		at, err := c.wait("connected to the new room", 30*time.Second, removedAt, connectionIs("connected"))
		if err != nil {
			return err
		}
		if at.After(back) {
			back = at
		}
	}
	var newRoom string
	if err := waitFor("LiveKit to list the two who remain in a new room, and nobody in the old", 30*time.Second, func() (bool, string) {
		rooms, err := l.d.voiceRooms(s.channelID)
		if err != nil || len(rooms) != 1 {
			return false, fmt.Sprintf("%d rooms stored for squad (%v)", len(rooms), err)
		}
		if contains(oldRooms, rooms[0]) {
			return false, "the stored room is still the old one"
		}
		newRoom = rooms[0]
		for _, c := range s.clients {
			if ok, _, saw := l.lkHas(newRoom, c.who.id); !ok {
				return false, saw
			}
			for _, old := range oldRooms {
				if gone, saw := l.lkGone(old, c.who.id); !gone {
					return false, c.who.name + " in the old room: " + saw
				}
			}
		}
		if ok, _, _ := l.lkHas(newRoom, gone.who.id); ok {
			return false, gone.who.name + " is in the new room"
		}
		return true, ""
	}); err != nil {
		return err
	}
	h.room(newRoom)
	// The press held across the rotation ended with it: the key is still
	// down, and the holder is not transmitting and does not resume.
	if _, err := holder.wait("not transmitting after the room was deleted", 15*time.Second, removedAt, transmittingIs(false)); err != nil {
		return err
	}
	for _, e := range holder.snapshot() {
		if e.at.After(back) && transmittingIs(true)(e.obj) {
			return fmt.Errorf("%s's press resumed by itself in the new room: a press held across a rotation must end with it", holder.who.name)
		}
	}
	if err := s.quiet("in the new room, with the key still held from before the rotation", back); err != nil {
		return err
	}
	if err := holder.say("up"); err != nil {
		return err
	}
	// The press the rotation cut short is in the audit log as opened by the
	// holder's report and closed by the server because the room went away:
	// one row each, in that order, and nothing else of the holder's since.
	reported := fmt.Sprintf("channel=%d audience=channel source=reported", s.channelID)
	holderActor := fmt.Sprintf("principal:%d", holder.who.id)
	if err := waitFor("the press held across the rotation to be closed in the audit log with reason=left", 30*time.Second, func() (bool, string) {
		all, err := l.d.audit()
		if err != nil {
			return false, err.Error()
		}
		var rows []string
		for _, e := range all {
			if e.Subject == s.subject && e.Actor == holderActor && !e.CreatedAt.Before(asked.Truncate(time.Millisecond)) && strings.HasPrefix(e.Action, "voice_transmit") {
				rows = append(rows, e.Action+" "+e.Detail)
			}
		}
		ok := len(rows) == 2 && rows[0] == store.AuditVoiceTransmitStarted+" "+reported &&
			strings.HasPrefix(rows[1], store.AuditVoiceTransmitStopped+" ") && strings.HasSuffix(rows[1], "reason=left")
		return ok, fmt.Sprintf("%q", rows)
	}); err != nil {
		return err
	}
	// From here the rows of the new room are counted on their own.
	// Since the removal, not since the last of them was back: each said it
	// was ready the moment it was connected itself. Neither wrote such a line
	// between the removal and the disconnect, because nothing of its own
	// state changed then.
	for _, c := range s.clients {
		if _, err := c.wait("ready to talk in the new room", 15*time.Second, removedAt, readyToTalk); err != nil {
			return err
		}
	}
	s.since, s.presses, s.provoked = time.Now(), map[int64]int{}, map[int64]int{}
	if err := s.press(holder.who.name+" holding the key in the new room", holder); err != nil {
		return err
	}
	if _, err := s.audited("in the new room"); err != nil {
		return err
	}
	h.say("ok   conch-voice: removing cy rotated the room; ann and ben joined the new one without a restart, the press ann held across the rotation ended with it (closed in the audit log with reason=left) and did not resume, a new press was heard there; cy's client stopped with exit status 1 and a message")
	return nil
}

// outage stops LiveKit and starts it again: the clients are not connected
// while it is down, reconnect by themselves when it is back, and exchange
// audio again.
func (s *squad) outage(ctx context.Context, a, b *voiceClient) error {
	l, h := s.l, s.l.h
	outagesBefore, err := l.auditCount(store.AuditVoiceEnforcementUnavailable, "", "")
	if err != nil {
		return err
	}
	stoppedAt := time.Now()
	if err := l.srv.stop(ctx); err != nil {
		return err
	}
	for _, c := range s.clients {
		if _, err := c.wait("not connected while LiveKit is down", 60*time.Second, stoppedAt, func(o map[string]any) bool {
			return o["event"] == "connection" && (o["state"] == "reconnecting" || o["state"] == "waiting")
		}); err != nil {
			return err
		}
	}
	// conchd has noticed too, so that the outage it audits is this one and
	// not the next scenario's.
	if err := l.waitAudit("one voice_enforcement_unavailable row for this outage", store.AuditVoiceEnforcementUnavailable, "", "", outagesBefore+1); err != nil {
		return err
	}
	// Nothing was heard or sent up to here, the outage so far included. (A
	// client prints stats only while connected, so this is what it has.)
	if err := s.stillQuiet("before the press that is refused"); err != nil {
		return err
	}
	// A press while not connected does nothing and says so.
	asked := time.Now()
	if err := a.say("down"); err != nil {
		return err
	}
	if _, err := a.wait("a press refused as not connected", 15*time.Second, asked, func(o map[string]any) bool {
		return o["event"] == "press_ignored" && o["reason"] == "not_connected"
	}); err != nil {
		return err
	}
	if err := a.say("up"); err != nil {
		return err
	}
	startedAt := time.Now()
	if err := l.srv.start(ctx); err != nil {
		return err
	}
	var back time.Time
	for _, c := range s.clients {
		at, err := c.wait("connected again after LiveKit came back", 3*time.Minute, startedAt, connectionIs("connected"))
		if err != nil {
			return err
		}
		if at.After(back) {
			back = at
		}
	}
	for _, c := range s.clients {
		if _, err := c.wait("ready to talk after LiveKit came back", 30*time.Second, startedAt, readyToTalk); err != nil {
			return err
		}
	}
	rooms, err := l.d.voiceRooms(s.channelID)
	if err != nil {
		return err
	}
	for _, r := range rooms {
		h.room(r)
	}
	s.since, s.presses, s.provoked = time.Now(), map[int64]int{}, map[int64]int{}
	if err := s.press(b.who.name+" holding the key after LiveKit came back", b); err != nil {
		return err
	}
	if _, err := s.audited("after LiveKit came back"); err != nil {
		return err
	}
	h.say("ok   conch-voice: LiveKit stopped and started again; while it was down a press was refused as not connected; ann and ben reconnected by themselves %s after it came back and were heard again", back.Sub(startedAt).Round(100*time.Millisecond))
	return nil
}
