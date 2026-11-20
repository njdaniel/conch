package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Credential provenance (issue #2042, child of #2013, Nick's decision 4 of
// 2026-11-19): every credential records whether its principal made it for
// themselves or another principal made it for them. The DM and call routes
// and the per-person stream refuse a credential made by another, so
// act_as_members (an operator's credential, sign-in link or reset for
// someone else) reaches no one's direct messages or calls. Every other route
// reads nothing of it.
//
// credentials.self_made is 1 for a credential its principal made, and 0 for
// one made by another or of unknown origin: the column's default, so a row
// written by a path that forgot to say is refused, never admitted. A
// credential is self-made only when it comes from one of these, each of
// which proves the person and nobody else is present:
//
//   - a password sign-in (IssueLoginCredential, insertOperatorSessionTx);
//   - the person's own entry: redeeming an invite, joining as an account
//     with a password, a guest's visit and its conversion to an account;
//   - a device link the person made for themselves on a self-made
//     credential (sign_in_links.self_made, copied from that credential);
//   - the bootstrap credential, which only the host's administrator, the
//     owner, can mint, for the owner, whom act_as_members never reaches;
//   - a rotation of the caller's own session (rotateSessionTx), which keeps
//     the provenance of the credential it replaces.
//
// Everything else is made by another: a credential an operator issues or
// rotates (POST /v1/principals/{id}/credentials and
// POST /v1/credentials/{credential_id}/rotate, both act_as_members, even
// for the operator themselves), a sign-in link of any purpose but a
// self-made device link, and a host-made setup credential (whose scope
// reaches no DM or call route anyway).

// credentialProvenanceMigration is the migration that adds self_made to
// credentials and sign_in_links (issue #2042). Every existing row gets 0,
// made by another: which path made it is not on record, and a wrong "self"
// would let an operator's credential read someone's messages, while a wrong
// "another" costs the person one sign-in.
const credentialProvenanceMigration = 52

// credentialMaker says who made a credential: its principal (madeBySelf) or
// another principal or nobody known (madeByAnother). It is what
// insertCredentialTx writes to credentials.self_made.
type credentialMaker bool

const (
	madeByAnother credentialMaker = false
	madeBySelf    credentialMaker = true
)

// CredentialSelfMade reports whether credential id was made by its own
// principal (credentials.self_made). It reads only provenance, which never
// changes after the row is written; whether the credential is live is
// CredentialLive's to say. An unknown id is ErrCredentialNotFound. It is how
// the server learns the provenance of a request that did not come through
// ResolveRequestCredential (a socket opened with a ticket).
func (s *Store) CredentialSelfMade(ctx context.Context, id int64) (bool, error) {
	var self bool
	switch err := s.db.QueryRowContext(ctx, "SELECT self_made FROM credentials WHERE id = ?", id).Scan(&self); {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrCredentialNotFound
	case err != nil:
		return false, fmt.Errorf("store: credential provenance: %w", err)
	}
	return self, nil
}

// credentialSelfMadeTx is CredentialSelfMade for credential id of principal
// principalID, inside a transaction: false for an id that is not that
// principal's, so a path copying provenance from the caller's credential
// never copies another's.
func credentialSelfMadeTx(ctx context.Context, tx execer, id, principalID int64) (credentialMaker, error) {
	var self bool
	switch err := tx.QueryRowContext(ctx,
		"SELECT self_made FROM credentials WHERE id = ? AND principal_id = ?", id, principalID).Scan(&self); {
	case errors.Is(err, sql.ErrNoRows):
		return madeByAnother, nil
	case err != nil:
		return madeByAnother, fmt.Errorf("store: credential provenance: %w", err)
	}
	return credentialMaker(self), nil
}
