package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func credentialsPath(principalID int64) string {
	return fmt.Sprintf("/v1/principals/%d/credentials", principalID)
}

func credentialPath(id int64) string { return fmt.Sprintf("/v1/credentials/%d", id) }

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func assertErrorBody(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body)
	}
	if got := decodeBody[schema.Error](t, rec).Code; got != code {
		t.Errorf("error code = %q, want %q", got, code)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestCredentialEndpointsTable(t *testing.T) {
	f := newManifestFixture(t)
	const missing = 9999

	// Seed: a live credential, and a revoked one.
	live := f.createCredential(t, f.agent.ID, `{"label":"live"}`)
	revoked := f.createCredential(t, f.human.ID, `{"label":"dead"}`)
	if rec := f.do(t, http.MethodDelete, credentialPath(revoked.Credential.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("seed revoke = %d", rec.Code)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name     string
		method   string
		path     string
		body     string
		status   int
		wantCode string
	}{
		{"create agent", "POST", credentialsPath(f.agent.ID), `{"label":"a"}`, 201, ""},
		{"create human", "POST", credentialsPath(f.human.ID), `{"label":"h"}`, 201, ""},
		{"create with future expiry", "POST", credentialsPath(f.agent.ID), `{"label":"a","expires_at":"` + future + `"}`, 201, ""},
		{"create blank label", "POST", credentialsPath(f.agent.ID), `{"label":"  "}`, 400, "invalid_request"},
		{"create label with newline", "POST", credentialsPath(f.agent.ID), `{"label":"ci\nbuilds"}`, 400, "invalid_request"},
		{"create label with bidi override", "POST", credentialsPath(f.agent.ID), `{"label":"ci\u202ebuilds"}`, 400, "invalid_request"},
		{"create label with leading space", "POST", credentialsPath(f.agent.ID), `{"label":" ci"}`, 400, "invalid_request"},
		{"create unicode label", "POST", credentialsPath(f.agent.ID), `{"label":"夜間ビルド"}`, 201, ""},
		{"create missing label", "POST", credentialsPath(f.agent.ID), `{}`, 400, "invalid_request"},
		{"create long label", "POST", credentialsPath(f.agent.ID), `{"label":"` + strings.Repeat("x", 101) + `"}`, 400, "invalid_request"},
		{"create past expiry", "POST", credentialsPath(f.agent.ID), `{"label":"a","expires_at":"` + past + `"}`, 400, "invalid_request"},
		{"create bad json", "POST", credentialsPath(f.agent.ID), `{`, 400, "invalid_request"},
		{"create empty body", "POST", credentialsPath(f.agent.ID), ``, 400, "invalid_request"},
		{"create unknown field", "POST", credentialsPath(f.agent.ID), `{"label":"a","token":"x"}`, 400, "invalid_request"},
		{"create bad principal id", "POST", "/v1/principals/abc/credentials", `{"label":"a"}`, 400, "invalid_request"},
		{"create unknown principal", "POST", credentialsPath(missing), `{"label":"a"}`, 404, "principal_not_found"},
		{"list", "GET", credentialsPath(f.agent.ID), "", 200, ""},
		{"list unknown principal", "GET", credentialsPath(missing), "", 404, "principal_not_found"},
		{"list bad id", "GET", "/v1/principals/0/credentials", "", 400, "invalid_request"},
		{"rotate live", "POST", credentialPath(live.Credential.ID) + "/rotate", "", 201, ""},
		{"rotate revoked", "POST", credentialPath(revoked.Credential.ID) + "/rotate", "", 409, "credential_revoked"},
		{"rotate already-rotated", "POST", credentialPath(live.Credential.ID) + "/rotate", "", 409, "credential_revoked"},
		{"rotate unknown", "POST", credentialPath(missing) + "/rotate", "", 404, "credential_not_found"},
		{"rotate bad id", "POST", "/v1/credentials/x/rotate", "", 400, "invalid_request"},
		{"revoke", "DELETE", credentialPath(f.mustFirstCredential(t, f.human.ID)), "", 204, ""},
		{"revoke twice", "DELETE", credentialPath(revoked.Credential.ID), "", 204, ""},
		{"revoke unknown", "DELETE", credentialPath(missing), "", 404, "credential_not_found"},
		{"revoke bad id", "DELETE", "/v1/credentials/-1", "", 400, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, tt.method, tt.path, tt.body)
			if tt.wantCode != "" {
				assertErrorBody(t, rec, tt.status, tt.wantCode)
				return
			}
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
			switch tt.status {
			case 201:
				resp := decodeBody[schema.CreateCredentialResponseV1](t, rec)
				if !strings.HasPrefix(resp.Token, "conch_") || len(resp.Token) != schema.CredentialTokenLength {
					t.Errorf("token shape wrong (len %d)", len(resp.Token))
				}
				if resp.Credential.ID <= 0 {
					t.Errorf("credential = %+v", resp.Credential)
				}
				if got := rec.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("Cache-Control = %q, want no-store", got)
				}
			case 204:
				if rec.Body.Len() != 0 {
					t.Errorf("204 with body %q", rec.Body)
				}
			}
		})
	}
}

// TestCredentialLabelRuleMessage: the label refusal names the rule and does
// not echo the label (the table above pins the status and code).
func TestCredentialLabelRuleMessage(t *testing.T) {
	f := newManifestFixture(t)
	tests := []struct {
		name    string
		label   string
		message string
	}{
		{"newline", "ci\nbuilds", "control characters"},
		{"bidi override", "ci\u202ebuilds", "bidi controls"},
		{"leading space", " ci", "whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, credentialsPath(f.agent.ID),
				fmt.Sprintf(`{"label":%q}`, tt.label))
			assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
			if !strings.Contains(rec.Body.String(), tt.message) {
				t.Errorf("body = %s, want a message naming the rule (%q)", rec.Body.String(), tt.message)
			}
			if strings.Contains(rec.Body.String(), tt.label) {
				t.Errorf("body echoes the refused label: %s", rec.Body.String())
			}
		})
	}
}

func (f manifestFixture) createCredential(t *testing.T, principalID int64, body string) schema.CreateCredentialResponseV1 {
	t.Helper()
	rec := f.do(t, http.MethodPost, credentialsPath(principalID), body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create credential = %d: %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.CreateCredentialResponseV1](t, rec)
}

// mustFirstCredential creates a credential for the principal and returns its id.
func (f manifestFixture) mustFirstCredential(t *testing.T, principalID int64) int64 {
	t.Helper()
	return f.createCredential(t, principalID, `{"label":"to-revoke"}`).Credential.ID
}

func TestCredentialLifecycleOverHTTP(t *testing.T) {
	f := newManifestFixture(t)
	ctx := context.Background()

	created := f.createCredential(t, f.agent.ID, `{"label":"leviathan"}`)
	if p, err := f.srv.store.ResolveCredential(ctx, created.Token); err != nil || p.ID != f.agent.ID {
		t.Fatalf("resolve created: %+v, %v", p, err)
	}

	rec := f.do(t, http.MethodPost, credentialPath(created.Credential.ID)+"/rotate", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("rotate = %d: %s", rec.Code, rec.Body)
	}
	rotated := decodeBody[schema.RotateCredentialResponseV1](t, rec)
	if rotated.Credential.Label != "leviathan" || rotated.Credential.PrincipalID != f.agent.ID {
		t.Errorf("rotated = %+v", rotated.Credential)
	}
	if _, err := f.srv.store.ResolveCredential(ctx, created.Token); err == nil {
		t.Error("old token still resolves after rotation")
	}
	if _, err := f.srv.store.ResolveCredential(ctx, rotated.Token); err != nil {
		t.Errorf("new token: %v", err)
	}

	rec = f.do(t, http.MethodGet, credentialsPath(f.agent.ID), "")
	list := decodeBody[schema.ListCredentialsResponseV1](t, rec)
	if len(list.Credentials) != 2 || list.Credentials[0].ID != rotated.Credential.ID || list.Credentials[1].RevokedAt == nil {
		t.Errorf("list = %+v", list.Credentials)
	}

	if rec := f.do(t, http.MethodDelete, credentialPath(rotated.Credential.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if _, err := f.srv.store.ResolveCredential(ctx, rotated.Token); err == nil {
		t.Error("revoked token still resolves")
	}
	// Revoking never touches the principal.
	if _, err := f.srv.store.PrincipalByID(ctx, f.agent.ID); err != nil {
		t.Errorf("principal: %v", err)
	}
}

func TestListCredentialsEmptyIsArray(t *testing.T) {
	f := newManifestFixture(t)
	rec := f.do(t, http.MethodGet, credentialsPath(f.agent.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"credentials":[]}` {
		t.Errorf("body = %s, want {\"credentials\":[]}", got)
	}
}

func TestListCredentialsNeverIncludesSecret(t *testing.T) {
	f := newManifestFixture(t)
	a := f.createCredential(t, f.agent.ID, `{"label":"a"}`)
	b := f.createCredential(t, f.agent.ID, `{"label":"b"}`)
	rec := f.do(t, http.MethodGet, credentialsPath(f.agent.ID), "")
	body := rec.Body.String()
	for _, tok := range []string{a.Token, b.Token} {
		if strings.Contains(body, tok) || strings.Contains(body, sha256Hex(tok)) || strings.Contains(body, strings.TrimPrefix(tok, "conch_")) {
			t.Errorf("list body leaks token material: %s", body)
		}
	}
	var generic struct {
		Credentials []map[string]json.RawMessage `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
		t.Fatal(err)
	}
	for _, c := range generic.Credentials {
		for k := range c {
			switch k {
			case "id", "principal_id", "label", "created_at", "expires_at", "revoked_at":
			default:
				t.Errorf("unexpected field %q in listed credential", k)
			}
		}
	}
}

func TestCreateCredentialAuditAndExpiryRoundTrip(t *testing.T) {
	f := newManifestFixture(t)
	exp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Millisecond)
	resp := f.createCredential(t, f.human.ID, `{"label":"nick laptop","expires_at":"`+exp.Format(time.RFC3339Nano)+`"}`)
	if resp.Credential.ExpiresAt == nil || !resp.Credential.ExpiresAt.Time().Equal(exp) {
		t.Errorf("expires_at = %v, want %v", resp.Credential.ExpiresAt, exp)
	}
	f.do(t, http.MethodPost, credentialPath(resp.Credential.ID)+"/rotate", "")
	actions := f.auditActions(t)
	want := fmt.Sprintf("credential_created principal:%d", f.human.ID)
	if len(actions) == 0 || actions[0] != want {
		t.Errorf("audit = %v, want first %q", actions, want)
	}
	if len(actions) != 2 || !strings.HasPrefix(actions[1], "credential_rotated ") {
		t.Errorf("audit = %v", actions)
	}
}

func TestRotateExpiredCredentialIs400(t *testing.T) {
	f := newManifestFixture(t)
	exp := time.Now().Add(150 * time.Millisecond).UTC()
	resp := f.createCredential(t, f.agent.ID, `{"label":"short","expires_at":"`+exp.Format(time.RFC3339Nano)+`"}`)
	// Wait (non-mutating) until the credential has actually expired.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := f.srv.store.ResolveCredential(context.Background(), resp.Token); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("credential never expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec := f.do(t, http.MethodPost, credentialPath(resp.Credential.ID)+"/rotate", "")
	assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
	// Nothing was written: still exactly one credential, and it is not revoked.
	list := decodeBody[schema.ListCredentialsResponseV1](t, f.do(t, http.MethodGet, credentialsPath(f.agent.ID), ""))
	if len(list.Credentials) != 1 || list.Credentials[0].RevokedAt != nil {
		t.Errorf("list after failed rotate = %+v", list.Credentials)
	}
}

// logCapture swaps the default slog logger for one writing to a buffer.
type logCapture struct{ buf bytes.Buffer }

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	lc := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lc.buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return lc
}

// TestCredentialRedaction runs success and failure cases, including a store
// outage that logs, and asserts the plaintext token and its hex hash appear
// nowhere -- not in any log line, not in any response body -- except in the
// single create/rotate success body's token field.
func TestCredentialRedaction(t *testing.T) {
	logs := captureLogs(t)
	f := newManifestFixture(t)

	created := f.createCredential(t, f.agent.ID, `{"label":"redact"}`)
	secrets := []string{created.Token, sha256Hex(created.Token), strings.TrimPrefix(created.Token, "conch_")}

	var bodies []string
	record := func(rec *httptest.ResponseRecorder) { bodies = append(bodies, rec.Body.String()) }

	cases := []struct{ method, path, body string }{
		{"GET", credentialsPath(f.agent.ID), ""},
		{"POST", credentialsPath(f.agent.ID), `{"label":"x","token":"` + created.Token + `"}`},
		{"POST", credentialsPath(f.agent.ID), `{"label":"` + created.Token},
		{"POST", "/v1/credentials/" + created.Token + "/rotate", ""},
		{"DELETE", "/v1/credentials/" + created.Token, ""},
		{"POST", credentialsPath(9999), `{"label":"x"}`},
		{"POST", credentialPath(9999) + "/rotate", ""},
		{"DELETE", credentialPath(9999), ""},
	}
	for _, c := range cases {
		record(f.do(t, c.method, c.path, c.body))
	}
	// Rotate then rotate the revoked original again (409), then revoke twice.
	rotated := f.do(t, http.MethodPost, credentialPath(created.Credential.ID)+"/rotate", "")
	if rotated.Code != http.StatusCreated {
		t.Fatalf("rotate = %d", rotated.Code)
	}
	newTok := decodeBody[schema.RotateCredentialResponseV1](t, rotated).Token
	secrets = append(secrets, newTok, sha256Hex(newTok))
	record(f.do(t, http.MethodPost, credentialPath(created.Credential.ID)+"/rotate", ""))
	record(f.do(t, http.MethodDelete, credentialPath(created.Credential.ID), ""))

	// A store outage exercises the 500 paths and their log lines.
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", credentialsPath(f.agent.ID), `{"label":"x"}`},
		{"GET", credentialsPath(f.agent.ID), ""},
		{"POST", credentialPath(created.Credential.ID) + "/rotate", ""},
		{"DELETE", credentialPath(created.Credential.ID), ""},
	} {
		rec := f.do(t, c.method, c.path, c.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s during outage = %d, want 500", c.method, c.path, rec.Code)
		}
		record(rec)
	}
	if _, err := f.srv.store.ResolveCredential(context.Background(), newTok); err == nil || errors.Is(err, store.ErrCredentialInvalid) {
		t.Errorf("resolve during outage = %v, want a non-sentinel error", err)
	} else {
		bodies = append(bodies, err.Error())
	}

	if logs.buf.Len() == 0 {
		t.Fatal("no log output captured; the outage cases should have logged")
	}
	bodies = append(bodies, logs.buf.String())
	for i, text := range bodies {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("output %d contains secret material %q...: %.200s", i, secret[:6], text)
			}
		}
	}
}
