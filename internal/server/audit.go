package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// auditExportPage is how many rows one query reads and one flush sends. The
// export holds at most one page at a time, so its memory does not grow with
// the trail. A variable, not a constant, only so tests can make pages small.
var auditExportPage = 500

// auditExportAfterPage, when set, runs after each page has been sent and
// flushed. It is a test seam for writing events while an export is mid-stream.
var auditExportAfterPage func()

// handleExportAudit serves GET /v1/audit: the audit trail as JSON Lines, in
// ascending id order, up to the last id at the moment the request started.
// The route is operator-only (the trail names every principal and refusal).
func (s *Server) handleExportAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	after, limit, msg := parseAuditExportQuery(r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	// Fixed once, before anything is written: this is what makes the stream
	// end while the server is busy, and keeps the export's own audit row
	// (written next, so numbered above the bound) out of it.
	upTo, err := s.store.LastAuditID(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "audit: read last id failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	limitText := "none"
	if limit > 0 {
		limitText = strconv.FormatInt(limit, 10)
	}
	// Recorded before the first byte, so an export that is cut short is still
	// on the trail; a failure to record it refuses the export.
	if _, err := s.store.AppendAuditEvent(ctx, auditActor(ctx), store.AuditExported, "",
		fmt.Sprintf("after=%d limit=%s upto=%d", after, limitText, upTo)); err != nil {
		slog.ErrorContext(ctx, "audit: record export failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	var page bytes.Buffer
	remaining := limit // 0 means no limit
	cursor := after
	for cursor < upTo && (limit == 0 || remaining > 0) {
		// A client that went away ends the export between pages; a write to
		// it fails sooner than that.
		if ctx.Err() != nil {
			return
		}
		n := auditExportPage
		if limit > 0 && remaining < int64(n) {
			n = int(remaining)
		}
		events, err := s.store.ListAuditEventsUpTo(ctx, cursor, upTo, n)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// The status is already sent. Ending the body cleanly would pass a
			// truncated trail off as a whole one, so abort the connection.
			slog.ErrorContext(ctx, "audit: read page failed", "error", err)
			panic(http.ErrAbortHandler)
		}
		if len(events) == 0 {
			return
		}
		page.Reset()
		enc := json.NewEncoder(&page) // one object and one "\n" per Encode
		for _, e := range events {
			line := schema.AuditEventV1{
				Schema:    schema.AuditEventSchemaV1,
				ID:        e.ID,
				Actor:     e.Actor,
				Action:    e.Action,
				Subject:   e.Subject,
				Detail:    e.Detail,
				CreatedAt: schema.NewTimestamp(e.CreatedAt),
			}
			if err := enc.Encode(line); err != nil {
				slog.ErrorContext(ctx, "audit: encode event failed", "event", e.ID, "error", err)
				panic(http.ErrAbortHandler)
			}
		}
		if _, err := w.Write(page.Bytes()); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		cursor = events[len(events)-1].ID
		if limit > 0 {
			remaining -= int64(len(events))
		}
		if hook := auditExportAfterPage; hook != nil {
			hook()
		}
	}
}

// parseAuditExportQuery reads after and limit. Each is optional; present, each
// must be a plain non-negative decimal integer, and limit must not be zero. A
// non-empty message is the 400 text.
func parseAuditExportQuery(r *http.Request) (after, limit int64, msg string) {
	q := r.URL.Query()
	parse := func(name string) (int64, bool, string) {
		vals, present := q[name]
		if !present {
			return 0, false, ""
		}
		if len(vals) != 1 || !isDecimal(vals[0]) {
			return 0, true, name + " must be a non-negative integer"
		}
		n, err := strconv.ParseInt(vals[0], 10, 64)
		if err != nil {
			return 0, true, name + " must be a non-negative integer"
		}
		return n, true, ""
	}
	after, _, msg = parse("after")
	if msg != "" {
		return 0, 0, msg
	}
	limit, given, msg := parse("limit")
	if msg != "" {
		return 0, 0, msg
	}
	if given && limit == 0 {
		return 0, 0, "limit must be positive"
	}
	return after, limit, ""
}

// isDecimal reports whether s is one or more ASCII digits: no sign, no space.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
