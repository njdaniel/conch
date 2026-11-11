package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// handleVoiceWS serves GET /v1/voice/ws?channel=<name>: it upgrades to a
// WebSocket and sends the channel's voice presence as a
// conch.voice_presence.v1 document on connect and again whenever it changes,
// and nothing otherwise. It is a separate route from the message sockets
// because those carry bare message envelopes (design note §6).
//
// It follows ws.go: pre-upgrade failures are plain HTTP responses, the
// subscription is registered before membership is checked (a removal that
// commits during the checks then closes it), and an open socket re-checks its
// credential and membership on an interval and is closed at once when the
// principal is removed, disabled or signed out, or the credential it was
// opened with is revoked.
func (s *Server) handleVoiceWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	version, ok := voicePresenceVersion(r.URL.Query())
	if !ok {
		writeVoicePresenceSchemaInvalid(w)
		return
	}
	name := r.URL.Query().Get("channel")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel query parameter is required")
		return
	}
	var sub *voiceSub
	var credID int64
	var hasCred bool
	channel, caller, ok := s.voicePresenceChannel(w, r, name, func(ch store.Channel, caller store.Principal) bool {
		// The subscription carries the credential, so that revoking that
		// one credential closes it at once (issue #519).
		credID, hasCred = credentialIDFrom(ctx)
		sub = s.voice.subscribe(ch.ID, caller.ID, credID)
		// A disable or a revoke that committed after the middleware
		// resolved the token but before the subscription registered has
		// run its drop already.
		if hasCred {
			live, err := s.store.CredentialLive(ctx, credID)
			if err != nil {
				slog.ErrorContext(ctx, "voice ws: check credential failed", "error", err)
				writeInternalError(w)
				return false
			}
			if !live {
				writeUnauthenticated(w)
				return false
			}
		}
		return true
	})
	if sub != nil {
		defer sub.cancel()
	}
	if !ok {
		return
	}

	conn, ctx, done, ok := s.acceptSocket(w, r)
	if !ok {
		return
	}
	defer done()
	var recheck <-chan time.Time
	if hasCred {
		ticker := time.NewTicker(s.credRecheckInterval)
		defer ticker.Stop()
		recheck = ticker.C
	}
	s.streamVoice(ctx, conn, sub, channel.ID, caller.ID, credID, version, recheck)
}

// streamVoice writes presence until the peer leaves, the subscription is
// dropped, or the credential or membership lapses. Each wake-up rebuilds the
// whole snapshot and sends it only if it differs from the last one sent, so
// the socket carries one frame per change and nothing otherwise. Writes carry
// a deadline; the poller never waits for this loop.
func (s *Server) streamVoice(ctx context.Context, conn *websocket.Conn, sub *voiceSub, channelID, principalID, credID int64, version string, recheck <-chan time.Time) {
	ctx = s.keepAlive(ctx, conn, false)
	var last []byte
	send := func() bool {
		snap, live, err := sub.current(version)
		if !live {
			s.closeVoiceDropped(ctx, conn, sub, channelID, principalID, credID)
			return false
		}
		if err != nil {
			slog.ErrorContext(ctx, "voice ws: built an invalid presence document", "channel", channelID, "error", err)
			closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
			return false
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
			return false
		}
		if bytes.Equal(raw, last) {
			return true
		}
		// A drop that came while the frame was built (a revoke, a disable,
		// a removal) stops it here, as on the events socket (issue #519).
		select {
		case <-sub.dropped:
			s.closeVoiceDropped(ctx, conn, sub, channelID, principalID, credID)
			return false
		default:
		}
		wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
		err = countFrame(ctx, conn.Write(wctx, websocket.MessageText, raw))
		cancel()
		if err != nil {
			closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "write failed")
			_ = conn.CloseNow()
			return false
		}
		last = raw
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			closeWS(ctx, conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
			return
		case <-recheck:
			if !s.credentialStillValid(ctx, conn, credID) || !s.voiceStillMember(ctx, conn, channelID, principalID) {
				return
			}
		case <-sub.wake:
			if !send() {
				return
			}
		case <-sub.dropped:
			s.closeVoiceDropped(ctx, conn, sub, channelID, principalID, credID)
			return
		}
	}
}

// voiceStillMember re-reads the subscriber's membership. It reports true when
// the stream may continue; otherwise it has closed conn, failing closed on a
// store error.
func (s *Server) voiceStillMember(ctx context.Context, conn *websocket.Conn, channelID, principalID int64) bool {
	cctx, cancel := context.WithTimeout(ctx, wsRecheckTimeout)
	defer cancel()
	member, err := s.store.IsChannelMember(cctx, channelID, principalID)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "voice ws: recheck membership failed", "error", err)
		closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "membership check failed")
		return false
	case !member:
		closeWS(ctx, conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "no longer a member of this channel")
		return false
	}
	return true
}

// closeVoiceDropped closes conn after its subscription was dropped, saying
// why: shutdown, a revoked credential or disabled principal, removal from the
// channel, or a moderator's voice-only removal (issue #850). The last is the
// one close whose text is the reason itself, schema.SocketCloseVoiceRemoved:
// the subscriber is still a member, and a client keys on that value to stay
// out of voice without taking the channel for gone (issue #1409). The stronger
// reason always wins: membership is read from the store first, and a removal
// that commits after that read reaches the kicked subscription through
// dropPrincipal, under the poller's lock, before the reason is chosen. A
// membership read that fails closes with an error, never voice_removed.
func (s *Server) closeVoiceDropped(ctx context.Context, conn *websocket.Conn, sub *voiceSub, channelID, principalID, credID int64) {
	if s.voice.isClosed() {
		closeWS(ctx, conn, schema.SocketCloseServerShutdown, websocket.StatusGoingAway, "server shutting down")
		return
	}
	bg := context.WithoutCancel(ctx)
	if credID != 0 {
		if live, err := s.store.CredentialLive(bg, credID); err == nil && !live {
			closeWS(ctx, conn, schema.SocketCloseRevoked, websocket.StatusPolicyViolation, reasonCredentialInvalid)
			return
		}
	}
	member, err := s.store.IsChannelMember(bg, channelID, principalID)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "voice ws: membership check after drop failed", "error", err)
		closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "membership check failed")
		return
	case !member:
		closeWS(ctx, conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "no longer a member of this channel")
		return
	}
	if hook := s.voice.beforeCloseReason; hook != nil {
		hook()
	}
	switch sub.dropCause() {
	case voiceDropRemoved:
		closeWS(ctx, conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "no longer a member of this channel")
	case voiceDropKicked:
		closeWS(ctx, conn, schema.SocketCloseVoiceRemoved, websocket.StatusPolicyViolation, string(schema.SocketCloseVoiceRemoved))
	default:
		closeWS(ctx, conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "presence subscription closed")
	}
}
