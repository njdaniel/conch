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
	_ = os.Unsetenv(configDirEnv)
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
	t.Setenv(configDirEnv, "")
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

func TestCredentialsRefusedHosts(t *testing.T) {
	isolateConfig(t)
	refused := []string{
		"https://\u0130stanbul.example",
		"https://\u0130STANBUL.example:8443/conch",
		"http://conch.\u0130stanbul.example",
		"https://%C4%B0stanbul.example",
		"http://%c4%b0stanbul.example",
		"https://STRA\u1e9eE.example",
		"http://STRA\u1e9eE.example:8080/prefix",
		"https://stra%e1%ba%9ee.example",
		"https://stra%E1%BA%9Ee.example",
	}
	for _, raw := range refused {
		t.Run("load/"+raw, func(t *testing.T) {
			_, err := LoadToken(raw)
			if err == nil {
				t.Fatalf("LoadToken(%q) succeeded; want refusal", raw)
			}
			if !strings.Contains(err.Error(), "cannot be used safely") {
				t.Errorf("error = %v, want 'cannot be used safely'", err)
			}
			if strings.Contains(err.Error(), "stanbul") || strings.Contains(err.Error(), "stra") || strings.Contains(err.Error(), raw) {
				t.Errorf("error %q leaks server", err.Error())
			}
			// Environment variable must not bypass the refusal.
			t.Setenv("CONCH_TOKEN", "secret-token")
			_, err = LoadToken(raw)
			if err == nil || !strings.Contains(err.Error(), "cannot be used safely") {
				t.Errorf("LoadToken with CONCH_TOKEN = %v, want refusal", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-token") {
				t.Errorf("error %q leaks token", err.Error())
			}
		})
		t.Run("save/"+raw, func(t *testing.T) {
			err := SaveToken(raw, "my-secret-token")
			if err == nil {
				t.Fatalf("SaveToken(%q) succeeded; want refusal", raw)
			}
			if !strings.Contains(err.Error(), "cannot be used safely") {
				t.Errorf("error = %v, want 'cannot be used safely'", err)
			}
			if strings.Contains(err.Error(), "my-secret-token") || strings.Contains(err.Error(), raw) {
				t.Errorf("error %q leaks secret or server", err.Error())
			}
		})
		t.Run("delete/"+raw, func(t *testing.T) {
			err := DeleteToken(raw)
			if err == nil {
				t.Fatalf("DeleteToken(%q) succeeded; want refusal", raw)
			}
			if !strings.Contains(err.Error(), "cannot be used safely") {
				t.Errorf("error = %v, want 'cannot be used safely'", err)
			}
			if strings.Contains(err.Error(), raw) {
				t.Errorf("error %q leaks server", err.Error())
			}
		})
	}
}

// TestConfigDirSharedVectors runs testdata/config-dir-vectors.json through the
// real resolution (CONCH_CONFIG_DIR, then os.UserConfigDir's XDG_CONFIG_HOME and
// HOME). The same file drives a test of the Rust voice client
// (voice/crates/conch-voice-api/src/login.rs), which reads the credentials this
// package writes: both must look in the same directory from the same
// environment (docs/design/conch-voice.md §8).
func TestConfigDirSharedVectors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the vectors are Linux paths: os.UserConfigDir reads other variables elsewhere")
	}
	data, err := os.ReadFile(filepath.Join("testdata", "config-dir-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		ConchConfigDir string `json:"conch_config_dir"`
		XDGConfigHome  string `json:"xdg_config_home"`
		Home           string `json:"home"`
		Want           string `json:"want"`
		WantError      bool   `json:"want_error"`
		Note           string `json:"note"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&vectors); err != nil {
		t.Fatalf("config-dir-vectors.json: %v", err)
	}
	if len(vectors) < 15 {
		t.Fatalf("config-dir-vectors.json has %d vectors; the file was cut short", len(vectors))
	}
	old := userConfigDir
	userConfigDir = os.UserConfigDir
	t.Cleanup(func() { userConfigDir = old })
	for _, v := range vectors {
		t.Run(v.Note, func(t *testing.T) {
			t.Setenv(configDirEnv, v.ConchConfigDir)
			t.Setenv("XDG_CONFIG_HOME", v.XDGConfigHome)
			t.Setenv("HOME", v.Home)
			got, err := configDir()
			if v.WantError {
				if err == nil || got != "" || !strings.Contains(err.Error(), configDirEnv) {
					t.Fatalf("configDir() = %q, %v; want an error naming %s", got, err, configDirEnv)
				}
				return
			}
			if err != nil || got != v.Want {
				t.Fatalf("configDir() = %q, %v; want %q", got, err, v.Want)
			}
		})
	}
}

// TestCredentialsRelativeConfigDirRefused: a relative CONCH_CONFIG_DIR fails
// every operation with one line naming the variable, and writes nothing.
func TestCredentialsRelativeConfigDirRefused(t *testing.T) {
	isolateConfig(t)
	t.Setenv(configDirEnv, "relative/dir")
	wd := t.TempDir()
	t.Chdir(wd)

	ops := map[string]func() error{
		"load":   func() error { _, err := LoadToken("http://a:1"); return err },
		"save":   func() error { return SaveToken("http://a:1", "tok") },
		"delete": func() error { return DeleteToken("http://a:1") },
	}
	for name, op := range ops {
		err := op()
		if err == nil || !strings.Contains(err.Error(), configDirEnv) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%s: err = %v, want one line naming %s", name, err, configDirEnv)
		}
	}
	if entries, _ := os.ReadDir(wd); len(entries) != 0 {
		t.Errorf("a refused save wrote %d entries into the working directory", len(entries))
	}
}

// TestLoginTwoConfigDirs: two logins with two CONCH_CONFIG_DIR values never read
// or write each other's file, and the directory is used as given.
func TestLoginTwoConfigDirs(t *testing.T) {
	isolateConfig(t)
	home := t.TempDir()
	dirs := map[string]string{"alice": filepath.Join(home, "alice"), "bob": filepath.Join(home, "bob")}
	servers := map[string]*authFake{"alice": newAuthFake(t, true), "bob": newAuthFake(t, true)}

	for who, dir := range dirs {
		t.Setenv(configDirEnv, dir)
		if _, _, err := runCLI(t, fakeToken+"\n", "login", "--server", servers[who].URL); err != nil {
			t.Fatalf("login %s: %v", who, err)
		}
	}
	for who, dir := range dirs {
		t.Setenv(configDirEnv, dir)
		other := "bob"
		if who == "bob" {
			other = "alice"
		}
		if tok, err := LoadToken(servers[who].URL); err != nil || tok != fakeToken {
			t.Errorf("%s: LoadToken(own server) = %q, %v", who, tok, err)
		}
		if tok, err := LoadToken(servers[other].URL); err != nil || tok != "" {
			t.Errorf("%s: LoadToken(%s's server) = %q, %v; want nothing", who, other, tok, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, credentialsFileName)) //nolint:gosec // test-local path
		if err != nil {
			t.Fatalf("%s: credentials file is not directly under CONCH_CONFIG_DIR: %v", who, err)
		}
		var stored map[string]storedCredential
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		key, err := NormalizeServer(servers[who].URL)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stored[key]; !ok || len(stored) != 1 {
			t.Errorf("%s: file holds %v, want only its own entry %q", who, stored, key)
		}
		if _, err := os.Stat(filepath.Join(dir, "conch")); !os.IsNotExist(err) {
			t.Errorf("%s: a conch/ directory was appended under CONCH_CONFIG_DIR (%v)", who, err)
		}
	}
}
