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
	// defaultCredentialRecheckInterval is how often an authenticated socket
	// re-checks the credential it connected with (issue #101). It bounds how
	// long a socket outlives the revocation or expiry of that one credential;
	// disabling a principal or revoking all its credentials closes sockets
	// immediately through the hub and does not wait for this.
	defaultCredentialRecheckInterval = 30 * time.Second
	// wsRecheckTimeout bounds one liveness read so a stuck store cannot pin
	// the stream loop.
	wsRecheckTimeout = 5 * time.Second

	reasonCredentialInvalid = "credential no longer valid" //nolint:gosec // close-frame reason text, not a credential
)

// handleWS serves GET /v0/ws?channel=<name>: it upgrades to a WebSocket and
// streams every message posted to the channel from the moment of subscription,
// one JSON-encoded schema.MessageV0 per text frame. The v0 wire is unchanged:
// body-only frames, no envelope or payload fields. Pre-upgrade failures are
// plain HTTP responses with the structured error body.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	s.handleWSVersion(w, r, apiV0)
}

// handleWSV1 serves GET /v1/ws?channel=<name>: the same subscription contract
// as handleWS, but each frame is a full schema.MessageV1 envelope including
// any typed payload.
func (s *Server) handleWSV1(w http.ResponseWriter, r *http.Request) {
	s.handleWSVersion(w, r, apiV1)
}

// handleWSV2 serves GET /v2/ws?channel=<name>: each frame is a
// schema.MessageV2. It is the only socket that can be delivered a scoped
// message, and only one the connection's principal is a recipient of; a
// connection with no verified principal (authentication off) receives
// channel-wide messages only. A message the subscriber is not an audience of
// is not sent at all, not even as a placeholder. The recipient list is never
// in a frame.
func (s *Server) handleWSV2(w http.ResponseWriter, r *http.Request) {
	s.handleWSVersion(w, r, apiV2)
}

func (s *Server) handleWSVersion(w http.ResponseWriter, r *http.Request, version apiVersion) {
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
	var sub2 *hub.SubscriptionV2
	switch version {
	case apiV2:
		sub2 = s.hub.SubscribeV2(channel.ID, principalID, wsSendBuffer)
		defer sub2.Cancel()
	case apiV1:
		sub1 = s.hub.SubscribeV1(channel.ID, principalID, wsSendBuffer)
		defer sub1.Cancel()
	default:
		sub0 = s.hub.Subscribe(channel.ID, principalID, wsSendBuffer)
		defer sub0.Cancel()
	}
	// Same ordering argument for the credential: a disable that committed
	// after the middleware resolved the token but before this subscription
	// registered would have run DropPrincipalAll past it. Re-checking now,
	// after subscribing, closes that window. The failure is the standard 401.
	credID, hasCred := credentialIDFrom(ctx)
	if hasCred {
		live, err := s.store.CredentialLive(ctx, credID)
		if err != nil {
			slog.ErrorContext(ctx, "ws: check credential failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		if !live {
			writeUnauthenticated(w)
			return
		}
	}
	member, err := s.callerIsMember(r, channel.ID)
	if err != nil {
		slog.ErrorContext(ctx, "ws: check membership failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if !member {
		s.auditAgentNonMember(r, schema.CapabilityMessagesRead, channel.ID)
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
	// Only an authenticated connection re-checks; a nil channel never fires,
	// so AuthOff starts no timer.
	var recheck <-chan time.Time
	if hasCred {
		ticker := time.NewTicker(s.credRecheckInterval)
		defer ticker.Stop()
		recheck = ticker.C
	}
	switch version {
	case apiV2:
		s.streamWSV2(ctx, conn, sub2, channel.ID, principalID, credID, recheck)
		return
	case apiV1:
		s.streamWSV1(ctx, conn, sub1, channel.ID, principalID, credID, recheck)
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
		case <-recheck:
			if !s.credentialStillValid(ctx, conn, credID) {
				return
			}
		case msg, ok := <-sub.Messages():
			if !ok {
				// The hub dropped us — tell the client whether to blame
				// itself (too slow) or the server (shutdown), so a client
				// like conch tail knows whether reconnecting makes sense.
				s.closeDropped(ctx, conn, channel.ID, principalID, credID)
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

func (s *Server) streamWSV1(ctx context.Context, conn *websocket.Conn, sub *hub.SubscriptionV1, channelID, principalID, credID int64, recheck <-chan time.Time) {
	ctx = conn.CloseRead(ctx)
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		case <-recheck:
			if !s.credentialStillValid(ctx, conn, credID) {
				return
			}
		case msg, ok := <-sub.Messages():
			if !ok {
				s.closeDropped(ctx, conn, channelID, principalID, credID)
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

func (s *Server) streamWSV2(ctx context.Context, conn *websocket.Conn, sub *hub.SubscriptionV2, channelID, principalID, credID int64, recheck <-chan time.Time) {
	ctx = conn.CloseRead(ctx)
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		case <-recheck:
			if !s.credentialStillValid(ctx, conn, credID) {
				return
			}
		case msg, ok := <-sub.Messages():
			if !ok {
				s.closeDropped(ctx, conn, channelID, principalID, credID)
				return
			}
			wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := wsjson.Write(wctx, conn, msg)
			cancel()
			if err != nil {
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

// credentialStillValid re-reads the credential the connection authenticated
// with. It reports true when the stream may continue. Otherwise it has closed
// conn: with a policy violation when the credential is revoked, expired, or
// its principal disabled, and fail closed with a generic reason when the store
// itself failed (the error is logged, never sent to the client).
func (s *Server) credentialStillValid(ctx context.Context, conn *websocket.Conn, credID int64) bool {
	cctx, cancel := context.WithTimeout(ctx, wsRecheckTimeout)
	defer cancel()
	live, err := s.store.CredentialLive(cctx, credID)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "ws: recheck credential failed", "error", err)
		_ = conn.Close(websocket.StatusInternalError, "credential check failed")
		return false
	case !live:
		_ = conn.Close(websocket.StatusPolicyViolation, reasonCredentialInvalid)
		return false
	}
	return true
}

// closeDropped closes conn after the hub dropped its subscription, telling the
// client why: shutdown, a revoked credential or disabled principal, removal
// from the channel, or falling too far behind.
func (s *Server) closeDropped(ctx context.Context, conn *websocket.Conn, channelID, principalID, credID int64) {
	if s.hub.Closed() {
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	// The request context may already be done; the reason is advisory, so
	// fall back to the slow-consumer wording on any error.
	bg := context.WithoutCancel(ctx)
	if credID != 0 {
		if live, err := s.store.CredentialLive(bg, credID); err == nil && !live {
			_ = conn.Close(websocket.StatusPolicyViolation, reasonCredentialInvalid)
			return
		}
	}
	if principalID != 0 {
		if member, err := s.store.IsChannelMember(bg, channelID, principalID); err == nil && !member {
			_ = conn.Close(websocket.StatusPolicyViolation, "no longer a member of this channel")
			return
		}
		if s.agentMayNoLongerRead(bg, principalID, channelID) {
			_ = conn.Close(websocket.StatusPolicyViolation, "manifest no longer permits reading this channel")
			return
		}
	}
	_ = conn.Close(websocket.StatusPolicyViolation, "subscriber too slow")
}
