package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// The AI administrator's phase (issue #1330, docs/design/ai-administrators.md
// §3.4): an agent of B's the owner granted every agent-grantable permission
// is refused on every administrative route it calls directly, exactly as B's
// ungranted agent is, and through the admin door its catalog's reads answer.

// disableThrough is the door's request to disable principal id.
func disableThrough(id int64) schema.SubmitAdminActionRequestV1 {
	return schema.SubmitAdminActionRequestV1{Action: schema.AdminActionDisableMember,
		Target: &schema.AdminTargetV1{Kind: schema.AdminTargetPrincipal, ID: id}}
}

// adminClassRoute finds the administrative routes of routes.go: the
// permission, operator, owner and channel-manager classes.
var adminClassRoute = regexp.MustCompile(`\brt\("((?:GET|POST|PUT|DELETE) /[^"]*)",\s*access(?:Permission|Operator|Owner|ChannelManager)\b`)

func (w *world) aiAdminPhase() (direct, door int, err error) {
	admin, err := w.newAgent(w.bName("admin"), w.b)
	if err != nil {
		return 0, 0, err
	}
	var grant []schema.Permission
	for _, p := range schema.Permissions {
		if p.AgentGrantable() {
			grant = append(grant, p)
		}
	}
	req := schema.PutAgentManifestRequestV1{DisplayName: admin.name, Tier: schema.AgentTierA, Capabilities: schema.Capabilities(),
		Channels:    []schema.ChannelGrant{{ChannelID: w.b.chanIDs[0], Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}}},
		Permissions: grant, Admin: &schema.AgentAdminV1{ApprovalChannelID: w.b.chanIDs[0]}}
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", admin.id), req, nil); err != nil {
		return 0, 0, fmt.Errorf("the owner's grant: %w", err)
	}
	// Ungranted again at the end, so the phases after see the world as it was.
	defer func() {
		req.Permissions, req.Admin = nil, nil
		if rerr := w.op.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", admin.id), req, nil); rerr != nil && err == nil {
			err = fmt.Errorf("revoking the grant: %w", rerr)
		}
	}()

	src, err := os.ReadFile(filepath.Join(w.h.repo, "internal", "server", "routes.go"))
	if err != nil {
		return 0, 0, err
	}
	answerOf := func(status int, body []byte) string {
		var e schema.Error
		_ = json.Unmarshal(body, &e)
		return fmt.Sprintf("%d %s", status, e.Code)
	}
	for _, m := range adminClassRoute.FindAllStringSubmatch(string(src), -1) {
		method, path, _ := strings.Cut(m[1], " ")
		path = fillGhost(path, w.b.main())
		var body any
		if method == http.MethodPost || method == http.MethodPut {
			body = map[string]any{}
		}
		gs, gb, err := admin.api.do(method, path, body)
		if err != nil {
			return direct, door, err
		}
		us, ub, err := w.b.agent.api.do(method, path, body)
		if err != nil {
			return direct, door, err
		}
		if got, want := answerOf(gs, gb), answerOf(us, ub); got != want || gs < 400 {
			return direct, door, fmt.Errorf("%s directly: the granted agent got %s, B's ungranted agent %s", m[1], got, want)
		}
		direct++
	}

	for _, a := range []schema.AdminAction{
		schema.AdminActionListMembers, schema.AdminActionListChannels, schema.AdminActionListHooks,
		schema.AdminActionListMemberLinks, schema.AdminActionGetJoinSettings, schema.AdminActionGetPermissions,
		schema.AdminActionListReports, schema.AdminActionExportAudit,
	} {
		var res schema.AdminActionResultV1
		if err := admin.api.call(http.MethodPost, "/v1/admin/actions", schema.SubmitAdminActionRequestV1{Action: a}, &res); err != nil {
			return direct, door, fmt.Errorf("%s through the door: %w", a, err)
		}
		if res.Status != schema.AdminActionRan || res.HTTPStatus != http.StatusOK {
			return direct, door, fmt.Errorf("%s through the door answered %s %d %s", a, res.Status, res.HTTPStatus, res.Result)
		}
		door++
	}
	return direct, door, nil
}

// fillGhost fills a pattern's parameters with a channel name and an id
// nobody holds.
func fillGhost(path, channel string) string {
	for {
		i := strings.Index(path, "{")
		if i < 0 {
			return path
		}
		j := strings.Index(path[i:], "}")
		val := "999999999"
		if path[i+1:i+j] == "channel" {
			val = channel
		}
		path = path[:i] + val + path[i+j+1:]
	}
}
