package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func manifestRequest() schema.PutAgentManifestRequestV1 {
	return schema.PutAgentManifestRequestV1{
		DisplayName:  "Leviathan",
		Tier:         schema.AgentTierA,
		Capabilities: []schema.Capability{schema.CapabilityMessagesPost, schema.CapabilityMessagesRead},
		Channels: []schema.ChannelGrant{
			{ChannelID: 0, Permissions: []schema.ChannelPermission{schema.ChannelPermissionPost, schema.ChannelPermissionRead}},
		},
		RateLimits: []schema.RateLimit{{Capability: schema.CapabilityMessagesPost, Max: 60, WindowSeconds: 60}},
	}
}

func TestAgentManifestCreateReadReplace(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "leviathan")
	if err != nil {
		t.Fatal(err)
	}
	req := manifestRequest()
	req.Channels[0].ChannelID = ch.ID

	if _, err := s.AgentManifestByPrincipal(ctx, agent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read before create error = %v, want ErrNotFound", err)
	}

	created, wasCreated, err := s.PutAgentManifest(ctx, "system", agent.ID, req)
	if err != nil || !wasCreated {
		t.Fatalf("create = (%v, %v)", wasCreated, err)
	}
	if err := created.Validate(); err != nil {
		t.Fatalf("created manifest invalid: %v", err)
	}
	read, err := s.AgentManifestByPrincipal(ctx, agent.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(read, created) {
		t.Errorf("read = %+v, want %+v", read, created)
	}
	// Lists are stored as given: no reordering.
	if read.Capabilities[0] != schema.CapabilityMessagesPost || read.Channels[0].Permissions[0] != schema.ChannelPermissionPost {
		t.Errorf("lists were reordered: %+v", read)
	}

	// Replacement is total: omitted lists become empty, created_at is kept,
	// updated_at advances.
	repl := schema.PutAgentManifestRequestV1{DisplayName: "Leviathan 2", Tier: schema.AgentTierC}
	replaced, wasCreated, err := s.PutAgentManifest(ctx, "system", agent.ID, repl)
	if err != nil || wasCreated {
		t.Fatalf("replace = (%v, %v)", wasCreated, err)
	}
	if replaced.CreatedAt != created.CreatedAt {
		t.Errorf("created_at = %v, want preserved %v", replaced.CreatedAt, created.CreatedAt)
	}
	if !replaced.UpdatedAt.Time().After(created.UpdatedAt.Time()) {
		t.Errorf("updated_at = %v, want after %v", replaced.UpdatedAt, created.UpdatedAt)
	}
	read, err = s.AgentManifestByPrincipal(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.DisplayName != "Leviathan 2" || read.Tier != schema.AgentTierC ||
		read.Capabilities != nil || read.Channels != nil || read.RateLimits != nil {
		t.Errorf("replacement not total: %+v", read)
	}

	events, err := s.ListAuditEvents(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Action != "manifest_created" || events[1].Action != "manifest_replaced" ||
		events[0].Subject != "principal:1" || events[0].Actor != "system" {
		t.Errorf("audit events = %+v", events)
	}
}

func TestPutAgentManifestRejects(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ch, _ := s.CreateChannel(ctx, "general")
	agent, _ := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	human, _ := s.CreatePrincipal(ctx, PrincipalHuman, "nick")

	withChannel := func(id int64) schema.PutAgentManifestRequestV1 {
		r := manifestRequest()
		r.Channels[0].ChannelID = id
		return r
	}
	tests := []struct {
		name      string
		principal int64
		req       schema.PutAgentManifestRequestV1
		check     func(error) bool
	}{
		{"missing principal", 999, withChannel(ch.ID), func(err error) bool { return errors.Is(err, ErrPrincipalNotFound) }},
		{"human principal", human.ID, withChannel(ch.ID), func(err error) bool { return errors.Is(err, ErrPrincipalNotAgent) }},
		{"missing channel", agent.ID, withChannel(404), func(err error) bool {
			var c *ChannelNotFoundError
			return errors.As(err, &c) && c.ChannelID == 404
		}},
		{"invalid manifest", agent.ID, schema.PutAgentManifestRequestV1{Tier: "Z"}, func(err error) bool { return err != nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := s.PutAgentManifest(ctx, "system", tt.principal, tt.req); !tt.check(err) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	events, err := s.ListAuditEvents(ctx, 0, 10)
	if err != nil || len(events) != 0 {
		t.Errorf("audit events after failures = %v, %v; want none", events, err)
	}
	if _, err := s.AgentManifestByPrincipal(ctx, agent.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("manifest exists after failed puts: %v", err)
	}
}

func TestAgentManifestSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := s.CreateChannel(ctx, "general")
	agent, _ := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	req := manifestRequest()
	req.Channels[0].ChannelID = ch.ID
	want, _, err := s.PutAgentManifest(ctx, "system", agent.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.AgentManifestByPrincipal(ctx, agent.ID)
	if err != nil {
		t.Fatalf("read after restart: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after restart = %+v, want %+v", got, want)
	}
}

func TestAgentManifestInvalidStoredRowFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"corrupt capabilities json", `UPDATE agent_manifests SET capabilities = '{not json'`},
		{"corrupt channels json", `UPDATE agent_manifests SET channels = '['`},
		{"wrong json type", `UPDATE agent_manifests SET rate_limits = '"x"'`},
		{"unknown field", `UPDATE agent_manifests SET rate_limits = '[{"capability":"messages.post","max":1,"window_seconds":1,"burst":9}]'`},
		{"unknown capability", `UPDATE agent_manifests SET capabilities = '["*"]'`},
		{"duplicate capability", `UPDATE agent_manifests SET capabilities = '["messages.read","messages.read"]'`},
		{"bad tier", `UPDATE agent_manifests SET tier = 'Z'`},
		{"blank display name", `UPDATE agent_manifests SET display_name = '  '`},
		{"zero rate limit", `UPDATE agent_manifests SET rate_limits = '[{"capability":"messages.post","max":0,"window_seconds":1}]'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			ch, _ := s.CreateChannel(ctx, "general")
			agent, _ := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
			req := manifestRequest()
			req.Channels[0].ChannelID = ch.ID
			if _, _, err := s.PutAgentManifest(ctx, "system", agent.ID, req); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(tt.sql); err != nil {
				t.Fatalf("corrupt row: %v", err)
			}
			m, err := s.AgentManifestByPrincipal(ctx, agent.ID)
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("error = %v, want ErrInvalidManifest", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("invalid row reported as not found")
			}
			if !reflect.DeepEqual(m, schema.AgentManifestV1{}) {
				t.Errorf("returned usable manifest %+v alongside error", m)
			}
		})
	}
}

// TestManifestMigrationFromPreManifestSchema builds a database at schema
// version 4 (before agent_manifests) and checks the upgrade adds the table
// without creating manifests or disturbing existing rows.
func TestManifestMigrationFromPreManifestSchema(t *testing.T) {
	const preManifest = 4
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < preManifest; i++ {
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
		`PRAGMA user_version = 4`,
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

	counts := []struct {
		table string
		want  int
	}{{"agent_manifests", 0}, {"principals", 2}, {"channels", 1}, {"messages", 1}}
	for _, c := range counts {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + c.table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if n != c.want {
			t.Errorf("%s rows = %d, want %d", c.table, n, c.want)
		}
	}
	// The existing agent has no manifest: the explicit default.
	if _, err := s.AgentManifestByPrincipal(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("existing agent manifest error = %v, want ErrNotFound", err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d (%v), want %d", version, err, len(migrations))
	}
}
