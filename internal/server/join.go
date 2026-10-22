package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Guest entry (issue #710, ADR-009 D1, docs/design/guests-and-accounts.md §2,
// §7.2): the front door, joining as a guest from the bare address or through
// an invite, the operator's joining settings, the guest list and End all
// guest visits. The store (store/guest_policy.go) holds the policy and checks
// it again in the transaction that starts a visit; these handlers add what
// only HTTP has: rate limits, body limits and the client address.
//
// The two entry routes are exempt from authentication and mint a credential,
// so they answer only over HTTPS or loopback (https_required otherwise, as
// the password and code routes do). Like invite redemption they spend a per-address token before the body is
// read, and the server-wide redemption token before the store is asked, so
// they share #437's buckets: a flood of guests costs the same as a flood of
// invite redemptions. Nothing here logs, audits or echoes a code, a label or
// a token, and the client address stays in memory (clientaddr.go).
//
// No route here makes an account: open_joining account and both, and invite
// join_as account and both, admit no new account route until passwords
// exist (#707, #711). The front door says so by never offering one.

// DefaultServerName is the server's label when the operator names none
// (--server-name): what the front door shows, and a name no guest label may
// take. Reserving it as a permanent username too is #760.
const DefaultServerName = "conch"

// MaxServerNameLength bounds --server-name, in characters.
const MaxServerNameLength = 64

// joinSettingsBodyMaxBytes bounds PUT /v1/join/settings: MaxOpenChannels
// names of the longest channel name, escaped, fit with room to spare.
const joinSettingsBodyMaxBytes = 64 << 10

// ValidateServerName reports whether name is acceptable as --server-name:
// what the schema allows a display name, at most MaxServerNameLength
// characters.
func ValidateServerName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("server name must not be blank")
	}
	return schema.ValidateDisplayName("server name", name, MaxServerNameLength)
}

// serverName is the label the front door shows.
func (s *Server) serverName() string {
	if s.cfg.ServerName != "" {
		return s.cfg.ServerName
	}
	return DefaultServerName
}

// guestAddressBook remembers which address each visit this process admitted
// came from, for the per-address cap (review F13). It holds the address in
// limitKey's form, in memory only, like the rate limiter: a restart forgets
// it, so a restart lets each address start its cap afresh, the same trade
// the buckets make. The server-wide cap is in the database and is not
// affected.
type guestAddressBook struct {
	// admit is held across the per-address count and the commit, so two
	// admissions from one address cannot both count the place the other is
	// taking.
	admit sync.Mutex
	mu    sync.Mutex
	byKey map[string]map[int64]struct{}
	keyOf map[int64]string
}

// peers returns the visits admitted from key.
func (b *guestAddressBook) peers(key string) []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]int64, 0, len(b.byKey[key]))
	for id := range b.byKey[key] {
		out = append(out, id)
	}
	return out
}

func (b *guestAddressBook) add(key string, id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byKey == nil {
		b.byKey, b.keyOf = map[string]map[int64]struct{}{}, map[int64]string{}
	}
	if b.byKey[key] == nil {
		b.byKey[key] = map[int64]struct{}{}
	}
	b.byKey[key][id] = struct{}{}
	b.keyOf[id] = key
}

// forget drops a visit that ended.
func (b *guestAddressBook) forget(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key, ok := b.keyOf[id]
	if !ok {
		return
	}
	delete(b.keyOf, id)
	delete(b.byKey[key], id)
	if len(b.byKey[key]) == 0 {
		delete(b.byKey, key)
	}
}

// soleAddress returns the visits of the one address every remembered visit
// came from, and false when they came from more than one, or there are none.
func (b *guestAddressBook) soleAddress() ([]int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.byKey) != 1 {
		return nil, false
	}
	for _, ids := range b.byKey {
		out := make([]int64, 0, len(ids))
		for id := range ids {
			out = append(out, id)
		}
		return out, true
	}
	return nil, false
}

// admitGuest starts a visit from the address key with admit, which the store
// runs in one transaction under every cap: the per-address cap counts the
// live visits among the peers it is given.
func (s *Server) admitGuest(key string, admit func(peers []int64) (store.AdmittedGuest, error)) (store.AdmittedGuest, error) {
	s.guestAddrs.admit.Lock()
	defer s.guestAddrs.admit.Unlock()
	g, err := admit(s.guestAddrs.peers(key))
	if err == nil {
		s.guestAddrs.add(key, g.Principal.ID)
	}
	return g, err
}

func writeGuestsDisabled(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeGuestsDisabled, "guests cannot join here now")
}

// writeGuestsFull is 429 with no Retry-After: visits end on their own, at no
// time the server can promise.
func writeGuestsFull(w http.ResponseWriter) {
	writeError(w, http.StatusTooManyRequests, schema.ErrorCodeGuestsFull, "no more guests can join right now; try again later")
}

// handleJoinInfo serves GET /v1/join, exempt from authentication: the bare
// address's front door. It says what the address offers now, the server's
// label and the bounds a visit would have, and nothing else: no channel, no
// person, no count. It spends a preview token, as an invite preview does.
func (s *Server) handleJoinInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(false, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	set, live, err := s.store.JoinSettings(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "join: read the joining policy failed", "error", err)
		writeInternalError(w)
		return
	}
	info := schema.JoinInfoV1{
		ServerName: s.serverName(),
		// Password sign-in is #707's; until it is here, nobody signs in by
		// password on this server.
		PasswordLogin:        false,
		GuestIdleSeconds:     set.GuestIdleSeconds,
		GuestAbsoluteSeconds: set.GuestAbsoluteSeconds,
	}
	// With authentication off there is no identity to join as (design
	// §7.4): the address offers nothing and no voice.
	if s.cfg.authRequired() {
		addrLive, err := s.store.LiveGuestsAmong(ctx, s.guestAddrs.peers(limitKey(addr)))
		if err != nil {
			slog.ErrorContext(ctx, "join: count this address's visits failed", "error", err)
			writeInternalError(w)
			return
		}
		info.Join = store.AddressJoinOptions(set, len(set.OpenChannels), live, addrLive)
		info.Voice = set.JoinVoice
	}
	writeJSON(w, http.StatusOK, info)
}

// handleJoinGuest serves POST /v1/join/guest, exempt from authentication:
// Join as guest from the bare address. The order is redemption's: the
// per-address limit before the body is read, a bad label a 400, the
// server-wide limit last and given back when the store refuses. The store
// decides the rest in the transaction that starts the visit, so a stale
// front door admits nobody the policy now refuses.
func (s *Server) handleJoinGuest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.cfg.authRequired() {
		writeAccountsRequireAuth(w)
		return
	}
	// The request takes no code, but its answer is a new bearer token, and
	// sending that over plain HTTP beyond loopback is the exposure the
	// password and code routes refuse (design §5.4, §7.2): so is this.
	if !secureTransport(r, s.cfg.TrustedProxies) {
		writeGuestHTTPSRequired(w)
		return
	}
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(true, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.JoinGuestRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		writeGuestLabelInvalid(w)
		return
	}
	if limit, ok, retry := s.inviteLimits.takeGlobal(s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	g, err := s.admitGuest(limitKey(addr), func(peers []int64) (store.AdmittedGuest, error) {
		return s.store.JoinAsGuest(ctx, store.GuestEntry{Label: req.Label, ServerName: s.serverName(), AddressPeers: peers})
	})
	if err != nil {
		s.inviteLimits.refundGlobal(s.now())
	}
	switch {
	case errors.Is(err, store.ErrGuestsDisabled):
		writeGuestsDisabled(w)
	case errors.Is(err, store.ErrJoiningPaused):
		writeError(w, http.StatusForbidden, schema.ErrorCodeJoiningPaused, "joining is paused on this server")
	case errors.Is(err, store.ErrGuestsFull):
		writeGuestsFull(w)
	case errors.Is(err, store.ErrGuestLabelInvalid):
		writeGuestLabelInvalid(w)
	case err != nil:
		slog.ErrorContext(ctx, "join: guest visit failed", "error", err)
		writeInternalError(w)
	default:
		s.writeGuestJoined(ctx, w, g)
	}
}

// handleRedeemInviteGuest serves POST /v1/invites/redeem-guest, exempt from
// authentication: Join as guest through an invite. It is redemption's order
// and limits: a malformed code is the same 404 as any invalid one, a bad
// label a 400 that says nothing about the code.
func (s *Server) handleRedeemInviteGuest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.cfg.authRequired() {
		writeAccountsRequireAuth(w)
		return
	}
	// It takes a code, as every anonymous code route does, over HTTPS or
	// loopback only (design §7.2), and its answer is a bearer token.
	if !secureTransport(r, s.cfg.TrustedProxies) {
		writeGuestHTTPSRequired(w)
		return
	}
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(true, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.RedeemInviteGuestRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if schema.ValidateInviteCode(req.Code) != nil {
		writeInviteInvalid(w)
		return
	}
	if req.Validate() != nil {
		writeGuestLabelInvalid(w)
		return
	}
	if limit, ok, retry := s.inviteLimits.takeGlobal(s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	g, err := s.admitGuest(limitKey(addr), func(peers []int64) (store.AdmittedGuest, error) {
		return s.store.RedeemInviteGuest(ctx, store.HashInviteCode(req.Code),
			store.GuestEntry{Label: req.Label, ServerName: s.serverName(), AddressPeers: peers}, s.now())
	})
	if err != nil {
		s.inviteLimits.refundGlobal(s.now())
	}
	switch {
	case errors.Is(err, store.ErrInviteInvalid):
		s.auditRedeemRefused(r, schema.ErrorCodeInviteInvalid)
		writeInviteInvalid(w)
	case errors.Is(err, store.ErrInvitesPaused):
		writeInvitesPaused(w)
	case errors.Is(err, store.ErrGuestsDisabled):
		writeGuestsDisabled(w)
	case errors.Is(err, store.ErrGuestsFull):
		writeGuestsFull(w)
	case errors.Is(err, store.ErrGuestLabelInvalid):
		writeGuestLabelInvalid(w)
	case err != nil:
		slog.ErrorContext(ctx, "join: guest visit through an invite failed", "error", err)
		writeInternalError(w)
	default:
		s.writeGuestJoined(ctx, w, g)
	}
}

func writeGuestHTTPSRequired(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, schema.ErrorCodeHTTPSRequired,
		"a guest visit starts only over HTTPS; behind a reverse proxy, name it with --trusted-proxies")
}

func writeGuestLabelInvalid(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, "invalid_request", "the label is not acceptable")
}

// writeGuestJoined answers a visit that started: the principal, the visit,
// its one credential and its token, no-store, and the channels it joined.
// Joining starts no voice: the guest asks for a voice session as anyone
// does, so no microphone opens as a side effect (design §3.1).
func (s *Server) writeGuestJoined(ctx context.Context, w http.ResponseWriter, g store.AdmittedGuest) {
	for _, ch := range g.Channels {
		s.events.notifyChannelRoster(ch.ID)
	}
	slog.InfoContext(ctx, "join: guest visit started", "principal", g.Principal.ID, "origin", string(g.Visit.Origin))
	writeSecretJSON(w, http.StatusCreated, schema.JoinGuestResponseV1{
		Principal:  principalFromStore(g.Principal),
		Guest:      g.Visit.V1(),
		Credential: g.Credential,
		Token:      g.Token,
		Channels:   g.Channels,
	})
}

// operatorGuestCaller is the check the operator guest routes start with:
// with authentication off there is no guest and no operator (design §7.4).
// The route table has already refused anyone who is not an operator.
func (s *Server) operatorGuestCaller(w http.ResponseWriter, r *http.Request) (store.Principal, bool) {
	if !s.cfg.authRequired() {
		writeAccountsRequireAuth(w)
		return store.Principal{}, false
	}
	caller, ok := callerFrom(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return store.Principal{}, false
	}
	return caller, true
}

// joinSettingsResponse is the answer of both settings routes.
func (s *Server) joinSettingsResponse(ctx context.Context, st store.JoinState) schema.JoinSettingsResponseV1 {
	out := schema.JoinSettingsResponseV1{Settings: st.Settings, ServerName: s.serverName(), LiveGuests: int64(st.LiveGuests),
		Warnings: []schema.JoinWarning{}}
	if !st.Settings.GuestsEnabled && st.LiveGuests > 0 {
		out.Warnings = append(out.Warnings, schema.JoinWarningLiveGuestsRemain)
	}
	if st.Settings.GuestsEnabled && len(s.cfg.TrustedProxies) == 0 {
		out.Warnings = append(out.Warnings, schema.JoinWarningTrustedProxyUnset)
	}
	if st.LiveGuests >= 2 {
		if ids, ok := s.guestAddrs.soleAddress(); ok {
			if n, err := s.store.LiveGuestsAmong(ctx, ids); err == nil && n == st.LiveGuests {
				out.Warnings = append(out.Warnings, schema.JoinWarningOneAddress)
			}
		}
	}
	if st.ReservedNames > 0 {
		out.Warnings = append(out.Warnings, schema.JoinWarningReservedNamesInUse)
	}
	return out
}

// handleGetJoinSettings serves GET /v1/join/settings: the joining policy,
// the live visits and the warnings that apply.
func (s *Server) handleGetJoinSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.operatorGuestCaller(w, r); !ok {
		return
	}
	st, err := s.store.JoinState(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "join: read settings failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, s.joinSettingsResponse(ctx, st))
}

// handlePutJoinSettings serves PUT /v1/join/settings: an operator changes
// the joining policy. The store applies, checks and audits it in one
// transaction. Turning guests off ends no visit; the answer says how many
// remain and warns, and End all guest visits ends them.
func (s *Server) handlePutJoinSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.operatorGuestCaller(w, r)
	if !ok {
		return
	}
	var req schema.PutJoinSettingsRequestV1
	if !decodeBoundedBody(w, r, &req, joinSettingsBodyMaxBytes) {
		return
	}
	st, err := s.store.PutJoinSettings(ctx, auditActor(ctx), caller.ID, req)
	var refused *store.JoinSettingsError
	switch {
	case errors.As(err, &refused) && errors.Is(err, store.ErrJoinPolicyConflict):
		writeError(w, http.StatusConflict, schema.ErrorCodeJoinPolicyConflict, refused.Reason)
	case errors.As(err, &refused):
		writeError(w, http.StatusBadRequest, "invalid_request", refused.Reason)
	case err != nil:
		slog.ErrorContext(ctx, "join: write settings failed", "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "join: settings changed", "principal", caller.ID,
			"guests_enabled", st.Settings.GuestsEnabled, "open_joining", string(st.Settings.OpenJoining), "live_guests", st.LiveGuests)
		writeJSON(w, http.StatusOK, s.joinSettingsResponse(ctx, st))
	}
}

// handleListGuests serves GET /v1/guests: one page of visits, newest first,
// by ?state= (default active), ?origin=, ?before= and ?limit=.
func (s *Server) handleListGuests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.operatorGuestCaller(w, r); !ok {
		return
	}
	q := r.URL.Query()
	f := store.GuestFilter{State: schema.GuestStateActive, Limit: schema.DefaultListGuestsLimit}
	if v := q.Get("state"); v != "" {
		if !schema.GuestState(v).Valid() {
			writeError(w, http.StatusBadRequest, "invalid_request", "state must be one of: "+strings.Join(schema.Vocabularies["GuestState"], ", "))
			return
		}
		f.State = schema.GuestState(v)
	}
	if v := q.Get("origin"); v != "" {
		if !schema.GuestOrigin(v).Valid() {
			writeError(w, http.StatusBadRequest, "invalid_request", "origin must be one of: "+strings.Join(schema.Vocabularies["GuestOrigin"], ", "))
			return
		}
		f.Origin = schema.GuestOrigin(v)
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "before must be a positive integer")
			return
		}
		f.Before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > schema.MaxListGuestsLimit {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("limit must be between 1 and %d", schema.MaxListGuestsLimit))
			return
		}
		f.Limit = n
	}
	visits, more, err := s.store.ListGuests(ctx, f)
	if err != nil {
		slog.ErrorContext(ctx, "join: list guests failed", "error", err)
		writeInternalError(w)
		return
	}
	out := schema.ListGuestsResponseV1{Guests: make([]schema.GuestAdminV1, 0, len(visits))}
	for _, v := range visits {
		out.Guests = append(out.Guests, v.Admin())
	}
	if more && len(visits) > 0 {
		out.NextBefore = visits[len(visits)-1].PrincipalID
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEndGuest serves POST /v1/guests/{id}/end: an operator ends one
// visit, as End visit does, with reason operator. A principal that never
// was a guest is 404 not_a_guest; a visit already ended or converted is 409
// guest_ended.
func (s *Server) handleEndGuest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.operatorGuestCaller(w, r); !ok {
		return
	}
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	// The operator's client may go; the end must still finish.
	v, err := s.store.EndGuestVisit(context.WithoutCancel(ctx), auditActor(ctx), id, schema.GuestEndOperator)
	switch {
	case errors.Is(err, store.ErrNotAGuest):
		writeError(w, http.StatusNotFound, schema.ErrorCodeNotAGuest, "no guest visit has that id")
	case errors.Is(err, store.ErrGuestEnded):
		writeError(w, http.StatusConflict, schema.ErrorCodeGuestEnded, "the visit has already ended")
	case err != nil:
		slog.ErrorContext(ctx, "join: end guest failed", "principal", id, "error", err)
		writeInternalError(w)
	default:
		writeJSON(w, http.StatusOK, schema.EndGuestResponseV1{Guest: v.Admin()})
	}
}

// handleEndGuests serves POST /v1/guests/end: End all guest visits, or those
// of one origin or invite. dry_run is required: true lists whom it would
// end, false ends them, at most schema.MaxEndGuests a call, and says how
// many more there are. Converted members are never selected.
func (s *Server) handleEndGuests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.operatorGuestCaller(w, r); !ok {
		return
	}
	var req schema.EndGuestsRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), "schema: "))
		return
	}
	sel := store.GuestSelector{Origin: req.Origin, InviteID: req.InviteID}
	dry := *req.DryRun
	// Not cut short by a dropped connection: half of the visits ended, unseen,
	// is the outcome this route exists to avoid.
	refs, remaining, err := s.store.EndGuests(context.WithoutCancel(ctx), auditActor(ctx), sel, schema.MaxEndGuests, dry)
	if err != nil {
		slog.ErrorContext(ctx, "join: end guests failed", "dry_run", dry, "error", err)
		writeInternalError(w)
		return
	}
	out := schema.EndGuestsResponseV1{DryRun: dry, Count: len(refs), Guests: make([]schema.GuestRefV1, 0, len(refs)), Remaining: remaining}
	for _, g := range refs {
		out.Guests = append(out.Guests, schema.GuestRefV1{ID: g.ID, Handle: g.Handle})
	}
	if !dry {
		slog.InfoContext(ctx, "join: guest visits ended", "count", len(refs), "remaining", remaining)
	}
	writeJSON(w, http.StatusOK, out)
}
