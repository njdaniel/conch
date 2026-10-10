package approvals

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// ntfyCounter is an ntfy stand-in that records the path of every request.
type ntfyCounter struct {
	mu    sync.Mutex
	paths []string
	*httptest.Server
}

func newNtfyCounter(t *testing.T) *ntfyCounter {
	t.Helper()
	c := &ntfyCounter{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *ntfyCounter) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...)
}

// topicCases is every way the two topics can be set, with an ntfy server set
// in all of them.
var topicCases = []struct {
	name              string
	approvals, urgent string
	missing           []string
}{
	{"both topics", "approvals", "urgent", nil},
	{"only the approvals topic", "approvals", "", []string{"--ntfy-urgent-topic"}},
	{"only the urgent topic", "", "urgent", []string{"--ntfy-topic"}},
	{"neither topic", "", "", []string{"--ntfy-topic", "--ntfy-urgent-topic"}},
}

// A notification whose topic was left empty is not sent, and says so with
// ErrNoTopic: it used to return nil, which is what a delivered notification
// returns (issue #170). Which topic each notification uses is unchanged.
func TestNtfyTopicLeftEmptyIsNotSentAndSaysSo(t *testing.T) {
	a := store.Approval{ID: 42, RequesterID: 7, ChannelID: 3, Title: "Ship it", Body: "Please review", Deadline: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	r := schema.ApprovalResolutionV1{ApprovalID: 42, Outcome: schema.OutcomeApproved, OptionID: "approve"}
	for _, tt := range topicCases {
		t.Run(tt.name, func(t *testing.T) {
			ntfy := newNtfyCounter(t)
			n, err := NewNtfyNotifier(NtfyConfig{Server: ntfy.URL, ApprovalsTopic: tt.approvals, UrgentTopic: tt.urgent, Timeout: time.Second})
			if err != nil || n == nil {
				t.Fatalf("NewNtfyNotifier = %v, %v; a server with a topic left empty is still a notifier", n, err)
			}
			if got := n.MissingTopics(); !slices.Equal(got, tt.missing) {
				t.Errorf("MissingTopics = %v, want %v", got, tt.missing)
			}
			ctx := context.Background()
			sends := []struct {
				what  string
				topic string
				send  func() error
			}{
				{"created", tt.approvals, func() error { return n.ApprovalCreated(ctx, a) }},
				{"escalated", tt.urgent, func() error { return n.ApprovalEscalated(ctx, a) }},
				{"resolved", tt.approvals, func() error { return n.ApprovalResolved(ctx, a, r) }},
			}
			var want []string
			for _, s := range sends {
				err := s.send()
				if s.topic == "" {
					if !errors.Is(err, ErrNoTopic) {
						t.Errorf("%s with its topic left empty = %v, want ErrNoTopic", s.what, err)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s = %v, want it sent", s.what, err)
				}
				want = append(want, "/"+s.topic)
			}
			if got := ntfy.requests(); !slices.Equal(got, want) {
				t.Errorf("requests to ntfy = %v, want %v", got, want)
			}
		})
	}
}

// The manager records a notification whose topic is empty as not attempted,
// in fixed words, for each of the transitions; the others are sent and
// recorded as before; and nothing reaches ntfy for the ones not attempted.
// The approval is taken through its whole life: created, escalated at its
// deadline, resolved by a decision.
func TestTopicLeftEmptyIsAuditedAsNotAttempted(t *testing.T) {
	const notAttemptedDetail = `error="not attempted: no ntfy topic configured for this notification"`
	for _, tt := range topicCases {
		t.Run(tt.name, func(t *testing.T) {
			ntfy := newNtfyCounter(t)
			n, err := NewNtfyNotifier(NtfyConfig{Server: ntfy.URL, ApprovalsTopic: tt.approvals, UrgentTopic: tt.urgent, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			s := openTestStore(t)
			m := New(s, n)
			defer m.Close()
			ctx := context.Background()
			channelID, agentID, humanID := fixture(t, s)

			now := time.Now()
			a, err := m.Create(ctx, params(channelID, agentID, now.Add(30*time.Millisecond), now.Add(time.Hour)))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			subject := fmt.Sprintf("approval:%d", a.ID)
			notifyRows := func() []string {
				events, err := s.ListAuditEvents(ctx, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				var rows []string
				for _, e := range events {
					if e.Subject == subject && (e.Action == AuditNotifySent || e.Action == AuditNotifyFailed) {
						rows = append(rows, e.Action+" "+e.Detail)
					}
				}
				return rows
			}
			// Created, then escalated when the deadline passes.
			waitFor(t, func() bool { return len(notifyRows()) == 2 })
			if _, r, err := m.Decide(ctx, a.ID, humanID, "approve", "fine"); err != nil || r == nil {
				t.Fatalf("Decide = %+v, %v", r, err)
			}
			waitFor(t, func() bool { return len(notifyRows()) == 3 })

			row := func(event, topic string) string {
				if topic == "" {
					return AuditNotifyFailed + " event=" + event + " " + notAttemptedDetail
				}
				return AuditNotifySent + " event=" + event
			}
			want := []string{row("created", tt.approvals), row("escalated", tt.urgent), row("resolved", tt.approvals)}
			if got := notifyRows(); !slices.Equal(got, want) {
				t.Errorf("notify rows:\n got  %q\n want %q", got, want)
			}
			var wantRequests []string
			for _, topic := range []string{tt.approvals, tt.urgent, tt.approvals} {
				if topic != "" {
					wantRequests = append(wantRequests, "/"+topic)
				}
			}
			if got := ntfy.requests(); !slices.Equal(got, wantRequests) {
				t.Errorf("requests to ntfy = %v, want %v", got, wantRequests)
			}
			for _, r := range notifyRows() {
				if strings.Contains(r, ErrNoTopic.Error()) {
					t.Errorf("row %q carries the notifier's own error text; the wording of a not-attempted row is the manager's", r)
				}
			}
		})
	}
}
