package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The channel-list signal (issue #1027, docs/design/membership-live.md §6).
// conch.channels_changed.v1 goes to the human whose own channel list changed,
// and to no one else, and it carries nothing. These are the leak rows:
//
//   - L1: ann is added to cove, a channel of A's that B is not in; bob, whose
//     sockets are open on B's channels, is sent no channels_changed.
//   - L2: ann is removed from cove while ola (of B) is a member of it; ola is
//     sent none (her member list changes through the members frame, which
//     her socket, open on B's channel, does not carry).
//   - L3: through both, the operator (a member of cove) and both groups'
//     agents, each with an events socket open, are sent none.
//   - L4: ann's own frames are exactly the wire form, nothing more.
//   - L7: cove is renamed; only its human members (the operator and ann) are
//     sent the frame, and bob, ola and the agents stay quiet.
//
// A quiet socket is checked after the subject's own frame has arrived and a
// further ccQuiet has passed: the frame is trailing-edge, one roster floor
// (250 ms) after the change, so one owed to anyone else would have gone too.

const (
	channelsChangedWire = `{"schema":"conch.channels_changed.v1"}`
	ccQuiet             = 1500 * time.Millisecond
)

// channelsChanged counts f's conch.channels_changed.v1 frames, and fails on
// one that is not exactly the wire form (L4).
func channelsChanged(f *feed) (int, error) {
	n := 0
	for _, fr := range f.received() {
		if !strings.Contains(string(fr), schema.ChannelsChangedSchemaV1) {
			continue
		}
		if string(fr) != channelsChangedWire {
			return n, fmt.Errorf("%s carried %s, want exactly %s", f.what, fr, channelsChangedWire)
		}
		n++
	}
	return n, nil
}

// ccWatch is the sockets the rows look at, with how many frames each had
// before the step being checked.
type ccWatch struct {
	feeds []*feed
	base  map[*feed]int
}

func (c *ccWatch) mark() error {
	c.base = map[*feed]int{}
	for _, f := range c.feeds {
		n, err := channelsChanged(f)
		if err != nil {
			return err
		}
		c.base[f] = n
	}
	return nil
}

// step runs act, waits for each of want's sockets to carry exactly one more
// frame, waits ccQuiet, and requires that every other socket carried none
// and that none closed.
func (c *ccWatch) step(row string, act func() error, want ...*feed) error {
	if err := c.mark(); err != nil {
		return err
	}
	if err := act(); err != nil {
		return fmt.Errorf("%s: %w", row, err)
	}
	for _, f := range want {
		if err := waitFor(f.what+" to carry channels_changed ("+row+")", 30*time.Second, func() (bool, string) {
			n, err := channelsChanged(f)
			if err != nil {
				return true, ""
			}
			return n > c.base[f], fmt.Sprintf("%d frames, %d of them channels_changed", len(f.received()), n)
		}); err != nil {
			return err
		}
	}
	select {
	case <-stopCtx.Done():
		return errInterrupted
	case <-time.After(ccQuiet):
	}
	for _, f := range c.feeds {
		n, err := channelsChanged(f)
		if err != nil {
			return fmt.Errorf("%s: %w", row, err)
		}
		expected := 0
		for _, wf := range want {
			if wf == f {
				expected = 1
			}
		}
		if got := n - c.base[f]; got != expected {
			return fmt.Errorf("%s: %s carried %d channels_changed frames, want %d", row, f.what, got, expected)
		}
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s: %s closed", row, f.what)
		}
	}
	return nil
}

// channelsChangedRows runs L1–L4 and L7 with bob's sockets (bsock), open
// since before A was active, and sockets of its own. It leaves cove with the
// operator alone in it.
func (w *world) channelsChangedRows(bsock *bSockets) (int, error) {
	ann, ola := w.a.people[0], w.b.people[2]
	cove, renamed := w.aName("cove"), w.aName("cove-renamed")
	w.marks.str("A's channel name", cove)
	w.marks.str("A's channel name", renamed)
	var created schema.CreateChannelResponse
	if err := w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: cove}, &created); err != nil {
		return 0, err
	}
	// Its id is not one of A's markers: ola, of B, is a member for a while,
	// and her socket is then owed its read state, which names it.

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
	annF, err := open(ann.api, "ann's events socket on "+w.a.main(), w.a.main())
	if err != nil {
		return 0, err
	}
	// The operator's is on cove itself: it stays a member throughout.
	opF, err := open(w.op, "the operator's events socket on cove", cove)
	if err != nil {
		return 0, err
	}
	olaF, err := open(ola.api, "ola's events socket on "+w.b.main(), w.b.main())
	if err != nil {
		return 0, err
	}
	aBot, err := open(w.a.agent.api, "A's agent's events socket on "+w.a.main(), w.a.main())
	if err != nil {
		return 0, err
	}
	bBot, err := open(w.b.agent.api, "B's agent's events socket on "+w.b.main(), w.b.main())
	if err != nil {
		return 0, err
	}
	c := &ccWatch{feeds: []*feed{annF, opF, olaF, aBot, bBot}}
	for _, f := range bsock.feeds {
		if strings.Contains(f.what, "/v1/events/ws") {
			c.feeds = append(c.feeds, f)
		}
	}
	// The operator created cove before its socket opened; that frame of its
	// own has had a floor to go.
	select {
	case <-stopCtx.Done():
		return 0, errInterrupted
	case <-time.After(ccQuiet):
	}

	member := func(method, channel string, m *member) func() error {
		return func() error {
			return w.op.call(method, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(channel), m.id), nil, nil)
		}
	}
	steps := []struct {
		row  string
		act  func() error
		want []*feed
	}{
		{"L1/L3/L4: ann added to cove", member(http.MethodPut, cove, ann), []*feed{annF}},
		{"ola added to cove (her own)", member(http.MethodPut, cove, ola), []*feed{olaF}},
		{"L2/L3/L4: ann removed from cove, ola a member", member(http.MethodDelete, cove, ann), []*feed{annF}},
		{"ola removed from cove (her own)", member(http.MethodDelete, cove, ola), []*feed{olaF}},
		{"ann added back to cove", member(http.MethodPut, cove, ann), []*feed{annF}},
		{"L7: cove renamed", func() error {
			return w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%d/name", created.Channel.ID), schema.RenameChannelRequestV1{From: cove, Name: renamed}, nil)
		}, []*feed{annF, opF}},
		{"ann removed from cove again", member(http.MethodDelete, renamed, ann), []*feed{annF}},
	}
	for _, s := range steps {
		if err := c.step(s.row, s.act, s.want...); err != nil {
			return 0, err
		}
	}
	// Nothing of A on B's own sockets of this phase, ola's and B's agent's.
	for _, f := range []*feed{olaF, bBot} {
		for i, fr := range f.received() {
			if leaks := w.leaks(fr, asked{}); len(leaks) > 0 {
				return 0, fmt.Errorf("frame %d on %s holds %s", i, f.what, strings.Join(leaks, "; "))
			}
		}
	}
	for _, f := range []*feed{annF, opF, olaF, aBot, bBot} {
		f.close()
	}
	return len(c.feeds), nil
}
