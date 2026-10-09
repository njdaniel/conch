package store

import (
	"context"
	"errors"
	"testing"
)

// LastAuditEvent is how the approval manager finds its previous start in the
// log (approval-object.md §5.1): the most recent row with an action, whatever
// else was written since.
func TestLastAuditEvent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.LastAuditEvent(ctx, "approvals_started"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("with no such row: err = %v, want ErrNotFound", err)
	}
	var want AuditEvent
	for _, row := range []struct{ action, detail string }{
		{"approvals_started", "notifications=off"},
		{"something_else", "between"},
		{"approvals_started", "notifications=on"},
		{"something_else", "after"},
	} {
		e, err := s.AppendAuditEvent(ctx, "system", row.action, "approvals", row.detail)
		if err != nil {
			t.Fatal(err)
		}
		if row.action == "approvals_started" {
			want = e
		}
	}
	got, err := s.LastAuditEvent(ctx, "approvals_started")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Detail != "notifications=on" || got.Actor != "system" || got.Subject != "approvals" || got.CreatedAt.IsZero() {
		t.Errorf("LastAuditEvent = %+v, want row %d (notifications=on)", got, want.ID)
	}
	if _, err := s.LastAuditEvent(ctx, "never_written"); !errors.Is(err, ErrNotFound) {
		t.Errorf("for an action never written: err = %v, want ErrNotFound", err)
	}
}
