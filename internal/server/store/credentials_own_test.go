package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A person's own sign-ins (issue #496): the live ones listed, and one ended
// by id, never another's.

func TestListLiveCredentials(t *testing.T) {
	s, agent, human := credentialFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	setCredentialClock(t, &now)

	empty, err := s.ListLiveCredentials(ctx, human.ID)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v, %v; want non-nil empty", empty, err)
	}
	if got, err := s.ListLiveCredentials(ctx, 9999); err != nil || len(got) != 0 {
		t.Errorf("unknown principal = %#v, %v; want none", got, err)
	}

	soon := now.Add(time.Hour)
	fixed, _, err := s.CreateCredential(ctx, "system", human.ID, "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	expiring, _, err := s.CreateCredential(ctx, "system", human.ID, "expires soon", &soon)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	revoked, _, err := s.CreateCredential(ctx, "system", human.ID, "revoked", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "system", revoked.ID); err != nil {
		t.Fatal(err)
	}
	renews, _, err := s.CreateRenewableCredential(ctx, "system", human.ID, "invite 1", 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateCredential(ctx, "system", agent.ID, "someone else's", nil); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		at   time.Duration // past the clock as it stands
		want []int64
	}{
		{"every live one, newest first", 0, []int64{renews.ID, expiring.ID, fixed.ID}},
		{"an expired one left out", 2 * time.Hour, []int64{renews.ID, fixed.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := now
			now = now.Add(tt.at)
			defer func() { now = at }()
			got, err := s.ListLiveCredentials(ctx, human.ID)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, c := range got {
				ids = append(ids, c.ID)
				if c.PrincipalID != human.ID || c.RevokedAt != nil {
					t.Errorf("listed %+v: only the principal's live credentials may be", c)
				}
				if c.Renews != (c.ID == renews.ID) {
					t.Errorf("credential %d renews = %v", c.ID, c.Renews)
				}
			}
			if !slices.Equal(ids, tt.want) {
				t.Errorf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestRevokeCredentialOf(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		// pick returns the principal acting and the credential it names,
		// given the person, their credential and the agent's.
		pick    func(person, own, other int64) (int64, int64)
		wantErr error
		// revoked is whether the person's credential is revoked afterwards.
		revoked bool
	}{
		{"one's own", func(p, own, _ int64) (int64, int64) { return p, own }, nil, true},
		{"someone else's", func(p, _, other int64) (int64, int64) { return p, other }, ErrCredentialNotFound, false},
		{"one never issued", func(p, _, _ int64) (int64, int64) { return p, 9999 }, ErrCredentialNotFound, false},
		{"no principal named", func(_, own, _ int64) (int64, int64) { return 0, own }, ErrCredentialNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, agent, human := credentialFixture(t)
			own, _, err := s.CreateCredential(ctx, "system", human.ID, "phone", nil)
			if err != nil {
				t.Fatal(err)
			}
			other, _, err := s.CreateCredential(ctx, "system", agent.ID, "bot", nil)
			if err != nil {
				t.Fatal(err)
			}
			who, id := tt.pick(human.ID, own.ID, other.ID)
			err = s.RevokeCredentialOf(ctx, "principal:test", who, id)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("RevokeCredentialOf = %v, want %v", err, tt.wantErr)
			}
			live, err := s.ListLiveCredentials(ctx, human.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gone := len(live) == 0; gone != tt.revoked {
				t.Errorf("own credential revoked = %v, want %v", gone, tt.revoked)
			}
			if bot, err := s.ListLiveCredentials(ctx, agent.ID); err != nil || len(bot) != 1 {
				t.Errorf("the other principal's credentials = %+v, %v; want untouched", bot, err)
			}
			details := auditDetails(t, s, "credential_revoked")
			if tt.revoked {
				if len(details) != 1 || !strings.HasSuffix(details[0], " reason=self") {
					t.Errorf("credential_revoked details = %q, want one with reason=self", details)
				}
			} else if len(details) != 0 {
				t.Errorf("credential_revoked details = %q, want none", details)
			}
		})
	}
}

// Ending a sign-in that has already ended changes nothing and audits nothing
// more, as an operator's revoke of one does.
func TestRevokeCredentialOfTwice(t *testing.T) {
	s, _, human := credentialFixture(t)
	ctx := context.Background()
	c, _, err := s.CreateCredential(ctx, "system", human.ID, "phone", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := s.RevokeCredentialOf(ctx, "principal:test", human.ID, c.ID); err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}
	if n := countAudit(t, s, "credential_revoked"); n != 1 {
		t.Errorf("credential_revoked events = %d, want 1", n)
	}
}

// The last operator's last credential cannot be ended by id either.
func TestRevokeCredentialOfKeepsTheLastOperator(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	op, cred, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredentialOf(ctx, "principal:test", op.ID, cred.ID); !errors.Is(err, ErrLastOperator) {
		t.Fatalf("RevokeCredentialOf = %v, want ErrLastOperator", err)
	}
	second, _, err := s.CreateCredential(ctx, "system", op.ID, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredentialOf(ctx, "principal:test", op.ID, cred.ID); err != nil {
		t.Fatalf("with a second credential: %v", err)
	}
	if err := s.RevokeCredentialOf(ctx, "principal:test", op.ID, second.ID); !errors.Is(err, ErrLastOperator) {
		t.Fatalf("the second, now the last = %v, want ErrLastOperator", err)
	}
}

// auditDetails returns the detail of every audit event with this action.
func auditDetails(t *testing.T, s *Store, action string) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT detail FROM audit_events WHERE action = ? ORDER BY id", action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
