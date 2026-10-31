package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Roles and permissions wire shapes (issue #1012, ADR-014,
// docs/design/admin-roles.md §8). The roles are fixed (owner, operator,
// member, and the per-channel manager); what each server role may do is a
// closed catalog of permissions, each with a member cell and an operator
// cell. Owners hold every permission and have no column. Agents hold no role
// and no permission: their rights stay their manifest's capabilities and
// channel grants (ADR-000 D10).
//
// Everything here is additive. The REST bodies carry no schema-name field
// and are versioned by their V1 suffix; the events document carries one
// (PermissionsChangedSchemaV1).

// Permission names one configurable right of a server role (ADR-014). The
// vocabulary is closed on the server, which refuses an unknown value, and
// client-tolerant: a client shows a value it does not know by its name and
// never sends one.
//
// The rule for adding a permission later: its default (Default) must
// reproduce what the server did before it existed, so that an upgrade
// changes nothing. An absent stored row reads as the default, so a new value
// needs no migration (design §4.1, §9). A new constant goes at the end of
// the catalog, in Permissions and in Vocabularies.
type Permission string

const (
	// PermCreatePublicChannels: create a public channel, and become its
	// manager. Members and operators: on by default.
	PermCreatePublicChannels Permission = "create_public_channels"
	// PermCreatePrivateChannels: create a private channel, and become its
	// manager. Members and operators: on by default.
	PermCreatePrivateChannels Permission = "create_private_channels"
	// PermMakeMemberLinks: make member links (and personal invites on v1
	// while that route answers). Members and operators: on by default. The
	// may_invite brake withholds it from members only.
	PermMakeMemberLinks Permission = "make_member_links"
	// PermSeeMemberDirectory: read the whole member directory. Members: on by
	// default. Operators: fixed on.
	PermSeeMemberDirectory Permission = "see_member_directory"
	// PermUseVoice: join voice in channels one is in. Members and operators:
	// on by default. The voice_allowed brake withholds it.
	PermUseVoice Permission = "use_voice"
	// PermShareScreen: share a screen in voice. Requires PermUseVoice in the
	// same column. Members and operators: on by default. The voice_allowed
	// brake withholds it.
	PermShareScreen Permission = "share_screen"
	// PermManageMembers: administer people and agents without speaking as
	// them (design §4.1 row 7). Operator-only; on by default.
	PermManageMembers Permission = "manage_members"
	// PermActAsMembers: sign in as, and post as, any person or agent who
	// isn't an owner: credentials, sign-in links and hooks for someone else.
	// Requires PermManageMembers in the same column. Operator-only; on by
	// default.
	PermActAsMembers Permission = "act_as_members"
	// PermManageChannels: administer every channel, and see and name
	// channels the holder is not in (design §4.1 row 9). Operator-only; on
	// by default.
	PermManageChannels Permission = "manage_channels"
	// PermChangeServerSettings: change server-wide policy, and the member
	// column of the permissions document. Operator-only; off by default.
	PermChangeServerSettings Permission = "change_server_settings"
	// PermReadAuditLog: export the audit trail, which names every member and
	// every private channel. Operator-only; off by default.
	PermReadAuditLog Permission = "read_audit_log"
)

// Permissions is the catalog in wire order: the order of every list of
// permissions on the wire and in every view.
var Permissions = []Permission{
	PermCreatePublicChannels,
	PermCreatePrivateChannels,
	PermMakeMemberLinks,
	PermSeeMemberDirectory,
	PermUseVoice,
	PermShareScreen,
	PermManageMembers,
	PermActAsMembers,
	PermManageChannels,
	PermChangeServerSettings,
	PermReadAuditLog,
}

// Valid reports whether p is a permission of the catalog.
func (p Permission) Valid() bool {
	switch p {
	case PermCreatePublicChannels, PermCreatePrivateChannels, PermMakeMemberLinks, PermSeeMemberDirectory,
		PermUseVoice, PermShareScreen, PermManageMembers, PermActAsMembers, PermManageChannels,
		PermChangeServerSettings, PermReadAuditLog:
		return true
	default:
		return false
	}
}

// OperatorOnly reports whether p can never be granted to members: rows 7 to
// 11 of the catalog (manage_members, act_as_members, manage_channels,
// change_server_settings, read_audit_log). An unknown permission is
// operator-only, so it can never reach a member.
func (p Permission) OperatorOnly() bool {
	switch p {
	case PermCreatePublicChannels, PermCreatePrivateChannels, PermMakeMemberLinks, PermSeeMemberDirectory,
		PermUseVoice, PermShareScreen:
		return false
	default:
		return true
	}
}

// Fixed reports whether role r's cell for p is fixed, and if so its value
// (design §4.1). The member cell of every operator-only permission is fixed
// off, and the operator cell of see_member_directory is fixed on. Every
// other cell of the catalog is configurable, and Fixed answers (false,
// false) for it.
//
// The store applies fixed cells in code and never reads them from a row, so
// a stored row that contradicts one is ignored; nothing writes one (design
// §7.1, §9). An unknown permission or role is answered as fixed off, so it
// can never be granted.
func (p Permission) Fixed(r PermissionRole) (fixed, value bool) {
	if !p.Valid() || !r.Valid() {
		return true, false
	}
	switch {
	case r == PermissionRoleMember && p.OperatorOnly():
		return true, false
	case r == PermissionRoleOperator && p == PermSeeMemberDirectory:
		return true, true
	default:
		return false, false
	}
}

// Default reports p's default for role r: the value a cell has while no row
// is stored for it (design §4.1). Members hold rows 1 to 6 (channel
// creation, member links, the directory, voice and screen share); operators
// hold rows 1 to 9; change_server_settings and read_audit_log are off for
// both. The defaults reproduce what a server did before ADR-014. An unknown
// permission or role defaults to off.
func (p Permission) Default(r PermissionRole) bool {
	if !p.Valid() || !r.Valid() {
		return false
	}
	switch p {
	case PermChangeServerSettings, PermReadAuditLog:
		return false
	case PermManageMembers, PermActAsMembers, PermManageChannels:
		return r == PermissionRoleOperator
	default:
		return true
	}
}

// Requires lists the permissions p depends on in the same column (design
// §4.4.2): share_screen needs use_voice, and act_as_members needs
// manage_members. A cell may be on only while every cell it requires in its
// column is on; the store checks this on every write and ANDs it into every
// answer. Nil for a permission with no dependency. The slice is the
// caller's.
func (p Permission) Requires() []Permission {
	switch p {
	case PermShareScreen:
		return []Permission{PermUseVoice}
	case PermActAsMembers:
		return []Permission{PermManageMembers}
	default:
		return nil
	}
}

// PermissionRole names a column of the permissions document. Closed and
// client-tolerant. Owners have no column: they hold every permission. A
// later "guest" column is an addition (design §3.4). It is not
// PrincipalRole: the wire keeps a principal's role as operator or member
// plus the owner mark (ADR-010).
type PermissionRole string

const (
	PermissionRoleMember   PermissionRole = "member"
	PermissionRoleOperator PermissionRole = "operator"
)

// Valid reports whether r is a column of the permissions document.
func (r PermissionRole) Valid() bool {
	switch r {
	case PermissionRoleMember, PermissionRoleOperator:
		return true
	default:
		return false
	}
}

// PermissionPreset names a value of the member column (design §6). Applying
// one sets the member column and nothing else, once; it is not a stored
// mode. Closed and client-tolerant.
type PermissionPreset string

const (
	// PresetPersonal: a personal server. Members keep voice and screen share
	// and nothing else: no channel creation, no member links, no directory.
	PresetPersonal PermissionPreset = "personal"
	// PresetGroup: a group server, equal to the member defaults.
	PresetGroup PermissionPreset = "group"
)

// Valid reports whether p is a known preset.
func (p PermissionPreset) Valid() bool {
	switch p {
	case PresetPersonal, PresetGroup:
		return true
	default:
		return false
	}
}

// PresetMemberPermissions is each preset's member column: the permissions it
// turns on, in catalog order. It turns every other member permission off.
// The server sends this table in the permissions answer (presets), so no
// client keeps a copy of it.
var PresetMemberPermissions = map[PermissionPreset][]Permission{
	PresetPersonal: {PermUseVoice, PermShareScreen},
	PresetGroup: {
		PermCreatePublicChannels, PermCreatePrivateChannels, PermMakeMemberLinks,
		PermSeeMemberDirectory, PermUseVoice, PermShareScreen,
	},
}

// PermissionsWarning is a condition of the permissions document the owner
// should know about, never a refusal (design §6, O11). Closed and
// client-tolerant.
type PermissionsWarning string

const (
	// PermissionsWarningDefaultChannelExposesMembers: the member directory is
	// off for members and at least one default channel exists, whose members
	// all see each other.
	PermissionsWarningDefaultChannelExposesMembers PermissionsWarning = "default_channel_exposes_members"
	// PermissionsWarningOpenSignupOn: member links are off for members, but
	// open sign-up still lets anyone in through the address.
	PermissionsWarningOpenSignupOn PermissionsWarning = "open_signup_on"
)

// Valid reports whether w is a known warning.
func (w PermissionsWarning) Valid() bool {
	switch w {
	case PermissionsWarningDefaultChannelExposesMembers, PermissionsWarningOpenSignupOn:
		return true
	default:
		return false
	}
}

// IssuedKind says what an IssuedByOperatorV1 is. Closed and client-tolerant.
type IssuedKind string

const (
	// IssuedCredential: a credential; its id is what the credential revoke
	// route takes.
	IssuedCredential IssuedKind = "credential"
	// IssuedHook: a hook; its id is what the hook revoke route takes.
	IssuedHook IssuedKind = "hook"
)

// Valid reports whether k is a known kind.
func (k IssuedKind) Valid() bool {
	switch k {
	case IssuedCredential, IssuedHook:
		return true
	default:
		return false
	}
}

// Error codes of the permission checks (ErrorV0.Code; design §7.4, §8.2).
// Existing codes stay wherever they already answer the same question
// (forbidden for a member on an operator route, voice_not_allowed,
// invite_not_allowed), and each then also carries Error.Permission.
const (
	// ErrorCodePermissionDenied (HTTP 403): the caller lacks the permission
	// named in Error.Permission, and no older code already says so.
	ErrorCodePermissionDenied = "permission_denied"
	// ErrorCodePermissionsStale (HTTP 409): a PUT /v1/permissions carried a
	// revision that is not the current one. The client re-reads GET
	// /v1/permissions before deciding again.
	ErrorCodePermissionsStale = "permissions_stale"
	// ErrorCodePermissionsConflict (HTTP 409): the change would break
	// precedence (members never exceed operators) or a dependency
	// (Permission.Requires); the message names the permission.
	ErrorCodePermissionsConflict = "permissions_conflict"
)

// MaxPermissionChanges is the most cells one PUT /v1/permissions may set.
const MaxPermissionChanges = 32

// PermissionRowV1 is one permission's cells in the permissions document.
// Fixed cells are reported (MemberFixed, OperatorFixed; omitted when false)
// so a client draws them locked without knowing the rules; a fixed cell's
// Member or Operator is its fixed value.
type PermissionRowV1 struct {
	Permission    Permission `json:"permission"`
	Member        bool       `json:"member"`
	Operator      bool       `json:"operator"`
	MemberFixed   bool       `json:"member_fixed,omitempty"`
	OperatorFixed bool       `json:"operator_fixed,omitempty"`
}

// PermissionPresetV1 is one preset as the server defines it: the member
// permissions it turns on (PresetMemberPermissions), in catalog order.
// Member is never null.
type PermissionPresetV1 struct {
	Preset PermissionPreset `json:"preset"`
	Member []Permission     `json:"member"`
}

// MarshalJSON writes Member as [] when nil, never null.
func (p PermissionPresetV1) MarshalJSON() ([]byte, error) {
	type wire PermissionPresetV1
	w := wire(p)
	if w.Member == nil {
		w.Member = []Permission{}
	}
	return json.Marshal(w)
}

// PermissionsResponseV1 is the body of GET /v1/permissions (any operator)
// and the answer to PUT /v1/permissions.
//
//   - Revision increases by one on every change that changes a cell; a
//     server that has never changed one answers 1.
//   - Rows lists every permission once, in catalog order.
//   - Preset is the preset the member column equals, omitted when it equals
//     none.
//   - Presets is every preset as the server defines it.
//   - Editable lists the columns the caller may change: both for an owner,
//     ["member"] for an operator holding change_server_settings, []
//     otherwise.
//   - Warnings are conditions to show, never refusals.
//   - UpdatedAt is when a cell last changed.
//
// Rows, Presets, Editable and Warnings are never null.
type PermissionsResponseV1 struct {
	Revision  int64                `json:"revision"`
	Rows      []PermissionRowV1    `json:"rows"`
	Preset    PermissionPreset     `json:"preset,omitempty"`
	Presets   []PermissionPresetV1 `json:"presets"`
	Editable  []PermissionRole     `json:"editable"`
	Warnings  []PermissionsWarning `json:"warnings"`
	UpdatedAt Timestamp            `json:"updated_at"`
	// IssuedToOthers is filled only in the answer to a PUT that turned
	// act_as_members off for operators: the live credentials and hooks that
	// operators issued to principals other than themselves, from the audit
	// trail (design §7.5, O13), so the owner can revoke them. Taking the
	// permission away does not end them. Omitted otherwise, and when there
	// are none.
	IssuedToOthers []IssuedByOperatorV1 `json:"issued_to_others,omitempty"`
}

// MarshalJSON writes Rows, Presets, Editable and Warnings as [] when nil,
// never null.
func (r PermissionsResponseV1) MarshalJSON() ([]byte, error) {
	type wire PermissionsResponseV1
	w := wire(r)
	if w.Rows == nil {
		w.Rows = []PermissionRowV1{}
	}
	if w.Presets == nil {
		w.Presets = []PermissionPresetV1{}
	}
	if w.Editable == nil {
		w.Editable = []PermissionRole{}
	}
	if w.Warnings == nil {
		w.Warnings = []PermissionsWarning{}
	}
	return json.Marshal(w)
}

// IssuedByOperatorV1 is one live credential or hook an operator issued to
// someone other than themself. ID is what the revoke route of its Kind
// takes; PrincipalID is whom it speaks as, and IssuedBy the operator that
// issued it.
type IssuedByOperatorV1 struct {
	Kind        IssuedKind `json:"kind"`
	ID          int64      `json:"id"`
	PrincipalID int64      `json:"principal_id"`
	IssuedBy    int64      `json:"issued_by"`
	IssuedAt    Timestamp  `json:"issued_at"`
}

// PermissionChangeV1 sets one cell: role's cell for permission, on or off.
type PermissionChangeV1 struct {
	Permission Permission     `json:"permission"`
	Role       PermissionRole `json:"role"`
	Granted    bool           `json:"granted"`
}

// PutPermissionsRequestV1 is the body of PUT /v1/permissions: an owner, or
// an operator holding change_server_settings for member cells only; recent
// authentication always.
//
// It is a patch. Changes sets the cells it names and leaves every other
// cell, so a client that does not know a later permission never clears it.
// Preset sets the member column to that preset (PresetMemberPermissions),
// expanded by the server into cell changes and checked as they would be.
// Exactly one of Changes and Preset is set. Revision is the revision the
// caller read; a stale one is 409 permissions_stale.
type PutPermissionsRequestV1 struct {
	Revision int64                `json:"revision"`
	Changes  []PermissionChangeV1 `json:"changes,omitempty"`
	Preset   PermissionPreset     `json:"preset,omitempty"`
}

// Validate reports whether the request is well-formed on its own: revision
// positive; exactly one of changes and preset; changes with 1 to
// MaxPermissionChanges entries, no (permission, role) pair twice; every
// value known; no member grant of an operator-only permission; no change to
// a fixed cell. Precedence, the dependencies and what the caller may write
// need the stored column and the caller, so they are the store's to check
// (design §4.4).
func (r PutPermissionsRequestV1) Validate() error {
	if r.Revision <= 0 {
		return errors.New("schema: revision must be positive")
	}
	hasChanges, hasPreset := r.Changes != nil, r.Preset != ""
	if hasChanges == hasPreset {
		return errors.New("schema: exactly one of changes and preset must be set")
	}
	if hasPreset {
		if !r.Preset.Valid() {
			return fmt.Errorf("schema: preset must be one of: %s", strings.Join(Vocabularies["PermissionPreset"], ", "))
		}
		return nil
	}
	if len(r.Changes) == 0 || len(r.Changes) > MaxPermissionChanges {
		return fmt.Errorf("schema: changes must have 1 to %d entries", MaxPermissionChanges)
	}
	type cell struct {
		p Permission
		r PermissionRole
	}
	seen := make(map[cell]bool, len(r.Changes))
	for i, c := range r.Changes {
		if !c.Permission.Valid() {
			return fmt.Errorf("schema: changes[%d].permission must be one of: %s", i, strings.Join(Vocabularies["Permission"], ", "))
		}
		if !c.Role.Valid() {
			return fmt.Errorf("schema: changes[%d].role must be one of: %s", i, strings.Join(Vocabularies["PermissionRole"], ", "))
		}
		k := cell{c.Permission, c.Role}
		if seen[k] {
			return fmt.Errorf("schema: changes[%d] sets %s for %s a second time", i, c.Permission, c.Role)
		}
		seen[k] = true
		if c.Role == PermissionRoleMember && c.Granted && c.Permission.OperatorOnly() {
			return fmt.Errorf("schema: changes[%d]: %s is operator-only and cannot be granted to members", i, c.Permission)
		}
		if fixed, _ := c.Permission.Fixed(c.Role); fixed {
			return fmt.Errorf("schema: changes[%d]: the %s cell of %s is fixed and cannot be changed", i, c.Role, c.Permission)
		}
	}
	return nil
}

// PermissionsChangedSchemaV1 is the events-socket document that says the
// receiver's effective permissions may have changed (design §8.6). It
// carries nothing else: the client re-reads GET /v1/whoami and, if it shows
// the matrix, GET /v1/permissions. Content-free, as
// conch.directory_changed.v1 is. It is sent to every human principal's
// sockets after a change to the document, and to the target alone after a
// change to one of their brakes.
const PermissionsChangedSchemaV1 = "conch.permissions_changed.v1"

// PermissionsChangedV1 is the conch.permissions_changed.v1 document.
type PermissionsChangedV1 struct {
	// Schema is the frame discriminator; always PermissionsChangedSchemaV1.
	Schema string `json:"schema"`
}

// Validate reports whether the document names its schema.
func (d PermissionsChangedV1) Validate() error {
	if d.Schema != PermissionsChangedSchemaV1 {
		return fmt.Errorf("schema: permissions changed schema must be %q, got %q", PermissionsChangedSchemaV1, d.Schema)
	}
	return nil
}
