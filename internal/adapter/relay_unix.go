//go:build unix

package adapter

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// relaySupported is whether this system has the Unix sockets relay mode needs.
const relaySupported = true

// ownedByUs reports whether the file belongs to the user running the relay.
func ownedByUs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// acquireRelayLock takes an exclusive lock on the file at path, creating it
// mode 0600. The lock belongs to the open file, so the system drops it when
// the relay dies however it dies; a stale lock file therefore never keeps a
// relay from starting, which the stale socket beside it needs. The file is
// left in place when the lock is released: removing it would let a second
// relay lock a new file of the same name while the first still runs.
func acquireRelayLock(path string) (func(), error) {
	// Never through a link: in a directory others can write, a planted link
	// would make the relay create and lock a file of its choosing.
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("lock file %s is not a regular file", path)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- the operator configures the lock path
	if err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("lock file %s is not a regular file", path)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another instance holds lock file %s", path)
		}
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	return func() {
		if err := file.Close(); err != nil { // closing drops the lock
			slog.Warn("close lock file", "error", err)
		}
	}, nil
}

// privateUmask runs f with a umask that leaves a new file readable and
// writable by its owner alone, so a socket is born mode 0600. The relay starts
// no process, and f is the only thing making files, so the process-wide umask
// is safe to change for the call.
func privateUmask(f func()) {
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	f()
}
