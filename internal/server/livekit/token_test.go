package livekit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func testClient(t *testing.T, apiURL string) *Client {
	t.Helper()
	c, err := New(Config{URL: "ws://lk.test", APIURL: apiURL, APIKey: "devkey", APISecret: NewSecret("s3cret-value")})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return testNow }
	return c
}

// decodeJWT verifies the HS256 signature with secret and returns the header
// and claims. It fails the test on a bad signature.
func decodeJWT(t *testing.T, token, secret string) (header, claims map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		t.Fatal("signature does not verify")
	}
	for i, dst := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatal(err)
		}
	}
	return header, claims
}

func TestJoinToken(t *testing.T) {
	tests := []struct {
		name       string
		canPublish bool
		lifetime   time.Duration
		backdate   time.Duration
	}{
		{"publisher", true, 60 * time.Second, 30 * time.Second},
		{"listener only", false, 60 * time.Second, 30 * time.Second},
		{"no backdate", true, 5 * time.Minute, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t, "http://unused")
			tok, err := c.JoinToken(JoinParams{
				Identity: "p7", Room: "conch-abc", CanPublish: tt.canPublish,
				Lifetime: tt.lifetime, BackdateNotBefore: tt.backdate,
			})
			if err != nil {
				t.Fatal(err)
			}
			header, claims := decodeJWT(t, tok, "s3cret-value")
			if !reflect.DeepEqual(header, map[string]any{"alg": "HS256", "typ": "JWT"}) {
				t.Errorf("header = %v", header)
			}
			if got, want := len(claims), 6; got != want {
				t.Errorf("claims = %v, want exactly iss, sub, jti, nbf, exp, video", claims)
			}
			if id, _ := claims["jti"].(string); len(id) != 16 {
				t.Errorf("jti = %v, want 16 characters", claims["jti"])
			}
			if claims["iss"] != "devkey" || claims["sub"] != "p7" {
				t.Errorf("iss/sub = %v/%v", claims["iss"], claims["sub"])
			}
			if got, want := claims["nbf"], float64(testNow.Add(-tt.backdate).Unix()); got != want {
				t.Errorf("nbf = %v, want %v", got, want)
			}
			if got, want := claims["exp"], float64(testNow.Add(tt.lifetime).Unix()); got != want {
				t.Errorf("exp = %v, want %v", got, want)
			}
			wantVideo := map[string]any{
				"roomJoin": true, "room": "conch-abc", "canSubscribe": true,
				"canPublish": tt.canPublish, "canPublishData": false,
				"canPublishSources": []any{"microphone"},
			}
			if !reflect.DeepEqual(claims["video"], wantVideo) {
				t.Errorf("video = %v, want %v", claims["video"], wantVideo)
			}
			// A wrong secret must not verify.
			parts := strings.Split(tok, ".")
			mac := hmac.New(sha256.New, []byte("wrong"))
			mac.Write([]byte(parts[0] + "." + parts[1]))
			if hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), []byte(parts[2])) {
				t.Error("token verifies under the wrong secret")
			}
		})
	}
}

func TestJoinTokenRejectsBadParams(t *testing.T) {
	c := testClient(t, "http://unused")
	tests := []struct {
		name string
		p    JoinParams
	}{
		{"no identity", JoinParams{Room: "r", Lifetime: time.Second}},
		{"no room", JoinParams{Identity: "p1", Lifetime: time.Second}},
		{"zero lifetime", JoinParams{Identity: "p1", Room: "r"}},
		{"negative backdate", JoinParams{Identity: "p1", Room: "r", Lifetime: time.Second, BackdateNotBefore: -time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := c.JoinToken(tt.p)
			if err == nil || tok != "" {
				t.Fatalf("token %q, err %v; want an error", tok, err)
			}
			if strings.Contains(err.Error(), "s3cret-value") {
				t.Error("error contains the secret")
			}
		})
	}
}

func TestNewRequiresConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New(Config{}) succeeded")
	}
}

// Every join token is a distinct string, even for the same identity and room
// at the same instant: the test client's clock is frozen, so only the random
// "jti" can tell them apart. Admin tokens carry none.
func TestJoinTokensAreDistinct(t *testing.T) {
	// A client of its own, with another secret, and the clock frozen.
	const secret = "another-s3cret"
	c, err := New(Config{URL: "ws://lk.test", APIURL: "http://unused", APIKey: "devkey", APISecret: NewSecret(secret)})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return testNow }
	p := JoinParams{Identity: "p7", Room: "conch-room", CanPublish: true, Lifetime: 15 * time.Second}
	seen := map[string]bool{}
	ids := map[string]bool{}
	for i := 0; i < 50; i++ {
		token, err := c.JoinToken(p)
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatalf("token %d repeats an earlier one", i)
		}
		seen[token] = true
		_, cl := decodeJWT(t, token, secret)
		id, _ := cl["jti"].(string)
		if len(id) != 16 || ids[id] {
			t.Fatalf("jti = %q: want 16 characters (96 random bits), never repeated", id)
		}
		ids[id] = true
	}
	admin, err := c.adminToken(adminGrant{RoomList: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, cl := decodeJWT(t, admin, secret); cl["jti"] != nil {
		t.Errorf("an admin token carries jti %v", cl["jti"])
	}
}
