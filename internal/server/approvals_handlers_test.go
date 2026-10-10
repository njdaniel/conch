package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// approvalTestFixture creates a channel, a requesting agent, and a deciding
// human directly in the store.
func approvalTestFixture(t *testing.T, srv *Server) (store.Channel, store.Principal, store.Principal) {
	t.Helper()
	channel, agent := createTestChannelAndPrincipal(t, srv)
	human, err := srv.store.CreatePrincipal(context.Background(), store.PrincipalHuman, "nick")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	// The agent uses MCP in these tests, so it needs membership and a manifest.
	grantAgent(t, srv, agent.ID, channel.ID)
	return channel, agent, human
}

func createApprovalBody(channelID, requesterID int64) string {
	deadline := schema.NewTimestamp(time.Now().Add(time.Hour))
	b, _ := json.Marshal(schema.CreateApprovalRequestV1{
		RequesterID: requesterID,
		ChannelID:   channelID,
		Title:       "Enter BTC long",
		Body:        "Signal fired; approve to place the order.",
		Options: []schema.Option{
			{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
		},
		Deadline: deadline,
	})
	return string(b)
}

func postJSON(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestCreateListDecideApprovalOverREST(t *testing.T) {
	srv := newTestServer(t)
	channel, agent, human := approvalTestFixture(t, srv)

	// Create.
	rec := postJSON(t, srv, "/v1/approvals", createApprovalBody(channel.ID, agent.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created schema.CreateApprovalResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Approval.State != schema.ApprovalStatePending || created.Approval.Quorum != 1 {
		t.Fatalf("created approval = %+v, want pending with defaulted quorum 1", created.Approval)
	}
	if err := created.Approval.Validate(); err != nil {
		t.Fatalf("created approval invalid on the wire: %v", err)
	}

	// List open.
	listRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/v1/approvals", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d", listRec.Code)
	}
	var listed schema.ListApprovalsResponseV1
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed.Approvals) != 1 || listed.Approvals[0].ID != created.Approval.ID {
		t.Fatalf("open approvals = %+v, want the created one", listed.Approvals)
	}

	// Decide (quorum 1 → resolves).
	decision := fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"risk is fine"}`, human.ID)
	decideRec := postJSON(t, srv, fmt.Sprintf("/v1/approvals/%d/decisions", created.Approval.ID), decision)
	if decideRec.Code != http.StatusOK {
		t.Fatalf("decide status = %d, body = %s", decideRec.Code, decideRec.Body.String())
	}
	var decided schema.CastDecisionResponseV1
	if err := json.Unmarshal(decideRec.Body.Bytes(), &decided); err != nil {
		t.Fatalf("decode decide response: %v", err)
	}
	if decided.State != schema.ApprovalStateResolved || decided.Resolution == nil ||
		decided.Resolution.Outcome != schema.OutcomeApproved {
		t.Fatalf("decide response = %+v, want resolved/approved with resolution", decided)
	}
	if err := decided.Resolution.Validate(); err != nil {
		t.Fatalf("resolution invalid on the wire: %v", err)
	}

	// The approval is no longer open.
	listRec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/v1/approvals", nil))
	listed = schema.ListApprovalsResponseV1{}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Approvals) != 0 {
		t.Fatalf("open approvals after resolve = %+v, want none", listed.Approvals)
	}
}

func TestCreateApprovalErrors(t *testing.T) {
	srv := newTestServer(t)
	channel, agent, _ := approvalTestFixture(t, srv)

	tests := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"malformed JSON", "{", http.StatusBadRequest, "invalid_request"},
		{"schema-invalid", `{"requester_id":1}`, http.StatusBadRequest, "invalid_request"},
		{"unknown requester", createApprovalBody(channel.ID, 999), http.StatusBadRequest, "requester_not_found"},
		{"unknown channel", createApprovalBody(999, agent.ID), http.StatusBadRequest, "channel_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postJSON(t, srv, "/v1/approvals", tt.body)
			assertAPIError(t, rec, tt.wantCode, tt.wantErr)
		})
	}
}

func TestCastDecisionErrors(t *testing.T) {
	srv := newTestServer(t)
	channel, agent, human := approvalTestFixture(t, srv)

	rec := postJSON(t, srv, "/v1/approvals", createApprovalBody(channel.ID, agent.ID))
	var created schema.CreateApprovalResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Approval.ID

	decideURL := fmt.Sprintf("/v1/approvals/%d/decisions", id)
	tests := []struct {
		name     string
		url      string
		body     string
		wantCode int
		wantErr  string
	}{
		{"non-numeric id", "/v1/approvals/nope/decisions", `{"principal_id":1,"option_id":"approve","reason":"x"}`, http.StatusBadRequest, "invalid_request"},
		{"missing reason", decideURL, fmt.Sprintf(`{"principal_id":%d,"option_id":"approve"}`, human.ID), http.StatusBadRequest, "invalid_request"},
		{"unknown principal", decideURL, `{"principal_id":999,"option_id":"approve","reason":"x"}`, http.StatusBadRequest, "principal_not_found"},
		{"agent may not decide", decideURL, fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"x"}`, agent.ID), http.StatusForbidden, "human_required"},
		{"unknown option", decideURL, fmt.Sprintf(`{"principal_id":%d,"option_id":"nope","reason":"x"}`, human.ID), http.StatusBadRequest, "unknown_option"},
		{"unknown approval", "/v1/approvals/424242/decisions", fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"x"}`, human.ID), http.StatusNotFound, "approval_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postJSON(t, srv, tt.url, tt.body)
			assertAPIError(t, rec, tt.wantCode, tt.wantErr)
		})
	}

	// Resolve it, then a further decision is a terminal-state protocol error.
	ok := postJSON(t, srv, decideURL, fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, human.ID))
	if ok.Code != http.StatusOK {
		t.Fatalf("resolving decision status = %d, body = %s", ok.Code, ok.Body.String())
	}
	late := postJSON(t, srv, decideURL, fmt.Sprintf(`{"principal_id":%d,"option_id":"reject","reason":"too late"}`, human.ID))
	assertAPIError(t, late, http.StatusConflict, "approval_terminal")
}

func TestApprovalRESTNtfyFailureDegradesFullChain(t *testing.T) {
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ntfy.Close()
	runApprovalRESTNtfyFailureFullChain(t, ntfy.URL, "status 500")
}

func TestApprovalRESTNtfyConnectionRefusedDegradesFullChain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	_ = ln.Close()
	runApprovalRESTNtfyFailureFullChain(t, url, "")
}

func runApprovalRESTNtfyFailureFullChain(t *testing.T, ntfyURL, wantDetail string) {
	t.Helper()
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff, Ntfy: approvals.NtfyConfig{Server: ntfyURL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: 200 * time.Millisecond}})
	channel, agent, human := approvalTestFixture(t, srv)

	rec := postJSON(t, srv, "/v1/approvals", createApprovalBody(channel.ID, agent.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created schema.CreateApprovalResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	decision := fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"risk is fine"}`, human.ID)
	decideRec := postJSON(t, srv, fmt.Sprintf("/v1/approvals/%d/decisions", created.Approval.ID), decision)
	if decideRec.Code != http.StatusOK {
		t.Fatalf("decide status = %d, body = %s", decideRec.Code, decideRec.Body.String())
	}

	events, err := srv.store.ListAuditEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	var failedDetail string
	subject := fmt.Sprintf("approval:%d", created.Approval.ID)
	for _, e := range events {
		if e.Subject != subject {
			continue
		}
		actions = append(actions, e.Action)
		if e.Action == approvals.AuditNotifyFailed && failedDetail == "" {
			failedDetail = e.Detail
		}
	}
	want := []string{store.AuditApprovalCreated, approvals.AuditNotifyFailed, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifyFailed}
	if !equalStringSlices(actions, want) {
		t.Fatalf("audit chain = %v, want %v", actions, want)
	}
	if wantDetail != "" && !strings.Contains(failedDetail, wantDetail) {
		t.Fatalf("notify_failed detail %q, want containing %q", failedDetail, wantDetail)
	}
}

func equalStringSlices(a, b []string) bool {
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

// Issue #157, the whole chain. A title with control characters in it used to
// stop both notifications from leaving conchd (Go's HTTP client refuses such a
// header value). Request, notify, resolve, audit must all happen whatever the
// title holds, and the title is stored and returned as written.
func TestApprovalWithControlCharactersInTitleIsNotified(t *testing.T) {
	titles := map[string]string{
		"newline":          "Deploy\nprod",
		"header injection": "Deploy\r\nX-Evil: 1",
		"NUL and DEL":      "Deploy\x00prod\x7f",
	}
	for name, title := range titles {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var titles []string
			evil := false
			ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				titles = append(titles, r.Header.Get("Title"))
				evil = evil || r.Header.Get("X-Evil") != ""
				mu.Unlock()
			}))
			defer ntfy.Close()
			srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff, Ntfy: approvals.NtfyConfig{Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second}})
			channel, agent, human := approvalTestFixture(t, srv)

			var req schema.CreateApprovalRequestV1
			if err := json.Unmarshal([]byte(createApprovalBody(channel.ID, agent.ID)), &req); err != nil {
				t.Fatal(err)
			}
			req.Title = title
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			rec := postJSON(t, srv, "/v1/approvals", string(body))
			if rec.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", rec.Code, rec.Body)
			}
			var created schema.CreateApprovalResponseV1
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if created.Approval.Title != title {
				t.Errorf("stored title = %q, want it as written %q", created.Approval.Title, title)
			}
			decision := fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, human.ID)
			if rec := postJSON(t, srv, fmt.Sprintf("/v1/approvals/%d/decisions", created.Approval.ID), decision); rec.Code != http.StatusOK {
				t.Fatalf("decide = %d %s", rec.Code, rec.Body)
			}

			events, err := srv.store.ListAuditEvents(context.Background(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var chain []string
			subject := fmt.Sprintf("approval:%d", created.Approval.ID)
			for _, e := range events {
				if e.Subject == subject {
					chain = append(chain, e.Action)
				}
			}
			want := []string{store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent}
			if !equalStringSlices(chain, want) {
				t.Errorf("audit chain = %v, want %v", chain, want)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(titles) != 2 {
				t.Fatalf("ntfy received %d notifications, want 2", len(titles))
			}
			for _, got := range titles {
				if strings.ContainsAny(got, "\r\n\x00\x7f") || !strings.Contains(got, "Deploy") {
					t.Errorf("Title header = %q", got)
				}
			}
			if evil {
				t.Error("the title injected a header into the ntfy request")
			}
		})
	}
}

// Issue #164, the whole chain, both halves. A megabyte of approval body still
// gets its notifications (cut to fit one ntfy message), and when ntfy cannot
// be reached the failure is audited and logged without the topic, which on a
// public ntfy server is what lets someone read the notifications.
func TestApprovalNotificationBodyAndTransportErrors(t *testing.T) {
	const topic = "tpc-SECRET-grep-me"
	t.Run("a huge body is still notified", func(t *testing.T) {
		var mu sync.Mutex
		var sizes []int
		ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			sizes = append(sizes, len(raw))
			mu.Unlock()
			if len(raw) > 4096 { // a self-hosted ntfy without attachments
				http.Error(w, "attachments not allowed", http.StatusBadRequest)
			}
		}))
		defer ntfy.Close()
		srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff, Ntfy: approvals.NtfyConfig{Server: ntfy.URL, ApprovalsTopic: topic, UrgentTopic: topic, Timeout: time.Second}})
		channel, agent, human := approvalTestFixture(t, srv)
		var req schema.CreateApprovalRequestV1
		if err := json.Unmarshal([]byte(createApprovalBody(channel.ID, agent.ID)), &req); err != nil {
			t.Fatal(err)
		}
		req.Body = strings.Repeat("Deploy everything to prod. ", 4000) // about 100 KiB
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		rec := postJSON(t, srv, "/v1/approvals", string(body))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", rec.Code, rec.Body)
		}
		var created schema.CreateApprovalResponseV1
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if created.Approval.Body != req.Body {
			t.Error("the stored approval body was changed")
		}
		decision := fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, human.ID)
		if rec := postJSON(t, srv, fmt.Sprintf("/v1/approvals/%d/decisions", created.Approval.ID), decision); rec.Code != http.StatusOK {
			t.Fatalf("decide = %d %s", rec.Code, rec.Body)
		}
		events, err := srv.store.ListAuditEvents(context.Background(), 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var chain []string
		for _, e := range events {
			if e.Subject == fmt.Sprintf("approval:%d", created.Approval.ID) {
				chain = append(chain, e.Action)
			}
		}
		want := []string{store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent}
		if !equalStringSlices(chain, want) {
			t.Errorf("audit chain = %v, want %v", chain, want)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, n := range sizes {
			if n > 4096 {
				t.Errorf("a notification body of %d bytes was sent", n)
			}
		}
	})
	t.Run("an unreachable ntfy is recorded without the topic", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		logs := captureLogs(t)
		srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff, Ntfy: approvals.NtfyConfig{Server: "http://alice:hunter2@" + addr, ApprovalsTopic: topic, UrgentTopic: topic, Timeout: 300 * time.Millisecond}})
		channel, agent, human := approvalTestFixture(t, srv)
		rec := postJSON(t, srv, "/v1/approvals", createApprovalBody(channel.ID, agent.ID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", rec.Code, rec.Body)
		}
		var created schema.CreateApprovalResponseV1
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		decision := fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, human.ID)
		if rec := postJSON(t, srv, fmt.Sprintf("/v1/approvals/%d/decisions", created.Approval.ID), decision); rec.Code != http.StatusOK {
			t.Fatalf("decide = %d %s", rec.Code, rec.Body)
		}
		events, err := srv.store.ListAuditEvents(context.Background(), 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		failed := 0
		for _, e := range events {
			text := e.Actor + " " + e.Action + " " + e.Subject + " " + e.Detail
			for _, secret := range []string{topic, "hunter2", addr} {
				if strings.Contains(text, secret) {
					t.Errorf("an audit row names %q: %s", secret, text)
				}
			}
			if e.Action == approvals.AuditNotifyFailed {
				failed++
				if !strings.Contains(e.Detail, "connection refused") {
					t.Errorf("notify_failed detail lost the cause: %q", e.Detail)
				}
			}
		}
		if failed != 2 {
			t.Errorf("notify_failed rows = %d, want 2", failed)
		}
		for _, secret := range []string{topic, "hunter2", addr} {
			if strings.Contains(logs.buf.String(), secret) {
				t.Errorf("a log line names %q", secret)
			}
		}
		if !strings.Contains(logs.buf.String(), "notification failed") {
			t.Error("the log capture is not seeing the notifier's lines")
		}
	})
}
