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

// credentialsPath returns the credentials file location, or "" when the
// platform has no user configuration directory.
func credentialsPath() string {
	dir, err := userConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, credentialsDirName, credentialsFileName)
}

// LoadToken returns the bearer token for server: CONCH_TOKEN when set and
// non-empty, otherwise the stored credential. An empty token with a nil error
// means no credential is available.
func LoadToken(server string) (string, error) {
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
		path := credentialsPath()
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cli: remove credentials: %w", err)
		}
		return nil
	}
	return writeCredentials(creds)
}

func readCredentials() (map[string]storedCredential, error) {
	creds := make(map[string]storedCredential)
	path := credentialsPath()
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

func writeCredentials(creds map[string]storedCredential) error {
	path := credentialsPath()
	if path == "" {
		return errors.New("cli: no user configuration directory available to store credentials")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cli: create credentials directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // 0700 is a directory mode: owner-only
		return fmt.Errorf("cli: secure credentials directory: %w", err)
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
