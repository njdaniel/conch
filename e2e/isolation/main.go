// Command isolation-check is the private alpha's cross-group check (issue
// #447, docs/design/private-alpha.md §1, exit steps 3 and 7): two groups on
// one server, and a member of one finds nothing of the other, over REST, the
// sockets and LiveKit's media; and a revoked tester is out at once.
//
// It runs a real conchd and, when Docker is there, the pinned LiveKit and lk
// (e2e/internal/lkit, the same as e2e/voice), and calls the public API only:
// every request it makes is checked against internal/server/routes.go at the
// end.
//
//   - The world (world.go): group A joins through an operator's open link,
//     group B through personal invites, the second made by a B tester the
//     operator let invite. Each posts, replies in a thread, whispers, posts to
//     a net, uploads a file, runs an agent's session and run, asks for an
//     intervention and an approval and sends a problem report; A talks in
//     voice and shares a screen.
//   - The sockets (sockets.go): every socket variant stays open on both of
//     B's channels from before A is active to the end of the run, carries
//     B's controls, and nothing of A in any frame.
//   - The matrix (matrix.go): for every route of the table, B tries A's
//     objects by name and id, as an invited tester, as a member the operator
//     made, and on the agent routes as B's agent (agents.go). Each answer is
//     the stated refusal, holds no marker of A in its body or headers
//     (markers.go), and is the same answer B gets about something that never
//     existed. The coverage check (routes.go) fails the run when a route has
//     no row and no stated exemption.
//   - MCP (agents.go): B's agent calls the tools that take a channel or an
//     id on A's objects; each is refused as for a ghost.
//   - The media half (media.go): B cannot get a session for A's room, a B
//     token bent to name A's room is refused by LiveKit, and a B viewer
//     subscribed to everything in B's room is told nothing of A's.
//   - Occupancy (occupancy.go): B's occupancy sockets, open from before A
//     is active, carry rows for B's channels alone, every count zero while
//     only A is in voice, and what B's own presence says once B is; B reads
//     it as a row of the matrix, and B's agent is refused it.
//   - Revocation (revoke.go): a second credential of a tester's, holding
//     every socket variant, is revoked alone, and the sockets of her first
//     stay open; then the tester, holding every socket variant, a LiveKit
//     participant and the share, is disabled, then everyone from A's open
//     link, then the link is closed. Each is timed and held to its
//     bound: 2 s for a socket, one voice poller pass plus a room rotation for
//     LiveKit (read from the server's source). Then the revoked must stay
//     out: a kept ticket and a kept join grant no longer work.
//   - The guests (issue #717, guests.go and the files it names): a third
//     part of the world, the plaza, open at the bare address, where members
//     who made accounts there leave private markers before any guest comes.
//     Guests are removed for now (issue #1005): no route starts a visit,
//     so the guests' visits are started beside the server in its database
//     (seed.go), as the entry routes did, each guest calling from a client
//     address of its own through the trusted proxy, and the entry routes
//     are tried as refusals; the guests run the guests' rows of the matrix
//     (the history from the visit's bound alone, nothing of A's, every
//     right a guest may not have refused) and the newcomers' rows; the
//     entry gates are stepped through; a guest becomes a member, and two
//     visits run out on the real clock while the rest goes on; End visit,
//     the operator's end and End all guest visits are timed as the
//     revocations are; and a member's reset, a device link and the
//     operator's sign-in, recent-auth proof and offline recovery hold
//     against the races that matter.
//
// The run says first which commit and conchd it tested.
//
// Without Docker, or without the network to pull the image or lk, the media
// half and LiveKit's side of revocation are skipped with one line and the
// program exits 0, unless CI is set: then any skip is a failure. It prints
// one "isolation-check: PASS" line, with the measured revocation times, when
// everything ran. A SIGINT or SIGTERM stops the run and removes what it
// started.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	h, err := newHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "isolation-check: FAIL:", err)
		return 1
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		for n := 1; ; n++ {
			<-sigs
			switch n {
			case 1:
				fmt.Fprintln(os.Stderr, "isolation-check: interrupted; cleaning up")
				stopCancel()
			case 2:
				fmt.Fprintln(os.Stderr, "isolation-check: still cleaning up; signal again to give up (a container may be left behind)")
			default:
				fmt.Fprintln(os.Stderr, "isolation-check: giving up on clean-up")
				os.Exit(130)
			}
		}
	}()

	start := time.Now()
	sum, err := guardedRun(h)
	h.teardown()
	switch {
	case stopCtx.Err() != nil:
		fmt.Fprintln(os.Stderr, "isolation-check: interrupted")
		return 130
	case err != nil:
		fmt.Fprintln(os.Stderr, "isolation-check: FAIL:", h.redact(err.Error()))
		return 1
	case sum.skipped != "":
		fmt.Printf("isolation-check: SKIPPED %s; everything else passed in %s (%s)\n", sum.skipped, time.Since(start).Round(time.Second), sum.line())
		return 0
	}
	fmt.Printf("isolation-check: PASS in %s (%s)\n", time.Since(start).Round(time.Second), sum.line())
	return 0
}

// summary is what the last line reports.
type summary struct {
	skipped string
	timings []timing
	bounds  voiceBounds
}

func (s summary) line() string {
	var parts []string
	for _, t := range s.timings {
		p := fmt.Sprintf("%s: sockets closed within %s", t.what, slowest(t.sockets))
		if len(t.livekit) > 0 {
			p += fmt.Sprintf(", LiveKit participant gone within %s of a %s bound", slowest(t.livekit), s.bounds.total())
		}
		parts = append(parts, p)
	}
	return "revocation: " + strings.Join(parts, "; ")
}

// guardedRun is run, turning a panic into a failure so that realMain still
// tears everything down.
func guardedRun(h *harness) (sum summary, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return run(h)
}

// inCI is true when the job says so. In CI a skip is a failure.
func inCI() bool {
	v := strings.ToLower(os.Getenv("CI"))
	return v != "" && v != "false" && v != "0"
}

func run(h *harness) (summary, error) {
	var sum summary
	// The coverage check first: it needs nothing running.
	table, err := h.checkCoverage()
	if err != nil {
		return sum, err
	}
	h.say("ok   coverage: each of the %d routes of internal/server/routes.go has a row in the matrix (%d rows, %d more as an agent, %d as a guest, %d as a newcomer) or a stated exemption (%d)",
		len(table), len(matrixRows()), len(agentRows()), len(guestRows()), len(entryRows()), len(exempt))
	if sum.bounds, err = readVoiceBounds(h.repo); err != nil {
		return sum, err
	}
	h.say("ok   bounds: sockets %s; LiveKit %s", socketBound, sum.bounds)

	if err := h.build(); err != nil {
		return sum, fmt.Errorf("build conchd: %w", err)
	}
	var lk *liveKit
	lkBin, reason, err := h.kit.Prepare(stopCtx)
	if err != nil {
		return sum, err
	}
	if reason != "" {
		if inCI() {
			return sum, fmt.Errorf("the media half cannot run in CI: %s", reason)
		}
		sum.skipped = "the media half and LiveKit's side of revocation: " + reason
	} else {
		srv, err := h.kit.StartLiveKit(stopCtx)
		if err != nil {
			return sum, err
		}
		lk = &liveKit{srv: srv, adm: srv.Admin(), lkBin: lkBin}
		dir, err := h.mkdir("ogg-")
		if err != nil {
			return sum, err
		}
		lk.ogg = filepath.Join(dir, "silence.ogg")
		if err := lkit.WriteSilentOpus(lk.ogg, 900); err != nil {
			return sum, err
		}
		h.say("ok   LiveKit is up (container %s, the pinned image, auto_create off)", srv.Name)
	}
	var srv *lkit.Server
	if lk != nil {
		srv = lk.srv
	}
	d, err := h.startConchd(srv)
	if err != nil {
		return sum, err
	}
	if err := h.recordBuild(d); err != nil {
		return sum, err
	}
	w := newWorld(h, d, lk)

	if err := w.buildGroups(); err != nil {
		return sum, fmt.Errorf("the groups: %w", err)
	}
	h.say("ok   the groups: A (%d people and an agent) joined through an open link; B (%d people) through personal invites, the second made by a B tester, and one the operator made by hand, with an agent",
		len(w.a.people), len(w.b.people))

	bob := w.b.people[1]
	bsock, err := openBSockets(bob, w.b.channels)
	if err != nil {
		return sum, fmt.Errorf("group B's sockets: %w", err)
	}
	if err := w.activityA(); err != nil {
		return sum, fmt.Errorf("group A's activity: %w", err)
	}
	if err := w.publicA(); err != nil {
		return sum, fmt.Errorf("group A's public channel: %w", err)
	}
	var v *voiceA
	if lk != nil {
		if v, err = w.startVoiceA(); err != nil {
			return sum, fmt.Errorf("group A in voice: %w", err)
		}
		if err := bsock.checkOccupancyWhileAIsInVoice(w); err != nil {
			return sum, fmt.Errorf("group B's occupancy sockets: %w", err)
		}
	}
	text, secrets, ids := w.marks.count()
	h.say("ok   A is active: messages (v0, v1, v2, a thread, a whisper, a net), a file, read state, an agent's session and run, an intervention, an approval, a report%s; markers: %d pieces of A's text, %d secrets, %d ids",
		map[bool]string{true: ", voice and a screen share", false: ""}[lk != nil], text, secrets, ids)

	c, err := w.controlB(1)
	if err != nil {
		return sum, fmt.Errorf("group B's control: %w", err)
	}
	if err := bsock.checkQuiet(w, c); err != nil {
		return sum, fmt.Errorf("group B's sockets: %w", err)
	}
	h.say("ok   sockets: B's %d open sockets (every socket variant on both B channels, and the occupancy socket with the bearer and a ticket) carried B's control, none with anything of A", len(bsock.feeds))

	total := 0
	for _, m := range []*member{w.b.people[0], w.b.people[2]} {
		n, err := w.runMatrix(m, matrixRows())
		if err != nil {
			return sum, err
		}
		total += n
	}
	n, err := w.runMatrix(w.b.agent, agentRows())
	if err != nil {
		return sum, err
	}
	total += n
	h.say("ok   matrix: %d rows as each of two B members (one invited, one made by the operator) and %d as B's agent, %d requests; every refusal as stated and the same as for a thing that never existed, every success B's own, and no body or header with anything of A",
		len(matrixRows()), len(agentRows()), total)
	sort.Strings(w.onBehalf)
	h.say("     acting as an A member on B's own channel and approval was answered: %s", strings.Join(slices.Compact(w.onBehalf), ", "))
	n, codes, err := w.runMCP()
	if err != nil {
		return sum, err
	}
	h.say("ok   MCP: %d tool calls as B's agent about A's objects and ghosts, each refused alike and with nothing of A (%s)", n, strings.Join(codes, ", "))

	if n, err = w.channelsChangedRows(bsock); err != nil {
		return sum, fmt.Errorf("the channel-list signal: %w", err)
	}
	h.say("ok   channel list: ann added to and removed from a channel of A's, with ola of B in it, and the channel renamed; each conch.channels_changed.v1 went to its subject alone, exactly %s, and none of %d events sockets (bob's, ola's, the operator's, both agents') carried one of another's (L1-L4, L7)", channelsChangedWire, n)

	if v != nil {
		if err := w.mediaB(v); err != nil {
			return sum, fmt.Errorf("media: %w", err)
		}
	}

	if sum.timings, err = w.revoke(v, sum.bounds); err != nil {
		return sum, fmt.Errorf("revocation: %w", err)
	}
	took, err := w.removalDropsTheRow()
	if err != nil {
		return sum, fmt.Errorf("removal from a channel: %w", err)
	}
	h.say("ok   removal: bea, removed from one of B's channels, keeps her occupancy sockets, which lost that channel's row within %s (bound %s); she is put back", took.Round(100*time.Microsecond), socketBound)

	// The plaza and its guests (issue #717). B's sockets stay open through
	// it, and the second control scans them for the plaza's private markers
	// too.
	gt, err := w.guestPhase(sum.bounds)
	if err != nil {
		return sum, fmt.Errorf("the guests: %w", err)
	}
	sum.timings = append(sum.timings, gt...)

	// B's sockets have been open through all of it: a second control, and
	// every frame since they opened is scanned again.
	if c, err = w.controlB(2); err != nil {
		return sum, fmt.Errorf("group B's second control: %w", err)
	}
	if err := bsock.checkQuiet(w, c); err != nil {
		return sum, fmt.Errorf("group B's sockets, at the end: %w", err)
	}
	h.say("ok   sockets at the end: B's %d sockets, open through the matrix, the media half, every revocation and the guests' phase, carried B's second control and %d frames in all, none with anything of A's or anything private of the plaza's", len(bsock.feeds), bsock.frames())

	if err := h.checkCalls(table); err != nil {
		return sum, err
	}
	if err := h.scanLogs(); err != nil {
		return sum, err
	}
	h.say("ok   the run used only routes of the public table, and no credential, ticket or token is in conchd's or lk's output")
	return sum, nil
}
