package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// frameServer sends one text frame of the given size on any WebSocket, then
// closes normally. The frame is a JSON object whose "body" is padded to size.
func frameServer(t *testing.T, size int) *httptest.Server {
	t.Helper()
	const head, tail = `{"body":"`, `"}`
	frame := head + strings.Repeat("x", size-len(head)-len(tail)) + tail
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			return // the client refused the frame and went away
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Every socket the client reads takes a frame as large as a server can send,
// and still refuses one that is larger: the limit is raised, not removed.
func TestSubscriptionsReadFramesUpToTheLimitAndNoFurther(t *testing.T) {
	readers := map[string]func(*Client, context.Context, *int) error{
		"Tail": func(c *Client, ctx context.Context, got *int) error {
			return c.Tail(ctx, "general", func(m schema.MessageV0) error { *got = len(m.Body); return nil })
		},
		"Subscribe": func(c *Client, ctx context.Context, got *int) error {
			return c.Subscribe(ctx, "general", func(m schema.MessageV1) error { *got = len(m.Body); return nil })
		},
		"SubscribeV2": func(c *Client, ctx context.Context, got *int) error {
			return c.SubscribeV2(ctx, "general", func(m schema.MessageV2) error { *got = len(m.Body); return nil })
		},
	}
	for name, read := range readers {
		t.Run(name+" reads a frame of the limit", func(t *testing.T) {
			srv := frameServer(t, schema.MaxMessageFrameBytes)
			client, err := NewClient(srv.URL, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			err = read(client, context.Background(), &got)
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("subscription ended with %v, want the server's normal close after the frame", err)
			}
			if got < schema.MaxMessageFrameBytes-64 {
				t.Errorf("received a body of %d bytes, want the whole frame", got)
			}
		})
		t.Run(name+" refuses a frame over the limit", func(t *testing.T) {
			srv := frameServer(t, schema.MaxMessageFrameBytes+1)
			client, err := NewClient(srv.URL, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			err = read(client, context.Background(), &got)
			if err == nil || !strings.Contains(err.Error(), "read limited") {
				t.Fatalf("subscription ended with %v, want the read limit's error", err)
			}
			if got != 0 {
				t.Errorf("a frame over the limit was delivered (%d bytes)", got)
			}
		})
	}
}

// The limit is set where the socket is opened, in one place. A second way of
// opening one would be a socket left with the library's 32 KiB default, which
// is how issue #231 happened; this trips if another call site appears.
func TestEveryWebSocketIsOpenedByDial(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file) // #nosec G304 -- this package's own source files
		if err != nil {
			t.Fatal(err)
		}
		calls += strings.Count(string(src), "websocket.Dial(")
	}
	if calls != 1 {
		t.Errorf("websocket.Dial is called in %d places in this package, want 1: Client.dial", calls)
	}
}
