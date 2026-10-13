package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// mcpToolCall is a tools/call request whose encoded length is exactly size:
// the named string argument is padded with "x" until it is. Other arguments
// are given as already-encoded JSON.
func mcpToolCall(t *testing.T, tool, others, padded string, size int) string {
	t.Helper()
	const shape = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{%s%q:"%s"}}}`
	if others != "" {
		others += ","
	}
	empty := fmt.Sprintf(shape, tool, others, padded, "")
	if size <= len(empty) {
		t.Fatalf("cannot build a %s request of only %d bytes", tool, size)
	}
	return fmt.Sprintf(shape, tool, others, padded, strings.Repeat("x", size-len(empty)))
}

func mcpRaw(srv *Server, token string, body io.Reader, length int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.ContentLength = length
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// countingReader counts what is read from it.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// An MCP request is held to the size a REST post may be (issue #237). Before,
// an agent could store a message of any size through post_message. A request
// of exactly the limit is served; one byte more is refused with the same 413
// the REST routes give, whatever it carries, and leaves nothing behind.
func TestMCPRequestSizeLimit(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff})
	channel, agent, _ := approvalTestFixture(t, srv)
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}
	lastAudit := func() int64 {
		t.Helper()
		id, err := srv.store.LastAuditID(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	approvalArgs := fmt.Sprintf(`"channel_id":%d,"title":"t","options":[{"id":"approve","kind":"approve","label":"Approve"},{"id":"reject","kind":"reject","label":"Reject"}],"deadline":%q,"quorum":1`,
		channel.ID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	const limit = schema.MaxPostRequestBytes

	over := []struct {
		name    string
		request string
	}{
		{"post_message, body one byte over", mcpToolCall(t, "post_message", fmt.Sprintf(`"channel":%q`, channel.Name), "body", limit+1)},
		{"post_message, far over", mcpToolCall(t, "post_message", fmt.Sprintf(`"channel":%q`, channel.Name), "body", 8*limit)},
		{"request_approval, body one byte over", mcpToolCall(t, "request_approval", approvalArgs, "body", limit+1)},
		{"request_approval, title one byte over", mcpToolCall(t, "request_approval", strings.Replace(approvalArgs, `"title":"t",`, `"body":"b",`, 1), "title", limit+1)},
	}
	for _, tt := range over {
		for _, framing := range []string{"with its length declared", "with no length declared"} {
			t.Run(tt.name+", "+framing, func(t *testing.T) {
				before := lastAudit()
				length := int64(len(tt.request))
				if framing == "with no length declared" {
					length = -1
				}
				rec := mcpRaw(srv, "token-1", strings.NewReader(tt.request), length)
				if rec.Code != http.StatusRequestEntityTooLarge || errCodeOf(t, rec.Body.String()) != "request_too_large" {
					t.Fatalf("status = %d, body = %.200s; want 413 request_too_large", rec.Code, rec.Body.String())
				}
				// A stored message and a created approval each write an audit
				// row in their own transaction: none means nothing was stored.
				if after := lastAudit(); after != before {
					t.Errorf("the refused request wrote audit rows %d..%d", before+1, after)
				}
			})
		}
	}

	t.Run("a request of exactly the limit is served", func(t *testing.T) {
		request := mcpToolCall(t, "post_message", fmt.Sprintf(`"channel":%q`, channel.Name), "body", limit)
		rec := mcpRaw(srv, "token-1", strings.NewReader(request), int64(len(request)))
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"isError":true`) {
			t.Fatalf("status = %d, body = %.300s; want the message posted", rec.Code, rec.Body.String())
		}
		var out struct {
			Result struct {
				Structured schema.PostMessageResponseV2 `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if got := len(out.Result.Structured.Message.Body); got < limit-512 {
			t.Errorf("stored body is %d bytes, want nearly the whole request (%d)", got, limit)
		}
	})

	t.Run("authentication comes first: a refused caller's body is never read", func(t *testing.T) {
		for _, token := range []string{"", "not-a-token"} {
			body := &countingReader{r: strings.NewReader(strings.Repeat("x", 4*limit))}
			rec := mcpRaw(srv, token, body, int64(4*limit))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("token %q: status = %d, want 401", token, rec.Code)
			}
			if n := body.n.Load(); n != 0 {
				t.Errorf("token %q: %d bytes of the body were read before the 401", token, n)
			}
		}
	})
}

// TestApprovalChainWithTheRequestSizeLimit is the approval path's end-to-end
// test for issue #237: bounding what /mcp reads changes nothing for an
// approval of ordinary size (request, notify, resolve, audit), and an
// oversized request_approval is refused before any of that chain exists.
func TestApprovalChainWithTheRequestSizeLimit(t *testing.T) {
	var delivered atomic.Int64
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		delivered.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ntfy.Close)
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff,
		Ntfy: approvals.NtfyConfig{Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "approvals-urgent"}})
	channel, agent, human := approvalTestFixture(t, srv)
	srv.cfg.MCPBearerTokens = map[string]int64{"token-1": agent.ID}

	// Oversized first: nothing of a chain may come of it.
	approvalArgs := fmt.Sprintf(`"channel_id":%d,"title":"Enter BTC long","options":[{"id":"approve","kind":"approve","label":"Approve"},{"id":"reject","kind":"reject","label":"Reject"}],"deadline":%q,"quorum":1`,
		channel.ID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	big := mcpToolCall(t, "request_approval", approvalArgs, "body", schema.MaxPostRequestBytes+1)
	if rec := mcpRaw(srv, "token-1", strings.NewReader(big), int64(len(big))); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request_approval = %d, want 413", rec.Code)
	}

	created := mcpCallInProcess[schema.RequestApprovalOutput](t, srv, 1, "request_approval", approvalMCPArguments(channel.ID, time.Now().Add(time.Hour)))
	awaited := make(chan schema.AwaitDecisionOutput, 1)
	go func() {
		awaited <- mcpCallInProcess[schema.AwaitDecisionOutput](t, srv, 2, "await_decision", map[string]any{"approval_id": created.ID, "timeout_ms": 5000})
	}()
	time.Sleep(300 * time.Millisecond)
	decideApprovalREST(t, srv, created.ID, human.ID)
	waitResult := <-awaited
	checked := mcpCallInProcess[schema.CheckDecisionOutput](t, srv, 3, "check_decision", map[string]any{"approval_id": created.ID})
	if waitResult.State != schema.ApprovalStateResolved || waitResult.Resolution == nil {
		t.Fatalf("await result = %+v, want resolved with resolution", waitResult)
	}
	if !reflect.DeepEqual(waitResult.Resolution, checked.Resolution) {
		t.Fatalf("await/check resolutions differ: await=%+v check=%+v", waitResult.Resolution, checked.Resolution)
	}

	events, err := srv.store.ListAuditEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var chain []string
	createdRows := 0
	for _, e := range events {
		if e.Subject == fmt.Sprintf("approval:%d", created.ID) {
			chain = append(chain, e.Action)
		}
		if e.Action == store.AuditApprovalCreated {
			createdRows++
		}
	}
	want := []string{store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent}
	if !reflect.DeepEqual(chain, want) {
		t.Errorf("audit chain = %v, want %v", chain, want)
	}
	if createdRows != 1 {
		t.Errorf("approvals created = %d, want 1: the oversized request must create none", createdRows)
	}
	if got := delivered.Load(); got != 2 {
		t.Errorf("notifications that reached ntfy = %d, want 2 (created, resolved): the oversized request must send none", got)
	}
}
