package server

import (
	"net/http"
	"net/http/httptest"
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
		{"configured", livekit.Config{URL: "ws://lk", APIURL: lk.URL, APIKey: "k", APISecret: "s"}, true},
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
