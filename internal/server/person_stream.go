package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The per-person events stream (issue #2040, child of #2013; ADR-017 and
// docs/design/direct-messages.md §2.3).
//
// GET /v1/events/person/ws is scoped to one signed-in person, not to a
// channel. A member with no channel open still gets their direct-message
// frames on it and, when the call frames land (#2029), incoming-call rings.
//
// Why a route of its own, not /v1/events/ws without ?channel=: the channel
// socket's hub, its subscription and every frame it builds are keyed by a
// channel, and its checks (membership, manifest, roster) are the channel's.
// Making channel optional would put a second, person-scoped mode inside code
// whose one job is to carry channel documents, and "this socket never carries
// a channel frame" would depend on a branch inside it. Here it is structural:
// this file has no channel in any signature, and the only way a frame reaches
// a socket is publish(principal, doc) with a doc of a registered kind
// (personFrameKinds). The channel socket's behaviour and code are unchanged.
//
// What a socket sends, in order: conch.person_stream_hello.v1 (its own
// instance id), conch.dm_snapshot.v1, then the registered frames addressed to
// its principal as they are published, and the heartbeat every socket gets
// (conch.heartbeat.v1, keepalive.go), which a client's silence watchdog lives
// on and which names no one. Frames are addressed by principal id:
// a frame published for principal 7 reaches only 7's sockets. A frame that
// arrives while the snapshot is being taken is held and sent after it; a DM
// frame for a conversation at or below the Revision the snapshot reported for
// that conversation is already reflected in the snapshot and is not sent
// (DMSnapshotV1: the ordering is per conversation, by Revision; there is no
// global high-water mark). What a socket missed while it was away is not
// replayed here: the client reads it from GET /v1/dms/{c}/changes?after=.
//
// Delivery needs current access, decided when a frame is about to be written,
// not when it was queued (direct-messages.md §3.2): the credential is live,
// the person is enabled, and dmFrameAccess (installed by the DM store, which
// holds the block relation of #1832) says the person may be sent frames of
// that conversation now. During a block neither participant gets a frame of
// the pair, the author's own other devices included. A frame the person may
// not have is dropped, not queued for later; a failed check closes the socket
// (fail closed), and the client reconnects and re-reads.
//
// Who may open it: a verified human member's own credential. An agent
// (403 forbidden), a guest (403 guest_not_allowed) and a caller without
// proof of their own sign-in (403 own_sign_in_required) are refused before
// the upgrade.
//
// Revocation: principalDisabled, authChanged, guestsEnded and
// credentialRevoked close the sockets at once through dropPrincipalAll and
// dropCredential, as for channel sockets; and the interval re-check and the
// re-check before each write close a socket whose credential or
// principal lapsed without a drop reaching it (fail closed).
//
// This file holds no DM visibility rule and no SQL beyond the credential and
// principal re-checks. The DM issues compute recipients against current state
// and call publish; the snapshot comes from a source they install
// (Server.dmSnapshotSource).

const (
	// personQueueMax bounds the frames waiting for one socket. A socket that
	// falls this far behind is closed (slow consumer) rather than allowed to
	// hold memory or slow a writer; it reconnects and gets a fresh snapshot.
	personQueueMax = 256
)

// personDoc is a document the stream can carry.
type personDoc interface{ Validate() error }

// personFrameKind is one entry of the stream's frame-kind registry. A frame
// kind is a schema this stream may carry; publish refuses a document no entry
// accepts, so a channel document (or any other) cannot be sent on it by
// mistake. The call frames of #2029 are one more entry here, and need no
// change to the hub or the socket.
type personFrameKind struct {
	// schema is the schema name the frame carries, for logs.
	schema string
	// accept reports whether doc is this kind's document, the DM conversation
	// it concerns and the conversation Revision it carries (0 for a kind with
	// none; used to drop a change the snapshot already reflects).
	accept func(doc personDoc) (dmKey, bool)
}

// dmKey identifies the conversation, revision and owner of reader-private data
// carried by a frame. Zero means the corresponding field does not apply.
type dmKey struct {
	conversationID int64
	revision       int64
	readerID       int64
}

// kindOf builds the registry entry for document type T.
func kindOf[T personDoc](name string, key func(T) dmKey) personFrameKind {
	return personFrameKind{schema: name, accept: func(doc personDoc) (dmKey, bool) {
		d, ok := doc.(T)
		if !ok {
			return dmKey{}, false
		}
		return key(d), true
	}}
}

func messageKey(m schema.DMMessageV1) dmKey {
	return dmKey{conversationID: m.ConversationID, revision: m.Revision}
}

// readerDMChange binds a change signal to the reader whose projection changed.
// Publishers take readerID from the changed row or its visibility check,
// independently of the addressee. Only DMChangedV1 is encoded on the wire.
type readerDMChange struct {
	schema.DMChangedV1
	readerID int64
}

func (d readerDMChange) Validate() error {
	if d.readerID <= 0 {
		return errors.New("DM change requires a reader")
	}
	if !d.Kind.Valid() {
		return errors.New("unrecognized DM change kind")
	}
	return d.DMChangedV1.Validate()
}

// personFrameKinds is the registry. Only the frames of ADR-017 that are
// addressed to one person belong here. The hello and the snapshot are the
// socket's own and are not published, so they are not in it.
var personFrameKinds = []personFrameKind{
	kindOf(schema.DMMessageSchemaV1, messageKey),
	kindOf(schema.DMMessageEditedSchemaV1, func(f schema.DMMessageEditedV1) dmKey { return messageKey(f.Message) }),
	kindOf(schema.DMMessageRemovedSchemaV1, func(f schema.DMMessageRemovedV1) dmKey { return messageKey(f.Message) }),
	// A change signal names a conversation but no revision: it is never
	// dropped as a duplicate (it only asks the client to re-read).
	kindOf(schema.DMChangedSchemaV1, func(f readerDMChange) dmKey {
		return dmKey{conversationID: f.ConversationID, readerID: f.readerID}
	}),
	// TODO(#2029): the call frames (call.state) are registered here.
}

// personFrame is one encoded, validated frame in a socket's queue.
type personFrame struct {
	raw []byte
	key dmKey // the DM conversation and revision it carries
}

var errPersonFrameKind = errors.New("server: a document that is not a person-stream frame")

// newPersonFrame validates and encodes doc if a registered kind accepts it.
func newPersonFrame(doc personDoc) (personFrame, error) {
	for _, k := range personFrameKinds {
		key, ok := k.accept(doc)
		if !ok {
			continue
		}
		if err := doc.Validate(); err != nil {
			return personFrame{}, fmt.Errorf("server: person stream %s frame: %w", k.schema, err)
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			return personFrame{}, fmt.Errorf("server: encode person stream %s frame: %w", k.schema, err)
		}
		return personFrame{raw: raw, key: key}, nil
	}
	return personFrame{}, fmt.Errorf("%w: %T", errPersonFrameKind, doc)
}

// personHub is the registry of person-stream sockets.
type personHub struct {
	mu     sync.Mutex
	subs   map[*personSub]struct{}
	closed bool
}

func newPersonHub() *personHub {
	return &personHub{subs: make(map[*personSub]struct{})}
}

// personSub is one socket's subscription. queue and gone are guarded by
// hub.mu.
type personSub struct {
	hub          *personHub
	principalID  int64
	credentialID int64
	// instanceID is the socket's own id (PersonStreamHelloV1).
	instanceID string
	// wake has room for one signal; writers only ever send non-blocking.
	wake    chan struct{}
	dropped chan struct{}
	queue   []personFrame
	gone    bool
}

// newInstanceID is 128 random bits as lowercase hex.
func newInstanceID() (string, error) {
	var b [schema.PersonStreamInstanceIDLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("server: person stream instance id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// subscribe registers a socket. After closeAll it returns one that is
// already dropped.
func (h *personHub) subscribe(principalID, credentialID int64, instanceID string) *personSub {
	sub := &personSub{hub: h, principalID: principalID, credentialID: credentialID, instanceID: instanceID, wake: make(chan struct{}, 1), dropped: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		sub.gone = true
		close(sub.dropped)
		return sub
	}
	h.subs[sub] = struct{}{}
	return sub
}

func (sub *personSub) cancel() {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	delete(sub.hub.subs, sub)
}

// dropLocked closes sub's dropped channel once. h.mu is held.
func (h *personHub) dropLocked(sub *personSub) {
	if sub.gone {
		return
	}
	sub.gone = true
	sub.queue = nil
	close(sub.dropped)
}

// publish queues doc for every socket of principalID and for no one else. It
// never blocks and never reads the store. A socket whose queue is full is
// dropped (slow consumer). It reports an error, and sends nothing, for a
// document no registered kind accepts or that fails its Validate.
func (h *personHub) publish(principalID int64, doc personDoc) error {
	if principalID <= 0 {
		return errors.New("server: person stream frame has no addressee")
	}
	f, err := newPersonFrame(doc)
	if err != nil {
		return err
	}
	if f.key.readerID != 0 && f.key.readerID != principalID {
		return errors.New("server: DM change addressed to a different reader")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.principalID != principalID || sub.gone {
			continue
		}
		if len(sub.queue) >= personQueueMax {
			h.dropLocked(sub)
			continue
		}
		sub.queue = append(sub.queue, f)
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// take returns what is waiting and empties it, or live=false once dropped.
func (sub *personSub) take() (frames []personFrame, live bool) {
	sub.hub.mu.Lock()
	defer sub.hub.mu.Unlock()
	if sub.gone {
		return nil, false
	}
	frames, sub.queue = sub.queue, nil
	return frames, true
}

// dropPrincipalAll closes principalID's person sockets. A zero principal
// matches nothing.
func (h *personHub) dropPrincipalAll(principalID int64) {
	if principalID == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.principalID == principalID {
			h.dropLocked(sub)
		}
	}
}

// dropCredential closes the person sockets opened with credentialID; the
// principal's sockets through its other credentials stay. A zero credential
// matches nothing.
func (h *personHub) dropCredential(credentialID int64) {
	if credentialID == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.credentialID == credentialID {
			h.dropLocked(sub)
		}
	}
}

// closeAll drops every socket and refuses new ones: shutdown.
func (h *personHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for sub := range h.subs {
		h.dropLocked(sub)
	}
}

func (h *personHub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// dmSnapshotSource builds the reader's DM snapshot (unread counts, and later
// the active call attempts). The DM conversation issues install the real one;
// the stream does not know how DM state is stored. It is called after the
// socket is subscribed, so a frame published while it runs is held, not lost.
type dmSnapshotSource func(ctx context.Context, principalID int64) (personDMSnapshot, error)

type personDMSnapshot struct {
	doc schema.DMSnapshotV1
	// privateOpeners maps a conversation that has never been shared to the
	// reader who opened it, from that reader's own persisted state.
	privateOpeners map[int64]int64
}

// dmFrameAccess reports whether principalID may be sent frames of DM
// conversation conversationID right now: no personal block either way between
// the pair (#1832), DMs enabled for the person, and the person a participant.
// The DM store installs it. A missing hook denies every DM frame. The hook is
// asked at write time, so a block that lands while a frame is queued stops it.
type dmFrameAccess func(ctx context.Context, principalID, conversationID int64) (bool, error)

// emptyDMSnapshot is the source until the DM store is installed: no
// conversations, and DMs not available (dms_enabled false), which is the
// truth while no DM backend exists on this server.
func (s *Server) emptyDMSnapshot(_ context.Context, _ int64) (schema.DMSnapshotV1, error) {
	return schema.DMSnapshotV1{
		Schema:        schema.DMSnapshotSchemaV1,
		Conversations: []schema.DMConversationV1{},
		TakenAt:       schema.NewTimestamp(s.now()),
	}, nil
}

func (s *Server) personSnapshot(ctx context.Context, principalID int64) (schema.DMSnapshotV1, error) {
	if s.dmSnapshotSource == nil {
		return s.emptyDMSnapshot(ctx, principalID)
	}
	result, err := s.dmSnapshotSource(ctx, principalID)
	if err != nil {
		return schema.DMSnapshotV1{}, err
	}
	snap := result.doc
	if err := snap.Validate(); err != nil {
		return schema.DMSnapshotV1{}, err
	}
	for _, c := range snap.Conversations {
		// Validate pins a two-person pair and a peer who is one of its
		// members. The reader is the other member.
		if c.PeerID == principalID || !slices.Contains(c.ParticipantIDs, principalID) {
			return schema.DMSnapshotV1{}, errors.New("person stream: snapshot row belongs to a different reader")
		}
		if c.CreatedAt == nil && result.privateOpeners[c.ID] != principalID {
			return schema.DMSnapshotV1{}, errors.New("person stream: snapshot row has no matching private open")
		}
	}
	return snap, nil
}

// personStreamCaller is the check before the upgrade: a verified person with
// a credential. An agent and a guest are refused (403, audited as any refused
// people-only route is); with authentication off there is no person.
func (s *Server) personStreamCaller(w http.ResponseWriter, r *http.Request) (store.Principal, int64, bool) {
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeError(w, http.StatusBadRequest, schema.ErrorCodeDMRequireAuth, "the person stream requires authentication")
		return store.Principal{}, 0, false
	}
	credID, ok := credentialIDFrom(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	}
	switch {
	case caller.Kind != store.PrincipalHuman:
		s.auditAgentDenial(r.Context(), caller.ID, r.Pattern, "", 0, denyAgentOnHumanRoute)
		writeError(w, http.StatusForbidden, errForbidden.Code, "the person stream is for people")
		return store.Principal{}, 0, false
	case caller.IsGuest():
		s.denyForbidden(w, r, caller, schema.ErrorCodeGuestNotAllowed, "a guest cannot do this")
		return store.Principal{}, 0, false
	case caller.DisabledAt != nil || caller.HiddenAt != nil:
		writeUnauthenticated(w)
		return store.Principal{}, 0, false
	}
	// An ordinary HTTP request cannot upgrade and needs no provenance read.
	if len(r.Header.Values("Upgrade")) == 0 {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		http.Error(w, "a WebSocket upgrade is required", http.StatusUpgradeRequired)
		return store.Principal{}, 0, false
	}
	// Provenance (a credential the person made for themselves, not an
	// operator's act-as credential or sign-in link) is checked by guard before
	// this handler runs: the route is in selfMadeCredentialRoutes (#2042,
	// #2047), which answers 403 own_sign_in_required for a ticket as for a
	// bearer.
	return caller, credID, true
}

// personStillAllowed reports whether the principal and the credential are
// still good: a live credential, and a person who is human, enabled, visible
// and not a guest. It fails closed: a store error is "no".
func (s *Server) personStillAllowed(ctx context.Context, principalID, credID int64) (ok bool, reason schema.SocketCloseReason, text string, err error) {
	cctx, cancel := context.WithTimeout(ctx, wsRecheckTimeout)
	defer cancel()
	p, err := s.store.PrincipalByID(cctx, principalID)
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		return false, schema.SocketCloseRevoked, reasonCredentialInvalid, nil
	case err != nil:
		return false, schema.SocketCloseError, "principal check failed", err
	case p.Kind != store.PrincipalHuman || p.IsGuest() || p.DisabledAt != nil || p.HiddenAt != nil:
		return false, schema.SocketCloseRevoked, reasonCredentialInvalid, nil
	}
	live, err := s.store.CredentialLive(cctx, credID)
	if err != nil {
		return false, schema.SocketCloseError, "credential check failed", err
	}
	if !live {
		return false, schema.SocketCloseRevoked, reasonCredentialInvalid, nil
	}
	return true, "", "", nil
}

// handlePersonEventsWS serves GET /v1/events/person/ws. The route takes a
// ticket, as the channel socket does: it is the socket a browser opens.
//
// It follows events_ws.go: the subscription is registered before the last
// checks, so a disable or a revoke that commits in between closes it, and
// nothing is upgraded for a refusal.
func (s *Server) handlePersonEventsWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credID, ok := s.personStreamCaller(w, r)
	if !ok {
		return
	}
	instanceID, err := newInstanceID()
	if err != nil {
		slog.ErrorContext(ctx, "person stream: no instance id", "error", err)
		writeInternalError(w)
		return
	}
	sub := s.person.subscribe(caller.ID, credID, instanceID)
	defer sub.cancel()
	allowed, _, _, err := s.personStillAllowed(ctx, caller.ID, credID)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "person stream: check failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	case !allowed:
		writeUnauthenticated(w)
		return
	}
	// The snapshot is taken after the subscription, so a frame published
	// while it is being built is queued, and sent after it unless the
	// snapshot already reflects it (its conversation's Revision).
	snap, err := s.personSnapshot(ctx, caller.ID)
	if err != nil {
		slog.ErrorContext(ctx, "person stream: snapshot failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	allowed, _, _, err = s.personStillAllowed(ctx, caller.ID, credID)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "person stream: check after snapshot failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	case !allowed:
		writeUnauthenticated(w)
		return
	}
	conn, ctx, done, ok := s.acceptSocket(w, r)
	if !ok {
		return
	}
	defer done()
	ticker := time.NewTicker(s.credRecheckInterval)
	defer ticker.Stop()
	ps := &personStream{s: s, conn: conn, sub: sub, principalID: caller.ID, credID: credID, snapRevisions: snapshotRevisions(snap)}
	ps.run(ctx, snap, ticker.C)
}

// personStream is one open person socket.
type personStream struct {
	s           *Server
	conn        *websocket.Conn
	sub         *personSub
	principalID int64
	credID      int64
	// snapRevisions is the Revision the snapshot reported for each
	// conversation it lists: a frame for one of them at or below it is
	// already in the snapshot. A conversation the snapshot does not list has
	// no entry, and its frames are all sent.
	snapRevisions map[int64]int64
}

func snapshotRevisions(snap schema.DMSnapshotV1) map[int64]int64 {
	revs := make(map[int64]int64, len(snap.Conversations))
	for _, c := range snap.Conversations {
		revs[c.ID] = c.Revision
	}
	return revs
}

// run sends the hello, the snapshot and then frames until the peer leaves,
// the subscription is dropped or the credential or person lapses.
func (p *personStream) run(ctx context.Context, snap schema.DMSnapshotV1, recheck <-chan time.Time) {
	ctx = p.s.keepAlive(ctx, p.conn, true)
	hello := schema.PersonStreamHelloV1{Schema: schema.PersonStreamHelloSchemaV1, InstanceID: p.sub.instanceID}
	if !p.sendDoc(ctx, hello) || !p.sendDoc(ctx, snap) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			closeWS(ctx, p.conn, schema.SocketCloseClient, websocket.StatusNormalClosure, "")
			return
		case <-recheck:
			if !p.stillAllowed(ctx) {
				return
			}
		case <-p.sub.wake:
			if !p.flush(ctx) {
				return
			}
		case <-p.sub.dropped:
			p.s.closePersonDropped(ctx, p.conn, p.principalID, p.credID)
			return
		}
	}
}

// stillAllowed closes the socket and reports false if the credential or the
// person lapsed.
func (p *personStream) stillAllowed(ctx context.Context) bool {
	ok, reason, text, err := p.s.personStillAllowed(ctx, p.principalID, p.credID)
	if err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "person stream: recheck failed", "principal", p.principalID, "error", err)
	}
	if !ok {
		code := websocket.StatusPolicyViolation
		if reason == schema.SocketCloseError {
			code = websocket.StatusInternalError
		}
		closeWS(ctx, p.conn, reason, code, text)
	}
	return ok
}

// flush sends what is waiting, after asking again whether the person may
// still read it: a socket sends nothing to someone who has lost access.
func (p *personStream) flush(ctx context.Context) bool {
	frames, live := p.sub.take()
	if !live {
		p.s.closePersonDropped(ctx, p.conn, p.principalID, p.credID)
		return false
	}
	for _, f := range frames {
		if f.key.revision != 0 && f.key.revision <= p.snapRevisions[f.key.conversationID] {
			continue // the snapshot already reflects it
		}
		if f.key.conversationID != 0 {
			check := p.s.dmFrameAccess
			if check == nil {
				continue
			}
			allowed, err := check(ctx, p.principalID, f.key.conversationID)
			if err != nil {
				if ctx.Err() == nil {
					slog.ErrorContext(ctx, "person stream: access check failed", "principal", p.principalID, "error", err)
				}
				closeWS(ctx, p.conn, schema.SocketCloseError, websocket.StatusInternalError, "access check failed")
				return false
			}
			if !allowed {
				continue // no access now: this person is sent nothing of it
			}
		}
		if !p.write(ctx, f.raw) {
			return false
		}
	}
	return true
}

func (p *personStream) sendDoc(ctx context.Context, doc personDoc) bool {
	if err := doc.Validate(); err != nil {
		slog.ErrorContext(ctx, "person stream: built an invalid document", "principal", p.principalID, "error", err)
		closeWS(ctx, p.conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
		return false
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		slog.ErrorContext(ctx, "person stream: encode failed", "principal", p.principalID, "error", err)
		closeWS(ctx, p.conn, schema.SocketCloseError, websocket.StatusInternalError, "internal error")
		return false
	}
	return p.write(ctx, raw)
}

func (p *personStream) write(ctx context.Context, raw []byte) bool {
	// Encoding, access checks and earlier writes may outlast a credential.
	if !p.stillAllowed(ctx) {
		return false
	}
	// Check for a drop that landed during the liveness read.
	select {
	case <-p.sub.dropped:
		p.s.closePersonDropped(ctx, p.conn, p.principalID, p.credID)
		return false
	default:
	}
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	if err := countFrame(ctx, p.conn.Write(wctx, websocket.MessageText, raw)); err != nil {
		closeWS(ctx, p.conn, schema.SocketCloseError, websocket.StatusInternalError, "write failed")
		_ = p.conn.CloseNow()
		return false
	}
	return true
}

// closePersonDropped closes conn after its subscription was dropped, saying
// why: shutdown, a revoked credential or a disabled person, or falling too
// far behind.
func (s *Server) closePersonDropped(ctx context.Context, conn *websocket.Conn, principalID, credID int64) {
	if s.person.isClosed() {
		closeWS(ctx, conn, schema.SocketCloseServerShutdown, websocket.StatusGoingAway, "server shutting down")
		return
	}
	// The request context may already be done; the reason is advisory.
	bg := context.WithoutCancel(ctx)
	if ok, _, _, err := s.personStillAllowed(bg, principalID, credID); err == nil && !ok {
		closeWS(ctx, conn, schema.SocketCloseRevoked, websocket.StatusPolicyViolation, reasonCredentialInvalid)
		return
	}
	closeWS(ctx, conn, schema.SocketCloseSlowConsumer, websocket.StatusPolicyViolation, "subscriber too slow")
}
