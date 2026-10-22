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
	// operator) its second factor is gone. The server answers it as it
	// answers any failed sign-in.
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
		`SELECT p.id, p.kind, p.name, p.role, p.created_at, p.disabled_at, p.hidden_at, p.invited_by, p.invite_id,
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
		if err := rows.Scan(&c.Principal.ID, &kind, &c.Principal.Name, &role, &createdAt, &disabledAt, &hiddenAt,
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
// new generation. Live sockets and media are not reached from here: a caller
// that bumps also drops them through the server's revocation hooks, as a
// revocation does.
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
	var generation int64
	if err := tx.QueryRowContext(ctx, "SELECT auth_generation FROM principals WHERE id = ?", principalID).Scan(&generation); err != nil {
		return 0, fmt.Errorf("store: bump auth generation: %w", err)
	}
	if err := appendAuditEventTx(ctx, tx, actor, AuditAuthGenerationBumped, principalActor(principalID),
		fmt.Sprintf("cause=%s generation=%d", cause, generation), now); err != nil {
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

// newOpaqueSecret returns prefix followed by n bytes from crypto/rand in
// unpadded base64url: the shape of every secret the server mints.
func newOpaqueSecret(prefix string, n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generate secret: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}
