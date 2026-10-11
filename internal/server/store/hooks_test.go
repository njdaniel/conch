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
	"strconv"
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
	created, err := s.CreateHookWithLabel(ctx, "system", "test-token", "ci", channel.ID, principal.ID)
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
	if _, err := s.CreateHook(ctx, "system", "same", channel.ID, principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHook(ctx, "system", "same", channel.ID, principal.ID); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate token error = %v, want ErrDuplicate", err)
	}
}

// hookCreatedEvents returns the hook_created events of the store.
func hookCreatedEvents(t *testing.T, s *Store) []AuditEvent {
	t.Helper()
	events, err := s.ListAuditEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found []AuditEvent
	for _, e := range events {
		if e.Action == "hook_created" {
			found = append(found, e)
		}
	}
	return found
}

// TestCreateHookWritesHookCreated: one successful create appends exactly one
// hook_created event with the caller as actor, the hook's principal as
// subject and hook=<id> channel=<id> label=<quoted label> as detail.
func TestCreateHookWritesHookCreated(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	hook, err := s.CreateHookWithLabel(ctx, "principal:1", "audit-token", "ci builds", channel.ID, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := hookCreatedEvents(t, s)
	if len(found) != 1 {
		t.Fatalf("hook_created events = %+v, want exactly one", found)
	}
	e := found[0]
	wantDetail := fmt.Sprintf("hook=%d channel=%d label=%q", hook.ID, channel.ID, "ci builds")
	if e.Actor != "principal:1" || e.Subject != principalActor(principal.ID) || e.Detail != wantDetail {
		t.Errorf("audit event = %+v, want actor principal:1, subject %q, detail %q",
			e, principalActor(principal.ID), wantDetail)
	}
	if !e.CreatedAt.Equal(hook.CreatedAt) {
		t.Errorf("event time %v, want the hook's created_at %v", e.CreatedAt, hook.CreatedAt)
	}
}

// TestCreateHookFailureWritesNeitherRowNorEvent: a create that fails — unknown
// channel, unknown principal, duplicate token — leaves no hook row and no
// audit event.
func TestCreateHookFailureWritesNeitherRowNorEvent(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	if _, err := s.CreateHook(ctx, "system", "taken", channel.ID, principal.ID); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name                   string
		token                  string
		channelID, principalID int64
	}{
		{"unknown channel", "fresh-token-1", 9999, principal.ID},
		{"unknown principal", "fresh-token-2", channel.ID, 9999},
		{"duplicate token", "taken", channel.ID, principal.ID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooks := countRows(t, s, "hooks")
			events := countRows(t, s, "audit_events")
			if _, err := s.CreateHook(ctx, "system", tt.token, tt.channelID, tt.principalID); err == nil {
				t.Fatal("CreateHook succeeded, want an error")
			}
			if got := countRows(t, s, "hooks"); got != hooks {
				t.Errorf("hooks rows = %d, want %d (the failed create left a row)", got, hooks)
			}
			if got := countRows(t, s, "audit_events"); got != events {
				t.Errorf("audit events = %d, want %d (the failed create wrote an event)", got, events)
			}
		})
	}
}

// TestCreateHookAuditFailureLeavesNoHook: the audit append shares the hook
// insert's transaction, so when it cannot be written there is no hook either
// (the same seam TestRevokeAllCredentialsAtomic uses for credentials).
func TestCreateHookAuditFailureLeavesNoHook(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events
		WHEN NEW.action = 'hook_created' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHook(ctx, "system", "unaudited", channel.ID, principal.ID); err == nil {
		t.Fatal("CreateHook succeeded with the audit table broken")
	}
	if n := countRows(t, s, "hooks"); n != 0 {
		t.Errorf("hooks rows = %d, want 0: a failed audit must not leave a hook", n)
	}
	if _, err := s.HookByToken(ctx, "unaudited"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the unaudited hook resolves: %v", err)
	}
}

// TestHookDetailQuotesTheLabel: a label holding a quote, a newline and an
// equals sign still yields a one-line detail. A reader recovers the label by
// taking everything after the final "label=" and applying strconv.Unquote;
// because the label comes last and is quoted, the ids parse unambiguously
// even when the label itself contains "channel=" or a newline.
func TestHookDetailQuotesTheLabel(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	const label = "ci \"nightly\"\nchannel=alpha"
	hook, err := s.CreateHookWithLabel(ctx, "system", "quoted-label-token", label, channel.ID, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := hookCreatedEvents(t, s)
	if len(found) != 1 {
		t.Fatalf("hook_created events = %+v, want exactly one", found)
	}
	detail := found[0].Detail
	if strings.ContainsAny(detail, "\n\r") {
		t.Errorf("detail spans lines: %q", detail)
	}
	prefix := fmt.Sprintf("hook=%d channel=%d label=", hook.ID, channel.ID)
	rest, ok := strings.CutPrefix(detail, prefix)
	if !ok {
		t.Fatalf("detail = %q, want prefix %q", detail, prefix)
	}
	got, err := strconv.Unquote(rest)
	if err != nil || got != label {
		t.Errorf("Unquote(%q) = %q, %v; want the label back", rest, got, err)
	}
}

// TestHookStoresOnlyTheHash checks no column of the hooks table holds the
// token, and that the stored value is the SHA-256 hex digest.
func TestHookStoresOnlyTheHash(t *testing.T) {
	s, channel, principal := hookFixture(t)
	ctx := context.Background()
	const token = "plaintext-hook-token-do-not-store" // #nosec G101 -- test input, not a credential
	if _, err := s.CreateHookWithLabel(ctx, "system", token, "label", channel.ID, principal.ID); err != nil {
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
	hook, err := s.CreateHookWithLabel(ctx, "system", "to-revoke", `a "quoted" label`, channel.ID, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateHook(ctx, "system", "bystander", channel.ID, principal.ID)
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
	hook, err := s.CreateHook(ctx, "system", "racy", channel.ID, principal.ID)
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
			writeFileT(t, filepath.Join(crashCopyDir, "conch.db"+suffix), data, 0o600)
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
				if !bytes.Contains(readFileT(t, path+"-wal"), []byte(tokens[0])) {
					t.Fatal("test setup: expected the plaintext token in the leftover WAL")
				}
			} else {
				path = seedSchema9WithHooks(t, dir, tokens, "")
			}

			s, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("Open (migrate): %v", err)
			}
			// While the database is still open: Close would checkpoint and
			// delete the WAL itself, hiding a missing checkpoint in Open.
			assertNoTokenBytes(t, dir, tokens)
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
			if _, err := s.CreateHook(ctx, "system", "post-migration", 1, 2); err != nil {
				t.Errorf("create after migration: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			assertNoTokenBytes(t, dir, tokens)
		})
	}
}

// The start-up checkpoint does not depend on a migration having just run. A
// process that died after migration 10 committed but before the checkpoint
// leaves a database already at the current version with the plaintext still
// in its WAL; the next Open must clear it all the same.
func TestOpenTruncatesTheWALOnEveryStart(t *testing.T) {
	ctx := context.Background()
	const marker = "leftover-wal-frame-marker-0123456789"
	dir := t.TempDir()
	path := filepath.Join(dir, "conch.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel(ctx, marker); err != nil {
		t.Fatal(err)
	}
	// Copy the files as a crashed process would leave them: WAL not folded in.
	crashed := t.TempDir()
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix) // #nosec G304 -- temp dir
		if err != nil {
			t.Fatal(err)
		}
		writeFileT(t, filepath.Join(crashed, "conch.db"+suffix), data, 0o600)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wal := filepath.Join(crashed, "conch.db-wal")
	if !bytes.Contains(readFileT(t, wal), []byte(marker)) {
		t.Fatal("test setup: expected the marker in the leftover WAL")
	}

	reopened, err := Open(ctx, filepath.Join(crashed, "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if info, err := os.Stat(wal); err != nil || info.Size() != 0 {
		t.Errorf("WAL after Open: size %v (err %v), want it truncated to 0", info, err)
	}
	// Nothing was lost: the channel written before the crash is there.
	if _, err := reopened.ChannelByName(ctx, marker); err != nil {
		t.Errorf("data from the WAL was not recovered: %v", err)
	}
}

// A checkpoint blocked by another process reports "busy" in its result row,
// not as an error. That must be noticed and logged, and must not stop startup.
func TestTruncateWALReportsABlockedCheckpoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	// A second process holding a read transaction pins the WAL.
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	reader, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback() }()
	var n int
	if err := reader.QueryRowContext(ctx, "SELECT count(*) FROM channels").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel(ctx, "written-while-pinned"); err != nil {
		t.Fatal(err)
	}

	logs := newLogCapture(t)
	if err := s.truncateWAL(ctx); err != nil {
		t.Fatalf("a blocked checkpoint must not be an error: %v", err)
	}
	if !strings.Contains(logs.String(), "could not truncate the write-ahead log") {
		t.Errorf("a blocked checkpoint was not logged: %q", logs.String())
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

// The permission change goes through an open handle on a regular file. A
// directory or other non-file where the database should be is refused, and a
// data directory other users can write to is reported, since they could
// replace the files whatever their modes.
func TestDatabasePathHazards(t *testing.T) {
	ctx := context.Background()
	t.Run("a directory where the WAL should be is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conch.db")
		if err := os.Mkdir(path+"-wal", 0o700); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(ctx, path); err == nil {
			_ = s.Close()
			t.Fatal("Open succeeded with a directory in place of the WAL")
		}
	})
	t.Run("a symlinked database tightens the file it points to", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "real.db")
		writeFileT(t, target, nil, 0o644)
		chmodT(t, target, 0o644)
		path := filepath.Join(dir, "conch.db")
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("target mode = %v (err %v), want 0600", info.Mode().Perm(), err)
		}
	})
	t.Run("a data directory writable by others is reported once", func(t *testing.T) {
		dir := t.TempDir()
		chmodT(t, dir, 0o777)
		logs := newLogCapture(t)
		s, err := Open(ctx, filepath.Join(dir, "conch.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if n := strings.Count(logs.String(), "data directory is writable by other users"); n != 1 {
			t.Errorf("warnings = %d, want 1: %q", n, logs.String())
		}
	})
}

// The three helpers below keep each file call on a line of its own. The lint
// exceptions they need are honoured there by every gosec version in use; a
// comment after the brace of an if statement is not.

func readFileT(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p) // #nosec G304 -- a path inside this test's own temp directory
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeFileT and chmodT take whatever mode the test asks for, including
// deliberately loose ones: tightening them is what is under test.
func writeFileT(t *testing.T, p string, data []byte, mode os.FileMode) {
	t.Helper()
	err := os.WriteFile(p, data, mode) // #nosec G306,G703 -- test-chosen mode, in this test's own temp directory
	if err != nil {
		t.Fatal(err)
	}
}

func chmodT(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	err := os.Chmod(p, mode) // #nosec G302 -- test-chosen mode, to exercise tightening
	if err != nil {
		t.Fatal(err)
	}
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
			writeFileT(t, path+suffix, data, 0o644)
			chmodT(t, path+suffix, 0o644)
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
		chmodT(t, path, 0o640)
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
