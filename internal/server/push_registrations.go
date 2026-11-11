package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/njdaniel/conch/internal/server/push"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Push registrations (issue #1714, Mobile M11-B;
// docs/design/mobile-notifications.md §3.6, §5, §8). A phone gives the server
// an address it may wake. This slice sends nothing: its only outbound traffic
// is the DNS lookup of the endpoint's address check.
//
//	GET    /v1/push/config
//	POST   /v1/push/registrations
//	GET    /v1/push/registrations/mine
//	PUT    /v1/push/registrations/{id}
//	DELETE /v1/push/registrations/{id}
//
// Everything but the config is for a person with a full credential, as the
// notification routes are: an agent is refused with 403 human_required, a
// guest with 403 guest_not_allowed (membersOnly), a setup-scope credential by
// the authentication layer, and none is previewable or in the admin catalog.
// The config alone is for any signed-in caller: nothing in it is secret.
//
// The registration is the calling credential's own. /mine lists that
// credential's, and PUT and DELETE act on one of its registrations by id; no
// route names another credential's, so an id of another's, an ended one and
// one never issued are the same 404, byte for byte (threat T13).
//
// The target (endpoint, p256dh, auth, provider, token) is write-only: it is
// stored and never read back by a route, and it is in no log line, no audit
// row and no error. The endpoint is a URL a member chose that conchd will one
// day post to, so it is the one thing here that is checked against the
// network, and the check's failure is a single fixed answer
// (pushEndpointRefusedMessage): not the URL, not the host, not an address, and
// not which way it failed, because under --push-webpush-allow-private the way
// it failed would tell a member what is listening on the LAN. Errors from the
// check are never put in a log: only a push.Classify output may be.

// pushEndpointRefusedMessage is the whole of the answer to an endpoint the
// server will not use, whatever the reason: it names neither the endpoint nor
// any part of it, and does not say which rule refused it.
const pushEndpointRefusedMessage = "the push endpoint is not acceptable to this server"

// PushConfig is conchd's push configuration (issue #1714).
type PushConfig struct {
	// WebPush turns the Web Push transport on (--push-webpush). Off, the
	// default, push is off and the routes answer as the design says.
	WebPush bool
	// AllowPrivate admits endpoints that resolve to private addresses, and
	// any port (--push-webpush-allow-private). It means nothing without
	// WebPush, and conchd refuses it given alone.
	AllowPrivate bool
	// VAPID is the server's application server key. WebPush is on only with
	// one: with none the transport is reported off and every registration is
	// refused, so a configuration that lost its key fails closed.
	VAPID *push.VAPIDKey
	// Resolver looks endpoint hosts up for the address check. Nil is the
	// system resolver; tests set a fake, so no test needs DNS.
	Resolver push.Resolver
}

// webPushOn reports whether the Web Push transport is on.
func (s *Server) webPushOn() bool { return s.cfg.Push.WebPush && s.cfg.Push.VAPID != nil }

// pushOn reports whether any push transport is on. The relay (#1716) is not
// built, so it is Web Push alone.
func (s *Server) pushOn() bool { return s.webPushOn() }

// pushCaller is the check every registration route but the config starts
// with: a verified human with a credential. With authentication off there is no
// credential to hold a registration, and the answer is the notification
// routes'.
func pushCaller(w http.ResponseWriter, r *http.Request) (store.Principal, int64, bool) {
	return notificationCaller(w, r)
}

// handleGetPushConfig serves GET /v1/push/config: which transports are on, and
// the application server key a Web Push subscription is made under. Any
// signed-in caller may read it. It is rate limited per credential like the
// other reads.
func (s *Server) handleGetPushConfig(w http.ResponseWriter, r *http.Request) {
	if credentialID, ok := credentialIDFrom(r.Context()); ok && !s.takeNotificationReadToken(w, credentialID) {
		return
	}
	cfg := schema.PushConfigV1{Transports: []schema.PushTransport{}}
	if s.webPushOn() {
		cfg.Transports = append(cfg.Transports, schema.PushTransportWebPush)
		cfg.VAPIDPublicKey = s.cfg.Push.VAPID.PublicKey()
	}
	writeJSON(w, http.StatusOK, cfg)
}

// pushTargetOf is the store's form of a registration request, with the
// padding of p256dh and auth dropped.
func pushTargetOf(transport schema.PushTransport, platform schema.PushPlatform, app, endpoint, p256dh, auth string, provider schema.PushProvider, token string) store.PushTarget {
	return store.PushTarget{
		Transport: transport, Platform: platform, App: app,
		Endpoint: endpoint, P256DH: p256dh, Auth: auth, Provider: provider, Token: token,
	}
}

// transportAvailable answers a registration request for a transport that is not
// on: push off altogether is 409 push_not_configured, and push on with this
// transport off (or the relay, which does not exist yet) is 409
// push_transport_unavailable. It reports whether the request may go on.
func (s *Server) transportAvailable(w http.ResponseWriter, transport schema.PushTransport) bool {
	switch {
	case !s.pushOn():
		writeError(w, http.StatusConflict, schema.ErrorCodePushNotConfigured, "push is not configured on this server")
	case transport == schema.PushTransportWebPush && s.webPushOn():
		return true
	default:
		// Includes relay, which is refused until #1716.
		writeError(w, http.StatusConflict, schema.ErrorCodePushTransportUnavailable, "this push transport is not available on this server")
	}
	return false
}

// checkPushEndpoint reports whether endpoint, which passed schema's form
// check, may be registered: its port, and then every address its host resolves
// to (push.Check, #1717), with the operator's allow-private switch. It answers
// nothing about why a refusal was one: the caller sends the one fixed message.
// It never returns, logs or wraps err.Error() of anything it called.
//
// Port: without --push-webpush-allow-private only 443 (the port an https URL
// has when it names none) is accepted. Every public push service speaks on
// 443: Google's, Mozilla's and Apple's, and a UnifiedPush distributor behind a
// public name does too. Any other port is a service a member picked on a public
// host, and conchd would be posting to it from the server's address:
// restricting the port narrows what a hostile endpoint can reach to the port a
// push service has. With the switch on, the operator has said their
// distributor lives on their own network, where it may be on any port, and any
// port but 0 is accepted.
func (s *Server) checkPushEndpoint(ctx context.Context, endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	port := 443
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
		port = n
	}
	if port != 443 && !s.cfg.Push.AllowPrivate {
		return false
	}
	_, err = push.Check(ctx, u.Hostname(), s.cfg.Push.Resolver, s.cfg.Push.AllowPrivate)
	return err == nil
}

// refusePushEndpoint answers the one refusal of an endpoint.
func refusePushEndpoint(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, schema.ErrorCodePushEndpointRefused, pushEndpointRefusedMessage)
}

// decodePushBody reads a JSON body into dst, answering a body that is not one
// without echoing any of it. It reports whether to go on.
func decodePushBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decodeJSONBody(w, r, dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return false
	}
	return true
}

// handleCreatePushRegistration serves POST /v1/push/registrations. The checks
// run in this order, and the first to fail answers: the body (400), push on
// (409 push_not_configured), the transport on (409
// push_transport_unavailable), the write rate limit (429), the endpoint and
// its addresses (400 push_endpoint_refused), and then the limits of
// registrations (409 too_many_push_registrations). The rate limit is spent
// before the lookup, so a loop cannot make the server resolve names for free.
func (s *Server) handleCreatePushRegistration(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credentialID, ok := pushCaller(w, r)
	if !ok {
		return
	}
	var req schema.CreatePushRegistrationRequestV1
	if !decodePushBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.transportAvailable(w, req.Transport) || !s.takeNotificationWriteToken(w, caller.ID) {
		return
	}
	req = req.Normalized()
	if !s.checkPushEndpoint(ctx, req.Endpoint) {
		refusePushEndpoint(w)
		return
	}
	reg, err := s.store.CreatePushRegistration(ctx, credentialID,
		pushTargetOf(req.Transport, req.Platform, req.App, req.Endpoint, req.P256DH, req.Auth, req.Provider, req.Token), s.now())
	s.answerPushRegistration(w, r, http.StatusCreated, reg, err, "create")
}

// answerPushRegistration writes the answer of a store call that returns a
// registration, or its refusal.
func (s *Server) answerPushRegistration(w http.ResponseWriter, r *http.Request, status int, reg schema.PushRegistrationV1, err error, op string) {
	switch {
	case errors.Is(err, store.ErrTooManyPushRegistrations):
		writeError(w, http.StatusConflict, schema.ErrorCodeTooManyPushRegistrations,
			"this sign-in or this account already holds as many push registrations as it may")
	case errors.Is(err, store.ErrPushRegistrationNotFound):
		writePushRegistrationNotFound(w)
	case errors.Is(err, store.ErrNotFound):
		// The credential was revoked between authentication and the write.
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
	case err != nil:
		credentialID, _ := credentialIDFrom(r.Context())
		slog.ErrorContext(r.Context(), "push: registration write failed", "op", op, "credential", credentialID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	default:
		writeJSON(w, status, reg)
	}
}

// writePushRegistrationNotFound is the one answer for an id that is not an
// active registration of the calling credential, whichever way it is not.
func writePushRegistrationNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, schema.ErrorCodePushRegistrationNotFound, "push registration not found")
}

// handleListOwnPushRegistrations serves GET /v1/push/registrations/mine: the
// calling credential's active registrations, with no target. It works with
// push off, and then is empty.
func (s *Server) handleListOwnPushRegistrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, credentialID, ok := pushCaller(w, r)
	if !ok || !s.takeNotificationReadToken(w, credentialID) {
		return
	}
	regs, err := s.store.ListOwnPushRegistrations(ctx, credentialID)
	if err != nil {
		slog.ErrorContext(ctx, "push: list registrations failed", "credential", credentialID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, schema.ListOwnPushRegistrationsResponseV1{Registrations: regs})
}

// handlePutPushRegistration serves PUT /v1/push/registrations/{id}: it
// replaces the target of the calling credential's registration, as a POST
// would have set it. The id-independent checks come first (the body, push on,
// the rate limit), so that none of them tells a caller whether an id exists;
// then the registration is looked up, which is the 404; and only then the
// checks that depend on it (its transport and platform, whether its transport
// is on) and on the endpoint.
func (s *Server) handlePutPushRegistration(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credentialID, ok := pushCaller(w, r)
	if !ok {
		return
	}
	var req schema.UpdatePushRegistrationRequestV1
	if !decodePushBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.pushOn() {
		writeError(w, http.StatusConflict, schema.ErrorCodePushNotConfigured, "push is not configured on this server")
		return
	}
	if !s.takeNotificationWriteToken(w, caller.ID) {
		return
	}
	id := r.PathValue("id")
	current, err := s.store.GetOwnPushRegistration(ctx, credentialID, id)
	if err != nil {
		s.answerPushRegistration(w, r, 0, schema.PushRegistrationV1{}, err, "update")
		return
	}
	if req.Transport != current.Transport || req.Platform != current.Platform {
		writeError(w, http.StatusBadRequest, "invalid_request", "a push registration's transport and platform cannot change; delete it and register again")
		return
	}
	if !s.transportAvailable(w, current.Transport) {
		return
	}
	req = req.Normalized()
	if !s.checkPushEndpoint(ctx, req.Endpoint) {
		refusePushEndpoint(w)
		return
	}
	reg, err := s.store.ReplacePushTarget(ctx, credentialID, id,
		pushTargetOf(req.Transport, req.Platform, req.App, req.Endpoint, req.P256DH, req.Auth, req.Provider, req.Token), s.now())
	s.answerPushRegistration(w, r, http.StatusOK, reg, err, "update")
}

// handleDeletePushRegistration serves DELETE /v1/push/registrations/{id}: the
// calling credential ends its own registration, with reason self. It works
// with push off, so a phone can clean up after the operator turned push off.
func (s *Server) handleDeletePushRegistration(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, credentialID, ok := pushCaller(w, r)
	if !ok || !s.takeNotificationWriteToken(w, caller.ID) {
		return
	}
	err := s.store.DeletePushRegistration(ctx, credentialID, r.PathValue("id"), s.now())
	if err != nil {
		s.answerPushRegistration(w, r, 0, schema.PushRegistrationV1{}, err, "delete")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
