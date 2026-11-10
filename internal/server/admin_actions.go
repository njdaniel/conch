package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// errAdminPayloadAuthored is the refusal of an approval create that carries
// the admin door's payload (F1 of #1328's review; ai-administrators §3.5.2).
const errAdminPayloadAuthored = "the payload schema conch.admin_action.v1 is the admin door's: no client may author it"

// adminPayloadAuthored reports whether a client-supplied approval payload
// claims to be the admin door's. Only the door composes it.
func adminPayloadAuthored(p *schema.Payload) bool {
	return p != nil && p.Schema == schema.AdminActionV1Name
}

// The action catalog (ai-administrators §3.3): every action an AI
// administrator may take, each one existing route with the permission it
// needs, its default gate and what it targets. The structural test
// (TestAdminCatalogMatchesRouteTable) keeps it honest against the route
// table: every pattern exists, an accessPermission route needs the
// catalog's permission, and no wrapper admits dispatch on a route outside it.

// adminBuildInput is what a builder has: the caller, the target with its
// current name, and the agent's params.
type adminBuildInput struct {
	caller adminCaller
	target *schema.AdminTargetV1
	params json.RawMessage
}

// adminTouches is what a built action touches beyond its target: the
// channels it changes (the approval channel is refused among them), the
// second entity of a channel-member action, and the params as clamped,
// which a gated action stores and shows.
type adminTouches struct {
	channels []int64
	member   int64
	params   json.RawMessage
}

type adminBuilder func(ctx context.Context, s *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal)

// adminCatalogEntry is one catalog row.
type adminCatalogEntry struct {
	action     schema.AdminAction
	pattern    string
	permission schema.Permission
	gate       schema.AdminGate
	target     schema.AdminTargetKind
	build      adminBuilder
	// changesTarget: the action changes the channel it targets, so it is
	// refused on the agent's approval channel (§3.5.6).
	changesTarget bool
}

const (
	direct   = schema.AdminGateDirect
	approval = schema.AdminGateApproval

	tPrincipal  = schema.AdminTargetPrincipal
	tChannel    = schema.AdminTargetChannel
	tCredential = schema.AdminTargetCredential
	tHook       = schema.AdminTargetHook
	tInvite     = schema.AdminTargetInvite
	tReport     = schema.AdminTargetReport

	pMembers  = schema.PermManageMembers
	pChannels = schema.PermManageChannels
	pLinks    = schema.PermMakeMemberLinks
	pSettings = schema.PermChangeServerSettings
	pAudit    = schema.PermReadAuditLog
)

// Member links an AI administrator makes are clamped (§3.3).
const (
	adminLinkMaxUses   = 5
	adminLinkMaxExpiry = 7 * 24 * time.Hour
)

var adminCatalog = []adminCatalogEntry{
	{action: schema.AdminActionListMembers, pattern: "GET /v1/principals", permission: pMembers, gate: direct,
		build: queryParams("kind", "role", "state", "invite_id", "guest", "name_prefix", "before", "limit")},
	{action: schema.AdminActionCreateMember, pattern: "POST /v0/principals", permission: pMembers, gate: direct, build: buildCreateMember},
	{action: schema.AdminActionRenameMember, pattern: "PUT /v1/principals/{id}/name", permission: pMembers, gate: direct, target: tPrincipal, build: passBody("")},
	{action: schema.AdminActionSetVoicePermission, pattern: "PUT /v1/principals/{id}/voice-permission", permission: pMembers, gate: direct, target: tPrincipal, build: passBody("")},
	{action: schema.AdminActionSetInvitePermission, pattern: "PUT /v1/principals/{id}/invite-permission", permission: pMembers, gate: direct, target: tPrincipal, build: passBody("")},
	{action: schema.AdminActionDisableMember, pattern: "POST /v1/principals/{id}/disable", permission: pMembers, gate: approval, target: tPrincipal, build: noBody},
	{action: schema.AdminActionEnableMember, pattern: "POST /v1/principals/{id}/enable", permission: pMembers, gate: approval, target: tPrincipal, build: noBody},
	{action: schema.AdminActionHideMember, pattern: "POST /v1/principals/{id}/hide", permission: pMembers, gate: approval, target: tPrincipal, build: noBody},
	{action: schema.AdminActionListCredentials, pattern: "GET /v1/principals/{id}/credentials", permission: pMembers, gate: direct, target: tPrincipal, build: noBody},
	{action: schema.AdminActionRevokeCredential, pattern: "DELETE /v1/credentials/{credential_id}", permission: pMembers, gate: approval, target: tCredential, build: noBody},
	{action: schema.AdminActionRevokeAllCredentials, pattern: "POST /v1/principals/{id}/credentials/revoke-all", permission: pMembers, gate: approval, target: tPrincipal, build: noBody},
	{action: schema.AdminActionListHooks, pattern: "GET /v1/hooks", permission: pMembers, gate: direct, build: noBody},
	{action: schema.AdminActionRevokeHook, pattern: "DELETE /v1/hooks/{id}", permission: pMembers, gate: approval, target: tHook, build: noBody},

	{action: schema.AdminActionListChannels, pattern: "GET /v1/directory/channels", permission: pChannels, gate: direct, build: fixedQuery("scope=all")},
	{action: schema.AdminActionCreateChannel, pattern: "POST /v0/channels", permission: pChannels, gate: direct, build: passBody("")},
	{action: schema.AdminActionRenameChannel, pattern: "PUT /v1/channels/{id}/name", permission: pChannels, gate: direct, target: tChannel, build: buildRenameChannel, changesTarget: true},
	{action: schema.AdminActionSetChannelDefault, pattern: "PUT /v1/channels/{id}/settings", permission: pChannels, gate: direct, target: tChannel, build: onlyFields("default"), changesTarget: true},
	{action: schema.AdminActionSetChannelVisibility, pattern: "PUT /v1/channels/{id}/settings", permission: pChannels, gate: approval, target: tChannel, build: onlyFields("visibility"), changesTarget: true},
	{action: schema.AdminActionArchiveChannel, pattern: "PUT /v1/channels/{id}/settings", permission: pChannels, gate: approval, target: tChannel, build: fixedBody(`{"archived":true}`), changesTarget: true},
	{action: schema.AdminActionRestoreChannel, pattern: "PUT /v1/channels/{id}/settings", permission: pChannels, gate: direct, target: tChannel, build: fixedBody(`{"archived":false}`), changesTarget: true},
	{action: schema.AdminActionListChannelMembers, pattern: "GET /v1/channels/{channel}/members", permission: pChannels, gate: direct, target: tChannel, build: noBody},
	{action: schema.AdminActionAddChannelMember, pattern: "POST /v1/channels/{channel}/members", permission: pChannels, gate: direct, target: tChannel, build: buildAddChannelMember, changesTarget: true},
	{action: schema.AdminActionRemoveChannelMember, pattern: "DELETE /v1/channels/{channel}/members/{principal_id}", permission: pChannels, gate: approval, target: tChannel, build: buildMemberPath(false), changesTarget: true},
	{action: schema.AdminActionSetChannelMemberRights, pattern: "PUT /v1/channels/{channel}/members/{principal_id}", permission: pChannels, gate: approval, target: tChannel, build: buildMemberPath(true), changesTarget: true},

	{action: schema.AdminActionCreateMemberLink, pattern: "POST /v2/invites", permission: pLinks, gate: direct, build: buildMemberLink},
	{action: schema.AdminActionListMemberLinks, pattern: "GET /v2/invites", permission: pMembers, gate: direct, build: noBody},
	{action: schema.AdminActionCloseMemberLink, pattern: "DELETE /v1/invites/{id}", permission: pMembers, gate: direct, target: tInvite, build: noBody},
	{action: schema.AdminActionListLinkJoiners, pattern: "GET /v2/invites/{id}/joiners", permission: pMembers, gate: direct, target: tInvite, build: noBody},
	{action: schema.AdminActionDisableLinkJoiners, pattern: "POST /v1/invites/{id}/disable-joiners", permission: pMembers, gate: approval, target: tInvite, build: passBody("{}")},
	{action: schema.AdminActionDisableInvitees, pattern: "POST /v1/principals/{id}/disable-invitees", permission: pMembers, gate: approval, target: tPrincipal, build: passBody("{}")},
	{action: schema.AdminActionPauseInvites, pattern: "PUT /v1/invites/settings", permission: pMembers, gate: direct, build: fixedBody(`{"paused":true}`)},
	{action: schema.AdminActionResumeInvites, pattern: "PUT /v1/invites/settings", permission: pMembers, gate: approval, build: fixedBody(`{"paused":false}`)},

	{action: schema.AdminActionGetJoinSettings, pattern: "GET /v1/join/settings", permission: pSettings, gate: direct, build: noBody},
	{action: schema.AdminActionSetJoinSettings, pattern: "PUT /v1/join/settings", permission: pSettings, gate: approval, build: buildJoinSettings},
	{action: schema.AdminActionGetPermissions, pattern: "GET /v1/permissions", permission: pSettings, gate: direct, build: noBody},
	{action: schema.AdminActionSetPermissions, pattern: "PUT /v1/permissions", permission: pSettings, gate: approval, build: passBody("")},

	{action: schema.AdminActionListReports, pattern: "GET /v1/reports", permission: pMembers, gate: direct, build: queryParams("limit")},
	{action: schema.AdminActionGetReport, pattern: "GET /v1/reports/{id}", permission: pMembers, gate: direct, target: tReport, build: noBody},
	{action: schema.AdminActionUpdateReport, pattern: "PUT /v1/reports/{id}", permission: pMembers, gate: direct, target: tReport, build: passBody("")},

	{action: schema.AdminActionExportAudit, pattern: "GET /v1/audit", permission: pAudit, gate: direct, build: queryParams("after", "limit")},

	// Channel entry bans (moderation M3, issue #851; #849 Q4). The list is a
	// read; a ban removes a person from the channel and keeps them out, so it
	// is gated, and so is lifting one. Both name the member in params, as the
	// member actions do, and carry no body: a ban has no reason or expiry.
	// The route's own rules are not relaxed for the agent: the store gives it
	// operator standing only where it holds manage_channels, and an owner is
	// out of an operator's reach.
	{action: schema.AdminActionListChannelBans, pattern: "GET /v1/channels/{channel}/bans", permission: pChannels, gate: direct, target: tChannel, build: noBody},
	{action: schema.AdminActionBanChannelMember, pattern: "PUT /v1/channels/{channel}/bans/{principal_id}", permission: pChannels, gate: approval, target: tChannel, build: buildMemberPath(false), changesTarget: true},
	{action: schema.AdminActionUnbanChannelMember, pattern: "DELETE /v1/channels/{channel}/bans/{principal_id}", permission: pChannels, gate: approval, target: tChannel, build: buildMemberPath(false), changesTarget: true},

	// A channel's topic (issue #1523, docs/design/channel-topic.md §4.3):
	// direct, as rename_channel is. The route's own standing check is not
	// relaxed for the agent: the store gives it operator standing only where
	// it holds manage_channels, and the route's write-rate bound (#1667)
	// applies to the agent as to anyone.
	{action: schema.AdminActionSetChannelTopic, pattern: "PUT /v1/channels/{id}/topic", permission: pChannels, gate: direct, target: tChannel, build: buildSetChannelTopic, changesTarget: true},
}

// adminCatalogByAction indexes adminCatalog.
var adminCatalogByAction = func() map[schema.AdminAction]adminCatalogEntry {
	m := make(map[schema.AdminAction]adminCatalogEntry, len(adminCatalog))
	for _, e := range adminCatalog {
		m[e.action] = e
	}
	return m
}()

// adminCatalogPatterns is every route pattern the catalog dispatches.
var adminCatalogPatterns = func() map[string]bool {
	m := make(map[string]bool, len(adminCatalog))
	for _, e := range adminCatalog {
		m[e.pattern] = true
	}
	return m
}()

func badParams(action schema.AdminAction, why string) *adminRefusal {
	return doorRefuse(http.StatusBadRequest, "invalid_request", fmt.Sprintf("%s: %s", action, why))
}

// entryOf is the catalog row a builder is building for.
func entryOf(ctx context.Context) adminCatalogEntry {
	e, _ := ctx.Value(adminBuildingKey{}).(adminCatalogEntry)
	return e
}

type adminBuildingKey struct{}

// fillPath fills the pattern's path parameters: {channel} with the target
// channel's name, {principal_id} with member, any other with the target's id.
func fillPath(pattern string, target *schema.AdminTargetV1, member int64) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	for {
		i := strings.Index(path, "{")
		if i < 0 {
			return method, path
		}
		j := strings.Index(path[i:], "}")
		name := path[i+1 : i+j]
		val := ""
		switch {
		case name == "channel" && target != nil:
			val = url.PathEscape(target.Name)
		case name == "principal_id":
			val = strconv.FormatInt(member, 10)
		case target != nil:
			val = strconv.FormatInt(target.ID, 10)
		}
		path = path[:i] + val + path[i+j+1:]
	}
}

// canonicalJSON re-encodes a JSON value: keys sorted, duplicates collapsed to
// the last (as the route's decoder takes them), numbers kept as written. What
// the decider is shown, what is stored and what runs are these same bytes
// (security review of #1339).
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	return json.Marshal(v)
}

func emptyParams(p json.RawMessage) bool {
	t := bytes.TrimSpace(p)
	return len(t) == 0 || bytes.Equal(t, []byte("{}")) || bytes.Equal(t, []byte("null"))
}

// paramsObject decodes params as one JSON object; nil for none.
func paramsObject(p json.RawMessage) (map[string]json.RawMessage, error) {
	if emptyParams(p) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(p, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func targetChannels(in adminBuildInput) []int64 {
	if in.target != nil && in.target.Kind == schema.AdminTargetChannel {
		return []int64{in.target.ID}
	}
	return nil
}

// noBody: the route reads no body, and the action takes no params.
func noBody(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
	e := entryOf(ctx)
	if !emptyParams(in.params) {
		return adminRequest{}, adminTouches{}, badParams(e.action, "takes no params")
	}
	method, path := fillPath(e.pattern, in.target, 0)
	return adminRequest{method: method, path: path}, adminTouches{channels: touchedIf(e, in)}, nil
}

func touchedIf(e adminCatalogEntry, in adminBuildInput) []int64 {
	if e.changesTarget {
		return targetChannels(in)
	}
	return nil
}

// passBody: the params are the route's body, verbatim; the route's strict
// decoding refuses what it does not take. def is the body when there are no
// params ("" sends none).
func passBody(def string) adminBuilder {
	return func(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
		e := entryOf(ctx)
		method, path := fillPath(e.pattern, in.target, 0)
		body := []byte(def)
		var stored json.RawMessage
		if !emptyParams(in.params) {
			if _, err := paramsObject(in.params); err != nil {
				return adminRequest{}, adminTouches{}, badParams(e.action, "params must be a JSON object")
			}
			canon, err := canonicalJSON(in.params)
			if err != nil {
				return adminRequest{}, adminTouches{}, badParams(e.action, "params must be a JSON object")
			}
			body, stored = canon, canon
		}
		if len(body) == 0 {
			body = nil
		}
		return adminRequest{method: method, path: path, body: body}, adminTouches{channels: touchedIf(e, in), params: stored}, nil
	}
}

// fixedBody: the action is the route with this body, and takes no params.
func fixedBody(body string) adminBuilder {
	return func(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
		e := entryOf(ctx)
		if !emptyParams(in.params) {
			return adminRequest{}, adminTouches{}, badParams(e.action, "takes no params")
		}
		method, path := fillPath(e.pattern, in.target, 0)
		return adminRequest{method: method, path: path, body: []byte(body)}, adminTouches{channels: touchedIf(e, in)}, nil
	}
}

// fixedQuery: the route with this query, and no params.
func fixedQuery(query string) adminBuilder {
	return func(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
		e := entryOf(ctx)
		if !emptyParams(in.params) {
			return adminRequest{}, adminTouches{}, badParams(e.action, "takes no params")
		}
		method, path := fillPath(e.pattern, in.target, 0)
		return adminRequest{method: method, path: path, query: query}, adminTouches{}, nil
	}
}

// queryParams: the params are the route's query parameters, only those
// named, each a string, number or boolean.
func queryParams(allowed ...string) adminBuilder {
	return func(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
		e := entryOf(ctx)
		m, err := paramsObject(in.params)
		if err != nil {
			return adminRequest{}, adminTouches{}, badParams(e.action, "params must be a JSON object")
		}
		q := url.Values{}
		for k, raw := range m {
			if !slices.Contains(allowed, k) {
				return adminRequest{}, adminTouches{}, badParams(e.action, fmt.Sprintf("unknown param %q; it takes %s", k, strings.Join(allowed, ", ")))
			}
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return adminRequest{}, adminTouches{}, badParams(e.action, fmt.Sprintf("param %q is not JSON", k))
			}
			switch x := v.(type) {
			case string:
				q.Set(k, x)
			case float64:
				q.Set(k, strconv.FormatFloat(x, 'f', -1, 64))
			case bool:
				q.Set(k, strconv.FormatBool(x))
			default:
				return adminRequest{}, adminTouches{}, badParams(e.action, fmt.Sprintf("param %q must be a string, number or boolean", k))
			}
		}
		method, path := fillPath(e.pattern, in.target, 0)
		var stored json.RawMessage
		if len(m) > 0 {
			stored, _ = json.Marshal(m)
		}
		return adminRequest{method: method, path: path, query: q.Encode()}, adminTouches{params: stored}, nil
	}
}

// onlyFields: the params may carry only the named fields of the route's
// body (channel settings: one action per field, each with its own gate).
func onlyFields(fields ...string) adminBuilder {
	return func(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
		e := entryOf(ctx)
		m, err := paramsObject(in.params)
		if err != nil {
			return adminRequest{}, adminTouches{}, badParams(e.action, "params must be a JSON object")
		}
		if len(m) == 0 {
			return adminRequest{}, adminTouches{}, badParams(e.action, "needs "+strings.Join(fields, ", "))
		}
		for k := range m {
			if !slices.Contains(fields, k) {
				return adminRequest{}, adminTouches{}, badParams(e.action, fmt.Sprintf("takes only %s", strings.Join(fields, ", ")))
			}
		}
		method, path := fillPath(e.pattern, in.target, 0)
		canon, err := json.Marshal(m)
		if err != nil {
			return adminRequest{}, adminTouches{}, badParams(e.action, "params must be a JSON object")
		}
		return adminRequest{method: method, path: path, body: canon}, adminTouches{channels: touchedIf(e, in), params: canon}, nil
	}
}

// buildCreateMember: a person, always; an agent never creates an agent
// (§3.3), so the params are {name} and kind is the dispatcher's.
func buildCreateMember(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
	e := entryOf(ctx)
	var p struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(bytes.NewReader(in.params))
	dec.DisallowUnknownFields()
	if emptyParams(in.params) || dec.Decode(&p) != nil {
		return adminRequest{}, adminTouches{}, badParams(e.action, `params are {"name": ...}; the kind is always human`)
	}
	body, _ := json.Marshal(schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: p.Name})
	stored, _ := json.Marshal(p)
	return adminRequest{method: http.MethodPost, path: "/v0/principals", body: body}, adminTouches{params: stored}, nil
}

// buildRenameChannel: {name}; the route's "from" is the name the server read
// for the target, so a rename races nothing the decider did not see.
func buildRenameChannel(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
	e := entryOf(ctx)
	var p struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(bytes.NewReader(in.params))
	dec.DisallowUnknownFields()
	if emptyParams(in.params) || dec.Decode(&p) != nil || p.Name == "" {
		return adminRequest{}, adminTouches{}, badParams(e.action, `params are {"name": ...}`)
	}
	body, _ := json.Marshal(schema.RenameChannelRequestV1{Name: p.Name, From: in.target.Name})
	method, path := fillPath(e.pattern, in.target, 0)
	stored, _ := json.Marshal(p)
	return adminRequest{method: method, path: path, body: body}, adminTouches{channels: targetChannels(in), params: stored}, nil
}

// buildSetChannelTopic: {topic}, "" clearing it. The field is required, so an
// agent that sends {} has not asked to clear the topic by accident. The topic
// is validated here, so a bad one is refused at submit with the door's own
// answer, and because the store does not check it (security review of
// #1531): a caller of the store must.
func buildSetChannelTopic(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
	e := entryOf(ctx)
	var p struct {
		Topic *string `json:"topic"`
	}
	dec := json.NewDecoder(bytes.NewReader(in.params))
	dec.DisallowUnknownFields()
	if emptyParams(in.params) || dec.Decode(&p) != nil || p.Topic == nil {
		return adminRequest{}, adminTouches{}, badParams(e.action, `params are {"topic": ...}; "" clears it`)
	}
	req := schema.SetChannelTopicRequestV1{Topic: *p.Topic}
	if err := req.Validate(); err != nil {
		return adminRequest{}, adminTouches{}, badParams(e.action, err.Error())
	}
	body, _ := json.Marshal(req)
	method, path := fillPath(e.pattern, in.target, 0)
	stored, _ := json.Marshal(req)
	return adminRequest{method: method, path: path, body: body}, adminTouches{channels: targetChannels(in), params: stored}, nil
}

// memberParams is a channel-member action's params: the member, and for
// set_channel_member_rights the rights.
type memberParams struct {
	PrincipalID int64 `json:"principal_id"`
	CanDecide   *bool `json:"can_decide,omitempty"`
	Manages     *bool `json:"manages,omitempty"`
}

func decodeMemberParams(e adminCatalogEntry, raw json.RawMessage, rights bool) (memberParams, *adminRefusal) {
	var p memberParams
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	want := `{"principal_id": N}`
	if rights {
		want = `{"principal_id": N, "can_decide"?: bool, "manages"?: bool}`
	}
	if emptyParams(raw) || dec.Decode(&p) != nil || p.PrincipalID <= 0 {
		return p, badParams(e.action, "params are "+want)
	}
	if !rights && (p.CanDecide != nil || p.Manages != nil) {
		return p, badParams(e.action, "params are "+want+"; rights change only by set_channel_member_rights")
	}
	if rights && p.CanDecide == nil && p.Manages == nil {
		return p, badParams(e.action, "needs can_decide or manages")
	}
	return p, nil
}

// buildAddChannelMember: {principal_id}. The dispatched body names the
// member and nothing else: can_decide and manages are never in it, so an
// added member does not decide (§3.5.4 wall 3).
func buildAddChannelMember(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
	e := entryOf(ctx)
	p, r := decodeMemberParams(e, in.params, false)
	if r != nil {
		return adminRequest{}, adminTouches{}, r
	}
	// Only the member: Name and CanDecide stay empty and are omitted.
	body, _ := json.Marshal(schema.AddChannelMemberRequestV1{ID: p.PrincipalID})
	method, path := fillPath(e.pattern, in.target, 0)
	stored, _ := json.Marshal(p)
	return adminRequest{method: method, path: path, body: body}, adminTouches{channels: targetChannels(in), member: p.PrincipalID, params: stored}, nil
}

// buildMemberPath: the member in the path; for the rights action the rights
// are the body.
func buildMemberPath(rights bool) adminBuilder {
	return func(ctx context.Context, _ *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
		e := entryOf(ctx)
		p, r := decodeMemberParams(e, in.params, rights)
		if r != nil {
			return adminRequest{}, adminTouches{}, r
		}
		method, path := fillPath(e.pattern, in.target, p.PrincipalID)
		var body []byte
		if rights {
			body, _ = json.Marshal(schema.PutChannelMemberRequestV1{CanDecide: p.CanDecide, Manages: p.Manages})
		}
		stored, _ := json.Marshal(p)
		return adminRequest{method: method, path: path, body: body}, adminTouches{channels: targetChannels(in), member: p.PrincipalID, params: stored}, nil
	}
}

// buildMemberLink clamps the link (§3.3): a member link, at most five uses
// (five when unsaid), expiring within seven days (seven when unsaid), never
// "never expires", landing only in channels the agent may add to and never
// its approval channel. The store ties the link to the agent as maker.
func buildMemberLink(ctx context.Context, s *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
	e := entryOf(ctx)
	var req schema.CreateInviteRequestV2
	if !emptyParams(in.params) {
		dec := json.NewDecoder(bytes.NewReader(in.params))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return adminRequest{}, adminTouches{}, badParams(e.action, "params must be a CreateInviteRequestV2")
		}
	}
	if req.Kind != "" && req.Kind != schema.InviteKindMember {
		return adminRequest{}, adminTouches{}, badParams(e.action, "an AI administrator makes member links only")
	}
	req.Kind = schema.InviteKindMember
	if req.NeverExpires {
		return adminRequest{}, adminTouches{}, badParams(e.action, "an AI administrator's link always expires, within 7 days")
	}
	limit := s.now().Add(adminLinkMaxExpiry)
	if req.ExpiresAt == nil || req.ExpiresAt.Time().After(limit) {
		t := schema.NewTimestamp(limit)
		req.ExpiresAt = &t
	}
	if req.MaxUses == nil || *req.MaxUses > adminLinkMaxUses {
		n := int64(adminLinkMaxUses)
		req.MaxUses = &n
	}
	var touched []int64
	for _, name := range req.LandingChannels {
		ch, err := s.store.ChannelByName(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			continue // the route answers channel_not_found
		}
		if err != nil {
			return adminRequest{}, adminTouches{}, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
		}
		touched = append(touched, ch.ID)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return adminRequest{}, adminTouches{}, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
	}
	return adminRequest{method: http.MethodPost, path: "/v2/invites", body: body}, adminTouches{channels: touched, params: body}, nil
}

// buildJoinSettings passes the body, refusing the approval channel among
// open_channels.
func buildJoinSettings(ctx context.Context, s *Server, in adminBuildInput) (adminRequest, adminTouches, *adminRefusal) {
	e := entryOf(ctx)
	if emptyParams(in.params) {
		return adminRequest{}, adminTouches{}, badParams(e.action, "needs at least one setting")
	}
	var req schema.PutJoinSettingsRequestV1
	dec := json.NewDecoder(bytes.NewReader(in.params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return adminRequest{}, adminTouches{}, badParams(e.action, "params must be a PutJoinSettingsRequestV1")
	}
	var touched []int64
	if req.OpenChannels != nil {
		for _, name := range *req.OpenChannels {
			ch, err := s.store.ChannelByName(ctx, name)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return adminRequest{}, adminTouches{}, doorRefuse(http.StatusInternalServerError, errInternal.Code, errInternal.Message)
			}
			touched = append(touched, ch.ID)
		}
	}
	canon, err := canonicalJSON(in.params)
	if err != nil {
		return adminRequest{}, adminTouches{}, badParams(e.action, "params must be a PutJoinSettingsRequestV1")
	}
	return adminRequest{method: http.MethodPut, path: "/v1/join/settings", body: canon}, adminTouches{channels: touched, params: canon}, nil
}
