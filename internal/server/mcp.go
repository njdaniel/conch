package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

type mcpPayloadInput struct {
	Schema string `json:"schema" jsonschema:"versioned payload schema name"`
	Data   any    `json:"data" jsonschema:"payload data, any JSON value"`
}

type mcpPostMessageInput struct {
	Channel string           `json:"channel" jsonschema:"channel name"`
	Body    string           `json:"body" jsonschema:"rendered human-readable message body"`
	Payload *mcpPayloadInput `json:"payload,omitempty" jsonschema:"optional typed machine payload"`
}

type mcpReadChannelInput struct {
	Channel string `json:"channel" jsonschema:"channel name"`
	After   int64  `json:"after,omitempty" jsonschema:"return messages with id greater than this cursor"`
	Limit   *int64 `json:"limit,omitempty" jsonschema:"page size, default 50, max 100"`
}

func (s *Server) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		principalID, ok := s.authenticateMCP(r)
		if !ok {
			return nil
		}
		return s.mcpServerForPrincipal(principalID)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.authenticateMCP(r); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="conch-mcp"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) authenticateMCP(r *http.Request) (int64, bool) {
	raw := r.Header.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		return 0, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if token == "" {
		return 0, false
	}
	principalID, ok := s.cfg.MCPBearerTokens[token]
	if !ok || principalID <= 0 {
		return 0, false
	}
	principal, err := s.store.PrincipalByID(r.Context(), principalID)
	if err != nil || principal.Kind != store.PrincipalAgent {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.ErrorContext(r.Context(), "mcp: authenticate principal failed", "error", err)
		}
		return 0, false
	}
	return principalID, true
}

func (s *Server) mcpServerForPrincipal(principalID int64) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "conchd", Version: s.cfg.Version}, nil)
mcp.AddTool(server, &mcp.Tool{Name: "post_message", Description: "Post a message to a Conch channel as the authenticated agent."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpPostMessageInput) (*mcp.CallToolResult, schema.PostMessageResponseV1, error) {
			out, serr := s.postMessageMCP(ctx, principalID, in)
			if serr != nil {
				return mcpToolError(serr), schema.PostMessageResponseV1{}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("posted message %d", out.Message.ID)}}}, out, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "read_channel", Description: "Read one paginated page of messages from a Conch channel."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpReadChannelInput) (*mcp.CallToolResult, schema.ListMessagesResponseV1, error) {
			out, serr := s.readChannelMCP(ctx, in)
			if serr != nil {
				return mcpToolError(serr), schema.ListMessagesResponseV1{}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("read %d messages", len(out.Messages))}}}, out, nil
		})
	return server
}

func mcpToolError(err *schema.Error) *mcp.CallToolResult {
	b, marshalErr := json.Marshal(err)
	text := err.Message
	if marshalErr == nil {
		text = string(b)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: err, IsError: true}
}

func (s *Server) postMessageMCP(ctx context.Context, authorID int64, in mcpPostMessageInput) (schema.PostMessageResponseV1, *schema.Error) {
	if strings.TrimSpace(in.Channel) == "" {
		return schema.PostMessageResponseV1{}, &schema.Error{Code: "invalid_request", Message: "channel must not be empty"}
	}
	if strings.TrimSpace(in.Body) == "" {
		return schema.PostMessageResponseV1{}, &schema.Error{Code: "invalid_request", Message: "body must not be empty"}
	}
	var schemaPayload *schema.Payload
	if in.Payload != nil {
		dataBytes, marshalErr := json.Marshal(in.Payload.Data)
		if marshalErr != nil {
			return schema.PostMessageResponseV1{}, &schema.Error{Code: "invalid_request", Message: "invalid payload data"}
		}
		schemaPayload = &schema.Payload{Schema: in.Payload.Schema, Data: json.RawMessage(dataBytes)}
	}
	req := schema.PostMessageRequestV1{AuthorID: authorID, Body: in.Body, Payload: schemaPayload}
	if err := req.Validate(); err != nil {
		return schema.PostMessageResponseV1{}, &schema.Error{Code: "invalid_request", Message: err.Error()}
	}
	channel, err := s.store.ChannelByName(ctx, in.Channel)
	if errors.Is(err, store.ErrNotFound) {
		return schema.PostMessageResponseV1{}, &schema.Error{Code: "channel_not_found", Message: "channel not found"}
	}
	if err != nil {
		slog.ErrorContext(ctx, "mcp: find channel failed", "error", err)
		return schema.PostMessageResponseV1{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	stored, err := s.store.InsertMessageV1(ctx, channel.ID, authorID, in.Body, schemaPayload)
	if err != nil {
		slog.ErrorContext(ctx, "mcp: insert failed", "error", err)
		return schema.PostMessageResponseV1{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	messageV1 := messageV1FromStore(stored)
	s.hub.BroadcastMessageV1(ctx, messageV1)
	messageV0 := messageV0FromStore(stored)
	s.hub.BroadcastMessage(ctx, messageV0)
	s.broadcaster.BroadcastMessage(ctx, messageV0)
	return schema.PostMessageResponseV1{Message: messageV1}, nil
}

func (s *Server) readChannelMCP(ctx context.Context, in mcpReadChannelInput) (schema.ListMessagesResponseV1, *schema.Error) {
	if strings.TrimSpace(in.Channel) == "" {
		return schema.ListMessagesResponseV1{}, &schema.Error{Code: "invalid_request", Message: "channel must not be empty"}
	}
	if in.After < 0 {
		return schema.ListMessagesResponseV1{}, &schema.Error{Code: "invalid_request", Message: "after must be a non-negative integer"}
	}
	limit := in.Limit
	var limitVal int64
	if limit == nil {
		limitVal = defaultMessageLimit
	} else if *limit <= 0 || *limit > maxMessageLimit {
		return schema.ListMessagesResponseV1{}, &schema.Error{Code: "invalid_request", Message: "limit must be between 1 and 100"}
	} else {
		limitVal = *limit
	}
	channel, err := s.store.ChannelByName(ctx, in.Channel)
	if errors.Is(err, store.ErrNotFound) {
		return schema.ListMessagesResponseV1{}, &schema.Error{Code: "channel_not_found", Message: "channel not found"}
	}
	if err != nil {
		slog.ErrorContext(ctx, "mcp: find channel failed", "error", err)
		return schema.ListMessagesResponseV1{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	stored, err := s.store.ListMessages(ctx, channel.ID, in.After, int(limitVal)+1)
	if err != nil {
		slog.ErrorContext(ctx, "mcp: list failed", "error", err)
		return schema.ListMessagesResponseV1{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	nextAfter := int64(0)
	if len(stored) > int(limitVal) {
		stored = stored[:int(limitVal)]
		nextAfter = stored[len(stored)-1].ID
	}
	messages := make([]schema.MessageV1, len(stored))
	for i, message := range stored {
		messages[i] = messageV1FromStore(message)
	}
	return schema.ListMessagesResponseV1{Messages: messages, NextAfter: nextAfter}, nil
}
