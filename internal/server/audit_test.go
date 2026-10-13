package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// setAuditPage makes export pages small for one test, so paging, the fixed
// upper bound and cancellation are exercised on a handful of rows.
func setAuditPage(t *testing.T, n int, afterPage func()) {
	t.Helper()
	oldPage, oldHook := auditExportPage, auditExportAfterPage
	auditExportPage, auditExportAfterPage = n, afterPage
	t.Cleanup(func() { auditExportPage, auditExportAfterPage = oldPage, oldHook })
}

// parseJSONL decodes an export body line by line, rejecting blank lines and
// any line that is not a valid conch.audit_event.v1 object.
func parseJSONL(t *testing.T, body string) []schema.AuditEventV1 {
	t.Helper()
	if body == "" {
		return nil
	}
	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("body does not end with a newline: %q", body)
	}
	var out []schema.AuditEventV1
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		var e schema.AuditEventV1
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not JSON: %v: %q", i+1, err, line)
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		out = append(out, e)
	}
	return out
}

func eventIDs(events []schema.AuditEventV1) []int64 {
	ids := make([]int64, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	return ids
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// exportedRows returns the audit_exported rows on the trail.
func exportedRows(t *testing.T, f *authFixture) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == store.AuditExported {
			out = append(out, e)
		}
	}
	return out
}

// seedAudit appends n rows with a recognizable action.
func seedAudit(t *testing.T, f *authFixture, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := f.srv.store.AppendAuditEvent(context.Background(), "system", "seed", fmt.Sprintf("seed:%d", i), "d"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportAuditRange(t *testing.T) {
	setAuditPage(t, 3, nil)
	f := newAuthFixture(t, AuthRequired)
	seedAudit(t, f, 10) // well over one page of 3
	// Each export adds its own audit_exported row, so every case reads the
	// trail as it stands when it starts and states its range against that.
	tests := []struct {
		name  string
		query func(all []int64) string
		want  func(all []int64) []int64
	}{
		{"everything, across several pages",
			func([]int64) string { return "" },
			func(all []int64) []int64 { return all }},
		{"after",
			func(all []int64) string { return fmt.Sprintf("?after=%d", all[4]) },
			func(all []int64) []int64 { return all[5:] }},
		{"after zero is the default",
			func([]int64) string { return "?after=0" },
			func(all []int64) []int64 { return all }},
		{"limit inside one page",
			func([]int64) string { return "?limit=2" },
			func(all []int64) []int64 { return all[:2] }},
		{"limit on a page boundary",
			func([]int64) string { return "?limit=6" },
			func(all []int64) []int64 { return all[:6] }},
		{"limit across pages",
			func([]int64) string { return "?limit=7" },
			func(all []int64) []int64 { return all[:7] }},
		{"limit beyond the trail",
			func([]int64) string { return "?limit=100000" },
			func(all []int64) []int64 { return all }},
		{"after and limit",
			func(all []int64) string { return fmt.Sprintf("?after=%d&limit=4", all[2]) },
			func(all []int64) []int64 { return all[3:7] }},
		{"after far beyond the last id is empty",
			func(all []int64) string { return fmt.Sprintf("?after=%d", all[len(all)-1]+50) },
			func([]int64) []int64 { return nil }},
		{"after exactly the last id is empty",
			func(all []int64) string { return fmt.Sprintf("?after=%d", all[len(all)-1]) },
			func([]int64) []int64 { return nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var all []int64
			for _, e := range f.audit(t) {
				all = append(all, e.ID)
			}
			rec := f.do(t, "GET", "/v1/audit"+tt.query(all), f.rootTok, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/x-ndjson" {
				t.Errorf("Content-Type = %q", got)
			}
			got := parseJSONL(t, rec.Body.String())
			if want := tt.want(all); !equalIDs(eventIDs(got), want) {
				t.Errorf("ids = %v, want %v", eventIDs(got), want)
			}
		})
	}
}

// A line is the stored row, as the issue's wire shape states it.
func TestExportAuditLineContent(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ev, err := f.srv.store.AppendAuditEventAt(context.Background(), "principal:3", "approval_resolved", "", "line one",
		time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "GET", fmt.Sprintf("/v1/audit?after=%d&limit=1", ev.ID-1), f.rootTok, "")
	want := fmt.Sprintf(`{"schema":"conch.audit_event.v1","id":%d,"actor":"principal:3","action":"approval_resolved","subject":"","detail":"line one","created_at":"2026-10-12T18:00:00.000Z"}`+"\n", ev.ID)
	if rec.Body.String() != want {
		t.Errorf("body = %q\nwant   %q", rec.Body.String(), want)
	}
}

func TestExportAuditBadParameters(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	seedAudit(t, f, 3)
	tests := []string{
		"?after=-1", "?after=abc", "?after=", "?after=1.5", "?after=%2B1", "?after=%201",
		"?after=99999999999999999999", "?after=1&after=2",
		"?limit=0", "?limit=-1", "?limit=abc", "?limit=", "?limit=1.5", "?limit=%2B1",
		"?limit=99999999999999999999", "?limit=1&limit=2",
		"?after=1&limit=0", "?after=x&limit=1",
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			before := len(f.audit(t))
			rec := f.do(t, "GET", "/v1/audit"+q, f.rootTok, "")
			assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("Content-Type = %q, want the standard JSON error", got)
			}
			if after := len(f.audit(t)); after != before {
				t.Errorf("a refused export wrote %d audit rows", after-before)
			}
		})
	}
}

func TestExportAuditCallers(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	seedAudit(t, f, 2)
	tests := []struct {
		name         string
		token        string
		wantStatus   int
		wantExported int
	}{
		{"unauthenticated", "", http.StatusUnauthorized, 0},
		{"garbage credential", "not-a-credential", http.StatusUnauthorized, 0},
		{"member human", f.aliceTok, http.StatusForbidden, 0},
		{"member agent", f.botTok, http.StatusForbidden, 0},
		{"operator", f.rootTok, http.StatusOK, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(exportedRows(t, f))
			rec := f.do(t, "GET", "/v1/audit", tt.token, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantStatus != http.StatusOK {
				if strings.Contains(rec.Body.String(), "conch.audit_event") {
					t.Errorf("a refused caller was sent audit lines: %s", rec.Body)
				}
				if got := rec.Header().Get("Content-Type"); strings.Contains(got, "ndjson") {
					t.Errorf("refusal Content-Type = %q", got)
				}
			}
			if got := len(exportedRows(t, f)) - before; got != tt.wantExported {
				t.Errorf("audit_exported rows written = %d, want %d", got, tt.wantExported)
			}
		})
	}
}

func TestExportAuditIsAudited(t *testing.T) {
	setAuditPage(t, 2, nil)
	f := newAuthFixture(t, AuthRequired)
	seedAudit(t, f, 4)
	last, err := f.srv.store.LastAuditID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		query      string
		wantDetail string
	}{
		{"", fmt.Sprintf("after=0 limit=none upto=%d", last)},
		{"?after=2&limit=3", fmt.Sprintf("after=2 limit=3 upto=%d", last+1)}, // the first export's own row
	}
	for i, tt := range tests {
		rec := f.do(t, "GET", "/v1/audit"+tt.query, f.rootTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("export %d: status %d", i, rec.Code)
		}
		rows := exportedRows(t, f)
		if len(rows) != i+1 {
			t.Fatalf("after export %d: audit_exported rows = %d, want %d", i, len(rows), i+1)
		}
		row := rows[i]
		if want := fmt.Sprintf("principal:%d", f.root.ID); row.Actor != want {
			t.Errorf("export %d: actor = %q, want %q", i, row.Actor, want)
		}
		if row.Detail != tt.wantDetail {
			t.Errorf("export %d: detail = %q, want %q", i, row.Detail, tt.wantDetail)
		}
		for _, e := range parseJSONL(t, rec.Body.String()) {
			if e.Action == store.AuditExported && e.ID >= row.ID {
				t.Errorf("export %d contains its own audit_exported row %d", i, e.ID)
			}
			if e.ID >= row.ID {
				t.Errorf("export %d contains row %d, written by or after the export (id %d)", i, e.ID, row.ID)
			}
		}
	}
}

// The upper bound is read once, at the start. A bound re-read for each page
// would take in the row written between pages and, with one written after
// every page, never end.
func TestExportAuditUpperBoundFixedAtStart(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	seedAudit(t, f, 8)
	web := httptest.NewServer(f.srv.Handler())
	defer web.Close()

	var pages atomic.Int32
	setAuditPage(t, 2, func() {
		if pages.Add(1) > 50 {
			return // a bound taken per page would loop; fail below, do not hang
		}
		_, err := f.srv.store.AppendAuditEvent(context.Background(), "system", "late", "", "written mid-export")
		if err != nil {
			t.Error(err)
			return
		}
	})
	startBound, err := f.srv.store.LastAuditID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	body := fetchExport(t, web.URL, f.rootTok, "")
	got := parseJSONL(t, body)
	if pages.Load() == 0 {
		t.Fatal("test bug: nothing was written between pages")
	}
	if pages.Load() > 50 {
		t.Fatal("the export did not end: its upper bound moves with the trail")
	}
	if len(got) != int(startBound) {
		t.Errorf("exported %d events, want exactly the %d that existed at the start", len(got), startBound)
	}
	for i, e := range got {
		if e.ID != int64(i+1) {
			t.Fatalf("line %d has id %d: not ascending from 1", i+1, e.ID)
		}
		if e.Action == "late" || e.ID > startBound {
			t.Errorf("event %d (%s) was written after the request started", e.ID, e.Action)
		}
	}

	// Resuming from the last id received returns exactly what followed.
	setAuditPage(t, 2, nil)
	last := got[len(got)-1].ID
	want, err := f.srv.store.ListAuditEvents(context.Background(), last, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var wantIDs []int64
	for _, e := range want {
		wantIDs = append(wantIDs, e.ID)
	}
	next := parseJSONL(t, fetchExport(t, web.URL, f.rootTok, fmt.Sprintf("?after=%d", last)))
	if !equalIDs(eventIDs(next), wantIDs) {
		t.Errorf("resumed ids = %v, want %v", eventIDs(next), wantIDs)
	}
	if len(next) == 0 || next[0].ID != last+1 {
		t.Errorf("resume does not start at id %d: %v", last+1, eventIDs(next))
	}
}

func fetchExport(t *testing.T, base, token, query string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/audit"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	return string(body)
}

// A client that disconnects mid-stream ends the handler: the server's Close
// waits for every handler, so a handler still reading pages would hang it.
func TestExportAuditClientGoesAway(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	const rows = 400
	seedAudit(t, f, rows)
	web := httptest.NewServer(f.srv.Handler())

	var pages atomic.Int32
	setAuditPage(t, 1, func() { pages.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, web.URL+"/v1/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.rootTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close()

	closed := make(chan struct{})
	go func() { web.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after the client went away")
	}
	if got := pages.Load(); got >= rows {
		t.Errorf("handler read %d pages: it kept going to the end after the client left", got)
	}
}
