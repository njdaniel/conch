package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The messages, in the order they are posted. Channel-wide messages ("w…") are
// interleaved with scoped ones so that every reader's expected set is a proper,
// non-contiguous subset.
const (
	msgW1       = "w1"       // channel-wide, by olga
	msgNet1     = "net1"     // net1, by ann
	msgW3       = "w3"       // channel-wide, by ann
	msgNet2     = "net2"     // net2, by nina
	msgW5       = "w5"       // channel-wide, by wes
	msgWhisper1 = "whisper1" // ann to wes and arlo
	msgW7       = "w7"       // channel-wide, by ann
	msgWhisper2 = "whisper2" // ann to aria
	msgW9       = "w9"       // channel-wide, by olga: the sentinel every socket receives last
)

var wideMessages = []string{msgW1, msgW3, msgW5, msgW7, msgW9}

// postScopedCorpus posts the messages above through the real v2 REST handler
// and returns label -> id. Each scoped body contains its label, so a leaked
// body is recognisable in a response.
func (f *scopedFixture) postScopedCorpus(t *testing.T) map[string]int64 {
	t.Helper()
	ids := map[string]int64{}
	post := func(label, who, audience string) {
		body := fmt.Sprintf(`{"body":"SECRET-%s"`, label)
		if audience != "" {
			body += "," + audience
		}
		ids[label] = f.mustPost(t, who, body+"}")
	}
	post(msgW1, "olga", "")
	post(msgNet1, "ann", netAudience(f.net1.ID))
	post(msgW3, "ann", "")
	post(msgNet2, "nina", netAudience(f.net2.ID))
	post(msgW5, "wes", "")
	post(msgWhisper1, "ann", whisperAudience(f.p("wes").ID, f.p("arlo").ID))
	post(msgW7, "ann", "")
	post(msgWhisper2, "ann", whisperAudience(f.p("aria").ID))
	post(msgW9, "olga", "")
	return ids
}

type readAccess string

const (
	readOK        readAccess = "ok"
	readNotFound  readAccess = "not found"
	readForbidden readAccess = "forbidden"
	readUnauth    readAccess = "unauthenticated"
)

// scopedReaders is the table of readers. v2 is what a reader that is allowed
// to read sees on a v2 path; every other path shows channel-wide messages only.
var scopedReaders = []struct {
	name   string
	who    string // key in the fixture, "" for no credential
	access readAccess
	agent  bool
	v2     []string
}{
	{"author of a net message and two whispers", "ann", readOK, false,
		[]string{msgW1, msgNet1, msgW3, msgW5, msgWhisper1, msgW7, msgWhisper2, msgW9}},
	{"net member on two nets", "nina", readOK, false,
		[]string{msgW1, msgNet1, msgW3, msgNet2, msgW5, msgW7, msgW9}},
	{"net monitor", "mona", readOK, false,
		[]string{msgW1, msgNet1, msgW3, msgW5, msgW7, msgW9}},
	{"whisper target and second net member", "wes", readOK, false,
		[]string{msgW1, msgW3, msgNet2, msgW5, msgWhisper1, msgW7, msgW9}},
	{"channel member outside every audience", "olga", readOK, false,
		[]string{msgW1, msgW3, msgW5, msgW7, msgW9}},
	{"operator outside every audience", "root", readOK, false,
		[]string{msgW1, msgW3, msgW5, msgW7, msgW9}},
	{"agent in two audiences with the read grant", "aria", readOK, true,
		[]string{msgW1, msgNet1, msgW3, msgW5, msgW7, msgWhisper2, msgW9}},
	{"agent in an audience without the read grant", "arlo", readForbidden, true, nil},
	{"agent outside the channel", "nomad", readNotFound, true, nil},
	{"human outside the channel", "nora", readNotFound, false, nil},
	{"disabled principal who was a recipient", "dina", readUnauth, false, nil},
	{"unauthenticated", "", readUnauth, false, nil},
}

var readStatus = map[readAccess]int{readOK: http.StatusOK, readNotFound: http.StatusNotFound, readForbidden: http.StatusForbidden, readUnauth: http.StatusUnauthorized}

// scopedReadPaths names every way a client can read messages. Each has a row
// in the table below. A new read path must be added to this list AND to
// TestScopedMessagesExactSets; TestEveryMessageReadPathIsInTheLeakTest fails
// until it is.
var scopedReadPaths = []string{
	"GET /v0/channels/{channel}/messages",
	"GET /v1/channels/{channel}/messages",
	"GET /v2/channels/{channel}/messages",
	"GET /v0/ws",
	"GET /v1/ws",
	"GET /v2/ws",
	"mcp read_channel",
}

func (f *scopedFixture) labelsToIDs(t *testing.T, ids map[string]int64, labels []string) []int64 {
	t.Helper()
	out := make([]int64, 0, len(labels))
	for _, l := range labels {
		id, ok := ids[l]
		if !ok {
			t.Fatalf("unknown message label %q", l)
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestScopedMessagesExactSets is the leak test. For every reader and every
// path by which messages can be read, it asserts the EXACT set of message ids
// received: not "contains", not "excludes the secret". v0, v1 and MCP readers
// see channel-wide messages only; only v2 readers see scoped messages, and
// only the ones they are a recipient of. Operators get no exemption.
//
// EVERY NEW WAY TO READ MESSAGES MUST ADD A COLUMN HERE (and to
// scopedReadPaths). Sockets are opened before the corpus is posted, since a
// socket carries live traffic only; the last message, a channel-wide one,
// ends each read.
func TestScopedMessagesExactSets(t *testing.T) {
	f := newScopedFixture(t)
	secrets := map[string]string{}
	for _, l := range []string{msgNet1, msgNet2, msgWhisper1, msgWhisper2} {
		secrets[l] = "SECRET-" + l
	}

	// Sockets of the readers who can connect, opened before anything is posted.
	type sockets struct{ v0, v1, v2 *socket }
	open := map[string]sockets{}
	for _, r := range scopedReaders {
		if r.access != readOK {
			continue
		}
		tok := f.p(r.who).token
		open[r.who] = sockets{
			v0: openSocket(t, f.base, "/v0/ws?channel=ops", tok),
			v1: openSocket(t, f.base, "/v1/ws?channel=ops", tok),
			v2: openSocket(t, f.base, "/v2/ws?channel=ops", tok),
		}
		for _, s := range []*socket{open[r.who].v0, open[r.who].v1, open[r.who].v2} {
			if s.status != http.StatusSwitchingProtocols {
				t.Fatalf("%s could not connect: %d %s", r.who, s.status, s.body)
			}
		}
	}

	ids := f.postScopedCorpus(t)
	// dina received the net1 message and is disabled afterwards: her rows and
	// seat stay, her credentials are revoked.
	if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.p("dina").ID); err != nil {
		t.Fatal(err)
	}
	sentinel := ids[msgW9]
	wide := f.labelsToIDs(t, ids, wideMessages)

	wantFor := func(r int, v2 bool) []int64 {
		if scopedReaders[r].access != readOK {
			return nil
		}
		if v2 {
			return f.labelsToIDs(t, ids, scopedReaders[r].v2)
		}
		return wide
	}
	noSecrets := func(t *testing.T, label, raw string, visible []int64) {
		t.Helper()
		allowed := map[string]bool{}
		for l, id := range ids {
			for _, v := range visible {
				if v == id {
					allowed[l] = true
				}
			}
		}
		for l, s := range secrets {
			if !allowed[l] && strings.Contains(raw, s) {
				t.Errorf("%s leaked the body of %s: %s", label, l, raw)
			}
		}
	}

	for ri, r := range scopedReaders {
		tok := ""
		if r.who != "" {
			tok = f.p(r.who).token
		}
		for _, version := range []string{"v0", "v1", "v2"} {
			v2 := version == "v2"
			t.Run(r.name+"/GET "+version+" list", func(t *testing.T) {
				got := f.callREST(t, "GET", "/"+version+"/channels/ops/messages?limit=100", tok, "")
				if got.status != readStatus[r.access] {
					t.Fatalf("status = %d, want %d; body %s", got.status, readStatus[r.access], got.body)
				}
				if r.access != readOK {
					return
				}
				gotIDs, _ := listIDs(t, got.body)
				if want := wantFor(ri, v2); !reflect.DeepEqual(gotIDs, want) {
					t.Errorf("ids = %v, want %v", gotIDs, want)
				}
				noSecrets(t, "list", got.body, wantFor(ri, v2))
				if !v2 && strings.Contains(got.body, `"audience"`) {
					t.Errorf("a %s list mentions an audience: %s", version, got.body)
				}
				if strings.Contains(got.body, "recipient") {
					t.Errorf("list mentions recipients: %s", got.body)
				}
			})
			t.Run(r.name+"/GET "+version+" ws", func(t *testing.T) {
				if r.access != readOK {
					got := openSocket(t, f.base, "/"+version+"/ws?channel=ops", tok)
					if got.status != readStatus[r.access] {
						t.Fatalf("status = %d, want %d; body %s", got.status, readStatus[r.access], got.body)
					}
					return
				}
				s := map[string]*socket{"v0": open[r.who].v0, "v1": open[r.who].v1, "v2": open[r.who].v2}[version]
				frames := s.readUntil(t, sentinel)
				if got, want := idsOf(t, frames), wantFor(ri, v2); !reflect.DeepEqual(got, want) {
					t.Errorf("ids = %v, want %v", got, want)
				}
				for _, raw := range frames {
					noSecrets(t, "socket", string(raw), wantFor(ri, v2))
					if strings.Contains(string(raw), "recipient") {
						t.Errorf("frame mentions recipients: %s", raw)
					}
					if !v2 && bytes.Contains(raw, []byte(`"audience"`)) {
						t.Errorf("a %s frame mentions an audience: %s", version, raw)
					}
				}
			})
		}
		t.Run(r.name+"/mcp read_channel", func(t *testing.T) {
			status, code, got := f.mcpReadChannel(t, tok, map[string]any{"channel": "ops", "limit": 100})
			// MCP is the agent front end: humans, the disabled and the
			// unauthenticated are refused at the door.
			wantStatus, wantCode := http.StatusOK, ""
			var want []int64
			switch {
			case !r.agent:
				wantStatus = http.StatusUnauthorized
			case r.access == readForbidden:
				wantCode = "forbidden"
			case r.access == readNotFound:
				wantCode = "channel_not_found"
			default:
				want = wantFor(ri, false) // the MCP read is a v1 reader
			}
			if status != wantStatus || code != wantCode {
				t.Fatalf("status = %d code = %q, want %d %q", status, code, wantStatus, wantCode)
			}
			if wantStatus == http.StatusOK && wantCode == "" && !reflect.DeepEqual(got, want) {
				t.Errorf("ids = %v, want %v", got, want)
			}
		})
	}
}

// Every route that can return message content, and every MCP tool that reads
// it, must be covered by TestScopedMessagesExactSets. Adding one without
// extending that test fails here.
func TestEveryMessageReadPathIsInTheLeakTest(t *testing.T) {
	f := newScopedFixture(t)
	covered := map[string]bool{}
	for _, p := range scopedReadPaths {
		covered[p] = true
	}
	for _, rt := range f.srv.routes {
		isRead := strings.HasPrefix(rt.pattern, "GET ") && (strings.Contains(rt.pattern, "/messages") || strings.HasSuffix(rt.pattern, "/ws"))
		if isRead && !covered[rt.pattern] {
			t.Errorf("route %q reads messages but is not in scopedReadPaths and TestScopedMessagesExactSets: add a column there", rt.pattern)
		}
	}
	for _, p := range scopedReadPaths {
		if strings.HasPrefix(p, "mcp ") {
			continue
		}
		found := false
		for _, rt := range f.srv.routes {
			found = found || rt.pattern == p
		}
		if !found {
			t.Errorf("scopedReadPaths lists %q, which is not a route", p)
		}
	}
	// MCP tools: each is either a message reader (and in the table) or declared
	// not to return message content.
	notMessageContent := map[string]string{
		"post_message":     "writes channel-wide; returns only the message it stored",
		"request_approval": "approvals carry no channel messages",
		"check_decision":   "approvals carry no channel messages",
		"await_decision":   "approvals carry no channel messages",
	}
	for tool := range schema.MCPToolCapabilities() {
		if tool == "read_channel" {
			if !covered["mcp read_channel"] {
				t.Error("read_channel is not in scopedReadPaths")
			}
			continue
		}
		if notMessageContent[tool] == "" {
			t.Errorf("MCP tool %q is new: if it returns message content add it to TestScopedMessagesExactSets, otherwise declare it here", tool)
		}
	}
}

// post rules ---------------------------------------------------------------

func TestScopedPostRules(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	archived := f.net(t, "old")
	f.seat(t, "old", "ann", schema.NetRoleMember)
	if _, err := f.srv.store.ArchiveNet(ctx, "system", f.ops.ID, "old"); err != nil {
		t.Fatal(err)
	}
	// What the same caller sees for a net that does not exist at all: the
	// not-on-net answer must be byte-identical.
	unknown := f.postV2(t, "olga", `{"body":"x",`+netAudience(9999)+`}`)
	if unknown.status != http.StatusNotFound {
		t.Fatalf("unknown net = %d %s", unknown.status, unknown.body)
	}
	other, err := f.srv.store.CreateChannel(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	foreignNet, err := f.srv.store.CreateNet(ctx, "system", other.ID, "foreign", f.root.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A scoped post by someone outside the channel is as blind as any other
	// access to it: the net is never looked at before membership, so the answer
	// is byte-identical to the one for a channel that does not exist.
	for _, who := range []string{"nora", "nomad"} {
		for _, aud := range []string{netAudience(f.net1.ID), whisperAudience(f.p("wes").ID)} {
			body := `{"body":"x",` + aud + `}`
			got := f.callREST(t, "POST", "/v2/channels/ops/messages", f.p(who).token, body)
			want := f.callREST(t, "POST", "/v2/channels/nosuch/messages", f.p(who).token, body)
			if got != want || got.status != http.StatusNotFound {
				t.Errorf("%s scoped post to a channel they are not in = %+v, want the unknown-channel response %+v", who, got, want)
			}
		}
	}

	// Watchers on the hub: nobody may hear about a refused post.
	watch := map[string]*hubWatch{}
	for _, name := range []string{"nina", "mona", "wes", "olga"} {
		watch[name] = newHubWatch(f, f.p(name).ID)
	}

	tests := []struct {
		name     string
		who      string
		body     string
		status   int
		code     string
		identity *wireResult // must be byte-identical to this response
	}{
		{"member posts to its net", "ann", `{"body":"x",` + netAudience(f.net1.ID) + `}`, http.StatusCreated, "", nil},
		{"monitor posting to its net", "mona", `{"body":"x",` + netAudience(f.net1.ID) + `}`, http.StatusForbidden, "forbidden", nil},
		{"caller not on the net", "olga", `{"body":"x",` + netAudience(f.net1.ID) + `}`, http.StatusNotFound, "net_not_found", &unknown},
		{"unknown net", "olga", `{"body":"x",` + netAudience(9999) + `}`, http.StatusNotFound, "net_not_found", &unknown},
		{"archived net, by a former member", "ann", `{"body":"x",` + netAudience(archived.ID) + `}`, http.StatusNotFound, "net_not_found", &unknown},
		{"net of another channel", "ann", `{"body":"x",` + netAudience(foreignNet.ID) + `}`, http.StatusNotFound, "net_not_found", &unknown},
		{"whisper to a channel member", "olga", `{"body":"x",` + whisperAudience(f.p("wes").ID) + `}`, http.StatusCreated, "", nil},
		{"whisper to someone outside the channel", "olga", `{"body":"x",` + whisperAudience(f.p("nora").ID) + `}`, http.StatusBadRequest, "invalid_audience", nil},
		{"whisper to an unknown principal", "olga", `{"body":"x",` + whisperAudience(99999) + `}`, http.StatusBadRequest, "invalid_audience", nil},
		{"whisper to nobody but oneself", "olga", `{"body":"x",` + whisperAudience(f.p("olga").ID) + `}`, http.StatusBadRequest, "invalid_audience", nil},
		{"whisper to nobody", "olga", `{"body":"x","audience":{"kind":"principals","principal_ids":[]}}`, http.StatusBadRequest, "invalid_audience", nil},
		{"unknown audience kind", "olga", `{"body":"x","audience":{"kind":"channel"}}`, http.StatusBadRequest, "invalid_audience", nil},
		{"net audience carrying principals", "ann", fmt.Sprintf(`{"body":"x","audience":{"kind":"net","net_id":%d,"principal_ids":[2]}}`, f.net1.ID), http.StatusBadRequest, "invalid_audience", nil},
		{"empty body", "ann", `{"body":"",` + netAudience(f.net1.ID) + `}`, http.StatusBadRequest, "invalid_request", nil},
		{"whitespace body", "ann", `{"body":"   ",` + netAudience(f.net1.ID) + `}`, http.StatusBadRequest, "invalid_request", nil},
		{"author of someone else", "ann", fmt.Sprintf(`{"author_id":%d,"body":"x",%s}`, f.p("nina").ID, netAudience(f.net1.ID)), http.StatusForbidden, "author_mismatch", nil},
		{"not in the channel", "nora", `{"body":"x",` + whisperAudience(f.p("wes").ID) + `}`, http.StatusNotFound, "channel_not_found", nil},
		{"unauthenticated", "", `{"body":"x",` + netAudience(f.net1.ID) + `}`, http.StatusUnauthorized, "unauthenticated", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, scoped, posts := f.messageCount(t), f.auditCount(t, store.AuditMessageScoped), f.auditCount(t, "message.post")
			for _, w := range watch {
				w.drain()
			}
			got := f.postV2(t, tt.who, tt.body)
			if got.status != tt.status {
				t.Fatalf("status = %d, want %d; body %s", got.status, tt.status, got.body)
			}
			if tt.code != "" {
				var e schema.Error
				if err := json.Unmarshal([]byte(got.body), &e); err != nil || e.Code != tt.code {
					t.Errorf("error body %q, want code %q", got.body, tt.code)
				}
			}
			if tt.identity != nil && got != *tt.identity {
				t.Errorf("response differs from the unknown-net response:\n got  %+v\n want %+v", got, *tt.identity)
			}
			refused := tt.status >= 400
			wantMsgs, wantScoped, wantPosts := msgs, scoped, posts
			if !refused {
				wantMsgs, wantScoped, wantPosts = msgs+1, scoped+1, posts+1
			}
			if n := f.messageCount(t); n != wantMsgs {
				t.Errorf("messages stored = %d, want %d", n, wantMsgs)
			}
			if n := f.auditCount(t, store.AuditMessageScoped); n != wantScoped {
				t.Errorf("message_scoped events = %d, want %d", n, wantScoped)
			}
			if n := f.auditCount(t, "message.post"); n != wantPosts {
				t.Errorf("message.post events = %d, want %d", n, wantPosts)
			}
			if refused {
				for name, w := range watch {
					if got := w.drain(); got != 0 {
						t.Errorf("a refused post was fanned out to %s (%d frames)", name, got)
					}
				}
			}
		})
	}
	if seen := f.rec.seen(); len(seen) != 0 {
		t.Errorf("scoped posts reached the Broadcaster seam: %v", seen)
	}
}

// hubWatch is a v2, a v1 and a v0 hub subscription for one principal, to see
// what a post fans out.
type hubWatch struct {
	v2 interface {
		Messages() <-chan schema.MessageV2
	}
	v1 interface {
		Messages() <-chan schema.MessageV1
	}
	v0 interface {
		Messages() <-chan schema.MessageV0
	}
}

func newHubWatch(f *scopedFixture, principal int64) *hubWatch {
	return &hubWatch{
		v2: f.srv.hub.SubscribeV2(f.ops.ID, principal, 16),
		v1: f.srv.hub.SubscribeV1(f.ops.ID, principal, 16),
		v0: f.srv.hub.Subscribe(f.ops.ID, principal, 16),
	}
}

// drain discards and counts everything buffered on all three subscriptions.
func (w *hubWatch) drain() int {
	n := 0
	for {
		select {
		case <-w.v2.Messages():
			n++
		case <-w.v1.Messages():
			n++
		case <-w.v0.Messages():
			n++
		default:
			return n
		}
	}
}

// A scoped post is delivered to its recipients' v2 sockets only: never to a v0
// or v1 subscription, never to the Broadcaster tap, and not to a v2 subscriber
// who is not a recipient.
func TestScopedPostFanOutAndResponse(t *testing.T) {
	f := newScopedFixture(t)
	names := []string{"ann", "nina", "mona", "wes", "olga", "aria"}
	watch := map[string]*hubWatch{}
	for _, n := range names {
		watch[n] = newHubWatch(f, f.p(n).ID)
	}
	tests := []struct {
		name string
		who  string
		body string
		aud  *schema.Audience
		got  []string // who receives it on v2
	}{
		{"net post", "ann", `{"body":"hello net",` + netAudience(f.net1.ID) + `}`,
			&schema.Audience{Kind: schema.AudienceKindNet, NetID: f.net1.ID}, []string{"ann", "nina", "mona", "aria"}},
		{"whisper, unordered with a duplicate-free list", "olga", `{"body":"psst",` + whisperAudience(f.p("wes").ID, f.p("ann").ID) + `}`,
			&schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: sortedIDs(f.p("ann").ID, f.p("wes").ID, f.p("olga").ID)}, []string{"ann", "wes", "olga"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.postV2(t, tt.who, tt.body)
			if res.status != http.StatusCreated {
				t.Fatalf("post = %d %s", res.status, res.body)
			}
			var resp schema.PostMessageResponseV2
			if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
				t.Fatal(err)
			}
			if err := resp.Message.Validate(); err != nil {
				t.Errorf("response is not a valid MessageV2: %v", err)
			}
			if !reflect.DeepEqual(resp.Message.Audience, tt.aud) {
				t.Errorf("audience = %+v, want %+v", resp.Message.Audience, tt.aud)
			}
			if resp.Message.AuthorID != f.p(tt.who).ID {
				t.Errorf("author = %d", resp.Message.AuthorID)
			}
			if strings.Contains(res.body, "recipient") {
				t.Errorf("response mentions recipients: %s", res.body)
			}
			want := map[string]bool{}
			for _, n := range tt.got {
				want[n] = true
			}
			for _, n := range names {
				w := watch[n]
				var v2 []int64
				for done := false; !done; {
					select {
					case m := <-w.v2.Messages():
						v2 = append(v2, m.ID)
						raw, _ := json.Marshal(m)
						if strings.Contains(string(raw), "recipient") {
							t.Errorf("frame mentions recipients: %s", raw)
						}
					default:
						done = true
					}
				}
				if want[n] != (len(v2) == 1) || len(v2) > 1 {
					t.Errorf("%s received %v on v2, want delivered=%v", n, v2, want[n])
				}
				if len(w.v1.Messages()) != 0 || len(w.v0.Messages()) != 0 {
					t.Errorf("%s received a scoped message on a v0 or v1 subscription", n)
				}
			}
			if seen := f.rec.seen(); len(seen) != 0 {
				t.Errorf("Broadcaster tap saw %v", seen)
			}
		})
	}

	// A channel-wide v2 post reaches every v0, v1 and v2 subscriber once.
	if res := f.postV2(t, "olga", `{"body":"hello all"}`); res.status != http.StatusCreated {
		t.Fatalf("channel-wide v2 post = %d %s", res.status, res.body)
	}
	for _, n := range names {
		w := watch[n]
		if len(w.v2.Messages()) != 1 || len(w.v1.Messages()) != 1 || len(w.v0.Messages()) != 1 {
			t.Errorf("%s: channel-wide post fanned out v2=%d v1=%d v0=%d, want 1 each",
				n, len(w.v2.Messages()), len(w.v1.Messages()), len(w.v0.Messages()))
		}
	}
	if seen := f.rec.seen(); len(seen) != 1 {
		t.Errorf("Broadcaster tap saw %v, want just the channel-wide post", seen)
	}
}

func sortedIDs(ids ...int64) []int64 {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// A v0 or v1 post stays channel-wide and reaches v2 sockets; an old client
// cannot create a scoped message.
func TestV0AndV1PostsAreChannelWide(t *testing.T) {
	f := newScopedFixture(t)
	w := newHubWatch(f, f.p("olga").ID)
	for _, path := range []string{"/v0/channels/ops/messages", "/v1/channels/ops/messages"} {
		res := f.callREST(t, "POST", path, f.p("ann").token, `{"body":"hello"}`)
		if res.status != http.StatusCreated {
			t.Fatalf("%s = %d %s", path, res.status, res.body)
		}
		// An audience on a v0/v1 post is an unknown field and refused.
		res = f.callREST(t, "POST", path, f.p("ann").token, `{"body":"hello",`+netAudience(f.net1.ID)+`}`)
		if res.status != http.StatusBadRequest {
			t.Errorf("%s with an audience = %d %s, want 400", path, res.status, res.body)
		}
	}
	if len(w.v2.Messages()) != 2 {
		t.Errorf("v2 subscriber heard %d of the 2 channel-wide posts", len(w.v2.Messages()))
	}
	ids, _ := listIDs(t, f.callREST(t, "GET", "/v2/channels/ops/messages", f.p("olga").token, "").body)
	if len(ids) != 2 {
		t.Errorf("v2 list = %v, want both channel-wide posts", ids)
	}
}

// Agents need the matching manifest grant in the channel to post a scoped
// message. Each grant allows exactly its own case and each refusal is audited
// once.
func TestScopedPostAgentGrants(t *testing.T) {
	f := newScopedFixture(t)
	type agentSpec struct {
		caps  []schema.Capability
		perms []schema.ChannelPermission
	}
	agents := map[string]agentSpec{
		"gnet":       {nil, []schema.ChannelPermission{schema.ChannelPermissionPostNet}},
		"gwhisper":   {nil, []schema.ChannelPermission{schema.ChannelPermissionWhisper}},
		"gboth":      {nil, []schema.ChannelPermission{schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent}},
		"gagentonly": {nil, []schema.ChannelPermission{schema.ChannelPermissionWhisperAgent}},
		"gpost":      {nil, []schema.ChannelPermission{schema.ChannelPermissionPost}},
		"gnone":      {nil, []schema.ChannelPermission{schema.ChannelPermissionRead}},
		"gnocap":     {[]schema.Capability{schema.CapabilityMessagesRead}, []schema.ChannelPermission{schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent}},
	}
	for name, spec := range agents {
		f.add(t, name, store.PrincipalAgent, true)
		f.manifest(t, name, spec.caps, spec.perms...)
		// Every agent is on net1 as a member so the net itself never refuses.
		f.seat(t, "net1", name, schema.NetRoleMember)
	}
	netBody := `{"body":"x",` + netAudience(f.net1.ID) + `}`
	humanWhisper := `{"body":"x",` + whisperAudience(f.p("wes").ID) + `}`
	agentWhisper := `{"body":"x",` + whisperAudience(f.p("aria").ID) + `}`
	mixedWhisper := `{"body":"x",` + whisperAudience(f.p("wes").ID, f.p("aria").ID) + `}`
	wide := `{"body":"x"}`

	tests := []struct {
		name  string
		who   string
		body  string
		allow bool
	}{
		{"post_net allows a net post", "gnet", netBody, true},
		{"whisper does not allow a net post", "gwhisper", netBody, false},
		{"channel-wide post does not allow a net post", "gpost", netBody, false},
		{"no scoped grant does not allow a net post", "gnone", netBody, false},
		{"post_net alone does not allow a channel-wide post", "gnet", wide, false},
		{"the capability is needed too", "gnocap", netBody, false},

		{"whisper allows a whisper to humans", "gwhisper", humanWhisper, true},
		{"post_net does not allow a whisper", "gnet", humanWhisper, false},
		{"channel-wide post does not allow a whisper", "gpost", humanWhisper, false},
		{"whisper alone does not allow a whisper to an agent", "gwhisper", agentWhisper, false},
		{"whisper alone does not allow a mixed whisper", "gwhisper", mixedWhisper, false},
		{"whisper_agent alone does not allow a whisper", "gagentonly", agentWhisper, false},
		{"whisper and whisper_agent allow a whisper to an agent", "gboth", agentWhisper, true},
		{"whisper and whisper_agent allow a mixed whisper", "gboth", mixedWhisper, true},
		{"whisper_agent is not needed for humans", "gboth", humanWhisper, true},

		{"post allows a channel-wide v2 post", "gpost", wide, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			denials := func() int { return countAction(f.audit(t), "access_denied") }
			beforeDenials, beforeMsgs := denials(), f.messageCount(t)
			got := f.postV2(t, tt.who, tt.body)
			wantStatus, wantDenials, wantMsgs := http.StatusCreated, 0, 1
			if !tt.allow {
				wantStatus, wantDenials, wantMsgs = http.StatusForbidden, 1, 0
			}
			if got.status != wantStatus {
				t.Fatalf("status = %d, want %d; body %s", got.status, wantStatus, got.body)
			}
			if n := denials() - beforeDenials; n != wantDenials {
				t.Errorf("access_denied events = %d, want %d", n, wantDenials)
			}
			if n := f.messageCount(t) - beforeMsgs; n != wantMsgs {
				t.Errorf("messages stored = %d, want %d", n, wantMsgs)
			}
		})
	}
	// A refusal is a manifest denial like any other: actor, subject, target.
	var last store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" {
			last = e
		}
	}
	if !strings.HasPrefix(last.Actor, "principal:") || last.Subject != "POST /v2/channels/{channel}/messages" || !strings.Contains(last.Detail, fmt.Sprintf("target=channel:%d", f.ops.ID)) {
		t.Errorf("denial audit = %+v", last)
	}
}

// Each scoped post writes one message_scoped event in the insert transaction
// naming the recipients; a channel-wide post writes none; no body appears in
// the audit log.
func TestScopedPostAudit(t *testing.T) {
	f := newScopedFixture(t)
	wide := f.mustPost(t, "olga", `{"body":"SECRET-wide"}`)
	net := f.mustPost(t, "ann", `{"body":"SECRET-net",`+netAudience(f.net1.ID)+`}`)
	whisper := f.mustPost(t, "olga", `{"body":"SECRET-whisper",`+whisperAudience(f.p("wes").ID)+`}`)

	scoped := map[string]store.AuditEvent{}
	for _, e := range f.audit(t) {
		if strings.Contains(e.Actor+e.Subject+e.Detail, "SECRET") {
			t.Errorf("audit event carries a message body: %+v", e)
		}
		if e.Action == store.AuditMessageScoped {
			scoped[e.Subject] = e
		}
	}
	if len(scoped) != 2 {
		t.Fatalf("message_scoped events = %d, want 2", len(scoped))
	}
	if _, ok := scoped[fmt.Sprintf("message:%d", wide)]; ok {
		t.Error("a channel-wide post wrote message_scoped")
	}
	ids := func(names ...string) string {
		var all []int64
		for _, n := range names {
			all = append(all, f.p(n).ID)
		}
		parts := []string{}
		for _, id := range sortedIDs(all...) {
			parts = append(parts, fmt.Sprint(id))
		}
		return strings.Join(parts, ",")
	}
	wantNet := fmt.Sprintf("channel=%d audience=net net=%d recipients=%s", f.ops.ID, f.net1.ID, ids("ann", "nina", "mona", "aria", "dina"))
	wantWhisper := fmt.Sprintf("channel=%d audience=principals recipients=%s", f.ops.ID, ids("olga", "wes"))
	if e := scoped[fmt.Sprintf("message:%d", net)]; e.Detail != wantNet || e.Actor != fmt.Sprintf("principal:%d", f.p("ann").ID) {
		t.Errorf("net audit = %+v, want detail %q", e, wantNet)
	}
	if e := scoped[fmt.Sprintf("message:%d", whisper)]; e.Detail != wantWhisper || e.Actor != fmt.Sprintf("principal:%d", f.p("olga").ID) {
		t.Errorf("whisper audit = %+v, want detail %q", e, wantWhisper)
	}
}

// Joining a net later reveals no history, leaving it hides nothing received,
// and being removed from the channel hides everything, through the real
// endpoints.
func TestScopedSnapshotThroughTheAPI(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	read := func(who string) []int64 {
		t.Helper()
		res := f.callREST(t, "GET", "/v2/channels/ops/messages", f.p(who).token, "")
		if res.status != http.StatusOK {
			t.Fatalf("%s list = %d %s", who, res.status, res.body)
		}
		ids, _ := listIDs(t, res.body)
		return ids
	}
	before := f.mustPost(t, "ann", `{"body":"before",`+netAudience(f.net1.ID)+`}`)
	f.seat(t, "net1", "olga", schema.NetRoleMonitor)
	after := f.mustPost(t, "ann", `{"body":"after",`+netAudience(f.net1.ID)+`}`)
	if got, want := read("olga"), []int64{after}; !reflect.DeepEqual(got, want) {
		t.Errorf("late joiner reads %v, want %v", got, want)
	}

	if _, err := f.srv.store.RemoveNetMember(ctx, "system", f.ops.ID, "net1", f.p("nina").ID); err != nil {
		t.Fatal(err)
	}
	later := f.mustPost(t, "ann", `{"body":"later",`+netAudience(f.net1.ID)+`}`)
	if got, want := read("nina"), []int64{before, after}; !reflect.DeepEqual(got, want) {
		t.Errorf("leaver reads %v, want %v (and not %d)", got, want, later)
	}

	// Removal from the channel: sockets close and nothing is readable.
	sock := openSocket(t, f.base, "/v2/ws?channel=ops", f.p("mona").token)
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("mona's socket = %d", sock.status)
	}
	if got := read("mona"); !reflect.DeepEqual(got, []int64{before, after, later}) {
		t.Fatalf("mona reads %v before removal", got)
	}
	if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.p("mona").ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	res := f.callREST(t, "GET", "/v2/channels/ops/messages", f.p("mona").token, "")
	if res.status != http.StatusNotFound {
		t.Errorf("removed member list = %d %s, want 404", res.status, res.body)
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := sock.conn.Read(ctx2); err == nil {
		t.Error("removed member's socket still delivers")
	}
	// A message posted after the removal is not delivered to anyone removed.
	f.mustPost(t, "ann", `{"body":"post-removal",`+netAudience(f.net1.ID)+`}`)
}

// next_after pages through interleaved scoped and channel-wide messages on all
// three list versions: a reader steps over messages it cannot see without
// skipping visible ones or looping.
func TestScopedListPaging(t *testing.T) {
	f := newScopedFixture(t)
	ids := f.postScopedCorpus(t)
	// More traffic, so the pages are not all one message.
	for i := 0; i < 4; i++ {
		f.mustPost(t, "olga", fmt.Sprintf(`{"body":"extra wide %d"}`, i))
		f.mustPost(t, "ann", `{"body":"extra net",`+netAudience(f.net1.ID)+`}`)
		f.mustPost(t, "ann", `{"body":"extra whisper",`+whisperAudience(f.p("wes").ID)+`}`)
	}
	for _, version := range []string{"v0", "v1", "v2"} {
		for _, who := range []string{"nina", "wes", "olga", "root"} {
			for _, limit := range []int{1, 2, 3, 7, 100} {
				t.Run(fmt.Sprintf("%s/%s/limit %d", version, who, limit), func(t *testing.T) {
					tok := f.p(who).token
					fullPath := "/" + version + "/channels/ops/messages?limit=100"
					want, next := listIDs(t, f.callREST(t, "GET", fullPath, tok, "").body)
					if next != 0 {
						t.Fatalf("a 100-limit page still has next_after %d", next)
					}
					var got []int64
					after := int64(0)
					for pages := 0; ; pages++ {
						if pages > 200 {
							t.Fatal("paging did not terminate")
						}
						path := fmt.Sprintf("/%s/channels/ops/messages?limit=%d&after=%d", version, limit, after)
						res := f.callREST(t, "GET", path, tok, "")
						if res.status != http.StatusOK {
							t.Fatalf("%s = %d %s", path, res.status, res.body)
						}
						page, nextAfter := listIDs(t, res.body)
						got = append(got, page...)
						if nextAfter == 0 {
							break
						}
						if len(page) == 0 || nextAfter != page[len(page)-1] {
							t.Fatalf("next_after %d does not follow page %v", nextAfter, page)
						}
						after = nextAfter
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("paged %v, want %v", got, want)
					}
					if version != "v2" {
						for _, id := range got {
							for _, l := range []string{msgNet1, msgNet2, msgWhisper1, msgWhisper2} {
								if ids[l] == id {
									t.Errorf("%s page contains scoped message %s", version, l)
								}
							}
						}
					}
				})
			}
		}
	}
}

// With authentication off there is no verified author or reader: a scoped post
// is refused, and every read returns channel-wide messages only.
func TestScopedMessagesAuthOff(t *testing.T) {
	ctx := context.Background()
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff})
	base := wsTestServer(t, srv)
	ch, err := srv.store.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) store.Principal {
		p, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := mk("a"), mk("b")
	n, err := srv.store.CreateNet(ctx, "system", ch.ID, "n", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []store.Principal{a, b} {
		if _, err := srv.store.PutNetMember(ctx, "system", ch.ID, "n", p.ID, schema.NetRoleMember, 0); err != nil {
			t.Fatal(err)
		}
	}
	do := func(method, path, body string) wireResult {
		t.Helper()
		f := &authFixture{srv: srv}
		return f.callREST(t, method, path, "", body)
	}

	tests := []struct {
		name string
		body string
	}{
		{"net post", fmt.Sprintf(`{"author_id":%d,"body":"x","audience":{"kind":"net","net_id":%d}}`, a.ID, n.ID)},
		{"whisper", fmt.Sprintf(`{"author_id":%d,"body":"x","audience":{"kind":"principals","principal_ids":[%d]}}`, a.ID, b.ID)},
		{"whisper without an author", fmt.Sprintf(`{"body":"x","audience":{"kind":"principals","principal_ids":[%d]}}`, b.ID)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := do("POST", "/v2/channels/ops/messages", tt.body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("status = %d %s, want 400", res.status, res.body)
			}
			var e schema.Error
			if err := json.Unmarshal([]byte(res.body), &e); err != nil || e.Code != "audience_requires_auth" {
				t.Errorf("body = %s, want audience_requires_auth", res.body)
			}
			if got, _ := srv.store.CountMessages(ctx, ch.ID); got != 0 {
				t.Errorf("a refused post stored %d messages", got)
			}
		})
	}

	// A channel-wide v2 post still works.
	if res := do("POST", "/v2/channels/ops/messages", fmt.Sprintf(`{"author_id":%d,"body":"open"}`, a.ID)); res.status != http.StatusCreated {
		t.Fatalf("channel-wide v2 post = %d %s", res.status, res.body)
	}
	sock := openSocket(t, base, "/v2/ws?channel=ops", "")
	if sock.status != http.StatusSwitchingProtocols {
		t.Fatalf("v2 socket = %d", sock.status)
	}
	// A scoped message that exists (made by an identified path, as MCP will
	// in #117) is neither listed nor delivered to anyone unidentified.
	m, rec, err := srv.store.InsertScopedMessage(ctx, store.ScopedPost{
		ChannelID: ch.ID, AuthorID: a.ID, Body: "SECRET", Audience: schema.Audience{Kind: schema.AudienceKindNet, NetID: n.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.hub.BroadcastMessageV2(ctx, messageV2FromStore(m), rec)
	sentinel := func() int64 {
		res := do("POST", "/v1/channels/ops/messages", fmt.Sprintf(`{"author_id":%d,"body":"after"}`, a.ID))
		var resp schema.PostMessageResponseV1
		if err := json.Unmarshal([]byte(res.body), &resp); err != nil || res.status != http.StatusCreated {
			t.Fatalf("sentinel = %d %s", res.status, res.body)
		}
		return resp.Message.ID
	}()
	for _, raw := range sock.readUntil(t, sentinel) {
		if bytes.Contains(raw, []byte("SECRET")) {
			t.Errorf("an unidentified socket received a scoped message: %s", raw)
		}
	}
	for _, version := range []string{"v0", "v1", "v2"} {
		res := do("GET", "/"+version+"/channels/ops/messages", "")
		if res.status != http.StatusOK || strings.Contains(res.body, "SECRET") {
			t.Errorf("%s list = %d %s", version, res.status, res.body)
		}
		if ids, _ := listIDs(t, res.body); len(ids) != 2 {
			t.Errorf("%s list = %v, want the 2 channel-wide messages", version, ids)
		}
	}
}
