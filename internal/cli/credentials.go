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
	"time"
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

// profilePath returns the location of the file name in the configuration
// directory (credentials.json, guest-visits.json), or "" when there is no
// configuration directory. An error means CONCH_CONFIG_DIR is unusable.
func profilePath(name string) (string, error) {
	dir, err := configDir()
	if err != nil || dir == "" {
		return "", err
	}
	// Not filepath.Join: Join cleans "..", which conch-voice's Rust path join
	// does not, and through a symlink the two would then name different files.
	return strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator) + name, nil
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
// non-empty, otherwise the stored credential, otherwise a live guest visit's.
// An empty token with a nil error means no credential is available.
func LoadToken(server string) (string, error) {
	token, _, err := LoadCredential(server)
	return token, err
}

// credentialSource is where a command's token came from.
type credentialSource int

const (
	// sourceNone: there is no token; the command goes without one.
	sourceNone credentialSource = iota
	// sourceEnv: CONCH_TOKEN.
	sourceEnv
	// sourceStored: the member login in credentials.json.
	sourceStored
	// sourceGuest: a temporary guest visit, in its own file (guestVisitsFileName).
	sourceGuest
)

// LoadCredential is LoadToken, and also says whether the token has to be
// handed to a helper: fromEnv is true when it is not in credentials.json,
// which conch-voice reads itself. That is CONCH_TOKEN's, and a temporary guest
// visit's, which is never written to credentials.json. A voice helper the TUI
// starts must use the same source (docs/design/tui-voice.md §9).
func LoadCredential(server string) (token string, fromEnv bool, err error) {
	token, source, err := loadCredential(server)
	return token, source == sourceEnv || source == sourceGuest, err
}

// loadCredential finds server's token and says where it came from:
// CONCH_TOKEN, then the stored member login, then a live guest visit. A
// guest visit past its deadline is never returned, and is removed.
func loadCredential(server string) (string, credentialSource, error) {
	if err := checkSafeServer(server); err != nil {
		return "", sourceNone, err
	}
	if token := strings.TrimSpace(os.Getenv(tokenEnv)); token != "" {
		return token, sourceEnv, nil
	}
	key, err := NormalizeServer(server)
	if err != nil {
		return "", sourceNone, err
	}
	creds, err := readCredentials()
	if err != nil {
		return "", sourceNone, err
	}
	if token := creds[key].Token; token != "" {
		return token, sourceStored, nil
	}
	visits, err := liveGuestVisits()
	if err != nil {
		return "", sourceNone, err
	}
	if v, ok := visits[key]; ok {
		return v.Token, sourceGuest, nil
	}
	return "", sourceNone, nil
}

// storedServers returns the servers that have a stored login or a live guest
// visit, sorted. A key that is not a server as NormalizeServer writes it was
// not stored by conch and names no server it could reach, so it is left out.
// Any error reading the files gives none: this only picks a default, and the
// command that then loads its token reports the error itself.
func storedServers() []string {
	creds, err := readCredentials()
	if err != nil {
		return nil
	}
	keys := make(map[string]bool, len(creds))
	for key := range creds {
		keys[key] = true
	}
	if visits, err := readGuestVisits(); err == nil {
		now := guestNow()
		for key, v := range visits {
			if v.live(now) {
				keys[key] = true
			}
		}
	}
	servers := make([]string, 0, len(keys))
	for key := range keys {
		if normalized, err := NormalizeServer(key); err == nil && normalized == key && checkSafeServer(key) == nil {
			servers = append(servers, key)
		}
	}
	sort.Strings(servers)
	return servers
}

// SaveToken stores token for server, creating the directory (0700) and file
// (0600) as needed. The file is replaced atomically. It refuses while a live
// guest visit is stored for server: signing in as someone else does not end
// the visit, so the person ends it, or keeps it by creating an account, first.
func SaveToken(server, token string) error {
	if err := checkSafeServer(server); err != nil {
		return err
	}
	key, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	if err := refuseMemberOverGuest(key); err != nil {
		return err
	}
	return saveMemberToken(key, token)
}

// refuseMemberOverGuest says why a member login may not be stored for key, a
// normalized server, or nil when it may: a live guest visit is stored there.
func refuseMemberOverGuest(key string) error {
	visits, err := liveGuestVisits()
	if err != nil {
		return err
	}
	if _, ok := visits[key]; ok {
		return fmt.Errorf("cli: a temporary guest visit is stored for %s: end it first with 'conch end-visit', or keep its access by creating an account with 'conch account create <name>'", safeLine(key))
	}
	return nil
}

// saveMemberToken writes token as the member login of key, a normalized
// server.
func saveMemberToken(key, token string) error {
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
		return removeProfileFile(credentialsFileName, "credentials")
	}
	return writeCredentials(creds)
}

// A temporary guest visit (ADR-009 D2, design §3.4) is not a login. Its one
// credential lives in a file of its own, guest-visits.json beside
// credentials.json and held to the same rules (a private directory, a 0600
// file replaced atomically), and never in credentials.json, which conch-voice
// and every later sign-in read as the person's lasting login. Each entry
// carries the visit's hard deadline: an entry past it is dropped whenever the
// file is read, and the file goes when it is empty. The server still decides:
// a visit that ended early (End visit, idle, an operator) answers 401, and
// the entry is removed then (Client.guestServer).

const (
	guestVisitsFileName = "guest-visits.json"
	guestVisitsWhat     = "guest visits"
)

// storedGuestVisit is one server's guest visit as this machine keeps it.
type storedGuestVisit struct {
	Token  string `json:"token"`
	Handle string `json:"handle"`
	// ExpiresAt is the visit's absolute deadline, from the server's answer.
	ExpiresAt time.Time `json:"expires_at"`
}

// live reports whether the visit may still be in use at now. One with no
// deadline is not: every visit the server makes has one.
func (v storedGuestVisit) live(now time.Time) bool {
	return v.Token != "" && !v.ExpiresAt.IsZero() && now.Before(v.ExpiresAt)
}

// guestNow is the clock stored guest visits are held to; a variable so tests
// can move it.
var guestNow = time.Now

// readGuestVisits returns every stored guest visit, live or not.
func readGuestVisits() (map[string]storedGuestVisit, error) {
	visits := make(map[string]storedGuestVisit)
	if err := readProfileFile(guestVisitsFileName, guestVisitsWhat, &visits); err != nil {
		return nil, err
	}
	if visits == nil {
		visits = make(map[string]storedGuestVisit)
	}
	return visits, nil
}

// liveGuestVisits returns the stored guest visits that have not passed their
// deadline, and removes the others from the file. A failure to rewrite the
// file is not an error: the expired entries are left out either way, and the
// next read tries again.
func liveGuestVisits() (map[string]storedGuestVisit, error) {
	visits, err := readGuestVisits()
	if err != nil {
		return nil, err
	}
	now := guestNow()
	expired := false
	for key, v := range visits {
		if !v.live(now) {
			delete(visits, key)
			expired = true
		}
	}
	if expired {
		_ = writeGuestVisits(visits)
	}
	return visits, nil
}

// writeGuestVisits replaces the guest visits file, or removes it when there
// are none.
func writeGuestVisits(visits map[string]storedGuestVisit) error {
	if len(visits) == 0 {
		return removeProfileFile(guestVisitsFileName, guestVisitsWhat)
	}
	return writeProfileFile(guestVisitsFileName, guestVisitsWhat, visits)
}

// saveGuestVisit stores a new guest visit for server. It refuses while a
// member login is stored for the server, so that the visit can never replace
// it or be mistaken for it, and while another live visit is stored there.
func saveGuestVisit(server string, v storedGuestVisit) error {
	key, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	if err := refuseGuestOver(key); err != nil {
		return err
	}
	visits, err := liveGuestVisits()
	if err != nil {
		return err
	}
	visits[key] = v
	return writeGuestVisits(visits)
}

// refuseGuestOver says why a guest visit may not be stored for key, a
// normalized server, or nil when it may: a member login or a live visit is
// stored there already.
func refuseGuestOver(key string) error {
	creds, err := readCredentials()
	if err != nil {
		return err
	}
	if creds[key].Token != "" {
		return fmt.Errorf("cli: you are signed in to %s with a lasting login, which a guest visit would not replace: run 'conch logout' first, or use another CONCH_CONFIG_DIR", safeLine(key))
	}
	visits, err := liveGuestVisits()
	if err != nil {
		return err
	}
	if v, ok := visits[key]; ok {
		return fmt.Errorf("cli: a guest visit (%s) is already stored for %s; end it with 'conch end-visit' first", safeLine(v.Handle), safeLine(key))
	}
	return nil
}

// deleteGuestVisit removes server's guest visit, if one is stored.
func deleteGuestVisit(server string) error {
	key, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	visits, err := readGuestVisits()
	if err != nil {
		return err
	}
	if _, ok := visits[key]; !ok {
		return nil
	}
	delete(visits, key)
	return writeGuestVisits(visits)
}

// replaceGuestWithMember stores token, the member credential a conversion
// answered, as server's login, and only then removes the guest visit: a
// failure between the two leaves a dead guest entry, which the next 401
// clears, never a lost account.
func replaceGuestWithMember(server, token string) error {
	key, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	if err := saveMemberToken(key, token); err != nil {
		return err
	}
	return deleteGuestVisit(key)
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
	if err := readProfileFile(credentialsFileName, "credentials", &creds); err != nil {
		return nil, err
	}
	if creds == nil {
		creds = make(map[string]storedCredential)
	}
	return creds, nil
}

// readProfileFile decodes the file name in the configuration directory into
// v, which is left as it is when there is no directory or no file. what
// names the file in errors.
func readProfileFile(name, what string, v any) error {
	path, err := profilePath(name)
	if err != nil || path == "" {
		return err
	}
	// Not os.Open: that would wait forever on a FIFO named like the file,
	// before any check could run. A symlink at the leaf is refused outright:
	// conch never writes one (a rename replaces it), so one is not ours.
	f, err := os.OpenFile(path, readFlags, 0) //nolint:gosec // path is a fixed file name under the config dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return lineError(err, "cli: read %s %s: %v", what, path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return lineError(err, "cli: read %s %s: %v", what, path, err)
	}
	if !info.Mode().IsRegular() {
		return lineError(nil, "cli: %s file %s is not a regular file", what, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return lineError(nil, "cli: %s file %s is accessible by other users (mode %04o); run 'chmod 600 %q'", what, path, info.Mode().Perm(), path)
	}
	if err := json.NewDecoder(f).Decode(v); err != nil {
		return lineError(err, "cli: %s file %s is not valid: %v", what, path, err)
	}
	return nil
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
	return writeProfileFile(credentialsFileName, "credentials", creds)
}

// writeProfileFile replaces the file name in the configuration directory
// with v, atomically, creating the directory (0700) and the file (0600) as
// needed. what names the file in errors.
func writeProfileFile(name, what string, v any) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if dir == "" {
		return fmt.Errorf("cli: no user configuration directory available to store %s", what)
	}
	root, err := openCredentialsDir(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("cli: encode %s: %w", what, err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("cli: write %s: %w", what, err)
	}
	tmpName := ".credentials-" + hex.EncodeToString(suffix[:])
	tmp, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return lineError(err, "cli: write %s: %v", what, err)
	}
	cleanup := func() { _ = root.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return lineError(err, "cli: write %s: %v", what, err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return lineError(err, "cli: write %s: %v", what, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return lineError(err, "cli: write %s: %v", what, err)
	}
	if err := root.Rename(tmpName, name); err != nil {
		cleanup()
		return lineError(err, "cli: write %s: %v", what, err)
	}
	return nil
}

// removeProfileFile deletes the file name in the configuration directory;
// one that is not there is not an error.
func removeProfileFile(name, what string) error {
	path, err := profilePath(name)
	if err != nil || path == "" {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return lineError(err, "cli: remove %s: %v", what, err)
	}
	return nil
}
