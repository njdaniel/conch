package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// communityMigration is the store's migration 40 (issue #979), which this
// test runs over a database that held rows before it.
const communityMigration = 40

// Issue #979, the whole chain after migration 40 (CLAUDE.md rule 3):
// request, notify, resolve, audit, through the REST API of a server whose
// database was converted to the community model with rows in it. bob, who
// was in no channel but #belgium, was put in #general (the open channel,
// now a default channel) by the migration, without the right to decide: he
// sees the approval read-only and is refused, audited. The owner keeps the
// right it held, and its one decision resolves the approval, as the quorum
// says it should.
func TestCommunityMigrationApprovalChain(t *testing.T) {
	ctx := context.Background()
	path := newTestDBPath(t)

	// A server at 39: an owner with the right in #general, alice in it with
	// the right too, bob in #belgium only, and #general the open channel.
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	root, _, rootTok, err := st.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) (store.Principal, string) {
		p, err := st.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := st.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		return p, tok
	}
	alice, aliceTok := mk("alice")
	bob, bobTok := mk("bob")
	general, err := st.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	belgium, err := st.CreateChannel(ctx, "belgium")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	for _, m := range []struct {
		ch store.Channel
		p  store.Principal
	}{{general, root}, {general, alice}, {belgium, bob}} {
		if _, err := st.PutChannelMember(ctx, "system", m.ch.ID, m.p.ID, 0, &yes); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		// Undo migration 40's DDL: the database is as a server at 39 held it.
		"DROP TRIGGER channels_default_public_insert",
		"DROP TRIGGER channels_default_public_update",
		"ALTER TABLE channels DROP COLUMN visibility",
		"ALTER TABLE channels DROP COLUMN is_default",
		"ALTER TABLE channels DROP COLUMN default_listed_by",
		"DROP TABLE legacy_history_bounds",
		"ALTER TABLE invites DROP COLUMN link",
		"DELETE FROM server_settings WHERE key IN ('open_signup', 'member_link_default_expiry_seconds')",
		fmt.Sprintf("INSERT INTO open_channels (channel_id, added_by, added_at) VALUES (%d, %d, 0)", general.ID, root.ID),
		`INSERT INTO server_settings (key, value, updated_at) VALUES ('open_joining', 'account', 0)`,
		fmt.Sprintf("PRAGMA user_version = %d", communityMigration-1),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Open it: migration 40 runs. Then serve it.
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("store.Open (migrate): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var mu sync.Mutex
	var notified []string
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		notified = append(notified, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ntfy.Close()
	srv := New(Config{AuthMode: AuthRequired, DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "v1.2.3-test",
		Ntfy: approvals.NtfyConfig{Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second}}, st)
	defer srv.approvals.Close()
	testDBPaths.Store(srv, path)
	f := &authFixture{srv: srv, root: root, rootTok: rootTok, alice: alice, aliceTok: aliceTok}

	// The migration's writes: bob in #general without the right; the owner
	// and alice keep theirs.
	members, err := st.ListChannelMembers(ctx, general.ID)
	if err != nil {
		t.Fatal(err)
	}
	decides := map[int64]bool{}
	for _, m := range members {
		decides[m.PrincipalID] = m.CanDecide
	}
	if d, in := decides[bob.ID]; !in || d {
		t.Fatalf("bob in general = %v, can_decide %v; want joined without the right", in, d)
	}
	if !decides[root.ID] || !decides[alice.ID] {
		t.Fatalf("rights after the migration = %v; want the owner's and alice's kept", decides)
	}

	// Request.
	rec := f.do(t, "POST", "/v1/approvals", aliceTok, createApprovalBody(general.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	path2 := fmt.Sprintf("/v1/approvals/%d/decisions", id)

	// Notify.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(notified)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the approval was never notified")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// bob sees it, read-only, and is refused, audited; it stays pending.
	list := decodeBody[schema.ListApprovalsResponseV1](t, f.do(t, "GET", "/v1/approvals", bobTok, ""))
	if len(list.Approvals) != 1 || list.Approvals[0].ID != id || len(list.ReadOnly) != 1 || list.ReadOnly[0] != id {
		t.Fatalf("bob's list = %+v, want the approval, read-only", list)
	}
	assertErrorBody(t, f.do(t, "POST", path2, bobTok, decisionBody(0)), http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted)
	if a, err := st.ApprovalByID(ctx, id); err != nil || a.State != schema.ApprovalStatePending {
		t.Fatalf("after bob's refusal approval = %+v, %v; want pending", a, err)
	}

	// Resolve: the owner's one decision meets the quorum.
	rec = f.do(t, "POST", path2, rootTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("the owner's decision = %d %s", rec.Code, rec.Body)
	}
	if resp := decodeBody[schema.CastDecisionResponseV1](t, rec); resp.State != schema.ApprovalStateResolved {
		t.Fatalf("state = %s, want resolved", resp.State)
	}

	// Audit: the migration's row, bob's refusal, then created by alice,
	// decided by the owner, resolved, in that order.
	var chain []string
	refused, migrated := false, 0
	for _, e := range f.audit(t) {
		switch {
		case e.Action == store.AuditCommunityMigrated:
			migrated++
		case e.Action == "access_denied" && e.Actor == fmt.Sprintf("principal:%d", bob.ID) && strings.Contains(e.Detail, schema.ErrorCodeDecisionNotPermitted):
			refused = true
		case e.Action == store.AuditDecisionCast && e.Actor == fmt.Sprintf("principal:%d", bob.ID):
			t.Error("bob's refused decision was cast")
		case (e.Action == store.AuditApprovalCreated || e.Action == store.AuditDecisionCast || e.Action == store.AuditApprovalResolved) &&
			e.Subject == fmt.Sprintf("approval:%d", id):
			chain = append(chain, e.Action+" "+e.Actor)
		}
	}
	if migrated != 1 {
		t.Errorf("community_migrated rows = %d, want 1", migrated)
	}
	if !refused {
		t.Error("bob's refusal was not audited")
	}
	want := []string{
		store.AuditApprovalCreated + " " + fmt.Sprintf("principal:%d", alice.ID),
		store.AuditDecisionCast + " " + fmt.Sprintf("principal:%d", root.ID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Errorf("audit chain = %q, want %q", chain, want)
	}
}
