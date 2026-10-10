package main

import (
	"fmt"
	"time"
)

const (
	costSpeakers  = 3
	costIntervals = 6 // of ten seconds each
)

// costRun is one client publishing while it receives three speakers.
type costRun struct {
	cpuPercent []float64 // of one core, per ten-second interval
	cpuSeconds float64   // user + system over the minute
	userShare  float64   // the part of that spent in user mode
	rssKB      []float64 // at the start and at each ten seconds
	rssStartKB float64   // when the process had just started, before the SDK did anything
	rssJoinKB  float64   // when Room::connect had returned
	threads    float64
	frames     []float64 // received from each speaker over the minute
	loud       []float64
	fed        float64 // frames it handed to its own track over the minute
}

// measureCost is row 4: CPU time and resident memory of one process that
// publishes a tone while receiving three other speakers, for a minute.
func (r *run) measureCost() error {
	r.say("")
	r.say("== 5: the cost of publishing while receiving three speakers (row 4) ==")
	r.say("one client publishes a 440 Hz tone, unmuted, and reads every frame of three other speakers' tracks for 60 s;")
	r.say("release build; CPU is user + system time from /proc/self/stat (%.0f ticks a second), memory is VmRSS. %d runs.", r.clkTck, r.reps)
	var runs []costRun
	for i := 0; i < r.reps; i++ {
		c, err := r.costOnce()
		r.stopProbes()
		if err != nil {
			return fmt.Errorf("run %d: %w", i+1, err)
		}
		runs = append(runs, c)
	}
	var percent, seconds, user, first, last, peak, threads, frames, loud, fed, bare, joined []float64
	for _, c := range runs {
		percent = append(percent, c.cpuPercent...)
		seconds = append(seconds, c.cpuSeconds)
		user = append(user, c.userShare*100)
		first = append(first, c.rssKB[0]/1024)
		last = append(last, c.rssKB[len(c.rssKB)-1]/1024)
		for _, kb := range c.rssKB {
			peak = append(peak, kb/1024)
		}
		threads = append(threads, c.threads)
		bare, joined = append(bare, c.rssStartKB/1024), append(joined, c.rssJoinKB/1024)
		frames = append(frames, c.frames...)
		loud = append(loud, c.loud...)
		fed = append(fed, c.fed)
	}
	r.say("  CPU per 10 s interval: %s of one core", spread(percent, "%"))
	r.say("  CPU time over the 60 s: %s, of which user mode %s", spread(seconds, "s"), spread(user, "%"))
	r.say("  resident memory: at the start of the minute %s; at its end %s; every 10 s reading %s", spread(first, "MB"), spread(last, "MB"), spread(peak, "MB"))
	r.say("  resident memory before that: when the process had just started %s; when Room::connect had returned %s", spread(bare, "MB"), spread(joined, "MB"))
	r.say("  threads: %s", spread(threads, ""))
	r.say("  frames received per speaker over the 60 s (6000 is all of them): %s, of which loud %s", spread(frames, ""), spread(loud, ""))
	r.say("  frames it handed to its own track over the 60 s: %s", spread(fed, ""))
	for i, c := range runs {
		line := fmt.Sprintf("  run %d: CPU %% per interval", i+1)
		for _, p := range c.cpuPercent {
			line += " " + short(p)
		}
		line += "; RSS MB"
		for _, kb := range c.rssKB {
			line += " " + short(kb/1024)
		}
		r.say("%s", line)
	}
	return nil
}

func (r *run) costOnce() (costRun, error) {
	var c costRun
	room, err := r.newRoom(true)
	if err != nil {
		return c, err
	}
	defer r.deleteRoom(room)
	for i := 0; i < costSpeakers; i++ {
		who := fmt.Sprintf("speaker%d", i+1)
		if _, err := r.joined(who, "publish", who, room, "--start=unmuted", fmt.Sprintf("--freq=%d", 300+200*i)); err != nil {
			return c, err
		}
	}
	client, err := r.joined("client", "duplex", "client", room, "--start=unmuted", "--stats-secs=10")
	if err != nil {
		return c, err
	}
	// Ready once it hears all three.
	if _, err := untilAll([]*probe{client}, 30*time.Second, func(p *probe) bool {
		heard := map[string]bool{}
		for _, e := range p.since(0) {
			if e.Ev == "loud_start" {
				heard[e.str("speaker")] = true
			}
		}
		return len(heard) == costSpeakers
	}); err != nil {
		return c, err
	}
	from := client.mark()
	// The probe reports its statistics and its frame counts every ten
	// seconds; the minute runs from the first report after it was ready.
	stats, audio, err := costReports(client, from, costIntervals+1)
	if err != nil {
		return c, err
	}
	ticks := func(e event) float64 { return e.num("utime_ticks") + e.num("stime_ticks") }
	for i := 1; i < len(stats); i++ {
		seconds := (stats[i].T - stats[i-1].T) / 1000
		c.cpuPercent = append(c.cpuPercent, (ticks(stats[i])-ticks(stats[i-1]))/r.clkTck/seconds*100)
	}
	lastStat := stats[len(stats)-1]
	c.cpuSeconds = (ticks(lastStat) - ticks(stats[0])) / r.clkTck
	if total := ticks(lastStat) - ticks(stats[0]); total > 0 {
		c.userShare = (lastStat.num("utime_ticks") - stats[0].num("utime_ticks")) / total
	}
	for _, s := range stats {
		c.rssKB = append(c.rssKB, s.num("rss_kb"))
	}
	c.threads = lastStat.num("threads")
	if e, ok := client.first(0, func(e event) bool { return e.Ev == "stats_at_start" }); ok {
		c.rssStartKB = e.num("rss_kb")
	}
	if e, ok := client.first(0, func(e event) bool { return e.Ev == "stats_connected" }); ok {
		c.rssJoinKB = e.num("rss_kb")
	}
	for i := 0; i < costSpeakers; i++ {
		who := fmt.Sprintf("speaker%d", i+1)
		a, b := heardFrom(audio[0], who), heardFrom(audio[len(audio)-1], who)
		c.frames = append(c.frames, b.frames-a.frames)
		c.loud = append(c.loud, b.loud-a.loud)
	}
	c.fed = audio[len(audio)-1].num("fed") - audio[0].num("fed")
	return c, nil
}

// costReports waits for n consecutive "stats" events after index from, and
// returns them with the "audio" event that follows each.
func costReports(p *probe, from int, n int) (stats, audio []event, err error) {
	if _, err := untilAll([]*probe{p}, time.Duration(n+2)*10*time.Second, func(p *probe) bool {
		count := 0
		for _, e := range p.since(from) {
			if e.Ev == "audio" {
				count++
			}
		}
		return count >= n
	}); err != nil {
		return nil, nil, err
	}
	for _, e := range p.since(from) {
		switch e.Ev {
		case "stats":
			if len(stats) < n {
				stats = append(stats, e)
			}
		case "audio":
			if len(audio) < len(stats) {
				audio = append(audio, e)
			}
		}
	}
	if len(stats) < n || len(audio) < n {
		return nil, nil, fmt.Errorf("%s: only %d reports in the time for %d", p.name, len(stats), n)
	}
	return stats, audio, nil
}
