package server

import (
	"fmt"
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
)

// Tests of the transmit rule (issue #135, docs/design/conch-voice.md §6) as
// tables of inputs with explicit times. Nothing here sleeps, reads a clock or
// touches a server: a scenario is a list of passes and reports, and the rows
// they must produce.

// ---------------------------------------------------------------------------
// The pairing check (§6, aim 4)

// transmitLogRow is one transmit row as the pairing check reads it, whether it
// came from the rule directly or from the audit log.
type transmitLogRow struct {
	// who tells one principal in one channel from another.
	who    string
	action string
	source string
	reason string
	at     time.Time
}

// checkTransmitPairs reads transmit rows in the order they were written and
// checks aim 4: every row that opens a transmission is followed by a row that
// closes it. It is strict about more than that, so that a rule which writes a
// row it should not is caught too:
//
//   - a reported transmission is opened by voice_transmit_started
//     source=reported and closed by voice_transmit_stopped source=reported,
//     or by the poller with reason=no_stop_report or reason=left;
//   - an unreported one is opened by voice_transmit_unreported
//     source=observed and closed by the poller with reason=muted,
//     reason=reported or reason=left;
//   - reason=left closes whatever is open for the principal, both kinds at
//     once if both are;
//   - nothing is opened twice, nothing is closed that is not open, no row is
//     timed before the row that opened what it closes, and the poller never
//     writes voice_transmit_started.
//
// It returns what is wrong, and what is still open at the end.
func checkTransmitPairs(rows []transmitLogRow) (problems, open []string) {
	type state struct {
		reported, unreported     bool
		reportedAt, unreportedAt time.Time
	}
	states := map[string]*state{}
	var order []string
	bad := func(i int, r transmitLogRow, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("row %d (%s %s source=%s reason=%s): %s", i, r.who, r.action, r.source, r.reason, fmt.Sprintf(format, args...)))
	}
	for i, r := range rows {
		s := states[r.who]
		if s == nil {
			s = &state{}
			states[r.who] = s
			order = append(order, r.who)
		}
		closeReported := func() {
			if r.at.Before(s.reportedAt) {
				bad(i, r, "timed before the started row it closes")
			}
			s.reported = false
		}
		closeUnreported := func() {
			if r.at.Before(s.unreportedAt) {
				bad(i, r, "timed before the unreported row it closes")
			}
			s.unreported = false
		}
		switch r.action {
		case transmitActionStarted:
			switch {
			case r.source != transmitSourceReported:
				bad(i, r, "only a report opens a reported transmission")
			case r.reason != "":
				bad(i, r, "an opening row has no reason")
			case s.reported:
				bad(i, r, "a reported transmission is already open")
			}
			s.reported, s.reportedAt = true, r.at
		case transmitActionUnreported:
			switch {
			case r.source != transmitSourceObserved:
				bad(i, r, "only the poller opens an unreported transmission")
			case r.reason != "":
				bad(i, r, "an opening row has no reason")
			case s.unreported:
				bad(i, r, "an unreported transmission is already open")
			}
			s.unreported, s.unreportedAt = true, r.at
		case transmitActionStopped:
			switch {
			case r.source == transmitSourceReported:
				switch {
				case r.reason != "":
					bad(i, r, "a reported stop has no reason")
				case !s.reported:
					bad(i, r, "no reported transmission is open")
				}
				closeReported()
			case r.source != transmitSourceObserved:
				bad(i, r, "unknown source")
			case r.reason == transmitReasonMuted || r.reason == transmitReasonReported:
				if !s.unreported {
					bad(i, r, "no unreported transmission is open")
				}
				closeUnreported()
			case r.reason == transmitReasonNoStopReport:
				if !s.reported {
					bad(i, r, "no reported transmission is open")
				}
				closeReported()
			case r.reason == transmitReasonLeft:
				if !s.reported && !s.unreported {
					bad(i, r, "nothing is open")
				}
				if s.reported {
					closeReported()
				}
				if s.unreported {
					closeUnreported()
				}
			default:
				bad(i, r, "unknown reason")
			}
		default:
			bad(i, r, "not a transmit row")
		}
	}
	for _, who := range order {
		if states[who].reported {
			open = append(open, who+": a reported transmission (voice_transmit_started) has no closing row")
		}
		if states[who].unreported {
			open = append(open, who+": an unreported transmission (voice_transmit_unreported) has no closing row")
		}
	}
	return problems, open
}

func logRowsFromRule(rows []transmitRow) []transmitLogRow {
	out := make([]transmitLogRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, transmitLogRow{who: strconv.FormatInt(r.principalID, 10), action: r.action, source: r.source, reason: r.reason, at: r.at})
	}
	return out
}

// logRowsFromAudit picks the transmit rows out of an audit log, in the order
// they were written, reading source= and reason= from the detail.
func logRowsFromAudit(events []store.AuditEvent) []transmitLogRow {
	var out []transmitLogRow
	for _, e := range events {
		switch e.Action {
		case store.AuditVoiceTransmitStarted, store.AuditVoiceTransmitStopped, store.AuditVoiceTransmitUnreported:
		default:
			continue
		}
		r := transmitLogRow{who: e.Subject + " " + e.Actor, action: e.Action, at: e.CreatedAt}
		for _, field := range strings.Fields(e.Detail) {
			if v, ok := strings.CutPrefix(field, "source="); ok {
				r.source = v
			}
			if v, ok := strings.CutPrefix(field, "reason="); ok {
				r.reason = v
			}
		}
		out = append(out, r)
	}
	return out
}

// assertRulePairs fails the test unless every transmission the rows open is
// closed by them, and nothing else is wrong with them.
func assertRulePairs(t *testing.T, rows []transmitRow) {
	t.Helper()
	problems, open := checkTransmitPairs(logRowsFromRule(rows))
	for _, p := range append(problems, open...) {
		t.Errorf("pairing: %s", p)
	}
}

// assertTransmitPairs is assertRulePairs for an audit log: every scenario
// that goes through a server ends with it.
func assertTransmitPairs(t *testing.T, events []store.AuditEvent) {
	t.Helper()
	problems, open := checkTransmitPairs(logRowsFromAudit(events))
	for _, p := range append(problems, open...) {
		t.Errorf("pairing: %s", p)
	}
}

// TestCheckTransmitPairs: the check itself. A helper that cannot fail proves
// nothing about the scenarios that call it.
func TestCheckTransmitPairs(t *testing.T) {
	at := func(ms int) time.Time { return ruleT0.Add(time.Duration(ms) * time.Millisecond) }
	row := func(ms int, action, source, reason string) transmitLogRow {
		return transmitLogRow{who: "p7", action: action, source: source, reason: reason, at: at(ms)}
	}
	const (
		started, stopped, unreported = transmitActionStarted, transmitActionStopped, transmitActionUnreported
		rep, obs                     = transmitSourceReported, transmitSourceObserved
	)
	tests := []struct {
		name         string
		rows         []transmitLogRow
		wantProblems int
		wantOpen     int
	}{
		{"nothing", nil, 0, 0},
		{"a reported pair", []transmitLogRow{row(0, started, rep, ""), row(50, stopped, rep, "")}, 0, 0},
		{"an unreported pair, each close reason", []transmitLogRow{
			row(0, unreported, obs, ""), row(500, stopped, obs, "muted"),
			row(600, unreported, obs, ""), row(900, stopped, obs, "reported"),
			row(1000, unreported, obs, ""), row(1500, stopped, obs, "left"),
		}, 0, 0},
		{"a reported start closed by the poller", []transmitLogRow{
			row(0, started, rep, ""), row(2000, stopped, obs, "no_stop_report"),
			row(3000, started, rep, ""), row(3100, stopped, obs, "left"),
		}, 0, 0},
		{"left closes both kinds with one row", []transmitLogRow{
			row(0, unreported, obs, ""), row(100, started, rep, ""), row(500, stopped, obs, "left"),
		}, 0, 0},
		{"a late report: the two transmissions overlap", []transmitLogRow{
			row(0, unreported, obs, ""), row(700, started, rep, ""), row(1000, stopped, obs, "reported"), row(1500, stopped, rep, ""),
		}, 0, 0},
		{"two principals do not close each other", []transmitLogRow{
			row(0, started, rep, ""),
			{who: "p8", action: stopped, source: rep, at: at(10)},
		}, 1, 1},
		{"a reported start never closed", []transmitLogRow{row(0, started, rep, "")}, 0, 1},
		{"an unreported transmission never closed", []transmitLogRow{row(0, unreported, obs, "")}, 0, 1},
		{"a reported stop with nothing open", []transmitLogRow{row(0, stopped, rep, "")}, 1, 0},
		{"an observed stop with nothing open", []transmitLogRow{row(0, stopped, obs, "muted")}, 1, 0},
		{"left with nothing open", []transmitLogRow{row(0, stopped, obs, "left")}, 1, 0},
		{"muted does not close a reported start", []transmitLogRow{row(0, started, rep, ""), row(500, stopped, obs, "muted")}, 1, 1},
		{"no_stop_report does not close an unreported transmission", []transmitLogRow{row(0, unreported, obs, ""), row(500, stopped, obs, "no_stop_report")}, 1, 1},
		{"a reported stop does not close an unreported transmission", []transmitLogRow{row(0, unreported, obs, ""), row(500, stopped, rep, "")}, 1, 1},
		{"started twice", []transmitLogRow{row(0, started, rep, ""), row(10, started, rep, ""), row(20, stopped, rep, "")}, 1, 0},
		{"unreported twice", []transmitLogRow{row(0, unreported, obs, ""), row(10, unreported, obs, ""), row(20, stopped, obs, "muted")}, 1, 0},
		{"the poller writes a start, as V3 did", []transmitLogRow{row(0, started, obs, ""), row(500, stopped, obs, "muted")}, 2, 1},
		{"an observed stop with no reason, as V3 wrote", []transmitLogRow{row(0, unreported, obs, ""), row(500, stopped, obs, "")}, 1, 1},
		{"a close timed before its opening", []transmitLogRow{row(500, unreported, obs, ""), row(400, stopped, obs, "muted")}, 1, 0},
		{"a row that is not a transmit row", []transmitLogRow{row(0, "voice_joined", obs, "")}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems, open := checkTransmitPairs(tt.rows)
			if len(problems) != tt.wantProblems || len(open) != tt.wantOpen {
				t.Errorf("problems = %q, open = %q; want %d and %d", problems, open, tt.wantProblems, tt.wantOpen)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Scenarios

// ruleT0 is when every scenario starts; step times are milliseconds after it.
var ruleT0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// rulePID is the principal of a one-person scenario.
const rulePID = int64(7)

type ruleStepKind int

const (
	stepPass ruleStepKind = iota
	stepReport
	stepRemoved
	stepDropped
	stepUnread
)

// ruleStep is one input to the rule, at a time in milliseconds after ruleT0.
type ruleStep struct {
	at      int
	kind    ruleStepKind
	mic     micObservation
	started bool
}

// A pass that sees the principal's microphone transmitting, sees them in the
// room and quiet, or does not find them in the room.
func talking(at int) ruleStep { return ruleStep{at: at, kind: stepPass, mic: micTransmitting} }
func quiet(at int) ruleStep   { return ruleStep{at: at, kind: stepPass, mic: micQuiet} }
func absent(at int) ruleStep  { return ruleStep{at: at, kind: stepPass, mic: micAbsent} }

// A pass that learned nothing about the principal (its entitlement could not
// be read).
func unread(at int) ruleStep { return ruleStep{at: at, kind: stepUnread} }

// A report received.
func started(at int) ruleStep { return ruleStep{at: at, kind: stepReport, started: true} }
func stopped(at int) ruleStep { return ruleStep{at: at, kind: stepReport} }

// The principal is removed by name; the room is dropped (a rotation).
func removed(at int) ruleStep { return ruleStep{at: at, kind: stepRemoved} }
func dropped(at int) ruleStep { return ruleStep{at: at, kind: stepDropped} }

// rowText renders a row as "<action>@<ms> <source>[ <reason>]", with the
// action's voice_transmit_ prefix dropped.
func rowText(r transmitRow) string {
	s := fmt.Sprintf("%s@%d %s", strings.TrimPrefix(r.action, "voice_transmit_"), r.at.Sub(ruleT0).Milliseconds(), r.source)
	if r.reason != "" {
		s += " " + r.reason
	}
	return s
}

// runRule feeds steps to a fresh ledger and returns the rows in the order the
// rule produced them. It checks what must hold of any run: inputs are in time
// order, every row is the principal's, a pass writes at most one closing row,
// and a row that is not an unreported opening is timed at its input.
func runRule(t *testing.T, steps []ruleStep) []transmitRow {
	t.Helper()
	var ledger transmitLedger
	return runRuleOn(t, &ledger, steps)
}

func runRuleOn(t *testing.T, ledger *transmitLedger, steps []ruleStep) []transmitRow {
	t.Helper()
	var all []transmitRow
	last := -1
	for i, st := range steps {
		if st.at < last {
			t.Fatalf("step %d is at %d ms, before the step before it (%d ms): a scenario is in time order", i, st.at, last)
		}
		last = st.at
		at := ruleT0.Add(time.Duration(st.at) * time.Millisecond)
		var rows []transmitRow
		switch st.kind {
		case stepPass:
			seen := map[int64]micObservation{}
			if st.mic != micAbsent {
				seen[rulePID] = st.mic
			}
			rows = ledger.pass(at, seen, nil)[rulePID]
		case stepUnread:
			rows = ledger.pass(at, map[int64]micObservation{}, map[int64]struct{}{rulePID: {}})[rulePID]
		case stepReport:
			if row, changed := ledger.report(at, rulePID, st.started); changed {
				rows = []transmitRow{row}
			}
		case stepRemoved:
			rows = ledger.leave(at, rulePID)
		case stepDropped:
			rows = ledger.drop(at)
		}
		closes := 0
		for _, r := range rows {
			if r.principalID != rulePID {
				t.Errorf("step %d: a row for principal %d", i, r.principalID)
			}
			if r.action == transmitActionStopped {
				closes++
			}
			if r.action != transmitActionUnreported && !r.at.Equal(at) {
				t.Errorf("step %d: %s is not timed at its input (%d ms)", i, rowText(r), st.at)
			}
			if r.action == transmitActionUnreported && r.at.After(at) {
				t.Errorf("step %d: %s is timed after the input that produced it (%d ms)", i, rowText(r), st.at)
			}
		}
		if closes > 1 {
			t.Errorf("step %d (at %d ms) wrote %d closing rows, want at most one a pass", i, st.at, closes)
		}
		all = append(all, rows...)
	}
	return all
}

type ruleScenario struct {
	name  string
	steps []ruleStep
	want  []string
}

// runScenarios runs each scenario, compares its rows and checks the pairing.
func runScenarios(t *testing.T, scenarios []ruleScenario) {
	t.Helper()
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			rows := runRule(t, sc.steps)
			got := make([]string, 0, len(rows))
			for _, r := range rows {
				got = append(got, rowText(r))
			}
			if !slices.Equal(got, sc.want) {
				t.Errorf("rows:\n got  %q\n want %q", got, sc.want)
			}
			assertRulePairs(t, rows)
		})
	}
}

// TestTransmitRuleReportsAreTheRecord: while reports and what the poller sees
// agree, the reports are the record and the poller writes nothing (§6, "The
// rule"; "A press shorter than the gap between passes"). A report that does
// not change the state writes nothing.
func TestTransmitRuleReportsAreTheRecord(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"a 50 ms press that no pass falls in", []ruleStep{
			quiet(0), started(100), stopped(150), quiet(500),
		}, []string{"started@100 reported", "stopped@150 reported"}},
		{"a 50 ms press that a pass falls in", []ruleStep{
			quiet(0), started(480), talking(500), stopped(530), quiet(1000),
		}, []string{"started@480 reported", "stopped@530 reported"}},
		{"a long press, seen on every pass", []ruleStep{
			quiet(0), started(100), talking(500), talking(1000), talking(1500), talking(2000), talking(2500), talking(3000), stopped(3200), quiet(3500),
		}, []string{"started@100 reported", "stopped@3200 reported"}},
		{"repeating either report writes nothing more", []ruleStep{
			quiet(0), stopped(50), started(100), started(110), started(120), talking(500), stopped(600), stopped(610), stopped(620), quiet(1000),
		}, []string{"started@100 reported", "stopped@600 reported"}},
		{"nobody in the room and nothing reported", []ruleStep{
			absent(0), absent(500), stopped(600), absent(1000),
		}, nil},
		{"in the room, never unmuted", []ruleStep{
			quiet(0), quiet(500), quiet(1000),
		}, nil},
	})
}

// TestTransmitRuleNeverReports: a participant that never reports (the headless
// participant in e2e/voice, a modified client). Every transmission the poller
// sees, even on a single pass, is recorded as unreported, timed at first sight
// (§6, aim 1).
func TestTransmitRuleNeverReports(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"seen unmuted on many passes, then muted", []ruleStep{
			quiet(0), talking(500), talking(1000), talking(1500), talking(2000), quiet(2500),
		}, []string{"unreported@500 observed", "stopped@2500 observed muted"}},
		{"it opens on the second pass, timed at the first", []ruleStep{
			quiet(0), talking(500), talking(1000),
			// Nothing more is written while it goes on.
			talking(1500), talking(2000), talking(60_000), quiet(60_500),
		}, []string{"unreported@500 observed", "stopped@60500 observed muted"}},
		{"seen unmuted on a single pass, then muted", []ruleStep{
			quiet(0), talking(500), quiet(1000),
		}, []string{"unreported@500 observed", "stopped@1000 observed muted"}},
		{"seen unmuted on a single pass, then gone", []ruleStep{
			quiet(0), talking(500), absent(1000),
		}, []string{"unreported@500 observed", "stopped@1000 observed left"}},
		{"seen unmuted on the first pass that sees them at all", []ruleStep{
			talking(0), quiet(500),
		}, []string{"unreported@0 observed", "stopped@500 observed muted"}},
		{"leaves while transmitting", []ruleStep{
			quiet(0), talking(500), talking(1000), absent(1500),
		}, []string{"unreported@500 observed", "stopped@1500 observed left"}},
		{"three separate bursts are three pairs", []ruleStep{
			talking(0), quiet(500), talking(1000), talking(1500), quiet(2000), talking(2500), absent(3000),
		}, []string{
			"unreported@0 observed", "stopped@500 observed muted",
			"unreported@1000 observed", "stopped@2000 observed muted",
			"unreported@2500 observed", "stopped@3000 observed left",
		}},
		// The stated limit (§6, "Still unrecorded"): a burst between two
		// passes that nobody reports is seen by nobody.
		{"a burst between two passes is not recorded", []ruleStep{
			quiet(0), quiet(500), quiet(1000),
		}, nil},
	})
}

// TestTransmitRuleHonestEdges: a report and a mute travel separately, so an
// honest client can cross a pass at the edge of a press. One pass between the
// report and the mute or unmute, in either order, writes no observed row (§6,
// the first way a lone unaccounted pass is excused; aim 3).
func TestTransmitRuleHonestEdges(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"press: the pass falls after the unmute and before the started report", []ruleStep{
			quiet(0), talking(500), started(520), talking(1000), stopped(1200), quiet(1500),
		}, []string{"started@520 reported", "stopped@1200 reported"}},
		{"press: the pass falls after the started report and before the unmute", []ruleStep{
			quiet(0), started(480), quiet(500), talking(1000), stopped(1200), quiet(1500),
		}, []string{"started@480 reported", "stopped@1200 reported"}},
		{"release: the pass falls after the stopped report and before the mute", []ruleStep{
			quiet(0), started(100), talking(500), stopped(980), talking(1000), quiet(1500),
		}, []string{"started@100 reported", "stopped@980 reported"}},
		{"release: the pass falls after the mute and before the stopped report", []ruleStep{
			quiet(0), started(100), talking(500), quiet(1000), stopped(1020), quiet(1500),
		}, []string{"started@100 reported", "stopped@1020 reported"}},
		{"a whole press inside one gap, its unmute seen by the one pass", []ruleStep{
			// unmute 490, pass, report 510, release reported 700, muted 705.
			quiet(0), talking(500), started(510), stopped(700), quiet(1000),
		}, []string{"started@510 reported", "stopped@700 reported"}},
		{"press and release edges both land on passes, seconds apart", []ruleStep{
			quiet(0), talking(500), started(520), talking(1000), talking(1500), stopped(1980), talking(2000), quiet(2500),
		}, []string{"started@520 reported", "stopped@1980 reported"}},
		{"the release edge is excused though the participant then leaves", []ruleStep{
			quiet(0), started(100), talking(500), stopped(980), talking(1000), absent(1500),
		}, []string{"started@100 reported", "stopped@980 reported"}},
	})
}

// TestTransmitRuleExcuseWindow: "a report near a lone unaccounted pass" means
// one received after the pass before it and before the pass after it. Each
// pair of scenarios differs only in which side of a boundary the report is on.
// The lone unaccounted pass is the one at 1000; its neighbours are at 500 and
// 1500.
func TestTransmitRuleExcuseWindow(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"a report just before the pass before it does not excuse", []ruleStep{
			quiet(0), started(100), stopped(499), quiet(500), talking(1000), quiet(1500),
		}, []string{"started@100 reported", "stopped@499 reported", "unreported@1000 observed", "stopped@1500 observed muted"}},
		{"a report just after the pass before it excuses", []ruleStep{
			quiet(0), started(100), quiet(500), stopped(501), talking(1000), quiet(1500),
		}, []string{"started@100 reported", "stopped@501 reported"}},
		{"a report just before the pass after it excuses", []ruleStep{
			quiet(0), quiet(500), talking(1000), started(1499), quiet(1500), stopped(1600), quiet(2000),
		}, []string{"started@1499 reported", "stopped@1600 reported"}},
		{"a report just after the pass after it does not excuse", []ruleStep{
			quiet(0), quiet(500), talking(1000), quiet(1500), started(1501), stopped(1600), quiet(2000),
		}, []string{"unreported@1000 observed", "stopped@1500 observed muted", "started@1501 reported", "stopped@1600 reported"}},
		{"a report just before the lone pass itself excuses", []ruleStep{
			quiet(0), started(100), talking(500), stopped(999), talking(1000), quiet(1500),
		}, []string{"started@100 reported", "stopped@999 reported"}},
		{"a report just after the lone pass itself excuses", []ruleStep{
			quiet(0), quiet(500), talking(1000), started(1001), talking(1500), stopped(1600), quiet(2000),
		}, []string{"started@1001 reported", "stopped@1600 reported"}},
		{"no report at all does not excuse", []ruleStep{
			quiet(0), quiet(500), talking(1000), quiet(1500),
		}, []string{"unreported@1000 observed", "stopped@1500 observed muted"}},
		// Before and after are the order the inputs arrive in, not a
		// comparison of clock readings: a report applied after a pass was
		// received after it, even within the same millisecond.
		{"a report in the same millisecond as the pass before it, applied after it, excuses", []ruleStep{
			quiet(0), started(100), quiet(500), stopped(500), talking(1000), quiet(1500),
		}, []string{"started@100 reported", "stopped@500 reported"}},
		{"a report in the same millisecond as the pass before it, applied before it, does not", []ruleStep{
			quiet(0), started(100), stopped(500), quiet(500), talking(1000), quiet(1500),
		}, []string{"started@100 reported", "stopped@500 reported", "unreported@1000 observed", "stopped@1500 observed muted"}},
		// A report that changes nothing is evidence of nothing. If it
		// excused, a client that never reports a press could send `stopped`
		// four times a second, at no cost in rows, and have a report near
		// every pass.
		{"a report that does not change the reported state does not excuse", []ruleStep{
			quiet(0), stopped(400), quiet(500), stopped(600), stopped(900), talking(1000), stopped(1100), stopped(1400), quiet(1500),
		}, []string{"unreported@1000 observed", "stopped@1500 observed muted"}},
		// A pass that learned nothing about the principal is no pass for
		// them: the judgement waits for the next one that did.
		{"a pass that could not read the principal does not judge the lone pass", []ruleStep{
			quiet(0), quiet(500), talking(1000), unread(1500), started(1700), talking(2000), stopped(2100), quiet(2500),
		}, []string{"started@1700 reported", "stopped@2100 reported"}},
	})
}

// TestTransmitRuleExcuseNeedsAnEdgeOrTheRation: with a report in the window, a
// lone unaccounted pass is excused the first way when either neighbouring pass
// saw the microphone not transmitting, and otherwise the second way, which is
// rationed: once in 3 s (§6). The lone pass is at 1000 in each.
func TestTransmitRuleExcuseNeedsAnEdgeOrTheRation(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"the pass before saw no transmission, the pass after did", []ruleStep{
			quiet(500), talking(1000), started(1100), talking(1500), stopped(1600), quiet(2000),
		}, []string{"started@1100 reported", "stopped@1600 reported"}},
		{"the pass before saw a transmission, the pass after did not", []ruleStep{
			started(100), talking(500), stopped(900), talking(1000), quiet(1500),
		}, []string{"started@100 reported", "stopped@900 reported"}},
		{"neither neighbour saw a transmission", []ruleStep{
			quiet(500), started(700), stopped(800), talking(1000), quiet(1500),
		}, []string{"started@700 reported", "stopped@800 reported"}},
		{"the pass before did not find the participant in the room", []ruleStep{
			absent(500), talking(1000), started(1100), talking(1500), stopped(1600), quiet(2000),
		}, []string{"started@1100 reported", "stopped@1600 reported"}},
		// The first way is not rationed: each kind of edge twice within 3 s,
		// and nothing is recorded. (Were an edge excused the second way, the
		// second of each pair would be.)
		{"two release edges within 3 s are both excused", []ruleStep{
			started(0), talking(500), stopped(990), talking(1000), quiet(1500),
			started(1600), talking(2000), stopped(2490), talking(2500), quiet(3000),
		}, []string{"started@0 reported", "stopped@990 reported", "started@1600 reported", "stopped@2490 reported"}},
		{"two press edges within 3 s are both excused", []ruleStep{
			quiet(0), talking(500), started(510), talking(1000), stopped(1100), quiet(1500),
			talking(2000), started(2010), talking(2500), stopped(2600), quiet(3000),
		}, []string{"started@510 reported", "stopped@1100 reported", "started@2010 reported", "stopped@2600 reported"}},
		// An honest quick release and re-press that lands on a pass: both
		// neighbours saw a transmission. Excused once.
		{"both neighbours saw a transmission: the second way, once", []ruleStep{
			started(100), talking(500), stopped(990), talking(1000), started(1010), talking(1500), stopped(1700), quiet(2000),
		}, []string{"started@100 reported", "stopped@990 reported", "started@1010 reported", "stopped@1700 reported"}},
		// §6: "An honest client can still be recorded as unreported in one
		// more case: two quick re-presses within 3 s that each land on a
		// pass." The second is a one-pass unreported transmission, closed by
		// the next pass because the client had caught up.
		{"a second such coincidence within 3 s is recorded", []ruleStep{
			started(100), talking(500),
			stopped(990), talking(1000), started(1010), talking(1500),
			talking(2000), talking(2500), talking(3000), talking(3500),
			stopped(3989), talking(3999), started(4009), talking(4500),
			stopped(4700), quiet(5000),
		}, []string{
			"started@100 reported", "stopped@990 reported", "started@1010 reported",
			"stopped@3989 reported", "started@4009 reported",
			"unreported@3999 observed", "stopped@4500 observed reported",
			"stopped@4700 reported",
		}},
		{"a second such coincidence exactly 3 s after the first is excused", []ruleStep{
			started(100), talking(500),
			stopped(990), talking(1000), started(1010), talking(1500),
			talking(2000), talking(2500), talking(3000), talking(3500),
			stopped(3990), talking(4000), started(4010), talking(4500),
			stopped(4700), quiet(5000),
		}, []string{
			"started@100 reported", "stopped@990 reported", "started@1010 reported",
			"stopped@3990 reported", "started@4010 reported",
			"stopped@4700 reported",
		}},
		// Leaving the room and coming back does not hand the ration back: the
		// rule remembers the principal for as long as it remembers the room.
		{"the ration outlasts a reconnect", []ruleStep{
			started(0), talking(500),
			stopped(990), talking(1000), started(1010), talking(1500), // excused at 1000
			stopped(1600), absent(2000), quiet(2100),
			started(2200), talking(2500),
			stopped(2990), talking(3000), started(3010), talking(3500), // 2 s after 1000: recorded
			stopped(3600), quiet(4000),
		}, []string{
			"started@0 reported", "stopped@990 reported", "started@1010 reported", "stopped@1600 reported",
			"started@2200 reported", "stopped@2990 reported", "started@3010 reported",
			"unreported@3000 observed", "stopped@3500 observed reported",
			"stopped@3600 reported",
		}},
		// The ration is spent only by the second way: edges of presses do
		// not use it up, however many there are.
		{"edges do not spend the ration", []ruleStep{
			quiet(0),
			talking(500), started(520), talking(1000), stopped(1480), talking(1500), quiet(2000), // press and release edges
			talking(2500), started(2520), talking(3000), // a press edge
			stopped(3490), talking(3500), started(3510), talking(4000), // a re-press: the second way, first use
			stopped(4200), quiet(4500),
		}, []string{
			"started@520 reported", "stopped@1480 reported", "started@2520 reported",
			"stopped@3490 reported", "started@3510 reported", "stopped@4200 reported",
		}},
		// A pass that was recorded did not spend the ration either: 3 s
		// after the one excused pass, the next is excused again.
		{"the ration counts excused passes, not recorded ones", []ruleStep{
			started(0), talking(500),
			stopped(990), talking(1000), started(1010), talking(1500), // excused at 1000
			stopped(1990), talking(2000), started(2010), talking(2500), // recorded
			stopped(2990), talking(3000), started(3010), talking(3500), // recorded
			stopped(3990), talking(4000), started(4010), talking(4500), // 3 s after 1000: excused
			stopped(4700), quiet(5000),
		}, []string{
			"started@0 reported", "stopped@990 reported", "started@1010 reported",
			"stopped@1990 reported", "started@2010 reported", "unreported@2000 observed", "stopped@2500 observed reported",
			"stopped@2990 reported", "started@3010 reported", "unreported@3000 observed", "stopped@3500 observed reported",
			"stopped@3990 reported", "started@4010 reported",
			"stopped@4700 reported",
		}},
	})
}

// TestTransmitRuleTwoInARow: two unaccounted passes in a row open an
// unreported transmission, timed at the first, and no report excuses that
// (§6; aim 2: "A false `stopped`, with the transmission continuing").
func TestTransmitRuleTwoInARow(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"a false stopped, the transmission continuing", []ruleStep{
			quiet(0), started(100), talking(500), stopped(600),
			talking(1000), // unaccounted
			talking(1500), // the second: opened here, timed at 1000
			talking(2000), talking(2500), quiet(3000),
		}, []string{"started@100 reported", "stopped@600 reported", "unreported@1000 observed", "stopped@3000 observed muted"}},
		{"reports between and around the two passes do not excuse them", []ruleStep{
			quiet(0),
			started(900), stopped(950), talking(1000),
			started(1100), stopped(1150), talking(1500),
			started(1600), stopped(1650), quiet(2000),
		}, []string{
			"started@900 reported", "stopped@950 reported",
			"started@1100 reported", "stopped@1150 reported",
			"unreported@1000 observed",
			"started@1600 reported", "stopped@1650 reported",
			"stopped@2000 observed muted",
		}},
		{"it is closed by the first pass that sees the microphone muted", []ruleStep{
			talking(0), talking(500), talking(1000), quiet(1500), quiet(2000),
		}, []string{"unreported@0 observed", "stopped@1500 observed muted"}},
		{"it is closed by the first pass that finds the participant gone", []ruleStep{
			talking(0), talking(500), absent(1000), absent(1500),
		}, []string{"unreported@0 observed", "stopped@1000 observed left"}},
		{"after it closes, a new one needs two passes or a lone one again", []ruleStep{
			talking(0), talking(500), quiet(1000), talking(1500), talking(2000), quiet(2500),
		}, []string{"unreported@0 observed", "stopped@1000 observed muted", "unreported@1500 observed", "stopped@2500 observed muted"}},
	})
}

// TestTransmitRuleLateStart: a `started` report that arrives after an
// unreported transmission was opened. The report is written; the next pass
// closes the unreported transmission with reason=reported; the press ends with
// its reported `stopped` (§6). This is also what a slow link produces.
func TestTransmitRuleLateStart(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"observed first, reported late", []ruleStep{
			quiet(0), talking(500), talking(1000), started(1200), talking(1500), talking(2000), stopped(2300), quiet(2500),
		}, []string{"unreported@500 observed", "started@1200 reported", "stopped@1500 observed reported", "stopped@2300 reported"}},
		{"reported first, observed after: nothing from the poller", []ruleStep{
			quiet(0), started(100), quiet(500), talking(1000), talking(1500), stopped(1700), quiet(2000),
		}, []string{"started@100 reported", "stopped@1700 reported"}},
		{"a late start and its stop, both before the next pass, account for nothing", []ruleStep{
			quiet(0), talking(500), talking(1000), started(1200), stopped(1300), talking(1500), quiet(2000),
		}, []string{"unreported@500 observed", "started@1200 reported", "stopped@1300 reported", "stopped@2000 observed muted"}},
	})
}

// TestTransmitRuleNoStopReport: not transmitting on every pass for 2 s while
// the reported state is `started`. The stop report was lost or never sent, or
// the report was false. The poller closes it and the reported state is
// `stopped` again, so a `stopped` report arriving afterwards writes nothing
// (§6). The 2 s run from the later of the report and the last pass that saw
// the microphone transmitting.
func TestTransmitRuleNoStopReport(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"a started with no audio", []ruleStep{
			quiet(0), started(100), quiet(500), quiet(1000), quiet(1500), quiet(2000), quiet(2500), stopped(2600), quiet(3000),
		}, []string{"started@100 reported", "stopped@2500 observed no_stop_report"}},
		{"a holder that reports and never connects", []ruleStep{
			started(100), absent(500), absent(1000), absent(1500), absent(2000), absent(2500), stopped(9000), absent(9500),
		}, []string{"started@100 reported", "stopped@2500 observed no_stop_report"}},
		{"a stopped report that never arrives", []ruleStep{
			quiet(0), started(100), talking(500), talking(1000), quiet(1500), quiet(2000), quiet(2500), quiet(3000), quiet(3500),
		}, []string{"started@100 reported", "stopped@3000 observed no_stop_report"}},
		{"not at 1999 ms", []ruleStep{
			started(0), quiet(1999), stopped(2500), quiet(2600),
		}, []string{"started@0 reported", "stopped@2500 reported"}},
		{"at 2000 ms", []ruleStep{
			started(0), quiet(2000), stopped(2500), quiet(2600),
		}, []string{"started@0 reported", "stopped@2000 observed no_stop_report"}},
		{"a pass that sees the microphone transmitting starts the 2 s again", []ruleStep{
			started(0), quiet(500), quiet(1000), talking(1500), quiet(2000), quiet(2500), quiet(3000), quiet(3499), quiet(3500),
		}, []string{"started@0 reported", "stopped@3500 observed no_stop_report"}},
		{"after it, the transmission resuming unreported is recorded", []ruleStep{
			started(0), quiet(500), quiet(2000), talking(2500), talking(3000), quiet(3500),
		}, []string{"started@0 reported", "stopped@2000 observed no_stop_report", "unreported@2500 observed", "stopped@3500 observed muted"}},
		{"a new started after it is a new reported transmission", []ruleStep{
			started(0), quiet(2000), started(2100), talking(2500), stopped(2700), quiet(3000),
		}, []string{"started@0 reported", "stopped@2000 observed no_stop_report", "started@2100 reported", "stopped@2700 reported"}},
	})
}

// TestTransmitRuleLeft: when a participant the poller had seen is gone, is
// removed by name, or the room is rotated, every transmission still open for
// it, reported or unreported, is closed at once with reason=left (§6).
func TestTransmitRuleLeft(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"gone while reported started", []ruleStep{
			quiet(0), started(100), talking(500), absent(1000), stopped(1100), absent(1500),
		}, []string{"started@100 reported", "stopped@1000 observed left"}},
		{"gone while reported started and muted", []ruleStep{
			quiet(0), started(100), quiet(500), absent(1000),
		}, []string{"started@100 reported", "stopped@1000 observed left"}},
		{"removed by name while reported started", []ruleStep{
			quiet(0), started(100), talking(500), removed(700), absent(1000),
		}, []string{"started@100 reported", "stopped@700 observed left"}},
		{"removed by name with an unreported transmission open", []ruleStep{
			talking(0), talking(500), removed(700), absent(1000),
		}, []string{"unreported@0 observed", "stopped@700 observed left"}},
		{"removed by name after one unaccounted pass", []ruleStep{
			quiet(0), talking(500), removed(700),
		}, []string{"unreported@500 observed", "stopped@700 observed left"}},
		{"the room rotated while reported started", []ruleStep{
			quiet(0), started(100), talking(500), dropped(700),
		}, []string{"started@100 reported", "stopped@700 observed left"}},
		{"the room rotated with an unreported transmission open", []ruleStep{
			talking(0), talking(500), talking(1000), dropped(1200),
		}, []string{"unreported@0 observed", "stopped@1200 observed left"}},
		{"the room rotated within 2 s of a started from a holder who never connected", []ruleStep{
			started(100), absent(500), dropped(900),
		}, []string{"started@100 reported", "stopped@900 observed left"}},
		{"the room rotated with nothing open writes nothing", []ruleStep{
			quiet(0), started(100), talking(500), stopped(600), quiet(1000), dropped(1200),
		}, []string{"started@100 reported", "stopped@600 reported"}},
		{"gone, then back and reporting: a new transmission", []ruleStep{
			started(100), talking(500), absent(1000), quiet(1500), started(1600), talking(2000), stopped(2100), quiet(2500),
		}, []string{"started@100 reported", "stopped@1000 observed left", "started@1600 reported", "stopped@2100 reported"}},
		// A holder never seen in the room has not left it: its started is
		// closed by the 2 s rule, not as left.
		{"a holder never seen is not gone", []ruleStep{
			started(100), absent(500), absent(2100),
		}, []string{"started@100 reported", "stopped@2100 observed no_stop_report"}},
	})
}

// TestTransmitRuleOneClosingRow: when more than one reason to close applies on
// a pass, exactly one row is written, with the first of left, muted, reported
// (§6). In each scenario an unreported transmission is open and the client's
// late `started` has arrived, so reason=reported applies on the next pass
// whatever else does.
func TestTransmitRuleOneClosingRow(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"reported alone", []ruleStep{
			talking(0), talking(500), started(700), talking(1000), stopped(1100), quiet(1500),
		}, []string{"unreported@0 observed", "started@700 reported", "stopped@1000 observed reported", "stopped@1100 reported"}},
		{"muted and reported: muted", []ruleStep{
			talking(0), talking(500), started(700), quiet(1000), stopped(1100), quiet(1500),
		}, []string{"unreported@0 observed", "started@700 reported", "stopped@1000 observed muted", "stopped@1100 reported"}},
		// One row closes both the unreported transmission and the reported
		// one: the participant left, and that is why both ended.
		{"left and reported: left, and nothing more", []ruleStep{
			talking(0), talking(500), started(700), absent(1000), stopped(1100), absent(1500),
		}, []string{"unreported@0 observed", "started@700 reported", "stopped@1000 observed left"}},
		{"left, by removal, and reported: left", []ruleStep{
			talking(0), talking(500), started(700), removed(800),
		}, []string{"unreported@0 observed", "started@700 reported", "stopped@800 observed left"}},
		{"left, by rotation, and reported: left", []ruleStep{
			talking(0), talking(500), started(700), dropped(800),
		}, []string{"unreported@0 observed", "started@700 reported", "stopped@800 observed left"}},
		// A lone unaccounted pass that is not excused opens and closes on
		// the pass that judges it: still one closing row.
		{"a lone pass judged as the participant leaves", []ruleStep{
			quiet(0), talking(500), absent(1000),
		}, []string{"unreported@500 observed", "stopped@1000 observed left"}},
		// The 2 s rule can come due on the pass that closes an unreported
		// transmission, if passes were far apart. It waits for the next
		// pass, so that each transmission has its own closing row and a
		// pass writes one.
		{"muted, and the 2 s rule due on the same pass: muted now, no_stop_report on the next", []ruleStep{
			talking(0), talking(500), started(700), quiet(5000), quiet(5500),
		}, []string{"unreported@0 observed", "started@700 reported", "stopped@5000 observed muted", "stopped@5500 observed no_stop_report"}},
	})
}

// TestTransmitRuleManyShortPairs: many short report pairs during one long
// transmission (§6, aim 2). The reported state at a pass is almost always
// `stopped`, so an unreported transmission opens and stays open. The passes
// here are placed by the test.
func TestTransmitRuleManyShortPairs(t *testing.T) {
	// pairs returns a started/stopped pair every period ms, each lasting
	// held ms, from 0 until end, merged in time order with the passes.
	pairs := func(period, held, end int, passes ...ruleStep) []ruleStep {
		var steps []ruleStep
		for at := 0; at < end; at += period {
			steps = append(steps, started(at), stopped(at+held))
		}
		steps = append(steps, passes...)
		slices.SortStableFunc(steps, func(a, b ruleStep) int { return a.at - b.at })
		return steps
	}
	reported := func(period, held, end int) []string {
		var rows []string
		for at := 0; at < end; at += period {
			rows = append(rows, fmt.Sprintf("started@%d reported", at), fmt.Sprintf("stopped@%d reported", at+held))
		}
		return rows
	}
	observedOnly := func(rows []transmitRow) []string {
		var out []string
		for _, r := range rows {
			if r.source == transmitSourceObserved {
				out = append(out, rowText(r))
			}
		}
		return out
	}
	tests := []struct {
		name               string
		period, held, end  int
		passes             []ruleStep
		wantObserved       []string
		wantReportedRowCnt int
	}{
		// A pair every 400 ms, each 50 ms long, unmuted throughout; passes
		// at uneven gaps, none inside a pair. Opened on the second pass,
		// kept open until the mute.
		{"a pair every 400 ms: opened and kept open", 400, 50, 4000,
			[]ruleStep{talking(360), talking(790), talking(1160), talking(1740), talking(2390), talking(2750), talking(3380), talking(3990), quiet(4400)},
			[]string{"unreported@360 observed", "stopped@4400 observed muted"}, 20},
		// The same, 900 ms apart.
		{"a pair every 900 ms: opened and kept open", 900, 50, 4500,
			[]ruleStep{talking(360), talking(790), talking(1160), talking(1740), talking(2390), talking(2760), talking(3380), talking(3990), quiet(4500)},
			[]string{"unreported@360 observed", "stopped@4500 observed muted"}, 10},
		// One pass does land inside a pair (at 1210, in the pair at
		// 1200). The client is then, at that instant, reporting truthfully:
		// the unreported transmission closes with reason=reported and the
		// next two unaccounted passes open another. Nothing is hidden.
		{"a pass that lands inside a pair closes it, and it opens again", 400, 50, 3000,
			[]ruleStep{talking(360), talking(790), talking(1210), talking(1740), talking(2390), quiet(3100)},
			[]string{"unreported@360 observed", "stopped@1210 observed reported", "unreported@1740 observed", "stopped@3100 observed muted"}, 16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := runRule(t, pairs(tt.period, tt.held, tt.end, tt.passes...))
			if got := observedOnly(rows); !slices.Equal(got, tt.wantObserved) {
				t.Errorf("the poller's rows:\n got  %q\n want %q", got, tt.wantObserved)
			}
			var got []string
			for _, r := range rows {
				if r.source == transmitSourceReported {
					got = append(got, rowText(r))
				}
			}
			if want := reported(tt.period, tt.held, tt.end); !slices.Equal(got, want) || len(got) != tt.wantReportedRowCnt {
				t.Errorf("the reported rows:\n got  %q\n want %q (%d rows)", got, want, tt.wantReportedRowCnt)
			}
			assertRulePairs(t, rows)
		})
	}
}

// TestTransmitRuleStraddledPasses: a client that knew when the passes were
// (it cannot: the gaps are random; the test can) and reported `stopped` just
// before a pass and `started` just after it, while unmuted throughout. It is
// excused at most once in 3 s and recorded for every other unaccounted pass
// (§6, aim 2).
func TestTransmitRuleStraddledPasses(t *testing.T) {
	// straddle wraps a pass in a stopped 5 ms before it and a started 5 ms
	// after it.
	straddle := func(at int) []ruleStep { return []ruleStep{stopped(at - 5), talking(at), started(at + 5)} }
	join := func(parts ...[]ruleStep) []ruleStep { return slices.Concat(parts...) }
	one := func(s ruleStep) []ruleStep { return []ruleStep{s} }

	t.Run("every pass straddled: two in a row, opened and kept open", func(t *testing.T) {
		steps := join(one(started(0)), straddle(500), straddle(1000), straddle(1500), straddle(2000), straddle(2500), straddle(3000),
			[]ruleStep{stopped(3400), quiet(3500)})
		rows := runRule(t, steps)
		var observed []string
		for _, r := range rows {
			if r.source == transmitSourceObserved {
				observed = append(observed, rowText(r))
			}
		}
		if want := []string{"unreported@500 observed", "stopped@3500 observed muted"}; !slices.Equal(observed, want) {
			t.Errorf("the poller's rows = %q, want %q", observed, want)
		}
		assertRulePairs(t, rows)
	})

	t.Run("every other pass straddled: excused once in 3 s, recorded otherwise", func(t *testing.T) {
		// Passes every 500 ms from 500 to 8000; the ones at 1000, 2000,
		// ... 7000 are straddled, the ones between find `started`.
		steps := one(started(0))
		for at := 500; at <= 8000; at += 500 {
			if at%1000 == 0 && at <= 7000 {
				steps = append(steps, straddle(at)...)
			} else {
				steps = append(steps, talking(at))
			}
		}
		steps = append(steps, stopped(8100), quiet(8500))
		rows := runRule(t, steps)
		var observed []string
		for _, r := range rows {
			if r.source == transmitSourceObserved {
				observed = append(observed, rowText(r))
			}
		}
		// Excused: 1000 (the first), 4000 (3 s later), 7000 (3 s after
		// that). Recorded: 2000, 3000, 5000, 6000, each closed by the next
		// pass, which finds `started`.
		want := []string{
			"unreported@2000 observed", "stopped@2500 observed reported",
			"unreported@3000 observed", "stopped@3500 observed reported",
			"unreported@5000 observed", "stopped@5500 observed reported",
			"unreported@6000 observed", "stopped@6500 observed reported",
		}
		if !slices.Equal(observed, want) {
			t.Errorf("the poller's rows:\n got  %q\n want %q", observed, want)
		}
		assertRulePairs(t, rows)
	})
}

// TestTransmitRuleRestartMidPress: conchd restarts during a press. Reported
// state is in memory and is lost, so it reads `stopped`: the poller opens an
// unreported transmission and closes it when the microphone is muted, and the
// client's `stopped` report changes nothing (§6). A fresh ledger is the
// restart; the `started` row written before it is the one row with no closing
// row of its own, and is not in this ledger's rows at all.
func TestTransmitRuleRestartMidPress(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"already transmitting on the first pass after the restart", []ruleStep{
			talking(0), talking(500), talking(1000), stopped(1200), quiet(1500),
		}, []string{"unreported@0 observed", "stopped@1500 observed muted"}},
		{"released between the restart's first two passes", []ruleStep{
			talking(0), stopped(200), quiet(500),
		}, []string{"unreported@0 observed", "stopped@500 observed muted"}},
	})
}

// TestTransmitRuleStaleReports: the two cases §6 lists where a report that is
// not the client's latest changes the state (a displaced device's last report;
// a report delivered late and out of order). A false `started` is closed after
// 2 s; a false `stopped` shows the press as unreported until its next report.
// Neither can hide a transmission.
func TestTransmitRuleStaleReports(t *testing.T) {
	runScenarios(t, []ruleScenario{
		{"a stale started while nothing is transmitted", []ruleStep{
			quiet(0), started(100), quiet(500), quiet(1000), quiet(1500), quiet(2000), quiet(2500),
		}, []string{"started@100 reported", "stopped@2500 observed no_stop_report"}},
		{"a stale stopped during the new device's press", []ruleStep{
			quiet(0), started(100), talking(500), stopped(600) /* stale */, talking(1000), talking(1500), talking(2000),
			stopped(2200), quiet(2500),
		}, []string{"started@100 reported", "stopped@600 reported", "unreported@1000 observed", "stopped@2500 observed muted"}},
		{"stopped and started delivered in the wrong order", []ruleStep{
			quiet(0), started(100), talking(500), started(700) /* the re-press, early */, stopped(710), /* the release, late */
			talking(1000), talking(1500), stopped(1700), quiet(2000),
		}, []string{"started@100 reported", "stopped@710 reported", "unreported@1000 observed", "stopped@2000 observed muted"}},
	})
}

// ---------------------------------------------------------------------------
// The room's ledger

// TestTransmitLedgerPrincipalsAreSeparate: the rule is per principal. One
// principal's report accounts for nobody else's microphone, and rows come out
// grouped by principal.
func TestTransmitLedgerPrincipalsAreSeparate(t *testing.T) {
	at := func(ms int) time.Time { return ruleT0.Add(time.Duration(ms) * time.Millisecond) }
	var ledger transmitLedger
	var all []transmitRow
	pass := func(ms int, seen map[int64]micObservation) {
		rows := ledger.pass(at(ms), seen, nil)
		pids := make([]int64, 0, len(rows))
		for pid := range rows {
			pids = append(pids, pid)
		}
		slices.Sort(pids)
		for _, pid := range pids {
			for _, r := range rows[pid] {
				if r.principalID != pid {
					t.Errorf("a row for %d filed under %d", r.principalID, pid)
				}
			}
			all = append(all, rows[pid]...)
		}
	}
	report := func(ms int, pid int64, started bool) {
		if row, changed := ledger.report(at(ms), pid, started); changed {
			all = append(all, row)
		}
	}
	// 7 reports honestly; 8 transmits and never reports; 9 reports and is
	// never in the room.
	pass(0, map[int64]micObservation{7: micQuiet, 8: micQuiet})
	report(100, 7, true)
	report(150, 9, true)
	pass(500, map[int64]micObservation{7: micTransmitting, 8: micTransmitting})
	pass(1000, map[int64]micObservation{7: micTransmitting, 8: micTransmitting})
	report(1200, 7, false)
	pass(1500, map[int64]micObservation{7: micQuiet, 8: micTransmitting})
	if !ledger.unsettled() {
		t.Error("the ledger is settled with 8's transmission open and 9's started unclosed")
	}
	pass(2000, map[int64]micObservation{7: micQuiet})
	pass(2500, map[int64]micObservation{7: micQuiet})
	if ledger.unsettled() {
		t.Error("the ledger is unsettled though everything has been closed")
	}

	var got []string
	for _, r := range all {
		got = append(got, fmt.Sprintf("%d %s", r.principalID, rowText(r)))
	}
	want := []string{
		"7 started@100 reported", "9 started@150 reported",
		"8 unreported@500 observed",
		"7 stopped@1200 reported",
		"8 stopped@2000 observed left",
		"9 stopped@2500 observed no_stop_report",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows:\n got  %q\n want %q", got, want)
	}
	assertRulePairs(t, all)

	// Dropping the room closes everyone's, in order of principal id.
	report(3000, 9, true)
	report(3000, 7, true)
	rows := ledger.drop(at(3100))
	if len(rows) != 2 || rows[0].principalID != 7 || rows[1].principalID != 9 || rows[0].reason != transmitReasonLeft || rows[1].reason != transmitReasonLeft {
		t.Errorf("drop rows = %+v, want reason=left for 7 then 9", rows)
	}
	if ledger.unsettled() || len(ledger.tracks) != 0 {
		t.Error("a dropped ledger still holds state")
	}
}

// TestTransmitLedgerRetract: a report whose row could not be written is taken
// back, unless something has changed the reported state since.
func TestTransmitLedgerRetract(t *testing.T) {
	at := func(ms int) time.Time { return ruleT0.Add(time.Duration(ms) * time.Millisecond) }
	t.Run("a started is taken back: the transmission is then unreported", func(t *testing.T) {
		var ledger transmitLedger
		if _, changed := ledger.report(at(0), rulePID, true); !changed {
			t.Fatal("the report changed nothing")
		}
		ledger.retract(rulePID, true)
		rows := runRuleOn(t, &ledger, []ruleStep{talking(500), talking(1000), quiet(1500)})
		if len(rows) != 2 || rows[0].action != transmitActionUnreported || rows[1].reason != transmitReasonMuted {
			t.Errorf("rows = %+v, want an unreported transmission", rows)
		}
		assertRulePairs(t, rows)
	})
	t.Run("a stopped is taken back: the reported transmission is still open", func(t *testing.T) {
		var ledger transmitLedger
		first, _ := ledger.report(at(0), rulePID, true)
		ledger.report(at(100), rulePID, false)
		ledger.retract(rulePID, false)
		row, changed := ledger.report(at(200), rulePID, false)
		if !changed {
			t.Fatal("the retried stopped changed nothing: the retraction did not restore the reported state")
		}
		assertRulePairs(t, []transmitRow{first, row})
	})
	t.Run("not if the state has changed since", func(t *testing.T) {
		var ledger transmitLedger
		ledger.report(at(0), rulePID, true)
		ledger.report(at(100), rulePID, false)
		ledger.retract(rulePID, true) // the started's write failed late
		if _, changed := ledger.report(at(200), rulePID, false); changed {
			t.Error("retracting a started after a stopped made the state started again")
		}
	})
	t.Run("an unknown principal is nothing to retract", func(t *testing.T) {
		var ledger transmitLedger
		ledger.retract(99, true)
		if len(ledger.tracks) != 0 {
			t.Error("retract made a track")
		}
	})
}

// TestTransmitRuleAlwaysPairs runs the rule over many random sequences of
// passes and reports and checks aim 4 on each: whatever a client does, every
// row that opens a transmission is followed by one that closes it. Left to
// itself (quiet passes, no reports) for a little over 2 s, the rule settles
// everything without the room being dropped. The sequences come from fixed
// seeds, so a failure is reproducible.
func TestTransmitRuleAlwaysPairs(t *testing.T) {
	for seed := int64(1); seed <= 400; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var steps []ruleStep
		at := 0
		for range 60 + rng.Intn(60) {
			at += rng.Intn(700)
			switch rng.Intn(12) {
			case 0, 1, 2:
				steps = append(steps, talking(at))
			case 3, 4:
				steps = append(steps, quiet(at))
			case 5:
				steps = append(steps, absent(at))
			case 6, 7, 8:
				steps = append(steps, started(at))
			case 9, 10:
				steps = append(steps, stopped(at))
			case 11:
				switch rng.Intn(3) {
				case 0:
					steps = append(steps, removed(at))
				case 1:
					steps = append(steps, unread(at))
				default:
					steps = append(steps, talking(at))
					at += rng.Intn(600)
					steps = append(steps, talking(at))
				}
			}
		}
		// Settle: nothing more is reported and the microphone stays quiet.
		for i := 1; i <= 6; i++ {
			steps = append(steps, quiet(at+1300+i*500))
		}
		var ledger transmitLedger
		rows := runRuleOn(t, &ledger, steps)
		problems, open := checkTransmitPairs(logRowsFromRule(rows))
		if len(problems) > 0 || len(open) > 0 || ledger.unsettled() {
			t.Fatalf("seed %d: problems %q, open %q, unsettled %v\nsteps: %+v", seed, problems, open, ledger.unsettled(), steps)
		}
	}
}

// ---------------------------------------------------------------------------
// What the rule is made of

// TestTransmitRuleIsPure: the rule's file imports nothing it could take a
// lock with, do I/O with, or read a clock other than through the times it is
// handed. (time is imported for the type; the file calls neither time.Now nor
// time.Since, which the second check covers.)
func TestTransmitRuleIsPure(t *testing.T) {
	const name = "voice_transmit_rule.go"
	if got, want := sourceImports(t, name), []string{"slices", "time"}; !slices.Equal(got, want) {
		t.Errorf("%s imports %q, want only %q", name, got, want)
	}
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"time.Now", "time.Since", "time.Until", "time.After(", "time.Sleep", "time.NewTimer", "time.Tick"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("%s uses %s: the rule reads no clock", name, banned)
		}
	}
}

// sourceImports returns the import paths of one of this package's source
// files, sorted.
func sourceImports(t *testing.T, name string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var imports []string
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		imports = append(imports, path)
	}
	slices.Sort(imports)
	return imports
}

// TestTransmitRuleConstants: the rule's file names its audit actions as plain
// strings so that it imports nothing of the server. They must be the store's.
func TestTransmitRuleConstants(t *testing.T) {
	for got, want := range map[string]string{
		transmitActionStarted:    store.AuditVoiceTransmitStarted,
		transmitActionStopped:    store.AuditVoiceTransmitStopped,
		transmitActionUnreported: store.AuditVoiceTransmitUnreported,
	} {
		if got != want {
			t.Errorf("rule action %q, store action %q", got, want)
		}
	}
	if voiceNoStopReportAfter != 2*time.Second || voiceExcuseRation != 3*time.Second {
		t.Errorf("the 2 s close is %v and the 3 s ration is %v (docs/design/conch-voice.md §6)", voiceNoStopReportAfter, voiceExcuseRation)
	}
}
