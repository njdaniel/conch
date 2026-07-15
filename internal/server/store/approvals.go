package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Approval-path sentinel errors, distinguished so the API layer can map each
// to a distinct wire error.
var (
	// ErrTerminalApproval is returned for a decision (or transition) against a
	// resolved or expired approval — a protocol error per approval-object.md §2.
	ErrTerminalApproval = errors.New("store: approval is in a terminal state")
	// ErrUnknownOption is returned when a decision selects an option id the
	// approval does not carry.
	ErrUnknownOption = errors.New("store: option not among the approval's options")
	// ErrDuplicateDecision is returned when a principal casts a second decision
	// on the same approval.
	ErrDuplicateDecision = errors.New("store: principal already decided this approval")
)

// Audit actions for the approval lifecycle (approval-object.md §6). notify_sent
// and notify_failed are appended by the notification layer, not here.
const (
	AuditApprovalCreated   = "approval_created"
	AuditDecisionCast      = "decision_cast"
	AuditApprovalResolved  = "approval_resolved"
	AuditApprovalEscalated = "approval_escalated"
	AuditApprovalExpired   = "approval_expired"
)

// Approval is a stored approval object (approval-object.md §1). It mirrors
// schema.ApprovalV1 with Go-native field types; ToSchema converts to the wire
// shape.
type Approval struct {
	ID            int64
	RequesterID   int64
	ChannelID     int64
	Title         string
	Body          string
	Payload       *schema.Payload
	Options       []schema.Option
	Deadline      time.Time
	GraceDeadline time.Time
	Quorum        int
	Escalation    *schema.EscalationTarget
	State         schema.ApprovalState
	CreatedAt     time.Time
}

// ToSchema converts the stored approval to its wire shape.
func (a Approval) ToSchema() schema.ApprovalV1 {
	return schema.ApprovalV1{
		ID:               a.ID,
		RequesterID:      a.RequesterID,
		ChannelID:        a.ChannelID,
		Title:            a.Title,
		Body:             a.Body,
		Payload:          a.Payload,
		Options:          a.Options,
		Deadline:         schema.NewTimestamp(a.Deadline),
		Quorum:           a.Quorum,
		EscalationTarget: a.Escalation,
		State:            a.State,
		CreatedAt:        schema.NewTimestamp(a.CreatedAt),
	}
}

// ApprovalParams are the caller-supplied fields of a new approval. The server
// assigns ID, State, and CreatedAt; GraceDeadline is the final deadline after
// which an escalated approval expires.
type ApprovalParams struct {
	RequesterID   int64
	ChannelID     int64
	Title         string
	Body          string
	Payload       *schema.Payload
	Options       []schema.Option
	Deadline      time.Time
	GraceDeadline time.Time
	Quorum        int
	Escalation    *schema.EscalationTarget
}

// execer is the subset of *sql.Conn / *sql.DB used inside a transaction body.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// withImmediateTx runs fn inside BEGIN IMMEDIATE … COMMIT on a dedicated
// connection. IMMEDIATE takes the write lock up front, so concurrent
// approval-path transactions serialize at begin instead of failing with a
// busy-snapshot error at their first write — this is what makes the in-tx
// quorum check safe under concurrency (approval-object.md §2).
func (s *Store) withImmediateTx(ctx context.Context, fn func(tx execer) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: begin immediate: %w", err)
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// appendAuditEventTx appends an audit event inside an open transaction, so a
// state transition and its audit entry commit or roll back together.
func appendAuditEventTx(ctx context.Context, tx execer, actor, action, subject, detail string, at time.Time) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO audit_events (actor, action, subject, detail, created_at) VALUES (?, ?, ?, ?, ?)",
		actor, action, subject, detail, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: append audit event %q: %w", action, err)
	}
	return nil
}

func approvalSubject(id int64) string { return fmt.Sprintf("approval:%d", id) }

func principalActor(id int64) string { return fmt.Sprintf("principal:%d", id) }

// CreateApproval persists a new pending approval and its approval_created
// audit event in one transaction. Validation of the caller-supplied fields is
// the API layer's job (via schema.ApprovalV1.Validate); the store only encodes.
func (s *Store) CreateApproval(ctx context.Context, p ApprovalParams) (Approval, error) {
	optionsJSON, err := json.Marshal(p.Options)
	if err != nil {
		return Approval{}, fmt.Errorf("store: encode approval options: %w", err)
	}
	var payloadSchema string
	var payloadJSON []byte
	if p.Payload != nil {
		payloadSchema = p.Payload.Schema
		payloadJSON = p.Payload.Data
	}
	var escKind, escTopic string
	var escPrincipal int64
	if p.Escalation != nil {
		escKind = string(p.Escalation.Kind)
		escPrincipal = p.Escalation.PrincipalID
		escTopic = p.Escalation.Topic
	}

	now := time.Now().Truncate(time.Millisecond)
	a := Approval{
		RequesterID:   p.RequesterID,
		ChannelID:     p.ChannelID,
		Title:         p.Title,
		Body:          p.Body,
		Payload:       p.Payload,
		Options:       p.Options,
		Deadline:      p.Deadline.Truncate(time.Millisecond),
		GraceDeadline: p.GraceDeadline.Truncate(time.Millisecond),
		Quorum:        p.Quorum,
		Escalation:    p.Escalation,
		State:         schema.ApprovalStatePending,
		CreatedAt:     now,
	}

	err = s.withImmediateTx(ctx, func(tx execer) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO approvals (requester_id, channel_id, title, body,
			   payload_schema, payload_json, options_json,
			   deadline, grace_deadline, quorum,
			   escalation_kind, escalation_principal_id, escalation_topic,
			   state, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.RequesterID, p.ChannelID, p.Title, p.Body,
			payloadSchema, string(payloadJSON), string(optionsJSON),
			a.Deadline.UnixMilli(), a.GraceDeadline.UnixMilli(), p.Quorum,
			escKind, escPrincipal, escTopic,
			string(schema.ApprovalStatePending), now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: insert approval: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: insert approval: %w", err)
		}
		a.ID = id
		return appendAuditEventTx(ctx, tx,
			principalActor(p.RequesterID), AuditApprovalCreated, approvalSubject(id),
			fmt.Sprintf("title=%q deadline=%s quorum=%d", p.Title, schema.NewTimestamp(a.Deadline).Time().Format(time.RFC3339), p.Quorum),
			now)
	})
	if err != nil {
		return Approval{}, err
	}
	return a, nil
}

// approvalColumns is the SELECT list scanApproval expects.
const approvalColumns = `id, requester_id, channel_id, title, body,
	payload_schema, payload_json, options_json,
	deadline, grace_deadline, quorum,
	escalation_kind, escalation_principal_id, escalation_topic,
	state, created_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanApproval(row rowScanner) (Approval, error) {
	var a Approval
	var payloadSchema, payloadJSON, optionsJSON string
	var deadline, grace, createdAt int64
	var escKind, escTopic string
	var escPrincipal int64
	var state string
	err := row.Scan(&a.ID, &a.RequesterID, &a.ChannelID, &a.Title, &a.Body,
		&payloadSchema, &payloadJSON, &optionsJSON,
		&deadline, &grace, &a.Quorum,
		&escKind, &escPrincipal, &escTopic,
		&state, &createdAt)
	if err != nil {
		return Approval{}, err
	}
	if payloadSchema != "" {
		a.Payload = &schema.Payload{Schema: payloadSchema, Data: json.RawMessage(payloadJSON)}
	}
	if err := json.Unmarshal([]byte(optionsJSON), &a.Options); err != nil {
		return Approval{}, fmt.Errorf("store: decode approval options: %w", err)
	}
	if escKind != "" {
		a.Escalation = &schema.EscalationTarget{
			Kind:        schema.EscalationTargetKind(escKind),
			PrincipalID: escPrincipal,
			Topic:       escTopic,
		}
	}
	a.Deadline = time.UnixMilli(deadline)
	a.GraceDeadline = time.UnixMilli(grace)
	a.State = schema.ApprovalState(state)
	a.CreatedAt = time.UnixMilli(createdAt)
	return a, nil
}

// ApprovalByID returns the approval with id, or ErrNotFound.
func (s *Store) ApprovalByID(ctx context.Context, id int64) (Approval, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+approvalColumns+" FROM approvals WHERE id = ?", id)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, fmt.Errorf("store: find approval %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Approval{}, fmt.Errorf("store: find approval %d: %w", id, err)
	}
	return a, nil
}

// ListOpenApprovals returns every approval still open for decisions (pending
// or escalated), in ascending ID order. It is the rehydration source for the
// deadline timers on boot.
func (s *Store) ListOpenApprovals(ctx context.Context) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+approvalColumns+" FROM approvals WHERE state IN (?, ?) ORDER BY id ASC",
		string(schema.ApprovalStatePending), string(schema.ApprovalStateEscalated))
	if err != nil {
		return nil, fmt.Errorf("store: list open approvals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var approvals []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list open approvals: %w", err)
		}
		approvals = append(approvals, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list open approvals: %w", err)
	}
	return approvals, nil
}

// CastDecision records one principal's decision on an approval, appends its
// decision_cast audit event, and — inside the same transaction — checks
// quorum. When this decision is the one that meets quorum it transitions the
// approval to resolved, persists the resolution event, appends
// approval_resolved, and returns the resolution; otherwise resolution is nil.
//
// The whole read-check-write sequence runs under one IMMEDIATE transaction, so
// exactly one concurrent decision can be the resolving one.
func (s *Store) CastDecision(ctx context.Context, approvalID, principalID int64, optionID, reason string) (schema.Decision, *schema.ApprovalResolutionV1, error) {
	d := schema.Decision{
		PrincipalID: principalID,
		OptionID:    optionID,
		Reason:      reason,
		At:          schema.NewTimestamp(time.Now()),
	}
	if err := d.Validate(); err != nil {
		return schema.Decision{}, nil, err
	}

	var resolution *schema.ApprovalResolutionV1
	err := s.withImmediateTx(ctx, func(tx execer) error {
		row := tx.QueryRowContext(ctx,
			"SELECT "+approvalColumns+" FROM approvals WHERE id = ?", approvalID)
		a, err := scanApproval(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: find approval %d: %w", approvalID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: find approval %d: %w", approvalID, err)
		}
		if a.State.IsTerminal() {
			return fmt.Errorf("store: decide approval %d in state %s: %w", approvalID, a.State, ErrTerminalApproval)
		}
		valid := false
		for _, o := range a.Options {
			if o.ID == optionID {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("store: decide approval %d with option %q: %w", approvalID, optionID, ErrUnknownOption)
		}

		if _, err := tx.ExecContext(ctx,
			"INSERT INTO approval_decisions (approval_id, principal_id, option_id, reason, created_at) VALUES (?, ?, ?, ?, ?)",
			approvalID, principalID, optionID, reason, d.At.Time().UnixMilli()); err != nil {
			if isUniqueConstraintErr(err) {
				return fmt.Errorf("store: principal %d re-deciding approval %d: %w", principalID, approvalID, ErrDuplicateDecision)
			}
			return fmt.Errorf("store: insert decision: %w", err)
		}
		if err := appendAuditEventTx(ctx, tx,
			principalActor(principalID), AuditDecisionCast, approvalSubject(approvalID),
			fmt.Sprintf("option=%s reason=%q", optionID, reason), d.At.Time()); err != nil {
			return err
		}

		// Quorum: concurring decisions select the same option
		// (approval-object.md §1).
		var concurring int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM approval_decisions WHERE approval_id = ? AND option_id = ?",
			approvalID, optionID).Scan(&concurring); err != nil {
			return fmt.Errorf("store: count concurring decisions: %w", err)
		}
		if concurring < a.Quorum {
			return nil
		}

		r, err := resolveTx(ctx, tx, a, optionID, d.At.Time())
		if err != nil {
			return err
		}
		resolution = r
		return nil
	})
	if err != nil {
		return schema.Decision{}, nil, err
	}
	return d, resolution, nil
}

// resolveTx transitions an open approval to resolved inside an open
// transaction: it derives the outcome from the winning option's kind, persists
// the resolution event, and appends approval_resolved.
func resolveTx(ctx context.Context, tx execer, a Approval, winningOptionID string, at time.Time) (*schema.ApprovalResolutionV1, error) {
	var outcome schema.Outcome
	for _, o := range a.Options {
		if o.ID != winningOptionID {
			continue
		}
		switch o.Kind {
		case schema.OptionKindApprove:
			outcome = schema.OutcomeApproved
		case schema.OptionKindReject:
			outcome = schema.OutcomeRejected
		case schema.OptionKindCustom:
			outcome = schema.OutcomeCustom
		}
	}
	if outcome == "" {
		return nil, fmt.Errorf("store: resolve approval %d: option %q not found", a.ID, winningOptionID)
	}

	decisions, err := listDecisionsTx(ctx, tx, a.ID)
	if err != nil {
		return nil, err
	}

	r := schema.ApprovalResolutionV1{
		ApprovalID: a.ID,
		Outcome:    outcome,
		OptionID:   winningOptionID,
		Decisions:  decisions,
		ResolvedAt: schema.NewTimestamp(at),
	}
	if err := insertResolutionTx(ctx, tx, r, schema.ApprovalStateResolved); err != nil {
		return nil, err
	}
	if err := appendAuditEventTx(ctx, tx,
		"system", AuditApprovalResolved, approvalSubject(a.ID),
		fmt.Sprintf("outcome=%s option=%s decisions=%d", outcome, winningOptionID, len(decisions)), at); err != nil {
		return nil, err
	}
	return &r, nil
}

// insertResolutionTx persists the single resolution event for an approval and
// flips the approval row to its terminal state, guarding against double
// resolution at the SQL level (the state predicate) on top of the state checks.
func insertResolutionTx(ctx context.Context, tx execer, r schema.ApprovalResolutionV1, terminal schema.ApprovalState) error {
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("store: encode resolution: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE approvals SET state = ? WHERE id = ? AND state IN (?, ?)",
		string(terminal), r.ApprovalID,
		string(schema.ApprovalStatePending), string(schema.ApprovalStateEscalated))
	if err != nil {
		return fmt.Errorf("store: transition approval %d to %s: %w", r.ApprovalID, terminal, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: transition approval %d to %s: %w", r.ApprovalID, terminal, err)
	}
	if n == 0 {
		return fmt.Errorf("store: transition approval %d to %s: %w", r.ApprovalID, terminal, ErrTerminalApproval)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO approval_resolutions (approval_id, outcome, option_id, resolution_json, resolved_at) VALUES (?, ?, ?, ?, ?)",
		r.ApprovalID, string(r.Outcome), r.OptionID, string(body), r.ResolvedAt.Time().UnixMilli()); err != nil {
		return fmt.Errorf("store: insert resolution for approval %d: %w", r.ApprovalID, err)
	}
	return nil
}

func listDecisionsTx(ctx context.Context, tx execer, approvalID int64) ([]schema.Decision, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT principal_id, option_id, reason, created_at FROM approval_decisions WHERE approval_id = ? ORDER BY id ASC",
		approvalID)
	if err != nil {
		return nil, fmt.Errorf("store: list decisions for approval %d: %w", approvalID, err)
	}
	defer func() { _ = rows.Close() }()

	decisions := []schema.Decision{}
	for rows.Next() {
		var d schema.Decision
		var at int64
		if err := rows.Scan(&d.PrincipalID, &d.OptionID, &d.Reason, &at); err != nil {
			return nil, fmt.Errorf("store: list decisions for approval %d: %w", approvalID, err)
		}
		d.At = schema.NewTimestamp(time.UnixMilli(at))
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list decisions for approval %d: %w", approvalID, err)
	}
	return decisions, nil
}

// EscalateApproval transitions a pending approval to escalated and appends
// approval_escalated, in one transaction. It reports whether the transition
// happened: false when the approval is no longer pending (already decided,
// escalated, or terminal), which callers treat as a benign no-op.
func (s *Store) EscalateApproval(ctx context.Context, id int64) (bool, error) {
	escalated := false
	err := s.withImmediateTx(ctx, func(tx execer) error {
		now := time.Now().Truncate(time.Millisecond)
		res, err := tx.ExecContext(ctx,
			"UPDATE approvals SET state = ? WHERE id = ? AND state = ?",
			string(schema.ApprovalStateEscalated), id, string(schema.ApprovalStatePending))
		if err != nil {
			return fmt.Errorf("store: escalate approval %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: escalate approval %d: %w", id, err)
		}
		if n == 0 {
			return nil
		}
		escalated = true
		return appendAuditEventTx(ctx, tx,
			"system", AuditApprovalEscalated, approvalSubject(id), "deadline passed while pending", now)
	})
	if err != nil {
		return false, err
	}
	return escalated, nil
}

// ExpireApproval transitions an open approval whose grace deadline has passed
// to expired, persisting the outcome=expired resolution event and appending
// approval_expired, in one transaction. It returns the resolution, or nil when
// the approval was no longer open (resolved concurrently) — a benign no-op.
func (s *Store) ExpireApproval(ctx context.Context, id int64) (*schema.ApprovalResolutionV1, error) {
	var resolution *schema.ApprovalResolutionV1
	err := s.withImmediateTx(ctx, func(tx execer) error {
		row := tx.QueryRowContext(ctx,
			"SELECT "+approvalColumns+" FROM approvals WHERE id = ?", id)
		a, err := scanApproval(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: find approval %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: find approval %d: %w", id, err)
		}
		if a.State.IsTerminal() {
			return nil
		}

		now := time.Now().Truncate(time.Millisecond)
		decisions, err := listDecisionsTx(ctx, tx, id)
		if err != nil {
			return err
		}
		r := schema.ApprovalResolutionV1{
			ApprovalID: id,
			Outcome:    schema.OutcomeExpired,
			Decisions:  decisions,
			ResolvedAt: schema.NewTimestamp(now),
		}
		if err := insertResolutionTx(ctx, tx, r, schema.ApprovalStateExpired); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx,
			"system", AuditApprovalExpired, approvalSubject(id),
			fmt.Sprintf("no quorum by final deadline; decisions=%d", len(decisions)), now); err != nil {
			return err
		}
		resolution = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resolution, nil
}

// ResolutionByApprovalID returns the persisted resolution event for an
// approval, or ErrNotFound when the approval has not reached a terminal state.
// Every reader — awaiters, pollers, the audit trail — sees this same event.
func (s *Store) ResolutionByApprovalID(ctx context.Context, approvalID int64) (schema.ApprovalResolutionV1, error) {
	var body string
	err := s.db.QueryRowContext(ctx,
		"SELECT resolution_json FROM approval_resolutions WHERE approval_id = ?", approvalID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return schema.ApprovalResolutionV1{}, fmt.Errorf("store: resolution for approval %d: %w", approvalID, ErrNotFound)
	}
	if err != nil {
		return schema.ApprovalResolutionV1{}, fmt.Errorf("store: resolution for approval %d: %w", approvalID, err)
	}
	var r schema.ApprovalResolutionV1
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return schema.ApprovalResolutionV1{}, fmt.Errorf("store: decode resolution for approval %d: %w", approvalID, err)
	}
	return r, nil
}
