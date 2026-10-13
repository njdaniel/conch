package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

// Audit actions this program reads. They are the strings in the audit trail,
// which is a public surface since audit export (#21); spelling them here and
// not importing the server's constants is what makes a rename fail this run.
const (
	auditAccessDenied     = "access_denied"
	auditApprovalCreated  = "approval_created"
	auditDecisionCast     = "decision_cast"
	auditApprovalResolved = "approval_resolved"
	auditNotifySent       = "notify_sent"
	auditNotifyFailed     = "notify_failed"
	auditAuditExported    = "audit_exported"
	auditAgentRateLimited = "agent_rate_limited"
)

// madeUpToken has the look of a credential and was never issued by anyone.
const madeUpToken = "conch_not-a-credential-anyone-was-issued" // #nosec G101 -- deliberately not a credential

// identity shows that nothing is served without a credential, that two agents
// hold separate manifests and credentials, and that rotating or revoking a
// credential stops it at once while the principal and its history stay.
func (h *harness) identity() error {
	// No external identity service exists in this run: every credential is
	// one conchd issued, and conchd is the only server process.
	anon := h.anonymous()
	stranger := api{baseURL: h.proc.baseURL, token: madeUpToken}
	for _, c := range []struct {
		what, method, path string
		as                 api
	}{
		{"a read with no credential", http.MethodGet, "/v1/channels", anon},
		{"a post with no credential", http.MethodPost, "/v1/channels/ops/messages", anon},
		{"an audit export with no credential", http.MethodGet, "/v1/audit", anon},
		{"a read with a credential nobody was issued", http.MethodGet, "/v1/channels", stranger},
	} {
		if _, err := c.as.refused(c.what, c.method, c.path, nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
			return err
		}
	}
	for what, token := range map[string]string{"no credential": "", "a credential nobody was issued": stranger.token} {
		err := mcpclient.New(h.proc.baseURL, token).Initialize(runCtx, name)
		if err := mcpUnauthorized("MCP with "+what, err); err != nil {
			return err
		}
	}
	step("REST and MCP refuse a request with no credential, or an unknown one, as 401 unauthenticated")

	// People sign in with a credential the operator issued, and nothing else
	// is consulted: conchd is the only server process in this run.
	who, err := h.nick.cli.run("", "whoami")
	if err != nil {
		return err
	}
	if !strings.Contains(who, "name: nick") || !strings.Contains(who, "kind: human") || !strings.Contains(who, fmt.Sprintf("id: %d", h.nick.id)) {
		return fmt.Errorf("conch whoami after conch login does not name nick, a human, id %d", h.nick.id)
	}
	// A made-up credential: the login command itself must fail, name nobody,
	// and store nothing.
	nobody, err := h.cliUser("nobody")
	if err != nil {
		return err
	}
	out, _, err := nobody.runSplit(madeUpToken+"\n", "login")
	if err == nil {
		return fmt.Errorf("conch login exited 0 for a credential nobody was issued")
	}
	if strings.Contains(out, "logged in") {
		return fmt.Errorf("conch login failed for a made-up credential and still printed a login line")
	}
	if _, stderr, err := nobody.runSplit("", "whoami"); err == nil || !strings.Contains(stderr, "not logged in") {
		return fmt.Errorf("conch whoami after a refused login: failed: %v; want it to fail and say nobody is logged in", err != nil)
	}
	step("a person signs in with conch login and an operator-issued credential; a made-up one is refused; no identity service exists in this run")

	// Two channels P1 never had: lab, where the second agent works, and
	// vault, which no agent is granted.
	if h.labID, err = h.createChannel("lab"); err != nil {
		return err
	}
	if _, err = h.createChannel("vault"); err != nil {
		return err
	}
	// nick is in both, so that a person can check what the agents left there.
	for _, channel := range []string{"lab", "vault"} {
		if err := h.addMember(channel, h.nick); err != nil {
			return err
		}
	}
	if h.outsider, err = h.newHuman("outsider"); err != nil {
		return err
	}

	// leviathan is the agent P1 knew. It is a member of P1's channels (the
	// upgrade kept everyone's access to them), and that is all it has: with
	// no manifest it can do nothing, so an upgrade grants no agent anything.
	leviathanCredential, token, err := h.issue(h.leviathan.id, "leviathan")
	if err != nil {
		return err
	}
	h.leviathan.token = token
	if err := h.connect(h.leviathan); err != nil {
		return err
	}
	for _, c := range []struct {
		what, tool string
		args       map[string]any
	}{
		{"post", "post_message", map[string]any{"channel": "ops", "body": "too early"}},
		{"read", "read_channel", map[string]any{"channel": "ops"}},
	} {
		if _, err := toolRefused(h.leviathan, "a P1 agent with no manifest must not "+c.what, c.tool, c.args, "forbidden"); err != nil {
			return err
		}
	}
	step("the agent P1 knew authenticates after the upgrade and can do nothing until it has a manifest")

	courierID, err := h.createPrincipal(schema.PrincipalAgent, "courier")
	if err != nil {
		return err
	}
	h.courier = &person{name: "courier", id: courierID}
	if _, h.courier.token, err = h.issue(courierID, "courier"); err != nil {
		return err
	}
	if err := h.addMember("lab", h.courier); err != nil {
		return err
	}
	// Two different manifests: leviathan may do everything, in ops only;
	// courier may exchange messages, in lab only.
	if err := h.putManifest(h.leviathan, schema.Capabilities(), nil, readPost(h.opsID)); err != nil {
		return err
	}
	messagesOnly := []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost}
	if err := h.putManifest(h.courier, messagesOnly, nil, readPost(h.labID)); err != nil {
		return err
	}
	if err := h.connect(h.courier); err != nil {
		return err
	}
	for _, c := range []struct {
		agent, other *person
		caps         []schema.Capability
		channel      int64
	}{
		{h.leviathan, h.courier, schema.Capabilities(), h.opsID},
		{h.courier, h.leviathan, messagesOnly, h.labID},
	} {
		var own schema.GetAgentManifestResponseV1
		if err := h.as(c.agent).call(http.MethodGet, fmt.Sprintf("/v1/principals/%d/manifest", c.agent.id), nil, &own); err != nil {
			return fmt.Errorf("%s reads its own manifest: %w", c.agent.name, err)
		}
		m := own.Manifest
		if err := m.Validate(); err != nil {
			return err
		}
		if m.PrincipalID != c.agent.id || !reflect.DeepEqual(m.Capabilities, c.caps) || len(m.Channels) != 1 || m.Channels[0].ChannelID != c.channel {
			return fmt.Errorf("%s's manifest is for principal %d with capabilities %v in %d channels, want its own: %v in channel %d", c.agent.name, m.PrincipalID, m.Capabilities, len(m.Channels), c.caps, c.channel)
		}
		if _, err := h.as(c.agent).refused(c.agent.name+" reads "+c.other.name+"'s manifest", http.MethodGet, fmt.Sprintf("/v1/principals/%d/manifest", c.other.id), nil, http.StatusForbidden, "forbidden"); err != nil {
			return err
		}
		var who schema.WhoAmIResponseV1
		if err := h.as(c.agent).call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
			return err
		}
		if who.ID != c.agent.id || who.Kind != schema.PrincipalAgent {
			return fmt.Errorf("%s's credential names principal %d (%s), want agent %d", c.agent.name, who.ID, who.Kind, c.agent.id)
		}
	}
	step("two agents hold two manifests and two credentials; each reads its own manifest and not the other's")

	// Rotation: the old token stops at once, on REST and on the MCP session
	// that was open with it; the new one works.
	old := h.leviathan.token
	var rotated schema.RotateCredentialResponseV1
	if err := h.op().call(http.MethodPost, fmt.Sprintf("/v1/credentials/%d/rotate", leviathanCredential), nil, &rotated); err != nil {
		return fmt.Errorf("rotate leviathan's credential: %w", err)
	}
	if rotated.Token == "" || rotated.Token == old || rotated.Credential.PrincipalID != h.leviathan.id {
		return fmt.Errorf("rotation returned no new token, the old one, or a credential for principal %d", rotated.Credential.PrincipalID)
	}
	h.secret(rotated.Token)
	if _, err := (api{baseURL: h.proc.baseURL, token: old}).refused("the rotated-out token on REST", http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
		return err
	}
	_, err = h.leviathan.mcp.CallTool(runCtx, "read_channel", map[string]any{"channel": "ops"})
	if err := mcpUnauthorized("the MCP session opened with the rotated-out token", err); err != nil {
		return err
	}
	h.leviathan.token = rotated.Token
	if err := h.connect(h.leviathan); err != nil {
		return fmt.Errorf("the new token after rotation: %w", err)
	}
	if _, err := tool[schema.ListMessagesResponseV2](h.leviathan, "read_channel", map[string]any{"channel": "ops"}); err != nil {
		return fmt.Errorf("the new token after rotation: %w", err)
	}
	step("rotating a credential stops the old token at once, on REST and on its open MCP session; the new one works")

	// Revocation fails closed and deletes nothing: the credential stays on
	// the list as revoked, the principal's other credential still works, and
	// the trail has every step.
	spareID, spare, err := h.issue(h.courier.id, "courier-spare")
	if err != nil {
		return err
	}
	spareAPI := api{baseURL: h.proc.baseURL, token: spare}
	if err := spareAPI.call(http.MethodGet, "/v1/whoami", nil, nil); err != nil {
		return fmt.Errorf("courier's second credential before it is revoked: %w", err)
	}
	if err := h.op().call(http.MethodDelete, fmt.Sprintf("/v1/credentials/%d", spareID), nil, nil); err != nil {
		return fmt.Errorf("revoke courier's second credential: %w", err)
	}
	if _, err := spareAPI.refused("a revoked token on REST", http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
		return err
	}
	err = mcpclient.New(h.proc.baseURL, spare).Initialize(runCtx, name)
	if err := mcpUnauthorized("a revoked token on MCP", err); err != nil {
		return err
	}
	if _, err := tool[schema.ListMessagesResponseV2](h.courier, "read_channel", map[string]any{"channel": "lab"}); err != nil {
		return fmt.Errorf("courier's other credential after the revocation: %w", err)
	}
	var creds schema.ListCredentialsResponseV1
	if err := h.op().call(http.MethodGet, fmt.Sprintf("/v1/principals/%d/credentials", h.courier.id), nil, &creds); err != nil {
		return err
	}
	revoked, live := 0, 0
	for _, c := range creds.Credentials {
		switch {
		case c.ID == spareID && c.RevokedAt != nil:
			revoked++
		case c.RevokedAt == nil:
			live++
		}
	}
	if len(creds.Credentials) != 2 || revoked != 1 || live != 1 {
		return fmt.Errorf("courier's credentials after the revocation: %d listed, %d revoked, %d live; want 2 listed, the revoked one kept as revoked", len(creds.Credentials), revoked, live)
	}
	events, err := h.audit()
	if err != nil {
		return err
	}
	for _, c := range []struct {
		agent *person
		want  []string
	}{
		// leviathan also got one credential in the upgrade phase.
		{h.leviathan, []string{"credential_created", "credential_created", "credential_rotated"}},
		{h.courier, []string{"credential_created", "credential_created", "credential_revoked"}},
	} {
		var got []string
		for _, e := range events {
			if e.Subject == fmt.Sprintf("principal:%d", c.agent.id) && strings.HasPrefix(e.Action, "credential_") {
				got = append(got, e.Action)
			}
		}
		if !reflect.DeepEqual(got, c.want) {
			return fmt.Errorf("the audit trail of %s's credentials = %v, want %v", c.agent.name, got, c.want)
		}
	}
	step("revoking a credential stops it at once and deletes nothing: it stays listed as revoked, the principal keeps working, the trail has each step")
	return h.closeOut()
}

// closeOut shows the two ways an operator shuts a principal out entirely,
// revoking all its credentials and disabling it: every token it held stops,
// and the principal, what it wrote and its history are all still there.
func (h *harness) closeOut() error {
	id, err := h.createPrincipal(schema.PrincipalAgent, "drifter")
	if err != nil {
		return err
	}
	drifter := &person{name: "drifter", id: id}
	_, first, err := h.issue(id, "drifter-1")
	if err != nil {
		return err
	}
	_, second, err := h.issue(id, "drifter-2")
	if err != nil {
		return err
	}
	drifter.token = first
	if err := h.addMember("lab", drifter); err != nil {
		return err
	}
	if err := h.putManifest(drifter, []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost}, nil, readPost(h.labID)); err != nil {
		return err
	}
	if err := h.connect(drifter); err != nil {
		return err
	}
	const left = "drifter was here"
	if _, err := tool[schema.PostMessageResponseV2](drifter, "post_message", map[string]any{"channel": "lab", "body": left}); err != nil {
		return err
	}
	// stopped checks that a token is refused on REST and on MCP.
	stopped := func(what, token string) error {
		if _, err := (api{baseURL: h.proc.baseURL, token: token}).refused(what+" on REST", http.MethodGet, "/v1/whoami", nil, http.StatusUnauthorized, "unauthenticated"); err != nil {
			return err
		}
		return mcpUnauthorized(what+" on MCP", mcpclient.New(h.proc.baseURL, token).Initialize(runCtx, name))
	}
	// intact checks that the principal and what it wrote are still there.
	intact := func(after string) error {
		lab, err := h.allMessages(h.nick, "lab")
		if err != nil {
			return err
		}
		kept := false
		for _, m := range lab {
			if m.Body == left && m.AuthorID == id {
				kept = true
			}
		}
		if !kept {
			return fmt.Errorf("after %s, the message drifter posted is gone from lab", after)
		}
		var own schema.GetAgentManifestResponseV1
		if err := h.op().call(http.MethodGet, fmt.Sprintf("/v1/principals/%d/manifest", id), nil, &own); err != nil {
			return fmt.Errorf("after %s, drifter's manifest: %w", after, err)
		}
		return nil
	}

	credentials := fmt.Sprintf("/v1/principals/%d/credentials", id)
	var all schema.RevokeAllCredentialsResponseV1
	if err := h.op().call(http.MethodPost, credentials+"/revoke-all", nil, &all); err != nil {
		return err
	}
	if all.Revoked != 2 {
		return fmt.Errorf("revoke-all revoked %d credentials, want drifter's 2", all.Revoked)
	}
	for i, token := range []string{first, second} {
		if err := stopped(fmt.Sprintf("drifter's credential %d after revoke-all", i+1), token); err != nil {
			return err
		}
	}
	if err := intact("revoke-all"); err != nil {
		return err
	}
	// The principal is not disabled by that: a new credential works.
	_, third, err := h.issue(id, "drifter-3")
	if err != nil {
		return fmt.Errorf("a new credential after revoke-all: %w", err)
	}
	drifter.token = third
	if err := h.connect(drifter); err != nil {
		return err
	}

	// Disabled: the credential stops, none can be issued, and enabling it
	// again brings back the principal but no old credential.
	state := fmt.Sprintf("/v1/principals/%d/", id)
	if err := h.op().call(http.MethodPost, state+"disable", nil, nil); err != nil {
		return err
	}
	if err := stopped("a disabled principal's credential", third); err != nil {
		return err
	}
	if _, err := h.op().refused("a credential for a disabled principal", http.MethodPost, credentials, schema.CreateCredentialRequestV1{Label: "too late"}, http.StatusConflict, "principal_disabled"); err != nil {
		return err
	}
	if err := intact("disabling it"); err != nil {
		return err
	}
	if err := h.op().call(http.MethodPost, state+"enable", nil, nil); err != nil {
		return err
	}
	if err := stopped("a credential revoked by disabling, after the principal is enabled again", third); err != nil {
		return err
	}
	_, fourth, err := h.issue(id, "drifter-4")
	if err != nil {
		return fmt.Errorf("a new credential after enabling: %w", err)
	}
	drifter.token = fourth
	if err := h.connect(drifter); err != nil {
		return err
	}
	if _, err := tool[schema.ListMessagesResponseV2](drifter, "read_channel", map[string]any{"channel": "lab"}); err != nil {
		return fmt.Errorf("drifter after being enabled again: %w", err)
	}

	// The whole history is in the trail, in order.
	events, err := h.audit()
	if err != nil {
		return err
	}
	var got []string
	for _, e := range events {
		if e.Subject == fmt.Sprintf("principal:%d", id) && (strings.HasPrefix(e.Action, "credential") || strings.HasPrefix(e.Action, "principal_")) {
			got = append(got, e.Action)
		}
	}
	want := []string{
		"credential_created", "credential_created",
		"credential_revoked", "credential_revoked", "credentials_revoked_all",
		"credential_created",
		"principal_disabled", "credential_revoked", "principal_enabled",
		"credential_created",
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("the audit trail of drifter's credentials and state = %v, want %v", got, want)
	}
	step("revoke-all and disabling shut a principal out at once, on REST and MCP; its messages, manifest and history stay, and enabling it revives no old credential")
	return nil
}

// mcpUnauthorized checks that an MCP request failed with a 401: a credential
// problem is answered by HTTP, before any tool runs.
func mcpUnauthorized(what string, err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s: the call succeeded, want a 401", what)
	case !strings.Contains(err.Error(), "status 401"):
		return fmt.Errorf("%s: want a 401, got: %w", what, err)
	}
	return nil
}

// capabilities shows that what a manifest grants works and that everything
// else is refused with a stable error and an audit row.
func (h *harness) capabilities() error {
	// What leviathan is granted, in ops: post a typed message and read it
	// back, and a person sees the same message over REST.
	posted, err := tool[schema.PostMessageResponseV2](h.leviathan, "post_message", map[string]any{
		"channel": "ops", "body": "build 43 is green",
		"payload": map[string]any{"schema": "leviathan.deploy.v1", "data": map[string]any{"env": "prod", "build": 43}},
	})
	if err != nil {
		return err
	}
	if err := posted.Message.Validate(); err != nil {
		return err
	}
	if posted.Message.AuthorID != h.leviathan.id || posted.Message.ChannelID != h.opsID {
		return fmt.Errorf("leviathan's post is by principal %d in channel %d, want %d in %d", posted.Message.AuthorID, posted.Message.ChannelID, h.leviathan.id, h.opsID)
	}
	read, err := tool[schema.ListMessagesResponseV2](h.leviathan, "read_channel", map[string]any{"channel": "ops", "after": posted.Message.ID - 1})
	if err != nil {
		return err
	}
	var rest schema.ListMessagesResponseV2
	if err := h.as(h.nick).call(http.MethodGet, fmt.Sprintf("/v2/channels/ops/messages?after=%d", posted.Message.ID-1), nil, &rest); err != nil {
		return err
	}
	if len(read.Messages) != 1 || read.Messages[0].ID != posted.Message.ID || len(rest.Messages) != 1 || rest.Messages[0].ID != posted.Message.ID {
		return fmt.Errorf("leviathan's post %d: MCP reads back %d messages and REST %d, want that one message on both", posted.Message.ID, len(read.Messages), len(rest.Messages))
	}
	if _, err := tool[schema.PostMessageResponseV2](h.courier, "post_message", map[string]any{"channel": "lab", "body": "courier online"}); err != nil {
		return err
	}
	step("each agent reads and posts in the channel its manifest grants")

	// The approval the refusals below aim at: raised by leviathan in ops and
	// left pending. The approval chain decides it later.
	pending, err := h.requestApproval(h.leviathan, "Deploy build 43")
	if err != nil {
		return err
	}
	h.pendingForChain = pending
	// ntfy is told once that it exists.
	if err := h.notified("Deploy build 43", 1); err != nil {
		return err
	}

	decision := map[string]any{"option_id": "approve", "reason": "an agent must never decide"}
	decisions := fmt.Sprintf("/v1/approvals/%d/decisions", pending)
	type refusal struct {
		what string
		try  func(what string) error
	}
	viaMCP := func(p *person, toolName string, args map[string]any, code string) func(string) error {
		return func(what string) error { _, err := toolRefused(p, what, toolName, args, code); return err }
	}
	// Over REST an agent's refusal is always a 403: forbidden.
	viaREST := func(p *person, method, path string, body any) func(string) error {
		return func(what string) error {
			_, err := h.as(p).refused(what, method, path, body, http.StatusForbidden, "forbidden")
			return err
		}
	}
	run := func(agent *person, refusals []refusal) error {
		before, err := h.denials(agent)
		if err != nil {
			return err
		}
		for _, r := range refusals {
			if err := r.try(agent.name + " must not " + r.what); err != nil {
				return err
			}
		}
		after, err := h.denials(agent)
		if err != nil {
			return err
		}
		if after-before != len(refusals) {
			return fmt.Errorf("%d refusals of %s wrote %d access_denied rows, want one each", len(refusals), agent.name, after-before)
		}
		return nil
	}

	// leviathan, outside its grant. It is not a member of vault or lab, so
	// they look as if they did not exist; it is a member of general (P1) and
	// has no grant there, so that is forbidden.
	if err := run(h.leviathan, []refusal{
		{"read a channel it is not in", viaMCP(h.leviathan, "read_channel", map[string]any{"channel": "vault"}, "channel_not_found")},
		{"post to a channel it is not in", viaMCP(h.leviathan, "post_message", map[string]any{"channel": "vault", "body": "intrusion"}, "channel_not_found")},
		{"read another agent's channel", viaMCP(h.leviathan, "read_channel", map[string]any{"channel": "lab"}, "channel_not_found")},
		{"read a P1 channel its manifest does not grant", viaMCP(h.leviathan, "read_channel", map[string]any{"channel": "general"}, "forbidden")},
		{"post to a P1 channel its manifest does not grant", viaMCP(h.leviathan, "post_message", map[string]any{"channel": "general", "body": "intrusion"}, "forbidden")},
		{"read a P1 channel over REST", viaREST(h.leviathan, http.MethodGet, "/v2/channels/general/messages", nil)},
		{"decide an approval, even its own", viaREST(h.leviathan, http.MethodPost, decisions, decision)},
		{"list approvals over the human route", viaREST(h.leviathan, http.MethodGet, "/v1/approvals", nil)},
		{"create a channel", viaREST(h.leviathan, http.MethodPost, "/v0/channels", map[string]any{"name": "mine"})},
		{"write its own manifest", viaREST(h.leviathan, http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", h.leviathan.id), map[string]any{"display_name": "leviathan", "tier": "H"})},
		{"export the audit trail", viaREST(h.leviathan, http.MethodGet, "/v1/audit", nil)},
	}); err != nil {
		return err
	}
	// Membership alone is not a grant.
	if err := h.addMember("vault", h.leviathan); err != nil {
		return err
	}
	if err := run(h.leviathan, []refusal{
		{"read a channel it is in but not granted", viaMCP(h.leviathan, "read_channel", map[string]any{"channel": "vault"}, "forbidden")},
		{"post to a channel it is in but not granted", viaMCP(h.leviathan, "post_message", map[string]any{"channel": "vault", "body": "intrusion"}, "forbidden")},
	}); err != nil {
		return err
	}
	step("the approval-capable agent is refused everything outside its grant: other channels, the operator's routes, and deciding an approval")

	// courier may exchange messages and nothing else: it can neither raise
	// an approval nor see one nor decide one.
	approval := approvalArgs(h.labID, "should never exist")
	if err := run(h.courier, []refusal{
		{"raise an approval", viaMCP(h.courier, "request_approval", approval, "forbidden")},
		{"poll an approval", viaMCP(h.courier, "check_decision", map[string]any{"approval_id": pending}, "forbidden")},
		{"wait on an approval", viaMCP(h.courier, "await_decision", map[string]any{"approval_id": pending, "timeout_ms": 100}, "forbidden")},
		{"decide an approval", viaREST(h.courier, http.MethodPost, decisions, decision)},
		{"raise an approval over the human route", viaREST(h.courier, http.MethodPost, "/v1/approvals", approval)},
		{"read an approval over the human route", viaREST(h.courier, http.MethodGet, fmt.Sprintf("/v1/approvals/%d", pending), nil)},
		{"read the other agent's channel", viaMCP(h.courier, "read_channel", map[string]any{"channel": "ops"}, "channel_not_found")},
		{"post to the other agent's channel", viaMCP(h.courier, "post_message", map[string]any{"channel": "ops", "body": "intrusion"}, "channel_not_found")},
	}); err != nil {
		return err
	}
	step("the message-only agent can neither raise, watch nor decide an approval, nor reach the other agent's channel")

	// People are held to membership the same way, and to their own name.
	forged := map[string]any{"principal_id": h.nick.id, "option_id": "approve", "reason": "in someone else's name"}
	for _, c := range []struct {
		what, method, path string
		as                 *person
		body               any
		status             int
		code               string
	}{
		{"a non-member reads a channel", http.MethodGet, "/v1/channels/ops/messages", h.outsider, nil, http.StatusNotFound, "channel_not_found"},
		{"a non-member posts to a channel", http.MethodPost, "/v1/channels/ops/messages", h.outsider, map[string]any{"body": "intrusion"}, http.StatusNotFound, "channel_not_found"},
		{"a non-member decides an approval", http.MethodPost, decisions, h.outsider, decision, http.StatusNotFound, "approval_not_found"},
		{"a non-member reads an approval", http.MethodGet, fmt.Sprintf("/v1/approvals/%d", pending), h.outsider, nil, http.StatusNotFound, "approval_not_found"},
		{"a member decides in another person's name", http.MethodPost, decisions, h.ana, forged, http.StatusForbidden, "principal_mismatch"},
		{"a member uses an operator's route", http.MethodPost, "/v0/channels", h.ana, map[string]any{"name": "mine"}, http.StatusForbidden, "forbidden"},
	} {
		if _, err := h.as(c.as).refused(c.what, c.method, c.path, c.body, c.status, c.code); err != nil {
			return err
		}
	}
	step("people are held to membership and to their own name, with the same error shape")

	// Nothing above decided the approval or left a message behind.
	var still schema.GetApprovalResponseV1
	if err := h.as(h.nick).call(http.MethodGet, fmt.Sprintf("/v1/approvals/%d", pending), nil, &still); err != nil {
		return err
	}
	if still.Approval.State != schema.ApprovalStatePending || still.Resolution != nil {
		return fmt.Errorf("approval %d is %s after the refusals, want still pending with no decision", pending, still.Approval.State)
	}
	for _, channel := range []string{"ops", "general", "lab", "vault"} {
		list, err := h.allMessages(h.nick, channel)
		if err != nil {
			return err
		}
		for _, m := range list {
			if m.Body == "intrusion" {
				return fmt.Errorf("a refused post left message %d in %s", m.ID, channel)
			}
		}
	}
	step("no refused call changed anything")
	return nil
}

// denials counts the access_denied rows the audit trail holds for one
// principal.
func (h *harness) denials(p *person) (int, error) {
	events, err := h.audit()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range events {
		if e.Action == auditAccessDenied && e.Actor == fmt.Sprintf("principal:%d", p.id) {
			n++
		}
	}
	return n, nil
}

func approvalArgs(channelID int64, title string) map[string]any {
	return map[string]any{
		"channel_id": channelID, "title": title, "body": "raised by " + name,
		"options": []map[string]any{
			{"id": "approve", "kind": "approve", "label": "Approve"},
			{"id": "reject", "kind": "reject", "label": "Reject"},
		},
		"deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
}

// requestApproval raises an approval in ops through MCP and returns its id.
func (h *harness) requestApproval(agent *person, title string) (int64, error) {
	created, err := tool[schema.RequestApprovalOutput](agent, "request_approval", approvalArgs(h.opsID, title))
	if err != nil {
		return 0, err
	}
	if created.ID == 0 || created.State != schema.ApprovalStatePending {
		return 0, fmt.Errorf("request_approval returned id %d in state %s, want a pending approval", created.ID, created.State)
	}
	return created.ID, nil
}

// notified waits until the fake ntfy has answered exactly n deliveries about
// the approval with this title, all on the normal topic, and fails if it ever
// has more.
func (h *harness) notified(title string, n int) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := 0
		for _, hit := range h.ntfy.delivered() {
			if !strings.Contains(hit.Title, title) {
				continue
			}
			if hit.Topic != ntfyTopic {
				return fmt.Errorf("a notification about %q went to topic %q, want %q", title, hit.Topic, ntfyTopic)
			}
			got++
		}
		if got == n {
			return nil
		}
		if got > n || time.Now().After(deadline) {
			return fmt.Errorf("ntfy has %d deliveries about %q, want %d", got, title, n)
		}
		if err := pause(20 * time.Millisecond); err != nil {
			return err
		}
	}
}

// approvalChain runs the loop Conch exists for, four times: on the two
// approvals P1 left open, on a new one with ntfy answering, and on a new one
// with ntfy down.
func (h *harness) approvalChain() error {
	// 1. The approval P1 left pending. leviathan raised it under P1; today
	// nick approves it with the CLI and leviathan reads the outcome.
	p1Pending, err := h.p1.approval("pending", 1)
	if err != nil {
		return err
	}
	listed, err := h.nick.cli.run("", "approvals", "list")
	if err != nil {
		return err
	}
	if !strings.Contains(listed, p1Pending.title) {
		return fmt.Errorf("conch approvals list does not show the approval P1 left pending (id %d)", p1Pending.id)
	}
	if _, err := h.nick.cli.run("", "approve", "--reason", "approved after the upgrade", strconv.FormatInt(p1Pending.id, 10)); err != nil {
		return err
	}
	checked, err := tool[schema.CheckDecisionOutput](h.leviathan, "check_decision", map[string]any{"approval_id": p1Pending.id})
	if err != nil {
		return err
	}
	if err := wantResolution(checked.State, checked.Resolution, schema.OutcomeApproved, decided{h.nick.id, "approve", "approved after the upgrade"}); err != nil {
		return fmt.Errorf("the approval P1 left pending: %w", err)
	}
	// P1 could not tell anyone it was created; today's resolution is the one
	// notification about it.
	if err := h.notified(p1Pending.title, 1); err != nil {
		return err
	}
	// P1's rows (created, and a notification that failed then), and today's.
	if err := h.wantChain(p1Pending.id, auditApprovalCreated, auditNotifyFailed, auditDecisionCast, auditApprovalResolved, auditNotifySent); err != nil {
		return err
	}
	step("the approval P1 left pending is approved with conch approve; the agent that raised it under P1 reads the outcome")

	// 2. The approval P1 left half decided: quorum 2, nick's decision on
	// record. ana's is the second, and the resolution holds both.
	half, err := h.p1.approval("pending", 2)
	if err != nil {
		return err
	}
	if len(half.decisions) != 1 || half.decisions[0].principalID != h.nick.id {
		return fmt.Errorf("the P1 fixture's quorum-2 approval has %d decisions, want nick's one", len(half.decisions))
	}
	var cast schema.CastDecisionResponseV1
	if err := h.as(h.ana).call(http.MethodPost, fmt.Sprintf("/v1/approvals/%d/decisions", half.id),
		schema.CastDecisionRequestV1{PrincipalID: h.ana.id, OptionID: "approve", Reason: "second of two"}, &cast); err != nil {
		return err
	}
	var read schema.GetApprovalResponseV1
	if err := h.as(h.nick).call(http.MethodGet, fmt.Sprintf("/v1/approvals/%d", half.id), nil, &read); err != nil {
		return err
	}
	if err := wantResolution(read.Approval.State, read.Resolution, schema.OutcomeApproved,
		decided{h.nick.id, "approve", half.decisions[0].reason}, decided{h.ana.id, "approve", "second of two"}); err != nil {
		return fmt.Errorf("the approval P1 left half decided: %w", err)
	}
	if err := h.notified(half.title, 1); err != nil {
		return err
	}
	if err := h.wantChain(half.id, auditApprovalCreated, auditNotifyFailed, auditDecisionCast, auditDecisionCast, auditApprovalResolved, auditNotifySent); err != nil {
		return err
	}
	step("the approval P1 left with one of two decisions resolves on the second; the resolution holds P1's decision and today's")

	// 3. The whole chain today, ntfy answering: the approval the capability
	// phase raised. await_decision blocks, a person decides with the CLI,
	// and every reader sees one resolution.
	id := h.pendingForChain
	const title = "Deploy build 43"
	if err := h.notified(title, 1); err != nil {
		return fmt.Errorf("the creation of approval %d: %w", id, err)
	}
	pendingCheck, err := tool[schema.CheckDecisionOutput](h.leviathan, "check_decision", map[string]any{"approval_id": id})
	if err != nil {
		return err
	}
	if pendingCheck.State != schema.ApprovalStatePending || pendingCheck.Resolution != nil {
		return fmt.Errorf("check_decision before anyone decided: state %s, want pending with no resolution", pendingCheck.State)
	}
	// await_decision waits: with nobody deciding, it comes back only when
	// its own timeout has passed, and says the approval is still pending.
	const shortWait = 600 * time.Millisecond
	began := time.Now()
	timedOut, err := tool[schema.AwaitDecisionOutput](h.leviathan, "await_decision", map[string]any{"approval_id": id, "timeout_ms": shortWait.Milliseconds()})
	if err != nil {
		return err
	}
	if waited := time.Since(began); waited < shortWait || timedOut.State != schema.ApprovalStatePending || timedOut.Resolution != nil {
		return fmt.Errorf("await_decision with a %s timeout and nobody deciding returned after %s in state %s, want it to wait the timeout out and report pending", shortWait, waited.Round(time.Millisecond), timedOut.State)
	}

	// And it returns when someone decides. The decision is cast only once
	// the request is known to have reached conchd, so that what comes back
	// is the wait being woken and not a call that found the answer ready.
	type awaited struct {
		out schema.AwaitDecisionOutput
		err error
	}
	waiting := make(chan awaited, 1)
	sent := make(chan struct{})
	var sentOnce sync.Once
	go func() {
		ctx, cancel := context.WithTimeout(runCtx, 60*time.Second)
		defer cancel()
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { sentOnce.Do(func() { close(sent) }) },
		})
		raw, err := h.leviathan.mcp.CallTool(ctx, "await_decision", map[string]any{"approval_id": id, "timeout_ms": 30000})
		if err != nil {
			waiting <- awaited{err: fmt.Errorf("await_decision as leviathan: %w", err)}
			return
		}
		out, err := mcpclient.Decode[schema.AwaitDecisionOutput](raw)
		waiting <- awaited{out, err}
	}()
	select {
	case <-sent:
	case early := <-waiting:
		return fmt.Errorf("await_decision ended before its request was sent: %w", early.err)
	case <-runCtx.Done():
		return runCtx.Err()
	case <-time.After(20 * time.Second):
		return fmt.Errorf("the await_decision request was not sent within 20 seconds")
	}
	// conchd has the whole request. It must now be holding it.
	select {
	case early := <-waiting:
		if early.err != nil {
			return fmt.Errorf("await_decision failed before anyone decided: %w", early.err)
		}
		return fmt.Errorf("await_decision returned before anyone decided, with state %s", early.out.State)
	case <-runCtx.Done():
		return runCtx.Err()
	case <-time.After(400 * time.Millisecond):
	}
	listed, err = h.ana.cli.run("", "approvals", "list")
	if err != nil {
		return err
	}
	if !strings.Contains(listed, title) || !strings.Contains(listed, strconv.FormatInt(id, 10)) {
		return fmt.Errorf("conch approvals list does not show approval %d", id)
	}
	if _, err := h.ana.cli.run("", "approve", "--reason", "ship it", strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	var got awaited
	select {
	case got = <-waiting:
	case <-time.After(15 * time.Second):
		return fmt.Errorf("await_decision was still blocked 15 seconds after approval %d was decided", id)
	}
	if got.err != nil {
		return got.err
	}
	want := decided{h.ana.id, "approve", "ship it"}
	if err := wantResolution(got.out.State, got.out.Resolution, schema.OutcomeApproved, want); err != nil {
		return fmt.Errorf("await_decision: %w", err)
	}
	checked, err = tool[schema.CheckDecisionOutput](h.leviathan, "check_decision", map[string]any{"approval_id": id})
	if err != nil {
		return err
	}
	// A terminal approval read by id over REST (#65) is the same resolution.
	if err := h.as(h.nick).call(http.MethodGet, fmt.Sprintf("/v1/approvals/%d", id), nil, &read); err != nil {
		return err
	}
	if !reflect.DeepEqual(checked.Resolution, got.out.Resolution) || !reflect.DeepEqual(read.Resolution, got.out.Resolution) || read.Approval.State != schema.ApprovalStateResolved {
		return fmt.Errorf("approval %d: await_decision, check_decision and GET /v1/approvals/{id} do not give one resolution", id)
	}
	if err := h.notified(title, 2); err != nil {
		return fmt.Errorf("the resolution of approval %d: %w", id, err)
	}
	if err := h.wantChain(id, auditApprovalCreated, auditNotifySent, auditDecisionCast, auditApprovalResolved, auditNotifySent); err != nil {
		return err
	}
	step("await_decision waits out its timeout while nobody decides, and is woken by conch approve; it, check_decision and the REST read by id give one resolution; ntfy is told twice; the audit chain is in order")

	// 4. The same with ntfy down: nothing waits for it, the approval is
	// still decided, and the trail says the notifications failed.
	h.ntfy.setDown(true)
	const degradedTitle = "Roll back build 43"
	degraded, err := h.requestApproval(h.leviathan, degradedTitle)
	if err != nil {
		return fmt.Errorf("with ntfy down: %w", err)
	}
	if _, err := h.nick.cli.run("", "reject", "--reason", "not needed after all", strconv.FormatInt(degraded, 10)); err != nil {
		return fmt.Errorf("with ntfy down: %w", err)
	}
	checked, err = tool[schema.CheckDecisionOutput](h.leviathan, "check_decision", map[string]any{"approval_id": degraded})
	if err != nil {
		return err
	}
	if err := wantResolution(checked.State, checked.Resolution, schema.OutcomeRejected, decided{h.nick.id, "reject", "not needed after all"}); err != nil {
		return fmt.Errorf("with ntfy down: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = h.wantChain(degraded, auditApprovalCreated, auditNotifyFailed, auditDecisionCast, auditApprovalResolved, auditNotifyFailed)
		if err == nil || time.Now().After(deadline) || pause(50*time.Millisecond) != nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("with ntfy down: %w", err)
	}
	// The two failures are two real attempts: conchd tried ntfy once for the
	// creation and once for the resolution, got no answer, and did not keep
	// trying. Nothing about this approval was ever delivered.
	attempts := 0
	for _, hit := range h.ntfy.attemptedWhileDown() {
		if strings.Contains(hit.Title, degradedTitle) && hit.Topic == ntfyTopic {
			attempts++
		}
	}
	if attempts != 2 {
		return fmt.Errorf("with ntfy down: conchd made %d delivery attempts about approval %d, want 2 (created, resolved)", attempts, degraded)
	}
	if err := h.notified(degradedTitle, 0); err != nil {
		return fmt.Errorf("with ntfy down: %w", err)
	}
	h.ntfy.setDown(false)
	h.degraded = degraded
	step("with ntfy down the approval is raised and rejected all the same; conchd tried ntfy twice, and both failures are in the audit trail")
	return nil
}

// decided is one decision a resolution must hold.
type decided struct {
	principalID      int64
	optionID, reason string
}

// wantResolution checks a resolved approval's outcome and its decisions, in
// order.
func wantResolution(state schema.ApprovalState, r *schema.ApprovalResolutionV1, outcome schema.Outcome, want ...decided) error {
	if state != schema.ApprovalStateResolved || r == nil {
		return fmt.Errorf("state %s with a resolution: %v; want resolved with one", state, r != nil)
	}
	if r.Outcome != outcome || len(r.Decisions) != len(want) {
		return fmt.Errorf("outcome %s with %d decisions, want %s with %d", r.Outcome, len(r.Decisions), outcome, len(want))
	}
	for i, w := range want {
		d := r.Decisions[i]
		if d.PrincipalID != w.principalID || d.OptionID != w.optionID || d.Reason != w.reason {
			return fmt.Errorf("decision %d is by principal %d for %s, want principal %d for %s with the reason given", i+1, d.PrincipalID, d.OptionID, w.principalID, w.optionID)
		}
	}
	return nil
}

// rateLimits shows that an agent at its limit is refused with a retry time,
// over MCP and over REST, and that nobody else is slowed by it: not a second
// agent with the very same limit, not an agent with none, not a person.
func (h *harness) rateLimits() error {
	const max = 3
	limit := []schema.RateLimit{{Capability: schema.CapabilityMessagesPost, Max: max, WindowSeconds: 3600}}
	limited := func(who string) (*person, error) {
		id, err := h.createPrincipal(schema.PrincipalAgent, who)
		if err != nil {
			return nil, err
		}
		p := &person{name: who, id: id}
		if _, p.token, err = h.issue(id, who); err != nil {
			return nil, err
		}
		if err := h.addMember("lab", p); err != nil {
			return nil, err
		}
		if err := h.putManifest(p, []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost}, limit, readPost(h.labID)); err != nil {
			return nil, err
		}
		return p, h.connect(p)
	}
	ticker, err := limited("ticker")
	if err != nil {
		return err
	}
	tocker, err := limited("tocker")
	if err != nil {
		return err
	}
	// spend uses an agent's whole budget and checks the post after it is
	// refused, with a retry time, over MCP and over REST.
	spend := func(agent *person) error {
		for i := 1; i <= max; i++ {
			if _, err := tool[schema.PostMessageResponseV2](agent, "post_message", map[string]any{"channel": "lab", "body": fmt.Sprintf("%s %d", agent.name, i)}); err != nil {
				return fmt.Errorf("post %d of %d inside the limit: %w", i, max, err)
			}
		}
		refusedTool, err := toolRefused(agent, "the post past the limit", "post_message", map[string]any{"channel": "lab", "body": "one too many"}, schema.ErrorCodeRateLimited)
		if err != nil {
			return err
		}
		if refusedTool.RetryAfterSeconds <= 0 || refusedTool.RetryAfterSeconds > 3600 {
			return fmt.Errorf("the MCP refusal of %s says to retry in %d seconds, want a time within the hour's window", agent.name, refusedTool.RetryAfterSeconds)
		}
		resp, err := h.as(agent).refused("the post past the limit over REST", http.MethodPost, "/v2/channels/lab/messages", map[string]any{"body": "one too many"}, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		if err != nil {
			return err
		}
		var body schema.Error
		if err := decodeStrict(resp.body, &body); err != nil {
			return err
		}
		header, err := strconv.Atoi(resp.header.Get("Retry-After"))
		if err != nil || header <= 0 || header != body.RetryAfterSeconds {
			return fmt.Errorf("the REST refusal of %s has Retry-After %q and retry_after_seconds %d, want the same positive number in both", agent.name, resp.header.Get("Retry-After"), body.RetryAfterSeconds)
		}
		return nil
	}
	if err := spend(ticker); err != nil {
		return err
	}
	// Reading is a different capability and has its own budget (none here).
	if _, err := tool[schema.ListMessagesResponseV2](ticker, "read_channel", map[string]any{"channel": "lab"}); err != nil {
		return fmt.Errorf("a read while the post budget is spent: %w", err)
	}
	step("an agent at its post limit is refused as rate_limited with a retry time, over MCP and REST; its reads go on")

	// tocker has the same limit in the same channel and has used none of it:
	// with ticker refused, tocker still has all three of its own.
	if err := spend(tocker); err != nil {
		return fmt.Errorf("the second agent's own budget while the first is at its limit: %w", err)
	}
	// Agents with no limit and a person post as before.
	for i := 1; i <= max+2; i++ {
		if _, err := tool[schema.PostMessageResponseV2](h.courier, "post_message", map[string]any{"channel": "lab", "body": fmt.Sprintf("courier %d", i)}); err != nil {
			return fmt.Errorf("courier's post %d while two agents are at their limit: %w", i, err)
		}
		if _, err := tool[schema.PostMessageResponseV2](h.leviathan, "post_message", map[string]any{"channel": "ops", "body": fmt.Sprintf("leviathan %d", i)}); err != nil {
			return fmt.Errorf("leviathan's post %d while two agents are at their limit: %w", i, err)
		}
	}
	if err := h.as(h.nick).call(http.MethodPost, "/v2/channels/lab/messages", map[string]any{"body": "people are not rate limited by an agent"}, nil); err != nil {
		return err
	}
	lab, err := h.allMessages(h.nick, "lab")
	if err != nil {
		return err
	}
	posts := map[int64]int{}
	for _, m := range lab {
		posts[m.AuthorID]++
	}
	if posts[ticker.id] != max || posts[tocker.id] != max {
		return fmt.Errorf("ticker has %d messages in lab and tocker %d, want exactly the %d inside each one's limit", posts[ticker.id], posts[tocker.id], max)
	}
	// The trail names who hit which limit, and nobody else.
	events, err := h.audit()
	if err != nil {
		return err
	}
	rows := map[string]int{}
	for _, e := range events {
		if e.Action != auditAgentRateLimited {
			continue
		}
		if !strings.Contains(e.Detail, "capability=messages.post max=3 window_seconds=3600") {
			return fmt.Errorf("an agent_rate_limited row by %s has detail %q, want it to name the limit", e.Actor, e.Detail)
		}
		rows[e.Actor]++
	}
	want := map[string]int{fmt.Sprintf("principal:%d", ticker.id): 2, fmt.Sprintf("principal:%d", tocker.id): 2}
	if !reflect.DeepEqual(rows, want) {
		return fmt.Errorf("agent_rate_limited rows by actor = %v, want two each for ticker and tocker and none for anyone else", rows)
	}
	step("one agent at its limit slows nobody: a second agent with the same limit has its whole budget, and agents without one and a person post freely")
	return nil
}

// webhook shows that the webhook URL a P1 conchd handed out survives the
// upgrade (the upgrade stores only its hash) and is held to today's rules: it
// posts as its agent, so it does nothing until that agent has a manifest.
func (h *harness) webhook() error {
	if h.p1.hookAuthor != h.scout.id || h.p1.hookChannel != h.opsID {
		return fmt.Errorf("the P1 fixture's webhook is for principal %d in channel %d, want scout in ops", h.p1.hookAuthor, h.p1.hookChannel)
	}
	hook := "/v1/hooks/" + h.p1.hookToken
	before, err := h.denials(h.scout)
	if err != nil {
		return err
	}
	// scout has no manifest: the URL answers exactly as an unknown one does.
	if _, err := h.anonymous().refused("P1's webhook before its agent has a manifest", http.MethodPost, hook, map[string]any{"body": "too early"}, http.StatusNotFound, "hook_not_found"); err != nil {
		return err
	}
	if _, err := h.anonymous().refused("a webhook URL nobody was given", http.MethodPost, "/v1/hooks/not-a-token-anyone-was-given", map[string]any{"body": "guess"}, http.StatusNotFound, "hook_not_found"); err != nil {
		return err
	}
	after, err := h.denials(h.scout)
	if err != nil {
		return err
	}
	if after-before != 1 {
		return fmt.Errorf("the refused webhook post wrote %d access_denied rows for scout, want 1", after-before)
	}
	if err := h.putManifest(h.scout, []schema.Capability{schema.CapabilityMessagesPost}, nil,
		schema.ChannelGrant{ChannelID: h.opsID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionPost}}); err != nil {
		return err
	}
	const alert = "alert from the webhook P1 handed out"
	if err := h.anonymous().call(http.MethodPost, hook, map[string]any{"body": alert}, nil); err != nil {
		return fmt.Errorf("P1's webhook once its agent may post: %w", err)
	}
	list, err := h.allMessages(h.nick, "ops")
	if err != nil {
		return err
	}
	last := list[len(list)-1]
	if last.Body != alert || last.AuthorID != h.scout.id {
		return fmt.Errorf("the newest message in ops is by principal %d, want the webhook's post by scout (%d)", last.AuthorID, h.scout.id)
	}
	// The operator can see that the hook exists; nobody is shown its token
	// again.
	resp, err := h.op().send(http.MethodGet, "/v1/hooks", nil)
	if err != nil {
		return err
	}
	if resp.status != http.StatusOK || strings.Contains(string(resp.body), h.p1.hookToken) {
		return fmt.Errorf("GET /v1/hooks: status %d; it must list the hook and must not show its token", resp.status)
	}
	var hooks schema.ListHooksResponseV1
	if err := decodeStrict(resp.body, &hooks); err != nil {
		return err
	}
	if len(hooks.Hooks) != 1 || hooks.Hooks[0].Principal != h.scout.id || hooks.Hooks[0].Channel != "ops" || hooks.Hooks[0].RevokedAt != nil {
		return fmt.Errorf("GET /v1/hooks lists %d hooks, want P1's one, live, for scout in ops", len(hooks.Hooks))
	}
	step("the webhook URL P1 handed out still posts as its agent, once that agent has a manifest; before that it looks like no hook at all")
	return nil
}
