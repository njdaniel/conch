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
	// VoiceAllowed says whether the caller may use voice, so a client offers
	// it only to those who hold it (issue #468). The server always sends it:
	// true unless the person's invite withheld voice, and false for an agent,
	// which never uses voice. It is a pointer so that a server that predates
	// it, which omits it, is told apart from one that says false.
	VoiceAllowed *bool `json:"voice_allowed,omitempty"`
	// InvitesRemaining is how many more personal invites the caller may make
	// now: the server's per-inviter allowance less their open personal
	// invites, never below zero (issue #468). Present only for a person who
	// may invite and is not an operator, since operators have no allowance;
	// omitted for everyone else, never sent as 0 in their place.
	InvitesRemaining *int64 `json:"invites_remaining,omitempty"`
}
