package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Who issued each credential and hook (issue #1024, ADR-014 O13,
// docs/design/admin-roles.md §7.5, §12 A12). credentials.issued_by and
// hooks.issued_by name the principal that issued the row: the operator who
// created or rotated the credential or bound the hook, or the person
// themself for a sign-in. NULL where no principal did (a bootstrap, a link
// or invite redemption, whose actors are not principals) or where none is on
// record (rows from before e48b6e3, 2026-10-08, backfilled from the audit
// trail by migration 42). What a non-owner issued to someone other than
// itself stays live when its act_as_members goes, when it is demoted and
// when it is disabled (§7.5): each of those answers with the list
// (IssuedToOthers, IssuedToOthersBy), and the owner revokes it all in one
// step (RevokeIssued).

// issuedByMigration is the schema version that adds the two columns.
const issuedByMigration = 42

// AuditIssuedRevokedAll records an owner's revoke-all of what a principal
// issued to others: actor the owner, subject the issuer, detail the counts.
const AuditIssuedRevokedAll = "issued_revoked_all"

// issuedDetail reads the credential or hook a credential_created,
// credential_rotated or hook_created audit row names: its detail starts
// "credential=<id>" or "hook=<id>" (credentialDetail, hookDetail).
var issuedDetail = regexp.MustCompile(`^(credential|hook)=([0-9]+)\b`)

// issuerOf is the principal behind an audit actor ("principal:<id>"), or 0
// for any other actor (system, a link, an invite): what issued_by records.
func issuerOf(actor string) int64 {
	id, ok := principalOfActor(actor)
	if !ok {
		return 0
	}
	return id
}

// principalOfActor reads the id of an audit actor or subject of the form
// "principal:<id>", positive.
func principalOfActor(s string) (int64, bool) {
	rest, ok := strings.CutPrefix(s, "principal:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

// backfillIssuedBy is migration 42's Go step: it fills issued_by from the
// audit trail. For each credential or hook, the latest credential_created,
// credential_rotated or hook_created row naming its id whose actor is a
// principal decides (a credential rotated by someone other than its maker
// is a new row, named by the rotation; the latest row wins should two name
// one id). A row with no such audit row, as every one from before e48b6e3,
// stays NULL, and so does one whose actor names no principal on record.
func backfillIssuedBy(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT actor, detail FROM audit_events
		 WHERE action IN ('credential_created', 'credential_rotated', 'hook_created')
		   AND actor LIKE 'principal:%'
		 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("backfill issued_by: read audit rows: %w", err)
	}
	issuers := map[schema.IssuedKind]map[int64]int64{schema.IssuedCredential: {}, schema.IssuedHook: {}}
	for rows.Next() {
		var actor, detail string
		if err := rows.Scan(&actor, &detail); err != nil {
			_ = rows.Close()
			return fmt.Errorf("backfill issued_by: scan audit row: %w", err)
		}
		from, ok := principalOfActor(actor)
		m := issuedDetail.FindStringSubmatch(detail)
		if !ok || m == nil {
			continue
		}
		id, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		// Later rows overwrite earlier ones: the latest issuer wins.
		issuers[schema.IssuedKind(m[1])][id] = from
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("backfill issued_by: read audit rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("backfill issued_by: read audit rows: %w", err)
	}
	// Only a principal on record is written: principals are never deleted,
	// but the column references them and the trail is not trusted to agree.
	for kind, stmt := range map[schema.IssuedKind]string{
		schema.IssuedCredential: `UPDATE credentials SET issued_by = ? WHERE id = ? AND EXISTS (SELECT 1 FROM principals WHERE id = ?)`,
		schema.IssuedHook:       `UPDATE hooks SET issued_by = ? WHERE id = ? AND EXISTS (SELECT 1 FROM principals WHERE id = ?)`,
	} {
		for id, from := range issuers[kind] {
			if _, err := tx.ExecContext(ctx, stmt, from, id, from); err != nil {
				return fmt.Errorf("backfill issued_by: %s %d: %w", kind, id, err)
			}
		}
	}
	return nil
}

// IssuedToOthers lists the live credentials and hooks that someone other
// than an owner issued to a principal other than itself (ADR-014 design
// §7.5, O13): what the answer to a PUT that turns act_as_members off for
// operators lists, so that the owner can revoke them. Taking the permission
// away ends none of them.
//
// It reads issued_by (migration 42). An item whose issuer is not on record,
// as before e48b6e3, is absent. An owner's own issues are left out: owners
// keep act_as_members. An item is live while it is not revoked and, for a
// credential, not expired at now. Oldest first, a credential before a hook
// issued in the same millisecond, then by id.
func (s *Store) IssuedToOthers(ctx context.Context, now time.Time) ([]schema.IssuedByOperatorV1, error) {
	return s.issuedToOthers(ctx, 0, now)
}

// IssuedToOthersBy is IssuedToOthers for one issuer: what principal issuer
// issued to others that is still live. Empty for an owner, whose issues are
// never listed, and for an unknown id.
func (s *Store) IssuedToOthersBy(ctx context.Context, issuer int64, now time.Time) ([]schema.IssuedByOperatorV1, error) {
	if issuer <= 0 {
		return []schema.IssuedByOperatorV1{}, nil
	}
	return s.issuedToOthers(ctx, issuer, now)
}

func (s *Store) issuedToOthers(ctx context.Context, issuer int64, now time.Time) ([]schema.IssuedByOperatorV1, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("store: issued to others: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return issuedToOthersTx(ctx, tx, issuer, now)
}

// The owner tests of the issued queries. An owner is an operator with the
// mark (Principal.IsOwner), fixed, not a permission. Both the list
// (issuedToOthersTx) and the sweep (RevokeIssued) use holderNotOwnerSQL,
// so what is listed and what is revoked cannot diverge: a credential or
// hook held by an owner is never listed and never revoked by the sweep
// (security review of #1024, NB-1). Owners' credentials end deliberately,
// one by one or through their own revoke-all, never as a side effect of
// cleaning up after someone else: an ex-owner X may have issued one to
// owner O, and the sweep of X must not sign O out. The list alone also
// leaves out owner issuers (issuerNotOwnerSQL): owners keep act_as_members,
// and the sweep refuses an owner issuer before it reads anything.
const (
	issuerNotOwnerSQL = "NOT (i.role = 'operator' AND i.owner = 1)"
	holderNotOwnerSQL = "NOT (p.role = 'operator' AND p.owner = 1)"
)

// issuedToOthersTx is IssuedToOthers inside tx; issuer 0 is every issuer.
// The join on the issuer (i) leaves out rows whose issued_by is NULL; the
// join on the holder (p) is for holderNotOwnerSQL.
func issuedToOthersTx(ctx context.Context, tx execer, issuer int64, now time.Time) ([]schema.IssuedByOperatorV1, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT kind, id, principal_id, issued_by, created_at FROM (
		   SELECT 'credential' AS kind, c.id AS id, c.principal_id AS principal_id, c.issued_by AS issued_by, c.created_at AS created_at
		     FROM credentials c JOIN principals i ON i.id = c.issued_by JOIN principals p ON p.id = c.principal_id
		    WHERE c.issued_by <> c.principal_id AND c.revoked_at IS NULL
		      AND (c.expires_at IS NULL OR c.expires_at > ?)
		      AND `+issuerNotOwnerSQL+` AND `+holderNotOwnerSQL+`
		      AND (? = 0 OR c.issued_by = ?)
		   UNION ALL
		   SELECT 'hook', h.id, h.principal_id, h.issued_by, h.created_at
		     FROM hooks h JOIN principals i ON i.id = h.issued_by JOIN principals p ON p.id = h.principal_id
		    WHERE h.issued_by <> h.principal_id AND h.revoked_at IS NULL
		      AND `+issuerNotOwnerSQL+` AND `+holderNotOwnerSQL+`
		      AND (? = 0 OR h.issued_by = ?)
		 ) ORDER BY created_at, kind, id`,
		now.UnixMilli(), issuer, issuer, issuer, issuer)
	if err != nil {
		return nil, fmt.Errorf("store: issued to others: %w", err)
	}
	out := []schema.IssuedByOperatorV1{}
	for rows.Next() {
		var kind string
		var id, target, from, at int64
		if err := rows.Scan(&kind, &id, &target, &from, &at); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: issued to others: scan: %w", err)
		}
		out = append(out, schema.IssuedByOperatorV1{Kind: schema.IssuedKind(kind), ID: id, PrincipalID: target, IssuedBy: from,
			IssuedAt: schema.NewTimestamp(time.UnixMilli(at))})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store: issued to others: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: issued to others: %w", err)
	}
	return out, nil
}

// RevokedIssued is what RevokeIssued did: the counts the wire answer
// carries, and the revoked credentials' ids, for the server to close their
// sockets after the commit.
type RevokedIssued struct {
	CredentialsRevoked int
	HooksRevoked       int
	Credentials        []int64
}

// RevokeIssued revokes, in one IMMEDIATE transaction, every unrevoked
// credential and hook that principal issuerID issued to a principal other
// than itself (ADR-014 O13, design §12 A12): the owner's one confirmed
// step after a demotion, a disable or act_as_members going, revoking
// everything those answers listed. An expired but unrevoked credential is
// revoked too, as a sign-out everywhere does (revokePrincipalCredentialsTx).
// A credential or hook held by an owner is never revoked by it, as it is
// never listed (holderNotOwnerSQL): an ex-owner's issue to an owner stays.
//   - actorID is re-read under the write lock and must be an enabled,
//     non-hidden human owner (else ErrNotOwner): what an operator issued is
//     the owner's to end. actorID 0 is the system (a server under AuthOff),
//     as in RevokeAllCredentialsAs, and is not checked.
//   - issuerID must exist (else ErrPrincipalNotFound); a hidden one is
//     allowed, since the call acts on what it issued, not on it. An owner
//     is refused with ErrOwnerProtected: its issues are never listed, and
//     the mark is cleared first (PUT .../role) if they are to be ended.
//   - Each credential is revoked as DELETE /v1/credentials/{id} revokes it:
//     audited credential_revoked (reason=issued revoke-all), its sessions
//     ended, its holder's device links closed and, for a guest, the visit
//     ended. No owner's credential is among them, so the last-owner guard
//     has nothing to ask.
//   - Each hook is revoked and audited hook_revoked with the same reason.
//   - One issued_revoked_all row records the call and the counts, even when
//     both are zero.
func (s *Store) RevokeIssued(ctx context.Context, actor string, actorID, issuerID int64) (RevokedIssued, error) {
	var out RevokedIssued
	var runs []EndedRun
	var guests []EndedGuest
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out, runs, guests = RevokedIssued{}, nil, nil
		if actorID != 0 {
			actorP, found, err := queryPrincipal(ctx, tx, actorID)
			if err != nil {
				return err
			}
			if !found || actorP.DisabledAt != nil || actorP.HiddenAt != nil || actorP.Kind != PrincipalHuman || !actorP.IsOwner() || actorP.guestTarget() {
				return ErrNotOwner
			}
		}
		issuer, found, err := queryPrincipal(ctx, tx, issuerID)
		if err != nil {
			return err
		}
		if !found {
			return ErrPrincipalNotFound
		}
		if issuer.IsOwner() {
			return ErrOwnerProtected
		}
		now := credentialNow().Truncate(time.Millisecond)
		const reason = "issued revoke-all"

		type cred struct {
			id, holder int64
			label      string
		}
		var creds []cred
		// The holder join and holderNotOwnerSQL are the list's: what the
		// owner was shown is what goes, and an owner's credential never.
		rows, err := tx.QueryContext(ctx,
			`SELECT c.id, c.principal_id, c.label
			   FROM credentials c JOIN principals p ON p.id = c.principal_id
			  WHERE c.issued_by = ? AND c.principal_id <> ? AND c.revoked_at IS NULL
			    AND `+holderNotOwnerSQL+` ORDER BY c.id`, issuerID, issuerID)
		if err != nil {
			return fmt.Errorf("store: revoke issued: list credentials: %w", err)
		}
		for rows.Next() {
			var c cred
			if err := rows.Scan(&c.id, &c.holder, &c.label); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: revoke issued: scan credential: %w", err)
			}
			creds = append(creds, c)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: revoke issued: list credentials: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: revoke issued: list credentials: %w", err)
		}
		for _, c := range creds {
			// No last-owner guard: no owner's credential is selected above,
			// so the sweep can never end the last way an owner signs in.
			res, err := tx.ExecContext(ctx,
				"UPDATE credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), c.id)
			if err != nil {
				return fmt.Errorf("store: revoke issued: credential %d: %w", c.id, err)
			}
			if n, err := res.RowsAffected(); err != nil {
				return fmt.Errorf("store: revoke issued: credential %d: %w", c.id, err)
			} else if n != 1 {
				return fmt.Errorf("store: revoke issued: credential %d: affected %d rows", c.id, n)
			}
			if err := appendAuditEventTx(ctx, tx, actor, "credential_revoked", principalActor(c.holder),
				credentialDetail(c.id, c.label)+" reason="+reason, now); err != nil {
				return err
			}
			if err := endCredentialNotificationsTx(ctx, tx, actor, c.id, now); err != nil {
				return err
			}
			if err := endCredentialSessionsTx(ctx, tx, actor, c.id, SessionEndedByRevocation, now, &runs); err != nil {
				return err
			}
			if err := closeDeviceLinksTx(ctx, tx, actor, c.holder, SignInLinkCloseCredentialRevoked, now); err != nil {
				return err
			}
			if err := endIfGuestTx(ctx, tx, actor, c.holder, schema.GuestEndOperator, now, &runs, &guests); err != nil {
				return err
			}
			out.Credentials = append(out.Credentials, c.id)
		}
		out.CredentialsRevoked = len(creds)

		type hook struct {
			id, channelID, holder int64
			label                 string
		}
		var hooks []hook
		rows, err = tx.QueryContext(ctx,
			`SELECT h.id, h.channel_id, h.principal_id, h.label
			   FROM hooks h JOIN principals p ON p.id = h.principal_id
			  WHERE h.issued_by = ? AND h.principal_id <> ? AND h.revoked_at IS NULL
			    AND `+holderNotOwnerSQL+` ORDER BY h.id`, issuerID, issuerID)
		if err != nil {
			return fmt.Errorf("store: revoke issued: list hooks: %w", err)
		}
		for rows.Next() {
			var h hook
			if err := rows.Scan(&h.id, &h.channelID, &h.holder, &h.label); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: revoke issued: scan hook: %w", err)
			}
			hooks = append(hooks, h)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: revoke issued: list hooks: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: revoke issued: list hooks: %w", err)
		}
		for _, h := range hooks {
			res, err := tx.ExecContext(ctx,
				"UPDATE hooks SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), h.id)
			if err != nil {
				return fmt.Errorf("store: revoke issued: hook %d: %w", h.id, err)
			}
			if n, err := res.RowsAffected(); err != nil {
				return fmt.Errorf("store: revoke issued: hook %d: %w", h.id, err)
			} else if n != 1 {
				return fmt.Errorf("store: revoke issued: hook %d: affected %d rows", h.id, n)
			}
			if err := appendAuditEventTx(ctx, tx, actor, "hook_revoked", principalActor(h.holder),
				hookDetail(h.id, h.channelID, h.label)+" reason="+reason, now); err != nil {
				return err
			}
		}
		out.HooksRevoked = len(hooks)
		return appendAuditEventTx(ctx, tx, actor, AuditIssuedRevokedAll, principalActor(issuerID),
			fmt.Sprintf("credentials=%d hooks=%d", out.CredentialsRevoked, out.HooksRevoked), now)
	})
	if err != nil {
		return RevokedIssued{}, err
	}
	s.announceRunsEnded(runs)
	s.announceGuestsEnded(guests)
	return out, nil
}
