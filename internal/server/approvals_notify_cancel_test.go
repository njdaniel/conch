package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #155. Once an approval is created or resolved, telling people about it
// and recording that they were told must not depend on the client that made
// the request still being connected. A client that hung up while conchd was
// talking to ntfy used to cancel the delivery and the audit row with it.

// slowNtfy is an ntfy stand-in that holds each delivery until the test lets it
// go, so a test can hang up at exactly the moment conchd is delivering.
type slowNtfy struct {
	*httptest.Server
	status   int
	arrived  chan string // one value (the body) per delivery that reached ntfy
	release  chan struct{}
	mu       sync.Mutex
	finished int // deliveries answered in full
}

func newSlowNtfy(t *testing.T, status int) *slowNtfy {
	t.Helper()
	n := &slowNtfy{status: status, arrived: make(chan string, 16), release: make(chan struct{}, 16)}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n.arrived <- string(body)
		select {
		case <-n.release:
		case <-r.Context().Done():
			return // conchd gave up on the delivery
		}
		w.WriteHeader(n.status)
		n.mu.Lock()
		n.finished++
		n.mu.Unlock()
	}))
	t.Cleanup(n.Close)
	return n
}

func (n *slowNtfy) done() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.finished
}

type notifyFixture struct {
	st      *store.Store
	srv     *Server
	addr    string
	base    string
	channel store.Channel
	ids     map[string]int64
	tokens  map[string]string
}

func newNotifyFixture(t *testing.T, ntfyURL string) *notifyFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &notifyFixture{st: st, ids: map[string]int64{}, tokens: map[string]string{}}
	f.srv = New(Config{AuthMode: AuthRequired, DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "test",
		Ntfy: approvals.NtfyConfig{Server: ntfyURL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: 5 * time.Second}}, st)
	if f.channel, err = st.CreateChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ann", "bob"} {
		p, err := st.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := st.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddChannelMember(ctx, "system", f.channel.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
		f.ids[name], f.tokens[name] = p.ID, tok
	}
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	f.base, f.addr = web.URL, strings.TrimPrefix(web.URL, "http://")
	return f
}

// send writes one complete request and returns the open connection.
func (f *notifyFixture) send(t *testing.T, method, target, who, body string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		method, target, f.addr, f.tokens[who], len(body), body)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// hangUp closes conn the way a client that vanished does.
func hangUp(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// chain waits until the audit events about subject are want, and fails with
// what it found if they never are.
func (f *notifyFixture) chain(t *testing.T, subject string, want []string) {
	t.Helper()
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, err := f.st.ListAuditEvents(context.Background(), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		got = got[:0]
		for _, e := range events {
			if e.Subject == subject {
				got = append(got, e.Action)
			}
		}
		if equalStringSlices(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit chain for %s = %v, want %v", subject, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// onlyApproval returns the id of the single approval in the store.
func (f *notifyFixture) onlyApproval(t *testing.T) int64 {
	t.Helper()
	open, err := f.st.ListOpenApprovals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open approvals = %d, want 1", len(open))
	}
	return open[0].ID
}

func TestHangUpDuringNotificationDoesNotSuppressIt(t *testing.T) {
	tests := []struct {
		name       string
		ntfyStatus int
		resolve    bool // hang up on the resolving decision instead of the creation
		want       []string
	}{
		{"created, delivered", http.StatusOK, false,
			[]string{store.AuditApprovalCreated, approvals.AuditNotifySent}},
		{"created, ntfy refuses", http.StatusInternalServerError, false,
			[]string{store.AuditApprovalCreated, approvals.AuditNotifyFailed}},
		{"resolved, delivered", http.StatusOK, true,
			[]string{store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent}},
		{"resolved, ntfy refuses", http.StatusInternalServerError, true,
			[]string{store.AuditApprovalCreated, approvals.AuditNotifyFailed, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifyFailed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ntfy := newSlowNtfy(t, tt.ntfyStatus)
			f := newNotifyFixture(t, ntfy.URL)
			wantDone := 1

			conn := f.send(t, "POST", "/v1/approvals", "ann", createApprovalBody(f.channel.ID, f.ids["ann"]))
			if tt.resolve {
				// Let the creation through whole; the hang-up is on the decision.
				<-ntfy.arrived
				ntfy.release <- struct{}{}
				if _, err := io.ReadAll(io.LimitReader(conn, 1)); err != nil {
					t.Fatalf("read the creation's answer: %v", err)
				}
				_ = conn.Close()
				id := f.onlyApproval(t)
				conn = f.send(t, "POST", fmt.Sprintf("/v1/approvals/%d/decisions", id), "bob",
					fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, f.ids["bob"]))
				wantDone = 2
			}
			// conchd is now inside the delivery; the transaction is committed.
			select {
			case <-ntfy.arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("the notification never reached ntfy")
			}
			hangUp(conn)
			// Long enough for the server to notice the client is gone.
			time.Sleep(100 * time.Millisecond)
			ntfy.release <- struct{}{}

			var id int64
			if tt.resolve {
				var n int
				// The approval is resolved, so it is no longer in the open list.
				events, err := f.st.ListAuditEvents(context.Background(), 0, 1000)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range events {
					if e.Action == store.AuditApprovalCreated {
						n++
						_, _ = fmt.Sscanf(e.Subject, "approval:%d", &id)
					}
				}
				if n != 1 {
					t.Fatalf("approval_created events = %d, want 1", n)
				}
			} else {
				id = f.onlyApproval(t)
			}
			f.chain(t, fmt.Sprintf("approval:%d", id), tt.want)
			if got := ntfy.done(); got != wantDone {
				t.Errorf("deliveries ntfy answered in full = %d, want %d: the hang-up cancelled one", got, wantDone)
			}
		})
	}
}

// The other side of the rule: a request that never commits tells nobody.
func TestNoNotificationWithoutACommit(t *testing.T) {
	ntfy := newSlowNtfy(t, http.StatusOK)
	f := newNotifyFixture(t, ntfy.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var req schema.CreateApprovalRequestV1
	if err := json.Unmarshal([]byte(createApprovalBody(f.channel.ID, f.ids["ann"])), &req); err != nil {
		t.Fatal(err)
	}
	_, err := f.srv.approvals.Create(ctx, store.ApprovalParams{
		RequesterID: f.ids["ann"], ChannelID: f.channel.ID, Title: req.Title, Body: req.Body, Options: req.Options,
		Deadline: req.Deadline.Time(), GraceDeadline: req.Deadline.Time().Add(time.Hour), Quorum: 1,
	})
	if err == nil {
		t.Fatal("creating an approval on a cancelled context succeeded")
	}
	select {
	case body := <-ntfy.arrived:
		t.Errorf("a notification was sent for an approval that was not created: %s", body)
	case <-time.After(200 * time.Millisecond):
	}
	events, err := f.st.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.HasPrefix(e.Subject, "approval:") {
			t.Errorf("audit row for an approval that was not created: %+v", e)
		}
	}
}
