package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const (
	// wsSendBuffer bounds each WebSocket subscriber's queue in the hub; a
	// subscriber that falls this far behind is disconnected rather than
	// allowed to stall the hub (see hub.Hub's slow-consumer policy).
	wsSendBuffer = 64
	// wsWriteTimeout bounds a single frame write so one dead peer cannot pin
	// its handler goroutine.
	wsWriteTimeout = 5 * time.Second
)

// handleWS serves GET /v0/ws?channel=<name>: it upgrades to a WebSocket and
// streams every message posted to the channel from the moment of subscription,
// one JSON-encoded schema.MessageV0 per text frame. Pre-upgrade failures are
// plain HTTP responses with the structured error body.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.URL.Query().Get("channel")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel query parameter is required")
		return
	}
	channel, err := s.store.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel_not_found", "channel not found")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "ws: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept has already written the HTTP error response.
		slog.DebugContext(ctx, "ws: accept failed", "error", err)
		return
	}

	sub := s.hub.Subscribe(channel.ID, wsSendBuffer)
	defer sub.Cancel()

	// The stream is server-to-client only; CloseRead discards client frames
	// and cancels the context when the peer closes or errors.
	ctx = conn.CloseRead(ctx)
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		case msg, ok := <-sub.Messages():
			if !ok {
				// The hub dropped us — tell the client whether to blame
				// itself (too slow) or the server (shutdown), so a client
				// like conch tail knows whether reconnecting makes sense.
				if s.hub.Closed() {
					_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
				} else {
					_ = conn.Close(websocket.StatusPolicyViolation, "subscriber too slow")
				}
				return
			}
			if err := writeWSMessage(ctx, conn, msg); err != nil {
				// 1011: RFC 6455 reserves StatusAbnormalClosure for reporting;
				// it must not go out in a close frame. CloseNow guards against
				// the close handshake blocking on an already-dead peer.
				_ = conn.Close(websocket.StatusInternalError, "write failed")
				_ = conn.CloseNow()
				return
			}
		}
	}
}

func writeWSMessage(ctx context.Context, conn *websocket.Conn, msg schema.MessageV0) error {
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return wsjson.Write(wctx, conn, msg)
}
