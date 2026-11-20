package server

import (
	"context"
	"errors"
	"hash/crc32"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

type privateCallMediaService interface {
	CreateRoom(context.Context, string) error
	JoinToken(livekit.JoinParams) (string, error)
	ListParticipantsIfRoomExists(context.Context, string) ([]livekit.Participant, error)
	RemoveParticipant(context.Context, string, string) error
	DeleteRoom(context.Context, string) error
}

func (p *voicePoller) privateCallMedia() privateCallMediaService {
	if p.callMedia != nil {
		return p.callMedia
	}
	if p.s.lk != nil {
		return p.s.lk
	}
	return nil
}

func (s *Server) handlePrivateCallSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := privateCallActor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("call")
	if len(id) < schema.MinPrivateCallIDBytes || len(id) > schema.MaxPrivateCallIDBytes || !privateCallToken.MatchString(id) {
		writePrivateCallResult(w, r, store.PrivateCallResult{}, store.ErrPrivateCallNotFound)
		return
	}
	ctx := r.Context()
	media := s.voice.privateCallMedia()
	name, out, err := s.store.PreparePrivateCallMedia(ctx, id, actor, s.now, media != nil)
	if !privateCallMediaAllowed(w, r, out, err) {
		return
	}
	if media == nil {
		writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceNotConfigured, "voice is not configured")
		return
	}
	if err := s.voice.createPrivateCallRoom(ctx, name); err != nil {
		if errors.Is(err, livekit.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, "voice is temporarily unavailable")
		} else {
			writePrivateCallResult(w, r, store.PrivateCallResult{}, err)
		}
		return
	}
	session := schema.PrivateCallSessionV1{
		Schema: schema.PrivateCallSessionSchemaV1, CallID: id, Room: name,
		LivekitURL: s.cfg.LiveKit.URL, Identity: voiceIdentity(actor.PrincipalID),
	}
	out, err = s.store.GrantPrivateCallMedia(ctx, id, actor, s.now, name, func(revision int64) error {
		session.Revision = revision
		// Match the channel grant's rounding. Take the time before signing
		// so the reported expiry is never later than the signed expiry.
		session.ExpiresAt = schema.NewTimestamp(time.Now().Add(voiceTokenLifetime).Truncate(time.Second))
		token, err := media.JoinToken(livekit.JoinParams{
			Room: name, Identity: session.Identity, CanPublish: true, Lifetime: voiceTokenLifetime,
		})
		if err != nil {
			return errors.New("private call: could not sign media grant")
		}
		session.Token = token
		return session.Validate()
	})
	if privateCallMediaAllowed(w, r, out, err) {
		writeJSON(w, http.StatusOK, session)
	}
}

// Hold the same lock as deletion across the store check and the CreateRoom
// RPC. A create already in flight finishes before deletion. A request waiting
// behind deletion sees the retirement and cannot provision the old name.
func (p *voicePoller) createPrivateCallRoom(ctx context.Context, name string) error {
	defer p.lockPrivateCallRoom(name)()
	live, err := p.s.store.PrivateCallMediaRoomLive(ctx, name)
	if err != nil || !live {
		return err
	}
	if err := p.privateCallMedia().CreateRoom(ctx, name); err != nil {
		return livekit.ErrUnavailable
	}
	return nil
}

func (p *voicePoller) lockPrivateCallRoom(name string) func() {
	lock := &p.callRoomLocks[crc32.ChecksumIEEE([]byte(name))%uint32(len(p.callRoomLocks))]
	lock.Lock()
	return lock.Unlock
}

func privateCallMediaAllowed(w http.ResponseWriter, r *http.Request, out store.PrivateCallResult, err error) bool {
	if err != nil {
		writePrivateCallResult(w, r, out, err)
		return false
	}
	if out.Code == schema.ErrorCodeCallWrongDevice {
		writeError(w, http.StatusForbidden, out.Code, "join on the selected device")
		return false
	}
	if out.Code != "" {
		writePrivateCallCommand(w, out)
		return false
	}
	return true
}

const privateCallMediaInterval = time.Second

// Private call media has its own pass within the voice poller. Channel voice
// backoff and its presence caches do not govern private calls.
func (p *voicePoller) loopPrivateCallMedia(ctx context.Context) {
	ticker := time.NewTicker(privateCallMediaInterval)
	defer ticker.Stop()
	for {
		p.runPrivateCallMediaPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *voicePoller) runPrivateCallMediaPass(ctx context.Context) {
	rooms, err := p.s.store.PrivateCallMediaRooms(ctx)
	if err == nil {
		err = runPrivateCallRoomJobs(ctx, rooms, p.reconcilePrivateCallRoom)
	}
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "private call media pass failed; retrying", "error", err)
	}
}

func (p *voicePoller) reconcilePrivateCallRoom(ctx context.Context, room store.PrivateCallMediaRoom) error {
	if room.Retired {
		return p.deletePrivateCallRoom(ctx, room)
	}
	media := p.privateCallMedia()
	parts, err := media.ListParticipantsIfRoomExists(ctx, room.Name)
	if err != nil {
		return err
	}
	holders, err := p.s.store.PrivateCallMediaHolders(ctx, room.CallID)
	if err != nil {
		return err
	}
	callerHolder, calleeHolder := holders[room.CallerID], holders[room.CalleeID]
	var caller, callee int64
	for _, part := range parts {
		switch {
		case part.CameraGranted || part.ShareSourcesGranted || len(part.Cameras) > 0 ||
			part.ScreenSharePublished || part.ScreenShareAudioPublished || len(part.Mismatched) > 0:
			// LiveKit checks the declared source. A client can label a
			// video track as microphone, so also enforce the track kind.
			err = errors.Join(err, media.RemoveParticipant(ctx, room.Name, part.Identity))
		case callerHolder != 0 && part.Identity == voiceIdentity(room.CallerID):
			caller = callerHolder
		case calleeHolder != 0 && part.Identity == voiceIdentity(room.CalleeID):
			callee = calleeHolder
		default:
			err = errors.Join(err, media.RemoveParticipant(ctx, room.Name, part.Identity))
		}
	}
	return errors.Join(err, p.s.store.ObservePrivateCallEndpoints(ctx, room.CallID, room.Revision, caller, callee, p.s.now))
}

// DeleteRoom disconnects both endpoints together. Only a confirmed delete
// clears the pending work. The name stays retired even after confirmation.
func (p *voicePoller) deletePrivateCallRoom(ctx context.Context, room store.PrivateCallMediaRoom) error {
	defer p.lockPrivateCallRoom(room.Name)()
	if err := p.privateCallMedia().DeleteRoom(ctx, room.Name); err != nil {
		return err
	}
	return p.s.store.MarkPrivateCallMediaDeleted(ctx, room.Name)
}

func runPrivateCallRoomJobs(ctx context.Context, rooms []store.PrivateCallMediaRoom, job func(context.Context, store.PrivateCallMediaRoom) error) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, voicePollConcurrency)
	for _, room := range rooms {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(ctx, voiceEvictTimeout)
			defer cancel()
			if err := job(ctx, room); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return firstErr
}

// privateCallRoomStates takes its snapshot after ListRooms. Room names are
// committed before CreateRoom, so a concurrent join cannot look like an orphan.
func (p *voicePoller) privateCallRoomStates(ctx context.Context, listed []livekit.Room) (map[string]bool, error) {
	names := make([]string, 0, len(listed))
	for _, room := range listed {
		names = append(names, room.Name)
	}
	return p.s.store.PrivateCallRoomStates(ctx, names)
}

func (p *voicePoller) sweepRetiredPrivateCallRooms(ctx context.Context, states map[string]bool) {
	var rooms []store.PrivateCallMediaRoom
	for name, retired := range states {
		if retired {
			rooms = append(rooms, store.PrivateCallMediaRoom{Name: name, Retired: true})
		}
	}
	if err := runPrivateCallRoomJobs(ctx, rooms, p.deletePrivateCallRoom); err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "private call room sweep failed; retrying", "error", err)
		p.sweepSoon()
	}
}
