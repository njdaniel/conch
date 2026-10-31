package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// The events socket's lifetime (issue #269): what closes it, what a slow
// reader costs a writer, and what the hub does with a notification.

// TestEventsClosesWhenAccessIsLost: removal from the channel and loss of the
// credential close the socket with 1008 and the existing reasons, at once for
// the drop calls /v2/ws honours and within the re-check interval for the rest,
// and another subscriber is not disturbed.
func TestEventsClosesWhenAccessIsLost(t *testing.T) {
	tests := []struct {
		name   string
		act    func(t *testing.T, f *runFixture)
		reason string
	}{
		{
			name: "removed through the API",
			act: func(t *testing.T, f *runFixture) {
				if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("remove member = %d %s", rec.Code, rec.Body)
				}
			},
			reason: "no longer a member of this channel",
		},
		{
			name: "removed behind the server's back, found by the re-check",
			act: func(t *testing.T, f *runFixture) {
				if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.general.ID, f.bob.ID); err != nil {
					t.Fatal(err)
				}
			},
			reason: "no longer a member of this channel",
		},
		{
			name: "credential revoked through the API",
			act: func(t *testing.T, f *runFixture) {
				id := f.credentialID(t, f.bobTok)
				if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", id), f.rootTok, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
				}
			},
			reason: reasonCredentialInvalid,
		},
		{
			name: "credential revoked behind the server's back, found by the re-check",
			act: func(t *testing.T, f *runFixture) {
				if err := f.srv.store.RevokeCredential(context.Background(), "system", f.credentialID(t, f.bobTok)); err != nil {
					t.Fatal(err)
				}
			},
			reason: reasonCredentialInvalid,
		},
		{
			name: "principal disabled",
			act: func(t *testing.T, f *runFixture) {
				if rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("disable = %d %s", rec.Code, rec.Body)
				}
			},
			reason: reasonCredentialInvalid,
		},
		{
			name: "every credential revoked",
			act: func(t *testing.T, f *runFixture) {
				if rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusOK {
					t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
				}
			},
			reason: reasonCredentialInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRunFixture(t)
			// Short, so the cases that wait for the re-check do not take 30 s.
			// Nothing sleeps: the read below returns when the socket closes.
			f.srv.credRecheckInterval = 20 * time.Millisecond
			base := wsTestServer(t, f.srv)
			alice := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
			bob := dialEvents(t, base, "/v1/events/ws?channel=general", f.bobTok)
			alice.next()
			bob.next()

			tt.act(t, f)
			ce := bob.closed()
			if ce.Code != websocket.StatusPolicyViolation || ce.Reason != tt.reason {
				t.Errorf("bob's socket closed with %d %q, want 1008 %q", ce.Code, ce.Reason, tt.reason)
			}

			// Alice is unaffected: a change still reaches her.
			f.create(t, f.aliceTok, humanBody)
			alice.until("a roster with her session", isRoster(rosterHas(f.alice.ID, schema.SessionStatusOnline)))
		})
	}

	t.Run("an agent's manifest stops permitting the channel", func(t *testing.T) {
		f := newRunFixture(t)
		base := wsTestServer(t, f.srv)
		bot := dialEvents(t, base, "/v1/events/ws?channel=general", f.botTok)
		bot.next()
		body, err := json.Marshal(schema.PutAgentManifestRequestV1{
			DisplayName: "test agent", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
			Channels: []schema.ChannelGrant{{ChannelID: f.private.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if rec := f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/manifest", f.bot.ID), f.rootTok, string(body)); rec.Code != http.StatusOK {
			t.Fatalf("put manifest = %d %s", rec.Code, rec.Body)
		}
		ce := bot.closed()
		if ce.Code != websocket.StatusPolicyViolation || ce.Reason != "manifest no longer permits reading this channel" {
			t.Errorf("the agent's socket closed with %d %q", ce.Code, ce.Reason)
		}
	})
}

// TestEventsSlowSubscriberDoesNotDelayAWriter: a subscriber that is stuck in a
// write (the test holds it there, as a peer that never reads would) is closed
// with the slow-consumer reason once its queue is full, and a writer posting
// activity is not slowed by it at any point.
func TestEventsSlowSubscriberDoesNotDelayAWriter(t *testing.T) {
	f := newRunFixture(t)
	base := wsTestServer(t, f.srv)
	sess := f.session(t, f.botTok)
	run := f.start(t, f.botTok, sess, "general", 0, "long job")

	var armed atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	f.srv.eventsBeforeWrite = func() {
		if armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
	}
	alice := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	alice.next()
	armed.Store(true)

	post := func() {
		rec := f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody)
		if rec.Code != http.StatusCreated {
			t.Errorf("append activity = %d %s", rec.Code, rec.Body)
		}
	}
	post()
	select {
	case <-entered:
	case <-time.After(eventsReadTimeout):
		t.Fatal("the subscriber never reached its write")
	}

	done := make(chan time.Duration)
	go func() {
		var slowest time.Duration
		// More than the queue holds, while the subscriber is stuck.
		for range eventsQueueMax + 50 {
			start := time.Now()
			post()
			slowest = max(slowest, time.Since(start))
		}
		done <- slowest
	}()
	select {
	case slowest := <-done:
		if slowest > 3*time.Second {
			t.Errorf("one POST took %v while a subscriber was stuck", slowest)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a writer was held up by a subscriber that does not read")
	}
	if n := f.srv.eventSubscribers(); n != 0 {
		t.Errorf("%d subscribers are still registered; the slow one should have been dropped", n)
	}

	close(release)
	ce := alice.closed()
	if ce.Code != websocket.StatusPolicyViolation || ce.Reason != "subscriber too slow" {
		t.Errorf("the slow subscriber's socket closed with %d %q, want 1008 \"subscriber too slow\"", ce.Code, ce.Reason)
	}
}

// TestEventsHub: what a writer's notification does to the subscribers, with
// no socket in the way.
func TestEventsHub(t *testing.T) {
	const (
		general = int64(1)
		other   = int64(2)
	)
	pending := func(t *testing.T, sub *eventsSub) (bool, []eventRef) {
		t.Helper()
		roster, _, _, refs, live := sub.take()
		if !live {
			t.Fatal("the subscription was dropped")
		}
		return roster, refs
	}
	tests := []struct {
		name       string
		act        func(h *eventsHub)
		wantRoster [2]bool // general's subscriber, other's
		wantRefs   [2]int
	}{
		{"rosters reach everyone", func(h *eventsHub) { h.notifyRosters() }, [2]bool{true, true}, [2]int{0, 0}},
		{"a channel's roster reaches that channel", func(h *eventsHub) { h.notifyChannelRoster(general) }, [2]bool{true, false}, [2]int{0, 0}},
		{"a run reaches its channel, its status everyone's roster", func(h *eventsHub) { h.notifyRun(general, 7) }, [2]bool{true, true}, [2]int{1, 0}},
		{"a run is queued once", func(h *eventsHub) { h.notifyRun(general, 7); h.notifyRun(general, 7) }, [2]bool{true, true}, [2]int{1, 0}},
		{"two runs are two", func(h *eventsHub) { h.notifyRun(general, 7); h.notifyRun(general, 8) }, [2]bool{true, true}, [2]int{2, 0}},
		{"every activity entry is its own", func(h *eventsHub) { h.notifyActivity(general, 7, 1); h.notifyActivity(general, 7, 2) }, [2]bool{false, false}, [2]int{2, 0}},
		{"an intervention reaches its channel only", func(h *eventsHub) { h.notifyIntervention(other, 3) }, [2]bool{false, false}, [2]int{0, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newEventsHub()
			subs := [2]*eventsSub{h.subscribe(general, 10, 0), h.subscribe(other, 11, 0)}
			// Each is owed its first roster from the start (subscribe).
			for i, sub := range subs {
				if roster, refs := pending(t, sub); !roster || len(refs) != 0 {
					t.Fatalf("subscriber %d: owed roster %v, %d refs on subscribing; want the roster alone", i, roster, len(refs))
				}
			}
			tt.act(h)
			for i, sub := range subs {
				roster, refs := pending(t, sub)
				if roster != tt.wantRoster[i] || len(refs) != tt.wantRefs[i] {
					t.Errorf("subscriber %d: roster %v, %d refs; want %v, %d", i, roster, len(refs), tt.wantRoster[i], tt.wantRefs[i])
				}
			}
		})
	}

	t.Run("a full queue drops the subscriber and the writer carries on", func(t *testing.T) {
		h := newEventsHub()
		slow, fine := h.subscribe(general, 10, 0), h.subscribe(general, 11, 0)
		for i := range eventsQueueMax + 1 {
			h.notifyActivity(general, 7, int64(i+1))
			fine.take() // this one keeps up
		}
		select {
		case <-slow.dropped:
		default:
			t.Fatal("the subscriber with a full queue was not dropped")
		}
		select {
		case <-fine.dropped:
			t.Fatal("the subscriber that keeps up was dropped")
		default:
		}
	})

	t.Run("dropping a principal leaves the others", func(t *testing.T) {
		h := newEventsHub()
		a, b, c := h.subscribe(general, 10, 0), h.subscribe(other, 10, 0), h.subscribe(general, 11, 0)
		h.dropPrincipal(general, 10)
		for sub, wantDropped := range map[*eventsSub]bool{a: true, b: false, c: false} {
			select {
			case <-sub.dropped:
				if !wantDropped {
					t.Errorf("subscriber of principal %d on channel %d was dropped", sub.principalID, sub.channelID)
				}
			default:
				if wantDropped {
					t.Error("the subscriber was not dropped")
				}
			}
		}
		h.dropPrincipal(general, 0) // no caller matches nothing
		select {
		case <-c.dropped:
			t.Error("a zero principal dropped someone")
		default:
		}
		h.dropPrincipalAll(10)
		select {
		case <-b.dropped:
		default:
			t.Error("dropPrincipalAll left a subscriber of the principal")
		}
	})

	t.Run("dropping a credential leaves the principal's others", func(t *testing.T) {
		h := newEventsHub()
		a, b := h.subscribe(general, 10, 100), h.subscribe(other, 10, 100)
		c, anon := h.subscribe(general, 10, 101), h.subscribe(general, 0, 0)
		h.dropCredential(100)
		h.dropCredential(0) // no credential matches nothing
		for sub, wantDropped := range map[*eventsSub]bool{a: true, b: true, c: false, anon: false} {
			select {
			case <-sub.dropped:
				if !wantDropped {
					t.Errorf("subscriber of credential %d on channel %d was dropped", sub.credentialID, sub.channelID)
				}
			default:
				if wantDropped {
					t.Errorf("subscriber of credential %d on channel %d was not dropped", sub.credentialID, sub.channelID)
				}
			}
		}
	})

	t.Run("shutdown drops everyone and refuses more", func(t *testing.T) {
		h := newEventsHub()
		sub := h.subscribe(general, 10, 0)
		h.closeAll()
		for name, s := range map[string]*eventsSub{"registered": sub, "late": h.subscribe(general, 11, 0)} {
			select {
			case <-s.dropped:
			default:
				t.Errorf("%s subscriber was not dropped", name)
			}
		}
		if !h.isClosed() {
			t.Error("the hub does not say it is closed")
		}
	})
}

// TestEventsRosterFloor: one subscriber rebuilds its roster at most once per
// floor, however many changes owe it one, and the roster it sends then holds
// every change. A principal looping cheap writes cannot keep every
// subscriber's goroutine reading the store (the security review of #308).
func TestEventsRosterFloor(t *testing.T) {
	f := newSessionFixture(t)
	const floor = 600 * time.Millisecond
	f.srv.eventsRosterFloor = floor
	base := wsTestServer(t, f.srv)
	start := time.Now()
	c := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	if fr := c.next(); fr.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("first frame = %v, want the roster", fr)
	}
	const changes = 5
	for range changes {
		f.create(t, f.bobTok, `{"client":"tui"}`)
	}
	bobs := func(r schema.SessionRosterV1) int {
		n := 0
		for _, s := range r.Sessions {
			if s.PrincipalID == f.bob.ID {
				n++
			}
		}
		return n
	}
	// The first roster was built after start, so the next cannot be built
	// before start+floor.
	fr := c.next()
	if fr.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("next frame = %v, want a roster", fr)
	}
	if waited := time.Since(start); waited < floor {
		t.Errorf("a second roster after %v, inside the %v floor", waited, floor)
	}
	// Rosters owed meanwhile coalesce, so far fewer rosters than changes
	// arrive before one holds them all: this one when the five posts beat the
	// floor, at worst the next on a slow machine.
	if bobs(fr.roster) != changes {
		seen := c.until("a roster with all of bob's sessions", isRoster(func(r schema.SessionRosterV1) bool { return bobs(r) == changes }))
		if len(seen) > 1 {
			t.Errorf("%d more rosters before one with all %d changes: they did not coalesce", len(seen)+1, changes)
		}
	}
}
