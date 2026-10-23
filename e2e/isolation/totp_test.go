package main

import (
	"encoding/base32"
	"testing"
	"time"
)

// TestTOTPVectors: the harness's authenticator gives RFC 6238's SHA-1 test
// vectors (Appendix B, the 20-byte ASCII secret "12345678901234567890", eight
// digits), and their last six digits at six.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, tc := range []struct {
		unix  int64
		eight string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		step := totpStep(time.Unix(tc.unix, 0))
		if got := totpAt(secret, step, 8); got != tc.eight {
			t.Errorf("T=%d: %s, want %s", tc.unix, got, tc.eight)
		}
		if got := totpAt(secret, step, 6); got != tc.eight[2:] {
			t.Errorf("T=%d, six digits: %s, want %s", tc.unix, got, tc.eight[2:])
		}
	}
}

// TestDecodeTOTPSecret: the secret is read padded or not, in either case.
func TestDecodeTOTPSecret(t *testing.T) {
	raw := []byte("12345678901234567890")
	enc := base32.StdEncoding.EncodeToString(raw)
	for _, s := range []string{enc, enc + "==", " " + enc + " "} {
		got, err := decodeTOTPSecret(s)
		if err != nil || string(got) != string(raw) {
			t.Errorf("decode %q = %q, %v", s, got, err)
		}
	}
	if _, err := decodeTOTPSecret("not base32!"); err == nil {
		t.Error("a secret that is not base32 was read")
	}
}

// TestAuthenticatorNeverRepeats: two codes in a row are of different steps.
func TestAuthenticatorNeverRepeats(t *testing.T) {
	a := &authenticator{secret: []byte("12345678901234567890")}
	first, err := a.code()
	if err != nil {
		t.Fatal(err)
	}
	step := a.last
	if _, err := a.code(); err != nil {
		t.Fatal(err)
	}
	if a.last <= step {
		t.Errorf("the second code is of step %d, not after %d", a.last, step)
	}
	if first == "" || len(first) != 6 {
		t.Errorf("code %q", first)
	}
}
