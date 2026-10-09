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
			if got, want := len(claims), 5; got != want {
				t.Errorf("claims = %v, want exactly iss, sub, nbf, exp, video", claims)
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
