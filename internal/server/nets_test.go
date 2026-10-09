package server

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// netFixture is a memberFixture whose channel "general" carries two nets:
//
//	ops    alice (member), eve (monitor), bot (member agent)
//	secret dave (member)
//
// fred is a channel member on no net, blind is an agent that is a channel
// member but whose manifest lacks messages.read, and carol (from the
// memberFixture) is not in the channel.
type netFixture struct {
	*memberFixture
	dave, eve, fred, blind             store.Principal
	daveTok, eveTok, fredTok, blindTok string
	ops, secret                        store.Net
}

func newNetFixture(t *testing.T) *netFixture {
	t.Helper()
	ctx := context.Background()
	f := &netFixture{memberFixture: newMemberFixture(t)}
	st := f.srv.store
	mk := func(kind store.PrincipalKind, name string) (store.Principal, string) {
		p, err := st.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddChannelMember(ctx, "system", f.general.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
		_, tok, err := st.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		return p, tok
	}
	f.dave, f.daveTok = mk(store.PrincipalHuman, "dave")
	f.eve, f.eveTok = mk(store.PrincipalHuman, "eve")
	f.fred, f.fredTok = mk(store.PrincipalHuman, "fred")
	f.blind, f.blindTok = mk(store.PrincipalAgent, "blind")
	setAgentManifest(t, f.srv, f.blind.ID, []schema.Capability{schema.CapabilityMessagesPost}, f.general.ID)

	var err error
	if f.ops, err = st.CreateNet(ctx, "system", f.general.ID, "ops", f.root.ID); err != nil {
		t.Fatal(err)
	}
	if f.secret, err = st.CreateNet(ctx, "system", f.general.ID, "secret", f.root.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		net  string
		p    store.Principal
		role schema.NetRole
	}{
		{"ops", f.alice, schema.NetRoleMember},
		{"ops", f.eve, schema.NetRoleMonitor},
		{"ops", f.bot, schema.NetRoleMember},
		{"secret", f.dave, schema.NetRoleMember},
	} {
		if _, err := st.PutNetMember(ctx, "system", f.general.ID, m.net, m.p.ID, m.role, f.root.ID); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func netNames(nets []schema.NetV1) []string {
	names := []string{}
	for _, n := range nets {
		names = append(names, n.Name)
	}
	return names
}

func TestListNets(t *testing.T) {
	f := newNetFixture(t)
	const path = "/v1/channels/general/nets"

	tests := []struct {
		name      string
		token     string
		want      int
		wantCode  string
		wantNames []string
	}{
		{"operator sees every live net", f.rootTok, 200, "", []string{"ops", "secret"}},
		{"member on a net sees only that net", f.aliceTok, 200, "", []string{"ops"}},
		{"monitor sees the net", f.eveTok, 200, "", []string{"ops"}},
		{"member on the other net", f.daveTok, 200, "", []string{"secret"}},
		{"member on no net sees none", f.fredTok, 200, "", []string{}},
		{"agent with the read grant", f.botTok, 200, "", []string{"ops"}},
		{"agent without the read grant", f.blindTok, 403, "forbidden", nil},
		{"channel non-member", f.carolTok, 404, "channel_not_found", nil},
		{"unauthenticated", "", 401, "unauthenticated", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, "GET", path, tt.token, "")
			if tt.wantCode != "" {
				assertErrorBody(t, rec, tt.want, tt.wantCode)
				return
			}
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; %s", rec.Code, tt.want, rec.Body)
			}
			resp := decodeBody[schema.ListNetsResponseV1](t, rec)
			if got := netNames(resp.Nets); !reflect.DeepEqual(got, tt.wantNames) {
				t.Errorf("nets = %v, want %v", got, tt.wantNames)
			}
			// A net the caller is not on appears nowhere in the response.
			for _, hidden := range []string{"ops", "secret"} {
				if !contains(tt.wantNames, hidden) && strings.Contains(rec.Body.String(), `"`+hidden+`"`) {
					t.Errorf("response mentions %q, which the caller is not on: %s", hidden, rec.Body)
				}
			}
			for _, n := range resp.Nets {
				if err := n.Validate(); err != nil {
					t.Errorf("net %q does not validate: %v", n.Name, err)
				}
			}
		})
	}

	t.Run("roster is complete and ordered by principal id", func(t *testing.T) {
		resp := decodeBody[schema.ListNetsResponseV1](t, f.do(t, "GET", path, f.aliceTok, ""))
		want := []schema.NetMember{
			{PrincipalID: f.alice.ID, Role: schema.NetRoleMember},
			{PrincipalID: f.bot.ID, Role: schema.NetRoleMember},
			{PrincipalID: f.eve.ID, Role: schema.NetRoleMonitor},
		}
		if len(resp.Nets) != 1 || !reflect.DeepEqual(resp.Nets[0].Members, want) || resp.Nets[0].ChannelID != f.general.ID {
			t.Errorf("nets = %+v, want one net with roster %+v", resp.Nets, want)
		}
	})

	t.Run("a caller on no nets gets an empty array, never null", func(t *testing.T) {
		rec := f.do(t, "GET", path, f.fredTok, "")
		if got := strings.TrimSpace(rec.Body.String()); got != `{"nets":[]}` {
			t.Errorf("body = %s", got)
		}
	})

	t.Run("non-member response is byte-identical to an unknown channel's", func(t *testing.T) {
		for _, who := range []struct{ name, token string }{{"human", f.carolTok}} {
			got := f.callREST(t, "GET", path, who.token, "")
			ref := f.callREST(t, "GET", "/v1/channels/nosuch/nets", who.token, "")
			if got != ref {
				t.Errorf("%s: non-member %+v differs from unknown channel %+v", who.name, got, ref)
			}
		}
		// Likewise for an agent that is not in the channel.
		got := f.callREST(t, "GET", "/v1/channels/alpha/nets", f.botTok, "")
		ref := f.callREST(t, "GET", "/v1/channels/nosuch/nets", f.botTok, "")
		if got != ref || got.status != 404 {
			t.Errorf("non-member agent %+v differs from unknown channel %+v", got, ref)
		}
	})

	t.Run("an archived net drops out of every list", func(t *testing.T) {
		if rec := f.do(t, "DELETE", "/v1/channels/general/nets/ops", f.rootTok, ""); rec.Code != 204 {
			t.Fatal(rec.Code, rec.Body)
		}
		for _, c := range []struct{ token, want string }{{f.rootTok, "secret"}, {f.aliceTok, ""}} {
			resp := decodeBody[schema.ListNetsResponseV1](t, f.do(t, "GET", path, c.token, ""))
			got := strings.Join(netNames(resp.Nets), ",")
			if got != c.want {
				t.Errorf("nets = %q, want %q", got, c.want)
			}
		}
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestNetWritesRequireOperator sends every net write as every kind of caller.
// Only the operator gets through, and a refusal changes and audits nothing.
func TestNetWritesRequireOperator(t *testing.T) {
	f := newNetFixture(t)
	callers := []struct {
		name  string
		token string
		want  int
	}{
		{"channel member on the net", f.aliceTok, 403},
		{"channel member not on the net", f.fredTok, 403},
		{"monitor", f.eveTok, 403},
		{"agent with the grant", f.botTok, 403},
		{"agent without the grant", f.blindTok, 403},
		{"channel non-member", f.carolTok, 403},
		{"unauthenticated", "", 401},
	}
	dave := fmt.Sprintf("%d", f.dave.ID)
	writes := []struct{ name, method, path, body string }{
		{"create", "POST", "/v1/channels/general/nets", `{"name":"fresh"}`},
		{"archive", "DELETE", "/v1/channels/general/nets/ops", ""},
		{"put member", "PUT", "/v1/channels/general/nets/ops/members/" + dave, `{"role":"member"}`},
		{"delete member", "DELETE", "/v1/channels/general/nets/ops/members/" + fmt.Sprint(f.alice.ID), ""},
	}
	// Denials may be audited as such; no net_* change event may appear.
	netEvents := func() (n int) {
		for _, e := range f.audit(t) {
			if strings.HasPrefix(e.Action, "net_") {
				n++
			}
		}
		return n
	}
	before := netEvents()
	for _, w := range writes {
		for _, c := range callers {
			t.Run(w.name+"/"+c.name, func(t *testing.T) {
				rec := f.do(t, w.method, w.path, c.token, w.body)
				code := "forbidden"
				if c.want == 401 {
					code = "unauthenticated"
				}
				assertErrorBody(t, rec, c.want, code)
			})
		}
	}
	if n := netEvents(); n != before {
		t.Errorf("refused writes wrote %d net audit events", n-before)
	}
	nets, err := f.srv.store.ListNets(context.Background(), f.general.ID)
	if err != nil || len(nets) != 2 {
		t.Fatalf("nets = %v, %v; refused writes changed something", nets, err)
	}
	if ok, _ := f.srv.store.IsNetMember(context.Background(), f.ops.ID, f.alice.ID); !ok {
		t.Error("alice was removed from ops by a refused request")
	}
}

func TestNetEndpoints(t *testing.T) {
	f := newNetFixture(t)
	root := f.rootTok
	carol := fmt.Sprint(f.carol.ID)
	fred := fmt.Sprint(f.fred.ID)

	count := func(action string) int { return countAction(f.audit(t), action) }
	base := map[string]int{}
	actions := []string{store.AuditNetCreated, store.AuditNetArchived, store.AuditNetMemberAdded,
		store.AuditNetMemberRoleChanged, store.AuditNetMemberRemoved}
	for _, a := range actions {
		base[a] = count(a)
	}

	steps := []struct {
		name     string
		method   string
		path     string
		body     string
		want     int
		wantCode string
		// cumulative new audit events per action
		audit map[string]int
	}{
		{"create", "POST", "/v1/channels/general/nets", `{"name":"squad-1"}`, 201, "", map[string]int{store.AuditNetCreated: 1}},
		{"duplicate live name", "POST", "/v1/channels/general/nets", `{"name":"squad-1"}`, 409, "net_exists", map[string]int{store.AuditNetCreated: 1}},
		{"same name in another channel", "POST", "/v1/channels/alpha/nets", `{"name":"squad-1"}`, 201, "", map[string]int{store.AuditNetCreated: 2}},
		{"invalid name", "POST", "/v1/channels/general/nets", `{"name":"Bad Name"}`, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2}},
		{"empty name", "POST", "/v1/channels/general/nets", `{"name":""}`, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2}},
		{"malformed body", "POST", "/v1/channels/general/nets", `{`, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2}},
		{"unknown field", "POST", "/v1/channels/general/nets", `{"name":"x","members":[1]}`, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2}},
		{"unknown channel", "POST", "/v1/channels/nosuch/nets", `{"name":"x"}`, 404, "channel_not_found", map[string]int{store.AuditNetCreated: 2}},

		{"put adds", "PUT", "/v1/channels/general/nets/squad-1/members/" + fred, `{"role":"member"}`, 204, "", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1}},
		{"put again is idempotent", "PUT", "/v1/channels/general/nets/squad-1/members/" + fred, `{"role":"member"}`, 204, "", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1}},
		{"put changes the role", "PUT", "/v1/channels/general/nets/squad-1/members/" + fred, `{"role":"monitor"}`, 204, "", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},
		{"put invalid role", "PUT", "/v1/channels/general/nets/squad-1/members/" + fred, `{"role":"captain"}`, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},
		{"put empty body", "PUT", "/v1/channels/general/nets/squad-1/members/" + fred, ``, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},
		{"put non-member of the channel", "PUT", "/v1/channels/general/nets/squad-1/members/" + carol, `{"role":"member"}`, 400, "not_a_channel_member", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},
		{"put unknown principal", "PUT", "/v1/channels/general/nets/squad-1/members/9999", `{"role":"member"}`, 400, "not_a_channel_member", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},
		{"put bad principal id", "PUT", "/v1/channels/general/nets/squad-1/members/abc", `{"role":"member"}`, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},
		{"put unknown net", "PUT", "/v1/channels/general/nets/nosuch/members/" + fred, `{"role":"member"}`, 404, "net_not_found", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},
		{"put invalid net name", "PUT", "/v1/channels/general/nets/BAD/members/" + fred, `{"role":"member"}`, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},
		{"put unknown channel", "PUT", "/v1/channels/nosuch/nets/squad-1/members/" + fred, `{"role":"member"}`, 404, "channel_not_found", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1}},

		{"delete member", "DELETE", "/v1/channels/general/nets/squad-1/members/" + fred, ``, 204, "", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
		{"delete member again is idempotent", "DELETE", "/v1/channels/general/nets/squad-1/members/" + fred, ``, 204, "", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
		{"delete a stranger is a no-op", "DELETE", "/v1/channels/general/nets/squad-1/members/" + carol, ``, 204, "", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
		{"delete member of unknown net", "DELETE", "/v1/channels/general/nets/nosuch/members/" + fred, ``, 404, "net_not_found", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
		{"delete zero principal id", "DELETE", "/v1/channels/general/nets/squad-1/members/0", ``, 400, "invalid_request", map[string]int{store.AuditNetCreated: 2, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},

		{"archive", "DELETE", "/v1/channels/general/nets/squad-1", ``, 204, "", map[string]int{store.AuditNetCreated: 2, store.AuditNetArchived: 1, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
		{"archive again", "DELETE", "/v1/channels/general/nets/squad-1", ``, 404, "net_not_found", map[string]int{store.AuditNetCreated: 2, store.AuditNetArchived: 1, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
		{"put on an archived net", "PUT", "/v1/channels/general/nets/squad-1/members/" + fred, `{"role":"member"}`, 404, "net_not_found", map[string]int{store.AuditNetCreated: 2, store.AuditNetArchived: 1, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
		{"archive unknown channel", "DELETE", "/v1/channels/nosuch/nets/squad-1", ``, 404, "channel_not_found", map[string]int{store.AuditNetCreated: 2, store.AuditNetArchived: 1, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
		{"name is reusable after archive", "POST", "/v1/channels/general/nets", `{"name":"squad-1"}`, 201, "", map[string]int{store.AuditNetCreated: 3, store.AuditNetArchived: 1, store.AuditNetMemberAdded: 1, store.AuditNetMemberRoleChanged: 1, store.AuditNetMemberRemoved: 1}},
	}
	var createdIDs []int64
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			rec := f.do(t, st.method, st.path, root, st.body)
			switch {
			case st.wantCode != "":
				assertErrorBody(t, rec, st.want, st.wantCode)
			case st.want == 201:
				if rec.Code != 201 {
					t.Fatalf("status = %d, want 201; %s", rec.Code, rec.Body)
				}
				resp := decodeBody[schema.CreateNetResponseV1](t, rec)
				if err := resp.Net.Validate(); err != nil {
					t.Errorf("created net does not validate: %v", err)
				}
				if !strings.Contains(rec.Body.String(), `"members":[]`) {
					t.Errorf("a new net must encode an empty roster as [], got %s", rec.Body)
				}
				createdIDs = append(createdIDs, resp.Net.ID)
			default:
				if rec.Code != st.want || rec.Body.Len() != 0 {
					t.Fatalf("status = %d body %q, want %d with empty body", rec.Code, rec.Body, st.want)
				}
			}
			for _, a := range actions {
				if got := count(a) - base[a]; got != st.audit[a] {
					t.Errorf("%s events = %d, want %d", a, got, st.audit[a])
				}
			}
		})
	}
	// Reusing the name gave a different net.
	if len(createdIDs) < 3 || createdIDs[0] == createdIDs[2] {
		t.Errorf("created ids = %v; a recreated name must get a new id", createdIDs)
	}
}

// TestNetAuditContent checks who, what and where is recorded.
func TestNetAuditContent(t *testing.T) {
	f := newNetFixture(t)
	rootActor := fmt.Sprintf("principal:%d", f.root.ID)
	fred := fmt.Sprint(f.fred.ID)
	for _, st := range []struct{ method, path, body string }{
		{"POST", "/v1/channels/general/nets", `{"name":"audited"}`},
		{"PUT", "/v1/channels/general/nets/audited/members/" + fred, `{"role":"member"}`},
		{"PUT", "/v1/channels/general/nets/audited/members/" + fred, `{"role":"monitor"}`},
		{"DELETE", "/v1/channels/general/nets/audited/members/" + fred, ""},
		{"DELETE", "/v1/channels/general/nets/audited", ""},
	} {
		if rec := f.do(t, st.method, st.path, f.rootTok, st.body); rec.Code/100 != 2 {
			t.Fatalf("%s %s = %d %s", st.method, st.path, rec.Code, rec.Body)
		}
	}
	// The net is archived by now, so find its id through the audit trail.
	var created store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == store.AuditNetCreated && strings.Contains(e.Detail, "name=audited") {
			created = e
		}
	}
	if created.ID == 0 {
		t.Fatal("no net_created event for the new net")
	}
	var got []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Subject == created.Subject {
			got = append(got, e)
		}
	}
	wantActions := []string{store.AuditNetCreated, store.AuditNetMemberAdded, store.AuditNetMemberRoleChanged, store.AuditNetMemberRemoved, store.AuditNetArchived}
	if len(got) != len(wantActions) {
		t.Fatalf("events for %s = %+v, want %v", created.Subject, got, wantActions)
	}
	for i, e := range got {
		if e.Action != wantActions[i] || e.Actor != rootActor {
			t.Errorf("event %d = %+v, want action %s by %s", i, e, wantActions[i], rootActor)
		}
		if !strings.Contains(e.Detail, fmt.Sprintf("channel=%d", f.general.ID)) {
			t.Errorf("event %d detail %q lacks the channel", i, e.Detail)
		}
		if i >= 1 && i <= 3 && !strings.Contains(e.Detail, "principal="+fred) {
			t.Errorf("event %d detail %q lacks the subject principal", i, e.Detail)
		}
	}
}

// Removing a principal from the channel through the API takes them off its
// nets and audits it, and the roster the other members see reflects that.
func TestChannelMemberRemovalLeavesNets(t *testing.T) {
	f := newNetFixture(t)
	before := countAction(f.audit(t), store.AuditNetMemberRemoved)
	if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", f.alice.ID), f.rootTok, ""); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body)
	}
	if got := countAction(f.audit(t), store.AuditNetMemberRemoved) - before; got != 1 {
		t.Errorf("net_member_removed events = %d, want 1", got)
	}
	resp := decodeBody[schema.ListNetsResponseV1](t, f.do(t, "GET", "/v1/channels/general/nets", f.eveTok, ""))
	for _, n := range resp.Nets {
		for _, m := range n.Members {
			if m.PrincipalID == f.alice.ID {
				t.Errorf("alice is still on net %q", n.Name)
			}
		}
	}
	// She is no longer in the channel, so the nets endpoint is a 404 for her.
	assertErrorBody(t, f.do(t, "GET", "/v1/channels/general/nets", f.aliceTok, ""), 404, "channel_not_found")
}

func TestNetsAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	alice := fmt.Sprint(f.alice.ID)
	ghost := "4" // exists, is not a member of "general"
	steps := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1/channels/general/nets", `{"name":"ops"}`, 201},
		{"POST", "/v1/channels/general/nets", `{"name":"ops"}`, 409},
		{"GET", "/v1/channels/general/nets", "", 200},
		{"PUT", "/v1/channels/general/nets/ops/members/" + alice, `{"role":"monitor"}`, 204},
		{"PUT", "/v1/channels/general/nets/ops/members/" + alice, `{"role":"monitor"}`, 204},
		{"PUT", "/v1/channels/general/nets/ops/members/" + ghost, `{"role":"member"}`, 400},
		{"DELETE", "/v1/channels/general/nets/ops/members/" + alice, "", 204},
		{"DELETE", "/v1/channels/general/nets/nosuch", "", 404},
		{"GET", "/v1/channels/nosuch/nets", "", 404},
	}
	for _, st := range steps {
		if rec := f.do(t, st.method, st.path, "", st.body); rec.Code != st.want {
			t.Fatalf("%s %s = %d, want %d; %s", st.method, st.path, rec.Code, st.want, rec.Body)
		}
	}
	// With no caller the list is every live net, empty rosters as [].
	rec := f.do(t, "GET", "/v1/channels/general/nets", "", "")
	resp := decodeBody[schema.ListNetsResponseV1](t, rec)
	if len(resp.Nets) != 1 || resp.Nets[0].Name != "ops" || !strings.Contains(rec.Body.String(), `"members":[]`) {
		t.Errorf("auth-off list = %s", rec.Body)
	}
	if rec := f.do(t, "DELETE", "/v1/channels/general/nets/ops", "", ""); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if got := f.do(t, "GET", "/v1/channels/general/nets", "", "").Body.String(); strings.TrimSpace(got) != `{"nets":[]}` {
		t.Errorf("list after archive = %s", got)
	}
	// Every audit event is attributed to the system actor.
	var n int
	for _, e := range f.audit(t) {
		if strings.HasPrefix(e.Action, "net_") {
			n++
			if e.Actor != "system" {
				t.Errorf("auth-off %s actor = %q, want system", e.Action, e.Actor)
			}
		}
	}
	// created, added (the repeat is a no-op), removed, archived.
	if n != 4 {
		t.Errorf("net audit events = %d, want 4", n)
	}
}
