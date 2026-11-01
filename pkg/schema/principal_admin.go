package schema

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Operator administration of principals (issue #445): the list of every
// principal, and hiding one or many. Deleting an account in the private alpha
// means disabling and hiding it: a hidden principal is disabled, belongs to no
// channel or net, and has given up its name for HiddenPrincipalNamePrefix and
// its id, so the name can be taken again. Its messages and audit rows stay.

// HiddenPrincipalNamePrefix starts the name a hidden principal is given:
// "hidden-12" for principal 12. Names whose key begins with this prefix's key
// are reserved: no create, invite redemption or rename may take one, so a
// hide can always free a name.
const HiddenPrincipalNamePrefix = "hidden-"

// Bounds of the principal administration routes.
const (
	// MaxHidePrincipals is the most principals one bulk hide acts on. Past
	// it, the answer says how many more match and the operator repeats it.
	MaxHidePrincipals = 500
	// MinHideNamePrefixLength is the shortest name prefix, in characters, a
	// bulk hide accepts, so one stray letter cannot select half the server.
	MinHideNamePrefixLength = 3
	// MaxListPrincipalsLimit is the largest page of the principal list.
	MaxListPrincipalsLimit = 200
	// DefaultListPrincipalsLimit is the page size when none is asked for.
	DefaultListPrincipalsLimit = 100
)

// PrincipalState is where a principal is, as the operator's list filters it.
// The vocabulary is closed.
type PrincipalState string

const (
	// PrincipalStateActive is enabled: it may sign in.
	PrincipalStateActive PrincipalState = "active"
	// PrincipalStateDisabled is switched off but not hidden: it keeps its
	// name and its memberships.
	PrincipalStateDisabled PrincipalState = "disabled"
	// PrincipalStateHidden is disabled and hidden.
	PrincipalStateHidden PrincipalState = "hidden"
)

// Valid reports whether s is a recognized principal state.
func (s PrincipalState) Valid() bool {
	switch s {
	case PrincipalStateActive, PrincipalStateDisabled, PrincipalStateHidden:
		return true
	default:
		return false
	}
}

// PrincipalAdminV1 is one principal as an operator sees it. DisabledAt and
// HiddenAt are absent while the principal is enabled, or not hidden.
// InvitedBy and InviteID are absent for a principal no invite made.
// LiveCredentials counts the credentials that would still authenticate:
// neither revoked nor expired. GuestState is present for a principal that
// is or was a guest (issue #704): active while its visit is live, ended or
// converted after; absent for one that never was. The guest list, GET
// /v1/guests, has the visit itself.
type PrincipalAdminV1 struct {
	ID   int64         `json:"id"`
	Kind PrincipalKind `json:"kind"`
	Name string        `json:"name"`
	Role PrincipalRole `json:"role"`
	// Owner is true for a server owner (ADR-010, issue #697). Omitted
	// when false.
	Owner           bool       `json:"owner,omitempty"`
	CreatedAt       Timestamp  `json:"created_at"`
	DisabledAt      *Timestamp `json:"disabled_at,omitempty"`
	HiddenAt        *Timestamp `json:"hidden_at,omitempty"`
	InvitedBy       *int64     `json:"invited_by,omitempty"`
	InviteID        *int64     `json:"invite_id,omitempty"`
	LiveCredentials int        `json:"live_credentials"`
	GuestState      GuestState `json:"guest_state,omitempty"`
	// VoiceAllowed is the person's voice switch and MayInvite their
	// may_invite brake (ADR-013 decisions 4 and 7, issue #978). Both are
	// filled for humans by a server with the community model and omitted
	// by one that predates it, and for agents, which hold neither.
	VoiceAllowed *bool `json:"voice_allowed,omitempty"`
	MayInvite    *bool `json:"may_invite,omitempty"`
}

// ListPrincipalsResponseV1 is one page of the principal list, newest first.
// NextBefore is the id to pass as the next request's before parameter; it is
// present only when there may be more. Producers must send an empty slice,
// not nil.
type ListPrincipalsResponseV1 struct {
	Principals []PrincipalAdminV1 `json:"principals"`
	NextBefore int64              `json:"next_before,omitempty"`
}

// HidePrincipalResponseV1 is the answer to hiding one principal: the
// principal as it now is, under its new name.
type HidePrincipalResponseV1 struct {
	Principal PrincipalAdminV1 `json:"principal"`
}

// HidePrincipalsRequestV1 asks to hide every principal that matches: those
// that joined through invite InviteID, and those whose name begins with
// NamePrefix (exactly as typed, case included). Kind narrows either to humans
// or agents. At least one of InviteID and NamePrefix is required; when both
// are given, a principal must match both. DryRun is required: true only
// lists them, false hides them. A request that leaves it out is refused
// rather than read as false, so a bulk hide is never done by omission. (The
// tag says omitempty only so that a nil pointer is left out rather than sent
// as null; Validate is what requires it.)
type HidePrincipalsRequestV1 struct {
	InviteID   int64         `json:"invite_id,omitempty"`
	NamePrefix string        `json:"name_prefix,omitempty"`
	Kind       PrincipalKind `json:"kind,omitempty"`
	DryRun     *bool         `json:"dry_run,omitempty"`
}

// Validate reports whether the request says whether it is a dry run and
// names a selection the server accepts: an invite id or a name prefix of at least MinHideNamePrefixLength
// characters, and a known kind when one is given. Kind alone selects
// nothing: it would be everyone of that kind.
func (r HidePrincipalsRequestV1) Validate() error {
	if r.DryRun == nil {
		return errors.New("schema: dry_run is required: true to list whom it would hide, false to hide them")
	}
	if r.InviteID < 0 {
		return errors.New("schema: invite_id must be a positive integer")
	}
	if r.InviteID == 0 && r.NamePrefix == "" {
		return errors.New("schema: give invite_id or name_prefix")
	}
	if r.NamePrefix != "" {
		if !utf8.ValidString(r.NamePrefix) {
			return errors.New("schema: name_prefix must be valid UTF-8")
		}
		if n := utf8.RuneCountInString(r.NamePrefix); n < MinHideNamePrefixLength || n > MaxPrincipalNameLength {
			return fmt.Errorf("schema: name_prefix must be %d to %d characters", MinHideNamePrefixLength, MaxPrincipalNameLength)
		}
		if strings.TrimSpace(r.NamePrefix) == "" {
			return errors.New("schema: name_prefix must not be blank")
		}
	}
	if r.Kind != "" && r.Kind != PrincipalHuman && r.Kind != PrincipalAgent {
		return errors.New(`schema: kind must be "human" or "agent"`)
	}
	return nil
}

// HiddenPrincipalV1 is one principal a bulk hide selected. Name is the name
// it had when it was selected, before the hide replaced it.
type HiddenPrincipalV1 struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// HidePrincipalsResponseV1 is the answer of a bulk hide: how many principals
// were hidden and which, or with DryRun how many and which would be. At most
// MaxHidePrincipals are acted on; Remaining is how many more matched, for the
// operator to repeat the call. Producers must send an empty slice, not nil.
type HidePrincipalsResponseV1 struct {
	DryRun     bool                `json:"dry_run"`
	Count      int                 `json:"count"`
	Principals []HiddenPrincipalV1 `json:"principals"`
	Remaining  int                 `json:"remaining"`
}
