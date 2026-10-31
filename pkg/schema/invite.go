package schema

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Invite wire shapes (issue #436, ADR-008 §4, docs/design/private-alpha.md
// §3). An invite is a second way to get a credential, beside an operator
// issuing one: whoever holds its code may join as a new human principal with
// the channels and grants the invite names. The code is shown exactly once, in
// the create response; no other shape carries it or its hash. These shapes are
// additive and carry no schema-name field: they are REST bodies, versioned by
// the V1 type suffix.

// InviteKind says who may use an invite and how often (private-alpha.md §3).
// The vocabulary is closed.
type InviteKind string

const (
	// InviteKindPersonal is single-use and may be made by any person allowed
	// to invite.
	InviteKindPersonal InviteKind = "personal"
	// InviteKindOpen is made by operators only: anyone with the link may join
	// until it expires, closes or (when capped) is used up.
	InviteKindOpen InviteKind = "open"
)

// Valid reports whether k is a recognized invite kind.
func (k InviteKind) Valid() bool {
	switch k {
	case InviteKindPersonal, InviteKindOpen:
		return true
	default:
		return false
	}
}

// InviteJoinMode says what an invite admits (ADR-009 D1, design §2): an
// account, a temporary guest visit, or either. An invite with no mode on
// the wire predates modes and admits accounts only. A guest mode takes
// effect only while the server's guests_enabled switch is on; the invite
// cannot override server policy. The vocabulary is closed.
type InviteJoinMode string

const (
	// InviteJoinAccount: Create an account only. The default.
	InviteJoinAccount InviteJoinMode = "account"
	// InviteJoinGuest: Join as guest only.
	InviteJoinGuest InviteJoinMode = "guest"
	// InviteJoinBoth: either.
	InviteJoinBoth InviteJoinMode = "both"
)

// Valid reports whether m is a recognized join mode.
func (m InviteJoinMode) Valid() bool {
	switch m {
	case InviteJoinAccount, InviteJoinGuest, InviteJoinBoth:
		return true
	default:
		return false
	}
}

const (
	// InviteCodePrefix starts every invite code.
	InviteCodePrefix = "conch_inv_"
	// InviteCodeRandomBytes is the number of crypto/rand bytes in a code.
	InviteCodeRandomBytes = 32
	// InviteCodeLength is the exact length of a code: the prefix plus 32
	// random bytes in unpadded base64url (43 characters).
	InviteCodeLength = len(InviteCodePrefix) + 43

	// MaxInviteChannels is the most channels one invite may name.
	MaxInviteChannels = 32

	// DefaultPersonalInviteLifetime and DefaultOpenInviteLifetime apply when a
	// create request names no expiry.
	DefaultPersonalInviteLifetime = 7 * 24 * time.Hour
	DefaultOpenInviteLifetime     = 24 * time.Hour
	// MaxPersonalInviteLifetime and MaxOpenInviteLifetime bound how far ahead
	// an expiry may be set.
	MaxPersonalInviteLifetime = 30 * 24 * time.Hour
	MaxOpenInviteLifetime     = 7 * 24 * time.Hour

	// DefaultInviteCredentialLifetimeSeconds is how long the joiner's first
	// credential lives when the request names no lifetime: 90 days.
	DefaultInviteCredentialLifetimeSeconds = 90 * 24 * 60 * 60
	// MaxInviteCredentialLifetimeSeconds bounds a requested lifetime: 365 days.
	MaxInviteCredentialLifetimeSeconds = 365 * 24 * 60 * 60

	// MaxInviteMaxUses bounds the use cap of an open link.
	MaxInviteMaxUses = 1_000_000
)

// Error codes of the invite routes (ErrorV0.Code).
const (
	// ErrorCodeInviteInvalid is the one answer for a code that is unknown,
	// expired, used up, closed, or whose inviter is disabled. The cases are
	// deliberately indistinguishable, so the answer is not an oracle.
	ErrorCodeInviteInvalid = "invite_invalid"
	// ErrorCodeInvitesPaused: an operator has paused invites.
	ErrorCodeInvitesPaused = "invites_paused"
	// ErrorCodeInviteAllowanceExceeded: the inviter already has as many
	// outstanding personal invites as the allowance permits.
	ErrorCodeInviteAllowanceExceeded = "invite_allowance_exceeded"
	// ErrorCodeInviteGrantExceedsInviter: the invite names a channel or voice
	// its maker does not hold, or asks for the right to invite from a maker who
	// is not an operator (only operators grant it).
	ErrorCodeInviteGrantExceedsInviter = "invite_grant_exceeds_inviter"
	// ErrorCodeInviteNotAllowed: the caller may not make this kind of invite.
	ErrorCodeInviteNotAllowed = "invite_not_allowed"
	// ErrorCodeInviteNotFound: no invite the caller may close has that id
	// (HTTP 404). Someone else's invite gets the same answer as one that does
	// not exist, so an id reveals nothing (issue #437).
	ErrorCodeInviteNotFound = "invite_not_found"
	// ErrorCodePrincipalExists: the name asked for is already taken (HTTP
	// 409). Redeeming an invite answers with it and does not use the invite
	// up; it is the code POST /v0/principals has always answered with.
	ErrorCodePrincipalExists = "principal_exists"
)

// InviteChannelV1 names one channel an invite adds its joiner to.
type InviteChannelV1 struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// InviteV1 describes one invite. It never carries the code or its hash.
// MaxUses is omitted when uncapped and is always 1 for a personal invite;
// ClosedAt is omitted while the invite is open. JoinAs is what the invite
// admits (issue #704); a server that predates it omits it, which means
// account, and a server with it always sends it.
type InviteV1 struct {
	ID                        int64             `json:"id"`
	Kind                      InviteKind        `json:"kind"`
	CreatedBy                 int64             `json:"created_by"`
	CreatedAt                 Timestamp         `json:"created_at"`
	ExpiresAt                 Timestamp         `json:"expires_at"`
	MaxUses                   *int64            `json:"max_uses,omitempty"`
	Uses                      int64             `json:"uses"`
	ClosedAt                  *Timestamp        `json:"closed_at,omitempty"`
	Channels                  []InviteChannelV1 `json:"channels"`
	Voice                     bool              `json:"voice"`
	MayInvite                 bool              `json:"may_invite"`
	CredentialLifetimeSeconds int64             `json:"credential_lifetime_seconds"`
	JoinAs                    InviteJoinMode    `json:"join_as,omitempty"`
	// OtherChannels is how many channels the invite lands its joiner in
	// beyond those Channels lists: the ones the reader is not in, left out
	// because the reader does not hold manage_channels (ADR-014,
	// docs/design/admin-roles.md §7.3), so the invite does not seem to grant
	// less than it does. Omitted when 0, and by a server that predates it.
	OtherChannels int64 `json:"other_channels,omitempty"`
}

// CreateInviteRequestV1 is the body of the route that makes an invite.
// Channels are names. ExpiresAt, MayInvite, CredentialLifetimeSeconds and
// JoinAs are optional: the server applies EffectiveExpiry,
// EffectiveMayInvite, EffectiveCredentialLifetime and EffectiveJoinAs.
// MaxUses is for open invites only and is absent by default (no cap).
type CreateInviteRequestV1 struct {
	Kind                      InviteKind     `json:"kind"`
	Channels                  []string       `json:"channels"`
	ExpiresAt                 *Timestamp     `json:"expires_at,omitempty"`
	MaxUses                   *int64         `json:"max_uses,omitempty"`
	Voice                     bool           `json:"voice"`
	MayInvite                 *bool          `json:"may_invite,omitempty"`
	CredentialLifetimeSeconds *int64         `json:"credential_lifetime_seconds,omitempty"`
	JoinAs                    InviteJoinMode `json:"join_as,omitempty"`
}

// EffectiveJoinAs is what the invite admits: the requested mode, or
// account. Existing invites, and new ones that say nothing, admit accounts
// only (ADR-009 D1). A guest visit through an invite grants no more than
// the invite's maker holds, as an account does.
func (r CreateInviteRequestV1) EffectiveJoinAs() InviteJoinMode {
	if r.JoinAs == "" {
		return InviteJoinAccount
	}
	return r.JoinAs
}

// EffectiveExpiry is the expiry the invite gets: the requested one, or the
// default for its kind counted from now.
func (r CreateInviteRequestV1) EffectiveExpiry(now time.Time) time.Time {
	if r.ExpiresAt != nil {
		return r.ExpiresAt.Time()
	}
	if r.Kind == InviteKindOpen {
		return now.Add(DefaultOpenInviteLifetime)
	}
	return now.Add(DefaultPersonalInviteLifetime)
}

// EffectiveMayInvite is whether the joiner may invite in turn: the requested
// value, or false. Nobody who joins through an invite may invite unless an
// operator grants it (Nick, #420; issue #457), so that one tester cannot
// mint accounts that mint accounts.
func (r CreateInviteRequestV1) EffectiveMayInvite() bool {
	return r.MayInvite != nil && *r.MayInvite
}

// EffectiveCredentialLifetime is how long the joiner's first credential lives.
func (r CreateInviteRequestV1) EffectiveCredentialLifetime() time.Duration {
	if r.CredentialLifetimeSeconds != nil {
		return time.Duration(*r.CredentialLifetimeSeconds) * time.Second
	}
	return DefaultInviteCredentialLifetimeSeconds * time.Second
}

// Validate reports whether the request is well-formed at the instant now: the
// kind is known; the expiry, when present, is in the future and within the
// maximum for the kind; a personal invite has no max_uses and an open one's is
// positive; there are 1 to MaxInviteChannels distinct, valid channel names;
// the credential lifetime, when present, is positive and at most
// MaxInviteCredentialLifetimeSeconds; and join_as, when present, is known.
// Whether the maker may grant what is asked is the store's rule, not this
// one's.
func (r CreateInviteRequestV1) Validate(now time.Time) error {
	if !r.Kind.Valid() {
		return fmt.Errorf("schema: invite kind must be one of: %s", strings.Join(Vocabularies["InviteKind"], ", "))
	}
	if r.JoinAs != "" && !r.JoinAs.Valid() {
		return fmt.Errorf("schema: invite join_as must be one of: %s", strings.Join(Vocabularies["InviteJoinMode"], ", "))
	}
	maxLifetime := MaxPersonalInviteLifetime
	if r.Kind == InviteKindOpen {
		maxLifetime = MaxOpenInviteLifetime
	}
	if r.ExpiresAt != nil {
		exp := r.ExpiresAt.Time()
		if !exp.After(now) {
			return errors.New("schema: invite expires_at must be in the future")
		}
		if exp.After(now.Add(maxLifetime)) {
			return fmt.Errorf("schema: invite expires_at must be within %s", maxLifetime)
		}
	}
	if r.MaxUses != nil {
		if r.Kind != InviteKindOpen {
			return errors.New("schema: only an open invite takes max_uses")
		}
		if *r.MaxUses < 1 || *r.MaxUses > MaxInviteMaxUses {
			return fmt.Errorf("schema: invite max_uses must be between 1 and %d", MaxInviteMaxUses)
		}
	}
	if n := len(r.Channels); n < 1 || n > MaxInviteChannels {
		return fmt.Errorf("schema: an invite names 1 to %d channels, got %d", MaxInviteChannels, n)
	}
	seen := make(map[string]struct{}, len(r.Channels))
	for _, name := range r.Channels {
		if strings.TrimSpace(name) == "" {
			return errors.New("schema: invite channel name must not be blank")
		}
		if err := ValidateDisplayName("channel name", name, MaxChannelNameLength); err != nil {
			return err
		}
		if _, dup := seen[name]; dup {
			return errors.New("schema: invite names a channel twice")
		}
		seen[name] = struct{}{}
	}
	if r.CredentialLifetimeSeconds != nil {
		if s := *r.CredentialLifetimeSeconds; s < 1 || s > MaxInviteCredentialLifetimeSeconds {
			return fmt.Errorf("schema: invite credential_lifetime_seconds must be between 1 and %d", MaxInviteCredentialLifetimeSeconds)
		}
	}
	return nil
}

// CreateInviteResponseV1 is the response to making an invite. Code is the
// plaintext invite code, shown only here.
type CreateInviteResponseV1 struct {
	Invite InviteV1 `json:"invite"`
	Code   string   `json:"code"`
}

// ValidateInviteCode reports whether code has the exact shape of an invite
// code. It is a cheap pre-check so malformed input never reaches the database;
// the error never echoes the input.
func ValidateInviteCode(code string) error {
	if len(code) != InviteCodeLength || !strings.HasPrefix(code, InviteCodePrefix) {
		return errors.New("schema: malformed invite code")
	}
	for _, c := range []byte(code[len(InviteCodePrefix):]) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return errors.New("schema: malformed invite code")
		}
	}
	return nil
}

// InvitePreviewRequestV1 asks what an invite would grant, without using it.
type InvitePreviewRequestV1 struct {
	Code string `json:"code"`
}

// Validate reports whether the code is well-formed.
func (r InvitePreviewRequestV1) Validate() error { return ValidateInviteCode(r.Code) }

// InvitePreviewV1 is what someone holding a code may learn before joining:
// who invited them and what they would get. Nothing else: no member counts and
// no message content. Join is what the invite offers right now, after the
// server's switches are applied to its join_as (issue #704); a server that
// predates guests omits it, which means an account and nothing else.
type InvitePreviewV1 struct {
	Kind      InviteKind     `json:"kind"`
	Inviter   string         `json:"inviter"`
	Channels  []string       `json:"channels"`
	Voice     bool           `json:"voice"`
	ExpiresAt Timestamp      `json:"expires_at"`
	Join      *JoinOptionsV1 `json:"join,omitempty"`
}

// RedeemInviteRequestV1 uses an invite: Name is the new account's name.
//
// It makes an account with no password, which ADR-009 ends: once passwords
// exist (migration 32) its route answers ErrorCodePasswordRequired and
// makes nothing, and a client redeems with RedeemInviteRequestV2 instead.
// The shape is published and unchanged; the route's retirement is listed
// for the owner's sign-off in docs/design/guests-and-accounts.md §7.
type RedeemInviteRequestV1 struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Validate reports whether the code is well-formed and the name acceptable as
// a human principal name.
func (r RedeemInviteRequestV1) Validate() error {
	if err := ValidateInviteCode(r.Code); err != nil {
		return err
	}
	return CreatePrincipalRequest{Kind: PrincipalHuman, Name: r.Name}.Validate()
}

// RedeemInviteResponseV1 is the response to using an invite: the new principal
// and its first credential. Token is the plaintext bearer token, shown only
// here. POST /v2/invites/redeem answers with it too.
type RedeemInviteResponseV1 struct {
	Principal  PrincipalV0  `json:"principal"`
	Credential CredentialV1 `json:"credential"`
	Token      string       `json:"token"`
}

// RedeemInviteRequestV2 is the body of POST /v2/invites/redeem: Create an
// account through an invite, name and password together (ADR-009 D3,
// design §3.1). The account exists only once a usable password is stored;
// a failed setup makes no account and does not use the invite. Refused
// with ErrorCodeJoiningClosed by an invite that admits guests only. The
// response is RedeemInviteResponseV1.
type RedeemInviteRequestV2 struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Validate reports whether the code is well-formed, the name acceptable as
// a permanent human principal name outside the reserved guest namespace,
// and the password passes ValidatePassword.
func (r RedeemInviteRequestV2) Validate() error {
	if err := ValidateInviteCode(r.Code); err != nil {
		return err
	}
	if err := validatePermanentName(r.Name); err != nil {
		return err
	}
	return ValidatePassword(r.Password)
}

// RedeemInviteGuestRequestV1 is the body of POST /v1/invites/redeem-guest:
// Join as guest through an invite whose join_as admits guests, while the
// server's guests_enabled switch is on (ErrorCodeGuestsDisabled otherwise).
// Label is the optional display label (design §4.1). The visit's absolute
// deadline is the earlier of the server's bound and the invite's credential
// lifetime; the response is JoinGuestResponseV1.
type RedeemInviteGuestRequestV1 struct {
	Code  string `json:"code"`
	Label string `json:"label,omitempty"`
}

// Validate reports whether the code is well-formed and the label, when
// given, passes ValidateGuestLabel.
func (r RedeemInviteGuestRequestV1) Validate() error {
	if err := ValidateInviteCode(r.Code); err != nil {
		return err
	}
	if r.Label == "" {
		return nil
	}
	return ValidateGuestLabel(r.Label)
}

// ListInvitesResponseV1 is the response to listing invites. Producers must
// send an empty slice, not nil, so the wire form is [] rather than null.
type ListInvitesResponseV1 struct {
	Invites []InviteV1 `json:"invites"`
}

// Operator administration of invites (issue #438): seeing who joined through
// an invite, disabling them or everyone a person invited, the right to invite,
// and the pause switch. Like the shapes above, these are additive REST bodies
// with no schema-name field.

// InviteJoinerV1 is one person who joined through an invite, as an operator
// sees them.
type InviteJoinerV1 struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt Timestamp `json:"created_at"`
	Disabled  bool      `json:"disabled"`
}

// InviteJoinersResponseV1 lists everyone who joined through one invite, by id.
// Producers must send an empty slice, not nil.
type InviteJoinersResponseV1 struct {
	Joiners []InviteJoinerV1 `json:"joiners"`
}

// DisableJoinersRequestV1 asks to disable everyone who joined through an
// invite. DryRun only counts them. Close, absent by default, says whether the
// invite is closed too; see EffectiveClose.
type DisableJoinersRequestV1 struct {
	DryRun bool  `json:"dry_run"`
	Close  *bool `json:"close,omitempty"`
}

// EffectiveClose is whether the invite is closed with the disabling: the
// requested value, or true. A link that leaked is closed unless the operator
// says otherwise.
func (r DisableJoinersRequestV1) EffectiveClose() bool {
	return r.Close == nil || *r.Close
}

// DisableJoinersResponseV1 is the answer of both bulk disables, of an
// invite's joiners and of a person's invitees: how many principals were
// disabled and which, or with DryRun how many and which would be. Producers
// must send an empty slice, not nil.
type DisableJoinersResponseV1 struct {
	DryRun       bool    `json:"dry_run"`
	Count        int     `json:"count"`
	PrincipalIDs []int64 `json:"principal_ids"`
}

// InviteeDepth is how far down the tree of invites a disable-invitees
// request goes. The vocabulary is closed.
type InviteeDepth string

const (
	// InviteeDepthDirect is the people the person invited, and no further.
	InviteeDepthDirect InviteeDepth = "1"
	// InviteeDepthAll is everyone under the person: whom they invited, whom
	// those invited, and so on.
	InviteeDepthAll InviteeDepth = "all"
)

// Valid reports whether d is a recognized depth.
func (d InviteeDepth) Valid() bool {
	switch d {
	case InviteeDepthDirect, InviteeDepthAll:
		return true
	default:
		return false
	}
}

// DisableInviteesRequestV1 asks to disable the people one person invited.
// DryRun only counts them. Depth is absent by default, which is
// InviteeDepthDirect.
type DisableInviteesRequestV1 struct {
	DryRun bool         `json:"dry_run"`
	Depth  InviteeDepth `json:"depth,omitempty"`
}

// EffectiveDepth is the depth asked for, or InviteeDepthDirect.
func (r DisableInviteesRequestV1) EffectiveDepth() InviteeDepth {
	if r.Depth == "" {
		return InviteeDepthDirect
	}
	return r.Depth
}

// Validate reports whether the depth, when present, is known.
func (r DisableInviteesRequestV1) Validate() error {
	if r.Depth != "" && !r.Depth.Valid() {
		return fmt.Errorf("schema: depth must be one of: %s", strings.Join(Vocabularies["InviteeDepth"], ", "))
	}
	return nil
}

// InvitePermissionRequestV1 grants or takes away one person's right to make
// personal invites. Only an operator sends it.
type InvitePermissionRequestV1 struct {
	MayInvite bool `json:"may_invite"`
}

// InviteSettingsV1 is the server's invite switch: while Paused, no invite can
// be made, previewed or used. It is both the body of the change and the
// answer to reading it.
type InviteSettingsV1 struct {
	Paused bool `json:"paused"`
}
