package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
