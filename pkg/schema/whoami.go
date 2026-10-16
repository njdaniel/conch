package schema

// PrincipalRole is a principal's administrative role (issue #89). Operators
// administer the instance; members use it. The role is deliberately not part of
// PrincipalV0, which is unchanged.
type PrincipalRole string

const (
	RoleOperator PrincipalRole = "operator"
	RoleMember   PrincipalRole = "member"
)

// WhoAmIResponseV1 is the body of GET /v1/whoami: the principal the request's
// bearer credential resolves to. It is additive and carries no schema-name
// field; it is versioned by its V1 suffix.
type WhoAmIResponseV1 struct {
	ID   int64         `json:"id"`
	Kind PrincipalKind `json:"kind"`
	Name string        `json:"name"`
	Role PrincipalRole `json:"role"`
	// ServerVersion is the build version of the conchd that answered, the same
	// value /healthz reports (issue #425). Added after the first release of
	// this shape, so it is optional: a server that predates it omits it.
	ServerVersion string `json:"server_version,omitempty"`
	// MayInvite says whether the caller may make invites, so a client knows
	// whether to offer it (issue #436). True for an operator and for a member
	// granted the right; omitted when false, and by a server that predates it.
	MayInvite bool `json:"may_invite,omitempty"`
}
