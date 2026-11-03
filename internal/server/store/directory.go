package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The community member directory (ADR-013 decision 1, issue #983,
// docs/design/community-model.md §3.5, §5 and §13.11).

// knowsCond is the one SQL fragment that says which principals a viewer
// who does not hold see_member_directory may know (design §13.2, ADR-014):
// a condition on a principals row aliased p, for the viewer written as the
// SQL expression viewer ("?" for a bound id, "v.id" to ask it of every
// viewer at once). A viewer may know:
//
//   - themself (clause 3);
//   - a principal in a channel they are in, public or private (clause 4);
//   - a principal who joined through a link they made, p.invited_by (clause
//     5, O14b), while they are a member, which every caller of this has
//     already required.
//
// Clauses 1 and 2 of §13.2 (the setting is off; the viewer is an operator)
// are see_member_directory, decided by the callers before they get here.
// The fragment is built from constants and the viewer expression the caller
// passes, never from a request.
func knowsCond(viewer string) string {
	return knowsCondExcept(viewer, "")
}

// knowsCondExcept is knowsCond without the contribution of one channel,
// written as the SQL expression exceptChannel ("" for none): who would still
// know p if that channel did not link them. The directory signal asks it to
// find the viewers a membership change of that channel moved (§13.6).
func knowsCondExcept(viewer, exceptChannel string) string {
	skip := ""
	if exceptChannel != "" {
		skip = " AND m1.channel_id <> " + exceptChannel
	}
	return "(p.id = " + viewer + " OR p.invited_by = " + viewer +
		" OR EXISTS (SELECT 1 FROM channel_members m1 JOIN channel_members m2 ON m2.channel_id = m1.channel_id" +
		" WHERE m1.principal_id = " + viewer + " AND m2.principal_id = p.id" + skip + "))"
}

// knownPrincipalsSQL is the one place that says which principals a viewer
// may know (design §13.2, O14): a SQL condition on a principals row aliased
// p, with its arguments, and the scope of the list it makes. Every reader
// goes through it: the directory (ListDirectory), a channel manager's add
// (mayKnowTx) and the audience of the directory signal (DirectoryKnowers).
//
// seeAll is whether the viewer holds see_member_directory (ADR-014; always
// true for an operator), in which case they may know everyone. Without it
// the viewer may know what knowsCond says. The same answer bounds a channel
// manager's add (mayKnowTx), so a manager without the directory can add a
// channel-mate or one of their own joiners but cannot probe for, or recruit,
// anyone else.
func knownPrincipalsSQL(viewerID int64, seeAll bool) (cond string, args []any, scope schema.DirectoryScope) {
	if seeAll {
		return "1 = 1", nil, schema.DirectoryScopeEveryone
	}
	return knowsCond("?"), []any{viewerID, viewerID, viewerID}, schema.DirectoryScopeSharedChannels
}

// mayKnowTx reports whether viewerID may know targetID, by knownPrincipalsSQL.
func mayKnowTx(ctx context.Context, tx execer, viewerID, targetID int64, seeAll bool) (bool, error) {
	cond, args, _ := knownPrincipalsSQL(viewerID, seeAll)
	var known bool
	err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM principals p WHERE p.id = ? AND ("+cond+"))", // #nosec G202 -- cond is a constant of knownPrincipalsSQL
		append([]any{targetID}, args...)...).Scan(&known)
	if err != nil {
		return false, fmt.Errorf("store: may know %d: %w", targetID, err)
	}
	return known, nil
}

// DirectoryViewer is who asks for the directory and what the permissions
// document lets them see (ADR-014): SeeAll is see_member_directory, and
// Operator is manage_members, which fills the operator fields.
type DirectoryViewer struct {
	ID       int64
	SeeAll   bool
	Operator bool
}

// ListDirectory returns one page of the member directory for viewer: every
// member (isMemberTx's test, memberPredicateSQL) and every enabled,
// non-hidden agent that the viewer may know, ordered by id ascending, with
// id greater than after. At most limit rows are returned; After is set only
// when more follow, and is the id of the last row returned. The operator
// fields are filled for an Operator viewer and for no one else.
func (s *Store) ListDirectory(ctx context.Context, viewer DirectoryViewer, after int64, limit int) (schema.ServerMembersResponseV1, error) {
	out := schema.ServerMembersResponseV1{Members: []schema.ServerMemberV1{}}
	if limit <= 0 {
		return out, fmt.Errorf("store: list directory: limit must be positive, got %d", limit)
	}
	cond, args, scope := knownPrincipalsSQL(viewer.ID, viewer.SeeAll)
	out.Scope = scope
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.kind, p.name, p.role, p.owner, p.created_at, p.voice_allowed, p.may_invite, p.invited_by
		 FROM principals p
		 WHERE p.id > ? AND (`+cond+`)
		   AND ((`+memberPredicateSQL+`)
		        OR (p.kind = 'agent' AND p.disabled_at IS NULL AND p.hidden_at IS NULL))
		 ORDER BY p.id LIMIT ?`, // #nosec G202 -- cond and the predicate are constants; every value is an argument
		append(append([]any{after}, args...), limit+1)...)
	if err != nil {
		return out, fmt.Errorf("store: list directory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			m                schema.ServerMemberV1
			kind, role       string
			createdAt        int64
			owner            bool
			voice, mayInvite bool
			invitedBy        sql.NullInt64
		)
		if err := rows.Scan(&m.ID, &kind, &m.Name, &role, &owner, &createdAt, &voice, &mayInvite, &invitedBy); err != nil {
			return out, fmt.Errorf("store: list directory: %w", err)
		}
		m.Kind = schema.PrincipalKind(kind)
		m.CreatedAt = schema.NewTimestamp(time.UnixMilli(createdAt))
		if m.Kind == schema.PrincipalHuman {
			m.Role = schema.PrincipalRole(role)
			if viewer.Operator {
				m.Owner = owner
				m.VoiceAllowed = &voice
				m.MayInvite = &mayInvite
				if invitedBy.Valid {
					m.InvitedBy = &invitedBy.Int64
				}
			}
		}
		out.Members = append(out.Members, m)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: list directory: %w", err)
	}
	if len(out.Members) > limit {
		out.Members = out.Members[:limit]
		out.After = out.Members[limit-1].ID
	}
	return out, nil
}

// HasDefaultChannel reports whether any channel is a default channel, one
// every new member is put in (design §13.7): with members unable to see the
// directory it shows them to each other, which the permissions answer warns
// of (default_channel_exposes_members).
func (s *Store) HasDefaultChannel(ctx context.Context) (bool, error) {
	var has bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM channels WHERE is_default = 1)").Scan(&has); err != nil {
		return false, fmt.Errorf("store: has default channel: %w", err)
	}
	return has, nil
}

// DirectoryKnowers is who may know subjectID now, for the directory signal
// (design §13.6): the ids of the humans whose directory answer lists the
// subject. everyone is true when members hold see_member_directory: every
// member knows everyone, ids is then empty, and the caller addresses every
// human. Otherwise ids are the operators and the owner (who hold it
// always), the subject, and every member that knowsCond lets know it. A
// disabled, hidden or non-human principal never listens and is left out.
func (s *Store) DirectoryKnowers(ctx context.Context, subjectID int64) (everyone bool, ids []int64, err error) {
	d, err := readPermissionDocTx(ctx, s.db)
	if err != nil {
		return false, nil, err
	}
	if d.roleHolds(schema.PermissionRoleMember, schema.PermSeeMemberDirectory) {
		return true, nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.id FROM principals v
		 WHERE v.kind = 'human' AND v.disabled_at IS NULL AND v.hidden_at IS NULL
		   AND (v.role = 'operator' OR v.owner = 1 OR v.id = ?
		        OR EXISTS (SELECT 1 FROM principals p WHERE p.id = ? AND `+knowsCond("v.id")+`))
		 ORDER BY v.id`, // #nosec G202 -- the fragment is a constant of knowsCond
		subjectID, subjectID)
	if err != nil {
		return false, nil, fmt.Errorf("store: directory knowers of %d: %w", subjectID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return false, nil, fmt.Errorf("store: directory knowers of %d: %w", subjectID, err)
		}
		ids = append(ids, id)
	}
	return false, ids, rows.Err()
}

// DirectoryMembershipAudience is who is owed the directory signal after
// subjectID was added to or removed from channelID (design §13.6), read after
// the change committed. everyone is true when members hold
// see_member_directory: a membership then changes nobody's directory and
// nobody is told. Otherwise ids are the subject, the operators and the
// owner, and each member of the channel who would not know the subject
// without it, whose knowing of the subject therefore changed with the
// membership (the predicate after against before, by knowsCondExcept).
func (s *Store) DirectoryMembershipAudience(ctx context.Context, channelID, subjectID int64) (everyone bool, ids []int64, err error) {
	d, err := readPermissionDocTx(ctx, s.db)
	if err != nil {
		return false, nil, err
	}
	if d.roleHolds(schema.PermissionRoleMember, schema.PermSeeMemberDirectory) {
		return true, nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.id FROM principals v
		 WHERE v.kind = 'human' AND v.disabled_at IS NULL AND v.hidden_at IS NULL
		   AND (v.role = 'operator' OR v.owner = 1 OR v.id = ?
		        OR (EXISTS (SELECT 1 FROM channel_members c WHERE c.channel_id = ? AND c.principal_id = v.id)
		            AND NOT EXISTS (SELECT 1 FROM principals p WHERE p.id = ? AND `+knowsCondExcept("v.id", "?")+`)))
		 ORDER BY v.id`, // #nosec G202 -- the fragment is a constant of knowsCondExcept
		subjectID, channelID, subjectID, channelID)
	if err != nil {
		return false, nil, fmt.Errorf("store: directory membership audience of %d in %d: %w", subjectID, channelID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return false, nil, fmt.Errorf("store: directory membership audience of %d in %d: %w", subjectID, channelID, err)
		}
		ids = append(ids, id)
	}
	return false, ids, rows.Err()
}
