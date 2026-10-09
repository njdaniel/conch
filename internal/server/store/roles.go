package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// ErrOperatorExists is returned by BootstrapOperator when the database
// already has an operator; nothing is written.
var ErrOperatorExists = errors.New("store: an operator already exists")

// BootstrapCredentialLabel labels the credential BootstrapOperator issues.
const BootstrapCredentialLabel = "bootstrap"

// BootstrapOptions tunes BootstrapOperatorWith.
type BootstrapOptions struct {
	// KeepExistingCredentials skips revoking the credentials that exist
	// before the operator does. By default they are all revoked.
	KeepExistingCredentials bool
}

// BootstrapResult is what a successful bootstrap created.
type BootstrapResult struct {
	Principal  Principal
	Credential schema.CredentialV1
	// Token is the operator's plaintext token, shown once; only its hash is stored.
	Token string
	// Revoked is the number of pre-existing live credentials revoked.
	Revoked int
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
// returns ErrOperatorExists, and if name is taken (by a member) it returns
// ErrDuplicate; in both cases nothing is written, including no revocations.
//
// Unless opts.KeepExistingCredentials is set, every live credential (not
// revoked, not expired) is revoked first, because until an operator exists the
// credential endpoints were open and any earlier credential could have been
// minted by anyone who reached the port. Each gets a credential_revoked audit
// event (actor system, detail noting the bootstrap); an already-revoked
// credential keeps its original revoked_at. The operator's own credential is
// created after, so it is never revoked. On success an operator_bootstrapped
// event and the usual credential_created event are appended in the same
// transaction; none carries a token.
func (s *Store) BootstrapOperatorWith(ctx context.Context, name string, opts BootstrapOptions) (BootstrapResult, error) {
	token, err := newCredentialToken()
	if err != nil {
		return BootstrapResult{}, err
	}
	now := credentialNow().Truncate(time.Millisecond)
	var res BootstrapResult
	err = s.withImmediateTx(ctx, func(tx execer) error {
		var one int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM principals WHERE role = 'operator' LIMIT 1").Scan(&one); {
		case err == nil:
			return ErrOperatorExists
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: bootstrap operator: check operators: %w", err)
		}
		ins, err := tx.ExecContext(ctx,
			"INSERT INTO principals (kind, name, role, created_at) VALUES ('human', ?, 'operator', ?)",
			name, now.UnixMilli())
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
		res.Principal = Principal{ID: id, Kind: PrincipalHuman, Name: name, Role: RoleOperator, CreatedAt: now}
		if !opts.KeepExistingCredentials {
			if res.Revoked, err = revokeLiveCredentialsTx(ctx, tx, now); err != nil {
				return err
			}
		}
		if res.Credential, err = insertCredentialTx(ctx, tx, id, BootstrapCredentialLabel, token, now, nil); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, "system", "operator_bootstrapped", principalActor(id), "", now); err != nil {
			return err
		}
		return appendAuditEventTx(ctx, tx, "system", "credential_created",
			principalActor(id), credentialDetail(res.Credential.ID, res.Credential.Label), now)
	})
	if err != nil {
		return BootstrapResult{}, err
	}
	res.Token = token
	return res, nil
}

// revokeLiveCredentialsTx revokes every credential that is neither revoked nor
// expired at now, writing one credential_revoked event each, and returns how
// many it revoked.
func revokeLiveCredentialsTx(ctx context.Context, tx execer, now time.Time) (int, error) {
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
	}
	return len(found), nil
}
