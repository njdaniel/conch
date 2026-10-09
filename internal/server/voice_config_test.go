package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/njdaniel/conch/internal/server/livekit"
)

func TestVoiceConfiguredAndStartupNeverContactsLiveKit(t *testing.T) {
	var hits atomic.Int32
	lk := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer lk.Close()

	tests := []struct {
		name string
		cfg  livekit.Config
		want bool
	}{
		{"not configured", livekit.Config{}, false},
		{"configured", livekit.Config{URL: "ws://lk", APIURL: lk.URL, APIKey: "k", APISecret: livekit.NewSecret("s")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff, LiveKit: tt.cfg})
			if got := srv.VoiceConfigured(); got != tt.want {
				t.Errorf("VoiceConfigured() = %v, want %v", got, tt.want)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("LiveKit received %d request(s) during startup, want 0", n)
	}
}

// The one place conchd logs its LiveKit settings is the startup line. It must
// name the addresses and never the secret, whatever the log handler.
func TestStartupLogRedactsTheLiveKitSecret(t *testing.T) {
	const secret = "startup-secret-must-not-be-logged-0123456789"
	logs := captureLogs(t)
	newTestServerWithConfig(t, Config{AuthMode: AuthOff, LiveKit: livekit.Config{
		URL: "ws://lk.test:7880", APIURL: "http://lk.test:7880", APIKey: "the-key-id", APISecret: livekit.NewSecret(secret),
	}})
	out := logs.buf.String()
	if strings.Contains(out, secret) || strings.Contains(out, "the-key-id") {
		t.Fatalf("startup logs contain the key pair: %s", out)
	}
	if !strings.Contains(out, "voice: configured") || !strings.Contains(out, "[redacted]") || !strings.Contains(out, "ws://lk.test:7880") {
		t.Errorf("startup log = %s, want the addresses and a redaction marker", out)
	}
}
