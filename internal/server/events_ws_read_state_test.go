package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func untilReadState(t *testing.T, conn *eventsClient, channelID int64) schema.ReadStateV1 {
	t.Helper()
	frames := conn.until("read state", func(fr eventFrame) bool {
		return fr.schema == schema.ReadStateSchemaV1 && fr.readState.ChannelID == channelID
	})
	return frames[len(frames)-1].readState
}

func queuedReadStates(s *Server, principalID int64) int {
	s.events.mu.Lock()
	defer s.events.mu.Unlock()
	n := 0
	for sub := range s.events.subs {
		if sub.principalID != principalID {
			continue
		}
		if sub.readState {
			n++
		}
	}
	return n
}

func TestEventsReadStateCoalescesAndSkipsAgents(t *testing.T) {
	hub := newEventsHub()
	human := hub.subscribeKind(1, 10, 0, true)
	channels := make([]store.Channel, 300)
	for i := range channels {
		channels[i].ID = int64(i)
	}
	human.setMemberships(channels)
	agent := hub.subscribeKind(1, 20, 0, false)
	for i := range 300 {
		hub.notifyReadState(int64(i), 99)
		hub.notifyReadMarked(int64(i), 10)
	}
	if !human.readState || len(human.refs) != 0 {
		t.Fatalf("human read-state refresh = %v with %d references, want one flag and no references", human.readState, len(human.refs))
	}
	if agent.readState || len(agent.refs) != 0 {
		t.Fatalf("agent received read-state work: refresh=%v references=%d", agent.readState, len(agent.refs))
	}
}

func TestEventsReadStateFollowsMessagesAndMarks(t *testing.T) {
	f := newScopedFixture(t)
	ann := dialEvents(t, f.base, "/v1/events/ws?channel=ops", f.p("ann").token)
	annOther := dialEvents(t, f.base, "/v1/events/ws?channel=ops", f.p("ann").token)
	olga := dialEvents(t, f.base, "/v1/events/ws?channel=ops", f.p("olga").token)
	for who, conn := range map[string]*eventsClient{"ann": ann, "ann's other client": annOther, "olga": olga} {
		if first := conn.next(); first.schema != schema.SessionRosterSchemaV1 {
			t.Fatalf("%s: first frame = %v, want roster", who, first)
		}
	}

	f.mustPost(t, "ann", `{"body":"the author's own post"}`)
	if got := queuedReadStates(f.srv, f.p("ann").ID); got != 0 {
		t.Fatalf("the author has %d unread notifications for their own post, want 0", got)
	}
	if got := untilReadState(t, olga, f.ops.ID).Unread; got != 1 {
		t.Fatalf("olga unread = %d, want 1", got)
	}

	netID := f.mustPost(t, "nina", `{"body":"net1 only",`+netAudience(f.net1.ID)+`}`)
	for who, conn := range map[string]*eventsClient{"ann": ann, "ann's other client": annOther} {
		if got := untilReadState(t, conn, f.ops.ID).Unread; got != 1 {
			t.Errorf("%s unread = %d, want 1", who, got)
		}
	}
	if got := f.unread(t, "olga"); got != 1 {
		t.Errorf("olga, outside net1, unread = %d, want 1", got)
	}

	res, _ := f.putRead(t, f.p("ann").token, "ops", netID)
	if res.status != http.StatusOK {
		t.Fatalf("mark read = %d %s", res.status, res.body)
	}
	for who, conn := range map[string]*eventsClient{"ann": ann, "ann's other client": annOther} {
		got := untilReadState(t, conn, f.ops.ID)
		if got.LastReadID != netID || got.Unread != 0 {
			t.Errorf("%s after shared mark = %+v, want mark %d and no unread", who, got, netID)
		}
	}
}

func TestEventsReadStateDropsAnInvalidatedWrite(t *testing.T) {
	f := newScopedFixture(t)
	conn := dialEvents(t, f.base, "/v1/events/ws?channel=ops", f.p("ann").token)
	if first := conn.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("first frame = %v, want roster", first)
	}
	// Only read-state frames are counted and only the first is invalidated.
	// The hook sees them alone: eventsBeforeWrite also runs for the message
	// notification the same post queues, and whether that goes out before,
	// after or instead of the read state depends on the read-state floor and
	// on how the socket's wakes fall, which a loaded machine changes (#958).
	var writes atomic.Int32
	f.srv.eventsBeforeEpochWrite = func(frame []byte) {
		if frameSchema(frame) != schema.ReadStateSchemaV1 || writes.Add(1) != 1 {
			return
		}
		f.srv.events.mu.Lock()
		for sub := range f.srv.events.subs {
			if sub.principalID == f.p("ann").ID {
				sub.invalidateReadState()
			}
		}
		f.srv.events.mu.Unlock()
	}
	defer func() { f.srv.eventsBeforeEpochWrite = nil }()
	f.mustPost(t, "olga", `{"body":"invalidate the first build"}`)
	if got := untilReadState(t, conn, f.ops.ID).Unread; got != 1 {
		t.Fatalf("unread after the clean rebuild = %d, want 1", got)
	}
	if got := writes.Load(); got != 2 {
		t.Fatalf("read-state write attempts = %d, want the invalidated attempt and one clean write", got)
	}
}

// frameSchema is the schema an encoded events frame names.
func frameSchema(frame []byte) string {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(frame, &head); err != nil {
		return ""
	}
	return head.Schema
}

func TestEventsReadStateSkipsRevokedMember(t *testing.T) {
	f := newScopedFixture(t)
	res := f.callREST(t, "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.p("ann").ID), f.rootTok, "")
	if res.status != http.StatusNoContent {
		t.Fatalf("remove member = %d %s", res.status, res.body)
	}
	sub := f.srv.events.subscribeKind(f.ops.ID, f.p("ann").ID, 0, true)
	defer sub.cancel()
	stream := &eventsStream{s: f.srv, sub: sub, principalID: f.p("ann").ID}
	if !stream.sendReadStates(context.Background()) {
		t.Fatal("a revoked member's empty rebuild closed the stream")
	}
	if len(stream.lastReadStates) != 0 {
		t.Fatalf("revoked member has %d read states, want none", len(stream.lastReadStates))
	}
}
