package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func hookFixture(t *testing.T) (*Store, Channel, Principal) {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	channel, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := s.CreatePrincipal(ctx, PrincipalAgent, "monitor")
	if err != nil {
		t.Fatal(err)
	}
	return s, channel, principal
}

func TestHookRoundTrip(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	created, err := s.CreateHookWithLabel(ctx, "test-token", "ci", channel.ID, principal.ID)
	if err != nil {
		t.Fatalf("CreateHook: %v", err)
	}

	tests := []struct {
		name      string
		token     string
		wantError error
	}{
		{name: "existing", token: "test-token"},
		{name: "unknown", token: "missing", wantError: ErrNotFound},
		{name: "empty", token: "", wantError: ErrNotFound},
		{name: "malformed", token: "../../etc/passwd\x00 %", wantError: ErrNotFound}, // #nosec G101 -- test input, not a credential
		{name: "the stored hash is not a token", token: hashHookToken("test-token"), wantError: ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook, err := s.HookByToken(ctx, tt.token)
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("HookByToken error = %v, want %v", err, tt.wantError)
			}
			if tt.wantError == nil && (hook.ID != created.ID || hook.Label != "ci" || hook.ChannelID != channel.ID ||
				hook.PrincipalID != principal.ID || hook.CreatedAt.IsZero()) {
				t.Errorf("HookByToken = %+v", hook)
			}
		})
	}
}

func TestCreateHookDuplicateToken(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	if _, err := s.CreateHook(ctx, "same", channel.ID, principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHook(ctx, "same", channel.ID, principal.ID); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate token error = %v, want ErrDuplicate", err)
	}
}

// TestHookStoresOnlyTheHash checks no column of the hooks table holds the
// token, and that the stored value is the SHA-256 hex digest.
func TestHookStoresOnlyTheHash(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	const token = "plaintext-hook-token-do-not-store" // #nosec G101 -- test input, not a credential
	if _, err := s.CreateHookWithLabel(ctx, token, "label", channel.ID, principal.ID); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := s.db.QueryRow("SELECT token_hash FROM hooks").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != hashHookToken(token) || len(hash) != 64 {
		t.Errorf("token_hash = %q, want the SHA-256 hex of the token", hash)
	}
	cols, err := s.db.Query("SELECT * FROM hooks")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cols.Close() }()
	names, err := cols.Columns()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n == "token" {
			t.Errorf("hooks table still has a plaintext token column")
		}
	}
}

func TestRevokeHook(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	hook, err := s.CreateHookWithLabel(ctx, "to-revoke", `a "quoted" label`, channel.ID, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateHook(ctx, "bystander", channel.ID, principal.ID)
	if err != nil {
		t.Fatal(err)
	}

	auditCount := func() int { return countRows(t, s, "audit_events") }
	before := auditCount()

	for i, tt := range []struct {
		name       string
		id         int64
		wantErr    error
		wantAudits int
	}{
		{"first revoke audits once", hook.ID, nil, 1},
		{"revoke again is idempotent", hook.ID, nil, 0},
		{"unknown id", 9999, ErrHookNotFound, 0},
	} {
		t.Run(fmt.Sprintf("%d %s", i, tt.name), func(t *testing.T) {
			n := auditCount()
			if err := s.RevokeHook(ctx, "principal:1", tt.id); !errors.Is(err, tt.wantErr) {
				t.Fatalf("RevokeHook error = %v, want %v", err, tt.wantErr)
			}
			if got := auditCount() - n; got != tt.wantAudits {
				t.Errorf("audit events written = %d, want %d", got, tt.wantAudits)
			}
		})
	}

	if _, err := s.HookByToken(ctx, "to-revoke"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked hook still resolves: %v", err)
	}
	if _, err := s.HookByToken(ctx, "bystander"); err != nil {
		t.Errorf("revoking one hook affected another (%d): %v", other.ID, err)
	}

	events, err := s.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found []AuditEvent
	for _, e := range events {
		if e.Action == "hook_revoked" {
			found = append(found, e)
		}
	}
	if len(found) != 1 || before+1 != auditCount() {
		t.Fatalf("hook_revoked events = %+v", found)
	}
	e := found[0]
	if e.Actor != "principal:1" || e.Subject != principalActor(principal.ID) {
		t.Errorf("audit event = %+v, want actor principal:1 and the hook's principal as subject", e)
	}
	if !strings.Contains(e.Detail, fmt.Sprintf("hook=%d", hook.ID)) || strings.Contains(e.Detail, "to-revoke") || strings.Contains(e.Detail, hashHookToken("to-revoke")) {
		t.Errorf("audit detail = %q", e.Detail)
	}

	list, err := s.ListHooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListHooks = %d hooks, want 2 (revoked ones included)", len(list))
	}
	for _, h := range list {
		switch h.ID {
		case hook.ID:
			if h.RevokedAt == nil || h.Label != `a "quoted" label` || h.Channel != "general" || h.Principal != principal.ID {
				t.Errorf("revoked hook listed as %+v", h)
			}
		case other.ID:
			if h.RevokedAt != nil {
				t.Errorf("active hook listed as revoked: %+v", h)
			}
		}
	}
	if list[0].ID != other.ID {
		t.Errorf("ListHooks order = %d first, want newest (%d) first", list[0].ID, other.ID)
	}
}

func TestListHooksEmptyIsNotNil(t *testing.T) {
	s := openTestStore(t)
	got, err := s.ListHooks(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("ListHooks = %#v, %v; want empty non-nil slice", got, err)
	}
}

func TestRevokeHookConcurrentAuditsOnce(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	hook, err := s.CreateHook(ctx, "racy", channel.ID, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.RevokeHook(ctx, "principal:1", hook.ID); err != nil {
				t.Errorf("RevokeHook: %v", err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action = 'hook_revoked'").Scan(&n); err != nil || n != 1 {
		t.Errorf("hook_revoked events = %d (%v), want 1", n, err)
	}
}

// seedSchema9WithHooks builds a database at schema version 9 (plaintext hook
// tokens) holding the given tokens, and returns its path. When crashCopyDir
// is non-empty the database, its WAL and shm are copied there while the
// original connection is still open, so the copy keeps old page images in its
// WAL the way a crashed process would leave them.
func seedSchema9WithHooks(t *testing.T, dir string, tokens []string, crashCopyDir string) string {
	t.Helper()
	const pre = 9
	path := filepath.Join(dir, "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for i := 0; i < pre; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	stmts := []string{
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'agent', 'bot', 'member', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, created_at) VALUES (1, 2, 4)`,
	}
	for i, tok := range tokens {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO hooks (token, channel_id, principal_id, created_at) VALUES ('%s', 1, 2, %d)`, tok, 10+i))
	}
	stmts = append(stmts, `PRAGMA user_version = 9`)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if crashCopyDir != "" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			data, err := os.ReadFile(path + suffix) // #nosec G304 -- this test's own temp directory
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(crashCopyDir, "conch.db"+suffix), data, 0o600); err != nil { // #nosec G703 -- this test's own temp directory
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertNoTokenBytes(t *testing.T, dir string, tokens []string) {
	t.Helper()
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
		for _, tok := range tokens {
			if bytes.Contains(data, []byte(tok)) {
				t.Errorf("%s contains a plaintext hook token", e.Name())
			}
		}
	}
}

// TestHookMigrationFromSchema9 migrates a database that has plaintext hook
// tokens, both from a cleanly closed file and from a crashed process's
// leftover WAL, and checks old URLs still resolve while no token bytes remain
// anywhere in the data directory.
func TestHookMigrationFromSchema9(t *testing.T) {
	tokens := []string{
		"legacy-token-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"legacy-token-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
	}
	tests := []struct {
		name  string
		crash bool
	}{
		{"cleanly closed database", false},
		{"crashed process left a WAL", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			var path string
			if tt.crash {
				copyDir := t.TempDir()
				seedSchema9WithHooks(t, dir, tokens, copyDir)
				dir = copyDir
				path = filepath.Join(dir, "conch.db")
				if data, err := os.ReadFile(path + "-wal"); err != nil || !bytes.Contains(data, []byte(tokens[0])) { // #nosec G304 -- temp dir
					t.Fatalf("test setup: expected the plaintext token in the leftover WAL (err %v)", err)
				}
			} else {
				path = seedSchema9WithHooks(t, dir, tokens, "")
			}

			s, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("Open (migrate): %v", err)
			}
			for i, tok := range tokens {
				hook, err := s.HookByToken(ctx, tok)
				if err != nil {
					t.Fatalf("old token %d no longer resolves: %v", i, err)
				}
				if hook.ChannelID != 1 || hook.PrincipalID != 2 || hook.CreatedAt.UnixMilli() != int64(10+i) || hook.Label != "" {
					t.Errorf("migrated hook %d = %+v", i, hook)
				}
			}
			if _, err := s.HookByToken(ctx, "not-a-legacy-token"); !errors.Is(err, ErrNotFound) {
				t.Errorf("unknown token after migration: %v", err)
			}
			var version int
			if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
				t.Errorf("user_version = %d (%v), want %d", version, err, len(migrations))
			}
			// The old table is gone and nothing in the new one is a token.
			var n int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 'hooks_plaintext'").Scan(&n); err != nil || n != 0 {
				t.Errorf("hooks_plaintext still exists (%d, %v)", n, err)
			}
			rows, err := s.db.Query("SELECT token_hash, label FROM hooks")
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for rows.Next() {
				var hash, label string
				if err := rows.Scan(&hash, &label); err != nil {
					t.Fatal(err)
				}
				count++
				for _, tok := range tokens {
					if strings.Contains(hash, tok) || strings.Contains(label, tok) {
						t.Errorf("a hooks column holds a plaintext token")
					}
				}
			}
			_ = rows.Close()
			if count != len(tokens) {
				t.Errorf("hooks rows = %d, want %d", count, len(tokens))
			}
			// New hooks work after the migration and ids do not collide.
			if _, err := s.CreateHook(ctx, "post-migration", 1, 2); err != nil {
				t.Errorf("create after migration: %v", err)
			}
			if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			assertNoTokenBytes(t, dir, tokens)
		})
	}
}

func newLogCapture(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Writer(buf), nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestDatabaseFileModes(t *testing.T) {
	ctx := context.Background()
	modeOf := func(t *testing.T, p string) os.FileMode {
		t.Helper()
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}

	t.Run("fresh database is 0600 with 0600 siblings and no log", func(t *testing.T) {
		logs := newLogCapture(t)
		path := filepath.Join(t.TempDir(), "conch.db")
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if _, err := s.CreateChannel(ctx, "general"); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if got := modeOf(t, p); got != 0o600 {
				t.Errorf("%s mode = %04o, want 0600", filepath.Base(p), got)
			}
		}
		if logs.String() != "" {
			t.Errorf("a fresh database logged: %q", logs.String())
		}
	})

	t.Run("existing 0644 database and siblings are tightened with one log line", func(t *testing.T) {
		logs := newLogCapture(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "conch.db")
		// Leave a WAL and shm behind by copying them from a live database.
		src := filepath.Join(t.TempDir(), "conch.db")
		first, err := Open(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := first.CreateChannel(ctx, "general"); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			data, err := os.ReadFile(src + suffix) // #nosec G304 -- this test's own temp directory
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+suffix, data, 0o644); err != nil { // #nosec G306,G703 -- deliberately loose, in this test's own temp directory
				t.Fatal(err)
			}
			if err := os.Chmod(path+suffix, 0o644); err != nil { // #nosec G302 -- deliberately loose
				t.Fatal(err)
			}
		}
		_ = first.Close()
		logs.mu.Lock()
		logs.buf.Reset()
		logs.mu.Unlock()

		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if got := modeOf(t, p); got != 0o600 {
				t.Errorf("%s mode = %04o, want 0600", filepath.Base(p), got)
			}
		}
		if lines := strings.Count(strings.TrimSpace(logs.String()), "\n") + 1; logs.String() == "" || lines != 1 {
			t.Errorf("tightening logged %d lines, want exactly 1: %q", lines, logs.String())
		}
	})

	t.Run("already 0600 logs nothing", func(t *testing.T) {
		logs := newLogCapture(t)
		path := filepath.Join(t.TempDir(), "conch.db")
		for i := 0; i < 2; i++ {
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
		}
		if logs.String() != "" {
			t.Errorf("an already-private database logged: %q", logs.String())
		}
	})

	t.Run("group-readable only (0640) is tightened", func(t *testing.T) {
		newLogCapture(t)
		path := filepath.Join(t.TempDir(), "conch.db")
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		if err := os.Chmod(path, 0o640); err != nil { // #nosec G302 -- deliberately loose
			t.Fatal(err)
		}
		s, err = Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if got := modeOf(t, path); got != 0o600 {
			t.Errorf("mode = %04o, want 0600", got)
		}
	})
}
