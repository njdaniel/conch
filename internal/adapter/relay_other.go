//go:build !unix

package adapter

import (
	"errors"
	"os"
)

// relaySupported is false here: relay mode refuses to start.
const relaySupported = false

func ownedByUs(os.FileInfo) bool { return false }

func acquireRelayLock(string) (func(), error) {
	return nil, errors.New("relay mode needs Unix domain sockets, which this system does not have")
}

func privateUmask(f func()) { f() }
