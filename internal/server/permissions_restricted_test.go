package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #1014 (ADR-014 A3a, docs/design/admin-roles.md §7.2.1, §7.3, §7.7):
// what an operator may do is what it holds by the permissions document,
// never its role. These are the restricted-operator cases A3a decides, as
// server tests. Each seeds the document through the store, as no route can
// change it before A3b; A3b turns them into isolation rows through the real
// PUT /v1/permissions.

// restrictedFixture is an auth fixture with an owner (the fixture's root),
// an operator, operator-1, two members and an agent; channel-a, the owner's
// private channel, which operator-1 is not in; and channel-b, which
// operator-1 manages and member-1 decides in.
type restrictedFixture struct {
	*authFixture
	op, member, member2, agent          store.Principal
	opTok, memberTok, member2Tok, agTok string
	chanA, chanB                        store.Channel
}

func newRestrictedFixture(t *testing.T, cfg Config) *restrictedFixture {
	t.Helper()
	ctx := context.Background()
	f := &restrictedFixture{authFixture: newAuthFixtureWithConfig(t, cfg)}
	owner := fmt.Sprintf("principal:%d", f.root.ID)
	// mk makes a principal, promoted to operator when op is set, and its
	// credential (made after the promotion).
	mk := func(kind store.PrincipalKind, name string, op bool) (store.Principal, string) {
		t.Helper()
		p, err := f.srv.store.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatalf("CreatePrincipal %s: %v", name, err)
		}
		if op {
			if _, err := f.srv.store.SetRole(ctx, owner, f.root.ID, p.ID, store.RoleOperator, nil); err != nil {
				t.Fatalf("promote %s: %v", name, err)
			}
			if p, err = f.srv.store.PrincipalByID(ctx, p.ID); err != nil {
				t.Fatal(err)
			}
		}
		_, tok, err := f.srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatalf("CreateCredential %s: %v", name, err)
		}
		return p, tok
	}
	f.op, f.opTok = mk(store.PrincipalHuman, "operator-1", true)
	f.member, f.memberTok = mk(store.PrincipalHuman, "member-1", false)
	f.member2, f.member2Tok = mk(store.PrincipalHuman, "member-2", false)
	f.agent, f.agTok = mk(store.PrincipalAgent, "agent-1", false)
	var err error

	if f.chanA, err = f.srv.store.CreateChannelAs(ctx, owner, "channel-a", f.root.ID); err != nil {
		t.Fatal(err)
	}
	if f.chanB, err = f.srv.store.CreateChannel(ctx, "channel-b"); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	for _, m := range []struct {
		p                  store.Principal
		canDecide, manages *bool
	}{
		{f.op, &no, &yes},
		{f.member, &yes, &no},
		{f.root, &yes, &no},
	} {
		if _, err := f.srv.store.PutChannelMember(ctx, owner, f.chanB.ID, m.p.ID, f.root.ID, m.canDecide, m.manages); err != nil {
			t.Fatalf("seed channel-b member %s: %v", m.p.Name, err)
		}
	}
	// operator-1 and member-2 share general, so a manager may add member-2
	// to channel-b (the manager-add bound, ADR-010 §3.2).
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []store.Principal{f.op, f.member2} {
		if _, err := f.srv.store.AddChannelMember(ctx, "system", general.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// setOperatorCells sets the operator column's cells for perms to granted,
// as the owner, through the store.
func (f *restrictedFixture) setOperatorCells(t *testing.T, granted bool, perms ...schema.Permission) {
	t.Helper()
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision}
	for _, p := range perms {
		req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleOperator, Granted: granted})
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, req, time.Now()); err != nil {
		t.Fatalf("set operator cells %v to %t: %v", perms, granted, err)
	}
}

// withhold takes perms from every operator.
func (f *restrictedFixture) withhold(t *testing.T, perms ...schema.Permission) {
	t.Helper()
	f.setOperatorCells(t, false, perms...)
}

// auditHas reports whether an audit row by actor of action carries detail.
func (f *restrictedFixture) auditHas(t *testing.T, actor store.Principal, action, detail string) bool {
	t.Helper()
	for _, e := range f.audit(t) {
		if e.Actor == fmt.Sprintf("principal:%d", actor.ID) && e.Action == action && strings.Contains(e.Detail, detail) {
			return true
		}
	}
	return false
}

// permissionOf is the permission a refusal names.
func permissionOf(t *testing.T, body []byte) schema.Permission {
	t.Helper()
	var e schema.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return e.Permission
}

// TestRestrictedOperatorManagerHasManagerStanding: an operator without
// manage_channels, in a channel it manages, has exactly a member-manager's
// rights (§7.2.1). Under the default document the same operator has
// operator standing, which the first rows pin, so the mapping is shown to
// be the permission and not the role.
func TestRestrictedOperatorManagerHasManagerStanding(t *testing.T) {
	const detail = "permission=manage_channels"
	member := func(p store.Principal) string {
		return fmt.Sprintf("/v1/channels/channel-b/members/%d", p.ID)
	}

	t.Run("defaults: operator standing", func(t *testing.T) {
		f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
		if rec := f.do(t, "PUT", member(f.member2), f.opTok, `{"can_decide":true}`); rec.Code != http.StatusNoContent {
			t.Fatalf("operator sets can_decide = %d %s", rec.Code, rec.Body)
		}
		if rec := f.do(t, "PUT", member(f.agent), f.opTok, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("operator adds an agent = %d %s", rec.Code, rec.Body)
		}
	})

	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	f.withhold(t, schema.PermManageChannels)

	for _, tc := range []struct {
		name, body string
	}{
		{"set can_decide", `{"can_decide":true}`},
		{"set manages", `{"manages":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, "PUT", member(f.member2), f.opTok, tc.body)
			assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
			if got := permissionOf(t, rec.Body.Bytes()); got != schema.PermManageChannels {
				t.Errorf("permission = %q, want manage_channels", got)
			}
			if !f.auditHas(t, f.op, "access_denied", detail) {
				t.Errorf("refusal not audited access_denied with %s", detail)
			}
		})
	}
	t.Run("add an agent", func(t *testing.T) {
		rec := f.do(t, "PUT", member(f.agent), f.opTok, "")
		assertErrorBody(t, rec, http.StatusNotFound, "principal_not_found")
		if !f.auditHas(t, f.op, store.AuditMemberAddRefused, detail) {
			t.Errorf("refusal not audited member_add_refused with %s", detail)
		}
		if ok, err := f.srv.store.IsChannelMember(context.Background(), f.chanB.ID, f.agent.ID); err != nil || ok {
			t.Fatalf("agent in channel-b = %v, %v; want not", ok, err)
		}
	})
	t.Run("add an agent by name", func(t *testing.T) {
		rec := f.do(t, "POST", "/v1/channels/channel-b/members", f.opTok, fmt.Sprintf(`{"name":%q}`, f.agent.Name))
		assertErrorBody(t, rec, http.StatusNotFound, "principal_not_found")
	})
	for _, target := range []struct {
		name string
		p    func() store.Principal
	}{
		{"remove an approver", func() store.Principal { return f.member }},
		{"remove the owner", func() store.Principal { return f.root }},
	} {
		t.Run(target.name, func(t *testing.T) {
			rec := f.do(t, "DELETE", member(target.p()), f.opTok, "")
			assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
			if ok, err := f.srv.store.IsChannelMember(context.Background(), f.chanB.ID, target.p().ID); err != nil || !ok {
				t.Fatalf("%s still a member = %v, %v; want yes", target.p().Name, ok, err)
			}
			if !f.auditHas(t, f.op, "access_denied", detail) {
				t.Errorf("refusal not audited with %s", detail)
			}
		})
	}
	t.Run("remove an operator", func(t *testing.T) {
		ctx := context.Background()
		other, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "operator-2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.SetRole(ctx, "system", f.root.ID, other.ID, store.RoleOperator, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.AddChannelMember(ctx, "system", f.chanB.ID, other.ID, 0); err != nil {
			t.Fatal(err)
		}
		rec := f.do(t, "DELETE", member(other), f.opTok, "")
		assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
	})
	t.Run("a plain member it may still add", func(t *testing.T) {
		if rec := f.do(t, "PUT", member(f.member2), f.opTok, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("restricted manager adds a member = %d %s", rec.Code, rec.Body)
		}
		ms := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/channel-b/members", f.opTok, ""))
		for _, m := range ms.Members {
			if m.PrincipalID == f.member2.ID && (m.CanDecide == nil || *m.CanDecide || m.Manages == nil || *m.Manages) {
				t.Fatalf("member-2 = %+v, want can_decide and manages false", m)
			}
		}
	})
	t.Run("a member manager's refusal names no permission", func(t *testing.T) {
		ctx := context.Background()
		yes := true
		if _, err := f.srv.store.PutChannelMember(ctx, "system", f.chanB.ID, f.member2.ID, 0, nil, &yes); err != nil {
			t.Fatal(err)
		}
		rec := f.do(t, "PUT", member(f.agent), f.member2Tok, `{"can_decide":true}`)
		if rec.Code == http.StatusNoContent {
			t.Fatalf("member manager set can_decide on an agent")
		}
		rec = f.do(t, "PUT", member(f.member), f.member2Tok, `{"can_decide":false}`)
		assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
		if got := permissionOf(t, rec.Body.Bytes()); got != "" {
			t.Errorf("member's refusal names %q, want nothing (a member cannot hold manage_channels)", got)
		}
	})
}

// TestRestrictedOperatorReadsAsAMember: without manage_channels, a channel
// the operator is not in answers the uniform 404, and the lists that name
// channels show only its own, with counts (§7.3, SF-1).
func TestRestrictedOperatorReadsAsAMember(t *testing.T) {
	ctx := context.Background()
	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	if _, err := f.srv.store.CreateNet(ctx, "system", f.chanA.ID, "net-a", f.root.ID); err != nil {
		t.Fatal(err)
	}
	// Defaults: every operator holds manage_channels and sees channel-a.
	if rec := f.do(t, "GET", "/v1/channels/channel-a/members", f.opTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("default operator lists channel-a members = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "GET", "/v1/channels/channel-a/nets", f.opTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("default operator lists channel-a nets = %d %s", rec.Code, rec.Body)
	}

	f.withhold(t, schema.PermManageChannels)
	t.Run("members of a channel it is not in", func(t *testing.T) {
		assertErrorBody(t, f.do(t, "GET", "/v1/channels/channel-a/members", f.opTok, ""), http.StatusNotFound, "channel_not_found")
	})
	t.Run("nets of a channel it is not in", func(t *testing.T) {
		assertErrorBody(t, f.do(t, "GET", "/v1/channels/channel-a/nets", f.opTok, ""), http.StatusNotFound, "channel_not_found")
	})
	t.Run("its own channel's members show roles as a manager's view", func(t *testing.T) {
		ms := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/channel-b/members", f.opTok, ""))
		if len(ms.Members) == 0 || ms.Members[0].Role == "" {
			t.Fatalf("manager's list = %+v, want roles shown", ms.Members)
		}
	})
	t.Run("a member list it does not manage hides roles", func(t *testing.T) {
		if _, err := f.srv.store.AddChannelMember(ctx, "system", f.chanA.ID, f.op.ID, 0); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := f.srv.store.RemoveChannelMember(ctx, "system", f.chanA.ID, f.op.ID); err != nil {
				t.Fatal(err)
			}
		}()
		ms := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/channel-a/members", f.opTok, ""))
		for _, m := range ms.Members {
			if m.Role != "" {
				t.Fatalf("member %d shown with role %q to a restricted operator that does not manage channel-a", m.PrincipalID, m.Role)
			}
			if m.PrincipalID != f.op.ID && m.CanDecide != nil {
				t.Fatalf("member %d shown with can_decide to a restricted operator that does not manage channel-a", m.PrincipalID)
			}
		}
	})
}

// TestRestrictedOperatorInvites: links read manage_members and
// manage_channels at creation and again at redemption (§7.3, BLOCK-2).
func TestRestrictedOperatorInvites(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	invite := func(t *testing.T, f *restrictedFixture, tok, body string) *httptest.ResponseRecorder {
		t.Helper()
		return f.do(t, "POST", "/v1/invites", tok, body)
	}

	t.Run("creation", func(t *testing.T) {
		f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
		// Defaults: an operator names any channel, makes open links and grants may_invite.
		if rec := invite(t, f, f.opTok, `{"kind":"personal","channels":["channel-a"],"may_invite":true}`); rec.Code != http.StatusCreated {
			t.Fatalf("default operator's invite = %d %s", rec.Code, rec.Body)
		}
		f.withhold(t, schema.PermManageChannels, schema.PermActAsMembers, schema.PermManageMembers)
		assertErrorBody(t, invite(t, f, f.opTok, `{"kind":"personal","channels":["channel-a"]}`), http.StatusForbidden, schema.ErrorCodeInviteGrantExceedsInviter)
		assertErrorBody(t, invite(t, f, f.opTok, `{"kind":"personal","channels":["no-such-channel"]}`), http.StatusForbidden, schema.ErrorCodeInviteGrantExceedsInviter)
		assertErrorBody(t, invite(t, f, f.opTok, `{"kind":"open","channels":["channel-b"]}`), http.StatusForbidden, schema.ErrorCodeInviteNotAllowed)
		assertErrorBody(t, invite(t, f, f.opTok, `{"kind":"personal","channels":["channel-b"],"may_invite":true}`), http.StatusForbidden, schema.ErrorCodeInviteGrantExceedsInviter)
		if rec := invite(t, f, f.opTok, `{"kind":"personal","channels":["channel-b"]}`); rec.Code != http.StatusCreated {
			t.Fatalf("restricted operator's invite to its own channel = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("redemption after losing manage_channels", func(t *testing.T) {
		f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
		both, err := f.srv.store.CreateInvite(ctx, "system", f.op.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-a", "channel-b"}}, store.InviteOptions{}, now)
		if err != nil {
			t.Fatal(err)
		}
		onlyA, err := f.srv.store.CreateInvite(ctx, "system", f.op.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-a"}}, store.InviteOptions{}, now)
		if err != nil {
			t.Fatal(err)
		}
		f.withhold(t, schema.PermManageChannels)
		red, err := f.srv.store.RedeemInvite(ctx, store.HashInviteCode(both.Code), "joiner-1", now)
		if err != nil {
			t.Fatalf("redeem: %v", err)
		}
		if len(red.DroppedChannels) != 1 || red.DroppedChannels[0] != f.chanA.ID {
			t.Fatalf("dropped = %v, want channel-a only", red.DroppedChannels)
		}
		if in, err := f.srv.store.IsChannelMember(ctx, f.chanA.ID, red.Principal.ID); err != nil || in {
			t.Fatalf("joiner in channel-a = %v, %v; want not", in, err)
		}
		if in, err := f.srv.store.IsChannelMember(ctx, f.chanB.ID, red.Principal.ID); err != nil || !in {
			t.Fatalf("joiner in channel-b = %v, %v; want yes", in, err)
		}
		audited := false
		for _, e := range f.audit(t) {
			if e.Action == store.AuditInviteRedeemed && strings.Contains(e.Detail, fmt.Sprintf("dropped_channels=%d", f.chanA.ID)) {
				audited = true
			}
		}
		if !audited {
			t.Error("the dropped channel is not in invite_redeemed's dropped_channels")
		}
		if _, err := f.srv.store.PreviewInvite(ctx, store.HashInviteCode(onlyA.Code), now); !errors.Is(err, store.ErrInviteInvalid) {
			t.Fatalf("an invite left with no channel previews %v, want invite_invalid", err)
		}
	})

	t.Run("open links and may_invite grants are suspended without manage_members", func(t *testing.T) {
		f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
		yes := true
		open, err := f.srv.store.CreateInvite(ctx, "system", f.op.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"channel-b"}}, store.InviteOptions{}, now)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := f.srv.store.CreateInvite(ctx, "system", f.op.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-b"}, MayInvite: &yes}, store.InviteOptions{}, now)
		if err != nil {
			t.Fatal(err)
		}
		f.withhold(t, schema.PermActAsMembers, schema.PermManageMembers)
		if _, err := f.srv.store.PreviewInvite(ctx, store.HashInviteCode(open.Code), now); !errors.Is(err, store.ErrInviteInvalid) {
			t.Fatalf("open link of a maker without manage_members previews %v, want invite_invalid", err)
		}
		red, err := f.srv.store.RedeemInvite(ctx, store.HashInviteCode(grant.Code), "joiner-2", now)
		if err != nil {
			t.Fatal(err)
		}
		if red.Principal.MayInvite {
			t.Fatal("a may_invite grant landed although its maker no longer holds manage_members")
		}
		// Suspended, not closed: it works again when the permission comes back.
		f.setOperatorCells(t, true, schema.PermManageMembers, schema.PermActAsMembers)
		if _, err := f.srv.store.PreviewInvite(ctx, store.HashInviteCode(open.Code), now); err != nil {
			t.Fatalf("open link after manage_members returned previews %v, want usable", err)
		}
	})

	t.Run("listing and closing", func(t *testing.T) {
		f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
		ownerInv, err := f.srv.store.CreateInvite(ctx, "system", f.root.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-a"}}, store.InviteOptions{}, now)
		if err != nil {
			t.Fatal(err)
		}
		mine, err := f.srv.store.CreateInvite(ctx, "system", f.op.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-a", "channel-b"}}, store.InviteOptions{}, now)
		if err != nil {
			t.Fatal(err)
		}
		list := decodeBody[schema.ListInvitesResponseV1](t, f.do(t, "GET", "/v1/invites", f.opTok, ""))
		if len(list.Invites) != 2 {
			t.Fatalf("default operator lists %d invites, want everyone's 2", len(list.Invites))
		}
		for _, inv := range list.Invites {
			if inv.OtherChannels != 0 {
				t.Fatalf("default operator's invite %d hides %d channels", inv.ID, inv.OtherChannels)
			}
		}

		f.withhold(t, schema.PermManageChannels, schema.PermActAsMembers, schema.PermManageMembers)
		list = decodeBody[schema.ListInvitesResponseV1](t, f.do(t, "GET", "/v1/invites", f.opTok, ""))
		if len(list.Invites) != 1 || list.Invites[0].ID != mine.Invite.ID {
			t.Fatalf("restricted operator lists %+v, want its own invite only", list.Invites)
		}
		got := list.Invites[0]
		if len(got.Channels) != 1 || got.Channels[0].Name != "channel-b" || got.OtherChannels != 1 {
			t.Fatalf("restricted operator's invite channels = %+v other %d, want channel-b and 1 other", got.Channels, got.OtherChannels)
		}
		list = decodeBody[schema.ListInvitesResponseV1](t, f.do(t, "GET", fmt.Sprintf("/v1/invites?created_by=%d", f.root.ID), f.opTok, ""))
		if len(list.Invites) != 0 {
			t.Fatalf("restricted operator asked for the owner's invites and got %+v", list.Invites)
		}
		rec := f.do(t, "DELETE", fmt.Sprintf("/v1/invites/%d", ownerInv.Invite.ID), f.opTok, "")
		assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeInviteNotFound)
		if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/invites/%d", mine.Invite.ID), f.opTok, ""); rec.Code >= 300 {
			t.Fatalf("closing its own invite = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the allowance binds without manage_members", func(t *testing.T) {
		f := newRestrictedFixture(t, Config{AuthMode: AuthRequired, InviteAllowance: 1})
		who := decodeBody[schema.WhoAmIResponseV1](t, f.do(t, "GET", "/v1/whoami", f.opTok, ""))
		if who.InvitesRemaining != nil || !who.MayInvite {
			t.Fatalf("default operator whoami may_invite %t remaining %v, want true and none", who.MayInvite, who.InvitesRemaining)
		}
		f.withhold(t, schema.PermActAsMembers, schema.PermManageMembers)
		who = decodeBody[schema.WhoAmIResponseV1](t, f.do(t, "GET", "/v1/whoami", f.opTok, ""))
		if !who.MayInvite || who.InvitesRemaining == nil || *who.InvitesRemaining != 1 {
			t.Fatalf("restricted operator whoami may_invite %t remaining %v, want true and 1", who.MayInvite, who.InvitesRemaining)
		}
		if rec := invite(t, f, f.opTok, `{"kind":"personal","channels":["channel-b"]}`); rec.Code != http.StatusCreated {
			t.Fatalf("first invite = %d %s", rec.Code, rec.Body)
		}
		assertErrorBody(t, invite(t, f, f.opTok, `{"kind":"personal","channels":["channel-b"]}`), http.StatusForbidden, schema.ErrorCodeInviteAllowanceExceeded)
	})
}

// TestRestrictedOperatorSignInLinks: sign-in, reset and enrolment links read
// act_as_members when made and again when used (§7.3, §7.5, N3).
func TestRestrictedOperatorSignInLinks(t *testing.T) {
	ctx := context.Background()
	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	now := time.Now()
	link, err := f.srv.store.CreateSignInLink(ctx, "system", f.op.ID, f.member2.ID, schema.CreateSignInLinkRequestV1{}, now)
	if err != nil {
		t.Fatalf("default operator's link: %v", err)
	}
	hash := store.HashSignInLinkCode(link.Code)
	if _, err := f.srv.store.PreviewSignInLink(ctx, hash, now); err != nil {
		t.Fatalf("preview before: %v", err)
	}
	f.withhold(t, schema.PermActAsMembers)
	if _, err := f.srv.store.PreviewSignInLink(ctx, hash, now); !errors.Is(err, store.ErrSignInLinkInvalid) {
		t.Fatalf("preview of a link whose maker lost act_as_members = %v, want invalid", err)
	}
	if _, err := f.srv.store.RedeemSignInLink(ctx, hash, now); !errors.Is(err, store.ErrSignInLinkInvalid) {
		t.Fatalf("redeeming a link whose maker lost act_as_members = %v, want invalid", err)
	}
	if _, err := f.srv.store.CreateSignInLink(ctx, "system", f.op.ID, f.member2.ID, schema.CreateSignInLinkRequestV1{}, now); !errors.Is(err, store.ErrSignInLinkNotAllowed) {
		t.Fatalf("making a link without act_as_members = %v, want not allowed", err)
	}
	// Suspended, not closed.
	f.setOperatorCells(t, true, schema.PermActAsMembers)
	if _, err := f.srv.store.PreviewSignInLink(ctx, hash, now); err != nil {
		t.Fatalf("preview after act_as_members returned: %v", err)
	}
}

// TestRestrictedOperatorSessionHosts: a session's runtime and host need
// manage_members, or the session's own principal (§7.3).
func TestRestrictedOperatorSessionHosts(t *testing.T) {
	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	if rec := f.do(t, "POST", "/v1/sessions", f.botTok, `{"client":"test","runtime":"runtime-1","host":"host-1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("agent session = %d %s", rec.Code, rec.Body)
	}
	hosts := func() (runtime, host string) {
		t.Helper()
		roster := decodeBody[schema.SessionRosterV1](t, f.do(t, "GET", "/v1/sessions?channel=general", f.opTok, ""))
		for _, s := range roster.Sessions {
			if s.PrincipalID == f.bot.ID {
				return s.Runtime, s.Host
			}
		}
		t.Fatalf("no agent session in %+v", roster.Sessions)
		return "", ""
	}
	if r, h := hosts(); r != "runtime-1" || h != "host-1" {
		t.Fatalf("default operator sees runtime %q host %q, want both", r, h)
	}
	f.withhold(t, schema.PermActAsMembers, schema.PermManageMembers)
	if r, h := hosts(); r != "" || h != "" {
		t.Fatalf("restricted operator sees runtime %q host %q, want neither", r, h)
	}
}

// TestRestrictedOperatorFilteredNames: hooks and the joining settings show
// only the caller's channels without manage_channels, with counts, and a
// report comes back without its redaction list (§7.3, SF-1).
func TestRestrictedOperatorFilteredNames(t *testing.T) {
	ctx := context.Background()
	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	for _, ch := range []store.Channel{f.chanA, f.chanB} {
		if _, err := f.srv.store.CreateHook(ctx, "system", "hook-"+ch.Name, ch.ID, f.member.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.srv.store.PutJoinSettings(ctx, "system", f.root.ID, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"channel-a", "channel-b"}}); err != nil {
		t.Fatal(err)
	}
	report := decodeBody[schema.CreateProblemReportResponseV1](t, f.do(t, "POST", "/v1/reports", f.memberTok, reportBody(t)))

	hooks := decodeBody[schema.ListHooksResponseV1](t, f.do(t, "GET", "/v1/hooks", f.opTok, ""))
	if len(hooks.Hooks) != 2 || hooks.OtherHooks != 0 {
		t.Fatalf("default operator's hooks = %+v", hooks)
	}
	join := decodeBody[schema.JoinSettingsResponseV1](t, f.do(t, "GET", "/v1/join/settings", f.opTok, ""))
	if len(join.Settings.OpenChannels) != 2 || join.Settings.OtherOpenChannels != 0 {
		t.Fatalf("default operator's open channels = %v other %d", join.Settings.OpenChannels, join.Settings.OtherOpenChannels)
	}
	got := decodeBody[schema.ProblemReportV1](t, f.do(t, "GET", fmt.Sprintf("/v1/reports/%d", report.ID), f.opTok, ""))
	if got.Redact == nil || len(got.Redact.Channels) == 0 {
		t.Fatalf("default operator's report has no redaction list: %+v", got.Redact)
	}

	f.withhold(t, schema.PermManageChannels)
	hooks = decodeBody[schema.ListHooksResponseV1](t, f.do(t, "GET", "/v1/hooks", f.opTok, ""))
	if len(hooks.Hooks) != 1 || hooks.Hooks[0].Channel != "channel-b" || hooks.OtherHooks != 1 {
		t.Fatalf("restricted operator's hooks = %+v, want channel-b's and 1 other", hooks)
	}
	join = decodeBody[schema.JoinSettingsResponseV1](t, f.do(t, "GET", "/v1/join/settings", f.opTok, ""))
	if len(join.Settings.OpenChannels) != 1 || join.Settings.OpenChannels[0] != "channel-b" || join.Settings.OtherOpenChannels != 1 {
		t.Fatalf("restricted operator's open channels = %v other %d, want channel-b and 1 other", join.Settings.OpenChannels, join.Settings.OtherOpenChannels)
	}
	got = decodeBody[schema.ProblemReportV1](t, f.do(t, "GET", fmt.Sprintf("/v1/reports/%d", report.ID), f.opTok, ""))
	if got.Redact != nil {
		t.Fatalf("restricted operator's report carries the redaction list: %+v", got.Redact)
	}
	if got.Did == "" {
		t.Fatal("the report's own text was lost")
	}
	// The owner still sees everything.
	if got := decodeBody[schema.ProblemReportV1](t, f.do(t, "GET", fmt.Sprintf("/v1/reports/%d", report.ID), f.rootTok, "")); got.Redact == nil {
		t.Fatal("the owner's report has no redaction list")
	}
}

// TestRestrictedOperatorOperatorOrSelf: the operator-or-self routes read
// manage_members for anyone but oneself (§7.2). Revoke-all is the third; its
// re-authentication gate answers first for an operator acting on another,
// so it is pinned in A3b's isolation rows, not here.
func TestRestrictedOperatorOperatorOrSelf(t *testing.T) {
	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	rename := fmt.Sprintf("/v1/principals/%d/name", f.member2.ID)
	manifest := fmt.Sprintf("/v1/principals/%d/manifest", f.agent.ID)
	if rec := f.do(t, "PUT", rename, f.opTok, `{"name":"member-2b"}`); rec.Code != http.StatusOK {
		t.Fatalf("default operator renames member-2 = %d %s", rec.Code, rec.Body)
	}
	f.withhold(t, schema.PermActAsMembers, schema.PermManageMembers)
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", rename, `{"name":"member-2c"}`},
		{"GET", manifest, ""},
	} {
		rec := f.do(t, tc.method, tc.path, f.opTok, tc.body)
		assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
		if got := permissionOf(t, rec.Body.Bytes()); got != schema.PermManageMembers {
			t.Errorf("%s %s refusal names %q, want manage_members", tc.method, tc.path, got)
		}
	}
	if !f.auditHas(t, f.op, "access_denied", "permission=manage_members") {
		t.Error("refusal not audited with permission=manage_members")
	}
	// Itself, within the self-rename limits.
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/name", f.op.ID), f.opTok, `{"name":"operator-1b"}`); rec.Code != http.StatusOK {
		t.Fatalf("restricted operator renames itself = %d %s", rec.Code, rec.Body)
	}
}
