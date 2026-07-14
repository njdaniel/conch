package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// approvalFixture creates a channel, a requesting agent, n deciding humans,
// and one pending approval with the given quorum, returning the approval and
// the decider principals.
func approvalFixture(t *testing.T, s *Store, quorum, deciders int) (Approval, []Principal) {
	t.Helper()
	ctx := context.Background()
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "leviathan")
	if err != nil {
		t.Fatal(err)
	}
	humans := make([]Principal, deciders)
	for i := range humans {
		humans[i], err = s.CreatePrincipal(ctx, PrincipalHuman, fmt.Sprintf("human-%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	a, err := s.CreateApproval(ctx, ApprovalParams{
		RequesterID: agent.ID,
		ChannelID:   ch.ID,
		Title:       "Enter BTC long",
		Body:        "Signal fired; approve to place the order.",
		Payload:     &schema.Payload{Schema: "leviathan.trade_signal.v1", Data: json.RawMessage(`{"symbol":"BTC","side":"buy"}`)},
		Options: []schema.Option{
			{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
		},
		Deadline:      now.Add(time.Hour),
		GraceDeadline: now.Add(2 * time.Hour),
		Quorum:        quorum,
		Escalation:    &schema.EscalationTarget{Kind: schema.EscalationTargetNtfyTopic, Topic: "conch-urgent"},
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	return a, humans
}

func auditActions(t *testing.T, s *Store) []string {
	t.Helper()
	events, err := s.ListAuditEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	actions := make([]string, len(events))
	for i, e := range events {
		actions[i] = e.Action
	}
	return actions
}

func TestCreateApprovalPendingWithAudit(t *testing.T) {
	s := openTestStore(t)
	a, _ := approvalFixture(t, s, 1, 0)

	if a.State != schema.ApprovalStatePending {
		t.Errorf("state = %s, want pending", a.State)
	}
	got, err := s.ApprovalByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("ApprovalByID: %v", err)
	}
	if got.State != schema.ApprovalStatePending || got.Quorum != 1 || len(got.Options) != 2 {
		t.Errorf("read back = %+v", got)
	}
	if got.Payload == nil || string(got.Payload.Data) != `{"symbol":"BTC","side":"buy"}` {
		t.Errorf("payload = %+v, want round-tripped verbatim", got.Payload)
	}
	if got.Escalation == nil || got.Escalation.Topic != "conch-urgent" {
		t.Errorf("escalation = %+v", got.Escalation)
	}
	if err := got.ToSchema().Validate(); err != nil {
		t.Errorf("stored approval fails schema validation: %v", err)
	}
	if actions := auditActions(t, s); len(actions) != 1 || actions[0] != AuditApprovalCreated {
		t.Errorf("audit = %v, want [approval_created]", actions)
	}
}

func TestCastDecision(t *testing.T) {
	tests := []struct {
		name       string
		quorum     int
		decide     func(s *Store, a Approval, humans []Principal) (*schema.ApprovalResolutionV1, error)
		wantErr    error
		wantOut    schema.Outcome
		wantAudits []string
	}{
		{
			name:   "single decision resolves at quorum 1",
			quorum: 1,
			decide: func(s *Store, a Approval, humans []Principal) (*schema.ApprovalResolutionV1, error) {
				_, r, err := s.CastDecision(context.Background(), a.ID, humans[0].ID, "approve", "looks good")
				return r, err
			},
			wantOut:    schema.OutcomeApproved,
			wantAudits: []string{AuditApprovalCreated, AuditDecisionCast, AuditApprovalResolved},
		},
		{
			name:   "quorum 2 needs two concurring decisions",
			quorum: 2,
			decide: func(s *Store, a Approval, humans []Principal) (*schema.ApprovalResolutionV1, error) {
				ctx := context.Background()
				_, r, err := s.CastDecision(ctx, a.ID, humans[0].ID, "reject", "too risky")
				if err != nil {
					return nil, err
				}
				if r != nil {
					return nil, errors.New("first decision must not resolve at quorum 2")
				}
				_, r, err = s.CastDecision(ctx, a.ID, humans[1].ID, "reject", "agreed, too risky")
				return r, err
			},
			wantOut:    schema.OutcomeRejected,
			wantAudits: []string{AuditApprovalCreated, AuditDecisionCast, AuditDecisionCast, AuditApprovalResolved},
		},
		{
			name:   "dissenting decision does not meet quorum",
			quorum: 2,
			decide: func(s *Store, a Approval, humans []Principal) (*schema.ApprovalResolutionV1, error) {
				ctx := context.Background()
				if _, _, err := s.CastDecision(ctx, a.ID, humans[0].ID, "approve", "yes"); err != nil {
					return nil, err
				}
				_, r, err := s.CastDecision(ctx, a.ID, humans[1].ID, "reject", "no")
				return r, err
			},
			wantAudits: []string{AuditApprovalCreated, AuditDecisionCast, AuditDecisionCast},
		},
		{
			name:   "empty reason rejected at the store",
			quorum: 1,
			decide: func(s *Store, a Approval, humans []Principal) (*schema.ApprovalResolutionV1, error) {
				_, r, err := s.CastDecision(context.Background(), a.ID, humans[0].ID, "approve", "")
				return r, err
			},
			wantErr:    errors.New("schema: decision reason is required"),
			wantAudits: []string{AuditApprovalCreated},
		},
		{
			name:   "unknown option",
			quorum: 1,
			decide: func(s *Store, a Approval, humans []Principal) (*schema.ApprovalResolutionV1, error) {
				_, r, err := s.CastDecision(context.Background(), a.ID, humans[0].ID, "nonsense", "why not")
				return r, err
			},
			wantErr:    ErrUnknownOption,
			wantAudits: []string{AuditApprovalCreated},
		},
		{
			name:   "duplicate decision by same principal",
			quorum: 2,
			decide: func(s *Store, a Approval, humans []Principal) (*schema.ApprovalResolutionV1, error) {
				ctx := context.Background()
				if _, _, err := s.CastDecision(ctx, a.ID, humans[0].ID, "approve", "yes"); err != nil {
					return nil, err
				}
				_, r, err := s.CastDecision(ctx, a.ID, humans[0].ID, "approve", "yes again")
				return r, err
			},
			wantErr:    ErrDuplicateDecision,
			wantAudits: []string{AuditApprovalCreated, AuditDecisionCast},
		},
		{
			name:   "decision on resolved approval is a protocol error",
			quorum: 1,
			decide: func(s *Store, a Approval, humans []Principal) (*schema.ApprovalResolutionV1, error) {
				ctx := context.Background()
				if _, _, err := s.CastDecision(ctx, a.ID, humans[0].ID, "approve", "yes"); err != nil {
					return nil, err
				}
				_, r, err := s.CastDecision(ctx, a.ID, humans[1].ID, "reject", "too late")
				return r, err
			},
			wantErr:    ErrTerminalApproval,
			wantAudits: []string{AuditApprovalCreated, AuditDecisionCast, AuditApprovalResolved},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			a, humans := approvalFixture(t, s, tt.quorum, 2)

			r, err := tt.decide(s, a, humans)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("err = nil, want %v", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) && err.Error() != tt.wantErr.Error() {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("decide: %v", err)
			}

			if tt.wantOut != "" {
				if r == nil {
					t.Fatal("resolution = nil, want one")
				}
				if r.Outcome != tt.wantOut {
					t.Errorf("outcome = %s, want %s", r.Outcome, tt.wantOut)
				}
				if err := r.Validate(); err != nil {
					t.Errorf("resolution fails schema validation: %v", err)
				}
				stored, err := s.ResolutionByApprovalID(context.Background(), a.ID)
				if err != nil {
					t.Fatalf("ResolutionByApprovalID: %v", err)
				}
				storedJSON, _ := json.Marshal(stored)
				returnedJSON, _ := json.Marshal(*r)
				if string(storedJSON) != string(returnedJSON) {
					t.Errorf("stored resolution %s != returned %s", storedJSON, returnedJSON)
				}
			} else if tt.wantErr == nil && r != nil {
				t.Errorf("resolution = %+v, want nil", r)
			}

			if got := auditActions(t, s); !equalStrings(got, tt.wantAudits) {
				t.Errorf("audit chain = %v, want %v", got, tt.wantAudits)
			}
		})
	}
}

// TestConcurrentDecisionsExactlyOneResolves races many concurring decisions at
// quorum 1: exactly one must be the resolving decision, and the approval must
// carry exactly one resolution event.
func TestConcurrentDecisionsExactlyOneResolves(t *testing.T) {
	s := openTestStore(t)
	const deciders = 8
	a, humans := approvalFixture(t, s, 1, deciders)

	var wg sync.WaitGroup
	resolutions := make(chan *schema.ApprovalResolutionV1, deciders)
	errs := make(chan error, deciders)
	for i := 0; i < deciders; i++ {
		wg.Add(1)
		go func(p Principal) {
			defer wg.Done()
			_, r, err := s.CastDecision(context.Background(), a.ID, p.ID, "approve", "concurrent yes")
			if err != nil {
				errs <- err
				return
			}
			if r != nil {
				resolutions <- r
			}
		}(humans[i])
	}
	wg.Wait()
	close(resolutions)
	close(errs)

	var resolved int
	for range resolutions {
		resolved++
	}
	if resolved != 1 {
		t.Errorf("resolving decisions = %d, want exactly 1", resolved)
	}
	// Losers must fail with the terminal-state protocol error, never anything else.
	for err := range errs {
		if !errors.Is(err, ErrTerminalApproval) {
			t.Errorf("concurrent decision error = %v, want ErrTerminalApproval", err)
		}
	}
	if _, err := s.ResolutionByApprovalID(context.Background(), a.ID); err != nil {
		t.Errorf("resolution missing after race: %v", err)
	}
	got, err := s.ApprovalByID(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != schema.ApprovalStateResolved {
		t.Errorf("state = %s, want resolved", got.State)
	}
}

func TestEscalateAndExpire(t *testing.T) {
	s := openTestStore(t)
	a, _ := approvalFixture(t, s, 1, 0)
	ctx := context.Background()

	escalated, err := s.EscalateApproval(ctx, a.ID)
	if err != nil || !escalated {
		t.Fatalf("EscalateApproval = %v, %v; want true, nil", escalated, err)
	}
	// Escalating again is a benign no-op.
	escalated, err = s.EscalateApproval(ctx, a.ID)
	if err != nil || escalated {
		t.Fatalf("second EscalateApproval = %v, %v; want false, nil", escalated, err)
	}

	r, err := s.ExpireApproval(ctx, a.ID)
	if err != nil {
		t.Fatalf("ExpireApproval: %v", err)
	}
	if r == nil || r.Outcome != schema.OutcomeExpired || r.OptionID != "" {
		t.Fatalf("resolution = %+v, want outcome expired without option", r)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("expired resolution fails schema validation: %v", err)
	}
	// Expiring again is a benign no-op.
	if r2, err := s.ExpireApproval(ctx, a.ID); err != nil || r2 != nil {
		t.Fatalf("second ExpireApproval = %+v, %v; want nil, nil", r2, err)
	}

	got, err := s.ApprovalByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != schema.ApprovalStateExpired {
		t.Errorf("state = %s, want expired", got.State)
	}
	want := []string{AuditApprovalCreated, AuditApprovalEscalated, AuditApprovalExpired}
	if gotA := auditActions(t, s); !equalStrings(gotA, want) {
		t.Errorf("audit chain = %v, want %v", gotA, want)
	}
}

func TestDecisionDuringEscalationResolves(t *testing.T) {
	s := openTestStore(t)
	a, humans := approvalFixture(t, s, 1, 1)
	ctx := context.Background()

	if _, err := s.EscalateApproval(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	_, r, err := s.CastDecision(ctx, a.ID, humans[0].ID, "approve", "late but decisive")
	if err != nil {
		t.Fatalf("CastDecision during escalation: %v", err)
	}
	if r == nil || r.Outcome != schema.OutcomeApproved {
		t.Fatalf("resolution = %+v, want approved", r)
	}
}

func TestListOpenApprovals(t *testing.T) {
	s := openTestStore(t)
	a, humans := approvalFixture(t, s, 1, 1)
	ctx := context.Background()

	open, err := s.ListOpenApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != a.ID {
		t.Fatalf("open = %+v, want the pending approval", open)
	}

	if _, _, err := s.CastDecision(ctx, a.ID, humans[0].ID, "approve", "done"); err != nil {
		t.Fatal(err)
	}
	open, err = s.ListOpenApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("open after resolve = %+v, want none", open)
	}
}

func equalStrings(a, b []string) bool {
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
