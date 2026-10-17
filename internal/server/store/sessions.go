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

// Sessions (issue #265, P3 epic #263, decision D1). A session is one live
// client or runtime of a principal, owned by the credential that created it.
// The server flips its state on heartbeat thresholds and never deletes a row.
// Every change is audited in the transaction that makes it.
//
// Nothing here reads a clock of its own: the caller passes the time, so the
// server's injected clock governs creation, heartbeats and the sweeper alike.

// Audit actions written by session changes. Nothing is written for stale, for
// a heartbeat or for a revival: those are the ordinary life of a session and
// would only fill the log.
const (
	AuditSessionStarted = "session_started"
	AuditSessionEnded   = "session_ended"
	AuditSessionOffline = "session_offline"
)

// Why a session ended, as recorded in the session_ended event.
const (
	// SessionEndedByClient: the session's own credential asked (DELETE).
	SessionEndedByClient = "client"
	// SessionEndedByRevocation: the credential was revoked or rotated.
	SessionEndedByRevocation = "credential_revoked"
	// SessionEndedPrincipalDisabled: the principal was disabled.
	SessionEndedPrincipalDisabled = "principal_disabled"
	// SessionEndedExpiry: the credential's expiry passed; the
	// sweeper ends the session, as nothing else runs when that happens.
	SessionEndedExpiry = "credential_expired"
)

// MaxLiveSessionsPerCredential bounds the sessions per credential that are
// online or stale; offline and ended ones do not count. The bound is per credential, not per principal: one person on
// two devices is two credentials with a budget each.
const MaxLiveSessionsPerCredential = 16

// ErrTooManySessions is returned by CreateSession, and by HeartbeatSession for
// a heartbeat that would revive an offline session, when the credential
// already holds MaxLiveSessionsPerCredential sessions that are online or stale.
var ErrTooManySessions = errors.New("store: too many live sessions for this credential")

// Session is one row of the sessions table with the owning principal's kind
// and name, which are the principal's and never stored on the session.
type Session struct {
	ID           int64
	PrincipalID  int64
	CredentialID int64
	Kind         PrincipalKind
	Name         string
	Client       string
	Runtime      string
	Model        string
	Host         string
	// Interventions is never nil.
	Interventions []schema.InterventionKind
	StartedAt     time.Time
	LastSeenAt    time.Time
	// EndedAt is nil while the session has not ended.
	EndedAt *time.Time
	State   schema.SessionState
}

// SessionParams is what a client reports when it creates a session. The
// principal and the credential come from the caller's authentication; the
// kind is read from the principal row and is not a parameter.
type SessionParams struct {
	PrincipalID   int64
	CredentialID  int64
	Client        string
	Runtime       string
	Model         string
	Host          string
	Interventions []schema.InterventionKind
}

func sessionSubject(id int64) string { return fmt.Sprintf("session:%d", id) }

func sessionDetail(principalID, credentialID int64) string {
	return fmt.Sprintf("principal=%d credential=%d", principalID, credentialID)
}

const sessionColumns = `s.id, s.principal_id, s.credential_id, p.kind, p.name, s.client, s.runtime, s.model, s.host,
	s.interventions, s.started_at, s.last_seen_at, s.ended_at, s.state`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var (
		sess                    Session
		kind, interventions, st string
		started, lastSeen       int64
		ended                   sql.NullInt64
	)
	if err := row.Scan(&sess.ID, &sess.PrincipalID, &sess.CredentialID, &kind, &sess.Name, &sess.Client,
		&sess.Runtime, &sess.Model, &sess.Host, &interventions, &started, &lastSeen, &ended, &st); err != nil {
		return Session{}, err
	}
	sess.Kind = PrincipalKind(kind)
	sess.StartedAt = time.UnixMilli(started)
	sess.LastSeenAt = time.UnixMilli(lastSeen)
	if ended.Valid {
		t := time.UnixMilli(ended.Int64)
		sess.EndedAt = &t
	}
	sess.State = schema.SessionState(st)
	sess.Interventions = []schema.InterventionKind{}
	if err := json.Unmarshal([]byte(interventions), &sess.Interventions); err != nil {
		return Session{}, fmt.Errorf("decode interventions: %w", err)
	}
	if sess.Interventions == nil {
		// A stored JSON null: still a list on the wire.
		sess.Interventions = []schema.InterventionKind{}
	}
	return sess, nil
}

// sessionByIDTx reads one session inside tx.
func sessionByIDTx(ctx context.Context, tx execer, id int64) (Session, error) {
	sess, err := scanSession(tx.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions s JOIN principals p ON p.id = s.principal_id WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: read session %d: %w", id, err)
	}
	return sess, nil
}

// OwnedSession returns session id if it belongs to credentialID and has not
// ended, and ErrNotFound for a session that does not exist, belongs to another
// credential, or has ended: the three are not told apart, as for every session
// route. It changes nothing; a session that is stale or offline is returned as
// it is.
func (s *Store) OwnedSession(ctx context.Context, id, credentialID int64) (Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions s JOIN principals p ON p.id = s.principal_id
		 WHERE s.id = ? AND s.credential_id = ? AND s.ended_at IS NULL`, id, credentialID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: read session %d: %w", id, err)
	}
	return sess, nil
}

// CreateSession starts a session for the credential p.CredentialID and
// appends session_started in the same transaction. The credential is checked
// again inside it: it must belong to p.PrincipalID, not be revoked or
// expired, and its principal must not be disabled. Revoking and disabling end
// sessions in their own transactions, so a request that authenticated just
// before a revocation cannot start a session after it and leave one live
// behind a dead credential. Such a request gets ErrCredentialInvalid.
//
// It returns ErrTooManySessions, writing nothing, when the credential already
// holds MaxLiveSessionsPerCredential sessions that are online or stale.
func (s *Store) CreateSession(ctx context.Context, actor string, p SessionParams, now time.Time) (Session, error) {
	now = now.Truncate(time.Millisecond)
	interventions := p.Interventions
	if interventions == nil {
		interventions = []schema.InterventionKind{}
	}
	encoded, err := json.Marshal(interventions)
	if err != nil {
		return Session{}, fmt.Errorf("store: encode interventions: %w", err)
	}
	var out Session
	err = s.withImmediateTx(ctx, func(tx execer) error {
		var (
			owner                          int64
			expiresAt, revokedAt, disabled sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx,
			`SELECT c.principal_id, c.expires_at, c.revoked_at, pr.disabled_at
			 FROM credentials c JOIN principals pr ON pr.id = c.principal_id
			 WHERE c.id = ?`, p.CredentialID,
		).Scan(&owner, &expiresAt, &revokedAt, &disabled); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrCredentialInvalid
		case err != nil:
			return fmt.Errorf("store: create session: read credential: %w", err)
		}
		if owner != p.PrincipalID || revokedAt.Valid || disabled.Valid ||
			(expiresAt.Valid && expiresAt.Int64 <= credentialNow().UnixMilli()) {
			return ErrCredentialInvalid
		}
		var live int
		if err := tx.QueryRowContext(ctx,
			// An offline session does not count. A client that crashes and
			// starts a new session each time must not be locked out by the ones
			// it left behind, which go offline and are never deleted. Reviving
			// one is held to the same bound, in HeartbeatSession.
			"SELECT COUNT(*) FROM sessions WHERE credential_id = ? AND ended_at IS NULL AND state <> 'offline'", p.CredentialID,
		).Scan(&live); err != nil {
			return fmt.Errorf("store: create session: count live sessions: %w", err)
		}
		if live >= MaxLiveSessionsPerCredential {
			return ErrTooManySessions
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (principal_id, credential_id, client, runtime, model, host, interventions, started_at, last_seen_at, state)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'online')`,
			p.PrincipalID, p.CredentialID, p.Client, p.Runtime, p.Model, p.Host, string(encoded), now.UnixMilli(), now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: create session: insert: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create session: id: %w", err)
		}
		if out, err = sessionByIDTx(ctx, tx, id); err != nil {
			return err
		}
		return appendAuditEventTx(ctx, tx, actor, AuditSessionStarted, sessionSubject(id),
			sessionDetail(p.PrincipalID, p.CredentialID)+fmt.Sprintf(" client=%q", p.Client), now)
	})
	if err != nil {
		return Session{}, err
	}
	return out, nil
}

// HeartbeatSession records that session id, owned by credentialID, was heard
// from at now: last_seen_at moves forward and the state is online, whether it
// was online, stale or offline before (the same row and the same id). It
// returns the session as it is after, and ErrNotFound for a session that does
// not exist, belongs to another credential, or has ended: the three are not
// told apart. Reviving an offline session is refused with ErrTooManySessions,
// changing nothing, when the credential already holds
// MaxLiveSessionsPerCredential sessions that are online or stale. Nothing is
// audited.
func (s *Store) HeartbeatSession(ctx context.Context, id, credentialID int64, now time.Time) (Session, error) {
	now = now.Truncate(time.Millisecond)
	var out Session
	err := s.withImmediateTx(ctx, func(tx execer) error {
		// A revival counts against the bound as a new session would;
		// otherwise a credential could hold any number online by letting
		// sessions go offline, creating more, then reviving the old ones.
		var offline int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sessions WHERE id = ? AND credential_id = ? AND ended_at IS NULL AND state = 'offline'",
			id, credentialID).Scan(&offline); err != nil {
			return fmt.Errorf("store: heartbeat session: %w", err)
		}
		if offline == 1 {
			var live int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM sessions WHERE credential_id = ? AND ended_at IS NULL AND state <> 'offline'",
				credentialID).Scan(&live); err != nil {
				return fmt.Errorf("store: heartbeat session: count live sessions: %w", err)
			}
			if live >= MaxLiveSessionsPerCredential {
				return ErrTooManySessions
			}
		}
		// max(): a clock that stepped back must not put last_seen_at before
		// started_at, which SessionV1.Validate refuses.
		res, err := tx.ExecContext(ctx,
			`UPDATE sessions SET last_seen_at = max(started_at, ?), state = 'online'
			 WHERE id = ? AND credential_id = ? AND ended_at IS NULL`, now.UnixMilli(), id, credentialID)
		if err != nil {
			return fmt.Errorf("store: heartbeat session: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: heartbeat session: %w", err)
		} else if n != 1 {
			return ErrNotFound
		}
		out, err = sessionByIDTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return out, nil
}

// EndSession ends session id, owned by credentialID, at the client's request:
// ended_at is set, the state is offline for good, and session_ended with cause
// client is appended in the same transaction. It returns ErrNotFound for a
// session that does not exist, belongs to another credential, or has already
// ended.
func (s *Store) EndSession(ctx context.Context, actor string, id, credentialID int64, now time.Time) error {
	now = now.Truncate(time.Millisecond)
	var runs []EndedRun
	err := s.withImmediateTx(ctx, func(tx execer) error {
		runs = nil
		ended, err := endSessionsTx(ctx, tx, actor, "id = ? AND credential_id = ?", []any{id, credentialID}, SessionEndedByClient, now, &runs)
		if err != nil {
			return err
		}
		if ended == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.announceRunsEnded(runs)
	return nil
}

// endCredentialSessionsTx ends every session that has not ended of credential
// credentialID, one session_ended event each with the given cause. It is
// called inside the transactions that revoke a credential, so there is no
// moment at which the credential is dead and its session is live.
func endCredentialSessionsTx(ctx context.Context, tx execer, actor string, credentialID int64, cause string, now time.Time, ended *[]EndedRun) error {
	_, err := endSessionsTx(ctx, tx, actor, "credential_id = ?", []any{credentialID}, cause, now, ended)
	return err
}

// endPrincipalSessionsTx is endCredentialSessionsTx for every session of a
// principal, whatever credential it was made with: disabling a principal ends
// them all.
func endPrincipalSessionsTx(ctx context.Context, tx execer, actor string, principalID int64, cause string, now time.Time, ended *[]EndedRun) error {
	_, err := endSessionsTx(ctx, tx, actor, "principal_id = ?", []any{principalID}, cause, now, ended)
	return err
}

// endSessionsTx ends the sessions that match where, which is a fixed condition
// written by this package (never request text), and have not ended. It sets
// ended_at and the offline state together and writes one session_ended event
// per session, and fails the session's open runs (endSessionRunsTx). The
// caller must hold the write lock (withImmediateTx) so the set it reads is the
// set it ends. It returns how many it ended, and appends the runs that ended
// with them to *endedRuns, for the caller to announce after the commit.
func endSessionsTx(ctx context.Context, tx execer, actor, where string, args []any, cause string, now time.Time, endedRuns *[]EndedRun) (int, error) {
	type live struct{ id, principalID, credentialID int64 }
	rows, err := tx.QueryContext(ctx,
		"SELECT id, principal_id, credential_id FROM sessions WHERE ended_at IS NULL AND "+where+" ORDER BY id", args...) // #nosec G202 -- where is a constant of this package
	if err != nil {
		return 0, fmt.Errorf("store: list live sessions: %w", err)
	}
	var found []live
	for rows.Next() {
		var l live
		if err := rows.Scan(&l.id, &l.principalID, &l.credentialID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: scan session: %w", err)
		}
		found = append(found, l)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: list live sessions: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: list live sessions: %w", err)
	}
	for _, l := range found {
		// max(): ended_at is never before started_at, whatever the clocks did.
		res, err := tx.ExecContext(ctx,
			"UPDATE sessions SET ended_at = max(started_at, ?), state = 'offline' WHERE id = ? AND ended_at IS NULL", now.UnixMilli(), l.id)
		if err != nil {
			return 0, fmt.Errorf("store: end session %d: %w", l.id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, fmt.Errorf("store: end session %d: %w", l.id, err)
		} else if n != 1 {
			return 0, fmt.Errorf("store: end session %d: affected %d rows", l.id, n)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditSessionEnded, sessionSubject(l.id),
			"cause="+cause+" "+sessionDetail(l.principalID, l.credentialID), now); err != nil {
			return 0, err
		}
		// The agent can no longer see this session's runs, so they end with
		// it, in the same transaction (issue #402).
		if err := endSessionRunsTx(ctx, tx, actor, l.id, now, endedRuns); err != nil {
			return 0, err
		}
	}
	return len(found), nil
}

// SweepResult is what one SweepSessions pass changed.
type SweepResult struct {
	// Stale is how many sessions went from online to stale.
	Stale int
	// Offline is how many went offline (from online or from stale).
	Offline int
	// Expired is how many sessions were ended because their credential expired.
	Expired int
}

// SweepSessions applies the liveness thresholds at now in one short
// transaction: a session that has not ended and was last seen more than
// offline ago becomes offline (with a session_offline event, once, because
// only a session that was not offline can become it), and an online one last
// seen more than stale ago becomes stale (no event). A session is judged only
// by last_seen_at, so a revived session starts over. Every session that has
// not ended whose credential expired at or before now is ended, with cause
// credential_expired, whatever its state; a credential without an expiry is
// never a reason. Rows are never deleted.
func (s *Store) SweepSessions(ctx context.Context, now time.Time, stale, offline time.Duration) (SweepResult, error) {
	now = now.Truncate(time.Millisecond)
	var out SweepResult
	var runs []EndedRun
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out = SweepResult{}
		runs = nil
		type going struct{ id, principalID, credentialID int64 }
		rows, err := tx.QueryContext(ctx,
			`SELECT id, principal_id, credential_id FROM sessions
			 WHERE ended_at IS NULL AND state <> 'offline' AND last_seen_at < ? ORDER BY id`,
			now.Add(-offline).UnixMilli())
		if err != nil {
			return fmt.Errorf("store: sweep sessions: %w", err)
		}
		var found []going
		for rows.Next() {
			var g going
			if err := rows.Scan(&g.id, &g.principalID, &g.credentialID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: sweep sessions: scan: %w", err)
			}
			found = append(found, g)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: sweep sessions: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: sweep sessions: %w", err)
		}
		for _, g := range found {
			if _, err := tx.ExecContext(ctx,
				"UPDATE sessions SET state = 'offline' WHERE id = ? AND ended_at IS NULL AND state <> 'offline'", g.id); err != nil {
				return fmt.Errorf("store: sweep sessions: offline %d: %w", g.id, err)
			}
			if err := appendAuditEventTx(ctx, tx, "system", AuditSessionOffline, sessionSubject(g.id),
				sessionDetail(g.principalID, g.credentialID), now); err != nil {
				return err
			}
		}
		out.Offline = len(found)
		res, err := tx.ExecContext(ctx,
			// state <> 'offline' is implied by state = 'online', but SQLite
			// only uses the partial index sessions_live_by_last_seen when the
			// query repeats its predicate; without it this is a full scan of a
			// table that never shrinks, under the write lock, every sweep.
			"UPDATE sessions SET state = 'stale' WHERE ended_at IS NULL AND state <> 'offline' AND state = 'online' AND last_seen_at < ?",
			now.Add(-stale).UnixMilli())
		if err != nil {
			return fmt.Errorf("store: sweep sessions: stale: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: sweep sessions: stale: %w", err)
		}
		out.Stale = int(n)
		// Last, so a session that went offline above and whose credential has
		// also expired gets both events, in the order they happened. The
		// subquery finds the expired credentials and sessions_by_credential
		// finds their sessions.
		out.Expired, err = endSessionsTx(ctx, tx, "system",
			"credential_id IN (SELECT id FROM credentials WHERE expires_at IS NOT NULL AND expires_at <= ?)",
			[]any{now.UnixMilli()}, SessionEndedExpiry, now, &runs)
		return err
	})
	if err != nil {
		return SweepResult{}, err
	}
	s.announceRunsEnded(runs)
	return out, nil
}

// ListChannelSessions returns the sessions that have not ended, were last seen
// no longer than within before now, and belong to a current member of
// channelID, ordered by id. The result is never nil. Whether the caller may
// see the channel at all is the handler's question, asked before this one.
func (s *Store) ListChannelSessions(ctx context.Context, channelID int64, now time.Time, within time.Duration) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionColumns+`
		 FROM sessions s
		 JOIN channel_members m ON m.principal_id = s.principal_id AND m.channel_id = ?
		 JOIN principals p ON p.id = s.principal_id
		 WHERE s.ended_at IS NULL AND s.last_seen_at >= ?
		 ORDER BY s.id`, channelID, now.Add(-within).UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list sessions: scan: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	return out, nil
}
