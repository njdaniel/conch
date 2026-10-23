//go:build linux

package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The paths of the managed-voice check that need conchd to refuse or LiveKit
// to go away (issue #559): a member whose invite withheld voice joins from her
// TUI, and so does a TUI whose stored login has become another account's; then
// LiveKit is stopped and started again under a TUI in voice, once at once and
// once after a long outage. What the TUI says is read from its screen, in the
// TUI's words for the contract's reasons (docs/design/tui-voice.md §15), and
// on its /voice status page, which also names the reason's code.

const (
	// outageHold is how long the long outage keeps LiveKit down. The helper
	// has no bound on its attempts (voice/crates/conch-voice-control/src/
	// policy.rs, docs/design/conch-voice.md §5): the only limits it puts on
	// one outage are the SDK's own reconnect, which it gives 20 s, and closing
	// that connection, which it gives 10 s (voice/conch-voice/src/session.rs,
	// Timings). Past them it asks conchd again after waits that double from
	// 1 s and stop growing at 30 s, each cut by up to half, for as long as it
	// runs. This is past both limits, well into those waits.
	outageHold = 40 * time.Second
	// comebackBound is how long a helper may take to be connected again once
	// LiveKit is back: the rest of a wait of at most 30 s, then a session and
	// a connection.
	comebackBound = 90 * time.Second
)

// reconnectReasons are the reasons the contract allows with the reconnecting
// phase (tui-voice.md §6).
var reconnectReasons = []schema.VoiceControlReason{
	schema.VoiceControlReasonRoomDeleted, schema.VoiceControlReasonParticipantRemoved,
	schema.VoiceControlReasonConnectionLost, schema.VoiceControlReasonReconnectTimedOut,
	schema.VoiceControlReasonConnectFailed, schema.VoiceControlReasonServerUnreachable,
	schema.VoiceControlReasonVoiceUnavailable, schema.VoiceControlReasonNoSession,
	schema.VoiceControlReasonServerShutdown,
}

// reasonCode matches the /voice status page's reason row naming one of codes.
func reasonCode(codes ...schema.VoiceControlReason) *regexp.Regexp {
	quoted := make([]string, 0, len(codes))
	for _, c := range codes {
		quoted = append(quoted, regexp.QuoteMeta(string(c)))
	}
	return regexp.MustCompile(`reason +[^\n]*\((` + strings.Join(quoted, "|") + `)\)`)
}

// The rows of the /voice status page these checks read, value after key.
var (
	pageReconnecting = regexp.MustCompile(`voice +reconnecting`)
	pageConnected    = regexp.MustCompile(`voice +connected`)
	pageFailed       = regexp.MustCompile(`voice +failed`)
	pageMicOff       = regexp.MustCompile(`microphone +off`)
	pageNotConnected = regexp.MustCompile(`a press now +would do nothing: not connected`)
	pageWouldTalk    = regexp.MustCompile(`a press now +would transmit`)
	pageNextAttempt  = regexp.MustCompile(`next attempt +in \S+`)
	pageNoRetry      = regexp.MustCompile(`retry +a retry cannot fix this`)
	// pageHelperRan is the version the helper's hello gave: a helper was
	// started and spoke, so the refusal was its own.
	pageHelperRan = regexp.MustCompile(`helper version +\S`)
)

// stripRetry is the strip of a helper waiting to ask conchd again, with the
// reason in words and the wait in whole seconds.
var stripRetry = regexp.MustCompile(`voice: #squad · reconnecting \(([^)]+)\), retry in (\d+) s`)

// stripReasons are the reasons in words the strip gave for reconnecting in
// text, each once, in the order they first appeared.
func stripReasons(text string) []string {
	var seen []string
	for _, m := range stripRetry.FindAllStringSubmatch(text, -1) {
		if !contains(seen, m[1]) {
			seen = append(seen, m[1])
		}
	}
	return seen
}

// takeoverWarnings are what the TUI says when it thinks this account is in
// voice from another client (§11): none may appear across a reconnect.
var takeoverWarnings = []string{"disconnects your other", "elsewhere", "from another client"}

// waitMatch waits until the TUI has written text that re matches after mark.
func (t *tuiProc) waitMatch(what string, mark int, within time.Duration, re *regexp.Regexp) error {
	return waitForOrFail(t.who+"'s TUI to show "+what, within, func() (bool, string, error) {
		text := t.textSince(mark)
		if re.MatchString(text) {
			return true, "", nil
		}
		if t.exited() {
			return false, "", fmt.Errorf("the TUI exited with status %d; the last line it wrote: %s", t.cmd.ProcessState.ExitCode(), lastLine(text))
		}
		return false, "the last voice lines written: " + voiceLines(text), nil
	})
}

// statusPage opens /voice status, waits until it shows each of wants, and
// closes it again.
func (s *seat) statusPage(what string, wants ...*regexp.Regexp) error {
	mark := s.tui.mark()
	if err := s.tui.typeText("/voice status\r"); err != nil {
		return err
	}
	for _, re := range wants {
		if err := s.tui.waitMatch(strconv.Quote(re.String())+" on /voice status "+what, mark, 15*time.Second, re); err != nil {
			return err
		}
	}
	closing := s.tui.mark()
	if err := s.tui.typeText("\r"); err != nil {
		return err
	}
	return s.tui.waitText("the /voice status page closed "+what, closing, 15*time.Second, "help: F1, /help")
}

// refused types a command the TUI refuses, waits for its answer, and clears
// the composer, which keeps a refused command for editing.
func (s *seat) refused(line, answer string) error {
	if err := s.command(line, answer); err != nil {
		return err
	}
	return s.tui.typeText(strings.Repeat("\x7f", len(line)))
}

// noHelper waits until s's TUI has no conch-voice child: one it started has
// exited, or none is left running.
func (s *seat) noHelper(what string) error {
	return waitFor(s.name+"'s TUI to have no conch-voice child "+what, ownerLossBound+5*time.Second, func() (bool, string) {
		p, ok := s.tui.helperOf()
		return !ok, "conch-voice pid " + strconv.Itoa(p.pid) + " still running"
	})
}

// sameHelper checks that s's TUI still runs the helper it had: the supervisor
// never restarts one, and a reconnect is the helper's own.
func (s *seat) sameHelper(what string) error {
	p, ok := s.tui.helperOf()
	if !ok || p != s.helper || !s.helper.alive() {
		return fmt.Errorf("%s: %s's TUI no longer runs the conch-voice it started (pid %d): now %v %v", what, s.name, s.helper.pid, ok, p.pid)
	}
	return nil
}

// noTakeover checks text for the warnings of a join that would displace this
// account elsewhere, and for voice having stopped.
func noTakeover(who, what, text string) error {
	for _, w := range append([]string{"voice stopped"}, takeoverWarnings...) {
		if strings.Contains(text, w) {
			return fmt.Errorf("%s's TUI wrote %q %s: %s", who, w, what, voiceLines(text))
		}
	}
	return nil
}

// sessionsIssued counts the voice sessions conchd issued to p.
func (l *live) sessionsIssued(p *person) (int, error) {
	return l.auditCount(store.AuditVoiceSessionIssued, "", fmt.Sprintf("principal:%d", p.id))
}

// lkCount is how many connections LiveKit lists for p in channel's rooms.
func (l *live) lkCount(channelID int64, p *person) (int, error) {
	rooms, err := l.d.voiceRooms(channelID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rooms {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		parts, err := l.adm.ListParticipants(ctx, r)
		cancel()
		if lkit.RoomNotFound(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, part := range parts {
			if part.Identity == identity(p.id) {
				n++
			}
		}
	}
	return n, nil
}

// managedRefused: a member whose invite withheld voice joins voice from her
// TUI and is refused; then her TUI's stored login becomes ben's, and the
// helper refuses to join as anyone but her. Neither captures anything, joins
// LiveKit or is issued a session, and ben, whose account the second would
// have joined as, stays connected.
func (l *live) managedRefused(squadID int64, ben *person, ears *voiceClient) error {
	h, d := l.h, l.d
	var inv schema.CreateInviteResponseV1
	if err := d.operator.call(http.MethodPost, "/v1/invites", schema.CreateInviteRequestV1{
		Kind: schema.InviteKindPersonal, Channels: []string{"squad"}, Voice: false,
	}, &inv); err != nil {
		return fmt.Errorf("an invite without voice: %w", err)
	}
	h.secret("an invite code", inv.Code)
	var joined schema.RedeemInviteResponseV1
	// v2, with a password: v1 makes no account since #713.
	if err := d.as("").call(http.MethodPost, "/v2/invites/redeem", schema.RedeemInviteRequestV2{
		Code: inv.Code, Name: "dee", Password: "a passphrase long enough to pass",
	}, &joined); err != nil {
		return fmt.Errorf("redeem the invite without voice: %w", err)
	}
	h.secret("dee's conch credential", joined.Token)
	dee := &person{name: "dee", id: joined.Principal.ID, token: joined.Token, credID: joined.Credential.ID, d: d, api: d.as(joined.Token)}
	cli, err := h.newConchUser(d.baseURL, joined.Token)
	if err != nil {
		return err
	}
	dee.cli = cli
	// conchd refuses her a session itself: what the helper would be told.
	if err := expectRefusal("a voice session for a member whose invite withheld voice", dee.api, http.MethodPost, "/v1/channels/squad/voice/session", http.StatusForbidden, schema.ErrorCodeVoiceNotAllowed); err != nil {
		return err
	}

	deeSeat, err := l.newSeat(dee, "dee", toneAnn, "squad")
	if err != nil {
		return err
	}
	if err := deeSeat.start(); err != nil {
		return err
	}
	const notAllowed = "voice stopped: this account may not use voice"
	mark := deeSeat.tui.mark()
	if err := deeSeat.command("/voice join", notAllowed); err != nil {
		return err
	}
	if err := deeSeat.noHelper("after her voice was refused"); err != nil {
		return err
	}
	if err := refusedQuietly(deeSeat, mark, notAllowed); err != nil {
		return err
	}
	if err := deeSeat.refused("/voice retry", "voice: a retry cannot fix this: this account may not use voice"); err != nil {
		return err
	}
	if err := deeSeat.statusPage("after her voice was refused", pageFailed, pageMicOff, reasonCode(schema.VoiceControlReasonVoiceNotAllowed), pageNoRetry, pageHelperRan); err != nil {
		return err
	}
	if err := l.outOfRooms(squadID, dee, "after her voice was refused"); err != nil {
		return err
	}
	if n, err := l.sessionsIssued(dee); err != nil {
		return err
	} else if n != 0 {
		return fmt.Errorf("conchd issued dee %d voice sessions; her invite withheld voice", n)
	}
	h.say("ok   managed: dee, whose invite withheld voice (conchd answers her 403 voice_not_allowed), typed /voice join: her conch-voice started and refused, the TUI said voice stopped, this account may not use voice, offered no retry and refused one; /voice status says failed (voice_not_allowed), microphone off; no session was issued and LiveKit lists her nowhere")

	// ---- the stored login is another account's
	if _, err := dee.cli.run(ben.token+"\n", "login"); err != nil {
		return err
	}
	benSessions, err := l.sessionsIssued(ben)
	if err != nil {
		return err
	}
	// Ben's conch-voice has been issued sessions all along: none counted
	// would mean the rows are not counted as this check thinks, and dee's
	// zero above would prove nothing.
	if benSessions == 0 {
		return fmt.Errorf("no voice_session_issued rows counted for ben, who is in voice: the count is wrong")
	}
	since := time.Now()
	const mismatch = "voice stopped: the stored login is another account: sign in again with conch login"
	mark = deeSeat.tui.mark()
	if err := deeSeat.command("/voice join", mismatch); err != nil {
		return err
	}
	if err := deeSeat.noHelper("after the identity check"); err != nil {
		return err
	}
	if err := refusedQuietly(deeSeat, mark, mismatch); err != nil {
		return err
	}
	if err := deeSeat.statusPage("after the identity check", pageFailed, pageMicOff, reasonCode(schema.VoiceControlReasonIdentityMismatch), pageNoRetry); err != nil {
		return err
	}
	// Nothing joined as ben: his own conch-voice was not displaced, and he
	// has the one connection he had.
	if n, err := l.sessionsIssued(ben); err != nil {
		return err
	} else if n != benSessions {
		return fmt.Errorf("conchd issued ben %d voice sessions while dee's TUI held his login; he had %d before", n, benSessions)
	}
	if n, err := l.sessionsIssued(dee); err != nil {
		return err
	} else if n != 0 {
		return fmt.Errorf("conchd issued dee %d voice sessions", n)
	}
	for _, e := range ears.snapshot() {
		if !e.at.Before(since) && e.obj["event"] == "connection" {
			return fmt.Errorf("ben's conch-voice changed connection (%v) while dee's TUI held his login", e.obj["state"])
		}
	}
	if ears.exited() {
		return fmt.Errorf("ben's conch-voice exited while dee's TUI held his login: %s", ears.stderrTail())
	}
	if n, err := l.lkCount(squadID, ben); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("LiveKit lists ben %d times in squad's rooms, want once", n)
	}
	if err := l.outOfRooms(squadID, dee, "after the identity check"); err != nil {
		return err
	}
	if err := deeSeat.tui.typeText("\x1b"); err != nil {
		return err
	}
	select {
	case <-deeSeat.tui.done:
	case <-time.After(quitBound + 5*time.Second):
		return fmt.Errorf("dee's TUI was still running after esc")
	}
	h.say("ok   managed: with ben's login stored under dee's TUI, /voice join started conch-voice, whose identity check refused it: the TUI said the stored login is another account, with no retry, and /voice status says failed (identity_mismatch), microphone off; no session was issued to either, and ben's own conch-voice stayed connected, his one connection in the room")
	return nil
}

// refusedQuietly checks what s's TUI wrote since mark, up to and with the
// refusal: that it never showed a microphone or talking, and did not offer a
// retry with the refusal.
func refusedQuietly(s *seat, mark int, refusal string) error {
	text := s.tui.textSince(mark)
	for _, w := range []string{"mic on", "TALKING", "listen only", "reconnecting"} {
		if strings.Contains(text, w) {
			return fmt.Errorf("%s's TUI showed %q before %q: %s", s.name, w, refusal, voiceLines(text))
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, refusal) && strings.Contains(line, "/voice retry") {
			return fmt.Errorf("%s's TUI offered /voice retry with %q", s.name, refusal)
		}
	}
	return nil
}

// managedReconnect stops LiveKit while cy's TUI is in voice and starts it
// again straight away, as a restart of the media server does: the helper she
// has goes to reconnecting and back to connected by itself, her TUI follows it
// without a word about another client, and the talk key is heard again. Then
// she leaves and joins again at once, with squad on screen, and is not warned
// that she is in voice elsewhere: the session presence lists for her is the
// one her TUI has just left, though it began at the reconnect.
func (l *live) managedReconnect(ctx context.Context, squadID int64, cy *seat, ears *voiceClient) error {
	h := l.h
	mark := cy.tui.mark()
	stopped := time.Now()
	if err := l.srv.Stop(ctx); err != nil {
		return err
	}
	if err := cy.tui.waitText("voice reconnecting", mark, 60*time.Second, "voice: #squad · reconnecting"); err != nil {
		return err
	}
	if _, err := ears.wait("not connected while LiveKit is down", 60*time.Second, stopped, notConnected); err != nil {
		return err
	}
	if err := cy.statusPage("while LiveKit is down", pageReconnecting, pageNotConnected); err != nil {
		return err
	}
	started := time.Now()
	back := cy.tui.mark()
	up, startTook, err := l.startAgain(ctx)
	if err != nil {
		return err
	}
	if err := cy.tui.waitText("voice connected again", back, comebackBound, joinedSquad); err != nil {
		return err
	}
	tookBack := time.Since(up)
	if _, err := ears.wait("connected again after LiveKit came back", comebackBound, started, earsConnected); err != nil {
		return err
	}
	room, err := l.inRoom(squadID, cy.who)
	if err != nil {
		return err
	}
	h.room(room)
	if err := cy.sameHelper("after LiveKit came back"); err != nil {
		return err
	}
	if err := cy.statusPage("after LiveKit came back", pageConnected, pageWouldTalk); err != nil {
		return err
	}
	if err := l.talk(cy, ears, "cy holding the key after LiveKit came back", true); err != nil {
		return err
	}
	if err := noTakeover(cy.name, "across the reconnect", cy.tui.textSince(mark)); err != nil {
		return err
	}
	h.say("ok   managed: LiveKit stopped and started again under cy's TUI: its strip and /voice status said reconnecting (reasons shown: %s), then connected %s after LiveKit answered again (docker took %s to start it), with the same conch-voice (pid %d); the talk key was heard again, and nothing was said of another client", reasonList(stripReasons(cy.tui.textSince(mark))), tookBack.Round(100*time.Millisecond), startTook.Round(100*time.Millisecond), cy.helper.pid)

	// ---- leave and join again at once, after the reconnect
	inRoster := fmt.Sprintf("%d %s", cy.who.id, cy.who.name)
	if err := waitFor("cy's strip to list her in squad's room", 30*time.Second, func() (bool, string) {
		text := cy.tui.textSince(back)
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, joinedSquad) && strings.Contains(line, "room: ") && strings.Contains(line, inRoster) {
				return true, ""
			}
		}
		return false, voiceLines(text)
	}); err != nil {
		return err
	}
	left := time.Now()
	if err := cy.command("/voice leave", "voice: left", "voice off · /voice join"); err != nil {
		return err
	}
	if _, err := cy.helperGone("after /voice leave", left, leaveBound); err != nil {
		return err
	}
	rejoin := cy.tui.mark()
	if err := cy.command("/voice join", "voice: started for #squad", joinedSquad); err != nil {
		return err
	}
	if err := noTakeover(cy.name, "joining again after the reconnect", cy.tui.textSince(rejoin)); err != nil {
		return err
	}
	if err := cy.findHelper(); err != nil {
		return err
	}
	room, err = l.inRoom(squadID, cy.who)
	if err != nil {
		return err
	}
	h.room(room)
	h.say("ok   managed: after the reconnect cy left and joined again at once, with squad on screen, and was not warned of voice elsewhere")
	return nil
}

// earsConnected accepts a plain conch-voice saying it is connected.
func earsConnected(o map[string]any) bool {
	return o["event"] == "connection" && o["state"] == "connected"
}

// reasonList is reasons for a line of output.
func reasonList(reasons []string) string {
	if len(reasons) == 0 {
		return "none"
	}
	return strings.Join(reasons, ", ")
}

// startAgain starts LiveKit again and returns when it answered, and how long
// docker and LiveKit took to get there: on a busy machine that can be long,
// and it is not the helper's time.
func (l *live) startAgain(ctx context.Context) (time.Time, time.Duration, error) {
	began := time.Now()
	if err := l.srv.Start(ctx); err != nil {
		return time.Time{}, 0, err
	}
	return time.Now(), time.Since(began), nil
}

// notConnected accepts a plain conch-voice saying it is not connected.
func notConnected(o map[string]any) bool {
	return o["event"] == "connection" && (o["state"] == "reconnecting" || o["state"] == "waiting")
}

// managedOutage keeps LiveKit down past every limit the helper puts on one
// outage. The helper never gives up on a lost server (outageHold):
// it stays reconnecting, says why and when it will ask again, and the TUI
// leaves it be: /voice retry has nothing to retry, and no new helper is
// started. Once LiveKit is back the same helper connects by itself and is
// heard.
func (l *live) managedOutage(ctx context.Context, squadID int64, cy *seat, ears *voiceClient) error {
	h := l.h
	mark := cy.tui.mark()
	stopped := time.Now()
	if err := l.srv.Stop(ctx); err != nil {
		return err
	}
	if err := cy.tui.waitText("voice reconnecting", mark, 60*time.Second, "voice: #squad · reconnecting"); err != nil {
		return err
	}
	for until := stopped.Add(outageHold); time.Now().Before(until); {
		if err := cy.sameHelper("while LiveKit was down"); err != nil {
			return err
		}
		text := cy.tui.textSince(mark)
		for _, w := range []string{"voice stopped", "voice off"} {
			if strings.Contains(text, w) {
				return fmt.Errorf("cy's TUI said %q %s into the outage: %s", w, time.Since(stopped).Round(time.Second), voiceLines(text))
			}
		}
		select {
		case <-ctx.Done():
			return errInterrupted
		case <-time.After(250 * time.Millisecond):
		}
	}
	waits := stripRetry.FindAllStringSubmatch(cy.tui.textSince(mark), -1)
	if len(waits) == 0 {
		return fmt.Errorf("cy's strip never said when conch-voice would try again: %s", voiceLines(cy.tui.textSince(mark)))
	}
	longest, why := 0, ""
	for _, w := range waits {
		if n, _ := strconv.Atoi(w[2]); n > longest {
			longest, why = n, w[1]
		}
	}
	if err := cy.refused("/voice retry", "voice: nothing to retry"); err != nil {
		return err
	}
	if err := cy.statusPage(time.Since(stopped).Round(time.Second).String()+" into the outage", pageReconnecting, reasonCode(reconnectReasons...), pageNextAttempt, pageNotConnected); err != nil {
		return err
	}
	started := time.Now()
	back := cy.tui.mark()
	up, startTook, err := l.startAgain(ctx)
	if err != nil {
		return err
	}
	if err := cy.tui.waitText("voice connected again", back, comebackBound, joinedSquad); err != nil {
		return err
	}
	tookBack, held := time.Since(up), up.Sub(stopped)
	if _, err := ears.wait("connected again after the long outage", comebackBound, started, earsConnected); err != nil {
		return err
	}
	room, err := l.inRoom(squadID, cy.who)
	if err != nil {
		return err
	}
	h.room(room)
	if err := cy.sameHelper("after the long outage"); err != nil {
		return err
	}
	if err := l.talk(cy, ears, "cy holding the key after the long outage", true); err != nil {
		return err
	}
	if err := noTakeover(cy.name, "across the long outage", cy.tui.textSince(mark)); err != nil {
		return err
	}
	h.say("ok   managed: LiveKit down for %s under cy's TUI: conch-voice never gave up (it has no bound on its attempts); the strip said reconnecting with a reason and a retry (reasons shown: %s), the longest wait %d s (%s), /voice retry had nothing to retry and /voice status said reconnecting with the reason's code; LiveKit back (docker took %s to start it), the same conch-voice connected %s after it answered and was heard", held.Round(time.Second), reasonList(stripReasons(cy.tui.textSince(mark))), longest, why, startTook.Round(100*time.Millisecond), tookBack.Round(100*time.Millisecond))
	return nil
}
