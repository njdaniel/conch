package store

import (
	"context"
	"fmt"
	"time"
)

// AuditExported is the action of the row GET /v1/audit writes for each export.
// The row names the caller and the range asked for; it is written before the
// first line is sent and so lies beyond the export's own upper bound.
const AuditExported = "audit_exported"

// ListAuditEventsUpTo is ListAuditEvents with an inclusive upper bound: up to
// limit events with afterID < ID <= upToID, ascending. The export fixes
// upToID once, so rows written while it streams are not in it.
func (s *Store) ListAuditEventsUpTo(ctx context.Context, afterID, upToID int64, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list audit events: limit must be positive, got %d", limit)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, actor, action, subject, detail, created_at
		 FROM audit_events WHERE id > ? AND id <= ?
		 ORDER BY id ASC LIMIT ?`,
		afterID, upToID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Subject, &e.Detail, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list audit events: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit events: %w", err)
	}
	return events, nil
}
