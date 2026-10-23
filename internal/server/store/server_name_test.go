package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The server's name as a reserved permanent name (issue #760): SetServerName,
// claimNameTx's check and ServerNameHolders.

// The server's name is refused on every write of a permanent name, by folded
// key and by skeleton, whoever writes it, and nothing is written: the default
// name and a configured one, each exactly, in another case and spelt with
// look-alike characters.
func TestServerNameReserved(t *testing.T) {
	spellings := []struct{ server, name string }{
		{"conch", "conch"},
		{"conch", "CONCH"},
		{"conch", "c0nch"}, // a zero for the o
		{"conch", "соncһ"}, // Cyrillic с, о and һ
		{"Harbour", "Harbour"},
		{"Harbour", "hArBoUr"},
		{"Harbour", "Harbоur"}, // a Cyrillic о
	}
	// Each path makes what its write needs and returns the write, so the
	// rows are counted after the setup and before the write.
	type write func(name string) error
	paths := []struct {
		name    string
		prepare func(t *testing.T, f *guestFixture, inv *inviteFixture, server string) write
	}{
		{"create", func(_ *testing.T, f *guestFixture, _ *inviteFixture, _ string) write {
			return func(name string) error {
				_, err := f.s.CreatePrincipalAs(f.ctx, "system", PrincipalHuman, name)
				return err
			}
		}},
		{"create agent", func(_ *testing.T, f *guestFixture, _ *inviteFixture, _ string) write {
			return func(name string) error {
				_, err := f.s.CreatePrincipalAs(f.ctx, "system", PrincipalAgent, name)
				return err
			}
		}},
		{"own rename", func(_ *testing.T, f *guestFixture, _ *inviteFixture, _ string) write {
			return func(name string) error {
				_, err := f.s.RenamePrincipal(f.ctx, principalActor(f.alice.ID), f.alice.ID, name,
					RenameLimits{Renames: 100, Attempts: 100}, time.Now())
				return err
			}
		}},
		{"operator rename", func(_ *testing.T, f *guestFixture, _ *inviteFixture, _ string) write {
			return func(name string) error {
				_, err := f.s.RenamePrincipal(f.ctx, principalActor(f.op.ID), f.alice.ID, name, RenameLimits{}, time.Now())
				return err
			}
		}},
		{"v1 invite redemption", func(_ *testing.T, _ *guestFixture, inv *inviteFixture, _ string) write {
			code := inv.mustCreate(inv.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}).Code
			return func(name string) error {
				_, err := inv.redeem(code, name)
				return err
			}
		}},
		{"v2 invite redemption", func(_ *testing.T, f *guestFixture, inv *inviteFixture, _ string) write {
			code := inv.mustCreate(inv.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}).Code
			return func(name string) error {
				_, err := f.s.RedeemInviteWithPassword(f.ctx, HashInviteCode(code), name, testRecord("v2"), inv.now)
				return err
			}
		}},
		{"account from the bare address", func(_ *testing.T, f *guestFixture, inv *inviteFixture, _ string) write {
			return func(name string) error {
				_, err := f.s.JoinAsAccount(f.ctx, name, testRecord("join"), inv.now)
				return err
			}
		}},
		{"guest conversion", func(_ *testing.T, f *guestFixture, _ *inviteFixture, _ string) write {
			g := f.guest("")
			return func(name string) error {
				_, err := f.s.ConvertGuest(f.ctx, principalActor(g.Principal.ID), GuestConversion{Record: testRecord("conv"),
					PrincipalID: g.Principal.ID, Name: name, CredentialLifetime: time.Hour})
				return err
			}
		}},
		{"bootstrap", func(t *testing.T, _ *guestFixture, _ *inviteFixture, server string) write {
			// The first operator needs a store with none, given the name as
			// the fixture's is; the refusal leaves it empty.
			s := openTestStore(t)
			s.SetServerName(server)
			return func(name string) error {
				_, err := s.BootstrapOperatorWith(context.Background(), name, BootstrapOptions{})
				var n int
				if qerr := s.db.QueryRow("SELECT COUNT(*) FROM principals").Scan(&n); qerr != nil || n != 0 {
					t.Errorf("%d principals after a refused bootstrap (%v)", n, qerr)
				}
				return err
			}
		}},
		{"name check", func(_ *testing.T, f *guestFixture, _ *inviteFixture, _ string) write {
			return func(name string) error { return f.s.CheckNameFree(f.ctx, name, 0) }
		}},
	}
	for _, path := range paths {
		for _, sp := range spellings {
			t.Run(fmt.Sprintf("%s/%s/%+q", path.name, sp.server, sp.name), func(t *testing.T) {
				f := newGuestFixture(t)
				inv := &inviteFixture{t: t, s: f.s, ctx: f.ctx, now: time.Now().Truncate(time.Millisecond), op: f.op}
				inv.putJoin(schema.PutJoinSettingsRequestV1{OpenJoining: ptrMode(schema.OpenJoiningAccount), OpenChannels: &[]string{"general"}})
				f.s.SetServerName(sp.server)
				w := path.prepare(t, f, inv, sp.server)
				principals, audit := f.count("SELECT COUNT(*) FROM principals"), f.count("SELECT COUNT(*) FROM audit_events")
				err := w(sp.name)
				if !errors.Is(err, ErrServerNameReserved) || !errors.Is(err, ErrNameReserved) || !errors.Is(err, ErrDuplicate) {
					t.Fatalf("err = %v, want the server name's reservation (also ErrNameReserved and ErrDuplicate)", err)
				}
				if n := f.count("SELECT COUNT(*) FROM principals WHERE name = ?", sp.name); n != 0 {
					t.Errorf("%d principals called %q after the refusal", n, sp.name)
				}
				if n := f.count("SELECT COUNT(*) FROM principals"); n != principals {
					t.Errorf("a refusal made %d principals", n-principals)
				}
				if n := f.count("SELECT COUNT(*) FROM audit_events"); n != audit {
					t.Errorf("a refusal wrote %d audit rows", n-audit)
				}
				if p, err := f.s.PrincipalByID(f.ctx, f.alice.ID); err != nil || p.Name != "alice" {
					t.Errorf("alice = %+v, %v; want her name kept", p, err)
				}
			})
		}
	}

	// The server's name is one name, not a namespace: names that contain it
	// are anyone's. With no server name set, as in the offline commands,
	// nothing is reserved.
	ctx := context.Background()
	s := openTestStore(t)
	s.SetServerName("conch")
	for _, name := range []string{"conchs", "conch-bot", "the conch", "conk"} {
		if _, err := s.CreatePrincipal(ctx, PrincipalHuman, name); err != nil {
			t.Errorf("create %q: %v", name, err)
		}
	}
	s.SetServerName("")
	if _, err := s.CreatePrincipal(ctx, PrincipalHuman, "Conch"); err != nil {
		t.Errorf("create Conch with no server name set: %v", err)
	}
	if ids, err := s.ServerNameHolders(ctx); err != nil || len(ids) != 0 {
		t.Errorf("holders with no server name set = %v, %v; want none", ids, err)
	}
}

// A principal that holds the server's name when it is reserved, from before
// the reservation or before the server's name changed to it, keeps it: it is
// not renamed, it still signs in and renames, and the store reports it until
// it is renamed away. A guest is never counted.
func TestServerNameHolders(t *testing.T) {
	f := newGuestFixture(t)
	ctx := f.ctx
	holder, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "Harbour")
	if err != nil {
		t.Fatal(err)
	}
	f.guest("") // a live guest
	f.s.SetServerName("harbour")

	ids, err := f.s.ServerNameHolders(ctx)
	if err != nil || len(ids) != 1 || ids[0] != holder.ID {
		t.Fatalf("holders = %v, %v; want [%d]", ids, err, holder.ID)
	}
	st, err := f.s.JoinState(ctx)
	if err != nil || st.ServerNameHolders != 1 || st.ReservedNames != 0 {
		t.Errorf("join state = %+v, %v; want one holder and no reserved guest names", st, err)
	}
	if p, err := f.s.PrincipalByID(ctx, holder.ID); err != nil || p.Name != "Harbour" {
		t.Errorf("holder = %+v, %v; want its name kept", p, err)
	}
	_, tok, err := f.s.CreateCredential(ctx, "system", holder.ID, "laptop", nil)
	if err != nil {
		t.Fatalf("a credential for the holder: %v", err)
	}
	if p, err := f.s.ResolveCredential(ctx, tok); err != nil || p.ID != holder.ID {
		t.Errorf("the holder's token = %+v, %v; want it to resolve", p, err)
	}
	// Renaming to its own name is no rename; another spelling of the
	// server's name is refused to it as to anyone.
	if res, err := f.s.RenamePrincipal(ctx, principalActor(holder.ID), holder.ID, "Harbour", RenameLimits{}, time.Now()); err != nil || res.Changed {
		t.Errorf("rename to its own name = %+v, %v; want no change", res, err)
	}
	if _, err := f.s.RenamePrincipal(ctx, principalActor(holder.ID), holder.ID, "HARBOUR", RenameLimits{}, time.Now()); !errors.Is(err, ErrServerNameReserved) {
		t.Errorf("rename to HARBOUR = %v, want the server name's reservation", err)
	}
	if _, err := f.s.RenamePrincipal(ctx, principalActor(holder.ID), holder.ID, "harbourmaster", RenameLimits{}, time.Now()); err != nil {
		t.Fatalf("rename away: %v", err)
	}
	if ids, err := f.s.ServerNameHolders(ctx); err != nil || len(ids) != 0 {
		t.Errorf("holders after the rename = %v, %v; want none", ids, err)
	}
	if st, err := f.s.JoinState(ctx); err != nil || st.ServerNameHolders != 0 {
		t.Errorf("join state after the rename = %+v, %v", st, err)
	}
}
