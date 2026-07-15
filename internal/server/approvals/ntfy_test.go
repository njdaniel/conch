package approvals

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

type ntfyRequest struct {
	Path     string
	Title    string
	Priority string
	Body     string
}

func TestNtfyNotifierLifecycleTopicsPriorityAndBody(t *testing.T) {
	var mu sync.Mutex
	var got []ntfyRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, ntfyRequest{Path: r.URL.Path, Title: r.Header.Get("Title"), Priority: r.Header.Get("Priority"), Body: string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	n, err := NewNtfyNotifier(NtfyConfig{Server: ts.URL, ApprovalsTopic: "approvals", UrgentTopic: "approvals-urgent", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	a := store.Approval{ID: 42, RequesterID: 7, ChannelID: 3, Title: "Ship it", Body: "Please review", Deadline: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	r := schema.ApprovalResolutionV1{ApprovalID: 42, Outcome: schema.OutcomeApproved, OptionID: "approve", Decisions: []schema.Decision{{PrincipalID: 1, OptionID: "approve", Reason: "ok", At: schema.NewTimestamp(time.Now())}}, ResolvedAt: schema.NewTimestamp(time.Now())}
	if err := n.ApprovalCreated(context.Background(), a); err != nil {
		t.Fatalf("created: %v", err)
	}
	if err := n.ApprovalEscalated(context.Background(), a); err != nil {
		t.Fatalf("escalated: %v", err)
	}
	if err := n.ApprovalResolved(context.Background(), a, r); err != nil {
		t.Fatalf("resolved: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("requests = %d, want 3: %+v", len(got), got)
	}
	checks := []struct{ path, priority, contains string }{{"/approvals", "default", "Please review"}, {"/approvals-urgent", "urgent", "Deadline passed"}, {"/approvals", "default", "resolved: approved"}}
	for i, c := range checks {
		if got[i].Path != c.path || got[i].Priority != c.priority || !strings.Contains(got[i].Body, c.contains) || got[i].Title == "" {
			t.Fatalf("request %d = %+v, want path %s priority %s body containing %q", i, got[i], c.path, c.priority, c.contains)
		}
	}
}

func TestNtfyNotifierReturnsServerAndConnectionErrorsQuickly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }))
	defer ts.Close()
	n, err := NewNtfyNotifier(NtfyConfig{Server: ts.URL, ApprovalsTopic: "approvals", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.ApprovalCreated(context.Background(), store.Approval{Title: "x", Deadline: time.Now()}); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("500 error = %v, want status 500", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	_ = ln.Close()
	n, err = NewNtfyNotifier(NtfyConfig{Server: url, ApprovalsTopic: "approvals", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := n.ApprovalCreated(context.Background(), store.Approval{Title: "x", Deadline: time.Now()}); err == nil {
		t.Fatal("connection refused error = nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("connection refused took %s, want short non-blocking failure", elapsed)
	}
}
