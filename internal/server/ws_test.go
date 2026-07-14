package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

// wsTestServer runs the server's handler on a real socket and returns the
// base ws:// URL. WebSocket needs real TCP; httptest.NewRecorder cannot
// carry an upgraded connection.
func wsTestServer(t *testing.T, srv *Server) string {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

func wsDial(t *testing.T, ctx context.Context, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, url, nil) //nolint:bodyclose // closed via conn.CloseNow cleanup
	if err != nil {
		t.Fatalf("websocket.Dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func postMessage(t *testing.T, base, channel string, authorID int64, body string) {
	t.Helper()
	httpBase := "http" + strings.TrimPrefix(base, "ws")
	url := fmt.Sprintf("%s/v0/channels/%s/messages", httpBase, channel)
	resp, err := http.Post(url, "application/json", //nolint:gosec // test-local URL
		strings.NewReader(fmt.Sprintf(`{"author_id":%d,"body":%q}`, authorID, body)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s status = %d, want %d", url, resp.StatusCode, http.StatusCreated)
	}
}

func readWSMessage(t *testing.T, ctx context.Context, conn *websocket.Conn) schema.MessageV0 {
	t.Helper()
	var m schema.MessageV0
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := wsjson.Read(rctx, conn, &m); err != nil {
		t.Fatalf("read WS message: %v", err)
	}
	return m
}

// TestWSSubscribersReceivePostedMessages is the REST→hub→WS integration the
// issue requires: both subscribers on the posted channel receive the message;
// a subscriber on another channel does not (proven by it receiving its own
// channel's later message first).
func TestWSSubscribersReceivePostedMessages(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)
	channel, principal := createTestChannelAndPrincipal(t, srv)
	otherChannel, err := srv.store.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	base := wsTestServer(t, srv)

	sub1 := wsDial(t, ctx, base+"/v0/ws?channel=general")
	sub2 := wsDial(t, ctx, base+"/v0/ws?channel=general")
	other := wsDial(t, ctx, base+"/v0/ws?channel=ops")

	postMessage(t, base, "general", principal.ID, "hello subscribers")
	postMessage(t, base, "ops", principal.ID, "ops only")

	for i, conn := range []*websocket.Conn{sub1, sub2} {
		got := readWSMessage(t, ctx, conn)
		if got.Body != "hello subscribers" || got.ChannelID != channel.ID || got.AuthorID != principal.ID {
			t.Errorf("subscriber %d got %+v, want the general message", i+1, got)
		}
	}
	// The ops subscriber's first frame must be the ops message — if the
	// general broadcast had leaked across channels it would arrive first.
	got := readWSMessage(t, ctx, other)
	if got.Body != "ops only" || got.ChannelID != otherChannel.ID {
		t.Errorf("other-channel subscriber got %+v, want the ops message", got)
	}
}

func TestWSRejectsUnknownAndMissingChannel(t *testing.T) {
	srv := newTestServer(t)
	base := wsTestServer(t, srv)

	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "unknown channel", path: "/v0/ws?channel=missing", wantStatus: http.StatusNotFound},
		{name: "missing param", path: "/v0/ws", wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, resp, err := websocket.Dial(ctx, base+tt.path, nil) //nolint:bodyclose // Dial closes the body on handshake failure
			if err == nil {
				_ = conn.CloseNow()
				t.Fatal("Dial succeeded, want handshake rejection")
			}
			if resp == nil || resp.StatusCode != tt.wantStatus {
				t.Fatalf("handshake response = %+v, want status %d", resp, tt.wantStatus)
			}
		})
	}
}

// TestWSHandlerExitsOnClientDisconnect guards the no-goroutine-leak criterion:
// when the client goes away, the handler must drop its subscription and
// return, observable as the goroutine count settling back to baseline.
func TestWSHandlerExitsOnClientDisconnect(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)
	_, principal := createTestChannelAndPrincipal(t, srv)
	base := wsTestServer(t, srv)

	before := runtime.NumGoroutine()
	conn := wsDial(t, ctx, base+"/v0/ws?channel=general")
	postMessage(t, base, "general", principal.ID, "one")
	_ = readWSMessage(t, ctx, conn)

	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client close: %v", err)
	}
	assertNoGoroutineLeak(t, before)
}

func TestWSV1SubscriberReceivesPayload(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)
	_, principal := createTestChannelAndPrincipal(t, srv)
	base := wsTestServer(t, srv)
	conn := wsDial(t, ctx, base+"/v1/ws?channel=general")
	httpBase := "http" + strings.TrimPrefix(base, "ws")
	body := fmt.Sprintf(`{"author_id":%d,"body":"alert","payload":{"schema":"acme.alert.v1","data":{"level":2}}}`, principal.ID)
resp, err := http.Post(httpBase+"/v1/channels/general/messages", "application/json", strings.NewReader(body)) //nolint:gosec // test-local URL
if err != nil {
	t.Fatalf("POST v1: %v", err)
}
defer func() { _ = resp.Body.Close() }()
if resp.StatusCode != http.StatusCreated {
	t.Fatalf("POST v1 status = %d, want %d", resp.StatusCode, http.StatusCreated)
}
	var got schema.MessageV1
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := wsjson.Read(rctx, conn, &got); err != nil {
		t.Fatalf("read V1 frame: %v", err)
	}
	if got.Schema != schema.MessageSchemaV1 || got.Payload == nil || string(got.Payload.Data) != `{"level":2}` {
		t.Fatalf("V1 frame = %+v", got)
	}
}
