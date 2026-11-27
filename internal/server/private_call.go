package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

var privateCallToken = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// All four action routes share a per-principal bucket. A burst of 10 and
// one token per second leave room for retries and bound refused writes.
const (
	privateCallActionBurst  = 10
	privateCallActionRefill = time.Second
)

// privateCallActor reads the identity admitted by requireDMCredential.
// Under AuthOff there is no caller or credential to act on a call.
func privateCallActor(w http.ResponseWriter, r *http.Request) (store.PrivateCallActor, bool) {
	w.Header().Set("Cache-Control", "no-store")
	p, ok := callerFrom(r.Context())
	credentialID, authenticated := credentialIDFrom(r.Context())
	if !ok || !authenticated {
		writeDMRefusal(w, http.StatusBadRequest, schema.ErrorCodeDMRequireAuth)
		return store.PrivateCallActor{}, false
	}
	return store.PrivateCallActor{PrincipalID: p.ID, CredentialID: credentialID}, true
}

func privateCallConversationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("conversation"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid conversation id")
		return 0, false
	}
	return id, true
}

func (s *Server) privateCallInviteHandler() http.Handler {
	limit := newChannelWriteLimiter(inviteLimit{"private_call_invites", 3, time.Minute})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleInvitePrivateCall(w, r, limit)
	})
}

func (s *Server) handleInvitePrivateCall(w http.ResponseWriter, r *http.Request, limit *channelWriteLimiter) {
	actor, ok := privateCallActor(w, r)
	if !ok {
		return
	}
	if allowed, retry := limit.take(actor.PrincipalID, 0, s.now()); !allowed {
		writeRateLimited(w, &rateLimitedError{retryAfter: retry})
		return
	}
	conversationID, ok := privateCallConversationID(w, r)
	if !ok {
		return
	}
	var req schema.InvitePrivateCallRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil || req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid call invite")
		return
	}
	out, err := s.store.InvitePrivateCall(r.Context(), conversationID, actor, req, s.now)
	writePrivateCallResult(w, r, out, err)
}

func (s *Server) privateCallAction(event string, limit *channelWriteLimiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := privateCallActor(w, r)
		if !ok {
			return
		}
		if allowed, retry := limit.take(actor.PrincipalID, 0, s.now()); !allowed {
			writeRateLimited(w, &rateLimitedError{retryAfter: retry})
			return
		}
		id := r.PathValue("call")
		if len(id) < schema.MinPrivateCallIDBytes || len(id) > schema.MaxPrivateCallIDBytes || !privateCallToken.MatchString(id) {
			writePrivateCallResult(w, r, store.PrivateCallResult{}, store.ErrPrivateCallNotFound)
			return
		}
		var req schema.PrivateCallActionRequestV1
		if err := decodeJSONBody(w, r, &req); err != nil || req.Validate() != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid call action")
			return
		}
		out, err := s.store.ActOnPrivateCall(r.Context(), id, event, actor, req, s.now)
		writePrivateCallResult(w, r, out, err)
	})
}

func (s *Server) handleCurrentPrivateCall(w http.ResponseWriter, r *http.Request) {
	actor, ok := privateCallActor(w, r)
	if !ok {
		return
	}
	out, err := s.store.CurrentPrivateCall(r.Context(), actor, s.now)
	if err != nil {
		writePrivateCallResult(w, r, store.PrivateCallResult{}, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListPrivateCalls(w http.ResponseWriter, r *http.Request) {
	actor, ok := privateCallActor(w, r)
	if !ok {
		return
	}
	conversationID, ok := privateCallConversationID(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if cursor != "" && (len(cursor) > schema.MaxDMCursorBytes || !privateCallToken.MatchString(cursor)) {
		writePrivateCallResult(w, r, store.PrivateCallResult{}, store.ErrPrivateCallCursor)
		return
	}
	out, err := s.store.ListPrivateCalls(r.Context(), conversationID, actor, cursor, s.now())
	if err != nil {
		writePrivateCallResult(w, r, store.PrivateCallResult{}, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writePrivateCallResult(w http.ResponseWriter, r *http.Request, out store.PrivateCallResult, err error) {
	switch {
	case errors.Is(err, store.ErrPrivateCallPair):
		writeDMRefusal(w, http.StatusNotFound, schema.ErrorCodeDMNotFound)
	case errors.Is(err, store.ErrDMsOff):
		writeDMRefusal(w, http.StatusForbidden, schema.ErrorCodeDMsOff)
	case errors.Is(err, store.ErrPrivateCallNotFound):
		writeError(w, http.StatusNotFound, schema.ErrorCodeCallNotFound, "call not found")
	case errors.Is(err, store.ErrPrivateCallActor):
		writeError(w, http.StatusForbidden, errForbidden.Code, "action is not allowed for your side of the call")
	case errors.Is(err, store.ErrPrivateCallCursor):
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid call history cursor")
	case err != nil:
		slog.ErrorContext(r.Context(), "private call request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	default:
		writePrivateCallCommand(w, out)
	}
}

func writePrivateCallCommand(w http.ResponseWriter, out store.PrivateCallResult) {
	switch out.Code {
	case schema.ErrorCodeRateLimited:
		writeRateLimited(w, &rateLimitedError{retryAfter: out.RetryAfter})
	case schema.ErrorCodeCallStateChanged:
		writeJSON(w, http.StatusConflict, schema.PrivateCallConflictV1{
			Code: out.Code, Message: "call state changed", Call: out.Call,
		})
	case schema.ErrorCodeCallActionConflict:
		writeError(w, http.StatusConflict, out.Code, "action id already used for a different call command")
	case schema.ErrorCodeCallInProgress:
		writeError(w, http.StatusConflict, out.Code, "you already have a call in progress")
	case schema.ErrorCodeVoiceNotAllowed:
		writeError(w, http.StatusForbidden, out.Code, "voice is not part of your access to this server")
	default:
		status := http.StatusOK
		if out.Created && out.Call.State == schema.PrivateCallStateRinging {
			status = http.StatusCreated
		}
		// #2031 can publish committed transitions, using attempt IDs and
		// revisions to deduplicate. Replays must never ring the callee again.
		writeJSON(w, status, out.PrivateCallResponseV1)
	}
}

func (s *Server) startPrivateCallSweeper(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.store.SweepPrivateCalls(ctx, s.now); err != nil && ctx.Err() == nil {
					slog.ErrorContext(ctx, "private call sweep failed", "error", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
