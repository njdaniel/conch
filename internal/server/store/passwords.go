package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/pkg/schema"
)

// Passwords and the authentication generation (issue #707,
// docs/design/guests-and-accounts.md §5 and §7.3).
//
// The store keeps a password only as the encoded Argon2id record the server
// made; it never hashes, verifies or sees a password. Verification happens
// in the server, outside any write transaction, and the store then issues
// what the verification earned in one IMMEDIATE transaction that re-reads
// everything the verification relied on: the principal is still a person of
// the expected role, enabled and not hidden, its authentication generation
// is unchanged, and its record is still the one that was verified. So a
// password change, a reset, a disable or a sign-out everywhere that commits
// while a sign-in hashes wins, and the sign-in gets ErrLoginStale.
//
// The authentication generation is a counter on the principal. Every
// credential records the one it was issued under (insertCredentialTx), and
// a credential whose generation is not its principal's any more does not
// resolve: bumping it ends every credential of the principal at once,
// without touching the rows.

// passwordsMigration is the number of the migration that adds passwords,
// the authentication generation and the operator-authentication tables. A
// constant, so renumbering it when another migration lands first is one line
// here and one place in the list.
const passwordsMigration = 32

// Audit actions written by the password store (design §7.3). None of them
// carries a password, a record, a challenge or an address.
const (
	// AuditLogin is a credential issued by a password sign-in: actor and
	// subject the principal, detail the credential id.
	AuditLogin = "login"
	// AuditPasswordSet is a password stored for a principal that had none,
	// or replaced without the current one (detail says how).
	AuditPasswordSet = "password_set"
	// AuditAuthGenerationBumped is the principal's authentication generation
	// moving on: detail the cause and the new generation.
	AuditAuthGenerationBumped = "auth_generation_bumped"
)

// passwordRecordPrefix is what every stored record starts with. The store
// refuses anything else, so a bug upstream can never store a password in
// the clear.
const passwordRecordPrefix = "$argon2id$"

var (
	// ErrLoginStale is a sign-in whose verification no longer holds by the
	// time it would be issued: the principal was disabled, hidden or changed
	// role, its password or authentication generation changed, or (for an
	// operator) its second factor is gone, or the credential a request came
	// with was ended. On a sign-in route (no credential yet) the server
	// answers it as any failed sign-in, auth_failed. On an authenticated
	// route it answers it as a dead credential, unauthenticated, never
	// auth_failed: there that code means only a wrong secret, and clients
	// tell the two apart by it (issue #814).
	ErrLoginStale = errors.New("store: sign-in no longer matches the account")
	// ErrPasswordRecordInvalid is a record that is not an Argon2id record.
	ErrPasswordRecordInvalid = errors.New("store: not a password record")
	// ErrPasswordNotAllowed is a password for a principal that cannot have
	// one: an agent.
	ErrPasswordNotAllowed = errors.New("store: this principal cannot have a password")
)

// LoginCandidate is what a sign-in needs to know about the principal a typed
// name names, read without a write transaction.
type LoginCandidate struct {
	Principal Principal
	// Record is the principal's password record, empty when it has none.
	Record string
	// AuthGeneration is the generation the record was read under.
	AuthGeneration int64
	// TOTPEnrolled says the principal has a confirmed second factor.
	TOTPEnrolled bool
}

// IsGuestTarget reports whether the candidate is a guest, or was one whose
// visit ended: neither may sign in with a password, whatever rows exist. A
// converted guest is a member and may.
func (c LoginCandidate) IsGuestTarget() bool { return c.Principal.guestTarget() }

// LoginCandidate returns the principal name signs in as: the one whose
// case-folded name key is the typed name's. A database made before name keys
// were enforced can hold two names with one key ("Alice" and "alice", issue
// #441); then only the exact spelling signs in, and a spelling that matches
// neither signs in as nobody. found is false, with no error, when no
// principal answers to name. Nothing is written.
func (s *Store) LoginCandidate(ctx context.Context, name string) (LoginCandidate, bool, error) {
	key := names.KeysOf(name).Folded
	if key == "" {
		return LoginCandidate{}, false, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.kind, p.name, p.role, p.owner, p.created_at, p.disabled_at, p.hidden_at, p.invited_by, p.invite_id,
		        p.may_invite, p.voice_allowed, p.auth_generation, COALESCE(pw.hash, ''),
		        t.enrolled_at IS NOT NULL AND t.secret IS NOT NULL,
		        (SELECT state FROM guest_visits WHERE principal_id = p.id)
		 FROM principals p
		 LEFT JOIN passwords pw ON pw.principal_id = p.id
		 LEFT JOIN totp t ON t.principal_id = p.id
		 WHERE p.name_key = ? ORDER BY p.id LIMIT 8`, key)
	if err != nil {
		return LoginCandidate{}, false, fmt.Errorf("store: login candidate: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var found []LoginCandidate
	for rows.Next() {
		var (
			c                                       LoginCandidate
			kind, role                              string
			createdAt                               int64
			disabledAt, hiddenAt, invitedBy, invite sql.NullInt64
			guest                                   sql.NullString
		)
		if err := rows.Scan(&c.Principal.ID, &kind, &c.Principal.Name, &role, &c.Principal.Owner, &createdAt, &disabledAt, &hiddenAt,
			&invitedBy, &invite, &c.Principal.MayInvite, &c.Principal.VoiceAllowed, &c.AuthGeneration, &c.Record, &c.TOTPEnrolled,
			&guest); err != nil {
			return LoginCandidate{}, false, fmt.Errorf("store: login candidate: scan: %w", err)
		}
		c.Principal.Kind = PrincipalKind(kind)
		c.Principal.Role = Role(role)
		c.Principal.CreatedAt = time.UnixMilli(createdAt)
		c.Principal.InvitedBy = invitedBy.Int64
		c.Principal.InviteID = invite.Int64
		c.Principal.GuestState = schema.GuestState(guest.String)
		if disabledAt.Valid {
			t := time.UnixMilli(disabledAt.Int64)
			c.Principal.DisabledAt = &t
		}
		if hiddenAt.Valid {
			t := time.UnixMilli(hiddenAt.Int64)
			c.Principal.HiddenAt = &t
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return LoginCandidate{}, false, fmt.Errorf("store: login candidate: %w", err)
	}
	switch {
	case len(found) == 1:
		return found[0], true, nil
	case len(found) > 1:
		for _, c := range found {
			if c.Principal.Name == name {
				return c, true, nil
			}
		}
	}
	return LoginCandidate{}, false, nil
}

// LoginIssue is what a verified sign-in asks the store to honour: the
// principal, and the generation and record its password was verified
// against. Rehash, when set, replaces that record in the issuing
// transaction (a record of older parameters, upgraded); it is written only
// if the record is still the verified one.
type LoginIssue struct {
	PrincipalID    int64
	AuthGeneration int64
	Record         string
	Rehash         string
}

// IssuedLogin is a member's credential from a password sign-in. Token is the
// plaintext, returned once.
type IssuedLogin struct {
	Principal  Principal
	Credential schema.CredentialV1
	Token      string
}

// IssueLoginCredential issues a member the credential a verified password
// earned, in one IMMEDIATE transaction that first re-checks the sign-in
// (recheckLoginTx): anything that changed since the verification is
// ErrLoginStale and writes nothing. The credential is a member's renewable
// one, labelled "password sign-in", with the lifetime of the invite the
// member came through (or the default), as a sign-in link's is; login and
// credential_created are audited with it.
func (s *Store) IssueLoginCredential(ctx context.Context, in LoginIssue, now time.Time) (IssuedLogin, error) {
	now = now.Truncate(time.Millisecond)
	token, err := newCredentialToken()
	if err != nil {
		return IssuedLogin{}, err
	}
	var out IssuedLogin
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out = IssuedLogin{}
		p, err := recheckLoginTx(ctx, tx, in, RoleMember, now)
		if err != nil {
			return err
		}
		lifetime, err := signInLinkCredentialLifetimeTx(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		exp := now.Add(lifetime)
		cred, err := insertCredentialTx(ctx, tx, p.ID, "password sign-in", token, now, &exp, lifetime)
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

// PendingLogin is the challenge an operator's verified password yields.
// Challenge is the plaintext, returned once; only its SHA-256 is stored.
type PendingLogin struct {
	Challenge string
	ExpiresAt time.Time
}

// CreatePendingLogin records the challenge an operator's verified password
// earns instead of a credential (design §5.3): one use, bound to the
// principal, its authentication generation and addressKey, and good for
// schema.PendingLoginLifetimeSeconds. addressKey is opaque here: the server
// passes a keyed HMAC of the client address, never the address. It
// re-checks the sign-in as IssueLoginCredential does, and also that the
// operator has a confirmed second factor: an operator whose setup is
// incomplete gets ErrLoginStale, never a challenge. Nothing is audited here:
// the sign-in is audited when the second factor completes it.
func (s *Store) CreatePendingLogin(ctx context.Context, in LoginIssue, addressKey string, now time.Time) (PendingLogin, error) {
	now = now.Truncate(time.Millisecond)
	challenge, err := newOpaqueSecret(schema.PendingLoginPrefix, schema.PendingLoginRandomBytes)
	if err != nil {
		return PendingLogin{}, err
	}
	exp := now.Add(schema.PendingLoginLifetimeSeconds * time.Second)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		if _, err := recheckLoginTx(ctx, tx, in, RoleOperator, now); err != nil {
			return err
		}
		var enrolled bool
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM totp WHERE principal_id = ? AND enrolled_at IS NOT NULL AND secret IS NOT NULL)",
			in.PrincipalID).Scan(&enrolled); err != nil {
			return fmt.Errorf("store: pending login: totp: %w", err)
		}
		if !enrolled {
			return ErrLoginStale
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pending_logins (challenge_hash, principal_id, address_key, auth_generation, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			hashCredentialToken(challenge), in.PrincipalID, addressKey, in.AuthGeneration, now.UnixMilli(), exp.UnixMilli()); err != nil {
			return fmt.Errorf("store: pending login: %w", err)
		}
		return nil
	})
	if err != nil {
		return PendingLogin{}, err
	}
	return PendingLogin{Challenge: challenge, ExpiresAt: exp}, nil
}

// recheckLoginTx re-reads, inside the issuing transaction, everything a
// verification outside it relied on, and applies a pending rehash. It
// returns the principal as it is now, or ErrLoginStale.
func recheckLoginTx(ctx context.Context, tx execer, in LoginIssue, role Role, now time.Time) (Principal, error) {
	p, found, err := queryPrincipal(ctx, tx, in.PrincipalID)
	if err != nil {
		return Principal{}, err
	}
	// A guest, live or ended, never signs in with a password, even if a row
	// for it exists (issue #705); a converted one is a member.
	if !found || p.Kind != PrincipalHuman || p.Role != role || p.DisabledAt != nil || p.HiddenAt != nil || p.guestTarget() {
		return Principal{}, ErrLoginStale
	}
	var (
		generation int64
		record     sql.NullString
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT p.auth_generation, pw.hash FROM principals p LEFT JOIN passwords pw ON pw.principal_id = p.id WHERE p.id = ?`,
		in.PrincipalID).Scan(&generation, &record); err != nil {
		return Principal{}, fmt.Errorf("store: recheck login: %w", err)
	}
	if generation != in.AuthGeneration || !record.Valid || in.Record == "" || record.String != in.Record {
		return Principal{}, ErrLoginStale
	}
	if in.Rehash != "" {
		if !strings.HasPrefix(in.Rehash, passwordRecordPrefix) {
			return Principal{}, ErrPasswordRecordInvalid
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE passwords SET hash = ?, updated_at = ? WHERE principal_id = ? AND hash = ?",
			in.Rehash, now.UnixMilli(), in.PrincipalID, in.Record); err != nil {
			return Principal{}, fmt.Errorf("store: recheck login: rehash: %w", err)
		}
	}
	return p, nil
}

// setPasswordTx stores record as principalID's password, replacing any it
// had, and reports whether it replaced one. It is the enrolment primitive
// every route that sets a password builds on, inside its own transaction,
// which also audits and decides whether the generation moves: storing a
// first password ends nothing, and a change or reset bumps the generation
// (bumpAuthGenerationTx) in the same transaction.
func setPasswordTx(ctx context.Context, tx execer, principalID int64, record string, now time.Time) (replaced bool, err error) {
	if !strings.HasPrefix(record, passwordRecordPrefix) {
		return false, ErrPasswordRecordInvalid
	}
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM passwords WHERE principal_id = ?)", principalID).Scan(&replaced); err != nil {
		return false, fmt.Errorf("store: set password: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO passwords (principal_id, hash, created_at, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET hash = excluded.hash, updated_at = excluded.updated_at`,
		principalID, record, now.UnixMilli(), now.UnixMilli()); err != nil {
		return false, fmt.Errorf("store: set password: %w", err)
	}
	return replaced, nil
}

// SetPassword stores record as principalID's password in a transaction of
// its own and audits password_set, its detail via (what set it, such as
// "enrollment") and whether a password was replaced. Replacing one moves
// the authentication generation on in the same transaction, so nothing
// issued under the old password outlives it; storing a first one ends
// nothing. Only a person has a password: an agent is
// ErrPasswordNotAllowed, and a guest whose visit is active or ended
// ErrGuestNotAllowed. An unknown principal is ErrPrincipalNotFound, and a
// record that is not one ErrPasswordRecordInvalid; neither writes anything.
func (s *Store) SetPassword(ctx context.Context, actor string, principalID int64, record, via string, now time.Time) (replaced bool, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		p, found, err := queryPrincipal(ctx, tx, principalID)
		switch {
		case err != nil:
			return err
		case !found:
			return ErrPrincipalNotFound
		case p.Kind != PrincipalHuman:
			return ErrPasswordNotAllowed
		case p.guestTarget():
			// A guest, live or ended, has no password; conversion makes a
			// member first (issue #705).
			return ErrGuestNotAllowed
		}
		if replaced, err = setPasswordTx(ctx, tx, principalID, record, now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPasswordSet, principalActor(principalID),
			fmt.Sprintf("via=%s replaced=%t", via, replaced), now); err != nil {
			return err
		}
		if replaced {
			_, err = bumpAuthGenerationTx(ctx, tx, actor, principalID, "password_replaced", now)
		}
		return err
	})
	return replaced, err
}

// bumpAuthGenerationTx moves principalID's authentication generation on,
// which ends every credential, link, pending login and setup token issued
// under the old one, and clears every re-authentication proof of its
// credentials. It audits auth_generation_bumped with cause and returns the
// new generation. The generation check alone refuses what was issued
// before; the principal's pending logins are also marked spent and its open
// links closed (reason auth_changed, issue #708), so their rows and the
// audit trail say so too. Live sockets and media are not reached from here:
// a caller that bumps also drops them through the server's revocation hooks,
// as a revocation does.
func bumpAuthGenerationTx(ctx context.Context, tx execer, actor string, principalID int64, cause string, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, "UPDATE principals SET auth_generation = auth_generation + 1 WHERE id = ?", principalID)
	if err != nil {
		return 0, fmt.Errorf("store: bump auth generation: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("store: bump auth generation: %w", err)
	} else if n != 1 {
		return 0, ErrPrincipalNotFound
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE credentials SET recent_auth_until = NULL WHERE principal_id = ? AND recent_auth_until IS NOT NULL", principalID); err != nil {
		return 0, fmt.Errorf("store: bump auth generation: clear proofs: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE pending_logins SET spent_at = ? WHERE principal_id = ? AND spent_at IS NULL", now.UnixMilli(), principalID); err != nil {
		return 0, fmt.Errorf("store: bump auth generation: spend pending logins: %w", err)
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, "SELECT auth_generation FROM principals WHERE id = ?", principalID).Scan(&generation); err != nil {
		return 0, fmt.Errorf("store: bump auth generation: %w", err)
	}
	if err := appendAuditEventTx(ctx, tx, actor, AuditAuthGenerationBumped, principalActor(principalID),
		fmt.Sprintf("cause=%s generation=%d", cause, generation), now); err != nil {
		return 0, err
	}
	if err := closeOpenSignInLinksTx(ctx, tx, actor, principalID, false, SignInLinkCloseAuthChanged, now); err != nil {
		return 0, err
	}
	return generation, nil
}

// BumpAuthGeneration is bumpAuthGenerationTx in a transaction of its own. It
// returns ErrPrincipalNotFound for an unknown principal.
func (s *Store) BumpAuthGeneration(ctx context.Context, actor string, principalID int64, cause string, now time.Time) (int64, error) {
	now = now.Truncate(time.Millisecond)
	var generation int64
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var err error
		generation, err = bumpAuthGenerationTx(ctx, tx, actor, principalID, cause, now)
		return err
	})
	return generation, err
}

// Enrolment, change, reset and the recent-password proof (issue #708,
// design §5.2, §6.1, §7.2). As for a sign-in, the server verifies and hashes
// outside any transaction, and each write below re-reads, in its IMMEDIATE
// transaction, everything that work relied on. A change or a reset moves the
// authentication generation on (bumpAuthGenerationTx) and revokes the
// principal's other credentials in the same transaction, so nothing issued
// under the old password outlives it; the request's own session, when it
// survives, is rotated, and a reset issues no credential at all.

// Audit actions of enrolment, change, reset and re-authentication. None
// carries a password, a record, a code or a token.
const (
	// AuditPasswordChanged is a password replaced by its holder, who proved
	// the current one: detail the credential the request came with.
	AuditPasswordChanged = "password_changed"
	// AuditPasswordReset is a password replaced through a reset link: actor
	// the link, detail its id and whether a password was replaced.
	AuditPasswordReset = "password_reset"
	// AuditPasswordResetIssued and AuditEnrollmentIssued are the making of a
	// reset or an enrollment link (CreateSignInLink).
	AuditPasswordResetIssued = "password_reset_issued"
	AuditEnrollmentIssued    = "enrollment_issued"
	// AuditReauth is a recent-password proof recorded: detail the
	// credential it is bound to.
	AuditReauth = "reauth"
)

var (
	// ErrSetupOnHost is an operator setting a password on an ordinary
	// credential: an operator's first password (or a replacement) comes only
	// through a host-made setup token (security review F7).
	ErrSetupOnHost = errors.New("store: an operator's password is set only through a host-made setup token")
	// ErrPasswordAlreadySet is an enrolment on an ordinary credential for a
	// principal that has a password: they change it instead.
	ErrPasswordAlreadySet = errors.New("store: the principal already has a password")
	// ErrPasswordNotSet is a change or a re-authentication for a principal
	// that has no password yet.
	ErrPasswordNotSet = errors.New("store: the principal has no password")
	// ErrEnrollmentProofRequired is a member's first password with no
	// enrollment link: a long-lived bearer alone must not set one.
	ErrEnrollmentProofRequired = errors.New("store: a first password needs an operator's enrollment link")
	// ErrSecondFactorRequired is an operator's password change or
	// re-authentication with no TOTP code to check: it needs one as well
	// (design §5.2, §5.3), so it is refused whatever else is right.
	ErrSecondFactorRequired = errors.New("store: an operator's password change needs a second factor")
	// ErrNotASession is a recent-password proof asked for on a script
	// credential: the proof is for sign-in sessions only (design §7.2, the
	// S15 boundary).
	ErrNotASession = errors.New("store: a recent-password proof is kept on a sign-in session only")
	// ErrSetupCredentialRestricted is a password sent on a setup credential
	// that has already set one where password-only is allowed: it keeps only the
	// authenticator routes and its own end (ADR-011 item 2.1), so nothing
	// left behind can replace the password.
	ErrSetupCredentialRestricted = errors.New("store: this setup credential has set the password and may only enrol an authenticator")
)

// signInSessionSQL is the condition, over a credential's bare columns,
// that it is a sign-in session and not a script credential: a person's
// browser or CLI sign-in is renewable (an invite, a sign-in link, a password
// sign-in) or idle-bounded (a guest's, an operator's browser session), and a
// credential an operator issues for a script is neither unless asked to be.
const signInSessionSQL = `(renew_ms IS NOT NULL OR idle_ms IS NOT NULL)`

// PasswordSubject is what the password routes and whoami know of a caller
// and the credential its request came with, read without a write
// transaction. A route that writes acts on it only through a store call that
// reads it again.
type PasswordSubject struct {
	Principal Principal
	// Scope is the credential's. Session says it is a sign-in session
	// (signInSessionSQL), not a script credential.
	Scope   schema.CredentialScope
	Session bool
	// Record is the principal's password record, empty when it has none;
	// AuthGeneration is the generation it was read under.
	Record         string
	AuthGeneration int64
	// TOTPEnrolled says the principal has a confirmed second factor.
	TOTPEnrolled bool
	// PasswordOnlyAllowed is the stored flag (storedPasswordOnlyTx): the
	// server's last start allowed password-only operators (ADR-012 item 3).
	// What a client is told, whoami's setup_complete above all, reads it, so
	// a server whose configuration and store disagree never tells a setup
	// page "complete" while the store would refuse the operator.
	PasswordOnlyAllowed bool
	// ExpiresAt and IdleExpiresAt bound the credential, when it has those
	// bounds; RecentAuthUntil is its recent-password proof, when one stands
	// at the instant asked about.
	ExpiresAt, IdleExpiresAt, RecentAuthUntil *time.Time
}

// PasswordSubject reads principalID and its credential credentialID as of
// now. A credential that is not the principal's is ErrCredentialNotFound,
// an unknown principal ErrPrincipalNotFound.
func (s *Store) PasswordSubject(ctx context.Context, principalID, credentialID int64, now time.Time) (PasswordSubject, error) {
	var (
		out                            PasswordSubject
		owner                          int64
		scope                          string
		expiresAt, idleExpires, recent sql.NullInt64
	)
	switch err := s.db.QueryRowContext(ctx,
		`SELECT principal_id, scope, `+signInSessionSQL+`, expires_at, `+idleExpirySQL+`, recent_auth_until
		 FROM credentials WHERE id = ?`, credentialID,
	).Scan(&owner, &scope, &out.Session, &expiresAt, &idleExpires, &recent); {
	case errors.Is(err, sql.ErrNoRows):
		return PasswordSubject{}, ErrCredentialNotFound
	case err != nil:
		return PasswordSubject{}, fmt.Errorf("store: password subject: credential: %w", err)
	}
	if owner != principalID {
		return PasswordSubject{}, ErrCredentialNotFound
	}
	p, found, err := queryPrincipal(ctx, s.db, principalID)
	switch {
	case err != nil:
		return PasswordSubject{}, err
	case !found:
		return PasswordSubject{}, ErrPrincipalNotFound
	}
	var record sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT p.auth_generation, pw.hash,
		        EXISTS (SELECT 1 FROM totp t WHERE t.principal_id = p.id AND t.enrolled_at IS NOT NULL AND t.secret IS NOT NULL)
		 FROM principals p LEFT JOIN passwords pw ON pw.principal_id = p.id WHERE p.id = ?`, principalID,
	).Scan(&out.AuthGeneration, &record, &out.TOTPEnrolled); err != nil {
		return PasswordSubject{}, fmt.Errorf("store: password subject: %w", err)
	}
	if out.PasswordOnlyAllowed, err = storedPasswordOnlyTx(ctx, s.db); err != nil {
		return PasswordSubject{}, err
	}
	out.Principal, out.Scope, out.Record = p, schema.CredentialScope(scope), record.String
	out.ExpiresAt, out.IdleExpiresAt = millisPtr(expiresAt), millisPtr(idleExpires)
	if recent.Valid && recent.Int64 > now.UnixMilli() {
		out.RecentAuthUntil = millisPtr(recent)
	}
	return out, nil
}

func millisPtr(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}
	t := time.UnixMilli(ms.Int64)
	return &t
}

// passwordPrincipalTx reads principalID for a password write: a person, not
// a guest (live or ended), enabled and not hidden. Otherwise it is
// ErrPrincipalNotFound, ErrPasswordNotAllowed, ErrGuestNotAllowed,
// ErrPrincipalDisabled or ErrPrincipalHidden.
func passwordPrincipalTx(ctx context.Context, tx execer, principalID int64) (Principal, error) {
	p, found, err := queryPrincipal(ctx, tx, principalID)
	switch {
	case err != nil:
		return Principal{}, err
	case !found:
		return Principal{}, ErrPrincipalNotFound
	case p.Kind != PrincipalHuman:
		return Principal{}, ErrPasswordNotAllowed
	case p.guestTarget():
		return Principal{}, ErrGuestNotAllowed
	case p.HiddenAt != nil:
		return Principal{}, ErrPrincipalHidden
	case p.DisabledAt != nil:
		return Principal{}, ErrPrincipalDisabled
	}
	return p, nil
}

// callerCredential is the credential a password request came with, as it is
// inside the writing transaction.
type callerCredential struct {
	scope   schema.CredentialScope
	session bool
	// totpOnly says a setup credential has set its operator's password on a
	// server that allows password-only and keeps only the authenticator routes (ADR-011
	// item 2.1, migration 36).
	totpOnly bool
}

// callerCredentialTx re-reads the credential a request came with: it must be
// principalID's, unrevoked, unexpired, inside any idle bound and of the
// principal's present generation. Otherwise it is ErrLoginStale: the
// credential was ended while the server verified or hashed.
func callerCredentialTx(ctx context.Context, tx execer, principalID, credentialID int64, now time.Time) (callerCredential, error) {
	var (
		owner    int64
		scope    string
		session  bool
		totpOnly bool
		live     bool
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT c.principal_id, c.scope, (c.renew_ms IS NOT NULL OR c.idle_ms IS NOT NULL), c.setup_totp_only,
		        c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > ?)
		        AND (c.idle_ms IS NULL OR COALESCE(c.last_used_at, c.created_at) + c.idle_ms > ?)
		        AND c.auth_generation = p.auth_generation
		 FROM credentials c JOIN principals p ON p.id = c.principal_id WHERE c.id = ?`,
		now.UnixMilli(), now.UnixMilli(), credentialID,
	).Scan(&owner, &scope, &session, &totpOnly, &live); {
	case errors.Is(err, sql.ErrNoRows):
		return callerCredential{}, ErrLoginStale
	case err != nil:
		return callerCredential{}, fmt.Errorf("store: caller credential: %w", err)
	}
	if owner != principalID || !live {
		return callerCredential{}, ErrLoginStale
	}
	return callerCredential{scope: schema.CredentialScope(scope), session: session, totpOnly: totpOnly}, nil
}

// rotateSessionTx revokes credentialID and issues its principal a new one
// with the same label, expiry and renewal, audited credential_rotated as
// RotateCredential is; the old one's sessions end with it. The new one
// records the principal's generation as it is now, so a caller that bumped
// first gets a credential of the new generation. An idle bound is carried
// over too, counted since the new credential's creation and never past the
// kept expiry (issue #709): an operator's browser session keeps both its
// bounds. A guest never reaches it: a guest has no password.
func rotateSessionTx(ctx context.Context, tx execer, actor string, credentialID int64, token string, now time.Time, runs *[]EndedRun) (schema.CredentialV1, error) {
	var (
		principalID             int64
		label                   string
		expiresAt, renews, idle sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx,
		"SELECT principal_id, label, expires_at, renew_ms, idle_ms FROM credentials WHERE id = ?", credentialID,
	).Scan(&principalID, &label, &expiresAt, &renews, &idle); err != nil {
		return schema.CredentialV1{}, fmt.Errorf("store: rotate session: read: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), credentialID)
	if err != nil {
		return schema.CredentialV1{}, fmt.Errorf("store: rotate session: revoke: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return schema.CredentialV1{}, fmt.Errorf("store: rotate session: revoke: %w", err)
	} else if n != 1 {
		return schema.CredentialV1{}, ErrLoginStale
	}
	c, err := insertCredentialTx(ctx, tx, principalID, label, token, now, millisPtr(expiresAt), time.Duration(renews.Int64)*time.Millisecond)
	if err != nil {
		return schema.CredentialV1{}, err
	}
	if err := carryIdleBoundTx(ctx, tx, &c, idle, expiresAt, now); err != nil {
		return schema.CredentialV1{}, err
	}
	if err := appendAuditEventTx(ctx, tx, actor, "credential_rotated", principalActor(principalID),
		credentialDetail(c.ID, c.Label)+fmt.Sprintf(" replaces=%d", credentialID), now); err != nil {
		return schema.CredentialV1{}, err
	}
	if err := endCredentialSessionsTx(ctx, tx, actor, credentialID, SessionEndedByRevocation, now, runs); err != nil {
		return schema.CredentialV1{}, err
	}
	return c, nil
}

// carryIdleBoundTx gives c, a credential just inserted to replace another,
// the replaced one's idle bound idle, when it had one, so the replacement is
// a sign-in session exactly when the old one was (signInSessionSQL: renewal,
// which insertCredentialTx carries, or an idle bound). Without it a rotated
// operator browser session would become a script credential to every check
// that tells the two apart: the recent-auth gate, and the revocation when
// password-only stops being allowed (issue #866). The idle clock starts
// again at the rotation (last_used_at stays NULL, so it counts from
// created_at, now): the rotation is a use, as any REST request is, and the
// bound is never later than the kept expiry, which never moves.
func carryIdleBoundTx(ctx context.Context, tx execer, c *schema.CredentialV1, idle, expiresAt sql.NullInt64, now time.Time) error {
	if !idle.Valid {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE credentials SET idle_ms = ? WHERE id = ?", idle.Int64, c.ID); err != nil {
		return fmt.Errorf("store: rotate: idle bound: %w", err)
	}
	at := now.Add(time.Duration(idle.Int64) * time.Millisecond)
	if expiresAt.Valid && at.UnixMilli() > expiresAt.Int64 {
		at = time.UnixMilli(expiresAt.Int64)
	}
	ts := schema.NewTimestamp(at)
	c.IdleExpiresAt = &ts
	return nil
}

// spendPasswordLinkTx spends a reset or an enrollment link, once: the guarded
// update, not the read before it, decides which of two concurrent uses gets
// it, and the loser is ErrSignInLinkInvalid. Such a link issues no
// credential, and the table keeps used_at and credential_id together for
// the sign-in a link's use is (migration 30), so it is spent by closing it;
// the password_set or password_reset row of the same transaction is the
// record of the use, and no sign_in_link_closed row is written.
func spendPasswordLinkTx(ctx context.Context, tx execer, id int64, now time.Time) error {
	res, err := tx.ExecContext(ctx,
		"UPDATE sign_in_links SET closed_at = ? WHERE id = ? AND used_at IS NULL AND closed_at IS NULL", now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: spend sign-in link: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: spend sign-in link: %w", err)
	} else if n != 1 {
		return ErrSignInLinkInvalid
	}
	return nil
}

// PasswordEnrollment is a password POST /v1/password/mine stores, hashed by
// the server outside any transaction: the caller, the credential the request
// came with, the record, and ProofHash, the hash of the enrollment link's
// code, empty when none was given.
type PasswordEnrollment struct {
	PrincipalID, CredentialID int64
	Record                    string
	ProofHash                 string
}

// PasswordUpdate is what storing a password did.
type PasswordUpdate struct {
	PrincipalID int64
	// Credential and Token are the request's own session, rotated; Token is
	// empty when there is none: a reset issues none, and a setup-scoped
	// credential stays as it is.
	Credential schema.CredentialV1
	Token      string
	// Rotated is the credential the rotation revoked, 0 when none.
	Rotated int64
	// Revoked is how many other credentials of the principal were revoked.
	Revoked int
	// Bumped says the authentication generation moved on: the caller drops
	// the principal's live sockets and media, as after a revocation.
	Bumped bool
	// SetupEnded is the setup credential the write revoked because the
	// operator's setup is now complete (issue #709), 0 when none.
	SetupEnded int64
	// SetupRestricted is the setup credential that, where password-only is allowed,
	// completed its operator's setup with the password and now reaches only
	// the authenticator routes and its own end (ADR-011 item 2.1), 0 when
	// none.
	SetupRestricted int64
}

// EnrollPassword stores in.Record as the caller's password, by the rules of
// design §5.2 and §7.2, in one IMMEDIATE transaction that re-reads the
// principal (passwordPrincipalTx) and the credential (callerCredentialTx,
// else ErrLoginStale).
//
// On an ordinary, full-scope credential it is a member's first password: an
// operator is ErrSetupOnHost, a principal with a password
// ErrPasswordAlreadySet, and a member with no proof
// ErrEnrollmentProofRequired. The proof must be an enrollment link, still
// usable, made for this caller: anything else is ErrSignInLinkInvalid, the
// one answer for every link that does not work. The link is spent, nothing
// else of the member's ends (a first password replaces nothing), and the
// request's own session is rotated, so a copy of its token stops working.
//
// On a setup-scoped credential the host-made setup token was the proof: the
// password is set, replacing any the principal had (that is how an operator
// recovers, design §5.3), and the credential stays as it is. Replacing one
// moves the generation on and revokes every other credential, as a change
// does; the setup credential moves to the new generation with its
// principal, so the setup it is for can finish. When the operator already
// has a confirmed second factor, the password completes the setup and the
// setup credential is revoked (PasswordUpdate.SetupEnded). Where password-only
// is allowed (ADR-011 item 2.1, ADR-012) the password alone completes it: in the same
// transaction the setup credential is restricted to the authenticator routes
// and its own end (PasswordUpdate.SetupRestricted), and one already
// restricted is ErrSetupCredentialRestricted. A proof sent on it is held to
// the same rule as above.
//
// password_set is audited (via=enrollment or via=setup), with
// credential_rotated, or the bump and revocations, in the same transaction.
func (s *Store) EnrollPassword(ctx context.Context, actor string, in PasswordEnrollment, now time.Time) (PasswordUpdate, error) {
	now = now.Truncate(time.Millisecond)
	if !strings.HasPrefix(in.Record, passwordRecordPrefix) {
		return PasswordUpdate{}, ErrPasswordRecordInvalid
	}
	token, err := newCredentialToken()
	if err != nil {
		return PasswordUpdate{}, err
	}
	var (
		out  PasswordUpdate
		runs []EndedRun
	)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = PasswordUpdate{PrincipalID: in.PrincipalID}, nil
		p, err := passwordPrincipalTx(ctx, tx, in.PrincipalID)
		if err != nil {
			return err
		}
		cred, err := callerCredentialTx(ctx, tx, in.PrincipalID, in.CredentialID, now)
		if err != nil {
			return err
		}
		setup := cred.scope == schema.CredentialScopeSetup
		if setup && cred.totpOnly {
			// Checked again here, beside the middleware's route check, so two
			// requests in flight on one credential cannot both set it.
			return ErrSetupCredentialRestricted
		}
		if !setup {
			var has bool
			if err := tx.QueryRowContext(ctx,
				"SELECT EXISTS (SELECT 1 FROM passwords WHERE principal_id = ?)", in.PrincipalID).Scan(&has); err != nil {
				return fmt.Errorf("store: enroll password: %w", err)
			}
			switch {
			case p.Role == RoleOperator:
				return ErrSetupOnHost
			case has:
				return ErrPasswordAlreadySet
			case in.ProofHash == "":
				return ErrEnrollmentProofRequired
			}
		}
		var linkID int64
		if in.ProofHash != "" {
			l, err := usableLinkOfTx(ctx, tx, in.ProofHash, now, schema.SignInLinkPurposeEnrollment)
			if err != nil {
				return err
			}
			if l.principal.ID != in.PrincipalID {
				return ErrSignInLinkInvalid
			}
			if err := spendPasswordLinkTx(ctx, tx, l.id, now); err != nil {
				return err
			}
			linkID = l.id
		}
		replaced, err := setPasswordTx(ctx, tx, in.PrincipalID, in.Record, now)
		if err != nil {
			return err
		}
		via := "enrollment"
		if setup {
			via = "setup"
		}
		detail := fmt.Sprintf("via=%s replaced=%t", via, replaced)
		if linkID != 0 {
			detail += fmt.Sprintf(" sign_in_link=%d", linkID)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPasswordSet, principalActor(in.PrincipalID), detail, now); err != nil {
			return err
		}
		if !setup {
			c, err := rotateSessionTx(ctx, tx, actor, in.CredentialID, token, now, &runs)
			if err != nil {
				return err
			}
			out.Credential, out.Rotated = c, in.CredentialID
			return nil
		}
		if replaced {
			generation, err := bumpAuthGenerationTx(ctx, tx, actor, in.PrincipalID, "password_replaced", now)
			if err != nil {
				return err
			}
			out.Bumped = true
			if out.Revoked, err = revokeCredentialsExceptTx(ctx, tx, actor, in.PrincipalID, in.CredentialID,
				"password replaced", SessionEndedByRevocation, now, &runs); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE credentials SET auth_generation = ? WHERE id = ?", generation, in.CredentialID); err != nil {
				return fmt.Errorf("store: enroll password: %w", err)
			}
		}
		// With a confirmed second factor already, the password completes the
		// setup: the setup credential has done its work and ends here, so
		// it cannot replace the password again (issue #709).
		enrolled, err := totpEnrolledTx(ctx, tx, in.PrincipalID)
		if err != nil {
			return err
		}
		if enrolled {
			if err := endSetupCredentialTx(ctx, tx, actor, in.PrincipalID, in.CredentialID, now, &runs); err != nil {
				return err
			}
			out.SetupEnded = in.CredentialID
			return nil
		}
		// Where password-only is allowed the password alone completes it
		// (ADR-011 item 2.1, ADR-012 item 3, read from the stored flag). The
		// credential stays, so the authenticator step stays on offer, but
		// loses the right to set a password, in this transaction: nothing
		// left behind can replace it.
		allowed, err := storedPasswordOnlyTx(ctx, tx)
		if err != nil || !allowed {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE credentials SET setup_totp_only = 1 WHERE id = ? AND scope = 'setup'", in.CredentialID); err != nil {
			return fmt.Errorf("store: enroll password: restrict the setup credential: %w", err)
		}
		out.SetupRestricted = in.CredentialID
		return nil
	})
	if err != nil {
		return PasswordUpdate{}, err
	}
	s.announceRunsEnded(runs)
	if out.Rotated != 0 {
		out.Token = token
	}
	return out, nil
}

// CheckEnrollmentProof says, without using it or writing anything, whether
// the link with this code hash is a usable enrollment link made for
// principalID, or returns ErrSignInLinkInvalid as EnrollPassword would. The
// server asks before it hashes, so a proof that does not work costs no
// Argon2 work.
func (s *Store) CheckEnrollmentProof(ctx context.Context, codeHash string, principalID int64, now time.Time) error {
	l, err := usableLinkOfTx(ctx, s.db, codeHash, now, schema.SignInLinkPurposeEnrollment)
	if err != nil {
		return err
	}
	if l.principal.ID != principalID {
		return ErrSignInLinkInvalid
	}
	return nil
}

// PasswordChange is a change PUT /v1/password/mine verified outside any
// transaction: the current password matched Record under AuthGeneration,
// and NewRecord is the new one's.
type PasswordChange struct {
	PrincipalID, CredentialID int64
	AuthGeneration            int64
	Record, NewRecord         string
	// SecondFactor checks an operator's TOTP code (issue #709); a member's
	// change has none. An operator's change without it is
	// ErrSecondFactorRequired, unless the stored flag allows password-only and the
	// operator has no confirmed second factor (ADR-011 item 2.1).
	SecondFactor TOTPCheck
}

// ChangePassword replaces a member's password, in one IMMEDIATE transaction
// that first re-reads what the verification relied on (recheckLoginTx) and
// the credential the request came with, which must have full scope:
// anything changed since is ErrLoginStale and writes nothing. An operator's
// change needs a TOTP code as well (design §5.2): without in.SecondFactor it
// is ErrSecondFactorRequired, and a wrong or replayed code is
// ErrSecondFactorWrong; either writes nothing. An accepted code's step is
// recorded with the change. Where password-only is allowed an operator with no
// confirmed second factor changes it with the password alone
// (passwordOnlyOperatorTx, read in this transaction; ADR-011 item 2.1).
//
// The new password is stored and password_changed audited; the
// authentication generation moves on, which ends every pending login, link
// and recent-password proof of the member (bumpAuthGenerationTx); every
// other credential is revoked; and the request's own session is rotated,
// keeping its label and lifetime, so the member stays signed in where they
// changed it and nowhere else.
func (s *Store) ChangePassword(ctx context.Context, actor string, in PasswordChange, now time.Time) (PasswordUpdate, error) {
	now = now.Truncate(time.Millisecond)
	if !strings.HasPrefix(in.NewRecord, passwordRecordPrefix) {
		return PasswordUpdate{}, ErrPasswordRecordInvalid
	}
	token, err := newCredentialToken()
	if err != nil {
		return PasswordUpdate{}, err
	}
	var (
		out  PasswordUpdate
		runs []EndedRun
	)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = PasswordUpdate{PrincipalID: in.PrincipalID}, nil
		p, err := passwordPrincipalTx(ctx, tx, in.PrincipalID)
		if err != nil {
			return err
		}
		if p.Role == RoleOperator && in.SecondFactor == nil {
			if ok, err := passwordOnlyOperatorTx(ctx, tx, in.PrincipalID); err != nil {
				return err
			} else if !ok {
				return ErrSecondFactorRequired
			}
		}
		if _, err := recheckLoginTx(ctx, tx, LoginIssue{PrincipalID: in.PrincipalID, AuthGeneration: in.AuthGeneration, Record: in.Record},
			p.Role, now); err != nil {
			return err
		}
		cred, err := callerCredentialTx(ctx, tx, in.PrincipalID, in.CredentialID, now)
		if err != nil {
			return err
		}
		if cred.scope != schema.CredentialScopeFull {
			return ErrLoginStale
		}
		if p.Role == RoleOperator && in.SecondFactor != nil {
			if err := consumeTOTPTx(ctx, tx, in.PrincipalID, in.SecondFactor); err != nil {
				return err
			}
		}
		if _, err := setPasswordTx(ctx, tx, in.PrincipalID, in.NewRecord, now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPasswordChanged, principalActor(in.PrincipalID),
			fmt.Sprintf("credential=%d", in.CredentialID), now); err != nil {
			return err
		}
		if _, err := bumpAuthGenerationTx(ctx, tx, actor, in.PrincipalID, "password_changed", now); err != nil {
			return err
		}
		out.Bumped = true
		if out.Revoked, err = revokeCredentialsExceptTx(ctx, tx, actor, in.PrincipalID, in.CredentialID,
			"password changed", SessionEndedByRevocation, now, &runs); err != nil {
			return err
		}
		c, err := rotateSessionTx(ctx, tx, actor, in.CredentialID, token, now, &runs)
		if err != nil {
			return err
		}
		out.Credential, out.Rotated = c, in.CredentialID
		return nil
	})
	if err != nil {
		return PasswordUpdate{}, err
	}
	s.announceRunsEnded(runs)
	out.Token = token
	return out, nil
}

// CheckResetLink says, without using it or writing anything, whether the
// link with this code hash is a usable reset link, and whose: the person it
// is for, or ErrSignInLinkInvalid as ResetPassword would answer. The server
// asks before it hashes, so a code that does not work costs no Argon2 work,
// and checks the new password against the person's name.
func (s *Store) CheckResetLink(ctx context.Context, codeHash string, now time.Time) (Principal, error) {
	// A read, not a write transaction: this is an unauthenticated route.
	l, err := usableLinkOfTx(ctx, s.db, codeHash, now, schema.SignInLinkPurposeReset)
	if err != nil {
		return Principal{}, err
	}
	return l.principal, nil
}

// ResetPassword uses the reset link with this code hash to store record as
// its member's password, in one IMMEDIATE transaction: the link is checked
// again (a reset link, unused, unexpired, of the member's present
// generation, from a maker who is still an operator, for a member who may
// still be given one: guests, operators, disabled and hidden people are
// refused), the password stored, the link spent, password_reset audited,
// the generation moved on (which ends every other link, pending login and
// recent-password proof of the member) and every credential revoked. No
// credential is issued: the member signs in with the new password. Every
// failure is ErrSignInLinkInvalid and writes nothing; of two concurrent
// uses one wins. The audit rows are attributed to "sign_in_link:<id>", as
// a sign-in link's use is.
func (s *Store) ResetPassword(ctx context.Context, codeHash, record string, now time.Time) (PasswordUpdate, error) {
	now = now.Truncate(time.Millisecond)
	if !strings.HasPrefix(record, passwordRecordPrefix) {
		return PasswordUpdate{}, ErrPasswordRecordInvalid
	}
	var (
		out  PasswordUpdate
		runs []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = PasswordUpdate{}, nil
		l, err := usableLinkOfTx(ctx, tx, codeHash, now, schema.SignInLinkPurposeReset)
		if err != nil {
			return err
		}
		if err := spendPasswordLinkTx(ctx, tx, l.id, now); err != nil {
			return err
		}
		id := l.principal.ID
		replaced, err := setPasswordTx(ctx, tx, id, record, now)
		if err != nil {
			return err
		}
		actor := signInLinkSubject(l.id)
		if err := appendAuditEventTx(ctx, tx, actor, AuditPasswordReset, principalActor(id),
			fmt.Sprintf("sign_in_link=%d replaced=%t", l.id, replaced), now); err != nil {
			return err
		}
		if _, err := bumpAuthGenerationTx(ctx, tx, actor, id, "password_reset", now); err != nil {
			return err
		}
		out.PrincipalID, out.Bumped = id, true
		out.Revoked, err = revokeCredentialsExceptTx(ctx, tx, actor, id, 0, "password reset", SessionEndedByRevocation, now, &runs)
		return err
	})
	if err != nil {
		return PasswordUpdate{}, err
	}
	s.announceRunsEnded(runs)
	return out, nil
}

// RecordRecentAuth records the recent-password proof a verified
// re-authentication earned on credentialID, the credential the request came
// with and no other, until schema.RecentAuthLifetimeSeconds after now, and
// returns that instant. In one IMMEDIATE transaction it first re-checks the
// verification as a sign-in's is re-checked (recheckLoginTx, for a member;
// a pending rehash is applied) and the credential: it must be live, of the
// present generation, of full scope and a sign-in session, not a script
// credential (ErrNotASession). Anything changed since is ErrLoginStale.
// reauth is audited. Any later bump of the generation clears the proof, and
// revoking the credential ends it with the credential.
func (s *Store) RecordRecentAuth(ctx context.Context, in LoginIssue, credentialID int64, now time.Time) (time.Time, error) {
	return s.recordRecentAuth(ctx, in, credentialID, RoleMember, nil, now)
}

// recordRecentAuth is RecordRecentAuth for a principal of role; with a
// check, the second factor is consumed (consumeTOTPTx) after the credential
// is re-read, in the same transaction (RecordOperatorRecentAuth). An
// operator with no check is let through only by passwordOnlyOperatorTx, read
// in the same transaction (RecordPasswordOnlyOperatorRecentAuth), and is
// ErrSecondFactorRequired otherwise.
func (s *Store) recordRecentAuth(ctx context.Context, in LoginIssue, credentialID int64, role Role, check TOTPCheck, now time.Time) (time.Time, error) {
	now = now.Truncate(time.Millisecond)
	until := now.Add(schema.RecentAuthLifetimeSeconds * time.Second)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if _, err := recheckLoginTx(ctx, tx, in, role, now); err != nil {
			return err
		}
		cred, err := callerCredentialTx(ctx, tx, in.PrincipalID, credentialID, now)
		switch {
		case err != nil:
			return err
		case cred.scope != schema.CredentialScopeFull:
			return ErrLoginStale
		case !cred.session:
			return ErrNotASession
		}
		switch {
		case check != nil:
			if err := consumeTOTPTx(ctx, tx, in.PrincipalID, check); err != nil {
				return err
			}
		case role == RoleOperator:
			if ok, err := passwordOnlyOperatorTx(ctx, tx, in.PrincipalID); err != nil {
				return err
			} else if !ok {
				return ErrSecondFactorRequired
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE credentials SET recent_auth_until = ? WHERE id = ?", until.UnixMilli(), credentialID); err != nil {
			return fmt.Errorf("store: record recent auth: %w", err)
		}
		actor := principalActor(in.PrincipalID)
		return appendAuditEventTx(ctx, tx, actor, AuditReauth, actor, fmt.Sprintf("credential=%d", credentialID), now)
	})
	if err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// RecentAuthStands reports whether credentialID holds a recent-password
// proof at now: the credential is live (unrevoked, unexpired, its principal
// enabled, of the present generation and full scope) and its proof has not
// run out. It is what a route of the re-authentication class asks; making a
// device link (#712) asks it again inside its transaction (recentAuthTx).
// An unknown credential is false, with no error.
func (s *Store) RecentAuthStands(ctx context.Context, credentialID int64, now time.Time) (bool, error) {
	_, stands, err := recentAuthTx(ctx, s.db, credentialID, now)
	return stands, err
}

// recentAuthTx is RecentAuthStands on tx, and also says whose the credential
// is (0 for an unknown one), so a write that needs the proof
// (CreateDeviceLink) checks it, and that it is the caller's, inside its own
// transaction.
func recentAuthTx(ctx context.Context, tx execer, credentialID int64, now time.Time) (owner int64, stands bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT c.principal_id, c.recent_auth_until IS NOT NULL AND c.recent_auth_until > ?
		        AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > ?)
		        AND p.disabled_at IS NULL AND `+currentAuthSQL+`
		 FROM credentials c JOIN principals p ON p.id = c.principal_id WHERE c.id = ?`,
		now.UnixMilli(), now.UnixMilli(), credentialID).Scan(&owner, &stands)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("store: recent auth: %w", err)
	}
	return owner, stands, nil
}

// newOpaqueSecret returns prefix followed by n bytes from crypto/rand in
// unpadded base64url: the shape of every secret the server mints.
func newOpaqueSecret(prefix string, n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generate secret: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}
