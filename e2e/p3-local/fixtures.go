package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// The community the walkthrough runs in (issue #283). It is data, so that the
// provisioning and the read-back that checks it cannot disagree about it.

const (
	operatorName = "p3-operator"

	chGeneral = "general"
	chPrivate = "test-private"
	chNets    = "nets-demo"
)

var (
	humanNames = []string{"nick-test", "guest-test", "browser-test"}
	agentNames = []string{"worker-test", "observer-test"}
	everyone   = append(append([]string(nil), humanNames...), agentNames...)
)

// The three real runtimes --real-adapters can start, each as an agent of its
// own named real-<runtime>. The runtime is the name the adapter's profile has.
var realRuntimes = []struct{ name, profile, label string }{
	{"claude", "claude-code", "Claude Code"},
	{"codex", "codex", "Codex"},
	{"ollama", "ollama", "Ollama"},
}

// realAgent is the participant, and the principal, of a real runtime.
func realAgent(runtime string) string { return "real-" + runtime }

// agentNames is the agents of this run: the two scripted ones and one per real
// runtime asked for.
func (r *runner) agentNames() []string {
	names := append([]string(nil), agentNames...)
	for _, rt := range r.realAgents {
		names = append(names, realAgent(rt))
	}
	return names
}

// everyone is every principal of this run.
func (r *runner) everyone() []string {
	return append(append([]string(nil), humanNames...), r.agentNames()...)
}

// channelSpecs is the channels of this run: the real agents are in all three,
// as the manifest of worker-test they share grants.
func (r *runner) channelSpecs() []channelSpec {
	specs := slices.Clone(channelSpecs)
	for i := range specs {
		specs[i].members = slices.Clone(specs[i].members)
		for _, rt := range r.realAgents {
			specs[i].members = append(specs[i].members, realAgent(rt))
		}
	}
	return specs
}

// credSpecs is the credentials of this run.
func (r *runner) credSpecs() []credSpec {
	specs := slices.Clone(credSpecs)
	for _, rt := range r.realAgents {
		specs = append(specs, credSpec{realAgent(rt), realAgent(rt)})
	}
	return specs
}

// channelSpecs lists each channel and who is a member of it. "Everyone" is the
// five principals; the observer is in them, and its manifest is what keeps it
// to general.
type channelSpec struct {
	name    string
	members []string
}

var channelSpecs = []channelSpec{
	{chGeneral, everyone},
	{chPrivate, []string{"nick-test", "worker-test"}},
	{chNets, everyone},
}

// netSpecs are the nets of nets-demo and their roles. They overlap: guest-test
// is on both.
var netSpecs = []struct {
	name  string
	roles map[string]schema.NetRole
}{
	{"red", map[string]schema.NetRole{"nick-test": schema.NetRoleMember, "guest-test": schema.NetRoleMonitor}},
	{"blue", map[string]schema.NetRole{"guest-test": schema.NetRoleMember, "browser-test": schema.NetRoleMember}},
}

// credSpecs are the credentials issued, one per participant. nick-test has
// two, for the repeat login.
type credSpec struct{ participant, principal string }

var credSpecs = []credSpec{
	{"tui-a", "nick-test"},
	{"tui-b", "guest-test"},
	{"tui-a2", "nick-test"},
	{"browser", "browser-test"},
	{"agent-a", "worker-test"},
	{"agent-b", "observer-test"},
}

// manifestFor is the manifest of an agent, given the channel ids.
//
// worker-test can do everything the walkthrough asks of an agent, in all three
// channels, answering a whisper included. observer-test can read and keep a
// session, and nothing else: it cannot post, ask for approval or record a
// run, so what it is refused is part of what the walkthrough shows.
func manifestFor(agent string, channels map[string]int64) schema.PutAgentManifestRequestV1 {
	grant := func(names ...string) []schema.ChannelGrant {
		var grants []schema.ChannelGrant
		for _, n := range names {
			grants = append(grants, schema.ChannelGrant{
				ChannelID: channels[n],
				// whisper as well: a run that answers a whisper, and the reply
				// to it, need it (P3-05).
				Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionWhisper},
			})
		}
		return grants
	}
	if agent == "observer-test" {
		return schema.PutAgentManifestRequestV1{
			DisplayName: agent,
			Tier:        schema.AgentTierC,
			Capabilities: []schema.Capability{
				schema.CapabilityMessagesRead,
				schema.CapabilitySessionsManage,
			},
			Channels: []schema.ChannelGrant{{ChannelID: channels[chGeneral], Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}},
		}
	}
	return schema.PutAgentManifestRequestV1{
		DisplayName: agent,
		Tier:        schema.AgentTierC,
		Capabilities: []schema.Capability{
			schema.CapabilityMessagesRead,
			schema.CapabilityMessagesPost,
			schema.CapabilityApprovalsRequest,
			schema.CapabilityApprovalsAwait,
			schema.CapabilitySessionsManage,
			schema.CapabilityRunsManage,
		},
		Channels: grant(chGeneral, chPrivate, chNets),
	}
}

// fixtures is what provisioning made: the ids and the credentials.
type fixtures struct {
	operatorToken string
	operatorID    int64
	principals    map[string]int64
	channels      map[string]int64
	creds         map[string]credential // by participant
}

type credential struct {
	id        int64 // the credential's own id, for revoking it
	principal string
	token     string
}

func newFixtures(operatorToken string) *fixtures {
	return &fixtures{
		operatorToken: operatorToken,
		principals:    make(map[string]int64),
		channels:      make(map[string]int64),
		creds:         make(map[string]credential),
	}
}

// operatorEnv is the environment of a `conch` run with the operator's
// credential and a configuration directory of its own inside the root.
func (r *runner) operatorEnv() []string {
	return r.isolatedEnv(
		"CONCH_CONFIG_DIR="+filepath.Join(r.root, "config", "operator"),
		"CONCH_SERVER="+r.baseURL(),
		"CONCH_TOKEN="+r.fx.operatorToken)
}

// conch runs the conch CLI as the operator.
func (r *runner) conch(stdin string, args ...string) ([]byte, error) {
	return r.output(stdin, r.repo, r.operatorEnv(), r.bin.conch, args...)
}

// provision makes the community through `conch admin` and `conch nets`, as the
// operator, and reads it back through the API.
func (r *runner) provision() error {
	fx := r.fx
	if err := ensureDir(filepath.Join(r.root, "config", "operator")); err != nil {
		return err
	}
	for _, name := range r.everyone() {
		kind := "human"
		if slices.Contains(r.agentNames(), name) {
			kind = "agent"
		}
		out, err := r.conch("", "admin", "principal", "create", "--json", "--kind", kind, name)
		if err != nil {
			return err
		}
		var resp schema.CreatePrincipalResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			return fmt.Errorf("principal create %s: %w", name, err)
		}
		fx.principals[name] = resp.Principal.ID
	}
	for _, spec := range r.channelSpecs() {
		out, err := r.conch("", "admin", "channel", "create", "--json", spec.name)
		if err != nil {
			return err
		}
		var resp schema.CreateChannelResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			return fmt.Errorf("channel create %s: %w", spec.name, err)
		}
		fx.channels[spec.name] = resp.Channel.ID
	}
	for _, spec := range r.channelSpecs() {
		for _, member := range spec.members {
			if _, err := r.conch("", "admin", "member", "add", spec.name, id(fx.principals[member])); err != nil {
				return err
			}
		}
	}
	for _, spec := range netSpecs {
		if _, err := r.conch("", "nets", "create", chNets, spec.name); err != nil {
			return err
		}
		for _, member := range slices.Sorted(maps.Keys(spec.roles)) {
			args := []string{"nets", "add", chNets, spec.name, id(fx.principals[member])}
			if spec.roles[member] == schema.NetRoleMonitor {
				args = append(args, "--monitor")
			}
			if _, err := r.conch("", args...); err != nil {
				return err
			}
		}
	}
	for _, agent := range r.agentNames() {
		body, err := json.Marshal(manifestFor(agent, fx.channels))
		if err != nil {
			return err
		}
		if _, err := r.conch(string(body), "admin", "manifest", "put", id(fx.principals[agent])); err != nil {
			return err
		}
	}
	for _, spec := range r.credSpecs() {
		out, err := r.conch("", "admin", "credential", "issue", "--json", "--label", spec.participant, id(fx.principals[spec.principal]))
		if err != nil {
			return err
		}
		var resp schema.CreateCredentialResponseV1
		if err := json.Unmarshal(out, &resp); err != nil {
			return fmt.Errorf("credential issue %s: %w", spec.participant, err)
		}
		r.secret(resp.Token)
		fx.creds[spec.participant] = credential{id: resp.Credential.ID, principal: spec.principal, token: resp.Token}
	}
	step("provisioned %d principals, %d channels, %d nets, %d manifests and %d credentials", len(fx.principals), len(fx.channels), len(netSpecs), len(r.agentNames()), len(fx.creds))
	return r.verifyFixtures()
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// client returns an API client for token.
func (r *runner) client(token string) (*cli.Client, error) {
	c, err := cli.NewClient(r.baseURL(), &http.Client{Timeout: 30 * time.Second, Transport: newLoggingTransport()})
	if err != nil {
		return nil, err
	}
	return c.WithToken(token), nil
}

// verifyFixtures reads the community back through the API, as the operator
// and as each participant, and compares it with the specification above. A
// difference is a failure: the walkthrough is only meaningful on exactly this
// community.
func (r *runner) verifyFixtures() error {
	fx := r.fx
	ctx := runCtx
	op, err := r.client(fx.operatorToken)
	if err != nil {
		return err
	}
	me, err := op.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("whoami as the operator: %w", err)
	}
	if me.Name != operatorName || me.Role != schema.RoleOperator {
		return fmt.Errorf("the operator credential resolves to %q with role %q", me.Name, me.Role)
	}
	fx.operatorID = me.ID

	channels, err := op.ListChannels(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]int64)
	for _, ch := range channels.Channels {
		byName[ch.Name] = ch.ID
	}
	for _, spec := range r.channelSpecs() {
		if byName[spec.name] != fx.channels[spec.name] || byName[spec.name] == 0 {
			return fmt.Errorf("channel %s: the API lists id %d, provisioning made %d", spec.name, byName[spec.name], fx.channels[spec.name])
		}
		resp, err := op.ListChannelMembers(ctx, spec.name)
		if err != nil {
			return err
		}
		var got []string
		for _, m := range resp.Members {
			if m.PrincipalID == fx.operatorID {
				continue // the operator may be listed; it is no participant
			}
			if want := fx.principals[m.Name]; want != m.PrincipalID {
				return fmt.Errorf("channel %s lists member %q as id %d", spec.name, m.Name, m.PrincipalID)
			}
			got = append(got, m.Name)
		}
		if err := sameSet("members of "+spec.name, got, spec.members); err != nil {
			return err
		}
	}

	nets, err := op.ListNets(ctx, chNets)
	if err != nil {
		return err
	}
	if len(nets.Nets) != len(netSpecs) {
		return fmt.Errorf("%s has %d nets, want %d", chNets, len(nets.Nets), len(netSpecs))
	}
	for _, spec := range netSpecs {
		found := false
		for _, n := range nets.Nets {
			if n.Name != spec.name {
				continue
			}
			found = true
			got := make(map[string]string)
			for _, m := range n.Members {
				got[strconv.FormatInt(m.PrincipalID, 10)] = string(m.Role)
			}
			want := make(map[string]string)
			for member, role := range spec.roles {
				want[id(fx.principals[member])] = string(role)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) { // fmt prints maps in key order
				return fmt.Errorf("net %s has members %v, want %v", spec.name, got, want)
			}
		}
		if !found {
			return fmt.Errorf("net %s is missing from %s", spec.name, chNets)
		}
	}

	for _, agent := range r.agentNames() {
		got, err := op.GetManifest(ctx, fx.principals[agent])
		if err != nil {
			return err
		}
		if err := compareManifest(agent, got, manifestFor(agent, fx.channels)); err != nil {
			return err
		}
	}

	active := make(map[string]int)
	tokens := map[string]string{fx.operatorToken: operatorName}
	for _, spec := range r.credSpecs() {
		c := fx.creds[spec.participant]
		if other, dup := tokens[c.token]; dup {
			return fmt.Errorf("%s and %s hold the same credential", spec.participant, other)
		}
		tokens[c.token] = spec.participant
		pc, err := r.client(c.token)
		if err != nil {
			return err
		}
		who, err := pc.WhoAmI(ctx)
		if err != nil {
			return fmt.Errorf("whoami as %s: %w", spec.participant, err)
		}
		if who.ID != fx.principals[spec.principal] || who.Name != spec.principal {
			return fmt.Errorf("%s's credential resolves to %q (id %d), want %q", spec.participant, who.Name, who.ID, spec.principal)
		}
		if who.Role == schema.RoleOperator {
			return fmt.Errorf("%s's credential is an operator's", spec.participant)
		}
		active[spec.principal]++
	}
	for principal, want := range active {
		list, err := op.ListCredentials(ctx, fx.principals[principal])
		if err != nil {
			return err
		}
		n := 0
		for _, c := range list.Credentials {
			if c.RevokedAt == nil {
				n++
			}
		}
		if n != want {
			return fmt.Errorf("%s has %d active credentials, want %d", principal, n, want)
		}
	}
	step("read back through the API: channels, memberships, nets and roles, manifests and credentials all match")
	return nil
}

// compareManifest compares the stored manifest with the one that was put. The
// server adds the timestamps and the principal; those are not compared.
func compareManifest(agent string, got schema.AgentManifestV1, want schema.PutAgentManifestRequestV1) error {
	if got.DisplayName != want.DisplayName || got.Tier != want.Tier {
		return fmt.Errorf("manifest of %s: name %q tier %q, want %q %q", agent, got.DisplayName, got.Tier, want.DisplayName, want.Tier)
	}
	if err := sameSet("capabilities of "+agent, capabilityNames(got.Capabilities), capabilityNames(want.Capabilities)); err != nil {
		return err
	}
	grants := func(gs []schema.ChannelGrant) []string {
		var out []string
		for _, g := range gs {
			var perms []string
			for _, p := range g.Permissions {
				perms = append(perms, string(p))
			}
			sort.Strings(perms)
			out = append(out, fmt.Sprintf("%d:%s", g.ChannelID, strings.Join(perms, ",")))
		}
		return out
	}
	return sameSet("channel grants of "+agent, grants(got.Channels), grants(want.Channels))
}

func capabilityNames(cs []schema.Capability) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

// sameSet reports a difference between two lists compared as sets.
func sameSet(what string, got, want []string) error {
	g, w := append([]string(nil), got...), append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if strings.Join(g, "\x00") != strings.Join(w, "\x00") {
		return fmt.Errorf("%s: the API has %v, want %v", what, g, w)
	}
	return nil
}
