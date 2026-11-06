package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Read state, end to end through the handlers (issue #351), on the scoped
// fixture: channel "ops" with people on net1, on net2 and on neither. Every
// document a handler sends is decoded strictly and run through Validate.

func decodeStrict(t *testing.T, body string, dst any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// readStates is GET /v1/read-state as token sees it, by channel id.
func (f *scopedFixture) readStates(t *testing.T, token string) map[int64]schema.ReadStateV1 {
	t.Helper()
	res := f.callREST(t, "GET", "/v1/read-state", token, "")
	if res.status != http.StatusOK {
		t.Fatalf("GET /v1/read-state = %d %s", res.status, res.body)
	}
	var list schema.ListReadStateResponseV1
	decodeStrict(t, res.body, &list)
	if list.ReadState == nil {
		t.Fatalf("read_state is null in %s", res.body)
	}
	out := map[int64]schema.ReadStateV1{}
	for _, rs := range list.ReadState {
		if err := rs.Validate(); err != nil {
			t.Errorf("channel %d: %v", rs.ChannelID, err)
		}
		out[rs.ChannelID] = rs
	}
	return out
}

func (f *scopedFixture) unread(t *testing.T, who string) int {
	t.Helper()
	rs, ok := f.readStates(t, f.p(who).token)[f.ops.ID]
	if !ok {
		t.Fatalf("%s has no read state for ops", who)
	}
	return rs.Unread
}

func (f *scopedFixture) putRead(t *testing.T, token string, channel string, id int64) (wireResult, schema.ReadStateV1) {
	t.Helper()
	res := f.callREST(t, "PUT", "/v1/channels/"+channel+"/read-state", token, fmt.Sprintf(`{"last_read_id":%d}`, id))
	var rs schema.ReadStateV1
	if res.status == http.StatusOK {
		decodeStrict(t, res.body, &rs)
		if err := rs.Validate(); err != nil {
			t.Errorf("PUT answered an invalid document: %v (%s)", err, res.body)
		}
	}
	return res, rs
}

func readStateErrorCode(t *testing.T, res wireResult) string {
	t.Helper()
	var e schema.Error
	decodeStrict(t, res.body, &e)
	return e.Code
}

// The issue's example: ann is on net1, has never marked ops, and sees three
// channel-wide messages, two on her net and one on another net as five. A
// whisper she is not in is not counted; one she is in is.
func TestReadStateCountsOnlyWhatTheReaderMaySee(t *testing.T) {
	f := newScopedFixture(t)
	for range 3 {
		f.mustPost(t, "olga", `{"body":"wide"}`)
	}
	f.mustPost(t, "nina", `{"body":"net1 a",`+netAudience(f.net1.ID)+`}`)
	f.mustPost(t, "nina", `{"body":"net1 b",`+netAudience(f.net1.ID)+`}`)
	f.mustPost(t, "wes", `{"body":"net2",`+netAudience(f.net2.ID)+`}`)

	steps := []struct {
		name string
		do   func()
		ann  int
		olga int
		nina int
	}{
		{"start", func() {}, 5, 0, 4},
		{"whisper ann is not in", func() {
			f.mustPost(t, "nina", `{"body":"psst",`+whisperAudience(f.p("wes").ID)+`}`)
		}, 5, 0, 4},
		{"whisper ann is in", func() {
			f.mustPost(t, "nina", `{"body":"psst",`+whisperAudience(f.p("ann").ID)+`}`)
		}, 6, 0, 4},
	}
	for _, s := range steps {
		s.do()
		for who, want := range map[string]int{"ann": s.ann, "olga": s.olga, "nina": s.nina} {
			if got := f.unread(t, who); got != want {
				t.Errorf("%s: %s unread = %d, want %d", s.name, who, got, want)
			}
		}
	}
	if rs := f.readStates(t, f.p("ann").token)[f.ops.ID]; rs.LastReadID != 0 || rs.UpdatedAt != nil {
		t.Errorf("never-marked state = %+v, want last_read_id 0 and no updated_at", rs)
	}
}

// Marking moves the count, and a mark is the same for every credential of the
// principal.
func TestReadStateMarkMovesTheCount(t *testing.T) {
	f := newScopedFixture(t)
	ids := []int64{
		f.mustPost(t, "olga", `{"body":"1"}`),
		f.mustPost(t, "nina", `{"body":"net",`+netAudience(f.net1.ID)+`}`),
		f.mustPost(t, "olga", `{"body":"3"}`),
		f.mustPost(t, "olga", `{"body":"4"}`),
	}
	_, secondTok, err := f.srv.store.CreateCredential(context.Background(), "system", f.p("ann").ID, "phone", nil)
	if err != nil {
		t.Fatal(err)
	}

	res, rs := f.putRead(t, f.p("ann").token, "ops", ids[1])
	if res.status != http.StatusOK || rs.LastReadID != ids[1] || rs.Unread != 2 || rs.UpdatedAt == nil {
		t.Fatalf("PUT = %d %s", res.status, res.body)
	}
	// The other credential of the same person sees the mark; olga, another
	// person, does not.
	if got := f.readStates(t, secondTok)[f.ops.ID]; got.LastReadID != ids[1] || got.Unread != 2 {
		t.Errorf("second credential sees %+v, want the shared mark", got)
	}
	if got := f.readStates(t, f.p("olga").token)[f.ops.ID]; got.LastReadID != 0 || got.Unread != 0 {
		t.Errorf("olga sees %+v, want her own untouched state", got)
	}
	// Marking from the second credential moves the first credential's view.
	if _, rs = f.putRead(t, secondTok, "ops", ids[3]); rs.Unread != 0 {
		t.Errorf("after reading to the end unread = %d", rs.Unread)
	}
	if got := f.readStates(t, f.p("ann").token)[f.ops.ID]; got.LastReadID != ids[3] || got.Unread != 0 {
		t.Errorf("first credential sees %+v after the second marked", got)
	}
}

func TestReadStatePut(t *testing.T) {
	f := newScopedFixture(t)
	w1 := f.mustPost(t, "olga", `{"body":"1"}`)
	w2 := f.mustPost(t, "olga", `{"body":"2"}`)
	hidden := f.mustPost(t, "nina", `{"body":"net1 only",`+netAudience(f.net1.ID)+`}`)

	// olga is not on net1: she cannot see the newest message.
	tests := []struct {
		name       string
		who        string
		channel    string
		body       string
		wantStatus int
		wantCode   string
		wantMark   int64
	}{
		{"forward", "olga", "ops", fmt.Sprintf(`{"last_read_id":%d}`, w1), 200, "", w1},
		{"backward is a no-op", "olga", "ops", fmt.Sprintf(`{"last_read_id":%d}`, w1-1), 200, "", w1},
		{"the same again", "olga", "ops", fmt.Sprintf(`{"last_read_id":%d}`, w1), 200, "", w1},
		{"zero is a no-op", "olga", "ops", `{"last_read_id":0}`, 200, "", w1},
		{"forward again", "olga", "ops", fmt.Sprintf(`{"last_read_id":%d}`, w2), 200, "", w2},
		{"beyond the newest visible is clamped, not to the hidden id", "olga", "ops", fmt.Sprintf(`{"last_read_id":%d}`, hidden+1000), 200, "", w2},
		{"to the hidden id itself is clamped too", "olga", "ops", fmt.Sprintf(`{"last_read_id":%d}`, hidden), 200, "", w2},
		{"a net member may mark the net message", "ann", "ops", fmt.Sprintf(`{"last_read_id":%d}`, hidden+1000), 200, "", hidden},
		{"non-member human", "nora", "ops", `{"last_read_id":1}`, 404, "channel_not_found", 0},
		{"unknown channel", "olga", "nosuch", `{"last_read_id":1}`, 404, "channel_not_found", 0},
		{"agent", "aria", "ops", `{"last_read_id":1}`, 403, schema.ErrorCodeHumanRequired, 0},
		{"agent, unknown channel", "aria", "nosuch", `{"last_read_id":1}`, 403, schema.ErrorCodeHumanRequired, 0},
		{"agent outside the channel", "nomad", "ops", `{"last_read_id":1}`, 403, schema.ErrorCodeHumanRequired, 0},
		{"negative", "olga", "ops", `{"last_read_id":-1}`, 400, "invalid_request", 0},
		{"unknown field", "olga", "ops", `{"last_read_id":1,"extra":1}`, 400, "invalid_request", 0},
		{"not json", "olga", "ops", `nope`, 400, "invalid_request", 0},
		{"empty object marks nothing", "olga", "ops", `{}`, 200, "", w2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.callREST(t, "PUT", "/v1/channels/"+tt.channel+"/read-state", f.p(tt.who).token, tt.body)
			if res.status != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", res.status, tt.wantStatus, res.body)
			}
			if tt.wantStatus != 200 {
				if code := readStateErrorCode(t, res); code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
				return
			}
			var rs schema.ReadStateV1
			decodeStrict(t, res.body, &rs)
			if err := rs.Validate(); err != nil {
				t.Error(err)
			}
			if rs.LastReadID != tt.wantMark || rs.ChannelID != f.ops.ID {
				t.Errorf("state = %+v, want mark %d in channel %d", rs, tt.wantMark, f.ops.ID)
			}
		})
	}

	// A non-member is told what an unknown channel is told, byte for byte.
	nora := f.callREST(t, "PUT", "/v1/channels/ops/read-state", f.p("nora").token, `{"last_read_id":1}`)
	nosuch := f.callREST(t, "PUT", "/v1/channels/nosuch/read-state", f.p("nora").token, `{"last_read_id":1}`)
	if nora != nosuch {
		t.Errorf("non-member %+v differs from unknown channel %+v", nora, nosuch)
	}
	// And nothing was stored for anyone who was refused.
	for _, who := range []string{"nora", "aria", "nomad"} {
		marks, err := f.srv.store.ReadMarksFor(context.Background(), f.p(who).ID)
		if err != nil || len(marks) != 0 {
			t.Errorf("%s has marks %v, %v after refusals", who, marks, err)
		}
	}
}

func TestReadStateList(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	side, err := f.srv.store.CreateChannel(ctx, "side")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", side.ID, f.p("olga").ID, 0); err != nil {
		t.Fatal(err)
	}
	f.mustPost(t, "ann", `{"body":"ops"}`)
	if _, err := f.srv.store.InsertMessageV1(ctx, side.ID, f.p("olga").ID, "side", nil); err != nil {
		t.Fatal(err)
	}

	got := f.readStates(t, f.p("olga").token)
	if len(got) != 2 || got[f.ops.ID].Unread != 1 || got[side.ID].Unread != 0 {
		t.Errorf("olga's states = %+v, want one in ops and none for her own post in side", got)
	}
	// ann is only in ops: side is absent, not zero.
	if _, ok := f.readStates(t, f.p("ann").token)[side.ID]; ok {
		t.Error("ann sees a channel she is not in")
	}
	// A person in nothing gets an empty array, not null.
	res := f.callREST(t, "GET", "/v1/read-state", f.p("nora").token, "")
	if res.status != 200 || strings.TrimSpace(res.body) != `{"read_state":[]}` {
		t.Errorf("nora's list = %d %s", res.status, res.body)
	}

	// Membership removed between two GETs removes the row, and a mark kept
	// from before comes back with the membership.
	f.putRead(t, f.p("olga").token, "side", 1<<40)
	if _, err := f.srv.store.RemoveChannelMember(ctx, "system", side.ID, f.p("olga").ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.readStates(t, f.p("olga").token)[side.ID]; ok {
		t.Error("a channel olga left is still listed")
	}
	if res, _ := f.putRead(t, f.p("olga").token, "side", 1); res.status != 404 {
		t.Errorf("PUT after leaving = %d, want 404", res.status)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", side.ID, f.p("olga").ID, 0); err != nil {
		t.Fatal(err)
	}
	if got := f.readStates(t, f.p("olga").token)[side.ID]; got.LastReadID == 0 || got.Unread != 0 {
		t.Errorf("after rejoining olga sees %+v, want her old mark", got)
	}
}

func TestReadStateRefusesAgents(t *testing.T) {
	f := newScopedFixture(t)
	for _, who := range []string{"aria", "arlo", "nomad"} {
		res := f.callREST(t, "GET", "/v1/read-state", f.p(who).token, "")
		if res.status != http.StatusForbidden || readStateErrorCode(t, res) != schema.ErrorCodeHumanRequired {
			t.Errorf("%s: GET = %d %s, want 403 human_required", who, res.status, res.body)
		}
	}
	// Without a credential there is nobody to refuse or serve.
	if res := f.callREST(t, "GET", "/v1/read-state", "", ""); res.status != http.StatusUnauthorized {
		t.Errorf("no credential: %d", res.status)
	}
}

func TestReadStateAuthOff(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff})
	f := &authFixture{srv: srv}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/read-state", ""},
		{"PUT", "/v1/channels/general/read-state", `{"last_read_id":1}`},
	} {
		res := f.callREST(t, c.method, c.path, "", c.body)
		if res.status != http.StatusBadRequest || readStateErrorCode(t, res) != schema.ErrorCodeReadStateRequireAuth {
			t.Errorf("%s %s = %d %s, want 400 read_state_require_auth", c.method, c.path, res.status, res.body)
		}
	}
}

// Disabling a person hides what was scoped to them (re-enabling restores it),
// and removing someone from the channel takes the channel out of their list.
func TestReadStateFollowsTheVisibilityRule(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	f.mustPost(t, "olga", `{"body":"wide"}`)
	f.mustPost(t, "nina", `{"body":"to dina",`+whisperAudience(f.p("dina").ID)+`}`)
	f.mustPost(t, "nina", `{"body":"net1",`+netAudience(f.net1.ID)+`}`)
	if got := f.unread(t, "dina"); got != 3 {
		t.Fatalf("dina unread = %d, want 3", got)
	}
	if _, err := f.srv.store.DisablePrincipal(ctx, "system", f.p("dina").ID); err != nil {
		t.Fatal(err)
	}
	// A disabled principal's credential is refused outright; ask the store
	// for the count the way the handler does, to see the predicate at work.
	n, err := f.srv.store.CountVisibleMessagesAfter(ctx, f.ops.ID, store.Reader{PrincipalID: f.p("dina").ID, Scoped: true}, 0, schema.MaxUnreadCount)
	if err != nil || n != 1 {
		t.Errorf("disabled dina counts %d, %v; want 1", n, err)
	}
	if _, err := f.srv.store.EnablePrincipal(ctx, "system", f.p("dina").ID); err != nil {
		t.Fatal(err)
	}
	if n, err = f.srv.store.CountVisibleMessagesAfter(ctx, f.ops.ID, store.Reader{PrincipalID: f.p("dina").ID, Scoped: true}, 0, schema.MaxUnreadCount); err != nil || n != 3 {
		t.Errorf("re-enabled dina counts %d, %v; want 3", n, err)
	}
	// Olga has a credential that still works: removing her takes the channel
	// out of her list.
	if _, err := f.srv.store.RemoveChannelMember(ctx, "system", f.ops.ID, f.p("olga").ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.readStates(t, f.p("olga").token)[f.ops.ID]; ok {
		t.Error("olga still has read state for a channel she left")
	}
}

// The count is capped at the schema's maximum.
func TestReadStateUnreadIsCapped(t *testing.T) {
	if testing.Short() {
		t.Skip("posts more than a thousand messages")
	}
	f := newScopedFixture(t)
	ctx := context.Background()
	for i := 0; i < schema.MaxUnreadCount+5; i++ {
		if _, err := f.srv.store.InsertMessageV1(ctx, f.ops.ID, f.p("olga").ID, "m", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.unread(t, "ann"); got != schema.MaxUnreadCount {
		t.Errorf("unread = %d, want the cap %d", got, schema.MaxUnreadCount)
	}
	_, rs := f.putRead(t, f.p("ann").token, "ops", 3)
	if rs.Unread != schema.MaxUnreadCount {
		t.Errorf("unread after a mark of 3 = %d, want the cap", rs.Unread)
	}
}

// read_state.go reads no message rows and names no message table. Its count is
// the store's, and the readers it passes are built by readerFor.
// (TestMessageReadsStayInTheVisibilityFunction holds the general rule; this
// one pins this file's shape.)
func TestReadStateHandlersCountThroughTheStore(t *testing.T) {
	src, err := os.ReadFile("read_state.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	for _, want := range []string{"CountVisibleUnreadMessagesAfter(", "readerFor(r, apiV2)", "MarkRead("} {
		if !strings.Contains(code, want) {
			t.Errorf("read_state.go does not call %s", want)
		}
	}
	for _, forbidden := range []string{"database/sql", "FROM messages", "from messages", "message_recipients", "store.Reader{", "ListVisibleMessages"} {
		if strings.Contains(code, forbidden) {
			t.Errorf("read_state.go contains %q", forbidden)
		}
	}
}
