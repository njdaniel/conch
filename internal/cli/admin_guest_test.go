package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Mistakes `conch admin join` and `conch admin guest` can see are refused
// before any request (issue #715).
func TestAdminJoinAndGuestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no change", []string{"join", "set"}, "the request changes no setting"},
		{"an unknown mode", []string{"join", "set", "--open-joining", "sometimes"}, "open_joining must be one of"},
		{"a switch that is not on or off", []string{"join", "set", "--guests", "maybe"}, "--guests must be on or off"},
		{"a duration that is not one", []string{"join", "set", "--guest-idle", "5"}, "--guest-idle must be a whole number of seconds"},
		{"both channel flags", []string{"join", "set", "--open-channel", "lobby", "--no-open-channels"}, "not both"},
		{"an unknown state", []string{"guest", "list", "--state", "gone"}, "--state must be active, ended or converted"},
		{"an unknown origin", []string{"guest", "list", "--origin", "door"}, "--origin must be address or invite"},
		{"an invite from the address", []string{"guest", "end-all", "--origin", "address", "--invite", "3"}, "origin cannot be address"},
		{"a dry run that is done", []string{"guest", "end-all", "--dry-run", "--yes"}, "not both"},
		{"no id", []string{"guest", "end"}, "expected <id>"},
		{"a permit for too long", []string{"guest", "permit", "2", "--expires", "2h"}, "expires_at must be within"},
		{"an unknown invite mode", []string{"invite", "create", "--open", "--channel", "lobby", "--expires", "1h", "--join-as", "visitor"}, "join_as must be one of"},
		{"a link of another purpose", []string{"principal", "sign-in-link", "2", "--purpose", "device"}, "a device link is not made on this route"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer fake.Close()
			stdout, stderr, err := runCLI(t, "", append(append([]string{"admin"}, tt.args...), "--server", fake.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if stdout != "" || stderr != "" || requests.Load() != 0 {
				t.Errorf("stdout %q, stderr %q, %d requests; want none", stdout, stderr, requests.Load())
			}
		})
	}
}
