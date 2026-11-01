package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// deadDefault points defaultServer at a port nothing listens on, so a test
// that falls through to the default gets a refused connection rather than
// whatever happens to run on this machine's 8080. It returns that server.
//
// The port is picked on 127.0.0.2, not 127.0.0.1. A port freed on 127.0.0.1
// goes back into the pool every httptest.NewServer draws from, so under load
// this test's own named servers, or another package's, could take it and the
// "dead" default would answer (issue #1104). Nothing else in the tests binds
// a specific address other than 127.0.0.1, and every address in 127.0.0.0/8
// is loopback on Linux. Where 127.0.0.2 is not available the pick falls back
// to 127.0.0.1.
func deadDefault(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		l, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	server := "http://" + l.Addr().String()
	_ = l.Close()
	old := defaultServer
	defaultServer = server
	t.Cleanup(func() { defaultServer = old })
	return server
}

// namedServer answers whoami with name and counts the requests it gets.
type namedServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests int
}

func newNamedServer(t *testing.T, name string) *namedServer {
	t.Helper()
	s := &namedServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()
		if r.URL.Path != "/v1/whoami" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTeapot)
			_ = json.NewEncoder(w).Encode(schema.Error{Code: "test", Message: "recorded"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: name, Role: schema.RoleMember})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *namedServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// TestServerChoice pins the order a command chooses its server in (issue
// #522): --server, then CONCH_SERVER, then the only stored login's server,
// then the default.
func TestServerChoice(t *testing.T) {
	tests := []struct {
		name   string
		logins []string // "a" and "b" name the two test servers
		env    string   // CONCH_SERVER
		flag   string   // --server
		token  bool     // CONCH_TOKEN is set
		// want is the name whoami prints, or "" when the default is dialled.
		want string
		// wantStored is, for the default, the servers the error must name.
		wantStored []string
	}{
		{name: "none", want: ""},
		{name: "flag", flag: "a", want: "a"},
		{name: "variable", env: "a", want: "a"},
		{name: "flag over variable", flag: "b", env: "a", want: "b"},
		{name: "one login", logins: []string{"a"}, want: "a"},
		{name: "variable over one login", logins: []string{"a"}, env: "b", want: "b"},
		{name: "flag over one login", logins: []string{"a"}, flag: "b", want: "b"},
		{name: "several logins", logins: []string{"a", "b"}, want: "", wantStored: []string{"a", "b"}},
		{name: "several logins, flag", logins: []string{"a", "b"}, flag: "b", want: "b"},
		{name: "several logins, variable", logins: []string{"a", "b"}, env: "a", want: "a"},
		// CONCH_TOKEN is not the stored login, so the login does not choose
		// where it goes; the refusal still names the login.
		{name: "one login, CONCH_TOKEN", logins: []string{"a"}, token: true, want: "", wantStored: []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			dead := deadDefault(t)
			servers := map[string]*namedServer{"a": newNamedServer(t, "a"), "b": newNamedServer(t, "b")}
			for _, login := range tt.logins {
				if err := SaveToken(servers[login].URL, "tok-"+login); err != nil {
					t.Fatal(err)
				}
			}
			if tt.env != "" {
				t.Setenv("CONCH_SERVER", servers[tt.env].URL)
			}
			if tt.token {
				t.Setenv("CONCH_TOKEN", "tok-env")
			}
			args := []string{"whoami"}
			if tt.flag != "" {
				args = append(args, "--server", servers[tt.flag].URL)
			}
			stdout, _, err := runCLI(t, "", args...)
			if tt.want != "" {
				if err != nil {
					t.Fatalf("whoami: %v", err)
				}
				if !strings.Contains(stdout, "name: "+tt.want+"\n") {
					t.Fatalf("whoami printed %q, want it from server %s", stdout, tt.want)
				}
				for name, s := range servers {
					if name != tt.want && s.count() != 0 {
						t.Errorf("server %s got %d requests, want none", name, s.count())
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("whoami printed %q, want a refused connection to the default", stdout)
			}
			if !strings.Contains(err.Error(), strings.TrimPrefix(dead, "http://")) {
				t.Errorf("error %q does not name the default %s", err, dead)
			}
			for name, s := range servers {
				if s.count() != 0 {
					t.Errorf("server %s got %d requests, want none", name, s.count())
				}
			}
			if len(tt.wantStored) == 0 {
				if strings.Contains(err.Error(), "logins are stored") {
					t.Errorf("error %q names stored logins, want none named", err)
				}
				return
			}
			urls := make([]string, len(tt.wantStored))
			for i, name := range tt.wantStored {
				urls[i] = servers[name].URL
			}
			for _, u := range urls {
				if !strings.Contains(err.Error(), u) {
					t.Errorf("error %q does not name the stored login %s", err, u)
				}
			}
			if !strings.Contains(err.Error(), "--server <url> or CONCH_SERVER=<url>") {
				t.Errorf("error %q does not say how to choose a server", err)
			}
		})
	}
}

// With one stored login every command that talks to a server uses it without
// --server: they all take their default from the same rule.
func TestEveryCommandUsesTheOnlyLogin(t *testing.T) {
	isolateConfig(t)
	deadDefault(t)
	srv := newNamedServer(t, "a")
	if err := SaveToken(srv.URL, "tok-a"); err != nil {
		t.Fatal(err)
	}
	upload := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(upload, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"whoami"},
		{"send", "general", "hello"},
		{"tail", "general"},
		{"thread", "general", "1"},
		{"search", "general", "word"},
		{"upload", "general", upload},
		{"download", "-o", filepath.Join(t.TempDir(), "out"), "1"},
		{"nets", "list", "general"},
		{"admin", "channel", "create", "ops"},
		{"admin", "principal", "list"},
		{"voice", "status", "general"},
		{"audit", "export"},
		{"approvals", "list"},
		{"approve", "--reason", "ok", "1"},
		{"reject", "--reason", "no", "1"},
		{"rename", "newname"},
		{"report", "--did", "a", "--expected", "b", "--happened", "c", "--often", "once", "--yes"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args[:min(2, len(args))], " "), func(t *testing.T) {
			before := srv.count()
			_, _, err := runCLI(t, "", args...)
			if srv.count() == before {
				t.Errorf("%v made no request to the stored login's server (err %v)", args, err)
			}
		})
	}
	// login and logout without --server act on the same login.
	if _, _, err := runCLI(t, "tok-again", "login"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if token, err := LoadToken(srv.URL); err != nil || token != "tok-again" {
		t.Fatalf("after login the stored token is %q (%v), want the new one", token, err)
	}
	if _, _, err := runCLI(t, "", "logout"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if servers := storedServers(); len(servers) != 0 {
		t.Errorf("after logout logins are stored for %v, want none", servers)
	}
	if got := DefaultServer(); got != defaultServer {
		t.Errorf("DefaultServer() with no login = %q, want %q", got, defaultServer)
	}
}

// TestExplainUnreachable pins when a failed connection names the stored
// logins, and the exact words it uses.
func TestExplainUnreachable(t *testing.T) {
	const def = "http://127.0.0.1:8080"
	refused := fmt.Errorf("cli: whoami: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")})
	tests := []struct {
		name   string
		server string
		env    string
		logins []string
		err    error
		want   string // "" means err comes back unchanged
	}{
		{
			name: "several logins", server: def, logins: []string{"http://b:2", "http://a:1"}, err: refused,
			want: "cli: whoami: dial tcp: connect: connection refused (nothing answered at http://127.0.0.1:8080; logins are stored for http://a:1, http://b:2: choose one with --server <url> or CONCH_SERVER=<url>)",
		},
		{
			name: "the default's own login among others", server: def, logins: []string{def, "http://a:1"}, err: refused,
			want: "cli: whoami: dial tcp: connect: connection refused (nothing answered at http://127.0.0.1:8080; logins are stored for http://127.0.0.1:8080, http://a:1: choose one with --server <url> or CONCH_SERVER=<url>)",
		},
		{name: "no logins", server: def, err: refused},
		{name: "only the default's login", server: def, logins: []string{def}, err: refused},
		{name: "not the default", server: "http://a:1", logins: []string{"http://a:1", "http://b:2"}, err: refused},
		{name: "chosen by CONCH_SERVER", server: def, env: def, logins: []string{"http://a:1", "http://b:2"}, err: refused},
		{name: "not a connection failure", server: def, logins: []string{"http://a:1", "http://b:2"}, err: errors.New("cli: server error x: y")},
		{
			name: "interrupted", server: def, logins: []string{"http://a:1", "http://b:2"},
			err: &net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled},
		},
		{name: "no error", server: def, logins: []string{"http://a:1", "http://b:2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			old := defaultServer
			defaultServer = def
			t.Cleanup(func() { defaultServer = old })
			t.Setenv("CONCH_SERVER", tt.env)
			for _, login := range tt.logins {
				if err := SaveToken(login, "tok"); err != nil {
					t.Fatal(err)
				}
			}
			got := explainUnreachable(tt.server, tt.err)
			if tt.want == "" {
				if got != tt.err { //nolint:errorlint // the very same error must come back
					t.Fatalf("got %v, want the error unchanged (%v)", got, tt.err)
				}
				return
			}
			if got == nil || got.Error() != tt.want {
				t.Fatalf("got %q,\nwant %q", got, tt.want)
			}
			var op *net.OpError
			if !errors.As(got, &op) {
				t.Error("the network error is no longer reachable through the explained one")
			}
		})
	}
}

// storedServers lists only keys conch could have written: a hand-edited key
// that is not a normalized server URL names nothing to dial.
func TestStoredServersSkipsForeignKeys(t *testing.T) {
	path := isolateConfig(t)
	if err := SaveToken("http://Host:1/", "tok"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	var creds map[string]storedCredential
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatal(err)
	}
	creds["not a url"] = storedCredential{Token: "x"}
	creds["HTTP://Other:2/"] = storedCredential{Token: "y"}
	if err := writeCredentials(creds); err != nil {
		t.Fatal(err)
	}
	if got := storedServers(); len(got) != 1 || got[0] != "http://host:1" {
		t.Errorf("storedServers() = %v, want [http://host:1]", got)
	}
}
