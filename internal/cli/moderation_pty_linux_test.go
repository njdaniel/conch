//go:build linux

package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestModerationCommandsInteractiveConfirmation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantPrompt string
	}{
		{"voice kick", []string{"voice", "kick", "general", "7"}, "They can rejoin; chat access is unchanged"},
		{"member remove", []string{"admin", "member", "remove", "general", "7"}, "and its nets. This does not ban them or revoke their account"},
		{"ban add", []string{"admin", "ban", "add", "general", "7"}, "A new account or guest visit is not identified by this ban"},
		{"unban", []string{"admin", "ban", "remove", "general", "7"}, "does not re-add them or restore any rights"},
		{"message remove", []string{"admin", "message", "remove", "general", "42"}, "server backups and copies already received cannot be recalled"},
	}
	for _, tt := range tests {
		for _, answer := range []string{"y\n", "n\n", "\n"} {
			t.Run(tt.name+"/"+strings.TrimSpace(answer), func(t *testing.T) {
				isolateConfig(t)
				t.Setenv("CONCH_TOKEN", "tok")
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer srv.Close()
				master, slave := openPTY(t)
				if _, err := master.WriteString(answer); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				err := RunWithStdin(context.Background(), append(tt.args, "--server", srv.URL), slave, &stdout, &stderr, "vtest")
				confirmed := answer == "y\n"
				if confirmed && err != nil {
					t.Fatal(err)
				}
				if !confirmed && (err == nil || !strings.Contains(err.Error(), "not confirmed")) {
					t.Fatalf("error = %v, want cancellation", err)
				}
				if got, want := calls.Load(), int32(1); confirmed && got != want || !confirmed && got != 0 {
					t.Fatalf("calls = %d, confirmed=%v", got, confirmed)
				}
				if !strings.Contains(stderr.String(), tt.wantPrompt) || !strings.Contains(stderr.String(), "[y/N]") {
					t.Fatalf("prompt = %q, want scope %q", stderr.String(), tt.wantPrompt)
				}
			})
		}
	}
}
