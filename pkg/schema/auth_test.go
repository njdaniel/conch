package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAuthGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"login-request-v1.json", func() any { return new(LoginRequestV1) }},
		{"login-response-v1.json", func() any { return new(LoginResponseV1) }},
		{"login-response-v1-totp.json", func() any { return new(LoginResponseV1) }},
		{"totp-login-request-v1.json", func() any { return new(TOTPLoginRequestV1) }},
		{"set-password-request-v1.json", func() any { return new(SetPasswordRequestV1) }},
		{"set-password-request-v1-proof.json", func() any { return new(SetPasswordRequestV1) }},
		{"change-password-request-v1.json", func() any { return new(ChangePasswordRequestV1) }},
		{"change-password-request-v1-totp.json", func() any { return new(ChangePasswordRequestV1) }},
		{"reset-password-request-v1.json", func() any { return new(ResetPasswordRequestV1) }},
		{"password-updated-response-v1.json", func() any { return new(PasswordUpdatedResponseV1) }},
		{"password-updated-response-v1-reset.json", func() any { return new(PasswordUpdatedResponseV1) }},
		{"reauth-request-v1.json", func() any { return new(ReauthRequestV1) }},
		{"reauth-request-v1-totp.json", func() any { return new(ReauthRequestV1) }},
		{"reauth-response-v1.json", func() any { return new(ReauthResponseV1) }},
		{"auth-state-v1.json", func() any { return new(AuthStateV1) }},
		{"auth-state-v1-setup.json", func() any { return new(AuthStateV1) }},
		{"auth-state-v1-operator.json", func() any { return new(AuthStateV1) }},
		{"totp-enrollment-v1.json", func() any { return new(TOTPEnrollmentV1) }},
		{"confirm-totp-request-v1.json", func() any { return new(ConfirmTOTPRequestV1) }},
		{"redeem-setup-token-request-v1.json", func() any { return new(RedeemSetupTokenRequestV1) }},
		{"redeem-setup-token-response-v1.json", func() any { return new(RedeemSetupTokenResponseV1) }},
		{"error-auth-failed.json", func() any { return new(Error) }},
		{"error-reauth-required.json", func() any { return new(Error) }},
		{"error-password-required.json", func() any { return new(Error) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// decodeFixture decodes the named fixture from testdata into v, refusing
// unknown fields.
func decodeFixture(t *testing.T, file string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- a fixture name from this package's own test table
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
}

// The fixtures of the two login shapes validate as what they claim to be.
func TestLoginResponseFixturesValidate(t *testing.T) {
	for file, next := range map[string]LoginNext{"login-response-v1.json": LoginNextSignedIn, "login-response-v1-totp.json": LoginNextTOTP} {
		var r LoginResponseV1
		decodeFixture(t, file, &r)
		if r.Next != next {
			t.Errorf("%s: next = %s, want %s", file, r.Next, next)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("%s: Validate = %v", file, err)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name, password string
		wantErr        string
	}{
		{"a passphrase", goodPass, ""},
		{"the least", strings.Repeat("a", MinPasswordLength), ""},
		{"one short", strings.Repeat("a", MinPasswordLength-1), "15 to 128"},
		{"the most", strings.Repeat("a", MaxPasswordLength), ""},
		{"one past the most", strings.Repeat("a", MaxPasswordLength+1), "15 to 128"},
		{"empty", "", "15 to 128"},
		{"spaces and another script", "山田 太郎 の 合言葉 は 長い です よ", ""},
		{"the most in four-byte characters", strings.Repeat("\U0001F41A", MaxPasswordLength), ""},
		{"past the byte bound", strings.Repeat("\U0001F41A", MaxPasswordLength+1), "512 bytes"},
		{"invalid UTF-8", "correct horse\xff battery staple", "UTF-8"},
		{"leading and trailing spaces are kept", "  correct horse battery  ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.password)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("ValidatePassword = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("ValidatePassword = %v, want an error containing %q", err, tt.wantErr)
			}
			if err != nil && tt.password != "" && strings.Contains(err.Error(), tt.password) {
				t.Errorf("the error echoes the password: %v", err)
			}
		})
	}
}

func TestValidateTOTPCode(t *testing.T) {
	tests := []struct {
		name, code string
		ok         bool
	}{
		{"six digits", "123456", true},
		{"leading zeros", "000000", true},
		{"five", "12345", false},
		{"seven", "1234567", false},
		{"a letter", "12345a", false},
		{"a space", "123 45", false},
		{"full-width digits", "１２３４５６", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateTOTPCode(tt.code); (err == nil) != tt.ok {
				t.Errorf("ValidateTOTPCode = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

// Each opaque secret has its own prefix and is refused by every other
// route's check: a sign-in link is not a challenge, a challenge is not a
// setup token, and a credential token is neither.
func TestOpaqueCodeShapes(t *testing.T) {
	suffix := strings.Repeat("A", 43)
	chal := PendingLoginPrefix + suffix
	setup := SetupTokenPrefix + suffix
	tests := []struct {
		name string
		fn   func(string) error
		code string
		ok   bool
	}{
		{"challenge", ValidatePendingLoginChallenge, chal, true},
		{"challenge, base64url alphabet", ValidatePendingLoginChallenge, PendingLoginPrefix + strings.Repeat("a-_9", 10) + "Zz0", true},
		{"challenge short", ValidatePendingLoginChallenge, PendingLoginPrefix + "AAAA", false},
		{"challenge long", ValidatePendingLoginChallenge, chal + "A", false},
		{"challenge with padding", ValidatePendingLoginChallenge, PendingLoginPrefix + strings.Repeat("A", 42) + "=", false},
		{"a setup token as a challenge", ValidatePendingLoginChallenge, setup, false},
		{"a sign-in link as a challenge", ValidatePendingLoginChallenge, SignInLinkCodePrefix + suffix, false},
		{"a credential token as a challenge", ValidatePendingLoginChallenge, CredentialTokenPrefix + suffix, false},
		{"setup token", ValidateSetupToken, setup, true},
		{"a challenge as a setup token", ValidateSetupToken, chal, false},
		{"an invite code as a setup token", ValidateSetupToken, InviteCodePrefix + suffix, false},
		{"empty", ValidateSetupToken, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn(tt.code)
			if (err == nil) != tt.ok {
				t.Fatalf("got %v, want ok %v", err, tt.ok)
			}
			if err != nil && tt.code != "" && strings.Contains(err.Error(), tt.code) {
				t.Errorf("the error echoes the code: %v", err)
			}
		})
	}
	// Every secret starts "conch_" and then names its kind; no two kinds
	// share a prefix, and since every kind is its prefix plus 43
	// characters, no code of one kind has the length of another.
	prefixes := []string{CredentialTokenPrefix, InviteCodePrefix, SignInLinkCodePrefix, WSTicketPrefix, PendingLoginPrefix, SetupTokenPrefix}
	for i, a := range prefixes {
		for j, b := range prefixes {
			if i != j && (a == b || (strings.HasPrefix(a, b) && len(a) == len(b))) {
				t.Errorf("prefixes %q and %q are not distinct", a, b)
			}
		}
	}
	if PendingLoginLength != len(chal) || SetupTokenLength != len(setup) {
		t.Errorf("lengths %d, %d; want %d, %d", PendingLoginLength, SetupTokenLength, len(chal), len(setup))
	}
}

func TestAuthRequestsValidate(t *testing.T) {
	sil := SignInLinkCodePrefix + strings.Repeat("A", 43)
	inv := InviteCodePrefix + strings.Repeat("A", 43)
	chal := PendingLoginPrefix + strings.Repeat("A", 43)
	long := strings.Repeat("a", MaxPasswordBytes+1)
	newPass := "a longer and newer passphrase"
	tests := []struct {
		name    string
		req     interface{ Validate() error }
		wantErr string
	}{
		{"login", LoginRequestV1{Name: "pat", Password: goodPass}, ""},
		{"login with a short password is not a 400", LoginRequestV1{Name: "pat", Password: "x"}, ""},
		{"login, no name", LoginRequestV1{Password: goodPass}, "name is required"},
		{"login, blank name", LoginRequestV1{Name: "  ", Password: goodPass}, "name is required"},
		{"login, a name no principal can have", LoginRequestV1{Name: "pat\u202e", Password: goodPass}, "bidi"},
		{"login, a name too long", LoginRequestV1{Name: strings.Repeat("a", MaxPrincipalNameLength+1), Password: goodPass}, "at most"},
		{"login, no password", LoginRequestV1{Name: "pat"}, "password is required"},
		{"login, a password past the byte bound", LoginRequestV1{Name: "pat", Password: long}, "512 bytes"},
		{"totp login", TOTPLoginRequestV1{Challenge: chal, Code: "123456"}, ""},
		{"totp login, bad code", TOTPLoginRequestV1{Challenge: chal, Code: "12345"}, "digits"},
		{"totp login, a link for a challenge", TOTPLoginRequestV1{Challenge: sil, Code: "123456"}, "challenge"},
		{"set", SetPasswordRequestV1{Password: goodPass}, ""},
		{"set with a proof", SetPasswordRequestV1{Password: goodPass, Proof: sil}, ""},
		{"set with an invite code for a proof", SetPasswordRequestV1{Password: goodPass, Proof: inv}, "proof"},
		{"set, short", SetPasswordRequestV1{Password: "short"}, "15 to 128"},
		{"change", ChangePasswordRequestV1{Current: goodPass, Password: newPass}, ""},
		{"change with totp", ChangePasswordRequestV1{Current: goodPass, Password: newPass, Code: "123456"}, ""},
		{"change, no current", ChangePasswordRequestV1{Password: newPass}, "current password is required"},
		{"change, same", ChangePasswordRequestV1{Current: goodPass, Password: goodPass}, "differ"},
		{"change, short new", ChangePasswordRequestV1{Current: goodPass, Password: "short"}, "15 to 128"},
		{"change, bad code", ChangePasswordRequestV1{Current: goodPass, Password: newPass, Code: "abc"}, "digits"},
		{"reset", ResetPasswordRequestV1{Code: sil, Password: goodPass}, ""},
		{"reset with an invite code", ResetPasswordRequestV1{Code: inv, Password: goodPass}, "sign-in link"},
		{"reset, short", ResetPasswordRequestV1{Code: sil, Password: "short"}, "15 to 128"},
		{"reauth", ReauthRequestV1{Password: goodPass}, ""},
		{"reauth with totp", ReauthRequestV1{Password: goodPass, Code: "123456"}, ""},
		{"reauth, no password", ReauthRequestV1{}, "password is required"},
		{"reauth, bad code", ReauthRequestV1{Password: goodPass, Code: "1"}, "digits"},
		{"confirm totp", ConfirmTOTPRequestV1{Code: "123456"}, ""},
		{"confirm totp, bad", ConfirmTOTPRequestV1{Code: ""}, "digits"},
		{"redeem setup", RedeemSetupTokenRequestV1{Token: SetupTokenPrefix + strings.Repeat("A", 43)}, ""},
		{"redeem setup with a challenge", RedeemSetupTokenRequestV1{Token: chal}, "setup token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate = %v, want an error containing %q", err, tt.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), goodPass) || strings.Contains(err.Error(), newPass)) {
				t.Errorf("the error echoes a password: %v", err)
			}
		})
	}
}

func TestLoginResponseV1Validate(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	exp := NewTimestamp(now.Add(5 * time.Minute))
	chal := PendingLoginPrefix + strings.Repeat("A", 43)
	p := PrincipalV0{ID: 12, Kind: PrincipalHuman, Name: "pat", CreatedAt: now}
	c := CredentialV1{ID: 31, PrincipalID: 12, Label: "browser", CreatedAt: NewTimestamp(now)}
	tests := []struct {
		name    string
		resp    LoginResponseV1
		wantErr string
	}{
		{"signed in", LoginResponseV1{Next: LoginNextSignedIn, Principal: &p, Credential: &c, Token: "t"}, ""},
		{"totp", LoginResponseV1{Next: LoginNextTOTP, Challenge: chal, ChallengeExpiresAt: &exp}, ""},
		{"signed in without a token", LoginResponseV1{Next: LoginNextSignedIn, Principal: &p, Credential: &c}, "carries principal, credential and token"},
		{"signed in with a challenge", LoginResponseV1{Next: LoginNextSignedIn, Principal: &p, Credential: &c, Token: "t", Challenge: chal}, "no challenge"},
		{"totp without an expiry", LoginResponseV1{Next: LoginNextTOTP, Challenge: chal}, "challenge_expires_at"},
		{"totp with a malformed challenge", LoginResponseV1{Next: LoginNextTOTP, Challenge: "nope", ChallengeExpiresAt: &exp}, "malformed"},
		{"totp with a principal", LoginResponseV1{Next: LoginNextTOTP, Challenge: chal, ChallengeExpiresAt: &exp, Principal: &p}, "no principal"},
		{"unknown next", LoginResponseV1{Next: "passkey"}, "next must be one of"},
		{"empty", LoginResponseV1{}, "next must be one of"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.resp.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestAuthStateV1Validate(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *Timestamp {
		ts := NewTimestamp(now.Add(d))
		return &ts
	}
	tests := []struct {
		name    string
		state   AuthStateV1
		wantErr string
	}{
		{"full", AuthStateV1{Scope: CredentialScopeFull, PasswordSet: true, SetupComplete: true}, ""},
		{"setup", AuthStateV1{Scope: CredentialScopeSetup, ExpiresAt: at(15 * time.Minute)}, ""},
		{"bounded", AuthStateV1{Scope: CredentialScopeFull, ExpiresAt: at(12 * time.Hour), IdleExpiresAt: at(time.Hour)}, ""},
		{"unknown scope", AuthStateV1{Scope: "admin"}, "scope"},
		{"idle after expiry", AuthStateV1{Scope: CredentialScopeFull, ExpiresAt: at(time.Hour), IdleExpiresAt: at(2 * time.Hour)}, "idle_expires_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.state.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// The design's numbers (ADR-009 §5–6): five-minute challenges with five
// tries, ten-minute re-authentication, a twelve-hour operator session idle
// after an hour, a ten-minute setup token and a fifteen-minute setup
// credential, RFC 6238 defaults, and three open device links.
func TestAuthConstants(t *testing.T) {
	if PendingLoginLifetimeSeconds != 300 || MaxTOTPAttemptsPerChallenge != 5 || RecentAuthLifetimeSeconds != 600 {
		t.Errorf("challenge %ds, %d tries, reauth %ds", PendingLoginLifetimeSeconds, MaxTOTPAttemptsPerChallenge, RecentAuthLifetimeSeconds)
	}
	if OperatorSessionAbsoluteSeconds != 43200 || OperatorSessionIdleSeconds != 3600 {
		t.Errorf("operator session %ds, idle %ds", OperatorSessionAbsoluteSeconds, OperatorSessionIdleSeconds)
	}
	if SetupTokenLifetimeSeconds != 600 || SetupCredentialLifetimeSeconds != 900 {
		t.Errorf("setup token %ds, credential %ds", SetupTokenLifetimeSeconds, SetupCredentialLifetimeSeconds)
	}
	if TOTPSecretBytes != 20 || TOTPDigits != 6 || TOTPPeriodSeconds != 30 || TOTPWindowSteps != 1 || TOTPAlgorithm != "SHA1" {
		t.Errorf("TOTP %d bytes, %d digits, %ds, window %d, %s", TOTPSecretBytes, TOTPDigits, TOTPPeriodSeconds, TOTPWindowSteps, TOTPAlgorithm)
	}
	if MinPasswordLength != 15 || MaxPasswordLength != 128 || MaxPasswordBytes != 512 || MaxOpenDeviceLinks != 3 {
		t.Errorf("password %d to %d, %d bytes; %d device links", MinPasswordLength, MaxPasswordLength, MaxPasswordBytes, MaxOpenDeviceLinks)
	}
	if ErrorCodeAuthFailed != "auth_failed" || ErrorCodeReauthRequired != "reauth_required" || ErrorCodeSetupOnHost != "setup_on_host" ||
		ErrorCodePasswordRequired != "password_required" || ErrorCodeAccountsRequireAuth != "accounts_require_auth" {
		t.Error("an authentication error code changed")
	}
}

// secretKeys are the json keys that name a secret. jsonTagKeys lists every
// json key a struct declares, omitempty ones included, so a secret that
// would be sent only when set is found too.
var secretKeys = []string{"password", "hash", "secret", "code", "token", "challenge"}

func jsonTagKeys(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s has no json tag", typ.Name(), typ.Field(i).Name)
		}
		keys = append(keys, strings.Split(tag, ",")[0])
	}
	return keys
}

// assertNoSecretKeys fails if any json key of v is a secret key, except
// the ones allowed, which the shape is documented to carry.
func assertNoSecretKeys(t *testing.T, v any, allowed ...string) {
	t.Helper()
	for _, k := range jsonTagKeys(t, v) {
		if slices.Contains(secretKeys, k) && !slices.Contains(allowed, k) {
			t.Errorf("%T carries the secret key %q", v, k)
		}
	}
}

// No authentication shape carries a password, a hash, a TOTP secret, a
// code, a token or a challenge, except the ones documented to: the
// responses that issue a secret exactly once, and the request bodies that
// must present one. Each exception is named here.
func TestAuthShapesCarryNoSecret(t *testing.T) {
	// Answers that carry no secret at all.
	for _, v := range []any{AuthStateV1{}, ReauthResponseV1{}, TOTPLoginRequestV1{}, ConfirmTOTPRequestV1{}} {
		switch v.(type) {
		case TOTPLoginRequestV1:
			assertNoSecretKeys(t, v, "challenge", "code")
		case ConfirmTOTPRequestV1:
			assertNoSecretKeys(t, v, "code")
		default:
			assertNoSecretKeys(t, v)
		}
	}
	// Responses that issue a secret, shown exactly once.
	assertNoSecretKeys(t, LoginResponseV1{}, "token", "challenge")
	assertNoSecretKeys(t, TOTPEnrollmentV1{}, "secret")
	assertNoSecretKeys(t, RedeemSetupTokenResponseV1{}, "token")
	assertNoSecretKeys(t, PasswordUpdatedResponseV1{}, "token")
	// Request bodies that present a password, a code or a token.
	assertNoSecretKeys(t, LoginRequestV1{}, "password")
	assertNoSecretKeys(t, SetPasswordRequestV1{}, "password")
	assertNoSecretKeys(t, ChangePasswordRequestV1{}, "password", "code")
	assertNoSecretKeys(t, ResetPasswordRequestV1{}, "code", "password")
	assertNoSecretKeys(t, ReauthRequestV1{}, "password", "code")
	assertNoSecretKeys(t, RedeemSetupTokenRequestV1{}, "token")
	// The credential shapes these responses embed carry none (a token is
	// never on a CredentialV1), and nor do the device link shapes.
	for _, v := range []any{CredentialV1{}, OwnCredentialV1{}, SignInLinkV1{}, SignInLinkPreviewV1{}, CreateDeviceLinkRequestV1{}, ListOwnSignInLinksResponseV1{}} {
		assertNoSecretKeys(t, v)
	}
}
