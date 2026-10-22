package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238's default, which every authenticator app speaks; HMAC-SHA-1 is not broken as a MAC
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The operator's second factor (issue #709, docs/design/guests-and-accounts.md
// §5.3): time-based one-time passwords by RFC 6238, on the standard library.
// A 160-bit secret, HMAC-SHA-1, six digits, 30-second steps and one step
// either side, the parameters of pkg/schema. The secret is kept only sealed,
// with AES-256-GCM under the host's TOTP key (TOTPKey), a key that lives
// outside the database and its backups; nothing here logs a secret, a code
// or the key, and errors never carry one.

// TOTPKeyBytes is the length of the host's TOTP key: an AES-256 key.
const TOTPKeyBytes = 32

// TOTPKey is the host key the operators' TOTP secrets are sealed under. It
// prints as a placeholder however it is formatted or logged, so a Config in a
// log line cannot leak it.
type TOTPKey struct{ k [TOTPKeyBytes]byte }

// String, GoString and LogValue never show the key.
func (TOTPKey) String() string               { return "[redacted]" }
func (TOTPKey) GoString() string             { return "[redacted]" }
func (TOTPKey) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (k TOTPKey) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(k.String())) }

// ParseTOTPKey reads the key from its configured form: the standard base64 of
// exactly TOTPKeyBytes bytes, as `openssl rand -base64 32` prints it, with
// surrounding white space ignored. Anything else is refused with an error
// that names the rule and never the value.
func ParseTOTPKey(s string) (*TOTPKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("the TOTP key is empty")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("the TOTP key is not base64; make one with: openssl rand -base64 %d", TOTPKeyBytes)
	}
	if len(raw) != TOTPKeyBytes {
		return nil, fmt.Errorf("the TOTP key is %d bytes, want %d; make one with: openssl rand -base64 %d", len(raw), TOTPKeyBytes, TOTPKeyBytes)
	}
	k := &TOTPKey{}
	copy(k.k[:], raw)
	return k, nil
}

// errTOTPKeyMissing is a TOTP operation on a server started without a TOTP
// key, and errTOTPUnsealable a sealed secret the key does not open (a
// different key, or a damaged row). Both fail closed: no code is accepted.
var (
	errTOTPKeyMissing = errors.New("server: no TOTP key is configured (CONCHD_TOTP_KEY)")
	errTOTPUnsealable = errors.New("server: a TOTP secret cannot be opened with the configured TOTP key")
)

// sealedTOTPVersion starts every sealed secret, so a later format can be told
// apart.
const sealedTOTPVersion = 1

// totpAAD binds a sealed secret to the principal it is for, so a row copied
// onto another operator's does not open.
func totpAAD(principalID int64) []byte {
	return []byte("conch totp secret v1 principal=" + strconv.FormatInt(principalID, 10))
}

func (k *TOTPKey) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.k[:])
	if err != nil {
		return nil, fmt.Errorf("server: totp key: %w", err)
	}
	return cipher.NewGCM(block)
}

// sealTOTP encrypts secret for principalID: the version byte, a random
// nonce and the ciphertext with its tag.
func (k *TOTPKey) sealTOTP(principalID int64, secret []byte) ([]byte, error) {
	if k == nil {
		return nil, errTOTPKeyMissing
	}
	a, err := k.aead()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+a.NonceSize(), 1+a.NonceSize()+len(secret)+a.Overhead())
	out[0] = sealedTOTPVersion
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, fmt.Errorf("server: totp nonce: %w", err)
	}
	return a.Seal(out, out[1:], secret, totpAAD(principalID)), nil
}

// openTOTP decrypts what sealTOTP made for principalID. Any failure is
// errTOTPUnsealable (or errTOTPKeyMissing): the caller accepts no code.
func (k *TOTPKey) openTOTP(principalID int64, sealed []byte) ([]byte, error) {
	if k == nil {
		return nil, errTOTPKeyMissing
	}
	a, err := k.aead()
	if err != nil {
		return nil, err
	}
	if len(sealed) < 1+a.NonceSize()+a.Overhead() || sealed[0] != sealedTOTPVersion {
		return nil, errTOTPUnsealable
	}
	nonce, ct := sealed[1:1+a.NonceSize()], sealed[1+a.NonceSize():]
	secret, err := a.Open(nil, nonce, ct, totpAAD(principalID))
	if err != nil {
		return nil, errTOTPUnsealable
	}
	return secret, nil
}

// newTOTPSecret is a fresh secret from crypto/rand.
func newTOTPSecret() ([]byte, error) {
	b := make([]byte, schema.TOTPSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("server: totp secret: %w", err)
	}
	return b, nil
}

// totpBase32 is the secret as an authenticator app takes it: base32, no
// padding.
var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// hotp is RFC 4226's HOTP value of counter under secret with HMAC-SHA-1, as
// digits decimal digits.
func hotp(secret []byte, counter uint64, digits int) string {
	m := hmac.New(sha1.New, secret)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], counter)
	_, _ = m.Write(c[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// totpStep is the RFC 6238 time step of t.
func totpStep(t time.Time) int64 { return t.Unix() / schema.TOTPPeriodSeconds }

// totpCodeAt is the code for secret at step.
func totpCodeAt(secret []byte, step int64) string {
	return hotp(secret, uint64(step), schema.TOTPDigits) // #nosec G115 -- a step is the Unix time over 30, never negative for a real clock
}

// matchTOTP reports which time step code is the code of, within
// schema.TOTPWindowSteps either side of now, among the steps after lastStep:
// a step already accepted, or one before it, never matches again (replay).
// Every candidate is compared, in constant time, whether or not an earlier
// one matched, so the answer's timing says nothing about which step it was.
// The latest matching step wins.
func matchTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	cur := totpStep(now)
	var found int64
	ok := false
	for s := cur - schema.TOTPWindowSteps; s <= cur+schema.TOTPWindowSteps; s++ {
		eq := subtle.ConstantTimeCompare([]byte(totpCodeAt(secret, s)), []byte(code)) == 1
		if eq && s > lastStep && s >= 0 {
			found, ok = s, true
		}
	}
	return found, ok
}

// totpIssuer is the issuer an authenticator shows beside the account.
const totpIssuer = "Conch"

// totpAccount is the account label for name: the name, and the host the
// request came to when it is a plain host name, so an operator of two
// servers can tell their entries apart. A host that is anything else is
// left out rather than shown.
func totpAccount(name, host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || len(host) > 253 || strings.Trim(host, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-") != "" {
		return name
	}
	return name + "@" + host
}

// totpEnrollment is the setup material of secret for an authenticator app:
// the manual parameters and the same as an otpauth:// URI.
func totpEnrollment(secret []byte, account string) schema.TOTPEnrollmentV1 {
	b32 := totpBase32.EncodeToString(secret)
	q := url.Values{}
	q.Set("secret", b32)
	q.Set("issuer", totpIssuer)
	q.Set("algorithm", schema.TOTPAlgorithm)
	q.Set("digits", strconv.Itoa(schema.TOTPDigits))
	q.Set("period", strconv.Itoa(schema.TOTPPeriodSeconds))
	label := url.PathEscape(totpIssuer) + ":" + url.PathEscape(account)
	return schema.TOTPEnrollmentV1{
		Secret:        b32,
		Issuer:        totpIssuer,
		Account:       account,
		Algorithm:     schema.TOTPAlgorithm,
		Digits:        schema.TOTPDigits,
		PeriodSeconds: schema.TOTPPeriodSeconds,
		URI:           "otpauth://totp/" + label + "?" + q.Encode(),
	}
}
