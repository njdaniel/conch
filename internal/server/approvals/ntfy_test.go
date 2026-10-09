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
	"unicode"
	"unicode/utf8"

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
	checks := []struct{ path, priority, contains string }{{"/approvals", "default", "Please review"}, {"/approvals-urgent", "max", "Deadline passed"}, {"/approvals", "default", "resolved: approved"}}
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

// Issue #157. An approval's title goes into ntfy's Title header. Go's HTTP
// client refuses to send a header value with a control character in it, so a
// newline in a title used to stop every notification for that approval from
// leaving conchd. Whatever the title holds, the notification is delivered, the
// header is one clean value, and nothing in it becomes a header of its own.
func TestNtfyTitleHeaderSurvivesAnyTitle(t *testing.T) {
	type seen struct {
		title   string
		headers http.Header
		body    string
	}
	var mu sync.Mutex
	var got []seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{title: r.Header.Get("Title"), headers: r.Header.Clone(), body: string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	n, err := NewNtfyNotifier(NtfyConfig{Server: ts.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("deploy ", 200)
	tests := []struct {
		name  string
		title string
		want  string // the title part of the header, after the notification's own prefix
	}{
		{"plain", "Ship it", "Ship it"},
		{"newline", "Ship\nit", "Ship it"},
		{"carriage return and newline", "Ship\r\nit", "Ship it"},
		{"NUL", "Ship\x00it", "Ship it"},
		{"DEL", "Ship\x7fit", "Ship it"},
		{"escape sequence", "Ship \x1b[31mit", "Ship [31mit"},
		{"tab", "Ship\tit", "Ship it"},
		{"only control characters", "\n\r\x00", ""},
		{"leading and trailing whitespace", "  \n Ship it \n ", "Ship it"},
		{"header injection", "x\r\nX-Evil: 1\r\nPriority: max", "x X-Evil: 1 Priority: max"},
		{"non-ASCII is kept", "Déployer — 発射 🚀", "Déployer — 発射 🚀"},
		{"C1 control", "Ship\u0085it", "Ship it"},
		{"very long", long, ""},
	}
	resolution := schema.ApprovalResolutionV1{ApprovalID: 1, Outcome: schema.OutcomeApproved, OptionID: "approve", ResolvedAt: schema.NewTimestamp(time.Now())}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			got = nil
			mu.Unlock()
			a := store.Approval{ID: 1, RequesterID: 7, ChannelID: 3, Title: tt.title, Body: "body", Deadline: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
			// All three kinds of notification carry the title.
			if err := n.ApprovalCreated(context.Background(), a); err != nil {
				t.Fatalf("created: %v", err)
			}
			if err := n.ApprovalEscalated(context.Background(), a); err != nil {
				t.Fatalf("escalated: %v", err)
			}
			if err := n.ApprovalResolved(context.Background(), a, resolution); err != nil {
				t.Fatalf("resolved: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != 3 {
				t.Fatalf("deliveries = %d, want 3", len(got))
			}
			prefixes := []string{"Approval requested:", "URGENT approval escalated:", "Approval resolved:"}
			for i, g := range got {
				if !strings.HasPrefix(g.title, prefixes[i]) {
					t.Errorf("delivery %d: Title = %q, want prefix %q", i, g.title, prefixes[i])
				}
				rest := strings.TrimPrefix(strings.TrimPrefix(g.title, prefixes[i]), " ")
				if tt.name == "very long" {
					if len(g.title) > maxTitleBytes || !strings.HasPrefix(rest, "deploy deploy") || !utf8.ValidString(g.title) {
						t.Errorf("delivery %d: long title sent as %d bytes: %q", i, len(g.title), g.title)
					}
				} else if rest != tt.want {
					t.Errorf("delivery %d: title part = %q, want %q", i, rest, tt.want)
				}
				for _, r := range g.title {
					if unicode.IsControl(r) {
						t.Errorf("delivery %d: Title carries a control character: %q", i, g.title)
					}
				}
				if g.headers.Get("X-Evil") != "" {
					t.Errorf("delivery %d: the title injected a header: %v", i, g.headers)
				}
				if p := g.headers.Get("Priority"); p != "default" && p != "max" || len(g.headers.Values("Priority")) != 1 {
					t.Errorf("delivery %d: Priority = %v", i, g.headers.Values("Priority"))
				}
			}
			// The body is the place for the title as written.
			if !strings.HasPrefix(got[0].body, tt.title+"\n") {
				t.Errorf("the created notification's body does not start with the title as written: %q", got[0].body[:min(len(got[0].body), 60)])
			}
		})
	}
}

func TestHeaderValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"a", "a"},
		{"a  b", "a b"},
		{"a\n\n\nb", "a b"},
		{"\x00", ""},
		{"a\x00", "a"},
		{strings.Repeat("é", 200), strings.Repeat("é", maxTitleBytes/2)},
	}
	for _, tt := range tests {
		if got := headerValue(tt.in); got != tt.want {
			t.Errorf("headerValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// A multi-byte character is never cut in half at the cap.
	for n := 240; n < 260; n++ {
		if got := headerValue(strings.Repeat("a", n) + "発射"); !utf8.ValidString(got) || len(got) > maxTitleBytes {
			t.Errorf("headerValue cut at %d bytes into invalid UTF-8 or past the cap: %d bytes", n, len(got))
		}
	}
}
