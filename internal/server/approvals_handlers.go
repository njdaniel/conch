package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// handleCreateApproval serves POST /v1/approvals: an agent (or human) raises
// a new approval, which is persisted pending with its approval_created audit
// event, notified, and armed with its deadline timer.
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
		writeError(w, http.StatusBadRequest, "channel_not_found", "channel not found")
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "approvals: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
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

// handleListOpenApprovals serves GET /v1/approvals: every approval still open
// for decisions (pending or escalated) — the parity base for
// `conch approvals list` (issue #16).
func (s *Server) handleListOpenApprovals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	open, err := s.store.ListOpenApprovals(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "approvals: list open failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
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

	decision, resolution, err := s.approvals.Decide(ctx, approvalID, req.PrincipalID, req.OptionID, req.Reason)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "approval_not_found", "approval not found")
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

	after, err := s.store.ApprovalByID(ctx, approvalID)
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
