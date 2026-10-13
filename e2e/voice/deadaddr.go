package main

import (
	"fmt"
	"syscall"
)

// deadAddr reserves a loopback address that nothing listens on and nothing
// else can be given: a TCP socket bound to a port the system chose and never
// put into the listening state. A connection to it is refused, as for a port
// with no socket at all, and until release is called no other process can bind
// it.
//
// The degraded half of this check points conchd at such an address as its
// LiveKit, then runs e2e/dogfood, which opens a listener of its own on a port
// the system chooses. With an address that was merely free a moment ago
// (freeAddr), the system could hand that very port to dogfood's ntfy stand-in;
// conchd's voice poller then sent its ListRooms call there, the stand-in
// counted it as a notification, and the run failed for a reason that had
// nothing to do with what was being tested (issue #245).
func deadAddr() (addr string, release func(), err error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return "", nil, fmt.Errorf("reserve a dead address: socket: %w", err)
	}
	release = func() { _ = syscall.Close(fd) }
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		release()
		return "", nil, fmt.Errorf("reserve a dead address: bind: %w", err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		release()
		return "", nil, fmt.Errorf("reserve a dead address: getsockname: %w", err)
	}
	in4, ok := name.(*syscall.SockaddrInet4)
	if !ok || in4.Port == 0 {
		release()
		return "", nil, fmt.Errorf("reserve a dead address: unexpected socket name %#v", name)
	}
	return fmt.Sprintf("127.0.0.1:%d", in4.Port), release, nil
}
