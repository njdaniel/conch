package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type disableFixture struct {
	s     *Store
	op    Principal // operator
	opTok string
	alice Principal
}

func newDisableFixture(t *testing.T) disableFixture {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	op, _, tok, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return disableFixture{s, op, tok, alice}
}

func (f disableFixture) cred(t *testing.T, p Principal, exp *time.Time) (int64, string) {
	t.Helper()
	c, tok, err := f.s.CreateCredential(context.Background(), "system", p.ID, "x", exp)
	if err != nil {
		t.Fatal(err)
	}
	return c.ID, tok
}

func (f disableFixture) actions(t *testing.T) []string {
	t.Helper()
	events, err := f.s.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Action)
	}
	return out
}

func count(xs []string, a string) int {
	n := 0
	for _, x := range xs {
		if x == a {
			n++
		}
	}
	return n
}

func TestDisableEnableLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newDisableFixture(t)
	_, tok1 := f.cred(t, f.alice, nil)
	exp := time.Now().Add(time.Hour)
	_, tok2 := f.cred(t, f.alice, &exp)
	// Created a moment before the disable.
	id3, tok3 := f.cred(t, f.alice, nil)
	if err := f.s.RevokeCredential(ctx, "system", id3); err != nil { // already revoked: must not be re-revoked or audited
		t.Fatal(err)
	}
	_, opTok2 := f.cred(t, f.op, nil)

	changed, err := f.s.DisablePrincipal(ctx, "principal:1", f.alice.ID)
	if err != nil || !changed {
		t.Fatalf("DisablePrincipal = %v, %v", changed, err)
	}
	for i, tok := range []string{tok1, tok2, tok3} {
		if _, err := f.s.ResolveCredential(ctx, tok); !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("token %d after disable: err = %v, want ErrCredentialInvalid", i+1, err)
		}
	}
	if _, err := f.s.ResolveCredential(ctx, opTok2); err != nil {
		t.Errorf("other principal's credential affected: %v", err)
	}
	p, err := f.s.PrincipalByID(ctx, f.alice.ID)
	if err != nil || p.DisabledAt == nil {
		t.Fatalf("PrincipalByID = %+v, %v; want DisabledAt set", p, err)
	}
	a := f.actions(t)
	if count(a, "principal_disabled") != 1 {
		t.Errorf("principal_disabled events = %d, want 1 (%v)", count(a, "principal_disabled"), a)
	}
	// tok1 and tok2 were live; tok3 was revoked beforehand. Plus its own
	// revocation event from the explicit RevokeCredential.
	if got := count(a, "credential_revoked"); got != 3 {
		t.Errorf("credential_revoked events = %d, want 3 (2 by disable, 1 earlier)", got)
	}
	events, _ := f.s.ListAuditEvents(ctx, 0, 1000)
	for _, e := range events {
		if e.Action == "principal_disabled" && (e.Actor != "principal:1" || e.Subject != "principal:2") {
			t.Errorf("principal_disabled event = %+v", e)
		}
		if e.Action == "credential_revoked" && strings.HasSuffix(e.Detail, "reason=principal disabled") && e.Actor != "principal:1" {
			t.Errorf("credential_revoked actor = %q", e.Actor)
		}
		for _, tok := range []string{tok1, tok2, tok3} {
			if strings.Contains(e.Detail+e.Actor+e.Subject, tok) || strings.Contains(e.Detail, hashCredentialToken(tok)) {
				t.Errorf("audit event %q contains token material", e.Action)
			}
		}
	}

	// Cannot create or rotate while disabled.
	if _, _, err := f.s.CreateCredential(ctx, "system", f.alice.ID, "n", nil); !errors.Is(err, ErrPrincipalDisabled) {
		t.Errorf("CreateCredential on disabled: %v", err)
	}
	id1 := int64(2)
	if _, _, err := f.s.RotateCredential(ctx, "system", id1); !errors.Is(err, ErrPrincipalDisabled) {
		t.Errorf("RotateCredential on disabled: %v", err)
	}

	// Idempotent: no change, no extra audit.
	before := len(f.actions(t))
	changed, err = f.s.DisablePrincipal(ctx, "principal:1", f.alice.ID)
	if err != nil || changed {
		t.Errorf("second disable = %v, %v; want unchanged", changed, err)
	}
	if after := len(f.actions(t)); after != before {
		t.Errorf("second disable wrote %d audit events", after-before)
	}

	// Enable revives nothing.
	changed, err = f.s.EnablePrincipal(ctx, "principal:1", f.alice.ID)
	if err != nil || !changed {
		t.Fatalf("EnablePrincipal = %v, %v", changed, err)
	}
	for i, tok := range []string{tok1, tok2, tok3} {
		if _, err := f.s.ResolveCredential(ctx, tok); !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("token %d after enable: err = %v, want still invalid", i+1, err)
		}
	}
	_, fresh := f.cred(t, f.alice, nil)
	if got, err := f.s.ResolveCredential(ctx, fresh); err != nil || got.ID != f.alice.ID || got.DisabledAt != nil {
		t.Errorf("fresh credential after enable = %+v, %v", got, err)
	}
	before = len(f.actions(t))
	if changed, err := f.s.EnablePrincipal(ctx, "principal:1", f.alice.ID); err != nil || changed {
		t.Errorf("second enable = %v, %v", changed, err)
	}
	if after := len(f.actions(t)); after != before {
		t.Errorf("second enable wrote audit events")
	}
	if count(f.actions(t), "principal_enabled") != 1 {
		t.Errorf("principal_enabled events != 1")
	}
}

func TestDisableEnableUnknownPrincipal(t *testing.T) {
	f := newDisableFixture(t)
	ctx := context.Background()
	if _, err := f.s.DisablePrincipal(ctx, "system", 999); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("disable unknown: %v", err)
	}
	if _, err := f.s.EnablePrincipal(ctx, "system", 999); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("enable unknown: %v", err)
	}
	if _, err := f.s.RevokeAllCredentials(ctx, "system", 999); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("revoke-all unknown: %v", err)
	}
}

func TestRevokeAllCredentials(t *testing.T) {
	ctx := context.Background()
	f := newDisableFixture(t)
	_, tok1 := f.cred(t, f.alice, nil)
	revokedID, tok2 := f.cred(t, f.alice, nil)
	if err := f.s.RevokeCredential(ctx, "system", revokedID); err != nil {
		t.Fatal(err)
	}
	_, tok3 := f.cred(t, f.alice, nil)
	_, opTok := f.cred(t, f.op, nil)

	n, err := f.s.RevokeAllCredentials(ctx, "principal:1", f.alice.ID)
	if err != nil || n != 2 {
		t.Fatalf("RevokeAllCredentials = %d, %v; want 2", n, err)
	}
	for _, tok := range []string{tok1, tok2, tok3} {
		if _, err := f.s.ResolveCredential(ctx, tok); !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("token still resolves: %v", err)
		}
	}
	if _, err := f.s.ResolveCredential(ctx, opTok); err != nil {
		t.Errorf("other principal affected: %v", err)
	}
	a := f.actions(t)
	// 1 earlier explicit + 2 by revoke-all.
	if got := count(a, "credential_revoked"); got != 3 {
		t.Errorf("credential_revoked = %d, want 3", got)
	}
	if count(a, "principal_disabled") != 0 {
		t.Error("revoke-all must not disable")
	}
	// Principal stays enabled and can get a new credential.
	_, fresh := f.cred(t, f.alice, nil)
	if _, err := f.s.ResolveCredential(ctx, fresh); err != nil {
		t.Errorf("fresh credential: %v", err)
	}
	// Second call: nothing left except the fresh one.
	if n, err := f.s.RevokeAllCredentials(ctx, "principal:1", f.alice.ID); err != nil || n != 1 {
		t.Errorf("second revoke-all = %d, %v; want 1", n, err)
	}
	if n, err := f.s.RevokeAllCredentials(ctx, "principal:1", f.alice.ID); err != nil || n != 0 {
		t.Errorf("third revoke-all = %d, %v; want 0", n, err)
	}
	events, _ := f.s.ListAuditEvents(ctx, 0, 1000)
	for _, e := range events {
		if e.Action == "credential_revoked" && strings.Contains(e.Detail, "reason=revoke-all") && e.Actor != "principal:1" {
			t.Errorf("actor = %q", e.Actor)
		}
	}
}

func TestRevokeAllCredentialsAtomic(t *testing.T) {
	// If the transaction fails part-way nothing is revoked: break the audit
	// table so the first audit insert fails.
	ctx := context.Background()
	f := newDisableFixture(t)
	_, tok1 := f.cred(t, f.alice, nil)
	_, tok2 := f.cred(t, f.alice, nil)
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events
		WHEN NEW.action = 'credential_revoked' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RevokeAllCredentials(ctx, "system", f.alice.ID); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := f.s.DisablePrincipal(ctx, "system", f.alice.ID); err == nil {
		t.Fatal("expected failure")
	}
	for _, tok := range []string{tok1, tok2} {
		if _, err := f.s.ResolveCredential(ctx, tok); err != nil {
			t.Errorf("failed transaction leaked a revocation or disable: %v", err)
		}
	}
	if p, _ := f.s.PrincipalByID(ctx, f.alice.ID); p.DisabledAt != nil {
		t.Error("failed disable left the principal disabled")
	}
}

func TestResolveCredentialDetailAndLive(t *testing.T) {
	ctx := context.Background()
	f := newDisableFixture(t)
	now := time.Now().Truncate(time.Millisecond)
	setCredentialClock(t, &now)
	id, tok := f.cred(t, f.alice, nil)
	expID, _ := f.cred(t, f.alice, func() *time.Time { e := now.Add(time.Minute); return &e }())

	p, gotID, err := f.s.ResolveCredentialDetail(ctx, tok)
	if err != nil || gotID != id || p.ID != f.alice.ID {
		t.Fatalf("ResolveCredentialDetail = %+v, %d, %v", p, gotID, err)
	}
	live := func(id int64) bool {
		ok, err := f.s.CredentialLive(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !live(id) || !live(expID) || live(9999) {
		t.Error("liveness wrong at start")
	}
	now = now.Add(2 * time.Minute)
	if live(expID) {
		t.Error("expired credential reported live")
	}
	if !live(id) {
		t.Error("unexpiring credential not live")
	}
	if err := f.s.RevokeCredential(ctx, "system", id); err != nil {
		t.Fatal(err)
	}
	if live(id) {
		t.Error("revoked credential reported live")
	}
	id2, _ := f.cred(t, f.alice, nil)
	if _, err := f.s.DisablePrincipal(ctx, "system", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if live(id2) {
		t.Error("credential of disabled principal reported live")
	}
	// A store failure is an error, never "live".
	_ = f.s.db.Close()
	if ok, err := f.s.CredentialLive(ctx, id2); err == nil || ok {
		t.Errorf("closed store: %v, %v; want error and false", ok, err)
	}
}

func TestLastOperatorGuard(t *testing.T) {
	ctx := context.Background()
	f := newDisableFixture(t)
	// Sole operator cannot be disabled.
	if _, err := f.s.DisablePrincipal(ctx, "system", f.op.ID); !errors.Is(err, ErrLastOperator) {
		t.Fatalf("disable sole operator: %v", err)
	}
	if _, err := f.s.ResolveCredential(ctx, f.opTok); err != nil {
		t.Errorf("failed disable changed state: %v", err)
	}
	// A member does not count as an operator; promote a second operator.
	if _, err := f.s.db.Exec(`INSERT INTO principals (kind, name, role, created_at) VALUES ('human', 'op2', 'operator', 1)`); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.s.DisablePrincipal(ctx, "system", f.op.ID); err != nil || !changed {
		t.Fatalf("disable with two operators = %v, %v", changed, err)
	}
	// The disabled operator no longer counts: op2 is now the last.
	var op2 int64
	if err := f.s.db.QueryRow(`SELECT id FROM principals WHERE name = 'op2'`).Scan(&op2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DisablePrincipal(ctx, "system", op2); !errors.Is(err, ErrLastOperator) {
		t.Errorf("disable remaining operator: %v", err)
	}
	// Re-disabling the already-disabled operator is idempotent, not a guard hit.
	if changed, err := f.s.DisablePrincipal(ctx, "system", f.op.ID); err != nil || changed {
		t.Errorf("repeat disable = %v, %v", changed, err)
	}
	// Enabling it again restores the headroom.
	if _, err := f.s.EnablePrincipal(ctx, "system", f.op.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.s.DisablePrincipal(ctx, "system", op2); err != nil || !changed {
		t.Errorf("disable op2 after re-enable = %v, %v", changed, err)
	}
}

func TestLastOperatorGuardConcurrent(t *testing.T) {
	for range 10 {
		ctx := context.Background()
		f := newDisableFixture(t)
		if _, err := f.s.db.Exec(`INSERT INTO principals (kind, name, role, created_at) VALUES ('human', 'op2', 'operator', 1)`); err != nil {
			t.Fatal(err)
		}
		var op2 int64
		if err := f.s.db.QueryRow(`SELECT id FROM principals WHERE name = 'op2'`).Scan(&op2); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, id := range []int64{f.op.ID, op2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = f.s.DisablePrincipal(ctx, "system", id)
			}()
		}
		wg.Wait()
		ok, last := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrLastOperator):
				last++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if ok != 1 || last != 1 {
			t.Fatalf("results = %v; want exactly one success and one ErrLastOperator", errs)
		}
		var enabled int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM principals WHERE role = 'operator' AND disabled_at IS NULL`).Scan(&enabled); err != nil || enabled != 1 {
			t.Fatalf("enabled operators = %d (%v), want 1", enabled, err)
		}
	}
}

// TestDisableRacesCreateCredential: whatever the interleaving, once Disable
// has returned and every create has finished, no credential of the principal
// is live.
func TestDisableRacesCreateCredential(t *testing.T) {
	for range 10 {
		ctx := context.Background()
		f := newDisableFixture(t)
		var wg sync.WaitGroup
		tokens := make(chan string, 20)
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, tok, err := f.s.CreateCredential(ctx, "system", f.alice.ID, "race", nil)
				switch {
				case err == nil:
					tokens <- tok
				case errors.Is(err, ErrPrincipalDisabled):
				default:
					t.Errorf("create: %v", err)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.s.DisablePrincipal(ctx, "system", f.alice.ID); err != nil {
				t.Errorf("disable: %v", err)
			}
		}()
		wg.Wait()
		close(tokens)
		for tok := range tokens {
			if _, err := f.s.ResolveCredential(ctx, tok); !errors.Is(err, ErrCredentialInvalid) {
				t.Fatalf("a credential survived the disable: %v", err)
			}
		}
		var live int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM credentials WHERE principal_id = ? AND revoked_at IS NULL`, f.alice.ID).Scan(&live); err != nil || live != 0 {
			t.Fatalf("live credential rows = %d (%v)", live, err)
		}
	}
}

func TestDisableMigrationFromSchema8(t *testing.T) {
	const pre = 8
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
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'agent', 'bot', 'member', 2)`,
		`INSERT INTO credentials (principal_id, label, token_hash, created_at) VALUES (2, 'old', 'h1', 3)`,
		`PRAGMA user_version = 8`,
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
	for _, id := range []int64{1, 2} {
		p, err := s.PrincipalByID(ctx, id)
		if err != nil || p.DisabledAt != nil {
			t.Errorf("principal %d = %+v, %v; want enabled", id, p, err)
		}
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 9 {
		t.Errorf("user_version = %d (%v), want 9", version, err)
	}
	if changed, err := s.DisablePrincipal(ctx, "system", 2); err != nil || !changed {
		t.Errorf("disable after migration = %v, %v", changed, err)
	}
}
