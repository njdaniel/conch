package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestProvisionThenPostGetRoundTrip proves a fresh server needs no tooling
// beyond the REST API itself: provision a channel and a principal, then post
// and read a message using only the IDs those endpoints returned.
func TestProvisionThenPostGetRoundTrip(t *testing.T) {
	srv := newTestServer(t)

	channelReq := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(`{"name":"general"}`))
	channelRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(channelRec, channelReq)
	if channelRec.Code != http.StatusCreated {
		t.Fatalf("create channel status = %d, want %d; body = %s", channelRec.Code, http.StatusCreated, channelRec.Body.String())
	}
	var channel schema.CreateChannelResponse
	if err := json.Unmarshal(channelRec.Body.Bytes(), &channel); err != nil {
		t.Fatalf("decode create channel response: %v", err)
	}

	principalReq := httptest.NewRequest(http.MethodPost, "/v0/principals", bytes.NewBufferString(`{"kind":"agent","name":"leviathan"}`))
	principalRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(principalRec, principalReq)
	if principalRec.Code != http.StatusCreated {
		t.Fatalf("create principal status = %d, want %d; body = %s", principalRec.Code, http.StatusCreated, principalRec.Body.String())
	}
	var principal schema.CreatePrincipalResponse
	if err := json.Unmarshal(principalRec.Body.Bytes(), &principal); err != nil {
		t.Fatalf("decode create principal response: %v", err)
	}

	postBody := fmt.Sprintf(`{"author_id":%d,"body":"hello, conch"}`, principal.Principal.ID)
	postReq := httptest.NewRequest(http.MethodPost, "/v0/channels/general/messages", bytes.NewBufferString(postBody))
	postRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusCreated {
		t.Fatalf("post message status = %d, want %d; body = %s", postRec.Code, http.StatusCreated, postRec.Body.String())
	}
	var posted schema.PostMessageResponse
	if err := json.Unmarshal(postRec.Body.Bytes(), &posted); err != nil {
		t.Fatalf("decode post message response: %v", err)
	}
	if posted.Message.ChannelID != channel.Channel.ID || posted.Message.AuthorID != principal.Principal.ID {
		t.Fatalf("posted message = %+v, want channel %d author %d", posted.Message, channel.Channel.ID, principal.Principal.ID)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/v0/channels/general/messages", nil)
	getRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get messages status = %d, want %d; body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	var listed schema.ListMessagesResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list messages response: %v", err)
	}
	if len(listed.Messages) != 1 || listed.Messages[0].Body != "hello, conch" {
		t.Errorf("listed messages = %+v, want one message with body %q", listed.Messages, "hello, conch")
	}
}
