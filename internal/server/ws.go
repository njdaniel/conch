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
	// long a socket outlives the expiry of that credential; revoking it
	// (issue #519), revoking all the principal's credentials or disabling the
	// principal closes sockets immediately through the hubs and does not wait
	// for this.
	defaultCredentialRecheckInterval = 30 * time.Second
	// wsRecheckTimeout bounds one liveness read so a stuck store cannot pin
	// the stream loop.
	wsRecheckTimeout = 5 * time.Second

	// wsReplayPageSize is how many messages one page of a replay reads. It is
	// the most the history route allows in a page, so a replay costs what
	// paging the same history over REST costs.
	wsReplayPageSize = maxMessageLimit

	reasonCredentialInvalid = "credential no longer valid" //nolint:gosec // close-frame reason text, not a credential
)

// wsReplay is a /v2/ws ?after= request: send every message the reader may see
// with an id greater than after, then carry on live.
//
// r is the request that opened the socket: the reader is built from it by
// readerFor at the point of the query, exactly as for the history route, and
// not kept anywhere (the visibility guard test allows no other source).
type wsReplay struct {
	after int64
	r     *http.Request
}

// historyGate keeps a socket's live frames to its reader's history bound
// (issue #719, design §4.3). The hub delivers by channel and audience; it
// knows nothing of a bound, and a message committed before a guest's
// admission can still reach the hub after it, as can a new reply in a
// thread whose root is from before. So a socket whose reader is held to a
// bound (store.HistoryBounded, read once the membership check has passed:
// a removal closes the socket, and a re-admission is a new membership on a
// new socket) asks the one visibility condition about every live frame
// before sending it, through the listing the history route uses. A frame it
// would not list is not sent. Every other socket sends what the hub gives
// it, unchanged, at no cost.
//
// r is the request that opened the socket, as for wsReplay: the reader is
// built from it by readerFor at the point of the query.
type historyGate struct {
	bounded   bool
	r         *http.Request
	version   apiVersion
	channelID int64
}

// allows reports whether the frame for message id may be sent.
func (s *Server) allows(ctx context.Context, g historyGate, id int64) (bool, error) {
	if !g.bounded {
		return true, nil
	}
	if id <= 0 {
		return false, nil
	}
	// The page of one that starts just before the message is the message
	// exactly when the reader may see it.
	msgs, err := s.store.ListVisibleMessages(ctx, g.channelID, readerFor(g.r, g.version), id-1, 1)
	if err != nil {
		return false, err
	}
	return len(msgs) == 1 && msgs[0].ID == id, nil
}

// gateFrame is allows for a stream loop: it reports whether to send the
// frame, and whether to go on. A failed read closes conn (fail closed: a
// frame that could not be checked is never sent).
func (s *Server) gateFrame(ctx context.Context, conn *websocket.Conn, g historyGate, id int64) (send, ok bool) {
	send, err := s.allows(ctx, g, id)
	if err != nil {
		if ctx.Err() != nil {
			closeWS(ctx, conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
			return false, false
		}
		slog.ErrorContext(ctx, "ws: history bound check failed", "error", err)
		closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "history check failed")
		return false, false
	}
	return send, true
}

// wsReplayHooks are test seams, nil in production. beforePage runs before page
// n (from 0) is read from the store, afterPage after it is read and before any
// of it is sent. A message posted from either lands in the subscription's
// buffer as well, and, from beforePage or from afterPage of an earlier page, in
// a page too: the case the replay must send only once.
type wsReplayHooks struct {
	beforePage func(page int)
	afterPage  func(page int)
}

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

// handleWSV2 serves GET /v2/ws?channel=<name>[&after=<id>]: each frame is a
// schema.MessageV2. It is the only socket that can be delivered a scoped
// message, and only one the connection's principal is a recipient of; a
// connection with no verified principal (authentication off) receives
// channel-wide messages only. A message the subscriber is not an audience of
// is not sent at all, not even as a placeholder. The recipient list is never
// in a frame.
//
// With after=<id> the server subscribes, sends the visible messages with a
// greater id in ascending order, then streams live; see streamWSV2.
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
	// ?after= is v2 only: v0 and v1 cannot express an audience, so a replay
	// there could not be told apart from a leak. It is checked here, before
	// anything is subscribed or upgraded, so a bad value is a plain 400.
	var replay *wsReplay
	if version == apiV2 && r.URL.Query().Has("after") {
		raw := r.URL.Query().Get("after")
		if raw == "" {
			// nonNegativeQueryInt64 reads an empty value as its default, which
			// would turn "after=" into a replay of the whole channel.
			writeError(w, http.StatusBadRequest, "invalid_request", "after must be a non-negative integer")
			return
		}
		after, err := nonNegativeQueryInt64(r, "after", 0)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		replay = &wsReplay{after: after, r: r}
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
	// The credential is the subscription's too, so that revoking that one
	// credential closes it at once (DropCredential, issue #519).
	credID, hasCred := credentialIDFrom(ctx)
	var sub0 *hub.Subscription
	var sub1 *hub.SubscriptionV1
	var sub2 *hub.SubscriptionV2
	switch version {
	case apiV2:
		sub2 = s.hub.SubscribeV2(channel.ID, principalID, credID, wsSendBuffer)
		defer sub2.Cancel()
	case apiV1:
		sub1 = s.hub.SubscribeV1(channel.ID, principalID, credID, wsSendBuffer)
		defer sub1.Cancel()
	default:
		sub0 = s.hub.Subscribe(channel.ID, principalID, credID, wsSendBuffer)
		defer sub0.Cancel()
	}
	// Same ordering argument for the credential: a disable that committed
	// after the middleware resolved the token but before this subscription
	// registered would have run DropPrincipalAll past it, and a revoke of
	// the credential DropCredential. Re-checking now, after subscribing,
	// closes that window. The failure is the standard 401.
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
	gate := historyGate{r: r, version: version, channelID: channel.ID}
	if principalID > 0 {
		if gate.bounded, err = s.store.HistoryBounded(ctx, channel.ID, principalID); err != nil {
			slog.ErrorContext(ctx, "ws: read history bound failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
	}

	conn, ctx, done, ok := s.acceptSocket(w, r)
	if !ok {
		return
	}
	defer done()
	// Only an authenticated connection re-checks; a nil channel never fires,
	// so AuthOff starts no timer.
	var recheck <-chan time.Time
	if hasCred {
		ticker := time.NewTicker(s.credRecheckInterval)
		defer ticker.Stop()
		recheck = ticker.C
	}
	// The reader and the keepalive start here, so every version has them. Only
	// /v2/ws sends heartbeat frames, and only to a client that asked: the
	// others' readers take every frame for a message.
	ctx = s.keepAlive(ctx, conn, version == apiV2 && r.URL.Query().Get(schema.HeartbeatQueryParam) == "1")
	switch version {
	case apiV2:
		s.streamWSV2(ctx, conn, sub2, replay, gate, principalID, credID, recheck)
		return
	case apiV1:
		s.streamWSV1(ctx, conn, sub1, gate, principalID, credID, recheck)
		return
	}
	sub := sub0

	// The stream is server-to-client only; keepAlive's reader discards client
	// frames and cancels the context when the peer closes or errors.
	for {
		select {
		case <-ctx.Done():
			closeWS(ctx, conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
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
			send, ok := s.gateFrame(ctx, conn, gate, msg.ID)
			if !ok {
				return
			}
			if !send {
				continue
			}
			if err := writeWSMessage(ctx, conn, msg); err != nil {
				// 1011: RFC 6455 reserves StatusAbnormalClosure for reporting;
				// it must not go out in a close frame. CloseNow guards against
				// the close handshake blocking on an already-dead peer.
				closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "write failed")
				_ = conn.CloseNow()
				return
			}
		}
	}
}

func (s *Server) streamWSV1(ctx context.Context, conn *websocket.Conn, sub *hub.SubscriptionV1, gate historyGate, principalID, credID int64, recheck <-chan time.Time) {
	channelID := gate.channelID
	for {
		select {
		case <-ctx.Done():
			closeWS(ctx, conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
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
			send, ok := s.gateFrame(ctx, conn, gate, msg.ID)
			if !ok {
				return
			}
			if !send {
				continue
			}
			if err := writeWSMessageV1(ctx, conn, msg); err != nil {
				closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "write failed")
				_ = conn.CloseNow()
				return
			}
		}
	}
}

// streamWSV2 sends a v2 connection's frames. With replay set it first sends the
// history after replay.after, then the live frames. The subscription was
// registered before the replay read anything, so nothing can fall between the
// two; a message committed during the replay is in both, in a replay page and
// in the subscription's buffer, and must arrive once.
//
// So a live frame is dropped when its id is not greater than the last id the
// replay sent. That is exact, not a guess: message ids are given out under
// SQLite's single write lock, so every message with an id at or below one the
// replay read was already committed when it was read, and, being visible to
// this reader (it was broadcast to them), was in that page.
//
// The mark does not move once the replay is over. Two posts can reach the hub
// out of id order, because each handler broadcasts after its own commit, and a
// mark that followed the live frames would drop the lower id when it came
// second. Live frames after the replay are sent as they arrive, as they are on
// a connection that asked for no replay, each through the history gate.
func (s *Server) streamWSV2(ctx context.Context, conn *websocket.Conn, sub *hub.SubscriptionV2, replay *wsReplay, gate historyGate, principalID, credID int64, recheck <-chan time.Time) {
	channelID := gate.channelID
	var replayed int64 // the last id the replay sent; 0 without one
	if replay != nil {
		var ok bool
		if replayed, ok = s.replayWSV2(ctx, conn, sub, replay, channelID, principalID, credID, recheck); !ok {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			closeWS(ctx, conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
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
			if replay != nil && msg.ID <= replayed {
				continue
			}
			send, ok := s.gateFrame(ctx, conn, gate, msg.ID)
			if !ok {
				return
			}
			if !send {
				continue
			}
			if err := writeWSMessageV2(ctx, conn, msg); err != nil {
				closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "write failed")
				_ = conn.CloseNow()
				return
			}
		}
	}
}

// replayWSV2 sends the messages the reader may see after replay.after, in
// pages through the same visibility function the history route uses, and
// returns the id of the last one sent (0 if there were none: a live frame is
// dropped only for an id already sent, never merely for being at or below the
// client's cursor). ok is false when it has closed conn instead of finishing.
//
// While it runs nobody reads the subscription, so its buffer fills with live
// frames. That buffer is the bound: a replay that lets wsSendBuffer frames
// arrive is a slow consumer like any other and the hub drops it. The replay
// notices at the next frame, closes with the slow-consumer reason and stops
// reading history; the client reconnects with the newest id it got. It does
// not drain the subscription into memory of its own, which would be the same
// queue without the bound.
func (s *Server) replayWSV2(ctx context.Context, conn *websocket.Conn, sub *hub.SubscriptionV2, replay *wsReplay, channelID, principalID, credID int64, recheck <-chan time.Time) (last int64, ok bool) {
	size := s.replayPageSize
	if size <= 0 {
		size = wsReplayPageSize
	}
	cursor := replay.after
	// The stream loop is not running yet, so the replay gives the credential
	// its periodic check itself, before every frame: a page is up to a
	// hundred frames, and a reader who takes each one slowly must not go on
	// receiving history for minutes after the credential was revoked.
	credentialOK := func() bool {
		select {
		case <-recheck:
			return s.credentialStillValid(ctx, conn, credID)
		default:
			return true
		}
	}
	for page := 0; ; page++ {
		if !credentialOK() {
			return last, false
		}
		if ctx.Err() != nil {
			closeWS(ctx, conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
			return last, false
		}
		if hook := s.replayHooks.beforePage; hook != nil {
			hook(page)
		}
		msgs, err := s.store.ListVisibleMessages(ctx, channelID, readerFor(replay.r, apiV2), cursor, size)
		if err != nil {
			if ctx.Err() != nil {
				closeWS(ctx, conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
				return last, false
			}
			// A read that failed because the server is stopping is a
			// shutdown, not a fault.
			if s.hub.Closed() {
				s.closeDropped(ctx, conn, channelID, principalID, credID)
				return last, false
			}
			slog.ErrorContext(ctx, "ws: replay failed", "error", err)
			closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "replay failed")
			return last, false
		}
		if hook := s.replayHooks.afterPage; hook != nil {
			hook(page)
		}
		for _, m := range msgs {
			if !credentialOK() {
				return last, false
			}
			// Dropped by the hub (overflow, removal from the channel, a
			// disabled principal, shutdown): send nothing more and say why.
			if !sub.Live() {
				s.closeDropped(ctx, conn, channelID, principalID, credID)
				return last, false
			}
			if err := writeWSMessageV2(ctx, conn, messageV2FromStore(m)); err != nil {
				closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "write failed")
				_ = conn.CloseNow()
				return last, false
			}
			last, cursor = m.ID, m.ID
		}
		if len(msgs) < size {
			return last, true
		}
	}
}

func writeWSMessage(ctx context.Context, conn *websocket.Conn, msg schema.MessageV0) error {
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return countFrame(ctx, wsjson.Write(wctx, conn, msg))
}

func writeWSMessageV2(ctx context.Context, conn *websocket.Conn, msg schema.MessageV2) error {
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return countFrame(ctx, wsjson.Write(wctx, conn, msg))
}

func writeWSMessageV1(ctx context.Context, conn *websocket.Conn, msg schema.MessageV1) error {
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return countFrame(ctx, wsjson.Write(wctx, conn, msg))
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
		closeWS(ctx, conn, schema.SocketCloseError, websocket.StatusInternalError, "credential check failed")
		return false
	case !live:
		closeWS(ctx, conn, schema.SocketCloseRevoked, websocket.StatusPolicyViolation, reasonCredentialInvalid)
		return false
	}
	return true
}

// closeDropped closes conn after the hub dropped its subscription, telling the
// client why: shutdown, a revoked credential or disabled principal, removal
// from the channel, or falling too far behind.
func (s *Server) closeDropped(ctx context.Context, conn *websocket.Conn, channelID, principalID, credID int64) {
	if s.hub.Closed() {
		closeWS(ctx, conn, schema.SocketCloseServerShutdown, websocket.StatusGoingAway, "server shutting down")
		return
	}
	// The request context may already be done; the reason is advisory, so
	// fall back to the slow-consumer wording on any error.
	bg := context.WithoutCancel(ctx)
	if credID != 0 {
		if live, err := s.store.CredentialLive(bg, credID); err == nil && !live {
			closeWS(ctx, conn, schema.SocketCloseRevoked, websocket.StatusPolicyViolation, reasonCredentialInvalid)
			return
		}
	}
	if principalID != 0 {
		if member, err := s.store.IsChannelMember(bg, channelID, principalID); err == nil && !member {
			closeWS(ctx, conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "no longer a member of this channel")
			return
		}
		if s.agentMayNoLongerRead(bg, principalID, channelID) {
			closeWS(ctx, conn, schema.SocketCloseRemoved, websocket.StatusPolicyViolation, "manifest no longer permits reading this channel")
			return
		}
	}
	closeWS(ctx, conn, schema.SocketCloseSlowConsumer, websocket.StatusPolicyViolation, "subscriber too slow")
}
