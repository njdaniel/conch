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
