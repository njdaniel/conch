package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Member links over REST (issue #985, design §2.1 and §3.3): POST, GET
// /v2/invites, POST /v2/invites/preview and GET /v2/invites/{id}/joiners.

// createMemberLink makes a member link over REST and fails the test unless it
// is made.
func (f *inviteFixture) createMemberLink(t *testing.T, token string, req schema.CreateInviteRequestV2) schema.CreateInviteResponseV2 {
	t.Helper()
	rec := f.send(t, "POST", "/v2/invites", token, inviteJSON(t, req), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create member link = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	return decodeBody[schema.CreateInviteResponseV2](t, rec)
}

func (f *inviteFixture) previewV2(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v2/invites/preview", "", inviteJSON(t, schema.InvitePreviewRequestV1{Code: code}), "")
}

// linkCallers are the people and others the v2 routes are tried as. Every
// token is real; the guests are seeded through the store (no route makes one
// now), and the ended guest's token is replayed after its visit ended.
type linkCallers struct {
	member, quiet, op, op2, agent, guest, endedGuest string
	memberID, op2ID                                  int64
}

func (f *inviteFixture) linkCallers(t *testing.T) linkCallers {
	t.Helper()
	ctx := context.Background()
	st := f.srv.store
	mk := func(name string, role store.Role, may bool) (store.Principal, string) {
		t.Helper()
		p, err := st.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		if role != store.RoleMember {
			if _, err := st.SetRole(ctx, "system", f.root.ID, p.ID, role, nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.SetMayInvite(ctx, "system", p.ID, may); err != nil {
			t.Fatal(err)
		}
		_, tok, err := st.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		return p, tok
	}
	var c linkCallers
	var member store.Principal
	member, c.member = mk("maker", store.RoleMember, true)
	c.memberID = member.ID
	_, c.quiet = mk("quiet", store.RoleMember, false)
	_, c.op = f.root, f.rootTok
	var op2 store.Principal
	op2, c.op2 = mk("second", store.RoleOperator, false)
	c.op2ID = op2.ID
	c.agent = f.botTok
	g := f.seedGuest(t, "Pat")
	c.guest = g.Token
	eg := f.seedGuest(t, "Lee")
	c.endedGuest = eg.Token
	if _, err := st.EndGuestVisit(ctx, "system", eg.Principal.ID, schema.GuestEndOperator); err != nil {
		t.Fatal(err)
	}
	return c
}

// linkError is an error body with the permission a refusal names.
type linkError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Permission string `json:"permission"`
}

// POST /v2/invites for each kind of caller: a member whose brake is on, a
// member whose brake is off, an operator, a non-owner operator, a guest, an
// ended guest with a replayed token, an agent and nobody.
func TestCreateMemberLinkRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	c := f.linkCallers(t)
	tests := []struct {
		name     string
		token    string
		want     int
		wantCode string
		wantPerm string
	}{
		{"member, brake on", c.member, 201, "", ""},
		{"member, brake off", c.quiet, 403, schema.ErrorCodeInviteNotAllowed, string(schema.PermMakeMemberLinks)},
		{"operator", c.op, 201, "", ""},
		{"non-owner operator, brake off", c.op2, 201, "", ""},
		{"guest", c.guest, 403, schema.ErrorCodeGuestNotAllowed, ""},
		{"ended guest, replayed token", c.endedGuest, 401, "unauthenticated", ""},
		{"agent", c.agent, 403, schema.ErrorCodeHumanRequired, ""},
		{"nobody", "", 401, "unauthenticated", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.send(t, "POST", "/v2/invites", tt.token, `{"kind":"member"}`, "")
			if tt.want == http.StatusCreated {
				if rec.Code != http.StatusCreated {
					t.Fatalf("status = %d %s", rec.Code, rec.Body)
				}
				resp := decodeBody[schema.CreateInviteResponseV2](t, rec)
				if schema.ValidateInviteCode(resp.Code) != nil || resp.Invite.Kind != schema.InviteKindMember || resp.Invite.Legacy {
					t.Errorf("response = %+v", resp)
				}
				return
			}
			assertErrorBody(t, rec, tt.want, tt.wantCode)
			if got := decodeBody[linkError](t, rec).Permission; got != tt.wantPerm {
				t.Errorf("permission = %q, want %q", got, tt.wantPerm)
			}
			if strings.Contains(rec.Body.String(), schema.InviteCodePrefix) {
				t.Errorf("a refusal carries a code: %s", rec.Body)
			}
		})
	}
	// Only the three that were answered 201 made a link.
	if n := len(f.auditOf(t, store.AuditInviteCreated)); n != 3 {
		t.Errorf("%d invite_created rows, want 3", n)
	}
}

// What a request may say.
func TestCreateMemberLinkBodies(t *testing.T) {
	week := time.Duration(schema.DefaultMemberLinkExpirySeconds) * time.Second
	future := func(f *inviteFixture, d time.Duration) string {
		return f.clock.Add(d).UTC().Format(time.RFC3339Nano)
	}
	tests := []struct {
		name     string
		who      string // member, op
		body     func(f *inviteFixture) string
		setup    func(*inviteFixture)
		want     int
		wantCode string
		// expires: how far ahead the link expires; 0 is never.
		expires time.Duration
	}{
		{"defaults", "member", func(*inviteFixture) string { return `{"kind":"member"}` }, nil, 201, "", week},
		{"an expiry named", "member", func(f *inviteFixture) string {
			return fmt.Sprintf(`{"kind":"member","expires_at":%q}`, future(f, 2*time.Hour))
		}, nil, 201, "", 2 * time.Hour},
		{"an expiry past the bound", "op", func(f *inviteFixture) string {
			return fmt.Sprintf(`{"kind":"member","expires_at":%q}`, future(f, time.Duration(schema.MaxMemberLinkExpirySeconds+3600)*time.Second))
		}, nil, 400, "invalid_request", 0},
		{"an expiry at the sentinel", "op", func(f *inviteFixture) string {
			return `{"kind":"member","expires_at":"9999-12-31T23:59:59Z"}`
		}, nil, 400, "invalid_request", 0},
		{"an expiry in the past", "member", func(f *inviteFixture) string {
			return fmt.Sprintf(`{"kind":"member","expires_at":%q}`, future(f, -time.Hour))
		}, nil, 400, "invalid_request", 0},
		{"never_expires, a member", "member", func(*inviteFixture) string { return `{"kind":"member","never_expires":true}` }, nil, 403, schema.ErrorCodeInviteNotAllowed, 0},
		{"never_expires, an operator", "op", func(*inviteFixture) string { return `{"kind":"member","never_expires":true}` }, nil, 201, "", 0},
		{"never_expires with expires_at", "op", func(f *inviteFixture) string {
			return fmt.Sprintf(`{"kind":"member","never_expires":true,"expires_at":%q}`, future(f, time.Hour))
		}, nil, 400, "invalid_request", 0},
		{"the default expiry setting is none", "member", func(*inviteFixture) string { return `{"kind":"member"}` }, func(f *inviteFixture) {
			legacyExec(t, f.srv, `INSERT INTO server_settings (key, value, updated_at) VALUES ('member_link_default_expiry_seconds', '0', 0)`)
		}, 201, "", 0},
		{"max_uses", "member", func(*inviteFixture) string { return `{"kind":"member","max_uses":5}` }, nil, 201, "", week},
		{"max_uses 0", "member", func(*inviteFixture) string { return `{"kind":"member","max_uses":0}` }, nil, 400, "invalid_request", 0},
		{"kind guest, an operator", "op", func(*inviteFixture) string { return `{"kind":"guest"}` }, nil, 400, "invalid_request", 0},
		{"kind guest, a member", "member", func(*inviteFixture) string { return `{"kind":"guest"}` }, nil, 400, "invalid_request", 0},
		{"kind personal", "member", func(*inviteFixture) string { return `{"kind":"personal"}` }, nil, 409, schema.ErrorCodeInviteKindRetired, 0},
		{"kind open, an operator", "op", func(*inviteFixture) string { return `{"kind":"open"}` }, nil, 409, schema.ErrorCodeInviteKindRetired, 0},
		{"a v1 body", "op", func(*inviteFixture) string { return inviteJSON(t, personal("general")) }, nil, 409, schema.ErrorCodeInviteKindRetired, 0},
		{"no kind", "member", func(*inviteFixture) string { return `{}` }, nil, 400, "invalid_request", 0},
		{"a channels field", "op", func(*inviteFixture) string { return `{"kind":"member","channels":["general"]}` }, nil, 400, "invalid_request", 0},
		{"a voice field", "op", func(*inviteFixture) string { return `{"kind":"member","voice":true}` }, nil, 400, "invalid_request", 0},
		{"a may_invite field", "op", func(*inviteFixture) string { return `{"kind":"member","may_invite":true}` }, nil, 400, "invalid_request", 0},
		{"not json", "op", func(*inviteFixture) string { return `nope` }, nil, 400, "invalid_request", 0},
		{"two objects", "op", func(*inviteFixture) string { return `{"kind":"member"}{}` }, nil, 400, "invalid_request", 0},
		{"too large", "op", func(*inviteFixture) string {
			return `{"kind":"member","landing_channels":["` + strings.Repeat("x", inviteBodyMaxBytes) + `"]}`
		}, nil, 413, "request_too_large", 0},
		{"paused", "op", func(*inviteFixture) string { return `{"kind":"member"}` }, func(f *inviteFixture) {
			if _, err := f.srv.store.SetInvitesPaused(context.Background(), "system", true); err != nil {
				t.Fatal(err)
			}
		}, 503, schema.ErrorCodeInvitesPaused, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t, Config{})
			c := f.linkCallers(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			tok := map[string]string{"member": c.member, "op": c.op}[tt.who]
			before := len(f.auditOf(t, store.AuditInviteCreated))
			rec := f.send(t, "POST", "/v2/invites", tok, tt.body(f), "")
			if tt.want != http.StatusCreated {
				assertErrorBody(t, rec, tt.want, tt.wantCode)
				if n := len(f.auditOf(t, store.AuditInviteCreated)); n != before {
					t.Errorf("invite_created rows %d -> %d after a refusal", before, n)
				}
				legacyCount(t, f, "SELECT COUNT(*) FROM invites WHERE link = 'guest'", 0)
				return
			}
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			resp := decodeBody[schema.CreateInviteResponseV2](t, rec)
			if tt.expires == 0 {
				if resp.Invite.ExpiresAt != nil || strings.Contains(rec.Body.String(), `"expires_at"`) {
					t.Errorf("a link that never expires says expires_at: %s", rec.Body)
				}
				v1 := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.rootTok, "", ""))
				for _, inv := range v1.Invites {
					if inv.ID == resp.Invite.ID && inv.ExpiresAt.Time().UnixMilli() != schema.NoExpiryMillis {
						t.Errorf("v1 shows expires_at %v, want the sentinel", inv.ExpiresAt.Time())
					}
				}
			} else if resp.Invite.ExpiresAt == nil || (resp.Invite.ExpiresAt.Time().Sub(f.clock)-tt.expires).Abs() > 2*time.Millisecond {
				t.Errorf("expires_at = %v, want %v ahead of %v", resp.Invite.ExpiresAt, tt.expires, f.clock)
			}
		})
	}
}

// legacyCount fails unless the query counts want rows.
func legacyCount(t *testing.T, f *inviteFixture, query string, want int) {
	t.Helper()
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Errorf("%s = %d, want %d", query, n, want)
	}
}

// Landing channels over REST: an operator names any channel; everyone else is
// refused with one answer, whatever the name is, and a name that is no
// channel is not found for an operator only.
func TestCreateMemberLinkLandingRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	c := f.linkCallers(t)
	body := func(names ...string) string {
		return inviteJSON(t, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember, LandingChannels: names})
	}
	ok := f.send(t, "POST", "/v2/invites", c.op, body("blue", "red"), "")
	if ok.Code != http.StatusCreated {
		t.Fatalf("operator = %d %s", ok.Code, ok.Body)
	}
	if got := decodeBody[schema.CreateInviteResponseV2](t, ok).Invite.LandingChannels; len(got) != 2 {
		t.Errorf("landing channels = %+v", got)
	}
	assertErrorBody(t, f.send(t, "POST", "/v2/invites", c.op, body("nosuch"), ""), http.StatusNotFound, "channel_not_found")

	// A member: private, public in or out of, and nonexistent are one answer.
	var bodies []string
	for _, name := range []string{"blue", "red", "general", "nosuch"} {
		rec := f.send(t, "POST", "/v2/invites", c.member, body(name), "")
		assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeInviteGrantExceedsInviter)
		bodies = append(bodies, rec.Body.String())
		// manage_channels is fixed off for a member: no cell can lift the
		// refusal, so it is not named (design §7.4).
		if got := decodeBody[linkError](t, rec).Permission; got != "" {
			t.Errorf("permission = %q, want none for a member", got)
		}
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("answers differ by channel:\n%s\n%s", bodies[0], b)
		}
	}
	if strings.Contains(strings.Join(bodies, ""), "blue") || strings.Contains(strings.Join(bodies, ""), "nosuch") {
		t.Errorf("a refusal echoes a channel name: %v", bodies)
	}
	// A restricted operator is a member in this respect.
	withholdOperator(t, f, schema.PermManageChannels)
	for _, name := range []string{"blue", "nosuch"} {
		rec := f.send(t, "POST", "/v2/invites", c.op2, body(name), "")
		assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeInviteGrantExceedsInviter)
		// An operator's cell can lift it, so the refusal names the permission.
		if got := decodeBody[linkError](t, rec).Permission; got != string(schema.PermManageChannels) {
			t.Errorf("permission = %q, want manage_channels for a restricted operator", got)
		}
	}
}

// withholdOperator switches perms off in the operator column, as the owner does.
func withholdOperator(t *testing.T, f *inviteFixture, perms ...schema.Permission) {
	t.Helper()
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision}
	for _, p := range perms {
		req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleOperator, Granted: false})
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, req, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// The allowance, over REST: counted on member links, an uncapped link counting
// as open; a refused create is audited as the others are.
func TestMemberLinkAllowanceRoute(t *testing.T) {
	f := newInviteFixture(t, Config{InviteAllowance: 2})
	c := f.linkCallers(t)
	first := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember}) // uncapped
	one := int64(1)
	f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember, MaxUses: &one})
	rec := f.send(t, "POST", "/v2/invites", c.member, `{"kind":"member"}`, "")
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeInviteAllowanceExceeded)
	// Operators have none; another member has their own.
	for range 3 {
		f.createMemberLink(t, c.op, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	}
	// Closing one makes room.
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", first.Invite.ID), c.member, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d %s", rec.Code, rec.Body)
	}
	f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
}

// DELETE /v1/invites/{id} closes a v2 link for its maker and an operator, and
// is a 404 for another member.
func TestCloseMemberLink(t *testing.T) {
	f := newInviteFixture(t, Config{})
	c := f.linkCallers(t)
	mk := func() int64 {
		return f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember}).Invite.ID
	}
	id := mk()
	assertErrorBody(t, f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", id), c.quiet, "", ""), http.StatusNotFound, schema.ErrorCodeInviteNotFound)
	for _, tok := range []string{c.member, c.op} {
		id := mk()
		if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", id), tok, "", ""); rec.Code != http.StatusNoContent {
			t.Errorf("close = %d %s", rec.Code, rec.Body)
		}
	}
}

// The brake over REST: PUT invite-permission off closes the person's open
// member links, audited; on reopens none; an operator's links stand.
func TestBrakeRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	c := f.linkCallers(t)
	mine := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	ops := f.createMemberLink(t, c.op2, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	put := func(id int64, may bool) {
		t.Helper()
		rec := f.send(t, "PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", id), f.rootTok, fmt.Sprintf(`{"may_invite":%t}`, may), "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("invite-permission = %d %s", rec.Code, rec.Body)
		}
	}
	put(c.memberID, false)
	rec := f.send(t, "POST", "/v2/invites", c.member, `{"kind":"member"}`, "")
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeInviteNotAllowed)
	list := decodeBody[schema.ListInvitesResponseV2](t, f.send(t, "GET", "/v2/invites", c.member, "", ""))
	if len(list.Invites) != 1 || list.Invites[0].ID != mine.Invite.ID || list.Invites[0].ClosedAt == nil {
		t.Fatalf("after the brake: %+v, want the one link, closed", list.Invites)
	}
	assertErrorBody(t, f.previewV2(t, mine.Code), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
	var reasons []string
	for _, e := range f.auditOf(t, store.AuditInviteClosed) {
		reasons = append(reasons, e.Subject+" "+e.Detail[strings.LastIndex(e.Detail, "reason="):])
	}
	if want := []string{fmt.Sprintf("invite:%d reason=permission_revoked", mine.Invite.ID)}; !slices.Equal(reasons, want) {
		t.Errorf("invite_closed rows = %q, want %q", reasons, want)
	}
	put(c.memberID, true)
	if rec := f.previewV2(t, mine.Code); rec.Code != http.StatusNotFound {
		t.Errorf("preview after the brake came back = %d, want the link still closed", rec.Code)
	}
	f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	// The operator's brake off closes nothing.
	put(c.op2ID, true)
	put(c.op2ID, false)
	if rec := f.previewV2(t, ops.Code); rec.Code != http.StatusOK {
		t.Errorf("the operator's link with the brake off = %d %s", rec.Code, rec.Body)
	}
}

// GET /v2/invites: the caller's own links; an operator's every link; no
// legacy guest-admitting invite, which GET /v1/invites still lists, closed.
func TestListMemberLinksRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	c := f.linkCallers(t)
	mine := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	n := int64(4)
	capped := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember, MaxUses: &n})
	ops := f.createMemberLink(t, c.op, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	legacy := f.createInvite(t, f.rootTok, openLink("general"))
	guest := f.createInvite(t, f.rootTok, openLink("general"))
	legacyExec(t, f.srv, "UPDATE invites SET join_as = 'both', closed_at = 1 WHERE id = ?", guest.Invite.ID)

	ids := func(tok string) []int64 {
		t.Helper()
		rec := f.send(t, "GET", "/v2/invites", tok, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body)
		}
		var out []int64
		for _, inv := range decodeBody[schema.ListInvitesResponseV2](t, rec).Invites {
			out = append(out, inv.ID)
			if strings.Contains(rec.Body.String(), schema.InviteCodePrefix) {
				t.Errorf("the list carries a code")
			}
		}
		return out
	}
	if got, want := ids(c.member), []int64{capped.Invite.ID, mine.Invite.ID}; !slices.Equal(got, want) {
		t.Errorf("member's list = %v, want %v", got, want)
	}
	if got, want := ids(c.quiet), []int64(nil); !slices.Equal(got, want) {
		t.Errorf("another member's list = %v, want none", got)
	}
	if got, want := ids(c.op), []int64{ops.Invite.ID, capped.Invite.ID, mine.Invite.ID, legacy.Invite.ID}; !slices.Equal(got, want) {
		// Newest first; the legacy invite was made after the three links.
		want = []int64{legacy.Invite.ID, ops.Invite.ID, capped.Invite.ID, mine.Invite.ID}
		if !slices.Equal(got, want) {
			t.Errorf("operator's list = %v, want %v and no guest-admitting invite %d", got, want, guest.Invite.ID)
		}
	}
	empty := f.send(t, "GET", "/v2/invites", c.quiet, "", "")
	if !strings.Contains(empty.Body.String(), `"invites":[]`) {
		t.Errorf("an empty list = %s, want a JSON array", empty.Body)
	}
	// v1 still shows the closed guest-admitting invite.
	v1 := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.rootTok, "", ""))
	found := false
	for _, inv := range v1.Invites {
		if inv.ID == guest.Invite.ID {
			found = inv.ClosedAt != nil
		}
	}
	if !found {
		t.Errorf("v1 does not list the closed guest-admitting invite")
	}
	for name, tok := range map[string]string{"guest": c.guest, "agent": c.agent, "nobody": ""} {
		if rec := f.send(t, "GET", "/v2/invites", tok, "", ""); rec.Code < 400 {
			t.Errorf("%s: list = %d %s", name, rec.Code, rec.Body)
		}
	}
	// The wire says what it should of the shapes.
	for _, inv := range decodeBody[schema.ListInvitesResponseV2](t, f.send(t, "GET", "/v2/invites", c.op, "", "")).Invites {
		switch inv.ID {
		case legacy.Invite.ID:
			if !inv.Legacy || len(inv.Channels) != 1 {
				t.Errorf("legacy = %+v", inv)
			}
		case capped.Invite.ID:
			if inv.Legacy || inv.MaxUses == nil || *inv.MaxUses != 4 || inv.ExpiresAt == nil {
				t.Errorf("capped = %+v", inv)
			}
		}
	}
}

// The v2 preview answers what §3.3 lists and nothing else, for a member link
// and a legacy invite, and one error for every code that is no good.
func TestPreviewMemberLinkRoute(t *testing.T) {
	f := newInviteFixture(t, Config{ServerName: "harbour"})
	c := f.linkCallers(t)
	mustExec := func(q string, args ...any) { legacyExec(t, f.srv, q, args...) }
	mustExec("UPDATE channels SET visibility = 'public', is_default = 1 WHERE name = 'red'")
	link := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})

	rec := f.previewV2(t, link.Code)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview = %d %s (Cache-Control %q)", rec.Code, rec.Body, rec.Header().Get("Cache-Control"))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"channels", "expires_at", "inviter", "join", "kind", "server_name"}; !slices.Equal(keys, want) {
		t.Errorf("preview fields = %v, want %v", keys, want)
	}
	got := decodeBody[schema.InvitePreviewV2](t, rec)
	if got.Kind != schema.InviteKindMember || got.ServerName != "harbour" || got.Inviter != "maker" ||
		!slices.Equal(got.Channels, []string{"red"}) || got.ExpiresAt == nil || !got.Join.Account || got.Join.Guest {
		t.Errorf("preview = %+v", got)
	}
	if strings.Contains(rec.Body.String(), "blue") {
		t.Errorf("the preview names a private channel: %s", rec.Body)
	}
	// A legacy invite is shown as a member link, with the channels it grants.
	legacy := f.createInvite(t, f.rootTok, openLink("blue"))
	lp := decodeBody[schema.InvitePreviewV2](t, f.previewV2(t, legacy.Code))
	if lp.Kind != schema.InviteKindMember || !slices.Equal(lp.Channels, []string{"blue"}) {
		t.Errorf("legacy preview = %+v", lp)
	}
	// A landing channel's name is shown to the holder of the code (by design).
	land := f.createMemberLink(t, c.op, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember, LandingChannels: []string{"blue"}})
	if lp := decodeBody[schema.InvitePreviewV2](t, f.previewV2(t, land.Code)); !slices.Equal(lp.Channels, []string{"blue"}) {
		t.Errorf("landing preview = %+v", lp)
	}
	// No expiry: the field is absent.
	forever := f.createMemberLink(t, c.op, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember, NeverExpires: true})
	if body := f.previewV2(t, forever.Code).Body.String(); strings.Contains(body, "expires_at") {
		t.Errorf("preview of a link that never expires has expires_at: %s", body)
	}

	// Every code that is no good is the same bytes.
	closed := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", closed.Invite.ID), c.member, "", "")
	guestOnly := f.createInvite(t, f.rootTok, openLink("red"))
	mustExec("UPDATE invites SET join_as = 'guest' WHERE id = ?", guestOnly.Invite.ID)
	var first string
	for name, code := range map[string]string{"unknown": fakeInviteCode, "closed": closed.Code, "guest-admitting": guestOnly.Code, "malformed": "nope"} {
		rec := f.previewV2(t, code)
		assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeInviteInvalid)
		if first == "" {
			first = rec.Body.String()
		} else if rec.Body.String() != first {
			t.Errorf("%s: %s differs from %s", name, rec.Body, first)
		}
	}
	// Paused answers whatever the code.
	if _, err := f.srv.store.SetInvitesPaused(context.Background(), "system", true); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.previewV2(t, fakeInviteCode), http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused)
}

// The preview is limited per address as v1's is, before the body is read.
func TestPreviewMemberLinkRateLimited(t *testing.T) {
	f := newInviteFixture(t, Config{})
	limits := looseInviteLimits()
	limits.previewPerAddress = inviteLimit{"preview_per_address", 2, time.Hour}
	f.srv.setInviteLimits(limits)
	for range 2 {
		assertErrorBody(t, f.send(t, "POST", "/v2/invites/preview", "", `{}`, "198.51.100.7:1"), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
	}
	assertErrorBody(t, f.send(t, "POST", "/v2/invites/preview", "", `{}`, "198.51.100.7:1"), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
}

// The joiners of a link, over REST: its maker and an operator read them; a
// member, a guest, an ended guest, an agent get the bytes of a ghost id.
func TestMemberLinkJoinersRoute(t *testing.T) {
	f := newInviteFixture(t, Config{})
	c := f.linkCallers(t)
	link := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	opsLink := f.createMemberLink(t, c.op, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	for _, n := range []string{"wren", "moss"} {
		if rec := f.redeem(t, link.Code, n); rec.Code != http.StatusCreated {
			t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
		}
	}
	path := func(id int64) string { return fmt.Sprintf("/v2/invites/%d/joiners", id) }
	ghost := f.send(t, "GET", path(link.Invite.ID+500), c.member, "", "")
	assertErrorBody(t, ghost, http.StatusNotFound, schema.ErrorCodeInviteNotFound)

	tests := []struct {
		name  string
		token string
		id    int64
		// want is the joiners' names, or nil for the ghost's answer.
		want []string
	}{
		{"the maker", c.member, link.Invite.ID, []string{"wren", "moss"}},
		{"an operator", c.op, link.Invite.ID, []string{"wren", "moss"}},
		{"a non-owner operator", c.op2, link.Invite.ID, []string{"wren", "moss"}},
		{"an operator's own link, no joiners", c.op, opsLink.Invite.ID, []string{}},
		{"another member", c.quiet, link.Invite.ID, nil},
		{"a member, an operator's link", c.member, opsLink.Invite.ID, nil},
		{"a guest", c.guest, link.Invite.ID, nil},
		{"an agent", c.agent, link.Invite.ID, nil},
		{"the maker, a ghost id", c.member, link.Invite.ID + 500, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.send(t, "GET", path(tt.id), tt.token, "", "")
			if tt.want == nil {
				assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeInviteNotFound)
				if rec.Body.String() != ghost.Body.String() {
					t.Errorf("body %s differs from a ghost id's %s", rec.Body, ghost.Body)
				}
				if rec.Header().Get("Content-Type") != ghost.Header().Get("Content-Type") {
					t.Errorf("content type differs from a ghost id's")
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			names := []string{}
			for _, j := range decodeBody[schema.InviteJoinersResponseV1](t, rec).Joiners {
				names = append(names, j.Name)
			}
			if !slices.Equal(names, tt.want) {
				t.Errorf("joiners = %v, want %v", names, tt.want)
			}
		})
	}
	assertErrorBody(t, f.send(t, "GET", path(link.Invite.ID), c.endedGuest, "", ""), http.StatusUnauthorized, "unauthenticated")
	assertErrorBody(t, f.send(t, "GET", path(link.Invite.ID), "", "", ""), http.StatusUnauthorized, "unauthenticated")
	// The ghost answer leaves no audit row that tells it from a hit.
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && strings.Contains(e.Subject, "joiners") {
			t.Errorf("a refusal audited as %s %s", e.Action, e.Detail)
		}
	}
	// InviteV2.joiners counts them.
	list := decodeBody[schema.ListInvitesResponseV2](t, f.send(t, "GET", "/v2/invites", c.member, "", ""))
	if len(list.Invites) != 1 || list.Invites[0].Joiners != 2 || list.Invites[0].Uses != 2 {
		t.Errorf("list = %+v, want joiners 2 uses 2", list.Invites)
	}
}

// A request for a v1 body or kind on the v2 route, and the retired v1 route,
// never leave a row behind; the v1 list/preview/close keep working for a
// member link (the projections of v2 rows are I6b's, so only that they do not
// fail).
func TestMemberLinkOnV1Routes(t *testing.T) {
	f := newInviteFixture(t, Config{})
	c := f.linkCallers(t)
	link := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	if rec := f.send(t, "GET", "/v1/invites", c.member, "", ""); rec.Code != http.StatusOK {
		t.Errorf("v1 list = %d %s", rec.Code, rec.Body)
	}
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", link.Invite.ID), c.member, "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("v1 close = %d %s", rec.Code, rec.Body)
	}
}
