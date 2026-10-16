package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// decideFixture is a channel with an agent requester and three human members
// who hold the right to decide, and one open approval of the given quorum.
type decideFixture struct {
	s            *Store
	channelID    int64
	approvalID   int64
	ann, bob, cy Principal
}

func newDecideFixture(t *testing.T, quorum int) decideFixture {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	f := decideFixture{s: s, channelID: ch.ID}
	for _, p := range []struct {
		dst  *Principal
		name string
	}{{&f.ann, "ann"}, {&f.bob, "bob"}, {&f.cy, "cy"}} {
		*p.dst, err = s.CreatePrincipal(ctx, PrincipalHuman, p.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutChannelMember(ctx, "system", ch.ID, p.dst.ID, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	a, err := s.CreateApproval(ctx, ApprovalParams{
		RequesterID: agent.ID,
		ChannelID:   ch.ID,
		Title:       "Deploy",
		Body:        "Approve the deploy.",
		Options: []schema.Option{
			{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
		},
		Deadline:      now.Add(time.Hour),
		GraceDeadline: now.Add(2 * time.Hour),
		Quorum:        quorum,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.approvalID = a.ID
	return f
}

// decisionRows counts what a decision writes: its row and its audit row.
func (f decideFixture) decisionRows(t *testing.T) (decisions, audits int) {
	t.Helper()
	ctx := context.Background()
	if err := f.s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM approval_decisions WHERE approval_id = ?", f.approvalID).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_events WHERE action = ? AND subject = ?", AuditDecisionCast, approvalSubject(f.approvalID)).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	return decisions, audits
}

// CastMemberDecision checks membership and the right in the transaction that
// writes the decision (issue #453), so a revoke or a removal that commits
// after the handler's own check still stops the decision, and writes nothing.
func TestCastMemberDecisionChecksTheRightInTheTransaction(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		before func(f decideFixture) error
		want   error
	}{
		{"right revoked", func(f decideFixture) error {
			_, err := f.s.SetChannelMemberCanDecide(ctx, "system", f.channelID, f.ann.ID, false)
			return err
		}, ErrNoDecideRight},
		{"removed from the channel", func(f decideFixture) error {
			_, err := f.s.RemoveChannelMember(ctx, "system", f.channelID, f.ann.ID)
			return err
		}, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecideFixture(t, 1)
			if err := tc.before(f); err != nil {
				t.Fatal(err)
			}
			_, r, err := f.s.CastMemberDecision(ctx, f.approvalID, f.ann.ID, "approve", "ok")
			if !errors.Is(err, tc.want) || r != nil {
				t.Fatalf("decision = %v, %v; want %v", r, err, tc.want)
			}
			if d, a := f.decisionRows(t); d != 0 || a != 0 {
				t.Errorf("a refused decision wrote %d decisions and %d audit rows", d, a)
			}
			if a, err := f.s.ApprovalByID(ctx, f.approvalID); err != nil || a.State != schema.ApprovalStatePending {
				t.Errorf("approval = %+v, %v; want pending", a, err)
			}
		})
	}
}

// Quorum counts only the concurring decisions of members who hold the right
// now. The decision of someone who lost it, or left, stays on the record and
// in the resolution, and a revoke alone changes no state.
func TestCastMemberDecisionCountsOnlyCurrentHolders(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		lose func(f decideFixture) error
	}{
		{"revoked", func(f decideFixture) error {
			_, err := f.s.SetChannelMemberCanDecide(ctx, "system", f.channelID, f.ann.ID, false)
			return err
		}},
		{"left", func(f decideFixture) error {
			_, err := f.s.RemoveChannelMember(ctx, "system", f.channelID, f.ann.ID)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecideFixture(t, 2)
			if _, r, err := f.s.CastMemberDecision(ctx, f.approvalID, f.ann.ID, "approve", "yes"); err != nil || r != nil {
				t.Fatalf("ann = %v, %v", r, err)
			}
			if err := tc.lose(f); err != nil {
				t.Fatal(err)
			}
			if a, err := f.s.ApprovalByID(ctx, f.approvalID); err != nil || a.State != schema.ApprovalStatePending {
				t.Fatalf("after ann lost the right approval = %+v, %v; want pending", a, err)
			}
			if _, r, err := f.s.CastMemberDecision(ctx, f.approvalID, f.bob.ID, "approve", "yes"); err != nil || r != nil {
				t.Fatalf("bob = %v, %v; want pending, ann's decision no longer counts", r, err)
			}
			_, r, err := f.s.CastMemberDecision(ctx, f.approvalID, f.cy.ID, "approve", "yes")
			if err != nil || r == nil {
				t.Fatalf("cy = %v, %v; want the resolution", r, err)
			}
			if r.Outcome != schema.OutcomeApproved || len(r.Decisions) != 3 {
				t.Errorf("resolution = %+v; want approved with all 3 decisions", r)
			}
		})
	}
}

// Without authentication (CastDecision) a decider need not be a member and
// every decision counts, as before #453.
func TestCastDecisionWithoutAuthenticationCountsEveryDecision(t *testing.T) {
	ctx := context.Background()
	f := newDecideFixture(t, 2)
	outsider, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "outsider")
	if err != nil {
		t.Fatal(err)
	}
	if _, r, err := f.s.CastDecision(ctx, f.approvalID, outsider.ID, "approve", "yes"); err != nil || r != nil {
		t.Fatalf("outsider = %v, %v", r, err)
	}
	if _, err := f.s.SetChannelMemberCanDecide(ctx, "system", f.channelID, f.ann.ID, false); err != nil {
		t.Fatal(err)
	}
	_, r, err := f.s.CastDecision(ctx, f.approvalID, f.ann.ID, "approve", "yes")
	if err != nil || r == nil || len(r.Decisions) != 2 {
		t.Fatalf("ann = %+v, %v; want resolved on two decisions", r, err)
	}
}
