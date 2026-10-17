package schema

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sign-in link wire shapes (issue #493). A sign-in link is how a person who
// signed out, or cleared their browser, gets back into the same account: an
// operator makes a one-time code for that existing person, and whoever opens
// the link gets a new credential for them. It is an invite's machinery turned
// to an account that already exists: a hashed code shown once, single use,
// a short expiry, the invite routes' rate limits, and one answer for every
// code that does not work. Like the invite shapes these are additive REST
// bodies with no schema-name field, versioned by the V1 type suffix.

const (
	// SignInLinkCodePrefix starts every sign-in link code. It differs from an
	// invite's, so neither route ever mistakes one for the other.
	SignInLinkCodePrefix = "conch_sil_"
	// SignInLinkCodeRandomBytes is the number of crypto/rand bytes in a code.
	SignInLinkCodeRandomBytes = 32
	// SignInLinkCodeLength is the exact length of a code: the prefix plus 32
	// random bytes in unpadded base64url (43 characters).
	SignInLinkCodeLength = len(SignInLinkCodePrefix) + 43

	// DefaultSignInLinkLifetime applies when a create request names no
	// expiry; MaxSignInLinkLifetime bounds how far ahead one may be set.
	DefaultSignInLinkLifetime = 24 * time.Hour
	MaxSignInLinkLifetime     = 7 * 24 * time.Hour
)

// ErrorCodeSignInLinkInvalid is the one answer (HTTP 404) for a code that is
// malformed, unknown, expired, used, closed, made by someone no longer an
// operator, or for a person who may no longer sign in. The cases are
// deliberately indistinguishable, so the answer is not an oracle.
const ErrorCodeSignInLinkInvalid = "sign_in_link_invalid"

// ErrorCodeSignInLinkNotFound: no sign-in link has that id (HTTP 404), on
// the operator's route that closes one.
const ErrorCodeSignInLinkNotFound = "sign_in_link_not_found"

// SignInLinkV1 describes one sign-in link as it is made: whom it is for, who
// made it, and until when it works. It never carries the code or its hash.
type SignInLinkV1 struct {
	ID          int64     `json:"id"`
	PrincipalID int64     `json:"principal_id"`
	CreatedBy   int64     `json:"created_by"`
	CreatedAt   Timestamp `json:"created_at"`
	ExpiresAt   Timestamp `json:"expires_at"`
}

// CreateSignInLinkRequestV1 is the body of the route that makes a sign-in
// link. ExpiresAt is optional: the server applies EffectiveExpiry.
type CreateSignInLinkRequestV1 struct {
	ExpiresAt *Timestamp `json:"expires_at,omitempty"`
}

// EffectiveExpiry is the expiry the link gets: the requested one, or
// DefaultSignInLinkLifetime from now.
func (r CreateSignInLinkRequestV1) EffectiveExpiry(now time.Time) time.Time {
	if r.ExpiresAt != nil {
		return r.ExpiresAt.Time()
	}
	return now.Add(DefaultSignInLinkLifetime)
}

// Validate reports whether the request is well-formed at the instant now:
// the expiry, when present, is in the future and within
// MaxSignInLinkLifetime.
func (r CreateSignInLinkRequestV1) Validate(now time.Time) error {
	if r.ExpiresAt == nil {
		return nil
	}
	exp := r.ExpiresAt.Time()
	if !exp.After(now) {
		return errors.New("schema: sign-in link expires_at must be in the future")
	}
	if exp.After(now.Add(MaxSignInLinkLifetime)) {
		return fmt.Errorf("schema: sign-in link expires_at must be within %s", MaxSignInLinkLifetime)
	}
	return nil
}

// CreateSignInLinkResponseV1 is the response to making a sign-in link. Code
// is the plaintext code, shown only here.
type CreateSignInLinkResponseV1 struct {
	SignInLink SignInLinkV1 `json:"sign_in_link"`
	Code       string       `json:"code"`
}

// ValidateSignInLinkCode reports whether code has the exact shape of a
// sign-in link code. It is a cheap pre-check so malformed input never
// reaches the database; the error never echoes the input.
func ValidateSignInLinkCode(code string) error {
	if len(code) != SignInLinkCodeLength || !strings.HasPrefix(code, SignInLinkCodePrefix) {
		return errors.New("schema: malformed sign-in link code")
	}
	for _, c := range []byte(code[len(SignInLinkCodePrefix):]) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return errors.New("schema: malformed sign-in link code")
		}
	}
	return nil
}

// SignInLinkPreviewRequestV1 asks whose account a link opens, without using
// it.
type SignInLinkPreviewRequestV1 struct {
	Code string `json:"code"`
}

// Validate reports whether the code is well-formed.
func (r SignInLinkPreviewRequestV1) Validate() error { return ValidateSignInLinkCode(r.Code) }

// SignInLinkPreviewV1 is what someone holding a code may learn before using
// it: the name of the account it opens, and nothing else.
type SignInLinkPreviewV1 struct {
	Name string `json:"name"`
}

// RedeemSignInLinkRequestV1 uses a sign-in link.
type RedeemSignInLinkRequestV1 struct {
	Code string `json:"code"`
}

// Validate reports whether the code is well-formed.
func (r RedeemSignInLinkRequestV1) Validate() error { return ValidateSignInLinkCode(r.Code) }

// RedeemSignInLinkResponseV1 is the response to using a sign-in link: the
// person it was for, unchanged, and their new credential. Token is the
// plaintext bearer token, shown only here.
type RedeemSignInLinkResponseV1 struct {
	Principal  PrincipalV0  `json:"principal"`
	Credential CredentialV1 `json:"credential"`
	Token      string       `json:"token"`
}
