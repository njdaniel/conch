package server

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// testTOTPKey is a fixed key for the tests that need one.
func testTOTPKey(t testing.TB) *TOTPKey {
	t.Helper()
	k, err := ParseTOTPKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, TOTPKeyBytes)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// rfcSecret is the secret of RFC 4226 Appendix D and RFC 6238 Appendix B for
// SHA-1.
var rfcSecret = []byte("12345678901234567890")

// The published HOTP values (RFC 4226 Appendix D), six digits.
func TestHOTPVectors(t *testing.T) {
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for counter, code := range want {
		if got := hotp(rfcSecret, uint64(counter), 6); got != code {
			t.Errorf("HOTP(%d) = %s, want %s", counter, got, code)
		}
	}
}

// The published TOTP values for SHA-1 (RFC 6238 Appendix B), eight digits
// as the RFC prints them, at 30-second steps.
func TestTOTPVectors(t *testing.T) {
	tests := []struct {
		unix int64
		code string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, tt := range tests {
		step := totpStep(time.Unix(tt.unix, 0))
		if got := hotp(rfcSecret, uint64(step), 8); got != tt.code { // #nosec G115 -- every vector's time is positive
			t.Errorf("TOTP at %d = %s, want %s", tt.unix, got, tt.code)
		}
	}
	// The six digits a code here has are the last six of the RFC's eight.
	if got := totpCodeAt(rfcSecret, totpStep(time.Unix(59, 0))); got != "287082" {
		t.Errorf("six-digit TOTP at 59 = %s, want 287082", got)
	}
}

// The window is one step either side of now, at the edges of a step too; a
// step at or before the last accepted one never matches (replay).
func TestMatchTOTPWindow(t *testing.T) {
	// The first instant of a step.
	start := time.Unix(1_700_000_010/schema.TOTPPeriodSeconds*schema.TOTPPeriodSeconds, 0)
	cur := totpStep(start)
	tests := []struct {
		name     string
		now      time.Time
		codeStep int64
		last     int64
		ok       bool
	}{
		{"the current step", start, cur, 0, true},
		{"the step before", start, cur - 1, 0, true},
		{"the step after", start, cur + 1, 0, true},
		{"two steps before", start, cur - 2, 0, false},
		{"two steps after", start, cur + 2, 0, false},
		{"the last instant of the step", start.Add(29*time.Second + 999*time.Millisecond), cur - 1, 0, true},
		{"the first instant of the next step", start.Add(30 * time.Second), cur - 1, 0, false},
		{"the step already used", start, cur, cur, false},
		{"a step before the one used", start, cur - 1, cur, false},
		{"the step after the one used", start, cur + 1, cur, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step, ok := matchTOTP(rfcSecret, totpCodeAt(rfcSecret, tt.codeStep), tt.now, tt.last)
			if ok != tt.ok || (ok && step != tt.codeStep) {
				t.Errorf("matchTOTP = %d, %v; want %d, %v", step, ok, tt.codeStep, tt.ok)
			}
		})
	}
	if _, ok := matchTOTP(rfcSecret, "000000", start, 0); ok && totpCodeAt(rfcSecret, cur) != "000000" {
		t.Error("a made-up code matched")
	}
}

// A sealed secret opens only with its key, for its principal, untampered.
func TestSealOpenTOTP(t *testing.T) {
	k := testTOTPKey(t)
	other, err := ParseTOTPKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, TOTPKeyBytes)))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := k.sealTOTP(4, rfcSecret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, rfcSecret) {
		t.Fatal("the sealed secret holds the secret")
	}
	if again, _ := k.sealTOTP(4, rfcSecret); bytes.Equal(again, sealed) {
		t.Error("two seals of one secret are the same bytes: the nonce is not fresh")
	}
	if got, err := k.openTOTP(4, sealed); err != nil || !bytes.Equal(got, rfcSecret) {
		t.Fatalf("open = %q, %v", got, err)
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	var nilKey *TOTPKey
	for _, tt := range []struct {
		name string
		open func() ([]byte, error)
		want error
	}{
		{"another principal's", func() ([]byte, error) { return k.openTOTP(5, sealed) }, errTOTPUnsealable},
		{"another key", func() ([]byte, error) { return other.openTOTP(4, sealed) }, errTOTPUnsealable},
		{"tampered", func() ([]byte, error) { return k.openTOTP(4, tampered) }, errTOTPUnsealable},
		{"truncated", func() ([]byte, error) { return k.openTOTP(4, sealed[:5]) }, errTOTPUnsealable},
		{"the old placeholder byte", func() ([]byte, error) { return k.openTOTP(4, []byte{0}) }, errTOTPUnsealable},
		{"no key", func() ([]byte, error) { return nilKey.openTOTP(4, sealed) }, errTOTPKeyMissing},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := tt.open(); !errors.Is(err, tt.want) || got != nil {
				t.Errorf("open = %q, %v; want %v", got, err, tt.want)
			}
		})
	}
	if _, err := nilKey.sealTOTP(4, rfcSecret); !errors.Is(err, errTOTPKeyMissing) {
		t.Errorf("seal with no key = %v", err)
	}
}

// The key's configured form is strict, and an error never shows the value.
func TestParseTOTPKey(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, TOTPKeyBytes))
	tests := []struct {
		name, in string
		ok       bool
	}{
		{"openssl rand -base64 32", good, true},
		{"with a newline", good + "\n", true},
		{"empty", "", false},
		{"blank", "   ", false},
		{"not base64", "this is not a key at all, not even close!!", false},
		{"too short", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)), false},
		{"too long", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 33)), false},
		{"unpadded", strings.TrimRight(good, "="), false},
		{"url alphabet", strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, TOTPKeyBytes))), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := ParseTOTPKey(tt.in)
			if (err == nil) != tt.ok || (tt.ok && k == nil) {
				t.Fatalf("ParseTOTPKey = %v, %v; want ok %v", k, err, tt.ok)
			}
			if err != nil && strings.TrimSpace(tt.in) != "" && strings.Contains(err.Error(), strings.TrimSpace(tt.in)) {
				t.Errorf("the error shows the value: %v", err)
			}
		})
	}
}

// The key prints as a placeholder however it is printed or logged, alone or
// inside the server's Config.
func TestTOTPKeyNeverPrints(t *testing.T) {
	raw := bytes.Repeat([]byte{0x41}, TOTPKeyBytes) // "AAAA…" once printed as text
	k, err := ParseTOTPKey(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{TOTPKey: k}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("config", "key", k, "cfg", cfg)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("config", "key", k)
	out := fmt.Sprintf("%v %+v %#v %s %v %+v %#v %x", k, k, k, k, *k, cfg, cfg, k) + logged.String()
	for _, leak := range []string{string(raw), base64.StdEncoding.EncodeToString(raw), "41414141", "65 65 65"} {
		if strings.Contains(out, leak) {
			t.Fatalf("the key was printed: %s", out)
		}
	}
}

// The enrolment material is the secret in base32 and the RFC 6238
// parameters, and the otpauth URI says the same.
func TestTOTPEnrollmentMaterial(t *testing.T) {
	e := totpEnrollment(rfcSecret, totpAccount("root", "chat.example.com:443"))
	if e.Secret != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" || e.Account != "root@chat.example.com" || e.Issuer != "Conch" ||
		e.Algorithm != "SHA1" || e.Digits != 6 || e.PeriodSeconds != 30 {
		t.Errorf("enrolment %+v", e)
	}
	u, err := url.Parse(e.URI)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Conch:root@chat.example.com" ||
		q.Get("secret") != e.Secret || q.Get("issuer") != "Conch" || q.Get("digits") != "6" || q.Get("period") != "30" || q.Get("algorithm") != "SHA1" {
		t.Errorf("uri %q", e.URI)
	}
	for host, want := range map[string]string{"": "root", "[::1]:8080": "root", "evil host/x": "root", "127.0.0.1:8080": "root@127.0.0.1"} {
		if got := totpAccount("root", host); got != want {
			t.Errorf("totpAccount(%q) = %q, want %q", host, got, want)
		}
	}
}
