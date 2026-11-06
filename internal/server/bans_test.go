package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Channel entry bans (issue #851, docs/design/moderation.md §6, §9).

// banFixture is the voice moderation cast with the rights set up for bans:
//
//	root     owner                   op2   non-owner operator, a member of nothing
//	mgr      manager of ops, no decide right
//	plain    member of ops, no decide right
//	ann      member of ops with the decide right (an approver); manages ops2
//	ann2     member of ops, no decide right
//	robo     agent member of ops   mgr2  manager of ops2 only (a ghost for ops)
//	bob      a member of nothing
type banFixture struct{ *voiceKickFixture }

func newBanFixture(t *testing.T, o presenceOpts) *banFixture {
	t.Helper()
	f := &banFixture{newVoiceKickFixture(t, o)}
	ctx := context.Background()
	for _, name := range []string{"mgr", "plain", "ann2"} {
		if _, err := f.srv.store.SetChannelMemberCanDecide(ctx, "system", f.ops.ID, f.ids[name], false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.srv.store.SetChannelMemberCanDecide(ctx, "system", f.ops.ID, f.ids["robo"], false); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *banFixture) rawDB(t *testing.T) *sql.DB {
	t.Helper()
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedBan writes a ban row the way a restored backup could: with the
// principal's membership still in place.
func (f *banFixture) seedBan(t *testing.T, by, who string) {
	t.Helper()
	if _, err := f.rawDB(t).Exec("INSERT INTO channel_bans (channel_id, principal_id, created_by, created_at) VALUES (?, ?, ?, 1)",
		f.ops.ID, f.ids[who], f.ids[by]); err != nil {
		t.Fatal(err)
	}
}

func (f *banFixture) put(t *testing.T, as string, target int64) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPut, fmt.Sprintf("/v1/channels/ops/bans/%d", target), f.tokens[as], "")
}

func (f *banFixture) lift(t *testing.T, as string, target int64) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/bans/%d", target), f.tokens[as], "")
}

func (f *banFixture) list(t *testing.T, as, channel string) (wireResult, schema.ListChannelBansResponseV1) {
	t.Helper()
	res := f.callREST(t, http.MethodGet, "/v1/channels/"+channel+"/bans", f.tokens[as], "")
	var out schema.ListChannelBansResponseV1
	if res.status == http.StatusOK {
		if err := json.Unmarshal([]byte(res.body), &out); err != nil {
			t.Fatalf("decode %s: %v", res.body, err)
		}
		if err := out.Validate(); err != nil {
			t.Fatalf("invalid list %s: %v", res.body, err)
		}
	}
	return res, out
}

func (f *banFixture) member(t *testing.T, ch store.Channel, who string) bool {
	t.Helper()
	ok, err := f.srv.store.IsChannelMember(context.Background(), ch.ID, f.ids[who])
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (f *banFixture) banAudits(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditChannelBanAdded, store.AuditChannelBanRemoved, store.AuditMemberRemoved, store.AuditNetMemberRemoved:
			out = append(out, e.Action+" "+e.Actor+" "+e.Subject+" "+e.Detail)
		}
	}
	return out
}

// TestBanRouteActorTargetMatrix: actor x target x membership over the wire
// (design §6.1, §9 cases 1 and 2). A refusal changes and audits nothing.
func TestBanRouteActorTargetMatrix(t *testing.T) {
	const (
		ok        = http.StatusNoContent
		forbidden = http.StatusForbidden
		notFound  = http.StatusNotFound
	)
	tests := []struct {
		name   string
		actor  string
		target string
		want   int
	}{
		{"owner bans a member", "root", "plain", ok},
		{"owner bans an approver", "root", "ann", ok},
		{"owner bans an agent", "root", "robo", ok},
		{"owner pre-bans a non-member", "root", "bob", ok},
		{"operator bans a member", "op2", "plain", ok},
		{"operator bans an approver", "op2", "ann", ok},
		{"operator pre-bans a non-member", "op2", "bob", ok},
		{"operator cannot ban the owner", "op2", "root", forbidden},
		{"manager bans a member", "mgr", "plain", ok},
		{"manager bans a member who also manages elsewhere", "mgr", "ann2", ok},
		{"manager bans themselves", "mgr", "mgr", ok},
		{"manager cannot ban an approver", "mgr", "ann", forbidden},
		{"manager cannot ban an agent", "mgr", "robo", forbidden},
		{"manager cannot pre-ban a non-member", "mgr", "bob", notFound},
		{"manager cannot pre-ban an operator who is not a member", "mgr", "op2", notFound},
		{"manager cannot pre-ban the owner who is not a member", "mgr", "root", notFound},
		{"manager of another channel and a member here has no standing", "ann", "plain", forbidden},
		{"manager of another channel only is a ghost", "mgr2", "plain", notFound},
		{"plain member", "plain", "ann2", forbidden},
		{"non-member", "bob", "plain", notFound},
		{"agent member", "robo", "plain", forbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newBanFixture(t, presenceOpts{auth: AuthRequired})
			target := f.ids[tt.target]
			wasMember := f.member(t, f.ops, tt.target)
			res := f.put(t, tt.actor, target)
			if res.status != tt.want {
				t.Fatalf("status = %d (%s), want %d", res.status, res.body, tt.want)
			}
			banned, err := f.srv.store.IsChannelBanned(context.Background(), f.ops.ID, target)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == ok {
				if res.body != "" {
					t.Errorf("a 204 carried a body %q", res.body)
				}
				if !banned || f.member(t, f.ops, tt.target) {
					t.Errorf("after the ban: banned = %v, member = %v", banned, f.member(t, f.ops, tt.target))
				}
				rows := f.banAudits(t)
				wantMember := 0
				if wasMember {
					wantMember = 1
				}
				var added, removed int
				for _, r := range rows {
					switch {
					case strings.HasPrefix(r, store.AuditChannelBanAdded):
						added++
						if !strings.Contains(r, f.actor(tt.actor)) || !strings.HasSuffix(r, fmt.Sprintf("channel:%d principal=%d", f.ops.ID, target)) {
							t.Errorf("ban row %q", r)
						}
					case strings.HasPrefix(r, store.AuditMemberRemoved):
						removed++
					}
				}
				if added != 1 || removed != wantMember {
					t.Errorf("audit = %v, want one ban and %d removals", rows, wantMember)
				}
				// Another channel's membership is unaffected.
				if f.member(t, f.ops2, tt.target) != (tt.target == "ann") {
					t.Errorf("ops2 membership of %s changed", tt.target)
				}
				return
			}
			if banned || f.member(t, f.ops, tt.target) != wasMember {
				t.Errorf("a refusal changed state: banned = %v", banned)
			}
			if rows := f.banAudits(t); len(rows) != 0 {
				t.Errorf("a refusal wrote %v", rows)
			}
		})
	}
	// No credential at all.
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	if got := f.callREST(t, http.MethodPut, "/v1/channels/ops/bans/1", "", ""); got.status != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", got.status)
	}
}

// TestBanRouteGhostIdentical: a manager of another channel, a plain member
// and a non-member get the unknown channel's response (or the class's 403 for
// a member), whatever the target; no target is looked up first (design §9
// case 1).
func TestBanRouteGhostIdentical(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	for _, as := range []string{"mgr2", "bob"} {
		for _, target := range []int64{f.ids["plain"], f.ids["mgr"], f.ids["root"], 99999} {
			for _, m := range []string{http.MethodPut, http.MethodDelete} {
				path := func(ch string) string { return fmt.Sprintf("/v1/channels/%s/bans/%d", ch, target) }
				real := f.callREST(t, m, path("ops"), f.tokens[as], "")
				ghost := f.callREST(t, m, path("nosuch"), f.tokens[as], "")
				if real != ghost || real.status != http.StatusNotFound || !strings.Contains(real.body, "channel_not_found") {
					t.Errorf("%s %s target %d: real %+v, ghost %+v; want identical channel_not_found", m, as, target, real, ghost)
				}
			}
		}
		real, _ := f.list(t, as, "ops")
		ghost, _ := f.list(t, as, "nosuch")
		if real != ghost || real.status != http.StatusNotFound {
			t.Errorf("list as %s: real %+v, ghost %+v", as, real, ghost)
		}
	}
	// A plain member is refused 403 whatever the target, before any lookup.
	for _, target := range []int64{f.ids["plain"], 99999} {
		if got := f.put(t, "plain", target); got.status != http.StatusForbidden {
			t.Errorf("plain member PUT target %d = %d", target, got.status)
		}
	}
	// A bad target id is a request error only for someone with standing.
	if got := f.callREST(t, http.MethodPut, "/v1/channels/ops/bans/x", f.tokens["plain"], ""); got.status != http.StatusForbidden {
		t.Errorf("plain member with a bad id = %d, want 403", got.status)
	}
	if got := f.callREST(t, http.MethodPut, "/v1/channels/ops/bans/x", f.tokens["mgr"], ""); got.status != http.StatusBadRequest {
		t.Errorf("manager with a bad id = %d, want 400", got.status)
	}
}

// TestBanRouteIdempotencyAndList: a repeat writes no second ban row, an
// unban of an absent ban writes none, the list is ordered and carries who
// banned, and a manager sees the creator so it can tell whose ban it may
// lift (design §3.3, §6.1).
func TestBanRouteIdempotencyAndList(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	if res, list := f.list(t, "mgr", "ops"); res.status != http.StatusOK || len(list.Bans) != 0 || !strings.Contains(res.body, `"bans":[]`) {
		t.Fatalf("empty list = %+v", res)
	}
	for i := 0; i < 2; i++ {
		if got := f.put(t, "mgr", f.ids["plain"]); got.status != http.StatusNoContent {
			t.Fatalf("PUT #%d = %d %s", i, got.status, got.body)
		}
	}
	if got := f.put(t, "op2", f.ids["bob"]); got.status != http.StatusNoContent {
		t.Fatalf("pre-ban = %d", got.status)
	}
	var added int
	for _, r := range f.banAudits(t) {
		if strings.HasPrefix(r, store.AuditChannelBanAdded) {
			added++
		}
	}
	if added != 2 {
		t.Errorf("channel_ban_added rows = %d after a repeat, want 2 (one per principal)", added)
	}
	res, list := f.list(t, "mgr", "ops")
	if res.status != http.StatusOK || len(list.Bans) != 2 {
		t.Fatalf("list = %+v", res)
	}
	if b := list.Bans[0]; b.Name != "bob" && b.Name != "plain" {
		t.Errorf("first ban = %+v", b)
	}
	for _, b := range list.Bans {
		want := f.ids["mgr"]
		if b.Name == "bob" {
			want = f.ids["op2"]
		}
		if b.CreatedBy != want {
			t.Errorf("ban of %s created_by = %d, want %d", b.Name, b.CreatedBy, want)
		}
	}
	// The manager cannot lift the operator's ban, can lift their own; an
	// absent ban is a 204 for them without any lookup.
	if got := f.lift(t, "mgr", f.ids["bob"]); got.status != http.StatusForbidden {
		t.Errorf("manager lifting an operator's ban = %d, want 403", got.status)
	}
	if got := f.lift(t, "mgr", f.ids["plain"]); got.status != http.StatusNoContent {
		t.Errorf("manager lifting their own = %d", got.status)
	}
	if got := f.lift(t, "mgr", f.ids["plain"]); got.status != http.StatusNoContent {
		t.Errorf("manager lifting an absent ban = %d", got.status)
	}
	if got := f.lift(t, "mgr", 99999); got.status != http.StatusNoContent {
		t.Errorf("manager lifting an unknown id = %d", got.status)
	}
	if got := f.lift(t, "op2", f.ids["bob"]); got.status != http.StatusNoContent {
		t.Errorf("operator lifting any ban = %d", got.status)
	}
	var lifted int
	for _, r := range f.banAudits(t) {
		if strings.HasPrefix(r, store.AuditChannelBanRemoved) {
			lifted++
		}
	}
	if lifted != 2 {
		t.Errorf("channel_ban_removed rows = %d, want 2", lifted)
	}
	// An unban does not restore, and an absent-ban unban removes no member.
	if f.member(t, f.ops, "plain") {
		t.Error("unban restored a membership")
	}
	if got := f.lift(t, "root", f.ids["ann2"]); got.status != http.StatusNoContent || !f.member(t, f.ops, "ann2") {
		t.Errorf("absent-ban unban of a member = %d, member = %v", got.status, f.member(t, f.ops, "ann2"))
	}
	// List authorization: members without standing and agents are refused.
	for as, want := range map[string]int{"ann2": http.StatusForbidden, "plain": http.StatusNotFound, "robo": http.StatusForbidden, "bob": http.StatusNotFound, "root": http.StatusOK, "op2": http.StatusOK} {
		if res, _ := f.list(t, as, "ops"); res.status != want {
			t.Errorf("list as %s = %d, want %d", as, res.status, want)
		}
	}
}

// TestBanInArchivedChannelOverTheWire: #849 Q1.
func TestBanInArchivedChannelOverTheWire(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	if _, err := f.rawDB(t).Exec("UPDATE channels SET archived_at = 1 WHERE id = ?", f.ops.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.put(t, "mgr", f.ids["plain"]); got.status != http.StatusNoContent {
		t.Errorf("ban in an archived channel = %d %s", got.status, got.body)
	}
	if got := f.lift(t, "mgr", f.ids["plain"]); got.status != http.StatusNoContent {
		t.Errorf("unban in an archived channel = %d %s", got.status, got.body)
	}
}

// TestBanAuditRedaction: a ban's audit rows carry ids only (design §3.3).
func TestBanAuditRedaction(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	f.put(t, "mgr", f.ids["plain"])
	f.lift(t, "mgr", f.ids["plain"])
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditChannelBanAdded, store.AuditChannelBanRemoved, store.AuditMemberRemoved:
		default:
			continue
		}
		text := e.Actor + " " + e.Subject + " " + e.Detail
		for _, secret := range []string{"plain", "ops", f.tokens["mgr"], f.tokens["plain"], "127.0.0.1", "192.0.2."} {
			if strings.Contains(text, secret) {
				t.Errorf("%s row %q carries %q", e.Action, text, secret)
			}
		}
	}
}

// TestBannedResidualMembershipGrantsNothing: a membership row that outlived
// a ban reaches no REST, WebSocket, voice or MCP path, before cleanup, after a
// repeat ban and after an unban, and every refusal is the byte-identical
// response of a person who was never a member (design §6.2, §9 case 5).
func TestBannedResidualMembershipGrantsNothing(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	base := wsTestServer(t, f.srv)
	ctx := context.Background()
	// "plain" is the residual; "bob" never was a member. Both are people, so
	// their refusals must be the same bytes.
	root := f.ids["root"]
	msg, err := f.srv.store.InsertMessage(ctx, f.ops.ID, f.ids["ann"], "hello")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name   string
		method string
		path   string
		body   string
		ws     bool
	}
	ch := "ops"
	rows := []row{
		{"v0 list", "GET", "/v0/channels/%s/messages", "", false},
		{"v1 list", "GET", "/v1/channels/%s/messages", "", false},
		{"v2 list", "GET", "/v2/channels/%s/messages", "", false},
		{"v0 post", "POST", "/v0/channels/%s/messages", `{"body":"x"}`, false},
		{"v1 post", "POST", "/v1/channels/%s/messages", `{"body":"x"}`, false},
		{"v2 post", "POST", "/v2/channels/%s/messages", `{"body":"x"}`, false},
		{"v2 reply (a thread)", "POST", "/v2/channels/%s/messages", fmt.Sprintf(`{"body":"x","thread_root_id":%d}`, msg.ID), false},
		{"v2 search", "GET", "/v2/channels/%s/messages/search?q=hello", "", false},
		{"members", "GET", "/v1/channels/%s/members", "", false},
		{"authors", "GET", "/v1/channels/%s/authors?ids=1", "", false},
		{"nets", "GET", "/v1/channels/%s/nets", "", false},
		{"read state", "PUT", "/v1/channels/%s/read-state", `{"last_read_message_id":1}`, false},
		{"self-join", "POST", "/v1/channels/%s/join", "", false},
		{"file upload", "POST", "/v2/channels/%s/files", "", false},
		{"reactions list", "GET", "/v2/channels/%s/reactions", "", false},
		{"reaction set", "PUT", fmt.Sprintf("/v2/channels/%%s/messages/%d/reactions/%%%%F0%%%%9F%%%%91%%%%8D", msg.ID), "", false},
		{"reaction read", "GET", fmt.Sprintf("/v2/channels/%%s/messages/%d/reactions", msg.ID), "", false},
		{"edit", "PUT", fmt.Sprintf("/v2/channels/%%s/messages/%d/body", msg.ID), `{"body":"y"}`, false},
		{"revisions", "GET", fmt.Sprintf("/v2/channels/%%s/messages/%d/revisions", msg.ID), "", false},
		{"voice presence", "GET", "/v1/channels/%s/voice", "", false},
		{"voice session", "POST", "/v1/channels/%s/voice/session", "", false},
		{"voice transmit", "POST", "/v1/channels/%s/voice/transmit", `{"state":"started"}`, false},
		{"voice share", "POST", "/v1/channels/%s/voice/share", "", false},
		{"voice camera", "POST", "/v1/channels/%s/voice/camera", "", false},
		{"voice companion", "POST", "/v1/channels/%s/voice/companion", `{"publish":"screen","subscribe":true}`, false},
		{"v0 socket", "GET", "/v0/ws?channel=%s", "", true},
		{"v1 socket", "GET", "/v1/ws?channel=%s", "", true},
		{"v2 socket", "GET", "/v2/ws?channel=%s", "", true},
		{"events socket", "GET", "/v1/events/ws?channel=%s", "", true},
		{"voice presence socket", "GET", "/v1/voice/ws?channel=%s", "", true},
	}
	call := func(r row, who, channel string) wireResult {
		path := fmt.Sprintf(r.path, channel)
		if r.ws {
			return callWS(t, base, path, f.tokens[who])
		}
		return f.callREST(t, r.method, path, f.tokens[who], r.body)
	}
	// Control: before the ban "plain" is a member and reaches the channel.
	for _, name := range []string{"v1 list", "v2 list", "members", "voice presence"} {
		for _, r := range rows {
			if r.name == name {
				if got := call(r, "plain", ch); got.status != http.StatusOK {
					t.Fatalf("control %s: a member got %d %s", name, got.status, got.body)
				}
			}
		}
	}
	for _, r := range rows {
		if r.ws {
			if got := call(r, "plain", ch); got.status != http.StatusSwitchingProtocols {
				t.Fatalf("control %s: a member got %d %s", r.name, got.status, got.body)
			}
		}
	}
	_ = root

	// refused: every row for the residual equals the never-a-member's, equals
	// the unknown channel's.
	refused := func(stage string) {
		t.Helper()
		for _, r := range rows {
			got := call(r, "plain", ch)
			never := call(r, "bob", ch)
			ghost := call(r, "bob", "nosuch")
			if got != never || got != ghost {
				t.Errorf("%s %s: residual %+v, never-a-member %+v, unknown channel %+v; want identical", stage, r.name, got, never, ghost)
			}
			if got.status != http.StatusNotFound || !strings.Contains(got.body, "channel_not_found") {
				t.Errorf("%s %s: status %d %s, want channel_not_found", stage, r.name, got.status, got.body)
			}
		}
		// The residual's agent-free reads: the channel list and unread state
		// leave the channel out, and a decision they once could cast is gone.
		if chs, err := f.srv.store.ListChannelsForPrincipal(ctx, f.ids["plain"]); err != nil || len(chs) != 0 {
			t.Errorf("%s: channels of the residual = %v, %v", stage, chs, err)
		}
		if res := f.callREST(t, "GET", "/v1/channels", f.tokens["plain"], ""); strings.Contains(res.body, `"ops"`) {
			t.Errorf("%s: the channel list shows ops: %s", stage, res.body)
		}
	}

	f.seedBan(t, "mgr", "plain")
	if ok, _ := f.srv.store.IsChannelBanned(ctx, f.ops.ID, f.ids["plain"]); !ok {
		t.Fatal("not banned")
	}
	var stillRows int
	if err := f.rawDB(t).QueryRow("SELECT COUNT(*) FROM channel_members WHERE channel_id = ? AND principal_id = ?", f.ops.ID, f.ids["plain"]).Scan(&stillRows); err != nil || stillRows != 1 {
		t.Fatalf("the residual row is not there: %d %v", stillRows, err)
	}
	refused("before cleanup")

	// A repeat ban cleans it, audited.
	if got := f.put(t, "op2", f.ids["plain"]); got.status != http.StatusNoContent {
		t.Fatalf("repeat ban = %d %s", got.status, got.body)
	}
	if err := f.rawDB(t).QueryRow("SELECT COUNT(*) FROM channel_members WHERE channel_id = ? AND principal_id = ?", f.ops.ID, f.ids["plain"]).Scan(&stillRows); err != nil || stillRows != 0 {
		t.Fatalf("the residual survived a repeat ban: %d %v", stillRows, err)
	}
	if rows := f.banAudits(t); len(rows) != 1 || !strings.HasPrefix(rows[0], store.AuditMemberRemoved) {
		t.Errorf("audit after the repeat = %v, want one member_removed and no second ban", rows)
	}
	refused("after the repeat ban")

	// Seed again, then unban: cleaned before the ban goes, and nothing returns.
	if _, err := f.rawDB(t).Exec("INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide) VALUES (?, ?, NULL, 1, 1)",
		f.ops.ID, f.ids["plain"]); err != nil {
		t.Fatal(err)
	}
	refused("re-seeded")
	if got := f.lift(t, "op2", f.ids["plain"]); got.status != http.StatusNoContent {
		t.Fatalf("unban = %d %s", got.status, got.body)
	}
	refused("after the unban")
	// Only an explicit add admits them, as a new member.
	if got := f.callREST(t, "PUT", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["plain"]), f.tokens["op2"], ""); got.status != http.StatusNoContent {
		t.Fatalf("explicit add after unban = %d %s", got.status, got.body)
	}
	if got := call(rows[1], "plain", ch); got.status != http.StatusOK {
		t.Errorf("after the explicit add the member got %d", got.status)
	}
}

// TestBannedResidualMembershipMCP: the agent surface reads the same predicate.
func TestBannedResidualMembershipMCP(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	if _, err := f.srv.store.InsertMessage(ctx, f.ops.ID, f.ids["ann"], "hello"); err != nil {
		t.Fatal(err)
	}
	// robo is a member of ops with a manifest; strange is an agent with the
	// same manifest that never was a member.
	strange, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "strange")
	if err != nil {
		t.Fatal(err)
	}
	_, strangeTok, err := f.srv.store.CreateCredential(ctx, "system", strange.ID, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	setAgentManifest(t, f.srv, strange.ID, nil, f.ops.ID)
	tools := []struct {
		name string
		args map[string]any
	}{
		{"read_channel", map[string]any{"channel": "ops"}},
		{"post_message", map[string]any{"channel": "ops", "body": "x"}},
		{"read_reactions", map[string]any{"channel": "ops", "message_ids": []int64{1}}},
	}
	for _, tool := range tools {
		if res := mcpTool(t, f.srv, f.tokens["robo"], tool.name, tool.args); res.code != "ok" {
			t.Fatalf("control %s: a member agent got %s", tool.name, res.code)
		}
	}
	f.seedBan(t, "mgr", "robo")
	for stage := 0; stage < 2; stage++ {
		for _, tool := range tools {
			got := mcpTool(t, f.srv, f.tokens["robo"], tool.name, tool.args)
			never := mcpTool(t, f.srv, strangeTok, tool.name, tool.args)
			if got.code != never.code || got.code != "channel_not_found" {
				t.Errorf("stage %d %s: residual banned agent = %q, never-a-member agent = %q; want both channel_not_found", stage, tool.name, got.code, never.code)
			}
		}
		if res := f.put(t, "root", f.ids["robo"]); res.status != http.StatusNoContent {
			t.Fatal(res)
		}
	}
}

// TestBanDropsLiveSockets: a ban removes the target's sockets on the channel
// as a removal does, and only theirs (design §5.2, §6.2).
func TestBanDropsLiveSockets(t *testing.T) {
	for _, version := range []string{"v0", "v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			f := newMemberFixture(t)
			base := wsTestServer(t, f.srv)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/alpha/members/%d", f.bot.ID), f.rootTok, ""); rec.Code != 204 {
				t.Fatal(rec.Code)
			}
			ws := func(ch string) string { return "/" + version + "/ws?channel=" + ch }
			aliceAlpha := startReader(ctx, dialAs(t, ctx, base, ws("alpha"), f.aliceTok))
			aliceGeneral := startReader(ctx, dialAs(t, ctx, base, ws("general"), f.aliceTok))
			botAlpha := startReader(ctx, dialAs(t, ctx, base, ws("alpha"), f.botTok))
			events := dialAs(t, ctx, base, "/v1/events/ws?channel=alpha", f.aliceTok)
			eventsDone := make(chan error, 1)
			go func() {
				for {
					if _, _, err := events.Read(ctx); err != nil {
						eventsDone <- err
						return
					}
				}
			}()
			before := f.postAs(t, f.botTok, "alpha", "before")
			aliceAlpha.waitForID(t, before)

			if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/alpha/bans/%d", f.alice.ID), f.rootTok, ""); rec.Code != 204 {
				t.Fatalf("ban = %d %s", rec.Code, rec.Body)
			}
			after := f.postAs(t, f.botTok, "alpha", "after")
			generalMsg := f.postAs(t, f.aliceTok, "general", "still here")

			err := aliceAlpha.waitClosed(t)
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Errorf("close = %v, want policy violation", err)
			}
			for _, id := range aliceAlpha.seen() {
				if id >= after {
					t.Errorf("the banned socket received message %d posted after the ban", id)
				}
			}
			select {
			case err := <-eventsDone:
				if websocket.CloseStatus(err) == -1 {
					t.Errorf("events socket ended without a close frame: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("the banned member's events socket on the channel is still open")
			}
			botAlpha.waitForID(t, after)
			aliceGeneral.waitForID(t, generalMsg)
			select {
			case <-aliceGeneral.done:
				t.Error("alice's socket on another channel was closed")
			default:
			}
			if got := callWS(t, base, ws("alpha"), f.aliceTok); got.status != 404 {
				t.Errorf("reconnect = %d, want 404", got.status)
			}
			// The credential is valid and other channels work.
			if rec := f.do(t, "GET", "/v1/whoami", f.aliceTok, ""); rec.Code != 200 {
				t.Errorf("whoami = %d", rec.Code)
			}
		})
	}
}

// TestBanAdmissionRefusals: operator and manager adds, by id and by name, and
// self-join, refuse a banned principal over the wire; unbanning allows a
// later explicit add and nothing else (design §6.2).
func TestBanAdmissionRefusals(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	bobID := f.ids["bob"]
	if got := f.put(t, "op2", bobID); got.status != http.StatusNoContent {
		t.Fatalf("pre-ban = %d", got.status)
	}
	refusal := wireResult{status: http.StatusNotFound}
	adds := []struct {
		name, as, method, path, body string
	}{
		{"operator PUT", "op2", "PUT", fmt.Sprintf("/v1/channels/ops/members/%d", bobID), ""},
		{"owner PUT with can_decide", "root", "PUT", fmt.Sprintf("/v1/channels/ops/members/%d", bobID), `{"can_decide":true}`},
		{"manager PUT", "mgr", "PUT", fmt.Sprintf("/v1/channels/ops/members/%d", bobID), ""},
		{"operator POST by name", "op2", "POST", "/v1/channels/ops/members", `{"name":"bob"}`},
		{"operator POST by id", "op2", "POST", "/v1/channels/ops/members", fmt.Sprintf(`{"id":%d}`, bobID)},
		{"manager POST by name", "mgr", "POST", "/v1/channels/ops/members", `{"name":"bob"}`},
		{"manager POST by id", "mgr", "POST", "/v1/channels/ops/members", fmt.Sprintf(`{"id":%d}`, bobID)},
	}
	for _, a := range adds {
		got := f.callREST(t, a.method, a.path, f.tokens[a.as], a.body)
		if got.status != refusal.status || !strings.Contains(got.body, "principal_not_found") {
			t.Errorf("%s of a banned principal = %d %s, want 404 principal_not_found", a.name, got.status, got.body)
		}
	}
	if f.member(t, f.ops, "bob") {
		t.Fatal("a banned principal was added")
	}
	// A principal who is not banned is added by the same routes.
	carolish := f.ids["ann2"]
	if got := f.callREST(t, "POST", "/v1/channels/ops2/members", f.tokens["root"], fmt.Sprintf(`{"id":%d}`, carolish)); got.status != http.StatusCreated {
		t.Errorf("an unbanned add = %d %s", got.status, got.body)
	}
	// Unban does not re-add.
	if got := f.lift(t, "op2", bobID); got.status != http.StatusNoContent || f.member(t, f.ops, "bob") {
		t.Fatalf("unban = %d, member = %v", got.status, f.member(t, f.ops, "bob"))
	}
	if got := f.callREST(t, "POST", "/v1/channels/ops/members", f.tokens["mgr"], `{"name":"bob"}`); got.status != http.StatusCreated {
		t.Errorf("explicit add after unban = %d %s", got.status, got.body)
	}
}

// TestBanSelfJoinOfAPublicChannel: a banned person cannot walk back into a
// public channel, and another channel stays open to them.
func TestBanSelfJoinOfAPublicChannel(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	for _, ch := range []store.Channel{f.ops, f.ops2} {
		if _, err := f.rawDB(t).Exec("UPDATE channels SET visibility = 'public' WHERE id = ?", ch.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.put(t, "op2", f.ids["bob"]); got.status != http.StatusNoContent {
		t.Fatal(got)
	}
	if got := f.callREST(t, "POST", "/v1/channels/ops/join", f.tokens["bob"], ""); got.status != http.StatusForbidden || !strings.Contains(got.body, schema.ErrorCodeChannelNotJoinable) {
		t.Errorf("banned self-join = %d %s, want 403 channel_not_joinable", got.status, got.body)
	}
	if got := f.callREST(t, "POST", "/v1/channels/ops2/join", f.tokens["bob"], ""); got.status != http.StatusOK && got.status != http.StatusCreated {
		t.Errorf("self-join of another channel = %d %s", got.status, got.body)
	}
	if ok, _ := f.srv.store.IsChannelMember(ctx, f.ops.ID, f.ids["bob"]); ok {
		t.Error("the banned person is a member")
	}
}

// TestBanVoiceEntitlementRace: a ban that commits before the final
// entitlement check in the session grant leaves no token; a ban that commits
// after it is terminated by the removal's normal revocation, and no absolute
// no-token claim is made (design §6.2).
func TestBanVoiceEntitlementRace(t *testing.T) {
	t.Run("before the final check", func(t *testing.T) {
		f := newBanFixture(t, presenceOpts{auth: AuthRequired})
		ctx := context.Background()
		room := f.holder(t, "ann2", f.ops)
		f.srv.voice.noteSession(room)
		ann, err := f.srv.store.PrincipalByID(ctx, f.ids["ann"])
		if err != nil {
			t.Fatal(err)
		}
		credID := f.credID(t, "ann")
		// The handler has checked ann; the ban lands before the session is built.
		if got := f.put(t, "mgr2", f.ids["ann"]); got.status == http.StatusNoContent {
			t.Fatal("a ghost manager banned")
		}
		if got := f.put(t, "root", f.ids["ann"]); got.status != http.StatusNoContent {
			t.Fatal(got)
		}
		base := len(f.lk.calls())
		_, _, err = f.srv.voiceSession(withCredentialID(ctx, credID), ann, f.ops)
		if err == nil || !strings.Contains(err.Error(), errVoiceNoLongerEntitled.Error()) {
			t.Fatalf("voiceSession after the ban = %v, want errVoiceNoLongerEntitled", err)
		}
		if calls := f.lk.calls()[base:]; len(calls) != 0 {
			t.Errorf("LiveKit was asked for a banned caller: %+v", calls)
		}
		holders, err := f.srv.store.VoiceRoomHolders(ctx, room.ID)
		if err != nil || len(holders) != 1 || holders[0].PrincipalID != f.ids["ann2"] {
			t.Errorf("holders = %+v, %v; want ann2 alone (no holder recorded for the banned caller)", holders, err)
		}
	})
	t.Run("a residual membership never signs", func(t *testing.T) {
		f := newBanFixture(t, presenceOpts{auth: AuthRequired})
		ctx := context.Background()
		f.seedBan(t, "mgr", "ann")
		ann, err := f.srv.store.PrincipalByID(ctx, f.ids["ann"])
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.srv.voiceSession(withCredentialID(ctx, f.credID(t, "ann")), ann, f.ops); err == nil {
			t.Fatal("a residual banned membership was given a voice session")
		}
	})
	t.Run("after the final check", func(t *testing.T) {
		f := newBanFixture(t, presenceOpts{auth: AuthRequired})
		room := f.inUse(t, f.ops)
		ann := f.identity("ann")
		// A token was signed and she is in the room.
		f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
		f.pass(t)
		if got := f.who(t, f.ops); got != "ann,ann2" {
			t.Fatalf("before: %q", got)
		}
		if got := f.put(t, "root", f.ids["ann"]); got.status != http.StatusNoContent {
			t.Fatal(got)
		}
		// The ban's follow-up ended her presence and removed her from LiveKit.
		if f.lk.in(room, ann) {
			t.Errorf("the banned participant is still in the room; calls = %+v", f.lk.calls())
		}
		if got := f.who(t, f.ops); strings.Contains(got, "ann,") || got == "ann" {
			t.Errorf("presence after the ban = %q", got)
		}
		if !f.lk.in(room, f.identity("ann2")) {
			t.Error("another participant was removed")
		}
		// And she cannot join again.
		if r := f.session(t, "ann", "ops"); r.status != http.StatusNotFound {
			t.Errorf("session after the ban = %d %s, want the unknown channel's 404", r.status, r.body)
		}
	})
}

// TestBanLeavesApprovalRightsAlone: banning someone who decided an approval
// changes no decision; a manager cannot ban an approver; and an approver the
// operator banned no longer decides (design §9 case 2).
func TestBanLeavesApprovalRightsAlone(t *testing.T) {
	f := newBanFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	if got := f.put(t, "mgr", f.ids["ann"]); got.status != http.StatusForbidden {
		t.Fatalf("manager banning an approver = %d", got.status)
	}
	if ok, err := f.srv.store.CanDecideApprovals(ctx, f.ops.ID, f.ids["ann"]); err != nil || !ok {
		t.Fatalf("ann's right after the refusal = %v, %v", ok, err)
	}
	if got := f.put(t, "root", f.ids["ann"]); got.status != http.StatusNoContent {
		t.Fatal(got)
	}
	if ok, err := f.srv.store.CanDecideApprovals(ctx, f.ops.ID, f.ids["ann"]); err != nil || ok {
		t.Errorf("a banned approver's right = %v, %v; want none (not a member)", ok, err)
	}
	// Nobody else's right moved.
	if f.member(t, f.ops2, "ann") != true {
		t.Error("ann lost her other channel")
	}
}

// TestBanLandsInsideTheGrant: the two sides of the race on the real session
// endpoint. A ban that commits while LiveKit is asked for the room (before
// the final entitlement check) is answered like one before the request, with
// no token; a ban after a token was signed is allowed to have signed it, and
// the next request is refused. No claim is made that a token never exists
// after a ban is requested (design §6.2).
func TestBanLandsInsideTheGrant(t *testing.T) {
	ctx := context.Background()
	t.Run("during CreateRoom", func(t *testing.T) {
		f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
		f.lk.onCreateRoom = func() {
			if _, err := f.srv.store.BanChannelMember(ctx, "system", f.ops.ID, f.ids["ann"], 0); err != nil {
				t.Errorf("ban: %v", err)
			}
		}
		res := f.session(t, "ann", "ops")
		if res.status != http.StatusNotFound || errCode(t, res.body) != "channel_not_found" {
			t.Fatalf("answer = %d %s, want the unknown-channel 404", res.status, res.body)
		}
		if strings.Contains(res.body, "token") || strings.Contains(res.body, "eyJ") {
			t.Errorf("the refusal carries a token: %s", res.body)
		}
		if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 0 {
			t.Errorf("voice_session_issued = %d, want 0", n)
		}
		if other := f.session(t, "bob", "ops"); other.body != res.body {
			t.Errorf("differs from a non-member's answer:\n %s\n %s", res.body, other.body)
		}
	})
	t.Run("after the token", func(t *testing.T) {
		f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
		first := f.session(t, "ann", "ops")
		if first.status != http.StatusOK || !strings.Contains(first.body, "eyJ") {
			t.Fatalf("first session = %d %s", first.status, first.body)
		}
		if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/ops/bans/%d", f.ids["ann"]), f.rootTok, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("ban = %d %s", rec.Code, rec.Body)
		}
		again := f.session(t, "ann", "ops")
		if again.status != http.StatusNotFound || strings.Contains(again.body, "eyJ") {
			t.Errorf("session after the ban = %d %s", again.status, again.body)
		}
	})
}
