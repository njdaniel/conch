package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

// aiAdminPath is the AI-administrator loop (issue #1335, docs/design/
// ai-administrators.md): an owner sets up an AI administrator with
// `conch admin ai-admin create`; the agent connects over MCP, reads the
// catalog, submits disable_member and gets pending_approval; ntfy fires; a
// human approves with `conch approve --option run`; the agent runs the
// approved action once; the member is disabled; and the audit chain shows
// the whole of it. It also asserts the refusals around that: an agent can
// never decide an approval, running before approval is refused, a second
// run is refused, and revoking the AI administrator ends its access.
func aiAdminPath(bin binaries) error {
	ntfy := newFakeNtfy()
	defer ntfy.Close()
	proc, err := startConchd(bin, ntfy.srv.URL, false)
	if err != nil {
		return err
	}
	defer func() { _ = proc.Stop() }()
	op := proc.operator()
	ctx := context.Background()

	// A decider and the member the agent will be asked about. The decider is
	// added to the approval channel once the setup has made it.
	_, memberToken, err := newHuman(op, "dogfood-member", "")
	if err != nil {
		return err
	}
	memberID, err := whoami(proc.as(memberToken))
	if err != nil {
		return err
	}

	// 1. The owner makes an AI administrator, with the real CLI, signed in
	// by the operator's credential. The MCP entry is on stdout, alone.
	entry, err := conchOut(bin.conch, proc.baseURL, proc.operatorToken, "admin", "ai-admin", "create", "dogfood-admin")
	if err != nil {
		return fmt.Errorf("conch admin ai-admin create: %w", err)
	}
	var mcpEntry struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(entry, &mcpEntry); err != nil || len(mcpEntry.MCPServers) != 1 {
		// The count, not the bytes: the entry carries the bearer token.
		return fmt.Errorf("ai-admin create stdout is not one MCP entry (%d entries, err %v)", len(mcpEntry.MCPServers), err)
	}
	var agentToken string
	for _, s := range mcpEntry.MCPServers {
		agentToken = strings.TrimPrefix(s.Headers["Authorization"], "Bearer ")
	}
	if agentToken == "" {
		return fmt.Errorf("the MCP entry carries no bearer token: %q", entry)
	}
	agentID, err := whoami(proc.as(agentToken))
	if err != nil {
		return fmt.Errorf("the entry's credential: %w", err)
	}
	_, deciderToken, err := newHuman(op, "dogfood-decider", "admin")
	if err != nil {
		return err
	}
	fmt.Println("ok  an owner made an AI administrator (conch admin ai-admin create)")

	// 2. The agent connects and reads the catalog.
	client := mcpclient.New(proc.baseURL, agentToken)
	if err := client.Initialize(ctx, "dogfood-check"); err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	raw, err := client.CallTool(ctx, "describe_admin_actions", map[string]any{})
	if err != nil {
		return fmt.Errorf("describe_admin_actions: %w", err)
	}
	catalog, err := mcpclient.Decode[schema.AdminCatalogResponseV1](raw)
	if err != nil {
		return err
	}
	var disable *schema.AdminCatalogEntryV1
	for i, e := range catalog.Actions {
		if e.Action == schema.AdminActionDisableMember {
			disable = &catalog.Actions[i]
		}
		if (e.Permission == schema.PermChangeServerSettings || e.Permission == schema.PermReadAuditLog) && e.Held {
			return fmt.Errorf("catalog: %s is held, but the powerful permissions are never granted unless named", e.Permission)
		}
	}
	if disable == nil || !disable.Held || !disable.Gated {
		return fmt.Errorf("catalog entry for disable_member = %+v, want held and gated", disable)
	}
	if catalog.Admin == nil || catalog.Admin.ApprovalChannelID == 0 {
		return fmt.Errorf("catalog admin settings = %+v, want an approval channel", catalog.Admin)
	}
	fmt.Println("ok  describe_admin_actions: disable_member is held and gated")

	// Direct REST use of an administrative route is refused to an agent: the
	// grant is authority only through the door.
	if err := proc.as(agentToken).expectStatus("the agent disabling a member over REST", http.MethodPost,
		fmt.Sprintf("/v1/principals/%d/disable", memberID), nil, nil, http.StatusForbidden, ""); err != nil {
		return err
	}

	// 3. A gated action: pending_approval, and the member is untouched.
	submit := map[string]any{
		"action": string(schema.AdminActionDisableMember),
		"target": map[string]any{"kind": string(schema.AdminTargetPrincipal), "id": memberID},
		"reason": "dogfood: the account is abandoned",
	}
	raw, err = client.CallTool(ctx, "submit_admin_action", submit)
	if err != nil {
		return fmt.Errorf("submit_admin_action: %w", err)
	}
	pending, err := mcpclient.Decode[schema.AdminActionResultV1](raw)
	if err != nil {
		return err
	}
	if pending.Status != schema.AdminActionPendingApproval || pending.ApprovalID == 0 {
		return fmt.Errorf("submit_admin_action = %+v, want pending_approval with an approval id", pending)
	}
	id := pending.ApprovalID
	if err := proc.as(memberToken).call(http.MethodGet, "/v1/whoami", nil, nil); err != nil {
		return fmt.Errorf("the member is touched before any approval: %w", err)
	}
	// A repeat of the same request while it is open names the existing one.
	if err := expectToolError(client, "submit_admin_action", submit, schema.ErrorCodeAdminActionPending); err != nil {
		return err
	}
	// Running before the approval resolves is refused.
	if err := expectToolError(client, "run_approved_action", map[string]any{"approval_id": id}, schema.ErrorCodeApprovalNotApproved); err != nil {
		return err
	}
	raw, err = client.CallTool(ctx, "check_decision", map[string]any{"approval_id": id})
	if err != nil {
		return fmt.Errorf("check_decision: %w", err)
	}
	if open, err := mcpclient.Decode[schema.CheckDecisionOutput](raw); err != nil || open.State != schema.ApprovalStatePending {
		return fmt.Errorf("check_decision before the decision = %+v (%v), want pending", open, err)
	}
	fmt.Println("ok  submit_admin_action disable_member answered pending_approval; running early is refused")

	// 4. ntfy fires, for the approval the server raised.
	if err := waitNtfyTitle(ntfy, strconv.FormatInt(id, 10)); err != nil {
		// The title is the server's wording; any delivery will do if it
		// does not carry the id.
		if len(ntfy.Hits()) == 0 {
			return fmt.Errorf("no ntfy notification for the admin approval: %w", err)
		}
	}
	fmt.Println("ok  ntfy fired for the admin approval")

	// An agent can never decide an approval, its own included.
	if err := proc.as(agentToken).expectStatus("the agent deciding its own approval", http.MethodPost,
		fmt.Sprintf("/v1/approvals/%d/decisions", id),
		schema.CastDecisionRequestV1{PrincipalID: agentID, OptionID: "run", Reason: "self"}, nil, http.StatusForbidden, "forbidden"); err != nil {
		return err
	}

	// 5. The owner-side human approves with the real CLI.
	human, err := newConchUser(bin.conch, proc.baseURL)
	if err != nil {
		return err
	}
	defer human.cleanup()
	if _, err := human.run(deciderToken+"\n", "login"); err != nil {
		return fmt.Errorf("conch login: %w", err)
	}
	listOut, err := human.run("", "approvals", "list")
	if err != nil {
		return fmt.Errorf("conch approvals list: %w", err)
	}
	if !strings.Contains(listOut, strconv.FormatInt(id, 10)) {
		return fmt.Errorf("conch approvals list does not show approval %d:\n%s", id, listOut)
	}
	if _, err := human.run("", "approve", "--option", "run", "--reason", "dogfood", strconv.FormatInt(id, 10)); err != nil {
		return fmt.Errorf("conch approve --option run: %w", err)
	}
	fmt.Println("ok  a human approved with conch approve --option run")

	// 6. The agent sees the decision and runs the action, once.
	raw, err = client.CallTool(ctx, "await_decision", map[string]any{"approval_id": id, "timeout_ms": 10000})
	if err != nil {
		return fmt.Errorf("await_decision: %w", err)
	}
	decided, err := mcpclient.Decode[schema.AwaitDecisionOutput](raw)
	if err != nil {
		return err
	}
	if decided.State != schema.ApprovalStateResolved || decided.Resolution == nil || decided.Resolution.Outcome != schema.OutcomeApproved {
		return fmt.Errorf("await_decision = %+v, want resolved and approved", decided)
	}
	raw, err = client.CallTool(ctx, "run_approved_action", map[string]any{"approval_id": id})
	if err != nil {
		return fmt.Errorf("run_approved_action: %w", err)
	}
	ran, err := mcpclient.Decode[schema.AdminActionResultV1](raw)
	if err != nil {
		return err
	}
	if ran.Status != schema.AdminActionRan || ran.HTTPStatus != http.StatusOK {
		return fmt.Errorf("run_approved_action = %+v, want ran with 200", ran)
	}
	if err := expectToolError(client, "run_approved_action", map[string]any{"approval_id": id}, schema.ErrorCodeApprovalAlreadyRun); err != nil {
		return err
	}
	fmt.Println("ok  run_approved_action ran the action once; a second run is refused")

	// 7. The member is disabled: its credential no longer works.
	if err := proc.as(memberToken).expectStatus("the disabled member's credential", http.MethodGet, "/v1/whoami", nil, nil, http.StatusUnauthorized, ""); err != nil {
		return err
	}
	fmt.Println("ok  the member is disabled")

	// 8. The audit chain.
	if err := assertAdminAuditChain(proc, id, memberID); err != nil {
		return err
	}
	fmt.Println("ok  audit: approval_created, admin_action_requested, decision_cast, approval_resolved, principal_disabled, admin_action_ran")

	// Revoking the AI administrator ends its access at once.
	if _, err := conchOut(bin.conch, proc.baseURL, proc.operatorToken, "admin", "ai-admin", "revoke", strconv.FormatInt(agentID, 10)); err != nil {
		return fmt.Errorf("conch admin ai-admin revoke: %w", err)
	}
	if _, err := client.CallTool(ctx, "describe_admin_actions", map[string]any{}); err == nil {
		return fmt.Errorf("the revoked AI administrator can still call describe_admin_actions")
	}
	fmt.Println("ok  conch admin ai-admin revoke ended the agent's access")
	return nil
}

// whoami is the principal id a credential belongs to.
func whoami(a api) (int64, error) {
	var who schema.WhoAmIResponseV1
	if err := a.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return 0, err
	}
	return who.ID, nil
}

// conchOut runs one conch subcommand signed in by token and returns its
// standard output alone; standard error goes to this program's.
func conchOut(binPath, server, token string, args ...string) ([]byte, error) {
	cmd := exec.Command(binPath, args...) // #nosec G204 -- binPath is this program's own just-built conch binary; args are local constants
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CONCH_") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "CONCH_SERVER="+server, "CONCH_TOKEN="+token)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// assertAdminAuditChain asserts the order of the rows the loop leaves: the
// approval's own, the request and the run the agent made, and the member's
// disabling, in that order.
func assertAdminAuditChain(proc *conchdProc, approvalID, memberID int64) error {
	events, err := proc.auditEvents()
	if err != nil {
		return fmt.Errorf("read audit events: %w", err)
	}
	want := []string{"approval_created", "admin_action_requested", "decision_cast", "approval_resolved", "principal_disabled", "admin_action_ran"}
	wanted := map[string]bool{}
	for _, a := range want {
		wanted[a] = true
	}
	subjects := map[string]bool{
		fmt.Sprintf("approval:%d", approvalID): true,
		fmt.Sprintf("principal:%d", memberID):  true,
	}
	var got []string
	for _, e := range events {
		if subjects[e.Subject] && wanted[e.Action] {
			got = append(got, e.Action)
		}
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("audit chain = %v, want %v", got, want)
	}
	return nil
}
