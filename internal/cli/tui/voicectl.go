package tui

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/cli/voicehelper"
	"github.com/njdaniel/conch/pkg/schema"
)

// Voice in the TUI (docs/design/tui-voice.md §15): the /voice commands drive
// a voicehelper.Supervisor, which owns one conch-voice helper process per
// voice session, and the voice strip shows what the helper confirmed. Every
// call to the supervisor runs in a tea.Cmd, never in Update, so text never
// waits on voice; a failed or missing helper changes nothing but the strip
// and the status line.

// voiceController is the part of voicehelper.Supervisor the TUI uses. Tests
// stand in for it.
type voiceController interface {
	Join(ctx context.Context, channel string) error
	Leave(ctx context.Context) error
	Retry(ctx context.Context) error
	SetMute(ctx context.Context, on bool) error
	SetDeafen(ctx context.Context, on bool) error
	Status() voicehelper.Status
	Updates() <-chan voicehelper.Status
	Diagnostics() []string
	HelperPath() string
	Close(ctx context.Context) error
}

// voiceCloseTimeout bounds closing the supervisor: its own stop ends in
// SIGKILL at StopKill and a reap, and a second more covers that (§8.5).
const voiceCloseTimeout = time.Duration(schema.VoiceControlStopKillAfterMS)*time.Millisecond + time.Second

// voiceConfirmWindow is how long a repeated /voice join counts as going ahead
// when presence shows this account already in that channel's voice (§11).
const voiceConfirmWindow = 10 * time.Second

// VoiceSetup is what the TUI needs to give voice to the person signed in:
// the server as the credential store keys it, the configuration directory,
// and where the TUI's own login came from. VoiceSetupFor builds it.
type VoiceSetup struct {
	server     string
	configDir  string
	credential schema.VoiceControlCredential // "" with no login
	token      string                        // only with credential env
	helperPath string
	// err is why voice cannot be offered at all: an unusable server address,
	// configuration directory or credentials file.
	err error
}

// VoiceSetupFor reads, for server, what voice needs: the server normalized,
// the configuration directory, and the TUI's login and where it came from.
// The login is passed on only as conch-voice already takes one: the stored
// login by name (stored), or CONCH_TOKEN in the helper's environment when the
// TUI's own token came from there (env). With no login at all voice is
// refused when asked for: it needs a signed-in person. The helper is found as
// CONCH_VOICE_HELPER says, or beside conch, or in PATH; nothing here can give
// it test options.
func VoiceSetupFor(server string) VoiceSetup {
	s := VoiceSetup{helperPath: voicehelper.HelperPathFromEnv()}
	key, err := cli.NormalizeServer(server)
	if err != nil {
		s.err = err
		return s
	}
	s.server = key
	dir, err := cli.ConfigDir()
	if err != nil {
		s.err = err
		return s
	}
	s.configDir = dir
	token, fromEnv, err := cli.LoadCredential(server)
	switch {
	case err != nil:
		s.err = err
	case token == "":
	case fromEnv:
		s.credential, s.token = schema.VoiceControlCredentialEnv, token
	default:
		s.credential = schema.VoiceControlCredentialStored
	}
	return s
}

// factory makes the supervisor for one principal, or says why there can be
// none.
func (s VoiceSetup) factory() func(principal int64) (voiceController, error) {
	return func(principal int64) (voiceController, error) {
		switch {
		case s.err != nil:
			return nil, s.err
		case s.credential == "":
			return nil, voicehelper.ErrNoCredential
		}
		return voicehelper.New(voicehelper.Profile{
			Server: s.server, ConfigDir: s.configDir, PrincipalID: principal,
			Credential: s.credential, Token: s.token,
		}, voicehelper.Options{HelperPath: s.helperPath}), nil
	}
}

// voiceOwner holds the TUI's one supervisor. Like ownSession it is shared by
// every copy of the model, so that Run can close it after the program has
// returned.
type voiceOwner struct {
	mu        sync.Mutex
	make      func(principal int64) (voiceController, error)
	ctl       voiceController
	principal int64
	// serial numbers the supervisors made, so that an update from one that
	// has since been dropped is ignored.
	serial int
	// done is closed when ctl is dropped; it ends ctl's update waiter.
	done chan struct{}
	// shut is set once the TUI is ending, or its person signed out: no
	// supervisor is made after it.
	shut bool
	// closing counts the supervisors dropped and not yet closed. While one
	// is, no other is made (its helper may still be stopping), and the TUI's
	// exit waits for them (closeVoice).
	closing int
	closed  sync.WaitGroup
}

var (
	errVoiceShut     = errors.New("voice: signed out; restart conch to use voice")
	errVoiceStopping = errors.New("voice: still stopping the voice of the previous account; try again in a moment")
)

// get is the supervisor for principal, made now if there is none. made says
// it is new, and the caller starts its update waiter.
func (o *voiceOwner) get(principal int64) (ctl voiceController, serial int, done chan struct{}, made bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.shut {
		return nil, 0, nil, false, errVoiceShut
	}
	if o.ctl != nil && o.principal == principal {
		return o.ctl, o.serial, o.done, false, nil
	}
	if o.ctl != nil || o.closing > 0 {
		// Belongs to someone else, or one dropped is still stopping: no
		// second helper until it has been reaped.
		return nil, 0, nil, false, errVoiceStopping
	}
	c, err := o.make(principal)
	if err != nil {
		return nil, 0, nil, false, err
	}
	o.ctl, o.principal = c, principal
	o.serial++
	o.done = make(chan struct{})
	return c, o.serial, o.done, true, nil
}

// current is the supervisor there is, its serial and the channel closed when
// it is dropped; nil and 0 for none.
func (o *voiceOwner) current() (voiceController, int, chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ctl == nil {
		return nil, 0, nil
	}
	return o.ctl, o.serial, o.done
}

// heldForOther reports whether the supervisor there is was made for another
// principal than principal.
func (o *voiceOwner) heldForOther(principal int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ctl != nil && o.principal != principal
}

// drop detaches the supervisor and ends its update waiter. With shut set no
// other is made. It returns nil when there was none; otherwise the caller
// closes it and then calls closed, once, and until then get makes no other
// and closeVoice waits.
func (o *voiceOwner) drop(shut bool) (c voiceController, closed func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.shut = o.shut || shut
	c = o.ctl
	if c == nil {
		return nil, func() {}
	}
	close(o.done)
	o.ctl, o.principal, o.done = nil, 0, nil
	o.closing++
	o.closed.Add(1)
	var once sync.Once
	return c, func() {
		once.Do(func() {
			o.mu.Lock()
			o.closing--
			o.mu.Unlock()
			o.closed.Done()
		})
	}
}

// awaitClosed waits for every supervisor dropped to have been closed, or for
// ctx to end.
func (o *voiceOwner) awaitClosed(ctx context.Context) {
	done := make(chan struct{})
	go func() { o.closed.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// closeVoice stops the helper the TUI owns, if any, and waits for it and for
// any being stopped already (after a sign-out or another account), all
// bounded by voiceCloseTimeout. Run calls it on the way out, before anything
// else, whatever ended the program.
func (m Model) closeVoice() {
	if m.vc.owner == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), voiceCloseTimeout)
	defer cancel()
	if c, closed := m.vc.owner.drop(true); c != nil {
		_ = c.Close(ctx)
		closed()
	}
	m.vc.owner.awaitClosed(ctx)
}

// voiceCtl is the TUI's side of voice: the latest status the supervisor
// published, and what the person has asked for and not yet had answered.
type voiceCtl struct {
	// owner is nil when this terminal does not offer voice (Run was given no
	// setup): /voice says so and the strip is #538's, presence only.
	owner *voiceOwner
	// status is the latest Status from the supervisor's Updates; the strip
	// is drawn from it alone.
	status voicehelper.Status
	// busy is the command in flight ("joining #general", "muting"), "" for
	// none. It is shown as asked for, never as done.
	busy string
	// confirm is the channel that a repeated /voice join within
	// voiceConfirmWindow of confirmAt joins although this account is in its
	// voice from another client.
	confirm   string
	confirmAt time.Time
	// confirmKnown is whether presence showed this account in that voice
	// (the channel on screen) or could not tell (any other channel).
	confirmKnown bool
	// left is this TUI's own session as presence showed it when /voice leave
	// ended it, so that presence still listing it afterwards is not taken for
	// another client (§11). A join clears it.
	left voiceLeft
	// path and diag are the helper's path and log tail as /voice status
	// last read them, for its page.
	path string
	diag []string
}

// voiceLeft is a session this TUI left: the channel, the account, and the
// join times presence gave that account in the channel's room. Presence is
// read on a sweep, so it can go on listing the session for a while after the
// helper has gone. The server gives each connection its own joined_at, so an
// entry with one of these join times is the session that was left, and any
// other entry of the account is another client.
type voiceLeft struct {
	channel   string
	principal int64
	joined    []schema.Timestamp
}

// leaving is the session this TUI's helper has in the channel on screen, as
// presence shows it now: what /voice leave records. It is empty when presence
// says nothing of it, and when the helper is not in voice there: a failed
// helper may have been displaced by another client, whose entry this would
// otherwise record as its own.
func (m Model) leaving() voiceLeft {
	if !m.vc.voiceActive() || m.vc.status.Channel != m.current() || m.voice.state != voiceLive ||
		!m.voice.doc.Available || m.authorID <= 0 {
		return voiceLeft{}
	}
	l := voiceLeft{channel: m.current(), principal: m.authorID}
	for _, room := range m.voice.doc.Rooms {
		// The helper joins only the channel-wide room.
		if room.Audience != nil {
			continue
		}
		for _, p := range room.Participants {
			if p.PrincipalID == m.authorID && !p.JoinedAt.IsZero() {
				l.joined = append(l.joined, p.JoinedAt)
			}
		}
	}
	return l
}

// justLeft reports whether p, in room of the channel on screen, is the
// session this TUI left, still listed by presence.
func (m Model) justLeft(room schema.VoicePresenceRoom, p schema.VoiceParticipant) bool {
	l := m.vc.left
	if room.Audience != nil || l.channel != m.current() || l.principal != p.PrincipalID || p.JoinedAt.IsZero() {
		return false
	}
	return slices.ContainsFunc(l.joined, func(at schema.Timestamp) bool { return at.Time().Equal(p.JoinedAt.Time()) })
}

// WithVoice gives the model voice controls with setup. Without it /voice says
// this terminal does not offer voice.
func (m Model) WithVoice(setup VoiceSetup) Model {
	m.vc.owner = &voiceOwner{make: setup.factory()}
	return m
}

// voiceStatusMsg is one Status the supervisor published.
type voiceStatusMsg struct {
	serial int
	st     voicehelper.Status
}

// voiceDone is the end of one call to the supervisor numbered serial.
type voiceDone struct {
	serial  int
	op      voiceOp
	channel string
	err     error
}

// voicePage is what /voice status read from the supervisor.
type voicePage struct {
	st   voicehelper.Status
	path string
	diag []string
}

type voiceOp int

const (
	opJoin voiceOp = iota
	opLeave
	opRetry
	opMuteOn
	opMuteOff
	opDeafenOn
	opDeafenOff
	opClose
)

// asked is how a command in flight is shown: what was asked, with an
// ellipsis, never as if it were done.
func (op voiceOp) asked(channel string) string {
	return map[voiceOp]string{
		opJoin: "joining #" + sanitize(channel), opLeave: "leaving", opRetry: "retrying",
		opMuteOn: "muting", opMuteOff: "unmuting", opDeafenOn: "deafening", opDeafenOff: "undeafening",
		opClose: "stopping voice",
	}[op]
}

// waitVoice delivers the supervisor's next Status. It ends when the
// supervisor is dropped or the TUI's context ends; Update arms it again after
// each one.
func (m Model) waitVoice(ctl voiceController, serial int, done <-chan struct{}) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case st := <-ctl.Updates():
			return voiceStatusMsg{serial: serial, st: st}
		case <-done:
		case <-ctx.Done():
		}
		return nil
	}
}

func (m Model) voiceStatus(msg voiceStatusMsg) (tea.Model, tea.Cmd) {
	if m.vc.owner == nil {
		return m, nil
	}
	ctl, serial, done := m.vc.owner.current()
	if ctl == nil || serial != msg.serial {
		return m, nil
	}
	m.vc.status = msg.st
	return m, m.waitVoice(ctl, serial, done)
}

// voiceCall runs one call to supervisor serial in a tea.Cmd and reports its
// end.
func (m Model) voiceCall(ctl voiceController, serial int, op voiceOp, channel string) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		var err error
		switch op {
		case opJoin:
			err = ctl.Join(ctx, channel)
		case opLeave:
			err = ctl.Leave(ctx)
		case opRetry:
			err = ctl.Retry(ctx)
		case opMuteOn, opMuteOff:
			err = ctl.SetMute(ctx, op == opMuteOn)
		case opDeafenOn, opDeafenOff:
			err = ctl.SetDeafen(ctx, op == opDeafenOn)
		}
		return voiceDone{serial: serial, op: op, channel: channel, err: err}
	}
}

// voiceClosing closes a dropped supervisor in a tea.Cmd, bounded, and then
// says so to the owner.
func voiceClosing(ctl voiceController, closed func()) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), voiceCloseTimeout)
		defer cancel()
		err := ctl.Close(ctx)
		closed()
		return voiceDone{op: opClose, err: err}
	}
}

func (m Model) voiceDone(msg voiceDone) (tea.Model, tea.Cmd) {
	if msg.op == opClose {
		return m, nil
	}
	// The answer of a supervisor dropped since (sign-out, another account)
	// is about nothing on screen any more.
	if _, serial, _ := m.vc.owner.current(); serial != msg.serial {
		return m, nil
	}
	m.vc.busy = ""
	switch {
	case msg.err != nil:
		m.setStatus(modeChannels, voiceErrText(msg.err))
	case msg.op == opJoin, msg.op == opRetry:
		m.setStatus(modeChannels, "voice: started for #"+sanitize(msg.channel)+"; the strip shows its state")
	case msg.op == opLeave:
		m.setStatus(modeChannels, "voice: left")
	case msg.op == opMuteOn:
		m.setStatus(modeChannels, "voice: mute on")
	case msg.op == opMuteOff:
		m.setStatus(modeChannels, "voice: mute off")
	case msg.op == opDeafenOn:
		m.setStatus(modeChannels, "voice: deafen on")
	case msg.op == opDeafenOff:
		m.setStatus(modeChannels, "voice: deafen off")
	}
	return m, nil
}

// voiceSignedOut stops voice for good: the TUI's login no longer works, or
// it is ending. The helper is stopped in a tea.Cmd.
func (m Model) voiceSignedOut() (Model, tea.Cmd) {
	if m.vc.owner == nil {
		return m, nil
	}
	c, closed := m.vc.owner.drop(true)
	if c == nil {
		return m, nil
	}
	m.vc.status = voicehelper.Status{Phase: voicehelper.PhaseIdle}
	m.vc.busy = ""
	return m, voiceClosing(c, closed)
}

// voiceIdentity drops a supervisor made for another account than the one the
// server now says is signed in: its helper would join as the wrong person.
func (m Model) voiceIdentity(principal int64) (Model, tea.Cmd) {
	if m.vc.owner == nil {
		return m, nil
	}
	if !m.vc.owner.heldForOther(principal) {
		return m, nil
	}
	c, closed := m.vc.owner.drop(false)
	m.vc.status = voicehelper.Status{Phase: voicehelper.PhaseIdle}
	m.vc.busy = ""
	m.setStatus(modeChannels, "voice: stopped, because you are now signed in as someone else")
	return m, voiceClosing(c, closed)
}

const voiceUsage = "usage: /voice join [channel] | leave | mute on|off | deafen on|off | retry | status | help"

// voiceCommand handles "/voice ...". Every refusal leaves the input as typed;
// a command that goes ahead clears it.
func (m Model) voiceCommand(rest string) (tea.Model, tea.Cmd) {
	args := strings.Fields(rest)
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "help":
		m.input = ""
		m.help, m.helpTop, m.helpTopic = true, 0, topicVoice
		return m, nil
	}
	if m.vc.owner == nil {
		m.setStatus(modeChannels, "voice: this terminal does not offer voice")
		return m, nil
	}
	onOff := func() (bool, bool) {
		if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
			m.setStatus(modeChannels, "usage: /voice "+sub+" on|off")
			return false, false
		}
		return args[0] == "on", true
	}
	switch sub {
	case "join":
		if len(args) > 1 {
			m.setStatus(modeChannels, "usage: /voice join [channel]")
			return m, nil
		}
		return m.voiceJoin(args)
	case "leave":
		return m.voiceLeave()
	case "mute":
		on, ok := onOff()
		if !ok {
			return m, nil
		}
		return m.voiceSet(map[bool]voiceOp{true: opMuteOn, false: opMuteOff}[on])
	case "deafen":
		on, ok := onOff()
		if !ok {
			return m, nil
		}
		return m.voiceSet(map[bool]voiceOp{true: opDeafenOn, false: opDeafenOff}[on])
	case "retry":
		return m.voiceTryAgain()
	case "status":
		ctl, _, _ := m.vc.owner.current()
		m.input = ""
		if ctl == nil {
			return m.openVoicePage(voicePage{st: m.vc.status}), nil
		}
		return m, func() tea.Msg {
			return voicePage{st: ctl.Status(), path: ctl.HelperPath(), diag: ctl.Diagnostics()}
		}
	}
	m.setStatus(modeChannels, voiceUsage)
	return m, nil
}

// voiceActive reports whether a helper is running or stopping, from what the
// supervisor last published.
func (v voiceCtl) voiceActive() bool {
	switch v.status.Phase {
	case voicehelper.PhaseStarting, voicehelper.PhaseConnecting, voicehelper.PhaseConnected,
		voicehelper.PhaseReconnecting, voicehelper.PhaseStopping:
		return true
	}
	return false
}

// voiceJoin starts voice for the named channel, or the one on screen. Only a
// channel of this TUI's list can be named; browsing the list afterwards
// changes nothing about where voice goes.
func (m Model) voiceJoin(args []string) (tea.Model, tea.Cmd) {
	channel := m.current()
	if len(args) == 1 {
		channel = strings.TrimPrefix(args[0], "#")
	}
	switch {
	case m.vc.busy != "":
		m.setStatus(modeChannels, "voice: still "+m.vc.busy+"…; wait for it")
		return m, nil
	case m.vc.voiceActive():
		m.setStatus(modeChannels, "voice: already in #"+sanitize(m.vc.status.Channel)+" voice; /voice leave first (to switch channel, leave and join)")
		return m, nil
	case !slices.Contains(m.channels, channel):
		m.setStatus(modeChannels, "voice: "+strconv.Quote(sanitize(channel))+" is not one of your channels")
		return m, nil
	}
	if m.authorID <= 0 && m.authenticated {
		m.setStatus(modeChannels, m.noAuthorStatus("join voice"))
		return m, nil
	}
	// This account may already be in this channel's voice from another
	// client: joining here would disconnect that one, so it is said first,
	// and a repeat within voiceConfirmWindow goes ahead (§11). Presence covers
	// only the channel on screen: there it is known; for any other channel it
	// is not, and the same step is asked for. The session this TUI has just
	// left, which presence may still list, is not another client
	// (selfInVoice). The status line is cut beside the key hints on a small
	// terminal, so the strip says it too.
	in, _, known := m.selfInVoice()
	onScreen := channel == m.current()
	if !onScreen || (known && in) {
		now := m.now()
		if m.vc.confirm != channel || now.Sub(m.vc.confirmAt) > voiceConfirmWindow {
			m.vc.confirm, m.vc.confirmAt, m.vc.confirmKnown = channel, now, onScreen
			// Cleared, so that the command can be typed again as asked.
			m.input = ""
			if onScreen {
				m.setStatus(modeChannels, "voice: joining disconnects your other #"+sanitize(channel)+" voice; repeat /voice join within 10 s")
			} else {
				m.setStatus(modeChannels, "voice: this account may already be in #"+sanitize(channel)+"'s voice from another client; repeat /voice join #"+sanitize(channel)+" within 10 s to join here")
			}
			return m, nil
		}
	}
	m.vc.confirm = ""
	ctl, serial, done, made, err := m.vc.owner.get(m.authorID)
	if err != nil {
		m.setStatus(modeChannels, voiceErrText(err))
		return m, nil
	}
	m.input = ""
	m.vc.left = voiceLeft{}
	m.vc.busy = opJoin.asked(channel)
	m.setStatus(modeChannels, "voice: joining #"+sanitize(channel)+"…")
	cmd := m.voiceCall(ctl, serial, opJoin, channel)
	if made {
		cmd = tea.Batch(cmd, m.waitVoice(ctl, serial, done))
	}
	return m, cmd
}

func (m Model) voiceLeave() (tea.Model, tea.Cmd) {
	ctl, serial, _ := m.vc.owner.current()
	idle := m.vc.status.Phase == voicehelper.PhaseIdle || m.vc.status.Phase == ""
	if ctl == nil || idle && m.vc.busy == "" {
		m.setStatus(modeChannels, "voice: not in voice")
		return m, nil
	}
	// Leave is never refused for a command in flight: it is how a join that
	// hangs is given up on.
	m.input = ""
	m.vc.left = m.leaving()
	m.vc.busy = opLeave.asked("")
	m.setStatus(modeChannels, "voice: leaving…")
	return m, m.voiceCall(ctl, serial, opLeave, "")
}

func (m Model) voiceSet(op voiceOp) (tea.Model, tea.Cmd) {
	ctl, serial, _ := m.vc.owner.current()
	switch {
	case m.vc.busy != "":
		m.setStatus(modeChannels, "voice: still "+m.vc.busy+"…; wait for it")
		return m, nil
	case ctl == nil || !m.vc.voiceActive() || m.vc.status.Phase == voicehelper.PhaseStopping:
		m.setStatus(modeChannels, "voice: not in voice; /voice join first")
		return m, nil
	}
	m.input = ""
	m.vc.busy = op.asked("")
	return m, m.voiceCall(ctl, serial, op, "")
}

func (m Model) voiceTryAgain() (tea.Model, tea.Cmd) {
	ctl, serial, _ := m.vc.owner.current()
	st := m.vc.status
	switch {
	case m.vc.busy != "":
		m.setStatus(modeChannels, "voice: still "+m.vc.busy+"…; wait for it")
		return m, nil
	case ctl == nil || st.Phase != voicehelper.PhaseFailed:
		m.setStatus(modeChannels, "voice: nothing to retry")
		return m, nil
	case !st.Retryable:
		m.setStatus(modeChannels, "voice: a retry cannot fix this: "+voiceReasonWords(st.Reason))
		return m, nil
	}
	m.input = ""
	m.vc.busy = opRetry.asked("")
	m.setStatus(modeChannels, "voice: retrying #"+sanitize(st.Channel)+"…")
	return m, m.voiceCall(ctl, serial, opRetry, st.Channel)
}

// openVoicePage opens the /voice status page with what was read.
func (m Model) openVoicePage(p voicePage) Model {
	m.vc.status = p.st
	m.vc.path, m.vc.diag = p.path, p.diag
	m.help, m.helpTop, m.helpTopic = true, 0, topicVoiceStatus
	return m
}

// voiceErrText is a supervisor error for the status line.
func voiceErrText(err error) string {
	var e *voicehelper.Error
	text := strings.TrimPrefix(err.Error(), "voicehelper: ")
	switch {
	case errors.Is(err, voicehelper.ErrBusy):
		text = "voice is still stopping; try again in a moment"
	case errors.Is(err, context.Canceled):
		text = "canceled"
	case errors.As(err, &e) && e.Reason != voicehelper.ReasonHelperNotFound && e.Reason != voicehelper.ReasonHelperUntrusted:
		// The others carry their own advice: where to put conch-voice.
		text = "voice stopped: " + voiceReasonWords(e.Reason)
	}
	if !strings.HasPrefix(text, "voice") {
		text = "voice: " + text
	}
	return sanitize(text)
}

// reasonWords are the wire and supervisor reasons in words, with what to do
// where the person can do something.
var reasonWords = map[string]string{
	string(schema.VoiceControlReasonRoomDeleted):        "the room was closed",
	string(schema.VoiceControlReasonParticipantRemoved): "removed from the room",
	string(schema.VoiceControlReasonConnectionLost):     "connection lost",
	string(schema.VoiceControlReasonReconnectTimedOut):  "reconnecting timed out",
	string(schema.VoiceControlReasonConnectFailed):      "could not connect",
	string(schema.VoiceControlReasonServerUnreachable):  "server unreachable",
	string(schema.VoiceControlReasonVoiceUnavailable):   "voice server unavailable",
	string(schema.VoiceControlReasonNoSession):          "no voice session",
	string(schema.VoiceControlReasonServerShutdown):     "server shutting down",
	string(schema.VoiceControlReasonLeft):               "left",
	string(schema.VoiceControlReasonOwnerLost):          "conch stopped it",
	string(schema.VoiceControlReasonSignal):             "stopped by a signal",
	string(schema.VoiceControlReasonNotSignedIn):        "not signed in: sign in again with conch login",
	string(schema.VoiceControlReasonIdentityMismatch):   "the stored login is another account: sign in again with conch login",
	string(schema.VoiceControlReasonBadEnvironment):     "conch-voice was started wrongly; this is a bug, please report it",
	string(schema.VoiceControlReasonBadConfig):          "voice.toml was refused: fix it (see conch-voice devices)",
	string(schema.VoiceControlReasonVoiceRequiresAuth):  "voice needs sign-in on this server",
	string(schema.VoiceControlReasonNotAPerson):         "agents cannot use voice",
	string(schema.VoiceControlReasonVoiceNotAllowed):    "this account may not use voice",
	string(schema.VoiceControlReasonNotAMember):         "not a member of this channel",
	string(schema.VoiceControlReasonVoiceNotConfigured): "this server has no voice",
	string(schema.VoiceControlReasonDuplicateIdentity):  "this account joined voice elsewhere (a retry disconnects the other)",
	string(schema.VoiceControlReasonAudioUnavailable):   "audio unavailable (PipeWire or the speakers)",
	string(schema.VoiceControlReasonStartTimeout):       "conch-voice was not told to start in time",
	string(schema.VoiceControlReasonProtocolError):      "conch-voice could not understand conch",
	string(schema.VoiceControlReasonInternalError):      "conch-voice failed",
	voicehelper.ReasonHelperNotFound:                    "conch-voice not found: install it beside conch or in PATH, or set CONCH_VOICE_HELPER",
	voicehelper.ReasonHelperUntrusted:                   "conch-voice is not safe to run (see /voice status)",
	voicehelper.ReasonHelperIncompatible:                "conch-voice does not match this conch: install the same release",
	voicehelper.ReasonSpawnFailed:                       "conch-voice could not be started",
	voicehelper.ReasonHelloTimeout:                      "conch-voice did not start in time",
	voicehelper.ReasonAckTimeout:                        "conch-voice stopped answering",
	voicehelper.ReasonBadEvent:                          "conch-voice broke the control protocol",
	voicehelper.ReasonHelperExited:                      "conch-voice ended without saying why",
	voicehelper.ReasonHelperCrashed:                     "conch-voice crashed",
}

// voiceReasonWords is reason in words; one this TUI does not know is shown as
// the code, made safe.
func voiceReasonWords(reason string) string {
	if w, ok := reasonWords[reason]; ok {
		return w
	}
	return sanitize(reason)
}

var pttProblemWords = map[schema.VoiceControlPTTProblem]string{
	schema.VoiceControlPTTProblemCannotOpen:    "cannot open the key device",
	schema.VoiceControlPTTProblemRefused:       "no permission to read the key device",
	schema.VoiceControlPTTProblemReadFailed:    "reading the key device failed",
	schema.VoiceControlPTTProblemEnded:         "the key device went away",
	schema.VoiceControlPTTProblemPartialRecord: "the key device sent a broken record",
}

var blockWords = map[schema.VoiceControlBlock]string{
	schema.VoiceControlBlockNotConnected:   "not connected",
	schema.VoiceControlBlockNoPublishGrant: "listen only",
	schema.VoiceControlBlockNoMicrophone:   "no microphone",
	schema.VoiceControlBlockDeafened:       "deafened",
	schema.VoiceControlBlockMuted:          "muted",
	schema.VoiceControlBlockKeyDeviceLost:  "talk key lost",
	schema.VoiceControlBlockMaxTransmit:    "talk limit reached: release the key",
}

func wordOr[K ~string](words map[K]string, k K) string {
	if w, ok := words[k]; ok {
		return w
	}
	return sanitize(string(k))
}

// voiceSelf is the left part of the voice strip: what the helper last
// confirmed, in the words of §15, and the command in flight. It is "" when
// this terminal has no helper and has not asked for one: then the strip is
// presence only.
func (m Model) voiceSelf() string {
	v := m.vc
	if v.owner == nil {
		return ""
	}
	st := v.status
	channel := "#" + sanitize(st.Channel)
	var text string
	switch st.Phase {
	case voicehelper.PhaseStarting, voicehelper.PhaseConnecting:
		text = "voice: joining " + channel + "…"
	case voicehelper.PhaseConnected:
		text = "voice: " + channel + " · " + strings.Join(connectedParts(st), " · ")
	case voicehelper.PhaseReconnecting:
		text = "voice: " + channel + " · reconnecting"
		if st.Reason != "" {
			text += " (" + voiceReasonWords(st.Reason) + ")"
		}
		if st.RetryIn > 0 {
			text += ", retry in " + strconv.Itoa(int((st.RetryIn+time.Second-1)/time.Second)) + " s"
		}
		text += flags(st)
	case voicehelper.PhaseStopping:
		text = "voice: leaving " + channel + "…"
	case voicehelper.PhaseStopped:
		text = "voice off (" + voiceReasonWords(st.Reason) + ") · /voice join"
	case voicehelper.PhaseFailed:
		text = "voice stopped: " + voiceReasonWords(st.Reason)
		if st.Retryable {
			text += " · /voice retry"
		}
	default:
		// Idle: nothing running. A join in flight is shown as asked, and so
		// is the warning of a join that would take over another client's
		// voice, while a repeat would still go ahead.
		switch {
		case v.busy != "":
			return "voice: " + v.busy + "…"
		case v.confirm != "" && m.now().Sub(v.confirmAt) <= voiceConfirmWindow && v.confirmKnown:
			return "voice: you are in #" + sanitize(v.confirm) + " elsewhere · /voice join again within 10 s takes over"
		case v.confirm != "" && m.now().Sub(v.confirmAt) <= voiceConfirmWindow:
			return "voice: maybe in #" + sanitize(v.confirm) + " elsewhere · repeat /voice join #" + sanitize(v.confirm) + " within 10 s"
		}
		return ""
	}
	if v.busy != "" {
		text += " · " + v.busy + "…"
	}
	return text
}

// connectedParts are the strip's words for a connected helper: capture and
// transmission apart (a joined person with the gate shut is "mic on"), mute
// and deafen, and the talk key.
func connectedParts(st voicehelper.Status) []string {
	var parts []string
	switch {
	case st.Transmitting:
		parts = append(parts, "TALKING")
	case st.Capture == schema.VoiceControlCaptureOn:
		parts = append(parts, "mic on")
	case st.Capture == schema.VoiceControlCaptureUnavailable:
		parts = append(parts, "no microphone")
	case slices.Contains(st.Blocked, schema.VoiceControlBlockNoPublishGrant):
		parts = append(parts, "listen only")
	default:
		parts = append(parts, "mic off")
	}
	if st.Muted {
		parts = append(parts, "muted")
	}
	if st.Deafened {
		parts = append(parts, "deafened")
	}
	switch st.PTT {
	case schema.VoiceControlPTTReady:
		if len(st.Blocked) == 0 && !st.Transmitting {
			parts = append(parts, "hold the talk key to talk")
		}
	case schema.VoiceControlPTTNotConfigured:
		parts = append(parts, "no talk key: set [keys] in voice.toml (conch-voice devices)")
	case schema.VoiceControlPTTUnavailable:
		parts = append(parts, "talk key unavailable ("+wordOr(pttProblemWords, st.PTTProblem)+")")
	case schema.VoiceControlPTTLost:
		parts = append(parts, "talk key lost: presses refused")
	}
	if slices.Contains(st.Blocked, schema.VoiceControlBlockMaxTransmit) {
		parts = append(parts, blockWords[schema.VoiceControlBlockMaxTransmit])
	}
	return parts
}

// flags is mute and deafen, for the phases that keep them while not
// connected.
func flags(st voicehelper.Status) string {
	var s string
	if st.Muted {
		s += " · muted"
	}
	if st.Deafened {
		s += " · deafened"
	}
	return s
}

// voiceStatusLines is the /voice status page in lines of at most cols cells:
// the whole Status in words, the helper's version and path, and the last
// lines of its log. Everything from the helper is made safe for the terminal
// again here, though the supervisor has already done so.
func (m Model) voiceStatusLines(cols int) []string {
	st := m.vc.status
	yesNo := map[bool]string{true: "yes", false: "no"}
	var rows [][2]string
	add := func(k, v string) { rows = append(rows, [2]string{k, v}) }
	phase := string(st.Phase)
	if phase == "" {
		phase = string(voicehelper.PhaseIdle)
	}
	switch {
	case m.vc.owner == nil:
		add("voice", "this terminal does not offer voice")
	case st.Phase == "" || st.Phase == voicehelper.PhaseIdle:
		add("voice", "off: /voice join to join the voice of the channel on screen")
	default:
		add("voice", phase)
		add("channel", "#"+sanitize(st.Channel)+" (browsing channels does not change it)")
		add("microphone", wordOr(map[schema.VoiceControlCapture]string{
			schema.VoiceControlCaptureOn:          "on: open while you are in voice, whether or not you talk",
			schema.VoiceControlCaptureOff:         "off",
			schema.VoiceControlCaptureUnavailable: "unavailable",
		}, st.Capture))
		add("transmitting", yesNo[st.Transmitting])
		add("muted", yesNo[st.Muted])
		add("deafened", yesNo[st.Deafened])
		key := wordOr(map[schema.VoiceControlPTT]string{
			schema.VoiceControlPTTReady:         "ready",
			schema.VoiceControlPTTNotConfigured: "not configured: set [keys] in voice.toml (conch-voice devices)",
			schema.VoiceControlPTTUnavailable:   "unavailable",
			schema.VoiceControlPTTLost:          "lost: presses refused until it is back",
			schema.VoiceControlPTTUnknown:       "not read yet",
		}, st.PTT)
		if st.PTTProblem != "" {
			key += " (" + wordOr(pttProblemWords, st.PTTProblem) + ")"
		}
		add("talk key", key)
		press := "would transmit"
		if len(st.Blocked) > 0 {
			var why []string
			for _, b := range st.Blocked {
				why = append(why, wordOr(blockWords, b))
			}
			press = "would do nothing: " + strings.Join(why, ", ")
		}
		add("a press now", press)
		if st.Reason != "" {
			add("reason", voiceReasonWords(st.Reason)+" ("+sanitize(st.Reason)+")")
		}
		if st.RetryIn > 0 {
			add("next attempt", "in "+st.RetryIn.Round(time.Second).String())
		}
		if st.Phase == voicehelper.PhaseFailed {
			add("retry", map[bool]string{true: "/voice retry may fix this", false: "a retry cannot fix this"}[st.Retryable])
		}
	}
	if st.HelperVersion != "" {
		add("helper version", sanitize(st.HelperVersion))
	}
	path := sanitize(m.vc.path)
	if path == "" {
		path = "none started yet (CONCH_VOICE_HELPER, beside conch, then PATH)"
	}
	add("helper", path)
	keyW := 0
	for _, r := range rows {
		keyW = max(keyW, cellWidth(r[0]))
	}
	keyW = min(keyW, cols/3)
	var lines []string
	for _, r := range rows {
		for i, w := range wrapWords(r[1], max(1, cols-keyW-3), max(1, cols-keyW-3)) {
			k := ""
			if i == 0 {
				k = r[0]
			}
			lines = append(lines, " "+activeStyle.Render(padCells(fitCells(k, keyW), keyW))+"  "+w)
		}
	}
	lines = append(lines, "", " "+titleStyle.Render("conch-voice's log, newest last"))
	if len(m.vc.diag) == 0 {
		lines = append(lines, " "+statusStyle.Render("(nothing)"))
	}
	for _, d := range m.vc.diag {
		lines = append(lines, " "+statusStyle.Render(fitCells(sanitize(d), max(1, cols-1))))
	}
	return lines
}

// voiceHelpEntries and voiceHelpNotes are the /voice help page.
func voiceHelpEntries() []helpEntry {
	return []helpEntry{
		{"/voice join [ch]", "join the voice of channel ch, or of the channel on screen; browsing channels afterwards does not move it"},
		{"/voice leave", "leave voice; to switch channel, leave and join"},
		{"/voice mute on|off", "mute or unmute your microphone"},
		{"/voice deafen on|off", "stop or start hearing others; undeafening leaves mute as it was"},
		{"/voice retry", "try again after a failure that a new attempt could fix"},
		{"/voice status", "everything voice is doing, conch-voice's version and path, and its log"},
		{"/voice help", "this page"},
	}
}

func voiceHelpNotes() []string {
	return []string{
		"Talking: hold the talk key that conch-voice reads from your keyboard, set under [keys] in voice.toml (conch-voice devices lists keyboards; conch-voice keys shows key codes). Nothing typed here transmits.",
		"The microphone is open while you are in voice (\"mic on\"); audio leaves only while the talk key is held (\"TALKING\").",
		"Over SSH: conch-voice runs on the machine where conch runs and uses that machine's microphone, speakers and keyboard, not those of the computer you connect from. No audio is carried over SSH.",
		"conch-voice is found through CONCH_VOICE_HELPER (an absolute path), then beside conch, then in PATH; conch and conch-voice must come from the same release. Voice needs Linux with PipeWire.",
	}
}
