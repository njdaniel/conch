package approvals

import (
	"context"
	"io"
	"mime"
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
		// A title arrives as JSON, so it is valid UTF-8 by the time it is
		// stored; if bytes that are not ever got here, each becomes U+FFFD
		// and the notification is still delivered.
		{"invalid UTF-8", "Ship\xff\xc3it", "Ship\ufffd\ufffdit"},
		{"line and paragraph separators", "Ship\u2028it\u2029now", "Ship it now"},
		// ntfy would decode this encoded-word into "Ship\r\nX-Evil: 1" plus
		// an escape sequence; split, it is shown as typed.
		{"RFC 2047 encoded-word", "=?UTF-8?Q?Ship=0D=0AX-Evil:_1=1B[2J?=", "= ?UTF-8?Q?Ship=0D=0AX-Evil:_1=1B[2J?="},
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
				// What ntfy will make of the value after its own decoding.
				if decoded, err := new(mime.WordDecoder).DecodeHeader(g.title); err != nil || decoded != g.title {
					t.Errorf("delivery %d: ntfy would decode the Title into %q (err %v)", i, decoded, err)
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
			// The body is the place for the title as written (made valid
			// UTF-8, which ntfy requires of a message body: issue #164).
			if !strings.HasPrefix(got[0].body, strings.ToValidUTF8(tt.title, "\uFFFD")+"\n") {
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

// Issue #164. ntfy turns a body over 4,096 bytes into an attachment and, with
// attachments not configured, refuses it: the push never happens. Whatever the
// approval's body, what is sent fits one ntfy message, is valid UTF-8, and
// says when it was cut and where the rest is. A body that fits is untouched.
func TestNtfyBodyFitsOneMessage(t *testing.T) {
	const ntfyLimit = 4096
	var mu sync.Mutex
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		// As a self-hosted ntfy without attachments answers an oversized body.
		if len(raw) > ntfyLimit || !utf8.Valid(raw) {
			http.Error(w, `{"code":40014,"error":"attachments not allowed"}`, http.StatusBadRequest)
			return
		}
	}))
	defer ts.Close()
	n, err := NewNtfyNotifier(NtfyConfig{Server: ts.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	const mark = "\n[cut here; the full text is in approval 42]"
	tests := []struct {
		name    string
		body    string
		wantCut bool
	}{
		{"short", "please review", false},
		{"just under what fits", strings.Repeat("a", maxBodyBytes-200), false},
		{"well over ntfy's limit", strings.Repeat("a", 5000), true},
		{"a megabyte", strings.Repeat("deploy the thing. ", 60000), true},
		{"multi-byte text across the cut", strings.Repeat("発射", 3000), true},
		{"four-byte characters across the cut", strings.Repeat("🚀", 2000), true},
		{"invalid UTF-8", "ok \xff\xfe then text", false},
		// A run of bytes that are not UTF-8 becomes one replacement character.
		{"a long run of invalid UTF-8", strings.Repeat("\xff", 6000), false},
		{"invalid UTF-8 scattered through a long body", strings.Repeat("ab\xffcd ", 2000), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			bodies = nil
			mu.Unlock()
			a := store.Approval{ID: 42, RequesterID: 7, ChannelID: 3, Title: "Ship it", Body: tt.body, Deadline: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
			if err := n.ApprovalCreated(context.Background(), a); err != nil {
				t.Fatalf("created: %v", err)
			}
			if err := n.ApprovalEscalated(context.Background(), a); err != nil {
				t.Fatalf("escalated: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 2 {
				t.Fatalf("deliveries = %d, want 2", len(bodies))
			}
			for i, got := range bodies {
				if len(got) > ntfyLimit || !utf8.ValidString(got) {
					t.Errorf("delivery %d: body is %d bytes, valid UTF-8 %v", i, len(got), utf8.ValidString(got))
				}
				if strings.HasSuffix(got, mark) != tt.wantCut {
					t.Errorf("delivery %d: cut mark present = %v, want %v (tail %q)", i, strings.HasSuffix(got, mark), tt.wantCut, got[max(0, len(got)-60):])
				}
				if !strings.Contains(got, "Requester: principal:7") {
					t.Errorf("delivery %d: the lines Conch adds are missing", i)
				}
				if !tt.wantCut && utf8.ValidString(tt.body) && !strings.HasSuffix(got, tt.body) {
					t.Errorf("delivery %d: a body that fits was changed", i)
				}
			}
		})
	}
}

func TestNotificationBodyCut(t *testing.T) {
	// The cut never splits a character, wherever it falls.
	for pad := 0; pad < 8; pad++ {
		got := notificationBody(strings.Repeat("a", maxBodyBytes-4+pad)+strings.Repeat("🚀", 10), 9)
		if !utf8.ValidString(got) || len(got) > 4096 {
			t.Errorf("pad %d: %d bytes, valid %v", pad, len(got), utf8.ValidString(got))
		}
	}
	if got := notificationBody(strings.Repeat("a", maxBodyBytes), 9); len(got) != maxBodyBytes {
		t.Errorf("a body exactly at the cap was changed: %d bytes", len(got))
	}
	if got := notificationBody(strings.Repeat("a", maxBodyBytes+1), 9); !strings.HasSuffix(got, "approval 9]") {
		t.Errorf("a body one byte over the cap was not cut: tail %q", got[len(got)-30:])
	}
}

// Issue #164. The HTTP client's error quotes the request URL, and the URL's
// path is the topic: on a public ntfy server, what lets someone read the
// notifications. The notifier's errors are written to the audit log and the
// server log, so they say what went wrong and not where.
func TestNtfyTransportErrorsDoNotNameTheTopic(t *testing.T) {
	const topic = "tpc-SECRET-grep-me"
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedAddr := refused.Addr().String()
	_ = refused.Close()
	// A server that never answers. It is released at the end of the test: a
	// handler that has not read the request body is not told when the client
	// gives up, and Close would wait for it for ever.
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))
	defer hang.Close()
	defer close(release)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+refusedAddr+"/elsewhere/"+topic, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	tests := []struct {
		name   string
		server string
		want   string // part of the cause that must survive
	}{
		{"connection refused", "http://" + refusedAddr, "connection refused"},
		{"connection refused, credentials in the URL", "http://alice:hunter2@" + refusedAddr, "connection refused"},
		{"timeout", hang.URL, "deadline exceeded"},
		{"redirect to a dead address", redirect.URL, "connection refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewNtfyNotifier(NtfyConfig{Server: tt.server, ApprovalsTopic: topic, UrgentTopic: topic, Timeout: 300 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			err = n.ApprovalCreated(context.Background(), store.Approval{ID: 1, Title: "x", Body: "b", Deadline: time.Now()})
			if err == nil {
				t.Fatal("no error")
			}
			text := err.Error()
			for _, secret := range []string{topic, "hunter2", "alice", refusedAddr, "127.0.0.1", "http://"} {
				if strings.Contains(text, secret) {
					t.Errorf("the error names %q: %s", secret, text)
				}
			}
			if !strings.Contains(text, tt.want) || !strings.HasPrefix(text, "ntfy: ") {
				t.Errorf("error = %q, want it to keep the cause %q", text, tt.want)
			}
		})
	}
}
