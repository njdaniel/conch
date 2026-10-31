package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// migratedDB is the file of a database with every migration applied, made
// once per test binary. Migrating a fresh database takes about a third of a
// second under -race, and most tests here open a store only to use it; each
// of those starts from a copy of this file instead (issue #498, as #472 did
// for the server package). Open still runs on the copy, permission checks and
// version check included. The tests of the migrations themselves never use
// it: they build the schema version they name and migrate it with Open.
var migratedDB struct {
	once sync.Once
	data []byte
	err  error
}

// newTestDBPath returns the path of a database, in a fresh temporary
// directory, that is already migrated to the current schema version.
func newTestDBPath(t *testing.T) string {
	t.Helper()
	migratedDB.once.Do(func() {
		dir, err := os.MkdirTemp("", "conch-store-test-")
		if err != nil {
			migratedDB.err = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "conch.db")
		s, err := Open(context.Background(), path)
		if err != nil {
			migratedDB.err = err
			return
		}
		// Closing the last connection checkpoints the write-ahead log into
		// the file, so the file alone is the whole database.
		if err := s.Close(); err != nil {
			migratedDB.err = err
			return
		}
		migratedDB.data, migratedDB.err = os.ReadFile(path) // #nosec G304 -- a file this helper just made in its own temporary directory
	})
	if migratedDB.err != nil {
		t.Fatalf("make the migrated test database: %v", migratedDB.err)
	}
	path := filepath.Join(t.TempDir(), "conch.db")
	if err := os.WriteFile(path, migratedDB.data, 0o600); err != nil {
		t.Fatalf("copy the migrated test database: %v", err)
	}
	return path
}

// openTestStore opens a copy of the migrated test database.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStoreAt(t, newTestDBPath(t))
}

// openFreshTestStore opens a new, empty database file, so Open runs every
// migration on it, for the tests about what migrating from nothing makes.
func openFreshTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStoreAt(t, filepath.Join(t.TempDir(), "conch.db"))
}

func openTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestMessagePayloadMigrationRoundTripAndAudit(t *testing.T) {
	// Migrated here from an empty file, not copied: this test is about what
	// the migrations make.
	s := openFreshTestStore(t)
	ctx := context.Background()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("schema version = %d, want %d", version, len(migrations))
	}
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePrincipal(ctx, PrincipalAgent, "leviathan")
	if err != nil {
		t.Fatal(err)
	}
	payload := &schema.Payload{Schema: "acme.alert.v1", Data: json.RawMessage(`{"nested":{"x":1}}`)}
	inserted, err := s.InsertMessageV1(ctx, ch.ID, p.ID, "alert", payload)
	if err != nil {
		t.Fatalf("InsertMessageV1: %v", err)
	}
	listed, err := s.ListVisibleMessages(ctx, ch.ID, ChannelWideOnly, 0, 10)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(listed) != 1 || listed[0].Payload == nil || string(listed[0].Payload.Data) != string(payload.Data) {
		t.Fatalf("round trip = %+v", listed)
	}
	events, err := s.ListAuditEvents(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(events) != 1 || events[0].Actor != fmt.Sprintf("principal:%d", p.ID) ||
		events[0].Action != "message.post" || events[0].Subject != fmt.Sprintf("message:%d", inserted.ID) {
		t.Fatalf("audit events = %+v", events)
	}
}

func TestOpenPragmas(t *testing.T) {
	s := openTestStore(t)

	tests := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
	}
	for _, tt := range tests {
		t.Run(tt.pragma, func(t *testing.T) {
			var got string
			if err := s.db.QueryRow("PRAGMA " + tt.pragma).Scan(&got); err != nil {
				t.Fatalf("PRAGMA %s: %v", tt.pragma, err)
			}
			if got != tt.want {
				t.Errorf("PRAGMA %s = %q, want %q", tt.pragma, got, tt.want)
			}
		})
	}
}

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Running migrate again on a live store must be a no-op.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate on open store: %v", err)
	}
	if _, err := s.CreateChannel(ctx, "general"); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening an already-migrated database must succeed and keep data.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}
	var channels int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM channels").Scan(&channels); err != nil {
		t.Fatalf("count channels: %v", err)
	}
	if channels != 1 {
		t.Errorf("channels after reopen = %d, want 1", channels)
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1)); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := Open(ctx, path); err == nil {
		t.Fatal("Open succeeded on a database from a newer schema version, want error")
	}
}

func TestCreatePrincipal(t *testing.T) {
	tests := []struct {
		name    string
		kind    PrincipalKind
		pname   string
		wantErr bool
	}{
		{"human", PrincipalHuman, "nick", false},
		{"agent", PrincipalAgent, "leviathan", false},
		{"invalid kind", PrincipalKind("robot"), "hal", true},
		{"duplicate name", PrincipalHuman, "nick", true},
	}

	s := openTestStore(t)
	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := s.CreatePrincipal(ctx, tt.kind, tt.pname)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CreatePrincipal(%q, %q) succeeded, want error", tt.kind, tt.pname)
				}
				if tt.name == "duplicate name" && !errors.Is(err, ErrDuplicate) {
					t.Errorf("CreatePrincipal(%q, %q) error = %v, want ErrDuplicate", tt.kind, tt.pname, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreatePrincipal(%q, %q): %v", tt.kind, tt.pname, err)
			}
			if p.ID == 0 || p.Kind != tt.kind || p.Name != tt.pname {
				t.Errorf("CreatePrincipal returned %+v", p)
			}
		})
	}
}

func TestCreateChannelDuplicateName(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateChannel(ctx, "general"); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := s.CreateChannel(ctx, "general"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate CreateChannel error = %v, want ErrDuplicate", err)
	}
}

func TestListChannels(t *testing.T) {
	tests := []struct {
		name   string
		create []string
	}{
		{name: "empty"},
		{name: "several ordered by id", create: []string{"zeta", "alpha", "general"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			for _, name := range tt.create {
				if _, err := s.CreateChannel(ctx, name); err != nil {
					t.Fatalf("CreateChannel(%q): %v", name, err)
				}
			}
			got, err := s.ListChannels(ctx)
			if err != nil {
				t.Fatalf("ListChannels: %v", err)
			}
			if got == nil || len(got) != len(tt.create) {
				t.Fatalf("channels = %#v, want %d non-nil entries", got, len(tt.create))
			}
			for i, name := range tt.create {
				if got[i].Name != name {
					t.Errorf("channels[%d] = %q, want %q", i, got[i].Name, name)
				}
				if i > 0 && got[i].ID <= got[i-1].ID {
					t.Errorf("ids not ascending: %+v", got)
				}
			}
		})
	}
}

func TestInsertMessageForeignKeys(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "nick")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}

	tests := []struct {
		name      string
		channelID int64
		authorID  int64
		wantErr   bool
	}{
		{"valid", ch.ID, p.ID, false},
		{"unknown channel", ch.ID + 99, p.ID, true},
		{"unknown author", ch.ID, p.ID + 99, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.InsertMessage(ctx, tt.channelID, tt.authorID, "hello")
			if (err != nil) != tt.wantErr {
				t.Errorf("InsertMessage(channel=%d, author=%d) error = %v, wantErr %v",
					tt.channelID, tt.authorID, err, tt.wantErr)
			}
		})
	}
}

func TestListMessagesOrderingAndPagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	other, err := s.CreateChannel(ctx, "other")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	p, err := s.CreatePrincipal(ctx, PrincipalAgent, "leviathan")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}

	var ids []int64
	for i := 0; i < 5; i++ {
		m, err := s.InsertMessage(ctx, ch.ID, p.ID, fmt.Sprintf("msg %d", i))
		if err != nil {
			t.Fatalf("InsertMessage %d: %v", i, err)
		}
		ids = append(ids, m.ID)
	}
	// A message in another channel must never leak into general's pages.
	if _, err := s.InsertMessage(ctx, other.ID, p.ID, "elsewhere"); err != nil {
		t.Fatalf("InsertMessage other: %v", err)
	}

	tests := []struct {
		name       string
		channelID  int64
		afterID    int64
		limit      int
		wantBodies []string
	}{
		{"all from start", ch.ID, 0, 10, []string{"msg 0", "msg 1", "msg 2", "msg 3", "msg 4"}},
		{"first page", ch.ID, 0, 2, []string{"msg 0", "msg 1"}},
		{"second page", ch.ID, ids[1], 2, []string{"msg 2", "msg 3"}},
		{"last partial page", ch.ID, ids[3], 2, []string{"msg 4"}},
		{"past the end", ch.ID, ids[4], 2, nil},
		{"empty channel", other.ID + 99, 0, 10, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListVisibleMessages(ctx, tt.channelID, ChannelWideOnly, tt.afterID, tt.limit)
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			var bodies []string
			for _, m := range got {
				bodies = append(bodies, m.Body)
			}
			if strings.Join(bodies, "|") != strings.Join(tt.wantBodies, "|") {
				t.Errorf("ListMessages bodies = %v, want %v", bodies, tt.wantBodies)
			}
			for i := 1; i < len(got); i++ {
				if got[i].ID <= got[i-1].ID {
					t.Errorf("ListMessages not in ascending ID order: %d after %d", got[i].ID, got[i-1].ID)
				}
			}
		})
	}

	if _, err := s.ListVisibleMessages(ctx, ch.ID, ChannelWideOnly, 0, 0); err == nil {
		t.Error("ListMessages with limit 0 succeeded, want error")
	}
}

func TestAuditAppendAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first, err := s.AppendAuditEvent(ctx, "system", "server.start", "", "")
	if err != nil {
		t.Fatalf("AppendAuditEvent: %v", err)
	}
	second, err := s.AppendAuditEvent(ctx, "principal:1", "channel.create", "channel:1", `{"name":"general"}`)
	if err != nil {
		t.Fatalf("AppendAuditEvent: %v", err)
	}
	if second.ID <= first.ID {
		t.Errorf("audit IDs not increasing: %d then %d", first.ID, second.ID)
	}

	events, err := s.ListAuditEvents(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ListAuditEvents returned %d events, want 2", len(events))
	}
	if events[0].Action != "server.start" || events[1].Action != "channel.create" {
		t.Errorf("ListAuditEvents order = [%s, %s], want [server.start, channel.create]",
			events[0].Action, events[1].Action)
	}
	if events[1].Actor != "principal:1" || events[1].Subject != "channel:1" || events[1].Detail != `{"name":"general"}` {
		t.Errorf("ListAuditEvents[1] = %+v", events[1])
	}

	paged, err := s.ListAuditEvents(ctx, first.ID, 10)
	if err != nil {
		t.Fatalf("ListAuditEvents after %d: %v", first.ID, err)
	}
	if len(paged) != 1 || paged[0].ID != second.ID {
		t.Errorf("ListAuditEvents after first = %+v, want just the second event", paged)
	}
}

// The store API exposes no update or delete for audit events; the schema
// triggers back that up against raw SQL too.
func TestAuditEventsAppendOnlyTriggers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.AppendAuditEvent(ctx, "system", "server.start", "", ""); err != nil {
		t.Fatalf("AppendAuditEvent: %v", err)
	}

	tests := []struct {
		name string
		stmt string
	}{
		{"update", "UPDATE audit_events SET action = 'tampered'"},
		{"delete", "DELETE FROM audit_events"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.db.Exec(tt.stmt); err == nil {
				t.Fatalf("%s on audit_events succeeded, want append-only trigger to abort", tt.name)
			}
		})
	}
}

func TestOwnerMarkMigration(t *testing.T) {
	const pre = ownerMarkMigration - 1
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
		`INSERT INTO channels (id, name, name_key, name_skel, created_at) VALUES (1, 'general', 'general', 'general', 1)`,
		`INSERT INTO principals (id, kind, name, name_key, name_skel, role, created_at) VALUES
			(1, 'human', 'root', 'root', 'root', 'operator', 1),
			(2, 'human', 'deputy', 'deputy', 'deputy', 'operator', 2),
			(3, 'human', 'alice', 'alice', 'alice', 'member', 3),
			(4, 'agent', 'bot', 'bot', 'bot', 'member', 4)`,
		`INSERT INTO credentials (principal_id, label, token_hash, created_at) VALUES
			(1, 'c1', 'h1', 10),
			(1, 'c2', 'h2', 11),
			(2, 'c3', 'h3', 12)`,
		`INSERT INTO credentials (principal_id, label, token_hash, created_at, revoked_at) VALUES
			(2, 'dead', 'h4', 13, 14)`,
		`INSERT INTO hooks (token_hash, label, channel_id, principal_id, created_at) VALUES
			('hk1', 'ci', 1, 1, 20),
			('hk2', 'deploy', 1, 2, 21),
			('hk3', 'alerts', 1, 2, 22)`,
		`INSERT INTO hooks (token_hash, label, channel_id, principal_id, created_at, revoked_at) VALUES
			('hk4', 'old', 1, 1, 23, 24)`,
		fmt.Sprintf("PRAGMA user_version = %d", pre),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer func() { _ = s.Close() }()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d (%v), want %d", version, err, len(migrations))
	}

	now := time.Now()
	for _, want := range []struct {
		id    int64
		role  Role
		owner bool
	}{
		{1, RoleOperator, true},
		{2, RoleOperator, true},
		{3, RoleMember, false},
		{4, RoleMember, false},
	} {
		p, err := s.PrincipalByID(ctx, want.id)
		if err != nil {
			t.Fatalf("PrincipalByID(%d): %v", want.id, err)
		}
		if p.Role != want.role || p.Owner != want.owner || p.IsOwner() != want.owner {
			t.Errorf("principal %d = %+v (IsOwner=%v), want role=%s owner=%v", want.id, p, p.IsOwner(), want.role, want.owner)
		}
		adm, err := s.PrincipalAdmin(ctx, want.id, now)
		if err != nil || adm.Owner != want.owner {
			t.Errorf("PrincipalAdmin(%d) = %+v, %v; want Owner=%v", want.id, adm, err, want.owner)
		}
	}

	for _, bad := range []string{
		`UPDATE principals SET owner = 1 WHERE id = 3`,
		`UPDATE principals SET role = 'member' WHERE id = 1`,
		`UPDATE principals SET owner = 2 WHERE id = 1`,
		`UPDATE principals SET owner = -1 WHERE id = 1`,
		`INSERT INTO principals (kind, name, name_key, name_skel, role, owner, created_at) VALUES ('human', 'bad', 'bad', 'bad', 'member', 1, 99)`,
	} {
		if _, err := s.db.Exec(bad); err == nil {
			t.Errorf("CHECK constraint allowed %q", bad)
		}
	}

	logged := logBuf.String()
	for _, want := range []string{
		"principal_id=1 live_credentials=2 hooks_bound=1",
		"principal_id=2 live_credentials=1 hooks_bound=2",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("migration log %q does not contain %q", logged, want)
		}
	}

	var auditCount int
	// The permissions migration (39) audits permissions_initialized on an
	// upgraded database; this migration audits nothing.
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action != ?", AuditPermissionsInitialized).Scan(&auditCount); err != nil || auditCount != 0 {
		t.Errorf("audit_events count = %d (%v), want 0", auditCount, err)
	}
}
