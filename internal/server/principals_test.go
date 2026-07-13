package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
