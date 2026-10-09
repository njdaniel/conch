package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

type netFixture struct {
	s     *Store
	ch    Channel
	other Channel
	alice Principal
	bob   Principal
	carol Principal // not a member of ch
}

func newNetFixture(t *testing.T) *netFixture {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)
	f := &netFixture{s: s}
	var err error
	if f.ch, err = s.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	if f.other, err = s.CreateChannel(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	// A slice, not a map: ids must be assigned in a fixed order, because the
	// roster tests assert an ordering by principal id.
	for _, p := range []struct {
		name string
		dst  *Principal
	}{{"alice", &f.alice}, {"bob", &f.bob}, {"carol", &f.carol}} {
		if *p.dst, err = s.CreatePrincipal(ctx, PrincipalHuman, p.name); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []Principal{f.alice, f.bob} {
		if _, err := s.AddChannelMember(ctx, "system", f.ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *netFixture) mustNet(t *testing.T, name string) Net {
	t.Helper()
	n, err := f.s.CreateNet(context.Background(), "system", f.ch.ID, name, f.alice.ID)
	if err != nil {
		t.Fatalf("CreateNet %q: %v", name, err)
	}
	return n
}

func (f *netFixture) put(t *testing.T, net string, p Principal, role schema.NetRole) NetMemberChange {
	t.Helper()
	c, err := f.s.PutNetMember(context.Background(), "system", f.ch.ID, net, p.ID, role, f.alice.ID)
	if err != nil {
		t.Fatalf("PutNetMember(%s, %s, %s): %v", net, p.Name, role, err)
	}
	return c
}

func netIDs(nets []Net) []int64 {
	ids := []int64{}
	for _, n := range nets {
		ids = append(ids, n.ID)
	}
	return ids
}

func TestNetRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newNetFixture(t)

	if nets, err := f.s.ListNets(ctx, f.ch.ID); err != nil || nets == nil || len(nets) != 0 {
		t.Fatalf("ListNets on a fresh channel = %#v, %v; want empty non-nil", nets, err)
	}
	alpha := f.mustNet(t, "alpha")
	beta := f.mustNet(t, "beta")
	if alpha.ID == 0 || alpha.CreatedAt.IsZero() || alpha.CreatedBy != f.alice.ID || alpha.ChannelID != f.ch.ID {
		t.Errorf("created net = %+v", alpha)
	}

	got, err := f.s.NetByName(ctx, f.ch.ID, "alpha")
	if err != nil || got.ID != alpha.ID || got.ArchivedAt != nil || !got.CreatedAt.Equal(alpha.CreatedAt) {
		t.Errorf("NetByName = %+v, %v", got, err)
	}
	for name, want := range map[string]error{"nosuch": ErrNotFound, "": ErrNotFound} {
		if _, err := f.s.NetByName(ctx, f.ch.ID, name); !errors.Is(err, want) {
			t.Errorf("NetByName(%q) = %v, want %v", name, err, want)
		}
	}
	if _, err := f.s.NetByName(ctx, f.other.ID, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("NetByName in another channel = %v, want ErrNotFound", err)
	}
	nets, err := f.s.ListNets(ctx, f.ch.ID)
	if err != nil || !reflect.DeepEqual(netIDs(nets), []int64{alpha.ID, beta.ID}) {
		t.Errorf("ListNets = %v, %v", netIDs(nets), err)
	}
	if nets, err := f.s.ListNets(ctx, f.other.ID); err != nil || len(nets) != 0 {
		t.Errorf("ListNets(other) = %v, %v", nets, err)
	}

	// Duplicate live name is refused; the same name elsewhere is fine; an
	// unknown channel is not found.
	if _, err := f.s.CreateNet(ctx, "system", f.ch.ID, "alpha", 0); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate create = %v, want ErrDuplicate", err)
	}
	if _, err := f.s.CreateNet(ctx, "system", f.other.ID, "alpha", 0); err != nil {
		t.Errorf("same name in another channel: %v", err)
	}
	if _, err := f.s.CreateNet(ctx, "system", 9999, "alpha", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("create in unknown channel = %v, want ErrNotFound", err)
	}
	if n := auditCount(t, f.s, AuditNetCreated); n != 3 {
		t.Errorf("net_created events = %d, want 3 (failed creates write none)", n)
	}
	// An unknown creator (0) is stored as NULL and read back as 0.
	if n, err := f.s.CreateNet(ctx, "system", f.ch.ID, "anon", 0); err != nil || n.CreatedBy != 0 {
		t.Errorf("anonymous create = %+v, %v", n, err)
	}
}

func TestNetMembershipRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newNetFixture(t)
	n := f.mustNet(t, "alpha")

	if ms, err := f.s.ListNetMembers(ctx, n.ID); err != nil || ms == nil || len(ms) != 0 {
		t.Fatalf("ListNetMembers on an empty net = %#v, %v; want empty non-nil", ms, err)
	}
	// Add out of order: the roster is ordered by principal id.
	if c := f.put(t, "alpha", f.bob, schema.NetRoleMonitor); c != NetMemberAdded {
		t.Errorf("first put = %v, want added", c)
	}
	if c := f.put(t, "alpha", f.alice, schema.NetRoleMember); c != NetMemberAdded {
		t.Errorf("first put = %v, want added", c)
	}
	ms, err := f.s.ListNetMembers(ctx, n.ID)
	if err != nil || len(ms) != 2 {
		t.Fatalf("ListNetMembers = %+v, %v", ms, err)
	}
	if ms[0].PrincipalID != f.alice.ID || ms[0].Role != schema.NetRoleMember || ms[0].AddedBy != f.alice.ID || ms[0].CreatedAt.IsZero() ||
		ms[1].PrincipalID != f.bob.ID || ms[1].Role != schema.NetRoleMonitor {
		t.Errorf("roster = %+v", ms)
	}

	// Idempotent put: no change, no audit event.
	if c := f.put(t, "alpha", f.alice, schema.NetRoleMember); c != NetMemberUnchanged {
		t.Errorf("repeat put = %v, want unchanged", c)
	}
	if got := auditCount(t, f.s, AuditNetMemberAdded); got != 2 {
		t.Errorf("net_member_added events = %d, want 2", got)
	}

	// IsNetMember is true for a member, false for a monitor and a stranger.
	for _, tt := range []struct {
		name string
		p    Principal
		want bool
	}{{"member", f.alice, true}, {"monitor", f.bob, false}, {"not on the net", f.carol, false}} {
		if got, err := f.s.IsNetMember(ctx, n.ID, tt.p.ID); err != nil || got != tt.want {
			t.Errorf("IsNetMember(%s) = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}

	// Role change both ways.
	if c := f.put(t, "alpha", f.bob, schema.NetRoleMember); c != NetMemberRoleChanged {
		t.Errorf("promote = %v, want role changed", c)
	}
	if ok, _ := f.s.IsNetMember(ctx, n.ID, f.bob.ID); !ok {
		t.Error("bob should now transmit")
	}
	if c := f.put(t, "alpha", f.bob, schema.NetRoleMonitor); c != NetMemberRoleChanged {
		t.Errorf("demote = %v, want role changed", c)
	}
	if got := auditCount(t, f.s, AuditNetMemberRoleChanged); got != 2 {
		t.Errorf("net_member_role_changed events = %d, want 2", got)
	}

	// Per-principal listing covers both roles and skips other nets.
	f.mustNet(t, "beta")
	f.put(t, "beta", f.alice, schema.NetRoleMonitor)
	for _, tt := range []struct {
		p    Principal
		want int
	}{{f.alice, 2}, {f.bob, 1}, {f.carol, 0}} {
		got, err := f.s.ListNetsForPrincipal(ctx, f.ch.ID, tt.p.ID)
		if err != nil || got == nil || len(got) != tt.want {
			t.Errorf("ListNetsForPrincipal(%s) = %v, %v; want %d", tt.p.Name, netIDs(got), err, tt.want)
		}
	}
	if got, err := f.s.ListNetsForPrincipal(ctx, f.other.ID, f.alice.ID); err != nil || len(got) != 0 {
		t.Errorf("ListNetsForPrincipal in another channel = %v, %v", got, err)
	}

	// Remove, then remove again.
	if removed, err := f.s.RemoveNetMember(ctx, "system", f.ch.ID, "alpha", f.bob.ID); err != nil || !removed {
		t.Errorf("remove = %v, %v", removed, err)
	}
	if removed, err := f.s.RemoveNetMember(ctx, "system", f.ch.ID, "alpha", f.bob.ID); err != nil || removed {
		t.Errorf("repeat remove = %v, %v; want false", removed, err)
	}
	if removed, err := f.s.RemoveNetMember(ctx, "system", f.ch.ID, "alpha", 9999); err != nil || removed {
		t.Errorf("remove of unknown principal = %v, %v; want false, nil", removed, err)
	}
	if got := auditCount(t, f.s, AuditNetMemberRemoved); got != 1 {
		t.Errorf("net_member_removed events = %d, want 1", got)
	}
}

func TestNetMemberErrors(t *testing.T) {
	ctx := context.Background()
	f := newNetFixture(t)
	f.mustNet(t, "alpha")
	before := auditCount(t, f.s, AuditNetMemberAdded)

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"principal not in the channel", func() error {
			_, err := f.s.PutNetMember(ctx, "system", f.ch.ID, "alpha", f.carol.ID, schema.NetRoleMember, 0)
			return err
		}, ErrNotChannelMember},
		{"unknown principal", func() error {
			_, err := f.s.PutNetMember(ctx, "system", f.ch.ID, "alpha", 9999, schema.NetRoleMember, 0)
			return err
		}, ErrNotChannelMember},
		{"unknown net", func() error {
			_, err := f.s.PutNetMember(ctx, "system", f.ch.ID, "nosuch", f.alice.ID, schema.NetRoleMember, 0)
			return err
		}, ErrNotFound},
		{"unknown channel", func() error {
			_, err := f.s.PutNetMember(ctx, "system", 9999, "alpha", f.alice.ID, schema.NetRoleMember, 0)
			return err
		}, ErrNotFound},
		{"net of another channel", func() error {
			_, err := f.s.PutNetMember(ctx, "system", f.other.ID, "alpha", f.alice.ID, schema.NetRoleMember, 0)
			return err
		}, ErrNotFound},
		{"remove from unknown net", func() error {
			_, err := f.s.RemoveNetMember(ctx, "system", f.ch.ID, "nosuch", f.alice.ID)
			return err
		}, ErrNotFound},
		{"archive unknown net", func() error {
			_, err := f.s.ArchiveNet(ctx, "system", f.ch.ID, "nosuch")
			return err
		}, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
	if _, err := f.s.PutNetMember(ctx, "system", f.ch.ID, "alpha", f.alice.ID, "captain", 0); err == nil {
		t.Error("an invalid role was accepted")
	}
	if got := auditCount(t, f.s, AuditNetMemberAdded); got != before {
		t.Errorf("failed puts wrote %d audit events", got-before)
	}
}

func TestNetArchiveFreesName(t *testing.T) {
	ctx := context.Background()
	f := newNetFixture(t)
	first := f.mustNet(t, "alpha")
	f.put(t, "alpha", f.alice, schema.NetRoleMember)

	archived, err := f.s.ArchiveNet(ctx, "principal:1", f.ch.ID, "alpha")
	if err != nil || archived.ID != first.ID || archived.ArchivedAt == nil {
		t.Fatalf("ArchiveNet = %+v, %v", archived, err)
	}
	// Archived: gone from lookup, listings and member writes; a second archive
	// is not found and writes nothing.
	if _, err := f.s.NetByName(ctx, f.ch.ID, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("NetByName(archived) = %v, want ErrNotFound", err)
	}
	if nets, _ := f.s.ListNets(ctx, f.ch.ID); len(nets) != 0 {
		t.Errorf("ListNets after archive = %v", netIDs(nets))
	}
	if nets, _ := f.s.ListNetsForPrincipal(ctx, f.ch.ID, f.alice.ID); len(nets) != 0 {
		t.Errorf("ListNetsForPrincipal after archive = %v", netIDs(nets))
	}
	if _, err := f.s.PutNetMember(ctx, "system", f.ch.ID, "alpha", f.bob.ID, schema.NetRoleMember, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("put on archived net = %v, want ErrNotFound", err)
	}
	if _, err := f.s.ArchiveNet(ctx, "system", f.ch.ID, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second archive = %v, want ErrNotFound", err)
	}
	if got := auditCount(t, f.s, AuditNetArchived); got != 1 {
		t.Errorf("net_archived events = %d, want 1", got)
	}
	// The roster is kept as the record of who was on the net.
	if ms, _ := f.s.ListNetMembers(ctx, first.ID); len(ms) != 1 {
		t.Errorf("archived net roster = %+v, want the kept row", ms)
	}

	// The name is free again, with a new id and an empty roster.
	second, err := f.s.CreateNet(ctx, "system", f.ch.ID, "alpha", f.alice.ID)
	if err != nil || second.ID == first.ID {
		t.Fatalf("recreate = %+v, %v; want a new id", second, err)
	}
	if ms, _ := f.s.ListNetMembers(ctx, second.ID); len(ms) != 0 {
		t.Errorf("recreated net roster = %+v, want empty", ms)
	}
	if got, err := f.s.NetByName(ctx, f.ch.ID, "alpha"); err != nil || got.ID != second.ID {
		t.Errorf("NetByName = %+v, %v; want the new net", got, err)
	}
}

func TestChannelRemovalCascadesToNets(t *testing.T) {
	ctx := context.Background()
	f := newNetFixture(t)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		f.mustNet(t, name)
	}
	f.put(t, "alpha", f.alice, schema.NetRoleMember)
	f.put(t, "beta", f.alice, schema.NetRoleMonitor)
	f.put(t, "gamma", f.alice, schema.NetRoleMember) // archived below
	f.put(t, "alpha", f.bob, schema.NetRoleMember)
	if _, err := f.s.ArchiveNet(ctx, "system", f.ch.ID, "gamma"); err != nil {
		t.Fatal(err)
	}
	// Alice's seat in another channel's net must survive.
	if _, err := f.s.AddChannelMember(ctx, "system", f.other.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	otherNet, err := f.s.CreateNet(ctx, "system", f.other.ID, "alpha", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutNetMember(ctx, "system", f.other.ID, "alpha", f.alice.ID, schema.NetRoleMember, 0); err != nil {
		t.Fatal(err)
	}

	if removed, err := f.s.RemoveChannelMember(ctx, "principal:7", f.ch.ID, f.alice.ID); err != nil || !removed {
		t.Fatalf("RemoveChannelMember = %v, %v", removed, err)
	}
	var seats int
	if err := f.s.db.QueryRow(
		`SELECT COUNT(*) FROM net_members m JOIN nets n ON n.id = m.net_id
		 WHERE n.channel_id = ? AND m.principal_id = ?`, f.ch.ID, f.alice.ID).Scan(&seats); err != nil || seats != 0 {
		t.Errorf("alice's net seats in the channel = %d (%v), want 0 (archived nets included)", seats, err)
	}
	if nets, _ := f.s.ListNetsForPrincipal(ctx, f.ch.ID, f.alice.ID); len(nets) != 0 {
		t.Errorf("alice still on nets %v", netIDs(nets))
	}
	if ms, _ := f.s.ListNetMembers(ctx, otherNet.ID); len(ms) != 1 {
		t.Errorf("another channel's net roster = %+v, want alice kept", ms)
	}
	alpha, _ := f.s.NetByName(ctx, f.ch.ID, "alpha")
	if ms, _ := f.s.ListNetMembers(ctx, alpha.ID); len(ms) != 1 || ms[0].PrincipalID != f.bob.ID {
		t.Errorf("alpha roster = %+v, want only bob", ms)
	}

	// Each removal is audited, as the actor that removed the channel member.
	evs, err := f.s.ListAuditEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	removals := 0
	for _, e := range evs {
		if e.Action == AuditNetMemberRemoved {
			removals++
			if e.Actor != "principal:7" {
				t.Errorf("cascade audit actor = %q", e.Actor)
			}
		}
	}
	if removals != 3 {
		t.Errorf("net_member_removed events = %d, want 3", removals)
	}

	// Removing a non-member again cascades nothing and audits nothing.
	if removed, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.alice.ID); err != nil || removed {
		t.Fatalf("repeat RemoveChannelMember = %v, %v", removed, err)
	}
	if got := auditCount(t, f.s, AuditNetMemberRemoved); got != 3 {
		t.Errorf("net_member_removed events after repeat = %d, want 3", got)
	}
	// The channel-member row and its net seats go together: a rejoin starts clean.
	if _, err := f.s.AddChannelMember(ctx, "system", f.ch.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	if nets, _ := f.s.ListNetsForPrincipal(ctx, f.ch.ID, f.alice.ID); len(nets) != 0 {
		t.Errorf("rejoined alice is on nets %v", netIDs(nets))
	}
}

func TestNetsMigrationFromSchema10(t *testing.T) {
	const pre = 10
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pre; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'human', 'sam', 'member', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, NULL, 4)`,
		`PRAGMA user_version = 10`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer func() { _ = s.Close() }()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 11 {
		t.Errorf("user_version = %d (%v), want %d", version, err, len(migrations))
	}
	if nets, err := s.ListNets(ctx, 1); err != nil || nets == nil || len(nets) != 0 {
		t.Errorf("ListNets after migration = %#v, %v; want none", nets, err)
	}
	if nets, err := s.ListNetsForPrincipal(ctx, 1, 2); err != nil || len(nets) != 0 {
		t.Errorf("ListNetsForPrincipal after migration = %v, %v", nets, err)
	}
	// Existing data is intact and nets work on it.
	if ok, err := s.IsChannelMember(ctx, 1, 2); err != nil || !ok {
		t.Errorf("channel member lost in migration: %v, %v", ok, err)
	}
	n, err := s.CreateNet(ctx, "system", 1, "alpha", 1)
	if err != nil {
		t.Fatalf("CreateNet after migration: %v", err)
	}
	if _, err := s.PutNetMember(ctx, "system", 1, "alpha", 2, schema.NetRoleMonitor, 1); err != nil {
		t.Fatalf("PutNetMember after migration: %v", err)
	}
	if ms, err := s.ListNetMembers(ctx, n.ID); err != nil || len(ms) != 1 {
		t.Errorf("roster = %+v, %v", ms, err)
	}
}

// The store refuses a net name the schema would not accept, whoever calls it:
// the name goes into the audit detail, which is parsed as key=value text.
func TestCreateNetRejectsInvalidNames(t *testing.T) {
	f := newNetFixture(t)
	ctx := context.Background()
	for _, name := range []string{"", "Alpha", "two words", "new\nline", "a=b", "-lead", strings.Repeat("x", 33)} {
		if _, err := f.s.CreateNet(ctx, "system", f.ch.ID, name, f.alice.ID); err == nil {
			t.Errorf("CreateNet(%q) succeeded", name)
		}
	}
	nets, err := f.s.ListNets(ctx, f.ch.ID)
	if err != nil || len(nets) != 0 {
		t.Errorf("nets after refused creates = %v, %v; want none", nets, err)
	}
	events, err := f.s.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == AuditNetCreated {
			t.Errorf("a refused create was audited: %+v", e)
		}
	}
}
