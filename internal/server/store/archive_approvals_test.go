package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Approvals, runs and interventions in an archived channel (issue #1296): new
// ones are refused from inside the transaction that would write them; the
// ones already open carry on, and restore puts everything back.

// archiveApproval raises an approval in channelID as requester, deciding
// within an hour.
func archiveApproval(s *Store, requester, channelID int64, title string) (Approval, error) {
	now := time.Now()
	return s.CreateApproval(context.Background(), ApprovalParams{
		RequesterID: requester,
		ChannelID:   channelID,
		Title:       title,
		Options: []schema.Option{
			{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
			{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
		},
		Deadline:      now.Add(time.Hour),
		GraceDeadline: now.Add(2 * time.Hour),
		Quorum:        1,
	})
}

// workCounts is every table a new approval, run or intervention would add a
// row to.
func workCounts(t *testing.T, s *Store) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"approvals", "approval_decisions", "approval_resolutions", "runs", "run_activity", "interventions", "audit_events"} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", table, n))
	}
	return strings.Join(parts, " ")
}

// TestArchivedRefusesNewApprovalsRunsAndInterventions: while the channel is
// archived, each create answers ErrChannelArchived and writes nothing, not
// even an audit row; once restored, each works.
func TestArchivedRefusesNewApprovalsRunsAndInterventions(t *testing.T) {
	w := newInterventionWorld(t)
	s := w.s
	creates := []struct {
		name string
		call func() error
	}{
		{"approval", func() error {
			_, err := archiveApproval(s, w.agt.ID, w.ch.ID, "deploy?")
			return err
		}},
		{"run", func() error {
			_, err := w.start(w.ch.ID, 0, "late work")
			return err
		}},
		{"intervention", func() error {
			_, err := w.ask(w.rWide, w.alice, schema.InterventionKindCancel, "")
			return err
		}},
	}

	setArchived(t, s, w.ch, true)
	for _, c := range creates {
		before := workCounts(t, s)
		if err := c.call(); !errors.Is(err, ErrChannelArchived) {
			t.Errorf("%s in an archived channel: err = %v, want ErrChannelArchived", c.name, err)
		}
		if after := workCounts(t, s); after != before {
			t.Errorf("%s in an archived channel wrote rows:\nbefore %s\nafter  %s", c.name, before, after)
		}
	}
	// The channel next door is not archived, and is not refused.
	if _, err := archiveApproval(s, w.agt.ID, w.other.ID, "elsewhere"); err != nil {
		t.Errorf("an approval in a live channel beside the archived one: %v", err)
	}

	setArchived(t, s, w.ch, false)
	for _, c := range creates {
		if err := c.call(); err != nil {
			t.Errorf("%s after restore: %v", c.name, err)
		}
	}
}

// TestArchivedLetsInFlightWorkFinish: an approval, a run and an intervention
// opened before the archive carry on after it, as in a live channel: the
// approval is decided (or expires) and audited, the run changes state and
// reports, the intervention is answered.
func TestArchivedLetsInFlightWorkFinish(t *testing.T) {
	ctx := context.Background()
	w := newInterventionWorld(t)
	s := w.s
	decided, err := archiveApproval(s, w.agt.ID, w.ch.ID, "decide me")
	if err != nil {
		t.Fatal(err)
	}
	expiring, err := archiveApproval(s, w.agt.ID, w.ch.ID, "let me expire")
	if err != nil {
		t.Fatal(err)
	}
	escalating, err := archiveApproval(s, w.agt.ID, w.ch.ID, "escalate me")
	if err != nil {
		t.Fatal(err)
	}
	iv := w.mustAsk(t, w.rWide, w.alice, schema.InterventionKindCancel)

	setArchived(t, s, w.ch, true)

	_, res, err := s.CastDecision(ctx, decided.ID, w.alice.ID, "approve", "ok")
	if err != nil {
		t.Fatalf("a decision on an open approval in an archived channel: %v", err)
	}
	if res == nil || res.Outcome != schema.OutcomeApproved {
		t.Fatalf("the decision did not resolve the approval: %+v", res)
	}
	if got := w.auditOf(t, AuditApprovalResolved); len(got) != 1 || got[0].Subject != approvalSubject(decided.ID) {
		t.Errorf("approval_resolved audit rows = %+v, want one for approval %d", got, decided.ID)
	}
	if ok, err := s.EscalateApproval(ctx, escalating.ID); err != nil || !ok {
		t.Errorf("escalating an open approval in an archived channel = %t, %v", ok, err)
	}
	exp, err := s.ExpireApproval(ctx, expiring.ID)
	if err != nil || exp == nil || exp.Outcome != schema.OutcomeExpired {
		t.Errorf("expiring an open approval in an archived channel = %+v, %v", exp, err)
	}

	if _, err := s.SetRunState(ctx, principalActor(w.agt.ID), w.runWide, w.agtCred, w.agtReader(), schema.RunStateRunning, "", runT0.Add(time.Minute)); err != nil {
		t.Errorf("a state change on a run already going in an archived channel: %v", err)
	}
	w.mustActivity(t, w.runWide, "still working")
	if _, err := w.ack(iv.ID, schema.InterventionStateAcknowledged, "stopping"); err != nil {
		t.Errorf("answering an intervention in an archived channel: %v", err)
	}
	if _, err := s.SetRunState(ctx, principalActor(w.agt.ID), w.runWide, w.agtCred, w.agtReader(), schema.RunStateCompleted, "done", runT0.Add(2*time.Minute)); err != nil {
		t.Errorf("ending a run in an archived channel: %v", err)
	}
}
