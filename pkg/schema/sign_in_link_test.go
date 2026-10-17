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
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
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
