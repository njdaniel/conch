package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestSendServerFlagOverridesEnvironment(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{})
	}))
	defer server.Close()
	t.Setenv("CONCH_SERVER", "http://127.0.0.1:1")
	t.Setenv("CONCH_AUTHOR", "9")

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"send", "--server", server.URL, "general", "hello"}, &stdout, &stderr, "test")
	if err != nil {
		t.Fatalf("run send: %v", err)
	}
	if !called {
		t.Fatal("flag server was not called")
	}
}

func TestSendUsesServerEnvironment(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{})
	}))
	defer server.Close()
	t.Setenv("CONCH_SERVER", server.URL)
	t.Setenv("CONCH_AUTHOR", "9")

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"send", "general", "hello"}, &stdout, &stderr, "test")
	if err != nil {
		t.Fatalf("run send: %v", err)
	}
	if !called {
		t.Fatal("environment server was not called")
	}
}

func TestApprovalsList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/approvals" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(schema.ListApprovalsResponseV1{
			Approvals: []schema.ApprovalV1{
				{ID: 1, State: schema.ApprovalStatePending, Title: "Test", RequesterID: 42, Deadline: schema.NewTimestamp(time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC))},
			},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approvals", "list", "--server", server.URL}, &stdout, &stderr, "vtest")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Test") || !strings.Contains(out, "1") {
		t.Errorf("output missing approval data: %q", out)
	}
}

func TestApprovalsDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/approvals/1/decisions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req schema.CastDecisionRequestV1
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.PrincipalID != 42 || req.Reason != "LGTM" || req.OptionID != "approve" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.CastDecisionResponseV1{
			Decision:   schema.Decision{PrincipalID: 42, OptionID: "approve", Reason: "LGTM"},
			State:      schema.ApprovalStateResolved,
			Resolution: &schema.ApprovalResolutionV1{},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approve", "--server", server.URL, "--author", "42", "--reason", "LGTM", "1"}, &stdout, &stderr, "vtest")
	if err != nil {
		t.Fatalf("run failed: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "resolved") {
		t.Errorf("expected resolved message, got: %q", out)
	}
}

func TestApprovalsDecisionMissingReason(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approve", "--author", "42", "1"}, &stdout, &stderr, "vtest")
	if err == nil {
		t.Fatal("expected error for missing reason")
	}
	if !strings.Contains(err.Error(), "--reason is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestApprovalsDecisionTerminalState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(schema.Error{
			Code:    "invalid_state",
			Message: "approval is in terminal state",
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approve", "--server", server.URL, "--author", "42", "--reason", "LGTM", "1"}, &stdout, &stderr, "vtest")
	if err == nil {
		t.Fatal("expected error for terminal state")
	}

	out := stderr.String()
	if !strings.Contains(out, "approval 1 is no longer open") {
		t.Errorf("expected helpful terminal state error, got: %q", out)
	}
}
