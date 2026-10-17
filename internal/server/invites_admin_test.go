package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Operator administration of invites (issue #438): the list of every invite,
// joiners, the two bulk disables, the right to invite and the pause switch.

// joinThrough redeems code as name and returns the new principal and token.
func (f *inviteFixture) joinThrough(t *testing.T, code, name string) (schema.PrincipalV0, string) {
	t.Helper()
	rec := f.redeem(t, code, name)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem %s = %d %s", name, rec.Code, rec.Body)
	}
	res := decodeBody[schema.RedeemInviteResponseV1](t, rec)
	return res.Principal, res.Token
}

// asOperator makes principal id a second operator, with a fresh credential.
// No route or store call makes an operator after the first, so it goes
// underneath both, through the database file: the exemption tests need an
// operator who joined through an invite.
func (f *inviteFixture) asOperator(t *testing.T, id int64) string {
	t.Helper()
	ctx := context.Background()
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "UPDATE principals SET role = 'operator' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.srv.store.CreateCredential(ctx, "system", id, "operator", nil)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func auditRows(t *testing.T, f *authFixture, action string) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func inviteIDs(l []schema.InviteV1) []int64 {
	out := []int64{}
	for _, inv := range l {
		out = append(out, inv.ID)
	}
	return out
}

// An operator lists every invite, narrowed by kind, state and maker; anyone
// else lists their own, whatever they ask for.
func TestListInvitesOperatorSeesEvery(t *testing.T) {
	f := newInviteFixture(t, Config{})
	if _, err := f.srv.store.SetMayInvite(context.Background(), "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	used := f.createInvite(t, f.rootTok, personal("general"))
	f.joinThrough(t, used.Code, "first")
	link := f.createInvite(t, f.rootTok, openLink("red"))
	alices := f.createInvite(t, f.aliceTok, personal("red"))
	closed := f.createInvite(t, f.aliceTok, personal("red"))
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", closed.Invite.ID), f.aliceTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d", rec.Code)
	}

	tests := []struct {
		name  string
		tok   string
		query string
		want  []int64
	}{
		{"operator, everything", f.rootTok, "", []int64{closed.Invite.ID, alices.Invite.ID, link.Invite.ID, used.Invite.ID}},
		{"operator, open links", f.rootTok, "?kind=open", []int64{link.Invite.ID}},
		{"operator, still open", f.rootTok, "?state=open", []int64{alices.Invite.ID, link.Invite.ID}},
		{"operator, used", f.rootTok, "?state=used", []int64{used.Invite.ID}},
		{"operator, closed", f.rootTok, "?state=closed", []int64{closed.Invite.ID}},
		{"operator, by maker", f.rootTok, fmt.Sprintf("?created_by=%d", f.alice.ID), []int64{closed.Invite.ID, alices.Invite.ID}},
		{"operator, two filters", f.rootTok, fmt.Sprintf("?created_by=%d&state=open", f.alice.ID), []int64{alices.Invite.ID}},
		{"member, everything is their own", f.aliceTok, "", []int64{closed.Invite.ID, alices.Invite.ID}},
		{"member, filtered", f.aliceTok, "?state=closed", []int64{closed.Invite.ID}},
		{"member, by themselves", f.aliceTok, fmt.Sprintf("?created_by=%d", f.alice.ID), []int64{closed.Invite.ID, alices.Invite.ID}},
		{"member, by someone else", f.aliceTok, fmt.Sprintf("?created_by=%d", f.root.ID), []int64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.send(t, "GET", "/v1/invites"+tt.query, tt.tok, "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("list = %d %s", rec.Code, rec.Body)
			}
			got := decodeBody[schema.ListInvitesResponseV1](t, rec)
			if ids := inviteIDs(got.Invites); !slices.Equal(ids, tt.want) {
				t.Errorf("ids = %v, want %v", ids, tt.want)
			}
		})
	}

	// The state and use count are on each invite.
	all := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.rootTok, "", ""))
	for _, inv := range all.Invites {
		if inv.ID == used.Invite.ID && (inv.Uses != 1 || inv.MaxUses == nil || *inv.MaxUses != 1) {
			t.Errorf("used invite = %+v, want 1 of 1 uses", inv)
		}
		if inv.ID == closed.Invite.ID && inv.ClosedAt == nil {
			t.Errorf("closed invite = %+v, want closed_at", inv)
		}
	}

	for _, q := range []string{"?kind=forever", "?state=gone", "?created_by=0", "?created_by=x"} {
		assertErrorBody(t, f.send(t, "GET", "/v1/invites"+q, f.rootTok, "", ""), http.StatusBadRequest, "invalid_request")
	}
}

// Every new route: an operator gets the documented answer; a member or an
// agent is refused 403 and audited as access_denied, and nothing changes; no
// credential is a 401.
func TestInviteAdminRoutesAccess(t *testing.T) {
	// {invite} is the fixture's open link; {root} and {alice} are principals.
	routes := []struct {
		name, method, path, body string
		want                     int
	}{
		{"joiners", "GET", "/v1/invites/{invite}/principals", "", http.StatusOK},
		{"disable joiners", "POST", "/v1/invites/{invite}/disable-joiners", `{"dry_run":false}`, http.StatusOK},
		{"disable invitees", "POST", "/v1/principals/{root}/disable-invitees", `{"dry_run":false,"depth":"all"}`, http.StatusOK},
		{"invite permission", "PUT", "/v1/principals/{alice}/invite-permission", `{"may_invite":true}`, http.StatusNoContent},
		{"read settings", "GET", "/v1/invites/settings", "", http.StatusOK},
		{"pause", "PUT", "/v1/invites/settings", `{"paused":true}`, http.StatusOK},
	}
	for _, rt := range routes {
		for _, who := range []string{"member", "agent", "unauthenticated", "operator"} {
			t.Run(rt.name+"/"+who, func(t *testing.T) {
				f := newInviteFixture(t, Config{})
				link := f.createInvite(t, f.rootTok, openLink("red"))
				_, joinerTok := f.joinThrough(t, link.Code, "joiner")
				path := strings.NewReplacer("{invite}", fmt.Sprint(link.Invite.ID), "{root}", fmt.Sprint(f.root.ID),
					"{alice}", fmt.Sprint(f.alice.ID)).Replace(rt.path)
				tok := map[string]string{"member": f.aliceTok, "agent": f.botTok, "operator": f.rootTok}[who]
				denials := countAudit(t, f.authFixture, "access_denied")
				rec := f.send(t, rt.method, path, tok, rt.body, "")
				switch who {
				case "operator":
					if rec.Code != rt.want {
						t.Fatalf("status = %d, want %d; %s", rec.Code, rt.want, rec.Body)
					}
					return
				case "unauthenticated":
					assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
				default:
					assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
					if got := countAudit(t, f.authFixture, "access_denied"); got != denials+1 {
						t.Errorf("access_denied rows = %d, want %d", got, denials+1)
					}
				}
				// Nothing changed: the joiner still signs in, alice still may
				// not invite, the link is open and invites are not paused.
				if r := f.send(t, "GET", "/v1/whoami", joinerTok, "", ""); r.Code != http.StatusOK {
					t.Errorf("joiner whoami after a refused call = %d", r.Code)
				}
				if who := decodeBody[schema.WhoAmIResponseV1](t, f.send(t, "GET", "/v1/whoami", f.aliceTok, "", "")); who.MayInvite {
					t.Error("a refused call granted alice the right to invite")
				}
				if paused, err := f.srv.store.InvitesPaused(context.Background()); err != nil || paused {
					t.Errorf("paused = %v, %v after a refused call", paused, err)
				}
				list := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites?state=open", f.rootTok, "", ""))
				if !slices.Equal(inviteIDs(list.Invites), []int64{link.Invite.ID}) {
					t.Errorf("open invites after a refused call = %v", inviteIDs(list.Invites))
				}
			})
		}
	}
}

// With authentication off there is no operator to act: every new route is
// the 401 of the other invite routes, and nothing is written.
func TestInviteAdminRoutesAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	before := len(f.audit(t))
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/invites/1/principals", ""},
		{"POST", "/v1/invites/1/disable-joiners", `{"dry_run":false}`},
		{"POST", "/v1/principals/1/disable-invitees", `{"dry_run":false}`},
		{"PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", f.alice.ID), `{"may_invite":true}`},
		{"GET", "/v1/invites/settings", ""},
		{"PUT", "/v1/invites/settings", `{"paused":true}`},
	} {
		assertErrorBody(t, f.do(t, c.method, c.path, "", c.body), http.StatusUnauthorized, "unauthenticated")
	}
	if after := len(f.audit(t)); after != before {
		t.Errorf("audit rows %d -> %d", before, after)
	}
}

func TestInviteJoinersRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	link := f.createInvite(t, f.rootTok, openLink("red"))
	other := f.createInvite(t, f.rootTok, openLink("red"))
	wren, _ := f.joinThrough(t, link.Code, "wren")
	piper, _ := f.joinThrough(t, link.Code, "piper")
	f.joinThrough(t, other.Code, "elsewhere")
	if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", piper.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disable = %d", rec.Code)
	}

	rec := f.send(t, "GET", fmt.Sprintf("/v1/invites/%d/principals", link.Invite.ID), f.rootTok, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("joiners = %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[schema.InviteJoinersResponseV1](t, rec)
	if len(got.Joiners) != 2 {
		t.Fatalf("joiners = %+v, want wren and piper", got.Joiners)
	}
	for i, want := range []struct {
		p        schema.PrincipalV0
		disabled bool
	}{{wren, false}, {piper, true}} {
		j := got.Joiners[i]
		if j.ID != want.p.ID || j.Name != want.p.Name || j.Disabled != want.disabled || j.CreatedAt.Time().IsZero() {
			t.Errorf("joiner %d = %+v, want %s disabled=%v", i, j, want.p.Name, want.disabled)
		}
	}

	// An invite nobody used is [], not null; one that does not exist is 404.
	unused := f.createInvite(t, f.rootTok, openLink("blue"))
	if body := f.send(t, "GET", fmt.Sprintf("/v1/invites/%d/principals", unused.Invite.ID), f.rootTok, "", "").Body.String(); !strings.Contains(body, `"joiners":[]`) {
		t.Errorf("no joiners = %s, want []", body)
	}
	assertErrorBody(t, f.send(t, "GET", "/v1/invites/9999/principals", f.rootTok, "", ""), http.StatusNotFound, schema.ErrorCodeInviteNotFound)
	assertErrorBody(t, f.send(t, "GET", "/v1/invites/x/principals", f.rootTok, "", ""), http.StatusBadRequest, "invalid_request")
}

// adminSnapshot is everything a bulk disable could change: every audit row,
// every principal, every credential and every invite.
type adminSnapshot struct {
	audit       []store.AuditEvent
	principals  []store.Principal
	credentials [][]schema.CredentialV1
	invites     []schema.InviteV1
}

func takeAdminSnapshot(t *testing.T, f *inviteFixture) adminSnapshot {
	t.Helper()
	ctx := context.Background()
	snap := adminSnapshot{audit: f.audit(t)}
	for id := int64(1); ; id++ {
		p, err := f.srv.store.PrincipalByID(ctx, id)
		if err != nil {
			break
		}
		snap.principals = append(snap.principals, p)
		creds, err := f.srv.store.ListCredentials(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		snap.credentials = append(snap.credentials, creds)
	}
	invites, err := f.srv.store.ListInvites(ctx, store.InviteFilter{}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	snap.invites = invites
	return snap
}

// disableJoinersFixture is an open link with four joiners: two ordinary
// ones, one already disabled, and one made an operator, who is also the
// caller of the cases that need one.
type disableJoinersFixture struct {
	*inviteFixture
	link                    schema.CreateInviteResponseV1
	wren, piper, gone, boss schema.PrincipalV0
	wrenTok, piperTok       string
	bossTok                 string
}

func newDisableJoinersFixture(t *testing.T) *disableJoinersFixture {
	t.Helper()
	f := &disableJoinersFixture{inviteFixture: newInviteFixture(t, Config{})}
	f.link = f.createInvite(t, f.rootTok, openLink("red"))
	f.wren, f.wrenTok = f.joinThrough(t, f.link.Code, "wren")
	f.piper, f.piperTok = f.joinThrough(t, f.link.Code, "piper")
	f.gone, _ = f.joinThrough(t, f.link.Code, "gone")
	f.boss, _ = f.joinThrough(t, f.link.Code, "boss")
	if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", f.gone.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disable = %d", rec.Code)
	}
	f.bossTok = f.asOperator(t, f.boss.ID)
	return f
}

func (f *disableJoinersFixture) path() string {
	return fmt.Sprintf("/v1/invites/%d/disable-joiners", f.link.Invite.ID)
}

// A dry run counts and changes nothing at all: the audit trail, the
// principals, their credentials and the invite are exactly as they were.
func TestDisableJoinersDryRunChangesNothing(t *testing.T) {
	for _, body := range []string{`{"dry_run":true}`, `{"dry_run":true,"close":true}`, `{"dry_run":true,"close":false}`} {
		t.Run(body, func(t *testing.T) {
			f := newDisableJoinersFixture(t)
			before := takeAdminSnapshot(t, f.inviteFixture)
			rec := f.send(t, "POST", f.path(), f.bossTok, body, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("dry run = %d %s", rec.Code, rec.Body)
			}
			got := decodeBody[schema.DisableJoinersResponseV1](t, rec)
			if !got.DryRun || got.Count != 2 || !slices.Equal(got.PrincipalIDs, []int64{f.wren.ID, f.piper.ID}) {
				t.Errorf("dry run = %+v, want wren and piper", got)
			}
			if after := takeAdminSnapshot(t, f.inviteFixture); !reflect.DeepEqual(before, after) {
				t.Errorf("a dry run changed something:\nbefore %+v\nafter  %+v", before, after)
			}
			for _, tok := range []string{f.wrenTok, f.piperTok} {
				if r := f.send(t, "GET", "/v1/whoami", tok, "", ""); r.Code != http.StatusOK {
					t.Errorf("whoami after a dry run = %d", r.Code)
				}
			}
		})
	}
}

// Without a dry run every enabled joiner is disabled through the single
// disable path, never an operator and never the caller, and the invite is
// closed unless the request says otherwise.
func TestDisableJoiners(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		caller     func(f *disableJoinersFixture) (tok string, id int64)
		wantClosed bool
	}{
		{"closes the link by default", `{"dry_run":false}`, func(f *disableJoinersFixture) (string, int64) { return f.rootTok, f.root.ID }, true},
		{"close asked for", `{"dry_run":false,"close":true}`, func(f *disableJoinersFixture) (string, int64) { return f.rootTok, f.root.ID }, true},
		{"link kept open", `{"dry_run":false,"close":false}`, func(f *disableJoinersFixture) (string, int64) { return f.rootTok, f.root.ID }, false},
		{"caller is a joiner", `{"dry_run":false}`, func(f *disableJoinersFixture) (string, int64) { return f.bossTok, f.boss.ID }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDisableJoinersFixture(t)
			tok, callerID := tt.caller(f)
			disabledBefore := len(auditRows(t, f.authFixture, "principal_disabled"))
			rec := f.send(t, "POST", f.path(), tok, tt.body, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("disable joiners = %d %s", rec.Code, rec.Body)
			}
			got := decodeBody[schema.DisableJoinersResponseV1](t, rec)
			if got.DryRun || got.Count != 2 || !slices.Equal(got.PrincipalIDs, []int64{f.wren.ID, f.piper.ID}) {
				t.Errorf("response = %+v, want wren and piper", got)
			}
			for _, jt := range []string{f.wrenTok, f.piperTok} {
				assertErrorBody(t, f.send(t, "GET", "/v1/whoami", jt, "", ""), http.StatusUnauthorized, "unauthenticated")
			}
			// The operator who joined, and so the caller, is untouched.
			if r := f.send(t, "GET", "/v1/whoami", f.bossTok, "", ""); r.Code != http.StatusOK {
				t.Errorf("the operator joiner's whoami = %d", r.Code)
			}

			// One principal_disabled each, attributed to the caller; one summary.
			var disabled []string
			for _, e := range auditRows(t, f.authFixture, "principal_disabled")[disabledBefore:] {
				disabled = append(disabled, e.Subject)
				if e.Actor != fmt.Sprintf("principal:%d", callerID) {
					t.Errorf("principal_disabled actor = %q", e.Actor)
				}
			}
			if want := []string{fmt.Sprintf("principal:%d", f.wren.ID), fmt.Sprintf("principal:%d", f.piper.ID)}; !slices.Equal(disabled, want) {
				t.Errorf("principal_disabled subjects = %v, want %v", disabled, want)
			}
			summary := auditRows(t, f.authFixture, store.AuditJoinersDisabled)
			wantDetail := fmt.Sprintf("invite=%d count=2 closed=%t", f.link.Invite.ID, tt.wantClosed)
			if len(summary) != 1 || summary[0].Subject != fmt.Sprintf("invite:%d", f.link.Invite.ID) || summary[0].Detail != wantDetail ||
				summary[0].Actor != fmt.Sprintf("principal:%d", callerID) {
				t.Errorf("joiners_disabled = %+v, want one row %q", summary, wantDetail)
			}

			list := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites?state=closed", f.rootTok, "", ""))
			if closed := slices.Contains(inviteIDs(list.Invites), f.link.Invite.ID); closed != tt.wantClosed {
				t.Errorf("invite closed = %v, want %v", closed, tt.wantClosed)
			}
			// root made the link: closing it is root's as its maker, anyone
			// else's as an operator.
			reason := "reason=operator"
			if callerID == f.root.ID {
				reason = "reason=creator"
			}
			closedRows := auditRows(t, f.authFixture, store.AuditInviteClosed)
			if tt.wantClosed && (len(closedRows) != 1 || !strings.HasSuffix(closedRows[0].Detail, reason)) {
				t.Errorf("invite_closed = %+v, want one with %s", closedRows, reason)
			}
			if tt.wantClosed {
				assertErrorBody(t, f.redeem(t, f.link.Code, "late"), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
			}

			// Again: nobody is left to disable, and it says so.
			again := decodeBody[schema.DisableJoinersResponseV1](t, f.send(t, "POST", f.path(), tok, tt.body, ""))
			if again.Count != 0 || again.PrincipalIDs == nil || len(again.PrincipalIDs) != 0 {
				t.Errorf("second call = %+v, want count 0 and []", again)
			}
		})
	}
}

func TestDisableJoinersRefusals(t *testing.T) {
	f := newDisableJoinersFixture(t)
	tests := []struct {
		name, path, body string
		want             int
		code             string
	}{
		{"unknown invite", "/v1/invites/9999/disable-joiners", `{"dry_run":false}`, http.StatusNotFound, schema.ErrorCodeInviteNotFound},
		{"unknown invite, dry run", "/v1/invites/9999/disable-joiners", `{"dry_run":true}`, http.StatusNotFound, schema.ErrorCodeInviteNotFound},
		{"bad id", "/v1/invites/x/disable-joiners", `{"dry_run":true}`, http.StatusBadRequest, "invalid_request"},
		{"no body", f.path(), ``, http.StatusBadRequest, "invalid_request"},
		{"unknown field", f.path(), `{"dry_run":true,"everyone":true}`, http.StatusBadRequest, "invalid_request"},
	}
	before := takeAdminSnapshot(t, f.inviteFixture)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorBody(t, f.send(t, "POST", tt.path, f.rootTok, tt.body, ""), tt.want, tt.code)
		})
	}
	if after := takeAdminSnapshot(t, f.inviteFixture); !reflect.DeepEqual(before, after) {
		t.Error("a refused request changed something")
	}
}

func TestBulkDisableTargets(t *testing.T) {
	now := time.Now()
	member := func(id int64) store.Principal { return store.Principal{ID: id, Role: store.RoleMember} }
	off := store.Principal{ID: 3, Role: store.RoleMember, DisabledAt: &now}
	op := store.Principal{ID: 4, Role: store.RoleOperator}
	tests := []struct {
		name   string
		caller store.Principal
		found  []store.Principal
		want   []int64
	}{
		{"nobody", store.Principal{ID: 1, Role: store.RoleOperator}, nil, []int64{}},
		{"enabled members", store.Principal{ID: 1, Role: store.RoleOperator}, []store.Principal{member(2), member(5)}, []int64{2, 5}},
		{"never a disabled one", store.Principal{ID: 1, Role: store.RoleOperator}, []store.Principal{member(2), off}, []int64{2}},
		{"never an operator", store.Principal{ID: 1, Role: store.RoleOperator}, []store.Principal{op, member(2)}, []int64{2}},
		{"never the caller", member(2), []store.Principal{member(2), member(5)}, []int64{5}},
		{"no caller", store.Principal{}, []store.Principal{member(2)}, []int64{2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bulkDisableTargets(tt.caller, tt.found)
			if got == nil || !slices.Equal(got, tt.want) {
				t.Errorf("targets = %v, want %v", got, tt.want)
			}
		})
	}
}

// inviteTree is root (the operator) -> ada (through an operator's link that
// grants the right to invite) -> ben (ada's personal invite) -> cy (ben's,
// once an operator grants ben the right), plus dot, whom ada also invited.
type inviteTree struct {
	*inviteFixture
	ada, ben, cy, dot             schema.PrincipalV0
	adaTok, benTok, cyTok, dotTok string
}

func newInviteTree(t *testing.T) *inviteTree {
	t.Helper()
	f := &inviteTree{inviteFixture: newInviteFixture(t, Config{})}
	grant := func(id int64) {
		t.Helper()
		if rec := f.send(t, "PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", id), f.rootTok, `{"may_invite":true}`, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("grant = %d %s", rec.Code, rec.Body)
		}
	}
	link := f.createInvite(t, f.rootTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"red"}, MayInvite: boolPtr(true)})
	f.ada, f.adaTok = f.joinThrough(t, link.Code, "ada")
	f.ben, f.benTok = f.joinThrough(t, f.createInvite(t, f.adaTok, personal("red")).Code, "ben")
	f.dot, f.dotTok = f.joinThrough(t, f.createInvite(t, f.adaTok, personal("red")).Code, "dot")
	grant(f.ben.ID)
	f.cy, f.cyTok = f.joinThrough(t, f.createInvite(t, f.benTok, personal("red")).Code, "cy")
	return f
}

func TestDisableInvitees(t *testing.T) {
	tests := []struct {
		name  string
		root  func(f *inviteTree) int64
		depth string
		want  func(f *inviteTree) []int64
	}{
		{"direct, the default", func(f *inviteTree) int64 { return f.ada.ID }, "", func(f *inviteTree) []int64 { return []int64{f.ben.ID, f.dot.ID} }},
		{"direct", func(f *inviteTree) int64 { return f.ada.ID }, `,"depth":"1"`, func(f *inviteTree) []int64 { return []int64{f.ben.ID, f.dot.ID} }},
		{"the whole tree", func(f *inviteTree) int64 { return f.ada.ID }, `,"depth":"all"`, func(f *inviteTree) []int64 { return []int64{f.ben.ID, f.dot.ID, f.cy.ID} }},
		{"a leaf", func(f *inviteTree) int64 { return f.cy.ID }, `,"depth":"all"`, func(*inviteTree) []int64 { return []int64{} }},
		{"under the operator", func(f *inviteTree) int64 { return f.root.ID }, `,"depth":"all"`, func(f *inviteTree) []int64 { return []int64{f.ada.ID, f.ben.ID, f.dot.ID, f.cy.ID} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteTree(t)
			root := tt.root(f)
			path := fmt.Sprintf("/v1/principals/%d/disable-invitees", root)
			want := tt.want(f)

			before := takeAdminSnapshot(t, f.inviteFixture)
			dry := decodeBody[schema.DisableJoinersResponseV1](t, f.send(t, "POST", path, f.rootTok, `{"dry_run":true`+tt.depth+`}`, ""))
			if !dry.DryRun || dry.Count != len(want) || !slices.Equal(dry.PrincipalIDs, want) {
				t.Errorf("dry run = %+v, want %v", dry, want)
			}
			if after := takeAdminSnapshot(t, f.inviteFixture); !reflect.DeepEqual(before, after) {
				t.Error("a dry run changed something")
			}

			rec := f.send(t, "POST", path, f.rootTok, `{"dry_run":false`+tt.depth+`}`, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("disable invitees = %d %s", rec.Code, rec.Body)
			}
			got := decodeBody[schema.DisableJoinersResponseV1](t, rec)
			if got.DryRun || !slices.Equal(got.PrincipalIDs, want) {
				t.Errorf("response = %+v, want %v", got, want)
			}
			tokens := map[int64]string{f.ada.ID: f.adaTok, f.ben.ID: f.benTok, f.cy.ID: f.cyTok, f.dot.ID: f.dotTok, f.root.ID: f.rootTok}
			for id, tok := range tokens {
				wantCode := http.StatusOK
				if slices.Contains(want, id) {
					wantCode = http.StatusUnauthorized
				}
				if r := f.send(t, "GET", "/v1/whoami", tok, "", ""); r.Code != wantCode {
					t.Errorf("principal %d whoami = %d, want %d", id, r.Code, wantCode)
				}
			}
			depth := "1"
			if strings.Contains(tt.depth, "all") {
				depth = "all"
			}
			summary := auditRows(t, f.authFixture, store.AuditInviteesDisabled)
			wantDetail := fmt.Sprintf("root=%d depth=%s count=%d", root, depth, len(want))
			if len(summary) != 1 || summary[0].Subject != fmt.Sprintf("principal:%d", root) || summary[0].Detail != wantDetail {
				t.Errorf("invitees_disabled = %+v, want %q", summary, wantDetail)
			}
			if n := len(auditRows(t, f.authFixture, "principal_disabled")); n != len(want) {
				t.Errorf("principal_disabled rows = %d, want %d", n, len(want))
			}
		})
	}
}

func TestDisableInviteesRefusals(t *testing.T) {
	f := newInviteTree(t)
	before := takeAdminSnapshot(t, f.inviteFixture)
	for _, c := range []struct {
		name, path, body string
		want             int
		code             string
	}{
		{"unknown principal", "/v1/principals/9999/disable-invitees", `{"dry_run":false}`, http.StatusNotFound, "principal_not_found"},
		{"unknown depth", fmt.Sprintf("/v1/principals/%d/disable-invitees", f.ada.ID), `{"dry_run":false,"depth":"2"}`, http.StatusBadRequest, "invalid_request"},
		{"a number depth", fmt.Sprintf("/v1/principals/%d/disable-invitees", f.ada.ID), `{"dry_run":false,"depth":1}`, http.StatusBadRequest, "invalid_request"},
		{"no body", fmt.Sprintf("/v1/principals/%d/disable-invitees", f.ada.ID), ``, http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertErrorBody(t, f.send(t, "POST", c.path, f.rootTok, c.body, ""), c.want, c.code)
		})
	}
	if after := takeAdminSnapshot(t, f.inviteFixture); !reflect.DeepEqual(before, after) {
		t.Error("a refused request changed something")
	}
	// Plain disable never follows the tree.
	if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", f.ada.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disable = %d", rec.Code)
	}
	for _, tok := range []string{f.benTok, f.cyTok, f.dotTok} {
		if r := f.send(t, "GET", "/v1/whoami", tok, "", ""); r.Code != http.StatusOK {
			t.Errorf("an invitee's whoami after disabling their inviter = %d", r.Code)
		}
	}
}

func TestInvitePermissionRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	path := fmt.Sprintf("/v1/principals/%d/invite-permission", f.alice.ID)
	may := func() bool {
		t.Helper()
		return decodeBody[schema.WhoAmIResponseV1](t, f.send(t, "GET", "/v1/whoami", f.aliceTok, "", "")).MayInvite
	}

	// A member cannot grant it, to themselves or anyone: 403, audited, unchanged.
	assertErrorBody(t, f.send(t, "PUT", path, f.aliceTok, `{"may_invite":true}`, ""), http.StatusForbidden, "forbidden")
	if may() || countAudit(t, f.authFixture, store.AuditInvitePermissionChanged) != 0 {
		t.Fatal("a member granted themselves the right to invite")
	}
	if rows := auditRows(t, f.authFixture, "access_denied"); len(rows) != 1 || rows[0].Subject != "PUT /v1/principals/{id}/invite-permission" {
		t.Errorf("access_denied = %+v", rows)
	}

	if rec := f.send(t, "PUT", path, f.rootTok, `{"may_invite":true}`, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body)
	}
	if !may() {
		t.Fatal("whoami does not show the right after a grant")
	}
	kept := f.createInvite(t, f.aliceTok, personal("red"))
	alsoKept := f.createInvite(t, f.aliceTok, personal("red"))
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", alsoKept.Invite.ID), f.aliceTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d", rec.Code)
	}
	rootsLink := f.createInvite(t, f.rootTok, openLink("red"))

	// Taking it away closes alice's open personal invites, and only those.
	if rec := f.send(t, "PUT", path, f.rootTok, `{"may_invite":false}`, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if may() {
		t.Fatal("whoami still shows the right after it was taken away")
	}
	open := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites?state=open", f.rootTok, "", ""))
	if !slices.Equal(inviteIDs(open.Invites), []int64{rootsLink.Invite.ID}) {
		t.Errorf("open invites = %v, want only the operator's link", inviteIDs(open.Invites))
	}
	var reasons []string
	for _, e := range auditRows(t, f.authFixture, store.AuditInviteClosed) {
		reasons = append(reasons, e.Subject+" "+e.Detail[strings.LastIndex(e.Detail, "reason="):])
	}
	want := []string{
		fmt.Sprintf("invite:%d reason=creator", alsoKept.Invite.ID),
		fmt.Sprintf("invite:%d reason=permission_revoked", kept.Invite.ID),
	}
	if !slices.Equal(reasons, want) {
		t.Errorf("invite_closed = %v, want %v", reasons, want)
	}
	// Giving it back does not revive them.
	if rec := f.send(t, "PUT", path, f.rootTok, `{"may_invite":true}`, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("grant = %d", rec.Code)
	}
	assertErrorBody(t, f.redeem(t, kept.Code, "late"), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
	if n := countAudit(t, f.authFixture, store.AuditInvitePermissionChanged); n != 3 {
		t.Errorf("invite_permission_changed rows = %d, want 3", n)
	}

	assertErrorBody(t, f.send(t, "PUT", "/v1/principals/9999/invite-permission", f.rootTok, `{"may_invite":true}`, ""), http.StatusNotFound, "principal_not_found")
	assertErrorBody(t, f.send(t, "PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", f.bot.ID), f.rootTok, `{"may_invite":true}`, ""), http.StatusBadRequest, "invalid_request")
	assertErrorBody(t, f.send(t, "PUT", path, f.rootTok, `{"may_invite":"yes"}`, ""), http.StatusBadRequest, "invalid_request")
}

// Disabling a person closes every invite they made, open links included, as
// creator_disabled; enabling them does not revive any.
func TestDisableClosesTheirInvites(t *testing.T) {
	f := newInviteFixture(t, Config{})
	ctx := context.Background()
	other, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "deputy")
	if err != nil {
		t.Fatal(err)
	}
	deputyTok := f.asOperator(t, other.ID)
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.red.ID, other.ID, 0); err != nil {
		t.Fatal(err)
	}
	link := f.createInvite(t, deputyTok, openLink("red"))
	pers := f.createInvite(t, deputyTok, personal("red"))
	rootsLink := f.createInvite(t, f.rootTok, openLink("red"))

	if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", other.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body)
	}
	if rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/enable", other.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("enable = %d", rec.Code)
	}
	closed := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites?state=closed", f.rootTok, "", ""))
	if !slices.Equal(inviteIDs(closed.Invites), []int64{pers.Invite.ID, link.Invite.ID}) {
		t.Errorf("closed invites = %v, want both of the disabled operator's", inviteIDs(closed.Invites))
	}
	for _, e := range auditRows(t, f.authFixture, store.AuditInviteClosed) {
		if !strings.HasSuffix(e.Detail, "reason=creator_disabled") || e.Actor != fmt.Sprintf("principal:%d", f.root.ID) {
			t.Errorf("invite_closed = %+v, want reason=creator_disabled by root", e)
		}
	}
	if rows := auditRows(t, f.authFixture, store.AuditInviteClosed); len(rows) != 2 {
		t.Errorf("invite_closed rows = %d, want 2", len(rows))
	}
	assertErrorBody(t, f.redeem(t, link.Code, "late"), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
	if _, tok := f.joinThrough(t, rootsLink.Code, "fine"); tok == "" {
		t.Error("another operator's link stopped working")
	}
}

func TestInviteSettingsRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	link := f.createInvite(t, f.rootTok, openLink("red"))
	read := func() schema.InviteSettingsV1 {
		t.Helper()
		rec := f.send(t, "GET", "/v1/invites/settings", f.rootTok, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("read settings = %d %s", rec.Code, rec.Body)
		}
		return decodeBody[schema.InviteSettingsV1](t, rec)
	}
	put := func(body string) schema.InviteSettingsV1 {
		t.Helper()
		rec := f.send(t, "PUT", "/v1/invites/settings", f.rootTok, body, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("put settings = %d %s", rec.Code, rec.Body)
		}
		return decodeBody[schema.InviteSettingsV1](t, rec)
	}
	if read().Paused {
		t.Fatal("paused at start")
	}
	for range 2 { // the second changes and audits nothing
		if !put(`{"paused":true}`).Paused || !read().Paused {
			t.Fatal("not paused after pausing")
		}
	}
	assertErrorBody(t, f.send(t, "POST", "/v1/invites", f.rootTok, inviteJSON(t, openLink("red")), ""), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)
	assertErrorBody(t, f.preview(t, link.Code), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)
	assertErrorBody(t, f.redeem(t, link.Code, "late"), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)

	if put(`{"paused":false}`).Paused || read().Paused {
		t.Fatal("still paused after resuming")
	}
	f.joinThrough(t, link.Code, "afterwards")
	paused, resumed := auditRows(t, f.authFixture, store.AuditInvitesPaused), auditRows(t, f.authFixture, store.AuditInvitesResumed)
	if len(paused) != 1 || len(resumed) != 1 || paused[0].Actor != fmt.Sprintf("principal:%d", f.root.ID) {
		t.Errorf("invites_paused = %+v, invites_resumed = %+v; want one each by root", paused, resumed)
	}
	for _, body := range []string{``, `{"paused":"yes"}`, `{"paused":true,"forever":true}`} {
		assertErrorBody(t, f.send(t, "PUT", "/v1/invites/settings", f.rootTok, body, ""), http.StatusBadRequest, "invalid_request")
	}
}

// The pause switch is stored: a server started again on the same database is
// still paused, and refuses a redemption until an operator resumes.
func TestInvitesPauseSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	start := func() (*Server, *store.Store) {
		t.Helper()
		st, err := store.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		srv := New(Config{AuthMode: AuthRequired, DataDir: t.TempDir(), Listen: "127.0.0.1:0"}, st)
		srv.setInviteLimits(looseInviteLimits())
		return srv, st
	}
	call := func(srv *Server, method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	srv, st := start()
	_, _, rootTok, err := st.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateChannel(ctx, "red"); err != nil {
		t.Fatal(err)
	}
	rec := call(srv, "POST", "/v1/invites", rootTok, inviteJSON(t, openLink("red")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	code := decodeBody[schema.CreateInviteResponseV1](t, rec).Code
	if rec := call(srv, "PUT", "/v1/invites/settings", rootTok, `{"paused":true}`); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d %s", rec.Code, rec.Body)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	srv, st = start()
	t.Cleanup(func() { _ = st.Close() })
	if got := decodeBody[schema.InviteSettingsV1](t, call(srv, "GET", "/v1/invites/settings", rootTok, "")); !got.Paused {
		t.Fatal("not paused after a restart")
	}
	redeem := inviteJSON(t, schema.RedeemInviteRequestV1{Code: code, Name: "newcomer"})
	assertErrorBody(t, call(srv, "POST", "/v1/invites/redeem", "", redeem), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)
	if rec := call(srv, "PUT", "/v1/invites/settings", rootTok, `{"paused":false}`); rec.Code != http.StatusOK {
		t.Fatalf("resume = %d", rec.Code)
	}
	if rec := call(srv, "POST", "/v1/invites/redeem", "", redeem); rec.Code != http.StatusCreated {
		t.Fatalf("redeem after resuming = %d %s", rec.Code, rec.Body)
	}
}

// Disabling joiners ends everything they hold, as the single disable route
// does: an open /v2/ws socket and an events socket close at once, and the
// voice path runs before the response, rotating the room a joiner held a
// session for. The LiveKit fake is the one the disable route's own voice
// tests use.
func TestDisableJoinersClosesSocketsAndVoice(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.srv.setInviteLimits(looseInviteLimits())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.ops.ID, f.root.ID, 0); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "POST", "/v1/invites", f.rootTok, inviteJSON(t, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"ops"}, Voice: true}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	link := decodeBody[schema.CreateInviteResponseV1](t, rec)
	rec = f.do(t, "POST", "/v1/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV1{Code: link.Code, Name: "wren"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	joined := decodeBody[schema.RedeemInviteResponseV1](t, rec)
	f.tokens["wren"], f.ids["wren"] = joined.Token, joined.Principal.ID

	// wren holds a voice session in ops and is in its room.
	room := f.inUse(t, f.ops)
	f.holder(t, "wren", f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("wren"), joinedMs: 1_000, published: true},
		fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann2,wren*" {
		t.Fatalf("before: %q", got)
	}

	// And holds both sockets, each known to be subscribed.
	base := wsTestServer(t, f.srv)
	v2 := startReader(ctx, dialAs(t, ctx, base, "/v2/ws?channel=ops", joined.Token))
	events := dialEvents(t, base, "/v1/events/ws?channel=ops", joined.Token)
	events.next()
	v2.waitForID(t, f.postAs(t, f.rootTok, "ops", "warmup"))

	base0 := len(f.lk.calls())
	req := httptest.NewRequest("POST", fmt.Sprintf("/v1/invites/%d/disable-joiners", link.Invite.ID), strings.NewReader(`{"dry_run":false}`))
	req.Header.Set("Authorization", "Bearer "+f.rootTok)
	out := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(&orderWriter{ResponseWriter: out, lk: f.lk}, req)
	if out.Code != http.StatusOK {
		t.Fatalf("disable joiners = %d %s", out.Code, out.Body)
	}
	if got := decodeBody[schema.DisableJoinersResponseV1](t, out); !slices.Equal(got.PrincipalIDs, []int64{joined.Principal.ID}) {
		t.Fatalf("response = %+v", got)
	}

	closeErrContains(t, v2.waitClosed(t), websocket.StatusPolicyViolation, reasonRevoked)
	if ce := events.closed(); ce.Code != websocket.StatusPolicyViolation || ce.Reason != reasonCredentialInvalid {
		t.Errorf("events socket closed with %d %q, want 1008 %q", ce.Code, ce.Reason, reasonCredentialInvalid)
	}

	// The room wren held a session for was rotated before the answer.
	var deleted []string
	respAt := 0
	for _, c := range f.lk.calls()[base0:] {
		switch c.method {
		case "DeleteRoom":
			if respAt != 0 {
				t.Errorf("DeleteRoom came after the response")
			}
			deleted = append(deleted, c.room)
		case "http-response":
			respAt = c.seq
		}
	}
	if !slices.Equal(deleted, []string{room}) {
		t.Errorf("DeleteRoom = %v, want the ops room", deleted)
	}
	rotated := f.audits(t, store.AuditVoiceRoomRotated)
	if len(rotated) != 1 || rotated[0].Detail != "reason="+store.VoiceRotatePrincipalDisabled {
		t.Errorf("voice_room_rotated = %+v", rotated)
	}
	if got := f.who(t, f.ops); got != "" {
		t.Errorf("presence right after = %q, want nobody", got)
	}
	assertErrorBody(t, f.do(t, "GET", "/v1/whoami", joined.Token, ""), http.StatusUnauthorized, "unauthenticated")
}
