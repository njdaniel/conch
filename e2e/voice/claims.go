package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// The token conchd signs is the gate to a room, so the program reads it. This
// file shares no code with internal/server/livekit on purpose: the claim set
// below is written out here from the design note (§4) and from what LiveKit's
// access-token grant means, so that a change on either side shows up as a
// disagreement rather than as two copies of the same mistake.

// joinClaimLifetime is the lifetime conchd gives a join token (design note
// §4). The check allows a second either way: nbf and exp have one-second
// resolution.
const joinClaimLifetime = 15 * time.Second

// joinClaims is every top-level claim a join token may carry. Decoding is
// strict, so a claim added to the token is a failure here.
type joinClaims struct {
	Iss   string         `json:"iss"`
	Sub   string         `json:"sub"`
	Jti   string         `json:"jti"`
	Nbf   int64          `json:"nbf"`
	Exp   int64          `json:"exp"`
	Video map[string]any `json:"video"`
}

// checkJoinToken decodes a join token, verifies its HMAC-SHA256 signature with
// the API secret this run chose, and checks every claim: who it is for, which
// one room it admits to, what it lets the holder do, and how long it lives.
// It also requires that no two tokens seen in this run share a jti. It never
// returns the token in an error.
func (h *harness) checkJoinToken(token string, lk *livekitSettings, sub, room string, canPublish bool, expiresAt time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("join token is not a three-part JWT")
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("join token header is not base64url")
	}
	var header map[string]any
	if err := json.Unmarshal(rawHeader, &header); err != nil || !reflect.DeepEqual(header, map[string]any{"alg": "HS256", "typ": "JWT"}) {
		return errors.New("join token header is not exactly {alg: HS256, typ: JWT}")
	}
	mac := hmac.New(sha256.New, []byte(lk.secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), []byte(parts[2])) {
		return errors.New("join token is not signed with the configured API secret")
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("join token payload is not base64url")
	}
	var c joinClaims
	dec := json.NewDecoder(bytes.NewReader(rawPayload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return fmt.Errorf("join token carries a claim outside iss, sub, jti, nbf, exp, video: %w", err)
	}
	now := time.Now().Unix()
	life := time.Duration(c.Exp-c.Nbf) * time.Second
	switch {
	case c.Iss != lk.key:
		return fmt.Errorf("join token iss %q, want the configured key %q", c.Iss, lk.key)
	case c.Sub != sub:
		return fmt.Errorf("join token sub %q, want %q", c.Sub, sub)
	case c.Jti == "":
		return errors.New("join token has no jti, so two tokens issued in one second would be the same string")
	case life < joinClaimLifetime-time.Second || life > joinClaimLifetime+time.Second:
		return fmt.Errorf("join token lives %s (exp-nbf), want %s", life, joinClaimLifetime)
	case c.Exp-now > int64(joinClaimLifetime/time.Second)+1:
		return fmt.Errorf("join token expires %d s from now, want at most %d", c.Exp-now, int64(joinClaimLifetime/time.Second))
	case c.Nbf > now+1 || c.Nbf < now-5:
		return fmt.Errorf("join token not-before is %d s from now, want about now", c.Nbf-now)
	case c.Exp-expiresAt.Unix() > 1 || expiresAt.Unix()-c.Exp > 1:
		return fmt.Errorf("join token exp differs from the response's expires_at by %d s", c.Exp-expiresAt.Unix())
	}
	want := map[string]any{
		"roomJoin":          true,
		"room":              room,
		"canSubscribe":      true,
		"canPublish":        canPublish,
		"canPublishData":    false,
		"canPublishSources": []any{"microphone"},
	}
	if !reflect.DeepEqual(c.Video, want) {
		// Say which grants differ without printing the room name.
		var diff []string
		for k, v := range c.Video {
			if k == "room" {
				if v != room {
					diff = append(diff, "room is not the granted room")
				}
				continue
			}
			if w, ok := want[k]; !ok {
				diff = append(diff, fmt.Sprintf("extra grant %s=%v", k, v))
			} else if !reflect.DeepEqual(w, v) {
				diff = append(diff, fmt.Sprintf("%s=%v, want %v", k, v, w))
			}
		}
		for k := range want {
			if _, ok := c.Video[k]; !ok {
				diff = append(diff, "missing grant "+k)
			}
		}
		return fmt.Errorf("join token video grant is not exactly join+subscribe+publish(microphone only) for the one room: %s", strings.Join(diff, "; "))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.jtis[c.Jti] {
		return errors.New("two join tokens in this run share a jti")
	}
	h.jtis[c.Jti] = true
	return nil
}
