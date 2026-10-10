package server

import (
	"slices"
	"time"
)

// The transmit rule (issue #135, docs/design/conch-voice.md §6 "The rule").
//
// A voice client reports each press and release; the poller sees each
// microphone about twice a second. This file is the comparison of the two, and
// nothing else: it decides which audit rows follow from a sequence of passes
// and reports. It holds no lock, does no I/O and reads no clock. Every input
// carries its time, and the caller feeds inputs in the order they happened
// (voicePoller does so under its mutex, see voice_transmit.go). That makes the
// rule a function of its inputs, which is what its tests are tables of.
//
// "Before" and "after" in the rule mean the order in which inputs are fed, not
// a comparison of their times: a report fed between two passes was received
// after the first and before the second, whatever the clock's resolution. The
// times are used for what the rows say and for the two durations below.

const (
	// voiceNoStopReportAfter is how long a reported `started` may go without
	// a pass seeing the microphone transmit before the poller closes it (§6:
	// "Not transmitting on every pass for 2 s").
	voiceNoStopReportAfter = 2 * time.Second
	// voiceExcuseRation is how long after a lone unaccounted pass was excused
	// the second way no other one is (§6: "no other pass was excused this
	// second way in the previous 3 s").
	voiceExcuseRation = 3 * time.Second
)

// Where a transmit row came from, and why the poller closed a transmission.
// They are the values of source= and reason= in the audit detail.
const (
	transmitSourceReported = "reported"
	transmitSourceObserved = "observed"

	// transmitReasonLeft: the participant was gone, was removed by name, or
	// the room was rotated. It comes first when several reasons apply.
	transmitReasonLeft = "left"
	// transmitReasonMuted: a pass saw the microphone not transmitting.
	transmitReasonMuted = "muted"
	// transmitReasonReported: the client caught up; a pass found the
	// reported state `started`.
	transmitReasonReported = "reported"
	// transmitReasonNoStopReport: reported `started`, and no pass saw the
	// microphone transmit for voiceNoStopReportAfter.
	transmitReasonNoStopReport = "no_stop_report"
)

// Audit actions of the rows, as plain strings so that this file imports
// nothing of the server. A test checks them against the store's constants.
const (
	transmitActionStarted    = "voice_transmit_started"
	transmitActionStopped    = "voice_transmit_stopped"
	transmitActionUnreported = "voice_transmit_unreported"
)

// micObservation is what one pass saw of one principal.
type micObservation int

const (
	// micAbsent: the principal is not in the room. For someone the previous
	// pass saw, that is leaving; for a holder never seen there it counts as
	// not transmitting (§6).
	micAbsent micObservation = iota
	// micQuiet: in the room, microphone muted or not published.
	micQuiet
	// micTransmitting: in the room, microphone published and unmuted.
	micTransmitting
)

// transmitRow is one audit row the rule asks for.
type transmitRow struct {
	principalID int64
	action      string
	// at is the time the row carries. For voice_transmit_unreported it is
	// the first unaccounted pass, which is earlier than the input that
	// produced the row.
	at     time.Time
	source string
	// reason is set on rows the poller writes to close a transmission.
	reason string
}

// detail is the row's part of the audit detail: where it came from and, for a
// close by the poller, why.
func (r transmitRow) detail() string {
	if r.reason == "" {
		return "source=" + r.source
	}
	return "source=" + r.source + " reason=" + r.reason
}

// transmitTrack is the rule's memory of one principal in one room.
//
// Two kinds of transmission can be open, and each is opened by one row and
// closed by a later one:
//
//   - a reported one: opened by the client's `started` (source=reported),
//     closed by its `stopped`, or by the poller with reason=no_stop_report or
//     reason=left;
//   - an unreported one: opened by voice_transmit_unreported, closed by the
//     poller with reason=muted, reason=reported or reason=left.
//
// A pass writes at most one closing row for a principal. When someone leaves
// with both kinds open, the one reason=left row closes both.
type transmitTrack struct {
	// reported is the reported state: true is `started`. It is `stopped`
	// until told otherwise.
	reported bool
	// change names the last change of the reported state, whatever made it: a
	// report, a close by the poller, a retraction. The ledger numbers every
	// change, so two never share one (transmitLedger.changes). It is how
	// retract tells the report it is undoing from any that came after.
	change uint64
	// quietFrom is the later of the `started` report and the last pass that
	// saw the microphone transmitting: the start of the time the 2 s rule
	// measures. Meaningful only while reported.
	quietFrom time.Time
	// present is whether the previous pass saw the principal in the room.
	present bool
	// prevTransmitting is whether the previous pass saw the microphone
	// transmitting. A principal never seen counts as not transmitting.
	prevTransmitting bool
	// changedSincePass is whether a report changed the reported state since
	// the previous pass.
	changedSincePass bool
	// open is whether an unreported transmission is open.
	open bool

	// pending is set when the previous pass was unaccounted and nothing was
	// open: it may be the first of two in a row or a lone one, and which is
	// known only at the next pass (§6: "Judging a lone pass therefore waits
	// for the next one").
	pending bool
	// pendingAt is the time of that pass.
	pendingAt time.Time
	// pendingPrevTransmitting is whether the pass before it saw the
	// microphone transmitting.
	pendingPrevTransmitting bool
	// pendingReportBefore is whether a report changed the reported state
	// between the pass before it and it.
	pendingReportBefore bool

	// rationed and lastExcused record the last lone pass excused the second
	// way, for the 3 s ration.
	rationed    bool
	lastExcused time.Time
}

// report applies one report received at `at`. A report that does not change
// the reported state writes nothing and is no evidence of anything: only a
// report that changed the state can excuse a lone unaccounted pass. Otherwise
// a client that never reports a transmission could send `stopped` over and
// over, at no cost in audit rows, and have a report "near" every pass.
func (k *transmitTrack) report(pid int64, at time.Time, started bool) (transmitRow, bool) {
	if k.reported == started {
		return transmitRow{}, false
	}
	k.reported = started
	k.changedSincePass = true
	action := transmitActionStopped
	if started {
		action = transmitActionStarted
		k.quietFrom = at
	}
	return transmitRow{principalID: pid, action: action, at: at, source: transmitSourceReported}, true
}

// undo puts the reported state back to what it was before the last report,
// for a report whose audit row could not be written: a reported state with no
// row behind it would let the poller treat a transmission as accounted for
// when the log does not have it. A report that was not recorded excuses
// nothing either, so the note that a report changed the state since the last
// pass goes too. That can also forget an earlier report in the same gap, one
// that was recorded; it errs toward recording, and only when the store is
// failing. Whether the last change is the report to undo is the ledger's to
// decide (transmitLedger.retract).
func (k *transmitTrack) undo() {
	k.reported = !k.reported
	k.changedSincePass = false
}

// observe applies one pass at `at`. left forces the leaving of a principal
// whatever the previous pass saw: the room is being dropped, or the principal
// was removed by name. The microphone is micAbsent whenever left is true.
func (k *transmitTrack) observe(pid int64, at time.Time, mic micObservation, left bool) []transmitRow {
	transmitting := mic == micTransmitting
	// Gone: someone the poller had seen is no longer there.
	gone := left || (mic == micAbsent && k.present)
	// §6: "An unaccounted pass sees the microphone unmuted while the
	// reported state is `stopped` at that instant."
	unaccounted := transmitting && !k.reported

	var rows []transmitRow
	switch {
	case k.pending:
		k.pending = false
		// §6: two unaccounted passes in a row open an unreported
		// transmission, timed at the first, and no report excuses that. One
		// alone is also an unreported transmission, one that began and
		// ended, unless it is excused. Either way it opens here, at the
		// time of the earlier pass; a lone one is closed just below, by
		// this pass.
		if unaccounted || !k.excused(transmitting) {
			rows = append(rows, transmitRow{principalID: pid, action: transmitActionUnreported, at: k.pendingAt, source: transmitSourceObserved})
			k.open = true
		}
	case unaccounted && !k.open:
		k.pending = true
		k.pendingAt = at
		k.pendingPrevTransmitting = k.prevTransmitting
		k.pendingReportBefore = k.changedSincePass
	}

	// §6: an unreported transmission is closed by the first later pass that
	// sees the microphone muted, the participant gone, or the reported state
	// `started`: that is, by the first pass that is not unaccounted. One row,
	// with the first reason that applies of left, muted, reported.
	closed := false
	if k.open && !unaccounted {
		reason := transmitReasonReported
		switch {
		case gone:
			reason = transmitReasonLeft
		case !transmitting:
			reason = transmitReasonMuted
		}
		rows = append(rows, transmitRow{principalID: pid, action: transmitActionStopped, at: at, source: transmitSourceObserved, reason: reason})
		k.open = false
		closed = true
	}

	if k.reported {
		switch {
		case gone:
			// §6: when a participant the poller had seen is gone, or the
			// room is rotated, every transmission still open is closed at
			// once. If the row above already says reason=left, it is the
			// one row for this pass and closes this too.
			if !closed {
				rows = append(rows, transmitRow{principalID: pid, action: transmitActionStopped, at: at, source: transmitSourceObserved, reason: transmitReasonLeft})
			}
			k.reported = false
		case transmitting:
			k.quietFrom = at
		case !closed && at.Sub(k.quietFrom) >= voiceNoStopReportAfter:
			// §6: not transmitting on every pass for 2 s while reported
			// `started`. The stop report was lost or never sent, or the
			// report was false. A holder never seen in the room is here
			// too: it counts as not transmitting. Not on a pass that
			// already closed an unreported transmission (one closing row a
			// pass); the next pass does it.
			rows = append(rows, transmitRow{principalID: pid, action: transmitActionStopped, at: at, source: transmitSourceObserved, reason: transmitReasonNoStopReport})
			k.reported = false
		}
	}

	// Someone who has left is absent and was not transmitting, as far as the
	// next pass is concerned: the first pass of a new connection is at the
	// edge of a transmission whatever the old connection was doing.
	k.present = mic != micAbsent
	k.prevTransmitting = transmitting
	k.changedSincePass = false
	return rows
}

// excused judges the lone unaccounted pass that is pending, given what the
// pass after it saw. §6: it is excused when a report was received after the
// pass before it and before the pass after it, and either one of those two
// passes saw the microphone not transmitting (it is at the edge of a
// transmission), or no other pass was excused this second way in the previous
// 3 s (an honest quick re-press can land on a pass; it cannot keep doing so).
func (k *transmitTrack) excused(nextTransmitting bool) bool {
	if !k.pendingReportBefore && !k.changedSincePass {
		return false
	}
	if !k.pendingPrevTransmitting || !nextTransmitting {
		return true
	}
	if k.rationed && k.pendingAt.Sub(k.lastExcused) < voiceExcuseRation {
		return false
	}
	k.rationed, k.lastExcused = true, k.pendingAt
	return true
}

// unsettled reports whether the track holds something a later pass must
// settle: a reported `started`, an open unreported transmission, or an
// unaccounted pass not yet judged.
//
// In the poller the last of these never decides anything by itself: a pass is
// pending only for someone the previous pass saw transmitting, and a room with
// anyone in it is polled anyway. It is here so that the ledger's answer is
// right without leaning on that.
func (k *transmitTrack) unsettled() bool { return k.reported || k.open || k.pending }

// transmitLedger is the rule for one room: a track per principal who has been
// seen there or has reported. It is part of the poller's state for the room
// and is dropped with it, after drop has closed what is open.
//
// A track is kept for as long as the ledger is, also after its principal has
// left the room: it is a few words, there are at most as many as the channel
// has had members, and dropping it would hand a client its 3 s ration back
// for the price of a reconnect.
//
// The zero value is ready to use.
type transmitLedger struct {
	tracks map[int64]*transmitTrack
	// changes counts the changes of anyone's reported state. Each change
	// takes the next number (transmitTrack.change), and the count outlives a
	// drop, so no two changes in the life of a ledger share a number.
	changes uint64
}

// observe is transmitTrack.observe, numbering the change if the pass changed
// the reported state (a close for no_stop_report or for leaving).
func (l *transmitLedger) observe(k *transmitTrack, pid int64, at time.Time, mic micObservation, left bool) []transmitRow {
	was := k.reported
	rows := k.observe(pid, at, mic, left)
	if k.reported != was {
		l.changes++
		k.change = l.changes
	}
	return rows
}

func (l *transmitLedger) track(pid int64) *transmitTrack {
	if l.tracks == nil {
		l.tracks = make(map[int64]*transmitTrack)
	}
	k := l.tracks[pid]
	if k == nil {
		k = &transmitTrack{}
		l.tracks[pid] = k
	}
	return k
}

// report applies a report from pid received at `at`. It returns the row to
// write, the number of the change it made, and true when it changed the
// reported state. The number is what retract takes.
func (l *transmitLedger) report(at time.Time, pid int64, started bool) (row transmitRow, change uint64, changed bool) {
	k := l.track(pid)
	row, changed = k.report(pid, at, started)
	if changed {
		l.changes++
		k.change = l.changes
	}
	return row, k.change, changed
}

// retract undoes the report from pid that made the given change, whose row
// could not be written, provided that change is still the last one to pid's
// reported state. If anything has changed the state since (a later report,
// which has its own row; a close by the poller), there is nothing of this
// report left to undo, and undoing would take back the later change instead:
// with `started` reported, a `stopped` whose write hangs, then a `started` and
// a `stopped` that are both recorded, the first one failing late must not
// leave the state `started`.
func (l *transmitLedger) retract(pid int64, change uint64) {
	k := l.tracks[pid]
	if k == nil || change == 0 || k.change != change {
		return
	}
	k.undo()
	l.changes++
	k.change = l.changes
}

// pass applies one pass over the room at `at`. seen holds what the pass saw of
// everyone it found in the room (micQuiet or micTransmitting). Every other
// principal the ledger knows is absent on this pass, except those in skip,
// about whom the pass learned nothing: they get no input at all. The rows are
// grouped by principal.
func (l *transmitLedger) pass(at time.Time, seen map[int64]micObservation, skip map[int64]struct{}) map[int64][]transmitRow {
	pids := make([]int64, 0, len(seen)+len(l.tracks))
	for pid := range seen {
		pids = append(pids, pid)
	}
	for pid := range l.tracks {
		if _, ok := seen[pid]; !ok {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	var out map[int64][]transmitRow
	for _, pid := range pids {
		if _, ok := skip[pid]; ok {
			continue
		}
		mic, ok := seen[pid]
		if !ok {
			mic = micAbsent
		}
		if rows := l.observe(l.track(pid), pid, at, mic, false); len(rows) > 0 {
			if out == nil {
				out = make(map[int64][]transmitRow)
			}
			out[pid] = rows
		}
	}
	return out
}

// leave closes at `at` whatever is open for pid, with reason=left: pid was
// removed by name, or its connection was replaced by another.
func (l *transmitLedger) leave(at time.Time, pid int64) []transmitRow {
	k := l.tracks[pid]
	if k == nil {
		return nil
	}
	return l.observe(k, pid, at, micAbsent, true)
}

// drop closes at `at` whatever is open for anyone, with reason=left, and
// forgets everything: the room is being dropped. That includes a reported
// `started` from a holder who was never seen in the room. Rows are in order of
// principal id.
func (l *transmitLedger) drop(at time.Time) []transmitRow {
	pids := make([]int64, 0, len(l.tracks))
	for pid := range l.tracks {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	var rows []transmitRow
	for _, pid := range pids {
		rows = append(rows, l.observe(l.tracks[pid], pid, at, micAbsent, true)...)
	}
	l.tracks = nil
	return rows
}

// unsettled reports whether any track holds something a later pass must
// settle. A room for which it is true is polled.
func (l *transmitLedger) unsettled() bool {
	for _, k := range l.tracks {
		if k.unsettled() {
			return true
		}
	}
	return false
}
