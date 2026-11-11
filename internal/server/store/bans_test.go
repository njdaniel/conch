package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// banFixture is a channel "ops" with every kind of actor and target the ban
// rules distinguish (docs/design/moderation.md §6.1):
//
//	root     the owner            opr      a non-owner operator
//	owner2   a second owner       mgr      a manager of ops
//	mgr2     another manager      plain    a member without standing
//	appr     a member with can_decide
//	robo     an agent member      guest    an active guest member
//	oprMem   an operator who is a member
//	away     a human in no channel; ghost does not exist
type banFixture struct {
	t   *testing.T
	s   *Store
	ctx context.Context
	ch  Channel
	ids map[string]int64
}

func newBanFixture(t *testing.T) *banFixture {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	f := &banFixture{t: t, s: s, ctx: ctx, ids: map[string]int64{"ghost": 987654}}
	root, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	f.ids["root"] = root.ID
	if f.ch, err = s.CreateChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	mk := func(kind PrincipalKind, name string) int64 {
		p, err := s.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		f.ids[name] = p.ID
		return p.ID
	}
	op := func(name string) int64 {
		id := mk(PrincipalHuman, name)
		if _, err := s.db.Exec("UPDATE principals SET role = 'operator' WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	op("opr")
	owner2 := op("owner2")
	promoteToOwner(t, s, root.ID, owner2)
	op("oprMem")
	for _, name := range []string{"mgr", "mgr2", "plain", "appr", "away"} {
		mk(PrincipalHuman, name)
	}
	mk(PrincipalAgent, "robo")
	for _, name := range []string{"owner2", "oprMem", "mgr", "mgr2", "plain", "appr", "robo"} {
		if _, err := s.AddChannelMember(ctx, "system", f.ch.ID, f.ids[name], 0); err != nil {
			t.Fatal(err)
		}
	}
	no := false
	for _, name := range []string{"owner2", "oprMem", "mgr", "mgr2", "plain", "robo"} {
		if _, err := s.SetChannelMemberCanDecide(ctx, "system", f.ch.ID, f.ids[name], no); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"mgr", "mgr2"} {
		if _, err := s.SetChannelMemberManages(ctx, "system", f.ch.ID, f.ids[name], true); err != nil {
			t.Fatal(err)
		}
	}
	g, err := s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.ch.ID}, Idle: time.Hour, Absolute: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	f.ids["guest"] = g.Principal.ID
	return f
}

func (f *banFixture) member(name string) bool {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM channel_members WHERE channel_id = ? AND principal_id = ?", f.ch.ID, f.ids[name]).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

func (f *banFixture) banned(name string) bool {
	f.t.Helper()
	ok, err := f.s.IsChannelBanned(f.ctx, f.ch.ID, f.ids[name])
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *banFixture) banRow(by, name string) {
	f.t.Helper()
	var creator any
	if by != "" {
		creator = f.ids[by]
	}
	if _, err := f.s.db.Exec("INSERT INTO channel_bans (channel_id, principal_id, created_by, created_at) VALUES (?, ?, ?, 1)",
		f.ch.ID, f.ids[name], creator); err != nil {
		f.t.Fatal(err)
	}
}

func (f *banFixture) audits(action string) []AuditEvent {
	f.t.Helper()
	evs, err := f.s.ListAuditEvents(f.ctx, 0, 100000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []AuditEvent
	for _, e := range evs {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func (f *banFixture) ban(actor, target string) (BanResult, error) {
	return f.s.BanChannelMember(f.ctx, principalActor(f.ids[actor]), f.ch.ID, f.ids[target], f.ids[actor])
}

func (f *banFixture) unban(actor, target string) (BanResult, error) {
	return f.s.UnbanChannelMember(f.ctx, principalActor(f.ids[actor]), f.ch.ID, f.ids[target], f.ids[actor])
}

// TestBanActorTargetMatrix: actor x target x current-member, with the ban
// that already stands (design §6.1, §9 cases 2 and 5). A refusal writes
// nothing; a ban removes the membership in the same transaction.
func TestBanActorTargetMatrix(t *testing.T) {
	var (
		forbidden = ErrManagerRefused
		owned     = ErrOwnerProtected
		notFound  = ErrPrincipalNotFound
	)
	tests := []struct {
		name     string
		actor    string
		target   string
		existing [2]string // by, target of a ban row that stands before the call
		want     error
	}{
		// An owner bans any existing principal, member or not.
		{"owner bans a member", "root", "plain", [2]string{}, nil},
		{"owner bans an approver", "root", "appr", [2]string{}, nil},
		{"owner bans a non-member (pre-ban)", "root", "away", [2]string{}, nil},
		{"owner bans another owner", "root", "owner2", [2]string{}, nil},
		{"owner bans an unknown principal", "root", "ghost", [2]string{}, notFound},
		// A non-owner operator is stopped by owner protection only.
		{"operator bans a member", "opr", "plain", [2]string{}, nil},
		{"operator bans a manager", "opr", "mgr", [2]string{}, nil},
		{"operator bans an approver", "opr", "appr", [2]string{}, nil},
		{"operator bans an agent", "opr", "robo", [2]string{}, nil},
		{"operator pre-bans a non-member", "opr", "away", [2]string{}, nil},
		{"operator cannot ban an owner", "opr", "owner2", [2]string{}, owned},
		{"operator cannot ban an owner who is not a member either", "opr", "root", [2]string{}, owned},
		{"operator bans an unknown principal", "opr", "ghost", [2]string{}, notFound},
		// A manager bans a human, non-operator, non-approver member.
		{"manager bans a member", "mgr", "plain", [2]string{}, nil},
		{"manager bans an active guest", "mgr", "guest", [2]string{}, nil},
		{"manager bans a fellow manager", "mgr", "mgr2", [2]string{}, nil},
		{"manager bans themselves", "mgr", "mgr", [2]string{}, nil},
		{"manager cannot ban an approver", "mgr", "appr", [2]string{}, forbidden},
		{"manager cannot ban an agent", "mgr", "robo", [2]string{}, forbidden},
		{"manager cannot ban an operator member", "mgr", "oprMem", [2]string{}, forbidden},
		{"manager cannot ban an owner member", "mgr", "owner2", [2]string{}, forbidden},
		{"manager cannot pre-ban a non-member", "mgr", "away", [2]string{}, notFound},
		{"manager cannot pre-ban an unknown id", "mgr", "ghost", [2]string{}, notFound},
		{"manager cannot pre-ban a non-member operator", "mgr", "opr", [2]string{}, notFound},
		{"manager repeats their own ban", "mgr", "plain", [2]string{"mgr", "plain"}, nil},
		{"manager repeats their own pre-ban of a non-member", "mgr", "away", [2]string{"mgr", "away"}, nil},
		{"manager cannot repeat another manager's ban", "mgr", "plain", [2]string{"mgr2", "plain"}, forbidden},
		{"manager cannot repeat an operator's ban", "mgr", "plain", [2]string{"opr", "plain"}, forbidden},
		{"manager cannot repeat a ban written with authentication off", "mgr", "plain", [2]string{"", "plain"}, forbidden},
		// Nobody else has standing.
		{"plain member", "plain", "away", [2]string{}, forbidden},
		{"plain member banning a member", "plain", "mgr", [2]string{}, forbidden},
		{"approver", "appr", "plain", [2]string{}, forbidden},
		{"agent without a grant", "robo", "plain", [2]string{}, forbidden},
		{"non-member", "away", "plain", [2]string{}, forbidden},
		{"guest", "guest", "plain", [2]string{}, forbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newBanFixture(t)
			if tt.existing[1] != "" {
				f.banRow(tt.existing[0], tt.existing[1])
			}
			wasMember, wasBanned := f.member(tt.target), f.banned(tt.target)
			beforeAudit := len(f.audits(AuditChannelBanAdded)) + len(f.audits(AuditMemberRemoved))
			res, err := f.ban(tt.actor, tt.target)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("error = %v, want %v", err, tt.want)
				}
				if f.member(tt.target) != wasMember || f.banned(tt.target) != wasBanned {
					t.Error("a refused ban changed state")
				}
				if got := len(f.audits(AuditChannelBanAdded)) + len(f.audits(AuditMemberRemoved)); got != beforeAudit {
					t.Errorf("a refused ban wrote %d audit rows", got-beforeAudit)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !f.banned(tt.target) || f.member(tt.target) {
				t.Errorf("after the ban: banned = %v, member = %v; want banned and not a member", f.banned(tt.target), f.member(tt.target))
			}
			if res.Changed == wasBanned {
				t.Errorf("Changed = %v with the ban standing before = %v", res.Changed, wasBanned)
			}
			if res.Cleaned != wasMember {
				t.Errorf("Cleaned = %v with membership before = %v", res.Cleaned, wasMember)
			}
			wantAdded := 1
			if wasBanned {
				wantAdded = 0
			}
			if got := len(f.audits(AuditChannelBanAdded)); got != wantAdded {
				t.Errorf("channel_ban_added rows = %d, want %d", got, wantAdded)
			}
			wantRemoved := 0
			if wasMember {
				wantRemoved = 1
			}
			if got := len(f.audits(AuditMemberRemoved)); got != wantRemoved {
				t.Errorf("member_removed rows = %d, want %d", got, wantRemoved)
			}
			for _, e := range f.audits(AuditChannelBanAdded) {
				if e.Subject != channelSubject(f.ch.ID) || e.Detail != memberDetail(f.ids[tt.target]) || e.Actor != principalActor(f.ids[tt.actor]) {
					t.Errorf("ban row = %+v", e)
				}
			}
		})
	}
}

// TestUnbanActorTargetMatrix: who may lift whose ban, and that lifting an
// absent ban is a no-op that needs no lookup.
func TestUnbanActorTargetMatrix(t *testing.T) {
	forbidden, owned := ErrManagerRefused, ErrOwnerProtected
	tests := []struct {
		name     string
		actor    string
		target   string
		existing [2]string
		want     error
		changed  bool
	}{
		{"owner lifts a manager's ban", "root", "plain", [2]string{"mgr", "plain"}, nil, true},
		{"owner lifts a ban on an owner", "root", "owner2", [2]string{"root", "owner2"}, nil, true},
		{"operator lifts a manager's ban", "opr", "plain", [2]string{"mgr", "plain"}, nil, true},
		{"operator lifts an operator's ban", "opr", "plain", [2]string{"oprMem", "plain"}, nil, true},
		{"operator cannot lift a ban on an owner", "opr", "owner2", [2]string{"root", "owner2"}, owned, false},
		{"manager lifts their own ban", "mgr", "plain", [2]string{"mgr", "plain"}, nil, true},
		{"manager cannot lift another manager's ban", "mgr", "plain", [2]string{"mgr2", "plain"}, forbidden, false},
		{"manager cannot lift an operator's ban", "mgr", "plain", [2]string{"opr", "plain"}, forbidden, false},
		{"manager cannot lift an unattributed ban", "mgr", "plain", [2]string{"", "plain"}, forbidden, false},
		{"manager lifts an absent ban", "mgr", "plain", [2]string{}, nil, false},
		{"manager lifts an absent ban of an unknown id", "mgr", "ghost", [2]string{}, nil, false},
		{"operator lifts an absent ban of an unknown id", "opr", "ghost", [2]string{}, nil, false},
		{"plain member", "plain", "plain", [2]string{"plain", "plain"}, forbidden, false},
		{"plain member, absent ban", "plain", "away", [2]string{}, forbidden, false},
		{"non-member", "away", "plain", [2]string{"mgr", "plain"}, forbidden, false},
		{"agent without a grant", "robo", "plain", [2]string{"mgr", "plain"}, forbidden, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newBanFixture(t)
			if tt.existing[1] != "" {
				f.banRow(tt.existing[0], tt.existing[1])
			}
			res, err := f.unban(tt.actor, tt.target)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("error = %v, want %v", err, tt.want)
				}
				if tt.existing[1] != "" && !f.banned(tt.target) {
					t.Error("a refused unban lifted the ban")
				}
				if n := len(f.audits(AuditChannelBanRemoved)); n != 0 {
					t.Errorf("a refused unban wrote %d rows", n)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Changed != tt.changed {
				t.Errorf("Changed = %v, want %v", res.Changed, tt.changed)
			}
			if f.banned(tt.target) {
				t.Error("the ban stands")
			}
			wantRows := 0
			if tt.changed {
				wantRows = 1
			}
			if n := len(f.audits(AuditChannelBanRemoved)); n != wantRows {
				t.Errorf("channel_ban_removed rows = %d, want %d", n, wantRows)
			}
		})
	}
}

// TestUnbanRestoresNothing and the absent-ban rule: an unban makes a later
// explicit add possible and puts nothing back; an absent-ban unban leaves an
// ordinary membership alone (design §6.2).
func TestUnbanRestoresNothing(t *testing.T) {
	f := newBanFixture(t)
	if _, err := f.ban("mgr", "plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.unban("mgr", "plain"); err != nil {
		t.Fatal(err)
	}
	if f.member("plain") || f.banned("plain") {
		t.Fatal("unban restored a membership or left the ban")
	}
	// The ordinary member is not removed by an unban of nobody.
	if res, err := f.unban("opr", "mgr2"); err != nil || res.Changed || res.Cleaned || !f.member("mgr2") {
		t.Fatalf("absent-ban unban = %+v, %v, member = %v", res, err, f.member("mgr2"))
	}
	// An explicit add now works, and gives the ordinary new-member rights.
	if added, err := f.s.PutChannelMember(f.ctx, "system", f.ch.ID, f.ids["plain"], f.ids["opr"], nil, nil); err != nil || !added {
		t.Fatalf("re-add = %v, %v", added, err)
	}
}

// TestBanResidualMembership: a membership row that outlives a ban (a restored
// backup, a row from before the ban existed) grants nothing, is cleaned with
// audit by a repeat ban and by an unban before the ban row is deleted, and is
// not alive again after (design §6.2, §9 case 5).
func TestBanResidualMembership(t *testing.T) {
	seed := func(f *banFixture, name string) {
		t.Helper()
		// The residual: a membership row, a net seat and a ban, none cleaned.
		n, err := f.s.CreateNet(f.ctx, "system", f.ch.ID, "net1", f.ids["root"])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.PutNetMember(f.ctx, "system", f.ch.ID, n.Name, f.ids[name], schema.NetRoleMember, f.ids["root"]); err != nil {
			t.Fatal(err)
		}
		f.banRow("mgr", name)
	}
	reads := func(f *banFixture, name string) []string {
		t.Helper()
		id := f.ids[name]
		var got []string
		if ok, _ := f.s.IsChannelMember(f.ctx, f.ch.ID, id); ok {
			got = append(got, "IsChannelMember")
		}
		if ms, _ := f.s.ListChannelMembers(f.ctx, f.ch.ID); func() bool {
			for _, m := range ms {
				if m.PrincipalID == id {
					return true
				}
			}
			return false
		}() {
			got = append(got, "ListChannelMembers")
		}
		if chs, _ := f.s.ListChannelsForPrincipal(f.ctx, id); len(chs) != 0 {
			got = append(got, "ListChannelsForPrincipal")
		}
		if ok, _ := f.s.CanDecideApprovals(f.ctx, f.ch.ID, id); ok {
			got = append(got, "CanDecideApprovals")
		}
		if ok, _ := f.s.ManagesChannel(f.ctx, f.ch.ID, id); ok {
			got = append(got, "ManagesChannel")
		}
		if set, _ := f.s.DecidingChannels(f.ctx, id); len(set) != 0 {
			got = append(got, "DecidingChannels")
		}
		if nets, _ := f.s.ListNetsForPrincipal(f.ctx, f.ch.ID, id); len(nets) != 0 {
			got = append(got, "ListNetsForPrincipal")
		}
		return got
	}
	for _, name := range []string{"plain", "appr", "mgr2", "guest"} {
		t.Run(name, func(t *testing.T) {
			f := newBanFixture(t)
			seed(f, name)
			if !f.member(name) || !f.banned(name) {
				t.Fatal("the residual was not seeded")
			}
			// Before any cleanup: nothing is granted.
			if got := reads(f, name); len(got) != 0 {
				t.Fatalf("a residual banned membership still grants: %v", got)
			}
			// A repeat ban cleans it, audited, and writes no second ban row.
			res, err := f.ban("opr", name)
			if err != nil {
				t.Fatal(err)
			}
			if res.Changed || !res.Cleaned {
				t.Errorf("repeat ban = %+v, want the ban unchanged and the residual cleaned", res)
			}
			if f.member(name) {
				t.Error("the residual membership survived a repeat ban")
			}
			if n := len(f.audits(AuditChannelBanAdded)); n != 0 {
				t.Errorf("channel_ban_added rows = %d after a repeat of a standing ban", n)
			}
			if n := len(f.audits(AuditMemberRemoved)); n != 1 {
				t.Errorf("member_removed rows = %d, want 1", n)
			}
			if n := len(f.audits(AuditNetMemberRemoved)); n != 1 {
				t.Errorf("net_member_removed rows = %d, want 1", n)
			}
			// Seed again; the unban cleans before it deletes the ban.
			if _, err := f.s.db.Exec("INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide) VALUES (?, ?, NULL, 1, 1)",
				f.ch.ID, f.ids[name]); err != nil {
				t.Fatal(err)
			}
			if got := reads(f, name); len(got) != 0 {
				t.Fatalf("a re-seeded residual still grants: %v", got)
			}
			res, err = f.unban("opr", name)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Changed || !res.Cleaned {
				t.Errorf("unban = %+v, want the ban lifted and the residual cleaned", res)
			}
			if f.member(name) || f.banned(name) {
				t.Errorf("after unban: member = %v, banned = %v", f.member(name), f.banned(name))
			}
			if got := reads(f, name); len(got) != 0 {
				t.Errorf("after unban the principal reads as: %v", got)
			}
			if n := len(f.audits(AuditMemberRemoved)); n != 2 {
				t.Errorf("member_removed rows = %d, want 2", n)
			}
			if rows := f.audits(AuditChannelBanRemoved); len(rows) != 1 {
				t.Errorf("channel_ban_removed rows = %+v", rows)
			}
		})
	}
}

// TestUnbanKeepsResidualBehindTheBan: a residual that appears while the ban is
// unlifted is never alive: at every committed point either the ban stands or
// there is no membership (design §6.2). An unban whose cleanup fails leaves
// the ban standing.
func TestUnbanKeepsResidualBehindTheBan(t *testing.T) {
	f := newBanFixture(t)
	f.banRow("mgr", "plain")
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_removal BEFORE DELETE ON channel_members BEGIN SELECT RAISE(ABORT, 'no'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.unban("opr", "plain"); err == nil {
		t.Fatal("unban succeeded though the cleanup failed")
	}
	if !f.banned("plain") {
		t.Fatal("the ban was lifted while the residual membership remained")
	}
	if ok, _ := f.s.IsChannelMember(f.ctx, f.ch.ID, f.ids["plain"]); ok {
		t.Fatal("the residual membership is alive")
	}
}

// TestBanRefusedAdmissions: every path that can create or restore a
// membership refuses a banned principal inside its write transaction.
func TestBanRefusedAdmissions(t *testing.T) {
	tests := []struct {
		name string
		add  func(f *banFixture, target string) error
	}{
		{"PutChannelMember by an operator", func(f *banFixture, target string) error {
			_, err := f.s.PutChannelMember(f.ctx, "system", f.ch.ID, f.ids[target], f.ids["opr"], nil, nil)
			return err
		}},
		{"PutChannelMember with unconstrained actor", func(f *banFixture, target string) error {
			_, err := f.s.PutChannelMember(f.ctx, "system", f.ch.ID, f.ids[target], 0, nil, nil)
			return err
		}},
		{"PutChannelMember setting can_decide", func(f *banFixture, target string) error {
			yes := true
			_, err := f.s.PutChannelMember(f.ctx, "system", f.ch.ID, f.ids[target], f.ids["opr"], &yes, nil)
			return err
		}},
		{"PutChannelMember by a manager", func(f *banFixture, target string) error {
			_, err := f.s.PutChannelMember(f.ctx, "system", f.ch.ID, f.ids[target], f.ids["mgr"], nil, nil)
			return err
		}},
		{"AddChannelMember", func(f *banFixture, target string) error {
			_, err := f.s.AddChannelMember(f.ctx, "system", f.ch.ID, f.ids[target], 0)
			return err
		}},
		{"AddChannelMemberByName", func(f *banFixture, target string) error {
			_, _, err := f.s.AddChannelMemberByName(f.ctx, "system", f.ch.ID, f.ids["opr"], target)
			return err
		}},
		{"AddChannelMemberIfAbsent by id, manager", func(f *banFixture, target string) error {
			_, _, err := f.s.AddChannelMemberIfAbsent(f.ctx, "system", f.ch.ID, f.ids["mgr"], MemberTarget{ID: f.ids[target]}, nil)
			return err
		}},
		{"AddChannelMemberIfAbsent by id, operator with can_decide", func(f *banFixture, target string) error {
			yes := true
			_, _, err := f.s.AddChannelMemberIfAbsent(f.ctx, "system", f.ch.ID, f.ids["opr"], MemberTarget{ID: f.ids[target]}, &yes)
			return err
		}},
		{"JoinChannel (self-join) of a public channel", func(f *banFixture, target string) error {
			if _, err := f.s.db.Exec("UPDATE channels SET visibility = 'public' WHERE id = ?", f.ch.ID); err != nil {
				return err
			}
			_, err := f.s.JoinChannel(f.ctx, f.ch.ID, f.ids[target], time.Now())
			if errors.Is(err, ErrChannelNotJoinable) {
				return ErrNotAddable
			}
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newBanFixture(t)
			// A person with no membership, banned in advance.
			f.banRow("opr", "away")
			err := tt.add(f, "away")
			if !errors.Is(err, ErrNotAddable) {
				t.Fatalf("add of a banned principal = %v, want ErrNotAddable", err)
			}
			if f.member("away") {
				t.Fatal("a banned principal was added")
			}
			// The same path admits them once unbanned, so the refusal was the ban.
			if _, err := f.unban("opr", "away"); err != nil {
				t.Fatal(err)
			}
			if err := tt.add(f, "away"); err != nil {
				t.Fatalf("add after unban = %v", err)
			}
			if !f.member("away") {
				t.Error("the add after unban did not add")
			}
			// An unbanned principal is not affected by someone else's ban.
			f2 := newBanFixture(t)
			f2.banRow("opr", "plain")
			if err := tt.add(f2, "away"); err != nil {
				t.Errorf("a ban of one principal refused another: %v", err)
			}
		})
	}
}

// TestBanRefusesGuestAdmission: an active guest who is banned is not
// re-admitted by the operator add, and a new visit is a new principal that
// the old ban does not match (design §6.4).
func TestBanRefusesGuestAdmission(t *testing.T) {
	f := newBanFixture(t)
	other, err := f.s.CreateChannel(f.ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ban("mgr", "guest"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutChannelMember(f.ctx, "system", f.ch.ID, f.ids["guest"], f.ids["opr"], nil, nil); !errors.Is(err, ErrNotAddable) {
		t.Fatalf("operator re-admitting a banned guest = %v, want ErrNotAddable", err)
	}
	// Other channels are unaffected by a channel ban.
	if added, err := f.s.PutChannelMember(f.ctx, "system", other.ID, f.ids["guest"], f.ids["opr"], nil, nil); err != nil || !added {
		t.Fatalf("the banned guest in another channel = %v, %v", added, err)
	}
	// A new visit is a different principal.
	g2, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.ch.ID}, Idle: time.Hour, Absolute: 2 * time.Hour})
	if err != nil {
		t.Fatalf("a new visit to the channel = %v", err)
	}
	if g2.Principal.ID == f.ids["guest"] {
		t.Fatal("a new visit reused the principal id")
	}
	if ok, _ := f.s.IsChannelBanned(f.ctx, f.ch.ID, g2.Principal.ID); ok {
		t.Error("the old ban matched a new visit")
	}
	if ok, _ := f.s.IsChannelMember(f.ctx, f.ch.ID, g2.Principal.ID); !ok {
		t.Error("the new visit was not admitted")
	}
}

// TestBanSurvivesConversionAndDisable: conversion keeps the principal id, so
// the ban stands and conversion restores no membership; disable and enable
// leave it alone (design §6.3).
func TestBanSurvivesConversionAndDisable(t *testing.T) {
	f := newBanFixture(t)
	if _, err := f.ban("mgr", "guest"); err != nil {
		t.Fatal(err)
	}
	allowAddressAccounts(t, f.s)
	if _, err := f.s.ConvertGuest(f.ctx, "system", GuestConversion{PrincipalID: f.ids["guest"], Name: "pat", Record: testRecord("conv"), CredentialLifetime: time.Hour}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !f.banned("guest") || f.member("guest") {
		t.Fatalf("after conversion: banned = %v, member = %v", f.banned("guest"), f.member("guest"))
	}
	if _, err := f.s.PutChannelMember(f.ctx, "system", f.ch.ID, f.ids["guest"], f.ids["opr"], nil, nil); !errors.Is(err, ErrNotAddable) {
		t.Fatalf("adding the converted guest = %v, want ErrNotAddable", err)
	}
	if _, err := f.s.DisablePrincipal(f.ctx, "system", f.ids["plain"]); err != nil {
		t.Fatal(err)
	}
	f.banRow("opr", "away")
	for _, name := range []string{"away", "guest"} {
		if _, err := f.s.DisablePrincipal(f.ctx, "system", f.ids[name]); err != nil && !strings.Contains(err.Error(), "not found") {
			t.Fatal(err)
		}
		if _, err := f.s.EnablePrincipal(f.ctx, "system", f.ids[name]); err != nil {
			t.Fatal(err)
		}
		if !f.banned(name) {
			t.Errorf("%s: disable/enable cleared the ban", name)
		}
	}
}

// TestBanListOrderAndNames: ordered by principal id, current names, creator.
func TestBanListOrderAndNames(t *testing.T) {
	f := newBanFixture(t)
	for _, name := range []string{"plain", "away", "mgr2"} {
		if _, err := f.ban("opr", name); err != nil {
			t.Fatal(err)
		}
	}
	f.banRow("", "appr")
	bans, err := f.s.ListChannelBans(f.ctx, f.ch.ID, f.ids["opr"])
	if err != nil {
		t.Fatal(err)
	}
	// The ids were made in the order mgr, mgr2, plain, appr, away.
	want := []string{
		fmt.Sprintf("mgr2/%d", f.ids["opr"]), fmt.Sprintf("plain/%d", f.ids["opr"]),
		"appr/0", fmt.Sprintf("away/%d", f.ids["opr"]),
	}
	var got []string
	for _, b := range bans {
		got = append(got, fmt.Sprintf("%s/%d", b.Name, b.CreatedBy))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bans = %v, want %v", got, want)
	}
	if empty, err := f.s.ListChannelBans(f.ctx, 99999, 0); err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("an empty list = %v, %v; want a non-nil empty slice", empty, err)
	}
}

// TestBanInArchivedChannel: banning and unbanning are reduce-only and allowed
// in an archived channel (#849 Q1).
func TestBanInArchivedChannel(t *testing.T) {
	f := newBanFixture(t)
	if _, err := f.s.db.Exec("UPDATE channels SET archived_at = 1 WHERE id = ?", f.ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ban("mgr", "plain"); err != nil {
		t.Fatalf("ban in an archived channel = %v", err)
	}
	if _, err := f.unban("mgr", "plain"); err != nil {
		t.Fatalf("unban in an archived channel = %v", err)
	}
}

// TestBanUnknownChannel: the channel is checked, for any actor with standing.
func TestBanUnknownChannel(t *testing.T) {
	f := newBanFixture(t)
	if _, err := f.s.BanChannelMember(f.ctx, "system", 99999, f.ids["plain"], f.ids["opr"]); !errors.Is(err, ErrNotFound) {
		t.Errorf("ban in an unknown channel = %v, want ErrNotFound", err)
	}
	if _, err := f.s.UnbanChannelMember(f.ctx, "system", 99999, f.ids["plain"], f.ids["opr"]); !errors.Is(err, ErrNotFound) {
		t.Errorf("unban in an unknown channel = %v, want ErrNotFound", err)
	}
}

// TestBannedResidualManagerHasNoStanding: a manager grant on a membership row
// that outlived a ban is not a grant (the actor's standing is read through
// the ban predicate).
func TestBannedResidualManagerHasNoStanding(t *testing.T) {
	f := newBanFixture(t)
	f.banRow("opr", "mgr")
	if _, err := f.ban("mgr", "plain"); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("a banned manager banning = %v, want ErrManagerRefused", err)
	}
	if ok, err := f.s.ManagesChannel(f.ctx, f.ch.ID, f.ids["mgr"]); err != nil || ok {
		t.Errorf("ManagesChannel = %v, %v", ok, err)
	}
	if err := f.s.CheckChannelModerator(f.ctx, f.ch.ID, f.ids["mgr"]); !errors.Is(err, ErrManagerRefused) {
		t.Errorf("CheckChannelModerator = %v, want ErrManagerRefused", err)
	}
}

// TestBannedUploaderReachesNoFile: the uploader path of a file is a
// membership path too: a banned uploader cannot attach or read an unattached
// upload, whatever the membership row says.
func TestBannedUploaderReachesNoFile(t *testing.T) {
	f := newBanFixture(t)
	file, err := f.s.InsertFile(f.ctx, NewFile{ChannelID: f.ch.ID, UploaderID: f.ids["plain"], Filename: "a.txt", MediaType: "text/plain", Size: 1, SHA256: strings.Repeat("0", 64)})
	if err != nil {
		t.Fatal(err)
	}
	reader := Reader{PrincipalID: f.ids["plain"], Scoped: true}
	if _, err := f.s.FileVisibleTo(f.ctx, file.ID, reader); err != nil {
		t.Fatalf("the uploader before the ban: %v", err)
	}
	f.banRow("mgr", "plain")
	if _, err := f.s.FileVisibleTo(f.ctx, file.ID, reader); !errors.Is(err, ErrNotFound) {
		t.Errorf("a banned uploader reading their file = %v, want ErrNotFound", err)
	}
	if _, err := f.s.InsertFile(f.ctx, NewFile{ChannelID: f.ch.ID, UploaderID: f.ids["plain"], Filename: "b.txt", MediaType: "text/plain", Size: 1, SHA256: strings.Repeat("1", 64)}); !errors.Is(err, ErrNotChannelMember) {
		t.Errorf("a banned uploader uploading = %v, want ErrNotChannelMember", err)
	}
}

// TestBanListHidesWhatAManagerMayNotKnow: an operator's pre-ban of a person
// outside a manager's view does not tell the manager the person's name; the
// operator sees it.
func TestBanListHidesWhatAManagerMayNotKnow(t *testing.T) {
	f := newBanFixture(t)
	// away shares no channel with mgr; members lose see_member_directory.
	doc, err := f.s.Permissions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutPermissions(f.ctx, principalActor(f.ids["root"]), f.ids["root"], schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: false}},
	}, credentialNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ban("opr", "away"); err != nil {
		t.Fatal(err)
	}
	names := func(viewer string) map[int64]string {
		bans, err := f.s.ListChannelBans(f.ctx, f.ch.ID, f.ids[viewer])
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]string{}
		for _, b := range bans {
			out[b.PrincipalID] = b.Name
		}
		return out
	}
	op := names("opr")
	if op[f.ids["away"]] != "away" {
		t.Errorf("operator sees %v", op)
	}
	mg := names("mgr")
	if mg[f.ids["away"]] == "away" {
		t.Errorf("manager sees %v; want hidden name for away", mg)
	}
	if want := fmt.Sprintf("hidden-%d", f.ids["away"]); mg[f.ids["away"]] != want {
		t.Errorf("manager sees %q, want %q", mg[f.ids["away"]], want)
	}
}

// TestConvertedGuestChannelsSkipsABannedResidual: conversion reports only
// channels the guest is live in.
func TestConvertedGuestChannelsSkipsABannedResidual(t *testing.T) {
	f := newBanFixture(t)
	allowAddressAccounts(t, f.s)
	f.banRow("mgr", "guest")
	out, err := f.s.ConvertGuest(f.ctx, "system", GuestConversion{PrincipalID: f.ids["guest"], Name: "pat", Record: testRecord("conv"), CredentialLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Channels) != 0 {
		t.Errorf("Channels = %v, want none for a banned residual", out.Channels)
	}
}
