package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/internal/server/hub"
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
// one JSON-encoded schema.MessageV0 per text frame. The v0 wire is unchanged:
// body-only frames, no envelope or payload fields. Pre-upgrade failures are
// plain HTTP responses with the structured error body.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	s.handleWSVersion(w, r, false)
}

// handleWSV1 serves GET /v1/ws?channel=<name>: the same subscription contract
// as handleWS, but each frame is a full schema.MessageV1 envelope including
// any typed payload.
func (s *Server) handleWSV1(w http.ResponseWriter, r *http.Request) {
	s.handleWSVersion(w, r, true)
}

func (s *Server) handleWSVersion(w http.ResponseWriter, r *http.Request, v1 bool) {
	ctx := r.Context()
	name := r.URL.Query().Get("channel")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel query parameter is required")
		return
	}
	channel, err := s.store.ChannelByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		writeChannelNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "ws: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	// Subscribe BEFORE checking membership. DropPrincipal (run when a member is
	// removed, after the removal commits) only closes subscriptions that are
	// registered, so checking first and subscribing second would let a
	// subscription slip in between and outlive the removal. With this order
	// either the check sees the removal (refused) or the subscription exists
	// when the drop runs (closed). A refused request cancels the subscription
	// before anything is sent on it.
	var principalID int64
	if caller, ok := callerFrom(ctx); ok {
		principalID = caller.ID
	}
	var sub0 *hub.Subscription
	var sub1 *hub.SubscriptionV1
	if v1 {
		sub1 = s.hub.SubscribeV1(channel.ID, principalID, wsSendBuffer)
		defer sub1.Cancel()
	} else {
		sub0 = s.hub.Subscribe(channel.ID, principalID, wsSendBuffer)
		defer sub0.Cancel()
	}
	member, err := s.callerIsMember(r, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "ws: check membership failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if !member {
		writeChannelNotFound(w)
		return
	}
	// An agent subscribing over WebSocket is reading the channel, so its
	// manifest must allow that here (issue #79).
	if !s.agentCallerAllowed(w, r, schema.CapabilityMessagesRead, channel.ID, schema.ChannelPermissionRead) {
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept has already written the HTTP error response.
		slog.DebugContext(ctx, "ws: accept failed", "error", err)
		return
	}
	if v1 {
		s.streamWSV1(ctx, conn, sub1, channel.ID, principalID)
		return
	}
	sub := sub0

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
				s.closeDropped(ctx, conn, channel.ID, principalID)
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

func (s *Server) streamWSV1(ctx context.Context, conn *websocket.Conn, sub *hub.SubscriptionV1, channelID, principalID int64) {
	ctx = conn.CloseRead(ctx)
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		case msg, ok := <-sub.Messages():
			if !ok {
				s.closeDropped(ctx, conn, channelID, principalID)
				return
			}
			if err := writeWSMessageV1(ctx, conn, msg); err != nil {
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

func writeWSMessageV1(ctx context.Context, conn *websocket.Conn, msg schema.MessageV1) error {
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return wsjson.Write(wctx, conn, msg)
}

// closeDropped closes conn after the hub dropped its subscription, telling the
// client why: shutdown, removal from the channel, or falling too far behind.
func (s *Server) closeDropped(ctx context.Context, conn *websocket.Conn, channelID, principalID int64) {
	if s.hub.Closed() {
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	if principalID != 0 {
		// The request context may already be done; the reason is advisory, so
		// fall back to the slow-consumer wording on any error.
		if member, err := s.store.IsChannelMember(context.WithoutCancel(ctx), channelID, principalID); err == nil && !member {
			_ = conn.Close(websocket.StatusPolicyViolation, "no longer a member of this channel")
			return
		}
	}
	_ = conn.Close(websocket.StatusPolicyViolation, "subscriber too slow")
}
