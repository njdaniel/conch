package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// Socket keepalive (issue #428, private alpha A4). Without it a peer whose
// network died without a FIN (Wi-Fi off, a laptop asleep) looks connected
// until a TCP or proxy timeout fires, which can take minutes, and its
// subscription and socket slot are held all that time. Every socket handler
// reads through keepAlive, so each one is pinged the same way.

const (
	// wsPingInterval is how often the server pings each socket. It is also
	// how often a socket that asked for heartbeats gets one, so a proxy
	// with a 60 s idle timeout never sees a quiet connection.
	wsPingInterval = schema.HeartbeatIntervalSeconds * time.Second
	// wsPingTimeout is how long a ping may wait for its pong. A dead peer is
	// therefore closed within wsPingInterval + wsPingTimeout, 35 s.
	wsPingTimeout = 10 * time.Second
)

// keepAlive starts the connection's reader, which discards what the client
// sends and cancels the returned context when the peer closes or errors, and
// pings the peer on an interval. A ping with no pong in time closes the
// connection, which ends the reader and so the handler's stream loop. With
// heartbeat set it also sends a schema.HeartbeatV1 text frame after each
// answered ping, for a client (a browser) that cannot see pings.
//
// The goroutine ends with the returned context. Ping needs the reader
// CloseRead runs, because the pong is read there.
func (s *Server) keepAlive(ctx context.Context, conn *websocket.Conn, heartbeat bool) context.Context {
	ctx = conn.CloseRead(ctx)
	interval, timeout := s.pingInterval, s.pingTimeout
	if interval <= 0 {
		interval = wsPingInterval
	}
	if timeout <= 0 {
		timeout = wsPingTimeout
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, cancel := context.WithTimeout(ctx, timeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					// Said before the close, which ends the reader and so
					// the handler's loop: the connection log line then
					// names the timeout, not the client.
					noteClose(ctx, schema.SocketCloseKeepaliveTimeout)
					slog.DebugContext(ctx, "ws: ping failed, closing", "error", err)
				}
				// CloseNow, not Close: the close handshake would wait on
				// the peer that just failed to answer.
				_ = conn.CloseNow()
				return
			}
			if !heartbeat {
				continue
			}
			if err := writeHeartbeat(ctx, conn); err != nil {
				if ctx.Err() == nil {
					noteClose(ctx, schema.SocketCloseError)
				}
				_ = conn.CloseNow()
				return
			}
		}
	}()
	return ctx
}

// heartbeatFrame is the one frame every heartbeat sends.
var heartbeatFrame = mustHeartbeatFrame()

func mustHeartbeatFrame() []byte {
	raw, err := json.Marshal(schema.HeartbeatV1{Schema: schema.HeartbeatSchemaV1})
	if err != nil {
		panic("server: encode heartbeat: " + err.Error())
	}
	return raw
}

func writeHeartbeat(ctx context.Context, conn *websocket.Conn) error {
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return countFrame(ctx, conn.Write(wctx, websocket.MessageText, heartbeatFrame))
}
