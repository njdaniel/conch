package schema

// Channel membership wire shapes (issue #90, ADR-003). A member is a
// principal on a channel's explicit member list. These shapes are additive and
// carry no schema-name field: they are REST bodies, versioned by the V1 type
// suffix.

// ChannelMemberV1 is one entry of a channel's member list. AddedBy is the
// principal who added the member; it is omitted when unknown (memberships
// created by the upgrade migration, or added with authentication off).
//
// Name and Kind say what the member is called and whether it is a person or an
// agent. They are optional on the wire so that older fixtures and clients are
// unaffected; the server always fills them. Nothing about the member's role or
// whether it is disabled is carried here, on purpose.
type ChannelMemberV1 struct {
	PrincipalID int64         `json:"principal_id"`
	AddedBy     int64         `json:"added_by,omitempty"`
	CreatedAt   Timestamp     `json:"created_at"`
	Name        string        `json:"name,omitempty"`
	Kind        PrincipalKind `json:"kind,omitempty"`
}

// ListChannelMembersResponseV1 is the body of GET /v1/channels/{channel}/members.
// Members is always a JSON array, ordered by principal_id.
type ListChannelMembersResponseV1 struct {
	Members []ChannelMemberV1 `json:"members"`
}
