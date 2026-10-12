package schema

import (
	"errors"
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
)

// CredentialV1 describes one credential. It never contains the token or its
// hash. ExpiresAt is omitted when the credential does not expire; RevokedAt is
// omitted while the credential is active.
type CredentialV1 struct {
	ID          int64      `json:"id"`
	PrincipalID int64      `json:"principal_id"`
	Label       string     `json:"label"`
	CreatedAt   Timestamp  `json:"created_at"`
	ExpiresAt   *Timestamp `json:"expires_at,omitempty"`
	RevokedAt   *Timestamp `json:"revoked_at,omitempty"`
}

// CreateCredentialRequestV1 is the body of POST /v1/principals/{id}/credentials.
type CreateCredentialRequestV1 struct {
	Label     string     `json:"label"`
	ExpiresAt *Timestamp `json:"expires_at,omitempty"`
}

// Validate reports whether the request is well-formed at the instant now: the
// label must be non-blank, pass ValidateDisplayName and be at most
// MaxCredentialLabelLength characters, and expires_at, when present, must be
// in the future.
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
