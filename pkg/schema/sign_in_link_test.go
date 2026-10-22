package schema

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSignInLinkGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"sign-in-link-v1.json", func() any { return new(SignInLinkV1) }},
		{"create-sign-in-link-request-v1.json", func() any { return new(CreateSignInLinkRequestV1) }},
		{"create-sign-in-link-request-v1-default.json", func() any { return new(CreateSignInLinkRequestV1) }},
		{"create-sign-in-link-response-v1.json", func() any { return new(CreateSignInLinkResponseV1) }},
		{"sign-in-link-preview-request-v1.json", func() any { return new(SignInLinkPreviewRequestV1) }},
		{"sign-in-link-preview-v1.json", func() any { return new(SignInLinkPreviewV1) }},
		{"redeem-sign-in-link-request-v1.json", func() any { return new(RedeemSignInLinkRequestV1) }},
		{"redeem-sign-in-link-response-v1.json", func() any { return new(RedeemSignInLinkResponseV1) }},
		// Purposes, device links and the member's own list (issue #704).
		{"sign-in-link-v1-reset.json", func() any { return new(SignInLinkV1) }},
		{"sign-in-link-v1-device.json", func() any { return new(SignInLinkV1) }},
		{"create-sign-in-link-request-v1-reset.json", func() any { return new(CreateSignInLinkRequestV1) }},
		{"sign-in-link-preview-v1-reset.json", func() any { return new(SignInLinkPreviewV1) }},
		{"create-device-link-request-v1.json", func() any { return new(CreateDeviceLinkRequestV1) }},
		{"create-device-link-request-v1-default.json", func() any { return new(CreateDeviceLinkRequestV1) }},
		{"list-own-sign-in-links-response-v1.json", func() any { return new(ListOwnSignInLinksResponseV1) }},
		{"list-own-sign-in-links-response-v1-empty.json", func() any { return new(ListOwnSignInLinksResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// A purpose picks the link's lifetimes; the operator route makes sign-in,
// reset and enrolment links and no other; and an unknown purpose is
// refused rather than read as a sign-in.
func TestSignInLinkPurposes(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *Timestamp {
		ts := NewTimestamp(now.Add(d))
		return &ts
	}
	tests := []struct {
		name    string
		req     CreateSignInLinkRequestV1
		wantErr string
		purpose SignInLinkPurpose
		expiry  time.Time
	}{
		{"no purpose is a sign-in", CreateSignInLinkRequestV1{}, "", SignInLinkPurposeSignIn, now.Add(24 * time.Hour)},
		{"sign-in", CreateSignInLinkRequestV1{Purpose: SignInLinkPurposeSignIn}, "", SignInLinkPurposeSignIn, now.Add(24 * time.Hour)},
		{"reset", CreateSignInLinkRequestV1{Purpose: SignInLinkPurposeReset}, "", SignInLinkPurposeReset, now.Add(10 * time.Minute)},
		{"enrollment", CreateSignInLinkRequestV1{Purpose: SignInLinkPurposeEnrollment}, "", SignInLinkPurposeEnrollment, now.Add(10 * time.Minute)},
		{"reset for an hour", CreateSignInLinkRequestV1{Purpose: SignInLinkPurposeReset, ExpiresAt: at(time.Hour)}, "", SignInLinkPurposeReset, now.Add(time.Hour)},
		{"reset past an hour", CreateSignInLinkRequestV1{Purpose: SignInLinkPurposeReset, ExpiresAt: at(time.Hour + time.Millisecond)}, "within 1h", "", time.Time{}},
		{"sign-in for a week", CreateSignInLinkRequestV1{ExpiresAt: at(7 * 24 * time.Hour)}, "", SignInLinkPurposeSignIn, now.Add(7 * 24 * time.Hour)},
		{"device is the member's own", CreateSignInLinkRequestV1{Purpose: SignInLinkPurposeDevice}, "not made on this route", "", time.Time{}},
		{"conversion is the guest route's", CreateSignInLinkRequestV1{Purpose: SignInLinkPurposeConversion}, "not made on this route", "", time.Time{}},
		{"unknown", CreateSignInLinkRequestV1{Purpose: "passkey"}, "purpose must be one of", "", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate(now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Validate = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if got := tt.req.EffectivePurpose(); got != tt.purpose {
				t.Errorf("EffectivePurpose = %s, want %s", got, tt.purpose)
			}
			if got := tt.req.EffectiveExpiry(now); !got.Equal(tt.expiry) {
				t.Errorf("EffectiveExpiry = %v, want %v", got, tt.expiry)
			}
		})
	}
	for _, p := range []SignInLinkPurpose{SignInLinkPurposeDevice, SignInLinkPurposeReset, SignInLinkPurposeEnrollment, SignInLinkPurposeConversion} {
		if def, max := p.Lifetimes(); def != DefaultRestrictedLinkLifetime || max != MaxRestrictedLinkLifetime {
			t.Errorf("%s lifetimes = %v, %v", p, def, max)
		}
	}
}

func TestCreateDeviceLinkRequestV1(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *Timestamp {
		ts := NewTimestamp(now.Add(d))
		return &ts
	}
	tests := []struct {
		name    string
		req     CreateDeviceLinkRequestV1
		wantErr string
		expiry  time.Time
	}{
		{"default", CreateDeviceLinkRequestV1{}, "", now.Add(10 * time.Minute)},
		{"a label", CreateDeviceLinkRequestV1{Label: "phone"}, "", now.Add(10 * time.Minute)},
		{"an hour", CreateDeviceLinkRequestV1{ExpiresAt: at(time.Hour)}, "", now.Add(time.Hour)},
		{"past an hour", CreateDeviceLinkRequestV1{ExpiresAt: at(time.Hour + time.Second)}, "within 1h", time.Time{}},
		{"past", CreateDeviceLinkRequestV1{ExpiresAt: at(-time.Second)}, "future", time.Time{}},
		{"blank label", CreateDeviceLinkRequestV1{Label: " "}, "blank", time.Time{}},
		{"long label", CreateDeviceLinkRequestV1{Label: strings.Repeat("a", MaxCredentialLabelLength+1)}, "at most", time.Time{}},
		{"control in the label", CreateDeviceLinkRequestV1{Label: "ph\x1bone"}, "control", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate(now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Validate = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if got := tt.req.EffectiveExpiry(now); !got.Equal(tt.expiry) {
				t.Errorf("EffectiveExpiry = %v, want %v", got, tt.expiry)
			}
		})
	}
}

func jsonKeys(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	return strings.Join(got, ",")
}

// Neither the link nor its preview may ever carry a code, a hash or more of
// the account than its name.
func TestSignInLinkShapesCarryNoSecret(t *testing.T) {
	if got, want := jsonKeys(t, SignInLinkV1{}), "created_at,created_by,expires_at,id,principal_id"; got != want {
		t.Errorf("SignInLinkV1 keys = %s, want %s", got, want)
	}
	if got, want := jsonKeys(t, SignInLinkPreviewV1{}), "name"; got != want {
		t.Errorf("SignInLinkPreviewV1 keys = %s, want %s", got, want)
	}
}

func TestValidateSignInLinkCode(t *testing.T) {
	good := SignInLinkCodePrefix + strings.Repeat("A", 43)
	tests := []struct {
		name, code string
		ok         bool
	}{
		{"well-formed", good, true},
		{"base64url alphabet", SignInLinkCodePrefix + strings.Repeat("a-_9", 10) + "Zz0", true},
		{"empty", "", false},
		{"short", SignInLinkCodePrefix + "AAAA", false},
		{"long", good + "A", false},
		{"an invite code", InviteCodePrefix + strings.Repeat("A", 43), false},
		{"a credential token", CredentialTokenPrefix + strings.Repeat("A", 47), false},
		{"padding", SignInLinkCodePrefix + strings.Repeat("A", 42) + "=", false},
		{"a space", SignInLinkCodePrefix + strings.Repeat("A", 42) + " ", false},
		{"non-ASCII", SignInLinkCodePrefix + strings.Repeat("A", 41) + "é", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSignInLinkCode(tt.code)
			if (err == nil) != tt.ok {
				t.Fatalf("ValidateSignInLinkCode = %v, want ok %v", err, tt.ok)
			}
			if err != nil && tt.code != "" && strings.Contains(err.Error(), tt.code) {
				t.Errorf("the error echoes the code: %v", err)
			}
			if (SignInLinkPreviewRequestV1{Code: tt.code}.Validate() == nil) != tt.ok || (RedeemSignInLinkRequestV1{Code: tt.code}.Validate() == nil) != tt.ok {
				t.Errorf("the request validators disagree with ValidateSignInLinkCode")
			}
		})
	}
	if SignInLinkCodeLength != len(good) {
		t.Errorf("SignInLinkCodeLength = %d, want %d", SignInLinkCodeLength, len(good))
	}
}

func TestCreateSignInLinkRequestV1(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *Timestamp {
		ts := NewTimestamp(now.Add(d))
		return &ts
	}
	tests := []struct {
		name    string
		req     CreateSignInLinkRequestV1
		wantErr string
		expiry  time.Time
	}{
		{"default", CreateSignInLinkRequestV1{}, "", now.Add(24 * time.Hour)},
		{"an hour", CreateSignInLinkRequestV1{ExpiresAt: at(time.Hour)}, "", now.Add(time.Hour)},
		{"the most", CreateSignInLinkRequestV1{ExpiresAt: at(MaxSignInLinkLifetime)}, "", now.Add(MaxSignInLinkLifetime)},
		{"past the most", CreateSignInLinkRequestV1{ExpiresAt: at(MaxSignInLinkLifetime + time.Millisecond)}, "within", time.Time{}},
		{"now", CreateSignInLinkRequestV1{ExpiresAt: at(0)}, "future", time.Time{}},
		{"past", CreateSignInLinkRequestV1{ExpiresAt: at(-time.Hour)}, "future", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate(now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Validate = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if got := tt.req.EffectiveExpiry(now); !got.Equal(tt.expiry) {
				t.Errorf("EffectiveExpiry = %v, want %v", got, tt.expiry)
			}
		})
	}
}

func TestSignInLinkErrorCode(t *testing.T) {
	if ErrorCodeSignInLinkInvalid != "sign_in_link_invalid" || ErrorCodeSignInLinkNotFound != "sign_in_link_not_found" {
		t.Errorf("codes = %q, %q", ErrorCodeSignInLinkInvalid, ErrorCodeSignInLinkNotFound)
	}
	if SignInLinkCodePrefix == InviteCodePrefix || strings.HasPrefix(SignInLinkCodePrefix, InviteCodePrefix) || strings.HasPrefix(InviteCodePrefix, SignInLinkCodePrefix) {
		t.Errorf("the sign-in link prefix %q must differ from the invite prefix %q", SignInLinkCodePrefix, InviteCodePrefix)
	}
}
