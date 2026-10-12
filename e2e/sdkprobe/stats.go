package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// msBetween is the time from a to b in milliseconds.
func msBetween(a, b time.Time) float64 {
	return float64(b.Sub(a)) / float64(time.Millisecond)
}

// spread is "min / median / max unit (n=N)" of the values that are numbers.
func spread(values []float64, unit string) string {
	var xs []float64
	for _, v := range values {
		if !math.IsNaN(v) {
			xs = append(xs, v)
		}
	}
	if len(xs) == 0 {
		return "no samples"
	}
	sort.Float64s(xs)
	median := xs[len(xs)/2]
	if len(xs)%2 == 0 {
		median = (xs[len(xs)/2-1] + xs[len(xs)/2]) / 2
	}
	if unit != "" {
		unit = " " + unit
	}
	return fmt.Sprintf("%s / %s / %s%s (n=%d)", short(xs[0]), short(median), short(xs[len(xs)-1]), unit, len(xs))
}

// short prints a number with no more digits than it deserves.
func short(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e9 {
		return fmt.Sprintf("%.0f", v)
	}
	switch a := math.Abs(v); {
	case a >= 100:
		return fmt.Sprintf("%.0f", v)
	case a >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

// tally counts how often each string occurs and prints "3x a; 2x b".
func tally(items []string) string {
	counts := map[string]int{}
	var order []string
	for _, item := range items {
		if counts[item] == 0 {
			order = append(order, item)
		}
		counts[item]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	parts := make([]string, 0, len(order))
	for _, item := range order {
		parts = append(parts, fmt.Sprintf("%dx %s", counts[item], item))
	}
	return strings.Join(parts, "; ")
}

// sighting is one answer from ListParticipants about one participant.
type sighting struct {
	asked, answered time.Time
	there           bool // the participant is in the room
	published       bool // with a microphone track
	transmitting    bool // that is not muted
}

// watcher asks LiveKit, over and over, what it says about one participant:
// the server's side of a mute or an unmute.
type watcher struct {
	stop chan struct{}
	done chan struct{}

	mu   sync.Mutex
	seen []sighting
}

// watch starts asking about identity in room every interval.
func (r *run) watch(room, identity string, every time.Duration) *watcher {
	w := &watcher{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			s := sighting{asked: time.Now()}
			ctx, cancel := context.WithTimeout(stopCtx, 2*time.Second)
			people, err := r.api.ListParticipants(ctx, room)
			cancel()
			s.answered = time.Now()
			if err == nil {
				for _, p := range people {
					if p.Identity == identity {
						s.there, s.published, s.transmitting = true, p.MicrophonePublished, p.Transmitting()
					}
				}
				w.mu.Lock()
				w.seen = append(w.seen, s)
				w.mu.Unlock()
			}
			select {
			case <-w.stop:
				return
			case <-stopCtx.Done():
				return
			case <-time.After(every):
			}
		}
	}()
	return w
}

func (w *watcher) end() []sighting {
	close(w.stop)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// shownBy is how long after t the server first answered with the wanted
// transmitting state, in milliseconds: the time the answer arrived, so an
// upper bound. NaN when it never did before until.
func shownBy(seen []sighting, t, until time.Time, transmitting bool) float64 {
	for _, s := range seen {
		if s.asked.Before(t) {
			continue
		}
		if !until.IsZero() && s.asked.After(until) {
			break
		}
		if s.published && s.transmitting == transmitting {
			return msBetween(t, s.answered)
		}
	}
	return math.NaN()
}
