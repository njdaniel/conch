package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Audience-safe message removal (issue #852, docs/design/moderation.md §7), on
// the scoped fixture: channel "ops" with people on nets, a monitor, agents and
// a stranger. Every message the tests remove carries the word SECRET, so a
// leak is a substring check.

// removalWorld is the file fixture (it uploads) with olga made a manager of
// ops, an operator who is not a member, and one message of each audience.
type removalWorld struct {
	*fileFixture
	wide, net, whisper, typed, reply, attached int64
	file                                       schema.FileV1
}

func newRemovalWorld(t *testing.T) *removalWorld {
	t.Helper()
	f := newFileFixture(t)
	w := &removalWorld{fileFixture: f}
	ctx := context.Background()
	no, yes := false, true
	// olga, a manager who is on no net: she sees channel-wide messages only.
	if _, err := f.srv.store.PutChannelMember(ctx, "system", f.ops.ID, f.p("olga").ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	f.add(t, "op2", store.PrincipalHuman, false)
	if _, err := f.srv.store.SetRole(ctx, "system", f.root.ID, f.p("op2").ID, store.RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	w.wide = f.mustPost(t, "ann", `{"body":"SECRET-wide needle"}`)
	w.net = f.mustPost(t, "ann", `{"body":"SECRET-net needle",`+netAudience(f.net1.ID)+`}`)
	w.whisper = f.mustPost(t, "ann", `{"body":"SECRET-whisper needle",`+whisperAudience(f.p("wes").ID)+`}`)
	w.typed = f.mustPost(t, "ann", `{"body":"SECRET-typed needle","payload":{"schema":"leviathan.trade_signal.v1","data":{"side":"SECRET-buy"}}}`)
	w.reply = f.mustPost(t, "nina", fmt.Sprintf(`{"body":"SECRET-reply needle","thread_root_id":%d}`, w.wide))
	w.file = f.mustUpload(t, "ann", "secret.txt", []byte("SECRET-bytes"))
	w.attached = f.mustPost(t, "ann", attachBody("SECRET-attached needle", "", w.file.ID))
	return w
}

func rmPath(channel string, msg any) string {
	return fmt.Sprintf("/v2/channels/%s/messages/%v", channel, msg)
}

func (w *removalWorld) remove(t *testing.T, who string, msg any) wireResult {
	t.Helper()
	return w.callREST(t, "DELETE", rmPath("ops", msg), w.p(who).token, "")
}

func (w *removalWorld) mustRemove(t *testing.T, who string, msg int64) {
	t.Helper()
	if res := w.remove(t, who, msg); res.status != http.StatusNoContent || res.body != "" {
		t.Fatalf("%s removes %d = %d %s", who, msg, res.status, res.body)
	}
}

func (w *removalWorld) removals(t *testing.T) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range w.audit(t) {
		if e.Action == store.AuditMessageRemoved {
			out = append(out, e)
		}
	}
	return out
}

// Who may remove what, in the order of the handler's checks, and what each
// refusal looks like: the same bytes as a ghost, whatever the target.
func TestMessageRemovalChecks(t *testing.T) {
	w := newRemovalWorld(t)
	for _, tt := range []struct {
		name, who, channel string
		msg                any
		status             int
		code               string
	}{
		{"a plain member", "ann", "ops", w.wide, 403, "forbidden"},
		{"an agent", "aria", "ops", w.wide, 403, "forbidden"},
		{"a stranger", "nora", "ops", w.wide, 404, "channel_not_found"},
		{"an operator who is not a member", "op2", "ops", w.wide, 404, "channel_not_found"},
		{"a ghost channel", "olga", "nosuch", w.wide, 404, "channel_not_found"},
		{"a bad id", "olga", "ops", "abc", 400, "invalid_request"},
		{"a zero id", "olga", "ops", 0, 400, "invalid_request"},
		{"a negative id", "olga", "ops", -4, 400, "invalid_request"},
		{"a ghost message", "olga", "ops", 999999, 404, "message_not_found"},
		{"the manager, a net message they are not on", "olga", "ops", w.net, 404, "message_not_found"},
		{"the manager, a whisper they are not in", "olga", "ops", w.whisper, 404, "message_not_found"},
		{"the owner, a whisper they are not in", "root", "ops", w.whisper, 404, "message_not_found"},
		{"the owner, a net message they are not on", "root", "ops", w.net, 404, "message_not_found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := w.callREST(t, "DELETE", rmPath(tt.channel, tt.msg), w.p(tt.who).token, "")
			if res.status != tt.status || reactErrCode(res) != tt.code {
				t.Errorf("= %d %s, want %d %s", res.status, res.body, tt.status, tt.code)
			}
		})
	}
	// Unknown and invisible targets are one answer, byte for byte.
	ghost := w.remove(t, "olga", 999999)
	for _, c := range []struct {
		who string
		msg int64
	}{{"olga", w.net}, {"olga", w.whisper}, {"root", w.whisper}, {"root", w.net}} {
		if got := w.remove(t, c.who, c.msg); got != ghost {
			t.Errorf("%s on %d: %+v; a ghost gives %+v", c.who, c.msg, got, ghost)
		}
		if w.remove(t, c.who, c.msg).status == 204 {
			t.Errorf("%s removed %d", c.who, c.msg)
		}
	}
	// A stranger and a non-member operator get an unknown channel's answer.
	for _, who := range []string{"nora", "op2"} {
		if got, want := w.remove(t, who, w.wide), w.callREST(t, "DELETE", rmPath("nosuch", w.wide), w.p(who).token, ""); got != want {
			t.Errorf("%s: %+v; an unknown channel gives %+v", who, got, want)
		}
	}
	// Nothing was removed and nothing audited.
	if n := len(w.removals(t)); n != 0 {
		t.Errorf("%d removal audit rows after only refusals", n)
	}
	for _, id := range []int64{w.wide, w.net, w.whisper} {
		if got := w.visibleAs(t, "ann", id); got.Removed || !strings.HasPrefix(got.Body, "SECRET-") {
			t.Errorf("message %d after the probes = %+v", id, got)
		}
	}
}

// The owner and a manager remove what they may see; the removal is 204 and
// idempotent, writes one audit row with ids only, and the message reads as the
// tombstone on every route and version.
func TestMessageRemovalIdempotentAndAudited(t *testing.T) {
	w := newRemovalWorld(t)
	w.mustRemove(t, "olga", w.wide)
	w.mustRemove(t, "olga", w.wide) // already removed
	w.mustRemove(t, "root", w.wide) // the owner, too
	w.mustRemove(t, "root", w.attached)
	events := w.removals(t)
	if len(events) != 2 {
		t.Fatalf("%d removal rows, want one per message: %+v", len(events), events)
	}
	want := []store.AuditEvent{
		{Actor: fmt.Sprintf("principal:%d", w.p("olga").ID), Action: store.AuditMessageRemoved, Subject: fmt.Sprintf("message:%d", w.wide), Detail: fmt.Sprintf("channel=%d", w.ops.ID)},
		{Actor: fmt.Sprintf("principal:%d", w.root.ID), Action: store.AuditMessageRemoved, Subject: fmt.Sprintf("message:%d", w.attached), Detail: fmt.Sprintf("channel=%d", w.ops.ID)},
	}
	for i, e := range events {
		if e.Actor != want[i].Actor || e.Subject != want[i].Subject || e.Detail != want[i].Detail {
			t.Errorf("audit row %d = %+v, want %+v", i, e, want[i])
		}
	}
	for _, e := range w.audit(t) {
		if strings.Contains(e.Detail+e.Subject+e.Actor, "SECRET") {
			t.Errorf("an audit row holds content: %+v", e)
		}
	}
	// The row is in the database, once per message, with the actor.
	if n := queryDB(t, w.srv, "SELECT COUNT(*) FROM message_removals"); n != "2" {
		t.Errorf("message_removals rows = %s; want 2", n)
	}
	if n := queryDB(t, w.srv, fmt.Sprintf("SELECT removed_by FROM message_removals WHERE message_id = %d", w.wide)); n != fmt.Sprint(w.p("olga").ID) {
		t.Errorf("the first remover recorded = %s, want olga", n)
	}
}

// All reads of every audience, as every kind of reader, hold no original once
// a message is removed; the tombstone is the specified shape on each version.
func TestMessageRemovalProjectionOnEveryRead(t *testing.T) {
	w := newRemovalWorld(t)
	f := w.fileFixture
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet)
	w.mustRemove(t, "olga", w.wide)
	w.mustRemove(t, "olga", w.typed)
	w.mustRemove(t, "olga", w.attached)
	// The reply of a removed root is a message of its own.
	w.mustRemove(t, "olga", w.reply)
	// The ones olga cannot see are removed by someone who can: the author's
	// net, and the whisper by a manager who is in it.
	no, yes := false, true
	ctx := context.Background()
	if _, err := f.srv.store.PutChannelMember(ctx, "system", f.ops.ID, f.p("wes").ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutChannelMember(ctx, "system", f.ops.ID, f.p("nina").ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	w.mustRemove(t, "wes", w.whisper)
	w.mustRemove(t, "nina", w.net)

	removedIDs := map[int64]bool{w.wide: true, w.typed: true, w.attached: true, w.reply: true, w.whisper: true, w.net: true}
	people := []string{"ann", "nina", "mona", "wes", "olga", "dina", "root", "aria"}
	for _, who := range people {
		var all strings.Builder
		for _, path := range []string{
			"/v0/channels/ops/messages?limit=100",
			"/v1/channels/ops/messages?limit=100",
			"/v2/channels/ops/messages?limit=100",
			"/v2/channels/ops/messages?limit=100&order=desc",
			fmt.Sprintf("/v2/channels/ops/messages?thread=%d", w.wide),
			"/v2/channels/ops/messages/search?q=needle",
			"/v2/channels/ops/messages/search?q=SECRET",
		} {
			res := w.callREST(t, "GET", path, f.p(who).token, "")
			if who == "aria" && strings.HasPrefix(path, "/v0") {
				continue
			}
			all.WriteString(res.body)
			if strings.Contains(res.body, "SECRET") && !strings.Contains(path, "search") {
				t.Errorf("%s GET %s holds an original: %s", who, path, res.body)
			}
			if strings.Contains(path, "search") && res.status == 200 {
				var out schema.SearchMessagesResponseV2
				if err := json.Unmarshal([]byte(res.body), &out); err != nil {
					t.Fatal(err)
				}
				for _, m := range out.Messages {
					if removedIDs[m.ID] {
						t.Errorf("%s's search returned removed message %d", who, m.ID)
					}
				}
			}
		}
		out := f.mcpCall(t, f.p(who).token, "read_channel", map[string]any{"channel": "ops"})
		if strings.Contains(out.body, "SECRET-wide") || strings.Contains(out.body, "SECRET-typed") || strings.Contains(out.body, "SECRET-buy") || strings.Contains(out.body, "SECRET-attached") {
			t.Errorf("%s's read_channel holds an original: %s", who, out.body)
		}
	}
	// The shape, as ann reads it on each version.
	var v2 schema.ListMessagesResponseV2
	body := w.callREST(t, "GET", "/v2/channels/ops/messages?limit=100", w.p("ann").token, "").body
	if err := json.Unmarshal([]byte(body), &v2); err != nil {
		t.Fatal(err)
	}
	for _, m := range v2.Messages {
		if err := m.Validate(); err != nil {
			t.Errorf("message %d is invalid: %v", m.ID, err)
		}
	}
	seen := 0
	for _, m := range v2.Messages {
		if !removedIDs[m.ID] {
			if m.Removed {
				t.Errorf("message %d reads removed", m.ID)
			}
			continue
		}
		seen++
		if !m.Removed || m.Body != schema.MessageRemovedBody || m.Payload != nil || m.EditedAt != nil {
			t.Errorf("message %d = %+v, want the tombstone", m.ID, m)
		}
		if m.AuthorID == 0 || m.CreatedAt.Time().IsZero() {
			t.Errorf("message %d lost its place: %+v", m.ID, m)
		}
	}
	if seen != len(removedIDs) {
		t.Errorf("ann reads %d tombstones, want %d", seen, len(removedIDs))
	}
	for _, m := range v2.Messages {
		switch m.ID {
		case w.reply:
			if m.ThreadRootID != w.wide {
				t.Errorf("the removed reply lost its thread: %+v", m)
			}
		case w.net:
			if m.Audience == nil || m.Audience.Kind != schema.AudienceKindNet {
				t.Errorf("the removed net message lost its audience: %+v", m)
			}
		case w.whisper:
			if m.Audience == nil || m.Audience.Kind != schema.AudienceKindPrincipals {
				t.Errorf("the removed whisper lost its audience: %+v", m)
			}
		}
	}
	// v1 and v0 serve the fixed body and no marker.
	for _, path := range []string{"/v1/channels/ops/messages?limit=100", "/v0/channels/ops/messages?limit=100"} {
		res := w.callREST(t, "GET", path, w.p("ann").token, "")
		if !strings.Contains(res.body, schema.MessageRemovedBody) || strings.Contains(res.body, `"removed"`) {
			t.Errorf("%s = %s", path, res.body)
		}
	}
	// A reader who may not see a scoped message gets no tombstone for it.
	body = w.callREST(t, "GET", "/v2/channels/ops/messages?limit=100", w.p("olga").token, "").body
	for _, id := range []int64{w.net, w.whisper} {
		if strings.Contains(body, fmt.Sprintf(`"id":%d,`, id)) {
			t.Errorf("olga reads scoped message %d: %s", id, body)
		}
	}
	// Reactions and edits find no message to act on.
	if res := w.callREST(t, "PUT", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions/%s", w.wide, url.PathEscape("👍")), w.p("ann").token, ""); res.status != 404 || reactErrCode(res) != "message_not_found" {
		t.Errorf("reacting to a removed message = %d %s", res.status, res.body)
	}
	if res := w.callREST(t, "PUT", epath("ops", w.wide), w.p("ann").token, editBody("SECRET-again")); res.status != 409 || reactErrCode(res) != schema.ErrorCodeMessageNotEditable {
		t.Errorf("editing a removed message = %d %s", res.status, res.body)
	}
	if res := w.callREST(t, "GET", revpath("ops", w.wide), w.p("ann").token, ""); res.status != 404 || strings.Contains(res.body, "SECRET") {
		t.Errorf("reading the revisions of a removed message = %d %s", res.status, res.body)
	}
}

// Search drops removed messages in the statement, before the limit and the
// cursor: pages stay full and next_before skips them.
func TestMessageRemovalSearch(t *testing.T) {
	w := newRemovalWorld(t)
	var ids []int64
	for i := 0; i < 6; i++ {
		ids = append(ids, w.mustPost(t, "ann", fmt.Sprintf(`{"body":"findme %d"}`, i)))
	}
	// Remove the two newest and one in the middle.
	for _, id := range []int64{ids[5], ids[4], ids[2]} {
		w.mustRemove(t, "olga", id)
	}
	page := func(query string) (got []int64, next int64) {
		res := w.callREST(t, "GET", "/v2/channels/ops/messages/search?q=findme&"+query, w.p("ann").token, "")
		var out schema.SearchMessagesResponseV2
		if err := json.Unmarshal([]byte(res.body), &out); err != nil {
			t.Fatalf("%s: %v", res.body, err)
		}
		for _, m := range out.Messages {
			got = append(got, m.ID)
		}
		return got, out.NextBefore
	}
	got, next := page("limit=2")
	if want := []int64{ids[3], ids[1]}; !reflect.DeepEqual(got, want) || next != ids[1] {
		t.Errorf("first page = %v next %d, want %v next %d", got, next, want, ids[1])
	}
	got, next = page(fmt.Sprintf("limit=2&before=%d", next))
	if want := []int64{ids[0]}; !reflect.DeepEqual(got, want) || next != 0 {
		t.Errorf("second page = %v next %d, want %v and no next", got, next, want)
	}
	// A cursor on a removed id still pages.
	got, _ = page(fmt.Sprintf("limit=5&before=%d", ids[2]))
	if want := []int64{ids[1], ids[0]}; !reflect.DeepEqual(got, want) {
		t.Errorf("a cursor at the removed message = %v, want %v", got, want)
	}
	// An old term finds nothing once its only message is removed.
	res := w.callREST(t, "GET", "/v2/channels/ops/messages/search?q=SECRET-wide", w.p("ann").token, "")
	before := res.body
	w.mustRemove(t, "olga", w.wide)
	res = w.callREST(t, "GET", "/v2/channels/ops/messages/search?q=SECRET-wide", w.p("ann").token, "")
	if strings.Contains(before, fmt.Sprintf(`"id":%d,`, w.wide)) == false {
		t.Fatalf("the term never found the message: %s", before)
	}
	if strings.Contains(res.body, fmt.Sprintf(`"id":%d,`, w.wide)) || strings.Contains(res.body, "SECRET-wide") {
		t.Errorf("the removed message is still found: %s", res.body)
	}
}

// A file attached to a removed message is a 404 on both routes, to the
// uploader and to a recipient, the same bytes as a file that does not exist;
// an unattached upload is unchanged.
func TestMessageRemovalFiles(t *testing.T) {
	w := newRemovalWorld(t)
	loose := w.mustUpload(t, "ann", "loose.txt", []byte("not attached"))
	for _, who := range []string{"ann", "olga", "wes"} {
		if got := w.fetch(t, who, w.file.ID); got.status != 200 && who != "wes" {
			t.Fatalf("%s cannot read the attachment before: %s", who, got)
		}
	}
	w.mustRemove(t, "olga", w.attached)
	for _, who := range []string{"ann", "olga", "root"} {
		for name, got := range map[string]fileResp{"file": w.fetch(t, who, w.file.ID), "meta": w.meta(t, who, w.file.ID)} {
			want := w.fetch(t, who, ghostID)
			if name == "meta" {
				want = w.meta(t, who, ghostID)
			}
			if got.status != 404 || got.status != want.status || string(got.body) != string(want.body) {
				t.Errorf("%s %s of a removed attachment = %s; a ghost gives %s", who, name, got, want)
			}
			if strings.Contains(string(got.body), "secret.txt") || strings.Contains(string(got.body), "SECRET") {
				t.Errorf("%s %s leaks: %s", who, name, got)
			}
		}
	}
	if got := w.fetch(t, "ann", loose.ID); got.status != 200 || string(got.body) != "not attached" {
		t.Errorf("an unattached upload = %s", got)
	}
}

// A removed root is a tombstone that keeps its replies coherent, takes no new
// reply (thread_not_found, over REST and MCP), and leaves existing replies
// alone; removing a reply touches only the reply.
func TestMessageRemovalThreads(t *testing.T) {
	w := newRemovalWorld(t)
	f := w.fileFixture
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet)
	root := f.mustPost(t, "ann", `{"body":"SECRET-root"}`)
	r1 := f.mustPost(t, "nina", fmt.Sprintf(`{"body":"reply one","thread_root_id":%d}`, root))
	r2 := f.mustPost(t, "olga", fmt.Sprintf(`{"body":"reply two","thread_root_id":%d}`, root))
	w.mustRemove(t, "olga", r1)
	thread := func() []schema.MessageV2 {
		res := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages?thread=%d", root), w.p("ann").token, "")
		var out schema.ListMessagesResponseV2
		if err := json.Unmarshal([]byte(res.body), &out); err != nil || res.status != 200 {
			t.Fatalf("thread = %d %s: %v", res.status, res.body, err)
		}
		return out.Messages
	}
	if got := thread(); len(got) != 3 || got[0].Removed || !got[1].Removed || got[2].Removed || got[2].Body != "reply two" {
		t.Errorf("after removing a reply the thread is %+v", got)
	}
	w.mustRemove(t, "olga", root)
	got := thread()
	if len(got) != 3 || !got[0].Removed || got[0].ID != root || got[0].Body != schema.MessageRemovedBody || got[2].Body != "reply two" || got[2].ThreadRootID != root {
		t.Errorf("after removing the root the thread is %+v", got)
	}
	// A reply to a removed root is the existing thread_not_found, over every
	// front, and stores nothing.
	count := f.messageCount(t)
	for _, who := range []string{"ann", "olga"} {
		res := w.callREST(t, "POST", "/v2/channels/ops/messages", w.p(who).token, fmt.Sprintf(`{"body":"late","thread_root_id":%d}`, root))
		if res.status != 404 || reactErrCode(res) != "thread_not_found" {
			t.Errorf("%s replies to a removed root = %d %s", who, res.status, res.body)
		}
	}
	mcp := f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "late", "thread_root_id": root})
	if !mcp.isError || mcp.code != "thread_not_found" {
		t.Errorf("an agent replies to a removed root: %+v", mcp)
	}
	if res, _, err := f.srv.store.InsertReplyMessage(t.Context(), store.ReplyPost{ChannelID: f.ops.ID, AuthorID: f.p("ann").ID, RootID: root, Body: "late"},
		store.Reader{PrincipalID: f.p("ann").ID, Scoped: true}); err == nil {
		t.Errorf("the store took a reply to a removed root: %+v", res)
	}
	if f.messageCount(t) != count {
		t.Error("a refused reply stored a message")
	}
	// The thread reads from a tombstone root by the other roots' rule, and a
	// reply to a live root still works.
	live := f.mustPost(t, "ann", `{"body":"another root"}`)
	if res := w.callREST(t, "POST", "/v2/channels/ops/messages", w.p("nina").token, fmt.Sprintf(`{"body":"ok","thread_root_id":%d}`, live)); res.status != 201 {
		t.Errorf("a reply to a live root = %d %s", res.status, res.body)
	}
	_ = r2
}

// The same removal under concurrency: many removers, readers and a reply race;
// one removal row, never a half-state, and a reader who has seen the tombstone
// never reads the original again.
func TestMessageRemovalRaces(t *testing.T) {
	w := newRemovalWorld(t)
	target := w.mustPost(t, "ann", `{"body":"SECRET-race"}`)
	root := w.mustPost(t, "ann", `{"body":"SECRET-race-root"}`)
	var wg sync.WaitGroup
	statuses := make(chan int, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- w.remove(t, "olga", target).status
		}()
	}
	readerErr := make(chan string, 4)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 3; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			sawTomb := false
			for {
				select {
				case <-stop:
					return
				default:
				}
				m := w.visibleAs(t, "nina", target)
				switch {
				case m.Removed && (m.Body != schema.MessageRemovedBody || m.Payload != nil):
					readerErr <- fmt.Sprintf("half state %+v", m)
					return
				case m.Removed:
					sawTomb = true
				case sawTomb:
					readerErr <- "the original came back after the tombstone"
					return
				}
			}
		}()
	}
	// A reply races the removal of its root: stored before it, or refused.
	replyDone := make(chan int, 1)
	go func() {
		replyDone <- w.callREST(t, "POST", "/v2/channels/ops/messages", w.p("nina").token,
			fmt.Sprintf(`{"body":"racing reply","thread_root_id":%d}`, root)).status
	}()
	rmDone := make(chan int, 1)
	go func() { rmDone <- w.remove(t, "olga", root).status }()
	wg.Wait()
	if s := <-rmDone; s != 204 {
		t.Errorf("removing the root = %d", s)
	}
	if s := <-replyDone; s != 201 && s != 404 {
		t.Errorf("the racing reply = %d", s)
	}
	close(stop)
	readers.Wait()
	close(statuses)
	for s := range statuses {
		if s != 204 {
			t.Errorf("a concurrent removal answered %d", s)
		}
	}
	select {
	case msg := <-readerErr:
		t.Error(msg)
	default:
	}
	n := 0
	for _, e := range w.removals(t) {
		if e.Subject == fmt.Sprintf("message:%d", target) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d removal rows for one message removed 8 times", n)
	}
}

// An archived channel allows a removal (Q1: reduce-only), and a store-level
// remover is held to the same rules as the route.
func TestMessageRemovalArchivedAndStore(t *testing.T) {
	w := newRemovalWorld(t)
	ctx := context.Background()
	st := w.srv.store
	reader := func(who string) store.Reader { return store.Reader{PrincipalID: w.p(who).ID, Scoped: true} }
	if _, err := st.RemoveMessage(ctx, w.ops.ID, reader("ann"), w.wide, "principal:x", time.Now()); err == nil {
		t.Error("a plain member removed a message through the store")
	}
	if _, err := st.RemoveMessage(ctx, w.ops.ID, reader("root"), w.whisper, "principal:x", time.Now()); err == nil {
		t.Error("the owner removed a whisper they are not in through the store")
	}
	if _, err := st.RemoveMessage(ctx, w.ops.ID, store.Reader{PrincipalID: w.p("olga").ID}, w.wide, "principal:x", time.Now()); err == nil {
		t.Error("a reader that is not scoped removed a message")
	}
	if res := w.callREST(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", w.ops.ID), w.rootTok, `{"archived":true}`); res.status != 200 {
		t.Fatalf("archive = %d %s", res.status, res.body)
	}
	if res := w.callREST(t, "POST", "/v2/channels/ops/messages", w.p("ann").token, `{"body":"refused"}`); res.status != 409 {
		t.Fatalf("a post to the archived channel = %d %s", res.status, res.body)
	}
	w.mustRemove(t, "olga", w.wide)
	// With authentication off (no principal) the system actor removes a
	// channel-wide message, and only one.
	if _, err := st.RemoveMessage(ctx, w.ops.ID, store.Reader{}, w.whisper, "system", time.Now()); err == nil {
		t.Error("the system actor removed a whisper")
	}
	off, err := st.RemoveMessage(ctx, w.ops.ID, store.Reader{}, w.typed, "system", time.Now())
	if err != nil || !off.Changed || !off.Message.Removed || off.Message.Payload != nil {
		t.Errorf("removal with authentication off = %+v, %v", off, err)
	}
	if got := w.visibleAs(t, "ann", w.wide); !got.Removed {
		t.Errorf("the message in the archived channel = %+v", got)
	}
}

// queryDB reads one value from the fixture's database file.
func queryDB(t *testing.T, srv *Server, query string) string {
	t.Helper()
	path, ok := testDBPaths.Load(srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var out sql.NullString
	if err := db.QueryRowContext(context.Background(), query).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out.String
}
