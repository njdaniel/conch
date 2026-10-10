package store

import (
	"context"
	"errors"
	"testing"
	"time"
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

// AppendAuditEventAt writes a row with the time it is given (issue #135): the
// row keeps its place in the order of writing, and its time is the caller's,
// to the millisecond the log stores, whether that is before or after the rows
// around it. AppendAuditEvent is the same write with the time of the call.
func TestAppendAuditEventAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"a time", base.Add(1500 * time.Millisecond), base.Add(1500 * time.Millisecond)},
		{"an earlier time, written later", base.Add(1000 * time.Millisecond), base.Add(1000 * time.Millisecond)},
		{"the same time again", base.Add(1000 * time.Millisecond), base.Add(1000 * time.Millisecond)},
		{"a time finer than the log keeps", base.Add(2000*time.Millisecond + 999*time.Microsecond), base.Add(2000 * time.Millisecond)},
	}
	var ids []int64
	for _, tt := range tests {
		e, err := s.AppendAuditEventAt(ctx, "principal:7", "voice_transmit_unreported", "channel:3", tt.name, tt.at)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if !e.CreatedAt.Equal(tt.at) {
			t.Errorf("%s: returned time %v, want the time given, %v", tt.name, e.CreatedAt, tt.at)
		}
		ids = append(ids, e.ID)
	}
	before := time.Now().Truncate(time.Millisecond)
	plain, err := s.AppendAuditEvent(ctx, "system", "something", "none", "")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	events, err := s.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(tests)+1 {
		t.Fatalf("%d rows, want %d", len(events), len(tests)+1)
	}
	for i, tt := range tests {
		e := events[i]
		if e.ID != ids[i] || e.Detail != tt.name || !e.CreatedAt.Equal(tt.want) || e.Actor != "principal:7" || e.Subject != "channel:3" {
			t.Errorf("row %d = %+v, want %q at %v, in the order written", i, e, tt.name, tt.want)
		}
		if i > 0 && e.ID <= events[i-1].ID {
			t.Errorf("row %d has id %d after id %d: ids are the order of writing", i, e.ID, events[i-1].ID)
		}
	}
	if got := events[len(tests)]; got.ID != plain.ID || got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Errorf("AppendAuditEvent row = %+v, want it timed at the call (between %v and %v)", got, before, after)
	}
}

func TestLastAuditID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if id, err := s.LastAuditID(ctx); err != nil || id != 0 {
		t.Fatalf("empty log: %d, %v; want 0", id, err)
	}
	var last AuditEvent
	for i := 0; i < 3; i++ {
		e, err := s.AppendAuditEvent(ctx, "system", "something", "none", "")
		if err != nil {
			t.Fatal(err)
		}
		last = e
	}
	if id, err := s.LastAuditID(ctx); err != nil || id != last.ID {
		t.Errorf("LastAuditID = %d, %v; want %d", id, err, last.ID)
	}
}
