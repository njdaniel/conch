package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// tapBroadcaster is the Config.Broadcaster tap. Scoped messages must
// never reach it: it carries the v0 envelope, which cannot express an audience.
type tapBroadcaster struct {
	mu  sync.Mutex
	ids []int64
}

func (b *tapBroadcaster) BroadcastMessage(_ context.Context, m schema.MessageV0) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ids = append(b.ids, m.ID)
}

func (b *tapBroadcaster) seen() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int64(nil), b.ids...)
}

// person is a principal with a credential.
type person struct {
	store.Principal
	token string
}

// scopedFixture is a server with authentication required and one channel,
// "ops", holding people in every audience situation the leak tests need:
//
//	net1 (nets "net1"): ann (member), nina (member), mona (monitor),
//	                    aria (agent, member), dina (member)
//	net2:               nina (member), wes (member)
//
// olga and the operator root are channel members on no net; nora is not in the
// channel; arlo is an agent that may post but not read; nomad is an agent
// outside the channel.
type scopedFixture struct {
	*authFixture
	rec    *tapBroadcaster
	base   string // ws://host of a real listener
	ops    store.Channel
	people map[string]*person
	net1   store.Net
	net2   store.Net
}

func (f *scopedFixture) p(name string) *person {
	p, ok := f.people[name]
	if !ok {
		panic("no such person " + name)
	}
	return p
}

func newScopedFixture(t *testing.T) *scopedFixture {
	t.Helper()
	ctx := context.Background()
	rec := &tapBroadcaster{}
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, Broadcaster: rec})
	root, _, rootTok, err := srv.store.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	f := &scopedFixture{authFixture: &authFixture{srv: srv, root: root, rootTok: rootTok}, rec: rec, people: map[string]*person{}}
	f.base = wsTestServer(t, srv)
	if f.ops, err = srv.store.CreateChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	f.people["root"] = &person{Principal: root, token: rootTok}
	f.join(t, "root")
	for _, spec := range []struct {
		name   string
		kind   store.PrincipalKind
		member bool
	}{
		{"ann", store.PrincipalHuman, true}, {"nina", store.PrincipalHuman, true}, {"mona", store.PrincipalHuman, true},
		{"wes", store.PrincipalHuman, true}, {"olga", store.PrincipalHuman, true}, {"dina", store.PrincipalHuman, true},
		{"nora", store.PrincipalHuman, false},
		{"aria", store.PrincipalAgent, true}, {"arlo", store.PrincipalAgent, true}, {"nomad", store.PrincipalAgent, false},
	} {
		f.add(t, spec.name, spec.kind, spec.member)
	}
	// aria may read and post in ops. arlo may only post there, so it can be in
	// an audience but cannot read. nomad would be allowed everything, but is
	// not in the channel.
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost)
	f.manifest(t, "arlo", nil, schema.ChannelPermissionPost)
	f.manifest(t, "nomad", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost)

	f.net1, f.net2 = f.net(t, "net1"), f.net(t, "net2")
	f.seat(t, "net1", "ann", schema.NetRoleMember)
	f.seat(t, "net1", "nina", schema.NetRoleMember)
	f.seat(t, "net1", "mona", schema.NetRoleMonitor)
	f.seat(t, "net1", "aria", schema.NetRoleMember)
	f.seat(t, "net1", "dina", schema.NetRoleMember)
	f.seat(t, "net2", "nina", schema.NetRoleMember)
	f.seat(t, "net2", "wes", schema.NetRoleMember)
	return f
}

func (f *scopedFixture) add(t *testing.T, name string, kind store.PrincipalKind, member bool) {
	t.Helper()
	ctx := context.Background()
	pr, err := f.srv.store.CreatePrincipal(ctx, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.srv.store.CreateCredential(ctx, "system", pr.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.people[name] = &person{Principal: pr, token: tok}
	if member {
		f.join(t, name)
	}
}

func (f *scopedFixture) join(t *testing.T, name string) {
	t.Helper()
	if _, err := f.srv.store.AddChannelMember(context.Background(), "system", f.ops.ID, f.p(name).ID, 0); err != nil {
		t.Fatal(err)
	}
}

// manifest gives agent name every capability and the listed permissions in ops.
func (f *scopedFixture) manifest(t *testing.T, name string, caps []schema.Capability, perms ...schema.ChannelPermission) {
	t.Helper()
	if caps == nil {
		caps = schema.Capabilities()
	}
	req := schema.PutAgentManifestRequestV1{DisplayName: name, Tier: schema.AgentTierC, Capabilities: caps,
		Channels: []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: perms}}}
	if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", f.p(name).ID, req); err != nil {
		t.Fatal(err)
	}
}

func (f *scopedFixture) net(t *testing.T, name string) store.Net {
	t.Helper()
	n, err := f.srv.store.CreateNet(context.Background(), "system", f.ops.ID, name, f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *scopedFixture) seat(t *testing.T, net, who string, role schema.NetRole) {
	t.Helper()
	if _, err := f.srv.store.PutNetMember(context.Background(), "system", f.ops.ID, net, f.p(who).ID, role, 0); err != nil {
		t.Fatal(err)
	}
}

func (f *scopedFixture) postV2(t *testing.T, who string, body string) wireResult {
	t.Helper()
	tok := ""
	if who != "" {
		tok = f.p(who).token
	}
	return f.callREST(t, "POST", "/v2/channels/ops/messages", tok, body)
}

// mustPost posts and returns the stored message's id, failing on any refusal.
func (f *scopedFixture) mustPost(t *testing.T, who, body string) int64 {
	t.Helper()
	res := f.postV2(t, who, body)
	if res.status != http.StatusCreated {
		t.Fatalf("%s post %s = %d %s", who, body, res.status, res.body)
	}
	var resp schema.PostMessageResponseV2
	if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
		t.Fatalf("decode post response %s: %v", res.body, err)
	}
	return resp.Message.ID
}

func netAudience(id int64) string {
	return fmt.Sprintf(`"audience":{"kind":"net","net_id":%d}`, id)
}

func whisperAudience(ids ...int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return fmt.Sprintf(`"audience":{"kind":"principals","principal_ids":[%s]}`, strings.Join(parts, ","))
}

// socket is an open WebSocket, read after the fact.
type socket struct {
	conn   *websocket.Conn
	status int
	body   string
}

// openSocket dials path and keeps the connection if the upgrade is accepted.
func openSocket(t *testing.T, base, path, token string) *socket {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var hdr http.Header
	if token != "" {
		hdr = http.Header{"Authorization": {"Bearer " + token}}
	}
	conn, resp, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		t.Cleanup(func() { _ = conn.CloseNow() })
		return &socket{conn: conn, status: http.StatusSwitchingProtocols}
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
	return &socket{status: resp.StatusCode, body: sb.String()}
}

// readUntil reads frames until the one with id sentinel (inclusive) and
// returns their raw JSON. The sentinel is a channel-wide message every
// subscriber of the channel must receive, so a socket that never gets it fails
// instead of hanging.
func (s *socket) readUntil(t *testing.T, sentinel int64) []json.RawMessage {
	t.Helper()
	var frames []json.RawMessage
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var raw json.RawMessage
		err := wsjson.Read(ctx, s.conn, &raw)
		cancel()
		if err != nil {
			t.Fatalf("read frame after %d frames: %v", len(frames), err)
		}
		frames = append(frames, raw)
		var head struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatalf("decode frame %s: %v", raw, err)
		}
		if head.ID == sentinel {
			return frames
		}
	}
}

// idsOf extracts the message ids from raw frames.
func idsOf(t *testing.T, frames []json.RawMessage) []int64 {
	t.Helper()
	ids := []int64{}
	for _, raw := range frames {
		var head struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, head.ID)
	}
	return ids
}

// listIDs decodes the message ids (and next_after) of a list response body.
func listIDs(t *testing.T, body string) (ids []int64, next int64) {
	t.Helper()
	var resp struct {
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
		NextAfter int64 `json:"next_after"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode list %q: %v", body, err)
	}
	ids = []int64{}
	for _, m := range resp.Messages {
		ids = append(ids, m.ID)
	}
	return ids, resp.NextAfter
}

// mcpReadChannel calls the read_channel tool with a bearer token and reports
// the HTTP status, the tool error code if any, and the ids it returned.
func (f *scopedFixture) mcpReadChannel(t *testing.T, token string, args map[string]any) (status int, code string, ids []int64) {
	t.Helper()
	out := f.mcpCall(t, token, "read_channel", args)
	if out.status != http.StatusOK || out.isError {
		return out.status, out.code, nil
	}
	var resp struct {
		Result struct {
			Structured struct {
				Messages []struct {
					ID int64 `json:"id"`
				} `json:"messages"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.body), &resp); err != nil {
		t.Fatalf("decode read_channel %s: %v", out.body, err)
	}
	ids = []int64{}
	for _, m := range resp.Result.Structured.Messages {
		ids = append(ids, m.ID)
	}
	return out.status, "", ids
}

func (f *scopedFixture) mcpCall(t *testing.T, token, tool string, args map[string]any) mcpOutcome {
	t.Helper()
	az := &authzFixture{srv: f.srv}
	out, err := az.callWithToken(token, tool, args)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *scopedFixture) messageCount(t *testing.T) int {
	t.Helper()
	n, err := f.srv.store.CountMessages(context.Background(), f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *scopedFixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	return countAction(f.audit(t), action)
}
