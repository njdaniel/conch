package schema

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Password, sign-in and operator authentication wire shapes (ADR-009 §5–6,
// docs/design/guests-and-accounts.md; issue #704). A member signs in with a
// username and password; an operator with those and a TOTP code, through a
// short-lived pending-login challenge; a sensitive action needs a recent
// proof of the password, bound to the session. These shapes are additive
// and carry no schema-name field: they are REST bodies, versioned by the V1
// type suffix. No shape ever carries a password hash, a TOTP secret after
// enrolment, or a session token other than in the one response that issues
// it. They are contracts for #707–#713; this file adds no behaviour.

const (
	// MinPasswordLength and MaxPasswordLength bound a password in Unicode
	// code points (design §5.2; NIST SP 800-63B's direction). The server
	// normalizes to NFC before hashing and applies the same bounds again,
	// since composition can shorten a string in code points.
	MinPasswordLength = 15
	MaxPasswordLength = 128
	// MaxPasswordBytes bounds a password's UTF-8 independently of its
	// length in characters, before and after normalization.
	MaxPasswordBytes = 512

	// PendingLoginPrefix starts every pending-login challenge, which an
	// operator's correct password yields in place of a credential; it is
	// spent by one TOTP completion. It differs from every other secret's
	// prefix, so no route mistakes one for a credential, a ticket or a link.
	PendingLoginPrefix = "conch_chal_"
	// PendingLoginRandomBytes is the number of crypto/rand bytes in a
	// challenge; PendingLoginLength its exact length (the prefix plus 43
	// characters of unpadded base64url).
	PendingLoginRandomBytes = 32
	PendingLoginLength      = len(PendingLoginPrefix) + 43
	// PendingLoginLifetimeSeconds is how long a challenge may be completed
	// after it is issued: five minutes.
	PendingLoginLifetimeSeconds = 5 * 60
	// MaxTOTPAttemptsPerChallenge is how many wrong codes one challenge
	// takes before it is spent. Every wrong code also counts in the typed
	// name's failure window, with wrong passwords (security review F2).
	MaxTOTPAttemptsPerChallenge = 5

	// TOTP parameters (RFC 6238, design §5.3): a 160-bit secret, HMAC-SHA-1,
	// six digits, 30-second steps, and a window of one step either side.
	TOTPSecretBytes   = 20
	TOTPAlgorithm     = "SHA1"
	TOTPDigits        = 6
	TOTPPeriodSeconds = 30
	TOTPWindowSteps   = 1

	// SetupTokenPrefix starts every setup token, which a host command makes
	// to bootstrap the first operator or recover an operator's
	// authentication offline; SetupTokenLength is its exact length.
	// Redeeming one spends it and answers a credential of scope setup.
	SetupTokenPrefix      = "conch_setup_"
	SetupTokenRandomBytes = 32
	SetupTokenLength      = len(SetupTokenPrefix) + 43
	// SetupTokenLifetimeSeconds is how long a setup token may be redeemed
	// after the host command makes it: ten minutes.
	SetupTokenLifetimeSeconds = 10 * 60
	// SetupCredentialLifetimeSeconds is how long the setup-scoped
	// credential it answers lives: fifteen minutes, enough to set a
	// password and enrol TOTP.
	SetupCredentialLifetimeSeconds = 15 * 60

	// OperatorSessionAbsoluteSeconds and OperatorSessionIdleSeconds bound a
	// credential that an operator's browser sign-in issues: twelve hours,
	// and an hour without a REST request carrying it.
	OperatorSessionAbsoluteSeconds = 12 * 60 * 60
	OperatorSessionIdleSeconds     = 60 * 60
	// RecentAuthLifetimeSeconds is how long a proof of the password (and,
	// for an operator, the TOTP code) stands: ten minutes. It is bound to
	// the credential that proved it, and any credential or authentication
	// change ends it early.
	RecentAuthLifetimeSeconds = 10 * 60

	// MaxOpenDeviceLinks is the most unused, unexpired device links one
	// member may hold at once (design §6.2).
	MaxOpenDeviceLinks = 3
)

// Error codes of the authentication routes (ErrorV0.Code).
const (
	// ErrorCodeAuthFailed (HTTP 401) is the one answer for a sign-in that
	// does not succeed: an unknown name, a wrong password, a wrong or
	// replayed TOTP code, an unknown or expired challenge, a disabled,
	// hidden or guest principal, or one with no password yet. An operator
	// whose setup is incomplete (no password, or a password but no
	// confirmed TOTP) answers it too: never signed_in and never a
	// challenge, until the host-made setup token completes the setup. The
	// cases are deliberately indistinguishable and cost the server
	// comparable work, so the answer is not an oracle. A re-authentication
	// that fails answers it as well, an operator's missing or wrong code
	// included.
	ErrorCodeAuthFailed = "auth_failed"
	// ErrorCodeReauthRequired (HTTP 403): the action needs a recent proof of
	// the password (and TOTP for an operator) and the session has none
	// that is still good; the client proves it with POST /v1/reauth and
	// repeats the action.
	ErrorCodeReauthRequired = "reauth_required"
	// ErrorCodeEnrollmentProofRequired (HTTP 403): POST /v1/password/mine
	// set a first password with no enrollment proof. It is not a recent-auth
	// prompt: the caller has no password to prove, so POST /v1/reauth cannot
	// help (it answers password_not_set). The client asks for the one-use
	// enrollment code an operator made, and repeats the request with it as
	// the proof.
	ErrorCodeEnrollmentProofRequired = "enrollment_proof_required"
	// ErrorCodeSetupOnHost (HTTP 403): an operator's first password and TOTP
	// enrolment happen only through a host-made setup token, never on an
	// ordinary bearer credential (security review F7).
	ErrorCodeSetupOnHost = "setup_on_host"
	// ErrorCodeSetupIncomplete (HTTP 403): the credential has scope setup,
	// which reaches only the routes that finish setup (design §5.3,
	// review F6). Checked in the authentication middleware, so no route can
	// forget it.
	ErrorCodeSetupIncomplete = "setup_incomplete"
	// ErrorCodePasswordRequired (HTTP 409): the request would create an
	// account with no password. POST /v1/invites/redeem answers it once
	// passwords exist (migration 32): a client that predates them must be
	// updated to POST /v2/invites/redeem; no account is made.
	ErrorCodePasswordRequired = "password_required"
	// ErrorCodePasswordUnacceptable (HTTP 400): the password passes the wire
	// bounds but not the server's policy: it is on the bundled common
	// password list, or contains the account's name or the server's.
	ErrorCodePasswordUnacceptable = "password_unacceptable"
	// ErrorCodePasswordNotSet (HTTP 409): a change asked for the current
	// password of an account that has none yet; the account enrols one
	// with POST /v1/password/mine and an enrolment proof instead.
	ErrorCodePasswordNotSet = "password_not_set"
	// ErrorCodePasswordAlreadySet (HTTP 409): an enrolment on a full-scope
	// credential for an account that has a password; it changes it with
	// PUT /v1/password/mine. On a setup-scoped credential the enrolment
	// replaces the password instead: the host-made setup token is the
	// proof, and that is how an operator recovers.
	ErrorCodePasswordAlreadySet = "password_already_set"
	// ErrorCodeTOTPAlreadyEnrolled (HTTP 409): the operator has TOTP; only
	// offline recovery replaces it.
	ErrorCodeTOTPAlreadyEnrolled = "totp_already_enrolled"
	// ErrorCodeTOTPNotEnrolled (HTTP 409): a confirmation with no enrolment
	// pending.
	ErrorCodeTOTPNotEnrolled = "totp_not_enrolled"
	// ErrorCodeSetupTokenInvalid (HTTP 404): the token is malformed,
	// unknown, expired or spent; one answer for every case.
	ErrorCodeSetupTokenInvalid = "setup_token_invalid"
	// ErrorCodeTooManyDeviceLinks (HTTP 409): the member already holds
	// MaxOpenDeviceLinks open device links; they revoke one first.
	ErrorCodeTooManyDeviceLinks = "too_many_device_links"
	// ErrorCodeHTTPSRequired (HTTP 403): a password, code or token was sent
	// over plain HTTP beyond loopback (design §5.4); the server reads the
	// scheme from its configured public origin and trusted proxies.
	ErrorCodeHTTPSRequired = "https_required"
	// ErrorCodeAccountsRequireAuth (HTTP 403): conchd runs with
	// authentication off, so no account, visit, password or sign-in exists
	// to act on. Every joining, sign-in, password, TOTP, re-authentication
	// and guest route answers it then, as the ticket and session routes
	// answer their own codes.
	ErrorCodeAccountsRequireAuth = "accounts_require_auth"
)

// CredentialScope is what a credential may reach. The vocabulary is closed.
type CredentialScope string

const (
	// CredentialScopeFull: everything its principal may do. The scope of
	// every credential that exists today, and the default when absent.
	CredentialScopeFull CredentialScope = "full"
	// CredentialScopeSetup: only POST /v1/password/mine, the TOTP enrolment
	// routes and GET /v1/whoami, for the fifteen minutes after a setup
	// token is redeemed.
	CredentialScopeSetup CredentialScope = "setup"
)

// Valid reports whether s is a recognized scope.
func (s CredentialScope) Valid() bool {
	switch s {
	case CredentialScopeFull, CredentialScopeSetup:
		return true
	default:
		return false
	}
}

// LoginNext is what a sign-in response says happens next.
type LoginNext string

const (
	// LoginNextSignedIn: a credential was issued; the response carries it.
	LoginNextSignedIn LoginNext = "signed_in"
	// LoginNextTOTP: the password was right and the account is an
	// operator's; the response carries a challenge for POST /v1/login/totp.
	LoginNextTOTP LoginNext = "totp"
)

// Valid reports whether n is a recognized next step.
func (n LoginNext) Valid() bool {
	switch n {
	case LoginNextSignedIn, LoginNextTOTP:
		return true
	default:
		return false
	}
}

// ValidatePassword reports whether password is acceptable to set: valid
// UTF-8, MinPasswordLength to MaxPasswordLength characters and at most
// MaxPasswordBytes. Spaces and any script are fine; there is no composition
// rule. The server normalizes to NFC, checks the bounds again, and refuses
// a common or name-derived password with ErrorCodePasswordUnacceptable.
// Errors never echo the input.
func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return errors.New("schema: password must be valid UTF-8")
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("schema: password must be at most %d bytes", MaxPasswordBytes)
	}
	if n := utf8.RuneCountInString(password); n < MinPasswordLength || n > MaxPasswordLength {
		return fmt.Errorf("schema: password must be %d to %d characters", MinPasswordLength, MaxPasswordLength)
	}
	return nil
}

// validatePasswordBounds is the check on a password that is presented, not
// set: non-empty, valid UTF-8 and at most MaxPasswordBytes. A presented
// password that is too short for the policy is not refused here: no account
// has one, so it fails with ErrorCodeAuthFailed like any wrong password.
func validatePasswordBounds(field, password string) error {
	if password == "" {
		return fmt.Errorf("schema: %s is required", field)
	}
	if !utf8.ValidString(password) {
		return fmt.Errorf("schema: %s must be valid UTF-8", field)
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("schema: %s must be at most %d bytes", field, MaxPasswordBytes)
	}
	return nil
}

// ValidateTOTPCode reports whether code has the shape of a TOTP code:
// exactly TOTPDigits ASCII digits. The error never echoes the input.
func ValidateTOTPCode(code string) error {
	if len(code) != TOTPDigits {
		return fmt.Errorf("schema: a TOTP code is %d digits", TOTPDigits)
	}
	for _, c := range []byte(code) {
		if c < '0' || c > '9' {
			return fmt.Errorf("schema: a TOTP code is %d digits", TOTPDigits)
		}
	}
	return nil
}

// validateOpaqueCode reports whether code is prefix plus exactly 43
// characters of unpadded base64url: the shape of every secret the server
// mints. The error names what the code should be and never echoes it.
func validateOpaqueCode(code, prefix, what string) error {
	if len(code) != len(prefix)+43 || !strings.HasPrefix(code, prefix) {
		return fmt.Errorf("schema: malformed %s", what)
	}
	for _, c := range []byte(code[len(prefix):]) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return fmt.Errorf("schema: malformed %s", what)
		}
	}
	return nil
}

// ValidatePendingLoginChallenge reports whether challenge has the exact
// shape of a pending-login challenge.
func ValidatePendingLoginChallenge(challenge string) error {
	return validateOpaqueCode(challenge, PendingLoginPrefix, "login challenge")
}

// ValidateSetupToken reports whether token has the exact shape of a setup
// token.
func ValidateSetupToken(token string) error {
	return validateOpaqueCode(token, SetupTokenPrefix, "setup token")
}

// validateLoginName is the check on a name someone signs in with: present,
// and of a shape a principal name can have. A name that could never be a
// principal's is refused as malformed (HTTP 400), which reveals nothing,
// since no account has it either way.
func validateLoginName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("schema: name is required")
	}
	return ValidateDisplayName("name", name, MaxPrincipalNameLength)
}

// LoginRequestV1 is the body of POST /v1/login: username and password. It
// is open to anyone, rate-limited by address and by the typed name's
// failure window (design §5.2), and answers ErrorCodeAuthFailed for every
// failure. A guest handle is not a name anyone can sign in with.
type LoginRequestV1 struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Validate reports whether the name and password have the shapes of a name
// and a password; whether they are right is the server's answer.
func (r LoginRequestV1) Validate() error {
	if err := validateLoginName(r.Name); err != nil {
		return err
	}
	return validatePasswordBounds("password", r.Password)
}

// LoginResponseV1 is the response to POST /v1/login and POST /v1/login/totp,
// in two shapes picked by Next. With LoginNextSignedIn it carries the
// principal, the new credential and its plaintext token, shown only here;
// a member's credential lives as an invite credential does, an operator's
// for OperatorSessionAbsoluteSeconds with OperatorSessionIdleSeconds idle.
// With LoginNextTOTP it carries a challenge, which is the one thing a
// correct operator password yields: no credential, no principal. The
// challenge is bound to the account, the client address it was issued to
// and ChallengeExpiresAt, and is spent by its first completion or its
// MaxTOTPAttemptsPerChallenge wrong code. The TOTP completion route always
// answers LoginNextSignedIn.
type LoginResponseV1 struct {
	Next               LoginNext     `json:"next"`
	Principal          *PrincipalV0  `json:"principal,omitempty"`
	Credential         *CredentialV1 `json:"credential,omitempty"`
	Token              string        `json:"token,omitempty"`
	Challenge          string        `json:"challenge,omitempty"`
	ChallengeExpiresAt *Timestamp    `json:"challenge_expires_at,omitempty"`
}

// Validate reports whether the response has exactly the fields of its Next.
func (r LoginResponseV1) Validate() error {
	switch r.Next {
	case LoginNextSignedIn:
		if r.Principal == nil || r.Credential == nil || r.Token == "" {
			return errors.New("schema: a signed_in login response carries principal, credential and token")
		}
		if r.Challenge != "" || r.ChallengeExpiresAt != nil {
			return errors.New("schema: a signed_in login response carries no challenge")
		}
	case LoginNextTOTP:
		if r.Challenge == "" || r.ChallengeExpiresAt == nil {
			return errors.New("schema: a totp login response carries challenge and challenge_expires_at")
		}
		if err := ValidatePendingLoginChallenge(r.Challenge); err != nil {
			return err
		}
		if r.Principal != nil || r.Credential != nil || r.Token != "" {
			return errors.New("schema: a totp login response carries no principal, credential or token")
		}
	default:
		return fmt.Errorf("schema: login next must be one of: %s", strings.Join(Vocabularies["LoginNext"], ", "))
	}
	return nil
}

// TOTPLoginRequestV1 is the body of POST /v1/login/totp: the challenge from
// a LoginNextTOTP response and the authenticator's code.
type TOTPLoginRequestV1 struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

// Validate reports whether the challenge and code are well-formed.
func (r TOTPLoginRequestV1) Validate() error {
	if err := ValidatePendingLoginChallenge(r.Challenge); err != nil {
		return err
	}
	return ValidateTOTPCode(r.Code)
}

// SetPasswordRequestV1 is the body of POST /v1/password/mine: the caller's
// first password. An operator sends it on a setup-scoped credential and
// nothing else (ErrorCodeSetupOnHost otherwise); on that credential it
// replaces any password the operator already had, since the host-made
// setup token is the proof and operator recovery works through it. A
// member who predates passwords sends it on their existing session with
// Proof, a one-use enrolment link an operator made for them (purpose
// enrollment), since a long-lived bearer alone must not set a password
// (design §5.2). The proof is bound to the caller: one made for another
// principal, or of any other purpose, is ErrorCodeSignInLinkInvalid. A
// member made after passwords exist has one already
// (ErrorCodePasswordAlreadySet).
type SetPasswordRequestV1 struct {
	Password string `json:"password"`
	Proof    string `json:"proof,omitempty"`
}

// Validate reports whether the password passes ValidatePassword and the
// proof, when given, has a sign-in link code's shape.
func (r SetPasswordRequestV1) Validate() error {
	if err := ValidatePassword(r.Password); err != nil {
		return err
	}
	if r.Proof != "" {
		if err := ValidateSignInLinkCode(r.Proof); err != nil {
			return errors.New("schema: malformed enrolment proof")
		}
	}
	return nil
}

// ChangePasswordRequestV1 is the body of PUT /v1/password/mine: the current
// password and the new one. An operator gives Code, a current TOTP code, as
// well. On success every other credential of the principal, every open
// link for it and every pending login are revoked, and the credential the
// request came with is rotated: the response carries the new token.
type ChangePasswordRequestV1 struct {
	Current  string `json:"current"`
	Password string `json:"password"`
	Code     string `json:"code,omitempty"`
}

// Validate reports whether the current password has a password's bounds,
// the new one passes ValidatePassword and differs, and the code, when
// given, is a TOTP code's shape.
func (r ChangePasswordRequestV1) Validate() error {
	if err := validatePasswordBounds("current password", r.Current); err != nil {
		return err
	}
	if err := ValidatePassword(r.Password); err != nil {
		return err
	}
	if r.Password == r.Current {
		return errors.New("schema: the new password must differ from the current one")
	}
	if r.Code != "" {
		return ValidateTOTPCode(r.Code)
	}
	return nil
}

// ResetPasswordRequestV1 is the body of POST /v1/password/reset, open to
// anyone: a reset link's code (purpose reset, made by an operator for the
// member after checking who they are) and the new password. The code
// authorizes only this: it is not a sign-in. On success the link is spent,
// every credential, link and pending login of the member is revoked, and no
// credential is issued; the member signs in with POST /v1/login.
type ResetPasswordRequestV1 struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

// Validate reports whether the code has a sign-in link code's shape and the
// password passes ValidatePassword.
func (r ResetPasswordRequestV1) Validate() error {
	if err := ValidateSignInLinkCode(r.Code); err != nil {
		return err
	}
	return ValidatePassword(r.Password)
}

// PasswordUpdatedResponseV1 is the response to setting, changing or
// resetting a password. Revoked is how many other credentials of the
// principal the change revoked. Credential and Token are present when the
// request's own session survives, rotated: a change, and an enrolment on
// an ordinary session. They are absent after a reset, which issues no
// credential, and after an operator's setup, whose setup-scoped credential
// stays as it is until setup is complete.
type PasswordUpdatedResponseV1 struct {
	Credential *CredentialV1 `json:"credential,omitempty"`
	Token      string        `json:"token,omitempty"`
	Revoked    int           `json:"revoked"`
}

// ReauthRequestV1 is the body of POST /v1/reauth: the caller proves their
// password again, and an operator a current TOTP code too, before a
// sensitive action (design §5.3, §6.2). A wrong pair is an account failure
// in the name's window, like a wrong sign-in (review F2).
type ReauthRequestV1 struct {
	Password string `json:"password"`
	Code     string `json:"code,omitempty"`
}

// Validate reports whether the password has a password's bounds and the
// code, when given, a TOTP code's shape.
func (r ReauthRequestV1) Validate() error {
	if err := validatePasswordBounds("password", r.Password); err != nil {
		return err
	}
	if r.Code != "" {
		return ValidateTOTPCode(r.Code)
	}
	return nil
}

// ReauthResponseV1 is the response to a successful re-authentication: the
// instant until which the proof stands, RecentAuthLifetimeSeconds from
// now. The proof is recorded on the credential the request came with and
// on no other; a password change or reset, a sign-out everywhere, a
// disable, a restore or any bump of the principal's authentication
// generation ends it before then. GET /v1/whoami shows it as
// auth.recent_auth_until.
type ReauthResponseV1 struct {
	Until Timestamp `json:"until"`
}

// AuthStateV1 is how the caller is authenticated, in GET /v1/whoami and the
// setup-token response. Scope is the credential's. PasswordSet says the
// principal has a password; TOTPEnrolled, for an operator, that TOTP is
// enrolled (omitted for everyone else). SetupComplete is both, or
// PasswordSet alone for a member: a client shows setup incomplete until it
// is true. ExpiresAt and IdleExpiresAt are the credential's bounds, when it
// has them; RecentAuthUntil the standing re-authentication proof, when one
// stands. Absent from a server that predates passwords, and from a server
// running with authentication off.
type AuthStateV1 struct {
	Scope           CredentialScope `json:"scope"`
	PasswordSet     bool            `json:"password_set"`
	TOTPEnrolled    bool            `json:"totp_enrolled,omitempty"`
	SetupComplete   bool            `json:"setup_complete"`
	ExpiresAt       *Timestamp      `json:"expires_at,omitempty"`
	IdleExpiresAt   *Timestamp      `json:"idle_expires_at,omitempty"`
	RecentAuthUntil *Timestamp      `json:"recent_auth_until,omitempty"`
}

// Validate reports whether the scope is known and the idle bound, when
// both bounds are present, is not after the absolute one.
func (a AuthStateV1) Validate() error {
	if !a.Scope.Valid() {
		return fmt.Errorf("schema: auth scope must be one of: %s", strings.Join(Vocabularies["CredentialScope"], ", "))
	}
	if a.ExpiresAt != nil && a.IdleExpiresAt != nil && a.IdleExpiresAt.Time().After(a.ExpiresAt.Time()) {
		return errors.New("schema: auth idle_expires_at must not be after expires_at")
	}
	return nil
}

// TOTPEnrollmentV1 is the response to POST /v1/totp/enroll (no body), on a
// setup-scoped credential: the material an authenticator app needs, shown
// exactly once. Secret is the TOTPSecretBytes secret in base32 without
// padding; URI is the same as an otpauth:// URI, for a client that renders
// a QR code of its own. The enrolment is pending until POST
// /v1/totp/confirm proves a code; a second enrol replaces a pending one.
// The server keeps the secret encrypted under its host key and never
// shows it again.
type TOTPEnrollmentV1 struct {
	Secret        string `json:"secret"`
	Issuer        string `json:"issuer"`
	Account       string `json:"account"`
	Algorithm     string `json:"algorithm"`
	Digits        int    `json:"digits"`
	PeriodSeconds int    `json:"period_seconds"`
	URI           string `json:"uri"`
}

// ConfirmTOTPRequestV1 is the body of POST /v1/totp/confirm: a code from the
// authenticator, which proves the secret was entered. Success answers 204
// and completes the operator's setup.
type ConfirmTOTPRequestV1 struct {
	Code string `json:"code"`
}

// Validate reports whether the code has a TOTP code's shape.
func (r ConfirmTOTPRequestV1) Validate() error { return ValidateTOTPCode(r.Code) }

// RedeemSetupTokenRequestV1 is the body of POST /v1/setup/redeem, open to
// anyone who holds a setup token a host command made.
type RedeemSetupTokenRequestV1 struct {
	Token string `json:"token"`
}

// Validate reports whether the token has a setup token's shape.
func (r RedeemSetupTokenRequestV1) Validate() error { return ValidateSetupToken(r.Token) }

// RedeemSetupTokenResponseV1 is the response to redeeming a setup token: the
// operator it is for, a credential of scope setup that lives
// SetupCredentialLifetimeSeconds, its plaintext token shown only here, and
// what setup remains. The token is spent whether or not setup completes.
type RedeemSetupTokenResponseV1 struct {
	Principal  PrincipalV0  `json:"principal"`
	Credential CredentialV1 `json:"credential"`
	Token      string       `json:"token"`
	Auth       AuthStateV1  `json:"auth"`
}
