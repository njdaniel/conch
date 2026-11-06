package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AI administrators (issue #1326, docs/design/ai-administrators.md §3.3 to
// §3.6, §6): an agent whose manifest grants it operator permissions acts
// through a closed catalog of named actions, each mapped by the server to one
// existing REST route. A direct action runs at once; a gated one first raises
// an ordinary approval whose payload, conch.admin_action.v1, binds the
// decision to this exact action, and runs once after the approval resolves
// approved.
//
// This file holds the catalog's vocabulary, the payload, and the bodies of
// the three routes behind the door (POST /v1/admin/actions and its two
// companions, design §3.4). The catalog table itself (each action's route,
// permission, default gate and target kind) is the server's
// (internal/server/admin_actions.go), kept honest by a structural test; the
// schema says only which names exist. Everything here is additive and the
// REST bodies are versioned by their V1 suffix; the payload carries its name
// in the approval's Payload.Schema.

// AdminAction names one administrative action an AI administrator may take
// (design §3.3). The vocabulary is closed on the server, which refuses an
// unknown value, and client-tolerant: a client shows a value it does not know
// by its name and never sends one. It grows only by adding constants, at the
// end, in AdminActions and in Vocabularies; an action is never renamed, and
// one that is withdrawn stays a name the server refuses.
type AdminAction string

// The catalog, in the order of the design's table.
const (
	// Members (manage_members).
	AdminActionListMembers          AdminAction = "list_members"
	AdminActionCreateMember         AdminAction = "create_member"
	AdminActionRenameMember         AdminAction = "rename_member"
	AdminActionSetVoicePermission   AdminAction = "set_voice_permission"
	AdminActionSetInvitePermission  AdminAction = "set_invite_permission"
	AdminActionDisableMember        AdminAction = "disable_member"
	AdminActionEnableMember         AdminAction = "enable_member"
	AdminActionHideMember           AdminAction = "hide_member"
	AdminActionListCredentials      AdminAction = "list_credentials"
	AdminActionRevokeCredential     AdminAction = "revoke_credential"      // #nosec G101 -- an action name, not a credential
	AdminActionRevokeAllCredentials AdminAction = "revoke_all_credentials" // #nosec G101 -- an action name, not a credential
	AdminActionListHooks            AdminAction = "list_hooks"
	AdminActionRevokeHook           AdminAction = "revoke_hook"
	// Channels (manage_channels).
	AdminActionListChannels           AdminAction = "list_channels"
	AdminActionCreateChannel          AdminAction = "create_channel"
	AdminActionRenameChannel          AdminAction = "rename_channel"
	AdminActionSetChannelDefault      AdminAction = "set_channel_default"
	AdminActionSetChannelVisibility   AdminAction = "set_channel_visibility"
	AdminActionArchiveChannel         AdminAction = "archive_channel"
	AdminActionRestoreChannel         AdminAction = "restore_channel"
	AdminActionListChannelMembers     AdminAction = "list_channel_members"
	AdminActionAddChannelMember       AdminAction = "add_channel_member"
	AdminActionRemoveChannelMember    AdminAction = "remove_channel_member"
	AdminActionSetChannelMemberRights AdminAction = "set_channel_member_rights"
	// Member links (make_member_links to create, manage_members otherwise).
	AdminActionCreateMemberLink   AdminAction = "create_member_link"
	AdminActionListMemberLinks    AdminAction = "list_member_links"
	AdminActionCloseMemberLink    AdminAction = "close_member_link"
	AdminActionListLinkJoiners    AdminAction = "list_link_joiners"
	AdminActionDisableLinkJoiners AdminAction = "disable_link_joiners"
	AdminActionDisableInvitees    AdminAction = "disable_invitees"
	AdminActionPauseInvites       AdminAction = "pause_invites"
	AdminActionResumeInvites      AdminAction = "resume_invites"
	// Server settings (change_server_settings).
	AdminActionGetJoinSettings AdminAction = "get_join_settings"
	AdminActionSetJoinSettings AdminAction = "set_join_settings"
	AdminActionGetPermissions  AdminAction = "get_permissions"
	AdminActionSetPermissions  AdminAction = "set_permissions"
	// Problem reports (manage_members).
	AdminActionListReports  AdminAction = "list_reports"
	AdminActionGetReport    AdminAction = "get_report"
	AdminActionUpdateReport AdminAction = "update_report"
	// The audit trail (read_audit_log).
	AdminActionExportAudit AdminAction = "export_audit"
	// Channel entry bans (manage_channels; moderation M3, issue #851, signed
	// off with #849 Q4). Added at the end, as the vocabulary only grows.
	AdminActionListChannelBans    AdminAction = "list_channel_bans"
	AdminActionBanChannelMember   AdminAction = "ban_channel_member"
	AdminActionUnbanChannelMember AdminAction = "unban_channel_member"
)

// AdminActions is the catalog in wire order: the order of every list of
// actions on the wire and in every view.
var AdminActions = []AdminAction{
	AdminActionListMembers, AdminActionCreateMember, AdminActionRenameMember,
	AdminActionSetVoicePermission, AdminActionSetInvitePermission,
	AdminActionDisableMember, AdminActionEnableMember, AdminActionHideMember,
	AdminActionListCredentials, AdminActionRevokeCredential, AdminActionRevokeAllCredentials,
	AdminActionListHooks, AdminActionRevokeHook,
	AdminActionListChannels, AdminActionCreateChannel, AdminActionRenameChannel,
	AdminActionSetChannelDefault, AdminActionSetChannelVisibility,
	AdminActionArchiveChannel, AdminActionRestoreChannel,
	AdminActionListChannelMembers, AdminActionAddChannelMember,
	AdminActionRemoveChannelMember, AdminActionSetChannelMemberRights,
	AdminActionCreateMemberLink, AdminActionListMemberLinks, AdminActionCloseMemberLink,
	AdminActionListLinkJoiners, AdminActionDisableLinkJoiners, AdminActionDisableInvitees,
	AdminActionPauseInvites, AdminActionResumeInvites,
	AdminActionGetJoinSettings, AdminActionSetJoinSettings,
	AdminActionGetPermissions, AdminActionSetPermissions,
	AdminActionListReports, AdminActionGetReport, AdminActionUpdateReport,
	AdminActionExportAudit,
	AdminActionListChannelBans, AdminActionBanChannelMember, AdminActionUnbanChannelMember,
}

// Valid reports whether a is an action of the catalog.
func (a AdminAction) Valid() bool {
	switch a {
	case AdminActionListMembers, AdminActionCreateMember, AdminActionRenameMember,
		AdminActionSetVoicePermission, AdminActionSetInvitePermission,
		AdminActionDisableMember, AdminActionEnableMember, AdminActionHideMember,
		AdminActionListCredentials, AdminActionRevokeCredential, AdminActionRevokeAllCredentials,
		AdminActionListHooks, AdminActionRevokeHook,
		AdminActionListChannels, AdminActionCreateChannel, AdminActionRenameChannel,
		AdminActionSetChannelDefault, AdminActionSetChannelVisibility,
		AdminActionArchiveChannel, AdminActionRestoreChannel,
		AdminActionListChannelMembers, AdminActionAddChannelMember,
		AdminActionRemoveChannelMember, AdminActionSetChannelMemberRights,
		AdminActionCreateMemberLink, AdminActionListMemberLinks, AdminActionCloseMemberLink,
		AdminActionListLinkJoiners, AdminActionDisableLinkJoiners, AdminActionDisableInvitees,
		AdminActionPauseInvites, AdminActionResumeInvites,
		AdminActionGetJoinSettings, AdminActionSetJoinSettings,
		AdminActionGetPermissions, AdminActionSetPermissions,
		AdminActionListReports, AdminActionGetReport, AdminActionUpdateReport,
		AdminActionExportAudit,
		AdminActionListChannelBans, AdminActionBanChannelMember, AdminActionUnbanChannelMember:
		return true
	default:
		return false
	}
}

// AdminGate is an action's gate class (design §3.3): direct runs at once,
// approval raises an approval first. The catalog's gate is the server's
// fixed default; a manifest's admin.require_approval moves a direct action
// to approval for that agent, never the reverse. Closed; a client switches
// on it.
type AdminGate string

const (
	AdminGateDirect   AdminGate = "direct"
	AdminGateApproval AdminGate = "approval"
)

// Valid reports whether g is a gate class.
func (g AdminGate) Valid() bool {
	switch g {
	case AdminGateDirect, AdminGateApproval:
		return true
	default:
		return false
	}
}

// AdminTargetKind says what an AdminTargetV1's id names. Closed on the
// server and client-tolerant, like AdminAction: a later catalog row may act
// on a kind of thing no client knows yet, and the client shows it by its
// name.
type AdminTargetKind string

const (
	AdminTargetPrincipal  AdminTargetKind = "principal"
	AdminTargetChannel    AdminTargetKind = "channel"
	AdminTargetCredential AdminTargetKind = "credential"
	AdminTargetHook       AdminTargetKind = "hook"
	AdminTargetInvite     AdminTargetKind = "invite"
	AdminTargetReport     AdminTargetKind = "report"
)

// Valid reports whether k is a known target kind.
func (k AdminTargetKind) Valid() bool {
	switch k {
	case AdminTargetPrincipal, AdminTargetChannel, AdminTargetCredential,
		AdminTargetHook, AdminTargetInvite, AdminTargetReport:
		return true
	default:
		return false
	}
}

// AdminTargetV1 is the one thing an action acts on: the entity whose id
// fills the route's path (design §3.3, the Target column). A server-wide
// action (list_members, pause_invites, set_permissions, …) has none.
//
// An action that names two entities (add_channel_member,
// remove_channel_member, set_channel_member_rights) targets the channel, and
// the principal travels in the params as principal_id, where the add route's
// body already carries it; the dispatcher fills the member routes' path from
// it. The decider's picture names both, from the stored params, but only the
// target's name is re-checked at run time (design §3.5.2, target_changed):
// the second entity is bound by its id alone.
type AdminTargetV1 struct {
	// Kind says what ID names.
	Kind AdminTargetKind `json:"kind"`
	// ID is the entity's id, positive.
	ID int64 `json:"id"`
	// Name is the entity's name as the server read it at request time: a
	// principal's or channel's name, a credential's or hook's label, an
	// invite's code, a report's title. The server fills it when it composes
	// an AdminActionV1 and ignores it in a request; it is empty for an entity
	// that has none. At run time the target_changed check compares it with
	// the entity's current name (design §3.5.2).
	Name string `json:"name,omitempty"`
}

// Validate reports whether the target is well-formed: a known kind and a
// positive id. It cannot check that the entity exists; the server does.
func (t AdminTargetV1) Validate() error {
	if !t.Kind.Valid() {
		return fmt.Errorf("schema: admin target kind must be one of: %s", strings.Join(Vocabularies["AdminTargetKind"], ", "))
	}
	if t.ID <= 0 {
		return fmt.Errorf("schema: admin target id must be positive, got %d", t.ID)
	}
	return nil
}

// MaxAdminReasonChars is the longest reason an agent may attach to an action:
// the one text of the agent's that a decider sees (design §3.5.1).
const MaxAdminReasonChars = 500

// validateAdminReason holds a reason to MaxAdminReasonChars characters (code
// points, not bytes) of valid UTF-8 with no control character. The reason is
// quoted on one line of the approval's body, among lines the server composed
// (design §3.5.1); a newline, carriage return or tab in it could add a line
// that reads as one of the server's facts to a plain-text client, so every
// rune for which unicode.IsControl is true (C0, DEL and C1) is refused.
func validateAdminReason(reason string) error {
	if !utf8.ValidString(reason) {
		return errors.New("schema: admin action reason is not valid UTF-8")
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return fmt.Errorf("schema: admin action reason contains a control character (%U)", r)
		}
	}
	if n := utf8.RuneCountInString(reason); n > MaxAdminReasonChars {
		return fmt.Errorf("schema: admin action reason is %d characters, the most is %d", n, MaxAdminReasonChars)
	}
	return nil
}

// validateAdminParams requires params, when present, to be one JSON object:
// the dispatched route's body, which the catalog clamps.
func validateAdminParams(params json.RawMessage) error {
	if len(params) == 0 {
		return nil
	}
	if !json.Valid(params) {
		return errors.New("schema: admin action params is not valid JSON")
	}
	trimmed := strings.TrimSpace(string(params))
	if !strings.HasPrefix(trimmed, "{") {
		return errors.New("schema: admin action params must be a JSON object")
	}
	return nil
}

// validateAdminRoute requires a route of the form "METHOD /path": the
// catalog's route with the target's ids filled in, e.g. "POST
// /v1/principals/7/disable".
func validateAdminRoute(route string) error {
	method, path, ok := strings.Cut(route, " ")
	if !ok || method == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \t\r\n") {
		return fmt.Errorf("schema: admin action route %q is not \"METHOD /path\"", route)
	}
	for _, r := range method {
		if r < 'A' || r > 'Z' {
			return fmt.Errorf("schema: admin action route %q is not \"METHOD /path\"", route)
		}
	}
	return nil
}

// AdminActionV1Name is the registered versioned name of the admin-action
// payload: what a gated action's approval carries (design §3.5.1, §3.5.2).
//
// The payload is server-composed only. Registering it here lets every client
// decode it; it does not let a client author it. An approval whose payload
// merely has this schema and shape proves nothing: the run step proves an
// approval's origin by the row the admin door writes in the same transaction
// as the approval, never by the payload's shape. The approval create routes
// (REST and request_approval) refuse a client-supplied payload of this
// schema (S3, #1330).
const AdminActionV1Name = "conch.admin_action.v1"

// AdminActionV1 is the machine form of a gated action, stored on its
// approval as the payload at request time, in the same transaction as
// approval_created. The agent's later run carries only the approval id; the
// server reads this back and runs exactly this, so there is no second place
// the action is spelled. Everything but Reason is composed by the server from
// the stored request; Reason is the agent's words, labelled as such and
// capped.
//
// Origin, and what is trusted (security review of #1328):
//
//   - The payload is server-composed only. The run step proves that an
//     approval was raised by the admin door by a row the door writes in the
//     same transaction as the approval, never by the payload's shape: an
//     approval carrying this schema that a client authored through
//     request_approval or POST /v1/approvals is not an admin action, and the
//     approval create routes refuse a client-supplied payload of this schema
//     (S3, #1330).
//   - Route is display only. The dispatcher rebuilds the method and path from
//     its catalog entry for Action and the target ids, and never parses Route.
//   - Target.Name is server-filled at request time, and is what the run
//     step's target_changed check compares against the entity's current name.
//
// Clients that know the schema render it as a card; those that do not show
// the approval's body, which says the same in prose.
type AdminActionV1 struct {
	// Action is the catalog action: the one field the dispatcher runs from,
	// with Target.ID and Params.
	Action AdminAction `json:"action"`
	// Route is the route the action runs, with the target's ids filled:
	// "POST /v1/principals/7/disable". Display only: a client shows it, and
	// the dispatcher rebuilds the method and path from the catalog entry for
	// Action and the target ids, never from this string.
	Route string `json:"route"`
	// Target is what the action acts on, with its name at request time;
	// omitted for a server-wide action. The server fills Name when it
	// composes the payload, and the run step's target_changed check compares
	// it with the entity's current name.
	Target *AdminTargetV1 `json:"target,omitempty"`
	// Params is the route's body as the agent sent it, verbatim, after the
	// catalog's clamps; omitted when there is none.
	Params json.RawMessage `json:"params,omitempty"`
	// AgentID is the agent that requested it, and the only principal that
	// may run it.
	AgentID int64 `json:"agent_id"`
	// Reason is the agent's stated reason, at most MaxAdminReasonChars;
	// omitted when it gave none.
	Reason string `json:"reason,omitempty"`
	// RequestedAt is when the action was requested (UTC, ms precision).
	RequestedAt Timestamp `json:"requested_at"`
}

// Validate reports whether the payload is well-formed. It cannot check that
// the route is the catalog's for the action or that the target exists; the
// server composed both and re-checks them at run time.
func (a AdminActionV1) Validate() error {
	if !a.Action.Valid() {
		return fmt.Errorf("schema: admin action %q is not in the catalog", a.Action)
	}
	if err := validateAdminRoute(a.Route); err != nil {
		return err
	}
	if a.Target != nil {
		if err := a.Target.Validate(); err != nil {
			return err
		}
	}
	if err := validateAdminParams(a.Params); err != nil {
		return err
	}
	if a.AgentID <= 0 {
		return fmt.Errorf("schema: admin action agent_id must be positive, got %d", a.AgentID)
	}
	if err := validateAdminReason(a.Reason); err != nil {
		return err
	}
	if a.RequestedAt.IsZero() {
		return errors.New("schema: admin action requested_at is required")
	}
	return nil
}

// SubmitAdminActionRequestV1 is the body of POST /v1/admin/actions and the
// input of the submit_admin_action MCP tool (design §3.4, §3.6): the action,
// what it acts on, the route's body, and why. The caller is the request's
// identity; the route, the gate and the target's name are the server's.
type SubmitAdminActionRequestV1 struct {
	// Action is the catalog action.
	Action AdminAction `json:"action"`
	// Target is what the action acts on: kind and id. Omitted for a
	// server-wide action. A name sent here is ignored; the server reads the
	// entity's name itself.
	Target *AdminTargetV1 `json:"target,omitempty"`
	// Params is the dispatched route's body, a JSON object, which the
	// catalog clamps. Omitted when the route takes none.
	Params json.RawMessage `json:"params,omitempty"`
	// Reason is shown to the decider of a gated action, labelled as the
	// agent's words, at most MaxAdminReasonChars. Optional.
	Reason string `json:"reason,omitempty"`
}

// Validate reports whether the request is well-formed on its own: a catalog
// action, a well-formed target when present, params that are one JSON
// object when present, and a reason within its cap. Whether the action takes
// a target of that kind, what the params may hold and whether the caller may
// act are the server's to decide, from the catalog and the caller's grant.
func (r SubmitAdminActionRequestV1) Validate() error {
	if !r.Action.Valid() {
		return fmt.Errorf("schema: admin action %q is not in the catalog", r.Action)
	}
	if r.Target != nil {
		if err := r.Target.Validate(); err != nil {
			return err
		}
	}
	if err := validateAdminParams(r.Params); err != nil {
		return err
	}
	return validateAdminReason(r.Reason)
}

// AdminActionStatus says what the door did with an action. Closed; a client
// switches on it, and it is the tag of AdminActionResultV1.
type AdminActionStatus string

const (
	// AdminActionRan: the action was dispatched and the route answered;
	// HTTPStatus and Result carry its answer, whatever it was.
	AdminActionRan AdminActionStatus = "ran"
	// AdminActionPendingApproval: the action is gated and an approval was
	// raised in the agent's approval channel; nothing ran. The agent watches
	// it with await_decision or check_decision and, once it resolves
	// approved, runs it with run_approved_action within the run window.
	AdminActionPendingApproval AdminActionStatus = "pending_approval"
)

// Valid reports whether s is a status.
func (s AdminActionStatus) Valid() bool {
	switch s {
	case AdminActionRan, AdminActionPendingApproval:
		return true
	default:
		return false
	}
}

// AdminActionResultV1 is the answer of POST /v1/admin/actions and of POST
// /v1/admin/actions/{approval_id}/run, and the output of the
// submit_admin_action and run_approved_action MCP tools (design §3.4). Which
// fields it has depends on Status.
//
// A refusal is not a result: it is the usual Error body, with the door's own
// codes (ErrorCodeAdminActionUnknown and the rest below) or the dispatched
// route's code passed through unchanged.
type AdminActionResultV1 struct {
	// Status picks the variant.
	Status AdminActionStatus `json:"status"`
	// HTTPStatus is the dispatched route's status code. Status ran only.
	HTTPStatus int `json:"http_status,omitempty"`
	// Result is the dispatched route's body, verbatim, or null when it
	// answered with none. Status ran only.
	Result json.RawMessage `json:"result,omitempty"`
	// ApprovalID is the approval raised for the action. Status
	// pending_approval only.
	ApprovalID int64 `json:"approval_id,omitempty"`
	// Approval is that approval as created. Status pending_approval only.
	Approval *ApprovalV1 `json:"approval,omitempty"`
	// Deadline is the approval's deadline, repeated so a caller need not
	// open the approval to learn how long it has. Status pending_approval
	// only.
	Deadline *Timestamp `json:"deadline,omitempty"`
}

// Validate reports whether the result has exactly the fields of its Status.
func (r AdminActionResultV1) Validate() error {
	switch r.Status {
	case AdminActionRan:
		if r.HTTPStatus < 100 || r.HTTPStatus > 599 {
			return fmt.Errorf("schema: a ran admin action result carries an HTTP status, got %d", r.HTTPStatus)
		}
		if len(r.Result) == 0 || !json.Valid(r.Result) {
			return errors.New("schema: a ran admin action result carries the route's body as JSON, or null")
		}
		if r.ApprovalID != 0 || r.Approval != nil || r.Deadline != nil {
			return errors.New("schema: a ran admin action result carries no approval")
		}
	case AdminActionPendingApproval:
		if r.ApprovalID <= 0 || r.Approval == nil || r.Deadline == nil {
			return errors.New("schema: a pending_approval admin action result carries approval_id, approval and deadline")
		}
		if err := r.Approval.Validate(); err != nil {
			return err
		}
		if r.Approval.ID != r.ApprovalID {
			return fmt.Errorf("schema: admin action result approval_id %d names a different approval than its approval (%d)", r.ApprovalID, r.Approval.ID)
		}
		if r.Deadline.IsZero() {
			return errors.New("schema: admin action result deadline is required")
		}
		if r.HTTPStatus != 0 || len(r.Result) != 0 {
			return errors.New("schema: a pending_approval admin action result carries no http_status or result")
		}
	default:
		return fmt.Errorf("schema: admin action status must be one of: %s", strings.Join(Vocabularies["AdminActionStatus"], ", "))
	}
	return nil
}

// AdminCatalogEntryV1 is one catalog action as it applies to the caller
// (design §3.4): the route it runs, the permission it needs, the server's
// default gate, and whether this caller holds the permission (Held) and
// would be gated (Gated: the default gate is approval, or the caller's
// manifest lists the action in admin.require_approval).
type AdminCatalogEntryV1 struct {
	Action     AdminAction `json:"action"`
	Route      string      `json:"route"`
	Permission Permission  `json:"permission"`
	Gate       AdminGate   `json:"gate"`
	Held       bool        `json:"held"`
	Gated      bool        `json:"gated"`
}

// Validate reports whether the entry is well-formed.
func (e AdminCatalogEntryV1) Validate() error {
	if !e.Action.Valid() {
		return fmt.Errorf("schema: admin catalog action %q is not in the catalog", e.Action)
	}
	if err := validateAdminRoute(e.Route); err != nil {
		return err
	}
	if !e.Permission.Valid() {
		return fmt.Errorf("schema: admin catalog entry %s: permission %q is not in the catalog", e.Action, e.Permission)
	}
	if !e.Gate.Valid() {
		return fmt.Errorf("schema: admin catalog entry %s: gate must be one of: %s", e.Action, strings.Join(Vocabularies["AdminGate"], ", "))
	}
	if e.Gate == AdminGateApproval && !e.Gated {
		return fmt.Errorf("schema: admin catalog entry %s: an action gated by default is gated for every caller", e.Action)
	}
	return nil
}

// AdminCatalogResponseV1 is the body of GET /v1/admin/actions and the output
// of the describe_admin_actions MCP tool (design §3.4): every catalog action
// as it applies to the caller, in catalog order, and the caller's own admin
// settings when the caller is an agent with a grant. Actions is never null.
type AdminCatalogResponseV1 struct {
	Actions []AdminCatalogEntryV1 `json:"actions"`
	// Admin is the caller's manifest admin settings, with the defaults
	// filled in; omitted for a human caller and for an agent with no grant.
	Admin *AgentAdminV1 `json:"admin,omitempty"`
}

// MarshalJSON writes Actions as [] when nil, never null.
func (r AdminCatalogResponseV1) MarshalJSON() ([]byte, error) {
	type wire AdminCatalogResponseV1
	w := wire(r)
	if w.Actions == nil {
		w.Actions = []AdminCatalogEntryV1{}
	}
	return json.Marshal(w)
}

// Validate reports whether the response is well-formed: every entry valid,
// each action at most once, and valid admin settings when present.
func (r AdminCatalogResponseV1) Validate() error {
	seen := make(map[AdminAction]struct{}, len(r.Actions))
	for i, e := range r.Actions {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("schema: admin catalog actions[%d]: %w", i, err)
		}
		if _, dup := seen[e.Action]; dup {
			return fmt.Errorf("schema: admin catalog lists %s more than once", e.Action)
		}
		seen[e.Action] = struct{}{}
	}
	if r.Admin != nil {
		return r.Admin.Validate()
	}
	return nil
}

// Error codes of the door (Error.Code; design §3.6). Codes of the dispatched
// route pass through unchanged: permission_denied with its permission,
// forbidden, last_operator, principal_not_found, channel_not_found,
// invalid_request, rate_limited and the rest.
const (
	// ErrorCodeAdminActionUnknown (HTTP 400): the action is not in the
	// catalog.
	ErrorCodeAdminActionUnknown = "admin_action_unknown"
	// ErrorCodeAdminActionNotAllowed (HTTP 403): the action is in the catalog
	// but refused before dispatch, whatever the caller holds: it would touch
	// the agent's approval channel, it names the agent itself as its target,
	// or its target is one the catalog excludes.
	ErrorCodeAdminActionNotAllowed = "admin_action_not_allowed"
	// ErrorCodeAdminActionPending (HTTP 409): the same action on the same
	// target with the same params already has an open approval; the message
	// names it.
	ErrorCodeAdminActionPending = "admin_action_pending"
	// ErrorCodeTooManyPendingAdminActions (HTTP 429): the agent has
	// admin.max_pending open gated approvals. retry_after_seconds is 0: it
	// clears when one resolves, not with time.
	ErrorCodeTooManyPendingAdminActions = "too_many_pending_admin_actions"
	// ErrorCodeApprovalNotApproved (HTTP 409): run_approved_action on an
	// approval that is pending, escalated, rejected or expired.
	ErrorCodeApprovalNotApproved = "approval_not_approved"
	// ErrorCodeApprovalAlreadyRun (HTTP 409): the approval's action was
	// already run, or claimed for a run, once.
	ErrorCodeApprovalAlreadyRun = "approval_already_run"
	// ErrorCodeApprovalRunWindowPassed (HTTP 409): the approval resolved
	// approved longer ago than admin.run_window_seconds; the agent requests
	// a new one.
	ErrorCodeApprovalRunWindowPassed = "approval_run_window_passed"
	// ErrorCodeApprovalNotAdminAction (HTTP 409): the approval's payload is
	// not a conch.admin_action.v1 whose agent_id is the caller.
	ErrorCodeApprovalNotAdminAction = "approval_not_admin_action"
	// ErrorCodeTargetChanged (HTTP 409): the target's name is no longer the
	// one the decider was shown; the agent requests a new approval.
	ErrorCodeTargetChanged = "target_changed"
)

func init() {
	// Registered so that every client can decode the payload. Registration is
	// not authorship: the payload is server-composed only, the run step
	// proves an approval's origin by the admin door's own row written in the
	// same transaction as the approval and never by the payload's shape, and
	// the approval create routes refuse a client-supplied payload of this
	// schema (S3, #1330).
	RegisterPayload(PayloadSchema{
		Name: AdminActionV1Name,
		New:  func() any { return new(AdminActionV1) },
	})
}
