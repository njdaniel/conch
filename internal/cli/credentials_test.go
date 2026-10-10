package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"
)

// TestMain keeps every test away from the real user configuration and from a
// CONCH_TOKEN in the developer's environment.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "conch-cli-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("XDG_CONFIG_HOME", dir)
	userConfigDir = func() (string, error) { return dir, nil }
	_ = os.Unsetenv("CONCH_TOKEN")
	_ = os.Unsetenv("CONCH_AUTHOR")
	_ = os.Unsetenv("CONCH_SERVER")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// isolateConfig points the config directory at a fresh temp dir for one test.
func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("CONCH_TOKEN", "")
	old := userConfigDir
	userConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { userConfigDir = old })
	return filepath.Join(dir, "conch", "credentials.json")
}

func TestNormalizeServer(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{in: "http://Host:8080/", want: "http://host:8080"},
		{in: "HTTPS://Example.COM/some/Path/", want: "https://example.com/some/Path"},
		{in: "http://Host:8080/", want: "http://host:8080"},
		{in: "http://host:8080//", want: "http://host:8080"},
		{in: "ftp://host", wantErr: true},
		{in: "host:8080", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := NormalizeServer(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("NormalizeServer(%q) = %q, %v; want %q (err %v)", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestNormalizeServerSharedVectors runs testdata/credentials-vectors.json
// through NormalizeServer. The same file drives a test of the Rust voice
// client (voice/crates/conch-voice-api/src/server.rs), which reads the login
// this package stores: the key a server address is stored under is a contract
// between the two programs (docs/design/conch-voice.md §8), and this file is
// what keeps them from drifting.
//
// Every vector was produced by running NormalizeServer, none by hand. So a
// failure here means that what a key is has changed, in this package or
// underneath it (net/url, or the Unicode tables strings.ToLower uses): logins
// stored under the old key are no longer found, and the Rust side has to
// change in the same pull request.
//
// Two changes underneath are expected, and the file is built to notice both.
// The vectors cannot hold every letter, so the Unicode version is asserted
// outright. And Go releases after 1.25.0 refuse a bracketed host that is not
// an IPv6 address (http://[evil.com]); ten vectors have such a host, so this
// test fails when go.mod moves to one of those releases.
func TestNormalizeServerSharedVectors(t *testing.T) {
	// The Unicode version the Go and Rust lower-casing were compared under.
	const comparedUnderUnicode = "15.0.0"
	if unicode.Version != comparedUnderUnicode {
		t.Fatalf("this Go lower-cases with Unicode %s, and the shared vectors were made under %s. "+
			"Run the comparison again: every code point as a host (http://<c>.x) through NormalizeServer "+
			"and through the Rust ServerAddress::parse. Then bring newer_than_gos_tables in "+
			"voice/crates/conch-voice-api/src/server.rs and testdata/credentials-vectors.json up to date, "+
			"and this constant last.", unicode.Version, comparedUnderUnicode)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "credentials-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Input string  `json:"input"`
		Key   *string `json:"key"`
		Error bool    `json:"error"`
		Note  string  `json:"note"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&vectors); err != nil {
		t.Fatalf("credentials-vectors.json: %v", err)
	}
	if len(vectors) < 160 {
		t.Fatalf("credentials-vectors.json has %d vectors; the file was cut short", len(vectors))
	}
	keys, refusals := 0, 0
	for _, v := range vectors {
		t.Run(fmt.Sprintf("%q", v.Input), func(t *testing.T) {
			if (v.Key != nil) == v.Error {
				t.Fatalf("vector must have a key or error: true, not both and not neither (%s)", v.Note)
			}
			got, err := NormalizeServer(v.Input)
			if v.Error {
				if err == nil || got != "" {
					t.Fatalf("NormalizeServer(%q) = %q, %v; want an error (%s)", v.Input, got, err, v.Note)
				}
				return
			}
			if err != nil || got != *v.Key {
				t.Fatalf("NormalizeServer(%q) = %q, %v; want %q (%s)", v.Input, got, err, *v.Key, v.Note)
			}
		})
		if v.Error {
			refusals++
		} else {
			keys++
		}
	}
	if keys < 20 || refusals < 10 {
		t.Fatalf("credentials-vectors.json has %d keys and %d refusals; it must exercise both", keys, refusals)
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	path := isolateConfig(t)

	if tok, err := LoadToken("http://a:1"); err != nil || tok != "" {
		t.Fatalf("empty store: token %q err %v", tok, err)
	}
	if err := SaveToken("http://a:1", "tok-a"); err != nil {
		t.Fatal(err)
	}
	if err := SaveToken("https://b.example", "tok-b"); err != nil {
		t.Fatal(err)
	}
	for server, want := range map[string]string{
		"http://a:1":         "tok-a",
		"https://b.example/": "tok-b",
		"http://c:3":         "",
	} {
		if got, err := LoadToken(server); err != nil || got != want {
			t.Errorf("LoadToken(%q) = %q, %v; want %q", server, got, err, want)
		}
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %o, want 600", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %o, want 700", di.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("config dir has %d entries, want only credentials.json (temp file left behind?)", len(entries))
	}
}

func TestCredentialsURLNormalisationIsOneEntry(t *testing.T) {
	path := isolateConfig(t)
	if err := SaveToken("http://Host:8080/", "one"); err != nil {
		t.Fatal(err)
	}
	if err := SaveToken("http://host:8080", "two"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path) //nolint:gosec // test-local path
	if n := strings.Count(string(data), `"token"`); n != 1 {
		t.Errorf("file has %d entries, want 1:\n%s", n, data)
	}
	if got, _ := LoadToken("http://HOST:8080/"); got != "two" {
		t.Errorf("token = %q, want two", got)
	}
}

func TestCredentialsEnvPrecedence(t *testing.T) {
	isolateConfig(t)
	if err := SaveToken("http://a:1", "from-file"); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ env, want string }{
		{"", "from-file"},
		{"   ", "from-file"},
		{"from-env", "from-env"},
		{" from-env\n", "from-env"},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("CONCH_TOKEN", tt.env)
			if got, err := LoadToken("http://a:1"); err != nil || got != tt.want {
				t.Errorf("LoadToken = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	// The env var applies to every server, even one with nothing stored.
	t.Setenv("CONCH_TOKEN", "from-env")
	if got, _ := LoadToken("http://other:2"); got != "from-env" {
		t.Errorf("token = %q", got)
	}
}

func TestCredentialsRefuseLooseFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	path := isolateConfig(t)
	if err := SaveToken("http://a:1", "secret-token-value"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadToken("http://a:1")
		if err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %o: err = %v, want chmod hint", mode, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret-token-value") {
			t.Errorf("error leaks token: %v", err)
		}
		if err := SaveToken("http://b:2", "x"); err == nil {
			t.Errorf("mode %o: save over a loose file should be refused", mode)
		}
	}
}

func TestCredentialsMalformedFile(t *testing.T) {
	path := isolateConfig(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken("http://a:1"); err == nil {
		t.Error("want error for malformed file")
	}
}

func TestDeleteToken(t *testing.T) {
	path := isolateConfig(t)

	// Nothing stored: quiet success, no file created.
	if err := DeleteToken("http://a:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not exist: %v", err)
	}

	_ = SaveToken("http://a:1", "a")
	_ = SaveToken("http://b:2", "b")
	if err := DeleteToken("http://a:1/"); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadToken("http://a:1"); got != "" {
		t.Errorf("a still stored: %q", got)
	}
	if got, _ := LoadToken("http://b:2"); got != "b" {
		t.Errorf("b lost: %q", got)
	}
	// Removing an unknown server leaves the file alone.
	if err := DeleteToken("http://zzz:9"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should remain: %v", err)
	}
	// The last entry takes the file with it.
	if err := DeleteToken("http://b:2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be removed after last logout: %v", err)
	}
}

func TestCredentialsKeyIncludesPath(t *testing.T) {
	path := isolateConfig(t)
	_ = SaveToken("https://proxy.example/conch-a", "tok-a")
	_ = SaveToken("https://proxy.example/conch-b/", "tok-b")
	_ = SaveToken("https://proxy.example", "tok-root")
	for server, want := range map[string]string{
		"https://proxy.example/conch-a": "tok-a",
		"https://PROXY.example/conch-b": "tok-b",
		"https://proxy.example/":        "tok-root",
		"https://proxy.example/conch-A": "",
	} {
		if got, _ := LoadToken(server); got != want {
			t.Errorf("LoadToken(%q) = %q, want %q", server, got, want)
		}
	}
	data, _ := os.ReadFile(path) //nolint:gosec // test-local path
	if n := strings.Count(string(data), `"token"`); n != 3 {
		t.Errorf("want 3 entries:\n%s", data)
	}
}

func TestCredentialsTightenLooseDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	path := isolateConfig(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // deliberately loose
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // deliberately loose
		t.Fatal(err)
	}
	if err := SaveToken("http://a:1", "t"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 700", fi.Mode().Perm())
	}
}
