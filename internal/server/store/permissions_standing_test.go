package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #1014 (ADR-014 A3a, docs/design/admin-roles.md §7.2.1, §7.3): the
// store decides an actor's standing, and who stands behind the bare
// address's channels, by permission and never by role.

// standingFixture is a store with an owner, an operator (operator-1) and a
// member (member-1); channel-a, which only the owner is in; and channel-b,
// which operator-1 and member-1 both manage.
type standingFixture struct {
	s                 *Store
	ctx               context.Context
	owner, op, member Principal
	plain             Principal
	chanA, chanB      Channel
}

func newStandingFixture(t *testing.T) *standingFixture {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	f := &standingFixture{s: s, ctx: ctx}
	var err error
	if f.owner, _, _, err = s.BootstrapOperator(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dst  *Principal
		name string
	}{{&f.op, "operator-1"}, {&f.member, "member-1"}, {&f.plain, "member-2"}} {
		if *c.dst, err = s.CreatePrincipal(ctx, PrincipalHuman, c.name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetRole(ctx, "system", f.owner.ID, f.op.ID, RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	if f.chanA, err = s.CreateChannelAs(ctx, "system", "channel-a", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if f.chanB, err = s.CreateChannel(ctx, "channel-b"); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	for _, p := range []Principal{f.op, f.member} {
		if _, err := s.PutChannelMember(ctx, "system", f.chanB.ID, p.ID, 0, &no, &yes); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddChannelMember(ctx, "system", f.chanB.ID, f.plain.ID, 0); err != nil {
		t.Fatal(err)
	}
	return f
}

// setOperatorCells sets operator cells, as the owner.
func (f *standingFixture) setOperatorCells(t *testing.T, granted bool, perms ...schema.Permission) {
	t.Helper()
	doc, err := f.s.Permissions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision}
	for _, p := range perms {
		req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleOperator, Granted: granted})
	}
	if _, err := f.s.PutPermissions(f.ctx, "system", f.owner.ID, req, time.Now()); err != nil {
		t.Fatalf("set operator cells %v %t: %v", perms, granted, err)
	}
}

// TestStandingReadsManageChannels pins the five-step rule of §7.2.1 in both
// standing functions, under the default document and with manage_channels
// taken from operators.
func TestStandingReadsManageChannels(t *testing.T) {
	f := newStandingFixture(t)
	type row struct {
		name    string
		actor   func() int64
		channel func() int64
		want    memberActorStanding // 0: refused
	}
	rows := func(restricted bool) []row {
		opInB := standingOperator
		opInA := standingOperator
		if restricted {
			opInB, opInA = standingManager, 0
		}
		return []row{
			{"actor 0", func() int64 { return 0 }, func() int64 { return f.chanB.ID }, standingOwner},
			{"owner", func() int64 { return f.owner.ID }, func() int64 { return f.chanB.ID }, standingOwner},
			{"operator, a channel it manages", func() int64 { return f.op.ID }, func() int64 { return f.chanB.ID }, opInB},
			{"operator, a channel it is not in", func() int64 { return f.op.ID }, func() int64 { return f.chanA.ID }, opInA},
			{"member manager", func() int64 { return f.member.ID }, func() int64 { return f.chanB.ID }, standingManager},
			{"member, no manages", func() int64 { return f.plain.ID }, func() int64 { return f.chanB.ID }, 0},
			{"member, a channel it is not in", func() int64 { return f.member.ID }, func() int64 { return f.chanA.ID }, 0},
		}
	}
	check := func(t *testing.T, restricted bool) {
		for _, tt := range rows(restricted) {
			t.Run(tt.name, func(t *testing.T) {
				got, err := memberActorStandingTx(f.ctx, f.s.db, tt.channel(), tt.actor())
				if tt.want == 0 {
					if !errors.Is(err, ErrManagerRefused) {
						t.Fatalf("memberActorStandingTx = %v, %v; want refused", got, err)
					}
				} else if err != nil || got != tt.want {
					t.Fatalf("memberActorStandingTx = %v, %v; want %v", got, err, tt.want)
				}
				if tt.actor() == 0 {
					return // the rename twin has no actor 0: rename always has a caller or none
				}
				got, err = channelRenameStandingTx(f.ctx, f.s.db, tt.channel(), tt.actor())
				if tt.want == 0 {
					// The twin hides a channel the caller is not in (404), and
					// refuses a member without manages (403).
					if !errors.Is(err, ErrManagerRefused) && !errors.Is(err, ErrNotFound) {
						t.Fatalf("channelRenameStandingTx = %v, %v; want refused or not found", got, err)
					}
				} else if err != nil || got != tt.want {
					t.Fatalf("channelRenameStandingTx = %v, %v; want %v", got, err, tt.want)
				}
			})
		}
	}
	t.Run("defaults", func(t *testing.T) { check(t, false) })
	f.setOperatorCells(t, false, schema.PermManageChannels)
	t.Run("manage_channels off for operators", func(t *testing.T) { check(t, true) })
}

// TestRestrictedOperatorWritesAsAManager: the store's membership writes
// refuse a restricted operator what they refuse a member-manager, whatever
// the handler asked.
func TestRestrictedOperatorWritesAsAManager(t *testing.T) {
	f := newStandingFixture(t)
	f.setOperatorCells(t, false, schema.PermManageChannels)
	actor := fmt.Sprintf("principal:%d", f.op.ID)
	yes := true
	if _, err := f.s.SetChannelMemberCanDecide(f.ctx, actor, f.chanB.ID, f.plain.ID, true, f.op.ID); !errors.Is(err, ErrManagerRefused) {
		t.Errorf("SetChannelMemberCanDecide = %v, want refused", err)
	}
	if _, err := f.s.SetChannelMemberManages(f.ctx, actor, f.chanB.ID, f.plain.ID, true, f.op.ID); !errors.Is(err, ErrManagerRefused) {
		t.Errorf("SetChannelMemberManages = %v, want refused", err)
	}
	if _, err := f.s.putChannelMember(f.ctx, actor, f.op.ID, f.chanB.ID, f.plain.ID, f.op.ID, &yes, nil); !errors.Is(err, ErrManagerRefused) {
		t.Errorf("putChannelMember with can_decide = %v, want refused", err)
	}
	// member-1 becomes an approver, by the owner; a manager may not remove one.
	if _, err := f.s.SetChannelMemberCanDecide(f.ctx, "system", f.chanB.ID, f.member.ID, true, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RemoveChannelMember(f.ctx, actor, f.chanB.ID, f.member.ID, f.op.ID); !errors.Is(err, ErrManagerRefused) {
		t.Errorf("removing an approver = %v, want refused", err)
	}
	// Under the defaults the same operator may do each.
	f.setOperatorCells(t, true, schema.PermManageChannels)
	if _, err := f.s.SetChannelMemberManages(f.ctx, actor, f.chanB.ID, f.plain.ID, true, f.op.ID); err != nil {
		t.Errorf("SetChannelMemberManages by an operator holding manage_channels = %v", err)
	}
	if _, err := f.s.RemoveChannelMember(f.ctx, actor, f.chanB.ID, f.member.ID, f.op.ID); err != nil {
		t.Errorf("removing an approver by an operator holding manage_channels = %v", err)
	}
}

// joinFixture is a standingFixture whose bare address offers accounts, with
// change_server_settings granted to operators.
func newJoinFixture(t *testing.T) *standingFixture {
	t.Helper()
	f := newStandingFixture(t)
	f.setOperatorCells(t, true, schema.PermChangeServerSettings)
	return f
}

func (f *standingFixture) putJoin(actorID int64, req schema.PutJoinSettingsRequestV1) error {
	_, err := f.s.PutJoinSettings(f.ctx, "system", actorID, req)
	return err
}

func (f *standingFixture) openChannelListers(t *testing.T) map[int64]sql.NullInt64 {
	t.Helper()
	// The open channels are the default channels, and their lister is
	// channels.default_listed_by, since migration 40 (issue #979).
	rows, err := f.s.db.QueryContext(f.ctx, "SELECT id, default_listed_by FROM channels WHERE is_default = 1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]sql.NullInt64{}
	for rows.Next() {
		var id int64
		var by sql.NullInt64
		if err := rows.Scan(&id, &by); err != nil {
			t.Fatal(err)
		}
		out[id] = by
	}
	return out
}

// TestPutJoinSettingsChecksTheResultingState: a write that leaves the address
// admitting people needs manage_channels for every listed channel the
// writer is not in, whether or not the write touched the list (SF-2).
func TestPutJoinSettingsChecksTheResultingState(t *testing.T) {
	account := ptrMode(schema.OpenJoiningAccount)
	off := ptrMode(schema.OpenJoiningOff)
	isDenied := func(err error) bool {
		var pe *PermissionError
		return errors.As(err, &pe) && errors.Is(err, ErrPermissionDenied) && pe.Permission == schema.PermManageChannels
	}

	t.Run("defaults", func(t *testing.T) {
		f := newJoinFixture(t)
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenJoining: account, OpenChannels: &[]string{"channel-a"}}); err != nil {
			t.Fatalf("an operator holding manage_channels lists channel-a: %v", err)
		}
	})

	f := newJoinFixture(t)
	// The owner lists channel-a with the address off.
	if err := f.putJoin(f.owner.ID, schema.PutJoinSettingsRequestV1{OpenJoining: off, OpenChannels: &[]string{"channel-a"}}); err != nil {
		t.Fatal(err)
	}
	f.setOperatorCells(t, false, schema.PermManageChannels)
	t.Run("naming channel-a", func(t *testing.T) {
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenJoining: account, OpenChannels: &[]string{"channel-a", "channel-b"}}); !isDenied(err) {
			t.Fatalf("err = %v, want permission_denied naming manage_channels", err)
		}
	})
	t.Run("opening over a list it did not touch", func(t *testing.T) {
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenJoining: account}); !isDenied(err) {
			t.Fatalf("err = %v, want permission_denied naming manage_channels", err)
		}
		set, _, err := f.s.JoinSettings(f.ctx)
		if err != nil || set.OpenJoining != schema.OpenJoiningOff {
			t.Fatalf("after the refusal the mode is %q, %v; want off, unwritten", set.OpenJoining, err)
		}
	})
	t.Run("a write that leaves the address closed", func(t *testing.T) {
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{JoinVoice: boolRef(true)}); err != nil {
			t.Fatalf("a write that admits nobody: %v", err)
		}
	})
	t.Run("a paused address admits nobody", func(t *testing.T) {
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenJoining: account, JoiningPaused: boolRef(true)}); err != nil {
			t.Fatalf("opening a paused address: %v", err)
		}
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{JoiningPaused: boolRef(false)}); !isDenied(err) {
			t.Fatalf("unpausing over channel-a = %v, want permission_denied", err)
		}
	})
	t.Run("its own channels only", func(t *testing.T) {
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"channel-b"}, JoiningPaused: boolRef(false)}); err != nil {
			t.Fatalf("opening over channel-b, which it is in: %v", err)
		}
	})
}

// TestNoChannelSlippedInWhilePaused: a writer without manage_channels cannot
// list a channel it is not in while the address admits nobody, so an
// owner's later unpause, which becomes the lister of every listed channel,
// never vouches for one it did not choose (security SF-1).
func TestNoChannelSlippedInWhilePaused(t *testing.T) {
	f := newJoinFixture(t)
	account := ptrMode(schema.OpenJoiningAccount)
	if err := f.putJoin(f.owner.ID, schema.PutJoinSettingsRequestV1{OpenJoining: account, JoiningPaused: boolRef(true),
		OpenChannels: &[]string{"channel-b"}}); err != nil {
		t.Fatal(err)
	}
	f.setOperatorCells(t, false, schema.PermManageChannels)
	var pe *PermissionError
	for _, mode := range []*schema.OpenJoiningMode{nil, ptrMode(schema.OpenJoiningOff)} {
		err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenJoining: mode, OpenChannels: &[]string{"channel-a", "channel-b"}})
		if !errors.As(err, &pe) || pe.Permission != schema.PermManageChannels {
			t.Fatalf("operator-1 slips channel-a into a list that admits nobody (mode %v) = %v, want permission_denied", mode, err)
		}
	}
	// A channel it is in, it may list while paused.
	if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"channel-b"}}); err != nil {
		t.Fatalf("operator-1 keeps channel-b: %v", err)
	}
	// The owner unpauses.
	if err := f.putJoin(f.owner.ID, schema.PutJoinSettingsRequestV1{JoiningPaused: boolRef(false)}); err != nil {
		t.Fatal(err)
	}
	listers := f.openChannelListers(t)
	if _, listed := listers[f.chanA.ID]; listed || len(listers) != 1 {
		t.Fatalf("open channels after the owner's unpause = %v, want channel-b only", listers)
	}
	got, err := f.s.JoinAsAccount(f.ctx, "joiner-1", testRecord("j"), time.Now())
	if err != nil || len(got.Channels) != 1 || got.Channels[0].ID != f.chanB.ID {
		t.Fatalf("join = %+v, %v; want channel-b only", got.Channels, err)
	}
}

// TestOwnerWriteRefreshesListers: an owner's write makes it the lister of
// every channel it leaves listed; another's write does not.
func TestOwnerWriteRefreshesListers(t *testing.T) {
	f := newJoinFixture(t)
	if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"channel-b"}}); err != nil {
		t.Fatal(err)
	}
	if got := f.openChannelListers(t)[f.chanB.ID]; got.Int64 != f.op.ID {
		t.Fatalf("channel-b listed by %v, want operator-1", got)
	}
	if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{JoinVoice: boolRef(true)}); err != nil {
		t.Fatal(err)
	}
	if got := f.openChannelListers(t)[f.chanB.ID]; got.Int64 != f.op.ID {
		t.Fatalf("after an operator's write channel-b is listed by %v, want operator-1 still", got)
	}
	if err := f.putJoin(f.owner.ID, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"channel-a", "channel-b"}}); err != nil {
		t.Fatal(err)
	}
	for id, by := range f.openChannelListers(t) {
		if !by.Valid || by.Int64 != f.owner.ID {
			t.Errorf("after the owner's write channel %d is listed by %v, want the owner", id, by)
		}
	}
}

// TestJoinAsAccountKeepsOnlyVouchedChannels: an address join lands only in
// channels whose lister is an owner or holds manage_channels now; a NULL
// lister is an owner's; with none left the address is closed (SF-2).
func TestJoinAsAccountKeepsOnlyVouchedChannels(t *testing.T) {
	now := time.Now()
	t.Run("the lister lost manage_channels", func(t *testing.T) {
		f := newJoinFixture(t)
		if err := f.putJoin(f.owner.ID, schema.PutJoinSettingsRequestV1{OpenJoining: ptrMode(schema.OpenJoiningAccount), OpenChannels: &[]string{"channel-a"}}); err != nil {
			t.Fatal(err)
		}
		// operator-1 lists channel-b too, while it holds manage_channels.
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"channel-a", "channel-b"}}); err != nil {
			t.Fatal(err)
		}
		f.setOperatorCells(t, false, schema.PermManageChannels)
		got, err := f.s.JoinAsAccount(f.ctx, "joiner-1", testRecord("j"), now)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Channels) != 1 || got.Channels[0].ID != f.chanA.ID {
			t.Fatalf("joined %+v, want channel-a only", got.Channels)
		}
		if in, err := f.s.IsChannelMember(f.ctx, f.chanB.ID, got.Principal.ID); err != nil || in {
			t.Fatalf("joiner in channel-b = %v, %v; want not", in, err)
		}
		events, err := f.s.ListAuditEvents(f.ctx, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		dropped := 0
		for _, e := range events {
			if e.Action == AuditJoinChannelDropped {
				dropped++
				if e.Actor != "system" || e.Subject != channelSubject(f.chanB.ID) || e.Detail != "reason=permission" {
					t.Errorf("join_channel_dropped row = %+v", e)
				}
			}
		}
		if dropped != 1 {
			t.Errorf("join_channel_dropped rows = %d, want 1", dropped)
		}
		// Back under the defaults, the next join lands in both.
		f.setOperatorCells(t, true, schema.PermManageChannels)
		got, err = f.s.JoinAsAccount(f.ctx, "joiner-2", testRecord("j"), now)
		if err != nil || len(got.Channels) != 2 {
			t.Fatalf("join after manage_channels returned = %+v, %v; want both channels", got.Channels, err)
		}
	})
	t.Run("a NULL lister is an owner's", func(t *testing.T) {
		f := newJoinFixture(t)
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenJoining: ptrMode(schema.OpenJoiningAccount), OpenChannels: &[]string{"channel-b"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.ExecContext(f.ctx, "UPDATE channels SET default_listed_by = NULL"); err != nil {
			t.Fatal(err)
		}
		f.setOperatorCells(t, false, schema.PermManageChannels)
		got, err := f.s.JoinAsAccount(f.ctx, "joiner-1", testRecord("j"), now)
		if err != nil || len(got.Channels) != 1 || got.Channels[0].ID != f.chanB.ID {
			t.Fatalf("join = %+v, %v; want channel-b", got.Channels, err)
		}
	})
	t.Run("every channel dropped", func(t *testing.T) {
		f := newJoinFixture(t)
		if err := f.putJoin(f.op.ID, schema.PutJoinSettingsRequestV1{OpenJoining: ptrMode(schema.OpenJoiningAccount), OpenChannels: &[]string{"channel-a"}}); err != nil {
			t.Fatal(err)
		}
		f.setOperatorCells(t, false, schema.PermManageChannels)
		if _, err := f.s.JoinAsAccount(f.ctx, "joiner-1", testRecord("j"), now); !errors.Is(err, ErrJoiningClosed) {
			t.Fatalf("join = %v, want the closed address's answer", err)
		}
		var n int
		if err := f.s.db.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM principals WHERE name = 'joiner-1'").Scan(&n); err != nil || n != 0 {
			t.Fatalf("a refused join left %d principals, %v", n, err)
		}
	})
}

// TestMayInviteBrakeIsNotThePermission: turning an operator's may_invite off
// while the document has switched make_member_links off for operators must
// not close the operator's invites. The brake does not bind operators
// (design §5); the document's switch only suspends them, and they work
// again when it comes back. A member's invites still close for good.
func TestMayInviteBrakeIsNotThePermission(t *testing.T) {
	f := newStandingFixture(t)
	now := time.Now()
	personal := func(maker Principal) string {
		t.Helper()
		resp, err := f.s.CreateInvite(f.ctx, "system", maker.ID,
			schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-b"}}, InviteOptions{}, now)
		if err != nil {
			t.Fatalf("invite by %s: %v", maker.Name, err)
		}
		return HashInviteCode(resp.Code)
	}
	if _, err := f.s.SetMayInvite(f.ctx, "system", f.member.ID, true); err != nil {
		t.Fatal(err)
	}
	opInvite := personal(f.op)
	memberInvite := personal(f.member)

	// make_member_links off for both columns (a member cell may not exceed
	// the operator's), then back on.
	links := func(granted bool) {
		t.Helper()
		doc, err := f.s.Permissions(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		req := schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: []schema.PermissionChangeV1{
			{Permission: schema.PermMakeMemberLinks, Role: schema.PermissionRoleMember, Granted: granted},
			{Permission: schema.PermMakeMemberLinks, Role: schema.PermissionRoleOperator, Granted: granted},
		}}
		if _, err := f.s.PutPermissions(f.ctx, "system", f.owner.ID, req, time.Now()); err != nil {
			t.Fatalf("make_member_links %t: %v", granted, err)
		}
	}
	links(false)
	if _, err := f.s.PreviewInvite(f.ctx, opInvite, now); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("operator's invite with make_member_links off = %v, want suspended", err)
	}
	for _, may := range []bool{true, false} {
		if _, err := f.s.SetMayInvite(f.ctx, "system", f.op.ID, may); err != nil {
			t.Fatal(err)
		}
	}
	links(true)
	if _, err := f.s.PreviewInvite(f.ctx, opInvite, now); err != nil {
		t.Fatalf("operator's invite after make_member_links came back = %v, want usable: the brake closed it", err)
	}

	if _, err := f.s.SetMayInvite(f.ctx, "system", f.member.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetMayInvite(f.ctx, "system", f.member.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PreviewInvite(f.ctx, memberInvite, now); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("member's invite after its brake was applied = %v, want closed for good", err)
	}
}
