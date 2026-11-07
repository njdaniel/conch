package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Preview as member (issue #1007, docs/design/permission-preview.md): an
// administrator reads, under their own credential, what a named member sees,
// by sending Conch-Preview-As on a short allowlist of GET routes. The server
// answers those routes as the member would be answered, writes nothing, and
// makes no credential or session for the member. The one new shape is the
// entry read's answer, PreviewResponseV1; the one new refusal is the header
// on a route that is not previewable. Both are additive (design §6); the
// REST body carries no schema-name field and is versioned by its V1 suffix.

// PreviewResponseV1 answers GET /v1/preview (issue #1007,
// docs/design/permission-preview.md §4.5): the standing and effective
// permissions of the member an administrator is previewing, computed by the
// server. The fields named as in WhoAmIResponseV1 mean the same there.
//
// whoami's account fields (auth, guest, identity_class, invites_remaining,
// server_version, server_mode) are absent on purpose and never added: they
// are the target's private account state, which a preview must not reveal,
// and the server's own facts, which the administrator's whoami already
// carries. Unlike whoami, nothing here is omitted when false or empty, so a
// client reads each answer rather than inferring it from absence.
type PreviewResponseV1 struct {
	// ID, Kind, Name and Role are the target's. Under a role simulation
	// (Simulation set) there is no target: ID is 0, Name is empty and Role
	// is the simulated role.
	ID   int64         `json:"id"`
	Kind PrincipalKind `json:"kind"`
	Name string        `json:"name"`
	Role PrincipalRole `json:"role"`
	// Owner is the target's owner mark. Always sent, false included.
	Owner bool `json:"owner"`
	// Permissions is what the target holds now, in catalog order, with the
	// target's brakes applied (EffectivePermissions). Never nil: a target
	// holding nothing has []. Client-tolerant, as WhoAmIResponseV1's is: a
	// client shows a value it does not know by its name.
	Permissions []Permission `json:"permissions"`
	// PermissionsRevision is the revision of the permissions document the
	// answer was computed from.
	PermissionsRevision int64 `json:"permissions_revision"`
	// MayInvite and VoiceAllowed are the effective answers whoami would give
	// the target: the role's cell AND the target's brake. Always sent, false
	// included.
	MayInvite    bool `json:"may_invite"`
	VoiceAllowed bool `json:"voice_allowed"`
	// Simulation is set when the answer is nobody's view (design §8, issue
	// #1519): a role simulation computed for a synthetic principal of Role
	// with the role's cells, no brakes and no memberships beyond the default
	// channels. Absent for a person's preview. A client never labels an
	// answer carrying it with a person's name.
	Simulation PreviewSimulation `json:"simulation,omitempty"`
}

// MarshalJSON writes Permissions as [] when nil, never null.
func (r PreviewResponseV1) MarshalJSON() ([]byte, error) {
	type wire PreviewResponseV1
	w := wire(r)
	if w.Permissions == nil {
		w.Permissions = []Permission{}
	}
	return json.Marshal(w)
}

// Validate reports whether r is a well-formed preview answer: a known kind
// and role, and simulation absent or a recognized value. Permissions are not
// checked: the vocabulary is client-tolerant.
func (r PreviewResponseV1) Validate() error {
	if r.Kind != PrincipalHuman && r.Kind != PrincipalAgent {
		return fmt.Errorf("schema: preview kind must be %q or %q, got %q", PrincipalHuman, PrincipalAgent, r.Kind)
	}
	if !r.Role.Valid() {
		return fmt.Errorf("schema: preview role must be one of: %s", strings.Join(Vocabularies["PrincipalRole"], ", "))
	}
	if r.Simulation != "" && !r.Simulation.Valid() {
		return fmt.Errorf("schema: preview simulation must be absent or one of: %s", strings.Join(Vocabularies["PreviewSimulation"], ", "))
	}
	return nil
}

// PreviewSimulation says what kind of nobody a preview answer is the view of
// (design §8). Closed: a client switches on it to pick its banner, and a
// value it does not know it cannot label honestly, so it refuses the
// document rather than show it as a person's view.
type PreviewSimulation string

const (
	// PreviewSimulationRole: the view of a brand-new principal of the
	// answer's Role, with the role's cells, no brakes, no memberships beyond
	// the default channels and no joiners (Conch-Preview-Role).
	PreviewSimulationRole PreviewSimulation = "role"
)

// Valid reports whether s is a recognized simulation.
func (s PreviewSimulation) Valid() bool {
	switch s {
	case PreviewSimulationRole:
		return true
	default:
		return false
	}
}

// ErrorCodePreviewReadOnly (HTTP 403, Error.Code): the request carried a
// preview header on a route that is not previewable (design §4.3). The
// message is "this route cannot be read in preview". It is answered before
// the target is resolved, so it reveals nothing about the target, and it is
// the answer for every write, every socket and every content read, so no
// handler ever runs with a previewed principal. The other preview refusals
// reuse existing codes: forbidden, permission_denied (with Error.Permission),
// principal_not_found and invalid_request. Error codes are not a closed
// vocabulary; this is a preview code, not part of the permission catalog
// that permission.go holds.
const ErrorCodePreviewReadOnly = "preview_read_only"
