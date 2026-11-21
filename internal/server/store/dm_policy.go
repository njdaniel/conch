package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Who may contact whom by direct message, and the three settings that decide
// it (issue #2018; ADR-017 Decisions 3, 4, 9, 10 and 11; design §3). This is
// the one policy every DM and call operation consults, inside the write's own
// transaction, so a block, a switch or a preference written a moment before
// is seen by the very next write:
//
//   - dmWriteGateTx: the caller's own gate, the server-wide switch and the
//     caller's per-person switch. Its refusal, ErrDMsOff, says nothing about
//     anyone but the caller and the server.
//   - dmMayKnowTx and openDMTx's peer read: whether a principal is visible to
//     a viewer at all, by the directory's rule (knownPrincipalsSQL). A
//     principal that is not is ErrPrincipalNotFound, the answer for an id
//     never issued.
//   - dmContactTx: whether from may contact to now, for an open of a pair
//     from cannot yet see, a send, an edit or a call invite. Every reason is
//     the one ErrDMContactUnavailable, which never says which.
//   - dmPairReachableTx: whether a call between two people may go on (design
//     §4.1's "revoked" row): no block either way, both enabled, DMs on for
//     both and for the server.
//   - dmPeerFormerTx: whether a reader is shown the peer of a conversation
//     as a former member rather than by name.
//
// Reading retained history is not contact: it needs only pair membership and
// a standing reader (dm_visibility.go), and none of this applies to it.
// Neither does removing one's own message, moving a read mark or archiving.
// Act-as credentials (Decision 7) are a property of the request, refused by
// the routes before the store is asked (provenance.go).
//
// Nothing here writes an audit row naming a counterparty (Decision 8). The
// operator's two switches are audited as settings, naming only the person
// switched or the server; a person's own preference is not audited at all.

var (
	// ErrDMsOff is a write (an open of a pair the caller cannot yet see, a
	// send, an edit, a call invite) by a caller whose direct messages are
	// switched off, by an operator for them or for the whole server. It says
	// which of the two to nobody.
	ErrDMsOff = errors.New("store: direct messages are switched off")
)

// dmPolicyMigration adds the per-person switch (principals.dm_allowed) and
// the contact preferences (dm_contact_preferences). The server-wide switch is
// a server_settings row, which needs no migration: its absence is "on".
const dmPolicyMigration = 54

// settingDMsEnabled is the server_settings key of the server-wide switch.
// No row means on (schema.DefaultDMServerSettings).
const settingDMsEnabled = "dms_enabled"

// The audit actions of the operator's two switches (ADR-017 Decision 9,
// design §8). Each names only what was switched: subject principal:<id> and
// detail dm_allowed=<bool> for a person, subject server and detail
// dms_enabled=<bool> for the server. Neither ever names a conversation, a
// pair or a counterparty.
const (
	AuditDMPermissionChanged = "dm_permission_changed"
	AuditDMSettingsChanged   = "dm_settings_changed"
)

// dmPolicyMigrationStatements are migration 54's.
var dmPolicyMigrationStatements = []string{
	// 1 while the person may use direct messages (the default for every
	// existing and new principal), 0 when an operator switched them off.
	`ALTER TABLE principals ADD COLUMN dm_allowed INTEGER NOT NULL DEFAULT 1 CHECK (dm_allowed IN (0, 1))`,
	// A person's own contact preference; no row is everyone. It is a table of
	// its own, not a principals column, so no read of principals (operator
	// views included) can carry it: operators do not see it (design §3.3).
	`CREATE TABLE dm_contact_preferences (
	principal_id INTEGER PRIMARY KEY REFERENCES principals (id),
	accept       TEXT    NOT NULL CHECK (accept IN ('everyone', 'shared_channel', 'nobody')),
	updated_at   INTEGER NOT NULL
)`,
}

// dmServerEnabledTx reports the server-wide switch. No row is on; a value
// other than "true" or "false" is off, so a damaged row refuses contact
// rather than allowing it.
func dmServerEnabledTx(ctx context.Context, q execer) (bool, error) {
	var v string
	switch err := q.QueryRowContext(ctx, "SELECT value FROM server_settings WHERE key = ?", settingDMsEnabled).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return schema.DefaultDMServerSettings().DMsEnabled, nil
	case err != nil:
		return false, fmt.Errorf("store: read the direct message switch: %w", err)
	}
	return v == "true", nil
}

// dmPrincipalAllowedTx reports principalID's per-person switch. An id with no
// row is off. It reads one bit and names no one.
func dmPrincipalAllowedTx(ctx context.Context, q execer, principalID int64) (bool, error) {
	var allowed bool
	switch err := q.QueryRowContext(ctx, "SELECT dm_allowed FROM principals WHERE id = ?", principalID).Scan(&allowed); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: read the direct message switch of %d: %w", principalID, err)
	}
	return allowed, nil
}

// dmEnabledForTx is whether the server's switch and principalID's own are
// both on.
func dmEnabledForTx(ctx context.Context, q execer, principalID int64) (bool, error) {
	on, err := dmServerEnabledTx(ctx, q)
	if err != nil || !on {
		return false, err
	}
	return dmPrincipalAllowedTx(ctx, q, principalID)
}

// dmWriteGateTx is check 4 of the refusal order (schema/dm.go, design §3.1):
// on a write, the server's switch and the caller's own. It runs before the
// conversation or the target is looked up, so its answer depends on nothing
// but the caller and the server.
func dmWriteGateTx(ctx context.Context, q execer, caller int64) error {
	ok, err := dmEnabledForTx(ctx, q, caller)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDMsOff
	}
	return nil
}

// dmLivePrincipalCond holds for a principals row aliased p that is not hidden
// and is not a guest whose visit has ended: an identity that still exists for
// the purpose of being found. A disabled principal passes; whether it can be
// contacted is dmContactTx's question.
const dmLivePrincipalCond = `p.hidden_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM guest_visits gv WHERE gv.principal_id = p.id AND gv.state = 'ended')`

// dmMayKnowTx reports whether viewer may know target: target is a live
// identity (dmLivePrincipalCond) and the directory's rule admits it for
// viewer (knownPrincipalsSQL, with viewer's see_member_directory read in tx).
// On a server where members hold see_member_directory everyone may know
// everyone; on a hide_members server a member knows the people they share a
// channel with, and those who joined by their link.
func dmMayKnowTx(ctx context.Context, q execer, viewer, target int64) (bool, error) {
	seeAll, err := permittedTx(ctx, q, viewer, schema.PermSeeMemberDirectory)
	if err != nil {
		return false, err
	}
	cond, args, _ := knownPrincipalsSQL(viewer, seeAll)
	var known bool
	if err := q.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM principals p WHERE p.id = ? AND "+dmLivePrincipalCond+" AND ("+cond+"))", // #nosec G202 -- every fragment is a constant
		append([]any{target}, args...)...).Scan(&known); err != nil {
		return false, fmt.Errorf("store: may %d know %d: %w", viewer, target, err)
	}
	return known, nil
}

// dmBlockedEitherWayTx reports whether a block (#1832) holds in either
// direction between a and b. The answer is one bit for the pair: it never
// says who set it, and every caller turns it into the same refusal.
func dmBlockedEitherWayTx(ctx context.Context, q execer, a, b int64) (bool, error) {
	var blocked bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM blocks
		WHERE (owner_id = ?1 AND blocked_id = ?2) OR (owner_id = ?2 AND blocked_id = ?1))`, a, b).Scan(&blocked); err != nil {
		return false, fmt.Errorf("store: read blocks: %w", err)
	}
	return blocked, nil
}

// dmAcceptOfTx is principalID's contact preference, everyone when none is
// stored. A stored value outside the vocabulary (the table's CHECK forbids
// one) is taken as nobody.
func dmAcceptOfTx(ctx context.Context, q execer, principalID int64) (schema.DMAcceptPolicy, error) {
	var v string
	switch err := q.QueryRowContext(ctx, "SELECT accept FROM dm_contact_preferences WHERE principal_id = ?", principalID).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return schema.DefaultDMPreferences().Accept, nil
	case err != nil:
		return "", fmt.Errorf("store: read the contact preference of %d: %w", principalID, err)
	}
	if p := schema.DMAcceptPolicy(v); p.Valid() {
		return p, nil
	}
	return schema.DMAcceptNobody, nil
}

// dmShareChannelTx reports whether a and b are both members of at least one
// channel, public or private, now (design §3.3). A member banned from a
// channel does not share it (liveMembers, as the directory's rule reads it).
// The aliases differ from knowsCond's on purpose: this is the preference's
// question about two given people, not a read of principals.
func dmShareChannelTx(ctx context.Context, q execer, a, b int64) (bool, error) {
	var shared bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+liveMembers+" sa JOIN "+liveMembers+" sb ON sb.channel_id = sa.channel_id"+
		" WHERE sa.principal_id = ? AND sb.principal_id = ?)", a, b).Scan(&shared); err != nil {
		return false, fmt.Errorf("store: read shared channels: %w", err)
	}
	return shared, nil
}

// dmContactTx is the single seam of eligibility: whether from may contact to
// now, for an open of a pair from cannot yet see, a send, an edit or a call
// invite. Every act has the same rules (the preference applies to every
// write and invite, not only the first: design §3.3), so act only documents
// the caller. It runs inside the write's transaction, after the caller's
// standing and gate (dmWriteGateTx), and holds when every one of these does:
//
//  1. both stand: humans, enabled, not guests (dmStandingCond);
//  2. to's per-person switch is on (the server's was checked by the gate);
//  3. no block in either direction (#1832);
//  4. to's preference admits from: everyone; shared_channel and the two are
//     in a channel together now; never nobody;
//  5. mutual visibility: each may know the other by the directory's rule.
//     Where members hold see_member_directory this always holds; on a
//     hide_members server it is what makes contact need a channel in common
//     (Decision 11 keeps what was already said readable, with its authors'
//     names; it does not let anything new be said).
//
// Any failure is ErrDMContactUnavailable, one error for every reason
// (Decision 10). Every check is made whatever the earlier ones answered, so
// neither the answer nor the number of statements says which failed, or
// which of the two set a block.
func dmContactTx(ctx context.Context, q execer, from, to int64, act dmAct) error {
	_ = act
	ok := true
	for _, check := range []func() (bool, error){
		func() (bool, error) { return dmViewerStandsTx(ctx, q, from) },
		func() (bool, error) { return dmViewerStandsTx(ctx, q, to) },
		func() (bool, error) { return dmPrincipalAllowedTx(ctx, q, to) },
		func() (bool, error) {
			blocked, err := dmBlockedEitherWayTx(ctx, q, from, to)
			return !blocked, err
		},
		func() (bool, error) { return dmAcceptsTx(ctx, q, from, to) },
		func() (bool, error) { return dmMayKnowTx(ctx, q, from, to) },
		func() (bool, error) { return dmMayKnowTx(ctx, q, to, from) },
	} {
		pass, err := check()
		if err != nil {
			return err
		}
		ok = ok && pass
	}
	if !ok {
		return ErrDMContactUnavailable
	}
	return nil
}

// dmAcceptsTx reports whether to's contact preference admits from. The
// shared-channel question is asked whatever the preference, so the number of
// statements does not depend on it.
func dmAcceptsTx(ctx context.Context, q execer, from, to int64) (bool, error) {
	accept, err := dmAcceptOfTx(ctx, q, to)
	if err != nil {
		return false, err
	}
	shared, err := dmShareChannelTx(ctx, q, from, to)
	if err != nil {
		return false, err
	}
	switch accept {
	case schema.DMAcceptEveryone:
		return true, nil
	case schema.DMAcceptSharedChannel:
		return shared, nil
	default:
		return false, nil
	}
}

// dmPairReachableTx reports whether a and b may stay connected: a private
// call between them goes on only while it holds (design §4.1, the
// "revoked" row). Both stand, the server's switch and both people's are on,
// and no block holds either way. A preference or a visibility change does not
// end a call already accepted; it refuses the next invite (dmContactTx).
func dmPairReachableTx(ctx context.Context, q execer, a, b int64) (bool, error) {
	ok := true
	for _, check := range []func() (bool, error){
		func() (bool, error) { return dmViewerStandsTx(ctx, q, a) },
		func() (bool, error) { return dmViewerStandsTx(ctx, q, b) },
		func() (bool, error) { return dmEnabledForTx(ctx, q, a) },
		func() (bool, error) { return dmEnabledForTx(ctx, q, b) },
		func() (bool, error) {
			blocked, err := dmBlockedEitherWayTx(ctx, q, a, b)
			return !blocked, err
		},
	} {
		pass, err := check()
		if err != nil {
			return false, err
		}
		ok = ok && pass
	}
	return ok, nil
}

// dmPeerFormerTx reports whether viewer is shown peer as a former member,
// with no name (design §3.2, the "Target disabled" row's Read column): peer
// is no longer an active identity (disabled, hidden, or a guest whose visit
// ended) and viewer may no longer know it by the directory's rule. A hidden
// or ended identity is never known, so it is always former. A peer who is
// enabled is never former, even when a hide_members server no longer lets
// the viewer find them: what was said keeps its authors' names (Decision 11).
func dmPeerFormerTx(ctx context.Context, q execer, viewer, peer int64) (bool, error) {
	seeAll, err := permittedTx(ctx, q, viewer, schema.PermSeeMemberDirectory)
	if err != nil {
		return false, err
	}
	cond, args, _ := knownPrincipalsSQL(viewer, seeAll)
	var named bool
	if err := q.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM principals p WHERE p.id = ? AND "+dmLivePrincipalCond+
			" AND (p.disabled_at IS NULL OR ("+cond+")))", // #nosec G202 -- every fragment is a constant
		append([]any{peer}, args...)...).Scan(&named); err != nil {
		return false, fmt.Errorf("store: read the peer of %d: %w", viewer, err)
	}
	return !named, nil
}

// DMPreferences returns principalID's contact preference, everyone when they
// have written none. It is the person's own: the route serves it only to
// them.
func (s *Store) DMPreferences(ctx context.Context, principalID int64) (schema.DMPreferencesV1, error) {
	accept, err := dmAcceptOfTx(ctx, s.db, principalID)
	if err != nil {
		return schema.DMPreferencesV1{}, err
	}
	return schema.DMPreferencesV1{Accept: accept}, nil
}

// SetDMPreferences replaces principalID's contact preference and returns it.
// The person must stand (ErrDMNoStanding). It is not audited (design §8): it
// is the person's own and operators do not see it. It takes effect on the
// next write to them, which reads it in its own transaction.
func (s *Store) SetDMPreferences(ctx context.Context, principalID int64, prefs schema.DMPreferencesV1, now time.Time) (schema.DMPreferencesV1, error) {
	if err := prefs.Validate(); err != nil {
		return schema.DMPreferencesV1{}, err
	}
	err := s.withImmediateTx(ctx, func(tx execer) error {
		if ok, err := dmViewerStandsTx(ctx, tx, principalID); err != nil {
			return err
		} else if !ok {
			return ErrDMNoStanding
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dm_contact_preferences (principal_id, accept, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (principal_id) DO UPDATE SET accept = excluded.accept, updated_at = excluded.updated_at`,
			principalID, string(prefs.Accept), now.UnixMilli()); err != nil {
			return fmt.Errorf("store: set the contact preference of %d: %w", principalID, err)
		}
		return nil
	})
	if err != nil {
		return schema.DMPreferencesV1{}, err
	}
	return prefs, nil
}

// DMServerSettings returns the server-wide switch, on unless an operator
// turned it off.
func (s *Store) DMServerSettings(ctx context.Context) (schema.DMServerSettingsV1, error) {
	on, err := dmServerEnabledTx(ctx, s.db)
	if err != nil {
		return schema.DMServerSettingsV1{}, err
	}
	return schema.DMServerSettingsV1{DMsEnabled: on}, nil
}

// SetDMServerSettings sets the server-wide switch and reports whether it
// changed. callerID's change_server_settings is asked again under the write
// lock (ErrPermissionDenied); 0 means no caller (authentication off). A
// change is audited as dm_settings_changed, subject server, in the same
// transaction; a repeat writes nothing. It ends no live call by itself: the
// caller runs whatever ends them after it commits.
func (s *Store) SetDMServerSettings(ctx context.Context, actor string, callerID int64, settings schema.DMServerSettingsV1, now time.Time) (changed bool, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		if callerID != 0 {
			held, err := permittedTx(ctx, tx, callerID, schema.PermChangeServerSettings)
			if err != nil {
				return err
			}
			if !held {
				return &PermissionError{Err: ErrPermissionDenied, Permission: schema.PermChangeServerSettings,
					Reason: "switching direct messages for the server needs change_server_settings"}
			}
		}
		cur, err := dmServerEnabledTx(ctx, tx)
		if err != nil {
			return err
		}
		if cur == settings.DMsEnabled {
			// A damaged row reads as off; writing off over it changes
			// nothing anyone can observe, so it is left as it is.
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			settingDMsEnabled, fmt.Sprintf("%t", settings.DMsEnabled), now.UnixMilli()); err != nil {
			return fmt.Errorf("store: set the direct message switch: %w", err)
		}
		changed = true
		return appendAuditEventTx(ctx, tx, actor, AuditDMSettingsChanged, "server",
			fmt.Sprintf("dms_enabled=%t", settings.DMsEnabled), now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// DMsEnabledFor reports whether principalID may start or send direct
// messages now as far as the switches go: the server's and their own both on
// (DMSnapshotV1.DMsEnabled). It says nothing about anyone else.
func (s *Store) DMsEnabledFor(ctx context.Context, principalID int64) (bool, error) {
	return dmEnabledForTx(ctx, s.db, principalID)
}

// SetDMAllowed switches a person's direct messages on or off (ADR-017
// Decision 9), as SetVoiceAllowed does their voice: callerID's
// manage_members is asked again under the write lock (ErrPermissionDenied;
// 0 means no caller, authentication off), a caller who is not an owner gets
// ErrOwnerProtected for an owner target, a hidden or unknown id is
// ErrPrincipalNotFound and an agent ErrNotAPerson. A change is audited as
// dm_permission_changed naming only the person switched; a repeat writes
// nothing. It is content-blind: it reads and shows no conversation. It ends
// no live call by itself.
func (s *Store) SetDMAllowed(ctx context.Context, actor string, callerID, principalID int64, allowed bool, now time.Time) (changed bool, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		forbidOwner := false // no caller (authentication off): no owner protection
		if callerID != 0 {
			held, err := permittedTx(ctx, tx, callerID, schema.PermManageMembers)
			if err != nil {
				return err
			}
			if !held {
				return &PermissionError{Err: ErrPermissionDenied, Permission: schema.PermManageMembers,
					Reason: "switching a person's direct messages needs manage_members"}
			}
			var role string
			var owner bool
			if err := tx.QueryRowContext(ctx, "SELECT role, owner FROM principals WHERE id = ?", callerID).Scan(&role, &owner); err != nil {
				return fmt.Errorf("store: set dm_allowed: read caller: %w", err)
			}
			forbidOwner = Role(role) != RoleOperator || !owner
		}
		var (
			kind   string
			cur    bool
			hidden sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx, "SELECT kind, dm_allowed, hidden_at FROM principals WHERE id = ?", principalID).Scan(&kind, &cur, &hidden); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrPrincipalNotFound
		case err != nil:
			return fmt.Errorf("store: set dm_allowed: find principal: %w", err)
		}
		if hidden.Valid {
			return ErrPrincipalNotFound
		}
		if err := ownerProtectedTx(ctx, tx, principalID, forbidOwner); err != nil {
			return err
		}
		if PrincipalKind(kind) != PrincipalHuman {
			return ErrNotAPerson
		}
		if cur == allowed {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE principals SET dm_allowed = ? WHERE id = ?", boolInt(allowed), principalID); err != nil {
			return fmt.Errorf("store: set dm_allowed: %w", err)
		}
		changed = true
		return appendAuditEventTx(ctx, tx, actor, AuditDMPermissionChanged, principalActor(principalID),
			fmt.Sprintf("dm_allowed=%t", allowed), now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
