//go:build unix

package push

import (
	"errors"
	"os"
	"syscall"
)

// keyOpenFlags are added to the read-only open of the key file: O_NOFOLLOW so
// a symbolic link is refused rather than followed to a file the operator did
// not mean, and O_NONBLOCK so a FIFO planted at that name cannot hang the
// start (the regular-file check then refuses it).
const keyOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// keyOwnedByUs reports whether the open file belongs to this process's user.
// A key file someone else owns is one they can replace or read, whatever its
// mode says.
func keyOwnedByUs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Geteuid()
}

// isSymlinkRefusal reports whether err is O_NOFOLLOW refusing a symbolic link.
func isSymlinkRefusal(err error) bool { return errors.Is(err, syscall.ELOOP) }
