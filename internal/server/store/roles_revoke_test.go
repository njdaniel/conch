package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// seedPreOperatorCredentials makes two live credentials (one with a future
// expiry) and one already-revoked credential, returning their tokens and the
// revoked one's id and original revoked_at.
func seedPreOperatorCredentials(t *testing.T, s *Store) (live1, live2, dead string, deadID, deadRevokedAt int64) {
	t.Helper()
	ctx := context.Background()
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "old-agent")
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	if _, live1, err = s.CreateCredential(ctx, "system", agent.ID, "live-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, live2, err = s.CreateCredential(ctx, "system", agent.ID, "live-2", &exp); err != nil {
		t.Fatal(err)
	}
	c, dead, err := s.CreateCredential(ctx, "system", agent.ID, "dead", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "system", c.ID); err != nil {
		t.Fatal(err)
	}
	return live1, live2, dead, c.ID, revokedAtMillis(t, s, c.ID)
}

func TestBootstrapOperatorRevokesExistingCredentials(t *testing.T) {
	tests := []struct {
		name        string
		keep        bool
		wantRevoked int
	}{
		{"default revokes live credentials", false, 2},
		{"keep flag leaves them", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			live1, live2, dead, deadID, deadAt := seedPreOperatorCredentials(t, s)
			eventsBefore := countAudit(t, s, "credential_revoked")

			res, err := s.BootstrapOperatorWith(ctx, "root", BootstrapOptions{KeepExistingCredentials: tt.keep})
			if err != nil {
				t.Fatal(err)
			}
			if res.Revoked != tt.wantRevoked {
				t.Errorf("Revoked = %d, want %d", res.Revoked, tt.wantRevoked)
			}
			for name, tok := range map[string]string{"live-1": live1, "live-2": live2} {
				_, err := s.ResolveCredential(ctx, tok)
				if tt.keep && err != nil {
					t.Errorf("%s no longer resolves with the keep flag: %v", name, err)
				}
				if !tt.keep && !errors.Is(err, ErrCredentialInvalid) {
					t.Errorf("%s still resolves after bootstrap: %v", name, err)
				}
			}
			if _, err := s.ResolveCredential(ctx, dead); !errors.Is(err, ErrCredentialInvalid) {
				t.Errorf("dead credential resolves: %v", err)
			}
			if got := revokedAtMillis(t, s, deadID); got != deadAt {
				t.Errorf("already-revoked credential revoked_at changed %d -> %d", deadAt, got)
			}
			if p, err := s.ResolveCredential(ctx, res.Token); err != nil || p.Role != RoleOperator {
				t.Errorf("operator token = %+v, %v", p, err)
			}
			if got := countAudit(t, s, "credential_revoked") - eventsBefore; got != tt.wantRevoked {
				t.Errorf("credential_revoked events added = %d, want %d", got, tt.wantRevoked)
			}
			var n int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_events
				WHERE action = 'credential_revoked' AND actor = 'system' AND detail LIKE '%bootstrap%'`).Scan(&n); err != nil || n != tt.wantRevoked {
				t.Errorf("system bootstrap revocation events = %d (%v), want %d", n, err, tt.wantRevoked)
			}
		})
	}
}

// A refused bootstrap must not revoke anything.
func TestRefusedBootstrapRevokesNothing(t *testing.T) {
	ctx := context.Background()

	s := openTestStore(t)
	if _, err := s.BootstrapOperatorWith(ctx, "root", BootstrapOptions{}); err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "later")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := s.CreateCredential(ctx, "system", agent.ID, "later", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootstrapOperatorWith(ctx, "second", BootstrapOptions{}); !errors.Is(err, ErrOperatorExists) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.ResolveCredential(ctx, tok); err != nil {
		t.Errorf("a refused bootstrap revoked a credential: %v", err)
	}

	s2 := openTestStore(t)
	live1, _, _, _, _ := seedPreOperatorCredentials(t, s2)
	if _, err := s2.BootstrapOperatorWith(ctx, "old-agent", BootstrapOptions{}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s2.ResolveCredential(ctx, live1); err != nil {
		t.Errorf("a name-collision refusal revoked a credential: %v", err)
	}
}

// seedPreOperatorHooks creates a channel, an agent with a manifest, and two
// webhook hooks, returning the hook tokens.
func seedPreOperatorHooks(t *testing.T, s *Store) []string {
	t.Helper()
	ctx := context.Background()
	channel, err := s.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "hooked-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutAgentManifest(ctx, "system", agent.ID, schema.PutAgentManifestRequestV1{DisplayName: "Hooked", Tier: schema.AgentTierC}); err != nil {
		t.Fatal(err)
	}
	tokens := []string{"hook-token-one", "hook-token-two"}
	for _, token := range tokens {
		if _, err := s.CreateHook(ctx, "system", token, channel.ID, agent.ID); err != nil {
			t.Fatal(err)
		}
	}
	return tokens
}

func TestBootstrapOperatorDeletesExistingHooks(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		opts      BootstrapOptions
		wantHooks int
		wantLive  bool
	}{
		{"deleted by default", BootstrapOptions{}, 2, false},
		{"kept with the opt-out", BootstrapOptions{KeepExistingCredentials: true}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			tokens := seedPreOperatorHooks(t, s)
			res, err := s.BootstrapOperatorWith(ctx, "root", tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if res.RevokedHooks != tt.wantHooks {
				t.Errorf("RevokedHooks = %d, want %d", res.RevokedHooks, tt.wantHooks)
			}
			if res.Manifests != 1 {
				t.Errorf("Manifests = %d, want 1 (manifests are counted, never removed)", res.Manifests)
			}
			if _, err := s.AgentManifestByPrincipal(ctx, 1); err != nil {
				t.Errorf("the pre-existing manifest was removed: %v", err)
			}
			for _, token := range tokens {
				_, err := s.HookByToken(ctx, token)
				if live := err == nil; live != tt.wantLive {
					t.Errorf("hook resolves = %v (err %v), want %v", live, err, tt.wantLive)
				}
			}
			events, err := s.ListAuditEvents(ctx, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var revoked int
			for _, e := range events {
				for _, token := range tokens {
					if strings.Contains(e.Actor+e.Action+e.Subject+e.Detail, token) {
						t.Errorf("audit event %q contains a hook token", e.Action)
					}
				}
				if e.Action == "hook_revoked" {
					revoked++
					if e.Actor != "system" || e.Subject != "principal:1" || !strings.Contains(e.Detail, "revoked at bootstrap") {
						t.Errorf("hook_revoked event = %+v", e)
					}
				}
			}
			if revoked != tt.wantHooks {
				t.Errorf("hook_revoked events = %d, want %d", revoked, tt.wantHooks)
			}
		})
	}
}

func TestRefusedBootstrapKeepsHooks(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.BootstrapOperatorWith(ctx, "root", BootstrapOptions{}); err != nil {
		t.Fatal(err)
	}
	tokens := seedPreOperatorHooks(t, s)
	if _, err := s.BootstrapOperatorWith(ctx, "second", BootstrapOptions{}); !errors.Is(err, ErrOperatorExists) {
		t.Fatalf("err = %v", err)
	}
	for _, token := range tokens {
		if _, err := s.HookByToken(ctx, token); err != nil {
			t.Errorf("a refused bootstrap deleted a hook: %v", err)
		}
	}
}
