package server

import (
	"fmt"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func nextNotification(t *testing.T, conn *eventsClient) schema.MessageV2 {
	t.Helper()
	frames := conn.until("notification message", func(fr eventFrame) bool { return fr.schema == schema.MessageSchemaV2 })
	return frames[len(frames)-1].message
}

func queuedNotifications(s *Server, principalID int64) int {
	s.events.mu.Lock()
	defer s.events.mu.Unlock()
	n := 0
	for sub := range s.events.subs {
		if sub.principalID != principalID {
			continue
		}
		for _, ref := range sub.refs {
			if ref.kind == eventMessage {
				n++
			}
		}
	}
	return n
}

func TestEventsNotificationsAreLiveAndScoped(t *testing.T) {
	f := newScopedFixture(t)
	before := f.mustPost(t, "olga", `{"body":"before the socket"}`)
	ann := dialEvents(t, f.base, "/v1/events/ws?channel=ops", f.p("ann").token)
	olga := dialEvents(t, f.base, "/v1/events/ws?channel=ops", f.p("olga").token)
	agent := dialEvents(t, f.base, "/v1/events/ws?channel=ops", f.p("aria").token)
	for _, conn := range []*eventsClient{ann, olga, agent} {
		if first := conn.next(); first.schema != schema.SessionRosterSchemaV1 {
			t.Fatalf("first frame = %v, want roster", first)
		}
	}

	f.mustPost(t, "ann", `{"body":"from ann"}`)
	got := nextNotification(t, olga)
	if got.ID == before || got.Body != "from ann" {
		t.Fatalf("notification = %+v, want only the live message", got)
	}
	if got := queuedNotifications(f.srv, f.p("ann").ID); got != 0 {
		t.Errorf("author has %d queued notifications, want none", got)
	}
	if got := queuedNotifications(f.srv, f.p("aria").ID); got != 0 {
		t.Errorf("agent has %d queued notifications, want none", got)
	}

	f.mustPost(t, "nina", `{"body":"net1",`+netAudience(f.net1.ID)+`}`)
	if got := nextNotification(t, ann).Body; got != "net1" {
		t.Errorf("net notification body = %q", got)
	}
	if got := queuedNotifications(f.srv, f.p("olga").ID); got != 0 {
		t.Errorf("outside-net reader has %d queued notifications, want none", got)
	}
	f.mustPost(t, "nina", `{"body":"whisper to ann",`+whisperAudience(f.p("ann").ID)+`}`)
	if got := nextNotification(t, ann).Body; got != "whisper to ann" {
		t.Errorf("whisper notification body = %q", got)
	}

	f.mustPost(t, "aria", `{"body":"from an agent"}`)
	if got := nextNotification(t, ann).Body; got != "from an agent" {
		t.Errorf("agent-authored notification body = %q", got)
	}
	root := f.mustPost(t, "olga", `{"body":"root"}`)
	if got := nextNotification(t, ann).Body; got != "root" {
		t.Errorf("root notification body = %q", got)
	}
	f.mustPost(t, "nina", fmt.Sprintf(`{"body":"reply","thread_root_id":%d}`, root))
	if got := nextNotification(t, ann).Body; got != "reply" {
		t.Errorf("reply notification body = %q", got)
	}
}

func TestEventsNotificationsHonorGuestHistoryWatermark(t *testing.T) {
	w := newBoundWorld(t)
	guest := dialEvents(t, w.base, "/v1/events/ws?channel=ops", w.guest.Token)
	if first := guest.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("first frame = %v, want roster", first)
	}
	w.mustPost(t, "ann", `{"body":"after admission"}`)
	if got := nextNotification(t, guest); got.Body != "after admission" || got.ID == w.preWide {
		t.Fatalf("guest notification = %+v, want only post-admission content", got)
	}
}

func TestEventsNotificationsDropMessageOnRevocation(t *testing.T) {
	f := newScopedFixture(t)
	ctx := f.srv.store
	side, err := ctx.CreateChannel(t.Context(), "side")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{f.p("ann").ID, f.p("olga").ID} {
		if _, err := ctx.AddChannelMember(t.Context(), "system", side.ID, id, 0); err != nil {
			t.Fatal(err)
		}
	}
	ann := dialEvents(t, f.base, "/v1/events/ws?channel=ops", f.p("ann").token)
	if first := ann.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("first frame = %v, want roster", first)
	}
	f.srv.eventsBeforeWrite = func() {
		f.srv.events.dropPrincipal(side.ID, f.p("ann").ID)
		f.srv.eventsBeforeWrite = nil
	}
	defer func() { f.srv.eventsBeforeWrite = nil }()
	f.mustPost(t, "olga", `{"body":"private side content"}`)
	f.mustPost(t, "olga", `{"body":"public ops content"}`)
	var frames []eventFrame
	foundPublic, foundTombstone := false, false
	for range 64 {
		fr := ann.next()
		frames = append(frames, fr)
		if fr.schema == schema.MessageSchemaV2 && fr.message.ChannelID == f.ops.ID && fr.message.Body == "public ops content" {
			foundPublic = true
		}
		if fr.schema == schema.ReadStateSchemaV1 && fr.readState.ChannelID == side.ID && fr.readState.Unread == 0 && fr.readState.LastReadID == 0 {
			foundTombstone = true
		}
		if fr.schema == schema.MessageSchemaV2 && fr.message.Body == "private side content" {
			t.Fatalf("revocation leaked the private message: %+v", fr.message)
		}
		if foundPublic && foundTombstone {
			break
		}
	}
	if !foundPublic || !foundTombstone {
		t.Fatalf("missing public notification or tombstone: %v", frames)
	}
}

func TestEventsMembershipRefreshDoesNotResurrectRevoked(t *testing.T) {
	f := newScopedFixture(t)
	sub := f.srv.events.subscribeKind(f.ops.ID, f.p("ann").ID, 0, true)
	defer sub.cancel()
	epoch := sub.readStateVersion()
	f.srv.events.dropPrincipal(f.ops.ID, f.p("ann").ID)
	if sub.setMembershipsIfCurrent([]store.Channel{f.ops}, epoch) {
		t.Fatal("an older membership list replaced the revoked snapshot")
	}
	if sub.hasMembership(f.ops.ID) {
		t.Fatal("the revoked channel was resurrected")
	}
}
