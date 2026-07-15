package approvals

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

// recordingNotifier records every delivery and can be told to fail.
type recordingNotifier struct {
	mu     sync.Mutex
	events []string
	fail   bool
}

func (n *recordingNotifier) record(event string, err error) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, event)
	return err
}

func (n *recordingNotifier) ApprovalCreated(_ context.Context, _ store.Approval) error {
	if n.fail {
		return n.record("created", errors.New("ntfy unreachable"))
	}
	return n.record("created", nil)
}

func (n *recordingNotifier) ApprovalEscalated(_ context.Context, _ store.Approval) error {
	return n.record("escalated", nil)
}

func (n *recordingNotifier) ApprovalResolved(_ context.Context, _ store.Approval, _ schema.ApprovalResolutionV1) error {
	return n.record("resolved", nil)
}

func (n *recordingNotifier) recorded() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.events...)
}

func fixture(t *testing.T, s *store.Store) (channelID int64, agentID int64, humanID int64) {
	t.Helper()
	ctx := context.Background()
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, store.PrincipalAgent, "leviathan")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.CreatePrincipal(ctx, store.PrincipalHuman, "nick")
	if err != nil {
		t.Fatal(err)
	}
	return ch.ID, agent.ID, human.ID
}

func params(channelID, agentID int64, deadline, grace time.Time) store.ApprovalParams {
	return store.ApprovalParams{
		RequesterID: agentID,
		ChannelID:   channelID,
		Title:       "Enter BTC long",
		Body:        "Signal fired; approve to place the order.",
		Options: []schema.Option{
			{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
		},
		Deadline:      deadline,
		GraceDeadline: grace,
		Quorum:        1,
	}
}

func auditChain(t *testing.T, s *store.Store, subject string) []string {
	t.Helper()
	events, err := s.ListAuditEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range events {
		if e.Subject == subject {
			actions = append(actions, e.Action)
		}
	}
	return actions
}

// waitFor polls until cond is true or the deadline passes — timers fire on
// their own goroutines, so tests wait on observable state, not sleeps.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

// TestFullChainRequestNotifyResolveAudit is the end-to-end approval-path test
// required by CLAUDE.md rule 3: request → notify → resolve → audit chain,
// asserted in order per approval-object.md §6.
func TestFullChainRequestNotifyResolveAudit(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{}
	m := New(s, n)
	defer m.Close()
	ctx := context.Background()
	channelID, agentID, humanID := fixture(t, s)

	// Request: an agent raises an approval.
	a, err := m.Create(ctx, params(channelID, agentID, time.Now().Add(time.Hour), time.Time{}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.State != schema.ApprovalStatePending {
		t.Fatalf("state = %s, want pending", a.State)
	}
	// Default grace = deadline + original window.
	if !a.GraceDeadline.After(a.Deadline) {
		t.Fatalf("grace %s not after deadline %s", a.GraceDeadline, a.Deadline)
	}

	// Notify: the creation notification fired.
	if events := n.recorded(); len(events) != 1 || events[0] != "created" {
		t.Fatalf("notifications = %v, want [created]", events)
	}

	// Resolve: a human decides with a required reason.
	d, r, err := m.Decide(ctx, a.ID, humanID, "approve", "signal confirmed on the chart")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Reason == "" || r == nil || r.Outcome != schema.OutcomeApproved {
		t.Fatalf("decision = %+v resolution = %+v", d, r)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("resolution invalid on the wire: %v", err)
	}

	// The resolution notification fired.
	waitFor(t, time.Second, func() bool { return len(n.recorded()) == 2 })
	if events := n.recorded(); events[1] != "resolved" {
		t.Fatalf("notifications = %v, want [created resolved]", events)
	}

	// Audit: the chain per approval-object.md §6, in order.
	subject := fmt.Sprintf("approval:%d", a.ID)
	want := []string{
		store.AuditApprovalCreated,
		AuditNotifySent, // created
		store.AuditDecisionCast,
		store.AuditApprovalResolved,
		AuditNotifySent, // resolved
	}
	got := auditChain(t, s, subject)
	if len(got) != len(want) {
		t.Fatalf("audit chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("audit chain = %v, want %v", got, want)
		}
	}

	// Any number of later readers see the identical resolution event.
	stored, err := s.ResolutionByApprovalID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Outcome != r.Outcome || stored.OptionID != r.OptionID || len(stored.Decisions) != len(r.Decisions) {
		t.Fatalf("stored resolution %+v != returned %+v", stored, r)
	}
}

func TestNotifyFailureIsAuditedNotFatal(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{fail: true}
	m := New(s, n)
	defer m.Close()
	channelID, agentID, _ := fixture(t, s)

	a, err := m.Create(context.Background(), params(channelID, agentID, time.Now().Add(time.Hour), time.Time{}))
	if err != nil {
		t.Fatalf("Create must succeed despite notify failure: %v", err)
	}
	want := []string{store.AuditApprovalCreated, AuditNotifyFailed}
	if got := auditChain(t, s, fmt.Sprintf("approval:%d", a.ID)); !equal(got, want) {
		t.Fatalf("audit chain = %v, want %v", got, want)
	}
}

func TestDeadlineEscalatesThenExpires(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{}
	m := New(s, n)
	defer m.Close()
	ctx := context.Background()
	channelID, agentID, _ := fixture(t, s)

	now := time.Now()
	a, err := m.Create(ctx, params(channelID, agentID, now.Add(30*time.Millisecond), now.Add(80*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool {
		got, err := s.ApprovalByID(ctx, a.ID)
		return err == nil && got.State == schema.ApprovalStateExpired
	})

	r, err := s.ResolutionByApprovalID(ctx, a.ID)
	if err != nil {
		t.Fatalf("expired approval must carry a resolution: %v", err)
	}
	if r.Outcome != schema.OutcomeExpired {
		t.Errorf("outcome = %s, want expired", r.Outcome)
	}
	want := []string{
		store.AuditApprovalCreated,
		AuditNotifySent, // created
		store.AuditApprovalEscalated,
		AuditNotifySent, // escalated (urgent)
		store.AuditApprovalExpired,
		AuditNotifySent, // expired confirmation
	}
	waitFor(t, time.Second, func() bool {
		return len(auditChain(t, s, fmt.Sprintf("approval:%d", a.ID))) == len(want)
	})
	if got := auditChain(t, s, fmt.Sprintf("approval:%d", a.ID)); !equal(got, want) {
		t.Fatalf("audit chain = %v, want %v", got, want)
	}
}

func TestDecisionDuringEscalationStillResolves(t *testing.T) {
	s := openTestStore(t)
	m := New(s, nil)
	defer m.Close()
	ctx := context.Background()
	channelID, agentID, humanID := fixture(t, s)

	now := time.Now()
	a, err := m.Create(ctx, params(channelID, agentID, now.Add(20*time.Millisecond), now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		got, err := s.ApprovalByID(ctx, a.ID)
		return err == nil && got.State == schema.ApprovalStateEscalated
	})

	_, r, err := m.Decide(ctx, a.ID, humanID, "approve", "late approval during escalation")
	if err != nil {
		t.Fatalf("Decide during escalation: %v", err)
	}
	if r == nil || r.Outcome != schema.OutcomeApproved {
		t.Fatalf("resolution = %+v, want approved", r)
	}
}

// TestRehydrateRearmsTimers restarts the manager (new Manager, same store) and
// asserts pending deadlines fire after the restart — including a deadline that
// passed entirely while the manager was down, which must escalate and then
// expire in order.
func TestRehydrateRearmsTimers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	channelID, agentID, _ := fixture(t, s)

	// First manager creates two approvals, then "crashes" (Close only stops
	// timers; the store — our durable state — survives).
	m1 := New(s, nil)
	now := time.Now()
	missed, err := m1.Create(ctx, params(channelID, agentID, now.Add(10*time.Millisecond), now.Add(20*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	future, err := m1.Create(ctx, store.ApprovalParams{
		RequesterID: agentID,
		ChannelID:   channelID,
		Title:       "Second approval",
		Body:        "Outlives the restart.",
		Options: []schema.Option{
			{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
		},
		Deadline:      now.Add(40 * time.Millisecond),
		GraceDeadline: now.Add(time.Hour),
		Quorum:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	m1.Close()

	// Let both deadlines pass while "down".
	time.Sleep(30 * time.Millisecond)

	m2 := New(s, nil)
	defer m2.Close()
	if err := m2.Rehydrate(ctx); err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}

	// The fully-missed approval must walk escalated → expired with the
	// complete audit chain, never skipping straight to expired.
	waitFor(t, 2*time.Second, func() bool {
		got, err := s.ApprovalByID(ctx, missed.ID)
		return err == nil && got.State == schema.ApprovalStateExpired
	})
	want := []string{store.AuditApprovalCreated, AuditNotifySent, store.AuditApprovalEscalated, AuditNotifySent, store.AuditApprovalExpired, AuditNotifySent}
	waitFor(t, time.Second, func() bool {
		return len(auditChain(t, s, fmt.Sprintf("approval:%d", missed.ID))) >= len(want)
	})
	if got := auditChain(t, s, fmt.Sprintf("approval:%d", missed.ID)); !equal(got, want) {
		t.Fatalf("missed approval audit chain = %v, want %v", got, want)
	}

	// The still-live approval escalates after restart when its deadline passes.
	waitFor(t, 2*time.Second, func() bool {
		got, err := s.ApprovalByID(ctx, future.ID)
		return err == nil && got.State == schema.ApprovalStateEscalated
	})
}

func TestCreateValidation(t *testing.T) {
	s := openTestStore(t)
	m := New(s, nil)
	defer m.Close()
	channelID, agentID, _ := fixture(t, s)
	now := time.Now()

	tests := []struct {
		name   string
		mutate func(*store.ApprovalParams)
	}{
		{"missing title", func(p *store.ApprovalParams) { p.Title = "" }},
		{"no options", func(p *store.ApprovalParams) { p.Options = nil }},
		{"missing reject option", func(p *store.ApprovalParams) {
			p.Options = []schema.Option{{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove}}
		}},
		{"zero deadline", func(p *store.ApprovalParams) { p.Deadline = time.Time{}; p.GraceDeadline = now.Add(time.Hour) }},
		{"quorum zero", func(p *store.ApprovalParams) { p.Quorum = 0 }},
		{"grace before deadline", func(p *store.ApprovalParams) { p.GraceDeadline = p.Deadline.Add(-time.Minute) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := params(channelID, agentID, now.Add(time.Hour), now.Add(2*time.Hour))
			tt.mutate(&p)
			if _, err := m.Create(context.Background(), p); err == nil {
				t.Fatal("Create succeeded, want validation error")
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
