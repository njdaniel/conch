//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// inTerminalForeground reports whether this process is the foreground job of
// the terminal behind f. A full-screen program that is not gets stopped by
// the kernel when it sets the terminal's modes (SIGTTOU on TCSETS) or reads
// from it (SIGTTIN), and a stopped wrapper child is usually never continued —
// the silence of issue #214. Only the controlling terminal answers TIOCGPGRP,
// so anything else — a pipe, a file, another session's terminal — comes back
// true: a guess must never refuse a start.
func inTerminalForeground(f *os.File) bool {
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&pgrp))) // #nosec G103 -- the ioctl wants a pointer to an int
	if errno != 0 {
		return true
	}
	return int(pgrp) == syscall.Getpgrp()
}
