package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Sign-in links on the REST API (issue #493). A person who signed out, or
// whose browser forgot them, has no password to come back with; an operator
// makes them a one-time link that signs them into the same account again.
// The store (store/sign_in_links.go) holds every rule of who may make and
// use one; these handlers add what only HTTP has, and they reuse what the
// invite routes already do (invites.go, invite_limits.go): the same body
// limit and decoder, the same rate limits and buckets, the same thinned
// audit of refusals, and one answer for every code that does not work.
//
// Preview and redeem are exempt from authentication. Each spends a
// rate-limit token before the body is read, and redeem a server-wide one
// before the store is asked. The code travels only in a POST body; the link
// carries it in the URL fragment, which no server sees. Nothing here logs,
// audits or echoes a code or the new credential's token, which appears only
// in the body of a successful redeem, sent no-store.

// signInLinkInvalidMessage is the one message of every code that does not
// work.
const signInLinkInvalidMessage = "this sign-in link is not valid"

// auditSignInLinkRedeemRefused records, thinned as invite_redeem_refused is,
// that a well-formed code signed nobody in.
const auditSignInLinkRedeemRefused = "sign_in_link_redeem_refused"

// writeSignInLinkInvalid writes the single answer for a code that is
// malformed, unknown, expired, used or closed, or for a person who may no
// longer be signed in by link, so the answer is not an oracle for which.
func writeSignInLinkInvalid(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid, signInLinkInvalidMessage)
}

// handleCreateSignInLink serves POST /v1/principals/{id}/sign-in-links: an
// operator makes a one-time sign-in link for a member. The route table lets
// only operators in; with authentication off there is no operator to make
// it, and the answer is the 401 of the invite routes. The code is in the 201
// body and nowhere else, sent no-store.
func (s *Server) handleCreateSignInLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.inviteCaller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	// Making one is limited per operator, in the bucket invites use.
	if limit, ok, retry := s.inviteLimits.takeCreate(caller.ID, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, caller.ID, clientAddr(r, s.cfg.TrustedProxies))
		return
	}
	var req schema.CreateSignInLinkRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	now := s.now()
	// The schema's messages name the rule broken, never the value sent.
	if err := req.Validate(now); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	resp, err := s.store.CreateSignInLink(ctx, auditActor(ctx), caller.ID, id, req, now)
	switch {
	case errors.Is(err, store.ErrSignInLinkRequestInvalid):
		// Validated above; the clock moved past a just-valid expiry.
		writeError(w, http.StatusBadRequest, "invalid_request", "the sign-in link request is not valid")
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
	case errors.Is(err, store.ErrSignInLinkTarget):
		writeError(w, http.StatusBadRequest, "invalid_request", signInLinkTargetMessage(err))
	case errors.Is(err, store.ErrPrincipalHidden):
		// A hidden principal is always disabled: the code is the disabled one.
		writeError(w, http.StatusConflict, "principal_disabled", "principal is hidden; enable it before making a sign-in link")
	case errors.Is(err, store.ErrPrincipalDisabled):
		writeError(w, http.StatusConflict, "principal_disabled", "principal is disabled; enable it before making a sign-in link")
	case errors.Is(err, store.ErrSignInLinkNotAllowed):
		// The route table admits operators only; the caller stopped being
		// one a moment ago. Fail closed.
		s.denyForbidden(w, r, caller, "forbidden", "only an operator makes a sign-in link")
	case err != nil:
		slog.ErrorContext(ctx, "sign-in links: create failed", "principal", id, "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "sign-in links: created", "operator", caller.ID, "principal", id, "sign_in_link", resp.SignInLink.ID)
		writeSecretJSON(w, http.StatusCreated, resp)
	}
}

// signInLinkTargetMessage is the store's reason a principal cannot have a
// link, without its prefix. It names a rule, never a value sent.
func signInLinkTargetMessage(err error) string {
	msg := err.Error()
	if _, reason, ok := strings.Cut(msg, store.ErrSignInLinkTarget.Error()+": "); ok {
		return "a sign-in link is for a member: " + reason
	}
	return "a sign-in link is for a member"
}

// handleCloseSignInLink serves DELETE /v1/sign-in-links/{id}: an operator
// closes a link that leaked, without disabling the person it is for. The
// route table lets only operators in; with authentication off the answer is
// the 401 of the other operator routes here. A link already closed or used
// is a 204 that changes nothing; an unknown id is a 404.
func (s *Server) handleCloseSignInLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := s.inviteCaller(w, r); !ok {
		return
	}
	id, ok := pathID(w, r, "id", "sign-in link")
	if !ok {
		return
	}
	_, err := s.store.CloseSignInLink(ctx, auditActor(ctx), id, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, schema.ErrorCodeSignInLinkNotFound, "sign-in link not found")
	case err != nil:
		slog.ErrorContext(ctx, "sign-in links: close failed", "sign_in_link", id, "error", err)
		writeInternalError(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePreviewSignInLink serves POST /v1/sign-in-links/preview, exempt from
// authentication: the name of the account the link opens, without using it.
func (s *Server) handlePreviewSignInLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(false, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.SignInLinkPreviewRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		writeSignInLinkInvalid(w)
		return
	}
	preview, err := s.store.PreviewSignInLink(ctx, store.HashSignInLinkCode(req.Code), s.now())
	switch {
	case errors.Is(err, store.ErrSignInLinkInvalid):
		writeSignInLinkInvalid(w)
	case err != nil:
		slog.ErrorContext(ctx, "sign-in links: preview failed", "error", err)
		writeInternalError(w)
	default:
		// Not a secret, but it answers a secret: no cache keeps it.
		writeSecretJSON(w, http.StatusOK, preview)
	}
}

// handleRedeemSignInLink serves POST /v1/sign-in-links/redeem, exempt from
// authentication: it uses the link and returns a new credential for the
// person it was made for. The order is redeeming an invite's: the
// per-address limit before the body is read; a malformed code the same 404
// as any invalid one; the server-wide limit last, so only a use that reaches
// the store can spend it, and one that fails there gives it back.
func (s *Server) handleRedeemSignInLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.inviteLimits.takeAddress(true, addr, s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	var req schema.RedeemSignInLinkRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		writeSignInLinkInvalid(w)
		return
	}
	if limit, ok, retry := s.inviteLimits.takeGlobal(s.now()); !ok {
		s.refuseInviteRateLimited(w, r, limit, retry, 0, addr)
		return
	}
	res, err := s.store.RedeemSignInLink(ctx, store.HashSignInLinkCode(req.Code), s.now())
	if err != nil {
		s.inviteLimits.refundGlobal(s.now())
	}
	switch {
	case errors.Is(err, store.ErrSignInLinkInvalid):
		s.auditCodeRefused(r, auditSignInLinkRedeemRefused, schema.ErrorCodeSignInLinkInvalid)
		writeSignInLinkInvalid(w)
	case err != nil:
		// The store's errors carry no code, hash or token.
		slog.ErrorContext(ctx, "sign-in links: redeem failed", "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "sign-in links: redeemed", "principal", res.Principal.ID, "credential", res.Credential.ID)
		writeSecretJSON(w, http.StatusCreated, schema.RedeemSignInLinkResponseV1{
			Principal:  principalFromStore(res.Principal),
			Credential: res.Credential,
			Token:      res.Token,
		})
	}
}
