package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func TestDMPreferencesRoutes(t *testing.T) {
	f := newDMFixture(t)
	alice, bob := f.join(t), f.join(t)
	const path = "/v1/dms/preferences"

	for _, tt := range []struct {
		name, method, query, token, body string
		want                             schema.DMAcceptPolicy
	}{
		{name: "default", method: http.MethodGet, token: alice.tok, want: schema.DMAcceptEveryone},
		{name: "share a channel", method: http.MethodPut, token: alice.tok, body: `{"accept":"shared_channel"}`, want: schema.DMAcceptSharedChannel},
		{name: "persisted", method: http.MethodGet, token: alice.tok, want: schema.DMAcceptSharedChannel},
		{name: "other member", method: http.MethodGet, token: bob.tok, want: schema.DMAcceptEveryone},
		{name: "operator reads own preference", method: http.MethodGet, token: f.rootTok, query: fmt.Sprintf("?principal_id=%d", alice.p.ID), want: schema.DMAcceptEveryone},
		{name: "nobody", method: http.MethodPut, token: alice.tok, body: `{"accept":"nobody"}`, want: schema.DMAcceptNobody},
		{name: "everyone", method: http.MethodPut, token: alice.tok, body: `{"accept":"everyone"}`, want: schema.DMAcceptEveryone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, tt.method, path+tt.query, tt.token, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d %s", tt.method, path, rec.Code, rec.Body)
			}
			var got schema.DMPreferencesV1
			dmDecode(t, rec, &got)
			if got.Accept != tt.want {
				t.Errorf("accept = %q, want %q", got.Accept, tt.want)
			}
		})
	}
	for _, body := range []string{`{}`, `null`, `{"accept":null}`, `{"accept":"friends"}`, `{"accept":"nobody"}{}`,
		fmt.Sprintf(`{"accept":"nobody","principal_id":%d}`, bob.p.ID)} {
		t.Run(body, func(t *testing.T) {
			f.wantError(t, f.do(t, http.MethodPut, path, alice.tok, body), http.StatusBadRequest, "invalid_request")
		})
	}
}

func TestDMServerSettingsRoutes(t *testing.T) {
	f := newDMFixture(t)
	operator := f.join(t)
	const path = "/v1/dms/settings"
	if _, err := f.srv.store.SetRole(t.Context(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, operator.p.ID, store.RoleOperator, nil); err != nil {
		t.Fatal(err)
	}
	_, actAs, err := f.srv.store.CreateCredential(t.Context(), fmt.Sprintf("principal:%d", f.root.ID), operator.p.ID, "act as operator", nil)
	if err != nil {
		t.Fatal(err)
	}
	member := f.join(t)
	before := countAction(f.audit(t), store.AuditDMSettingsChanged)
	for _, tt := range []struct {
		name, method, token, body, code string
		status                          int
		wantOn                          bool
	}{
		{"default", http.MethodGet, f.rootTok, "", "", http.StatusOK, true},
		{"operator can read", http.MethodGet, operator.tok, "", "", http.StatusOK, true},
		{"operator lacks grant", http.MethodPut, operator.tok, `{"dms_enabled":false}`, schema.ErrorCodePermissionDenied, http.StatusForbidden, false},
		{"member cannot read", http.MethodGet, member.tok, "", "forbidden", http.StatusForbidden, false},
		{"member cannot write", http.MethodPut, member.tok, `{"dms_enabled":false}`, "forbidden", http.StatusForbidden, false},
		{"turn off", http.MethodPut, f.rootTok, `{"dms_enabled":false}`, "", http.StatusOK, false},
		{"read off", http.MethodGet, f.rootTok, "", "", http.StatusOK, false},
		{"repeat off", http.MethodPut, f.rootTok, `{"dms_enabled":false}`, "", http.StatusOK, false},
		{"turn on", http.MethodPut, f.rootTok, `{"dms_enabled":true}`, "", http.StatusOK, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, tt.method, path, tt.token, tt.body)
			if tt.code != "" {
				f.wantError(t, rec, tt.status, tt.code)
				return
			}
			if rec.Code != tt.status {
				t.Fatalf("%s %s = %d %s", tt.method, path, rec.Code, rec.Body)
			}
			var got schema.DMServerSettingsV1
			dmDecode(t, rec, &got)
			if got.DMsEnabled != tt.wantOn {
				t.Errorf("dms_enabled = %t, want %t", got.DMsEnabled, tt.wantOn)
			}
		})
	}
	for _, body := range []string{`{}`, `null`, `{"dms_enabled":null}`, `{"dms_enabled":"false"}`,
		`{"dms_enabled":false,"accept":"nobody"}`, `{"dms_enabled":false}{}`} {
		t.Run(body, func(t *testing.T) {
			f.wantError(t, f.do(t, http.MethodPut, path, f.rootTok, body), http.StatusBadRequest, "invalid_request")
		})
	}
	t.Run("act-as with the server settings grant", func(t *testing.T) {
		doc, err := f.srv.store.Permissions(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		grant := schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: []schema.PermissionChangeV1{
			{Permission: schema.PermChangeServerSettings, Role: schema.PermissionRoleOperator, Granted: true},
		}}
		if rec := f.do(t, http.MethodPut, "/v1/permissions", f.rootTok, inviteJSON(t, grant)); rec.Code != http.StatusOK {
			t.Fatalf("grant change_server_settings = %d %s", rec.Code, rec.Body)
		}
		const off = `{"dms_enabled":false}`
		f.wantError(t, f.do(t, http.MethodPut, path, actAs, off), http.StatusForbidden, schema.ErrorCodeOwnSignInRequired)
		f.wantError(t, f.do(t, http.MethodGet, path, actAs, ""), http.StatusForbidden, schema.ErrorCodeOwnSignInRequired)
		f.wantError(t, f.do(t, http.MethodPut, path, operator.tok, off), http.StatusForbidden, schema.ErrorCodeReauthRequired)
	})
	if got := countAction(f.audit(t), store.AuditDMSettingsChanged) - before; got != 2 {
		t.Errorf("switch changes wrote %d audit events, want 2", got)
	}
}

func TestDMServerSettingsRequiresRecentAuth(t *testing.T) {
	f := newOperatorFixture(t)
	secret := f.enrolOperator(t, f.root.ID)
	_, token := f.operatorSession(t, secret)
	f.advance(schema.RecentAuthLifetimeSeconds * time.Second)
	const body = `{"dms_enabled":false}`
	const path = "/v1/dms/settings"
	assertErrorBody(t, f.do(t, http.MethodPut, path, token, body), http.StatusForbidden, schema.ErrorCodeReauthRequired)
	if rec := f.reauthOp(t, token, rootPassword, f.code(secret)); rec.Code != http.StatusOK {
		t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodPut, path, token, body); rec.Code != http.StatusOK {
		t.Fatalf("settings after reauth = %d %s", rec.Code, rec.Body)
	}
}

func TestDMControlsEnforceSendAndEdit(t *testing.T) {
	for _, tt := range []struct {
		name, path, disable, enable, code string
	}{
		{"server switch", "/v1/dms/settings", `{"dms_enabled":false}`, `{"dms_enabled":true}`, schema.ErrorCodeDMsOff},
		{"recipient preference", "/v1/dms/preferences", `{"accept":"nobody"}`, `{"accept":"everyone"}`, schema.ErrorCodeContactUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newDMFixture(t)
			author, recipient := f.join(t), f.join(t)
			token := recipient.tok
			if tt.path == "/v1/dms/settings" {
				token = f.rootTok
			}
			conv := f.mustOpen(t, author, recipient.p.ID)
			base := fmt.Sprintf("/v1/dms/%d/messages", conv.ID)
			sent := f.do(t, http.MethodPost, base, author.tok, `{"body":"original","client_nonce":"original"}`)
			if sent.Code != http.StatusCreated {
				t.Fatalf("initial send = %d %s", sent.Code, sent.Body)
			}
			msg := decodeBody[schema.PostDMMessageResponseV1](t, sent).Message
			item := fmt.Sprintf("%s/%d", base, msg.ID)
			set := func(body string) {
				t.Helper()
				if rec := f.do(t, http.MethodPut, tt.path, token, body); rec.Code != http.StatusOK {
					t.Fatalf("set control = %d %s", rec.Code, rec.Body)
				}
			}
			const next = `{"body":"next","client_nonce":"next"}`
			const edit = `{"body":"edited"}`
			set(tt.disable)
			f.wantError(t, f.do(t, http.MethodPost, base, author.tok, next), http.StatusForbidden, tt.code)
			f.wantError(t, f.do(t, http.MethodPatch, item, author.tok, edit), http.StatusForbidden, tt.code)
			if rec := f.do(t, http.MethodGet, base, recipient.tok, ""); rec.Code != http.StatusOK {
				t.Fatalf("retained history = %d %s", rec.Code, rec.Body)
			}
			set(tt.enable)
			if rec := f.do(t, http.MethodPost, base, author.tok, next); rec.Code != http.StatusCreated {
				t.Fatalf("restored send = %d %s", rec.Code, rec.Body)
			}
			if rec := f.do(t, http.MethodPatch, item, author.tok, edit); rec.Code != http.StatusOK {
				t.Fatalf("restored edit = %d %s", rec.Code, rec.Body)
			}
		})
	}
}
