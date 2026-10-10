package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestClientSend(t *testing.T) {
	wantMessage := schema.MessageV0{
		ID: 4, ChannelID: 2, AuthorID: 7, Body: "hello",
		CreatedAt: time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantError string
	}{
		{
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v0/channels/general/messages" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				var request schema.PostMessageRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if request.AuthorID != 7 || request.Body != "hello" {
					t.Errorf("request = %+v", request)
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{Message: wantMessage})
			},
		},
		{
			name: "structured error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(schema.Error{Code: "channel_not_found", Message: "channel not found"})
			},
			wantError: "channel_not_found: channel not found",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			message, err := client.Send(context.Background(), "general", 7, "hello")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("send error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if message != wantMessage {
				t.Errorf("message = %+v, want %+v", message, wantMessage)
			}
		})
	}
}

func TestClientSendConnectionRefused(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	serverURL := server.URL
	server.Close()
	client, err := NewClient(serverURL, nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := client.Send(context.Background(), "general", 7, "hello"); err == nil || !strings.Contains(err.Error(), "cli: post message") {
		t.Fatalf("send error = %v, want connection error", err)
	}
}

func TestClientTailReceivesMessage(t *testing.T) {
	want := schema.MessageV0{
		ID: 4, ChannelID: 2, AuthorID: 7, Body: "broadcast",
		CreatedAt: time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" || r.URL.Query().Get("channel") != "general" {
			t.Errorf("tail URL = %s", r.URL.String())
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := wsjson.Write(r.Context(), conn, want); err != nil {
			t.Errorf("write websocket message: %v", err)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	var got schema.MessageV0
	errStop := errors.New("stop")
	err = client.Tail(context.Background(), "general", func(message schema.MessageV0) error {
		got = message
		return errStop
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("tail error = %v, want stop", err)
	}
	if got != want {
		t.Errorf("message = %+v, want %+v", got, want)
	}
}

func TestClientListMessagesV1(t *testing.T) {
	want := schema.MessageV1{Schema: schema.MessageSchemaV1, ID: 3, ChannelID: 2, AuthorID: 7, Body: "hello"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/channels/team%2Fops/messages" {
			t.Errorf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		if r.URL.Query().Get("after") != "4" || r.URL.Query().Get("limit") != "25" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(schema.ListMessagesResponseV1{Messages: []schema.MessageV1{want}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := client.ListMessages(context.Background(), "team/ops", 4, 25)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].ID != want.ID {
		t.Errorf("messages = %+v", got.Messages)
	}
}

func TestClientSendMessageV1(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/channels/general/messages" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var request schema.PostMessageRequestV1
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
		}
		if request.AuthorID != 7 || request.Body != "hello" {
			t.Errorf("request = %+v", request)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponseV1{Message: schema.MessageV1{ID: 9, Body: "hello"}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := client.SendMessage(context.Background(), "general", 7, "hello")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	if got.ID != 9 {
		t.Errorf("message = %+v", got)
	}
}

func TestClientListApprovals(t *testing.T) {
	want := schema.ApprovalV1{ID: 42, Title: "Deploy to prod", State: schema.ApprovalStatePending}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/approvals" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(schema.ListApprovalsResponseV1{Approvals: []schema.ApprovalV1{want}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := client.ListApprovals(context.Background())
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(got.Approvals) != 1 || got.Approvals[0].ID != want.ID {
		t.Errorf("approvals = %+v, want ID %d", got.Approvals, want.ID)
	}
}

func TestClientListChannels(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
		want    []string
	}{
		{name: "success", status: http.StatusOK, body: `{"channels":[{"id":1,"name":"general","created_at":"2026-07-13T12:34:56Z"},{"id":2,"name":"ops","created_at":"2026-07-13T12:34:56Z"}]}`, want: []string{"general", "ops"}},
		{name: "empty list", status: http.StatusOK, body: `{"channels":[]}`, want: []string{}},
		{name: "server error", status: http.StatusInternalServerError, body: `{"code":"internal_error","message":"internal server error"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/channels" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			got, err := client.ListChannels(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got.Channels) != len(tt.want) {
				t.Fatalf("channels = %+v, want %v", got.Channels, tt.want)
			}
			for i, name := range tt.want {
				if got.Channels[i].Name != name {
					t.Errorf("channels[%d] = %q, want %q", i, got.Channels[i].Name, name)
				}
			}
		})
	}
}

func TestClientCastDecision(t *testing.T) {
	want := schema.CastDecisionResponseV1{
		Decision: schema.Decision{PrincipalID: 7, OptionID: "approve", Reason: "LGTM"},
		State:    schema.ApprovalStateResolved,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/approvals/42/decisions" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var request schema.CastDecisionRequestV1
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
		}
		if request.PrincipalID != 7 || request.OptionID != "approve" || request.Reason != "LGTM" {
			t.Errorf("request = %+v", request)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := client.CastDecision(context.Background(), 42, schema.CastDecisionRequestV1{PrincipalID: 7, OptionID: "approve", Reason: "LGTM"})
	if err != nil {
		t.Fatalf("cast decision: %v", err)
	}
	if got.Decision.PrincipalID != want.Decision.PrincipalID || got.State != want.State {
		t.Errorf("response = %+v, want %+v", got, want)
	}
}

func TestClientSendsBearerOnEveryCall(t *testing.T) {
	calls := map[string]func(*Client) error{
		"ListMessages":  func(c *Client) error { _, err := c.ListMessages(context.Background(), "g", 0, 10); return err },
		"SendMessage":   func(c *Client) error { _, err := c.SendMessage(context.Background(), "g", 1, "x"); return err },
		"ListChannels":  func(c *Client) error { _, err := c.ListChannels(context.Background()); return err },
		"ListApprovals": func(c *Client) error { _, err := c.ListApprovals(context.Background()); return err },
		"CastDecision": func(c *Client) error {
			_, err := c.CastDecision(context.Background(), 1, schema.CastDecisionRequestV1{})
			return err
		},
		"Send":   func(c *Client) error { _, err := c.Send(context.Background(), "g", 1, "x"); return err },
		"WhoAmI": func(c *Client) error { _, err := c.WhoAmI(context.Background()); return err },
	}
	for name, call := range calls {
		for _, token := range []string{"", "tok-abc"} {
			t.Run(name+"/"+token, func(t *testing.T) {
				var got string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got = r.Header.Get("Authorization")
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(schema.Error{Code: "unauthenticated", Message: "authentication required"})
				}))
				defer server.Close()
				client, err := NewClient(server.URL, server.Client())
				if err != nil {
					t.Fatal(err)
				}
				err = call(client.WithToken(token))
				want := ""
				if token != "" {
					want = "Bearer " + token
				}
				if got != want {
					t.Errorf("Authorization = %q, want %q", got, want)
				}
				var unauth *UnauthenticatedError
				if !errors.As(err, &unauth) || !errors.Is(err, ErrUnauthenticated) || unauth.Server != server.URL {
					t.Errorf("err = %v, want UnauthenticatedError for %s", err, server.URL)
				}
			})
		}
	}
}

func TestWebSocketDialCarriesBearer(t *testing.T) {
	tests := []struct {
		name string
		path string
		run  func(c *Client) error
	}{
		{"tail", "/v0/ws", func(c *Client) error {
			return c.Tail(context.Background(), "g", func(schema.MessageV0) error { return nil })
		}},
		{"subscribe", "/v1/ws", func(c *Client) error {
			return c.Subscribe(context.Background(), "g", func(schema.MessageV1) error { return nil })
		}},
	}
	for _, tt := range tests {
		for _, token := range []string{"", "tok-abc"} {
			t.Run(tt.name+"/"+token, func(t *testing.T) {
				headers := make(chan string, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != tt.path {
						t.Errorf("path = %s, want %s", r.URL.Path, tt.path)
					}
					headers <- r.Header.Get("Authorization")
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					_ = conn.Close(websocket.StatusGoingAway, "bye")
				}))
				defer server.Close()
				client, err := NewClient(server.URL, server.Client())
				if err != nil {
					t.Fatal(err)
				}
				_ = tt.run(client.WithToken(token))
				want := ""
				if token != "" {
					want = "Bearer " + token
				}
				if got := <-headers; got != want {
					t.Errorf("upgrade Authorization = %q, want %q", got, want)
				}
			})
		}
	}

	t.Run("401 on upgrade", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		client, _ := NewClient(server.URL, server.Client())
		err := client.Tail(context.Background(), "g", func(schema.MessageV0) error { return nil })
		if !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("err = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var sinkHits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkHits.Add(1)
	}))
	defer sink.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/sink", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	// The caller's client deliberately follows redirects; NewClient must not trust it.
	followClient := origin.Client()
	followClient.CheckRedirect = nil

	tests := []struct {
		name string
		run  func(c *Client) error
	}{
		{"rest", func(c *Client) error { _, err := c.ListChannels(context.Background()); return err }},
		{"whoami", func(c *Client) error { _, err := c.WhoAmI(context.Background()); return err }},
		{"tail", func(c *Client) error {
			return c.Tail(context.Background(), "g", func(schema.MessageV0) error { return nil })
		}},
		{"subscribe", func(c *Client) error {
			return c.Subscribe(context.Background(), "g", func(schema.MessageV1) error { return nil })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(origin.URL, followClient)
			if err != nil {
				t.Fatal(err)
			}
			err = tt.run(client.WithToken("tok-secret"))
			if err == nil {
				t.Fatal("want an error for a redirect")
			}
			if !strings.Contains(err.Error(), "server redirected to "+sink.URL) || !strings.Contains(err.Error(), "HTTP 307") || !strings.Contains(err.Error(), "--server") {
				t.Errorf("err = %q", err)
			}
			if strings.Contains(err.Error(), "tok-secret") {
				t.Error("error leaks token")
			}
			if n := sinkHits.Load(); n != 0 {
				t.Errorf("redirect target received %d requests, want 0", n)
			}
		})
	}
	if followClient.CheckRedirect != nil {
		t.Error("NewClient mutated the caller's client")
	}
}

func TestNewClientDefaultDoesNotTouchDefaultClient(t *testing.T) {
	client, err := NewClient("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient == http.DefaultClient || http.DefaultClient.CheckRedirect != nil {
		t.Error("must use a private client and leave http.DefaultClient alone")
	}
	if client.httpClient.CheckRedirect == nil {
		t.Error("private client must refuse redirects")
	}
}

func TestUnauthenticatedHintKeepsServerPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Conch-A/v1/whoami" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL+"/Conch-A/", server.Client())
	_, err := client.WhoAmI(context.Background())
	want := "not logged in to " + server.URL + "/Conch-A: run 'conch login'"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// recordedRequest is what the fake server saw of one call.
type recordedRequest struct {
	method, path, rawQuery, auth, body string
}

func recordingServer(t *testing.T, status int, response any) (*httptest.Server, *recordedRequest) {
	t.Helper()
	got := &recordedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*got = recordedRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), string(raw)}
		w.WriteHeader(status)
		if response != nil {
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	t.Cleanup(server.Close)
	return server, got
}

func TestClientV2AndNetsMethods(t *testing.T) {
	ts := schema.NewTimestamp(time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC))
	scoped := schema.MessageV2{
		Schema: schema.MessageSchemaV2, ID: 5, ChannelID: 2, AuthorID: 7, CreatedAt: ts, Body: "psst",
		Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{3, 7}},
	}
	netV1 := schema.NetV1{ID: 4, ChannelID: 2, Name: "alpha", CreatedAt: ts,
		Members: []schema.NetMember{{PrincipalID: 7, Role: schema.NetRoleMember}}}
	tests := []struct {
		name       string
		status     int
		response   any
		call       func(*Client) (any, error)
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   string
		want       any
	}{
		{
			name: "post channel-wide", status: http.StatusCreated,
			response: schema.PostMessageResponseV2{Message: schema.MessageV2{Schema: schema.MessageSchemaV2, ID: 1, Body: "hi"}},
			call: func(c *Client) (any, error) {
				return c.PostMessageV2(context.Background(), "general", 7, "hi", nil)
			},
			wantMethod: http.MethodPost, wantPath: "/v2/channels/general/messages",
			wantBody: `{"author_id":7,"body":"hi"}`,
			want:     schema.MessageV2{Schema: schema.MessageSchemaV2, ID: 1, Body: "hi"},
		},
		{
			name: "post to a net", status: http.StatusCreated,
			response: schema.PostMessageResponseV2{Message: scoped},
			call: func(c *Client) (any, error) {
				return c.PostMessageV2(context.Background(), "general", 0, "hi", &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4})
			},
			wantMethod: http.MethodPost, wantPath: "/v2/channels/general/messages",
			wantBody: `{"body":"hi","audience":{"kind":"net","net_id":4}}`,
			want:     scoped,
		},
		{
			name: "post whisper", status: http.StatusCreated,
			response: schema.PostMessageResponseV2{Message: scoped},
			call: func(c *Client) (any, error) {
				return c.PostMessageV2(context.Background(), "general", 0, "hi", &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{3}})
			},
			wantMethod: http.MethodPost, wantPath: "/v2/channels/general/messages",
			wantBody: `{"body":"hi","audience":{"kind":"principals","principal_ids":[3]}}`,
			want:     scoped,
		},
		{
			name: "list messages", status: http.StatusOK,
			response: schema.ListMessagesResponseV2{Messages: []schema.MessageV2{scoped}},
			call: func(c *Client) (any, error) {
				return c.ListMessagesV2(context.Background(), "general", 3, 50)
			},
			wantMethod: http.MethodGet, wantPath: "/v2/channels/general/messages", wantQuery: "after=3&limit=50",
			want: schema.ListMessagesResponseV2{Messages: []schema.MessageV2{scoped}},
		},
		{
			name: "list nets", status: http.StatusOK,
			response:   schema.ListNetsResponseV1{Nets: []schema.NetV1{netV1}},
			call:       func(c *Client) (any, error) { return c.ListNets(context.Background(), "general") },
			wantMethod: http.MethodGet, wantPath: "/v1/channels/general/nets",
			want: schema.ListNetsResponseV1{Nets: []schema.NetV1{netV1}},
		},
		{
			name: "create net", status: http.StatusCreated,
			response:   schema.CreateNetResponseV1{Net: netV1},
			call:       func(c *Client) (any, error) { return c.CreateNet(context.Background(), "general", "alpha") },
			wantMethod: http.MethodPost, wantPath: "/v1/channels/general/nets",
			wantBody: `{"name":"alpha"}`, want: netV1,
		},
		{
			name: "archive net", status: http.StatusNoContent,
			call:       func(c *Client) (any, error) { return nil, c.ArchiveNet(context.Background(), "general", "alpha") },
			wantMethod: http.MethodDelete, wantPath: "/v1/channels/general/nets/alpha",
		},
		{
			name: "put member", status: http.StatusNoContent,
			call: func(c *Client) (any, error) {
				return nil, c.PutNetMember(context.Background(), "general", "alpha", 9, schema.NetRoleMonitor)
			},
			wantMethod: http.MethodPut, wantPath: "/v1/channels/general/nets/alpha/members/9",
			wantBody: `{"role":"monitor"}`,
		},
		{
			name: "remove member", status: http.StatusNoContent,
			call: func(c *Client) (any, error) {
				return nil, c.RemoveNetMember(context.Background(), "general", "alpha", 9)
			},
			wantMethod: http.MethodDelete, wantPath: "/v1/channels/general/nets/alpha/members/9",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, got := recordingServer(t, tt.status, tt.response)
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := tt.call(client.WithToken("tok"))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.method != tt.wantMethod || got.path != tt.wantPath || got.rawQuery != tt.wantQuery {
				t.Errorf("request = %s %s?%s, want %s %s?%s", got.method, got.path, got.rawQuery, tt.wantMethod, tt.wantPath, tt.wantQuery)
			}
			if got.auth != "Bearer tok" {
				t.Errorf("Authorization = %q", got.auth)
			}
			if tt.wantBody == "" && got.body != "" {
				t.Errorf("body = %q, want none", got.body)
			}
			if tt.wantBody != "" && strings.TrimSpace(got.body) != tt.wantBody {
				t.Errorf("body = %s, want %s", got.body, tt.wantBody)
			}
			if !reflect.DeepEqual(result, tt.want) {
				t.Errorf("result = %+v, want %+v", result, tt.want)
			}
		})
	}
}

func TestClientV2AndNetsErrorMapping(t *testing.T) {
	calls := map[string]func(*Client) error{
		"post":    func(c *Client) error { _, err := c.PostMessageV2(context.Background(), "g", 1, "x", nil); return err },
		"list":    func(c *Client) error { _, err := c.ListMessagesV2(context.Background(), "g", 0, 10); return err },
		"nets":    func(c *Client) error { _, err := c.ListNets(context.Background(), "g"); return err },
		"create":  func(c *Client) error { _, err := c.CreateNet(context.Background(), "g", "a"); return err },
		"archive": func(c *Client) error { return c.ArchiveNet(context.Background(), "g", "a") },
		"put":     func(c *Client) error { return c.PutNetMember(context.Background(), "g", "a", 1, schema.NetRoleMember) },
		"remove":  func(c *Client) error { return c.RemoveNetMember(context.Background(), "g", "a", 1) },
		"subscribe": func(c *Client) error {
			return c.SubscribeV2(context.Background(), "g", func(schema.MessageV2) error { return nil })
		},
	}
	for name, call := range calls {
		for _, code := range []string{"net_not_found", "forbidden", "invalid_audience"} {
			t.Run(name+"/"+code, func(t *testing.T) {
				server, _ := recordingServer(t, http.StatusForbidden, schema.Error{Code: code, Message: "nope"})
				client, err := NewClient(server.URL, server.Client())
				if err != nil {
					t.Fatal(err)
				}
				err = call(client)
				if err == nil || !strings.Contains(err.Error(), code) || !strings.Contains(err.Error(), "nope") {
					t.Fatalf("err = %v, want server code %q and message", err, code)
				}
			})
		}
	}
	t.Run("401 is the login hint", func(t *testing.T) {
		server, _ := recordingServer(t, http.StatusUnauthorized, schema.Error{Code: "unauthenticated"})
		client, _ := NewClient(server.URL, server.Client())
		if _, err := client.ListNets(context.Background(), "g"); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestClientSubscribeV2ReceivesScopedMessageWithBearer(t *testing.T) {
	want := schema.MessageV2{
		Schema: schema.MessageSchemaV2, ID: 4, ChannelID: 2, AuthorID: 7, Body: "psst",
		CreatedAt: schema.NewTimestamp(time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)),
		Audience:  &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/ws" || r.URL.Query().Get("channel") != "general" {
			t.Errorf("subscribe URL = %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = wsjson.Write(r.Context(), conn, want)
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var got schema.MessageV2
	errStop := errors.New("stop")
	err = client.WithToken("tok").SubscribeV2(context.Background(), "general", func(m schema.MessageV2) error {
		got = m
		return errStop
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("err = %v, want stop", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("message = %+v, want %+v", got, want)
	}
}

// voiceDoc builds a valid presence document for channel 2. A nil participant
// slice is sent as an empty array, as the server does.
func voiceDoc(configured, available bool, people ...schema.VoiceParticipant) schema.VoicePresenceV1 {
	doc := schema.VoicePresenceV1{
		Schema: schema.VoicePresenceSchemaV1, ChannelID: 2,
		Configured: configured, Available: available, Rooms: []schema.VoicePresenceRoom{},
	}
	if available {
		if people == nil {
			people = []schema.VoiceParticipant{}
		}
		doc.Rooms = append(doc.Rooms, schema.VoicePresenceRoom{Participants: people})
	}
	return doc
}

func voicePerson(id int64, transmitting bool) schema.VoiceParticipant {
	return schema.VoiceParticipant{
		PrincipalID: id, CanPublish: true, Transmitting: transmitting,
		JoinedAt: schema.NewTimestamp(time.Date(2026, time.October, 9, 8, 30, 0, 0, time.UTC)),
	}
}

func TestClientVoicePresence(t *testing.T) {
	want := voiceDoc(true, true, voicePerson(3, true), voicePerson(7, false))
	// A document that decodes but contradicts itself must not be returned.
	badSchema := want
	badSchema.Schema = "conch.voice_presence.v9"
	notAvailableWithRooms := want
	notAvailableWithRooms.Available = false
	duplicate := voiceDoc(true, true, voicePerson(3, false), voicePerson(3, false))
	tests := []struct {
		name      string
		status    int
		response  any
		wantErr   string // substring; empty means success
		wantUnath bool
	}{
		{name: "ok", status: http.StatusOK, response: want},
		{name: "not configured", status: http.StatusOK, response: voiceDoc(false, false)},
		{name: "unavailable", status: http.StatusOK, response: voiceDoc(true, false)},
		{name: "unknown channel or non-member", status: http.StatusNotFound, response: schema.Error{Code: "channel_not_found", Message: "no such channel"}, wantErr: "channel_not_found"},
		{name: "agent credential", status: http.StatusForbidden, response: schema.Error{Code: "forbidden", Message: "humans only"}, wantErr: "forbidden"},
		{name: "auth off", status: http.StatusBadRequest, response: schema.Error{Code: schema.ErrorCodeVoiceRequiresAuth, Message: "needs auth"}, wantErr: schema.ErrorCodeVoiceRequiresAuth},
		{name: "not logged in", status: http.StatusUnauthorized, response: schema.Error{Code: "unauthenticated"}, wantUnath: true, wantErr: "not logged in"},
		{name: "wrong schema", status: http.StatusOK, response: badSchema, wantErr: "invalid voice presence"},
		{name: "rooms while unavailable", status: http.StatusOK, response: notAvailableWithRooms, wantErr: "invalid voice presence"},
		{name: "duplicate participant", status: http.StatusOK, response: duplicate, wantErr: "invalid voice presence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, got := recordingServer(t, tt.status, tt.response)
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			doc, err := client.WithToken("tok").VoicePresence(context.Background(), "ops/ci room")
			if got.method != http.MethodGet || got.path != "/v1/channels/ops%2Fci%20room/voice" || got.rawQuery != "" {
				t.Errorf("request = %s %s?%s", got.method, got.path, got.rawQuery)
			}
			if got.auth != "Bearer tok" {
				t.Errorf("Authorization = %q", got.auth)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if !reflect.DeepEqual(doc, tt.response) {
					t.Errorf("doc = %+v, want %+v", doc, tt.response)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if errors.Is(err, ErrUnauthenticated) != tt.wantUnath {
				t.Errorf("errors.Is(ErrUnauthenticated) = %v, want %v", !tt.wantUnath, tt.wantUnath)
			}
			if !reflect.DeepEqual(doc, schema.VoicePresenceV1{}) {
				t.Errorf("a failed call returned %+v", doc)
			}
		})
	}
}

// presenceSocket serves /v1/voice/ws with script, after checking the request.
func presenceSocket(t *testing.T, script func(ctx context.Context, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/voice/ws" || r.URL.Query().Get("channel") != "general" {
			t.Errorf("subscribe URL = %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		script(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestClientSubscribeVoicePresence(t *testing.T) {
	docs := []schema.VoicePresenceV1{
		voiceDoc(true, true),
		voiceDoc(true, true, voicePerson(3, false)),
		voiceDoc(true, true, voicePerson(3, true)),
		voiceDoc(true, false),
	}
	tests := []struct {
		name       string
		script     func(ctx context.Context, conn *websocket.Conn)
		cancelAt   int // cancel the context after this many documents; 0 never
		wantDocs   int
		wantStatus websocket.StatusCode // expected close status of the returned error; -1 none
		wantCancel bool
		wantErr    string
	}{
		{
			name: "documents arrive in order, then going away",
			script: func(ctx context.Context, conn *websocket.Conn) {
				for _, d := range docs {
					_ = wsjson.Write(ctx, conn, d)
				}
				_ = conn.Close(websocket.StatusGoingAway, "bye")
			},
			wantDocs: 4, wantStatus: websocket.StatusGoingAway,
		},
		{
			name: "removed from the channel",
			script: func(ctx context.Context, conn *websocket.Conn) {
				_ = wsjson.Write(ctx, conn, docs[0])
				_ = conn.Close(websocket.StatusPolicyViolation, "removed")
			},
			wantDocs: 1, wantStatus: websocket.StatusPolicyViolation,
		},
		{
			name: "an invalid frame ends the subscription",
			script: func(ctx context.Context, conn *websocket.Conn) {
				_ = wsjson.Write(ctx, conn, docs[0])
				bad := docs[1]
				bad.Schema = "nope"
				_ = wsjson.Write(ctx, conn, bad)
				_ = wsjson.Write(ctx, conn, docs[2]) // never delivered
				_ = conn.Close(websocket.StatusNormalClosure, "done")
			},
			wantDocs: 1, wantStatus: -1, wantErr: "invalid voice presence",
		},
		{
			name: "not JSON at all",
			script: func(ctx context.Context, conn *websocket.Conn) {
				_ = conn.Write(ctx, websocket.MessageText, []byte("<html>"))
				_ = conn.Close(websocket.StatusNormalClosure, "done")
			},
			wantDocs: 0, wantStatus: -1, wantErr: "read voice presence",
		},
		{
			name: "context cancellation",
			script: func(ctx context.Context, conn *websocket.Conn) {
				_ = wsjson.Write(ctx, conn, docs[0])
				<-ctx.Done()
			},
			cancelAt: 1, wantDocs: 1, wantStatus: -1, wantCancel: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := presenceSocket(t, tt.script)
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var got []schema.VoicePresenceV1
			err = client.WithToken("tok").SubscribeVoicePresence(ctx, "general", func(d schema.VoicePresenceV1) error {
				got = append(got, d)
				if tt.cancelAt != 0 && len(got) == tt.cancelAt {
					cancel()
				}
				return nil
			})
			if err == nil {
				t.Fatal("subscription ended without an error")
			}
			if len(got) != tt.wantDocs || (tt.wantDocs > 0 && !reflect.DeepEqual(got, docs[:tt.wantDocs])) {
				t.Errorf("received %d documents %+v, want the first %d", len(got), got, tt.wantDocs)
			}
			if status := websocket.CloseStatus(err); status != tt.wantStatus {
				t.Errorf("close status = %d, want %d (err %v)", status, tt.wantStatus, err)
			}
			if errors.Is(err, context.Canceled) != tt.wantCancel {
				t.Errorf("errors.Is(context.Canceled) = %v, want %v (err %v)", !tt.wantCancel, tt.wantCancel, err)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestClientSubscribeVoicePresenceReceiveErrorStops(t *testing.T) {
	server := presenceSocket(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = wsjson.Write(ctx, conn, voiceDoc(true, true))
		<-ctx.Done()
	})
	client, _ := NewClient(server.URL, server.Client())
	stop := errors.New("stop")
	err := client.WithToken("tok").SubscribeVoicePresence(context.Background(), "general", func(schema.VoicePresenceV1) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want stop", err)
	}
}

func TestClientSubscribeVoicePresenceRefusedUpgrade(t *testing.T) {
	tests := []struct {
		status  int
		body    schema.Error
		wantErr string
		unauth  bool
	}{
		{http.StatusNotFound, schema.Error{Code: "channel_not_found", Message: "no"}, "channel_not_found", false},
		{http.StatusForbidden, schema.Error{Code: "forbidden", Message: "no"}, "forbidden", false},
		{http.StatusBadRequest, schema.Error{Code: schema.ErrorCodeVoiceRequiresAuth, Message: "no"}, schema.ErrorCodeVoiceRequiresAuth, false},
		{http.StatusUnauthorized, schema.Error{Code: "unauthenticated"}, "not logged in", true},
	}
	for _, tt := range tests {
		t.Run(tt.wantErr, func(t *testing.T) {
			server, got := recordingServer(t, tt.status, tt.body)
			client, _ := NewClient(server.URL, server.Client())
			err := client.WithToken("tok").SubscribeVoicePresence(context.Background(), "general", func(schema.VoicePresenceV1) error {
				t.Error("received a document from a refused upgrade")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || errors.Is(err, ErrUnauthenticated) != tt.unauth {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if got.path != "/v1/voice/ws" || got.rawQuery != "channel=general" {
				t.Errorf("request path %s query %s", got.path, got.rawQuery)
			}
		})
	}
}

func TestServerErrorKeepsStatusAndCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"voice_requires_auth","message":"voice needs a signed-in caller"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"snapshot": func() error { _, err := client.VoicePresence(context.Background(), "general"); return err },
		"socket": func() error {
			return client.SubscribeVoicePresence(context.Background(), "general", func(schema.VoicePresenceV1) error { return nil })
		},
	} {
		err := call()
		var serverErr *ServerError
		if !errors.As(err, &serverErr) || serverErr.Status != http.StatusBadRequest || serverErr.Code != schema.ErrorCodeVoiceRequiresAuth {
			t.Fatalf("%s: err = %#v", name, err)
		}
		if want := "cli: server error voice_requires_auth: voice needs a signed-in caller"; !strings.Contains(err.Error(), want) {
			t.Errorf("%s: text = %q, want it to contain %q", name, err.Error(), want)
		}
	}
}
