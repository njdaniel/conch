package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "conch.db"))
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
			got, err := s.ListMessages(ctx, tt.channelID, tt.afterID, tt.limit)
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

	if _, err := s.ListMessages(ctx, ch.ID, 0, 0); err == nil {
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
