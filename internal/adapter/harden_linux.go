//go:build linux

package adapter

import "syscall"

// hardenProcess makes the adapter not dumpable. The runtime it starts runs as
// the same user, and without this it can read the token straight out of
// /proc/<adapter pid>/environ, which the cleaned child environment does not
// cover. Not dumpable, the adapter's /proc files belong to root and same-user
// ptrace is refused; the runtime is unaffected, because exec makes each child
// dumpable again. ssh-agent does the same. A failure is ignored: the adapter
// still works, only without this protection.
func hardenProcess() {
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0)
}
