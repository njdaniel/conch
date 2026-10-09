package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Under authentication the approval REST routes bind identity to the caller
// and to channel membership (issue #92). The fixture: alice is a member of
// "general" and "alpha"; root (operator) only of "general"; carol of nothing.

func decisionBody(principalID int64) string {
	if principalID == 0 {
		return `{"option_id":"approve","reason":"looks right"}`
	}
	return fmt.Sprintf(`{"principal_id":%d,"option_id":"approve","reason":"looks right"}`, principalID)
}

func openApprovals(t *testing.T, srv *Server) []store.Approval {
	t.Helper()
	open, err := srv.store.ListOpenApprovals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return open
}

func TestCreateApprovalIsBoundToCallerAndMembership(t *testing.T) {
	f := newMemberFixture(t)
	create := func(tok string, channelID, requesterID int64) *struct {
		code int
		body string
	} {
		rec := f.do(t, "POST", "/v1/approvals", tok, createApprovalBody(channelID, requesterID))
		return &struct {
			code int
			body string
		}{rec.Code, rec.Body.String()}
	}

	// The requester is the caller: omitted and matching ids both work.
	for _, requester := range []int64{0, f.alice.ID} {
		rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(f.alpha.ID, requester))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create with requester_id %d = %d %s", requester, rec.Code, rec.Body)
		}
		if got := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.RequesterID; got != f.alice.ID {
			t.Errorf("requester = %d, want the caller %d", got, f.alice.ID)
		}
	}
	// Naming anyone else is refused and audited, and creates nothing.
	before := len(openApprovals(t, f.srv))
	rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(f.alpha.ID, f.root.ID))
	assertErrorBody(t, rec, http.StatusForbidden, "requester_mismatch")
	if got := len(openApprovals(t, f.srv)); got != before {
		t.Fatalf("a refused create raised an approval")
	}
	var denied bool
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" && e.Actor == fmt.Sprintf("principal:%d", f.alice.ID) && e.Detail == "requester_mismatch" {
			denied = true
		}
	}
	if !denied {
		t.Error("the requester mismatch was not audited")
	}
	// The approval_created events name the authenticated caller as actor.
	for _, e := range f.audit(t) {
		if e.Action == store.AuditApprovalCreated && e.Actor != fmt.Sprintf("principal:%d", f.alice.ID) {
			t.Errorf("approval_created actor = %s, want the caller", e.Actor)
		}
	}

	// A non-member cannot raise an approval in a channel, and learns nothing:
	// the answer is the one an unknown channel gets.
	unknown := create(f.carolTok, 9999, 0)
	nonMember := create(f.carolTok, f.alpha.ID, 0)
	if nonMember.code != unknown.code || nonMember.body != unknown.body {
		t.Errorf("non-member create = %d %s, want the unknown-channel response %d %s", nonMember.code, nonMember.body, unknown.code, unknown.body)
	}
	// The operator has no exemption either: root is not a member of alpha.
	if got := create(f.rootTok, f.alpha.ID, 0); got.code != unknown.code || got.body != unknown.body {
		t.Errorf("non-member operator create = %d %s, want the unknown-channel response", got.code, got.body)
	}
	if got := len(openApprovals(t, f.srv)); got != before {
		t.Fatalf("a non-member raised an approval")
	}
}

func TestApprovalListShowsOnlyTheCallersChannels(t *testing.T) {
	f := newMemberFixture(t)
	mk := func(tok string, channelID int64) int64 {
		t.Helper()
		rec := f.do(t, "POST", "/v1/approvals", tok, createApprovalBody(channelID, 0))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", rec.Code, rec.Body)
		}
		return decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	}
	inAlpha := mk(f.aliceTok, f.alpha.ID)
	inGeneral := mk(f.rootTok, f.general.ID)

	tests := []struct {
		who  string
		tok  string
		want []int64
	}{
		{"member of both channels", f.aliceTok, []int64{inAlpha, inGeneral}},
		{"operator, member of general only", f.rootTok, []int64{inGeneral}},
		{"member of nothing", f.carolTok, nil},
	}
	for _, tt := range tests {
		t.Run(tt.who, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/approvals", tt.tok, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("list = %d %s", rec.Code, rec.Body)
			}
			var got []int64
			for _, a := range decodeBody[schema.ListApprovalsResponseV1](t, rec).Approvals {
				got = append(got, a.ID)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("approvals listed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCastDecisionIsBoundToCallerAndMembership(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)
	rec := f.do(t, "POST", "/v1/approvals", f.aliceTok, createApprovalBody(f.alpha.ID, 0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateApprovalResponseV1](t, rec).Approval.ID
	path := fmt.Sprintf("/v1/approvals/%d/decisions", id)
	stillPending := func(why string) {
		t.Helper()
		a, err := f.srv.store.ApprovalByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.State != schema.ApprovalStatePending {
			t.Fatalf("%s: approval state = %s, want still pending", why, a.State)
		}
	}

	// A decision cannot be cast in someone else's name.
	assertErrorBody(t, f.do(t, "POST", path, f.aliceTok, decisionBody(f.root.ID)), http.StatusForbidden, "principal_mismatch")
	stillPending("decision naming another principal")

	// ...including a disabled principal's (issue #101).
	if _, err := f.srv.store.DisablePrincipal(ctx, "system", f.carol.ID); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.do(t, "POST", path, f.aliceTok, decisionBody(f.carol.ID)), http.StatusForbidden, "principal_mismatch")
	stillPending("decision naming a disabled principal")
	if _, err := f.srv.store.EnablePrincipal(ctx, "system", f.carol.ID); err != nil {
		t.Fatal(err)
	}
	_, carolTok, err := f.srv.store.CreateCredential(ctx, "system", f.carol.ID, "again", nil)
	if err != nil {
		t.Fatal(err)
	}

	// A human who is not a member of the approval's channel sees exactly what
	// an unknown approval id looks like — for carol (member of nothing) and
	// for the operator (member of general only).
	unknown := f.do(t, "POST", "/v1/approvals/9999/decisions", carolTok, decisionBody(0))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown approval = %d %s", unknown.Code, unknown.Body)
	}
	for who, tok := range map[string]string{"non-member": carolTok, "non-member operator": f.rootTok} {
		rec := f.do(t, "POST", path, tok, decisionBody(0))
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
			t.Errorf("%s decision = %d %s, want the unknown-approval response %d %s", who, rec.Code, rec.Body, unknown.Code, unknown.Body)
		}
	}
	stillPending("decisions by non-members")

	// Without a credential there is no decision at all.
	assertErrorBody(t, f.do(t, "POST", path, "", decisionBody(f.alice.ID)), http.StatusUnauthorized, "unauthenticated")
	stillPending("unauthenticated decision")

	// The member decides as themselves, with principal_id omitted.
	rec = f.do(t, "POST", path, f.aliceTok, decisionBody(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("member decision = %d %s", rec.Code, rec.Body)
	}
	resp := decodeBody[schema.CastDecisionResponseV1](t, rec)
	if resp.Decision.PrincipalID != f.alice.ID || resp.State != schema.ApprovalStateResolved {
		t.Fatalf("decision = %+v state %s, want alice's and resolved", resp.Decision, resp.State)
	}
	var cast int
	for _, e := range f.audit(t) {
		if e.Action == store.AuditDecisionCast {
			cast++
			if e.Actor != fmt.Sprintf("principal:%d", f.alice.ID) {
				t.Errorf("decision_cast actor = %s, want the authenticated caller", e.Actor)
			}
		}
	}
	if cast != 1 {
		t.Errorf("decision_cast events = %d, want exactly 1", cast)
	}
}
