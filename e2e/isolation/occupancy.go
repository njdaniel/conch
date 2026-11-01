package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/pkg/schema"
)

// Voice occupancy (issue #596, docs/design/voice-occupancy.md §5): how many
// are in each channel's voice, for the channels the reader is in, read once
// or on a socket of its own. A channel of A's is never in what B is sent, not
// even as a zero; B's counts are B's alone, and they do not move when A's
// people join; the socket closes when its credential goes and loses a row,
// and stays open, when its holder is removed from a channel.

// occupancyPath is the occupancy socket.
const occupancyPath = "/v1/voice/occupancy/ws"

// occupancySock is the request that opens the occupancy socket: it takes no
// channel. Its row reads the first frame, the document sent on connect.
func occupancySock(ticket bool) probe {
	return probe{method: http.MethodGet, path: occupancyPath, socket: true, ticket: ticket, frame: true}
}

// openOccupancy opens, as m, the occupancy socket with the bearer and with a
// ticket.
func openOccupancy(m *member) ([]*feed, error) {
	var out []*feed
	for _, ticket := range []bool{false, true} {
		what := fmt.Sprintf("%s's %s (ticket=%v)", m.name, occupancyPath, ticket)
		f, r, err := dialSocket(m.api, what, occupancyPath, url.Values{}, ticket)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, fmt.Errorf("%s: status %d %s", what, r.status, m.api.h.redact(string(r.body)))
		}
		out = append(out, f)
	}
	return out, nil
}

// isOccupancy reports whether f is an occupancy socket.
func isOccupancy(f *feed) bool { return strings.Contains(f.what, occupancyPath) }

// parseOccupancy reads one occupancy document: no field it does not declare,
// and valid.
func parseOccupancy(raw []byte) (schema.VoiceOccupancyV1, error) {
	var doc schema.VoiceOccupancyV1
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("not an occupancy document (%w): %s", err, lkit.Truncate(string(raw), 200))
	}
	if err := doc.Validate(); err != nil {
		return doc, err
	}
	return doc, nil
}

// counts is a document's counts by channel id.
func counts(doc schema.VoiceOccupancyV1) map[int64]int {
	out := map[int64]int{}
	for _, c := range doc.Channels {
		out[c.ChannelID] = c.InVoice
	}
	return out
}

// checkRows requires that an available document has a row for exactly the
// channels in own, and an unavailable one none: a channel the reader is not
// in is absent, never a zero.
func checkRows(doc schema.VoiceOccupancyV1, own []int64) error {
	var got []int64
	for _, c := range doc.Channels {
		got = append(got, c.ChannelID)
	}
	want := own
	if !doc.Available {
		want = nil
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("the occupancy rows are for channels %v, want exactly %v", got, want)
	}
	return nil
}

// channelIDs is the ids of the channels a is a member of, as GET /v1/channels
// lists them.
func channelIDs(a api) ([]int64, error) {
	var list schema.ListChannelsResponse
	if err := a.call(http.MethodGet, "/v1/channels", nil, &list); err != nil {
		return nil, err
	}
	var ids []int64
	for _, c := range list.Channels {
		ids = append(ids, c.ID)
	}
	slices.Sort(ids)
	return ids, nil
}

// checkOwnOccupancy is the occupancy rows' own check: the document the prober
// was given has a row for each of her channels and for nothing else.
func checkOwnOccupancy(_ *world, a api, body []byte) error {
	doc, err := parseOccupancy(body)
	if err != nil {
		return err
	}
	own, err := channelIDs(a)
	if err != nil {
		return err
	}
	return checkRows(doc, own)
}

// checkAgentVoiceDenied returns the agent rows' check: the refusal is in the
// audit log as an access_denied of the agent's, for the route, as a voice
// refusal.
func checkAgentVoiceDenied(route string) func(w *world, a api) error {
	return func(w *world, _ api) error {
		status, body, err := w.op.do(http.MethodGet, "/v1/audit", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("the operator reads the audit log: status %d", status)
		}
		actor := fmt.Sprintf("principal:%d", w.prober.id)
		sc := bufio.NewScanner(bytes.NewReader(body))
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			var e schema.AuditEventV1
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				return err
			}
			if e.Action == "access_denied" && e.Actor == actor && e.Subject == route && strings.Contains(e.Detail, "reason=agents_no_voice") {
				return nil
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
		return fmt.Errorf("no access_denied of %s for %s in the audit log", actor, route)
	}
}

// presenceCounts is how many are in each of channels' channel-wide rooms as
// a reads presence: what occupancy must say.
func (w *world) presenceCounts(a api, g *group) (map[int64]int, error) {
	out := map[int64]int{}
	for i, ch := range g.channels {
		var doc schema.VoicePresenceV1
		if err := a.call(http.MethodGet, "/v1/channels/"+url.PathEscape(ch)+"/voice", nil, &doc); err != nil {
			return nil, err
		}
		in := map[int64]bool{}
		for _, r := range doc.Rooms {
			if r.Audience != nil {
				continue
			}
			for _, p := range r.Participants {
				in[p.PrincipalID] = true
			}
		}
		out[g.chanIDs[i]] = len(in)
	}
	return out, nil
}

// checkOccupancyFeed is checkQuiet's part for one of B's occupancy sockets:
// every frame it has carried is a document with a row for each of B's
// channels and nothing else, and, as the control, its latest frame says what
// B's own presence says. mode is "counts" with LiveKit, the schema name for
// the one document a socket carries without it, or "" for nothing to wait
// for.
func (s *bSockets) checkOccupancyFeed(w *world, f *feed, mode string, since int) error {
	bob := w.b.people[1]
	own := slices.Clone(w.b.chanIDs)
	switch mode {
	case "":
	case "counts":
		if err := waitFor(f.what+" to say what B's presence says", 30*time.Second, func() (bool, string) {
			want, err := w.presenceCounts(bob.api, w.b)
			if err != nil {
				return false, err.Error()
			}
			frames := f.received()
			if len(frames) == 0 {
				return false, "no frame yet"
			}
			doc, err := parseOccupancy(frames[len(frames)-1])
			if err != nil || !doc.Available {
				return false, "latest frame " + lkit.Truncate(string(frames[len(frames)-1]), 200)
			}
			got := counts(doc)
			return maps.Equal(got, want), fmt.Sprintf("latest frame %v, B's presence %v (%d frames, %d since the last check)", got, want, len(frames), len(frames)-since)
		}); err != nil {
			return err
		}
	default:
		if err := waitFor(f.what+" to carry its document", 30*time.Second, func() (bool, string) {
			frames := f.received()
			for _, fr := range frames[min(since, len(frames)):] {
				if bytes.Contains(fr, []byte(mode)) {
					return true, ""
				}
			}
			return false, fmt.Sprintf("%d frames", len(frames))
		}); err != nil {
			return err
		}
	}
	for i, fr := range f.received() {
		doc, err := parseOccupancy(fr)
		if err != nil {
			return fmt.Errorf("frame %d on %s: %w", i, f.what, err)
		}
		if err := checkRows(doc, own); err != nil {
			return fmt.Errorf("frame %d on %s: %w", i, f.what, err)
		}
	}
	return nil
}

// checkOccupancyWhileAIsInVoice runs once A is in voice: A's own occupancy
// shows ann and amy in A's room, so the server has seen them, and yet no
// frame B's occupancy sockets carried counts anyone in B's channels, where
// nobody of B's has joined yet. The silence means something because A's side
// is seen to move.
func (s *bSockets) checkOccupancyWhileAIsInVoice(w *world) error {
	ann := w.a.people[0]
	camp := w.a.chanIDs[0]
	// ann's channels: A's, and the public channel she made (communityrows.go).
	annsChannels := slices.Clone(w.a.chanIDs)
	if w.a.publicID != 0 {
		annsChannels = append(annsChannels, w.a.publicID)
	}
	if err := waitFor("ann's occupancy to show ann and amy in A's room", 30*time.Second, func() (bool, string) {
		var doc schema.VoiceOccupancyV1
		if err := ann.api.call(http.MethodGet, "/v1/voice/occupancy", nil, &doc); err != nil {
			return false, err.Error()
		}
		if err := checkRows(doc, annsChannels); err != nil {
			return false, err.Error()
		}
		return counts(doc)[camp] == 2, fmt.Sprintf("%v", counts(doc))
	}); err != nil {
		return err
	}
	n := 0
	for _, f := range s.feeds {
		if !isOccupancy(f) {
			continue
		}
		for i, fr := range f.received() {
			doc, err := parseOccupancy(fr)
			if err != nil {
				return fmt.Errorf("frame %d on %s: %w", i, f.what, err)
			}
			for id, c := range counts(doc) {
				if c != 0 {
					return fmt.Errorf("frame %d on %s counts %d in B's channel %d while only A was in voice", i, f.what, c, id)
				}
			}
			n++
		}
	}
	if n == 0 {
		return errors.New("no frame on any of B's occupancy sockets")
	}
	w.h.say("ok   occupancy: ann's own shows ann and amy in A's room; B's occupancy sockets carried %d frames, every count in B's channels zero, and no channel of A's", n)
	return nil
}

// closedRevoked requires that the occupancy sockets among feeds closed with
// the code the presence socket closes with for a credential that went: 1008
// and "credential no longer valid".
func closedRevoked(feeds []*feed) error {
	for _, f := range feeds {
		if !isOccupancy(f) {
			continue
		}
		f.mu.Lock()
		err := f.err
		f.mu.Unlock()
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != "credential no longer valid" {
			return fmt.Errorf("%s ended with %w, want 1008 \"credential no longer valid\"", f.what, err)
		}
	}
	return nil
}

// removalDropsTheRow: bea holds an occupancy socket and the operator removes
// her from B's second channel. The socket stays open and, within the bound a
// presence socket is closed in, sends a document without that channel's row.
// She is put back afterwards, for the second control.
func (w *world) removalDropsTheRow() (time.Duration, error) {
	bea := w.b.people[0]
	loft, loftID := w.b.channels[1], w.b.chanIDs[1]
	feeds, err := openOccupancy(bea)
	if err != nil {
		return 0, err
	}
	defer func() {
		for _, f := range feeds {
			f.close()
		}
	}()
	for _, f := range feeds {
		if err := waitFor(f.what+" to send its document", 30*time.Second, func() (bool, string) {
			return len(f.received()) > 0, "no frame"
		}); err != nil {
			return 0, err
		}
		if doc, err := parseOccupancy(f.received()[0]); err != nil {
			return 0, err
		} else if err := checkRows(doc, w.b.chanIDs); err != nil {
			return 0, fmt.Errorf("%s before the removal: %w", f.what, err)
		}
	}
	start := time.Now()
	if err := w.op.call(http.MethodDelete, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(loft), bea.id), nil, nil); err != nil {
		return 0, err
	}
	var slowest time.Duration
	for _, f := range feeds {
		var at time.Duration
		if err := waitFor(f.what+" to drop "+loft, 30*time.Second, func() (bool, string) {
			frames := f.received()
			doc, err := parseOccupancy(frames[len(frames)-1])
			if err != nil {
				return false, err.Error()
			}
			if slices.ContainsFunc(doc.Channels, func(c schema.VoiceChannelOccupancy) bool { return c.ChannelID == loftID }) {
				return false, fmt.Sprintf("%d frames, the latest with the channel", len(frames))
			}
			at = time.Since(start)
			return true, ""
		}); err != nil {
			return 0, err
		}
		if ended, _ := f.ended(); ended {
			return 0, fmt.Errorf("%s closed when bea was removed from one channel", f.what)
		}
		frames := f.received()
		doc, _ := parseOccupancy(frames[len(frames)-1])
		if err := checkRows(doc, w.b.chanIDs[:1]); err != nil {
			return 0, fmt.Errorf("%s after the removal: %w", f.what, err)
		}
		slowest = max(slowest, at)
	}
	if slowest > socketBound {
		return slowest, fmt.Errorf("bea's occupancy dropped %s after %s, bound %s", loft, slowest.Round(time.Millisecond), socketBound)
	}
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(loft), bea.id), nil, nil); err != nil {
		return slowest, err
	}
	return slowest, nil
}
