package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

// ConfigDir returns the configuration directory conch uses, by the rule of
// configDir: always absolute, or "" when there is none. The TUI hands it to
// the conch-voice helper it starts, so that both read the same login and
// voice.toml (docs/design/tui-voice.md §9).
func ConfigDir() (string, error) { return configDir() }

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
	token, _, err := LoadCredential(server)
	return token, err
}

// LoadCredential is LoadToken, and also says where the token came from:
// fromEnv is true when it is CONCH_TOKEN's, false when it is the stored login
// (or there is none). A voice helper the TUI starts must use the same source
// (docs/design/tui-voice.md §9).
func LoadCredential(server string) (token string, fromEnv bool, err error) {
	if err := checkSafeServer(server); err != nil {
		return "", false, err
	}
	if token := strings.TrimSpace(os.Getenv(tokenEnv)); token != "" {
		return token, true, nil
	}
	key, err := NormalizeServer(server)
	if err != nil {
		return "", false, err
	}
	creds, err := readCredentials()
	if err != nil {
		return "", false, err
	}
	return creds[key].Token, false, nil
}

// storedServers returns the servers that have a stored login, sorted. A key
// that is not a server as NormalizeServer writes it was not stored by conch
// and names no server it could reach, so it is left out. Any error reading
// the file gives none: this only picks a default, and the command that then
// loads its token reports the error itself.
func storedServers() []string {
	creds, err := readCredentials()
	if err != nil {
		return nil
	}
	servers := make([]string, 0, len(creds))
	for key := range creds {
		if normalized, err := NormalizeServer(key); err == nil && normalized == key && checkSafeServer(key) == nil {
			servers = append(servers, key)
		}
	}
	sort.Strings(servers)
	return servers
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
			return lineError(err, "cli: remove credentials: %v", err)
		}
		return nil
	}
	return writeCredentials(creds)
}

// lineError is an error whose text is made safe for one terminal line. The
// directory comes from CONCH_CONFIG_DIR, which may hold anything, and the
// errors of the os package repeat it. The path itself is not secret, so it is
// kept, with anything unprintable written as a visible escape.
func lineError(cause error, format string, args ...any) error {
	return &serverText{msg: safeLine(fmt.Sprintf(format, args...)), err: cause}
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
	// Not os.Open: that would wait forever on a FIFO named credentials.json,
	// before any check could run. A symlink at the leaf is refused outright:
	// conch never writes one (a rename replaces it), so one is not ours.
	f, err := os.OpenFile(path, readFlags, 0) //nolint:gosec // path is the fixed credentials location under the config dir
	if errors.Is(err, fs.ErrNotExist) {
		return creds, nil
	}
	if err != nil {
		return nil, lineError(err, "cli: read credentials %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, lineError(err, "cli: read credentials %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, lineError(nil, "cli: credentials file %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, lineError(nil, "cli: credentials file %s is accessible by other users (mode %04o); run 'chmod 600 %q'", path, info.Mode().Perm(), path)
	}
	if err := json.NewDecoder(f).Decode(&creds); err != nil {
		return nil, lineError(err, "cli: credentials file %s is not valid: %v", path, err)
	}
	if creds == nil {
		creds = make(map[string]storedCredential)
	}
	return creds, nil
}

// openCredentialsDir returns the credentials directory opened as a root, so the
// checks and the writes that follow are bound to the one directory that was
// checked, however the path is later changed underneath. dir is exactly what
// configDir returned (not cleaned: through a symlink, "link/.." is not the
// parent of the link), and it is used for nothing else.
//
// The default directory (.../conch) is conch's own: it is created 0700 and
// tightened to 0700 if it is not. A directory named by CONCH_CONFIG_DIR is the
// user's: a missing one is created 0700, an existing one is never changed. If
// others may read or enter it, or it is not the caller's, the login is refused,
// so that CONCH_CONFIG_DIR=$HOME does not quietly lock down a home directory
// or put a token where others can list it. The check is made on the opened
// directory, after it exists, so one created by someone else in the meantime
// is checked too.
func openCredentialsDir(dir string) (*os.Root, error) {
	custom := strings.TrimSpace(os.Getenv(configDirEnv)) != ""
	subject := "the credentials directory"
	if custom {
		subject = configDirEnv
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, lineError(err, "cli: create %s %s: %v", subject, dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if fi, statErr := os.Stat(dir); statErr == nil && !fi.IsDir() {
			return nil, lineError(nil, "cli: %s is not a directory", subject)
		}
		return nil, lineError(err, "cli: open %s %s: %v", subject, dir, err)
	}
	if !custom {
		if err := root.Chmod(".", 0o700); err != nil {
			_ = root.Close()
			return nil, lineError(err, "cli: secure %s %s: %v", subject, dir, err)
		}
	}
	fi, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, lineError(err, "cli: %s %s: %v", subject, dir, err)
	}
	switch {
	case !ownedByCaller(fi):
		_ = root.Close()
		return nil, lineError(nil, "cli: %s is not owned by you; choose another directory", subject)
	case fi.Mode().Perm()&0o077 != 0:
		_ = root.Close()
		return nil, lineError(nil, "cli: %s may be read by others (mode %#o); make it private with chmod 700, or choose another directory", subject, fi.Mode().Perm())
	}
	return root, nil
}

func writeCredentials(creds map[string]storedCredential) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if dir == "" {
		return errors.New("cli: no user configuration directory available to store credentials")
	}
	root, err := openCredentialsDir(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("cli: encode credentials: %w", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("cli: write credentials: %w", err)
	}
	tmpName := ".credentials-" + hex.EncodeToString(suffix[:])
	tmp, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return lineError(err, "cli: write credentials: %v", err)
	}
	cleanup := func() { _ = root.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return lineError(err, "cli: write credentials: %v", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return lineError(err, "cli: write credentials: %v", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return lineError(err, "cli: write credentials: %v", err)
	}
	if err := root.Rename(tmpName, credentialsFileName); err != nil {
		cleanup()
		return lineError(err, "cli: write credentials: %v", err)
	}
	return nil
}
