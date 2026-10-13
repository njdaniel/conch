package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// deadAddr reserves a loopback address that nothing listens on and nothing
// else can be given: a TCP socket bound to a port the system chose and never
// put into the listening state. A connection to it is refused, as for a port
// with no socket at all, and until release is called no other process can bind
// it.
//
// This check points conchd at such an address as its LiveKit and then runs
// e2e/dogfood, which opens a listener of its own on a port the system chooses.
// With an address that was merely free a moment ago (freeAddr), the system
// could hand that very port to dogfood's ntfy stand-in; conchd's voice poller
// then sent its ListRooms call there, the stand-in counted it as a
// notification, and the run failed for a reason that had nothing to do with
// what was being tested (issue #245).
func deadAddr() (addr string, release func(), err error) {
	port, release, err := bindDead(0, false)
	if err != nil {
		return "", nil, err
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), release, nil
}

// holdDead does the same for one particular loopback address that has just
// stopped being listened on (a LiveKit container that was stopped): it keeps
// the system from giving the port to anyone who asks for "any port" while
// conchd is still configured to call it. Whatever was listening may take a
// moment to let go of the port, so the bind is retried briefly.
//
// Connections that were open to the old listener leave the port in TIME_WAIT
// for a minute, and a plain bind is refused for as long. So this socket sets
// SO_REUSEADDR, as every listener does. The price: another socket that also
// sets it and names this very port could still bind it. That is not the case
// being guarded against. A socket bound to port 0, which is how dogfood's
// stand-in and every test server get theirs, is never given a port another
// socket holds, reuse or not.
func holdDead(addr string) (release func(), err error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("hold a dead address: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || (host != "127.0.0.1" && host != "localhost") {
		return nil, fmt.Errorf("hold a dead address: %q is not a loopback address with a port", addr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, release, err = bindDead(port, true)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// bindDead binds a TCP socket to 127.0.0.1:port (0 lets the system choose)
// without listening on it, and returns the port and a release that may be
// called more than once. reuse sets SO_REUSEADDR before binding (see holdDead). The socket is marked close-on-exec: the processes
// this program starts must not inherit it, or the port would stay taken for as
// long as any of them lived, whatever release did.
func bindDead(port int, reuse bool) (bound int, release func(), err error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return 0, nil, fmt.Errorf("reserve a dead address: socket: %w", err)
	}
	syscall.CloseOnExec(fd)
	var once sync.Once
	release = func() { once.Do(func() { _ = syscall.Close(fd) }) }
	if reuse {
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			release()
			return 0, nil, fmt.Errorf("reserve a dead address: SO_REUSEADDR: %w", err)
		}
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		release()
		return 0, nil, fmt.Errorf("reserve a dead address: bind: %w", err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		release()
		return 0, nil, fmt.Errorf("reserve a dead address: getsockname: %w", err)
	}
	in4, ok := name.(*syscall.SockaddrInet4)
	if !ok || in4.Port == 0 {
		release()
		return 0, nil, fmt.Errorf("reserve a dead address: unexpected socket name %#v", name)
	}
	return in4.Port, release, nil
}
