package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// TestAdminAuditActor checks the five administrative audit actions record the
// authenticated caller under AuthRequired and "system" under AuthOff.
func TestAdminAuditActor(t *testing.T) {
	tests := []struct {
		name      string
		mode      AuthMode
		wantActor string
	}{
		{"required records the operator", AuthRequired, "principal:1"},
		{"off records system", AuthOff, "system"},
	}
	const manifest = `{"display_name":"Bot","tier":"A","capabilities":[],"channels":[],"rate_limits":[]}`
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, tt.mode)
			if f.root.ID != 1 {
				t.Fatalf("operator id = %d, want 1", f.root.ID)
			}
			token := ""
			if tt.mode == AuthRequired {
				token = f.rootTok
			}
			// The fixture agent already has a manifest (issue #79); use a fresh
			// agent so both manifest_created and manifest_replaced are written.
			fresh, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalAgent, "fresh-agent")
			if err != nil {
				t.Fatal(err)
			}
			baseline := len(f.audit(t))

			created := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials", f.bot.ID), token, `{"label":"ci"}`)
			if created.Code != http.StatusCreated {
				t.Fatalf("create credential = %d %s", created.Code, created.Body)
			}
			credID := decodeBody[schema.CreateCredentialResponseV1](t, created).Credential.ID
			rotated := f.do(t, "POST", fmt.Sprintf("/v1/credentials/%d/rotate", credID), token, "")
			if rotated.Code != http.StatusCreated {
				t.Fatalf("rotate = %d %s", rotated.Code, rotated.Body)
			}
			newID := decodeBody[schema.RotateCredentialResponseV1](t, rotated).Credential.ID
			if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", newID), token, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
			}
			manifestPath := fmt.Sprintf("/v1/principals/%d/manifest", fresh.ID)
			for i := 0; i < 2; i++ { // create, then replace
				if rec := f.do(t, "PUT", manifestPath, token, manifest); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
					t.Fatalf("put manifest = %d %s", rec.Code, rec.Body)
				}
			}

			got := map[string]string{}
			for _, e := range f.audit(t)[baseline:] {
				got[e.Action] = e.Actor
			}
			for _, action := range []string{"credential_created", "credential_rotated", "credential_revoked", "manifest_created", "manifest_replaced"} {
				actor, ok := got[action]
				if !ok {
					t.Errorf("no %s event written", action)
				} else if actor != tt.wantActor {
					t.Errorf("%s actor = %q, want %q", action, actor, tt.wantActor)
				}
			}
		})
	}
}
