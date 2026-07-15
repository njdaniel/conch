package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/njdaniel/conch/internal/server/approvals"
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

type mcpRequestApprovalInput struct {
	ChannelID        int64                    `json:"channel_id" jsonschema:"channel in which to raise the approval"`
	Title            string                   `json:"title" jsonschema:"short approval heading"`
	Body             string                   `json:"body" jsonschema:"approval detail"`
	Payload          *mcpPayloadInput         `json:"payload,omitempty" jsonschema:"optional typed machine payload"`
	Options          []schema.Option          `json:"options" jsonschema:"choices including at least one approve and one reject"`
	Deadline         string                   `json:"deadline" jsonschema:"required RFC3339 decision deadline"`
	Quorum           int                      `json:"quorum,omitempty" jsonschema:"concurring decisions required; default 1"`
	EscalationTarget *schema.EscalationTarget `json:"escalation_target,omitempty" jsonschema:"optional deadline escalation destination"`
}

type mcpAwaitDecisionInput struct {
	ApprovalID int64 `json:"approval_id" jsonschema:"approval id"`
	TimeoutMS  int64 `json:"timeout_ms" jsonschema:"maximum wait in milliseconds; values above 60000 are clamped"`
}

type mcpCheckDecisionInput struct {
	ApprovalID int64 `json:"approval_id" jsonschema:"approval id"`
}

type mcpRequestApprovalOutput struct {
	ID       int64                `json:"id"`
	Title    string               `json:"title"`
	State    schema.ApprovalState `json:"state"`
	Deadline string               `json:"deadline"`
}

// Approval resolutions contain canonical Timestamp values, which the SDK
// otherwise infers as objects rather than their JSON string encoding.
type mcpCheckDecisionOutput struct {
	State      schema.ApprovalState `json:"state"`
	Resolution any                  `json:"resolution,omitempty"`
}

type mcpAwaitDecisionOutput struct {
	State              schema.ApprovalState `json:"state"`
	Resolution         any                  `json:"resolution,omitempty"`
	EffectiveTimeoutMS int64                `json:"effective_timeout_ms"`
}

const (
	mcpAwaitTimeoutCap = 60 * time.Second
	mcpPollInterval    = 200 * time.Millisecond
)

// The SDK infers json.RawMessage as a byte array even though its JSON encoding
// is an arbitrary JSON value. Keep the canonical pkg/schema values internally,
// but project MCP outputs through equivalent structs whose payload data is any
// so the generated output schema validates objects, arrays, and scalars.
type mcpPayloadOutput struct {
	Schema string `json:"schema"`
	Data   any    `json:"data"`
}

type mcpMessageOutput struct {
	Schema    string            `json:"schema"`
	ID        int64             `json:"id"`
	ChannelID int64             `json:"channel_id"`
	AuthorID  int64             `json:"author_id"`
	CreatedAt string            `json:"created_at"`
	Body      string            `json:"body"`
	Payload   *mcpPayloadOutput `json:"payload,omitempty"`
}

type mcpPostMessageOutput struct {
	Message mcpMessageOutput `json:"message"`
}

type mcpListMessagesOutput struct {
	Messages  []mcpMessageOutput `json:"messages"`
	NextAfter int64              `json:"next_after,omitempty"`
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpPostMessageInput) (*mcp.CallToolResult, mcpPostMessageOutput, error) {
			out, serr := s.postMessageMCP(ctx, principalID, in)
			if serr != nil {
				return mcpToolError(serr), mcpPostMessageOutput{}, nil
			}
			message, err := mcpMessageFromSchema(out.Message)
			if err != nil {
				return nil, mcpPostMessageOutput{}, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("posted message %d", out.Message.ID)}}}, mcpPostMessageOutput{Message: message}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "read_channel", Description: "Read one paginated page of messages from a Conch channel."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpReadChannelInput) (*mcp.CallToolResult, mcpListMessagesOutput, error) {
			out, serr := s.readChannelMCP(ctx, in)
			if serr != nil {
				return mcpToolError(serr), mcpListMessagesOutput{}, nil
			}
			messages := make([]mcpMessageOutput, len(out.Messages))
			for i, message := range out.Messages {
				projected, err := mcpMessageFromSchema(message)
				if err != nil {
					return nil, mcpListMessagesOutput{}, err
				}
				messages[i] = projected
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("read %d messages", len(out.Messages))}}}, mcpListMessagesOutput{Messages: messages, NextAfter: out.NextAfter}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "request_approval", Description: "Raise an approval as the authenticated agent."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpRequestApprovalInput) (*mcp.CallToolResult, mcpRequestApprovalOutput, error) {
			out, serr := s.requestApprovalMCP(ctx, principalID, in)
			if serr != nil {
				return mcpToolError(serr), mcpRequestApprovalOutput{}, nil
			}
			projected := mcpRequestApprovalOutput{ID: out.ID, Title: out.Title, State: out.State, Deadline: out.Deadline.Time().Format(time.RFC3339Nano)}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("requested approval %d", out.ID)}}}, projected, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "await_decision", Description: "Wait for an approval resolution, polling persisted state. timeout_ms is clamped to a server-side maximum of 60000 ms."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAwaitDecisionInput) (*mcp.CallToolResult, mcpAwaitDecisionOutput, error) {
			out, serr := s.awaitDecisionMCP(ctx, in)
			if serr != nil {
				return mcpToolError(serr), mcpAwaitDecisionOutput{}, nil
			}
			projected := mcpAwaitDecisionOutput{State: out.State, EffectiveTimeoutMS: out.EffectiveTimeoutMS}
			if out.Resolution != nil {
				projected.Resolution = out.Resolution
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("approval %d is %s", in.ApprovalID, out.State)}}}, projected, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "check_decision", Description: "Immediately read an approval's current persisted state and resolution, if terminal."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCheckDecisionInput) (*mcp.CallToolResult, mcpCheckDecisionOutput, error) {
			out, serr := s.checkDecisionMCP(ctx, in)
			if serr != nil {
				return mcpToolError(serr), mcpCheckDecisionOutput{}, nil
			}
			projected := mcpCheckDecisionOutput{State: out.State}
			if out.Resolution != nil {
				projected.Resolution = out.Resolution
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("approval %d is %s", in.ApprovalID, out.State)}}}, projected, nil
		})
	return server
}

func mcpMessageFromSchema(message schema.MessageV1) (mcpMessageOutput, error) {
	wire, err := json.Marshal(message)
	if err != nil {
		return mcpMessageOutput{}, fmt.Errorf("mcp: marshal canonical message: %w", err)
	}
	var out mcpMessageOutput
	if err := json.Unmarshal(wire, &out); err != nil {
		return mcpMessageOutput{}, fmt.Errorf("mcp: project canonical message: %w", err)
	}
	return out, nil
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

func (s *Server) requestApprovalMCP(ctx context.Context, requesterID int64, in mcpRequestApprovalInput) (schema.RequestApprovalOutput, *schema.Error) {
	deadline, err := time.Parse(time.RFC3339, in.Deadline)
	if err != nil {
		return schema.RequestApprovalOutput{}, &schema.Error{Code: "invalid_request", Message: "deadline must be an RFC3339 timestamp"}
	}
	var payload *schema.Payload
	if in.Payload != nil {
		data, marshalErr := json.Marshal(in.Payload.Data)
		if marshalErr != nil {
			return schema.RequestApprovalOutput{}, &schema.Error{Code: "invalid_request", Message: "invalid payload data"}
		}
		payload = &schema.Payload{Schema: in.Payload.Schema, Data: data}
	}
	req := schema.CreateApprovalRequestV1{RequesterID: requesterID, ChannelID: in.ChannelID, Title: in.Title, Body: in.Body, Payload: payload, Options: in.Options, Deadline: schema.NewTimestamp(deadline), Quorum: in.Quorum, EscalationTarget: in.EscalationTarget}
	if err := req.Validate(); err != nil {
		return schema.RequestApprovalOutput{}, &schema.Error{Code: "invalid_request", Message: err.Error()}
	}
	if _, err := s.store.ChannelByID(ctx, in.ChannelID); errors.Is(err, store.ErrNotFound) {
		return schema.RequestApprovalOutput{}, &schema.Error{Code: "channel_not_found", Message: "channel not found"}
	} else if err != nil {
		slog.ErrorContext(ctx, "mcp: find approval channel failed", "error", err)
		return schema.RequestApprovalOutput{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	quorum := in.Quorum
	if quorum == 0 {
		quorum = 1
	}
	created, err := s.approvals.Create(ctx, store.ApprovalParams{RequesterID: requesterID, ChannelID: in.ChannelID, Title: in.Title, Body: in.Body, Payload: payload, Options: in.Options, Deadline: deadline, Quorum: quorum, Escalation: in.EscalationTarget})
	if errors.Is(err, approvals.ErrInvalid) {
		return schema.RequestApprovalOutput{}, &schema.Error{Code: "invalid_request", Message: err.Error()}
	}
	if err != nil {
		slog.ErrorContext(ctx, "mcp: create approval failed", "error", err)
		return schema.RequestApprovalOutput{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	return schema.RequestApprovalOutput{ID: created.ID, Title: created.Title, State: created.State, Deadline: schema.NewTimestamp(created.Deadline)}, nil
}

func (s *Server) checkDecisionMCP(ctx context.Context, in mcpCheckDecisionInput) (schema.CheckDecisionOutput, *schema.Error) {
	if in.ApprovalID <= 0 {
		return schema.CheckDecisionOutput{}, &schema.Error{Code: "invalid_request", Message: "approval_id must be a positive integer"}
	}
	approval, err := s.store.ApprovalByID(ctx, in.ApprovalID)
	if errors.Is(err, store.ErrNotFound) {
		return schema.CheckDecisionOutput{}, &schema.Error{Code: "approval_not_found", Message: "approval not found"}
	}
	if err != nil {
		slog.ErrorContext(ctx, "mcp: check approval failed", "error", err)
		return schema.CheckDecisionOutput{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	out := schema.CheckDecisionOutput{State: approval.State}
	if approval.State.IsTerminal() {
		resolution, err := s.store.ResolutionByApprovalID(ctx, in.ApprovalID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp: read approval resolution failed", "error", err)
			return schema.CheckDecisionOutput{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
		}
		out.Resolution = &resolution
	}
	return out, nil
}

func (s *Server) awaitDecisionMCP(ctx context.Context, in mcpAwaitDecisionInput) (schema.AwaitDecisionOutput, *schema.Error) {
	if in.TimeoutMS <= 0 {
		return schema.AwaitDecisionOutput{}, &schema.Error{Code: "invalid_request", Message: "timeout_ms must be a positive integer"}
	}
	timeout := mcpAwaitTimeoutCap
	if in.TimeoutMS < mcpAwaitTimeoutCap.Milliseconds() {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
	}
	effectiveMS := timeout.Milliseconds()
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(mcpPollInterval)
	defer ticker.Stop()
	var last schema.CheckDecisionOutput
	for {
		if waitCtx.Err() != nil {
			if ctx.Err() != nil {
				return schema.AwaitDecisionOutput{}, &schema.Error{Code: "request_cancelled", Message: "tool call cancelled"}
			}
			return schema.AwaitDecisionOutput{State: last.State, EffectiveTimeoutMS: effectiveMS}, nil
		}
		checked, serr := s.checkDecisionMCP(waitCtx, mcpCheckDecisionInput{ApprovalID: in.ApprovalID})
		if serr != nil {
			return schema.AwaitDecisionOutput{}, serr
		}
		last = checked
		if checked.State.IsTerminal() {
			return schema.AwaitDecisionOutput{State: checked.State, Resolution: checked.Resolution, EffectiveTimeoutMS: effectiveMS}, nil
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return schema.AwaitDecisionOutput{}, &schema.Error{Code: "request_cancelled", Message: "tool call cancelled"}
			}
			return schema.AwaitDecisionOutput{State: checked.State, EffectiveTimeoutMS: effectiveMS}, nil
		case <-ticker.C:
		}
	}
}
