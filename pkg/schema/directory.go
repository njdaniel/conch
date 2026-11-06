package schema

import (
	"errors"
	"fmt"
	"strings"
)

// The member directory (ADR-013 decision 1, issue #978,
// docs/design/community-model.md §3.5 and §13): one server is one community,
// and its members see each other in a server-wide list. These are additive
// REST bodies with no schema-name field, versioned by the V1 type suffix;
// DirectoryChangedV1 is the exception, an events document that says what it
// is.

// DirectoryScope says which list GET /v1/directory/members answered with
// (community-model §13.3), so a client can say "you see the people you share
// a channel with" instead of implying the list is the server. The vocabulary
// is closed.
type DirectoryScope string

const (
	// DirectoryScopeEveryone: the whole community. The answer of a viewer who
	// holds see_member_directory (ADR-014; on by default), and of every
	// operator.
	DirectoryScopeEveryone DirectoryScope = "everyone"
	// DirectoryScopeSharedChannels: only the people the viewer may know
	// (community-model §13.2): themself, the members of the channels they
	// are in, and the people their own links brought in. The answer of a
	// member who does not hold see_member_directory.
	DirectoryScopeSharedChannels DirectoryScope = "shared_channels"
)

// Valid reports whether s is a recognized directory scope.
func (s DirectoryScope) Valid() bool {
	switch s {
	case DirectoryScopeEveryone, DirectoryScopeSharedChannels:
		return true
	default:
		return false
	}
}

// ServerMemberV1 is one entry of the community's member list, GET
// /v1/directory/members: every member (community-model §1.1: enabled, not
// hidden, no active or ended guest visit) and every enabled, non-hidden
// agent, marked by Kind.
//
// Role is filled for every human and shown to every viewer (O4); an agent
// carries none. Owner, VoiceAllowed, MayInvite and InvitedBy are filled for
// a viewer holding manage_members (ADR-014; operators by default) and
// omitted for everyone else, so the controls that act on them sit on this
// list: a member sees id, kind, name, role and created_at and nothing more.
// Nothing says which channels anyone is in, who is in voice, who is disabled
// or hidden (they are not listed), or who invited whom (manage_members
// excepted).
type ServerMemberV1 struct {
	ID        int64         `json:"id"`
	Kind      PrincipalKind `json:"kind"`
	Name      string        `json:"name"`
	Role      PrincipalRole `json:"role,omitempty"`
	CreatedAt Timestamp     `json:"created_at"`
	// Owner is true for a server owner (ADR-010). Omitted when false, and
	// for a viewer without manage_members.
	Owner bool `json:"owner,omitempty"`
	// VoiceAllowed is the person's voice switch (ADR-013 decision 4; PUT
	// /v1/principals/{id}/voice-permission). Present for a viewer with
	// manage_members, on humans only.
	VoiceAllowed *bool `json:"voice_allowed,omitempty"`
	// MayInvite is the person's may_invite brake (ADR-013 decision 7: on by
	// default, pulled by an operator). Present for a viewer with
	// manage_members, on humans only.
	MayInvite *bool `json:"may_invite,omitempty"`
	// InvitedBy is the maker of the link the person joined through. Present
	// for a viewer with manage_members, when a link made the person.
	InvitedBy *int64 `json:"invited_by,omitempty"`
}

// ServerMembersResponseV1 is one page of the directory, ordered by id
// ascending. After is the id to pass as the next request's ?after=, present
// only when there may be more; it is always the id of a returned row, so a
// cursor never names a principal the viewer may not know. ?limit= defaults
// to DefaultListPrincipalsLimit and is capped at MaxListPrincipalsLimit. No
// total is carried. Scope says which list this is (community-model §13.3);
// a server that predates it omits it, which means everyone. Members is
// always a JSON array, never null: producers send an empty slice.
type ServerMembersResponseV1 struct {
	Members []ServerMemberV1 `json:"members"`
	After   int64            `json:"after,omitempty"`
	Scope   DirectoryScope   `json:"scope,omitempty"`
}

// Validate reports whether the page is structurally well-formed: ids are
// positive and distinct; names are present; kinds are known; a human's
// role, when present, is known; an agent carries no role and none of the
// manage_members fields; a scope, when present, is known; and a cursor,
// when present, is the id of a listed row.
func (d ServerMembersResponseV1) Validate() error {
	seen := make(map[int64]struct{}, len(d.Members))
	for i, m := range d.Members {
		if m.ID <= 0 {
			return fmt.Errorf("schema: members[%d] id must be positive, got %d", i, m.ID)
		}
		if _, dup := seen[m.ID]; dup {
			return fmt.Errorf("schema: members[%d] id %d is listed twice", i, m.ID)
		}
		seen[m.ID] = struct{}{}
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("schema: members[%d] name is required", i)
		}
		switch m.Kind {
		case PrincipalHuman:
			if m.Role != "" && !m.Role.Valid() {
				return fmt.Errorf("schema: members[%d] role must be one of: %s", i, strings.Join(Vocabularies["PrincipalRole"], ", "))
			}
		case PrincipalAgent:
			if m.Role != "" {
				return fmt.Errorf("schema: members[%d] is an agent and carries a role", i)
			}
			if m.Owner || m.VoiceAllowed != nil || m.MayInvite != nil || m.InvitedBy != nil {
				return fmt.Errorf("schema: members[%d] is an agent and carries a person's fields", i)
			}
		default:
			return fmt.Errorf("schema: members[%d] kind must be one of: %s", i, strings.Join(Vocabularies["PrincipalKind"], ", "))
		}
	}
	if d.Scope != "" && !d.Scope.Valid() {
		return fmt.Errorf("schema: scope must be one of: %s", strings.Join(Vocabularies["DirectoryScope"], ", "))
	}
	if d.After != 0 {
		if _, ok := seen[d.After]; !ok {
			return errors.New("schema: after must be the id of a listed member")
		}
	}
	return nil
}

// DirectoryChangedSchemaV1 is the versioned name identifying the v1
// directory signal on the events socket (community-model §13.6).
const DirectoryChangedSchemaV1 = "conch.directory_changed.v1"

// DirectoryChangedV1 tells a client that its answer to GET
// /v1/directory/members may have changed: a principal was created, renamed,
// disabled, enabled, hidden or had its role changed, a membership changed
// what the viewer may know, or the directory permission was switched. It
// carries nothing but its schema: no principal, no count, no reason, so
// nothing about a person can leak through it, and a race between the change
// and the frame cannot either. It goes only to the viewers whose answer
// changed, on their events sockets (GET /v1/events/ws), coalesced like the
// read-state frames.
//
// A client that receives it re-reads the directory and replaces, never
// merges, its people cache and autocomplete candidates, dropping every name
// the new list lacks and the authors route (ChannelAuthorsResponseV1) does
// not answer for that channel. It differs from conch.channels_changed.v1 in
// what it tells the client to re-read: that one the caller's own channels,
// this one the people. One change may send both, each to its own
// recipients. The whole-list document once sketched for the directory
// (conch.directory_members.v1) was never published.
type DirectoryChangedV1 struct {
	// Schema is the document discriminator; always DirectoryChangedSchemaV1.
	Schema string `json:"schema"`
}

// Validate reports whether the frame is a v1 directory signal.
func (d DirectoryChangedV1) Validate() error {
	if d.Schema != DirectoryChangedSchemaV1 {
		return fmt.Errorf("schema: directory changed schema must be %q, got %q", DirectoryChangedSchemaV1, d.Schema)
	}
	return nil
}
