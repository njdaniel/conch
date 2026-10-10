package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #170, the whole chain (CLAUDE.md rule 3): request, notify, escalate,
// resolve, audit, through the REST API, with an ntfy server set and one of its
// two topics left empty. The notification that has no topic is not sent and
// is audited as not attempted; it used to be audited as notify_sent. The
// others are sent and audited as before, the approval's life is the same in
// every case, and conchd says once at start which topic is missing.
func TestNtfyTopicLeftEmptyFullChain(t *testing.T) {
	const notAttempted = ` error="not attempted: no ntfy topic configured for this notification"`
	for _, tt := range []struct {
		name              string
		approvals, urgent string
		// warns is the flag the start-up warning names, or "" for no warning.
		warns string
	}{
		{"both topics set", "approvals", "urgent", ""},
		{"the urgent topic left empty", "approvals", "", "--ntfy-urgent-topic"},
		{"the approvals topic left empty", "", "urgent", "--ntfy-topic"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogsLocked(t)
			var mu sync.Mutex
			var paths []string
			ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer ntfy.Close()

			srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff, Ntfy: approvals.NtfyConfig{
				Server: ntfy.URL, ApprovalsTopic: tt.approvals, UrgentTopic: tt.urgent, Timeout: time.Second}})
			defer srv.approvals.Close()

			// conchd said so at start, once, naming the flag; or said nothing.
			const warning = "an ntfy server is set but a topic is not"
			started := logs.String()
			switch n := strings.Count(started, warning); {
			case tt.warns == "" && n != 0:
				t.Errorf("with both topics set the start-up log warns about a topic:\n%s", started)
			case tt.warns != "" && (n != 1 || !strings.Contains(started, "level=WARN") || !strings.Contains(started, tt.warns)):
				t.Errorf("start-up log has %d warnings about a missing topic, want one at WARN naming %s:\n%s", n, tt.warns, started)
			}

			channel, agent, human := approvalTestFixture(t, srv)

			// Request: a deadline close enough to pass during the test.
			body, err := json.Marshal(schema.CreateApprovalRequestV1{
				RequesterID: agent.ID, ChannelID: channel.ID, Title: "Enter BTC long", Body: "Signal fired.",
				Options: []schema.Option{
					{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
					{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
				},
				Deadline: schema.NewTimestamp(time.Now().Add(300 * time.Millisecond)),
			})
			if err != nil {
				t.Fatal(err)
			}
			rec := postJSON(t, srv, "/v1/approvals", string(body))
			if rec.Code != http.StatusCreated {
				t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var created schema.CreateApprovalResponseV1
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			subject := fmt.Sprintf("approval:%d", created.Approval.ID)
			chain := func() []string {
				events, err := srv.store.ListAuditEvents(context.Background(), 0, 200)
				if err != nil {
					t.Fatal(err)
				}
				var rows []string
				for _, e := range events {
					if e.Subject != subject {
						continue
					}
					switch e.Action {
					case approvals.AuditNotifySent, approvals.AuditNotifyFailed:
						rows = append(rows, e.Action+" "+e.Detail)
					default:
						rows = append(rows, e.Action)
					}
				}
				return rows
			}
			wait := func(what string, rows int) {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for len(chain()) < rows {
					if time.Now().After(deadline) {
						t.Fatalf("waiting for %s: audit chain is %q", what, chain())
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			// Notify, then escalate at the deadline and notify again (or not).
			wait("the escalation and its notify row", 4)

			// Resolve: a human decides while it is escalated.
			decision := fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"risk is fine"}`, human.ID)
			if rec := postJSON(t, srv, fmt.Sprintf("/v1/approvals/%d/decisions", created.Approval.ID), decision); rec.Code != http.StatusOK {
				t.Fatalf("decide status = %d, body = %s", rec.Code, rec.Body.String())
			}
			wait("the resolution and its notify row", 7)

			// Audit: the chain, in order, with each notify row saying what
			// became of that notification.
			notify := func(event, topic string) string {
				if topic == "" {
					return approvals.AuditNotifyFailed + " event=" + event + notAttempted
				}
				return approvals.AuditNotifySent + " event=" + event
			}
			want := []string{
				store.AuditApprovalCreated,
				notify("created", tt.approvals),
				store.AuditApprovalEscalated,
				notify("escalated", tt.urgent),
				store.AuditDecisionCast,
				store.AuditApprovalResolved,
				notify("resolved", tt.approvals),
			}
			if got := chain(); !slices.Equal(got, want) {
				t.Errorf("audit chain:\n got  %q\n want %q", got, want)
			}
			// And only what was recorded as sent reached ntfy.
			var wantPaths []string
			for _, topic := range []string{tt.approvals, tt.urgent, tt.approvals} {
				if topic != "" {
					wantPaths = append(wantPaths, "/"+topic)
				}
			}
			mu.Lock()
			got := append([]string(nil), paths...)
			mu.Unlock()
			if !slices.Equal(got, wantPaths) {
				t.Errorf("requests to ntfy = %v, want %v", got, wantPaths)
			}
			// The approval itself is resolved the same way whatever the topics.
			a, err := srv.store.ApprovalByID(context.Background(), created.Approval.ID)
			if err != nil || a.State != schema.ApprovalStateResolved {
				t.Errorf("approval state = %s (%v), want resolved", a.State, err)
			}
			// No further warning after start: a notification not attempted is
			// in the audit log, not a line per approval at WARN.
			if n := strings.Count(logs.String(), warning); (tt.warns == "") != (n == 0) || n > 1 {
				t.Errorf("after the approval's life the log has %d warnings about a missing topic", n)
			}
		})
	}
}

// lockedLog is a log destination that can be read while conchd's goroutines
// are still writing to it.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLogsLocked swaps the default slog logger for one writing to a
// lockedLog, until the test ends.
func captureLogsLocked(t *testing.T) *lockedLog {
	t.Helper()
	l := &lockedLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}
