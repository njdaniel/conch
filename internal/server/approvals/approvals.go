// Package approvals is conchd's approval state machine coordinator
// (docs/design/approval-object.md). The store owns the transactional state
// transitions; this package owns time — deadline and escalation-grace timers,
// their rehydration after a restart — and the notification seam.
//
// Notification delivery is optional (ADR-002): a failure is recorded as a
// notify_failed audit event and never blocks the approval lifecycle.
package approvals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Audit actions for the notification seam (approval-object.md §5–6).
const (
	AuditNotifySent   = "notify_sent"
	AuditNotifyFailed = "notify_failed"
)

// ErrInvalid wraps every validation failure of a create request, so the API
// layer can distinguish caller mistakes (a 400) from server faults (a 500).
var ErrInvalid = errors.New("approvals: invalid approval")

// Notifier delivers approval lifecycle notifications (approval-object.md §5).
// Implementations must be safe for concurrent use. The ntfy notifier (issue
// #14) implements this; NoopNotifier stands in until then.
type Notifier interface {
	// ApprovalCreated announces a new approval on the approvals topic.
	ApprovalCreated(ctx context.Context, a store.Approval) error
	// ApprovalEscalated announces a deadline breach, urgent priority.
	ApprovalEscalated(ctx context.Context, a store.Approval) error
	// ApprovalResolved closes the loop with the terminal resolution.
	ApprovalResolved(ctx context.Context, a store.Approval, r schema.ApprovalResolutionV1) error
}

// NoopNotifier is the default Notifier when no notification integration is
// configured: every delivery succeeds without doing anything.
type NoopNotifier struct{}

func (NoopNotifier) ApprovalCreated(context.Context, store.Approval) error { return nil }
func (NoopNotifier) ApprovalEscalated(context.Context, store.Approval) error {
	return nil
}
func (NoopNotifier) ApprovalResolved(context.Context, store.Approval, schema.ApprovalResolutionV1) error {
	return nil
}

// Manager drives approvals through their lifecycle: it creates them, accepts
// decisions, and fires the deadline (escalate) and grace (expire) transitions
// from timers that survive restarts via Rehydrate.
type Manager struct {
	store    *store.Store
	notifier Notifier

	mu     sync.Mutex
	timers map[int64]*time.Timer
	closed bool

	// inflight tracks running timer callbacks so Close can wait for them,
	// keeping tests deterministic.
	inflight sync.WaitGroup
}

// New builds a Manager on st. A nil notifier defaults to NoopNotifier.
func New(st *store.Store, notifier Notifier) *Manager {
	if notifier == nil {
		notifier = NoopNotifier{}
	}
	return &Manager{store: st, notifier: notifier, timers: make(map[int64]*time.Timer)}
}

// Close stops every scheduled timer and waits for in-flight transitions to
// finish. The manager must not be used afterwards.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for id, t := range m.timers {
		// A successful Stop means the callback will never run, so its
		// inflight slot is released here; otherwise the running callback
		// releases it itself.
		if t.Stop() {
			m.inflight.Done()
		}
		delete(m.timers, id)
	}
	m.mu.Unlock()
	m.inflight.Wait()
}

// Create validates and persists a new approval (audit: approval_created),
// notifies (audit: notify_sent / notify_failed), and schedules its deadline
// timer. The default escalation grace equals the original deadline window
// (approval-object.md §2); params.GraceDeadline overrides when set.
func (m *Manager) Create(ctx context.Context, params store.ApprovalParams) (store.Approval, error) {
	now := time.Now()
	if params.GraceDeadline.IsZero() {
		params.GraceDeadline = params.Deadline.Add(params.Deadline.Sub(now))
	}
	// The schema's Validate is the single rule set for approval
	// well-formedness; run it against the approval as it will exist. The
	// placeholder ID and server-assigned fields are overwritten by the store.
	candidate := store.Approval{
		ID:          1,
		RequesterID: params.RequesterID,
		ChannelID:   params.ChannelID,
		Title:       params.Title,
		Body:        params.Body,
		Payload:     params.Payload,
		Options:     params.Options,
		Deadline:    params.Deadline,
		Quorum:      params.Quorum,
		Escalation:  params.Escalation,
		State:       schema.ApprovalStatePending,
		CreatedAt:   now,
	}
	if err := candidate.ToSchema().Validate(); err != nil {
		return store.Approval{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !params.GraceDeadline.After(params.Deadline) {
		return store.Approval{}, fmt.Errorf("%w: grace deadline %s must be after deadline %s", ErrInvalid,
			params.GraceDeadline.UTC().Format(time.RFC3339), params.Deadline.UTC().Format(time.RFC3339))
	}

	a, err := m.store.CreateApproval(ctx, params)
	if err != nil {
		return store.Approval{}, err
	}
	m.notify(ctx, "created", a, func() error { return m.notifier.ApprovalCreated(ctx, a) })
	m.schedule(a.ID, time.Until(a.Deadline), m.onDeadline)
	return a, nil
}

// Decide casts one principal's decision (audit: decision_cast). When the
// decision meets quorum the approval resolves (audit: approval_resolved), its
// timers stop, and the resolution notification fires.
func (m *Manager) Decide(ctx context.Context, approvalID, principalID int64, optionID, reason string) (schema.Decision, *schema.ApprovalResolutionV1, error) {
	d, r, err := m.store.CastDecision(ctx, approvalID, principalID, optionID, reason)
	if err != nil {
		return schema.Decision{}, nil, err
	}
	if r != nil {
		m.cancel(approvalID)
		if a, err := m.store.ApprovalByID(ctx, approvalID); err == nil {
			m.notify(ctx, "resolved", a, func() error { return m.notifier.ApprovalResolved(ctx, a, *r) })
		} else {
			slog.ErrorContext(ctx, "approvals: load resolved approval for notification failed", "approval", approvalID, "error", err)
		}
	}
	return d, r, nil
}

// Rehydrate reloads every open approval and re-arms its next transition —
// deadline for pending, grace deadline for escalated. Deadlines that passed
// while the server was down fire immediately, in order (escalate, then
// expire), so the audit chain stays complete across restarts.
func (m *Manager) Rehydrate(ctx context.Context) error {
	open, err := m.store.ListOpenApprovals(ctx)
	if err != nil {
		return err
	}
	for _, a := range open {
		switch a.State {
		case schema.ApprovalStatePending:
			m.schedule(a.ID, time.Until(a.Deadline), m.onDeadline)
		case schema.ApprovalStateEscalated:
			m.schedule(a.ID, time.Until(a.GraceDeadline), m.onGrace)
		default:
			// ListOpenApprovals only returns pending/escalated.
		}
	}
	return nil
}

// schedule arms (or re-arms) the single timer for an approval. A non-positive
// delay fires as soon as the timer goroutine runs.
func (m *Manager) schedule(id int64, d time.Duration, fire func(id int64)) {
	if d < 0 {
		d = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if t, ok := m.timers[id]; ok && t.Stop() {
		// Replacing an armed timer releases the slot its callback will
		// now never claim.
		m.inflight.Done()
	}
	m.inflight.Add(1)
	m.timers[id] = time.AfterFunc(d, func() {
		defer m.inflight.Done()
		fire(id)
	})
}

// cancel stops and forgets an approval's timer, releasing its inflight slot if
// the timer had not fired.
func (m *Manager) cancel(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.timers[id]
	if !ok {
		return
	}
	delete(m.timers, id)
	if t.Stop() {
		m.inflight.Done()
	}
}

// onDeadline fires when an approval's deadline passes: pending → escalated
// (audit: approval_escalated), urgent notification, then the grace timer is
// armed. If the approval was decided in the meantime the escalation is a
// no-op and no timer is re-armed.
func (m *Manager) onDeadline(id int64) {
	ctx := context.Background()
	m.forget(id)
	escalated, err := m.store.EscalateApproval(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: escalate failed", "approval", id, "error", err)
		return
	}
	if !escalated {
		return
	}
	a, err := m.store.ApprovalByID(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: load escalated approval failed", "approval", id, "error", err)
		return
	}
	m.notify(ctx, "escalated", a, func() error { return m.notifier.ApprovalEscalated(ctx, a) })
	m.schedule(id, time.Until(a.GraceDeadline), m.onGrace)
}

// onGrace fires when the escalation grace period ends: escalated → expired
// with an outcome=expired resolution (audit: approval_expired). A concurrent
// resolution makes this a no-op.
func (m *Manager) onGrace(id int64) {
	ctx := context.Background()
	m.forget(id)
	r, err := m.store.ExpireApproval(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: expire failed", "approval", id, "error", err)
		return
	}
	if r == nil {
		return
	}
	a, err := m.store.ApprovalByID(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: load expired approval failed", "approval", id, "error", err)
		return
	}
	m.notify(ctx, "expired", a, func() error { return m.notifier.ApprovalResolved(ctx, a, *r) })
}

// forget drops the timer entry for id without stopping it — used from inside
// a firing callback, whose inflight slot is released by the AfterFunc wrapper.
func (m *Manager) forget(id int64) {
	m.mu.Lock()
	delete(m.timers, id)
	m.mu.Unlock()
}

// notify runs one delivery through the notifier seam and appends the matching
// notify_sent / notify_failed audit event. Delivery failure is recorded, never
// propagated: notifications are reachability, not correctness (ADR-002).
func (m *Manager) notify(ctx context.Context, event string, a store.Approval, deliver func() error) {
	action := AuditNotifySent
	detail := "event=" + event
	if err := deliver(); err != nil {
		slog.ErrorContext(ctx, "approvals: notification failed", "approval", a.ID, "event", event, "error", err)
		action = AuditNotifyFailed
		detail = fmt.Sprintf("event=%s error=%q", event, err)
	}
	if _, err := m.store.AppendAuditEvent(ctx, "system", action, fmt.Sprintf("approval:%d", a.ID), detail); err != nil {
		slog.ErrorContext(ctx, "approvals: append notify audit failed", "approval", a.ID, "event", event, "error", err)
	}
}
