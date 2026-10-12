package main

import (
	"context"
	"fmt"
	"math"
	"time"
)

// pair is a talker and a listener hearing it, in a room of their own. The
// talker feeds its tone the whole time; its track is unmuted, or muted.
type pair struct {
	room             string
	muted            bool
	redisable        bool // the muted talker disables its track again when the SDK republishes it
	talker, listener *probe
	talkerToken      string
	minted           time.Time
	talkerFrom       int
	listenerFrom     int
}

// pairResult is what one pair saw across a freeze. Times are in seconds.
type pairResult struct {
	muted, redisable bool
	talker, listener life
	quietAfterFreeze float64 // the listener's last loud frame, after the freeze began
	loudAfterThaw    float64 // the listener's first loud frame after the thaw
	neverQuiet       bool    // the listener heard the tone throughout
	refreshed        bool    // LiveKit had sent the talker a token of its own before the freeze
	oldToken         string  // what the server did with the talker's original token afterwards

	// For a muted talker: payload bytes a second it sent before the freeze and
	// after it was connected again, what the server then said of its track,
	// and how often its listener heard loud audio begin.
	sentBefore, sentAfter float64
	serverSays            string
	loudStarts            int
}

func (r *run) startPair(muted, redisable bool) (*pair, error) {
	room, err := r.newRoom(true)
	if err != nil {
		return nil, err
	}
	p := &pair{room: room, muted: muted, redisable: redisable}
	if p.listener, err = r.joined("listener", "listen", "listener", room); err != nil {
		return nil, err
	}
	p.minted = time.Now()
	if p.talkerToken, err = r.token("talker", room); err != nil {
		return nil, err
	}
	args, heard := []string{"--start=unmuted"}, "loud_start"
	if muted {
		args, heard = []string{"--start=postmute"}, "track_subscribed"
	}
	if redisable {
		args = append(args, "--disable-on-republish")
	}
	if p.talker, err = r.start("talker", "publish", p.talkerToken, room, args...); err != nil {
		return nil, err
	}
	if _, err := p.talker.wait(0, 30*time.Second, "published"); err != nil {
		return nil, err
	}
	if _, err := p.listener.wait(0, 20*time.Second, heard); err != nil {
		return nil, err
	}
	return p, nil
}

// sendRate is the payload bytes a second each of the given talkers sends,
// over five seconds. NaN where a talker did not answer.
func sendRate(pairs []*pair) (map[*pair]float64, error) {
	const over = 5 * time.Second
	sent := func() map[*pair]float64 {
		bytes := map[*pair]float64{}
		for _, p := range pairs {
			bytes[p] = math.NaN()
			if e, err := p.talker.ask("rtp", "rtp"); err == nil {
				bytes[p] = e.num("bytes_sent")
			}
		}
		return bytes
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	before := sent()
	if err := pause(over); err != nil {
		return nil, err
	}
	after := sent()
	rate := map[*pair]float64{}
	for _, p := range pairs {
		rate[p] = (after[p] - before[p]) / over.Seconds()
	}
	return rate, nil
}

// freezeOnce freezes the server for d under n pairs with an unmuted talker,
// nMuted pairs with a muted one and as many again whose muted talker disables
// its track again when the SDK republishes it, and reports what each saw. With tokenAge
// set it first waits until every join token is that old, so that the server
// would refuse it for a new connection. With checkOld it then presents each
// talker's original token again and records the answer.
func (r *run) freezeOnce(d time.Duration, n, nMuted int, tokenAge time.Duration, checkOld bool) ([]pairResult, error) {
	var pairs, mutedPairs []*pair
	for i := 0; i < n+2*nMuted; i++ {
		p, err := r.startPair(i >= n, i >= n+nMuted)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, p)
		if p.muted {
			mutedPairs = append(mutedPairs, p)
		}
	}
	if tokenAge > 0 {
		if err := pause(time.Until(pairs[len(pairs)-1].minted.Add(tokenAge))); err != nil {
			return nil, err
		}
	} else if err := pause(2 * time.Second); err != nil {
		return nil, err
	}
	sentBefore, err := sendRate(mutedPairs)
	if err != nil {
		return nil, err
	}
	var all []*probe
	from := map[*probe]int{}
	pairOf := map[*probe]*pair{}
	for _, p := range pairs {
		p.talkerFrom, p.listenerFrom = p.talker.mark(), p.listener.mark()
		from[p.talker], from[p.listener] = p.talkerFrom, p.listenerFrom
		pairOf[p.talker], pairOf[p.listener] = p, p
		all = append(all, p.talker, p.listener)
	}
	frozen, _, err := r.srv.freeze()
	if err != nil {
		return nil, err
	}
	if err := pause(time.Until(frozen.Add(d))); err != nil {
		return nil, err
	}
	thawed, _, err := r.srv.thaw()
	if err != nil {
		return nil, err
	}
	// Wait until no connection is in the middle of reconnecting and every
	// listener of an unmuted talker hears the tone again or has been
	// disconnected; then a little longer, for anything that follows.
	if _, err := untilAll(all, 150*time.Second, func(p *probe) bool {
		reconnecting, gone := false, false
		for _, e := range p.since(from[p]) {
			switch e.Ev {
			case "reconnecting":
				reconnecting = true
			case "reconnected":
				reconnecting = false
			case "disconnected":
				reconnecting, gone = false, true
			}
		}
		if reconnecting {
			return false
		}
		if gone {
			return true
		}
		pr := pairOf[p]
		_, wasQuiet := pr.listener.first(pr.listenerFrom, func(e event) bool { return e.Ev == "loud_end" })
		// A listener that never lost the tone, or never had one, has nothing
		// to wait for once the freeze has been over for a while.
		if !wasQuiet {
			return time.Since(thawed) > 8*time.Second
		}
		_, again := pr.listener.first(pr.listenerFrom, func(e event) bool { return e.Ev == "loud_start" && e.At.After(thawed) })
		return again || pairDisconnected(pr)
	}); err != nil {
		return nil, err
	}
	if err := pause(6 * time.Second); err != nil {
		return nil, err
	}
	sentAfter, err := sendRate(mutedPairs)
	if err != nil {
		return nil, err
	}

	var results []pairResult
	for _, p := range pairs {
		res := pairResult{
			muted:            p.muted,
			redisable:        p.redisable,
			talker:           lifeOf(p.talker.since(p.talkerFrom), frozen),
			listener:         lifeOf(p.listener.since(p.listenerFrom), frozen),
			quietAfterFreeze: math.NaN(),
			loudAfterThaw:    math.NaN(),
		}
		_, res.refreshed = p.talker.first(0, func(e event) bool { return e.Ev == "token_refreshed" && e.At.Before(frozen) })
		if e, ok := p.listener.first(p.listenerFrom, func(e event) bool { return e.Ev == "loud_end" }); ok {
			res.quietAfterFreeze = msBetween(frozen, e.At) / 1000
		} else {
			res.neverQuiet = true
		}
		if e, ok := p.listener.first(p.listenerFrom, func(e event) bool { return e.Ev == "loud_start" && e.At.After(thawed) }); ok {
			res.loudAfterThaw = msBetween(thawed, e.At) / 1000
		}
		if p.muted {
			res.sentBefore, res.sentAfter = sentBefore[p], sentAfter[p]
			res.serverSays = r.serverSays(p.room, "talker")
			for _, e := range p.listener.since(p.listenerFrom) {
				if e.Ev == "loud_start" {
					res.loudStarts++
				}
			}
		}
		results = append(results, res)
	}
	if checkOld {
		for i, p := range pairs {
			if !p.muted {
				results[i].oldToken = r.presentAgain(p)
			}
		}
	}
	r.stopProbes()
	for _, p := range pairs {
		r.deleteRoom(p.room)
	}
	return results, nil
}

// serverSays is what ListParticipants answers about one participant's
// microphone track.
func (r *run) serverSays(room, identity string) string {
	ctx, cancel := context.WithTimeout(stopCtx, 5*time.Second)
	defer cancel()
	people, err := r.api.ListParticipants(ctx, room)
	if err != nil {
		return "no answer"
	}
	for _, p := range people {
		if p.Identity != identity {
			continue
		}
		switch {
		case !p.MicrophonePublished:
			return "no track"
		case p.MicrophoneMuted:
			return "muted"
		default:
			return "NOT muted"
		}
	}
	return "not in the room"
}

func pairDisconnected(p *pair) bool {
	gone := func(e event) bool { return e.Ev == "disconnected" }
	_, a := p.talker.first(p.talkerFrom, gone)
	_, b := p.listener.first(p.listenerFrom, gone)
	return a || b
}

// presentAgain starts a new connection on the talker's original join token
// and says what LiveKit did with it. (Accepting it would displace the talker,
// which is why this is the last thing done with the pair.)
func (r *run) presentAgain(p *pair) string {
	age := time.Since(p.minted).Round(10 * time.Second)
	again, err := r.start("old-token", "listen", p.talkerToken, p.room)
	if err != nil {
		return "could not be tried: " + err.Error()
	}
	e, err := again.next(0, 40*time.Second, "an answer", func(e event) bool { return e.Ev == "connected" || e.Ev == "connect_failed" })
	if err != nil {
		return "could not be tried: " + err.Error()
	}
	if e.Ev == "connected" {
		return fmt.Sprintf("ACCEPTED, about %s old", age)
	}
	return fmt.Sprintf("refused, about %s old (%s)", age, e.str("error"))
}

// measureResume is row 3: the SDK's own reconnection across a freeze of 2 s,
// 10 s and 60 s, how long it tries when the server never comes back, and
// whether the application can end it.
func (r *run) measureResume() error {
	r.say("")
	r.say("== 4: the SDK's own resume (row 3) ==")
	r.say("each pair is a talker feeding a tone and a listener, in a room of their own; the server is frozen with docker pause.")
	r.say("times are seconds after the freeze began unless they say otherwise.")
	for _, d := range []time.Duration{2 * time.Second, 10 * time.Second} {
		var results []pairResult
		for i := 0; i < r.reps; i++ {
			res, err := r.freezeOnce(d, 1, 0, 0, false)
			r.stopProbes()
			if err != nil {
				return fmt.Errorf("frozen %s: %w", d, err)
			}
			results = append(results, res...)
		}
		r.reportFreeze(fmt.Sprintf("frozen for %s (%d freezes, one pair each, talker unmuted)", d, r.reps), results)
	}
	// From here on one freeze is under several pairs at once: they share the
	// server, not their SDK state.
	results, err := r.freezeOnce(25*time.Second, r.reps, 0, 0, false)
	r.stopProbes()
	if err != nil {
		return fmt.Errorf("frozen 25s: %w", err)
	}
	r.reportFreeze(fmt.Sprintf("frozen for 25 s (one freeze, %d pairs, talker unmuted)", r.reps), results)

	// The tokens are 80 s old first, past their 15 s life and LiveKit's 60 s
	// of leeway. Two more sets of pairs have a muted talker.
	results, err = r.freezeOnce(60*time.Second, r.reps, r.reps, 80*time.Second, true)
	r.stopProbes()
	if err != nil {
		return fmt.Errorf("frozen 60s: %w", err)
	}
	var unmuted, muted, redisabled []pairResult
	var old []string
	for _, res := range results {
		switch {
		case res.redisable:
			redisabled = append(redisabled, res)
		case res.muted:
			muted = append(muted, res)
		default:
			unmuted = append(unmuted, res)
			old = append(old, res.oldToken)
		}
	}
	r.reportFreeze(fmt.Sprintf("frozen for 60 s (one freeze, %d pairs, talker unmuted; join tokens at least 80 s old when it began)", r.reps), unmuted)
	r.say("  the talker's original join token, presented again afterwards: %s", tally(old))
	r.reportMutedAcross(fmt.Sprintf("the same 60 s freeze, %d more pairs whose talker is muted (published, then muted) and feeds its tone the whole time", r.reps), muted)
	r.reportMutedAcross(fmt.Sprintf("the same 60 s freeze, %d more such pairs whose talker disables its track again when it is told LocalTrackRepublished", r.reps), redisabled)

	if err := r.giveUp(); err != nil {
		return err
	}
	return r.closeOnReconnecting()
}

func (r *run) reportFreeze(what string, results []pairResult) {
	var talkers, listeners []life
	var quiet, loud []float64
	never, refreshed := 0, 0
	for _, res := range results {
		talkers, listeners = append(talkers, res.talker), append(listeners, res.listener)
		quiet, loud = append(quiet, res.quietAfterFreeze), append(loud, res.loudAfterThaw)
		if res.neverQuiet {
			never++
		}
		if res.refreshed {
			refreshed++
		}
	}
	r.say("")
	r.say("-- %s --", what)
	r.say("  LiveKit had sent the talker a token of its own before the freeze: %d of %d", refreshed, len(results))
	r.say("  the listener's last loud frame: %s; listeners that heard the tone throughout: %d", spread(quiet, "s"), never)
	r.say("  the listener's first loud frame after the thaw: %s after the thaw", spread(loud, "s"))
	r.reportSides(talkers, listeners)
}

func (r *run) reportSides(talkers, listeners []life) {
	for _, side := range []struct {
		name  string
		lives []life
	}{{"talker", talkers}, {"listener", listeners}} {
		var orders []string
		var reconnecting, reconnected, disconnected []float64
		for _, l := range side.lives {
			orders = append(orders, l.order)
			reconnecting = append(reconnecting, l.when("reconnecting"))
			reconnected = append(reconnected, l.when("reconnected"))
			disconnected = append(disconnected, l.when("disconnected"))
		}
		r.say("  %s events: %s", side.name, tally(orders))
		for _, row := range []struct {
			name   string
			values []float64
		}{{"Reconnecting", reconnecting}, {"Reconnected", reconnected}, {"Disconnected", disconnected}} {
			if s := spread(row.values, "s"); s != "no samples" {
				r.say("    %s at %s", row.name, s)
			}
		}
	}
}

// reportMutedAcross is what became of a muted track across the SDK's own
// reconnection.
func (r *run) reportMutedAcross(what string, results []pairResult) {
	var talkers, listeners []life
	var before, after []float64
	var says []string
	loudStarts := 0
	for _, res := range results {
		talkers, listeners = append(talkers, res.talker), append(listeners, res.listener)
		before, after = append(before, res.sentBefore), append(after, res.sentAfter)
		says = append(says, res.serverSays)
		loudStarts += res.loudStarts
	}
	r.say("")
	r.say("-- %s --", what)
	r.say("  payload bytes a second the muted talker sent: before the freeze %s; after it was connected again %s", spread(before, ""), spread(after, ""))
	r.say("  ListParticipants about the talker's track afterwards: %s", tally(says))
	r.say("  times a listener heard loud audio begin, from the freeze to the end: %d", loudStarts)
	r.reportSides(talkers, listeners)
}

// giveUp freezes the server and leaves it frozen until every client has
// reported Disconnected: how long the SDK keeps trying.
func (r *run) giveUp() error {
	probes, from, err := r.clients()
	if err != nil {
		return err
	}
	frozen, _, err := r.srv.freeze()
	if err != nil {
		return err
	}
	gaveUp, err := untilAll(probes, 400*time.Second, func(p *probe) bool {
		_, gone := p.first(from[p], func(e event) bool { return e.Ev == "disconnected" })
		return gone
	})
	if err != nil {
		return err
	}
	waited := time.Since(frozen)
	thawed, _, err := r.srv.thaw()
	if err != nil {
		return err
	}
	if err := pause(15 * time.Second); err != nil {
		return err
	}
	var lives []life
	var tried []float64
	after := 0
	for _, p := range probes {
		l := lifeOf(p.since(from[p]), frozen)
		lives = append(lives, l)
		tried = append(tried, l.when("disconnected")-l.when("reconnecting"))
		if _, ok := p.first(from[p], func(e event) bool {
			_, isLife := lifeName(e)
			return isLife && e.At.After(thawed)
		}); ok {
			after++
		}
	}
	r.stopProbes()
	note := fmt.Sprintf("every client had reported Disconnected %.0f s after the freeze began; the server was then thawed", waited.Seconds())
	if !gaveUp {
		note = "NOT every client had reported Disconnected after 400 s frozen"
	}
	r.reportLives(fmt.Sprintf("frozen until the SDK gives up (%d clients, one freeze)", r.reps), lives, note, false)
	r.say("  from Reconnecting to Disconnected: %s", spread(tried, "s"))
	r.say("  clients that reported anything more in the 15 s after the thaw: %d", after)
	return nil
}

// clients connects one client per repetition, each alone in its own room,
// and returns them with the index their events start at from now.
func (r *run) clients(args ...string) ([]*probe, map[*probe]int, error) {
	var probes []*probe
	for i := 0; i < r.reps; i++ {
		room, err := r.newRoom(true)
		if err != nil {
			return nil, nil, err
		}
		p, err := r.client(room, args...)
		if err != nil {
			return nil, nil, err
		}
		probes = append(probes, p)
	}
	if err := pause(2 * time.Second); err != nil {
		return nil, nil, err
	}
	from := map[*probe]int{}
	for _, p := range probes {
		from[p] = p.mark()
	}
	return probes, from, nil
}

// closeOnReconnecting is the application ending the SDK's resume itself: each
// client calls Room::close as soon as it is told Reconnecting, while the
// server is still frozen.
func (r *run) closeOnReconnecting() error {
	probes, from, err := r.clients("--close-on-reconnecting")
	if err != nil {
		return err
	}
	frozen, _, err := r.srv.freeze()
	if err != nil {
		return err
	}
	closedAll, err := untilAll(probes, 120*time.Second, func(p *probe) bool {
		_, ok := p.first(from[p], func(e event) bool { return e.Ev == "closed" })
		return ok
	})
	if err != nil {
		return err
	}
	if _, _, err := r.srv.thaw(); err != nil {
		return err
	}
	var lives []life
	var took []float64
	var outcomes []string
	for _, p := range probes {
		lives = append(lives, lifeOf(p.since(from[p]), frozen))
		if e, ok := p.first(from[p], func(e event) bool { return e.Ev == "closed" }); ok {
			took = append(took, e.num("close_ms"))
			outcomes = append(outcomes, e.str("outcome"))
		}
	}
	r.stopProbes()
	note := "the server stayed frozen until every client had closed"
	if !closedAll {
		note = "NOT every client had closed its room after 120 s frozen"
	}
	r.reportLives(fmt.Sprintf("the application calls Room::close when it is told Reconnecting (%d clients, one freeze)", r.reps), lives, note, false)
	r.say("  Room::close returned after %s; it answered: %s", spread(took, "ms"), tally(outcomes))
	return nil
}
