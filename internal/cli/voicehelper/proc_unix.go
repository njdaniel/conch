//go:build unix

package voicehelper

import (
	"io/fs"
	"os"
	"syscall"
)

// sysProcAttr puts the helper in a process group of its own, so that the
// terminal's job-control signals (Ctrl-C, Ctrl-Z, a hang-up) do not reach it:
// the supervisor decides when it stops (design note §2).
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// ownedByUserOrRoot reports whether fi names a file owned by the user conch
// runs as, or by root.
func ownedByUserOrRoot(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && ownerTrusted(st.Uid, os.Geteuid())
}

// ownerTrusted is the rule of ownedByUserOrRoot, apart so a test can give it
// owners no test can create.
func ownerTrusted(uid uint32, euid int) bool {
	return uid == 0 || (euid >= 0 && uint64(uid) == uint64(euid))
}

// endedBySignal reports the signal that ended the process, if one did.
func endedBySignal(ps *os.ProcessState) (os.Signal, bool) {
	if ps == nil {
		return nil, false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return nil, false
	}
	return ws.Signal(), true
}
