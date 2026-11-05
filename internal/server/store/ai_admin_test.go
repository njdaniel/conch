package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// AI administrators in the store (issue #1326, slice S2 #1329,
// docs/design/ai-administrators.md §3.1, §3.2, §3.5.4): an agent holds
// exactly the permissions its manifest grants, only an owner grants them,
// a granted agent has operator standing for what it holds and nothing more,
// and an agent's decision never counts.

type aiAdminFixture struct {
	s      *Store
	owner  Principal // the bootstrapped operator, who holds the owner mark
	op     Principal // an operator without the mark
	alice  Principal // a member
	agent  Principal
	admin  Channel // the agent's approval channel
	ops    Channel // a channel the agent is not in
	ownerA string
}

func newAIAdminFixture(t *testing.T) *aiAdminFixture {
	t.Helper()
	ctx := context.Background()
	f := &aiAdminFixture{s: openTestStore(t)}
	var err error
	if f.owner, _, _, err = f.s.BootstrapOperator(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	f.ownerA = principalActor(f.owner.ID)
	if f.op, err = f.s.CreatePrincipal(ctx, PrincipalHuman, "opal"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetRole(ctx, f.ownerA, f.owner.ID, f.op.ID, RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	if f.alice, err = f.s.CreatePrincipal(ctx, PrincipalHuman, "alice"); err != nil {
		t.Fatal(err)
	}
	if f.agent, err = f.s.CreatePrincipal(ctx, PrincipalAgent, "helper"); err != nil {
		t.Fatal(err)
	}
	if f.admin, err = f.s.CreateChannel(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if f.ops, err = f.s.CreateChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{f.owner.ID, f.agent.ID} {
		if _, err := f.s.AddChannelMember(ctx, "system", f.admin.ID, id, 0); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// manifest is a valid manifest for the agent, granting perms with the
// admin channel as its approval channel (nil perms: no grant).
func (f *aiAdminFixture) manifest(perms ...schema.Permission) schema.PutAgentManifestRequestV1 {
	req := schema.PutAgentManifestRequestV1{
		DisplayName:  "helper",
		Tier:         schema.AgentTierA,
		Capabilities: []schema.Capability{schema.CapabilityMessagesRead},
		Channels:     []schema.ChannelGrant{{ChannelID: f.admin.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}},
	}
	if len(perms) > 0 {
		req.Permissions = perms
		req.Admin = &schema.AgentAdminV1{ApprovalChannelID: f.admin.ID}
	}
	return req
}

func (f *aiAdminFixture) put(t *testing.T, actor string, perms ...schema.Permission) {
	t.Helper()
	if _, _, err := f.s.PutAgentManifest(context.Background(), actor, f.agent.ID, f.manifest(perms...)); err != nil {
		t.Fatalf("put manifest as %s granting %v: %v", actor, perms, err)
	}
}

func (f *aiAdminFixture) holds(t *testing.T, id int64, perm schema.Permission) bool {
	t.Helper()
	ok, err := f.s.Permitted(WithAdminDispatch(context.Background()), id, perm)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (f *aiAdminFixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	all, err := f.s.ListAuditEvents(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range all {
		if e.Action == action && e.Subject == principalActor(f.agent.ID) {
			n++
		}
	}
	return n
}

// TestAgentHoldsExactlyItsGrant is rule 1a: nothing without a grant, exactly
// the grant with one, nothing once disabled, nothing from a stored row that
// does not validate, and never act_as_members.
func TestAgentHoldsExactlyItsGrant(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, "system")
	for _, perm := range schema.Permissions {
		if f.holds(t, f.agent.ID, perm) {
			t.Errorf("an agent with no grant holds %s", perm)
		}
	}
	f.put(t, f.ownerA, schema.PermManageMembers, schema.PermMakeMemberLinks)
	for _, perm := range schema.Permissions {
		want := perm == schema.PermManageMembers || perm == schema.PermMakeMemberLinks
		if got := f.holds(t, f.agent.ID, perm); got != want {
			t.Errorf("granted manage_members and make_member_links: holds %s = %v, want %v", perm, got, want)
		}
	}
	held, _, err := f.s.EffectivePermissions(ctx, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 2 {
		t.Errorf("effective permissions = %v, want the two granted", held)
	}

	// A stored row naming a permission no agent may hold makes the
	// manifest invalid: the agent holds nothing at all, not just that one.
	if _, err := f.s.db.Exec(`UPDATE agent_manifests SET permissions = '["manage_members","act_as_members"]' WHERE principal_id = ?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	for _, perm := range []schema.Permission{schema.PermManageMembers, schema.PermActAsMembers} {
		if f.holds(t, f.agent.ID, perm) {
			t.Errorf("an agent whose stored grant does not validate holds %s", perm)
		}
	}
	f.put(t, f.ownerA, schema.PermManageMembers)
	if _, err := f.s.DisablePrincipal(ctx, f.ownerA, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if f.holds(t, f.agent.ID, schema.PermManageMembers) {
		t.Error("a disabled agent holds its grant")
	}
	if n := f.auditCount(t, AuditAIAdminRevoked); n != 1 {
		t.Errorf("ai_admin_revoked rows after disabling a granted agent = %d, want 1", n)
	}
}

// TestUndispatchedAgentHoldsNothing: a grant is authority only through the
// admin door (WithAdminDispatch). In any other context, such as a REST request the
// agent makes directly or an MCP tool, a granted agent holds nothing,
// so no gated action can run by a route that never asks for an approval;
// but a link it made through the door still stands behind its grant.
func TestUndispatchedAgentHoldsNothing(t *testing.T) {
	plain := context.Background()
	door := WithAdminDispatch(plain)
	f := newAIAdminFixture(t)
	f.put(t, f.ownerA, schema.PermManageMembers, schema.PermManageChannels, schema.PermMakeMemberLinks)
	for _, perm := range []schema.Permission{schema.PermManageMembers, schema.PermManageChannels, schema.PermMakeMemberLinks} {
		if ok, err := f.s.Permitted(plain, f.agent.ID, perm); err != nil || ok {
			t.Errorf("undispatched: Permitted(%s) = %v, %v; want false", perm, ok, err)
		}
		if ok, err := f.s.Permitted(door, f.agent.ID, perm); err != nil || !ok {
			t.Errorf("dispatched: Permitted(%s) = %v, %v; want true", perm, ok, err)
		}
	}
	if held, _, err := f.s.EffectivePermissions(plain, f.agent.ID); err != nil || len(held) != 0 {
		t.Errorf("undispatched effective permissions = %v, %v; want none", held, err)
	}
	if held, err := f.s.HeldBy(plain, f.agent, schema.PermManageMembers); err != nil || held[0] {
		t.Errorf("undispatched HeldBy = %v, %v; want false", held, err)
	}
	if _, err := f.s.PutChannelMember(plain, principalActor(f.agent.ID), f.ops.ID, f.alice.ID, f.agent.ID, nil, nil); !errors.Is(err, ErrManagerRefused) {
		t.Errorf("undispatched member add: err = %v, want ErrManagerRefused", err)
	}
	req := schema.CreateInviteRequestV2{Kind: schema.InviteKindMember}
	if _, err := f.s.CreateMemberLink(plain, principalActor(f.agent.ID), f.agent.ID, req, InviteOptions{}, time.Now()); !errors.Is(err, ErrInviteNotAllowed) {
		t.Errorf("undispatched member link: err = %v, want ErrInviteNotAllowed", err)
	}
	made, err := f.s.CreateMemberLink(door, principalActor(f.agent.ID), f.agent.ID, req, InviteOptions{}, time.Now())
	if err != nil {
		t.Fatalf("dispatched member link: %v", err)
	}
	// A joiner's preview is nobody's dispatch: the link stands on the grant.
	if _, err := f.s.PreviewMemberLink(plain, HashInviteCode(made.Code), time.Now()); err != nil {
		t.Errorf("preview of the agent's link in a plain context: %v", err)
	}
}

// TestOnlyAnOwnerGrants: a non-empty grant from a non-owner is ErrNotOwner
// and writes nothing; an owner's is audited; emptying it is anyone's the
// route admits; the approval channel must be granted read.
func TestOnlyAnOwnerGrants(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, "system")
	for _, actor := range []string{principalActor(f.op.ID), principalActor(f.alice.ID), principalActor(f.agent.ID), "principal:999999", "nonsense"} {
		_, _, err := f.s.PutAgentManifest(ctx, actor, f.agent.ID, f.manifest(schema.PermManageChannels))
		if !errors.Is(err, ErrNotOwner) {
			t.Errorf("grant by %s: err = %v, want ErrNotOwner", actor, err)
		}
	}
	if f.holds(t, f.agent.ID, schema.PermManageChannels) || f.auditCount(t, AuditAdminGrantChanged) != 0 {
		t.Fatal("a refused grant was written")
	}

	f.put(t, f.ownerA, schema.PermManageChannels)
	if n := f.auditCount(t, AuditAdminGrantChanged); n != 1 {
		t.Errorf("admin_grant_changed rows after the owner's grant = %d, want 1", n)
	}
	if n := f.auditCount(t, AuditAIAdminCreated); n != 1 {
		t.Errorf("ai_admin_created rows = %d, want 1", n)
	}
	// The same manifest again changes no grant and writes no grant row.
	f.put(t, f.ownerA, schema.PermManageChannels)
	if n := f.auditCount(t, AuditAdminGrantChanged); n != 1 {
		t.Errorf("admin_grant_changed rows after an unchanged write = %d, want 1", n)
	}

	// An operator without the mark empties it: revocation is not an
	// owner's act.
	f.put(t, principalActor(f.op.ID))
	if f.holds(t, f.agent.ID, schema.PermManageChannels) {
		t.Error("the agent still holds manage_channels after its grant was emptied")
	}
	if n := f.auditCount(t, AuditAIAdminRevoked); n != 1 {
		t.Errorf("ai_admin_revoked rows after the grant was emptied = %d, want 1", n)
	}

	// The approval channel must be one the manifest grants read.
	req := f.manifest(schema.PermManageChannels)
	req.Admin.ApprovalChannelID = f.ops.ID
	if _, _, err := f.s.PutAgentManifest(ctx, f.ownerA, f.agent.ID, req); !errors.Is(err, ErrAdminChannelNotGranted) {
		t.Errorf("approval channel not granted: err = %v, want ErrAdminChannelNotGranted", err)
	}
}

// TestGrantedAgentStanding: an agent granted manage_channels administers
// channel membership with operator standing; without the grant it is
// refused, even with a manages row (an agent is never a manager).
func TestGrantedAgentStanding(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, "system")
	// A manages row on the agent, written raw: no path may make it count.
	if _, err := f.s.db.Exec("UPDATE channel_members SET manages = 1 WHERE channel_id = ? AND principal_id = ?", f.admin.ID, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := memberActorStandingOf(t, f, f.ops.ID); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("ungranted agent's standing: err = %v, want ErrManagerRefused", err)
	}
	if _, err := memberActorStandingOf(t, f, f.admin.ID); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("ungranted agent with a manages row: err = %v, want ErrManagerRefused", err)
	}
	f.put(t, f.ownerA, schema.PermManageChannels)
	st, err := memberActorStandingOf(t, f, f.ops.ID)
	if err != nil || st != standingOperator {
		t.Fatalf("granted agent's standing in a channel it is not in = %v, %v; want operator standing", st, err)
	}
	if _, err := f.s.PutChannelMember(ctx, principalActor(f.agent.ID), f.ops.ID, f.alice.ID, f.agent.ID, nil, nil); err != nil {
		t.Fatalf("the granted agent adding alice to ops: %v", err)
	}
	// A grant of manage_members is not manage_channels.
	f.put(t, f.ownerA, schema.PermManageMembers)
	if _, err := memberActorStandingOf(t, f, f.ops.ID); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("agent granted manage_members only: err = %v, want ErrManagerRefused", err)
	}
	if _, err := f.s.PutChannelMember(ctx, principalActor(f.agent.ID), f.ops.ID, f.op.ID, f.agent.ID, nil, nil); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("agent granted manage_members only, adding opal to ops: err = %v, want ErrManagerRefused", err)
	}
}

func memberActorStandingOf(t *testing.T, f *aiAdminFixture, channelID int64) (memberActorStanding, error) {
	t.Helper()
	var st memberActorStanding
	ctx := WithAdminDispatch(context.Background())
	err := f.s.withImmediateTx(ctx, func(tx execer) error {
		var err error
		st, err = memberActorStandingTx(ctx, tx, channelID, f.agent.ID)
		return err
	})
	return st, err
}

// TestGrantedAgentMakesMemberLinks: make_member_links lets an agent make a
// member link, and the link stands behind the grant: emptying it suspends
// the link.
func TestGrantedAgentMakesMemberLinks(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, "system")
	req := schema.CreateInviteRequestV2{Kind: schema.InviteKindMember}
	if _, err := f.s.CreateMemberLink(ctx, principalActor(f.agent.ID), f.agent.ID, req, InviteOptions{}, time.Now()); !errors.Is(err, ErrInviteNotAllowed) {
		t.Fatalf("ungranted agent making a member link: err = %v, want ErrInviteNotAllowed", err)
	}
	f.put(t, f.ownerA, schema.PermMakeMemberLinks)
	made, err := f.s.CreateMemberLink(ctx, principalActor(f.agent.ID), f.agent.ID, req, InviteOptions{}, time.Now())
	if err != nil {
		t.Fatalf("granted agent making a member link: %v", err)
	}
	hash := HashInviteCode(made.Code)
	if _, err := f.s.PreviewMemberLink(ctx, hash, time.Now()); err != nil {
		t.Fatalf("preview of the agent's link: %v", err)
	}
	f.put(t, principalActor(f.op.ID))
	if _, err := f.s.PreviewMemberLink(ctx, hash, time.Now()); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("preview after the grant was emptied: err = %v, want ErrInviteInvalid", err)
	}
}

// TestAgentDecisionNeverCounts is wall 2 of ai-administrators §3.5.4: an
// agent cannot decide an approval, and a decision row of an agent, however
// written, does not count toward the quorum.
func TestAgentDecisionNeverCounts(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, f.ownerA, schema.PermManageMembers)
	// can_decide on the agent's membership, written raw: the deciding
	// transaction must still refuse it.
	if _, err := f.s.db.Exec("UPDATE channel_members SET can_decide = 1 WHERE channel_id = ? AND principal_id IN (?, ?)", f.admin.ID, f.agent.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a, err := f.s.CreateApproval(ctx, ApprovalParams{
		RequesterID: f.agent.ID, ChannelID: f.admin.ID, Title: "disable alice", Body: "b",
		Options: []schema.Option{
			{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
		},
		Deadline: now.Add(time.Hour), GraceDeadline: now.Add(2 * time.Hour), Quorum: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.CastMemberDecision(ctx, a.ID, f.agent.ID, "approve", "self"); !errors.Is(err, ErrNoDecideRight) {
		t.Fatalf("the agent deciding its own approval: err = %v, want ErrNoDecideRight", err)
	}
	// A decision row of the agent's, written raw, must not count either.
	if _, err := f.s.db.Exec("INSERT INTO approval_decisions (approval_id, principal_id, option_id, reason, created_at) VALUES (?, ?, 'approve', 'raw', ?)", a.ID, f.agent.ID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_, res, err := f.s.CastMemberDecision(ctx, a.ID, f.owner.ID, "approve", "ok")
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Fatalf("quorum 2 resolved with one human decision and one agent row: %+v", res)
	}
}

// TestMigration43LeavesEveryAgentUngranted: existing manifests read with
// no grant after the migration, so nothing changes on upgrade.
func TestMigration43LeavesEveryAgentUngranted(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, "system")
	m, err := f.s.AgentManifestByPrincipal(ctx, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Permissions) != 0 || m.Admin != nil {
		t.Errorf("a manifest written without a grant reads back with %v, %+v", m.Permissions, m.Admin)
	}
	var perms, admin string
	if err := f.s.db.QueryRow("SELECT permissions, admin FROM agent_manifests WHERE principal_id = ?", f.agent.ID).Scan(&perms, &admin); err != nil {
		t.Fatal(err)
	}
	if perms != "[]" || admin != "" {
		t.Errorf("stored grant columns = %q, %q; want the defaults", perms, admin)
	}
	if !strings.Contains(strings.Join(tableColumns(t, f.s, "admin_actions"), ","), "claimed_at") {
		t.Error("admin_actions has no claimed_at column")
	}
}

func tableColumns(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestPrincipalHoldsAgentWall is rule 1a on its own: whatever bits an
// agent's grant carries and whatever role or owner mark its row says, it
// never holds a permission no agent may hold, and holds nothing when
// disabled, hidden or a guest.
func TestPrincipalHoldsAgentWall(t *testing.T) {
	now := time.Now()
	all := ^uint64(0)
	for _, tc := range []struct {
		name string
		p    Principal
		perm schema.Permission
		want bool
	}{
		{"granted bit", Principal{Kind: PrincipalAgent, agentGrant: agentGrantBit(schema.PermManageMembers)}, schema.PermManageMembers, true},
		{"other bit", Principal{Kind: PrincipalAgent, agentGrant: agentGrantBit(schema.PermManageMembers)}, schema.PermManageChannels, false},
		{"no grant loaded", Principal{Kind: PrincipalAgent}, schema.PermManageMembers, false},
		{"act_as_members with every bit", Principal{Kind: PrincipalAgent, agentGrant: all}, schema.PermActAsMembers, false},
		{"owner mark on an agent", Principal{Kind: PrincipalAgent, Role: RoleOperator, Owner: true}, schema.PermManageMembers, false},
		{"disabled", Principal{Kind: PrincipalAgent, agentGrant: all, DisabledAt: &now}, schema.PermManageMembers, false},
		{"hidden", Principal{Kind: PrincipalAgent, agentGrant: all, HiddenAt: &now}, schema.PermManageMembers, false},
		{"guest", Principal{Kind: PrincipalAgent, agentGrant: all, GuestState: schema.GuestStateActive}, schema.PermManageMembers, false},
		{"grant bits on an unknown kind", Principal{Kind: PrincipalKind("bot"), agentGrant: all}, schema.PermManageMembers, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := principalHolds(tc.p, permissionDoc{}, tc.perm); got != tc.want {
				t.Errorf("principalHolds(%s) = %v, want %v", tc.perm, got, tc.want)
			}
		})
	}
}

// TestAgentApprovalChannelProtected is wall 3 of ai-administrators §3.5.4
// in the store (§3.5.6): an agent granted manage_channels shapes every
// channel but its own approval channel, whatever path it takes; and it
// never seats a decider by adding a member.
func TestAgentApprovalChannelProtected(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, f.ownerA, schema.PermManageChannels)
	agentA := principalActor(f.agent.ID)
	yes, no := true, false
	opt := ChannelRenameOptions{CallerID: f.agent.ID, Limits: RenameLimits{Renames: 5, Attempts: 20}}
	private := schema.ChannelVisibility("private")
	refused := []struct {
		name string
		run  func() error
		want error
	}{
		{"add a member", func() error {
			_, err := f.s.PutChannelMember(ctx, agentA, f.admin.ID, f.alice.ID, f.agent.ID, nil, nil)
			return err
		}, ErrManagerRefused},
		{"add a member if absent", func() error {
			_, _, err := f.s.AddChannelMemberIfAbsent(ctx, agentA, f.admin.ID, f.agent.ID, MemberTarget{ID: f.alice.ID}, nil)
			return err
		}, ErrManagerRefused},
		{"take the owner's right to decide", func() error {
			_, err := f.s.SetChannelMemberCanDecide(ctx, agentA, f.admin.ID, f.owner.ID, false, f.agent.ID)
			return err
		}, ErrManagerRefused},
		{"remove the owner", func() error {
			_, err := f.s.RemoveChannelMember(ctx, agentA, f.admin.ID, f.owner.ID, f.agent.ID)
			return err
		}, ErrManagerRefused},
		{"rename it", func() error {
			_, err := f.s.RenameChannel(ctx, agentA, f.admin.ID, "admin", "renamed", time.Now(), opt)
			return err
		}, ErrManagerRefused},
		{"change its settings", func() error {
			_, err := f.s.ChannelSettings(ctx, agentA, f.agent.ID, f.admin.ID, ChannelSettingsChange{Visibility: &private}, time.Now())
			return err
		}, ErrManageChannelsRequired},
		{"seat a decider elsewhere by adding one", func() error {
			_, _, err := f.s.AddChannelMemberIfAbsent(ctx, agentA, f.ops.ID, f.agent.ID, MemberTarget{ID: f.op.ID}, &yes)
			return err
		}, ErrManagerRefused},
	}
	for _, tc := range refused {
		if err := tc.run(); !errors.Is(err, tc.want) {
			t.Errorf("%s in the approval channel: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if members, err := f.s.ListChannelMembers(context.Background(), f.admin.ID); err != nil || len(members) != 2 {
		t.Errorf("approval channel members = %+v, %v; want the owner and the agent only", members, err)
	}
	if ok, err := f.s.CanDecideApprovals(context.Background(), f.admin.ID, f.owner.ID); err != nil || !ok {
		t.Errorf("the owner's right to decide = %v, %v; want kept", ok, err)
	}

	// Elsewhere the agent acts with operator standing, and the members it
	// adds do not decide unless a person approved saying so.
	if _, err := f.s.PutChannelMember(ctx, agentA, f.ops.ID, f.alice.ID, f.agent.ID, nil, nil); err != nil {
		t.Fatalf("agent adding alice to ops: %v", err)
	}
	if ok, err := f.s.CanDecideApprovals(context.Background(), f.ops.ID, f.alice.ID); err != nil || ok {
		t.Errorf("alice, added by the agent, decides in ops = %v, %v; want false", ok, err)
	}
	if _, _, err := f.s.AddChannelMemberIfAbsent(ctx, agentA, f.ops.ID, f.agent.ID, MemberTarget{ID: f.op.ID}, &no); err != nil {
		t.Errorf("agent adding opal to ops without the right: %v", err)
	}
	if _, err := f.s.RenameChannel(ctx, agentA, f.ops.ID, "ops", "ops2", time.Now(), opt); err != nil {
		t.Errorf("agent renaming ops: %v", err)
	}
	if _, err := f.s.ChannelSettings(ctx, agentA, f.agent.ID, f.ops.ID, ChannelSettingsChange{Visibility: &private}, time.Now()); err != nil {
		t.Errorf("agent changing ops' settings: %v", err)
	}
}

// TestDisablingAnAIAdminRevokesTheGrant: disabling clears the grant, so
// enabling the agent again gives it nothing back.
func TestDisablingAnAIAdminRevokesTheGrant(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, f.ownerA, schema.PermManageMembers)
	if _, err := f.s.DisablePrincipal(ctx, principalActor(f.op.ID), f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EnablePrincipal(ctx, principalActor(f.op.ID), f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if f.holds(t, f.agent.ID, schema.PermManageMembers) {
		t.Error("an AI administrator disabled and enabled again holds its old grant")
	}
	m, err := f.s.AgentManifestByPrincipal(ctx, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Permissions) != 0 || m.Admin != nil {
		t.Errorf("manifest after the disable = %v, %+v; want no grant", m.Permissions, m.Admin)
	}
	var detail string
	for _, e := range mustAudit(t, f) {
		if e.Action == AuditAdminGrantChanged && e.Subject == principalActor(f.agent.ID) {
			detail = e.Detail
		}
	}
	if !strings.Contains(detail, `->[]`) || !strings.Contains(detail, "reason=disabled") {
		t.Errorf("last admin_grant_changed detail = %q; want the grant cleared by the disable", detail)
	}
}

// TestOnlyAPersonWritesAManifest: an agent's actor never writes a
// manifest, its own or another's, even one with no grant.
func TestOnlyAPersonWritesAManifest(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, f.ownerA, schema.PermManageMembers)
	for _, actor := range []string{principalActor(f.agent.ID), "principal:999999", "nonsense"} {
		if _, _, err := f.s.PutAgentManifest(ctx, actor, f.agent.ID, f.manifest()); !errors.Is(err, ErrManifestActorNotHuman) {
			t.Errorf("manifest write by %s: err = %v, want ErrManifestActorNotHuman", actor, err)
		}
	}
	if !f.holds(t, f.agent.ID, schema.PermManageMembers) {
		t.Error("a refused write changed the grant")
	}
	f.put(t, principalActor(f.op.ID))
	f.put(t, "system")
}

func mustAudit(t *testing.T, f *aiAdminFixture) []AuditEvent {
	t.Helper()
	all, err := f.s.ListAuditEvents(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return all
}
