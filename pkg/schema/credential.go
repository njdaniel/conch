package schema

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Credential wire shapes (issue #78). A credential binds one bearer token to
// exactly one principal, human or agent. The token itself is shown exactly
// once, in a create or rotate response; no other shape ever carries it or its
// hash. These shapes are additive and carry no schema-name field: they are
// REST bodies, versioned by the V1 type suffix.

const (
	// CredentialTokenPrefix starts every credential token.
	CredentialTokenPrefix = "conch_"
	// CredentialTokenRandomBytes is the number of crypto/rand bytes in a token.
	CredentialTokenRandomBytes = 32
	// CredentialTokenLength is the exact length of a token: the prefix plus 32
	// random bytes in unpadded base64url (43 characters).
	CredentialTokenLength = len(CredentialTokenPrefix) + 43
	// MaxCredentialLabelLength is the longest allowed label, in characters.
	MaxCredentialLabelLength = 100
	// MinCredentialRenewLifetimeSeconds and MaxCredentialRenewLifetimeSeconds
	// bound renew_lifetime_seconds (issue #439). A renewable credential is
	// renewed at most once a day, so a lifetime of a day or less would never
	// renew: the floor is two days. The ceiling is an invite's.
	MinCredentialRenewLifetimeSeconds = 2 * 24 * 60 * 60
	MaxCredentialRenewLifetimeSeconds = MaxInviteCredentialLifetimeSeconds
)

// CredentialV1 describes one credential. It never contains the token or its
// hash. ExpiresAt is omitted when the credential does not expire; RevokedAt is
// omitted while the credential is active. Renews is true for a credential
// whose expiry moves forward as it is used (issue #439): ExpiresAt is where
// the expiry stands now, and a later read may show it later.
//
// IdleExpiresAt is present for a credential with an idle bound (issue #704,
// ADR-009): a guest's and an operator's browser session. It is where the
// idle bound stands now: a REST request carrying the bearer moves it, at
// most once a minute and never past ExpiresAt, and nothing else does (not a
// socket, a ping, a ticket or the voice poller; security review F5). The
// credential is live until the earlier of the two. Scope is the
// credential's reach; absent means full, which every credential had before
// setup tokens existed.
type CredentialV1 struct {
	ID            int64           `json:"id"`
	PrincipalID   int64           `json:"principal_id"`
	Label         string          `json:"label"`
	CreatedAt     Timestamp       `json:"created_at"`
	ExpiresAt     *Timestamp      `json:"expires_at,omitempty"`
	RevokedAt     *Timestamp      `json:"revoked_at,omitempty"`
	Renews        bool            `json:"renews,omitempty"`
	IdleExpiresAt *Timestamp      `json:"idle_expires_at,omitempty"`
	Scope         CredentialScope `json:"scope,omitempty"`
}

// CreateCredentialRequestV1 is the body of POST /v1/principals/{id}/credentials.
//
// RenewLifetimeSeconds asks for a renewable credential (issue #439): it
// expires that long after it is issued, and on a day it is used its expiry
// moves to that long after the use. One left unused for the whole lifetime
// expires. It cannot be combined with ExpiresAt, which fixes the expiry.
type CreateCredentialRequestV1 struct {
	Label                string     `json:"label"`
	ExpiresAt            *Timestamp `json:"expires_at,omitempty"`
	RenewLifetimeSeconds *int64     `json:"renew_lifetime_seconds,omitempty"`
}

// Validate reports whether the request is well-formed at the instant now: the
// label must be non-blank, pass ValidateDisplayName and be at most
// MaxCredentialLabelLength characters; expires_at, when present, must be in
// the future; and renew_lifetime_seconds, when present, must come without
// expires_at and lie within MinCredentialRenewLifetimeSeconds and
// MaxCredentialRenewLifetimeSeconds.
func (r CreateCredentialRequestV1) Validate(now time.Time) error {
	if strings.TrimSpace(r.Label) == "" {
		return errors.New("schema: credential label must not be blank")
	}
	if err := ValidateDisplayName("credential label", r.Label, MaxCredentialLabelLength); err != nil {
		return err
	}
	if r.ExpiresAt != nil && !r.ExpiresAt.Time().After(now) {
		return errors.New("schema: credential expires_at must be in the future")
	}
	if r.RenewLifetimeSeconds != nil {
		if r.ExpiresAt != nil {
			return errors.New("schema: credential renew_lifetime_seconds and expires_at cannot both be given")
		}
		if s := *r.RenewLifetimeSeconds; s < MinCredentialRenewLifetimeSeconds || s > MaxCredentialRenewLifetimeSeconds {
			return fmt.Errorf("schema: credential renew_lifetime_seconds must be between %d and %d", MinCredentialRenewLifetimeSeconds, MaxCredentialRenewLifetimeSeconds)
		}
	}
	return nil
}

// CreateCredentialResponseV1 is the response to creating a credential. Token
// is the plaintext bearer token, shown only here.
type CreateCredentialResponseV1 struct {
	Credential CredentialV1 `json:"credential"`
	Token      string       `json:"token"`
}

// RotateCredentialResponseV1 is the response to rotating a credential: the new
// credential and its plaintext token, shown only here.
type RotateCredentialResponseV1 struct {
	Credential CredentialV1 `json:"credential"`
	Token      string       `json:"token"`
}

// ListCredentialsResponseV1 is the response to listing a principal's
// credentials, newest first, revoked ones included. Producers must send an
// empty slice, not nil, so the wire form is [] rather than null.
type ListCredentialsResponseV1 struct {
	Credentials []CredentialV1 `json:"credentials"`
}

// OwnCredentialV1 is one of the caller's own sign-ins, as
// GET /v1/credentials/mine lists it (issue #496): a credential of theirs that
// can still sign in. It never contains the token or its hash, nor the
// principal, which is the caller. Current is true for the credential the
// listing request itself came with ("this browser"). A credential made by a
// sign-in link is labelled "sign-in link <id>", so the person can see that a
// link was used for their account; one made by a device link carries the
// label its maker gave the link. IdleExpiresAt is as on CredentialV1.
type OwnCredentialV1 struct {
	ID            int64      `json:"id"`
	Label         string     `json:"label"`
	CreatedAt     Timestamp  `json:"created_at"`
	ExpiresAt     *Timestamp `json:"expires_at,omitempty"`
	Renews        bool       `json:"renews,omitempty"`
	Current       bool       `json:"current,omitempty"`
	IdleExpiresAt *Timestamp `json:"idle_expires_at,omitempty"`
}

// ListOwnCredentialsResponseV1 is the response to GET /v1/credentials/mine:
// the caller's live credentials, newest first; revoked and expired ones are
// left out. Producers must send an empty slice, not nil.
type ListOwnCredentialsResponseV1 struct {
	Credentials []OwnCredentialV1 `json:"credentials"`
}
