package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// voiceSwitchWorld is an owner, a non-owner operator, a member (the caller
// without manage_members) and a member target.
type voiceSwitchWorld struct {
	s                               *Store
	owner, operator, member, target Principal
	ownerActor                      string
}

func newVoiceSwitchWorld(t *testing.T) *voiceSwitchWorld {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)
	owner, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	w := &voiceSwitchWorld{s: s, owner: owner, ownerActor: fmt.Sprintf("principal:%d", owner.ID)}
	mk := func(name string) Principal {
		p, err := s.CreatePrincipal(ctx, PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	w.operator, w.member, w.target = mk("op"), mk("mem"), mk("target")
	if _, err := s.SetRole(ctx, w.ownerActor, owner.ID, w.operator.ID, RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *voiceSwitchWorld) voice(t *testing.T, id int64) bool {
	t.Helper()
	p, err := w.s.PrincipalByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p.VoiceAllowed
}

func (w *voiceSwitchWorld) changeRows(t *testing.T) int {
	t.Helper()
	n := 0
	for _, e := range allAuditEvents(t, w.s) {
		if e.Action == AuditVoicePermissionChanged {
			n++
		}
	}
	return n
}

// cutManageMembers turns manage_members off for operators, as the owner.
func (w *voiceSwitchWorld) cutManageMembers(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	doc, err := w.s.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermActAsMembers, Role: schema.PermissionRoleOperator, Granted: false},
		{Permission: schema.PermManageMembers, Role: schema.PermissionRoleOperator, Granted: false},
	}}
	if _, err := w.s.PutPermissions(ctx, w.ownerActor, w.owner.ID, req, time.Now()); err != nil {
		t.Fatalf("cut manage_members: %v", err)
	}
}

// TestSetVoiceAllowedRechecksTheCaller: the caller's manage_members is asked
// again inside the transaction, before the target is touched (admin-roles
// §7.1). A caller who passed the route guard and then lost it (stalled body,
// cell cut) brakes nobody: refused, voice unchanged, no change-audit row.
// Caller 0 is authentication off and is not asked.
func TestSetVoiceAllowedRechecksTheCaller(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		caller  func(w *voiceSwitchWorld) int64
		before  func(t *testing.T, w *voiceSwitchWorld)
		denied  bool
		wantNow bool // the target's voice afterwards
	}{
		{"a member", func(w *voiceSwitchWorld) int64 { return w.member.ID }, nil, true, true},
		{"an operator", func(w *voiceSwitchWorld) int64 { return w.operator.ID }, nil, false, false},
		{"an operator who lost manage_members after the guard", func(w *voiceSwitchWorld) int64 { return w.operator.ID },
			func(t *testing.T, w *voiceSwitchWorld) { w.cutManageMembers(t) }, true, true},
		{"the owner", func(w *voiceSwitchWorld) int64 { return w.owner.ID }, nil, false, false},
		{"no caller (authentication off)", func(*voiceSwitchWorld) int64 { return 0 }, nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newVoiceSwitchWorld(t)
			if tt.before != nil {
				tt.before(t, w)
			}
			changed, err := w.s.SetVoiceAllowed(ctx, "principal:x", tt.caller(w), w.target.ID, false, true)
			if tt.denied {
				var perr *PermissionError
				if !errors.Is(err, ErrPermissionDenied) || !errors.As(err, &perr) || perr.Permission != schema.PermManageMembers {
					t.Fatalf("err = %v, want ErrPermissionDenied naming manage_members", err)
				}
				if changed {
					t.Error("a refused write reports a change")
				}
			} else if err != nil || !changed {
				t.Fatalf("changed=%t err=%v, want a change", changed, err)
			}
			if got := w.voice(t, w.target.ID); got != tt.wantNow {
				t.Errorf("target voice_allowed = %t, want %t", got, tt.wantNow)
			}
			wantRows := 1
			if tt.denied {
				wantRows = 0
			}
			if got := w.changeRows(t); got != wantRows {
				t.Errorf("%d voice_permission_changed rows, want %d", got, wantRows)
			}
		})
	}
}

// TestSetVoiceAllowedOwnerProtected: with forbidOwner an owner target is
// refused inside the transaction (ADR-010), whatever the handler checked
// before: unchanged, no change-audit row. Without it the owner is switched.
func TestSetVoiceAllowedOwnerProtected(t *testing.T) {
	ctx := context.Background()
	w := newVoiceSwitchWorld(t)
	changed, err := w.s.SetVoiceAllowed(ctx, "principal:x", w.operator.ID, w.owner.ID, false, true)
	if !errors.Is(err, ErrOwnerProtected) || changed {
		t.Fatalf("changed=%t err=%v, want ErrOwnerProtected", changed, err)
	}
	if !w.voice(t, w.owner.ID) || w.changeRows(t) != 0 {
		t.Errorf("the refused write changed the owner (voice %t, %d rows)", w.voice(t, w.owner.ID), w.changeRows(t))
	}
	if changed, err := w.s.SetVoiceAllowed(ctx, w.ownerActor, w.owner.ID, w.owner.ID, false, false); err != nil || !changed {
		t.Fatalf("an owner on an owner: changed=%t err=%v", changed, err)
	}
	if w.voice(t, w.owner.ID) || w.changeRows(t) != 1 {
		t.Errorf("the owner's own write: voice %t, %d rows", w.voice(t, w.owner.ID), w.changeRows(t))
	}
}

// TestSetVoiceAllowedAuditFailureRollsBack: the change and its audit row are
// one transaction. A trigger that fails the voice_permission_changed insert
// leaves voice_allowed as it was.
func TestSetVoiceAllowedAuditFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	w := newVoiceSwitchWorld(t)
	if _, err := w.s.db.Exec(`CREATE TRIGGER fail_voice_audit BEFORE INSERT ON audit_events WHEN NEW.action = 'voice_permission_changed'
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	changed, err := w.s.SetVoiceAllowed(ctx, w.ownerActor, w.owner.ID, w.target.ID, false, false)
	if err == nil || changed {
		t.Fatalf("changed=%t err=%v, want the injected failure", changed, err)
	}
	if !w.voice(t, w.target.ID) {
		t.Error("voice_allowed changed though its audit row failed: the write did not roll back")
	}
	if _, err := w.s.db.Exec("DROP TRIGGER fail_voice_audit"); err != nil {
		t.Fatal(err)
	}
	if changed, err := w.s.SetVoiceAllowed(ctx, w.ownerActor, w.owner.ID, w.target.ID, false, false); err != nil || !changed {
		t.Fatalf("after the cause is gone: changed=%t err=%v", changed, err)
	}
}
