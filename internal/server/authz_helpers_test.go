package server

import (
	"context"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// setAgentManifest writes an agent manifest granting caps (every capability
// when caps is nil) with read and post in each of channelIDs. It does not
// touch channel membership.
func setAgentManifest(t *testing.T, srv *Server, agentID int64, caps []schema.Capability, channelIDs ...int64) {
	t.Helper()
	if caps == nil {
		caps = schema.Capabilities()
	}
	req := schema.PutAgentManifestRequestV1{DisplayName: "test agent", Tier: schema.AgentTierC, Capabilities: caps}
	for _, id := range channelIDs {
		req.Channels = append(req.Channels, schema.ChannelGrant{
			ChannelID:   id,
			Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost},
		})
	}
	if _, _, err := srv.store.PutAgentManifest(context.Background(), "system", agentID, req); err != nil {
		t.Fatalf("PutAgentManifest: %v", err)
	}
}

// grantAgent gives an agent everything it needs to use one channel through
// both gates: membership, and a manifest with every capability plus read and
// post there.
func grantAgent(t *testing.T, srv *Server, agentID, channelID int64) {
	t.Helper()
	if _, err := srv.store.AddChannelMember(context.Background(), "system", channelID, agentID, 0); err != nil {
		t.Fatalf("AddChannelMember: %v", err)
	}
	setAgentManifest(t, srv, agentID, nil, channelID)
}
