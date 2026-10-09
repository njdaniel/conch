package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
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
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponseV2{})
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
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponseV2{})
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
		case "POST /v2/channels/general/messages":
			var req schema.PostMessageRequestV2
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.sentAs = req.AuthorID
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(schema.PostMessageResponseV2{Message: schema.MessageV2{ID: 1, AuthorID: req.AuthorID, Body: req.Body}})
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
		case "GET /v2/ws":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			_ = wsjson.Write(r.Context(), conn, schema.MessageV2{ID: 1, AuthorID: 42, Body: "hi", CreatedAt: schema.NewTimestamp(time.Now())})
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
		{"send", []string{"send", "--author", "42", "general", "hello"}, "POST /v2/channels/general/messages"},
		{"tail", []string{"tail", "general"}, "GET /v2/ws"},
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

// fakeTerminal makes readToken treat any *os.File as a terminal and routes the
// no-echo read through read.
func fakeTerminal(t *testing.T, read func(ctx context.Context, fd int) ([]byte, error)) *os.File {
	t.Helper()
	oldIs, oldRead := isTerminalFD, readSecret
	isTerminalFD = func(int) bool { return true }
	readSecret = read
	t.Cleanup(func() { isTerminalFD, readSecret = oldIs, oldRead })
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestReadTokenTerminal(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context //nolint:containedctx // table field
		read    func(context.Context, int) ([]byte, error)
		want    string
		wantErr string
	}{
		{name: "success", read: func(context.Context, int) ([]byte, error) { return []byte(" " + fakeToken + "\r"), nil }, want: fakeToken},
		{name: "empty", read: func(context.Context, int) ([]byte, error) { return nil, nil }, wantErr: "no token provided"},
		{name: "ctrl-d", read: func(context.Context, int) ([]byte, error) { return nil, io.EOF }, wantErr: "no token provided"},
		{
			name:    "read error",
			read:    func(context.Context, int) ([]byte, error) { return nil, errors.New("input/output error") },
			wantErr: "cannot read the token without echo (input/output error); use 'conch login < tokenfile'",
		},
		{
			name:    "cannot disable echo",
			read:    func(context.Context, int) ([]byte, error) { return nil, syscall.ENOTTY },
			wantErr: "use 'conch login < tokenfile'",
		},
		{
			name:    "interrupted",
			ctx:     canceled,
			read:    func(ctx context.Context, _ int) ([]byte, error) { return nil, ctx.Err() },
			wantErr: context.Canceled.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin := fakeTerminal(t, tt.read)
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			var stderr bytes.Buffer
			got, err := readToken(ctx, stdin, &stderr)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if got != "" {
					t.Errorf("token = %q on error", got)
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("readToken = %q, %v; want %q", got, err, tt.want)
			}
			if want := "Token: \n"; stderr.String() != want {
				t.Errorf("stderr = %q, want %q", stderr.String(), want)
			}
			if strings.Contains(stderr.String(), fakeToken) || (err != nil && strings.Contains(err.Error(), fakeToken)) {
				t.Error("token leaked to stderr or the error")
			}
		})
	}
}

// A failed terminal read must neither store a credential nor reach the server,
// and the token must not appear on any stream.
func TestLoginTerminalPaths(t *testing.T) {
	tests := []struct {
		name       string
		read       func(context.Context, int) ([]byte, error)
		wantErr    string
		wantStored bool
	}{
		{name: "success", read: func(context.Context, int) ([]byte, error) { return []byte(fakeToken), nil }, wantStored: true},
		{name: "echo unavailable", read: func(context.Context, int) ([]byte, error) { return nil, syscall.ENOTTY }, wantErr: "conch login < tokenfile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := newAuthFake(t, true)
			stdin := fakeTerminal(t, tt.read)
			var out, errOut bytes.Buffer
			err := RunWithStdin(context.Background(), []string{"login", "--server", fake.URL}, stdin, &out, &errOut, "vtest")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if len(fake.requests) != 0 {
					t.Errorf("server was contacted: %v", fake.requests)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			stored, loadErr := LoadToken(fake.URL)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if tt.wantStored != (stored == fakeToken) {
				t.Errorf("stored = %q, wantStored %v", stored, tt.wantStored)
			}
			if strings.Contains(out.String(), fakeToken) || strings.Contains(errOut.String(), fakeToken) || (err != nil && strings.Contains(err.Error(), fakeToken)) {
				t.Errorf("token appears in output: stdout %q stderr %q err %v", out.String(), errOut.String(), err)
			}
			if strings.Contains(errOut.String(), "visible") {
				t.Errorf("stale visible-token warning: %q", errOut.String())
			}
		})
	}
}

// With stdin an *os.File that is not a terminal (conch login < tokenfile), the
// no-echo path must not run and nothing extra is written to stderr.
func TestReadTokenFileStdinIsNotATerminal(t *testing.T) {
	oldRead := readSecret
	readSecret = func(context.Context, int) ([]byte, error) {
		t.Error("terminal read used for a regular file")
		return nil, nil
	}
	t.Cleanup(func() { readSecret = oldRead })
	path := filepath.Join(t.TempDir(), "tokenfile")
	if err := os.WriteFile(path, []byte(fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // test-local path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	var stderr bytes.Buffer
	got, err := readToken(context.Background(), f, &stderr)
	if err != nil || got != fakeToken || stderr.Len() != 0 {
		t.Fatalf("readToken = %q, %v, stderr %q", got, err, stderr.String())
	}
}

// netsFake is a conchd stand-in for the v2 message and nets routes. It records
// every request line and the bodies it received, and can be told to refuse a
// route with a server error.
type netsFake struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	posted   []schema.PostMessageRequestV2
	puts     []schema.PutNetMemberRequestV1
	nets     []schema.NetV1
	stream   []schema.MessageV2
	refuse   map[string]string // "METHOD path" -> server error code
}

func newNetsFake(t *testing.T) *netsFake {
	t.Helper()
	f := &netsFake{
		nets: []schema.NetV1{
			{ID: 4, Name: "alpha", Members: []schema.NetMember{{PrincipalID: 7, Role: schema.NetRoleMember}, {PrincipalID: 9, Role: schema.NetRoleMonitor}}},
			{ID: 5, Name: "bravo", Members: []schema.NetMember{}},
		},
		refuse: map[string]string{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.requests = append(f.requests, key)
		code := f.refuse[key]
		f.mu.Unlock()
		if code != "" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(schema.Error{Code: code, Message: "refused by test"})
			return
		}
		switch key {
		case "POST /v2/channels/general/messages":
			var req schema.PostMessageRequestV2
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.posted = append(f.posted, req)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(schema.PostMessageResponseV2{Message: schema.MessageV2{ID: 1, Body: req.Body}})
		case "GET /v1/channels/general/nets":
			_ = json.NewEncoder(w).Encode(schema.ListNetsResponseV1{Nets: f.nets})
		case "POST /v1/channels/general/nets":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(schema.CreateNetResponseV1{Net: schema.NetV1{ID: 6, Name: "charlie"}})
		case "DELETE /v1/channels/general/nets/alpha",
			"DELETE /v1/channels/general/nets/alpha/members/9":
			w.WriteHeader(http.StatusNoContent)
		case "PUT /v1/channels/general/nets/alpha/members/9":
			var req schema.PutNetMemberRequestV1
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.puts = append(f.puts, req)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "GET /v2/ws":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			for _, m := range f.stream {
				_ = wsjson.Write(r.Context(), conn, m)
			}
			_ = conn.Close(websocket.StatusGoingAway, "bye")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *netsFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func TestSendScopes(t *testing.T) {
	tests := []struct {
		name         string
		flags        []string
		wantAudience *schema.Audience
		wantNotice   bool
		wantErr      string
		wantNoPost   bool
		wantNoReq    bool // no request of any kind may leave the client
	}{
		{name: "channel-wide", flags: nil},
		{name: "net", flags: []string{"--net", "alpha"}, wantAudience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}},
		{name: "whisper one", flags: []string{"--to", "3"}, wantNotice: true,
			wantAudience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{3}}},
		{name: "whisper many keeps order", flags: []string{"--to", "9,3,5"}, wantNotice: true,
			wantAudience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{9, 3, 5}}},
		{name: "both flags", flags: []string{"--net", "alpha", "--to", "3"}, wantErr: "cannot be used together", wantNoPost: true, wantNoReq: true},
		{name: "unknown net", flags: []string{"--net", "zulu"}, wantErr: `no net "zulu"`, wantNoPost: true},
		{name: "invalid net name", flags: []string{"--net", "Bad Name"}, wantErr: "--net", wantNoPost: true, wantNoReq: true},
		{name: "to not a number", flags: []string{"--to", "3,x"}, wantErr: "--to", wantNoPost: true, wantNoReq: true},
		{name: "to empty element", flags: []string{"--to", "3,,5"}, wantErr: "--to", wantNoPost: true, wantNoReq: true},
		{name: "to zero", flags: []string{"--to", "0"}, wantErr: "--to", wantNoPost: true, wantNoReq: true},
		{name: "to negative", flags: []string{"--to", "-2"}, wantErr: "--to", wantNoPost: true, wantNoReq: true},
		{name: "to duplicate", flags: []string{"--to", "3,3"}, wantErr: "listed twice", wantNoPost: true, wantNoReq: true},
		{name: "to with spaces", flags: []string{"--to", " 9, 3 ,5"}, wantNotice: true,
			wantAudience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{9, 3, 5}}},
		{name: "to duplicate with spaces", flags: []string{"--to", "3, 3"}, wantErr: "listed twice", wantNoPost: true, wantNoReq: true},
		{name: "to blank element", flags: []string{"--to", "3, ,5"}, wantErr: "--to", wantNoPost: true, wantNoReq: true},
		// A flag given with an empty value (an unset shell variable) is a
		// scope the user asked for and did not get: never a channel-wide post.
		{name: "net given but empty", flags: []string{"--net", ""}, wantErr: "--net", wantNoPost: true, wantNoReq: true},
		{name: "net given as empty with =", flags: []string{"--net="}, wantErr: "--net", wantNoPost: true, wantNoReq: true},
		{name: "to given but empty", flags: []string{"--to", ""}, wantErr: "--to", wantNoPost: true, wantNoReq: true},
		{name: "to given but blank", flags: []string{"--to", " "}, wantErr: "--to", wantNoPost: true, wantNoReq: true},
		{name: "empty net with to", flags: []string{"--net", "", "--to", "3"}, wantErr: "cannot be used together", wantNoPost: true, wantNoReq: true},
		{name: "net with empty to", flags: []string{"--net", "alpha", "--to", ""}, wantErr: "cannot be used together", wantNoPost: true, wantNoReq: true},
	}
	const notice = "note: whispers are recorded in the audit log\n"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := newNetsFake(t)
			args := append([]string{"send", "--server", fake.URL, "--author", "7"}, tt.flags...)
			args = append(args, "general", "hello")
			stdout, stderr, err := runCLI(t, "", args...)
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if strings.Contains(stderr, notice) {
					t.Error("the audit-log notice must not print when nothing was sent")
				}
			} else if err != nil {
				t.Fatalf("run: %v", err)
			}
			if tt.wantNoPost && len(fake.posted) != 0 {
				t.Errorf("posted %+v, want nothing", fake.posted)
			}
			if tt.wantNoReq && len(fake.seen()) != 0 {
				t.Errorf("requests = %v, want none", fake.seen())
			}
			if tt.wantErr == "" {
				if len(fake.posted) != 1 {
					t.Fatalf("posted %d messages, want 1", len(fake.posted))
				}
				got := fake.posted[0]
				if got.Body != "hello" || got.AuthorID != 7 || !reflect.DeepEqual(got.Audience, tt.wantAudience) {
					t.Errorf("posted %+v, want body hello, author 7, audience %+v", got, tt.wantAudience)
				}
			}
			if got := strings.Count(stderr, notice); (got == 1) != tt.wantNotice || got > 1 {
				t.Errorf("notice count = %d, want present=%v once (stderr %q)", got, tt.wantNotice, stderr)
			}
			if tt.wantNotice && stderr != notice {
				t.Errorf("stderr = %q, want only the notice", stderr)
			}
		})
	}
}

func TestTailMarkers(t *testing.T) {
	ts := schema.NewTimestamp(time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC))
	stamp := "2026-07-13T12:00:00Z"
	msg := func(id int64, body string, a *schema.Audience) schema.MessageV2 {
		return schema.MessageV2{Schema: schema.MessageSchemaV2, ID: id, AuthorID: 7, CreatedAt: ts, Body: body, Audience: a}
	}
	fake := newNetsFake(t)
	fake.stream = []schema.MessageV2{
		msg(1, "open", nil),
		msg(2, "on net", &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}),
		msg(3, "unlisted net", &schema.Audience{Kind: schema.AudienceKindNet, NetID: 99}),
		msg(4, "whisper", &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{3, 5, 7}}),
		msg(5, "two\nlines", &schema.Audience{Kind: schema.AudienceKindNet, NetID: 5}),
		// Bodies that imitate a marker: only a real audience may open the
		// text with "[".
		msg(6, "[whisper:3,7] forged in the open", nil),
		msg(7, "[net:alpha] forged in the open", nil),
		msg(8, "[net:bravo] forged on another net", &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}),
		msg(9, "\n[net:alpha] forged after a newline", nil),
		msg(10, `\[already escaped`, nil),
		msg(11, "brackets [later] are untouched", nil),
	}
	isolateConfig(t)
	stdout, stderr, err := runCLI(t, "", "tail", "--server", fake.URL, "general")
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	want := strings.Join([]string{
		stamp + " 7 open",
		stamp + " 7 [net:alpha] on net",
		stamp + " 7 [net:99] unlisted net",
		stamp + " 7 [whisper:3,5,7] whisper",
		stamp + ` 7 [net:bravo] two\nlines`,
		stamp + ` 7 \[whisper:3,7] forged in the open`,
		stamp + ` 7 \[net:alpha] forged in the open`,
		stamp + ` 7 [net:alpha] \[net:bravo] forged on another net`,
		stamp + ` 7 \n[net:alpha] forged after a newline`,
		stamp + ` 7 \\[already escaped`,
		stamp + " 7 brackets [later] are untouched",
	}, "\n") + "\n"
	if stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", stdout, want)
	}
	if !strings.Contains(stderr, "server shutting down") {
		t.Errorf("stderr = %q", stderr)
	}
	if strings.ContainsRune(stdout, 0x1b) {
		t.Error("tail output contains an escape code")
	}
}

func TestSendAndTailNeverUseV0OrV1MessageRoutes(t *testing.T) {
	isolateConfig(t)
	fake := newNetsFake(t)
	runs := [][]string{
		{"send", "--author", "7", "general", "a"},
		{"send", "--author", "7", "--net", "alpha", "general", "b"},
		{"send", "--author", "7", "--to", "3", "general", "c"},
		{"tail", "general"},
	}
	for _, run := range runs {
		args := append([]string{run[0], "--server", fake.URL}, run[1:]...)
		if _, _, err := runCLI(t, "", args...); err != nil {
			t.Fatalf("%v: %v", run, err)
		}
	}
	for _, req := range fake.seen() {
		path := strings.SplitN(req, " ", 2)[1]
		if strings.HasPrefix(path, "/v0/") || (strings.HasPrefix(path, "/v1/") && strings.Contains(path, "/messages")) || path == "/v1/ws" {
			t.Errorf("used a v0/v1 message route: %s", req)
		}
	}
	if len(fake.seen()) == 0 {
		t.Fatal("no requests recorded")
	}
}

func TestNetsCommands(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantReq    string
		wantStdout string
		wantPut    *schema.PutNetMemberRequestV1
	}{
		{name: "list", args: []string{"nets", "list", "general"}, wantReq: "GET /v1/channels/general/nets",
			wantStdout: "alpha  7:member 9:monitor\nbravo  (empty)\n"},
		{name: "create", args: []string{"nets", "create", "general", "charlie"}, wantReq: "POST /v1/channels/general/nets",
			wantStdout: "created net charlie (id 6) in general\n"},
		{name: "archive", args: []string{"nets", "archive", "general", "alpha"}, wantReq: "DELETE /v1/channels/general/nets/alpha",
			wantStdout: "archived net alpha in general\n"},
		{name: "add member", args: []string{"nets", "add", "general", "alpha", "9"}, wantReq: "PUT /v1/channels/general/nets/alpha/members/9",
			wantStdout: "added 9 to net alpha as member\n", wantPut: &schema.PutNetMemberRequestV1{Role: schema.NetRoleMember}},
		{name: "add monitor", args: []string{"nets", "add", "general", "alpha", "9", "--monitor"}, wantReq: "PUT /v1/channels/general/nets/alpha/members/9",
			wantStdout: "added 9 to net alpha as monitor\n", wantPut: &schema.PutNetMemberRequestV1{Role: schema.NetRoleMonitor}},
		{name: "add monitor flag first", args: []string{"nets", "add", "--monitor", "general", "alpha", "9"}, wantReq: "PUT /v1/channels/general/nets/alpha/members/9",
			wantStdout: "added 9 to net alpha as monitor\n", wantPut: &schema.PutNetMemberRequestV1{Role: schema.NetRoleMonitor}},
		{name: "remove", args: []string{"nets", "remove", "general", "alpha", "9"}, wantReq: "DELETE /v1/channels/general/nets/alpha/members/9",
			wantStdout: "removed 9 from net alpha\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := newNetsFake(t)
			args := append([]string{tt.args[0], tt.args[1], "--server", fake.URL}, tt.args[2:]...)
			stdout, stderr, err := runCLI(t, "", args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
			if got := fake.seen(); !reflect.DeepEqual(got, []string{tt.wantReq}) {
				t.Errorf("requests = %v, want [%s]", got, tt.wantReq)
			}
			if tt.wantPut != nil && (len(fake.puts) != 1 || fake.puts[0] != *tt.wantPut) {
				t.Errorf("put body = %+v, want %+v", fake.puts, *tt.wantPut)
			}
		})
	}
}

func TestNetsUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", []string{"nets"}, "requires a subcommand"},
		{"unknown subcommand", []string{"nets", "frob"}, "unknown nets subcommand"},
		{"list without channel", []string{"nets", "list"}, "expected <channel>"},
		{"create without name", []string{"nets", "create", "general"}, "expected <channel> <name>"},
		{"add without id", []string{"nets", "add", "general", "alpha"}, "expected <channel> <name> <principal-id>"},
		{"add bad id", []string{"nets", "add", "general", "alpha", "x"}, "positive integer"},
		{"remove zero id", []string{"nets", "remove", "general", "alpha", "0"}, "positive integer"},
		{"monitor only on add", []string{"nets", "remove", "--monitor", "general", "alpha", "9"}, "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := newNetsFake(t)
			args := tt.args
			if len(args) > 1 && args[1] != "frob" {
				args = append([]string{args[0], args[1], "--server", fake.URL}, args[2:]...)
			}
			_, _, err := runCLI(t, "", args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if len(fake.seen()) != 0 {
				t.Errorf("requests = %v, want none", fake.seen())
			}
		})
	}
}

func TestScopedCommandsReportServerRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		key  string
		code string
	}{
		{"send net", []string{"send", "--author", "7", "--net", "alpha", "general", "x"}, "POST /v2/channels/general/messages", "forbidden"},
		{"send whisper", []string{"send", "--author", "7", "--to", "3", "general", "x"}, "POST /v2/channels/general/messages", "invalid_audience"},
		{"send net list refused", []string{"send", "--author", "7", "--net", "alpha", "general", "x"}, "GET /v1/channels/general/nets", "channel_not_found"},
		{"nets list", []string{"nets", "list", "general"}, "GET /v1/channels/general/nets", "channel_not_found"},
		{"nets create", []string{"nets", "create", "general", "charlie"}, "POST /v1/channels/general/nets", "net_exists"},
		{"nets archive", []string{"nets", "archive", "general", "alpha"}, "DELETE /v1/channels/general/nets/alpha", "net_not_found"},
		{"nets add", []string{"nets", "add", "general", "alpha", "9"}, "PUT /v1/channels/general/nets/alpha/members/9", "not_a_channel_member"},
		{"nets remove", []string{"nets", "remove", "general", "alpha", "9"}, "DELETE /v1/channels/general/nets/alpha/members/9", "forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := newNetsFake(t)
			fake.refuse[tt.key] = tt.code
			verbs := 1
			if tt.args[0] == "nets" {
				verbs = 2
			}
			args := append(append(append([]string{}, tt.args[:verbs]...), "--server", fake.URL), tt.args[verbs:]...)
			stdout, _, err := runCLI(t, "", args...)
			if err == nil || !strings.Contains(err.Error(), tt.code) {
				t.Fatalf("err = %v, want containing %q", err, tt.code)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("error is more than one line: %q", err)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty on refusal", stdout)
			}
		})
	}
}

func TestSendNoticeNotPrintedWhenWhisperRefused(t *testing.T) {
	isolateConfig(t)
	fake := newNetsFake(t)
	fake.refuse["POST /v2/channels/general/messages"] = "invalid_audience"
	_, stderr, err := runCLI(t, "", "send", "--server", fake.URL, "--author", "7", "--to", "3", "general", "x")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(stderr, "audit log") {
		t.Errorf("stderr = %q, the notice must not claim a whisper was recorded", stderr)
	}
}

func TestUsageMentionsNetsAndWhispers(t *testing.T) {
	var out bytes.Buffer
	Usage(&out)
	for _, want := range []string{"--net <name>", "--to <id>", "conch nets list", "conch nets create", "conch nets archive", "conch nets add", "--monitor", "conch nets remove"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage lacks %q", want)
		}
	}
}
