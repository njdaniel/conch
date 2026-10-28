package main

import (
	"fmt"

	"github.com/njdaniel/conch/pkg/schema"
)

// sessionStates is the states one session was shown in, in the order the
// roster frames carried them, repeats collapsed.
type sessionStates []schema.SessionState

func (st sessionStates) has(want schema.SessionState) bool {
	for _, got := range st {
		if got == want {
			return true
		}
	}
	return false
}

// sessionStatesSeen is the states of session across frames, in order.
func sessionStatesSeen(frames []schema.SessionRosterV1, session int64) sessionStates {
	var out sessionStates
	for _, fr := range frames {
		sess, ok := sessionOf(fr.Sessions, session)
		if !ok {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != sess.State {
			out = append(out, sess.State)
		}
	}
	return out
}

// staleBeforeOffline reports an error unless the frames show session stale
// (with the status stale) and then offline, in that order.
func staleBeforeOffline(frames []schema.SessionRosterV1, session int64) error {
	stale, offline := -1, -1
	for i, fr := range frames {
		sess, ok := sessionOf(fr.Sessions, session)
		if !ok {
			continue
		}
		switch sess.State {
		case schema.SessionStateStale:
			if sess.Status != schema.SessionStatusStale {
				return fmt.Errorf("the stale session shows the status %q", sess.Status)
			}
			if stale < 0 {
				stale = i
			}
		case schema.SessionStateOffline:
			if offline < 0 {
				offline = i
			}
		}
	}
	switch {
	case offline < 0:
		return fmt.Errorf("session #%d was never shown offline", session)
	case stale < 0 || stale > offline:
		return fmt.Errorf("the session went offline without being seen stale")
	}
	return nil
}
