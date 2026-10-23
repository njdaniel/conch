package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestBootstrapOperatorFresh(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	p, cred, token, err := s.BootstrapOperator(ctx, "nick")
	if err != nil {
		t.Fatalf("BootstrapOperator: %v", err)
	}
	if p.Kind != PrincipalHuman || p.Role != RoleOperator || !p.Owner || !p.IsOwner() || p.Name != "nick" {
		t.Errorf("principal = %+v", p)
	}
	if cred.Label != BootstrapCredentialLabel || cred.PrincipalID != p.ID {
		t.Errorf("credential = %+v", cred)
	}
	if !tokenShape.MatchString(token) {
		t.Errorf("token has the wrong shape")
	}

	got, err := s.ResolveCredential(ctx, token)
	if err != nil || got.ID != p.ID || got.Role != RoleOperator || !got.Owner || !got.IsOwner() {
		t.Errorf("ResolveCredential = %+v, %v; want the owner operator", got, err)
	}
	gotDetail, _, err := s.ResolveCredentialDetail(ctx, token)
	if err != nil || !gotDetail.Owner || !gotDetail.IsOwner() {
		t.Errorf("ResolveCredentialDetail = %+v, %v; want Owner=true", gotDetail, err)
	}
	gotReq, _, err := s.ResolveRequestCredential(ctx, token)
	if err != nil || !gotReq.Owner || !gotReq.IsOwner() {
		t.Errorf("ResolveRequestCredential = %+v, %v; want Owner=true", gotReq, err)
	}
	byID, err := s.PrincipalByID(ctx, p.ID)
	if err != nil || byID.Role != RoleOperator || !byID.Owner || !byID.IsOwner() {
		t.Errorf("PrincipalByID = %+v (%v)", byID, err)
	}
	cand, found, err := s.LoginCandidate(ctx, "nick")
	if err != nil || !found || !cand.Principal.Owner || !cand.Principal.IsOwner() {
		t.Errorf("LoginCandidate = %+v, %v, %v; want Owner=true", cand, found, err)
	}
	adm, err := s.PrincipalAdmin(ctx, p.ID, time.Now())
	if err != nil || !adm.Owner {
		t.Errorf("PrincipalAdmin = %+v, %v; want Owner=true", adm, err)
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
	if strings.Join(actions, ",") != "principal_created,operator_bootstrapped,credential_created" {
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

func boolPtr(b bool) *bool { return &b }

func TestSetRoleTransitionsAndRefusals(t *testing.T) {
	ctx := context.Background()

	type roleFixture struct {
		s      *Store
		owner1 Principal
		owner2 Principal
		op     Principal
		member Principal
		agent  Principal
		guest  Principal
		ended  Principal
	}
	setup := func(t *testing.T) roleFixture {
		t.Helper()
		s := openTestStore(t)
		owner1, _, _, err := s.BootstrapOperator(ctx, "owner1")
		if err != nil {
			t.Fatal(err)
		}
		member, err := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
		if err != nil {
			t.Fatal(err)
		}
		op, err := s.CreatePrincipal(ctx, PrincipalHuman, "bob")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRole(ctx, principalActor(owner1.ID), owner1.ID, op.ID, RoleOperator, nil); err != nil {
			t.Fatal(err)
		}
		op, _ = s.PrincipalByID(ctx, op.ID)

		owner2, err := s.CreatePrincipal(ctx, PrincipalHuman, "owner2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRole(ctx, principalActor(owner1.ID), owner1.ID, owner2.ID, RoleOperator, boolPtr(true)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CreateCredential(ctx, "system", owner2.ID, "o2", nil); err != nil {
			t.Fatal(err)
		}
		owner2, _ = s.PrincipalByID(ctx, owner2.ID)

		agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
		if err != nil {
			t.Fatal(err)
		}

		ch, err := s.CreateChannel(ctx, "general")
		if err != nil {
			t.Fatal(err)
		}
		g1, err := s.CreateGuest(ctx, "system", NewGuest{
			Label: "visitor", Origin: schema.GuestOriginAddress,
			Channels: []int64{ch.ID}, Idle: time.Hour, Absolute: 24 * time.Hour, VoiceAllowed: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		g2, err := s.CreateGuest(ctx, "system", NewGuest{
			Label: "leaver", Origin: schema.GuestOriginAddress,
			Channels: []int64{ch.ID}, Idle: time.Hour, Absolute: 24 * time.Hour, VoiceAllowed: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.EndGuestVisit(ctx, principalActor(owner1.ID), g2.Principal.ID, schema.GuestEndOperator); err != nil {
			t.Fatal(err)
		}
		return roleFixture{
			s:      s,
			owner1: owner1,
			owner2: owner2,
			op:     op,
			member: member,
			agent:  agent,
			guest:  g1.Principal,
			ended:  g2.Principal,
		}
	}

	tests := []struct {
		name        string
		prepare     func(t *testing.T, f *roleFixture) (actorID, targetID int64)
		role        Role
		owner       *bool
		wantErr     error
		wantChanged bool
		wantRole    schema.PrincipalRole
		wantOwner   bool
		wantDetail  string
	}{
		{
			name:        "member to plain operator (owner nil)",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.member.ID },
			role:        RoleOperator,
			owner:       nil,
			wantChanged: true,
			wantRole:    schema.RoleOperator,
			wantOwner:   false,
			wantDetail:  "from=member to=operator owner=false manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name:        "member to plain operator (owner false)",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.member.ID },
			role:        RoleOperator,
			owner:       boolPtr(false),
			wantChanged: true,
			wantRole:    schema.RoleOperator,
			wantOwner:   false,
			wantDetail:  "from=member to=operator owner=false manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name:        "member to owner",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.member.ID },
			role:        RoleOperator,
			owner:       boolPtr(true),
			wantChanged: true,
			wantRole:    schema.RoleOperator,
			wantOwner:   true,
			wantDetail:  "from=member to=operator owner=true manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name:        "plain operator to owner",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.op.ID },
			role:        RoleOperator,
			owner:       boolPtr(true),
			wantChanged: true,
			wantRole:    schema.RoleOperator,
			wantOwner:   true,
			wantDetail:  "from=operator to=operator owner=true manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name:        "owner to plain operator when another owner can sign in",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.owner2.ID },
			role:        RoleOperator,
			owner:       boolPtr(false),
			wantChanged: true,
			wantRole:    schema.RoleOperator,
			wantOwner:   false,
			wantDetail:  "from=operator to=operator owner=false manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name:        "owner self-unmark when another owner can sign in",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.owner1.ID },
			role:        RoleOperator,
			owner:       boolPtr(false),
			wantChanged: true,
			wantRole:    schema.RoleOperator,
			wantOwner:   false,
			wantDetail:  "from=operator to=operator owner=false manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name:        "owner self-demote to member when another owner can sign in",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.owner1.ID },
			role:        RoleMember,
			owner:       nil,
			wantChanged: true,
			wantRole:    schema.RoleMember,
			wantOwner:   false,
			wantDetail:  "from=operator to=member owner=false manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name:        "plain operator to member",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.op.ID },
			role:        RoleMember,
			owner:       nil,
			wantChanged: true,
			wantRole:    schema.RoleMember,
			wantOwner:   false,
			wantDetail:  "from=operator to=member owner=false manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name: "disabled operator demoted to member stays disabled",
			prepare: func(t *testing.T, f *roleFixture) (int64, int64) {
				if _, err := f.s.DisablePrincipal(ctx, "system", f.op.ID); err != nil {
					t.Fatal(err)
				}
				return f.owner1.ID, f.op.ID
			},
			role:        RoleMember,
			owner:       nil,
			wantChanged: true,
			wantRole:    schema.RoleMember,
			wantOwner:   false,
			wantDetail:  "from=operator to=member owner=false manages=none invites_closed=0 credentials_revoked=0 hooks_revoked=0",
		},
		{
			name:        "no-op member to member",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.member.ID },
			role:        RoleMember,
			owner:       nil,
			wantChanged: false,
			wantRole:    schema.RoleMember,
			wantOwner:   false,
		},
		{
			name:        "no-op plain operator with owner nil keeps false",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.op.ID },
			role:        RoleOperator,
			owner:       nil,
			wantChanged: false,
			wantRole:    schema.RoleOperator,
			wantOwner:   false,
		},
		{
			name:        "no-op owner with owner nil keeps true",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.owner2.ID },
			role:        RoleOperator,
			owner:       nil,
			wantChanged: false,
			wantRole:    schema.RoleOperator,
			wantOwner:   true,
		},
		{
			name:        "no-op owner with owner true",
			prepare:     func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.owner2.ID },
			role:        RoleOperator,
			owner:       boolPtr(true),
			wantChanged: false,
			wantRole:    schema.RoleOperator,
			wantOwner:   true,
		},
		{
			name:    "actor is plain operator (not owner)",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.op.ID, f.member.ID },
			role:    RoleOperator,
			wantErr: ErrNotOwner,
		},
		{
			name:    "actor is member",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.member.ID, f.op.ID },
			role:    RoleMember,
			wantErr: ErrNotOwner,
		},
		{
			name:    "actor is agent",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.agent.ID, f.member.ID },
			role:    RoleOperator,
			wantErr: ErrNotOwner,
		},
		{
			name:    "actor is unknown",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return 99999, f.member.ID },
			role:    RoleOperator,
			wantErr: ErrNotOwner,
		},
		{
			name: "actor is disabled owner",
			prepare: func(t *testing.T, f *roleFixture) (int64, int64) {
				if _, err := f.s.DisablePrincipal(ctx, "system", f.owner2.ID); err != nil {
					t.Fatal(err)
				}
				return f.owner2.ID, f.member.ID
			},
			role:    RoleOperator,
			wantErr: ErrNotOwner,
		},
		{
			name:    "target is unknown",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, 99999 },
			role:    RoleOperator,
			wantErr: ErrPrincipalNotFound,
		},
		{
			name: "target is hidden",
			prepare: func(t *testing.T, f *roleFixture) (int64, int64) {
				if _, err := f.s.HidePrincipal(ctx, "system", f.member.ID, HideOptions{}); err != nil {
					t.Fatal(err)
				}
				return f.owner1.ID, f.member.ID
			},
			role:    RoleOperator,
			wantErr: ErrPrincipalNotFound,
		},
		{
			name:    "target is agent",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.agent.ID },
			role:    RoleOperator,
			wantErr: ErrRoleNotAllowed,
		},
		{
			name:    "target is active guest",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.guest.ID },
			role:    RoleOperator,
			wantErr: ErrRoleNotAllowed,
		},
		{
			name:    "target is ended guest",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.ended.ID },
			role:    RoleOperator,
			wantErr: ErrRoleNotAllowed,
		},
		{
			name:    "member with owner true",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.member.ID },
			role:    RoleMember,
			owner:   boolPtr(true),
			wantErr: ErrRoleNotAllowed,
		},
		{
			name:    "unknown role",
			prepare: func(_ *testing.T, f *roleFixture) (int64, int64) { return f.owner1.ID, f.member.ID },
			role:    Role("root"),
			wantErr: ErrRoleNotAllowed,
		},
		{
			name: "sole owner able to sign in cannot demote self",
			prepare: func(t *testing.T, f *roleFixture) (int64, int64) {
				if _, err := f.s.DisablePrincipal(ctx, "system", f.owner2.ID); err != nil {
					t.Fatal(err)
				}
				return f.owner1.ID, f.owner1.ID
			},
			role:    RoleMember,
			wantErr: ErrLastOperator,
		},
		{
			name: "sole owner able to sign in cannot clear own mark",
			prepare: func(t *testing.T, f *roleFixture) (int64, int64) {
				if _, err := f.s.DisablePrincipal(ctx, "system", f.owner2.ID); err != nil {
					t.Fatal(err)
				}
				return f.owner1.ID, f.owner1.ID
			},
			role:    RoleOperator,
			owner:   boolPtr(false),
			wantErr: ErrLastOperator,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			actorID, targetID := tt.prepare(t, &f)
			beforeAudit := countRows(t, f.s, "audit_events")

			res, err := f.s.SetRole(ctx, principalActor(actorID), actorID, targetID, tt.role, tt.owner)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("SetRole err = %v, want %v", err, tt.wantErr)
				}
				if afterAudit := countRows(t, f.s, "audit_events"); afterAudit != beforeAudit {
					t.Errorf("refused SetRole wrote %d audit events", afterAudit-beforeAudit)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetRole unexpected error: %v", err)
			}
			if res.Changed != tt.wantChanged {
				t.Errorf("Changed = %v, want %v", res.Changed, tt.wantChanged)
			}
			if res.Principal.Role != tt.wantRole || res.Principal.Owner != tt.wantOwner {
				t.Errorf("Principal = %+v, want role=%s owner=%v", res.Principal, tt.wantRole, tt.wantOwner)
			}
			if res.Manages == nil || len(res.Manages) != 0 {
				t.Errorf("Manages = %#v, want empty non-nil slice", res.Manages)
			}
			if !tt.wantChanged {
				if afterAudit := countRows(t, f.s, "audit_events"); afterAudit != beforeAudit {
					t.Errorf("no-op SetRole wrote %d audit events", afterAudit-beforeAudit)
				}
				return
			}
			ev, err := f.s.LastAuditEvent(ctx, AuditPrincipalRoleChanged)
			if err != nil {
				t.Fatalf("LastAuditEvent: %v", err)
			}
			if ev.Actor != principalActor(actorID) || ev.Subject != principalActor(targetID) || ev.Detail != tt.wantDetail {
				t.Errorf("audit event = %+v, want actor=%s subject=%s detail=%q",
					ev, principalActor(actorID), principalActor(targetID), tt.wantDetail)
			}
		})
	}
}

func TestSetRoleOwnerMarkRevokesEverything(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	root, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	if err != nil {
		t.Fatal(err)
	}

	// Issue 2 live credentials and 1 already-revoked credential for alice.
	_, tok1, err := s.CreateCredential(ctx, "system", alice.ID, "c1", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, tok2, err := s.CreateCredential(ctx, "system", alice.ID, "c2", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadCred, _, err := s.CreateCredential(ctx, "system", alice.ID, "dead", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "system", deadCred.ID); err != nil {
		t.Fatal(err)
	}

	// Bind 2 live hooks and 1 already-revoked hook to alice.
	if _, err := s.CreateHookWithLabel(ctx, "system", "alice-hook-1", "h1", ch.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHookWithLabel(ctx, "system", "alice-hook-2", "h2", ch.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	deadHook, err := s.CreateHookWithLabel(ctx, "system", "alice-hook-dead", "hdead", ch.ID, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeHook(ctx, "system", deadHook.ID); err != nil {
		t.Fatal(err)
	}

	// Issue open sign_in, reset, enrollment, and device links for alice.
	now := time.Now().Truncate(time.Millisecond)
	signInLink, err := s.CreateSignInLink(ctx, principalActor(root.ID), root.ID, alice.ID, schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeSignIn}, now)
	if err != nil {
		t.Fatal(err)
	}
	resetLink, err := s.CreateSignInLink(ctx, principalActor(root.ID), root.ID, alice.ID, schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeReset}, now)
	if err != nil {
		t.Fatal(err)
	}
	enrollLink, err := s.CreateSignInLink(ctx, principalActor(root.ID), root.ID, alice.ID, schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeEnrollment}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Seed an open device link and an unspent pending login for alice.
	if _, err := s.db.Exec(
		`INSERT INTO sign_in_links (code_hash, principal_id, created_by, created_at, expires_at, purpose, label, auth_generation)
		 VALUES ('devhash', ?, ?, ?, ?, 'device', 'phone', 0)`,
		alice.ID, alice.ID, now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO pending_logins (challenge_hash, principal_id, address_key, auth_generation, created_at, expires_at)
		 VALUES ('chalhash', ?, 'addr', 0, ?, ?)`,
		alice.ID, now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Promoting alice to plain operator first revokes nothing and reports counts.
	plainRes, err := s.SetRole(ctx, principalActor(root.ID), root.ID, alice.ID, RoleOperator, nil)
	if err != nil {
		t.Fatalf("SetRole(plain operator): %v", err)
	}
	if plainRes.CredentialsRevoked != 0 || plainRes.HooksRevoked != 0 || plainRes.LiveCredentials != 2 || plainRes.HooksBound != 2 {
		t.Fatalf("plain operator result = %+v; want 0 revoked, 2 live credentials, 2 bound hooks", plainRes)
	}

	// Setting the owner mark on alice revokes her 2 live credentials, 2 live hooks,
	// closes all 4 open links, and spends her pending login.
	ownerRes, err := s.SetRole(ctx, principalActor(root.ID), root.ID, alice.ID, RoleOperator, boolPtr(true))
	if err != nil {
		t.Fatalf("SetRole(owner): %v", err)
	}
	if !ownerRes.Changed || !ownerRes.Principal.Owner ||
		ownerRes.CredentialsRevoked != 2 || ownerRes.HooksRevoked != 2 ||
		ownerRes.LiveCredentials != 0 || ownerRes.HooksBound != 0 {
		t.Fatalf("owner result = %+v", ownerRes)
	}

	for _, tok := range []string{tok1, tok2} {
		if _, err := s.ResolveCredential(ctx, tok); !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("credential still resolves after owner mark: %v", err)
		}
	}
	for _, htok := range []string{"alice-hook-1", "alice-hook-2"} {
		if _, err := s.HookByToken(ctx, htok); !errors.Is(err, ErrNotFound) {
			t.Errorf("hook %q still resolves after owner mark: %v", htok, err)
		}
	}
	for _, code := range []string{signInLink.Code, resetLink.Code, enrollLink.Code} {
		if _, err := s.PreviewSignInLink(ctx, HashSignInLinkCode(code), now); !errors.Is(err, ErrSignInLinkInvalid) {
			t.Errorf("link still previews after owner mark: %v", err)
		}
	}
	var openLinks, unspentLogins int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sign_in_links WHERE principal_id = ? AND closed_at IS NULL", alice.ID).Scan(&openLinks); err != nil || openLinks != 0 {
		t.Errorf("open sign-in links for alice = %d (%v), want 0", openLinks, err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM pending_logins WHERE principal_id = ? AND spent_at IS NULL", alice.ID).Scan(&unspentLogins); err != nil || unspentLogins != 0 {
		t.Errorf("unspent pending logins for alice = %d (%v), want 0", unspentLogins, err)
	}

	ev, err := s.LastAuditEvent(ctx, AuditPrincipalRoleChanged)
	if err != nil {
		t.Fatal(err)
	}
	wantDetail := "from=operator to=operator owner=true manages=none invites_closed=0 credentials_revoked=2 hooks_revoked=2"
	if ev.Detail != wantDetail {
		t.Errorf("principal_role_changed detail = %q, want %q", ev.Detail, wantDetail)
	}
}

func TestSetRoleReportsManagedChannels(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	root, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	ch1, err := s.CreateChannel(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	ch2, err := s.CreateChannel(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	ch3, err := s.CreateChannel(ctx, "gamma")
	if err != nil {
		t.Fatal(err)
	}
	deputy, err := s.CreatePrincipal(ctx, PrincipalHuman, "deputy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRole(ctx, principalActor(root.ID), root.ID, deputy.ID, RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	// Grant manages on ch3 and ch1 (out of order) and plain membership on ch2.
	if _, err := s.PutChannelMember(ctx, principalActor(root.ID), ch3.ID, deputy.ID, root.ID, nil, boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutChannelMember(ctx, principalActor(root.ID), ch2.ID, deputy.ID, root.ID, nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutChannelMember(ctx, principalActor(root.ID), ch1.ID, deputy.ID, root.ID, nil, boolPtr(true)); err != nil {
		t.Fatal(err)
	}

	res, err := s.SetRole(ctx, principalActor(root.ID), root.ID, deputy.ID, RoleMember, nil)
	if err != nil {
		t.Fatalf("SetRole(demote deputy): %v", err)
	}
	wantManages := []int64{ch1.ID, ch3.ID}
	if !slices.Equal(res.Manages, wantManages) {
		t.Errorf("res.Manages = %v, want %v", res.Manages, wantManages)
	}
	ev, err := s.LastAuditEvent(ctx, AuditPrincipalRoleChanged)
	if err != nil {
		t.Fatal(err)
	}
	wantDetail := fmt.Sprintf("from=operator to=member owner=false manages=%d,%d invites_closed=0 credentials_revoked=0 hooks_revoked=0", ch1.ID, ch3.ID)
	if ev.Detail != wantDetail {
		t.Errorf("principal_role_changed detail = %q, want %q", ev.Detail, wantDetail)
	}
	// Manages grant remains live after demotion.
	if manages, err := s.ManagesChannel(ctx, ch1.ID, deputy.ID); err != nil || !manages {
		t.Errorf("ManagesChannel(ch1) after demotion = %v, %v; want true", manages, err)
	}
}

func TestSetRoleDemotionClosesInvitesAndSignInLinks(t *testing.T) {
	ctx := context.Background()
	for _, mayInvite := range []bool{false, true} {
		t.Run(fmt.Sprintf("may_invite=%t", mayInvite), func(t *testing.T) {
			s := openTestStore(t)
			root, _, _, err := s.BootstrapOperator(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			ch, err := s.CreateChannel(ctx, "general")
			if err != nil {
				t.Fatal(err)
			}
			deputy, err := s.CreatePrincipal(ctx, PrincipalHuman, "deputy")
			if err != nil {
				t.Fatal(err)
			}
			member, err := s.CreatePrincipal(ctx, PrincipalHuman, "member")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddChannelMember(ctx, "system", ch.ID, deputy.ID, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetMayInvite(ctx, principalActor(root.ID), deputy.ID, mayInvite); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetRole(ctx, principalActor(root.ID), root.ID, deputy.ID, RoleOperator, nil); err != nil {
				t.Fatal(err)
			}
			_, deputyTok, err := s.CreateCredential(ctx, "system", deputy.ID, "deputy", nil)
			if err != nil {
				t.Fatal(err)
			}

			now := time.Now().Truncate(time.Millisecond)
			openInv, err := s.CreateInvite(ctx, principalActor(deputy.ID), deputy.ID, schema.CreateInviteRequestV1{
				Kind:     schema.InviteKindOpen,
				Channels: []string{ch.Name},
			}, InviteOptions{}, now)
			if err != nil {
				t.Fatal(err)
			}
			persInv, err := s.CreateInvite(ctx, principalActor(deputy.ID), deputy.ID, schema.CreateInviteRequestV1{
				Kind:     schema.InviteKindPersonal,
				Channels: []string{ch.Name},
			}, InviteOptions{}, now)
			if err != nil {
				t.Fatal(err)
			}
			link, err := s.CreateSignInLink(ctx, principalActor(deputy.ID), deputy.ID, member.ID, schema.CreateSignInLinkRequestV1{}, now)
			if err != nil {
				t.Fatal(err)
			}

			res, err := s.SetRole(ctx, principalActor(root.ID), root.ID, deputy.ID, RoleMember, nil)
			if err != nil {
				t.Fatalf("SetRole(demote deputy): %v", err)
			}
			wantClosed := 2
			if mayInvite {
				wantClosed = 1
			}
			if res.InvitesClosed != wantClosed {
				t.Errorf("InvitesClosed = %d, want %d", res.InvitesClosed, wantClosed)
			}
			// Deputy's credential stays live on demotion (D5).
			if _, err := s.ResolveCredential(ctx, deputyTok); err != nil {
				t.Errorf("demoted operator's credential was revoked: %v", err)
			}
			// Open invite link and sign-in link are closed.
			if _, err := s.PreviewInvite(ctx, HashInviteCode(openInv.Code), now); !errors.Is(err, ErrInviteInvalid) {
				t.Errorf("open invite link still usable after demotion: %v", err)
			}
			if _, err := s.PreviewSignInLink(ctx, HashSignInLinkCode(link.Code), now); !errors.Is(err, ErrSignInLinkInvalid) {
				t.Errorf("sign-in link still usable after demotion: %v", err)
			}
			// Personal invite stays open iff may_invite is true on deputy.
			_, err = s.PreviewInvite(ctx, HashInviteCode(persInv.Code), now)
			if mayInvite && err != nil {
				t.Errorf("personal invite with may_invite=true was closed: %v", err)
			}
			if !mayInvite && !errors.Is(err, ErrInviteInvalid) {
				t.Errorf("personal invite with may_invite=false still usable: %v", err)
			}
		})
	}
}

func TestSetRoleLastOwnerConcurrent(t *testing.T) {
	ctx := context.Background()
	for range 10 {
		s := openTestStore(t)
		o1, _, _, err := s.BootstrapOperator(ctx, "o1")
		if err != nil {
			t.Fatal(err)
		}
		o2, err := s.CreatePrincipal(ctx, PrincipalHuman, "o2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRole(ctx, principalActor(o1.ID), o1.ID, o2.ID, RoleOperator, boolPtr(true)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CreateCredential(ctx, "system", o2.ID, "o2", nil); err != nil {
			t.Fatal(err)
		}

		// Two owners concurrently demote themselves: IMMEDIATE transactions
		// serialize them so exactly one succeeds and one is refused with
		// ErrLastOperator.
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, id := range []int64{o1.ID, o2.ID} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.SetRole(ctx, principalActor(id), id, id, RoleMember, nil)
			}()
		}
		wg.Wait()

		ok, refused := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrLastOperator):
				refused++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("concurrent self-demotions = %v; want 1 ok and 1 ErrLastOperator", errs)
		}
		var owners int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM principals WHERE role = 'operator' AND owner = 1 AND disabled_at IS NULL").Scan(&owners); err != nil || owners != 1 {
			t.Fatalf("remaining enabled owners = %d (%v), want 1", owners, err)
		}
	}
}

// TestOwnerColumnWriteSites enforces §8.1: in internal/server/store/, only the
// migration (migrations in store.go) and setRoleTx (in roles.go) update
// principals.owner, and the only INSERT writing principals.owner is
// BootstrapOperatorWith (in roles.go).
func TestOwnerColumnWriteSites(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var updateSites, allWriteSites []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			scope := ""
			switch d := decl.(type) {
			case *ast.FuncDecl:
				scope = d.Name.Name
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
						scope = vs.Names[0].Name
						break
					}
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				lower := strings.ToLower(val)
				if !strings.Contains(lower, "principals") || !strings.Contains(lower, "owner") {
					return true
				}
				isUpdate := strings.Contains(lower, "update principals")
				isWrite := isUpdate || strings.Contains(lower, "insert into principals") || strings.Contains(lower, "alter table principals")
				if !isWrite {
					return true
				}
				site := name + ":" + scope
				if !slices.Contains(allWriteSites, site) {
					allWriteSites = append(allWriteSites, site)
				}
				if isUpdate && !slices.Contains(updateSites, site) {
					updateSites = append(updateSites, site)
				}
				return true
			})
		}
	}

	slices.Sort(updateSites)
	slices.Sort(allWriteSites)
	wantUpdates := []string{"roles.go:setRoleTx", "store.go:migrations"}
	wantWrites := []string{"roles.go:BootstrapOperatorWith", "roles.go:setRoleTx", "store.go:migrations"}
	if !slices.Equal(updateSites, wantUpdates) {
		t.Errorf("SQL UPDATE sites writing principals.owner = %v, want %v", updateSites, wantUpdates)
	}
	if !slices.Equal(allWriteSites, wantWrites) {
		t.Errorf("all SQL write sites touching principals.owner = %v, want %v", allWriteSites, wantWrites)
	}
}
