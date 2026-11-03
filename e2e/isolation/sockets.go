package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// The sockets: what B's own open sockets receive while A is active, from
// before A starts to the end of the run, and the sockets a revoked tester
// holds. (B asking for A's channels on a socket is a row of the matrix:
// refused before the upgrade.)

// feed is one open socket and everything it received.
type feed struct {
	what   string
	c      *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	frames   [][]byte
	closedAt time.Time
	err      error
}

// socketURL is the address of a socket route on conchd, with the query.
func socketURL(base, path string, q url.Values) string {
	return strings.Replace(base, "http://", "ws://", 1) + path + "?" + q.Encode()
}

// refused is the answer to an upgrade that was refused.
type refused struct {
	status int
	body   []byte
	header http.Header
}

// dialSocket opens path as a, with its bearer or (ticket true) with a ticket
// it asks for first. On a refused upgrade it returns the answer.
func dialSocket(a api, what, path string, q url.Values, ticket bool) (*feed, refused, error) {
	if ticket {
		t, err := a.ticket()
		if err != nil {
			return nil, refused{}, err
		}
		return dialWithTicket(a, what, path, q, t)
	}
	return dial(a, what, path, q, http.Header{"Authorization": {"Bearer " + a.token}})
}

// dialWithTicket opens path with a ticket the caller already holds.
func dialWithTicket(a api, what, path string, q url.Values, ticket string) (*feed, refused, error) {
	q.Set("ticket", ticket)
	return dial(a, what, path, q, http.Header{})
}

func dial(a api, what, path string, q url.Values, hdr http.Header) (*feed, refused, error) {
	forwarded(hdr, a.addr)
	a.h.called(http.MethodGet, path)
	ctx, cancel := context.WithTimeout(stopCtx, 10*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, socketURL(a.base, path, q), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		var r refused
		if resp != nil {
			r.status, r.header = resp.StatusCode, resp.Header
			if resp.Body != nil {
				r.body, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
				_ = resp.Body.Close()
			}
		}
		if r.status == 0 {
			return nil, r, fmt.Errorf("%s: %w", what, err)
		}
		return nil, r, nil
	}
	c.SetReadLimit(4 << 20)
	rctx, rcancel := context.WithCancel(context.Background())
	f := &feed{what: what, c: c, cancel: rcancel, done: make(chan struct{})}
	go f.read(rctx)
	a.h.onCleanup(f.close)
	return f, refused{}, nil
}

func (f *feed) read(ctx context.Context) {
	defer close(f.done)
	for {
		_, data, err := f.c.Read(ctx)
		if err != nil {
			f.mu.Lock()
			f.closedAt, f.err = time.Now(), err
			f.mu.Unlock()
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, data)
		f.mu.Unlock()
	}
}

// received is every frame so far.
func (f *feed) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

// ended reports whether the socket has closed, and when.
func (f *feed) ended() (bool, time.Time) {
	select {
	case <-f.done:
	default:
		return false, time.Time{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return true, f.closedAt
}

func (f *feed) close() {
	f.cancel()
	_ = f.c.CloseNow()
	<-f.done
}

// socketVariants are every socket route, with the bearer and, where the route
// takes one, with a ticket: the eight ways to hold a socket open.
var socketVariants = []struct {
	path   string
	ticket bool
}{
	{"/v0/ws", false}, {"/v1/ws", false},
	{"/v2/ws", false}, {"/v2/ws", true},
	{"/v1/events/ws", false}, {"/v1/events/ws", true},
	{"/v1/voice/ws", false}, {"/v1/voice/ws", true},
}

// openAll opens, as m, every socket variant on channel.
func openAll(m *member, channel string) ([]*feed, error) {
	var out []*feed
	for _, v := range socketVariants {
		// A guest holds no voice (ADR-014 A4, #1016: permittedTx answers
		// false for every guest), so it has no voice socket to hold.
		if m.guest != nil && v.path == "/v1/voice/ws" {
			continue
		}
		what := fmt.Sprintf("%s's %s on %s (ticket=%v)", m.name, v.path, channel, v.ticket)
		f, r, err := dialSocket(m.api, what, v.path, url.Values{"channel": {channel}}, v.ticket)
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

// bSockets is what B holds open from before A is active to the end of the
// run: every socket variant on each of B's channels.
type bSockets struct {
	feeds []*feed
	// checked is how many frames of each feed an earlier check saw: a later
	// check's control must arrive after them.
	checked map[*feed]int
}

// openBSockets opens, as m, every socket variant on each of channels, and
// the occupancy socket, which is m's across every channel, with the bearer
// and with a ticket.
func openBSockets(m *member, channels []string) (*bSockets, error) {
	out := &bSockets{checked: map[*feed]int{}}
	for _, ch := range channels {
		feeds, err := openAll(m, ch)
		if err != nil {
			return nil, err
		}
		out.feeds = append(out.feeds, feeds...)
	}
	// After the per-channel ones, so that by the time checkQuiet reaches
	// them the voice sockets have shown B's control in presence.
	occ, err := openOccupancy(m)
	if err != nil {
		return nil, err
	}
	out.feeds = append(out.feeds, occ...)
	return out, nil
}

// controls is what B does so that each of its sockets is known to deliver,
// per channel: a message (message sockets), a session (events sockets) and a
// string the voice sockets must carry ("" when none is to be waited for);
// and what the occupancy sockets wait for (checkOccupancyFeed's mode).
type controls struct {
	message   string
	session   string
	voice     map[string]string // by channel name
	occupancy string
}

// checkQuiet waits until each socket of B has carried B's control since the
// last check, so that each is known to deliver live; then it requires that
// none of the frames it has carried since it opened holds a marker of A, and
// that it is still open.
func (s *bSockets) checkQuiet(w *world, c controls) error {
	for _, f := range s.feeds {
		want := c.message
		switch {
		case isOccupancy(f):
			if err := s.checkOccupancyFeed(w, f, c.occupancy, s.checked[f]); err != nil {
				return err
			}
			want = ""
		case strings.Contains(f.what, "/v1/voice/ws"):
			want = ""
			for ch, v := range c.voice {
				if strings.Contains(f.what, " on "+ch+" ") {
					want = v
				}
			}
		case strings.Contains(f.what, "/v1/events/ws"):
			want = c.session
		}
		since := s.checked[f]
		if want != "" {
			if err := waitFor(f.what+" to carry B's control", 30*time.Second, func() (bool, string) {
				frames := f.received()
				for _, fr := range frames[min(since, len(frames)):] {
					if bytes.Contains(fr, []byte(want)) {
						return true, ""
					}
				}
				return false, fmt.Sprintf("%d frames, %d since the last check", len(frames), len(frames)-since)
			}); err != nil {
				return err
			}
		}
		frames := f.received()
		for i, fr := range frames {
			if leaks := w.leaks(fr, asked{}); len(leaks) > 0 {
				return fmt.Errorf("frame %d on %s holds %s", i, f.what, strings.Join(leaks, "; "))
			}
		}
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s closed while A was active", f.what)
		}
		s.checked[f] = len(frames)
	}
	return nil
}

// frames is how many frames B's sockets carried in all.
func (s *bSockets) frames() int {
	n := 0
	for _, f := range s.feeds {
		n += len(f.received())
	}
	return n
}

// The directory signal (issue #993, community-model §13.6, ADR-014).
// conch.directory_changed.v1 goes to the human events sockets of the viewers
// whose answer to GET /v1/directory/members may have changed, and it carries
// nothing: no id, no name, no count, no reason. In the community as the run
// has it (members hold see_member_directory, so the list is everyone) the
// rows are:
//
//   - D1: a membership change (ann added to and removed from a channel of A's
//     that ola of B is in) does not change any directory, so no events socket
//     of anyone carries a frame: not ann's, the operator's, ola's, bob's nor
//     an agent's.
//   - D2: ann, of A, is renamed. Her name is public in the community's
//     directory, so every human socket is owed exactly one frame, bob's and
//     ola's of B included, and an agent's is owed none (an agent has no
//     directory, O11); the frame names neither her nor her group.
//   - D3: every frame is exactly the wire form, nothing more.
//
// A quiet socket is checked after the owed ones have their frame and a
// further ccQuiet has passed (the frame is coalesced under the 250 ms roster
// floor, so one owed to anyone else would have gone too).

// directoryChanged counts f's conch.directory_changed.v1 frames, and fails
// on one that is not exactly the wire form (D3).
func directoryChanged(f *feed) (int, error) {
	n := 0
	for _, fr := range f.received() {
		if !strings.Contains(string(fr), schema.DirectoryChangedSchemaV1) {
			continue
		}
		if string(fr) != directoryChangedWire {
			return n, fmt.Errorf("%s carried %s, want exactly %s", f.what, fr, directoryChangedWire)
		}
		n++
	}
	return n, nil
}

// directoryChangedRows runs D1–D3 with bob's sockets (bsock) and sockets of
// its own, and returns how many sockets it looked at. Ann is left with her
// own name and in no channel of cove's.
func (w *world) directoryChangedRows(bsock *bSockets) (int, error) {
	ann, ola := w.a.people[0], w.b.people[2]
	cove := w.aName("cove-directory")
	w.marks.str("A's channel name", cove)
	if err := w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: cove}, nil); err != nil {
		return 0, err
	}
	open := func(a api, what, channel string) (*feed, error) {
		f, r, err := dialSocket(a, what, "/v1/events/ws", url.Values{"channel": {channel}}, false)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, fmt.Errorf("%s: status %d %s", what, r.status, w.h.redact(string(r.body)))
		}
		return f, waitFor(what+" to send its roster", 30*time.Second, func() (bool, string) {
			return len(f.received()) > 0, "no frame"
		})
	}
	var owed, quiet []*feed
	for _, s := range []struct {
		a             api
		what, channel string
	}{
		{ann.api, "ann's events socket on " + w.a.main(), w.a.main()},
		{w.op, "the operator's events socket on " + w.a.main(), w.a.main()},
		{ola.api, "ola's events socket on " + w.b.main(), w.b.main()},
	} {
		f, err := open(s.a, s.what, s.channel)
		if err != nil {
			return 0, err
		}
		owed = append(owed, f)
	}
	for _, s := range []struct {
		a             api
		what, channel string
	}{
		{w.a.agent.api, "A's agent's events socket on " + w.a.main(), w.a.main()},
		{w.b.agent.api, "B's agent's events socket on " + w.b.main(), w.b.main()},
	} {
		f, err := open(s.a, s.what, s.channel)
		if err != nil {
			return 0, err
		}
		quiet = append(quiet, f)
	}
	// Bob's are human: owed what the other humans are.
	for _, f := range bsock.feeds {
		if strings.Contains(f.what, "/v1/events/ws") {
			owed = append(owed, f)
		}
	}
	all := append(append([]*feed(nil), owed...), quiet...)
	pause := func() error {
		select {
		case <-stopCtx.Done():
			return errInterrupted
		case <-time.After(ccQuiet):
			return nil
		}
	}
	// Whatever the setup above owed has had a floor to go.
	if err := pause(); err != nil {
		return 0, err
	}
	step := func(row string, act func() error, want []*feed) error {
		base := map[*feed]int{}
		for _, f := range all {
			n, err := directoryChanged(f)
			if err != nil {
				return fmt.Errorf("%s: %w", row, err)
			}
			base[f] = n
		}
		if err := act(); err != nil {
			return fmt.Errorf("%s: %w", row, err)
		}
		for _, f := range want {
			if err := waitFor(f.what+" to carry directory_changed ("+row+")", 30*time.Second, func() (bool, string) {
				n, err := directoryChanged(f)
				if err != nil {
					return true, ""
				}
				return n > base[f], fmt.Sprintf("%d frames, %d of them directory_changed", len(f.received()), n)
			}); err != nil {
				return err
			}
		}
		if err := pause(); err != nil {
			return err
		}
		for _, f := range all {
			n, err := directoryChanged(f)
			if err != nil {
				return fmt.Errorf("%s: %w", row, err)
			}
			expected := 0
			if slices.Contains(want, f) {
				expected = 1
			}
			if got := n - base[f]; got != expected {
				return fmt.Errorf("%s: %s carried %d directory_changed frames, want %d", row, f.what, got, expected)
			}
			if ended, _ := f.ended(); ended {
				return fmt.Errorf("%s: %s closed", row, f.what)
			}
		}
		return nil
	}
	member := func(method string) func() error {
		return func() error {
			return w.op.call(method, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(cove), ann.id), nil, nil)
		}
	}
	// ola is in cove too, so that a change of its members is as visible to B
	// as it can be made.
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(cove), ola.id), nil, nil); err != nil {
		return 0, err
	}
	if err := pause(); err != nil {
		return 0, err
	}
	rename := func(name string) func() error {
		return func() error {
			return w.op.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/name", ann.id), schema.RenamePrincipalRequestV1{Name: name}, nil)
		}
	}
	original := ann.name
	steps := []struct {
		row  string
		act  func() error
		want []*feed
	}{
		{"D1/D3: ann added to cove, ola of B in it", member(http.MethodPut), nil},
		{"D1/D3: ann removed from cove", member(http.MethodDelete), nil},
		{"D1/D3: ola removed from cove", func() error {
			return w.op.call(http.MethodDelete, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(cove), ola.id), nil, nil)
		}, nil},
		{"D2/D3: ann renamed", rename(w.aName("ann-renamed")), owed},
		{"D2/D3: ann renamed back", rename(original), owed},
	}
	for _, s := range steps {
		if err := step(s.row, s.act, s.want); err != nil {
			return 0, err
		}
	}
	// Nothing of A on B's own sockets of this phase, ola's and B's agent's.
	for _, f := range []*feed{owed[2], quiet[1]} {
		for i, fr := range f.received() {
			if leaks := w.leaks(fr, asked{}); len(leaks) > 0 {
				return 0, fmt.Errorf("frame %d on %s holds %s", i, f.what, strings.Join(leaks, "; "))
			}
		}
	}
	for _, f := range append(append([]*feed(nil), owed[:3]...), quiet...) {
		f.close()
	}
	return len(all), nil
}
