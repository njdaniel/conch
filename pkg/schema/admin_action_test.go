package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Every admin fixture decodes with no unknown field, re-encodes byte for byte,
// and validates: a published fixture is a value the schema itself accepts.
func TestAdminActionGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"admin-action-v1.json", func() any { return new(AdminActionV1) }},
		{"admin-action-v1-server-wide.json", func() any { return new(AdminActionV1) }},
		{"admin-action-v1-channel-member.json", func() any { return new(AdminActionV1) }},
		{"submit-admin-action-request-v1.json", func() any { return new(SubmitAdminActionRequestV1) }},
		{"submit-admin-action-request-v1-params.json", func() any { return new(SubmitAdminActionRequestV1) }},
		{"submit-admin-action-request-v1-server-wide.json", func() any { return new(SubmitAdminActionRequestV1) }},
		{"admin-action-result-v1.json", func() any { return new(AdminActionResultV1) }},
		{"admin-action-result-v1-no-body.json", func() any { return new(AdminActionResultV1) }},
		{"admin-action-result-v1-pending.json", func() any { return new(AdminActionResultV1) }},
		{"admin-catalog-response-v1.json", func() any { return new(AdminCatalogResponseV1) }},
		{"admin-catalog-response-v1-human.json", func() any { return new(AdminCatalogResponseV1) }},
		{"admin-catalog-response-v1-empty.json", func() any { return new(AdminCatalogResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			raw, err := os.ReadFile(filepath.Join("testdata", tt.file)) // #nosec G304 -- fixture name comes from this test's own table
			if err != nil {
				t.Fatal(err)
			}
			value := tt.new()
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(value); err != nil {
				t.Fatal(err)
			}
			if err := value.(validator).Validate(); err != nil {
				t.Errorf("fixture does not validate: %v", err)
			}
		})
	}
}

// The payload is registered under its name, and the approval fixture that
// carries it decodes through the registry into the same value as the payload
// fixture (design §3.5.1: one machine form, stored on the approval).
func TestAdminActionV1RegisteredPayload(t *testing.T) {
	if AdminActionV1Name != "conch.admin_action.v1" {
		t.Fatalf("AdminActionV1Name = %q", AdminActionV1Name)
	}
	if _, ok := LookupPayload(AdminActionV1Name); !ok {
		t.Fatalf("LookupPayload(%q) not found", AdminActionV1Name)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "conch-approval-v1-admin-action.json"))
	if err != nil {
		t.Fatal(err)
	}
	var approval ApprovalV1
	if err := json.Unmarshal(raw, &approval); err != nil {
		t.Fatal(err)
	}
	if approval.Payload == nil || approval.Payload.Schema != AdminActionV1Name {
		t.Fatalf("approval payload = %+v, want a %s", approval.Payload, AdminActionV1Name)
	}
	decoded, known, err := approval.Payload.Decode()
	if err != nil || !known {
		t.Fatalf("Decode() = %v, %v, %v", decoded, known, err)
	}
	got, ok := decoded.(*AdminActionV1)
	if !ok {
		t.Fatalf("Decode() returned %T, want *AdminActionV1", decoded)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("decoded payload does not validate: %v", err)
	}
	want := validAdminAction()
	if got.Action != want.Action || got.Route != want.Route || got.AgentID != want.AgentID || got.Reason != want.Reason ||
		!got.RequestedAt.Time().Equal(want.RequestedAt.Time()) || !reflect.DeepEqual(got.Target, want.Target) {
		t.Errorf("approval payload = %+v, want %+v", got, want)
	}
	// The options are the two fixed ones: yes or no on this exact action.
	if len(approval.Options) != 2 || approval.Options[0].ID != "run" || approval.Options[0].Kind != OptionKindApprove ||
		approval.Options[1].ID != "refuse" || approval.Options[1].Kind != OptionKindReject {
		t.Errorf("approval options = %+v, want run (approve) and refuse (reject)", approval.Options)
	}
	// An unknown field in the payload is refused, not dropped.
	p := Payload{Schema: AdminActionV1Name, Data: json.RawMessage(`{"action":"disable_member","route":"POST /v1/principals/7/disable","agent_id":12,"requested_at":"2026-11-05T10:14:00.000Z","run_at":"now"}`)}
	if _, _, err := p.Decode(); err == nil {
		t.Error("Decode() accepted a payload with an unknown field")
	}
}

func validAdminAction() AdminActionV1 {
	return AdminActionV1{
		Action:      AdminActionDisableMember,
		Route:       "POST /v1/principals/7/disable",
		Target:      &AdminTargetV1{Kind: AdminTargetPrincipal, ID: 7, Name: "alice"},
		Params:      json.RawMessage(`{}`),
		AgentID:     12,
		Reason:      "alice asked to leave the server in #general",
		RequestedAt: NewTimestamp(time.Date(2026, 11, 5, 10, 14, 0, 0, time.UTC)),
	}
}

func TestAdminActionV1Validate(t *testing.T) {
	long := strings.Repeat("x", MaxAdminReasonChars)
	tests := []struct {
		name    string
		mutate  func(*AdminActionV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*AdminActionV1) {}},
		{name: "valid server-wide: no target", mutate: func(a *AdminActionV1) {
			a.Target = nil
			a.Action = AdminActionPauseInvites
			a.Route = "PUT /v1/invites/settings"
		}},
		{name: "valid with no params", mutate: func(a *AdminActionV1) { a.Params = nil }},
		{name: "valid with no reason", mutate: func(a *AdminActionV1) { a.Reason = "" }},
		{name: "valid reason at the cap", mutate: func(a *AdminActionV1) { a.Reason = long }},
		{name: "valid reason at the cap in multi-byte characters", mutate: func(a *AdminActionV1) { a.Reason = strings.Repeat("é", MaxAdminReasonChars) }},
		{name: "valid target with no name", mutate: func(a *AdminActionV1) { a.Target.Name = "" }},
		{name: "valid params with nested values", mutate: func(a *AdminActionV1) { a.Params = json.RawMessage(`{"max_uses": 5, "channels": [1, 2]}`) }},

		{name: "unknown action", mutate: func(a *AdminActionV1) { a.Action = "delete_server" }, wantErr: `admin action "delete_server" is not in the catalog`},
		{name: "empty action", mutate: func(a *AdminActionV1) { a.Action = "" }, wantErr: `admin action "" is not in the catalog`},
		{name: "upper-case action", mutate: func(a *AdminActionV1) { a.Action = "DISABLE_MEMBER" }, wantErr: "is not in the catalog"},
		{name: "a route pattern, not a route", mutate: func(a *AdminActionV1) { a.Action = AdminActionListMembers; a.Route = "list" }, wantErr: `route "list" is not "METHOD /path"`},
		{name: "empty route", mutate: func(a *AdminActionV1) { a.Route = "" }, wantErr: `route "" is not "METHOD /path"`},
		{name: "route with a lower-case method", mutate: func(a *AdminActionV1) { a.Route = "post /v1/principals/7/disable" }, wantErr: `is not "METHOD /path"`},
		{name: "route with no leading slash", mutate: func(a *AdminActionV1) { a.Route = "POST v1/principals/7/disable" }, wantErr: `is not "METHOD /path"`},
		{name: "route with a space in the path", mutate: func(a *AdminActionV1) { a.Route = "POST /v1/principals/7 /disable" }, wantErr: `is not "METHOD /path"`},
		{name: "unknown target kind", mutate: func(a *AdminActionV1) { a.Target.Kind = "net" }, wantErr: "admin target kind must be one of: principal, channel, credential, hook, invite, report"},
		{name: "zero target id", mutate: func(a *AdminActionV1) { a.Target.ID = 0 }, wantErr: "admin target id must be positive, got 0"},
		{name: "negative target id", mutate: func(a *AdminActionV1) { a.Target.ID = -7 }, wantErr: "admin target id must be positive, got -7"},
		{name: "params not JSON", mutate: func(a *AdminActionV1) { a.Params = json.RawMessage(`{`) }, wantErr: "params is not valid JSON"},
		{name: "params an array", mutate: func(a *AdminActionV1) { a.Params = json.RawMessage(`[1]`) }, wantErr: "params must be a JSON object"},
		{name: "params a string", mutate: func(a *AdminActionV1) { a.Params = json.RawMessage(`"paused"`) }, wantErr: "params must be a JSON object"},
		{name: "params null", mutate: func(a *AdminActionV1) { a.Params = json.RawMessage(`null`) }, wantErr: "params must be a JSON object"},
		{name: "zero agent id", mutate: func(a *AdminActionV1) { a.AgentID = 0 }, wantErr: "agent_id must be positive, got 0"},
		{name: "negative agent id", mutate: func(a *AdminActionV1) { a.AgentID = -1 }, wantErr: "agent_id must be positive, got -1"},
		{name: "reason over the cap", mutate: func(a *AdminActionV1) { a.Reason = long + "x" }, wantErr: "reason is 501 characters, the most is 500"},
		{name: "reason not UTF-8", mutate: func(a *AdminActionV1) { a.Reason = "\xff" }, wantErr: "reason is not valid UTF-8"},
		// A reason is one quoted line of the approval's body; a line break in it
		// could forge a fact line (security review of #1328, F2).
		{name: "reason with a newline", mutate: func(a *AdminActionV1) { a.Reason = "ok\"\nAction:     enable_member" }, wantErr: "reason contains a control character (U+000A)"},
		{name: "reason with a carriage return", mutate: func(a *AdminActionV1) { a.Reason = "ok\rTarget: principal #1" }, wantErr: "reason contains a control character (U+000D)"},
		{name: "reason with a tab", mutate: func(a *AdminActionV1) { a.Reason = "ok\tfine" }, wantErr: "reason contains a control character (U+0009)"},
		{name: "reason with a C1 control", mutate: func(a *AdminActionV1) { a.Reason = "ok\u0085Action: x" }, wantErr: "reason contains a control character (U+0085)"},
		{name: "reason with DEL", mutate: func(a *AdminActionV1) { a.Reason = "ok\u007f" }, wantErr: "reason contains a control character (U+007F)"},
		{name: "valid reason with non-control Unicode", mutate: func(a *AdminActionV1) { a.Reason = "alice (Алиса) asked — see #general" }},
		{name: "zero requested_at", mutate: func(a *AdminActionV1) { a.RequestedAt = Timestamp{} }, wantErr: "requested_at is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validAdminAction()
			tt.mutate(&a)
			assertValidation(t, a.Validate(), tt.wantErr)
		})
	}
}

func TestSubmitAdminActionRequestV1Validate(t *testing.T) {
	valid := func() SubmitAdminActionRequestV1 {
		return SubmitAdminActionRequestV1{
			Action: AdminActionDisableMember,
			Target: &AdminTargetV1{Kind: AdminTargetPrincipal, ID: 7},
			Reason: "alice asked to leave",
		}
	}
	tests := []struct {
		name    string
		mutate  func(*SubmitAdminActionRequestV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*SubmitAdminActionRequestV1) {}},
		{name: "valid server-wide", mutate: func(r *SubmitAdminActionRequestV1) { r.Action, r.Target, r.Reason = AdminActionListMembers, nil, "" }},
		{name: "valid with params", mutate: func(r *SubmitAdminActionRequestV1) {
			r.Action, r.Target, r.Params = AdminActionAddChannelMember, &AdminTargetV1{Kind: AdminTargetChannel, ID: 9}, json.RawMessage(`{"principal_id": 7}`)
		}},
		{name: "valid with a target name, which the server ignores", mutate: func(r *SubmitAdminActionRequestV1) { r.Target.Name = "alice" }},
		{name: "valid for every catalog action", mutate: func(r *SubmitAdminActionRequestV1) { r.Action = AdminActionExportAudit }},

		{name: "unknown action", mutate: func(r *SubmitAdminActionRequestV1) { r.Action = "promote_to_owner" }, wantErr: `admin action "promote_to_owner" is not in the catalog`},
		{name: "empty action", mutate: func(r *SubmitAdminActionRequestV1) { r.Action = "" }, wantErr: `admin action "" is not in the catalog`},
		{name: "unknown target kind", mutate: func(r *SubmitAdminActionRequestV1) { r.Target.Kind = "server" }, wantErr: "admin target kind must be one of"},
		{name: "zero target id", mutate: func(r *SubmitAdminActionRequestV1) { r.Target.ID = 0 }, wantErr: "admin target id must be positive"},
		{name: "params not an object", mutate: func(r *SubmitAdminActionRequestV1) { r.Params = json.RawMessage(`true`) }, wantErr: "params must be a JSON object"},
		{name: "params not JSON", mutate: func(r *SubmitAdminActionRequestV1) { r.Params = json.RawMessage(`{"a":`) }, wantErr: "params is not valid JSON"},
		{name: "reason over the cap", mutate: func(r *SubmitAdminActionRequestV1) { r.Reason = strings.Repeat("r", MaxAdminReasonChars+1) }, wantErr: "reason is 501 characters, the most is 500"},
		{name: "reason with a newline", mutate: func(r *SubmitAdminActionRequestV1) { r.Reason = "ok\"\nAction: enable_member" }, wantErr: "reason contains a control character (U+000A)"},
		{name: "reason with a carriage return", mutate: func(r *SubmitAdminActionRequestV1) { r.Reason = "ok\r" }, wantErr: "reason contains a control character (U+000D)"},
		{name: "reason with a C1 control", mutate: func(r *SubmitAdminActionRequestV1) { r.Reason = "ok\u009b" }, wantErr: "reason contains a control character (U+009B)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
	for _, action := range AdminActions {
		r := SubmitAdminActionRequestV1{Action: action}
		if err := r.Validate(); err != nil {
			t.Errorf("a bare request for %s: Validate() = %v", action, err)
		}
	}
}

func TestAdminActionResultV1Validate(t *testing.T) {
	deadline := NewTimestamp(time.Date(2026, 11, 6, 10, 14, 0, 0, time.UTC))
	ran := func() AdminActionResultV1 {
		return AdminActionResultV1{Status: AdminActionRan, HTTPStatus: 200, Result: json.RawMessage(`{"ok":true}`)}
	}
	pending := func() AdminActionResultV1 {
		approval := validApproval()
		approval.ID = 58
		approval.Deadline = deadline
		approval.Payload = &Payload{Schema: AdminActionV1Name, Data: json.RawMessage(`{"action":"disable_member"}`)}
		return AdminActionResultV1{Status: AdminActionPendingApproval, ApprovalID: 58, Approval: &approval, Deadline: &deadline}
	}
	tests := []struct {
		name    string
		result  func() AdminActionResultV1
		wantErr string
	}{
		{name: "valid ran", result: ran},
		{name: "valid ran with a null body", result: func() AdminActionResultV1 {
			r := ran()
			r.HTTPStatus, r.Result = 204, json.RawMessage(`null`)
			return r
		}},
		{name: "valid ran with the route's refusal", result: func() AdminActionResultV1 {
			r := ran()
			r.HTTPStatus, r.Result = 403, json.RawMessage(`{"code":"forbidden","message":"only an owner may act on an owner"}`)
			return r
		}},
		{name: "valid pending", result: pending},

		{name: "empty status", result: func() AdminActionResultV1 { r := ran(); r.Status = ""; return r }, wantErr: "admin action status must be one of: ran, pending_approval"},
		{name: "unknown status", result: func() AdminActionResultV1 { r := ran(); r.Status = "queued"; return r }, wantErr: "admin action status must be one of: ran, pending_approval"},
		{name: "ran with no http status", result: func() AdminActionResultV1 { r := ran(); r.HTTPStatus = 0; return r }, wantErr: "a ran admin action result carries an HTTP status, got 0"},
		{name: "ran with an impossible http status", result: func() AdminActionResultV1 { r := ran(); r.HTTPStatus = 600; return r }, wantErr: "carries an HTTP status, got 600"},
		{name: "ran with no result", result: func() AdminActionResultV1 { r := ran(); r.Result = nil; return r }, wantErr: "carries the route's body as JSON, or null"},
		{name: "ran with a result that is not JSON", result: func() AdminActionResultV1 { r := ran(); r.Result = json.RawMessage(`{`); return r }, wantErr: "carries the route's body as JSON, or null"},
		{name: "ran with an approval id", result: func() AdminActionResultV1 { r := ran(); r.ApprovalID = 58; return r }, wantErr: "a ran admin action result carries no approval"},
		{name: "ran with a deadline", result: func() AdminActionResultV1 { r := ran(); r.Deadline = &deadline; return r }, wantErr: "a ran admin action result carries no approval"},
		{name: "pending with no approval id", result: func() AdminActionResultV1 { r := pending(); r.ApprovalID = 0; return r }, wantErr: "carries approval_id, approval and deadline"},
		{name: "pending with no approval", result: func() AdminActionResultV1 { r := pending(); r.Approval = nil; return r }, wantErr: "carries approval_id, approval and deadline"},
		{name: "pending with no deadline", result: func() AdminActionResultV1 { r := pending(); r.Deadline = nil; return r }, wantErr: "carries approval_id, approval and deadline"},
		{name: "pending with a zero deadline", result: func() AdminActionResultV1 { r := pending(); r.Deadline = &Timestamp{}; return r }, wantErr: "deadline is required"},
		{name: "pending with an approval of another id", result: func() AdminActionResultV1 { r := pending(); r.ApprovalID = 59; return r }, wantErr: "approval_id 59 names a different approval than its approval (58)"},
		{name: "pending with an invalid approval", result: func() AdminActionResultV1 { r := pending(); r.Approval.Title = ""; return r }, wantErr: "approval title is required"},
		{name: "pending with an http status", result: func() AdminActionResultV1 { r := pending(); r.HTTPStatus = 202; return r }, wantErr: "carries no http_status or result"},
		{name: "pending with a result", result: func() AdminActionResultV1 { r := pending(); r.Result = json.RawMessage(`{}`); return r }, wantErr: "carries no http_status or result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.result().Validate(), tt.wantErr)
		})
	}
}

// The result's variants are exactly its fields: every field but the tag is in
// one of them, so the generated union has no field the Go type lacks and no
// Go field it drops.
func TestAdminActionResultV1Union(t *testing.T) {
	var union *TSUnion
	for i := range TSUnions {
		if _, ok := TSUnions[i].Value.(AdminActionResultV1); ok {
			union = &TSUnions[i]
		}
	}
	if union == nil {
		t.Fatal("AdminActionResultV1 is not in TSUnions")
	}
	if union.Tag != "status" {
		t.Errorf("tag = %q, want status", union.Tag)
	}
	want := map[string][]string{
		string(AdminActionRan):             {"http_status", "result"},
		string(AdminActionPendingApproval): {"approval_id", "approval", "deadline"},
	}
	if !reflect.DeepEqual(union.Variants, want) {
		t.Errorf("variants = %v, want %v", union.Variants, want)
	}
}

func validAdminCatalogEntry() AdminCatalogEntryV1 {
	return AdminCatalogEntryV1{
		Action:     AdminActionDisableMember,
		Route:      "POST /v1/principals/{id}/disable",
		Permission: PermManageMembers,
		Gate:       AdminGateApproval,
		Held:       true,
		Gated:      true,
	}
}

func TestAdminCatalogEntryV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AdminCatalogEntryV1)
		wantErr string
	}{
		{name: "valid gated", mutate: func(*AdminCatalogEntryV1) {}},
		{name: "valid direct", mutate: func(e *AdminCatalogEntryV1) {
			e.Action, e.Gate, e.Gated = AdminActionListMembers, AdminGateDirect, false
		}},
		{name: "valid direct, gated for this caller by its manifest", mutate: func(e *AdminCatalogEntryV1) {
			e.Action, e.Gate, e.Gated = AdminActionRenameMember, AdminGateDirect, true
		}},
		{name: "valid not held", mutate: func(e *AdminCatalogEntryV1) { e.Held = false }},
		{name: "unknown action", mutate: func(e *AdminCatalogEntryV1) { e.Action = "reboot" }, wantErr: `admin catalog action "reboot" is not in the catalog`},
		{name: "a bad route", mutate: func(e *AdminCatalogEntryV1) { e.Route = "/v1/principals" }, wantErr: `is not "METHOD /path"`},
		{name: "unknown permission", mutate: func(e *AdminCatalogEntryV1) { e.Permission = "administer" }, wantErr: `permission "administer" is not in the catalog`},
		{name: "empty permission", mutate: func(e *AdminCatalogEntryV1) { e.Permission = "" }, wantErr: `permission "" is not in the catalog`},
		{name: "unknown gate", mutate: func(e *AdminCatalogEntryV1) { e.Gate = "never" }, wantErr: "gate must be one of: direct, approval"},
		{name: "an approval-gated action shown as not gated", mutate: func(e *AdminCatalogEntryV1) { e.Gated = false }, wantErr: "an action gated by default is gated for every caller"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validAdminCatalogEntry()
			tt.mutate(&e)
			assertValidation(t, e.Validate(), tt.wantErr)
		})
	}
}

func TestAdminCatalogResponseV1Validate(t *testing.T) {
	admin := func() *AgentAdminV1 { return &AgentAdminV1{ApprovalChannelID: 11} }
	tests := []struct {
		name    string
		resp    AdminCatalogResponseV1
		wantErr string
	}{
		{name: "valid empty", resp: AdminCatalogResponseV1{}},
		{name: "valid human caller", resp: AdminCatalogResponseV1{Actions: []AdminCatalogEntryV1{validAdminCatalogEntry()}}},
		{name: "valid agent caller", resp: AdminCatalogResponseV1{Actions: []AdminCatalogEntryV1{validAdminCatalogEntry()}, Admin: admin()}},
		{name: "an invalid entry", resp: AdminCatalogResponseV1{Actions: []AdminCatalogEntryV1{{}}}, wantErr: `admin catalog actions[0]: schema: admin catalog action "" is not in the catalog`},
		{name: "an action twice", resp: AdminCatalogResponseV1{Actions: []AdminCatalogEntryV1{validAdminCatalogEntry(), validAdminCatalogEntry()}}, wantErr: "admin catalog lists disable_member more than once"},
		{name: "invalid admin settings", resp: AdminCatalogResponseV1{Admin: &AgentAdminV1{}}, wantErr: "approval_channel_id must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.resp.Validate(), tt.wantErr)
		})
	}
	// Actions is never null.
	raw, err := json.Marshal(AdminCatalogResponseV1{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"actions":[]}` {
		t.Errorf("empty response encodes as %s, want {\"actions\":[]}", raw)
	}
}

func TestAdminTargetV1Validate(t *testing.T) {
	for _, kind := range []AdminTargetKind{AdminTargetPrincipal, AdminTargetChannel, AdminTargetCredential, AdminTargetHook, AdminTargetInvite, AdminTargetReport} {
		if err := (AdminTargetV1{Kind: kind, ID: 1}).Validate(); err != nil {
			t.Errorf("target of kind %s: Validate() = %v", kind, err)
		}
	}
	tests := []struct {
		name    string
		target  AdminTargetV1
		wantErr string
	}{
		{name: "zero value", target: AdminTargetV1{}, wantErr: "admin target kind must be one of: principal, channel, credential, hook, invite, report"},
		{name: "unknown kind", target: AdminTargetV1{Kind: "net", ID: 1}, wantErr: "admin target kind must be one of"},
		{name: "upper-case kind", target: AdminTargetV1{Kind: "Principal", ID: 1}, wantErr: "admin target kind must be one of"},
		{name: "zero id", target: AdminTargetV1{Kind: AdminTargetChannel}, wantErr: "admin target id must be positive, got 0"},
		{name: "negative id", target: AdminTargetV1{Kind: AdminTargetChannel, ID: -2}, wantErr: "admin target id must be positive, got -2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.target.Validate(), tt.wantErr)
		})
	}
}

// The catalog has forty actions, in the design's order, each valid, none
// twice, and nothing else is valid; the vocabulary table lists the same.
func TestAdminActionsCatalog(t *testing.T) {
	if len(AdminActions) != 40 {
		t.Errorf("AdminActions has %d actions, want 40", len(AdminActions))
	}
	seen := map[AdminAction]bool{}
	for _, a := range AdminActions {
		if !a.Valid() {
			t.Errorf("AdminActions lists %q, which is not Valid", a)
		}
		if seen[a] {
			t.Errorf("AdminActions lists %q more than once", a)
		}
		seen[a] = true
	}
	if first, last := AdminActions[0], AdminActions[len(AdminActions)-1]; first != AdminActionListMembers || last != AdminActionExportAudit {
		t.Errorf("AdminActions runs from %s to %s, want list_members to export_audit", first, last)
	}
	if got := values(AdminActions...); !reflect.DeepEqual(got, Vocabularies["AdminAction"]) {
		t.Errorf("AdminActions = %v,\nVocabularies[AdminAction] = %v", got, Vocabularies["AdminAction"])
	}
	// What the design keeps out of the catalog (§3.3) is not a name.
	for _, a := range []AdminAction{
		"", "*", "set_role", "put_manifest", "get_manifest", "create_credential", "rotate_credential",
		"create_hook", "create_sign_in_link", "hide_members", "end_guest", "cast_decision", "create_agent",
		"DISABLE_MEMBER", "disable-member", "disable_member ",
	} {
		if a.Valid() {
			t.Errorf("AdminAction %q should be invalid", a)
		}
	}
	for _, g := range []AdminGate{"", "none", "DIRECT", "gated"} {
		if g.Valid() {
			t.Errorf("AdminGate %q should be invalid", g)
		}
	}
	for _, s := range []AdminActionStatus{"", "pending", "approved", "RAN", "refused"} {
		if s.Valid() {
			t.Errorf("AdminActionStatus %q should be invalid", s)
		}
	}
}

func TestAdminActionErrorCodes(t *testing.T) {
	assertErrorCodes(t, map[string]string{
		ErrorCodeAdminActionUnknown:         "admin_action_unknown",
		ErrorCodeAdminActionNotAllowed:      "admin_action_not_allowed",
		ErrorCodeAdminActionPending:         "admin_action_pending",
		ErrorCodeTooManyPendingAdminActions: "too_many_pending_admin_actions",
		ErrorCodeApprovalNotApproved:        "approval_not_approved",
		ErrorCodeApprovalAlreadyRun:         "approval_already_run",
		ErrorCodeApprovalRunWindowPassed:    "approval_run_window_passed",
		ErrorCodeApprovalNotAdminAction:     "approval_not_admin_action",
		ErrorCodeTargetChanged:              "target_changed",
	})
}

// The admin vocabularies are classified for the web client as the design
// says: the catalog and the target kinds are client-tolerant (a later row is
// shown by its name), the status is a union's tag and the gate is switched
// on, so both are strict.
func TestAdminVocabularyModes(t *testing.T) {
	want := map[string]TSVocabularyMode{
		"AdminAction":       TSTolerant,
		"AdminTargetKind":   TSTolerant,
		"AdminActionStatus": TSStrict,
		"AdminGate":         TSStrict,
	}
	for name, mode := range want {
		if got, ok := TSVocabularyModes[name]; !ok || got != mode {
			t.Errorf("TSVocabularyModes[%q] = %v, %v; want %v", name, got, ok, mode)
		}
	}
}
