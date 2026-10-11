package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func TestCreatePrincipal(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v0/principals", bytes.NewBufferString(`{"kind":"agent","name":"leviathan"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created schema.CreatePrincipalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Principal.ID == 0 || created.Principal.Kind != schema.PrincipalAgent ||
		created.Principal.Name != "leviathan" || created.Principal.CreatedAt.IsZero() {
		t.Errorf("created principal = %+v", created.Principal)
	}
}

func TestCreatePrincipalDuplicate(t *testing.T) {
	srv := newTestServer(t)

	body := `{"kind":"human","name":"nick"}`
	first := httptest.NewRequest(http.MethodPost, "/v0/principals", bytes.NewBufferString(body))
	firstRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d; body = %s", firstRec.Code, http.StatusCreated, firstRec.Body.String())
	}

	second := httptest.NewRequest(http.MethodPost, "/v0/principals", bytes.NewBufferString(body))
	secondRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(secondRec, second)
	assertAPIError(t, secondRec, http.StatusConflict, "principal_exists")
}

func TestCreatePrincipalValidation(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{"kind":`},
		{name: "invalid kind", body: `{"kind":"robot","name":"hal"}`},
		{name: "empty name", body: `{"kind":"human","name":""}`},
		{name: "blank name", body: `{"kind":"human","name":"   "}`},
		{name: "unknown field", body: `{"kind":"human","name":"nick","extra":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v0/principals", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			assertAPIError(t, rec, http.StatusBadRequest, "invalid_request")
		})
	}
}

// jsonString marshals s as a JSON string; unlike %q it uses only escapes JSON
// accepts (\x1b is not one of them).
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCreatePrincipalNameRule is the handler level of issue #204's name rule:
// names that could rewrite a log line or a terminal are refused with 400 and
// a message naming the rule; names in any script are accepted.
func TestCreatePrincipalNameRule(t *testing.T) {
	srv := newTestServer(t)

	accepted := []string{"Zoë Müller", "山田 太郎", "o'brien-smith", "build-bot 2.0 (eu-west)"}
	for _, name := range accepted {
		t.Run("accepted/"+name, func(t *testing.T) {
			body := fmt.Sprintf(`{"kind":"human","name":%s}`, jsonString(t, name))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v0/principals", bytes.NewBufferString(body)))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
			}
			var created schema.CreatePrincipalResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if created.Principal.Name != name {
				t.Errorf("stored name = %q, want %q", created.Principal.Name, name)
			}
		})
	}

	refused := []struct {
		name    string
		input   string
		message string
	}{
		{"escape sequence", "MARK\x1b[31mRED", "control characters"},
		{"newline", "LINE\nONE", "control characters"},
		{"C1 control", "a\u0085b", "control characters"},
		{"line separator", "a\u2028b", "control characters"},
		{"bidi override", "a\u202eb", "bidi controls"},
		{"leading space", " alice", "whitespace"},
		{"trailing space", "alice ", "whitespace"},
		{"over length", strings.Repeat("a", schema.MaxPrincipalNameLength+1), "at most"},
	}
	for _, tt := range refused {
		t.Run("refused/"+tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"kind":"human","name":%s}`, jsonString(t, tt.input))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v0/principals", bytes.NewBufferString(body)))
			assertAPIError(t, rec, http.StatusBadRequest, "invalid_request")
			// Decode the error: the wire form escapes what a raw body scan
			// would miss, so scan the message itself.
			e := decodeBody[schema.Error](t, rec)
			if !strings.Contains(e.Message, tt.message) {
				t.Errorf("message = %q, want it to name the rule (%q)", e.Message, tt.message)
			}
			// The message names the rule; it does not echo the hostile input.
			if strings.Contains(e.Message, tt.input) {
				t.Errorf("message echoes the refused name: %q", e.Message)
			}
		})
	}
}

// TestPrincipalWithLegacyNameStillWorks: a name stored before the rule of
// issue #204 existed — here written straight to the store, which does not
// validate — still signs in, is reported by whoami as stored, and lists.
func TestPrincipalWithLegacyNameStillWorks(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired})
	ctx := context.Background()
	const legacy = "legacy\nname \x1b[31m"
	p, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, legacy)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := get("/v1/whoami")
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var me schema.WhoAmIResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Name != legacy {
		t.Errorf("whoami name = %q, want the stored name", me.Name)
	}
	if rec := get("/v1/channels"); rec.Code != http.StatusOK {
		t.Errorf("list channels = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}
