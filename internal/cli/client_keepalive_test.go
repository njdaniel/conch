package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// The client's socket watchdog (issue #428): a connection that goes silent
// without a close ends the subscription with an error, which is what makes the
// TUI and `conch tail` reconnect; a live one is left alone.

// silentServer accepts any WebSocket. With read set it reads, so the library
// answers pings; without it the server never reads and so never answers one,
// which is what a peer behind a dead network looks like.
func silentServer(t *testing.T, read bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		if read {
			ctx = conn.CloseRead(ctx)
		}
		<-ctx.Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func keepAliveReaders() map[string]func(*Client, context.Context) error {
	return map[string]func(*Client, context.Context) error{
		"Tail": func(c *Client, ctx context.Context) error {
			return c.Tail(ctx, "general", func(schema.MessageV0) error { return nil })
		},
		"Subscribe": func(c *Client, ctx context.Context) error {
			return c.Subscribe(ctx, "general", func(schema.MessageV1) error { return nil })
		},
		"SubscribeV2": func(c *Client, ctx context.Context) error {
			return c.SubscribeV2(ctx, "general", func(schema.MessageV2) error { return nil })
		},
		"SubscribeEvents": func(c *Client, ctx context.Context) error {
			return c.SubscribeEvents(ctx, "general", 0, EventHandler{})
		},
		"SubscribeVoicePresence": func(c *Client, ctx context.Context) error {
			return c.SubscribeVoicePresence(ctx, "general", func(schema.VoicePresenceV1) error { return nil })
		},
	}
}

func TestSubscriptionsNoticeASilentServer(t *testing.T) {
	for name, read := range keepAliveReaders() {
		t.Run(name, func(t *testing.T) {
			srv := silentServer(t, false)
			client, err := NewClient(srv.URL, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			client.pingInterval = 20 * time.Millisecond
			client.pingTimeout = 50 * time.Millisecond

			// The deadline is the failure: the subscription must end by itself.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = read(client, ctx)
			if err == nil {
				t.Fatal("subscription ended without an error")
			}
			if ctx.Err() != nil {
				t.Fatalf("subscription was still open at the deadline: %v", err)
			}
		})
	}
}

func TestSubscriptionsKeepAnAnsweringServer(t *testing.T) {
	for name, read := range keepAliveReaders() {
		t.Run(name, func(t *testing.T) {
			srv := silentServer(t, true)
			client, err := NewClient(srv.URL, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			client.pingInterval = 10 * time.Millisecond
			client.pingTimeout = 200 * time.Millisecond

			// Many intervals pass; the subscription must still be open when
			// the context ends, so the end is the context's, not the watchdog's.
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			err = read(client, ctx)
			if ctx.Err() == nil {
				t.Fatalf("subscription ended early: %v", err)
			}
		})
	}
}
