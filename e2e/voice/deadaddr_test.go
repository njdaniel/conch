package main

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

// A dead address refuses connections and cannot be bound by anyone else while
// it is held; that second property is what an address that was only free a
// moment ago does not have, and what issue #245 needed.
func TestDeadAddrRefusesAndCannotBeTaken(t *testing.T) {
	addr, release, err := deadAddr()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("a connection to the dead address %s was accepted", addr)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("dialing the dead address: %v, want connection refused", err)
	}

	// Nobody else can be given the port while it is held: not by asking for
	// it, and so not by asking the system to choose, either.
	if l, err := net.Listen("tcp", addr); err == nil {
		_ = l.Close()
		t.Fatalf("another listener was given the dead address %s", addr)
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("listening on the dead address: %v, want address in use", err)
	}
	for range 200 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		got := l.Addr().String()
		_ = l.Close()
		if got == addr {
			t.Fatalf("the system chose the dead address %s for another listener", addr)
		}
	}

	// Once released it is an ordinary free port again.
	release()
	released = true
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("after release the address cannot be bound: %v", err)
	}
	_ = l.Close()
}

// The address freeAddr returns is not reserved: it can be bound at once, by
// anyone. That is right for a server this check is about to start there, and
// wrong for an address that must stay dead.
func TestFreeAddrIsNotReserved(t *testing.T) {
	addr, err := freeAddr()
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the free address %s could not be bound: %v", addr, err)
	}
	_ = l.Close()
}
