package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	// mutedWindow is how long the listener counts frames before the first
	// press, with the track muted.
	mutedWindow = 10 * time.Second
	// presses is the number of unmute/mute cycles in one session.
	presses = 6
)

// muteVariant is one way of having a track published and muted.
type muteVariant struct {
	what string
	args []string
}

var muteVariants = []muteVariant{
	{"A: muted before publishing (track.mute(), then publish_track); tone fed the whole time", []string{"--start=premute"}},
	{"B: as A, and the track disabled again after publish_track returns; tone fed the whole time", []string{"--start=premute-disable"}},
	{"C: published, then muted with the next statement; tone fed the whole time", []string{"--start=postmute"}},
	{"D: as A, but frames are handed to the source only between unmute and mute, as a client with a transmit gate does", []string{"--start=premute", "--gated"}},
}

// press is one unmute/mute cycle, in milliseconds. NaN is "not seen".
type press struct {
	unmuteToLoud   float64 // unmute call -> first loud frame at the listener
	unmuteToServer float64 // unmute call -> ListParticipants answers "not muted"
	pipeline       float64 // a louder burst handed to the source -> heard by the listener
	muteToQuiet    float64 // mute call -> last loud frame at the listener
	muteToServer   float64 // mute call -> ListParticipants answers "muted"
	callReturned   float64 // how long the unmute call itself took
	dropouts       int     // times the audio went quiet and came back within the press
	alreadyLoud    bool    // the listener was hearing the tone before the unmute
}

// muteSession is one publisher and one listener in a room of their own.
type muteSession struct {
	connectMS, publishMS float64
	subscribedMuted      bool // the listener was told the track is muted when it subscribed
	rtcEnabledMuted      bool // when publish_track returned, the WebRTC track was enabled although the track was muted
	serverUnmutedEarly   int  // ListParticipants answers "not muted" before the first press
	serverAnswersEarly   int
	windowFrames         float64 // frames the listener got in the muted window
	windowLoud           float64 // of them, loud
	windowNonzero        float64 // of them, with any sample not zero
	windowQuietRMS       float64 // the loudest of them
	windowFed            float64 // frames the talker handed its audio source in the window
	windowPackets        float64 // RTP packets the talker sent in the window
	windowBytes          float64 // and their payload bytes
	loudBeforePress      float64 // loud frames from subscribing to the first press
	loudBetween          int     // times loud audio started between a mute and the next unmute
	pressPackets         float64 // RTP packets the talker sent over the presses, for comparison
	pressBytes           float64
	presses              []press
}

// measureMute is rows 1 and 12: a track published muted, then unmuted and
// muted as push-to-talk would, with a listener counting what arrives. The
// variants run side by side, each in rooms of its own.
func (r *run) measureMute() error {
	r.say("")
	r.say("== 1 and 2: publish muted, unmute, mute; what is heard while muted (rows 1 and 12) ==")
	r.say("each session: one talker with a 440 Hz tone for its audio source and one listener, in a room of their own;")
	r.say("%s muted, then %d presses (unmuted 1.5 s, muted 1.5 s). %d sessions for each of %d ways of doing it, the ways side by side.",
		mutedWindow, presses, r.reps, len(muteVariants))
	results := make([][]muteSession, len(muteVariants))
	errs := make([]error, len(muteVariants))
	var wg sync.WaitGroup
	for i, v := range muteVariants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < r.reps; n++ {
				s, err := r.muteSession(v.args)
				if err != nil {
					errs[i] = fmt.Errorf("%s, session %d: %w", v.what[:1], n+1, err)
					return
				}
				results[i] = append(results[i], s)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	for i, v := range muteVariants {
		r.reportMute(v.what, results[i])
	}
	return nil
}

func (r *run) muteSession(args []string) (s muteSession, err error) {
	room, err := r.newRoom(true)
	if err != nil {
		return s, err
	}
	defer r.deleteRoom(room)
	listener, err := r.joined("listener", "listen", "listener", room)
	if err != nil {
		return s, err
	}
	defer listener.quit()
	early := r.watch(room, "talker", 20*time.Millisecond)
	watching := early
	defer func() {
		if watching != nil {
			watching.end()
		}
	}()
	talker, err := r.joined("talker", "publish", "talker", room, args...)
	if err != nil {
		return s, err
	}
	defer talker.quit()
	if e, ok := talker.first(0, func(e event) bool { return e.Ev == "connected" }); ok {
		s.connectMS = e.num("connect_ms")
	}
	published, _ := talker.first(0, func(e event) bool { return e.Ev == "published" })
	s.publishMS = published.num("publish_ms")
	s.rtcEnabledMuted = published.flag("rtc_enabled") && published.flag("muted")
	subscribed, err := listener.wait(0, 15*time.Second, "track_subscribed")
	if err != nil {
		return s, err
	}
	s.subscribedMuted = subscribed.flag("muted")

	// Row 12: the muted window.
	before, err := listener.ask("audio", "audio")
	if err != nil {
		return s, err
	}
	sentBefore, err := talker.ask("rtp", "rtp")
	if err != nil {
		return s, err
	}
	fedBefore, err := talker.ask("audio", "audio")
	if err != nil {
		return s, err
	}
	if err := pause(mutedWindow); err != nil {
		return s, err
	}
	after, err := listener.ask("audio", "audio")
	if err != nil {
		return s, err
	}
	sentAfter, err := talker.ask("rtp", "rtp")
	if err != nil {
		return s, err
	}
	fedAfter, err := talker.ask("audio", "audio")
	if err != nil {
		return s, err
	}
	watching = nil
	for _, sighting := range early.end() {
		if !sighting.published {
			continue
		}
		s.serverAnswersEarly++
		if sighting.transmitting {
			s.serverUnmutedEarly++
		}
	}
	a, b := heardFrom(before, "talker"), heardFrom(after, "talker")
	s.windowFrames, s.windowLoud, s.windowNonzero = b.frames-a.frames, b.loud-a.loud, b.nonzero-a.nonzero
	s.windowQuietRMS = b.maxQuietRMS
	s.loudBeforePress = b.loud
	s.windowFed = fedAfter.num("fed") - fedBefore.num("fed")
	s.windowPackets = sentAfter.num("packets_sent") - sentBefore.num("packets_sent")
	s.windowBytes = sentAfter.num("bytes_sent") - sentBefore.num("bytes_sent")

	// Row 1: the presses.
	server := r.watch(room, "talker", 5*time.Millisecond)
	watching = server
	type cycle struct {
		from                  int // the listener's events from here on belong to this press
		unmute, mute, markFed event
	}
	var cycles []cycle
	for i := 0; i < presses; i++ {
		c := cycle{from: listener.mark()}
		if c.unmute, err = talker.ask("unmute", "unmute_call"); err != nil {
			return s, err
		}
		if err := pause(700 * time.Millisecond); err != nil {
			return s, err
		}
		if c.markFed, err = talker.ask("mark", "mark_fed"); err != nil {
			return s, err
		}
		if err := pause(800 * time.Millisecond); err != nil {
			return s, err
		}
		if c.mute, err = talker.ask("mute", "mute_call"); err != nil {
			return s, err
		}
		if err := pause(1500 * time.Millisecond); err != nil {
			return s, err
		}
		cycles = append(cycles, c)
	}
	end := listener.mark()
	watching = nil
	seen := server.end()
	if sentEnd, err := talker.ask("rtp", "rtp"); err == nil {
		s.pressPackets = sentEnd.num("packets_sent") - sentAfter.num("packets_sent")
		s.pressBytes = sentEnd.num("bytes_sent") - sentAfter.num("bytes_sent")
	}

	for i, c := range cycles {
		to := end
		var nextUnmute time.Time
		if i+1 < len(cycles) {
			to = cycles[i+1].from
			nextUnmute = cycles[i+1].unmute.At
		}
		p := press{
			unmuteToLoud: math.NaN(), pipeline: math.NaN(), muteToQuiet: math.NaN(),
			unmuteToServer: shownBy(seen, c.unmute.At, c.mute.At, true),
			muteToServer:   shownBy(seen, c.mute.At, nextUnmute, false),
			callReturned:   c.unmute.num("returned_after_ms"),
		}
		starts := 0
		for _, e := range listener.since(c.from)[:to-c.from] {
			switch e.Ev {
			case "loud_start":
				if e.At.Before(c.unmute.At) || e.At.After(c.mute.At.Add(time.Second)) {
					// Loud audio began outside this press.
					s.loudBetween++
					continue
				}
				starts++
				if starts == 1 {
					p.unmuteToLoud = msBetween(c.unmute.At, e.At)
				}
			case "mark_heard":
				if math.IsNaN(p.pipeline) && e.At.After(c.markFed.At) {
					p.pipeline = msBetween(c.markFed.At, e.At)
				}
			case "loud_end":
				p.muteToQuiet = msBetween(c.mute.At, e.At)
			}
		}
		if starts == 0 {
			// Nothing started: either nothing was heard, or it was already loud.
			p.alreadyLoud = !math.IsNaN(p.muteToQuiet)
		} else {
			p.dropouts = starts - 1
		}
		s.presses = append(s.presses, p)
	}
	return s, nil
}

// heard is one speaker's line of a probe's "audio" event.
type heard struct{ frames, loud, nonzero, maxQuietRMS float64 }

func heardFrom(e event, speaker string) heard {
	speakers, _ := e.Fields["speakers"].([]any)
	for _, raw := range speakers {
		m, _ := raw.(map[string]any)
		if m["speaker"] != speaker {
			continue
		}
		num := func(key string) float64 {
			f, _ := m[key].(float64)
			return f
		}
		return heard{frames: num("frames"), loud: num("loud"), nonzero: num("nonzero"), maxQuietRMS: num("max_quiet_rms")}
	}
	return heard{}
}

func (r *run) deleteRoom(room string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.api.DeleteRoom(ctx, room)
}

func (r *run) reportMute(what string, sessions []muteSession) {
	var connect, publish, first, later, firstServer, laterServer, pipeline, quiet, quietServer, returned []float64
	var laterLost, tailLost []float64
	var frames, loud, nonzero, loudBefore, quietRMS, fed, packets, bytes, pressBytes []float64
	subscribedMuted, rtcEnabled, earlyUnmuted, earlyAnswers, between, dropouts, alreadyLoud := 0, 0, 0, 0, 0, 0, 0
	for _, s := range sessions {
		connect = append(connect, s.connectMS)
		publish = append(publish, s.publishMS)
		frames = append(frames, s.windowFrames)
		loud = append(loud, s.windowLoud)
		nonzero = append(nonzero, s.windowNonzero)
		loudBefore = append(loudBefore, s.loudBeforePress)
		quietRMS = append(quietRMS, s.windowQuietRMS)
		fed = append(fed, s.windowFed)
		packets = append(packets, s.windowPackets)
		bytes = append(bytes, s.windowBytes)
		// The presses are unmuted for 1.5 s of every 3 s: this is bytes per
		// second of unmuted time, to set beside the muted window's.
		pressBytes = append(pressBytes, s.pressBytes/(presses*1.5))
		if s.subscribedMuted {
			subscribedMuted++
		}
		if s.rtcEnabledMuted {
			rtcEnabled++
		}
		earlyUnmuted += s.serverUnmutedEarly
		earlyAnswers += s.serverAnswersEarly
		between += s.loudBetween
		for i, p := range s.presses {
			dropouts += p.dropouts
			if p.alreadyLoud {
				alreadyLoud++
			}
			if i == 0 {
				first = append(first, p.unmuteToLoud)
				firstServer = append(firstServer, p.unmuteToServer)
			} else {
				later = append(later, p.unmuteToLoud)
				laterServer = append(laterServer, p.unmuteToServer)
				laterLost = append(laterLost, p.unmuteToLoud-p.pipeline)
			}
			pipeline = append(pipeline, p.pipeline)
			quiet = append(quiet, p.muteToQuiet)
			quietServer = append(quietServer, p.muteToServer)
			tailLost = append(tailLost, p.pipeline-p.muteToQuiet)
			returned = append(returned, p.callReturned)
		}
	}
	n := len(sessions)
	perSecond := make([]float64, len(bytes))
	for i, b := range bytes {
		perSecond[i] = b / mutedWindow.Seconds()
	}
	r.say("")
	r.say("-- %s --", what)
	r.say("  Room::connect: %s; publish_track: %s", spread(connect, "ms"), spread(publish, "ms"))
	r.say("  when publish_track returned, the track was muted and its WebRTC track enabled: %d of %d sessions", rtcEnabled, n)
	r.say("  the listener was told \"muted\" when it subscribed: %d of %d sessions", subscribedMuted, n)
	r.say("  ListParticipants before the first press: %d answers with the track published, %d of them \"not muted\"", earlyAnswers, earlyUnmuted)
	r.say("  [row 12] in the %s muted window the talker handed its source %s frames", mutedWindow, spread(fed, ""))
	r.say("           and sent %s RTP packets, %s payload bytes a second (unmuted, over the presses: %s)", spread(packets, ""), spread(perSecond, ""), spread(pressBytes, ""))
	r.say("           the listener received %s frames; loud: %s; with any nonzero sample: %s; loudest RMS %s", spread(frames, ""), spread(loud, ""), spread(nonzero, ""), spread(quietRMS, ""))
	r.say("           loud frames from subscribing to the first press: %s", spread(loudBefore, ""))
	r.say("           loud audio starting between a mute and the next unmute: %d times in %d presses", between, n*presses)
	r.say("  [row 1] unmute call -> first loud frame at the listener, first press:  %s", spread(first, "ms"))
	r.say("          unmute call -> first loud frame at the listener, later presses: %s", spread(later, "ms"))
	r.say("          unmute call -> ListParticipants says \"not muted\", first press:  %s", spread(firstServer, "ms"))
	r.say("          unmute call -> ListParticipants says \"not muted\", later:        %s", spread(laterServer, "ms"))
	r.say("          the unmute call itself returned after %s", spread(returned, "ms"))
	r.say("          mute call -> last loud frame at the listener: %s", spread(quiet, "ms"))
	r.say("          mute call -> ListParticipants says \"muted\":   %s", spread(quietServer, "ms"))
	r.say("          delay of the path itself, 0.7 s into a press (a louder burst handed to the source -> heard): %s", spread(pipeline, "ms"))
	r.say("          unmute -> loud, less that delay (above zero: the start of a press is cut by that much), later presses: %s", spread(laterLost, "ms"))
	r.say("          that delay, less mute -> last loud (above zero: the end of a press is cut by that much): %s", spread(tailLost, "ms"))
	r.say("          presses where the listener already heard the tone before the unmute: %d; dropouts within a press: %d", alreadyLoud, dropouts)
}
