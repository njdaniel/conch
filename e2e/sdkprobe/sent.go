package main

import (
	"fmt"
	"sync"
	"time"
)

// sentVariant is one combination of how a track is silenced and what its
// audio source is handed meanwhile.
type sentVariant struct {
	what string
	args []string
}

var sentVariants = []sentVariant{
	{"A, muted before publishing; tone fed", []string{"--start=premute"}},
	{"B, muted before publishing and disabled after; tone fed", []string{"--start=premute-disable"}},
	{"C, published then muted; tone fed", []string{"--start=postmute"}},
	{"C, published then muted; nothing fed", []string{"--start=postmute", "--gated"}},
	{"published, then only the WebRTC track disabled, LiveKit not told (so the server forwards what is sent); tone fed", []string{"--start=disable-only"}},
	{"not muted; tone fed (for comparison)", []string{"--start=unmuted"}},
	{"not muted; digital silence fed (for comparison)", []string{"--start=unmuted", "--amp=0"}},
	{"C, published then muted; tone fed; discontinuous transmission switched off when publishing", []string{"--start=postmute", "--no-dtx"}},
	{"only the WebRTC track disabled, LiveKit not told; tone fed; discontinuous transmission switched off", []string{"--start=disable-only", "--no-dtx"}},
	{"not muted; digital silence fed; discontinuous transmission switched off (for comparison)", []string{"--start=unmuted", "--amp=0", "--no-dtx"}},
}

// sent is what one talker's track sent, and what its listener received from
// it, over three seconds.
type sent struct {
	packets float64 // RTP packets a second
	bytes   float64 // payload bytes a second
	heard   heard   // the listener's frames from the talker in the same time
}

// measureSent is the other half of row 12: not only what a listener hears
// from a muted track, but what leaves the talker's machine. A talker joins,
// with a listener in the room, and WebRTC's own count of what its track sent
// is read twice, three seconds apart, with the listener's frame counts. Many
// sessions, because what a silent track costs to send turned out to depend on
// which Opus settings a session happens to negotiate.
func (r *run) measureSent() error {
	const (
		perVariant = 6
		atOnce     = 10
	)
	sessions := perVariant * r.reps
	r.say("")
	r.say("== 2, continued: what a muted track sends to the server (row 12, the talker's side) ==")
	r.say("%d sessions for each case, each a talker and a listener in a room of their own. RTP packets and payload bytes a second", sessions)
	r.say("that the talker's track sent, from WebRTC's statistics, over 3 s starting 1.5 s after it published; and what the")
	r.say("listener received from it in those 3 s. A session either sends a packet every 20 ms the whole time, or only now and")
	r.say("then while there is silence to send (Opus discontinuous transmission, DTX): with the SDK's defaults, which of the")
	r.say("two a session gets is not something the application chose.")
	type job struct{ variant int }
	jobs := make(chan job)
	results := make([][]sent, len(sentVariants))
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for w := 0; w < atOnce; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				s, err := r.sentOnce(sentVariants[j.variant].args)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if err == nil {
					results[j.variant] = append(results[j.variant], s)
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for n := 0; n < sessions; n++ {
		for v := range sentVariants {
			select {
			case jobs <- job{v}:
			case <-stopCtx.Done():
				break feed
			}
		}
	}
	close(jobs)
	wg.Wait()
	if stopCtx.Err() != nil {
		return errInterrupted
	}
	if firstErr != nil {
		return firstErr
	}
	for v, variant := range sentVariants {
		r.say("")
		r.say("-- %s --", variant.what)
		// Fifty packets a second is one every 20 ms.
		for _, mode := range []struct {
			name   string
			steady bool
		}{{"sending now and then (DTX)", false}, {"sending every 20 ms", true}} {
			var packets, bytes, frames, loud, nonzero, rms []float64
			for _, s := range results[v] {
				if (s.packets >= 40) != mode.steady {
					continue
				}
				packets, bytes = append(packets, s.packets), append(bytes, s.bytes)
				frames, loud, nonzero = append(frames, s.heard.frames), append(loud, s.heard.loud), append(nonzero, s.heard.nonzero)
				rms = append(rms, s.heard.maxQuietRMS)
			}
			if len(packets) == 0 {
				continue
			}
			r.say("  %d sessions %s: %s packets/s, %s payload bytes/s", len(packets), mode.name, spread(packets, ""), spread(bytes, ""))
			r.say("     their listeners: %s frames; loud: %s; with any nonzero sample: %s; loudest frame that was not loud: RMS %s",
				spread(frames, ""), spread(loud, ""), spread(nonzero, ""), spread(rms, ""))
		}
	}
	return nil
}

func (r *run) sentOnce(args []string) (sent, error) {
	const over = 3 * time.Second
	room, err := r.newRoom(true)
	if err != nil {
		return sent{}, err
	}
	defer r.deleteRoom(room)
	listener, err := r.joined("listener", "listen", "listener", room)
	if err != nil {
		return sent{}, err
	}
	defer listener.quit()
	talker, err := r.joined("talker", "publish", "talker", room, args...)
	if err != nil {
		return sent{}, err
	}
	defer talker.quit()
	if err := pause(1500 * time.Millisecond); err != nil {
		return sent{}, err
	}
	heardBefore, err := listener.ask("audio", "audio")
	if err != nil {
		return sent{}, err
	}
	before, err := talker.ask("rtp", "rtp")
	if err != nil {
		return sent{}, err
	}
	if err := pause(over); err != nil {
		return sent{}, err
	}
	after, err := talker.ask("rtp", "rtp")
	if err != nil {
		return sent{}, err
	}
	heardAfter, err := listener.ask("audio", "audio")
	if err != nil {
		return sent{}, err
	}
	if after.num("streams") != 1 {
		return sent{}, fmt.Errorf("%s: %v outgoing streams in its statistics, expected one", talker.name, after.Fields["streams"])
	}
	seconds := (after.T - before.T) / 1000
	a, b := heardFrom(heardBefore, "talker"), heardFrom(heardAfter, "talker")
	return sent{
		packets: (after.num("packets_sent") - before.num("packets_sent")) / seconds,
		bytes:   (after.num("bytes_sent") - before.num("bytes_sent")) / seconds,
		heard:   heard{frames: b.frames - a.frames, loud: b.loud - a.loud, nonzero: b.nonzero - a.nonzero, maxQuietRMS: b.maxQuietRMS},
	}, nil
}
