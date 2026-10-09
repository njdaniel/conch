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
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #153. A client that sends a request and hangs up cancels the request's
// context while the handler is inside a store transaction. That used to leave
// the transaction open on a pooled connection: from then on approvals could
// not be created or decided, and audit rows written afterwards were lost.
//
// This is the approval path end to end, over real TCP: after hundreds of
// abandoned requests, a request -> notify -> resolve -> audit chain must still
// complete, and everything reported as written must be durable.
func TestAbandonedRequestsDoNotBreakTheApprovalChain(t *testing.T) {
	ctx := context.Background()
	var notified atomic.Int64
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		notified.Add(1)
	}))
	defer ntfy.Close()

	path := filepath.Join(t.TempDir(), "conch.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	srv := New(Config{AuthMode: AuthRequired, DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "test",
		Ntfy: approvals.NtfyConfig{Server: ntfy.URL, ApprovalsTopic: "approvals", UrgentTopic: "urgent", Timeout: time.Second}}, st)
	_, _, rootTok, err := st.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := st.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	member := func(name string) (store.Principal, string) {
		p, err := st.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := st.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddChannelMember(ctx, "system", channel.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
		return p, tok
	}
	mallory, malloryTok := member("mallory")
	ann, annTok := member("ann")
	bob, bobTok := member("bob")

	web := httptest.NewServer(srv.Handler())
	defer web.Close()
	addr := strings.TrimPrefix(web.URL, "http://")

	// abandon sends one complete request and closes the connection after
	// wait, without reading the response.
	abandon := func(method, target, token, body string, wait time.Duration) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Errorf("dial: %v", err)
			return
		}
		req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			method, target, addr, token, len(body), body)
		_, _ = conn.Write([]byte(req))
		time.Sleep(wait)
		// An abortive close, as a client that vanished would produce.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
	// The hang-up has to land while the handler is inside its transaction,
	// a window of a few hundred microseconds. Sweeping the wait across the
	// time a request takes, one request at a time, lands in it many times:
	// against the old code the store was poisoned within the first twenty
	// requests of this loop, every run. How long a request takes depends on
	// the machine (and on -race), so the sweep is sized from a measurement:
	// twice a completed request, and never less than 1.5 ms.
	span := 1500 * time.Microsecond
	started := time.Now()
	if status, body := callWith(t, web.URL, "PUT", fmt.Sprintf("/v1/channels/ops/members/%d", bob.ID), rootTok, ""); status >= 300 {
		t.Fatalf("timing request: %d %s", status, body)
	}
	if took := 2 * time.Since(started); took > span {
		span = took
	}
	abandoned := 0
	for round := 0; round < 4; round++ {
		for wait := time.Duration(0); wait <= span; wait += span / 60 {
			if round%2 == 0 {
				// Raising an approval: a transaction on the approval path.
				abandon("POST", "/v1/approvals", malloryTok, createApprovalBody(channel.ID, mallory.ID), wait)
			} else {
				// Another transactional route: membership.
				abandon("PUT", fmt.Sprintf("/v1/channels/ops/members/%d", bob.ID), rootTok, "", wait)
			}
			abandoned++
		}
	}

	// The chain, by people who had nothing to do with the abandoned requests.
	call := func(method, target, token, body string) (int, string) {
		t.Helper()
		return callWith(t, web.URL, method, target, token, body)
	}
	before := notified.Load()
	status, body := call("POST", "/v1/approvals", annTok, createApprovalBody(channel.ID, ann.ID))
	if status != http.StatusCreated {
		t.Fatalf("request: %d %s", status, body)
	}
	var created schema.CreateApprovalResponseV1
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	status, body = call("POST", fmt.Sprintf("/v1/approvals/%d/decisions", created.Approval.ID), bobTok,
		fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"fine"}`, bob.ID))
	if status != http.StatusOK {
		t.Fatalf("resolve: %d %s", status, body)
	}
	var decided schema.CastDecisionResponseV1
	if err := json.Unmarshal([]byte(body), &decided); err != nil {
		t.Fatal(err)
	}
	if decided.State != schema.ApprovalStateResolved || decided.Resolution == nil || decided.Resolution.Outcome != schema.OutcomeApproved {
		t.Fatalf("decision = %+v, want resolved and approved", decided)
	}
	if got := notified.Load() - before; got < 2 {
		t.Errorf("notifications for the chain = %d, want the created and resolved ones", got)
	}
	// A plain audit write after all that, which must not vanish.
	if _, err := st.AppendAuditEvent(ctx, "test", "after_abandoned_requests", "probe", "d"); err != nil {
		t.Fatalf("plain audit write: %v", err)
	}

	// What a restart would find: read through a second handle on the file.
	web.Close()
	other, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	events, err := other.ListAuditEvents(ctx, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("approval:%d", created.Approval.ID)
	var chain []string
	count := map[string]int{}
	for _, e := range events {
		count[e.Action]++
		if e.Subject == subject {
			chain = append(chain, e.Action)
		}
	}
	want := []string{store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent}
	if !equalStringSlices(chain, want) {
		t.Errorf("durable audit chain = %v, want %v", chain, want)
	}
	if count["after_abandoned_requests"] != 1 {
		t.Errorf("the plain audit write reported success but %d rows are durable", count["after_abandoned_requests"])
	}
	// Every approval an abandoned request did create is whole: one row, one
	// approval_created event. None is half there.
	open, err := other.ListOpenApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, wantCreated := count[store.AuditApprovalCreated], len(open)+1; got != wantCreated {
		t.Errorf("approval_created events = %d, approvals stored = %d (open %d plus the resolved one)", got, wantCreated, len(open))
	}
	t.Logf("%d abandoned requests; %d of their approvals were committed whole, the rest rolled back", abandoned, len(open))
}

// callWith makes one ordinary request and returns its status and body.
func callWith(t *testing.T, base, method, target, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, base+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}
