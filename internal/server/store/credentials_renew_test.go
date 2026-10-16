package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Renewable credentials (issue #439): a credential with renew_ms has its
// expiry moved to renew_ms past a use, at most once a day, and never once it
// has expired or been revoked.

const renewLifetime = 90 * 24 * time.Hour

// renewFixture is a store with one person holding one renewable credential,
// issued at t0 on a clock the test moves.
type renewFixture struct {
	s     *Store
	ctx   context.Context
	now   time.Time
	t0    time.Time
	human Principal
	id    int64
	token string
}

func newRenewFixture(t *testing.T) *renewFixture {
	t.Helper()
	s, _, human := credentialFixture(t)
	f := &renewFixture{s: s, ctx: context.Background(), human: human}
	f.t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	f.now = f.t0
	setCredentialClock(t, &f.now)
	c, token, err := s.CreateRenewableCredential(f.ctx, "system", human.ID, "browser", renewLifetime)
	if err != nil {
		t.Fatalf("CreateRenewableCredential: %v", err)
	}
	if !c.Renews || c.ExpiresAt == nil || !c.ExpiresAt.Time().Equal(f.t0.Add(renewLifetime)) {
		t.Fatalf("credential = %+v, want renewable, expiring %v", c, f.t0.Add(renewLifetime))
	}
	f.id, f.token = c.ID, token
	return f
}

// expiresAt reads the stored expiry.
func (f *renewFixture) expiresAt(t *testing.T) time.Time {
	t.Helper()
	var ms int64
	if err := f.s.db.QueryRow("SELECT expires_at FROM credentials WHERE id = ?", f.id).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	return time.UnixMilli(ms).UTC()
}

// use resolves the token at t0+at and reports whether it was accepted.
func (f *renewFixture) use(t *testing.T, at time.Duration) bool {
	t.Helper()
	f.now = f.t0.Add(at)
	_, err := f.s.ResolveCredential(f.ctx, f.token)
	if err != nil && !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("ResolveCredential at %v: %v", at, err)
	}
	return err == nil
}

func TestRenewableCredentialRenewsOnUse(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		name string
		// uses are when the credential is used, after t0, in order.
		uses []time.Duration
		// want is the expiry after the last use, after t0.
		want time.Duration
	}{
		{"unused", nil, renewLifetime},
		{"used within the first day: no write", []time.Duration{time.Hour}, renewLifetime},
		{"used exactly a day in: no write yet", []time.Duration{day}, renewLifetime},
		{"used just past a day: renewed", []time.Duration{day + time.Millisecond}, day + time.Millisecond + renewLifetime},
		{"used twice in one day: renewed once", []time.Duration{day + time.Hour, day + 5*time.Hour, day + 23*time.Hour}, day + time.Hour + renewLifetime},
		{"used on two days: renewed twice", []time.Duration{day + time.Hour, 2*day + 2*time.Hour}, 2*day + 2*time.Hour + renewLifetime},
		{"used late in its life: renewed", []time.Duration{renewLifetime - time.Minute}, renewLifetime - time.Minute + renewLifetime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRenewFixture(t)
			for _, at := range tt.uses {
				if !f.use(t, at) {
					t.Fatalf("use at %v refused", at)
				}
			}
			if got, want := f.expiresAt(t), f.t0.Add(tt.want); !got.Equal(want) {
				t.Errorf("expires_at = %v, want %v", got, want)
			}
		})
	}
}

// A renewable credential used every day never expires, and the expiry moves
// at most once a day however often it is used.
func TestRenewableCredentialUsedDailyNeverExpires(t *testing.T) {
	f := newRenewFixture(t)
	writes := 0
	last := f.expiresAt(t)
	for d := 0; d < 400; d++ {
		today := 0
		for _, h := range []time.Duration{0, 3, 8, 14, 23} {
			if !f.use(t, time.Duration(d)*24*time.Hour+h*time.Hour) {
				t.Fatalf("refused on day %d at hour %d", d, h)
			}
			if got := f.expiresAt(t); !got.Equal(last) {
				today++
				last = got
			}
		}
		if today > 1 {
			t.Fatalf("day %d: the expiry moved %d times, want at most once", d, today)
		}
		writes += today
	}
	// 400 days is longer than the lifetime: it was renewed to get there.
	if writes == 0 {
		t.Error("the expiry never moved")
	}
	if !last.After(f.now.Add(renewLifetime - 24*time.Hour)) {
		t.Errorf("expiry %v is not about a lifetime past the last use %v", last, f.now)
	}
}

// One left unused for its whole lifetime expires, and using it afterwards
// does not bring it back.
func TestRenewableCredentialUnusedExpires(t *testing.T) {
	f := newRenewFixture(t)
	if f.use(t, renewLifetime) {
		t.Fatal("accepted at its expiry")
	}
	if f.use(t, renewLifetime+48*time.Hour) {
		t.Fatal("accepted after its expiry")
	}
	if got, want := f.expiresAt(t), f.t0.Add(renewLifetime); !got.Equal(want) {
		t.Errorf("expires_at = %v after use past expiry, want unchanged %v", got, want)
	}
}

// A refused credential is never renewed: revoked, or of a disabled principal.
func TestRenewalNeverRevivesRefusedCredential(t *testing.T) {
	tests := []struct {
		name   string
		refuse func(t *testing.T, f *renewFixture)
	}{
		{"revoked", func(t *testing.T, f *renewFixture) {
			if err := f.s.RevokeCredential(f.ctx, "system", f.id); err != nil {
				t.Fatal(err)
			}
		}},
		{"revoked by its holder", func(t *testing.T, f *renewFixture) {
			if err := f.s.RevokeOwnCredential(f.ctx, principalActor(f.human.ID), f.id); err != nil {
				t.Fatal(err)
			}
		}},
		{"all revoked", func(t *testing.T, f *renewFixture) {
			if _, err := f.s.RevokeAllCredentials(f.ctx, principalActor(f.human.ID), f.human.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"principal disabled", func(t *testing.T, f *renewFixture) {
			if _, err := f.s.DisablePrincipal(f.ctx, "system", f.human.ID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRenewFixture(t)
			tt.refuse(t, f)
			if f.use(t, 48*time.Hour) {
				t.Fatal("refused credential accepted")
			}
			if got, want := f.expiresAt(t), f.t0.Add(renewLifetime); !got.Equal(want) {
				t.Errorf("expires_at = %v, want unchanged %v", got, want)
			}
		})
	}
}

// The renewal write is guarded on the row as resolution read it, so what
// happens between the read and the write cannot be undone by it.
func TestRenewalGuardAgainstChangesSinceTheRead(t *testing.T) {
	tests := []struct {
		name string
		// change runs between the read and the write, and returns the
		// expiry the read saw.
		change func(t *testing.T, f *renewFixture) int64
	}{
		{"revoked in between", func(t *testing.T, f *renewFixture) int64 {
			read := f.expiresAt(t).UnixMilli()
			if err := f.s.RevokeCredential(f.ctx, "system", f.id); err != nil {
				t.Fatal(err)
			}
			return read
		}},
		{"renewed by another request in between", func(t *testing.T, f *renewFixture) int64 {
			read := f.expiresAt(t).UnixMilli()
			if _, err := f.s.db.Exec("UPDATE credentials SET expires_at = expires_at + 1 WHERE id = ?", f.id); err != nil {
				t.Fatal(err)
			}
			return read
		}},
		{"expired by the time of the write", func(t *testing.T, f *renewFixture) int64 {
			// The read saw an expiry that has passed at the write's now.
			if _, err := f.s.db.Exec("UPDATE credentials SET expires_at = ? WHERE id = ?", f.now.Add(-time.Second).UnixMilli(), f.id); err != nil {
				t.Fatal(err)
			}
			return f.now.Add(-time.Second).UnixMilli()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRenewFixture(t)
			f.now = f.t0.Add(48 * time.Hour)
			read := tt.change(t, f)
			before := f.expiresAt(t)
			f.s.renewCredential(f.ctx, f.id, read, renewLifetime.Milliseconds(), f.now)
			if got := f.expiresAt(t); !got.Equal(before) {
				t.Errorf("expires_at = %v, want %v: the guarded write changed it", got, before)
			}
		})
	}
}

// A credential without renew_ms keeps its expiry, fixed or none, however it
// is used.
func TestFixedCredentialIsNeverRenewed(t *testing.T) {
	s, _, human := credentialFixture(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	now := t0
	setCredentialClock(t, &now)
	exp := t0.Add(10 * 24 * time.Hour)
	c, token, err := s.CreateCredential(ctx, "system", human.ID, "fixed", &exp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Renews {
		t.Errorf("fixed credential says it renews: %+v", c)
	}
	now = t0.Add(9 * 24 * time.Hour)
	if _, err := s.ResolveCredential(ctx, token); err != nil {
		t.Fatal(err)
	}
	var ms int64
	if err := s.db.QueryRow("SELECT expires_at FROM credentials WHERE id = ?", c.ID).Scan(&ms); err != nil || !time.UnixMilli(ms).Equal(exp) {
		t.Errorf("expires_at = %v, %v; want unchanged %v", time.UnixMilli(ms), err, exp)
	}
}

func TestRenewableCredentialListRotateAndErrors(t *testing.T) {
	f := newRenewFixture(t)
	t.Run("list says it renews", func(t *testing.T) {
		list, err := f.s.ListCredentials(f.ctx, f.human.ID)
		if err != nil || len(list) != 1 || !list[0].Renews {
			t.Fatalf("ListCredentials = %+v, %v", list, err)
		}
	})
	t.Run("rotation keeps it renewable", func(t *testing.T) {
		c, token, err := f.s.RotateCredential(f.ctx, "system", f.id)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Renews || c.ExpiresAt == nil || !c.ExpiresAt.Time().Equal(f.t0.Add(renewLifetime)) {
			t.Errorf("rotated = %+v", c)
		}
		f.id, f.token = c.ID, token
		if !f.use(t, 48*time.Hour) || !f.expiresAt(t).Equal(f.t0.Add(48*time.Hour+renewLifetime)) {
			t.Errorf("rotated credential did not renew: expires_at %v", f.expiresAt(t))
		}
	})
	t.Run("a lifetime that is not positive", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Hour, time.Microsecond} {
			if _, _, err := f.s.CreateRenewableCredential(f.ctx, "system", f.human.ID, "x", d); !errors.Is(err, ErrCredentialExpiryPast) {
				t.Errorf("lifetime %v: err = %v, want ErrCredentialExpiryPast", d, err)
			}
		}
	})
	t.Run("the table refuses renew_ms without an expiry", func(t *testing.T) {
		_, err := f.s.db.Exec(`INSERT INTO credentials (principal_id, label, token_hash, created_at, renew_ms) VALUES (?, 'x', 'h-no-expiry', 1, 1000)`, f.human.ID)
		if err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("insert = %v, want a CHECK failure", err)
		}
	})
}

// Revoking one's own credential audits reason=self and is otherwise a
// revocation: the last operator guard holds.
func TestRevokeOwnCredential(t *testing.T) {
	f := newRenewFixture(t)
	if err := f.s.RevokeOwnCredential(f.ctx, principalActor(f.human.ID), f.id); err != nil {
		t.Fatal(err)
	}
	evs, err := f.s.ListAuditEvents(f.ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, e := range evs {
		if e.Action == "credential_revoked" {
			found++
			if want := credentialDetail(f.id, "browser") + " reason=self"; e.Detail != want || e.Actor != principalActor(f.human.ID) {
				t.Errorf("audit = %+v, want detail %q", e, want)
			}
		}
	}
	if found != 1 {
		t.Errorf("credential_revoked rows = %d, want 1", found)
	}
	// Again: nothing more is written.
	if err := f.s.RevokeOwnCredential(f.ctx, principalActor(f.human.ID), f.id); err != nil || countAudit(t, f.s, "credential_revoked") != 1 {
		t.Errorf("repeat = %v, rows %d", err, countAudit(t, f.s, "credential_revoked"))
	}

	op, opCred, _, err := f.s.BootstrapOperator(f.ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RevokeOwnCredential(f.ctx, principalActor(op.ID), opCred.ID); !errors.Is(err, ErrLastOperator) {
		t.Errorf("last operator revoking itself = %v, want ErrLastOperator", err)
	}
}

// Redemption issues a renewable credential with the invite's lifetime.
func TestRedeemInviteIssuesRenewableCredential(t *testing.T) {
	f := newInviteFixture(t)
	lifetime := int64(30 * 24 * 60 * 60)
	req := personal("general")
	req.CredentialLifetimeSeconds = &lifetime
	inv := f.mustCreate(f.alice, req)
	got, err := f.redeem(inv.Code, "river")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Credential.Renews || got.Credential.ExpiresAt == nil || !got.Credential.ExpiresAt.Time().Equal(f.now.Add(30*24*time.Hour)) {
		t.Errorf("credential = %+v", got.Credential)
	}
	if n := f.count("SELECT renew_ms FROM credentials WHERE id = ?", got.Credential.ID); int64(n) != lifetime*1000 {
		t.Errorf("renew_ms = %d, want %d", n, lifetime*1000)
	}
}

// A database written before issue #439 (version 25) keeps every credential
// as it was: fixed expiry or none, never renewed.
func TestRenewMigrationFromSchema25(t *testing.T) {
	const pre = 25
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
		if step := migrationSteps[i+1]; step != nil {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := step(ctx, tx); err != nil {
				t.Fatalf("migration step %d: %v", i+1, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	token := "conch_" + strings.Repeat("B", 43)
	exp := t0.Add(10 * 24 * time.Hour)
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'ann', 'member', 1)`, nil},
		{`INSERT INTO credentials (id, principal_id, label, token_hash, created_at, expires_at) VALUES (1, 1, 'old', ?, 2, ?)`, []any{hashCredentialToken(token), exp.UnixMilli()}},
		{`INSERT INTO credentials (id, principal_id, label, token_hash, created_at) VALUES (2, 1, 'forever', 'h2', 3)`, nil},
		{`PRAGMA user_version = 25`, nil},
	} {
		if _, err := db.Exec(stmt.q, stmt.args...); err != nil {
			t.Fatalf("seed %q: %v", stmt.q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 26 {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	var renewing int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM credentials WHERE renew_ms IS NOT NULL").Scan(&renewing); err != nil || renewing != 0 {
		t.Errorf("credentials with renew_ms after migration = %d, %v; want 0", renewing, err)
	}
	list, err := s.ListCredentials(ctx, 1)
	if err != nil || len(list) != 2 || list[0].Renews || list[1].Renews {
		t.Errorf("ListCredentials = %+v, %v; want two that do not renew", list, err)
	}
	// Used late in its life, the old credential still resolves and is not
	// renewed.
	now := t0.Add(9 * 24 * time.Hour)
	setCredentialClock(t, &now)
	if _, err := s.ResolveCredential(ctx, token); err != nil {
		t.Fatalf("old credential after migration: %v", err)
	}
	var ms int64
	if err := s.db.QueryRow("SELECT expires_at FROM credentials WHERE id = 1").Scan(&ms); err != nil || ms != exp.UnixMilli() {
		t.Errorf("expires_at = %d, %v; want unchanged %d", ms, err, exp.UnixMilli())
	}
}
