package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// setCredentialClock makes the credential clock return *now until the test ends.
func setCredentialClock(t *testing.T, now *time.Time) {
	t.Helper()
	prev := credentialNow
	credentialNow = func() time.Time { return *now }
	t.Cleanup(func() { credentialNow = prev })
}

func credentialFixture(t *testing.T) (*Store, Principal, Principal) {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "leviathan")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.CreatePrincipal(ctx, PrincipalHuman, "nick")
	if err != nil {
		t.Fatal(err)
	}
	return s, agent, human
}

var tokenShape = regexp.MustCompile(`^conch_[A-Za-z0-9_-]{43}$`)

func TestCreateAndResolveCredential(t *testing.T) {
	s, agent, human := credentialFixture(t)
	ctx := context.Background()

	for _, p := range []Principal{agent, human} {
		t.Run(string(p.Kind), func(t *testing.T) {
			c, token, err := s.CreateCredential(ctx, "system", p.ID, "laptop", nil)
			if err != nil {
				t.Fatalf("CreateCredential: %v", err)
			}
			if !tokenShape.MatchString(token) {
				t.Errorf("token %q does not match conch_ + 43 base64url chars", token[:10])
			}
			if c.PrincipalID != p.ID || c.Label != "laptop" || c.ExpiresAt != nil || c.RevokedAt != nil || c.ID <= 0 {
				t.Errorf("credential = %+v", c)
			}
			got, err := s.ResolveCredential(ctx, token)
			if err != nil {
				t.Fatalf("ResolveCredential: %v", err)
			}
			if got.ID != p.ID || got.Kind != p.Kind || got.Name != p.Name {
				t.Errorf("resolved %+v, want principal %+v", got, p)
			}
		})
	}
}

func TestCreateCredentialErrors(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)

	if _, _, err := s.CreateCredential(ctx, "system", 9999, "x", nil); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("unknown principal error = %v, want ErrPrincipalNotFound", err)
	}
	if _, _, err := s.CreateCredential(ctx, "system", agent.ID, "x", &past); !errors.Is(err, ErrCredentialExpiryPast) {
		t.Errorf("past expiry error = %v, want ErrCredentialExpiryPast", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM credentials").Scan(&n); err != nil || n != 0 {
		t.Errorf("credentials rows = %d (%v), want 0", n, err)
	}
	if n := countAudit(t, s, "credential_created"); n != 0 {
		t.Errorf("credential_created events = %d, want 0", n)
	}
}

func TestResolveCredentialMalformedAndUnknown(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	_, good, err := s.CreateCredential(ctx, "system", agent.ID, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(good, "conch_")

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"no prefix", body},
		{"wrong prefix", "other_" + body},
		{"prefix only", "conch_"},
		{"right prefix, too short", good[:len(good)-1]},
		{"right prefix, too long", good + "A"},
		{"right shape but unknown", "conch_" + strings.Repeat("A", 43)},
		{"trailing newline", good + "\n"},
		{"trailing space", good + " "},
		{"leading space", " " + good},
		{"bearer scheme included", "Bearer " + good},
		{"illegal character", "conch_" + strings.Repeat("A", 42) + "="},
		{"uppercase prefix", "CONCH_" + body},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := s.ResolveCredential(ctx, tt.token)
			if !errors.Is(err, ErrCredentialInvalid) {
				t.Fatalf("error = %v, want ErrCredentialInvalid", err)
			}
			if p != (Principal{}) {
				t.Errorf("returned principal %+v alongside error", p)
			}
		})
	}
	// The good one still works: the table did not disturb anything.
	if _, err := s.ResolveCredential(ctx, good); err != nil {
		t.Errorf("good token: %v", err)
	}
}

func TestResolveCredentialExpiry(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	setCredentialClock(t, &now)

	exp := now.Add(time.Hour)
	_, token, err := s.CreateCredential(ctx, "system", agent.ID, "short", &exp)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		at      time.Time
		wantErr bool
	}{
		{"before expiry", exp.Add(-time.Millisecond), false},
		{"exactly at expiry", exp, true},
		{"after expiry", exp.Add(time.Second), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now = tt.at
			_, err := s.ResolveCredential(ctx, token)
			if tt.wantErr != errors.Is(err, ErrCredentialInvalid) {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error %v", err)
			}
		})
	}
}

func TestResolveCredentialRevoked(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	c, token, err := s.CreateCredential(ctx, "system", agent.ID, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "system", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveCredential(ctx, token); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("revoked error = %v, want ErrCredentialInvalid", err)
	}
}

func TestResolveCredentialStoreErrorIsDistinct(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	_, token, err := s.CreateCredential(ctx, "system", agent.ID, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = s.ResolveCredential(ctx, token)
	if err == nil {
		t.Fatal("resolve against a closed database succeeded; must fail closed")
	}
	if errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("store failure reported as ErrCredentialInvalid: %v", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), hashCredentialToken(token)) {
		t.Errorf("error leaks token material: %v", err)
	}
}

func TestRotateCredential(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	setCredentialClock(t, &now)

	exp := now.Add(24 * time.Hour)
	old, oldToken, err := s.CreateCredential(ctx, "system", agent.ID, "prod", &exp)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	fresh, newToken, err := s.RotateCredential(ctx, "system", old.ID)
	if err != nil {
		t.Fatalf("RotateCredential: %v", err)
	}
	if fresh.ID == old.ID || newToken == oldToken || !tokenShape.MatchString(newToken) {
		t.Errorf("rotation did not issue a distinct credential: %+v", fresh)
	}
	if fresh.PrincipalID != agent.ID || fresh.Label != "prod" || fresh.ExpiresAt == nil || !fresh.ExpiresAt.Time().Equal(exp) {
		t.Errorf("new credential = %+v, want same principal/label/expiry", fresh)
	}
	if _, err := s.ResolveCredential(ctx, newToken); err != nil {
		t.Errorf("new token: %v", err)
	}
	if _, err := s.ResolveCredential(ctx, oldToken); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("old token error = %v, want ErrCredentialInvalid", err)
	}

	list, err := s.ListCredentials(ctx, agent.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if list[0].ID != fresh.ID || list[0].RevokedAt != nil {
		t.Errorf("newest = %+v, want the live replacement", list[0])
	}
	if list[1].ID != old.ID || list[1].RevokedAt == nil {
		t.Errorf("oldest = %+v, want revoked original", list[1])
	}

	// Rotating the revoked original fails and writes nothing.
	if _, _, err := s.RotateCredential(ctx, "system", old.ID); !errors.Is(err, ErrCredentialRevoked) {
		t.Errorf("rotate revoked error = %v, want ErrCredentialRevoked", err)
	}
	if _, _, err := s.RotateCredential(ctx, "system", 9999); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("rotate unknown error = %v, want ErrCredentialNotFound", err)
	}
	if n := countRows(t, s, "credentials"); n != 2 {
		t.Errorf("credentials rows = %d, want 2", n)
	}
}

func TestRotateExpiredCredentialFailsClosed(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	setCredentialClock(t, &now)

	exp := now.Add(time.Hour)
	c, _, err := s.CreateCredential(ctx, "system", agent.ID, "short", &exp)
	if err != nil {
		t.Fatal(err)
	}
	now = exp.Add(time.Second)
	if _, _, err := s.RotateCredential(ctx, "system", c.ID); !errors.Is(err, ErrCredentialExpired) {
		t.Fatalf("rotate expired error = %v, want ErrCredentialExpired", err)
	}
	if n := countRows(t, s, "credentials"); n != 1 {
		t.Errorf("credentials rows = %d, want 1 (nothing written)", n)
	}
	if n := countAudit(t, s, "credential_rotated"); n != 0 {
		t.Errorf("credential_rotated events = %d, want 0", n)
	}
}

func TestRotateConcurrent(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	old, oldToken, err := s.CreateCredential(ctx, "system", agent.ID, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 2
	type result struct {
		c     schema.CredentialV1
		token string
		err   error
	}
	results := make([]result, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, tok, err := s.RotateCredential(ctx, "system", old.ID)
			results[i] = result{c, tok, err}
		}()
	}
	close(start)
	wg.Wait()

	var wins, revoked int
	var winner result
	for _, r := range results {
		switch {
		case r.err == nil:
			wins++
			winner = r
		case errors.Is(r.err, ErrCredentialRevoked):
			revoked++
		default:
			t.Errorf("unexpected rotate error: %v", r.err)
		}
	}
	if wins != 1 || revoked != 1 {
		t.Fatalf("wins = %d, revoked = %d; want exactly 1 and 1", wins, revoked)
	}
	var live int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM credentials WHERE revoked_at IS NULL").Scan(&live); err != nil || live != 1 {
		t.Errorf("live credentials = %d (%v), want 1", live, err)
	}
	if _, err := s.ResolveCredential(ctx, winner.token); err != nil {
		t.Errorf("winner token: %v", err)
	}
	if _, err := s.ResolveCredential(ctx, oldToken); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("old token error = %v", err)
	}
	if n := countAudit(t, s, "credential_rotated"); n != 1 {
		t.Errorf("credential_rotated events = %d, want 1", n)
	}
}

func TestRevokeCredential(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	setCredentialClock(t, &now)

	a, tokA, err := s.CreateCredential(ctx, "system", agent.ID, "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, tokB, err := s.CreateCredential(ctx, "system", agent.ID, "b", nil)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	if err := s.RevokeCredential(ctx, "system", a.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	firstRevokedAt := revokedAtMillis(t, s, a.ID)
	now = now.Add(time.Hour)
	if err := s.RevokeCredential(ctx, "system", a.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if got := revokedAtMillis(t, s, a.ID); got != firstRevokedAt {
		t.Errorf("revoked_at changed on second revoke: %d -> %d", firstRevokedAt, got)
	}
	if n := countAudit(t, s, "credential_revoked"); n != 1 {
		t.Errorf("credential_revoked events = %d, want 1", n)
	}
	if err := s.RevokeCredential(ctx, "system", 9999); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("revoke unknown error = %v, want ErrCredentialNotFound", err)
	}

	if _, err := s.ResolveCredential(ctx, tokA); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("revoked a: %v", err)
	}
	if _, err := s.ResolveCredential(ctx, tokB); err != nil {
		t.Errorf("b must be unaffected: %v", err)
	}
	// Row kept, principal untouched.
	if n := countRows(t, s, "credentials"); n != 2 {
		t.Errorf("credentials rows = %d, want 2", n)
	}
	if _, err := s.PrincipalByID(ctx, agent.ID); err != nil {
		t.Errorf("principal: %v", err)
	}
	if err := s.RevokeCredential(ctx, "system", b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveCredential(ctx, tokB); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("revoked b: %v", err)
	}
}

func TestListCredentialsOrderAndShape(t *testing.T) {
	s, agent, human := credentialFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	setCredentialClock(t, &now)

	empty, err := s.ListCredentials(ctx, agent.ID)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v, %v; want non-nil empty", empty, err)
	}
	if _, err := s.ListCredentials(ctx, 9999); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("unknown principal error = %v", err)
	}

	// Two share a created_at (tie broken by id desc); one is older; one belongs to someone else.
	c1, _, _ := s.CreateCredential(ctx, "system", agent.ID, "oldest", nil)
	now = now.Add(time.Second)
	c2, _, _ := s.CreateCredential(ctx, "system", agent.ID, "tie-first", nil)
	c3, _, _ := s.CreateCredential(ctx, "system", agent.ID, "tie-second", nil)
	if _, _, err := s.CreateCredential(ctx, "system", human.ID, "other", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "system", c3.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListCredentials(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, c := range got {
		ids = append(ids, c.ID)
		if c.PrincipalID != agent.ID {
			t.Errorf("foreign credential in list: %+v", c)
		}
	}
	want := []int64{c3.ID, c2.ID, c1.ID}
	if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Errorf("order = %v, want %v (newest first, ties by id desc, revoked included)", ids, want)
	}
	if got[0].RevokedAt == nil {
		t.Error("revoked credential should show revoked_at")
	}
}

func TestCredentialPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateCredential(ctx, "system", agent.ID, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	p, err := s2.ResolveCredential(ctx, token)
	if err != nil || p.ID != agent.ID {
		t.Errorf("after restart: %+v, %v", p, err)
	}
}

func TestCredentialAtRestAndAuditHoldNoPlaintext(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	c, token, err := s.CreateCredential(ctx, "system", agent.ID, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, rotatedToken, err := s.RotateCredential(ctx, "system", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	c2, tok3, err := s.CreateCredential(ctx, "system", agent.ID, "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "system", c2.ID); err != nil {
		t.Fatal(err)
	}
	tokens := []string{token, rotatedToken, tok3}

	rows, err := s.db.Query("SELECT id, principal_id, label, token_hash, created_at, expires_at, revoked_at FROM credentials")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	hexHash := regexp.MustCompile(`^[0-9a-f]{64}$`)
	wantHashes := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		wantHashes[hashCredentialToken(tok)] = true
	}
	var seen int
	for rows.Next() {
		var id, pid, created int64
		var label, hash string
		var exp, rev sql.NullInt64
		if err := rows.Scan(&id, &pid, &label, &hash, &created, &exp, &rev); err != nil {
			t.Fatal(err)
		}
		seen++
		if !hexHash.MatchString(hash) {
			t.Errorf("token_hash %q is not 64 lowercase hex chars", hash)
		}
		// The column must be exactly SHA-256(token) for one of the issued
		// tokens, not merely something digest-shaped.
		if !wantHashes[hash] {
			t.Errorf("row %d token_hash is not the SHA-256 of an issued token", id)
		}
		delete(wantHashes, hash)
		for _, tok := range tokens {
			if strings.Contains(label, tok) || strings.Contains(hash, tok) || strings.Contains(hash, strings.TrimPrefix(tok, "conch_")) {
				t.Errorf("row %d holds plaintext token material", id)
			}
		}
	}
	if seen != 3 {
		t.Errorf("rows = %d, want 3", seen)
	}
	if len(wantHashes) != 0 {
		t.Errorf("%d issued tokens have no row holding their hash", len(wantHashes))
	}

	events, err := s.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		blob := e.Actor + "|" + e.Action + "|" + e.Subject + "|" + e.Detail
		for _, tok := range tokens {
			if strings.Contains(blob, tok) || strings.Contains(blob, hashCredentialToken(tok)) {
				t.Errorf("audit event %q leaks token material", e.Action)
			}
		}
	}
}

func TestCredentialAuditEvents(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	ctx := context.Background()
	c, _, err := s.CreateCredential(ctx, "system", agent.ID, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, err := s.RotateCredential(ctx, "system", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "system", fresh.ID); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []AuditEvent
	for _, e := range events {
		if strings.HasPrefix(e.Action, "credential_") {
			got = append(got, e)
		}
	}
	wantActions := []string{"credential_created", "credential_rotated", "credential_revoked"}
	if len(got) != len(wantActions) {
		t.Fatalf("events = %+v, want %v", got, wantActions)
	}
	for i, e := range got {
		if e.Action != wantActions[i] || e.Actor != "system" || e.Subject != principalActor(agent.ID) {
			t.Errorf("event %d = %+v", i, e)
		}
		if !strings.Contains(e.Detail, `label="ci"`) || !strings.Contains(e.Detail, "credential=") {
			t.Errorf("event %d detail = %q, want credential id and label", i, e.Detail)
		}
	}
}

// TestCredentialMigrationFromPreCredentialSchema builds a database at schema
// version 5 (principals and manifests present) and checks the upgrade adds an
// empty credentials table without disturbing existing rows.
func TestCredentialMigrationFromPreCredentialSchema(t *testing.T) {
	const preCredentials = 5
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < preCredentials; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, created_at) VALUES (1, 'agent', 'bot', 1), (2, 'human', 'nick', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'hi', 4)`,
		`INSERT INTO agent_manifests (principal_id, display_name, tier, capabilities, channels, rate_limits, created_at, updated_at)
		 VALUES (1, 'Bot', 'A', '[]', '[]', '[]', 5, 5)`,
		`PRAGMA user_version = 5`,
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

	for _, c := range []struct {
		table string
		want  int
	}{{"credentials", 0}, {"principals", 2}, {"channels", 1}, {"messages", 1}, {"agent_manifests", 1}} {
		if n := countRows(t, s, c.table); n != c.want {
			t.Errorf("%s rows = %d, want %d", c.table, n, c.want)
		}
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d (%v), want %d", version, err, len(migrations))
	}
	// The migrated database is immediately usable.
	if _, _, err := s.CreateCredential(ctx, "system", 1, "post-migration", nil); err != nil {
		t.Errorf("create after migration: %v", err)
	}
}

func TestCredentialTokenHashIsUnique(t *testing.T) {
	s, agent, _ := credentialFixture(t)
	if _, err := s.db.Exec(
		`INSERT INTO credentials (principal_id, label, token_hash, created_at) VALUES (?, 'a', 'h', 1), (?, 'b', 'h', 1)`,
		agent.ID, agent.ID); err == nil {
		t.Error("duplicate token_hash accepted; the unique index is missing")
	}
}

// TestCredentialFilesHoldNoPlaintext scans every file SQLite leaves in the
// data directory (database, WAL, shared memory) after the store is closed.
func TestCredentialFilesHoldNoPlaintext(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, filepath.Join(dir, "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	c, token, err := s.CreateCredential(ctx, "system", agent.ID, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, rotated, err := s.RotateCredential(ctx, "system", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no database files to scan")
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- files from this test's own temp directory
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{token, rotated} {
			if bytes.Contains(data, []byte(tok)) || bytes.Contains(data, []byte(strings.TrimPrefix(tok, schema.CredentialTokenPrefix))) {
				t.Errorf("%s contains plaintext token material", e.Name())
			}
		}
	}
}

// TestRotateRevokeConcurrent races a rotation against a revocation of the same
// credential: whoever wins, the old token is dead and the operator's revoke
// never leaves a live credential it did not expect.
func TestRotateRevokeConcurrent(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		s, agent, _ := credentialFixture(t)
		old, oldToken, err := s.CreateCredential(ctx, "system", agent.ID, "prod", nil)
		if err != nil {
			t.Fatal(err)
		}
		var (
			wg          sync.WaitGroup
			rotatedTok  string
			rotateErr   error
			revokeErr   error
			start       = make(chan struct{})
			rotatedCred schema.CredentialV1
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			rotatedCred, rotatedTok, rotateErr = s.RotateCredential(ctx, "system", old.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			revokeErr = s.RevokeCredential(ctx, "system", old.ID)
		}()
		close(start)
		wg.Wait()

		if revokeErr != nil {
			t.Fatalf("iteration %d: revoke: %v", i, revokeErr)
		}
		if _, err := s.ResolveCredential(ctx, oldToken); !errors.Is(err, ErrCredentialInvalid) {
			t.Fatalf("iteration %d: old token still resolves (err %v)", i, err)
		}
		var live int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM credentials WHERE revoked_at IS NULL").Scan(&live); err != nil {
			t.Fatal(err)
		}
		switch {
		case rotateErr == nil:
			// Rotation won: exactly its new credential is live.
			if live != 1 {
				t.Fatalf("iteration %d: rotate won but live = %d", i, live)
			}
			if p, err := s.ResolveCredential(ctx, rotatedTok); err != nil || p.ID != agent.ID {
				t.Fatalf("iteration %d: rotated token: %v", i, err)
			}
			if rotatedCred.ID == old.ID {
				t.Fatalf("iteration %d: rotation reused the old id", i)
			}
		case errors.Is(rotateErr, ErrCredentialRevoked):
			// Revocation won: nothing is live.
			if live != 0 {
				t.Fatalf("iteration %d: revoke won but live = %d", i, live)
			}
		default:
			t.Fatalf("iteration %d: unexpected rotate error: %v", i, rotateErr)
		}
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func countAudit(t *testing.T, s *Store, action string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action = ?", action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func revokedAtMillis(t *testing.T, s *Store, id int64) int64 {
	t.Helper()
	var v sql.NullInt64
	if err := s.db.QueryRow("SELECT revoked_at FROM credentials WHERE id = ?", id).Scan(&v); err != nil || !v.Valid {
		t.Fatalf("revoked_at for %d: %v (valid=%v)", id, err, v.Valid)
	}
	return v.Int64
}
