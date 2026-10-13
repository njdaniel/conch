package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Threads (issue #25), over the scoped fixture: a reply has its root's
// audience and a thread read shows what the channel listing shows.

func (f *scopedFixture) replyBody(root int64, text string) string {
	return fmt.Sprintf(`{"body":%q,"thread_root_id":%d}`, text, root)
}

func (f *scopedFixture) postReply(t *testing.T, who string, root int64) wireResult {
	t.Helper()
	return f.postV2(t, who, f.replyBody(root, "re: "+who))
}

// listPage decodes a v2 list or thread response.
func decodeMessages(t *testing.T, body string) schema.ListMessagesResponseV2 {
	t.Helper()
	var resp schema.ListMessagesResponseV2
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return resp
}

func (f *scopedFixture) channelList(t *testing.T, who string) wireResult {
	t.Helper()
	return f.callREST(t, "GET", "/v2/channels/ops/messages?limit=100", f.p(who).token, "")
}

func (f *scopedFixture) threadList(t *testing.T, who string, root int64, extra string) wireResult {
	t.Helper()
	return f.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages?thread=%d%s", root, extra), f.p(who).token, "")
}

func errorCode(t *testing.T, res wireResult) string {
	t.Helper()
	var e schema.Error
	if err := json.Unmarshal([]byte(res.body), &e); err != nil {
		t.Fatalf("decode error %q: %v", res.body, err)
	}
	return e.Code
}

// outcome is an expected answer to a reply: a status and, for a refusal, a code.
type outcome struct {
	status int
	code   string
}

var (
	replied   = outcome{http.StatusCreated, ""}
	noChannel = outcome{http.StatusNotFound, "channel_not_found"}
	noThread  = outcome{http.StatusNotFound, "thread_not_found"}
	refused   = outcome{http.StatusForbidden, "forbidden"}
	unauthn   = outcome{http.StatusUnauthorized, ""}
)

// TestThreadVisibilityMatrix is the audience boundary: three kinds of root by
// every kind of replier and reader. For each cell it asserts whether they may
// reply, what the thread listing shows them and what the channel listing shows
// them, and that the thread listing is the channel listing narrowed to the
// thread.
func TestThreadVisibilityMatrix(t *testing.T) {
	type root struct {
		name   string
		post   func(f *scopedFixture) int64
		expect map[string]outcome
		// granted are agents refused above who are then given the grants
		// replying needs, and must then succeed.
		granted []string
		// leaves is a member who is removed from the channel after the root and
		// the replies; their next reply is refused as to any non-member.
		leaves string
	}
	everyone := []string{"ann", "nina", "mona", "wes", "olga", "root", "dina", "aria", "arlo", "nomad", "nora", ""}
	roots := []root{
		{
			name: "channel-wide",
			post: func(f *scopedFixture) int64 { return f.mustPost(t, "olga", `{"body":"wide root"}`) },
			expect: map[string]outcome{
				"ann": replied, "nina": replied, "mona": replied, "wes": replied, "olga": replied, "root": replied,
				"dina": replied, "aria": replied, "arlo": replied, "nomad": noChannel, "nora": noChannel, "": unauthn,
			},
			leaves: "wes",
		},
		{
			name: "net",
			post: func(f *scopedFixture) int64 {
				return f.mustPost(t, "ann", fmt.Sprintf(`{"body":"net root",%s}`, netAudience(f.net1.ID)))
			},
			// net1: ann, nina, aria, dina members, mona monitor. aria has the
			// post grant only.
			expect: map[string]outcome{
				"ann": replied, "nina": replied, "dina": replied, "mona": refused, "aria": refused,
				"wes": noThread, "olga": noThread, "root": noThread, "arlo": noThread,
				"nomad": noChannel, "nora": noChannel, "": unauthn,
			},
			granted: []string{"aria"},
			leaves:  "nina",
		},
		{
			name: "whisper",
			post: func(f *scopedFixture) int64 {
				return f.mustPost(t, "ann", fmt.Sprintf(`{"body":"whisper root",%s}`, whisperAudience(f.p("wes").ID, f.p("arlo").ID)))
			},
			// ann writes to wes and arlo (an agent with the post grant only).
			expect: map[string]outcome{
				"ann": replied, "wes": replied, "arlo": refused,
				"nina": noThread, "mona": noThread, "olga": noThread, "root": noThread, "dina": noThread, "aria": noThread,
				"nomad": noChannel, "nora": noChannel, "": unauthn,
			},
			granted: []string{"arlo"},
			leaves:  "wes",
		},
	}
	for _, r := range roots {
		t.Run(r.name, func(t *testing.T) {
			f := newScopedFixture(t)
			rootID := r.post(f)
			var replies []int64
			tryAll := func(label string, who []string) {
				for _, w := range who {
					before := f.messageCount(t)
					res := f.postReply(t, w, rootID)
					want := r.expect[w]
					if res.status != want.status || (want.code != "" && errorCode(t, res) != want.code) {
						t.Errorf("%s: %q replies to the %s root: %d %s, want %d %s", label, w, r.name, res.status, res.body, want.status, want.code)
						continue
					}
					if want == replied {
						var resp schema.PostMessageResponseV2
						if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
							t.Fatal(err)
						}
						if err := resp.Message.Validate(); err != nil || resp.Message.ThreadRootID != rootID {
							t.Errorf("%s: reply = %+v (%v), want a valid message with thread_root_id %d", w, resp.Message, err, rootID)
						}
						replies = append(replies, resp.Message.ID)
					} else if f.messageCount(t) != before {
						t.Errorf("%s: a refused reply by %q stored a message", label, w)
					}
				}
			}
			tryAll("first pass", everyone)

			// The agents refused for want of a grant succeed once it is given.
			if len(r.granted) > 0 {
				for _, w := range r.granted {
					f.manifest(t, w, nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost,
						schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
					r.expect[w] = replied
				}
				tryAll("after the grant", r.granted)
			}

			// Readers: the channel listing and the thread listing of every person.
			for _, who := range everyone {
				if who == "" {
					continue
				}
				chRes := f.channelList(t, who)
				thRes := f.threadList(t, who, rootID, "&limit=100")
				if chRes.status != http.StatusOK {
					// Whoever cannot list the channel cannot read a thread of it either, for the same reason.
					if thRes.status != chRes.status || errorCode(t, thRes) != errorCode(t, chRes) {
						t.Errorf("%s: channel listing %d %s but thread listing %d %s", who, chRes.status, chRes.body, thRes.status, thRes.body)
					}
					continue
				}
				channel := decodeMessages(t, chRes.body)
				seen := map[int64]bool{}
				var narrowed []int64
				for _, m := range channel.Messages {
					seen[m.ID] = true
					if m.ID == rootID || m.ThreadRootID == rootID {
						narrowed = append(narrowed, m.ID)
					}
				}
				// Whoever sees the root sees every reply and nobody else sees one.
				for _, id := range replies {
					if seen[id] != seen[rootID] {
						t.Errorf("%s: sees the root = %v, sees reply %d = %v", who, seen[rootID], id, seen[id])
					}
				}
				if !seen[rootID] {
					if thRes.status != http.StatusNotFound || errorCode(t, thRes) != "thread_not_found" {
						t.Errorf("%s cannot see the root, yet the thread listing is %d %s", who, thRes.status, thRes.body)
					}
					continue
				}
				if thRes.status != http.StatusOK {
					t.Errorf("%s sees the root, yet the thread listing is %d %s", who, thRes.status, thRes.body)
					continue
				}
				thread := decodeMessages(t, thRes.body)
				var got []int64
				for _, m := range thread.Messages {
					got = append(got, m.ID)
					if err := m.Validate(); err != nil {
						t.Errorf("%s: thread holds an invalid message: %v", who, err)
					}
				}
				if want := append([]int64{rootID}, replies...); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, narrowed) {
					t.Errorf("%s: thread = %v, channel listing narrowed = %v, want root then replies %v", who, got, narrowed, want)
				}
				if thread.Messages[0].ThreadRootID != 0 {
					t.Errorf("%s: the root is marked as a reply", who)
				}
			}

			// A member removed after the root: refused as any non-member, and the next
			// reply no longer reaches them.
			if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.p(r.leaves).ID); err != nil {
				t.Fatal(err)
			}
			if res := f.postReply(t, r.leaves, rootID); res.status != http.StatusNotFound || errorCode(t, res) != "channel_not_found" {
				t.Errorf("%s was removed from the channel and replies: %d %s", r.leaves, res.status, res.body)
			}
			if res := f.threadList(t, r.leaves, rootID, ""); res.status != http.StatusNotFound || errorCode(t, res) != "channel_not_found" {
				t.Errorf("%s was removed from the channel and reads the thread: %d %s", r.leaves, res.status, res.body)
			}
			res := f.postReply(t, "ann", rootID)
			if res.status != http.StatusCreated {
				t.Fatalf("ann replies after %s left: %d %s", r.leaves, res.status, res.body)
			}
			var resp schema.PostMessageResponseV2
			if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
				t.Fatal(err)
			}
			if a := resp.Message.Audience; r.name == "whisper" && (a == nil || contains64(a.PrincipalIDs, f.p(r.leaves).ID)) {
				t.Errorf("the whisper reply still lists the member who left: %+v", a)
			}
		})
	}
}

func contains64(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// rawResponse is everything a client can tell about an answer.
type rawResponse struct {
	status int
	header http.Header
	body   string
}

func (f *scopedFixture) raw(t *testing.T, method, path, token, body string) rawResponse {
	t.Helper()
	rec := f.do(t, method, path, token, body)
	return rawResponse{rec.Code, rec.Header().Clone(), rec.Body.String()}
}

// TestThreadAttemptsAreIndistinguishableFromAbsence: message ids are
// sequential, so for a caller outside a scoped root's audience the whole
// response (status, every header, body) to replying to it, or to reading its
// thread, is the response for an id that does not exist, for a root in another
// channel, and for a root that is itself a reply. Nothing is stored either way.
func TestThreadAttemptsAreIndistinguishableFromAbsence(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	other, err := f.srv.store.CreateChannel(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"olga", "wes", "nina", "mona", "ann", "root"} {
		if _, err := f.srv.store.AddChannelMember(ctx, "system", other.ID, f.p(who).ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere, err := f.srv.store.InsertMessage(ctx, other.ID, f.p("ann").ID, "in another channel")
	if err != nil {
		t.Fatal(err)
	}
	wide := f.mustPost(t, "ann", `{"body":"open"}`)
	aReply := f.mustReply(t, "ann", wide)
	netRoot := f.mustPost(t, "ann", fmt.Sprintf(`{"body":"net secret",%s}`, netAudience(f.net1.ID)))
	whisperRoot := f.mustPost(t, "ann", fmt.Sprintf(`{"body":"whisper secret",%s}`, whisperAudience(f.p("wes").ID)))
	f.mustReply(t, "nina", netRoot)
	f.mustReply(t, "wes", whisperRoot)

	cases := []struct {
		kind      string
		root      int64
		outsiders []string
	}{
		// arlo is an agent who may post but not read, so its read is a 403 of its
		// own whatever it asks for; it is probed for the reply only, below.
		{"net", netRoot, []string{"wes", "olga", "root"}},
		{"whisper", whisperRoot, []string{"nina", "mona", "olga", "root", "aria"}},
	}
	// arlo (an agent who may post and not read) replying to a net it is not on.
	arloReply := f.raw(t, "POST", "/v2/channels/ops/messages", f.p("arlo").token, f.replyBody(netRoot, "probe"))
	if want := f.raw(t, "POST", "/v2/channels/ops/messages", f.p("arlo").token, f.replyBody(99999, "probe")); !reflect.DeepEqual(arloReply, want) || arloReply.status != 404 {
		t.Errorf("arlo's reply to the net root = %+v, to an unknown id = %+v", arloReply, want)
	}
	for _, c := range cases {
		for _, who := range c.outsiders {
			t.Run(c.kind+"/"+who, func(t *testing.T) {
				token := f.p(who).token
				variants := map[string]int64{
					"unknown":       99999,
					"other channel": elsewhere.ID,
					"a reply":       aReply,
				}
				before, audit := f.messageCount(t), f.auditCount(t, store.AuditMessageScoped)
				post := func(root int64) rawResponse {
					return f.raw(t, "POST", "/v2/channels/ops/messages", token, f.replyBody(root, "probe"))
				}
				get := func(root int64) rawResponse {
					return f.raw(t, "GET", fmt.Sprintf("/v2/channels/ops/messages?thread=%d", root), token, "")
				}
				for _, op := range []struct {
					name string
					do   func(int64) rawResponse
				}{{"reply", post}, {"read", get}} {
					// What an attempt writes to the audit log is part of what it
					// reveals to whoever can read the log, and a row written
					// for a hidden root and not for a missing one would be a
					// difference too: each attempt is measured.
					measured := func(root int64) (rawResponse, int) {
						rows := len(f.audit(t))
						res := op.do(root)
						return res, len(f.audit(t)) - rows
					}
					got, gotRows := measured(c.root)
					if got.status != http.StatusNotFound || !strings.Contains(got.body, `"thread_not_found"`) {
						t.Fatalf("%s to the %s root by %s = %d %s, want 404 thread_not_found", op.name, c.kind, who, got.status, got.body)
					}
					for name, id := range variants {
						want, wantRows := measured(id)
						if !reflect.DeepEqual(got, want) {
							t.Errorf("%s by %s: the %s root answers differently from %s\n scoped root: %+v\n %s: %+v", op.name, who, c.kind, name, got, name, want)
						}
						if gotRows != wantRows {
							t.Errorf("%s by %s: the %s root wrote %d audit row(s), %s wrote %d", op.name, who, c.kind, gotRows, name, wantRows)
						}
					}
				}
				if f.messageCount(t) != before || f.auditCount(t, store.AuditMessageScoped) != audit {
					t.Error("a refused probe stored something")
				}
			})
		}
	}
}

func (f *scopedFixture) mustReply(t *testing.T, who string, root int64) int64 {
	t.Helper()
	res := f.postReply(t, who, root)
	if res.status != http.StatusCreated {
		t.Fatalf("%s reply to %d = %d %s", who, root, res.status, res.body)
	}
	var resp schema.PostMessageResponseV2
	if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Message.ID
}

func TestReplyAndAudienceTogetherIsRefused(t *testing.T) {
	f := newScopedFixture(t)
	root := f.mustPost(t, "ann", `{"body":"root"}`)
	before := f.messageCount(t)
	for name, audience := range map[string]string{
		"net":     netAudience(f.net1.ID),
		"whisper": whisperAudience(f.p("wes").ID),
	} {
		for _, target := range []int64{root, 99999} {
			body := fmt.Sprintf(`{"body":"x","thread_root_id":%d,%s}`, target, audience)
			res := f.postV2(t, "ann", body)
			if res.status != http.StatusBadRequest || errorCode(t, res) != "thread_audience_conflict" {
				t.Errorf("%s audience with root %d = %d %s, want 400 thread_audience_conflict", name, target, res.status, res.body)
			}
		}
	}
	if f.messageCount(t) != before {
		t.Error("a refused post stored a message")
	}
}

func TestReplyRequestValidation(t *testing.T) {
	f := newScopedFixture(t)
	root := f.mustPost(t, "ann", `{"body":"root"}`)
	tests := []struct {
		name string
		body string
		want outcome
	}{
		{"negative root", `{"body":"x","thread_root_id":-3}`, outcome{400, "invalid_request"}},
		{"a string root", `{"body":"x","thread_root_id":"2"}`, outcome{400, "invalid_request"}},
		{"empty body", fmt.Sprintf(`{"body":"","thread_root_id":%d}`, root), outcome{400, "invalid_request"}},
		{"blank body", fmt.Sprintf(`{"body":"  ","thread_root_id":%d}`, root), outcome{400, "invalid_request"}},
		{"a zero root is a top-level message", `{"body":"x","thread_root_id":0}`, replied},
		{"a reply to a reply", "", noThread},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.body
			if tt.name == "a reply to a reply" {
				body = f.replyBody(f.mustReply(t, "wes", root), "x")
			}
			res := f.postV2(t, "ann", body)
			if res.status != tt.want.status || (tt.want.code != "" && errorCode(t, res) != tt.want.code) {
				t.Errorf("= %d %s, want %d %s", res.status, res.body, tt.want.status, tt.want.code)
			}
		})
	}
}

func TestScopedReplyIsAudited(t *testing.T) {
	f := newScopedFixture(t)
	root := f.mustPost(t, "ann", fmt.Sprintf(`{"body":"root",%s}`, netAudience(f.net1.ID)))
	scoped, posts := f.auditCount(t, store.AuditMessageScoped), f.auditCount(t, "message.post")
	id := f.mustReply(t, "nina", root)
	if got := f.auditCount(t, store.AuditMessageScoped); got != scoped+1 {
		t.Errorf("message_scoped rows %d -> %d, want one more", scoped, got)
	}
	if got := f.auditCount(t, "message.post"); got != posts+1 {
		t.Errorf("message.post rows %d -> %d, want one more", posts, got)
	}
	var detail string
	for _, e := range f.audit(t) {
		if e.Action == store.AuditMessageScoped && e.Subject == fmt.Sprintf("message:%d", id) {
			detail = e.Detail
		}
	}
	// net1's members and its monitor, ascending by principal id.
	var members []int64
	for _, who := range []string{"ann", "nina", "mona", "aria", "dina"} {
		members = append(members, f.p(who).ID)
	}
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	parts := make([]string, len(members))
	for i, id := range members {
		parts[i] = fmt.Sprint(id)
	}
	want := fmt.Sprintf("channel=%d audience=net net=%d recipients=%s", f.ops.ID, f.net1.ID, strings.Join(parts, ","))
	if detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
	// A channel-wide reply writes the audit row any channel-wide post writes.
	wide := f.mustPost(t, "ann", `{"body":"open"}`)
	scoped = f.auditCount(t, store.AuditMessageScoped)
	f.mustReply(t, "wes", wide)
	if got := f.auditCount(t, store.AuditMessageScoped); got != scoped {
		t.Errorf("a channel-wide reply wrote a message_scoped row")
	}
}

func TestThreadListingPagingAndParameters(t *testing.T) {
	f := newScopedFixture(t)
	root := f.mustPost(t, "ann", `{"body":"root"}`)
	f.mustPost(t, "ann", `{"body":"noise"}`)
	var want []int64
	want = append(want, root)
	for i := 0; i < 5; i++ {
		want = append(want, f.mustReply(t, "wes", root))
		f.mustPost(t, "ann", `{"body":"noise"}`)
	}
	var got []int64
	after := int64(0)
	for page := 0; page < 10; page++ {
		res := f.threadList(t, "olga", root, fmt.Sprintf("&limit=2&after=%d", after))
		if res.status != http.StatusOK {
			t.Fatalf("page %d = %d %s", page, res.status, res.body)
		}
		resp := decodeMessages(t, res.body)
		if len(resp.Messages) > 2 {
			t.Fatalf("page %d holds %d messages", page, len(resp.Messages))
		}
		for _, m := range resp.Messages {
			got = append(got, m.ID)
		}
		if resp.NextAfter == 0 {
			break
		}
		after = resp.NextAfter
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged thread = %v, want %v", got, want)
	}

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"non-numeric", "thread=abc", 400},
		{"zero", "thread=0", 400},
		{"negative", "thread=-4", 400},
		{"empty", "thread=", 400},
		{"too large for an int", "thread=99999999999999999999", 400},
		{"a bad limit", fmt.Sprintf("thread=%d&limit=0", root), 400},
		{"a bad after", fmt.Sprintf("thread=%d&after=-1", root), 400},
		{"unknown", "thread=424242", 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.callREST(t, "GET", "/v2/channels/ops/messages?"+tt.query, f.p("olga").token, "")
			if res.status != tt.want {
				t.Errorf("= %d %s, want %d", res.status, res.body, tt.want)
			}
		})
	}

	// v0 and v1 have no threads: the parameter changes nothing there.
	for _, v := range []string{"v0", "v1"} {
		plain := f.callREST(t, "GET", "/"+v+"/channels/ops/messages?limit=100", f.p("olga").token, "")
		with := f.callREST(t, "GET", fmt.Sprintf("/%s/channels/ops/messages?limit=100&thread=%d", v, root), f.p("olga").token, "")
		if plain != with {
			t.Errorf("%s: the thread parameter changed the listing", v)
		}
	}
	// A reply to a channel-wide root is an ordinary message to a v1 reader.
	v1 := f.callREST(t, "GET", "/v1/channels/ops/messages?limit=100", f.p("olga").token, "")
	if strings.Contains(v1.body, "thread") {
		t.Errorf("a v1 listing mentions a thread: %s", v1.body)
	}
	if ids, _ := listIDs(t, v1.body); !contains64(ids, want[1]) {
		t.Errorf("a v1 listing %v lacks the channel-wide reply %d", ids, want[1])
	}
}

// With authentication off there is no verified reader, so only a channel-wide
// root can be answered or read, as with every other read.
func TestThreadsWithAuthenticationOff(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()
	ch, err := srv.store.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	author, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "nick")
	if err != nil {
		t.Fatal(err)
	}
	other, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "sam")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []store.Principal{author, other} {
		if _, err := srv.store.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	wide, err := srv.store.InsertMessage(ctx, ch.ID, author.ID, "open")
	if err != nil {
		t.Fatal(err)
	}
	whisper, _, err := srv.store.InsertScopedMessage(ctx, store.ScopedPost{ChannelID: ch.ID, AuthorID: author.ID, Body: "secret",
		Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{other.ID}}.Normalize(author.ID)})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	post := func(root int64) (int, string) {
		return do("POST", "/v2/channels/general/messages", fmt.Sprintf(`{"author_id":%d,"body":"re","thread_root_id":%d}`, other.ID, root))
	}
	if code, body := post(wide.ID); code != http.StatusCreated || !strings.Contains(body, fmt.Sprintf(`"thread_root_id":%d`, wide.ID)) {
		t.Errorf("reply to a channel-wide root = %d %s", code, body)
	}
	missing, missingBody := post(99999)
	if code, body := post(whisper.ID); code != missing || body != missingBody || code != http.StatusNotFound {
		t.Errorf("reply to a whisper = %d %s, want what an unknown id gets (%d %s)", code, body, missing, missingBody)
	}
	code, body := do("GET", fmt.Sprintf("/v2/channels/general/messages?thread=%d", wide.ID), "")
	if code != http.StatusOK || strings.Count(body, `"id"`) != 2 {
		t.Errorf("thread of the channel-wide root = %d %s", code, body)
	}
	if code, body := do("GET", fmt.Sprintf("/v2/channels/general/messages?thread=%d", whisper.ID), ""); code != http.StatusNotFound || strings.Contains(body, "secret") {
		t.Errorf("thread of a whisper = %d %s", code, body)
	}
}

// TestRepliesOnTheSockets: a reply reaches exactly its audience, and carries
// thread_root_id on v2; on v0 and v1 a channel-wide reply is an ordinary
// message and a scoped one does not arrive.
func TestRepliesOnTheSockets(t *testing.T) {
	f := newScopedFixture(t)
	type sockets struct{ v0, v1, v2 *socket }
	open := map[string]sockets{}
	for _, who := range []string{"ann", "wes", "nina", "olga"} {
		tok := f.p(who).token
		open[who] = sockets{
			v0: openSocket(t, f.base, "/v0/ws?channel=ops", tok),
			v1: openSocket(t, f.base, "/v1/ws?channel=ops", tok),
			v2: openSocket(t, f.base, "/v2/ws?channel=ops", tok),
		}
		for _, s := range []*socket{open[who].v0, open[who].v1, open[who].v2} {
			if s.status != http.StatusSwitchingProtocols {
				t.Fatalf("%s could not connect: %d %s", who, s.status, s.body)
			}
		}
	}
	wideRoot := f.mustPost(t, "olga", `{"body":"wide root"}`)
	netRoot := f.mustPost(t, "ann", fmt.Sprintf(`{"body":"net root",%s}`, netAudience(f.net1.ID)))
	whisperRoot := f.mustPost(t, "ann", fmt.Sprintf(`{"body":"whisper root",%s}`, whisperAudience(f.p("wes").ID)))
	wideReply := f.mustReply(t, "wes", wideRoot)
	netReply := f.mustReply(t, "nina", netRoot)
	whisperReply := f.mustReply(t, "wes", whisperRoot)
	sentinel := f.mustPost(t, "olga", `{"body":"end"}`)

	v2 := map[string][]int64{
		"ann":  {wideRoot, netRoot, whisperRoot, wideReply, netReply, whisperReply, sentinel},
		"wes":  {wideRoot, whisperRoot, wideReply, whisperReply, sentinel},
		"nina": {wideRoot, netRoot, wideReply, netReply, sentinel},
		"olga": {wideRoot, wideReply, sentinel},
	}
	wide := []int64{wideRoot, wideReply, sentinel}
	roots := map[int64]int64{wideReply: wideRoot, netReply: netRoot, whisperReply: whisperRoot}
	for who, s := range open {
		frames := s.v2.readUntil(t, sentinel)
		if got := idsOf(t, frames); !reflect.DeepEqual(got, v2[who]) {
			t.Errorf("%s v2 frames = %v, want %v", who, got, v2[who])
		}
		for _, raw := range frames {
			var m schema.MessageV2
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if want := roots[m.ID]; m.ThreadRootID != want {
				t.Errorf("%s v2 frame %d has thread_root_id %d, want %d", who, m.ID, m.ThreadRootID, want)
			}
		}
		for version, sock := range map[string]*socket{"v0": s.v0, "v1": s.v1} {
			frames := sock.readUntil(t, sentinel)
			if got := idsOf(t, frames); !reflect.DeepEqual(got, wide) {
				t.Errorf("%s %s frames = %v, want the channel-wide messages %v", who, version, got, wide)
			}
			for _, raw := range frames {
				if strings.Contains(string(raw), "thread") || strings.Contains(string(raw), "audience") {
					t.Errorf("%s %s frame carries a v2 field: %s", who, version, raw)
				}
			}
		}
	}
	for _, id := range f.rec.seen() {
		if id == netReply || id == whisperReply {
			t.Errorf("a scoped reply reached the broadcaster seam")
		}
	}
	if !contains64(f.rec.seen(), wideReply) {
		t.Errorf("the channel-wide reply did not reach the broadcaster seam")
	}
}

// Replying is an agent's post, and needs the grants a post to that audience
// needs: the channel permission "post" does not stand in for post_net or
// whisper.
func TestAgentRepliesNeedTheGrantsOfTheirRoot(t *testing.T) {
	f := newScopedFixture(t)
	wide := f.mustPost(t, "ann", `{"body":"open"}`)
	net := f.mustPost(t, "ann", fmt.Sprintf(`{"body":"net",%s}`, netAudience(f.net1.ID)))
	f.manifest(t, "aria", []schema.Capability{schema.CapabilityMessagesRead}, schema.ChannelPermissionRead, schema.ChannelPermissionPost)
	for name, root := range map[string]int64{"channel-wide": wide, "net": net} {
		if res := f.postReply(t, "aria", root); res.status != http.StatusForbidden {
			t.Errorf("%s: an agent without the messages.post capability replied: %d %s", name, res.status, res.body)
		}
	}
}
