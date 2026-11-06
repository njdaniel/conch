package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// convertedMemberApproves is issue #860: the guest Sam was refused on every
// approval route (guestRows); the member he became, the same principal, can
// raise an approval in the plaza and sees it listed, is refused a decision
// until his membership there may decide, then decides it, and the approval
// runs its whole chain: created, notified, decided, resolved, notified, in
// the audit log and on the ntfy stand-in, as e2e/dogfood asserts for any
// member. His right to decide is taken back after, so nothing later in the
// run sees an approver it did not make.
func (w *world) convertedMemberApproves(m *member) error {
	p := w.p
	title := w.pubName("sam-asks")
	var created schema.CreateApprovalResponseV1
	if err := m.api.call(http.MethodPost, "/v1/approvals", schema.CreateApprovalRequestV1{
		ChannelID: p.chanID, Title: title, Body: "raised by the member a guest became",
		Options: []schema.Option{
			{ID: "yes", Label: "Yes", Kind: schema.OptionKindApprove},
			{ID: "no", Label: "No", Kind: schema.OptionKindReject},
		},
		Deadline: schema.NewTimestamp(time.Now().Add(time.Hour)), Quorum: 1,
	}, &created); err != nil {
		return fmt.Errorf("the member raising an approval: %w", err)
	}
	id := created.Approval.ID
	if created.Approval.RequesterID != m.id {
		return fmt.Errorf("the approval's requester is %d, want the member %d", created.Approval.RequesterID, m.id)
	}
	var open schema.ListApprovalsResponseV1
	if err := m.api.call(http.MethodGet, "/v1/approvals", nil, &open); err != nil {
		return err
	}
	listed := false
	for _, a := range open.Approvals {
		listed = listed || a.ID == id
	}
	if !listed {
		return fmt.Errorf("approval %d is not in the member's open list", id)
	}
	if err := w.awaitNtfy("Approval requested: "+title, 1); err != nil {
		return err
	}

	// Not an approver yet: the refusal a member without can_decide gets.
	path := "/v1/approvals/" + itoa(id) + "/decisions"
	decide := schema.CastDecisionRequestV1{OptionID: "yes", Reason: "fine"}
	status, body, err := m.api.do(http.MethodPost, path, decide)
	if err != nil {
		return err
	}
	if err := checkRefusal(result{status: status, body: body}, answer{http.StatusForbidden, schema.ErrorCodeDecisionNotPermitted}, asked{}); err != nil {
		return fmt.Errorf("the member deciding without can_decide: %w", err)
	}

	membership := "/v1/channels/" + url.PathEscape(p.channel) + "/members/" + itoa(m.id)
	yes, no := true, false
	if err := w.op.call(http.MethodPut, membership, schema.PutChannelMemberRequestV1{CanDecide: &yes}, nil); err != nil {
		return err
	}
	defer func() {
		_ = w.op.call(http.MethodPut, membership, schema.PutChannelMemberRequestV1{CanDecide: &no}, nil)
	}()
	if err := m.api.call(http.MethodPost, path, decide, nil); err != nil {
		return fmt.Errorf("the member deciding with can_decide: %w", err)
	}
	var got schema.GetApprovalResponseV1
	if err := m.api.call(http.MethodGet, "/v1/approvals/"+itoa(id), nil, &got); err != nil {
		return err
	}
	if got.Approval.State != schema.ApprovalStateResolved {
		return fmt.Errorf("approval %d is %s after the member's decision, want resolved", id, got.Approval.State)
	}
	if err := w.awaitNtfy("Approval resolved: "+title, 1); err != nil {
		return err
	}

	// The audit chain, in order: created by the member, the request's
	// notification, his decision, resolved, the resolution's notification.
	// Notify rows follow their transition outside its transaction
	// (approval-object.md §6), so they are counted, not placed.
	subject := "approval:" + itoa(id)
	actor := "principal:" + itoa(m.id)
	var chain []string
	notified := 0
	for _, action := range []string{store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent} {
		rows, err := w.auditRows(action)
		if err != nil {
			return err
		}
		for _, e := range rows {
			if e.Subject != subject {
				continue
			}
			if action == approvals.AuditNotifySent {
				notified++
				continue
			}
			chain = append(chain, action+" "+e.Actor)
		}
	}
	want := []string{store.AuditApprovalCreated + " " + actor, store.AuditDecisionCast + " " + actor, store.AuditApprovalResolved + " system"}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		return fmt.Errorf("approval %d's audit chain is %q, want %q", id, chain, want)
	}
	if notified != 2 {
		return fmt.Errorf("approval %d has %d notify_sent rows, want 2 (requested, resolved)", id, notified)
	}
	return nil
}

// awaitNtfy waits up to five seconds for at least want notifications whose
// title contains s.
func (w *world) awaitNtfy(s string, want int) error {
	deadline := time.Now().Add(5 * time.Second)
	for w.d.ntfy.mentioning(s) < want {
		if time.Now().After(deadline) {
			return fmt.Errorf("ntfy: %d notifications titled %q within 5s, want %d", w.d.ntfy.mentioning(s), s, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}
