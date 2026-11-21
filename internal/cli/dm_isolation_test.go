package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The stacked schema branch has no DM handlers. Exercise the real MCP and
// admin tool surfaces to pin that an issued agent can use its channel grant
// but can neither invoke DM operations nor dispatch them as admin actions.
func TestDMMCPIsolation(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	agent, err := st.CreatePrincipal(ctx, store.PrincipalAgent, "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := st.CreateCredential(ctx, "system", agent.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := st.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddChannelMember(ctx, "system", channel.ID, agent.ID, 0); err != nil {
		t.Fatal(err)
	}
	req := schema.PutAgentManifestRequestV1{
		DisplayName: "test-agent", Tier: schema.AgentTierA,
		Capabilities: []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityAdminAct},
		Channels:     []schema.ChannelGrant{{ChannelID: channel.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}},
	}
	if _, _, err := st.PutAgentManifest(ctx, "system", agent.ID, req); err != nil {
		t.Fatal(err)
	}
	handler := server.New(server.Config{DataDir: t.TempDir(), Version: "test"}, st).Handler()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	sdk := mcp.NewClient(&mcp.Implementation{Name: "dm-isolation-test", Version: "test"}, nil)
	httpClient := srv.Client()
	httpClient.Transport = dmAgentTransport{token: token, base: httpClient.Transport}
	session, err := sdk.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: httpClient, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_channel", Arguments: map[string]any{"channel": channel.Name},
	})
	if err != nil || result.IsError {
		t.Fatalf("agent could not read its granted channel: %+v, %v", result, err)
	}
	for _, tt := range []struct {
		tool, route string
		args        map[string]any
	}{
		{"list_dms", "GET /v1/dms", map[string]any{}},
		{"read_dm", "GET /v1/dms/1/messages", map[string]any{"conversation_id": 1}},
		{"open_dm", "POST /v1/dms", map[string]any{"peer_id": 2}},
		{"send_dm", "POST /v1/dms/1/messages", map[string]any{"conversation_id": 1, "author_id": 2, "body": "hello"}},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tt.tool, Arguments: tt.args})
			if err == nil || !strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("DM tool %s should be absent: %v", tt.tool, err)
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name: "submit_admin_action",
				Arguments: map[string]any{
					"action": tt.route, "params": tt.args, "reason": "DM isolation test",
				},
			})
			if err != nil || !result.IsError {
				t.Fatalf("DM route accepted by admin dispatch: %+v, %v", result, err)
			}
			body, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var refusal schema.Error
			if err := json.Unmarshal(body, &refusal); err != nil || refusal.Code != schema.ErrorCodeAdminActionUnknown {
				t.Fatalf("want unknown admin action, got %s, %v", body, err)
			}
		})
	}
}

type dmAgentTransport struct {
	token string
	base  http.RoundTripper
}

func (transport dmAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header.Set("Authorization", "Bearer "+transport.token)
	return transport.base.RoundTrip(req)
}
