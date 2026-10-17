// Package mcpclient provides Conch's small streamable-HTTP MCP client.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/njdaniel/conch/pkg/schema"
)

// Client calls a conchd MCP endpoint.
type Client struct {
	baseURL   string
	token     string
	http      *http.Client
	sessionID string
	nextID    int
	mu        sync.Mutex
}

// New constructs a client that refuses redirects. Go re-sends the
// Authorization header to a redirect target on the same hostname, even on
// another port or from https to http, so following one would hand the agent's
// bearer to whatever a misconfigured front proxy points at. A redirect comes
// back as the 3xx response and surfaces as a status error.
//
// There is no client timeout: await_decision is a long poll, and every call
// is already bounded by its caller's context.
func New(baseURL, token string) *Client {
	return &Client{baseURL: baseURL, token: token, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context, clientName string) error {
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": clientName, "version": "1"},
		"capabilities":    map[string]any{},
	})
	return err
}

// CallTool calls any named MCP tool and returns its structured content.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (json.RawMessage, error) {
	return c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
}

// ReadChannel reads one page from the read_channel tool. The messages are v2
// envelopes: one with an Audience was not sent to the whole channel.
func (c *Client) ReadChannel(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV2, error) {
	arguments := map[string]any{"channel": channel}
	if after != 0 {
		arguments["after"] = after
	}
	if limit != 0 {
		arguments["limit"] = limit
	}
	raw, err := c.CallTool(ctx, "read_channel", arguments)
	if err != nil {
		return schema.ListMessagesResponseV2{}, err
	}
	return Decode[schema.ListMessagesResponseV2](raw)
}

// PostMessage posts an untyped, channel-wide message through the
// post_message tool.
func (c *Client) PostMessage(ctx context.Context, channel, body string) (schema.PostMessageResponseV2, error) {
	return c.PostMessageTo(ctx, channel, body, nil)
}

// PostMessageTo posts an untyped message through the post_message tool to the
// given audience, which is sent as is; nil posts channel-wide. A refusal by
// the server comes back as a *ToolError.
func (c *Client) PostMessageTo(ctx context.Context, channel, body string, audience *schema.Audience) (schema.PostMessageResponseV2, error) {
	arguments := map[string]any{"channel": channel, "body": body}
	if audience != nil {
		arguments["audience"] = audience
	}
	raw, err := c.CallTool(ctx, "post_message", arguments)
	if err != nil {
		return schema.PostMessageResponseV2{}, err
	}
	return Decode[schema.PostMessageResponseV2](raw)
}

// ToolError is a tool call the server answered with isError. Code is the
// schema.Error code when the text carries one (for example forbidden or
// net_not_found), so a caller can tell a refusal from a transport failure.
type ToolError struct {
	Method  string
	Code    string
	Message string
	// RetryAfterSeconds is the server's retry delay when the refusal is one
	// that waiting cures (rate_limited), 0 otherwise.
	RetryAfterSeconds int
	text              string
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("mcp %s tool error: %s", e.Method, e.text)
}

func newToolError(method, text string) *ToolError {
	e := &ToolError{Method: method, text: text, Message: text}
	var parsed schema.Error
	if json.Unmarshal([]byte(text), &parsed) == nil && parsed.Code != "" {
		e.Code, e.Message, e.RetryAfterSeconds = parsed.Code, parsed.Message, parsed.RetryAfterSeconds
	}
	return e
}

// Decode unmarshals a tool's structured content into a schema type.
func Decode[T any](raw json.RawMessage) (T, error) {
	var out T
	err := json.Unmarshal(raw, &out)
	return out, err
}

func (c *Client) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	sessionID := c.sessionID
	c.mu.Unlock()

	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// A response body can hold message bodies, scoped ones included, and
	// callers log these errors: an error never quotes a tool result, and
	// quotes only the start of a refusal from the HTTP layer.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp %s status %d: %s", method, resp.StatusCode, excerpt(respBody))
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode mcp %s response (%d bytes): %w", method, len(respBody), err)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("mcp %s rpc error: %s", method, envelope.Error.Message)
	}
	if envelope.Result.IsError {
		var text string
		if len(envelope.Result.Content) > 0 {
			text = envelope.Result.Content[0].Text
		}
		return nil, newToolError(method, text)
	}
	return envelope.Result.StructuredContent, nil
}

// excerpt is the first line of an HTTP error body, cut to 120 bytes.
func excerpt(body []byte) string {
	const limit = 120
	line, _, _ := strings.Cut(strings.TrimSpace(string(body)), "\n")
	if len(line) > limit {
		return line[:limit] + "…"
	}
	return line
}

// validator is a schema document that can check its own structure.
type validator interface{ Validate() error }

// decodeValid decodes a tool's structured content into a schema document and
// checks it, so a caller never acts on a document the schema would refuse.
func decodeValid[T validator](tool string, raw json.RawMessage) (T, error) {
	out, err := Decode[T](raw)
	if err != nil {
		return out, fmt.Errorf("decode %s result: %w", tool, err)
	}
	if err := out.Validate(); err != nil {
		return out, fmt.Errorf("%s result: %w", tool, err)
	}
	return out, nil
}

// arguments turns a schema request into tool arguments, with the fields the
// request does not carry (the id of the thing it is about) added. The request
// is encoded as the schema writes it, so the shape never differs from REST's.
func arguments(request any, extra map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(fields)+len(extra))
	for name, value := range fields {
		out[name] = value
	}
	for name, value := range extra {
		out[name] = value
	}
	return out, nil
}

// RegisterSession registers this runtime as a session of the agent.
func (c *Client) RegisterSession(ctx context.Context, req schema.CreateSessionRequestV1) (schema.SessionV1, error) {
	if err := req.Validate(); err != nil {
		return schema.SessionV1{}, err
	}
	args, err := arguments(req, nil)
	if err != nil {
		return schema.SessionV1{}, err
	}
	raw, err := c.CallTool(ctx, "register_session", args)
	if err != nil {
		return schema.SessionV1{}, err
	}
	return decodeValid[schema.SessionV1]("register_session", raw)
}

// Heartbeat keeps one of the agent's own sessions alive.
func (c *Client) Heartbeat(ctx context.Context, sessionID int64) (schema.SessionV1, error) {
	raw, err := c.CallTool(ctx, "heartbeat", map[string]any{"session_id": sessionID})
	if err != nil {
		return schema.SessionV1{}, err
	}
	return decodeValid[schema.SessionV1]("heartbeat", raw)
}

// EndSession ends one of the agent's own sessions.
func (c *Client) EndSession(ctx context.Context, sessionID int64) error {
	_, err := c.CallTool(ctx, "end_session", map[string]any{"session_id": sessionID})
	return err
}

// StartRun starts a run on one of the agent's own sessions.
func (c *Client) StartRun(ctx context.Context, sessionID int64, req schema.CreateRunRequestV1) (schema.RunV1, error) {
	if err := req.Validate(); err != nil {
		return schema.RunV1{}, err
	}
	args, err := arguments(req, map[string]any{"session_id": sessionID})
	if err != nil {
		return schema.RunV1{}, err
	}
	raw, err := c.CallTool(ctx, "start_run", args)
	if err != nil {
		return schema.RunV1{}, err
	}
	return decodeValid[schema.RunV1]("start_run", raw)
}

// UpdateRun moves one of the agent's own runs to another state.
func (c *Client) UpdateRun(ctx context.Context, runID int64, req schema.SetRunStateRequestV1) (schema.RunV1, error) {
	if err := req.Validate(); err != nil {
		return schema.RunV1{}, err
	}
	args, err := arguments(req, map[string]any{"run_id": runID})
	if err != nil {
		return schema.RunV1{}, err
	}
	raw, err := c.CallTool(ctx, "update_run", args)
	if err != nil {
		return schema.RunV1{}, err
	}
	return decodeValid[schema.RunV1]("update_run", raw)
}

// PostActivity adds one entry to the activity stream of one of the agent's own
// runs.
func (c *Client) PostActivity(ctx context.Context, runID int64, req schema.AppendActivityRequestV1) (schema.AgentActivityV1, error) {
	if err := req.Validate(); err != nil {
		return schema.AgentActivityV1{}, err
	}
	args, err := arguments(req, map[string]any{"run_id": runID})
	if err != nil {
		return schema.AgentActivityV1{}, err
	}
	raw, err := c.CallTool(ctx, "post_activity", args)
	if err != nil {
		return schema.AgentActivityV1{}, err
	}
	return decodeValid[schema.AgentActivityV1]("post_activity", raw)
}

// PollInterventions reads the interventions made on the runs of one of the
// agent's own sessions, oldest first, in the given state (empty for any) and
// after the given id.
func (c *Client) PollInterventions(ctx context.Context, sessionID int64, state schema.InterventionState, after int64) (schema.ListInterventionsResponseV1, error) {
	args := map[string]any{"session_id": sessionID}
	if state != "" {
		args["state"] = state
	}
	if after > 0 {
		args["after"] = after
	}
	raw, err := c.CallTool(ctx, "poll_interventions", args)
	if err != nil {
		return schema.ListInterventionsResponseV1{}, err
	}
	list, err := Decode[schema.ListInterventionsResponseV1](raw)
	if err != nil {
		return list, fmt.Errorf("decode poll_interventions result: %w", err)
	}
	for _, iv := range list.Interventions {
		if err := iv.Validate(); err != nil {
			return schema.ListInterventionsResponseV1{}, fmt.Errorf("poll_interventions result: %w", err)
		}
	}
	return list, nil
}

// AnswerIntervention answers an intervention on one of the agent's own runs.
func (c *Client) AnswerIntervention(ctx context.Context, interventionID int64, req schema.AckInterventionRequestV1) (schema.InterventionV1, error) {
	if err := req.Validate(); err != nil {
		return schema.InterventionV1{}, err
	}
	args, err := arguments(req, map[string]any{"intervention_id": interventionID})
	if err != nil {
		return schema.InterventionV1{}, err
	}
	raw, err := c.CallTool(ctx, "ack_intervention", args)
	if err != nil {
		return schema.InterventionV1{}, err
	}
	return decodeValid[schema.InterventionV1]("ack_intervention", raw)
}

// RequestApproval raises an approval through the request_approval tool. The
// requester is the authenticated agent: the server sets it, so the field is
// not sent (the request carries it only so that it validates as a whole).
func (c *Client) RequestApproval(ctx context.Context, req schema.CreateApprovalRequestV1) (schema.RequestApprovalOutput, error) {
	if err := req.Validate(); err != nil {
		return schema.RequestApprovalOutput{}, err
	}
	args, err := arguments(req, nil)
	if err != nil {
		return schema.RequestApprovalOutput{}, err
	}
	delete(args, "requester_id")
	raw, err := c.CallTool(ctx, "request_approval", args)
	if err != nil {
		return schema.RequestApprovalOutput{}, err
	}
	out, err := Decode[schema.RequestApprovalOutput](raw)
	if err != nil {
		return out, fmt.Errorf("decode request_approval result: %w", err)
	}
	if out.ID <= 0 || !out.State.Valid() {
		return schema.RequestApprovalOutput{}, fmt.Errorf("request_approval result: id %d, state %q", out.ID, out.State)
	}
	return out, nil
}

// AwaitDecision waits up to timeoutMS (the server clamps it) for an approval to
// resolve.
func (c *Client) AwaitDecision(ctx context.Context, approvalID, timeoutMS int64) (schema.AwaitDecisionOutput, error) {
	raw, err := c.CallTool(ctx, "await_decision", map[string]any{"approval_id": approvalID, "timeout_ms": timeoutMS})
	if err != nil {
		return schema.AwaitDecisionOutput{}, err
	}
	out, err := Decode[schema.AwaitDecisionOutput](raw)
	if err != nil {
		return out, fmt.Errorf("decode await_decision result: %w", err)
	}
	if !out.State.Valid() {
		return schema.AwaitDecisionOutput{}, fmt.Errorf("await_decision result: state %q", out.State)
	}
	if out.Resolution != nil {
		if err := out.Resolution.Validate(); err != nil {
			return schema.AwaitDecisionOutput{}, fmt.Errorf("await_decision result: %w", err)
		}
	}
	return out, nil
}
