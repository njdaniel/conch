package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// The transmit rule checked over every short sequence of inputs, and over many
// long random ones (issue #135, docs/design/conch-voice.md §6).
//
// The scenario tables in voice_transmit_rule_test.go say what the rule does in
// the cases someone thought of. This file says what must hold in every case,
// and tries them all: each sentence of §6's rule is a check on the rows that
// came out, given only the inputs that went in. It is not a second
// implementation of the rule. It keeps no state of the rule's own: what it
// knows of the reported state and of what is open it reads back from the rows,
// as a reader of the audit log would.
//
// It grew from the security review of #135, which compared the rule with an
// independent model over all 47 million sequences of up to nine inputs and
// found no difference. This is the part of that which runs in seconds.

// ruleInput is one input to the rule for a single principal.
type ruleInput int

const (
	inTalking ruleInput = iota // a pass that sees the microphone transmitting
	inQuiet                    // a pass that sees the principal, not transmitting
	inAbsent                   // a pass that does not find the principal
	inUnread                   // a pass that learns nothing about the principal
	inStarted                  // a report
	inStopped                  // a report
	inRemoved                  // removed by name, or the connection replaced
	numRuleInputs
)

var ruleInputNames = [numRuleInputs]string{"talking", "quiet", "absent", "unread", "started", "stopped", "removed"}

// The two durations of §6, as the design note states them and not as the code
// names them: the check is of the code against the note.
const (
	section6Ration = 3 * time.Second // "in the previous 3 s"
	section6NoStop = 2 * time.Second // "Not transmitting on every pass for 2 s"
)

// ruleUnderCheck is what the check drives: the ledger, or the ledger with its
// rows doctored (TestRuleInvariantsCheckFindsABrokenRule).
type ruleUnderCheck interface {
	pass(at time.Time, seen map[int64]micObservation, skip map[int64]struct{}) map[int64][]transmitRow
	report(at time.Time, pid int64, started bool) (transmitRow, uint64, bool)
	leave(at time.Time, pid int64) []transmitRow
	drop(at time.Time) []transmitRow
	unsettled() bool
}

// ruleLook is one look the rule had at the principal: a pass that read them,
// or their removal, which the rule takes as a look that finds them gone.
type ruleLook struct {
	at           time.Time
	transmitting bool
	// unaccounted: transmitting, with the reported state `stopped` at that
	// instant according to the rows written so far.
	unaccounted bool
	// reportBefore: a report changed the reported state (wrote a row) since
	// the look before this one, or since the beginning.
	reportBefore bool
	// recorded: a voice_transmit_unreported row timed at this look exists.
	recorded bool
}

// ruleTally counts how often each part of the rule was met, so that a run
// which never reached one is not taken for a run in which it held.
type ruleTally struct {
	sequences     int
	twoInARow     int
	loneRecorded  int // a lone unaccounted pass with no report near it
	edgeExcused   int
	rationExcused int
	rationDenied  int
	noStopReport  int
	leftClosed    int
	oneRowForBoth int // one reason=left row closing both kinds
}

func (a *ruleTally) add(b ruleTally) {
	a.sequences += b.sequences
	a.twoInARow += b.twoInARow
	a.loneRecorded += b.loneRecorded
	a.edgeExcused += b.edgeExcused
	a.rationExcused += b.rationExcused
	a.rationDenied += b.rationDenied
	a.noStopReport += b.noStopReport
	a.leftClosed += b.leftClosed
	a.oneRowForBoth += b.oneRowForBoth
}

// checkRuleInvariants feeds inputs to rule at the given times, drops it at the
// end (a rotation), and checks the rows against §6. It returns what is wrong,
// if anything.
func checkRuleInvariants(rule ruleUnderCheck, inputs []ruleInput, times []time.Time) (problems []string, tally ruleTally) {
	const pid = rulePID
	bad := func(i int, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("input %d: %s", i, fmt.Sprintf(format, args...)))
	}

	// What is open, read back from the rows and never from the rule.
	var (
		reported     bool      // a reported transmission is open
		startedAt    time.Time // when its `started` was reported
		unreported   bool      // an unreported transmission is open
		unreportedAt time.Time
		all          []transmitRow
	)
	// Facts about the inputs.
	var (
		looks        []ruleLook
		reportSince  bool        // a report wrote a row since the last look
		lastTalking  time.Time   // the last pass that saw the microphone transmitting
		anyTalking   bool        // there has been one
		present      bool        // the last look found the principal in the room
		excusedTimes []time.Time // lone passes excused the second way
		owedNoStop   bool        // the 2 s close came due on a pass that closed something else
	)
	// quietSince is where the 2 s of the no_stop_report rule start: the later
	// of the `started` report and the last pass that saw a transmission.
	quietSince := func() time.Time {
		if anyTalking && lastTalking.After(startedAt) {
			return lastTalking
		}
		return startedAt
	}

	// opened is called for a voice_transmit_unreported row. §6: "timed at the
	// first of them". The row is written by the look after the one that first
	// saw the transmission, so it must carry the time of the look before
	// this input, and that look must have been unaccounted and the first of
	// its run.
	opened := func(i int, r transmitRow) {
		n := len(looks) - 1
		switch {
		case unreported:
			bad(i, "%s while an unreported transmission is open", rowText(r))
		case n < 0 || !looks[n].unaccounted || !looks[n].at.Equal(r.at):
			bad(i, "%s is not timed at the look before, an unaccounted one", rowText(r))
		case n > 0 && looks[n-1].unaccounted:
			bad(i, "%s is timed at the second unaccounted pass of a run, not the first", rowText(r))
		default:
			looks[n].recorded = true
		}
		unreported, unreportedAt = true, r.at
	}

	// judge is called when the look after looks[n] is known (next), or when
	// there will be none (nil, at the final drop). It checks the sentences of
	// §6 about unaccounted passes against whether looks[n] was recorded.
	judge := func(i, n int, next *ruleLook) {
		p := looks[n]
		if !p.unaccounted {
			return
		}
		var prev *ruleLook
		if n > 0 {
			prev = &looks[n-1]
		}
		if prev != nil && prev.unaccounted {
			return // not the first of a run: the run's first speaks for it
		}
		if next != nil && next.unaccounted {
			// §6: "Two unaccounted passes in a row open an unreported
			// transmission, timed at the first of them. No report excuses
			// that."
			tally.twoInARow++
			if !p.recorded {
				bad(i, "two unaccounted passes in a row (at %v and %v) and no unreported row timed at the first", p.at.Sub(ruleT0), next.at.Sub(ruleT0))
			}
			return
		}
		// §6: "One unaccounted pass alone is also an unreported
		// transmission ... unless it is excused."
		near := p.reportBefore || (next != nil && next.reportBefore) || (next == nil && reportSince)
		edge := prev == nil || !prev.transmitting || next == nil || !next.transmitting
		switch {
		case !near:
			tally.loneRecorded++
			if !p.recorded {
				bad(i, "a lone unaccounted pass at %v with no report that changed the state near it was not recorded", p.at.Sub(ruleT0))
			}
		case edge:
			// "... excused when a report was received after the pass before
			// it and before the pass after it, and ... one of those two
			// passes saw the microphone not transmitting".
			tally.edgeExcused++
			if p.recorded {
				bad(i, "a lone unaccounted pass at %v at the edge of a transmission, with a report near it, was recorded", p.at.Sub(ruleT0))
			}
		case p.recorded:
			// "... or no other pass was excused this second way in the
			// previous 3 s": it was recorded, so one must have been.
			tally.rationDenied++
			within := false
			for _, e := range excusedTimes {
				if p.at.Sub(e) < section6Ration {
					within = true
				}
			}
			if !within {
				bad(i, "a lone unaccounted pass at %v with a report near it was recorded though none was excused the second way in the 3 s before", p.at.Sub(ruleT0))
			}
		default:
			tally.rationExcused++
			for _, e := range excusedTimes {
				if p.at.Sub(e) < section6Ration {
					bad(i, "two lone passes excused the second way within 3 s: at %v and %v", e.Sub(ruleT0), p.at.Sub(ruleT0))
				}
			}
			excusedTimes = append(excusedTimes, p.at)
		}
	}

	for i, in := range inputs {
		at := times[i]
		var rows []transmitRow
		isLook := in == inTalking || in == inQuiet || in == inAbsent || in == inRemoved
		switch in {
		case inTalking:
			rows = rule.pass(at, map[int64]micObservation{pid: micTransmitting}, nil)[pid]
		case inQuiet:
			rows = rule.pass(at, map[int64]micObservation{pid: micQuiet}, nil)[pid]
		case inAbsent:
			rows = rule.pass(at, map[int64]micObservation{}, nil)[pid]
		case inUnread:
			rows = rule.pass(at, map[int64]micObservation{}, map[int64]struct{}{pid: {}})[pid]
		case inStarted, inStopped:
			if row, _, changed := rule.report(at, pid, in == inStarted); changed {
				rows = []transmitRow{row}
			}
		case inRemoved:
			rows = rule.leave(at, pid)
		}

		reportedBefore, unreportedBefore := reported, unreported
		gone := in == inRemoved || (in == inAbsent && present)
		closes, closedUnreported := 0, false
		for _, r := range rows {
			all = append(all, r)
			// A row is timed at its input, except the row that opens an
			// unreported transmission.
			if r.action != transmitActionUnreported && !r.at.Equal(at) {
				bad(i, "%s is not timed at its input", rowText(r))
			}
			switch {
			case r.source == transmitSourceReported:
				// "A report that does not change the state ... writes
				// nothing"; one that does writes its own row and no other.
				want := transmitActionStopped
				if in == inStarted {
					want = transmitActionStarted
				}
				if (in != inStarted && in != inStopped) || r.action != want || reported == (in == inStarted) || r.reason != "" {
					bad(i, "%s from input %s with the reported state started=%v", rowText(r), ruleInputNames[in], reported)
				}
				reported = in == inStarted
				if reported {
					startedAt = at
				}
				reportSince = true
			case !isLook:
				bad(i, "%s from input %s: only a look at the principal makes the poller write", rowText(r), ruleInputNames[in])
			case r.action == transmitActionUnreported:
				opened(i, r)
			case r.action == transmitActionStopped && r.source == transmitSourceObserved && r.reason == transmitReasonNoStopReport:
				// "Not transmitting on every pass for 2 s while the reported
				// state is `started`."
				closes++
				tally.noStopReport++
				switch {
				case !reported:
					bad(i, "%s with the reported state stopped", rowText(r))
				case in == inTalking || gone:
					bad(i, "%s on input %s", rowText(r), ruleInputNames[in])
				case at.Sub(quietSince()) < section6NoStop:
					bad(i, "%s only %v after the later of the started report and the last pass that saw a transmission", rowText(r), at.Sub(quietSince()))
				}
				reported = false
			case r.action == transmitActionStopped && r.source == transmitSourceObserved:
				// "When more than one reason to close applies on a pass, one
				// row is written, with the first of: left, muted, reported."
				closes++
				want := transmitReasonReported
				switch {
				case gone:
					want = transmitReasonLeft
				case in != inTalking:
					want = transmitReasonMuted
				}
				if r.reason != want {
					bad(i, "%s: the first reason that applies on input %s is %s", rowText(r), ruleInputNames[in], want)
				}
				switch {
				case r.reason != transmitReasonLeft && !unreported:
					bad(i, "%s with no unreported transmission open", rowText(r))
				case r.reason == transmitReasonLeft && !unreported && !reported:
					bad(i, "%s with nothing open", rowText(r))
				}
				if unreported {
					closedUnreported = true
					if r.at.Before(unreportedAt) {
						bad(i, "%s is timed before the row it closes", rowText(r))
					}
				}
				if r.reason == transmitReasonLeft {
					tally.leftClosed++
					if unreported && reported {
						tally.oneRowForBoth++
					}
					reported = false
				}
				unreported = false
			default:
				bad(i, "%s: not a row the rule writes", rowText(r))
			}
		}
		// "one row is written": never two closing rows from one input.
		if closes > 1 {
			bad(i, "%d closing rows from one input", closes)
		}
		if !isLook {
			continue
		}

		look := ruleLook{at: at, transmitting: in == inTalking, unaccounted: in == inTalking && !reportedBefore, reportBefore: reportSince}
		looks = append(looks, look)
		if n := len(looks) - 1; n > 0 {
			judge(i, n-1, &looks[n])
		}
		// "An unreported transmission is closed by the first later pass that
		// sees the microphone muted, the participant gone, or the reported
		// state `started`", and by no other.
		if unreported && !look.unaccounted {
			bad(i, "an unreported transmission is still open after a look that was not unaccounted")
		}
		if unreportedBefore && look.unaccounted && !unreported {
			bad(i, "an unreported transmission was closed by an unaccounted pass")
		}
		// "When a participant the poller had seen is gone ... every
		// transmission still open for it ... is closed at once."
		if gone && (reported || unreported) {
			bad(i, "something is still open after the principal was gone")
		}
		// The 2 s close comes on the first pass at or after 2 s that does not
		// see the microphone transmitting. It may wait one pass, if that pass
		// closed an unreported transmission (one closing row a pass), and no
		// longer.
		switch {
		case in == inTalking:
			lastTalking, anyTalking = at, true
			owedNoStop = false
		case reported && at.Sub(quietSince()) >= section6NoStop:
			if owedNoStop || !closedUnreported {
				bad(i, "reported started with no transmission seen for %v, and this pass did not close it", at.Sub(quietSince()))
			}
			owedNoStop = true
		default:
			owedNoStop = false
		}
		present = in == inTalking || in == inQuiet
		reportSince = false
	}

	// The room is rotated. Everything open is closed, with reason=left; a
	// lone unaccounted pass not yet judged is judged now, and may be opened
	// and closed by the drop.
	end := ruleT0
	if len(times) > 0 {
		end = times[len(times)-1].Add(time.Millisecond)
	}
	final := len(inputs)
	closes := 0
	for _, r := range rule.drop(end) {
		all = append(all, r)
		switch {
		case r.action == transmitActionUnreported:
			opened(final, r)
		case r.action == transmitActionStopped && r.source == transmitSourceObserved && r.reason == transmitReasonLeft && r.at.Equal(end):
			closes++
			if !reported && !unreported {
				bad(final, "the drop wrote %s with nothing open", rowText(r))
			}
			reported, unreported = false, false
		default:
			bad(final, "the drop wrote %s", rowText(r))
		}
	}
	if closes > 1 {
		bad(final, "the drop wrote %d closing rows", closes)
	}
	if reported || unreported {
		bad(final, "something is still open after the drop")
	}
	if len(looks) > 0 {
		judge(final, len(looks)-1, nil)
	}
	// Aim 4: every row that opens a transmission is followed by a row that
	// closes it.
	pairing, open := checkTransmitPairs(logRowsFromRule(all))
	for _, p := range append(pairing, open...) {
		bad(final, "pairing: %s", p)
	}
	if rule.unsettled() {
		bad(final, "the rule holds something unsettled after a drop")
	}
	tally.sequences = 1
	return problems, tally
}

// ruleSequenceText renders a sequence for a failure message.
func ruleSequenceText(inputs []ruleInput, times []time.Time) string {
	var sb strings.Builder
	for i, in := range inputs {
		fmt.Fprintf(&sb, "%s@%d ", ruleInputNames[in], times[i].Sub(ruleT0).Milliseconds())
	}
	return strings.TrimSpace(sb.String())
}

// everyRuleSequence calls visit with every sequence of 1 to maxLen inputs.
// Passes are 400 ms apart, and a report or a removal comes 50 ms after the
// input before it, so that five quiet passes after a `started` reach the 2 s
// rule exactly. The slices are reused between calls.
func everyRuleSequence(maxLen int, visit func(inputs []ruleInput, times []time.Time)) {
	inputs := make([]ruleInput, maxLen)
	times := make([]time.Time, maxLen)
	var walk func(depth int, now time.Time)
	walk = func(depth int, now time.Time) {
		if depth > 0 {
			visit(inputs[:depth], times[:depth])
		}
		if depth == maxLen {
			return
		}
		for in := ruleInput(0); in < numRuleInputs; in++ {
			step := 400 * time.Millisecond
			if in == inStarted || in == inStopped || in == inRemoved {
				step = 50 * time.Millisecond
			}
			inputs[depth], times[depth] = in, now.Add(step)
			walk(depth+1, now.Add(step))
		}
	}
	walk(0, ruleT0)
}

// TestTransmitRuleInvariantsExhaustive checks every sequence of up to seven
// inputs (six with -short): 960,799 of them.
func TestTransmitRuleInvariantsExhaustive(t *testing.T) {
	maxLen := 7
	if testing.Short() {
		maxLen = 6
	}
	var total ruleTally
	failures := 0
	everyRuleSequence(maxLen, func(inputs []ruleInput, times []time.Time) {
		problems, tally := checkRuleInvariants(&transmitLedger{}, inputs, times)
		total.add(tally)
		if len(problems) > 0 {
			failures++
			if failures <= 5 {
				t.Errorf("%s\n  %s", ruleSequenceText(inputs, times), strings.Join(problems, "\n  "))
			}
		}
	})
	if failures > 5 {
		t.Errorf("and %d more sequences with problems", failures-5)
	}
	want := 0
	for n, p := 1, 1; n <= maxLen; n++ {
		p *= int(numRuleInputs)
		want += p
	}
	if total.sequences != want {
		t.Errorf("checked %d sequences, want all %d of length 1 to %d", total.sequences, want, maxLen)
	}
	// Every part of the rule a short sequence can reach was reached. (A
	// refusal of the second way needs ten inputs; the random test has them.)
	for name, n := range map[string]int{
		"two unaccounted passes in a row":         total.twoInARow,
		"a lone pass with no report near it":      total.loneRecorded,
		"a lone pass excused at an edge":          total.edgeExcused,
		"a lone pass excused the second way":      total.rationExcused,
		"a close for want of a stop report":       total.noStopReport,
		"a close because the principal left":      total.leftClosed,
		"one left row closing both kinds at once": total.oneRowForBoth,
	} {
		if n == 0 {
			t.Errorf("no sequence reached: %s", name)
		}
	}
	t.Logf("%+v", total)
}

// TestTransmitRuleInvariantsRandom checks long random sequences, with passes
// spaced as the poller spaces them (voicePassGapMin plus an exponential with
// mean voicePassGapMean, capped at voicePassGapMax) and reports at any time
// between. Long sequences are what reach the 3 s ration, both the excuse and
// its refusal. The seed is fixed, so a failure is reproducible.
func TestTransmitRuleInvariantsRandom(t *testing.T) {
	sequences := 20000
	if testing.Short() {
		sequences = 4000
	}
	rng := rand.New(rand.NewSource(135))
	var total ruleTally
	failures := 0
	inputs := make([]ruleInput, 60)
	times := make([]time.Time, 60)
	// A third of the sequences are built round the pattern that the second
	// way of excusing exists for and is rationed against: transmitting on
	// every pass, with `stopped` before every other pass and `started` after
	// it. The rest are any inputs at all.
	straddle := [...]ruleInput{inStarted, inTalking, inStopped, inTalking}
	for seq := range sequences {
		now := ruleT0
		for i := range inputs {
			// Mostly passes that see the principal and reports; now and
			// then an absence, a removal, a pass that reads nothing.
			in := ruleInput(rng.Intn(int(numRuleInputs)))
			if (in == inAbsent || in == inUnread || in == inRemoved) && rng.Intn(4) != 0 {
				in = [...]ruleInput{inTalking, inTalking, inQuiet, inStarted, inStopped}[rng.Intn(5)]
			}
			if seq%3 == 0 && rng.Intn(10) != 0 {
				in = straddle[i%len(straddle)]
			}
			var step time.Duration
			switch in {
			case inStarted, inStopped, inRemoved:
				step = time.Duration(rng.Intn(120)) * time.Millisecond
			default:
				step = min(voicePassGapMin+time.Duration(rng.ExpFloat64()*float64(voicePassGapMean)), voicePassGapMax).Truncate(time.Millisecond)
			}
			now = now.Add(step)
			inputs[i], times[i] = in, now
		}
		problems, tally := checkRuleInvariants(&transmitLedger{}, inputs, times)
		total.add(tally)
		if len(problems) > 0 {
			failures++
			if failures <= 5 {
				t.Errorf("%s\n  %s", ruleSequenceText(inputs, times), strings.Join(problems, "\n  "))
			}
		}
	}
	if failures > 5 {
		t.Errorf("and %d more sequences with problems", failures-5)
	}
	for name, n := range map[string]int{
		"two unaccounted passes in a row":                 total.twoInARow,
		"a lone pass with no report near it":              total.loneRecorded,
		"a lone pass excused at an edge":                  total.edgeExcused,
		"a lone pass excused the second way":              total.rationExcused,
		"a lone pass refused the second way, inside 3 s":  total.rationDenied,
		"a close for want of a stop report":               total.noStopReport,
		"a close because the principal left":              total.leftClosed,
		"one left row closing both kinds of transmission": total.oneRowForBoth,
	} {
		if n < 20 {
			t.Errorf("reached only %d times: %s", n, name)
		}
	}
	t.Logf("%+v", total)
}

// doctoredRule is the ledger with something done to the rows the poller's
// side of it writes: the rule as it would be with one sentence of §6 broken.
type doctoredRule struct {
	transmitLedger
	doctor func(at time.Time, rows []transmitRow) []transmitRow
}

func (d *doctoredRule) pass(at time.Time, seen map[int64]micObservation, skip map[int64]struct{}) map[int64][]transmitRow {
	out := d.transmitLedger.pass(at, seen, skip)
	for pid, rows := range out {
		out[pid] = d.doctor(at, rows)
	}
	return out
}

func (d *doctoredRule) leave(at time.Time, pid int64) []transmitRow {
	return d.doctor(at, d.transmitLedger.leave(at, pid))
}

func (d *doctoredRule) drop(at time.Time) []transmitRow {
	return d.doctor(at, d.transmitLedger.drop(at))
}

// TestRuleInvariantsCheckFindsABrokenRule: the check itself. A check that
// cannot fail proves nothing about the rule it passes, so it is run on the
// rule with its rows doctored, one way at a time, over every sequence of up to
// six inputs: each way must be found wrong by at least one. (That it also
// finds the rule's own code wrong when that is changed is what the mutation
// run in the pull request shows.)
func TestRuleInvariantsCheckFindsABrokenRule(t *testing.T) {
	without := func(drop func(transmitRow) bool) func(time.Time, []transmitRow) []transmitRow {
		return func(_ time.Time, rows []transmitRow) []transmitRow {
			var out []transmitRow
			for _, r := range rows {
				if !drop(r) {
					out = append(out, r)
				}
			}
			return out
		}
	}
	each := func(change func(at time.Time, r *transmitRow)) func(time.Time, []transmitRow) []transmitRow {
		return func(at time.Time, rows []transmitRow) []transmitRow {
			out := make([]transmitRow, len(rows))
			for i, r := range rows {
				change(at, &r)
				out[i] = r
			}
			return out
		}
	}
	tests := []struct {
		name   string
		doctor func(time.Time, []transmitRow) []transmitRow
	}{
		{"an unreported transmission is never opened or closed", without(func(r transmitRow) bool {
			return r.action == transmitActionUnreported || r.reason == transmitReasonMuted || r.reason == transmitReasonReported
		})},
		{"an unreported transmission is opened and never closed", without(func(r transmitRow) bool {
			return r.reason == transmitReasonMuted || r.reason == transmitReasonReported
		})},
		{"the unreported row is timed when it is written", each(func(at time.Time, r *transmitRow) {
			if r.action == transmitActionUnreported {
				r.at = at
			}
		})},
		{"a reported start with no transmission is never closed", without(func(r transmitRow) bool {
			return r.reason == transmitReasonNoStopReport
		})},
		{"leaving closes nothing", without(func(r transmitRow) bool { return r.reason == transmitReasonLeft })},
		{"muted is written as reported", each(func(_ time.Time, r *transmitRow) {
			if r.reason == transmitReasonMuted {
				r.reason = transmitReasonReported
			}
		})},
		{"left is written as muted", each(func(_ time.Time, r *transmitRow) {
			if r.reason == transmitReasonLeft {
				r.reason = transmitReasonMuted
			}
		})},
		{"every close is written twice", func(_ time.Time, rows []transmitRow) []transmitRow {
			var out []transmitRow
			for _, r := range rows {
				out = append(out, r)
				if r.action == transmitActionStopped {
					out = append(out, r)
				}
			}
			return out
		}},
		{"a close is timed a millisecond late", each(func(_ time.Time, r *transmitRow) {
			if r.action == transmitActionStopped {
				r.at = r.at.Add(time.Millisecond)
			}
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found := 0
			everyRuleSequence(6, func(inputs []ruleInput, times []time.Time) {
				if problems, _ := checkRuleInvariants(&doctoredRule{doctor: tt.doctor}, inputs, times); len(problems) > 0 {
					found++
				}
			})
			if found == 0 {
				t.Error("the check found nothing wrong with this rule in any sequence of up to six inputs")
			}
		})
	}
	// And undoctored, the same wrapper passes: the wrapper is not what the
	// check objects to.
	everyRuleSequence(6, func(inputs []ruleInput, times []time.Time) {
		same := func(_ time.Time, rows []transmitRow) []transmitRow { return rows }
		if problems, _ := checkRuleInvariants(&doctoredRule{doctor: same}, inputs, times); len(problems) > 0 {
			t.Fatalf("the undoctored rule: %s\n  %s", ruleSequenceText(inputs, times), strings.Join(problems, "\n  "))
		}
	})
}
