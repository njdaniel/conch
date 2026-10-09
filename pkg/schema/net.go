package schema

import (
	"errors"
	"fmt"
)

// Net wire shapes (issue #114, ADR-005). A net is a named, persistent subset
// of a channel's members. Nets are flat and belong to exactly one channel;
// layering comes from overlapping membership. Like the channel-member shapes
// these are REST bodies with no schema-name field, versioned by the V1 type
// suffix.

// MaxNetNameLength is the longest net name accepted, in bytes (net names are
// ASCII, so bytes and characters agree).
const MaxNetNameLength = 32

// NetRole is a principal's role on a net. The vocabulary is closed.
type NetRole string

// Net roles.
const (
	// NetRoleMember listens on the net and may transmit on it.
	NetRoleMember NetRole = "member"
	// NetRoleMonitor listens on the net only.
	NetRoleMonitor NetRole = "monitor"
)

// Valid reports whether r is a recognized net role.
func (r NetRole) Valid() bool {
	switch r {
	case NetRoleMember, NetRoleMonitor:
		return true
	default:
		return false
	}
}

// ValidateNetName reports whether name is an acceptable net name: 1 to
// MaxNetNameLength characters from a-z, 0-9, "-" and "_", starting with a
// letter or digit. Net names appear in URL paths, CLI flags and the TUI
// prompt, so nothing else is accepted. This is the one rule for net names;
// the server and clients call it rather than restating it. Channel names are
// not governed by it.
func ValidateNetName(name string) error {
	if name == "" {
		return errors.New("schema: net name is required")
	}
	if len(name) > MaxNetNameLength {
		return fmt.Errorf("schema: net name %q is longer than %d characters", name, MaxNetNameLength)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case c == '-' || c == '_':
			if i == 0 {
				return fmt.Errorf("schema: net name %q must start with a letter or digit", name)
			}
			continue
		default:
			return fmt.Errorf("schema: net name %q may contain only a-z, 0-9, - and _", name)
		}
	}
	return nil
}

// NetMember is one entry of a net's member list: a principal and its role.
type NetMember struct {
	// PrincipalID references the principal on the net.
	PrincipalID int64 `json:"principal_id"`
	// Role is member (listens and transmits) or monitor (listens only).
	Role NetRole `json:"role"`
}

// Validate reports whether the entry is structurally well-formed.
func (m NetMember) Validate() error {
	if m.PrincipalID <= 0 {
		return fmt.Errorf("schema: net member principal_id must be positive, got %d", m.PrincipalID)
	}
	if !m.Role.Valid() {
		return fmt.Errorf("schema: net role %q is not one of member, monitor", m.Role)
	}
	return nil
}

// validateNetMembers checks a member list: every entry well-formed and no
// principal listed twice. Order is not checked; the server emits members
// ordered by principal_id.
func validateNetMembers(members []NetMember) error {
	seen := make(map[int64]struct{}, len(members))
	for _, m := range members {
		if err := m.Validate(); err != nil {
			return err
		}
		if _, dup := seen[m.PrincipalID]; dup {
			return fmt.Errorf("schema: net lists principal %d more than once", m.PrincipalID)
		}
		seen[m.PrincipalID] = struct{}{}
	}
	return nil
}

// NetV1 is the v1 wire representation of a net. Members is always a JSON
// array, ordered by principal_id; a net with nobody on it is an empty array.
type NetV1 struct {
	// ID is the server-assigned net id, referenced by Audience.NetID.
	ID int64 `json:"id"`
	// ChannelID references the one channel the net belongs to.
	ChannelID int64 `json:"channel_id"`
	// Name is the net's name, unique within its channel; see ValidateNetName.
	Name string `json:"name"`
	// Members lists every principal on the net with its role.
	Members []NetMember `json:"members"`
	// CreatedAt is when the server created the net (UTC, ms precision).
	CreatedAt Timestamp `json:"created_at"`
}

// Validate reports whether the net is structurally well-formed. It cannot
// check that the channel or the principals exist; those are the store's job.
func (n NetV1) Validate() error {
	if n.ID <= 0 {
		return fmt.Errorf("schema: net id must be positive, got %d", n.ID)
	}
	if n.ChannelID <= 0 {
		return fmt.Errorf("schema: net channel_id must be positive, got %d", n.ChannelID)
	}
	if err := ValidateNetName(n.Name); err != nil {
		return err
	}
	if err := validateNetMembers(n.Members); err != nil {
		return err
	}
	if n.CreatedAt.IsZero() {
		return errors.New("schema: net created_at is required")
	}
	return nil
}

// CreateNetRequestV1 is the request body for creating a net in a channel
// (POST /v1/channels/{channel}/nets). The channel comes from the URL and the
// id and timestamp from the server. A net is created with nobody on it and
// populated one principal at a time with PutNetMemberRequestV1, so that every
// membership change is its own audited request.
type CreateNetRequestV1 struct {
	// Name is the net's name; see ValidateNetName.
	Name string `json:"name"`
}

// Validate reports whether the request is structurally well-formed.
func (r CreateNetRequestV1) Validate() error {
	return ValidateNetName(r.Name)
}

// CreateNetResponseV1 is the response body after a net is created. It embeds
// the full NetV1 as stored.
type CreateNetResponseV1 struct {
	Net NetV1 `json:"net"`
}

// ListNetsResponseV1 is the body of GET /v1/channels/{channel}/nets. Nets is
// always a JSON array, ordered by id.
type ListNetsResponseV1 struct {
	Nets []NetV1 `json:"nets"`
}

// PutNetMemberRequestV1 is the request body for adding a principal to a net or
// changing its role (PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}).
// The principal comes from the URL, as it does for channel members; the body
// carries only the role.
type PutNetMemberRequestV1 struct {
	// Role is member or monitor.
	Role NetRole `json:"role"`
}

// Validate reports whether the request is structurally well-formed.
func (r PutNetMemberRequestV1) Validate() error {
	if !r.Role.Valid() {
		return fmt.Errorf("schema: net role %q is not one of member, monitor", r.Role)
	}
	return nil
}
