package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// memberFixture extends authFixture with a channel "alpha" whose only member
// is alice, and a non-member human "carol".
type memberFixture struct {
	*authFixture
	alpha    store.Channel
	general  store.Channel
	carol    store.Principal
	carolTok string
}

func newMemberFixture(t *testing.T) *memberFixture {
	t.Helper()
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	m := &memberFixture{authFixture: f}
	var err error
	if m.general, err = f.srv.store.ChannelByName(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	if m.alpha, err = f.srv.store.CreateChannel(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", m.alpha.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	if m.carol, err = f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, m.carolTok, err = f.srv.store.CreateCredential(ctx, "system", m.carol.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	// The fixture agent's manifest allows both channels, so that in these
	// tests membership alone decides what it can reach.
	setAgentManifest(t, f.srv, f.bot.ID, nil, m.general.ID, m.alpha.ID)
	return m
}

type wireResult struct {
	status      int
	body        string
	contentType string
}

// callREST sends a REST request and captures everything the client sees.
func (f *authFixture) callREST(t *testing.T, method, path, token, body string) wireResult {
	t.Helper()
	rec := f.do(t, method, path, token, body)
	return wireResult{rec.Code, rec.Body.String(), rec.Header().Get("Content-Type")}
}

// callWS dials a WebSocket route. A refused upgrade yields the HTTP response;
// an accepted one yields status 101 and an empty body.
func callWS(t *testing.T, base, path, token string) wireResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var hdr http.Header
	if token != "" {
		hdr = http.Header{"Authorization": {"Bearer " + token}}
	}
	conn, resp, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		_ = conn.CloseNow()
		return wireResult{status: http.StatusSwitchingProtocols}
	}
	if resp == nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return wireResult{resp.StatusCode, sb.String(), resp.Header.Get("Content-Type")}
}

// TestChannelContentLeak is the visibility-boundary test: it runs every REST
// and WebSocket path that returns or accepts channel content, as a member, a
// non-member member-role human, a non-member agent, and a non-member
// operator, and requires that every non-member response is byte-identical to
// the response for a channel that does not exist.
//
// EVERY NEW READ OR WRITE PATH OVER CHANNEL CONTENT MUST ADD A ROW HERE. A new
// way to READ MESSAGES must also extend the exact-set leak test,
// TestScopedMessagesExactSets, which asserts which messages each kind of reader
// receives once messages can carry an audience (issue #116); the completeness
// guard below fails for a route with no row here, and
// TestEveryMessageReadPathIsInTheLeakTest fails for a message-reading route
// that is not in that test.
func TestChannelContentLeak(t *testing.T) {
	f := newMemberFixture(t)
	base := wsTestServer(t, f.srv)
	const notFound = http.StatusNotFound
	const ok, created, upgraded = http.StatusOK, http.StatusCreated, http.StatusSwitchingProtocols

	callers := []struct {
		who   string
		token string
	}{
		{"member", f.aliceTok},
		{"non-member human", f.carolTok},
		{"non-member agent", f.botTok},
		{"non-member operator", f.rootTok},
	}
	// want: status per caller, in the order of callers above. "nosuch" is the
	// reference: the response a non-member must be indistinguishable from.
	rows := []struct {
		name   string
		method string
		path   string // %s is the channel name
		body   string
		ws     bool
		want   [4]int
	}{
		{"v0 message list", "GET", "/v0/channels/%s/messages", "", false, [4]int{ok, notFound, notFound, notFound}},
		{"v1 message list", "GET", "/v1/channels/%s/messages", "", false, [4]int{ok, notFound, notFound, notFound}},
		{"v2 message list", "GET", "/v2/channels/%s/messages", "", false, [4]int{ok, notFound, notFound, notFound}},
		{"v0 message post", "POST", "/v0/channels/%s/messages", `{"body":"hi"}`, false, [4]int{created, notFound, notFound, notFound}},
		{"v1 message post", "POST", "/v1/channels/%s/messages", `{"body":"hi"}`, false, [4]int{created, notFound, notFound, notFound}},
		{"v2 message post", "POST", "/v2/channels/%s/messages", `{"body":"hi"}`, false, [4]int{created, notFound, notFound, notFound}},
		{"v0 websocket", "GET", "/v0/ws?channel=%s", "", true, [4]int{upgraded, notFound, notFound, notFound}},
		{"v1 websocket", "GET", "/v1/ws?channel=%s", "", true, [4]int{upgraded, notFound, notFound, notFound}},
		{"v2 websocket", "GET", "/v2/ws?channel=%s", "", true, [4]int{upgraded, notFound, notFound, notFound}},
		// Listing members is allowed to members and to operators only.
		{"member list", "GET", "/v1/channels/%s/members", "", false, [4]int{ok, notFound, notFound, ok}},
		// Listing nets: members see their own nets, operators all of them.
		{"net list", "GET", "/v1/channels/%s/nets", "", false, [4]int{ok, notFound, notFound, ok}},
	}
	// Completeness guard: every route that addresses a channel must either have
	// a row above or be listed here with the reason it is not a content path.
	// A new channel route added without a decision fails this test.
	notContent := map[string]string{
		"PUT /v1/channels/{channel}/members/{principal_id}":               "operator-only membership write; covered by the role matrix",
		"DELETE /v1/channels/{channel}/members/{principal_id}":            "operator-only membership write; covered by the role matrix",
		"POST /v1/channels/{channel}/nets":                                "operator-only net write; covered by the role matrix and TestNetEndpoints",
		"DELETE /v1/channels/{channel}/nets/{net}":                        "operator-only net write; covered by the role matrix and TestNetEndpoints",
		"PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}":    "operator-only net write; covered by the role matrix and TestNetEndpoints",
		"DELETE /v1/channels/{channel}/nets/{net}/members/{principal_id}": "operator-only net write; covered by the role matrix and TestNetEndpoints",
	}
	covered := make(map[string]bool, len(rows))
	for _, row := range rows {
		path := strings.ReplaceAll(strings.TrimSuffix(row.path, "?channel=%s"), "%s", "{channel}")
		covered[row.method+" "+path] = true
	}
	for _, rt := range f.srv.routes {
		if !strings.Contains(rt.pattern, "{channel}") && !strings.Contains(rt.pattern, "/ws") {
			continue
		}
		if !covered[rt.pattern] && notContent[rt.pattern] == "" {
			t.Fatalf("route %q addresses a channel but has no row in this leak test", rt.pattern)
		}
	}
	call := func(method, path, token, body string, ws bool) wireResult {
		if ws {
			return callWS(t, base, path, token)
		}
		return f.callREST(t, method, path, token, body)
	}
	for _, row := range rows {
		for i, c := range callers {
			t.Run(row.name+"/"+c.who, func(t *testing.T) {
				got := call(row.method, fmt.Sprintf(row.path, "alpha"), c.token, row.body, row.ws)
				if got.status != row.want[i] {
					t.Fatalf("status = %d, want %d; body %s", got.status, row.want[i], got.body)
				}
				if row.want[i] != notFound {
					return
				}
				// A non-member must not be able to tell the channel exists.
				ref := call(row.method, fmt.Sprintf(row.path, "nosuch"), c.token, row.body, row.ws)
				if got != ref {
					t.Errorf("non-member response differs from unknown-channel response:\n got  %+v\n want %+v", got, ref)
				}
				var e schema.Error
				if err := json.Unmarshal([]byte(got.body), &e); err != nil || e.Code != "channel_not_found" {
					t.Errorf("body = %q, want channel_not_found", got.body)
				}
			})
		}
	}

	// Denied posts wrote nothing: only the member's three channel-wide posts
	// exist, and the audit log holds exactly their three message.post events.
	msgs, err := f.srv.store.ListVisibleMessages(context.Background(), f.alpha.ID, store.ChannelWideOnly, 0, 100)
	if err != nil || len(msgs) != 3 {
		t.Errorf("alpha messages = %d (%v), want only the member's 3", len(msgs), err)
	}
	if n, err := f.srv.store.CountMessages(context.Background(), f.alpha.ID); err != nil || n != 3 {
		t.Errorf("alpha holds %d messages in all (%v), want 3", n, err)
	}
	for _, m := range msgs {
		if m.AuthorID != f.alice.ID {
			t.Errorf("message %d author = %d, want alice", m.ID, m.AuthorID)
		}
	}
	if n := countAction(f.audit(t), "message.post"); n != 3 {
		t.Errorf("message.post audit events = %d, want 3", n)
	}
	if n := countAction(f.audit(t), store.AuditMessageScoped); n != 0 {
		t.Errorf("message_scoped audit events = %d, want 0", n)
	}

	// The channel list shows each caller only their own channels, operators
	// included.
	listTests := []struct {
		who   string
		token string
		want  []string
	}{
		{"member", f.aliceTok, []string{"general", "alpha"}},
		{"non-member human", f.carolTok, nil},
		{"non-member agent", f.botTok, []string{"general"}},
		{"non-member operator", f.rootTok, []string{"general"}},
	}
	for _, tt := range listTests {
		t.Run("channel list/"+tt.who, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/channels", tt.token, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			resp := decodeBody[schema.ListChannelsResponse](t, rec)
			var got []string
			for _, ch := range resp.Channels {
				got = append(got, ch.Name)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("channels = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMembershipEndpoints(t *testing.T) {
	f := newMemberFixture(t)
	ctx := context.Background()
	carolPath := fmt.Sprintf("/v1/channels/alpha/members/%d", f.carol.ID)

	events := func(action string) []store.AuditEvent {
		var out []store.AuditEvent
		for _, e := range f.audit(t) {
			if e.Action == action {
				out = append(out, e)
			}
		}
		return out
	}
	baseAdded := len(events(store.AuditMemberAdded))
	baseRemoved := len(events(store.AuditMemberRemoved))

	steps := []struct {
		name        string
		method      string
		path        string
		token       string
		want        int
		wantCode    string
		wantAdded   int // cumulative new member_added events
		wantRemoved int // cumulative new member_removed events
	}{
		{"unauthenticated put", "PUT", carolPath, "", 401, "unauthenticated", 0, 0},
		{"member put is forbidden", "PUT", carolPath, f.aliceTok, 403, "forbidden", 0, 0},
		{"non-member put is forbidden", "PUT", carolPath, f.carolTok, 403, "forbidden", 0, 0},
		{"member delete is forbidden", "DELETE", carolPath, f.aliceTok, 403, "forbidden", 0, 0},
		{"operator put adds", "PUT", carolPath, f.rootTok, 204, "", 1, 0},
		{"operator put again is idempotent", "PUT", carolPath, f.rootTok, 204, "", 1, 0},
		{"operator delete removes", "DELETE", carolPath, f.rootTok, 204, "", 1, 1},
		{"operator delete again is idempotent", "DELETE", carolPath, f.rootTok, 204, "", 1, 1},
		{"put unknown channel", "PUT", "/v1/channels/nosuch/members/2", f.rootTok, 404, "channel_not_found", 1, 1},
		{"delete unknown channel", "DELETE", "/v1/channels/nosuch/members/2", f.rootTok, 404, "channel_not_found", 1, 1},
		{"put unknown principal", "PUT", "/v1/channels/alpha/members/9999", f.rootTok, 404, "principal_not_found", 1, 1},
		{"delete unknown principal", "DELETE", "/v1/channels/alpha/members/9999", f.rootTok, 404, "principal_not_found", 1, 1},
		{"put bad principal id", "PUT", "/v1/channels/alpha/members/abc", f.rootTok, 400, "invalid_request", 1, 1},
		{"delete zero principal id", "DELETE", "/v1/channels/alpha/members/0", f.rootTok, 400, "invalid_request", 1, 1},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			rec := f.do(t, st.method, st.path, st.token, "")
			if st.wantCode != "" {
				assertErrorBody(t, rec, st.want, st.wantCode)
			} else if rec.Code != st.want || rec.Body.Len() != 0 {
				t.Fatalf("status = %d body %q, want %d with empty body", rec.Code, rec.Body, st.want)
			}
			if n := len(events(store.AuditMemberAdded)) - baseAdded; n != st.wantAdded {
				t.Errorf("member_added events = %d, want %d", n, st.wantAdded)
			}
			if n := len(events(store.AuditMemberRemoved)) - baseRemoved; n != st.wantRemoved {
				t.Errorf("member_removed events = %d, want %d", n, st.wantRemoved)
			}
		})
	}

	// Audit content: actor is the operator, subject the channel, detail the target.
	wantActor := fmt.Sprintf("principal:%d", f.root.ID)
	wantSubject := fmt.Sprintf("channel:%d", f.alpha.ID)
	wantDetail := fmt.Sprintf("principal=%d", f.carol.ID)
	for _, e := range append(events(store.AuditMemberAdded)[baseAdded:], events(store.AuditMemberRemoved)[baseRemoved:]...) {
		if e.Actor != wantActor || e.Subject != wantSubject || e.Detail != wantDetail {
			t.Errorf("audit = %+v, want actor %q subject %q detail %q", e, wantActor, wantSubject, wantDetail)
		}
	}
	// The add recorded who added the member.
	rec := f.do(t, "PUT", carolPath, f.rootTok, "")
	if rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	ms, err := f.srv.store.ListChannelMembers(ctx, f.alpha.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.PrincipalID == f.carol.ID && m.AddedBy != f.root.ID {
			t.Errorf("added_by = %d, want operator %d", m.AddedBy, f.root.ID)
		}
	}
}

func TestListChannelMembers(t *testing.T) {
	f := newMemberFixture(t)
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/alpha/members/%d", f.bot.ID), f.rootTok, ""); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	tests := []struct {
		name    string
		channel string
		token   string
		want    int
		wantIDs []int64
	}{
		{"member", "alpha", f.aliceTok, 200, []int64{f.alice.ID, f.bot.ID}},
		{"other member", "alpha", f.botTok, 200, []int64{f.alice.ID, f.bot.ID}},
		{"operator non-member", "alpha", f.rootTok, 200, []int64{f.alice.ID, f.bot.ID}},
		{"non-member", "alpha", f.carolTok, 404, nil},
		{"unknown channel as operator", "nosuch", f.rootTok, 404, nil},
		{"unknown channel as member", "nosuch", f.aliceTok, 404, nil},
		{"unauthenticated", "alpha", "", 401, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/channels/"+tt.channel+"/members", tt.token, "")
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want != 200 {
				return
			}
			resp := decodeBody[schema.ListChannelMembersResponseV1](t, rec)
			var got []int64
			for _, m := range resp.Members {
				got = append(got, m.PrincipalID)
				if m.CreatedAt.IsZero() {
					t.Errorf("member %d has no created_at", m.PrincipalID)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.wantIDs) {
				t.Errorf("members = %v, want %v", got, tt.wantIDs)
			}
		})
	}
	// An empty channel lists as [] and not null.
	if _, err := f.srv.store.CreateChannel(context.Background(), "empty"); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "GET", "/v1/channels/empty/members", f.rootTok, "")
	if strings.TrimSpace(rec.Body.String()) != `{"members":[]}` {
		t.Errorf("empty list body = %s", rec.Body)
	}
}

func TestCreatorMembership(t *testing.T) {
	f := newMemberFixture(t)
	rec := f.do(t, "POST", "/v0/channels", f.rootTok, `{"name":"ops"}`)
	if rec.Code != 201 {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	ch := decodeBody[schema.CreateChannelResponse](t, rec).Channel

	members := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/ops/members", f.rootTok, ""))
	if len(members.Members) != 1 || members.Members[0].PrincipalID != f.root.ID || members.Members[0].AddedBy != f.root.ID {
		t.Fatalf("members = %+v, want only the creator", members.Members)
	}
	// The creator reads and posts; nobody else sees it.
	if rec := f.do(t, "POST", "/v0/channels/ops/messages", f.rootTok, `{"body":"x"}`); rec.Code != 201 {
		t.Errorf("creator post = %d", rec.Code)
	}
	for _, tok := range []string{f.aliceTok, f.botTok, f.carolTok} {
		assertErrorBody(t, f.do(t, "GET", "/v0/channels/ops/messages", tok, ""), 404, "channel_not_found")
	}
	var found bool
	for _, e := range f.audit(t) {
		if e.Action == store.AuditMemberAdded && e.Subject == fmt.Sprintf("channel:%d", ch.ID) {
			found = true
			if e.Actor != fmt.Sprintf("principal:%d", f.root.ID) || e.Detail != fmt.Sprintf("principal=%d", f.root.ID) {
				t.Errorf("creator audit = %+v", e)
			}
		}
	}
	if !found {
		t.Error("no member_added event for the creator")
	}
	// A duplicate create leaves no membership behind.
	if rec := f.do(t, "POST", "/v0/channels", f.rootTok, `{"name":"ops"}`); rec.Code != 409 {
		t.Errorf("duplicate create = %d", rec.Code)
	}
}

// TestMembershipAuthOff: without a caller the membership routes work without
// role checks, channels start with no members, nothing is filtered, and the
// audit actor is "system".
func TestMembershipAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	steps := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v0/channels", `{"name":"fresh"}`, 201},
		{"GET", "/v1/channels/fresh/members", "", 200},
		{"PUT", fmt.Sprintf("/v1/channels/fresh/members/%d", f.alice.ID), "", 204},
		{"PUT", fmt.Sprintf("/v1/channels/fresh/members/%d", f.alice.ID), "", 204},
		{"GET", "/v1/channels/fresh/messages", "", 200},
		{"POST", "/v0/channels/fresh/messages", fmt.Sprintf(`{"author_id":%d,"body":"x"}`, f.bot.ID), 201},
		{"DELETE", fmt.Sprintf("/v1/channels/fresh/members/%d", f.alice.ID), "", 204},
		{"GET", "/v1/channels/nosuch/members", "", 404},
	}
	for _, st := range steps {
		if rec := f.do(t, st.method, st.path, "", st.body); rec.Code != st.want {
			t.Fatalf("%s %s = %d, want %d; %s", st.method, st.path, rec.Code, st.want, rec.Body)
		}
	}
	// New channel had no members; everything is listed regardless.
	chans := decodeBody[schema.ListChannelsResponse](t, f.do(t, "GET", "/v1/channels", "", ""))
	if len(chans.Channels) != 2 {
		t.Errorf("channels = %+v, want both listed", chans.Channels)
	}
	var added, removed int
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditMemberAdded:
			if e.Actor != "system" && e.Subject != "channel:1" {
				t.Errorf("auth-off audit actor = %q", e.Actor)
			}
			added++
		case store.AuditMemberRemoved:
			if e.Actor != "system" {
				t.Errorf("auth-off audit actor = %q", e.Actor)
			}
			removed++
		}
	}
	// 3 from the authFixture, 1 idempotent-once add, 1 remove.
	if added != 4 || removed != 1 {
		t.Errorf("member_added = %d (want 4), member_removed = %d (want 1)", added, removed)
	}
}

// wsReader reads frames from conn until it errors, recording message ids.
type wsReader struct {
	mu   sync.Mutex
	ids  []int64
	err  error
	done chan struct{}
	conn *websocket.Conn
}

func startReader(ctx context.Context, conn *websocket.Conn) *wsReader {
	r := &wsReader{done: make(chan struct{}), conn: conn}
	go func() {
		defer close(r.done)
		for {
			var m schema.MessageV1
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				r.mu.Lock()
				r.err = err
				r.mu.Unlock()
				return
			}
			r.mu.Lock()
			r.ids = append(r.ids, m.ID)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *wsReader) waitClosed(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("websocket was not closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *wsReader) waitForID(t *testing.T, id int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, got := range r.ids {
			if got == id {
				r.mu.Unlock()
				return
			}
		}
		r.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("message %d never delivered", id)
}

func (r *wsReader) seen() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.ids...)
}

func dialAs(t *testing.T, ctx context.Context, base, path, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{ //nolint:bodyclose // upgraded
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func (f *authFixture) postAs(t *testing.T, token, channel, body string) int64 {
	t.Helper()
	rec := f.do(t, "POST", "/v1/channels/"+channel+"/messages", token, fmt.Sprintf(`{"body":%q}`, body))
	if rec.Code != 201 {
		t.Fatalf("post to %s = %d %s", channel, rec.Code, rec.Body)
	}
	return decodeBody[schema.PostMessageResponseV1](t, rec).Message.ID
}

// TestWebSocketRemovedMemberIsDropped: removing a member closes that member's
// sockets on that channel only; other members and the member's other channels
// are unaffected, and nothing posted after removal reaches the removed socket.
func TestWebSocketRemovedMemberIsDropped(t *testing.T) {
	for _, version := range []string{"v0", "v1"} {
		t.Run(version, func(t *testing.T) {
			f := newMemberFixture(t)
			base := wsTestServer(t, f.srv)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// bot joins alpha; alice is in alpha and general.
			if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/alpha/members/%d", f.bot.ID), f.rootTok, ""); rec.Code != 204 {
				t.Fatal(rec.Code)
			}
			ws := func(ch string) string { return "/" + version + "/ws?channel=" + ch }
			// Frames are decoded as V1; v0 frames are a subset of its fields.
			aliceAlpha := startReader(ctx, dialAs(t, ctx, base, ws("alpha"), f.aliceTok))
			aliceAlpha2 := startReader(ctx, dialAs(t, ctx, base, ws("alpha"), f.aliceTok))
			aliceGeneral := startReader(ctx, dialAs(t, ctx, base, ws("general"), f.aliceTok))
			botAlpha := startReader(ctx, dialAs(t, ctx, base, ws("alpha"), f.botTok))

			before := f.postAs(t, f.botTok, "alpha", "before")
			aliceAlpha.waitForID(t, before)
			botAlpha.waitForID(t, before)

			if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/alpha/members/%d", f.alice.ID), f.rootTok, ""); rec.Code != 204 {
				t.Fatalf("remove = %d", rec.Code)
			}
			after := f.postAs(t, f.botTok, "alpha", "after")
			generalMsg := f.postAs(t, f.aliceTok, "general", "still here")

			for name, r := range map[string]*wsReader{"alice alpha": aliceAlpha, "alice alpha (second socket)": aliceAlpha2} {
				err := r.waitClosed(t)
				if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
					t.Errorf("%s: close = %v, want policy violation", name, err)
				} else if !strings.Contains(err.Error(), "no longer a member") {
					t.Errorf("%s: close reason = %v", name, err)
				}
				for _, id := range r.seen() {
					if id >= after {
						t.Errorf("%s received message %d posted after removal", name, id)
					}
				}
			}
			botAlpha.waitForID(t, after)
			aliceGeneral.waitForID(t, generalMsg)
			select {
			case <-aliceGeneral.done:
				t.Error("alice's other channel socket was closed")
			default:
			}
			// She can no longer reconnect to alpha.
			if got := callWS(t, base, ws("alpha"), f.aliceTok); got.status != 404 {
				t.Errorf("reconnect status = %d, want 404", got.status)
			}
		})
	}
}

// TestRemovalRacesBroadcast removes a member while another principal posts
// continuously. A post that began after the removal returned must never reach
// the removed member's socket. Run with -race.
func TestRemovalRacesBroadcast(t *testing.T) {
	const rounds = 5
	for round := 0; round < rounds; round++ {
		f := newMemberFixture(t)
		base := wsTestServer(t, f.srv)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/alpha/members/%d", f.bot.ID), f.rootTok, ""); rec.Code != 204 {
			t.Fatal(rec.Code)
		}
		reader := startReader(ctx, dialAs(t, ctx, base, "/v1/ws?channel=alpha", f.aliceTok))
		first := f.postAs(t, f.botTok, "alpha", "warmup")
		reader.waitForID(t, first)

		var removed atomic.Bool
		var mu sync.Mutex
		postRemoval := map[int64]bool{}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					startedAfter := removed.Load()
					rec := f.do(t, "POST", "/v1/channels/alpha/messages", f.botTok, `{"body":"race"}`)
					if rec.Code != 201 {
						t.Errorf("post = %d", rec.Code)
						return
					}
					if startedAfter {
						id := decodeBody[schema.PostMessageResponseV1](t, rec).Message.ID
						mu.Lock()
						postRemoval[id] = true
						mu.Unlock()
					}
				}
			}()
		}
		time.Sleep(20 * time.Millisecond)
		if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/alpha/members/%d", f.alice.ID), f.rootTok, ""); rec.Code != 204 {
			t.Fatalf("remove = %d", rec.Code)
		}
		removed.Store(true)
		time.Sleep(30 * time.Millisecond)
		close(stop)
		wg.Wait()

		if err := reader.waitClosed(t); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Errorf("close = %v, want policy violation", err)
		}
		mu.Lock()
		if len(postRemoval) == 0 {
			t.Log("no post started after removal in this round")
		}
		for _, id := range reader.seen() {
			if postRemoval[id] {
				t.Errorf("round %d: removed member received message %d posted after removal", round, id)
			}
		}
		mu.Unlock()
		cancel()
	}
}

// TestHookIngestIgnoresMembership: webhook ingest has no caller and does not
// check membership.
// A webhook hook posts as its principal, so it must follow that principal's
// membership: a hook for a non-member, or for a member who is later removed,
// answers exactly like an unknown token and writes nothing.
func TestHookIngestRequiresMembership(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)
	const memberHook, outsiderHook = "hook-for-alice", "hook-for-bot"
	// alpha has alice only; bot is not a member.
	if _, err := f.srv.store.CreateHook(ctx, memberHook, f.alpha.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.CreateHook(ctx, outsiderHook, f.alpha.ID, f.bot.ID); err != nil {
		t.Fatal(err)
	}
	countMessages := func() int {
		t.Helper()
		messages, err := f.srv.store.ListVisibleMessages(ctx, f.alpha.ID, store.ChannelWideOnly, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(messages)
	}
	unknown := f.do(t, "POST", "/v1/hooks/no-such-token", "", `{"body":"x"}`)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown token = %d", unknown.Code)
	}
	before := countMessages()

	if rec := f.do(t, "POST", "/v1/hooks/"+outsiderHook, "", `{"body":"from a non-member"}`); rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
		t.Errorf("hook for a non-member = %d %s, want the unknown-token response %d %s", rec.Code, rec.Body, unknown.Code, unknown.Body)
	}
	if got := countMessages(); got != before {
		t.Fatalf("a hook for a non-member posted a message")
	}

	if rec := f.do(t, "POST", "/v1/hooks/"+memberHook, "", `{"body":"from a member"}`); rec.Code != http.StatusCreated {
		t.Fatalf("hook for a member = %d %s", rec.Code, rec.Body)
	}
	if got := countMessages(); got != before+1 {
		t.Fatalf("messages = %d, want %d", got, before+1)
	}

	if _, err := f.srv.store.RemoveChannelMember(ctx, "system", f.alpha.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "POST", "/v1/hooks/"+memberHook, "", `{"body":"after removal"}`); rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
		t.Errorf("hook after its principal was removed = %d %s, want the unknown-token response", rec.Code, rec.Body)
	}
	if got := countMessages(); got != before+1 {
		t.Fatalf("a hook posted after its principal was removed from the channel")
	}
}

// With authentication off there is no membership boundary, so hooks behave as
// they always did.
func TestHookIngestIgnoresMembershipWhenAuthOff(t *testing.T) {
	ctx := context.Background()
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff})
	channel, err := srv.store.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateHook(ctx, "open-hook", channel.ID, agent.ID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/hooks/open-hook", strings.NewReader(`{"body":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest with auth off = %d %s", rec.Code, rec.Body)
	}
}
