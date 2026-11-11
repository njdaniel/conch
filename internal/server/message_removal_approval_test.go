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
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The approval-path chain with a message removal in it (issue #852, #849 Q2).
// A message that carries an approval card may be removed: the tombstone drops
// the card's payload from every read, and the approval it named stays pending
// and decidable through its own routes. The test runs the whole chain against
// a real server:
//
//	request (and notify) -> the card's message removed (and its payload gone
//	from every read) -> the approval decided through its route -> resolve (and
//	notify) -> audit
//
// The card is seeded under the store, as a message stored before the post
// routes refused a reserved payload is (issue #1345).
func TestMessageRemovalOfAnApprovalCardChain(t *testing.T) {
	var ntfyHits atomic.Int64
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		ntfyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfy.Close()
	f := newNotifyFixture(t, ntfy.URL)
	ctx := context.Background()
	boot, err := f.st.BootstrapOperatorWith(ctx, "root", store.BootstrapOptions{KeepExistingCredentials: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AddChannelMember(ctx, "system", f.channel.ID, boot.Principal.ID, 0); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token, body string) (int, []byte) {
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
		return resp.StatusCode, out
	}

	// Request, and notify.
	status, created := call("POST", "/v1/approvals", f.tokens["ann"], createApprovalBody(f.channel.ID, f.ids["ann"]))
	if status != http.StatusCreated {
		t.Fatalf("create approval = %d %s", status, created)
	}
	var resp schema.CreateApprovalResponseV1
	if err := json.Unmarshal(created, &resp); err != nil {
		t.Fatal(err)
	}
	approval := resp.Approval
	subject := fmt.Sprintf("approval:%d", approval.ID)
	f.chain(t, subject, []string{store.AuditApprovalCreated, approvals.AuditNotifySent})
	if ntfyHits.Load() != 1 {
		t.Fatalf("ntfy was told %d times after the request, want 1", ntfyHits.Load())
	}

	// The card: a message carrying the approval, seeded under the store.
	cardJSON, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	execOnDB(t, f.srv,
		"INSERT INTO messages (channel_id, author_id, body, created_at, payload_schema, payload_json) VALUES (?, ?, 'Approve BTC long?', ?, ?, ?)",
		f.channel.ID, f.ids["ann"], time.Now().UnixMilli(), schema.ApprovalV1Name, string(cardJSON))
	cardID := queryDB(t, f.srv, "SELECT MAX(id) FROM messages")
	cardPath := "/v2/channels/ops/messages/" + cardID
	listBefore := func(token string) string {
		_, body := call("GET", "/v2/channels/ops/messages?limit=100", token, "")
		return string(body)
	}
	if body := listBefore(f.tokens["bob"]); !strings.Contains(body, schema.ApprovalV1Name) || !strings.Contains(body, "Enter BTC long") {
		t.Fatalf("the card is not readable before the removal: %s", body)
	}

	// The card's message is removed by the owner: a 204, a tombstone on every
	// read, and not one byte of the card.
	if status, body := call("DELETE", cardPath, f.tokens["ann"], ""); status != http.StatusForbidden {
		t.Fatalf("a plain member removed the card = %d %s", status, body)
	}
	if status, body := call("DELETE", cardPath, boot.Token, ""); status != http.StatusNoContent {
		t.Fatalf("remove the card = %d %s", status, body)
	}
	for _, who := range []string{"ann", "bob"} {
		body := listBefore(f.tokens[who])
		if strings.Contains(body, schema.ApprovalV1Name) || strings.Contains(body, "Enter BTC long") || strings.Contains(body, "Signal fired") || !strings.Contains(body, schema.MessageRemovedBody) {
			t.Errorf("%s still reads the card: %s", who, body)
		}
	}
	// The approval itself is untouched: still pending, still listed.
	status, got := call("GET", fmt.Sprintf("/v1/approvals/%d", approval.ID), f.tokens["bob"], "")
	if status != http.StatusOK || !strings.Contains(string(got), `"state":"pending"`) {
		t.Fatalf("the approval after the card's removal = %d %s", status, got)
	}
	if status, got = call("GET", "/v1/approvals", f.tokens["bob"], ""); status != http.StatusOK || !strings.Contains(string(got), "Enter BTC long") {
		t.Errorf("the approvals list lost the approval: %d %s", status, got)
	}
	if ntfyHits.Load() != 1 {
		t.Errorf("removing the card notified ntfy (%d hits)", ntfyHits.Load())
	}

	// Resolve through its own route, and notify.
	if status, got := call("POST", fmt.Sprintf("/v1/approvals/%d/decisions", approval.ID), f.tokens["bob"],
		fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, f.ids["bob"])); status >= 300 {
		t.Fatalf("decide = %d %s", status, got)
	}

	// Audit: the approval's chain, whole and in order, and the removal's own
	// row, by the owner, with ids only.
	f.chain(t, subject, []string{
		store.AuditApprovalCreated, approvals.AuditNotifySent,
		store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent,
	})
	if ntfyHits.Load() != 2 {
		t.Errorf("ntfy was told %d times, want 2 (created, resolved)", ntfyHits.Load())
	}
	f.chain(t, "message:"+cardID, []string{store.AuditMessageRemoved})
	events, err := f.st.ListAuditEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Subject == "message:"+cardID && e.Action == store.AuditMessageRemoved {
			if e.Actor != fmt.Sprintf("principal:%d", boot.Principal.ID) || e.Detail != fmt.Sprintf("channel=%d", f.channel.ID) {
				t.Errorf("the removal's audit row = %+v", e)
			}
		}
		if e.Action == store.AuditMessageRemoved && (strings.Contains(e.Detail, "Enter BTC") || strings.Contains(e.Detail, "approval")) {
			t.Errorf("the removal's audit row holds the card: %+v", e)
		}
	}
	// And the resolved approval still reads as resolved after the card is
	// gone.
	if status, got := call("GET", fmt.Sprintf("/v1/approvals/%d", approval.ID), f.tokens["ann"], ""); status != http.StatusOK || !strings.Contains(string(got), "approved") {
		t.Errorf("the resolved approval = %d %s", status, got)
	}
}
