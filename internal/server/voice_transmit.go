package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Transmit reports (issue #135, docs/design/conch-voice.md §6).
//
// The poller sees each microphone about twice a second and can miss a press
// shorter than that, so the voice client tells conchd about every press and
// release, and the poller checks the client. This file is the endpoint the
// client reports to, the bound on it, and the one place a report is applied
// to the poller's state. The comparison of reports with what the poller sees
// is the transmit rule, in voice_transmit_rule.go; the passes that feed it are
// in voice_presence.go.
//
// The endpoint never calls LiveKit. Nothing here puts a token, the API secret
// or a room name in a log line, an audit row or an error.

const (
	// voiceReportMaxBytes bounds a report's body. A channel-wide report is
	// about twenty bytes; the largest the schema allows (an audience of 64
	// principals) is under two kilobytes.
	voiceReportMaxBytes = 4 << 10

	// voiceReportBurst and voiceReportPerSecond are the bound on reports per
	// principal: a bucket of voiceReportBurst reports, refilled at
	// voiceReportPerSecond a second. A press and its release are two reports,
	// so this allows two presses a second for as long as anyone likes and
	// fifteen at once on top of that. Speech is far below it: nobody keys a
	// microphone twice a second for long, and a burst of key mashing at five
	// presses a second runs for five seconds before it is slowed. What it
	// stops is the audit log being used as a sink: whatever a client sends,
	// its reports add at most voiceReportPerSecond rows a second once the
	// burst is spent.
	voiceReportBurst     = 30
	voiceReportPerSecond = 4

	// voiceReportRefusalWindow is how often refusals are audited: the first
	// refusal of a principal writes one voice_report_rate_limited row, and
	// further refusals write nothing until this long after it. The next row
	// says how many were refused in between. A minute keeps a sustained flood
	// to one row a minute while still showing, to the minute, when it ran.
	voiceReportRefusalWindow = time.Minute
)

// handleVoiceTransmit serves POST /v1/channels/{channel}/voice/transmit: one
// press or release, reported by the client that made it.
//
// The caller is checked exactly as for a session, by the same code and so in
// the same order (voiceSessionCaller): a non-member gets the unknown-channel
// 404 whatever it sends, and learns nothing about voice. Then the bound, then
// the body, then whether the caller holds a session for the room. The body is
// read only for a caller who may report at all.
//
// The report carries no time. conchd stamps the instant it applies it, so a
// client can neither back-date nor post-date its own record.
func (s *Server) handleVoiceTransmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, caller, ok := s.voiceSessionCaller(w, r)
	if !ok {
		return
	}

	// Every request from here on counts against the bound, whatever becomes
	// of it: a malformed body and a caller with no session cost a lookup
	// each, and neither may be used to hammer the server either.
	allowed, audit, suppressed, at := s.voice.reports.take(caller.ID, s.voice.now)
	if !allowed {
		if audit {
			slog.WarnContext(ctx, "voice: transmit reports over the bound are being refused", "principal", caller.ID, "channel", channel.ID)
			s.voice.writeEvents(ctx, voiceEvent{
				actor: fmt.Sprintf("principal:%d", caller.ID), action: store.AuditVoiceReportRateLimited, channelID: channel.ID,
				detail: fmt.Sprintf("channel=%d burst=%d per_second=%d window_seconds=%d suppressed=%d",
					channel.ID, voiceReportBurst, voiceReportPerSecond, int(voiceReportRefusalWindow/time.Second), suppressed),
				at: at,
			})
		}
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, schema.ErrorCodeVoiceReportRateLimited, "too many transmit reports")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, voiceReportMaxBytes)
	report, err := schema.DecodeVoiceTransmitReportV1(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		// A fixed message: the decoder's own names what the client sent.
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a voice transmit report: a state of started or stopped, and an optional audience")
		return
	}

	// V4 has one room per channel, for the whole channel. A report that
	// names any other audience (a net, a whisper) names an audience nobody
	// can hold a session for yet, so it is answered as having no session.
	// V5 gives nets rooms and this their lookup.
	if report.Audience != nil {
		writeVoiceNoSession(w)
		return
	}

	// The credential the request came in under must have been issued a
	// session for the channel's current room. Presence is not asked: it lags
	// a join by up to a second (docs/design/conch-voice.md §6).
	credID, ok := credentialIDFrom(ctx)
	if !ok {
		writeVoiceNoSession(w)
		return
	}
	room, held, err := s.voiceHeldRoom(ctx, channel.ID, caller.ID, credID)
	if err != nil {
		slog.ErrorContext(ctx, "voice: holder check for a transmit report failed", "channel", channel.ID, "error", err)
		writeInternalError(w)
		return
	}
	if !held {
		writeVoiceNoSession(w)
		return
	}

	if err := s.voice.reportTransmit(ctx, room, caller.ID, report.State == schema.VoiceTransmitStateStarted); err != nil {
		slog.ErrorContext(ctx, "voice: a transmit report could not be audited and was not applied", "channel", channel.ID, "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeVoiceNoSession is the one answer for a report from a caller who holds
// no session it could be reporting in: never issued one, issued one under
// another credential, the room rotated away since, or an audience that has no
// room. The client does not retry it; it goes back for a session.
func writeVoiceNoSession(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, schema.ErrorCodeVoiceNoSession, "no voice session for this channel under this credential")
}

// voiceHeldRoom finds the channel's current channel-wide room and reports
// whether principalID was issued a session for it under credentialID: the
// recorded holders (docs/design/voice-control-plane.md §4) are the authority.
// It only reads. A channel nobody has asked a session for has no room, and
// this must not be what creates one.
func (s *Server) voiceHeldRoom(ctx context.Context, channelID, principalID, credentialID int64) (store.VoiceRoom, bool, error) {
	rooms, err := s.store.VoiceRoomsForChannel(ctx, channelID)
	if err != nil {
		return store.VoiceRoom{}, false, err
	}
	for _, room := range rooms {
		if room.NetID != 0 {
			continue
		}
		holders, err := s.store.VoiceRoomHolders(ctx, room.ID)
		if err != nil {
			return store.VoiceRoom{}, false, err
		}
		want := store.VoiceHolder{RoomID: room.ID, PrincipalID: principalID, CredentialID: credentialID}
		return room, slices.Contains(holders, want), nil
	}
	return store.VoiceRoom{}, false, nil
}

// reportTransmit applies one report from principalID to room and writes its
// audit row. A report that does not change the reported state writes nothing
// and succeeds.
//
// The invariant, with applyRoom: a report is applied under p.mu, and its time
// (the time its audit row carries) is read under that same lock, as a pass's
// is. Reports and passes are therefore in one order, with times that rise in
// that order: a pass reads the reported state as it is at the instant of that
// pass, and no report is ever applied "between" a pass and its time. The row
// is written after the lock is released, with the time taken inside it, so two
// rows written in a race can be a few rows apart from their order in time;
// their times are the order of record.
//
// An error means the row could not be written. The report is then taken back,
// so that the reported state never says more than the audit log does.
func (p *voicePoller) reportTransmit(ctx context.Context, room store.VoiceRoom, principalID int64, started bool) error {
	p.mu.Lock()
	now := p.now()
	r := p.rooms[room.RoomName]
	if r == nil && !started {
		// Nothing is known of the room, so the reported state is `stopped`
		// already: no change, and no state to create for it.
		p.mu.Unlock()
		return nil
	}
	r = p.roomLocked(room)
	wasInUse := r.inUse(now)
	row, changed := r.transmit.report(now, principalID, started)
	p.mu.Unlock()
	if !changed {
		return nil
	}

	// Not tied to the caller: a client that hangs up now must not leave a
	// reported state with no row.
	ctx = context.WithoutCancel(ctx)
	if err := p.writeEvent(ctx, transmitEvents(room.ChannelID, []transmitRow{row})[0]); err != nil {
		p.mu.Lock()
		r.transmit.retract(principalID, started)
		p.mu.Unlock()
		return err
	}

	// A reported `started` is something the poller has to look at: a holder
	// who is not in the room, or never connects, is closed after 2 s by a
	// pass (§6). The room is in use from the report on (inUse), and if the
	// loop was not watching it, it is woken, as a session wakes it. A room
	// already being polled is not polled sooner: a report cannot be used to
	// choose when a pass happens.
	if !wasInUse {
		select {
		case p.wakeLoop <- struct{}{}:
		default: // a wake-up is already queued
		}
	}

	// A rotation can land between the holder check and here. If it also
	// dropped the room's state before the report was applied, the report
	// made that state again, for a room nobody can be in. So after any
	// change: if the room is retired, forget it, which closes what the
	// report opened with reason=left. A rotation that commits after this
	// read forgets the room itself.
	ctx, cancel := context.WithTimeout(ctx, voiceEvictTimeout)
	defer cancel()
	live, err := p.s.store.VoiceRoomLive(ctx, room.ID)
	switch {
	case err != nil:
		// The sweep forgets every retired room; bring it forward.
		slog.ErrorContext(ctx, "voice: could not tell whether a room was retired after a transmit report; the sweep checks", "channel", room.ChannelID, "error", err)
		p.sweepSoon()
	case !live:
		p.forgetRoom(ctx, room.RoomName)
	}
	return nil
}

// voiceReportLimiter is the bound on transmit reports: a token bucket per
// principal, in memory. It is lost on a restart, which gives every principal a
// full bucket once; that is one burst, not a way round the bound. The zero
// value is ready to use.
type voiceReportLimiter struct {
	mu      sync.Mutex
	buckets map[int64]*voiceReportBucket
	// pruned is when idle buckets were last dropped.
	pruned time.Time
}

// voiceReportCost is what one report costs, in the unit the bucket is kept in:
// time. A bucket earns one nanosecond of credit per nanosecond, up to
// voiceReportBurst reports' worth, and a report spends voiceReportCost of it.
// Whole nanoseconds, so the arithmetic is exact: a quarter of a second buys
// one report, and one nanosecond less buys none.
const voiceReportCost = time.Second / voiceReportPerSecond

type voiceReportBucket struct {
	// credit is the time banked, at most voiceReportBurst*voiceReportCost.
	credit time.Duration
	// at is when credit was last brought up to date.
	at time.Time
	// refusedAt is when the last audited refusal happened; refused says there
	// was one. suppressed counts the refusals since that wrote nothing.
	refused    bool
	refusedAt  time.Time
	suppressed int
}

// take spends one report of principalID's allowance and reports whether there
// was one to spend. When there was not, audit says whether this refusal is the
// one to write a row for (the first, or the first since
// voiceReportRefusalWindow after the last row) and suppressed how many
// refusals went unwritten since that last row. at is the time it read from
// now, under its own lock, so two calls are never judged out of order.
func (l *voiceReportLimiter) take(principalID int64, now func() time.Time) (allowed, audit bool, suppressed int, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at = now()
	l.prune(at)
	if l.buckets == nil {
		l.buckets = make(map[int64]*voiceReportBucket)
	}
	const full = voiceReportBurst * voiceReportCost
	b := l.buckets[principalID]
	if b == nil {
		b = &voiceReportBucket{credit: full, at: at}
		l.buckets[principalID] = b
	}
	if elapsed := at.Sub(b.at); elapsed > 0 {
		// Capped before it is added, so a long idle time cannot overflow.
		b.credit += min(elapsed, full-b.credit)
		b.at = at
	}
	if b.credit >= voiceReportCost {
		b.credit -= voiceReportCost
		return true, false, 0, at
	}
	if b.refused && at.Sub(b.refusedAt) < voiceReportRefusalWindow {
		b.suppressed++
		return false, false, 0, at
	}
	suppressed = b.suppressed
	b.refused, b.refusedAt, b.suppressed = true, at, 0
	return false, true, suppressed, at
}

// prune drops the buckets that hold nothing: full again, and with no refusal
// window still running. It runs at most once a window, so the map is bounded
// by the principals who reported in the last two.
func (l *voiceReportLimiter) prune(now time.Time) {
	if now.Sub(l.pruned) < voiceReportRefusalWindow {
		return
	}
	l.pruned = now
	for id, b := range l.buckets {
		if now.Sub(b.at) >= voiceReportBurst*voiceReportCost && (!b.refused || now.Sub(b.refusedAt) >= voiceReportRefusalWindow) {
			delete(l.buckets, id)
		}
	}
}
