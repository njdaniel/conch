//go:build !unix

package voicehelper

import (
	"io/fs"
	"os"
	"syscall"
)

// The managed helper is Linux only (design note §1). Elsewhere nothing is
// trusted, so nothing is started.

func sysProcAttr() *syscall.SysProcAttr { return nil }

func ownedByUserOrRoot(fs.FileInfo) bool { return false }

func endedBySignal(*os.ProcessState) (os.Signal, bool) { return nil, false }
