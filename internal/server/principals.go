package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func (s *Server) handleCreatePrincipal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req schema.CreatePrincipalRequest
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

	// principal_created is written with the principal (issue #445).
	principal, err := s.store.CreatePrincipalAs(ctx, auditActor(ctx), store.PrincipalKind(req.Kind), req.Name)
	if errors.Is(err, store.ErrNameReserved) {
		writeError(w, http.StatusConflict, schema.ErrorCodePrincipalExists, nameReservedMessageFor(err))
		return
	}
	if errors.Is(err, store.ErrDuplicate) {
		writeError(w, http.StatusConflict, schema.ErrorCodePrincipalExists, principalExistsMessage)
		return
	}
	if errors.Is(err, store.ErrNameInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request", nameInvisibleMessage)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "principals: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, schema.CreatePrincipalResponse{Principal: principalFromStore(principal)})
}

// The limits on a person's renames of their own name in a store.RenameWindow
// (issue #441). renamesPerDay is enough to fix a typo or two and too few to
// cycle through other people's names. renameAttemptsPerDay counts refused
// attempts too: a rename refused as taken says the name, or one like it, is
// on the server, and without a bound that would be a free way to ask.
const (
	renamesPerDay        = 5
	renameAttemptsPerDay = 20
)

// handleRenamePrincipal serves PUT /v1/principals/{id}/name (issue #441). A
// person may rename themselves, within the limits above; only their own
// renames and attempts count toward them. An operator may rename anyone with
// no limit, and an operator's rename of someone else counts toward nothing:
// an operator may already create a principal with any name, and an
// operator's cleanup must not fail on a limit. (Hiding an account renames it
// with the same store write, but through POST /v1/principals/{id}/hide, not
// this route.) An agent may not rename itself: its name is set with its
// manifest by an operator. Refusals of who may rename are audited as
// access_denied, and are decided before the body is read or the principal
// looked up, so a caller cannot probe which principals exist. The new name
// meets the create rule, must have something a reader can see (400), must not
// be reserved for hidden principals (409 principal_exists, saying so), and
// must not clash with anyone else's (409 principal_exists; for a person
// renaming themselves the refusal is also audited, without the name, as
// principal_rename_refused).
func (s *Server) handleRenamePrincipal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "principal")
	if !ok {
		return
	}
	var lim store.RenameLimits
	self := false
	// No caller is AuthOff, where the route is open like every other.
	if caller, authed := callerFrom(ctx); authed && !isOperator(caller) {
		if caller.ID != id {
			s.denyForbidden(w, r, caller, "forbidden", "operator role or the principal itself required")
			return
		}
		if caller.Kind != store.PrincipalHuman {
			s.denyForbidden(w, r, caller, schema.ErrorCodeHumanRequired, "an agent's name is set by an operator")
			return
		}
		lim = store.RenameLimits{Renames: renamesPerDay, Attempts: renameAttemptsPerDay}
		self = true
	}

	var req schema.RenamePrincipalRequestV1
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

	res, err := s.store.RenamePrincipal(ctx, auditActor(ctx), id, req.Name, lim, s.now())
	var limited *store.RenameLimitedError
	switch {
	case errors.Is(err, store.ErrPrincipalNotFound):
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	case errors.Is(err, store.ErrGuestNotHideable):
		// Issue #705, review F10: a guest's name is its handle, whoever asks.
		writeGuestNotHideable(w)
		return
	case errors.Is(err, store.ErrNameInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", nameInvisibleMessage)
		return
	case errors.Is(err, store.ErrNameReserved):
		// Not counted as a refused attempt: it says nothing of who is on the
		// server, which is what the attempt limit guards.
		writeError(w, http.StatusConflict, schema.ErrorCodePrincipalExists, nameReservedMessageFor(err))
		return
	case errors.Is(err, store.ErrDuplicate):
		if self {
			// The rename rolled back; the attempt is recorded on its own,
			// and still counts if the client has gone.
			if err := s.store.RecordRenameRefused(context.WithoutCancel(ctx), id, s.now()); err != nil {
				slog.ErrorContext(ctx, "principals: audit refused rename failed", "principal", id, "error", err)
			}
		}
		writeError(w, http.StatusConflict, schema.ErrorCodePrincipalExists, principalExistsMessage)
		return
	case errors.As(err, &limited):
		seconds := max(1, int(math.Ceil(limited.Until.Sub(s.now()).Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeJSON(w, http.StatusTooManyRequests, schema.Error{
			Code: schema.ErrorCodeRateLimited,
			Message: fmt.Sprintf("you may change your name at most %d times, and try at most %d times, a day",
				renamesPerDay, renameAttemptsPerDay),
			RetryAfterSeconds: seconds,
		})
		return
	case err != nil:
		slog.ErrorContext(ctx, "principals: rename failed", "principal", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if res.Changed {
		// Rosters carry names: every events socket rebuilds its roster and
		// sends it if it changed. Messages carry author ids, so clients show
		// the new name on old messages once they reread the member list.
		s.events.notifyRosters()
	}
	writeJSON(w, http.StatusOK, schema.RenamePrincipalResponseV1{Principal: principalFromStore(res.Principal)})
}

// The refusals every name write shares (issue #441).
const (
	principalExistsMessage = "a principal with this name, or one that looks the same, already exists"
	// nameReservedMessage answers a name only a hide gives (issue #445),
	// with the 409 a taken name gets: clients already ask for another.
	nameReservedMessage  = "names beginning with " + schema.HiddenPrincipalNamePrefix + " are reserved; choose another"
	nameInvisibleMessage = "schema: principal name must have a character a reader can see"
	// guestNameReservedMessage answers a permanent name in the guest
	// namespace (issue #705), which only the server gives, to guests.
	guestNameReservedMessage = "names beginning with " + schema.ReservedGuestNameKeyPrefix + ", or with a look-alike of it, are reserved for guests; choose another"
)

// nameReservedMessageFor is the message of a 409 for a reserved name: the
// guest namespace's or the hidden prefix's.
func nameReservedMessageFor(err error) string {
	if errors.Is(err, store.ErrGuestNameReserved) {
		return guestNameReservedMessage
	}
	return nameReservedMessage
}

// writeGuestNotHideable is the 409 for a rename or hide of a guest.
func writeGuestNotHideable(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, schema.ErrorCodeGuestNotHideable,
		"a guest keeps its handle: it is not renamed or hidden; end the visit instead")
}

func principalFromStore(principal store.Principal) schema.PrincipalV0 {
	return schema.PrincipalV0{
		ID:   principal.ID,
		Kind: schema.PrincipalKind(principal.Kind),
		Name: principal.Name,
		// The wire timestamp is always UTC; store values carry the server's
		// local zone.
		CreatedAt: principal.CreatedAt.UTC(),
	}
}
