//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The managed-voice check (issue #542, docs/design/tui-voice.md): voice inside
// the TUI, end to end, with nothing but synthetic sources. It runs only when
// asked, as `go run ./e2e/voice managed`: it needs Docker for LiveKit and the
// pinned Rust toolchain for conch-voice, which ordinary `go test ./...` and
// CI's other jobs do not have.
//
// Real `conch` TUIs run on pseudo-terminals against a real conchd and LiveKit.
// Each finds conch-voice through CONCH_VOICE_HELPER, as a person may point it,
// and starts it itself, as its own child, for each voice session. Here that
// variable names a wrapper which runs the real conch-voice with its test-only
// options added: a tone for a microphone, a counting sink for speakers, and a
// named pipe this program writes key records to for the keyboard. conch
// itself never passes those options. What is heard is counted by a plain
// `conch-voice join` (ben, no microphone); LiveKit's admin API says who is in
// the room; /proc says when a helper has exited; the TUI's screen says what
// it shows.
//
// It checks: joining from the TUI with another channel on screen; that typing
// transmits nothing and the talk key does; mute; deafen, and that undeafening
// leaves mute as it was; a second TUI with a helper of its own; killing a TUI
// while its key is held (its helper stops sending at once and exits within
// the contract's bound, and the other TUI's helper carries on); a new TUI
// joining again; leaving; quitting while the key is held; one account in two
// TUIs (the second is warned, then displaces the first, which says so);
// retry; a removal from the channel, after which the TUI says why voice
// stopped and text still works; a member whose invite withheld voice, and a
// TUI whose stored login has become another account's, each refused by the
// helper with the reason in the TUI and nothing captured or joined; LiveKit
// restarted under a TUI in voice, which reconnects by itself and is heard
// again; and LiveKit down past every limit the helper puts on one outage,
// which it waits out without giving up (issue #559). No microphone, speaker
// or keyboard of the machine is used, and none of this is the maintainer's
// acceptance with a real headset and key, which stays a separate, manual
// check.

const (
	// keyTalk is KEY_F13, the talk key the check's voice.toml binds.
	keyTalk = 183
	evSyn   = 0
	evKey   = 1
	// managedVoiceTOML binds the talk key and nothing else.
	managedVoiceTOML = "[keys]\ntalk = \"KEY_F13\"\n"

	// ownerLossBound is how long a helper may take to exit once its owner
	// has been killed: the contract's bound (the end of its standard input
	// is noticed at once), and half a second for /proc to show it.
	ownerLossBound = time.Duration(schema.VoiceControlOwnerLossExitMS+500) * time.Millisecond
	// leaveBound is the same for /voice leave, from the command typed.
	leaveBound = time.Duration(schema.VoiceControlLeaveExitMS+1000) * time.Millisecond
	// quitBound is how long the TUI may take to exit while its helper is
	// running: its Close is bounded by StopKill and a second.
	quitBound = time.Duration(schema.VoiceControlStopKillAfterMS)*time.Millisecond + 2*time.Second
)

// keyRecord is one Linux input_event of 64-bit Linux: 16 bytes of time, then
// type, code and value, little-endian. The time is made up.
func keyRecord(kind, code uint16, value int32) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint64(b[0:], 1700000000)
	binary.LittleEndian.PutUint16(b[16:], kind)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value)) // #nosec G115 -- 0 or 1
	return b
}

// runManaged is `go run ./e2e/voice managed`.
func runManaged(ctx context.Context, h *harness) (string, error) {
	if err := h.build(); err != nil {
		return "", fmt.Errorf("build binaries: %w", err)
	}
	h.say("== managed voice: real TUIs, their own conch-voice helpers, LiveKit in Docker ==")
	lv, reason, err := prepareLive(ctx, h)
	if err != nil {
		return "", err
	}
	if reason != "" {
		if inCI() {
			return "", fmt.Errorf("the managed-voice check cannot run in CI: %s", reason)
		}
		return "the managed-voice check: " + reason, nil
	}
	if lv.voiceBin == "" {
		return "the managed-voice check: " + lv.voiceSkip, nil
	}
	srv, err := h.kit.StartLiveKit(ctx)
	if err != nil {
		return "", err
	}
	lv.srv, lv.adm = srv, srv.Admin()
	h.say("ok   LiveKit %s is up on %s (container %s, auto_create off)", "v1.13.7", srv.APIURL, srv.Name)
	d, err := h.startConchd("managed", settingsOf(srv))
	if err != nil {
		return "", err
	}
	lv.d = d
	if err := lv.managedVoice(ctx); err != nil {
		return "", fmt.Errorf("managed voice: %w", err)
	}
	if err := d.scanAudit("managed"); err != nil {
		return "", err
	}
	h.say("ok   no token, secret or room name in this output, conchd's log, the TUIs' screens or the audit log")
	return "", nil
}

// seat is one person at a TUI: their conch configuration (from conch login)
// with a voice.toml that binds the talk key, the named pipe that is their
// keyboard, and the wrapper their TUI starts as conch-voice.
type seat struct {
	l    *live
	who  *person
	name string
	env  []string
	keys *os.File
	tone int
	// screen is the channel on screen when the TUI starts: the first of
	// its list.
	screen string
	tui    *tuiProc
	helper helperProc
}

// safeDir is a private directory where the TUI's trust checks accept a
// program: it and every parent are the user's or root's and not writable by
// others, which the system's temporary directory is not.
func (h *harness) safeDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	parent := filepath.Join(base, "conch-voice-check")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "managed-")
	if err != nil {
		return "", err
	}
	h.onCleanup(func() { _ = os.RemoveAll(dir) })
	return dir, nil
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// newSeat sets up p at a TUI whose channel list is channels.
func (l *live) newSeat(p *person, name string, tone int, channels string) (*seat, error) {
	h := l.h
	dir, err := h.safeDir()
	if err != nil {
		return nil, err
	}
	pipe := filepath.Join(dir, "keyboard")
	if err := mkfifo(pipe); err != nil {
		return nil, err
	}
	// Held open for the whole run, read and write: a helper's open of it
	// never waits, and its read waits for the next record.
	keys, err := os.OpenFile(pipe, os.O_RDWR, 0) // #nosec G304 -- this program's own pipe
	if err != nil {
		return nil, err
	}
	h.onCleanup(func() { _ = keys.Close() })
	wrapper := filepath.Join(dir, "conch-voice")
	script := "#!/bin/sh\n" +
		"# conch-voice with the managed-voice check's test options; conch never passes them.\n" +
		"exec " + shq(l.voiceBin) + " \"$@\" --mic tone:" + strconv.Itoa(tone) + " --sink count --test-key-device " + shq(pipe) + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { // #nosec G306 -- an executable only this user may change
		return nil, err
	}
	var config string
	for _, kv := range p.cli.env {
		if v, ok := strings.CutPrefix(kv, "XDG_CONFIG_HOME="); ok {
			config = filepath.Join(v, "conch")
		}
	}
	if config == "" {
		return nil, fmt.Errorf("%s has no configuration directory", name)
	}
	if err := os.WriteFile(filepath.Join(config, "voice.toml"), []byte(managedVoiceTOML), 0o600); err != nil {
		return nil, err
	}
	env := append(slices.Clone(p.cli.env), "TERM=xterm-256color", "CONCH_MOUSE=0", "CONCH_CHANNELS="+channels, "CONCH_VOICE_HELPER="+wrapper)
	screen, _, _ := strings.Cut(channels, ",")
	return &seat{l: l, who: p, name: name, env: env, keys: keys, tone: tone, screen: screen}, nil
}

func (s *seat) start() error {
	t, err := s.l.h.startTUI(s.name, s.env)
	if err != nil {
		return err
	}
	s.tui, s.helper = t, helperProc{}
	return nil
}

func (s *seat) press() error {
	_, err := s.keys.Write(append(keyRecord(evKey, keyTalk, 1), keyRecord(evSyn, 0, 0)...))
	return err
}

func (s *seat) release() error {
	_, err := s.keys.Write(append(keyRecord(evKey, keyTalk, 0), keyRecord(evSyn, 0, 0)...))
	return err
}

// command types line and enter, and waits until the TUI has written each of
// wants since: the answer in the status line and the state in the strip,
// which arrive in either order.
func (s *seat) command(line string, wants ...string) error {
	mark := s.tui.mark()
	if err := s.tui.typeText(line + "\r"); err != nil {
		return err
	}
	for _, want := range wants {
		if err := s.tui.waitText(strconv.Quote(want)+" after "+line, mark, 60*time.Second, want); err != nil {
			return err
		}
	}
	return nil
}

// joinedSquad is the strip of a helper connected to squad with nothing in the
// way of a press.
const joinedSquad = "voice: #squad · mic on · hold the talk key to talk"

// joinSquad joins squad's voice from the TUI and finds the helper it started.
// From a TUI with another channel on screen, whose presence it does not read,
// the first /voice join squad only warns that this account may already be in
// squad's voice elsewhere, and the repeat joins (design note §11).
func (s *seat) joinSquad() error {
	if s.screen != "squad" {
		if err := s.command("/voice join squad", "voice: maybe in #squad elsewhere · repeat /voice join #squad within 10 s"); err != nil {
			return err
		}
		if _, ok := s.tui.helperOf(); ok {
			return fmt.Errorf("%s's TUI started conch-voice before /voice join squad was repeated", s.name)
		}
	}
	if err := s.command("/voice join squad", "voice: started for #squad", joinedSquad); err != nil {
		return err
	}
	return s.findHelper()
}

func (s *seat) findHelper() error {
	return waitFor(s.name+"'s TUI to have a conch-voice child", 10*time.Second, func() (bool, string) {
		p, ok := s.tui.helperOf()
		if ok {
			s.helper = p
			s.l.h.onCleanup(p.kill)
		}
		return ok, "no conch-voice child"
	})
}

// helperGone waits until s's helper has exited and returns how long that took
// from since.
func (s *seat) helperGone(what string, since time.Time, within time.Duration) (time.Duration, error) {
	p := s.helper
	var took time.Duration
	err := waitFor(s.name+"'s conch-voice (pid "+strconv.Itoa(p.pid)+") to exit "+what, within+5*time.Second, func() (bool, string) {
		if p.alive() {
			return false, "still running"
		}
		took = time.Since(since)
		return true, ""
	})
	if err == nil && took > within {
		err = fmt.Errorf("%s's conch-voice exited %s after %s; the bound is %s", s.name, took.Round(time.Millisecond), what, within)
	}
	return took, err
}

// talk holds s's talk key until the ears have counted two whole seconds inside
// the press, checks that they heard s's tone (heard) or nothing at all,
// releases it, and checks that they hear nothing afterwards.
func (l *live) talk(s *seat, ears *voiceClient, what string, heard bool) error {
	mark := s.tui.mark()
	if err := s.press(); err != nil {
		return err
	}
	on := time.Now()
	if heard {
		if err := s.tui.waitText("TALKING", mark, 15*time.Second, "voice: #squad · TALKING"); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		on = time.Now()
	}
	from := on.Add(hearMargin)
	to, err := ears.waitStats(2, from)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	want := map[string]int{}
	if heard {
		want[identity(s.who.id)] = s.tone
	}
	if err := ears.heard(what, from, to, 2, want); err != nil {
		return err
	}
	if !heard && strings.Contains(s.tui.textSince(mark), "TALKING") {
		return fmt.Errorf("%s: %s's TUI showed TALKING", what, s.name)
	}
	mark = s.tui.mark()
	if err := s.release(); err != nil {
		return err
	}
	if heard {
		if err := s.tui.waitText("the end of TALKING", mark, 15*time.Second, joinedSquad); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return quietAfter(ears, what+", after the release", time.Now())
}

// quietAfter checks that the ears count nothing from anyone for one whole
// second that starts after since.
func quietAfter(ears *voiceClient, what string, since time.Time) error {
	from := since.Add(hearMargin)
	to, err := ears.waitStats(1, from)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return ears.heard(what, from, to, 1, nil)
}

// inRoom waits until LiveKit lists p in one of channel's rooms with a
// published microphone, muted while nobody holds a key, and returns the room.
func (l *live) inRoom(channelID int64, p *person) (string, error) {
	var room string
	err := waitFor("LiveKit to list "+p.name+" in squad's room with a muted microphone", 30*time.Second, func() (bool, string) {
		rooms, err := l.d.voiceRooms(channelID)
		if err != nil {
			return false, err.Error()
		}
		saw := fmt.Sprintf("%d rooms", len(rooms))
		for _, r := range rooms {
			ok, part, s := l.lkHas(r, p.id)
			if !ok {
				saw = s
				continue
			}
			if t, has := part.Mic(); !has || !t.Muted {
				return false, fmt.Sprintf("microphone published=%v muted=%v", has, t.Muted)
			}
			room = r
			return true, ""
		}
		return false, saw
	})
	return room, err
}

// outOfRooms waits until LiveKit lists p in none of channel's rooms.
func (l *live) outOfRooms(channelID int64, p *person, what string) error {
	return waitFor("LiveKit to list "+p.name+" in no room of squad "+what, 15*time.Second, func() (bool, string) {
		rooms, err := l.d.voiceRooms(channelID)
		if err != nil {
			return false, err.Error()
		}
		for _, r := range rooms {
			if gone, saw := l.lkGone(r, p.id); !gone {
				return false, saw
			}
		}
		return true, ""
	})
}

// managedVoice is the check.
func (l *live) managedVoice(ctx context.Context) error {
	h, d := l.h, l.d
	squadID, err := d.createChannel("squad")
	if err != nil {
		return err
	}
	if _, err := d.createChannel("lobby"); err != nil {
		return err
	}
	ann, err := h.newPerson(d, "ann", "squad", "lobby")
	if err != nil {
		return err
	}
	ben, err := h.newPerson(d, "ben", "squad")
	if err != nil {
		return err
	}
	cy, err := h.newPerson(d, "cy", "squad")
	if err != nil {
		return err
	}

	// Ben listens with plain conch-voice: a counting sink and no microphone.
	started := time.Now()
	ears, err := l.startVoiceClient(ben, "squad", "none", 0)
	if err != nil {
		return err
	}
	connectedEvent := func(o map[string]any) bool { return o["event"] == "connection" && o["state"] == "connected" }
	if _, err := ears.wait("connected", 60*time.Second, started, connectedEvent); err != nil {
		return err
	}

	// ---- joining from the TUI, another channel on screen
	annSeat, err := l.newSeat(ann, "ann", toneAnn, "lobby,squad")
	if err != nil {
		return err
	}
	if err := annSeat.start(); err != nil {
		return err
	}
	mark := annSeat.tui.mark()
	if err := annSeat.joinSquad(); err != nil {
		return err
	}
	room, err := l.inRoom(squadID, ann)
	if err != nil {
		return err
	}
	h.room(room)
	if !strings.Contains(annSeat.tui.textSince(mark), joinedSquad+" · #lobby room:") {
		return fmt.Errorf("ann's strip does not keep squad as the destination and lobby's room apart: %q", lastLine(annSeat.tui.textSince(mark)))
	}
	h.say("ok   managed: in her TUI, on lobby, ann typed /voice join squad, was warned she may be in its voice elsewhere, and repeated it; the TUI started its own conch-voice (pid %d), which joined squad's room with the microphone muted; the strip names squad, and lobby's room apart", annSeat.helper.pid)

	// ---- typing transmits nothing; the talk key does
	typed := "hello, nothing here talks"
	if err := annSeat.tui.typeText(typed); err != nil {
		return err
	}
	typing := time.Now()
	if to, err := ears.waitStats(2, typing.Add(hearMargin)); err != nil {
		return err
	} else if err := ears.heard("ann typing in her TUI", typing.Add(hearMargin), to, 2, nil); err != nil {
		return err
	}
	if err := annSeat.tui.typeText(strings.Repeat("\x7f", len(typed))); err != nil {
		return err
	}
	if err := l.talk(annSeat, ears, "ann holding the talk key", true); err != nil {
		return err
	}
	h.say("ok   managed: typing in the TUI sent nothing; holding the talk key showed TALKING and ben heard ann's tone, and nothing once it was released")

	// ---- mute and deafen
	if err := annSeat.command("/voice mute on", "voice: mute on", "voice: #squad · mic on · muted"); err != nil {
		return err
	}
	if err := l.talk(annSeat, ears, "ann holding the talk key while muted", false); err != nil {
		return err
	}
	if err := annSeat.command("/voice mute off", "voice: mute off", joinedSquad); err != nil {
		return err
	}
	if err := annSeat.command("/voice deafen on", "voice: deafen on", "voice: #squad · mic on · deafened"); err != nil {
		return err
	}
	if err := l.talk(annSeat, ears, "ann holding the talk key while deafened", false); err != nil {
		return err
	}
	if err := annSeat.command("/voice deafen off", "voice: deafen off", joinedSquad); err != nil {
		return err
	}
	if err := l.talk(annSeat, ears, "ann holding the talk key after undeafening", true); err != nil {
		return err
	}
	h.say("ok   managed: muted, and then deafened, the key sent nothing; undeafened, mute stayed off and the key was heard again")

	// ---- a second TUI, with its own helper
	cySeat, err := l.newSeat(cy, "cy", toneCy, "squad")
	if err != nil {
		return err
	}
	if err := cySeat.start(); err != nil {
		return err
	}
	if err := cySeat.joinSquad(); err != nil {
		return err
	}
	if _, err := l.inRoom(squadID, cy); err != nil {
		return err
	}

	// ---- ann's TUI is killed while she holds the key
	mark = annSeat.tui.mark()
	if err := annSeat.press(); err != nil {
		return err
	}
	if err := annSeat.tui.waitText("TALKING", mark, 15*time.Second, "voice: #squad · TALKING"); err != nil {
		return err
	}
	on := time.Now()
	to, err := ears.waitStats(2, on.Add(hearMargin))
	if err != nil {
		return err
	}
	if err := ears.heard("ann holding the key before her TUI is killed", on.Add(hearMargin), to, 2, map[string]int{identity(ann.id): toneAnn}); err != nil {
		return err
	}
	killed := time.Now()
	annSeat.tui.kill()
	took, err := annSeat.helperGone("after its TUI was killed with SIGKILL", killed, ownerLossBound)
	if err != nil {
		return err
	}
	if err := quietAfter(ears, "after ann's TUI was killed with her key held", killed); err != nil {
		return err
	}
	if err := l.outOfRooms(squadID, ann, "after her TUI was killed"); err != nil {
		return err
	}
	if !cySeat.helper.alive() || cySeat.tui.exited() {
		return fmt.Errorf("cy's TUI or conch-voice ended when ann's TUI was killed")
	}
	if ok, _, saw := l.lkHas(room, cy.id); !ok {
		return fmt.Errorf("cy left squad's room when ann's TUI was killed (%s)", saw)
	}
	h.say("ok   managed: ann's TUI killed with SIGKILL while she held the key: her conch-voice exited %s later (bound %s), ben heard nothing from a moment after, LiveKit dropped her; cy's TUI and conch-voice carried on", took.Round(time.Millisecond), ownerLossBound)

	// ---- a new TUI joins again; leave
	if err := annSeat.start(); err != nil {
		return err
	}
	if err := annSeat.joinSquad(); err != nil {
		return err
	}
	if _, err := l.inRoom(squadID, ann); err != nil {
		return err
	}
	if err := l.talk(annSeat, ears, "ann holding the key in a new TUI", true); err != nil {
		return err
	}
	left := time.Now()
	if err := annSeat.command("/voice leave", "voice: left", "voice off · /voice join"); err != nil {
		return err
	}
	took, err = annSeat.helperGone("after /voice leave", left, leaveBound)
	if err != nil {
		return err
	}
	if err := l.outOfRooms(squadID, ann, "after /voice leave"); err != nil {
		return err
	}
	h.say("ok   managed: a new TUI joined again and was heard; /voice leave stopped its conch-voice in %s and LiveKit dropped her", took.Round(time.Millisecond))

	// ---- quitting the TUI while the key is held
	if err := annSeat.joinSquad(); err != nil {
		return err
	}
	mark = annSeat.tui.mark()
	if err := annSeat.press(); err != nil {
		return err
	}
	if err := annSeat.tui.waitText("TALKING", mark, 15*time.Second, "voice: #squad · TALKING"); err != nil {
		return err
	}
	quit := time.Now()
	if err := annSeat.tui.typeText("\x1b"); err != nil {
		return err
	}
	select {
	case <-annSeat.tui.done:
	case <-time.After(quitBound + 5*time.Second):
		return fmt.Errorf("ann's TUI was still running %s after esc", quitBound+5*time.Second)
	case <-ctx.Done():
		return errInterrupted
	}
	exitTook := time.Since(quit)
	if exitTook > quitBound {
		return fmt.Errorf("ann's TUI took %s to quit with voice on; the bound is %s", exitTook.Round(time.Millisecond), quitBound)
	}
	if annSeat.helper.alive() {
		return fmt.Errorf("ann's conch-voice (pid %d) was still running after her TUI had quit", annSeat.helper.pid)
	}
	if err := annSeat.release(); err != nil {
		return err
	}
	if err := quietAfter(ears, "after ann quit her TUI with her key held", quit); err != nil {
		return err
	}
	if err := l.outOfRooms(squadID, ann, "after her TUI quit"); err != nil {
		return err
	}
	h.say("ok   managed: esc with the key held: the TUI stopped its conch-voice and quit in %s (bound %s); ben heard nothing after, LiveKit dropped her", exitTook.Round(time.Millisecond), quitBound)

	// ---- one account, two TUIs
	if err := annSeat.start(); err != nil {
		return err
	}
	if err := annSeat.joinSquad(); err != nil {
		return err
	}
	other, err := h.newConchUser(d.baseURL, ann.token)
	if err != nil {
		return err
	}
	ann2 := *ann
	ann2.cli = other
	twinSeat, err := l.newSeat(&ann2, "ann-2", toneAnn, "squad")
	if err != nil {
		return err
	}
	if err := twinSeat.start(); err != nil {
		return err
	}
	if err := twinSeat.tui.waitText("ann in squad's room from her other TUI", 0, 30*time.Second, "you: in #squad voice from another client"); err != nil {
		return err
	}
	if err := twinSeat.command("/voice join", "voice: you are in #squad elsewhere · /voice join again within 10 s takes over"); err != nil {
		return err
	}
	if _, ok := twinSeat.tui.helperOf(); ok {
		return fmt.Errorf("the second TUI started conch-voice before /voice join was repeated")
	}
	firstMark := annSeat.tui.mark()
	if err := twinSeat.command("/voice join", "voice: started for #squad", joinedSquad); err != nil {
		return err
	}
	if err := annSeat.tui.waitText("that voice was taken over", firstMark, 60*time.Second, "voice stopped: this account joined voice elsewhere"); err != nil {
		return err
	}
	if _, err := annSeat.helperGone("after another TUI took over", time.Now(), ownerLossBound); err != nil {
		return err
	}
	if err := twinSeat.command("/voice leave", "voice: left"); err != nil {
		return err
	}
	twinSeat.tui.kill()
	// Typed as soon as the helper's process is gone: the supervisor may still
	// be reading the last of its pipes, and the retry waits for that rather
	// than being refused. Only the strip is waited for: the status line says
	// "started for #squad" as it did after the join before, and a row that
	// does not change is not written again.
	if err := annSeat.command("/voice retry", joinedSquad); err != nil {
		return err
	}
	if err := annSeat.findHelper(); err != nil {
		return err
	}
	h.say("ok   managed: ann in a second TUI was warned that joining would disconnect her first, joined on the repeat; the first said this account joined voice elsewhere, its conch-voice exited, and /voice retry took voice back")

	// ---- removed from the channel: voice stops, and says why; text goes on
	mark = annSeat.tui.mark()
	if err := d.operator.call(http.MethodDelete, fmt.Sprintf("/v1/channels/squad/members/%d", ann.id), nil, nil); err != nil {
		return err
	}
	if err := annSeat.tui.waitText("why voice stopped", mark, 90*time.Second, "voice stopped: not a member of this channel"); err != nil {
		return err
	}
	if _, err := annSeat.helperGone("after ann was removed from squad", time.Now(), ownerLossBound); err != nil {
		return err
	}
	if err := l.outOfRooms(squadID, ann, "after her removal"); err != nil {
		return err
	}
	const still = "still here after voice stopped"
	if err := annSeat.command(still, "sent"); err != nil {
		return err
	}
	var page schema.ListMessagesResponseV2
	if err := ann.api.call(http.MethodGet, "/v2/channels/lobby/messages?order=desc&limit=50", nil, &page); err != nil {
		return err
	}
	found := false
	for _, m := range page.Messages {
		found = found || (m.AuthorID == ann.id && m.Body == still)
	}
	if !found {
		return fmt.Errorf("ann's message from her TUI after voice stopped is not in lobby")
	}
	h.say("ok   managed: ann removed from squad: her TUI said voice stopped, not a member of this channel; its conch-voice exited and LiveKit dropped her; a message typed afterwards reached lobby")

	// ---- refused voice, a restart of LiveKit, and an outage waited out
	// (issue #559, managed_paths_linux.go)
	if err := l.managedRefused(squadID, ben, ears); err != nil {
		return err
	}
	if err := l.managedReconnect(ctx, squadID, cySeat, ears); err != nil {
		return err
	}
	if err := l.managedOutage(ctx, squadID, cySeat, ears); err != nil {
		return err
	}

	// ---- the end
	for _, s := range []*seat{annSeat, cySeat} {
		if err := s.tui.typeText("\x1b"); err != nil {
			return err
		}
		select {
		case <-s.tui.done:
		case <-time.After(quitBound + 5*time.Second):
			return fmt.Errorf("%s's TUI was still running after esc", s.name)
		}
	}
	if cySeat.helper.alive() {
		return fmt.Errorf("cy's conch-voice was still running after her TUI had quit")
	}
	if err := ears.say("quit"); err != nil {
		return err
	}
	select {
	case <-ears.done:
	case <-time.After(30 * time.Second):
		return fmt.Errorf("ben's conch-voice was still running 30 s after quit")
	}
	return nil
}
