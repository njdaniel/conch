//go:build !unix

package cli

import (
	"io/fs"
	"os"
)

// readFlags: no FIFOs or O_NOFOLLOW here; the regular-file check still applies.
const readFlags = os.O_RDONLY

// ownedByCaller: ownership is not a Unix uid here, so only the mode check applies.
func ownedByCaller(fs.FileInfo) bool { return true }
