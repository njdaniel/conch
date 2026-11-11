//go:build !unix

package push

import "os"

// Platforms without O_NOFOLLOW and Unix ownership: the key file is checked as
// a regular file and for its mode alone.
const keyOpenFlags = 0

func keyOwnedByUs(os.FileInfo) bool { return true }

func isSymlinkRefusal(error) bool { return false }
