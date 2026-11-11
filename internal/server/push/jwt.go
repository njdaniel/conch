package push

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// The VAPID authorization of RFC 8292: an ES256 JWT, built by hand with
// crypto/ecdsa, encoding/base64 and encoding/json.

// JWTLifetime is how far ahead a VAPID token's exp is set. RFC 8292 section
// 2 allows at most 24 hours; half of that leaves room for a clock that
// disagrees with a push service's.
const JWTLifetime = 12 * time.Hour

// MaxJWTLifetime is the RFC 8292 limit on exp.
const MaxJWTLifetime = 24 * time.Hour

// ValidateSubject checks an operator-given VAPID subject (RFC 8292 section
// 2.1): "mailto:" or "https:" followed by something, and nothing a header or
// a JSON string could mishandle. The empty subject is valid and means none.
func ValidateSubject(sub string) error {
	if sub == "" {
		return nil
	}
	if len(sub) > 256 {
		return errors.New("the subject is too long")
	}
	for _, r := range sub {
		if r <= ' ' || r == 0x7f {
			return errors.New("the subject must not contain spaces or control characters")
		}
	}
	switch {
	case strings.HasPrefix(sub, "mailto:"):
		if len(sub) == len("mailto:") {
			return errors.New("the mailto: subject has no address")
		}
	case strings.HasPrefix(sub, "https:"):
		u, err := url.Parse(sub)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return errors.New("the https: subject is not a URL")
		}
	default:
		return errors.New("the subject must start with mailto: or https: and a value")
	}
	return nil
}

// jwtClaims is the JWT payload. sub is left out when it is empty.
type jwtClaims struct {
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Sub string `json:"sub,omitempty"`
}

// jwtHeader is the fixed JWT header, ES256 (RFC 8292 section 2).
const jwtHeader = `{"typ":"JWT","alg":"ES256"}`

// Origin returns the origin (scheme, host and port as written) of an https
// endpoint, which is a VAPID token's aud (RFC 8292 section 2). It is false for
// a URL without a host or with another scheme.
func Origin(endpoint string) (string, bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	return "https://" + u.Host, true
}

// SignVAPID returns the compact JWT for a request to the push service at aud,
// expiring at exp, signed with key. sub is added when it is not empty.
func SignVAPID(key *ecdsa.PrivateKey, aud, sub string, exp time.Time) (string, error) {
	claims, err := json.Marshal(jwtClaims{Aud: aud, Exp: exp.Unix(), Sub: sub})
	if err != nil {
		return "", errors.New("push: cannot encode the VAPID claims")
	}
	enc := base64.RawURLEncoding
	input := enc.EncodeToString([]byte(jwtHeader)) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	der, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return "", errors.New("push: cannot sign the VAPID token")
	}
	var sig struct{ R, S *big.Int }
	if rest, err := asn1.Unmarshal(der, &sig); err != nil || len(rest) != 0 {
		return "", errors.New("push: cannot sign the VAPID token")
	}
	// JWS ES256 (RFC 7518 section 3.4): R and S, each as 32 big-endian
	// bytes, concatenated; not the DER form.
	raw := make([]byte, 64)
	sig.R.FillBytes(raw[:32])
	sig.S.FillBytes(raw[32:])
	return input + "." + enc.EncodeToString(raw), nil
}

// AuthorizationHeader is the Authorization header value of RFC 8292 section
// 3: "vapid t=<jwt>, k=<public key>", the key being the unpadded base64url
// public key PushConfigV1 serves.
func AuthorizationHeader(jwt, publicKey string) string {
	return "vapid t=" + jwt + ", k=" + publicKey
}
