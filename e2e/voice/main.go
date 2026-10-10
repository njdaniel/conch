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
// Also checked throughout: the join token's own claims (claims.go); that no
// token, secret or room name appears in anything conchd sends to a client
// (every response header and body, every presence frame), in its log, in its
// audit log, in lk's output or in this program's (scan.go).
//
// With no Docker, or no network to pull the image or lk, the live half is
// skipped with one line and the program exits 0, unless CI is set: then a skip
// is a failure. An image or lk that is not the pinned one fails everywhere. It
// prints one "voice-check: PASS" line when everything ran. A SIGINT or SIGTERM
// stops the run and removes what it started.
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
	// A signal cancels stopCtx and nothing else. Every child runs under it and
	// every wait watches it, so the run unwinds on its own path and teardown
	// below runs once, after nothing is in flight. (Tearing down from the
	// handler raced a container being started, and a build in progress.)
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		for n := 1; ; n++ {
			<-sigs
			switch n {
			case 1:
				fmt.Fprintln(os.Stderr, "voice-check: interrupted; cleaning up")
				stopCancel()
			case 2:
				fmt.Fprintln(os.Stderr, "voice-check: still cleaning up; signal again to give up (a container may be left behind)")
			default:
				fmt.Fprintln(os.Stderr, "voice-check: giving up on clean-up")
				os.Exit(130)
			}
		}
	}()

	start := time.Now()
	skipped, err := guardedRun(h)
	h.teardown()
	switch {
	case stopCtx.Err() != nil:
		fmt.Fprintln(os.Stderr, "voice-check: interrupted")
		return 130
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

// guardedRun is run and the final scan, turning a panic into a failure so
// that realMain still tears everything down.
func guardedRun(h *harness) (skipped string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	skipped, err = run(stopCtx, h)
	if err == nil {
		err = h.scanAll()
	}
	return skipped, err
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
	lv, reason, err := prepareLive(ctx, h)
	if err != nil {
		return "", err
	}
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
	bot               *person
	aliceWS           *presenceSocket
	afterSweep        func() error // checks to repeat once a sweep has run
	bridgeRoom        string
	bridgeSubject     string
	aliceHL           *headless
}

// prepareLive checks what the live half needs. It returns a reason to skip
// when the machine cannot run it (no Docker, no network), and an error when it
// could but something is wrong: a pulled image or a downloaded lk that is not
// the one pinned in this program is not a missing network, and fails
// everywhere, CI or not.
func prepareLive(ctx context.Context, h *harness) (*live, string, error) {
	if goos := runtime.GOOS; goos != "linux" {
		return nil, "the live half runs LiveKit on the Docker host network, which only Linux has (this is " + goos + ")", nil
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := docker(pctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return nil, "Docker is not available: " + oneLine(out, err), nil
	}
	ictx, icancel := context.WithTimeout(ctx, 5*time.Minute)
	defer icancel()
	if out, err := docker(ictx, "pull", "--quiet", livekitImage); err != nil {
		if ctx.Err() != nil {
			return nil, "", errInterrupted
		}
		if looksOffline(out) {
			return nil, "cannot pull the LiveKit image (no network?): " + oneLine(out, err), nil
		}
		return nil, "", fmt.Errorf("cannot pull the pinned LiveKit image %s: %s", livekitImage, oneLine(out, err))
	}
	dir, err := h.mkdir("lkbin-")
	if err != nil {
		return nil, "", err
	}
	lkBin, err := fetchLK(ictx, dir)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", errInterrupted
		}
		if errors.Is(err, errUnreachable) {
			return nil, "cannot download lk " + lkVersion + " (no network?): " + err.Error(), nil
		}
		return nil, "", fmt.Errorf("lk %s: %w", lkVersion, err)
	}
	ogg := filepath.Join(dir, "silence.ogg")
	if err := writeSilentOpus(ogg, 900); err != nil {
		return nil, "", err
	}
	return &live{h: h, lkBin: lkBin, ogg: ogg}, "", nil
}

// looksOffline reports whether a failed pull is about reaching the registry,
// as opposed to what the registry answered.
func looksOffline(out string) bool {
	out = strings.ToLower(out)
	for _, w := range []string{"no such host", "timeout", "timed out", "connection refused", "network is unreachable",
		"temporary failure", "tls handshake", "dial tcp", "no route to host", "i/o timeout", "could not resolve",
		"unexpected eof", "toomanyrequests", "rate limit", "deadline exceeded", "cannot connect to the docker daemon"} {
		if strings.Contains(out, w) {
			return true
		}
	}
	return false
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
		{"removed while not connected", l.removedNotConnected},
		{"other ways of losing the room", l.rotationScenario},
		{"never issued", l.neverIssued},
		{"credential expiring by itself", l.credentialExpires},
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

// lkGone reports that LiveKit itself says identity p<id> is not in room:
// either it listed the room's participants and they are not among them, or
// it answered that the room does not exist. A call that failed for any other
// reason says nothing, and is not taken for absence.
func (l *live) lkGone(room string, id int64) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	parts, err := l.adm.listParticipants(ctx, room)
	if roomNotFound(err) {
		return true, "the room does not exist"
	}
	if err != nil {
		return false, "LiveKit could not be asked: " + err.Error()
	}
	for _, p := range parts {
		if p.Identity == identity(id) {
			return false, "LiveKit lists them in the room"
		}
	}
	return true, "LiveKit does not list them in the room"
}

// lkHas asks LiveKit whether identity p<id> is in room. False means only
// "not seen": use lkGone to assert that someone is absent.
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
