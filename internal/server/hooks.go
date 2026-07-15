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
	channel, err := s.store.ChannelByName(ctx, req.Channel)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "channel_not_found", "channel not found")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "hooks: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if _, err := s.store.PrincipalByID(ctx, req.Principal); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "principal_not_found", "principal not found")
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "hooks: find principal failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	tokenBytes := make([]byte, hookTokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		slog.ErrorContext(ctx, "hooks: generate token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	if _, err := s.store.CreateHook(ctx, token, channel.ID, req.Principal); err != nil {
		slog.ErrorContext(ctx, "hooks: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, schema.CreateHookResponse{Token: token})
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
		post = schema.PostMessageRequestV1{Body: string(body)}
	} else if err := decodeHookPost(body, &post); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a v1 post message object")
		return
	}
	post.AuthorID = hook.PrincipalID
	normalized, err := json.Marshal(post)
	if err != nil {
		slog.ErrorContext(ctx, "hooks: normalize request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(normalized))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("channel", channel.Name)
	s.handlePostMessageV1(w, r)
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
