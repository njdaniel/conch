// Package approvals is conchd's approval state machine coordinator
// (docs/design/approval-object.md). The store owns the transactional state
// transitions; this package owns time — deadline and escalation-grace timers,
// their rehydration after a restart — and the notification seam.
//
// Notification delivery is optional (ADR-002): a failure is recorded as a
// notify_failed audit event and never blocks the approval lifecycle. What is
// guaranteed about it, and how each failure after a committed transition is
// handled, is approval-object.md §5.1.
package approvals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Audit actions for the notification seam (approval-object.md §5–6).
const (
	AuditNotifySent   = "notify_sent"
	AuditNotifyFailed = "notify_failed"
	// AuditApprovalsStarted is appended once per start, with whether a
	// notifier is configured. The next start reads the log from it to find
	// transitions a stopped conchd left without a notify row (§5.1).
	AuditApprovalsStarted = "approvals_started"
)

// The two details an approvals_started row carries.
const (
	notificationsOn  = "notifications=on"
	notificationsOff = "notifications=off"
)

// The four notifications, named as a notify row's detail names them.
const (
	eventCreated   = "created"
	eventEscalated = "escalated"
	eventResolved  = "resolved"
	eventExpired   = "expired"
)

const (
	// defaultRetryEvery is how long a step that failed on the store waits
	// before it is tried again.
	defaultRetryEvery = 5 * time.Second
	// loadRetries is how many times reading an approval back is retried
	// before its notification is recorded as not attempted.
	loadRetries = 5
	// defaultCatchUpBudget bounds the deliveries made at start for
	// transitions a stopped conchd left without a notify row, so an
	// unreachable ntfy cannot hold up start-up.
	defaultCatchUpBudget = 10 * time.Second
	// auditPage is how many audit rows catch-up reads at a time.
	auditPage = 500
)

// Why a notification was recorded as not attempted.
const (
	whyNotLoaded   = "not attempted: the approval could not be loaded"
	whyBudgetSpent = "not attempted: start-up time budget spent"
	whyNoTopic     = "not attempted: no ntfy topic configured for this notification"
)

// ErrInvalid wraps every validation failure of a create request, so the API
// layer can distinguish caller mistakes (a 400) from server faults (a 500).
var ErrInvalid = errors.New("approvals: invalid approval")

// Notifier delivers approval lifecycle notifications (approval-object.md §5).
// Implementations must be safe for concurrent use. A nil Notifier means
// notifications are unconfigured and remain completely silent.
type Notifier interface {
	// ApprovalCreated announces a new approval on the approvals topic.
	ApprovalCreated(ctx context.Context, a store.Approval) error
	// ApprovalEscalated announces a deadline breach, urgent priority.
	ApprovalEscalated(ctx context.Context, a store.Approval) error
	// ApprovalResolved closes the loop with the terminal resolution.
	ApprovalResolved(ctx context.Context, a store.Approval, r schema.ApprovalResolutionV1) error
}

// backing is the part of the store the manager uses. It is an interface only
// so that a test can make one call fail; conchd always passes a *store.Store.
type backing interface {
	CreateApproval(ctx context.Context, p store.ApprovalParams) (store.Approval, error)
	CastDecision(ctx context.Context, approvalID, principalID int64, optionID, reason string) (schema.Decision, *schema.ApprovalResolutionV1, error)
	ApprovalByID(ctx context.Context, id int64) (store.Approval, error)
	ResolutionByApprovalID(ctx context.Context, approvalID int64) (schema.ApprovalResolutionV1, error)
	ListOpenApprovals(ctx context.Context) ([]store.Approval, error)
	EscalateApproval(ctx context.Context, id int64) (bool, error)
	ExpireApproval(ctx context.Context, id int64) (*schema.ApprovalResolutionV1, error)
	AppendAuditEvent(ctx context.Context, actor, action, subject, detail string) (store.AuditEvent, error)
	ListAuditEvents(ctx context.Context, afterID int64, limit int) ([]store.AuditEvent, error)
	LastAuditEvent(ctx context.Context, action string) (store.AuditEvent, error)
	LastAuditID(ctx context.Context) (int64, error)
}

// notice is one notification owed for a committed transition.
type notice struct {
	approval int64
	event    string
}

// Manager drives approvals through their lifecycle: it creates them, accepts
// decisions, and fires the deadline (escalate) and grace (expire) transitions
// from timers that survive restarts via Rehydrate.
type Manager struct {
	store    backing
	notifier Notifier

	// retryEvery and catchUpBudget are fields so tests need not wait.
	retryEvery    time.Duration
	catchUpBudget time.Duration

	mu     sync.Mutex
	timers map[int64]*time.Timer
	// retries holds the steps waiting to be tried again (later), so Close
	// can stop them.
	retries   map[uint64]*time.Timer
	nextRetry uint64
	closed    bool
	// attempted holds the owed notifications catch-up has already attempted
	// in this process, so that a catch-up that has to be repeated (a row
	// could not be written) does not deliver them again.
	attempted map[notice]bool

	// inflight tracks running timer callbacks so Close can wait for them,
	// keeping tests deterministic.
	inflight sync.WaitGroup
}

// New builds a Manager on st. A nil notifier leaves notifications unconfigured.
func New(st *store.Store, notifier Notifier) *Manager {
	return &Manager{
		store:         st,
		notifier:      notifier,
		retryEvery:    defaultRetryEvery,
		catchUpBudget: defaultCatchUpBudget,
		timers:        make(map[int64]*time.Timer),
		retries:       make(map[uint64]*time.Timer),
		attempted:     make(map[notice]bool),
	}
}

// Close stops every scheduled timer and waits for in-flight transitions to
// finish. The manager must not be used afterwards. A notification step still
// waiting to be retried is dropped here; the next start finds its transition
// without a notify row and makes the attempt then (§5.1).
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	abandoned := 0
	for key, t := range m.retries {
		if t.Stop() {
			m.inflight.Done()
			abandoned++
		}
		delete(m.retries, key)
	}
	if abandoned > 0 {
		slog.Warn("approvals: notification steps were waiting to be retried at shutdown; the next start redoes them", "steps", abandoned)
	}
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
	// The approval exists now. What follows from that must not depend on the
	// caller still being there (issue #155): a client that hangs up cancels
	// ctx, and on ctx the notification would not be sent and its audit row
	// would not be written, leaving an approval nobody was told about and a
	// log that does not say so. The notifier's own timeout bounds the call.
	m.announce(context.WithoutCancel(ctx), notice{a.ID, eventCreated}, 0)
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
		// The decision is committed: as in Create, the notification and its
		// audit row no longer depend on the caller (issue #155).
		m.announce(context.WithoutCancel(ctx), notice{approvalID, eventResolved}, 0)
	}
	return d, r, nil
}

// Rehydrate reloads every open approval and re-arms its next transition —
// deadline for pending, grace deadline for escalated. Deadlines that passed
// while the server was down fire immediately, in order (escalate, then
// expire), so the audit chain stays complete across restarts. Before any
// timer is armed it makes the notification attempts a stopped conchd left
// undone (§5.1). It must be called once, before requests are served.
func (m *Manager) Rehydrate(ctx context.Context) error {
	open, err := m.store.ListOpenApprovals(ctx)
	if err != nil {
		return err
	}
	// Everything in the log up to this id was written before this process
	// served a request or armed a timer: it is the past, which catch-up is
	// about. What comes after belongs to this run and is announced as it
	// happens.
	past, err := m.store.LastAuditID(ctx)
	if err != nil {
		return err
	}
	m.catchUp(ctx, past)
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
// no-op and no timer is re-armed. A store failure re-arms this timer rather
// than leaving the approval pending until the next restart.
func (m *Manager) onDeadline(id int64) { m.escalate(id, false) }

// onDeadlineAgain is onDeadline after the store failed it once.
func (m *Manager) onDeadlineAgain(id int64) { m.escalate(id, true) }

func (m *Manager) escalate(id int64, again bool) {
	ctx := context.Background()
	m.forget(id)
	escalated, err := m.store.EscalateApproval(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: escalate failed; trying again", "approval", id, "retry_in", m.retryEvery, "error", err)
		m.schedule(id, m.retryEvery, m.onDeadlineAgain)
		return
	}
	if !escalated {
		if !again {
			return
		}
		// The store reported a failure for this transition a moment ago. If
		// the approval is escalated now, that attempt committed after all
		// (nothing else escalates), and what follows the commit was never
		// done: do it now.
		a, err := m.store.ApprovalByID(ctx, id)
		if err != nil {
			slog.ErrorContext(ctx, "approvals: load approval after a failed escalation failed; trying again", "approval", id, "retry_in", m.retryEvery, "error", err)
			m.schedule(id, m.retryEvery, m.onDeadlineAgain)
			return
		}
		if a.State != schema.ApprovalStateEscalated {
			return
		}
	}
	m.announce(ctx, notice{id, eventEscalated}, 0)
	m.armGrace(id)
}

// armGrace arms the grace timer of an approval that has escalated. The grace
// deadline is read from the store; if that fails, this is tried again instead
// of leaving the approval without a timer until the next restart.
func (m *Manager) armGrace(id int64) {
	ctx := context.Background()
	m.forget(id)
	a, err := m.store.ApprovalByID(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: load escalated approval failed; trying again", "approval", id, "retry_in", m.retryEvery, "error", err)
		m.schedule(id, m.retryEvery, m.armGrace)
		return
	}
	if a.State != schema.ApprovalStateEscalated {
		// Decided since it escalated.
		return
	}
	m.schedule(id, time.Until(a.GraceDeadline), m.onGrace)
}

// onGrace fires when the escalation grace period ends: escalated → expired
// with an outcome=expired resolution (audit: approval_expired). A concurrent
// resolution makes this a no-op. A store failure re-arms this timer.
func (m *Manager) onGrace(id int64) { m.expire(id, false) }

// onGraceAgain is onGrace after the store failed it once.
func (m *Manager) onGraceAgain(id int64) { m.expire(id, true) }

func (m *Manager) expire(id int64, again bool) {
	ctx := context.Background()
	m.forget(id)
	r, err := m.store.ExpireApproval(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: expire failed; trying again", "approval", id, "retry_in", m.retryEvery, "error", err)
		m.schedule(id, m.retryEvery, m.onGraceAgain)
		return
	}
	if r == nil {
		if !again {
			return
		}
		// As in escalate: if the approval is expired now, the attempt the
		// store reported as failed committed (nothing else expires one; a
		// decision resolves it and announces that itself).
		a, err := m.store.ApprovalByID(ctx, id)
		if err != nil {
			slog.ErrorContext(ctx, "approvals: load approval after a failed expiry failed; trying again", "approval", id, "retry_in", m.retryEvery, "error", err)
			m.schedule(id, m.retryEvery, m.onGraceAgain)
			return
		}
		if a.State != schema.ApprovalStateExpired {
			return
		}
	}
	m.announce(ctx, notice{id, eventExpired}, 0)
}

// forget drops the timer entry for id without stopping it — used from inside
// a firing callback, whose inflight slot is released by the AfterFunc wrapper.
func (m *Manager) forget(id int64) {
	m.mu.Lock()
	delete(m.timers, id)
	m.mu.Unlock()
}

// announce makes the delivery attempt owed for a committed transition and
// records it (§5.1). Delivery failure is recorded, never propagated:
// notifications are reachability, not correctness (ADR-002). What the
// notification needs is read from the store here rather than passed in, so a
// retry and the catch-up at start go through the same steps.
//
// A failed read is retried loadRetries times, then recorded as not attempted.
func (m *Manager) announce(ctx context.Context, n notice, attempt int) {
	if m.notifier == nil {
		return
	}
	action, detail, err := m.attempt(ctx, time.Time{}, n)
	if err != nil {
		if attempt < loadRetries {
			slog.ErrorContext(ctx, "approvals: load approval for notification failed; trying again",
				"approval", n.approval, "event", n.event, "retry_in", m.retryEvery, "error", err)
			m.later(func(ctx context.Context) { m.announce(ctx, n, attempt+1) })
			return
		}
		slog.ErrorContext(ctx, "approvals: load approval for notification failed; giving up",
			"approval", n.approval, "event", n.event, "error", err)
		action, detail = notAttempted(n, whyNotLoaded)
	}
	m.record(ctx, n, action, detail)
}

// attempt reads what the notification needs and delivers it, returning the
// audit row that records the outcome. An error means the approval or its
// resolution could not be read and nothing was sent. A non-zero deliverBy
// bounds the delivery itself (not the reads).
func (m *Manager) attempt(ctx context.Context, deliverBy time.Time, n notice) (action, detail string, err error) {
	a, err := m.store.ApprovalByID(ctx, n.approval)
	if err != nil {
		return "", "", err
	}
	var r schema.ApprovalResolutionV1
	if n.event == eventResolved || n.event == eventExpired {
		if r, err = m.store.ResolutionByApprovalID(ctx, n.approval); err != nil {
			return "", "", err
		}
	}
	dctx := ctx
	if !deliverBy.IsZero() {
		var cancel context.CancelFunc
		dctx, cancel = context.WithDeadline(ctx, deliverBy)
		defer cancel()
	}
	if err := m.deliver(dctx, n, a, r); err != nil {
		if errors.Is(err, ErrNoTopic) {
			// Nothing was sent because nothing could be: the wording is fixed
			// here and does not come from the notifier's error. conchd said
			// which topic is missing once, when it started.
			action, detail = notAttempted(n, whyNoTopic)
			return action, detail, nil
		}
		slog.ErrorContext(ctx, "approvals: notification failed", "approval", n.approval, "event", n.event, "error", err)
		return AuditNotifyFailed, fmt.Sprintf("event=%s error=%q", n.event, err), nil
	}
	return AuditNotifySent, "event=" + n.event, nil
}

// errNotifierPanicked is what a delivery is recorded as failing with when the
// notifier panicked.
var errNotifierPanicked = errors.New("the notifier panicked")

// deliver hands one notification to the notifier. A notifier that panics has
// failed this delivery and nothing more: an approval must not lose its
// timers, nor conchd its start-up, to the optional side of the system.
func (m *Manager) deliver(ctx context.Context, n notice, a store.Approval, r schema.ApprovalResolutionV1) (err error) {
	defer func() {
		if p := recover(); p != nil {
			// The value is not logged: it can hold whatever the notifier had
			// in hand, the approval's text included.
			slog.ErrorContext(ctx, "approvals: the notifier panicked", "approval", n.approval, "event", n.event, "panic_type", fmt.Sprintf("%T", p))
			err = errNotifierPanicked
		}
	}()
	switch n.event {
	case eventCreated:
		return m.notifier.ApprovalCreated(ctx, a)
	case eventEscalated:
		return m.notifier.ApprovalEscalated(ctx, a)
	default:
		return m.notifier.ApprovalResolved(ctx, a, r)
	}
}

// notAttempted is the audit row for a notification conchd could not attempt.
func notAttempted(n notice, why string) (action, detail string) {
	return AuditNotifyFailed, fmt.Sprintf("event=%s error=%q", n.event, why)
}

// record appends a notify row, and keeps trying if the store refuses it: the
// attempt has been made, so only the row is retried, not the delivery. It
// reports whether the row was written now.
func (m *Manager) record(ctx context.Context, n notice, action, detail string) bool {
	if err := m.write(ctx, n, action, detail); err != nil {
		slog.ErrorContext(ctx, "approvals: append notify audit failed; trying again",
			"approval", n.approval, "event", n.event, "retry_in", m.retryEvery, "error", err)
		m.later(func(ctx context.Context) { m.record(ctx, n, action, detail) })
		return false
	}
	return true
}

func (m *Manager) write(ctx context.Context, n notice, action, detail string) error {
	_, err := m.store.AppendAuditEvent(ctx, "system", action, fmt.Sprintf("approval:%d", n.approval), detail)
	return err
}

// later runs step after retryEvery. A step that fails again calls later
// itself. Steps still waiting when the manager closes are dropped (see Close).
func (m *Manager) later(step func(ctx context.Context)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		slog.Warn("approvals: a notification step to retry was dropped at shutdown; the next start redoes it")
		return
	}
	m.nextRetry++
	key := m.nextRetry
	m.inflight.Add(1)
	m.retries[key] = time.AfterFunc(m.retryEvery, func() {
		defer m.inflight.Done()
		m.mu.Lock()
		delete(m.retries, key)
		m.mu.Unlock()
		step(context.Background())
	})
}

// catchUp makes the notification attempts that a stopped conchd left undone
// and then appends this start's approvals_started row (§5.1). past is the
// last audit id from before this process: catch-up concerns transitions up to
// it, never this run's own. If it cannot finish (the log cannot be read, a
// row cannot be written) it is tried again every retryEvery until it has, and
// the start row is written only then. Nothing here fails start-up: an
// approval must stay decidable with the audit log or ntfy in trouble.
func (m *Manager) catchUp(ctx context.Context, past int64) {
	if !m.caughtUp(ctx, past) {
		m.later(func(ctx context.Context) { m.catchUp(ctx, past) })
	}
}

// caughtUp is one pass of catchUp. It reports whether the start row is
// written.
func (m *Manager) caughtUp(ctx context.Context, past int64) bool {
	state := notificationsOff
	if m.notifier != nil {
		state = notificationsOn
	}
	last, err := m.store.LastAuditEvent(ctx, AuditApprovalsStarted)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// First start with this log: there is nowhere to read from.
	case err != nil:
		slog.ErrorContext(ctx, "approvals: read the last start from the audit log failed; trying again", "retry_in", m.retryEvery, "error", err)
		return false
	default:
		was, since := startedRow(last)
		if was != notificationsOn || m.notifier == nil {
			break
		}
		owed, err := m.owed(ctx, since, past)
		if err != nil {
			slog.ErrorContext(ctx, "approvals: read the audit log for owed notifications failed; trying again", "retry_in", m.retryEvery, "error", err)
			return false
		}
		if !m.settle(ctx, owed) {
			return false
		}
	}
	// since says where the next start reads from. It is the end of the past
	// as this process found it, not this row's own id: the row may be written
	// late, after transitions of this run, and they must not fall before it.
	detail := fmt.Sprintf("%s since=%d", state, past)
	if _, err := m.store.AppendAuditEvent(ctx, "system", AuditApprovalsStarted, "approvals", detail); err != nil {
		slog.ErrorContext(ctx, "approvals: append approvals_started failed; trying again", "retry_in", m.retryEvery, "error", err)
		return false
	}
	return true
}

// startedRow reads an approvals_started row: whether notifications were on
// (notificationsOn or notificationsOff), and the audit id the next start
// reads from. A row without since= is read from its own id.
func startedRow(e store.AuditEvent) (state string, since int64) {
	since = e.ID
	for _, field := range strings.Fields(e.Detail) {
		if v, ok := strings.CutPrefix(field, "since="); ok {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil {
				since = id
			}
			continue
		}
		if strings.HasPrefix(field, "notifications=") {
			state = field
		}
	}
	return state, since
}

// owed returns, in log order, the transitions in audit rows (afterID, upTo]
// that have no notify row. Notify rows are read to the end of the log: one
// written by this process for an older transition settles it.
func (m *Manager) owed(ctx context.Context, afterID, upTo int64) ([]notice, error) {
	events := map[string]string{
		store.AuditApprovalCreated:   eventCreated,
		store.AuditApprovalEscalated: eventEscalated,
		store.AuditApprovalResolved:  eventResolved,
		store.AuditApprovalExpired:   eventExpired,
	}
	at := make(map[notice]int64)
	for {
		page, err := m.store.ListAuditEvents(ctx, afterID, auditPage)
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			afterID = e.ID
			rest, ok := strings.CutPrefix(e.Subject, "approval:")
			if !ok {
				continue
			}
			id, err := strconv.ParseInt(rest, 10, 64)
			if err != nil {
				continue
			}
			switch e.Action {
			case AuditNotifySent, AuditNotifyFailed:
				name, _, _ := strings.Cut(strings.TrimPrefix(e.Detail, "event="), " ")
				delete(at, notice{id, name})
			default:
				if event, ok := events[e.Action]; ok && e.ID <= upTo {
					at[notice{id, event}] = e.ID
				}
			}
		}
		if len(page) < auditPage {
			break
		}
	}
	owed := make([]notice, 0, len(at))
	for n := range at {
		owed = append(owed, n)
	}
	sort.Slice(owed, func(i, j int) bool { return at[owed[i]] < at[owed[j]] })
	return owed, nil
}

// settle makes each owed attempt and writes its row, and reports whether
// every one of them now has a row. Deliveries stop when the start-up budget
// is spent, which also bounds the delivery in progress; the rest are recorded
// as not attempted. A read that fails here is recorded as not attempted at
// once: start-up does not wait on retries. An attempt made is not made again
// by a later pass in this process; only its row is retried.
func (m *Manager) settle(ctx context.Context, owed []notice) bool {
	deadline := time.Now().Add(m.catchUpBudget)
	all := true
	announced := false
	for _, n := range owed {
		m.mu.Lock()
		done := m.attempted[n]
		m.attempted[n] = true
		m.mu.Unlock()
		if done {
			// Its row is still being retried (record).
			all = false
			continue
		}
		if !announced {
			slog.WarnContext(ctx, "approvals: transitions from before this start have no notification record; attempting them now", "count", len(owed))
			announced = true
		}
		var action, detail string
		if time.Now().After(deadline) {
			action, detail = notAttempted(n, whyBudgetSpent)
		} else {
			var err error
			if action, detail, err = m.attempt(ctx, deadline, n); err != nil {
				slog.ErrorContext(ctx, "approvals: load approval for notification failed", "approval", n.approval, "event", n.event, "error", err)
				action, detail = notAttempted(n, whyNotLoaded)
			}
		}
		if !m.record(ctx, n, action, detail) {
			all = false
		}
	}
	return all
}
