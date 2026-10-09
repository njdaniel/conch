package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestSendServerFlagOverridesEnvironment(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{})
	}))
	defer server.Close()
	t.Setenv("CONCH_SERVER", "http://127.0.0.1:1")
	t.Setenv("CONCH_AUTHOR", "9")

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"send", "--server", server.URL, "general", "hello"}, &stdout, &stderr, "test")
	if err != nil {
		t.Fatalf("run send: %v", err)
	}
	if !called {
		t.Fatal("flag server was not called")
	}
}

func TestSendUsesServerEnvironment(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{})
	}))
	defer server.Close()
	t.Setenv("CONCH_SERVER", server.URL)
	t.Setenv("CONCH_AUTHOR", "9")

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"send", "general", "hello"}, &stdout, &stderr, "test")
	if err != nil {
		t.Fatalf("run send: %v", err)
	}
	if !called {
		t.Fatal("environment server was not called")
	}
}

func TestApprovalsList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/approvals" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(schema.ListApprovalsResponseV1{
			Approvals: []schema.ApprovalV1{
				{ID: 1, State: schema.ApprovalStatePending, Title: "Test", RequesterID: 42, Deadline: schema.NewTimestamp(time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC))},
			},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approvals", "list", "--server", server.URL}, &stdout, &stderr, "vtest")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Test") || !strings.Contains(out, "1") {
		t.Errorf("output missing approval data: %q", out)
	}
}

func TestApprovalsDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/approvals/1/decisions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req schema.CastDecisionRequestV1
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.PrincipalID != 42 || req.Reason != "LGTM" || req.OptionID != "approve" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.CastDecisionResponseV1{
			Decision:   schema.Decision{PrincipalID: 42, OptionID: "approve", Reason: "LGTM"},
			State:      schema.ApprovalStateResolved,
			Resolution: &schema.ApprovalResolutionV1{},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approve", "--server", server.URL, "--author", "42", "--reason", "LGTM", "1"}, &stdout, &stderr, "vtest")
	if err != nil {
		t.Fatalf("run failed: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "resolved") {
		t.Errorf("expected resolved message, got: %q", out)
	}
}

func TestApprovalsReject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/approvals/1/decisions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req schema.CastDecisionRequestV1
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.PrincipalID != 42 || req.Reason != "not ready" || req.OptionID != "reject" {
			t.Errorf("request = %+v", req)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.CastDecisionResponseV1{
			Decision:   schema.Decision{PrincipalID: 42, OptionID: "reject", Reason: "not ready"},
			State:      schema.ApprovalStateResolved,
			Resolution: &schema.ApprovalResolutionV1{},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"reject", "--server", server.URL, "--author", "42", "--reason", "not ready", "1"}, &stdout, &stderr, "vtest")
	if err != nil {
		t.Fatalf("run failed: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "resolved") {
		t.Errorf("expected resolved message, got: %q", stdout.String())
	}
}

func TestApprovalsDecisionCustomOption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req schema.CastDecisionRequestV1
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.OptionID != "escalate-to-vp" {
			t.Errorf("optionID = %q, want custom option honored", req.OptionID)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.CastDecisionResponseV1{
			Decision: schema.Decision{PrincipalID: 42, OptionID: "escalate-to-vp", Reason: "needs VP"},
			State:    schema.ApprovalStatePending,
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approve", "--server", server.URL, "--author", "42", "--reason", "needs VP", "--option", "escalate-to-vp", "1"}, &stdout, &stderr, "vtest")
	if err != nil {
		t.Fatalf("run failed: %v\nstderr: %s", err, stderr.String())
	}
}

func TestApprovalsDecisionNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(schema.Error{
			Code:    "approval_not_found",
			Message: "approval not found",
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approve", "--server", server.URL, "--author", "42", "--reason", "LGTM", "999"}, &stdout, &stderr, "vtest")
	if err == nil {
		t.Fatal("expected error for unknown id")
	}
	if !strings.Contains(err.Error(), "approval 999 not found") {
		t.Errorf("expected helpful not-found error, got: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no direct stderr output, got: %q", stderr.String())
	}
}

func TestApprovalsDecisionMissingReason(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approve", "--author", "42", "1"}, &stdout, &stderr, "vtest")
	if err == nil {
		t.Fatal("expected error for missing reason")
	}
	if !strings.Contains(err.Error(), "--reason is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestApprovalsDecisionTerminalState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(schema.Error{
			Code:    "invalid_state",
			Message: "approval is in terminal state",
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"approve", "--server", server.URL, "--author", "42", "--reason", "LGTM", "1"}, &stdout, &stderr, "vtest")
	if err == nil {
		t.Fatal("expected error for terminal state")
	}
	if !strings.Contains(err.Error(), "approval 1 is no longer open") {
		t.Errorf("expected helpful terminal state error, got: %v", err)
	}
	// Run itself must not also print to stderr — cmd/conch's wrapper prints
	// the returned error, so writing here too would show the message twice.
	if stderr.Len() != 0 {
		t.Errorf("expected no direct stderr output, got: %q", stderr.String())
	}
}

// authFake is a conchd stand-in that enforces one bearer token and records the
// Authorization header and acting principal of every request it sees.
type authFake struct {
	*httptest.Server
	mu       sync.Mutex
	auth     map[string]string // "METHOD path" -> last Authorization header
	requests []string
	sentAs   int64
	decided  int64
}

const fakeToken = "tok-0123456789abcdef"

func newAuthFake(t *testing.T, enforce bool) *authFake {
	t.Helper()
	f := &authFake{auth: make(map[string]string)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		key := r.Method + " " + r.URL.Path
		f.auth[key] = r.Header.Get("Authorization")
		f.requests = append(f.requests, key)
		f.mu.Unlock()
		if enforce && r.Header.Get("Authorization") != "Bearer "+fakeToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(schema.Error{Code: "unauthenticated", Message: "authentication required"})
			return
		}
		switch key {
		case "GET /v1/whoami":
			_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 42, Kind: "human", Name: "nick", Role: schema.RoleOperator})
		case "POST /v0/channels/general/messages":
			var req schema.PostMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.sentAs = req.AuthorID
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{Message: schema.MessageV0{ID: 1, AuthorID: req.AuthorID, Body: req.Body}})
		case "GET /v1/approvals":
			_ = json.NewEncoder(w).Encode(schema.ListApprovalsResponseV1{})
		case "POST /v1/approvals/1/decisions":
			var req schema.CastDecisionRequestV1
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.decided = req.PrincipalID
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(schema.CastDecisionResponseV1{State: schema.ApprovalStateResolved, Resolution: &schema.ApprovalResolutionV1{}})
		case "GET /v0/ws":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			_ = wsjson.Write(r.Context(), conn, schema.MessageV0{ID: 1, AuthorID: 42, Body: "hi", CreatedAt: time.Now()})
			_ = conn.Close(websocket.StatusGoingAway, "bye")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *authFake) headerFor(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth[key]
}

func runCLI(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = RunWithStdin(context.Background(), args, strings.NewReader(stdin), &out, &errOut, "vtest")
	return out.String(), errOut.String(), err
}

func TestSubcommandsSendCredential(t *testing.T) {
	subcommands := []struct {
		name string
		args []string // --server is appended after the verb
		key  string   // the request whose header is asserted
	}{
		{"send", []string{"send", "--author", "42", "general", "hello"}, "POST /v0/channels/general/messages"},
		{"tail", []string{"tail", "general"}, "GET /v0/ws"},
		{"approvals list", []string{"approvals", "list"}, "GET /v1/approvals"},
		{"approve", []string{"approve", "--author", "42", "--reason", "ok", "1"}, "POST /v1/approvals/1/decisions"},
		{"reject", []string{"reject", "--author", "42", "--reason", "no", "1"}, "POST /v1/approvals/1/decisions"},
		{"whoami", []string{"whoami"}, "GET /v1/whoami"},
	}
	for _, sc := range subcommands {
		for _, source := range []string{"env", "file", "none"} {
			t.Run(sc.name+"/"+source, func(t *testing.T) {
				isolateConfig(t)
				fake := newAuthFake(t, false)
				switch source {
				case "env":
					t.Setenv("CONCH_TOKEN", fakeToken)
				case "file":
					if err := SaveToken(fake.URL, fakeToken); err != nil {
						t.Fatal(err)
					}
				}
				args := reorderServer(sc.args, fake.URL)
				_, _, err := runCLI(t, "", args...)
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				want := ""
				if source != "none" {
					want = "Bearer " + fakeToken
				}
				if got := fake.headerFor(sc.key); got != want {
					t.Errorf("Authorization on %s = %q, want %q", sc.key, got, want)
				}
				// Every request the command made must agree, not just the main one.
				fake.mu.Lock()
				defer fake.mu.Unlock()
				for k, v := range fake.auth {
					if v != want {
						t.Errorf("Authorization on %s = %q, want %q", k, v, want)
					}
				}
			})
		}
	}
}

// reorderServer inserts --server after the verb (and its sub-verb) so flags
// precede positional arguments.
func reorderServer(args []string, server string) []string {
	n := 1
	if args[0] == "approvals" {
		n = 2
	}
	out := append([]string{}, args[:n]...)
	out = append(out, "--server", server)
	return append(out, args[n:]...)
}

func TestAuthorResolution(t *testing.T) {
	const warning = "warning: --author is deprecated; identity comes from your login"
	tests := []struct {
		name        string
		credential  bool
		author      string
		wantErr     string
		wantWarning bool
		wantAs      int64
		wantSent    bool
	}{
		{name: "credential only", credential: true, wantAs: 42, wantSent: true},
		{name: "credential + matching author", credential: true, author: "42", wantAs: 42, wantSent: true, wantWarning: true},
		{name: "credential + different author", credential: true, author: "99", wantErr: "does not match your login"},
		{name: "credential + garbage author", credential: true, author: "abc", wantErr: "positive integer"},
		{name: "no credential + author", author: "7", wantAs: 7, wantSent: true},
		{name: "no credential, no author", wantErr: "--author (or CONCH_AUTHOR) is required"},
	}
	for _, verb := range []string{"send", "approve", "reject"} {
		for _, tt := range tests {
			t.Run(verb+"/"+tt.name, func(t *testing.T) {
				isolateConfig(t)
				fake := newAuthFake(t, tt.credential)
				if tt.credential {
					t.Setenv("CONCH_TOKEN", fakeToken)
				}
				args := []string{verb, "--server", fake.URL}
				if tt.author != "" {
					args = append(args, "--author", tt.author)
				}
				if verb == "send" {
					args = append(args, "general", "hello")
				} else {
					args = append(args, "--reason", "because", "1")
				}
				_, stderr, err := runCLI(t, "", args...)
				if tt.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
					}
					fake.mu.Lock()
					defer fake.mu.Unlock()
					for _, req := range fake.requests {
						if strings.HasPrefix(req, "POST") {
							t.Errorf("nothing should be sent, saw %s", req)
						}
					}
					return
				}
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				if got := strings.Contains(stderr, warning); got != tt.wantWarning {
					t.Errorf("warning present = %v, want %v (stderr %q)", got, tt.wantWarning, stderr)
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				got := fake.sentAs
				if verb != "send" {
					got = fake.decided
				}
				if got != tt.wantAs {
					t.Errorf("acted as %d, want %d", got, tt.wantAs)
				}
			})
		}
	}
}

func TestEnvAuthorUsedWithoutCredential(t *testing.T) {
	isolateConfig(t)
	fake := newAuthFake(t, false)
	t.Setenv("CONCH_AUTHOR", "9")
	if _, _, err := runCLI(t, "", "send", "--server", fake.URL, "general", "x"); err != nil {
		t.Fatal(err)
	}
	if fake.sentAs != 9 {
		t.Errorf("sent as %d, want 9", fake.sentAs)
	}
}

func TestUnauthenticatedHint(t *testing.T) {
	commands := map[string][]string{
		"send":           {"send", "--author", "42", "general", "hello"},
		"tail":           {"tail", "general"},
		"approvals list": {"approvals", "list"},
		"approve":        {"approve", "--author", "42", "--reason", "r", "1"},
		"reject":         {"reject", "--author", "42", "--reason", "r", "1"},
		"whoami":         {"whoami"},
		"send+token":     {"send", "general", "hello"},
	}
	for name, args := range commands {
		t.Run(name, func(t *testing.T) {
			isolateConfig(t)
			fake := newAuthFake(t, true) // every request without the right token is a 401
			if name == "send+token" {
				t.Setenv("CONCH_TOKEN", "a-revoked-token")
			}
			stdout, _, err := runCLI(t, "", reorderServer(args, fake.URL)...)
			want := "not logged in to " + fake.URL + ": run 'conch login'"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if !errors.Is(err, ErrUnauthenticated) {
				t.Error("error should match ErrUnauthenticated")
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if strings.Contains(err.Error(), "a-revoked-token") {
				t.Error("error leaks token")
			}
		})
	}
}

func TestLogin(t *testing.T) {
	tests := []struct {
		name       string
		stdin      string
		wantErr    string
		wantStored bool
	}{
		{name: "success", stdin: fakeToken + "\n", wantStored: true},
		{name: "no trailing newline", stdin: fakeToken, wantStored: true},
		{name: "surrounding whitespace", stdin: "  \t" + fakeToken + " \r\n", wantStored: true},
		{name: "rejected", stdin: "wrong-token-value\n", wantErr: "login failed: token rejected (or the server is not running with --auth required)"},
		{name: "empty", stdin: "\n", wantErr: "no token provided"},
		{name: "eof", stdin: "", wantErr: "no token provided"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := newAuthFake(t, true)
			stdout, stderr, err := runCLI(t, tt.stdin, "login", "--server", fake.URL+"/")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("login: %v", err)
			} else if want := "logged in to " + fake.URL + " as nick (operator)\n"; stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			stored, loadErr := LoadToken(fake.URL)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if tt.wantStored && stored != fakeToken {
				t.Errorf("stored = %q, want the trimmed token", stored)
			}
			if !tt.wantStored && stored != "" {
				t.Errorf("a failed login stored %q", stored)
			}
			if !tt.wantStored {
				if _, statErr := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "conch", "credentials.json")); !os.IsNotExist(statErr) { //nolint:gosec // test-local path
					t.Errorf("a failed login created the credentials file (%v)", statErr)
				}
			}
			for _, secret := range []string{strings.TrimSpace(tt.stdin)} {
				if secret == "" {
					continue
				}
				if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
					t.Errorf("token appears in output: stdout %q stderr %q err %v", stdout, stderr, err)
				}
			}
		})
	}
}

func TestLoginThenUseStoredCredential(t *testing.T) {
	isolateConfig(t)
	fake := newAuthFake(t, true)
	if _, _, err := runCLI(t, fakeToken, "login", "--server", fake.URL); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCLI(t, "", "whoami", "--server", fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := "id: 42\nkind: human\nname: nick\nrole: operator\n"; stdout != want {
		t.Errorf("whoami = %q, want %q", stdout, want)
	}
	if _, _, err := runCLI(t, "", "send", "--server", fake.URL, "general", "hello"); err != nil {
		t.Fatal(err)
	}
	if fake.sentAs != 42 {
		t.Errorf("sent as %d, want 42", fake.sentAs)
	}
	if _, _, err := runCLI(t, "", "logout", "--server", fake.URL); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, "", "send", "--server", fake.URL, "general", "hello")
	if err == nil || !strings.Contains(err.Error(), "--author") {
		t.Errorf("after logout, err = %v, want the legacy author requirement", err)
	}
	// Logging out twice is quiet.
	if _, _, err := runCLI(t, "", "logout", "--server", fake.URL); err != nil {
		t.Errorf("second logout: %v", err)
	}
}

func TestHelpDoesNotMentionTokenFlag(t *testing.T) {
	var out bytes.Buffer
	Usage(&out)
	if strings.Contains(out.String(), "--token") {
		t.Error("help must not offer a token flag")
	}
	if _, _, err := runCLI(t, "", "login", "--token", "x"); err == nil {
		t.Error("login must not accept --token")
	}
}
