package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Any change of role clears every recent-auth proof of the principal, in
// the transaction that changes it, and counts them in the audit row; a call
// that changes nothing and another principal's credentials are left alone
// (issue #1054).
func TestSetRoleClearsRecentAuthProofs(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		startRole  Role
		role       Role
		wantClear  bool
		wantDetail string
	}{
		{"promotion", RoleMember, RoleOperator, true, "proofs_cleared=2"},
		{"demotion", RoleOperator, RoleMember, true, "proofs_cleared=2"},
		{"no change", RoleMember, RoleMember, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			owner, _, _, err := s.BootstrapOperator(ctx, "owner1")
			if err != nil {
				t.Fatal(err)
			}
			target, err := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
			if err != nil {
				t.Fatal(err)
			}
			bystander, err := s.CreatePrincipal(ctx, PrincipalHuman, "carol")
			if err != nil {
				t.Fatal(err)
			}
			actor := principalActor(owner.ID)
			if tt.startRole == RoleOperator {
				if _, err := s.SetRole(ctx, actor, owner.ID, target.ID, RoleOperator, nil); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().Truncate(time.Millisecond)
			until := now.Add(schema.RecentAuthLifetimeSeconds * time.Second)
			var creds []int64
			for i := 0; i < 2; i++ {
				c, _, err := s.CreateCredential(ctx, "system", target.ID, "c", nil)
				if err != nil {
					t.Fatal(err)
				}
				creds = append(creds, c.ID)
			}
			other, _, err := s.CreateCredential(ctx, "system", bystander.ID, "c", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range append(creds, other.ID) {
				if _, err := s.db.ExecContext(ctx, "UPDATE credentials SET recent_auth_until = ? WHERE id = ?", until.UnixMilli(), id); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := s.SetRole(ctx, actor, owner.ID, target.ID, tt.role, nil); err != nil {
				t.Fatalf("SetRole = %v", err)
			}
			for _, id := range creds {
				got, err := s.RecentAuthStands(ctx, id, now)
				if err != nil {
					t.Fatalf("RecentAuthStands(%d) = %v", id, err)
				}
				if got == tt.wantClear {
					t.Errorf("credential %d: proof stands = %v after the change, want %v", id, got, !tt.wantClear)
				}
			}
			if ok, err := s.RecentAuthStands(ctx, other.ID, now); err != nil || !ok {
				t.Errorf("another principal's proof = %v, %v; want it standing", ok, err)
			}
			if tt.wantDetail == "" {
				return
			}
			var found bool
			for _, e := range allAuditEvents(t, s) {
				if e.Action == AuditPrincipalRoleChanged && e.Subject == principalActor(target.ID) && strings.Contains(e.Detail, tt.wantDetail) {
					found = true
				}
			}
			if !found {
				t.Errorf("no principal_role_changed row for the target with %q", tt.wantDetail)
			}
		})
	}
}
