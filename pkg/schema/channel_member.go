package schema

import (
	"errors"
	"fmt"
	"strings"
)

// Channel membership wire shapes (issue #90, ADR-003). A member is a
// principal on a channel's explicit member list. These shapes are additive and
// carry no schema-name field: they are REST bodies, versioned by the V1 type
// suffix. ChannelMembersV1 is the exception: it travels on the events socket,
// where every frame says what it is.

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
	// CanDecide is whether the member may decide the channel's approvals
	// (issue #450). The server fills it for human members; it is absent for
	// agents, which never decide. The store keeps it false for a guest
	// (ADR-009; security review F1).
	CanDecide *bool `json:"can_decide,omitempty"`
	// HistoryAfter is the membership's history bound (issue #704, ADR-009
	// §4.3): the id of the last message of the channel at admission, chosen
	// by the server. The member may read messages with a greater id and
	// nothing before, on every read surface: history, direct ids, search,
	// threads, quotes, files, replay, counts and notifications. Absent, or
	// zero, is no bound, which every membership that predates guests has. A
	// guest's memberships always carry one, and conversion keeps it.
	HistoryAfter int64 `json:"history_after,omitempty"`
	// Guest is how the member is rendered while it is a guest: present
	// exactly then, and never derived from the name's text.
	Guest *GuestAttributionV1 `json:"guest,omitempty"`
}

// ListChannelMembersResponseV1 is the body of GET /v1/channels/{channel}/members.
// Members is always a JSON array, ordered by principal_id.
type ListChannelMembersResponseV1 struct {
	Members []ChannelMemberV1 `json:"members"`
}

// ChannelMembersSchemaV1 is the versioned name identifying the v1 members
// document: a channel's whole member list, as the events socket sends it.
const ChannelMembersSchemaV1 = "conch.channel_members.v1"

// ChannelMembersV1 is a channel's member list as one reader may see it: the
// entries GET /v1/channels/{channel}/members answers that reader with. The
// events socket sends it when the list changes while the socket is open: a
// member added or removed, hidden or renamed (issue #515). Like the roster it
// is the whole state, not a delta, so a reader that misses one loses nothing.
// Members is always a JSON array, ordered by principal_id.
type ChannelMembersV1 struct {
	// Schema is the document discriminator; always ChannelMembersSchemaV1.
	Schema string `json:"schema"`
	// ChannelID references the channel the list describes, by id as the
	// roster names its channel.
	ChannelID int64 `json:"channel_id"`
	// Members lists every member of the channel.
	Members []ChannelMemberV1 `json:"members"`
}

// Validate reports whether the document is structurally well-formed: the v1
// schema name, a positive channel_id, a members list that is a list (empty is
// fine, null is not), and every member listed once, with a positive
// principal_id, a created_at, a name and a known kind. The server always fills
// the name and kind, so this document requires them where the older REST
// body does not. An agent never carries can_decide. Order is not checked.
func (d ChannelMembersV1) Validate() error {
	if d.Schema != ChannelMembersSchemaV1 {
		return fmt.Errorf("schema: channel members schema must be %q, got %q", ChannelMembersSchemaV1, d.Schema)
	}
	if d.ChannelID <= 0 {
		return fmt.Errorf("schema: channel members channel_id must be positive, got %d", d.ChannelID)
	}
	if d.Members == nil {
		return errors.New("schema: channel members members must be a list")
	}
	seen := make(map[int64]struct{}, len(d.Members))
	for _, m := range d.Members {
		if m.PrincipalID <= 0 {
			return fmt.Errorf("schema: channel member principal_id must be positive, got %d", m.PrincipalID)
		}
		if _, dup := seen[m.PrincipalID]; dup {
			return fmt.Errorf("schema: channel members lists principal %d more than once", m.PrincipalID)
		}
		seen[m.PrincipalID] = struct{}{}
		if m.CreatedAt.IsZero() {
			return fmt.Errorf("schema: channel member %d created_at is required", m.PrincipalID)
		}
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("schema: channel member %d name is required", m.PrincipalID)
		}
		if m.Kind != PrincipalHuman && m.Kind != PrincipalAgent {
			return fmt.Errorf("schema: channel member %d kind %q is not one of human, agent", m.PrincipalID, m.Kind)
		}
		if m.Kind == PrincipalAgent && m.CanDecide != nil {
			return fmt.Errorf("schema: channel member %d is an agent and must not carry can_decide", m.PrincipalID)
		}
		if m.HistoryAfter < 0 {
			return fmt.Errorf("schema: channel member %d history_after must not be negative", m.PrincipalID)
		}
		if m.Guest != nil {
			if m.Kind != PrincipalHuman {
				return fmt.Errorf("schema: channel member %d is not human and must not carry guest", m.PrincipalID)
			}
			if m.CanDecide != nil && *m.CanDecide {
				return fmt.Errorf("schema: channel member %d is a guest and cannot carry can_decide true", m.PrincipalID)
			}
			if err := m.Guest.Validate(); err != nil {
				return fmt.Errorf("schema: channel member %d: %w", m.PrincipalID, err)
			}
		}
	}
	return nil
}

// PutChannelMemberRequestV1 is the optional body of PUT
// /v1/channels/{channel}/members/{principal_id} (issue #450). With no body,
// or with CanDecide absent, a new member may decide the channel's approvals
// and an existing member keeps what it has. CanDecide sets it either way.
type PutChannelMemberRequestV1 struct {
	CanDecide *bool `json:"can_decide,omitempty"`
}
