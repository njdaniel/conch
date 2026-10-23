package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// ownerMarkMigration is the number of the migration that adds principals.owner
// (issue #788, ADR-010, docs/design/delegated-administration.md §3.1).
const ownerMarkMigration = 34

// AuditPrincipalRoleChanged records a role or owner-mark change (issue #788,
// docs/design/delegated-administration.md §6).
const AuditPrincipalRoleChanged = "principal_role_changed"

// ErrOperatorExists is returned by BootstrapOperator when the database
// already has an operator; nothing is written.
var ErrOperatorExists = errors.New("store: an operator already exists")

var (
	// ErrNotOwner is returned by SetRole when the acting principal is not an
	// enabled human operator holding the owner mark under the write lock.
	ErrNotOwner = errors.New("store: owner required")
	// ErrRoleNotAllowed is returned by SetRole when the target cannot hold a
	// role (an agent or a guest) or the requested role and owner mark are not
	// a valid combination.
	ErrRoleNotAllowed = errors.New("store: role change not allowed")
)

// BootstrapCredentialLabel labels the credential BootstrapOperator issues.
const BootstrapCredentialLabel = "bootstrap"

// BootstrapOptions tunes BootstrapOperatorWith.
type BootstrapOptions struct {
	// KeepExistingCredentials skips revoking the credentials and deleting the
	// webhook hooks that exist before the operator does. By default every
	// live credential is revoked and every hook is deleted.
	KeepExistingCredentials bool
	// Setup makes the operator a setup token (issue #709) in place of a
	// credential: the operator then sets a password and enrols TOTP through
	// the restricted setup flow, and signs in with both. No script
	// credential exists until the operator makes one.
	Setup bool
}

// BootstrapResult is what a successful bootstrap created.
type BootstrapResult struct {
	Principal  Principal
	Credential schema.CredentialV1
	// Token is the operator's plaintext token, shown once; only its hash is
	// stored. Empty with BootstrapOptions.Setup, which makes SetupToken
	// instead.
	Token      string
	SetupToken SetupToken
	// Revoked is the number of pre-existing live credentials revoked.
	Revoked int
	// RevokedHooks is the number of pre-existing webhook hooks deleted.
	RevokedHooks int
	// Manifests is the number of agent manifests that already existed. They
	// are kept, but were writable by anyone before the operator existed, so
	// the caller should tell the operator to review them.
	Manifests int
}

// BootstrapOperator is BootstrapOperatorWith with default options: existing
// credentials are revoked.
func (s *Store) BootstrapOperator(ctx context.Context, name string) (Principal, schema.CredentialV1, string, error) {
	r, err := s.BootstrapOperatorWith(ctx, name, BootstrapOptions{})
	return r.Principal, r.Credential, r.Token, err
}

// BootstrapOperatorWith creates the instance's first operator: a human
// principal with role operator plus one credential labelled "bootstrap". It is
// the only code path that creates an operator.
//
// Everything happens in one write transaction. If any operator exists it
// returns ErrOperatorExists, and if name is taken (by a member), clashes
// with a member's by case or look-alike characters, or is reserved for
// hidden principals, it returns ErrDuplicate;
// in both cases nothing is written, including no revocations.
//
// Unless opts.KeepExistingCredentials is set, every live credential (not
// revoked, not expired) is revoked first, because until an operator exists the
// credential endpoints were open and any earlier credential could have been
// minted by anyone who reached the port. Each gets a credential_revoked audit
// event (actor system, detail noting the bootstrap); an already-revoked
// credential keeps its original revoked_at. Webhook hooks are handled the
// same way and for the same reason: a hook token is an unauthenticated posting
// credential bound to a principal of its creator's choosing, so every existing
// hook is deleted, with one hook_revoked audit event each (never containing
// the token). Agent manifests are only counted. The operator's own credential
// is created after, so it is never revoked. On success principal_created
// (issue #445), operator_bootstrapped and the usual credential_created event
// are appended in the same transaction; none carries a token. With
// opts.Setup the operator gets a setup token instead of the credential
// (setup_token_issued in place of credential_created).
func (s *Store) BootstrapOperatorWith(ctx context.Context, name string, opts BootstrapOptions) (BootstrapResult, error) {
	token, err := newCredentialToken()
	if err != nil {
		return BootstrapResult{}, err
	}
	now := credentialNow().Truncate(time.Millisecond)
	var res BootstrapResult
	var runs []EndedRun
	err = s.withImmediateTx(ctx, func(tx execer) error {
		runs = nil
		var one int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM principals WHERE role = 'operator' LIMIT 1").Scan(&one); {
		case err == nil:
			return ErrOperatorExists
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: bootstrap operator: check operators: %w", err)
		}
		keys, err := s.claimNameTx(ctx, tx, name, 0)
		if err != nil {
			return fmt.Errorf("store: bootstrap operator: %w", err)
		}
		ins, err := tx.ExecContext(ctx,
			"INSERT INTO principals (kind, name, name_key, name_skel, role, owner, created_at) VALUES ('human', ?, ?, ?, 'operator', 1, ?)",
			name, keys.Folded, keys.Skeletal, now.UnixMilli())
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("store: bootstrap operator %q: %w", name, ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: bootstrap operator %q: %w", name, err)
		}
		id, err := ins.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: bootstrap operator %q: %w", name, err)
		}
		res.Principal = Principal{ID: id, Kind: PrincipalHuman, Name: name, Role: RoleOperator, Owner: true, CreatedAt: now, VoiceAllowed: true}
		if !opts.KeepExistingCredentials {
			if res.Revoked, err = revokeLiveCredentialsTx(ctx, tx, now, &runs); err != nil {
				return err
			}
			if res.RevokedHooks, err = deleteHooksTx(ctx, tx, now); err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_manifests").Scan(&res.Manifests); err != nil {
			return fmt.Errorf("store: bootstrap: count manifests: %w", err)
		}
		if !opts.Setup {
			if res.Credential, err = insertCredentialTx(ctx, tx, id, BootstrapCredentialLabel, token, now, nil, 0); err != nil {
				return err
			}
		}
		if err := appendAuditEventTx(ctx, tx, "system", AuditPrincipalCreated, principalActor(id), "kind=human", now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, "system", "operator_bootstrapped", principalActor(id), "", now); err != nil {
			return err
		}
		if opts.Setup {
			res.SetupToken, err = issueSetupTokenTx(ctx, tx, id, "bootstrap", now)
			return err
		}
		return appendAuditEventTx(ctx, tx, "system", "credential_created",
			principalActor(id), credentialDetail(res.Credential.ID, res.Credential.Label), now)
	})
	if err != nil {
		return BootstrapResult{}, err
	}
	s.announceRunsEnded(runs)
	if !opts.Setup {
		res.Token = token
	}
	return res, nil
}

// revokeLiveCredentialsTx revokes every credential that is neither revoked nor
// expired at now, writing one credential_revoked event each, and returns how
// many it revoked.
func revokeLiveCredentialsTx(ctx context.Context, tx execer, now time.Time, ended *[]EndedRun) (int, error) {
	type live struct {
		id, principalID int64
		label           string
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, principal_id, label FROM credentials
		 WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY id`, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: bootstrap: list live credentials: %w", err)
	}
	var found []live
	for rows.Next() {
		var c live
		if err := rows.Scan(&c.id, &c.principalID, &c.label); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: bootstrap: scan credential: %w", err)
		}
		found = append(found, c)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: bootstrap: list live credentials: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: bootstrap: list live credentials: %w", err)
	}
	for _, c := range found {
		res, err := tx.ExecContext(ctx,
			"UPDATE credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), c.id)
		if err != nil {
			return 0, fmt.Errorf("store: bootstrap: revoke credential %d: %w", c.id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return 0, fmt.Errorf("store: bootstrap: revoke credential %d: %w", c.id, err)
		} else if n != 1 {
			return 0, fmt.Errorf("store: bootstrap: revoke credential %d: affected %d rows", c.id, n)
		}
		if err := appendAuditEventTx(ctx, tx, "system", "credential_revoked", principalActor(c.principalID),
			credentialDetail(c.id, c.label)+" reason=revoked at bootstrap", now); err != nil {
			return 0, err
		}
		// Its sessions end with it, as on every other revocation (issue #265).
		if err := endCredentialSessionsTx(ctx, tx, "system", c.id, SessionEndedByRevocation, now, ended); err != nil {
			return 0, err
		}
	}
	return len(found), nil
}

// deleteHooksTx deletes every webhook hook, writing one hook_revoked event
// each, and returns how many it deleted. The hook token is never read.
func deleteHooksTx(ctx context.Context, tx execer, now time.Time) (int, error) {
	type hook struct{ channelID, principalID int64 }
	rows, err := tx.QueryContext(ctx, "SELECT channel_id, principal_id FROM hooks ORDER BY created_at, rowid")
	if err != nil {
		return 0, fmt.Errorf("store: bootstrap: list hooks: %w", err)
	}
	var found []hook
	for rows.Next() {
		var h hook
		if err := rows.Scan(&h.channelID, &h.principalID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: bootstrap: scan hook: %w", err)
		}
		found = append(found, h)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: bootstrap: list hooks: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: bootstrap: list hooks: %w", err)
	}
	if len(found) == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM hooks"); err != nil {
		return 0, fmt.Errorf("store: bootstrap: delete hooks: %w", err)
	}
	for _, h := range found {
		if err := appendAuditEventTx(ctx, tx, "system", "hook_revoked", principalActor(h.principalID),
			fmt.Sprintf("channel=%d reason=revoked at bootstrap", h.channelID), now); err != nil {
			return 0, err
		}
	}
	return len(found), nil
}

// reportPromotedOwners is migration 34's Go step: every existing operator was
// promoted to owner by the migration so an upgraded installation is never left
// without an owner, and this step logs each promoted operator's live-credential
// and unrevoked bound-hook counts for review, writing no audit row.
func reportPromotedOwners(ctx context.Context, tx *sql.Tx) error {
	now := credentialNow().UnixMilli()
	rows, err := tx.QueryContext(ctx,
		`SELECT p.id,
		        (SELECT COUNT(*) FROM credentials c
		         WHERE c.principal_id = p.id AND p.disabled_at IS NULL AND c.revoked_at IS NULL
		           AND (c.expires_at IS NULL OR c.expires_at > ?)
		           AND (c.idle_ms IS NULL OR COALESCE(c.last_used_at, c.created_at) + c.idle_ms > ?)
		           AND `+currentAuthSQL+`),
		        (SELECT COUNT(*) FROM hooks h WHERE h.principal_id = p.id AND h.revoked_at IS NULL)
		 FROM principals p
		 WHERE p.role = 'operator'
		 ORDER BY p.id`, now, now)
	if err != nil {
		return fmt.Errorf("read promoted owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type promoted struct {
		id              int64
		liveCredentials int
		hooksBound      int
	}
	var found []promoted
	for rows.Next() {
		var p promoted
		if err := rows.Scan(&p.id, &p.liveCredentials, &p.hooksBound); err != nil {
			return fmt.Errorf("scan promoted owner: %w", err)
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read promoted owners: %w", err)
	}
	for _, p := range found {
		slog.WarnContext(ctx, "store: existing operator promoted to owner by migration; review its live credentials and bound hooks",
			"principal_id", p.id, "live_credentials", p.liveCredentials, "hooks_bound", p.hooksBound)
	}
	return nil
}

// RoleChangeResult is what SetRole did, and the wire response it builds.
type RoleChangeResult struct {
	schema.RoleChangeResponseV1
	// Changed is false when the target already held the requested role and
	// owner mark: nothing was written and no audit row was appended.
	Changed bool
}

// SetRole sets targetID's role and owner mark in one IMMEDIATE transaction
// (issue #788, docs/design/delegated-administration.md §6):
//   - actorID is re-read under the write lock and must be an enabled, non-hidden
//     human operator holding the owner mark (else ErrNotOwner).
//   - targetID must exist and not be hidden (else ErrPrincipalNotFound), and
//     must be a non-guest human (else ErrRoleNotAllowed).
//   - role must be RoleOperator or RoleMember, and owner may not be true with
//     RoleMember (else ErrRoleNotAllowed). When owner is nil, an existing
//     operator keeps its mark and a promoted member gets owner = false.
//   - When nothing changes, nothing is written or audited and Changed is false.
//   - Demoting, or clearing the mark of, an enabled owner when no other owner
//     could sign in is refused with ErrLastOperator.
//   - Setting the owner mark revokes every live credential and every bound hook
//     of targetID, and closes its open sign-in, reset, enrollment and device
//     links.
//   - Demoting an operator to member closes the open invite links and sign-in
//     links they made, and their personal invites unless targetID holds
//     may_invite.
//   - Every effective change appends one principal_role_changed audit row.
func (s *Store) SetRole(ctx context.Context, actor string, actorID, targetID int64, role Role, owner *bool) (RoleChangeResult, error) {
	now := credentialNow().Truncate(time.Millisecond)
	var (
		out  RoleChangeResult
		runs []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		runs = nil
		var err error
		out, err = setRoleTx(ctx, tx, actor, actorID, targetID, role, owner, now, &runs)
		return err
	})
	if err != nil {
		return RoleChangeResult{}, err
	}
	s.announceRunsEnded(runs)
	return out, nil
}

func setRoleTx(ctx context.Context, tx execer, actor string, actorID, targetID int64, role Role, owner *bool, now time.Time, runs *[]EndedRun) (RoleChangeResult, error) {
	actorP, found, err := queryPrincipal(ctx, tx, actorID)
	if err != nil {
		return RoleChangeResult{}, err
	}
	if !found || actorP.DisabledAt != nil || actorP.HiddenAt != nil || actorP.Kind != PrincipalHuman || !actorP.IsOwner() || actorP.guestTarget() {
		return RoleChangeResult{}, ErrNotOwner
	}

	target, found, err := queryPrincipal(ctx, tx, targetID)
	if err != nil {
		return RoleChangeResult{}, err
	}
	if !found || target.HiddenAt != nil {
		return RoleChangeResult{}, ErrPrincipalNotFound
	}
	if target.Kind != PrincipalHuman {
		return RoleChangeResult{}, fmt.Errorf("%w: an agent has no role", ErrRoleNotAllowed)
	}
	if target.guestTarget() {
		return RoleChangeResult{}, fmt.Errorf("%w: a guest holds no role", ErrRoleNotAllowed)
	}
	if role != RoleOperator && role != RoleMember {
		return RoleChangeResult{}, fmt.Errorf("%w: unknown role %q", ErrRoleNotAllowed, role)
	}
	if role == RoleMember && owner != nil && *owner {
		return RoleChangeResult{}, fmt.Errorf("%w: a member cannot hold the owner mark", ErrRoleNotAllowed)
	}

	wantOwner := false
	if role == RoleOperator {
		if owner != nil {
			wantOwner = *owner
		} else {
			wantOwner = target.Owner
		}
	}

	if target.Role == role && target.Owner == wantOwner {
		return populateRoleChangeResultTx(ctx, tx, targetID, 0, 0, 0, false, now)
	}

	if target.IsOwner() && !wantOwner && target.DisabledAt == nil {
		others, err := liveOperatorCredentialsTx(ctx, tx, targetID, 0, targetID, now)
		if err != nil {
			return RoleChangeResult{}, err
		}
		if others == 0 {
			return RoleChangeResult{}, ErrLastOperator
		}
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE principals SET role = ?, owner = ? WHERE id = ?",
		string(role), boolInt(wantOwner), targetID); err != nil {
		return RoleChangeResult{}, fmt.Errorf("store: set role: %w", err)
	}

	var invitesClosed, credsRevoked, hooksRevoked int
	if !target.Owner && wantOwner {
		if credsRevoked, err = revokePrincipalCredentialsTx(ctx, tx, actor, targetID, "owner mark set", SessionEndedByRevocation, now, runs); err != nil {
			return RoleChangeResult{}, err
		}
		if hooksRevoked, err = revokePrincipalHooksTx(ctx, tx, actor, targetID, "owner mark set", now); err != nil {
			return RoleChangeResult{}, err
		}
		if err := closeOpenSignInLinksTx(ctx, tx, actor, targetID, false, SignInLinkCloseOperator, now); err != nil {
			return RoleChangeResult{}, err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE pending_logins SET spent_at = ? WHERE principal_id = ? AND spent_at IS NULL",
			now.UnixMilli(), targetID); err != nil {
			return RoleChangeResult{}, fmt.Errorf("store: set role: spend pending logins: %w", err)
		}
	}

	if target.Role == RoleOperator && role == RoleMember {
		var kind schema.InviteKind
		if target.MayInvite {
			kind = schema.InviteKindOpen
		}
		if invitesClosed, err = countOpenCreatorInvitesTx(ctx, tx, targetID, kind, now); err != nil {
			return RoleChangeResult{}, err
		}
		if err := closeCreatorInvitesTx(ctx, tx, actor, targetID, kind, InviteClosePermissionRevoked, now); err != nil {
			return RoleChangeResult{}, err
		}
		if err := closeOpenSignInLinksTx(ctx, tx, actor, targetID, true, SignInLinkCloseOperator, now); err != nil {
			return RoleChangeResult{}, err
		}
	}

	out, err := populateRoleChangeResultTx(ctx, tx, targetID, invitesClosed, credsRevoked, hooksRevoked, true, now)
	if err != nil {
		return RoleChangeResult{}, err
	}
	detail := fmt.Sprintf("from=%s to=%s owner=%t invites_closed=%d credentials_revoked=%d hooks_revoked=%d",
		target.Role, role, wantOwner, invitesClosed, credsRevoked, hooksRevoked)
	if err := appendAuditEventTx(ctx, tx, actor, AuditPrincipalRoleChanged, principalActor(targetID), detail, now); err != nil {
		return RoleChangeResult{}, err
	}
	return out, nil
}

func populateRoleChangeResultTx(ctx context.Context, tx execer, targetID int64, invitesClosed, credsRevoked, hooksRevoked int, changed bool, now time.Time) (RoleChangeResult, error) {
	adm, err := principalAdminTx(ctx, tx, targetID, now)
	if err != nil {
		return RoleChangeResult{}, err
	}
	var hooksBound int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM hooks WHERE principal_id = ? AND revoked_at IS NULL", targetID,
	).Scan(&hooksBound); err != nil {
		return RoleChangeResult{}, fmt.Errorf("store: count bound hooks: %w", err)
	}
	return RoleChangeResult{
		RoleChangeResponseV1: schema.RoleChangeResponseV1{
			Principal:          adm,
			Manages:            []int64{},
			InvitesClosed:      invitesClosed,
			CredentialsRevoked: credsRevoked,
			HooksRevoked:       hooksRevoked,
			LiveCredentials:    adm.LiveCredentials,
			HooksBound:         hooksBound,
		},
		Changed: changed,
	}, nil
}

func countOpenCreatorInvitesTx(ctx context.Context, tx execer, creatorID int64, kind schema.InviteKind, now time.Time) (int, error) {
	q := `SELECT COUNT(*) FROM invites
		 WHERE created_by = ? AND closed_at IS NULL AND expires_at > ? AND (max_uses IS NULL OR uses < max_uses)`
	args := []any{creatorID, now.UnixMilli()}
	if kind != "" {
		q += " AND kind = ?"
		args = append(args, string(kind))
	}
	var n int
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count open invites: %w", err)
	}
	return n, nil
}
