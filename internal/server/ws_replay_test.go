package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

// replayFixture is a scopedFixture whose server is reached through a listener
// started after the test seams were set, so the handler goroutines never race
// the test's writes to them.
type replayFixture struct {
	*scopedFixture
	wsBase string
}

func newReplayFixture(t *testing.T, pageSize int, hooks wsReplayHooks) *replayFixture {
	t.Helper()
	f := newScopedFixture(t)
	f.srv.replayPageSize = pageSize
	f.srv.replayHooks = hooks
	return &replayFixture{scopedFixture: f, wsBase: wsTestServer(t, f.srv)}
}

// postID posts as who and returns the stored id. It never calls FailNow, so a
// replay hook (which runs on the server's goroutine) can use it.
func (f *scopedFixture) postID(t *testing.T, who, body string) int64 {
	t.Helper()
	res := f.postV2(t, who, body)
	if res.status != http.StatusCreated {
		t.Errorf("%s post %s = %d %s", who, body, res.status, res.body)
		return 0
	}
	var resp schema.PostMessageResponseV2
	if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
		t.Errorf("decode post response %s: %v", res.body, err)
		return 0
	}
	return resp.Message.ID
}

// closeOf reads until the connection ends and returns how it was closed.
func closeOf(t *testing.T, conn *websocket.Conn) websocket.CloseError {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("read ended with %v, want a close frame", err)
		}
		return ce
	}
}

// A credential revoked on its own (not a disabled principal, not revoke-all)
// does not drop the subscription: only the periodic re-check ends the stream.
// During a replay that check runs before every frame, so a revocation that
// lands after a page was read stops the page from being sent.
func TestWSReplayStopsAtTheNextFrameWhenTheCredentialIsRevoked(t *testing.T) {
	const recheck = 20 * time.Millisecond
	// Built by hand, not with newReplayFixture, so that the re-check interval
	// is set, like the hooks, before the listener starts.
	f := &replayFixture{scopedFixture: newScopedFixture(t)}
	f.srv.credRecheckInterval = recheck
	f.srv.replayHooks = wsReplayHooks{afterPage: func(page int) {
		if page != 0 {
			return
		}
		creds, err := f.srv.store.ListCredentials(context.Background(), f.p("olga").ID)
		if err != nil || len(creds) != 1 {
			t.Errorf("olga's credentials = %v, %v", creds, err)
			return
		}
		if err := f.srv.store.RevokeCredential(context.Background(), "system", creds[0].ID); err != nil {
			t.Errorf("revoke: %v", err)
		}
		// Let the re-check interval pass, so the tick is waiting when the
		// replay looks before its first frame.
		time.Sleep(4 * recheck)
	}}
	f.wsBase = wsTestServer(t, f.srv)
	for i := 0; i < 5; i++ {
		f.mustPost(t, "ann", `{"body":"history"}`)
	}
	sock := openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=0", f.p("olga").token)
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d %s", sock.status, sock.body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := sock.conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("first read = %q, %v; want a close and no frame of the page read before the revocation", data, err)
	}
	if ce.Code != websocket.StatusPolicyViolation || ce.Reason != reasonCredentialInvalid {
		t.Errorf("close = %d %q, want %d %q", ce.Code, ce.Reason, websocket.StatusPolicyViolation, reasonCredentialInvalid)
	}
}

// TestWSReplayDeliversEachMessageOnce is the ordering contract: whatever
// moment a message is committed relative to the replay (before it reads a
// page, after it read one, during the last page, after the replay is over), the
// client receives every message once, in id order. The message is posted from a
// test hook at an exact point of the replay, not after a sleep.
func TestWSReplayDeliversEachMessageOnce(t *testing.T) {
	tests := []struct {
		name     string
		pageSize int
		skip     int // seeds the client already has; after is the id of the last of them
		hook     string
		hookPage int
	}{
		{"nothing posted during the replay", 100, 1, "", 0},
		{"after=0 replays the whole channel", 100, 0, "", 0},
		{"cursor at the newest message replays nothing", 100, 5, "", 0},
		{"posted between subscribe and replay", 100, 1, "before", 0},
		{"posted after the only page was read", 100, 1, "after", 0},
		{"posted before a middle page is read", 2, 1, "before", 1},
		{"posted after the first of three pages was read", 2, 0, "after", 0},
		{"posted after a middle page was read", 2, 0, "after", 1},
		{"posted after the last page was read", 2, 0, "after", 2},
		{"posted before the empty final page", 2, 1, "before", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f *replayFixture
			var hookID atomic.Int64
			fired := make(chan struct{})
			var once sync.Once
			fire := func(stage string, page int) {
				if stage != tt.hook || page != tt.hookPage {
					return
				}
				once.Do(func() {
					hookID.Store(f.postID(t, "ann", `{"body":"during replay"}`))
					close(fired)
				})
			}
			f = newReplayFixture(t, tt.pageSize, wsReplayHooks{
				beforePage: func(p int) { fire("before", p) },
				afterPage:  func(p int) { fire("after", p) },
			})
			var seeds []int64
			for i := 0; i < 5; i++ {
				seeds = append(seeds, f.mustPost(t, "ann", fmt.Sprintf(`{"body":"seed %d"}`, i)))
			}
			after := int64(0)
			if tt.skip > 0 {
				after = seeds[tt.skip-1]
			}

			sock := openSocket(t, f.wsBase, fmt.Sprintf("/v2/ws?channel=ops&after=%d", after), f.p("olga").token)
			if sock.status != http.StatusSwitchingProtocols {
				t.Fatalf("upgrade = %d %s", sock.status, sock.body)
			}
			if tt.hook != "" {
				select {
				case <-fired:
				case <-time.After(5 * time.Second):
					t.Fatal("the hook never ran")
				}
			}
			// The sentinel is posted last, so a duplicate anywhere before it shows
			// in the list, and one after it cannot hide.
			sentinel := f.mustPost(t, "ann", `{"body":"sentinel"}`)

			want := append([]int64(nil), seeds[tt.skip:]...)
			if id := hookID.Load(); id != 0 {
				want = append(want, id)
			}
			want = append(want, sentinel)
			if got := idsOf(t, sock.readUntil(t, sentinel)); !reflect.DeepEqual(got, want) {
				t.Errorf("received ids %v, want %v", got, want)
			}
			if tt.hook != "" && hookID.Load() == 0 {
				t.Error("the hook did not post")
			}
		})
	}
}

// A cursor past every stored id is not an excuse to drop what is posted next:
// only an id already sent is dropped.
func TestWSReplayAfterBeyondNewestStillStreams(t *testing.T) {
	f := newReplayFixture(t, 0, wsReplayHooks{})
	f.mustPost(t, "ann", `{"body":"old"}`)
	sock := openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=1000000", f.p("olga").token)
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d %s", sock.status, sock.body)
	}
	fresh := f.mustPost(t, "ann", `{"body":"fresh"}`)
	if got, want := idsOf(t, sock.readUntil(t, fresh)), []int64{fresh}; !reflect.DeepEqual(got, want) {
		t.Errorf("received ids %v, want %v", got, want)
	}
}

// Two posts can reach the hub out of id order, because each handler broadcasts
// after its own commit. A replay must not turn that into a lost message: only
// the ids the replay itself sent are dropped, so a live frame with a lower id
// than the one before it is still delivered.
func TestWSReplayKeepsALiveMessageThatArrivesOutOfOrder(t *testing.T) {
	f := newReplayFixture(t, 0, wsReplayHooks{})
	old := f.mustPost(t, "ann", `{"body":"old"}`)
	sock := openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=0", f.p("olga").token)
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d %s", sock.status, sock.body)
	}
	replayed := sock.readUntil(t, old)
	var seed schema.MessageV2
	if err := json.Unmarshal(replayed[len(replayed)-1], &seed); err != nil {
		t.Fatal(err)
	}
	// The higher id reaches the hub first, as when its handler won the race
	// to broadcast.
	first, second := seed, seed
	first.ID, second.ID = old+2, old+1
	f.srv.hub.BroadcastMessageV2(context.Background(), first, nil)
	f.srv.hub.BroadcastMessageV2(context.Background(), second, nil)
	if got, want := idsOf(t, sock.readUntil(t, old+1)), []int64{old + 2, old + 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("live ids after the replay = %v, want %v: the lower id was dropped", got, want)
	}
	// And one the replay did send is still dropped when it comes again live.
	f.srv.hub.BroadcastMessageV2(context.Background(), seed, nil)
	sentinel := f.mustPost(t, "ann", `{"body":"sentinel"}`)
	if got, want := idsOf(t, sock.readUntil(t, sentinel)), []int64{sentinel}; !reflect.DeepEqual(got, want) {
		t.Errorf("after a repeat of a replayed id = %v, want %v", got, want)
	}
}

// A replay shows a reader what the history route shows them and nothing else:
// a net message or whisper outside their audience is not sent, including one
// posted while the replay runs.
func TestWSReplayScoped(t *testing.T) {
	var f *replayFixture
	var live atomic.Bool // the hook posts only for the first connection
	var duringID atomic.Int64
	fired := make(chan struct{})
	var once sync.Once
	f = newReplayFixture(t, 2, wsReplayHooks{afterPage: func(page int) {
		if page != 0 || !live.Load() {
			return
		}
		once.Do(func() {
			duringID.Store(f.postID(t, "ann", `{"body":"whisper while replaying",`+whisperAudience(f.p("nina").ID)+`}`))
			close(fired)
		})
	}})
	f.mustPost(t, "ann", `{"body":"wide 1"}`)
	f.mustPost(t, "ann", `{"body":"net1",`+netAudience(f.net1.ID)+`}`)
	f.mustPost(t, "ann", `{"body":"whisper to wes",`+whisperAudience(f.p("wes").ID)+`}`)
	f.mustPost(t, "wes", `{"body":"net2",`+netAudience(f.net2.ID)+`}`)
	f.mustPost(t, "ann", `{"body":"whisper to nina",`+whisperAudience(f.p("nina").ID)+`}`)
	f.mustPost(t, "ann", `{"body":"wide 2"}`)

	// nina first, with the hook armed: a scoped message committed mid-replay.
	live.Store(true)
	ninaSock := openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=0", f.p("nina").token)
	if ninaSock.status != http.StatusSwitchingProtocols {
		t.Fatalf("nina upgrade = %d %s", ninaSock.status, ninaSock.body)
	}
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook never ran")
	}
	live.Store(false)
	sentinel := f.mustPost(t, "ann", `{"body":"sentinel"}`)

	for _, who := range []string{"nina", "wes", "olga", "mona", "root"} {
		t.Run(who, func(t *testing.T) {
			sock := ninaSock
			if who != "nina" {
				sock = openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=0", f.p(who).token)
				if sock.status != http.StatusSwitchingProtocols {
					t.Fatalf("upgrade = %d %s", sock.status, sock.body)
				}
			}
			got := idsOf(t, sock.readUntil(t, sentinel))
			// What the history route shows this reader is the oracle.
			want, _ := listIDs(t, f.callREST(t, "GET", "/v2/channels/ops/messages?limit=100", f.p(who).token, "").body)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s replay received %v, the history route shows %v", who, got, want)
			}
			sawDuring := false
			for _, id := range got {
				sawDuring = sawDuring || id == duringID.Load()
			}
			if wantDuring := who == "nina"; sawDuring != wantDuring {
				t.Errorf("%s saw the whisper posted during the replay = %v, want %v", who, sawDuring, wantDuring)
			}
		})
	}
}

// Removal from the channel while the replay runs ends the stream with the
// membership reason, as it does for a live stream, and nothing further is sent.
func TestWSReplayEndsWhenRemovedFromChannel(t *testing.T) {
	var f *replayFixture
	var removed atomic.Int64
	f = newReplayFixture(t, 2, wsReplayHooks{afterPage: func(page int) {
		if page != 0 {
			return
		}
		if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.p("mona").ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
			t.Errorf("remove = %d %s", rec.Code, rec.Body)
		}
		removed.Add(1)
	}})
	for i := 0; i < 6; i++ {
		f.mustPost(t, "ann", fmt.Sprintf(`{"body":"seed %d"}`, i))
	}
	sock := openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=0", f.p("mona").token)
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d %s", sock.status, sock.body)
	}
	ce := closeOf(t, sock.conn)
	if ce.Code != websocket.StatusPolicyViolation || ce.Reason != "no longer a member of this channel" {
		t.Errorf("closed with %d %q, want a policy violation for membership", ce.Code, ce.Reason)
	}
	if removed.Load() != 1 {
		t.Errorf("hook removed %d times, want 1", removed.Load())
	}
}

// A replay that lets more than the subscription's buffer arrive is a slow
// consumer: it is closed with that reason, sent nothing out of order or twice,
// and a client that comes back with its newest id gets everything.
func TestWSReplayOverflowClosesAsSlowConsumer(t *testing.T) {
	var f *replayFixture
	var once sync.Once
	var flooded []int64
	f = newReplayFixture(t, 2, wsReplayHooks{afterPage: func(page int) {
		if page != 0 {
			return
		}
		once.Do(func() {
			for i := 0; i < wsSendBuffer+6; i++ {
				flooded = append(flooded, f.postID(t, "ann", fmt.Sprintf(`{"body":"flood %d"}`, i)))
			}
		})
	}})
	var seeds []int64
	for i := 0; i < 5; i++ {
		seeds = append(seeds, f.mustPost(t, "ann", fmt.Sprintf(`{"body":"seed %d"}`, i)))
	}

	sock := openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=0", f.p("olga").token)
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d %s", sock.status, sock.body)
	}
	var got []int64
	var ce websocket.CloseError
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var m schema.MessageV2
		err := wsjson.Read(ctx, sock.conn, &m)
		cancel()
		if err != nil {
			if !errors.As(err, &ce) {
				t.Fatalf("read ended with %v, want a close frame", err)
			}
			break
		}
		got = append(got, m.ID)
	}
	if ce.Code != websocket.StatusPolicyViolation || ce.Reason != "subscriber too slow" {
		t.Errorf("closed with %d %q, want a policy violation for a slow subscriber", ce.Code, ce.Reason)
	}
	for i, id := range got {
		if i > 0 && id <= got[i-1] {
			t.Errorf("ids not ascending: %v", got)
		}
	}

	// The client retries with the newest id it has and is given the rest.
	after := int64(0)
	if len(got) > 0 {
		after = got[len(got)-1]
	}
	retry := openSocket(t, f.wsBase, fmt.Sprintf("/v2/ws?channel=ops&after=%d", after), f.p("olga").token)
	if retry.status != http.StatusSwitchingProtocols {
		t.Fatalf("retry upgrade = %d %s", retry.status, retry.body)
	}
	// The flood is in the store and larger than the buffer, but the hook has
	// fired: this replay is not flooded, and (the buffer being empty) it
	// finishes.
	sentinel := f.mustPost(t, "ann", `{"body":"sentinel"}`)
	want := append(append(append([]int64(nil), seeds...), flooded...), sentinel)
	want = want[len(got):]
	if rest := idsOf(t, retry.readUntil(t, sentinel)); !reflect.DeepEqual(rest, want) {
		t.Errorf("retry after %d received %d ids, want %d: %v vs %v", after, len(rest), len(want), rest, want)
	}
}

// ?after= is checked before the upgrade, and only /v2/ws has it.
func TestWSReplayAfterParameter(t *testing.T) {
	f := newReplayFixture(t, 0, wsReplayHooks{})
	f.mustPost(t, "ann", `{"body":"one"}`)

	tests := []struct {
		name       string
		path       string
		token      string
		wantStatus int
	}{
		{"zero", "/v2/ws?channel=ops&after=0", "olga", http.StatusSwitchingProtocols},
		{"a number", "/v2/ws?channel=ops&after=12", "olga", http.StatusSwitchingProtocols},
		{"negative", "/v2/ws?channel=ops&after=-1", "olga", http.StatusBadRequest},
		{"text", "/v2/ws?channel=ops&after=abc", "olga", http.StatusBadRequest},
		{"empty", "/v2/ws?channel=ops&after=", "olga", http.StatusBadRequest},
		{"fraction", "/v2/ws?channel=ops&after=1.5", "olga", http.StatusBadRequest},
		{"exponent", "/v2/ws?channel=ops&after=1e3", "olga", http.StatusBadRequest},
		{"hex", "/v2/ws?channel=ops&after=0x10", "olga", http.StatusBadRequest},
		{"beyond int64", "/v2/ws?channel=ops&after=99999999999999999999", "olga", http.StatusBadRequest},
		{"v1 does not have it", "/v1/ws?channel=ops&after=abc", "olga", http.StatusSwitchingProtocols},
		{"v0 does not have it", "/v0/ws?channel=ops&after=abc", "olga", http.StatusSwitchingProtocols},
		{"a non-member is refused as without it", "/v2/ws?channel=ops&after=0", "nora", http.StatusNotFound},
		{"a non-member's bad value is the same 400", "/v2/ws?channel=ops&after=abc", "nora", http.StatusBadRequest},
		{"an agent that may not read is refused as without it", "/v2/ws?channel=ops&after=0", "arlo", callWS(t, f.wsBase, "/v2/ws?channel=ops", f.p("arlo").token).status},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock := openSocket(t, f.wsBase, tt.path, f.p(tt.token).token)
			if sock.status != tt.wantStatus {
				t.Fatalf("status = %d %s, want %d", sock.status, sock.body, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusBadRequest {
				var e schema.Error
				if err := json.Unmarshal([]byte(sock.body), &e); err != nil || e.Code != "invalid_request" || !strings.Contains(e.Message, "after") {
					t.Errorf("body = %q, want invalid_request naming after", sock.body)
				}
			}
		})
	}
}
