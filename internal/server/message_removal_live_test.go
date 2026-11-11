package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// removed returns the frames that are the tombstone of id.
func tombstones(frames []liveFrame, id int64) []liveFrame {
	var out []liveFrame
	for _, f := range frames {
		switch {
		case f.Schema == schema.MessageRemovedSchemaV1 && f.Message != nil && f.Message.ID == id:
			out = append(out, f)
		case f.ID == id && f.Body == schema.MessageRemovedBody:
			out = append(out, f)
		}
	}
	return out
}

// removedEvents are the events-socket conch.message_removed.v1 frames for id.
func removedEvents(frames []liveFrame, id int64) []liveFrame {
	var out []liveFrame
	for _, f := range frames {
		if f.Schema == schema.MessageRemovedSchemaV1 && f.Message != nil && f.Message.ID == id {
			out = append(out, f)
		}
	}
	return out
}

// The live half (§7.4): the tombstone, and only it, reaches exactly the
// sockets that could have the message, in the envelope each speaks; the
// Broadcaster sees the safe v0 for a channel-wide removal and nothing for a
// scoped one; the events socket gets conch.message_removed.v1 for those who
// can read it; no original is on any wire; nobody is notified and no read state
// moves; and nobody is disconnected.
func TestMessageRemovalLive(t *testing.T) {
	w := newRemovalWorld(t)
	f := w.fileFixture
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet)
	people := []string{"ann", "nina", "mona", "wes", "olga", "dina", "root", "aria"}
	v2 := map[string]*frameFeed{}
	ev := map[string]*frameFeed{}
	for _, who := range people {
		v2[who] = feedOf(t, openSocket(t, f.base, "/v2/ws?channel=ops", f.p(who).token))
		ev[who] = feedOf(t, openSocket(t, f.base, "/v1/events/ws?channel=ops", f.p(who).token))
	}
	v0 := feedOf(t, openSocket(t, f.base, "/v0/ws?channel=ops", f.p("olga").token))
	v1 := feedOf(t, openSocket(t, f.base, "/v1/ws?channel=ops", f.p("olga").token))
	time.Sleep(400 * time.Millisecond)
	all := append([]*frameFeed{v0, v1}, mapValues(v2, ev)...)
	for _, feed := range all {
		feed.take(0)
	}
	readBefore := map[string]string{}
	for _, who := range []string{"ann", "nina", "olga", "wes"} {
		readBefore[who] = f.callREST(t, "GET", "/v1/read-state", f.p(who).token, "").body
	}
	tapBefore := len(f.rec.messages())

	// A scoped removal first: the broadcaster must see nothing. ann and wes
	// are made managers: a manager removes what they can read.
	if res := w.remove(t, "ann", w.whisper); res.status != 403 {
		t.Fatalf("a plain member's removal = %d %s", res.status, res.body)
	}
	no, yes := false, true
	for _, who := range []string{"ann", "wes"} {
		if _, err := f.srv.store.PutChannelMember(context.Background(), "system", f.ops.ID, f.p(who).ID, 0, &no, &yes); err != nil {
			t.Fatal(err)
		}
	}
	w.mustRemove(t, "ann", w.whisper)
	w.mustRemove(t, "ann", w.net)
	if got := f.rec.messages(); len(got) != tapBefore {
		t.Errorf("a scoped removal reached the Broadcaster: %+v", got[tapBefore:])
	}
	w.mustRemove(t, "olga", w.wide)
	w.mustRemove(t, "olga", w.wide) // a repeat sends nothing
	sentinel := f.mustPost(t, "root", `{"body":"sentinel"}`)

	got := map[string][]liveFrame{}
	gotEv := map[string][]liveFrame{}
	v2Raw := map[string][]json.RawMessage{}
	time.Sleep(700 * time.Millisecond)
	for _, who := range people {
		v2Raw[who] = v2[who].take(0)
		got[who] = decodeFrames(t, v2Raw[who])
		gotEv[who] = decodeFrames(t, ev[who].take(0))
	}
	reached := map[int64][]string{
		w.whisper: {"ann", "wes"},
		w.net:     {"ann", "nina", "mona", "dina", "aria"},
		w.wide:    people,
	}
	for id, to := range reached {
		for _, who := range people {
			want := 0
			for _, r := range to {
				if r == who {
					want = 1
				}
			}
			if n := len(tombstones(got[who], id)); n != want {
				t.Errorf("%s's /v2/ws got %d tombstones of %d, want %d", who, n, id, want)
			}
			for _, fr := range got[who] {
				if fr.ID == id && (fr.Message != nil || fr.Body != schema.MessageRemovedBody) {
					t.Errorf("%s's frame for %d = %+v, want the tombstone only", who, id, fr)
				}
			}
			for _, raw := range v2Raw[who] {
				if strings.Contains(string(raw), fmt.Sprintf(`"id":%d,`, id)) && !strings.Contains(string(raw), `"removed":true`) {
					t.Errorf("%s's frame for %d has no removed marker: %s", who, id, raw)
				}
			}
		}
	}
	// v0 and v1 get the channel-wide tombstone once, in their own envelope,
	// with no removed marker, and never a scoped one.
	for name, feed := range map[string]*frameFeed{"v0": v0, "v1": v1} {
		raws := feed.take(0)
		frames := decodeFrames(t, raws)
		wide := 0
		for i, fr := range frames {
			switch fr.ID {
			case w.wide:
				wide++
				if fr.Body != schema.MessageRemovedBody || strings.Contains(string(raws[i]), `"removed"`) {
					t.Errorf("the %s socket got %s", name, raws[i])
				}
			case sentinel:
			default:
				t.Errorf("the %s socket got a frame for %d: %s", name, fr.ID, raws[i])
			}
		}
		if wide != 1 {
			t.Errorf("the %s socket got %d tombstones of the channel-wide message, want 1", name, wide)
		}
	}
	// The events socket: humans who can read it, once each; never an agent.
	evReached := map[int64][]string{
		w.whisper: {"ann", "wes"},
		w.net:     {"ann", "nina", "mona", "dina"},
		w.wide:    {"ann", "nina", "mona", "wes", "olga", "dina", "root"},
	}
	for id, to := range evReached {
		for _, who := range people {
			want := false
			for _, r := range to {
				want = want || r == who
			}
			if n := len(removedEvents(gotEv[who], id)); n != btoi(want) {
				t.Errorf("%s's events socket got %d message_removed frames of %d, want %v", who, n, id, want)
			}
		}
	}
	for _, who := range people {
		for _, fr := range gotEv[who] {
			if fr.Schema == schema.MessageSchemaV2 && fr.ID != sentinel {
				t.Errorf("%s's events socket took a removal for a new message: %+v", who, fr)
			}
		}
	}
	// No original anywhere on any socket.
	for _, who := range people {
		for _, feed := range []*frameFeed{v2[who], ev[who]} {
			for _, raw := range feed.take(0) {
				if strings.Contains(string(raw), "SECRET") {
					t.Errorf("%s's socket carried %s", who, raw)
				}
			}
		}
	}
	// The Broadcaster saw the sentinel and the one channel-wide tombstone.
	var saw []schema.MessageV0
	saw = append(saw, f.rec.messages()[tapBefore:]...)
	if len(saw) != 2 || saw[0].ID != w.wide || saw[0].Body != schema.MessageRemovedBody || saw[0].AuthorID != f.p("ann").ID || saw[1].ID != sentinel {
		t.Errorf("the Broadcaster saw %+v, want the safe tombstone of %d then the sentinel", saw, w.wide)
	}
	for _, m := range saw {
		if b, _ := json.Marshal(m); strings.Contains(string(b), "SECRET") {
			t.Errorf("the Broadcaster saw an original: %s", b)
		}
	}
	// A removal moves no read count, and nobody was disconnected: a post
	// after it still arrives on every socket.
	for who, before := range readBefore {
		var b, a schema.ListReadStateResponseV1
		_ = json.Unmarshal([]byte(before), &b)
		_ = json.Unmarshal([]byte(f.callREST(t, "GET", "/v1/read-state", f.p(who).token, "").body), &a)
		if len(a.ReadState) != 1 || len(b.ReadState) != 1 || a.ReadState[0].Unread != b.ReadState[0].Unread+1 {
			t.Errorf("%s's read state %s -> %+v: only the sentinel is new", who, before, a)
		}
	}
	after := f.mustPost(t, "root", `{"body":"still connected"}`)
	for _, who := range people {
		time.Sleep(10 * time.Millisecond)
		found := false
		deadline := time.Now().Add(3 * time.Second)
		for !found && time.Now().Before(deadline) {
			for _, fr := range decodeFrames(t, v2[who].take(50*time.Millisecond)) {
				found = found || fr.ID == after
			}
		}
		if !found {
			t.Errorf("%s was disconnected by the removal", who)
		}
	}
}

// The v2 frame of a removal is the tombstone MessageV2: removed true, the
// fixed body, the audience and thread of the original, no payload.
func TestMessageRemovalV2FrameShape(t *testing.T) {
	w := newRemovalWorld(t)
	f := w.fileFixture
	feed := feedOf(t, openSocket(t, f.base, "/v2/ws?channel=ops", f.p("nina").token))
	time.Sleep(200 * time.Millisecond)
	feed.take(0)
	w.mustRemove(t, "olga", w.reply)
	time.Sleep(400 * time.Millisecond)
	raws := feed.take(0)
	if len(raws) != 1 {
		t.Fatalf("%d frames", len(raws))
	}
	var m schema.MessageV2
	if err := json.Unmarshal(raws[0], &m); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("the tombstone frame is invalid: %v", err)
	}
	if !m.Removed || m.ID != w.reply || m.Body != schema.MessageRemovedBody || m.ThreadRootID != w.wide || m.Payload != nil || m.AuthorID != f.p("nina").ID {
		t.Errorf("tombstone frame = %+v", m)
	}
}

// ---- the ws replay filter ----

// replay reads frames up to and including the sentinel id.
func tombCount(frames []json.RawMessage, id int64) (originals, tombs int) {
	for _, raw := range frames {
		var m schema.MessageV2
		if json.Unmarshal(raw, &m) != nil || m.ID != id {
			continue
		}
		if m.Removed {
			tombs++
		} else {
			originals++
		}
	}
	return originals, tombs
}

// The replay filter (§7.4): an original the replay sent then removed live
// yields the tombstone once; a tombstone the replay sent followed by the
// buffered original yields nothing more; duplicate tombstones and an edit that
// predates the removal are dropped; older pages and a message from before the
// client's cursor are covered; and nothing reaches a reader who may not see it.
func TestWSReplayRemoval(t *testing.T) {
	type scenario struct {
		name     string
		pageSize int
		// setup posts the seeds and returns (ids, after cursor index+1 style).
		skip int
		// hook runs on the server goroutine at ("before"|"after", page).
		stage string
		page  int
		// act is what the hook does; target indexes the seeds.
		target int
		// wantOriginal says whether the replay is expected to have sent the
		// original of the target (the removal landed after its page was read).
		wantOriginal bool
		// postFirst: the hook posts a fresh message and removes it (the
		// buffer then holds its original and its tombstone).
		postFirst bool
	}
	scenarios := []scenario{
		{name: "original sent by the replay, then removed live", pageSize: 100, stage: "after", page: 0, target: 1, wantOriginal: true},
		{name: "removed before the replay reads the page", pageSize: 100, stage: "before", page: 0, target: 1},
		{name: "removed in a later page before it is read", pageSize: 2, stage: "after", page: 0, target: 3},
		{name: "removed in an earlier page after it was sent", pageSize: 2, stage: "after", page: 1, target: 0, wantOriginal: true},
		{name: "removed older than the client's cursor", pageSize: 2, skip: 3, stage: "before", page: 0, target: 0},
		{name: "posted and removed during the replay", pageSize: 100, stage: "before", page: 0, postFirst: true},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var f *replayFixture
			var seeds []int64
			var target atomic.Int64
			var once sync.Once
			fire := func(stage string, page int) {
				if stage != sc.stage || page != sc.page {
					return
				}
				once.Do(func() {
					id := seeds[sc.target]
					if sc.postFirst {
						id = f.postID(t, "nina", `{"body":"SECRET-fresh"}`)
					}
					target.Store(id)
					if res := f.callREST(t, "DELETE", rmPath("ops", id), f.p("olga").token, ""); res.status != 204 {
						t.Errorf("hook removal of %d = %d %s", id, res.status, res.body)
					}
				})
			}
			f = newReplayFixture(t, sc.pageSize, wsReplayHooks{
				beforePage: func(p int) { fire("before", p) },
				afterPage:  func(p int) { fire("after", p) },
			})
			no, yes := false, true
			if _, err := f.srv.store.PutChannelMember(context.Background(), "system", f.ops.ID, f.p("olga").ID, 0, &no, &yes); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				seeds = append(seeds, f.mustPost(t, "ann", fmt.Sprintf(`{"body":"SECRET-seed %d"}`, i)))
			}
			after := int64(0)
			if sc.skip > 0 {
				after = seeds[sc.skip-1]
			}
			feed := feedOf(t, openSocket(t, f.wsBase, fmt.Sprintf("/v2/ws?channel=ops&after=%d", after), f.p("nina").token))
			deadline := time.Now().Add(5 * time.Second)
			for target.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if target.Load() == 0 {
				t.Fatal("the hook never ran")
			}
			// Duplicates and stale copies injected into the hub: a second
			// tombstone, the original (a buffered post), and an edit that
			// predates the removal. None may reach the client.
			id := target.Load()
			stale := schema.MessageV2{Schema: schema.MessageSchemaV2, ID: id, ChannelID: f.ops.ID, AuthorID: f.p("ann").ID,
				Body: "SECRET-stale", CreatedAt: schema.NewTimestamp(time.Now())}
			edited := stale
			at := schema.NewTimestamp(time.Now().Add(time.Second))
			edited.EditedAt = &at
			tomb := stale
			tomb.Body, tomb.Removed = schema.MessageRemovedBody, true
			// Let the replay finish and everything buffered arrive (a replay
			// reads a sentinel as history if it is still running), then the
			// duplicates.
			frames := feed.take(900 * time.Millisecond)
			f.srv.hub.BroadcastMessageV2(context.Background(), tomb, nil)
			f.srv.hub.BroadcastMessageV2(context.Background(), stale, nil)
			f.srv.hub.BroadcastMessageV2(context.Background(), edited, nil)
			sentinel := f.mustPost(t, "ann", `{"body":"sentinel two"}`)
			frames = append(frames, feed.take(900*time.Millisecond)...)
			if ids := idsOf(t, frames); len(ids) == 0 || ids[len(ids)-1] != sentinel {
				t.Errorf("the socket's last frame is not the sentinel: %v", ids)
			}

			originals, tombs := tombCount(frames, id)
			wantOriginals := 0
			if sc.wantOriginal {
				wantOriginals = 1
			}
			if tombs != 1 {
				for _, raw := range frames {
					t.Logf("frame: %s", raw)
				}
				t.Errorf("the client got %d tombstones of %d, want exactly one", tombs, id)
			}
			if sc.postFirst {
				// The original was only ever in the buffer.
				wantOriginals = 0
			}
			if originals != wantOriginals {
				t.Errorf("the client got %d originals of %d, want %d", originals, id, wantOriginals)
			}
			for _, raw := range frames {
				var m schema.MessageV2
				if json.Unmarshal(raw, &m) == nil && m.ID == id && m.Removed && strings.Contains(string(raw), "SECRET") {
					t.Errorf("a tombstone frame holds an original: %s", raw)
				}
			}
			// An original that was sent must precede its tombstone.
			seenTomb := false
			for _, raw := range frames {
				var m schema.MessageV2
				if json.Unmarshal(raw, &m) != nil || m.ID != id {
					continue
				}
				if m.Removed {
					seenTomb = true
				} else if seenTomb {
					t.Errorf("an original of %d followed its tombstone: %s", id, raw)
				}
			}
		})
	}
}

// A removal reaches a bounded reader only for a message inside its bound:
// the live gate applies to a tombstone as to any frame, on older pages of a
// replay too; a whisper's removal reaches its recipients only.
func TestWSReplayRemovalScopedAndBounded(t *testing.T) {
	var f *replayFixture
	var preRemoved, postRemoved atomic.Int64
	var once sync.Once
	var pre, post, whisper int64
	f = newReplayFixture(t, 2, wsReplayHooks{afterPage: func(page int) {
		if page != 0 {
			return
		}
		once.Do(func() {
			// Remove an older message the replay has not reached yet, one
			// inside the bound, and a whisper to nina.
			for _, id := range []int64{pre, post, whisper} {
				if res := f.callREST(t, "DELETE", rmPath("ops", id), f.p("root").token, ""); res.status != 204 && id != whisper {
					t.Errorf("hook removal of %d = %d %s", id, res.status, res.body)
				}
			}
			preRemoved.Store(pre)
			postRemoved.Store(post)
		})
	}})
	pre = f.mustPost(t, "ann", `{"body":"SECRET-before the guest"}`)
	guest := admitGuest(t, f.scopedFixture)
	post = f.mustPost(t, "ann", `{"body":"SECRET-after the guest"}`)
	whisper = f.mustPost(t, "ann", `{"body":"SECRET-whisper",`+whisperAudience(f.p("nina").ID)+`}`)
	filler := f.mustPost(t, "ann", `{"body":"filler"}`)
	_ = filler
	// root is the owner: a member, so the removals pass for the visible ones;
	// the whisper is not root's to see, which the hook tolerates.
	guestReplay := feedOf(t, openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=0", guest.token))
	deadline := time.Now().Add(5 * time.Second)
	for postRemoved.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// The replay may still be running; wait for it and what it buffered.
	frames := guestReplay.take(900 * time.Millisecond)
	for _, id := range []int64{pre, whisper} {
		for _, raw := range frames {
			var m schema.MessageV2
			if json.Unmarshal(raw, &m) == nil && m.ID == id {
				t.Errorf("the bounded guest got a frame for %d: %s", id, raw)
			}
		}
	}
	if o, tb := tombCount(frames, post); tb != 1 || o > 1 {
		t.Errorf("the guest got %d originals and %d tombstones of the message inside its bound", o, tb)
	}
	for _, raw := range frames {
		var m schema.MessageV2
		if json.Unmarshal(raw, &m) == nil && m.ID != post && strings.Contains(string(raw), "SECRET") {
			t.Errorf("the guest's replay carried an original: %s", raw)
		}
		if m.ID == post && m.Removed && strings.Contains(string(raw), "SECRET") {
			t.Errorf("the guest's tombstone carried an original: %s", raw)
		}
	}
	// And nina, the whisper's recipient, gets its tombstone live, not the
	// guest. (A fresh removal: the hook's belongs to the earlier socket.)
	ninaFeed := feedOf(t, openSocket(t, f.wsBase, "/v2/ws?channel=ops", f.p("nina").token))
	time.Sleep(200 * time.Millisecond)
	ninaFeed.take(0)
	other := f.mustPost(t, "ann", `{"body":"SECRET-second whisper",`+whisperAudience(f.p("nina").ID)+`}`)
	ninaFeed.take(300 * time.Millisecond)
	guestFeed := feedOf(t, openSocket(t, f.wsBase, "/v2/ws?channel=ops", guest.token))
	time.Sleep(200 * time.Millisecond)
	annMgr := f.p("ann")
	no, yes := false, true
	if _, err := f.srv.store.PutChannelMember(context.Background(), "system", f.ops.ID, annMgr.ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	if res := f.callREST(t, "DELETE", rmPath("ops", other), annMgr.token, ""); res.status != 204 {
		t.Fatalf("ann removes her whisper = %d %s", res.status, res.body)
	}
	time.Sleep(400 * time.Millisecond)
	if _, tb := tombCountFeed(t, ninaFeed, other); tb != 1 {
		t.Errorf("nina got %d tombstones of her whisper, want 1", tb)
	}
	if got := guestFeed.take(0); len(got) != 0 {
		t.Errorf("the guest's live socket got %s on a whisper's removal", got)
	}
}

func tombCountFeed(t *testing.T, feed *frameFeed, id int64) (int, int) {
	t.Helper()
	return tombCount(feed.take(0), id)
}

// A manager held to a history bound removes only what is inside it; the same
// 404 as a ghost for what is before it, and for an operator outside the
// channel the channel's own 404.
func TestMessageRemovalManagerHistoryBound(t *testing.T) {
	w := newRemovalWorld(t)
	ctx := context.Background()
	before := w.mustPost(t, "ann", `{"body":"SECRET-before the bound"}`)
	after := w.mustPost(t, "ann", `{"body":"SECRET-after the bound"}`)
	execOnDB(t, w.srv, "UPDATE channel_members SET history_after = ? WHERE channel_id = ? AND principal_id = ?",
		before, w.ops.ID, w.p("olga").ID)
	ghost := w.remove(t, "olga", 999999)
	if got := w.remove(t, "olga", before); got != ghost || got.status != 404 {
		t.Errorf("a bounded manager on a message at the bound = %+v; a ghost gives %+v", got, ghost)
	}
	if got := w.remove(t, "olga", w.wide); got != ghost {
		t.Errorf("a bounded manager on an earlier message = %+v; a ghost gives %+v", got, ghost)
	}
	w.mustRemove(t, "olga", after)
	if got := w.visibleAs(t, "ann", before); got.Removed {
		t.Errorf("the message before the bound was removed: %+v", got)
	}
	if got := w.visibleAs(t, "ann", after); !got.Removed {
		t.Errorf("the message inside the bound was not removed: %+v", got)
	}
	_ = ctx
}

// Guest attribution and bounds survive a removal: the tombstone of a guest's
// message keeps its guest_author; a guest admitted afterwards gets no frame
// or history for what was before it; and a guest converted to a member keeps
// the bound for the tombstone.
func TestMessageRemovalGuests(t *testing.T) {
	w := newRemovalWorld(t)
	f := w.fileFixture
	ctx := context.Background()
	g1 := admitGuest(t, f.scopedFixture)
	f.people["g1"] = &person{Principal: store.Principal{ID: g1.id}, token: g1.token}
	posted := f.mustPost(t, "g1", `{"body":"SECRET-guest words"}`)
	w.mustRemove(t, "olga", posted)
	got := w.visibleAs(t, "ann", posted)
	if !got.Removed || got.GuestAuthor == nil || got.AuthorID != g1.id || got.Body != schema.MessageRemovedBody {
		t.Errorf("the guest's removed message = %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("invalid: %v", err)
	}
	// A guest of a later visit gets the removal of an earlier message as
	// nothing: no frame, no history.
	g2 := admitGuest(t, f.scopedFixture)
	f.people["g2"] = &person{Principal: store.Principal{ID: g2.id}, token: g2.token}
	feed := feedOf(t, openSocket(t, f.base, "/v2/ws?channel=ops", g2.token))
	events := feedOf(t, openSocket(t, f.base, "/v1/events/ws?channel=ops", g2.token))
	time.Sleep(300 * time.Millisecond)
	feed.take(0)
	events.take(0)
	w.mustRemove(t, "olga", w.reply)
	time.Sleep(500 * time.Millisecond)
	for _, raw := range append(feed.take(0), events.take(0)...) {
		if strings.Contains(string(raw), fmt.Sprintf(`"id":%d,`, w.reply)) || strings.Contains(string(raw), "SECRET-reply") {
			t.Errorf("a later guest was sent %s for a removal before its bound", raw)
		}
	}
	res := w.callREST(t, "GET", "/v2/channels/ops/messages?limit=100", g2.token, "")
	if strings.Contains(res.body, fmt.Sprintf(`"id":%d,`, posted)) || strings.Contains(res.body, "SECRET-guest") || strings.Contains(res.body, "SECRET-reply") {
		t.Errorf("the later guest reads history from before its visit: %s", res.body)
	}
	// Conversion: the same principal, now a member with the bound it had.
	execOnDB(t, f.srv, `INSERT INTO server_settings (key, value, updated_at) VALUES ('open_signup', 'true', 0)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`)
	conv, err := f.srv.store.ConvertGuest(ctx, "system", store.GuestConversion{PrincipalID: g2.id, Name: "pat", CredentialLifetime: time.Hour,
		Record: "$argon2id$v=19$m=65536,t=3,p=1$c2FsdHNhbHRzYWx0c2FsdA$conv"})
	if err != nil {
		t.Fatal(err)
	}
	f.people["g2"].token = conv.Token
	w.mustRemove(t, "olga", w.wide)
	after := f.mustPost(t, "ann", `{"body":"SECRET-after conversion"}`)
	w.mustRemove(t, "olga", after)
	res = w.callREST(t, "GET", "/v2/channels/ops/messages?limit=100", conv.Token, "")
	if strings.Contains(res.body, "SECRET") || strings.Contains(res.body, fmt.Sprintf(`"id":%d,`, w.wide)) {
		t.Errorf("the converted member reads history from before its bound: %s", res.body)
	}
	if !strings.Contains(res.body, fmt.Sprintf(`"id":%d,`, after)) || !strings.Contains(res.body, schema.MessageRemovedBody) {
		t.Errorf("the converted member does not read the tombstone inside its bound: %s", res.body)
	}
}

// Reconnecting, a second credential and the MCP read all see the tombstone.
func TestMessageRemovalReconnectAndAlternateCredentials(t *testing.T) {
	w := newRemovalWorld(t)
	f := w.fileFixture
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost)
	w.mustRemove(t, "olga", w.wide)
	// A new credential for ann.
	_, tok2, err := f.srv.store.CreateCredential(context.Background(), "system", f.p("ann").ID, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{f.p("ann").token, tok2} {
		for _, path := range []string{"/v0/ws?channel=ops", "/v1/ws?channel=ops", "/v2/ws?channel=ops&after=0", "/v1/events/ws?channel=ops"} {
			sock := openSocket(t, f.base, path, token)
			if sock.status != http.StatusSwitchingProtocols {
				t.Fatalf("%s = %d %s", path, sock.status, sock.body)
			}
			feed := feedOf(t, sock)
			time.Sleep(400 * time.Millisecond)
			for _, raw := range feed.take(0) {
				if strings.Contains(string(raw), "SECRET-wide") {
					t.Errorf("%s carried the original after the removal: %s", path, raw)
				}
			}
		}
		res := w.callREST(t, "GET", "/v2/channels/ops/messages?limit=100", token, "")
		if strings.Contains(res.body, "SECRET-wide") {
			t.Errorf("history on a second credential: %s", res.body)
		}
	}
	out := f.mcpCall(t, f.p("aria").token, "read_channel", map[string]any{"channel": "ops"})
	if out.isError || !strings.Contains(out.body, schema.MessageRemovedBody) || strings.Contains(out.body, "SECRET-wide") {
		t.Errorf("read_channel after the removal: %s", out.body)
	}
	var resp struct {
		Result struct {
			Structured struct {
				Messages []struct {
					ID      int64            `json:"id"`
					Body    string           `json:"body"`
					Payload *json.RawMessage `json:"payload"`
					Removed bool             `json:"removed"`
				} `json:"messages"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.body), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range resp.Result.Structured.Messages {
		if m.ID == w.wide {
			found = true
			if !m.Removed || m.Body != schema.MessageRemovedBody || m.Payload != nil {
				t.Errorf("the MCP message = %+v", m)
			}
		}
		if m.ID == w.typed {
			if m.Removed || m.Payload == nil {
				t.Errorf("an unremoved typed message over MCP = %+v", m)
			}
		}
	}
	if !found {
		t.Errorf("read_channel did not return the tombstone: %s", out.body)
	}
}
