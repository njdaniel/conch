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
//
// Every link has a purpose (schema.SignInLinkPurpose, issue #708): a sign_in
// link signs the person in, a reset link lets them replace a forgotten
// password and an enrollment link proves their first one, and each route
// takes only the purposes it names, so no link does another's work. A link
// records its person's authentication generation when it is made and works
// only while that is still theirs: a password change or reset, a sign-out
// everywhere or a disable ends every link made before it.
//
// A device link (issue #712, design §6.2) is the one link a member makes,
// for their own account, to sign it in on another device: made under a
// recent-password proof of the session asking, at most
// schema.MaxOpenDeviceLinks open at once, ten minutes by default and an
// hour at most. It is redeemed as a sign_in link is, and works only while
// its maker is still the member it is for; the credential it issues
// carries the label its maker gave it.

// Audit actions written by the sign-in link store.
const (
	AuditSignInLinkCreated  = "sign_in_link_created"
	AuditSignInLinkRedeemed = "sign_in_link_redeemed"
	AuditSignInLinkClosed   = "sign_in_link_closed"
	// AuditSignInLinkSuspended and AuditSignInLinkResumed are the same for
	// sign-in, reset and enrolment links when act_as_members changes for
	// their maker (ADR-014 design §7.5, §8.5): actor system, subject the
	// link, detail reason=permission.
	AuditSignInLinkSuspended = "sign_in_link_suspended"
	AuditSignInLinkResumed   = "sign_in_link_resumed"
	// AuditDeviceLinkIssued is a member making a device link for
	// themselves (design §7.3).
	AuditDeviceLinkIssued = "device_link_issued"
	// AuditConversionPermitIssued is an operator letting one live guest
	// create an account although its origin's policy is closed (design §3.3,
	// §7.3): subject the guest, detail the permit's id and expiry.
	AuditConversionPermitIssued = "conversion_permit_issued"
)

// Why a sign-in link was closed, the reason= of its sign_in_link_closed row:
// an operator closed it by hand (a link that leaked), the person it is for
// was disabled, or the operator who made it was.
const (
	SignInLinkCloseOperator          = "operator"
	SignInLinkClosePrincipalDisabled = "principal_disabled"
	SignInLinkCloseCreatorDisabled   = "creator_disabled"
	// SignInLinkCloseAuthChanged: the person's authentication generation
	// moved on (bumpAuthGenerationTx), so a link made before it is dead.
	SignInLinkCloseAuthChanged = "auth_changed"
	// SignInLinkCloseSelf: the member closed a device link of their own.
	SignInLinkCloseSelf = "self"
	// SignInLinkCloseSignedOut: the member signed out of a session, which
	// ends the device links they have open (design §7.2).
	SignInLinkCloseSignedOut = "signed_out"
	// SignInLinkCloseCredentialRevoked: one of the member's credentials was
	// revoked by id, by the member or an operator, which ends the device
	// links they have open (design §6.2).
	SignInLinkCloseCredentialRevoked = "credential_revoked" // #nosec G101 -- an audit reason, not a credential
	// SignInLinkCloseConverted: the guest a conversion permit was for
	// created an account, so its other permits have nothing left to permit.
	SignInLinkCloseConverted = "converted"
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
	// ErrDeviceLinkNotAllowed is a device link asked for by someone who is
	// not a member who may be given a link: an agent, an operator, a guest,
	// a hidden or a disabled principal.
	ErrDeviceLinkNotAllowed = errors.New("store: only a member makes a device link")
	// ErrReauthRequired is a device link asked for on a credential that
	// holds no recent-password proof, or one that is not the caller's.
	ErrReauthRequired = errors.New("store: a recent password proof is required")
	// ErrTooManyDeviceLinks is a device link asked for by a member who
	// already holds schema.MaxOpenDeviceLinks open ones.
	ErrTooManyDeviceLinks = errors.New("store: too many open device links")
)

// signInLinkCreatedAction is the audit action of making a link of purpose p:
// a reset or an enrollment link is named for what it lets the person do
// (design §7.3), a sign-in link as it always was.
func signInLinkCreatedAction(p schema.SignInLinkPurpose) string {
	switch p {
	case schema.SignInLinkPurposeReset:
		return AuditPasswordResetIssued
	case schema.SignInLinkPurposeEnrollment:
		return AuditEnrollmentIssued
	case schema.SignInLinkPurposeConversion:
		return AuditConversionPermitIssued
	default:
		return AuditSignInLinkCreated
	}
}

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

// CreateSignInLink makes a link of the request's purpose for principalID,
// made by creatorID, and returns it with its code, which is shown only here.
// In one IMMEDIATE transaction with the insert it checks that the maker holds
// act_as_members (makerMayActAsTx, ErrSignInLinkNotAllowed) and that the
// person may be
// given one (signInLinkTargetTx, the same rule for every purpose); the row
// records the person's authentication generation as it is then.
// sign_in_link_created (password_reset_issued for a reset link,
// enrollment_issued for an enrollment one) is audited in the same
// transaction, attributed to actor, with the person as its subject. The code
// is not.
func (s *Store) CreateSignInLink(ctx context.Context, actor string, creatorID, principalID int64, req schema.CreateSignInLinkRequestV1, now time.Time, forbidOwner ...bool) (schema.CreateSignInLinkResponseV1, error) {
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
		switch ok, err := makerMayActAsTx(ctx, tx, creatorID); {
		case err != nil:
			return err
		case !ok:
			return ErrSignInLinkNotAllowed
		}
		if err := ownerProtectedTx(ctx, tx, principalID, len(forbidOwner) > 0 && forbidOwner[0]); err != nil {
			return err
		}
		if _, err := signInLinkTargetTx(ctx, tx, principalID); err != nil {
			return err
		}
		purpose := req.EffectivePurpose()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO sign_in_links (code_hash, principal_id, created_by, created_at, expires_at, purpose, auth_generation)
			 SELECT ?, ?, ?, ?, ?, ?, auth_generation FROM principals WHERE id = ?`,
			HashSignInLinkCode(code), principalID, creatorID, now.UnixMilli(), expires.UnixMilli(), string(purpose), principalID)
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
			Purpose:     purpose,
		}
		return appendAuditEventTx(ctx, tx, actor, signInLinkCreatedAction(purpose), principalActor(principalID),
			fmt.Sprintf("sign_in_link=%d expires=%s", id, out.ExpiresAt.Time().Format(time.RFC3339)), now)
	})
	if err != nil {
		return schema.CreateSignInLinkResponseV1{}, err
	}
	return schema.CreateSignInLinkResponseV1{SignInLink: out, Code: code}, nil
}

// usableLink is a link that still works: its id, purpose and label, and the
// person it is for.
type usableLink struct {
	id        int64
	purpose   schema.SignInLinkPurpose
	label     string
	principal Principal
}

// liveLink is a link whose row still works, before anything about its
// maker, its person or its purpose is checked.
type liveLink struct {
	id, principalID, createdBy int64
	purpose                    schema.SignInLinkPurpose
	label                      string
}

// liveSignInLinkTx finds the link whose code hashes to codeHash and returns
// it if its row still works: unused, unclosed, unexpired and made under its
// person's present authentication generation. Otherwise it is
// ErrSignInLinkInvalid. The stored hash is compared in constant time even
// though the row was found by it, as for invites. Who made it and whom it is
// for are the callers' to check (usableSignInLinkTx, usableConversionPermitTx).
func liveSignInLinkTx(ctx context.Context, tx execer, codeHash string, now time.Time) (liveLink, error) {
	var (
		l                   liveLink
		expiresAt           int64
		storedHash, purpose string
		label               sql.NullString
		usedAt, closedAt    sql.NullInt64
		current             bool
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT l.id, l.code_hash, l.principal_id, l.created_by, l.expires_at, l.used_at, l.closed_at, l.purpose, l.label,
		        l.auth_generation = p.auth_generation
		 FROM sign_in_links l JOIN principals p ON p.id = l.principal_id WHERE l.code_hash = ?`, codeHash,
	).Scan(&l.id, &storedHash, &l.principalID, &l.createdBy, &expiresAt, &usedAt, &closedAt, &purpose, &label, &current); {
	case errors.Is(err, sql.ErrNoRows):
		return liveLink{}, ErrSignInLinkInvalid
	case err != nil:
		return liveLink{}, fmt.Errorf("store: find sign-in link: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(codeHash)) != 1 {
		return liveLink{}, ErrSignInLinkInvalid
	}
	if usedAt.Valid || closedAt.Valid || expiresAt <= now.UnixMilli() || !current {
		return liveLink{}, ErrSignInLinkInvalid
	}
	l.purpose, l.label = schema.SignInLinkPurpose(purpose), label.String
	return l, nil
}

// makerMayActAsTx reports whether principal id holds act_as_members now: who
// must make, and stand behind at every redemption, a sign-in, reset or
// enrolment link (ADR-014 design §7.3, §7.5, N3). A link whose maker has lost
// it is suspended, not closed: it works again if the permission comes back.
// A principal with no row holds nothing.
func makerMayActAsTx(ctx context.Context, tx execer, id int64) (bool, error) {
	ok, err := permittedTx(ctx, tx, id, schema.PermActAsMembers)
	if err != nil {
		return false, fmt.Errorf("store: sign-in link maker: %w", err)
	}
	return ok, nil
}

// makerIsOperatorTx reports whether principal id is still an enabled human
// operator: who must make and stand behind a conversion permit. It is a role
// check, kept for the permits alone until #1005 retires them; every other
// link reads makerMayActAsTx.
func makerIsOperatorTx(ctx context.Context, tx execer, id int64) (bool, error) {
	creator, err := principalByIDTx(ctx, tx, id)
	if err != nil {
		return false, fmt.Errorf("store: sign-in link maker: %w", err)
	}
	return creator.DisabledAt == nil && creator.Kind == PrincipalHuman && creator.Role == RoleOperator, nil
}

// usableSignInLinkTx finds the link whose code hashes to codeHash and returns
// it if it still works, whatever its purpose (usableLinkOfTx checks that).
// Otherwise it is ErrSignInLinkInvalid: for a code that is unknown, used,
// closed or expired, a link made under an authentication generation that is
// no longer its person's (liveSignInLinkTx), a maker who can no longer stand
// behind it, or a person who may no longer be given a link. The maker of a
// device link must be the member it is for (and so, by the person's check,
// still a member); of any other, a holder of act_as_members now
// (makerMayActAsTx). A conversion
// permit is for a guest, whom no sign-in link is for, so it is never usable
// here: usableConversionPermitTx is its check.
func usableSignInLinkTx(ctx context.Context, tx execer, codeHash string, now time.Time) (usableLink, error) {
	l, err := liveSignInLinkTx(ctx, tx, codeHash, now)
	if err != nil {
		return usableLink{}, err
	}
	if l.purpose == schema.SignInLinkPurposeDevice {
		// A member makes a device link for themselves only; signInLinkTargetTx
		// below holds the maker, who is the person, to being a member still,
		// so one promoted to operator since cannot use it.
		if l.createdBy != l.principalID {
			return usableLink{}, ErrSignInLinkInvalid
		}
	} else {
		// The maker must still be able to stand behind it, as for an invite.
		if ok, err := makerMayActAsTx(ctx, tx, l.createdBy); err != nil {
			return usableLink{}, err
		} else if !ok {
			return usableLink{}, ErrSignInLinkInvalid
		}
	}
	p, err := signInLinkTargetTx(ctx, tx, l.principalID)
	switch {
	case errors.Is(err, ErrSignInLinkTarget), errors.Is(err, ErrPrincipalHidden), errors.Is(err, ErrPrincipalDisabled):
		return usableLink{}, ErrSignInLinkInvalid
	case err != nil:
		return usableLink{}, fmt.Errorf("store: sign-in link principal: %w", err)
	}
	return usableLink{id: l.id, purpose: l.purpose, label: l.label, principal: p}, nil
}

// usableLinkOfTx is usableSignInLinkTx for a route that takes only the
// purposes named: a link of any other is ErrSignInLinkInvalid, the answer for
// a code that does not work at all, so no route tells what another route's
// code is for.
func usableLinkOfTx(ctx context.Context, tx execer, codeHash string, now time.Time, purposes ...schema.SignInLinkPurpose) (usableLink, error) {
	l, err := usableSignInLinkTx(ctx, tx, codeHash, now)
	if err != nil {
		return usableLink{}, err
	}
	for _, p := range purposes {
		if l.purpose == p {
			return l, nil
		}
	}
	return usableLink{}, ErrSignInLinkInvalid
}

// PreviewSignInLink says whose account the link with this code hash opens,
// and what the link is for, without using it, or returns
// ErrSignInLinkInvalid as RedeemSignInLink (for a reset link,
// ResetPassword) would. An enrollment or a conversion link has no page to
// land on, so it previews as invalid. It writes nothing.
func (s *Store) PreviewSignInLink(ctx context.Context, codeHash string, now time.Time) (schema.SignInLinkPreviewV1, error) {
	// A read, not a write transaction: this is an unauthenticated route, and
	// it must not queue behind (or hold up) writers.
	l, err := usableLinkOfTx(ctx, s.db, codeHash, now,
		schema.SignInLinkPurposeSignIn, schema.SignInLinkPurposeDevice, schema.SignInLinkPurposeReset)
	if err != nil {
		return schema.SignInLinkPreviewV1{}, err
	}
	return schema.SignInLinkPreviewV1{Name: l.principal.Name, Purpose: l.purpose}, nil
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
// transaction: it checks the link (as PreviewSignInLink does, but only a
// sign_in or a device link signs anyone in: any other purpose is
// ErrSignInLinkInvalid), issues the person a new credential (label
// "sign-in link <id>", or for a device link the label its maker gave it,
// "device link <id>" when they gave none; renewable with the lifetime
// signInLinkCredentialLifetimeTx gives), marks the link used with that
// credential, and audits sign_in_link_redeemed and credential_created.
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
		l, err := usableLinkOfTx(ctx, tx, codeHash, now, schema.SignInLinkPurposeSignIn, schema.SignInLinkPurposeDevice)
		if err != nil {
			return err
		}
		id, p := l.id, l.principal
		lifetime, err := signInLinkCredentialLifetimeTx(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		exp := now.Add(lifetime)
		label := fmt.Sprintf("sign-in link %d", id)
		if l.purpose == schema.SignInLinkPurposeDevice {
			// The member named the device, so the sign-in it makes is theirs
			// to tell apart from the others (design §6.2).
			label = l.label
			if label == "" {
				label = fmt.Sprintf("device link %d", id)
			}
		}
		cred, err := insertCredentialTx(ctx, tx, p.ID, label, token, now, &exp, lifetime)
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
// Closing a link that is already closed or used changes nothing and writes no
// audit row (changed is false); an expired one is closed like an open one. An
// unknown id is ErrNotFound, as is a link whose principal is an owner when
// forbidOwner is true (issue #790, ADR-010 §4.1). sign_in_link_closed
// (reason=operator) is audited in the same transaction.
func (s *Store) CloseSignInLink(ctx context.Context, actor string, id int64, now time.Time, forbidOwner ...bool) (changed bool, err error) {
	hideOwner := len(forbidOwner) > 0 && forbidOwner[0]
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		var principalID int64
		var role string
		var targetOwner bool
		switch err := tx.QueryRowContext(ctx,
			`SELECT l.principal_id, p.role, p.owner
			 FROM sign_in_links l JOIN principals p ON p.id = l.principal_id WHERE l.id = ?`, id,
		).Scan(&principalID, &role, &targetOwner); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: close sign-in link %d: %w", id, ErrNotFound)
		case err != nil:
			return fmt.Errorf("store: close sign-in link: find: %w", err)
		}
		if hideOwner && Role(role) == RoleOperator && targetOwner {
			return fmt.Errorf("store: close sign-in link %d: %w", id, ErrNotFound)
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
	return closeOpenSignInLinksTx(ctx, tx, actor, principalID, true, SignInLinkClosePrincipalDisabled, now)
}

// closeOpenSignInLinksTx closes every link that could still be used and is
// for principalID and, when madeBy is set, every one they made, writing one
// sign_in_link_closed row each: with reason for a link for them, and
// creator_disabled for one they made for somebody else. The caller holds the
// write lock.
func closeOpenSignInLinksTx(ctx context.Context, tx execer, actor string, principalID int64, madeBy bool, reason string, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, principal_id FROM sign_in_links
		 WHERE (principal_id = ? OR (? AND created_by = ?)) AND used_at IS NULL AND closed_at IS NULL AND expires_at > ?
		 ORDER BY id`, principalID, madeBy, principalID, now.UnixMilli())
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
		why := reason
		if o.principalID != principalID {
			why = SignInLinkCloseCreatorDisabled
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditSignInLinkClosed, principalActor(o.principalID),
			fmt.Sprintf("sign_in_link=%d reason=%s", o.id, why), now); err != nil {
			return err
		}
	}
	return nil
}

// openDeviceLinkSQL is the condition, over sign_in_links l, that l is a
// device link that can still be used: unused, unclosed, unexpired at the
// first parameter, and of its person's present authentication generation.
// A link of an older generation is dead even before anything closes it (a
// restore moves every generation on at once), so it neither counts towards
// the cap nor is listed.
const openDeviceLinkSQL = `l.purpose = 'device' AND l.used_at IS NULL AND l.closed_at IS NULL AND l.expires_at > ?
	AND l.auth_generation = (SELECT auth_generation FROM principals WHERE id = l.principal_id)`

// CreateDeviceLink makes a device link for principalID, asked for on
// credentialID, and returns it with its code, which is shown only here. In
// one IMMEDIATE transaction with the insert it checks that the principal is
// still a member who may be given a link (else ErrDeviceLinkNotAllowed),
// that credentialID is theirs and holds a recent-password proof that still
// stands (else ErrReauthRequired), and that they hold fewer than
// schema.MaxOpenDeviceLinks open device links (else ErrTooManyDeviceLinks):
// two concurrent asks serialize on the write lock, so the cap holds. The row
// records the member as both its maker and its person, the label asked
// for, and the member's authentication generation. device_link_issued is
// audited in the same transaction, attributed to actor; the code and the
// label are not.
func (s *Store) CreateDeviceLink(ctx context.Context, actor string, principalID, credentialID int64, req schema.CreateDeviceLinkRequestV1, now time.Time) (schema.CreateSignInLinkResponseV1, error) {
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
		switch _, err := signInLinkTargetTx(ctx, tx, principalID); {
		case errors.Is(err, ErrPrincipalNotFound), errors.Is(err, ErrSignInLinkTarget),
			errors.Is(err, ErrPrincipalHidden), errors.Is(err, ErrPrincipalDisabled):
			return ErrDeviceLinkNotAllowed
		case err != nil:
			return err
		}
		owner, stands, err := recentAuthTx(ctx, tx, credentialID, now)
		if err != nil {
			return err
		}
		if owner != principalID || !stands {
			return ErrReauthRequired
		}
		var open int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sign_in_links l WHERE l.principal_id = ? AND `+openDeviceLinkSQL,
			principalID, now.UnixMilli()).Scan(&open); err != nil {
			return fmt.Errorf("store: create device link: count: %w", err)
		}
		if open >= schema.MaxOpenDeviceLinks {
			return ErrTooManyDeviceLinks
		}
		var label any
		if req.Label != "" {
			label = req.Label
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO sign_in_links (code_hash, principal_id, created_by, created_at, expires_at, purpose, label, auth_generation)
			 SELECT ?, ?, ?, ?, ?, 'device', ?, auth_generation FROM principals WHERE id = ?`,
			HashSignInLinkCode(code), principalID, principalID, now.UnixMilli(), expires.UnixMilli(), label, principalID)
		if err != nil {
			return fmt.Errorf("store: create device link: insert: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create device link: id: %w", err)
		}
		out = schema.SignInLinkV1{
			ID:          id,
			PrincipalID: principalID,
			CreatedBy:   principalID,
			CreatedAt:   schema.NewTimestamp(now),
			ExpiresAt:   schema.NewTimestamp(expires),
			Purpose:     schema.SignInLinkPurposeDevice,
			Label:       req.Label,
		}
		return appendAuditEventTx(ctx, tx, actor, AuditDeviceLinkIssued, principalActor(principalID),
			fmt.Sprintf("sign_in_link=%d credential=%d expires=%s", id, credentialID, out.ExpiresAt.Time().Format(time.RFC3339)), now)
	})
	if err != nil {
		return schema.CreateSignInLinkResponseV1{}, err
	}
	return schema.CreateSignInLinkResponseV1{SignInLink: out, Code: code}, nil
}

// ListOwnDeviceLinks returns principalID's open device links at now, newest
// first (ties by id descending): the ones that could still be used. It never
// returns a code or a hash. The result is never nil.
func (s *Store) ListOwnDeviceLinks(ctx context.Context, principalID int64, now time.Time) ([]schema.SignInLinkV1, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT l.id, l.created_at, l.expires_at, l.label FROM sign_in_links l
		 WHERE l.principal_id = ? AND l.created_by = l.principal_id AND `+openDeviceLinkSQL+`
		 ORDER BY l.created_at DESC, l.id DESC`, principalID, now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("store: list device links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.SignInLinkV1{}
	for rows.Next() {
		var (
			id, created, expires int64
			label                sql.NullString
		)
		if err := rows.Scan(&id, &created, &expires, &label); err != nil {
			return nil, fmt.Errorf("store: list device links: scan: %w", err)
		}
		out = append(out, schema.SignInLinkV1{
			ID:          id,
			PrincipalID: principalID,
			CreatedBy:   principalID,
			CreatedAt:   schema.NewTimestamp(time.UnixMilli(created)),
			ExpiresAt:   schema.NewTimestamp(time.UnixMilli(expires)),
			Purpose:     schema.SignInLinkPurposeDevice,
			Label:       label.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list device links: %w", err)
	}
	return out, nil
}

// CloseOwnDeviceLink closes device link id for principalID, the member who
// made it, so it can no longer be used. A link that is not one of their
// device links (another's, a link of another purpose, an id never issued)
// is ErrNotFound and nothing is written, so the answer says nothing of
// whether it exists. One already closed, used or expired changes nothing
// and writes no audit row (changed is false). sign_in_link_closed
// (reason=self) is audited in the same transaction.
func (s *Store) CloseOwnDeviceLink(ctx context.Context, actor string, principalID, id int64, now time.Time) (changed bool, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		var one int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM sign_in_links WHERE id = ? AND principal_id = ? AND created_by = ? AND purpose = 'device'`,
			id, principalID, principalID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: close device link %d: %w", id, ErrNotFound)
		case err != nil:
			return fmt.Errorf("store: close device link: find: %w", err)
		}
		// The guard, not the read, decides, as CloseSignInLink's does. An
		// expired link is left as it is: it can no longer be used anyway.
		res, err := tx.ExecContext(ctx,
			"UPDATE sign_in_links SET closed_at = ? WHERE id = ? AND used_at IS NULL AND closed_at IS NULL AND expires_at > ?",
			now.UnixMilli(), id, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: close device link: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: close device link: %w", err)
		} else if n != 1 {
			return nil
		}
		changed = true
		return appendAuditEventTx(ctx, tx, actor, AuditSignInLinkClosed, principalActor(principalID),
			fmt.Sprintf("sign_in_link=%d reason=%s", id, SignInLinkCloseSelf), now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// closeDeviceLinksTx closes every device link of principalID's that could
// still be used, writing one sign_in_link_closed row each with reason. It
// is what a member's single sign-out, and any revocation of one of their
// credentials, does to the device links they made (design §6.2, §7.2), so
// ending a session leaves no way back in that was opened before it. The
// caller holds the write lock.
func closeDeviceLinksTx(ctx context.Context, tx execer, actor string, principalID int64, reason string, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM sign_in_links
		 WHERE principal_id = ? AND purpose = 'device' AND used_at IS NULL AND closed_at IS NULL AND expires_at > ?
		 ORDER BY id`, principalID, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: list device links: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: list device links: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: list device links: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: list device links: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"UPDATE sign_in_links SET closed_at = ? WHERE id = ? AND closed_at IS NULL", now.UnixMilli(), id); err != nil {
			return fmt.Errorf("store: close device link %d: %w", id, err)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditSignInLinkClosed, principalActor(principalID),
			fmt.Sprintf("sign_in_link=%d reason=%s", id, reason), now); err != nil {
			return err
		}
	}
	return nil
}

// Conversion permits (issue #711, design §3.3, §7.2). A permit is a sign-in
// link of purpose conversion that an operator makes for one live guest, so
// that it may create an account although the policy of its visit's origin is
// closed. It is held to every rule of a link (one use, hashed at rest, ten
// minutes by default and an hour at most, made under its guest's
// authentication generation, dead once its maker is no longer an enabled
// operator) and to one more: it is for a guest whose visit is live, which no
// other link may be for. It grants nothing by itself: the guest still
// chooses a permanent name and a password, and no permit revives an ended
// visit or adds a channel or a right. No route but conversion takes one.

// CreateConversionPermit makes a conversion permit for guestID, made by
// creatorID, and returns it with its code, which is shown only here. In one
// IMMEDIATE transaction with the insert it checks that the maker is an
// enabled human operator (ErrSignInLinkNotAllowed) and that guestID is a
// guest (ErrNotAGuest) whose visit is live: active, before its deadline and
// enabled (ErrGuestEnded otherwise, a converted visit included).
// conversion_permit_issued is audited in the same transaction, attributed
// to actor, with the guest as its subject. The code is not. now is the link
// clock, as for CreateSignInLink.
func (s *Store) CreateConversionPermit(ctx context.Context, actor string, creatorID, guestID int64, req schema.CreateConversionPermitRequestV1, now time.Time) (schema.CreateSignInLinkResponseV1, error) {
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
		switch ok, err := makerIsOperatorTx(ctx, tx, creatorID); {
		case errors.Is(err, ErrPrincipalNotFound):
			return ErrSignInLinkNotAllowed
		case err != nil:
			return err
		case !ok:
			return ErrSignInLinkNotAllowed
		}
		if err := liveGuestTx(ctx, tx, guestID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO sign_in_links (code_hash, principal_id, created_by, created_at, expires_at, purpose, auth_generation)
			 SELECT ?, ?, ?, ?, ?, 'conversion', auth_generation FROM principals WHERE id = ?`,
			HashSignInLinkCode(code), guestID, creatorID, now.UnixMilli(), expires.UnixMilli(), guestID)
		if err != nil {
			return fmt.Errorf("store: create conversion permit: insert: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create conversion permit: id: %w", err)
		}
		out = schema.SignInLinkV1{
			ID:          id,
			PrincipalID: guestID,
			CreatedBy:   creatorID,
			CreatedAt:   schema.NewTimestamp(now),
			ExpiresAt:   schema.NewTimestamp(expires),
			Purpose:     schema.SignInLinkPurposeConversion,
		}
		return appendAuditEventTx(ctx, tx, actor, AuditConversionPermitIssued, principalActor(guestID),
			fmt.Sprintf("sign_in_link=%d expires=%s", id, out.ExpiresAt.Time().Format(time.RFC3339)), now)
	})
	if err != nil {
		return schema.CreateSignInLinkResponseV1{}, err
	}
	return schema.CreateSignInLinkResponseV1{SignInLink: out, Code: code}, nil
}

// usableConversionPermitTx returns the id of the conversion permit whose
// code hashes to codeHash if it still works for guestID: its row is live
// (liveSignInLinkTx), it is a conversion permit, it is guestID's, and its
// maker is still an enabled human operator. Whether guestID's visit is live
// is the caller's to check, in the same transaction. Every other case is
// ErrConversionPermitInvalid, one answer for all of them, so a permit that
// does not work says nothing of why: another purpose's code, another
// guest's permit and a code never issued are alike.
func usableConversionPermitTx(ctx context.Context, tx execer, codeHash string, guestID int64, now time.Time) (int64, error) {
	l, err := liveSignInLinkTx(ctx, tx, codeHash, now)
	switch {
	case errors.Is(err, ErrSignInLinkInvalid):
		return 0, ErrConversionPermitInvalid
	case err != nil:
		return 0, err
	}
	if l.purpose != schema.SignInLinkPurposeConversion || l.principalID != guestID {
		return 0, ErrConversionPermitInvalid
	}
	switch ok, err := makerIsOperatorTx(ctx, tx, l.createdBy); {
	case err != nil:
		return 0, err
	case !ok:
		return 0, ErrConversionPermitInvalid
	}
	return l.id, nil
}
