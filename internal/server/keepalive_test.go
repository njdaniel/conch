package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The socket keepalive (issue #428). Intervals are shortened through the
// server's fields before it serves; nothing sleeps for a guessed time, each
// wait is a poll or a read with a deadline.

const keepAliveTestWait = 10 * time.Second

// TestKeepAliveClosesSilentPeer: a client that stops reading never answers a
// ping, because the library answers pings from its read loop. On every socket
// the server then closes the connection and releases its slot.
func TestKeepAliveClosesSilentPeer(t *testing.T) {
	for _, tt := range socketRoutes {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemberFixture(t)
			f.srv.pingInterval = 50 * time.Millisecond
			f.srv.pingTimeout = 100 * time.Millisecond
			base := wsTestServer(t, f.srv)
			credID, tok := f.newCred(t, f.alice.ID)

			// Dialled and never read.
			conn, _, err := capDial(base, tt.path, tok)
			if err != nil {
				t.Fatalf("dial %s: %v", tt.path, err)
			}
			t.Cleanup(func() { _ = conn.CloseNow() })
			waitSockets(t, f.srv, credID, 0)
		})
	}
}

// TestKeepAliveKeepsAnsweringPeer: a client that reads answers every ping, so
// the socket outlives many intervals.
func TestKeepAliveKeepsAnsweringPeer(t *testing.T) {
	for _, tt := range socketRoutes {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemberFixture(t)
			f.srv.pingInterval = 20 * time.Millisecond
			f.srv.pingTimeout = 200 * time.Millisecond
			base := wsTestServer(t, f.srv)
			credID, tok := f.newCred(t, f.alice.ID)

			capOpen(t, base, tt.path, tok)
			waitSockets(t, f.srv, credID, 1)
			time.Sleep(15 * f.srv.pingInterval) // many pings; the absence of a close is what is checked
			if n := f.srv.sockets.count(credID); n != 1 {
				t.Errorf("socket count = %d after many pings, want 1", n)
			}
		})
	}
}

// TestHeartbeatFrames: the events socket sends the heartbeat to everyone;
// /v2/ws only to a client that asked with heartbeat=1; the others never.
func TestHeartbeatFrames(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"events", "/v1/events/ws?channel=alpha", true},
		{"v2 asked", "/v2/ws?channel=alpha&heartbeat=1", true},
		{"v2 not asked", "/v2/ws?channel=alpha", false},
		{"v2 asked with another value", "/v2/ws?channel=alpha&heartbeat=0", false},
		{"v1", "/v1/ws?channel=alpha&heartbeat=1", false},
		{"v0", "/v0/ws?channel=alpha&heartbeat=1", false},
		{"voice", "/v1/voice/ws?channel=alpha&heartbeat=1", false},
		{"voice occupancy", "/v1/voice/occupancy/ws?heartbeat=1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemberFixture(t)
			f.srv.pingInterval = 20 * time.Millisecond
			f.srv.pingTimeout = 500 * time.Millisecond
			base := wsTestServer(t, f.srv)
			_, tok := f.newCred(t, f.alice.ID)
			conn, _, err := capDial(base, tt.path, tok)
			if err != nil {
				t.Fatalf("dial %s: %v", tt.path, err)
			}
			t.Cleanup(func() { _ = conn.CloseNow() })

			wait := keepAliveTestWait
			if !tt.want {
				wait = 300 * time.Millisecond // some 15 intervals
			}
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			for {
				_, raw, err := conn.Read(ctx)
				if err != nil {
					if tt.want {
						t.Fatalf("no heartbeat: %v", err)
					}
					// A read that outlives its context closes the socket,
					// and the error then may be the close's. The deadline
					// passing with no heartbeat is the result.
					if ctx.Err() == nil {
						t.Fatalf("read: %v", err)
					}
					return
				}
				var hb schema.HeartbeatV1
				if json.Unmarshal(raw, &hb) == nil && hb.Validate() == nil {
					if !tt.want {
						t.Fatalf("got a heartbeat the socket was not asked for: %s", raw)
					}
					return
				}
			}
		})
	}
}

// TestKeepAliveDefaults pins the contract the issue states: a ping every 25 s
// with a 10 s wait closes a dead peer inside 35 s.
func TestKeepAliveDefaults(t *testing.T) {
	if wsPingInterval != 25*time.Second || wsPingTimeout != 10*time.Second {
		t.Errorf("ping interval %v, timeout %v; want 25s and 10s", wsPingInterval, wsPingTimeout)
	}
	// The browser recognises a heartbeat only in a text frame of at most 64
	// bytes (web/src/state/feed.ts isHeartbeat); a larger one would reach
	// the page as a frame it cannot read, every 25 s.
	if n := len(heartbeatFrame); n > 64 {
		t.Errorf("heartbeat frame is %d bytes; the web client recognises at most 64", n)
	}
}
