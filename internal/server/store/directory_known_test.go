package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// The may-know predicate (issue #988, design §13.2): knownPrincipalsSQL is
// the one SQL fragment, mayKnowTx and ListDirectory and the signal's
// audiences read it.

type knowsWorld struct {
	s                                              *Store
	root, zed, bob, carol, dave, pub1, pub2, agent Principal
	chA, chB, chC                                  Channel
}

func newKnowsWorld(t *testing.T) *knowsWorld {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)
	w := &knowsWorld{s: s}
	root, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	w.root = root
	mk := func(kind PrincipalKind, name string) Principal {
		t.Helper()
		p, err := s.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	w.zed, w.bob, w.carol, w.dave = mk(PrincipalHuman, "zed"), mk(PrincipalHuman, "bob"), mk(PrincipalHuman, "carol"), mk(PrincipalHuman, "dave")
	w.pub1, w.pub2 = mk(PrincipalHuman, "pub1"), mk(PrincipalHuman, "pub2")
	w.agent = mk(PrincipalAgent, "agent")
	ch := func(name string, v schema.ChannelVisibility) Channel {
		t.Helper()
		c, err := s.CreateUnownedChannel(ctx, "system", name, v)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	w.chA, w.chB, w.chC = ch("a", schema.ChannelPrivate), ch("b", schema.ChannelPrivate), ch("c", schema.ChannelPublic)
	add := func(c Channel, ps ...Principal) {
		t.Helper()
		for _, p := range ps {
			if _, err := s.AddChannelMember(ctx, "system", c.ID, p.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(w.chA, root, w.zed, w.dave, w.agent)
	add(w.chB, root, w.bob)
	add(w.chC, w.pub1, w.pub2)
	// carol joined through a link zed made; zed joined through carol's:
	// neither shares a channel with the other.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE principals SET invited_by = ? WHERE id = ?", w.zed.ID, w.carol.ID)
	exec("UPDATE principals SET invited_by = ? WHERE id = ?", w.bob.ID, w.dave.ID)
	return w
}

// TestKnownPrincipalsPredicate: every clause alone and the clauses refused,
// for a viewer who does not hold see_member_directory, and the viewer who
// does.
func TestKnownPrincipalsPredicate(t *testing.T) {
	w := newKnowsWorld(t)
	ctx := context.Background()
	// pub1 joins A, where zed is, in addition to C.
	if _, err := w.s.AddChannelMember(ctx, "system", w.chA.ID, w.pub1.ID, 0); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		viewer, target Principal
		seeAll         bool
		want           bool
	}{
		{"clause 3: themself", w.bob, w.bob, false, true},
		{"clause 4: a private channel in common", w.zed, w.dave, false, true},
		{"clause 4: and the other way round", w.dave, w.zed, false, true},
		{"clause 4: an agent in a private channel in common", w.zed, w.agent, false, true},
		{"clause 4: a public channel in common", w.pub2, w.pub1, false, true},
		{"clause 4: a channel in common with the owner", w.bob, w.root, false, true},
		{"clause 5: a joiner of their own link, in no channel", w.zed, w.carol, false, true},
		{"clause 5 is one way: the joiner does not know the maker", w.carol, w.zed, false, false},
		{"clause 5: bob's joiner dave is known to bob, who shares nothing with him", w.bob, w.dave, false, true},
		{"nothing in common", w.zed, w.bob, false, false},
		{"nothing in common, the other way", w.bob, w.zed, false, false},
		{"a stranger to an agent", w.bob, w.agent, false, false},
		{"a person in no channel", w.zed, w.pub2, false, false},
		{"the cell held: a stranger is known", w.zed, w.bob, true, true},
		{"the cell held: a ghost id", w.zed, Principal{ID: 1 << 40}, true, false},
		{"a ghost id, the cell off", w.zed, Principal{ID: 1 << 40}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mayKnowTx(ctx, w.s.db, tt.viewer.ID, tt.target.ID, tt.seeAll)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("mayKnow(%s, %s, seeAll=%v) = %v, want %v", tt.viewer.Name, tt.target.Name, tt.seeAll, got, tt.want)
			}
		})
	}
}

// TestListDirectoryHidden: the directory of a viewer without the cell is the
// predicate's set, filtered before paging, and the base set (enabled, not
// hidden, a member or an enabled agent) wins whatever the predicate says.
func TestListDirectoryHidden(t *testing.T) {
	w := newKnowsWorld(t)
	ctx := context.Background()
	// Two people zed shares A with who are not members: one disabled, one
	// hidden.
	off, err := w.s.CreatePrincipal(ctx, PrincipalHuman, "off")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := w.s.CreatePrincipal(ctx, PrincipalHuman, "gone")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{off, gone} {
		if _, err := w.s.AddChannelMember(ctx, "system", w.chA.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.s.DisablePrincipal(ctx, "system", off.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.HidePrincipal(ctx, "system", gone.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	// A hidden one also keeps a membership row the way a hand-edited
	// database could: the base set still drops it.
	if _, err := w.s.db.ExecContext(ctx, "INSERT OR IGNORE INTO channel_members (channel_id, principal_id, created_at) VALUES (?, ?, 0)", w.chA.ID, gone.ID); err != nil {
		t.Fatal(err)
	}

	ids := func(p schema.ServerMembersResponseV1) []int64 {
		out := []int64{}
		for _, m := range p.Members {
			out = append(out, m.ID)
		}
		return out
	}
	zed := DirectoryViewer{ID: w.zed.ID}
	want := []int64{w.root.ID, w.zed.ID, w.carol.ID, w.dave.ID, w.agent.ID}
	slices.Sort(want)
	page, err := w.s.ListDirectory(ctx, zed, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page); !slices.Equal(got, want) {
		t.Errorf("zed's directory = %v, want %v", got, want)
	}
	if page.Scope != schema.DirectoryScopeSharedChannels {
		t.Errorf("scope = %q", page.Scope)
	}
	// Paged by one: the same ids, every cursor a listed id, bob's in none.
	var walked []int64
	after := int64(0)
	for range want {
		p, err := w.s.ListDirectory(ctx, zed, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, ids(p)...)
		if p.After == 0 {
			break
		}
		if p.After != p.Members[0].ID {
			t.Errorf("cursor %d is not the listed row %d", p.After, p.Members[0].ID)
		}
		after = p.After
	}
	if !slices.Equal(walked, want) {
		t.Errorf("paged %v, want %v", walked, want)
	}

	// The cell held, or an operator: everyone in the base set, not the
	// disabled and the hidden.
	all, err := w.s.ListDirectory(ctx, DirectoryViewer{ID: w.bob.ID, SeeAll: true}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.Scope == all.Scope || all.Scope != schema.DirectoryScopeEveryone {
		t.Errorf("scope with the cell held = %q", all.Scope)
	}
	if got := ids(all); slices.Contains(got, off.ID) || slices.Contains(got, gone.ID) || !slices.Contains(got, w.zed.ID) {
		t.Errorf("everyone's directory = %v", got)
	}
}

// TestManagerAddFollowsThePredicate: a manager without the cell adds a
// channel-mate or one of their own joiners and no one else, and the base set
// refuses a disabled person whatever they share.
func TestManagerAddFollowsThePredicate(t *testing.T) {
	w := newKnowsWorld(t)
	ctx := context.Background()
	yes := true
	// zed manages A.
	if _, err := w.s.PutChannelMember(ctx, "system", w.chA.ID, w.zed.ID, 0, nil, &yes); err != nil {
		t.Fatal(err)
	}
	// pub1 and zed share C; pub1 is a candidate for A.
	if _, err := w.s.AddChannelMember(ctx, "system", w.chC.ID, w.zed.ID, 0); err != nil {
		t.Fatal(err)
	}
	disabled, err := w.s.CreatePrincipal(ctx, PrincipalHuman, "disabled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.AddChannelMember(ctx, "system", w.chC.ID, disabled.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.DisablePrincipal(ctx, "system", disabled.ID); err != nil {
		t.Fatal(err)
	}
	for _, held := range []bool{true, false} {
		if !held {
			doc, err := w.s.Permissions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.s.PutPermissions(ctx, principalActor(w.root.ID), w.root.ID, schema.PutPermissionsRequestV1{
				Revision: doc.Revision,
				Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: false}},
			}, credentialNow()); err != nil {
				t.Fatal(err)
			}
		}
		tests := []struct {
			name   string
			target int64
			// addable with the cell held, and with it off
			held, off bool
		}{
			{"a person who shares public C", w.pub1.ID, true, true},
			{"their own joiner", w.carol.ID, true, true},
			{"a stranger", w.bob.ID, true, false},
			{"a ghost id", 1 << 40, false, false},
			{"a disabled person they share C with", disabled.ID, false, false},
		}
		for _, tt := range tests {
			want := tt.off
			if held {
				want = tt.held
			}
			err := w.s.CheckManagerAddTarget(ctx, w.chA.ID, w.zed.ID, tt.target)
			if (err == nil) != want {
				t.Errorf("cell held=%v, %s: err = %v, want addable=%v", held, tt.name, err, want)
			}
		}
	}
}

// TestDirectoryMembershipAudience: who is owed the signal after a membership
// change, by who would still know the subject without the channel.
func TestDirectoryMembershipAudience(t *testing.T) {
	w := newKnowsWorld(t)
	ctx := context.Background()
	ids := func(everyone bool, ids []int64, err error) ([]int64, bool) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		return ids, everyone
	}
	// With the cell held (the default) nobody is told.
	if got, everyone := ids(w.s.DirectoryMembershipAudience(ctx, w.chB.ID, w.bob.ID)); !everyone || len(got) != 0 {
		t.Errorf("cell held: %v everyone=%v", got, everyone)
	}
	doc, err := w.s.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.PutPermissions(ctx, principalActor(w.root.ID), w.root.ID, schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: false}},
	}, credentialNow()); err != nil {
		t.Fatal(err)
	}
	// bob in B: B's only other member is the owner. bob himself and the
	// operator are told.
	if got, _ := ids(w.s.DirectoryMembershipAudience(ctx, w.chB.ID, w.bob.ID)); !slices.Equal(got, sorted(w.root.ID, w.bob.ID)) {
		t.Errorf("bob in B: %v", got)
	}
	// dave in A: zed, the agent (not a listener) and the owner share A with
	// him; bob made dave's link, so bob knows him regardless and is not told
	// by A.
	got, _ := ids(w.s.DirectoryMembershipAudience(ctx, w.chA.ID, w.dave.ID))
	if want := sorted(w.root.ID, w.zed.ID, w.dave.ID); !slices.Equal(got, want) {
		t.Errorf("dave in A: %v, want %v", got, want)
	}
	// Once zed and dave share C too, A no longer decides whether zed knows
	// dave.
	for _, p := range []Principal{w.zed, w.dave} {
		if _, err := w.s.AddChannelMember(ctx, "system", w.chC.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = ids(w.s.DirectoryMembershipAudience(ctx, w.chA.ID, w.dave.ID))
	if want := sorted(w.root.ID, w.dave.ID); !slices.Equal(got, want) {
		t.Errorf("dave in A, zed also sharing C: %v, want %v", got, want)
	}
}

// countingExecer counts the statements run through it.
type countingExecer struct {
	execer
	n int
}

func (c *countingExecer) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	c.n++
	return c.execer.ExecContext(ctx, q, args...)
}

func (c *countingExecer) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	c.n++
	return c.execer.QueryContext(ctx, q, args...)
}

func (c *countingExecer) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	c.n++
	return c.execer.QueryRowContext(ctx, q, args...)
}

// TestManagerAddRunsTheSameStatementsInEveryClass: a refusal must not be
// faster than another (security review S1 of #988). A ghost id, a hidden,
// a disabled and an operator target, an agent, a member the manager may not
// know and one they may all run the same number of statements.
func TestManagerAddRunsTheSameStatementsInEveryClass(t *testing.T) {
	w := newKnowsWorld(t)
	ctx := context.Background()
	hidden, err := w.s.CreatePrincipal(ctx, PrincipalHuman, "hid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.HidePrincipal(ctx, "system", hidden.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	off, err := w.s.CreatePrincipal(ctx, PrincipalHuman, "off")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.DisablePrincipal(ctx, "system", off.ID); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		target int64
		seeAll bool
	}{
		{"a ghost id", 1 << 40, false},
		{"a hidden person", hidden.ID, false},
		{"a disabled person", off.ID, false},
		{"an operator", w.root.ID, false},
		{"an agent", w.agent.ID, false},
		{"a member the manager may not know", w.bob.ID, false},
		{"a member the manager may know", w.dave.ID, false},
	}
	if _, err := w.s.PutPermissions(ctx, principalActor(w.root.ID), w.root.ID, schema.PutPermissionsRequestV1{
		Revision: func() int64 { d, _ := w.s.Permissions(ctx); return d.Revision }(),
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: false}},
	}, credentialNow()); err != nil {
		t.Fatal(err)
	}
	want := -1
	for _, tt := range tests {
		c := &countingExecer{execer: w.s.db}
		err := checkManagerAddTargetTx(ctx, c, w.zed.ID, tt.target)
		if tt.name == "a member the manager may know" {
			if err != nil {
				t.Errorf("%s: %v", tt.name, err)
			}
		} else if !errors.Is(err, ErrNotAddable) {
			t.Errorf("%s: err = %v, want ErrNotAddable", tt.name, err)
		}
		if want < 0 {
			want = c.n
		}
		if c.n != want {
			t.Errorf("%s ran %d statements, a ghost id ran %d", tt.name, c.n, want)
		}
	}
}

// TestHidePrincipalReturnsWhoCouldKnowIt: the audience of the directory
// signal is read in the hiding transaction, before the memberships go.
func TestHidePrincipalReturnsWhoCouldKnowIt(t *testing.T) {
	w := newKnowsWorld(t)
	ctx := context.Background()
	doc, err := w.s.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.PutPermissions(ctx, principalActor(w.root.ID), w.root.ID, schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: false}},
	}, credentialNow()); err != nil {
		t.Fatal(err)
	}
	res, err := w.s.HidePrincipal(ctx, "system", w.dave.ID, HideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// dave shared A with zed and the owner, and bob made his link.
	slices.Sort(res.Knowers)
	if want := sorted(w.root.ID, w.zed.ID, w.bob.ID, w.dave.ID); res.KnowersEveryone || !slices.Equal(res.Knowers, want) {
		t.Errorf("knowers = %v everyone=%v, want %v", res.Knowers, res.KnowersEveryone, want)
	}
	// A repeat changes nothing and reports nobody.
	again, err := w.s.HidePrincipal(ctx, "system", w.dave.ID, HideOptions{})
	if err != nil || again.Changed || len(again.Knowers) != 0 {
		t.Errorf("a repeated hide = %+v, %v", again, err)
	}
}

func sorted(ids ...int64) []int64 {
	slices.Sort(ids)
	return ids
}
