package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Reactions over REST, MCP and the events socket (issue #765,
// docs/design/message-reactions.md §14), on the scoped fixture: channel "ops"
// with people on nets, a monitor, agents and a stranger.

const (
	thumbsUp = "\U0001F44D"
	partyKey = "\U0001F389"
)

type reactionWorld struct {
	*scopedFixture
	cache              map[string]int64
	wide, net, whisper int64 // olga's post; ann's post to net1; ann's whisper to wes
}

func newReactionWorld(t *testing.T) *reactionWorld {
	t.Helper()
	f := newScopedFixture(t)
	w := &reactionWorld{scopedFixture: f}
	w.wide = f.mustPost(t, "olga", `{"body":"SECRET-wide"}`)
	w.net = f.mustPost(t, "ann", `{"body":"SECRET-net",`+netAudience(f.net1.ID)+`}`)
	w.whisper = f.mustPost(t, "ann", `{"body":"SECRET-whisper",`+whisperAudience(f.p("wes").ID)+`}`)
	return w
}

func rpath(channel string, msg int64, emoji string) string {
	return fmt.Sprintf("/v2/channels/%s/messages/%d/reactions/%s", channel, msg, url.PathEscape(emoji))
}

func (w *reactionWorld) react(t *testing.T, method, who, channel string, msg int64, emoji string) wireResult {
	t.Helper()
	return w.callREST(t, method, rpath(channel, msg, emoji), w.p(who).token, "")
}

func decodeSummary(t *testing.T, res wireResult) schema.ReactionSummaryV1 {
	t.Helper()
	var s schema.ReactionSummaryV1
	if err := json.Unmarshal([]byte(res.body), &s); err != nil {
		t.Fatalf("decode summary %q: %v", res.body, err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("summary %s is invalid: %v", res.body, err)
	}
	return s
}

func reactErrCode(res wireResult) string {
	var e schema.Error
	_ = json.Unmarshal([]byte(res.body), &e)
	return e.Code
}

// The lifecycle on the wire, W1/W3, with a non-canonical spelling and the
// error rows that are the caller's mistakes.
func TestReactionRESTLifecycle(t *testing.T) {
	w := newReactionWorld(t)
	res := w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp)
	if res.status != 200 {
		t.Fatalf("set = %d %s", res.status, res.body)
	}
	s := decodeSummary(t, res)
	if len(s.Reactions) != 1 || s.Reactions[0].Count != 1 || !s.Reactions[0].Me || s.MessageID != w.wide || s.ChannelID != w.ops.ID {
		t.Fatalf("summary = %+v", s)
	}
	// Two sessions of one person: the second is a no-op that answers the same.
	again := decodeSummary(t, w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp))
	if again.Reactions[0].Count != 1 {
		t.Errorf("a repeat counted twice: %+v", again)
	}
	// Another reader sees the count, not as theirs.
	other := decodeSummary(t, w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", w.wide), w.p("nina").token, ""))
	if other.Reactions[0].Count != 1 || other.Reactions[0].Me {
		t.Errorf("another reader's summary = %+v", other)
	}
	// The unqualified spelling of a heart is the same key as the qualified one.
	h1 := decodeSummary(t, w.react(t, "PUT", "ann", "ops", w.wide, "❤"))
	h2 := decodeSummary(t, w.react(t, "PUT", "nina", "ops", w.wide, "❤️"))
	// Found by key, not position: two reactions made in the same millisecond
	// are ordered by their keys, and U+2764 sorts before U+1F44D.
	var heart *schema.ReactionCountV1
	for i := range h2.Reactions {
		if h2.Reactions[i].Emoji == "❤️" {
			heart = &h2.Reactions[i]
		}
	}
	if heart == nil || heart.Count != 2 || len(h2.Reactions) != 2 || len(h1.Reactions) != 2 {
		t.Errorf("hearts = %+v after %+v", h2, h1)
	}
	// The batch read and the reactor list.
	batch := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/reactions?messages=%d,%d,999999", w.wide, w.net), w.p("olga").token, "")
	var br schema.ReactionSummariesResponseV1
	if err := json.Unmarshal([]byte(batch.body), &br); err != nil || br.Validate() != nil || len(br.Summaries) != 1 || br.Summaries[0].MessageID != w.wide {
		t.Errorf("batch = %d %s", batch.status, batch.body)
	}
	lr := w.callREST(t, "GET", rpath("ops", w.wide, thumbsUp)+"?limit=1", w.p("olga").token, "")
	var reactors schema.ListReactorsResponseV1
	if err := json.Unmarshal([]byte(lr.body), &reactors); err != nil || reactors.Validate() != nil || len(reactors.Reactors) != 1 || reactors.Reactors[0].PrincipalID != w.p("ann").ID {
		t.Errorf("reactors = %d %s", lr.status, lr.body)
	}
	// Remove, and remove again.
	for range 2 {
		if gone := decodeSummary(t, w.react(t, "DELETE", "ann", "ops", w.wide, thumbsUp)); len(gone.Reactions) != 1 {
			t.Errorf("after removing: %+v", gone)
		}
	}
	// The caller's mistakes.
	for _, tt := range []struct {
		name, method, path string
		status             int
		code               string
	}{
		{"not an emoji", "PUT", rpath("ops", w.wide, "x"), 400, "invalid_reaction"},
		{"two emoji", "PUT", rpath("ops", w.wide, thumbsUp+thumbsUp), 400, "invalid_reaction"},
		{"bad cursor", "GET", rpath("ops", w.wide, thumbsUp) + "?after=-1", 400, "invalid_request"},
		{"bad limit", "GET", rpath("ops", w.wide, thumbsUp) + "?limit=101", 400, "invalid_request"},
		{"zero limit", "GET", rpath("ops", w.wide, thumbsUp) + "?limit=0", 400, "invalid_request"},
		{"no ids", "GET", "/v2/channels/ops/reactions", 400, "invalid_request"},
		{"a repeated id", "GET", fmt.Sprintf("/v2/channels/ops/reactions?messages=%d,%d", w.wide, w.wide), 400, "invalid_request"},
		{"a non-positive id", "GET", "/v2/channels/ops/reactions?messages=0", 400, "invalid_request"},
		{"a ghost message", "PUT", rpath("ops", 99999, thumbsUp), 404, "message_not_found"},
		{"a non-numeric message", "PUT", "/v2/channels/ops/messages/abc/reactions/" + url.PathEscape(thumbsUp), 404, "message_not_found"},
	} {
		res := w.callREST(t, tt.method, tt.path, w.p("ann").token, "")
		if res.status != tt.status || reactErrCode(res) != tt.code {
			t.Errorf("%s = %d %s, want %d %s", tt.name, res.status, res.body, tt.status, tt.code)
		}
	}
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint(i + 1)
	}
	if res := w.callREST(t, "GET", "/v2/channels/ops/reactions?messages="+strings.Join(tooMany, ","), w.p("ann").token, ""); res.status != 400 {
		t.Errorf("101 ids = %d", res.status)
	}
	// The v1 and v0 sockets and lists are not touched: a message list carries no reaction.
	list := w.callREST(t, "GET", "/v2/channels/ops/messages", w.p("olga").token, "")
	if strings.Contains(list.body, "reaction") {
		t.Errorf("the message list mentions reactions: %s", list.body)
	}
}

// V1 and V2: a stranger to the channel (a person, an agent, and an operator
// who is not a member) gets exactly an unknown channel's answer on all seven
// routes, and a message that is not theirs is exactly a ghost's, for the
// whisper they are not in.
func TestReactionChannelGate(t *testing.T) {
	w := newReactionWorld(t)
	w.add(t, "op2", store.PrincipalHuman, false)
	if _, err := w.srv.store.SetRole(context.Background(), "system", w.root.ID, w.p("op2").ID, store.RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	w.react(t, "PUT", "wes", "ops", w.whisper, thumbsUp)
	routes := func(ch string, msg int64) []struct{ method, path string } {
		return []struct{ method, path string }{
			{"PUT", rpath(ch, msg, thumbsUp)},
			{"DELETE", rpath(ch, msg, thumbsUp)},
			{"GET", fmt.Sprintf("/v2/channels/%s/messages/%d/reactions", ch, msg)},
			{"GET", rpath(ch, msg, thumbsUp)},
			{"GET", fmt.Sprintf("/v2/channels/%s/reactions?messages=%d", ch, msg)},
			{"DELETE", rpath(ch, msg, thumbsUp) + "/" + fmt.Sprint(w.p("wes").ID)},
			{"DELETE", fmt.Sprintf("/v2/channels/%s/messages/%d/reactors/%d", ch, msg, w.p("wes").ID)},
		}
	}
	for _, who := range []string{"nora", "nomad", "op2"} {
		for i, r := range routes("ops", w.wide) {
			got := w.callREST(t, r.method, r.path, w.p(who).token, "")
			want := w.callREST(t, r.method, routes("nosuch", w.wide)[i].path, w.p(who).token, "")
			if got != want || got.status != 404 && got.status != 403 {
				t.Errorf("%s %s %s: %+v, an unknown channel gives %+v", who, r.method, r.path, got, want)
			}
			if got.status == 404 && reactErrCode(got) != "channel_not_found" {
				t.Errorf("%s %s: code %s", who, r.path, reactErrCode(got))
			}
		}
	}
	// V2: olga is in the channel but not the whisper.
	for i, r := range routes("ops", w.whisper)[:5] {
		got := w.callREST(t, r.method, r.path, w.p("olga").token, "")
		ghost := w.callREST(t, r.method, routes("ops", 999999)[i].path, w.p("olga").token, "")
		if r.path[len(r.path)-8:] == "messages" || strings.Contains(r.path, "/reactions?messages=") {
			// The batch read omits: an empty list for the whisper and the ghost alike.
			if got.status != 200 || strings.Contains(got.body, "wes") || !strings.Contains(got.body, `"summaries":[]`) || got.body != ghost.body && strings.ReplaceAll(got.body, fmt.Sprint(w.whisper), "") != strings.ReplaceAll(ghost.body, "999999", "") {
				t.Errorf("batch for a whisper = %d %s, ghost %d %s", got.status, got.body, ghost.status, ghost.body)
			}
			continue
		}
		if got.status != 404 || reactErrCode(got) != "message_not_found" || got.body != ghost.body {
			t.Errorf("%s %s: %+v, a ghost gives %+v", r.method, r.path, got, ghost)
		}
	}
	// The same through the channel of another: the message is not that channel's.
	other, err := w.srv.store.CreateChannel(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.store.AddChannelMember(context.Background(), "system", other.ID, w.p("olga").ID, 0); err != nil {
		t.Fatal(err)
	}
	if res := w.react(t, "PUT", "olga", "other", w.wide, thumbsUp); res.status != 404 || reactErrCode(res) != "message_not_found" {
		t.Errorf("a message through another channel = %d %s", res.status, res.body)
	}
	// No marker of the whisper anywhere in what a stranger was told.
	for _, r := range routes("ops", w.whisper) {
		if res := w.callREST(t, r.method, r.path, w.p("olga").token, ""); strings.Contains(res.body, "SECRET") || strings.Contains(res.body, "wes") {
			t.Errorf("%s %s leaked: %s", r.method, r.path, res.body)
		}
	}
}

// V3 to V5: nets and whispers on the wire.
func TestReactionNetsAndWhispers(t *testing.T) {
	w := newReactionWorld(t)
	for _, tt := range []struct {
		name, method, who string
		msg               int64
		status            int
		code              string
	}{
		{"net member sets", "PUT", "nina", w.net, 200, ""},
		{"monitor reads", "GET", "mona", w.net, 200, ""},
		{"monitor may not set", "PUT", "mona", w.net, 403, "reaction_not_permitted"},
		{"monitor may remove its own", "DELETE", "mona", w.net, 200, ""},
		{"not on the net", "PUT", "olga", w.net, 404, "message_not_found"},
		{"the whisper's recipient sets", "PUT", "wes", w.whisper, 200, ""},
		{"a stranger to the whisper", "PUT", "olga", w.whisper, 404, "message_not_found"},
	} {
		path := rpath("ops", tt.msg, thumbsUp)
		if tt.method == "GET" {
			path = fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", tt.msg)
		}
		res := w.callREST(t, tt.method, path, w.p(tt.who).token, "")
		if res.status != tt.status || tt.code != "" && reactErrCode(res) != tt.code {
			t.Errorf("%s = %d %s", tt.name, res.status, res.body)
		}
	}
	// Taken off the net since: still reads, may not set, may take back.
	w.react(t, "PUT", "dina", "ops", w.net, partyKey)
	if _, err := w.srv.store.RemoveNetMember(context.Background(), "system", w.ops.ID, "net1", w.p("dina").ID); err != nil {
		t.Fatal(err)
	}
	if res := w.react(t, "PUT", "dina", "ops", w.net, thumbsUp); res.status != 403 || reactErrCode(res) != "reaction_not_permitted" {
		t.Errorf("set after leaving the net = %d %s", res.status, res.body)
	}
	if res := w.react(t, "DELETE", "dina", "ops", w.net, partyKey); res.status != 200 {
		t.Errorf("remove after leaving the net = %d %s", res.status, res.body)
	}
}

// V9 is TestAuthOffUnchanged; here the other half of the order: a stranger
// takes no token, a refused write by a member does (W6, W6b).
func TestReactionRateLimit(t *testing.T) {
	w := newReactionWorld(t)
	w.srv.setReactionLimit(inviteLimit{name: "test", burst: 4, window: time.Hour})
	// A channel_not_found before the gate passes takes none.
	for range 10 {
		if res := w.react(t, "PUT", "nora", "ops", w.wide, thumbsUp); res.status != 404 {
			t.Fatalf("stranger = %d", res.status)
		}
		if res := w.react(t, "PUT", "ann", "nosuch", w.wide, thumbsUp); res.status != 404 {
			t.Fatalf("unknown channel = %d", res.status)
		}
	}
	// Four refusals, each of a different kind, each took a token.
	refused := []struct {
		who   string
		msg   int64
		emoji string
		code  string
	}{
		{"ann", w.wide, "x", "invalid_reaction"},
		{"ann", 99999, thumbsUp, "message_not_found"},
		{"mona", w.net, thumbsUp, "reaction_not_permitted"},
		{"ann", w.whisper, thumbsUp, ""},
	}
	// Spend ann's four: the last is a real, allowed write.
	for _, r := range refused[:2] {
		if res := w.react(t, "PUT", r.who, "ops", r.msg, r.emoji); reactErrCode(res) != r.code {
			t.Errorf("%+v = %s", r, res.body)
		}
	}
	w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp)
	w.react(t, "DELETE", "ann", "ops", w.wide, thumbsUp)
	res := w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp)
	if res.status != 429 || reactErrCode(res) != "rate_limited" {
		t.Fatalf("fifth write = %d %s, want 429", res.status, res.body)
	}
	var e schema.Error
	_ = json.Unmarshal([]byte(res.body), &e)
	if e.RetryAfterSeconds < 1 {
		t.Errorf("retry_after_seconds = %d", e.RetryAfterSeconds)
	}
	// A 429 spends nothing, and reads are not limited.
	if res := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", w.wide), w.p("ann").token, ""); res.status != 200 {
		t.Errorf("a read while limited = %d", res.status)
	}
	// Mona's refusal for the monitor seat takes a token of hers.
	for range 4 {
		w.react(t, "PUT", "mona", "ops", w.net, thumbsUp)
	}
	if res := w.react(t, "PUT", "mona", "ops", w.net, thumbsUp); res.status != 429 {
		t.Errorf("refused writes did not spend tokens: %d %s", res.status, res.body)
	}
	// Another person's bucket is their own.
	if res := w.react(t, "PUT", "nina", "ops", w.wide, thumbsUp); res.status != 200 {
		t.Errorf("another person = %d %s", res.status, res.body)
	}
}

func (w *reactionWorld) auditDetails(t *testing.T, action string) []string {
	t.Helper()
	var out []string
	for _, e := range w.audit(t) {
		if e.Action == action {
			out = append(out, e.Detail)
		}
	}
	return out
}

// V7, V7b, V8: an agent reacts with the permission it would need to reply in
// that audience; every refusal is audited; the budget is spent once.
func TestReactionAgents(t *testing.T) {
	w := newReactionWorld(t)
	read, post, postNet, whisper := schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper
	tests := []struct {
		name  string
		perms []schema.ChannelPermission
		msg   func() int64
		want  int
	}{
		{"post reacts channel-wide", []schema.ChannelPermission{read, post}, func() int64 { return w.wide }, 200},
		{"post alone is not post_net", []schema.ChannelPermission{read, post}, func() int64 { return w.net }, 403},
		{"post_net alone reacts on a net, as it may reply there", []schema.ChannelPermission{read, postNet}, func() int64 { return w.net }, 200},
		{"post_net alone is not post", []schema.ChannelPermission{read, postNet}, func() int64 { return w.wide }, 403},
		{"read only reacts nowhere", []schema.ChannelPermission{read}, func() int64 { return w.wide }, 403},
		{"a whisper needs whisper", []schema.ChannelPermission{read, post, postNet}, func() int64 { return w.whisperToAria(t) }, 403},
		{"whisper reacts in a whisper", []schema.ChannelPermission{read, whisper}, func() int64 { return w.whisperToAria(t) }, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w.manifest(t, "aria", nil, tt.perms...)
			before := len(w.auditDetails(t, "access_denied"))
			rows := countReactionRows(t, w.srv)
			res := w.callREST(t, "PUT", rpath("ops", tt.msg(), thumbsUp), w.p("aria").token, "")
			if res.status != tt.want {
				t.Fatalf("PUT = %d %s, want %d", res.status, res.body, tt.want)
			}
			if tt.want == 403 {
				if reactErrCode(res) != "forbidden" || countReactionRows(t, w.srv) != rows {
					t.Errorf("refusal %s stored rows %d -> %d", res.body, rows, countReactionRows(t, w.srv))
				}
				if len(w.auditDetails(t, "access_denied")) != before+1 {
					t.Error("the refusal was not audited once")
				}
			}
			// The same through MCP.
			out := w.mcpCall(t, w.p("aria").token, "set_reaction", map[string]any{"channel": "ops", "message_id": tt.msg(), "emoji": thumbsUp})
			if (tt.want == 200) != (out.result() == "ok") || tt.want == 403 && out.result() != "forbidden" {
				t.Errorf("set_reaction = %s, REST said %d", out.result(), tt.want)
			}
		})
	}
	// V8 and a read-only agent that is not in the channel.
	if res := w.callREST(t, "PUT", rpath("ops", w.wide, thumbsUp), w.p("nomad").token, ""); res.status != 404 || reactErrCode(res) != "channel_not_found" {
		t.Errorf("an agent outside the channel = %d %s", res.status, res.body)
	}
	if out := w.mcpCall(t, w.p("nomad").token, "set_reaction", map[string]any{"channel": "ops", "message_id": w.wide, "emoji": thumbsUp}); out.result() != "channel_not_found" {
		t.Errorf("mcp outside the channel = %s", out.result())
	}
	// An agent that may post but not read may not read reactions.
	if res := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", w.wide), w.p("arlo").token, ""); res.status != 403 {
		t.Errorf("a write-only agent's read = %d", res.status)
	}
	// Reads succeed for a reading agent that cannot write.
	w.manifest(t, "aria", nil, read)
	if res := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", w.wide), w.p("aria").token, ""); res.status != 200 {
		t.Errorf("a reading agent's read = %d %s", res.status, res.body)
	}
	if got := w.auditDetails(t, "access_denied"); len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), "audience_not_granted") {
		t.Errorf("no audience_not_granted denial in %v", got)
	}
}

// whisperToAria posts (once) a whisper from ann to aria.
func (w *reactionWorld) whisperToAria(t *testing.T) int64 {
	t.Helper()
	if id, ok := w.cache["wa"]; ok {
		return id
	}
	if w.cache == nil {
		w.cache = map[string]int64{}
	}
	w.cache["wa"] = w.mustPost(t, "ann", `{"body":"SECRET-aria",`+whisperAudience(w.p("aria").ID)+`}`)
	return w.cache["wa"]
}

func countReactionRows(t *testing.T, srv *Server) int {
	t.Helper()
	n, err := srv.store.CountReactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// An agent's budget is spent once per reaction call, in the audience step.
func TestReactionAgentBudgetSpentOnce(t *testing.T) {
	w := newReactionWorld(t)
	req := schema.PutAgentManifestRequestV1{DisplayName: "aria", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels:   []schema.ChannelGrant{{ChannelID: w.ops.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet}}},
		RateLimits: []schema.RateLimit{{Capability: schema.CapabilityMessagesPost, Max: 3, WindowSeconds: 3600}}}
	if _, _, err := w.srv.store.PutAgentManifest(context.Background(), "system", w.p("aria").ID, req); err != nil {
		t.Fatal(err)
	}
	// Three calls (REST, on a channel-wide message and a net message, and MCP)
	// each cost one; the fourth is limited. Were a call to cost two, it would
	// have been limited sooner.
	for i, call := range []func() string{
		func() string { return fmt.Sprint(w.react(t, "PUT", "aria", "ops", w.wide, thumbsUp).status) },
		func() string { return fmt.Sprint(w.react(t, "PUT", "aria", "ops", w.net, thumbsUp).status) },
		func() string {
			return w.mcpCall(t, w.p("aria").token, "remove_reaction", map[string]any{"channel": "ops", "message_id": w.net, "emoji": thumbsUp}).result()
		},
	} {
		if got := call(); got != "200" && got != "ok" {
			t.Fatalf("call %d = %s", i, got)
		}
	}
	if res := w.react(t, "PUT", "aria", "ops", w.wide, partyKey); res.status != 429 {
		t.Errorf("fourth call = %d %s, want 429", res.status, res.body)
	}
}

// P1: REST and MCP answer alike for every error row.
func TestReactionRESTMCPParity(t *testing.T) {
	w := newReactionWorld(t)
	w.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet)
	token := w.p("aria").token
	wa := w.whisperToAria(t)
	_ = wa
	cases := []struct {
		name         string
		channel      string
		msg          int64
		emoji        string
		wantStatus   int
		wantCode     string
		invisibleMsg bool
	}{
		{"ok", "ops", w.wide, thumbsUp, 200, "", false},
		{"invalid emoji", "ops", w.wide, "nope", 400, "invalid_reaction", false},
		{"ghost message", "ops", 99999, thumbsUp, 404, "message_not_found", false},
		{"a whisper aria is not in", "ops", w.whisper, thumbsUp, 404, "message_not_found", false},
		{"unknown channel", "nosuch", w.wide, thumbsUp, 404, "channel_not_found", false},
	}
	for _, c := range cases {
		rest := w.callREST(t, "PUT", rpath(c.channel, c.msg, c.emoji), token, "")
		mcp := w.mcpCall(t, token, "set_reaction", map[string]any{"channel": c.channel, "message_id": c.msg, "emoji": c.emoji})
		wantMCP := "ok"
		if c.wantCode != "" {
			wantMCP = c.wantCode
		}
		if rest.status != c.wantStatus || reactErrCode(rest) != c.wantCode || mcp.result() != wantMCP {
			t.Errorf("%s: REST %d %s, MCP %s; want %d %q", c.name, rest.status, rest.body, mcp.result(), c.wantStatus, c.wantCode)
		}
		restDel := w.callREST(t, "DELETE", rpath(c.channel, c.msg, c.emoji), token, "")
		mcpDel := w.mcpCall(t, token, "remove_reaction", map[string]any{"channel": c.channel, "message_id": c.msg, "emoji": c.emoji})
		if restDel.status != c.wantStatus || mcpDel.result() != wantMCP {
			t.Errorf("%s remove: REST %d, MCP %s", c.name, restDel.status, mcpDel.result())
		}
	}
	// The reads.
	w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp)
	rest := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/reactions?messages=%d,%d", w.wide, w.whisper), token, "")
	mcp := w.mcpCall(t, token, "read_reactions", map[string]any{"channel": "ops", "message_ids": []int64{w.wide, w.whisper}})
	if rest.status != 200 || mcp.result() != "ok" || !strings.Contains(mcp.body, `\"message_id\":`+fmt.Sprint(w.wide)) && !strings.Contains(mcp.body, `"message_id":`+fmt.Sprint(w.wide)) {
		t.Errorf("batch: REST %d %s, MCP %s %s", rest.status, rest.body, mcp.result(), mcp.body)
	}
	if strings.Contains(mcp.body, fmt.Sprintf(`"message_id":%d`, w.whisper)) {
		t.Errorf("MCP batch named an invisible message: %s", mcp.body)
	}
	for name, args := range map[string]map[string]any{
		"too many":  {"channel": "ops", "message_ids": make([]int64, 101)},
		"none":      {"channel": "ops", "message_ids": []int64{}},
		"duplicate": {"channel": "ops", "message_ids": []int64{w.wide, w.wide}},
	} {
		if got := w.mcpCall(t, token, "read_reactions", args).result(); got != "invalid_request" {
			t.Errorf("read_reactions %s = %s, want invalid_request", name, got)
		}
	}
	list := w.mcpCall(t, token, "list_reactors", map[string]any{"channel": "ops", "message_id": w.wide, "emoji": thumbsUp})
	if list.result() != "ok" || !strings.Contains(list.body, fmt.Sprintf(`"principal_id":%d`, w.p("ann").ID)) {
		t.Errorf("list_reactors = %s %s", list.result(), list.body)
	}
	if got := w.mcpCall(t, token, "list_reactors", map[string]any{"channel": "ops", "message_id": w.whisper, "emoji": thumbsUp}).result(); got != "message_not_found" {
		t.Errorf("list_reactors on a whisper = %s", got)
	}
	if got := w.mcpCall(t, token, "list_reactors", map[string]any{"channel": "ops", "message_id": w.wide, "emoji": thumbsUp, "limit": 101}).result(); got != "invalid_request" {
		t.Errorf("list_reactors limit = %s", got)
	}
	// The tools are on offer.
	var offered []string
	for tool := range schema.MCPToolCapabilities() {
		offered = append(offered, tool)
	}
	for _, name := range []string{"set_reaction", "remove_reaction", "read_reactions", "list_reactors"} {
		if !strings.Contains(strings.Join(offered, ","), name) {
			t.Errorf("%s is not mapped", name)
		}
	}
}

// MD1 and MD2: the moderation routes.
func TestReactionModeration(t *testing.T) {
	w := newReactionWorld(t)
	ctx := context.Background()
	no, yes := false, true
	if _, err := w.srv.store.PutChannelMember(ctx, "system", w.ops.ID, w.p("dina").ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	// A second channel with its own manager, who is not in ops.
	other, err := w.srv.store.CreateChannel(ctx, "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	w.add(t, "mgr2", store.PrincipalHuman, false)
	if _, err := w.srv.store.PutChannelMember(ctx, "system", other.ID, w.p("mgr2").ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	// An operator who is not a member of ops.
	w.add(t, "op2", store.PrincipalHuman, false)
	if _, err := w.srv.store.SetRole(ctx, "system", w.root.ID, w.p("op2").ID, store.RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	w.react(t, "PUT", "olga", "ops", w.wide, thumbsUp)
	w.react(t, "PUT", "olga", "ops", w.wide, partyKey)
	w.react(t, "PUT", "nina", "ops", w.wide, thumbsUp)
	w.react(t, "PUT", "wes", "ops", w.whisper, thumbsUp)
	olga := w.p("olga").ID
	one := func(msg int64, emoji string, target int64) string {
		return fmt.Sprintf("%s/%d", rpath("ops", msg, emoji), target)
	}
	all := func(msg, target int64) string {
		return fmt.Sprintf("/v2/channels/ops/messages/%d/reactors/%d", msg, target)
	}
	// MD2.
	for _, tt := range []struct {
		name, who, path string
		status          int
		code            string
	}{
		{"a plain member", "ann", one(w.wide, thumbsUp, olga), 403, "forbidden"},
		{"an agent", "aria", one(w.wide, thumbsUp, olga), 403, "forbidden"},
		{"a stranger", "nora", one(w.wide, thumbsUp, olga), 404, "channel_not_found"},
		{"an operator who is not a member", "op2", one(w.wide, thumbsUp, olga), 404, "channel_not_found"},
		{"a manager of another channel", "mgr2", one(w.wide, thumbsUp, olga), 404, "channel_not_found"},
		{"the manager on a whisper they are not in", "dina", one(w.whisper, thumbsUp, w.p("wes").ID), 404, "message_not_found"},
		{"the manager on a ghost", "dina", one(99999, thumbsUp, w.p("wes").ID), 404, "message_not_found"},
		{"a bad emoji", "dina", one(w.wide, "x", olga), 400, "invalid_reaction"},
		{"the operator on a whisper they are not in", "root", all(w.whisper, w.p("wes").ID), 404, "message_not_found"},
	} {
		res := w.callREST(t, "DELETE", tt.path, w.p(tt.who).token, "")
		if res.status != tt.status || reactErrCode(res) != tt.code {
			t.Errorf("%s = %d %s, want %d %s", tt.name, res.status, res.body, tt.status, tt.code)
		}
	}
	if a, b := w.callREST(t, "DELETE", one(w.whisper, thumbsUp, 1), w.p("dina").token, ""), w.callREST(t, "DELETE", one(99999, thumbsUp, 1), w.p("dina").token, ""); a.body != b.body {
		t.Errorf("a whisper and a ghost differ: %s / %s", a.body, b.body)
	}
	if got := countReactionRows(t, w.srv); got != 4 {
		t.Fatalf("%d rows after the refusals, want 4", got)
	}
	// MD1: the manager removes one; an operator removes all of a person's.
	res := w.callREST(t, "DELETE", one(w.wide, thumbsUp, olga), w.p("dina").token, "")
	if s := decodeSummary(t, res); len(s.Reactions) != 2 {
		t.Errorf("after removing olga's thumbs up: %+v", s)
	}
	if res := w.callREST(t, "DELETE", one(w.wide, thumbsUp, olga), w.p("dina").token, ""); res.status != 200 {
		t.Errorf("repeat = %d", res.status)
	}
	if res := w.callREST(t, "DELETE", all(w.wide, olga), w.p("root").token, ""); res.status != 200 {
		t.Errorf("remove all = %d %s", res.status, res.body)
	}
	if res := w.callREST(t, "DELETE", all(w.wide, 987654), w.p("root").token, ""); res.status != 200 {
		t.Errorf("unknown target = %d %s", res.status, res.body)
	}
	got := w.auditDetails(t, store.AuditReactionModerated)
	if len(got) != 2 || !strings.Contains(got[1], "all") || !strings.Contains(got[1], "removed=1") {
		t.Errorf("moderation audit = %v", got)
	}
	// The target is still a member, and may react again.
	if res := w.react(t, "PUT", "olga", "ops", w.wide, thumbsUp); res.status != 200 {
		t.Errorf("the target reacting again = %d", res.status)
	}
	// Moderation takes the same token as other writes.
	w.srv.setReactionLimit(inviteLimit{name: "test", burst: 1, window: time.Hour})
	w.callREST(t, "DELETE", one(w.wide, thumbsUp, olga), w.p("dina").token, "")
	if res := w.callREST(t, "DELETE", one(w.wide, thumbsUp, olga), w.p("dina").token, ""); res.status != 429 {
		t.Errorf("a second moderation write = %d, want 429", res.status)
	}
}

// reactionFeed reads an events socket in the background, so that waiting for a
// frame never cancels a read (which closes the socket).
type reactionFeed struct {
	frames chan schema.ReactionSummaryV1
}

var feeds = map[*socket]*reactionFeed{}

func reactionFrames(t *testing.T, s *socket, d time.Duration) []schema.ReactionSummaryV1 {
	t.Helper()
	feed := feeds[s]
	if feed == nil {
		feed = &reactionFeed{frames: make(chan schema.ReactionSummaryV1, 1024)}
		feeds[s] = feed
		t.Cleanup(func() { delete(feeds, s) })
		go func() {
			for {
				var raw json.RawMessage
				if err := wsjson.Read(context.Background(), s.conn, &raw); err != nil {
					return
				}
				var head struct {
					Schema string `json:"schema"`
				}
				_ = json.Unmarshal(raw, &head)
				if head.Schema == schema.ReactionSummarySchemaV1 {
					var sum schema.ReactionSummaryV1
					if json.Unmarshal(raw, &sum) != nil || sum.Validate() != nil {
						return
					}
					feed.frames <- sum
				}
			}
		}()
	}
	var out []schema.ReactionSummaryV1
	timer := time.After(d)
	for {
		select {
		case sum := <-feed.frames:
			out = append(out, sum)
		case <-timer:
			return out
		}
	}
}

// E1, E2, E4: who is sent a frame, and what a reaction does not do.
func TestReactionEvents(t *testing.T) {
	w := newReactionWorld(t)
	w.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet)
	open := func(who string) *socket {
		s := openSocket(t, w.base, "/v1/events/ws?channel=ops", w.p(who).token)
		if s.status != http.StatusSwitchingProtocols {
			t.Fatalf("%s events socket = %d %s", who, s.status, s.body)
		}
		return s
	}
	olga, wes, ann, aria := open("olga"), open("wes"), open("ann"), open("aria")
	v2 := openSocket(t, w.base, "/v2/ws?channel=ops", w.p("olga").token)
	time.Sleep(300 * time.Millisecond) // the first rosters
	reactionFrames(t, olga, 200*time.Millisecond)
	reactionFrames(t, wes, 200*time.Millisecond)
	reactionFrames(t, ann, 200*time.Millisecond)
	reactionFrames(t, aria, 200*time.Millisecond)

	unreadBefore := w.callREST(t, "GET", "/v1/read-state", w.p("olga").token, "").body
	seenBefore := len(w.rec.seen())
	auditBefore := len(w.audit(t))

	// A channel-wide message: everyone, the agent too, the actor's own socket.
	w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp)
	for who, s := range map[string]*socket{"olga": olga, "wes": wes, "ann": ann, "aria": aria} {
		fr := reactionFrames(t, s, 600*time.Millisecond)
		if len(fr) == 0 || fr[len(fr)-1].MessageID != w.wide || fr[len(fr)-1].Reactions[0].Count != 1 {
			t.Errorf("%s got %+v for a channel-wide reaction", who, fr)
		}
		if who == "ann" && !fr[len(fr)-1].Reactions[0].Me {
			t.Errorf("the actor's frame lost me: %+v", fr)
		}
		if who == "olga" && fr[len(fr)-1].Reactions[0].Me {
			t.Errorf("olga's frame says me: %+v", fr)
		}
	}
	// A whisper: only the two in it, and the frame is computed per reader.
	w.react(t, "PUT", "wes", "ops", w.whisper, thumbsUp)
	if fr := reactionFrames(t, olga, 500*time.Millisecond); len(fr) != 0 {
		t.Errorf("a stranger to the whisper got a frame: %+v", fr)
	}
	if fr := reactionFrames(t, aria, 100*time.Millisecond); len(fr) != 0 {
		t.Errorf("an agent outside the whisper got a frame: %+v", fr)
	}
	if fr := reactionFrames(t, ann, 100*time.Millisecond); len(fr) != 1 || fr[0].MessageID != w.whisper {
		t.Errorf("the whisper's author got %+v", fr)
	}
	// A net message: nobody off the net.
	w.react(t, "PUT", "nina", "ops", w.net, thumbsUp)
	if fr := reactionFrames(t, olga, 400*time.Millisecond); len(fr) != 0 {
		t.Errorf("a reader off the net got a frame: %+v", fr)
	}
	// E2: a burst converges to the whole final state.
	for _, e := range []string{"😀", "😃", "😄", "😁", "😆", "😅", "😂", "🤣", "😊", "😇"} {
		w.react(t, "PUT", "nina", "ops", w.wide, e)
	}
	fr := reactionFrames(t, olga, 800*time.Millisecond)
	if len(fr) == 0 || len(fr[len(fr)-1].Reactions) != 11 {
		t.Errorf("after a burst the last frame is %+v", fr)
	}
	// Removing the last reaction sends the empty state.
	w.react(t, "DELETE", "wes", "ops", w.whisper, thumbsUp)
	if fr := reactionFrames(t, ann, 500*time.Millisecond); len(fr) == 0 || len(fr[len(fr)-1].Reactions) != 0 {
		t.Errorf("the empty summary was not sent: %+v", fr)
	}

	// E4: nothing on the message sockets, no read-state change, no broadcast, no
	// notification; the only new audit rows are the reactions' own.
	if fr := reactionFrames(t, v2, 100*time.Millisecond); len(fr) != 0 {
		t.Errorf("/v2/ws carried a reaction: %+v", fr)
	}
	if after := w.callREST(t, "GET", "/v1/read-state", w.p("olga").token, "").body; after != unreadBefore {
		t.Errorf("read state changed: %s -> %s", unreadBefore, after)
	}
	if len(w.rec.seen()) != seenBefore {
		t.Error("the Broadcaster was called")
	}
	for _, e := range w.audit(t)[auditBefore:] {
		if e.Action != store.AuditReactionSet && e.Action != store.AuditReactionRemoved {
			t.Errorf("a reaction wrote an audit row %q", e.Action)
		}
	}
}

// R1b: a reference flushed after a removal committed but before the socket was
// dropped sends nothing, for a channel-wide message as for a scoped one.
func TestReactionFrameAfterRemovalSendsNothing(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  func(*reactionWorld) int64
	}{
		{"channel-wide", func(w *reactionWorld) int64 { return w.wide }},
		{"net", func(w *reactionWorld) int64 { return w.net }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newReactionWorld(t)
			who := "nina"
			s := openSocket(t, w.base, "/v1/events/ws?channel=ops", w.p(who).token)
			time.Sleep(300 * time.Millisecond)
			reactionFrames(t, s, 200*time.Millisecond)
			armed := make(chan struct{})
			w.srv.reactionBeforeRead = func() {
				select {
				case <-armed:
					return
				default:
				}
				close(armed)
				// The removal commits; the socket has not been dropped.
				if _, err := w.srv.store.RemoveChannelMember(context.Background(), "system", w.ops.ID, w.p(who).ID); err != nil {
					t.Error(err)
				}
			}
			w.react(t, "PUT", "ann", "ops", tt.msg(w), thumbsUp)
			if fr := reactionFrames(t, s, 700*time.Millisecond); len(fr) != 0 {
				t.Errorf("a removed member was sent %+v", fr)
			}
			select {
			case <-armed:
			default:
				t.Error("the hook never ran: the test did not reach the window")
			}
		})
	}
}

// Closing the socket on a slow consumer is the events hub's own test; here a
// reconnect re-hydrates by the batch read and agrees with the rows (E3, E6).
func TestReactionReconnectRehydrates(t *testing.T) {
	w := newReactionWorld(t)
	s := openSocket(t, w.base, "/v1/events/ws?channel=ops", w.p("olga").token)
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
	w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp)
	w.react(t, "PUT", "nina", "ops", w.wide, thumbsUp)
	batch := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/reactions?messages=%d", w.wide), w.p("olga").token, "")
	var br schema.ReactionSummariesResponseV1
	if err := json.Unmarshal([]byte(batch.body), &br); err != nil || len(br.Summaries) != 1 || br.Summaries[0].Reactions[0].Count != 2 {
		t.Errorf("rehydrated = %s", batch.body)
	}
	if n := countReactionRows(t, w.srv); n != 2 {
		t.Errorf("%d rows", n)
	}
}

// V7b, a whisper whose audience holds another agent: the reacting agent needs
// whisper_agent as well as whisper, as a reply there does
// (authorizeScopedPostGates). The probe must carry the whisper's recipients,
// or the check has nobody to look at.
func TestReactionWhisperToAgentsNeedsWhisperAgent(t *testing.T) {
	w := newReactionWorld(t)
	read, whisper, whisperAgent := schema.ChannelPermissionRead, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent
	msg := w.mustPost(t, "ann", `{"body":"SECRET-agents",`+whisperAudience(w.p("aria").ID, w.p("arlo").ID)+`}`)
	token := w.p("aria").token
	args := map[string]any{"channel": "ops", "message_id": msg, "emoji": thumbsUp}

	w.manifest(t, "aria", nil, read, whisper)
	for _, tt := range []struct{ method, tool string }{{"PUT", "set_reaction"}, {"DELETE", "remove_reaction"}} {
		rows := countReactionRows(t, w.srv)
		before := len(w.auditDetails(t, "access_denied"))
		if res := w.callREST(t, tt.method, rpath("ops", msg, thumbsUp), token, ""); res.status != 403 || reactErrCode(res) != "forbidden" {
			t.Errorf("%s without whisper_agent = %d %s, want 403 forbidden", tt.method, res.status, res.body)
		}
		if got := w.mcpCall(t, token, tt.tool, args).result(); got != "forbidden" {
			t.Errorf("%s without whisper_agent = %s, want forbidden", tt.tool, got)
		}
		denials := w.auditDetails(t, "access_denied")
		if len(denials) != before+2 || strings.Count(strings.Join(denials[before:], "\n"), "reason="+denyAudienceGrant) != 2 {
			t.Errorf("%s: the refusals were not audited once each as %s: %v", tt.method, denyAudienceGrant, denials[before:])
		}
		if got := countReactionRows(t, w.srv); got != rows {
			t.Errorf("%s: a refusal changed the rows %d -> %d", tt.method, rows, got)
		}
	}

	w.manifest(t, "aria", nil, read, whisper, whisperAgent)
	if res := w.callREST(t, "PUT", rpath("ops", msg, thumbsUp), token, ""); res.status != 200 || countReactionRows(t, w.srv) != 1 {
		t.Errorf("PUT with whisper_agent = %d %s", res.status, res.body)
	}
	if res := w.callREST(t, "DELETE", rpath("ops", msg, thumbsUp), token, ""); res.status != 200 || countReactionRows(t, w.srv) != 0 {
		t.Errorf("DELETE with whisper_agent = %d %s", res.status, res.body)
	}
	for _, tool := range []string{"set_reaction", "remove_reaction"} {
		if got := w.mcpCall(t, token, tool, args).result(); got != "ok" {
			t.Errorf("%s with whisper_agent = %s", tool, got)
		}
	}
}

// An agent's write answers the message's whole summary, so it needs the read
// grant too: an agent that may post but not read gets the GET's own 403, byte
// for byte, on both writes and both tools, audited, and nothing is stored.
func TestReactionWriteNeedsRead(t *testing.T) {
	w := newReactionWorld(t)
	w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp)
	token := w.p("arlo").token // post, not read
	get := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", w.wide), token, "")
	if get.status != 403 || reactErrCode(get) != "forbidden" {
		t.Fatalf("the write-only agent's GET = %d %s", get.status, get.body)
	}
	rows := countReactionRows(t, w.srv)
	for _, method := range []string{"PUT", "DELETE"} {
		before := len(w.auditDetails(t, "access_denied"))
		if res := w.callREST(t, method, rpath("ops", w.wide, partyKey), token, ""); res != get {
			t.Errorf("%s = %+v, the GET gave %+v", method, res, get)
		}
		denials := w.auditDetails(t, "access_denied")
		if len(denials) != before+1 || !strings.Contains(denials[len(denials)-1], "capability=messages.read") {
			t.Errorf("%s: the refusal was not audited once as a read refusal: %v", method, denials[before:])
		}
	}
	for _, tool := range []string{"set_reaction", "remove_reaction"} {
		out := w.mcpCall(t, token, tool, map[string]any{"channel": "ops", "message_id": w.wide, "emoji": partyKey})
		if out.result() != "forbidden" || strings.Contains(out.body, `count`) {
			t.Errorf("%s = %s %s, want forbidden", tool, out.result(), out.body)
		}
	}
	if got := countReactionRows(t, w.srv); got != rows {
		t.Errorf("refused writes changed the rows %d -> %d", rows, got)
	}
}

// execOnDB runs a statement on the server's database file, underneath the
// store, to seed rows the store would refuse to write.
func execOnDB(t *testing.T, srv *Server, query string, args ...any) {
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
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// A stored key that is not canonical (an older catalog, a hand edit) is left
// out of every summary the server builds, so REST, MCP and the events socket
// all serve a valid document, and the socket is not closed for it. The drop
// is logged once per summary, without the key.
func TestReactionNonCanonicalStoredKey(t *testing.T) {
	logs := captureLogsLocked(t) // the events goroutines log too
	w := newReactionWorld(t)
	w.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost)
	w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp)
	const unknownKey, unqualified = "old-key-x", "❤" // a heart without its selector
	for _, key := range []string{unknownKey, unqualified} {
		execOnDB(t, w.srv, `INSERT INTO message_reactions (message_id, emoji, principal_id, channel_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			w.wide, key, w.p("nina").ID, w.ops.ID, int64(1))
	}
	s := openSocket(t, w.base, "/v1/events/ws?channel=ops", w.p("olga").token)
	time.Sleep(300 * time.Millisecond) // the first rosters
	reactionFrames(t, s, 200*time.Millisecond)
	onlyThumbs := func(where string, sum schema.ReactionSummaryV1) {
		t.Helper()
		if err := sum.Validate(); err != nil {
			t.Errorf("%s: invalid summary: %v", where, err)
		}
		for _, c := range sum.Reactions {
			if c.Emoji == unknownKey || c.Emoji == unqualified {
				t.Errorf("%s served the stored key %q", where, c.Emoji)
			}
		}
	}

	logStart := len(logs.String())
	get := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", w.wide), w.p("olga").token, "")
	if get.status != 200 {
		t.Fatalf("GET = %d %s", get.status, get.body)
	}
	sum := decodeSummary(t, get)
	onlyThumbs("GET", sum)
	if len(sum.Reactions) != 1 || sum.Reactions[0].Emoji != thumbsUp {
		t.Errorf("GET = %+v, want the thumbs up alone", sum)
	}
	if got := logs.String()[logStart:]; strings.Count(got, "dropped stored keys") != 1 {
		t.Errorf("one summary logged %d drop lines, want 1: %s", strings.Count(got, "dropped stored keys"), got)
	}

	batch := w.callREST(t, "GET", fmt.Sprintf("/v2/channels/ops/reactions?messages=%d", w.wide), w.p("olga").token, "")
	var br schema.ReactionSummariesResponseV1
	if err := json.Unmarshal([]byte(batch.body), &br); err != nil || br.Validate() != nil || len(br.Summaries) != 1 {
		t.Errorf("batch = %d %s", batch.status, batch.body)
	} else {
		onlyThumbs("batch", br.Summaries[0])
	}
	put := w.react(t, "PUT", "olga", "ops", w.wide, partyKey)
	onlyThumbs("PUT", decodeSummary(t, put))

	mcp := w.mcpCall(t, w.p("aria").token, "read_reactions", map[string]any{"channel": "ops", "message_ids": []int64{w.wide}})
	if mcp.result() != "ok" || strings.Contains(mcp.body, unknownKey) || strings.Contains(mcp.body, unqualified) || !strings.Contains(mcp.body, partyKey) {
		t.Errorf("read_reactions = %s %s", mcp.result(), mcp.body)
	}
	if out := w.mcpCall(t, w.p("aria").token, "set_reaction", map[string]any{"channel": "ops", "message_id": w.wide, "emoji": thumbsUp}); out.result() != "ok" || strings.Contains(out.body, unknownKey) {
		t.Errorf("set_reaction = %s %s", out.result(), out.body)
	}

	// The events socket: a frame for each change, every one valid, and the
	// socket still open after them.
	fr := reactionFrames(t, s, 800*time.Millisecond)
	if len(fr) == 0 {
		t.Fatal("the events socket sent no frame for a message holding a non-canonical key")
	}
	for _, f := range fr {
		onlyThumbs("events", f)
	}
	w.react(t, "DELETE", "olga", "ops", w.wide, partyKey)
	if fr := reactionFrames(t, s, 800*time.Millisecond); len(fr) == 0 {
		t.Error("no frame after the next change: the socket was closed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.conn.Ping(ctx); err != nil {
		t.Errorf("the events socket is closed: %v", err)
	}
	if all := logs.String(); strings.Contains(all, unknownKey) || strings.Contains(all, unqualified) {
		t.Errorf("the log names a stored key: %s", all)
	}
	// The rows are still stored: only the wire leaves them out.
	if n := countReactionRows(t, w.srv); n != 4 {
		t.Errorf("%d rows, want 4", n)
	}
}

// W6, with a clock: a 409 too_many_reactions takes a token like any refusal,
// and after a 429 one token's time buys exactly one write.
func TestReactionRateLimitRefill(t *testing.T) {
	w := newReactionWorld(t)
	clock := newManualClock()
	w.srv.now = clock.Now
	// ann holds the most reactions one person may on one message.
	held := []string{"😀", "😃", "😄", "😁", "😆", "😅", "😂", "🤣", "😊", "😇"}
	if len(held) != schema.MaxReactionsPerPrincipalPerMessage {
		t.Fatalf("seed %d reactions, the limit is %d", len(held), schema.MaxReactionsPerPrincipalPerMessage)
	}
	for _, e := range held {
		if res := w.react(t, "PUT", "ann", "ops", w.wide, e); res.status != 200 {
			t.Fatalf("seed %s = %d %s", e, res.status, res.body)
		}
	}
	// Two tokens, one a minute.
	w.srv.setReactionLimit(inviteLimit{name: "test", burst: 2, window: 2 * time.Minute})
	if res := w.react(t, "PUT", "ann", "ops", w.wide, thumbsUp); res.status != 409 || reactErrCode(res) != "too_many_reactions" {
		t.Fatalf("one too many = %d %s", res.status, res.body)
	}
	// The 409 took exactly one: one more write passes, the next is limited.
	if res := w.react(t, "DELETE", "ann", "ops", w.wide, held[0]); res.status != 200 {
		t.Fatalf("the second write = %d %s; the 409 took more than one token", res.status, res.body)
	}
	res := w.react(t, "PUT", "ann", "ops", w.wide, held[0])
	if res.status != 429 {
		t.Fatalf("the third write = %d %s; the 409 took no token", res.status, res.body)
	}
	var limited schema.Error
	if err := json.Unmarshal([]byte(res.body), &limited); err != nil || limited.RetryAfterSeconds != 60 {
		t.Errorf("retry_after_seconds = %d (%v), want 60", limited.RetryAfterSeconds, err)
	}
	clock.Advance(time.Minute)
	if res := w.react(t, "PUT", "ann", "ops", w.wide, held[0]); res.status != 200 {
		t.Errorf("the write after one token's time = %d %s", res.status, res.body)
	}
	if res := w.react(t, "DELETE", "ann", "ops", w.wide, held[1]); res.status != 429 {
		t.Errorf("a second write after one token's time = %d %s, want 429", res.status, res.body)
	}
}
