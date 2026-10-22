package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Operator authentication (issue #709, docs/design/guests-and-accounts.md
// §5.3, §7.2, §7.3): the second factor, the pending-login completion, the
// operator's browser session, the recent-auth proof with a code, and the
// host-made setup tokens that bootstrap and recover an operator offline.
//
// The store never sees a TOTP secret in the clear, a code or the host key.
// It keeps the sealed secret the server made (totp.secret, totp.pending_secret)
// and the highest time step it accepted (totp.last_step). Checking a code is
// the server's (TOTPCheck), but the check runs inside the IMMEDIATE
// transaction that records the step, so of two uses of one code, at a
// sign-in and a re-authentication at once say, exactly one passes: the
// second sees the step already taken (design §5.3, replay refusal).

// Audit actions of operator authentication (design §7.3). None carries a
// secret, a code, a challenge or a token.
const (
	// AuditTOTPEnrolled is a second factor confirmed: detail the credential
	// it was confirmed on.
	AuditTOTPEnrolled = "totp_enrolled"
	// AuditSetupTokenIssued is a setup token made on the host: detail its id
	// and the command that made it.
	AuditSetupTokenIssued = "setup_token_issued" // #nosec G101 -- an audit action's name, not a credential
	// AuditSetupTokenRedeemed is a setup token spent for a setup-scoped
	// credential: actor the token, detail the credential.
	AuditSetupTokenRedeemed = "setup_token_redeemed"
	// AuditOperatorRecovered is an operator's authentication reset on the
	// host: detail how many credentials it revoked.
	AuditOperatorRecovered = "operator_recovered"
)

// Labels of the credentials this file issues.
const (
	// OperatorSessionLabel labels the credential an operator's password and
	// TOTP sign-in earns.
	OperatorSessionLabel = "operator sign-in"
	// SetupCredentialLabel labels the setup-scoped credential a setup token
	// answers.
	SetupCredentialLabel = "setup"
)

// hostActor is the audit actor of the host commands: whoever can run conchd
// on the data directory, as for the bootstrap.
const hostActor = "system"

var (
	// ErrSecondFactorWrong is a TOTP code that is wrong, outside the window,
	// or already used (a replay). The server answers it as it answers any
	// failed sign-in.
	ErrSecondFactorWrong = errors.New("store: the second factor did not match")
	// ErrChallengeInvalid is a pending-login challenge that is unknown,
	// spent, expired or out of attempts.
	ErrChallengeInvalid = errors.New("store: the sign-in challenge is not usable")
	// ErrTOTPAlreadyEnrolled is an enrolment for an operator whose second
	// factor is confirmed: only offline recovery replaces it.
	ErrTOTPAlreadyEnrolled = errors.New("store: the operator already has a second factor")
	// ErrTOTPNotEnrolled is a confirmation with no enrolment pending.
	ErrTOTPNotEnrolled = errors.New("store: no second factor is pending")
	// ErrSetupTokenInvalid is a setup token that is unknown, spent, expired
	// or for an operator whose authentication has changed since.
	ErrSetupTokenInvalid = errors.New("store: the setup token is not usable")
	// ErrNotAnOperator is a host command naming a principal that is not an
	// operator.
	ErrNotAnOperator = errors.New("store: the principal is not an operator")
	// ErrSetupComplete is a setup token asked for an operator who has a
	// password and a second factor: recovery, which revokes what they hold,
	// is the way to replace them.
	ErrSetupComplete = errors.New("store: the operator's setup is complete")
)

// TOTPCheck checks a code against a sealed secret: it opens sealed and
// returns the time step the code is of, which must be after lastStep. A
// wrong or replayed code is ErrSecondFactorWrong; any other error means the
// secret cannot be checked at all (no key, the wrong key), and fails closed.
// The server makes it from the request's code and its clock; the store calls
// it inside the transaction that records the step.
type TOTPCheck func(sealed []byte, lastStep int64) (int64, error)

// consumeTOTPTx checks principalID's confirmed second factor with check and
// records the step it accepted, in the caller's transaction. An operator
// with no confirmed second factor is ErrLoginStale: it was removed (by a
// recovery) after the request began.
func consumeTOTPTx(ctx context.Context, tx execer, principalID int64, check TOTPCheck) error {
	if check == nil {
		return ErrSecondFactorRequired
	}
	var (
		sealed   []byte
		lastStep int64
	)
	switch err := tx.QueryRowContext(ctx,
		"SELECT secret, last_step FROM totp WHERE principal_id = ? AND secret IS NOT NULL AND enrolled_at IS NOT NULL",
		principalID).Scan(&sealed, &lastStep); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrLoginStale
	case err != nil:
		return fmt.Errorf("store: second factor: %w", err)
	}
	step, err := check(sealed, lastStep)
	if err != nil {
		return err
	}
	if step <= lastStep {
		return ErrSecondFactorWrong
	}
	// The guard is the replay refusal: the transaction already serializes
	// writers, and the update still takes the step only if nothing took it
	// or a later one first.
	res, err := tx.ExecContext(ctx, "UPDATE totp SET last_step = ? WHERE principal_id = ? AND last_step < ?", step, principalID, step)
	if err != nil {
		return fmt.Errorf("store: second factor: record step: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: second factor: record step: %w", err)
	} else if n != 1 {
		return ErrSecondFactorWrong
	}
	return nil
}

// PendingChallenge is a pending login as the TOTP completion first reads
// it, outside any transaction: whose it is, the name its failures count
// under, and the keyed address it was issued to (opaque here; the server
// compares it).
type PendingChallenge struct {
	PrincipalID int64
	Name        string
	AddressKey  string
}

// PendingLoginOf returns the usable pending login with this challenge hash:
// unspent, unexpired, with attempts left, of its principal's present
// generation. Anything else is ErrChallengeInvalid. Nothing is written.
func (s *Store) PendingLoginOf(ctx context.Context, challengeHash string, now time.Time) (PendingChallenge, error) {
	var out PendingChallenge
	err := s.db.QueryRowContext(ctx,
		`SELECT l.principal_id, p.name, l.address_key FROM pending_logins l JOIN principals p ON p.id = l.principal_id
		 WHERE l.challenge_hash = ? AND l.spent_at IS NULL AND l.expires_at > ? AND l.attempts < ?
		   AND l.auth_generation = p.auth_generation`,
		challengeHash, now.UnixMilli(), schema.MaxTOTPAttemptsPerChallenge).Scan(&out.PrincipalID, &out.Name, &out.AddressKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PendingChallenge{}, ErrChallengeInvalid
	case err != nil:
		return PendingChallenge{}, fmt.Errorf("store: pending login: %w", err)
	}
	return out, nil
}

// HashChallenge is the stored form of a pending-login challenge.
func HashChallenge(challenge string) string { return hashCredentialToken(challenge) }

// TOTPLogin is the completion of a pending login: the challenge's hash,
// whether the request came from the address it was issued to (the server
// compares the keyed address, review F14), and the check of the code.
type TOTPLogin struct {
	ChallengeHash  string
	AddressMatches bool
	Check          TOTPCheck
}

// CompleteTOTPLogin completes an operator's sign-in, in one IMMEDIATE
// transaction: the challenge must still be usable, the request from the
// address it was issued to, the operator unchanged since the password was
// verified (a person, an operator, enabled, not hidden, of the challenge's
// generation, with a password), and the code right and unused. Then the
// step is recorded, the challenge spent, and the operator issued a browser
// session: schema.OperatorSessionAbsoluteSeconds absolute and
// schema.OperatorSessionIdleSeconds idle, never renewed. login and
// credential_created are audited.
//
// A wrong address or code counts as one of the challenge's attempts and is
// committed as such (the challenge is spent at the last one), then answered
// ErrSecondFactorWrong. A challenge that is not usable is ErrChallengeInvalid,
// an operator that changed ErrLoginStale. An error from the check that is
// not ErrSecondFactorWrong (the secret cannot be checked) writes nothing and
// counts nothing: no code was looked at.
func (s *Store) CompleteTOTPLogin(ctx context.Context, in TOTPLogin, now time.Time) (IssuedLogin, error) {
	now = now.Truncate(time.Millisecond)
	token, err := newCredentialToken()
	if err != nil {
		return IssuedLogin{}, err
	}
	var (
		out   IssuedLogin
		wrong bool
	)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out, wrong = IssuedLogin{}, false
		var (
			id, principalID, generation, expiresAt int64
			attempts                               int
			spent                                  sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx,
			"SELECT id, principal_id, auth_generation, expires_at, attempts, spent_at FROM pending_logins WHERE challenge_hash = ?",
			in.ChallengeHash).Scan(&id, &principalID, &generation, &expiresAt, &attempts, &spent); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrChallengeInvalid
		case err != nil:
			return fmt.Errorf("store: complete login: %w", err)
		}
		if spent.Valid || expiresAt <= now.UnixMilli() || attempts >= schema.MaxTOTPAttemptsPerChallenge {
			return ErrChallengeInvalid
		}
		// One wrong attempt: counted, and the challenge spent at the last.
		miss := func() error {
			wrong = true
			_, err := tx.ExecContext(ctx,
				`UPDATE pending_logins SET attempts = attempts + 1,
				        spent_at = CASE WHEN attempts + 1 >= ? THEN ? ELSE spent_at END
				 WHERE id = ?`, schema.MaxTOTPAttemptsPerChallenge, now.UnixMilli(), id)
			if err != nil {
				return fmt.Errorf("store: complete login: count attempt: %w", err)
			}
			return nil
		}
		if !in.AddressMatches {
			return miss()
		}
		p, found, err := queryPrincipal(ctx, tx, principalID)
		if err != nil {
			return err
		}
		var current, hasPassword bool
		if err := tx.QueryRowContext(ctx,
			`SELECT p.auth_generation = ?, EXISTS (SELECT 1 FROM passwords WHERE principal_id = p.id) FROM principals p WHERE p.id = ?`,
			generation, principalID).Scan(&current, &hasPassword); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: complete login: %w", err)
		}
		if !found || p.Kind != PrincipalHuman || p.Role != RoleOperator || p.DisabledAt != nil || p.HiddenAt != nil ||
			p.guestTarget() || !current || !hasPassword {
			return ErrLoginStale
		}
		switch err := consumeTOTPTx(ctx, tx, principalID, in.Check); {
		case errors.Is(err, ErrSecondFactorWrong):
			return miss()
		case err != nil:
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE pending_logins SET spent_at = ? WHERE id = ? AND spent_at IS NULL", now.UnixMilli(), id)
		if err != nil {
			return fmt.Errorf("store: complete login: spend challenge: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: complete login: spend challenge: %w", err)
		} else if n != 1 {
			return ErrChallengeInvalid
		}
		cred, err := insertOperatorSessionTx(ctx, tx, principalID, token, now)
		if err != nil {
			return err
		}
		out.Principal, out.Credential = p, cred
		actor := principalActor(principalID)
		if err := appendAuditEventTx(ctx, tx, actor, AuditLogin, actor, fmt.Sprintf("credential=%d", cred.ID), now); err != nil {
			return err
		}
		return appendAuditEventTx(ctx, tx, actor, "credential_created", actor, credentialDetail(cred.ID, cred.Label), now)
	})
	switch {
	case err != nil:
		return IssuedLogin{}, err
	case wrong:
		return IssuedLogin{}, ErrSecondFactorWrong
	}
	out.Token = token
	return out, nil
}

// insertOperatorSessionTx issues principalID the browser session a password
// and TOTP sign-in earns: absolute and idle bounds, no renewal, full scope.
// The idle bound is moved only by REST requests (NoteGuestUse), as a guest's
// is (review F5), so a socket or a ping loop does not keep it alive.
func insertOperatorSessionTx(ctx context.Context, tx execer, principalID int64, token string, now time.Time) (schema.CredentialV1, error) {
	exp := now.Add(schema.OperatorSessionAbsoluteSeconds * time.Second)
	cred, err := insertCredentialTx(ctx, tx, principalID, OperatorSessionLabel, token, now, &exp, 0)
	if err != nil {
		return schema.CredentialV1{}, err
	}
	idle := schema.OperatorSessionIdleSeconds * time.Second
	if _, err := tx.ExecContext(ctx, "UPDATE credentials SET idle_ms = ? WHERE id = ?", idle.Milliseconds(), cred.ID); err != nil {
		return schema.CredentialV1{}, fmt.Errorf("store: operator session: %w", err)
	}
	idleAt := schema.NewTimestamp(now.Add(idle))
	cred.IdleExpiresAt = &idleAt
	return cred, nil
}

// BeginTOTPEnrollment records sealed as principalID's pending second factor,
// replacing one already pending, on the setup-scoped credential the request
// came with. Only an operator on a live setup credential enrols
// (ErrSetupOnHost otherwise, review F7); one whose second factor is
// confirmed is ErrTOTPAlreadyEnrolled. Nothing is audited: the enrolment
// counts once a code confirms it.
func (s *Store) BeginTOTPEnrollment(ctx context.Context, principalID, credentialID int64, sealed []byte, now time.Time) error {
	now = now.Truncate(time.Millisecond)
	if len(sealed) == 0 {
		return errors.New("store: begin totp enrolment: no sealed secret")
	}
	return s.withImmediateTx(ctx, func(tx execer) error {
		if err := setupCallerTx(ctx, tx, principalID, credentialID, now); err != nil {
			return err
		}
		if enrolled, err := totpEnrolledTx(ctx, tx, principalID); err != nil {
			return err
		} else if enrolled {
			return ErrTOTPAlreadyEnrolled
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO totp (principal_id, pending_secret, pending_at) VALUES (?, ?, ?)
			 ON CONFLICT (principal_id) DO UPDATE SET pending_secret = excluded.pending_secret, pending_at = excluded.pending_at`,
			principalID, sealed, now.UnixMilli()); err != nil {
			return fmt.Errorf("store: begin totp enrolment: %w", err)
		}
		return nil
	})
}

// setupCallerTx checks, inside a writing transaction, that a TOTP enrolment
// comes from an operator (a person, enabled, not hidden) on a live
// setup-scoped credential of theirs.
func setupCallerTx(ctx context.Context, tx execer, principalID, credentialID int64, now time.Time) error {
	p, err := passwordPrincipalTx(ctx, tx, principalID)
	if err != nil {
		return err
	}
	if p.Role != RoleOperator {
		return ErrSetupOnHost
	}
	cred, err := callerCredentialTx(ctx, tx, principalID, credentialID, now)
	if err != nil {
		return err
	}
	if cred.scope != schema.CredentialScopeSetup {
		return ErrSetupOnHost
	}
	return nil
}

func totpEnrolledTx(ctx context.Context, tx execer, principalID int64) (bool, error) {
	var enrolled bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM totp WHERE principal_id = ? AND secret IS NOT NULL AND enrolled_at IS NOT NULL)",
		principalID).Scan(&enrolled); err != nil {
		return false, fmt.Errorf("store: second factor: %w", err)
	}
	return enrolled, nil
}

// TOTPConfirmation is a confirmation of a pending second factor: the
// caller, the setup credential it came with, and the check of its code.
type TOTPConfirmation struct {
	PrincipalID, CredentialID int64
	Check                     TOTPCheck
}

// TOTPConfirmed is what a confirmation did. SetupEnded is the setup
// credential it revoked because setup is now complete (a password and a
// second factor), 0 when the operator still has no password.
type TOTPConfirmed struct {
	SetupEnded int64
}

// ConfirmTOTP makes principalID's pending second factor its confirmed one,
// in one IMMEDIATE transaction: an operator on a live setup credential
// (ErrSetupOnHost otherwise), with an enrolment pending
// (ErrTOTPNotEnrolled) and none confirmed (ErrTOTPAlreadyEnrolled), and a
// code of the pending secret (ErrSecondFactorWrong otherwise, writing
// nothing). The code's step is recorded, so it cannot sign in afterwards,
// and totp_enrolled is audited. When the operator has a password too, setup
// is complete and the setup credential is revoked in the same transaction:
// the operator now signs in with both factors.
func (s *Store) ConfirmTOTP(ctx context.Context, actor string, in TOTPConfirmation, now time.Time) (TOTPConfirmed, error) {
	now = now.Truncate(time.Millisecond)
	var (
		out  TOTPConfirmed
		runs []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = TOTPConfirmed{}, nil
		if err := setupCallerTx(ctx, tx, in.PrincipalID, in.CredentialID, now); err != nil {
			return err
		}
		var (
			pending  []byte
			enrolled bool
			lastStep int64
		)
		switch err := tx.QueryRowContext(ctx,
			"SELECT pending_secret, secret IS NOT NULL AND enrolled_at IS NOT NULL, last_step FROM totp WHERE principal_id = ?",
			in.PrincipalID).Scan(&pending, &enrolled, &lastStep); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrTOTPNotEnrolled
		case err != nil:
			return fmt.Errorf("store: confirm totp: %w", err)
		}
		switch {
		case enrolled:
			return ErrTOTPAlreadyEnrolled
		case len(pending) == 0:
			return ErrTOTPNotEnrolled
		case in.Check == nil:
			return ErrSecondFactorRequired
		}
		step, err := in.Check(pending, lastStep)
		if err != nil {
			return err
		}
		if step <= lastStep {
			return ErrSecondFactorWrong
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE totp SET secret = pending_secret, enrolled_at = ?, pending_secret = NULL, pending_at = NULL, last_step = ?
			 WHERE principal_id = ?`, now.UnixMilli(), step, in.PrincipalID); err != nil {
			return fmt.Errorf("store: confirm totp: %w", err)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditTOTPEnrolled, principalActor(in.PrincipalID),
			fmt.Sprintf("credential=%d", in.CredentialID), now); err != nil {
			return err
		}
		var hasPassword bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM passwords WHERE principal_id = ?)", in.PrincipalID).Scan(&hasPassword); err != nil {
			return fmt.Errorf("store: confirm totp: %w", err)
		}
		if !hasPassword {
			return nil
		}
		if err := endSetupCredentialTx(ctx, tx, actor, in.PrincipalID, in.CredentialID, now, &runs); err != nil {
			return err
		}
		out.SetupEnded = in.CredentialID
		return nil
	})
	if err != nil {
		return TOTPConfirmed{}, err
	}
	s.announceRunsEnded(runs)
	return out, nil
}

// endSetupCredentialTx revokes the setup credential credentialID once its
// setup is complete, audited credential_revoked with the reason. A setup
// credential is no sign-in, so the last-operator guard does not apply: the
// operator now signs in with both factors.
func endSetupCredentialTx(ctx context.Context, tx execer, actor string, principalID, credentialID int64, now time.Time, runs *[]EndedRun) error {
	res, err := tx.ExecContext(ctx,
		"UPDATE credentials SET revoked_at = ? WHERE id = ? AND principal_id = ? AND scope = 'setup' AND revoked_at IS NULL",
		now.UnixMilli(), credentialID, principalID)
	if err != nil {
		return fmt.Errorf("store: end setup credential: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: end setup credential: %w", err)
	} else if n != 1 {
		return nil
	}
	if err := appendAuditEventTx(ctx, tx, actor, "credential_revoked", principalActor(principalID),
		credentialDetail(credentialID, SetupCredentialLabel)+" reason=setup complete", now); err != nil {
		return err
	}
	return endCredentialSessionsTx(ctx, tx, actor, credentialID, SessionEndedByRevocation, now, runs)
}

// RecordOperatorRecentAuth is RecordRecentAuth for an operator (design
// §5.3): the password was verified outside the transaction, and inside it
// check must accept a current, unused TOTP code, whose step is recorded with
// the proof. A wrong or replayed code is ErrSecondFactorWrong and writes
// nothing; a nil check is ErrSecondFactorRequired.
func (s *Store) RecordOperatorRecentAuth(ctx context.Context, in LoginIssue, credentialID int64, check TOTPCheck, now time.Time) (time.Time, error) {
	if check == nil {
		return time.Time{}, ErrSecondFactorRequired
	}
	return s.recordRecentAuth(ctx, in, credentialID, RoleOperator, check, now)
}

// SetupToken is a setup token a host command made: the plaintext, shown
// once (only its hash is kept), its expiry, and whom it is for.
type SetupToken struct {
	ID          int64
	Token       string
	ExpiresAt   time.Time
	PrincipalID int64
}

// HashSetupToken is the stored form of a setup token.
func HashSetupToken(token string) string { return hashCredentialToken(token) }

// issueSetupTokenTx makes principalID a setup token of its present
// generation, good for schema.SetupTokenLifetimeSeconds, and spends any it
// still had unused: one at a time. setup_token_issued is audited with via,
// the host command.
func issueSetupTokenTx(ctx context.Context, tx execer, principalID int64, via string, now time.Time) (SetupToken, error) {
	token, err := newOpaqueSecret(schema.SetupTokenPrefix, schema.SetupTokenRandomBytes)
	if err != nil {
		return SetupToken{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE setup_tokens SET spent_at = ? WHERE principal_id = ? AND spent_at IS NULL",
		now.UnixMilli(), principalID); err != nil {
		return SetupToken{}, fmt.Errorf("store: setup token: spend older: %w", err)
	}
	exp := now.Add(schema.SetupTokenLifetimeSeconds * time.Second)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO setup_tokens (token_hash, principal_id, auth_generation, created_at, expires_at)
		 SELECT ?, id, auth_generation, ?, ? FROM principals WHERE id = ?`,
		HashSetupToken(token), now.UnixMilli(), exp.UnixMilli(), principalID)
	if err != nil {
		return SetupToken{}, fmt.Errorf("store: setup token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return SetupToken{}, fmt.Errorf("store: setup token: %w", err)
	}
	if err := appendAuditEventTx(ctx, tx, hostActor, AuditSetupTokenIssued, principalActor(principalID),
		fmt.Sprintf("setup_token=%d via=%s", id, via), now); err != nil {
		return SetupToken{}, err
	}
	return SetupToken{ID: id, Token: token, ExpiresAt: exp, PrincipalID: principalID}, nil
}

// hostOperatorTx reads principalID for a host command: it must be an
// operator (ErrNotAnOperator; ErrPrincipalNotFound for no one), a person,
// enabled and not hidden.
func hostOperatorTx(ctx context.Context, tx execer, principalID int64) (Principal, error) {
	p, found, err := queryPrincipal(ctx, tx, principalID)
	switch {
	case err != nil:
		return Principal{}, err
	case !found:
		return Principal{}, ErrPrincipalNotFound
	case p.Kind != PrincipalHuman || p.Role != RoleOperator:
		return Principal{}, ErrNotAnOperator
	case p.HiddenAt != nil:
		return Principal{}, ErrPrincipalHidden
	case p.DisabledAt != nil:
		return Principal{}, ErrPrincipalDisabled
	}
	return p, nil
}

// IssueSetupToken makes an existing operator whose setup is incomplete (no
// password, or no confirmed second factor) a setup token, on the host, and
// revokes nothing: an operator made before passwords keeps the script
// credentials it has (design §7.3). One whose setup is complete is
// ErrSetupComplete: RecoverOperator replaces both factors.
func (s *Store) IssueSetupToken(ctx context.Context, principalID int64, now time.Time) (SetupToken, error) {
	now = now.Truncate(time.Millisecond)
	var out SetupToken
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if _, err := hostOperatorTx(ctx, tx, principalID); err != nil {
			return err
		}
		var complete bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM passwords WHERE principal_id = ?)
			    AND EXISTS (SELECT 1 FROM totp WHERE principal_id = ? AND secret IS NOT NULL AND enrolled_at IS NOT NULL)`,
			principalID, principalID).Scan(&complete); err != nil {
			return fmt.Errorf("store: setup token: %w", err)
		}
		if complete {
			return ErrSetupComplete
		}
		var err error
		out, err = issueSetupTokenTx(ctx, tx, principalID, "setup", now)
		return err
	})
	return out, err
}

// Recovery is what RecoverOperator did.
type Recovery struct {
	Setup   SetupToken
	Revoked int
}

// RecoverOperator resets an operator's authentication on the host (design
// §5.3, §7.2), in one IMMEDIATE transaction: every credential of the
// operator is revoked (the last-operator guard does not apply: the setup
// token is the way back in), its second factor is removed, its
// authentication generation moves on (which ends every pending login, link
// and recent-auth proof of its), operator_recovered is audited, and a setup
// token is made. The password stays until the setup-scoped credential the
// token answers replaces it; with no second factor it signs nobody in. There
// is no route that does this: only conchd on the host.
func (s *Store) RecoverOperator(ctx context.Context, principalID int64, now time.Time) (Recovery, error) {
	now = now.Truncate(time.Millisecond)
	var (
		out  Recovery
		runs []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = Recovery{}, nil
		if _, err := hostOperatorTx(ctx, tx, principalID); err != nil {
			return err
		}
		var err error
		if out.Revoked, err = revokePrincipalCredentialsTx(ctx, tx, hostActor, principalID, "operator recovery",
			SessionEndedByRevocation, now, &runs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM totp WHERE principal_id = ?", principalID); err != nil {
			return fmt.Errorf("store: recover operator: remove second factor: %w", err)
		}
		if _, err := bumpAuthGenerationTx(ctx, tx, hostActor, principalID, "operator_recovered", now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, hostActor, AuditOperatorRecovered, principalActor(principalID),
			fmt.Sprintf("revoked=%d", out.Revoked), now); err != nil {
			return err
		}
		out.Setup, err = issueSetupTokenTx(ctx, tx, principalID, "recover", now)
		return err
	})
	if err != nil {
		return Recovery{}, err
	}
	s.announceRunsEnded(runs)
	return out, nil
}

// RedeemedSetup is a spent setup token's answer: the operator, a
// setup-scoped credential and its plaintext token (shown once), and what
// setup remains.
type RedeemedSetup struct {
	Principal    Principal
	Credential   schema.CredentialV1
	Token        string
	PasswordSet  bool
	TOTPEnrolled bool
}

// RedeemSetupToken spends the setup token with this hash for a setup-scoped
// credential that lives schema.SetupCredentialLifetimeSeconds, in one
// IMMEDIATE transaction: the token must be unspent and unexpired, of its
// operator's present generation, for an operator who is a person, enabled
// and not hidden. Every other case is ErrSetupTokenInvalid and writes
// nothing; of two concurrent uses one wins. setup_token_redeemed and
// credential_created are audited, the first attributed to
// "setup_token:<id>".
func (s *Store) RedeemSetupToken(ctx context.Context, tokenHash string, now time.Time) (RedeemedSetup, error) {
	now = now.Truncate(time.Millisecond)
	token, err := newCredentialToken()
	if err != nil {
		return RedeemedSetup{}, err
	}
	var out RedeemedSetup
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out = RedeemedSetup{}
		var (
			id, principalID, generation, expiresAt int64
			storedHash                             string
			spent                                  sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx,
			"SELECT id, token_hash, principal_id, auth_generation, expires_at, spent_at FROM setup_tokens WHERE token_hash = ?", tokenHash,
		).Scan(&id, &storedHash, &principalID, &generation, &expiresAt, &spent); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrSetupTokenInvalid
		case err != nil:
			return fmt.Errorf("store: redeem setup token: %w", err)
		}
		// As a credential's resolution does: the lookup found the row, and
		// the comparison that decides takes the same time whatever it holds.
		if subtle.ConstantTimeCompare([]byte(storedHash), []byte(tokenHash)) != 1 {
			return ErrSetupTokenInvalid
		}
		if spent.Valid || expiresAt <= now.UnixMilli() {
			return ErrSetupTokenInvalid
		}
		p, err := hostOperatorTx(ctx, tx, principalID)
		if err != nil {
			return ErrSetupTokenInvalid
		}
		var current bool
		if err := tx.QueryRowContext(ctx, "SELECT auth_generation = ? FROM principals WHERE id = ?", generation, principalID).Scan(&current); err != nil {
			return fmt.Errorf("store: redeem setup token: %w", err)
		}
		if !current {
			return ErrSetupTokenInvalid
		}
		res, err := tx.ExecContext(ctx, "UPDATE setup_tokens SET spent_at = ? WHERE id = ? AND spent_at IS NULL", now.UnixMilli(), id)
		if err != nil {
			return fmt.Errorf("store: redeem setup token: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: redeem setup token: %w", err)
		} else if n != 1 {
			return ErrSetupTokenInvalid
		}
		exp := now.Add(schema.SetupCredentialLifetimeSeconds * time.Second)
		cred, err := insertCredentialTx(ctx, tx, principalID, SetupCredentialLabel, token, now, &exp, 0)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE credentials SET scope = 'setup' WHERE id = ?", cred.ID); err != nil {
			return fmt.Errorf("store: redeem setup token: %w", err)
		}
		cred.Scope = schema.CredentialScopeSetup
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM passwords WHERE principal_id = ?),
			        EXISTS (SELECT 1 FROM totp WHERE principal_id = ? AND secret IS NOT NULL AND enrolled_at IS NOT NULL)`,
			principalID, principalID).Scan(&out.PasswordSet, &out.TOTPEnrolled); err != nil {
			return fmt.Errorf("store: redeem setup token: %w", err)
		}
		out.Principal, out.Credential = p, cred
		if err := appendAuditEventTx(ctx, tx, fmt.Sprintf("setup_token:%d", id), AuditSetupTokenRedeemed, principalActor(principalID),
			fmt.Sprintf("credential=%d", cred.ID), now); err != nil {
			return err
		}
		return appendAuditEventTx(ctx, tx, principalActor(principalID), "credential_created", principalActor(principalID),
			credentialDetail(cred.ID, cred.Label), now)
	})
	if err != nil {
		return RedeemedSetup{}, err
	}
	out.Token = token
	return out, nil
}

// SweepPendingLogins deletes every pending login that can no longer be
// completed: spent, or expired at now. A challenge is good for five minutes
// and the row is the only thing that remembers it, so nothing of value goes;
// the sign-in it led to, or did not, is in the audit trail. It returns how
// many it deleted.
func (s *Store) SweepPendingLogins(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM pending_logins WHERE spent_at IS NOT NULL OR expires_at <= ?", now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: sweep pending logins: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: sweep pending logins: %w", err)
	}
	return n, nil
}

// AnyConfirmedTOTP returns one operator's confirmed sealed second factor,
// for the server's check at start that its TOTP key opens what is stored.
// found is false when no operator has one.
func (s *Store) AnyConfirmedTOTP(ctx context.Context) (principalID int64, sealed []byte, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		"SELECT principal_id, secret FROM totp WHERE secret IS NOT NULL AND enrolled_at IS NOT NULL ORDER BY principal_id LIMIT 1",
	).Scan(&principalID, &sealed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil, false, nil
	case err != nil:
		return 0, nil, false, fmt.Errorf("store: confirmed totp: %w", err)
	}
	return principalID, sealed, true, nil
}
