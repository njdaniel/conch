package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// archivedPath is the approval chain across an archive and a restore (issue
// #1296), against real binaries:
//  1. the agent requests in a live channel, and ntfy is told;
//  2. the operator archives the channel;
//  3. a request over MCP and one over REST are refused with channel_archived,
//     and so is starting a run: nothing is stored, notified or audited;
//  4. a human decides the first approval while the agent blocks on
//     await_decision, which wakes with the resolution,
//     and its audit chain is complete;
//  5. the operator restores the channel, and a new request runs the chain.
func archivedPath(bin binaries) error {
	ntfy := newFakeNtfy()
	defer ntfy.Close()
	proc, err := startConchd(bin, ntfy.srv.URL, false)
	if err != nil {
		return err
	}
	defer func() { _ = proc.Stop() }()
	op := proc.operator()

	channelID, err := createChannel(op, "archived-ops")
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	agentID, err := createPrincipal(op, schema.PrincipalAgent, "archive-agent")
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	_, agentToken, err := issueCredential(op, agentID, "dogfood")
	if err != nil {
		return fmt.Errorf("issue agent credential: %w", err)
	}
	if err := provisionAgent(op, agentID, "archive-agent", "archived-ops", channelID); err != nil {
		return err
	}
	humanID, humanToken, err := newHuman(op, "dogfood-human", "archived-ops")
	if err != nil {
		return err
	}
	human := proc.as(humanToken)
	client := mcpclient.New(proc.baseURL, agentToken)
	if err := client.Initialize(context.Background(), "dogfood-check"); err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	request := func(title string) (int64, error) {
		raw, err := client.CallTool(context.Background(), "request_approval", archivedApprovalArgs(channelID, title))
		if err != nil {
			return 0, fmt.Errorf("request_approval %q: %w", title, err)
		}
		created, err := mcpclient.Decode[schema.RequestApprovalOutput](raw)
		if err != nil {
			return 0, err
		}
		return created.ID, nil
	}
	// decide has the agent block on await_decision first, then the human
	// decides: the long poll must wake with the resolution, archived or not.
	decide := func(id int64) error {
		type awaited struct {
			out schema.AwaitDecisionOutput
			err error
		}
		done := make(chan awaited, 1)
		go func() {
			raw, err := client.CallTool(context.Background(), "await_decision", map[string]any{"approval_id": id, "timeout_ms": 30000})
			if err != nil {
				done <- awaited{err: err}
				return
			}
			out, err := mcpclient.Decode[schema.AwaitDecisionOutput](raw)
			done <- awaited{out: out, err: err}
		}()
		time.Sleep(200 * time.Millisecond) // let the poll start before the decision
		if err := human.call(http.MethodPost, fmt.Sprintf("/v1/approvals/%d/decisions", id),
			schema.CastDecisionRequestV1{PrincipalID: humanID, OptionID: "approve", Reason: "fine"}, nil); err != nil {
			return fmt.Errorf("decide approval %d: %w", id, err)
		}
		var got awaited
		select {
		case got = <-done:
		case <-time.After(40 * time.Second):
			return fmt.Errorf("await_decision %d never returned", id)
		}
		if got.err != nil {
			return fmt.Errorf("await_decision %d: %w", id, got.err)
		}
		if got.out.State != schema.ApprovalStateResolved || got.out.Resolution == nil || got.out.Resolution.Outcome != schema.OutcomeApproved {
			return fmt.Errorf("await_decision %d = %+v, want resolved and approved", id, got.out)
		}
		return nil
	}
	settings := func(archived bool) error {
		return op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", channelID), map[string]any{"archived": archived}, nil)
	}

	// 1. Live: requested and notified.
	first, err := request("Before the archive")
	if err != nil {
		return err
	}
	if err := waitNtfyTitle(ntfy, "Before the archive"); err != nil {
		return err
	}
	fmt.Println("ok  an approval requested in a live channel is notified")

	// 2. Archived.
	if err := settings(true); err != nil {
		return fmt.Errorf("archive: %w", err)
	}

	// 3. Refused over MCP and REST, a run too, and nothing written.
	if err := expectToolError(client, "request_approval", archivedApprovalArgs(channelID, "Refused over MCP"), schema.ErrorCodeChannelArchived); err != nil {
		return fmt.Errorf("archived channel: %w", err)
	}
	restRequest := archivedApprovalArgs(channelID, "Refused over REST")
	if err := human.expectStatus("REST approval in an archived channel", http.MethodPost, "/v1/approvals", restRequest, nil, http.StatusConflict, schema.ErrorCodeChannelArchived); err != nil {
		return err
	}
	var session schema.SessionV1
	if err := proc.as(agentToken).call(http.MethodPost, "/v1/sessions", map[string]any{"client": "dogfood", "runtime": "go", "model": "none", "host": "local"}, &session); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	if err := proc.as(agentToken).expectStatus("run in an archived channel", http.MethodPost, fmt.Sprintf("/v1/sessions/%d/runs", session.ID),
		map[string]any{"channel": "archived-ops", "task": "too late"}, nil, http.StatusConflict, schema.ErrorCodeChannelArchived); err != nil {
		return err
	}
	fmt.Println("ok  an archived channel refuses new approvals (MCP and REST) and new runs with channel_archived")

	// 4. The approval opened before the archive is decided as usual.
	if err := decide(first); err != nil {
		return fmt.Errorf("in-flight approval: %w", err)
	}
	fmt.Println("ok  the approval opened before the archive is decided and read back")

	// 5. Restored: the whole chain again.
	if err := settings(false); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	third, err := request("After the restore")
	if err != nil {
		return err
	}
	if err := waitNtfyTitle(ntfy, "After the restore"); err != nil {
		return err
	}
	if err := decide(third); err != nil {
		return fmt.Errorf("after restore: %w", err)
	}
	fmt.Println("ok  after restore an approval runs request, notify and resolve")

	// Nothing of the refusals reached ntfy or the log; both live approvals
	// carry the whole chain. Read once both resolutions are delivered, so no
	// late notification is missed.
	if err := waitNtfyCount(ntfy, "Approval resolved", 2); err != nil {
		return err
	}
	for _, title := range []string{"Refused over MCP", "Refused over REST"} {
		if hits := ntfyMentioning(ntfy, title); len(hits) != 0 {
			return fmt.Errorf("a refused request was notified: %+v", hits)
		}
	}
	events, err := proc.auditEvents()
	if err != nil {
		return err
	}
	for _, e := range events {
		if strings.Contains(e.Detail, "Refused over") {
			return fmt.Errorf("a refused approval request was audited: %+v", e)
		}
		// This path starts no run: its only attempt was refused.
		if e.Action == store.AuditRunStarted {
			return fmt.Errorf("the refused run start was audited: %+v", e)
		}
	}
	for _, id := range []int64{first, third} {
		if err := archivedChainComplete(events, id, agentID, humanID); err != nil {
			return err
		}
	}
	fmt.Println("ok  refusals left no notification and no audit row; both live approvals have their whole audit chain")
	return nil
}

// archivedApprovalArgs is one approval request in channelID, as MCP arguments
// and as the REST body alike.
func archivedApprovalArgs(channelID int64, title string) map[string]any {
	return map[string]any{
		"channel_id": channelID, "title": title, "body": "issue #1296",
		"options": []map[string]any{
			{"id": "approve", "kind": "approve", "label": "Approve"},
			{"id": "reject", "kind": "reject", "label": "Reject"},
		},
		"deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"quorum":   1,
	}
}

// archivedChainComplete checks approval id's chain: created by the agent,
// the human's decision and the resolution, in that order, and a notify row for
// the request and for the resolution. Notify rows are written after their
// transition, not in its transaction (approval-object.md §6), so only their
// number is checked, not where they fall.
func archivedChainComplete(events []store.AuditEvent, id, agentID, humanID int64) error {
	subject := fmt.Sprintf("approval:%d", id)
	var got []string
	notified := 0
	for _, e := range events {
		if e.Subject != subject {
			continue
		}
		switch e.Action {
		case store.AuditApprovalCreated, store.AuditDecisionCast, store.AuditApprovalResolved:
			got = append(got, e.Action+" "+e.Actor)
		case approvals.AuditNotifySent:
			notified++
		}
	}
	want := []string{
		store.AuditApprovalCreated + fmt.Sprintf(" principal:%d", agentID),
		store.AuditDecisionCast + fmt.Sprintf(" principal:%d", humanID),
		store.AuditApprovalResolved + " system",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		return fmt.Errorf("approval %d audit chain = %q, want %q", id, got, want)
	}
	if notified != 2 {
		return fmt.Errorf("approval %d has %d notify_sent rows, want 2 (requested, resolved)", id, notified)
	}
	return nil
}

// ntfyMentioning is every delivery whose title or body contains s.
func ntfyMentioning(n *fakeNtfy, s string) []ntfyHit {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []ntfyHit
	for _, h := range n.hits {
		if strings.Contains(h.Title, s) || strings.Contains(h.Body, s) {
			out = append(out, h)
		}
	}
	return out
}

// waitNtfyTitle waits until a delivery mentions s.
func waitNtfyTitle(n *fakeNtfy, s string) error {
	return waitNtfyCount(n, s, 1)
}

// waitNtfyCount waits until at least want deliveries mention s.
func waitNtfyCount(n *fakeNtfy, s string, want int) error {
	deadline := time.Now().Add(10 * time.Second)
	for len(ntfyMentioning(n, s)) < want {
		if time.Now().After(deadline) {
			return fmt.Errorf("ntfy: %d deliveries mention %q within 10s, want %d", len(ntfyMentioning(n, s)), s, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}
