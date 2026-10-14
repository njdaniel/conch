//go:build unix

package cli

import (
	"io/fs"
	"os"
	"syscall"
)

// readFlags open the credentials file without blocking on a FIFO someone put
// there (a plain open would wait for a writer and hang every command) and
// without following a symlink at the leaf.
const readFlags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW

// ownedByCaller reports whether fi names a file the calling user owns. Where
// the owner cannot be read it says yes: the mode check still applies.
func ownedByCaller(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Geteuid()
}
