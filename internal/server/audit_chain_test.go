package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #21: the export carries an approval's whole trail, in order. This
// runs an approval through request, notify and resolve against a real server,
// then reads it back only through GET /v1/audit as an operator. It reads the
// approval path; it changes nothing in it.
func TestExportAuditCarriesApprovalChain(t *testing.T) {
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfy.Close()
	f := newNotifyFixture(t, ntfy.URL)
	// Keep ann's and bob's credentials: the default would revoke them.
	boot, err := f.st.BootstrapOperatorWith(context.Background(), "root", store.BootstrapOptions{KeepExistingCredentials: true})
	if err != nil {
		t.Fatal(err)
	}
	rootTok := boot.Token
	call := func(method, path, token, body string) []byte {
		t.Helper()
		req, err := http.NewRequest(method, f.base+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s = %d: %s", method, path, resp.StatusCode, out)
		}
		return out
	}

	// Request (and notify).
	created := call("POST", "/v1/approvals", f.tokens["ann"], createApprovalBody(f.channel.ID, f.ids["ann"]))
	var createdResp schema.CreateApprovalResponseV1
	if err := json.Unmarshal(created, &createdResp); err != nil {
		t.Fatalf("decode created approval: %v: %s", err, created)
	}
	approval := createdResp.Approval
	// Resolve (and notify).
	call("POST", fmt.Sprintf("/v1/approvals/%d/decisions", approval.ID), f.tokens["bob"],
		fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, f.ids["bob"]))
	subject := fmt.Sprintf("approval:%d", approval.ID)
	f.chain(t, subject, []string{
		store.AuditApprovalCreated, approvals.AuditNotifySent,
		store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent,
	})

	// Export.
	events := parseJSONL(t, string(call("GET", "/v1/audit", rootTok, "")))
	var got []string
	var lastID int64
	for _, e := range events {
		if e.ID <= lastID {
			t.Fatalf("event %d follows %d: the export is not in ascending id order", e.ID, lastID)
		}
		lastID = e.ID
		if e.Subject == subject {
			got = append(got, e.Action)
		}
	}
	want := []string{
		store.AuditApprovalCreated, approvals.AuditNotifySent,
		store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("exported chain for %s = %v, want %v", subject, got, want)
	}

	// A non-operator who took part in the approval still reads none of it.
	req, err := http.NewRequest("GET", f.base+"/v1/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.tokens["ann"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a non-operator's export = %d, want 403", resp.StatusCode)
	}
}
