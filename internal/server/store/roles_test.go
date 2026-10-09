package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBootstrapOperatorFresh(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	p, cred, token, err := s.BootstrapOperator(ctx, "nick")
	if err != nil {
		t.Fatalf("BootstrapOperator: %v", err)
	}
	if p.Kind != PrincipalHuman || p.Role != RoleOperator || p.Name != "nick" {
		t.Errorf("principal = %+v", p)
	}
	if cred.Label != BootstrapCredentialLabel || cred.PrincipalID != p.ID {
		t.Errorf("credential = %+v", cred)
	}
	if !tokenShape.MatchString(token) {
		t.Errorf("token has the wrong shape")
	}

	got, err := s.ResolveCredential(ctx, token)
	if err != nil || got.ID != p.ID || got.Role != RoleOperator {
		t.Errorf("ResolveCredential = %+v, %v; want the operator", got, err)
	}
	byID, err := s.PrincipalByID(ctx, p.ID)
	if err != nil || byID.Role != RoleOperator {
		t.Errorf("PrincipalByID role = %q (%v)", byID.Role, err)
	}

	events, err := s.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
		for _, field := range []string{e.Actor, e.Subject, e.Detail} {
			if strings.Contains(field, token) || strings.Contains(field, hashCredentialToken(token)) {
				t.Errorf("audit event %q contains token material", e.Action)
			}
		}
	}
	if strings.Join(actions, ",") != "operator_bootstrapped,credential_created" {
		t.Errorf("audit actions = %v", actions)
	}
}

func TestBootstrapOperatorRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, s *Store)
		call  string
		want  error
	}{
		{
			name: "operator already exists",
			setup: func(t *testing.T, s *Store) {
				if _, _, _, err := s.BootstrapOperator(context.Background(), "first"); err != nil {
					t.Fatal(err)
				}
			},
			call: "second",
			want: ErrOperatorExists,
		},
		{
			name: "same name as the existing operator",
			setup: func(t *testing.T, s *Store) {
				if _, _, _, err := s.BootstrapOperator(context.Background(), "first"); err != nil {
					t.Fatal(err)
				}
			},
			call: "first",
			want: ErrOperatorExists,
		},
		{
			name: "name taken by a member",
			setup: func(t *testing.T, s *Store) {
				if _, err := s.CreatePrincipal(context.Background(), PrincipalHuman, "taken"); err != nil {
					t.Fatal(err)
				}
			},
			call: "taken",
			want: ErrDuplicate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			tt.setup(t, s)
			counts := func() [4]int {
				return [4]int{countRows(t, s, "principals"), countRows(t, s, "credentials"),
					countRows(t, s, "audit_events"), countRows(t, s, "channels")}
			}
			before := counts()
			_, _, token, err := s.BootstrapOperator(context.Background(), tt.call)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if token != "" {
				t.Error("a refused bootstrap returned a token")
			}
			if after := counts(); after != before {
				t.Errorf("row counts changed %v -> %v", before, after)
			}
		})
	}
}

// Two concurrent bootstraps produce exactly one operator.
func TestBootstrapOperatorConcurrent(t *testing.T) {
	s := openTestStore(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, errs[i] = s.BootstrapOperator(context.Background(), fmt.Sprintf("op%d", i))
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrOperatorExists):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d bootstraps succeeded, want exactly 1", ok)
	}
	var operators int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM principals WHERE role = 'operator'").Scan(&operators); err != nil || operators != 1 {
		t.Errorf("operators = %d (%v)", operators, err)
	}
}

func TestCreatePrincipalIsMember(t *testing.T) {
	s := openTestStore(t)
	for _, kind := range []PrincipalKind{PrincipalHuman, PrincipalAgent} {
		p, err := s.CreatePrincipal(context.Background(), kind, "p-"+string(kind))
		if err != nil || p.Role != RoleMember {
			t.Fatalf("CreatePrincipal = %+v, %v", p, err)
		}
		got, err := s.PrincipalByID(context.Background(), p.ID)
		if err != nil || got.Role != RoleMember {
			t.Errorf("stored role = %q (%v)", got.Role, err)
		}
	}
}

func TestRoleColumnRejectsUnknownRole(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO principals (kind, name, role, created_at) VALUES ('human','x','root',1)`); err == nil {
		t.Error("an unknown role was accepted")
	}
}

// TestRoleMigrationFromSchema6 builds a version 6 database (with credentials)
// and checks every existing principal becomes a member, none an operator.
func TestRoleMigrationFromSchema6(t *testing.T) {
	const preRole = 6
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < preRole; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, created_at) VALUES (1, 'human', 'nick', 1), (2, 'agent', 'bot', 2)`,
		`INSERT INTO credentials (principal_id, label, token_hash, created_at) VALUES (1, 'old', 'h1', 3)`,
		`PRAGMA user_version = 6`,
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
		if err != nil || p.Role != RoleMember {
			t.Errorf("principal %d role = %q (%v), want member", id, p.Role, err)
		}
	}
	var operators int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM principals WHERE role = 'operator'").Scan(&operators); err != nil || operators != 0 {
		t.Errorf("operators after migration = %d (%v), want 0", operators, err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d (%v), want %d", version, err, len(migrations))
	}
	// The first operator can still be bootstrapped on the migrated database.
	if _, _, _, err := s.BootstrapOperator(ctx, "root"); err != nil {
		t.Errorf("bootstrap after migration: %v", err)
	}
}
