package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The permissions document's routes (issue #1015, ADR-014,
// docs/design/admin-roles.md §8.2): GET /v1/permissions for any operator,
// and PUT /v1/permissions for a holder of change_server_settings, the
// operator column for an owner alone. The store holds the document and
// checks every write (store.PutPermissions); this file answers the routes,
// keeps the safety net (enforcedPermissions), joins issued_to_others and
// says what changed on the events socket.

// enforcedPermissions is every permission whose every gate reads can() or
// permittedTx today, so that turning its cell off is honoured everywhere it
// applies (SF-3, design §12). The PUT refuses a change to any other cell
// (409 permissions_conflict, "this server does not enforce <permission>
// yet"), so a release that ships a permission's cell before its gates
// refuses the change rather than ignoring it.
//
//   - Rows 1 and 2 gate a route that does not exist yet (#981): they are
//     enforced vacuously, so the personal preset can apply before them.
//   - Rows 7-11 are enforced by the route classes of this issue and the
//     store's standing checks of A3a (#1014).
//   - Rows 3 (make_member_links), 5 (use_voice) and 6 (share_screen) join
//     in A4 (#1016), which ships in the same release.
//
// TestEnforcedPermissionsAreHonest fails when a permission is listed here
// while one of its gates still reads only the role or the brake.
var enforcedPermissions = []schema.Permission{
	schema.PermCreatePublicChannels,
	schema.PermCreatePrivateChannels,
	schema.PermSeeMemberDirectory,
	schema.PermManageMembers,
	schema.PermActAsMembers,
	schema.PermManageChannels,
	schema.PermChangeServerSettings,
	schema.PermReadAuditLog,
}

// handleGetPermissions serves GET /v1/permissions: the document, for any
// operator (O10; the route is accessOperator). With authentication off there
// is no caller, and nothing is editable.
func (s *Server) handleGetPermissions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	doc, err := s.store.Permissions(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "permissions: read the document failed", "error", err)
		writeInternalError(w)
		return
	}
	resp, err := s.permissionsResponse(ctx, doc)
	if err != nil {
		slog.ErrorContext(ctx, "permissions: build the answer failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// permissionsResponse is the document as the routes answer it: the store's
// part, with the presets, what the caller may edit and the warnings.
func (s *Server) permissionsResponse(ctx context.Context, doc store.PermissionsDocument) (schema.PermissionsResponseV1, error) {
	resp := schema.PermissionsResponseV1{
		Revision:  doc.Revision,
		Rows:      doc.Rows,
		Preset:    doc.Preset,
		Presets:   permissionPresets(),
		Editable:  []schema.PermissionRole{},
		UpdatedAt: schema.NewTimestamp(doc.UpdatedAt),
	}
	if caller, ok := callerFrom(ctx); ok {
		switch {
		case isOwner(caller):
			resp.Editable = []schema.PermissionRole{schema.PermissionRoleMember, schema.PermissionRoleOperator}
		default:
			settings, err := s.can(ctx, caller, schema.PermChangeServerSettings)
			if err != nil {
				return schema.PermissionsResponseV1{}, err
			}
			if settings {
				resp.Editable = []schema.PermissionRole{schema.PermissionRoleMember}
			}
		}
	}
	warnings, err := s.permissionsWarnings(ctx, doc)
	if err != nil {
		return schema.PermissionsResponseV1{}, err
	}
	resp.Warnings = warnings
	return resp, nil
}

// permissionPresets is every preset as the server defines it, in the order
// of its vocabulary, each member list in catalog order.
func permissionPresets() []schema.PermissionPresetV1 {
	out := []schema.PermissionPresetV1{}
	for _, name := range schema.Vocabularies["PermissionPreset"] {
		p := schema.PermissionPreset(name)
		on := schema.PresetMemberPermissions[p]
		member := []schema.Permission{}
		for _, perm := range schema.Permissions {
			if slices.Contains(on, perm) {
				member = append(member, perm)
			}
		}
		out = append(out, schema.PermissionPresetV1{Preset: p, Member: member})
	}
	return out
}

// memberCell reads the member cell of perm in rows.
func memberCell(rows []schema.PermissionRowV1, perm schema.Permission) bool {
	for _, row := range rows {
		if row.Permission == perm {
			return row.Member
		}
	}
	return perm.Default(schema.PermissionRoleMember)
}

// permissionsWarnings are the conditions of design §6, shown and never
// refused:
//
//   - default_channel_exposes_members: the member directory is off and at
//     least one default channel exists. There are no default channels until
//     #979 adds them, so it is never raised yet.
//   - open_signup_on: member links are off for members, and the address
//     still admits accounts (open joining that takes accounts, not paused;
//     open sign-up once #987 replaces the joining modes).
func (s *Server) permissionsWarnings(ctx context.Context, doc store.PermissionsDocument) ([]schema.PermissionsWarning, error) {
	out := []schema.PermissionsWarning{}
	if !memberCell(doc.Rows, schema.PermMakeMemberLinks) {
		join, _, err := s.store.JoinSettings(ctx)
		if err != nil {
			return nil, err
		}
		mode := join.OpenJoining
		if (mode == schema.OpenJoiningAccount || mode == schema.OpenJoiningBoth) && !join.JoiningPaused {
			out = append(out, schema.PermissionsWarningOpenSignupOn)
		}
	}
	return out, nil
}

// handlePutPermissions serves PUT /v1/permissions (accessPermission +
// change_server_settings, reauthAlways). It refuses, in this order:
//
//  1. a body that is not one well-formed request: 400 invalid_request;
//  2. no caller (authentication off): 401, since a change is made by an
//     owner or an operator it can name, never by the system (the store
//     refuses actor 0);
//  3. an operator-column change from someone who is not an owner: 403
//     forbidden "owner required", audited;
//  4. a change to a cell this server does not enforce yet
//     (enforcedPermissions): 409 permissions_conflict;
//  5. whatever the store refuses (store.PutPermissions): the stale revision
//     (409 permissions_stale), precedence and the dependencies (409
//     permissions_conflict), a member cell the writer does not hold itself
//     (403 permission_denied).
//
// A write that changed something tells every human events socket
// (conch.permissions_changed.v1), ends any share whose share_screen is now
// withheld, and, when it turned act_as_members off for operators, answers
// issued_to_others.
func (s *Server) handlePutPermissions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req schema.PutPermissionsRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the body must be one permissions change: revision, and changes or a preset")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// As the role route does: the body first, then who is asking.
	caller, ok := callerFrom(ctx)
	if !ok {
		writeUnauthenticated(w)
		return
	}
	if !isOwner(caller) {
		for _, c := range req.Changes {
			if c.Role == schema.PermissionRoleOperator {
				s.denyForbidden(w, r, caller, "forbidden", "owner required")
				return
			}
		}
	}
	if perm, unenforced, ok := s.unenforcedChange(w, r, req); !ok {
		return
	} else if unenforced {
		writeError(w, http.StatusConflict, schema.ErrorCodePermissionsConflict,
			fmt.Sprintf("this server does not enforce %s yet", perm))
		return
	}

	out, err := s.store.PutPermissions(ctx, auditActor(ctx), caller.ID, req, s.now())
	if err != nil {
		s.writePermissionsRefusal(w, r, caller, err)
		return
	}
	resp, err := s.permissionsResponse(ctx, out.Document)
	if err != nil {
		slog.ErrorContext(ctx, "permissions: build the answer failed", "error", err)
		writeInternalError(w)
		return
	}
	if len(out.Changes) > 0 {
		// The document is server-wide, so telling everyone it changed
		// tells no one anything about anyone (design §8.6).
		s.events.notifyPermissionsChangedAll()
		if lostShare(out.Changes) {
			s.endWithheldShares(ctx)
		}
		if turnedOff(out.Changes, schema.PermissionRoleOperator, schema.PermActAsMembers) {
			issued, err := s.store.IssuedToOthers(ctx, s.now())
			if err != nil {
				// The change is made; the list is a help to the owner, so
				// the answer goes without it and says so in the log.
				slog.ErrorContext(ctx, "permissions: list what operators issued to others failed", "error", err)
			} else if len(issued) > 0 {
				resp.IssuedToOthers = issued
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// unenforcedChange reports the first cell req would change whose permission
// is not in enforcedPermissions. The cells are worked out from the document
// as it stands; when that is not the revision req read, the store refuses
// req as stale whatever it asks, so nothing is decided here (ok, false).
func (s *Server) unenforcedChange(w http.ResponseWriter, r *http.Request, req schema.PutPermissionsRequestV1) (schema.Permission, bool, bool) {
	doc, err := s.store.Permissions(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "permissions: read the document failed", "error", err)
		writeInternalError(w)
		return "", false, false
	}
	if doc.Revision != req.Revision {
		return "", false, true
	}
	cell := func(role schema.PermissionRole, perm schema.Permission) bool {
		for _, row := range doc.Rows {
			if row.Permission == perm {
				if role == schema.PermissionRoleMember {
					return row.Member
				}
				return row.Operator
			}
		}
		return perm.Default(role)
	}
	changes := req.Changes
	if req.Preset != "" {
		on := schema.PresetMemberPermissions[req.Preset]
		changes = nil
		for _, perm := range schema.Permissions {
			if fixed, _ := perm.Fixed(schema.PermissionRoleMember); fixed {
				continue
			}
			changes = append(changes, schema.PermissionChangeV1{Permission: perm, Role: schema.PermissionRoleMember, Granted: slices.Contains(on, perm)})
		}
	}
	for _, c := range changes {
		if cell(c.Role, c.Permission) == c.Granted {
			continue
		}
		if !slices.Contains(enforcedPermissions, c.Permission) {
			return c.Permission, true, true
		}
	}
	return "", false, true
}

// writePermissionsRefusal answers a refusal of store.PutPermissions.
func (s *Server) writePermissionsRefusal(w http.ResponseWriter, r *http.Request, caller store.Principal, err error) {
	var perr *store.PermissionError
	reason := ""
	var perm schema.Permission
	if errors.As(err, &perr) {
		reason, perm = perr.Reason, perr.Permission
	}
	switch {
	case errors.Is(err, store.ErrPermissionsInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", "the permissions change is not valid: "+reason)
	case errors.Is(err, store.ErrPermissionsStale):
		writeError(w, http.StatusConflict, schema.ErrorCodePermissionsStale,
			"the permissions changed since you read them: read them again and decide again")
	case errors.Is(err, store.ErrPermissionsConflict):
		writeError(w, http.StatusConflict, schema.ErrorCodePermissionsConflict, "the change would break a rule of the permissions: "+reason)
	case errors.Is(err, store.ErrPermissionsOwnerRequired):
		s.denyForbidden(w, r, caller, "forbidden", "owner required")
	case errors.Is(err, store.ErrPermissionDenied):
		if named := refusedPermission(caller, perm); named != "" {
			s.denyForbiddenFor(w, r, caller, schema.ErrorCodePermissionDenied, reason, named)
			return
		}
		s.denyForbidden(w, r, caller, "forbidden", "operator role required")
	default:
		slog.ErrorContext(r.Context(), "permissions: change failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	}
}

// turnedOff reports whether changes turned role's cell of perm off.
func turnedOff(changes []store.PermissionCellChange, role schema.PermissionRole, perm schema.Permission) bool {
	return slices.ContainsFunc(changes, func(c store.PermissionCellChange) bool {
		return c.Role == role && c.Permission == perm && !c.Granted
	})
}

// lostShare reports whether changes may have withheld share_screen from
// someone: share_screen, or use_voice, which it needs, turned off in a
// column.
func lostShare(changes []store.PermissionCellChange) bool {
	return slices.ContainsFunc(changes, func(c store.PermissionCellChange) bool {
		return !c.Granted && (c.Permission == schema.PermShareScreen || c.Permission == schema.PermUseVoice)
	})
}

// sharingPrincipals is everyone the poller holds a share or a running claim
// for, in a live room or a retiring one, each once, in id order: whom a
// change that may withhold share_screen from a whole column asks again
// (endWithheldShares). It reads p.mu only.
func (p *voicePoller) sharingPrincipals() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[int64]bool{}
	for _, r := range p.rooms {
		if r.share != nil {
			seen[r.share.principalID] = true
		}
		if r.claimer != 0 {
			seen[r.claimer] = true
		}
	}
	for _, pid := range p.retiring {
		if pid != 0 {
			seen[pid] = true
		}
	}
	out := make([]int64, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	slices.Sort(out)
	return out
}

// endWithheldShare ends principalID's share, in every room, because its
// share_screen was withheld: the stop path's access_lost, as every other
// revocation's.
func (s *Server) endWithheldShare(ctx context.Context, principalID int64) {
	s.voice.endSharesOf(ctx, principalID, 0, voiceShareStopAccessLost)
}

// endShareIfWithheld is endWithheldShares for one principal: after a change
// to that principal alone (a role change).
func (s *Server) endShareIfWithheld(ctx context.Context, principalID int64) {
	if s.lk == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	holds, err := s.store.Permitted(ctx, principalID, schema.PermShareScreen)
	if err != nil {
		slog.ErrorContext(ctx, "permissions: read share_screen failed; ending the share", "principal", principalID, "error", err)
	} else if holds {
		return
	}
	s.endWithheldShare(ctx, principalID)
}

// endWithheldShares ends the share of everyone the poller holds one or a
// claim for who no longer holds share_screen (the security review of #1061,
// S1): the voice passes read only the person's brake, not the document, so
// without this a share would live on until stopped. It runs after the
// change is committed and before it is answered.
func (s *Server) endWithheldShares(ctx context.Context) {
	if s.lk == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, pid := range s.voice.sharingPrincipals() {
		holds, err := s.store.Permitted(ctx, pid, schema.PermShareScreen)
		if err != nil {
			// Fail closed: a share whose right cannot be read is ended.
			slog.ErrorContext(ctx, "permissions: read share_screen failed; ending the share", "principal", pid, "error", err)
		} else if holds {
			continue
		}
		s.endWithheldShare(ctx, pid)
	}
}
