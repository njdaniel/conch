package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	credentialsDirName  = "conch"
	credentialsFileName = "credentials.json"
	// tokenEnv overrides the credentials file; it is how scripts and CI log in.
	tokenEnv = "CONCH_TOKEN"
	// configDirEnv moves the directory credentials.json (and conch-voice's
	// voice.toml) live in, so two logins can share one machine. conch-voice reads
	// the same variable by the same rule (docs/design/conch-voice.md §8).
	configDirEnv = "CONCH_CONFIG_DIR"
)

type storedCredential struct {
	Token string `json:"token"`
}

// NormalizeServer reduces a server URL to the key credentials are stored under:
// lower-case scheme and host, no path, no trailing slash.
func NormalizeServer(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("cli: parse server URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("cli: server must be an http(s) URL")
	}
	return normalizeURL(u), nil
}

// normalizeURL keeps the path (case preserved, trailing slashes trimmed): two
// servers behind one host at different prefixes are different servers.
func normalizeURL(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
}

// userConfigDir is injectable so tests can never reach the real directory.
var userConfigDir = os.UserConfigDir

// configDir returns the directory that holds the credentials file, or "" when
// there is none (no CONCH_CONFIG_DIR and the platform has no user configuration
// directory). CONCH_CONFIG_DIR, when it holds more than white space, is used as
// given, without "conch" appended, and must be absolute: resolving a relative
// one against the working directory would put a login wherever the shell
// happened to be. Otherwise it is the user configuration directory plus "conch".
// conch-voice resolves it by the same rule; testdata/config-dir-vectors.json
// keeps the two from drifting.
func configDir() (string, error) {
	if dir := os.Getenv(configDirEnv); strings.TrimSpace(dir) != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("cli: %s must be an absolute path", configDirEnv)
		}
		return dir, nil
	}
	// No user configuration directory is not an error: there is no stored login.
	if base, err := userConfigDir(); err == nil && base != "" {
		return filepath.Join(base, credentialsDirName), nil
	}
	return "", nil
}

// credentialsPath returns the credentials file location, or "" when there is no
// configuration directory. An error means CONCH_CONFIG_DIR is unusable.
func credentialsPath() (string, error) {
	dir, err := configDir()
	if err != nil || dir == "" {
		return "", err
	}
	// Not filepath.Join: Join cleans "..", which conch-voice's Rust path join
	// does not, and through a symlink the two would then name different files.
	return strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator) + credentialsFileName, nil
}

// errUnsafeServerHost is returned when a server host cannot be used safely.
// The error message never echoes the server address or credentials.
var errUnsafeServerHost = errors.New("cli: server host cannot be used safely: the name its login is stored under and the name it would be reached at are different hosts")

// checkSafeServer verifies that server's host does not have ambiguous unicode characters
// (such as U+0130 and U+1E9E) where the stored key and dialed name would disagree.
func checkSafeServer(server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("cli: parse server URL: %w", err)
	}
	if hostContainsRefusedRune(u.Host) {
		return errUnsafeServerHost
	}
	return nil
}

func hostContainsRefusedRune(host string) bool {
	if strings.ContainsRune(host, '\u0130') || strings.ContainsRune(host, '\u1e9e') {
		return true
	}
	if unescaped, err := url.PathUnescape(host); err == nil {
		if strings.ContainsRune(unescaped, '\u0130') || strings.ContainsRune(unescaped, '\u1e9e') {
			return true
		}
	}
	lower := strings.ToLower(host)
	return strings.Contains(lower, "%c4%b0") || strings.Contains(lower, "%e1%ba%9e")
}

// LoadToken returns the bearer token for server: CONCH_TOKEN when set and
// non-empty, otherwise the stored credential. An empty token with a nil error
// means no credential is available.
func LoadToken(server string) (string, error) {
	if err := checkSafeServer(server); err != nil {
		return "", err
	}
	if token := strings.TrimSpace(os.Getenv(tokenEnv)); token != "" {
		return token, nil
	}
	key, err := NormalizeServer(server)
	if err != nil {
		return "", err
	}
	creds, err := readCredentials()
	if err != nil {
		return "", err
	}
	return creds[key].Token, nil
}

// SaveToken stores token for server, creating the directory (0700) and file
// (0600) as needed. The file is replaced atomically.
func SaveToken(server, token string) error {
	if err := checkSafeServer(server); err != nil {
		return err
	}
	key, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	creds, err := readCredentials()
	if err != nil {
		return err
	}
	creds[key] = storedCredential{Token: token}
	return writeCredentials(creds)
}

// DeleteToken removes the stored credential for server, deleting the file when
// it becomes empty. Removing a credential that is not there is not an error.
func DeleteToken(server string) error {
	if err := checkSafeServer(server); err != nil {
		return err
	}
	key, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	creds, err := readCredentials()
	if err != nil {
		return err
	}
	if _, ok := creds[key]; !ok {
		return nil
	}
	delete(creds, key)
	if len(creds) == 0 {
		path, err := credentialsPath()
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cli: remove credentials: %w", err)
		}
		return nil
	}
	return writeCredentials(creds)
}

func readCredentials() (map[string]storedCredential, error) {
	creds := make(map[string]storedCredential)
	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}
	if path == "" {
		return creds, nil
	}
	f, err := os.Open(path) //nolint:gosec // path is the fixed credentials location under the user config dir
	if errors.Is(err, fs.ErrNotExist) {
		return creds, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cli: read credentials: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cli: read credentials: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("cli: credentials file %s is accessible by other users (mode %04o); run 'chmod 600 %s'", path, info.Mode().Perm(), path)
	}
	if err := json.NewDecoder(f).Decode(&creds); err != nil {
		return nil, fmt.Errorf("cli: credentials file %s is not valid: %w", path, err)
	}
	if creds == nil {
		creds = make(map[string]storedCredential)
	}
	return creds, nil
}

// prepareCredentialsDir makes sure dir exists and is private before a token is
// written into it. The default directory (.../conch) is conch's own, so it is
// created 0700 and tightened to 0700 if it is not. A directory named by
// CONCH_CONFIG_DIR is the user's: one that does not exist is created 0700, but
// one that exists is never changed. If others may read or enter it, the login
// is refused, so that CONCH_CONFIG_DIR=$HOME does not quietly lock down a home
// directory, or put a token where others can list it.
func prepareCredentialsDir(dir string) error {
	if strings.TrimSpace(os.Getenv(configDirEnv)) == "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cli: create credentials directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // 0700 is a directory mode: owner-only
			return fmt.Errorf("cli: secure credentials directory: %w", err)
		}
		return nil
	}
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cli: create %s: %w", configDirEnv, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("cli: %s: %w", configDirEnv, err)
	case !fi.IsDir():
		return fmt.Errorf("cli: %s is not a directory", configDirEnv)
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("cli: %s may be read by others (mode %#o); make it private with chmod 700, or choose another directory", configDirEnv, fi.Mode().Perm())
	}
	return nil
}

func writeCredentials(creds map[string]storedCredential) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if path == "" {
		return errors.New("cli: no user configuration directory available to store credentials")
	}
	dir := filepath.Dir(path)
	if err := prepareCredentialsDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("cli: encode credentials: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("cli: write credentials: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("cli: write credentials: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("cli: write credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("cli: write credentials: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("cli: write credentials: %w", err)
	}
	return nil
}
