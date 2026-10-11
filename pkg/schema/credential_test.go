package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestCredentialGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"credential-v1.json", func() any { return new(CredentialV1) }},
		{"credential-v1-revoked.json", func() any { return new(CredentialV1) }},
		{"create-credential-request-v1.json", func() any { return new(CreateCredentialRequestV1) }},
		{"create-credential-response-v1.json", func() any { return new(CreateCredentialResponseV1) }},
		{"rotate-credential-response-v1.json", func() any { return new(RotateCredentialResponseV1) }},
		{"list-credentials-response-v1.json", func() any { return new(ListCredentialsResponseV1) }},
		{"list-credentials-response-v1-empty.json", func() any { return new(ListCredentialsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestCredentialFixtureTokenShape(t *testing.T) {
	for _, file := range []string{"create-credential-response-v1.json", "rotate-credential-response-v1.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name from this test's own table
		if err != nil {
			t.Fatal(err)
		}
		var resp CreateCredentialResponseV1
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(resp.Token, CredentialTokenPrefix) || len(resp.Token) != CredentialTokenLength {
			t.Errorf("%s: token %q is not prefix+43 chars (%d)", file, resp.Token, CredentialTokenLength)
		}
	}
}

// TestCredentialV1HasNoSecretFields pins the exact JSON key set of the
// credential object, in both the fixtures and a fully populated value: no
// token, hash, or secret may ever appear on it.
func TestCredentialV1HasNoSecretFields(t *testing.T) {
	want := []string{"created_at", "expires_at", "id", "label", "principal_id", "revoked_at"}

	ts := NewTimestamp(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	full := CredentialV1{ID: 1, PrincipalID: 2, Label: "l", CreatedAt: ts, ExpiresAt: &ts, RevokedAt: &ts}
	raw, err := json.Marshal(full)
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
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("CredentialV1 keys = %v, want %v", got, want)
	}

	for _, file := range []string{"credential-v1.json", "credential-v1-revoked.json"} {
		b, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name from this test's own table
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if k != "id" && k != "principal_id" && k != "label" && k != "created_at" && k != "expires_at" && k != "revoked_at" {
				t.Errorf("%s has unexpected key %q", file, k)
			}
		}
	}
}

func TestCreateCredentialRequestValidate(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	future := NewTimestamp(now.Add(time.Hour))
	past := NewTimestamp(now.Add(-time.Hour))
	atNow := NewTimestamp(now)

	tests := []struct {
		name    string
		req     CreateCredentialRequestV1
		wantErr string
	}{
		{"label only", CreateCredentialRequestV1{Label: "ci"}, ""},
		{"future expiry", CreateCredentialRequestV1{Label: "ci", ExpiresAt: &future}, ""},
		{"label at limit", CreateCredentialRequestV1{Label: strings.Repeat("a", 100)}, ""},
		{"multibyte label at limit", CreateCredentialRequestV1{Label: strings.Repeat("é", 100)}, ""},
		{"empty label", CreateCredentialRequestV1{}, "blank"},
		{"blank label", CreateCredentialRequestV1{Label: " \t\n"}, "blank"},
		{"label too long", CreateCredentialRequestV1{Label: strings.Repeat("a", 101)}, "at most 100"},
		{"multibyte label too long", CreateCredentialRequestV1{Label: strings.Repeat("é", 101)}, "at most 100"},
		{"label with newline", CreateCredentialRequestV1{Label: "ci\nbuilds"}, "control characters"},
		{"label with bidi override", CreateCredentialRequestV1{Label: "ci\u202ebuilds"}, "bidi controls"},
		{"label with leading space", CreateCredentialRequestV1{Label: " ci"}, "whitespace"},
		{"expiry in past", CreateCredentialRequestV1{Label: "ci", ExpiresAt: &past}, "future"},
		{"expiry exactly now", CreateCredentialRequestV1{Label: "ci", ExpiresAt: &atNow}, "future"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate(now)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
