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

// Runs and their activity (issue #266, P3 epic #263, decisions D8 and D9). A
// run is one piece of work an agent session reports: it belongs to one
// channel, may answer one message in it, and moves through the fixed
// lifecycle of schema.RunTransitionAllowed. Activity is the stream of what the
// run did. The server records what the agent reports and never invents a
// state or an outcome: nothing here changes a run when its session goes
// stale, offline or ends.
//
// Who may see a run is decided in audience.go, beside the message rule it is
// built on, and every read of the runs and run_activity tables is there
// (TestOnlyAudienceGoReadsRunRows). This file only writes; where a write must
// first read the run it changes, it asks audience.go, inside its transaction.
//
// Nothing here reads a clock of its own: the caller passes the time.

// Audit actions written by run changes. Nothing is written for an activity
// entry or for a transition between open states: those are the run's own
// stream, not oversight.
const (
	AuditRunStarted = "run_started"
	AuditRunEnded   = "run_ended"
)

// The bounds on what one session and one run may hold (issue #388). Nothing
// else stops a misbehaving runtime from growing the runs and run_activity
// tables, because the runs.manage rate limit is optional in a manifest. They
// apply to every agent.
const (
	// MaxOpenRunsPerSession bounds the runs of one session that have not
	// ended. Ended runs do not count, so ending one frees a slot.
	MaxOpenRunsPerSession = 32
	// MaxActivityPerRun bounds the activity entries of one run.
	MaxActivityPerRun = 10000
)

var (
	// ErrRunNotFound is returned when no run the reader may see, and (for a
	// write) that the credential owns, has the id. A missing run, a run the
	// reader cannot see and a run of another credential are one error: run
	// ids are sequential and any difference would let a caller probe them.
	ErrRunNotFound = errors.New("store: run not found")
	// ErrRunAnchorNotFound is returned when a run's message_id names no
	// message of the run's channel that the agent may see. A message that
	// does not exist, one in another channel and one scoped away from the
	// agent are one error, for the same reason.
	ErrRunAnchorNotFound = errors.New("store: run message not found")
	// ErrInvalidTransition is returned when a state change is not in the
	// lifecycle, and for activity on a run that has ended. Nothing is written.
	ErrInvalidTransition = errors.New("store: invalid run transition")
	// ErrTooManyRuns is returned by CreateRun when the session already has
	// MaxOpenRunsPerSession runs that have not ended. Nothing is written.
	ErrTooManyRuns = errors.New("store: too many open runs for this session")
	// ErrSessionOffline is returned by CreateRun, AppendRunActivity and
	// SetRunState (to a state that is not terminal) when the run's session is
	// offline (an ended session is offline too). Offline sessions are outside
	// the credential's live-session bound, so without this each would stay an
	// uncounted channel for new runs and activity (issue #392). It keeps
	// reports on the credential's live sessions; it does not bound the runs
	// already open on offline or ended ones (issue #402). A heartbeat revives
	// the session; ending a run is always allowed. Nothing is written.
	ErrSessionOffline = errors.New("store: session is offline")
	// ErrTooMuchActivity is returned by AppendRunActivity when the run already
	// holds MaxActivityPerRun entries. Nothing is written.
	ErrTooMuchActivity = errors.New("store: too much activity for this run")
)

// Run is one row of the runs table. MessageID is 0 for a run that answers no
// message; Link and Outcome are "" when absent; EndedAt is nil until the run
// reaches a terminal state.
type Run struct {
	ID          int64
	SessionID   int64
	PrincipalID int64
	ChannelID   int64
	MessageID   int64
	State       schema.RunState
	Task        string
	Link        string
	StartedAt   time.Time
	EndedAt     *time.Time
	Outcome     string
}

// Activity is one entry of a run's activity. PrincipalID is the run's agent,
// read from the run. Data is nil when the entry has none.
type Activity struct {
	ID          int64
	RunID       int64
	PrincipalID int64
	Kind        schema.ActivityKind
	Summary     string
	Outcome     string
	Data        json.RawMessage
	At          time.Time
}

// RunParams is what an agent reports when it starts a run. The session, the
// credential and the principal come from the caller's authentication and the
// URL; the channel is resolved from the request's name by the caller.
type RunParams struct {
	SessionID    int64
	CredentialID int64
	ChannelID    int64
	MessageID    int64
	Task         string
	Link         string
}

func runSubject(id int64) string { return fmt.Sprintf("run:%d", id) }

// storedTime is t as it reads back from the table: whole milliseconds, in
// the zone time.UnixMilli gives. What a write returns is then what a read of
// the same row returns.
func storedTime(t time.Time) time.Time { return time.UnixMilli(t.UnixMilli()) }

// CreateRun stores a new run in state queued for session p.SessionID and
// appends run_started in the same IMMEDIATE transaction, in which everything
// that allows it is checked again, so nothing that changed after the
// caller's checks can slip through:
//
//   - the session must exist, not have ended, belong to credential
//     p.CredentialID and to reader's principal (ErrNotFound otherwise, the
//     same answer as every session route gives);
//   - the session must not be offline (ErrSessionOffline), checked once the
//     caller is known to own it so that it tells a stranger nothing;
//   - reader must be, now, a member of the channel and an enabled principal
//     (ErrNotChannelMember);
//   - a message the run answers must be in the channel and visible to reader
//     by the message rule (ErrRunAnchorNotFound);
//   - the session must hold fewer than MaxOpenRunsPerSession runs that have
//     not ended (ErrTooManyRuns). The count and the insert share this
//     transaction, so concurrent creates cannot pass the bound together.
//
// reader is the agent itself; the run is then visible to it by RunVisible's
// rule. A refused run stores nothing.
func (s *Store) CreateRun(ctx context.Context, actor string, p RunParams, reader Reader, now time.Time) (Run, error) {
	now = storedTime(now)
	var out Run
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var principalID int64
		var sessionState string
		switch err := tx.QueryRowContext(ctx,
			"SELECT principal_id, state FROM sessions WHERE id = ? AND credential_id = ? AND ended_at IS NULL",
			p.SessionID, p.CredentialID).Scan(&principalID, &sessionState); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: create run: read session %d: %w", p.SessionID, err)
		}
		if reader.PrincipalID <= 0 || reader.PrincipalID != principalID {
			return ErrNotFound
		}
		if sessionState == string(schema.SessionStateOffline) {
			return ErrSessionOffline
		}
		usable, err := principalUsableInChannelTx(ctx, tx, p.ChannelID, principalID)
		if err != nil {
			return err
		}
		if !usable {
			return ErrNotChannelMember
		}
		var message any
		if p.MessageID > 0 {
			if err := runAnchorVisibleTx(ctx, tx, p.ChannelID, p.MessageID, reader); err != nil {
				return err
			}
			message = p.MessageID
		}
		open, err := countOpenRunsTx(ctx, tx, p.SessionID)
		if err != nil {
			return err
		}
		if open >= MaxOpenRunsPerSession {
			return ErrTooManyRuns
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO runs (session_id, principal_id, channel_id, message_id, state, task, link, started_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.SessionID, principalID, p.ChannelID, message, string(schema.RunStateQueued), p.Task, p.Link, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: create run: insert: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create run: id: %w", err)
		}
		out = Run{ID: id, SessionID: p.SessionID, PrincipalID: principalID, ChannelID: p.ChannelID, MessageID: p.MessageID,
			State: schema.RunStateQueued, Task: p.Task, Link: p.Link, StartedAt: now}
		// The detail names where the run is, never what it says: the task can
		// quote the message it answers.
		detail := fmt.Sprintf("session=%d principal=%d channel=%d", p.SessionID, principalID, p.ChannelID)
		if p.MessageID > 0 {
			detail += fmt.Sprintf(" message=%d", p.MessageID)
		}
		return appendAuditEventTx(ctx, tx, actor, AuditRunStarted, runSubject(id), detail, now)
	})
	if err != nil {
		return Run{}, err
	}
	return out, nil
}

// SetRunState moves run runID to state to and records outcome as the run's
// outcome (an empty outcome clears it: the outcome is what the agent said
// with the latest change). Entering a terminal state sets ended_at and
// appends run_ended, naming the final state, in the same transaction.
//
// The run must be one credentialID owns (through its session), that reader may
// see and whose message reader may still speak to (reportableRunTx), decided
// inside the transaction (ErrRunNotFound otherwise), and the change must be in
// the lifecycle (ErrInvalidTransition otherwise, writing
// nothing). It returns the run as it is after.
func (s *Store) SetRunState(ctx context.Context, actor string, runID, credentialID int64, reader Reader, to schema.RunState, outcome string, now time.Time) (Run, error) {
	now = storedTime(now)
	var out Run
	err := s.withImmediateTx(ctx, func(tx execer) error {
		run, err := reportableRunTx(ctx, tx, runID, credentialID, reader)
		if err != nil {
			return err
		}
		if !schema.RunTransitionAllowed(run.State, to) {
			return ErrInvalidTransition
		}
		// Ending a run is how an agent cleans up, so a terminal state is
		// allowed on an offline session; any other change is not.
		if !schema.RunStateTerminal(to) {
			if err := refuseOfflineSessionTx(ctx, tx, run.SessionID); err != nil {
				return err
			}
		}
		var ended any
		if schema.RunStateTerminal(to) {
			// Never before started_at, whatever the clock did, which
			// RunV1.Validate refuses.
			end := now
			if end.Before(run.StartedAt) {
				end = run.StartedAt
			}
			ended = end.UnixMilli()
			run.EndedAt = &end
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE runs SET state = ?, outcome = ?, ended_at = ? WHERE id = ? AND state = ?",
			string(to), outcome, ended, run.ID, string(run.State))
		if err != nil {
			return fmt.Errorf("store: set run %d state: %w", run.ID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: set run %d state: %w", run.ID, err)
		} else if n != 1 {
			return fmt.Errorf("store: set run %d state: affected %d rows", run.ID, n)
		}
		run.State = to
		run.Outcome = outcome
		out = run
		if !schema.RunStateTerminal(to) {
			return nil
		}
		return appendAuditEventTx(ctx, tx, actor, AuditRunEnded, runSubject(run.ID),
			fmt.Sprintf("state=%s session=%d principal=%d channel=%d", to, run.SessionID, run.PrincipalID, run.ChannelID), now)
	})
	if err != nil {
		return Run{}, err
	}
	return out, nil
}

// ActivityParams is one entry an agent appends. Data must already be the
// encoding the server keeps (compact JSON, as encoding/json writes it), or
// nil for none.
type ActivityParams struct {
	Kind    schema.ActivityKind
	Summary string
	Outcome string
	Data    json.RawMessage
}

// AppendRunActivity appends one entry to run runID at now. The run must be
// one credentialID owns, that reader may see and whose message reader may
// still speak to (reportableRunTx), decided inside the transaction
// (ErrRunNotFound otherwise), and must not have ended (ErrInvalidTransition).
// A run that already holds MaxActivityPerRun entries takes no more
// (ErrTooMuchActivity), counted in the same transaction as the insert.
// Nothing is audited: the entry is the record.
func (s *Store) AppendRunActivity(ctx context.Context, runID, credentialID int64, reader Reader, p ActivityParams, now time.Time) (Activity, error) {
	now = storedTime(now)
	var out Activity
	err := s.withImmediateTx(ctx, func(tx execer) error {
		run, err := reportableRunTx(ctx, tx, runID, credentialID, reader)
		if err != nil {
			return err
		}
		if schema.RunStateTerminal(run.State) {
			return ErrInvalidTransition
		}
		if err := refuseOfflineSessionTx(ctx, tx, run.SessionID); err != nil {
			return err
		}
		held, err := countRunActivityTx(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if held >= MaxActivityPerRun {
			return ErrTooMuchActivity
		}
		var data any
		if len(p.Data) > 0 {
			data = []byte(p.Data)
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO run_activity (run_id, kind, summary, outcome, data, at) VALUES (?, ?, ?, ?, ?, ?)",
			run.ID, string(p.Kind), p.Summary, p.Outcome, data, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: append activity to run %d: %w", run.ID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: append activity to run %d: %w", run.ID, err)
		}
		out = Activity{ID: id, RunID: run.ID, PrincipalID: run.PrincipalID, Kind: p.Kind, Summary: p.Summary,
			Outcome: p.Outcome, Data: p.Data, At: now}
		return nil
	})
	if err != nil {
		return Activity{}, err
	}
	return out, nil
}

// reportableRunTx is the run a report (a state change or an activity entry)
// may change, read inside the report's transaction: one credentialID owns and
// reader may see (ownedRunTx), and whose message, when it answers one, reader
// may still speak to by the rule a run is started under (runAnchorVisibleTx).
// A report reaches the run's audience as the start did, so an agent taken off
// the net since, demoted to monitor on it, or whose net has been archived may
// not report on a run it started there. Every failure is ErrRunNotFound, the
// answer a stranger gets.
func reportableRunTx(ctx context.Context, tx execer, runID, credentialID int64, reader Reader) (Run, error) {
	run, err := ownedRunTx(ctx, tx, runID, credentialID, reader)
	if err != nil {
		return Run{}, err
	}
	if run.MessageID > 0 {
		switch err := runAnchorVisibleTx(ctx, tx, run.ChannelID, run.MessageID, reader); {
		case errors.Is(err, ErrRunAnchorNotFound):
			return Run{}, ErrRunNotFound
		case err != nil:
			return Run{}, err
		}
	}
	return run, nil
}

// refuseOfflineSessionTx is ErrSessionOffline when session sessionID is
// offline, read inside the caller's transaction. A session that has ended is
// offline too (sessions.go sets both together), so a run left on one cannot
// be reported on either, though it can still be ended.
func refuseOfflineSessionTx(ctx context.Context, tx execer, sessionID int64) error {
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM sessions WHERE id = ?", sessionID).Scan(&state); err != nil {
		return fmt.Errorf("store: read session %d: %w", sessionID, err)
	}
	if state == string(schema.SessionStateOffline) {
		return ErrSessionOffline
	}
	return nil
}

// principalUsableInChannelTx reports whether principalID is, now, a member of
// channelID and an enabled principal: the two conditions every run rule
// starts with.
func principalUsableInChannelTx(ctx context.Context, tx execer, channelID, principalID int64) (bool, error) {
	var one int
	switch err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM channel_members c JOIN principals p ON p.id = c.principal_id
		 WHERE c.channel_id = ? AND c.principal_id = ? AND p.disabled_at IS NULL`, channelID, principalID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: check member %d of channel %d: %w", principalID, channelID, err)
	}
	return true, nil
}
