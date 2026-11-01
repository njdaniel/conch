package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const hookTokenBytes = 32

func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req schema.CreateHookRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if strings.TrimSpace(req.Channel) == "" || req.Principal <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel must not be empty and principal must be positive")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	caller, authed := callerFrom(ctx)
	channel, err := s.store.ChannelByName(ctx, req.Channel)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "hooks: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// Without manage_channels a hook goes only into a channel the caller is
	// in (ADR-014 design §7.3), and a name that is no channel gets the
	// answer a channel it is not in gets, so the route is not an oracle on
	// private channels' names (the security review of #1015).
	if authed {
		all, ok := s.canOrFail(w, r, caller, schema.PermManageChannels)
		if !ok {
			return
		}
		if !all {
			in := false
			if err == nil {
				if in, err = s.store.IsChannelMember(ctx, channel.ID, caller.ID); err != nil {
					slog.ErrorContext(ctx, "hooks: check channel membership failed", "error", err)
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
					return
				}
			}
			if !in {
				s.denyForbiddenFor(w, r, caller, schema.ErrorCodePermissionDenied,
					"a hook into a channel you are not in needs the manage_channels permission",
					refusedPermission(caller, schema.PermManageChannels))
				return
			}
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "channel_not_found", "channel not found")
		return
	}
	principal, err := s.store.PrincipalByID(ctx, req.Principal)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "principal_not_found", "principal not found")
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "hooks: find principal failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	forbidOwner := authed && !isOwner(caller)
	if forbidOwner && principal.IsOwner() {
		s.denyForbidden(w, r, caller, "forbidden", "only an owner may act on an owner")
		return
	}
	// A hook is a posting credential for its principal, so a disabled
	// principal cannot be given one (issue #101).
	if principal.DisabledAt != nil {
		writeError(w, http.StatusConflict, "principal_disabled", "principal is disabled")
		return
	}
	tokenBytes := make([]byte, hookTokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		slog.ErrorContext(ctx, "hooks: generate token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	hook, err := s.store.CreateHookWithLabel(ctx, auditActor(ctx), token, req.Label, channel.ID, req.Principal, forbidOwner)
	if errors.Is(err, store.ErrOwnerProtected) {
		s.denyForbidden(w, r, caller, "forbidden", "only an owner may act on an owner")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "hooks: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// The one place the token is ever returned; it must not be cached.
	writeSecretJSON(w, http.StatusCreated, schema.CreateHookResponse{ID: hook.ID, Token: token})
}

func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, authed := callerFrom(ctx)
	hooks, err := s.store.ListHooks(ctx, authed && !isOwner(caller))
	if err != nil {
		slog.ErrorContext(ctx, "hooks: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	resp := schema.ListHooksResponseV1{Hooks: hooks}
	if authed {
		// Without manage_channels, a hook posting in a channel the caller is
		// not in is left out and counted (ADR-014 design §7.3, SF-1).
		sight, err := s.channelSightFor(ctx, caller)
		if err != nil {
			slog.ErrorContext(ctx, "hooks: read channel sight failed", "principal", caller.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		resp = hooksAsSeen(resp, sight)
	}
	writeJSON(w, http.StatusOK, resp)
}

// hooksAsSeen is resp with only the hooks in channels sight shows; the rest
// are counted in OtherHooks.
func hooksAsSeen(resp schema.ListHooksResponseV1, sight channelSight) schema.ListHooksResponseV1 {
	if sight.all {
		return resp
	}
	shown := make([]schema.HookV1, 0, len(resp.Hooks))
	for _, h := range resp.Hooks {
		if sight.seesName(h.Channel) {
			shown = append(shown, h)
			continue
		}
		resp.OtherHooks++
	}
	resp.Hooks = shown
	return resp
}

// handleRevokeHook revokes a hook by its numeric id (never its token), the way
// credentials are revoked. Revoking an already-revoked hook succeeds.
func (s *Server) handleRevokeHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "hook")
	if !ok {
		return
	}
	caller, authed := callerFrom(ctx)
	err := s.store.RevokeHook(ctx, auditActor(ctx), id, authed && !isOwner(caller))
	switch {
	case errors.Is(err, store.ErrHookNotFound):
		writeError(w, http.StatusNotFound, "hook_not_found", "hook not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "hooks: revoke failed", "hook", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleIngestHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hook, err := s.store.HookByToken(ctx, r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "hook_not_found", "hook not found")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "hooks: resolve failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// A hook posts as its principal. Disabling the principal switches it off
	// everywhere, in every auth mode, so its hooks stop too and answer exactly
	// like an unknown token (issue #101). Enabling the principal brings them
	// back.
	hookPrincipal, err := s.store.PrincipalByID(ctx, hook.PrincipalID)
	if err != nil {
		slog.ErrorContext(ctx, "hooks: find principal failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if hookPrincipal.DisabledAt != nil {
		writeError(w, http.StatusNotFound, "hook_not_found", "hook not found")
		return
	}
	// A hook posts as its principal, so it inherits that principal's channel
	// membership (issue #90): once the principal is removed from the channel,
	// or if it was never a member, the hook stops working and answers exactly
	// like an unknown token. As with every membership check, this applies only
	// when authentication is required.
	if s.cfg.authRequired() {
		member, err := s.store.IsChannelMember(ctx, hook.ChannelID, hook.PrincipalID)
		if err != nil {
			slog.ErrorContext(ctx, "hooks: check membership failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		if !member {
			if hookPrincipal.Kind == store.PrincipalAgent {
				s.auditAgentDenial(ctx, hookPrincipal.ID, r.Pattern, schema.CapabilityMessagesPost, hook.ChannelID, denyNotMember)
			}
			writeError(w, http.StatusNotFound, "hook_not_found", "hook not found")
			return
		}
		// A hook bound to an agent posts as that agent, so it is also held to
		// the agent's manifest (issue #79). A refusal is audited and looks
		// like an unknown token to the sender.
		if hookPrincipal.Kind == store.PrincipalAgent {
			m, allowed, err := s.agentManifestCheck(ctx, hookPrincipal.ID, r.Pattern, schema.CapabilityMessagesPost, hook.ChannelID, schema.ChannelPermissionPost)
			if err != nil {
				slog.ErrorContext(ctx, "hooks: load manifest failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				return
			}
			if !allowed {
				writeError(w, http.StatusNotFound, "hook_not_found", "hook not found")
				return
			}
			// A hook is a posting credential for its agent, so a post through
			// it spends the agent's messages.post budget like any other
			// (issue #80): otherwise a hook would be the way round the limit.
			// Unlike the refusals above this one is not disguised: only a
			// sender holding a working token can reach it, and 429 with
			// Retry-After is what a webhook sender knows how to obey.
			if limited := s.spendAgentBudget(ctx, hookPrincipal.ID, r.Pattern, m, schema.CapabilityMessagesPost); limited != nil {
				writeRateLimited(w, limited)
				return
			}
		}
	}
	channel, err := s.store.ChannelByID(ctx, hook.ChannelID)
	if err != nil {
		slog.ErrorContext(ctx, "hooks: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}

	// Rendered text posts as-is; anything else must be exactly the v1 post
	// shape (schema-first, CLAUDE.md rule 2 — no ingest-only aliases). The
	// author is always the hook's principal, regardless of what was sent.
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var post schema.PostMessageRequestV1
	if mediaType == "text/plain" {
		// A stored body is always valid UTF-8: the JSON routes get that from
		// the decoder, which replaces each byte that is not with U+FFFD, and
		// rendered text must end up the same as if it had been sent that way.
		post = schema.PostMessageRequestV1{Body: validUTF8(string(body))}
	} else if err := decodeHookPost(body, &post); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a v1 post message object")
		return
	}
	// A hook posts as its principal without being that principal, so it may
	// not attach files (issue #23): its holder could otherwise publish, and
	// then read, an upload only the principal could see. postMessage refuses
	// the same payload for any post without a verified caller; this route is
	// the one that asserts an author and passes on a payload, so it says no
	// itself rather than rely on where it happens to hand the request.
	if post.Payload != nil && post.Payload.Schema == schema.FileAttachmentV1Name {
		writeError(w, http.StatusBadRequest, errFilesNeedAuth.Code, "a webhook cannot attach files")
		return
	}
	post.AuthorID = hook.PrincipalID
	// The post is stored from the decoded value, not re-encoded into the
	// request body for handlePostMessageV1: json.Marshal writes each <, > and
	// & as a six-byte escape, so a body inside the limit could be measured
	// again over it (issue #238). The limit has applied once, to what the
	// sender sent. This route has no verified caller, so there is no author
	// binding to do; the author is the hook's principal set above.
	if err := post.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	r.SetPathValue("channel", channel.Name)
	s.postMessage(w, r, post.AuthorID, post.Body, post.Payload, apiV1)
}

// validUTF8 returns s with each byte that is not part of valid UTF-8 replaced
// by U+FFFD, one replacement for each such byte: what encoding/json does when
// it decodes or encodes the same bytes. (strings.ToValidUTF8 replaces a whole
// run of them with one, which would make two stray bytes read as one.)
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return string([]rune(s))
}

func decodeHookPost(body []byte, post *schema.PostMessageRequestV1) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(post); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("server: hook body must contain one JSON object")
	}
	return nil
}
