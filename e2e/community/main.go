// Command community-check is the community model's newcomer scenario, end to
// end (issue #989, ADR-013, docs/design/community-model.md §10, "The tester
// story"). It must pass before the community model is called done.
//
// It runs a real conchd and the pinned LiveKit and lk (Docker; the same
// e2e/internal/lkit as e2e/isolation and e2e/voice), calls the public API
// only, and walks six steps. The owner is the operator who bootstraps the
// server, alice is the newcomer, #general is the default channel and
// #project starts private.
//
//  1. The owner makes a member link (POST /v2/invites). Alice, with a
//     phone's User-Agent, opens the bare address and is offered only Sign
//     in (GET /v1/join: account and guest both false, password login on),
//     previews the link, and the link names the owner and #general alone.
//  2. Alice redeems the link and creates an account (POST /v2/invites/redeem).
//     She lands in #general and not in private #project, reads #general's
//     full history, posted before she joined, sees the owner in the member
//     directory, and a message the owner posts afterwards reaches her open
//     socket.
//  3. The owner turns #project public. Alice sees it in the channel
//     directory (and did not before), joins it, reads its history, and
//     joins voice there: LiveKit lists her in the room.
//  4. The owner switches alice's voice off. Alice is out of LiveKit's room
//     within the design's bound (§2.5: one poller pass plus a join token's
//     lifetime, both read from the server's source), the time is measured,
//     and a new session is refused voice_not_allowed. The owner switches it
//     on again and alice rejoins.
//  5. The owner closes the link. It no longer redeems, and no longer
//     previews.
//  6. The server admits no guest: the owner's PUT /v1/join/settings
//     {guests_enabled: true} is 409 join_policy_conflict, the bare address
//     offers no guest, and POST /v1/join/guest is 403 guests_disabled.
//
// What this proves: the API and media-control path of the story, on one
// machine, with one conchd and LiveKit in Docker. Every principal is a
// separate account driven by this program, and no AI runtime is involved.
//
// What it does not prove: the browser's rendering (phone layout included;
// that is the Playwright work of I9a to I9c), real audio, or a LAN or VM
// deployment. A deployed-build check belongs to the alpha's acceptance record
// (#718). Cross-group isolation is e2e/isolation's.
//
// Without Docker, the image or lk the program fails: a skipped scenario is
// not a passed one. It exits non-zero naming the first step that failed, and
// prints one "community-check: PASS" line, with the measured voice-off time,
// when every step ran. A SIGINT or SIGTERM stops the run and removes what it
// started, and only that.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	h, err := newHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "community-check: FAIL:", err)
		return 1
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		for n := 1; ; n++ {
			<-sigs
			switch n {
			case 1:
				fmt.Fprintln(os.Stderr, "community-check: interrupted; cleaning up")
				stopCancel()
			case 2:
				fmt.Fprintln(os.Stderr, "community-check: still cleaning up; signal again to give up (a container may be left behind)")
			default:
				fmt.Fprintln(os.Stderr, "community-check: giving up on clean-up")
				os.Exit(130)
			}
		}
	}()

	start := time.Now()
	sum, err := guardedRun(h)
	h.teardown()
	switch {
	case stopCtx.Err() != nil:
		fmt.Fprintln(os.Stderr, "community-check: interrupted")
		return 130
	case err != nil:
		fmt.Fprintln(os.Stderr, "community-check: FAIL:", h.redact(err.Error()))
		return 1
	}
	fmt.Printf("community-check: PASS in %s (voice off took %s of a %s bound)\n",
		time.Since(start).Round(time.Second), sum.voiceOff.Round(100*time.Microsecond), sum.bound)
	return 0
}

// summary is what the last line reports.
type summary struct {
	voiceOff, bound time.Duration
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

func run(h *harness) (summary, error) {
	var sum summary
	var err error
	if sum.bound, err = readVoiceBound(h.repo); err != nil {
		return sum, err
	}
	h.say("ok   bound: voice off takes at most %s (one poller pass plus a join token's lifetime, design §2.5)", sum.bound)
	if err := h.build(); err != nil {
		return sum, fmt.Errorf("build conchd: %w", err)
	}
	lkBin, reason, err := h.kit.Prepare(stopCtx)
	if err != nil {
		return sum, err
	}
	if reason != "" {
		// Whatever CI says, this scenario has no half that can run without
		// LiveKit: step 3 and step 4 are voice.
		return sum, fmt.Errorf("the scenario cannot run: %s", reason)
	}
	lksrv, err := h.kit.StartLiveKit(stopCtx)
	if err != nil {
		return sum, err
	}
	h.say("ok   LiveKit is up (container %s, the pinned image)", lksrv.Name)
	d, err := h.startConchd(lksrv)
	if err != nil {
		return sum, err
	}
	if err := h.recordBuild(d); err != nil {
		return sum, err
	}
	s, err := newScenario(h, d, lksrv, lkBin)
	if err != nil {
		return sum, fmt.Errorf("set up the community: %w", err)
	}
	for i, step := range s.steps() {
		if err := step.run(); err != nil {
			return sum, fmt.Errorf("step %d (%s): %w", i+1, step.name, err)
		}
		h.say("ok   step %d: %s", i+1, step.name)
	}
	sum.voiceOff = s.voiceOff
	if err := h.scanLogs(); err != nil {
		return sum, err
	}
	h.say("ok   no credential, ticket or token of the run is in conchd's or lk's output")
	return sum, nil
}
