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
	// ErrorCodeInviteGrantExceedsInviter: the invite names a channel, voice or
	// invite right its maker does not hold.
	ErrorCodeInviteGrantExceedsInviter = "invite_grant_exceeds_inviter"
	// ErrorCodeInviteNotAllowed: the caller may not make this kind of invite.
	ErrorCodeInviteNotAllowed = "invite_not_allowed"
)

// InviteChannelV1 names one channel an invite adds its joiner to.
type InviteChannelV1 struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// InviteV1 describes one invite. It never carries the code or its hash.
// MaxUses is omitted when uncapped and is always 1 for a personal invite;
// ClosedAt is omitted while the invite is open.
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
}

// CreateInviteRequestV1 is the body of the route that makes an invite.
// Channels are names. ExpiresAt, MayInvite and CredentialLifetimeSeconds are
// optional: the server applies EffectiveExpiry, EffectiveMayInvite and
// EffectiveCredentialLifetime. MaxUses is for open invites only and is absent
// by default (no cap).
type CreateInviteRequestV1 struct {
	Kind                      InviteKind `json:"kind"`
	Channels                  []string   `json:"channels"`
	ExpiresAt                 *Timestamp `json:"expires_at,omitempty"`
	MaxUses                   *int64     `json:"max_uses,omitempty"`
	Voice                     bool       `json:"voice"`
	MayInvite                 *bool      `json:"may_invite,omitempty"`
	CredentialLifetimeSeconds *int64     `json:"credential_lifetime_seconds,omitempty"`
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
// value, or true for a personal invite and false for an open link.
func (r CreateInviteRequestV1) EffectiveMayInvite() bool {
	if r.MayInvite != nil {
		return *r.MayInvite
	}
	return r.Kind == InviteKindPersonal
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
// positive; there are 1 to MaxInviteChannels distinct, valid channel names; and
// the credential lifetime, when present, is positive and at most
// MaxInviteCredentialLifetimeSeconds. Whether the maker may grant what is asked
// is the store's rule, not this one's.
func (r CreateInviteRequestV1) Validate(now time.Time) error {
	if !r.Kind.Valid() {
		return fmt.Errorf("schema: invite kind must be one of: %s", strings.Join(Vocabularies["InviteKind"], ", "))
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
// no message content.
type InvitePreviewV1 struct {
	Kind      InviteKind `json:"kind"`
	Inviter   string     `json:"inviter"`
	Channels  []string   `json:"channels"`
	Voice     bool       `json:"voice"`
	ExpiresAt Timestamp  `json:"expires_at"`
}

// RedeemInviteRequestV1 uses an invite: Name is the new account's name.
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
// here.
type RedeemInviteResponseV1 struct {
	Principal  PrincipalV0  `json:"principal"`
	Credential CredentialV1 `json:"credential"`
	Token      string       `json:"token"`
}

// ListInvitesResponseV1 is the response to listing invites. Producers must
// send an empty slice, not nil, so the wire form is [] rather than null.
type ListInvitesResponseV1 struct {
	Invites []InviteV1 `json:"invites"`
}
