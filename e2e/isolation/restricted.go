package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

// The restricted operator (issue #1015, ADR-014,
// docs/design/admin-roles.md §7.7 item 1). The owner makes an operator, R,
// who manages a private channel of its own, and, through the real
// PUT /v1/permissions, cuts manage_members, act_as_members and
// manage_channels from every operator. Then R tries, and is refused:
//
//   - every accessPermission route of the table (read from routes.go, so a
//     route added later gets its row): 403 permission_denied naming the
//     route's permission;
//   - the channel-manager routes on A's channel: the 404 a channel that
//     never existed gets, byte for byte;
//   - in its own channel, setting can_decide or manages (403, audited with
//     permission=manage_channels), adding an agent, and removing an approver
//     or the owner, as a manager is refused;
//   - a link naming A's channel ("a channel you are not in"), an open link
//     and a link granting may_invite;
//   - the joining settings naming A's channel, after the owner grants it
//     change_server_settings: 403 permission_denied, manage_channels;
//   - A's channel's member list and nets: 404; GET /v1/invites lists its own
//     only, and closing A's open link is 404;
//   - a session in its own channel is listed without runtime or host;
//   - a sign-in link it made before the cut is refused at redemption, and a
//     link it made before the cut naming A's channel is redeemed without it.
//
// The owner then gives the operator column back, so the rest of the run is
// as before.

// permissionConsts names each schema.Permission constant, for reading the
// .needs(schema.Perm...) of routes.go.
var permissionConsts = map[string]schema.Permission{
	"PermCreatePublicChannels":  schema.PermCreatePublicChannels,
	"PermCreatePrivateChannels": schema.PermCreatePrivateChannels,
	"PermMakeMemberLinks":       schema.PermMakeMemberLinks,
	"PermSeeMemberDirectory":    schema.PermSeeMemberDirectory,
	"PermUseVoice":              schema.PermUseVoice,
	"PermShareScreen":           schema.PermShareScreen,
	"PermManageMembers":         schema.PermManageMembers,
	"PermActAsMembers":          schema.PermActAsMembers,
	"PermManageChannels":        schema.PermManageChannels,
	"PermChangeServerSettings":  schema.PermChangeServerSettings,
	"PermReadAuditLog":          schema.PermReadAuditLog,
}

// permissionRoute finds a route of the permission class and its permission.
var permissionRoute = regexp.MustCompile(`rt\("([^"]+)", accessPermission, [^\n]*?\.needs\(schema\.(Perm[A-Za-z]+)\)`)

// permissionRoutes reads every accessPermission route of routes.go and the
// permission it needs.
func permissionRoutes(repo string) (map[string]schema.Permission, error) {
	src, err := os.ReadFile(filepath.Join(repo, "internal", "server", "routes.go")) // #nosec G304 -- the repository this run is in
	if err != nil {
		return nil, err
	}
	out := map[string]schema.Permission{}
	for _, m := range permissionRoute.FindAllStringSubmatch(string(src), -1) {
		p, ok := permissionConsts[m[2]]
		if !ok {
			return nil, fmt.Errorf("routes.go: %s needs schema.%s, which this check does not know", m[1], m[2])
		}
		out[m[1]] = p
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("routes.go has no accessPermission route this check can read")
	}
	return out, nil
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// errorOf decodes an error document.
func errorOf(body []byte) (schema.Error, error) {
	var e schema.Error
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return e, fmt.Errorf("not an error document: %s", body)
	}
	return e, nil
}

// refusedNaming requires a 403 error answer of code and permission.
func refusedNaming(what string, status int, body []byte, err error, code string, perm schema.Permission) error {
	const wantStatus = http.StatusForbidden
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if status != wantStatus {
		return fmt.Errorf("%s = %d %s, want %d %s", what, status, body, wantStatus, code)
	}
	e, derr := errorOf(body)
	if derr != nil {
		return fmt.Errorf("%s: %w", what, derr)
	}
	if e.Code != code || e.Permission != perm {
		return fmt.Errorf("%s = %s, want code %q permission %q", what, body, code, perm)
	}
	return nil
}

// sameAnswer requires that two requests got the same answer, byte for byte.
func sameAnswer(what string, s1 int, b1 []byte, s2 int, b2 []byte) error {
	if s1 != s2 || !bytes.Equal(b1, b2) {
		return fmt.Errorf("%s: %d %s, but a thing that never existed gets %d %s", what, s1, b1, s2, b2)
	}
	return nil
}

// setOperatorCells sets the operator column's cells for perms, as the owner,
// through the real PUT.
func (w *world) setOperatorCells(granted bool, perms ...schema.Permission) error {
	var doc schema.PermissionsResponseV1
	if err := w.op.call(http.MethodGet, "/v1/permissions", nil, &doc); err != nil {
		return err
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision}
	for _, p := range perms {
		req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleOperator, Granted: granted})
	}
	return w.op.call(http.MethodPut, "/v1/permissions", req, nil)
}

// auditDenials counts the access_denied rows by actor whose detail names perm,
// in the owner's audit export.
func (w *world) auditDenials(actor int64, perm schema.Permission) (int, error) {
	status, body, err := w.op.do(http.MethodGet, "/v1/audit", nil)
	if err != nil {
		return 0, fmt.Errorf("the audit export: %w", err)
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("the audit export = %d %s", status, body)
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e schema.AuditEventV1
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return 0, err
		}
		if e.Actor == "principal:"+itoa(actor) && e.Action == "access_denied" && strings.Contains(e.Detail, "permission="+string(perm)) {
			n++
		}
	}
	return n, sc.Err()
}

// restrictedPhase runs the restricted operator's rows. It answers how many
// were run.
func (w *world) restrictedPhase() (int, error) {
	tag := newTag(w.tag, w.btag, w.ptag, w.qtag)
	name := func(s string) string { return "rho-" + tag + "-" + s }
	var whoami schema.WhoAmIResponseV1
	if err := w.op.call(http.MethodGet, "/v1/whoami", nil, &whoami); err != nil {
		return 0, err
	}
	owner := whoami.ID

	// R's channel, made by the owner, with R as its manager, a member, an
	// approver and the owner in it; an agent with a session in it; and an
	// agent outside it.
	rchan := name("room")
	var created schema.CreateChannelResponse
	if err := w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: rchan}, &created); err != nil {
		return 0, err
	}
	r, err := w.newPerson(name("operator"), rchan)
	if err != nil {
		return 0, err
	}
	if err := w.op.call(http.MethodPut, "/v1/principals/"+itoa(r.id)+"/role", schema.RoleChangeRequestV1{Role: schema.RoleOperator}, nil); err != nil {
		return 0, err
	}
	yes := true
	member := func(id int64) string { return "/v1/channels/" + rchan + "/members/" + itoa(id) }
	if err := w.op.call(http.MethodPut, member(r.id), schema.PutChannelMemberRequestV1{Manages: &yes}, nil); err != nil {
		return 0, err
	}
	rmember, err := w.newPerson(name("member"), rchan)
	if err != nil {
		return 0, err
	}
	approver, err := w.newPerson(name("approver"), rchan)
	if err != nil {
		return 0, err
	}
	if err := w.op.call(http.MethodPut, member(approver.id), schema.PutChannelMemberRequestV1{CanDecide: &yes}, nil); err != nil {
		return 0, err
	}
	// A second operator in R's channel, whom a manager may not remove.
	op2, err := w.newPerson(name("operator2"), rchan)
	if err != nil {
		return 0, err
	}
	if err := w.op.call(http.MethodPut, "/v1/principals/"+itoa(op2.id)+"/role", schema.RoleChangeRequestV1{Role: schema.RoleOperator}, nil); err != nil {
		return 0, err
	}
	if err := w.op.call(http.MethodPut, member(owner), nil, nil); err != nil {
		return 0, err
	}
	bot, err := w.newAgent(name("bot"), &group{channels: []string{rchan}, chanIDs: []int64{created.Channel.ID}})
	if err != nil {
		return 0, err
	}
	var sess schema.SessionV1
	if err := bot.api.call(http.MethodPost, "/v1/sessions", schema.CreateSessionRequestV1{Client: name("client"), Runtime: "fixture", Host: name("host")}, &sess); err != nil {
		return 0, err
	}
	var outsider schema.CreatePrincipalResponse
	if err := w.op.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalAgent, Name: name("outsider")}, &outsider); err != nil {
		return 0, err
	}

	// Before the cut, R makes a sign-in link for its member, and a link
	// naming its channel and A's, which it may while it holds
	// manage_channels.
	var link schema.CreateSignInLinkResponseV1
	if err := r.api.call(http.MethodPost, "/v1/principals/"+itoa(rmember.id)+"/sign-in-links", schema.CreateSignInLinkRequestV1{}, &link); err != nil {
		return 0, err
	}
	w.h.secret("the restricted operator's sign-in link code", link.Code)
	inv, err := w.invite(r.api, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{rchan, w.a.main()}})
	if err != nil {
		return 0, err
	}

	// The cut, through the real PUT.
	cut := []schema.Permission{schema.PermManageMembers, schema.PermActAsMembers, schema.PermManageChannels}
	if err := w.setOperatorCells(false, cut...); err != nil {
		return 0, fmt.Errorf("the owner's cut: %w", err)
	}
	var mine schema.WhoAmIResponseV1
	if err := r.api.call(http.MethodGet, "/v1/whoami", nil, &mine); err != nil {
		return 0, err
	}
	for _, p := range cut {
		if slices.Contains(mine.Permissions, p) {
			return 0, fmt.Errorf("the restricted operator's whoami still lists %s after the cut", p)
		}
	}

	rows := 0
	// Every route of the permission class.
	routes, err := permissionRoutes(w.h.repo)
	if err != nil {
		return 0, err
	}
	for route, perm := range routes {
		method, path, _ := strings.Cut(route, " ")
		path = pathParam.ReplaceAllString(path, "1")
		var body any
		if method != http.MethodGet {
			body = map[string]any{}
		}
		status, resp, err := r.api.do(method, path, body)
		if err := refusedNaming("the restricted operator: "+route, status, resp, err, schema.ErrorCodePermissionDenied, perm); err != nil {
			return rows, err
		}
		rows++
	}

	// The manager class on A's channel: the uniform 404.
	aChan, ghost := w.a.main(), "no-such-channel-"+tag
	aID := itoa(w.a.chanIDs[0])
	for _, c := range []struct{ route, method, a, g string }{
		{"POST /v1/channels/{channel}/members", http.MethodPost, "/v1/channels/" + aChan + "/members", "/v1/channels/" + ghost + "/members"},
		{"PUT /v1/channels/{channel}/members/{principal_id}", http.MethodPut, "/v1/channels/" + aChan + "/members/" + itoa(rmember.id), "/v1/channels/" + ghost + "/members/" + itoa(rmember.id)},
		{"DELETE /v1/channels/{channel}/members/{principal_id}", http.MethodDelete, "/v1/channels/" + aChan + "/members/" + itoa(rmember.id), "/v1/channels/" + ghost + "/members/" + itoa(rmember.id)},
		{"PUT /v1/channels/{id}/name", http.MethodPut, "/v1/channels/" + aID + "/name", "/v1/channels/999999999/name"},
		{"DELETE .../reactions/{emoji}/{principal_id}", http.MethodDelete, "/v2/channels/" + aChan + "/messages/1/reactions/%F0%9F%91%8D/1", "/v2/channels/" + ghost + "/messages/1/reactions/%F0%9F%91%8D/1"},
		{"DELETE .../reactors/{principal_id}", http.MethodDelete, "/v2/channels/" + aChan + "/messages/1/reactors/1", "/v2/channels/" + ghost + "/messages/1/reactors/1"},
		{"GET /v1/channels/{channel}/members", http.MethodGet, "/v1/channels/" + aChan + "/members", "/v1/channels/" + ghost + "/members"},
		{"GET /v1/channels/{channel}/nets", http.MethodGet, "/v1/channels/" + aChan + "/nets", "/v1/channels/" + ghost + "/nets"},
	} {
		var body any
		if c.method == http.MethodPost || c.method == http.MethodPut {
			body = map[string]any{}
		}
		s1, b1, err := r.api.do(c.method, c.a, body)
		if err != nil {
			return rows, err
		}
		s2, b2, err := r.api.do(c.method, c.g, body)
		if err != nil {
			return rows, err
		}
		if s1 != http.StatusNotFound {
			return rows, fmt.Errorf("the restricted operator: %s on A's channel = %d %s, want 404", c.route, s1, b1)
		}
		if err := sameAnswer("the restricted operator: "+c.route+" on A's channel", s1, bytes.ReplaceAll(b1, []byte(aChan), []byte(ghost)), s2, b2); err != nil {
			return rows, err
		}
		rows++
	}

	// In its own channel: a member-manager's rights, refused and audited.
	before, err := w.auditDenials(r.id, schema.PermManageChannels)
	if err != nil {
		return rows, err
	}
	for _, c := range []struct {
		what   string
		method string
		path   string
		body   any
		status int
		code   string
	}{
		{"makes its member an approver", http.MethodPut, member(rmember.id), schema.PutChannelMemberRequestV1{CanDecide: &yes}, http.StatusForbidden, "forbidden"},
		{"makes its member a manager", http.MethodPut, member(rmember.id), schema.PutChannelMemberRequestV1{Manages: &yes}, http.StatusForbidden, "forbidden"},
		{"adds an agent", http.MethodPut, member(outsider.Principal.ID), nil, http.StatusNotFound, "principal_not_found"},
		{"removes the approver", http.MethodDelete, member(approver.id), nil, http.StatusForbidden, "forbidden"},
		{"removes another operator", http.MethodDelete, member(op2.id), nil, http.StatusForbidden, "forbidden"},
		{"removes the owner", http.MethodDelete, member(owner), nil, http.StatusForbidden, "forbidden"},
	} {
		status, body, err := r.api.do(c.method, c.path, c.body)
		if err != nil {
			return rows, err
		}
		if status != c.status {
			return rows, fmt.Errorf("the restricted operator %s in its own channel = %d %s, want %d", c.what, status, body, c.status)
		}
		if e, err := errorOf(body); err != nil || e.Code != c.code {
			return rows, fmt.Errorf("the restricted operator %s in its own channel = %s, want code %s", c.what, body, c.code)
		}
		rows++
	}
	after, err := w.auditDenials(r.id, schema.PermManageChannels)
	if err != nil {
		return rows, err
	}
	if after-before < 5 {
		return rows, fmt.Errorf("the restricted operator's refusals in its own channel wrote %d access_denied rows with permission=manage_channels, want 5", after-before)
	}

	// Links: A's channel, an open link, the right to invite.
	inviteAs := func(req schema.CreateInviteRequestV1) (int, []byte, error) {
		return r.api.do(http.MethodPost, "/v1/invites", req)
	}
	s1, b1, err := inviteAs(schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{aChan}})
	if err := refusedNaming("the restricted operator: a link naming A's channel", s1, b1, err, schema.ErrorCodeInviteGrantExceedsInviter, schema.PermManageChannels); err != nil {
		return rows, err
	}
	s2, b2, err := inviteAs(schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{ghost}})
	if err != nil {
		return rows, err
	}
	if err := sameAnswer("the restricted operator: a link naming A's channel", s1, b1, s2, b2); err != nil {
		return rows, err
	}
	status, body, err := inviteAs(schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{rchan}})
	if err := refusedNaming("the restricted operator: an open link", status, body, err, schema.ErrorCodeInviteNotAllowed, schema.PermManageMembers); err != nil {
		return rows, err
	}
	status, body, err = inviteAs(schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{rchan}, MayInvite: &yes})
	if err := refusedNaming("the restricted operator: a link granting may_invite", status, body, err, schema.ErrorCodeInviteGrantExceedsInviter, schema.PermManageMembers); err != nil {
		return rows, err
	}
	rows += 3

	// The joining settings naming A's channel, once R may change settings.
	if err := w.setOperatorCells(true, schema.PermChangeServerSettings); err != nil {
		return rows, err
	}
	status, body, err = r.api.do(http.MethodPut, "/v1/join/settings", schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{rchan, aChan}})
	if err := refusedNaming("the restricted operator: the joining settings naming A's channel", status, body, err, schema.ErrorCodePermissionDenied, schema.PermManageChannels); err != nil {
		return rows, err
	}
	gs, gb, err := r.api.do(http.MethodPut, "/v1/join/settings", schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{rchan, ghost}})
	if err != nil {
		return rows, err
	}
	if err := sameAnswer("the restricted operator: the joining settings naming A's channel", status, body, gs, gb); err != nil {
		return rows, err
	}
	if err := w.setOperatorCells(false, schema.PermChangeServerSettings); err != nil {
		return rows, err
	}
	rows++

	// Invites: its own only, and A's open link is not its to close.
	var invites schema.ListInvitesResponseV1
	if err := r.api.call(http.MethodGet, "/v1/invites", nil, &invites); err != nil {
		return rows, err
	}
	for _, i := range invites.Invites {
		if i.CreatedBy != r.id {
			return rows, fmt.Errorf("the restricted operator's invite list holds invite %d of principal %d", i.ID, i.CreatedBy)
		}
	}
	s1, b1, err = r.api.do(http.MethodDelete, "/v1/invites/"+itoa(w.a.openInviteID), nil)
	if err != nil {
		return rows, err
	}
	s2, b2, err = r.api.do(http.MethodDelete, "/v1/invites/999999999", nil)
	if err != nil {
		return rows, err
	}
	if s1 != http.StatusNotFound {
		return rows, fmt.Errorf("the restricted operator closes A's open link = %d %s, want 404", s1, b1)
	}
	if err := sameAnswer("R closes A's open link", s1, b1, s2, b2); err != nil {
		return rows, err
	}
	rows += 2

	// A session in its own channel: no runtime or host.
	var list schema.SessionRosterV1
	if err := r.api.call(http.MethodGet, "/v1/sessions?channel="+rchan, nil, &list); err != nil {
		return rows, err
	}
	found := false
	for _, s := range list.Sessions {
		if s.ID == sess.ID {
			found = true
			if s.Runtime != "" || s.Host != "" {
				return rows, fmt.Errorf("the restricted operator sees the session's runtime %q and host %q", s.Runtime, s.Host)
			}
		}
	}
	if !found {
		return rows, fmt.Errorf("the restricted operator's channel's session %d is not listed to R", sess.ID)
	}
	rows++

	// What R made before the cut: the sign-in link is refused, the link
	// lands its joiner in R's channel alone.
	nobody := api{h: w.h, base: w.d.baseURL, who: "the restricted operator's member (redeeming)"}
	status, body, err = nobody.do(http.MethodPost, "/v1/sign-in-links/redeem", schema.RedeemSignInLinkRequestV1{Code: link.Code})
	if err != nil {
		return rows, err
	}
	if status != http.StatusNotFound {
		return rows, fmt.Errorf("the restricted operator's sign-in link after the cut = %d %s, want 404", status, body)
	}
	if e, err := errorOf(body); err != nil || e.Code != schema.ErrorCodeSignInLinkInvalid {
		return rows, fmt.Errorf("the restricted operator's sign-in link after the cut = %s, want %s", body, schema.ErrorCodeSignInLinkInvalid)
	}
	joiner, err := w.redeem(inv.Code, name("joiner"))
	if err != nil {
		return rows, fmt.Errorf("the restricted operator's link after the cut: %w", err)
	}
	var chans schema.ListChannelsResponse
	if err := joiner.api.call(http.MethodGet, "/v1/channels", nil, &chans); err != nil {
		return rows, err
	}
	var names []string
	for _, c := range chans.Channels {
		names = append(names, c.Name)
	}
	if !slices.Contains(names, rchan) || slices.Contains(names, aChan) {
		return rows, fmt.Errorf("the restricted operator's joiner is in %v: want R's channel and not A's", names)
	}
	rows += 2

	// With manage_members and act_as_members back but manage_channels still
	// cut, a hook or a manifest into A's channel is refused as one into a
	// channel that never existed (the security review of #1015).
	if err := w.setOperatorCells(true, schema.PermManageMembers, schema.PermActAsMembers); err != nil {
		return rows, err
	}
	hookInto := func(channel string) (int, []byte, error) {
		return r.api.do(http.MethodPost, "/v1/hooks", schema.CreateHookRequest{Channel: channel, Principal: bot.id})
	}
	s1, b1, err = hookInto(aChan)
	if err := refusedNaming("the restricted operator: a hook into A's channel", s1, b1, err, schema.ErrorCodePermissionDenied, schema.PermManageChannels); err != nil {
		return rows, err
	}
	if s2, b2, err = hookInto(ghost); err != nil {
		return rows, err
	}
	if err := sameAnswer("the restricted operator: a hook into A's channel", s1, b1, s2, b2); err != nil {
		return rows, err
	}
	grant := func(channelID int64) (int, []byte, error) {
		return r.api.do(http.MethodPut, "/v1/principals/"+itoa(bot.id)+"/manifest", schema.PutAgentManifestRequestV1{DisplayName: bot.name, Tier: schema.AgentTierA,
			Capabilities: schema.Capabilities(), Channels: []schema.ChannelGrant{{ChannelID: channelID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}}})
	}
	s1, b1, err = grant(w.a.chanIDs[0])
	if err := refusedNaming("the restricted operator: a manifest granting A's channel", s1, b1, err, schema.ErrorCodePermissionDenied, schema.PermManageChannels); err != nil {
		return rows, err
	}
	if s2, b2, err = grant(999999999); err != nil {
		return rows, err
	}
	if err := sameAnswer("the restricted operator: a manifest granting A's channel", s1, b1, s2, b2); err != nil {
		return rows, err
	}
	rows += 2

	// The operator column back, for the rest of the run.
	if err := w.setOperatorCells(true, cut...); err != nil {
		return rows, fmt.Errorf("give the operator column back: %w", err)
	}
	return rows, nil
}
