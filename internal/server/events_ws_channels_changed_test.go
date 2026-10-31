package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// conch.channels_changed.v1 on the events socket (issue #1027,
// docs/design/membership-live.md): a person's channel list follows changes to
// their memberships live. Each trigger sends one frame to each of its
// recipients' human sockets, after the roster floor, and nothing to anyone
// else: not another member, not an operator, not an unrelated person, not an
// agent.

// channelsChangedWire is the frame's only wire form (design §1, L4).
const channelsChangedWire = `{"schema":"conch.channels_changed.v1"}`

// ccFloor is the roster floor these tests run with, and ccSettle how long a
// socket must stay quiet afterwards to count as having been sent nothing:
// several floors, so a trailing frame that was owed has long since gone.
const (
	ccFloor  = 40 * time.Millisecond
	ccSettle = 8 * ccFloor
)

// watchedSocket reads an events socket on its own goroutine and keeps every
// frame, so a test can wait for some and then check that no more came.
type watchedSocket struct {
	name string
	conn *websocket.Conn

	mu     sync.Mutex
	frames [][]byte
	close  *websocket.CloseError
	ended  chan struct{}
}

func watchEvents(t *testing.T, base, path, token, name string) *watchedSocket {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("%s: dial %s = %v (status %d)", name, path, err, status)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	conn.SetReadLimit(1 << 20)
	w := &watchedSocket{name: name, conn: conn, ended: make(chan struct{})}
	t.Cleanup(func() { _ = conn.CloseNow(); <-w.ended })
	go func() {
		defer close(w.ended)
		for {
			_, raw, err := conn.Read(context.Background())
			if err != nil {
				var ce websocket.CloseError
				if errors.As(err, &ce) {
					w.mu.Lock()
					w.close = &ce
					w.mu.Unlock()
				}
				return
			}
			w.mu.Lock()
			w.frames = append(w.frames, raw)
			w.mu.Unlock()
		}
	}()
	// The first roster says the socket is live and subscribed.
	w.waitFor(t, "the first roster", func(raw []byte) bool { return schemaOf(raw) == schema.SessionRosterSchemaV1 })
	return w
}

func schemaOf(raw []byte) string {
	var head struct {
		Schema string `json:"schema"`
	}
	_ = json.Unmarshal(raw, &head)
	return head.Schema
}

// changed is every conch.channels_changed.v1 frame the socket has had.
func (w *watchedSocket) changed() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out [][]byte
	for _, raw := range w.frames {
		if schemaOf(raw) == schema.ChannelsChangedSchemaV1 {
			out = append(out, raw)
		}
	}
	return out
}

func (w *watchedSocket) waitFor(t *testing.T, what string, match func([]byte) bool) {
	t.Helper()
	deadline := time.Now().Add(eventsReadTimeout)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		for _, raw := range w.frames {
			if match(raw) {
				w.mu.Unlock()
				return
			}
		}
		w.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: no %s within %v", w.name, what, eventsReadTimeout)
}

// waitChanged waits until the socket has had n channels_changed frames.
func (w *watchedSocket) waitChanged(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(eventsReadTimeout)
	for time.Now().Before(deadline) {
		if len(w.changed()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: %d channels_changed frames within %v, want %d", w.name, len(w.changed()), eventsReadTimeout, n)
}

func (w *watchedSocket) closedWith() (*websocket.CloseError, bool) {
	select {
	case <-w.ended:
	case <-time.After(eventsReadTimeout):
		return nil, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.close, true
}

// ccFixture is the run fixture with the sockets every case watches. bob is
// the subject P, with two sockets on general (two tabs). alice is in general
// and in target, which bob is not in; root is an operator in general only;
// carol is in side alone; mute is an agent, in general and in target.
type ccFixture struct {
	*runFixture
	base         string
	target, side store.Channel
	sockets      map[string]*watchedSocket
}

func newCCFixture(t *testing.T) *ccFixture {
	t.Helper()
	ctx := context.Background()
	f := &ccFixture{runFixture: newRunFixture(t)}
	f.srv.eventsRosterFloor = ccFloor
	var err error
	if f.target, err = f.srv.store.CreateChannel(ctx, "target"); err != nil {
		t.Fatal(err)
	}
	if f.side, err = f.srv.store.CreateChannel(ctx, "side"); err != nil {
		t.Fatal(err)
	}
	f.join(t, f.target.ID, f.alice.ID, f.mute.ID)
	f.join(t, f.side.ID, f.carol.ID)
	f.base = wsTestServer(t, f.srv)
	return f
}

// watch opens every socket the cases look at.
func (f *ccFixture) watch(t *testing.T) {
	t.Helper()
	f.sockets = map[string]*watchedSocket{
		"subject":   watchEvents(t, f.base, "/v1/events/ws?channel=general", f.bobTok, "subject"),
		"subject-2": watchEvents(t, f.base, "/v1/events/ws?channel=general", f.bobTok, "subject-2"),
		"member":    watchEvents(t, f.base, "/v1/events/ws?channel=general", f.aliceTok, "member"),
		"operator":  watchEvents(t, f.base, "/v1/events/ws?channel=general", f.rootTok, "operator"),
		"unrelated": watchEvents(t, f.base, "/v1/events/ws?channel=side", f.carolTok, "unrelated"),
		"agent":     watchEvents(t, f.base, "/v1/events/ws?channel=general", f.muteTok, "agent"),
	}
}

func (f *ccFixture) must(t *testing.T, method, path, token, body string, want int) {
	t.Helper()
	if rec := f.do(t, method, path, token, body); rec.Code != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, rec.Code, rec.Body, want)
	}
}

// expect waits for each wanted socket's n frames, lets the rest settle, and
// checks every socket's count and every frame's bytes.
func (f *ccFixture) expect(t *testing.T, want map[string]int) {
	t.Helper()
	for name, n := range want {
		if n > 0 {
			f.sockets[name].waitChanged(t, n)
		}
	}
	time.Sleep(ccSettle)
	for name, sock := range f.sockets {
		got := sock.changed()
		if len(got) != want[name] {
			t.Errorf("%s: %d channels_changed frames, want %d", name, len(got), want[name])
		}
		for _, raw := range got {
			if string(raw) != channelsChangedWire {
				t.Errorf("%s: frame = %s, want exactly %s", name, raw, channelsChangedWire)
			}
		}
	}
}

// TestEventsChannelsChangedRecipients: each trigger of design §2 reaches only
// its recipients, once per socket, and a change that changed nothing sends
// nothing (L6).
func TestEventsChannelsChangedRecipients(t *testing.T) {
	subject := map[string]int{"subject": 1, "subject-2": 1}
	tests := []struct {
		name  string
		setup func(t *testing.T, f *ccFixture)
		act   func(t *testing.T, f *ccFixture)
		want  map[string]int
	}{
		{
			name: "added by id",
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "PUT", fmt.Sprintf("/v1/channels/target/members/%d", f.bob.ID), f.rootTok, "", http.StatusNoContent)
			},
			want: subject,
		},
		{
			name: "added by name",
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "POST", "/v1/channels/target/members", f.rootTok, `{"name":"bob"}`, http.StatusCreated)
			},
			want: subject,
		},
		{
			name:  "manages set by an operator",
			setup: func(t *testing.T, f *ccFixture) { f.join(t, f.target.ID, f.bob.ID) },
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "PUT", fmt.Sprintf("/v1/channels/target/members/%d", f.bob.ID), f.rootTok, `{"manages":true}`, http.StatusNoContent)
			},
			want: subject,
		},
		{
			name:  "removed",
			setup: func(t *testing.T, f *ccFixture) { f.join(t, f.target.ID, f.bob.ID) },
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "DELETE", fmt.Sprintf("/v1/channels/target/members/%d", f.bob.ID), f.rootTok, "", http.StatusNoContent)
			},
			want: subject,
		},
		{
			name: "a channel created, for its creator's other tabs",
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "POST", "/v0/channels", f.rootTok, `{"name":"fresh"}`, http.StatusCreated)
			},
			want: map[string]int{"operator": 1},
		},
		{
			// L7: every human member of the channel, and no one else; mute,
			// an agent member, is not sent it.
			name:  "a channel renamed",
			setup: func(t *testing.T, f *ccFixture) { f.join(t, f.target.ID, f.bob.ID) },
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "PUT", fmt.Sprintf("/v1/channels/%d/name", f.target.ID), f.rootTok, `{"from":"target","name":"renamed"}`, http.StatusOK)
			},
			want: map[string]int{"subject": 1, "subject-2": 1, "member": 1},
		},
		{
			name: "a rename to the same name changes nothing",
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "PUT", fmt.Sprintf("/v1/channels/%d/name", f.target.ID), f.rootTok, `{"from":"target","name":"target"}`, http.StatusOK)
			},
			want: map[string]int{},
		},
		{
			name: "a removal that removed nobody",
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "DELETE", fmt.Sprintf("/v1/channels/target/members/%d", f.bob.ID), f.rootTok, "", http.StatusNoContent)
			},
			want: map[string]int{},
		},
		{
			name:  "a PUT that neither added nor set manages",
			setup: func(t *testing.T, f *ccFixture) { f.join(t, f.target.ID, f.bob.ID) },
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "PUT", fmt.Sprintf("/v1/channels/target/members/%d", f.bob.ID), f.rootTok, "", http.StatusNoContent)
			},
			want: map[string]int{},
		},
		{
			name:  "an add by name of someone already in",
			setup: func(t *testing.T, f *ccFixture) { f.join(t, f.target.ID, f.bob.ID) },
			act: func(t *testing.T, f *ccFixture) {
				f.must(t, "POST", "/v1/channels/target/members", f.rootTok, `{"name":"bob"}`, http.StatusOK)
			},
			want: map[string]int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCCFixture(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			f.watch(t)
			tt.act(t, f)
			f.expect(t, tt.want)
		})
	}
}

// TestEventsChannelsChangedCoalesces: three changes to one person within the
// floor send one frame per socket, and only after the floor (trailing edge,
// design §3).
func TestEventsChannelsChangedCoalesces(t *testing.T) {
	f := newCCFixture(t)
	const floor = 400 * time.Millisecond
	f.srv.eventsRosterFloor = floor
	f.watch(t)
	start := time.Now()
	f.must(t, "PUT", fmt.Sprintf("/v1/channels/target/members/%d", f.bob.ID), f.rootTok, "", http.StatusNoContent)
	f.must(t, "PUT", fmt.Sprintf("/v1/channels/target/members/%d", f.bob.ID), f.rootTok, `{"manages":true}`, http.StatusNoContent)
	f.must(t, "POST", "/v1/channels/side/members", f.rootTok, `{"name":"bob"}`, http.StatusCreated)
	if spent := time.Since(start); spent >= floor/2 {
		t.Skipf("the three changes took %v, too long to fall inside one %v floor on this machine", spent, floor)
	}
	f.sockets["subject"].waitChanged(t, 1)
	if waited := time.Since(start); waited < floor {
		t.Errorf("the frame came %v after the first change, inside the %v floor", waited, floor)
	}
	time.Sleep(2 * floor)
	for _, name := range []string{"subject", "subject-2"} {
		if n := len(f.sockets[name].changed()); n != 1 {
			t.Errorf("%s: %d channels_changed frames for three changes within the floor, want 1", name, n)
		}
	}
	for _, name := range []string{"member", "operator", "unrelated", "agent"} {
		if n := len(f.sockets[name].changed()); n != 0 {
			t.Errorf("%s: %d channels_changed frames, want none", name, n)
		}
	}
}

// TestEventsChannelsChangedRemovedOpenChannel: a person removed from the
// channel a socket of theirs is open on loses that socket (1008, removed)
// with no channels_changed on it, and their socket on another channel is
// sent one (design §1).
func TestEventsChannelsChangedRemovedOpenChannel(t *testing.T) {
	f := newCCFixture(t)
	f.join(t, f.target.ID, f.bob.ID)
	f.watch(t)
	open := watchEvents(t, f.base, "/v1/events/ws?channel=target", f.bobTok, "subject on target")
	f.must(t, "DELETE", fmt.Sprintf("/v1/channels/target/members/%d", f.bob.ID), f.rootTok, "", http.StatusNoContent)
	ce, ok := open.closedWith()
	switch {
	case !ok:
		t.Fatal("the socket on the channel bob lost did not close")
	case ce == nil:
		t.Fatal("the socket on the channel bob lost ended without a close frame")
	case ce.Code != websocket.StatusPolicyViolation || ce.Reason != "no longer a member of this channel":
		t.Errorf("close = %d %q, want 1008 %q", ce.Code, ce.Reason, "no longer a member of this channel")
	}
	if got := open.changed(); len(got) != 0 {
		t.Errorf("the closed socket carried %d channels_changed frames: %s", len(got), got)
	}
	f.expect(t, map[string]int{"subject": 1, "subject-2": 1})
}

// TestEventsChannelsChangedJoinAccount: an account joining from the bare
// address is new, so no socket of its own is open, and it sends nothing to
// anyone else's: not the channel's members, an operator or an agent.
func TestEventsChannelsChangedJoinAccount(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.srv.eventsRosterFloor = ccFloor
	setAgentManifest(t, f.srv, f.bot.ID, []schema.Capability{schema.CapabilityMessagesRead}, f.red.ID)
	if _, err := f.srv.store.AddChannelMember(context.Background(), "system", f.red.ID, f.bot.ID, 0); err != nil {
		t.Fatal(err)
	}
	f.mustPutJoin(t, openAccounts("red"))
	base := wsTestServer(t, f.srv)
	sockets := map[string]*watchedSocket{
		"member":   watchEvents(t, base, "/v1/events/ws?channel=red", f.aliceTok, "member"),
		"operator": watchEvents(t, base, "/v1/events/ws?channel=general", f.rootTok, "operator"),
		"agent":    watchEvents(t, base, "/v1/events/ws?channel=red", f.botTok, "agent"),
	}
	acct := mustAccount(t, f.joinAccount(t, "newcomer", ""))
	// The newcomer's list over REST holds red: the join did change it.
	if len(acct.Channels) != 1 || acct.Channels[0].ID != f.red.ID {
		t.Fatalf("account channels = %+v", acct.Channels)
	}
	// A frame the join had owed the members' roster (the newcomer is on
	// red's member list) shows the socket was woken; nothing else is owed.
	sockets["member"].waitFor(t, "red's member list with the newcomer", func(raw []byte) bool {
		var d schema.ChannelMembersV1
		if schemaOf(raw) != schema.ChannelMembersSchemaV1 || json.Unmarshal(raw, &d) != nil {
			return false
		}
		_, ok := listsMember(d, acct.Principal.ID)
		return ok
	})
	time.Sleep(ccSettle)
	for name, sock := range sockets {
		if n := len(sock.changed()); n != 0 {
			t.Errorf("%s: %d channels_changed frames for another principal's join, want none", name, n)
		}
	}
}

// TestEventsHubNotifyChannelsChanged pins the helper's targeting directly:
// only human subscriptions of the named principals, never a zero id.
func TestEventsHubNotifyChannelsChanged(t *testing.T) {
	tests := []struct {
		name string
		ids  []int64
		want map[string]bool
	}{
		{name: "one principal, every human socket", ids: []int64{7}, want: map[string]bool{"7 on 1": true, "7 on 2": true}},
		{name: "two principals", ids: []int64{7, 8}, want: map[string]bool{"7 on 1": true, "7 on 2": true, "8 on 1": true}},
		{name: "a zero id matches nothing", ids: []int64{0}, want: map[string]bool{}},
		{name: "no ids", want: map[string]bool{}},
		{name: "an agent's socket is never owed it", ids: []int64{9}, want: map[string]bool{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newEventsHub()
			subs := map[string]*eventsSub{
				"7 on 1": h.subscribeKind(1, 7, 70, true),
				"7 on 2": h.subscribeKind(2, 7, 71, true),
				"8 on 1": h.subscribeKind(1, 8, 80, true),
				"9 on 1": h.subscribeKind(1, 9, 90, false),
				"0 on 1": h.subscribeKind(1, 0, 0, true),
			}
			for _, sub := range subs {
				sub.take() // what subscribe owed
			}
			h.notifyChannelsChanged(tt.ids...)
			for name, sub := range subs {
				_, _, changed, refs, live := sub.take()
				if !live {
					t.Fatalf("%s: dropped", name)
				}
				if changed != tt.want[name] {
					t.Errorf("%s: channelsChanged = %v, want %v", name, changed, tt.want[name])
				}
				if len(refs) != 0 {
					t.Errorf("%s: %d refs queued, want none", name, len(refs))
				}
			}
		})
	}
}
