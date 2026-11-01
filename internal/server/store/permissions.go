package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The permissions document (issue #1013, ADR-014,
// docs/design/admin-roles.md §4, §5, §7.1 and §9): each permission of the
// catalog (schema.Permissions) has a member cell and an operator cell, kept
// in role_permissions. An absent row reads as the catalog default
// (schema.Permission.Default), and a fixed cell (schema.Permission.Fixed) is
// decided here in code, never read from a row. Owners hold every permission
// and have no column; agents hold none.
//
// permittedTx is the one store question "does this principal hold this
// permission now"; every write a permission gates asks it again inside its
// own IMMEDIATE transaction (design §7.1). The document is read per call,
// with no cache.

// permissionsMigration is the number of the migration that adds
// role_permissions (issue #1013, design §9).
const permissionsMigration = 39

// The server_settings keys of the permissions document.
const (
	// settingPermissionsRevision is the document's revision: 1 until the
	// first change, and one more on every write that changes a cell. An
	// absent row reads 1.
	settingPermissionsRevision = "permissions_revision"
	// settingHideMembers is community-model §13.1's hide_members switch,
	// which ADR-014 replaces with the member cell of see_member_directory.
	// The permissions migration maps it where one exists and nothing reads
	// it after (design §9).
	settingHideMembers = "hide_members"
)

// Audit actions of the permissions document (design §8.5). Subject is
// always "server".
const (
	// AuditPermissionsChanged records a write that changed at least one
	// cell: actor the caller, detail revision=<n>, then preset=<name> when
	// the write applied a preset, then each changed cell as
	// <role>.<permission>=<old>-><new>, all joined by ";".
	AuditPermissionsChanged = "permissions_changed"
	// AuditPermissionsInitialized is written once, by the permissions
	// migration: actor "system", detail revision=1;source=defaults, or
	// revision=1;hide_members=<v>->member.see_member_directory=<!v> where
	// a hide_members setting was mapped.
	AuditPermissionsInitialized = "permissions_initialized"
)

// Errors of PutPermissions. Each refusal is returned as a *PermissionError
// wrapping one of these, so errors.Is matches the sentinel and errors.As
// gives the permission it names.
var (
	// ErrPermissionsInvalid: the request is malformed on its own (it fails
	// schema.PutPermissionsRequestV1.Validate), or names an unknown
	// permission or role, or a fixed cell. HTTP 400 invalid_request.
	ErrPermissionsInvalid = errors.New("store: invalid permissions change")
	// ErrPermissionsStale: the request's revision is not the document's
	// current one. HTTP 409 permissions_stale.
	ErrPermissionsStale = errors.New("store: permissions revision is stale")
	// ErrPermissionsConflict: the document the write would leave breaks
	// precedence (a member cell on while the operator cell is off) or a
	// dependency (schema.Permission.Requires) in a column. HTTP 409
	// permissions_conflict, naming the permission.
	ErrPermissionsConflict = errors.New("store: permissions conflict")
	// ErrPermissionsOwnerRequired: a writer who is not an owner sent a change
	// to the operator column. HTTP 403 forbidden, "owner required".
	ErrPermissionsOwnerRequired = errors.New("store: owner required to change the operator column")
	// ErrPermissionDenied: the writer lacks a permission the write needs:
	// change_server_settings to write the document at all, or, to turn a
	// member cell on, that permission itself (design §4.4.4). HTTP 403
	// permission_denied, naming the permission.
	ErrPermissionDenied = errors.New("store: permission denied")
)

// PermissionError is a refusal of PutPermissions. Err is one of the
// sentinels above; Permission and Role name the cell it concerns, when it
// concerns one. Reason is a plain sentence for the refusal's message; it
// never holds a token or a secret.
type PermissionError struct {
	Err        error
	Permission schema.Permission
	Role       schema.PermissionRole
	Reason     string
}

func (e *PermissionError) Error() string {
	if e.Reason == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + ": " + e.Reason
}

func (e *PermissionError) Unwrap() error { return e.Err }

// permissionCell names one cell of the document.
type permissionCell struct {
	role schema.PermissionRole
	perm schema.Permission
}

// permissionColumns are the document's two columns, in the order the audit
// detail and every walk over the cells use.
var permissionColumns = [...]schema.PermissionRole{schema.PermissionRoleMember, schema.PermissionRoleOperator}

// permissionDoc is the document as stored: the cells that have a row whose
// role and permission this build knows and which is not a fixed cell. Every
// other cell reads its fixed value or its default (cell).
type permissionDoc struct {
	stored map[permissionCell]bool
}

// cell reports the value of role r's cell for p, as the document says it:
// a fixed cell's fixed value, else the stored row, else the default. An
// unknown permission or role reads off.
func (d permissionDoc) cell(r schema.PermissionRole, p schema.Permission) bool {
	if fixed, v := p.Fixed(r); fixed {
		return v
	}
	if v, ok := d.stored[permissionCell{r, p}]; ok {
		return v
	}
	return p.Default(r)
}

// with returns a copy of d with the given cells set.
func (d permissionDoc) with(changes []PermissionCellChange) permissionDoc {
	out := permissionDoc{stored: make(map[permissionCell]bool, len(d.stored)+len(changes))}
	for k, v := range d.stored {
		out.stored[k] = v
	}
	for _, c := range changes {
		out.stored[permissionCell{c.Role, c.Permission}] = c.Granted
	}
	return out
}

// roleHolds reports whether role r's cell for p is on and, for members,
// whether operators' cell is on too (precedence). The store never writes a
// member cell above the operator's; applying the rule here as well means a
// row written by hand cannot grant past it. The dependencies are
// principalHolds's, since a brake on a permission required counts.
func (d permissionDoc) roleHolds(r schema.PermissionRole, p schema.Permission) bool {
	if !p.Valid() || !r.Valid() || !d.cell(r, p) {
		return false
	}
	return r != schema.PermissionRoleMember || d.cell(schema.PermissionRoleOperator, p)
}

// readPermissionDocTx reads the stored cells. A row whose role or
// permission this build does not know is ignored, and so is a row for a
// fixed cell: cell decides fixed cells in code.
func readPermissionDocTx(ctx context.Context, tx execer) (permissionDoc, error) {
	rows, err := tx.QueryContext(ctx, "SELECT role, permission, granted FROM role_permissions")
	if err != nil {
		return permissionDoc{}, fmt.Errorf("store: read permissions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	d := permissionDoc{stored: make(map[permissionCell]bool)}
	for rows.Next() {
		var role, perm string
		var granted int
		if err := rows.Scan(&role, &perm, &granted); err != nil {
			return permissionDoc{}, fmt.Errorf("store: read permissions: %w", err)
		}
		r, p := schema.PermissionRole(role), schema.Permission(perm)
		if !r.Valid() || !p.Valid() {
			continue
		}
		if fixed, _ := p.Fixed(r); fixed {
			continue
		}
		d.stored[permissionCell{r, p}] = granted == 1
	}
	if err := rows.Err(); err != nil {
		return permissionDoc{}, fmt.Errorf("store: read permissions: %w", err)
	}
	return d, nil
}

// readPermissionsRevisionTx reads the document's revision and when it last
// changed. An absent row is revision 1, never changed (a zero time). A
// stored value that is not a positive integer is an error, so nothing is
// written over a revision the store cannot read.
func readPermissionsRevisionTx(ctx context.Context, tx execer) (int64, time.Time, error) {
	var v string
	var at int64
	switch err := tx.QueryRowContext(ctx,
		"SELECT value, updated_at FROM server_settings WHERE key = ?", settingPermissionsRevision).Scan(&v, &at); {
	case errors.Is(err, sql.ErrNoRows):
		return 1, time.Time{}, nil
	case err != nil:
		return 0, time.Time{}, fmt.Errorf("store: read permissions revision: %w", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return 0, time.Time{}, fmt.Errorf("store: permissions revision %q is not a positive integer", v)
	}
	return n, time.UnixMilli(at), nil
}

// principalHolds reports whether principal p holds perm under document d
// (design §7.1), in this order:
//
//  1. an agent, a guest, a disabled or a hidden principal holds nothing;
//  2. an owner holds everything, and no brake applies to it (§4.3);
//  3. anyone else holds what its role holds (roleHolds: fixed cells in
//     code, else the stored cell or its default), only while no brake on
//     it withholds it (voice_allowed withholds use_voice and share_screen
//     from members and operators, and may_invite withholds
//     make_member_links from members only, §5), and only while it holds
//     every permission perm requires (share_screen needs use_voice,
//     act_as_members needs manage_members), brakes included.
func principalHolds(p Principal, d permissionDoc, perm schema.Permission) bool {
	if !perm.Valid() {
		return false
	}
	if p.Kind != PrincipalHuman || p.guestTarget() || p.DisabledAt != nil || p.HiddenAt != nil {
		return false
	}
	if p.IsOwner() {
		return true
	}
	var r schema.PermissionRole
	switch p.Role {
	case RoleOperator:
		r = schema.PermissionRoleOperator
	case RoleMember:
		r = schema.PermissionRoleMember
	default:
		return false
	}
	if !d.roleHolds(r, perm) || brakeWithholds(p, perm) {
		return false
	}
	for _, req := range perm.Requires() {
		if !principalHolds(p, d, req) {
			return false
		}
	}
	return true
}

// permittedTx reports whether principalID holds perm now, reading the
// principal and the document inside tx (design §7.1; principalHolds gives
// the order). A principal id with no row, and an unknown permission, answer
// false. Actor 0 (the system, or a server run with --auth off) has no row
// and so answers false too: each caller decides what actor 0 may do.
func permittedTx(ctx context.Context, tx execer, principalID int64, perm schema.Permission) (bool, error) {
	if !perm.Valid() {
		return false, nil
	}
	p, found, err := queryPrincipal(ctx, tx, principalID)
	if err != nil || !found {
		return false, err
	}
	d, err := readPermissionDocTx(ctx, tx)
	if err != nil {
		return false, err
	}
	return principalHolds(p, d, perm), nil
}

// principalPermittedTx is permittedTx for a principal already read inside
// tx: it reads only the document. A caller that has just re-read the actor
// under the write lock uses it to decide that actor's standing (§7.2.1).
func principalPermittedTx(ctx context.Context, tx execer, p Principal, perm schema.Permission) (bool, error) {
	if !perm.Valid() {
		return false, nil
	}
	d, err := readPermissionDocTx(ctx, tx)
	if err != nil {
		return false, err
	}
	return principalHolds(p, d, perm), nil
}

// ErrManageMembersRequired is a write on another principal's account (its
// name, its credentials) by an actor that does not hold manage_members now
// (ADR-014 design §7.2): the operator-or-self routes. HTTP 403 forbidden,
// naming manage_members to a role that could hold it.
var ErrManageMembersRequired = errors.New("store: acting on another principal needs manage_members")

// selfOrAdministratorTx is the standing of actorID writing to targetID's
// account, decided inside the write transaction (ADR-010 D7), so a
// permission removed after the handler asked cannot pass on the old answer.
// administers is true for actor 0 (the system, or --auth off) and for a
// holder of manage_members; otherwise the actor may act only on itself, and
// acting on anyone else is ErrManageMembersRequired.
func selfOrAdministratorTx(ctx context.Context, tx execer, actorID, targetID int64) (administers bool, err error) {
	if actorID == 0 {
		return true, nil
	}
	if administers, err = permittedTx(ctx, tx, actorID, schema.PermManageMembers); err != nil {
		return false, err
	}
	if !administers && actorID != targetID {
		return false, ErrManageMembersRequired
	}
	return administers, nil
}

// brakeWithholds reports whether a per-person brake on p withholds perm
// (design §5), whatever the document says of perm: voice_allowed = 0
// withholds use_voice and share_screen from members and operators, and
// may_invite = 0 withholds make_member_links from members only. An owner
// skips brakes (§4.3). It is the one statement of the brakes: principalHolds
// ANDs it in, and SetMayInvite asks it alone, so a permission switched off
// by the document (reversible) is never taken for the brake (final).
func brakeWithholds(p Principal, perm schema.Permission) bool {
	if p.IsOwner() {
		return false
	}
	switch perm {
	case schema.PermUseVoice, schema.PermShareScreen:
		return !p.VoiceAllowed
	case schema.PermMakeMemberLinks:
		return PermissionRoleOf(p) == schema.PermissionRoleMember && !p.MayInvite
	}
	return false
}

// mayInviteBrakeBindsTx reports whether principalID's may_invite brake, as it
// is now in tx, withholds make_member_links (brakeWithholds). A principal
// with no row answers false.
func mayInviteBrakeBindsTx(ctx context.Context, tx execer, principalID int64) (bool, error) {
	p, found, err := queryPrincipal(ctx, tx, principalID)
	if err != nil || !found {
		return false, err
	}
	return brakeWithholds(p, schema.PermMakeMemberLinks), nil
}

// PermissionRoleOf is the document column that p's role reads (design §1):
// operator for an operator, owner or not, and member for anyone else. It
// names a column; it decides nothing, and an owner holds every permission
// whatever its column says (principalHolds). A client of it asks
// Permission.Fixed whether a refusal can be lifted for that role, to word
// the refusal (design §7.4).
func PermissionRoleOf(p Principal) schema.PermissionRole {
	if p.Role == RoleOperator {
		return schema.PermissionRoleOperator
	}
	return schema.PermissionRoleMember
}

// Permitted reports whether principalID holds perm now (permittedTx), read
// in one read transaction. A write that a permission gates asks permittedTx
// again inside its own transaction instead.
func (s *Store) Permitted(ctx context.Context, principalID int64, perm schema.Permission) (bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, fmt.Errorf("store: permitted: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return permittedTx(ctx, tx, principalID, perm)
}

// HeldBy reports which of perms p holds by the document as it is now, in
// one read of the document and none of the principal: for a reader that has
// just read p and asks about several permissions at once, as an events
// socket does for each roster. The answer is in perms' order. It decides
// reads only; a write a permission gates asks permittedTx in its own
// transaction.
func (s *Store) HeldBy(ctx context.Context, p Principal, perms ...schema.Permission) ([]bool, error) {
	d, err := readPermissionDocTx(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(perms))
	for i, perm := range perms {
		out[i] = principalHolds(p, d, perm)
	}
	return out, nil
}

// effectivePermissionsTx lists what principalID holds now, in catalog
// order: every permission for an owner, none for an agent, a guest, a
// disabled, hidden or unknown principal, and otherwise its role's cells
// with its brakes applied. Never nil.
func effectivePermissionsTx(ctx context.Context, tx execer, principalID int64) ([]schema.Permission, error) {
	out := []schema.Permission{}
	p, found, err := queryPrincipal(ctx, tx, principalID)
	if err != nil || !found {
		return out, err
	}
	d, err := readPermissionDocTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, perm := range schema.Permissions {
		if principalHolds(p, d, perm) {
			out = append(out, perm)
		}
	}
	return out, nil
}

// EffectivePermissions lists what principalID holds now, in catalog order
// (effectivePermissionsTx), with the document's revision: what whoami
// reports (design §8.3). The slice is the caller's.
func (s *Store) EffectivePermissions(ctx context.Context, principalID int64) ([]schema.Permission, int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, fmt.Errorf("store: effective permissions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	perms, err := effectivePermissionsTx(ctx, tx, principalID)
	if err != nil {
		return nil, 0, err
	}
	rev, _, err := readPermissionsRevisionTx(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	return perms, rev, nil
}

// PermissionsDocument is the permissions document as the store holds it:
// the parts of schema.PermissionsResponseV1 that are the document's own.
// The caller adds the presets, the editable columns, the warnings and
// issued_to_others.
type PermissionsDocument struct {
	Revision int64
	// Rows is every permission of the catalog once, in catalog order, with
	// fixed cells at their fixed value and marked fixed. Each Rows is a new
	// slice, the caller's.
	Rows []schema.PermissionRowV1
	// Preset is the preset the member column equals, or "".
	Preset schema.PermissionPreset
	// UpdatedAt is when the document last changed: when a cell last
	// changed, or when it was initialized. Zero where neither is recorded.
	UpdatedAt time.Time
}

func permissionsDocumentTx(ctx context.Context, tx execer) (PermissionsDocument, error) {
	d, err := readPermissionDocTx(ctx, tx)
	if err != nil {
		return PermissionsDocument{}, err
	}
	rev, at, err := readPermissionsRevisionTx(ctx, tx)
	if err != nil {
		return PermissionsDocument{}, err
	}
	return PermissionsDocument{Revision: rev, Rows: d.rows(), Preset: d.matchedPreset(), UpdatedAt: at}, nil
}

// rows builds the document's rows in catalog order.
func (d permissionDoc) rows() []schema.PermissionRowV1 {
	out := make([]schema.PermissionRowV1, 0, len(schema.Permissions))
	for _, p := range schema.Permissions {
		mFixed, _ := p.Fixed(schema.PermissionRoleMember)
		oFixed, _ := p.Fixed(schema.PermissionRoleOperator)
		out = append(out, schema.PermissionRowV1{
			Permission:    p,
			Member:        d.cell(schema.PermissionRoleMember, p),
			Operator:      d.cell(schema.PermissionRoleOperator, p),
			MemberFixed:   mFixed,
			OperatorFixed: oFixed,
		})
	}
	return out
}

// presetCells reports the member cells preset pr sets: each member cell of
// the catalog that is not fixed, on when the preset lists it. ok is false
// for an unknown preset.
func presetCells(pr schema.PermissionPreset) (cells map[schema.Permission]bool, ok bool) {
	on, ok := schema.PresetMemberPermissions[pr]
	if !ok || !pr.Valid() {
		return nil, false
	}
	cells = make(map[schema.Permission]bool, len(schema.Permissions))
	for _, p := range schema.Permissions {
		if fixed, _ := p.Fixed(schema.PermissionRoleMember); fixed {
			continue
		}
		cells[p] = slices.Contains(on, p)
	}
	return cells, true
}

// matchedPreset reports the preset whose member column equals d's, or "".
func (d permissionDoc) matchedPreset() schema.PermissionPreset {
	presets := make([]schema.PermissionPreset, 0, len(schema.PresetMemberPermissions))
	for pr := range schema.PresetMemberPermissions {
		presets = append(presets, pr)
	}
	sort.Slice(presets, func(i, j int) bool { return presets[i] < presets[j] })
	for _, pr := range presets {
		cells, ok := presetCells(pr)
		if !ok {
			continue
		}
		match := true
		for p, v := range cells {
			if d.cell(schema.PermissionRoleMember, p) != v {
				match = false
				break
			}
		}
		if match {
			return pr
		}
	}
	return ""
}

// Permissions reads the permissions document (GET /v1/permissions).
func (s *Store) Permissions(ctx context.Context) (PermissionsDocument, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PermissionsDocument{}, fmt.Errorf("store: permissions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return permissionsDocumentTx(ctx, tx)
}

// PermissionCellChange is one cell a write changed, from !Granted to
// Granted.
type PermissionCellChange struct {
	Role       schema.PermissionRole
	Permission schema.Permission
	Granted    bool
}

// PermissionsWrite is the outcome of PutPermissions.
type PermissionsWrite struct {
	// Document is the document after the write.
	Document PermissionsDocument
	// Changes are the cells the write changed, in catalog order, member
	// before operator; empty for a write that changed nothing, which kept
	// the revision and wrote no row.
	Changes []PermissionCellChange
}

// PutPermissions applies a PUT /v1/permissions (design §4.4, §8.2) for the
// principal actorID; actor is its audit actor. It runs in one IMMEDIATE
// transaction, which re-reads the writer and the stored cells under the
// write lock and checks, in this order:
//
//  1. the request on its own (schema.PutPermissionsRequestV1.Validate, run
//     again here), and no change to an unknown or a fixed cell:
//     ErrPermissionsInvalid;
//  2. the writer: an enabled, visible human who is not a guest, and either
//     an owner, or a writer holding change_server_settings who sends no
//     change to the operator column (ErrPermissionsOwnerRequired, else
//     ErrPermissionDenied naming change_server_settings). Actor 0 has no
//     row and is refused;
//  3. the revision: ErrPermissionsStale unless it is the current one;
//  4. a preset expands into the member cells it changes against the column
//     as it stands, and from here is checked exactly as those changes sent
//     by hand would be. A write that changes no cell returns the document
//     as it is: no row, no audit, the same revision;
//  5. the document the write would leave: in both columns every
//     dependency holds, and every member cell that is on is on for
//     operators too (ErrPermissionsConflict, naming the permission);
//  6. a writer who is not an owner turning a member cell on holds that
//     permission itself (permittedTx's rule, applied to the writer and the
//     rows read here: its role's cell and its own brakes), else
//     ErrPermissionDenied naming it. Turning a cell off is never
//     bounded. An owner holds everything.
//
// A write that changes something writes each changed cell, moves the
// revision on by one and appends one permissions_changed audit row, all in
// the same transaction.
func (s *Store) PutPermissions(ctx context.Context, actor string, actorID int64, req schema.PutPermissionsRequestV1, now time.Time) (PermissionsWrite, error) {
	// The change is attributed to the writer who is checked, and to no one
	// else (the security review of #1013): an audit actor that is not
	// actorID's is refused before anything is read. Actor 0 has no audit
	// actor of its own and no row, and is refused below as before.
	if actorID != 0 && actor != principalActor(actorID) {
		return PermissionsWrite{}, &PermissionError{Err: ErrPermissionsInvalid, Reason: "the audit actor is not the writer"}
	}
	if err := req.Validate(); err != nil {
		return PermissionsWrite{}, &PermissionError{Err: ErrPermissionsInvalid, Reason: strings.TrimPrefix(err.Error(), "schema: ")}
	}
	for _, c := range req.Changes {
		if !c.Permission.Valid() || !c.Role.Valid() {
			return PermissionsWrite{}, &PermissionError{Err: ErrPermissionsInvalid, Reason: "unknown permission or role"}
		}
		if fixed, _ := c.Permission.Fixed(c.Role); fixed {
			return PermissionsWrite{}, &PermissionError{Err: ErrPermissionsInvalid, Permission: c.Permission, Role: c.Role,
				Reason: fmt.Sprintf("the %s cell of %s is fixed", c.Role, c.Permission)}
		}
	}
	now = now.Truncate(time.Millisecond)
	var out PermissionsWrite
	err := s.withImmediateTx(ctx, func(tx execer) error {
		var err error
		out, err = putPermissionsTx(ctx, tx, actor, actorID, req, now)
		return err
	})
	if err != nil {
		return PermissionsWrite{}, err
	}
	return out, nil
}

func putPermissionsTx(ctx context.Context, tx execer, actor string, actorID int64, req schema.PutPermissionsRequestV1, now time.Time) (PermissionsWrite, error) {
	writer, found, err := queryPrincipal(ctx, tx, actorID)
	if err != nil {
		return PermissionsWrite{}, err
	}
	denied := &PermissionError{Err: ErrPermissionDenied, Permission: schema.PermChangeServerSettings,
		Reason: "changing the permissions document needs change_server_settings"}
	if !found || writer.Kind != PrincipalHuman || writer.guestTarget() || writer.DisabledAt != nil || writer.HiddenAt != nil {
		return PermissionsWrite{}, denied
	}
	owner := writer.IsOwner()
	d, err := readPermissionDocTx(ctx, tx)
	if err != nil {
		return PermissionsWrite{}, err
	}
	if !owner {
		for _, c := range req.Changes {
			if c.Role == schema.PermissionRoleOperator {
				return PermissionsWrite{}, &PermissionError{Err: ErrPermissionsOwnerRequired, Permission: c.Permission, Role: c.Role,
					Reason: "only an owner changes the operator column"}
			}
		}
		if !principalHolds(writer, d, schema.PermChangeServerSettings) {
			return PermissionsWrite{}, denied
		}
	}

	rev, _, err := readPermissionsRevisionTx(ctx, tx)
	if err != nil {
		return PermissionsWrite{}, err
	}
	if req.Revision != rev {
		return PermissionsWrite{}, &PermissionError{Err: ErrPermissionsStale,
			Reason: fmt.Sprintf("the document is at revision %d, not %d", rev, req.Revision)}
	}

	changes, err := cellChanges(d, req)
	if err != nil {
		return PermissionsWrite{}, err
	}
	if len(changes) == 0 {
		doc, err := permissionsDocumentTx(ctx, tx)
		return PermissionsWrite{Document: doc, Changes: []PermissionCellChange{}}, err
	}

	if err := checkPermissionDoc(d.with(changes)); err != nil {
		return PermissionsWrite{}, err
	}
	if !owner {
		for _, c := range changes {
			if c.Granted && !principalHolds(writer, d, c.Permission) {
				return PermissionsWrite{}, &PermissionError{Err: ErrPermissionDenied, Permission: c.Permission, Role: c.Role,
					Reason: fmt.Sprintf("you can't give members %s, which you don't hold yourself", c.Permission)}
			}
		}
	}

	detail := []string{"revision=" + strconv.FormatInt(rev+1, 10)}
	if req.Preset != "" {
		detail = append(detail, "preset="+string(req.Preset))
	}
	for _, c := range changes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO role_permissions (role, permission, granted, updated_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (role, permission) DO UPDATE SET granted = excluded.granted, updated_at = excluded.updated_at`,
			string(c.Role), string(c.Permission), boolInt(c.Granted), now.UnixMilli()); err != nil {
			return PermissionsWrite{}, fmt.Errorf("store: write permission %s.%s: %w", c.Role, c.Permission, err)
		}
		detail = append(detail, fmt.Sprintf("%s.%s=%t->%t", c.Role, c.Permission, !c.Granted, c.Granted))
	}
	if err := writePermissionsRevisionTx(ctx, tx, rev+1, now); err != nil {
		return PermissionsWrite{}, err
	}
	if err := appendAuditEventTx(ctx, tx, actor, AuditPermissionsChanged, "server", strings.Join(detail, ";"), now); err != nil {
		return PermissionsWrite{}, err
	}
	doc, err := permissionsDocumentTx(ctx, tx)
	if err != nil {
		return PermissionsWrite{}, err
	}
	return PermissionsWrite{Document: doc, Changes: changes}, nil
}

// cellChanges turns a validated request into the cells it changes against
// d, in catalog order, member before operator. A preset expands into the
// member cells it sets; a patch keeps the cells it names whose value it
// changes. Cells already at the requested value are left out.
func cellChanges(d permissionDoc, req schema.PutPermissionsRequestV1) ([]PermissionCellChange, error) {
	want := make(map[permissionCell]bool)
	if req.Preset != "" {
		cells, ok := presetCells(req.Preset)
		if !ok {
			return nil, &PermissionError{Err: ErrPermissionsInvalid, Reason: fmt.Sprintf("unknown preset %q", req.Preset)}
		}
		for p, v := range cells {
			want[permissionCell{schema.PermissionRoleMember, p}] = v
		}
	} else {
		for _, c := range req.Changes {
			want[permissionCell{c.Role, c.Permission}] = c.Granted
		}
	}
	var out []PermissionCellChange
	for _, p := range schema.Permissions {
		for _, r := range permissionColumns {
			v, ok := want[permissionCell{r, p}]
			if !ok || d.cell(r, p) == v {
				continue
			}
			if fixed, _ := p.Fixed(r); fixed {
				return nil, &PermissionError{Err: ErrPermissionsInvalid, Permission: p, Role: r,
					Reason: fmt.Sprintf("the %s cell of %s is fixed", r, p)}
			}
			out = append(out, PermissionCellChange{Role: r, Permission: p, Granted: v})
		}
	}
	return out, nil
}

// checkPermissionDoc checks the invariants of a whole document (design
// §4.4.1 and §4.4.2): for every permission a member cell that is on is on
// for operators too, and in each column every cell that is on has every
// cell it requires on. It names the first permission in catalog order that
// breaks one.
func checkPermissionDoc(d permissionDoc) error {
	for _, p := range schema.Permissions {
		if d.cell(schema.PermissionRoleMember, p) && !d.cell(schema.PermissionRoleOperator, p) {
			return &PermissionError{Err: ErrPermissionsConflict, Permission: p, Role: schema.PermissionRoleMember,
				Reason: fmt.Sprintf("members can't hold %s while operators don't", p)}
		}
		for _, r := range permissionColumns {
			if !d.cell(r, p) {
				continue
			}
			for _, req := range p.Requires() {
				if !d.cell(r, req) {
					return &PermissionError{Err: ErrPermissionsConflict, Permission: p, Role: r,
						Reason: fmt.Sprintf("%s needs %s, which is off for %ss", p, req, r)}
				}
			}
		}
	}
	return nil
}

func writePermissionsRevisionTx(ctx context.Context, tx execer, rev int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingPermissionsRevision, strconv.FormatInt(rev, 10), now.UnixMilli()); err != nil {
		return fmt.Errorf("store: write permissions revision: %w", err)
	}
	return nil
}

// initializePermissions is the Go step of the permissions migration
// (design §9). The table starts empty, so every cell reads its default. A
// hide_members setting, if one exists, becomes the member cell of
// see_member_directory (on exactly when members were not hidden) and is
// not read again; a value that is not a boolean is read as hidden, the
// narrower answer. Then the revision is written as 1 and one
// permissions_initialized row is audited.
//
// A new database, one with no principal, no audit row and no setting yet,
// is left as it is: "a new server writes nothing, and the defaults apply"
// (design §9), and an absent revision reads 1. So the trail of a new server
// starts with what its people do, and only an upgraded server records
// where its document began.
func initializePermissions(ctx context.Context, tx *sql.Tx) error {
	var history bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM principals) OR EXISTS (SELECT 1 FROM audit_events)
		     OR EXISTS (SELECT 1 FROM server_settings)`).Scan(&history); err != nil {
		return fmt.Errorf("read whether the database is new: %w", err)
	}
	if !history {
		return nil
	}
	now := credentialNow().Truncate(time.Millisecond)
	detail := "revision=1;source=defaults"
	var v string
	switch err := tx.QueryRowContext(ctx, "SELECT value FROM server_settings WHERE key = ?", settingHideMembers).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read hide_members: %w", err)
	default:
		hide, perr := strconv.ParseBool(v)
		if perr != nil {
			slog.Warn("store: the hide_members setting is not a boolean; members start hidden from each other (member see_member_directory off)")
			hide = true
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO role_permissions (role, permission, granted, updated_at) VALUES (?, ?, ?, ?)",
			string(schema.PermissionRoleMember), string(schema.PermSeeMemberDirectory), boolInt(!hide), now.UnixMilli()); err != nil {
			return fmt.Errorf("map hide_members: %w", err)
		}
		detail = fmt.Sprintf("revision=1;hide_members=%t->member.%s=%t", hide, schema.PermSeeMemberDirectory, !hide)
	}
	if err := writePermissionsRevisionTx(ctx, tx, 1, now); err != nil {
		return err
	}
	return appendAuditEventTx(ctx, tx, "system", AuditPermissionsInitialized, "server", detail, now)
}

// issuedDetail reads the credential or hook a credential_created,
// credential_rotated or hook_created audit row names: its detail starts
// "credential=<id>" or "hook=<id>" (credentialDetail, hookDetail).
var issuedDetail = regexp.MustCompile(`^(credential|hook)=([0-9]+)\b`)

// IssuedToOthers lists the live credentials and hooks that someone other
// than an owner issued to a principal other than themself (ADR-014 design
// §7.5, O13): what the answer to a PUT that turns act_as_members off for
// operators lists, so that the owner can revoke them. Taking the permission
// away ends none of them.
//
// It is joined from the audit trail, the only record of who issued what:
// credential_created, credential_rotated and hook_created rows (written with
// their actor since e48b6e3, 2026-10-08), whose actor and subject are both
// "principal:<id>" and differ. An item issued before those rows existed has
// no row and is absent. An owner's own issues are left out: owners keep
// act_as_members. An item is live while it is not revoked (and, for a
// credential, not expired at now) and still belongs to the subject. Each
// item is listed once, at its first issuing row, oldest first.
func (s *Store) IssuedToOthers(ctx context.Context, now time.Time) ([]schema.IssuedByOperatorV1, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("store: issued to others: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT actor, subject, detail, created_at FROM audit_events
		 WHERE action IN ('credential_created', 'credential_rotated', 'hook_created')
		   AND actor LIKE 'principal:%' AND subject LIKE 'principal:%' AND actor <> subject
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: issued to others: read audit rows: %w", err)
	}
	type issued struct {
		kind             schema.IssuedKind
		id, target, from int64
		at               int64
	}
	var found []issued
	for rows.Next() {
		var actor, subject, detail string
		var at int64
		if err := rows.Scan(&actor, &subject, &detail, &at); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: issued to others: scan audit row: %w", err)
		}
		from, ok1 := principalOfActor(actor)
		target, ok2 := principalOfActor(subject)
		m := issuedDetail.FindStringSubmatch(detail)
		if !ok1 || !ok2 || m == nil {
			continue
		}
		id, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		kind := schema.IssuedKind(m[1])
		found = append(found, issued{kind: kind, id: id, target: target, from: from, at: at})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store: issued to others: read audit rows: %w", err)
	}
	_ = rows.Close()

	out := []schema.IssuedByOperatorV1{}
	seen := map[string]bool{}
	owners := map[int64]bool{}
	for _, it := range found {
		key := string(it.kind) + ":" + strconv.FormatInt(it.id, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		owner, known := owners[it.from]
		if !known {
			p, ok, err := queryPrincipal(ctx, tx, it.from)
			if err != nil {
				return nil, fmt.Errorf("store: issued to others: read issuer: %w", err)
			}
			owner = ok && p.IsOwner()
			owners[it.from] = owner
		}
		if owner {
			continue
		}
		var holder int64
		var query string
		args := []any{it.id}
		switch it.kind {
		case schema.IssuedCredential:
			query = `SELECT principal_id FROM credentials WHERE id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`
			args = append(args, now.UnixMilli())
		case schema.IssuedHook:
			query = `SELECT principal_id FROM hooks WHERE id = ? AND revoked_at IS NULL`
		default:
			continue
		}
		switch err := tx.QueryRowContext(ctx, query, args...).Scan(&holder); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("store: issued to others: read %s %d: %w", it.kind, it.id, err)
		}
		if holder != it.target {
			continue
		}
		out = append(out, schema.IssuedByOperatorV1{Kind: it.kind, ID: it.id, PrincipalID: it.target, IssuedBy: it.from,
			IssuedAt: schema.NewTimestamp(time.UnixMilli(it.at))})
	}
	return out, nil
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
