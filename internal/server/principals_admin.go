package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Operator administration of principals (issue #445): the list of every
// principal, and hiding one or many. Every route here is operator-only in the
// route table, so a member or an agent is refused and audited as
// access_denied before a handler runs.
//
// Hiding is how an account is deleted in the private alpha: the principal is
// disabled through the same store path and the same follow-up as
// POST /v1/principals/{id}/disable (sockets dropped, rosters refreshed, out of
// voice), leaves every channel and net, loses its webhooks, and gives up its
// name. A hidden principal's open approvals are left as they are: one that
// escalates to it stalls rather than failing open (see the follow-up issue).

// handleListPrincipals serves GET /v1/principals: every principal, newest
// first, as an operator sees it. ?kind=, ?role=, ?state=, ?invite_id=,
// ?guest= (a guest state, issue #705) and ?name_prefix= narrow it; ?before= and ?limit= page it. An empty value is as
// if absent; an unknown one is 400 invalid_request.
func (s *Server) handleListPrincipals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, ok := principalFilterFrom(w, r)
	if !ok {
		return
	}
	limit := f.Limit
	// One extra row, so next_before is sent only when there is more.
	f.Limit++
	rows, err := s.store.ListPrincipals(ctx, f, s.now())
	if err != nil {
		slog.ErrorContext(ctx, "principals: list failed", "error", err)
		writeInternalError(w)
		return
	}
	resp := schema.ListPrincipalsResponseV1{Principals: rows}
	if len(rows) > limit {
		resp.Principals = rows[:limit]
		resp.NextBefore = resp.Principals[limit-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// principalFilterFrom reads the list's query. On a bad value it writes 400
// and returns false.
func principalFilterFrom(w http.ResponseWriter, r *http.Request) (store.PrincipalFilter, bool) {
	q := r.URL.Query()
	bad := func(msg string) (store.PrincipalFilter, bool) {
		writeError(w, http.StatusBadRequest, "invalid_request", msg)
		return store.PrincipalFilter{}, false
	}
	f := store.PrincipalFilter{Limit: schema.DefaultListPrincipalsLimit}
	switch k := schema.PrincipalKind(q.Get("kind")); k {
	case "":
	case schema.PrincipalHuman, schema.PrincipalAgent:
		f.Kind = store.PrincipalKind(k)
	default:
		return bad(`kind must be "human" or "agent"`)
	}
	switch role := schema.PrincipalRole(q.Get("role")); role {
	case "":
	case schema.RoleOperator, schema.RoleMember:
		f.Role = store.Role(role)
	default:
		return bad(`role must be "operator" or "member"`)
	}
	if state := schema.PrincipalState(q.Get("state")); state != "" {
		if !state.Valid() {
			return bad("state must be one of: " + strings.Join(schema.Vocabularies["PrincipalState"], ", "))
		}
		f.State = state
	}
	if raw := q.Get("invite_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return bad("invite_id must be a positive integer")
		}
		f.InviteID = id
	}
	if guest := schema.GuestState(q.Get("guest")); guest != "" {
		if !guest.Valid() {
			return bad("guest must be one of: " + strings.Join(schema.Vocabularies["GuestState"], ", "))
		}
		f.Guest = guest
	}
	if prefix := q.Get("name_prefix"); prefix != "" {
		if !utf8.ValidString(prefix) || utf8.RuneCountInString(prefix) > schema.MaxPrincipalNameLength {
			return bad(fmt.Sprintf("name_prefix must be valid UTF-8 of at most %d characters", schema.MaxPrincipalNameLength))
		}
		f.NamePrefix = prefix
	}
	before, err := nonNegativeQueryInt64(r, "before", 0)
	if err != nil {
		return bad(err.Error())
	}
	f.Before = before
	limit, err := nonNegativeQueryInt64(r, "limit", schema.DefaultListPrincipalsLimit)
	if err != nil || limit == 0 || limit > schema.MaxListPrincipalsLimit {
		return bad(fmt.Sprintf("limit must be between 1 and %d", schema.MaxListPrincipalsLimit))
	}
	f.Limit = int(limit)
	return f, true
}

// handleHidePrincipal serves POST /v1/principals/{id}/hide: it disables the
// principal unless it already is, takes it out of every channel and net,
// marks it hidden and renames it hidden-<id>, all in one transaction, then
// drops what it still holds as the disable route does. The answer is the
// principal as it now is. Hiding a hidden principal changes nothing and
// answers the same. The caller may not hide themselves (400), and the last
// operator able to sign in is never hidden (409 last_operator).
//
// With authentication off there is no caller, and the route is open like the
// disable route.
func (s *Server) handleHidePrincipal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	if caller, authed := callerFrom(ctx); authed && caller.ID == id {
		writeError(w, http.StatusBadRequest, "invalid_request", "an operator cannot hide themselves")
		return
	}
	if s.ownerProtected(w, r, id) {
		return
	}
	_, err := s.store.HidePrincipal(ctx, auditActor(ctx), id, store.HideOptions{})
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrGuestNotHideable):
		writeGuestNotHideable(w)
		return
	case errors.Is(err, store.ErrLastOperator):
		writeError(w, http.StatusConflict, "last_operator", lastOperatorMessage)
		return
	case err != nil:
		slog.ErrorContext(ctx, "principals: hide failed", "principal", id, "error", err)
		writeInternalError(w)
		return
	}
	// Dropped even when nothing changed, as the disable route does: a repeat
	// is a harmless way to make sure nothing survives.
	s.principalDisabled(ctx, id)
	row, err := s.store.PrincipalAdmin(ctx, id, s.now())
	if err != nil {
		slog.ErrorContext(ctx, "principals: read hidden principal failed", "principal", id, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.HidePrincipalResponseV1{Principal: row})
}

// handleHidePrincipals serves POST /v1/principals/hide: it hides every
// principal the body selects, by invite, by name prefix or both, narrowed by
// kind, at most schema.MaxHidePrincipals at a time. Operators, principals
// already hidden and the caller are never selected. Each is hidden exactly as
// the single route hides one, in its own transaction, and one
// principals_bulk_hidden row records the selector and the count. A dry run
// writes nothing and answers with whom it would hide.
//
// With authentication off there is no caller to leave out, and the answer is
// the 401 of the other bulk routes.
func (s *Server) handleHidePrincipals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		writeUnauthenticated(w)
		return
	}
	var req schema.HidePrincipalsRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	sel := store.HideSelector{InviteID: req.InviteID, NamePrefix: req.NamePrefix, Kind: store.PrincipalKind(req.Kind)}
	targets, remaining, err := s.store.HideTargets(ctx, sel, caller.ID, schema.MaxHidePrincipals)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, schema.ErrorCodeInviteNotFound, "invite not found")
		return
	case err != nil:
		slog.ErrorContext(ctx, "principals: select for bulk hide failed", "error", err)
		writeInternalError(w)
		return
	}
	// Validate has refused a request without dry_run.
	if *req.DryRun {
		found := make([]schema.HiddenPrincipalV1, 0, len(targets))
		for _, p := range targets {
			found = append(found, schema.HiddenPrincipalV1{ID: p.ID, Name: p.Name})
		}
		writeJSON(w, http.StatusOK, schema.HidePrincipalsResponseV1{DryRun: true, Count: len(found), Principals: found, Remaining: remaining})
		return
	}
	done, hideErr := s.hidePrincipals(ctx, targets)

	// The client may be gone; what was done must still be recorded.
	ctx = context.WithoutCancel(ctx)
	_, auditErr := s.store.AppendAuditEvent(ctx, auditActor(ctx), store.AuditPrincipalsBulkHidden, bulkHideSubject(req),
		bulkHideDetail(req, len(done), remaining))
	if hideErr != nil || auditErr != nil {
		slog.ErrorContext(ctx, "principals: bulk hide failed", "hidden", len(done), "error", errors.Join(hideErr, auditErr))
		writeInternalError(w)
		return
	}
	slog.InfoContext(ctx, "principals: bulk hide", "hidden", len(done), "remaining", remaining)
	writeJSON(w, http.StatusOK, schema.HidePrincipalsResponseV1{Count: len(done), Principals: done, Remaining: remaining})
}

// hidePrincipals hides each of targets in turn, exactly as the single route
// does, refusing operators, and returns those it hid, under the names they
// had. A principal that is gone, already hidden or an operator is skipped;
// none should be among the targets. It stops at the first other error and
// returns what it did before it.
//
// It does not stop when the operator's client goes away, as the bulk disable
// does not: a cleanup cut short would leave the rest in place, unseen.
func (s *Server) hidePrincipals(ctx context.Context, targets []store.Principal) ([]schema.HiddenPrincipalV1, error) {
	ctx = context.WithoutCancel(ctx)
	actor := auditActor(ctx)
	done := []schema.HiddenPrincipalV1{}
	for _, p := range targets {
		res, err := s.store.HidePrincipal(ctx, actor, p.ID, store.HideOptions{RefuseOperator: true})
		switch {
		case errors.Is(err, store.ErrPrincipalNotFound), errors.Is(err, store.ErrHideOperator), errors.Is(err, store.ErrLastOperator),
			errors.Is(err, store.ErrGuestNotHideable):
			continue
		case err != nil:
			return done, fmt.Errorf("hide principal %d: %w", p.ID, err)
		}
		s.principalDisabled(ctx, p.ID)
		if res.Changed {
			done = append(done, schema.HiddenPrincipalV1{ID: p.ID, Name: p.Name})
		}
	}
	return done, nil
}

// bulkHideSubject is the invite a bulk hide selected by, if any.
func bulkHideSubject(req schema.HidePrincipalsRequestV1) string {
	if req.InviteID > 0 {
		return fmt.Sprintf("invite:%d", req.InviteID)
	}
	return ""
}

// bulkHideDetail records a bulk hide's selector, what it hid and what is
// left. The prefix is quoted, so one with spaces reads as one field.
func bulkHideDetail(req schema.HidePrincipalsRequestV1, count, remaining int) string {
	var parts []string
	if req.InviteID > 0 {
		parts = append(parts, fmt.Sprintf("invite=%d", req.InviteID))
	}
	if req.NamePrefix != "" {
		parts = append(parts, fmt.Sprintf("prefix=%q", req.NamePrefix))
	}
	if req.Kind != "" {
		parts = append(parts, "kind="+string(req.Kind))
	}
	parts = append(parts, fmt.Sprintf("count=%d remaining=%d", count, remaining))
	return strings.Join(parts, " ")
}
