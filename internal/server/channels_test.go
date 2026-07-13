package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestCreateChannel(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(`{"name":"general"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created schema.CreateChannelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Channel.ID == 0 || created.Channel.Name != "general" || created.Channel.CreatedAt.IsZero() {
		t.Errorf("created channel = %+v", created.Channel)
	}

	// A fresh channel must be immediately visible to the message endpoints.
	getReq := httptest.NewRequest(http.MethodGet, "/v0/channels/general/messages", nil)
	getRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET messages status = %d, want %d; body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
}

func TestCreateChannelDuplicate(t *testing.T) {
	srv := newTestServer(t)

	body := `{"name":"general"}`
	first := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(body))
	firstRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d; body = %s", firstRec.Code, http.StatusCreated, firstRec.Body.String())
	}

	second := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(body))
	secondRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(secondRec, second)
	assertAPIError(t, secondRec, http.StatusConflict, "channel_exists")
}

func TestCreateChannelValidation(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{"name":`},
		{name: "empty name", body: `{"name":""}`},
		{name: "blank name", body: `{"name":"   "}`},
		{name: "unknown field", body: `{"name":"general","extra":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			assertAPIError(t, rec, http.StatusBadRequest, "invalid_request")
		})
	}
}
