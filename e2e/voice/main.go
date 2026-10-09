// Command voice-check is the V3 exit program (issue #129, ADR-004, design note
// docs/design/voice-control-plane.md): it proves the voice control plane
// against a real conchd, the real conch CLI and a real LiveKit server, and
// proves that the rest of conchd does not notice when LiveKit is down or was
// never configured.
//
// Two halves:
//
//   - The degraded half needs no LiveKit and always runs. With no LiveKit
//     settings, voice answers voice_not_configured; with settings pointing at
//     nothing, it answers voice_unavailable; in both the approval dogfood
//     (e2e/dogfood, run unmodified as a subprocess) passes.
//   - The live half starts the pinned livekit/livekit-server image in Docker
//     (localhost only, automatic room creation off), downloads the pinned lk
//     tool, and runs the issue's scenarios: a headless participant joins on
//     the token conchd issued and is seen, unmutes and mutes and is seen to,
//     an outsider is refused, a removed member loses the room (rotation, #161),
//     a member who was never issued a session rotates nothing, and LiveKit is
//     stopped. LiveKit itself is asked for ground truth throughout.
//
// With no Docker, or no way to pull the image or lk, the live half is skipped
// with one line and the program exits 0, unless CI is set: then a skip is a
// failure. It prints one "voice-check: PASS" line when everything ran.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	h, err := newHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "voice-check: FAIL:", err)
		return 1
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		h.teardown()
		os.Exit(130)
	}()

	ctx := context.Background()
	start := time.Now()
	skipped, err := run(ctx, h)
	if err == nil {
		err = h.scanAll()
	}
	h.teardown()
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "voice-check: FAIL:", h.redact(err.Error()))
		return 1
	case skipped != "":
		fmt.Printf("voice-check: SKIPPED the live half (%s); the degraded half passed in %s\n", skipped, time.Since(start).Round(time.Second))
		return 0
	}
	fmt.Printf("voice-check: PASS in %s\n", time.Since(start).Round(time.Second))
	return 0
}

// inCI is true when the job says so. In CI a skip is a failure.
func inCI() bool {
	v := strings.ToLower(os.Getenv("CI"))
	return v != "" && v != "false" && v != "0"
}

func run(ctx context.Context, h *harness) (skipped string, err error) {
	if err := h.build(); err != nil {
		return "", fmt.Errorf("build binaries: %w", err)
	}
	h.say("== degraded half (no LiveKit needed) ==")
	if err := degraded(h); err != nil {
		return "", fmt.Errorf("degraded half: %w", err)
	}

	h.say("== live half (LiveKit in Docker, headless participant via lk) ==")
	lv, reason := prepareLive(ctx, h)
	if reason != "" {
		if inCI() {
			return "", fmt.Errorf("live half cannot run in CI: %s", reason)
		}
		return reason, nil
	}
	if err := lv.run(ctx); err != nil {
		return "", fmt.Errorf("live half: %w", err)
	}
	return "", nil
}

// ------------------------------------------------------------- degraded half

// degraded runs the two cases that need no LiveKit: voice not configured, and
// voice configured for a LiveKit that is not there.
func degraded(h *harness) error {
	if err := notConfigured(h); err != nil {
		return fmt.Errorf("not configured: %w", err)
	}
	// An address nothing listens on: a port that was free a moment ago.
	dead, err := freeAddr()
	if err != nil {
		return err
	}
	cfg := &livekitSettings{url: "ws://" + dead, key: "voicecheckkey", secret: "voice-check-NOT-FOR-PRODUCTION-" + randHex(24)}
	h.secret("the LiveKit API secret", cfg.secret)
	if err := unreachable(h, cfg, "LiveKit is not running"); err != nil {
		return fmt.Errorf("configured but unreachable: %w", err)
	}
	return nil
}

// notConfigured: no LiveKit settings. Every voice endpoint answers
// voice_not_configured, to a member only.
func notConfigured(h *harness) error {
	d, err := h.startConchd("unconfigured", nil)
	if err != nil {
		return err
	}
	if _, err := d.createChannel("lobby"); err != nil {
		return err
	}
	ann, err := h.newPerson(d, "ann", "lobby")
	if err != nil {
		return err
	}
	out, err := h.newPerson(d, "outsider")
	if err != nil {
		return err
	}
	if err := expectRefusal("session with voice not configured", ann.api, http.MethodPost, "/v1/channels/lobby/voice/session", http.StatusServiceUnavailable, schema.ErrorCodeVoiceNotConfigured); err != nil {
		return err
	}
	doc, err := ann.presence("lobby")
	if err != nil {
		return fmt.Errorf("presence: %w", err)
	}
	if doc.Configured || doc.Available || len(doc.Rooms) != 0 {
		return fmt.Errorf("presence with voice not configured: configured=%v available=%v rooms=%d, want false false 0", doc.Configured, doc.Available, len(doc.Rooms))
	}
	if err := expectRefusal("a non-member asking about voice learns nothing about voice", out.api, http.MethodPost, "/v1/channels/lobby/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if output, err := ann.cli.run("", "voice", "status", "lobby"); err == nil || !strings.Contains(output, "not configured") {
		return fmt.Errorf("conch voice status with voice not configured: exit=%s output=%q, want a nonzero exit and \"not configured\"", exitText(err), strings.TrimSpace(output))
	}
	h.say("ok   not configured: session and presence say so, a non-member learns nothing, conch voice status exits nonzero")
	if err := d.scanAudit("unconfigured"); err != nil {
		return err
	}
	d.stop()
	return h.runDogfood("voice not configured")
}

// unreachable: voice configured, LiveKit not answering. The session endpoint
// answers voice_unavailable and presence says so; dogfood still passes with
// the same settings in its environment.
func unreachable(h *harness, cfg *livekitSettings, why string) error {
	d, err := h.startConchd("unreachable", cfg)
	if err != nil {
		return err
	}
	if _, err := d.createChannel("lobby"); err != nil {
		return err
	}
	ann, err := h.newPerson(d, "ann", "lobby")
	if err != nil {
		return err
	}
	if err := expectDown(ann, "lobby"); err != nil {
		return err
	}
	h.say("ok   configured, %s: session answers voice_unavailable, presence says unavailable", why)
	if err := d.scanAudit("unreachable"); err != nil {
		return err
	}
	d.stop()
	env := []string{"CONCHD_LIVEKIT_URL=" + cfg.url, "CONCHD_LIVEKIT_API_KEY=" + cfg.key, "CONCHD_LIVEKIT_API_SECRET=" + cfg.secret}
	return h.runDogfood("voice configured, "+why, env...)
}

// expectDown asserts the whole "LiveKit is down" answer: the session
// endpoint, the presence snapshot and the CLI.
func expectDown(p *person, channel string) error {
	if err := expectRefusal("session while LiveKit is down", p.api, http.MethodPost, "/v1/channels/"+channel+"/voice/session", http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable); err != nil {
		return err
	}
	// Presence turns unavailable once the poller has seen a failed call.
	if err := waitFor("presence to say unavailable", 45*time.Second, func() (bool, string) {
		doc, err := p.presence(channel)
		if err != nil {
			return false, err.Error()
		}
		return doc.Configured && !doc.Available && len(doc.Rooms) == 0, fmt.Sprintf("configured=%v available=%v rooms=%d", doc.Configured, doc.Available, len(doc.Rooms))
	}); err != nil {
		return err
	}
	output, err := p.cli.run("", "voice", "status", channel)
	if err == nil || !strings.Contains(output, "unavailable") {
		return fmt.Errorf("conch voice status while LiveKit is down: exit=%s output=%q, want a nonzero exit and \"unavailable\"", exitText(err), strings.TrimSpace(output))
	}
	return nil
}

// expectRefusal sends a request and requires a specific status and error code.
func expectRefusal(what string, a api, method, path string, status int, code string) error {
	gotStatus, gotCode, err := a.refusal(method, path)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if gotStatus != status || gotCode != code {
		return fmt.Errorf("%s: status %d code %q, want status %d code %q", what, gotStatus, gotCode, status, code)
	}
	return nil
}

// ---------------------------------------------------------------- live half

type live struct {
	h     *harness
	srv   *livekitServer
	adm   *lkAdmin
	d     *conchdProc
	lkBin string
	ogg   string

	// Set by the first scenario, used by the later ones.
	alice, bob, carol *person
	fay               *person
	bridgeRoom        string
	bridgeSubject     string
	aliceHL           *headless
}

// prepareLive checks what the live half needs and returns a reason to skip
// when it cannot run.
func prepareLive(ctx context.Context, h *harness) (*live, string) {
	if goos := runtime.GOOS; goos != "linux" {
		return nil, "the live half runs LiveKit on the Docker host network, which only Linux has (this is " + goos + ")"
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := docker(pctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return nil, "Docker is not available: " + oneLine(out, err)
	}
	ictx, icancel := context.WithTimeout(ctx, 5*time.Minute)
	defer icancel()
	if out, err := docker(ictx, "pull", "--quiet", livekitImage); err != nil {
		return nil, "cannot pull the LiveKit image: " + oneLine(out, err)
	}
	dir, err := h.mkdir("lkbin-")
	if err != nil {
		return nil, err.Error()
	}
	lkBin, err := fetchLK(ictx, dir)
	if err != nil {
		return nil, "cannot get lk " + lkVersion + ": " + err.Error()
	}
	ogg := filepath.Join(dir, "silence.ogg")
	if err := writeSilentOpus(ogg, 900); err != nil {
		return nil, err.Error()
	}
	return &live{h: h, lkBin: lkBin, ogg: ogg}, ""
}

func oneLine(out string, err error) string {
	s := strings.TrimSpace(out)
	if s == "" {
		s = err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 160)
}

func (l *live) run(ctx context.Context) error {
	h := l.h
	srv, err := h.startLiveKit(ctx)
	if err != nil {
		return err
	}
	l.srv, l.adm = srv, srv.admin()
	h.say("ok   LiveKit %s is up on %s (container %s, auto_create off)", "v1.13.7", srv.apiURL, srv.name)

	d, err := h.startConchd("live", srv.settings())
	if err != nil {
		return err
	}
	l.d = d

	steps := []struct {
		name string
		fn   func(ctx context.Context) error
	}{
		{"joined, transmitting, outsider", l.joinedTransmittingOutsider},
		{"removed (room rotation)", l.removed},
		{"never issued", l.neverIssued},
		{"LiveKit down", l.liveKitDown},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	if err := d.scanAudit("live"); err != nil {
		return err
	}
	h.say("ok   no token, secret or room name in this output, conchd's log or the audit log")
	return nil
}

// presenceOf returns what viewer sees for a principal in channel's presence:
// whether listed, whether transmitting, and a description for messages.
func presenceOf(viewer *person, id int64) (listed, talking bool, saw string, err error) {
	doc, err := viewer.presence("bridge")
	if err != nil {
		return false, false, "", err
	}
	var ids []int64
	for _, room := range doc.Rooms {
		for _, p := range room.Participants {
			ids = append(ids, p.PrincipalID)
			if p.PrincipalID == id {
				listed, talking = true, p.Transmitting
			}
		}
	}
	return listed, talking, fmt.Sprintf("available=%v principals=%v", doc.Available, ids), nil
}

// lkHas asks LiveKit whether identity p<id> is in room.
func (l *live) lkHas(room string, id int64) (bool, lkParticipant, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	parts, err := l.adm.listParticipants(ctx, room)
	if err != nil {
		return false, lkParticipant{}, err.Error()
	}
	var idents []string
	for _, p := range parts {
		idents = append(idents, p.Identity)
		if p.Identity == identity(id) {
			return true, p, ""
		}
	}
	return false, lkParticipant{}, fmt.Sprintf("LiveKit lists %v", idents)
}

// exitText describes how a command ended, for a message.
func exitText(err error) string {
	if err == nil {
		return "0"
	}
	return err.Error()
}

func identity(id int64) string { return fmt.Sprintf("p%d", id) }

func (l *live) lkRooms() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return l.adm.listRooms(ctx)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// auditCount counts audit rows with an action, optionally narrowed to a
// subject and an actor ("" matches any).
func (l *live) auditCount(action, subject, actor string) (int, error) {
	rows, err := l.d.auditRows(action)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range rows {
		if (subject == "" || e.Subject == subject) && (actor == "" || e.Actor == actor) {
			n++
		}
	}
	return n, nil
}

func (l *live) waitAudit(what, action, subject, actor string, want int) error {
	return waitFor(what, 30*time.Second, func() (bool, string) {
		n, err := l.auditCount(action, subject, actor)
		if err != nil {
			return false, err.Error()
		}
		return n == want, fmt.Sprintf("%d %s rows, want %d", n, action, want)
	})
}

// joinHeadless joins lk with the token in grant, which conchd issued.
func (l *live) joinHeadless(grant schema.VoiceRoomGrant) (*headless, error) {
	hostPort := strings.TrimPrefix(l.srv.wsURL, "ws://")
	return l.h.startHeadless(l.lkBin, l.ogg, hostPort, grant.Token)
}

// ---- scenario 1: joined, transmitting, outsider

func (l *live) joinedTransmittingOutsider(ctx context.Context) error {
	h, d := l.h, l.d
	bridgeID, err := d.createChannel("bridge")
	if err != nil {
		return err
	}
	if _, err := d.createChannel("side"); err != nil {
		return err
	}
	bridgeSubject := fmt.Sprintf("channel:%d", bridgeID)
	alice, err := h.newPerson(d, "alice", "bridge")
	if err != nil {
		return err
	}
	bob, err := h.newPerson(d, "bob", "bridge")
	if err != nil {
		return err
	}
	carol, err := h.newPerson(d, "carol", "side")
	if err != nil {
		return err
	}

	// Two humans get sessions.
	sa, err := h.session(alice, "bridge")
	if err != nil {
		return err
	}
	if _, err := h.session(bob, "bridge"); err != nil {
		return err
	}
	if g := sa.Rooms[0]; sa.Identity != identity(alice.id) || sa.LivekitURL != l.srv.wsURL || !g.CanPublish {
		return fmt.Errorf("alice's session: identity %q (want %q), url %q (want %q), can_publish=%v (want true)", sa.Identity, identity(alice.id), sa.LivekitURL, l.srv.wsURL, g.CanPublish)
	}
	bridgeRoom := sa.Rooms[0].Room
	h.room(bridgeRoom)
	h.say("ok   alice and bob each got a session for bridge (schema-valid, identity p<id>, publish allowed)")

	// While LiveKit is up the session endpoint must not say it is down:
	// asked again, it answers again.
	if _, err := h.session(bob, "bridge"); err != nil {
		return fmt.Errorf("session endpoint while LiveKit is up: %w", err)
	}

	// A headless participant joins as alice on the token conchd issued.
	hl, err := l.joinHeadless(sa.Rooms[0])
	if err != nil {
		return err
	}
	l.alice, l.bob, l.carol, l.bridgeRoom, l.bridgeSubject, l.aliceHL = alice, bob, carol, bridgeRoom, bridgeSubject, hl
	aliceID := alice.id
	if err := waitFor("LiveKit to list alice in bridge's room", 60*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(bridgeRoom, aliceID)
		if !ok && hl.exited() {
			return false, "lk exited: " + hl.tail(h)
		}
		return ok, saw
	}); err != nil {
		return err
	}
	if err := waitFor("bob's presence to show alice", 30*time.Second, func() (bool, string) {
		listed, _, saw, err := presenceOf(bob, aliceID)
		if err != nil {
			return false, err.Error()
		}
		return listed, saw
	}); err != nil {
		return err
	}
	want := fmt.Sprintf("%d ", aliceID)
	if err := waitFor("conch voice status (as bob) to list alice", 30*time.Second, func() (bool, string) {
		out, err := bob.cli.run("", "voice", "status", "bridge")
		if err != nil {
			return false, oneLine(out, err)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, want) {
				return true, ""
			}
		}
		return false, strings.TrimSpace(out)
	}); err != nil {
		return err
	}
	// Presence shows exactly who is connected: alice, and nobody who holds
	// a session without having joined (bob) or is not in the channel (carol).
	doc, err := bob.presence("bridge")
	if err != nil {
		return err
	}
	var shown []int64
	for _, room := range doc.Rooms {
		for _, p := range room.Participants {
			shown = append(shown, p.PrincipalID)
		}
	}
	if len(shown) != 1 || shown[0] != aliceID {
		return fmt.Errorf("presence for bridge lists principals %v, want alice (%d) alone", shown, aliceID)
	}
	if err := l.waitAudit("voice_session_issued rows for bridge", store.AuditVoiceSessionIssued, bridgeSubject, "", 3); err != nil {
		return err
	}
	if err := l.waitAudit("alice's voice_joined row", store.AuditVoiceJoined, bridgeSubject, fmt.Sprintf("principal:%d", aliceID), 1); err != nil {
		return err
	}
	h.say("ok   joined: lk joined as alice on conchd's token; LiveKit, presence and conch voice status agree; audit has voice_session_issued and voice_joined")

	// Transmitting. lk publishes its audio track unmuted. Muting and
	// unmuting is done at LiveKit's side, which is all conchd can see of a
	// key press: the muted state of a published microphone track.
	actor := fmt.Sprintf("principal:%d", aliceID)
	var track lkTrack
	if err := waitFor("alice's microphone track in LiveKit", 30*time.Second, func() (bool, string) {
		ok, p, saw := l.lkHas(bridgeRoom, aliceID)
		if !ok {
			return false, saw
		}
		t, has := p.mic()
		track = t
		return has, "alice is in the room but has no microphone track"
	}); err != nil {
		return err
	}
	setMuted := func(muted bool) error {
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := l.adm.mute(mctx, bridgeRoom, identity(aliceID), track.SID, muted); err != nil {
			return err
		}
		return waitFor(fmt.Sprintf("LiveKit to report alice's track muted=%v", muted), 15*time.Second, func() (bool, string) {
			ok, p, saw := l.lkHas(bridgeRoom, aliceID)
			if !ok {
				return false, saw
			}
			t, _ := p.mic()
			return t.Muted == muted, fmt.Sprintf("muted=%v", t.Muted)
		})
	}
	seen := func(talking bool) error {
		return waitFor(fmt.Sprintf("bob's presence to show alice talking=%v", talking), 30*time.Second, func() (bool, string) {
			listed, tk, saw, err := presenceOf(bob, aliceID)
			if err != nil {
				return false, err.Error()
			}
			return listed && tk == talking, saw + fmt.Sprintf(" listed=%v talking=%v", listed, tk)
		})
	}
	// Start from muted so that the unmute is the event under test.
	if err := setMuted(true); err != nil {
		return err
	}
	if err := seen(false); err != nil {
		return err
	}
	started, _ := l.auditCount(store.AuditVoiceTransmitStarted, bridgeSubject, actor)
	stopped, _ := l.auditCount(store.AuditVoiceTransmitStopped, bridgeSubject, actor)
	if err := setMuted(false); err != nil {
		return err
	}
	if err := seen(true); err != nil {
		return err
	}
	if err := l.waitAudit("a voice_transmit_started row after the unmute", store.AuditVoiceTransmitStarted, bridgeSubject, actor, started+1); err != nil {
		return err
	}
	if out, err := bob.cli.run("", "voice", "status", "bridge"); err != nil || !strings.Contains(out, fmt.Sprintf("%d - talking ", aliceID)) {
		return fmt.Errorf("conch voice status while alice is unmuted: exit=%s output=%q, want her line to say talking", exitText(err), strings.TrimSpace(out))
	}
	if err := setMuted(true); err != nil {
		return err
	}
	if err := seen(false); err != nil {
		return err
	}
	if err := l.waitAudit("a voice_transmit_stopped row after the mute", store.AuditVoiceTransmitStopped, bridgeSubject, actor, stopped+1); err != nil {
		return err
	}
	// Leave her transmitting for what follows.
	if err := setMuted(false); err != nil {
		return err
	}
	if err := seen(true); err != nil {
		return err
	}
	h.say("ok   transmitting: unmute shows talking and writes voice_transmit_started; mute shows quiet and writes voice_transmit_stopped")

	// Outsider: carol is not in bridge.
	if err := expectRefusal("an outsider asking for a session in bridge", carol.api, http.MethodPost, "/v1/channels/bridge/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if err := expectRefusal("anyone asking for a session in a channel that does not exist", carol.api, http.MethodPost, "/v1/channels/no-such-channel/voice/session", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	if err := expectRefusal("an outsider reading bridge's presence", carol.api, http.MethodGet, "/v1/channels/bridge/voice", http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	// A token for a different room puts the holder in that room, not this one.
	cs, err := h.session(carol, "side")
	if err != nil {
		return err
	}
	sideRoom := cs.Rooms[0].Room
	h.room(sideRoom)
	if sideRoom == bridgeRoom {
		return errors.New("bridge and side were given the same room")
	}
	hostURL := l.srv.wsURL
	sc, status, body, err := dialSignal(ctx, hostURL, cs.Rooms[0].Token)
	if err != nil {
		return fmt.Errorf("carol joining side with her own token: HTTP %d %s", status, h.redact(body))
	}
	h.onCleanup(sc.Close)
	if err := waitFor("LiveKit to list carol in side's room", 30*time.Second, func() (bool, string) {
		ok, _, saw := l.lkHas(sideRoom, carol.id)
		return ok, saw
	}); err != nil {
		return err
	}
	if in, _, _ := l.lkHas(bridgeRoom, carol.id); in {
		return fmt.Errorf("LiveKit lists carol in bridge's room, though her token is for side")
	}
	listed, _, saw, err := presenceOf(bob, carol.id)
	if err != nil {
		return err
	}
	if listed {
		return fmt.Errorf("bridge's presence lists carol, who holds a token for another room (presence: %s)", saw)
	}
	sc.Close()
	h.say("ok   outsider: refused the unknown-channel answer; a token for side puts carol in side, not in bridge (LiveKit and presence)")
	return nil
}
