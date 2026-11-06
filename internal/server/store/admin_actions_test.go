package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestClaimAdminActionRunOnce: of many concurrent runs of one approved
// action, exactly one claims it (ai-administrators §3.5.2).
func TestClaimAdminActionRunOnce(t *testing.T) {
	ctx := WithAdminDispatch(context.Background())
	f := newAIAdminFixture(t)
	f.put(t, f.ownerA, schema.PermManageMembers)
	now := time.Now()
	action := schema.AdminActionV1{
		Action: schema.AdminActionDisableMember, Route: "POST /v1/principals/3/disable",
		Target:  &schema.AdminTargetV1{Kind: schema.AdminTargetPrincipal, ID: f.alice.ID, Name: f.alice.Name},
		AgentID: f.agent.ID, RequestedAt: schema.NewTimestamp(now),
	}
	data, _ := json.Marshal(action)
	a, err := f.s.CreateApproval(ctx, ApprovalParams{
		RequesterID: f.agent.ID, ChannelID: f.admin.ID, Title: "t", Body: "b",
		Payload:  &schema.Payload{Schema: schema.AdminActionV1Name, Data: data},
		Options:  []schema.Option{{ID: "run", Label: "Run it", Kind: schema.OptionKindApprove}, {ID: "refuse", Label: "No", Kind: schema.OptionKindReject}},
		Deadline: now.Add(time.Hour), GraceDeadline: now.Add(2 * time.Hour), Quorum: 1,
		AdminAction: &AdminActionRequest{Action: action, MaxPending: 8, Detail: "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, res, err := f.s.CastMemberDecision(ctx, a.ID, f.owner.ID, "run", "ok"); err != nil || res == nil {
		t.Fatalf("owner's decision: %v, %v", res, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, already := 0, 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.ClaimAdminAction(ctx, a.ID, f.agent.ID, time.Hour, time.Now())
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrApprovalAlreadyRun):
				already++
			default:
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || already != 15 {
		t.Errorf("claims: %d won, %d already run; want 1 and 15", wins, already)
	}
}
