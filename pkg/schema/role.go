package schema

import (
	"encoding/json"
	"errors"
)

// Role and owner-mark administration wire shapes (issue #787, ADR-010,
// docs/design/delegated-administration.md §7.4). These shapes are additive REST
// bodies with no schema-name field, versioned by the V1 type suffix.

// RoleChangeRequestV1 is the body of PUT /v1/principals/{id}/role
// (ADR-010, issue #697): owner-only, recent authentication required.
// Role is required ("operator" or "member"). Owner, when present, sets
// the owner mark; when absent, an existing operator keeps their owner
// mark and a promoted member becomes a non-owner operator. Demoting to
// "member" always clears the owner mark; sending Owner true with Role
// "member" is refused.
type RoleChangeRequestV1 struct {
	Role  PrincipalRole `json:"role"`
	Owner *bool         `json:"owner,omitempty"`
}

// Validate reports whether the request names a known role and does not
// ask for Owner true with Role "member".
func (r RoleChangeRequestV1) Validate() error {
	if r.Role != RoleOperator && r.Role != RoleMember {
		return errors.New(`schema: role must be "operator" or "member"`)
	}
	if r.Role == RoleMember && r.Owner != nil && *r.Owner {
		return errors.New("schema: a member cannot be an owner")
	}
	return nil
}

// RoleChangeResponseV1 is the response to PUT /v1/principals/{id}/role:
// the principal as it now is, and what the change did or left in place,
// so the owner sees the blast radius of a promotion or demotion in one
// response.
type RoleChangeResponseV1 struct {
	Principal PrincipalAdminV1 `json:"principal"`
	// Manages lists the channel ids where the principal has manages = 1,
	// ordered ascending; always a JSON array, never null.
	Manages []int64 `json:"manages"`
	// InvitesClosed is how many open invites created by this principal the
	// demotion closed (0 on promotion or when none were open).
	InvitesClosed int `json:"invites_closed"`
	// CredentialsRevoked and HooksRevoked are how many of the target's
	// credentials and hooks the change revoked (non-zero when a Role or
	// Owner bit changed; 0 on an idempotent no-op).
	CredentialsRevoked int `json:"credentials_revoked"`
	HooksRevoked       int `json:"hooks_revoked"`
	// LiveCredentials and HooksBound are how many live credentials and
	// non-revoked hooks remain bound to this principal after the change.
	LiveCredentials int `json:"live_credentials"`
	HooksBound      int `json:"hooks_bound"`
	// IssuedToOthers is filled when the change demoted an operator to
	// member and it had issued live credentials or hooks to principals other
	// than itself (issue #1024, ADR-014 O13): those stay live, since they
	// belong to their targets, and the owner may revoke them, one by one or
	// all at once through POST /v1/principals/{id}/issued/revoke-all.
	// Omitted on a promotion, on an owner-mark change, on a no-op and when
	// there are none.
	IssuedToOthers []IssuedByOperatorV1 `json:"issued_to_others,omitempty"`
}

// MarshalJSON writes Manages as [] when nil so the wire form is always an
// array, never null.
func (r RoleChangeResponseV1) MarshalJSON() ([]byte, error) {
	type wire RoleChangeResponseV1
	w := wire(r)
	if w.Manages == nil {
		w.Manages = []int64{}
	}
	return json.Marshal(w)
}
