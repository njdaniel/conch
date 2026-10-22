package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/pkg/schema"
)

// Hiding principals and the operator's list of them (issue #445,
// docs/design/private-alpha.md §3). Deleting an account means disabling and
// hiding it: the principal is disabled, leaves every channel and net, and
// gives up its name for schema.HiddenPrincipalNamePrefix and its id, so the
// name can be taken again. Nothing is deleted: its messages stay, attributed
// to it under the new name, and every audit row stays, the old name among
// them.

// Audit actions written by hiding.
const (
	// AuditPrincipalHidden records a hide: actor the operator, subject the
	// principal, detail name=<the name it gave up>, quoted.
	AuditPrincipalHidden = "principal_hidden"
	// AuditPrincipalUnhidden records that enabling a hidden principal
	// cleared hidden_at. It restores no name, membership or credential.
	AuditPrincipalUnhidden = "principal_unhidden"
	// AuditPrincipalsBulkHidden summarises an operator's bulk hide: the
	// selector and the count. Each principal it hid has its own rows too.
	AuditPrincipalsBulkHidden = "principals_bulk_hidden"
)

// ErrHideOperator is returned by HidePrincipal, writing nothing, when the
// options refuse an operator and the target is one. A bulk hide never hides
// an operator.
var ErrHideOperator = errors.New("store: an operator is not hidden this way")

// hiddenAtMigration is the number of the migration that adds hidden_at: its
// Go step is reportReservedNames.
const hiddenAtMigration = 28

// maxHiddenNameTries bounds the numeric suffixes a hide tries when its name
// is somehow taken. Only names that existed before the prefix was reserved
// can be in the way, so the first try all but always succeeds.
const maxHiddenNameTries = 100

// HideOptions tunes HidePrincipal.
type HideOptions struct {
	// RefuseOperator makes an operator target ErrHideOperator. A bulk hide
	// sets it; an operator hiding one other operator by id does not.
	RefuseOperator bool
}

// HideResult is what a hide did.
type HideResult struct {
	// Principal is the principal as it is after the call.
	Principal Principal
	// Changed is false when the principal was already hidden: nothing was
	// written.
	Changed bool
}

// HidePrincipal hides principal id in one IMMEDIATE transaction:
//
//  1. It is disabled as DisablePrincipal disables it (credentials revoked,
//     sessions ended, its invites closed, each audited), unless it already
//     is. The last operator able to sign in is refused with ErrLastOperator.
//  2. It is removed from every channel, and so from every net, each removal
//     audited as member_removed and net_member_removed.
//  3. Every webhook that posts as it is revoked, audited as hook_revoked.
//  4. hidden_at is set.
//  5. Its name becomes "hidden-<id>" (with "-2", "-3"... after it should that
//     be taken), through the same write as a rename, audited
//     principal_renamed, and principal_hidden records the name it gave up.
//
// If any step fails nothing is written. Hiding a hidden principal changes
// nothing and writes nothing (Changed false). An unknown id is
// ErrPrincipalNotFound. Who may hide whom (never the caller) is the server's
// to decide.
//
// The principal's open approvals are left as they are. One that escalates to
// a hidden principal stalls (it does not fail open) until it expires; what
// should happen to it is the approval path's to decide: see the follow-up
// issue.
func (s *Store) HidePrincipal(ctx context.Context, actor string, id int64, opts HideOptions) (HideResult, error) {
	var (
		out  HideResult
		runs []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = HideResult{}, nil
		p, err := principalByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		// A guest's name is its handle and is never replaced (issue #705,
		// review F10): End visit is the lever, and its posts keep their
		// attribution whatever happens to it.
		if p.guestTarget() {
			return ErrGuestNotHideable
		}
		if p.HiddenAt != nil {
			out.Principal = p
			return nil
		}
		if opts.RefuseOperator && p.Role == RoleOperator {
			return ErrHideOperator
		}
		now := credentialNow().Truncate(time.Millisecond)
		if _, err := disablePrincipalTx(ctx, tx, actor, id, now, &runs); err != nil {
			return err
		}
		if err := removeAllMembershipsTx(ctx, tx, actor, id, now); err != nil {
			return err
		}
		if err := revokePrincipalHooksTx(ctx, tx, actor, id, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE principals SET hidden_at = ? WHERE id = ? AND hidden_at IS NULL", now.UnixMilli(), id); err != nil {
			return fmt.Errorf("store: hide principal %d: %w", id, err)
		}
		name, keys, err := hiddenNameTx(ctx, tx, id)
		if err != nil {
			return err
		}
		// A principal enabled after an earlier hide may still have the
		// name; it is not renamed to itself.
		if name != p.Name {
			if err := setNameTx(ctx, tx, actor, p, name, keys, now); err != nil {
				return err
			}
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPrincipalHidden, principalActor(id),
			fmt.Sprintf("name=%q", p.Name), now); err != nil {
			return err
		}
		if out.Principal, err = principalByIDTx(ctx, tx, id); err != nil {
			return err
		}
		out.Changed = true
		return nil
	})
	if err != nil {
		return HideResult{}, err
	}
	s.announceRunsEnded(runs)
	return out, nil
}

// removeAllMembershipsTx removes principalID from every channel it is in,
// which takes it off each channel's nets too, then from any net seat left in
// a channel it was not a member of, which no write should have made. Each
// removal is audited by the function that makes it.
func removeAllMembershipsTx(ctx context.Context, tx execer, actor string, principalID int64, now time.Time) error {
	channels, err := idsTx(ctx, tx,
		"SELECT channel_id FROM channel_members WHERE principal_id = ? ORDER BY channel_id", principalID)
	if err != nil {
		return fmt.Errorf("store: list memberships of principal %d: %w", principalID, err)
	}
	for _, ch := range channels {
		if _, err := removeChannelMemberTx(ctx, tx, actor, ch, principalID, now); err != nil {
			return err
		}
	}
	stray, err := idsTx(ctx, tx,
		`SELECT DISTINCT n.channel_id FROM net_members m JOIN nets n ON n.id = m.net_id
		 WHERE m.principal_id = ? ORDER BY n.channel_id`, principalID)
	if err != nil {
		return fmt.Errorf("store: list net seats of principal %d: %w", principalID, err)
	}
	for _, ch := range stray {
		if err := removeNetMembershipsTx(ctx, tx, actor, ch, principalID, now); err != nil {
			return err
		}
	}
	return nil
}

// revokePrincipalHooksTx revokes every live webhook that posts as
// principalID, one hook_revoked row each, as RevokeHook revokes one. A
// disabled principal's hooks already answer like unknown tokens, but enabling
// the principal again would bring them back; a hidden account's hooks must
// stay dead, as its credentials do. The token is never read.
func revokePrincipalHooksTx(ctx context.Context, tx execer, actor string, principalID int64, now time.Time) error {
	type hook struct {
		id, channelID int64
		label         string
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT id, channel_id, label FROM hooks WHERE principal_id = ? AND revoked_at IS NULL ORDER BY id", principalID)
	if err != nil {
		return fmt.Errorf("store: list hooks of principal %d: %w", principalID, err)
	}
	var found []hook
	for rows.Next() {
		var h hook
		if err := rows.Scan(&h.id, &h.channelID, &h.label); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: list hooks of principal %d: %w", principalID, err)
		}
		found = append(found, h)
	}
	err = rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("store: list hooks of principal %d: %w", principalID, err)
	}
	for _, h := range found {
		if _, err := tx.ExecContext(ctx,
			"UPDATE hooks SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now.UnixMilli(), h.id); err != nil {
			return fmt.Errorf("store: revoke hook %d: %w", h.id, err)
		}
		if err := appendAuditEventTx(ctx, tx, actor, "hook_revoked", principalActor(principalID),
			hookDetail(h.id, h.channelID, h.label)+" reason=principal hidden", now); err != nil {
			return err
		}
	}
	return nil
}

// idsTx runs a query that selects one integer column and returns the values.
func idsTx(ctx context.Context, tx execer, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, id)
	}
	err = rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return out, err
}

// hiddenNameTx returns the name a hide gives principal id, and its keys:
// "hidden-<id>", or with a numeric suffix if that is taken. It is the one
// writer of a reserved name, so it checks for a clash (clashTx) and not for
// the reservation (claimNameTx). Only a name written before the reservation
// existed can be in its way.
func hiddenNameTx(ctx context.Context, tx execer, id int64) (string, names.Keys, error) {
	base := fmt.Sprintf("%s%d", schema.HiddenPrincipalNamePrefix, id)
	for i := 1; i <= maxHiddenNameTries; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		keys := names.KeysOf(name)
		err := clashTx(ctx, tx, name, keys, id)
		if err == nil {
			return name, keys, nil
		}
		if !errors.Is(err, ErrDuplicate) {
			return "", names.Keys{}, err
		}
	}
	return "", names.Keys{}, fmt.Errorf("store: hide principal %d: every name from %q to %q-%d is taken", id, base, base, maxHiddenNameTries)
}

// HideSelector is whom a bulk hide asks for: the principals that joined
// through invite InviteID and whose name begins with NamePrefix, character
// for character, case included. Kind narrows either. A zero field matches
// everyone; the server requires InviteID or NamePrefix.
type HideSelector struct {
	InviteID   int64
	NamePrefix string
	Kind       PrincipalKind
}

// HideTargets returns, by id, the first limit principals a bulk hide with sel
// would hide, and how many more match. It never returns an operator, a
// principal already hidden, a guest (issue #705: not hideable), or exceptID
// (the caller). An unknown invite is
// ErrNotFound. It writes nothing: it is a bulk hide's dry run, and its
// selection.
func (s *Store) HideTargets(ctx context.Context, sel HideSelector, exceptID int64, limit int) ([]Principal, int, error) {
	if sel.InviteID > 0 {
		var one int
		switch err := s.db.QueryRowContext(ctx, "SELECT 1 FROM invites WHERE id = ?", sel.InviteID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, 0, fmt.Errorf("store: invite %d: %w", sel.InviteID, ErrNotFound)
		case err != nil:
			return nil, 0, fmt.Errorf("store: hide targets: %w", err)
		}
	}
	where := []string{"hidden_at IS NULL", "role <> 'operator'", "id <> ?",
		"NOT EXISTS (SELECT 1 FROM guest_visits v WHERE v.principal_id = principals.id AND v.state <> 'converted')"}
	args := []any{exceptID}
	if sel.InviteID > 0 {
		where = append(where, "invite_id = ?")
		args = append(args, sel.InviteID)
	}
	if sel.NamePrefix != "" {
		// substr counts characters, so the prefix is compared whole, with no
		// LIKE wildcard to escape.
		where = append(where, "substr(name, 1, ?) = ?")
		args = append(args, utf8.RuneCountInString(sel.NamePrefix), sel.NamePrefix)
	}
	if sel.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, string(sel.Kind))
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM principals WHERE "+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count hide targets: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, name FROM principals WHERE "+cond+" ORDER BY id LIMIT ?", append(args, limit)...) // #nosec G202 -- cond joins constants of this function; every value is in args
	if err != nil {
		return nil, 0, fmt.Errorf("store: hide targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Principal{}
	for rows.Next() {
		var p Principal
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, 0, fmt.Errorf("store: hide targets: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: hide targets: %w", err)
	}
	return out, max(0, total-len(out)), nil
}

// PrincipalFilter narrows ListPrincipals. A zero field matches everyone,
// except State: empty means active and disabled, everyone not hidden.
type PrincipalFilter struct {
	Kind       PrincipalKind
	Role       Role
	State      schema.PrincipalState
	InviteID   int64
	NamePrefix string
	// Guest, when set, lists only the principals whose guest visit is in
	// that state (issue #705).
	Guest schema.GuestState
	// Before, when positive, lists only ids below it.
	Before int64
	// Limit is the most rows returned; it must be positive.
	Limit int
}

// ListPrincipals returns the principals f selects, newest (highest id)
// first, as an operator sees them, with each one's count of credentials live
// at now. The result is never nil.
func (s *Store) ListPrincipals(ctx context.Context, f PrincipalFilter, now time.Time) ([]schema.PrincipalAdminV1, error) {
	if f.Limit <= 0 {
		return nil, fmt.Errorf("store: list principals: limit must be positive, got %d", f.Limit)
	}
	var where []string
	var args []any
	switch f.State {
	case "":
		where = append(where, "p.hidden_at IS NULL")
	case schema.PrincipalStateActive:
		where = append(where, "p.disabled_at IS NULL")
	case schema.PrincipalStateDisabled:
		where = append(where, "p.disabled_at IS NOT NULL", "p.hidden_at IS NULL")
	case schema.PrincipalStateHidden:
		where = append(where, "p.hidden_at IS NOT NULL")
	default:
		return nil, fmt.Errorf("store: list principals: unknown state %q", f.State)
	}
	if f.Kind != "" {
		where = append(where, "p.kind = ?")
		args = append(args, string(f.Kind))
	}
	if f.Role != "" {
		where = append(where, "p.role = ?")
		args = append(args, string(f.Role))
	}
	if f.InviteID > 0 {
		where = append(where, "p.invite_id = ?")
		args = append(args, f.InviteID)
	}
	if f.NamePrefix != "" {
		where = append(where, "substr(p.name, 1, ?) = ?")
		args = append(args, utf8.RuneCountInString(f.NamePrefix), f.NamePrefix)
	}
	if f.Guest != "" {
		where = append(where, "EXISTS (SELECT 1 FROM guest_visits v WHERE v.principal_id = p.id AND v.state = ?)")
		args = append(args, string(f.Guest))
	}
	if f.Before > 0 {
		where = append(where, "p.id < ?")
		args = append(args, f.Before)
	}
	return s.adminPrincipals(ctx, strings.Join(where, " AND "), args, f.Limit, now)
}

// PrincipalAdmin returns one principal as ListPrincipals lists it, hidden or
// not. An unknown id is ErrPrincipalNotFound.
func (s *Store) PrincipalAdmin(ctx context.Context, id int64, now time.Time) (schema.PrincipalAdminV1, error) {
	out, err := s.adminPrincipals(ctx, "p.id = ?", []any{id}, 1, now)
	if err != nil {
		return schema.PrincipalAdminV1{}, err
	}
	if len(out) == 0 {
		return schema.PrincipalAdminV1{}, ErrPrincipalNotFound
	}
	return out[0], nil
}

// adminPrincipals is the query behind ListPrincipals and PrincipalAdmin.
// cond is built from constant SQL; every value is in args.
func (s *Store) adminPrincipals(ctx context.Context, cond string, args []any, limit int, now time.Time) ([]schema.PrincipalAdminV1, error) {
	query := `SELECT p.id, p.kind, p.name, p.role, p.created_at, p.disabled_at, p.hidden_at, p.invited_by, p.invite_id,
		(SELECT COUNT(*) FROM credentials c
		 WHERE c.principal_id = p.id AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > ?)
		   AND (c.idle_ms IS NULL OR COALESCE(c.last_used_at, c.created_at) + c.idle_ms > ?)
		   AND ` + currentAuthSQL + `),
		(SELECT v.state FROM guest_visits v WHERE v.principal_id = p.id)
		FROM principals p WHERE ` + cond + ` ORDER BY p.id DESC LIMIT ?` // #nosec G202 -- cond joins constants of the callers; every value is in args
	all := append([]any{now.UnixMilli(), now.UnixMilli()}, args...)
	rows, err := s.db.QueryContext(ctx, query, append(all, limit)...)
	if err != nil {
		return nil, fmt.Errorf("store: list principals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.PrincipalAdminV1{}
	for rows.Next() {
		var (
			p                                       schema.PrincipalAdminV1
			kind, role                              string
			createdAt                               int64
			disabledAt, hiddenAt, invitedBy, invite sql.NullInt64
			guest                                   sql.NullString
		)
		if err := rows.Scan(&p.ID, &kind, &p.Name, &role, &createdAt, &disabledAt, &hiddenAt, &invitedBy, &invite, &p.LiveCredentials, &guest); err != nil {
			return nil, fmt.Errorf("store: list principals: %w", err)
		}
		p.Kind = schema.PrincipalKind(kind)
		p.Role = schema.PrincipalRole(role)
		p.CreatedAt = schema.NewTimestamp(time.UnixMilli(createdAt))
		p.DisabledAt = msToTimestampPtr(disabledAt)
		p.HiddenAt = msToTimestampPtr(hiddenAt)
		p.GuestState = schema.GuestState(guest.String)
		if invitedBy.Valid {
			p.InvitedBy = &invitedBy.Int64
		}
		if invite.Valid {
			p.InviteID = &invite.Int64
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list principals: %w", err)
	}
	return out, nil
}

// reportReservedNames is the hidden_at migration's Go step: it logs the
// principals whose names are now reserved for hidden principals. They keep
// their names and are refused nothing; an operator may rename them, and none
// of them may be renamed to another reserved name. A hide of anyone whose
// "hidden-<id>" one of them holds takes the next free suffix.
func reportReservedNames(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id, name FROM principals ORDER BY id")
	if err != nil {
		return fmt.Errorf("read principal names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var reserved []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return fmt.Errorf("scan principal name: %w", err)
		}
		if reservedName(names.KeysOf(name)) {
			reserved = append(reserved, id)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read principal names: %w", err)
	}
	// Ids only, as the name-key step logs them.
	if len(reserved) > 0 {
		slog.WarnContext(ctx, "store: some existing principal names start with the prefix now reserved for hidden principals; they are kept as they are, and an operator may rename them",
			"prefix", schema.HiddenPrincipalNamePrefix, "principals", reserved)
	}
	return nil
}
