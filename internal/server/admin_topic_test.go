package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// TestAdminSetChannelTopic (issue #1523, docs/design/channel-topic.md §4.3):
// describe_admin_actions lists set_channel_topic as manage_channels and not
// gated; an agent holding manage_channels sets and clears a channel's topic
// through the door, and is the audit actor; one without it is refused; and
// params that are not exactly {"topic": …} are refused before anything runs.
func TestAdminSetChannelTopic(t *testing.T) {
	ctx := context.Background()
	d := newDoorFixture(t, schema.PermManageChannels)
	channel, err := d.srv.store.CreateChannel(ctx, "plans")
	if err != nil {
		t.Fatal(err)
	}
	target := &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: channel.ID}

	// The catalog, as the agent reads it.
	rec := d.do(t, "GET", "/v1/admin/actions", d.agentTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/admin/actions = %d %s", rec.Code, rec.Body)
	}
	var found bool
	for _, e := range decodeBody[schema.AdminCatalogResponseV1](t, rec).Actions {
		if e.Action != schema.AdminActionSetChannelTopic {
			continue
		}
		found = true
		if e.Permission != schema.PermManageChannels || e.Gate != schema.AdminGateDirect || e.Gated || !e.Held || e.Route != "PUT /v1/channels/{id}/topic" {
			t.Errorf("catalog entry = %+v, want manage_channels, direct, not gated, held, PUT /v1/channels/{id}/topic", e)
		}
	}
	if !found {
		t.Fatal("describe_admin_actions does not list set_channel_topic")
	}

	topicOf := func(t *testing.T) string {
		t.Helper()
		ch, err := d.srv.store.ChannelByName(ctx, "plans")
		if err != nil {
			t.Fatal(err)
		}
		return ch.Topic
	}
	agentActor := fmt.Sprintf("principal:%d", d.agent.ID)
	changedBy := func(t *testing.T) []string {
		t.Helper()
		var actors []string
		for _, e := range d.audit(t) {
			if e.Action == store.AuditChannelTopicChanged && e.Subject == fmt.Sprintf("channel:%d", channel.ID) {
				actors = append(actors, e.Actor)
			}
		}
		return actors
	}

	// Set, then clear: each runs at once, and the agent is the actor.
	for i, step := range []struct{ params, want string }{
		{`{"topic":"Release plans"}`, "Release plans"},
		{`{"topic":""}`, ""},
	} {
		rec := d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionSetChannelTopic, target, step.params, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("submit %s = %d %s", step.params, rec.Code, rec.Body)
		}
		res := decodeBody[schema.AdminActionResultV1](t, rec)
		if res.Status != schema.AdminActionRan || res.HTTPStatus != http.StatusOK || res.Validate() != nil {
			t.Fatalf("submit %s result = %+v", step.params, res)
		}
		if got := topicOf(t); got != step.want {
			t.Fatalf("after %s the topic is %q, want %q", step.params, got, step.want)
		}
		actors := changedBy(t)
		if len(actors) != i+1 || actors[i] != agentActor {
			t.Fatalf("channel_topic_changed actors = %v, want %d rows by %s", actors, i+1, agentActor)
		}
	}

	// Params that are not exactly {"topic": …}, or a topic the schema
	// refuses, are refused at submit; nothing is written.
	before := len(changedBy(t))
	for _, params := range []string{`{}`, `{"topic":null}`, `{"topic":"x","from":"y"}`, `{"name":"x"}`, `{"topic":"a\u0007b"}`} {
		rec := d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionSetChannelTopic, target, params, ""))
		assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
	}
	if got := len(changedBy(t)); got != before {
		t.Fatalf("a refused submit wrote %d topic rows", got-before)
	}

	// An agent without manage_channels is refused, by the permission.
	other := newDoorFixture(t, schema.PermManageMembers)
	ch2, err := other.srv.store.CreateChannel(ctx, "plans")
	if err != nil {
		t.Fatal(err)
	}
	rec = other.do(t, "POST", "/v1/admin/actions", other.agentTok, submitBody(schema.AdminActionSetChannelTopic, &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: ch2.ID}, `{"topic":"mine"}`, ""))
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodePermissionDenied)
	if e := decodeBody[schema.Error](t, rec); e.Permission != schema.PermManageChannels {
		t.Errorf("refusal names %q, want manage_channels", e.Permission)
	}
	if ch, _ := other.srv.store.ChannelByName(ctx, "plans"); ch.Topic != "" {
		t.Fatalf("a refused agent set the topic to %q", ch.Topic)
	}
}

// TestAdminSetChannelTopicRefusalsAndGating (security review of #1710): the
// agent's own approval channel is refused at the door, and an owner who adds
// set_channel_topic to the agent's require_approval gets an approval whose
// payload carries exactly the topic, with nothing changed until it runs.
func TestAdminSetChannelTopicRefusalsAndGating(t *testing.T) {
	ctx := context.Background()
	d := newDoorFixture(t, schema.PermManageChannels)
	approvalTarget := &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: d.approvalCh.ID}
	assertErrorBody(t, d.do(t, "POST", "/v1/admin/actions", d.agentTok, submitBody(schema.AdminActionSetChannelTopic, approvalTarget, `{"topic":"mine now"}`, "")),
		http.StatusForbidden, schema.ErrorCodeAdminActionNotAllowed)
	if ch, err := d.srv.store.ChannelByName(ctx, d.approvalCh.Name); err != nil || ch.Topic != "" {
		t.Fatalf("approval channel topic = %q (%v), want none", ch.Topic, err)
	}

	// Gated per agent: the owner requires approval for it.
	req := schema.PutAgentManifestRequestV1{
		DisplayName:  "helper",
		Tier:         schema.AgentTierA,
		Capabilities: []schema.Capability{schema.CapabilityAdminAct, schema.CapabilityMessagesRead},
		Channels: []schema.ChannelGrant{{ChannelID: d.approvalCh.ID, Permissions: []schema.ChannelPermission{
			schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		}}},
		Permissions: []schema.Permission{schema.PermManageChannels},
		Admin:       &schema.AgentAdminV1{ApprovalChannelID: d.approvalCh.ID, RequireApproval: []schema.AdminAction{schema.AdminActionSetChannelTopic}},
	}
	if _, _, err := d.srv.store.PutAgentManifest(ctx, "system", d.agent.ID, req); err != nil {
		t.Fatal(err)
	}
	channel, err := d.srv.store.CreateChannel(ctx, "plans")
	if err != nil {
		t.Fatal(err)
	}
	target := &schema.AdminTargetV1{Kind: schema.AdminTargetChannel, ID: channel.ID}
	id := d.submitGated(t, submitBody(schema.AdminActionSetChannelTopic, target, `{"topic":"Release plans"}`, "the team asked"))
	approval, err := d.srv.store.ApprovalByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Payload == nil || approval.Payload.Schema != schema.AdminActionV1Name {
		t.Fatalf("approval payload = %+v, want %s", approval.Payload, schema.AdminActionV1Name)
	}
	var action schema.AdminActionV1
	if err := json.Unmarshal(approval.Payload.Data, &action); err != nil {
		t.Fatal(err)
	}
	if action.Action != schema.AdminActionSetChannelTopic || string(action.Params) != `{"topic":"Release plans"}` {
		t.Fatalf("payload action %s params %s, want set_channel_topic {\"topic\":\"Release plans\"}", action.Action, action.Params)
	}
	if ch, _ := d.srv.store.ChannelByName(ctx, "plans"); ch.Topic != "" {
		t.Fatalf("the topic changed before the approval: %q", ch.Topic)
	}
	d.decide(t, id, "run")
	if rec := d.run(t, id, d.agentTok); rec.Code != http.StatusOK {
		t.Fatalf("run = %d %s", rec.Code, rec.Body)
	}
	if ch, _ := d.srv.store.ChannelByName(ctx, "plans"); ch.Topic != "Release plans" {
		t.Fatalf("after the approved run the topic is %q", ch.Topic)
	}
}
