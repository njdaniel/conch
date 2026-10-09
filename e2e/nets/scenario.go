package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

const channelName = "ops"

// padCredentials, padChannels and padNets are how many credentials, channels
// and nets are created only to use up low ids, so that no two things in the
// scenario share a number: principals are 1..10, then come the credentials,
// then the channel, then the nets (setUp checks it). On a fresh instance they
// would otherwise all start at 1, and a server that looked a reader, a
// channel or a net up by the wrong kind of id would go unnoticed.
const (
	padCredentials = 10
	padChannels    = 25
	padNets        = 40
)

// person is one participant. The operator and lead are humans; the eight
// squad members are agents, driven over MCP.
type person struct {
	name  string
	agent bool
	id    int64
	token string
	mcp   *mcpclient.Client
	// perms are the channel permissions an agent's manifest grants in "ops",
	// beyond read.
	perms []schema.ChannelPermission
}

// message is one row of the table that every expectation is built from.
// There is no way to post a message without stating who may see it.
type message struct {
	key     string
	author  string
	via     string   // "" (MCP for an agent, REST for a human) or "cli"
	net     string   // audience: the net's name, or
	to      []string // audience: a whisper's recipients (the author is implied), or neither: the whole channel
	viewers []string // every participant who may see it, the author included
	id      int64    // assigned when posted
}

func (m *message) scoped() bool { return m.net != "" || m.to != nil }

// body is the message text. It is unique per key and recognisable in a dump.
func (m *message) body() string { return "MSG-" + m.key + "-text" }

type harness struct {
	bin     binaries
	proc    *conchdProc
	secrets []string

	people    []*person
	byName    map[string]*person
	channelID int64
	netIDs    map[string]int64
	// roster is the harness's own record of each net's seats (name -> role),
	// kept as it changes the nets, used to check the table before each post.
	roster map[string]map[string]schema.NetRole

	table  []*message
	posted []*message // in post order, the table's rows and any probes
	cli    *conchUser

	cleanupMu sync.Mutex
	cleanups  []func()
}

func (h *harness) secret(t string) {
	if t != "" {
		h.secrets = append(h.secrets, t)
	}
}

// redact hides every credential this run issued from a failure message.
func (h *harness) redact(s string) string {
	for _, t := range h.secrets {
		s = strings.ReplaceAll(s, t, "[token]")
	}
	return s
}

func (h *harness) op() api { return api{baseURL: h.proc.baseURL, token: h.proc.operatorToken} }

func (h *harness) as(p *person) api { return api{baseURL: h.proc.baseURL, token: p.token} }

func (h *harness) person(name string) *person { return h.byName[name] }

func (h *harness) names() []string {
	out := make([]string, len(h.people))
	for i, p := range h.people {
		out[i] = p.name
	}
	return out
}

func (h *harness) ids(names []string) []int64 {
	out := make([]int64, 0, len(names))
	for _, n := range names {
		out = append(out, h.person(n).id)
	}
	slices.Sort(out)
	return out
}

func (h *harness) row(key string) *message {
	for _, m := range h.table {
		if m.key == key {
			return m
		}
	}
	panic("nets-check: no table row " + key)
}

// label names a message id in a failure.
func (h *harness) label(id int64) string {
	for _, m := range h.posted {
		if m.id == id {
			return m.key
		}
	}
	return "not posted by this run"
}

func scenario(h *harness) (err error) {
	h.byName = make(map[string]*person)
	h.netIDs = make(map[string]int64)
	h.roster = make(map[string]map[string]schema.NetRole)

	if err := h.setUp(); err != nil {
		return fmt.Errorf("set up: %w", err)
	}
	h.cli, err = newConchUser(h.bin.conch, h.proc.baseURL)
	if err != nil {
		return err
	}
	h.onCleanup(h.cli.cleanup)
	if _, err := h.cli.run(h.person("lead").token+"\n", "login"); err != nil {
		return fmt.Errorf("lead: conch login: %w", err)
	}
	step("layout built through the public API: %d participants, 3 nets, a CLI session for lead", len(h.people))

	h.table = h.newTable()

	// Every socket is open before the first post, so nothing posted can be
	// missed: the server registers a subscription before it upgrades.
	socks, err := h.openSockets()
	if err != nil {
		return err
	}
	defer closeSockets(socks)
	step("%d WebSocket subscriptions open (v0, v1 and v2 for every participant)", len(socks))

	// Phase 1: one message into every audience.
	for _, key := range []string{"wide-op", "wide-a2", "alpha-1", "bravo-1", "command-1", "whisper-a3-b2", "whisper-a1-lead"} {
		if err := h.post(h.row(key)); err != nil {
			return err
		}
	}
	step("posted: channel-wide x2, one message per net, whisper a3->b2, whisper a1->lead")

	// Refusals: each is refused with its documented answer and stores nothing.
	if err := h.refusals(); err != nil {
		return err
	}

	// Post-time snapshot: a membership change reveals no history and hides
	// none.
	if err := h.snapshotChanges(); err != nil {
		return err
	}

	// The human with the CLI: refused as a monitor, then promoted, then
	// sends to a net and whispers; `conch tail` runs through the end.
	tail, err := h.startTail()
	if err != nil {
		return err
	}
	h.onCleanup(tail.stop)
	if err := h.cliPhase(); err != nil {
		return err
	}

	// The sentinel: a channel-wide message posted last, so a reader that
	// has it has had everything delivered before it.
	if err := h.post(h.row("sentinel-1")); err != nil {
		return err
	}
	late, err := h.openLateSockets()
	if err != nil {
		return err
	}
	defer closeSockets(late)
	if err := h.post(h.row("sentinel-2")); err != nil {
		return err
	}
	step("sentinels posted")

	for _, m := range h.table {
		if m.id == 0 {
			return fmt.Errorf("table row %s was never posted", m.key)
		}
	}
	if err := h.checkSockets(socks, late); err != nil {
		return err
	}
	if err := h.checkTail(tail); err != nil {
		return err
	}
	if err := h.checkReads(); err != nil {
		return err
	}
	return h.checkAudit()
}

// setUp creates the channel, the principals with credentials, membership and
// manifests, and the three nets.
func (h *harness) setUp() error {
	op := h.op()
	var me schema.WhoAmIResponseV1
	if err := op.call(http.MethodGet, "/v1/whoami", nil, &me); err != nil {
		return err
	}
	if me.Role != schema.RoleOperator {
		return fmt.Errorf("the bootstrapped principal has role %q, want operator", me.Role)
	}
	// Credentials first use up the numbers the principals will have, and the
	// operator then works with a new one: on a fresh instance credential n
	// would belong to principal n for everyone, and a server that identified
	// the reader by the wrong one of the two would show everyone exactly the
	// right messages.
	var second schema.CreateCredentialResponseV1
	for i := 0; i <= padCredentials; i++ {
		if err := op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", me.ID), schema.CreateCredentialRequestV1{Label: fmt.Sprintf("operator-%d", i+2)}, &second); err != nil {
			return fmt.Errorf("issue an operator credential: %w", err)
		}
		h.secret(second.Token)
	}
	h.proc.operatorToken = second.Token
	op = h.op()
	h.add(&person{name: "operator", id: me.ID, token: op.token})
	used := map[int64]string{me.ID: "principal operator", second.Credential.ID: "credential of operator"}
	// claim records an id the scenario uses and fails if another kind of
	// thing already has that number.
	claim := func(id int64, what string) error {
		if other, taken := used[id]; taken {
			return fmt.Errorf("%s has id %d, which is also the id of %s: the layout must keep its id spaces apart", what, id, other)
		}
		used[id] = what
		return nil
	}

	// For the same reason the channel and the nets get ids no principal or
	// credential has: channels and nets made only to use up the low numbers.
	for i := 1; i <= padChannels; i++ {
		var pad schema.CreateChannelResponse
		if err := op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: fmt.Sprintf("pad-%d", i)}, &pad); err != nil {
			return fmt.Errorf("create padding channel: %w", err)
		}
		if i > 1 {
			continue
		}
		for j := 1; j <= padNets; j++ {
			if err := op.call(http.MethodPost, "/v1/channels/pad-1/nets", schema.CreateNetRequestV1{Name: fmt.Sprintf("pad-%d", j)}, nil); err != nil {
				return fmt.Errorf("create padding net: %w", err)
			}
		}
	}
	var ch schema.CreateChannelResponse
	if err := op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: channelName}, &ch); err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	h.channelID = ch.Channel.ID
	if err := claim(h.channelID, "channel "+channelName); err != nil {
		return err
	}

	read := schema.ChannelPermissionRead
	netPerm := schema.ChannelPermissionPostNet
	people := []*person{
		{name: "lead"},
		{name: "a1", agent: true, perms: []schema.ChannelPermission{read, netPerm, schema.ChannelPermissionWhisper}},
		{name: "a2", agent: true, perms: []schema.ChannelPermission{read, netPerm, schema.ChannelPermissionPost}},
		{name: "a3", agent: true, perms: []schema.ChannelPermission{read, netPerm, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent}},
		{name: "a4", agent: true, perms: []schema.ChannelPermission{read, netPerm}},
		{name: "b1", agent: true, perms: []schema.ChannelPermission{read, netPerm}},
		{name: "b2", agent: true, perms: []schema.ChannelPermission{read, netPerm}},
		{name: "b3", agent: true, perms: []schema.ChannelPermission{read, netPerm}},
		{name: "b4", agent: true, perms: []schema.ChannelPermission{read, netPerm}},
	}
	for _, p := range people {
		kind := schema.PrincipalHuman
		if p.agent {
			kind = schema.PrincipalAgent
		}
		var created schema.CreatePrincipalResponse
		if err := op.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: kind, Name: p.name}, &created); err != nil {
			return fmt.Errorf("create %s: %w", p.name, err)
		}
		p.id = created.Principal.ID
		var cred schema.CreateCredentialResponseV1
		if err := op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", p.id), schema.CreateCredentialRequestV1{Label: p.name}, &cred); err != nil {
			return fmt.Errorf("issue credential for %s: %w", p.name, err)
		}
		if cred.Token == "" {
			return fmt.Errorf("issuing a credential for %s returned no token", p.name)
		}
		p.token = cred.Token
		h.secret(p.token)
		if err := claim(p.id, "principal "+p.name); err != nil {
			return err
		}
		if err := claim(cred.Credential.ID, "credential of "+p.name); err != nil {
			return err
		}
		if err := op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", channelName, p.id), nil, nil); err != nil {
			return fmt.Errorf("add %s to %s: %w", p.name, channelName, err)
		}
		if p.agent {
			manifest := schema.PutAgentManifestRequestV1{
				DisplayName:  p.name,
				Tier:         schema.AgentTierA,
				Capabilities: schema.Capabilities(),
				Channels:     []schema.ChannelGrant{{ChannelID: h.channelID, Permissions: p.perms}},
			}
			if err := op.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", p.id), manifest, nil); err != nil {
				return fmt.Errorf("write manifest of %s: %w", p.name, err)
			}
			p.mcp = mcpclient.New(h.proc.baseURL, p.token)
			if err := p.mcp.Initialize(context.Background(), "nets-check-"+p.name); err != nil {
				return fmt.Errorf("mcp initialize as %s: %w", p.name, err)
			}
		}
		h.add(p)
	}

	for _, n := range []struct {
		name  string
		seats map[string]schema.NetRole
	}{
		{"alpha", seats(schema.NetRoleMember, "a1", "a2", "a3", "a4")},
		{"bravo", seats(schema.NetRoleMember, "b1", "b2", "b3", "b4")},
		{"command", map[string]schema.NetRole{"a1": schema.NetRoleMember, "b1": schema.NetRoleMember, "lead": schema.NetRoleMonitor}},
	} {
		var created schema.CreateNetResponseV1
		if err := op.call(http.MethodPost, "/v1/channels/"+channelName+"/nets", schema.CreateNetRequestV1{Name: n.name}, &created); err != nil {
			return fmt.Errorf("create net %s: %w", n.name, err)
		}
		h.netIDs[n.name] = created.Net.ID
		if err := claim(created.Net.ID, "net "+n.name); err != nil {
			return err
		}
		h.roster[n.name] = make(map[string]schema.NetRole)
		for _, name := range sortedKeys(n.seats) {
			if err := h.seat(n.name, name, n.seats[name]); err != nil {
				return err
			}
		}
	}
	return h.checkNetList()
}

func (h *harness) add(p *person) {
	h.people = append(h.people, p)
	h.byName[p.name] = p
}

func seats(role schema.NetRole, names ...string) map[string]schema.NetRole {
	out := make(map[string]schema.NetRole, len(names))
	for _, n := range names {
		out[n] = role
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seat puts a participant on a net (or changes its role) and records it.
func (h *harness) seat(netName, who string, role schema.NetRole) error {
	path := fmt.Sprintf("/v1/channels/%s/nets/%s/members/%d", channelName, netName, h.person(who).id)
	if err := h.op().call(http.MethodPut, path, schema.PutNetMemberRequestV1{Role: role}, nil); err != nil {
		return fmt.Errorf("seat %s on %s as %s: %w", who, netName, role, err)
	}
	h.roster[netName][who] = role
	return nil
}

func (h *harness) unseat(netName, who string) error {
	path := fmt.Sprintf("/v1/channels/%s/nets/%s/members/%d", channelName, netName, h.person(who).id)
	if err := h.op().call(http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("remove %s from %s: %w", who, netName, err)
	}
	delete(h.roster[netName], who)
	return nil
}

// checkNetList compares what the API tells each participant the nets are
// with the harness's own record: the operator is shown every net, everyone
// else exactly the nets they are on, each with its whole roster. Who is on a
// net a participant is not on, and that the net exists, is not theirs to see.
func (h *harness) checkNetList() error {
	for _, p := range h.people {
		var list schema.ListNetsResponseV1
		if err := h.as(p).call(http.MethodGet, "/v1/channels/"+channelName+"/nets", nil, &list); err != nil {
			return fmt.Errorf("%s: list nets: %w", p.name, err)
		}
		want := make(map[string]bool)
		for name, seats := range h.roster {
			if _, on := seats[p.name]; on || p.name == "operator" {
				want[name] = true
			}
		}
		got := make(map[string]bool)
		for _, n := range list.Nets {
			if got[n.Name] {
				return fmt.Errorf("%s is shown net %s twice", p.name, n.Name)
			}
			got[n.Name] = true
			if !want[n.Name] {
				return fmt.Errorf("%s is shown net %q (id %d), which it is not on", p.name, n.Name, n.ID)
			}
			seats := h.roster[n.Name]
			if h.netIDs[n.Name] != n.ID || len(n.Members) != len(seats) {
				return fmt.Errorf("%s is shown net %s as id %d with %d seats, want id %d and %d seats", p.name, n.Name, n.ID, len(n.Members), h.netIDs[n.Name], len(seats))
			}
			for _, m := range n.Members {
				var name string
				for _, q := range h.people {
					if q.id == m.PrincipalID {
						name = q.name
					}
				}
				if seats[name] != m.Role || name == "" {
					return fmt.Errorf("%s is shown net %s with principal %d as %q, want %q", p.name, n.Name, m.PrincipalID, m.Role, seats[name])
				}
			}
		}
		for name := range want {
			if !got[name] {
				return fmt.Errorf("%s is not shown net %s, which it is on", p.name, name)
			}
		}
	}
	return nil
}

// newTable is the one place that says who may see each message.
func (h *harness) newTable() []*message {
	everyone := h.names()
	alpha := []string{"a1", "a2", "a3", "a4"}
	bravo := []string{"b1", "b2", "b3", "b4"}
	return []*message{
		{key: "wide-op", author: "operator", viewers: everyone},
		{key: "wide-a2", author: "a2", viewers: everyone},
		{key: "alpha-1", author: "a1", net: "alpha", viewers: alpha},
		{key: "bravo-1", author: "b2", net: "bravo", viewers: bravo},
		{key: "command-1", author: "b1", net: "command", viewers: []string{"a1", "b1", "lead"}},
		{key: "whisper-a3-b2", author: "a3", to: []string{"b2"}, viewers: []string{"a3", "b2"}},
		{key: "whisper-a1-lead", author: "a1", to: []string{"lead"}, viewers: []string{"a1", "lead"}},
		// After a4 left alpha: a4 keeps alpha-1, is not given alpha-2.
		{key: "alpha-2", author: "a1", net: "alpha", viewers: []string{"a1", "a2", "a3"}},
		// After b4 joined command: b4 is not given command-1, is given command-2.
		{key: "command-2", author: "a1", net: "command", viewers: []string{"a1", "b1", "b4", "lead"}},
		// After lead was promoted from monitor to member of command.
		{key: "cli-net", author: "lead", via: "cli", net: "command", viewers: []string{"a1", "b1", "b4", "lead"}},
		{key: "cli-whisper", author: "lead", via: "cli", to: []string{"a1"}, viewers: []string{"a1", "lead"}},
		// Posted while lead's `conch tail` is running, to audiences lead is
		// not in: without them nothing the tail must not show is ever sent
		// during its lifetime, and its check could not fail on a leak.
		{key: "bravo-2", author: "b3", net: "bravo", viewers: bravo},
		{key: "whisper-a3-b2-2", author: "a3", to: []string{"b2"}, viewers: []string{"a3", "b2"}},
		{key: "sentinel-1", author: "operator", viewers: everyone},
		{key: "sentinel-2", author: "operator", viewers: everyone},
	}
}

// checkRow refuses a table row that disagrees with what the harness itself
// knows: a channel-wide message is for everyone, a whisper is for its
// recipients and author, a net message for the net's roster as it stands.
func (h *harness) checkRow(m *message) error {
	if len(m.viewers) == 0 {
		return fmt.Errorf("table row %s does not say who may see it", m.key)
	}
	got := append([]string(nil), m.viewers...)
	sort.Strings(got)
	var want []string
	switch {
	case m.net != "":
		want = sortedKeys(h.roster[m.net])
	case m.to != nil:
		want = append(append([]string(nil), m.to...), m.author)
		sort.Strings(want)
	default:
		want = append([]string(nil), h.names()...)
		sort.Strings(want)
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("table row %s lists viewers %v, but the audience it addresses is %v", m.key, got, want)
	}
	return nil
}

func (m *message) audience(h *harness) *schema.Audience {
	switch {
	case m.net != "":
		return &schema.Audience{Kind: schema.AudienceKindNet, NetID: h.netIDs[m.net]}
	case m.to != nil:
		return &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: h.ids(m.to)}
	}
	return nil
}

// post sends one table row from its author through the surface the author
// uses, checks what comes back, and records the id.
func (h *harness) post(m *message) error {
	if err := h.checkRow(m); err != nil {
		return err
	}
	author := h.person(m.author)
	var posted schema.MessageV2
	switch {
	case m.via == "cli":
		args := []string{"send"}
		if m.net != "" {
			args = append(args, "--net", m.net)
		} else {
			args = append(args, "--to", joinIDs(h.ids(m.to)))
		}
		if out, err := h.cli.run("", append(args, channelName, m.body())...); err != nil {
			return fmt.Errorf("%s: %w (output: %s)", m.key, err, out)
		}
		found, err := h.findByBody(author, m.body())
		if err != nil {
			return fmt.Errorf("%s: %w", m.key, err)
		}
		posted = found
	case author.agent:
		resp, err := author.mcp.PostMessageTo(context.Background(), channelName, m.body(), m.audience(h))
		if err != nil {
			return fmt.Errorf("%s: post_message as %s: %w", m.key, m.author, err)
		}
		posted = resp.Message
	default:
		var resp schema.PostMessageResponseV2
		req := schema.PostMessageRequestV2{Body: m.body(), Audience: m.audience(h)}
		raw, err := h.as(author).callRaw(http.MethodPost, "/v2/channels/"+channelName+"/messages", req, &resp)
		if err != nil {
			return fmt.Errorf("%s: REST post as %s: %w", m.key, m.author, err)
		}
		if err := closedMessages(raw, "message"); err != nil {
			return fmt.Errorf("%s: REST post as %s: %w", m.key, m.author, err)
		}
		posted = resp.Message
	}
	if err := posted.Validate(); err != nil {
		return fmt.Errorf("%s: the server returned an invalid v2 message: %w", m.key, err)
	}
	if posted.AuthorID != author.id || posted.Body != m.body() {
		return fmt.Errorf("%s: the server returned author %d, want %d (%s)", m.key, posted.AuthorID, author.id, m.author)
	}
	if err := h.checkEcho(m, posted.Audience); err != nil {
		return err
	}
	m.id = posted.ID
	h.posted = append(h.posted, m)
	return nil
}

// checkEcho compares the audience the server stored with the one asked for.
func (h *harness) checkEcho(m *message, got *schema.Audience) error {
	want := m.audience(h)
	switch {
	case want == nil && got == nil:
		return nil
	case want == nil || got == nil:
		return fmt.Errorf("%s: the server returned audience %+v, want %+v", m.key, got, want)
	case m.net != "":
		if got.Kind != want.Kind || got.NetID != want.NetID {
			return fmt.Errorf("%s: the server returned audience %+v, want %+v", m.key, got, want)
		}
	default:
		// The server adds the author and sorts.
		wantIDs := h.ids(append(append([]string(nil), m.to...), m.author))
		if got.Kind != want.Kind || !slices.Equal(got.PrincipalIDs, wantIDs) {
			return fmt.Errorf("%s: the server returned audience %+v, want principals %v", m.key, got, wantIDs)
		}
	}
	return nil
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}

// findByBody finds a message the reader can see by its (unique) body: the CLI
// does not print the id of what it sent.
func (h *harness) findByBody(reader *person, body string) (schema.MessageV2, error) {
	msgs, err := h.restV2(reader)
	if err != nil {
		return schema.MessageV2{}, err
	}
	for _, m := range msgs {
		if m.Body == body {
			return m, nil
		}
	}
	return schema.MessageV2{}, fmt.Errorf("%s cannot see the message it just sent", reader.name)
}

// ---------------------------------------------------------------- refusals

func (h *harness) refusals() error {
	lead, b3, a1 := h.person("lead"), h.person("b3"), h.person("a1")
	commandID := h.netIDs["command"]
	ctx := context.Background()

	// 1. The monitor of a net may listen and not transmit.
	if err := h.refused("lead (a monitor) posts to command over REST", func() error {
		req := schema.PostMessageRequestV2{
			Body:     "MSG-refused-monitor-text",
			Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: commandID},
		}
		status, body, err := h.as(lead).do(http.MethodPost, "/v2/channels/"+channelName+"/messages", req)
		if err != nil {
			return err
		}
		if status != http.StatusForbidden || errorCode(body) != "forbidden" {
			return fmt.Errorf("status %d (%s), want 403 forbidden", status, errorCode(body))
		}
		return nil
	}); err != nil {
		return err
	}

	// 2. A net the sender is not on and a net that does not exist are
	// answered identically.
	var onNotMember, onMissing error
	post := func(netID int64) error {
		_, err := b3.mcp.PostMessageTo(ctx, channelName, "MSG-refused-b3-text", &schema.Audience{Kind: schema.AudienceKindNet, NetID: netID})
		return err
	}
	if err := h.refused("b3 posts to alpha (not on it) over MCP", func() error {
		onNotMember = post(h.netIDs["alpha"])
		return wantToolError(onNotMember, "net_not_found")
	}); err != nil {
		return err
	}
	missingID := h.netIDs["alpha"] + h.netIDs["bravo"] + h.netIDs["command"] + 1000
	if err := h.refused("b3 posts to a net that does not exist over MCP", func() error {
		onMissing = post(missingID)
		return wantToolError(onMissing, "net_not_found")
	}); err != nil {
		return err
	}
	var te1, te2 *mcpclient.ToolError
	if !errors.As(onNotMember, &te1) || !errors.As(onMissing, &te2) {
		return fmt.Errorf("b3 net refusals are not tool errors: %w / %w", onNotMember, onMissing)
	}
	if te1.Error() != te2.Error() || te1.Code != te2.Code || te1.Message != te2.Message {
		return fmt.Errorf("b3 can tell alpha (exists, not a member) from a net that does not exist: %q versus %q", te1.Error(), te2.Error())
	}
	step("b3 gets one answer for alpha and for a missing net: %s", te1.Code)

	// 3. whisper without whisper_agent to an agent.
	return h.refused("a1 (whisper, no whisper_agent) whispers to agent a2 over MCP", func() error {
		_, err := a1.mcp.PostMessageTo(ctx, channelName, "MSG-refused-whisper-text",
			&schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{h.person("a2").id}})
		return wantToolError(err, "forbidden")
	})
}

func wantToolError(err error, code string) error {
	var te *mcpclient.ToolError
	if err == nil {
		return fmt.Errorf("the post succeeded, want tool error %s", code)
	}
	if !errors.As(err, &te) {
		return fmt.Errorf("got %w, want tool error %s", err, code)
	}
	if te.Code != code {
		return fmt.Errorf("got tool error %q, want %s", te.Code, code)
	}
	return nil
}

// refused runs an attempt that must be refused and checks that nothing was
// stored: what every participant can see is the same before and after.
func (h *harness) refused(what string, attempt func() error) error {
	before, err := h.snapshot()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := attempt(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	after, err := h.snapshot()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	for _, p := range h.people {
		if added := diffSets(after[p.name], before[p.name]); len(added) > 0 {
			return fmt.Errorf("%s: refused, but %s now sees message ids %v that it did not before", what, p.name, added)
		}
		if gone := diffSets(before[p.name], after[p.name]); len(gone) > 0 {
			return fmt.Errorf("%s: %s lost message ids %v", what, p.name, gone)
		}
	}
	step("refused, nothing stored: %s", what)
	return nil
}

// snapshot is the set of message ids each participant sees on its primary
// surface: MCP read_channel for an agent, REST v2 for a human.
func (h *harness) snapshot() (map[string]idSet, error) {
	out := make(map[string]idSet, len(h.people))
	for _, p := range h.people {
		var msgs []schema.MessageV2
		var err error
		if p.agent {
			msgs, err = h.mcpRead(p)
		} else {
			msgs, err = h.restV2(p)
		}
		if err != nil {
			return nil, fmt.Errorf("read as %s: %w", p.name, err)
		}
		out[p.name] = idsOf(msgs)
	}
	return out, nil
}

// ------------------------------------------------------------ the snapshot

func (h *harness) snapshotChanges() error {
	// a4 leaves alpha; alpha-2 goes to the remaining three.
	if err := h.unseat("alpha", "a4"); err != nil {
		return err
	}
	if err := h.post(h.row("alpha-2")); err != nil {
		return err
	}
	// b4 joins command; command-2 reaches it, command-1 never does.
	if err := h.seat("command", "b4", schema.NetRoleMember); err != nil {
		return err
	}
	if err := h.post(h.row("command-2")); err != nil {
		return err
	}
	if err := h.checkNetList(); err != nil {
		return err
	}
	// Checked right here as well as at the end: a4 still reads alpha-1 but
	// not alpha-2, and b4 reads command-2 and nothing posted earlier.
	for _, c := range []struct {
		who, key string
		want     bool
	}{
		{"a4", "alpha-1", true}, {"a4", "alpha-2", false},
		{"b4", "command-1", false}, {"b4", "command-2", true},
	} {
		msgs, err := h.mcpRead(h.person(c.who))
		if err != nil {
			return err
		}
		if got := idsOf(msgs)[h.row(c.key).id]; got != c.want {
			return fmt.Errorf("post-time snapshot: %s sees message %d (%s) on MCP read_channel = %v, want %v", c.who, h.row(c.key).id, c.key, got, c.want)
		}
	}
	step("post-time snapshot: a4 keeps alpha-1 and does not get alpha-2; b4 gets command-2 and not command-1")
	return nil
}

// ---------------------------------------------------------------- the CLI

func (h *harness) cliPhase() error {
	// As a monitor, lead's `conch send --net command` is refused.
	for _, key := range []string{"bravo-2", "whisper-a3-b2-2"} {
		if err := h.post(h.row(key)); err != nil {
			return err
		}
	}
	if err := h.refused("lead (a monitor) runs conch send --net command", func() error {
		out, err := h.cli.run("", "send", "--net", "command", channelName, "MSG-refused-cli-text")
		if err == nil {
			return fmt.Errorf("the CLI sent to a net on which lead only monitors (output: %s)", out)
		}
		// Refused by the server for that reason, not failed for another.
		if !strings.Contains(out, "forbidden") {
			return fmt.Errorf("conch send --net as a monitor failed, but not with the server's refusal (output: %s)", out)
		}
		return nil
	}); err != nil {
		return err
	}
	// The operator promotes lead; lead may now transmit on command.
	if err := h.seat("command", "lead", schema.NetRoleMember); err != nil {
		return err
	}
	if err := h.post(h.row("cli-net")); err != nil {
		return err
	}
	out, err := h.cli.run("", "send", "--to", fmt.Sprint(h.person("a1").id), channelName, h.row("cli-whisper").body())
	if err != nil {
		return fmt.Errorf("cli-whisper: %w (output: %s)", err, out)
	}
	if !strings.Contains(out, "audit log") {
		return fmt.Errorf("conch send --to did not say whispers are in the audit log; output: %s", out)
	}
	m := h.row("cli-whisper")
	if err := h.checkRow(m); err != nil {
		return err
	}
	found, err := h.findByBody(h.person("lead"), m.body())
	if err != nil {
		return err
	}
	if err := h.checkEcho(m, found.Audience); err != nil {
		return err
	}
	m.id = found.ID
	h.posted = append(h.posted, m)
	step("conch send --net and --to as lead, after a refusal as a monitor")
	return nil
}

// withDeadline is a context for one wait; a wait for a message is bounded by
// the message arriving, with this only as the cap. A socket opened before the
// first post waits under it for the whole run, so it is generous: a loaded CI
// runner must not turn a slow run into a failure.
func withDeadline() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}
