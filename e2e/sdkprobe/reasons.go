package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// lifeName is the name of an event that says something about the connection's
// life, with its detail; ok is false for every other event.
func lifeName(e event) (name string, ok bool) {
	switch e.Ev {
	case "reconnecting", "reconnected", "events_closed", "local_track_republished",
		"participant_connected", "participant_disconnected", "track_subscribed", "track_unsubscribed":
		return e.Ev, true
	case "disconnected":
		return "disconnected(" + e.str("reason") + ")", true
	case "connection_state":
		return "state=" + e.str("state"), true
	}
	return "", false
}

// life is what the application saw of its connection: the events in order,
// and when the first of each kind came, in seconds after zero.
type life struct {
	order string
	at    map[string]float64
}

func lifeOf(events []event, zero time.Time) life {
	l := life{at: map[string]float64{}}
	var names []string
	for _, e := range events {
		name, ok := lifeName(e)
		if !ok {
			continue
		}
		names = append(names, name)
		if _, seen := l.at[e.Ev]; !seen {
			l.at[e.Ev] = msBetween(zero, e.At) / 1000
		}
	}
	if len(names) == 0 {
		l.order = "(no event)"
	} else {
		l.order = strings.Join(names, " > ")
	}
	return l
}

func (l life) when(ev string) float64 {
	if t, ok := l.at[ev]; ok {
		return t
	}
	return math.NaN()
}

// settled reports whether the probe's connection has come to rest since
// index from: it reconnected or was disconnected, and is not reconnecting.
func settled(p *probe, from int) bool {
	last := ""
	for _, e := range p.since(from) {
		switch e.Ev {
		case "reconnecting", "reconnected", "disconnected":
			last = e.Ev
		}
	}
	return last == "reconnected" || last == "disconnected"
}

// untilAll waits until ok holds for every probe, or the time runs out; it
// reports whether it held.
func untilAll(probes []*probe, within time.Duration, ok func(*probe) bool) (bool, error) {
	deadline := time.Now().Add(within)
	for {
		all := true
		for _, p := range probes {
			all = all && ok(p)
		}
		if all || time.Now().After(deadline) {
			return all, nil
		}
		if err := pause(100 * time.Millisecond); err != nil {
			return false, err
		}
	}
}

// measureReasons is row 2: what the application is told, and when, for each
// way of losing the connection.
func (r *run) measureReasons() error {
	r.say("")
	r.say("== 3: what the SDK reports when the connection is lost (row 2) ==")
	r.say("each client is connected with a muted microphone track published, alone in a room of its own; %d per case.", r.reps)
	r.say("\"after\" is from the moment the cause was set off to the Disconnected event.")

	type byCall struct {
		what string
		do   func(ctx context.Context, room string) error
	}
	for _, c := range []byCall{
		{"the room is deleted (DeleteRoom)", func(ctx context.Context, room string) error { return r.api.DeleteRoom(ctx, room) }},
		{"the participant is removed (RemoveParticipant)", func(ctx context.Context, room string) error {
			return r.api.RemoveParticipant(ctx, room, "client")
		}},
	} {
		var lives []life
		for i := 0; i < r.reps; i++ {
			l, err := r.lostByCall(c.do)
			r.stopProbes()
			if err != nil {
				return fmt.Errorf("%s: %w", c.what, err)
			}
			lives = append(lives, l)
		}
		r.reportLives(c.what, lives, "", true)
	}

	var lives, seconds []life
	for i := 0; i < r.reps; i++ {
		first, second, err := r.lostToDuplicate()
		r.stopProbes()
		if err != nil {
			return fmt.Errorf("duplicate identity: %w", err)
		}
		lives, seconds = append(lives, first), append(seconds, second)
	}
	r.reportLives("a second connection joins with the same identity", lives, "after is from the second connection's Room::connect call", true)
	r.reportLives("  (that second connection, for 5 s after it connected)", seconds, "", true)

	frozen, note, err := r.lostToOutage(func() (time.Time, error) {
		called, _, err := r.srv.freeze()
		if err != nil {
			return called, err
		}
		if err := pause(90 * time.Second); err != nil {
			return called, err
		}
		_, _, err = r.srv.thaw()
		return called, err
	}, 120*time.Second)
	if err != nil {
		return fmt.Errorf("frozen 90 s: %w", err)
	}
	r.reportLives("the server is frozen for 90 s, then runs again (docker pause, unpause)", frozen, note+"; after is from the freeze; the thaw is at 90 s", false)

	var aside string
	shut, note, err := r.lostToOutage(func() (time.Time, error) {
		called, took, err := r.srv.shutDown()
		if err == nil {
			aside = fmt.Sprintf("LiveKit exited %.1f s after the first of the two signals; ", took.Seconds())
		}
		return called, err
	}, 240*time.Second)
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	r.reportLives("the server shuts down at once and stays down (SIGTERM twice: LiveKit's forced shutdown)", shut, aside+note, false)
	if err := r.srv.restart(); err != nil {
		return err
	}

	stopped, note, err := r.lostToOutage(func() (time.Time, error) {
		called, returned, err := r.srv.stop()
		if err == nil {
			aside = fmt.Sprintf("docker stop returned after %.1f s; ", returned.Sub(called).Seconds())
		}
		return called, err
	}, 240*time.Second)
	if err != nil {
		return fmt.Errorf("docker stop: %w", err)
	}
	r.reportLives("the server process is stopped and stays down (docker stop: one SIGTERM, then SIGKILL after 10 s)", stopped, aside+note, false)
	if err := r.srv.restart(); err != nil {
		return err
	}

	killed, note, err := r.lostToOutage(func() (time.Time, error) {
		called, _, err := r.srv.kill()
		return called, err
	}, 240*time.Second)
	if err != nil {
		return fmt.Errorf("docker kill: %w", err)
	}
	r.reportLives("the server process is killed and stays down (docker kill: SIGKILL, so it says nothing first)", killed, note, false)
	return r.srv.restart()
}

// client starts one client as the voice client would be: connected, its
// microphone track published muted.
func (r *run) client(room string, args ...string) (*probe, error) {
	return r.joined("client", "duplex", "client", room, append([]string{"--start=premute-disable"}, args...)...)
}

// lostByCall connects a client, makes one room-API call, and reports what the
// client saw in the five seconds after.
func (r *run) lostByCall(do func(ctx context.Context, room string) error) (life, error) {
	room, err := r.newRoom(true)
	if err != nil {
		return life{}, err
	}
	defer r.deleteRoom(room)
	p, err := r.client(room)
	if err != nil {
		return life{}, err
	}
	if err := pause(time.Second); err != nil {
		return life{}, err
	}
	from := p.mark()
	ctx, cancel := context.WithTimeout(stopCtx, 5*time.Second)
	defer cancel()
	called := time.Now()
	if err := do(ctx, room); err != nil {
		return life{}, err
	}
	if _, err := untilAll([]*probe{p}, 30*time.Second, func(p *probe) bool { return settled(p, from) }); err != nil {
		return life{}, err
	}
	if err := pause(5 * time.Second); err != nil {
		return life{}, err
	}
	return lifeOf(p.since(from), called), nil
}

// lostToDuplicate connects a client, then a second one with the same
// identity, and reports what each saw.
func (r *run) lostToDuplicate() (first, second life, err error) {
	room, err := r.newRoom(true)
	if err != nil {
		return first, second, err
	}
	defer r.deleteRoom(room)
	a, err := r.client(room)
	if err != nil {
		return first, second, err
	}
	if err := pause(time.Second); err != nil {
		return first, second, err
	}
	from := a.mark()
	b, err := r.client(room)
	if err != nil {
		return first, second, err
	}
	call, _ := b.first(0, func(e event) bool { return e.Ev == "connect_call" })
	connected, _ := b.first(0, func(e event) bool { return e.Ev == "connected" })
	if _, err := untilAll([]*probe{a}, 30*time.Second, func(p *probe) bool { return settled(p, from) }); err != nil {
		return first, second, err
	}
	if err := pause(5 * time.Second); err != nil {
		return first, second, err
	}
	return lifeOf(a.since(from), call.At), lifeOf(b.since(0), connected.At), nil
}

// lostToOutage connects one client per repetition, each in its own room, sets
// off an outage of the whole server, and reports what each client saw until
// its connection came to rest (or the time ran out) and five seconds more.
func (r *run) lostToOutage(outage func() (time.Time, error), within time.Duration) ([]life, string, error) {
	probes, index, err := r.clients()
	if err != nil {
		return nil, "", err
	}
	zero, err := outage()
	if err != nil {
		return nil, "", err
	}
	rested, err := untilAll(probes, within, func(p *probe) bool { return settled(p, index[p]) })
	if err != nil {
		return nil, "", err
	}
	if err := pause(5 * time.Second); err != nil {
		return nil, "", err
	}
	note := "every connection came to rest"
	if !rested {
		note = fmt.Sprintf("NOT every connection had come to rest %s after the outage began", within)
	}
	lives := make([]life, len(probes))
	for i, p := range probes {
		lives[i] = lifeOf(p.since(index[p]), zero)
	}
	r.stopProbes()
	return lives, note, nil
}

// reportLives prints what a set of clients saw. With inMS the times are short
// and printed in milliseconds.
func (r *run) reportLives(what string, lives []life, note string, inMS bool) {
	var orders []string
	var reconnecting, reconnected, disconnected []float64
	scale, unit := 1.0, "s"
	if inMS {
		scale, unit = 1000, "ms"
	}
	for _, l := range lives {
		orders = append(orders, l.order)
		reconnecting = append(reconnecting, l.when("reconnecting")*scale)
		reconnected = append(reconnected, l.when("reconnected")*scale)
		disconnected = append(disconnected, l.when("disconnected")*scale)
	}
	r.say("")
	r.say("-- %s --", what)
	if note != "" {
		r.say("  %s", note)
	}
	r.say("  events seen: %s", tally(orders))
	for _, row := range []struct {
		name   string
		values []float64
	}{{"Reconnecting", reconnecting}, {"Reconnected", reconnected}, {"Disconnected", disconnected}} {
		if s := spread(row.values, unit); s != "no samples" {
			r.say("  %s after %s", row.name, s)
		}
	}
}
