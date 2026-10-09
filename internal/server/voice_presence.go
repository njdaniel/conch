package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Voice presence and live enforcement (issue #127, docs/design/voice-control-plane.md
// §5, §6, §7).
//
// A LiveKit token cannot be revoked and an open connection outlives it, so
// conchd watches the rooms. The voicePoller asks LiveKit who is in each room
// in use, removes anyone who is not a current, enabled, human member of the
// room's channel, derives who is connected and who is transmitting, writes the
// audit events, and tells presence subscribers when the picture changes.
//
// Nothing here puts a token, the API secret or a room name in a log line, an
// audit row, a client-facing error or a presence document. Rooms are named in
// logs by channel id.

const (
	// voicePollInterval is the pause between passes while any room is in use.
	voicePollInterval = 500 * time.Millisecond
	// voiceSweepInterval is how often the room list is re-read to rebuild "in
	// use" (design note §6). With no room in use it is the only call made.
	voiceSweepInterval = 30 * time.Second
	// voiceRetiredKeep is how long a retired room's row is kept after LiveKit
	// has stopped listing the room. LiveKit's own tokens for a room last ten
	// minutes after the last connection and conchd never creates a room for a
	// retired row, so a day is a wide margin, not a tuned value (issue #167).
	voiceRetiredKeep = 24 * time.Hour
	// voiceSessionRecent is how long after a session is issued its room counts
	// as in use: long enough for the token to be used (it is accepted for
	// about 75 seconds) and for the join to show up.
	voiceSessionRecent = 2 * time.Minute
	// voiceBackoffMin and voiceBackoffMax bound the wait between attempts
	// while LiveKit cannot be reached; it doubles from the first to the second.
	voiceBackoffMin = time.Second
	voiceBackoffMax = 30 * time.Second
	// voicePollConcurrency is how many rooms one pass polls at once.
	voicePollConcurrency = 8
	// voiceRemovalDedupe keeps one successful removal from being audited twice
	// when a pass lists the same connection again before LiveKit has dropped
	// it. A new connection by the same identity (a rejoin) is a new removal
	// and is audited: connections are told apart by their join time.
	voiceRemovalDedupe = 5 * time.Second
	// voiceSweepRetry is how soon a sweep is tried again when LiveKit
	// answered but the stored rooms could not be read.
	voiceSweepRetry = time.Second
	// voiceEvictTimeout bounds the immediate LiveKit work done inside an HTTP
	// request (rotating a room and deleting the old one, removing a
	// participant), so a LiveKit that does not answer cannot hold the
	// response for long. Passes and sweeps retry.
	voiceEvictTimeout = 4 * time.Second
	// voiceMinDelay keeps the loop from spinning if a deadline is already due
	// but its call is being held back.
	voiceMinDelay = 25 * time.Millisecond
)

// Reasons recorded on voice_participant_removed. They name why someone was
// not entitled and never carry the identity LiveKit reported. A revoke-all
// removes nobody by name any more: the rooms its credentials held are rotated
// (issue #161), and voiceReasonCredsRevoked only tells the hook which kind of
// loss it is handling.
const (
	voiceReasonBadIdentity      = "bad_identity"
	voiceReasonUnknownPrincipal = "unknown_principal"
	voiceReasonDisabled         = "disabled"
	voiceReasonNotHuman         = "not_human"
	voiceReasonNotMember        = "not_member"
	voiceReasonMemberRemoved    = "member_removed"
	voiceReasonPrincipalOff     = "principal_disabled"
	voiceReasonCredsRevoked     = "credentials_revoked"
)

// voiceSeen is what the poller last observed about one connected principal.
type voiceSeen struct {
	joinedAt time.Time
	// joinedKnown is whether joinedAt came from LiveKit rather than being
	// the time conchd first saw the participant.
	joinedKnown  bool
	transmitting bool
}

// voiceRoomState is the poller's memory of one stored voice room. All fields
// except the immutable name and channelID are guarded by voicePoller.mu.
type voiceRoomState struct {
	name      string
	channelID int64
	// lastSession is when a session was last issued for the room.
	lastSession time.Time
	// marked means a sweep found the room in LiveKit: it is polled on the next
	// pass whatever else is true, and the mark is cleared once polled.
	marked       bool
	participants map[int64]voiceSeen
	// stale means participants is what was last seen, not what is known: a
	// LiveKit call for this room failed and it has not been read since.
	// Presence shows nobody in a stale room.
	stale bool
	// retired means the room was rotated away and its state dropped: a pass
	// that was already reading it must not report what it finds.
	retired bool
	// seq counts evictions from this room and evicted records, per principal,
	// the count at their last one. A pass notes seq before it asks LiveKit
	// and does not put back anyone evicted after that: without it a pass in
	// flight when a member is removed would show them again, and audit a
	// join that never happened.
	seq     uint64
	evicted map[int64]uint64
}

func (r *voiceRoomState) inUse(now time.Time) bool {
	return r.marked || len(r.participants) > 0 ||
		(!r.lastSession.IsZero() && now.Sub(r.lastSession) < voiceSessionRecent)
}

// voiceEvent is one audit row to write.
type voiceEvent struct {
	actor     string
	action    string
	channelID int64
	detail    string
}

// voicePoller owns presence state, the poller that fills it, and the
// subscribers who watch it. It is built for every server so that presence
// (and the sockets that stream it) answers "not configured" without a
// special case; its goroutine is started only when voice is configured.
type voicePoller struct {
	s *Server
	// now is the clock. Tests replace it; nothing else may.
	now func() time.Time
	// entitle and storedRooms are the poller's two reads of the store:
	// Server.voiceEntitlement and Store.ListVoiceRooms. Tests replace them to
	// make the store fail; nothing else may.
	entitle     func(ctx context.Context, identity string, channelID int64) (int64, string, error)
	storedRooms func(ctx context.Context) ([]store.VoiceRoom, error)
	// memberRooms is Store.VoiceRoomsForMember, the read behind the
	// immediate removal of a disabled principal. invalid is
	// Store.InvalidVoiceRooms, the invariant check, and retiredRooms is
	// Store.ListRetiredVoiceRooms. Test seams like the two above.
	memberRooms func(ctx context.Context, principalID int64) ([]store.VoiceRoom, error)
	invalid     func(ctx context.Context, now time.Time) ([]store.VoiceViolation, error)
	// recordHolder is Store.RecordVoiceHolder, called by the session path
	// before a token is signed. A test seam like the others.
	recordHolder func(ctx context.Context, roomID, principalID, credentialID int64) error
	retiredRooms func(ctx context.Context) ([]store.VoiceRoom, error)

	// pass is set while a pass runs, so one that has not finished is not
	// started again.
	pass atomic.Bool
	// wakeLoop interrupts the loop's wait. With no room in use the loop
	// sleeps until the next sweep, up to voiceSweepInterval away; a session
	// issued meanwhile must start the polling now, not then. It holds one
	// queued wake-up and is only ever sent to without blocking.
	wakeLoop chan struct{}

	mu    sync.Mutex
	rooms map[string]*voiceRoomState
	// removed remembers recent successful removals for audit de-duplication.
	removed map[string]time.Time
	// contacted is whether LiveKit has answered at least once; down is the
	// current outage state. Presence is available only when contacted && !down.
	contacted bool
	down      bool
	backoff   time.Duration
	// retryAt holds back every LiveKit call until it passes, during an outage.
	retryAt   time.Time
	nextSweep time.Time

	subs   map[*voiceSub]struct{}
	closed bool
}

func newVoicePoller(s *Server) *voicePoller {
	return &voicePoller{
		s:            s,
		now:          time.Now,
		entitle:      s.voiceEntitlement,
		storedRooms:  s.store.ListVoiceRooms,
		memberRooms:  s.store.VoiceRoomsForMember,
		invalid:      s.store.InvalidVoiceRooms,
		retiredRooms: s.store.ListRetiredVoiceRooms,
		recordHolder: s.store.RecordVoiceHolder,
		wakeLoop:     make(chan struct{}, 1),
		rooms:        make(map[string]*voiceRoomState),
		removed:      make(map[string]time.Time),
		subs:         make(map[*voiceSub]struct{}),
	}
}

// ---------------------------------------------------------------------------
// Entitlement

// parseVoiceIdentity reads a LiveKit identity of the form voiceIdentity
// writes: "p" and a positive decimal id with no sign, no leading zero and
// nothing after it. Anything else names no principal.
func parseVoiceIdentity(identity string) (int64, bool) {
	digits, ok := strings.CutPrefix(identity, "p")
	if !ok || digits == "" || digits[0] == '0' || len(digits) > 18 {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// voiceEntitlement is the one question the poller asks of a participant: is
// this identity a current, enabled, human member of the channel the room
// belongs to? It returns the principal id when the identity names one, and an
// empty reason when the participant is entitled; otherwise the reason says
// why not. A store failure is returned as err and means "could not decide":
// the caller neither shows nor removes the participant on that pass.
func (s *Server) voiceEntitlement(ctx context.Context, identity string, channelID int64) (principalID int64, reason string, err error) {
	id, ok := parseVoiceIdentity(identity)
	if !ok {
		return 0, voiceReasonBadIdentity, nil
	}
	p, err := s.store.PrincipalByID(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 0, voiceReasonUnknownPrincipal, nil
	case err != nil:
		return id, "", err
	}
	switch {
	case p.DisabledAt != nil:
		return id, voiceReasonDisabled, nil
	case p.Kind != store.PrincipalHuman:
		return id, voiceReasonNotHuman, nil
	}
	member, err := s.store.IsChannelMember(ctx, channelID, id)
	switch {
	case err != nil:
		return id, "", err
	case !member:
		return id, voiceReasonNotMember, nil
	}
	return id, "", nil
}

// ---------------------------------------------------------------------------
// Lifecycle

// start runs the poller until ctx ends or the returned stop is called. stop
// waits for the goroutine, so nothing is left running after it returns.
func (p *voicePoller) start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.loop(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (p *voicePoller) loop(ctx context.Context) {
	timer := time.NewTimer(0) // the first sweep is immediate: restart recovery
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-p.wakeLoop:
			// Stop the timer and drain it, so the Reset below starts clean.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		p.tick(ctx)
		timer.Reset(p.nextDelay())
	}
}

// tick is one turn of the loop: a sweep when one is due, then a pass.
func (p *voicePoller) tick(ctx context.Context) {
	p.mu.Lock()
	sweepDue := !p.now().Before(p.nextSweep)
	p.mu.Unlock()
	if sweepDue {
		p.runSweep(ctx)
	}
	if ctx.Err() == nil {
		p.runPass(ctx)
	}
}

// nextDelay is how long the loop waits before its next tick: the polling
// interval while any room is in use, otherwise until the next sweep, and
// never sooner than the end of a backoff.
func (p *voicePoller) nextDelay() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	d := p.nextSweep.Sub(now)
	for _, r := range p.rooms {
		if r.inUse(now) {
			d = min(d, voicePollInterval)
			break
		}
	}
	if wait := p.retryAt.Sub(now); wait > d {
		d = wait
	}
	return max(d, voiceMinDelay)
}

// anyInUse reports whether a pass would poll a room (for tests).
func (p *voicePoller) anyInUse() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, r := range p.rooms {
		if r.inUse(now) {
			return true
		}
	}
	return false
}

// noteSession records that a session was issued for room, so the next pass
// polls it. It is called before the token is signed: by the time a token
// exists, the room is already known to be in use.
func (p *voicePoller) noteSession(room store.VoiceRoom) {
	if room.NetID != 0 {
		return // net rooms arrive with V5; see roomLocked
	}
	now := p.now()
	p.mu.Lock()
	r := p.roomLocked(room)
	wasInUse := r.inUse(now)
	r.lastSession = now
	p.mu.Unlock()
	// A room already in use is being polled every interval; only a room the
	// loop was not watching needs it woken. So a member asking for sessions
	// in a loop cannot make the poller run faster than its interval.
	if wasInUse {
		return
	}
	select {
	case p.wakeLoop <- struct{}{}:
	default: // a wake-up is already queued
	}
}

// roomLocked returns the state of room, creating it. Only channel-wide rooms
// are tracked: they are the only rooms V3 creates, and a net room's
// entitlement is net membership, which this file does not check, so polling it
// as if it were the channel's room would show the wrong people. Callers pass
// only channel-wide rooms.
func (p *voicePoller) roomLocked(room store.VoiceRoom) *voiceRoomState {
	r := p.rooms[room.RoomName]
	if r == nil {
		r = &voiceRoomState{name: room.RoomName, channelID: room.ChannelID, participants: map[int64]voiceSeen{}, evicted: map[int64]uint64{}}
		p.rooms[room.RoomName] = r
	}
	return r
}

// ---------------------------------------------------------------------------
// Sweep

// runSweep rebuilds "in use" and enforces the invariant for every stored room.
//
// First the invariant, from the store alone (so it also runs while LiveKit is
// down): every live room with a holder who is no longer valid is rotated. That
// is what covers a credential expiring, a restart, and any change made without
// going through a hook. Then one ListRooms call: every live stored room that
// LiveKit lists is marked for the next pass, and every retired room that
// LiveKit still lists is deleted, which is how a rotation whose DeleteRoom
// failed, or that a restart interrupted, is finished. The listed participant
// count is never read (it lags a join by seconds, design note §10 finding 5).
// It reports whether it made the call.
func (p *voicePoller) runSweep(ctx context.Context) bool {
	if p.s.lk == nil {
		return false
	}
	_, invErr := p.rotateInvalid(ctx)
	if invErr != nil && ctx.Err() == nil {
		// Until this read works again, a credential expiring and any change
		// that did not go through a hook rotate nothing.
		slog.ErrorContext(ctx, "voice: sweep could not check the holders of every room; trying again soon", "error", invErr)
	}
	now := p.now()
	p.mu.Lock()
	held := now.Before(p.retryAt)
	p.mu.Unlock()
	if held {
		return false
	}
	listed, err := p.s.lk.ListRooms(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.fail(ctx, err)
		}
		return true
	}
	p.succeed()
	inLiveKit := make(map[string]struct{}, len(listed))
	for _, r := range listed {
		inLiveKit[r.Name] = struct{}{}
	}
	stored, err := p.storedRooms(ctx)
	if err != nil {
		// LiveKit answered; the store did not. Nothing is marked. The sweep
		// is tried again soon, but not on every tick: without a new due time
		// the loop would ask LiveKit for its rooms forty times a second for
		// as long as the store stayed broken.
		slog.ErrorContext(ctx, "voice: sweep could not read stored rooms", "error", err)
		p.mu.Lock()
		p.nextSweep = now.Add(voiceSweepRetry)
		p.mu.Unlock()
		return true
	}
	retired, retiredErr := p.retiredRooms(ctx)
	if retiredErr != nil {
		slog.ErrorContext(ctx, "voice: sweep could not read retired rooms", "error", retiredErr)
	}
	p.mu.Lock()
	next := now.Add(voiceSweepInterval)
	if invErr != nil || retiredErr != nil {
		// The invariant or the cleanup could not be done: look again soon,
		// not in thirty seconds.
		next = now.Add(voiceSweepRetry)
	}
	p.nextSweep = next
	for _, sr := range stored {
		if sr.NetID != 0 {
			continue
		}
		r := p.roomLocked(sr)
		if _, ok := inLiveKit[sr.RoomName]; ok {
			r.marked = true
		}
	}
	p.mu.Unlock()

	// A retired room is never polled or shown, and LiveKit is told to delete
	// it for as long as it still lists it.
	var stale []store.VoiceRoom
	for _, rr := range retired {
		p.forgetRoom(ctx, rr.RoomName)
		if _, ok := inLiveKit[rr.RoomName]; ok {
			stale = append(stale, rr)
		}
	}
	p.deleteRooms(ctx, stale)
	// A retired room that LiveKit no longer lists, a day after it was
	// retired, has nothing left to be found for: its row goes. Only when the
	// retired rooms were read (and so compared with LiveKit's list) in this
	// sweep.
	if retiredErr == nil {
		keep := make([]int64, 0, len(stale))
		for _, rr := range stale {
			keep = append(keep, rr.ID)
		}
		if n, err := p.s.store.PruneRetiredVoiceRooms(ctx, now.Add(-voiceRetiredKeep), keep); err != nil {
			slog.ErrorContext(ctx, "voice: sweep could not prune retired rooms", "error", err)
		} else if n > 0 {
			slog.InfoContext(ctx, "voice: pruned retired rooms LiveKit no longer has", "rooms", n)
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Pass

// runPass polls every room in use, at most voicePollConcurrency at a time,
// and reports whether it ran. A pass is skipped, and reports false, while a
// previous one is still running or while an outage backoff is in force.
func (p *voicePoller) runPass(ctx context.Context) bool {
	if p.s.lk == nil || !p.pass.CompareAndSwap(false, true) {
		return false
	}
	defer p.pass.Store(false)

	now := p.now()
	p.mu.Lock()
	if now.Before(p.retryAt) {
		p.mu.Unlock()
		return false
	}
	var due []*voiceRoomState
	for _, r := range p.rooms {
		if r.inUse(now) {
			due = append(due, r)
		}
	}
	p.mu.Unlock()
	if len(due) == 0 {
		return true
	}

	var (
		wg      sync.WaitGroup
		errMu   sync.Mutex
		failure error
		failed  []*voiceRoomState
		sem     = make(chan struct{}, voicePollConcurrency)
	)
	for _, r := range due {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := p.pollRoom(ctx, r); err != nil {
				errMu.Lock()
				failure = errors.Join(failure, err)
				failed = append(failed, r)
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()
	switch {
	case ctx.Err() != nil:
		// Shutting down: a cancelled call is not an outage.
	case len(failed) == len(due):
		// Nothing could be read: LiveKit is not answering.
		p.fail(ctx, failure)
	default:
		// LiveKit answered for at least one room, so it is reachable. A room
		// it could not answer for (one bad response must not stop enforcement
		// everywhere) is hidden from presence and tried again next pass.
		p.succeed()
		if len(failed) > 0 {
			slog.WarnContext(ctx, "voice: some rooms could not be read; they are hidden and retried", "rooms", len(failed), "error", failure)
			p.mu.Lock()
			var channels []int64
			for _, r := range failed {
				if !r.stale && len(r.participants) > 0 {
					channels = append(channels, r.channelID)
				}
				r.stale, r.marked = true, true
			}
			p.mu.Unlock()
			for _, id := range channels {
				p.notify(id)
			}
		}
	}
	return true
}

// pollRoom asks LiveKit who is in r, removes whoever is not entitled, and
// folds the rest into the room's state. A returned error means LiveKit could
// not be asked. No lock is held across a LiveKit or store call.
func (p *voicePoller) pollRoom(ctx context.Context, r *voiceRoomState) error {
	p.mu.Lock()
	since := r.seq
	p.mu.Unlock()
	parts, err := p.s.lk.ListParticipants(ctx, r.name)
	if err != nil {
		return err
	}
	seen := make(map[int64]livekit.Participant, len(parts))
	// undecided holds the principals whose entitlement could not be read.
	undecided := make(map[int64]struct{})
	// again is set when this pass left someone in the room it could not deal
	// with: a removal that failed, or an entitlement it could not read. The
	// room is then polled on the next pass whatever its state says, instead
	// of waiting for the next sweep to notice it is still occupied.
	again := false
	for _, part := range parts {
		pid, reason, err := p.entitle(ctx, part.Identity, r.channelID)
		if err != nil {
			// Not removed: removing on a database error would let a store
			// hiccup empty every room. Not newly shown either. Someone who
			// was entitled on the last pass keeps the state they had; the
			// next pass decides. Everyone else in the room is still handled
			// now, so a person removed on this pass leaves presence on this
			// pass, whatever happened to a neighbour's check.
			slog.ErrorContext(ctx, "voice: entitlement check failed", "channel", r.channelID, "error", err)
			if pid > 0 {
				undecided[pid] = struct{}{}
			}
			again = true
			continue
		}
		if reason != "" {
			if !p.removeObserved(ctx, r, part, pid, reason) {
				again = true
			}
			continue
		}
		seen[pid] = part
	}
	p.applyRoom(ctx, r, seen, undecided, again, since)
	return nil
}

// removeObserved removes a participant a pass found who must not be there.
// A failure is logged and left to the next pass, which finds them again: the
// participant is never added to the room's state, so no snapshot shows them.
// It reports whether the removal succeeded.
func (p *voicePoller) removeObserved(ctx context.Context, r *voiceRoomState, part livekit.Participant, pid int64, reason string) bool {
	identity := part.Identity
	removed, err := p.s.lk.Evict(ctx, r.name, identity)
	if err != nil {
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "voice: removal failed; the next pass retries", "channel", r.channelID, "principal", pid, "reason", reason, "error", err)
		}
		return false
	}
	// Audited when LiveKit actually disconnected someone: a participant who
	// left by themselves between the list and the call was not removed.
	if removed && p.firstRemoval(r.name, identity, part.JoinedAt) {
		p.writeEvents(ctx, p.removedEvent(r.channelID, pid, reason))
	}
	return true
}

// removedEvent builds the voice_participant_removed row. The actor is the
// system: conchd removed someone. The removed principal is in the detail when
// the identity named one.
func (p *voicePoller) removedEvent(channelID, pid int64, reason string) voiceEvent {
	target := "none"
	if pid > 0 {
		target = strconv.FormatInt(pid, 10)
	}
	return voiceEvent{
		actor: "system", action: store.AuditVoiceParticipantRemoved, channelID: channelID,
		detail: fmt.Sprintf("channel=%d audience=channel principal=%s reason=%s source=observed", channelID, target, reason),
	}
}

// applyRoom replaces r's participants with seen and writes the events that
// follow from the difference between the two passes.
//
// Every event time is the time of the pass that noticed the change, so it is
// accurate to one polling interval: a join or a transmission shorter than the
// interval can be missed entirely (design note §7). After an outage the
// difference spans the whole outage.
//
// keep names principals whose entitlement could not be read on this pass: one
// who was in the room's state stays in it unchanged, and one who was not is
// not added. again keeps the room marked for the next pass. since is the
// room's eviction count when the pass asked LiveKit: anyone evicted after that
// is left out, because what the pass saw of them is older than their removal.
func (p *voicePoller) applyRoom(ctx context.Context, r *voiceRoomState, seen map[int64]livekit.Participant, keep map[int64]struct{}, again bool, since uint64) {
	now := p.now()
	next := make(map[int64]voiceSeen, len(seen))

	p.mu.Lock()
	if r.retired {
		// Rotated away while this pass was reading it: whatever it saw is
		// in a room that no longer exists, and nobody is shown for it.
		p.mu.Unlock()
		return
	}
	old := r.participants
	for pid := range keep {
		if o, ok := old[pid]; ok {
			next[pid] = o
		}
	}
	for pid, at := range r.evicted {
		if at > since {
			delete(seen, pid)
			// Still connected as far as this pass saw: look again at once.
			again = true
		} else {
			delete(r.evicted, pid)
		}
	}
	for pid, part := range seen {
		s := voiceSeen{transmitting: part.Transmitting(), joinedAt: part.JoinedAt, joinedKnown: !part.JoinedAt.IsZero()}
		if !s.joinedKnown {
			s.joinedAt = now
			if o, ok := old[pid]; ok {
				s.joinedAt, s.joinedKnown = o.joinedAt, o.joinedKnown
			}
		}
		next[pid] = s
	}
	pids := make([]int64, 0, len(old)+len(next))
	for pid := range old {
		pids = append(pids, pid)
	}
	for pid := range next {
		if _, ok := old[pid]; !ok {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)

	var events []voiceEvent
	changed := false
	ev := func(action string, pid int64) {
		events = append(events, voiceEvent{
			actor: fmt.Sprintf("principal:%d", pid), action: action, channelID: r.channelID,
			detail: fmt.Sprintf("channel=%d audience=channel source=observed", r.channelID),
		})
	}
	for _, pid := range pids {
		o, wasThere := old[pid]
		n, isThere := next[pid]
		reconnected := wasThere && isThere && o.joinedKnown && n.joinedKnown && !o.joinedAt.Equal(n.joinedAt)
		switch {
		case wasThere && (!isThere || reconnected):
			// Gone, or displaced by a new connection between two passes
			// (the same identity joining again replaces the first).
			if o.transmitting {
				ev(store.AuditVoiceTransmitStopped, pid)
			}
			ev(store.AuditVoiceLeft, pid)
			changed = true
			if reconnected {
				ev(store.AuditVoiceJoined, pid)
				if n.transmitting {
					ev(store.AuditVoiceTransmitStarted, pid)
				}
			}
		case !wasThere && isThere:
			ev(store.AuditVoiceJoined, pid)
			if n.transmitting {
				ev(store.AuditVoiceTransmitStarted, pid)
			}
			changed = true
		case wasThere && isThere && o.transmitting != n.transmitting:
			if n.transmitting {
				ev(store.AuditVoiceTransmitStarted, pid)
			} else {
				ev(store.AuditVoiceTransmitStopped, pid)
			}
			changed = true
		}
	}
	r.participants = next
	r.stale = false
	r.marked = again
	p.mu.Unlock()

	p.writeEvents(ctx, events...)
	if changed {
		p.notify(r.channelID)
	}
}

// ---------------------------------------------------------------------------
// Outage state

// fail records that LiveKit could not be asked. The first failure of an
// outage writes one voice_enforcement_unavailable event and tells subscribers
// presence is unavailable; every failure lengthens the wait before the next
// attempt, 1 s doubling to 30 s.
func (p *voicePoller) fail(ctx context.Context, err error) {
	now := p.now()
	p.mu.Lock()
	first := !p.down
	p.down = true
	if p.backoff == 0 {
		p.backoff = voiceBackoffMin
	} else {
		p.backoff = min(p.backoff*2, voiceBackoffMax)
	}
	p.retryAt = now.Add(p.backoff)
	wait := p.backoff
	// What was last seen is no longer known. It stays hidden after LiveKit
	// answers again until each room has been read: a sweep can end the
	// outage before any room is polled.
	for _, r := range p.rooms {
		r.stale = true
	}
	p.mu.Unlock()
	if first {
		slog.WarnContext(ctx, "voice: livekit unreachable; enforcement and presence are unavailable", "error", err)
		p.writeEvents(ctx, voiceEvent{actor: "system", action: store.AuditVoiceEnforcementUnavailable, detail: "source=observed"})
		p.notifyAll()
		return
	}
	slog.DebugContext(ctx, "voice: livekit still unreachable", "retry_in", wait)
}

// succeed records that LiveKit answered. It ends an outage and reports the
// first contact; subscribers are told because availability changed.
func (p *voicePoller) succeed() {
	p.mu.Lock()
	changed := p.down || !p.contacted
	if p.down {
		slog.Info("voice: livekit reachable again")
	}
	p.down, p.contacted = false, true
	p.backoff, p.retryAt = 0, time.Time{}
	p.mu.Unlock()
	if changed {
		p.notifyAll()
	}
}

// ---------------------------------------------------------------------------
// Removal bookkeeping

// firstRemoval reports whether a successful removal of identity's connection
// to room should be audited: false when the removal of that same connection
// was audited within voiceRemovalDedupe. joinedAt identifies the connection;
// when LiveKit did not report one, any removal of the identity in that time
// counts as the same.
func (p *voicePoller) firstRemoval(room, identity string, joinedAt time.Time) bool {
	now := p.now()
	key := room + "\x00" + identity
	if !joinedAt.IsZero() {
		key += "\x00" + strconv.FormatInt(joinedAt.UnixMilli(), 10)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, at := range p.removed {
		if now.Sub(at) >= voiceRemovalDedupe {
			delete(p.removed, k)
		}
	}
	if _, ok := p.removed[key]; ok {
		return false
	}
	p.removed[key] = now
	return true
}

// evict removes principalID from rooms right now, without waiting for a pass:
// the member was removed, the principal disabled, or its credentials revoked.
// It never fails the caller's request. What it cannot do (LiveKit down, a
// removal refused) is left to the passes: an unentitled participant is found
// by membership. It is the immediate half of the per-pass removal, for
// identities the rotation of a room does not cover (one conchd never issued a
// session to); a holder's room is rotated instead (issue #161).
//
// The participant leaves presence first: nobody who has just lost their place
// is shown to the room for even one more pass.
func (p *voicePoller) evict(ctx context.Context, rooms []store.VoiceRoom, principalID int64, reason string) {
	if p.s.lk == nil || len(rooms) == 0 {
		return
	}
	// A caller who hangs up must not cancel a security action.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	identity := voiceIdentity(principalID)
	var wg sync.WaitGroup
	sem := make(chan struct{}, voicePollConcurrency)
	for _, room := range rooms {
		if room.NetID != 0 {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p.evictFromRoom(ctx, room, principalID, identity, reason)
		}()
	}
	wg.Wait()
}

func (p *voicePoller) evictFromRoom(ctx context.Context, room store.VoiceRoom, principalID int64, identity, reason string) {
	was, ok := p.forget(room.RoomName, principalID)
	removed, err := p.s.lk.Evict(ctx, room.RoomName, identity)
	var events []voiceEvent
	if err != nil {
		slog.WarnContext(ctx, "voice: immediate removal failed; passes retry", "channel", room.ChannelID, "principal", principalID, "reason", reason, "error", err)
	} else {
		joined := time.Time{}
		if was.joinedKnown {
			joined = was.joinedAt
		}
		// Audited when LiveKit disconnected someone, whether or not a pass
		// had seen them yet: a person removed a moment after joining was
		// still removed.
		if removed && p.firstRemoval(room.RoomName, identity, joined) {
			events = append(events, p.removedEvent(room.ChannelID, principalID, reason))
		}
	}
	if ok {
		actor := fmt.Sprintf("principal:%d", principalID)
		detail := fmt.Sprintf("channel=%d audience=channel source=observed", room.ChannelID)
		if was.transmitting {
			events = append(events, voiceEvent{actor: actor, action: store.AuditVoiceTransmitStopped, channelID: room.ChannelID, detail: detail})
		}
		events = append(events, voiceEvent{actor: actor, action: store.AuditVoiceLeft, channelID: room.ChannelID, detail: detail})
		p.notify(room.ChannelID)
	}
	p.writeEvents(ctx, events...)
}

// forgetEverywhere takes principalID out of presence in every room the
// poller has them in, with the events of their leaving, without calling
// LiveKit: used when the rooms to evict from could not be read.
func (p *voicePoller) forgetEverywhere(ctx context.Context, principalID int64) {
	var events []voiceEvent
	var channels []int64
	p.mu.Lock()
	for _, r := range p.rooms {
		was, ok := r.participants[principalID]
		if !ok {
			continue
		}
		delete(r.participants, principalID)
		r.seq++
		r.evicted[principalID] = r.seq
		// Someone is still connected there: keep the room polled.
		r.marked = true
		actor := fmt.Sprintf("principal:%d", principalID)
		detail := fmt.Sprintf("channel=%d audience=channel source=observed", r.channelID)
		if was.transmitting {
			events = append(events, voiceEvent{actor: actor, action: store.AuditVoiceTransmitStopped, channelID: r.channelID, detail: detail})
		}
		events = append(events, voiceEvent{actor: actor, action: store.AuditVoiceLeft, channelID: r.channelID, detail: detail})
		channels = append(channels, r.channelID)
	}
	p.mu.Unlock()
	p.writeEvents(ctx, events...)
	for _, id := range channels {
		p.notify(id)
	}
}

// forget takes principalID out of the room's presence, reporting what it
// held. ok is false when the poller did not believe the principal was there.
func (p *voicePoller) forget(room string, principalID int64) (voiceSeen, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rooms[room]
	if r == nil {
		return voiceSeen{}, false
	}
	s, ok := r.participants[principalID]
	delete(r.participants, principalID)
	// Recorded even when the poller had not seen them: a pass in flight may
	// be about to.
	r.seq++
	r.evicted[principalID] = r.seq
	return s, ok
}

// ---------------------------------------------------------------------------
// Audit

// writeEvents appends rows to the audit log. A failure is logged and does not
// stop the poller: presence and enforcement are more important than a
// complete record of one observation. The write is not tied to the pass's
// context, so a shutdown does not drop events already derived.
func (p *voicePoller) writeEvents(ctx context.Context, events ...voiceEvent) {
	ctx = context.WithoutCancel(ctx)
	for _, e := range events {
		subject := "voice"
		if e.channelID > 0 {
			subject = fmt.Sprintf("channel:%d", e.channelID)
		}
		if _, err := p.s.store.AppendAuditEvent(ctx, e.actor, e.action, subject, e.detail); err != nil {
			slog.ErrorContext(ctx, "voice: audit write failed", "action", e.action, "channel", e.channelID, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Presence documents and subscribers

// snapshot is the whole voice state of a channel as a reader sees it. It
// never contains a room name or a token. Participants last seen are not
// reported while LiveKit cannot be reached (schema: not available implies no
// rooms), and nothing is available until LiveKit has answered once.
func (p *voicePoller) snapshot(channelID int64) schema.VoicePresenceV1 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked(channelID)
}

func (p *voicePoller) snapshotLocked(channelID int64) schema.VoicePresenceV1 {
	snap := schema.VoicePresenceV1{Schema: schema.VoicePresenceSchemaV1, ChannelID: channelID, Rooms: []schema.VoicePresenceRoom{}}
	if p.s.lk == nil {
		return snap
	}
	snap.Configured = true
	if !p.contacted || p.down {
		return snap
	}
	snap.Available = true
	room := schema.VoicePresenceRoom{Participants: []schema.VoiceParticipant{}}
	for _, r := range p.rooms {
		if r.channelID != channelID || r.stale {
			continue
		}
		for pid, s := range r.participants {
			room.Participants = append(room.Participants, schema.VoiceParticipant{
				PrincipalID: pid,
				// Every V3 member may publish into the channel-wide room
				// (voiceAudiences); a listen-only room arrives with nets.
				CanPublish:   true,
				Transmitting: s.transmitting,
				JoinedAt:     schema.NewTimestamp(s.joinedAt),
			})
		}
	}
	slices.SortFunc(room.Participants, func(a, b schema.VoiceParticipant) int {
		switch {
		case a.PrincipalID < b.PrincipalID:
			return -1
		case a.PrincipalID > b.PrincipalID:
			return 1
		}
		return 0
	})
	snap.Rooms = append(snap.Rooms, room)
	return snap
}

// voiceSub is one presence socket's subscription. wake has room for one
// queued notification: the poller only ever does a non-blocking send, so a
// slow socket costs the poller nothing, and several changes collapse into one
// wake-up because the socket reads the whole state, not a delta.
type voiceSub struct {
	p           *voicePoller
	channelID   int64
	principalID int64
	wake        chan struct{}
	dropped     chan struct{}
	gone        bool // guarded by p.mu
}

// subscribe registers a subscriber. After closeAll it returns one that is
// already dropped.
func (p *voicePoller) subscribe(channelID, principalID int64) *voiceSub {
	sub := &voiceSub{p: p, channelID: channelID, principalID: principalID, wake: make(chan struct{}, 1), dropped: make(chan struct{})}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		sub.gone = true
		close(sub.dropped)
		return sub
	}
	p.subs[sub] = struct{}{}
	return sub
}

func (s *voiceSub) cancel() {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	delete(s.p.subs, s)
}

// current returns the channel's snapshot, or ok=false when the subscription
// has been dropped. Checking and reading under one lock means a snapshot is
// never taken for a subscriber after the drop that removed them returned.
func (s *voiceSub) current() (schema.VoicePresenceV1, bool) {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	if s.gone {
		return schema.VoicePresenceV1{}, false
	}
	return s.p.snapshotLocked(s.channelID), true
}

func (p *voicePoller) dropLocked(sub *voiceSub) {
	delete(p.subs, sub)
	if !sub.gone {
		sub.gone = true
		close(sub.dropped)
	}
}

// dropPrincipal closes principalID's presence subscriptions on channelID. A
// zero principal (no caller) matches nothing.
func (p *voicePoller) dropPrincipal(channelID, principalID int64) {
	if principalID == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.subs {
		if sub.principalID == principalID && sub.channelID == channelID {
			p.dropLocked(sub)
		}
	}
}

// dropPrincipalAll closes principalID's presence subscriptions on every channel.
func (p *voicePoller) dropPrincipalAll(principalID int64) {
	if principalID == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.subs {
		if sub.principalID == principalID {
			p.dropLocked(sub)
		}
	}
}

// closeAll drops every subscription and refuses new ones: shutdown.
func (p *voicePoller) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for sub := range p.subs {
		p.dropLocked(sub)
	}
}

func (p *voicePoller) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// notify wakes the subscribers of channelID. It never blocks.
func (p *voicePoller) notify(channelID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.subs {
		if sub.channelID == channelID {
			wake(sub)
		}
	}
}

// notifyAll wakes every subscriber: availability changed for all channels.
func (p *voicePoller) notifyAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.subs {
		wake(sub)
	}
}

func wake(sub *voiceSub) {
	select {
	case sub.wake <- struct{}{}:
	default: // one wake-up is already queued
	}
}

// ---------------------------------------------------------------------------
// Rotation (issue #161)

// voiceRotation is one room that was rotated: the retired room and the live
// one that replaced it.
type voiceRotation struct {
	old, next store.VoiceRoom
}

// rotateInvalid is the invariant (design note §5): every live room whose
// holders are not all valid is given a new name in the store and forgotten by
// the poller, whose presence for the old room goes with it. It needs only the
// store, so it runs whether or not LiveKit answers. It returns the rotations
// it made; the old rooms still have to be deleted in LiveKit, which is the
// caller's job (a hook does it at once, the sweep finds any that were missed).
//
// A store failure is returned and a sweep is scheduled soon. A rotation that
// fails does not stop the others: one bad row must not keep another channel's
// removed member in a room.
func (p *voicePoller) rotateInvalid(ctx context.Context) ([]voiceRotation, error) {
	violations, err := p.invalid(ctx, p.now())
	if err != nil {
		p.sweepSoon()
		return nil, err
	}
	var done []voiceRotation
	var failure error
	for _, v := range violations {
		next, rotated, err := p.s.store.RotateVoiceRoom(ctx, v.Room.ID, v.Reason)
		if err != nil {
			slog.ErrorContext(ctx, "voice: rotating a room failed; the sweep retries", "channel", v.Room.ChannelID, "reason", v.Reason, "error", err)
			failure = errors.Join(failure, err)
			continue
		}
		if !rotated {
			continue // someone else rotated it first
		}
		slog.InfoContext(ctx, "voice: room rotated", "channel", v.Room.ChannelID, "reason", v.Reason)
		p.forgetRoom(ctx, v.Room.RoomName)
		done = append(done, voiceRotation{old: v.Room, next: next})
	}
	if failure != nil {
		p.sweepSoon()
	}
	return done, failure
}

// sweepSoon brings the next sweep forward to voiceSweepRetry from now and
// wakes the loop, after a store failure that left the invariant unchecked or
// a retired room that could not be deleted.
func (p *voicePoller) sweepSoon() {
	at := p.now().Add(voiceSweepRetry)
	p.mu.Lock()
	if p.nextSweep.After(at) {
		p.nextSweep = at
	}
	p.mu.Unlock()
	select {
	case p.wakeLoop <- struct{}{}:
	default:
	}
}

// forgetRoom drops the poller's state for a retired room and tells the
// channel's subscribers: presence shows the new room, empty, until people
// rejoin. Whoever the poller had in the room is recorded as having left. The
// state is marked retired so a pass already reading the room reports nothing.
func (p *voicePoller) forgetRoom(ctx context.Context, name string) {
	p.mu.Lock()
	r := p.rooms[name]
	if r == nil {
		p.mu.Unlock()
		return
	}
	delete(p.rooms, name)
	r.retired = true
	pids := make([]int64, 0, len(r.participants))
	for pid := range r.participants {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	var events []voiceEvent
	for _, pid := range pids {
		actor := fmt.Sprintf("principal:%d", pid)
		detail := fmt.Sprintf("channel=%d audience=channel source=observed", r.channelID)
		if r.participants[pid].transmitting {
			events = append(events, voiceEvent{actor: actor, action: store.AuditVoiceTransmitStopped, channelID: r.channelID, detail: detail})
		}
		events = append(events, voiceEvent{actor: actor, action: store.AuditVoiceLeft, channelID: r.channelID, detail: detail})
	}
	channelID := r.channelID
	p.mu.Unlock()
	p.writeEvents(ctx, events...)
	p.notify(channelID)
}

// deleteRooms deletes retired rooms in LiveKit, which disconnects everyone in
// them and makes every token for them, LiveKit's own refresh tokens included,
// a token for a room that does not exist. A failure is logged and left to the
// sweep, which deletes any retired room LiveKit still lists and is brought
// forward for it: until the room is gone the people in it, the one who lost
// their place included, are connected and shown in no presence, and removing
// them by name would not keep them out. Rooms are named in logs by channel id.
func (p *voicePoller) deleteRooms(ctx context.Context, rooms []store.VoiceRoom) {
	if p.s.lk == nil || len(rooms) == 0 {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, voicePollConcurrency)
	for _, room := range rooms {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := p.s.lk.DeleteRoom(ctx, room.RoomName); err != nil {
				slog.WarnContext(ctx, "voice: deleting a retired room failed; the sweep retries", "channel", room.ChannelID, "error", err)
				p.sweepSoon()
			}
		}()
	}
	wg.Wait()
}

// voiceEnforceHolders checks the invariant now and finishes what it finds: rotate
// every room with an invalid holder and delete the old rooms in LiveKit. It
// is what the four hooks call, on a context that cannot be cancelled by a
// caller who hangs up and that is bounded so a LiveKit that does not answer
// cannot hold the response. It never fails the caller's request.
func (s *Server) voiceEnforceHolders(ctx context.Context) []voiceRotation {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceEvictTimeout)
	defer cancel()
	rotated, err := s.voice.rotateInvalid(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "voice: holder check failed; the sweep retries", "error", err)
	}
	olds := make([]store.VoiceRoom, 0, len(rotated))
	for _, r := range rotated {
		olds = append(olds, r.old)
	}
	s.voice.deleteRooms(ctx, olds)
	return rotated
}

// withoutRotated is rooms minus those a rotation just retired.
func withoutRotated(rooms []store.VoiceRoom, rotated []voiceRotation) []store.VoiceRoom {
	out := make([]store.VoiceRoom, 0, len(rooms))
	for _, room := range rooms {
		if !slices.ContainsFunc(rotated, func(r voiceRotation) bool { return r.old.ID == room.ID }) {
			out = append(out, room)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Immediate hooks (members.go, disable.go, credentials.go)
//
// Each runs after the change committed and before the HTTP response is
// written, never fails the request, and runs on a context its caller cannot
// cancel. What it cannot finish (LiveKit down, a store read failing) is left
// to the sweep, which checks the same invariant for every stored room.

// voiceMemberRemoved runs after a principal's membership of channelID was
// removed: their presence sockets on the channel close, the invariant is
// checked (a removed holder rotates the room), and they are removed from any
// of the channel's rooms that were not rotated, in case they are in one
// without having been issued a session.
func (s *Server) voiceMemberRemoved(ctx context.Context, channelID, principalID int64) {
	s.voice.dropPrincipal(channelID, principalID)
	if s.lk == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	rooms, roomsErr := s.store.VoiceRoomsForChannel(ctx, channelID)
	rotated := s.voiceEnforceHolders(ctx)
	if roomsErr != nil {
		slog.ErrorContext(ctx, "voice: list rooms for removal failed; passes will remove", "channel", channelID, "error", roomsErr)
		return
	}
	s.voice.evict(ctx, withoutRotated(rooms, rotated), principalID, voiceReasonMemberRemoved)
}

// voicePrincipalLostAccess runs after a principal was disabled or had every
// credential revoked: their presence sockets close and the invariant is
// checked. A disabled principal is also removed, by name, from the rooms of
// its channels that were not rotated. reason is voiceReasonPrincipalOff or
// voiceReasonCredsRevoked.
func (s *Server) voicePrincipalLostAccess(ctx context.Context, principalID int64, reason string) {
	s.voice.dropPrincipalAll(principalID)
	if s.lk == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	var rooms []store.VoiceRoom
	var roomsErr error
	if reason == voiceReasonPrincipalOff {
		rooms, roomsErr = s.voice.memberRooms(ctx, principalID)
	}
	rotated := s.voiceEnforceHolders(ctx)
	if reason != voiceReasonPrincipalOff {
		return
	}
	if roomsErr != nil {
		// Which rooms to call LiveKit for is unknown. The principal still
		// leaves presence at once, from every room the poller has them in,
		// and the next pass removes them by entitlement.
		slog.ErrorContext(ctx, "voice: list rooms for removal failed; passes will remove", "principal", principalID, "error", roomsErr)
		s.voice.forgetEverywhere(ctx, principalID)
		return
	}
	s.voice.evict(ctx, withoutRotated(rooms, rotated), principalID, reason)
}

// voiceCredentialRevoked runs after one credential was revoked or replaced:
// if a session was issued under it, its room is rotated.
func (s *Server) voiceCredentialRevoked(ctx context.Context) {
	if s.lk == nil {
		return
	}
	s.voiceEnforceHolders(ctx)
}
