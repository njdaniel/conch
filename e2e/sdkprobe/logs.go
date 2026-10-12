package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

var logLevels = []string{"ERROR", "WARN", "INFO", "DEBUG", "TRACE"}

// logTotals adds up the log searches of several probes in one situation.
type logTotals struct {
	probes   int
	records  map[string]float64            // level -> records captured
	holding  map[string]map[string]float64 // level -> what was searched for -> records holding it
	places   map[string]map[string]float64 // "LEVEL target (file:line)" -> what it held -> records
	targets  map[string]float64            // target -> records
	searched map[string]bool
	stderr   int // probes that wrote anything to standard error
}

func newLogTotals() *logTotals {
	return &logTotals{
		records: map[string]float64{}, holding: map[string]map[string]float64{},
		places: map[string]map[string]float64{}, targets: map[string]float64{}, searched: map[string]bool{},
	}
}

// add takes one probe's "log_scan" event.
func (t *logTotals) add(p *probe) error {
	scan, ok := p.first(0, func(e event) bool { return e.Ev == "log_scan" })
	if !ok {
		return fmt.Errorf("%s: no log search in its output", p.name)
	}
	t.probes++
	if p.stderrText() != "" {
		t.stderr++
	}
	for _, name := range anyList(scan.Fields["searched_for"]) {
		t.searched[fmt.Sprint(name)] = true
	}
	levels, _ := scan.Fields["levels"].(map[string]any)
	for level, raw := range levels {
		counts, _ := raw.(map[string]any)
		for what, n := range counts {
			count, _ := n.(float64)
			if what == "records" {
				t.records[level] += count
				continue
			}
			if t.holding[level] == nil {
				t.holding[level] = map[string]float64{}
			}
			t.holding[level][what] += count
		}
	}
	targets, _ := scan.Fields["targets"].(map[string]any)
	for target, raw := range targets {
		counts, _ := raw.(map[string]any)
		for _, n := range counts {
			count, _ := n.(float64)
			t.targets[target] += count
		}
	}
	for _, raw := range anyList(scan.Fields["hits"]) {
		hit, _ := raw.(map[string]any)
		count, _ := hit["records"].(float64)
		place := fmt.Sprintf("%v %v (%v)", hit["level"], hit["target"], hit["site"])
		if t.places[place] == nil {
			t.places[place] = map[string]float64{}
		}
		t.places[place][fmt.Sprint(hit["needle"])] += count
	}
	return nil
}

// opusSettings is the audio codec settings a probe's SDK offered and was
// answered, from the session descriptions it logged at Debug: the a=fmtp
// lines that mention Opus's own parameters, with the place that logged each.
func opusSettings(p *probe) string {
	scan, ok := p.first(0, func(e event) bool { return e.Ev == "log_scan" })
	if !ok {
		return "no log search"
	}
	var lines []string
	for _, raw := range anyList(scan.Fields["sdp_fmtp"]) {
		m, _ := raw.(map[string]any)
		line, _ := m["line"].(string)
		if !strings.Contains(line, "minptime") && !strings.Contains(line, "useinbandfec") {
			continue
		}
		site, _ := m["site"].(string)
		if i := strings.LastIndex(site, "/"); i >= 0 {
			site = site[i+1:]
		}
		lines = append(lines, fmt.Sprintf("%s %s", site, line))
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "no Opus settings logged"
	}
	return strings.Join(lines, " | ")
}

func anyList(v any) []any {
	list, _ := v.([]any)
	return list
}

func (r *run) reportLogs(what string, t *logTotals) {
	r.say("")
	r.say("-- %s (%d probes) --", what, t.probes)
	var searched []string
	for name := range t.searched {
		searched = append(searched, name)
	}
	sort.Strings(searched)
	r.say("  searched for: %s", strings.Join(searched, ", "))
	for _, level := range logLevels {
		var found []string
		for name, n := range t.holding[level] {
			found = append(found, fmt.Sprintf("%s in %.0f", name, n))
		}
		sort.Strings(found)
		verdict := "none of them"
		if len(found) > 0 {
			verdict = strings.Join(found, "; ")
		}
		r.say("  %-5s %7.0f records; holding: %s", level, t.records[level], verdict)
	}
	var places []string
	for place, held := range t.places {
		var found []string
		for name, n := range held {
			found = append(found, fmt.Sprintf("%s in %.0f", name, n))
		}
		sort.Strings(found)
		places = append(places, fmt.Sprintf("%s: %s", place, strings.Join(found, "; ")))
	}
	sort.Strings(places)
	for _, place := range places {
		r.say("    the statement at %s", place)
	}
	var targets []string
	for target, n := range t.targets {
		targets = append(targets, fmt.Sprintf("%s %.0f", target, n))
	}
	sort.Strings(targets)
	r.say("  targets that logged: %s", strings.Join(targets, ", "))
	r.say("  probes that wrote anything to standard error (where native code that goes round the log facade would write): %d", t.stderr)
}

// measureLogs is row 10: everything the SDK and what it links hand to the
// `log` facade, at every level, searched for the join token.
func (r *run) measureLogs() error {
	r.say("")
	r.say("== 6: tokens in the SDK's log output (row 10) ==")
	r.say("the probe installs a log::Log that keeps every record of every target at Trace, and searches them when it ends for:")
	r.say("the join token whole, each of its three dot-separated parts, the room's name, any token LiveKit sent it, and anything shaped like a JWT.")
	r.say("(a JWT's first part is the same for every token signed this way; it identifies no token.)")

	joins := newLogTotals()
	var codecs []string
	for i := 0; i < r.reps; i++ {
		room, err := r.newRoom(true)
		if err != nil {
			return err
		}
		p, err := r.client(room, "--capture-logs")
		if err != nil {
			return err
		}
		if err := pause(time.Second); err != nil {
			return err
		}
		before, err := p.ask("rtp", "rtp")
		if err != nil {
			return err
		}
		if err := pause(3 * time.Second); err != nil {
			return err
		}
		after, err := p.ask("rtp", "rtp")
		if err != nil {
			return err
		}
		p.quit()
		r.deleteRoom(room)
		if err := joins.add(p); err != nil {
			return err
		}
		codecs = append(codecs, fmt.Sprintf("muted, it sent %.0f RTP packets a second; %s",
			(after.num("packets_sent")-before.num("packets_sent"))/3, opusSettings(p)))
	}
	r.reportLogs("a successful join, a muted track published, four seconds connected, a clean leave", joins)
	r.say("  the Opus settings in the session descriptions the SDK logged at Debug, per client: %s", tally(codecs))

	deleted := newLogTotals()
	var saw []string
	for i := 0; i < r.reps; i++ {
		order, err := r.logsAcrossDelete(deleted)
		if err != nil {
			return err
		}
		saw = append(saw, order)
	}
	r.reportLogs("a join, then the room deleted under the client (what a rotation does)", deleted)
	r.say("  what the clients saw: %s", tally(saw))

	resumes, note, err := r.logsAcrossFreeze()
	if err != nil {
		return err
	}
	r.reportLogs("a join, then the server frozen until the SDK starts to reconnect, and thawed", resumes)
	r.say("  %s", note)

	failed := newLogTotals()
	var errors []string
	var took []float64
	holds := 0
	for i := 0; i < r.reps; i++ {
		room, err := r.newRoom(false)
		if err != nil {
			return err
		}
		tok, err := r.token("client", room)
		if err != nil {
			return err
		}
		p, err := r.start("client", "duplex", tok, room, "--capture-logs")
		if err != nil {
			return err
		}
		e, err := p.next(0, 60*time.Second, "the join to fail", func(e event) bool { return e.Ev == "connect_failed" || e.Ev == "connected" })
		if err != nil {
			return err
		}
		if e.Ev == "connected" {
			return fmt.Errorf("a token for a room that does not exist was let in")
		}
		errors = append(errors, fmt.Sprintf("%q", e.str("error")))
		took = append(took, e.num("connect_ms"))
		if e.flag("error_holds_token") {
			holds++
		}
		if _, err := p.wait(0, 20*time.Second, "exit"); err != nil {
			return err
		}
		if err := failed.add(p); err != nil {
			return err
		}
	}
	r.reportLogs("a failed join: a token for a room that does not exist", failed)
	r.say("  the error Room::connect returned (after the probe's own redaction): %s", tally(errors))
	r.say("  Room::connect returned it after %s", spread(took, "ms"))
	r.say("  errors whose text held the join token: %d of %d", holds, r.reps)
	return nil
}

// logsAcrossDelete captures the logs of one client whose room is deleted, and
// adds its search to totals.
func (r *run) logsAcrossDelete(totals *logTotals) (string, error) {
	room, err := r.newRoom(true)
	if err != nil {
		return "", err
	}
	p, err := r.client(room, "--capture-logs")
	if err != nil {
		return "", err
	}
	if err := pause(2 * time.Second); err != nil {
		return "", err
	}
	from := p.mark()
	called := time.Now()
	r.deleteRoom(room)
	if _, err := p.wait(from, 30*time.Second, "disconnected"); err != nil {
		return "", err
	}
	if err := pause(2 * time.Second); err != nil {
		return "", err
	}
	order := lifeOf(p.since(from), called).order
	p.quit()
	return order, totals.add(p)
}

// logsAcrossFreeze captures the logs of clients whose connection is
// interrupted: the server is frozen until every client has reported
// Reconnecting (or a minute has passed), then thawed.
func (r *run) logsAcrossFreeze() (*logTotals, string, error) {
	var probes []*probe
	for i := 0; i < r.reps; i++ {
		room, err := r.newRoom(true)
		if err != nil {
			return nil, "", err
		}
		p, err := r.client(room, "--capture-logs")
		if err != nil {
			return nil, "", err
		}
		probes = append(probes, p)
	}
	if err := pause(2 * time.Second); err != nil {
		return nil, "", err
	}
	from := map[*probe]int{}
	for _, p := range probes {
		from[p] = p.mark()
	}
	frozen, _, err := r.srv.freeze()
	if err != nil {
		return nil, "", err
	}
	if _, err := untilAll(probes, 60*time.Second, func(p *probe) bool {
		_, ok := p.first(from[p], func(e event) bool { return e.Ev == "reconnecting" })
		return ok
	}); err != nil {
		return nil, "", err
	}
	held := time.Since(frozen)
	if _, _, err := r.srv.thaw(); err != nil {
		return nil, "", err
	}
	if _, err := untilAll(probes, 120*time.Second, func(p *probe) bool { return settled(p, from[p]) }); err != nil {
		return nil, "", err
	}
	if err := pause(3 * time.Second); err != nil {
		return nil, "", err
	}
	var orders []string
	refreshed := 0
	for _, p := range probes {
		orders = append(orders, lifeOf(p.since(from[p]), frozen).order)
		if _, ok := p.first(0, func(e event) bool { return e.Ev == "token_refreshed" }); ok {
			refreshed++
		}
	}
	totals := newLogTotals()
	for _, p := range probes {
		p.quit()
		if err := totals.add(p); err != nil {
			return nil, "", err
		}
	}
	note := fmt.Sprintf("frozen %.0f s; what the clients saw: %s; clients LiveKit had sent a token of its own: %d of %d",
		held.Seconds(), tally(orders), refreshed, len(probes))
	return totals, note, nil
}
