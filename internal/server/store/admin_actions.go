package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The store half of the admin door (issue #1326, slice S3 #1330,
// docs/design/ai-administrators.md §3.5): the row that proves a gated
// action's approval came from the door, the bounds on what an agent may have
// pending, and the one-shot claim of the run.

// Audit actions of the admin door (design §3.7).
const (
	AuditAdminActionRequested = "admin_action_requested"
	AuditAdminActionRan       = "admin_action_ran"
	AuditAdminActionRefused   = "admin_action_refused"
)

var (
	// ErrTooManyPendingAdminActions: the agent already has its max_pending
	// gated actions awaiting a decision (design §3.8).
	ErrTooManyPendingAdminActions = errors.New("store: too many pending admin actions")
	// ErrApprovalNotAdminAction: the approval was not raised by the admin
	// door for this agent (no admin_actions row, or another agent's, or a
	// payload that is not the door's).
	ErrApprovalNotAdminAction = errors.New("store: approval is not an admin action")
	// ErrApprovalNotApproved: the approval is pending, escalated, rejected,
	// custom or expired: nothing runs.
	ErrApprovalNotApproved = errors.New("store: approval was not approved")
	// ErrApprovalRunWindowPassed: approved, but longer ago than the run
	// window.
	ErrApprovalRunWindowPassed = errors.New("store: approval run window passed")
	// ErrApprovalAlreadyRun: the run was claimed before, whatever it did.
	ErrApprovalAlreadyRun = errors.New("store: approval already run")
	// ErrTargetChanged: the target's name is not the one the decider saw.
	ErrTargetChanged = errors.New("store: admin action target changed")
	// ErrAdminPayloadReserved: an approval carrying conch.admin_action.v1
	// that the admin door did not raise, or a door request without it.
	ErrAdminPayloadReserved = errors.New("store: the admin action payload is the admin door's")
)

// AdminActionPendingError is the same action, on the same target with the
// same params, already awaiting a decision: ApprovalID names it.
type AdminActionPendingError struct{ ApprovalID int64 }

func (e *AdminActionPendingError) Error() string {
	return fmt.Sprintf("store: admin action already pending as approval %d", e.ApprovalID)
}

// AdminActionRequest makes CreateApproval an admin door's approval: in the
// same transaction it bounds the agent's pending actions, refuses a
// duplicate, writes the admin_actions row the run proves origin by, and
// audits admin_action_requested after approval_created (design §3.5.1,
// §3.8). Payload must be the door's composed conch.admin_action.v1.
type AdminActionRequest struct {
	Action     schema.AdminActionV1
	MaxPending int
	// Detail is the admin_action_requested row's detail.
	Detail string
}

// adminActionKey is what makes two gated actions the same for the duplicate
// rule: the action, the target and the params, compacted.
func adminActionKey(a schema.AdminActionV1) string {
	var params bytes.Buffer
	if len(a.Params) > 0 {
		if err := json.Compact(&params, a.Params); err != nil {
			params.Write(a.Params)
		}
	}
	target := ""
	if a.Target != nil {
		target = fmt.Sprintf("%s:%d", a.Target.Kind, a.Target.ID)
	}
	return fmt.Sprintf("%s|%s|%s", a.Action, target, params.String())
}

// checkAdminActionBoundsTx runs inside CreateApproval's transaction before
// the approval is inserted; insertAdminActionTx after it.
func checkAdminActionBoundsTx(ctx context.Context, tx execer, req *AdminActionRequest) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT a.id, a.payload_json FROM admin_actions x JOIN approvals a ON a.id = x.approval_id
		 WHERE x.agent_id = ? AND a.state IN ('pending', 'escalated') ORDER BY a.id`, req.Action.AgentID)
	if err != nil {
		return fmt.Errorf("store: read pending admin actions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	want := adminActionKey(req.Action)
	pending := 0
	for rows.Next() {
		var id int64
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return fmt.Errorf("store: read pending admin actions: %w", err)
		}
		pending++
		var a schema.AdminActionV1
		if json.Unmarshal([]byte(payload), &a) == nil && adminActionKey(a) == want {
			return &AdminActionPendingError{ApprovalID: id}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read pending admin actions: %w", err)
	}
	if req.MaxPending > 0 && pending >= req.MaxPending {
		return ErrTooManyPendingAdminActions
	}
	return nil
}

func insertAdminActionTx(ctx context.Context, tx execer, approvalID int64, req *AdminActionRequest, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO admin_actions (approval_id, agent_id, action, requested_at) VALUES (?, ?, ?, ?)",
		approvalID, req.Action.AgentID, string(req.Action.Action), now.UnixMilli()); err != nil {
		return fmt.Errorf("store: insert admin action: %w", err)
	}
	return appendAuditEventTx(ctx, tx, principalActor(req.Action.AgentID), AuditAdminActionRequested, approvalSubject(approvalID), req.Detail, now)
}

// ClaimAdminAction is the run's check and claim (design §3.5.2), in one
// IMMEDIATE transaction, in this order: the approval exists and agentID
// requested it (ErrNotFound otherwise, the uniform answer); the door wrote
// its admin_actions row for agentID and its payload is the door's for
// agentID (ErrApprovalNotAdminAction); it resolved approved
// (ErrApprovalNotApproved); within runWindow of the resolution
// (ErrApprovalRunWindowPassed); never claimed (ErrApprovalAlreadyRun); the
// target still has the name the decider saw (ErrTargetChanged). Then it sets
// claimed_at and returns the stored action, which is all the dispatcher runs.
// SQLite's single writer lets exactly one claim of an approval succeed.
func (s *Store) ClaimAdminAction(ctx context.Context, approvalID, agentID int64, runWindow time.Duration, now time.Time) (schema.AdminActionV1, error) {
	now = now.Truncate(time.Millisecond)
	var action schema.AdminActionV1
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var requester int64
		var payloadSchema, payloadJSON string
		switch err := tx.QueryRowContext(ctx,
			"SELECT requester_id, payload_schema, payload_json FROM approvals WHERE id = ?", approvalID).Scan(&requester, &payloadSchema, &payloadJSON); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: claim admin action: read approval: %w", err)
		}
		if requester != agentID {
			return ErrNotFound
		}
		var rowAgent int64
		var claimed sql.NullInt64
		switch err := tx.QueryRowContext(ctx,
			"SELECT agent_id, claimed_at FROM admin_actions WHERE approval_id = ?", approvalID).Scan(&rowAgent, &claimed); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrApprovalNotAdminAction
		case err != nil:
			return fmt.Errorf("store: claim admin action: read row: %w", err)
		}
		if rowAgent != agentID || payloadSchema != schema.AdminActionV1Name {
			return ErrApprovalNotAdminAction
		}
		if err := json.Unmarshal([]byte(payloadJSON), &action); err != nil || action.Validate() != nil || action.AgentID != agentID {
			return ErrApprovalNotAdminAction
		}
		var outcome string
		var resolvedAt int64
		switch err := tx.QueryRowContext(ctx,
			"SELECT outcome, resolved_at FROM approval_resolutions WHERE approval_id = ?", approvalID).Scan(&outcome, &resolvedAt); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrApprovalNotApproved
		case err != nil:
			return fmt.Errorf("store: claim admin action: read resolution: %w", err)
		}
		if schema.Outcome(outcome) != schema.OutcomeApproved {
			return ErrApprovalNotApproved
		}
		if now.After(time.UnixMilli(resolvedAt).Add(runWindow)) {
			return ErrApprovalRunWindowPassed
		}
		if claimed.Valid {
			return ErrApprovalAlreadyRun
		}
		if action.Target != nil && action.Target.Name != "" {
			name, err := adminTargetNameTx(ctx, tx, *action.Target)
			if err != nil {
				return err
			}
			if name != action.Target.Name {
				return ErrTargetChanged
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE admin_actions SET claimed_at = ? WHERE approval_id = ? AND claimed_at IS NULL", now.UnixMilli(), approvalID); err != nil {
			return fmt.Errorf("store: claim admin action: %w", err)
		}
		return nil
	})
	if err != nil {
		return schema.AdminActionV1{}, err
	}
	return action, nil
}

// RecordAdminActionOutcome writes the claimed run's outcome on its row. The
// claim is never released: a failed dispatch needs a new approval.
func (s *Store) RecordAdminActionOutcome(ctx context.Context, approvalID int64, httpStatus int, errorCode string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE admin_actions SET ran_at = ?, http_status = ?, error_code = ? WHERE approval_id = ?",
		now.UnixMilli(), httpStatus, errorCode, approvalID)
	if err != nil {
		return fmt.Errorf("store: record admin action run: %w", err)
	}
	return nil
}

// AdminTargetName is the current name of an admin action's target, as the
// decider is shown it: a principal's or channel's name, a credential's or
// hook's label, "" for an invite or a report (which have none). ErrNotFound
// for an entity that does not exist.
func (s *Store) AdminTargetName(ctx context.Context, t schema.AdminTargetV1) (string, error) {
	return adminTargetNameTx(ctx, s.db, t)
}

func adminTargetNameTx(ctx context.Context, tx execer, t schema.AdminTargetV1) (string, error) {
	var q string
	switch t.Kind {
	case schema.AdminTargetPrincipal:
		q = "SELECT name FROM principals WHERE id = ?"
	case schema.AdminTargetChannel:
		q = "SELECT name FROM channels WHERE id = ?"
	case schema.AdminTargetCredential:
		q = "SELECT label FROM credentials WHERE id = ?"
	case schema.AdminTargetHook:
		q = "SELECT label FROM hooks WHERE id = ?"
	case schema.AdminTargetInvite:
		q = "SELECT '' FROM invites WHERE id = ?"
	case schema.AdminTargetReport:
		q = "SELECT '' FROM problem_reports WHERE id = ?"
	default:
		return "", ErrNotFound
	}
	var name string
	switch err := tx.QueryRowContext(ctx, q, t.ID).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("store: admin target name: %w", err)
	}
	return name, nil
}

// PendingAdminActions counts agentID's gated actions awaiting a decision.
func (s *Store) PendingAdminActions(ctx context.Context, agentID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admin_actions x JOIN approvals a ON a.id = x.approval_id
		 WHERE x.agent_id = ? AND a.state IN ('pending', 'escalated')`, agentID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count pending admin actions: %w", err)
	}
	return n, nil
}

// CredentialPrincipal is the principal a credential belongs to; ErrNotFound
// for an unknown credential.
func (s *Store) CredentialPrincipal(ctx context.Context, credentialID int64) (int64, error) {
	var id int64
	switch err := s.db.QueryRowContext(ctx, "SELECT principal_id FROM credentials WHERE id = ?", credentialID).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrNotFound
	case err != nil:
		return 0, fmt.Errorf("store: credential principal: %w", err)
	}
	return id, nil
}
