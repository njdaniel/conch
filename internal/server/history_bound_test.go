package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The guest history bound on every read route (issue #719, ADR-009, design
// §4.3): markers are posted before a guest is admitted and after, and every
// route and socket is asked for them as the guest. What the guest is told
// about an object from before its admission is compared, byte for byte, with
// what it is told about one that never existed.

// ghostID is an id no object of the tests has.
const ghostID = int64(1) << 40

// boundWorld is the file fixture (ops, with nets, people and uploads) with
// markers from before a guest's admission and from after it.
type boundWorld struct {
	*fileFixture
	guest store.CreatedGuest
	// Before the admission.
	preWide, preRoot, preReply, preWhisper, preNet, preFileMsg, lateBroadcast int64
	preFile                                                                   schema.FileV1
	// After it.
	postWide, postNet, postWhisper, postRoot, postReply, oldThreadReply, postFileMsg int64
	postFile                                                                         schema.FileV1
}

func newBoundWorld(t *testing.T) *boundWorld {
	t.Helper()
	ctx := context.Background()
	w := &boundWorld{fileFixture: newFileFixture(t)}
	ann, olga := w.p("ann"), w.p("olga")

	w.preWide = w.mustPost(t, "ann", `{"body":"marker before"}`)
	w.preRoot = w.mustPost(t, "olga", `{"body":"old root"}`)
	w.preReply = w.mustPost(t, "ann", fmt.Sprintf(`{"body":"old reply","thread_root_id":%d}`, w.preRoot))
	w.preWhisper = w.mustPost(t, "ann", `{"body":"private marker before",`+whisperAudience(olga.ID)+`}`)
	w.preNet = w.mustPost(t, "ann", `{"body":"net marker before",`+netAudience(w.net1.ID)+`}`)
	w.preFile = w.mustUpload(t, "ann", "old.txt", []byte("old"))
	w.preFileMsg = w.mustPost(t, "ann", attachBody("old file", "", w.preFile.ID))
	// Stored before the admission and broadcast after it: the order two
	// handlers' commits and broadcasts can take.
	late, err := w.srv.store.InsertMessageV1(ctx, w.ops.ID, ann.ID, "late marker", nil)
	if err != nil {
		t.Fatal(err)
	}
	w.lateBroadcast = late.ID

	if w.guest, err = w.srv.store.CreateGuest(ctx, "system", store.NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{w.ops.ID}, Idle: time.Hour, Absolute: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	w.people["guest"] = &person{Principal: w.guest.Principal, token: w.guest.Token}
	w.seat(t, "net1", "guest", schema.NetRoleMember)

	w.postWide = w.mustPost(t, "ann", `{"body":"marker after"}`)
	w.postNet = w.mustPost(t, "ann", `{"body":"net marker after",`+netAudience(w.net1.ID)+`}`)
	w.postWhisper = w.mustPost(t, "ann", `{"body":"private marker after",`+whisperAudience(w.guest.Principal.ID)+`}`)
	w.postRoot = w.mustPost(t, "olga", `{"body":"new root"}`)
	w.postReply = w.mustPost(t, "ann", fmt.Sprintf(`{"body":"new reply","thread_root_id":%d}`, w.postRoot))
	w.oldThreadReply = w.mustPost(t, "ann", fmt.Sprintf(`{"body":"marker in the old thread","thread_root_id":%d}`, w.preRoot))
	w.postFile = w.mustUpload(t, "ann", "new.txt", []byte("new"))
	w.postFileMsg = w.mustPost(t, "ann", attachBody("new file", "", w.postFile.ID))
	return w
}

// v2After is what the guest may read over v2; v1After over v0 and v1.
func (w *boundWorld) v2After() []int64 {
	return []int64{w.postWide, w.postNet, w.postWhisper, w.postRoot, w.postReply, w.postFileMsg}
}
func (w *boundWorld) v1After() []int64 {
	return []int64{w.postWide, w.postRoot, w.postReply, w.postFileMsg}
}

// get is a GET as who, through the real listener.
func (w *boundWorld) get(t *testing.T, who, path string) fileResp {
	t.Helper()
	return w.do(t, "GET", path, who, "", nil)
}

// sameAsGhost fails unless the two answers are the same status and body.
func sameAsGhost(t *testing.T, what string, pre, ghost fileResp) {
	t.Helper()
	if pre.status != ghost.status || string(pre.body) != string(ghost.body) {
		t.Errorf("%s: before the bound %s; a ghost %s", what, pre, ghost)
	}
	if pre.status < 400 {
		t.Errorf("%s: an object from before the bound was served: %s", what, pre)
	}
}

// Every message route gives the guest what is after its bound and its
// audience, and nothing from before, in every version and order.
func TestHistoryBoundMessageRoutes(t *testing.T) {
	w := newBoundWorld(t)
	for _, tt := range []struct {
		path string
		want []int64
	}{
		{"/v2/channels/ops/messages", w.v2After()},
		{"/v2/channels/ops/messages?after=0", w.v2After()},
		{fmt.Sprintf("/v2/channels/ops/messages?after=%d", w.preWide), w.v2After()},
		{"/v1/channels/ops/messages", w.v1After()},
		{"/v0/channels/ops/messages", w.v1After()},
		{"/v2/channels/ops/messages?order=desc", reversed(w.v2After())},
		{"/v1/channels/ops/messages?order=desc", reversed(w.v1After())},
		{fmt.Sprintf("/v2/channels/ops/messages?thread=%d", w.postRoot), []int64{w.postRoot, w.postReply}},
		{"/v2/channels/ops/messages/search?q=marker", []int64{w.postWhisper, w.postNet, w.postWide}},
	} {
		t.Run(tt.path, func(t *testing.T) {
			res := w.get(t, "guest", tt.path)
			if res.status != http.StatusOK {
				t.Fatalf("%s", res)
			}
			if got, _ := listIDs(t, string(res.body)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ids = %v, want %v", got, tt.want)
			}
		})
	}

	// A member's view is what it was: everything from before is there.
	res := w.get(t, "ann", "/v2/channels/ops/messages")
	got, _ := listIDs(t, string(res.body))
	for _, id := range []int64{w.preWide, w.preRoot, w.preReply, w.preWhisper, w.preNet, w.preFileMsg, w.lateBroadcast, w.oldThreadReply} {
		if !slices.Contains(got, id) {
			t.Errorf("ann lost %d: %v", id, got)
		}
	}

	// An old thread, read or replied to, is a thread that does not exist.
	sameAsGhost(t, "thread",
		w.get(t, "guest", fmt.Sprintf("/v2/channels/ops/messages?thread=%d", w.preRoot)),
		w.get(t, "guest", fmt.Sprintf("/v2/channels/ops/messages?thread=%d", ghostID)))
	sameAsGhost(t, "thread of a reply", // a reply is never a root, but the answer is the same
		w.get(t, "guest", fmt.Sprintf("/v2/channels/ops/messages?thread=%d", w.preReply)),
		w.get(t, "guest", fmt.Sprintf("/v2/channels/ops/messages?thread=%d", ghostID)))
	before := w.messageCount(t)
	reply := func(root int64) fileResp {
		return w.do(t, "POST", "/v2/channels/ops/messages", "guest", "application/json",
			strings.NewReader(fmt.Sprintf(`{"body":"hello","thread_root_id":%d}`, root)))
	}
	sameAsGhost(t, "reply", reply(w.preRoot), reply(ghostID))
	if after := w.messageCount(t); after != before {
		t.Errorf("refused replies stored %d messages", after-before)
	}
}

// A file from before the bound, its metadata and its bytes, is answered as
// a file that does not exist; one shared after it is served.
func TestHistoryBoundFileRoutes(t *testing.T) {
	w := newBoundWorld(t)
	for _, suffix := range []string{"/meta", ""} {
		sameAsGhost(t, "file"+suffix,
			w.get(t, "guest", fmt.Sprintf("/v2/files/%d%s", w.preFile.ID, suffix)),
			w.get(t, "guest", fmt.Sprintf("/v2/files/%d%s", ghostID, suffix)))
		if res := w.get(t, "guest", fmt.Sprintf("/v2/files/%d%s", w.postFile.ID, suffix)); res.status != http.StatusOK {
			t.Errorf("new file%s = %s", suffix, res)
		}
		if res := w.get(t, "ann", fmt.Sprintf("/v2/files/%d%s", w.preFile.ID, suffix)); res.status != http.StatusOK {
			t.Errorf("ann's old file%s = %s", suffix, res)
		}
	}
}

// The unread count counts only what is after the bound, and a mark is
// clamped to the newest message the guest may see.
func TestHistoryBoundReadState(t *testing.T) {
	w := newBoundWorld(t)
	unread := func() schema.ReadStateV1 {
		t.Helper()
		res := w.get(t, "guest", "/v1/read-state")
		var list schema.ListReadStateResponseV1
		if err := json.Unmarshal(res.body, &list); err != nil || res.status != http.StatusOK {
			t.Fatalf("read state = %s (%v)", res, err)
		}
		for _, rs := range list.ReadState {
			if rs.ChannelID == w.ops.ID {
				return rs
			}
		}
		t.Fatalf("no read state for ops: %s", res.body)
		return schema.ReadStateV1{}
	}
	if rs := unread(); rs.Unread != len(w.v2After()) {
		t.Errorf("unread = %d, want %d", rs.Unread, len(w.v2After()))
	}
	res := w.do(t, "PUT", "/v1/channels/ops/read-state", "guest", "application/json",
		strings.NewReader(fmt.Sprintf(`{"last_read_id":%d}`, ghostID)))
	var mark schema.ReadStateV1
	if err := json.Unmarshal(res.body, &mark); err != nil || res.status != http.StatusOK {
		t.Fatalf("mark = %s (%v)", res, err)
	}
	if mark.LastReadID != w.postFileMsg || mark.Unread != 0 {
		t.Errorf("mark = %+v, want clamped to %d with nothing unread", mark, w.postFileMsg)
	}
}

// WS v2 replay sends what the history route lists; a live frame from before
// the bound, or in an old thread, reaches a member's sockets and none of the
// guest's, in any version.
func TestHistoryBoundSockets(t *testing.T) {
	ctx := context.Background()
	w := newBoundWorld(t)
	g := w.p("guest")

	replay := openSocket(t, w.base, "/v2/ws?channel=ops&after=0", g.token)
	if replay.status != http.StatusSwitchingProtocols {
		t.Fatalf("replay socket = %d %s", replay.status, replay.body)
	}
	if got := idsOf(t, replay.readUntil(t, w.postFileMsg)); !reflect.DeepEqual(got, w.v2After()) {
		t.Errorf("replay = %v, want %v", got, w.v2After())
	}

	type sock struct {
		name string
		s    *socket
	}
	var guestSocks, annSocks []sock
	for _, v := range []string{"v0", "v1", "v2"} {
		path := "/" + v + "/ws?channel=ops"
		guestSocks = append(guestSocks, sock{"guest " + v, openSocket(t, w.base, path, g.token)})
		annSocks = append(annSocks, sock{"ann " + v, openSocket(t, w.base, path, w.p("ann").token)})
	}
	guestSocks = append(guestSocks, sock{"guest v2 replay", replay})
	// A subscription is registered before the upgrade completes, but a read
	// of the history bound follows it; a post now proves every socket is
	// streaming.
	warm := w.mustPost(t, "ann", `{"body":"warm up"}`)
	for _, s := range append(append([]sock{}, guestSocks...), annSocks...) {
		s.s.readUntil(t, warm)
	}

	// The late broadcast of a message stored before the admission.
	stored, err := w.srv.store.ListVisibleMessages(ctx, w.ops.ID, store.Reader{PrincipalID: w.p("ann").ID, Scoped: true}, w.lateBroadcast-1, 1)
	if err != nil || len(stored) != 1 || stored[0].ID != w.lateBroadcast {
		t.Fatalf("late message = %v, %v", stored, err)
	}
	w.srv.hub.BroadcastMessageV1(ctx, messageV1FromStore(stored[0]))
	w.srv.hub.BroadcastMessage(ctx, messageV0FromStore(stored[0]))
	// A reply in the old thread, a new whisper to the guest, and the sentinel.
	oldThread := w.mustPost(t, "ann", fmt.Sprintf(`{"body":"another in the old thread","thread_root_id":%d}`, w.preRoot))
	whisper := w.mustPost(t, "ann", `{"body":"psst",`+whisperAudience(g.ID)+`}`)
	sentinel := w.mustPost(t, "ann", `{"body":"sentinel"}`)

	for _, s := range guestSocks {
		got := idsOf(t, s.s.readUntil(t, sentinel))
		for _, id := range []int64{w.lateBroadcast, oldThread} {
			if slices.Contains(got, id) {
				t.Errorf("%s was sent %d from before its bound: %v", s.name, id, got)
			}
		}
		v2 := s.name == "guest v2" || s.name == "guest v2 replay"
		if slices.Contains(got, whisper) != v2 {
			t.Errorf("%s: whisper %d in %v, want %v", s.name, whisper, got, v2)
		}
	}
	for _, s := range annSocks {
		got := idsOf(t, s.s.readUntil(t, sentinel))
		for _, id := range []int64{w.lateBroadcast, oldThread} {
			if !slices.Contains(got, id) {
				t.Errorf("%s missed %d: %v", s.name, id, got)
			}
		}
	}
}

// Runs, their activity and interventions, over REST and the events socket:
// those from before the bound, and those that answer no message, are
// answered as ones that do not exist.
func TestHistoryBoundRunRoutes(t *testing.T) {
	ctx := context.Background()
	f := &interventionFixture{runFixture: newRunFixture(t)}
	base := wsTestServer(t, f.srv)
	sess := f.session(t, f.botTok)

	old, err := f.srv.store.InsertMessageV1(ctx, f.general.ID, f.alice.ID, "an old question", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldRun := f.start(t, f.botTok, sess, "general", old.ID, "answering the old question")
	if rec := f.do(t, "POST", runPath(oldRun.ID, "/activity"), f.botTok, activityBody); rec.Code != http.StatusCreated {
		t.Fatalf("activity = %d %s", rec.Code, rec.Body)
	}
	oldIV := f.mustAsk(t, f.aliceTok, oldRun.ID, schema.InterventionKindCancel)
	whisperRun := f.start(t, f.botTok, sess, "general", f.whisper, "answering alice")

	g, err := f.srv.store.CreateGuest(ctx, "system", store.NewGuest{Origin: schema.GuestOriginAddress,
		Channels: []int64{f.general.ID}, Idle: time.Hour, Absolute: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	guestEvents := dialEvents(t, base, "/v1/events/ws?channel=general", g.Token)
	aliceEvents := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	guestEvents.next()
	aliceEvents.next()

	fresh, err := f.srv.store.InsertMessageV1(ctx, f.general.ID, f.alice.ID, "a new question", nil)
	if err != nil {
		t.Fatal(err)
	}
	nothingRun := f.start(t, f.botTok, sess, "general", 0, "working on nothing in particular")
	newRun := f.start(t, f.botTok, sess, "general", fresh.ID, "answering the new question")
	newIV := f.mustAsk(t, f.aliceTok, newRun.ID, schema.InterventionKindCancel)

	same := func(what, pre, ghost string) {
		t.Helper()
		a, b := f.do(t, "GET", pre, g.Token, ""), f.do(t, "GET", ghost, g.Token, "")
		if a.Code != b.Code || a.Body.String() != b.Body.String() || a.Code < 400 {
			t.Errorf("%s: before the bound %d %s; a ghost %d %s", what, a.Code, a.Body, b.Code, b.Body)
		}
	}
	for _, id := range []int64{oldRun.ID, whisperRun.ID, nothingRun.ID} {
		for _, suffix := range []string{"", "/activity", "/interventions"} {
			same(fmt.Sprintf("run %d%s", id, suffix), runPath(id, suffix), runPath(ghostID, suffix))
		}
	}
	same("intervention", fmt.Sprintf("/v1/interventions/%d", oldIV.ID), fmt.Sprintf("/v1/interventions/%d", ghostID))
	for _, path := range []string{runPath(newRun.ID, ""), runPath(newRun.ID, "/activity"), runPath(newRun.ID, "/interventions"),
		fmt.Sprintf("/v1/interventions/%d", newIV.ID)} {
		if rec := f.do(t, "GET", path, g.Token, ""); rec.Code != http.StatusOK {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
	rec := f.do(t, "GET", "/v1/runs?channel=general", g.Token, "")
	var list schema.ListRunsResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("list runs = %d %s", rec.Code, rec.Body)
	}
	var ids []int64
	for _, r := range list.Runs {
		ids = append(ids, r.ID)
	}
	if !reflect.DeepEqual(ids, []int64{newRun.ID}) {
		t.Errorf("guest's runs = %v, want [%d]", ids, newRun.ID)
	}
	// A member is not affected.
	for _, id := range []int64{oldRun.ID, nothingRun.ID} {
		if rec := f.do(t, "GET", runPath(id, ""), f.aliceTok, ""); rec.Code != http.StatusOK {
			t.Errorf("alice's run %d = %d", id, rec.Code)
		}
	}

	// The events socket: activity on the old run, and the run on nothing,
	// reach alice and not the guest; the new run's activity reaches both.
	if rec := f.do(t, "POST", runPath(oldRun.ID, "/activity"), f.botTok, activityBody); rec.Code != http.StatusCreated {
		t.Fatalf("activity = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "POST", runPath(newRun.ID, "/activity"), f.botTok, activityBody); rec.Code != http.StatusCreated {
		t.Fatalf("activity = %d %s", rec.Code, rec.Body)
	}
	newActivity := func(fr eventFrame) bool {
		return fr.schema == schema.AgentActivitySchemaV1 && fr.activity.RunID == newRun.ID
	}
	seen := guestEvents.until("the new run's activity", newActivity)
	for _, fr := range seen {
		switch fr.schema {
		case schema.RunSchemaV1:
			if fr.run.ID != newRun.ID {
				t.Errorf("the guest was sent run %d", fr.run.ID)
			}
		case schema.AgentActivitySchemaV1:
			if fr.activity.RunID != newRun.ID {
				t.Errorf("the guest was sent activity of run %d", fr.activity.RunID)
			}
		case schema.InterventionSchemaV1:
			if fr.intervention.RunID != newRun.ID {
				t.Errorf("the guest was sent intervention %d", fr.intervention.ID)
			}
		case schema.SessionRosterSchemaV1:
			if s, ok := rosterSession(fr.roster, f.bot.ID); ok && s.CurrentRun != 0 && s.CurrentRun != newRun.ID {
				t.Errorf("the guest's roster names run %d (%q)", s.CurrentRun, s.Task)
			}
		}
	}
	// The run on nothing was started before the old run's new activity, so
	// alice has been sent it by the time that activity arrives.
	got := aliceEvents.until("the old run's activity", func(fr eventFrame) bool {
		return fr.schema == schema.AgentActivitySchemaV1 && fr.activity.RunID == oldRun.ID
	})
	if !hasFrame(got, isRun(func(r schema.RunV1) bool { return r.ID == nothingRun.ID })) {
		t.Errorf("alice was not sent the run on nothing: %v", got)
	}
}

func reversed(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Reverse(out)
	return out
}
