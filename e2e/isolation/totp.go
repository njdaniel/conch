package main

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- RFC 6238's HMAC-SHA-1, the algorithm conchd's second factor uses (schema.TOTPAlgorithm)
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The operator's authenticator app, for the sign-in scenarios (authgates.go):
// RFC 6238 with the parameters conchd enrols (schema.TOTPAlgorithm,
// TOTPDigits, TOTPPeriodSeconds). It is written here, from the RFC, rather
// than taken from the server, so the run checks the server against the
// standard and not against itself.

// totpStep is the time step t falls in.
func totpStep(t time.Time) int64 { return t.Unix() / schema.TOTPPeriodSeconds }

// totpAt is the code for step, of digits digits, from the raw secret.
func totpAt(secret []byte, step int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) // #nosec G115 -- a time step is never negative
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, n%mod)
}

// decodeTOTPSecret reads the secret an enrolment shows: base32, as an
// authenticator app takes it, padded or not, in either case.
func decodeTOTPSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.TrimRight(strings.TrimSpace(s), "="))
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("the enrolment's secret is not base32")
	}
	return b, nil
}

// authenticator is an operator's authenticator app: the secret, and the
// last step it gave a code for, since conchd accepts each step once
// (design §5.3: an accepted step is spent, so a code is never good twice).
type authenticator struct {
	secret []byte
	last   int64
}

// code is a code conchd has not seen: the current step's, or, when that
// was given already, the next one's, which the ±1-step window accepts; when
// both are spent it waits for the next step to begin.
func (a *authenticator) code() (string, error) {
	for {
		now := totpStep(time.Now())
		for _, s := range []int64{now, now + 1} {
			if s > a.last {
				a.last = s
				return totpAt(a.secret, s, schema.TOTPDigits), nil
			}
		}
		next := time.Unix((now+1)*schema.TOTPPeriodSeconds, 0)
		select {
		case <-time.After(time.Until(next) + 50*time.Millisecond):
		case <-stopCtx.Done():
			return "", errInterrupted
		}
	}
}

// spent is a code of a step the authenticator already gave, for the replay
// check: conchd accepted it once and must not again.
func (a *authenticator) spent() string { return totpAt(a.secret, a.last, schema.TOTPDigits) }
