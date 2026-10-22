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
	// Both are for a link of purpose sign_in.
	DefaultSignInLinkLifetime = 24 * time.Hour
	MaxSignInLinkLifetime     = 7 * 24 * time.Hour
	// DefaultRestrictedLinkLifetime and MaxRestrictedLinkLifetime are the
	// same bounds for every other purpose (ADR-009 §6): a reset, enrolment,
	// device or conversion link is made to be used now, so it lives ten
	// minutes by default and an hour at most.
	DefaultRestrictedLinkLifetime = 10 * time.Minute
	MaxRestrictedLinkLifetime     = time.Hour
)

// SignInLinkPurpose is what a link authorizes (ADR-009 §6, issue #704).
// Every route that takes a code accepts the purposes it names and answers
// ErrorCodeSignInLinkInvalid for any other, so a reset code is not a
// sign-in, a device link is not an enrolment proof, and a sign-in link is
// not a conversion permit. The vocabulary is closed. A link with no purpose
// on the wire predates purposes and is a sign_in link.
type SignInLinkPurpose string

const (
	// SignInLinkPurposeSignIn: an operator-made link that signs the person
	// back in; redeemed by POST /v1/sign-in-links/redeem.
	SignInLinkPurposeSignIn SignInLinkPurpose = "sign_in"
	// SignInLinkPurposeDevice: a member-made link that signs their own
	// account in on another device (design §6.2); made by POST
	// /v1/sign-in-links/mine under a recent password proof, redeemed by
	// POST /v1/sign-in-links/redeem.
	SignInLinkPurposeDevice SignInLinkPurpose = "device"
	// SignInLinkPurposeReset: an operator-made link that lets the member
	// replace a forgotten password (design §6.1); spent by POST
	// /v1/password/reset and good for nothing else.
	SignInLinkPurposeReset SignInLinkPurpose = "reset"
	// SignInLinkPurposeEnrollment: an operator-made proof that lets a member
	// who predates passwords set their first one on their existing session
	// (design §5.2); spent by POST /v1/password/mine.
	SignInLinkPurposeEnrollment SignInLinkPurpose = "enrollment"
	// SignInLinkPurposeConversion: an operator-made permit that lets one
	// live guest create an account although its entry point's policy is
	// closed (design §3.3); made by POST /v1/guests/{id}/conversion-permit,
	// spent by POST /v1/guest/convert.
	SignInLinkPurposeConversion SignInLinkPurpose = "conversion"
)

// Valid reports whether p is a recognized purpose.
func (p SignInLinkPurpose) Valid() bool {
	switch p {
	case SignInLinkPurposeSignIn, SignInLinkPurposeDevice, SignInLinkPurposeReset, SignInLinkPurposeEnrollment, SignInLinkPurposeConversion:
		return true
	default:
		return false
	}
}

// Lifetimes are the default and longest lifetime of a link of purpose p.
func (p SignInLinkPurpose) Lifetimes() (def, max time.Duration) {
	if p == "" || p == SignInLinkPurposeSignIn {
		return DefaultSignInLinkLifetime, MaxSignInLinkLifetime
	}
	return DefaultRestrictedLinkLifetime, MaxRestrictedLinkLifetime
}

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
// Purpose is what it authorizes; a server that predates purposes omits it,
// which means sign_in, and a server with them always sends it. Label is the
// label a device link's maker gave it, which the resulting credential
// carries, so the member can tell their sign-ins apart.
type SignInLinkV1 struct {
	ID          int64             `json:"id"`
	PrincipalID int64             `json:"principal_id"`
	CreatedBy   int64             `json:"created_by"`
	CreatedAt   Timestamp         `json:"created_at"`
	ExpiresAt   Timestamp         `json:"expires_at"`
	Purpose     SignInLinkPurpose `json:"purpose,omitempty"`
	Label       string            `json:"label,omitempty"`
}

// CreateSignInLinkRequestV1 is the body of the operator route that makes a
// link for a person, POST /v1/principals/{id}/sign-in-links. Purpose is
// optional and sign_in by default; an operator may also make a reset or an
// enrollment link (a device link is the member's own to make, a conversion
// permit the guest route's). ExpiresAt is optional: the server applies
// EffectiveExpiry. A reset or enrollment link is refused for a guest,
// an operator, a disabled or a hidden target, as a sign-in link is.
type CreateSignInLinkRequestV1 struct {
	ExpiresAt *Timestamp        `json:"expires_at,omitempty"`
	Purpose   SignInLinkPurpose `json:"purpose,omitempty"`
}

// EffectivePurpose is the purpose asked for, or sign_in.
func (r CreateSignInLinkRequestV1) EffectivePurpose() SignInLinkPurpose {
	if r.Purpose == "" {
		return SignInLinkPurposeSignIn
	}
	return r.Purpose
}

// EffectiveExpiry is the expiry the link gets: the requested one, or the
// default lifetime of its purpose from now.
func (r CreateSignInLinkRequestV1) EffectiveExpiry(now time.Time) time.Time {
	if r.ExpiresAt != nil {
		return r.ExpiresAt.Time()
	}
	def, _ := r.EffectivePurpose().Lifetimes()
	return now.Add(def)
}

// Validate reports whether the request is well-formed at the instant now:
// the purpose, when present, is one an operator makes on this route, and
// the expiry, when present, is in the future and within the purpose's
// longest lifetime.
func (r CreateSignInLinkRequestV1) Validate(now time.Time) error {
	switch r.Purpose {
	case "", SignInLinkPurposeSignIn, SignInLinkPurposeReset, SignInLinkPurposeEnrollment:
	case SignInLinkPurposeDevice, SignInLinkPurposeConversion:
		return fmt.Errorf("schema: a %s link is not made on this route", r.Purpose)
	default:
		return fmt.Errorf("schema: purpose must be one of: %s", strings.Join(Vocabularies["SignInLinkPurpose"], ", "))
	}
	_, max := r.EffectivePurpose().Lifetimes()
	return validateLinkExpiry(r.ExpiresAt, now, max)
}

// validateLinkExpiry checks a link's requested expiry, when there is one:
// in the future and within max of now.
func validateLinkExpiry(expiresAt *Timestamp, now time.Time, max time.Duration) error {
	if expiresAt == nil {
		return nil
	}
	exp := expiresAt.Time()
	if !exp.After(now) {
		return errors.New("schema: sign-in link expires_at must be in the future")
	}
	if exp.After(now.Add(max)) {
		return fmt.Errorf("schema: sign-in link expires_at must be within %s", max)
	}
	return nil
}

// CreateDeviceLinkRequestV1 is the body of POST /v1/sign-in-links/mine: a
// member makes a link of purpose device for their own account (design
// §6.2). It needs a recent password proof (ErrorCodeReauthRequired) and
// at most MaxOpenDeviceLinks may be open (ErrorCodeTooManyDeviceLinks).
// Label, optional, names the device, and becomes the label of the
// credential the link issues. ExpiresAt is optional: the server applies
// EffectiveExpiry. Guests, operators and agents are refused.
type CreateDeviceLinkRequestV1 struct {
	Label     string     `json:"label,omitempty"`
	ExpiresAt *Timestamp `json:"expires_at,omitempty"`
}

// EffectiveExpiry is the expiry the link gets: the requested one, or
// DefaultRestrictedLinkLifetime from now.
func (r CreateDeviceLinkRequestV1) EffectiveExpiry(now time.Time) time.Time {
	if r.ExpiresAt != nil {
		return r.ExpiresAt.Time()
	}
	return now.Add(DefaultRestrictedLinkLifetime)
}

// Validate reports whether the request is well-formed at the instant now:
// the label, when present, passes ValidateDisplayName within
// MaxCredentialLabelLength, and the expiry, when present, is in the future
// and within MaxRestrictedLinkLifetime.
func (r CreateDeviceLinkRequestV1) Validate(now time.Time) error {
	if r.Label != "" {
		if strings.TrimSpace(r.Label) == "" {
			return errors.New("schema: device link label must not be blank")
		}
		if err := ValidateDisplayName("device link label", r.Label, MaxCredentialLabelLength); err != nil {
			return err
		}
	}
	return validateLinkExpiry(r.ExpiresAt, now, MaxRestrictedLinkLifetime)
}

// ListOwnSignInLinksResponseV1 is the response to GET /v1/sign-in-links/mine:
// the caller's own open device links, newest first; used, expired and closed
// ones are left out. It never carries a code. DELETE /v1/sign-in-links/mine/{id}
// closes one. Producers must send an empty slice, not nil.
type ListOwnSignInLinksResponseV1 struct {
	SignInLinks []SignInLinkV1 `json:"sign_in_links"`
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
// it: the name of the account it opens, and what the link is for, so the
// page shows the right flow (a sign-in, a device sign-in or a password
// reset). Nothing else. A server that predates purposes omits Purpose,
// which means sign_in. An enrollment proof or a conversion permit is used
// by a signed-in caller and has no landing page: the preview answers
// ErrorCodeSignInLinkInvalid for either.
type SignInLinkPreviewV1 struct {
	Name    string            `json:"name"`
	Purpose SignInLinkPurpose `json:"purpose,omitempty"`
}

// RedeemSignInLinkRequestV1 uses a sign-in link of purpose sign_in or
// device; any other purpose is refused as invalid.
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
