package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Sign-in links (issue #493). A person who joined by invite has a lasting
// browser credential and no password: once they sign out, or clear the
// browser, nothing of theirs is left to sign in with. An operator makes them
// a sign-in link, a one-time code that, used, issues a new credential for
// that same person. It is the invite machinery (invites.go) pointed at an
// account that already exists: only the code's SHA-256 is stored, the
// plaintext is returned once by CreateSignInLink, a link is used at most
// once and expires, and every code that does not work is the one error
// ErrSignInLinkInvalid. No code, hash or token appears in an audit row, a
// log line or an error string.
//
// A link is for a member: a person who is neither an agent nor an operator.
// An operator's credential is not one a browser keeps to last (decision D2),
// and a leaked link must never be a way to an operator's rights.

// Audit actions written by the sign-in link store.
const (
	AuditSignInLinkCreated  = "sign_in_link_created"
	AuditSignInLinkRedeemed = "sign_in_link_redeemed"
	AuditSignInLinkClosed   = "sign_in_link_closed"
)

// Why a sign-in link was closed, the reason= of its sign_in_link_closed row:
// an operator closed it by hand (a link that leaked), the person it is for
// was disabled, or the operator who made it was.
const (
	SignInLinkCloseOperator          = "operator"
	SignInLinkClosePrincipalDisabled = "principal_disabled"
	SignInLinkCloseCreatorDisabled   = "creator_disabled"
)

// Errors returned by the sign-in link store, in addition to
// ErrPrincipalNotFound and ErrPrincipalDisabled.
var (
	// ErrSignInLinkInvalid is the single error for a code that is unknown,
	// expired, used or closed, made by someone who is no longer an enabled
	// operator, or for a person who may no longer sign in by link. The cases
	// are deliberately indistinguishable.
	ErrSignInLinkInvalid = errors.New("store: sign-in link invalid")
	// ErrSignInLinkNotAllowed is returned when the maker is not an enabled
	// human operator.
	ErrSignInLinkNotAllowed = errors.New("store: only an operator makes a sign-in link")
	// ErrSignInLinkTarget wraps the reason a principal cannot be given a
	// sign-in link: it is an agent, or an operator.
	ErrSignInLinkTarget = errors.New("store: sign-in links are for members")
	// ErrPrincipalHidden is returned when the principal a sign-in link would
	// be for is hidden (issue #445): a deleted account is not signed back in.
	ErrPrincipalHidden = errors.New("store: principal hidden")
	// ErrSignInLinkRequestInvalid wraps a request the schema refuses.
	ErrSignInLinkRequestInvalid = errors.New("store: invalid sign-in link request")
)

// RedeemedSignInLink is what using a sign-in link gave.
type RedeemedSignInLink struct {
	// Principal is the person the link was for, unchanged.
	Principal  Principal
	Credential schema.CredentialV1
	// Token is the credential's plaintext token, shown once.
	Token string
}

// NewSignInLinkCode returns a fresh sign-in link code: the "conch_sil_"
// prefix followed by 32 bytes from crypto/rand in unpadded base64url.
func NewSignInLinkCode() (string, error) {
	b := make([]byte, schema.SignInLinkCodeRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generate sign-in link code: %w", err)
	}
	return schema.SignInLinkCodePrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashSignInLinkCode returns the lowercase hex SHA-256 of code: the only form
// of it the store keeps, and what PreviewSignInLink and RedeemSignInLink take.
func HashSignInLinkCode(code string) string { return hashCredentialToken(code) }

func signInLinkSubject(id int64) string { return fmt.Sprintf("sign_in_link:%d", id) }

// signInLinkTargetTx checks that principalID may be given a sign-in link: a
// human member, enabled and not hidden. It returns the principal, or
// ErrPrincipalNotFound, ErrSignInLinkTarget, ErrPrincipalHidden or
// ErrPrincipalDisabled.
func signInLinkTargetTx(ctx context.Context, tx execer, principalID int64) (Principal, error) {
	p, err := principalByIDTx(ctx, tx, principalID)
	if err != nil {
		return Principal{}, err
	}
	switch {
	case p.Kind != PrincipalHuman:
		return Principal{}, fmt.Errorf("%w: an agent signs in with a credential an operator issues", ErrSignInLinkTarget)
	case p.Role == RoleOperator:
		return Principal{}, fmt.Errorf("%w: an operator signs in with a credential issued to them, not a link", ErrSignInLinkTarget)
	case p.guestTarget():
		// Issue #705: a guest's one credential is its visit's.
		return Principal{}, fmt.Errorf("%w: a guest has the one credential of its visit", ErrSignInLinkTarget)
	case p.HiddenAt != nil:
		return Principal{}, ErrPrincipalHidden
	case p.DisabledAt != nil:
		return Principal{}, ErrPrincipalDisabled
	}
	return p, nil
}

// CreateSignInLink makes a sign-in link for principalID, made by creatorID,
// and returns it with its code, which is shown only here. In one IMMEDIATE
// transaction with the insert it checks that the maker is an enabled human
// operator (ErrSignInLinkNotAllowed) and that the person may be given one
// (signInLinkTargetTx). sign_in_link_created is audited in the same
// transaction, attributed to actor, with the person as its subject. The code
// is not.
func (s *Store) CreateSignInLink(ctx context.Context, actor string, creatorID, principalID int64, req schema.CreateSignInLinkRequestV1, now time.Time) (schema.CreateSignInLinkResponseV1, error) {
	now = now.Truncate(time.Millisecond)
	if err := req.Validate(now); err != nil {
		return schema.CreateSignInLinkResponseV1{}, fmt.Errorf("%w: %w", ErrSignInLinkRequestInvalid, err)
	}
	expires := req.EffectiveExpiry(now).Truncate(time.Millisecond)
	code, err := NewSignInLinkCode()
	if err != nil {
		return schema.CreateSignInLinkResponseV1{}, err
	}
	var out schema.SignInLinkV1
	err = s.withImmediateTx(ctx, func(tx execer) error {
		creator, err := principalByIDTx(ctx, tx, creatorID)
		switch {
		case errors.Is(err, ErrPrincipalNotFound):
			return ErrSignInLinkNotAllowed
		case err != nil:
			return err
		}
		if creator.DisabledAt != nil || creator.Kind != PrincipalHuman || creator.Role != RoleOperator {
			return ErrSignInLinkNotAllowed
		}
		if _, err := signInLinkTargetTx(ctx, tx, principalID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO sign_in_links (code_hash, principal_id, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			HashSignInLinkCode(code), principalID, creatorID, now.UnixMilli(), expires.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: create sign-in link: insert: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create sign-in link: id: %w", err)
		}
		out = schema.SignInLinkV1{
			ID:          id,
			PrincipalID: principalID,
			CreatedBy:   creatorID,
			CreatedAt:   schema.NewTimestamp(now),
			ExpiresAt:   schema.NewTimestamp(expires),
		}
		return appendAuditEventTx(ctx, tx, actor, AuditSignInLinkCreated, principalActor(principalID),
			fmt.Sprintf("sign_in_link=%d expires=%s", id, out.ExpiresAt.Time().Format(time.RFC3339)), now)
	})
	if err != nil {
		return schema.CreateSignInLinkResponseV1{}, err
	}
	return schema.CreateSignInLinkResponseV1{SignInLink: out, Code: code}, nil
}

// usableSignInLinkTx finds the link whose code hashes to codeHash and returns
// its id and the person it opens, or ErrSignInLinkInvalid: for a code that
// is unknown, used, closed or expired, a maker who is no longer an enabled
// human operator, or a person who may no longer be given a link. The stored
// hash is compared in constant time even though the row was found by it, as
// for invites.
func usableSignInLinkTx(ctx context.Context, tx execer, codeHash string, now time.Time) (int64, Principal, error) {
	var (
		id, principalID, createdBy, expiresAt int64
		storedHash                            string
		usedAt, closedAt                      sql.NullInt64
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT id, code_hash, principal_id, created_by, expires_at, used_at, closed_at FROM sign_in_links WHERE code_hash = ?`, codeHash,
	).Scan(&id, &storedHash, &principalID, &createdBy, &expiresAt, &usedAt, &closedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, Principal{}, ErrSignInLinkInvalid
	case err != nil:
		return 0, Principal{}, fmt.Errorf("store: find sign-in link: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(codeHash)) != 1 {
		return 0, Principal{}, ErrSignInLinkInvalid
	}
	if usedAt.Valid || closedAt.Valid || expiresAt <= now.UnixMilli() {
		return 0, Principal{}, ErrSignInLinkInvalid
	}
	creator, err := principalByIDTx(ctx, tx, createdBy)
	if err != nil {
		return 0, Principal{}, fmt.Errorf("store: sign-in link maker: %w", err)
	}
	// The maker must still be able to stand behind it, as for an invite.
	if creator.DisabledAt != nil || creator.Kind != PrincipalHuman || creator.Role != RoleOperator {
		return 0, Principal{}, ErrSignInLinkInvalid
	}
	p, err := signInLinkTargetTx(ctx, tx, principalID)
	switch {
	case errors.Is(err, ErrSignInLinkTarget), errors.Is(err, ErrPrincipalHidden), errors.Is(err, ErrPrincipalDisabled):
		return 0, Principal{}, ErrSignInLinkInvalid
	case err != nil:
		return 0, Principal{}, fmt.Errorf("store: sign-in link principal: %w", err)
	}
	return id, p, nil
}

// PreviewSignInLink says whose account the link with this code hash opens,
// without using it, or returns ErrSignInLinkInvalid as RedeemSignInLink
// would. It writes nothing.
func (s *Store) PreviewSignInLink(ctx context.Context, codeHash string, now time.Time) (schema.SignInLinkPreviewV1, error) {
	// A read, not a write transaction: this is an unauthenticated route, and
	// it must not queue behind (or hold up) writers.
	_, p, err := usableSignInLinkTx(ctx, s.db, codeHash, now)
	if err != nil {
		return schema.SignInLinkPreviewV1{}, err
	}
	return schema.SignInLinkPreviewV1{Name: p.Name}, nil
}

// signInLinkCredentialLifetimeTx is how long the credential a link issues
// lives, and how far its expiry moves with each day's use: what the invite
// the person joined through gave its joiners, or an invite's default for a
// person no invite made. It is the rule redemption follows (issue #439).
func signInLinkCredentialLifetimeTx(ctx context.Context, tx execer, principalID int64) (time.Duration, error) {
	var lifetimeMs sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT i.credential_lifetime_ms FROM principals p LEFT JOIN invites i ON i.id = p.invite_id WHERE p.id = ?`, principalID,
	).Scan(&lifetimeMs); err != nil {
		return 0, fmt.Errorf("store: sign-in link credential lifetime: %w", err)
	}
	if !lifetimeMs.Valid || lifetimeMs.Int64 <= 0 {
		return schema.DefaultInviteCredentialLifetimeSeconds * time.Second, nil
	}
	return time.Duration(lifetimeMs.Int64) * time.Millisecond, nil
}

// RedeemSignInLink uses the link with this code hash, in one IMMEDIATE
// transaction: it checks the link (as PreviewSignInLink does), issues the
// person a new credential (label "sign-in link <id>", renewable with the
// lifetime signInLinkCredentialLifetimeTx gives), marks the link used with
// that credential, and audits sign_in_link_redeemed and credential_created.
// If anything fails nothing is written. Two concurrent uses of one link
// serialize on the write lock: one wins, and the other is
// ErrSignInLinkInvalid. The person's other credentials are left as they are.
//
// The audit rows are attributed to "sign_in_link:<id>": who used it is not
// known until it has worked.
func (s *Store) RedeemSignInLink(ctx context.Context, codeHash string, now time.Time) (RedeemedSignInLink, error) {
	now = now.Truncate(time.Millisecond)
	token, err := newCredentialToken()
	if err != nil {
		return RedeemedSignInLink{}, err
	}
	var out RedeemedSignInLink
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out = RedeemedSignInLink{}
		id, p, err := usableSignInLinkTx(ctx, tx, codeHash, now)
		if err != nil {
			return err
		}
		lifetime, err := signInLinkCredentialLifetimeTx(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		exp := now.Add(lifetime)
		cred, err := insertCredentialTx(ctx, tx, p.ID, fmt.Sprintf("sign-in link %d", id), token, now, &exp, lifetime)
		if err != nil {
			return err
		}
		// The guarded update, not the read above, decides who gets the use.
		res, err := tx.ExecContext(ctx,
			`UPDATE sign_in_links SET used_at = ?, credential_id = ? WHERE id = ? AND used_at IS NULL AND closed_at IS NULL`,
			now.UnixMilli(), cred.ID, id)
		if err != nil {
			return fmt.Errorf("store: redeem sign-in link: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: redeem sign-in link: %w", err)
		} else if n != 1 {
			return ErrSignInLinkInvalid
		}
		out.Principal, out.Credential = p, cred
		actor := signInLinkSubject(id)
		if err := appendAuditEventTx(ctx, tx, actor, AuditSignInLinkRedeemed, principalActor(p.ID),
			fmt.Sprintf("sign_in_link=%d credential=%d", id, cred.ID), now); err != nil {
			return err
		}
		return appendAuditEventTx(ctx, tx, actor, "credential_created", principalActor(p.ID),
			credentialDetail(cred.ID, cred.Label), now)
	})
	if err != nil {
		return RedeemedSignInLink{}, err
	}
	out.Token = token
	return out, nil
}

// CloseSignInLink closes one sign-in link so it can no longer be used: how an
// operator stops a link that leaked without disabling the person it is for.
// The store does not check who acts; the route is operator-only. Closing a
// link that is already closed or used changes nothing and writes no audit
// row (changed is false); an expired one is closed like an open one. An
// unknown id is ErrNotFound. sign_in_link_closed (reason=operator) is
// audited in the same transaction.
func (s *Store) CloseSignInLink(ctx context.Context, actor string, id int64, now time.Time) (changed bool, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		var principalID int64
		switch err := tx.QueryRowContext(ctx, "SELECT principal_id FROM sign_in_links WHERE id = ?", id).Scan(&principalID); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: close sign-in link %d: %w", id, ErrNotFound)
		case err != nil:
			return fmt.Errorf("store: close sign-in link: find: %w", err)
		}
		// The guard, not the read, decides: a use that won the write lock
		// first leaves nothing to close.
		res, err := tx.ExecContext(ctx,
			"UPDATE sign_in_links SET closed_at = ? WHERE id = ? AND used_at IS NULL AND closed_at IS NULL", now.UnixMilli(), id)
		if err != nil {
			return fmt.Errorf("store: close sign-in link: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: close sign-in link: %w", err)
		} else if n != 1 {
			return nil
		}
		changed = true
		return appendAuditEventTx(ctx, tx, actor, AuditSignInLinkClosed, principalActor(principalID),
			fmt.Sprintf("sign_in_link=%d reason=%s", id, SignInLinkCloseOperator), now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// closeSignInLinksTx closes every sign-in link that could still be used and
// is for principalID or was made by them, writing one sign_in_link_closed row
// each. It runs when the principal is disabled (disablePrincipalTx), so a
// link outstanding then stays dead after the principal is enabled again, as
// their credentials do. The caller holds the write lock.
func closeSignInLinksTx(ctx context.Context, tx execer, actor string, principalID int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, principal_id FROM sign_in_links
		 WHERE (principal_id = ? OR created_by = ?) AND used_at IS NULL AND closed_at IS NULL AND expires_at > ?
		 ORDER BY id`, principalID, principalID, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: list open sign-in links: %w", err)
	}
	type open struct{ id, principalID int64 }
	var found []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.principalID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: list open sign-in links: scan: %w", err)
		}
		found = append(found, o)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: list open sign-in links: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: list open sign-in links: %w", err)
	}
	for _, o := range found {
		if _, err := tx.ExecContext(ctx,
			"UPDATE sign_in_links SET closed_at = ? WHERE id = ? AND closed_at IS NULL", now.UnixMilli(), o.id); err != nil {
			return fmt.Errorf("store: close sign-in link %d: %w", o.id, err)
		}
		reason := SignInLinkClosePrincipalDisabled
		if o.principalID != principalID {
			reason = SignInLinkCloseCreatorDisabled
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditSignInLinkClosed, principalActor(o.principalID),
			fmt.Sprintf("sign_in_link=%d reason=%s", o.id, reason), now); err != nil {
			return err
		}
	}
	return nil
}
