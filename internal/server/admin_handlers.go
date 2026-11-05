package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The admin door (issue #1326, slice S3 #1330, docs/design/ai-administrators.md
// §3.4, §3.5): three routes, humans and agents alike. The core functions take
// via ("rest" or "mcp") so the MCP tools (S4) and the REST routes are one path.

const (
	// viaREST is the door over REST; the MCP tools (S4) pass "mcp".
	viaREST = "rest"

	// adminDoorBodyMaxBytes bounds a submit body: the action, a target, a
	// reason and the route's params, which are each route's own small body.
	adminDoorBodyMaxBytes = 64 << 10

	defaultAdminApprovalTTL = 24 * time.Hour
	defaultAdminRunWindow   = 24 * time.Hour
	defaultAdminMaxPending  = 8
)

// adminRefusal is a refusal of the door itself, with the status it answers,
// the error body, and the access_denied detail to audit ("" writes none).
type adminRefusal struct {
	status int
	err    schema.Error
	audit  string
}

func (a *adminRefusal) Error() string { return a.err.Code + ": " + a.err.Message }

func doorRefuse(status int, code, message string) *adminRefusal {
	return &adminRefusal{status: status, err: schema.Error{Code: code, Message: message}}
}

// adminCaller is who is at the door: the principal, and for an agent its
// manifest and admin settings.
type adminCaller struct {
	p        store.Principal
	agent    bool
	manifest schema.AgentManifestV1
	admin    schema.AgentAdminV1
}

func (c adminCaller) actor() string { return fmt.Sprintf("principal:%d", c.p.ID) }

// adminDoorCaller loads the caller. An agent needs a manifest that grants
// admin.act and names an approval channel; anything else is refused as the
// agent's own refusals are (forbidden, audited under the door's subject). The
// budget is not spent here: a refused action spends nothing (design §3.8).
func (s *Server) adminDoorCaller(ctx context.Context, p store.Principal, subject string) (adminCaller, *adminRefusal) {
	c := adminCaller{p: p}
	if p.Kind != store.PrincipalAgent {
		return c, nil
	}
	c.agent = true
	m, allowed, err := s.agentSelfCheck(ctx, p.ID, subject, schema.CapabilityAdminAct)
	if err != nil {
		slog.ErrorContext(ctx, "admin door: load manifest failed", "principal", p.ID, "error", err)
		return c, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
	}
	if !allowed {
		return c, doorRefuse(http.StatusForbidden, errForbidden.Code, errForbidden.Message)
	}
	if m.Admin == nil {
		s.auditAgentDenial(ctx, p.ID, subject, schema.CapabilityAdminAct, 0, "no_admin_grant")
		return c, doorRefuse(http.StatusForbidden, errForbidden.Code, errForbidden.Message)
	}
	c.manifest, c.admin = m, *m.Admin
	return c, nil
}

// spend charges the agent's admin.act budget, last, after every check passed.
func (s *Server) adminSpend(ctx context.Context, c adminCaller, subject string) *adminRefusal {
	if !c.agent {
		return nil
	}
	if limited := s.spendAgentBudget(ctx, c.p.ID, subject, c.manifest, schema.CapabilityAdminAct); limited != nil {
		e := limited.schemaError()
		return &adminRefusal{status: http.StatusTooManyRequests, err: *e}
	}
	return nil
}

// gated reports whether entry raises an approval for c: an agent's gated
// action, by the catalog's default or its manifest's require_approval.
// A human at the door is an operator acting for themself: nothing is gated.
func (c adminCaller) gated(entry adminCatalogEntry) bool {
	if !c.agent {
		return false
	}
	return entry.gate == schema.AdminGateApproval || slices.Contains(c.admin.RequireApproval, entry.action)
}

// auditDoor writes one row of the door's trail; a failure is logged, not
// fatal, as access_denied's is.
func (s *Server) auditDoor(ctx context.Context, actor, action, subject, detail string) {
	if _, err := s.store.AppendAuditEvent(context.WithoutCancel(ctx), actor, action, subject, detail); err != nil {
		slog.ErrorContext(ctx, "admin door: audit failed", "action", action, "error", err)
	}
}

// auditDenied writes the access_denied row of a refusal, coalesced as every
// access_denied is (issue #80).
func (s *Server) auditDenied(ctx context.Context, caller store.Principal, subject, detail string) {
	write, count := s.denials.admit(caller.ID, subject+"|"+detail, s.now())
	if !write {
		return
	}
	s.auditDoor(ctx, fmt.Sprintf("principal:%d", caller.ID), "access_denied", subject, denialDetail(detail, count))
}

// submitAdminAction is POST /v1/admin/actions and submit_admin_action.
func (s *Server) submitAdminAction(ctx context.Context, p store.Principal, req schema.SubmitAdminActionRequestV1, via, subject string) (schema.AdminActionResultV1, int, *adminRefusal) {
	if !req.Action.Valid() {
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusBadRequest, schema.ErrorCodeAdminActionUnknown, fmt.Sprintf("%q is not an admin action; describe_admin_actions lists them", req.Action))
	}
	if err := req.Validate(); err != nil {
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusBadRequest, "invalid_request", err.Error())
	}
	entry, ok := adminCatalogByAction[req.Action]
	if !ok {
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusBadRequest, schema.ErrorCodeAdminActionUnknown, fmt.Sprintf("%q is not an admin action", req.Action))
	}
	c, refusal := s.adminDoorCaller(ctx, p, subject)
	if refusal != nil {
		return schema.AdminActionResultV1{}, 0, refusal
	}
	// The permission, asked in the door's context: the one place an agent's
	// grant is authority. Refused before anything is built, so a refused
	// action costs nothing and raises nothing (design §3.4 point 3).
	dctx := store.WithAdminDispatch(ctx)
	allowed, err := s.can(dctx, p, entry.permission)
	if err != nil {
		slog.ErrorContext(ctx, "admin door: permission check failed", "principal", p.ID, "error", err)
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
	}
	if !allowed {
		// Named to whoever could hold it: an operator, and an agent, which
		// an owner may grant it (design §3.6). A member is never told of an
		// operator permission (ADR-014 design §7.4): it gets the answer an
		// operator route gives it.
		named := refusedPermission(p, entry.permission)
		if c.agent {
			named = entry.permission
		}
		if named == "" {
			r := doorRefuse(http.StatusForbidden, "forbidden", "operator role required")
			r.audit = "forbidden"
			s.auditDenied(ctx, p, subject, r.audit)
			return schema.AdminActionResultV1{}, 0, r
		}
		r := doorRefuse(http.StatusForbidden, schema.ErrorCodePermissionDenied,
			fmt.Sprintf("%s needs the %s permission, which you do not hold on this server", entry.action, named))
		r.err.Permission = named
		r.audit = schema.ErrorCodePermissionDenied + " permission=" + string(named)
		s.auditDenied(ctx, p, subject, r.audit)
		return schema.AdminActionResultV1{}, 0, r
	}
	built, refusal := s.buildAdminRequest(dctx, c, entry, req.Target, req.Params)
	if refusal != nil {
		if refusal.audit != "" {
			s.auditDenied(ctx, p, subject, refusal.audit)
		}
		return schema.AdminActionResultV1{}, 0, refusal
	}
	// The budget is spent here, after every check of the door's own, and
	// before the approval's bounds (max_pending, the duplicate rule), which
	// are decided in the approval's transaction: a loop that keeps hitting
	// them is slowed by the budget too.
	if refusal := s.adminSpend(ctx, c, subject); refusal != nil {
		return schema.AdminActionResultV1{}, 0, refusal
	}
	if c.gated(entry) {
		return s.requestAdminAction(ctx, c, entry, built, req.Reason, via)
	}
	out, err := s.dispatch(ctx, entry, 0, built.req)
	if err != nil {
		slog.ErrorContext(ctx, "admin door: dispatch failed", "action", entry.action, "error", err)
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
	}
	s.auditRun(ctx, c, "action:"+string(entry.action), built.req.route(), out, via)
	return schema.AdminActionResultV1{Status: schema.AdminActionRan, HTTPStatus: out.status, Result: out.body}, http.StatusOK, nil
}

// auditRun writes admin_action_ran for a route that answered 2xx or 3xx and
// admin_action_refused, with the route's code, for one that refused.
func (s *Server) auditRun(ctx context.Context, c adminCaller, subject, route string, out dispatched, via string) {
	if out.status < 400 {
		s.auditDoor(ctx, c.actor(), store.AuditAdminActionRan, subject, fmt.Sprintf("route=%q status=%d via=%s", route, out.status, via))
		return
	}
	s.auditDoor(ctx, c.actor(), store.AuditAdminActionRefused, subject, fmt.Sprintf("reason=%s route=%q status=%d via=%s", out.code, route, out.status, via))
}

// builtAdminRequest is a built dispatch and what the decider is shown.
type builtAdminRequest struct {
	req adminRequest
	// target is the payload's target, with its name read now.
	target *schema.AdminTargetV1
	// params is the clamped body, as stored on a gated action's payload.
	params json.RawMessage
	// member is the second entity of a channel-member action, with its
	// name, for the decider's picture.
	member *schema.AdminTargetV1
}

// buildAdminRequest checks the target against the catalog, refuses what the
// agent may never do (itself as target, its approval channel), and builds the
// request with the catalog's clamps.
func (s *Server) buildAdminRequest(ctx context.Context, c adminCaller, entry adminCatalogEntry, target *schema.AdminTargetV1, params json.RawMessage) (builtAdminRequest, *adminRefusal) {
	switch {
	case entry.target == "" && target != nil:
		return builtAdminRequest{}, doorRefuse(http.StatusBadRequest, "invalid_request", fmt.Sprintf("%s takes no target", entry.action))
	case entry.target != "" && (target == nil || target.Kind != entry.target):
		return builtAdminRequest{}, doorRefuse(http.StatusBadRequest, "invalid_request", fmt.Sprintf("%s takes a target of kind %s", entry.action, entry.target))
	}
	var b builtAdminRequest
	if target != nil {
		name, err := s.store.AdminTargetName(ctx, *target)
		if errors.Is(err, store.ErrNotFound) {
			code := adminNotFoundCodes[target.Kind]
			return b, doorRefuse(http.StatusNotFound, code, strings.ReplaceAll(strings.TrimSuffix(code, "_not_found"), "_", " ")+" not found")
		}
		if err != nil {
			slog.ErrorContext(ctx, "admin door: read target failed", "error", err)
			return b, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
		}
		b.target = &schema.AdminTargetV1{Kind: target.Kind, ID: target.ID, Name: name}
	}
	in := adminBuildInput{caller: c, target: b.target, params: params}
	built, touches, refusal := entry.build(context.WithValue(ctx, adminBuildingKey{}, entry), s, in)
	if refusal != nil {
		return b, refusal
	}
	b.req, b.params = built, touches.params
	if b.target != nil && b.target.Kind == schema.AdminTargetChannel {
		b.req.channelID = b.target.ID
	}
	if touches.member != 0 {
		name, err := s.store.AdminTargetName(ctx, schema.AdminTargetV1{Kind: schema.AdminTargetPrincipal, ID: touches.member})
		if errors.Is(err, store.ErrNotFound) {
			return b, doorRefuse(http.StatusNotFound, "principal_not_found", "principal not found")
		}
		if err != nil {
			return b, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
		}
		b.member = &schema.AdminTargetV1{Kind: schema.AdminTargetPrincipal, ID: touches.member, Name: name}
	}
	if c.agent {
		// Self-administration goes through the manifest, by a person
		// (design §3.3): an action naming the agent itself is refused.
		self := touches.member == c.p.ID || (b.target != nil && b.target.Kind == schema.AdminTargetPrincipal && b.target.ID == c.p.ID)
		if !self && b.target != nil && b.target.Kind == schema.AdminTargetCredential {
			owner, err := s.store.CredentialPrincipal(ctx, b.target.ID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return b, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
			}
			self = owner == c.p.ID
		}
		if self {
			r := doorRefuse(http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed, "an AI administrator cannot act on itself; a person changes its manifest")
			r.audit = schema.ErrorCodeAdminActionNotAllowed + " reason=self"
			return b, r
		}
		// The channel that decides what the agent may do is the one it may
		// not shape (design §3.5.6).
		if slices.Contains(touches.channels, c.admin.ApprovalChannelID) {
			r := doorRefuse(http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed, "an AI administrator cannot change its own approval channel")
			r.audit = schema.ErrorCodeAdminActionNotAllowed + " reason=approval_channel_protected"
			return b, r
		}
	}
	return b, nil
}

// requestAdminAction raises a gated action's approval (design §3.5.1): the
// server composes its title, body and payload; the store writes the
// admin_actions row and admin_action_requested in the approval's
// transaction; the approvals manager notifies as for every approval.
func (s *Server) requestAdminAction(ctx context.Context, c adminCaller, entry adminCatalogEntry, b builtAdminRequest, reason, via string) (schema.AdminActionResultV1, int, *adminRefusal) {
	now := s.now().UTC().Truncate(time.Millisecond)
	action := schema.AdminActionV1{
		Action:      entry.action,
		Route:       b.req.route(),
		Target:      b.target,
		Params:      b.params,
		AgentID:     c.p.ID,
		Reason:      reason,
		RequestedAt: schema.NewTimestamp(now),
	}
	data, err := json.Marshal(action)
	if err != nil {
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
	}
	ttl := defaultAdminApprovalTTL
	if c.admin.ApprovalTTLSeconds > 0 {
		ttl = time.Duration(c.admin.ApprovalTTLSeconds) * time.Second
	}
	maxPending := defaultAdminMaxPending
	if c.admin.MaxPending > 0 {
		maxPending = c.admin.MaxPending
	}
	quorum := c.admin.Quorum
	if quorum == 0 {
		quorum = 1
	}
	targetDetail := ""
	if b.target != nil {
		targetDetail = fmt.Sprintf(" target=%s:%d", b.target.Kind, b.target.ID)
	}
	created, err := s.approvals.Create(ctx, store.ApprovalParams{
		RequesterID: c.p.ID,
		ChannelID:   c.admin.ApprovalChannelID,
		Title:       adminApprovalTitle(c.p, entry.action, b),
		Body:        adminApprovalBody(c.p, entry.action, b, reason, via, now, s.runWindow(c)),
		Payload:     &schema.Payload{Schema: schema.AdminActionV1Name, Data: data},
		Options: []schema.Option{
			{ID: "run", Label: "Run it", Kind: schema.OptionKindApprove},
			{ID: "refuse", Label: "Do not run", Kind: schema.OptionKindReject},
		},
		Deadline:   now.Add(ttl),
		Quorum:     quorum,
		Escalation: c.admin.EscalationTarget,
		AdminAction: &store.AdminActionRequest{
			Action:     action,
			MaxPending: maxPending,
			Detail:     fmt.Sprintf("action=%s route=%q%s via=%s", entry.action, b.req.route(), targetDetail, via),
		},
	})
	var pending *store.AdminActionPendingError
	switch {
	case errors.As(err, &pending):
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusConflict, schema.ErrorCodeAdminActionPending,
			fmt.Sprintf("the same action is already awaiting a decision as approval %d", pending.ApprovalID))
	case errors.Is(err, store.ErrTooManyPendingAdminActions):
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusTooManyRequests, schema.ErrorCodeTooManyPendingAdminActions,
			fmt.Sprintf("%d admin actions already await a decision; one must resolve first", maxPending))
	case errors.Is(err, store.ErrChannelArchived):
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusConflict, "channel_archived", "the approval channel is archived")
	case errors.Is(err, approvals.ErrInvalid):
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusBadRequest, "invalid_request", err.Error())
	case err != nil:
		slog.ErrorContext(ctx, "admin door: create approval failed", "error", err)
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
	}
	approval := created.ToSchema()
	deadline := approval.Deadline
	return schema.AdminActionResultV1{
		Status:     schema.AdminActionPendingApproval,
		ApprovalID: created.ID,
		Approval:   &approval,
		Deadline:   &deadline,
	}, http.StatusCreated, nil
}

func (s *Server) runWindow(c adminCaller) time.Duration {
	if c.admin.RunWindowSeconds > 0 {
		return time.Duration(c.admin.RunWindowSeconds) * time.Second
	}
	return defaultAdminRunWindow
}

// adminNotFoundCodes is each target kind's not-found code: the one its
// routes answer, so the door's refusal of an unknown target reads as theirs.
var adminNotFoundCodes = map[schema.AdminTargetKind]string{
	schema.AdminTargetPrincipal:  "principal_not_found",
	schema.AdminTargetChannel:    "channel_not_found",
	schema.AdminTargetCredential: "credential_not_found",
	schema.AdminTargetHook:       "hook_not_found",
	schema.AdminTargetInvite:     "invite_not_found",
	schema.AdminTargetReport:     "problem_report_not_found",
}

// adminVerb is the catalog action as words: "disable member".
func adminVerb(a schema.AdminAction) string { return strings.ReplaceAll(string(a), "_", " ") }

func describeTarget(t *schema.AdminTargetV1) string {
	if t.Name != "" {
		return fmt.Sprintf("%s #%d %q", t.Kind, t.ID, t.Name)
	}
	return fmt.Sprintf("%s #%d", t.Kind, t.ID)
}

// adminApprovalTitle: AI admin <agent>: disable member "alice" (#7).
func adminApprovalTitle(agent store.Principal, action schema.AdminAction, b builtAdminRequest) string {
	title := fmt.Sprintf("AI admin %s: %s", agent.Name, adminVerb(action))
	if b.target != nil {
		if b.target.Name != "" {
			title += fmt.Sprintf(" %q (#%d)", b.target.Name, b.target.ID)
		} else {
			title += fmt.Sprintf(" #%d", b.target.ID)
		}
	}
	if b.member != nil {
		title += fmt.Sprintf(", member %q (#%d)", b.member.Name, b.member.ID)
	}
	return title
}

// adminApprovalBody is the decider's picture (design §3.5.1): one line per
// fact, every one derived from the stored action but the reason, which is
// labelled as the agent's words. The reason has no control character
// (schema.SubmitAdminActionRequestV1.Validate), so it cannot add a line.
func adminApprovalBody(agent store.Principal, action schema.AdminAction, b builtAdminRequest, reason, via string, at time.Time, window time.Duration) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Requested by AI administrator %q (agent #%d) over %s at %s.\n", agent.Name, agent.ID, strings.ToUpper(via), at.UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "Action:     %s\n", action)
	fmt.Fprintf(&sb, "Will run:   %s\n", b.req.route())
	if b.target != nil {
		fmt.Fprintf(&sb, "Target:     %s\n", describeTarget(b.target))
	}
	if b.member != nil {
		fmt.Fprintf(&sb, "Member:     %s\n", describeTarget(b.member))
	}
	params := "{}"
	if len(b.params) > 0 {
		var compact bytes.Buffer
		if json.Compact(&compact, b.params) == nil {
			params = compact.String()
		}
	}
	fmt.Fprintf(&sb, "Parameters: %s\n", params)
	if reason != "" {
		fmt.Fprintf(&sb, "The agent's stated reason: %q\n", reason)
	}
	fmt.Fprintf(&sb, "If approved, conchd runs exactly this action once, as agent #%d, within %s of the decision. Nothing else runs.", agent.ID, window)
	return sb.String()
}

// runApprovedAction is POST /v1/admin/actions/{approval_id}/run and
// run_approved_action (design §3.5.2).
func (s *Server) runApprovedAction(ctx context.Context, p store.Principal, approvalID int64, via, subject string) (schema.AdminActionResultV1, int, *adminRefusal) {
	c, refusal := s.adminDoorCaller(ctx, p, subject)
	if refusal != nil {
		return schema.AdminActionResultV1{}, 0, refusal
	}
	notFound := doorRefuse(http.StatusNotFound, "approval_not_found", "approval not found")
	if !c.agent {
		// Only the agent that requested a gated action runs it.
		return schema.AdminActionResultV1{}, 0, notFound
	}
	subjectApproval := fmt.Sprintf("approval:%d", approvalID)
	action, err := s.store.ClaimAdminAction(ctx, approvalID, p.ID, s.runWindow(c), s.now())
	var code string
	var status int
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		return schema.AdminActionResultV1{}, 0, notFound
	case errors.Is(err, store.ErrApprovalNotAdminAction):
		code, status = schema.ErrorCodeApprovalNotAdminAction, http.StatusConflict
	case errors.Is(err, store.ErrApprovalNotApproved):
		code, status = schema.ErrorCodeApprovalNotApproved, http.StatusConflict
	case errors.Is(err, store.ErrApprovalRunWindowPassed):
		code, status = schema.ErrorCodeApprovalRunWindowPassed, http.StatusConflict
	case errors.Is(err, store.ErrApprovalAlreadyRun):
		code, status = schema.ErrorCodeApprovalAlreadyRun, http.StatusConflict
	case errors.Is(err, store.ErrTargetChanged):
		code, status = schema.ErrorCodeTargetChanged, http.StatusConflict
	default:
		slog.ErrorContext(ctx, "admin door: claim run failed", "approval", approvalID, "error", err)
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
	}
	if code != "" {
		s.auditDoor(ctx, c.actor(), store.AuditAdminActionRefused, subjectApproval, fmt.Sprintf("reason=%s via=%s", code, via))
		return schema.AdminActionResultV1{}, 0, doorRefuse(status, code, adminRunRefusalMessage(code))
	}
	entry, ok := adminCatalogByAction[action.Action]
	if !ok {
		return schema.AdminActionResultV1{}, 0, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
	}
	// The run rebuilds the request from the catalog and the stored target
	// and params; the stored route is display only. Everything is checked
	// again as at submit, so a grant revoked or a target protected since is
	// refused, by the door or by the route.
	b, refusal := s.buildAdminRequest(store.WithAdminDispatch(ctx), c, entry, action.Target, action.Params)
	var out dispatched
	switch {
	case refusal != nil:
		out = dispatched{status: refusal.status, code: refusal.err.Code}
		body, _ := json.Marshal(refusal.err)
		out.body = body
	default:
		out, err = s.dispatch(ctx, entry, approvalID, b.req)
		if err != nil {
			slog.ErrorContext(ctx, "admin door: dispatch failed", "action", entry.action, "error", err)
			out = dispatched{status: http.StatusInternalServerError, code: errInternal.Code, body: json.RawMessage(`null`)}
		}
	}
	if err := s.store.RecordAdminActionOutcome(context.WithoutCancel(ctx), approvalID, out.status, out.code, s.now()); err != nil {
		slog.ErrorContext(ctx, "admin door: record run failed", "approval", approvalID, "error", err)
	}
	s.auditRun(ctx, c, subjectApproval, action.Route, out, via)
	return schema.AdminActionResultV1{Status: schema.AdminActionRan, HTTPStatus: out.status, Result: out.body}, http.StatusOK, nil
}

func adminRunRefusalMessage(code string) string {
	switch code {
	case schema.ErrorCodeApprovalNotAdminAction:
		return "this approval was not raised by the admin door for you"
	case schema.ErrorCodeApprovalNotApproved:
		return "the approval was not approved: nothing runs"
	case schema.ErrorCodeApprovalRunWindowPassed:
		return "the approval is older than the run window: request the action again"
	case schema.ErrorCodeApprovalAlreadyRun:
		return "this approval's action was already run; request the action again"
	case schema.ErrorCodeTargetChanged:
		return "the target is not the one the decider saw: request the action again"
	}
	return code
}

// adminCatalogFor is GET /v1/admin/actions and describe_admin_actions.
func (s *Server) adminCatalogFor(ctx context.Context, p store.Principal, subject string) (schema.AdminCatalogResponseV1, *adminRefusal) {
	c, refusal := s.adminDoorCaller(ctx, p, subject)
	if refusal != nil {
		return schema.AdminCatalogResponseV1{}, refusal
	}
	dctx := store.WithAdminDispatch(ctx)
	out := schema.AdminCatalogResponseV1{Actions: make([]schema.AdminCatalogEntryV1, 0, len(adminCatalog))}
	holds := map[schema.Permission]bool{}
	for _, e := range adminCatalog {
		held, asked := holds[e.permission]
		if !asked {
			var err error
			if held, err = s.can(dctx, p, e.permission); err != nil {
				slog.ErrorContext(ctx, "admin door: permission check failed", "error", err)
				return schema.AdminCatalogResponseV1{}, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
			}
			holds[e.permission] = held
		}
		out.Actions = append(out.Actions, schema.AdminCatalogEntryV1{
			Action: e.action, Route: e.pattern, Permission: e.permission, Gate: e.gate,
			Held: held, Gated: e.gate == schema.AdminGateApproval || c.gated(e),
		})
	}
	if c.agent {
		admin := c.admin
		if admin.ApprovalTTLSeconds == 0 {
			admin.ApprovalTTLSeconds = int64(defaultAdminApprovalTTL / time.Second)
		}
		if admin.RunWindowSeconds == 0 {
			admin.RunWindowSeconds = int64(defaultAdminRunWindow / time.Second)
		}
		if admin.Quorum == 0 {
			admin.Quorum = 1
		}
		if admin.MaxPending == 0 {
			admin.MaxPending = defaultAdminMaxPending
		}
		out.Admin = &admin
	}
	return out, nil
}

func writeAdminRefusal(w http.ResponseWriter, r *adminRefusal) {
	if r.status == http.StatusTooManyRequests && r.err.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(r.err.RetryAfterSeconds))
	}
	writeJSON(w, r.status, r.err)
}

func (s *Server) handleAdminCatalog(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return
	}
	out, refusal := s.adminCatalogFor(r.Context(), caller, r.Pattern)
	if refusal != nil {
		writeAdminRefusal(w, refusal)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSubmitAdminAction(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adminDoorBodyMaxBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
		return
	}
	var req schema.SubmitAdminActionRequestV1
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a SubmitAdminActionRequestV1")
		return
	}
	out, status, refusal := s.submitAdminAction(r.Context(), caller, req, viaREST, r.Pattern)
	if refusal != nil {
		writeAdminRefusal(w, refusal)
		return
	}
	writeJSON(w, status, out)
}

func (s *Server) handleRunAdminAction(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return
	}
	id, ok := pathID(w, r, "approval_id", "approval")
	if !ok {
		return
	}
	out, status, refusal := s.runApprovedAction(r.Context(), caller, id, viaREST, r.Pattern)
	if refusal != nil {
		writeAdminRefusal(w, refusal)
		return
	}
	writeJSON(w, status, out)
}
