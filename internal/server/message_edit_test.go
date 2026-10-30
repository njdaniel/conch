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

	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Message editing over REST, the sockets and MCP (issue #845,
// docs/design/message-editing.md §14.1), on the scoped fixture: channel "ops"
// with people on nets, a monitor, agents and a stranger.

// editWorld is the scoped fixture with three of ann's messages and an edit
// limit no test reaches by accident.
type editWorld struct {
	*scopedFixture
	wide, net, whisper int64 // ann's post, ann's post to net1, ann's whisper to wes
	// offset moves the server's clock, in nanoseconds; tests set it between
	// requests to step past the window.
	offset atomic.Int64
}

func newEditWorld(t *testing.T) *editWorld {
	t.Helper()
	f := newScopedFixture(t)
	w := &editWorld{scopedFixture: f}
	f.srv.now = func() time.Time { return time.Now().Add(time.Duration(w.offset.Load())) }
	f.srv.setMessageEditLimit(inviteLimit{name: "test", burst: 1000, window: time.Minute})
	w.wide = f.mustPost(t, "ann", `{"body":"SECRET-wide"}`)
	w.net = f.mustPost(t, "ann", `{"body":"SECRET-net",`+netAudience(f.net1.ID)+`}`)
	w.whisper = f.mustPost(t, "ann", `{"body":"SECRET-whisper",`+whisperAudience(f.p("wes").ID)+`}`)
	return w
}

func epath(channel string, msg any) string {
	return fmt.Sprintf("/v2/channels/%s/messages/%v/body", channel, msg)
}

func revpath(channel string, msg any) string {
	return fmt.Sprintf("/v2/channels/%s/messages/%v/revisions", channel, msg)
}

func editBody(text string) string {
	b, _ := json.Marshal(schema.EditMessageRequestV1{Body: text})
	return string(b)
}

func (w *editWorld) edit(t *testing.T, who, channel string, msg any, raw string) wireResult {
	t.Helper()
	return w.callREST(t, "PUT", epath(channel, msg), w.p(who).token, raw)
}

func (w *editWorld) mustEdit(t *testing.T, who string, msg int64, text string) schema.MessageV2 {
	t.Helper()
	res := w.edit(t, who, "ops", msg, editBody(text))
	if res.status != http.StatusOK {
		t.Fatalf("%s edit %d = %d %s", who, msg, res.status, res.body)
	}
	var out schema.EditMessageResponseV1
	if err := json.Unmarshal([]byte(res.body), &out); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("edit response %s is invalid: %v", res.body, err)
	}
	return out.Message
}

// visibleAs reads message id the way who's v2 list does, failing when it is
// not there.
func (w *editWorld) visibleAs(t *testing.T, who string, id int64) schema.MessageV2 {
	t.Helper()
	res := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages?after=%d&limit=1", id-1), w.p(who).token, "")
	var list schema.ListMessagesResponseV2
	if err := json.Unmarshal([]byte(res.body), &list); err != nil || len(list.Messages) != 1 || list.Messages[0].ID != id {
		t.Fatalf("%s cannot read message %d: %d %s", who, id, res.status, res.body)
	}
	return list.Messages[0]
}

// Every refusal of §4.3, in the order of §4.2: the earlier check decides when
// two would refuse.
func TestMessageEditCheckOrder(t *testing.T) {
	w := newEditWorld(t)
	typed := w.mustPost(t, "ann", `{"body":"BUY 10","payload":{"schema":"leviathan.trade_signal.v1","data":{"side":"buy"}}}`)
	ariaOwn := w.mustPost(t, "aria", `{"body":"an agent's own message"}`)
	limit := schema.MaxPostRequestBytes
	atBound := `{"body":"` + strings.Repeat("a", limit-len(`{"body":""}`)) + `"}`
	overBound := `{"body":"` + strings.Repeat("a", limit+1-len(`{"body":""}`)) + `"}`
	if len(atBound) != limit || len(overBound) != limit+1 {
		t.Fatalf("bound bodies are %d and %d bytes", len(atBound), len(overBound))
	}
	for _, tt := range []struct {
		name, who, channel string
		msg                any
		body               string
		status             int
		code               string
	}{
		{"an agent on a ghost channel", "aria", "nosuch", 1, `{bad`, 403, schema.ErrorCodeHumanRequired},
		{"an agent outside the channel", "nomad", "ops", w.wide, editBody("x"), 403, schema.ErrorCodeHumanRequired},
		{"an agent, its own message", "aria", "ops", ariaOwn, editBody("x"), 403, schema.ErrorCodeHumanRequired},
		{"a non-member with a bad body", "nora", "ops", "abc", `{bad`, 404, "channel_not_found"},
		{"a member, a ghost channel", "ann", "nosuch", w.wide, editBody("x"), 404, "channel_not_found"},
		{"a bad id with a bad body", "ann", "ops", "abc", `{bad`, 400, "invalid_request"},
		{"a zero id", "ann", "ops", 0, editBody("x"), 400, "invalid_request"},
		{"a negative id", "ann", "ops", -3, editBody("x"), 400, "invalid_request"},
		{"not JSON", "ann", "ops", w.wide, `nope`, 400, "invalid_request"},
		{"two objects", "ann", "ops", w.wide, editBody("x") + editBody("y"), 400, "invalid_request"},
		{"an unknown field", "ann", "ops", w.wide, `{"body":"x","payload":{"schema":"conch.file_attachment.v1","data":{"file_ids":[1]}}}`, 400, "invalid_request"},
		{"an audience", "ann", "ops", w.wide, `{"body":"x","audience":{"kind":"net","net_id":1}}`, 400, "invalid_request"},
		{"an empty body", "ann", "ops", w.wide, editBody(""), 400, "invalid_request"},
		{"white space", "ann", "ops", w.wide, editBody(" \n\t "), 400, "invalid_request"},
		{"over the request bound", "ann", "ops", w.wide, overBound, 413, "request_too_large"},
		{"a ghost message", "ann", "ops", 999999, editBody("x"), 404, "message_not_found"},
		{"someone else's message", "olga", "ops", w.wide, editBody("x"), 404, "message_not_found"},
		{"the operator, someone else's message", "root", "ops", w.wide, editBody("x"), 404, "message_not_found"},
		{"a typed payload", "ann", "ops", typed, editBody("SELL 10"), 409, schema.ErrorCodeMessageNotEditable},
		{"at the request bound", "ann", "ops", w.wide, atBound, 200, ""},
		{"an edit", "ann", "ops", w.wide, editBody("SECRET-wide, edited"), 200, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := w.edit(t, tt.who, tt.channel, tt.msg, tt.body)
			if res.status != tt.status || reactErrCode(res) != tt.code {
				t.Errorf("= %d %s, want %d %s", res.status, res.body, tt.status, tt.code)
			}
		})
	}
	// The window, by the server's clock.
	w.offset.Store(int64(time.Hour + time.Second))
	if res := w.edit(t, "ann", "ops", w.net, editBody("late")); res.status != 409 || reactErrCode(res) != schema.ErrorCodeMessageEditWindowClosed {
		t.Errorf("after the window = %d %s", res.status, res.body)
	}
	// The same text after the window is the no-op, not a refusal (§3.5).
	if res := w.edit(t, "ann", "ops", w.wide, editBody("SECRET-wide, edited")); res.status != 200 {
		t.Errorf("a repeated edit after the window = %d %s", res.status, res.body)
	}
	w.offset.Store(0)
	// The limit.
	for i := range schema.MaxMessageEdits {
		w.mustEdit(t, "ann", w.whisper, fmt.Sprintf("whisper v%d", i+2))
	}
	if res := w.edit(t, "ann", "ops", w.whisper, editBody("one too many")); res.status != 409 || reactErrCode(res) != schema.ErrorCodeMessageEditLimitReached {
		t.Errorf("edit %d = %d %s", schema.MaxMessageEdits+1, res.status, res.body)
	}
	// With authentication off there is no author.
	off := newAuthFixture(t, AuthOff)
	for _, r := range []struct{ method, path, body string }{
		{"PUT", epath("general", 1), editBody("x")},
		{"GET", revpath("general", 1), ""},
	} {
		rec := off.do(t, r.method, r.path, "", r.body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), schema.ErrorCodeMessageEditRequiresAuth) {
			t.Errorf("auth off %s = %d %s", r.method, rec.Code, rec.Body)
		}
	}
}

// A token is spent on every request past the channel gate, refusals included;
// a 429 spends none; the revision read spends none; buckets are per person.
func TestMessageEditRateLimit(t *testing.T) {
	w := newEditWorld(t)
	w.srv.setMessageEditLimit(inviteLimit{name: "test", burst: 3, window: time.Hour})
	for range 10 {
		if res := w.edit(t, "nora", "ops", w.wide, editBody("x")); res.status != 404 {
			t.Fatalf("stranger = %d", res.status)
		}
		if res := w.edit(t, "ann", "nosuch", w.wide, editBody("x")); res.status != 404 {
			t.Fatalf("unknown channel = %d", res.status)
		}
		if res := w.edit(t, "aria", "ops", w.wide, editBody("x")); res.status != 403 {
			t.Fatalf("agent = %d", res.status)
		}
	}
	for _, r := range []struct {
		msg  any
		body string
		code string
	}{
		{999999, editBody("x"), "message_not_found"},
		{"abc", editBody("x"), "invalid_request"},
		{w.wide, editBody(""), "invalid_request"},
	} {
		if res := w.edit(t, "ann", "ops", r.msg, r.body); reactErrCode(res) != r.code {
			t.Errorf("%v = %s, want %s", r.msg, res.body, r.code)
		}
	}
	rec := w.do(t, "PUT", epath("ops", w.wide), w.p("ann").token, editBody("allowed?"))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("the fourth request = %d %s (Retry-After %q), want 429", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	var e schema.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != schema.ErrorCodeRateLimited || e.RetryAfterSeconds < 1 {
		t.Errorf("429 body = %s", rec.Body)
	}
	if got := w.visibleAs(t, "olga", w.wide); got.Body != "SECRET-wide" || got.EditedAt != nil {
		t.Errorf("a limited edit changed the message: %+v", got)
	}
	if res := w.callREST(t, "GET", revpath("ops", w.wide), w.p("ann").token, ""); res.status != 200 {
		t.Errorf("the revision read while limited = %d %s", res.status, res.body)
	}
	if got := w.mustEdit(t, "nina", w.mustPost(t, "nina", `{"body":"nina's"}`), "nina's, edited"); got.EditedAt == nil {
		t.Errorf("another person's edit = %+v", got)
	}
}

// A stranger to the channel (a person, an operator who is not a member) gets
// exactly an unknown channel's answer on both routes; an agent gets the same
// human_required wherever it points; inside the channel, every message that
// is not the caller's own is exactly a ghost id's answer, the whisper they
// are not in and the message they can read alike (§4.3, §12).
func TestMessageEditChannelGate(t *testing.T) {
	w := newEditWorld(t)
	w.add(t, "op2", store.PrincipalHuman, false)
	if _, err := w.srv.store.SetRole(context.Background(), "system", w.root.ID, w.p("op2").ID, store.RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	type route struct{ method, body string }
	routes := []route{{"PUT", editBody("probe")}, {"GET", ""}}
	path := func(r route, ch string, msg any) string {
		if r.method == "PUT" {
			return epath(ch, msg)
		}
		return revpath(ch, msg)
	}
	auditBefore := len(w.audit(t))
	for _, r := range routes {
		for _, who := range []string{"nora", "op2"} {
			got := w.callREST(t, r.method, path(r, "ops", w.wide), w.p(who).token, r.body)
			want := w.callREST(t, r.method, path(r, "nosuch", w.wide), w.p(who).token, r.body)
			if got != want || got.status != 404 || reactErrCode(got) != "channel_not_found" {
				t.Errorf("%s %s: %+v, an unknown channel gives %+v", who, r.method, got, want)
			}
		}
		for _, who := range []string{"aria", "nomad", "arlo"} {
			got := w.callREST(t, r.method, path(r, "ops", w.whisper), w.p(who).token, r.body)
			for _, ghost := range []string{path(r, "nosuch", w.whisper), path(r, "ops", 999999)} {
				if want := w.callREST(t, r.method, ghost, w.p(who).token, r.body); got != want || got.status != 403 {
					t.Errorf("%s %s: %+v, a ghost gives %+v", who, r.method, got, want)
				}
			}
		}
		// Not the author: the message they can read, the whisper they are
		// not in, the net message they are off, the operator and owner who
		// is a member, and a recipient of the whisper. All one answer, a
		// ghost id's.
		for _, c := range []struct {
			who string
			msg int64
		}{
			{"olga", w.wide}, {"olga", w.whisper}, {"olga", w.net}, {"wes", w.net},
			{"root", w.wide}, {"root", w.whisper}, {"wes", w.whisper}, {"nina", w.net}, {"mona", w.net},
		} {
			got := w.callREST(t, r.method, path(r, "ops", c.msg), w.p(c.who).token, r.body)
			want := w.callREST(t, r.method, path(r, "ops", 999999), w.p(c.who).token, r.body)
			if got != want || got.status != 404 || reactErrCode(got) != "message_not_found" {
				t.Errorf("%s %s message %d: %+v, a ghost id gives %+v", c.who, r.method, c.msg, got, want)
			}
			if strings.Contains(got.body, "SECRET") {
				t.Errorf("%s's refusal holds the text: %s", c.who, got.body)
			}
		}
	}
	// Nothing changed and nothing was written.
	for _, id := range []int64{w.wide, w.net, w.whisper} {
		if got := w.visibleAs(t, "ann", id); got.EditedAt != nil || !strings.HasPrefix(got.Body, "SECRET-") {
			t.Errorf("message %d after the probes = %+v", id, got)
		}
	}
	for _, e := range w.audit(t)[auditBefore:] {
		if e.Action == store.AuditMessageEdited {
			t.Errorf("a refusal wrote %+v", e)
		}
	}
}

// For every reader of the fixture and every message, the revision read
// answers the author alone, and anyone else exactly what they would be told
// about a message that does not exist.
func TestMessageRevisionsExactSets(t *testing.T) {
	w := newEditWorld(t)
	olgas := w.mustPost(t, "olga", `{"body":"SECRET-olga"}`)
	ninas := w.mustPost(t, "nina", `{"body":"SECRET-nina-net2",`+netAudience(w.net2.ID)+`}`)
	w.mustEdit(t, "ann", w.whisper, "SECRET-whisper v2")
	authors := map[int64]string{w.wide: "ann", w.net: "ann", w.whisper: "ann", olgas: "olga", ninas: "nina"}
	for _, who := range []string{"root", "ann", "nina", "mona", "wes", "olga", "dina", "nora", "aria", "arlo", "nomad"} {
		for id, author := range authors {
			got := w.callREST(t, "GET", revpath("ops", id), w.p(who).token, "")
			if who == author {
				var doc schema.ListMessageRevisionsResponseV1
				if got.status != 200 || json.Unmarshal([]byte(got.body), &doc) != nil || doc.Validate() != nil ||
					doc.MessageID != id || doc.ChannelID != w.ops.ID {
					t.Errorf("%s's own message %d: %d %s", who, id, got.status, got.body)
				}
				if id == w.whisper && (len(doc.Revisions) != 2 || doc.Revisions[0].Body != "SECRET-whisper" || doc.Revisions[1].Body != "SECRET-whisper v2") {
					t.Errorf("the whisper's revisions = %+v", doc.Revisions)
				}
				if id != w.whisper && len(doc.Revisions) != 1 {
					t.Errorf("an unedited message's revisions = %+v", doc.Revisions)
				}
				continue
			}
			want := w.callREST(t, "GET", revpath("ops", 999999), w.p(who).token, "")
			if got != want || got.status == 200 || strings.Contains(got.body, "SECRET") {
				t.Errorf("%s reading %s's message %d: %+v, a ghost gives %+v", who, author, id, got, want)
			}
		}
	}
}

// What an edit does not touch: the attachments and their bytes, an approval,
// reactions, the thread, the audience and recipients, and read state (§14.1
// "Immutability").
func TestMessageEditImmutability(t *testing.T) {
	ff := newFileFixture(t)
	w := &editWorld{scopedFixture: ff.scopedFixture}
	w.srv.setMessageEditLimit(inviteLimit{name: "test", burst: 1000, window: time.Minute})

	// A whisper with a file, to wes.
	file := ff.mustUpload(t, "ann", "notes.txt", []byte("SECRET-bytes"))
	withFile := w.mustPost(t, "ann", attachBody("see the file", whisperAudience(w.p("wes").ID), file.ID))
	before := w.visibleAs(t, "wes", withFile)
	beforeMeta := ff.meta(t, "wes", file.ID)
	edited := w.mustEdit(t, "ann", withFile, "see the attached file")
	after := w.visibleAs(t, "wes", withFile)
	if string(after.Payload.Data) != string(before.Payload.Data) || after.Payload.Schema != before.Payload.Schema ||
		fmt.Sprint(after.Audience) != fmt.Sprint(before.Audience) || after.EditedAt == nil || after.Body != "see the attached file" {
		t.Errorf("the attachment message changed beyond its text:\n before %+v\n after  %+v", before, after)
	}
	if string(edited.Payload.Data) != string(before.Payload.Data) {
		t.Errorf("the edit's answer changed the payload: %s", edited.Payload.Data)
	}
	for _, who := range []string{"ann", "wes"} {
		if got := ff.fetch(t, who, file.ID); got.status != 200 || string(got.body) != "SECRET-bytes" {
			t.Errorf("%s fetches %s after the edit", who, got)
		}
	}
	if got := ff.meta(t, "wes", file.ID); string(got.body) != string(beforeMeta.body) {
		t.Errorf("file meta changed: %s -> %s", beforeMeta.body, got.body)
	}
	if got := ff.fetch(t, "olga", file.ID); got.status != 404 {
		t.Errorf("a stranger to the whisper fetches the file after the edit: %s", got)
	}

	// An approval request's message is not text: refused, and the approval
	// is as it was.
	rec := w.do(t, "POST", "/v1/approvals", w.p("ann").token, createApprovalBody(w.ops.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create approval = %d %s", rec.Code, rec.Body)
	}
	var created schema.CreateApprovalResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	approvalJSON, err := json.Marshal(created.Approval)
	if err != nil {
		t.Fatal(err)
	}
	approvalMsg := w.mustPost(t, "ann", fmt.Sprintf(`{"body":"approve?","payload":{"schema":%q,"data":%s}}`, schema.ApprovalV1Name, approvalJSON))
	approvalPath := fmt.Sprintf("/v1/approvals/%d", created.Approval.ID)
	approvalBefore := w.callREST(t, "GET", approvalPath, w.p("ann").token, "")
	if res := w.edit(t, "ann", "ops", approvalMsg, editBody("approved already")); res.status != 409 || reactErrCode(res) != schema.ErrorCodeMessageNotEditable {
		t.Errorf("editing an approval message = %d %s", res.status, res.body)
	}
	if got := w.callREST(t, "GET", approvalPath, w.p("ann").token, ""); got != approvalBefore {
		t.Errorf("the approval changed: %+v -> %+v", approvalBefore, got)
	}
	if got := w.visibleAs(t, "olga", approvalMsg); got.Body != "approve?" || got.EditedAt != nil {
		t.Errorf("the approval message changed: %+v", got)
	}

	// Reactions, a thread, and read state.
	root := w.mustPost(t, "ann", `{"body":"thread root"}`)
	reply := w.mustPost(t, "olga", fmt.Sprintf(`{"body":"a reply","thread_root_id":%d}`, root))
	annReply := w.mustPost(t, "ann", fmt.Sprintf(`{"body":"ann's reply","thread_root_id":%d}`, root))
	if res := w.callREST(t, "PUT", rpath("ops", root, thumbsUp), w.p("olga").token, ""); res.status != 200 {
		t.Fatalf("react = %d %s", res.status, res.body)
	}
	if res := w.callREST(t, "PUT", "/v1/channels/ops/read-state", w.p("olga").token, fmt.Sprintf(`{"last_read_id":%d}`, root)); res.status != 200 {
		t.Fatalf("mark = %d %s", res.status, res.body)
	}
	reactionsBefore := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", root), w.p("olga").token, "")
	readStateBefore := w.callREST(t, "GET", "/v1/read-state", w.p("olga").token, "")
	threadIDs := func() string {
		res := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages?thread=%d", root), w.p("olga").token, "")
		ids, _ := listIDs(t, res.body)
		return fmt.Sprint(ids)
	}
	threadBefore := threadIDs()
	w.mustEdit(t, "ann", root, "thread root, edited")
	w.mustEdit(t, "ann", annReply, "ann's reply, edited")
	if got := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", root), w.p("olga").token, ""); !sameExceptAsOf(got.body, reactionsBefore.body) {
		t.Errorf("reactions changed: %s -> %s", reactionsBefore.body, got.body)
	}
	if got := w.callREST(t, "GET", "/v1/read-state", w.p("olga").token, ""); got != readStateBefore {
		t.Errorf("read state changed: %s -> %s", readStateBefore.body, got.body)
	}
	if got := threadIDs(); got != threadBefore {
		t.Errorf("the thread changed: %s -> %s", threadBefore, got)
	}
	if got := w.visibleAs(t, "olga", annReply); got.ThreadRootID != root {
		t.Errorf("the edited reply lost its root: %+v", got)
	}
	if got := w.visibleAs(t, "olga", reply); got.ThreadRootID != root || got.EditedAt != nil {
		t.Errorf("another's reply changed: %+v", got)
	}
}

// sameExceptAsOf compares two reaction summaries but for their as_of.
func sameExceptAsOf(a, b string) bool {
	var x, y schema.ReactionSummaryV1
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	x.AsOf, y.AsOf = schema.Timestamp{}, schema.Timestamp{}
	return fmt.Sprint(x) == fmt.Sprint(y)
}

// frameFeed collects every frame of a socket in the background, so a test
// can take what arrived in a while without a read blocking it.
type frameFeed struct {
	mu     sync.Mutex
	frames []json.RawMessage
}

func feedOf(t *testing.T, s *socket) *frameFeed {
	t.Helper()
	if s.status != http.StatusSwitchingProtocols {
		t.Fatalf("socket = %d %s", s.status, s.body)
	}
	f := &frameFeed{}
	go func() {
		for {
			var raw json.RawMessage
			if err := wsjson.Read(context.Background(), s.conn, &raw); err != nil {
				return
			}
			f.mu.Lock()
			f.frames = append(f.frames, raw)
			f.mu.Unlock()
		}
	}()
	return f
}

// take waits d and returns the frames that arrived since the last take.
func (f *frameFeed) take(d time.Duration) []json.RawMessage {
	time.Sleep(d)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.frames
	f.frames = nil
	return out
}

type liveFrame struct {
	Schema   string            `json:"schema"`
	ID       int64             `json:"id"`
	Body     string            `json:"body"`
	EditedAt *schema.Timestamp `json:"edited_at"`
	Message  *schema.MessageV2 `json:"message"`
}

func decodeFrames(t *testing.T, raws []json.RawMessage) []liveFrame {
	t.Helper()
	out := make([]liveFrame, 0, len(raws))
	for _, raw := range raws {
		var f liveFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if f.Schema == schema.MessageEditedSchemaV1 {
			var doc schema.MessageEditedV1
			if err := json.Unmarshal(raw, &doc); err != nil || doc.Validate() != nil {
				t.Fatalf("an invalid message_edited frame %s", raw)
			}
		}
		out = append(out, f)
	}
	return out
}

// edits are the message-socket frames for id that carry edited_at.
func edits(frames []liveFrame, id int64) []liveFrame {
	var out []liveFrame
	for _, f := range frames {
		if f.ID == id && f.EditedAt != nil {
			out = append(out, f)
		}
	}
	return out
}

// editedEvents are the events-socket frames for message id.
func editedEvents(frames []liveFrame, id int64) []liveFrame {
	var out []liveFrame
	for _, f := range frames {
		if f.Schema == schema.MessageEditedSchemaV1 && f.Message != nil && f.Message.ID == id {
			out = append(out, f)
		}
	}
	return out
}

// The live half (§7, §12 live rows): the edited MessageV2 reaches exactly the
// /v2/ws subscribers the post reached, once each; nothing reaches v0, v1 or
// the Broadcaster; the events socket sends conch.message_edited.v1 to the
// human members who can read it, the author's other socket included, and
// never to an agent; and an edit notifies no one and moves no count.
func TestMessageEditLive(t *testing.T) {
	f := newScopedFixture(t)
	f.srv.setMessageEditLimit(inviteLimit{name: "test", burst: 1000, window: time.Minute})
	w := &editWorld{scopedFixture: f}
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet)
	people := []string{"ann", "nina", "mona", "wes", "olga", "dina", "aria"}
	v2 := map[string]*frameFeed{}
	ev := map[string]*frameFeed{}
	for _, who := range people {
		v2[who] = feedOf(t, openSocket(t, f.base, "/v2/ws?channel=ops", f.p(who).token))
		ev[who] = feedOf(t, openSocket(t, f.base, "/v1/events/ws?channel=ops", f.p(who).token))
	}
	annOther := feedOf(t, openSocket(t, f.base, "/v1/events/ws?channel=ops", f.p("ann").token))
	v0 := feedOf(t, openSocket(t, f.base, "/v0/ws?channel=ops", f.p("olga").token))
	v1 := feedOf(t, openSocket(t, f.base, "/v1/ws?channel=ops", f.p("olga").token))
	time.Sleep(300 * time.Millisecond) // the first rosters and read states

	wide := f.mustPost(t, "ann", `{"body":"SECRET-wide"}`)
	net := f.mustPost(t, "ann", `{"body":"SECRET-net",`+netAudience(f.net1.ID)+`}`)
	whisper := f.mustPost(t, "ann", `{"body":"SECRET-whisper",`+whisperAudience(f.p("wes").ID)+`}`)
	for _, feed := range append([]*frameFeed{annOther, v0, v1}, mapValues(v2, ev)...) {
		feed.take(0)
	}
	time.Sleep(300 * time.Millisecond)
	for _, feed := range append([]*frameFeed{annOther, v0, v1}, mapValues(v2, ev)...) {
		feed.take(0)
	}
	readBefore := map[string]string{}
	for _, who := range people[:6] {
		readBefore[who] = f.callREST(t, "GET", "/v1/read-state", f.p(who).token, "").body
	}
	tapBefore := len(f.rec.seen())

	w.mustEdit(t, "ann", whisper, "SECRET-whisper, edited")
	w.mustEdit(t, "ann", net, "SECRET-net, edited")
	w.mustEdit(t, "ann", wide, "SECRET-wide, edited")
	sentinel := f.mustPost(t, "root", `{"body":"sentinel"}`)

	got := map[string][]liveFrame{}
	gotEv := map[string][]liveFrame{}
	for _, who := range people {
		got[who] = decodeFrames(t, v2[who].take(0))
	}
	time.Sleep(700 * time.Millisecond)
	for _, who := range people {
		got[who] = append(got[who], decodeFrames(t, v2[who].take(0))...)
		gotEv[who] = decodeFrames(t, ev[who].take(0))
	}
	// Who received the whisper and the net message (the post's recipients:
	// the net's roster, a monitor and the agent included).
	reached := map[int64][]string{
		whisper: {"ann", "wes"},
		net:     {"ann", "nina", "mona", "dina", "aria"},
		wide:    people,
	}
	for id, to := range reached {
		for _, who := range people {
			want := 0
			for _, r := range to {
				if r == who {
					want = 1
				}
			}
			e := edits(got[who], id)
			if len(e) != want {
				t.Errorf("%s's /v2/ws got %d edit frames of message %d, want %d: %+v", who, len(e), id, want, e)
			}
			if want == 1 && len(e) == 1 && !strings.HasSuffix(e[0].Body, ", edited") {
				t.Errorf("%s's edit frame of %d = %+v", who, id, e[0])
			}
		}
	}
	// v0 and v1 get nothing: the sentinel only.
	for name, feed := range map[string]*frameFeed{"v0": v0, "v1": v1} {
		frames := decodeFrames(t, feed.take(0))
		for _, fr := range frames {
			if fr.ID != sentinel {
				t.Errorf("the %s socket got %+v", name, fr)
			}
		}
	}
	if len(f.rec.seen()) != tapBefore+1 {
		t.Errorf("the Broadcaster saw %v; only the sentinel was a new message", f.rec.seen()[tapBefore:])
	}
	// The events socket: humans who can read it, the author's other page
	// included; never an agent.
	evReached := map[int64][]string{
		whisper: {"ann", "wes"},
		net:     {"ann", "nina", "mona", "dina"},
		wide:    {"ann", "nina", "mona", "wes", "olga", "dina"},
	}
	annOtherFrames := decodeFrames(t, annOther.take(0))
	for id, to := range evReached {
		for _, who := range people {
			want := false
			for _, r := range to {
				want = want || r == who
			}
			e := editedEvents(gotEv[who], id)
			if want != (len(e) == 1) || len(e) > 1 {
				t.Errorf("%s's events socket got %d message_edited frames of %d, want %v", who, len(e), id, want)
			}
		}
		if len(editedEvents(annOtherFrames, id)) != 1 {
			t.Errorf("the author's other page did not get the edit of %d", id)
		}
	}
	for _, who := range people {
		for _, fr := range gotEv[who] {
			if fr.Schema == schema.MessageSchemaV2 && fr.ID != sentinel {
				t.Errorf("%s's events socket took an edit for a new message: %+v", who, fr)
			}
		}
	}
	// No read state moved: the sentinel is the one new unread message.
	for _, who := range people[:6] {
		var before, after schema.ListReadStateResponseV1
		_ = json.Unmarshal([]byte(readBefore[who]), &before)
		_ = json.Unmarshal([]byte(f.callREST(t, "GET", "/v1/read-state", f.p(who).token, "").body), &after)
		if len(before.ReadState) != 1 || len(after.ReadState) != 1 || after.ReadState[0].Unread != before.ReadState[0].Unread+1 || after.ReadState[0].LastReadID != before.ReadState[0].LastReadID {
			t.Errorf("%s's read state %s -> %+v", who, readBefore[who], after)
		}
	}
}

func mapValues(ms ...map[string]*frameFeed) []*frameFeed {
	var out []*frameFeed
	for _, m := range ms {
		for _, v := range m {
			out = append(out, v)
		}
	}
	return out
}

// A guest held to a history bound gets no frame for an edit of a message from
// before the bound, on either socket, and the edit's own history is out of
// her reach (§7.1).
func TestMessageEditGuestHistoryBound(t *testing.T) {
	f := newScopedFixture(t)
	f.srv.setMessageEditLimit(inviteLimit{name: "test", burst: 1000, window: time.Minute})
	w := &editWorld{scopedFixture: f}
	old := f.mustPost(t, "ann", `{"body":"SECRET-before the guest"}`)
	guest := admitGuest(t, f)
	gv2 := feedOf(t, openSocket(t, f.base, "/v2/ws?channel=ops", guest.token))
	gev := feedOf(t, openSocket(t, f.base, "/v1/events/ws?channel=ops", guest.token))
	time.Sleep(300 * time.Millisecond)
	gev.take(0)
	w.mustEdit(t, "ann", old, "SECRET-before the guest, edited")
	newer := f.mustPost(t, "ann", `{"body":"after the guest"}`)
	w.mustEdit(t, "ann", newer, "after the guest, edited")
	time.Sleep(600 * time.Millisecond)
	frames := decodeFrames(t, gv2.take(0))
	if len(edits(frames, old)) != 0 || len(edits(frames, newer)) != 1 {
		t.Errorf("the guest's /v2/ws got %+v", frames)
	}
	evs := decodeFrames(t, gev.take(0))
	if len(editedEvents(evs, old)) != 0 || len(editedEvents(evs, newer)) != 1 {
		t.Errorf("the guest's events socket got %+v", evs)
	}
	for _, raw := range gv2.take(0) {
		if strings.Contains(string(raw), "SECRET") {
			t.Errorf("the guest's socket carried %s", raw)
		}
	}
}

// guestPerson admits a guest to ops and returns her credential.
type guestPerson struct {
	id    int64
	token string
}

func admitGuest(t *testing.T, f *scopedFixture) guestPerson {
	t.Helper()
	ctx := context.Background()
	g, err := f.srv.store.CreateGuest(ctx, "system", store.NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.ops.ID}, Idle: time.Hour, Absolute: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return guestPerson{id: g.Principal.ID, token: g.Token}
}

// A burst of edits to one message coalesces on a subscriber that is not
// keeping up (§7.2): olga's socket is held in its write of the first edit's
// frame while four more edits land; their references are one in her queue, so
// she gets two frames in all, the second with the last text.
func TestMessageEditEventsCoalesce(t *testing.T) {
	f := newScopedFixture(t)
	f.srv.setMessageEditLimit(inviteLimit{name: "test", burst: 1000, window: time.Minute})
	w := &editWorld{scopedFixture: f}
	msg := f.mustPost(t, "ann", `{"body":"burst"}`)
	olga := feedOf(t, openSocket(t, f.base, "/v1/events/ws?channel=ops", f.p("olga").token))
	time.Sleep(300 * time.Millisecond)
	olga.take(0)
	var armed atomic.Bool
	held := make(chan struct{})
	release := make(chan struct{})
	f.srv.eventsBeforeWrite = func() {
		if armed.CompareAndSwap(true, false) {
			close(held)
			<-release
		}
	}
	armed.Store(true)
	w.mustEdit(t, "ann", msg, "burst 0")
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscriber never wrote the first edit")
	}
	for i := 1; i < 5; i++ {
		w.mustEdit(t, "ann", msg, fmt.Sprintf("burst %d", i))
	}
	close(release)
	evs := editedEvents(decodeFrames(t, olga.take(700*time.Millisecond)), msg)
	if len(evs) != 2 || evs[1].Message.Body != "burst 4" {
		t.Errorf("after a burst the frames are %+v, want two, the last with the last text", evs)
	}
}

// The coalescing rule itself: two references to one edited message are one.
func TestMessageEditedReferencesCoalesce(t *testing.T) {
	h := newEventsHub()
	sub := h.subscribeKind(1, 2, 0, true)
	defer sub.cancel()
	h.mu.Lock()
	sub.memberships = map[int64]struct{}{1: {}}
	h.mu.Unlock()
	for range 3 {
		h.notifyMessageEdited(1, 42)
	}
	h.notifyMessageEdited(1, 43)
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, ref := range sub.refs {
		if ref.kind == eventMessageEdited {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d message_edited references queued for two messages, want 2: %+v", n, sub.refs)
	}
}

// read_channel shows edited_at, its published output schema has it, and no
// tool was added: agents cannot edit (§6.4).
func TestMessageEditMCP(t *testing.T) {
	w := newEditWorld(t)
	w.mustEdit(t, "ann", w.wide, "SECRET-wide, edited")
	out := w.mcpCall(t, w.p("aria").token, "read_channel", map[string]any{"channel": "ops"})
	if out.isError {
		t.Fatalf("read_channel = %s", out.body)
	}
	var resp struct {
		Result struct {
			Structured struct {
				Messages []struct {
					ID       int64  `json:"id"`
					Body     string `json:"body"`
					EditedAt string `json:"edited_at"`
				} `json:"messages"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.body), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range resp.Result.Structured.Messages {
		switch m.ID {
		case w.wide:
			found = true
			if m.Body != "SECRET-wide, edited" || m.EditedAt == "" {
				t.Errorf("read_channel's edited message = %+v", m)
			}
		default:
			if m.EditedAt != "" {
				t.Errorf("an unedited message carries edited_at: %+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("read_channel did not return the edited message: %s", out.body)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+w.p("aria").token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w.srv.Handler().ServeHTTP(rec, req)
	var list struct {
		Result struct {
			Tools []struct {
				Name         string `json:"name"`
				OutputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("tools/list: %v %s", err, rec.Body)
	}
	mapped := schema.MCPToolCapabilities()
	for _, tool := range list.Result.Tools {
		if strings.Contains(tool.Name, "edit") {
			t.Errorf("an edit tool is offered: %s", tool.Name)
		}
		if _, ok := mapped[tool.Name]; !ok {
			t.Errorf("tool %s has no capability", tool.Name)
		}
		if tool.Name == "read_channel" && !strings.Contains(string(tool.OutputSchema.Properties["messages"]), `"edited_at"`) {
			t.Errorf("read_channel's output schema has no edited_at: %s", tool.OutputSchema.Properties["messages"])
		}
	}
	if len(list.Result.Tools) != len(mapped) {
		t.Errorf("%d tools offered, %d mapped", len(list.Result.Tools), len(mapped))
	}
}

// One message_edited row per edit that changed the text, with the revision it
// made; none for a refusal or a no-op; no text in any row (§8.3).
func TestMessageEditAudit(t *testing.T) {
	w := newEditWorld(t)
	before := len(w.audit(t))
	w.mustEdit(t, "ann", w.whisper, "SECRET-whisper two")
	w.mustEdit(t, "ann", w.whisper, "SECRET-whisper two") // a no-op
	w.mustEdit(t, "ann", w.whisper, "SECRET-whisper three")
	w.edit(t, "olga", "ops", w.whisper, editBody("SECRET-olga"))
	w.edit(t, "ann", "ops", 999999, editBody("SECRET-ghost"))
	w.edit(t, "aria", "ops", w.wide, editBody("SECRET-agent"))
	var rows []store.AuditEvent
	for _, e := range w.audit(t)[before:] {
		if strings.Contains(e.Detail, "SECRET") || strings.Contains(e.Subject, "SECRET") || strings.Contains(e.Detail, "whisper") {
			t.Errorf("an audit row holds text: %+v", e)
		}
		if e.Action == store.AuditMessageEdited {
			rows = append(rows, e)
		}
	}
	want := []string{fmt.Sprintf("channel=%d revision=2", w.ops.ID), fmt.Sprintf("channel=%d revision=3", w.ops.ID)}
	if len(rows) != 2 || rows[0].Detail != want[0] || rows[1].Detail != want[1] ||
		rows[0].Subject != fmt.Sprintf("message:%d", w.whisper) || rows[0].Actor != fmt.Sprintf("principal:%d", w.p("ann").ID) {
		t.Errorf("message_edited rows = %+v, want details %v", rows, want)
	}
}

// Every v2 read of an edited message carries the current text and edited_at:
// the list, a thread, search (the new word and not the old) and the post's
// own root lookup; v0 and v1 carry the text unmarked (§6).
func TestMessageEditReadSurfaces(t *testing.T) {
	w := newEditWorld(t)
	root := w.mustPost(t, "ann", `{"body":"oldword root"}`)
	w.mustPost(t, "olga", fmt.Sprintf(`{"body":"a reply","thread_root_id":%d}`, root))
	edited := w.mustEdit(t, "ann", root, "newword root")
	if edited.EditedAt == nil || !edited.EditedAt.Time().After(edited.CreatedAt.Time()) {
		t.Fatalf("the edit's answer = %+v", edited)
	}
	check := func(what, body string) {
		t.Helper()
		var page struct {
			Messages []schema.MessageV2 `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("%s: %v %s", what, err, body)
		}
		for _, m := range page.Messages {
			if m.ID == root {
				if m.Body != "newword root" || m.EditedAt == nil || !m.EditedAt.Time().Equal(edited.EditedAt.Time()) {
					t.Errorf("%s: %+v", what, m)
				}
				return
			}
		}
		t.Errorf("%s does not hold the message: %s", what, body)
	}
	tok := w.p("olga").token
	check("the v2 list", w.callREST(t, "GET", "/v2/channels/ops/messages", tok, "").body)
	check("the thread", w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages?thread=%d", root), tok, "").body)
	check("search for the new word", w.callREST(t, "GET", "/v2/channels/ops/messages/search?q=newword", tok, "").body)
	if res := w.callREST(t, "GET", "/v2/channels/ops/messages/search?q=oldword", tok, ""); strings.Contains(res.body, fmt.Sprintf(`"id":%d`, root)) {
		t.Errorf("the old word still finds the message: %s", res.body)
	}
	for _, v := range []string{"v0", "v1"} {
		res := w.callREST(t, "GET", "/"+v+"/channels/ops/messages", tok, "")
		if !strings.Contains(res.body, "newword root") || strings.Contains(res.body, "edited_at") {
			t.Errorf("the %s list = %s", v, res.body)
		}
	}
}

// The /v2/ws replay (§7.1): an edit committed while the replay runs reaches
// the client although its id is at or below the last one replayed, and an
// edit committed before the replay is replayed as the edited text, with no
// stale copy.
func TestMessageEditReplay(t *testing.T) {
	var f *replayFixture
	var seeds []int64
	edited := make(chan struct{})
	var once sync.Once
	f = newReplayFixture(t, 100, wsReplayHooks{afterPage: func(page int) {
		if page != 0 {
			return
		}
		once.Do(func() {
			// seeds[0] was read by this page with its old text; its edit
			// lands in the subscription's buffer behind it.
			res := f.callREST(t, "PUT", epath("ops", seeds[0]), f.p("ann").token, editBody("seed 0, edited during the replay"))
			if res.status != http.StatusOK {
				t.Errorf("edit during the replay = %d %s", res.status, res.body)
			}
			close(edited)
		})
	}})
	f.srv.setMessageEditLimit(inviteLimit{name: "test", burst: 1000, window: time.Minute})
	for i := range 3 {
		seeds = append(seeds, f.mustPost(t, "ann", fmt.Sprintf(`{"body":"seed %d"}`, i)))
	}
	if res := f.callREST(t, "PUT", epath("ops", seeds[1]), f.p("ann").token, editBody("seed 1, edited before the replay")); res.status != http.StatusOK {
		t.Fatalf("edit before the replay = %d %s", res.status, res.body)
	}
	sock := openSocket(t, f.wsBase, "/v2/ws?channel=ops&after=0", f.p("olga").token)
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d %s", sock.status, sock.body)
	}
	select {
	case <-edited:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook never ran")
	}
	sentinel := f.mustPost(t, "ann", `{"body":"sentinel"}`)
	var got []string
	for _, raw := range sock.readUntil(t, sentinel) {
		var m schema.MessageV2
		if err := json.Unmarshal(raw, &m); err != nil || m.Validate() != nil {
			t.Fatalf("frame %s: %v", raw, err)
		}
		mark := ""
		if m.EditedAt != nil {
			mark = " (edited)"
		}
		got = append(got, fmt.Sprintf("%d %s%s", m.ID, m.Body, mark))
	}
	want := []string{
		fmt.Sprintf("%d seed 0", seeds[0]),
		fmt.Sprintf("%d seed 1, edited before the replay (edited)", seeds[1]),
		fmt.Sprintf("%d seed 2", seeds[2]),
		fmt.Sprintf("%d seed 0, edited during the replay (edited)", seeds[0]),
		fmt.Sprintf("%d sentinel", sentinel),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("frames:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
