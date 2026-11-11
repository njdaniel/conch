package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestReactionCommands(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		method  string
		path    string
		wantOut string
	}{
		{name: "react alias", args: []string{"react", "--text", "general", "42", ":thumbsup:"}, method: http.MethodPut, path: "/v2/channels/general/messages/42/reactions/👍", wantOut: "message 42: :thumbsup: x3 you\n"},
		{name: "react Unicode", args: []string{"react", "general", "42", "👍"}, method: http.MethodPut, path: "/v2/channels/general/messages/42/reactions/👍", wantOut: "message 42: 👍 x3 you\n"},
		{name: "unreact", args: []string{"unreact", "general", "42", ":+1:"}, method: http.MethodDelete, path: "/v2/channels/general/messages/42/reactions/👍", wantOut: "message 42: 👍 x3 you\n"},
		{name: "flags after arguments", args: []string{"react", "general", "42", ":+1:", "--text"}, method: http.MethodPut, path: "/v2/channels/general/messages/42/reactions/👍", wantOut: "message 42: :thumbsup: x3 you\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.Path != tt.path {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(reactionTestSummary(42, schema.ReactionCountV1{Emoji: "👍", Count: 3, Me: true}))
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			if err := RunWithStdin(context.Background(), append(tt.args, "--server", server.URL), strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
				t.Fatalf("command: %v; stderr %q", err, stderr.String())
			}
			if got := stdout.String(); got != tt.wantOut {
				t.Errorf("stdout = %q, want %q", got, tt.wantOut)
			}
		})
	}
}

func TestReactionsCommandListsReactors(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	created := schema.NewTimestamp(time.Date(2026, time.November, 6, 11, 59, 0, 0, time.UTC))
	guest := &schema.GuestAttributionV1{Handle: "guest-7h3kq2m9p4r8s6t1", Label: "Nick", Tag: "7h3k"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v2/channels/general/messages/42/reactions":
			_ = json.NewEncoder(w).Encode(reactionTestSummary(42, schema.ReactionCountV1{Emoji: "👍", Count: 3, Me: true}))
		case "GET /v1/channels/general/members":
			_ = json.NewEncoder(w).Encode(schema.ListChannelMembersResponseV1{Members: []schema.ChannelMemberV1{{PrincipalID: 7, Name: "alice\x1b[31m\u202e"}}})
		case "GET /v2/channels/general/messages/42/reactions/👍":
			_ = json.NewEncoder(w).Encode(schema.ListReactorsResponseV1{Reactors: []schema.ReactionV1{
				{PrincipalID: 7, Emoji: "👍", CreatedAt: created},
				{PrincipalID: 9, Emoji: "👍", CreatedAt: created},
				{PrincipalID: 31, Emoji: "👍", CreatedAt: created, GuestReactor: guest},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := RunWithStdin(context.Background(), []string{"reactions", "--text", "general", "42", "--server", server.URL}, strings.NewReader(""), &stdout, &stderr, "test")
	if err != nil {
		t.Fatalf("reactions: %v; stderr %q", err, stderr.String())
	}
	want := ":thumbsup: 3 you\n" +
		"  7 alice\\x1b[31m\\u202e 2026-11-06T11:59:00.000Z\n" +
		"  9 hidden-9 2026-11-06T11:59:00.000Z\n" +
		"  31 Guest: Nick · 7h3k 2026-11-06T11:59:00.000Z\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}
	for _, hostile := range []string{"\x1b", "\u202e"} {
		if strings.Contains(stdout.String(), hostile) {
			t.Errorf("raw hostile character %q reached output", hostile)
		}
	}
}

func TestReactionCommandErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		status  int
		body    string
		wantErr string
	}{
		{name: "denied", args: []string{"react", "general", "42", "👍"}, status: 403, body: `{"code":"forbidden","message":"hostile\u001b[2J"}`, wantErr: "server does not allow"},
		{name: "message hidden from guest", args: []string{"react", "general", "42", "👍"}, status: 404, body: `{"code":"message_not_found","message":"message not found"}`, wantErr: "cannot see"},
		{name: "old server", args: []string{"react", "general", "42", "👍"}, status: 404, body: "404 page not found\n", wantErr: "404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			err := RunWithStdin(context.Background(), append(tt.args, "--server", server.URL), strings.NewReader(""), &stdout, &stderr, "test")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "\x1b") {
				t.Errorf("raw escape in error %q", err)
			}
		})
	}
}

func TestReactionCommandValidationIsOffline(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"react", "general", "42", ":unknown:"}, "unknown emoji"},
		{[]string{"react", "general", "no", "👍"}, "positive integer"},
		{[]string{"reactions", "general", "42", "--limit", "101"}, "1 to 100"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		err := RunWithStdin(context.Background(), append(tt.args, "--server", server.URL), strings.NewReader(""), &stdout, &stderr, "test")
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want %q", tt.args, err, tt.want)
		}
	}
	if called {
		t.Error("an invalid command reached the server")
	}
}

func TestAdminReactionRemove(t *testing.T) {
	tests := []struct {
		name string
		args []string
		path string
	}{
		{name: "one", args: []string{"admin", "reaction", "remove", "general", "42", "7", ":+1:", "--yes"}, path: "/v2/channels/general/messages/42/reactions/👍/7"},
		{name: "all", args: []string{"admin", "reaction", "remove", "general", "42", "7", "--yes"}, path: "/v2/channels/general/messages/42/reactors/7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != tt.path {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(reactionTestSummary(42))
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			if err := RunWithStdin(context.Background(), append(tt.args, "--server", server.URL), strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
				t.Fatal(err)
			}
			if got := stdout.String(); got != "message 42: no reactions\n" {
				t.Errorf("stdout = %q", got)
			}
		})
	}
}

func TestAdminReactionRemoveNeedsConfirmation(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := RunWithStdin(context.Background(), []string{"admin", "reaction", "remove", "general", "42", "7", "--server", server.URL}, strings.NewReader("n\n"), &stdout, &stderr, "test")
	if err == nil || !strings.Contains(err.Error(), "not confirmed") || called {
		t.Fatalf("declined removal: err %v, server called %v", err, called)
	}
}
