package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// writeApprovalChannelNotFound is the single response for an approval aimed at
// a channel that does not exist and at one the caller is not a member of, so
// that membership is never revealed (issue #92).
func writeApprovalChannelNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, "channel_not_found", "channel not found")
}

// writeApprovalNotFound is the single response for an unknown approval id and
// for an approval in a channel the caller is not a member of.
func writeApprovalNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "approval_not_found", "approval not found")
}

// handleCreateApproval serves POST /v1/approvals: a principal raises a new
// approval, which is persisted pending with its approval_created audit event,
// notified, and armed with its deadline timer.
//
// With an authenticated caller (issue #92) the requester is the caller — a
// requester_id naming anyone else is refused — and the caller must be a member
// of the target channel. The audit actor is therefore the authenticated
// principal. With no caller (AuthOff) the body is trusted as before.
func (s *Server) handleCreateApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req schema.CreateApprovalRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	// Bind before validation: Validate rejects a zero requester_id, but an
	// authenticated caller may omit it.
	if !s.bindCallerID(w, r, &req.RequesterID, "requester_mismatch", "requester_id does not match the authenticated principal") {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	if _, err := s.store.PrincipalByID(ctx, req.RequesterID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "requester_not_found", "requester not found")
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "approvals: find requester failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if _, err := s.store.ChannelByID(ctx, req.ChannelID); errors.Is(err, store.ErrNotFound) {
		writeApprovalChannelNotFound(w)
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "approvals: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	// An approval renders in its channel, so raising one needs membership. A
	// non-member gets the unknown-channel answer.
	if member, err := s.callerIsMember(r, req.ChannelID); err != nil {
		slog.ErrorContext(ctx, "approvals: check membership failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	} else if !member {
		writeApprovalChannelNotFound(w)
		return
	}

	quorum := req.Quorum
	if quorum == 0 {
		quorum = 1
	}
	created, err := s.approvals.Create(ctx, store.ApprovalParams{
		RequesterID: req.RequesterID,
		ChannelID:   req.ChannelID,
		Title:       req.Title,
		Body:        req.Body,
		Payload:     req.Payload,
		Options:     req.Options,
		Deadline:    req.Deadline.Time(),
		Quorum:      quorum,
		Escalation:  req.EscalationTarget,
	})
	if errors.Is(err, approvals.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "approvals: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, schema.CreateApprovalResponseV1{Approval: created.ToSchema()})
}

// handleListOpenApprovals serves GET /v1/approvals: the approvals still open
// for decisions (pending or escalated) — the parity base for
// `conch approvals list` (issue #16). With an authenticated caller it lists
// only approvals in channels the caller is a member of (issue #92); with no
// caller, every open approval as before.
func (s *Server) handleListOpenApprovals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	open, err := s.store.ListOpenApprovals(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: list open failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if caller, ok := callerFrom(ctx); ok {
		channels, err := s.store.ListChannelsForPrincipal(ctx, caller.ID)
		if err != nil {
			slog.ErrorContext(ctx, "approvals: list caller channels failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		mine := make(map[int64]bool, len(channels))
		for _, c := range channels {
			mine[c.ID] = true
		}
		visible := open[:0:0]
		for _, a := range open {
			if mine[a.ChannelID] {
				visible = append(visible, a)
			}
		}
		open = visible
	}
	list := make([]schema.ApprovalV1, len(open))
	for i, a := range open {
		list[i] = a.ToSchema()
	}
	writeJSON(w, http.StatusOK, schema.ListApprovalsResponseV1{Approvals: list})
}

// handleCastDecision serves POST /v1/approvals/{id}/decisions: a human
// principal casts a decision with its required reason. Decisions are cast
// only by humans (approval-object.md §3); an agent principal is refused.
//
// With an authenticated caller (issue #92) the decider is the caller — a
// principal_id naming anyone else is refused — and the caller must be a member
// of the approval's channel; an approval elsewhere looks like one that does
// not exist. The audit actor is therefore the authenticated principal. With
// no caller (AuthOff) the body is trusted as before.
func (s *Server) handleCastDecision(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	approvalID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || approvalID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "approval id must be a positive integer")
		return
	}
	var req schema.CastDecisionRequestV1
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	// Bind before validation: Validate rejects a zero principal_id, but an
	// authenticated caller may omit it.
	if !s.bindCallerID(w, r, &req.PrincipalID, "principal_mismatch", "principal_id does not match the authenticated principal") {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	principal, err := s.store.PrincipalByID(ctx, req.PrincipalID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "principal_not_found", "principal not found")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "approvals: find principal failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if principal.Kind != store.PrincipalHuman {
		writeError(w, http.StatusForbidden, "human_required", "decisions are cast by human principals only")
		return
	}

	// With a caller, the decider must be a member of the approval's channel.
	// An unknown approval and one in a channel the caller is not in get the
	// same answer, so approval ids cannot be probed.
	if _, ok := callerFrom(ctx); ok {
		target, err := s.store.ApprovalByID(ctx, approvalID)
		if errors.Is(err, store.ErrNotFound) {
			writeApprovalNotFound(w)
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "approvals: find approval failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		}
		if member, err := s.callerIsMember(r, target.ChannelID); err != nil {
			slog.ErrorContext(ctx, "approvals: check membership failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			return
		} else if !member {
			writeApprovalNotFound(w)
			return
		}
	}

	decision, resolution, err := s.approvals.Decide(ctx, approvalID, req.PrincipalID, req.OptionID, req.Reason)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeApprovalNotFound(w)
		return
	case errors.Is(err, store.ErrTerminalApproval):
		writeError(w, http.StatusConflict, "approval_terminal", "approval is already resolved or expired")
		return
	case errors.Is(err, store.ErrUnknownOption):
		writeError(w, http.StatusBadRequest, "unknown_option", "option is not among the approval's options")
		return
	case errors.Is(err, store.ErrDuplicateDecision):
		writeError(w, http.StatusConflict, "duplicate_decision", "principal already decided this approval")
		return
	case err != nil:
		slog.ErrorContext(ctx, "approvals: cast decision failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	// The decision is committed. Reading the state back for the response
	// does not depend on the caller still being there: on the request
	// context a hang-up here was logged as a store failure (issue #158).
	after, err := s.store.ApprovalByID(context.WithoutCancel(ctx), approvalID)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: read state after decision failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, schema.CastDecisionResponseV1{
		Decision:   decision,
		State:      after.State,
		Resolution: resolution,
	})
}
