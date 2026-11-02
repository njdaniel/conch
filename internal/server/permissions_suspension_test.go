package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #1016 (ADR-014 A4, design §7.3, §7.5, §8.5): a role-level change of
// make_member_links or act_as_members suspends the links of the makers it
// affects, rather than closing them, and each change writes one audit row per
// link. The per-person may_invite brake keeps its close-on-off rule.

// auditRowsFor lists the audit rows of action, each as "actor subject detail".
func auditRowsFor(t *testing.T, f *authFixture, action string) []store.AuditEvent {
	t.Helper()
	return auditRows(t, f, action)
}

// subjects are the subjects of rows, sorted.
func subjects(rows []store.AuditEvent) map[string]string {
	out := map[string]string{}
	for _, e := range rows {
		out[e.Subject] = e.Actor + " " + e.Detail
	}
	return out
}

func TestRoleChangeSuspendsMemberLinks(t *testing.T) {
	ctx := context.Background()
	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	now := time.Now()
	owner := fmt.Sprintf("principal:%d", f.root.ID)
	if _, err := f.srv.store.SetMayInvite(ctx, owner, f.member.ID, true); err != nil {
		t.Fatal(err)
	}
	mk := func(maker store.Principal) (id int64, hash string) {
		t.Helper()
		inv, err := f.srv.store.CreateInvite(ctx, fmt.Sprintf("principal:%d", maker.ID), maker.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-b"}}, store.InviteOptions{}, now)
		if err != nil {
			t.Fatalf("%s makes a link: %v", maker.Name, err)
		}
		return inv.Invite.ID, store.HashInviteCode(inv.Code)
	}
	a, hashA := mk(f.member)
	b, hashB := mk(f.member)
	closed, _ := mk(f.member)
	if _, err := f.srv.store.CloseInvite(ctx, owner, f.root.ID, closed, now); err != nil {
		t.Fatal(err)
	}
	opLink, hashOp := mk(f.op)
	subj := func(id int64) string { return fmt.Sprintf("invite:%d", id) }

	put := func(granted bool) {
		t.Helper()
		res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f), Changes: []schema.PermissionChangeV1{
			{Permission: schema.PermMakeMemberLinks, Role: schema.PermissionRoleMember, Granted: granted}}})
		if res.code != http.StatusOK {
			t.Fatalf("PUT make_member_links=%t: %d %s", granted, res.code, res.body)
		}
	}
	usable := func(hash string) error {
		_, err := f.srv.store.PreviewInvite(ctx, hash, now)
		return err
	}

	put(false)
	for _, h := range []string{hashA, hashB} {
		if err := usable(h); !errors.Is(err, store.ErrInviteInvalid) {
			t.Errorf("preview of a suspended link = %v, want invalid", err)
		}
		if _, err := f.srv.store.RedeemInvite(ctx, h, "newcomer", now); !errors.Is(err, store.ErrInviteInvalid) {
			t.Errorf("redeeming a suspended link = %v, want invalid", err)
		}
	}
	if err := usable(hashOp); err != nil {
		t.Errorf("an operator's link, whose cell is on, = %v, want usable", err)
	}
	rows := subjects(auditRowsFor(t, f.authFixture, "invite_suspended"))
	if len(rows) != 2 || rows[subj(a)] == "" || rows[subj(b)] == "" {
		t.Fatalf("invite_suspended rows = %v, want one each for %s and %s", rows, subj(a), subj(b))
	}
	for s, r := range rows {
		if !strings.HasPrefix(r, "system ") || !strings.Contains(r, "reason=permission") {
			t.Errorf("%s: row %q, want actor system and reason=permission", s, r)
		}
	}
	if _, ok := rows[subj(opLink)]; ok {
		t.Error("an operator's link was suspended by a member-column change")
	}
	if _, ok := rows[subj(closed)]; ok {
		t.Error("a closed link was suspended")
	}
	// Nothing is closed.
	if n := len(auditRowsFor(t, f.authFixture, "invite_closed")); n != 1 {
		t.Errorf("%d invite_closed rows, want 1 (the one closed by hand)", n)
	}

	// A change that does not touch the cell writes nothing more.
	if res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f), Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermCreatePrivateChannels, Role: schema.PermissionRoleMember, Granted: false}}}); res.code != http.StatusOK {
		t.Fatalf("PUT: %d %s", res.code, res.body)
	}
	if n := len(auditRowsFor(t, f.authFixture, "invite_suspended")); n != 2 {
		t.Errorf("%d invite_suspended rows after an unrelated change, want 2", n)
	}

	put(true)
	resumed := subjects(auditRowsFor(t, f.authFixture, "invite_resumed"))
	if len(resumed) != 2 || resumed[subj(a)] == "" || resumed[subj(b)] == "" {
		t.Fatalf("invite_resumed rows = %v, want one each for %s and %s", resumed, subj(a), subj(b))
	}
	for s, r := range resumed {
		if !strings.HasPrefix(r, "system ") || !strings.Contains(r, "reason=permission") {
			t.Errorf("%s: row %q, want actor system and reason=permission", s, r)
		}
	}
	for _, h := range []string{hashA, hashB} {
		if err := usable(h); err != nil {
			t.Errorf("preview after the permission returned = %v, want usable", err)
		}
	}
	if _, ok := subjects(auditRowsFor(t, f.authFixture, "invite_resumed"))[subj(closed)]; ok {
		t.Error("a closed link was resumed")
	}

	// The per-person brake is not the permission: turning it off closes the
	// member's links for good (ADR-013), and giving it back revives none.
	if _, err := f.srv.store.SetMayInvite(ctx, owner, f.member.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{hashA, hashB} {
		if err := usable(h); !errors.Is(err, store.ErrInviteInvalid) {
			t.Errorf("preview after the brake closed the link = %v, want invalid", err)
		}
	}
	if n := len(auditRowsFor(t, f.authFixture, "invite_closed")); n != 3 {
		t.Errorf("%d invite_closed rows, want 3 (the hand-closed link and the brake's two)", n)
	}
	if _, err := f.srv.store.SetMayInvite(ctx, owner, f.member.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := usable(hashA); !errors.Is(err, store.ErrInviteInvalid) {
		t.Errorf("a link the brake closed, after the brake came back = %v, want still invalid", err)
	}
	if n := len(auditRowsFor(t, f.authFixture, "invite_resumed")); n != 2 {
		t.Errorf("%d invite_resumed rows after the brake came back, want 2: closing is not suspending", n)
	}
}

func TestRoleChangeSuspendsSignInLinks(t *testing.T) {
	ctx := context.Background()
	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	now := time.Now()
	mk := func(purpose schema.SignInLinkPurpose) (int64, string) {
		t.Helper()
		l, err := f.srv.store.CreateSignInLink(ctx, "system", f.op.ID, f.member2.ID, schema.CreateSignInLinkRequestV1{Purpose: purpose}, now)
		if err != nil {
			t.Fatalf("operator makes a %s link: %v", purpose, err)
		}
		return l.SignInLink.ID, store.HashSignInLinkCode(l.Code)
	}
	signIn, hashSignIn := mk(schema.SignInLinkPurposeSignIn)
	reset, hashReset := mk(schema.SignInLinkPurposeReset)
	enroll, hashEnroll := mk(schema.SignInLinkPurposeEnrollment)
	subj := func(id int64) string { return fmt.Sprintf("sign_in_link:%d", id) }
	put := func(granted bool) {
		t.Helper()
		res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f), Changes: []schema.PermissionChangeV1{
			{Permission: schema.PermActAsMembers, Role: schema.PermissionRoleOperator, Granted: granted}}})
		if res.code != http.StatusOK {
			t.Fatalf("PUT act_as_members=%t: %d %s", granted, res.code, res.body)
		}
	}

	put(false)
	for _, h := range []string{hashSignIn, hashReset, hashEnroll} {
		if _, err := f.srv.store.PreviewSignInLink(ctx, h, now); !errors.Is(err, store.ErrSignInLinkInvalid) {
			t.Errorf("preview of a suspended link = %v, want invalid", err)
		}
	}
	// Redeemed after the maker lost act_as_members: refused.
	if _, err := f.srv.store.RedeemSignInLink(ctx, hashSignIn, now); !errors.Is(err, store.ErrSignInLinkInvalid) {
		t.Errorf("redeeming a link whose maker lost act_as_members = %v, want invalid", err)
	}
	rows := subjects(auditRowsFor(t, f.authFixture, "sign_in_link_suspended"))
	if len(rows) != 3 || rows[subj(signIn)] == "" || rows[subj(reset)] == "" || rows[subj(enroll)] == "" {
		t.Fatalf("sign_in_link_suspended rows = %v, want one each for %s, %s and %s", rows, subj(signIn), subj(reset), subj(enroll))
	}
	for s, r := range rows {
		if !strings.HasPrefix(r, "system ") || !strings.Contains(r, "reason=permission") {
			t.Errorf("%s: row %q, want actor system and reason=permission", s, r)
		}
	}
	// Nothing closed.
	if n := len(auditRowsFor(t, f.authFixture, store.AuditSignInLinkClosed)); n != 0 {
		t.Errorf("%d sign_in_link_closed rows, want none", n)
	}

	put(true)
	resumed := subjects(auditRowsFor(t, f.authFixture, "sign_in_link_resumed"))
	if len(resumed) != 3 || resumed[subj(signIn)] == "" || resumed[subj(reset)] == "" || resumed[subj(enroll)] == "" {
		t.Fatalf("sign_in_link_resumed rows = %v, want one each", resumed)
	}
	// After the permission is regained it works.
	if _, err := f.srv.store.PreviewSignInLink(ctx, hashSignIn, now); err != nil {
		t.Errorf("preview after the permission returned = %v, want usable", err)
	}
	if _, err := f.srv.store.RedeemSignInLink(ctx, hashSignIn, now); err != nil {
		t.Errorf("redeeming after the permission returned = %v, want it to work", err)
	}
}

// TestCreateInviteByPermissionAndBrake: POST /v1/invites by a member whose role cell is
// off, whose brake is off, or both, and by an operator with may_invite = 0
// (who still makes links, the brake binding members only) and an owner with
// a stored brake. One code, invite_not_allowed, naming make_member_links,
// whichever check said no (design §7.4, §5).
func TestCreateInviteByPermissionAndBrake(t *testing.T) {
	const link = `{"kind":"personal","channels":["channel-b"]}`
	tests := []struct {
		name  string
		who   string // member, operator or owner
		setup func(t *testing.T, f *restrictedFixture)
		want  int
	}{
		{"member, role cell on, brake on", "member", func(t *testing.T, f *restrictedFixture) { f.mayInvite(t, f.member, true) }, http.StatusCreated},
		{"member, role cell off, brake on", "member", func(t *testing.T, f *restrictedFixture) {
			f.mayInvite(t, f.member, true)
			f.memberLinks(t)
		}, http.StatusForbidden},
		{"member, role cell on, brake off", "member", func(t *testing.T, f *restrictedFixture) {}, http.StatusForbidden},
		{"member, role cell off and brake off", "member", func(t *testing.T, f *restrictedFixture) { f.memberLinks(t) }, http.StatusForbidden},
		{"operator with may_invite = 0", "operator", func(t *testing.T, f *restrictedFixture) { f.mayInvite(t, f.op, false) }, http.StatusCreated},
		{"operator with may_invite = 0 and members' cell off", "operator", func(t *testing.T, f *restrictedFixture) {
			f.mayInvite(t, f.op, false)
			f.memberLinks(t)
		}, http.StatusCreated},
		{"owner with a stored brake", "owner", func(t *testing.T, f *restrictedFixture) {
			legacyExec(t, f.srv, "UPDATE principals SET may_invite = 0 WHERE id = ?", f.root.ID)
		}, http.StatusCreated},
		{"owner with every cell off", "owner", func(t *testing.T, f *restrictedFixture) { f.memberLinks(t) }, http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
			tt.setup(t, f)
			tok := map[string]string{"member": f.memberTok, "operator": f.opTok, "owner": f.rootTok}[tt.who]
			rec := f.do(t, "POST", "/v1/invites", tok, link)
			if rec.Code != tt.want {
				t.Fatalf("POST /v1/invites = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
			if tt.want == http.StatusForbidden {
				if e := decodeBody[schema.Error](t, rec); e.Code != schema.ErrorCodeInviteNotAllowed || e.Permission != schema.PermMakeMemberLinks {
					t.Errorf("refusal = %s, want invite_not_allowed naming make_member_links", rec.Body)
				}
			}
		})
	}
}

// mayInvite sets p's may_invite brake through the route's own store call.
func (f *restrictedFixture) mayInvite(t *testing.T, p store.Principal, on bool) {
	t.Helper()
	if _, err := f.srv.store.SetMayInvite(context.Background(), fmt.Sprintf("principal:%d", f.root.ID), p.ID, on); err != nil {
		t.Fatal(err)
	}
}

// memberLinks turns the members' make_member_links cell off through the real
// PUT.
func (f *restrictedFixture) memberLinks(t *testing.T) {
	t.Helper()
	res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f), Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermMakeMemberLinks, Role: schema.PermissionRoleMember, Granted: false}}})
	if res.code != http.StatusOK {
		t.Fatalf("PUT make_member_links=false: %d %s", res.code, res.body)
	}
}

// TestPersonalPresetApplies: with A4 the safety net lets the personal preset
// through, and it takes link-making away from members at once.
func TestPersonalPresetApplies(t *testing.T) {
	f := newRestrictedFixture(t, Config{AuthMode: AuthRequired})
	f.mayInvite(t, f.member, true)
	if rec := f.do(t, "POST", "/v1/invites", f.memberTok, `{"kind":"personal","channels":["channel-b"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("a member's link before the preset = %d %s", rec.Code, rec.Body)
	}
	res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f), Preset: schema.PresetPersonal})
	if res.code != http.StatusOK {
		t.Fatalf("PUT preset personal = %d %s, want 200", res.code, res.body)
	}
	if rec := f.do(t, "POST", "/v1/invites", f.memberTok, `{"kind":"personal","channels":["channel-b"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("a member's link after the preset = %d %s, want 403", rec.Code, rec.Body)
	}
}
