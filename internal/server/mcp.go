package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/pkg/schema"
)

type mcpPayloadInput struct {
	Schema string `json:"schema" jsonschema:"versioned payload schema name"`
	Data   any    `json:"data" jsonschema:"payload data, any JSON value"`
}

// mcpAudience is the audience of a message on the MCP wire, in both
// directions. It is the same shape as schema.Audience (ADR-005); the MCP SDK
// derives each tool's published schema from these structs.
type mcpAudience struct {
	Kind         string  `json:"kind" jsonschema:"net (a net of the channel the agent is a member of) or principals (a whisper)"`
	NetID        int64   `json:"net_id,omitempty" jsonschema:"the net id, for kind net"`
	PrincipalIDs []int64 `json:"principal_ids,omitempty" jsonschema:"the recipients, for kind principals; the author is added by the server"`
}

type mcpPostMessageInput struct {
	Channel  string           `json:"channel" jsonschema:"channel name"`
	Body     string           `json:"body" jsonschema:"rendered human-readable message body"`
	Payload  *mcpPayloadInput `json:"payload,omitempty" jsonschema:"optional typed machine payload"`
	Audience *mcpAudience     `json:"audience,omitempty" jsonschema:"optional audience; omit to post to the whole channel. To reply in kind, reuse the audience of the message being answered"`
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
	// Audience is absent for a channel-wide message. A message that carries
	// one was not sent to the whole channel, and a reply must not be either.
	Audience *mcpAudience `json:"audience,omitempty"`
}

type mcpPostMessageOutput struct {
	Message mcpMessageOutput `json:"message"`
}

type mcpListMessagesOutput struct {
	Messages  []mcpMessageOutput `json:"messages"`
	NextAfter int64              `json:"next_after,omitempty"`
}

// addAgentTool registers one MCP tool behind the capability gate (authz.go).
// Every tool must be registered through it: before the tool body runs, the
// tool's name is mapped to a capability and checked against the calling
// agent's manifest, and the body receives an agentScope, which is the only way
// it can reach a channel or an approval. A tool with no capability mapping is
// refused on every call.
func addAgentTool[In, Out any](s *Server, server *mcp.Server, identity mcpIdentity, tool *mcp.Tool,
	call func(ctx context.Context, scope *agentScope, in In) (*mcp.CallToolResult, Out, error),
) {
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		scope, serr := s.newAgentScope(ctx, identity, tool.Name)
		if serr != nil {
			var zero Out
			return mcpToolError(serr), zero, nil
		}
		return call(ctx, scope, in)
	})
}

// mcpServerFor builds the MCP server for one authenticated request.
func (s *Server) mcpServerFor(identity mcpIdentity) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "conchd", Version: s.cfg.Version}, nil)
	addAgentTool(s, server, identity, &mcp.Tool{Name: "post_message", Description: "Post a message to a Conch channel as the authenticated agent. Without an audience it goes to the whole channel; with one it goes only to a net or to the listed principals."},
		func(ctx context.Context, scope *agentScope, in mcpPostMessageInput) (*mcp.CallToolResult, mcpPostMessageOutput, error) {
			out, serr := s.postMessageMCP(ctx, scope, in)
			if serr != nil {
				return mcpToolError(serr), mcpPostMessageOutput{}, nil
			}
			message, err := mcpMessageFromSchema(out.Message)
			if err != nil {
				return nil, mcpPostMessageOutput{}, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("posted message %d", out.Message.ID)}}}, mcpPostMessageOutput{Message: message}, nil
		})
	addAgentTool(s, server, identity, &mcp.Tool{Name: "read_channel", Description: "Read one paginated page of messages from a Conch channel: those sent to the whole channel, and those with an audience the agent is in. A message with an audience field was not sent to everyone."},
		func(ctx context.Context, scope *agentScope, in mcpReadChannelInput) (*mcp.CallToolResult, mcpListMessagesOutput, error) {
			out, serr := s.readChannelMCP(ctx, scope, in)
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
	addAgentTool(s, server, identity, &mcp.Tool{Name: "request_approval", Description: "Raise an approval as the authenticated agent."},
		func(ctx context.Context, scope *agentScope, in mcpRequestApprovalInput) (*mcp.CallToolResult, mcpRequestApprovalOutput, error) {
			out, serr := s.requestApprovalMCP(ctx, scope, in)
			if serr != nil {
				return mcpToolError(serr), mcpRequestApprovalOutput{}, nil
			}
			projected := mcpRequestApprovalOutput{ID: out.ID, Title: out.Title, State: out.State, Deadline: out.Deadline.Time().Format(time.RFC3339Nano)}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("requested approval %d", out.ID)}}}, projected, nil
		})
	addAgentTool(s, server, identity, &mcp.Tool{Name: "await_decision", Description: "Wait for an approval resolution, polling persisted state. timeout_ms is clamped to a server-side maximum of 60000 ms."},
		func(ctx context.Context, scope *agentScope, in mcpAwaitDecisionInput) (*mcp.CallToolResult, mcpAwaitDecisionOutput, error) {
			out, serr := s.awaitDecisionMCP(ctx, scope, in)
			if serr != nil {
				return mcpToolError(serr), mcpAwaitDecisionOutput{}, nil
			}
			projected := mcpAwaitDecisionOutput{State: out.State, EffectiveTimeoutMS: out.EffectiveTimeoutMS}
			if out.Resolution != nil {
				projected.Resolution = out.Resolution
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("approval %d is %s", in.ApprovalID, out.State)}}}, projected, nil
		})
	addAgentTool(s, server, identity, &mcp.Tool{Name: "check_decision", Description: "Immediately read an approval's current persisted state and resolution, if terminal."},
		func(ctx context.Context, scope *agentScope, in mcpCheckDecisionInput) (*mcp.CallToolResult, mcpCheckDecisionOutput, error) {
			out, serr := s.checkDecisionMCP(ctx, scope, in)
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

func mcpMessageFromSchema(message schema.MessageV2) (mcpMessageOutput, error) {
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

func (s *Server) postMessageMCP(ctx context.Context, scope *agentScope, in mcpPostMessageInput) (schema.PostMessageResponseV2, *schema.Error) {
	authorID := scope.identity.principalID
	if strings.TrimSpace(in.Channel) == "" {
		return schema.PostMessageResponseV2{}, &schema.Error{Code: "invalid_request", Message: "channel must not be empty"}
	}
	if strings.TrimSpace(in.Body) == "" {
		return schema.PostMessageResponseV2{}, &schema.Error{Code: "invalid_request", Message: "body must not be empty"}
	}
	var schemaPayload *schema.Payload
	if in.Payload != nil {
		dataBytes, marshalErr := json.Marshal(in.Payload.Data)
		if marshalErr != nil {
			return schema.PostMessageResponseV2{}, &schema.Error{Code: "invalid_request", Message: "invalid payload data"}
		}
		schemaPayload = &schema.Payload{Schema: in.Payload.Schema, Data: json.RawMessage(dataBytes)}
	}
	var audience *schema.Audience
	if in.Audience != nil {
		// The same rules as the REST post, in the same order: shape first,
		// which says nothing about any channel or net.
		audience = &schema.Audience{Kind: schema.AudienceKind(in.Audience.Kind), NetID: in.Audience.NetID, PrincipalIDs: in.Audience.PrincipalIDs}
		if err := audience.Validate(); err != nil {
			return schema.PostMessageResponseV2{}, &schema.Error{Code: "invalid_audience", Message: err.Error()}
		}
		normalized := audience.Normalize(authorID)
		if normalized.Kind == schema.AudienceKindPrincipals && len(normalized.PrincipalIDs) < 2 {
			return schema.PostMessageResponseV2{}, &schema.Error{Code: "invalid_audience", Message: "a whisper needs at least one recipient other than the author"}
		}
		audience = &normalized
	}
	req := schema.PostMessageRequestV2{AuthorID: authorID, Body: in.Body, Payload: schemaPayload, Audience: audience}
	if err := req.Validate(); err != nil {
		return schema.PostMessageResponseV2{}, &schema.Error{Code: "invalid_request", Message: err.Error()}
	}
	if audience != nil {
		return s.postScopedMessageMCP(ctx, scope, in.Channel, in.Body, schemaPayload, *audience)
	}
	channel, serr := scope.channelByName(ctx, in.Channel, schema.ChannelPermissionPost)
	if serr != nil {
		return schema.PostMessageResponseV2{}, serr
	}
	stored, err := channel.insertMessage(ctx, in.Body, schemaPayload)
	if err != nil {
		slog.ErrorContext(ctx, "mcp: insert failed", "error", err)
		return schema.PostMessageResponseV2{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	messageV1 := messageV1FromStore(stored)
	s.hub.BroadcastMessageV1(ctx, messageV1)
	messageV0 := messageV0FromStore(stored)
	s.hub.BroadcastMessage(ctx, messageV0)
	s.broadcaster.BroadcastMessage(ctx, messageV0)
	return schema.PostMessageResponseV2{Message: schema.MessageV2FromV1(messageV1)}, nil
}

// postScopedMessageMCP posts a message with an audience. The scope decides
// whether the agent may address that audience in that channel, and the grant
// it returns carries the audience, so what is stored is what was authorized.
// The message goes to its recipients' v2 sockets only: never to the v0 or v1
// broadcasts or the Broadcaster seam, which cannot carry an audience.
func (s *Server) postScopedMessageMCP(ctx context.Context, scope *agentScope, channelName, body string, payload *schema.Payload, audience schema.Audience) (schema.PostMessageResponseV2, *schema.Error) {
	channel, serr := scope.channelForAudience(ctx, channelName, audience)
	if serr != nil {
		return schema.PostMessageResponseV2{}, serr
	}
	message, recipients, serr := channel.insertScopedMessage(ctx, body, payload)
	if serr != nil {
		return schema.PostMessageResponseV2{}, serr
	}
	s.hub.BroadcastMessageV2(ctx, message, recipients)
	return schema.PostMessageResponseV2{Message: message}, nil
}

func (s *Server) readChannelMCP(ctx context.Context, scope *agentScope, in mcpReadChannelInput) (schema.ListMessagesResponseV2, *schema.Error) {
	if strings.TrimSpace(in.Channel) == "" {
		return schema.ListMessagesResponseV2{}, &schema.Error{Code: "invalid_request", Message: "channel must not be empty"}
	}
	if in.After < 0 {
		return schema.ListMessagesResponseV2{}, &schema.Error{Code: "invalid_request", Message: "after must be a non-negative integer"}
	}
	limit := in.Limit
	var limitVal int64
	if limit == nil {
		limitVal = defaultMessageLimit
	} else if *limit <= 0 || *limit > maxMessageLimit {
		return schema.ListMessagesResponseV2{}, &schema.Error{Code: "invalid_request", Message: "limit must be between 1 and 100"}
	} else {
		limitVal = *limit
	}
	channel, serr := scope.channelByName(ctx, in.Channel, schema.ChannelPermissionRead)
	if serr != nil {
		return schema.ListMessagesResponseV2{}, serr
	}
	stored, err := channel.listMessages(ctx, in.After, int(limitVal)+1)
	if err != nil {
		slog.ErrorContext(ctx, "mcp: list failed", "error", err)
		return schema.ListMessagesResponseV2{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	nextAfter := int64(0)
	if len(stored) > int(limitVal) {
		stored = stored[:int(limitVal)]
		nextAfter = stored[len(stored)-1].ID
	}
	messages := make([]schema.MessageV2, len(stored))
	for i, message := range stored {
		messages[i] = messageV2FromStore(message)
	}
	return schema.ListMessagesResponseV2{Messages: messages, NextAfter: nextAfter}, nil
}

func (s *Server) requestApprovalMCP(ctx context.Context, scope *agentScope, in mcpRequestApprovalInput) (schema.RequestApprovalOutput, *schema.Error) {
	requesterID := scope.identity.principalID
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
	// Raising an approval writes into the channel, so it needs the post
	// permission there (agent-manifest.md).
	channel, serr := scope.channelByID(ctx, in.ChannelID, schema.ChannelPermissionPost)
	if serr != nil {
		return schema.RequestApprovalOutput{}, serr
	}
	created, err := channel.createApproval(ctx, req, deadline)
	if errors.Is(err, approvals.ErrInvalid) {
		return schema.RequestApprovalOutput{}, &schema.Error{Code: "invalid_request", Message: err.Error()}
	}
	if err != nil {
		slog.ErrorContext(ctx, "mcp: create approval failed", "error", err)
		return schema.RequestApprovalOutput{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
	}
	return schema.RequestApprovalOutput{ID: created.ID, Title: created.Title, State: created.State, Deadline: schema.NewTimestamp(created.Deadline)}, nil
}

func (s *Server) checkDecisionMCP(ctx context.Context, scope *agentScope, in mcpCheckDecisionInput) (schema.CheckDecisionOutput, *schema.Error) {
	if in.ApprovalID <= 0 {
		return schema.CheckDecisionOutput{}, &schema.Error{Code: "invalid_request", Message: "approval_id must be a positive integer"}
	}
	// Observing an approval reads from its channel, so it needs the read
	// permission there. The scope re-checks membership on every call, which
	// includes every poll of await_decision.
	approval, serr := scope.approval(ctx, in.ApprovalID, schema.ChannelPermissionRead)
	if serr != nil {
		return schema.CheckDecisionOutput{}, serr
	}
	out := schema.CheckDecisionOutput{State: approval.state()}
	if approval.state().IsTerminal() {
		resolution, err := approval.resolution(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "mcp: read approval resolution failed", "error", err)
			return schema.CheckDecisionOutput{}, &schema.Error{Code: "internal_error", Message: "internal server error"}
		}
		out.Resolution = &resolution
	}
	return out, nil
}

func (s *Server) awaitDecisionMCP(ctx context.Context, scope *agentScope, in mcpAwaitDecisionInput) (schema.AwaitDecisionOutput, *schema.Error) {
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
	return s.pollDecision(ctx, waitCtx, scope, in.ApprovalID, effectiveMS)
}

// pollDecision polls an approval until it is terminal or waitCtx ends. ctx is
// the tool call; waitCtx is the wait's own deadline. The first authorized
// check runs even when waitCtx is already done, so no timeout — however short
// — can produce an answer for an approval the agent may not observe.
func (s *Server) pollDecision(ctx, waitCtx context.Context, scope *agentScope, approvalID, effectiveMS int64) (schema.AwaitDecisionOutput, *schema.Error) {
	ticker := time.NewTicker(mcpPollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return schema.AwaitDecisionOutput{}, &schema.Error{Code: "request_cancelled", Message: "tool call cancelled"}
		}
		// Every poll is authorized afresh, and the first one always runs,
		// however short the timeout: the credential and principal must still
		// be usable, the manifest is read again, and checkDecisionMCP re-checks
		// membership and the channel permission. So revoking the credential,
		// disabling the agent, changing its manifest, or removing it from the
		// channel ends an in-flight wait on its next poll.
		if serr := scope.refresh(ctx); serr != nil {
			return schema.AwaitDecisionOutput{}, serr
		}
		checked, serr := s.checkDecisionMCP(ctx, scope, mcpCheckDecisionInput{ApprovalID: approvalID})
		if serr != nil {
			return schema.AwaitDecisionOutput{}, serr
		}
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
