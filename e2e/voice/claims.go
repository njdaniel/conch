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

// The screen share (issue #443) is not in any token conchd signs: the join
// token above stays microphone only. A share is granted by conchd setting the
// permission of the sharer's connection, and that grant is checked here in
// the two places it can be seen: the permission LiveKit reports for the
// connection, and the token LiveKit then sends the connection of its own.

// shareSources is what a connection may publish: the microphone, and while it
// holds the room's share the screen and its sound.
func shareSources(share bool, names ...string) []any {
	out := []any{names[0]}
	if share {
		out = append(out, names[1], names[2])
	}
	return out
}

// checkSharePermission checks the permission LiveKit's ListParticipants
// reports for a participant against exactly the join grant, plus the two
// screen-share sources while share is true. Every field is compared, so that
// a grant conchd set wider than that (data, metadata, hidden, an empty source
// list, which LiveKit reads as every source) is a failure.
func checkSharePermission(perm map[string]any, share bool) error {
	want := map[string]any{
		"can_subscribe":            true,
		"can_publish":              true,
		"can_publish_data":         false,
		"can_publish_sources":      shareSources(share, "MICROPHONE", "SCREEN_SHARE", "SCREEN_SHARE_AUDIO"),
		"hidden":                   false,
		"recorder":                 false,
		"can_update_metadata":      false,
		"agent":                    false,
		"can_subscribe_metrics":    false,
		"can_manage_agent_session": false,
	}
	if !reflect.DeepEqual(perm, want) {
		return fmt.Errorf("permission is %v, want exactly %v", perm, want)
	}
	return nil
}

// checkLiveKitToken checks a token LiveKit sent a connected participant after
// conchd changed its permission: signed with the API secret this run chose,
// for sub, and granting exactly the join grant for the one room plus the two
// screen-share sources while share is true. LiveKit spells out three more
// permissions as false once a permission has been set (measured on 1.13.7);
// they are part of the exact grant. The token is never put in an error.
func (h *harness) checkLiveKitToken(token string, lk *livekitSettings, sub, room string, share bool) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("LiveKit's token is not a three-part JWT")
	}
	mac := hmac.New(sha256.New, []byte(lk.secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), []byte(parts[2])) {
		return errors.New("LiveKit's token is not signed with the configured API secret")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("LiveKit's token payload is not base64url")
	}
	var c struct {
		Sub   string         `json:"sub"`
		Video map[string]any `json:"video"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return errors.New("LiveKit's token payload is not JSON")
	}
	if c.Sub != sub {
		return fmt.Errorf("LiveKit's token is for %q, want %q", c.Sub, sub)
	}
	want := map[string]any{
		"roomJoin":              true,
		"room":                  room,
		"canSubscribe":          true,
		"canPublish":            true,
		"canPublishData":        false,
		"canPublishSources":     shareSources(share, "microphone", "screen_share", "screen_share_audio"),
		"canUpdateOwnMetadata":  false,
		"canSubscribeMetrics":   false,
		"canManageAgentSession": false,
	}
	if !reflect.DeepEqual(c.Video, want) {
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
		return fmt.Errorf("LiveKit's token does not grant exactly the join grant with share=%v: %s", share, strings.Join(diff, "; "))
	}
	return nil
}
