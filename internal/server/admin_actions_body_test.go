package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestAdminDispatchedBodies (issue #1461): the four builders that dispatch a
// body now marshal the pkg/schema request type their route decodes, and the
// bytes are exactly what the anonymous structs sent before. A rights field in
// add_channel_member's params is still refused, and its body names only the
// member.
func TestAdminDispatchedBodies(t *testing.T) {
	channel := &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: 2, Name: "old"}
	tests := []struct {
		name    string
		action  schema.AdminAction
		target  *schema.AdminTargetV1
		params  string
		want    string
		refused bool
	}{
		{"create a member", schema.AdminActionCreateMember, nil, `{"name":"x"}`, `{"kind":"human","name":"x"}`, false},
		{"rename a channel", schema.AdminActionRenameChannel, channel, `{"name":"new"}`, `{"name":"new","from":"old"}`, false},
		{"add a member", schema.AdminActionAddChannelMember, channel, `{"principal_id":7}`, `{"id":7}`, false},
		{"add a member with rights", schema.AdminActionAddChannelMember, channel, `{"principal_id":7,"can_decide":true}`, "", true},
		// The refusal of a zero id is what keeps id,omitempty from sending {}.
		{"add member zero", schema.AdminActionAddChannelMember, channel, `{"principal_id":0}`, "", true},
		{"rights: decides", schema.AdminActionSetChannelMemberRights, channel, `{"principal_id":7,"can_decide":true}`, `{"can_decide":true}`, false},
		{"rights: manages", schema.AdminActionSetChannelMemberRights, channel, `{"principal_id":7,"manages":false}`, `{"manages":false}`, false},
		{"rights: both", schema.AdminActionSetChannelMemberRights, channel, `{"principal_id":7,"can_decide":false,"manages":true}`, `{"can_decide":false,"manages":true}`, false},
		// set_channel_topic (issue #1523): exactly {"topic":…}; "" clears it,
		// a missing topic is not a clear, and the topic is validated here.
		{"set a topic", schema.AdminActionSetChannelTopic, channel, `{"topic":"Incidents"}`, `{"topic":"Incidents"}`, false},
		{"clear the topic", schema.AdminActionSetChannelTopic, channel, `{"topic":""}`, `{"topic":""}`, false},
		{"topic missing", schema.AdminActionSetChannelTopic, channel, `{}`, "", true},
		{"topic null", schema.AdminActionSetChannelTopic, channel, `{"topic":null}`, "", true},
		{"topic with another field", schema.AdminActionSetChannelTopic, channel, `{"topic":"x","from":"y"}`, "", true},
		{"topic with a control character", schema.AdminActionSetChannelTopic, channel, `{"topic":"a\u0007b"}`, "", true},
		{"topic too long", schema.AdminActionSetChannelTopic, channel, `{"topic":"` + strings.Repeat("x", schema.MaxChannelTopicLength+1) + `"}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := adminCatalogByAction[tt.action]
			if !ok {
				t.Fatalf("no catalog entry for %s", tt.action)
			}
			ctx := context.WithValue(context.Background(), adminBuildingKey{}, entry)
			req, _, refusal := entry.build(ctx, nil, adminBuildInput{target: tt.target, params: json.RawMessage(tt.params)})
			if tt.refused {
				if refusal == nil {
					t.Fatalf("params %s were accepted, body %s", tt.params, req.body)
				}
				return
			}
			if refusal != nil {
				t.Fatalf("refused: %+v", refusal)
			}
			if string(req.body) != tt.want {
				t.Errorf("dispatched body = %s, want %s", req.body, tt.want)
			}
		})
	}
}
