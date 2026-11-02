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

// knownPrincipalsSQL is the one place that says which principals a viewer
// may know (design §13.11, O14): a SQL condition on a principals row aliased
// p, with its arguments, and the scope of the list it makes. Both readers go
// through it: the directory (ListDirectory) and a channel manager's add
// (mayKnowTx).
//
// seeAll is whether the viewer holds see_member_directory (ADR-014; always
// true for an operator). In this slice everyone who holds it may know
// everyone. Without it the viewer may know themself and the members of the
// channels they are in: clauses 1 and 2 of design §13.2's may-know list.
// I14a (#988) adds the third (the people their links brought in) here, behind
// this function, and nothing else changes. The same answer bounds a channel
// manager's add (mayKnowTx), so a manager without the directory can add a
// channel-mate but cannot probe for, or recruit, anyone else.
func knownPrincipalsSQL(viewerID int64, seeAll bool) (cond string, args []any, scope schema.DirectoryScope) {
	if seeAll {
		return "1 = 1", nil, schema.DirectoryScopeEveryone
	}
	return "(p.id = ? OR EXISTS (SELECT 1 FROM channel_members m1 JOIN channel_members m2 ON m2.channel_id = m1.channel_id" +
			" WHERE m1.principal_id = ? AND m2.principal_id = p.id))",
		[]any{viewerID, viewerID}, schema.DirectoryScopeSharedChannels
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
