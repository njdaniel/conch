package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// If the trail cannot be read part-way through an export, the status line has
// already gone out as 200. The response must then end as a broken one, not as
// a complete one: a reader who got a clean end would take a truncated trail
// for the whole of it.
func TestExportAuditThatFailsMidStreamIsNotACleanEnd(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	seedAudit(t, f, 10)
	web := httptest.NewServer(f.srv.Handler())
	defer web.Close()

	// After the first page of two rows is on the wire, the database goes away.
	var closed atomic.Bool
	setAuditPage(t, 2, func() {
		if closed.CompareAndSwap(false, true) {
			if err := f.srv.store.Close(); err != nil {
				t.Error(err)
			}
		}
	})

	req, err := http.NewRequest(http.MethodGet, web.URL+"/v1/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.rootTok)
	resp, err := web.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the failure comes after the first page)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if !closed.Load() {
		t.Fatal("the database was never closed; the test did not reach the failure")
	}
	if err == nil {
		t.Fatalf("the body ended cleanly after %d bytes; a truncated export must end as an error", len(body))
	}
	// What did arrive is whole lines of the first page, nothing half-written.
	if got := len(parseJSONL(t, string(body))); got != 2 {
		t.Errorf("events received before the failure = %d, want the first page of 2", got)
	}
}
