package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func (s *Server) handleGetDMPreferences(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	prefs, err := s.store.DMPreferences(r.Context(), caller.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "dms: read contact preference failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

func (s *Server) handlePutDMPreferences(w http.ResponseWriter, r *http.Request) {
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	var req schema.DMPreferencesV1
	if err := decodeJSONBody(w, r, &req); err != nil || req.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "contact preference must be everyone, shared_channel or nobody")
		return
	}
	prefs, err := s.store.SetDMPreferences(r.Context(), caller.ID, req, s.now())
	switch {
	case errors.Is(err, store.ErrDMNoStanding):
		writeError(w, http.StatusForbidden, errForbidden.Code, "direct messages are for people")
	case err != nil:
		slog.ErrorContext(r.Context(), "dms: write contact preference failed", "error", err)
		writeInternalError(w)
	default:
		writeJSON(w, http.StatusOK, prefs)
	}
}

func (s *Server) handleGetDMServerSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := dmCaller(w, r); !ok {
		return
	}
	settings, err := s.store.DMServerSettings(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "dms: read server settings failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handlePutDMServerSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := dmCaller(w, r)
	if !ok {
		return
	}
	var req dmServerSettingsBody
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "dms_enabled must be a boolean")
		return
	}
	// The store rechecks the permission and audits changes under the write lock.
	_, err := s.store.SetDMServerSettings(ctx, auditActor(ctx), caller.ID, req.DMServerSettingsV1, s.now())
	var denied *store.PermissionError
	switch {
	case errors.As(err, &denied) && errors.Is(err, store.ErrPermissionDenied):
		s.denyForbiddenFor(w, r, caller, schema.ErrorCodePermissionDenied, denied.Reason, denied.Permission)
	case err != nil:
		slog.ErrorContext(ctx, "dms: write server settings failed", "error", err)
		writeInternalError(w)
	default:
		writeJSON(w, http.StatusOK, req.DMServerSettingsV1)
	}
}

// A replacement must supply dms_enabled explicitly. A missing or null field
// would otherwise decode to false and turn off DMs.
type dmServerSettingsBody struct {
	schema.DMServerSettingsV1
}

func (b *dmServerSettingsBody) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if value, ok := fields["dms_enabled"]; !ok || bytes.Equal(value, []byte("null")) {
		return errors.New("dms_enabled is required")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(&b.DMServerSettingsV1)
}
