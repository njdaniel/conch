package server

import (
	"context"
	"fmt"
)

// Restore and media (issue #716). conchd restore ends every sign-in a backup
// holds, but LiveKit keeps its own state: a participant who joined before
// the restore, under a session the restored database no longer knows or no
// longer allows, stays in its room until something removes it. So the first
// start on restored data deletes every LiveKit room of this instance, before
// it accepts traffic, and people join again through conchd as they sign in.

// ClearVoiceRooms deletes every room LiveKit lists whose name is exactly the
// shape this instance gives its rooms (store.IsOwnVoiceRoomName: its prefix
// and the random part), which ends every media session in them, and returns
// how many it deleted. Rooms of other instances on the same LiveKit are left
// alone, even one whose prefix begins with this one's. With
// voice not configured it does nothing. It stops at the first refusal and
// returns it, with the count so far: the caller decides what then, since
// LiveKit is optional and conchd starts without it.
func (s *Server) ClearVoiceRooms(ctx context.Context) (int, error) {
	if s.lk == nil {
		return 0, nil
	}
	rooms, err := s.lk.ListRooms(ctx)
	if err != nil {
		return 0, fmt.Errorf("voice: list rooms: %w", err)
	}
	n := 0
	for _, r := range rooms {
		if !s.store.IsOwnVoiceRoomName(r.Name) {
			continue
		}
		if err := s.lk.DeleteRoom(ctx, r.Name); err != nil {
			return n, fmt.Errorf("voice: delete a room: %w", err)
		}
		n++
	}
	return n, nil
}
