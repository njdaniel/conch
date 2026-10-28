package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Temporary servers and password-only operators (issues #866 and #875,
// docs/adr/ADR-011-temporary-servers.md, docs/adr/ADR-012-optional-operator-totp.md).
//
// Two settings are fixed when a server starts: its mode, normal or temporary
// (ADR-011 item 1), and whether an operator's authenticator is required or
// optional (ADR-012 item 1). Temporary mode turns optional on (ADR-012 item
// 2): password-only is allowed when the server is temporary or its operator
// TOTP is optional, and then an operator's second factor is asked for only
// when one is enrolled (ADR-011 item 2.1).
//
// The store keeps both as one row (migration 36): the mode the server last
// started in, and whether password-only was allowed. StartMode, called once
// at start before anything is served, records them. Every store-side check
// that lets a password-only operator through reads the stored flag again in
// its own transaction (storedPasswordOnlyTx), so a server and its store never
// disagree into a weaker check: if either says required, the code is asked
// for.
//
// Turning password-only off ends what it made (ADR-011 item 5, as ADR-012
// item 4 amends it): every browser session of every operator, every
// recent-authentication proof, every pending login and every unfinished
// setup credential, in the one transaction that records the change. Script
// credentials are untouched, and no authentication generation moves, since
// a generation covers script credentials too. Turning it on, or a change of
// mode alone, revokes nothing.

// Mode is a server's mode as conchd is configured with it and the store
// keeps it. It is not a wire value: /healthz and whoami say only
// schema.ServerModeTemporary, and say nothing in normal mode.
type Mode string

// The modes. Exactly these, lower case (ADR-011 item 1).
const (
	ModeNormal    Mode = "normal"
	ModeTemporary Mode = "temporary"
)

// ParseMode reads a CONCHD_MODE value: empty is normal, so every existing
// server is unchanged, "normal" and "temporary" are themselves, and anything
// else is an error, which stops conchd at start (ADR-011 item 1). The error
// quotes the value: it is configuration, not a secret.
func ParseMode(raw string) (Mode, error) {
	switch Mode(raw) {
	case "", ModeNormal:
		return ModeNormal, nil
	case ModeTemporary:
		return ModeTemporary, nil
	default:
		return "", fmt.Errorf("%q is not a server mode; use temporary or normal (or leave it unset for normal)", raw)
	}
}

// OperatorTOTP is whether an operator needs a confirmed authenticator to
// sign in to the browser (ADR-012 item 1), as conchd is configured with it.
// It is not a wire value.
type OperatorTOTP string

// The settings. Exactly these, lower case.
const (
	OperatorTOTPRequired OperatorTOTP = "required"
	OperatorTOTPOptional OperatorTOTP = "optional"
)

// ParseOperatorTOTP reads a CONCHD_OPERATOR_TOTP value for a server of mode.
// Empty is required on a normal server, as today, and optional on a
// temporary one, so temporary servers start as they did; "required" and
// "optional" are themselves; and anything else is an error, which stops
// conchd at start. So is an explicit "required" on a temporary server: the
// two settings may not contradict each other (ADR-012 item 2).
func ParseOperatorTOTP(raw string, mode Mode) (OperatorTOTP, error) {
	switch OperatorTOTP(raw) {
	case "":
		if mode == ModeTemporary {
			return OperatorTOTPOptional, nil
		}
		return OperatorTOTPRequired, nil
	case OperatorTOTPOptional:
		return OperatorTOTPOptional, nil
	case OperatorTOTPRequired:
		if mode == ModeTemporary {
			return "", errors.New(`"required" contradicts CONCHD_MODE=temporary, which lets operators sign in with a password alone; unset it, or set CONCHD_MODE=normal`)
		}
		return OperatorTOTPRequired, nil
	default:
		return "", fmt.Errorf("%q is not an operator TOTP setting; use optional or required (or leave it unset)", raw)
	}
}

// PasswordOnlyAllowed reports whether a server of mode with setting lets an
// operator with no authenticator sign in with a password alone (ADR-012
// item 2).
func PasswordOnlyAllowed(mode Mode, setting OperatorTOTP) bool {
	return mode == ModeTemporary || setting == OperatorTOTPOptional
}

// AuditServerMode is the settings a server started with, written on the
// first start (detail mode, password_only and first_start=true) and on every
// start where either differs from the last one's (detail each as
// old->new, and, when password-only stopped being allowed, what was ended
// and how many operators are left without an authenticator, as counts).
// Actor system, subject server.
const AuditServerMode = "server_mode"

// ModeStart is what StartMode did.
type ModeStart struct {
	// Previous and PreviousPasswordOnly are what the server last started
	// with; Previous is empty on the first start.
	Previous             Mode
	PreviousPasswordOnly bool
	// Changed says either differs from last time, or there was no last time.
	Changed bool
	// Ended says password-only went from allowed to not allowed, and what
	// follows was ended (ADR-011 item 5); all zero otherwise.
	// SessionsRevoked are operators' browser sessions, ProofsCleared
	// credentials whose recent-authentication proof was cleared,
	// PendingLoginsSpent pending logins and SetupCredentialsRevoked setup
	// credentials. OperatorsWithoutAuthenticator are the enabled, unhidden
	// operators (owners among them) left with a password and no confirmed
	// authenticator, who cannot sign in to the browser until they are set up
	// again on the host (ADR-012 item 4).
	Ended                                                                       bool
	SessionsRevoked, ProofsCleared, PendingLoginsSpent, SetupCredentialsRevoked int64
	OperatorsWithoutAuthenticator                                               int64
}

// modeRevokedReason is the reason of the credential_revoked rows written
// when password-only stops being allowed.
const modeRevokedReason = "password_only_ended"

// StartMode records mode and passwordOnly (PasswordOnlyAllowed of the
// server's settings) as what the server starts with, in one IMMEDIATE
// transaction, before the server accepts traffic:
//
//   - on the first start (no row) it writes the row and audits server_mode;
//   - with the same mode and flag as last time it writes nothing;
//   - on any other change it updates the row and audits server_mode, naming
//     the old and new mode and flag, so turning password-only on is recorded
//     too;
//   - when, and only when, password-only goes from allowed to not allowed,
//     it also revokes every browser session (a sign-in session,
//     signInSessionSQL) of every operator, whether or not they have since
//     enrolled an authenticator; clears every recent-authentication proof;
//     spends every pending login; revokes every setup credential still live;
//     and counts the operators left with a password and no authenticator.
//     Each revocation is audited credential_revoked with reason
//     password_only_ended and ends the credential's sessions, as any
//     revocation does. Script credentials are untouched, and no
//     authentication generation moves. The count is not a refusal: the host
//     re-enrols each of them with conchd operator setup.
//
// Either all of it commits or none of it does. A mode that is not one of the
// two, or temporary mode with password-only off, is refused before anything
// is read.
func (s *Store) StartMode(ctx context.Context, mode Mode, passwordOnly bool, now time.Time) (ModeStart, error) {
	switch {
	case mode != ModeNormal && mode != ModeTemporary:
		return ModeStart{}, fmt.Errorf("store: start mode: %q is not a server mode", mode)
	case mode == ModeTemporary && !passwordOnly:
		return ModeStart{}, errors.New("store: start mode: a temporary server always allows password-only operators")
	}
	now = now.Truncate(time.Millisecond)
	var (
		out  ModeStart
		runs []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = ModeStart{}, nil
		var stored string
		switch err := tx.QueryRowContext(ctx, "SELECT mode, password_only FROM server_mode WHERE id = 1").Scan(&stored, &out.PreviousPasswordOnly); {
		case errors.Is(err, sql.ErrNoRows):
			out.Changed = true
			if _, err := tx.ExecContext(ctx, "INSERT INTO server_mode (id, mode, password_only, updated_at) VALUES (1, ?, ?, ?)",
				string(mode), passwordOnly, now.UnixMilli()); err != nil {
				return fmt.Errorf("store: start mode: %w", err)
			}
			// Every server's trail says what it began with.
			return appendAuditEventTx(ctx, tx, hostActor, AuditServerMode, "server",
				fmt.Sprintf("mode=%s password_only=%t first_start=true", mode, passwordOnly), now)
		case err != nil:
			return fmt.Errorf("store: start mode: %w", err)
		}
		out.Previous = Mode(stored)
		if out.Previous == mode && out.PreviousPasswordOnly == passwordOnly {
			return nil
		}
		out.Changed = true
		detail := fmt.Sprintf("mode=%s->%s password_only=%t->%t", out.Previous, mode, out.PreviousPasswordOnly, passwordOnly)
		if out.PreviousPasswordOnly && !passwordOnly {
			if err := endPasswordOnlyTx(ctx, tx, &out, now, &runs); err != nil {
				return err
			}
			detail += fmt.Sprintf(" sessions_revoked=%d proofs_cleared=%d pending_logins_spent=%d setup_credentials_revoked=%d operators_without_authenticator=%d",
				out.SessionsRevoked, out.ProofsCleared, out.PendingLoginsSpent, out.SetupCredentialsRevoked, out.OperatorsWithoutAuthenticator)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE server_mode SET mode = ?, password_only = ?, updated_at = ? WHERE id = 1",
			string(mode), passwordOnly, now.UnixMilli()); err != nil {
			return fmt.Errorf("store: start mode: %w", err)
		}
		return appendAuditEventTx(ctx, tx, hostActor, AuditServerMode, "server", detail, now)
	})
	if err != nil {
		return ModeStart{}, err
	}
	s.announceRunsEnded(runs)
	return out, nil
}

// endPasswordOnlyTx ends, in the caller's transaction, what a server that
// allowed password-only operators may have made with a password alone
// (ADR-011 item 5, ADR-012 item 4), counting into out.
func endPasswordOnlyTx(ctx context.Context, tx execer, out *ModeStart, now time.Time, runs *[]EndedRun) error {
	out.Ended = true
	// Any operator's session may have been signed in with a password alone,
	// enrolled since or not: signing in again is cheap on a switch that
	// happens rarely. A script credential is neither renewable nor
	// idle-bounded, so it is not one of these.
	sessions, err := revokeMatchingTx(ctx, tx,
		`SELECT c.id, c.principal_id, c.label FROM credentials c JOIN principals p ON p.id = c.principal_id
		 WHERE p.role = 'operator' AND c.revoked_at IS NULL AND c.scope = 'full' AND `+signInSessionSQL+` ORDER BY c.id`,
		now, runs)
	if err != nil {
		return err
	}
	out.SessionsRevoked = sessions
	res, err := tx.ExecContext(ctx, "UPDATE credentials SET recent_auth_until = NULL WHERE recent_auth_until IS NOT NULL")
	if err != nil {
		return fmt.Errorf("store: start mode: clear proofs: %w", err)
	}
	if out.ProofsCleared, err = res.RowsAffected(); err != nil {
		return fmt.Errorf("store: start mode: clear proofs: %w", err)
	}
	res, err = tx.ExecContext(ctx, "UPDATE pending_logins SET spent_at = ? WHERE spent_at IS NULL", now.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: start mode: spend pending logins: %w", err)
	}
	if out.PendingLoginsSpent, err = res.RowsAffected(); err != nil {
		return fmt.Errorf("store: start mode: spend pending logins: %w", err)
	}
	// A setup credential that set a password then may still be live, and so
	// may one that set none; either would finish a setup by the
	// password-only rule. The operator starts again with a token made on
	// the host.
	if out.SetupCredentialsRevoked, err = revokeMatchingTx(ctx, tx,
		"SELECT id, principal_id, label FROM credentials WHERE scope = 'setup' AND revoked_at IS NULL ORDER BY id", now, runs); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM principals p
		 WHERE p.role = 'operator' AND p.kind = 'human' AND p.disabled_at IS NULL AND p.hidden_at IS NULL
		   AND EXISTS (SELECT 1 FROM passwords pw WHERE pw.principal_id = p.id)
		   AND NOT EXISTS (SELECT 1 FROM totp t WHERE t.principal_id = p.id AND t.secret IS NOT NULL AND t.enrolled_at IS NOT NULL)`,
	).Scan(&out.OperatorsWithoutAuthenticator); err != nil {
		return fmt.Errorf("store: start mode: count operators without an authenticator: %w", err)
	}
	return nil
}

// revokeMatchingTx revokes every credential query selects (id, principal_id,
// label), each audited credential_revoked with reason password_only_ended
// and its sessions ended, and returns how many it revoked. The last-operator
// guard does not apply: an owner still signs in afterwards with a password
// and a code, or with a setup token made on the host.
func revokeMatchingTx(ctx context.Context, tx execer, query string, now time.Time, runs *[]EndedRun) (int64, error) {
	type cred struct {
		id, principalID int64
		label           string
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("store: start mode: list credentials: %w", err)
	}
	var creds []cred
	for rows.Next() {
		var c cred
		if err := rows.Scan(&c.id, &c.principalID, &c.label); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: start mode: list credentials: %w", err)
		}
		creds = append(creds, c)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: start mode: list credentials: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: start mode: list credentials: %w", err)
	}
	for _, c := range creds {
		if _, err := tx.ExecContext(ctx, "UPDATE credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), c.id); err != nil {
			return 0, fmt.Errorf("store: start mode: revoke credential: %w", err)
		}
		if err := appendAuditEventTx(ctx, tx, hostActor, "credential_revoked", principalActor(c.principalID),
			credentialDetail(c.id, c.label)+" reason="+modeRevokedReason, now); err != nil {
			return 0, err
		}
		if err := endCredentialSessionsTx(ctx, tx, hostActor, c.id, SessionEndedByRevocation, now, runs); err != nil {
			return 0, err
		}
	}
	return int64(len(creds)), nil
}

// StoredMode returns the mode the server last started in, and whether it
// allowed password-only operators: ModeNormal and false when no server has
// started since migration 36.
func (s *Store) StoredMode(ctx context.Context) (Mode, bool, error) {
	var (
		stored       string
		passwordOnly bool
	)
	switch err := s.db.QueryRowContext(ctx, "SELECT mode, password_only FROM server_mode WHERE id = 1").Scan(&stored, &passwordOnly); {
	case errors.Is(err, sql.ErrNoRows):
		return ModeNormal, false, nil
	case err != nil:
		return "", false, fmt.Errorf("store: server mode: %w", err)
	}
	return Mode(stored), passwordOnly, nil
}

// storedPasswordOnlyTx reports, on tx, whether the server's last start
// allowed password-only operators. A missing row is false: the stricter
// rule, so a store nobody started fails closed. It is what every store-side
// check reads (ADR-012 item 3), never the mode alone.
func storedPasswordOnlyTx(ctx context.Context, tx execer) (bool, error) {
	var passwordOnly bool
	switch err := tx.QueryRowContext(ctx, "SELECT password_only FROM server_mode WHERE id = 1").Scan(&passwordOnly); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: server mode: %w", err)
	}
	return passwordOnly, nil
}

// passwordOnlyOperatorTx reports, inside the caller's transaction, whether
// principalID may prove itself with a password alone: the stored flag says
// password-only is allowed and the principal has no confirmed second factor
// (ADR-011 item 2.1, ADR-012 item 3). An operator who has enrolled is asked
// for the code whatever the settings, so they never undo an enrolment.
func passwordOnlyOperatorTx(ctx context.Context, tx execer, principalID int64) (bool, error) {
	allowed, err := storedPasswordOnlyTx(ctx, tx)
	if err != nil || !allowed {
		return false, err
	}
	enrolled, err := totpEnrolledTx(ctx, tx, principalID)
	if err != nil {
		return false, err
	}
	return !enrolled, nil
}

// IssuePasswordOnlyOperatorSession issues an operator the browser session a
// verified password earns where password-only is allowed, without a second
// factor (ADR-011 item 2.1, ADR-012): the session CompleteTOTPLogin issues,
// absolute and idle bounded, never renewed, in one IMMEDIATE transaction that
// re-checks the sign-in (recheckLoginTx, for an operator), that the stored
// flag allows password-only and that the operator has no confirmed second
// factor (passwordOnlyOperatorTx). Anything else is ErrLoginStale and writes
// nothing: an operator who enrolled meanwhile, or a store that says
// required, gets no session without the code. login and credential_created are
// audited, as for any sign-in.
func (s *Store) IssuePasswordOnlyOperatorSession(ctx context.Context, in LoginIssue, now time.Time) (IssuedLogin, error) {
	now = now.Truncate(time.Millisecond)
	token, err := newCredentialToken()
	if err != nil {
		return IssuedLogin{}, err
	}
	var out IssuedLogin
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out = IssuedLogin{}
		p, err := recheckLoginTx(ctx, tx, in, RoleOperator, now)
		if err != nil {
			return err
		}
		if ok, err := passwordOnlyOperatorTx(ctx, tx, in.PrincipalID); err != nil {
			return err
		} else if !ok {
			return ErrLoginStale
		}
		cred, err := insertOperatorSessionTx(ctx, tx, p.ID, token, now)
		if err != nil {
			return err
		}
		out.Principal, out.Credential = p, cred
		actor := principalActor(p.ID)
		if err := appendAuditEventTx(ctx, tx, actor, AuditLogin, actor, fmt.Sprintf("credential=%d", cred.ID), now); err != nil {
			return err
		}
		return appendAuditEventTx(ctx, tx, actor, "credential_created", actor, credentialDetail(cred.ID, cred.Label), now)
	})
	if err != nil {
		return IssuedLogin{}, err
	}
	out.Token = token
	return out, nil
}

// RecordPasswordOnlyOperatorRecentAuth is RecordOperatorRecentAuth without a
// code, for an operator with no second factor where password-only is
// allowed (ADR-011 item 2.1, ADR-012). It re-checks everything
// RecordRecentAuth does and, in the same transaction, passwordOnlyOperatorTx:
// otherwise it is ErrSecondFactorRequired and writes nothing, so an operator
// who enrolled meanwhile, or a store that says required, still needs the
// code.
func (s *Store) RecordPasswordOnlyOperatorRecentAuth(ctx context.Context, in LoginIssue, credentialID int64, now time.Time) (time.Time, error) {
	return s.recordRecentAuth(ctx, in, credentialID, RoleOperator, nil, now)
}
