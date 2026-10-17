package voicehelper

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// HelperEnv names the variable that points conch at a helper: an
	// absolute path, used exactly.
	HelperEnv = "CONCH_VOICE_HELPER"
	// HelperName is the helper's executable name, looked for beside conch
	// and in PATH.
	HelperName = "conch-voice"
)

// finder finds the helper (design note §10). Its functions are fields so that
// a test can stand in for the running executable.
type finder struct {
	executable func() (string, error)
	lookPath   func(string) (string, error)
}

var defaultFinder = finder{executable: os.Executable, lookPath: exec.LookPath}

// find returns the helper to run, its links resolved, from configured (the
// value of CONCH_VOICE_HELPER, or "") or by searching. It takes the first
// candidate that exists and stops there: one that fails the checks is
// helper_untrusted, and the next is not tried, so that a file planted beside
// conch cannot quietly hand over to another.
func (f finder) find(configured string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		if !filepath.IsAbs(configured) {
			return "", newError(ReasonHelperUntrusted, HelperEnv+" must be an absolute path; it is never resolved against the working directory", nil)
		}
		if _, err := os.Lstat(configured); errors.Is(err, fs.ErrNotExist) {
			return "", newError(ReasonHelperNotFound, "the file "+HelperEnv+" names does not exist: "+quote(configured), nil)
		}
		return trusted(configured)
	}
	if exe, err := f.executable(); err == nil && filepath.IsAbs(exe) {
		beside := filepath.Join(filepath.Dir(exe), HelperName)
		if _, err := os.Lstat(beside); !errors.Is(err, fs.ErrNotExist) {
			return trusted(beside)
		}
	}
	// exec.ErrDot (a match through an empty or "." entry of PATH) is not
	// found, and so is anything else that is not absolute.
	if path, err := f.lookPath(HelperName); err == nil && filepath.IsAbs(path) {
		return trusted(path)
	}
	return "", newError(ReasonHelperNotFound, HelperName+" was not found: install it beside conch or in PATH, or set "+HelperEnv+
		" to its absolute path; conch and conch-voice must come from the same release", nil)
}

// maxLinks bounds the links followed while resolving a candidate, as the
// kernel's own limit does.
const maxLinks = 40

// trusted resolves path and checks that what it names may be run, and that no
// other user could have chosen or could change what that is:
//
//   - every directory the path passes through, from / down, including each
//     directory a link sits in and each a link leads to, is owned by the user
//     or root and not writable by group or others (as OpenSSH's safe_path);
//   - the file itself is a regular, executable file, owned by the user or
//     root and not writable by group or others.
//
// The path is walked one component at a time, following each link with
// os.Readlink, so a link planted in a directory someone else can write is
// refused however safe its target looks. It returns the real path, with no
// link left in it, which is what is run.
func trusted(path string) (string, error) {
	refuse := func(why string) (string, error) {
		return "", newError(ReasonHelperUntrusted, quote(path)+" "+why+"; it was not run", nil)
	}
	root, err := os.Lstat("/")
	if err != nil || !safeDir(root) {
		return refuse("is under a root directory other users can write to")
	}
	// cur is always a real path, with no link in it: every name in it has
	// been looked at, and every directory in it found safe.
	rest := components(path)
	cur := "/"
	links := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			return refuse("cannot be resolved")
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			links++
			if links > maxLinks {
				return refuse("goes through too many links")
			}
			target, err := os.Readlink(next)
			if err != nil {
				return refuse("cannot be resolved")
			}
			if filepath.IsAbs(target) {
				cur = "/"
			}
			// The link sits in cur, already found safe; what it leads to
			// is walked like the rest.
			rest = append(components(target), rest...)
			continue
		}
		if len(rest) > 0 && !safeDir(fi) {
			// Something follows, so this is a directory to go through.
			return refuse("is reached through " + quote(next) + ", which is not a directory only you or root can change")
		}
		cur = next
	}
	fi, err := os.Lstat(cur)
	if err != nil {
		return refuse("cannot be read")
	}
	switch {
	case !fi.Mode().IsRegular():
		return refuse("is not a regular file")
	case fi.Mode().Perm()&0o111 == 0:
		return refuse("is not executable")
	case !ownedByUserOrRoot(fi):
		return refuse("is not owned by you or by root")
	case fi.Mode().Perm()&0o022 != 0:
		return refuse("may be changed by other users (group or others can write it)")
	}
	return cur, nil
}

// safeDir reports whether fi is a directory only the user or root can change.
func safeDir(fi os.FileInfo) bool {
	return fi.IsDir() && ownedByUserOrRoot(fi) && fi.Mode().Perm()&0o022 == 0
}

// components splits a path into its names; an absolute path's leading slash
// gives an empty first name, which the walk skips.
func components(path string) []string {
	return strings.Split(path, string(filepath.Separator))
}
