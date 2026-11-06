package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func promoteToOwner(t *testing.T, s *Store, actorID, targetID int64) {
	t.Helper()
	mark := true
	if _, err := s.SetRole(context.Background(), principalActor(actorID), actorID, targetID, RoleOperator, &mark); err != nil {
		t.Fatalf("promote principal %d to owner: %v", targetID, err)
	}
}

func newOwnerWriteFixture(t *testing.T) (*Store, Principal, Principal) {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	root, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.CreatePrincipal(ctx, PrincipalHuman, "target")
	if err != nil {
		t.Fatal(err)
	}
	promoteToOwner(t, s, root.ID, target.ID)
	return s, root, target
}

func TestOwnerProtectedWritesRecheckTargetInTransaction(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *Store, Principal, Principal)
		call    func(*Store, Principal, Principal) error
		verify  func(*testing.T, *Store, Principal, Principal)
	}{
		{
			name: "disable",
			call: func(s *Store, _ Principal, target Principal) error {
				_, err := s.DisablePrincipal(context.Background(), "system", target.ID, true)
				return err
			},
		},
		{
			name: "enable",
			prepare: func(t *testing.T, s *Store, _ Principal, target Principal) {
				if _, err := s.DisablePrincipal(context.Background(), "system", target.ID); err != nil {
					t.Fatal(err)
				}
			},
			call: func(s *Store, _ Principal, target Principal) error {
				_, err := s.EnablePrincipal(context.Background(), "system", target.ID, true)
				return err
			},
		},
		{
			name: "revoke all credentials",
			prepare: func(t *testing.T, s *Store, _ Principal, target Principal) {
				if _, _, err := s.CreateCredential(context.Background(), "system", target.ID, "still-live", nil); err != nil {
					t.Fatal(err)
				}
			},
			call: func(s *Store, _ Principal, target Principal) error {
				_, err := s.RevokeAllCredentials(context.Background(), "system", target.ID, true)
				return err
			},
		},
		{
			name: "rename",
			call: func(s *Store, _ Principal, target Principal) error {
				_, err := s.RenamePrincipal(context.Background(), "system", target.ID, "renamed", RenameLimits{}, time.Now(), true)
				return err
			},
		},
		{
			name: "hide",
			call: func(s *Store, _ Principal, target Principal) error {
				_, err := s.HidePrincipal(context.Background(), "system", target.ID, HideOptions{ForbidOwner: true})
				return err
			},
		},
		{
			name: "remove net member",
			prepare: func(t *testing.T, s *Store, root Principal, target Principal) {
				ctx := context.Background()
				ch, err := s.CreateChannel(ctx, "general")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddChannelMember(ctx, "system", ch.ID, target.ID, root.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.CreateNet(ctx, "system", ch.ID, "crew", root.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PutNetMember(ctx, "system", ch.ID, "crew", target.ID, schema.NetRoleMember, root.ID); err != nil {
					t.Fatal(err)
				}
			},
			call: func(s *Store, _ Principal, target Principal) error {
				ctx := context.Background()
				ch, err := s.ChannelByName(ctx, "general")
				if err != nil {
					return err
				}
				_, err = s.RemoveNetMember(ctx, "system", ch.ID, "crew", target.ID, true)
				return err
			},
			verify: func(t *testing.T, s *Store, _ Principal, target Principal) {
				ch, err := s.ChannelByName(context.Background(), "general")
				if err != nil {
					t.Fatal(err)
				}
				nets, err := s.ListNets(context.Background(), ch.ID)
				if err != nil || len(nets) != 1 {
					t.Fatalf("ListNets = %+v, %v", nets, err)
				}
				members, err := s.ListNetMembers(context.Background(), nets[0].ID)
				if err != nil || len(members) != 1 || members[0].PrincipalID != target.ID {
					t.Errorf("net members = %+v, %v; want the promoted owner still present", members, err)
				}
			},
		},
		{
			name: "invite permission",
			prepare: func(t *testing.T, s *Store, _ Principal, target Principal) {
				if _, err := s.SetMayInvite(context.Background(), "system", target.ID, true); err != nil {
					t.Fatal(err)
				}
			},
			call: func(s *Store, _ Principal, target Principal) error {
				_, err := s.SetMayInvite(context.Background(), "system", target.ID, false, true)
				return err
			},
		},
		{
			name: "create sign-in link",
			call: func(s *Store, root Principal, target Principal) error {
				_, err := s.CreateSignInLink(context.Background(), "system", root.ID, target.ID, schema.CreateSignInLinkRequestV1{}, time.Now(), true)
				return err
			},
			verify: func(t *testing.T, s *Store, _ Principal, _ Principal) {
				if n := countRows(t, s, "sign_in_links"); n != 0 {
					t.Errorf("sign-in links = %d, want 0", n)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, root, target := newOwnerWriteFixture(t)
			ctx := context.Background()
			if tt.prepare != nil {
				tt.prepare(t, s, root, target)
			}
			before, err := s.PrincipalByID(ctx, target.ID)
			if err != nil || !before.IsOwner() {
				t.Fatalf("target before write = %+v, %v; want an owner", before, err)
			}
			auditBefore, err := s.LastAuditID(ctx)
			if err != nil {
				t.Fatal(err)
			}

			if err := tt.call(s, root, target); !errors.Is(err, ErrOwnerProtected) {
				t.Fatalf("write error = %v, want ErrOwnerProtected", err)
			}

			after, err := s.PrincipalByID(ctx, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("refused write changed the principal:\n before %+v\n after  %+v", before, after)
			}
			auditAfter, err := s.LastAuditID(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if auditAfter != auditBefore {
				t.Errorf("refused write appended audit rows: last id %d -> %d", auditBefore, auditAfter)
			}
			if tt.verify != nil {
				tt.verify(t, s, root, target)
			}
		})
	}
}

func TestOwnerProtectedWritesPreserveNotFoundAndLastOwnerSemantics(t *testing.T) {
	ctx := context.Background()
	t.Run("unknown principals", func(t *testing.T) {
		s, root, _ := newOwnerWriteFixture(t)
		const missing = int64(999999)
		calls := []struct {
			name string
			call func() error
		}{
			{"disable", func() error { _, err := s.DisablePrincipal(ctx, "system", missing, true); return err }},
			{"enable", func() error { _, err := s.EnablePrincipal(ctx, "system", missing, true); return err }},
			{"revoke all", func() error { _, err := s.RevokeAllCredentials(ctx, "system", missing, true); return err }},
			{"rename", func() error {
				_, err := s.RenamePrincipal(ctx, "system", missing, "new-name", RenameLimits{}, time.Now(), true)
				return err
			}},
			{"hide", func() error {
				_, err := s.HidePrincipal(ctx, "system", missing, HideOptions{ForbidOwner: true})
				return err
			}},
			{"invite permission", func() error { _, err := s.SetMayInvite(ctx, "system", missing, true, true); return err }},
			{"sign-in link", func() error {
				_, err := s.CreateSignInLink(ctx, "system", root.ID, missing, schema.CreateSignInLinkRequestV1{}, time.Now(), true)
				return err
			}},
		}
		for _, call := range calls {
			t.Run(call.name, func(t *testing.T) {
				if err := call.call(); !errors.Is(err, ErrPrincipalNotFound) {
					t.Errorf("error = %v, want ErrPrincipalNotFound", err)
				}
			})
		}
	})

	t.Run("unknown net and absent net member", func(t *testing.T) {
		s, root, target := newOwnerWriteFixture(t)
		ch, err := s.CreateChannel(ctx, "general")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RemoveNetMember(ctx, "system", ch.ID, "missing", target.ID, true); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown net error = %v, want ErrNotFound", err)
		}
		if _, err := s.CreateNet(ctx, "system", ch.ID, "crew", root.ID); err != nil {
			t.Fatal(err)
		}
		removed, err := s.RemoveNetMember(ctx, "system", ch.ID, "crew", 999999, true)
		if err != nil || removed {
			t.Errorf("absent member removal = %v, %v; want false, nil", removed, err)
		}
	})

	t.Run("owner protection precedes last owner guard", func(t *testing.T) {
		s := openTestStore(t)
		root, _, _, err := s.BootstrapOperator(ctx, "root")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DisablePrincipal(ctx, "system", root.ID, true); !errors.Is(err, ErrOwnerProtected) {
			t.Errorf("disable sole owner = %v, want ErrOwnerProtected", err)
		}
		if _, err := s.RevokeAllCredentials(ctx, "system", root.ID, true); !errors.Is(err, ErrOwnerProtected) {
			t.Errorf("revoke sole owner's credentials = %v, want ErrOwnerProtected", err)
		}
	})
}
