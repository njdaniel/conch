package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Interventions (issue #287, P3 epic #263, decision D10). An intervention is a
// person asking an agent's run to do something: take a message, or stop. The
// server records the request and what the run's adapter said about it, and
// nothing more. In particular it never changes the run: an applied cancel is
// the adapter saying it stopped the work, and the run becomes cancelled only
// when the adapter also reports that, through SetRunState (UX-19).
//
// Who may see an intervention is its run's rule, and every read of the table
// is in audience.go (TestOnlyAudienceGoReadsRunRows). This file only writes;
// where a write must first read the intervention or its run, it asks
// audience.go, inside its transaction.
//
// Nothing here reads a clock of its own: the caller passes the time.

// Audit actions written by interventions. The detail of each names the run and
// the kind and, for an answer, the state. It never holds the instruction a
// person gave (it is theirs and the run's, not the audit log's) or the reason
// text an adapter gave.
const (
	AuditInterventionRequested   = "intervention_requested"
	AuditInterventionAnswered    = "intervention_answered"
	AuditInterventionUnsupported = "intervention_unsupported"
)

// MaxRequestedInterventions is how many interventions may sit in state
// requested on one run. The next is refused: an adapter that has stopped
// answering must not let a run's backlog grow without end.
const MaxRequestedInterventions = 16

var (
	// ErrInterventionNotFound is returned when no intervention on a run the
	// reader may see, and (for an answer) that the credential owns, has the
	// id. A missing intervention, one the reader cannot see and one on
	// another credential's run are one error, as for runs.
	ErrInterventionNotFound = errors.New("store: intervention not found")
	// ErrInterventionUnsupported is returned when the run's session did not
	// declare the requested kind. Nothing is stored, except the audit row for
	// the refusal.
	ErrInterventionUnsupported = errors.New("store: intervention kind not supported by the session")
	// ErrTooManyInterventions is returned when the run already has
	// MaxRequestedInterventions in state requested.
	ErrTooManyInterventions = errors.New("store: too many requested interventions on the run")
	// ErrNotHuman is returned when the requester is not an enabled human
	// principal.
	ErrNotHuman = errors.New("store: interventions are requested by human principals")
)

// Intervention is one row of the interventions table. ChannelID is the run's,
// for the caller's channel gate; it is not part of the row. Body is "" except
// on a message; AnsweredAt is nil while requested; Reason is "" when the
// adapter gave none.
type Intervention struct {
	ID          int64
	RunID       int64
	ChannelID   int64
	Kind        schema.InterventionKind
	Body        string
	State       schema.InterventionState
	RequestedBy int64
	RequestedAt time.Time
	AnsweredAt  *time.Time
	Reason      string
}

// InterventionParams is what a person asks. The run comes from the URL and
// the requester from the caller's authentication.
type InterventionParams struct {
	RunID int64
	Kind  schema.InterventionKind
	Body  string
}

func interventionSubject(id int64) string { return fmt.Sprintf("intervention:%d", id) }

// InterventionMoveAllowed reports whether an intervention in state from may
// move to state to. The adapter's answers are acknowledged, applied and
// failed; nothing moves back to requested, and nothing moves out of applied
// or failed.
func InterventionMoveAllowed(from, to schema.InterventionState) bool {
	switch from {
	case schema.InterventionStateRequested:
		return to == schema.InterventionStateAcknowledged || to == schema.InterventionStateApplied || to == schema.InterventionStateFailed
	case schema.InterventionStateAcknowledged:
		return to == schema.InterventionStateApplied || to == schema.InterventionStateFailed
	default:
		return false
	}
}

// CreateIntervention stores a request, in state requested, by reader's
// principal on run p.RunID and appends intervention_requested in the same
// IMMEDIATE transaction, in which everything that allows it is decided:
//
//   - the requester must be an enabled human principal (ErrNotHuman);
//   - the run must be one reader may see (ErrRunNotFound otherwise, the answer
//     a missing run gets), whoever reader is: operators get no exemption;
//   - the run, and its session, must not have ended (ErrInvalidTransition);
//   - the run's session must have declared the kind. If it did not, nothing is
//     stored, intervention_unsupported is audited with the requester (a
//     refusal that rolled back would leave no trace) and the error is
//     ErrInterventionUnsupported;
//   - fewer than MaxRequestedInterventions may be requested (ErrTooManyInterventions).
//
// A refused request stores no intervention.
func (s *Store) CreateIntervention(ctx context.Context, actor string, p InterventionParams, reader Reader, now time.Time) (Intervention, error) {
	now = storedTime(now)
	var (
		out     Intervention
		refused error
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		run, err := runVisibleTx(ctx, tx, p.RunID, reader)
		if err != nil {
			return err
		}
		var kind PrincipalKind
		switch err := tx.QueryRowContext(ctx,
			"SELECT kind FROM principals WHERE id = ? AND disabled_at IS NULL", reader.PrincipalID).Scan(&kind); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotHuman
		case err != nil:
			return fmt.Errorf("store: create intervention: read requester %d: %w", reader.PrincipalID, err)
		}
		if kind != PrincipalHuman {
			return ErrNotHuman
		}
		sess, err := sessionByIDTx(ctx, tx, run.SessionID)
		if err != nil {
			return fmt.Errorf("store: create intervention: run %d: %w", run.ID, err)
		}
		// A run that has ended, or whose session has, takes no more: nothing
		// would ever poll for the request, and it would wait for good. This is
		// decided before the kind, so asking an ended run for what it never
		// declared writes nothing, not even the refusal's audit row.
		if schema.RunStateTerminal(run.State) || sess.EndedAt != nil {
			return ErrInvalidTransition
		}
		detail := fmt.Sprintf("run=%d kind=%s requested_by=%d", run.ID, p.Kind, reader.PrincipalID)
		if !slices.Contains(sess.Interventions, p.Kind) {
			refused = ErrInterventionUnsupported
			return appendAuditEventTx(ctx, tx, actor, AuditInterventionUnsupported, runSubject(run.ID), detail, now)
		}
		open, err := requestedInterventionsTx(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if open >= MaxRequestedInterventions {
			return ErrTooManyInterventions
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO interventions (run_id, kind, body, state, requested_by, requested_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			run.ID, string(p.Kind), p.Body, string(schema.InterventionStateRequested), reader.PrincipalID, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: create intervention: insert: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create intervention: id: %w", err)
		}
		out = Intervention{ID: id, RunID: run.ID, ChannelID: run.ChannelID, Kind: p.Kind, Body: p.Body,
			State: schema.InterventionStateRequested, RequestedBy: reader.PrincipalID, RequestedAt: now}
		return appendAuditEventTx(ctx, tx, actor, AuditInterventionRequested, interventionSubject(id), detail, now)
	})
	if err != nil {
		return Intervention{}, err
	}
	if refused != nil {
		return Intervention{}, refused
	}
	return out, nil
}

// AckIntervention records the adapter's answer to intervention id: it moves to
// state to, with reason as the adapter's explanation, and intervention_answered
// is appended in the same IMMEDIATE transaction. The intervention is read and
// written in that transaction, so two answers cannot both start from the same
// state.
//
// The intervention must be on a run credentialID owns that reader may see
// (ErrInterventionNotFound otherwise, the answer a stranger gets), and the
// credential must still be able to report on the run (reportableRunTx, the
// rule a run's own reports are under, so an agent that may no longer speak to
// the run's audience may not answer for it either). The move must be one of
// InterventionMoveAllowed (ErrInvalidTransition otherwise, writing nothing).
// A run that has ended may still have its interventions answered: the adapter
// reports on the work after it is done. The run itself is never changed here.
func (s *Store) AckIntervention(ctx context.Context, actor string, id, credentialID int64, reader Reader, to schema.InterventionState, reason string, now time.Time) (Intervention, error) {
	now = storedTime(now)
	var out Intervention
	err := s.withImmediateTx(ctx, func(tx execer) error {
		iv, err := ownedInterventionTx(ctx, tx, id, credentialID, reader)
		if err != nil {
			return err
		}
		switch _, err := reportableRunTx(ctx, tx, iv.RunID, credentialID, reader); {
		case errors.Is(err, ErrRunNotFound):
			return ErrInterventionNotFound
		case err != nil:
			return err
		}
		if !InterventionMoveAllowed(iv.State, to) {
			return ErrInvalidTransition
		}
		// Never before the request, whatever the clock did, which
		// InterventionV1.Validate refuses.
		answered := now
		if answered.Before(iv.RequestedAt) {
			answered = iv.RequestedAt
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE interventions SET state = ?, reason = ?, answered_at = ? WHERE id = ? AND state = ?",
			string(to), reason, answered.UnixMilli(), iv.ID, string(iv.State))
		if err != nil {
			return fmt.Errorf("store: answer intervention %d: %w", iv.ID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: answer intervention %d: %w", iv.ID, err)
		} else if n != 1 {
			return fmt.Errorf("store: answer intervention %d: affected %d rows", iv.ID, n)
		}
		iv.State, iv.Reason, iv.AnsweredAt = to, reason, &answered
		out = iv
		return appendAuditEventTx(ctx, tx, actor, AuditInterventionAnswered, interventionSubject(iv.ID),
			fmt.Sprintf("run=%d kind=%s state=%s", iv.RunID, iv.Kind, to), now)
	})
	if err != nil {
		return Intervention{}, err
	}
	return out, nil
}
