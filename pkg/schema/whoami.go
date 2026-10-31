package schema

// PrincipalRole is a principal's administrative role (issue #89). Operators
// administer the instance; members use it. The role is deliberately not part of
// PrincipalV0, which is unchanged.
type PrincipalRole string

const (
	RoleOperator PrincipalRole = "operator"
	RoleMember   PrincipalRole = "member"
)

// Valid reports whether r is a recognized principal role.
func (r PrincipalRole) Valid() bool {
	switch r {
	case RoleOperator, RoleMember:
		return true
	default:
		return false
	}
}

// IdentityClass is what the caller is, as the routes of ADR-009 decide who
// may call them (issue #704): the self routes of a guest refuse a member,
// and the member routes a guest. Kind and Role still say what the
// principal is; the class adds the guest state, which they do not carry.
// The vocabulary is closed.
type IdentityClass string

const (
	// IdentityGuest: a human in a live guest visit.
	IdentityGuest IdentityClass = "guest"
	// IdentityMember: a human member with an account.
	IdentityMember IdentityClass = "member"
	// IdentityOperator: a human operator.
	IdentityOperator IdentityClass = "operator"
	// IdentityAgent: an agent.
	IdentityAgent IdentityClass = "agent"
)

// Valid reports whether c is a recognized identity class.
func (c IdentityClass) Valid() bool {
	switch c {
	case IdentityGuest, IdentityMember, IdentityOperator, IdentityAgent:
		return true
	default:
		return false
	}
}

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
	// From ADR-014 on it is the effective answer, whether the caller holds
	// make_member_links: for a member the role's cell AND their may_invite
	// brake, for an operator the operator cell alone (the brake does not
	// apply to operators).
	MayInvite bool `json:"may_invite,omitempty"`
	// VoiceAllowed says whether the caller may use voice, so a client offers
	// it only to those who hold it (issue #468). The server always sends it:
	// true unless the person's invite withheld voice, and false for an agent,
	// which never uses voice. It is a pointer so that a server that predates
	// it, which omits it, is told apart from one that says false. From
	// ADR-014 on it is the effective answer, whether the caller holds
	// use_voice: the role's cell AND their voice_allowed brake.
	VoiceAllowed *bool `json:"voice_allowed,omitempty"`
	// InvitesRemaining is how many more personal invites the caller may make
	// now: the server's per-inviter allowance less their open personal
	// invites, never below zero (issue #468). Present only for a person who
	// may invite and is not an operator, since operators have no allowance;
	// omitted for everyone else, never sent as 0 in their place.
	InvitesRemaining *int64 `json:"invites_remaining,omitempty"`
	// IdentityClass says what the caller is (issue #704, ADR-009): guest,
	// member, operator or agent. A server that predates guests omits it, and
	// a client then reads Kind and Role; a server with guests always sends
	// it. A server running with authentication off omits it too: there is
	// no identity to classify.
	IdentityClass IdentityClass `json:"identity_class,omitempty"`
	// Guest is the caller's visit while it is live: its handle, label and
	// tag, its origin, and when it ends. Present for a guest and no one
	// else. For a guest, Name is the handle.
	Guest *GuestV1 `json:"guest,omitempty"`
	// Auth is how the caller is authenticated: the credential's scope and
	// bounds, whether a password is set, whether setup is complete, and
	// whether a recent re-authentication stands. Present for every
	// credential once passwords exist (migration 32); omitted before, and
	// with authentication off.
	Auth *AuthStateV1 `json:"auth,omitempty"`
	// Owner is true when the caller is a server owner (ADR-010, issue
	// #697). Omitted when false, so every non-owner response is
	// byte-for-byte unchanged.
	Owner bool `json:"owner,omitempty"`
	// ServerMode is "temporary" when the conchd that answered runs in
	// temporary mode, the same value /healthz reports (ADR-011 item 2.3,
	// issue #865). Omitted in normal mode, and by a server that predates
	// the field, so every normal-mode response is byte-for-byte unchanged.
	ServerMode ServerMode `json:"server_mode,omitempty"`
	// Permissions is what the caller holds now (ADR-014,
	// docs/design/admin-roles.md §8.3): its role's cells with its brakes and
	// the dependencies applied; every permission for an owner; none for an
	// agent. Catalog order, omitted when empty. A client keys its controls
	// on it and shows a value it does not know by its name; the server
	// decides every request regardless.
	Permissions []Permission `json:"permissions,omitempty"`
	// PermissionsRevision is the revision of the permissions document the
	// answer was computed from. A server that predates ADR-014 omits it and
	// Permissions; a server with it always sends it (it is at least 1), so
	// a revision with no Permissions means the caller holds none.
	PermissionsRevision int64 `json:"permissions_revision,omitempty"`
}

// Validate reports whether r is a well-formed whoami response: server_mode,
// when present, is a recognized mode.
func (r WhoAmIResponseV1) Validate() error {
	return validateServerMode(r.ServerMode)
}
