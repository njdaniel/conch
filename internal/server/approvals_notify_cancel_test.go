package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
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
	// gone receives "METHOD path" each time the server sees a request's
	// context end: when the request completes, or when its client hangs up.
	gone chan string
	// returned receives "METHOD path" each time a handler has returned.
	returned chan string
	st       *store.Store
	srv      *Server
	addr     string
	base     string
	channel  store.Channel
	ids      map[string]int64
	tokens   map[string]string
}

func newNotifyFixture(t *testing.T, ntfyURL string) *notifyFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &notifyFixture{st: st, ids: map[string]int64{}, tokens: map[string]string{}, gone: make(chan string, 64), returned: make(chan string, 64)}
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
	handler := f.srv.Handler()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		go func(ctx context.Context, what string) {
			<-ctx.Done()
			f.gone <- what
		}(r.Context(), r.Method+" "+r.URL.Path)
		handler.ServeHTTP(w, r)
		f.returned <- r.Method + " " + r.URL.Path
	}))
	t.Cleanup(web.Close)
	f.base, f.addr = web.URL, strings.TrimPrefix(web.URL, "http://")
	return f
}

// send writes one complete POST and returns the open connection.
func (f *notifyFixture) send(t *testing.T, target, who, body string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		target, f.addr, f.tokens[who], len(body), body)
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

// waitGone blocks until the server has seen the context of the request what
// ("METHOD path") end. After a hang-up on a request whose handler is still
// running, that is the moment the server noticed the client was gone.
func (f *notifyFixture) waitGone(t *testing.T, what string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-f.gone:
			if got == what {
				return
			}
		case <-deadline:
			t.Fatalf("the server never saw %q end", what)
		}
	}
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

			abandoned := "POST /v1/approvals"
			conn := f.send(t, "/v1/approvals", "ann", createApprovalBody(f.channel.ID, f.ids["ann"]))
			if tt.resolve {
				// Let the creation through whole; the hang-up is on the decision.
				<-ntfy.arrived
				ntfy.release <- struct{}{}
				if _, err := io.ReadAll(io.LimitReader(conn, 1)); err != nil {
					t.Fatalf("read the creation's answer: %v", err)
				}
				_ = conn.Close()
				id := f.onlyApproval(t)
				abandoned = fmt.Sprintf("POST /v1/approvals/%d/decisions", id)
				conn = f.send(t, fmt.Sprintf("/v1/approvals/%d/decisions", id), "bob",
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
			// Not a sleep: ntfy answers only once the server has seen this
			// request's context cancelled, while its handler is still inside
			// the delivery. On the old code that cancellation aborted the
			// delivery, however slow the machine.
			f.waitGone(t, abandoned)
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

// Told if and only if committed, whenever the caller's deadline falls: before
// the transaction, inside it, at the commit, or during the delivery. Every
// approval that exists has exactly one "created" notification row, every call
// that failed left nothing, and the two counts agree with what Create said.
func TestNotificationMatchesCommitUnderRandomDeadlines(t *testing.T) {
	ntfy := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer ntfy.Close()
	f := newNotifyFixture(t, ntfy.URL)
	bg := context.Background()
	params := func() store.ApprovalParams {
		deadline := time.Now().Add(time.Hour)
		return store.ApprovalParams{
			RequesterID: f.ids["ann"], ChannelID: f.channel.ID, Title: "t", Body: "b",
			Options: []schema.Option{
				{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
				{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
			},
			Deadline: deadline, GraceDeadline: deadline.Add(time.Hour), Quorum: 1,
		}
	}
	ok, failed := 0, 0
	create := func(ctx context.Context) {
		if _, err := f.srv.approvals.Create(ctx, params()); err == nil {
			ok++
		} else {
			failed++
		}
	}

	// How long one whole Create takes here, for the clock-driven loop below.
	started := time.Now()
	create(bg)
	oneCreate := time.Since(started)
	if ok != 1 {
		t.Fatal("Create with nothing cancelling it was refused")
	}

	// First, cancellation placed by construction. A countdown context cancels
	// itself the n-th time it is consulted, so stepping n from 1 lands the
	// cancellation at each point where Create looks at its context, one after
	// the other, whatever the machine's speed or number of processors. The
	// database driver looks a varying number of times, so the count is not the
	// same in every run; the loop stops only once Create has got through ten
	// times running, well past its last look.
	const through = 10
	throughInARow := 0
	for n := 1; n <= 1000 && throughInARow < through; n++ {
		before := ok
		create(newCountdownContext(bg, n))
		if ok > before {
			throughInARow++
		} else {
			throughInARow = 0
		}
	}
	placedOK, placedFailed := ok, failed
	if placedFailed == 0 || throughInARow < through {
		t.Fatalf("placed cancellations: %d created, %d refused, %d through in a row at the end: both outcomes must occur", placedOK, placedFailed, throughInARow)
	}

	// Then cancellation by the clock, which also reaches a deadline that passes
	// while the database driver is working. How these split between created and
	// refused depends on the machine (with one processor no timer fires inside
	// Create at all), so nothing is required of the split: the loop above is
	// what guarantees both outcomes.
	span := 2 * oneCreate
	rng := rand.New(rand.NewSource(5)) // #nosec G404 -- test timing only
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithTimeout(bg, time.Duration(rng.Int63n(int64(span)+1)))
		create(ctx)
		cancel()
	}
	events, err := f.st.ListAuditEvents(bg, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	created, notified := map[string]int{}, map[string]int{}
	for _, e := range events {
		switch e.Action {
		case store.AuditApprovalCreated:
			created[e.Subject]++
		case approvals.AuditNotifySent, approvals.AuditNotifyFailed:
			notified[e.Subject]++
		}
	}
	open, err := f.st.ListOpenApprovals(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != ok || len(created) != ok {
		t.Errorf("Create succeeded %d times; %d approvals stored, %d approval_created subjects", ok, len(open), len(created))
	}
	for _, a := range open {
		subject := fmt.Sprintf("approval:%d", a.ID)
		if created[subject] != 1 || notified[subject] != 1 {
			t.Errorf("%s: %d approval_created and %d notify rows, want one of each", subject, created[subject], notified[subject])
		}
	}
	if len(notified) != ok {
		t.Errorf("notify rows for %d approvals, want %d", len(notified), ok)
	}
	t.Logf("placed: %d created, %d refused; by the clock: %d created, %d refused", placedOK, placedFailed, ok-placedOK, failed-placedFailed)
}

// countdownContext is a context that cancels itself the n-th time it is
// consulted (Done or Err). It lets a test put a cancellation at an exact point
// in the code under test instead of hoping a timer lands there.
//
// It is cancelled by being asked, not by time passing: code that took the
// channel earlier and is waiting on it is woken when a later consultation
// reaches n, and never if none does. That is what a test wants (the n-th look
// is where the cancellation lands), and it is why this is not a general
// context: a parent's own cancellation is reported by Err but does not close
// this channel.
type countdownContext struct {
	context.Context
	mu   sync.Mutex
	left int
	done chan struct{}
}

func newCountdownContext(parent context.Context, n int) *countdownContext {
	return &countdownContext{Context: parent, left: n, done: make(chan struct{})}
}

func (c *countdownContext) consult() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.left > 0 {
		c.left--
		if c.left == 0 {
			close(c.done)
		}
	}
}

func (c *countdownContext) Done() <-chan struct{} {
	c.consult()
	return c.done
}

func (c *countdownContext) Err() error {
	c.consult()
	select {
	case <-c.done:
		return context.Canceled
	default:
		return c.Context.Err()
	}
}

// waitReturned blocks until the handler of the request what has returned.
func (f *notifyFixture) waitReturned(t *testing.T, what string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-f.returned:
			if got == what {
				return
			}
		case <-deadline:
			t.Fatalf("the handler of %q never returned", what)
		}
	}
}

// A decider who hangs up while conchd is delivering the notification has
// still decided. The handler then reads the approval back for a response
// nobody will get; on the request context that read failed and the hang-up
// was logged as a store failure (issue #158).
func TestHangUpOnADecisionIsNotLoggedAsAStoreFailure(t *testing.T) {
	logs := captureLogs(t)
	ntfy := newSlowNtfy(t, http.StatusOK)
	f := newNotifyFixture(t, ntfy.URL)

	conn := f.send(t, "/v1/approvals", "ann", createApprovalBody(f.channel.ID, f.ids["ann"]))
	<-ntfy.arrived
	ntfy.release <- struct{}{}
	if _, err := io.ReadAll(io.LimitReader(conn, 1)); err != nil {
		t.Fatalf("read the creation's answer: %v", err)
	}
	_ = conn.Close()
	id := f.onlyApproval(t)

	path := fmt.Sprintf("/v1/approvals/%d/decisions", id)
	conn = f.send(t, path, "bob", fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, f.ids["bob"]))
	select {
	case <-ntfy.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the notification never reached ntfy")
	}
	hangUp(conn)
	f.waitGone(t, "POST "+path)
	ntfy.release <- struct{}{}
	f.waitReturned(t, "POST "+path)

	f.chain(t, fmt.Sprintf("approval:%d", id), []string{
		store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent})
	if out := logs.buf.String(); strings.Contains(out, "level=ERROR") {
		t.Errorf("a decider hanging up was logged as an error:\n%s", out)
	}
}
