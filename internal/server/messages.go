package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const (
	defaultMessageLimit = 50
	maxMessageLimit     = 100
	maxMessageBodyBytes = 1 << 20
)

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel_not_found", "channel not found")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "messages: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	var req schema.PostMessageRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if req.AuthorID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "author_id must be positive")
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must not be empty")
		return
	}
	if _, err := s.store.PrincipalByID(ctx, req.AuthorID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "author_not_found", "author not found")
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "messages: find author failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	stored, err := s.store.InsertMessage(ctx, channel.ID, req.AuthorID, req.Body)
	if err != nil {
		slog.ErrorContext(ctx, "messages: insert failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	message := messageFromStore(stored)
	s.broadcaster.BroadcastMessage(ctx, message)
	writeJSON(w, http.StatusCreated, schema.PostMessageResponse{Message: message})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, err := s.store.ChannelByName(ctx, r.PathValue("channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel_not_found", "channel not found")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "messages: find channel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	after, err := nonNegativeQueryInt64(r, "after", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	parsedLimit, err := nonNegativeQueryInt64(r, "limit", defaultMessageLimit)
	if err != nil || parsedLimit == 0 || parsedLimit > maxMessageLimit {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
		return
	}
	limit := int(parsedLimit)

	// Fetch one extra row so the response only advertises a next page when one
	// is currently available.
	stored, err := s.store.ListMessages(ctx, channel.ID, after, limit+1)
	if err != nil {
		slog.ErrorContext(ctx, "messages: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	nextAfter := int64(0)
	if len(stored) > limit {
		stored = stored[:limit]
		nextAfter = stored[len(stored)-1].ID
	}
	messages := make([]schema.MessageV0, len(stored))
	for i, message := range stored {
		messages[i] = messageFromStore(message)
	}
	writeJSON(w, http.StatusOK, schema.ListMessagesResponse{
		Messages:  messages,
		NextAfter: nextAfter,
	})
}

func messageFromStore(message store.Message) schema.MessageV0 {
	return schema.MessageV0{
		ID:        message.ID,
		ChannelID: message.ChannelID,
		AuthorID:  message.AuthorID,
		Body:      message.Body,
		// The wire timestamp is always UTC; store values carry the server's
		// local zone.
		CreatedAt: message.CreatedAt.UTC(),
	}
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("server: request body must contain one JSON object")
	}
	return nil
}

func nonNegativeQueryInt64(r *http.Request, name string, defaultValue int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New(name + " must be a non-negative integer")
	}
	return value, nil
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, schema.Error{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("server: encode response failed", "error", err)
	}
}
