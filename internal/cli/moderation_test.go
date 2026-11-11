package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestModerationCommandsYesAndExactRoutes(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		method  string
		path    string
		wantOut string
	}{
		{"voice kick", []string{"voice", "kick", "general", "7", "--yes"}, http.MethodDelete, "/v1/channels/general/voice/participants/7", "they can rejoin; chat access is unchanged"},
		{"member remove", []string{"admin", "member", "remove", "general", "7", "--yes"}, http.MethodDelete, "/v1/channels/general/members/7", "removed or was already absent"},
		{"ban add", []string{"admin", "ban", "add", "general", "7", "--yes"}, http.MethodPut, "/v1/channels/general/bans/7", "ban is in force"},
		{"ban repeat", []string{"admin", "ban", "add", "general", "7", "--yes"}, http.MethodPut, "/v1/channels/general/bans/7", "it may already have been"},
		{"unban", []string{"admin", "ban", "remove", "general", "7", "--yes"}, http.MethodDelete, "/v1/channels/general/bans/7", "membership and rights are not restored"},
		{"unban repeat", []string{"admin", "ban", "remove", "general", "7", "--yes"}, http.MethodDelete, "/v1/channels/general/bans/7", "or was already not banned"},
		{"message remove", []string{"admin", "message", "remove", "general", "42", "--yes"}, http.MethodDelete, "/v2/channels/general/messages/42", "received copies are not erased"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != tt.method || r.URL.EscapedPath() != tt.path {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.EscapedPath(), tt.method, tt.path)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			stdout, stderr, err := runCLI(t, "", append(tt.args, "--server", srv.URL)...)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || !strings.Contains(stdout, tt.wantOut) || stderr != "" {
				t.Fatalf("calls=%d stdout=%q stderr=%q", calls.Load(), stdout, stderr)
			}
		})
	}
}

func TestModerationCommandsRequireYesNonInteractively(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"voice kick", []string{"voice", "kick", "general", "7"}},
		{"member remove", []string{"admin", "member", "remove", "general", "7"}},
		{"ban add", []string{"admin", "ban", "add", "general", "7"}},
		{"unban", []string{"admin", "ban", "remove", "general", "7"}},
		{"message remove", []string{"admin", "message", "remove", "general", "42"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			stdout, _, err := runCLI(t, "yes\n", append(tt.args, "--server", srv.URL)...)
			if err == nil || !strings.Contains(err.Error(), "--yes") || calls.Load() != 0 || stdout != "" {
				t.Fatalf("stdout=%q err=%v calls=%d", stdout, err, calls.Load())
			}
		})
	}
}

func TestVoiceKickUnavailableOffersRetryWithoutSuccess(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(schema.Error{Code: schema.ErrorCodeVoiceUnavailable, Message: "hostile server words"})
	}))
	defer srv.Close()
	stdout, _, err := runCLI(t, "", "voice", "kick", "general", "7", "--yes", "--server", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("error = %v", err)
	}
	if stdout != "" || strings.Contains(err.Error(), "completed") || strings.Contains(err.Error(), "hostile") {
		t.Fatalf("stdout=%q err=%q", stdout, err)
	}
}

func TestModerationBanListOutput(t *testing.T) {
	stamp := schema.NewTimestamp(time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC))
	tests := []struct {
		name    string
		json    bool
		bans    []schema.ChannelBanV1
		wantOut string
	}{
		{name: "empty", bans: []schema.ChannelBanV1{}, wantOut: "no bans in general\n"},
		{name: "escaped and creator", bans: []schema.ChannelBanV1{{PrincipalID: 7, Name: "bad\x1b[31m\u202e", CreatedBy: 2, CreatedAt: stamp}}, wantOut: "7 bad\\x1b[31m\\u202e banned-by 2 at 2026-11-06T12:00:00Z\n"},
		{name: "unknown creator", bans: []schema.ChannelBanV1{{PrincipalID: 7, Name: "person", CreatedAt: stamp}}, wantOut: "7 person banned-by unknown at 2026-11-06T12:00:00Z\n"},
		{name: "json", json: true, bans: []schema.ChannelBanV1{}, wantOut: "{\"bans\":[]}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/channels/general/bans" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(schema.ListChannelBansResponseV1{Bans: tt.bans})
			}))
			defer srv.Close()
			args := []string{"admin", "ban", "list", "general", "--server", srv.URL}
			if tt.json {
				args = append(args, "--json")
			}
			stdout, stderr, err := runCLI(t, "", args...)
			if err != nil || stdout != tt.wantOut || stderr != "" {
				t.Fatalf("stdout=%q want=%q stderr=%q err=%v", stdout, tt.wantOut, stderr, err)
			}
		})
	}
}

func TestModerationCommandRefusalsUseClientWords(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code string
		want string
	}{
		{"member forbidden", []string{"admin", "member", "remove", "general", "7", "--yes"}, "forbidden", "the server does not allow you"},
		{"message invisible", []string{"admin", "message", "remove", "general", "42", "--yes"}, schema.ErrorCodeMessageNotFound, "no such message, or you cannot see it"},
		{"ban missing person", []string{"admin", "ban", "add", "general", "7", "--yes"}, "principal_not_found", "no such person"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(schema.Error{Code: tt.code, Message: "SERVER\x1b[31m"})
			}))
			defer srv.Close()
			stdout, _, err := runCLI(t, "", append(tt.args, "--server", srv.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) || stdout != "" || !bytes.Equal([]byte(ErrorLine(err)), []byte(safeLine(ErrorLine(err)))) {
				t.Fatalf("stdout=%q err=%q", stdout, err)
			}
			if strings.Contains(ErrorLine(err), "SERVER") || strings.Contains(ErrorLine(err), "\x1b") {
				t.Fatalf("server text escaped incorrectly: %q", ErrorLine(err))
			}
		})
	}
}
