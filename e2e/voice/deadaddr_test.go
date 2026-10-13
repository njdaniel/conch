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

// holdDead takes one particular address that has just been let go of, as a
// stopped LiveKit container's is, with connections to it still winding down
// (TIME_WAIT), which is when a plain bind is refused. Held, it refuses
// connections and is never handed to a listener that asks for any port;
// release is safe to call twice.
func TestHoldDeadTakesAnAddressJustLetGo(t *testing.T) {
	was, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := was.Addr().String()
	// A connection the server side closes first leaves the server's port in
	// TIME_WAIT once the listener has gone.
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		if c, err := was.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	<-accepted
	_, _ = client.Read(make([]byte, 1)) // see the close
	_ = client.Close()
	_ = was.Close()

	release, err := holdDead(addr)
	if err != nil {
		t.Fatalf("holdDead on an address just let go of: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		_ = conn.Close()
		t.Fatalf("a connection to the held address %s was accepted", addr)
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("dialing the held address: %v, want connection refused", err)
	}
	// What matters: a listener that asks the system for any port is never
	// given this one. (A listener naming this port outright is a different
	// matter and not what the hold is for; see holdDead.)
	for range 2000 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		got := l.Addr().String()
		_ = l.Close()
		if got == addr {
			t.Fatalf("the system chose the held address %s for another listener", addr)
		}
	}
	release()
	release() // a second call must not close something else
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("after release the address cannot be bound: %v", err)
	}
	_ = l.Close()

	for _, bad := range []string{"", "127.0.0.1", "127.0.0.1:0", "example.com:80", "10.0.0.1:7880"} {
		if release, err := holdDead(bad); err == nil {
			release()
			t.Errorf("holdDead(%q) succeeded, want a refusal", bad)
		}
	}
}

// While a listener still has the port, holdDead waits for it and then takes
// it: a stopped container's proxy lets go of its port a moment after the stop
// returns.
func TestHoldDeadWaitsForThePortToBeLetGo(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = l.Close()
	}()
	release, err := holdDead(addr)
	if err != nil {
		t.Fatalf("holdDead did not wait for the port: %v", err)
	}
	release()
}
